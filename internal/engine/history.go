package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/utils/diff"
	"github.com/sergi/go-diff/diffmatchpatch"
)

// Version kinds, stored as a commit trailer so the UI can tell them apart.
const (
	KindCreate   = "create"
	KindEdit     = "edit"
	KindRestore  = "restore"
	KindExternal = "external" // files changed on disk outside Moorage
	KindImport   = "import"   // first snapshot of a stack Moorage found on disk
	KindDelete   = "delete"
)

// SystemAuthor signs versions Moorage records on its own (external changes).
const SystemAuthor = "moorage"

const (
	kindTrailer    = "Moorage-Kind: "
	restoreTrailer = "Moorage-Restored: "
	maxWalk        = 5000 // commits scanned when listing a stack's history
)

// ErrNoHistory means the version doesn't exist or doesn't touch the stack.
var ErrNoHistory = errors.New("version not found")

// History keeps every saved state of every stack in one bare git repository
// (<data>/history.git). Tree layout: <stack>/<compose file>, <stack>/.env.
// It never touches the stacks folder, so bind-mounted data and a user's own
// git repo there are left alone, and it never runs a git binary.
type History struct {
	mu   sync.Mutex
	path string
	repo *git.Repository
	st   storer.Storer
}

func openHistory(path string) (*History, error) {
	repo, err := git.PlainOpen(path)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		repo, err = git.PlainInitWithOptions(path, &git.PlainInitOptions{
			InitOptions: git.InitOptions{DefaultBranch: plumbing.Main},
			Bare:        true,
		})
	}
	if err != nil {
		return nil, err
	}
	return &History{path: path, repo: repo, st: repo.Storer}, nil
}

// Version is one entry of a stack's history.
type Version struct {
	Hash     string    `json:"hash"`
	Short    string    `json:"short"`
	Message  string    `json:"message"`
	Author   string    `json:"author"`
	Time     time.Time `json:"time"`
	Kind     string    `json:"kind"`
	Files    []string  `json:"files"`              // files changed in this version
	Restored string    `json:"restored,omitempty"` // short hash a restore came from
	Current  bool      `json:"current"`            // matches the files on disk now
}

// ---- writing ---------------------------------------------------------------

// commit records files (name -> content) as the new state of stack. An empty
// map records the stack's removal. It returns the version, or nil when nothing
// changed. message receives the names of changed files.
func (h *History) commit(stack string, files map[string][]byte, author, kind, restored string, message func(changed []string) string) (*Version, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	parent, root, err := h.head()
	if err != nil {
		return nil, err
	}
	oldTree, err := h.stackTree(root, stack)
	if err != nil {
		return nil, err
	}

	var newEntries []object.TreeEntry
	for name, data := range files {
		bh, err := h.writeBlob(data)
		if err != nil {
			return nil, err
		}
		newEntries = append(newEntries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: bh})
	}
	sort.Slice(newEntries, func(i, j int) bool { return newEntries[i].Name < newEntries[j].Name })

	changed := changedFiles(entriesOf(oldTree), newEntries)
	if len(changed) == 0 {
		return nil, nil
	}

	// New root: every other stack as before, this stack replaced or removed.
	var rootEntries []object.TreeEntry
	if root != nil {
		for _, e := range root.Entries {
			if e.Name != stack {
				rootEntries = append(rootEntries, e)
			}
		}
	}
	if len(newEntries) > 0 {
		th, err := h.writeTree(newEntries)
		if err != nil {
			return nil, err
		}
		rootEntries = append(rootEntries, object.TreeEntry{Name: stack, Mode: filemode.Dir, Hash: th})
	}
	// Git orders tree entries by name, with directories compared as "name/".
	sort.Slice(rootEntries, func(i, j int) bool { return rootEntries[i].Name+"/" < rootEntries[j].Name+"/" })
	rootHash, err := h.writeTree(rootEntries)
	if err != nil {
		return nil, err
	}

	msg := message(changed) + "\n\n" + kindTrailer + kind + "\nMoorage-Stack: " + stack + "\n"
	if restored != "" {
		msg += restoreTrailer + restored + "\n"
	}
	now := time.Now()
	sig := object.Signature{Name: author, Email: author + "@moorage.invalid", When: now}
	c := &object.Commit{Author: sig, Committer: sig, Message: msg, TreeHash: rootHash}
	var old *plumbing.Reference
	if parent != nil {
		c.ParentHashes = []plumbing.Hash{parent.Hash}
		old = plumbing.NewHashReference(plumbing.Main, parent.Hash)
	}
	obj := h.st.NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		return nil, err
	}
	ch, err := h.st.SetEncodedObject(obj)
	if err != nil {
		return nil, err
	}
	if err := h.st.CheckAndSetReference(plumbing.NewHashReference(plumbing.Main, ch), old); err != nil {
		return nil, err
	}
	if parent == nil {
		// Make sure HEAD points at main even if the repo was created elsewhere.
		_ = h.st.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.Main))
	}
	v := &Version{Hash: ch.String(), Short: ch.String()[:7], Message: firstLine(msg), Author: author, Time: now, Kind: kind, Files: changed, Restored: restored}
	return v, nil
}

func (h *History) writeBlob(data []byte) (plumbing.Hash, error) {
	obj := h.st.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := w.Write(data); err != nil {
		w.Close()
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	return h.st.SetEncodedObject(obj)
}

func (h *History) writeTree(entries []object.TreeEntry) (plumbing.Hash, error) {
	t := &object.Tree{Entries: entries}
	obj := h.st.NewEncodedObject()
	if err := t.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return h.st.SetEncodedObject(obj)
}

// ---- reading ---------------------------------------------------------------

// head returns the latest commit and its root tree, or nils for an empty repo.
func (h *History) head() (*object.Commit, *object.Tree, error) {
	ref, err := h.st.Reference(plumbing.Main)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	c, err := object.GetCommit(h.st, ref.Hash())
	if err != nil {
		return nil, nil, err
	}
	t, err := c.Tree()
	if err != nil {
		return nil, nil, err
	}
	return c, t, nil
}

// stackTree returns the subtree of one stack, or nil when the stack isn't there.
func (h *History) stackTree(root *object.Tree, stack string) (*object.Tree, error) {
	if root == nil {
		return nil, nil
	}
	for _, e := range root.Entries {
		if e.Name == stack && e.Mode == filemode.Dir {
			return object.GetTree(h.st, e.Hash)
		}
	}
	return nil, nil
}

func (h *History) stackTreeAt(c *object.Commit, stack string) (*object.Tree, error) {
	root, err := c.Tree()
	if err != nil {
		return nil, err
	}
	return h.stackTree(root, stack)
}

func entriesOf(t *object.Tree) []object.TreeEntry {
	if t == nil {
		return nil
	}
	return t.Entries
}

func treeHash(t *object.Tree) plumbing.Hash {
	if t == nil {
		return plumbing.ZeroHash
	}
	return t.Hash
}

// changedFiles lists names added, removed or modified between two entry lists.
func changedFiles(a, b []object.TreeEntry) []string {
	am := map[string]plumbing.Hash{}
	for _, e := range a {
		am[e.Name] = e.Hash
	}
	var out []string
	seen := map[string]bool{}
	for _, e := range b {
		seen[e.Name] = true
		if h, ok := am[e.Name]; !ok || h != e.Hash {
			out = append(out, e.Name)
		}
	}
	for _, e := range a {
		if !seen[e.Name] {
			out = append(out, e.Name)
		}
	}
	sort.Strings(out)
	return out
}

// Log lists the versions that changed stack, newest first. The newest one is
// marked current: callers sync external edits first, so it matches the disk.
func (h *History) Log(stack string, limit int) ([]Version, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, _, err := h.head()
	if err != nil || c == nil {
		return []Version{}, err
	}
	out := []Version{}
	first := true
	for i := 0; c != nil && i < maxWalk && len(out) < limit; i++ {
		cur, err := h.stackTreeAt(c, stack)
		if err != nil {
			return nil, err
		}
		var parent *object.Commit
		var prev *object.Tree
		if len(c.ParentHashes) > 0 {
			if parent, err = c.Parent(0); err != nil {
				return nil, err
			}
			if prev, err = h.stackTreeAt(parent, stack); err != nil {
				return nil, err
			}
		}
		if treeHash(cur) != treeHash(prev) {
			v := versionOf(c)
			v.Files = changedFiles(entriesOf(prev), entriesOf(cur))
			v.Current = first
			first = false
			out = append(out, v)
		}
		c = parent
	}
	return out, nil
}

func versionOf(c *object.Commit) Version {
	v := Version{Hash: c.Hash.String(), Short: c.Hash.String()[:7], Message: firstLine(c.Message), Author: c.Author.Name, Time: c.Author.When, Kind: KindEdit}
	for _, line := range strings.Split(c.Message, "\n") {
		switch {
		case strings.HasPrefix(line, kindTrailer):
			v.Kind = strings.TrimPrefix(line, kindTrailer)
		case strings.HasPrefix(line, restoreTrailer):
			v.Restored = strings.TrimPrefix(line, restoreTrailer)
		}
	}
	return v
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// filesAt returns the stack's files in commit hash and in its parent.
func (h *History) filesAt(hash, stack string) (Version, map[string][]byte, map[string][]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !plumbing.IsHash(hash) {
		return Version{}, nil, nil, ErrNoHistory
	}
	c, err := object.GetCommit(h.st, plumbing.NewHash(hash))
	if err != nil {
		return Version{}, nil, nil, ErrNoHistory
	}
	cur, err := h.stackTreeAt(c, stack)
	if err != nil {
		return Version{}, nil, nil, err
	}
	var prev *object.Tree
	if len(c.ParentHashes) > 0 {
		p, err := c.Parent(0)
		if err != nil {
			return Version{}, nil, nil, err
		}
		if prev, err = h.stackTreeAt(p, stack); err != nil {
			return Version{}, nil, nil, err
		}
	}
	if treeHash(cur) == treeHash(prev) {
		return Version{}, nil, nil, ErrNoHistory
	}
	after, err := h.readTree(cur)
	if err != nil {
		return Version{}, nil, nil, err
	}
	before, err := h.readTree(prev)
	if err != nil {
		return Version{}, nil, nil, err
	}
	v := versionOf(c)
	v.Files = changedFiles(entriesOf(prev), entriesOf(cur))
	return v, before, after, nil
}

func (h *History) readTree(t *object.Tree) (map[string][]byte, error) {
	out := map[string][]byte{}
	if t == nil {
		return out, nil
	}
	for _, e := range t.Entries {
		b, err := object.GetBlob(h.st, e.Hash)
		if err != nil {
			return nil, err
		}
		r, err := b.Reader()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(r, MaxFileSize+1))
		r.Close()
		if err != nil {
			return nil, err
		}
		out[e.Name] = data
	}
	return out, nil
}

// ---- stack files on disk ---------------------------------------------------

// readStackFiles returns the versioned files of a stack: its compose file and .env.
func readStackFiles(dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	if f := composeFile(dir); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		files[filepath.Base(f)] = b
	}
	b, err := os.ReadFile(filepath.Join(dir, ".env"))
	switch {
	case err == nil:
		files[".env"] = b
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	return files, nil
}

// writeStackFiles makes the versioned files on disk equal to files: writes
// each one atomically and removes versioned files that aren't in the map.
func (e *Engine) writeStackFiles(dir string, files map[string][]byte) error {
	prev, err := readStackFiles(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	e.chown(dir)
	for name, data := range files {
		if p, ok := prev[name]; ok && bytes.Equal(p, data) {
			continue
		}
		if err := e.writeFile(filepath.Join(dir, name), string(data)); err != nil {
			return err
		}
	}
	for name := range prev {
		if _, keep := files[name]; !keep {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// record commits the stack's current files. Failures are logged, never fatal:
// losing a history entry must not block a deploy.
func (e *Engine) record(stack, author, kind, restored string, message func([]string) string) *Version {
	if e.hist == nil {
		return nil
	}
	dir, err := e.stackDir(stack)
	if err != nil {
		return nil
	}
	files, err := readStackFiles(dir)
	if err != nil {
		e.log.Warn("history: read stack", "stack", stack, "err", err)
		return nil
	}
	if kind != KindDelete && len(files) == 0 {
		return nil
	}
	v, err := e.hist.commit(stack, files, author, kind, restored, message)
	if err != nil {
		e.log.Warn("history: commit", "stack", stack, "err", err)
		return nil
	}
	return v
}

// syncExternal records edits made on disk outside Moorage (ssh, editor) as their
// own version, so the next save or restore never hides who changed what.
func (e *Engine) syncExternal(stack string) {
	if e.hist == nil {
		return
	}
	if known, err := e.hist.hasStack(stack); err == nil && !known {
		e.record(stack, SystemAuthor, KindImport, "", func([]string) string { return "First version found on disk" })
		return
	}
	e.record(stack, SystemAuthor, KindExternal, "", func(changed []string) string {
		return "Changed outside Moorage: " + strings.Join(changed, ", ")
	})
}

// hasStack reports whether the latest version contains the stack.
func (h *History) hasStack(stack string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, root, err := h.head()
	if err != nil {
		return false, err
	}
	t, err := h.stackTree(root, stack)
	return t != nil, err
}

// SyncAllHistory records the current state of every managed stack (run at start).
func (e *Engine) SyncAllHistory() {
	if e.hist == nil {
		return
	}
	entries, err := os.ReadDir(e.cfg.StacksDir)
	if err != nil {
		return
	}
	for _, ent := range entries {
		if ent.IsDir() && nameRe.MatchString(ent.Name()) && composeFile(filepath.Join(e.cfg.StacksDir, ent.Name())) != "" {
			e.syncExternal(ent.Name())
		}
	}
}

// ---- public API ------------------------------------------------------------

// StackHistory lists versions of a managed stack, newest first.
func (e *Engine) StackHistory(name string, limit int) ([]Version, error) {
	if e.hist == nil {
		return nil, ErrHistoryOff
	}
	dir, err := e.stackDir(name)
	if err != nil {
		return nil, err
	}
	if composeFile(dir) == "" {
		return nil, ErrNotFound
	}
	if e.lock(name) { // don't record half-written files while an operation saves
		e.syncExternal(name)
		e.unlock(name)
	}
	return e.hist.Log(name, limit)
}

// ErrHistoryOff means the history repository couldn't be opened.
var ErrHistoryOff = errors.New("version history isn't available, check the Moorage log")

// FileDiff is the change of one file in a version.
type FileDiff struct {
	Name   string `json:"name"`
	Status string `json:"status"` // added | modified | deleted
	Hunks  []Hunk `json:"hunks"`
	Binary bool   `json:"binary,omitempty"`
}

// Hunk is a run of changed lines with context.
type Hunk struct {
	OldStart int        `json:"oldStart"`
	NewStart int        `json:"newStart"`
	Lines    []DiffLine `json:"lines"`
}

// DiffLine is one line of a hunk. Op is " ", "+" or "-".
type DiffLine struct {
	Op   string `json:"op"`
	Text string `json:"text"`
	Old  int    `json:"old,omitempty"`
	New  int    `json:"new,omitempty"`
}

// VersionDetail is a version with its diff against the previous one.
type VersionDetail struct {
	Version
	Diff []FileDiff `json:"diff"`
}

// StackVersion returns one version of a stack with its diff.
func (e *Engine) StackVersion(name, hash string) (VersionDetail, error) {
	if e.hist == nil {
		return VersionDetail{}, ErrHistoryOff
	}
	if _, err := e.stackDir(name); err != nil {
		return VersionDetail{}, err
	}
	v, before, after, err := e.hist.filesAt(hash, name)
	if err != nil {
		return VersionDetail{}, err
	}
	d := VersionDetail{Version: v, Diff: []FileDiff{}}
	for _, f := range v.Files {
		a, inA := before[f]
		b, inB := after[f]
		fd := FileDiff{Name: f, Status: "modified"}
		switch {
		case !inA:
			fd.Status = "added"
		case !inB:
			fd.Status = "deleted"
		}
		if bytes.IndexByte(a, 0) >= 0 || bytes.IndexByte(b, 0) >= 0 {
			fd.Binary = true
		} else {
			fd.Hunks = hunks(lineDiff(string(a), string(b)), 3)
		}
		d.Diff = append(d.Diff, fd)
	}
	return d, nil
}

// DeletedStack describes a stack that no longer exists on disk but has a restorable
// commit in history.git.
type DeletedStack struct {
	Name      string    `json:"name"`
	DeletedAt time.Time `json:"deletedAt"`
	Hash      string    `json:"hash"`
	Author    string    `json:"author"`
}

// DeletedStacks returns stacks present in history.git that have since been deleted.
func (e *Engine) DeletedStacks() ([]DeletedStack, error) {
	if e.hist == nil {
		return []DeletedStack{}, nil
	}
	e.hist.mu.Lock()
	defer e.hist.mu.Unlock()
	headCommit, headTree, err := e.hist.head()
	if err != nil || headCommit == nil || headTree == nil {
		return []DeletedStack{}, err
	}
	alive := map[string]bool{}
	for _, ent := range headTree.Entries {
		alive[ent.Name] = true
	}
	seen := map[string]bool{}
	out := []DeletedStack{}
	c := headCommit
	for i := 0; c != nil && i < maxWalk; i++ {
		if len(c.ParentHashes) == 0 {
			break
		}
		parent, err := c.Parent(0)
		if err != nil {
			return nil, err
		}
		curTree, err := c.Tree()
		if err != nil {
			return nil, err
		}
		prevTree, err := parent.Tree()
		if err != nil {
			return nil, err
		}
		curHas := map[string]bool{}
		for _, ent := range curTree.Entries {
			curHas[ent.Name] = true
		}
		for _, ent := range prevTree.Entries {
			if alive[ent.Name] || seen[ent.Name] || curHas[ent.Name] {
				continue
			}
			// Stack ent.Name was deleted in commit c; parent holds its last valid state.
			seen[ent.Name] = true
			out = append(out, DeletedStack{
				Name:      ent.Name,
				DeletedAt: c.Author.When.UTC(),
				Hash:      parent.Hash.String(),
				Author:    c.Author.Name,
			})
		}
		c = parent
	}
	return out, nil
}

// RestoreStack writes the stack's files from version hash back to disk and
// records that as a new version. Nothing is deployed here.
func (e *Engine) RestoreStack(ctx context.Context, name, hash, author string) (*Version, error) {
	if e.hist == nil {
		return nil, ErrHistoryOff
	}
	dir, err := e.stackDir(name)
	if err != nil {
		return nil, err
	}
	v, _, files, err := e.hist.filesAt(hash, name)
	if err != nil {
		return nil, err
	}
	compose, cname := "", ""
	for _, n := range ComposeFileNames {
		if b, ok := files[n]; ok {
			compose, cname = string(b), n
			break
		}
	}
	if cname == "" {
		return nil, ValidationError{"This version has no compose file (the stack was deleted in it). Pick an earlier version."}
	}
	if err := e.ValidateCompose(ctx, name, compose, string(files[".env"])); err != nil {
		var ve ValidationError
		if errors.As(err, &ve) {
			return nil, ValidationError{"This version no longer validates: " + ve.Msg}
		}
		return nil, err
	}
	if !e.lock(name) {
		return nil, ErrBusy
	}
	defer e.unlock(name)
	e.syncExternal(name)
	if err := e.writeStackFiles(dir, files); err != nil {
		return nil, err
	}
	nv := e.record(name, author, KindRestore, v.Short, func(changed []string) string {
		return fmt.Sprintf("Restore %s: %s", v.Short, strings.Join(changed, ", "))
	})
	return nv, nil
}

// ---- line diff -------------------------------------------------------------

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// lineDiff returns every line of a and b tagged as kept, removed or added.
func lineDiff(a, b string) []DiffLine {
	var out []DiffLine
	oldN, newN := 0, 0
	for _, d := range diff.Do(a, b) {
		for _, line := range splitLines(d.Text) {
			switch d.Type {
			case diffmatchpatch.DiffEqual:
				oldN++
				newN++
				out = append(out, DiffLine{Op: " ", Text: line, Old: oldN, New: newN})
			case diffmatchpatch.DiffDelete:
				oldN++
				out = append(out, DiffLine{Op: "-", Text: line, Old: oldN})
			case diffmatchpatch.DiffInsert:
				newN++
				out = append(out, DiffLine{Op: "+", Text: line, New: newN})
			}
		}
	}
	return out
}

// hunks keeps changed lines plus ctx lines of context around them.
func hunks(lines []DiffLine, ctx int) []Hunk {
	keep := make([]bool, len(lines))
	for i, l := range lines {
		if l.Op != " " {
			for j := max(0, i-ctx); j <= min(len(lines)-1, i+ctx); j++ {
				keep[j] = true
			}
		}
	}
	out := []Hunk{}
	for i := 0; i < len(lines); i++ {
		if !keep[i] {
			continue
		}
		h := Hunk{}
		for ; i < len(lines) && keep[i]; i++ {
			l := lines[i]
			if h.OldStart == 0 && l.Old > 0 {
				h.OldStart = l.Old
			}
			if h.NewStart == 0 && l.New > 0 {
				h.NewStart = l.New
			}
			h.Lines = append(h.Lines, l)
		}
		out = append(out, h)
	}
	return out
}
