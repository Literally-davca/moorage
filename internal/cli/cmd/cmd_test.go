package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"moorage/internal/cli/client"
)

func TestOfflineConvertAndCleanAndValidate(t *testing.T) {
	// 1. moorage convert offline
	var out bytes.Buffer
	root := newRootCmd("0.1.4", ServerHooks{})
	root.SetOut(&out)
	root.SetArgs([]string{"convert", "docker run -d --name demo -p 8080:80 nginx:1.27-alpine"})
	if err := root.Execute(); err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	if !strings.Contains(out.String(), "nginx:1.27-alpine") {
		t.Fatalf("unexpected convert output: %s", out.String())
	}

	// 2. moorage compose clean offline (preserves block scalar blank lines, removes others)
	dir := t.TempDir()
	fpath := filepath.Join(dir, "compose.yaml")
	sample := "# top comment\n\nservices:\n  app:\n    image: nginx:1.27\n\n    command: |\n      one\n\n      two\n\n    restart: unless-stopped # inline\n"
	if err := os.WriteFile(fpath, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	root = newRootCmd("0.1.4", ServerHooks{})
	root.SetOut(&out)
	root.SetArgs([]string{"compose", "clean", fpath})
	if err := root.Execute(); err != nil {
		t.Fatalf("compose clean failed: %v", err)
	}
	cleaned := out.String()
	if strings.Contains(cleaned, "# top") || strings.Contains(cleaned, "# inline") || strings.Contains(cleaned, "\n\n    restart") {
		t.Fatalf("expected comments and non-scalar blanks removed, got:\n%s", cleaned)
	}
	if !strings.Contains(cleaned, "one\n\n      two") {
		t.Fatalf("expected block scalar blank preserved, got:\n%s", cleaned)
	}

	// 3. moorage compose validate --local offline
	out.Reset()
	root = newRootCmd("0.1.4", ServerHooks{})
	root.SetOut(&out)
	root.SetArgs([]string{"compose", "validate", "--local", fpath})
	if err := root.Execute(); err != nil {
		t.Fatalf("compose validate --local failed: %v (out=%s)", err, out.String())
	}
}

func TestStacksListCommandAgainstMockAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/hosts":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"hosts": []client.HostEntry{{ID: "local", Name: "Hub", Connected: true}},
			})
		case "/api/coolify/names":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"names": map[string]client.CoolifyNameMeta{"local/uuid-1": {UUID: "uuid-1", Name: "Authentik"}},
			})
		case "/api/hosts/local/stacks":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"stacks": []client.StackSummary{{Name: "uuid-1", Status: "up", Running: 2, Total: 2, Managed: true}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	var out bytes.Buffer
	root := newRootCmd("0.1.4", ServerHooks{})
	root.SetOut(&out)
	root.SetArgs([]string{"--url", srv.URL, "--token", "mrg_tok", "stacks", "ls"})
	if err := root.Execute(); err != nil {
		t.Fatalf("stacks ls failed: %v", err)
	}
	if !strings.Contains(out.String(), "Authentik") || !strings.Contains(out.String(), "uuid-1") {
		t.Fatalf("expected Coolify decoded name Authentik in table:\n%s", out.String())
	}
}
