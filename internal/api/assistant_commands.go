package api

import (
	"net/http"
	"regexp"
	"strings"

	"moorage/internal/store"
)

// ---- saved assistant commands (M19I) ------------------------------------------------
//
// A command is only the text of a message with {stack}, {host} and {container} filled in
// from the page: it changes no permission, data level or ban.

var commandNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// builtinCommands can't be overridden.
var builtinCommands = map[string]bool{"why": true, "logs": true, "explain": true, "link": true}

func (s *Server) listAssistantCommands(w http.ResponseWriter, r *http.Request) {
	cs, err := s.store.AssistantCommands(r.Context(), currentUser(r).ID)
	if err != nil {
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"commands": cs, "max": store.MaxAssistantCommands})
}

func (s *Server) putAssistantCommand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name := strings.ToLower(strings.TrimSpace(r.PathValue("name")))
	if !commandNameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "A command name has lowercase letters, digits and - (up to 32).")
		return
	}
	if builtinCommands[name] {
		writeErr(w, http.StatusBadRequest, "/"+name+" is a built-in command. Choose another name.")
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" || len([]rune(req.Text)) > 2000 {
		writeErr(w, http.StatusBadRequest, "The text of a command has 1 to 2000 characters.")
		return
	}
	ok, err := s.store.SaveAssistantCommand(r.Context(), currentUser(r).ID, name, req.Text)
	if err != nil {
		s.engErr(w, err)
		return
	}
	if !ok {
		writeErr(w, http.StatusConflict, "You have 50 saved commands. Delete one first.")
		return
	}
	s.listAssistantCommands(w, r)
}

func (s *Server) deleteAssistantCommand(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteAssistantCommand(r.Context(), currentUser(r).ID, r.PathValue("name")); err != nil {
		s.engErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
