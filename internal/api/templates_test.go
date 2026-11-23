package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"moorage/internal/auth"
	"moorage/internal/config"
	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/store"
)

// newTemplateEnv runs the API with a real local engine (no Docker needed: only files and
// the version history are used), so the create path is the production one.
func newTemplateEnv(t *testing.T) *auditEnv {
	t.Helper()
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(t.TempDir(), "no-docker.sock"))
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	createTestUserSession(t, db, "admin", "admin", "tok-admin")
	createTestUserSession(t, db, "viewer", "viewer", "tok-viewer")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := auth.New(db, log)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DataDir: t.TempDir(), StacksDir: t.TempDir(), PUID: -1, PGID: -1}
	eng, err := engine.New(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	reg := host.NewRegistry(host.Local{E: eng}, "box", false, db)
	srv := New(cfg, a, db, reg, nil, nil, log, "test")
	return &auditEnv{db: db, srv: srv, h: srv.Handler()}
}

func TestTemplatesBrowseRenderCreate(t *testing.T) {
	env := newTemplateEnv(t)

	// Viewers browse the catalogue and a template, without pre-generated secrets.
	w := env.req("GET", "/api/templates", "", "tok-viewer")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"postgres"`) {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	w = env.req("GET", "/api/templates/bundled/postgres", "", "tok-viewer")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"generated"`) {
		t.Fatalf("viewer detail: %d %s", w.Code, w.Body)
	}
	if w := env.req("GET", "/api/templates/bundled/nope", "", "tok-viewer"); w.Code != http.StatusNotFound {
		t.Fatalf("missing template: %d", w.Code)
	}
	var detail templateDetail
	w = env.req("GET", "/api/templates/bundled/postgres", "", "tok-admin")
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || len(detail.Generated["DB_PASSWORD"]) != 32 {
		t.Fatalf("admin detail: %s %v", w.Body, err)
	}

	// Viewers can't render or create.
	body := `{"source":"bundled","id":"postgres","values":{"DB_USER":"shop"}}`
	if w := env.req("POST", "/api/templates/render", body, "tok-viewer"); w.Code != http.StatusForbidden {
		t.Fatalf("viewer render: %d", w.Code)
	}
	w = env.req("POST", "/api/templates/render", body, "tok-admin")
	if w.Code != http.StatusOK {
		t.Fatalf("render: %d %s", w.Code, w.Body)
	}
	var rendered struct {
		Result struct {
			Compose, Env string
			Values       map[string]string
			Secrets      []string
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rendered); err != nil {
		t.Fatal(err)
	}
	pw := rendered.Result.Values["DB_PASSWORD"]
	if len(pw) != 32 || !strings.Contains(rendered.Result.Env, "POSTGRES_USER=shop\n") || !strings.Contains(rendered.Result.Env, pw) {
		t.Fatalf("rendered: %+v", rendered.Result)
	}
	w = env.req("POST", "/api/templates/render", `{"source":"bundled","id":"postgres","values":{"DB_USER":"Bad User"}}`, "tok-admin")
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `"field":"DB_USER"`) {
		t.Fatalf("invalid value: %d %s", w.Code, w.Body)
	}
	if rows := auditRows(t, env.db); len(rows) != 0 {
		t.Fatalf("render must not be audited: %+v", rows)
	}

	// Create through the normal create path, with the template reference.
	create, _ := json.Marshal(map[string]any{
		"name": "shop-db", "compose": rendered.Result.Compose, "env": rendered.Result.Env,
		"template": map[string]string{"source": "bundled", "id": "postgres"},
	})
	if w := env.req("POST", "/api/hosts/local/stacks", string(create), "tok-viewer"); w.Code != http.StatusForbidden {
		t.Fatalf("viewer create: %d", w.Code)
	}
	if w := env.req("POST", "/api/hosts/local/stacks", string(create), "tok-admin"); w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	w = env.req("GET", "/api/hosts/local/stacks/shop-db/history", "", "tok-admin")
	var versions []engine.Version
	if err := json.Unmarshal(w.Body.Bytes(), &versions); err != nil || len(versions) != 1 {
		t.Fatalf("history: %s %v", w.Body, err)
	}
	if versions[0].Kind != engine.KindCreate || versions[0].Message != "Created from template postgres (bundled, version 1)" || versions[0].Author != "admin" {
		t.Fatalf("first version: %+v", versions[0])
	}
	rows := auditRows(t, env.db)
	if len(rows) != 1 || rows[0].Action != "stack.create" || rows[0].Stack != "shop-db" || rows[0].Outcome != store.AuditOK {
		t.Fatalf("audit: %+v", rows)
	}
	b, _ := json.Marshal(rows)
	if strings.Contains(string(b), pw) {
		t.Fatal("generated password leaked into the audit log")
	}

	// A made-up template reference is refused before anything is written.
	bad, _ := json.Marshal(map[string]any{
		"name": "other", "compose": rendered.Result.Compose, "env": rendered.Result.Env,
		"template": map[string]string{"source": "../etc", "id": "postgres"},
	})
	if w := env.req("POST", "/api/hosts/local/stacks", string(bad), "tok-admin"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad template ref: %d", w.Code)
	}
	// A plain create still records the plain message.
	plain, _ := json.Marshal(map[string]any{"name": "plain", "compose": "services:\n  web:\n    image: nginx:1.27-alpine\n"})
	if w := env.req("POST", "/api/hosts/local/stacks", string(plain), "tok-admin"); w.Code != http.StatusCreated {
		t.Fatalf("plain create: %d %s", w.Code, w.Body)
	}
	w = env.req("GET", "/api/hosts/local/stacks/plain/history", "", "tok-admin")
	if !strings.Contains(w.Body.String(), `"message":"Create stack"`) {
		t.Fatalf("plain history: %s", w.Body)
	}
}

func TestTemplateSourcesAPI(t *testing.T) {
	env := newTemplateEnv(t)
	if w := env.req("GET", "/api/templates/sources", "", "tok-viewer"); w.Code != http.StatusForbidden {
		t.Fatalf("viewer sources: %d", w.Code)
	}
	if w := env.req("POST", "/api/templates/sources", `{"url":"http://example.com/index.json"}`, "tok-admin"); w.Code != http.StatusBadRequest {
		t.Fatalf("http source: %d %s", w.Code, w.Body)
	}
	// 127.0.0.1:1 is refused by the address guard before any connection is made.
	w := env.req("POST", "/api/templates/sources", `{"url":"https://127.0.0.1:1/index.json"}`, "tok-admin")
	if w.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	env.srv.tpl.Wait()
	var created struct{ ID int64 }
	json.Unmarshal(w.Body.Bytes(), &created) //nolint:errcheck
	id := itoa(created.ID)
	w = env.req("GET", "/api/templates/sources", "", "tok-admin")
	if !strings.Contains(w.Body.String(), "allow local network") {
		t.Fatalf("refusal not shown on the source: %s", w.Body)
	}
	w = env.req("GET", "/api/templates", "", "tok-viewer")
	if !strings.Contains(w.Body.String(), `"label":"from 127.0.0.1"`) {
		t.Fatalf("remote source missing from the catalogue: %s", w.Body)
	}
	if w := env.req("PUT", "/api/templates/sources/"+id, `{"url":"https://127.0.0.1:2/index.json","allowLocal":false}`, "tok-admin"); w.Code != http.StatusNoContent {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	if w := env.req("POST", "/api/templates/sources/"+id+"/refresh", "", "tok-admin"); w.Code != http.StatusAccepted {
		t.Fatalf("refresh: %d", w.Code)
	}
	env.srv.tpl.Wait()
	if w := env.req("DELETE", "/api/templates/sources/"+id, "", "tok-admin"); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := env.req("DELETE", "/api/templates/sources/"+id, "", "tok-admin"); w.Code != http.StatusNotFound {
		t.Fatalf("second delete: %d", w.Code)
	}
	var actions []string
	for _, r := range auditRows(t, env.db) {
		actions = append(actions, r.Action+":"+r.Target)
	}
	want := "template_source.delete:" + id + ",template_source.refresh:" + id + ",template_source.save:" + id + ",template_source.create:"
	if strings.Join(actions, ",") != want {
		t.Fatalf("audit rows: %v", actions)
	}
}
