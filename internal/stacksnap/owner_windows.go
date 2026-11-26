package stacksnap

import (
	"archive/tar"
	"io/fs"
)

func fillOwner(*tar.Header, fs.FileInfo)   {}
func applyOwner(string, *tar.Header, bool) {}
