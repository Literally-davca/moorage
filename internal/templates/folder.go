package templates

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
)

// maxFolderTemplates bounds how many subfolders of the local folder are read.
const maxFolderTemplates = 500

var composeNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// item is one template of a source, or the reason it can't be used. A broken template
// stays in the catalogue with its error instead of disappearing.
type item struct {
	id  string
	t   Template
	err string
}

// loadFolders reads every subfolder of fsys as one template. One broken folder never
// hides the others. Without full only template.yaml is read and the other files are just
// checked, so listing a big folder stays cheap; Get reads the one template in full.
// A missing root is not an error: the source is simply empty.
func loadFolders(fsys fs.FS, full bool) ([]item, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read templates folder: %w", err)
	}
	var out []item
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if len(out) >= maxFolderTemplates {
			return out, fmt.Errorf("only the first %d template folders are read", maxFolderTemplates)
		}
		t, err := loadFolder(fsys, name, full)
		it := item{id: name, t: t}
		if err != nil {
			it.err = err.Error()
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out, nil
}

// loadFolder reads template.yaml, the compose file and the optional .env of one folder.
// The folder name must be the template id, so ids are unique within a folder.
func loadFolder(fsys fs.FS, dir string, full bool) (Template, error) {
	if !IDPattern.MatchString(dir) {
		return Template{}, errors.New("folder name must be a valid template id (lowercase letters, digits, dash or underscore)")
	}
	meta, ok, err := readFile(fsys, dir+"/template.yaml")
	if err != nil {
		return Template{}, err
	}
	if !ok {
		return Template{}, errors.New("template.yaml is missing")
	}
	m, err := ParseMeta(meta)
	if err != nil {
		return Template{}, err
	}
	if m.ID != dir {
		return Template{}, fmt.Errorf("id %q must match the folder name %q", m.ID, dir)
	}
	if !full {
		return Template{Meta: m}, checkFiles(fsys, dir)
	}
	var compose []byte
	for _, n := range composeNames {
		if compose, ok, err = readFile(fsys, dir+"/"+n); err != nil {
			return Template{}, err
		} else if ok {
			break
		}
	}
	env, _, err := readFile(fsys, dir+"/.env")
	if err != nil {
		return Template{}, err
	}
	return NewTemplate(m, string(compose), string(env))
}

// checkFiles finds the problems loadFolder would report for the files, without reading them.
func checkFiles(fsys fs.FS, dir string) error {
	found := false
	for _, n := range composeNames {
		st, err := fs.Stat(fsys, dir+"/"+n)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || !st.Mode().IsRegular() {
			return fmt.Errorf("%s is not a readable file", n)
		}
		if st.Size() == 0 {
			return errors.New("compose.yaml is missing or empty")
		}
		if st.Size() > MaxFileBytes {
			return fmt.Errorf("%s is larger than 1 MiB", n)
		}
		found = true
		break
	}
	if !found {
		return errors.New("compose.yaml is missing or empty")
	}
	if st, err := fs.Stat(fsys, dir+"/.env"); err == nil && (!st.Mode().IsRegular() || st.Size() > MaxFileBytes) {
		return errors.New(".env is not a regular file of at most 1 MiB")
	}
	return nil
}

// readFile reads a regular file of at most MaxFileBytes. ok is false when it doesn't exist.
func readFile(fsys fs.FS, name string) ([]byte, bool, error) {
	f, err := fsys.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("open %s: %w", baseName(name), err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("stat %s: %w", baseName(name), err)
	}
	if !st.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s is not a regular file", baseName(name))
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", baseName(name), err)
	}
	if len(b) > MaxFileBytes {
		return nil, false, fmt.Errorf("%s is larger than 1 MiB", baseName(name))
	}
	// Folders edited or checked out on Windows have CRLF endings; templates render with LF.
	return bytes.ReplaceAll(b, []byte{'\r', '\n'}, []byte{'\n'}), true, nil
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
