package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"

	"moorage/internal/tsserve"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
)

// M19A: Apply & restart rolls back to the configuration Moorage last ran with, not to
// whatever is on disk (a Save before Apply already put the new files there).

// SelfGoodDir holds the compose file and .env Moorage last ran with successfully.
func SelfGoodDir(dataDir string) string { return filepath.Join(dataDir, "self-stack-good") }

// errNoRunning means no running container of the self service was found.
var errNoRunning = errors.New("the Moorage container isn't running")

// selfDiskMatchesRunning reports whether the compose files in dir describe the running
// Moorage container: the service's config-hash equals the label compose stamped on it.
// Formatting and comments don't break the match; any real change does.
func (e *Engine) selfDiskMatchesRunning(ctx context.Context, s *SelfStack, dir string) (bool, error) {
	if e.api == nil {
		return false, errNoRunning
	}
	file := composeFile(dir)
	if file == "" {
		return false, ErrNotFound
	}
	svc, err := compose.NewComposeService(e.cli)
	if err != nil {
		return false, err
	}
	load := api.ProjectLoadOptions{ProjectName: s.Project, ConfigPaths: []string{file}, WorkingDir: dir}
	if _, err := os.Stat(filepath.Join(dir, ".env")); err == nil {
		load.EnvFiles = []string{filepath.Join(dir, ".env")}
	}
	proj, err := svc.LoadProject(ctx, load)
	if err != nil {
		return false, err
	}
	sc, ok := proj.Services[s.Service]
	if !ok {
		return false, nil
	}
	hash, err := ServiceConfigHash(sc)
	if err != nil {
		return false, err
	}
	cs, err := e.serviceContainers(ctx, s.Project, s.Service)
	if err != nil {
		return false, err
	}
	for _, c := range cs {
		if c.State == "running" {
			return c.Labels[api.ConfigHashLabel] == hash, nil
		}
	}
	return false, errNoRunning
}

// SelfPending reports whether the self stack's files on disk differ from what runs
// (saved, not applied). Unknown (no container, load error) is false.
func (e *Engine) SelfPending(ctx context.Context, name string) bool {
	s := e.selfStack()
	if s == nil || s.Project != name {
		return false
	}
	dir, err := e.stackDir(name)
	if err != nil {
		return false
	}
	match, err := e.selfDiskMatchesRunning(ctx, s, dir)
	return err == nil && !match
}

// SelfRollbackAvailable reports whether an Apply & restart now would have a working
// configuration to return to.
func (e *Engine) SelfRollbackAvailable(ctx context.Context, name string) bool {
	s := e.selfStack()
	if s == nil || s.Project != name {
		return false
	}
	if hasSelfFiles(SelfGoodDir(e.cfg.DataDir)) {
		return true
	}
	dir, err := e.stackDir(name)
	if err != nil {
		return false
	}
	match, err := e.selfDiskMatchesRunning(ctx, s, dir)
	return err == nil && match
}

// RefreshSelfGood records the files on disk as the last working configuration when
// they match the running Moorage container. Called at startup.
func (e *Engine) RefreshSelfGood(ctx context.Context) {
	s := e.selfStack()
	if s == nil {
		return
	}
	dir, err := e.stackDir(s.Project)
	if err != nil {
		return
	}
	match, err := e.selfDiskMatchesRunning(ctx, s, dir)
	if err != nil {
		e.log.Debug("self stack: can't compare files with the running container", "err", err)
		return
	}
	if !match {
		e.log.Info("Moorage's own stack has saved changes that aren't applied yet", "stack", s.Project)
		return
	}
	if err := CopySelfFiles(dir, SelfGoodDir(e.cfg.DataDir)); err != nil {
		e.log.Warn("keep the working self stack configuration", "err", err)
	}
}

// CopySelfFiles copies the compose file and .env of dir into to (replacing it). A
// missing .env is kept as missing, so a restore removes one added later.
func CopySelfFiles(dir, to string) error {
	cf := composeFile(dir)
	if cf == "" {
		return ErrNotFound
	}
	tmp := to + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	b, err := os.ReadFile(cf)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, filepath.Base(cf)), b, 0o600); err != nil {
		return err
	}
	if b, err := os.ReadFile(filepath.Join(dir, ".env")); err == nil {
		if err := os.WriteFile(filepath.Join(tmp, ".env"), b, 0o600); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.RemoveAll(to); err != nil {
		return err
	}
	return os.Rename(tmp, to)
}

func hasSelfFiles(dir string) bool { return composeFile(dir) != "" }

// SameSelfFiles reports whether the compose file and .env in a and b are identical
// (both missing .env counts as identical).
func SameSelfFiles(a, b string) bool {
	fa, fb := composeFile(a), composeFile(b)
	if fa == "" || fb == "" || filepath.Base(fa) != filepath.Base(fb) {
		return false
	}
	ca, err1 := os.ReadFile(fa)
	cb, err2 := os.ReadFile(fb)
	if err1 != nil || err2 != nil || !bytes.Equal(ca, cb) {
		return false
	}
	ea, errA := os.ReadFile(filepath.Join(a, ".env"))
	eb, errB := os.ReadFile(filepath.Join(b, ".env"))
	if os.IsNotExist(errA) && os.IsNotExist(errB) {
		return true
	}
	return errA == nil && errB == nil && bytes.Equal(ea, eb)
}

// ErrSelfUpdateLocked refuses a change of the Moorage image on a node with
// MOORAGE_SELF_UPDATE=false (M19G): it is updated by hand on the host only.
var ErrSelfUpdateLocked = errors.New("This host is set to manual updates only (MOORAGE_SELF_UPDATE=false). Update Moorage on the host: docker compose pull && docker compose up -d – see docs/troubleshooting.md")

// selfImageChange reports the image the new self compose sets for the Moorage service
// when it differs from the image the running container was created from ("" = same,
// or unknown).
func (e *Engine) selfImageChange(ctx context.Context, s *SelfStack, composeText, env string) string {
	if e.api == nil {
		return ""
	}
	dir, err := e.stackDir(s.Project)
	if err != nil {
		return ""
	}
	proj, err := loadSelfProject(ctx, s.Project, dir, composeText, env)
	if err != nil {
		return ""
	}
	sc, ok := proj.Services[s.Service]
	if !ok || sc.Image == "" {
		return ""
	}
	cs, err := e.serviceContainers(ctx, s.Project, s.Service)
	if err != nil {
		return ""
	}
	for _, c := range cs {
		if c.State != "running" {
			continue
		}
		if c.Image != sc.Image {
			return sc.Image
		}
		return ""
	}
	return ""
}

// SelfUpdateLocked reports whether this node refuses image changes of its own stack.
func (e *Engine) SelfUpdateLocked() bool { return !e.cfg.SelfUpdate }

// loadSelfProject loads a draft of the self stack (not the files on disk).
func loadSelfProject(ctx context.Context, name, dir, composeText, env string) (*types.Project, error) {
	vars, err := dotenv.UnmarshalBytesWithLookup([]byte(env), nil)
	if err != nil {
		return nil, err
	}
	return loader.LoadWithContext(ctx, types.ConfigDetails{
		WorkingDir:  dir,
		ConfigFiles: []types.ConfigFile{{Filename: filepath.Join(dir, "compose.yaml"), Content: []byte(composeText)}},
		Environment: vars,
	}, func(o *loader.Options) {
		o.SetProjectName(name, true)
		o.SkipResolveEnvironment = true
	})
}

// Tailscale returns the Tailscale Serve client of this node (M19J), or an error when
// MOORAGE_TAILSCALE_SERVE is off.
func (e *Engine) Tailscale() (*tsserve.Client, error) {
	if !e.cfg.TailscaleServe {
		return nil, tsserve.ErrDisabled
	}
	e.selfMu.Lock()
	defer e.selfMu.Unlock()
	if e.ts == nil {
		e.ts = &tsserve.Client{Socket: e.cfg.TailscaleSocket, DataDir: e.cfg.DataDir}
	}
	return e.ts, nil
}
