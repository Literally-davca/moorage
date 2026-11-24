//go:build linux

package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func startLocalPTY(shellPath string, cols, rows int) (TerminalSession, error) {
	if _, err := os.Stat(shellPath); err != nil {
		if shellPath != "/bin/sh" {
			if _, shErr := os.Stat("/bin/sh"); shErr == nil {
				shellPath = "/bin/sh"
			} else {
				return nil, FriendlyShellError(shellPath, err)
			}
		} else {
			return nil, FriendlyShellError(shellPath, err)
		}
	}

	ptmx, pts, err := openPTY(cols, rows)
	if err != nil {
		return nil, fmt.Errorf("open pty: %w", err)
	}

	home := os.Getenv("HOME")
	if home == "" {
		home = "/root"
	}
	cmd := exec.Command(shellPath, "-l")
	cmd.Dir = home
	if st, err := os.Stat(home); err != nil || !st.IsDir() {
		cmd.Dir = "/"
	}
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.Stdin = pts
	cmd.Stdout = pts
	cmd.Stderr = pts
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}
	if err := cmd.Start(); err != nil {
		_ = pts.Close()
		_ = ptmx.Close()
		return nil, FriendlyShellError(shellPath, err)
	}
	_ = pts.Close()

	s := &localPTYSession{
		ptmx:   ptmx,
		cmd:    cmd,
		doneCh: make(chan struct{}),
	}
	go func() {
		defer close(s.doneCh)
		err := cmd.Wait()
		s.mu.Lock()
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				s.exitCode = exitErr.ExitCode()
			} else {
				s.waitErr = err
			}
		}
		s.mu.Unlock()
	}()
	return s, nil
}

func openPTY(cols, rows int) (*os.File, *os.File, error) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	ptyNum, err := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPTN)
	if err != nil {
		_ = ptmx.Close()
		return nil, nil, err
	}
	if err := unix.IoctlSetPointerInt(int(ptmx.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		_ = ptmx.Close()
		return nil, nil, err
	}
	ptsName := fmt.Sprintf("/dev/pts/%d", ptyNum)
	pts, err := os.OpenFile(ptsName, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = ptmx.Close()
		return nil, nil, err
	}
	cols, rows = ClampTermSize(cols, rows)
	_ = unix.IoctlSetWinsize(int(ptmx.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: uint16(rows),
		Col: uint16(cols),
	})
	return ptmx, pts, nil
}

type localPTYSession struct {
	ptmx      *os.File
	cmd       *exec.Cmd
	doneCh    chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
	exitCode  int
	waitErr   error
}

func (s *localPTYSession) Read(p []byte) (int, error) {
	n, err := s.ptmx.Read(p)
	// On Linux, closing the slave PTY returns EIO on the master FD; treat that as clean EOF.
	if errors.Is(err, syscall.EIO) {
		return n, io.EOF
	}
	return n, err
}

func (s *localPTYSession) Write(p []byte) (int, error) {
	return s.ptmx.Write(p)
}

func (s *localPTYSession) Resize(_ context.Context, cols, rows int) error {
	cols, rows = ClampTermSize(cols, rows)
	return unix.IoctlSetWinsize(int(s.ptmx.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: uint16(rows),
		Col: uint16(cols),
	})
}

func (s *localPTYSession) Wait(ctx context.Context) (int, error) {
	select {
	case <-s.doneCh:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.exitCode, s.waitErr
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (s *localPTYSession) Close() error {
	s.closeOnce.Do(func() {
		if s.cmd != nil && s.cmd.Process != nil {
			pid := s.cmd.Process.Pid
			_ = syscall.Kill(-pid, syscall.SIGHUP)
			select {
			case <-s.doneCh:
			case <-time.After(800 * time.Millisecond):
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
		}
		_ = s.ptmx.Close()
	})
	return nil
}
