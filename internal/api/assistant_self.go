package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"

	"moorage/internal/assistant"
	"moorage/internal/host"
	"moorage/internal/store"
)

// ---- Moorage's own stack: proposal with Apply & restart (M19I) ---------------------
//
// An exception to the M18G ban the user approved: when a fix needs a change of
// Moorage's own compose file (mount /opt/dockge so Moorage reaches a stack), the
// assistant proposes it as a card with Apply & restart. Safeguards: Propose only (never
// Auto), admins only, the helper with rollback (M19A) and every Apply check (M18H,
// M19B), a prominent note for each new access to the host, and no image change (that is
// Moorage updates, M19C, and respects MOORAGE_SELF_UPDATE, M19G).

const draftSelfApply = "self_apply"

func (s *Server) hostName(ctx context.Context, hostID string) string {
	if n := s.hostNames(ctx)[hostID]; n != "" {
		return n
	}
	return hostID
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// assistantSelfProposal handles propose_compose on the self stack. handled=false means
// the stack isn't Moorage's own and the normal path continues.
func (s *Server) assistantSelfProposal(ctx context.Context, r *http.Request, set assistantSettings, in json.RawMessage, compose, note string, emit func(assistant.Event)) (string, bool, error) {
	hostID, stack, h, err := s.assistantStack(ctx, in)
	if err != nil {
		return "", false, nil
	}
	cur, err := h.GetStack(ctx, stack)
	if err != nil || !cur.Self {
		return "", false, nil
	}
	if currentUser(r).Role != "admin" {
		return "", true, errors.New("only an admin can change Moorage's own stack")
	}
	if _, meta, err := s.reg.Get(ctx, hostID); err == nil && meta.ReadOnly {
		return "", true, errors.New("the host is read-only (MOORAGE_READ_ONLY): change Moorage's compose file on the host itself")
	}
	if _, ok := h.(host.SelfApplier); !ok {
		return "", true, errors.New("this host can't apply changes to Moorage's own stack (update the agent)")
	}
	svc := cur.SelfService
	if svc == "" {
		return "", true, errors.New("this agent is too old to tell which service Moorage is")
	}
	oldP, err1 := loadDraftProject(ctx, stack, cur.Compose, cur.Env)
	newP, err2 := loadDraftProject(ctx, stack, compose, cur.Env)
	if err1 != nil || err2 != nil {
		if err2 != nil {
			return "", true, fmt.Errorf("the new compose.yaml doesn't load: %v", err2)
		}
		return "", true, fmt.Errorf("the current compose.yaml doesn't load: %v", err1)
	}
	if oldP.Services[svc].Image != newP.Services[svc].Image {
		return "", true, errors.New("changing the Moorage image is not allowed for the assistant: Moorage is updated through Moorage updates (Update Moorage)")
	}
	if why := s.selfApplyBlocked(r, hostID, h, stack, compose, cur.Env); why != "" {
		return "", true, errors.New("this configuration would be refused: " + why)
	}
	widens := selfWidens(oldP.Services[svc], newP.Services[svc])
	d := store.MCPDraft{TokenName: assistantDraftPrefix + currentUser(r).Username, Tool: draftSelfApply, HostID: hostID, Stack: stack,
		Compose: compose, Env: cur.Env, EnvKeys: []string{baseMarker(cur.Compose, cur.Env)}, Note: trimNote(note)}
	saved, err := s.store.CreateMCPDraft(ctx, d)
	if err != nil {
		return "", true, err
	}
	a := assistant.Action{DraftID: saved.ID, Kind: "self_apply", SelfApply: true, Title: "Change Moorage's own compose.yaml and restart Moorage",
		HostID: hostID, Host: s.hostName(ctx, hostID), Stack: stack, Diff: lineDiff(cur.Compose, compose), Widens: widens,
		Removes: selfRemoves(oldP.Services[svc], newP.Services[svc]),
		Impact: "Moorage restarts for a moment; if it doesn't come back, the configuration it runs now is put back.",
		Undo:   "Restore the previous version in the stack's History tab and use Apply & restart."}
	if set.Levels["compose"] == levelAuto {
		a.Demoted = true // never Auto for Moorage's own stack
	}
	s.log.Info("assistant proposed a self-stack change", "user", currentUser(r).Username, "host", hostID, "draft", saved.ID, "widens", len(widens))
	emit(assistant.Event{Type: "action", Action: &a})
	return "Proposed as a card with Apply & restart; the user applies it. Moorage's own stack is never changed automatically.", true, nil
}

func loadDraftProject(ctx context.Context, name, compose, env string) (*types.Project, error) {
	vars, err := dotenv.UnmarshalBytesWithLookup([]byte(env), nil)
	if err != nil {
		return nil, err
	}
	return loader.LoadWithContext(ctx, types.ConfigDetails{
		WorkingDir:  "/",
		ConfigFiles: []types.ConfigFile{{Filename: filepath.Join("/", "compose.yaml"), Content: []byte(compose)}},
		Environment: vars,
	}, func(o *loader.Options) {
		o.SetProjectName(name, true)
		o.SkipResolveEnvironment = true
		o.SkipValidation = true
	})
}

// selfWidens lists new access to the host the change gives Moorage: bind mounts,
// sockets, privileged, capabilities, devices, network and pid modes.
func selfWidens(old, nw types.ServiceConfig) []string {
	var out []string
	had := map[string]bool{}
	for _, v := range old.Volumes {
		if v.Type == types.VolumeTypeBind {
			had[v.Source] = true
		}
	}
	for _, v := range nw.Volumes {
		if v.Type != types.VolumeTypeBind || had[v.Source] {
			continue
		}
		what := "Gives Moorage access to " + v.Source + " on the host"
		if strings.HasSuffix(v.Source, ".sock") {
			what = "Gives Moorage the socket " + v.Source + " (full control of that service)"
		}
		if !v.ReadOnly {
			what += " (read and write)"
		}
		out = append(out, what)
	}
	if nw.Privileged && !old.Privileged {
		out = append(out, "Runs Moorage privileged: full access to the host")
	}
	oldCaps := map[string]bool{}
	for _, c := range old.CapAdd {
		oldCaps[c] = true
	}
	for _, c := range nw.CapAdd {
		if !oldCaps[c] {
			out = append(out, "Adds the capability "+c)
		}
	}
	if len(nw.Devices) > len(old.Devices) {
		out = append(out, "Gives Moorage host devices")
	}
	if nw.NetworkMode != old.NetworkMode && nw.NetworkMode != "" {
		out = append(out, "Changes the network to "+nw.NetworkMode)
	}
	if nw.Pid != old.Pid && nw.Pid != "" {
		out = append(out, "Shares the host's processes (pid: "+nw.Pid+")")
	}
	sort.Strings(out)
	return out
}

// selfRemoves lists what a self-stack change takes away: a model that rewrites the file
// can drop a mount it didn't understand, and Moorage then loses its stacks (M20F).
func selfRemoves(old, nw types.ServiceConfig) []string {
	var out []string
	vol := func(v types.ServiceVolumeConfig) string { return v.Source + ":" + v.Target }
	ports := func(p types.ServicePortConfig) string { return fmt.Sprintf("%s:%d/%s", p.Published, p.Target, p.Protocol) }
	keep := map[string]bool{}
	for _, v := range nw.Volumes {
		keep["v"+vol(v)] = true
	}
	for _, p := range nw.Ports {
		keep["p"+ports(p)] = true
	}
	for k := range nw.Environment {
		keep["e"+k] = true
	}
	for _, v := range old.Volumes {
		if !keep["v"+vol(v)] {
			out = append(out, "Removes the mount "+vol(v))
		}
	}
	for _, p := range old.Ports {
		if !keep["p"+ports(p)] {
			out = append(out, "Removes the port "+ports(p))
		}
	}
	for k := range old.Environment {
		if !keep["e"+k] {
			out = append(out, "Removes the variable "+k)
		}
	}
	sort.Strings(out)
	return out
}

// applySelfDraft runs Apply & restart for an approved self-stack proposal.
func (s *Server) applySelfDraft(w http.ResponseWriter, r *http.Request, d store.MCPDraft) {
	ctx := r.Context()
	h, meta, err := s.reg.Get(ctx, d.HostID)
	if err != nil {
		s.engErr(w, err)
		return
	}
	if meta.ReadOnly {
		writeErr(w, http.StatusForbidden, "The host is read-only.")
		return
	}
	sa, ok := h.(host.SelfApplier)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "This host can't apply changes to Moorage's own stack.")
		return
	}
	cur, err := h.GetStack(ctx, d.Stack)
	if err != nil || !cur.Self || len(d.EnvKeys) != 1 || baseMarker(cur.Compose, cur.Env) != d.EnvKeys[0] {
		writeErr(w, http.StatusConflict, "Moorage's stack changed since this was proposed. Ask the assistant again.")
		return
	}
	if why := s.selfApplyBlocked(r, d.HostID, h, d.Stack, d.Compose, cur.Env); why != "" {
		writeErr(w, http.StatusUnprocessableEntity, why)
		return
	}
	user := currentUser(r).Username
	if err := sa.SelfApply(ctx, d.Stack, d.Compose, cur.Env, user+" via assistant"); err != nil {
		s.engErr(w, err)
		return
	}
	_ = s.store.DecideMCPDraft(ctx, d.ID, "approved", user)
	s.audit(context.WithoutCancel(ctx), store.AuditEntry{Actor: user + " via assistant", IP: clientIP(r), Action: "assistant.self_apply",
		HostID: d.HostID, Stack: d.Stack, Target: "draft " + fmt.Sprint(d.ID), Outcome: store.AuditOK}) // the diff is in the stack history
	if s.mon != nil {
		s.mon.StackEvent(d.HostID, d.Stack, "up", user, "Apply & restart via assistant")
	}
	s.log.Info("assistant self-stack proposal applied", "user", user, "host", d.HostID, "draft", d.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "selfApply": true, "ownHub": d.HostID == host.LocalID})
}
