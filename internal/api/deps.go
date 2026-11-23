package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
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
	Checks   string `json:"checks,omitempty"`   // worst check status of the stack, "" without checks
	InCycle  bool   `json:"inCycle,omitempty"`  // part of a dependency loop (ignored for grouping)
	External bool   `json:"external,omitempty"` // on another host than the one viewed
}

type depsResponse struct {
	Edges     []store.StackDep   `json:"edges"`
	Nodes     []depNode          `json:"nodes"`
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
	writeJSON(w, http.StatusOK, resp)
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
		status := map[string]string{}
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if h, _, err := s.reg.Get(lctx, hid); err == nil {
			list, lerr := h.ListStacks(lctx)
			for _, st := range list {
				status[st.Name] = st.Status
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
			out = append(out, depNode{
				StackRef: ref, HostName: names[hid], Status: status[name], Checks: checks[ref.Key()],
				InCycle: g.InCycle(ref), External: hid != hostID,
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
