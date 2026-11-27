package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyAndSameSelfFiles(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, "compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CopySelfFiles(a, b); err != nil {
		t.Fatal(err)
	}
	if !SameSelfFiles(a, b) {
		t.Fatal("copy must be identical")
	}
	if _, err := os.Stat(filepath.Join(b, ".env")); !os.IsNotExist(err) {
		t.Fatal("a missing .env stays missing")
	}
	if err := os.WriteFile(filepath.Join(a, ".env"), []byte("X=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if SameSelfFiles(a, b) {
		t.Fatal("an added .env is a difference")
	}
	// the restore of b into a removes the .env added later
	if err := RestoreSelfBackup(a, b); err != nil {
		t.Fatal(err)
	}
	if !SameSelfFiles(a, b) {
		t.Fatal("restore must make the folders identical")
	}
	if hasSelfFiles(filepath.Join(root, "none")) {
		t.Fatal("missing folder has no files")
	}
}
