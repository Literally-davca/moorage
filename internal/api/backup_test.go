package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"moorage/internal/auth"
	"moorage/internal/config"
	"moorage/internal/store"
)

func newBackupEnv(t *testing.T) (*auditEnv, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
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
	srv := New(config.Config{DataDir: dir}, a, db, nil, nil, nil, log, "test")
	return &auditEnv{db: db, srv: srv, h: srv.Handler()}, dir
}

func TestBackupExportImportRoundTrip(t *testing.T) {
	e, dir := newBackupEnv(t)

	if w := e.req("POST", "/api/settings/backup/export", `{"password":"short"}`, "tok-admin"); w.Code != http.StatusBadRequest {
		t.Fatalf("short password: %d", w.Code)
	}
	if w := e.req("POST", "/api/settings/backup/export", `{"password":"a long password"}`, "tok-viewer"); w.Code != http.StatusForbidden {
		t.Fatalf("viewer export: %d", w.Code)
	}
	w := e.req("POST", "/api/settings/backup/export", `{"password":"a long password"}`, "tok-admin")
	if w.Code != http.StatusOK {
		t.Fatalf("export: %d %s", w.Code, w.Body.String())
	}
	archive := w.Body.Bytes()
	if len(archive) < 100 || strings.Contains(string(archive), "SQLite format") {
		t.Fatal("export is empty or not encrypted")
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "export-*")); len(m) != 0 {
		t.Fatalf("export temp dir left behind: %v", m)
	}

	upload := func(pw string, body []byte) (*http.Response, map[string]any) {
		hdr := map[string]string{csrfHeader: "1", "Cookie": cookieName + "=tok-admin", "X-Backup-Password": pw}
		rec := do(e.h, "POST", "/api/settings/backup/import/preview", string(body), hdr)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out) //nolint:errcheck
		return rec.Result(), out
	}
	if r, out := upload("wrong password", archive); r.StatusCode != http.StatusBadRequest || !strings.Contains(out["error"].(string), "password") {
		t.Fatalf("wrong password: %d %v", r.StatusCode, out)
	}
	cut := archive[:len(archive)-40]
	if r, _ := upload("a long password", cut); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("truncated upload: %d", r.StatusCode)
	}
	r, prev := upload("a long password", archive)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("preview: %d %v", r.StatusCode, prev)
	}
	dbInfo := prev["database"].(map[string]any)
	if dbInfo["users"].(float64) != 2 || int(dbInfo["schemaVersion"].(float64)) != store.SchemaVersion() {
		t.Fatalf("preview database: %v", dbInfo)
	}
	token := prev["token"].(string)
	if _, err := os.Stat(filepath.Join(dir, "moorage.db.restore")); err == nil {
		t.Fatal("preview already staged the database")
	}

	if w := e.req("POST", "/api/settings/backup/import/apply", `{"token":"`+token+`","database":false,"stacks":false}`, "tok-admin"); w.Code != http.StatusBadRequest {
		t.Fatalf("apply nothing: %d", w.Code)
	}
	w = e.req("POST", "/api/settings/backup/import/apply", `{"token":"`+token+`","database":true,"stacks":true}`, "tok-admin")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"dbStaged":true`) {
		t.Fatalf("apply: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "moorage.db.restore")); err != nil {
		t.Fatal("database not staged for restart")
	}
	if w := e.req("POST", "/api/settings/backup/import/apply", `{"token":"`+token+`","database":true}`, "tok-admin"); w.Code != http.StatusGone {
		t.Fatalf("token reused: %d", w.Code)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "import-*")); len(m) != 0 {
		t.Fatalf("import staging left behind: %v", m)
	}

	entries, err := e.db.AuditEntries(t.Context(), store.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, a := range entries {
		seen[a.Action] = true
	}
	if !seen["backup.export"] || !seen["backup.import"] {
		t.Fatalf("audit rows missing: %v", seen)
	}
}

func TestInspectBackupRejectsGarbage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.db")
	os.WriteFile(p, []byte("definitely not sqlite"), 0o600)
	if _, err := store.InspectBackup(t.Context(), p); err == nil {
		t.Fatal("garbage accepted as a database")
	}
}
