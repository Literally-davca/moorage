package api

import (
	"errors"
	"net/http"
	"strconv"

	"moorage/internal/host"
)

// ---- Tailscale Serve (M19J) ---------------------------------------------------------
//
// Opt-in on each node (MOORAGE_TAILSCALE_SERVE=true and the tailscaled socket mounted):
// the node itself enforces it. Tailnet only, never Funnel.

func (s *Server) tailscaleHost(w http.ResponseWriter, r *http.Request, write bool) (host.TailscaleServer, bool) {
	h, ok := s.hostOf(w, r, write)
	if !ok {
		return nil, false
	}
	ts, ok := h.(host.TailscaleServer)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "This host can't use Tailscale Serve.")
		return nil, false
	}
	return ts, true
}

func (s *Server) tailscaleStatus(w http.ResponseWriter, r *http.Request) {
	ts, ok := s.tailscaleHost(w, r, false)
	if !ok {
		return
	}
	st, err := ts.TailscaleStatus(r.Context())
	if err != nil {
		if host.IsUnsupported(err) {
			writeJSON(w, http.StatusOK, map[string]any{"available": false, "reason": "Update the agent to use Tailscale Serve.", "entries": []any{}})
			return
		}
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) tailscaleServe(w http.ResponseWriter, r *http.Request) {
	ts, ok := s.tailscaleHost(w, r, true)
	if !ok {
		return
	}
	var req struct {
		Port   int    `json:"port"`
		Target string `json:"target"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Target == "" {
		req.Target = "http://127.0.0.1:" + strconv.Itoa(req.Port)
	}
	e, err := ts.TailscaleServe(r.Context(), req.Port, req.Target)
	if err != nil {
		if errors.Is(err, host.ErrHostReadOnly) {
			s.engErr(w, err)
			return
		}
		writeErr(w, http.StatusConflict, capitalize(err.Error())+".")
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) tailscaleStop(w http.ResponseWriter, r *http.Request) {
	ts, ok := s.tailscaleHost(w, r, true)
	if !ok {
		return
	}
	p, _ := strconv.Atoi(r.PathValue("port"))
	if err := ts.TailscaleStop(r.Context(), p); err != nil {
		writeErr(w, http.StatusConflict, capitalize(err.Error())+".")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
