package monitor

import (
	"context"
	"sync"
	"time"

	"moorage/internal/engine"
	"moorage/internal/store"
)

// ContainerEventRetention is how long recorded container events are kept.
const ContainerEventRetention = 30 * 24 * time.Hour

// containerPoll is how often the recorder looks at the stacks of checks.
const containerPoll = 30 * time.Second

// containerSnap is what the recorder remembers about one service between polls.
type containerSnap struct {
	id, state, health string
	exit              *int
}

// containerRecorder turns changes in the container list of checked stacks into
// container events (stopped, restarting, unhealthy) for the incident timeline (M18A) and
// the Problems card (M18F). It polls the hosts' stack lists, so it works for every host
// and agent version; the first look at a stack only sets the baseline.
type containerRecorder struct {
	mu   sync.Mutex
	seen map[string]map[string]containerSnap // "host/stack" -> service -> last snapshot
}

func newContainerRecorder() *containerRecorder {
	return &containerRecorder{seen: map[string]map[string]containerSnap{}}
}

// diffContainers returns the events between two looks at a stack's containers.
func diffContainers(prev map[string]containerSnap, cur []engine.Container, hostID, stack string, at time.Time) (map[string]containerSnap, []store.ContainerEvent) {
	next := map[string]containerSnap{}
	var events []store.ContainerEvent
	for _, c := range cur {
		key := c.Service
		if key == "" {
			key = c.Name
		}
		snap := containerSnap{id: c.ID, state: c.State, health: c.Health, exit: c.ExitCode}
		next[key] = snap
		if prev == nil {
			continue // first look: baseline only
		}
		old, known := prev[key]
		ev := store.ContainerEvent{HostID: hostID, Stack: stack, Service: key, ContainerID: c.ID, Time: at}
		switch {
		case c.State == "restarting" && (!known || old.state != "restarting"):
			ev.Type, ev.Detail = store.ContainerRestart, c.Status
			events = append(events, ev)
		case (c.State == "exited" || c.State == "dead") && (!known || old.state != c.State || old.id != c.ID):
			ev.Type, ev.Detail, ev.ExitCode = store.ContainerDie, c.Status, c.ExitCode
			events = append(events, ev)
		}
		if c.State == "running" && c.Health != old.health && (c.Health == "unhealthy" || (c.Health == "healthy" && old.health == "unhealthy")) {
			ev.Type, ev.Detail, ev.ExitCode = store.ContainerUnhealthy, c.Health, nil
			events = append(events, ev)
		}
	}
	return next, events
}

// recordContainers polls the stacks that checks watch until ctx ends.
func (m *Monitor) recordContainers(ctx context.Context) {
	if m.reg == nil {
		return
	}
	t := time.NewTicker(containerPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, hs := range m.checkedStacks() {
			m.recordStack(ctx, hs[0], hs[1])
		}
	}
}

// checkedStacks returns the distinct [host, stack] pairs of the checks.
func (m *Monitor) checkedStacks() [][2]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[[2]string]bool{}
	var out [][2]string
	for _, s := range m.states {
		if s.chk == nil || s.chk.HostID == "" {
			continue
		}
		stack := stackOf(*s.chk)
		if stack == "" {
			continue
		}
		k := [2]string{s.chk.HostID, stack}
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func (m *Monitor) recordStack(ctx context.Context, hostID, stack string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	h, _, err := m.reg.Get(ctx, hostID)
	if err != nil {
		return // host offline: no news, keep the last snapshot
	}
	st, err := h.GetStack(ctx, stack)
	if err != nil {
		return
	}
	key := hostID + "/" + stack
	m.containers.mu.Lock()
	next, events := diffContainers(m.containers.seen[key], st.Containers, hostID, stack, time.Now())
	m.containers.seen[key] = next
	m.containers.mu.Unlock()
	for _, e := range events {
		if err := m.st.AddContainerEvent(context.Background(), e); err != nil {
			m.log.Warn("container event", "stack", stack, "err", err)
		}
	}
}
