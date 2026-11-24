package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

const maxExecOutput = 64 << 10

// ExecCommand runs a one-shot shell command inside a running service container of stack.
// Requires MOORAGE_EXEC=true or MOORAGE_TERMINAL=true (and never on MOORAGE_READ_ONLY=true).
func (e *Engine) ExecCommand(ctx context.Context, stack, service, command string) (output string, exitCode int, err error) {
	if e.cfg.ReadOnly {
		return "", 0, ErrReadOnly
	}
	if !e.cfg.Exec && !e.cfg.Terminal {
		return "", 0, ErrExecLocked
	}
	if stack == "" || strings.TrimSpace(command) == "" {
		return "", 0, errors.New("stack and command are required")
	}

	byProj, _, err := e.projectContainers(ctx, stack)
	if err != nil {
		return "", 0, err
	}
	var target *Container
	for i := range byProj[stack] {
		c := &byProj[stack][i]
		if c.State != "running" {
			continue
		}
		if service == "" || c.Service == service || c.Name == service {
			target = c
			break
		}
	}
	if target == nil {
		return "", 0, ErrContainerNotRunning
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	created, err := e.api.ExecCreate(ctx, target.ID, client.ExecCreateOptions{
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          []string{"/bin/sh", "-c", command},
	})
	if err != nil {
		return "", 0, err
	}
	attached, err := e.api.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return "", 0, err
	}
	defer attached.Close()

	var buf limitedWriter
	buf.max = maxExecOutput
	_, copyErr := stdcopy.StdCopy(&buf, &buf, attached.Reader)
	if copyErr != nil && !errors.Is(copyErr, io.EOF) && ctx.Err() == nil {
		return buf.String(), 0, copyErr
	}

	for {
		res, ierr := e.api.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
		if ierr != nil {
			if podmanExecGone(ierr) && e.isPodman(ctx) {
				return buf.String(), 0, nil
			}
			return buf.String(), 0, ierr
		}
		if !res.Running {
			if res.ExitCode != 0 {
				return buf.String(), res.ExitCode, fmt.Errorf("command exited with code %d", res.ExitCode)
			}
			return buf.String(), 0, nil
		}
		select {
		case <-ctx.Done():
			return buf.String(), 0, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

type limitedWriter struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.buf.Len() < l.max {
		rem := l.max - l.buf.Len()
		if len(p) > rem {
			_, _ = l.buf.Write(p[:rem])
			l.truncated = true
		} else {
			_, _ = l.buf.Write(p)
		}
	} else {
		l.truncated = true
	}
	return len(p), nil
}

func (l *limitedWriter) String() string {
	if l.truncated {
		return l.buf.String() + "\n… (output truncated at 64 KiB)"
	}
	return l.buf.String()
}
