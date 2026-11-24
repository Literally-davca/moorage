package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/notify"
	"moorage/internal/store"
)

// Tunables.
const (
	DeploySilence = 3 * time.Minute  // failures right after a deploy don't alert
	BatchWindow   = 10 * time.Second // alerts of one stack within this window become one message
	Retention     = 30 * 24 * time.Hour
	maxParallel   = 16
	contextLines  = 20
)

// Result statuses.
const (
	StatusUp       = "up"
	StatusDown     = "down"
	StatusSilenced = "silenced" // failed during a deploy window
	StatusPaused   = "paused"   // stack stopped on purpose
	StatusPending  = "pending"  // failing, but fewer times than the retries
	StatusUnknown  = "unknown"  // no result yet
)

// DeployKinds are stack events that silence checks of the stack for a while.
var DeployKinds = map[string]bool{"deploy": true, "restart": true, "update": true, "rollback": true, "stop": true, "down": true, "start": true}

// State is what the UI shows for a check.
type State struct {
	Status string     `json:"status"`
	Since  time.Time  `json:"since,omitzero"`
	Last   *LastProbe `json:"last,omitempty"`
	// Locations are the per-location answers of the last run of a multi-location check.
	Locations []LocationResult `json:"locations,omitempty"`
	// Targets are the per-target answers of the last run of a multi-target check.
	Targets []TargetResult `json:"targets,omitempty"`
}

// LastProbe is the newest result.
type LastProbe struct {
	At        time.Time `json:"at"`
	Status    string    `json:"status"`
	LatencyMS int       `json:"latencyMs"`
	Msg       string    `json:"msg"`
}

type state struct {
	status   string
	since    time.Time
	fails    int
	incident int64
	downAt   time.Time // when the open incident started (survives a maintenance window)
	last     *LastProbe
	chk      *store.Check     // the definition of the last run, for maintenance scope lookups
	kicked   bool             // an early run after a maintenance window is queued
	root     string           // "host/stack" of the cascade root the open incident was folded into
	locs     []LocationResult // per-location answers of the last run (multi-location checks only)
	targets  []TargetResult   // per-target answers of the last run (multi-target checks only)
}

// Monitor schedules checks and turns their results into incidents and alerts.
type Monitor struct {
	st        *store.Store
	reg       *host.Registry
	log       *slog.Logger
	publicURL string

	ctx         context.Context
	sem         chan struct{}
	mu          sync.Mutex
	runners     map[int64]context.CancelFunc
	states      map[int64]*state
	watchers    map[int64]*logWatcher
	silence     map[string]time.Time // host/stack -> until
	hostSilence map[string]time.Time // hostID -> until (silences host-offline alerts during self-update)
	batches     map[string]*batch
	agentDown   map[string]time.Time // agent ID -> time when offline alert was sent
	started     time.Time            // when this hub process started watching
	kicks       map[int64]chan struct{}

	// planned maintenance (maintenance.go)
	maint        *maintIndex
	maintTick    time.Time
	maintActive  map[string]ActiveMaintenance
	maintLogged  map[string]bool
	maintReload  chan struct{}
	onMaint      func()
	maintRefresh sync.Mutex // serialises RefreshMaintenance

	// dependency graph for cascade grouping (cascade.go); nil means no grouping
	deps         DepGraph
	stackDownAt  map[string]time.Time // "host/stack" -> when its last incident opened (pruned after CascadeWindow)
	onStackEvent func(hostID, stack, kind string)
	OnStackDown  func(hostID, stack, checkName, cause string)
	// Push receives every alert (check or host) for Web Push, channels or not.
	Push func(ctx context.Context, event string, msg notify.Message)

	// alert deliveries per channel kind since start, for the metrics endpoint
	alertsSent   map[string]uint64
	alertsFailed map[string]uint64

	// send is replaceable in tests.
	send func(ctx context.Context, ch store.Channel, m notify.Message) error
}

// New creates a monitor; call Start to run it.
func New(st *store.Store, reg *host.Registry, log *slog.Logger, publicURL string) *Monitor {
	m := &Monitor{
		st: st, reg: reg, log: log, publicURL: strings.TrimRight(publicURL, "/"),
		sem: make(chan struct{}, maxParallel), runners: map[int64]context.CancelFunc{},
		states: map[int64]*state{}, watchers: map[int64]*logWatcher{},
		silence: map[string]time.Time{}, hostSilence: map[string]time.Time{}, batches: map[string]*batch{},
		agentDown: map[string]time.Time{}, kicks: map[int64]chan struct{}{},
		maint: &maintIndex{}, maintLogged: map[string]bool{}, maintReload: make(chan struct{}, 1),
		alertsSent: map[string]uint64{}, alertsFailed: map[string]uint64{},
		stackDownAt: map[string]time.Time{},
	}
	m.send = func(ctx context.Context, ch store.Channel, msg notify.Message) error {
		var cfg notify.Config
		if err := json.Unmarshal([]byte(ch.Config), &cfg); err != nil {
			return err
		}
		return notify.Send(ctx, ch.Kind, cfg, msg)
	}
	return m
}

// Start loads checks and open incidents and runs every enabled check.
func (m *Monitor) Start(ctx context.Context) error {
	m.ctx = ctx
	m.started = time.Now()
	checks, err := m.st.Checks(ctx)
	if err != nil {
		return err
	}
	open, err := m.st.OpenIncidents(ctx)
	if err != nil {
		return err
	}
	inMaint := m.lastInMaintenance(ctx, checks)
	m.mu.Lock()
	for _, c := range checks {
		s := &state{status: StatusUnknown, chk: &c}
		if in, ok := open[c.ID]; ok {
			s.status, s.since, s.incident, s.fails, s.downAt = StatusDown, in.StartedAt, in.ID, c.Retries, in.StartedAt
			s.root = in.CausedBy
		}
		if at, ok := inMaint[c.ID]; ok {
			// It was failing inside a window when Moorage stopped: keep that, and keep the
			// failure count, so it still alerts once if it is down after the window.
			s.status, s.since, s.fails = StatusMaintenance, at, max(c.Retries, 1)
		}
		m.states[c.ID] = s
	}
	m.mu.Unlock()
	next := m.RefreshMaintenance(ctx, time.Now())
	go m.maintenanceLoop(ctx, next)
	for _, c := range checks {
		if c.Enabled {
			m.startRunner(c)
		}
	}
	go m.prune(ctx)
	go m.watchAgents(ctx)
	m.log.Info("checks started", "count", len(checks))
	return nil
}

// Reload restarts a check after it was saved, or stops it after deletion.
func (m *Monitor) Reload(id int64) {
	m.mu.Lock()
	if cancel := m.runners[id]; cancel != nil {
		cancel()
		delete(m.runners, id)
	}
	delete(m.kicks, id)
	delete(m.watchers, id)
	m.mu.Unlock()
	c, err := m.st.CheckByID(m.ctx, id)
	if err != nil { // deleted
		m.mu.Lock()
		delete(m.states, id)
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	s := m.states[id]
	if s == nil {
		s = &state{status: StatusUnknown}
		m.states[id] = s
	}
	s.chk = &c
	if !MultiLocation(c) {
		s.locs = nil
	}
	if !MultiTarget(c) {
		s.targets = nil
	}
	if !c.Enabled {
		s.status, s.since, s.fails = StatusPaused, time.Now(), 0
	} else if s.status == StatusPaused {
		s.status = StatusUnknown
	}
	m.mu.Unlock()
	if c.Enabled {
		m.startRunner(c)
	}
}

// SetState sets the in-memory state of a check (used by tests across packages).
func (m *Monitor) SetState(id int64, st State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[id] = &state{status: st.Status, since: st.Since, last: st.Last, locs: st.Locations, targets: st.Targets}
}

// State returns the current state of a check.
func (m *Monitor) State(id int64) State {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.states[id]
	if s == nil {
		return State{Status: StatusUnknown}
	}
	return State{Status: s.status, Since: s.since, Last: s.last, Locations: s.locs, Targets: s.targets}
}

// StackEvent records what happened to a stack; deploy-like events silence its checks.
func (m *Monitor) StackEvent(hostID, stack, kind, actor, detail string) {
	now := time.Now()
	if err := m.st.AddEvent(context.Background(), store.Event{TS: now, HostID: hostID, Stack: stack, Kind: kind, Actor: actor, Detail: detail}); err != nil {
		m.log.Warn("event", "err", err)
	}
	m.mu.Lock()
	if DeployKinds[kind] {
		m.silence[hostID+"/"+stack] = now.Add(DeploySilence)
	}
	hook := m.onStackEvent
	m.mu.Unlock()
	if hook != nil {
		hook(hostID, stack, kind)
	}
}

// OnStackEvent registers a function called after every stack event (dependency detection
// re-runs on saves and deploys).
func (m *Monitor) OnStackEvent(fn func(hostID, stack, kind string)) {
	m.mu.Lock()
	m.onStackEvent = fn
	m.mu.Unlock()
}

// quiet reports whether failures of c at t must not alert: a deploy or a maintenance window.
func (m *Monitor) quiet(c store.Check, at time.Time) bool {
	if _, ok := m.maintenanceFor(c, at); ok {
		return true
	}
	return m.silenced(c, at)
}

func (m *Monitor) silenced(c store.Check, at time.Time) bool {
	if c.Stack == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return at.Before(m.silence[c.HostID+"/"+c.Stack])
}

// Test runs a check definition once without storing anything.
func (m *Monitor) Test(ctx context.Context, c store.Check) Outcome {
	if MultiTarget(c) {
		return m.probeTargets(ctx, c, time.Now())
	}
	if MultiLocation(c) {
		return m.probeLocations(ctx, c)
	}
	return Probe(ctx, c, m.reg)
}

// ---- scheduling ------------------------------------------------------------

func (m *Monitor) startRunner(c store.Check) {
	ctx, cancel := context.WithCancel(m.ctx)
	kick := make(chan struct{}, 1)
	var w *logWatcher
	if c.Kind == KindLog {
		w = &logWatcher{started: time.Now()}
	}
	m.mu.Lock()
	if old := m.runners[c.ID]; old != nil {
		old()
	}
	m.runners[c.ID] = cancel
	m.kicks[c.ID] = kick
	if w != nil {
		m.watchers[c.ID] = w
	} else {
		delete(m.watchers, c.ID)
	}
	m.mu.Unlock()
	if w != nil {
		go m.watchLog(ctx, c, w)
	}
	go m.run(ctx, c, kick)
}

// run probes c every interval; a kick (the end of a maintenance window) probes it at once.
func (m *Monitor) run(ctx context.Context, c store.Check, kick <-chan struct{}) {
	interval := time.Duration(c.Interval) * time.Second
	// spread the first runs so a restart doesn't fire every check at once
	first := time.Duration(rand.Int64N(int64(min(interval, 10*time.Second)) + 1))
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kick:
			t.Stop()
		}
		select {
		case m.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		now := time.Now()
		var out Outcome
		switch {
		case c.Kind == KindLog:
			out = m.probeLog(ctx, c, now)
		case MultiTarget(c):
			out = m.probeTargets(ctx, c, now)
		case MultiLocation(c):
			out = m.probeLocations(ctx, c)
		default:
			out = Probe(ctx, c, m.reg)
		}
		<-m.sem
		if ctx.Err() != nil {
			return
		}
		m.handle(c, out, now)
		t.Reset(interval)
	}
}

// handle stores a result and moves the check between states.
func (m *Monitor) handle(c store.Check, out Outcome, now time.Time) {
	status := StatusUp
	multi := MultiTarget(c)
	switch {
	case out.Quiet != "":
		status = out.Quiet // every target of a multi-target check sat the run out
	case out.Unknown:
		status = StatusUnknown // no location could run the check: neither up nor down
	case out.Paused:
		status = StatusPaused
	case multi && out.Degraded:
		status = StatusDegraded // targets in maintenance or a deploy were already left out
	case multi && !out.OK:
		status = StatusDown
	case !out.OK && m.inMaintenance(c, now):
		status = StatusMaintenance
	case !out.OK && m.silenced(c, now):
		status = StatusSilenced
	case !out.OK:
		status = StatusDown
	}
	r := store.Result{CheckID: c.ID, TS: now, Status: status, LatencyMS: int(out.Latency.Milliseconds()), Msg: out.Msg}
	if err := m.st.AddResult(context.Background(), r); err != nil {
		m.log.Warn("store result", "check", c.ID, "err", err)
	}
	if len(out.Locations) > 0 {
		if err := m.st.AddLocationResults(context.Background(), storeLocations(c.ID, now, out.Locations)); err != nil {
			m.log.Warn("store location results", "check", c.ID, "err", err)
		}
	}

	m.mu.Lock()
	s := m.states[c.ID]
	if s == nil {
		s = &state{status: StatusUnknown}
		m.states[c.ID] = s
	}
	s.last = &LastProbe{At: now, Status: status, LatencyMS: r.LatencyMS, Msg: out.Msg}
	s.locs = out.Locations
	s.targets = out.Targets
	s.chk = &c
	s.kicked = false
	// A down that was announced before a maintenance window stays announced through it:
	// its recovery alerts, and failing on after the window doesn't alert again.
	wasDown := s.status == StatusDown || s.status == StatusDegraded || (s.status == StatusMaintenance && s.incident != 0)
	prevStatus := s.status
	incident, downSince, root := s.incident, s.since, s.root
	if !s.downAt.IsZero() {
		downSince = s.downAt
	}
	var open, closeIt, escalate bool
	switch status {
	case StatusUp:
		closeIt = wasDown
		if s.status != StatusUp {
			s.status, s.since = StatusUp, now
		}
		s.fails, s.incident, s.downAt, s.root = 0, 0, time.Time{}, ""
	case StatusPaused:
		closeIt = wasDown
		if s.status != StatusPaused {
			s.status, s.since = StatusPaused, now
		}
		s.fails, s.incident, s.downAt, s.root = 0, 0, time.Time{}, ""
	case StatusUnknown:
		// Nobody could run the check (every location offline or unsupported). That is no
		// vote: the failure count stays, an announced down stays down (no recovery alert),
		// and a check that was up shows unknown instead of a stale "up".
		if !wasDown && s.status != StatusMaintenance && s.status != StatusUnknown {
			s.status, s.since = StatusUnknown, now
		}
	case StatusSilenced:
		// a deploy is in progress: keep the state, don't count the failure
	case StatusMaintenance:
		// Planned work: no alert now, but count the failure, so a check that is still
		// failing after the window reaches its retries (and alerts once) right away.
		s.fails++
		if s.status != StatusMaintenance {
			s.status, s.since = StatusMaintenance, now
		}
	case StatusDown, StatusDegraded:
		s.fails++
		if wasDown && s.status != status {
			// Already announced. Partially down getting worse (down) updates the incident
			// and escalates; getting better (still partially down) only changes the state.
			escalate = status == StatusDown && prevStatus == StatusDegraded
			if prevStatus == StatusDown || prevStatus == StatusDegraded {
				s.status, s.since = status, now
			} else {
				s.status, s.since = status, downSince // back from a window, already announced
			}
		} else if !wasDown {
			if s.fails >= max(c.Retries, 1) {
				s.status, s.since, s.downAt = status, now, now
				open = true
				m.markStackDownLocked(c, now)
			} else if s.status != StatusPending {
				s.status, s.since = StatusPending, now
			}
		}
	}
	m.mu.Unlock()

	switch {
	case open:
		m.openIncident(c, out, now)
	case escalate:
		if err := m.st.SetIncidentCauseText(context.Background(), incident, out.Msg); err != nil {
			m.log.Warn("update incident", "err", err)
		}
		m.log.Warn("check down (was partially down)", "check", c.Name, "cause", out.Msg)
		m.enqueue(alert{check: c, cause: out.Msg, at: now, ctx: m.gatherContext(c, out), escalated: true})
	case closeIt:
		if err := m.st.CloseIncident(context.Background(), incident, now); err != nil {
			m.log.Warn("close incident", "err", err)
		}
		if status == StatusUp {
			m.enqueue(alert{check: c, up: true, cause: out.Msg, at: now, downFor: now.Sub(downSince), root: root})
		}
		m.log.Info("check recovered", "check", c.Name, "status", status)
	}
}

func (m *Monitor) openIncident(c store.Check, out Outcome, now time.Time) {
	actx := m.gatherContext(c, out)
	b, _ := json.Marshal(actx)
	root := m.rootFor(c, now)
	id, err := m.st.OpenIncident(context.Background(), c.ID, now, out.Msg, string(b))
	if err != nil {
		m.log.Warn("open incident", "err", err)
	} else if root != "" {
		if err := m.st.SetIncidentCause(context.Background(), id, root); err != nil {
			m.log.Warn("incident cause", "err", err)
		}
	}
	m.mu.Lock()
	if s := m.states[c.ID]; s != nil {
		s.incident, s.root = id, root
	}
	m.mu.Unlock()
	m.log.Warn("check down", "check", c.Name, "cause", out.Msg, "rootCause", root)
	m.enqueue(alert{check: c, cause: out.Msg, at: now, ctx: actx, root: root, partial: out.Degraded})
	if m.OnStackDown != nil && c.Stack != "" {
		go m.OnStackDown(c.HostID, c.Stack, c.Name, out.Msg)
	}
}

// ---- log watcher -----------------------------------------------------------

type logMatch struct {
	at       time.Time
	line     engine.LogLine
	silenced bool // arrived during a deploy of the stack or a maintenance window
}

// logWatcher keeps a bounded ring buffer of matching log lines for one check.
type logWatcher struct {
	mu      sync.Mutex
	started time.Time // the watcher sees nothing from before this
	matches []logMatch
}

func (w *logWatcher) record(at time.Time, l engine.LogLine, silenced bool) {
	l.Message = clip(l.Message, maxMatchLineLen)
	m := logMatch{at: at, line: l, silenced: silenced}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.matches) >= maxMatches {
		copy(w.matches, w.matches[1:])
		w.matches[len(w.matches)-1] = m
		return
	}
	w.matches = append(w.matches, m)
}

// since returns the number of matches at or after cutoff and the newest matching
// lines. Matches that arrived during a deploy count only while withSilenced is set,
// so errors printed while a stack restarts don't raise an alert once the deploy
// silence is over.
func (w *logWatcher) since(cutoff time.Time, withSilenced bool) (int, []engine.LogLine) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var lines []engine.LogLine
	for _, m := range w.matches {
		if !m.at.Before(cutoff) && (withSilenced || !m.silenced) {
			lines = append(lines, m.line)
		}
	}
	count := len(lines)
	if len(lines) > maxAlertMatches {
		lines = lines[len(lines)-maxAlertMatches:]
	}
	return count, lines
}

func (m *Monitor) watcher(id int64) *logWatcher {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.watchers[id]
	if w == nil {
		w = &logWatcher{started: time.Now()}
		m.watchers[id] = w
	}
	return w
}

// watchLog follows matching log lines of c's stack until ctx is cancelled,
// reconnecting with exponential backoff when the stream ends or fails.
func (m *Monitor) watchLog(ctx context.Context, c store.Check, w *logWatcher) {
	f := ParseSettings(c.Settings).LogFilter()
	match, err := f.Compile()
	if err != nil {
		return
	}
	backoff := 2 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		if m.reg != nil {
			if h, _, err := m.reg.Get(ctx, c.HostID); err == nil {
				_ = h.WatchLogs(ctx, c.Stack, c.Target, f, func(l engine.LogLine) {
					if match(l.Message) {
						now := time.Now()
						w.record(now, l, m.quiet(c, now))
					}
				})
			}
		}
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) >= 30*time.Second {
			backoff = 2 * time.Second
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(backoff*2, 60*time.Second)
	}
}

// announcedDown reports whether check id has an open incident (its down alert was sent).
func (m *Monitor) announcedDown(id int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.states[id]
	return s != nil && s.incident != 0
}

func (m *Monitor) probeLog(ctx context.Context, c store.Check, now time.Time) Outcome {
	st := ParseSettings(c.Settings)
	timeout := time.Duration(max(c.Timeout, 1)) * time.Second
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	ok, out := probeLogStack(pctx, m.reg, c.HostID, c.Stack, c.Target)
	if !ok {
		out.Latency = time.Since(start)
		return out
	}
	win := st.LogWindow()
	// Matches during a deploy or maintenance don't open an incident. But for a down that is
	// already announced they still count: otherwise the end of the window looks like a
	// recovery ("up again"), and the next match announces the same problem a second time.
	count, lines := m.watcher(c.ID).since(now.Add(-win), m.quiet(c, now) || m.announcedDown(c.ID))
	if count >= st.LogThreshold() {
		word := "matches"
		if count == 1 {
			word = "match"
		}
		first := ""
		if len(lines) > 0 {
			first = ": " + clip(lines[0].Message, 120)
		}
		return Outcome{
			Latency: time.Since(start),
			Msg:     fmt.Sprintf("%d %s of %q in %s%s", count, word, clip(st.Pattern, 40), human(win), first),
			Matches: lines,
		}
	}
	// After a restart of Moorage (or an edit) the buffer starts empty: a check
	// that was down stays down until a whole window was watched, otherwise every
	// restart would send a false "back up" followed by a new alert.
	if w := m.watcher(c.ID); now.Sub(w.started) < win && m.State(c.ID).Status == StatusDown {
		return Outcome{
			Latency: time.Since(start),
			Msg:     fmt.Sprintf("watching again since %s, %d of %d matches so far", w.started.Format("15:04:05"), count, st.LogThreshold()),
		}
	}
	return Outcome{
		OK:      true,
		Latency: time.Since(start),
		Msg:     fmt.Sprintf("no matches in the last %s", human(win)),
	}
}

// ---- alert context ---------------------------------------------------------

// AlertContext is what was going on in the stack when a check went down.
type AlertContext struct {
	Host       string             `json:"host,omitempty"`
	Stack      string             `json:"stack,omitempty"`
	Containers []engine.Container `json:"containers,omitempty"`
	Matches    []engine.LogLine   `json:"matches,omitempty"`
	Logs       []engine.LogLine   `json:"logs,omitempty"`
	Note       string             `json:"note,omitempty"`
	// Locations is the per-location breakdown of a multi-location check.
	Locations []LocationResult `json:"locations,omitempty"`
	// Targets is the per-target breakdown of a multi-target check.
	Targets []TargetResult `json:"targets,omitempty"`
}

// hasStackInfo reports whether the context says anything besides the location breakdown.
func (ac *AlertContext) hasStackInfo() bool {
	return ac.Stack != "" || len(ac.Containers) > 0 || len(ac.Matches) > 0 || len(ac.Logs) > 0 || ac.Note != ""
}

func (m *Monitor) gatherContext(c store.Check, out Outcome) *AlertContext {
	ac := m.gatherStackContext(c, out)
	if len(out.Locations) > 0 {
		if ac == nil {
			ac = &AlertContext{}
		}
		ac.Locations = out.Locations
	}
	if len(out.Targets) > 0 {
		if ac == nil {
			ac = &AlertContext{}
		}
		ac.Targets = out.Targets
	}
	return ac
}

func (m *Monitor) gatherStackContext(c store.Check, out Outcome) *AlertContext {
	stack := stackOf(c)
	if stack == "" || c.HostID == "" || m.reg == nil {
		if len(out.Matches) > 0 {
			return &AlertContext{Stack: stack, Matches: out.Matches}
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(m.ctx, 8*time.Second)
	defer cancel()
	h, e, err := m.reg.Get(ctx, c.HostID)
	ac := &AlertContext{Host: e.Name, Stack: stack, Matches: out.Matches}
	if err != nil {
		ac.Note = "The host is offline, no container details."
		return ac
	}
	if s, err := h.GetStack(ctx, stack); err == nil {
		ac.Containers = s.Containers
	}
	if lines, err := h.TailLogs(ctx, stack, contextLines); err == nil {
		ac.Logs = lines
	} else {
		ac.Note = "Logs couldn't be read: " + err.Error()
	}
	return ac
}

// ---- batching and sending --------------------------------------------------

type alert struct {
	check   store.Check
	up      bool
	cause   string
	at      time.Time
	downFor time.Duration
	ctx     *AlertContext
	// root is the "host/stack" of the upstream stack this alert is folded into (cascade.go)
	root string
	// afterRoot marks a re-alert of a dependent that stayed down after its root recovered
	afterRoot string
	// partial: a multi-target check went partially down; escalated: it went from
	// partially down to down
	partial, escalated bool
}

type batch struct {
	alerts []alert
	due    time.Time
	timer  *time.Timer
}

func groupKey(c store.Check) string {
	stack := c.Stack
	if c.Kind == KindStack {
		stack = c.Target
	}
	if stack != "" && c.HostID != "" {
		return "stack:" + c.HostID + "/" + stack
	}
	return fmt.Sprintf("check:%d", c.ID)
}

// enqueue adds an alert to its batch. The key is the alert's stack, or the stack of its
// cascade root (cascade.go), so a cascade becomes one message.
func (m *Monitor) enqueue(a alert) {
	now := time.Now()
	m.mu.Lock()
	key, window := m.batchKeyLocked(a)
	b := m.batches[key]
	if b == nil {
		b = &batch{due: now.Add(window)}
		m.batches[key] = b
		b.timer = time.AfterFunc(window, func() { m.flush(key) })
	} else if !a.up && a.root == "" && b.due.After(now.Add(BatchWindow)) {
		// A batch of dependents waits longer; an alert of its own must not wait with it.
		b.due = now.Add(BatchWindow)
		b.timer.Reset(BatchWindow)
	}
	b.alerts = append(b.alerts, a)
	var moved []causeUpdate
	if !a.up && a.root == "" && a.check.Kind != "host" {
		moved = m.pullDependentsLocked(a, key)
	}
	m.mu.Unlock()
	m.saveCauses(moved)
}

func (m *Monitor) flush(key string) {
	m.mu.Lock()
	b := m.batches[key]
	delete(m.batches, key)
	m.mu.Unlock()
	if b == nil || len(b.alerts) == 0 {
		return
	}
	// downs and recoveries are separate messages
	var downs, ups []alert
	for _, a := range b.alerts {
		if a.up {
			ups = append(ups, a)
		} else {
			downs = append(downs, a)
		}
	}
	root, cascade := cascadeRootOfKey(key)
	if len(downs) > 0 {
		var info *cascadeInfo
		if cascade {
			info = m.downCascade(root, downs)
		}
		m.deliverCascade(downs, info)
	}
	if len(ups) > 0 {
		var info *cascadeInfo
		if cascade {
			info = m.upCascade(key, root, ups)
		}
		m.deliverCascade(ups, info)
	}
}

func (m *Monitor) deliver(group []alert) { m.deliverCascade(group, nil) }

// deliverCascade sends one message for group to the union of the channels of every alert
// in it: a dependent folded into its root's message reaches its own channels too, and a
// dependent (or root) without channels adds none and removes none.
func (m *Monitor) deliverCascade(group []alert, info *cascadeInfo) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	want := map[int64]bool{}
	for _, a := range group {
		for _, id := range a.check.Channels {
			want[id] = true
		}
	}
	hostAlert := len(group) > 0 && group[0].check.Kind == "host"
	if m.Push == nil && len(want) == 0 {
		return
	}
	msg := m.composeCascade(group, info)
	if m.Push != nil {
		ev := notify.EventChecks
		if hostAlert {
			ev = notify.EventAgents
		}
		m.Push(ctx, ev, msg)
	}
	if len(want) == 0 {
		return
	}
	channels, err := m.st.Channels(ctx)
	if err != nil {
		m.log.Error("load channels", "err", err)
		return
	}
	for _, ch := range channels {
		if !want[ch.ID] || !ch.Enabled {
			continue
		}
		// A channel can opt out of check alerts while keeping other events. Host alerts
		// were already routed by CheckWatchdogAgents.
		var cfg notify.Config
		if !hostAlert && json.Unmarshal([]byte(ch.Config), &cfg) == nil && !cfg.WantsEvent(notify.EventChecks) {
			continue
		}
		err := m.send(ctx, ch, msg)
		m.countAlert(ch.Kind, err == nil)
		if err != nil {
			m.log.Error("notification failed", "channel", ch.Name, "kind", ch.Kind, "err", err)
		} else {
			m.log.Info("notification sent", "channel", ch.Name, "status", msg.Status)
		}
	}
}

func (m *Monitor) countAlert(kind string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ok {
		m.alertsSent[kind]++
	} else {
		m.alertsFailed[kind]++
	}
}

// AlertCounts returns how many alert messages were delivered and how many failed, per
// channel kind, since this process started. Channel kinds are a fixed set validated on
// save, so the maps stay small. Test messages don't count.
func (m *Monitor) AlertCounts() (sent, failed map[string]uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sent, failed = make(map[string]uint64, len(m.alertsSent)), make(map[string]uint64, len(m.alertsFailed))
	for k, v := range m.alertsSent {
		sent[k] = v
	}
	for k, v := range m.alertsFailed {
		failed[k] = v
	}
	return sent, failed
}

// compose builds the message. The wording is fixed in code (no user templates).
func (m *Monitor) compose(group []alert) notify.Message {
	first := group[0]
	up := first.up
	msg := notify.Message{Status: "down"}
	if up {
		msg.Status = "up"
	}
	where := ""
	if ac := first.ctx; ac != nil && ac.Stack != "" {
		where = fmt.Sprintf("stack %s on %s", ac.Stack, ac.Host)
	} else if s := stackOf(first.check); s != "" {
		where = "stack " + s
		if m.reg != nil && first.check.HostID != "" {
			if _, e, _ := m.reg.Get(context.Background(), first.check.HostID); e.Name != "" {
				where += " on " + e.Name
			}
		}
	}
	switch {
	case len(group) == 1 && first.check.Kind == "host" && up:
		msg.Title = fmt.Sprintf("Host %s is back after %s", first.check.Name, human(first.downFor))
	case len(group) == 1 && first.check.Kind == "host":
		msg.Title = fmt.Sprintf("Host %s is offline since %s", first.check.Name, first.at.Local().Format("15:04"))
	case first.check.Kind == "host" && up:
		msg.Title = fmt.Sprintf("Back up: %d hosts", len(group))
	case first.check.Kind == "host":
		msg.Title = fmt.Sprintf("DOWN: %d hosts", len(group))
	case len(group) == 1 && up:
		msg.Title = "Back up: " + first.check.Name
	case len(group) == 1 && first.escalated:
		msg.Title = "DOWN (was partially down): " + first.check.Name
	case len(group) == 1 && first.partial:
		msg.Title = "PARTIALLY DOWN: " + first.check.Name
	case len(group) == 1:
		msg.Title = "DOWN: " + first.check.Name
	case up:
		msg.Title = fmt.Sprintf("Back up: %d checks", len(group))
	default:
		msg.Title = fmt.Sprintf("DOWN: %d checks", len(group))
	}
	if where != "" {
		msg.Title += " (" + where + ")"
	}

	var lines []string
	for _, a := range group {
		switch {
		case up && a.check.Kind == "host":
			lines = append(lines, fmt.Sprintf("Host %s is back after %s.", a.check.Name, human(a.downFor)))
		case a.check.Kind == "host":
			lines = append(lines, fmt.Sprintf("Host %s is offline since %s.", a.check.Name, a.at.Local().Format("15:04")))
		case up:
			lines = append(lines, fmt.Sprintf("%s is up again after %s. %s", a.check.Name, human(a.downFor), a.cause))
		default:
			lines = append(lines, fmt.Sprintf("%s: %s (%s)", a.check.Name, a.cause, checkSummary(a.check)))
		}
	}
	var data []map[string]any
	for _, a := range group {
		data = append(data, map[string]any{
			"checkId": a.check.ID, "check": a.check.Name, "kind": a.check.Kind, "target": a.check.Target,
			"status": msg.Status, "cause": a.cause, "at": a.at.UTC().Format(time.RFC3339),
		})
	}
	if !up {
		for i, a := range group {
			if a.ctx != nil && len(a.ctx.Locations) > 0 {
				line := locationLine(a.ctx.Locations)
				if len(group) > 1 {
					line = a.check.Name + ": " + line
				}
				lines = append(lines, line)
				data[i]["locations"] = a.ctx.Locations
			}
		}
		var matched []engine.LogLine
		for _, a := range group {
			if a.ctx != nil {
				matched = append(matched, a.ctx.Matches...)
			}
		}
		if len(matched) > maxAlertMatches {
			matched = matched[len(matched)-maxAlertMatches:]
		}
		for _, l := range matched {
			msg.Logs = append(msg.Logs, fmt.Sprintf("%s %s | %s", shortTime(l.Time), l.Container, l.Message))
		}
		// context of the first alert that has one (they share the stack)
		for _, a := range group {
			if a.ctx == nil || !a.ctx.hasStackInfo() {
				continue
			}
			for _, c := range a.ctx.Containers {
				switch {
				case c.ExitCode != nil && *c.ExitCode != 0:
					lines = append(lines, fmt.Sprintf("Container %s exited with code %d.", c.Name, *c.ExitCode))
				case c.Health == "unhealthy":
					lines = append(lines, fmt.Sprintf("Container %s is unhealthy.", c.Name))
				case c.State == "restarting":
					lines = append(lines, fmt.Sprintf("Container %s keeps restarting.", c.Name))
				}
			}
			if a.ctx.Note != "" {
				lines = append(lines, a.ctx.Note)
			}
			if len(matched) > 0 && len(a.ctx.Logs) > 0 {
				msg.Logs = append(msg.Logs, "---")
			}
			for _, l := range a.ctx.Logs {
				msg.Logs = append(msg.Logs, fmt.Sprintf("%s %s | %s", shortTime(l.Time), l.Container, l.Message))
			}
			data[0]["context"] = a.ctx
			break
		}
	}
	msg.Text = strings.Join(lines, "\n")
	msg.Data = data
	pubURL := m.getPublicURL()
	if pubURL != "" {
		if first.check.Kind == "host" {
			msg.URL = pubURL + "/#/hosts"
		} else if s := stackOf(first.check); s != "" && first.check.HostID != "" {
			msg.URL = fmt.Sprintf("%s/#/h/%s/stacks/%s", pubURL, first.check.HostID, s)
		} else {
			msg.URL = fmt.Sprintf("%s/#/checks/%d", pubURL, first.check.ID)
		}
	}
	return msg
}

// SetPublicURL updates the base URL used in alert links without restarting Moorage.
func (m *Monitor) SetPublicURL(u string) {
	m.mu.Lock()
	m.publicURL = strings.TrimRight(u, "/")
	m.mu.Unlock()
}

func (m *Monitor) getPublicURL() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.publicURL
}

// TestChannel sends a test message through a channel.
func (m *Monitor) TestChannel(ctx context.Context, ch store.Channel) error {
	msg := notify.Message{
		Status: "test",
		Title:  "Moorage test notification",
		Text:   "If you can read this, alerts from Moorage will reach you here.",
		Logs:   []string{"12:00:01 web-1 | example log line attached to real alerts"},
		URL:    m.getPublicURL(),
	}
	return m.send(ctx, ch, msg)
}

func stackOf(c store.Check) string {
	if c.Kind == KindStack {
		return c.Target
	}
	return c.Stack
}

func checkSummary(c store.Check) string {
	if c.Kind == KindLog {
		if c.Target != "" {
			return "log " + c.Stack + "/" + c.Target
		}
		return "log " + c.Stack
	}
	return kindLabel(c.Kind) + " " + c.Target
}

func kindLabel(k string) string {
	switch k {
	case KindHTTP:
		return "HTTP"
	case KindTCP:
		return "TCP"
	case KindPing:
		return "ping"
	case KindDNS:
		return "DNS"
	case KindStack:
		return "stack"
	case KindLog:
		return "log"
	}
	return k
}

func shortTime(rfc string) string {
	t, err := time.Parse(time.RFC3339Nano, rfc)
	if err != nil {
		return ""
	}
	return t.Local().Format("15:04:05")
}

func human(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
	}
}

func (m *Monitor) prune(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := m.st.PruneResults(ctx, time.Now().Add(-Retention)); err == nil && n > 0 {
			m.log.Info("old check results pruned", "rows", n)
		}
		if n, err := m.st.PruneLocationResults(ctx, time.Now().Add(-Retention)); err == nil && n > 0 {
			m.log.Info("old per-location results pruned", "rows", n)
		}
		_ = m.st.PruneImageUpdates(ctx, time.Now().Add(-90*24*time.Hour))
		_ = m.st.PruneTerminalSessions(ctx, time.Now().Add(-365*24*time.Hour))
		_ = m.st.PruneAudit(ctx, time.Now().Add(-365*24*time.Hour))
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Monitor) watchAgents(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.CheckWatchdogAgents(ctx, time.Now())
		}
	}
}

// SilenceHost suppresses host-offline watchdog alerts for hostID for duration d
// (used while an agent disconnects to install a self-update or rollback).
func (m *Monitor) SilenceHost(hostID string, d time.Duration) {
	if m == nil || hostID == "" || d <= 0 {
		return
	}
	until := time.Now().Add(d)
	m.mu.Lock()
	if until.After(m.hostSilence[hostID]) {
		m.hostSilence[hostID] = until
	}
	delete(m.agentDown, hostID)
	m.mu.Unlock()
}

// ClearHostSilence ends an active host-offline alert silence for hostID.
func (m *Monitor) ClearHostSilence(hostID string) {
	if m == nil || hostID == "" {
		return
	}
	m.mu.Lock()
	delete(m.hostSilence, hostID)
	m.mu.Unlock()
}

// CheckWatchdogAgents checks whether any participating watchdog agent has been
// disconnected longer than grace (its own network or host issue, while the hub is alive)
// and sends an offline or recovery alert through the watchdog channel.
func (m *Monitor) CheckWatchdogAgents(ctx context.Context, now time.Time) {
	if m.reg == nil || m.st == nil {
		return
	}
	ws, err := m.st.Watchdog(ctx)
	if err != nil {
		return
	}
	watchdogOn := ws.Enabled && ws.ChannelID > 0
	// Channels subscribed to the "agents" event hear about every agent, watchdog or not.
	var eventChannels []int64
	if chs, err := m.st.Channels(ctx); err == nil {
		for _, ch := range chs {
			var cfg notify.Config
			if ch.Enabled && json.Unmarshal([]byte(ch.Config), &cfg) == nil && cfg.WantsEvent(notify.EventAgents) {
				eventChannels = append(eventChannels, ch.ID)
			}
		}
	}
	if !watchdogOn && len(eventChannels) == 0 {
		return
	}
	if watchdogOn && !ws.MaintenanceUntil.IsZero() && now.Before(ws.MaintenanceUntil) {
		return
	}
	// Only a planned window over everything silences host-offline alerts: a window for some
	// stacks says nothing about whole hosts going away.
	if !m.AllScopeUntil(now).IsZero() {
		return
	}
	agents, err := m.st.Agents(ctx)
	if err != nil {
		return
	}
	grace := time.Duration(max(ws.Grace, 1)) * time.Second
	activeIDs := map[string]bool{}
	for _, a := range agents {
		var channels []int64
		if watchdogOn && a.Watchdog {
			channels = append(channels, ws.ChannelID)
		}
		for _, id := range eventChannels {
			if !slices.Contains(channels, id) {
				channels = append(channels, id)
			}
		}
		if len(channels) == 0 {
			continue
		}
		activeIDs[a.ID] = true
		_, _, connected := m.reg.Remote(a.ID)
		m.mu.Lock()
		downSince, wasDown := m.agentDown[a.ID]
		silenceUntil := m.hostSilence[a.ID]
		silenced := !silenceUntil.IsZero() && now.Before(silenceUntil)
		if silenced || (connected && !silenceUntil.IsZero()) {
			if silenced {
				delete(m.agentDown, a.ID)
			}
			if connected {
				delete(m.hostSilence, a.ID)
			}
		}
		m.mu.Unlock()

		if silenced {
			continue
		}

		if connected {
			if wasDown {
				m.mu.Lock()
				delete(m.agentDown, a.ID)
				m.mu.Unlock()
				m.enqueue(alert{
					check:   store.Check{Name: a.Name, Kind: "host", Target: a.ID, Channels: channels},
					up:      true,
					at:      now,
					downFor: max(now.Sub(downSince), time.Second),
				})
			}
			continue
		}
		// The hub only knows an agent left while the hub itself was running. If the
		// hub was down too, its last-seen time is from before that outage, so count
		// from the hub start instead of claiming a time nobody observed.
		since := a.LastSeen
		if since.Before(m.started) {
			since = m.started
		}
		if !wasDown && !a.LastSeen.IsZero() && now.Sub(since) >= grace {
			m.mu.Lock()
			m.agentDown[a.ID] = since
			m.mu.Unlock()
			m.enqueue(alert{
				check: store.Check{Name: a.Name, Kind: "host", Target: a.ID, Channels: channels},
				at:    since,
				cause: fmt.Sprintf("offline since %s", since.Local().Format("15:04")),
			})
		}
	}
	m.mu.Lock()
	for id := range m.agentDown {
		if !activeIDs[id] {
			delete(m.agentDown, id)
		}
	}
	m.mu.Unlock()
}

// ---- strip data for the UI ---------------------------------------------------

// Tick is one result on the signature strip.
type Tick struct {
	At        time.Time `json:"at"`
	Status    string    `json:"status"`
	LatencyMS int       `json:"latencyMs"`
	Msg       string    `json:"msg,omitempty"`
}

// Anchor is a deploy-like event on the strip.
type Anchor struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Actor  string    `json:"actor"`
	Detail string    `json:"detail,omitempty"`
}

// Ticks returns the newest n results of a check, oldest first.
func (m *Monitor) Ticks(ctx context.Context, id int64, n int) ([]Tick, error) {
	rs, err := m.st.RecentResults(ctx, id, n)
	if err != nil {
		return nil, err
	}
	out := make([]Tick, 0, len(rs))
	for _, r := range rs {
		out = append(out, Tick{At: r.TS, Status: r.Status, LatencyMS: r.LatencyMS, Msg: r.Msg})
	}
	return out, nil
}

// Anchors returns deploy-like events of a stack since t.
func (m *Monitor) Anchors(ctx context.Context, hostID, stack string, since time.Time) ([]Anchor, error) {
	kinds := make([]string, 0, len(DeployKinds))
	for k := range DeployKinds {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	evs, err := m.st.Events(ctx, hostID, stack, since, kinds...)
	if err != nil {
		return nil, err
	}
	out := make([]Anchor, 0, len(evs))
	for _, e := range evs {
		out = append(out, Anchor{At: e.TS, Kind: e.Kind, Actor: e.Actor, Detail: e.Detail})
	}
	return out, nil
}
