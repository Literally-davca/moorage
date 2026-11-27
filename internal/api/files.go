package api

import (
	"errors"
	"net/http"

	"moorage/internal/engine"
	"moorage/internal/host"
)

// ---- Files tab (M19M) ----------------------------------------------------------------

func (s *Server) stackFiler(w http.ResponseWriter, r *http.Request, write bool) (host.StackFiler, bool) {
	h, ok := s.hostOf(w, r, write)
	if !ok {
		return nil, false
	}
	sf, ok := h.(host.StackFiler)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "This host has no Files tab.")
		return nil, false
	}
	return sf, true
}

func (s *Server) filesErr(w http.ResponseWriter, err error) {
	switch {
	case host.IsUnsupported(err):
		writeErr(w, http.StatusNotImplemented, "Update the agent to use the Files tab.")
	case errors.Is(err, engine.ErrNotText):
		writeErr(w, http.StatusUnprocessableEntity, "Only text files up to 512 KB can be opened here.")
	default:
		s.engErr(w, err)
	}
}

func (s *Server) listStackFiles(w http.ResponseWriter, r *http.Request) {
	sf, ok := s.stackFiler(w, r, false)
	if !ok {
		return
	}
	l, err := sf.ListStackFiles(r.Context(), r.PathValue("name"))
	if err != nil {
		s.filesErr(w, err)
		return
	}
	if l == nil {
		l = []engine.StackFile{}
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) readStackFile(w http.ResponseWriter, r *http.Request) {
	sf, ok := s.stackFiler(w, r, false)
	if !ok {
		return
	}
	c, err := sf.ReadStackFile(r.Context(), r.PathValue("name"), r.URL.Query().Get("path"))
	if err != nil {
		s.filesErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"content": c})
}

func (s *Server) writeStackFile(w http.ResponseWriter, r *http.Request) {
	sf, ok := s.stackFiler(w, r, true)
	if !ok {
		return
	}
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	if err := sf.WriteStackFile(r.Context(), name, req.Path, req.Content, currentUser(r).Username); err != nil {
		s.filesErr(w, err)
		return
	}
	setAuditStack(r, name)
	s.stackEvent(r, name, "edit", "Edited "+req.Path)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) stackFileVersions(w http.ResponseWriter, r *http.Request) {
	sf, ok := s.stackFiler(w, r, false)
	if !ok {
		return
	}
	v, err := sf.StackFileVersions(r.Context(), r.PathValue("name"), r.URL.Query().Get("path"))
	if err != nil {
		s.filesErr(w, err)
		return
	}
	if v == nil {
		v = []string{}
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) stackFileVersion(w http.ResponseWriter, r *http.Request) {
	sf, ok := s.stackFiler(w, r, false)
	if !ok {
		return
	}
	c, err := sf.StackFileVersion(r.Context(), r.PathValue("name"), r.URL.Query().Get("path"), r.PathValue("id"))
	if err != nil {
		s.filesErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"content": c})
}
