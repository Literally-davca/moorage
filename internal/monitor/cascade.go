package monitor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"moorage/internal/notify"
	"moorage/internal/store"
)

// Cascade grouping. When a stack goes down while a stack it depends on (directly or up to
// three hops away) is already down, its alert is folded into the message about that root
// cause: one notification names the cause and lists the affected stacks. Every incident is
// still recorded on its own; only notifications are grouped, never dropped.
//
// Batch keys: a cascaded alert uses the root's own key ("stack:<host>/<stack>"), so it joins
// the root's pending batch. A root recovery waits RecoveryWait in a "recover:" batch so the
// dependents that recover with it are listed in the same message.

// Tunables.
const (
	CascadeWindow   = 10 * time.Minute // a root that went down this long ago still counts (if recovered)
	DependentWindow = time.Minute      // batch window of dependents whose root was already announced
	RecoveryWait    = time.Minute      // a root recovery waits this long for dependents to recover with it
)

const recoverPrefix = "recover:"

// DepGraph is the dependency graph between stacks (deps.Service). Both methods return
// stacks nearest first, at most three hops away, with cycle edges left out.
type DepGraph interface {
	Upstream(r store.StackRef) []store.StackRef
	Downstream(r store.StackRef) []store.StackRef
}

// SetDeps turns cascade grouping on.
func (m *Monitor) SetDeps(g DepGraph) {
	m.mu.Lock()
	m.deps = g
	m.mu.Unlock()
}

// cascadeInfo carries what a cascade message says beyond its alerts.
type cascadeInfo struct {
	root      store.StackRef
	earlier   []string // dependents that went down (and alerted) before the root
	stillDown []string // dependents still down after the root recovered (alerted separately)
}

type causeUpdate struct {
	incident int64
	root     string
}

func stackRefOf(c store.Check) (store.StackRef, bool) {
	stack := stackOf(c)
	if c.Kind == "host" || c.HostID == "" || stack == "" {
		return store.StackRef{}, false
	}
	return store.StackRef{HostID: c.HostID, Stack: stack}, true
}

// cascadeRootOfKey returns the root stack of a batch key ("stack:…" or "recover:stack:…").
func cascadeRootOfKey(key string) (store.StackRef, bool) {
	k := strings.TrimPrefix(key, recoverPrefix)
	rest, ok := strings.CutPrefix(k, "stack:")
	if !ok {
		return store.StackRef{}, false
	}
	return store.ParseStackRef(rest)
}

func (m *Monitor) markStackDownLocked(c store.Check, now time.Time) {
	ref, ok := stackRefOf(c)
	if !ok {
		return
	}
	for k, at := range m.stackDownAt {
		if now.Sub(at) > CascadeWindow {
			delete(m.stackDownAt, k)
		}
	}
	m.stackDownAt[ref.Key()] = now
}

// stackDownLocked reports whether a stack counts as a cascade root: one of its checks has
// an open incident (or is down), or an incident of it started within CascadeWindow. It
// returns when the stack went down.
func (m *Monitor) stackDownLocked(r store.StackRef, now time.Time) (time.Time, bool) {
	var first time.Time
	found := false
	for _, s := range m.states {
		if s.chk == nil || (s.incident == 0 && s.status != StatusDown) {
			continue
		}
		if ref, ok := stackRefOf(*s.chk); !ok || ref != r {
			continue
		}
		at := s.downAt
		if at.IsZero() {
			at = s.since
		}
		if !found || at.Before(first) {
			first, found = at, true
		}
	}
	if found {
		return first, true
	}
	if at, ok := m.stackDownAt[r.Key()]; ok && now.Sub(at) <= CascadeWindow {
		return at, true
	}
	return time.Time{}, false
}

// stackOpenLocked reports whether a stack still has an announced, unresolved problem.
func (m *Monitor) stackOpenLocked(r store.StackRef) bool {
	for _, s := range m.states {
		if s.chk == nil || (s.incident == 0 && s.status != StatusDown) {
			continue
		}
		if ref, ok := stackRefOf(*s.chk); ok && ref == r {
			return true
		}
	}
	return false
}

// rootFor picks the cascade root for a check that just went down: among its upstream
// stacks that are down, the one that went down first (on a tie the one further up).
func (m *Monitor) rootFor(c store.Check, now time.Time) string {
	ref, ok := stackRefOf(c)
	m.mu.Lock()
	g := m.deps
	m.mu.Unlock()
	if !ok || g == nil {
		return ""
	}
	ups := g.Upstream(ref)
	if len(ups) == 0 {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	best, bestAt := "", time.Time{}
	for _, u := range ups {
		if u == ref {
			continue
		}
		at, down := m.stackDownLocked(u, now)
		if down && (best == "" || !at.After(bestAt)) {
			best, bestAt = u.Key(), at
		}
	}
	return best
}

// batchKeyLocked picks the batch and its window for an alert.
func (m *Monitor) batchKeyLocked(a alert) (string, time.Duration) {
	if a.check.Kind == "host" {
		return "hosts", BatchWindow
	}
	own := groupKey(a.check)
	if a.root != "" {
		rootKey := "stack:" + a.root
		if !a.up {
			if _, pending := m.batches[rootKey]; pending {
				return rootKey, BatchWindow
			}
			// The root was announced already: collect the dependents a little longer.
			return rootKey, DependentWindow
		}
		if _, waiting := m.batches[recoverPrefix+rootKey]; waiting {
			return recoverPrefix + rootKey, RecoveryWait
		}
		if r, ok := store.ParseStackRef(a.root); ok && m.stackOpenLocked(r) {
			return rootKey, BatchWindow // "back up, the root is still down"
		}
		return own, BatchWindow
	}
	if ref, ok := stackRefOf(a.check); ok && a.up && !m.stackOpenLocked(ref) && m.hasCascadedOpenLocked(ref.Key()) {
		return recoverPrefix + own, RecoveryWait
	}
	return own, BatchWindow
}

func (m *Monitor) hasCascadedOpenLocked(rootKey string) bool {
	for _, s := range m.states {
		if s.root == rootKey && s.incident != 0 {
			return true
		}
	}
	return false
}

// pullDependentsLocked moves pending (not yet sent) down alerts of downstream stacks into
// the root's batch, so a dependent that failed a few seconds before its root still ends up
// in the root's message.
func (m *Monitor) pullDependentsLocked(a alert, key string) []causeUpdate {
	ref, ok := stackRefOf(a.check)
	if !ok || m.deps == nil {
		return nil
	}
	target := m.batches[key]
	var moved []causeUpdate
	for _, d := range m.deps.Downstream(ref) {
		k := "stack:" + d.Key()
		b := m.batches[k]
		if b == nil || k == key {
			continue
		}
		var keep []alert
		for _, x := range b.alerts {
			if x.up || x.root != "" {
				keep = append(keep, x)
				continue
			}
			x.root = ref.Key()
			target.alerts = append(target.alerts, x)
			if s := m.states[x.check.ID]; s != nil && s.incident != 0 && s.root == "" {
				s.root = x.root
				moved = append(moved, causeUpdate{incident: s.incident, root: x.root})
			}
		}
		b.alerts = keep
	}
	return moved
}

func (m *Monitor) saveCauses(us []causeUpdate) {
	for _, u := range us {
		if err := m.st.SetIncidentCause(context.Background(), u.incident, u.root); err != nil {
			m.log.Warn("incident cause", "err", err)
		}
	}
}

// downCascade runs when a batch keyed by a root stack is sent. If the root's own alert is
// in it, dependents that went down shortly before the root (and alerted on their own,
// because the cause wasn't known yet) are named in the message and linked to the root.
func (m *Monitor) downCascade(root store.StackRef, downs []alert) *cascadeInfo {
	info := &cascadeInfo{root: root}
	var rootAt time.Time
	inGroup := map[int64]bool{}
	for _, a := range downs {
		inGroup[a.check.ID] = true
		if a.root == "" && (rootAt.IsZero() || a.at.Before(rootAt)) {
			if ref, ok := stackRefOf(a.check); ok && ref == root {
				rootAt = a.at
			}
		}
	}
	m.mu.Lock()
	g := m.deps
	m.mu.Unlock()
	if rootAt.IsZero() || g == nil {
		return info
	}
	below := map[store.StackRef]bool{}
	for _, d := range g.Downstream(root) {
		below[d] = true
	}
	var updates []causeUpdate
	names := map[string]bool{}
	m.mu.Lock()
	for id, s := range m.states {
		if inGroup[id] || s.chk == nil || s.incident == 0 || s.root != "" {
			continue
		}
		ref, ok := stackRefOf(*s.chk)
		if !ok || !below[ref] || s.downAt.Before(rootAt.Add(-CascadeWindow)) {
			continue
		}
		s.root = root.Key()
		updates = append(updates, causeUpdate{incident: s.incident, root: root.Key()})
		names[ref.Stack] = true
	}
	m.mu.Unlock()
	m.saveCauses(updates)
	info.earlier = sortedKeys(names)
	return info
}

// upCascade runs when recoveries keyed by a root are sent. After the root recovered,
// dependents that are still down alert again on their own ("still down after X
// recovered"), unless they are in maintenance or a deploy.
func (m *Monitor) upCascade(key string, root store.StackRef, ups []alert) *cascadeInfo {
	info := &cascadeInfo{root: root}
	if !strings.HasPrefix(key, recoverPrefix) {
		return info
	}
	now := time.Now()
	type still struct {
		chk   store.Check
		cause string
	}
	var left []still
	m.mu.Lock()
	if !m.stackOpenLocked(root) {
		for _, s := range m.states {
			if s.root != root.Key() || s.incident == 0 || s.chk == nil {
				continue
			}
			s.root = ""
			cause := ""
			if s.last != nil {
				cause = s.last.Msg
			}
			left = append(left, still{chk: *s.chk, cause: cause})
		}
	}
	m.mu.Unlock()
	names := map[string]bool{}
	for _, l := range left {
		if m.quiet(l.chk, now) {
			continue // maintenance and deploy silence win
		}
		names[stackOf(l.chk)] = true
		m.enqueue(alert{check: l.chk, cause: l.cause, at: now, afterRoot: root.Key()})
	}
	info.stillDown = sortedKeys(names)
	return info
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// rootLabel names a root stack in messages: its name, plus the host when it differs.
func (m *Monitor) rootLabel(root store.StackRef, from string) string {
	if root.HostID == from || m.reg == nil {
		return root.Stack
	}
	if _, e, _ := m.reg.Get(context.Background(), root.HostID); e.Name != "" {
		return root.Stack + " on " + e.Name
	}
	return root.Stack
}

// composeCascade builds the message for a group that may hold cascaded alerts. Without
// cascade parts it is exactly compose.
func (m *Monitor) composeCascade(group []alert, info *cascadeInfo) notify.Message {
	var own, dependents []alert
	for _, a := range group {
		if info != nil && a.root != "" && a.root == info.root.Key() {
			dependents = append(dependents, a)
		} else {
			own = append(own, a)
		}
	}
	if info == nil || (len(dependents) == 0 && len(info.earlier) == 0 && len(info.stillDown) == 0) {
		msg := m.compose(group)
		first := group[0]
		switch {
		case first.afterRoot != "":
			m.noteAfterRoot(&msg, first)
		case first.root != "" && first.up:
			// a dependent that recovered after its root's recovery message went out
			if r, ok := store.ParseStackRef(first.root); ok {
				msg.Text = strings.TrimSpace(msg.Text + "\n\nIt was probably affected by the " + m.rootLabel(r, first.check.HostID) + " outage.")
				msg.Data = tagRows(msg.Data, first.root)
			}
		}
		return msg
	}
	rootName := m.rootLabel(info.root, group[0].check.HostID)
	up := group[0].up
	if len(own) == 0 {
		return m.composeDependentsOnly(dependents, info, rootName, up)
	}
	msg := m.compose(own)
	affected := stackNames(dependents)
	var extra []string
	if up {
		if len(dependents) > 0 {
			msg.Title += " — also recovered: " + strings.Join(affected, ", ")
			extra = append(extra, "Recovered with "+rootName+":", m.compose(dependents).Text)
		}
		if len(info.stillDown) > 0 {
			extra = append(extra, fmt.Sprintf("Still down after %s recovered: %s (alerted separately).", rootName, strings.Join(info.stillDown, ", ")))
		}
	} else {
		for _, n := range info.earlier {
			affected = append(affected, n+" (alerted earlier)")
		}
		msg.Title += " — also affected: " + strings.Join(affected, ", ")
		extra = append(extra, "Also affected, probably caused by "+rootName+":")
		if len(dependents) > 0 {
			extra = append(extra, m.compose(dependents).Text)
		}
		if len(info.earlier) > 0 {
			extra = append(extra, fmt.Sprintf("%s went down shortly before %s and alerted on its own earlier.", strings.Join(info.earlier, ", "), rootName))
		}
	}
	if len(extra) > 0 {
		msg.Text = strings.TrimSpace(msg.Text + "\n\n" + strings.Join(extra, "\n"))
	}
	if len(dependents) > 0 {
		rows, _ := msg.Data.([]map[string]any)
		depRows, _ := tagRows(m.compose(dependents).Data, info.root.Key()).([]map[string]any)
		msg.Data = append(rows, depRows...)
	}
	return msg
}

// composeDependentsOnly is the message for dependents whose root was announced earlier
// (downs), or for dependents that recovered on their own.
func (m *Monitor) composeDependentsOnly(dependents []alert, info *cascadeInfo, rootName string, up bool) notify.Message {
	msg := m.compose(dependents)
	names := strings.Join(stackNames(dependents), ", ")
	m.mu.Lock()
	rootAt, rootDown := m.stackDownLocked(info.root, time.Now())
	rootOpen := m.stackOpenLocked(info.root)
	m.mu.Unlock()
	if up {
		if rootOpen {
			msg.Title += " (" + rootName + " is still down)"
		}
		msg.Text = strings.TrimSpace(msg.Text + "\n\nIt was probably affected by the " + rootName + " outage.")
	} else {
		msg.Title = "Also affected by the " + rootName + " outage: " + names
		lead := "Probably caused by " + rootName + "."
		if rootDown {
			lead = fmt.Sprintf("Probably caused by %s, down since %s.", rootName, rootAt.Local().Format("15:04"))
		}
		msg.Text = strings.TrimSpace(lead + "\n" + msg.Text)
	}
	msg.Data = tagRows(msg.Data, info.root.Key())
	return msg
}

// noteAfterRoot marks the re-alert of a dependent that stayed down after its root recovered.
func (m *Monitor) noteAfterRoot(msg *notify.Message, a alert) {
	r, ok := store.ParseStackRef(a.afterRoot)
	if !ok {
		return
	}
	name := m.rootLabel(r, a.check.HostID)
	msg.Title += " — still down after " + name + " recovered"
	msg.Text = strings.TrimSpace(msg.Text + "\n\nIt was folded into the " + name + " outage, but " + name + " is back and this is still down.")
}

func stackNames(as []alert) []string {
	seen := map[string]bool{}
	for _, a := range as {
		if s := stackOf(a.check); s != "" {
			seen[s] = true
		} else {
			seen[a.check.Name] = true
		}
	}
	return sortedKeys(seen)
}

// tagRows marks structured webhook rows with the root cause they were folded into.
func tagRows(data any, root string) any {
	rows, _ := data.([]map[string]any)
	for _, r := range rows {
		r["rootCause"] = root
	}
	return data
}

// StackCheckStatus sums up the checks of every stack ("host/stack" key) for the graph view:
// down beats pending beats maintenance beats silenced beats up. Stacks without checks (or
// with only paused or unknown ones) are missing from the map.
func (m *Monitor) StackCheckStatus() map[string]string {
	rank := map[string]int{StatusUp: 1, StatusSilenced: 2, StatusMaintenance: 3, StatusPending: 4, StatusDegraded: 5, StatusDown: 6}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for _, s := range m.states {
		if s.chk == nil || rank[s.status] == 0 {
			continue
		}
		ref, ok := stackRefOf(*s.chk)
		if !ok {
			continue
		}
		if k := ref.Key(); rank[s.status] > rank[out[k]] {
			out[k] = s.status
		}
	}
	return out
}
