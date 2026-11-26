package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/stacksnap"
	"moorage/internal/store"
)

// ---- stack snapshots (M18F) -------------------------------------------------------
//
// The files and manifests live on the host where the stack runs; these handlers pass
// through to it. Creating, restoring, deleting and downloading are admin actions in the
// audit log and the Logbook; viewers see the list.

// snapshotSettings are per stack (snapshot before updates, chosen external mounts) plus
// the global retention and free space limits (Settings → General).
type snapshotSettings struct {
	BeforeUpdates bool     `json:"beforeUpdates"`
	ExtraBinds    []string `json:"extraBinds"`
	RetainAuto    int      `json:"retainAuto"`
	MinFreePct    int      `json:"minFreePct"`
}

const (
	settingSnapRetain  = "snapshot_retain_auto"
	settingSnapMinFree = "snapshot_min_free_pct"
)

func stackSnapKey(hostID, stack string) string { return "stack_snapshot:" + hostID + "/" + stack }

func (s *Server) loadSnapshotSettings(ctx context.Context, hostID, stack string) snapshotSettings {
	out := snapshotSettings{ExtraBinds: []string{}, RetainAuto: engine.SnapshotRetainAuto, MinFreePct: engine.SnapshotMinFreePct}
	if raw, err := s.store.GetSetting(ctx, stackSnapKey(hostID, stack)); err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	if v, err := s.store.GetSetting(ctx, settingSnapRetain); err == nil {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			out.RetainAuto = n
		}
	}
	if v, err := s.store.GetSetting(ctx, settingSnapMinFree); err == nil {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			out.MinFreePct = n
		}
	}
	if out.ExtraBinds == nil {
		out.ExtraBinds = []string{}
	}
	return out
}

// snapshotter returns the host's snapshot support or answers why there is none.
func (s *Server) snapshotter(w http.ResponseWriter, r *http.Request, write bool) (host.Snapshotter, bool) {
	h, ok := s.hostOf(w, r, write)
	if !ok {
		return nil, false
	}
	sn, ok := h.(host.Snapshotter)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "This host doesn't support snapshots.")
		return nil, false
	}
	return sn, true
}

func (s *Server) snapErr(w http.ResponseWriter, err error) {
	switch {
	case host.IsUnsupported(err):
		writeErr(w, http.StatusNotImplemented, "Update the agent to use snapshots.")
	case err == engine.ErrSelfStack || strings.Contains(err.Error(), engine.ErrSelfStack.Error()):
		writeErr(w, http.StatusConflict, "Moorage's own stack is covered by the .mbk backup, not by snapshots.")
	case strings.Contains(err.Error(), engine.ErrSnapshotSpace.Error()):
		writeErr(w, http.StatusInsufficientStorage, capitalize(err.Error())+".")
	default:
		s.engErr(w, err)
	}
}

func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	sn, ok := s.snapshotter(w, r, false)
	if !ok {
		return
	}
	list, err := sn.ListSnapshots(r.Context(), r.PathValue("name"))
	if err != nil {
		s.snapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshots": list,
		"settings":  s.loadSnapshotSettings(r.Context(), r.PathValue("host"), r.PathValue("name")),
	})
}

func (s *Server) snapshotPlan(w http.ResponseWriter, r *http.Request) {
	sn, ok := s.snapshotter(w, r, false)
	if !ok {
		return
	}
	plan, err := sn.SnapshotPlan(r.Context(), r.PathValue("name"), r.URL.Query().Get("service"))
	if err != nil {
		s.snapErr(w, err)
		return
	}
	if plan.Sources == nil {
		plan.Sources = []engine.SnapshotSource{}
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) createSnapshot(w http.ResponseWriter, r *http.Request) {
	sn, ok := s.snapshotter(w, r, true)
	if !ok {
		return
	}
	var req engine.SnapshotRequest
	if !readJSON(w, r, &req) {
		return
	}
	hostID, stack := r.PathValue("host"), r.PathValue("name")
	set := s.loadSnapshotSettings(r.Context(), hostID, stack)
	req.Kind = stacksnap.KindManual
	req.Version, req.Author = s.version, currentUser(r).Username
	req.MinFreePct, req.RetainAuto = set.MinFreePct, set.RetainAuto
	if len(req.Note) > 200 {
		req.Note = req.Note[:200]
	}
	op, err := sn.CreateSnapshot(r.Context(), stack, req)
	if err != nil {
		s.snapErr(w, err)
		return
	}
	setAuditStack(r, stack)
	s.stackEvent(r, stack, "snapshot", "Snapshot ("+req.Mode+")")
	writeJSON(w, http.StatusAccepted, op)
}

func (s *Server) restoreSnapshot(w http.ResponseWriter, r *http.Request) {
	sn, ok := s.snapshotter(w, r, true)
	if !ok {
		return
	}
	var req engine.RestoreRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.Author, req.Version = currentUser(r).Username, s.version
	stack, id := r.PathValue("name"), r.PathValue("id")
	op, err := sn.RestoreSnapshot(r.Context(), stack, id, req)
	if err != nil {
		s.snapErr(w, err)
		return
	}
	setAuditStack(r, stack)
	setAuditTarget(r, id)
	s.stackEvent(r, stack, "restore", "Restore snapshot "+id+" ("+req.Scope+")")
	writeJSON(w, http.StatusAccepted, op)
}

func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	sn, ok := s.snapshotter(w, r, true)
	if !ok {
		return
	}
	stack, id := r.PathValue("name"), r.PathValue("id")
	if err := sn.DeleteSnapshot(r.Context(), stack, id); err != nil {
		s.snapErr(w, err)
		return
	}
	setAuditStack(r, stack)
	setAuditTarget(r, id)
	s.stackEvent(r, stack, "snapshot", "Deleted snapshot "+id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pinSnapshot(w http.ResponseWriter, r *http.Request) {
	sn, ok := s.snapshotter(w, r, true)
	if !ok {
		return
	}
	var req struct {
		Pinned bool `json:"pinned"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := sn.PinSnapshot(r.Context(), r.PathValue("name"), r.PathValue("id"), req.Pinned); err != nil {
		s.snapErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// snapshotPreview is the restore preview: the configuration in the snapshot next to the
// current one (the UI shows the diff).
func (s *Server) snapshotPreview(w http.ResponseWriter, r *http.Request) {
	sn, ok := s.snapshotter(w, r, false)
	if !ok {
		return
	}
	h, _ := s.hostOf(w, r, false)
	stack, id := r.PathValue("name"), r.PathValue("id")
	compose, env, err := sn.SnapshotConfig(r.Context(), stack, id)
	if err != nil {
		s.snapErr(w, err)
		return
	}
	out := map[string]any{"compose": compose, "env": env, "currentCompose": "", "currentEnv": ""}
	if st, err := h.GetStack(r.Context(), stack); err == nil {
		out["currentCompose"], out["currentEnv"] = st.Compose, st.Env
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) downloadSnapshot(w http.ResponseWriter, r *http.Request) {
	sn, ok := s.snapshotter(w, r, false)
	if !ok {
		return
	}
	stack, id := r.PathValue("name"), r.PathValue("id")
	list, err := sn.ListSnapshots(r.Context(), stack)
	if err != nil {
		s.snapErr(w, err)
		return
	}
	var m *stacksnap.Manifest
	for i := range list {
		if list[i].ID == id {
			m = &list[i]
		}
	}
	if m == nil {
		writeErr(w, http.StatusNotFound, "Snapshot not found.")
		return
	}
	// a GET is not audited by the middleware: the download is recorded here
	s.audit(context.WithoutCancel(r.Context()), store.AuditEntry{Actor: currentUser(r).Username, IP: clientIP(r),
		Action: "stack.snapshot_download", HostID: r.PathValue("host"), Stack: stack, Target: id, Outcome: store.AuditOK})
	w.Header().Set("Content-Type", "application/zstd")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.tar.zst"`, stack, id))
	if m.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(m.Size, 10))
	}
	if _, err := sn.DownloadSnapshot(r.Context(), stack, id, w); err != nil {
		s.log.Warn("snapshot download", "stack", stack, "id", id, "err", err)
	}
}

func (s *Server) putSnapshotSettings(w http.ResponseWriter, r *http.Request) {
	var req snapshotSettings
	if !readJSON(w, r, &req) {
		return
	}
	if req.RetainAuto < 1 || req.RetainAuto > 100 || req.MinFreePct < 0 || req.MinFreePct > 90 {
		writeErr(w, http.StatusBadRequest, "Keep 1–100 automatic snapshots and 0–90 % free space.")
		return
	}
	if len(req.ExtraBinds) > 50 {
		writeErr(w, http.StatusBadRequest, "Too many extra mounts.")
		return
	}
	ctx := r.Context()
	hostID, stack := r.PathValue("host"), r.PathValue("name")
	per, _ := json.Marshal(struct {
		BeforeUpdates bool     `json:"beforeUpdates"`
		ExtraBinds    []string `json:"extraBinds"`
	}{req.BeforeUpdates, req.ExtraBinds})
	for k, v := range map[string]string{
		stackSnapKey(hostID, stack): string(per),
		settingSnapRetain:           strconv.Itoa(req.RetainAuto),
		settingSnapMinFree:          strconv.Itoa(req.MinFreePct),
	} {
		if err := s.store.SetSetting(ctx, k, v); err != nil {
			s.engErr(w, err)
			return
		}
	}
	setAuditStack(r, stack)
	writeJSON(w, http.StatusOK, s.loadSnapshotSettings(ctx, hostID, stack))
}

// SnapshotBeforeUpdate takes an automatic snapshot before an image update when the
// stack has "Snapshot before updates" on. ok is false when it is off; an error means the
// update must wait (the admin turned the safeguard on on purpose).
func (s *Server) SnapshotBeforeUpdate(ctx context.Context, hostID, stack string) (id string, on bool, err error) {
	set := s.loadSnapshotSettings(ctx, hostID, stack)
	if !set.BeforeUpdates || s.reg == nil {
		return "", false, nil
	}
	h, _, err := s.reg.Get(ctx, hostID)
	if err != nil {
		return "", true, err
	}
	sn, ok := h.(host.Snapshotter)
	if !ok {
		return "", true, fmt.Errorf("the host doesn't support snapshots")
	}
	op, err := sn.CreateSnapshot(ctx, stack, engine.SnapshotRequest{Mode: stacksnap.ModePause, Kind: stacksnap.KindAuto,
		ExtraBinds: set.ExtraBinds, Version: s.version, Note: "before update", MinFreePct: set.MinFreePct, RetainAuto: set.RetainAuto})
	if err != nil {
		return "", true, err
	}
	var opErr string
	if ferr := h.FollowOp(ctx, op.ID, func(ev host.OpEvent) error {
		if ev.Done {
			opErr = ev.Error
		}
		return nil
	}); ferr != nil {
		return "", true, ferr
	}
	if opErr != "" {
		return "", true, fmt.Errorf("%s", opErr)
	}
	list, _ := sn.ListSnapshots(ctx, stack)
	if len(list) > 0 {
		id = list[0].ID
	}
	return id, true, nil
}
