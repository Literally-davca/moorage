package api

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"moorage/internal/backupcrypt"
	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/store"
)

// Backup archive (format 2): encrypted (backupcrypt) gzip'd tar with
//
//	moorage.db                         the hub database (VACUUM INTO copy)
//	stacks/<host id>/<stack>/compose.yaml, .env
//	history/<host id>.tar              that host's history.git
//	manifest.json                      written last, once the content is known
const (
	backupFormat     = 2
	maxImportBytes   = 2 << 30 // 2 GiB, histories included
	maxImportEntry   = 1 << 30
	importStagingTTL = 30 * time.Minute
)

type backupManifestHost struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Stacks  []string `json:"stacks"`
	History bool     `json:"history"`
}

type backupManifest struct {
	Format        int                  `json:"format"`
	ExportedAt    time.Time            `json:"exportedAt"`
	Version       string               `json:"version"`
	SchemaVersion int                  `json:"schemaVersion"`
	Hosts         []backupManifestHost `json:"hosts"`
	Skipped       []string             `json:"skipped,omitempty"` // hosts that were offline or failed
}

// ---- export -----------------------------------------------------------------

func (s *Server) exportBackup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := backupcrypt.CheckPassword(req.Password); err != nil {
		writeErr(w, http.StatusBadRequest, capitalize(err.Error())+".")
		return
	}
	ctx := r.Context()
	tmpDir, err := os.MkdirTemp(s.cfg.DataDir, "export-")
	if err != nil {
		s.engErr(w, err)
		return
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "moorage.db")
	if err := s.store.BackupTo(ctx, dbPath); err != nil {
		s.engErr(w, err)
		return
	}

	setAuditTarget(r, "backup")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="moorage-backup-%s.mbk"`, time.Now().Format("2006-01-02")))
	w.Header().Set("Cache-Control", "no-store")

	// From here on the status is 200: a failure cuts the stream, and the missing last
	// chunk makes the file unreadable instead of silently incomplete.
	enc, err := backupcrypt.NewWriter(w, req.Password)
	if err != nil {
		s.engErr(w, err)
		return
	}
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)
	if err := s.writeBackup(ctx, tw, tmpDir, dbPath); err != nil {
		s.log.Error("backup export failed", "err", err)
		return
	}
	if err := tw.Close(); err != nil {
		return
	}
	if err := gz.Close(); err != nil {
		return
	}
	_ = enc.Close()
}

func (s *Server) writeBackup(ctx context.Context, tw *tar.Writer, tmpDir, dbPath string) error {
	m := backupManifest{Format: backupFormat, ExportedAt: time.Now().UTC(), Version: s.version, SchemaVersion: store.SchemaVersion()}
	if err := tarFile(tw, "moorage.db", dbPath); err != nil {
		return err
	}
	if s.reg != nil {
		hosts, err := s.reg.List(ctx)
		if err != nil {
			return err
		}
		for _, meta := range hosts {
			if !meta.Connected {
				m.Skipped = append(m.Skipped, meta.Name+" (offline)")
				continue
			}
			mh, err := s.backupHost(ctx, tw, tmpDir, meta)
			if err != nil {
				m.Skipped = append(m.Skipped, fmt.Sprintf("%s (%v)", meta.Name, err))
				continue
			}
			m.Hosts = append(m.Hosts, mh)
		}
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return tarBytes(tw, "manifest.json", b)
}

func (s *Server) backupHost(ctx context.Context, tw *tar.Writer, tmpDir string, meta host.Entry) (backupManifestHost, error) {
	mh := backupManifestHost{ID: meta.ID, Name: meta.Name, Stacks: []string{}}
	h, _, err := s.reg.Get(ctx, meta.ID)
	if err != nil {
		return mh, err
	}
	stacks, err := h.ListStacks(ctx)
	if err != nil {
		return mh, err
	}
	for _, st := range stacks {
		if !st.Managed {
			continue
		}
		det, err := h.GetStack(ctx, st.Name)
		if err != nil || det.Compose == "" {
			continue
		}
		prefix := "stacks/" + meta.ID + "/" + st.Name + "/"
		if err := tarBytes(tw, prefix+"compose.yaml", []byte(det.Compose)); err != nil {
			return mh, err
		}
		if det.Env != "" {
			if err := tarBytes(tw, prefix+".env", []byte(det.Env)); err != nil {
				return mh, err
			}
		}
		mh.Stacks = append(mh.Stacks, st.Name)
	}
	if he, ok := h.(host.HistoryExporter); ok {
		// tar needs the size up front, so the history goes through a temp file.
		histPath := filepath.Join(tmpDir, "history-"+meta.ID+".tar")
		f, err := os.Create(histPath)
		if err != nil {
			return mh, err
		}
		herr := he.ExportHistory(ctx, f)
		f.Close()
		if herr == nil {
			if err := tarFile(tw, "history/"+meta.ID+".tar", histPath); err != nil {
				return mh, err
			}
			mh.History = true
		} else if !host.IsUnsupported(herr) && !errors.Is(herr, engine.ErrHistoryOff) {
			s.log.Error("backup: history export failed", "host", meta.Name, "err", herr)
		}
		os.Remove(histPath)
	}
	return mh, nil
}

func tarBytes(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now()}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func tarFile(tw *tar.Writer, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: fi.Size(), ModTime: time.Now()}); err != nil {
		return err
	}
	_, err = io.CopyN(tw, f, fi.Size())
	return err
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---- import: upload, check, preview ----------------------------------------

// importStaging is an uploaded, decrypted and checked backup waiting for confirmation.
type importStaging struct {
	dir      string
	manifest backupManifest
	stacks   map[string]map[string]bool // host id -> stack -> has .env
	expires  time.Time
}

type importState struct {
	mu      sync.Mutex
	staging map[string]*importStaging
}

var tokenRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (s *Server) imports() *importState {
	s.importOnce.Do(func() { s.importSt = &importState{staging: map[string]*importStaging{}} })
	return s.importSt
}

// dropExpired removes staged imports past their TTL. Callers hold mu.
func (st *importState) dropExpired(now time.Time) {
	for tok, sg := range st.staging {
		if now.After(sg.expires) {
			os.RemoveAll(sg.dir)
			delete(st.staging, tok)
		}
	}
}

type importPreviewStack struct {
	Name   string `json:"name"`
	Exists bool   `json:"exists"` // a stack of this name is on the host now (it gets replaced)
}

type importPreviewHost struct {
	ID       string               `json:"id"`
	Name     string               `json:"name"`
	Known    bool                 `json:"known"`    // registered on this hub
	Writable bool                 `json:"writable"` // connected and not read-only
	History  bool                 `json:"history"`
	Stacks   []importPreviewStack `json:"stacks"`
}

type importPreview struct {
	Token      string              `json:"token"`
	ExportedAt time.Time           `json:"exportedAt"`
	Version    string              `json:"version"`
	Database   store.BackupInfo    `json:"database"`
	Hosts      []importPreviewHost `json:"hosts"`
	Skipped    []string            `json:"skipped,omitempty"`
}

func (s *Server) importBackupPreview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	password := r.Header.Get("X-Backup-Password")
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBytes)

	dec, err := backupcrypt.NewReader(r.Body, password)
	if err != nil {
		writeErr(w, http.StatusBadRequest, capitalize(err.Error())+".")
		return
	}
	gz, err := gzip.NewReader(dec)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "The backup content is damaged.")
		return
	}
	defer gz.Close()

	dir, err := os.MkdirTemp(s.cfg.DataDir, "import-")
	if err != nil {
		s.engErr(w, err)
		return
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(dir)
		}
	}()

	sg := &importStaging{dir: dir, stacks: map[string]map[string]bool{}}
	var haveManifest, haveDB bool
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, "The backup content is damaged: "+err.Error())
			return
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Size < 0 || hdr.Size > maxImportEntry {
			continue
		}
		switch name := hdr.Name; {
		case name == "manifest.json":
			b, err := io.ReadAll(io.LimitReader(tr, 1<<20))
			if err != nil || json.Unmarshal(b, &sg.manifest) != nil || sg.manifest.Format != backupFormat {
				writeErr(w, http.StatusBadRequest, "The backup has an unsupported format.")
				return
			}
			haveManifest = true
		case name == "moorage.db":
			if err := saveEntry(tr, filepath.Join(dir, "moorage.db"), hdr.Size); err != nil {
				s.engErr(w, err)
				return
			}
			haveDB = true
		case strings.HasPrefix(name, "stacks/"):
			parts := strings.Split(strings.TrimPrefix(name, "stacks/"), "/")
			if len(parts) != 3 || !hostIDRe.MatchString(parts[0]) || !stackNameRe.MatchString(parts[1]) ||
				(parts[2] != "compose.yaml" && parts[2] != ".env") || hdr.Size > engine.MaxFileSize {
				continue
			}
			if err := os.MkdirAll(filepath.Join(dir, "stacks", parts[0], parts[1]), 0o700); err != nil {
				s.engErr(w, err)
				return
			}
			if err := saveEntry(tr, filepath.Join(dir, "stacks", parts[0], parts[1], parts[2]), hdr.Size); err != nil {
				s.engErr(w, err)
				return
			}
			if sg.stacks[parts[0]] == nil {
				sg.stacks[parts[0]] = map[string]bool{}
			}
			if parts[2] == ".env" {
				sg.stacks[parts[0]][parts[1]] = true
			} else if _, ok := sg.stacks[parts[0]][parts[1]]; !ok {
				sg.stacks[parts[0]][parts[1]] = false
			}
		}
		// history/<host>.tar is kept in the file for recovery by hand; it isn't imported.
	}
	if !haveManifest || !haveDB {
		writeErr(w, http.StatusBadRequest, "The backup is incomplete (no manifest or database).")
		return
	}
	info, err := store.InspectBackup(ctx, filepath.Join(dir, "moorage.db"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, capitalize(err.Error())+".")
		return
	}

	prev := importPreview{ExportedAt: sg.manifest.ExportedAt, Version: sg.manifest.Version, Database: info, Skipped: sg.manifest.Skipped, Hosts: []importPreviewHost{}}
	history := map[string]bool{}
	names := map[string]string{}
	for _, mh := range sg.manifest.Hosts {
		history[mh.ID] = mh.History
		names[mh.ID] = mh.Name
	}
	ids := make([]string, 0, len(sg.stacks))
	for id := range sg.stacks {
		ids = append(ids, id)
	}
	for id := range history {
		if _, ok := sg.stacks[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		ph := importPreviewHost{ID: id, Name: names[id], History: history[id], Stacks: []importPreviewStack{}}
		current := map[string]bool{}
		if s.reg != nil {
			if h, meta, err := s.reg.Get(ctx, id); err == nil {
				ph.Known = true
				ph.Writable = meta.Connected && !meta.ReadOnly
				if ph.Name == "" {
					ph.Name = meta.Name
				}
				if ph.Writable {
					if list, err := h.ListStacks(ctx); err == nil {
						for _, st := range list {
							current[st.Name] = true
						}
					}
				}
			}
		}
		if ph.Name == "" {
			ph.Name = id
		}
		stackNames := make([]string, 0, len(sg.stacks[id]))
		for n := range sg.stacks[id] {
			stackNames = append(stackNames, n)
		}
		sort.Strings(stackNames)
		for _, n := range stackNames {
			ph.Stacks = append(ph.Stacks, importPreviewStack{Name: n, Exists: current[n]})
		}
		prev.Hosts = append(prev.Hosts, ph)
	}

	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		s.engErr(w, err)
		return
	}
	prev.Token = hex.EncodeToString(tok)
	sg.expires = time.Now().Add(importStagingTTL)

	st := s.imports()
	st.mu.Lock()
	st.dropExpired(time.Now())
	// One pending import at a time: a new upload replaces the previous one.
	for t, old := range st.staging {
		os.RemoveAll(old.dir)
		delete(st.staging, t)
	}
	st.staging[prev.Token] = sg
	st.mu.Unlock()
	keep = true
	writeJSON(w, http.StatusOK, prev)
}

var hostIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// cleanBackupTemp removes export/import temp dirs a crash or restart left behind.
func (s *Server) cleanBackupTemp() {
	for _, pat := range []string{"export-*", "import-*"} {
		if m, err := filepath.Glob(filepath.Join(s.cfg.DataDir, pat)); err == nil {
			for _, d := range m {
				os.RemoveAll(d)
			}
		}
	}
}

func saveEntry(r io.Reader, path string, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(f, r, size); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ---- import: apply ----------------------------------------------------------

func (s *Server) importBackupApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token    string `json:"token"`
		Database bool   `json:"database"`
		Stacks   bool   `json:"stacks"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !tokenRe.MatchString(req.Token) {
		writeErr(w, http.StatusBadRequest, "Upload the backup again.")
		return
	}
	if !req.Database && !req.Stacks {
		writeErr(w, http.StatusBadRequest, "Choose what to restore.")
		return
	}
	st := s.imports()
	st.mu.Lock()
	st.dropExpired(time.Now())
	sg := st.staging[req.Token]
	delete(st.staging, req.Token)
	st.mu.Unlock()
	if sg == nil {
		writeErr(w, http.StatusGone, "The uploaded backup expired. Upload it again.")
		return
	}
	defer os.RemoveAll(sg.dir)

	ctx := r.Context()
	user := currentUser(r).Username
	restored := 0
	failed := []string{}
	if req.Stacks && s.reg != nil {
		for hostID, stacks := range sg.stacks {
			h, meta, err := s.reg.Get(ctx, hostID)
			if err != nil || !meta.Connected || meta.ReadOnly {
				for n := range stacks {
					failed = append(failed, n+" (host unavailable)")
				}
				continue
			}
			current := map[string]bool{}
			if list, err := h.ListStacks(ctx); err == nil {
				for _, x := range list {
					current[x.Name] = true
				}
			}
			for n, hasEnv := range stacks {
				dir := filepath.Join(sg.dir, "stacks", hostID, n)
				compose, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
				if err != nil {
					failed = append(failed, n+" (no compose.yaml)")
					continue
				}
				var env []byte
				if hasEnv {
					env, _ = os.ReadFile(filepath.Join(dir, ".env"))
				}
				if err := h.Save(ctx, n, string(compose), string(env), user, !current[n]); err != nil {
					failed = append(failed, fmt.Sprintf("%s (%v)", n, err))
					continue
				}
				restored++
			}
		}
	}
	dbStaged := false
	if req.Database {
		// Swapped in by store.Open on the next start (the old file stays as moorage.db.bak).
		if err := os.Rename(filepath.Join(sg.dir, "moorage.db"), filepath.Join(s.cfg.DataDir, "moorage.db.restore")); err != nil {
			s.engErr(w, err)
			return
		}
		dbStaged = true
	}
	sort.Strings(failed)
	setAuditTarget(r, fmt.Sprintf("backup (%d stacks, database %t)", restored, dbStaged))
	writeJSON(w, http.StatusOK, map[string]any{
		"restoredStacks": restored,
		"failedStacks":   failed,
		"dbStaged":       dbStaged,
	})
}
