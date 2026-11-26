// Package stacksnap writes and reads stack snapshots (M18F): one .tar.zst file with the
// stack's volumes, bind mounts and configuration, plus manifest.json inside (last entry)
// and next to the file. It runs inside the helper container (`moorage snapshot-helper`),
// so Moorage itself never copies application data and its memory use stays the same.
package stacksnap

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Consistency modes.
const (
	ModePause = "pause" // docker pause during the copy (default)
	ModeStop  = "stop"  // stop, copy, start
	ModeLive  = "live"  // no pause (a database may not restore)
)

// Kinds.
const (
	KindManual     = "manual"
	KindAuto       = "auto"        // before an update
	KindPreRestore = "pre-restore" // taken automatically before a restore
)

// Part kinds.
const (
	PartVolume = "volume" // named or anonymous volume
	PartBind   = "bind"   // bind mount
	PartConfig = "config" // compose.yaml + .env
)

// Part is one directory in the snapshot.
type Part struct {
	Name    string   `json:"name"` // path prefix in the tar: volumes/<name>, binds/<n>, config
	Kind    string   `json:"kind"`
	Source  string   `json:"source"`            // volume name or host path
	Target  string   `json:"target,omitempty"`  // mount path in the container
	Service string   `json:"service,omitempty"` // service that mounts it (anonymous volumes map by service + target)
	Users   []string `json:"users,omitempty"`   // other services sharing it
	Size    int64    `json:"size"`              // bytes before compression
	SHA256  string   `json:"sha256,omitempty"`  // of the part's tar stream
}

// Manifest describes a snapshot.
type Manifest struct {
	ID          string            `json:"id"`
	Stack       string            `json:"stack"`
	Host        string            `json:"host,omitempty"`
	Service     string            `json:"service,omitempty"` // container snapshot: only this service
	Kind        string            `json:"kind"`
	Mode        string            `json:"mode"`
	Pinned      bool              `json:"pinned,omitempty"`
	Note        string            `json:"note,omitempty"`
	Time        time.Time         `json:"time"`
	Version     string            `json:"version,omitempty"` // Moorage version
	Commit      string            `json:"commit,omitempty"`  // history.git version of compose.yaml
	Images      map[string]string `json:"images,omitempty"`  // service -> image digest or ref
	Parts       []Part            `json:"parts"`
	Size        int64             `json:"size"`           // file size (compressed)
	Uncompressd int64             `json:"uncompressed"`   // sum of part sizes
	File        string            `json:"file,omitempty"` // file name next to the manifest
}

// ManifestEntry is the name of the manifest inside the tar.
const ManifestEntry = "manifest.json"

// Write streams the parts (each a directory on disk) into w as tar + zstd and fills in
// sizes and checksums. dirs maps Part.Name to the directory to read.
func Write(w io.Writer, m *Manifest, dirs map[string]string) error {
	zw, err := zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return err
	}
	tw := tar.NewWriter(zw)
	m.Uncompressd = 0
	for i := range m.Parts {
		p := &m.Parts[i]
		dir, ok := dirs[p.Name]
		if !ok {
			return fmt.Errorf("no directory for part %s", p.Name)
		}
		h := sha256.New()
		size, err := addDir(tw, h, p.Name, dir)
		if err != nil {
			return fmt.Errorf("%s: %w", p.Name, err)
		}
		p.Size, p.SHA256 = size, hex.EncodeToString(h.Sum(nil))
		m.Uncompressd += size
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: ManifestEntry, Mode: 0o600, Size: int64(len(b)), ModTime: m.Time, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := tw.Write(b); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

// addDir writes the tree under dir with names prefix/<rel>. Symlinks are stored as links,
// never followed. The hash covers names, metadata and contents in walk order.
func addDir(tw *tar.Writer, h hash.Hash, prefix, dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		name := path.Join(prefix, filepath.ToSlash(rel))
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		if !info.Mode().IsRegular() && !info.IsDir() && info.Mode()&fs.ModeSymlink == 0 {
			return nil // sockets, devices, fifos: skipped
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = name
		if info.IsDir() {
			hdr.Name += "/"
		}
		fillOwner(hdr, info)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%o\x00%d\x00%s\x00", hdr.Name, hdr.Mode, hdr.Size, link)
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		n, err := io.Copy(io.MultiWriter(tw, h), f)
		f.Close()
		total += n
		return err
	})
	return total, err
}

// ReadManifest reads the manifest at the end of a snapshot (or from the JSON next to it).
func ReadManifest(r io.Reader) (Manifest, error) {
	var m Manifest
	found := false
	err := walk(r, func(hdr *tar.Header, body io.Reader) error {
		if hdr.Name == ManifestEntry {
			found = true
			return json.NewDecoder(io.LimitReader(body, 4<<20)).Decode(&m)
		}
		return nil
	})
	if err == nil && !found {
		err = errors.New("snapshot has no manifest")
	}
	return m, err
}

func walk(r io.Reader, fn func(*tar.Header, io.Reader) error) error {
	zr, err := zstd.NewReader(r)
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(hdr, tr); err != nil {
			return err
		}
	}
}

// Extract restores the parts listed in targets (Part.Name -> directory). Each target is
// emptied first. Paths that leave the target (.., absolute names, links pointing out)
// are refused. Owners and modes are kept when running as root.
func Extract(r io.Reader, targets map[string]string) error {
	names := make([]string, 0, len(targets))
	for n, dir := range targets {
		if err := emptyDir(dir); err != nil {
			return fmt.Errorf("empty %s: %w", dir, err)
		}
		names = append(names, n)
	}
	sort.Strings(names)
	var dirs []*tar.Header
	err := walk(r, func(hdr *tar.Header, body io.Reader) error {
		part, rel := splitPart(hdr.Name, names)
		if part == "" {
			return nil
		}
		root := targets[part]
		dst, err := safeJoin(root, rel)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
			h := *hdr
			h.Name = dst
			dirs = append(dirs, &h)
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(hdr.Mode)&fs.ModePerm)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, body); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			applyOwner(dst, hdr, false)
			_ = os.Chtimes(dst, hdr.ModTime, hdr.ModTime)
		case tar.TypeSymlink:
			// Links are restored as they were (absolute ones point at paths inside the
			// container, not on the host) and never followed here.
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			_ = os.Remove(dst)
			if err := os.Symlink(hdr.Linkname, dst); err != nil {
				return err
			}
			applyOwner(dst, hdr, true)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- { // directory modes last (a read-only dir would block its files)
		d := dirs[i]
		_ = os.Chmod(d.Name, fs.FileMode(d.Mode)&fs.ModePerm)
		applyOwner(d.Name, d, false)
		_ = os.Chtimes(d.Name, d.ModTime, d.ModTime)
	}
	return nil
}

func splitPart(name string, parts []string) (string, string) {
	name = strings.TrimSuffix(name, "/")
	for _, p := range parts {
		if name == p {
			return p, "."
		}
		if strings.HasPrefix(name, p+"/") {
			return p, strings.TrimPrefix(name, p+"/")
		}
	}
	return "", ""
}

func safeJoin(root, rel string) (string, error) {
	if strings.Contains(rel, "\\") || path.IsAbs(rel) {
		return "", fmt.Errorf("unsafe path %q in snapshot", rel)
	}
	clean := path.Clean("/" + rel)
	dst := filepath.Join(root, filepath.FromSlash(clean))
	if !within(root, dst) {
		return "", fmt.Errorf("unsafe path %q in snapshot", rel)
	}
	return dst, nil
}

func within(root, p string) bool {
	r, err := filepath.Rel(root, p)
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}

// emptyDir removes the contents of dir (not dir itself, which may be a mount point).
func emptyDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// DirSize sums the regular files under dir (for the estimate in the dialog).
func DirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
