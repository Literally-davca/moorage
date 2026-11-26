package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/lint"
	"moorage/internal/notify"
	"moorage/internal/store"
)

// ---- compose linter, quick fixes and moving environment values into .env ----------

type lintReq struct {
	Compose string `json:"compose"`
	Env     string `json:"env"`
	// Deploy also checks what only blocks a deploy (missing variables, host ports taken by
	// running containers).
	Deploy bool `json:"deploy"`
}

// runningPorts lists the host ports taken outside the stack: containers of other stacks
// and standalone containers, ports declared by stopped stacks (reserved) and, in native /
// host-network mode, the host's own sockets. It shares the collection with the port map.
func (s *Server) runningPorts(ctx context.Context, hostID string, h host.Host, except string) []lint.PortUse {
	rep, err := s.hostPorts(ctx, hostID, h, false)
	if err != nil {
		return nil
	}
	return portUses(rep, except)
}

func (s *Server) lintStack(ctx context.Context, hostID string, h host.Host, name, compose, env string, deploy bool) []lint.Issue {
	// The port check runs on every validation (live editing too): port-taken stays a
	// deploy error, so it warns while typing and only blocks Deploy.
	o := lint.Options{Name: name, AllowLatest: name == addonsStackName}
	o.Running = s.runningPorts(ctx, hostID, h, name)
	issues := lint.Lint(ctx, compose, env, o)
	if issues == nil {
		issues = []lint.Issue{}
	}
	return issues
}

// validate checks a draft: the host's own compose validation (as before) plus the linter.
func (s *Server) validate(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	var req lintReq
	if !readJSON(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	issues := s.lintStack(r.Context(), r.PathValue("host"), h, name, req.Compose, req.Env, req.Deploy)
	err := h.Validate(r.Context(), name, req.Compose, req.Env)
	var ve engine.ValidationError
	if errors.As(err, &ve) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": ve.Msg, "issues": issues})
		return
	}
	if err != nil {
		s.engErr(w, err)
		return
	}
	if b := lint.Blocking(issues, req.Deploy); b != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": b.Message, "issues": issues})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "issues": issues})
}

// lintFix applies one quick fix to the draft and returns the new files (nothing is saved).
func (s *Server) lintFix(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Compose string   `json:"compose"`
		Env     string   `json:"env"`
		Fix     lint.Fix `json:"fix"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Fix.Kind == "pin-tag" && strings.TrimSpace(req.Fix.TargetTag) == "" {
		img := lint.ServiceImage(req.Compose, req.Fix.Service)
		if img == "" {
			writeErr(w, http.StatusUnprocessableEntity, "Could not read service image from compose.yaml.")
			return
		}
		regHost, _, _ := engine.ParseImageRef(img)
		var creds engine.RegistryCreds
		regs, _ := s.effectiveRegistries(r.Context())
		for _, rg := range regs {
			if strings.EqualFold(rg.Server, regHost) || (regHost == "registry-1.docker.io" && rg.Server == "docker.io") {
				creds = engine.RegistryCreds{Username: rg.Username, Password: rg.Password, ServerAddress: rg.Server}
				break
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		tag, err := engine.LatestStableTagFromRegistry(ctx, img, creds, nil)
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, fmt.Sprintf("Could not find a stable version tag for %s (%v); edit the image: line manually.", img, err))
			return
		}
		req.Fix.TargetTag = tag
	}
	res, err := lint.Apply(req.Compose, req.Env, req.Fix)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// extractEnv moves literal environment values of the draft into .env (nothing is saved).
// Conflicts (the key is in .env with another value) stay in compose.yaml unless overwrite.
func (s *Server) extractEnv(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Compose   string `json:"compose"`
		Env       string `json:"env"`
		Overwrite bool   `json:"overwrite"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	res, err := lint.MoveEnv(req.Compose, req.Env, req.Overwrite)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---- deploy strategy -------------------------------------------------------------

func (s *Server) getStackStrategy(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.StackStrategy(r.Context(), r.PathValue("host"), r.PathValue("name"))
	if err != nil {
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"strategy": st})
}

func (s *Server) putStackStrategy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.hostOf(w, r, true); !ok {
		return
	}
	var req struct {
		Strategy string `json:"strategy"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Strategy != store.StrategyStandard && req.Strategy != store.StrategyRolling {
		writeErr(w, http.StatusBadRequest, "Pick standard or rolling.")
		return
	}
	if err := s.store.SetStackStrategy(r.Context(), r.PathValue("host"), r.PathValue("name"), req.Strategy); err != nil {
		s.engErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deployAction resolves the action a deploy really runs: "up" becomes "rolling" for a
// stack with the rolling strategy (and "update" "rolling-update"), unless the request asks
// for one strategy just this once (?strategy=standard|rolling).
func (s *Server) deployAction(ctx context.Context, hostID, stack, action, override string) string {
	if action != "up" && action != "update" && action != "rolling" && action != "rolling-update" {
		return action
	}
	strategy := override
	if strategy == "" {
		if action == "rolling" || action == "rolling-update" {
			return action
		}
		strategy, _ = s.store.StackStrategy(ctx, hostID, stack)
	}
	pull := action == "update" || action == "rolling-update"
	switch {
	case strategy == store.StrategyRolling && pull:
		return "rolling-update"
	case strategy == store.StrategyRolling:
		return "rolling"
	case pull:
		return "update"
	}
	return "up"
}

// deployBlocked runs the linter with the deploy rules over the stack as saved on the host.
// It returns the first blocking issue, nil when the deploy may go ahead (or the stack has
// no compose file Moorage can read).
func (s *Server) deployBlocked(ctx context.Context, hostID string, h host.Host, stack string) *lint.Issue {
	st, err := h.GetStack(ctx, stack)
	if err != nil || !st.Managed || st.Compose == "" {
		return nil
	}
	return lint.Blocking(s.lintStack(ctx, hostID, h, stack, st.Compose, st.Env, true), true)
}

// watchRollback follows a rolling deploy: when it put the previous container back, it
// writes the Logbook entry and sends an alert (auto-updates also switch to notify, in the
// updater).
func (s *Server) watchRollback(h host.Host, hostID, stack, opID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
		defer cancel()
		var opErr string
		_ = h.FollowOp(ctx, opID, func(ev host.OpEvent) error {
			if ev.Done {
				opErr = ev.Error
			}
			return nil
		})
		if !strings.Contains(opErr, engine.ErrRolledBack.Error()) {
			return
		}
		if s.mon != nil {
			s.mon.StackEvent(hostID, stack, "rollback", "moorage", "Auto-rollback after a failed rolling deploy: "+opErr)
		}
		_ = s.store.Audit(ctx, store.AuditEntry{At: time.Now(), Actor: store.AuditActorSystem, Action: "stack.auto_rollback",
			HostID: hostID, Stack: stack, Target: opErr, Outcome: "ok"})
		s.notifyEvent(ctx, notify.EventDeployFailed, notify.Message{
			Status: "down",
			Event:  notify.EventRollback,
			HostID: hostID,
			Stack:  stack,
			Cause:  opErr,
			Title:  fmt.Sprintf("Auto-rollback: %s", stack),
			Text:   fmt.Sprintf("The rolling deploy of %s failed and the previous container was put back: %s", stack, opErr),
			URL:    s.stackLink(hostID, stack),
		})
	}()
}
