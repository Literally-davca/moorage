package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"moorage/internal/templates"
)

// Templates exposes the template catalogue (main starts its daily refresh).
func (s *Server) Templates() *templates.Service { return s.tpl }

// listTemplates returns every template of every source with the sources' problems.
// Viewers may browse; only admins render and create.
func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	c, err := s.tpl.List(r.Context())
	if err != nil {
		s.log.Error("templates", "err", err)
		writeErr(w, http.StatusInternalServerError, "The template catalogue couldn't be read. Check the Moorage log.")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

type templateDetail struct {
	Template  templates.Entry      `json:"template"`
	Notes     string               `json:"notes,omitempty"`
	Variables []templates.Variable `json:"variables"`
	Compose   string               `json:"compose"`
	Env       string               `json:"env"`
	Warnings  []string             `json:"warnings"`
	// Generated holds fresh values for variables with a generator, so the form starts
	// pre-filled. Only for admins: viewers can't deploy.
	Generated map[string]string `json:"generated,omitempty"`
}

func (s *Server) getTemplate(w http.ResponseWriter, r *http.Request) {
	t, e, ok := s.loadTemplate(w, r, r.PathValue("source"), r.PathValue("id"))
	if !ok {
		return
	}
	d := templateDetail{Template: e, Notes: t.Notes, Variables: t.Variables, Compose: t.Compose, Env: t.Env, Warnings: templates.Warnings(t.Compose)}
	if currentUser(r).Role == "admin" {
		gen, err := templates.GenerateDefaults(t)
		if err != nil {
			s.log.Error("templates: generate", "err", err)
			writeErr(w, http.StatusInternalServerError, "Secrets couldn't be generated. Check the Moorage log.")
			return
		}
		d.Generated = gen
	}
	writeJSON(w, http.StatusOK, d)
}

// loadTemplate resolves a template or writes the refusal.
func (s *Server) loadTemplate(w http.ResponseWriter, r *http.Request, source, id string) (templates.Template, templates.Entry, bool) {
	t, e, err := s.tpl.Get(r.Context(), source, id)
	var be templates.BrokenError
	switch {
	case errors.Is(err, templates.ErrNotFound):
		writeErr(w, http.StatusNotFound, "Template not found.")
		return t, e, false
	case errors.As(err, &be):
		writeErr(w, http.StatusUnprocessableEntity, "This template can't be used: "+be.Msg)
		return t, e, false
	case err != nil:
		s.log.Error("templates: get", "source", source, "id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "The template couldn't be read. Check the Moorage log.")
		return t, e, false
	}
	return t, e, true
}

// renderTemplate turns variables into the final compose and .env. No side effects: it
// writes nothing and is not audited (auditSkip); the create step is.
func (s *Server) renderTemplate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source string            `json:"source"`
		ID     string            `json:"id"`
		Values map[string]string `json:"values"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	t, e, ok := s.loadTemplate(w, r, req.Source, req.ID)
	if !ok {
		return
	}
	out, err := templates.Render(t, req.Values)
	var fe *templates.FieldError
	if errors.As(err, &fe) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": fe.Name + ": " + fe.Msg + ".", "field": fe.Name})
		return
	}
	if err != nil {
		s.log.Error("templates: render", "err", err)
		writeErr(w, http.StatusInternalServerError, "The template couldn't be rendered. Check the Moorage log.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"template": e, "result": out})
}

// templateRef names the template a new stack comes from (createStack).
type templateRef struct {
	Source string `json:"source"`
	ID     string `json:"id"`
}

// templateNote builds the first version message for a stack created from ref. It is
// derived on the server from the catalogue, never taken as free text from the client.
func (s *Server) templateNote(r *http.Request, ref *templateRef) (string, bool) {
	if ref == nil {
		return "", true
	}
	if !templates.IDPattern.MatchString(ref.ID) || !validSourceKey(ref.Source) {
		return "", false
	}
	_, e, err := s.tpl.Get(r.Context(), ref.Source, ref.ID)
	if err != nil {
		// The template vanished between render and create (index refreshed): the stack is
		// still the admin's reviewed files, so record what is known.
		e = templates.Entry{Source: ref.Source, SourceLabel: ref.Source, ID: ref.ID}
	}
	return templates.Note(e), true
}

func validSourceKey(k string) bool {
	if k == templates.SourceBundled || k == templates.SourceLocal {
		return true
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(k, "remote-"), 10, 64)
	return strings.HasPrefix(k, "remote-") && err == nil && n > 0
}

// ---- remote sources (admin) ------------------------------------------------

func (s *Server) listTemplateSources(w http.ResponseWriter, r *http.Request) {
	list, err := s.tpl.Sources(r.Context())
	if err != nil {
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": list, "dir": s.cfg.TemplatesDir})
}

type sourceInput struct {
	URL        string `json:"url"`
	AllowLocal bool   `json:"allowLocal"`
}

func (s *Server) addTemplateSource(w http.ResponseWriter, r *http.Request) {
	var req sourceInput
	if !readJSON(w, r, &req) {
		return
	}
	id, err := s.tpl.AddSource(r.Context(), req.URL, req.AllowLocal)
	if s.sourceErr(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) updateTemplateSource(w http.ResponseWriter, r *http.Request) {
	id, ok := sourceID(w, r)
	if !ok {
		return
	}
	var req sourceInput
	if !readJSON(w, r, &req) {
		return
	}
	if s.sourceErr(w, s.tpl.UpdateSource(r.Context(), id, req.URL, req.AllowLocal)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteTemplateSource(w http.ResponseWriter, r *http.Request) {
	id, ok := sourceID(w, r)
	if !ok {
		return
	}
	if s.sourceErr(w, s.tpl.DeleteSource(r.Context(), id)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// refreshTemplateSource starts a background fetch; the UI polls the source list.
func (s *Server) refreshTemplateSource(w http.ResponseWriter, r *http.Request) {
	id, ok := sourceID(w, r)
	if !ok {
		return
	}
	if s.sourceErr(w, s.tpl.RefreshSource(r.Context(), id)) {
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func sourceID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusNotFound, "Template index not found.")
		return 0, false
	}
	return id, true
}

// sourceErr writes the response for a failed source change and reports whether it did.
func (s *Server) sourceErr(w http.ResponseWriter, err error) bool {
	var ie templates.InputError
	switch {
	case err == nil:
		return false
	case errors.As(err, &ie):
		writeErr(w, http.StatusBadRequest, sentence(ie))
	case errors.Is(err, templates.ErrNotFound):
		writeErr(w, http.StatusNotFound, "Template index not found.")
	default:
		s.log.Error("template sources", "err", err)
		writeErr(w, http.StatusInternalServerError, "The template index couldn't be saved. Check the Moorage log.")
	}
	return true
}
