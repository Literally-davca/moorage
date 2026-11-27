package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/netguard"
	"moorage/internal/notify"
	"moorage/internal/store"
)

func newTestMonitor(t *testing.T) (*Monitor, *store.Store, *[]notify.Message) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "https://moorage.example")
	m.ctx = context.Background()
	var mu sync.Mutex
	sent := &[]notify.Message{}
	m.send = func(_ context.Context, _ store.Channel, msg notify.Message) error {
		mu.Lock()
		*sent = append(*sent, msg)
		mu.Unlock()
		return nil
	}
	return m, st, sent
}

func TestStateMachineRetriesSilenceAndRecovery(t *testing.T) {
	m, st, sent := newTestMonitor(t)
	ctx := context.Background()
	chID, _ := st.SaveChannel(ctx, store.Channel{Name: "ntfy", Kind: "ntfy", Config: `{"url":"https://ntfy.sh/x"}`, Enabled: true})
	id, err := st.SaveCheck(ctx, store.Check{Name: "web", Kind: KindHTTP, Target: "https://x", HostID: "local", Stack: "web",
		Interval: 30, Timeout: 5, Retries: 2, Enabled: true, Channels: []int64{chID}})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := st.CheckByID(ctx, id)
	now := time.Now()
	down := Outcome{Msg: "HTTP 502"}

	// a deploy silences failures: no state change, no alert
	m.StackEvent("local", "web", "deploy", "ann", "")
	m.handle(c, down, now)
	m.handle(c, down, now.Add(time.Second))
	if s := m.State(id); s.Status != StatusUnknown {
		t.Fatalf("silenced failures changed the state: %s", s.Status)
	}

	later := now.Add(DeploySilence + time.Minute)
	m.handle(c, down, later)
	if s := m.State(id); s.Status != StatusPending {
		t.Fatalf("one failure with retries=2 should be pending, got %s", s.Status)
	}
	m.handle(c, down, later.Add(30*time.Second))
	if s := m.State(id); s.Status != StatusDown {
		t.Fatalf("want down, got %s", s.Status)
	}
	m.handle(c, down, later.Add(60*time.Second)) // still down: no second incident
	open, _ := st.OpenIncidents(ctx)
	if _, ok := open[id]; !ok || len(open) != 1 {
		t.Fatalf("want one open incident, got %v", open)
	}
	m.flush(groupKey(c))
	if len(*sent) != 1 || (*sent)[0].Status != "down" || !strings.Contains((*sent)[0].Title, "DOWN: web") {
		t.Fatalf("down alert: %+v", *sent)
	}
	if (*sent)[0].URL != "https://moorage.example/#/h/local/stacks/web?ask=explain" { // M19I
		t.Fatalf("link: %q", (*sent)[0].URL)
	}

	m.handle(c, Outcome{OK: true, Msg: "HTTP 200"}, later.Add(90*time.Second))
	m.flush(groupKey(c))
	if len(*sent) != 2 || (*sent)[1].Status != "up" || !strings.Contains((*sent)[1].Text, "after 1 min") {
		t.Fatalf("recovery alert: %+v", (*sent)[1:])
	}
	if open, _ := st.OpenIncidents(ctx); len(open) != 0 {
		t.Fatal("incident not closed")
	}
	if up, n, _ := st.Uptime(ctx, id, now.Add(-time.Hour)); n != 4 || up != 0.25 {
		t.Fatalf("uptime ignores silenced results: got %.2f over %d", up, n)
	}
	anchors, _ := m.Anchors(ctx, "local", "web", now.Add(-time.Minute))
	if len(anchors) != 1 || anchors[0].Actor != "ann" {
		t.Fatalf("anchors: %+v", anchors)
	}
}

func TestBatchingMergesAlertsOfOneStack(t *testing.T) {
	m, st, sent := newTestMonitor(t)
	ctx := context.Background()
	chID, _ := st.SaveChannel(ctx, store.Channel{Name: "hook", Kind: "webhook", Config: `{"url":"https://h"}`, Enabled: true})
	var checks []store.Check
	for i, k := range []string{KindHTTP, KindTCP} {
		id, _ := st.SaveCheck(ctx, store.Check{Name: fmt.Sprintf("c%d", i), Kind: k, Target: "x", HostID: "pi", Stack: "app",
			Interval: 30, Timeout: 5, Retries: 1, Enabled: true, Channels: []int64{chID}})
		c, _ := st.CheckByID(ctx, id)
		checks = append(checks, c)
	}
	for _, c := range checks {
		m.handle(c, Outcome{Msg: "refused"}, time.Now())
	}
	m.flush("stack:pi/app")
	if len(*sent) != 1 || !strings.HasPrefix((*sent)[0].Title, "DOWN: 2 checks") {
		t.Fatalf("want one merged message, got %+v", *sent)
	}
}

func TestComposeAddsContext(t *testing.T) {
	m, _, _ := newTestMonitor(t)
	code := 3
	msg := m.compose([]alert{{
		check: store.Check{ID: 1, Name: "crashy", Kind: KindStack, Target: "crashy", HostID: "local"},
		cause: "0 of 1 containers running",
		ctx: &AlertContext{Host: "pi", Stack: "crashy",
			Containers: []engine.Container{{Name: "crashy-job-1", State: "exited", ExitCode: &code}},
			Logs:       []engine.LogLine{{Time: "2026-11-20T10:00:00Z", Container: "crashy-job-1", Message: "panic: boom"}}},
	}})
	if !strings.Contains(msg.Text, "exited with code 3") || len(msg.Logs) != 1 || !strings.Contains(msg.Logs[0], "panic: boom") {
		t.Fatalf("context missing: %+v", msg)
	}
	if !strings.Contains(msg.Title, "stack crashy on pi") {
		t.Fatalf("title: %q", msg.Title)
	}
}

func TestHTTPProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			fmt.Fprint(w, `{"status":"healthy","items":[{"n":3}],"ok":true}`)
		case "/err":
			w.WriteHeader(502)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	run := func(path, settings string) Outcome {
		return Probe(ctx, store.Check{Kind: KindHTTP, Target: srv.URL + path, Timeout: 5, Settings: settings}, nil)
	}
	if o := run("/ok", ""); !o.OK || o.Msg != "HTTP 200" {
		t.Fatalf("ok: %+v", o)
	}
	if o := run("/err", ""); o.OK || o.Msg != "HTTP 502" {
		t.Fatalf("502: %+v", o)
	}
	if o := run("/err", `{"status":"500-599"}`); !o.OK {
		t.Fatalf("accepted 5xx: %+v", o)
	}
	if o := run("/ok", `{"keyword":"healthy"}`); !o.OK {
		t.Fatalf("keyword: %+v", o)
	}
	if o := run("/ok", `{"keyword":"healthy","keywordAbsent":true}`); o.OK {
		t.Fatal("absent keyword should fail")
	}
	if o := run("/ok", `{"jsonPath":"items[0].n","jsonValue":"3"}`); !o.OK {
		t.Fatalf("json path: %+v", o)
	}
	if o := run("/ok", `{"jsonPath":"ok","jsonValue":"false"}`); o.OK || !strings.Contains(o.Msg, `ok is "true"`) {
		t.Fatalf("json mismatch: %+v", o)
	}
	if o := Probe(ctx, store.Check{Kind: KindTCP, Target: strings.TrimPrefix(srv.URL, "http://"), Timeout: 5}, nil); !o.OK {
		t.Fatalf("tcp: %+v", o)
	}
}

func TestGuardBlocksMetadata(t *testing.T) {
	for _, s := range []string{"169.254.169.254", "fe80::1", "100.100.100.200", "0.0.0.0", "224.0.0.1", "::ffff:169.254.169.254"} {
		if netguard.Blocked(netip.MustParseAddr(s)) == "" {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"192.168.1.10", "10.0.0.5", "127.0.0.1", "1.1.1.1", "fd12::1"} {
		if r := netguard.Blocked(netip.MustParseAddr(s)); r != "" {
			t.Errorf("%s should be allowed: %s", s, r)
		}
	}
	o := Probe(context.Background(), store.Check{Kind: KindHTTP, Target: "http://169.254.169.254/latest/meta-data/", Timeout: 3}, nil)
	if o.OK || !strings.Contains(o.Msg, "link-local") {
		t.Fatalf("metadata endpoint reachable: %+v", o)
	}
}

func TestStatusSpec(t *testing.T) {
	cases := map[string][]int{"": {200, 302}, "200": {200}, "200-299,404": {204, 404}}
	for spec, ok := range cases {
		for _, code := range ok {
			if !statusOK(code, spec) {
				t.Errorf("%d should match %q", code, spec)
			}
		}
	}
	if statusOK(500, "") || statusOK(301, "200-299") {
		t.Error("unexpected match")
	}
	if ValidStatusSpec("abc") || ValidStatusSpec("300-200") || !ValidStatusSpec("200-299, 301") {
		t.Error("spec validation")
	}
}

type fakeLogHost struct {
	mu      sync.Mutex
	status  string
	batches [][]engine.LogLine
	calls   int
	tail    []engine.LogLine
}

func (f *fakeLogHost) Info(context.Context) engine.HostInfo {
	return engine.HostInfo{Name: "local", Reachable: true}
}
func (f *fakeLogHost) ListStacks(context.Context) ([]engine.StackSummary, error) { return nil, nil }
func (f *fakeLogHost) GetStack(_ context.Context, name string) (engine.Stack, error) {
	f.mu.Lock()
	st := f.status
	f.mu.Unlock()
	if st == "" {
		st = "up"
	}
	return engine.Stack{
		StackSummary: engine.StackSummary{Name: name, Status: st, Running: 1, Total: 1},
		Containers:   []engine.Container{{ID: "c1", Name: name + "-web-1", Service: "web", State: "running"}},
	}, nil
}
func (f *fakeLogHost) Stats(context.Context, string) ([]engine.Stat, error)             { return nil, nil }
func (f *fakeLogHost) Validate(context.Context, string, string, string) error           { return nil }
func (f *fakeLogHost) Save(context.Context, string, string, string, string, bool) error { return nil }
func (f *fakeLogHost) Delete(context.Context, string, string) error                     { return nil }
func (f *fakeLogHost) RunAction(context.Context, string, string, string) (engine.OpInfo, error) {
	return engine.OpInfo{}, nil
}
func (f *fakeLogHost) FollowOp(context.Context, string, func(host.OpEvent) error) error { return nil }
func (f *fakeLogHost) StreamLogs(context.Context, string, string, int, func(engine.LogLine)) error {
	return nil
}
func (f *fakeLogHost) WatchLogs(ctx context.Context, _, _ string, flt engine.LogFilter, emit func(engine.LogLine)) error {
	match, err := flt.Compile()
	if err != nil {
		return err
	}
	f.mu.Lock()
	idx := f.calls
	f.calls++
	var batch []engine.LogLine
	if idx < len(f.batches) {
		batch = f.batches[idx]
	}
	f.mu.Unlock()
	for _, l := range batch {
		if match(l.Message) {
			emit(l)
		}
	}
	if idx >= len(f.batches)-1 {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
func (f *fakeLogHost) TailLogs(context.Context, string, int) ([]engine.LogLine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tail, nil
}
func (f *fakeLogHost) History(context.Context, string) ([]engine.Version, error) { return nil, nil }
func (f *fakeLogHost) Version(context.Context, string, string) (engine.VersionDetail, error) {
	return engine.VersionDetail{}, nil
}
func (f *fakeLogHost) Restore(context.Context, string, string, string) (*engine.Version, error) {
	return nil, nil
}
func (f *fakeLogHost) CheckImages(context.Context, string) ([]engine.ServiceImage, error) {
	return nil, nil
}
func (f *fakeLogHost) StackImages(context.Context, string) (map[string]engine.RunningImage, error) {
	return nil, nil
}
func (f *fakeLogHost) RollbackImages(context.Context, string, map[string]engine.RollbackTarget, string) (engine.OpInfo, error) {
	return engine.OpInfo{}, nil
}
func (f *fakeLogHost) ChangeImage(context.Context, string, string, string, string, string) (*engine.Version, error) {
	return nil, nil
}
func (f *fakeLogHost) Terminal(context.Context, string, string, string, int, int) (engine.TerminalSession, engine.Container, error) {
	return nil, engine.Container{}, host.ErrTerminalDisabled
}

func TestLogCheckWindowRecoverySilenceAndContext(t *testing.T) {
	m, st, sent := newTestMonitor(t)
	fh := &fakeLogHost{
		status: "up",
		tail:   []engine.LogLine{{Time: "2026-11-20T10:00:00Z", Container: "app-web-1", Message: "context tail line"}},
	}
	m.reg = host.NewRegistry(fh, "local", false, st)
	ctx := context.Background()
	chID, _ := st.SaveChannel(ctx, store.Channel{Name: "hook", Kind: "webhook", Config: `{"url":"https://h"}`, Enabled: true})
	id, err := st.SaveCheck(ctx, store.Check{
		Name: "app errors", Kind: KindLog, Target: "web", HostID: "local", Stack: "app",
		Interval: 30, Timeout: 5, Retries: 1, Enabled: true, Channels: []int64{chID},
		Settings: `{"pattern":"FATAL","threshold":2,"window":300}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := st.CheckByID(ctx, id)
	w := m.watcher(id)
	t0 := time.Now()

	// 1 match is below threshold=2 -> check stays up
	w.record(t0, engine.LogLine{Time: "2026-11-20T10:00:01Z", Container: "app-web-1", Message: "FATAL: first failure"}, false)
	out := m.probeLog(ctx, c, t0.Add(10*time.Second))
	if !out.OK || !strings.Contains(out.Msg, "no matches in the last 5 min") {
		t.Fatalf("below threshold should be OK, got %+v", out)
	}
	m.handle(c, out, t0.Add(10*time.Second))
	if s := m.State(id); s.Status != StatusUp {
		t.Fatalf("want up, got %s", s.Status)
	}

	// 2nd match within window reaches threshold -> check fails, but deploy silence suppresses the alert
	w.record(t0.Add(20*time.Second), engine.LogLine{Time: "2026-11-20T10:00:20Z", Container: "app-web-1", Message: "FATAL: second failure"}, false)
	m.StackEvent("local", "app", "deploy", "ann", "")
	silencedAt := time.Now()
	out = m.probeLog(ctx, c, silencedAt)
	if out.OK || len(out.Matches) != 2 {
		t.Fatalf("want failed outcome with 2 matches, got %+v", out)
	}
	m.handle(c, out, silencedAt)
	if s := m.State(id); s.Status != StatusUp {
		t.Fatalf("silenced failure should not move check to down, got %s", s.Status)
	}

	// Clear deploy silence and probe while still inside the 5-minute match window -> check goes down
	m.mu.Lock()
	delete(m.silence, "local/app")
	m.mu.Unlock()
	downAt := t0.Add(30 * time.Second)
	out = m.probeLog(ctx, c, downAt)
	m.handle(c, out, downAt)
	if s := m.State(id); s.Status != StatusDown {
		t.Fatalf("want down, got %s", s.Status)
	}
	m.flush(groupKey(c))
	if len(*sent) != 1 || (*sent)[0].Status != "down" {
		t.Fatalf("want down alert, got %+v", *sent)
	}
	logsJoined := strings.Join((*sent)[0].Logs, "\n")
	if !strings.Contains(logsJoined, "FATAL: first failure") || !strings.Contains(logsJoined, "FATAL: second failure") || !strings.Contains(logsJoined, "context tail line") {
		t.Fatalf("alert logs missing matched or tail lines: %v", (*sent)[0].Logs)
	}
	incs, _ := st.Incidents(ctx, id, 5)
	if len(incs) != 1 {
		t.Fatalf("want 1 incident, got %d", len(incs))
	}
	var actx AlertContext
	if err := json.Unmarshal([]byte(incs[0].Context), &actx); err != nil || len(actx.Matches) != 2 {
		t.Fatalf("incident context matches: %+v %v", actx, err)
	}

	// Stopped stack pauses the check instead of alerting
	fh.mu.Lock()
	fh.status = "stopped"
	fh.mu.Unlock()
	pausedOut := m.probeLog(ctx, c, t0.Add(40*time.Second))
	if !pausedOut.Paused {
		t.Fatalf("stopped stack should pause log check, got %+v", pausedOut)
	}
	fh.mu.Lock()
	fh.status = "up"
	fh.mu.Unlock()

	// A fresh watcher (Moorage restarted) must not report recovery before it watched a whole window
	m.mu.Lock()
	m.watchers[id] = &logWatcher{started: t0.Add(5 * time.Minute)}
	m.mu.Unlock()
	if out := m.probeLog(ctx, c, t0.Add(6*time.Minute)); out.OK {
		t.Fatalf("a new watcher must keep a down check down, got %+v", out)
	}
	m.mu.Lock()
	m.watchers[id] = &logWatcher{started: t0}
	m.mu.Unlock()

	// After the 5-minute window passes with no new matches -> check recovers automatically
	upAt := t0.Add(6 * time.Minute)
	out = m.probeLog(ctx, c, upAt)
	if !out.OK {
		t.Fatalf("want recovery after quiet window, got %+v", out)
	}
	m.handle(c, out, upAt)
	if s := m.State(id); s.Status != StatusUp {
		t.Fatalf("want up after recovery, got %s", s.Status)
	}
	m.flush(groupKey(c))
	if len(*sent) != 2 || (*sent)[1].Status != "up" {
		t.Fatalf("want recovery notification, got %+v", *sent)
	}
}

func TestLogWatcherRingBufferAndReconnect(t *testing.T) {
	var w logWatcher
	now := time.Now()
	for i := range maxMatches + 25 {
		w.record(now, engine.LogLine{Container: "web", Message: fmt.Sprintf("FATAL %d %s", i, strings.Repeat("x", 500))}, false)
	}
	w.mu.Lock()
	bufLen := len(w.matches)
	firstMsg := w.matches[0].line.Message
	w.mu.Unlock()
	if bufLen != maxMatches {
		t.Fatalf("ring buffer exceeded bound: got %d, want %d", bufLen, maxMatches)
	}
	if !strings.HasPrefix(firstMsg, "FATAL 25 ") || len([]rune(firstMsg)) > maxMatchLineLen+1 {
		t.Fatalf("oldest match or clipping wrong: len=%d prefix=%q", len([]rune(firstMsg)), firstMsg[:20])
	}
	count, sample := w.since(now.Add(-time.Minute), false)
	if count != maxMatches || len(sample) != maxAlertMatches {
		t.Fatalf("since count=%d sample=%d", count, len(sample))
	}

	// Reconnect test: first WatchLogs stream ends after 1 matching line, watcher reconnects and picks up the 2nd stream without duplicating the 1st.
	m, st, _ := newTestMonitor(t)
	fh := &fakeLogHost{
		status: "up",
		batches: [][]engine.LogLine{
			{{Container: "web", Message: "tick 1"}, {Container: "web", Message: "FATAL: stream one"}},
			{{Container: "web", Message: "FATAL: stream two"}},
		},
	}
	m.reg = host.NewRegistry(fh, "local", false, st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := store.Check{ID: 99, Name: "watch", Kind: KindLog, HostID: "local", Stack: "app", Settings: `{"pattern":"FATAL"}`}
	rw := m.watcher(c.ID)
	go m.watchLog(ctx, c, rw)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, _ := rw.since(time.Now().Add(-time.Minute), false)
		if n == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	n, lines := rw.since(time.Now().Add(-time.Minute), false)
	if n != 2 || lines[0].Message != "FATAL: stream one" || lines[1].Message != "FATAL: stream two" {
		t.Fatalf("reconnect matches: count=%d lines=%+v", n, lines)
	}
}

// Errors printed while a deploy restarts the stack must not raise an alert once
// the deploy silence is over, even though they are still inside the window.
func TestLogMatchesDuringDeployDontAlertLater(t *testing.T) {
	var w logWatcher
	now := time.Now()
	w.record(now, engine.LogLine{Message: "FATAL during deploy"}, true)
	if n, _ := w.since(now.Add(-time.Minute), true); n != 1 {
		t.Fatalf("during silence the match shows as silenced failure, got %d", n)
	}
	if n, _ := w.since(now.Add(-time.Minute), false); n != 0 {
		t.Fatalf("after silence a deploy-time match must not count, got %d", n)
	}
	w.record(now, engine.LogLine{Message: "FATAL after deploy"}, false)
	if n, lines := w.since(now.Add(-time.Minute), false); n != 1 || lines[0].Message != "FATAL after deploy" {
		t.Fatalf("want only the later match, got %d %+v", n, lines)
	}
}

func TestWatchdogHostOfflineAndRecoveryAlert(t *testing.T) {
	m, st, sent := newTestMonitor(t)
	m.reg = host.NewRegistry(nil, "", false, st)
	ctx := context.Background()

	chID, _ := st.SaveChannel(ctx, store.Channel{Name: "hook", Kind: "webhook", Config: `{"url":"https://h"}`, Enabled: true})
	_ = st.CreateJoinToken(ctx, "hash1", "Raspberry Pi", time.Now().Add(time.Hour))
	_, err := st.RedeemJoinToken(ctx, "hash1", "ag_pi", "pi")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 20, 17, 5, 0, 0, time.UTC)
	_ = st.TouchAgent(ctx, "ag_pi", "pi", t0)
	_ = st.SaveWatchdog(ctx, store.WatchdogSettings{Enabled: true, ChannelID: chID, Grace: 120}, map[string]bool{"ag_pi": true})

	// Before grace -> no alert
	m.CheckWatchdogAgents(ctx, t0.Add(90*time.Second))
	m.flush("hosts")
	if len(*sent) != 0 {
		t.Fatalf("expected no alert before grace, got %+v", *sent)
	}

	// After grace -> one offline alert
	m.CheckWatchdogAgents(ctx, t0.Add(3*time.Minute))
	m.flush("hosts")
	if len(*sent) != 1 || !strings.Contains((*sent)[0].Title, "Host Raspberry Pi is offline since") {
		t.Fatalf("expected host offline alert, got %+v", *sent)
	}

	// Agent reconnects -> one recovery alert
	m.reg.Attach("ag_pi", &host.Remote{}, host.AgentInfo{}, func() error { return nil })
	m.CheckWatchdogAgents(ctx, t0.Add(15*time.Minute))
	m.flush("hosts")
	if len(*sent) != 2 || !strings.Contains((*sent)[1].Title, "Host Raspberry Pi is back after 15 min") {
		t.Fatalf("expected host recovery alert, got %+v", *sent)
	}
}

func TestWatchdogHostSilenceDuringSelfUpdate(t *testing.T) {
	m, st, sent := newTestMonitor(t)
	m.reg = host.NewRegistry(nil, "", false, st)
	ctx := context.Background()
	chID, err := st.SaveChannel(ctx, store.Channel{Name: "wd", Kind: "ntfy", Config: `{"url":"https://ntfy.sh/x"}`, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.CreateJoinToken(ctx, "hash2", "Agent One", time.Now().Add(time.Hour))
	if _, err := st.RedeemJoinToken(ctx, "hash2", "ag_silenced", "agent1"); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	_ = st.TouchAgent(ctx, "ag_silenced", "agent1", t0.Add(-10*time.Second))
	_ = st.SaveWatchdog(ctx, store.WatchdogSettings{Enabled: true, ChannelID: chID, Grace: 1}, map[string]bool{"ag_silenced": true})

	// Silencing host suppresses offline alert even past grace.
	m.SilenceHost("ag_silenced", 3*time.Minute)
	m.CheckWatchdogAgents(ctx, t0.Add(2*time.Minute))
	m.flush("hosts")
	if len(*sent) != 0 {
		t.Fatalf("expected offline alert suppressed during SilenceHost, got %+v", *sent)
	}

	// Clearing silence and checking after grace triggers offline alert cleanly.
	m.ClearHostSilence("ag_silenced")
	m.CheckWatchdogAgents(ctx, t0.Add(2*time.Minute))
	m.flush("hosts")
	if len(*sent) != 1 || !strings.Contains((*sent)[0].Title, "Host Agent One is offline since") {
		t.Fatalf("expected offline alert once silence ends, got %+v", *sent)
	}
}

func TestAlertCountsPerChannelKind(t *testing.T) {
	m, st, _ := newTestMonitor(t)
	ctx := context.Background()
	ok, _ := st.SaveChannel(ctx, store.Channel{Name: "a", Kind: "ntfy", Config: `{"url":"https://ntfy.sh/x"}`, Enabled: true})
	bad, _ := st.SaveChannel(ctx, store.Channel{Name: "b", Kind: "webhook", Config: `{"url":"https://hook.example/x"}`, Enabled: true})
	m.send = func(_ context.Context, ch store.Channel, _ notify.Message) error {
		if ch.Kind == "webhook" {
			return fmt.Errorf("refused")
		}
		return nil
	}
	c := store.Check{ID: 1, Name: "web", Kind: KindHTTP, Channels: []int64{ok, bad}}
	m.deliver([]alert{{check: c, at: time.Now(), cause: "HTTP 502"}})
	m.deliver([]alert{{check: c, up: true, at: time.Now()}})

	sent, failed := m.AlertCounts()
	if sent["ntfy"] != 2 || failed["webhook"] != 2 || sent["webhook"] != 0 || failed["ntfy"] != 0 {
		t.Fatalf("sent %v failed %v", sent, failed)
	}
	// the maps are copies
	sent["ntfy"] = 99
	if s, _ := m.AlertCounts(); s["ntfy"] != 2 {
		t.Fatal("AlertCounts returned the internal map")
	}
	// a test message doesn't count
	m.TestChannel(ctx, store.Channel{Kind: "ntfy"}) //nolint:errcheck
	if s, _ := m.AlertCounts(); s["ntfy"] != 2 {
		t.Fatalf("test message counted: %v", s)
	}
}
