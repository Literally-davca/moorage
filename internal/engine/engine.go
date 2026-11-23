// Package engine talks to the Docker daemon: stacks (compose), containers, stats and logs.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/moby/moby/client"

	"moorage/internal/config"
)

// Errors returned to the API layer.
var (
	ErrNotFound    = errors.New("stack not found")
	ErrReadOnly    = errors.New("this stack lives outside the Moorage stacks folder and is read-only")
	ErrBusy        = errors.New("another operation is running on this stack")
	ErrExists      = errors.New("a stack with this name already exists")
	ErrHasRunning  = errors.New("stop the stack with Down before deleting it")
	ErrInvalidName = errors.New("use 1–63 characters: lowercase letters, digits, dash or underscore, starting with a letter or digit")
)

// ValidationError carries a compose validation message for the editor.
type ValidationError struct{ Msg string }

func (e ValidationError) Error() string { return e.Msg }

// Compose project names must be lowercase; the same rule keeps paths safe.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ComposeFileNames are recognised in this order, like docker compose.
var ComposeFileNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// Engine is the Docker-facing side of Moorage.
type Engine struct {
	cfg config.Config
	log *slog.Logger
	cli command.Cli
	api client.APIClient

	mu   sync.Mutex
	busy map[string]bool // stack name -> operation running

	ops  *opRegistry
	hist *History // nil when the history repo can't be opened

	// ImageBackend overrides the real Docker/registry image client in tests.
	ImageBackend ImageBackend
	// ExecBackend overrides the real Docker exec calls in tests.
	ExecBackend ExecBackend

	imgMu    sync.Mutex
	imgSem   chan struct{}
	imgCache map[string]cachedRemoteImage
	imgBusy  map[string]chan struct{} // refs being looked up right now; others wait for the result

	termMu sync.Mutex
	terms  map[*mobyTerminalSession]struct{} // open container shells, ended on Close

	engMu    sync.Mutex
	eng      engineIdent // Docker or Podman, from /version (see podman.go)
	engKnown bool
	ownWarn  sync.Once
}

// New connects to the Docker daemon from DOCKER_HOST or the default socket.
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (*Engine, error) {
	// A scratch image has no HOME; keep the docker CLI config inside the data dir.
	if os.Getenv("DOCKER_CONFIG") == "" && os.Getenv("HOME") == "" {
		os.Setenv("DOCKER_CONFIG", filepath.Join(cfg.DataDir, "docker"))
	}
	cli, err := command.NewDockerCli()
	if err != nil {
		return nil, err
	}
	if err := cli.Initialize(flags.NewClientOptions()); err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	e := &Engine{cfg: cfg, log: log, cli: cli, api: cli.Client(), busy: map[string]bool{}, ops: newOpRegistry()}
	if h, err := openHistory(filepath.Join(cfg.DataDir, "history.git")); err != nil {
		log.Error("version history disabled", "err", err)
	} else {
		e.hist = h
	}

	if _, ok := e.detectEngine(ctx); !ok {
		log.Warn("docker daemon not reachable yet", "host", os.Getenv("DOCKER_HOST"))
	}
	return e, nil
}

// Close releases the Docker client.
func (e *Engine) Close() error {
	e.closeTerminals()
	return e.api.Close()
}

// HostInfo describes the Docker host for the UI header.
type HostInfo struct {
	Name string `json:"name"`
	// DockerVersion is the engine's server version, kept for hubs older than Podman
	// support. For Podman it is Podman's version, the same as EngineVersion.
	DockerVersion string `json:"dockerVersion"`
	// Engine is "docker" or "podman"; empty when the engine isn't reachable (and from
	// agents older than Podman support, which read as Docker).
	Engine        string `json:"engine,omitempty"`
	EngineVersion string `json:"engineVersion,omitempty"`
	Rootless      bool   `json:"rootless,omitempty"`
	StacksDir     string `json:"stacksDir"`
	Reachable     bool   `json:"reachable"`
	Error         string `json:"error,omitempty"`
}

// Host returns basic information about the Docker host.
func (e *Engine) Host(ctx context.Context) HostInfo {
	h := HostInfo{StacksDir: e.cfg.StacksDir}
	if e.api == nil {
		h.Name, _ = os.Hostname()
		return h
	}
	info, err := e.api.Info(ctx, client.InfoOptions{})
	if err != nil {
		h.Error = "Docker isn't reachable: " + err.Error()
		h.Name, _ = os.Hostname()
		return h
	}
	h.Reachable = true
	h.Name = info.Info.Name
	h.DockerVersion = info.Info.ServerVersion
	h.Engine, h.EngineVersion = EngineDocker, info.Info.ServerVersion
	if id, ok := e.detectEngine(ctx); ok {
		h.Engine = id.Kind
		if id.Version != "" {
			h.EngineVersion = id.Version
		}
	}
	h.Rootless = isRootless(info.Info.SecurityOptions) || (h.Engine == EnginePodman && rootlessSocket(os.Getenv("DOCKER_HOST")))
	e.warnRootlessOwnership(h.Rootless)
	return h
}

func (e *Engine) lock(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.busy[name] {
		return false
	}
	e.busy[name] = true
	return true
}

func (e *Engine) unlock(name string) {
	e.mu.Lock()
	delete(e.busy, name)
	e.mu.Unlock()
}

func (e *Engine) isBusy(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.busy[name]
}

// stackDir returns the absolute directory of a managed stack after validating the name.
func (e *Engine) stackDir(name string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", ErrInvalidName
	}
	return filepath.Join(e.cfg.StacksDir, name), nil
}

// composeFile finds the compose file in dir, or "" when there is none.
func composeFile(dir string) string {
	for _, f := range ComposeFileNames {
		p := filepath.Join(dir, f)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p
		}
	}
	return ""
}
