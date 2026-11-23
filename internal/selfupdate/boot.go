package selfupdate

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxBootAttempts is how many times a freshly installed binary may start without
// confirming health before the next start restores moorage.prev. systemd's default
// StartLimitBurst is 5 starts per 10 s, so the rollback start (attempt 4) still fits.
const maxBootAttempts = 3

// BootCheck runs at process start, before any server is started. It makes binary-mode
// rollback independent of the watcher process: under systemd the watcher lives in the
// service cgroup and dies together with a crashing new binary, so the restarted process
// itself counts its boot attempts in self-update-pending.json and, once the attempts or
// the health deadline are exceeded, restores moorage.prev, writes the rollback note and
// returns the executable path to exec (the restored previous version).
// It returns "" when the process should continue normally.
func BootCheck(dataDir, version string, now time.Time, log *slog.Logger) string {
	statePath := filepath.Join(dataDir, pendingStateFile)
	st, ok := readJSONFile[PendingState](statePath)
	if !ok || st.Mode != "binary" {
		return ""
	}
	if normVersion(version) != normVersion(st.ToVersion) {
		// Not the new binary (the old one runs again). A pending state past its deadline
		// is stale: drop it so it can't trigger a rollback later.
		if !now.Before(st.Deadline) {
			_ = os.Remove(statePath)
		}
		return ""
	}

	st.BootAttempts++
	reason := ""
	switch {
	case !now.Before(st.Deadline):
		reason = fmt.Sprintf("%s did not become healthy within %s", st.ToVersion, healthTimeout)
	case st.BootAttempts > maxBootAttempts:
		reason = fmt.Sprintf("%s failed to start %d times", st.ToVersion, st.BootAttempts-1)
	}
	if reason == "" {
		if err := writeJSONFile(statePath, st, 0o600); err != nil && log != nil {
			log.Warn("self-update: record boot attempt", "err", err)
		}
		return ""
	}

	if err := restoreBinary(st.ExePath, st.PrevPath); err != nil {
		// Without moorage.prev there is nothing to go back to: keep running the new binary.
		if log != nil {
			log.Error("self-update: boot rollback failed, continuing with new binary", "reason", reason, "err", err)
		}
		_ = os.Remove(statePath)
		return ""
	}
	writeRollbackNote(st.DataDir, RollbackNote{
		FromVersion: st.FromVersion,
		ToVersion:   st.ToVersion,
		Reason:      "rolled back: " + reason,
		At:          now,
	})
	_ = os.Remove(filepath.Join(st.DataDir, prevMetaFile))
	_ = os.Remove(st.PrevPath)
	_ = os.Remove(statePath)
	if log != nil {
		log.Warn("self-update: rolled back to previous binary at boot", "reason", reason, "to", st.FromVersion)
	}
	return st.ExePath
}

// FinalizePending confirms a pending binary update once the new version is healthy:
// it records moorage.prev as the kept previous version and removes the pending state, so
// neither the watcher nor BootCheck rolls the update back any more. It is a no-op when
// no pending state for version exists. Returns true when a pending state was finalised.
func FinalizePending(dataDir, version string, now time.Time) bool {
	statePath := filepath.Join(dataDir, pendingStateFile)
	st, ok := readJSONFile[PendingState](statePath)
	if !ok || normVersion(st.ToVersion) != normVersion(version) {
		return false
	}
	writePrevRecord(dataDir, PrevRecord{
		Mode:      "binary",
		Version:   st.FromVersion,
		ToVersion: st.ToVersion,
		SavedAt:   now,
		ExePath:   st.ExePath,
		PrevPath:  st.PrevPath,
	})
	_ = os.Remove(statePath)
	return true
}

// PendingExists reports whether a binary update is still waiting for health confirmation.
func PendingExists(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, pendingStateFile))
	return err == nil
}

// RolledBackAway reports whether this process (running version) was replaced by an
// automatic rollback while it ran: the watcher restored moorage.prev and stopped us.
// The caller then exits non-zero so systemd's Restart=on-failure starts the restored
// binary; with a clean exit only Restart=always would.
func RolledBackAway(dataDir, version string, startedAt time.Time) bool {
	note, ok := readRollbackNote(dataDir)
	if !ok || normVersion(note.ToVersion) != normVersion(version) {
		return false
	}
	return !note.At.Before(startedAt)
}

// ExecRestored replaces the current process with path, keeping args and environment.
func ExecRestored(path string) error {
	return execBinary(path, os.Args, os.Environ())
}

// SpawnWatcherChild is the intermediate step of the watcher double fork: it starts the
// real watcher (this executable, without --detach) in its own session and returns.
func SpawnWatcherChild(statePath string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve watcher executable: %w", err)
	}
	return spawnDetached(exe, "self-update-watch", "--state", statePath)
}

// restoreBinary copies prevPath over exePath atomically, keeping prevPath's file mode.
func restoreBinary(exePath, prevPath string) error {
	if _, err := os.Stat(prevPath); err != nil {
		return err
	}
	tmp, err := copyToTemp(exePath, prevPath)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, exePath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func normVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}
