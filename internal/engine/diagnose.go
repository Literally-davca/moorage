package engine

import (
	"context"
	"strings"
	"time"

	"github.com/moby/moby/client"

	"moorage/internal/diagnose"
)

// diagnoseLogLines / diagnoseLogBytes bound the log tail read for one unwell container.
const (
	diagnoseLogLines = 200
	diagnoseLogBytes = 64 << 10
)

// DiagnoseStack inspects the containers of a stack for the Problems card (M18F): state,
// exit code, OOM, restart count and policy, the last healthcheck outputs, the memory
// limit and the image architecture.
func (e *Engine) DiagnoseStack(ctx context.Context, stack string) ([]diagnose.Facts, error) {
	st, err := e.GetStack(ctx, stack)
	if err != nil {
		return nil, err
	}
	out := make([]diagnose.Facts, 0, len(st.Containers))
	for _, c := range st.Containers {
		out = append(out, e.containerFacts(ctx, c))
	}
	return out, nil
}

func (e *Engine) containerFacts(ctx context.Context, c Container) diagnose.Facts {
	f := BasicFacts(c)
	ir, err := e.api.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
	if err != nil {
		return f
	}
	in := ir.Container
	f.Detailed = true
	f.RestartCount = in.RestartCount
	if s := in.State; s != nil {
		f.ExitCode, f.OOMKilled, f.Error = s.ExitCode, s.OOMKilled, s.Error
		f.StartedAt, _ = time.Parse(time.RFC3339Nano, s.StartedAt)
		f.FinishedAt, _ = time.Parse(time.RFC3339Nano, s.FinishedAt)
		if f.FinishedAt.Year() < 2000 {
			f.FinishedAt = time.Time{}
		}
		if h := s.Health; h != nil {
			for _, r := range h.Log {
				if r != nil {
					f.HealthLog = append(f.HealthLog, strings.TrimSpace(r.Output))
				}
			}
			if n := len(f.HealthLog); n > 5 {
				f.HealthLog = f.HealthLog[n-5:]
			}
		}
	}
	if hc := in.HostConfig; hc != nil {
		f.RestartPolicy = string(hc.RestartPolicy.Name)
		f.MemLimit = hc.Memory
	}
	if img, err := e.api.ImageInspect(ctx, in.Image); err == nil {
		f.ImageArch = img.Architecture
	}
	f.HostArch = e.hostArch(ctx)
	return f
}

// BasicFacts is what the container list alone says (older agents, inspect failures).
func BasicFacts(c Container) diagnose.Facts {
	f := diagnose.Facts{ID: c.ID, Name: c.Name, Service: c.Service, Image: c.Image, State: c.State, Health: c.Health}
	if c.ExitCode != nil {
		f.ExitCode = *c.ExitCode
	}
	return f
}

// hostArch is the daemon's architecture in image notation (amd64, arm64…), cached.
func (e *Engine) hostArch(ctx context.Context) string {
	e.archOnce.Do(func() {
		if info, err := e.api.Info(ctx, client.InfoOptions{}); err == nil {
			e.arch = normalizeArch(info.Info.Architecture)
		}
	})
	return e.arch
}

func normalizeArch(a string) string {
	switch strings.ToLower(a) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "armv7l", "armhf", "arm":
		return "arm"
	}
	return strings.ToLower(a)
}
