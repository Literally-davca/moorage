package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/client"
)

// Errors returned when starting a container terminal session.
var (
	ErrInvalidShell        = errors.New("pick a shell from the list: sh, bash or ash")
	ErrContainerNotRunning = errors.New("this container isn't running")
	ErrContainerNotInStack = errors.New("this container isn't part of the stack")
)

// AllowedShells maps short shell names accepted from the browser to their fixed container paths.
// No arbitrary commands or arguments are ever accepted.
var AllowedShells = map[string]string{
	"sh":   "/bin/sh",
	"bash": "/bin/bash",
	"ash":  "/bin/ash",
}

// TerminalSession is a live interactive TTY session inside a container.
type TerminalSession interface {
	io.ReadWriteCloser
	Resize(ctx context.Context, cols, rows int) error
	Wait(ctx context.Context) (exitCode int, err error)
}

// ExecBackend abstracts the Docker daemon calls needed for container exec so tests can use fakes.
type ExecBackend interface {
	StackContainers(ctx context.Context, stack string) ([]Container, error)
	StartExec(ctx context.Context, containerID, shellPath string, cols, rows int) (TerminalSession, error)
}

// ClampTermSize bounds terminal dimensions to 1..500.
func ClampTermSize(cols, rows int) (int, int) {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	cols = min(max(cols, 1), 500)
	rows = min(max(rows, 1), 500)
	return cols, rows
}

type mobyExecBackend struct {
	e *Engine
}

func (e *Engine) execBackend() ExecBackend {
	if e.ExecBackend != nil {
		return e.ExecBackend
	}
	return mobyExecBackend{e: e}
}

func (m mobyExecBackend) StackContainers(ctx context.Context, stack string) ([]Container, error) {
	byProj, _, err := m.e.projectContainers(ctx, stack)
	if err != nil {
		return nil, err
	}
	return byProj[stack], nil
}

func (m mobyExecBackend) StartExec(ctx context.Context, containerID, shellPath string, cols, rows int) (TerminalSession, error) {
	cols, rows = ClampTermSize(cols, rows)
	size := client.ConsoleSize{Height: uint(rows), Width: uint(cols)}
	marker := termMarker()
	created, err := m.e.api.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		TTY:          true,
		ConsoleSize:  size,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Env:          []string{"TERM=xterm-256color", termMarkerVar + "=" + marker},
		Cmd:          []string{shellPath},
	})
	if err != nil {
		return nil, FriendlyShellError(shellPath, err)
	}
	attached, err := m.e.api.ExecAttach(ctx, created.ID, client.ExecAttachOptions{
		TTY:         true,
		ConsoleSize: size,
	})
	if err != nil {
		return nil, FriendlyShellError(shellPath, err)
	}
	// Resize once right after attach in case the runtime ignored ConsoleSize on ExecCreate.
	_, _ = m.e.api.ExecResize(ctx, created.ID, client.ExecResizeOptions{
		Height: uint(rows),
		Width:  uint(cols),
	})
	s := &mobyTerminalSession{
		e:           m.e,
		api:         m.e.api,
		containerID: containerID,
		execID:      created.ID,
		shellPath:   shellPath,
		marker:      marker,
		hijack:      attached.HijackedResponse,
	}
	m.e.termMu.Lock()
	if m.e.terms == nil {
		m.e.terms = map[*mobyTerminalSession]struct{}{}
	}
	m.e.terms[s] = struct{}{}
	m.e.termMu.Unlock()
	return s, nil
}

// closeTerminals ends every open shell. A stopping agent would otherwise exit before the
// broken tunnel reaches its sessions, and the shells would keep running in the containers.
func (e *Engine) closeTerminals() {
	e.termMu.Lock()
	open := make([]*mobyTerminalSession, 0, len(e.terms))
	for s := range e.terms {
		open = append(open, s)
	}
	hostOpen := make([]io.Closer, 0, len(e.hostShells))
	for s := range e.hostShells {
		hostOpen = append(hostOpen, s)
	}
	e.termMu.Unlock()
	var wg sync.WaitGroup
	for _, s := range open {
		wg.Go(func() { _ = s.Close() })
	}
	for _, s := range hostOpen {
		wg.Go(func() { _ = s.Close() })
	}
	wg.Wait()
}

type mobyTerminalSession struct {
	e           *Engine
	api         client.APIClient
	containerID string
	execID      string
	shellPath   string
	marker      string // value of termMarkerVar in every process of this session
	hijack      client.HijackedResponse
	closeOnce   sync.Once
}

// Read passes the TTY through. A missing shell shows up as the runtime's own error in the
// stream and exit code 126/127, which Wait turns into a clear message.
func (s *mobyTerminalSession) Read(p []byte) (int, error) {
	return s.hijack.Reader.Read(p)
}

func (s *mobyTerminalSession) Write(p []byte) (int, error) {
	return s.hijack.Conn.Write(p)
}

func (s *mobyTerminalSession) Resize(ctx context.Context, cols, rows int) error {
	cols, rows = ClampTermSize(cols, rows)
	_, err := s.api.ExecResize(ctx, s.execID, client.ExecResizeOptions{
		Height: uint(rows),
		Width:  uint(cols),
	})
	return err
}

func (s *mobyTerminalSession) Wait(ctx context.Context) (int, error) {
	// The stream can end a moment before Docker records the exit; until then ExitCode is 0.
	var res client.ExecInspectResult
	for {
		var err error
		res, err = s.api.ExecInspect(ctx, s.execID, client.ExecInspectOptions{})
		if err != nil {
			if podmanExecGone(err) && s.e.isPodman(ctx) {
				return 0, nil // Podman already removed the finished session; its exit code is lost
			}
			return 0, err
		}
		if !res.Running {
			break
		}
		select {
		case <-ctx.Done():
			return res.ExitCode, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if res.ExitCode == 126 || res.ExitCode == 127 {
		return res.ExitCode, errors.New(missingShellMessage(s.shellPath))
	}
	return res.ExitCode, nil
}

func (s *mobyTerminalSession) Close() error {
	s.closeOnce.Do(func() {
		_ = s.hijack.CloseWrite()
		s.hijack.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		s.reap(ctx)
		s.e.termMu.Lock()
		delete(s.e.terms, s)
		s.e.termMu.Unlock()
	})
	return nil
}

// termMarkerVar marks every process a terminal session starts (children inherit it).
const termMarkerVar = "MOORAGE_TERMINAL_SESSION"

func termMarker() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// reap ends the shell and everything started from it. Docker has no "kill exec", and closing the
// attach stream of a TTY exec does not end it: on Docker 29 the shell and a running `sleep` kept
// going after the browser left. So a second exec with a fixed script (never input from the
// browser) signals every process that carries this session's marker: HUP first, the way a
// closed terminal would, then KILL for anything that ignored it.
func (s *mobyTerminalSession) reap(ctx context.Context) {
	if res, err := s.api.ExecInspect(ctx, s.execID, client.ExecInspectOptions{}); err == nil && !res.Running {
		return // the shell exited by itself (exit, Ctrl-D); its foreground children ended with it
	}
	for i, sig := range []string{"HUP", "KILL"} {
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
		script := `for d in /proc/[0-9]*; do case "$(cat "$d/environ" 2>/dev/null)" in *` +
			termMarkerVar + "=" + s.marker + `*) kill -` + sig + ` "${d#/proc/}" 2>/dev/null;; esac; done`
		created, err := s.api.ExecCreate(ctx, s.containerID, client.ExecCreateOptions{Cmd: []string{s.shellPath, "-c", script}})
		if err != nil {
			return
		}
		if _, err := s.api.ExecStart(ctx, created.ID, client.ExecStartOptions{Detach: true}); err != nil {
			return
		}
		waitExec(ctx, s.api, created.ID)
	}
}

// waitExec polls until a detached exec has finished or ctx ends.
func waitExec(ctx context.Context, api client.APIClient, id string) {
	for {
		res, err := api.ExecInspect(ctx, id, client.ExecInspectOptions{})
		if err != nil || !res.Running {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Terminal opens an interactive shell inside a running container that belongs to stack.
// It resolves the container among projectContainers(stack), so a container from another
// stack or Moorage's own container can never be accessed.
func (e *Engine) Terminal(ctx context.Context, stack, containerRef, shell string, cols, rows int) (TerminalSession, Container, error) {
	if stack == "" || containerRef == "" {
		return nil, Container{}, ErrNotFound
	}
	if shell == "" {
		shell = "sh"
	}
	shellPath, ok := AllowedShells[shell]
	if !ok {
		return nil, Container{}, ErrInvalidShell
	}
	cols, rows = ClampTermSize(cols, rows)

	b := e.execBackend()
	containers, err := b.StackContainers(ctx, stack)
	if err != nil {
		return nil, Container{}, err
	}
	var matched *Container
	for i := range containers {
		c := &containers[i]
		if c.ID == containerRef || c.Name == containerRef || c.Service == containerRef ||
			(len(containerRef) >= 12 && strings.HasPrefix(c.ID, containerRef)) {
			if matched == nil || c.State == "running" {
				matched = c
			}
			if c.ID == containerRef || c.Name == containerRef {
				break
			}
		}
	}
	if matched == nil {
		return nil, Container{}, ErrContainerNotInStack
	}
	if matched.State != "running" {
		return nil, *matched, ErrContainerNotRunning
	}

	sess, err := b.StartExec(ctx, matched.ID, shellPath, cols, rows)
	if err != nil {
		return nil, *matched, FriendlyShellError(shellPath, err)
	}
	return sess, *matched, nil
}

// AllExecBackend is an optional extension of ExecBackend for tests of ContainerTerminal.
type AllExecBackend interface {
	AllContainers(ctx context.Context) ([]ContainerInfo, error)
}

// ContainerTerminal opens an interactive shell inside any running container on the host
// (managed stack, external/Coolify project, or standalone container). Internal Moorage
// helpers and Moorage's own container are refused.
func (e *Engine) ContainerTerminal(ctx context.Context, containerRef, shell string, cols, rows int) (TerminalSession, ContainerInfo, error) {
	if containerRef == "" {
		return nil, ContainerInfo{}, ErrNotFound
	}
	if shell == "" {
		shell = "sh"
	}
	shellPath, ok := AllowedShells[shell]
	if !ok {
		return nil, ContainerInfo{}, ErrInvalidShell
	}
	cols, rows = ClampTermSize(cols, rows)

	b := e.execBackend()
	var list []ContainerInfo
	var err error
	if ab, ok := b.(AllExecBackend); ok {
		list, err = ab.AllContainers(ctx)
	} else {
		list, err = e.AllContainers(ctx)
	}
	if err != nil {
		return nil, ContainerInfo{}, err
	}
	var matched *ContainerInfo
	for i := range list {
		c := &list[i]
		if c.ID == containerRef || c.Name == containerRef ||
			(len(containerRef) >= 12 && strings.HasPrefix(c.ID, containerRef)) {
			if matched == nil || c.State == "running" {
				matched = c
			}
			if c.ID == containerRef || c.Name == containerRef {
				break
			}
		}
	}
	if matched == nil {
		return nil, ContainerInfo{}, ErrNotFound
	}
	if s := e.selfStack(); s != nil && matched.Project == s.Project && matched.Service == s.Service {
		return nil, *matched, ErrSelfStack
	}
	if matched.State != "running" {
		return nil, *matched, ErrContainerNotRunning
	}
	sess, err := b.StartExec(ctx, matched.ID, shellPath, cols, rows)
	if err != nil {
		return nil, *matched, FriendlyShellError(shellPath, err)
	}
	return sess, *matched, nil
}

func isMissingShellMessage(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "executable file not found") ||
		strings.Contains(lower, "no such file or directory") ||
		strings.Contains(lower, "oci runtime exec failed") ||
		strings.Contains(lower, "crun: executable file")
}

func missingShellMessage(shellPath string) string {
	if shellPath == "/bin/sh" {
		return "This container has no /bin/sh (distroless or scratch image has no shell)."
	}
	return fmt.Sprintf("This container has no %s. Try sh.", shellPath)
}

// FriendlyShellError translates OCI runtime missing-binary errors into a clear user message.
func FriendlyShellError(shellPath string, err error) error {
	if err == nil {
		return nil
	}
	if isMissingShellMessage(err.Error()) {
		return errors.New(missingShellMessage(shellPath))
	}
	return err
}
