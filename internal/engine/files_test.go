package engine

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestStackFiles(t *testing.T) {
	e := newSelfEngine(t, nil) // a stack "moorage" with compose.yaml
	dir, _ := e.stackDir("moorage")
	if err := os.MkdirAll(filepath.Join(dir, "conf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "conf", "app.ini"), []byte("a=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := e.ListStackFiles("moorage")
	if err != nil || len(list) < 2 {
		t.Fatalf("%v %v", list, err)
	}
	if s, err := e.ReadStackFile("moorage", "conf/app.ini"); err != nil || s != "a=1\n" {
		t.Fatalf("%q %v", s, err)
	}
	if err := e.WriteStackFile("moorage", "conf/app.ini", "a=2\n", "ann"); err != nil {
		t.Fatal(err)
	}
	vs, _ := e.StackFileVersions("moorage", "conf/app.ini")
	if len(vs) != 1 {
		t.Fatalf("versions %v", vs)
	}
	if old, err := e.StackFileVersion("moorage", "conf/app.ini", vs[0]); err != nil || old != "a=1\n" {
		t.Fatalf("old %q %v", old, err)
	}
	var ve ValidationError
	for _, bad := range []string{"../x", "compose.yaml", ".git/config", "/../../etc/passwd"} {
		if err := e.WriteStackFile("moorage", bad, "x", "ann"); err == nil {
			t.Errorf("%s must be refused", bad)
		}
	}
	if err := e.WriteStackFile("moorage", "nofolder/x.txt", "x", "ann"); !errors.As(err, &ve) {
		t.Errorf("a missing folder must be refused: %v", err)
	}
	if runtime.GOOS != "windows" {
		_ = os.Symlink("/etc", filepath.Join(dir, "link"))
		if _, err := e.ReadStackFile("moorage", "link/hostname"); !errors.As(err, &ve) {
			t.Errorf("a symlink must be refused: %v", err)
		}
	}
	if _, err := e.StackFileVersion("moorage", "conf/app.ini", "../../x"); err == nil {
		t.Error("a bad version id must be refused")
	}
}
