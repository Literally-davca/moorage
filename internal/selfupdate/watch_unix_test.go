//go:build unix

package selfupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A new binary that hangs and ignores SIGTERM is SIGKILLed after the grace period, and
// outside systemd the restored binary is started in its place.
func TestWatcherKillsHungProcessAndRelaunchesRestored(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	dir := t.TempDir()
	exe := filepath.Join(dir, "moorage")
	prev := filepath.Join(dir, "moorage.prev")
	marker := filepath.Join(dir, "relaunched")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prev, []byte("#!/bin/sh\ntouch \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	hung := exec.Command("sh", "-c", "trap '' TERM; exec sleep 30")
	if err := hung.Start(); err != nil {
		t.Skipf("cannot start test process: %v", err)
	}
	exited := make(chan struct{})
	go func() { _ = hung.Wait(); close(exited) }() // reap, so a dead PID really is gone
	t.Cleanup(func() { _ = hung.Process.Kill() })
	time.Sleep(100 * time.Millisecond)

	st := PendingState{
		FromVersion: "0.0.1-test", ToVersion: "0.0.2-test", ExePath: exe, PrevPath: prev,
		DataDir: dir, PID: hung.Process.Pid, Args: []string{"moorage", marker},
	}
	err := rollbackWatchedBinary(st, "test hang", time.Now(), nil, func(time.Duration) { time.Sleep(20 * time.Millisecond) })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("hung process was not killed")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restored binary was not relaunched")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if note, ok := readRollbackNote(dir); !ok || note.ToVersion != "0.0.2-test" {
		t.Fatalf("rollback note missing: %+v", note)
	}
}
