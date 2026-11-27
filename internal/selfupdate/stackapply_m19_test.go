package selfupdate

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"moorage/internal/engine"
)

// M19A: a successful apply keeps the files as the configuration to return to.
func TestStackApplyHelperKeepsWorkingConfig(t *testing.T) {
	o, stack := stackApplyEnv(t)
	f := newFakeContainerEngine()
	f.addContainer(moorageCtr(true))
	if err := RunStackApplyHelper(context.Background(), f, func(context.Context, io.Writer) error { return nil }, o); err != nil {
		t.Fatal(err)
	}
	if !engine.SameSelfFiles(stack, engine.SelfGoodDir(o.DataDir)) {
		t.Fatal("self-stack-good must hold the applied files")
	}
}

// M19A: Save, then Apply – the backup equals the new files, so nothing is "rolled back".
func TestStackApplyHelperHonestWhenBackupEqualsNew(t *testing.T) {
	o, stack := stackApplyEnv(t)
	bdir := engine.SelfBackupDir(o.DataDir)
	if err := os.RemoveAll(bdir); err != nil {
		t.Fatal(err)
	}
	if err := engine.CopySelfFiles(stack, bdir); err != nil {
		t.Fatal(err)
	}
	ups := 0
	up := func(context.Context, io.Writer) error { ups++; return errors.New("bind: address already in use") }
	if err := RunStackApplyHelper(context.Background(), newFakeContainerEngine(), up, o); err == nil {
		t.Fatal("want an error")
	}
	r := readResult(t, o.DataDir)
	if r.RolledBack || !strings.Contains(r.Message, "no earlier working configuration") {
		t.Fatalf("result: %+v", r)
	}
	if ups != 1 {
		t.Fatalf("compose up must not run again with the same files, ran %d times", ups)
	}
}

// M19A: no backup at all (no known working configuration).
func TestStackApplyHelperNoBackup(t *testing.T) {
	o, _ := stackApplyEnv(t)
	_ = os.RemoveAll(engine.SelfBackupDir(o.DataDir))
	err := RunStackApplyHelper(context.Background(), newFakeContainerEngine(), func(context.Context, io.Writer) error { return errors.New("x") }, o)
	if err == nil || readResult(t, o.DataDir).RolledBack {
		t.Fatalf("want an honest failure, got %v", err)
	}
}

// M19B: the helper refuses before compose up when the listen port is taken.
func TestHostPortsTakenParsesListen(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  moorage:\n    image: m:1\n    network_mode: host\n    environment:\n      MOORAGE_LISTEN: \"5080\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	why := HostPortsTaken(context.Background(), newFakeContainerEngine(), "m", "moorage", dir, io.Discard)
	if engine.ReadNativePorts() == nil {
		if why != "" {
			t.Fatalf("without /proc the helper leaves the check to the API: %q", why)
		}
		return
	}
	if !strings.Contains(why, "missing the colon") {
		t.Fatalf("%q", why)
	}
}
