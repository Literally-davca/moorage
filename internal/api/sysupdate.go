package api

import (
	"net/http"
	"strings"

	"moorage/internal/selfupdate"
)

func (s *Server) systemUpdates(w http.ResponseWriter, r *http.Request) {
	if s.sysUpd == nil {
		writeErr(w, http.StatusServiceUnavailable, "Self-update service is not initialised.")
		return
	}
	ov, err := s.sysUpd.Overview(r.Context())
	if err != nil {
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ov)
}

func (s *Server) checkSystemUpdates(w http.ResponseWriter, r *http.Request) {
	if s.sysUpd == nil {
		writeErr(w, http.StatusServiceUnavailable, "Self-update service is not initialised.")
		return
	}
	ov, checkErr := s.sysUpd.CheckNow(r.Context())
	if checkErr != nil && ov.Release == nil {
		// Return 200 with checkError inside Overview so the UI renders the cached view + error message
		// cleanly, OR if an API client expects failure on unreachable update server, wait: CheckNow
		// always preserves the cached release and populates CheckError in OverviewResponse.
		ov.CheckError = checkErr.Error()
	}
	writeJSON(w, http.StatusOK, ov)
}

func (s *Server) applySystemUpdates(w http.ResponseWriter, r *http.Request) {
	if s.sysUpd == nil {
		writeErr(w, http.StatusServiceUnavailable, "Self-update service is not initialised.")
		return
	}
	var req struct {
		Target  string   `json:"target"`
		Targets []string `json:"targets"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Target != "" {
		req.Targets = append(req.Targets, req.Target)
	}
	u := currentUser(r)
	jobs, restartingHub, auditTarget, err := s.sysUpd.Apply(r.Context(), req.Targets, u.Username, clientIP(r))
	if err != nil {
		if code, msg, ok := selfupdate.IsRequestError(err); ok {
			writeErr(w, code, msg)
			return
		}
		s.engErr(w, err)
		return
	}
	setAuditTarget(r, auditTarget)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"jobs":          jobs,
		"restartingHub": restartingHub,
	})
}

func (s *Server) rollbackSystemUpdate(w http.ResponseWriter, r *http.Request) {
	if s.sysUpd == nil {
		writeErr(w, http.StatusServiceUnavailable, "Self-update service is not initialised.")
		return
	}
	nodeID := strings.TrimSpace(r.PathValue("node"))
	u := currentUser(r)
	job, auditTarget, err := s.sysUpd.Rollback(r.Context(), nodeID, u.Username, clientIP(r))
	if err != nil {
		if code, msg, ok := selfupdate.IsRequestError(err); ok {
			writeErr(w, code, msg)
			return
		}
		s.engErr(w, err)
		return
	}
	setAuditTarget(r, auditTarget)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"job":           job,
		"restartingHub": nodeID == "hub",
	})
}
