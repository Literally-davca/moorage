package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"moorage/internal/store"
)

// Multi-target checks: one HTTP check can watch up to MaxTargets URLs and one stack check
// can watch several stacks, also on different hosts. The check is up when every active
// target is up, "partially down" (StatusDegraded) when some fail, and down when all fail.
// Targets in a deploy silence or a maintenance window sit the run out, so deploying one
// stack of a group never takes the whole check down.

// StatusDegraded is a multi-target check where some, but not all, active targets fail.
const StatusDegraded = "degraded"

// MaxTargets bounds the URLs or stacks of one check.
const MaxTargets = 20

// targetParallel is how many targets of one check are probed at the same time.
const targetParallel = 5

// StackTarget is one stack of a multi-stack check.
type StackTarget struct {
	HostID string `json:"hostId"`
	Stack  string `json:"stack"`
}

// TargetResult is one target's answer in the last run of a multi-target check.
type TargetResult struct {
	Target    string `json:"target"` // URL, or hostId/stack
	Label     string `json:"label"`
	Status    string `json:"status"` // up, down, maintenance, silenced, paused, unknown
	LatencyMS int    `json:"latencyMs"`
	Msg       string `json:"msg"`
}

// MultiTarget reports whether c watches more than one URL or stack.
func MultiTarget(c store.Check) bool {
	st := ParseSettings(c.Settings)
	switch c.Kind {
	case KindHTTP:
		return len(st.URLs) > 1
	case KindStack:
		return len(st.Stacks) > 1
	}
	return false
}

// subChecks splits c into one check per target. Each keeps the ID (so a window that lists
// the check covers every target), the timeout, locations and settings of c.
func subChecks(c store.Check) []store.Check {
	st := ParseSettings(c.Settings)
	urls, stacks := st.URLs, st.Stacks
	st.URLs, st.Stacks = nil, nil
	b, _ := json.Marshal(st)
	var out []store.Check
	switch c.Kind {
	case KindHTTP:
		for _, u := range urls {
			sc := c
			sc.Target, sc.Settings = u, string(b)
			out = append(out, sc)
		}
	case KindStack:
		for _, t := range stacks {
			sc := c
			sc.Target, sc.HostID, sc.Stack, sc.Settings = t.Stack, t.HostID, t.Stack, string(b)
			out = append(out, sc)
		}
	}
	return out
}

// targetLabel is the short name of a target in messages: the host and path of a URL,
// the stack name (with the host when the check spans several hosts).
func (m *Monitor) targetLabel(c store.Check, multiHost bool) string {
	if c.Kind == KindHTTP {
		if u, err := url.Parse(c.Target); err == nil && u.Host != "" {
			p := strings.TrimRight(u.Path, "/")
			return u.Host + p
		}
		return c.Target
	}
	if multiHost && m.reg != nil {
		if _, e, _ := m.reg.Get(context.Background(), c.HostID); e.Name != "" {
			return e.Name + "/" + c.Target
		}
	}
	return c.Target
}

// probeTargets runs every target of a multi-target check (at most targetParallel at a
// time) and combines them into one outcome.
func (m *Monitor) probeTargets(ctx context.Context, c store.Check, now time.Time) Outcome {
	subs := subChecks(c)
	hosts := map[string]bool{}
	for _, sc := range subs {
		hosts[sc.HostID] = true
	}
	results := make([]TargetResult, len(subs))
	sem := make(chan struct{}, targetParallel)
	var wg sync.WaitGroup
	for i, sc := range subs {
		key := sc.Target
		if sc.Kind == KindStack {
			key = sc.HostID + "/" + sc.Stack
		}
		results[i] = TargetResult{Target: key, Label: m.targetLabel(sc, len(hosts) > 1)}
		// A target in a maintenance window or a deploy silence sits this run out.
		if m.inMaintenance(sc, now) {
			results[i].Status, results[i].Msg = StatusMaintenance, "in a maintenance window"
			continue
		}
		if sc.Kind == KindStack && m.silenced(sc, now) {
			results[i].Status, results[i].Msg = StatusSilenced, "just deployed"
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i].Status, results[i].Msg = StatusUnknown, "cancelled"
				return
			}
			defer func() { <-sem }()
			var o Outcome
			if MultiLocation(sc) {
				o = m.probeLocations(ctx, sc)
			} else {
				o = Probe(ctx, sc, m.reg)
			}
			r := &results[i]
			r.LatencyMS, r.Msg = int(o.Latency.Milliseconds()), o.Msg
			switch {
			case o.Unknown:
				r.Status = StatusUnknown
			case o.Paused:
				r.Status = StatusPaused
			case o.OK:
				r.Status = StatusUp
			default:
				r.Status = StatusDown
			}
		}()
	}
	wg.Wait()
	return combineTargets(results)
}

// combineTargets turns per-target results into one outcome: up when every active target
// is up, degraded when some fail, down when all fail. Quiet targets (maintenance,
// silenced), stopped stacks and targets nobody could probe don't count; when none is
// left the outcome carries the quiet status instead.
func combineTargets(results []TargetResult) Outcome {
	out := Outcome{Targets: results}
	var ok, total, maxLat int
	var bad []string
	quiet := ""
	paused := 0
	for _, r := range results {
		switch r.Status {
		case StatusUp, StatusDown:
			total++
			maxLat = max(maxLat, r.LatencyMS)
			if r.Status == StatusUp {
				ok++
			} else {
				bad = append(bad, fmt.Sprintf("%s (%s)", r.Label, clip(r.Msg, 80)))
			}
		case StatusMaintenance:
			quiet = StatusMaintenance
		case StatusSilenced:
			if quiet == "" {
				quiet = StatusSilenced
			}
		case StatusPaused:
			paused++
		}
	}
	out.Latency = time.Duration(maxLat) * time.Millisecond
	switch {
	case total == 0 && quiet != "":
		out.Quiet = quiet
		out.Msg = fmt.Sprintf("all %d targets are in maintenance or were just deployed", len(results))
	case total == 0 && paused > 0:
		out.Paused = true
		out.Msg = "every stack is stopped on purpose"
	case total == 0:
		out.Unknown = true
		out.Msg = "no target could be checked"
	case ok == total:
		out.OK = true
		out.Msg = fmt.Sprintf("%d/%d up", ok, total)
	case ok == 0:
		out.Msg = clip(fmt.Sprintf("%d/%d down: %s", total, total, strings.Join(bad, ", ")), 400)
	default:
		out.Degraded = true
		out.Msg = clip(fmt.Sprintf("%d/%d down: %s", total-ok, total, strings.Join(bad, ", ")), 400)
	}
	if skipped := len(results) - total - paused; skipped > 0 && total > 0 {
		out.Msg += fmt.Sprintf(" (%d skipped: maintenance or deploy)", skipped)
	}
	return out
}
