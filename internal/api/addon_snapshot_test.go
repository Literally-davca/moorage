package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"moorage/internal/monitor"
	"moorage/internal/snapshot"
	"moorage/internal/status"
	"moorage/internal/store"
)

// The snapshot holds only what the public page shows: no check names, targets, hosts,
// stacks, probe messages or incident causes, and unpublished pages are never rendered.
func TestStatusSnapshotRenderNoLeakOnlyPublished(t *testing.T) {
	srv, db, mon, _, _ := newStatusTestEnv(t, "https://moorage.example.com")
	ctx := context.Background()
	now := time.Now().UTC()
	chk, err := db.SaveCheck(ctx, store.Check{
		Name: "internal-db-check", Kind: monitor.KindHTTP, Target: "http://secret.lan:8080/health",
		Interval: 60, Timeout: 5, Retries: 1, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	mon.SetState(chk, monitor.State{Status: monitor.StatusDown, Since: now.Add(-time.Minute),
		Last: &monitor.LastProbe{At: now, Status: monitor.StatusDown, Msg: "password=hunter2"}})
	if _, err := db.OpenIncident(ctx, chk, now.Add(-time.Minute), "password=hunter2 refused", `{"host":"pi-home"}`); err != nil {
		t.Fatal(err)
	}
	pubID, err := db.SaveStatusPage(ctx, store.StatusPage{Slug: "public", Title: `Status <b>x</b>`, Domain: "status.example.com", Published: true, ShowIncidents: true,
		Items: []store.StatusPageItem{{Section: "Core", DisplayName: "Database", CheckID: chk}}})
	if err != nil {
		t.Fatal(err)
	}
	draftID, err := db.SaveStatusPage(ctx, store.StatusPage{Slug: "draft", Title: "Draft", Published: false,
		Items: []store.StatusPageItem{{DisplayName: "Database", CheckID: chk}}})
	if err != nil {
		t.Fatal(err)
	}

	files, slugs, err := srv.renderStatusSnapshot(ctx, snapshot.Config{Stale: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(slugs) != 1 || slugs[0] != "public" || len(files) != 2 {
		t.Fatalf("slugs %v, %d files", slugs, len(files))
	}
	for _, f := range files {
		if !strings.HasPrefix(f.Path, "public/") {
			t.Errorf("unexpected file %s", f.Path)
		}
		body := string(f.Data)
		for _, secret := range []string{"internal-db-check", "secret.lan", "8080/health", "hunter2", "pi-home", "status.example.com", "moorage.example.com", "draft"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s leaks %q", f.Path, secret)
			}
		}
		if !strings.Contains(body, "Database") {
			t.Errorf("%s lacks the public item name", f.Path)
		}
	}
	var doc status.SnapshotDoc
	if err := json.Unmarshal(files[0].Data, &doc); err != nil || files[0].Path != "public/status.json" || doc.StaleAfterSeconds != 1200 {
		t.Fatalf("status.json: %+v %v", doc, err)
	}
	if !strings.Contains(string(files[1].Data), `data-stale-after="1200"`) || !strings.Contains(string(files[1].Data), "Status &lt;b&gt;x&lt;/b&gt;") {
		t.Fatalf("index.html:\n%s", files[1].Data)
	}

	// Picking only the unpublished page publishes nothing.
	files, _, err = srv.renderStatusSnapshot(ctx, snapshot.Config{Pages: []int64{draftID}, Stale: 20})
	if err != nil || len(files) != 0 {
		t.Fatalf("draft rendered: %d files, %v", len(files), err)
	}
	files, _, _ = srv.renderStatusSnapshot(ctx, snapshot.Config{Pages: []int64{pubID}, Stale: 20})
	if len(files) != 2 {
		t.Fatalf("picked page: %d files", len(files))
	}
}

// End to end through the API: secrets are write-only, every write is audited, Test and
// Publish now reach a WebDAV target, and status page edits kick the publisher.
func TestStatusSnapshotAPI(t *testing.T) {
	const password = "dav-secret-value"
	var mu sync.Mutex
	files := map[string]string{}
	dav := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, p, _ := r.BasicAuth(); p != password {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			files[r.URL.Path] = string(b)
			w.WriteHeader(http.StatusCreated)
		case "MKCOL":
			w.WriteHeader(http.StatusCreated)
		case http.MethodDelete:
			delete(files, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer dav.Close()

	srv, db, _, h, _ := newStatusTestEnv(t, "")
	ctx := context.Background()
	createTestUserSession(t, db, "admin", "admin", "tok-admin")
	createTestUserSession(t, db, "viewer", "viewer", "tok-viewer")
	call := func(method, path, body, tok string) *httptest.ResponseRecorder {
		return do(h, method, path, body, map[string]string{csrfHeader: "1", "Content-Type": "application/json", "Cookie": cookieName + "=" + tok})
	}
	if _, err := db.SaveStatusPage(ctx, store.StatusPage{Slug: "main", Title: "Main", Published: true}); err != nil {
		t.Fatal(err)
	}

	if w := call("GET", "/api/addons/status-snapshot/config", "", "tok-viewer"); w.Code != http.StatusForbidden {
		t.Fatalf("viewer read the config: %d", w.Code)
	}
	cfg := `{"provider":"webdav","webdavUrl":"` + dav.URL + `/site","webdavUsername":"ann","webdavPassword":"` + password + `","intervalMinutes":5,"staleMinutes":15}`
	w := call("PUT", "/api/addons/status-snapshot/config", cfg, "tok-admin")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), password) || !strings.Contains(w.Body.String(), `"webdavPasswordSet":true`) {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	w = call("GET", "/api/addons/status-snapshot/config", "", "tok-admin")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), password) {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
	// The form sends no password: the saved one is used for the same folder…
	noPw := `{"provider":"webdav","webdavUrl":"` + dav.URL + `/site","webdavUsername":"ann"}`
	if w := call("POST", "/api/addons/status-snapshot/test", noPw, "tok-admin"); w.Code != http.StatusOK {
		t.Fatalf("test: %d %s", w.Code, w.Body)
	}
	// …but never for another one.
	other := `{"provider":"webdav","webdavUrl":"` + dav.URL + `/elsewhere","webdavUsername":"ann"}`
	if w := call("POST", "/api/addons/status-snapshot/test", other, "tok-admin"); w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), password) {
		t.Fatalf("test other folder: %d %s", w.Code, w.Body)
	}

	// Publish now needs the add-on on.
	if w := call("POST", "/api/addons/status-snapshot/publish", "", "tok-admin"); w.Code != http.StatusBadRequest {
		t.Fatalf("publish while off: %d %s", w.Code, w.Body)
	}
	if w := call("PUT", "/api/addons/status-snapshot", `{"enabled":true}`, "tok-admin"); w.Code != http.StatusNoContent {
		t.Fatalf("enable: %d %s", w.Code, w.Body)
	}
	w = call("POST", "/api/addons/status-snapshot/publish", "", "tok-admin")
	if w.Code != http.StatusOK {
		t.Fatalf("publish: %d %s", w.Code, w.Body)
	}
	mu.Lock()
	html, js := files["/site/main/index.html"], files["/site/main/status.json"]
	mu.Unlock()
	if !strings.Contains(html, "Data may be out of date") || !strings.Contains(js, `"slug": "main"`) {
		t.Fatalf("published files: %v", files)
	}
	w = call("GET", "/api/addons/status-snapshot/config", "", "tok-admin")
	var dto statusSnapshotDTO
	if err := json.Unmarshal(w.Body.Bytes(), &dto); err != nil || dto.Last == nil || !dto.Last.OK || dto.Stale || !dto.Enabled {
		t.Fatalf("status after publish: %+v %v", dto, err)
	}

	rows := auditRows(t, db)
	actions := map[string]bool{}
	for _, r := range rows {
		actions[r.Action] = true
		if strings.Contains(r.Target, password) {
			t.Fatalf("audit row holds the secret: %+v", r)
		}
	}
	for _, a := range []string{"status_snapshot.config", "status_snapshot.test", "status_snapshot.publish", "addon.toggle"} {
		if !actions[a] {
			t.Errorf("no audit row %s in %v", a, actions)
		}
	}
	_ = srv
}

func TestStatusSnapshotNotStartedInDemo(t *testing.T) {
	srv, _, _, _, _ := newStatusTestEnv(t, "")
	srv.cfg.Demo = true
	srv.StartStatusSnapshot(context.Background())
	if srv.snapshotRunning.Load() {
		t.Fatal("the publisher runs in demo mode")
	}
}
