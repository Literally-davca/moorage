package api

import (
	"net/http"

	"moorage/internal/engine"
	"moorage/internal/host"
)

// ---- every container of a host (global logs, terminals, external management) --------

func (s *Server) containerManager(w http.ResponseWriter, r *http.Request, write bool) (host.Host, host.ContainerManager, bool) {
	h, ok := s.hostOf(w, r, write)
	if !ok {
		return nil, nil, false
	}
	cm, ok := h.(host.ContainerManager)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "Update this host's agent to list all of its containers.")
		return nil, nil, false
	}
	return h, cm, true
}

func (s *Server) listContainers(w http.ResponseWriter, r *http.Request) {
	_, cm, ok := s.containerManager(w, r, false)
	if !ok {
		return
	}
	list, err := cm.Containers(r.Context())
	if host.IsUnsupported(err) {
		writeErr(w, http.StatusNotImplemented, "Update this host's agent to list all of its containers.")
		return
	}
	if err != nil {
		s.engErr(w, err)
		return
	}
	if list == nil {
		list = []engine.ContainerInfo{}
	}
	writeJSON(w, http.StatusOK, list)
}

// containerAction starts, stops, restarts or pulls and recreates one container outside a
// managed stack (MOORAGE_MANAGE_EXTERNAL on the host).
func (s *Server) containerAction(w http.ResponseWriter, r *http.Request) {
	h, cm, ok := s.containerManager(w, r, true)
	if !ok {
		return
	}
	id, action := r.PathValue("id"), r.PathValue("action")
	if !engine.ContainerActions[action] {
		writeErr(w, http.StatusBadRequest, "Unknown action.")
		return
	}
	// A container of a compose project silences that project's checks like a deploy.
	project := ""
	if list, err := cm.Containers(r.Context()); err == nil {
		for _, c := range list {
			if c.ID == id || c.Name == id {
				project = c.Project
				if c.Managed {
					writeErr(w, http.StatusConflict, "This container belongs to a stack Moorage manages: use the stack's actions.")
					return
				}
			}
		}
	}
	if s.coolifyOwns(r.Context(), r.PathValue("host"), project) {
		writeErr(w, http.StatusConflict, "This container is managed by Coolify: use the Coolify actions so Coolify's state stays right.")
		return
	}
	op, err := cm.ContainerAction(r.Context(), id, action, currentUser(r).Username)
	if err != nil {
		s.engErr(w, err)
		return
	}
	if project != "" {
		s.stackEvent(r, project, containerEvent[action], "container "+id)
	}
	s.watchOp(h, r.PathValue("host"), op.Stack, action, op.ID)
	writeJSON(w, http.StatusAccepted, op)
}

var containerEvent = map[string]string{"start": "start", "stop": "stop", "restart": "restart", "recreate": "update"}

// externalAction starts, stops or restarts a compose project outside the stacks folder.
func (s *Server) externalAction(w http.ResponseWriter, r *http.Request) {
	h, cm, ok := s.containerManager(w, r, true)
	if !ok {
		return
	}
	name, action := r.PathValue("name"), r.PathValue("action")
	if s.coolifyOwns(r.Context(), r.PathValue("host"), name) {
		writeErr(w, http.StatusConflict, "This stack is managed by Coolify: use the Coolify actions so Coolify's state stays right.")
		return
	}
	op, err := cm.ExternalStackAction(r.Context(), name, action, currentUser(r).Username)
	if err != nil {
		s.engErr(w, err)
		return
	}
	s.stackEvent(r, name, containerEvent[action], "")
	s.watchOp(h, r.PathValue("host"), name, action, op.ID)
	writeJSON(w, http.StatusAccepted, op)
}
