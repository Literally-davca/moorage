//go:build !windows

package stacksnap

import (
	"archive/tar"
	"io/fs"
	"os"
	"syscall"
)

func fillOwner(hdr *tar.Header, info fs.FileInfo) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		hdr.Uid, hdr.Gid = int(st.Uid), int(st.Gid)
	}
}

// applyOwner keeps the owner from the snapshot when running as root (the helper).
func applyOwner(p string, hdr *tar.Header, link bool) {
	if os.Geteuid() != 0 {
		return
	}
	if link {
		_ = os.Lchown(p, hdr.Uid, hdr.Gid)
		return
	}
	_ = os.Chown(p, hdr.Uid, hdr.Gid)
}
