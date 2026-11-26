package api

import (
	"errors"
	"net/http"
	"strconv"

	"moorage/internal/incident"
	"moorage/internal/store"
)

// checkIncident reads the incident id of the path and makes sure it belongs to the check
// of the path. It writes the error and returns false when not.
func (s *Server) checkIncident(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("incidentId"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid incident id.")
		return 0, false
	}
	inc, err := s.store.IncidentByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && inc.CheckID != checkID(r)) {
		writeErr(w, http.StatusNotFound, "Incident not found.")
		return 0, false
	}
	if err != nil {
		s.engErr(w, err)
		return 0, false
	}
	return id, true
}

// getIncidentTimeline returns what changed before an incident (M18A).
func (s *Server) getIncidentTimeline(w http.ResponseWriter, r *http.Request) {
	id, ok := s.checkIncident(w, r)
	if !ok {
		return
	}
	tl, err := incident.LoadTimeline(r.Context(), s.store, id)
	if err != nil {
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tl)
}

// getRollbackOptions lists what Rollback & Verify can undo for an open incident.
func (s *Server) getRollbackOptions(w http.ResponseWriter, r *http.Request) {
	id, ok := s.checkIncident(w, r)
	if !ok {
		return
	}
	if s.rollbackV == nil {
		writeJSON(w, http.StatusOK, []incident.RollbackOption{})
		return
	}
	opts, err := s.rollbackV.GetRollbackOptions(r.Context(), id)
	if err != nil {
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

// postIncidentRollback starts Rollback & Verify for an open incident.
func (s *Server) postIncidentRollback(w http.ResponseWriter, r *http.Request) {
	id, ok := s.checkIncident(w, r)
	if !ok {
		return
	}
	if s.rollbackV == nil {
		writeErr(w, http.StatusNotImplemented, "Rollback & Verify isn't available.")
		return
	}
	var in struct {
		Type string `json:"type"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	kind := incident.RollbackType(in.Type)
	if kind != incident.RollbackTypeImage && kind != incident.RollbackTypeCompose {
		writeErr(w, http.StatusBadRequest, "Type must be image or compose.")
		return
	}
	state, err := s.rollbackV.StartRollbackVerify(r.Context(), id, kind, currentUser(r).Username)
	switch {
	case errors.Is(err, incident.ErrRollbackRunning), errors.Is(err, incident.ErrChangedSince), errors.Is(err, incident.ErrIncidentClosed):
		writeErr(w, http.StatusConflict, capitalize(err.Error())+".")
	case errors.Is(err, incident.ErrNoRollbackTarget), errors.Is(err, incident.ErrNoStack):
		writeErr(w, http.StatusBadRequest, capitalize(err.Error())+".")
	case err != nil:
		s.engErr(w, err)
	default:
		writeJSON(w, http.StatusOK, state)
	}
}

// getIncidentRollbackState returns the Rollback & Verify state of an incident.
func (s *Server) getIncidentRollbackState(w http.ResponseWriter, r *http.Request) {
	id, ok := s.checkIncident(w, r)
	if !ok {
		return
	}
	state, err := incident.LoadState(r.Context(), s.store, id)
	if err != nil {
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}
