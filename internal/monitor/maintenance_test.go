package monitor

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/store"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func saveWindow(t *testing.T, st *store.Store, w store.MaintenanceWindow) store.MaintenanceWindow {
	t.Helper()
	w.Enabled = true
	if w.Title == "" {
		w.Title = "DB upgrade"
	}
	id, err := st.SaveMaintenanceWindow(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	w.ID = id
	return w
}

func saveTestCheck(t *testing.T, st *store.Store, name, stack string, retries int) store.Check {
	t.Helper()
	ctx := context.Background()
	chs, _ := st.Channels(ctx)
	var chID int64
	if len(chs) > 0 {
		chID = chs[0].ID
	} else {
		chID, _ = st.SaveChannel(ctx, store.Channel{Name: "hook", Kind: "webhook", Config: `{"url":"https://h"}`, Enabled: true})
	}
	id, err := st.SaveCheck(ctx, store.Check{Name: name, Kind: KindHTTP, Target: "https://x", HostID: "local", Stack: stack,
		Interval: 60, Timeout: 5, Retries: retries, Enabled: true, Channels: []int64{chID}})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := st.CheckByID(ctx, id)
	return c
}

func maintEvents(t *testing.T, st *store.Store, stack string) []string {
	t.Helper()
	evs, err := st.Events(context.Background(), "local", stack, time.Time{}, EventMaintenance)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		out = append(out, e.Detail)
	}
	return out
}

// A failing check inside its window is stored as maintenance and doesn't alert; still down
// after the window, it alerts exactly once right away.
func TestMaintenanceStillDownAfterWindowAlertsOnce(t *testing.T) {
	m, st, sent := newTestMonitor(t)
	ctx := context.Background()
	c := saveTestCheck(t, st, "web", "web", 2)
	t0 := time.Now().Truncate(time.Second)
	saveWindow(t, st, store.MaintenanceWindow{Kind: store.MaintenanceOnce, StartsAt: t0.Add(-time.Minute), EndsAt: t0.Add(30 * time.Minute),
		Scope: store.MaintenanceScope{Stacks: []store.MaintenanceStack{{HostID: "local", Stack: "web"}}}})
	m.RefreshMaintenance(ctx, t0)

	down := Outcome{Msg: "HTTP 502"}
	for i := range 3 {
		at := t0.Add(time.Duration(i) * time.Minute)
		m.handle(c, down, at)
	}
	if s := m.State(c.ID); s.Status != StatusMaintenance {
		t.Fatalf("want maintenance, got %s", s.Status)
	}
	if until := m.MaintenanceUntil(c); until.IsZero() {
		t.Fatal("MaintenanceUntil is zero inside the window")
	}
	m.flush(groupKey(c))
	if len(*sent) != 0 {
		t.Fatalf("alert during maintenance: %+v", *sent)
	}
	if open, _ := st.OpenIncidents(ctx); len(open) != 0 {
		t.Fatalf("incident opened during maintenance: %v", open)
	}
	rs, _ := st.RecentResults(ctx, c.ID, 10)
	for _, r := range rs {
		if r.Status != StatusMaintenance {
			t.Fatalf("result stored as %s", r.Status)
		}
	}

	// The window ends: the check is re-run at once (one kick, however often the index
	// refreshes), and the first failing result alerts once (retries already reached).
	kick := make(chan struct{}, 1)
	m.mu.Lock()
	m.kicks[c.ID] = kick
	m.mu.Unlock()
	end := t0.Add(30*time.Minute + time.Second)
	m.RefreshMaintenance(ctx, end)
	select {
	case <-kick:
	default:
		t.Fatal("the check was not re-run at the end of its window")
	}
	m.RefreshMaintenance(ctx, end.Add(time.Second))
	if len(kick) != 0 {
		t.Fatal("kicked twice")
	}
	m.handle(c, down, end)
	m.handle(c, down, end.Add(time.Minute))
	m.flush(groupKey(c))
	if len(*sent) != 1 || (*sent)[0].Status != "down" {
		t.Fatalf("want exactly one down alert after the window, got %+v", *sent)
	}
	if s := m.State(c.ID); s.Status != StatusDown {
		t.Fatalf("want down after the window, got %s", s.Status)
	}
	if ev := maintEvents(t, st, "web"); len(ev) != 2 || !strings.HasPrefix(ev[0], "Started: DB upgrade") || ev[1] != "Ended: DB upgrade" {
		t.Fatalf("logbook events: %q", ev)
	}
	// Uptime ignores maintenance results: 2 downs after the window, 3 maintenance before.
	if up, n, _ := st.Uptime(ctx, c.ID, t0.Add(-time.Hour)); n != 2 || up != 0 {
		t.Fatalf("uptime counted maintenance: %.2f over %d", up, n)
	}
}

// A check that recovers during its window sends nothing: its down was never announced.
func TestMaintenanceRecoveredDuringWindowNoAlert(t *testing.T) {
	m, st, sent := newTestMonitor(t)
	ctx := context.Background()
	c := saveTestCheck(t, st, "api", "api", 1)
	t0 := time.Now().Truncate(time.Second)
	saveWindow(t, st, store.MaintenanceWindow{Kind: store.MaintenanceOnce, StartsAt: t0, EndsAt: t0.Add(20 * time.Minute),
		Scope: store.MaintenanceScope{Checks: []int64{c.ID}}})
	m.RefreshMaintenance(ctx, t0)
	m.handle(c, Outcome{Msg: "refused"}, t0.Add(time.Minute))
	m.handle(c, Outcome{OK: true, Msg: "HTTP 200"}, t0.Add(2*time.Minute))
	m.RefreshMaintenance(ctx, t0.Add(21*time.Minute))
	m.handle(c, Outcome{OK: true, Msg: "HTTP 200"}, t0.Add(21*time.Minute))
	m.flush(groupKey(c))
	if len(*sent) != 0 {
		t.Fatalf("want no alert, got %+v", *sent)
	}
	if s := m.State(c.ID); s.Status != StatusUp {
		t.Fatalf("want up, got %s", s.Status)
	}
}

// A down announced before the window stays announced: failing on inside and after the
// window doesn't alert again, and its recovery does alert.
func TestMaintenanceAnnouncedDownKeepsIncident(t *testing.T) {
	m, st, sent := newTestMonitor(t)
	ctx := context.Background()
	c := saveTestCheck(t, st, "db", "db", 1)
	t0 := time.Now().Truncate(time.Second)
	m.handle(c, Outcome{Msg: "refused"}, t0.Add(-time.Minute))
	m.flush(groupKey(c))
	if len(*sent) != 1 {
		t.Fatalf("want the down alert before the window, got %+v", *sent)
	}
	saveWindow(t, st, store.MaintenanceWindow{Kind: store.MaintenanceOnce, StartsAt: t0, EndsAt: t0.Add(10 * time.Minute),
		Scope: store.MaintenanceScope{All: true}})
	m.RefreshMaintenance(ctx, t0)
	m.handle(c, Outcome{Msg: "refused"}, t0.Add(time.Minute))
	if s := m.State(c.ID); s.Status != StatusMaintenance {
		t.Fatalf("want maintenance, got %s", s.Status)
	}
	m.RefreshMaintenance(ctx, t0.Add(11*time.Minute))
	m.handle(c, Outcome{Msg: "refused"}, t0.Add(11*time.Minute))
	m.flush(groupKey(c))
	if len(*sent) != 1 {
		t.Fatalf("an announced down alerted again: %+v", *sent)
	}
	if s := m.State(c.ID); s.Status != StatusDown {
		t.Fatalf("want down, got %s", s.Status)
	}
	m.handle(c, Outcome{OK: true}, t0.Add(12*time.Minute))
	m.flush(groupKey(c))
	if len(*sent) != 2 || (*sent)[1].Status != "up" || !strings.Contains((*sent)[1].Text, "after 13 min") {
		t.Fatalf("want one recovery alert counted from the first down, got %+v", *sent)
	}
	if open, _ := st.OpenIncidents(ctx); len(open) != 0 {
		t.Fatal("incident still open")
	}
}

// Checks outside the scope alert as usual during a window.
func TestMaintenanceOutOfScopeAlerts(t *testing.T) {
	m, st, sent := newTestMonitor(t)
	ctx := context.Background()
	in := saveTestCheck(t, st, "in", "web", 1)
	out := saveTestCheck(t, st, "out", "other", 1)
	t0 := time.Now().Truncate(time.Second)
	saveWindow(t, st, store.MaintenanceWindow{Kind: store.MaintenanceOnce, StartsAt: t0, EndsAt: t0.Add(time.Hour),
		Scope: store.MaintenanceScope{Stacks: []store.MaintenanceStack{{HostID: "local", Stack: "web"}}, Checks: []int64{in.ID}}})
	m.RefreshMaintenance(ctx, t0)
	m.handle(in, Outcome{Msg: "x"}, t0.Add(time.Minute))
	m.handle(out, Outcome{Msg: "x"}, t0.Add(time.Minute))
	m.flush(groupKey(in))
	m.flush(groupKey(out))
	if len(*sent) != 1 || !strings.Contains((*sent)[0].Title, "DOWN: out") {
		t.Fatalf("want only the out-of-scope alert, got %+v", *sent)
	}
	if ev := maintEvents(t, st, "other"); len(ev) != 0 {
		t.Fatalf("out-of-scope stack got events: %q", ev)
	}
}

// Log rule matches inside the window send no alert, during or after it.
func TestMaintenanceLogRuleNoAlert(t *testing.T) {
	m, st, sent := newTestMonitor(t)
	fh := &fakeLogHost{status: "up"}
	m.reg = host.NewRegistry(fh, "local", false, st)
	ctx := context.Background()
	chID, _ := st.SaveChannel(ctx, store.Channel{Name: "hook", Kind: "webhook", Config: `{"url":"https://h"}`, Enabled: true})
	id, _ := st.SaveCheck(ctx, store.Check{Name: "errors", Kind: KindLog, Target: "", HostID: "local", Stack: "app",
		Interval: 30, Timeout: 5, Retries: 1, Enabled: true, Channels: []int64{chID}, Settings: `{"pattern":"FATAL","threshold":1,"window":300}`})
	c, _ := st.CheckByID(ctx, id)
	t0 := time.Now().Truncate(time.Second)
	saveWindow(t, st, store.MaintenanceWindow{Kind: store.MaintenanceOnce, StartsAt: t0, EndsAt: t0.Add(10 * time.Minute),
		Scope: store.MaintenanceScope{Stacks: []store.MaintenanceStack{{HostID: "local", Stack: "app"}}}})
	m.RefreshMaintenance(ctx, t0)
	w := m.watcher(id)
	w.mu.Lock()
	w.started = t0.Add(-time.Hour)
	w.mu.Unlock()
	at := t0.Add(time.Minute)
	w.record(at, engine.LogLine{Message: "FATAL during upgrade"}, m.quiet(c, at))
	out := m.probeLog(ctx, c, at.Add(time.Second))
	if out.OK {
		t.Fatalf("the match should show as a failure inside the window: %+v", out)
	}
	m.handle(c, out, at.Add(time.Second))
	if s := m.State(id); s.Status != StatusMaintenance {
		t.Fatalf("want maintenance, got %s", s.Status)
	}
	// Right after the window the match is still within the rule's 5 min, but doesn't count.
	after := t0.Add(10*time.Minute + time.Second)
	m.RefreshMaintenance(ctx, after)
	out = m.probeLog(ctx, c, after)
	if !out.OK {
		t.Fatalf("a match from the window counted after it: %+v", out)
	}
	m.handle(c, out, after)
	m.flush(groupKey(c))
	if len(*sent) != 0 {
		t.Fatalf("log rule alerted: %+v", *sent)
	}
}

// A log rule that was already down before a window doesn't "recover" when the window ends
// just because the matches during the window were quiet (review: it sent "up again" and,
// a minute later, the same down a second time).
func TestMaintenanceAnnouncedLogDownStaysDown(t *testing.T) {
	m, st, sent := newTestMonitor(t)
	fh := &fakeLogHost{status: "up"}
	m.reg = host.NewRegistry(fh, "local", false, st)
	ctx := context.Background()
	chID, _ := st.SaveChannel(ctx, store.Channel{Name: "hook", Kind: "webhook", Config: `{"url":"https://h"}`, Enabled: true})
	id, _ := st.SaveCheck(ctx, store.Check{Name: "errors", Kind: KindLog, Target: "", HostID: "local", Stack: "app",
		Interval: 30, Timeout: 5, Retries: 1, Enabled: true, Channels: []int64{chID}, Settings: `{"pattern":"FATAL","threshold":1,"window":300}`})
	c, _ := st.CheckByID(ctx, id)
	t0 := time.Now().Truncate(time.Second)
	w := m.watcher(id)
	w.mu.Lock()
	w.started = t0.Add(-time.Hour)
	w.mu.Unlock()

	// Down and announced before any window.
	w.record(t0, engine.LogLine{Message: "FATAL before"}, m.quiet(c, t0))
	m.handle(c, m.probeLog(ctx, c, t0.Add(time.Second)), t0.Add(time.Second))
	m.flush(groupKey(c))
	if len(*sent) != 1 || m.State(id).Status != StatusDown {
		t.Fatalf("want one down alert before the window, got %d alerts, state %s", len(*sent), m.State(id).Status)
	}

	// A window starts later and the errors go on inside it.
	ws := t0.Add(6 * time.Minute)
	saveWindow(t, st, store.MaintenanceWindow{Kind: store.MaintenanceOnce, StartsAt: ws, EndsAt: ws.Add(10 * time.Minute),
		Scope: store.MaintenanceScope{Stacks: []store.MaintenanceStack{{HostID: "local", Stack: "app"}}}})
	m.RefreshMaintenance(ctx, ws)
	at := ws.Add(8 * time.Minute)
	w.record(at, engine.LogLine{Message: "FATAL during"}, m.quiet(c, at))
	m.handle(c, m.probeLog(ctx, c, at.Add(time.Second)), at.Add(time.Second))

	// Right after the window the error from inside it still keeps the announced down.
	after := ws.Add(10*time.Minute + time.Second)
	m.RefreshMaintenance(ctx, after)
	out := m.probeLog(ctx, c, after)
	if out.OK {
		t.Fatalf("an announced down looked recovered after the window: %+v", out)
	}
	m.handle(c, out, after)
	m.flush(groupKey(c))
	if len(*sent) != 1 {
		t.Fatalf("want no further alert after the window, got %d: %+v", len(*sent), *sent)
	}
}

// A restart in the middle of a window keeps it active and the check's state, and doesn't
// write the start event twice.
func TestMaintenanceSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := saveTestCheck(t, st, "web", "web", 3)
	// disabled, so Start runs no probe against the network
	c.Enabled = false
	if _, err := st.SaveCheck(ctx, c); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	w := saveWindow(t, st, store.MaintenanceWindow{Kind: store.MaintenanceOnce, StartsAt: now.Add(-10 * time.Minute), EndsAt: now.Add(time.Hour),
		Scope: store.MaintenanceScope{Stacks: []store.MaintenanceStack{{HostID: "local", Stack: "web"}}}})

	m1 := New(st, nil, log, "")
	m1.ctx = ctx
	m1.RefreshMaintenance(ctx, now)
	m1.handle(c, Outcome{Msg: "down"}, now)

	// restart: a new monitor on the same database
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	m2 := New(st, nil, log, "")
	if err := m2.Start(runCtx); err != nil {
		t.Fatal(err)
	}
	if a, ok := m2.MaintenanceFor(c, time.Now()); !ok || a.Window.ID != w.ID {
		t.Fatal("window not active after restart")
	}
	if s := m2.State(c.ID); s.Status != StatusMaintenance {
		t.Fatalf("check state after restart: %s", s.Status)
	}
	if ev := maintEvents(t, st, "web"); len(ev) != 1 {
		t.Fatalf("start event written again after restart: %q", ev)
	}
}

// Only a window over everything silences host-offline alerts.
func TestMaintenanceAllScopeSilencesHostOffline(t *testing.T) {
	m, st, sent := newTestMonitor(t)
	m.reg = host.NewRegistry(nil, "", false, st)
	ctx := context.Background()
	chID, _ := st.SaveChannel(ctx, store.Channel{Name: "hook", Kind: "webhook", Config: `{"url":"https://h"}`, Enabled: true})
	_ = st.CreateJoinToken(ctx, "hash1", "Pi", time.Now().Add(time.Hour))
	if _, err := st.RedeemJoinToken(ctx, "hash1", "ag_pi", "pi"); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Truncate(time.Second)
	_ = st.TouchAgent(ctx, "ag_pi", "pi", t0)
	_ = st.SaveWatchdog(ctx, store.WatchdogSettings{Enabled: true, ChannelID: chID, Grace: 120}, map[string]bool{"ag_pi": true})

	stackOnly := saveWindow(t, st, store.MaintenanceWindow{Kind: store.MaintenanceOnce, StartsAt: t0, EndsAt: t0.Add(time.Hour),
		Scope: store.MaintenanceScope{Stacks: []store.MaintenanceStack{{HostID: "ag_pi", Stack: "web"}}}})
	all := saveWindow(t, st, store.MaintenanceWindow{Kind: store.MaintenanceOnce, StartsAt: t0, EndsAt: t0.Add(time.Hour),
		Scope: store.MaintenanceScope{All: true}})
	_ = stackOnly
	m.RefreshMaintenance(ctx, t0.Add(3*time.Minute))
	if got := m.AllScopeUntil(t0.Add(3 * time.Minute)); !got.Equal(all.EndsAt) {
		t.Fatalf("AllScopeUntil = %v", got)
	}
	m.CheckWatchdogAgents(ctx, t0.Add(3*time.Minute))
	m.flush("hosts")
	if len(*sent) != 0 {
		t.Fatalf("host alert during an all-scope window: %+v", *sent)
	}
	// Without the all-scope window, the stack-scoped one doesn't silence it.
	if err := st.DeleteMaintenanceWindow(ctx, all.ID); err != nil {
		t.Fatal(err)
	}
	m.RefreshMaintenance(ctx, t0.Add(4*time.Minute))
	m.CheckWatchdogAgents(ctx, t0.Add(4*time.Minute))
	m.flush("hosts")
	if len(*sent) != 1 || !strings.Contains((*sent)[0].Title, "offline") {
		t.Fatalf("want the host offline alert, got %+v", *sent)
	}
}

func weekly(days []int, start string, minutes int, tz string) store.MaintenanceWindow {
	return store.MaintenanceWindow{Kind: store.MaintenanceWeekly, Weekdays: days, StartTime: start, Duration: minutes, TZ: tz, Enabled: true}
}

func TestWeeklyOccurrencesDSTSkip(t *testing.T) {
	prague := mustLoc(t, "Europe/Prague")
	// 29 Mar 2026: clocks jump from 02:00 to 03:00; 02:30 doesn't exist.
	w := weekly([]int{0}, "02:30", 60, "Europe/Prague")
	day := time.Date(2026, 3, 29, 0, 0, 0, 0, prague)
	occ := Occurrences(w, day, day.Add(24*time.Hour))
	if len(occ) != 1 {
		t.Fatalf("want one occurrence, got %v", occ)
	}
	want := time.Date(2026, 3, 29, 3, 0, 0, 0, prague) // the next valid time
	if !occ[0].Start.Equal(want) || !occ[0].End.Equal(want.Add(time.Hour)) {
		t.Fatalf("start %v, want %v", occ[0].Start.In(prague), want)
	}
	// A week later the window is back at 02:30 local.
	next := Occurrences(w, day.AddDate(0, 0, 7), day.AddDate(0, 0, 8))
	if len(next) != 1 || !next[0].Start.Equal(time.Date(2026, 4, 5, 2, 30, 0, 0, prague)) {
		t.Fatalf("next week: %v", next)
	}
}

func TestWeeklyOccurrencesDSTFallBackOnce(t *testing.T) {
	prague := mustLoc(t, "Europe/Prague")
	// 25 Oct 2026: 02:00–03:00 happens twice; the window must start only once.
	w := weekly([]int{0}, "02:30", 60, "Europe/Prague")
	day := time.Date(2026, 10, 25, 0, 0, 0, 0, prague)
	occ := Occurrences(w, day, day.Add(25*time.Hour))
	if len(occ) != 1 {
		t.Fatalf("want one occurrence on the fall-back day, got %v", occ)
	}
	if l := occ[0].Start.In(prague); l.Hour() != 2 || l.Minute() != 30 || occ[0].End.Sub(occ[0].Start) != time.Hour {
		t.Fatalf("occurrence %v – %v", l, occ[0].End.In(prague))
	}
}

func TestWeeklyOccurrencesAcrossMidnightAndWeekdays(t *testing.T) {
	utc := time.UTC
	// Saturday 23:30 for 2 h: running on Sunday at 00:30, over at 01:30.
	w := weekly([]int{6}, "23:30", 120, "UTC")
	sun := time.Date(2026, 11, 22, 0, 30, 0, 0, utc) // a Sunday
	occ := Occurrences(w, sun, sun.Add(time.Minute))
	if len(occ) != 1 || !occ[0].Active(sun) || !occ[0].End.Equal(time.Date(2026, 11, 22, 1, 30, 0, 0, utc)) {
		t.Fatalf("across midnight: %v", occ)
	}
	if occ := Occurrences(w, sun.Add(time.Hour), sun.Add(2*time.Hour)); len(occ) != 0 {
		t.Fatalf("still active after its end: %v", occ)
	}

	// Mon, Wed, Fri at 02:00 in New York over one week.
	ny := mustLoc(t, "America/New_York")
	w = weekly([]int{1, 3, 5}, "02:00", 30, "America/New_York")
	from := time.Date(2026, 11, 22, 0, 0, 0, 0, ny) // Sunday
	occ = Occurrences(w, from, from.AddDate(0, 0, 7))
	if len(occ) != 3 {
		t.Fatalf("want 3 occurrences, got %v", occ)
	}
	for i, wd := range []time.Weekday{time.Monday, time.Wednesday, time.Friday} {
		l := occ[i].Start.In(ny)
		if l.Weekday() != wd || l.Hour() != 2 || l.Minute() != 0 {
			t.Fatalf("occurrence %d: %v", i, l)
		}
	}
	// disabled windows have no occurrences
	w.Enabled = false
	if occ := Occurrences(w, from, from.AddDate(0, 0, 7)); occ != nil {
		t.Fatalf("disabled window: %v", occ)
	}
}

// The index refreshes at the next edge, so a window starting within the minute isn't late.
func TestMaintenanceNextBoundary(t *testing.T) {
	m, st, _ := newTestMonitor(t)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Second)
	saveWindow(t, st, store.MaintenanceWindow{Kind: store.MaintenanceOnce, StartsAt: t0.Add(20 * time.Second), EndsAt: t0.Add(30 * time.Minute),
		Scope: store.MaintenanceScope{All: true}})
	if next := m.RefreshMaintenance(ctx, t0); !next.Equal(t0.Add(20 * time.Second)) {
		t.Fatalf("next boundary %v", next)
	}
	if len(m.ActiveWindows(t0)) != 0 || len(m.ActiveWindows(t0.Add(25*time.Second))) != 1 {
		t.Fatal("index lookups are not exact within the horizon")
	}
}
