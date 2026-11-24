package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"moorage/internal/config"
	"moorage/internal/store"
)

func TestCustomAddonEnvPrefixAndMove(t *testing.T) {
	baseEnv := "PORT=8080\nSHARED=keep\n"
	addonCompose := `services:
  metrics:
    image: prom/node-exporter:latest
    environment:
      PORT: "9100"
      SHARED: "keep"
`
	rewritten, mergedEnv, owned, err := mergeAddonEnvWithPrefix("node-exporter", addonCompose, "", baseEnv, nil)
	if err != nil {
		t.Fatalf("mergeAddonEnvWithPrefix: %v", err)
	}
	if !strings.Contains(rewritten, "${NODE_EXPORTER_PORT}") {
		t.Fatalf("expected prefixed var ref in compose, got:\n%s", rewritten)
	}
	if !strings.Contains(mergedEnv, "PORT=8080") || !strings.Contains(mergedEnv, "NODE_EXPORTER_PORT=9100") {
		t.Fatalf("expected both PORT and NODE_EXPORTER_PORT in .env, got:\n%s", mergedEnv)
	}
	if len(owned) == 0 {
		t.Fatalf("expected owned keys to be recorded")
	}

	origCompose := `services:
  app:
    image: nginx:alpine
    volumes:
      - ./data:/data
      - app_cache:/cache
volumes:
  app_cache:
`
	movedCompose, err := rewriteComposeForMoveToAddons("my-app", "my-app", origCompose)
	if err != nil {
		t.Fatalf("rewriteComposeForMoveToAddons: %v", err)
	}
	if !strings.Contains(movedCompose, "./my-app/data:/data") {
		t.Fatalf("expected relative bind mount rewritten under ./my-app/data, got:\n%s", movedCompose)
	}
	if !strings.Contains(movedCompose, "name: my-app_app_cache") {
		t.Fatalf("expected named volume preserved as my-app_app_cache, got:\n%s", movedCompose)
	}
}

func TestCronScheduleMatch(t *testing.T) {
	cs, err := parseCron("15 4 * * 1-5")
	if err != nil {
		t.Fatalf("parseCron: %v", err)
	}
	// Monday 2026-11-23 04:15
	mon := time.Date(2026, 11, 23, 4, 15, 0, 0, time.UTC)
	if !cs.matches(mon) {
		t.Fatalf("expected Monday 04:15 to match")
	}
	// Sunday 2026-11-22 04:15
	sun := time.Date(2026, 11, 22, 4, 15, 0, 0, time.UTC)
	if cs.matches(sun) {
		t.Fatalf("expected Sunday 04:15 not to match 1-5")
	}
	if _, err := parseCron("bad cron"); err == nil {
		t.Fatalf("expected invalid cron error")
	}
}

func TestRegistryWebhookHMACAndParsing(t *testing.T) {
	secret := "top-secret-key"
	body := []byte(`{"repository":{"repo_name":"ghcr.io/acme/web"}}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !verifyHubSignature256(secret, body, sig) {
		t.Fatalf("expected valid HMAC signature to verify")
	}
	if verifyHubSignature256(secret, body, "sha256=deadbeef") {
		t.Fatalf("expected wrong HMAC signature to fail")
	}

	repos := parseRegistryWebhookRepos("dockerhub", body)
	if len(repos) != 1 || repos[0] != "ghcr.io/acme/web" {
		t.Fatalf("unexpected parsed repos: %v", repos)
	}
	ghcr := []byte(`{"package":{"package_version":{"package_url":"pkg:docker/ghcr.io/acme/api@sha256:123"}}}`)
	repos2 := parseRegistryWebhookRepos("ghcr", ghcr)
	if len(repos2) == 0 || repos2[len(repos2)-1] != "ghcr.io/acme/api" {
		t.Fatalf("expected ghcr package_url repo, got: %v", repos2)
	}
}

func TestMCPServerSafetyAndDrafts(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	srv := New(config.Config{}, nil, db, nil, nil, nil, slog.Default(), "test")
	h := srv.Handler()

	// 1. Returns 404 when mcp-server add-on is off
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when mcp-server is disabled, got %d", rec.Code)
	}

	// 2. Enable mcp-server and create token with require_approval = true
	if err := db.SetAddon(ctx, addonMCPServer, true); err != nil {
		t.Fatalf("SetAddon: %v", err)
	}
	plain, hash := generateRandomToken("mr_mcp_")
	_, err = db.CreateMCPToken(ctx, "claude", hash, []store.MCPACLRule{
		{HostID: "*", StackPattern: "*", Scope: "deploy"},
	}, true, time.Time{}, "admin")
	if err != nil {
		t.Fatalf("CreateMCPToken: %v", err)
	}

	// Verify masked .env helper never leaks secret values
	masked := maskEnvText("DB_PASS=supersecret\n# comment\nPORT=8080\n")
	if strings.Contains(masked, "supersecret") || !strings.Contains(masked, "DB_PASS=********") {
		t.Fatalf("expected masked .env, got:\n%s", masked)
	}

	// Verify add-ons stack and disallowed actions are refused
	tok, _ := db.MCPTokenByHash(ctx, hash)
	if _, err := srv.execMCPTool(ctx, req, tok, "stack_action", json.RawMessage(`{"hostId":"local","stack":"add-ons","action":"restart"}`)); err == nil {
		t.Fatalf("expected MCP refusal on add-ons stack")
	}
	if _, err := srv.execMCPTool(ctx, req, tok, "stack_action", json.RawMessage(`{"hostId":"local","stack":"web","action":"down"}`)); err == nil {
		t.Fatalf("expected MCP refusal on down action")
	}

	// Verify HTTP /mcp initialize works with Bearer token
	initBody, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	req2 := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(initBody))
	req2.Header.Set("Authorization", "Bearer "+plain)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on /mcp initialize, got %d (%s)", rec2.Code, rec2.Body.String())
	}

	// Verify MCP draft approval lifecycle
	draft, err := db.CreateMCPDraft(ctx, store.MCPDraft{
		TokenName: tok.Name,
		Tool:      "save_compose",
		HostID:    "local",
		Stack:     "web",
		Compose:   "services:\n  web:\n    image: nginx:alpine\n",
		Note:      "update web",
	})
	if err != nil {
		t.Fatalf("CreateMCPDraft: %v", err)
	}
	pending, err := db.MCPDrafts(ctx, "pending")
	if err != nil || len(pending) != 1 || pending[0].ID != draft.ID {
		t.Fatalf("expected 1 pending MCP draft, got %v (err=%v)", pending, err)
	}
	if err := db.DecideMCPDraft(ctx, draft.ID, "approved", "admin"); err != nil {
		t.Fatalf("DecideMCPDraft: %v", err)
	}
	pendingAfter, err := db.MCPDrafts(ctx, "pending")
	if err != nil || len(pendingAfter) != 0 {
		t.Fatalf("expected 0 pending MCP drafts after decision, got %v (err=%v)", pendingAfter, err)
	}
}
