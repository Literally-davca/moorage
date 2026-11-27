package engine

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ---- Files of a stack (M19M) ---------------------------------------------------------
//
// The Files tab reads and edits the text files of a managed stack (config files next to
// compose.yaml). Paths stay inside the stack folder and never pass through a symlink.
// compose.yaml and .env are edited in their own editors (they are versioned in
// history.git); for other files the previous contents are kept as the last 10 versions
// in <data>/file-history, so a restore of a compose version can never delete them.

var fileVersionRe = regexp.MustCompile(`^\d{8}T\d{6}\.\d{9}$`)

// MaxStackFile is the largest file the tab opens.
const MaxStackFile = 512 << 10

// ErrNotText refuses binary or oversized files.
var ErrNotText = errors.New("only text files up to 512 KB can be opened here")

// StackFile is one entry of the file list.
type StackFile struct {
	Path    string    `json:"path"` // slash separated, relative to the stack folder
	Size    int64     `json:"size"`
	Dir     bool      `json:"dir,omitempty"`
	Symlink bool      `json:"symlink,omitempty"`
	ModTime time.Time `json:"modTime"`
	// Managed: compose.yaml or .env (edit them in the stack editor).
	Managed bool `json:"managed,omitempty"`
}

func isManagedFile(rel string) bool {
	switch rel {
	case "compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml", ".env":
		return true
	}
	return false
}

// stackFilePath resolves rel inside the stack folder and refuses ".." and symlinks on the
// way (the last element too).
func (e *Engine) stackFilePath(stack, rel string) (string, string, error) {
	dir, err := e.stackDir(stack)
	if err != nil {
		return "", "", err
	}
	for _, seg := range strings.Split(strings.ReplaceAll(rel, "\\", "/"), "/") {
		if seg == ".." {
			return "", "", ValidationError{"Choose a file inside the stack folder."}
		}
	}
	clean := path.Clean("/" + strings.ReplaceAll(rel, "\\", "/"))[1:]
	if clean == "" || clean == "." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", "", ValidationError{"Choose a file inside the stack folder."}
	}
	cur := dir
	for _, part := range strings.Split(clean, "/") {
		if part == ".git" {
			return "", "", ValidationError{"The .git folder can't be edited here."}
		}
		cur = filepath.Join(cur, part)
		if fi, err := os.Lstat(cur); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return "", "", ValidationError{clean + " goes through a symbolic link; edit it on the host."}
		}
	}
	return filepath.Join(dir, filepath.FromSlash(clean)), clean, nil
}

// ListStackFiles lists the files of a managed stack (at most 500, depth 6).
func (e *Engine) ListStackFiles(stack string) ([]StackFile, error) {
	dir, err := e.stackDir(stack)
	if err != nil {
		return nil, err
	}
	if composeFile(dir) == "" {
		return nil, ErrNotFound
	}
	out := []StackFile{}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules" || strings.Count(rel, "/") >= 6) {
			return filepath.SkipDir
		}
		if len(out) >= 500 {
			return filepath.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		f := StackFile{Path: rel, Size: info.Size(), Dir: d.IsDir(), Symlink: info.Mode()&os.ModeSymlink != 0, ModTime: info.ModTime(), Managed: isManagedFile(rel)}
		out = append(out, f)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

// ReadStackFile returns a text file of a stack.
func (e *Engine) ReadStackFile(stack, rel string) (string, error) {
	p, _, err := e.stackFilePath(stack, rel)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", ErrNotFound
	}
	if fi.IsDir() || fi.Size() > MaxStackFile {
		return "", ErrNotText
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	if bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {
		return "", ErrNotText
	}
	return string(b), nil
}

// WriteStackFile saves a text file of a stack (new files only in existing folders) and
// keeps the previous contents.
func (e *Engine) WriteStackFile(stack, rel, content, author string) error {
	p, clean, err := e.stackFilePath(stack, rel)
	if err != nil {
		return err
	}
	if isManagedFile(clean) {
		return ValidationError{clean + " is edited in the stack editor (it is versioned there)."}
	}
	if len(content) > MaxStackFile || strings.IndexByte(content, 0) >= 0 || !utf8.ValidString(content) {
		return ErrNotText
	}
	if fi, err := os.Stat(filepath.Dir(p)); err != nil || !fi.IsDir() {
		return ValidationError{"The folder doesn't exist."}
	}
	if !e.lock(stack) {
		return ErrBusy
	}
	defer e.unlock(stack)
	mode := os.FileMode(0o644)
	if old, err := os.ReadFile(p); err == nil {
		if fi, err := os.Stat(p); err == nil {
			mode = fi.Mode().Perm()
		}
		e.keepFileVersion(stack, clean, old)
	}
	tmp := p + ".moorage-tmp"
	if err := os.WriteFile(tmp, []byte(content), mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	e.chown(p)
	e.log.Info("stack file saved", "stack", stack, "file", clean, "user", author)
	return nil
}

func (e *Engine) fileHistoryDir(stack, clean string) string {
	return filepath.Join(e.cfg.DataDir, "file-history", stack, filepath.FromSlash(clean))
}

// keepFileVersion stores the previous contents (last 10).
func (e *Engine) keepFileVersion(stack, clean string, old []byte) {
	d := e.fileHistoryDir(stack, clean)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(d, time.Now().UTC().Format("20060102T150405.000000000")), old, 0o600)
	ents, _ := os.ReadDir(d)
	for i := 0; i+10 < len(ents); i++ {
		_ = os.Remove(filepath.Join(d, ents[i].Name()))
	}
}

// StackFileVersions lists the kept versions of a file, newest first.
func (e *Engine) StackFileVersions(stack, rel string) ([]string, error) {
	_, clean, err := e.stackFilePath(stack, rel)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(e.fileHistoryDir(stack, clean))
	if err != nil {
		return []string{}, nil
	}
	var out []string
	for i := len(ents) - 1; i >= 0; i-- {
		out = append(out, ents[i].Name())
	}
	return out, nil
}

// StackFileVersion returns one kept version.
func (e *Engine) StackFileVersion(stack, rel, id string) (string, error) {
	_, clean, err := e.stackFilePath(stack, rel)
	if err != nil {
		return "", err
	}
	if !fileVersionRe.MatchString(id) {
		return "", ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(e.fileHistoryDir(stack, clean), id))
	if err != nil {
		return "", fmt.Errorf("%w", ErrNotFound)
	}
	return string(b), nil
}
