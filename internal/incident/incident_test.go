package incident

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/store"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeHost implements the parts of host.Host the package uses; anything else panics.
type fakeHost struct {
	host.Host
	mu         sync.Mutex
	versions   []engine.Version
	containers []engine.Container
	restored   string
	actions    []string
	rolledBack map[string]engine.RollbackTarget
	opError    string
}

func (f *fakeHost) History(context.Context, string) ([]engine.Version, error) { return f.versions, nil }
func (f *fakeHost) GetStack(_ context.Context, name string) (engine.Stack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return engine.Stack{Name: name, Containers: f.containers}, nil
}
func (f *fakeHost) Restore(_ context.Context, _, hash, _ string) (*engine.Version, error) {
	f.mu.Lock()
	f.restored = hash
	f.mu.Unlock()
	return &engine.Version{Hash: hash}, nil
}
func (f *fakeHost) RunAction(_ context.Context, _, action, _ string) (engine.OpInfo, error) {
	f.mu.Lock()
	f.actions = append(f.actions, action)
	f.mu.Unlock()
	return engine.OpInfo{ID: "op1"}, nil
}
func (f *fakeHost) RollbackImages(_ context.Context, _ string, t map[string]engine.RollbackTarget, _ string) (engine.OpInfo, error) {
	f.mu.Lock()
	f.rolledBack = t
	f.mu.Unlock()
	return engine.OpInfo{ID: "op1"}, nil
}
func (f *fakeHost) FollowOp(_ context.Context, _ string, emit func(host.OpEvent) error) error {
	return emit(host.OpEvent{Done: true, Error: f.opError})
}

type fakeHosts struct{ h *fakeHost }

func (f fakeHosts) Get(context.Context, string) (host.Host, host.Entry, error) {
	return f.h, host.Entry{ID: "local"}, nil
}

// fakeMonitor closes the incident on a kick when up is set, like the real monitor does
// when the probe passes.
type fakeMonitor struct {
	st     *store.Store
	inc    int64
	up     bool
	mu     sync.Mutex
	kicks  int
	events []string
	failed string
}

func (m *fakeMonitor) KickCheck(int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.kicks++
	if m.up {
		_ = m.st.CloseIncident(context.Background(), m.inc, time.Now())
	}
}
func (m *fakeMonitor) StackEvent(_, _, kind, _, _ string) {
	m.mu.Lock()
	m.events = append(m.events, kind)
	m.mu.Unlock()
}
func (m *fakeMonitor) RollbackFailed(_ store.Check, reason string) {
	m.mu.Lock()
	m.failed = reason
	m.mu.Unlock()
}

func TestPickComposeTarget(t *testing.T) {
	start := time.Date(2026, 11, 26, 14, 0, 0, 0, time.UTC)
	v := func(h string, at time.Time) engine.Version { return engine.Version{Hash: h, Short: h, Time: at} }
	running, target, err := pickComposeTarget([]engine.Version{v("bad", start.Add(-5*time.Minute)), v("good", start.Add(-48*time.Hour))}, start)
	if err != nil || running.Hash != "bad" || target.Hash != "good" {
		t.Fatalf("want the version before the running one, got %v %v %v", running.Hash, target.Hash, err)
	}
	if _, _, err := pickComposeTarget([]engine.Version{v("later", start.Add(time.Minute)), v("bad", start.Add(-time.Minute))}, start); !errors.Is(err, ErrChangedSince) {
		t.Fatalf("a change after the outage began must refuse, got %v", err)
	}
	if _, _, err := pickComposeTarget([]engine.Version{v("only", start.Add(-time.Hour))}, start); !errors.Is(err, ErrNoRollbackTarget) {
		t.Fatalf("one version: %v", err)
	}
}

func TestPickImageUpdate(t *testing.T) {
	start := time.Date(2026, 11, 26, 14, 0, 0, 0, time.UTC)
	svc := []store.ImageUpdateService{{Service: "web", Ref: "nginx:1", OldID: "sha256:old", NewID: "sha256:new"}}
	upd := func(id int64, at time.Time, result string, s []store.ImageUpdateService) store.ImageUpdate {
		return store.ImageUpdate{ID: id, StartedAt: at, FinishedAt: at, Result: result, Services: s}
	}
	noChange := []store.ImageUpdateService{{Service: "web", OldID: "sha256:a", NewID: "sha256:a"}}
	u, err := pickImageUpdate([]store.ImageUpdate{
		upd(3, start.Add(-time.Minute), store.UpdateOK, noChange), // nothing changed: skipped
		upd(2, start.Add(-2*time.Minute), "pull-failed", svc),     // failed: skipped
		upd(1, start.Add(-10*time.Minute), store.UpdateOK, svc),
	}, start)
	if err != nil || u.ID != 1 {
		t.Fatalf("want update 1, got %d %v", u.ID, err)
	}
	if _, err := pickImageUpdate([]store.ImageUpdate{upd(4, start.Add(time.Minute), store.UpdateOK, svc), upd(1, start.Add(-time.Minute), store.UpdateOK, svc)}, start); !errors.Is(err, ErrChangedSince) {
		t.Fatalf("an update after the outage began must refuse, got %v", err)
	}
	if _, err := pickImageUpdate(nil, start); !errors.Is(err, ErrNoRollbackTarget) {
		t.Fatalf("no updates: %v", err)
	}
}

func setup(t *testing.T) (*store.Store, store.Check, int64, time.Time) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	c := store.Check{Name: "web", Kind: "http", Target: "http://web", HostID: "local", Stack: "web", Interval: 60, Timeout: 5, Retries: 1, Enabled: true}
	id, err := st.SaveCheck(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	c.ID = id
	start := time.Now().Add(-time.Minute).Truncate(time.Second)
	inc, err := st.OpenIncident(ctx, c.ID, start, "HTTP 502", "{}")
	if err != nil {
		t.Fatal(err)
	}
	return st, c, inc, start
}

func TestTimelineCollectsSourcesInWindow(t *testing.T) {
	st, c, _, start := setup(t)
	ctx := context.Background()
	code := 137
	_ = st.AddContainerEvent(ctx, store.ContainerEvent{HostID: "local", Stack: "web", Service: "app", ContainerID: "c1", Type: store.ContainerDie, ExitCode: &code, Time: start.Add(-3 * time.Minute)})
	_ = st.AddContainerEvent(ctx, store.ContainerEvent{HostID: "local", Stack: "web", Service: "app", ContainerID: "c1", Type: store.ContainerDie, ExitCode: &code, Time: start.Add(-2 * time.Minute)})
	_ = st.AddContainerEvent(ctx, store.ContainerEvent{HostID: "local", Stack: "web", Service: "app", ContainerID: "c0", Type: store.ContainerRestart, Time: start.Add(-time.Hour)}) // outside
	if _, err := st.AddImageUpdate(ctx, store.ImageUpdate{HostID: "local", Stack: "web", StartedAt: start.Add(-6 * time.Minute), FinishedAt: start.Add(-5 * time.Minute), Actor: "auto", Result: store.UpdateOK,
		Services: []store.ImageUpdateService{{Service: "app", Ref: "app:latest", OldDigest: "sha256:aaaaaaaaaaaaaaaa", NewDigest: "sha256:bbbbbbbbbbbbbbbb"}}}); err != nil {
		t.Fatal(err)
	}
	h := &fakeHost{versions: []engine.Version{
		{Hash: "h2", Short: "h2", Message: "Raise workers", Author: "admin", Time: start.Add(-8 * time.Minute), Files: []string{"compose.yaml"}},
		{Hash: "h1", Short: "h1", Message: "Old", Time: start.Add(-2 * time.Hour), Files: []string{"compose.yaml"}},
	}}
	b := NewTimelineBuilder(st, fakeHosts{h}, quiet)
	tl := b.Build(ctx, c, start, []engine.LogLine{{Container: "app", Message: "panic: boom"}})

	var kinds []string
	for _, s := range tl.Sources {
		kinds = append(kinds, s.Type)
	}
	got := strings.Join(kinds, ",")
	if got != "compose_change,image_update,docker_event,log_match" {
		t.Fatalf("sources in time order: %s (%+v)", got, tl.Sources)
	}
	if d := tl.Sources[2]; !strings.Contains(d.Title, "exited with code 137 (2×)") || !strings.Contains(d.Detail, "out of memory") {
		t.Fatalf("crash loop merged with an OOM hint: %+v", d)
	}
	changes := tl.Changes(5)
	if len(changes) != 3 || !strings.Contains(changes[0], "Changed compose.yaml (admin)") || !strings.Contains(changes[1], "aaaaaaaaaaaa → bbbbbbbbbbbb") {
		t.Fatalf("changes for the alert: %q", changes)
	}
	if err := SaveTimeline(ctx, st, 1, tl); err != nil {
		t.Fatal(err)
	}
	back, err := LoadTimeline(ctx, st, 1)
	if err != nil || len(back.Sources) != len(tl.Sources) {
		t.Fatalf("round trip: %v %d", err, len(back.Sources))
	}
}

func newRV(st *store.Store, h *fakeHost, mon *fakeMonitor) *RollbackVerify {
	rv := NewRollbackVerify(st, fakeHosts{h}, mon, quiet)
	rv.VerifyTimeout, rv.PollInterval = 300*time.Millisecond, 10*time.Millisecond
	rv.Start(context.Background())
	return rv
}

// eventually waits up to 2 s for cond (work that finishes right after the saved state).
func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200 && !cond(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
}

func waitPhase(t *testing.T, st *store.Store, inc int64) *RollbackVerifyState {
	t.Helper()
	for i := 0; i < 200; i++ {
		s, err := LoadState(context.Background(), st, inc)
		if err == nil && !s.Active && s.Phase != "" {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("rollback & verify didn't finish")
	return nil
}

func TestRollbackComposeVerifiesThroughTheMonitor(t *testing.T) {
	st, _, inc, start := setup(t)
	h := &fakeHost{
		versions:   []engine.Version{{Hash: "bad", Short: "bad", Time: start.Add(-5 * time.Minute)}, {Hash: "good", Short: "good", Time: start.Add(-time.Hour)}},
		containers: []engine.Container{{Service: "app", State: "running", Health: "healthy"}},
	}
	mon := &fakeMonitor{st: st, inc: inc, up: true}
	rv := newRV(st, h, mon)
	opts, err := rv.GetRollbackOptions(context.Background(), inc)
	if err != nil || len(opts) != 1 || opts[0].Type != RollbackTypeCompose || opts[0].TargetRef != "good" {
		t.Fatalf("options: %+v %v", opts, err)
	}
	if _, err := rv.StartRollbackVerify(context.Background(), inc, RollbackTypeCompose, "admin"); err != nil {
		t.Fatal(err)
	}
	s := waitPhase(t, st, inc)
	if s.Phase != PhaseDone || !s.IncidentClosed || s.TriggeredBy != "admin" || s.RollbackType != "compose" {
		t.Fatalf("state: %+v", s)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.restored != "good" || len(h.actions) != 1 || h.actions[0] != "up" {
		t.Fatalf("restored %q, actions %v", h.restored, h.actions)
	}
	if len(mon.events) != 1 || mon.events[0] != "rollback" || mon.failed != "" {
		t.Fatalf("monitor: %+v", mon)
	}
	// The result row is written right after the state: wait for it.
	var audit []store.AuditEntry
	eventually(t, func() bool {
		audit, _ = st.AuditEntries(context.Background(), store.AuditFilter{})
		return len(audit) > 0
	})
	if len(audit) == 0 || audit[0].Action != "incident.rollback_verify.result" || audit[0].Outcome != "ok" {
		t.Fatalf("audit: %+v", audit)
	}
}

func TestRollbackFailsAndEscalatesWhenCheckStaysDown(t *testing.T) {
	st, _, inc, start := setup(t)
	if _, err := st.AddImageUpdate(context.Background(), store.ImageUpdate{HostID: "local", Stack: "web", StartedAt: start.Add(-5 * time.Minute), FinishedAt: start.Add(-4 * time.Minute), Result: store.UpdateOK,
		Services: []store.ImageUpdateService{{Service: "app", Ref: "app:1", OldID: "sha256:old", NewID: "sha256:new"}}}); err != nil {
		t.Fatal(err)
	}
	h := &fakeHost{containers: []engine.Container{{Service: "app", State: "running"}}}
	mon := &fakeMonitor{st: st, inc: inc, up: false}
	rv := newRV(st, h, mon)
	if _, err := rv.StartRollbackVerify(context.Background(), inc, RollbackTypeImage, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := rv.StartRollbackVerify(context.Background(), inc, RollbackTypeImage, "admin"); !errors.Is(err, ErrRollbackRunning) {
		t.Fatalf("second start while running: %v", err)
	}
	s := waitPhase(t, st, inc)
	if s.Phase != PhaseFailed || !strings.Contains(s.Error, "the check still fails") || s.RollbackType != "image" {
		t.Fatalf("state: %+v", s)
	}
	// The state is saved before the escalation is sent: wait for it.
	eventually(t, func() bool { mon.mu.Lock(); defer mon.mu.Unlock(); return mon.failed != "" })
	mon.mu.Lock()
	defer mon.mu.Unlock()
	if mon.failed == "" || mon.kicks == 0 {
		t.Fatalf("must escalate and keep probing: %+v", mon)
	}
	if h.rolledBack["app"].OldID != "sha256:old" {
		t.Fatalf("rollback targets: %+v", h.rolledBack)
	}
}

func TestRollbackOperationErrorFails(t *testing.T) {
	st, _, inc, start := setup(t)
	h := &fakeHost{versions: []engine.Version{{Hash: "bad", Time: start.Add(-time.Minute)}, {Hash: "good", Time: start.Add(-time.Hour)}}, opError: "pull access denied"}
	mon := &fakeMonitor{st: st, inc: inc, up: true}
	rv := newRV(st, h, mon)
	if _, err := rv.StartRollbackVerify(context.Background(), inc, RollbackTypeCompose, "admin"); err != nil {
		t.Fatal(err)
	}
	s := waitPhase(t, st, inc)
	if s.Phase != PhaseFailed || !strings.Contains(s.Error, "pull access denied") {
		t.Fatalf("state: %+v", s)
	}
}

func TestRestartMarksRunningVerificationFailed(t *testing.T) {
	st, _, inc, _ := setup(t)
	if err := saveState(context.Background(), st, inc, &RollbackVerifyState{Active: true, Phase: PhaseVerifying, TargetLabel: "x"}); err != nil {
		t.Fatal(err)
	}
	NewRollbackVerify(st, nil, nil, quiet).Start(context.Background())
	s, _ := LoadState(context.Background(), st, inc)
	if s.Active || s.Phase != PhaseFailed || s.TargetLabel != "x" {
		t.Fatalf("state after restart: %+v", s)
	}
}

func TestClosedIncidentAndCheckWithoutStack(t *testing.T) {
	st, _, inc, _ := setup(t)
	rv := newRV(st, &fakeHost{}, &fakeMonitor{st: st})
	_ = st.CloseIncident(context.Background(), inc, time.Now())
	if _, err := rv.StartRollbackVerify(context.Background(), inc, RollbackTypeCompose, "admin"); !errors.Is(err, ErrIncidentClosed) {
		t.Fatalf("closed incident: %v", err)
	}
	ctx := context.Background()
	id, _ := st.SaveCheck(ctx, store.Check{Name: "ext", Kind: "http", Target: "http://x", Interval: 60, Timeout: 5, Retries: 1, Enabled: true})
	inc2, _ := st.OpenIncident(ctx, id, time.Now(), "down", "{}")
	if _, err := rv.StartRollbackVerify(ctx, inc2, RollbackTypeCompose, "admin"); !errors.Is(err, ErrNoStack) {
		t.Fatalf("check without stack: %v", err)
	}
	if opts, err := rv.GetRollbackOptions(ctx, inc2); err != nil || len(opts) != 0 {
		t.Fatalf("no options without a stack: %v %v", opts, err)
	}
}
