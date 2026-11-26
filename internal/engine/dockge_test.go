package engine

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestTakeOverDockgeFolder (M18C): pointing Moorage at an existing Dockge stacks folder
// makes its stacks managed and records a first version, without writing a single byte
// into the folder (history lives in the data folder).
func TestTakeOverDockgeFolder(t *testing.T) {
	e := newHistoryEngine(t)
	root := e.cfg.StacksDir
	files := map[string]string{
		"immich/compose.yaml":         "services:\n  server:\n    image: ghcr.io/immich-app/immich-server:release\n    env_file: .env\n",
		"immich/.env":                 "UPLOAD_LOCATION=./library\nDB_PASSWORD=secret\n",
		"immich/library/photo.jpg":    "jpeg",
		"legacy/docker-compose.yml":   "services:\n  web:\n    image: nginx:1.27\n",
		"global.env":                  "TZ=Europe/Prague\n", // Dockge's global env file: not a stack
		"notes/readme.txt":            "no compose file here",
		"Bad Name/compose.yaml":       "services:\n  x:\n    image: busybox\n",
		"immich/compose.override.yml": "services: {}\n",
	}
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	for p, c := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(full, old, old); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshotTree(t, root)

	e.SyncAllHistory()
	for _, name := range []string{"immich", "legacy"} {
		dir, err := e.stackDir(name)
		if err != nil || composeFile(dir) == "" {
			t.Fatalf("%s is not recognised as a stack: %v", name, err)
		}
		vs, err := e.StackHistory(name, 10)
		if err != nil || len(vs) != 1 || vs[0].Message != "First version found on disk" {
			t.Fatalf("%s history: %+v %v", name, vs, err)
		}
	}
	if got := filepath.Base(composeFile(filepath.Join(root, "legacy"))); got != "docker-compose.yml" {
		t.Errorf("legacy compose file: %s", got)
	}
	if vs, _ := e.StackHistory("notes", 10); len(vs) != 0 {
		t.Errorf("a folder without compose file became a stack")
	}

	after := snapshotTree(t, root)
	if len(before) != len(after) {
		t.Fatalf("files added or removed: before %d after %d", len(before), len(after))
	}
	for p, b := range before {
		if a, ok := after[p]; !ok || a != b {
			t.Errorf("%s changed: %+v -> %+v", p, b, a)
		}
	}
}

type fileState struct {
	content string
	mod     time.Time
	mode    fs.FileMode
}

func snapshotTree(t *testing.T, root string) map[string]fileState {
	t.Helper()
	out := map[string]fileState{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		st := fileState{mode: info.Mode()}
		if !d.IsDir() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			st.content, st.mod = string(b), info.ModTime()
		}
		out[p] = st
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
