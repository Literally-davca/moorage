package monitor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"moorage/internal/deps"
	"moorage/internal/notify"
	"moorage/internal/store"
)

// graphOf adapts a deps.Graph built from edges "from>to" on host local to DepGraph.
type graphOf struct{ g *deps.Graph }

func (a graphOf) Upstream(r store.StackRef) []store.StackRef { return a.g.Upstream(r, deps.MaxDepth) }
func (a graphOf) Downstream(r store.StackRef) []store.StackRef {
	return a.g.Downstream(r, deps.MaxDepth)
}

func testGraph(pairs ...string) graphOf {
	var edges []store.StackDep
	for _, p := range pairs {
		from, to, _ := strings.Cut(p, ">")
		edges = append(edges, store.StackDep{From: store.StackRef{HostID: "local", Stack: from}, To: store.StackRef{HostID: "local", Stack: to}})
	}
	return graphOf{deps.Build(edges)}
}

type sentMsg struct {
	channel int64
	msg     notify.Message
}

// newCascadeMonitor records which channel every message went to.
func newCascadeMonitor(t *testing.T, pairs ...string) (*Monitor, *store.Store, *[]sentMsg) {
	t.Helper()
	m, st, _ := newTestMonitor(t)
	var mu sync.Mutex
	out := &[]sentMsg{}
	m.send = func(_ context.Context, ch store.Channel, msg notify.Message) error {
		mu.Lock()
		*out = append(*out, sentMsg{channel: ch.ID, msg: msg})
		mu.Unlock()
		return nil
	}
	m.SetDeps(testGraph(pairs...))
	return m, st, out
}

func titles(ms []sentMsg) []string {
	var out []string
	for _, s := range ms {
		out = append(out, s.msg.Title)
	}
	return out
}

func causeOf(t *testing.T, st *store.Store, checkID int64) string {
	t.Helper()
	ins, err := st.Incidents(context.Background(), checkID, 1)
	if err != nil || len(ins) == 0 {
		t.Fatalf("no incident for check %d: %v", checkID, err)
	}
	return ins[0].CausedBy
}

var downOut = Outcome{Msg: "connection refused"}
var upOut = Outcome{OK: true, Msg: "HTTP 200"}

// A down, then B (depends on A) down within the batch window: one message naming A, both
// incidents stored, B's linked to A.
func TestCascadeRootThenDependentIsOneMessage(t *testing.T) {
	m, st, sent := newCascadeMonitor(t, "app1>pg", "app2>pg")
	pg := saveTestCheck(t, st, "pg-tcp", "pg", 1)
	a1 := saveTestCheck(t, st, "app1-http", "app1", 1)
	a2 := saveTestCheck(t, st, "app2-http", "app2", 1)
	now := time.Now()
	m.handle(pg, downOut, now)
	m.handle(a1, downOut, now.Add(2*time.Second))
	m.handle(a2, downOut, now.Add(3*time.Second))
	m.flush("stack:local/pg")
	m.flush("stack:local/app1")
	m.flush("stack:local/app2")
	if len(*sent) != 1 {
		t.Fatalf("want one message, got %q", titles(*sent))
	}
	msg := (*sent)[0].msg
	if !strings.Contains(msg.Title, "DOWN: pg-tcp") || !strings.Contains(msg.Title, "also affected: app1, app2") {
		t.Fatalf("title: %q", msg.Title)
	}
	if !strings.Contains(msg.Text, "probably caused by pg") || !strings.Contains(msg.Text, "app1-http") {
		t.Fatalf("text: %q", msg.Text)
	}
	open, _ := st.OpenIncidents(context.Background())
	if len(open) != 3 {
		t.Fatalf("want 3 open incidents, got %d", len(open))
	}
	if c := causeOf(t, st, a1.ID); c != "local/pg" {
		t.Fatalf("app1 cause %q", c)
	}
	if c := causeOf(t, st, pg.ID); c != "" {
		t.Fatalf("root has a cause: %q", c)
	}
}

// The root was announced already: later dependents come as "also affected by" with the root
// named, never silently dropped.
func TestCascadeDependentAfterRootWasAnnounced(t *testing.T) {
	m, st, sent := newCascadeMonitor(t, "app1>pg")
	pg := saveTestCheck(t, st, "pg-tcp", "pg", 1)
	a1 := saveTestCheck(t, st, "app1-http", "app1", 1)
	now := time.Now()
	m.handle(pg, downOut, now)
	m.flush("stack:local/pg")
	m.handle(a1, downOut, now.Add(3*time.Minute))
	m.mu.Lock()
	b := m.batches["stack:local/pg"]
	m.mu.Unlock()
	if b == nil || time.Until(b.due) < 30*time.Second {
		t.Fatal("a dependent of an announced root should wait DependentWindow in the root's batch")
	}
	m.flush("stack:local/pg")
	if len(*sent) != 2 {
		t.Fatalf("want 2 messages, got %q", titles(*sent))
	}
	msg := (*sent)[1].msg
	if !strings.HasPrefix(msg.Title, "Also affected by the pg outage: app1") || !strings.Contains(msg.Text, "Probably caused by pg") {
		t.Fatalf("dependent message: %q / %q", msg.Title, msg.Text)
	}
}

// B down first (alert already sent), then A: B's alert is not lost, and A's message says B
// was affected and alerted earlier.
func TestCascadeDependentBeforeRoot(t *testing.T) {
	m, st, sent := newCascadeMonitor(t, "app1>pg")
	pg := saveTestCheck(t, st, "pg-tcp", "pg", 1)
	a1 := saveTestCheck(t, st, "app1-http", "app1", 1)
	now := time.Now()
	m.handle(a1, downOut, now)
	m.flush("stack:local/app1")
	m.handle(pg, downOut, now.Add(20*time.Second))
	m.flush("stack:local/pg")
	if len(*sent) != 2 {
		t.Fatalf("want 2 messages, got %q", titles(*sent))
	}
	if !strings.HasPrefix((*sent)[0].msg.Title, "DOWN: app1-http") {
		t.Fatalf("first: %q", (*sent)[0].msg.Title)
	}
	if got := (*sent)[1].msg.Title; !strings.Contains(got, "DOWN: pg-tcp") || !strings.Contains(got, "app1 (alerted earlier)") {
		t.Fatalf("root title: %q", got)
	}
	if c := causeOf(t, st, a1.ID); c != "local/pg" {
		t.Fatalf("app1 should be linked to pg afterwards, got %q", c)
	}
}

// B down a few seconds before A, still in its batch: it moves into A's message.
func TestCascadePendingDependentJoinsRoot(t *testing.T) {
	m, st, sent := newCascadeMonitor(t, "app1>pg")
	pg := saveTestCheck(t, st, "pg-tcp", "pg", 1)
	a1 := saveTestCheck(t, st, "app1-http", "app1", 1)
	now := time.Now()
	m.handle(a1, downOut, now)
	m.handle(pg, downOut, now.Add(3*time.Second))
	m.flush("stack:local/app1")
	m.flush("stack:local/pg")
	if len(*sent) != 1 || !strings.Contains((*sent)[0].msg.Title, "also affected: app1") {
		t.Fatalf("want one root message, got %q", titles(*sent))
	}
	if c := causeOf(t, st, a1.ID); c != "local/pg" {
		t.Fatalf("app1 cause %q", c)
	}
}

// The root recovers: one message lists dependents that recovered with it; a dependent that
// stays down alerts again on its own with a note.
func TestCascadeRecoveries(t *testing.T) {
	m, st, sent := newCascadeMonitor(t, "app1>pg", "app2>pg")
	pg := saveTestCheck(t, st, "pg-tcp", "pg", 1)
	a1 := saveTestCheck(t, st, "app1-http", "app1", 1)
	a2 := saveTestCheck(t, st, "app2-http", "app2", 1)
	now := time.Now()
	m.handle(pg, downOut, now)
	m.handle(a1, downOut, now.Add(time.Second))
	m.handle(a2, downOut, now.Add(2*time.Second))
	m.flush("stack:local/pg")
	*sent = nil

	m.handle(pg, upOut, now.Add(5*time.Minute))
	m.handle(a1, upOut, now.Add(5*time.Minute+20*time.Second))
	m.mu.Lock()
	_, waiting := m.batches["recover:stack:local/pg"]
	m.mu.Unlock()
	if !waiting {
		t.Fatal("root recovery should wait for its dependents")
	}
	m.flush("recover:stack:local/pg")
	if len(*sent) != 1 {
		t.Fatalf("want one recovery message, got %q", titles(*sent))
	}
	rec := (*sent)[0].msg
	if rec.Status != "up" || !strings.Contains(rec.Title, "Back up: pg-tcp") || !strings.Contains(rec.Title, "also recovered: app1") {
		t.Fatalf("recovery title: %q", rec.Title)
	}
	if !strings.Contains(rec.Text, "Still down after pg recovered: app2") {
		t.Fatalf("recovery text: %q", rec.Text)
	}
	m.flush("stack:local/app2")
	if len(*sent) != 2 {
		t.Fatalf("want the still-down alert, got %q", titles(*sent))
	}
	still := (*sent)[1].msg
	if still.Status != "down" || !strings.Contains(still.Title, "app2-http") || !strings.Contains(still.Title, "still down after pg recovered") {
		t.Fatalf("still-down alert: %q", still.Title)
	}
	// app2 recovers later: a normal recovery, the root is no longer pending
	m.handle(a2, upOut, now.Add(10*time.Minute))
	m.flush("stack:local/app2")
	if len(*sent) != 3 || !strings.HasPrefix((*sent)[2].msg.Title, "Back up: app2-http") {
		t.Fatalf("late recovery: %q", titles(*sent))
	}
}

// A dependent that recovers while the root is still down says so.
func TestCascadeDependentRecoversBeforeRoot(t *testing.T) {
	m, st, sent := newCascadeMonitor(t, "app1>pg")
	pg := saveTestCheck(t, st, "pg-tcp", "pg", 1)
	a1 := saveTestCheck(t, st, "app1-http", "app1", 1)
	now := time.Now()
	m.handle(pg, downOut, now)
	m.handle(a1, downOut, now.Add(time.Second))
	m.flush("stack:local/pg")
	m.handle(a1, upOut, now.Add(time.Minute))
	m.flush("stack:local/pg")
	if len(*sent) != 2 || !strings.Contains((*sent)[1].msg.Title, "(pg is still down)") {
		t.Fatalf("got %q", titles(*sent))
	}
}

// Transitive edges count up to depth 3; the root is the stack that went down first.
func TestCascadeTransitiveDepth(t *testing.T) {
	m, st, sent := newCascadeMonitor(t, "a>root", "b>a", "c>b", "d>c")
	root := saveTestCheck(t, st, "root-chk", "root", 1)
	c := saveTestCheck(t, st, "c-chk", "c", 1)
	d := saveTestCheck(t, st, "d-chk", "d", 1)
	now := time.Now()
	m.handle(root, downOut, now)
	m.handle(c, downOut, now.Add(time.Second)) // c → b → a → root: depth 3
	m.handle(d, downOut, now.Add(2*time.Second))
	if got := causeOf(t, st, c.ID); got != "local/root" {
		t.Fatalf("c at depth 3 should fold into root, got %q", got)
	}
	// d is 4 hops from root, but c (1 hop) is down: d folds into the nearest root in reach
	if got := causeOf(t, st, d.ID); got != "local/c" {
		t.Fatalf("d cause %q", got)
	}
	m.flush("stack:local/root")
	m.flush("stack:local/c")
	if len(*sent) != 2 {
		t.Fatalf("got %q", titles(*sent))
	}
}

// A stack 4 hops away from the only down stack alerts on its own.
func TestCascadeBeyondDepthAlertsAlone(t *testing.T) {
	m, st, sent := newCascadeMonitor(t, "a>root", "b>a", "c>b", "d>c")
	root := saveTestCheck(t, st, "root-chk", "root", 1)
	d := saveTestCheck(t, st, "d-chk", "d", 1)
	now := time.Now()
	m.handle(root, downOut, now)
	m.handle(d, downOut, now.Add(time.Second))
	if got := causeOf(t, st, d.ID); got != "" {
		t.Fatalf("d is 4 hops away, got cause %q", got)
	}
	m.flush("stack:local/root")
	m.flush("stack:local/d")
	if len(*sent) != 2 {
		t.Fatalf("got %q", titles(*sent))
	}
}

// Stacks in a cycle don't group (and nothing loops).
func TestCascadeCycleIsIgnored(t *testing.T) {
	m, st, sent := newCascadeMonitor(t, "a>b", "b>a")
	a := saveTestCheck(t, st, "a-chk", "a", 1)
	b := saveTestCheck(t, st, "b-chk", "b", 1)
	now := time.Now()
	m.handle(a, downOut, now)
	m.handle(b, downOut, now.Add(time.Second))
	m.flush("stack:local/a")
	m.flush("stack:local/b")
	if len(*sent) != 2 {
		t.Fatalf("a cycle must not group: %q", titles(*sent))
	}
	if causeOf(t, st, b.ID) != "" || causeOf(t, st, a.ID) != "" {
		t.Fatal("cycle members got a cause")
	}
}

// Maintenance on the dependent wins: no alert for it, the root alerts alone.
func TestCascadeMaintenanceOnDependentWins(t *testing.T) {
	m, st, sent := newCascadeMonitor(t, "app1>pg")
	ctx := context.Background()
	pg := saveTestCheck(t, st, "pg-tcp", "pg", 1)
	a1 := saveTestCheck(t, st, "app1-http", "app1", 1)
	now := time.Now().Truncate(time.Second)
	saveWindow(t, st, store.MaintenanceWindow{Kind: store.MaintenanceOnce, StartsAt: now.Add(-time.Minute), EndsAt: now.Add(30 * time.Minute),
		Scope: store.MaintenanceScope{Stacks: []store.MaintenanceStack{{HostID: "local", Stack: "app1"}}}})
	m.RefreshMaintenance(ctx, now)
	m.handle(pg, downOut, now)
	m.handle(a1, downOut, now.Add(time.Second))
	m.flush("stack:local/pg")
	m.flush("stack:local/app1")
	if len(*sent) != 1 || strings.Contains((*sent)[0].msg.Title, "app1") {
		t.Fatalf("want the root alone, got %q", titles(*sent))
	}
	// a deploy of the dependent silences it as well
	m2, st2, sent2 := newCascadeMonitor(t, "app1>pg")
	pg2 := saveTestCheck(t, st2, "pg-tcp", "pg", 1)
	a2 := saveTestCheck(t, st2, "app1-http", "app1", 1)
	m2.StackEvent("local", "app1", "deploy", "ann", "")
	m2.handle(pg2, downOut, time.Now())
	m2.handle(a2, downOut, time.Now())
	m2.flush("stack:local/pg")
	if len(*sent2) != 1 || strings.Contains((*sent2)[0].msg.Title, "app1") {
		t.Fatalf("deploy silence: %q", titles(*sent2))
	}
}

// The grouped message goes to the union of the channels of all its alerts; a dependent
// without channels neither adds nor removes any.
func TestCascadeChannelUnion(t *testing.T) {
	m, st, sent := newCascadeMonitor(t, "app1>pg", "app2>pg")
	ctx := context.Background()
	chA, _ := st.SaveChannel(ctx, store.Channel{Name: "ops", Kind: "webhook", Config: `{"url":"https://a"}`, Enabled: true})
	chB, _ := st.SaveChannel(ctx, store.Channel{Name: "team", Kind: "webhook", Config: `{"url":"https://b"}`, Enabled: true})
	mk := func(name, stack string, chans []int64) store.Check {
		id, err := st.SaveCheck(ctx, store.Check{Name: name, Kind: KindHTTP, Target: "https://x", HostID: "local", Stack: stack,
			Interval: 60, Timeout: 5, Retries: 1, Enabled: true, Channels: chans})
		if err != nil {
			t.Fatal(err)
		}
		c, _ := st.CheckByID(ctx, id)
		return c
	}
	pg := mk("pg-tcp", "pg", []int64{chA})
	a1 := mk("app1-http", "app1", nil)
	a2 := mk("app2-http", "app2", []int64{chB})
	now := time.Now()
	m.handle(pg, downOut, now)
	m.handle(a1, downOut, now.Add(time.Second))
	m.handle(a2, downOut, now.Add(2*time.Second))
	m.flush("stack:local/pg")
	got := map[int64]int{}
	for _, s := range *sent {
		got[s.channel]++
		if !strings.Contains(s.msg.Title, "app1, app2") {
			t.Fatalf("title lacks dependents: %q", s.msg.Title)
		}
	}
	if len(*sent) != 2 || got[chA] != 1 || got[chB] != 1 {
		t.Fatalf("want one message on each channel, got %v", got)
	}
}

// After a hub restart the cascade link of open incidents is kept.
func TestCascadeRootSurvivesRestart(t *testing.T) {
	m, st, _ := newCascadeMonitor(t, "app1>pg")
	pg := saveTestCheck(t, st, "pg-tcp", "pg", 1)
	a1 := saveTestCheck(t, st, "app1-http", "app1", 1)
	now := time.Now()
	m.handle(pg, downOut, now)
	m.handle(a1, downOut, now.Add(time.Second))

	m2 := New(st, nil, m.log, "")
	m2.send = func(context.Context, store.Channel, notify.Message) error { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	m2.mu.Lock()
	root := m2.states[a1.ID].root
	m2.mu.Unlock()
	if root != "local/pg" {
		t.Fatalf("root after restart: %q", root)
	}
}

func TestStackCheckStatus(t *testing.T) {
	m, st, _ := newCascadeMonitor(t)
	pg := saveTestCheck(t, st, "pg-tcp", "pg", 1)
	a1 := saveTestCheck(t, st, "app1-http", "app1", 1)
	m.handle(pg, downOut, time.Now())
	m.handle(a1, upOut, time.Now())
	got := m.StackCheckStatus()
	if got["local/pg"] != StatusDown || got["local/app1"] != StatusUp {
		t.Fatalf("got %v", got)
	}
}
