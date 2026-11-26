package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"

	"moorage/internal/lint"
)

// MaxFileSize caps compose and .env files accepted from the UI.
const MaxFileSize = 512 * 1024

// StackSummary is one row of the stack list.
type StackSummary struct {
	Name     string `json:"name"`
	Managed  bool   `json:"managed"` // false = found running but outside the stacks folder (read-only)
	Status   string `json:"status"`
	Running  int    `json:"running"`
	Total    int    `json:"total"`
	Busy     bool   `json:"busy"`
	Location string `json:"location,omitempty"` // working dir of read-only stacks
	// Self marks the stack this Moorage node runs from: no stop/down/delete, changes go
	// through Apply & restart (self.go).
	Self      bool     `json:"self,omitempty"`
	URLs      []string `json:"urls,omitempty"`
	ProxyURLs []string `json:"proxyUrls,omitempty"`
	// AltURLs are the same port links on the host's other addresses (M17B).
	AltURLs []AltURL `json:"altUrls,omitempty"`

	// Optional Coolify enrichment populated by Hub when coolify-integration is active.
	DisplayName      string `json:"displayName,omitempty"`
	CoolifyUUID      string `json:"coolifyUuid,omitempty"`
	CoolifyType      string `json:"coolifyType,omitempty"`
	CoolifyProject   string `json:"coolifyProject,omitempty"`
	CoolifyEnv       string `json:"coolifyEnv,omitempty"`
	CoolifyURL       string `json:"coolifyUrl,omitempty"`
	CoolifyPR        string `json:"coolifyPr,omitempty"`
	CoolifyDeploying bool   `json:"coolifyDeploying,omitempty"`
}

// Stack is the detail view.
type Stack struct {
	StackSummary
	ComposeFile string      `json:"composeFile,omitempty"`
	Compose     string      `json:"compose"`
	Env         string      `json:"env"`
	Containers  []Container `json:"containers"`
	// SelfResult is the outcome of the last Apply & restart of the self stack.
	SelfResult *SelfApplyResult `json:"selfResult,omitempty"`
}

// ListStacks returns managed stacks from the folder plus read-only compose projects found in Docker.
func (e *Engine) ListStacks(ctx context.Context) ([]StackSummary, error) {
	byProject, dirs, dockerErr := e.projectContainers(ctx, "")
	if dockerErr != nil {
		byProject, dirs = map[string][]Container{}, map[string]string{}
	}
	rememberWorkingDirs(dirs)

	entries, err := os.ReadDir(e.cfg.StacksDir)
	if err != nil {
		return nil, err
	}
	var out []StackSummary
	seen := map[string]bool{}
	for _, ent := range entries {
		if !ent.IsDir() || !nameRe.MatchString(ent.Name()) {
			continue
		}
		if composeFile(filepath.Join(e.cfg.StacksDir, ent.Name())) == "" {
			continue
		}
		cs := byProject[ent.Name()]
		st, running := stackStatus(cs)
		urls, proxyURLs := collectStackURLs(cs)
		out = append(out, StackSummary{Name: ent.Name(), Managed: true, Status: st, Running: running, Total: len(cs), Busy: e.isBusy(ent.Name()), Self: e.IsSelf(ent.Name()), URLs: urls, ProxyURLs: proxyURLs})
		seen[ent.Name()] = true
	}
	// MOORAGE_EXTRA_STACKS_DIRS: more folders of fully managed stacks.
	if e.ManageExternal() {
		for _, root := range e.cfg.ExtraStacksDirs {
			ents, err := os.ReadDir(root)
			if err != nil {
				continue
			}
			for _, ent := range ents {
				n := ent.Name()
				if !ent.IsDir() || !nameRe.MatchString(n) || seen[n] || composeFile(filepath.Join(root, n)) == "" {
					continue
				}
				cs := byProject[n]
				st, running := stackStatus(cs)
				urls, proxyURLs := collectStackURLs(cs)
				out = append(out, StackSummary{Name: n, Managed: true, Status: st, Running: running, Total: len(cs), Busy: e.isBusy(n),
					Self: e.IsSelf(n), URLs: urls, ProxyURLs: proxyURLs, Location: filepath.Join(root, n)})
				seen[n] = true
			}
		}
	}
	for p, cs := range byProject {
		if seen[p] || p == "" {
			continue
		}
		if nameRe.MatchString(p) && e.ManageExternal() && usableWorkingDir(dirs[p]) {
			// a compose project whose folder Moorage can read and write: fully managed
			st, running := stackStatus(cs)
			urls, proxyURLs := collectStackURLs(cs)
			out = append(out, StackSummary{Name: p, Managed: true, Status: st, Running: running, Total: len(cs), Busy: e.isBusy(p),
				Location: dirs[p], Self: e.IsSelf(p), URLs: urls, ProxyURLs: proxyURLs})
			continue
		}
		st, running := stackStatus(cs)
		urls, proxyURLs := collectStackURLs(cs)
		out = append(out, StackSummary{Name: p, Managed: false, Status: st, Running: running, Total: len(cs), Location: dirs[p], Self: e.IsSelf(p), URLs: urls, ProxyURLs: proxyURLs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if dockerErr != nil {
		return out, fmt.Errorf("docker: %w", dockerErr)
	}
	return out, nil
}

// GetStack loads a stack with its files and containers.
func (e *Engine) GetStack(ctx context.Context, name string) (Stack, error) {
	if name == "" || len(name) > 63 {
		return Stack{}, ErrNotFound
	}
	byProject, dirs, err := e.projectContainers(ctx, name)
	if err != nil {
		return Stack{}, fmt.Errorf("docker: %w", err)
	}
	cs := byProject[name]
	if cs == nil {
		cs = []Container{}
	}
	st, running := stackStatus(cs)
	urls, proxyURLs := collectStackURLs(cs)
	s := Stack{
		StackSummary: StackSummary{Name: name, Status: st, Running: running, Total: len(cs), Busy: e.isBusy(name), Self: e.IsSelf(name), URLs: urls, ProxyURLs: proxyURLs},
		Containers:   cs,
	}
	if s.Self {
		s.SelfResult = e.SelfResult()
	}

	dir, derr := e.stackDir(name)
	if derr == nil {
		if f := composeFile(dir); f != "" {
			s.Managed = true
			s.ComposeFile = filepath.Base(f)
			b, err := os.ReadFile(f)
			if err != nil {
				return s, err
			}
			s.Compose = string(b)
			if b, err := os.ReadFile(filepath.Join(dir, ".env")); err == nil {
				s.Env = string(b)
			}
			return s, nil
		}
	}
	if len(cs) == 0 {
		return Stack{}, ErrNotFound
	}
	s.Location = dirs[name]
	return s, nil
}

// ValidateCompose parses compose (and .env) content exactly like docker compose would.
func (e *Engine) ValidateCompose(ctx context.Context, name, compose, env string) error {
	if len(compose) > MaxFileSize || len(env) > MaxFileSize {
		return ValidationError{"File is larger than 512 KB."}
	}
	if strings.TrimSpace(compose) == "" {
		return ValidationError{"Compose file is empty."}
	}
	dir, err := e.stackDir(name)
	if err != nil {
		return err
	}
	vars, err := dotenv.UnmarshalBytesWithLookup([]byte(env), nil)
	if err != nil {
		return ValidationError{".env: " + err.Error()}
	}
	details := types.ConfigDetails{
		WorkingDir:  dir,
		ConfigFiles: []types.ConfigFile{{Filename: filepath.Join(dir, "compose.yaml"), Content: []byte(compose)}},
		Environment: vars,
	}
	_, err = loader.LoadWithContext(ctx, details, func(o *loader.Options) {
		o.SetProjectName(name, true)
		o.SkipResolveEnvironment = true // env_file may not exist yet; that's checked on deploy
	})
	if err != nil {
		return ValidationError{friendlyComposeError(err.Error())}
	}
	return nil
}

var (
	// An optional drive letter keeps Windows paths (C:\...) from ending the match early.
	validatingPrefix = regexp.MustCompile(`^validating (?:[A-Za-z]:)?[^:]+: `)
	falseSchema      = regexp.MustCompile(`^(\S+) false schema$`)
	additionalProp   = regexp.MustCompile(`^(\S+) additional properties? '([^']+)' not allowed$`)
)

// friendlyComposeError hides host paths and rephrases the most common schema errors.
func friendlyComposeError(msg string) string {
	msg = validatingPrefix.ReplaceAllString(msg, "")
	if m := falseSchema.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("%s isn't a known compose option. Check the spelling.", m[1])
	}
	if m := additionalProp.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("%s.%s isn't a known compose option. Check the spelling.", m[1], m[2])
	}
	return msg
}

// SaveStack writes compose and .env after validation and records the result as
// a version by author. create=true refuses to overwrite.
func (e *Engine) SaveStack(ctx context.Context, name, compose, env, author string, create bool) error {
	return e.saveStack(ctx, name, compose, env, author, create, "")
}

// CreateStackNoted creates a stack like SaveStack(create=true) and uses note as the
// message of its first version (e.g. "Created from template postgres (bundled)").
func (e *Engine) CreateStackNoted(ctx context.Context, name, compose, env, author, note string) error {
	return e.saveStack(ctx, name, compose, env, author, true, CleanNote(note))
}

// CleanNote makes a version note safe for a commit message: one line, no control
// characters, at most 200 characters. A newline could otherwise forge the
// Moorage-Kind trailer that the timeline reads.
func CleanNote(note string) string {
	note = strings.TrimSpace(note)
	if i := strings.IndexAny(note, "\r\n"); i >= 0 {
		note = note[:i]
	}
	var b strings.Builder
	n := 0
	for _, r := range note {
		if r < 0x20 || r == 0x7f {
			continue
		}
		if n == 200 {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

func (e *Engine) saveStack(ctx context.Context, name, compose, env, author string, create bool, note string) error {
	dir, err := e.stackDir(name)
	if err != nil {
		return err
	}
	if err := e.ValidateCompose(ctx, name, compose, env); err != nil {
		return err
	}
	// The linter's blocking rules (no image, duplicate host ports, secrets written into
	// commands…) hold on every host, also when an older hub sends the file.
	if b := lint.Blocking(lint.Lint(ctx, compose, env, lint.Options{Name: name}), false); b != nil {
		return ValidationError{b.Message}
	}
	// The self stack keeps Moorage's service, socket and data mounts even when only saved:
	// a later Apply & restart or host reboot would otherwise start a Moorage cut off.
	if err := e.ValidateSelfCompose(ctx, name, compose, env); err != nil {
		return err
	}
	if !e.lock(name) {
		return ErrBusy
	}
	defer e.unlock(name)

	existing := composeFile(dir)
	if create && existing != "" {
		return ErrExists
	}
	if !create && existing == "" {
		return ErrNotFound
	}
	target := "compose.yaml"
	if existing != "" {
		target = filepath.Base(existing)
		e.syncExternal(name)
	}
	files := map[string][]byte{target: []byte(compose)}
	if env != "" {
		files[".env"] = []byte(env)
	}
	if err := e.writeStackFiles(dir, files); err != nil {
		return err
	}
	kind := KindEdit
	if create {
		kind = KindCreate
	}
	e.record(name, author, kind, "", func(changed []string) string {
		if create && note != "" {
			return note
		}
		if create {
			return "Create stack"
		}
		return "Edit " + strings.Join(changed, ", ")
	})
	return nil
}

// DeleteStack removes a managed stack folder. It refuses while containers exist.
// The last state stays in the version history.
func (e *Engine) DeleteStack(ctx context.Context, name, author string) error {
	if e.IsSelf(name) {
		return ErrSelfStack
	}
	dir, err := e.stackDir(name)
	if err != nil {
		return err
	}
	if composeFile(dir) == "" {
		return ErrNotFound
	}
	if !e.inStacksDir(dir) {
		// a stack in another folder is managed, but its folder isn't Moorage's to delete
		return ErrReadOnly
	}
	if !e.lock(name) {
		return ErrBusy
	}
	defer e.unlock(name)
	byProject, _, err := e.projectContainers(ctx, name)
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	if len(byProject[name]) > 0 {
		return ErrHasRunning
	}
	e.syncExternal(name)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	e.record(name, author, KindDelete, "", func([]string) string { return "Delete stack" })
	return nil
}

// writeFile writes atomically (temp file + rename) so a crash never leaves half a compose file.
func (e *Engine) writeFile(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".moorage-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	e.chown(tmp.Name())
	return os.Rename(tmp.Name(), path)
}

func (e *Engine) chown(path string) {
	if e.cfg.PUID < 0 {
		return
	}
	if err := os.Chown(path, e.cfg.PUID, e.cfg.PGID); err != nil {
		e.log.Warn("chown", "path", path, "err", err)
	}
}
