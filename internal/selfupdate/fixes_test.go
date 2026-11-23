package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"moorage/internal/config"
	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/store"
)

// --- binary mode ---------------------------------------------------------------------

func writePendingFixture(t *testing.T, dir string, deadline time.Time) (exe, prev, data string) {
	t.Helper()
	binDir := filepath.Join(dir, "bin")
	data = filepath.Join(dir, "data")
	_ = os.MkdirAll(binDir, 0o755)
	_ = os.MkdirAll(data, 0o755)
	exe = filepath.Join(binDir, "moorage")
	prev = filepath.Join(binDir, "moorage.prev")
	_ = os.WriteFile(exe, []byte("new-0.0.2"), 0o750)
	_ = os.WriteFile(prev, []byte("old-0.0.1"), 0o750)
	st := PendingState{
		Action: "apply", Mode: "binary", FromVersion: "0.0.1-test", ToVersion: "0.0.2-test",
		ExePath: exe, PrevPath: prev, DataDir: data, StartedAt: deadline.Add(-healthTimeout), Deadline: deadline,
	}
	if err := writeJSONFile(filepath.Join(data, pendingStateFile), st, 0o600); err != nil {
		t.Fatal(err)
	}
	return exe, prev, data
}

func TestBootCheckRollsBackCrashLoopingBinary(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	exe, prev, data := writePendingFixture(t, t.TempDir(), now.Add(time.Hour))

	// The old version booting never touches a fresh pending state.
	if got := BootCheck(data, "0.0.1-test", now, nil); got != "" || !PendingExists(data) {
		t.Fatalf("old version: BootCheck=%q pending=%v", got, PendingExists(data))
	}
	// Three starts of the new binary are allowed (systemd restarts it after crashes).
	for i := 1; i <= maxBootAttempts; i++ {
		if got := BootCheck(data, "0.0.2-test", now, nil); got != "" {
			t.Fatalf("attempt %d: unexpected rollback to %q", i, got)
		}
	}
	// The next start restores moorage.prev and asks the caller to exec it.
	if got := BootCheck(data, "0.0.2-test", now, nil); got != exe {
		t.Fatalf("expected rollback exec of %q, got %q", exe, got)
	}
	if b := mustRead(t, exe); string(b) != "old-0.0.1" {
		t.Fatalf("exe not restored: %q", b)
	}
	if fi, _ := os.Stat(exe); fi.Mode().Perm() != 0o750 {
		t.Fatalf("restored exe mode = %v, want 0750", fi.Mode().Perm())
	}
	if _, err := os.Stat(prev); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("moorage.prev should be consumed by the rollback")
	}
	if PendingExists(data) {
		t.Fatal("pending state should be removed after the boot rollback")
	}
	note, ok := readRollbackNote(data)
	if !ok || !strings.Contains(note.Reason, "failed to start 3 times") || note.ToVersion != "0.0.2-test" {
		t.Fatalf("unexpected rollback note %+v", note)
	}
	if fi, _ := os.Stat(filepath.Join(data, rollbackNoteFile)); fi.Mode().Perm() != 0o644 {
		t.Fatalf("rollback note mode = %v, want 0644", fi.Mode().Perm())
	}
	// The process stopped by the rollback recognises it and exits non-zero.
	if !RolledBackAway(data, "0.0.2-test", now.Add(-time.Minute)) || RolledBackAway(data, "0.0.1-test", now.Add(-time.Minute)) {
		t.Fatal("RolledBackAway must match only the rolled-back version")
	}
}

func TestBootCheckDeadlineAndStaleState(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	exe, _, data := writePendingFixture(t, t.TempDir(), now.Add(-time.Second))
	if got := BootCheck(data, "v0.0.2-test", now, nil); got != exe {
		t.Fatalf("expected rollback after deadline, got %q", got)
	}
	if note, _ := readRollbackNote(data); !strings.Contains(note.Reason, "did not become healthy") {
		t.Fatalf("unexpected note %+v", note)
	}

	// A stale pending state found by the old version after its deadline is dropped.
	_, _, data2 := writePendingFixture(t, t.TempDir(), now.Add(-time.Second))
	if got := BootCheck(data2, "0.0.1-test", now, nil); got != "" || PendingExists(data2) {
		t.Fatalf("stale state: got %q pending=%v", got, PendingExists(data2))
	}
}

func TestFinalizePendingRecordsPrevAndStopsWatcher(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	_, prev, data := writePendingFixture(t, t.TempDir(), now.Add(time.Hour))
	statePath := filepath.Join(data, pendingStateFile)

	if FinalizePending(data, "0.0.1-test", now) {
		t.Fatal("finalise must ignore another version")
	}
	if !FinalizePending(data, "0.0.2-test", now) || PendingExists(data) {
		t.Fatal("expected pending state finalised")
	}
	rec, ok := readPrevRecord(data)
	if !ok || rec.Version != "0.0.1-test" || rec.PrevPath != prev {
		t.Fatalf("unexpected prev record %+v", rec)
	}
	// Once finalised, BootCheck no longer counts or rolls back.
	if got := BootCheck(data, "0.0.2-test", now.Add(2*time.Hour), nil); got != "" {
		t.Fatalf("BootCheck after finalise = %q", got)
	}

	// The watcher stops quietly when the pending state disappears under it.
	_, _, data3 := writePendingFixture(t, t.TempDir(), now.Add(time.Hour))
	statePath = filepath.Join(data3, pendingStateFile)
	checks := 0
	err := RunWatch(statePath, &BinaryApplier{
		Now:   func() time.Time { return now },
		Sleep: func(time.Duration) {},
		HealthCheckFn: func(context.Context, PendingState) bool {
			checks++
			_ = os.Remove(statePath) // the new binary confirmed itself
			return false
		},
		RestartFn: func(string, []string) error { t.Fatal("must not roll back"); return nil },
	})
	if err != nil || checks != 1 {
		t.Fatalf("RunWatch = %v after %d checks", err, checks)
	}
}

func TestStageAndSwapKeepsFileMode(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "moorage")
	if err := os.WriteFile(exe, []byte("old"), 0o750); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(exe, 0o750)
	newBytes := []byte("new-binary")
	fx := makeSignedFixture(t, "0.0.2-test", "moorage-linux-amd64", "linux/amd64", newBytes, "", "")
	b := &BinaryApplier{
		DataDir: filepath.Join(dir, "data"), ExePath: exe, CurrentVersion: "0.0.1-test", PubKey: fx.pub,
		GOOS: "linux", GOARCH: "amd64", SpawnWatchFn: func(_, _ string) error { return nil },
	}
	if _, err := b.StageAndSwap(fx.manifest, bytes.NewReader(newBytes)); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{exe, filepath.Join(dir, "moorage.prev")} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o750 {
			t.Fatalf("%s mode = %v, want 0750", p, fi.Mode().Perm())
		}
	}
}

// --- container mode ------------------------------------------------------------------

func ctrRuntime(t *testing.T, targetID string) *RuntimeEnv {
	dir := t.TempDir()
	return &RuntimeEnv{
		GOOS: "linux", GOARCH: "amd64",
		Stat: func(p string) (os.FileInfo, error) {
			if p == "/.dockerenv" {
				return os.Stat(dir)
			}
			return nil, os.ErrNotExist
		},
		ReadFile: func(p string) ([]byte, error) {
			if p == "/proc/self/mountinfo" && targetID != "" {
				return []byte("1 1 0:1 / /var/lib/docker/containers/" + targetID + "/resolv.conf rw\n"), nil
			}
			return nil, os.ErrNotExist
		},
		Hostname:       func() (string, error) { return "moorage-agent", nil },
		Getenv:         func(string) string { return "" },
		SelfUpdateFlag: true,
	}
}

const testTargetID = "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"

func TestContainerPrevRestartPolicyDisabledAndRestored(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	eng := newFakeContainerEngine()
	eng.addContainer(sampleTargetInspect(testTargetID))
	_ = writeJSONFile(filepath.Join(dataDir, healthyMarkFile), HealthyMark{Version: "0.0.2-test", At: now.Add(time.Second)}, 0o600)
	opts := HelperOptions{
		TargetID: testTargetID, NewImage: "img@sha256:new", FromVer: "0.0.1-test", ToVer: "0.0.2-test",
		DataDir: dataDir, HealthTimeout: 5 * time.Second, PollInterval: time.Millisecond,
		Now: func() time.Time { return now }, Sleep: func(time.Duration) {},
	}
	if err := RunHelper(context.Background(), eng, opts); err != nil {
		t.Fatal(err)
	}
	prev, _ := eng.ContainerInspect(context.Background(), "moorage-agent-moorage-prev", client.ContainerInspectOptions{})
	if prev.Container.HostConfig.RestartPolicy.Name != container.RestartPolicyDisabled {
		t.Fatalf("kept container restart policy = %q, want no", prev.Container.HostConfig.RestartPolicy.Name)
	}
	rec, ok := readPrevRecord(dataDir)
	if !ok || rec.PrevRestartPolicy == nil || rec.PrevRestartPolicy.Name != container.RestartPolicyUnlessStopped {
		t.Fatalf("prev record lacks original policy: %+v", rec)
	}
	if fi, _ := os.Stat(filepath.Join(dataDir, prevMetaFile)); fi.Mode().Perm() != 0o644 {
		t.Fatalf("prev record mode = %v, want 0644", fi.Mode().Perm())
	}

	// Manual rollback gives the restored container its original policy back.
	cur, _ := eng.ContainerInspect(context.Background(), "moorage-agent", client.ContainerInspectOptions{})
	if err := RunHelper(context.Background(), eng, HelperOptions{
		TargetID: cur.Container.ID, Rollback: true, DataDir: dataDir, HealthTimeout: time.Second,
		PollInterval: time.Millisecond, Now: func() time.Time { return now }, Sleep: func(time.Duration) {},
	}); err != nil {
		t.Fatal(err)
	}
	restored, _ := eng.ContainerInspect(context.Background(), "moorage-agent", client.ContainerInspectOptions{})
	if restored.Container.ID != testTargetID || restored.Container.HostConfig.RestartPolicy.Name != container.RestartPolicyUnlessStopped {
		t.Fatalf("restored %s policy %q", restored.Container.ID, restored.Container.HostConfig.RestartPolicy.Name)
	}

	// Automatic rollback (health timeout) restores the policy too.
	eng2 := newFakeContainerEngine()
	eng2.addContainer(sampleTargetInspect(testTargetID))
	tick := 0
	opts2 := opts
	opts2.DataDir = t.TempDir()
	opts2.HealthTimeout = 2 * time.Second
	opts2.Now = func() time.Time { tick++; return now.Add(time.Duration(tick) * time.Second) }
	if err := RunHelper(context.Background(), eng2, opts2); err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("expected rollback, got %v", err)
	}
	back, _ := eng2.ContainerInspect(context.Background(), "moorage-agent", client.ContainerInspectOptions{})
	if back.Container.ID != testTargetID || back.Container.HostConfig.RestartPolicy.Name != container.RestartPolicyUnlessStopped {
		t.Fatalf("auto rollback left policy %q on %s", back.Container.HostConfig.RestartPolicy.Name, back.Container.ID)
	}
}

func TestHelperInheritsTargetContextAndRollbackUsesImageID(t *testing.T) {
	dataDir := t.TempDir()
	target := sampleTargetInspect(testTargetID)
	target.Image = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	target.Config.User = "1000:999"
	target.HostConfig.GroupAdd = []string{"998"}
	target.HostConfig.SecurityOpt = []string{"no-new-privileges:true", "apparmor=moorage"}
	eng := newFakeContainerEngine()
	eng.addContainer(target)

	fx := makeSignedFixture(t, "0.0.2-test", "moorage-linux-amd64", "linux/amd64", []byte("x"), "", "")
	ca := &ContainerApplier{
		Eng: eng, DataDir: dataDir, CurrentVersion: "0.0.1-test", PubKey: fx.pub,
		GOOS: "linux", GOARCH: "amd64", Runtime: ctrRuntime(t, testTargetID), HelperCheckDelay: time.Millisecond,
	}
	if err := ca.PrepareAndLaunchHelper(context.Background(), fx.manifest); err != nil {
		t.Fatal(err)
	}
	h := eng.created[len(eng.created)-1]
	if h.Config.User != "1000:999" || len(h.HostConfig.GroupAdd) != 1 || h.HostConfig.GroupAdd[0] != "998" ||
		len(h.HostConfig.SecurityOpt) != 2 {
		t.Fatalf("helper lacks target identity: user=%q groups=%v secopt=%v", h.Config.User, h.HostConfig.GroupAdd, h.HostConfig.SecurityOpt)
	}
	// Agents too (not only the hub) join the target's network.
	if string(h.HostConfig.NetworkMode) != "front" || h.NetworkingConfig == nil || h.NetworkingConfig.EndpointsConfig["front"] == nil {
		t.Fatalf("helper network = %q %+v", h.HostConfig.NetworkMode, h.NetworkingConfig)
	}
	if h.Config.Labels[labelUpdateTarget] != "moorage-agent" {
		t.Fatalf("helper target label = %q", h.Config.Labels[labelUpdateTarget])
	}

	// A second apply while that helper still runs is refused, and nothing is pulled.
	pulls := len(eng.pulledRefs)
	if err := ca.PrepareAndLaunchHelper(context.Background(), fx.manifest); !errors.Is(err, host.ErrSelfUpdateBusy) {
		t.Fatalf("expected busy while helper runs, got %v", err)
	}
	if len(eng.pulledRefs) != pulls {
		t.Fatal("refused apply must not pull")
	}

	// Rollback helper runs from the image ID, not the (movable) tag.
	for id, c := range eng.ctrs {
		if c.insp.Config.Labels[engine.LabelRole] == engine.RoleUpdater {
			delete(eng.ctrs, id)
		}
	}
	eng.addContainer(container.InspectResponse{
		ID: strings.Repeat("b", 64), Name: "/moorage-agent-moorage-prev",
		Config: &container.Config{Image: "old"}, State: &container.State{Status: "exited"},
	})
	writePrevRecord(dataDir, PrevRecord{Mode: "container", Version: "0.0.0-test", PrevContainer: "moorage-agent-moorage-prev", SavedAt: time.Now()})
	if err := ca.LaunchRollbackHelper(context.Background()); err != nil {
		t.Fatal(err)
	}
	rb := eng.created[len(eng.created)-1]
	if rb.Config.Image != target.Image {
		t.Fatalf("rollback helper image = %q, want image ID %q", rb.Config.Image, target.Image)
	}
}

func TestHelperThatExitsImmediatelyIsReported(t *testing.T) {
	eng := newFakeContainerEngine()
	eng.addContainer(sampleTargetInspect(testTargetID))
	eng.helperExits = true
	fx := makeSignedFixture(t, "0.0.2-test", "moorage-linux-amd64", "linux/amd64", []byte("x"), "", "")
	ca := &ContainerApplier{
		Eng: eng, DataDir: t.TempDir(), CurrentVersion: "0.0.1-test", PubKey: fx.pub,
		GOOS: "linux", GOARCH: "amd64", Runtime: ctrRuntime(t, testTargetID), HelperCheckDelay: time.Millisecond,
	}
	err := ca.PrepareAndLaunchHelper(context.Background(), fx.manifest)
	if err == nil || !strings.Contains(err.Error(), "exited immediately") {
		t.Fatalf("expected helper failure reported, got %v", err)
	}
}

func TestFindSelfContainerRequiresMoorageAndUniqueHostname(t *testing.T) {
	// An exact ID hint that isn't a running Moorage container is not "self".
	notMoorage := sampleTargetInspect(testTargetID)
	notMoorage.Config.Entrypoint = []string{"/bin/sh"}
	notMoorage.Path = "/bin/sh"
	eng := newFakeContainerEngine()
	eng.addContainer(notMoorage)
	if insp, _ := findSelfContainer(context.Background(), eng, *ctrRuntime(t, testTargetID)); insp != nil {
		t.Fatal("matched a container that doesn't run moorage")
	}

	stopped := sampleTargetInspect(testTargetID)
	stopped.State = &container.State{Status: "exited"}
	eng = newFakeContainerEngine()
	eng.addContainer(stopped)
	if insp, _ := findSelfContainer(context.Background(), eng, *ctrRuntime(t, testTargetID)); insp != nil {
		t.Fatal("matched a stopped container")
	}

	// Hostname fallback: two running Moorage containers share the custom hostname.
	a := sampleTargetInspect(strings.Repeat("a", 64))
	a.Name = "/one"
	a.Config.Hostname = "moorage-agent"
	b := sampleTargetInspect(strings.Repeat("c", 64))
	b.Name = "/two"
	b.Config.Hostname = "moorage-agent"
	eng = newFakeContainerEngine()
	eng.addContainer(a)
	eng.addContainer(b)
	insp, reason := findSelfContainer(context.Background(), eng, *ctrRuntime(t, ""))
	if insp != nil || !strings.Contains(reason, "matches 2") {
		t.Fatalf("expected ambiguity refusal, got %v %q", insp, reason)
	}
	// With one candidate the fallback works.
	delete(eng.ctrs, b.ID)
	if insp, reason := findSelfContainer(context.Background(), eng, *ctrRuntime(t, "")); insp == nil || insp.ID != a.ID {
		t.Fatalf("expected single hostname match, got %v %q", insp, reason)
	}
}

// --- service -------------------------------------------------------------------------

type auditRec struct {
	mu   sync.Mutex
	rows []store.AuditEntry
}

func (a *auditRec) fn(_ context.Context, e store.AuditEntry) {
	a.mu.Lock()
	a.rows = append(a.rows, e)
	a.mu.Unlock()
}

func (a *auditRec) has(action string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.rows {
		if r.Action == action {
			return true
		}
	}
	return false
}

func newTestService(t *testing.T) (*Service, *store.Store, *host.Registry, *auditRec) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	reg := host.NewRegistry(nil, "", false, st)
	ar := &auditRec{}
	cfg := config.Config{DataDir: t.TempDir(), UpdateURL: "http://127.0.0.1:1/releases", UpdateChannel: "stable", SelfUpdate: true}
	s := NewService(cfg, "0.0.1-test", false, st, reg, nil, nil, nil, ar.fn, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return s, st, reg, ar
}

func waitStatus(t *testing.T, st *store.Store, node, want string) store.SystemUpdate {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		m, _ := st.LatestSystemUpdateByNode(context.Background())
		if m[node].Status == want || time.Now().After(deadline) {
			if m[node].Status != want {
				t.Fatalf("%s job status = %q (%s), want %q", node, m[node].Status, m[node].Message, want)
			}
			return m[node]
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *Service) locked(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[id] != 0
}

func TestHubRestartTimeoutFailsStuckJob(t *testing.T) {
	s, st, _, ar := newTestService(t)
	s.HubRestartTimeout = 30 * time.Millisecond
	job, err := st.CreateSystemUpdate(context.Background(), store.SystemUpdate{
		NodeID: "hub", Action: store.SysUpdateActionApply, FromVersion: "0.0.1-test", ToVersion: "0.0.2-test",
		Mode: "container", Status: store.SysUpdateRestarting,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.lockNode("hub", job.ID)
	s.setHubMaintenance(context.Background(), true)
	s.watchHubRestart(job)
	if !s.locked("hub") {
		t.Fatal("hub lock must stay held while the restart is pending")
	}
	got := waitStatus(t, st, "hub", store.SysUpdateFailed)
	if !strings.Contains(got.Message, "did not restart") || !ar.has("update.failed") {
		t.Fatalf("unexpected failure %+v audit=%v", got, ar.rows)
	}
	if s.locked("hub") || !s.HubUpdateUntil(time.Now()).IsZero() {
		t.Fatal("lock and hub maintenance must be released after the timeout")
	}
}

func TestHubJobConfirmedOnlyAfterServingAndHelperRecord(t *testing.T) {
	s, st, _, ar := newTestService(t)
	s.Version = "0.0.2-test"
	s.HubConfirmAfter = time.Hour // no fallback: confirmation must come from the helper record
	s.HealthTimeout = time.Hour
	job, _ := st.CreateSystemUpdate(context.Background(), store.SystemUpdate{
		NodeID: "hub", Action: store.SysUpdateActionApply, FromVersion: "0.0.1-test", ToVersion: "0.0.2-test",
		Mode: "container", Status: store.SysUpdateRestarting, StartedAt: time.Now().Add(-time.Minute),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.ReconcileAfterRestart(ctx)
	if !s.locked("hub") {
		t.Fatal("hub must stay locked while its job is confirmed")
	}
	writePrevRecord(s.Cfg.DataDir, PrevRecord{Mode: "container", Version: "0.0.1-test", ToVersion: "0.0.2-test", SavedAt: time.Now()})
	time.Sleep(50 * time.Millisecond)
	if m, _ := st.LatestSystemUpdateByNode(ctx); m["hub"].Status != store.SysUpdateRestarting {
		t.Fatalf("job settled before the hub served: %+v", m["hub"])
	}
	s.MarkServing()
	waitStatus(t, st, "hub", store.SysUpdateDone)
	if !ar.has("update.done") || s.locked("hub") {
		t.Fatalf("audit=%v locked=%v", ar.rows, s.locked("hub"))
	}
	_ = job
}

// stubUpdater is an agent-side AgentSelfUpdater with a fixed status.
type stubUpdater struct {
	mu    sync.Mutex
	st    host.SelfUpdateStatus
	delay time.Duration
	calls atomic.Int32
}

func (u *stubUpdater) Status(context.Context) (host.SelfUpdateStatus, error) {
	u.calls.Add(1)
	time.Sleep(u.delay)
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.st, nil
}
func (u *stubUpdater) ApplyBinary(context.Context, host.SelfUpdateManifest, io.Reader) error {
	return nil
}
func (u *stubUpdater) ApplyContainer(context.Context, host.SelfUpdateManifest) error { return nil }
func (u *stubUpdater) Rollback(context.Context) error                                { return nil }
func (u *stubUpdater) ConfirmHealthy(string)                                         {}

func attachStubAgent(t *testing.T, st *store.Store, reg *host.Registry, id, ver string, u *stubUpdater) *host.Remote {
	t.Helper()
	seedAgent(t, st, id, id)
	rm := startRemoteAgentHandler(t, host.NewAgentHandler(stubHost{}, host.AgentOptions{Version: ver, SelfUpdate: u}))
	reg.Attach(id, rm, host.AgentInfo{HostInfo: stubHost{}.Info(context.Background()), Version: ver}, nil)
	return rm
}

func TestReconcileGivesAgentsGraceToReconnect(t *testing.T) {
	s, st, reg, ar := newTestService(t)
	s.AgentReconcileGrace = time.Hour
	_, _ = st.CreateSystemUpdate(context.Background(), store.SystemUpdate{
		NodeID: "ag1", Action: store.SysUpdateActionApply, FromVersion: "0.0.1-test", ToVersion: "0.0.2-test",
		Mode: "binary", Status: store.SysUpdateRestarting,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.ReconcileAfterRestart(ctx)
	time.Sleep(50 * time.Millisecond)
	if m, _ := st.LatestSystemUpdateByNode(ctx); m["ag1"].Status != store.SysUpdateRestarting {
		t.Fatalf("agent job settled before the agent reconnected: %+v", m["ag1"])
	}
	if !s.locked("ag1") {
		t.Fatal("agent must stay locked during the grace period")
	}
	attachStubAgent(t, st, reg, "ag1", "0.0.2-test", &stubUpdater{})
	s.OnAgentConnect("ag1")
	waitStatus(t, st, "ag1", store.SysUpdateDone)
	if !ar.has("update.done") {
		t.Fatal("missing update.done audit")
	}

	// An agent that never returns fails only after the grace period.
	s2, st2, _, ar2 := newTestService(t)
	s2.AgentReconcileGrace = 40 * time.Millisecond
	_, _ = st2.CreateSystemUpdate(context.Background(), store.SystemUpdate{
		NodeID: "ag9", Action: store.SysUpdateActionApply, FromVersion: "0.0.1-test", ToVersion: "0.0.2-test",
		Status: store.SysUpdateRestarting,
	})
	s2.ReconcileAfterRestart(ctx)
	waitStatus(t, st2, "ag9", store.SysUpdateFailed)
	if !ar2.has("update.failed") {
		t.Fatal("missing update.failed audit")
	}
}

func TestAgentWaitChecksRollbackAfterTimeout(t *testing.T) {
	s, st, reg, _ := newTestService(t)
	s.AgentWait = 50 * time.Millisecond
	u := &stubUpdater{st: host.SelfUpdateStatus{Version: "0.0.1-test", LastRollback: "rolled back: new binary exited"}}
	attachStubAgent(t, st, reg, "ag1", "0.0.1-test", u)
	// The hub never saw the agent disconnect (fast restart), yet the rollback is found.
	status, msg := s.waitAgentOutcome(context.Background(), "ag1", "0.0.1-test", "0.0.2-test", make(chan string))
	if status != store.SysUpdateRolledBack || !strings.Contains(msg, "rolled back") {
		t.Fatalf("got %s %q, want rolled-back", status, msg)
	}
	if defaultAgentWait <= healthTimeout {
		t.Fatal("agent wait must exceed the agent's own health window")
	}

	// A reconnect signal counts as a restart even when no disconnect was polled: the
	// rollback is reported at once, not after the whole wait.
	s.AgentWait = time.Hour
	ch := make(chan string, 1)
	ch <- "0.0.1-test"
	start := time.Now()
	status, _ = s.waitAgentOutcome(context.Background(), "ag1", "0.0.1-test", "0.0.2-test", ch)
	if status != store.SysUpdateRolledBack || time.Since(start) > 3*time.Second {
		t.Fatalf("got %s after %s, want a fast rolled-back", status, time.Since(start))
	}
}

func TestOverviewAsksAgentsInParallelAndCaches(t *testing.T) {
	s, st, reg, _ := newTestService(t)
	u1 := &stubUpdater{delay: 300 * time.Millisecond, st: host.SelfUpdateStatus{Mode: "binary", Supported: true, SelfUpdate: true}}
	u2 := &stubUpdater{delay: 300 * time.Millisecond, st: host.SelfUpdateStatus{Mode: "binary", Supported: true, SelfUpdate: true}}
	attachStubAgent(t, st, reg, "ag1", "0.0.1-test", u1)
	attachStubAgent(t, st, reg, "ag2", "0.0.1-test", u2)

	start := time.Now()
	ov, err := s.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 550*time.Millisecond {
		t.Fatalf("overview took %s; agents were not asked in parallel", took)
	}
	if len(ov.Nodes) != 3 || ov.Nodes[1].Mode != "binary" {
		t.Fatalf("unexpected nodes %+v", ov.Nodes)
	}
	if _, err := s.Overview(context.Background()); err != nil {
		t.Fatal(err)
	}
	if u1.calls.Load() != 1 || u2.calls.Load() != 1 {
		t.Fatalf("status not cached: %d/%d calls", u1.calls.Load(), u2.calls.Load())
	}
	s.OnAgentConnect("ag1") // a reconnect invalidates that agent
	_, _ = s.Overview(context.Background())
	if u1.calls.Load() != 2 || u2.calls.Load() != 1 {
		t.Fatalf("invalidation wrong: %d/%d calls", u1.calls.Load(), u2.calls.Load())
	}
}

func TestManualCommand(t *testing.T) {
	rel := &ReleaseInfo{
		Version: "0.0.2", Tag: "v0.0.2", Image: "registry.example/moorage",
		Assets: map[string]string{"SHA256SUMS": "https://mirror.example/rel/v0.0.2/SHA256SUMS"},
	}
	bin := buildManualCommand("binary", "", rel, "https://mirror.example/api/releases")
	for _, want := range []string{"uname -m", "https://mirror.example/rel/v0.0.2/moorage-linux-$ARCH", "sha256sum -c --ignore-missing SHA256SUMS"} {
		if !strings.Contains(bin, want) {
			t.Fatalf("binary command lacks %q:\n%s", want, bin)
		}
	}
	if arm := buildManualCommand("binary", "linux/arm64", rel, ""); !strings.Contains(arm, "moorage-linux-arm64") || strings.Contains(arm, "uname") {
		t.Fatalf("known platform should pick its asset: %s", arm)
	}
	gh := buildManualCommand("binary", "", &ReleaseInfo{Version: "1.2.3", Tag: "v1.2.3"}, "https://api.github.com/repos/fork/moorage2/releases")
	if !strings.Contains(gh, "https://github.com/fork/moorage2/releases/download/v1.2.3/") {
		t.Fatalf("fork URL not honoured: %s", gh)
	}
	ctr := buildManualCommand("container", "", rel, "")
	if !strings.Contains(ctr, "image: registry.example/moorage:0.0.2") || !strings.Contains(ctr, "docker compose up -d") {
		t.Fatalf("container command: %s", ctr)
	}
}
