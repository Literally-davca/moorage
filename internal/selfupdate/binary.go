package selfupdate

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"moorage/internal/host"
)

const healthTimeout = 120 * time.Second

// BinaryApplier performs signed binary upgrades and rollbacks in a host directory.
type BinaryApplier struct {
	DataDir        string
	ExePath        string
	CurrentVersion string
	IsHub          bool
	ListenAddr     string
	PubKey         ed25519.PublicKey
	GOOS           string
	GOARCH         string
	Log            *slog.Logger

	// Injectable hooks for tests.
	ExecFn        func(path string, argv, envv []string) error
	SpawnWatchFn  func(prevPath, statePath string) error
	HealthCheckFn func(ctx context.Context, st PendingState) bool
	RestartFn     func(exePath string, args []string) error
	Now           func() time.Time
	Sleep         func(time.Duration)
}

func (b *BinaryApplier) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *BinaryApplier) sleep(d time.Duration) {
	if b.Sleep != nil {
		b.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (b *BinaryApplier) exe() (string, error) {
	if b.ExePath != "" {
		return b.ExePath, nil
	}
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p, nil
}

func (b *BinaryApplier) prevPathFor(exePath string) string {
	return filepath.Join(filepath.Dir(exePath), "moorage.prev")
}

func (b *BinaryApplier) goos() string {
	if b.GOOS != "" {
		return b.GOOS
	}
	return runtime.GOOS
}

func (b *BinaryApplier) goarch() string {
	if b.GOARCH != "" {
		return b.GOARCH
	}
	return runtime.GOARCH
}

// StageAndSwap verifies the signed manifest and binary stream, preserves the running
// binary as moorage.prev, swaps the new binary into place atomically, and spawns a
// rollback watcher. The caller shuts down gracefully and calls ExecPending afterwards.
func (b *BinaryApplier) StageAndSwap(m host.SelfUpdateManifest, bin io.Reader) (string, error) {
	if _, err := ParseVersion(m.Version); err != nil {
		return "", fmt.Errorf("%w: %v", host.ErrSelfUpdateInvalid, err)
	}
	// Refuse downgrades (only manual rollback restores an older version).
	if cur, err := ParseVersion(b.CurrentVersion); err == nil {
		target, _ := ParseVersion(m.Version)
		cmp := compareSemVer(target, cur)
		if cmp < 0 {
			return "", fmt.Errorf("%w: refusing downgrade from %s to %s", host.ErrSelfUpdateInvalid, b.CurrentVersion, m.Version)
		}
		if cmp == 0 {
			return "", fmt.Errorf("%w: already running version %s", host.ErrSelfUpdateInvalid, b.CurrentVersion)
		}
	}

	pub := b.PubKey
	if len(pub) == 0 {
		var err error
		pub, err = DefaultPublicKey()
		if err != nil {
			return "", fmt.Errorf("%w: %v", host.ErrSelfUpdateInvalid, err)
		}
	}
	sums, _, err := VerifyManifest(m, pub)
	if err != nil {
		return "", err
	}
	assetName, err := AssetNameFor(b.goos(), b.goarch())
	if err != nil {
		return "", fmt.Errorf("%w: %v", host.ErrSelfUpdateUnsupported, err)
	}
	wantHex, ok := sums[assetName]
	if !ok {
		return "", fmt.Errorf("%w: %s missing from signed SHA256SUMS", host.ErrSelfUpdateInvalid, assetName)
	}

	exePath, err := b.exe()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	exeDir := filepath.Dir(exePath)
	tmpNew, err := VerifyReaderToTemp(exeDir, bin, wantHex, MaxBinarySize)
	if err != nil {
		return "", err
	}
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpNew)
		}
	}()

	// The new binary keeps the installed file's mode and owner (e.g. 0750 root:moorage),
	// not the staging default.
	if err := matchFileMode(tmpNew, exePath); err != nil {
		return "", fmt.Errorf("set staged binary mode: %w", err)
	}

	prevPath := b.prevPathFor(exePath)
	if err := copyFileAtomic(exePath, prevPath); err != nil {
		return "", fmt.Errorf("save moorage.prev: %w", err)
	}

	now := b.now()
	statePath := filepath.Join(b.DataDir, pendingStateFile)
	clearRollbackNote(b.DataDir)
	pending := PendingState{
		Action:      storeActionApply,
		Mode:        "binary",
		FromVersion: b.CurrentVersion,
		ToVersion:   m.Version,
		ExePath:     exePath,
		PrevPath:    prevPath,
		DataDir:     b.DataDir,
		IsHub:       b.IsHub,
		ListenAddr:  b.ListenAddr,
		PID:         os.Getpid(),
		Args:        append([]string(nil), os.Args...),
		StartedAt:   now,
		Deadline:    now.Add(healthTimeout),
	}
	if err := writeJSONFile(statePath, pending, 0o600); err != nil {
		return "", fmt.Errorf("write pending state: %w", err)
	}

	if err := os.Rename(tmpNew, exePath); err != nil {
		_ = os.Remove(statePath)
		return "", fmt.Errorf("replace executable: %w", err)
	}
	cleanupTmp = false

	// Launch the rollback watcher from moorage.prev BEFORE exiting/execing so a hung or
	// crashed new binary is rolled back within 120 s. Under systemd the watcher dies with
	// a crashing service (same cgroup); BootCheck in the restarted process covers that.
	spawn := b.SpawnWatchFn
	if spawn == nil {
		spawn = func(prev, st string) error {
			return spawnWatcher(prev, "self-update-watch", "--state", st)
		}
	}
	if err := spawn(prevPath, statePath); err != nil {
		// Revert immediately if watcher cannot start, rather than risking an unguarded swap.
		_ = os.Rename(prevPath, exePath)
		_ = os.Remove(statePath)
		return "", fmt.Errorf("start update watcher: %w", err)
	}

	return exePath, nil
}

// PrepareRollback atomically restores moorage.prev back to the active executable path.
func (b *BinaryApplier) PrepareRollback() (string, error) {
	exePath, err := b.exe()
	if err != nil {
		return "", err
	}
	prevPath := b.prevPathFor(exePath)
	st, err := os.Stat(prevPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: no previous binary (moorage.prev) kept for rollback", host.ErrSelfUpdateUnsupported)
		}
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%w: invalid moorage.prev file", host.ErrSelfUpdateInvalid)
	}
	if err := restoreBinary(exePath, prevPath); err != nil {
		return "", fmt.Errorf("restore executable: %w", err)
	}
	_ = os.Remove(prevPath)
	_ = os.Remove(filepath.Join(b.DataDir, prevMetaFile))
	clearRollbackNote(b.DataDir)
	return exePath, nil
}

// ExecPending replaces the current process with exePath (same args and env).
func (b *BinaryApplier) ExecPending(exePath string) error {
	fn := b.ExecFn
	if fn == nil {
		fn = execBinary
	}
	argv := os.Args
	if len(argv) == 0 {
		argv = []string{exePath}
	}
	return fn(exePath, argv, os.Environ())
}

// watchTermGrace is how long the watcher waits after SIGTERM before it SIGKILLs a hung
// new process during a rollback.
const watchTermGrace = 10 * time.Second

// RunWatch monitors an applied binary update until health is confirmed or Deadline expires.
// On failure or timeout, moorage.prev is atomically restored and started. It stops
// quietly when the pending state disappears (the new binary confirmed itself healthy, or
// BootCheck already rolled back).
func RunWatch(statePath string, applier *BinaryApplier) error {
	st, ok := readJSONFile[PendingState](statePath)
	if !ok {
		return fmt.Errorf("read pending update state %s", statePath)
	}
	nowFn := time.Now
	sleepFn := time.Sleep
	checkFn := checkBinaryHealth
	if applier != nil {
		if applier.Now != nil {
			nowFn = applier.Now
		}
		if applier.Sleep != nil {
			sleepFn = applier.Sleep
		}
		if applier.HealthCheckFn != nil {
			checkFn = applier.HealthCheckFn
		}
	}

	// Wait briefly before first check so the old process finishes exec/shutdown.
	sleepFn(1500 * time.Millisecond)

	for {
		if _, err := os.Stat(statePath); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		healthy := checkFn(ctx, st)
		cancel()
		if healthy {
			writePrevRecord(st.DataDir, PrevRecord{
				Mode:      "binary",
				Version:   st.FromVersion,
				ToVersion: st.ToVersion,
				SavedAt:   nowFn(),
				ExePath:   st.ExePath,
				PrevPath:  st.PrevPath,
			})
			_ = os.Remove(statePath)
			return nil
		}

		now := nowFn()
		reason := ""
		if !now.Before(st.Deadline) {
			reason = fmt.Sprintf("timed out waiting %s for %s to become healthy", healthTimeout, st.ToVersion)
		} else if !underSystemd() && st.PID > 0 && now.Sub(st.StartedAt) > 5*time.Second && !processAlive(st.PID) {
			// Outside systemd, a dead PID shortly after exec means the new binary exited/crashed.
			reason = fmt.Sprintf("new binary %s exited before becoming healthy", st.ToVersion)
		}

		if reason != "" {
			if rerr := rollbackWatchedBinary(st, reason, now, applier, sleepFn); rerr != nil {
				return fmt.Errorf("rollback failed after %s: %w", reason, rerr)
			}
			_ = os.Remove(statePath)
			return fmt.Errorf("rolled back: %s", reason)
		}
		sleepFn(2 * time.Second)
	}
}

func underSystemd() bool {
	return os.Getenv("INVOCATION_ID") != ""
}

func rollbackWatchedBinary(st PendingState, reason string, at time.Time, applier *BinaryApplier, sleepFn func(time.Duration)) error {
	if err := restoreBinary(st.ExePath, st.PrevPath); err != nil {
		return err
	}
	// The note is written before the hung process is stopped: that process exits non-zero
	// when it finds a note naming its own version (RolledBackAway), so systemd's
	// Restart=on-failure starts the restored binary.
	writeRollbackNote(st.DataDir, RollbackNote{
		FromVersion: st.FromVersion,
		ToVersion:   st.ToVersion,
		Reason:      "rolled back: " + reason,
		At:          at,
	})
	_ = os.Remove(filepath.Join(st.DataDir, prevMetaFile))
	_ = os.Remove(st.PrevPath)

	var args []string
	if len(st.Args) > 1 {
		args = st.Args[1:]
	}
	if applier != nil && applier.RestartFn != nil {
		return applier.RestartFn(st.ExePath, args)
	}

	// Stop the hung new process: SIGTERM, then SIGKILL after a grace period so the
	// restored binary never runs next to it (port clash) and systemd sees a failure.
	if st.PID > 0 && processAlive(st.PID) {
		terminateProcess(st.PID)
		for waited := time.Duration(0); waited < watchTermGrace && processAlive(st.PID); waited += 500 * time.Millisecond {
			sleepFn(500 * time.Millisecond)
		}
		if processAlive(st.PID) {
			killProcess(st.PID)
			sleepFn(time.Second)
		}
	}

	// systemd restarts the unit itself; standalone processes are relaunched here.
	if underSystemd() {
		return nil
	}
	if processAlive(st.PID) {
		return fmt.Errorf("process %d still running after SIGKILL; restored binary not started", st.PID)
	}
	if err := spawnDetached(st.ExePath, args...); err != nil {
		return fmt.Errorf("restart restored binary: %w", err)
	}
	return nil
}

func checkBinaryHealth(ctx context.Context, st PendingState) bool {
	if st.IsHub {
		url := localSessionURL(st.ListenAddr)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		var s struct {
			Version string `json:"version"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&s) != nil {
			return false
		}
		return strings.TrimPrefix(s.Version, "v") == strings.TrimPrefix(st.ToVersion, "v")
	}
	mark, ok := readHealthyMark(st.DataDir)
	if !ok {
		return false
	}
	return strings.TrimPrefix(mark.Version, "v") == strings.TrimPrefix(st.ToVersion, "v") && !mark.At.Before(st.StartedAt.Add(-time.Second))
}

func localSessionURL(listenAddr string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listenAddr))
	if err != nil || port == "" {
		port = "5080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/api/session"
}

// copyFileAtomic copies src to dst via a temp file and rename, keeping src's mode and owner.
func copyFileAtomic(src, dst string) error {
	tmp, err := copyToTemp(dst, src)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// matchFileMode gives path the permission bits and (best effort) owner of ref.
func matchFileMode(path, ref string) error {
	fi, err := os.Stat(ref)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, fi.Mode().Perm()); err != nil {
		return err
	}
	chownLike(path, fi)
	return nil
}

// copyToTemp copies src into a temp file next to dst. The copy keeps src's permission
// bits and, where the process may, its owner and group.
func copyToTemp(dst, src string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return "", err
	}
	out, err := os.CreateTemp(filepath.Dir(dst), ".moorage-copy-*")
	if err != nil {
		return "", err
	}
	tmp := out.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = out.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return "", err
	}
	if err := out.Chmod(fi.Mode().Perm()); err != nil {
		return "", err
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	chownLike(tmp, fi)
	cleanup = false
	return tmp, nil
}

const storeActionApply = "apply"
