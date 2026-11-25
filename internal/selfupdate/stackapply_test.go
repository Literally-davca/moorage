package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"moorage/internal/engine"
)

// stackApplyEnv prepares a stack folder with new files on disk and the previous ones in
// the backup, as SelfApply leaves them for the helper.
func stackApplyEnv(t *testing.T) (StackApplyOptions, string) {
	t.Helper()
	data, stack := t.TempDir(), t.TempDir()
	bdir := engine.SelfBackupDir(data)
	if err := os.MkdirAll(bdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bdir, "compose.yaml"), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stack, "compose.yaml"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stack, ".env"), []byte("NEW=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	o := StackApplyOptions{
		Project: "moorage", Service: "moorage", StackDir: stack, DataDir: data, Version: "1.0.0",
		StartDelay: time.Millisecond, HealthTimeout: 30 * time.Second, StableFor: 5 * time.Second, PollInterval: time.Second,
		Now:   func() time.Time { return now },
		Sleep: func(d time.Duration) { now = now.Add(d) },
	}
	return o, stack
}

func moorageCtr(running bool) container.InspectResponse {
	st := &container.State{Running: running, Status: container.StateExited, StartedAt: "2026-11-23T10:00:00Z"}
	if running {
		st.Status = container.StateRunning
	}
	return container.InspectResponse{
		ID: "c1", Name: "/moorage-moorage-1", State: st,
		Config: &container.Config{Labels: map[string]string{
			"com.docker.compose.project": "moorage", "com.docker.compose.service": "moorage",
		}},
	}
}

func readResult(t *testing.T, dataDir string) engine.SelfApplyResult {
	t.Helper()
	var r engine.SelfApplyResult
	b, err := os.ReadFile(filepath.Join(dataDir, engine.SelfStackResultFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestStackApplyHelperSuccess(t *testing.T) {
	o, stack := stackApplyEnv(t)
	f := newFakeContainerEngine()
	f.addContainer(moorageCtr(true))
	ups := 0
	up := func(context.Context, io.Writer) error { ups++; return nil }
	if err := RunStackApplyHelper(context.Background(), f, up, o); err != nil {
		t.Fatal(err)
	}
	if ups != 1 {
		t.Fatalf("compose up ran %d times", ups)
	}
	if r := readResult(t, o.DataDir); !r.OK || r.RolledBack {
		t.Fatalf("result: %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(stack, "compose.yaml")); string(b) != "new\n" {
		t.Fatal("a successful apply must keep the new files")
	}
	if _, err := os.Stat(engine.SelfBackupDir(o.DataDir)); !os.IsNotExist(err) {
		t.Fatal("the backup is removed after success")
	}
}

func TestStackApplyHelperRollsBackWhenMoorageStaysDown(t *testing.T) {
	o, stack := stackApplyEnv(t)
	f := newFakeContainerEngine()
	f.addContainer(moorageCtr(false)) // the new container exited
	ups := 0
	up := func(context.Context, io.Writer) error { ups++; return nil }
	err := RunStackApplyHelper(context.Background(), f, up, o)
	if err == nil {
		t.Fatal("want an error")
	}
	if ups != 2 {
		t.Fatalf("compose up must run again with the old files, ran %d times", ups)
	}
	if b, _ := os.ReadFile(filepath.Join(stack, "compose.yaml")); string(b) != "old\n" {
		t.Fatalf("compose not rolled back: %q", b)
	}
	if _, err := os.Stat(filepath.Join(stack, ".env")); !os.IsNotExist(err) {
		t.Fatal("a .env that didn't exist before must be removed on rollback")
	}
	if r := readResult(t, o.DataDir); r.OK || !r.RolledBack {
		t.Fatalf("result: %+v", r)
	}
}

func TestStackApplyHelperRollsBackWhenUpFails(t *testing.T) {
	o, stack := stackApplyEnv(t)
	f := newFakeContainerEngine()
	calls := 0
	up := func(context.Context, io.Writer) error {
		calls++
		if calls == 1 {
			return errors.New("invalid command")
		}
		return nil
	}
	if err := RunStackApplyHelper(context.Background(), f, up, o); err == nil {
		t.Fatal("want an error")
	}
	if b, _ := os.ReadFile(filepath.Join(stack, "compose.yaml")); string(b) != "old\n" {
		t.Fatal("compose not rolled back")
	}
	if r := readResult(t, o.DataDir); !r.RolledBack {
		t.Fatalf("result: %+v", r)
	}
}

func TestStackApplyHelperRefusesSecondRun(t *testing.T) {
	f := newFakeContainerEngine()
	running := moorageCtr(true)
	running.ID, running.Name = "h1", "/moorage-moorage-1-moorage-apply"
	f.addContainer(running)
	if err := refuseRunningApplyHelper(context.Background(), f, "moorage-moorage-1-moorage-apply"); !errors.Is(err, engine.ErrBusy) {
		t.Fatalf("want busy, got %v", err)
	}
	if err := refuseRunningApplyHelper(context.Background(), f, "nothing-here"); err != nil {
		t.Fatalf("no helper: %v", err)
	}
}

func TestStackApplyHelperHubIgnoresOldVersionMismatch(t *testing.T) {
	o, _ := stackApplyEnv(t)
	o.IsHub = true
	o.Version = "0.1.3"
	f := newFakeContainerEngine()
	f.addContainer(moorageCtr(true))
	// Simulate new container writing healthy mark for v0.1.4 after tag update via Apply & restart.
	_ = writeJSONFile(filepath.Join(o.DataDir, healthyMarkFile), HealthyMark{
		Version: "0.1.4",
		At:      o.Now().Add(time.Second),
	}, 0o600)
	up := func(context.Context, io.Writer) error { return nil }
	if err := RunStackApplyHelper(context.Background(), f, up, o); err != nil {
		t.Fatalf("expected hub apply with new version to succeed, got %v", err)
	}
	if r := readResult(t, o.DataDir); !r.OK || r.RolledBack {
		t.Fatalf("expected OK without rollback, got %+v", r)
	}
}
