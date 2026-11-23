package updater

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/notify"
	"moorage/internal/store"
)

type fakeUpdaterHost struct {
	mu           sync.Mutex
	stacks       []engine.StackSummary
	stackDetails map[string]engine.Stack
	checkResults map[string][]engine.ServiceImage
	checkErr     error
	runningImgs  map[string]map[string]engine.RunningImage
	afterImgs    map[string]map[string]engine.RunningImage
	opLines      []string
	opErr        string
	actionsRun   []string
	rollbacksRun []map[string]engine.RollbackTarget
	checkCalls   int
}

func newFakeUpdaterHost() *fakeUpdaterHost {
	return &fakeUpdaterHost{
		stackDetails: map[string]engine.Stack{},
		checkResults: map[string][]engine.ServiceImage{},
		runningImgs:  map[string]map[string]engine.RunningImage{},
		afterImgs:    map[string]map[string]engine.RunningImage{},
	}
}

func (f *fakeUpdaterHost) Info(context.Context) engine.HostInfo {
	return engine.HostInfo{Name: "local", Reachable: true}
}

func (f *fakeUpdaterHost) ListStacks(context.Context) ([]engine.StackSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]engine.StackSummary, len(f.stacks))
	copy(out, f.stacks)
	return out, nil
}

func (f *fakeUpdaterHost) GetStack(_ context.Context, name string) (engine.Stack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st, ok := f.stackDetails[name]; ok {
		return st, nil
	}
	for _, s := range f.stacks {
		if s.Name == name {
			return engine.Stack{StackSummary: s}, nil
		}
	}
	return engine.Stack{}, engine.ErrNotFound
}

func (f *fakeUpdaterHost) Stats(context.Context, string) ([]engine.Stat, error) { return nil, nil }
func (f *fakeUpdaterHost) Validate(context.Context, string, string, string) error {
	return nil
}
func (f *fakeUpdaterHost) Save(context.Context, string, string, string, string, bool) error {
	return nil
}
func (f *fakeUpdaterHost) Delete(context.Context, string, string) error { return nil }

func (f *fakeUpdaterHost) RunAction(_ context.Context, name, action, _ string) (engine.OpInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actionsRun = append(f.actionsRun, name+":"+action)
	if after, ok := f.afterImgs[name]; ok {
		f.runningImgs[name] = after
	}
	return engine.OpInfo{ID: "op-" + name, Stack: name, Action: action}, nil
}

func (f *fakeUpdaterHost) FollowOp(_ context.Context, _ string, emit func(host.OpEvent) error) error {
	f.mu.Lock()
	lines := append([]string(nil), f.opLines...)
	errStr := f.opErr
	f.mu.Unlock()
	return emit(host.OpEvent{Lines: lines, Done: true, Error: errStr})
}

func (f *fakeUpdaterHost) StreamLogs(context.Context, string, string, int, func(engine.LogLine)) error {
	return nil
}
func (f *fakeUpdaterHost) WatchLogs(context.Context, string, string, engine.LogFilter, func(engine.LogLine)) error {
	return nil
}
func (f *fakeUpdaterHost) TailLogs(context.Context, string, int) ([]engine.LogLine, error) {
	return nil, nil
}
func (f *fakeUpdaterHost) History(context.Context, string) ([]engine.Version, error) {
	return nil, nil
}
func (f *fakeUpdaterHost) Version(context.Context, string, string) (engine.VersionDetail, error) {
	return engine.VersionDetail{}, nil
}
func (f *fakeUpdaterHost) Restore(context.Context, string, string, string) (*engine.Version, error) {
	return nil, nil
}

func (f *fakeUpdaterHost) CheckImages(_ context.Context, stack string) ([]engine.ServiceImage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkCalls++
	if f.checkErr != nil {
		return nil, f.checkErr
	}
	out := make([]engine.ServiceImage, len(f.checkResults[stack]))
	copy(out, f.checkResults[stack])
	return out, nil
}

func (f *fakeUpdaterHost) StackImages(_ context.Context, stack string) (map[string]engine.RunningImage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]engine.RunningImage{}
	for k, v := range f.runningImgs[stack] {
		out[k] = v
	}
	return out, nil
}

func (f *fakeUpdaterHost) RollbackImages(_ context.Context, stack string, targets map[string]engine.RollbackTarget, _ string) (engine.OpInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := map[string]engine.RollbackTarget{}
	for k, v := range targets {
		cp[k] = v
	}
	f.rollbacksRun = append(f.rollbacksRun, cp)
	return engine.OpInfo{ID: "rb-" + stack, Stack: stack, Action: "rollback"}, nil
}

func (f *fakeUpdaterHost) ChangeImage(context.Context, string, string, string, string, string) (*engine.Version, error) {
	return &engine.Version{Short: "1234567"}, nil
}
func (f *fakeUpdaterHost) Terminal(context.Context, string, string, string, int, int) (engine.TerminalSession, engine.Container, error) {
	return nil, engine.Container{}, host.ErrTerminalDisabled
}

type recordedEvent struct {
	hostID, stack, kind, actor, detail string
}

type eventRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
}

func (r *eventRecorder) StackEvent(hostID, stack, kind, actor, detail string) {
	r.mu.Lock()
	r.events = append(r.events, recordedEvent{hostID, stack, kind, actor, detail})
	r.mu.Unlock()
}

// Part A #2: Policy and apply-window decision (including midnight wrap and DST in Europe/Prague).
func TestShouldApplyAndWindowLogic(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Fatalf("LoadLocation Europe/Prague: %v", err)
	}

	inside := time.Date(2026, 11, 22, 3, 30, 0, 0, loc)
	outside := time.Date(2026, 11, 22, 14, 0, 0, 0, loc)

	base := ApplyInput{
		Policy:       store.PolicyAuto,
		Status:       engine.ImageUpdateAvailable,
		WindowStart:  "03:00",
		WindowEnd:    "05:00",
		Now:          inside,
		Managed:      true,
		RemoteDigest: "sha256:new111",
	}

	if !ShouldApply(base) {
		t.Fatalf("expected ShouldApply(base) = true")
	}

	// Policy off or notify never auto-applies.
	for _, pol := range []string{store.PolicyOff, store.PolicyNotify} {
		in := base
		in.Policy = pol
		if ShouldApply(in) {
			t.Errorf("policy %q must not auto-apply", pol)
		}
	}

	// Outside window must not auto-apply.
	inOut := base
	inOut.Now = outside
	if ShouldApply(inOut) {
		t.Errorf("outside window must not auto-apply")
	}

	// Overnight window crossing midnight (23:00–02:00).
	overnightLate := base
	overnightLate.WindowStart, overnightLate.WindowEnd = "23:00", "02:00"
	overnightLate.Now = time.Date(2026, 11, 22, 23, 30, 0, 0, loc)
	if !ShouldApply(overnightLate) {
		t.Errorf("23:30 should be inside 23:00–02:00 window")
	}

	overnightEarly := overnightLate
	overnightEarly.Now = time.Date(2026, 11, 23, 1, 15, 0, 0, loc)
	if !ShouldApply(overnightEarly) {
		t.Errorf("01:15 should be inside 23:00–02:00 window")
	}
	// If already updated at 23:45 on Nov 22, must not re-apply at 01:15 on Nov 23 (same overnight window).
	overnightEarly.LastAutoAt = time.Date(2026, 11, 22, 23, 45, 0, 0, loc)
	if ShouldApply(overnightEarly) {
		t.Errorf("must not auto-apply twice in the same overnight window")
	}

	// DST transition in Europe/Prague (Spring forward: 2026-03-29, Fall back: 2026-10-25).
	dstFall := base
	dstFall.Now = time.Date(2026, 10, 25, 3, 15, 0, 0, loc)
	if !ShouldApply(dstFall) {
		t.Errorf("expected inside window on DST day")
	}

	// Busy, stopped, read-only, or unmanaged stack must not auto-apply.
	for name, mod := range map[string]func(*ApplyInput){
		"busy":      func(a *ApplyInput) { a.Busy = true },
		"stopped":   func(a *ApplyInput) { a.Stopped = true },
		"readOnly":  func(a *ApplyInput) { a.ReadOnly = true },
		"unmanaged": func(a *ApplyInput) { a.Managed = false },
		"skipped":   func(a *ApplyInput) { a.SkippedDigest = "sha256:new111" },
	} {
		in := base
		mod(&in)
		if ShouldApply(in) {
			t.Errorf("case %s must not auto-apply", name)
		}
	}
}

// Part A #3, #4, #5: Auto-apply, deploy silence, failure distinction, rollback policy switch + skipped digest,
// and notification deduplication across hub restarts.
func TestUpdaterAutoApplyRollbackAndNotifications(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	chID, err := st.SaveChannel(ctx, store.Channel{
		Name: "hooks", Kind: "webhook", Config: `{"url":"https://example.com/hook"}`, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveUpdateSettings(ctx, store.UpdateSettings{
		Enabled: true, Interval: 21600, WindowStart: "03:00", WindowEnd: "05:00", Channels: []int64{chID},
	}); err != nil {
		t.Fatal(err)
	}

	fh := newFakeUpdaterHost()
	fh.stacks = []engine.StackSummary{
		{Name: "web", Managed: true, Status: "up", Running: 1, Total: 1},
		{Name: "api", Managed: true, Status: "up", Running: 1, Total: 1},
	}
	fh.checkResults["web"] = []engine.ServiceImage{
		{Service: "nginx", Ref: "nginx:1.27", LocalID: "sha256:old1", LocalDigest: "sha256:1111111", RemoteDigest: "sha256:2222222", Status: engine.ImageUpdateAvailable, NewerTag: "1.28"},
	}
	fh.checkResults["api"] = []engine.ServiceImage{
		{Service: "app", Ref: "myorg/api:1.0", LocalID: "sha256:api1", LocalDigest: "sha256:aaaaaaa", RemoteDigest: "sha256:bbbbbbb", Status: engine.ImageUpdateAvailable},
	}

	reg := host.NewRegistry(fh, "local", false, st)
	evRec := &eventRecorder{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	upd := New(st, reg, evRec, log, "https://moorage.example.com")

	var sent []notify.Message
	upd.SetSender(func(_ context.Context, _ store.Channel, m notify.Message) error {
		sent = append(sent, m)
		return nil
	})

	// 1. Outside apply window (14:00): both stacks have updates; one batched discovery notice is sent.
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)
	upd.SetClock(func() time.Time { return now })
	if err := st.SaveStackUpdatePolicy(ctx, store.StackUpdatePolicy{HostID: "local", Stack: "web", Policy: store.PolicyAuto}); err != nil {
		t.Fatal(err)
	}

	upd.RunScheduled(ctx)
	if len(sent) != 1 {
		t.Fatalf("expected 1 batched discovery notification, got %d", len(sent))
	}
	if sent[0].Title != "3 image updates available" ||
		!strings.Contains(sent[0].Text, "web · nginx: nginx:1.27 has a new build") ||
		!strings.Contains(sent[0].Text, "web · nginx: 1.28 available") ||
		!strings.Contains(sent[0].Text, "api · app: myorg/api:1.0 has a new build") {
		t.Fatalf("batched message missing expected updates:\n%s", sent[0].Text)
	}

	// 2. Second run with same digests and tags -> 0 messages.
	sent = nil
	upd.RunScheduled(ctx)
	if len(sent) != 0 {
		t.Fatalf("expected 0 notifications on repeat run with same digests/tags, got %d", len(sent))
	}

	// 3. Simulate hub restart (new Updater instance on same SQLite DB): still 0 messages!
	upd2 := New(st, reg, evRec, log, "https://moorage.example.com")
	upd2.SetClock(func() time.Time { return now })
	upd2.SetSender(func(_ context.Context, _ store.Channel, m notify.Message) error {
		sent = append(sent, m)
		return nil
	})
	upd2.RunScheduled(ctx)
	if len(sent) != 0 {
		t.Fatalf("expected 0 notifications after hub restart, got %d", len(sent))
	}

	// 4. Inside apply window (03:30): "web" (policy=auto) auto-updates, records StackEvent + image_updates row, and sends applied notice.
	now = time.Date(2026, 11, 23, 3, 30, 0, 0, time.UTC)
	fh.runningImgs["web"] = map[string]engine.RunningImage{
		"nginx": {Service: "nginx", Ref: "nginx:1.27", ImageID: "sha256:old1", Digest: "sha256:1111111"},
	}
	fh.afterImgs["web"] = map[string]engine.RunningImage{
		"nginx": {Service: "nginx", Ref: "nginx:1.27", ImageID: "sha256:new2", Digest: "sha256:2222222"},
	}
	fh.opLines = []string{"Pulling nginx", "Recreated web-nginx-1", "✓ update finished"}

	upd2.RunScheduled(ctx)

	if len(evRec.events) == 0 || evRec.events[0].kind != "update" || evRec.events[0].actor != ActorAuto {
		t.Fatalf("expected StackEvent(update, auto-update), got %+v", evRec.events)
	}
	records, err := st.ImageUpdates(ctx, "local", "web", 10)
	if err != nil || len(records) != 1 {
		t.Fatalf("expected 1 image_updates record, got %d (%v)", len(records), err)
	}
	if records[0].Result != store.UpdateOK || records[0].Actor != ActorAuto {
		t.Fatalf("unexpected update record: %+v", records[0])
	}
	if len(sent) != 1 || sent[0].Title != "Updated web: nginx nginx:1.27 (sha256:1111111 → 2222222)" {
		t.Fatalf("expected 'Updated web' notice, got %+v", sent)
	}

	// 5. Roll back the update: switches policy to notify and records 2222222 as skipped_digest.
	opInfo, err := upd2.Rollback(ctx, "local", "web", records[0].ID, "admin")
	if err != nil || opInfo.Action != "rollback" {
		t.Fatalf("Rollback: %v, op=%+v", err, opInfo)
	}
	if len(fh.rollbacksRun) != 1 || fh.rollbacksRun[0]["nginx"].OldID != "sha256:old1" {
		t.Fatalf("unexpected rollback targets: %+v", fh.rollbacksRun)
	}
	upd2.Wait() // the record turns "rolled-back" once the rollback operation finished
	polAfter, _ := st.StackUpdatePolicy(ctx, "local", "web")
	if polAfter.Policy != store.PolicyNotify {
		t.Fatalf("policy after rollback = %q, want notify", polAfter.Policy)
	}
	if polAfter.SkippedDigests["nginx"] != "sha256:2222222" {
		t.Fatalf("skipped_digests after rollback = %+v, want sha256:2222222", polAfter.SkippedDigests)
	}
	recAfter, _ := st.ImageUpdateByID(ctx, records[0].ID)
	if recAfter.Result != store.UpdateRolledBack {
		t.Fatalf("update record result = %q, want rolled-back", recAfter.Result)
	}

	// 6. Test pull-failed vs up-failed distinction during auto-update.
	// Set "api" to auto, simulate pull failure (images unchanged, no recreate) -> not recorded in image_updates, notified once per digest.
	_ = st.SaveStackUpdatePolicy(ctx, store.StackUpdatePolicy{HostID: "local", Stack: "api", Policy: store.PolicyAuto})
	fh.runningImgs["api"] = map[string]engine.RunningImage{
		"app": {Service: "app", Ref: "myorg/api:1.0", ImageID: "sha256:api1", Digest: "sha256:aaaaaaa"},
	}
	delete(fh.afterImgs, "api") // unchanged
	fh.opLines = []string{"Pulling app", "error pulling image: connection reset"}
	fh.opErr = "pull failed"
	sent = nil

	upd2.RunScheduled(ctx)
	apiRecs, _ := st.ImageUpdates(ctx, "local", "api", 10)
	if len(apiRecs) != 0 {
		t.Fatalf("pull failure during auto-update must not record an image_updates row, got %+v", apiRecs)
	}
	if len(sent) != 1 || sent[0].Title != "Update of api failed: pull failed" {
		t.Fatalf("expected pull failure notice, got %+v", sent)
	}

	// Now simulate up-failed (containers touched) in the next day's window -> recorded as up-failed with log tail!
	now = time.Date(2026, 11, 24, 3, 30, 0, 0, time.UTC)
	fh.opLines = []string{"Pulling app", "Recreated api-app-1", "container exited 1"}
	fh.opErr = "container api-app-1 exited"
	sent = nil
	upd2.RunScheduled(ctx)
	apiRecs, _ = st.ImageUpdates(ctx, "local", "api", 10)
	if len(apiRecs) != 1 || apiRecs[0].Result != store.UpdateUpFailed || !strings.Contains(apiRecs[0].LogTail, "container exited 1") {
		t.Fatalf("expected up-failed record with log tail, got %+v", apiRecs)
	}
	// An image that failed to start is not retried every window: the digest is skipped.
	polAPI, _ := st.StackUpdatePolicy(ctx, "local", "api")
	if polAPI.SkippedDigests["app"] != "sha256:bbbbbbb" {
		t.Fatalf("up-failed digest not skipped: %+v", polAPI.SkippedDigests)
	}
	now = time.Date(2026, 11, 25, 3, 30, 0, 0, time.UTC)
	runsBefore := len(fh.actionsRun)
	upd2.RunScheduled(ctx)
	if len(fh.actionsRun) != runsBefore {
		t.Fatalf("failed digest auto-applied again: %v", fh.actionsRun[runsBefore:])
	}

	// R1: every auto-update the scheduler ran is in the audit log with actor "system";
	// the user's rollback goes through the API middleware, not here.
	audit, err := st.AuditEntries(ctx, store.AuditFilter{Actor: store.AuditActorSystem})
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ stack, outcome string }
	var got []row
	for _, e := range audit {
		if e.Action != AuditAutoUpdate || e.HostID != "local" {
			t.Fatalf("unexpected system row: %+v", e)
		}
		got = append(got, row{e.Stack, e.Outcome})
	}
	// newest first: api up-failed, api pull-failed, web ok
	want := []row{{"api", store.AuditFailed}, {"api", store.AuditFailed}, {"web", store.AuditOK}}
	if len(got) != len(want) {
		t.Fatalf("system audit rows = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("system audit rows = %+v, want %+v", got, want)
		}
	}
	if audit[2].Target != "update #"+strconv.FormatInt(records[0].ID, 10) {
		t.Fatalf("auto-update target = %q", audit[2].Target)
	}
}

// A rolled-back digest of one service blocks auto-updates of the whole stack: "update"
// pulls every service, so updating the other service would bring the bad image back.
func TestSkippedDigestBlocksWholeStack(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.SaveUpdateSettings(ctx, store.UpdateSettings{Enabled: true, Interval: 21600, WindowStart: "03:00", WindowEnd: "05:00"})
	fh := newFakeUpdaterHost()
	fh.stacks = []engine.StackSummary{{Name: "app", Managed: true, Status: "up", Running: 2, Total: 2}}
	fh.checkResults["app"] = []engine.ServiceImage{
		{Service: "db", Ref: "postgres:16", RemoteDigest: "sha256:bad", Status: engine.ImageUpdateAvailable},
		{Service: "web", Ref: "nginx:1.27", RemoteDigest: "sha256:new", Status: engine.ImageUpdateAvailable},
	}
	_ = st.SaveStackUpdatePolicy(ctx, store.StackUpdatePolicy{HostID: "local", Stack: "app", Policy: store.PolicyAuto,
		SkippedDigests: map[string]string{"db": "sha256:bad"}})
	upd := New(st, host.NewRegistry(fh, "local", false, st), &eventRecorder{}, slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	upd.SetClock(func() time.Time { return time.Date(2026, 11, 23, 3, 30, 0, 0, time.UTC) })
	upd.RunScheduled(ctx)
	if len(fh.actionsRun) != 0 {
		t.Fatalf("stack with a skipped digest was auto-updated: %v", fh.actionsRun)
	}
	// A newer db digest than the skipped one unblocks it.
	fh.checkResults["app"][0].RemoteDigest = "sha256:fixed"
	upd.RunScheduled(ctx)
	upd.Wait()
	if len(fh.actionsRun) != 1 {
		t.Fatalf("newer digest not applied: %v", fh.actionsRun)
	}
}

// Part A #7 & #8: Read-only agent refuses rollback/update; older agent (404) marks unsupported without tight retry.
func TestReadOnlyAndUnsupportedAgent(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fh := newFakeUpdaterHost()
	fh.stacks = []engine.StackSummary{{Name: "web", Managed: true, Status: "up", Running: 1, Total: 1}}
	fh.checkResults["web"] = []engine.ServiceImage{{Service: "web", Ref: "nginx:1.27", Status: engine.ImageUpToDate}}

	// 1. Read-only agent via AgentHandler + Remote over in-memory pipe.
	roHandler := host.AgentHandler(fh, true, "test")
	roRemote := newPipeRemote(t, roHandler)

	// CheckImages and StackImages are allowed on a read-only agent.
	if _, err := roRemote.CheckImages(ctx, "web"); err != nil {
		t.Fatalf("CheckImages on read-only agent should succeed, got %v", err)
	}
	if _, err := roRemote.StackImages(ctx, "web"); err != nil {
		t.Fatalf("StackImages on read-only agent should succeed, got %v", err)
	}
	// RollbackImages, ChangeImage, and RunAction("update") must be refused with ErrHostReadOnly.
	if _, err := roRemote.RollbackImages(ctx, "web", map[string]engine.RollbackTarget{}, "admin"); err != host.ErrHostReadOnly {
		t.Fatalf("RollbackImages on read-only agent: got %v, want ErrHostReadOnly", err)
	}
	if _, err := roRemote.ChangeImage(ctx, "web", "web", "nginx:1.27", "nginx:1.28", "admin"); err != host.ErrHostReadOnly {
		t.Fatalf("ChangeImage on read-only agent: got %v, want ErrHostReadOnly", err)
	}
	if _, err := roRemote.RunAction(ctx, "web", "update", "admin"); err != host.ErrHostReadOnly {
		t.Fatalf("RunAction(update) on read-only agent: got %v, want ErrHostReadOnly", err)
	}

	// 2. Older agent without M8 endpoints returns 404 -> hub marks services as "unsupported" ("Update the agent to check images").
	oldMux := http.NewServeMux()
	oldMux.HandleFunc("GET /stacks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stacks":[{"name":"legacy","managed":true,"status":"up","running":1,"total":1}]}`))
	})
	oldMux.HandleFunc("GET /stacks/{name}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"legacy","managed":true,"status":"up","running":1,"total":1,"containers":[{"service":"app","image":"alpine:3.20"}]}`))
	})
	oldRemote := newPipeRemote(t, oldMux)

	reg := host.NewRegistry(oldRemote, "old-agent", false, st)
	upd := New(st, reg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	rows, err := upd.CheckNow(ctx, "local", "legacy")
	if err != nil {
		t.Fatalf("CheckNow against older agent: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != engine.ImageUnsupported || rows[0].Error != "Update the agent to check images" {
		t.Fatalf("expected unsupported status row, got %+v", rows)
	}
}

func newPipeRemote(t *testing.T, handler http.Handler) *host.Remote {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() {
		srv.Close()
		ln.Close()
	})
	return host.NewRemote(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", ln.Addr().String())
	})
}
