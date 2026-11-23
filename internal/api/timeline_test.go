package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"moorage/internal/auth"
	"moorage/internal/config"
	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/store"
	"moorage/internal/updater"
)

var tlBase = time.Unix(1_700_000_000, 0)

type tlEnv struct {
	db  *store.Store
	srv *Server
	h   http.Handler
	reg *host.Registry
	ctx context.Context
}

func newTimelineEnv(t *testing.T) *tlEnv {
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
	// The local engine has no history repository: versions fail with ErrHistoryOff.
	reg := host.NewRegistry(host.Local{E: &engine.Engine{}}, "local", false, db)
	srv := New(config.Config{}, a, db, reg, nil, nil, log, "test")
	return &tlEnv{db: db, srv: srv, h: srv.Handler(), reg: reg, ctx: context.Background()}
}

func (e *tlEnv) get(t *testing.T, path, token string) (int, TimelinePage) {
	t.Helper()
	w := do(e.h, "GET", path, "", map[string]string{"Cookie": cookieName + "=" + token})
	var p TimelinePage
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code, p
}

func (e *tlEnv) event(t *testing.T, at time.Time, kind, actor, detail string) {
	t.Helper()
	if err := e.db.AddEvent(e.ctx, store.Event{TS: at, HostID: "local", Stack: "web", Kind: kind, Actor: actor, Detail: detail}); err != nil {
		t.Fatal(err)
	}
}

func (e *tlEnv) audit(t *testing.T, at time.Time, action, actor, target string) {
	t.Helper()
	if err := e.db.Audit(e.ctx, store.AuditEntry{At: at, Actor: actor, IP: "10.0.0.9", Action: action, HostID: "local", Stack: "web", Target: target}); err != nil {
		t.Fatal(err)
	}
}

func (e *tlEnv) incident(t *testing.T, name string, start, end time.Time) int64 {
	t.Helper()
	cid, err := e.db.SaveCheck(e.ctx, store.Check{Name: name, Kind: "stack", Target: "web", HostID: "local", Stack: "web",
		Interval: 30, Timeout: 5, Retries: 1, Settings: "{}", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	id, err := e.db.OpenIncident(e.ctx, cid, start, "container exited", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if !end.IsZero() {
		if err := e.db.CloseIncident(e.ctx, id, end); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func (e *tlEnv) update(t *testing.T, at time.Time, actor string) int64 {
	t.Helper()
	id, err := e.db.AddImageUpdate(e.ctx, store.ImageUpdate{HostID: "local", Stack: "web", StartedAt: at, FinishedAt: at.Add(time.Minute),
		Actor: actor, Result: "ok", Services: []store.ImageUpdateService{{Service: "app", Ref: "nginx", OldDigest: "sha256:aaaaaaaaa", NewDigest: "sha256:bbbbbbbbb"}}})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *tlEnv) terminal(t *testing.T, at time.Time, user string) {
	t.Helper()
	if _, err := e.db.StartTerminalSession(e.ctx, store.TerminalAuditRow{User: user, IP: "10.0.0.9", HostID: "local", Stack: "web",
		ContainerName: "web-app-1", Shell: "/bin/sh", StartedAt: at}); err != nil {
		t.Fatal(err)
	}
}

func fakeVersions(vs ...engine.Version) versionSource {
	return func(context.Context) ([]engine.Version, error) { return vs, nil }
}

func ver(hashByte byte, at time.Time, kind, author string) engine.Version {
	h := strings.Repeat(string(rune(hashByte)), 40)
	return engine.Version{Hash: h, Short: h[:7], Message: kind + " compose.yaml", Author: author, Time: at, Kind: kind}
}

func allKinds() map[string]bool {
	m := map[string]bool{}
	for _, k := range timelineKinds {
		m[k] = true
	}
	return m
}

// collect pages through the timeline with the given page size and returns all entries.
func collect(t *testing.T, e *tlEnv, q tlQuery, vs versionSource) []TimelineEntry {
	t.Helper()
	var out []TimelineEntry
	for i := 0; i < 200; i++ {
		p, err := e.srv.buildTimeline(e.ctx, q, vs)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p.Entries...)
		if !p.More {
			return out
		}
		c, err := parseCursor(p.Next)
		if err != nil {
			t.Fatalf("next %q: %v", p.Next, err)
		}
		if q.cursor.at != 0 && c.cmp(q.cursor) >= 0 {
			t.Fatalf("cursor did not move: %v -> %v", q.cursor, c)
		}
		q.cursor = c
	}
	t.Fatal("paging did not end")
	return nil
}

func keysOf(es []TimelineEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Key
	}
	return out
}

func TestTimelineMergeOrderAndStablePaging(t *testing.T) {
	e := newTimelineEnv(t)
	// Every source has rows in the very same second, plus some around it. No pair here
	// matches a merge rule (different users), so every row is its own entry.
	for _, d := range []time.Duration{-time.Second, 0, 0, 0, time.Second} {
		at := tlBase.Add(d)
		e.event(t, at, "deploy", "ev", "")
		e.audit(t, at, "image.policy", "au", "")
		e.incident(t, "api", at, at)
		e.update(t, at, "up")
		e.terminal(t, at, "te")
	}
	vs := fakeVersions(
		ver('a', tlBase.Add(time.Second), engine.KindEdit, "ve"),
		ver('b', tlBase, engine.KindEdit, "ve"),
		ver('c', tlBase, engine.KindExternal, engine.SystemAuthor),
		ver('d', tlBase.Add(-time.Second), engine.KindCreate, "ve"),
	)
	q := tlQuery{hostID: "local", stack: "web", kinds: allKinds(), limit: 500}
	all := collect(t, e, q, vs)
	// 5 events, 5 audits, 5 incident starts + 5 ends, 5 updates, 5 terminals, 4 versions.
	if len(all) != 34 {
		t.Fatalf("got %d entries, want 34", len(all))
	}
	for i := 1; i < len(all); i++ {
		a, _ := parseCursor(all[i-1].Key)
		b, _ := parseCursor(all[i].Key)
		if a.cmp(b) <= 0 {
			t.Fatalf("entries %d and %d out of order: %s, %s", i-1, i, all[i-1].Key, all[i].Key)
		}
	}
	want := strings.Join(keysOf(all), "\n")
	for _, limit := range []int{1, 2, 3, 5, 7, 13} {
		q.limit = limit
		got := collect(t, e, q, vs)
		if g := strings.Join(keysOf(got), "\n"); g != want {
			t.Fatalf("limit %d: paged result differs from one page:\n%s\n---\n%s", limit, g, want)
		}
	}
}

func TestTimelineRolesServerSide(t *testing.T) {
	e := newTimelineEnv(t)
	e.event(t, tlBase, "deploy", "admin", "")
	e.audit(t, tlBase.Add(time.Hour), "image.policy", "admin", "")
	e.terminal(t, tlBase.Add(2*time.Hour), "admin")
	e.event(t, tlBase.Add(2*time.Hour), "terminal", "admin", "app · /bin/sh")

	kindsOf := func(p TimelinePage) string {
		var ks []string
		for _, x := range p.Entries {
			ks = append(ks, x.Kind)
		}
		return strings.Join(ks, ",")
	}
	path := "/api/hosts/local/stacks/web/timeline"
	code, p := e.get(t, path, "tok-viewer")
	if code != 200 || kindsOf(p) != "event" {
		t.Fatalf("viewer: %d %q", code, kindsOf(p))
	}
	if p.VersionsError == "" {
		t.Error("versions failed but the page doesn't say so")
	}
	for _, x := range p.Entries {
		if x.IP != "" {
			t.Errorf("viewer got an IP: %+v", x)
		}
	}
	code, p = e.get(t, path+"?kinds=audit,terminal", "tok-viewer")
	if code != 200 || len(p.Entries) != 0 {
		t.Fatalf("viewer asking for audit/terminal: %d %+v", code, p.Entries)
	}
	code, p = e.get(t, path, "tok-admin")
	if code != 200 || kindsOf(p) != "terminal,audit,event" {
		t.Fatalf("admin: %d %q (terminal stack events must not show)", code, kindsOf(p))
	}
}

func TestTimelineDeduplication(t *testing.T) {
	e := newTimelineEnv(t)
	t0 := tlBase
	// A deploy: audit row 1 s after the event.
	e.event(t, t0, "deploy", "admin", "")
	e.audit(t, t0.Add(time.Second), "stack.up", "admin", "")
	// A save: version, event and audit row of the same action.
	t1 := t0.Add(time.Hour)
	vs := fakeVersions(ver('e', t1, engine.KindEdit, "admin"))
	e.event(t, t1, "edit", "admin", "")
	e.audit(t, t1, "stack.save", "admin", "")
	// Two deploys 3 s apart by the same user: two entries, each with its own audit row.
	t2 := t0.Add(2 * time.Hour)
	e.event(t, t2, "deploy", "admin", "")
	e.audit(t, t2, "stack.up", "admin", "")
	e.event(t, t2.Add(3*time.Second), "deploy", "admin", "")
	e.audit(t, t2.Add(3*time.Second), "stack.up", "admin", "")
	// A manual image update: audit, event and update record.
	t3 := t0.Add(3 * time.Hour)
	e.update(t, t3, "admin")
	e.event(t, t3.Add(2*time.Second), "update", "admin", "")
	e.audit(t, t3.Add(2*time.Second), "image.update", "admin", "")
	// An auto-update: its audit row comes minutes later and is linked by id.
	t4 := t0.Add(4 * time.Hour)
	uid := e.update(t, t4, "auto-update")
	e.event(t, t4, "update", "auto-update", "")
	e.audit(t, t4.Add(5*time.Minute), updater.AuditAutoUpdate, store.AuditActorSystem, fmt.Sprintf("update #%d", uid))
	// Different users or too far apart: never merged.
	t5 := t0.Add(5 * time.Hour)
	e.event(t, t5, "deploy", "bob", "")
	e.audit(t, t5, "stack.up", "admin", "")
	e.event(t, t5.Add(time.Minute), "restart", "admin", "")
	e.audit(t, t5.Add(time.Minute+30*time.Second), "stack.restart", "admin", "")

	q := tlQuery{hostID: "local", stack: "web", kinds: allKinds(), limit: 500}
	check := func(all []TimelineEntry) {
		t.Helper()
		var got []string
		for _, x := range all {
			got = append(got, fmt.Sprintf("%s:%s:%s:%s", x.Kind, x.Title, strings.Join(x.Merged, "+"), x.IP))
		}
		want := []string{
			"audit:stack.restart::10.0.0.9",
			"event:Restarted::",
			"event:Deployed::",
			"audit:stack.up::10.0.0.9",
			"update:Image update (ok):event:",
			"update:Image update (ok):event+audit:10.0.0.9",
			"event:Deployed:audit:10.0.0.9",
			"event:Deployed:audit:10.0.0.9",
			"version:Edited:event+audit:10.0.0.9",
			"event:Deployed:audit:10.0.0.9",
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	check(collect(t, e, q, vs))
	// The same merges when every page holds one entry, so pairs straddle page edges.
	q.limit = 1
	check(collect(t, e, q, vs))

	// Without events in the filter, audit rows are listed on their own.
	q = tlQuery{hostID: "local", stack: "web", kinds: map[string]bool{tlAudit: true}, limit: 500}
	if n := len(collect(t, e, q, vs)); n != 8 {
		t.Fatalf("audit-only view: %d rows, want 8", n)
	}
}

func TestTimelineKindsFilter(t *testing.T) {
	e := newTimelineEnv(t)
	e.event(t, tlBase, "deploy", "admin", "")
	e.incident(t, "api", tlBase.Add(time.Minute), time.Time{})
	e.update(t, tlBase.Add(2*time.Minute), "admin")
	path := "/api/hosts/local/stacks/web/timeline"
	code, p := e.get(t, path+"?kinds=incident", "tok-viewer")
	if code != 200 || len(p.Entries) != 1 || p.Entries[0].Kind != tlIncident || p.Entries[0].Incident.Phase != "open" {
		t.Fatalf("kinds=incident: %d %+v", code, p.Entries)
	}
	if p.VersionsError != "" {
		t.Error("versions were not asked for, yet an error is reported")
	}
	code, p = e.get(t, path+"?kinds=event,update", "tok-viewer")
	if code != 200 || len(p.Entries) != 2 {
		t.Fatalf("kinds=event,update: %d %+v", code, p.Entries)
	}
	for _, bad := range []string{"?kinds=nope", "?limit=0", "?before=garbage", "?until=x", "?before=1~version~123"} {
		if code, _ := e.get(t, path+bad, "tok-viewer"); code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", bad, code)
		}
	}
	if code, _ := e.get(t, "/api/hosts/nohost/stacks/web/timeline", "tok-viewer"); code != http.StatusNotFound {
		t.Errorf("unknown host: %d", code)
	}
	if code, _ := e.get(t, path+"?limit=100000", "tok-viewer"); code != 200 {
		t.Errorf("a big limit is capped, not refused: %d", code)
	}
}

func TestTimelineBeforeIncidentWindow(t *testing.T) {
	e := newTimelineEnv(t)
	start := tlBase
	e.event(t, start.Add(-3*time.Hour), "deploy", "admin", "too early")
	e.event(t, start.Add(-90*time.Minute), "deploy", "admin", "in window")
	e.event(t, start.Add(-2*time.Hour), "restart", "admin", "window edge")
	e.event(t, start.Add(10*time.Minute), "restart", "admin", "after")
	e.update(t, start.Add(-time.Hour), "admin")
	e.incident(t, "api", start, start.Add(time.Minute))

	v := url.Values{}
	v.Set("until", fmt.Sprint(start.Unix()))
	v.Set("since", fmt.Sprint(start.Add(-2*time.Hour).Unix()))
	code, p := e.get(t, "/api/hosts/local/stacks/web/timeline?"+v.Encode(), "tok-viewer")
	if code != 200 {
		t.Fatalf("code %d", code)
	}
	var got []string
	for _, x := range p.Entries {
		got = append(got, x.Kind+":"+x.Detail)
	}
	want := "incident:container exited|update:app: aaaaaaa → bbbbbbb|event:in window|event:window edge"
	if strings.Join(got, "|") != want {
		t.Fatalf("got %s\nwant %s", strings.Join(got, "|"), want)
	}
	if p.More {
		t.Error("the window has no more pages")
	}
}

func TestTimelineVersionsFromHost(t *testing.T) {
	e := newTimelineEnv(t)
	e.event(t, tlBase, "deploy", "admin", "")
	v := ver('f', tlBase.Add(time.Minute), engine.KindEdit, "admin")
	newer := newAgentRemote(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stacks/web/history") {
			json.NewEncoder(w).Encode([]engine.Version{v}) //nolint:errcheck
			return
		}
		http.NotFound(w, r)
	}))
	older := newAgentRemote(t, http.NotFoundHandler()) // answers 404 to everything
	e.reg.Attach("a1", newer, host.AgentInfo{}, nil)
	e.reg.Attach("a2", older, host.AgentInfo{}, nil)
	for _, id := range []string{"a1", "a2"} {
		if err := e.db.AddEvent(e.ctx, store.Event{TS: tlBase, HostID: id, Stack: "web", Kind: "deploy", Actor: "admin"}); err != nil {
			t.Fatal(err)
		}
	}

	_, p := e.get(t, "/api/hosts/a1/stacks/web/timeline", "tok-viewer")
	if len(p.Entries) != 2 || p.Entries[0].Kind != tlVersion || p.Entries[0].Ref.Hash != v.Hash || p.VersionsError != "" {
		t.Fatalf("agent with versions: %+v", p)
	}
	_, p = e.get(t, "/api/hosts/a2/stacks/web/timeline", "tok-viewer")
	if len(p.Entries) != 1 || p.Entries[0].Kind != tlEvent || !strings.Contains(p.VersionsError, "doesn't list them") {
		t.Fatalf("older agent: %+v", p)
	}
	// A failing version source (history off, offline) keeps the hub-side entries too.
	page, err := e.srv.buildTimeline(e.ctx, tlQuery{hostID: "local", stack: "web", kinds: allKinds(), limit: 10},
		func(context.Context) ([]engine.Version, error) { return nil, errors.New("tunnel closed") })
	if err != nil || len(page.Entries) != 1 || page.VersionsError == "" {
		t.Fatalf("failing versions: %v %+v", err, page)
	}
}
