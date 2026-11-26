package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"moorage/internal/diagnose"
	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/store"
)

// ---- Problems card (M18F) ---------------------------------------------------------
//
// GET /api/hosts/{host}/stacks/{name}/problems diagnoses the stack's containers on
// request: facts come from the host (docker inspect; older agents: the container list),
// the hub adds the log tail of unwell containers, recorded container events, the port
// map, recent changes and maintenance. Nothing new is alerted.

type problemsDTO struct {
	Problems []diagnose.Problem `json:"problems"`
	Ignored  []diagnose.Problem `json:"ignored"`
	// Basic is set when the host could only report the container list.
	Basic bool `json:"basic,omitempty"`
}

func problemIgnoreKey(hostID, stack string) string {
	return "problems_ignored:" + hostID + "/" + stack
}

// ignoredProblems are "service|rule" pairs the admin chose to ignore.
func (s *Server) ignoredProblems(ctx context.Context, hostID, stack string) map[string]bool {
	out := map[string]bool{}
	raw, err := s.store.GetSetting(ctx, problemIgnoreKey(hostID, stack))
	if err != nil || raw == "" {
		return out
	}
	var list []string
	if json.Unmarshal([]byte(raw), &list) == nil {
		for _, k := range list {
			out[k] = true
		}
	}
	return out
}

func problemKey(p diagnose.Problem) string {
	svc := p.Service
	if svc == "" {
		svc = p.Container
	}
	return svc + "|" + p.Rule
}

// stackProblems diagnoses a stack. It never fails the caller: a host that can't answer
// yields no problems.
func (s *Server) stackProblems(ctx context.Context, hostID string, h host.Host, stack string) (problemsDTO, error) {
	out := problemsDTO{Problems: []diagnose.Problem{}, Ignored: []diagnose.Problem{}}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	var facts []diagnose.Facts
	var err error
	if sd, ok := h.(host.StackDiagnoser); ok {
		facts, err = sd.DiagnoseStack(ctx, stack)
	}
	if facts == nil {
		if err != nil && !host.IsUnsupported(err) {
			return out, err
		}
		st, gerr := h.GetStack(ctx, stack)
		if gerr != nil {
			return out, gerr
		}
		for _, c := range st.Containers {
			facts = append(facts, engine.BasicFacts(c))
		}
		out.Basic = true
	}

	now := time.Now()
	dc := diagnose.Context{Now: now, DiskFreePct: -1, Logs: map[string][]string{}, PortOwners: map[int]string{}}
	unwell := false
	for _, f := range facts {
		if f.State != "running" || f.Health == "unhealthy" || f.RestartCount > 0 {
			unwell = true
		}
	}
	if !unwell {
		return out, nil
	}
	if lines, err := h.TailLogs(ctx, stack, 200); err == nil {
		size := map[string]int{}
		for _, l := range lines {
			if size[l.Container] > 64<<10 {
				continue
			}
			size[l.Container] += len(l.Message)
			dc.Logs[l.Container] = append(dc.Logs[l.Container], l.Message)
		}
	}
	if evs, err := s.store.ContainerEvents(ctx, hostID, stack, now.Add(-30*time.Minute), now); err == nil {
		for _, e := range evs {
			dc.Events = append(dc.Events, diagnose.Event{Service: e.Service, Type: e.Type, ExitCode: e.ExitCode, Time: e.Time, Count: e.Count})
		}
	}
	if rep, err := s.hostPorts(ctx, hostID, h, false); err == nil {
		for _, p := range portUses(rep, stack) {
			if _, taken := dc.PortOwners[p.Port]; !taken {
				dc.PortOwners[p.Port] = p.Owner
			}
		}
	}
	if vs, err := h.History(ctx, stack); err == nil {
		for _, v := range vs {
			if now.Sub(v.Time) > 2*time.Hour {
				break
			}
			msg := v.Message
			if msg == "" {
				msg = "a change"
			}
			dc.Changes = append(dc.Changes, diagnose.Change{Time: v.Time, Message: lowerFirst(msg)})
		}
	}
	if s.mon != nil {
		if _, ok := s.mon.MaintenanceFor(store.Check{HostID: hostID, Stack: stack}, now); ok {
			dc.Maintenance = true
		}
	}
	ignored := s.ignoredProblems(ctx, hostID, stack)
	for _, p := range diagnose.Diagnose(facts, dc) {
		if ignored[problemKey(p)] {
			out.Ignored = append(out.Ignored, p)
		} else {
			out.Problems = append(out.Problems, p)
		}
	}
	return out, nil
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func (s *Server) getStackProblems(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	out, err := s.stackProblems(r.Context(), r.PathValue("host"), h, r.PathValue("name"))
	if err != nil {
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// putProblemIgnored ignores or restores one problem (service + rule) of a stack.
func (s *Server) putProblemIgnored(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Service string `json:"service"`
		Rule    string `json:"rule"`
		Ignored bool   `json:"ignored"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Service, req.Rule = strings.TrimSpace(req.Service), strings.TrimSpace(req.Rule)
	if req.Service == "" || req.Rule == "" || len(req.Service) > 200 || len(req.Rule) > 64 {
		writeErr(w, http.StatusBadRequest, "Specify the service and the problem.")
		return
	}
	ctx := r.Context()
	hostID, stack := r.PathValue("host"), r.PathValue("name")
	set := s.ignoredProblems(ctx, hostID, stack)
	key := req.Service + "|" + req.Rule
	if req.Ignored {
		set[key] = true
	} else {
		delete(set, key)
	}
	list := make([]string, 0, len(set))
	for k := range set {
		list = append(list, k)
	}
	sort.Strings(list)
	b, _ := json.Marshal(list)
	if err := s.store.SetSetting(ctx, problemIgnoreKey(hostID, stack), string(b)); err != nil {
		s.engErr(w, err)
		return
	}
	setAuditStack(r, stack)
	w.WriteHeader(http.StatusNoContent)
}

// likelyCause is the one line for notifications and incidents ("Likely cause: Out of
// memory (exit 137)"); empty when nothing is known. Never includes log lines.
func (s *Server) likelyCause(ctx context.Context, hostID, stack string) string {
	if s.reg == nil || stack == "" {
		return ""
	}
	h, _, err := s.reg.Get(ctx, hostID)
	if err != nil {
		return ""
	}
	pr, err := s.stackProblems(ctx, hostID, h, stack)
	if err != nil || len(pr.Problems) == 0 || pr.Problems[0].Rule == "starting" {
		return ""
	}
	return pr.Problems[0].Cause
}
