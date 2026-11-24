package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"moorage/internal/deps"
	"moorage/internal/host"
	"moorage/internal/store"
)

// Deps exposes the dependency graph service (main wires it into the monitor and starts it).
func (s *Server) Deps() *deps.Service { return s.deps }

// depNode is a stack in the graph with its status for the UI.
type depNode struct {
	store.StackRef
	HostName string `json:"hostName,omitempty"`
	Status   string `json:"status,omitempty"`   // container status: up | down | degraded | … ("" unknown)
	Running  int    `json:"running,omitempty"`  // running container count
	Total    int    `json:"total,omitempty"`    // total container count
	Checks   string `json:"checks,omitempty"`   // worst check status of the stack, "" without checks
	InCycle  bool   `json:"inCycle,omitempty"`  // part of a dependency loop (ignored for grouping)
	External bool   `json:"external,omitempty"` // on another host than the one viewed
}

// depHost describes a host/device in the dependency graph.
type depHost struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Kind      string     `json:"kind"` // "local" or "agent"
	Connected bool       `json:"connected"`
	ReadOnly  bool       `json:"readOnly,omitempty"`
	LastSeen  *time.Time `json:"lastSeen,omitempty"`
}

type depsResponse struct {
	Edges     []store.StackDep   `json:"edges"`
	Nodes     []depNode          `json:"nodes"`
	Hosts     []depHost          `json:"hosts"`
	Cycles    [][]store.StackRef `json:"cycles"`
	Detection deps.Detection     `json:"detection"`
	// StacksError says why the host's own stack list is missing (offline).
	StacksError string `json:"stacksError,omitempty"`
}

// hostDeps returns every edge that starts or ends on a host, the stacks involved (all
// stacks of the host when it is online) with their status, cycles and the detection state.
func (s *Server) hostDeps(w http.ResponseWriter, r *http.Request) {
	if s.reg == nil {
		writeErr(w, http.StatusNotFound, "Host not found.")
		return
	}
	ctx := r.Context()
	hostID := r.PathValue("host")
	if _, _, err := s.reg.Get(ctx, hostID); errors.Is(err, host.ErrUnknownHost) {
		writeErr(w, http.StatusNotFound, "Host not found.")
		return
	}
	edges, err := s.store.StackDepsOfHost(ctx, hostID)
	if err != nil {
		s.engErr(w, err)
		return
	}
	if edges == nil {
		edges = []store.StackDep{}
	}
	resp := depsResponse{Edges: edges, Cycles: [][]store.StackRef{}, Detection: s.deps.Status(hostID)}
	g := s.deps.Graph()
	for _, c := range g.Cycles() {
		if cycleTouches(c, hostID) {
			resp.Cycles = append(resp.Cycles, c)
		}
	}
	resp.Nodes, resp.StacksError = s.depNodes(ctx, hostID, edges, g)
	seenHosts := map[string]bool{hostID: true}
	for _, n := range resp.Nodes {
		seenHosts[n.HostID] = true
	}
	resp.Hosts = s.depHosts(ctx, seenHosts)
	writeJSON(w, http.StatusOK, resp)
}

// allDeps returns the dependency graph across all registered hosts (Hub + all agents).
func (s *Server) allDeps(w http.ResponseWriter, r *http.Request) {
	if s.reg == nil {
		writeJSON(w, http.StatusOK, depsResponse{
			Edges:  []store.StackDep{},
			Nodes:  []depNode{},
			Hosts:  []depHost{},
			Cycles: [][]store.StackRef{},
		})
		return
	}
	ctx := r.Context()
	edges, err := s.store.StackDeps(ctx)
	if err != nil {
		s.engErr(w, err)
		return
	}
	if edges == nil {
		edges = []store.StackDep{}
	}
	g := s.deps.Graph()
	cycles := g.Cycles()
	if cycles == nil {
		cycles = [][]store.StackRef{}
	}
	hostsList, _ := s.reg.List(ctx)
	want := map[string]map[string]bool{}
	for _, h := range hostsList {
		want[h.ID] = map[string]bool{}
	}
	for _, e := range edges {
		for _, ref := range []store.StackRef{e.From, e.To} {
			if want[ref.HostID] == nil {
				want[ref.HostID] = map[string]bool{}
			}
			want[ref.HostID][ref.Stack] = true
		}
	}
	names := s.hostNames(ctx)
	checks := map[string]string{}
	if s.mon != nil {
		checks = s.mon.StackCheckStatus()
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	var out []depNode
	var errMsgs []string

	for hid, stacks := range want {
		wg.Add(1)
		go func(hid string, stacks map[string]bool) {
			defer wg.Done()
			type stackMeta struct {
				status         string
				running, total int
			}
			status := map[string]stackMeta{}
			lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if h, _, err := s.reg.Get(lctx, hid); err == nil {
				list, lerr := h.ListStacks(lctx)
				for _, st := range list {
					status[st.Name] = stackMeta{status: st.Status, running: st.Running, total: st.Total}
					stacks[st.Name] = true
				}
				if lerr != nil {
					mu.Lock()
					name := names[hid]
					if name == "" {
						name = hid
					}
					errMsgs = append(errMsgs, name+": "+sentence(lerr))
					mu.Unlock()
				}
			} else {
				_, msg := s.errStatus(err)
				mu.Lock()
				name := names[hid]
				if name == "" {
					name = hid
				}
				errMsgs = append(errMsgs, name+": "+msg)
				mu.Unlock()
			}
			mu.Lock()
			for name := range stacks {
				ref := store.StackRef{HostID: hid, Stack: name}
				sm := status[name]
				out = append(out, depNode{
					StackRef: ref, HostName: names[hid], Status: sm.status, Running: sm.running, Total: sm.total,
					Checks: checks[ref.Key()], InCycle: g.InCycle(ref),
				})
			}
			mu.Unlock()
		}(hid, stacks)
	}
	wg.Wait()

	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.HostID != b.HostID {
			if a.HostID == host.LocalID {
				return true
			}
			if b.HostID == host.LocalID {
				return false
			}
			return a.HostID < b.HostID
		}
		return a.Stack < b.Stack
	})
	if out == nil {
		out = []depNode{}
	}
	sort.Strings(errMsgs)
	seenHosts := map[string]bool{}
	for hid := range want {
		seenHosts[hid] = true
	}
	writeJSON(w, http.StatusOK, depsResponse{
		Edges:       edges,
		Nodes:       out,
		Hosts:       s.depHosts(ctx, seenHosts),
		Cycles:      cycles,
		StacksError: strings.Join(errMsgs, " "),
	})
}

func (s *Server) depHosts(ctx context.Context, filter map[string]bool) []depHost {
	list, err := s.reg.List(ctx)
	if err != nil {
		return []depHost{}
	}
	out := make([]depHost, 0, len(list))
	present := map[string]bool{}
	for _, h := range list {
		if len(filter) > 0 && !filter[h.ID] {
			continue
		}
		present[h.ID] = true
		dh := depHost{
			ID:        h.ID,
			Name:      h.Name,
			Kind:      h.Kind,
			Connected: h.Connected,
			ReadOnly:  h.ReadOnly,
		}
		if !h.LastSeen.IsZero() {
			t := h.LastSeen
			dh.LastSeen = &t
		}
		out = append(out, dh)
	}
	for hid := range filter {
		if !present[hid] {
			out = append(out, depHost{ID: hid, Name: hid, Kind: "agent"})
		}
	}
	return out
}

func cycleTouches(c []store.StackRef, hostID string) bool {
	for _, r := range c {
		if r.HostID == hostID {
			return true
		}
	}
	return false
}

// depNodes lists the stacks of hostID plus every stack an edge names, with statuses. Other
// hosts are asked for their stack list once each, with a short timeout.
func (s *Server) depNodes(ctx context.Context, hostID string, edges []store.StackDep, g *deps.Graph) ([]depNode, string) {
	names := s.hostNames(ctx)
	checks := map[string]string{}
	if s.mon != nil {
		checks = s.mon.StackCheckStatus()
	}
	want := map[string]map[string]bool{hostID: {}}
	for _, e := range edges {
		for _, ref := range []store.StackRef{e.From, e.To} {
			if want[ref.HostID] == nil {
				want[ref.HostID] = map[string]bool{}
			}
			want[ref.HostID][ref.Stack] = true
		}
	}
	var out []depNode
	stacksErr := ""
	for hid, stacks := range want {
		type stackMeta struct {
			status         string
			running, total int
		}
		status := map[string]stackMeta{}
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if h, _, err := s.reg.Get(lctx, hid); err == nil {
			list, lerr := h.ListStacks(lctx)
			for _, st := range list {
				status[st.Name] = stackMeta{status: st.Status, running: st.Running, total: st.Total}
				if hid == hostID {
					stacks[st.Name] = true // every stack of the viewed host is a node
				}
			}
			if lerr != nil && hid == hostID {
				stacksErr = sentence(lerr)
			}
		} else if hid == hostID {
			_, msg := s.errStatus(err)
			stacksErr = msg
		}
		cancel()
		for name := range stacks {
			ref := store.StackRef{HostID: hid, Stack: name}
			sm := status[name]
			out = append(out, depNode{
				StackRef: ref, HostName: names[hid], Status: sm.status, Running: sm.running, Total: sm.total,
				Checks: checks[ref.Key()], InCycle: g.InCycle(ref), External: hid != hostID,
			})
		}
	}
	sortNodes(out)
	return out, stacksErr
}

// sortNodes puts the viewed host's stacks first, then other hosts, each by name.
func sortNodes(ns []depNode) {
	sort.Slice(ns, func(i, j int) bool {
		a, b := ns[i], ns[j]
		if a.External != b.External {
			return !a.External
		}
		if a.HostID != b.HostID {
			return a.HostID < b.HostID
		}
		return a.Stack < b.Stack
	})
}

// depStackName accepts managed stack names and compose project names found in Docker.
var depStackName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// addDep adds a manual edge: the stack in the path depends on the stack in the body.
// Cross-host edges are allowed here (detection never makes them).
func (s *Server) addDep(w http.ResponseWriter, r *http.Request) {
	if s.reg == nil {
		writeErr(w, http.StatusNotFound, "Host not found.")
		return
	}
	from := store.StackRef{HostID: r.PathValue("host"), Stack: r.PathValue("name")}
	var req struct {
		HostID string `json:"hostId"`
		Stack  string `json:"stack"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	to := store.StackRef{HostID: strings.TrimSpace(req.HostID), Stack: strings.TrimSpace(req.Stack)}
	if to.HostID == "" {
		to.HostID = from.HostID
	}
	if !depStackName.MatchString(from.Stack) || !depStackName.MatchString(to.Stack) {
		writeErr(w, http.StatusBadRequest, "Use a stack name of 1–63 letters, digits, dots, dashes or underscores.")
		return
	}
	if from == to {
		writeErr(w, http.StatusBadRequest, "A stack can't depend on itself.")
		return
	}
	for _, hid := range []string{from.HostID, to.HostID} {
		if _, _, err := s.reg.Get(r.Context(), hid); errors.Is(err, host.ErrUnknownHost) {
			writeErr(w, http.StatusNotFound, "Host not found.")
			return
		}
	}
	// Refuse a duplicate and an edge that closes a loop, before anything is stored.
	if existing, err := s.store.StackDeps(r.Context()); err == nil {
		for _, e := range existing {
			if e.From == from && e.To == to && !e.Ignored {
				writeErr(w, http.StatusConflict, "This dependency already exists.")
				return
			}
		}
	}
	if g := s.deps.Graph(); g != nil {
		for _, up := range g.Upstream(to, 64) {
			if up == from {
				writeErr(w, http.StatusConflict, fmt.Sprintf("%s already depends on %s: this edge would make a loop.", to.Stack, from.Stack))
				return
			}
		}
	}
	d, err := s.store.AddManualDep(r.Context(), from, to, currentUser(r).Username)
	if err != nil {
		s.engErr(w, err)
		return
	}
	s.reloadDeps(r.Context())
	s.log.Info("stack dependency added", "from", from.Key(), "to", to.Key(), "user", currentUser(r).Username)
	writeJSON(w, http.StatusCreated, d)
}

// depOfPath loads the edge {id} and makes sure it touches the stack in the path, so the
// audit row (host, stack) names a stack the edge belongs to.
func (s *Server) depOfPath(w http.ResponseWriter, r *http.Request) (store.StackDep, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusNotFound, "Dependency not found.")
		return store.StackDep{}, false
	}
	d, err := s.store.StackDepByID(r.Context(), id)
	here := store.StackRef{HostID: r.PathValue("host"), Stack: r.PathValue("name")}
	if err != nil || (d.From != here && d.To != here) {
		writeErr(w, http.StatusNotFound, "Dependency not found.")
		return store.StackDep{}, false
	}
	return d, true
}

// removeDep deletes a manual edge. A detected edge would come back on the next run, so it
// is ignored instead.
func (s *Server) removeDep(w http.ResponseWriter, r *http.Request) {
	d, ok := s.depOfPath(w, r)
	if !ok {
		return
	}
	if d.Source != store.DepManual {
		writeErr(w, http.StatusConflict, "Detected dependencies come back on the next detection. Ignore it instead.")
		return
	}
	if err := s.store.DeleteDep(r.Context(), d.ID); err != nil {
		s.engErr(w, err)
		return
	}
	s.reloadDeps(r.Context())
	// Detection may find the same pair again; run it so the graph shows that at once.
	if d.From.HostID == d.To.HostID {
		s.deps.Trigger(d.From.HostID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ignoreDep rejects (or accepts again) a detected edge.
func (s *Server) ignoreDep(w http.ResponseWriter, r *http.Request) {
	d, ok := s.depOfPath(w, r)
	if !ok {
		return
	}
	var req struct {
		Ignored bool `json:"ignored"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if d.Source != store.DepDetected {
		writeErr(w, http.StatusConflict, "Only detected dependencies can be ignored. Remove a manual one instead.")
		return
	}
	if err := s.store.SetDepIgnored(r.Context(), d.ID, req.Ignored); err != nil {
		s.engErr(w, err)
		return
	}
	s.reloadDeps(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// setDepAutoRestart sets the auto-restart policy of an edge (the dependent is restarted
// when the stack it depends on recovers, or also after it was deployed).
func (s *Server) setDepAutoRestart(w http.ResponseWriter, r *http.Request) {
	d, ok := s.depOfPath(w, r)
	if !ok {
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !store.ValidAutoRestart[req.Mode] {
		writeErr(w, http.StatusBadRequest, "Pick off, on_recover or on_recover_and_deploy.")
		return
	}
	if err := s.store.SetDepAutoRestart(r.Context(), d.ID, req.Mode); err != nil {
		s.engErr(w, err)
		return
	}
	s.reloadDeps(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// detectDeps starts a detection run on a host in the background.
func (s *Server) detectDeps(w http.ResponseWriter, r *http.Request) {
	if s.reg == nil {
		writeErr(w, http.StatusNotFound, "Host not found.")
		return
	}
	hostID := r.PathValue("host")
	if _, _, err := s.reg.Get(r.Context(), hostID); err != nil {
		code, msg := http.StatusNotFound, "Host not found."
		if errors.Is(err, host.ErrOffline) {
			code, msg = http.StatusServiceUnavailable, "The host is offline."
		}
		writeErr(w, code, msg)
		return
	}
	s.deps.Trigger(hostID)
	writeJSON(w, http.StatusAccepted, s.deps.Status(hostID))
}

func (s *Server) reloadDeps(ctx context.Context) {
	if err := s.deps.Reload(ctx); err != nil {
		s.log.Warn("reload dependency graph", "err", err)
	}
}
