package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
	if req.Deploy {
		_, _ = h.RunAction(r.Context(), name, "up", user)
	}
	setAuditTarget(r, name)
	writeJSON(w, http.StatusOK, ver)
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
	go func() {
		t := time.NewTicker(1 * time.Minute)
		defer t.Stop()
		lastDay := ""
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				enabled, _ := s.store.GetSetting(ctx, store.SettingCleanupEnabled)
				if enabled != "true" {
					continue
				}
				sched, _ := s.store.GetSetting(ctx, store.SettingCleanupSchedule)
				if sched == "" {
					sched = "03:30"
				}
				day := now.Format("2006-01-02")
				if day == lastDay || now.Format("15:04") != sched {
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

type backupManifest struct {
	Format     int       `json:"format"`
	ExportedAt time.Time `json:"exportedAt"`
	Version    string    `json:"version"`
}

func (s *Server) exportBackup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tmpDB := filepath.Join(os.TempDir(), fmt.Sprintf("moorage-export-%d.db", time.Now().UnixNano()))
	defer os.Remove(tmpDB)

	if err := s.store.BackupTo(ctx, tmpDB); err != nil {
		s.engErr(w, err)
		return
	}
	dbBytes, err := os.ReadFile(tmpDB)
	if err != nil {
		s.engErr(w, err)
		return
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	addFile := func(name string, data []byte) error {
		hdr := &tar.Header{
			Name:    name,
			Mode:    0o600,
			Size:    int64(len(data)),
			ModTime: time.Now(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}

	manifestBytes, _ := json.MarshalIndent(backupManifest{
		Format:     1,
		ExportedAt: time.Now().UTC(),
		Version:    s.version,
	}, "", "  ")
	if err := addFile("manifest.json", manifestBytes); err != nil {
		s.engErr(w, err)
		return
	}
	if err := addFile("moorage.db", dbBytes); err != nil {
		s.engErr(w, err)
		return
	}

	if s.reg != nil {
		if hosts, err := s.reg.List(ctx); err == nil {
			for _, meta := range hosts {
				if !meta.Connected {
					continue
				}
				h, _, err := s.reg.Get(ctx, meta.ID)
				if err != nil {
					continue
				}
				stacks, err := h.ListStacks(ctx)
				if err != nil {
					continue
				}
				for _, st := range stacks {
					if st.ReadOnly {
						continue
					}
					det, err := h.GetStack(ctx, st.Name)
					if err != nil {
						continue
					}
					prefix := "stacks/" + meta.ID + "/" + st.Name + "/"
					if det.Compose != "" {
						_ = addFile(prefix+"compose.yaml", []byte(det.Compose))
					}
					if det.Env != "" {
						_ = addFile(prefix+".env", []byte(det.Env))
					}
				}
			}
		}
	}

	if err := tw.Close(); err != nil {
		s.engErr(w, err)
		return
	}
	if err := gz.Close(); err != nil {
		s.engErr(w, err)
		return
	}

	setAuditTarget(r, "backup")
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="moorage-backup-%s.tar.gz"`, time.Now().Format("2006-01-02")))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) importBackup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	const maxUpload = 64 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)

	gz, err := gzip.NewReader(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid gzip archive: "+err.Error())
		return
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var hasManifest bool
	var dbData []byte
	type stackFiles struct {
		compose string
		env     string
	}
	stacks := map[string]map[string]*stackFiles{} // hostID -> stackName -> files

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, "Corrupt tar archive: "+err.Error())
			return
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Size > 32<<20 {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, 32<<20))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		switch {
		case hdr.Name == "manifest.json":
			var m backupManifest
			if err := json.Unmarshal(data, &m); err != nil || m.Format != 1 {
				writeErr(w, http.StatusBadRequest, "Unsupported backup manifest.")
				return
			}
			hasManifest = true
		case hdr.Name == "moorage.db":
			dbData = data
		case strings.HasPrefix(hdr.Name, "stacks/"):
			parts := strings.Split(strings.TrimPrefix(hdr.Name, "stacks/"), "/")
			if len(parts) != 3 {
				continue
			}
			hostID, stName, file := parts[0], parts[1], parts[2]
			if stacks[hostID] == nil {
				stacks[hostID] = map[string]*stackFiles{}
			}
			if stacks[hostID][stName] == nil {
				stacks[hostID][stName] = &stackFiles{}
			}
			if file == "compose.yaml" {
				stacks[hostID][stName].compose = string(data)
			} else if file == ".env" {
				stacks[hostID][stName].env = string(data)
			}
		}
	}

	if !hasManifest || len(dbData) == 0 {
		writeErr(w, http.StatusBadRequest, "Archive is missing manifest.json or moorage.db.")
		return
	}

	// Stage database restore as moorage.db.restore next to moorage.db so next restart swaps it atomically,
	// and restore stack definitions to connected writable hosts immediately.
	restorePath := filepath.Join(s.cfg.DataDir, "moorage.db.restore")
	if err := os.WriteFile(restorePath, dbData, 0o600); err != nil {
		s.engErr(w, err)
		return
	}

	restoredStacks := 0
	user := currentUser(r).Username
	if s.reg != nil {
		for hostID, hostStacks := range stacks {
			h, meta, err := s.reg.Get(ctx, hostID)
			if err != nil || meta.ReadOnly {
				continue
			}
			for stName, files := range hostStacks {
				if files.compose == "" {
					continue
				}
				if err := h.Save(ctx, stName, files.compose, files.env, user, false); err == nil {
					restoredStacks++
				}
			}
		}
	}

	setAuditTarget(r, fmt.Sprintf("backup (%d stacks)", restoredStacks))
	writeJSON(w, http.StatusOK, map[string]any{
		"restoredStacks": restoredStacks,
		"dbStaged":       true,
		"message":        "Stack definitions restored. Restart Moorage to activate the imported database.",
	})
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
