package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/cmd/display"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
)

// ErrSelfStack refuses an action that would stop or remove the Moorage process itself.
var ErrSelfStack = errors.New("Moorage runs from this stack, so it can't be stopped, taken down or deleted from here. Use Restart, or Apply & restart for changes")

// SelfStack describes the compose stack this Moorage process runs from. It is set at
// startup from the container's own labels (binary installs have none).
type SelfStack struct {
	Project string // compose project name = stack name
	Service string // compose service of the Moorage container
	// SocketTarget and DataTarget are where the engine socket and the data directory
	// are mounted inside the Moorage container ("" when not mounted, e.g. DOCKER_HOST=tcp).
	SocketTarget string
	DataTarget   string
	// Launch starts the helper container that runs `compose up` for this stack from
	// outside Moorage, waits for it to come back and rolls back otherwise.
	Launch func(ctx context.Context) error
}

// selfBlocked are the actions refused on the self stack. Restart and pull stay allowed;
// up and update recreate the Moorage container from inside it, which would kill the
// operation halfway, so they go through SelfApply's helper instead.
var selfBlocked = map[string]bool{"down": true, "stop": true, "up": true, "up-local": true, "update": true}

// SetSelf records the stack Moorage runs from; nil means none (binary install).
func (e *Engine) SetSelf(s *SelfStack) {
	e.selfMu.Lock()
	e.self = s
	e.selfMu.Unlock()
}

func (e *Engine) selfStack() *SelfStack {
	e.selfMu.Lock()
	defer e.selfMu.Unlock()
	return e.self
}

// IsSelf reports whether name is the stack Moorage runs from.
func (e *Engine) IsSelf(name string) bool {
	s := e.selfStack()
	return s != nil && s.Project == name
}

// selfNotePath holds the outcome of the last Apply & restart, written by the helper.
func (e *Engine) selfNotePath() string { return filepath.Join(e.cfg.DataDir, "self-stack-result.json") }

// SelfBackupDir is where the files before an Apply & restart are kept for the helper's
// rollback.
func SelfBackupDir(dataDir string) string { return filepath.Join(dataDir, "self-stack-prev") }

// SelfStackResultFile is the name of the helper's result note in the data directory.
const SelfStackResultFile = "self-stack-result.json"

// ValidateSelfCompose refuses a compose file that would cut Moorage off: its service
// must stay, and so must the engine socket and the data mounts.
func (e *Engine) ValidateSelfCompose(ctx context.Context, name, composeText, env string) error {
	s := e.selfStack()
	if s == nil || s.Project != name {
		return nil
	}
	dir, err := e.stackDir(name)
	if err != nil {
		return err
	}
	vars, err := dotenv.UnmarshalBytesWithLookup([]byte(env), nil)
	if err != nil {
		return ValidationError{".env: " + err.Error()}
	}
	proj, err := loader.LoadWithContext(ctx, types.ConfigDetails{
		WorkingDir:  dir,
		ConfigFiles: []types.ConfigFile{{Filename: filepath.Join(dir, "compose.yaml"), Content: []byte(composeText)}},
		Environment: vars,
	}, func(o *loader.Options) {
		o.SetProjectName(name, true)
		o.SkipResolveEnvironment = true
	})
	if err != nil {
		return ValidationError{friendlyComposeError(err.Error())}
	}
	return checkSelfService(proj, s)
}

func checkSelfService(proj *types.Project, s *SelfStack) error {
	svc, ok := proj.Services[s.Service]
	if !ok {
		return ValidationError{fmt.Sprintf("Moorage runs as service %q of this stack. Keep that service, or Moorage would remove itself.", s.Service)}
	}
	has := func(target string) bool {
		for _, v := range svc.Volumes {
			if filepath.Clean(v.Target) == filepath.Clean(target) {
				return true
			}
		}
		return false
	}
	if s.SocketTarget != "" && !has(s.SocketTarget) {
		return ValidationError{fmt.Sprintf("Keep the engine socket mounted at %s in service %q: without it Moorage can't manage Docker.", s.SocketTarget, s.Service)}
	}
	if s.DataTarget != "" && !has(s.DataTarget) {
		return ValidationError{fmt.Sprintf("Keep the data volume mounted at %s in service %q: it holds accounts, checks and history.", s.DataTarget, s.Service)}
	}
	return nil
}

// SelfApply saves new files for the self stack and hands `compose up` to the helper
// container. The current files are kept so the helper can roll back when the new
// Moorage doesn't come up. The stack stays locked: the helper recreates this process.
func (e *Engine) SelfApply(ctx context.Context, name, composeText, env, author string) error {
	s := e.selfStack()
	if s == nil || s.Project != name {
		return ErrNotFound
	}
	if s.Launch == nil {
		return errors.New("apply & restart isn't available on this node")
	}
	if err := e.ValidateCompose(ctx, name, composeText, env); err != nil {
		return err
	}
	if err := e.ValidateSelfCompose(ctx, name, composeText, env); err != nil {
		return err
	}
	dir, err := e.stackDir(name)
	if err != nil {
		return err
	}
	if err := e.backupSelfFiles(dir); err != nil {
		return fmt.Errorf("keep current files for rollback: %w", err)
	}
	if err := e.saveStack(ctx, name, composeText, env, author, false, ""); err != nil {
		return err
	}
	if !e.lock(name) {
		return ErrBusy
	}
	_ = os.Remove(e.selfNotePath())
	if err := s.Launch(ctx); err != nil {
		// Nothing was applied: put the old files back so disk matches what runs.
		if rerr := RestoreSelfBackup(dir, SelfBackupDir(e.cfg.DataDir)); rerr != nil {
			e.log.Warn("restore self stack files", "err", rerr)
		}
		e.syncExternal(name)
		e.unlock(name)
		return fmt.Errorf("start the apply helper: %w", err)
	}
	e.log.Info("self stack apply handed to helper", "stack", name, "user", author)
	return nil
}

// backupSelfFiles copies the stack's compose file and .env (or the fact it has none).
func (e *Engine) backupSelfFiles(dir string) error {
	bdir := SelfBackupDir(e.cfg.DataDir)
	if err := os.RemoveAll(bdir); err != nil {
		return err
	}
	if err := os.MkdirAll(bdir, 0o700); err != nil {
		return err
	}
	cf := composeFile(dir)
	if cf == "" {
		return ErrNotFound
	}
	b, err := os.ReadFile(cf)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(bdir, filepath.Base(cf)), b, 0o600); err != nil {
		return err
	}
	if b, err := os.ReadFile(filepath.Join(dir, ".env")); err == nil {
		return os.WriteFile(filepath.Join(bdir, ".env"), b, 0o600)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// RestoreSelfBackup puts the kept compose file and .env back into dir. A .env that
// didn't exist before is removed.
func RestoreSelfBackup(dir, bdir string) error {
	entries, err := os.ReadDir(bdir)
	if err != nil {
		return err
	}
	hadEnv := false
	for _, ent := range entries {
		b, err := os.ReadFile(filepath.Join(bdir, ent.Name()))
		if err != nil {
			return err
		}
		if ent.Name() == ".env" {
			hadEnv = true
		}
		if err := os.WriteFile(filepath.Join(dir, ent.Name()), b, 0o644); err != nil {
			return err
		}
	}
	if !hadEnv {
		if err := os.Remove(filepath.Join(dir, ".env")); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// ComposeUpDir runs `docker compose up -d` for the project in dir, writing progress to
// out. The apply helper uses it from outside the Moorage container.
func (e *Engine) ComposeUpDir(ctx context.Context, project, dir string, out io.Writer) error {
	file := composeFile(dir)
	if file == "" {
		return ErrNotFound
	}
	svc, err := compose.NewComposeService(e.cli,
		compose.WithStreams(out, out, nil),
		compose.WithEventProcessor(display.Plain(out)),
		compose.WithPrompt(compose.AlwaysOkPrompt()),
	)
	if err != nil {
		return err
	}
	load := api.ProjectLoadOptions{ProjectName: project, ConfigPaths: []string{file}, WorkingDir: dir}
	if _, err := os.Stat(filepath.Join(dir, ".env")); err == nil {
		load.EnvFiles = []string{filepath.Join(dir, ".env")}
	}
	proj, err := svc.LoadProject(ctx, load)
	if err != nil {
		return err
	}
	return svc.Up(ctx, proj, api.UpOptions{Start: api.StartOptions{Project: proj}})
}

// SelfApplyResult is the helper's note about the last Apply & restart, shown on the
// stack page (<data>/self-stack-result.json).
type SelfApplyResult struct {
	At         time.Time `json:"at"`
	OK         bool      `json:"ok"`
	RolledBack bool      `json:"rolledBack"`
	Message    string    `json:"message"`
}

// SelfResult reads the helper's note about the last Apply & restart (nil when none).
func (e *Engine) SelfResult() *SelfApplyResult {
	b, err := os.ReadFile(e.selfNotePath())
	if err != nil {
		return nil
	}
	var r SelfApplyResult
	if json.Unmarshal(b, &r) != nil {
		return nil
	}
	return &r
}
