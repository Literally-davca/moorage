package metrics

import (
	"context"
	"sort"
	"sync"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/monitor"
	"moorage/internal/store"
)

// Defaults for the background snapshot of container data.
const (
	DefaultRefreshEvery = 30 * time.Second
	DefaultIdleStop     = 5 * time.Minute
	DefaultHostTimeout  = 10 * time.Second
	DefaultFirstWait    = 3 * time.Second
)

// containerStates are Docker's container states. Every stack gets a series per state
// (0 when none), so dashboards don't show gaps and the label set stays fixed.
var containerStates = []string{"created", "running", "paused", "restarting", "removing", "exited", "dead"}

// checkStates are the states a check can be in (monitor.Status*).
var checkStates = []string{
	monitor.StatusUp, monitor.StatusDown, monitor.StatusPending, monitor.StatusMaintenance,
	monitor.StatusSilenced, monitor.StatusPaused, monitor.StatusUnknown,
}

// Collector renders the metrics. Check states, host connections and alert counts come
// from memory on each scrape. Container data and the uptime ratios cost Docker calls or
// a scan of 30 days of results, so they come from a snapshot that a background loop
// refreshes at most every RefreshEvery while scrapes keep coming, and stops refreshing
// IdleStop after the last scrape. A scrape never waits on a host, except the first one
// after an idle period, and only up to FirstWait.
type Collector struct {
	st      *store.Store
	reg     *host.Registry // nil in tests without hosts
	mon     *monitor.Monitor
	version string

	RefreshEvery time.Duration
	IdleStop     time.Duration
	HostTimeout  time.Duration
	FirstWait    time.Duration
	Now          func() time.Time

	mu         sync.Mutex
	snap       *snapshot
	lastScrape time.Time
	running    bool
	refreshed  chan struct{} // closed and replaced after every refresh
	ctx        context.Context
}

type snapshot struct {
	at     time.Time
	hosts  map[string][]stackSnap // host ID -> stacks, only hosts that answered in time
	up24h  map[int64][2]int       // check ID -> (up, total)
	up30d  map[int64][2]int
	images []imageSnap
}

type stackSnap struct {
	name       string
	containers []containerSnap
}

type containerSnap struct {
	name, service, state string
}

type imageSnap struct {
	hostID, stack, service string
	update                 bool
}

// New creates a collector. reg and mon may be nil.
func New(st *store.Store, reg *host.Registry, mon *monitor.Monitor, version string) *Collector {
	return &Collector{
		st: st, reg: reg, mon: mon, version: version,
		RefreshEvery: DefaultRefreshEvery, IdleStop: DefaultIdleStop,
		HostTimeout: DefaultHostTimeout, FirstWait: DefaultFirstWait,
		refreshed: make(chan struct{}),
		ctx:       context.Background(),
	}
}

// SetContext stops the background refresh when ctx ends (hub shutdown).
func (c *Collector) SetContext(ctx context.Context) {
	c.mu.Lock()
	c.ctx = ctx
	c.mu.Unlock()
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// current records a scrape, makes sure the refresh loop runs, and returns the snapshot.
// A missing or stale snapshot (after an idle period) is waited for, up to FirstWait.
func (c *Collector) current() *snapshot {
	c.mu.Lock()
	c.lastScrape = c.now()
	if !c.running {
		c.running = true
		go c.loop(c.ctx)
	}
	snap, wait := c.snap, c.refreshed
	c.mu.Unlock()
	if snap != nil && c.now().Sub(snap.at) <= 2*c.RefreshEvery {
		return snap
	}
	t := time.NewTimer(c.FirstWait)
	defer t.Stop()
	select {
	case <-wait:
	case <-t.C:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snap
}

func (c *Collector) loop(ctx context.Context) {
	for {
		c.refresh(ctx)
		t := time.NewTimer(c.RefreshEvery)
		select {
		case <-ctx.Done():
			t.Stop()
			c.mu.Lock()
			c.running = false
			c.mu.Unlock()
			return
		case <-t.C:
		}
		c.mu.Lock()
		if c.now().Sub(c.lastScrape) >= c.IdleStop {
			c.running = false
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
	}
}

// refresh builds a new snapshot and publishes it.
func (c *Collector) refresh(ctx context.Context) {
	s := &snapshot{hosts: c.hostSnapshots(ctx)}
	if c.st != nil {
		c.fillStore(ctx, s)
	}
	s.at = c.now()
	c.mu.Lock()
	c.snap = s
	close(c.refreshed)
	c.refreshed = make(chan struct{})
	c.mu.Unlock()
}

// fillStore reads the uptime ratios and image check results from the database.
func (c *Collector) fillStore(ctx context.Context, s *snapshot) {
	if checks, err := c.st.Checks(ctx); err == nil && len(checks) > 0 {
		ids := make([]int64, len(checks))
		for i, ch := range checks {
			ids[i] = ch.ID
		}
		now := c.now()
		s.up24h, _ = c.st.CheckUptimesSince(ctx, ids, now.Add(-24*time.Hour))
		s.up30d, _ = c.st.CheckUptimesSince(ctx, ids, now.Add(-30*24*time.Hour))
	}
	if rows, err := c.st.ImageChecks(ctx, "", ""); err == nil {
		s.images = make([]imageSnap, 0, len(rows))
		for _, r := range rows {
			s.images = append(s.images, imageSnap{
				hostID: r.HostID, stack: r.Stack, service: r.Service,
				update: r.Status == engine.ImageUpdateAvailable || r.Status == engine.ImagePulledNotApplied,
			})
		}
	}
}

type hostResult struct {
	id     string
	stacks []stackSnap
	ok     bool
}

// hostSnapshots asks every connected host for its stacks and containers in parallel.
// Each host has HostTimeout; a host that doesn't answer in time is left out, and the
// refresh doesn't wait for it any longer even if it ignores its context.
func (c *Collector) hostSnapshots(ctx context.Context) map[string][]stackSnap {
	out := map[string][]stackSnap{}
	if c.reg == nil {
		return out
	}
	entries, err := c.reg.List(ctx)
	if err != nil {
		return out
	}
	results := make(chan hostResult, len(entries))
	n := 0
	for _, e := range entries {
		if !e.Connected {
			continue
		}
		n++
		go func(id string) {
			stacks, ok := c.hostStacks(ctx, id)
			results <- hostResult{id: id, stacks: stacks, ok: ok}
		}(e.ID)
	}
	deadline := time.NewTimer(c.HostTimeout + time.Second)
	defer deadline.Stop()
	for ; n > 0; n-- {
		select {
		case r := <-results:
			if r.ok {
				out[r.id] = r.stacks
			}
		case <-deadline.C:
			return out
		case <-ctx.Done():
			return out
		}
	}
	return out
}

func (c *Collector) hostStacks(ctx context.Context, id string) ([]stackSnap, bool) {
	ctx, cancel := context.WithTimeout(ctx, c.HostTimeout)
	defer cancel()
	h, _, err := c.reg.Get(ctx, id)
	if err != nil {
		return nil, false
	}
	list, err := h.ListStacks(ctx)
	if err != nil {
		// Docker unreachable: the list holds only folder names, no container truth.
		return nil, false
	}
	out := make([]stackSnap, 0, len(list))
	for _, s := range list {
		ss := stackSnap{name: s.Name}
		if s.Total > 0 {
			st, err := h.GetStack(ctx, s.Name)
			if ctx.Err() != nil {
				return nil, false
			}
			if err != nil {
				continue // removed between the two calls
			}
			for _, ct := range st.Containers {
				ss.containers = append(ss.containers, containerSnap{name: ct.Name, service: ct.Service, state: ct.State})
			}
		}
		out = append(out, ss)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, true
}
