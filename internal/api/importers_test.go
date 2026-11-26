package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"moorage/internal/auth"
	"moorage/internal/config"
	"moorage/internal/store"
)

// cliServer returns the local-socket handler of a fresh server: every request is admin.
func cliServer(t *testing.T, cfg config.Config) (http.Handler, *store.Store) {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := auth.New(db, log)
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, a, db, nil, nil, nil, log, "test").SocketHandler(), db
}

func TestMonitorImportKumaIdempotent(t *testing.T) {
	h, db := cliServer(t, config.Config{})
	data, err := os.ReadFile("../migrate/testdata/kuma-backup.json")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"source": "uptime-kuma", "content": string(data)})
	w := do(h, "POST", "/api/import/monitors/preview", string(body), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "secret-token") || strings.Contains(w.Body.String(), "tk_abc") {
		t.Fatalf("preview leaks channel secrets: %s", w.Body)
	}
	var pv struct {
		Checks []struct {
			Key   string
			Error string
		}
		Channels []struct{ Key string }
		Skipped  []struct{ Name string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pv); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, c := range pv.Checks {
		if c.Error != "" {
			t.Errorf("check %s would fail: %s", c.Key, c.Error)
		}
		keys = append(keys, c.Key)
	}
	for _, c := range pv.Channels {
		keys = append(keys, c.Key)
	}
	if len(pv.Checks) != 7 || len(pv.Channels) != 3 || len(pv.Skipped) != 6 {
		t.Fatalf("preview counts: %d checks, %d channels, %d skipped", len(pv.Checks), len(pv.Channels), len(pv.Skipped))
	}

	body, _ = json.Marshal(map[string]any{"source": "uptime-kuma", "content": string(data), "selected": keys, "pause": true})
	w = do(h, "POST", "/api/import/monitors/apply", string(body), nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"checks":7`) || !strings.Contains(w.Body.String(), `"channels":3`) {
		t.Fatalf("apply: %d %s", w.Code, w.Body)
	}
	ctx := t.Context()
	checks, _ := db.Checks(ctx)
	chs, _ := db.Channels(ctx)
	if len(checks) != 7 || len(chs) != 3 {
		t.Fatalf("stored: %d checks, %d channels", len(checks), len(chs))
	}
	for _, c := range checks {
		if c.Enabled {
			t.Errorf("check %q should be paused", c.Name)
		}
		if c.Name == "Website" && len(c.Channels) != 2 {
			t.Errorf("website channels: %v", c.Channels)
		}
	}
	for _, c := range chs {
		if c.Kind == "discord" && !strings.Contains(c.Config, "secret-token") {
			t.Errorf("discord secret not stored in the channel: %s", c.Config)
		}
	}
	// The audit row names the source and counts, never secrets.
	rows, _ := db.AuditEntries(ctx, store.AuditFilter{Limit: 10})
	if len(rows) != 1 || rows[0].Action != "import.monitors" || !strings.Contains(rows[0].Target, "uptime-kuma") || strings.Contains(rows[0].Target, "secret") {
		t.Fatalf("audit: %+v", rows)
	}

	// A second run creates nothing and says why.
	w = do(h, "POST", "/api/import/monitors/apply", string(body), nil)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "Already exists") {
		t.Fatalf("re-run: %d %s", w.Code, w.Body)
	}
	checks, _ = db.Checks(ctx)
	chs, _ = db.Channels(ctx)
	if len(checks) != 7 || len(chs) != 3 {
		t.Fatalf("re-run changed data: %d checks, %d channels", len(checks), len(chs))
	}
}

func TestMonitorImportGatusPage(t *testing.T) {
	h, db := cliServer(t, config.Config{})
	data, err := os.ReadFile("../migrate/testdata/gatus.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Only the checks and the page: the channels stay out.
	body, _ := json.Marshal(map[string]any{"source": "gatus", "content": string(data),
		"selected": []string{"check:0", "check:1", "check:3", "page:gatus"}})
	w := do(h, "POST", "/api/import/monitors/apply", string(body), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", w.Code, w.Body)
	}
	pages, _ := db.StatusPages(t.Context())
	if len(pages) != 1 || pages[0].Published || pages[0].Slug != "gatus" || len(pages[0].Items) != 3 {
		t.Fatalf("page: %+v", pages)
	}
	chs, _ := db.Channels(t.Context())
	if len(chs) != 0 {
		t.Fatalf("channels created without selection: %+v", chs)
	}
}

func TestMonitorImportRejects(t *testing.T) {
	h, _ := cliServer(t, config.Config{})
	for name, body := range map[string]string{
		"unknown source": `{"source":"nagios","content":"x"}`,
		"broken file":    `{"source":"gatus","content":"endpoints: ["}`,
		"nothing picked": `{"source":"gatus","content":"endpoints: []","selected":[]}`,
	} {
		path := "/api/import/monitors/preview"
		if name == "nothing picked" {
			path = "/api/import/monitors/apply"
		}
		if w := do(h, "POST", path, body, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	big := `{"source":"gatus","content":"` + strings.Repeat("a", 6<<20) + `"}`
	if w := do(h, "POST", "/api/import/monitors/preview", big, nil); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("too large: %d", w.Code)
	}
}

func TestDemoModeIsReadOnly(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := auth.New(db, log)
	if err != nil {
		t.Fatal(err)
	}
	h := New(config.Config{Demo: true}, a, db, nil, nil, nil, log, "test").Handler()
	csrf := map[string]string{csrfHeader: "1"}

	w := do(h, "GET", "/api/session", "", nil)
	if !strings.Contains(w.Body.String(), `"demo":true`) || !strings.Contains(w.Body.String(), `"username":"demo"`) ||
		strings.Contains(w.Body.String(), `"setupRequired":true`) {
		t.Fatalf("session: %s", w.Body)
	}
	if w := do(h, "GET", "/api/checks", "", nil); w.Code != http.StatusOK {
		t.Errorf("reading checks: %d %s", w.Code, w.Body)
	}
	for _, rq := range []struct{ method, path string }{
		{"POST", "/api/checks"},
		{"PUT", "/api/settings/config"},
		{"DELETE", "/api/hosts/local/stacks/x"},
		{"POST", "/api/hosts/local/stacks/x/actions/up"},
		{"POST", "/api/terminal/ticket"},
		{"POST", "/api/setup/start"},
		{"POST", "/api/login"},
		{"POST", "/api/import/monitors/apply"},
		{"POST", "/mcp"},
	} {
		w := do(h, rq.method, rq.path, `{}`, csrf)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "read-only demo") {
			t.Errorf("%s %s: %d %s", rq.method, rq.path, w.Code, w.Body)
		}
	}
	if rows, _ := db.AuditEntries(t.Context(), store.AuditFilter{Limit: 10}); len(rows) != 0 {
		t.Errorf("demo wrote audit rows: %+v", rows)
	}
}
