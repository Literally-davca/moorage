package engine

import (
	"archive/tar"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// ExportHistory writes history.git as a tar stream (paths relative to the repo). Writes
// to the history wait meanwhile, so the copy is consistent.
func (e *Engine) ExportHistory(w io.Writer) error {
	if e.hist == nil {
		return ErrHistoryOff
	}
	return e.hist.archive(w)
}

func (h *History) archive(w io.Writer) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(h.path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(h.path, p)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil // no symlinks or devices in a bare repo
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.CopyN(tw, f, info.Size())
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}
