package deps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/store"
)

// Tunables.
const (
	DetectEvery   = 10 * time.Minute // full re-detection of every connected host
	detectTick    = time.Minute      // how often the loop looks for hosts that are due
	detectTimeout = time.Minute
)

// Detection is the outcome of the last detection run on a host (in memory only: it runs
// again within a minute of a hub start).
type Detection struct {
	At          time.Time `json:"at,omitzero"`
	Running     bool      `json:"running,omitempty"`
	Unsupported bool      `json:"unsupported,omitempty"` // the agent is too old to report compose facts
	Error       string    `json:"error,omitempty"`
	Stacks      int       `json:"stacks"`
	Broken      []string  `json:"broken,omitempty"` // stacks whose compose file didn't load (edges kept)
}

// Service detects edges, stores them, and serves the graph snapshot.
type Service struct {
	st  *store.Store
	reg *host.Registry
	log *slog.Logger

	ctx   context.Context
	graph atomic.Pointer[Graph]

	mu     sync.Mutex
	runs   map[string]*run // hosts with a detection in progress
	status map[string]Detection
}

type run struct{ again bool }

// New creates the service; call Start to load the graph and run detection.
func New(st *store.Store, reg *host.Registry, log *slog.Logger) *Service {
	s := &Service{st: st, reg: reg, log: log, ctx: context.Background(), runs: map[string]*run{}, status: map[string]Detection{}}
	s.graph.Store(Build(nil))
	return s
}

// Start loads the stored edges and detects edges on every connected host now and then
// every DetectEvery.
func (s *Service) Start(ctx context.Context) error {
	s.ctx = ctx
	if err := s.Reload(ctx); err != nil {
		return err
	}
	go s.loop(ctx)
	return nil
}

// Reload rebuilds the graph snapshot from the store. Call it after any edge change.
func (s *Service) Reload(ctx context.Context) error {
	edges, err := s.st.StackDeps(ctx)
	if err != nil {
		return fmt.Errorf("load dependency graph: %w", err)
	}
	s.graph.Store(Build(edges))
	return nil
}

// Graph returns the current snapshot.
func (s *Service) Graph() *Graph { return s.graph.Load() }

// Upstream implements monitor.DepGraph.
func (s *Service) Upstream(r store.StackRef) []store.StackRef {
	return s.Graph().Upstream(r, MaxDepth)
}

// Downstream implements monitor.DepGraph.
func (s *Service) Downstream(r store.StackRef) []store.StackRef {
	return s.Graph().Downstream(r, MaxDepth)
}

// Status returns the last detection outcome of a host.
func (s *Service) Status(hostID string) Detection {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.status[hostID]
	d.Running = s.runs[hostID] != nil
	return d
}

// StackEvent reacts to a stack change: a deleted stack loses its edges, a saved or
// deployed one is detected again.
func (s *Service) StackEvent(hostID, stack, kind string) {
	switch kind {
	case "delete":
		if err := s.st.DeleteDepsOfStack(s.ctx, store.StackRef{HostID: hostID, Stack: stack}); err != nil {
			s.log.Warn("remove edges of deleted stack", "stack", stack, "err", err)
		}
		_ = s.Reload(s.ctx)
		s.Trigger(hostID)
	case "create", "edit", "deploy", "update", "restore", "rollback":
		s.Trigger(hostID)
	}
}

// HostRemoved drops every edge of an unpaired host.
func (s *Service) HostRemoved(hostID string) {
	if err := s.st.DeleteDepsOfHost(s.ctx, hostID); err != nil {
		s.log.Warn("remove edges of host", "host", hostID, "err", err)
	}
	s.mu.Lock()
	delete(s.status, hostID)
	s.mu.Unlock()
	_ = s.Reload(s.ctx)
}

// Trigger starts a detection run on a host in the background. While one runs, further
// triggers collapse into a single follow-up run: never more than one at a time per host.
func (s *Service) Trigger(hostID string) {
	s.mu.Lock()
	if r := s.runs[hostID]; r != nil {
		r.again = true
		s.mu.Unlock()
		return
	}
	s.runs[hostID] = &run{}
	s.mu.Unlock()
	go s.runLoop(hostID)
}

func (s *Service) runLoop(hostID string) {
	for {
		s.Detect(s.ctx, hostID)
		s.mu.Lock()
		r := s.runs[hostID]
		if r == nil || !r.again || s.ctx.Err() != nil {
			delete(s.runs, hostID)
			s.mu.Unlock()
			return
		}
		r.again = false
		s.mu.Unlock()
	}
}

func (s *Service) loop(ctx context.Context) {
	t := time.NewTicker(detectTick)
	defer t.Stop()
	// Agents connect a few seconds after the hub starts; the first pass waits a little.
	first := time.NewTimer(15 * time.Second)
	defer first.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-first.C:
		case <-t.C:
		}
		s.detectDue(ctx, time.Now())
	}
}

// detectDue triggers every connected host whose last run is older than DetectEvery or
// failed (offline, error).
func (s *Service) detectDue(ctx context.Context, now time.Time) {
	hosts, err := s.reg.List(ctx)
	if err != nil {
		return
	}
	for _, h := range hosts {
		if !h.Connected {
			continue
		}
		d := s.Status(h.ID)
		if d.At.IsZero() || d.Error != "" || now.Sub(d.At) >= DetectEvery {
			s.Trigger(h.ID)
		}
	}
}

// Detect runs one detection on a host synchronously (Trigger is the usual entry point).
func (s *Service) Detect(ctx context.Context, hostID string) Detection {
	d := s.detect(ctx, hostID)
	d.At = time.Now()
	s.mu.Lock()
	s.status[hostID] = d
	s.mu.Unlock()
	return d
}

func (s *Service) detect(ctx context.Context, hostID string) Detection {
	h, _, err := s.reg.Get(ctx, hostID)
	if err != nil {
		return Detection{Error: errText(err)}
	}
	src, ok := h.(host.DepFactsSource)
	if !ok {
		return Detection{Unsupported: true}
	}
	cctx, cancel := context.WithTimeout(ctx, detectTimeout)
	defer cancel()
	facts, err := src.DepFacts(cctx)
	if host.IsUnsupported(err) {
		return Detection{Unsupported: true}
	}
	if err != nil {
		return Detection{Error: errText(err)}
	}
	found, broken := s.withBrokenKept(ctx, hostID, facts)
	added, removed, err := s.st.ReplaceDetected(ctx, hostID, found)
	if err != nil {
		s.log.Warn("store detected edges", "host", hostID, "err", err)
		return Detection{Error: "couldn't store the result"}
	}
	if added > 0 || removed > 0 {
		s.log.Info("stack dependencies detected", "host", hostID, "added", added, "removed", removed)
		if err := s.Reload(ctx); err != nil {
			s.log.Warn("reload dependency graph", "err", err)
		}
	}
	return Detection{Stacks: len(facts.Stacks), Broken: broken}
}

// withBrokenKept computes edges and keeps the stored detected edges of stacks whose
// compose file failed to load, so a half-finished edit doesn't drop and re-add them.
func (s *Service) withBrokenKept(ctx context.Context, hostID string, facts engine.DepFacts) ([]store.StackDep, []string) {
	found := Compute(hostID, facts)
	var broken []string
	isBroken := map[string]bool{}
	for _, sf := range facts.Stacks {
		if sf.Error != "" {
			broken = append(broken, sf.Name)
			isBroken[sf.Name] = true
		}
	}
	if len(broken) == 0 {
		return found, nil
	}
	have, err := s.st.StackDepsOfHost(ctx, hostID)
	if err != nil {
		return found, broken
	}
	for _, d := range have {
		if d.Source == store.DepDetected && d.From.HostID == hostID && d.To.HostID == hostID &&
			(isBroken[d.From.Stack] || isBroken[d.To.Stack]) {
			found = append(found, d)
		}
	}
	return found, broken
}

func errText(err error) string {
	switch {
	case errors.Is(err, host.ErrOffline):
		return "host is offline"
	case errors.Is(err, host.ErrUnknownHost):
		return "host not found"
	case errors.Is(err, context.DeadlineExceeded):
		return "the host didn't answer in time"
	}
	return err.Error()
}
