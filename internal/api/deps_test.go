package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"moorage/internal/auth"
	"moorage/internal/config"
	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/store"
)

// stacksHost is a local host that only lists stacks (no dependency facts: like an old agent).
type stacksHost struct {
	host.Host
	list []engine.StackSummary
}

func (h stacksHost) ListStacks(context.Context) ([]engine.StackSummary, error) { return h.list, nil }

func newDepsEnv(t *testing.T) *auditEnv {
	t.Helper()
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
	reg := host.NewRegistry(stacksHost{list: []engine.StackSummary{{Name: "pg", Status: "up"}, {Name: "app", Status: "down"}}}, "box", false, db)
	srv := New(config.Config{}, a, db, reg, nil, nil, log, "test")
	return &auditEnv{db: db, srv: srv, h: srv.Handler()}
}

func readDeps(t *testing.T, e *auditEnv, token string) depsResponse {
	t.Helper()
	w := e.req("GET", "/api/hosts/local/deps", "", token)
	if w.Code != http.StatusOK {
		t.Fatalf("GET deps: %d %s", w.Code, w.Body)
	}
	var r depsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDepsAPI(t *testing.T) {
	e := newDepsEnv(t)
	ctx := context.Background()

	r := readDeps(t, e, "tok-viewer")
	if len(r.Edges) != 0 || len(r.Nodes) != 2 || r.Nodes[0].Stack != "app" || r.Nodes[0].Status != "down" {
		t.Fatalf("empty graph: %+v", r)
	}
	if w := e.req("POST", "/api/hosts/local/stacks/app/deps", `{"stack":"pg"}`, "tok-viewer"); w.Code != http.StatusForbidden {
		t.Fatalf("viewer add: %d", w.Code)
	}
	for body, want := range map[string]int{
		`{"stack":"app"}`:                   http.StatusBadRequest,
		`{"stack":"../x"}`:                  http.StatusBadRequest,
		`{"stack":"pg","hostId":"nowhere"}`: http.StatusNotFound,
	} {
		if w := e.req("POST", "/api/hosts/local/stacks/app/deps", body, "tok-admin"); w.Code != want {
			t.Errorf("%s: got %d, want %d", body, w.Code, want)
		}
	}
	w := e.req("POST", "/api/hosts/local/stacks/app/deps", `{"stack":"pg"}`, "tok-admin")
	if w.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	var manual store.StackDep
	json.Unmarshal(w.Body.Bytes(), &manual) //nolint:errcheck
	if manual.Source != store.DepManual || manual.CreatedBy != "admin" {
		t.Fatalf("manual edge: %+v", manual)
	}
	if up := e.srv.deps.Upstream(store.StackRef{HostID: "local", Stack: "app"}); len(up) != 1 {
		t.Fatal("the graph wasn't reloaded after adding")
	}

	// a detected edge: can be ignored, not removed
	if _, _, err := e.db.ReplaceDetected(ctx, "local", []store.StackDep{{
		From: store.StackRef{HostID: "local", Stack: "web"}, To: store.StackRef{HostID: "local", Stack: "pg"}, Reason: "network", Detail: "external network pg_b",
	}}); err != nil {
		t.Fatal(err)
	}
	var detected store.StackDep
	for _, d := range readDeps(t, e, "tok-admin").Edges {
		if d.Source == store.DepDetected {
			detected = d
		}
	}
	path := "/api/hosts/local/stacks/web/deps/" + itoa(detected.ID)
	if w := e.req("DELETE", path, "", "tok-admin"); w.Code != http.StatusConflict {
		t.Fatalf("delete detected: %d", w.Code)
	}
	if w := e.req("PUT", path+"/ignored", `{"ignored":true}`, "tok-admin"); w.Code != http.StatusNoContent {
		t.Fatalf("ignore: %d %s", w.Code, w.Body)
	}
	if d, _ := e.db.StackDepByID(ctx, detected.ID); !d.Ignored {
		t.Fatal("not ignored")
	}
	mpath := "/api/hosts/local/stacks/app/deps/" + itoa(manual.ID)
	if w := e.req("PUT", mpath+"/ignored", `{"ignored":true}`, "tok-admin"); w.Code != http.StatusConflict {
		t.Fatalf("ignore manual: %d", w.Code)
	}
	// the edge must touch the stack in the path
	if w := e.req("DELETE", "/api/hosts/local/stacks/other/deps/"+itoa(manual.ID), "", "tok-admin"); w.Code != http.StatusNotFound {
		t.Fatalf("delete through another stack: %d", w.Code)
	}

	// duplicate and loop edges are refused on manual add, while detected loops are reported
	if w := e.req("POST", "/api/hosts/local/stacks/app/deps", `{"stack":"pg"}`, "tok-admin"); w.Code != http.StatusConflict {
		t.Fatalf("duplicate add: %d", w.Code)
	}
	if w := e.req("POST", "/api/hosts/local/stacks/pg/deps", `{"stack":"app"}`, "tok-admin"); w.Code != http.StatusConflict {
		t.Fatalf("manual loop add: %d", w.Code)
	}
	if w := e.req("PUT", mpath+"/auto-restart", `{"mode":"on_recover"}`, "tok-admin"); w.Code != http.StatusNoContent {
		t.Fatalf("set auto-restart: %d %s", w.Code, w.Body)
	}
	if d, _ := e.db.StackDepByID(ctx, manual.ID); d.AutoRestart != store.AutoRestartRecover {
		t.Fatalf("auto-restart not stored: %+v", d)
	}
	if _, _, err := e.db.ReplaceDetected(ctx, "local", []store.StackDep{{
		From: store.StackRef{HostID: "local", Stack: "pg"}, To: store.StackRef{HostID: "local", Stack: "app"}, Reason: "hostname", Detail: "app",
	}}); err != nil {
		t.Fatal(err)
	}
	e.srv.reloadDeps(ctx)
	r = readDeps(t, e, "tok-viewer")
	if len(r.Cycles) != 1 || !r.Nodes[0].InCycle {
		t.Fatalf("cycle not reported: %+v", r)
	}
	if w := e.req("DELETE", mpath, "", "tok-admin"); w.Code != http.StatusNoContent {
		t.Fatalf("delete manual: %d", w.Code)
	}
	if w := e.req("POST", "/api/hosts/local/deps/detect", "", "tok-admin"); w.Code != http.StatusAccepted {
		t.Fatalf("detect: %d", w.Code)
	}

	got := map[string]int{}
	for _, row := range auditRows(t, e.db) {
		got[row.Action+" "+row.Stack]++
	}
	for _, want := range []string{"dep.add app", "dep.auto_restart app", "dep.ignore web", "dep.remove app", "dep.detect "} {
		if got[want] == 0 {
			t.Errorf("audit row %q missing: %v", want, got)
		}
	}

	// an older local host without facts reports "unsupported"
	deadline := time.Now().Add(3 * time.Second)
	for e.srv.deps.Status("local").At.IsZero() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if d := readDeps(t, e, "tok-viewer").Detection; !d.Unsupported {
		t.Fatalf("detection: %+v", d)
	}
}

func TestTimelineShowsCascadeCause(t *testing.T) {
	e := newTimelineEnv(t)
	id := e.incident(t, "web-http", tlBase, time.Time{})
	if err := e.db.SetIncidentCause(e.ctx, id, "local/pg"); err != nil {
		t.Fatal(err)
	}
	code, p := e.get(t, "/api/hosts/local/stacks/web/timeline?kinds=incident", "tok-viewer")
	if code != http.StatusOK || len(p.Entries) != 1 {
		t.Fatalf("timeline: %d %+v", code, p)
	}
	x := p.Entries[0].Incident
	if x == nil || x.CausedBy == nil || x.CausedBy.Stack != "pg" || p.Entries[0].Detail != "container exited · probably caused by pg" {
		t.Fatalf("entry: %+v", p.Entries[0])
	}
}

func TestAllDepsAPI(t *testing.T) {
	e := newDepsEnv(t)
	if w := e.req("POST", "/api/hosts/local/stacks/app/deps", `{"stack":"pg"}`, "tok-admin"); w.Code != http.StatusCreated {
		t.Fatalf("add dep: %d %s", w.Code, w.Body)
	}
	r := readDeps(t, e, "tok-viewer")
	if len(r.Hosts) != 1 || r.Hosts[0].ID != "local" || r.Hosts[0].Kind != "local" || !r.Hosts[0].Connected {
		t.Fatalf("hostDeps hosts: %+v", r.Hosts)
	}
	w := e.req("GET", "/api/deps", "", "tok-viewer")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/deps: %d %s", w.Code, w.Body)
	}
	var all depsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if len(all.Hosts) != 1 || len(all.Nodes) != 2 || len(all.Edges) != 1 {
		t.Fatalf("allDeps: %+v", all)
	}
}
