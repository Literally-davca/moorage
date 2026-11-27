package api

import (
	"net/http"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/lint"
)

// ---- Apply & restart rollback banner (M18H) ----------------------------------------

type selfNotice struct {
	HostID   string    `json:"hostId"`
	HostName string    `json:"hostName"`
	Stack    string    `json:"stack"`
	At       time.Time `json:"at"`
	Message  string    `json:"message"`
	Reason   string    `json:"reason,omitempty"`
}

// selfApplyNotices lists rolled-back Apply & restart runs no admin dismissed yet.
func (s *Server) selfApplyNotices(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := []selfNotice{}
	if s.reg != nil {
		if hosts, err := s.reg.List(ctx); err == nil {
			for _, he := range hosts {
				if !he.Connected && he.Kind != "local" {
					continue
				}
				h, _, err := s.reg.Get(ctx, he.ID)
				if err != nil {
					continue
				}
				list, err := h.ListStacks(ctx)
				if err != nil {
					continue
				}
				for _, sm := range list {
					if !sm.Self {
						continue
					}
					st, err := h.GetStack(ctx, sm.Name)
					if err != nil || st.SelfResult == nil || !st.SelfResult.RolledBack || st.SelfResult.Acknowledged {
						continue
					}
					out = append(out, selfNotice{HostID: he.ID, HostName: he.Name, Stack: sm.Name, At: st.SelfResult.At, Message: st.SelfResult.Message, Reason: st.SelfResult.Reason})
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) ackSelfApply(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	si, ok := h.(host.SelfApplyInfo)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "Update the agent.")
		return
	}
	if err := si.AckSelfResult(r.Context()); err != nil {
		s.engErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) selfApplyLog(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	si, ok := h.(host.SelfApplyInfo)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "Update the agent to see the log.")
		return
	}
	l, err := si.SelfApplyLog(r.Context())
	if err != nil {
		if host.IsUnsupported(err) {
			writeErr(w, http.StatusNotImplemented, "Update the agent to see the log.")
			return
		}
		writeErr(w, http.StatusNotFound, "No log of an Apply & restart yet.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"log": l})
}

// selfApplyBlocked refuses a configuration of Moorage's own stack whose ports Docker
// would drop or refuse (M18H), whose listen address would crash it, or whose listen
// ports are taken on the host in host network mode (M19B).
func (s *Server) selfApplyBlocked(r *http.Request, hostID string, h host.Host, name, compose, env string) string {
	o := lint.Options{Name: name}
	if st, err := h.GetStack(r.Context(), name); err == nil && st.Self {
		o.Self = s.selfLint(hostID, st)
		o.Running = s.runningPorts(r.Context(), hostID, h, name)
	}
	for _, is := range lint.Lint(r.Context(), compose, env, o) {
		switch is.Code {
		case "ports-ignored", "ports-conflict-network":
			return is.Message + " Moorage would not be reachable after the restart."
		case "self-port-taken", "self-listen-invalid":
			return is.Message
		}
	}
	return ""
}

// selfLint describes the self stack for the linter: its service, the ports the running
// Moorage holds (free again once it's recreated) and whether host networking can be
// verified by the helper.
func (s *Server) selfLint(hostID string, st engine.Stack) *lint.SelfLint {
	sl := &lint.SelfLint{Service: st.SelfService, HostNetUnverifiable: st.SelfHostNetUnverifiable}
	if sl.Service == "" {
		return nil // older agent: it doesn't say which service Moorage is
	}
	for _, c := range st.Containers {
		if c.Service != sl.Service {
			continue
		}
		for _, p := range c.Ports {
			if pb, ok := engine.ParsePortString(p); ok {
				sl.OwnPorts = append(sl.OwnPorts, pb.Public)
			}
		}
	}
	if hostID == host.LocalID {
		for _, a := range []string{s.cfg.Listen, s.cfg.AgentListen, s.cfg.WatchdogListen, s.cfg.StatusListen} {
			if _, p, _, ok, _ := lint.ParseListen(a); ok && p > 0 {
				sl.OwnPorts = append(sl.OwnPorts, p)
			}
		}
	}
	return sl
}
