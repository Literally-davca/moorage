package api

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/monitor"
	"moorage/internal/store"
)

// Automatic restart of dependent stacks. An edge "A depends on B" with auto_restart:
//   - on_recover: once B comes back from down to up and stays up (healthchecks included)
//     for autoRestartStable, A is restarted;
//   - on_recover_and_deploy: also after B was deployed or updated (a shared VPN or proxy).
//
// Guards against restart storms: a cooldown per edge, layers in topological order (direct
// dependents first, then theirs, at most autoRestartDepth deep, stacks in a loop never),
// and never a stack that is stopped on purpose, outside the stacks folder, in a maintenance
// window or on a read-only host. A restart silences the stack's checks like any restart and
// is written to the Logbook and the audit log.

const (
	autoRestartStable   = 20 * time.Second
	autoRestartCooldown = 15 * time.Minute
	autoRestartDepth    = 3
	autoRestartTick     = 5 * time.Second
	autoRestartDeployTTL = 15 * time.Minute
)

type autoRestarter struct {
	s *Server

	mu       sync.Mutex
	last     map[string]string    // upstream key -> last container status
	upSince  map[string]time.Time // upstream key -> up since (after it was down)
	deploys  map[string]time.Time // upstream key -> last deploy or update
	deployUp map[string]time.Time // upstream key -> up since the deploy
	cooldown map[int64]time.Time  // edge id -> last auto-restart
	warned   map[int64]time.Time  // edge id -> last cooldown warning in the Logbook
	now      func() time.Time
}

func newAutoRestarter(s *Server) *autoRestarter {
	return &autoRestarter{s: s, last: map[string]string{}, upSince: map[string]time.Time{}, deploys: map[string]time.Time{},
		deployUp: map[string]time.Time{}, cooldown: map[int64]time.Time{}, warned: map[int64]time.Time{}, now: time.Now}
}

// StackEvent notes deploys and updates of upstream stacks (on_recover_and_deploy).
func (a *autoRestarter) StackEvent(hostID, stack, kind string) {
	if kind != "deploy" && kind != "update" {
		return
	}
	a.mu.Lock()
	a.deploys[hostID+"/"+stack] = a.now()
	delete(a.deployUp, hostID+"/"+stack)
	a.mu.Unlock()
}

func (a *autoRestarter) run(ctx context.Context) {
	t := time.NewTicker(autoRestartTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.tick(ctx)
		}
	}
}

func activeEdges(edges []store.StackDep) []store.StackDep {
	var out []store.StackDep
	for _, e := range edges {
		if !e.Ignored && e.AutoRestart != "" && e.AutoRestart != store.AutoRestartOff {
			out = append(out, e)
		}
	}
	return out
}

// statuses returns the container status of every stack on the given hosts.
func (a *autoRestarter) statuses(ctx context.Context, hosts map[string]bool) map[string]engine.StackSummary {
	out := map[string]engine.StackSummary{}
	if a.s.reg == nil {
		return out
	}
	for hid := range hosts {
		h, _, err := a.s.reg.Get(ctx, hid)
		if err != nil {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		list, _ := h.ListStacks(cctx)
		cancel()
		for _, st := range list {
			out[hid+"/"+st.Name] = st
		}
	}
	return out
}

func (a *autoRestarter) tick(ctx context.Context) {
	edges, err := a.s.store.StackDeps(ctx)
	if err != nil {
		return
	}
	active := activeEdges(edges)
	if len(active) == 0 {
		return
	}
	hosts := map[string]bool{}
	ups := map[string]store.StackRef{}
	for _, e := range active {
		hosts[e.To.HostID], hosts[e.From.HostID] = true, true
		ups[e.To.Key()] = e.To
	}
	st := a.statuses(ctx, hosts)
	now := a.now()
	type fire struct {
		ref    store.StackRef
		deploy bool
	}
	var fires []fire
	a.mu.Lock()
	for key, ref := range ups {
		sum, known := st[key]
		cur := sum.Status
		if !known {
			continue // host offline: no change
		}
		prev := a.last[key]
		a.last[key] = cur
		if cur != engine.StatusUp {
			delete(a.upSince, key)
			delete(a.deployUp, key)
			continue
		}
		if prev == engine.StatusDown || prev == engine.StatusDegraded {
			a.upSince[key] = now
		}
		if t, ok := a.upSince[key]; ok && now.Sub(t) >= autoRestartStable {
			delete(a.upSince, key)
			fires = append(fires, fire{ref, false})
		}
		if d, ok := a.deploys[key]; ok {
			if now.Sub(d) > autoRestartDeployTTL {
				delete(a.deploys, key)
				delete(a.deployUp, key)
			} else {
				if _, ok := a.deployUp[key]; !ok {
					a.deployUp[key] = now
				}
				if now.Sub(a.deployUp[key]) >= autoRestartStable && now.Sub(d) >= autoRestartStable {
					delete(a.deploys, key)
					delete(a.deployUp, key)
					fires = append(fires, fire{ref, true})
				}
			}
		}
	}
	a.mu.Unlock()
	for _, f := range fires {
		a.cascade(ctx, f.ref, f.deploy, active, st)
	}
}

// cascade restarts the dependents of root layer by layer.
func (a *autoRestarter) cascade(ctx context.Context, root store.StackRef, deployed bool, edges []store.StackDep, st map[string]engine.StackSummary) {
	why := "recovered"
	if deployed {
		why = "was deployed"
	}
	g := a.s.deps.Graph()
	done := map[string]bool{root.Key(): true}
	layer := []store.StackRef{root}
	for depth := 1; depth <= autoRestartDepth && len(layer) > 0; depth++ {
		var next []store.StackRef
		var ops []func()
		for _, up := range layer {
			for _, e := range edges {
				if e.To != up || done[e.From.Key()] {
					continue
				}
				if deployed && e.AutoRestart != store.AutoRestartAndDeploy {
					continue
				}
				if g != nil && g.InCycle(e.From) {
					continue
				}
				done[e.From.Key()] = true
				if wait, ok := a.restart(ctx, e, root, why, st); ok {
					next = append(next, e.From)
					if wait != nil {
						ops = append(ops, wait)
					}
				}
			}
		}
		for _, w := range ops {
			w() // the next layer waits for this one's restarts to finish
		}
		layer = next
	}
}

// restart restarts the dependent of one edge, if every guard allows it. It returns a
// function that waits for the restart to finish.
func (a *autoRestarter) restart(ctx context.Context, e store.StackDep, root store.StackRef, why string, st map[string]engine.StackSummary) (func(), bool) {
	now := a.now()
	ref := e.From
	a.mu.Lock()
	last := a.cooldown[e.ID]
	if !last.IsZero() && now.Sub(last) < autoRestartCooldown {
		warn := now.Sub(a.warned[e.ID]) > autoRestartCooldown
		if warn {
			a.warned[e.ID] = now
		}
		a.mu.Unlock()
		if warn && a.s.mon != nil {
			a.s.mon.StackEvent(ref.HostID, ref.Stack, "warning", "moorage",
				fmt.Sprintf("Auto-restart skipped: %s %s again within %s (cooldown)", root.Stack, why, autoRestartCooldown))
		}
		return nil, false
	}
	a.mu.Unlock()
	sum, ok := st[ref.Key()]
	if !ok || !sum.Managed || sum.Status == engine.StatusStopped || sum.Status == "inactive" || sum.Self || sum.Busy {
		return nil, false
	}
	if a.s.mon != nil {
		for _, w := range a.s.mon.ActiveWindows(now) {
			if monitor.CoversStack(w.Window, ref.HostID, ref.Stack) {
				return nil, false
			}
		}
	}
	h, he, err := a.s.reg.Get(ctx, ref.HostID)
	if err != nil || he.ReadOnly {
		return nil, false
	}
	op, err := h.RunAction(ctx, ref.Stack, "restart", "auto-restart")
	if err != nil {
		a.s.log.Warn("auto-restart", "stack", ref.Key(), "err", err)
		return nil, false
	}
	a.mu.Lock()
	a.cooldown[e.ID] = now
	a.mu.Unlock()
	detail := fmt.Sprintf("Auto-restarted after %s %s", root.Stack, why)
	if root.HostID != ref.HostID {
		detail = fmt.Sprintf("Auto-restarted after %s on another host %s", root.Stack, why)
	}
	if a.s.mon != nil {
		a.s.mon.StackEvent(ref.HostID, ref.Stack, "restart", "auto-restart", detail)
	}
	_ = a.s.store.Audit(ctx, store.AuditEntry{At: now, Actor: store.AuditActorSystem, Action: "stack.auto_restart",
		HostID: ref.HostID, Stack: ref.Stack, Target: strings.TrimSpace(detail), Outcome: "ok"})
	a.s.log.Info("auto-restart", "stack", ref.Key(), "after", root.Key(), "why", why)
	return func() {
		wctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		_ = h.FollowOp(wctx, op.ID, func(host.OpEvent) error { return nil })
	}, true
}
