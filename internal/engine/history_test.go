package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newHistoryEngine(t *testing.T) *Engine {
	t.Helper()
	e := newTestEngine(t)
	h, err := openHistory(filepath.Join(t.TempDir(), "history.git"))
	if err != nil {
		t.Fatal(err)
	}
	e.hist = h
	return e
}

func TestHistorySaveDiffRestore(t *testing.T) {
	e := newHistoryEngine(t)
	ctx := context.Background()
	v1 := "services:\n  web:\n    image: nginx:1.27\n    restart: unless-stopped\n"
	v2 := "services:\n  web:\n    image: nginx:1.29\n    restart: unless-stopped\n"

	if err := e.SaveStack(ctx, "web", v1, "TAG=1\n", "ann", true); err != nil {
		t.Fatal(err)
	}
	// saving identical content records nothing
	if err := e.SaveStack(ctx, "web", v1, "TAG=1\n", "ann", false); err != nil {
		t.Fatal(err)
	}
	if err := e.SaveStack(ctx, "web", v2, "", "bob", false); err != nil {
		t.Fatal(err)
	}
	// an edit made outside Moorage becomes its own version
	dir := filepath.Join(e.cfg.StacksDir, "web")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("X=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	log, err := e.StackHistory("web", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 3 {
		t.Fatalf("want 3 versions, got %d: %+v", len(log), log)
	}
	if log[0].Kind != KindExternal || log[0].Author != SystemAuthor || !log[0].Current {
		t.Fatalf("newest should be the external edit: %+v", log[0])
	}
	if log[1].Kind != KindEdit || log[1].Author != "bob" || strings.Join(log[1].Files, ",") != ".env,compose.yaml" {
		t.Fatalf("edit version wrong: %+v", log[1])
	}
	if log[2].Kind != KindCreate || log[2].Message != "Create stack" {
		t.Fatalf("create version wrong: %+v", log[2])
	}

	d, err := e.StackVersion("web", log[1].Hash)
	if err != nil {
		t.Fatal(err)
	}
	var compose *FileDiff
	for i := range d.Diff {
		if d.Diff[i].Name == "compose.yaml" {
			compose = &d.Diff[i]
		}
	}
	if compose == nil || compose.Status != "modified" || len(compose.Hunks) != 1 {
		t.Fatalf("compose diff wrong: %+v", d.Diff)
	}
	var got []string
	for _, l := range compose.Hunks[0].Lines {
		got = append(got, l.Op+l.Text)
	}
	if !strings.Contains(strings.Join(got, "|"), "-    image: nginx:1.27|+    image: nginx:1.29") {
		t.Fatalf("diff lines: %q", got)
	}

	// restore the first version: compose and .env come back, a new version is recorded
	nv, err := e.RestoreStack(ctx, "web", log[2].Hash, "ann")
	if err != nil || nv == nil || nv.Kind != KindRestore || nv.Restored != log[2].Short {
		t.Fatalf("restore: %+v %v", nv, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	envb, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if string(b) != v1 || string(envb) != "TAG=1\n" {
		t.Fatalf("files not restored: %q %q", b, envb)
	}

	if _, err := e.StackVersion("web", "0000000000000000000000000000000000000000"); err != ErrNoHistory {
		t.Fatalf("unknown hash: %v", err)
	}
	if _, err := e.StackVersion("web", "../x"); err != ErrNoHistory {
		t.Fatalf("bad hash: %v", err)
	}
}

func TestHistoryRestoreRemovesRenamedComposeFile(t *testing.T) {
	e := newHistoryEngine(t)
	dir := filepath.Join(e.cfg.StacksDir, "app")
	os.MkdirAll(dir, 0o755)
	old := "services:\n  a:\n    image: busybox\n"
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(old), 0o644)
	e.SyncAllHistory()
	os.Remove(filepath.Join(dir, "docker-compose.yml"))
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  b:\n    image: busybox\n"), 0o644)

	log, err := e.StackHistory("app", 10)
	if err != nil || len(log) != 2 || log[1].Kind != KindImport || log[0].Kind != KindExternal {
		t.Fatalf("history: %+v %v", log, err)
	}
	if _, err := e.RestoreStack(context.Background(), "app", log[1].Hash, "ann"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "compose.yaml")); !os.IsNotExist(err) {
		t.Fatal("compose.yaml should be gone, otherwise it would win over docker-compose.yml")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "docker-compose.yml")); string(b) != old {
		t.Fatalf("docker-compose.yml not restored: %q", b)
	}
}

func TestHunksContext(t *testing.T) {
	a := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n"
	b := "1\n2\n3\n4\n5\nsix\n7\n8\n9\n10\n11\n12\nthirteen\n"
	h := hunks(lineDiff(a, b), 2)
	if len(h) != 2 {
		t.Fatalf("want 2 hunks, got %+v", h)
	}
	if h[0].OldStart != 4 || h[0].NewStart != 4 || len(h[0].Lines) != 6 {
		t.Fatalf("first hunk: %+v", h[0])
	}
}

// A stack created from a template carries the template note as its first version
// message; a note can't forge the kind trailer with a newline.
func TestCreateStackNoted(t *testing.T) {
	e := newHistoryEngine(t)
	ctx := context.Background()
	c := "services:\n  db:\n    image: postgres:17-alpine\n"
	note := "Created from template postgres (bundled, version 1)\n\nMoorage-Kind: delete"
	if err := e.CreateStackNoted(ctx, "pg", c, "A=1\n", "ann", note); err != nil {
		t.Fatal(err)
	}
	if err := e.CreateStackNoted(ctx, "pg", c, "", "ann", note); err != ErrExists {
		t.Fatalf("second create: %v", err)
	}
	log, err := e.StackHistory("pg", 10)
	if err != nil || len(log) != 1 {
		t.Fatalf("history: %+v %v", log, err)
	}
	if log[0].Kind != KindCreate || log[0].Message != "Created from template postgres (bundled, version 1)" || log[0].Author != "ann" {
		t.Fatalf("first version: %+v", log[0])
	}
	// An empty note falls back to the plain message.
	if err := e.CreateStackNoted(ctx, "plain", c, "", "ann", " \n "); err != nil {
		t.Fatal(err)
	}
	if log, _ := e.StackHistory("plain", 10); len(log) != 1 || log[0].Message != "Create stack" {
		t.Fatalf("plain: %+v", log)
	}
	if got := CleanNote(strings.Repeat("x", 300)); len(got) != 200 {
		t.Fatalf("note not capped: %d", len(got))
	}
}
