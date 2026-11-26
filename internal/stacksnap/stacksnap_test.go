package stacksnap

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestRoundTrip(t *testing.T) {
	src := t.TempDir()
	cfg := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(src, "sub"), 0o755))
	must(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o640))
	must(t, os.WriteFile(filepath.Join(src, "sub", "b.bin"), bytes.Repeat([]byte{7}, 100000), 0o600))
	must(t, os.WriteFile(filepath.Join(cfg, "compose.yaml"), []byte("services: {}\n"), 0o644))
	if runtime.GOOS != "windows" {
		must(t, os.Symlink("a.txt", filepath.Join(src, "link")))
	}
	m := Manifest{ID: "x", Stack: "s", Kind: KindManual, Mode: ModePause, Time: time.Now().UTC(),
		Parts: []Part{{Name: "volumes/data", Kind: PartVolume, Source: "s_data"}, {Name: "config", Kind: PartConfig}}}
	var buf bytes.Buffer
	if err := Write(&buf, &m, map[string]string{"volumes/data": src, "config": cfg}); err != nil {
		t.Fatal(err)
	}
	if m.Parts[0].Size != 100005 || m.Parts[0].SHA256 == "" || m.Uncompressd != 100005+13 {
		t.Fatalf("%+v", m)
	}
	got, err := ReadManifest(bytes.NewReader(buf.Bytes()))
	if err != nil || got.Parts[0].SHA256 != m.Parts[0].SHA256 {
		t.Fatalf("%v %+v", err, got)
	}
	dst := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dst, "stale"), []byte("x"), 0o644))
	if err := Extract(bytes.NewReader(buf.Bytes()), map[string]string{"volumes/data": dst}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "stale")); !os.IsNotExist(err) {
		t.Error("target not emptied")
	}
	b, err := os.ReadFile(filepath.Join(dst, "sub", "b.bin"))
	if err != nil || len(b) != 100000 {
		t.Fatalf("%v %d", err, len(b))
	}
	if runtime.GOOS != "windows" {
		if l, err := os.Readlink(filepath.Join(dst, "link")); err != nil || l != "a.txt" {
			t.Errorf("link %q %v", l, err)
		}
		if st, _ := os.Stat(filepath.Join(dst, "a.txt")); st.Mode().Perm() != 0o640 {
			t.Errorf("mode %v", st.Mode())
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "compose.yaml")); !os.IsNotExist(err) {
		t.Error("config part extracted into the volume")
	}
}

func TestExtractRefusesEscapes(t *testing.T) {
	for _, name := range []string{"volumes/data/../../evil", "volumes/data/../x"} {
		var buf bytes.Buffer
		zw, _ := zstd.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		must(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}))
		_, _ = tw.Write([]byte("x"))
		must(t, tw.Close())
		must(t, zw.Close())
		root := t.TempDir()
		dst := filepath.Join(root, "dst")
		err := Extract(bytes.NewReader(buf.Bytes()), map[string]string{"volumes/data": dst})
		if _, serr := os.Stat(filepath.Join(root, "evil")); serr == nil {
			t.Fatalf("%s escaped", name)
		}
		_ = err // a cleaned path inside the target is fine; leaving it is not
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
