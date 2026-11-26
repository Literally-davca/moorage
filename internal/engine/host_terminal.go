package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// HostShellBackend abstracts starting a host OS PTY session so tests can inject a fake.
type HostShellBackend interface {
	StartHostShell(ctx context.Context, shell string, cols, rows int) (TerminalSession, error)
}

// HostTerminal opens an interactive PTY in the host operating system when
// MOORAGE_HOST_TERMINAL=true (and MOORAGE_READ_ONLY=false).
// Native binary installs open a local PTY directly; container installs spawn a short-lived
// helper container (moorage.role=host-shell, pid:host) entering PID 1's namespaces via
// nsenter without requiring SSH keys. The helper is not AutoRemove: Moorage reads its exit
// code, error and last output first (so a failed start says why) and removes it on Close.
func (e *Engine) HostTerminal(ctx context.Context, shell string, cols, rows int) (TerminalSession, error) {
	if e.cfg.ReadOnly || !e.cfg.HostTerminal {
		return nil, ErrHostTerminalLocked
	}
	if shell == "" {
		shell = "bash"
	}
	shellPath, ok := AllowedShells[shell]
	if !ok {
		return nil, ErrInvalidShell
	}
	cols, rows = ClampTermSize(cols, rows)

	if e.HostShellBackend != nil {
		return e.HostShellBackend.StartHostShell(ctx, shell, cols, rows)
	}

	var (
		sess TerminalSession
		err  error
	)
	if !runningInContainer() {
		sess, err = startLocalPTY(shellPath, cols, rows)
	} else {
		sess, err = e.startContainerHostShell(ctx, shell, cols, rows)
	}
	if err != nil {
		return nil, err
	}
	return e.trackHostShell(sess), nil
}

func runningInContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		return true
	}
	// Scratch image without /bin/sh is also inside a container.
	if _, err := os.Stat("/bin/sh"); err != nil {
		return true
	}
	return false
}

type trackedHostShell struct {
	TerminalSession
	e    *Engine
	once sync.Once
}

func (e *Engine) trackHostShell(s TerminalSession) TerminalSession {
	t := &trackedHostShell{TerminalSession: s, e: e}
	e.termMu.Lock()
	if e.hostShells == nil {
		e.hostShells = map[io.Closer]struct{}{}
	}
	e.hostShells[t] = struct{}{}
	e.termMu.Unlock()
	return t
}

func (t *trackedHostShell) Close() error {
	var err error
	t.once.Do(func() {
		err = t.TerminalSession.Close()
		t.e.termMu.Lock()
		delete(t.e.hostShells, t)
		t.e.termMu.Unlock()
	})
	return err
}

func (e *Engine) findOwnContainerImage(ctx context.Context) (string, error) {
	if s := e.selfStack(); s != nil && s.Project != "" && s.Service != "" {
		f := make(client.Filters)
		f.Add("label", labelProject+"="+s.Project)
		f.Add("label", labelService+"="+s.Service)
		if list, err := e.api.ContainerList(ctx, client.ContainerListOptions{Filters: f}); err == nil {
			for _, item := range list.Items {
				if item.ImageID != "" {
					return item.ImageID, nil
				}
				if item.Image != "" {
					return item.Image, nil
				}
			}
		}
	}
	if hn, err := os.Hostname(); err == nil && hn != "" {
		if insp, err := e.api.ContainerInspect(ctx, hn, client.ContainerInspectOptions{}); err == nil {
			if insp.Container.Image != "" {
				return insp.Container.Image, nil
			}
			if insp.Container.Config != nil && insp.Container.Config.Image != "" {
				return insp.Container.Config.Image, nil
			}
		}
	}
	// Fallback: match container mounting our DataDir.
	cleanData := filepath.Clean(e.cfg.DataDir)
	if list, err := e.api.ContainerList(ctx, client.ContainerListOptions{}); err == nil {
		for _, item := range list.Items {
			for _, m := range item.Mounts {
				if filepath.Clean(m.Destination) == cleanData {
					if item.ImageID != "" {
						return item.ImageID, nil
					}
					return item.Image, nil
				}
			}
		}
	}
	return "", errors.New("could not determine Moorage container image to launch host shell helper")
}

func (e *Engine) startContainerHostShell(ctx context.Context, shell string, cols, rows int) (TerminalSession, error) {
	if e.api == nil {
		return nil, errors.New("docker engine is not available")
	}
	img, err := e.findOwnContainerImage(ctx)
	if err != nil {
		return nil, err
	}
	e.sweepHostShellHelpers(ctx)
	marker := termMarker()
	created, err := e.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:        img,
			Entrypoint:   []string{"/moorage", "host-shell-helper", shell},
			Tty:          true,
			OpenStdin:    true,
			StdinOnce:    true,
			AttachStdin:  true,
			AttachStdout: true,
			AttachStderr: true,
			Env:          []string{"TERM=xterm-256color", termMarkerVar + "=" + marker},
			Labels: map[string]string{
				LabelRole: RoleHostShell,
			},
		},
		HostConfig: &container.HostConfig{
			Privileged:  true,
			PidMode:     "host",
			NetworkMode: "host",
			IpcMode:     "host",
			UTSMode:     "host",
			ConsoleSize: [2]uint{uint(rows), uint(cols)},
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeBind,
					Source: "/",
					Target: "/host",
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create host shell helper: %w", err)
	}
	attached, err := e.api.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		_, _ = e.api.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
		return nil, fmt.Errorf("attach host shell helper: %w", err)
	}
	if _, err := e.api.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		attached.Close()
		_, _ = e.api.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
		return nil, fmt.Errorf("start host shell helper (it needs a privileged container with pid: host): %w", err)
	}
	_, _ = e.api.ContainerResize(ctx, created.ID, client.ContainerResizeOptions{
		Height: uint(rows),
		Width:  uint(cols),
	})
	return &containerHostShellSession{
		api:         e.api,
		containerID: created.ID,
		marker:      marker,
		hijack:      attached.HijackedResponse,
	}, nil
}

type containerHostShellSession struct {
	api         client.APIClient
	containerID string
	marker      string
	hijack      client.HijackedResponse
	closeOnce   sync.Once
}

func (s *containerHostShellSession) Read(p []byte) (int, error) {
	return s.hijack.Reader.Read(p)
}

func (s *containerHostShellSession) Write(p []byte) (int, error) {
	return s.hijack.Conn.Write(p)
}

func (s *containerHostShellSession) Resize(ctx context.Context, cols, rows int) error {
	cols, rows = ClampTermSize(cols, rows)
	_, err := s.api.ContainerResize(ctx, s.containerID, client.ContainerResizeOptions{
		Height: uint(rows),
		Width:  uint(cols),
	})
	return err
}

// quickExit is how soon after its start a helper that exits with an error counts as one
// that never came up (nsenter refused, missing privileges): then its last output is the
// reason the terminal shows.
const quickExit = 3 * time.Second

func (s *containerHostShellSession) Wait(ctx context.Context) (int, error) {
	for {
		insp, err := s.api.ContainerInspect(ctx, s.containerID, client.ContainerInspectOptions{})
		if err != nil {
			if isNotFound(err) {
				// Removed by someone else; the exit code is gone with it.
				return 0, errors.New("the host shell helper container was removed before Moorage could read its exit code")
			}
			return 0, err
		}
		st := insp.Container.State
		if st != nil && !st.Running {
			return st.ExitCode, helperFailure(ctx, s.api, s.containerID, st)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// helperFailure explains why a helper ended, or returns nil for a normal shell exit.
func helperFailure(ctx context.Context, api client.APIClient, id string, st *container.State) error {
	switch {
	case st.OOMKilled:
		return errors.New("the host shell helper ran out of memory")
	case st.Error != "":
		return fmt.Errorf("the host shell helper failed: %s", st.Error)
	case st.ExitCode == 0:
		return nil
	}
	started, err1 := time.Parse(time.RFC3339Nano, st.StartedAt)
	finished, err2 := time.Parse(time.RFC3339Nano, st.FinishedAt)
	if err1 != nil || err2 != nil || finished.Sub(started) > quickExit {
		return nil // a shell that ran and exited non-zero: the code says it all
	}
	tail := helperLogTail(ctx, api, id)
	if tail == "" {
		return fmt.Errorf("the host shell helper exited with code %d right after it started", st.ExitCode)
	}
	return fmt.Errorf("the host shell helper exited with code %d: %s", st.ExitCode, tail)
}

// helperLogTail is the last few lines the helper printed (TTY, so one stream).
func helperLogTail(ctx context.Context, api client.APIClient, id string) string {
	lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rc, err := api.ContainerLogs(lctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: "5"})
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, _ := io.ReadAll(io.LimitReader(rc, 4<<10))
	out := strings.Join(strings.Fields(string(b)), " ")
	if r := []rune(out); len(r) > 300 {
		out = string(r[len(r)-300:])
	}
	return out
}

func isNotFound(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "no such container") || strings.Contains(m, "not found")
}

// sweepHostShellHelpers removes helpers left behind by a hub that stopped mid-session
// (they are no longer AutoRemove). Only exited ones: a running helper is someone's shell.
func (e *Engine) sweepHostShellHelpers(ctx context.Context) {
	f := make(client.Filters)
	f.Add("label", LabelRole+"="+RoleHostShell)
	f.Add("status", "exited") // not "created": that may be another session starting now
	list, err := e.api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: f})
	if err != nil {
		return
	}
	for _, c := range list.Items {
		_, _ = e.api.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true})
	}
}

func (s *containerHostShellSession) Close() error {
	s.closeOnce.Do(func() {
		_ = s.hijack.CloseWrite()
		s.hijack.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = s.api.ContainerRemove(ctx, s.containerID, client.ContainerRemoveOptions{Force: true})
	})
	return nil
}
