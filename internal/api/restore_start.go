package api

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"moorage/internal/host"
	"moorage/internal/store"
)

// ---- restore from a backup on first start (M20K) --------------------------------------
//
// MOORAGE_RESTORE_FILE restores a .mbk into an empty data folder before the hub opens its
// database: the database (users, passkeys, tokens, provider keys), the local host's
// stacks and history, and the agent CA. It never overwrites: with moorage.db present the
// variable is ignored. A failure stops the start, so Moorage never comes up empty (the
// next start would skip the restore and Setup would be open to anyone).

// ErrRestoreSkipped: the data folder already has a database; nothing was touched.
var ErrRestoreSkipped = errors.New("MOORAGE_RESTORE_FILE ignored: data already exist – remove the variable")

// RestoreOptions says what to restore where.
type RestoreOptions struct {
	File, Password string
	DataDir        string
	StacksDir      string // "" = no local stacks (hub without a local engine)
}

// RestoreReport is written to <data>/restored-from.json and logged.
type RestoreReport struct {
	File     string    `json:"file"`
	SHA256   string    `json:"sha256"`
	At       time.Time `json:"at"`
	Format   int       `json:"format"`
	Schema   int       `json:"schema"`
	Stacks   int       `json:"stacks"`
	Skipped  []string  `json:"skippedStacks,omitempty"` // a folder of that name already existed
	History  bool      `json:"history"`
	AgentCA  bool      `json:"agentCA"` // false: agents must join again
	OldPaths []string  `json:"oldPaths,omitempty"`
}

// RestoreAtStart restores opt.File into opt.DataDir when it has no database yet.
func RestoreAtStart(ctx context.Context, opt RestoreOptions, log *slog.Logger) (*RestoreReport, error) {
	dbPath := filepath.Join(opt.DataDir, "moorage.db")
	if fileExists(dbPath) {
		return nil, ErrRestoreSkipped
	}
	if opt.Password == "" {
		return nil, errors.New("MOORAGE_RESTORE_FILE is set: also set MOORAGE_RESTORE_PASSWORD or MOORAGE_RESTORE_PASSWORD_FILE")
	}
	f, err := os.Open(opt.File)
	if err != nil {
		return nil, fmt.Errorf("MOORAGE_RESTORE_FILE: %w", err)
	}
	defer f.Close()
	if err := os.MkdirAll(opt.DataDir, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(opt.DataDir, "import-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	sum := sha256.New()
	sg, err := readBackup(io.TeeReader(f, sum), opt.Password, dir)
	if err != nil {
		return nil, fmt.Errorf("restore %s: %w", opt.File, err)
	}
	staged := filepath.Join(dir, "moorage.db")
	info, err := store.InspectBackup(ctx, staged)
	if errors.Is(err, store.ErrBackupNewer) {
		return nil, fmt.Errorf("this backup is from a newer Moorage (schema %d) – use that version or newer", info.SchemaVersion)
	} else if err != nil {
		return nil, fmt.Errorf("restore %s: %w", opt.File, err)
	}
	_, _ = io.Copy(sum, f) // the hash covers the whole file, also bytes after the archive
	rep := &RestoreReport{File: opt.File, SHA256: hex.EncodeToString(sum.Sum(nil)), At: time.Now().UTC(),
		Format: sg.manifest.Format, Schema: info.SchemaVersion, AgentCA: sg.caFiles == 2}

	if opt.StacksDir != "" {
		if err := restoreLocalStacks(sg, opt.StacksDir, rep); err != nil {
			return nil, err
		}
	}
	if hist := filepath.Join(dir, "history", host.LocalID+".tar"); fileExists(hist) && !fileExists(filepath.Join(opt.DataDir, "history.git")) {
		if err := untarDir(hist, filepath.Join(opt.DataDir, "history.git")); err != nil {
			return nil, fmt.Errorf("restore history: %w", err)
		}
		rep.History = true
	}
	if rep.AgentCA {
		if err := restoreCA(dir, opt.DataDir, false); err != nil {
			return nil, fmt.Errorf("restore agent CA: %w", err)
		}
	}

	// sign-ins of the old installation must not work here: no session survives
	db, err := store.Open(dir) // migrates the staged copy in place
	if err != nil {
		return nil, fmt.Errorf("restore: open database: %w", err)
	}
	err = db.DeleteAllSessions(ctx)
	if err == nil {
		err = db.Audit(ctx, store.AuditEntry{Actor: "system", Action: "backup.restore_env",
			Target: fmt.Sprintf("%s (format %d, %d stacks, agent CA %t)", filepath.Base(opt.File), rep.Format, rep.Stacks, rep.AgentCA)})
	}
	if cerr := db.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("restore: %w", err)
	}
	// the database goes in last: until here a failed restore leaves the folder without
	// one and the next start tries again. A WAL left by Close moves with it.
	for _, sfx := range []string{"-wal", "-shm"} {
		if fileExists(staged + sfx) {
			if err := os.Rename(staged+sfx, dbPath+sfx); err != nil {
				return nil, err
			}
		}
	}
	if err := os.Rename(staged, dbPath); err != nil {
		return nil, err
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	_ = os.WriteFile(filepath.Join(opt.DataDir, "restored-from.json"), b, 0o600)
	if log != nil {
		log.Info("restored from backup", "file", opt.File, "sha256", rep.SHA256, "format", rep.Format, "stacks", rep.Stacks,
			"skippedStacks", rep.Skipped, "history", rep.History, "agentCA", rep.AgentCA)
		if !rep.AgentCA {
			log.Warn("agents must join again: this backup has no agent CA")
		}
		for _, p := range rep.OldPaths {
			log.Warn("a restored compose file mounts a path of the old stacks folder", "path", p)
		}
		log.Warn("remove MOORAGE_RESTORE_FILE and the password variable now; later starts ignore them")
	}
	return rep, nil
}

// restoreLocalStacks writes the local host's stacks into folders that don't exist yet.
func restoreLocalStacks(sg *importStaging, stacksDir string, rep *RestoreReport) error {
	oldDirs := map[string]bool{}
	for name, hasEnv := range sg.stacks[host.LocalID] {
		dst := filepath.Join(stacksDir, name)
		if _, err := os.Stat(dst); err == nil {
			rep.Skipped = append(rep.Skipped, name)
			continue
		}
		src := filepath.Join(sg.dir, "stacks", host.LocalID, name)
		compose, err := os.ReadFile(filepath.Join(src, "compose.yaml"))
		if err != nil {
			continue // only a .env: nothing to run
		}
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, "compose.yaml"), compose, 0o644); err != nil {
			return err
		}
		if hasEnv {
			env, err := os.ReadFile(filepath.Join(src, ".env"))
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dst, ".env"), env, 0o600); err != nil {
				return err
			}
		}
		rep.Stacks++
		// a bind mount into another stacks folder than this one (moved to a new machine)
		for _, line := range strings.Split(string(compose), "\n") {
			if p := oldStacksPath(line, stacksDir); p != "" && !oldDirs[p] {
				oldDirs[p] = true
				rep.OldPaths = append(rep.OldPaths, p)
			}
		}
	}
	return nil
}

// oldStacksPath returns the host path of a "- /x/stacks/...:" mount line that isn't under
// stacksDir, or "".
func oldStacksPath(line, stacksDir string) string {
	l := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
	src, _, ok := strings.Cut(strings.Trim(l, `"'`), ":")
	if !ok || !strings.HasPrefix(src, "/") || !strings.Contains(src, "/stacks/") {
		return ""
	}
	if strings.HasPrefix(src, filepath.ToSlash(stacksDir)+"/") {
		return ""
	}
	return src
}

// restoreCA puts ca/ca.crt and ca/ca.key from a staged backup into <data>/ca. Without
// replace an existing CA stays.
func restoreCA(stageDir, dataDir string, replace bool) error {
	dst := filepath.Join(dataDir, "ca")
	if !replace && fileExists(filepath.Join(dst, "ca.crt")) {
		return nil
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	for _, f := range []string{"ca.key", "ca.crt"} { // the key first: a CA cert without its key is unusable
		b, err := os.ReadFile(filepath.Join(stageDir, "ca", f))
		if err != nil {
			return err
		}
		tmp := filepath.Join(dst, f+".restore")
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, filepath.Join(dst, f)); err != nil {
			return err
		}
	}
	return nil
}

// untarDir extracts a history archive (regular files and folders only) into a new folder.
func untarDir(archive, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	tmp := dst + ".restore"
	_ = os.RemoveAll(tmp)
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := path.Clean(hdr.Name)
		if name == "." || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("unsafe path %q in history archive", hdr.Name)
		}
		p := filepath.Join(tmp, filepath.FromSlash(name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return err
			}
			if err := saveEntry(tr, p, hdr.Size); err != nil {
				return err
			}
		}
	}
	return os.Rename(tmp, dst)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
