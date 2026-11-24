package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/store"
)

func (s *Server) listDeletedStacks(w http.ResponseWriter, r *http.Request) {
	h, code, msg := s.resolveHost(r, false)
	if code != 0 {
		writeErr(w, code, msg)
		return
	}
	ds, ok := h.(host.DeletedStacksSource)
	if !ok {
		writeJSON(w, http.StatusOK, []engine.DeletedStack{})
		return
	}
	list, err := ds.DeletedStacks(r.Context())
	if err != nil {
		if host.IsUnsupported(err) {
			writeJSON(w, http.StatusOK, []engine.DeletedStack{})
			return
		}
		s.engErr(w, err)
		return
	}
	if list == nil {
		list = []engine.DeletedStack{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) restoreDeletedStack(w http.ResponseWriter, r *http.Request) {
	h, code, msg := s.resolveHost(r, true)
	if code != 0 {
		writeErr(w, code, msg)
		return
	}
	name := r.PathValue("name")
	var req struct {
		Hash   string `json:"hash"`
		Deploy bool   `json:"deploy"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Hash) == "" {
		writeErr(w, http.StatusBadRequest, "Commit hash is required.")
		return
	}
	user := currentUser(r).Username
	ver, err := h.Restore(r.Context(), name, req.Hash, user)
	if err != nil {
		s.engErr(w, err)
		return
	}
	setAuditTarget(r, name)
	if req.Deploy {
		// The definition is back either way; a failed start is reported, not hidden.
		op, err := h.RunAction(r.Context(), name, "up", user)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"version": ver, "deployError": err.Error()})
			return
		}
		s.watchOp(h, r.PathValue("host"), name, "up", op.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": ver})
}

func (s *Server) prunePreview(w http.ResponseWriter, r *http.Request) {
	h, code, msg := s.resolveHost(r, false)
	if code != 0 {
		writeErr(w, code, msg)
		return
	}
	dp, ok := h.(host.DiskPruner)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "Disk prune preview is not supported on this host.")
		return
	}
	prev, err := dp.PrunePreview(r.Context())
	if err != nil {
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, prev)
}

func (s *Server) pruneDisk(w http.ResponseWriter, r *http.Request) {
	h, code, msg := s.resolveHost(r, true)
	if code != 0 {
		writeErr(w, code, msg)
		return
	}
	dp, ok := h.(host.DiskPruner)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "Disk prune is not supported on this host.")
		return
	}
	var req engine.PruneRequest
	if !readJSON(w, r, &req) {
		return
	}
	res, err := dp.PruneDisk(r.Context(), req)
	if err != nil {
		s.engErr(w, err)
		return
	}
	setAuditTarget(r, r.PathValue("host"))
	writeJSON(w, http.StatusOK, res)
}

// StartCleanupLoop runs daily automatic disk cleanup when enabled in General settings.
func (s *Server) StartCleanupLoop(ctx context.Context) {
	s.cleanBackupTemp()
	go func() {
		t := time.NewTicker(1 * time.Minute)
		defer t.Stop()
		lastDay := ""
		lastBackupDay := ""
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				day := now.Format("2006-01-02")
				hhmm := now.Format("15:04")

				// Scheduled remote encrypted backup (Settings -> Backups).
				if rb, _ := s.effectiveRemoteBackup(ctx); rb.Enabled && rb.Provider != "off" && day != lastBackupDay && hhmm == rb.Schedule {
					lastBackupDay = day
					bctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
					st, berr := s.executeRemoteBackup(bctx)
					cancel()
					outcome := store.AuditOK
					if berr != nil {
						outcome = store.AuditFailed
						if s.log != nil {
							s.log.Warn("scheduled remote backup failed", "err", berr)
						}
					}
					_ = s.store.Audit(ctx, store.AuditEntry{
						At: now, Actor: store.AuditActorSystem, Action: "backup.remote_run",
						Target: st.Target, Outcome: outcome,
					})
				}

				enabled, _ := s.store.GetSetting(ctx, store.SettingCleanupEnabled)
				if enabled != "true" {
					continue
				}
				sched, _ := s.store.GetSetting(ctx, store.SettingCleanupSchedule)
				if sched == "" {
					sched = "03:30"
				}
				if day == lastDay || hhmm != sched {
					continue
				}
				lastDay = day
				vols, _ := s.store.GetSetting(ctx, store.SettingCleanupVolumes)
				req := engine.PruneRequest{
					Images:     true,
					Containers: true,
					BuildCache: true,
					Volumes:    vols == "true",
				}
				s.runDailyCleanup(ctx, req)
			}
		}
	}()
}

func (s *Server) runDailyCleanup(ctx context.Context, req engine.PruneRequest) {
	if s.reg == nil {
		return
	}
	hosts, err := s.reg.List(ctx)
	if err != nil {
		return
	}
	for _, meta := range hosts {
		if !meta.Connected || meta.ReadOnly {
			continue
		}
		h, _, err := s.reg.Get(ctx, meta.ID)
		if err != nil {
			continue
		}
		dp, ok := h.(host.DiskPruner)
		if !ok {
			continue
		}
		res, err := dp.PruneDisk(ctx, req)
		if err != nil {
			if s.log != nil {
				s.log.Warn("daily disk cleanup failed", "host", meta.Name, "err", err)
			}
			continue
		}
		_ = s.store.Audit(ctx, store.AuditEntry{
			At:      time.Now(),
			Actor:   "system",
			Action:  "host.prune",
			HostID:  meta.ID,
			Target:  fmt.Sprintf("%s (%d B)", meta.Name, res.ReclaimedBytes),
			Outcome: "ok",
		})
	}
}

func (s *Server) listAutoRollbacks(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListUnackedAutoRollbacks(r.Context())
	if err != nil {
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) ackAutoRollback(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "Invalid rollback id.")
		return
	}
	if err := s.store.AckAutoRollback(r.Context(), id); err != nil {
		s.engErr(w, err)
		return
	}
	setAuditTarget(r, strconv.FormatInt(id, 10))
	w.WriteHeader(http.StatusNoContent)
}
