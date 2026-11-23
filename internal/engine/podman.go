package engine

// Podman support. Moorage talks to Podman through its Docker-compatible API socket
// (`podman system service` / podman.socket) with the same moby client it uses for Docker.
// Most calls behave the same; the helpers here cover the places where Podman's compat
// API really differs. Each one names the Podman source or issue it is based on.
//
// Compat API reference: https://docs.podman.io/en/latest/_static/api.html (the "compat" section).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Container engines Moorage can talk to.
const (
	EngineDocker = "docker"
	EnginePodman = "podman"
)

// engineIdent is what /version tells about the engine behind DOCKER_HOST.
type engineIdent struct {
	Kind    string // EngineDocker | EnginePodman
	Version string // engine version (Podman's own version for Podman, not the compat API level)
}

// identifyEngine reads the engine kind from a /version reply. Podman lists a component
// named "Podman Engine" (pkg/api/handlers/compat/version.go); Docker lists "Engine".
// Podman has no stable engine ID, so nothing in Moorage keys on Info.ID.
func identifyEngine(v client.ServerVersionResult) engineIdent {
	for _, c := range v.Components {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(c.Name)), "podman") {
			ver := c.Version
			if ver == "" {
				ver = v.Version
			}
			return engineIdent{Kind: EnginePodman, Version: ver}
		}
	}
	if strings.Contains(strings.ToLower(v.Platform.Name), "podman") {
		return engineIdent{Kind: EnginePodman, Version: v.Version}
	}
	return engineIdent{Kind: EngineDocker, Version: v.Version}
}

// isRootless reports a rootless engine. Both rootless Docker and rootless Podman list
// "name=rootless" in Info.SecurityOptions (Podman: pkg/api/handlers/compat/info.go).
func isRootless(securityOptions []string) bool {
	for _, o := range securityOptions {
		for _, part := range strings.Split(o, ",") {
			if strings.TrimSpace(part) == "name=rootless" {
				return true
			}
		}
	}
	return false
}

// rootlessSocket reports a DOCKER_HOST that points into a user's runtime dir
// ($XDG_RUNTIME_DIR = /run/user/<uid>), the standard place of the rootless Podman socket.
// Used only as a fallback when Info doesn't list "name=rootless".
func rootlessSocket(dockerHost string) bool {
	return strings.HasPrefix(dockerHost, "unix:///run/user/")
}

// detectEngine asks the engine who it is and caches the answer. A daemon that isn't
// reachable yet is asked again on the next call.
func (e *Engine) detectEngine(ctx context.Context) (engineIdent, bool) {
	e.engMu.Lock()
	defer e.engMu.Unlock()
	if e.engKnown {
		return e.eng, true
	}
	if e.api == nil {
		return engineIdent{Kind: EngineDocker}, false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	v, err := e.api.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return engineIdent{Kind: EngineDocker}, false
	}
	e.eng, e.engKnown = identifyEngine(v), true
	e.log.Info("connected to container engine", "engine", e.eng.Kind, "version", e.eng.Version, "api", v.APIVersion)
	return e.eng, true
}

// rootless reports whether the engine runs rootless (see isRootless and rootlessSocket).
func (e *Engine) rootless(ctx context.Context) bool {
	info, err := e.api.Info(ctx, client.InfoOptions{})
	if err != nil {
		return false
	}
	return isRootless(info.Info.SecurityOptions) || (e.isPodman(ctx) && rootlessSocket(os.Getenv("DOCKER_HOST")))
}

// isPodman reports whether DOCKER_HOST points at Podman. Unknown (daemon down) reads as Docker.
func (e *Engine) isPodman(ctx context.Context) bool {
	id, _ := e.detectEngine(ctx)
	return id.Kind == EnginePodman
}

// normalizeState maps Podman's own container states to Docker's. Podman's compat list
// reports a created container as "configured" and a container stopped by `stop` as
// "stopped" (LibpodToContainer in pkg/api/handlers/compat/containers.go, v4.9).
func normalizeState(s string) string {
	switch s {
	case "configured":
		return "created"
	case "stopped":
		return "exited"
	}
	return s
}

// healthFromStatus reads the health from the status text ("Up 3 hours (healthy)").
// Docker before API 1.52 and Podman's compat list have no Health field.
func healthFromStatus(status string) string {
	switch {
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(health: starting)"):
		return "starting"
	}
	return ""
}

// podmanHealth fills in the health of running containers the list left without it.
// Podman's compat list carries neither a Health field nor the health in the status
// text (v4.9), so each such container is inspected. Docker never takes this path.
func (e *Engine) podmanHealth(ctx context.Context, cs []Container) {
	for i := range cs {
		if cs[i].State != "running" || cs[i].Health != "" {
			continue
		}
		insp, err := e.api.ContainerInspect(ctx, cs[i].ID, client.ContainerInspectOptions{})
		if err != nil || insp.Container.State == nil || insp.Container.State.Health == nil {
			continue
		}
		if h := string(insp.Container.State.Health.Status); h != "" && h != "none" {
			cs[i].Health = h
		}
	}
}

// ---- stats ------------------------------------------------------------------

// Reasons shown instead of a number when the engine doesn't report a value.
const (
	noteMemEmpty    = "The engine reports no memory usage for this container (rootless without the cgroup memory controller, or memory accounting off)."
	noteCPUEmpty    = "The engine reports no CPU usage for this container (rootless without the cgroup cpu controller)."
	noteCPUNoSample = "CPU usage needs two samples; the second one wasn't returned."
)

// memStat fills the memory part of a sample. A running container never uses 0 bytes,
// so an empty memory_stats (rootless Podman on cgroups v1, or v2 without the memory
// controller delegated; Docker with memory accounting off) is "unknown", never 0.
func memStat(s *Stat, m container.MemoryStats) {
	if m.Usage == 0 {
		s.MemNote = noteMemEmpty
		return
	}
	s.MemUsed = memUsed(m)
	s.MemLimit = m.Limit
}

// podmanCPU computes CPU usage from two one-shot samples taken about a second apart:
// container CPU time over wall time, in percent of one core like `docker stats`.
//
// Podman's own precpu_stats can't be used: with stream=false it is empty, and its
// system_cpu_usage was the CPU time of all cgroups (not of the host) before Podman
// switched to /proc/stat (pkg/api/handlers/compat/containers_stats_linux.go, v4.9 vs main).
// The samples' read times are taken by Podman itself, so the request latency doesn't count.
func podmanCPU(first, second container.StatsResponse) (float64, string) {
	if first.CPUStats.CPUUsage.TotalUsage == 0 && second.CPUStats.CPUUsage.TotalUsage == 0 {
		return 0, noteCPUEmpty
	}
	wall := second.Read.Sub(first.Read)
	if first.Read.IsZero() || second.Read.IsZero() || wall <= 0 {
		return 0, noteCPUNoSample
	}
	if second.CPUStats.CPUUsage.TotalUsage < first.CPUStats.CPUUsage.TotalUsage {
		return 0, noteCPUNoSample // the container restarted between the samples
	}
	delta := float64(second.CPUStats.CPUUsage.TotalUsage - first.CPUStats.CPUUsage.TotalUsage)
	return delta / float64(wall.Nanoseconds()) * 100, ""
}

// podmanStatsGap is the time between the two Podman samples.
var podmanStatsGap = time.Second

// podmanSample takes two one-shot samples of a container (see podmanCPU).
func (e *Engine) podmanSample(ctx context.Context, id string) (Stat, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	first, err := e.oneShotStats(ctx, id)
	if err != nil {
		return Stat{}, err
	}
	select {
	case <-ctx.Done():
		return Stat{}, ctx.Err()
	case <-time.After(podmanStatsGap):
	}
	second, err := e.oneShotStats(ctx, id)
	if err != nil {
		return Stat{}, err
	}
	return podmanStat(id, first, second), nil
}

// podmanStat builds the sample from two Podman stats replies.
func podmanStat(id string, first, second container.StatsResponse) Stat {
	s := Stat{ID: id}
	s.CPU, s.CPUNote = podmanCPU(first, second)
	memStat(&s, second.MemoryStats)
	return s
}

func (e *Engine) oneShotStats(ctx context.Context, id string) (container.StatsResponse, error) {
	res, err := e.api.ContainerStats(ctx, id, client.ContainerStatsOptions{Stream: false})
	if err != nil {
		return container.StatsResponse{}, err
	}
	defer res.Body.Close()
	var v container.StatsResponse
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		return container.StatsResponse{}, fmt.Errorf("decode stats: %w", err)
	}
	return v, nil
}

// podmanStatsError turns a failed Podman stats call into a row with a reason, so the UI
// shows "–" with an explanation instead of silently nothing. Rootless Podman on cgroups v1
// can't read container stats at all.
func podmanStatsError(id string, err error) Stat {
	msg := "The engine didn't return stats: " + err.Error()
	if l := strings.ToLower(err.Error()); strings.Contains(l, "rootless") || strings.Contains(l, "cgroup") {
		msg = "Rootless Podman can't read container stats on this host (it needs cgroups v2 with the cpu and memory controllers delegated)."
	}
	return Stat{ID: id, CPUNote: msg, MemNote: msg}
}

// ---- compose operations -------------------------------------------------------

// podmanComposeWarnings lists known Podman gaps the project runs into, printed at the
// start of an operation so a hang or failure isn't a mystery.
//
// depends_on with condition service_healthy waits for a healthcheck. Podman runs
// healthchecks from systemd timers; without systemd (a rootless user without a session
// or linger, or Podman inside a container) they never run and compose waits until the
// operation times out (podman-run(1), --health-cmd: "uses systemd timers").
func podmanComposeWarnings(p *types.Project, rootless bool) []string {
	var out []string
	for _, name := range p.ServiceNames() {
		svc := p.Services[name]
		for dep, d := range svc.DependsOn {
			if d.Condition == types.ServiceConditionHealthy {
				out = append(out, fmt.Sprintf("Note (Podman): %s waits for %s to be healthy. Podman runs healthchecks from systemd timers; without systemd (or, rootless, without `loginctl enable-linger`) they never run and this waits until the operation times out.", name, dep))
			}
		}
		if rootless {
			for _, port := range svc.Ports {
				if n := publishedPort(port.Published); n > 0 && n < 1024 {
					out = append(out, fmt.Sprintf("Note (rootless Podman): %s publishes port %d. Ports below 1024 need net.ipv4.ip_unprivileged_port_start=%d (sysctl) or a higher port.", name, n, n))
				}
			}
		}
	}
	return out
}

// publishedPort returns the first host port of a published range ("8080", "80-81"), or 0.
func publishedPort(published string) int {
	first, _, _ := strings.Cut(published, "-")
	n := 0
	for _, r := range first {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
		if n > 65535 {
			return 0
		}
	}
	return n
}

// podmanOpHint explains Podman errors that are hard to read on their own.
func podmanOpHint(err error) string {
	if err == nil {
		return ""
	}
	l := strings.ToLower(err.Error())
	switch {
	case strings.Contains(l, "privileged port") || (strings.Contains(l, "rootlessport") && strings.Contains(l, "permission denied")):
		return "Rootless Podman can't publish ports below 1024. Set net.ipv4.ip_unprivileged_port_start (sysctl) on the host, or publish a port of 1024 or higher."
	case strings.Contains(l, "short-name") && strings.Contains(l, "did not resolve"):
		return "Podman couldn't resolve a short image name. Use the full name, e.g. docker.io/library/nginx."
	}
	return ""
}

// ---- terminal -------------------------------------------------------------------

// podmanExecGone reports an exec session Podman no longer knows. Podman may remove an
// exec session once it has exited, where Docker keeps it until the container goes, so
// "not found" right after the stream ended means the shell is gone.
func podmanExecGone(err error) bool {
	if err == nil {
		return false
	}
	l := strings.ToLower(err.Error())
	return strings.Contains(l, "no such exec") || strings.Contains(l, "no exec session")
}

// warnRootlessOwnership explains MOORAGE_PUID/PGID under rootless Podman once: files are
// owned by the user that runs Moorage, which is root inside the containers' user namespace,
// and a chown to another uid fails without privileges.
func (e *Engine) warnRootlessOwnership(rootless bool) {
	if !rootless || e.cfg.PUID < 0 || os.Geteuid() == 0 {
		return
	}
	e.ownWarn.Do(func() {
		e.log.Warn("MOORAGE_PUID/MOORAGE_PGID set with a rootless engine: files can only be given to the user that runs Moorage; leave both unset", "puid", e.cfg.PUID, "pgid", e.cfg.PGID)
	})
}
