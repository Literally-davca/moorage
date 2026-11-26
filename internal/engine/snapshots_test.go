package engine

import (
	"path/filepath"
	"testing"
)

func TestSnapshotHelpers(t *testing.T) {
	root := filepath.Join("opt", "stacks", "app")
	if !pathWithin(root, filepath.Join(root, "data")) || pathWithin(root, filepath.Join("opt", "stacks", "other")) || pathWithin(root, filepath.Join(root, "..", "x")) {
		t.Error("pathWithin")
	}
	if !validSnapID("20261126-120000-abcdef") || validSnapID("../x") || validSnapID("a/b") || validSnapID("") {
		t.Error("validSnapID")
	}
	framed := append([]byte{1, 0, 0, 0, 0, 0, 0, 5}, []byte("hello")...)
	if got := stripDockerLogHeaders(framed); got != "hello" {
		t.Errorf("%q", got)
	}
	if humanBytes(3<<20) != "3.0 MiB" || humanBytes(-1) != "unknown" {
		t.Error("humanBytes")
	}
}
