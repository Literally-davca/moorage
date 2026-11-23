package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"moorage/internal/host"
)

func TestBinarySwapPrevRollbackAndCleanup(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exePath := filepath.Join(binDir, "moorage")
	oldBytes := []byte("old-moorage-0.0.1")
	newBytes := []byte("new-moorage-0.0.2")
	if err := os.WriteFile(exePath, oldBytes, 0o755); err != nil {
		t.Fatal(err)
	}

	fx := makeSignedFixture(t, "0.0.2-test", "moorage-linux-amd64", "linux/amd64", newBytes, "", "")
	now := time.Unix(1_700_000_000, 0)

	var execdPath string
	var spawnedWatchPrev, spawnedState string
	re := RuntimeEnv{
		GOOS:           "linux",
		GOARCH:         "amd64",
		Executable:     func() (string, error) { return exePath, nil },
		ReadFile:       func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Stat:           func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		Hostname:       func() (string, error) { return "test-host", nil },
		Getenv:         func(string) string { return "" },
		CheckWritable:  probeDirWritable,
		SelfUpdateFlag: true,
	}

	var shutdownCalled bool
	node := &NodeUpdater{
		DataDir:        dataDir,
		CurrentVersion: "0.0.1-test",
		SelfUpdate:     true,
		IsHub:          false,
		PubKey:         fx.pub,
		GOOS:           "linux",
		GOARCH:         "amd64",
		Runtime:        &re,
		ShutdownWait:   5 * time.Millisecond,
		ShutdownFn:     func() { shutdownCalled = true },
		ExecFn: func(p string, _, _ []string) error {
			execdPath = p
			return nil
		},
		SpawnWatchFn: func(prev, st string) error {
			spawnedWatchPrev = prev
			spawnedState = st
			return nil
		},
	}

	if err := node.ApplyBinary(context.Background(), fx.manifest, bytes.NewReader(newBytes)); err != nil {
		t.Fatalf("ApplyBinary failed: %v", err)
	}
	time.Sleep(25 * time.Millisecond)
	if !shutdownCalled {
		t.Fatal("expected ShutdownFn to fire after ApplyBinary")
	}
	if err := node.ExecPending(); err != nil || execdPath != exePath {
		t.Fatalf("ExecPending = (%q, %v), want %q", execdPath, err, exePath)
	}

	gotExe, _ := os.ReadFile(exePath)
	if !bytes.Equal(gotExe, newBytes) {
		t.Fatalf("expected replaced binary bytes, got %q", gotExe)
	}
	gotPrev, _ := os.ReadFile(spawnedWatchPrev)
	if !bytes.Equal(gotPrev, oldBytes) {
		t.Fatalf("expected moorage.prev to hold old binary, got %q", gotPrev)
	}

	// Simulate healthy startup checked by RunWatch.
	watchApplier := &BinaryApplier{
		Now:   func() time.Time { return now },
		Sleep: func(time.Duration) {},
		HealthCheckFn: func(context.Context, PendingState) bool {
			return true
		},
	}
	if err := RunWatch(spawnedState, watchApplier); err != nil {
		t.Fatalf("RunWatch healthy check failed: %v", err)
	}

	// moorage.prev stays kept and DetectStatus surfaces PrevVersion.
	st := DetectStatus(context.Background(), nil, dataDir, "0.0.2-test", true, &re)
	if st.Mode != "binary" || !st.Supported || st.PrevVersion != "0.0.1-test" {
		t.Fatalf("unexpected status after swap: %+v", st)
	}

	// Cleanup before 24 h keeps moorage.prev.
	CleanupExpiredPrev(context.Background(), nil, dataDir, now.Add(23*time.Hour))
	if _, err := os.Stat(spawnedWatchPrev); err != nil {
		t.Fatalf("moorage.prev removed too early: %v", err)
	}

	// Manual rollback restores old binary and deletes moorage.prev.
	node2 := &NodeUpdater{
		DataDir:        dataDir,
		CurrentVersion: "0.0.2-test",
		SelfUpdate:     true,
		PubKey:         fx.pub,
		GOOS:           "linux",
		GOARCH:         "amd64",
		Runtime:        &re,
		ShutdownWait:   5 * time.Millisecond,
		ShutdownFn:     func() {},
		ExecFn:         func(string, []string, []string) error { return nil },
	}
	if err := node2.Rollback(context.Background()); err != nil {
		t.Fatalf("manual Rollback failed: %v", err)
	}
	restored, _ := os.ReadFile(exePath)
	if !bytes.Equal(restored, oldBytes) {
		t.Fatalf("rollback did not restore old binary: %q", restored)
	}
	if _, err := os.Stat(spawnedWatchPrev); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected moorage.prev removed after manual rollback")
	}

	// Test 24h cleanup when kept without manual rollback.
	_ = os.WriteFile(spawnedWatchPrev, oldBytes, 0o755)
	_ = writeJSONFile(filepath.Join(dataDir, prevMetaFile), PrevRecord{
		Mode:     "binary",
		Version:  "0.0.1-test",
		SavedAt:  now,
		ExePath:  exePath,
		PrevPath: spawnedWatchPrev,
	}, 0o600)
	CleanupExpiredPrev(context.Background(), nil, dataDir, now.Add(25*time.Hour))
	if _, err := os.Stat(spawnedWatchPrev); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected moorage.prev deleted after 24 h")
	}
}

func TestBinaryAutoRollbackOnHealthTimeoutAndSafeguards(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	dataDir := filepath.Join(dir, "data")
	_ = os.MkdirAll(binDir, 0o755)
	exePath := filepath.Join(binDir, "moorage")
	oldBytes := []byte("old-0.0.1")
	newBytes := []byte("broken-0.0.2")
	_ = os.WriteFile(exePath, oldBytes, 0o755)

	fx := makeSignedFixture(t, "0.0.2-test", "moorage-linux-amd64", "linux/amd64", newBytes, "", "")
	re := RuntimeEnv{
		GOOS:           "linux",
		GOARCH:         "amd64",
		Executable:     func() (string, error) { return exePath, nil },
		ReadFile:       func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Stat:           func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		Hostname:       func() (string, error) { return "test-host", nil },
		CheckWritable:  probeDirWritable,
		SelfUpdateFlag: true,
	}

	// MOORAGE_SELF_UPDATE=false must refuse ApplyBinary even with valid signature.
	disabledNode := &NodeUpdater{
		DataDir:        dataDir,
		CurrentVersion: "0.0.1-test",
		SelfUpdate:     false,
		PubKey:         fx.pub,
		GOOS:           "linux",
		GOARCH:         "amd64",
		Runtime:        &re,
	}
	if err := disabledNode.ApplyBinary(context.Background(), fx.manifest, bytes.NewReader(newBytes)); !errors.Is(err, host.ErrSelfUpdateDisabled) {
		t.Fatalf("expected ErrSelfUpdateDisabled, got %v", err)
	}

	// Downgrade refusal.
	higherNode := &NodeUpdater{
		DataDir:        dataDir,
		CurrentVersion: "1.0.0",
		SelfUpdate:     true,
		PubKey:         fx.pub,
		GOOS:           "linux",
		GOARCH:         "amd64",
		Runtime:        &re,
	}
	if err := higherNode.ApplyBinary(context.Background(), fx.manifest, bytes.NewReader(newBytes)); !errors.Is(err, host.ErrSelfUpdateInvalid) {
		t.Fatalf("expected downgrade refusal ErrSelfUpdateInvalid, got %v", err)
	}

	// Auto-rollback on 120s health timeout (fake clock).
	applier := &BinaryApplier{
		DataDir:        dataDir,
		ExePath:        exePath,
		CurrentVersion: "0.0.1-test",
		PubKey:         fx.pub,
		GOOS:           "linux",
		GOARCH:         "amd64",
		SpawnWatchFn:   func(_, _ string) error { return nil },
	}
	if _, err := applier.StageAndSwap(fx.manifest, bytes.NewReader(newBytes)); err != nil {
		t.Fatalf("StageAndSwap failed: %v", err)
	}

	fakeNow := time.Now()
	restartedExe := ""
	watchApplier := &BinaryApplier{
		Now: func() time.Time {
			fakeNow = fakeNow.Add(45 * time.Second)
			return fakeNow
		},
		Sleep:         func(time.Duration) {},
		HealthCheckFn: func(context.Context, PendingState) bool { return false },
		RestartFn: func(p string, _ []string) error {
			restartedExe = p
			return nil
		},
	}
	statePath := filepath.Join(dataDir, pendingStateFile)
	err := RunWatch(statePath, watchApplier)
	if err == nil || !strings.Contains(err.Error(), "rolled back:") {
		t.Fatalf("expected rolled back error, got %v", err)
	}
	if restartedExe != exePath {
		t.Fatalf("expected watcher to restart restored %q, got %q", exePath, restartedExe)
	}
	restored, _ := os.ReadFile(exePath)
	if !bytes.Equal(restored, oldBytes) {
		t.Fatalf("auto-rollback did not restore old binary: %q", restored)
	}
	st := DetectStatus(context.Background(), nil, dataDir, "0.0.1-test", true, &re)
	if !strings.Contains(st.LastRollback, "rolled back:") {
		t.Fatalf("expected LastRollback recorded, got %q", st.LastRollback)
	}
}
