package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"moorage/internal/store"
)

// StatusMaintenance is a failed result during a planned maintenance window: it is stored
// and shown, but it neither alerts nor counts against the uptime.
const StatusMaintenance = "maintenance"

// EventMaintenance is the stack event kind written when a window starts or ends. It is not
// a deploy kind: a maintenance window silences through its own state, not the deploy silence.
const EventMaintenance = "maintenance"

// Maintenance tunables.
const (
	MaintenanceMinDuration = 5 * time.Minute
	MaintenanceMaxDuration = 7 * 24 * time.Hour
	// maintenanceHorizon is how far ahead the in-memory index reaches. It is refreshed at
	// least every minute, so every lookup until the next refresh is exact.
	maintenanceHorizon = 10 * time.Minute
)

// Occurrence is one concrete period of a maintenance window, in UTC.
type Occurrence struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Active reports whether t falls inside the occurrence (start inclusive, end exclusive).
func (o Occurrence) Active(t time.Time) bool {
	return !t.Before(o.Start) && t.Before(o.End)
}

var (
	locMu    sync.Mutex
	locCache = map[string]*time.Location{}
)

// Location resolves a window's time zone; empty means the hub's own zone.
func Location(tz string) (*time.Location, error) {
	if tz == "" || tz == "Local" {
		return time.Local, nil
	}
	locMu.Lock()
	defer locMu.Unlock()
	if l, ok := locCache[tz]; ok {
		return l, nil
	}
	l, err := time.LoadLocation(tz)
	if err != nil {
		return nil, err
	}
	locCache[tz] = l
	return l, nil
}

// ParseClock parses "HH:MM" into minutes after midnight.
func ParseClock(s string) (int, bool) {
	h, m, ok := strings.Cut(s, ":")
	if !ok || len(h) != 2 || len(m) != 2 {
		return 0, false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, false
	}
	return hh*60 + mm, true
}

// wallClock returns the instant of local time hh:mm on the given date. When that time
// doesn't exist (the clock jumps over it at a DST change) it returns the first valid
// instant after it, i.e. the moment of the jump. When it exists twice (fall back), Go
// picks one of them, so a weekly window still starts once.
func wallClock(y int, mo time.Month, d, minutes int, loc *time.Location) time.Time {
	hh, mm := minutes/60, minutes%60
	t := time.Date(y, mo, d, hh, mm, 0, 0, loc)
	ty, tmo, td := t.Date()
	if ty == y && tmo == mo && td == d && t.Hour() == hh && t.Minute() == mm {
		return t
	}
	// In a gap: Go normalised t to one side of the jump. The jump itself is the
	// boundary of the zone period t landed in.
	start, end := t.ZoneBounds()
	if t.Hour()*60+t.Minute() > minutes || td != d {
		if !start.IsZero() {
			return start
		}
		return t
	}
	if !end.IsZero() {
		return end
	}
	return t
}

// Occurrences returns the window's periods that overlap [from, to), oldest first.
// Disabled windows have none.
func Occurrences(w store.MaintenanceWindow, from, to time.Time) []Occurrence {
	if !w.Enabled || !to.After(from) {
		return nil
	}
	switch w.Kind {
	case store.MaintenanceOnce:
		if w.StartsAt.Before(to) && w.EndsAt.After(from) && w.EndsAt.After(w.StartsAt) {
			return []Occurrence{{Start: w.StartsAt.UTC(), End: w.EndsAt.UTC()}}
		}
		return nil
	case store.MaintenanceWeekly:
		return weeklyOccurrences(w, from, to)
	}
	return nil
}

func weeklyOccurrences(w store.MaintenanceWindow, from, to time.Time) []Occurrence {
	loc, err := Location(w.TZ)
	if err != nil {
		return nil
	}
	minutes, ok := ParseClock(w.StartTime)
	dur := time.Duration(w.Duration) * time.Minute
	if !ok || dur <= 0 || len(w.Weekdays) == 0 {
		return nil
	}
	var days [7]bool
	for _, d := range w.Weekdays {
		if d >= 0 && d < 7 {
			days[d] = true
		}
	}
	// Local dates from the day before (from - duration) to the day after to: an occurrence
	// that started earlier can still be running at from.
	first := from.Add(-dur).In(loc)
	last := to.In(loc)
	day := time.Date(first.Year(), first.Month(), first.Day()-1, 12, 0, 0, 0, loc)
	stop := time.Date(last.Year(), last.Month(), last.Day()+1, 12, 0, 0, 0, loc)
	var out []Occurrence
	for ; !day.After(stop); day = time.Date(day.Year(), day.Month(), day.Day()+1, 12, 0, 0, 0, loc) {
		if !days[day.Weekday()] {
			continue
		}
		start := wallClock(day.Year(), day.Month(), day.Day(), minutes, loc)
		o := Occurrence{Start: start.UTC(), End: start.Add(dur).UTC()}
		if o.Start.Before(to) && o.End.After(from) {
			out = append(out, o)
		}
	}
	return out
}

// CoversCheck reports whether the window's scope includes the check: everything, the check
// itself, or the stack it belongs to.
func CoversCheck(w store.MaintenanceWindow, c store.Check) bool {
	if w.Scope.All {
		return true
	}
	for _, id := range w.Scope.Checks {
		if id == c.ID {
			return true
		}
	}
	if s := stackOf(c); s != "" && c.HostID != "" {
		return CoversStack(w, c.HostID, s)
	}
	return false
}

// CoversStack reports whether the window lists the stack (or covers everything).
func CoversStack(w store.MaintenanceWindow, hostID, stack string) bool {
	if w.Scope.All {
		return true
	}
	for _, s := range w.Scope.Stacks {
		if s.HostID == hostID && s.Stack == stack {
			return true
		}
	}
	return false
}

// ActiveMaintenance is a window that is running right now.
type ActiveMaintenance struct {
	Window store.MaintenanceWindow
	Occurrence
}

// maintIndex is the in-memory view of the windows the monitor consults on every check run.
type maintIndex struct {
	windows []store.MaintenanceWindow // enabled windows, from the DB
	occs    []ActiveMaintenance       // occurrences overlapping [built-1 min, built+horizon]
	built   time.Time
}

// active returns the occurrences running at t.
func (ix *maintIndex) active(t time.Time) []ActiveMaintenance {
	var out []ActiveMaintenance
	for _, a := range ix.occs {
		if a.Active(t) {
			out = append(out, a)
		}
	}
	return out
}

func buildIndex(windows []store.MaintenanceWindow, now time.Time) *maintIndex {
	ix := &maintIndex{built: now}
	for _, w := range windows {
		if !w.Enabled {
			continue
		}
		ix.windows = append(ix.windows, w)
		for _, o := range Occurrences(w, now.Add(-time.Minute), now.Add(maintenanceHorizon)) {
			ix.occs = append(ix.occs, ActiveMaintenance{Window: w, Occurrence: o})
		}
	}
	return ix
}

// nextBoundary is the first start or end after now in the index, or zero.
func (ix *maintIndex) nextBoundary(now time.Time) time.Time {
	var next time.Time
	for _, a := range ix.occs {
		for _, t := range []time.Time{a.Start, a.End} {
			if t.After(now) && (next.IsZero() || t.Before(next)) {
				next = t
			}
		}
	}
	return next
}

// MaintenanceFor returns the active window covering c at t.
func (m *Monitor) MaintenanceFor(c store.Check, t time.Time) (ActiveMaintenance, bool) {
	return m.maintenanceFor(c, t)
}

func (m *Monitor) maintenanceFor(c store.Check, t time.Time) (ActiveMaintenance, bool) {
	m.mu.Lock()
	ix := m.maint
	m.mu.Unlock()
	if ix == nil {
		return ActiveMaintenance{}, false
	}
	for _, a := range ix.occs {
		if a.Active(t) && CoversCheck(a.Window, c) {
			return a, true
		}
	}
	return ActiveMaintenance{}, false
}

func (m *Monitor) inMaintenance(c store.Check, t time.Time) bool {
	_, ok := m.maintenanceFor(c, t)
	return ok
}

// lastInMaintenance returns the checks whose newest stored result is a maintenance failure,
// with its time, so a restart in the middle of a window keeps their state.
func (m *Monitor) lastInMaintenance(ctx context.Context, checks []store.Check) map[int64]time.Time {
	out := map[int64]time.Time{}
	for _, c := range checks {
		if r, err := m.st.LastResult(ctx, c.ID); err == nil && r.Status == StatusMaintenance {
			out[c.ID] = r.TS
		}
	}
	return out
}

// MaintenanceUntil returns the planned end of the window covering c now (zero if none).
func (m *Monitor) MaintenanceUntil(c store.Check) time.Time {
	a, ok := m.maintenanceFor(c, time.Now())
	if !ok {
		return time.Time{}
	}
	return a.End
}

// ActiveWindows returns the windows running at t.
func (m *Monitor) ActiveWindows(t time.Time) []ActiveMaintenance {
	m.mu.Lock()
	ix := m.maint
	m.mu.Unlock()
	if ix == nil {
		return nil
	}
	return ix.active(t)
}

// MaintenanceWindows returns the enabled windows as last loaded from the DB.
func (m *Monitor) MaintenanceWindows() []store.MaintenanceWindow {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.maint == nil {
		return nil
	}
	return append([]store.MaintenanceWindow(nil), m.maint.windows...)
}

// AllScopeUntil returns the end of the running window that covers everything (zero if
// none). The hub watchdog uses it: only such a window silences hub and host-offline alerts.
func (m *Monitor) AllScopeUntil(t time.Time) time.Time {
	var until time.Time
	for _, a := range m.ActiveWindows(t) {
		if a.Window.Scope.All && a.End.After(until) {
			until = a.End
		}
	}
	return until
}

// OnMaintenanceChange sets a hook called after a window started or ended (status page
// caches, the hub watchdog push).
func (m *Monitor) OnMaintenanceChange(fn func()) {
	m.mu.Lock()
	m.onMaint = fn
	m.mu.Unlock()
}

// ReloadMaintenance re-reads the windows after an edit, so the change applies before the
// API answers, and wakes the loop to wait for the new next boundary.
func (m *Monitor) ReloadMaintenance(ctx context.Context) {
	m.RefreshMaintenance(ctx, time.Now())
	select {
	case m.maintReload <- struct{}{}:
	default:
	}
}

// maintenanceLoop keeps the index fresh: on every change, every minute and exactly at
// the next start or end, so a window's edge is never late by more than the timer slack.
func (m *Monitor) maintenanceLoop(ctx context.Context, next time.Time) {
	for {
		wait := time.Minute
		if d := time.Until(next); !next.IsZero() && d < wait {
			wait = max(d, 0)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-m.maintReload:
			t.Stop()
		}
		next = m.RefreshMaintenance(ctx, time.Now())
	}
}

// RefreshMaintenance rebuilds the index from the DB, records start and end events, and
// re-runs checks whose window ended. It returns the next boundary. Exported for tests.
func (m *Monitor) RefreshMaintenance(ctx context.Context, now time.Time) time.Time {
	// The loop and an API edit may refresh at once; edges must be logged by one of them.
	m.maintRefresh.Lock()
	defer m.maintRefresh.Unlock()
	windows, err := m.st.MaintenanceWindows(ctx)
	if err != nil {
		m.log.Warn("load maintenance windows", "err", err)
		return time.Time{}
	}
	ix := buildIndex(windows, now)
	m.mu.Lock()
	m.maint = ix
	prevTick := m.maintTick
	m.maintTick = now
	prevActive := m.maintActive
	m.mu.Unlock()
	if prevTick.IsZero() {
		prevTick = now.Add(-time.Hour) // after a restart, catch up on edges we slept through
	}

	nowActive := map[string]ActiveMaintenance{}
	for _, a := range ix.active(now) {
		nowActive[occKey(a)] = a
	}
	changed := m.logEdges(ctx, ix, windows, prevTick, now, prevActive, nowActive)
	m.mu.Lock()
	m.maintActive = nowActive
	hook := m.onMaint
	m.mu.Unlock()
	m.kickFinished(now)
	if changed && hook != nil {
		hook()
	}
	return ix.nextBoundary(now)
}

func occKey(a ActiveMaintenance) string {
	return fmt.Sprintf("%d/%d", a.Window.ID, a.Start.Unix())
}

// logEdges writes the maintenance stack events for windows that started or ended since the
// last refresh. Each edge is written once: remembered in memory, and checked in the DB
// after a restart. It returns whether any window started or ended.
func (m *Monitor) logEdges(ctx context.Context, ix *maintIndex, windows []store.MaintenanceWindow,
	prevTick, now time.Time, prevActive, nowActive map[string]ActiveMaintenance) bool {
	type edge struct {
		a     ActiveMaintenance
		start bool
		at    time.Time
		early bool
	}
	var edges []edge
	for _, w := range windows {
		for _, o := range Occurrences(w, prevTick, now.Add(time.Nanosecond)) {
			a := ActiveMaintenance{Window: w, Occurrence: o}
			if !o.Start.After(now) && o.Start.After(prevTick.Add(-time.Nanosecond)) || nowActive[occKey(a)].Window.ID != 0 {
				edges = append(edges, edge{a: a, start: true, at: o.Start})
			}
			if !o.End.After(now) && o.End.After(prevTick) {
				edges = append(edges, edge{a: a, at: o.End})
			}
		}
	}
	// An occurrence cut short by an edit, a disable or a delete ends now.
	for k, a := range prevActive {
		if _, still := nowActive[k]; !still && a.End.After(now) {
			edges = append(edges, edge{a: a, at: now, early: true})
		}
	}
	if len(edges) == 0 {
		return false
	}
	sort.SliceStable(edges, func(i, j int) bool { return edges[i].at.Before(edges[j].at) })
	var checks []store.Check
	changed := false
	for _, e := range edges {
		key := fmt.Sprintf("%s/%v", occKey(e.a), e.start)
		m.mu.Lock()
		done := m.maintLogged[key]
		m.mu.Unlock()
		if done {
			continue
		}
		if checks == nil {
			var err error
			if checks, err = m.st.Checks(ctx); err != nil {
				m.log.Warn("maintenance events", "err", err)
				return changed
			}
		}
		detail := "Ended: " + e.a.Window.Title
		switch {
		case e.start:
			detail = fmt.Sprintf("Started: %s (until %s UTC)", e.a.Window.Title, e.a.End.UTC().Format("2 Jan 15:04"))
		case e.early:
			detail = "Ended early: " + e.a.Window.Title
		}
		for _, s := range scopeStacks(e.a.Window, checks) {
			if m.eventLogged(ctx, s, e.a.Start, detail) {
				continue
			}
			if err := m.st.AddEvent(ctx, store.Event{TS: e.at, HostID: s.HostID, Stack: s.Stack, Kind: EventMaintenance, Actor: store.AuditActorSystem, Detail: detail}); err != nil {
				m.log.Warn("maintenance event", "err", err)
			}
		}
		m.mu.Lock()
		m.maintLogged[key] = true
		m.mu.Unlock()
		changed = true
		m.log.Info("maintenance window", "window", e.a.Window.ID, "title", e.a.Window.Title, "start", e.start)
	}
	m.pruneLogged(now)
	return changed
}

// eventLogged reports whether the same edge was already recorded (before a restart).
func (m *Monitor) eventLogged(ctx context.Context, s store.MaintenanceStack, since time.Time, detail string) bool {
	evs, err := m.st.Events(ctx, s.HostID, s.Stack, since.Add(-time.Second), EventMaintenance)
	if err != nil {
		return false
	}
	for _, e := range evs {
		if e.Detail == detail {
			return true
		}
	}
	return false
}

// pruneLogged forgets edges of occurrences that ended long ago, so the set stays small.
func (m *Monitor) pruneLogged(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.maintLogged) < 256 {
		return
	}
	clear(m.maintLogged) // the DB check keeps it exact after a reset
}

// scopeStacks lists the stacks a window's events belong to: its stacks plus the stacks of
// its checks; for "everything", every stack that has a check.
func scopeStacks(w store.MaintenanceWindow, checks []store.Check) []store.MaintenanceStack {
	seen := map[store.MaintenanceStack]bool{}
	var out []store.MaintenanceStack
	add := func(s store.MaintenanceStack) {
		if s.HostID != "" && s.Stack != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range w.Scope.Stacks {
		add(s)
	}
	for _, c := range checks {
		if w.Scope.All || containsID(w.Scope.Checks, c.ID) {
			add(store.MaintenanceStack{HostID: c.HostID, Stack: stackOf(c)})
		}
	}
	return out
}

func containsID(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// kickFinished re-runs, right now, every check that sits in the maintenance state but
// is no longer covered: a check still failing then raises its (single) down alert at the
// window's end instead of one interval later.
func (m *Monitor) kickFinished(now time.Time) {
	m.mu.Lock()
	var kicks []chan struct{}
	for id, s := range m.states {
		if s.status != StatusMaintenance || s.chk == nil || s.kicked {
			continue
		}
		covered := false
		for _, a := range m.maint.occs {
			if a.Active(now) && CoversCheck(a.Window, *s.chk) {
				covered = true
				break
			}
		}
		if !covered {
			if k := m.kicks[id]; k != nil {
				kicks = append(kicks, k)
				s.kicked = true // one early run is enough; handle() clears it
			}
		}
	}
	m.mu.Unlock()
	for _, k := range kicks {
		select {
		case k <- struct{}{}:
		default:
		}
	}
}
