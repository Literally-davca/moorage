package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const selfCompose = `services:
  moorage:
    image: ghcr.io/literally-davca/moorage:1.0.0
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - data:/data
      - /opt/stacks:/opt/stacks
volumes:
  data:
`

func newSelfEngine(t *testing.T, launch func(context.Context) error) *Engine {
	t.Helper()
	e := newHistoryEngine(t)
	e.cfg.DataDir = t.TempDir()
	if err := e.SaveStack(context.Background(), "moorage", selfCompose, "A=1\n", "ann", true); err != nil {
		t.Fatal(err)
	}
	e.SetSelf(&SelfStack{Project: "moorage", Service: "moorage", SocketTarget: "/var/run/docker.sock", DataTarget: "/data", Launch: launch})
	return e
}

func TestSelfStackRefusesStopDownDelete(t *testing.T) {
	e := newSelfEngine(t, nil)
	for _, a := range []string{"down", "stop", "up", "update"} {
		if _, err := e.RunAction("moorage", a); !errors.Is(err, ErrSelfStack) {
			t.Errorf("%s: want ErrSelfStack, got %v", a, err)
		}
	}
	if err := e.DeleteStack(context.Background(), "moorage", "ann"); !errors.Is(err, ErrSelfStack) {
		t.Fatalf("delete: %v", err)
	}
	if _, err := e.RollbackImages(context.Background(), "moorage", nil, "ann"); !errors.Is(err, ErrSelfStack) {
		t.Fatalf("image rollback: %v", err)
	}
	// Other stacks are unaffected by the self rule.
	if _, err := e.RunAction("other", "stop"); errors.Is(err, ErrSelfStack) {
		t.Fatal("another stack must not be treated as self")
	}
	if !e.IsSelf("moorage") || e.IsSelf("other") {
		t.Fatal("IsSelf must name only the self stack")
	}
	// Restart and pull stay allowed (not starting them here: there is no Docker).
	if selfBlocked["restart"] || selfBlocked["pull"] {
		t.Fatal("restart and pull must stay allowed on the self stack")
	}
}

func TestSelfStackValidation(t *testing.T) {
	e := newSelfEngine(t, nil)
	ctx := context.Background()
	cases := map[string]string{
		"service removed": strings.Replace(selfCompose, "  moorage:\n", "  other:\n", 1),
		"socket removed":  strings.Replace(selfCompose, "      - /var/run/docker.sock:/var/run/docker.sock\n", "", 1),
		"data removed":    strings.Replace(selfCompose, "      - data:/data\n", "", 1),
	}
	for name, c := range cases {
		err := e.SaveStack(ctx, "moorage", c, "", "ann", false)
		var ve ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s: want a validation error, got %v", name, err)
		}
	}
	ok := strings.Replace(selfCompose, "1.0.0", "1.1.0", 1)
	if err := e.SaveStack(ctx, "moorage", ok, "", "ann", false); err != nil {
		t.Fatalf("a harmless change must save: %v", err)
	}
}

func TestSelfApplyLaunchFailureRestoresFiles(t *testing.T) {
	e := newSelfEngine(t, func(context.Context) error { return errors.New("no socket") })
	ctx := context.Background()
	changed := strings.Replace(selfCompose, "1.0.0", "2.0.0", 1)
	if err := e.SelfApply(ctx, "moorage", changed, "", "ann"); err == nil {
		t.Fatal("a failed launch must fail the apply")
	}
	dir, _ := e.stackDir("moorage")
	b, _ := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if string(b) != selfCompose {
		t.Fatalf("compose not restored:\n%s", b)
	}
	if env, _ := os.ReadFile(filepath.Join(dir, ".env")); string(env) != "A=1\n" {
		t.Fatalf(".env not restored: %q", env)
	}
	if e.isBusy("moorage") {
		t.Fatal("the stack must be unlocked after a failed launch")
	}
}

func TestSelfApplyLaunchKeepsBackupAndLock(t *testing.T) {
	launched := false
	e := newSelfEngine(t, func(context.Context) error { launched = true; return nil })
	ctx := context.Background()
	changed := strings.Replace(selfCompose, "1.0.0", "2.0.0", 1)
	if err := e.SelfApply(ctx, "moorage", changed, "", "ann"); err != nil {
		t.Fatal(err)
	}
	if !launched {
		t.Fatal("helper not launched")
	}
	if !e.isBusy("moorage") {
		t.Fatal("the stack stays locked while the helper recreates Moorage")
	}
	bdir := SelfBackupDir(e.cfg.DataDir)
	if b, _ := os.ReadFile(filepath.Join(bdir, "compose.yaml")); string(b) != selfCompose {
		t.Fatalf("backup compose: %q", b)
	}
	// The helper's rollback puts the old files back, and removes a .env that didn't exist.
	dir, _ := e.stackDir("moorage")
	if err := RestoreSelfBackup(dir, bdir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "compose.yaml")); string(b) != selfCompose {
		t.Fatal("rollback didn't restore compose")
	}
	if err := e.SelfApply(ctx, "moorage", changed, "", "ann"); !errors.Is(err, ErrBusy) {
		t.Fatalf("a second apply while one runs: want busy, got %v", err)
	}
}

func TestSelfApplyNotSelf(t *testing.T) {
	e := newSelfEngine(t, nil)
	if err := e.SelfApply(context.Background(), "other", selfCompose, "", "ann"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
}
