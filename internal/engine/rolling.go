package engine

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"moorage/internal/dockerclone"
)

// Rolling deploy ("Rolling / Safe-swap"): every service is updated on its own, so a broken
// new version never replaces a working one.
//
//   - Mode A, parallel swap (no downtime): stateless services behind a reverse proxy (no
//     published host port, no container_name, not a database). A new instance starts next
//     to the running one on the same networks and with the same labels; once it is healthy
//     (or ran 10 s without a healthcheck) the proxy gets 3 s to take over and the old one is
//     stopped. If the new one fails, only it is removed: the old one never stopped.
//   - Mode B, safe fast-swap: services with a fixed host port or state, where two instances
//     can't run at once. Images are pulled first, so the swap itself takes a moment; the
//     new container is watched for 30 s. If it exits or turns unhealthy it is removed and
//     the previous container comes back from its saved definition and previous image.
//
// A rollback ends the operation with an error that contains ErrRolledBack's text, which the
// hub turns into a Logbook entry, an alert, and policy notify for auto-updates.

// Deploy strategies of a stack.
const (
	StrategyStandard = "standard"
	StrategyRolling  = "rolling"
)

// ErrRolledBack marks a rolling deploy that put the previous version back.
var ErrRolledBack = errors.New("auto-rolled back to the previous container")

// Tunables (vars so tests can shorten them).
var (
	rollingHealthyLimit = 60 * time.Second // Mode A: the new instance must be healthy within this
	rollingStable       = 10 * time.Second // Mode A without healthcheck: running this long
	rollingDrain        = 3 * time.Second  // Mode A: the proxy takes over before the old one stops
	rollingWatch        = 30 * time.Second // Mode B: the new container is watched this long
	rollingPoll         = time.Second
)

// statefulImageRe matches images of databases and other stateful services (Mode B).
var statefulImageRe = regexp.MustCompile(`(?i)(^|/)(postgres|postgis|timescale|mysql|mariadb|mongo|redis|valkey|keydb|dragonfly|elasticsearch|opensearch|memcached|influxdb|clickhouse|cassandra|couchdb|rabbitmq|nats|kafka|zookeeper|etcd|minio|garage|qdrant|meilisearch|typesense|neo4j|mssql|oracle|cockroach|surrealdb|questdb|victoria-?metrics|prometheus|loki|gitea|forgejo|vaultwarden|nextcloud|immich)([:@/-]|$)`)

// SwapMode decides how a rolling deploy replaces the containers of a service: "parallel"
// (Mode A) or "fast-swap" (Mode B), with the reason.
func SwapMode(s types.ServiceConfig) (mode, reason string) {
	for _, p := range s.Ports {
		if p.Published != "" {
			return "fast-swap", "publishes host port " + p.Published
		}
	}
	switch {
	case s.ContainerName != "":
		return "fast-swap", "has a fixed container_name"
	case strings.EqualFold(s.Labels["moorage.stateful"], "true"):
		return "fast-swap", "labelled moorage.stateful=true"
	case statefulImageRe.MatchString(s.Image):
		return "fast-swap", "looks like a database or stateful service"
	case s.NetworkMode == "host":
		return "fast-swap", "uses the host network"
	}
	return "parallel", "stateless"
}

func (e *Engine) rollingUp(ctx context.Context, op *Op, svc api.Compose, project *types.Project) error {
	fmt.Fprintln(op, "Rolling deploy: pulling images first")
	if err := svc.Pull(ctx, project, api.PullOptions{IgnoreBuildable: true}); err != nil {
		return err
	}
	// Networks, volumes and services that don't run yet: a plain up that recreates nothing.
	never := api.UpOptions{
		Create: api.CreateOptions{Recreate: api.RecreateNever, RecreateDependencies: api.RecreateNever, Inherit: true},
		Start:  api.StartOptions{Project: project},
	}
	var order []string
	_ = project.ForEachService(project.ServiceNames(), func(name string, _ *types.ServiceConfig) error {
		order = append(order, name)
		return nil
	})
	for _, name := range order {
		sc := project.Services[name]
		olds, err := e.serviceContainers(ctx, project.Name, name)
		if err != nil {
			return err
		}
		var running []container.Summary
		for _, c := range olds {
			if c.State == container.StateRunning {
				running = append(running, c)
			}
		}
		if len(running) == 0 {
			continue // nothing runs: the final up creates or starts it
		}
		mode, why := SwapMode(sc)
		if mode == "parallel" {
			changed, err := e.serviceDiverged(ctx, sc, running)
			if err != nil {
				return err
			}
			if !changed {
				fmt.Fprintf(op, "%s: up to date\n", name)
				continue
			}
			fmt.Fprintf(op, "%s: parallel swap (%s)\n", name, why)
			if err := e.parallelSwap(ctx, op, svc, project, name, running); err != nil {
				return err
			}
			continue
		}
		fmt.Fprintf(op, "%s: safe fast-swap (%s)\n", name, why)
		if err := e.fastSwap(ctx, op, svc, project, name, running); err != nil {
			return err
		}
	}
	return svc.Up(ctx, project, never)
}

// serviceContainers lists the containers compose runs for a service (no one-off runs).
func (e *Engine) serviceContainers(ctx context.Context, project, service string) ([]container.Summary, error) {
	f := make(client.Filters)
	f.Add("label", labelProject+"="+project)
	f.Add("label", labelService+"="+service)
	res, err := e.api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: f})
	if err != nil {
		return nil, err
	}
	var out []container.Summary
	for _, c := range res.Items {
		if c.Labels["com.docker.compose.oneoff"] == "True" || IsInternalContainer(c) {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// serviceDiverged reports whether the running containers differ from the service as
// defined now: another image, or another compose configuration.
func (e *Engine) serviceDiverged(ctx context.Context, sc types.ServiceConfig, running []container.Summary) (bool, error) {
	want := ""
	if sc.Image != "" {
		if img, err := e.api.ImageInspect(ctx, sc.Image); err == nil {
			want = img.ID
		}
	}
	hash, herr := ServiceConfigHash(sc)
	for _, c := range running {
		if want != "" && c.ImageID != want {
			return true, nil
		}
		if herr == nil && c.Labels[api.ConfigHashLabel] != hash {
			return true, nil
		}
		if herr != nil {
			return true, nil // can't tell: swap, it costs no downtime
		}
	}
	return false, nil
}

func narrow(project *types.Project, name string) (*types.Project, error) {
	return project.WithSelectedServices([]string{name}, types.IgnoreDependencies)
}

// parallelSwap is Mode A.
func (e *Engine) parallelSwap(ctx context.Context, op *Op, svc api.Compose, project *types.Project, name string, olds []container.Summary) error {
	p, err := narrow(project, name)
	if err != nil {
		return err
	}
	n := len(olds)
	sc := p.Services[name]
	double := n * 2
	sc.Scale = &double
	if sc.Deploy != nil {
		d := *sc.Deploy
		d.Replicas = &double
		sc.Deploy = &d
	}
	p.Services[name] = sc
	if err := svc.Up(ctx, p, api.UpOptions{
		Create: api.CreateOptions{Recreate: api.RecreateNever, RecreateDependencies: api.RecreateNever, Inherit: true},
		Start:  api.StartOptions{Project: p},
	}); err != nil {
		e.removeNew(ctx, project.Name, name, olds)
		return fmt.Errorf("%s: starting the new instance failed, the old one kept running: %w", name, err)
	}
	news, err := e.newContainers(ctx, project.Name, name, olds)
	if err != nil || len(news) == 0 {
		return fmt.Errorf("%s: no new instance came up, the old one kept running", name)
	}
	for _, id := range news {
		if reason := e.waitHealthy(ctx, id, true, rollingHealthyLimit); reason != "" {
			fmt.Fprintf(op, "%s: new instance %s, removing it; the old one kept running\n", name, reason)
			e.removeNew(ctx, project.Name, name, olds)
			return fmt.Errorf("%s: new instance %s; the old one kept running", name, reason)
		}
	}
	fmt.Fprintf(op, "%s: new instance healthy, draining the old one for %s\n", name, rollingDrain)
	sleepCtx(ctx, rollingDrain)
	timeout := stopTimeout(sc)
	for _, c := range olds {
		_, _ = e.api.ContainerStop(ctx, c.ID, client.ContainerStopOptions{Timeout: &timeout})
		if _, err := e.api.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{}); err != nil {
			fmt.Fprintf(op, "%s: removing the old container failed: %v\n", name, err)
		}
	}
	fmt.Fprintf(op, "%s: swapped without downtime\n", name)
	return nil
}

// fastSwap is Mode B.
func (e *Engine) fastSwap(ctx context.Context, op *Op, svc api.Compose, project *types.Project, name string, olds []container.Summary) error {
	snaps := make([]container.InspectResponse, 0, len(olds))
	for _, c := range olds {
		r, err := e.api.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if err != nil {
			return err
		}
		snaps = append(snaps, r.Container)
	}
	p, err := narrow(project, name)
	if err != nil {
		return err
	}
	upErr := svc.Up(ctx, p, api.UpOptions{
		Create: api.CreateOptions{Recreate: api.RecreateDiverged, RecreateDependencies: api.RecreateNever, Inherit: true},
		Start:  api.StartOptions{Project: p},
	})
	news, _ := e.newContainers(ctx, project.Name, name, olds)
	if upErr == nil && len(news) == 0 {
		fmt.Fprintf(op, "%s: up to date\n", name)
		return nil
	}
	reason := ""
	if upErr != nil {
		reason = "didn't start: " + upErr.Error()
	} else {
		fmt.Fprintf(op, "%s: watching the new container for %s\n", name, rollingWatch)
		for _, id := range news {
			if reason = e.waitHealthy(ctx, id, false, rollingWatch); reason != "" {
				break
			}
		}
	}
	if reason == "" {
		fmt.Fprintf(op, "%s: new container is running\n", name)
		return nil
	}
	fmt.Fprintf(op, "%s: new container %s, putting the previous one back\n", name, reason)
	timeout := 10
	for _, id := range news {
		_, _ = e.api.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &timeout})
		_, _ = e.api.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
	}
	for _, snap := range snaps {
		if err := e.restoreSnapshot(ctx, snap); err != nil {
			return fmt.Errorf("%s: new container %s, and restoring the previous one failed: %v", name, reason, err)
		}
	}
	return fmt.Errorf("%s: new container %s; %w", name, reason, ErrRolledBack)
}

// restoreSnapshot recreates a container from its saved definition with its previous image.
func (e *Engine) restoreSnapshot(ctx context.Context, snap container.InspectResponse) error {
	name := strings.TrimPrefix(snap.Name, "/")
	if r, err := e.api.ContainerInspect(ctx, name, client.ContainerInspectOptions{}); err == nil && r.Container.ID != snap.ID {
		_, _ = e.api.ContainerRemove(ctx, r.Container.ID, client.ContainerRemoveOptions{Force: true})
	}
	if _, err := e.api.ContainerInspect(ctx, snap.ID, client.ContainerInspectOptions{}); err == nil {
		// compose kept the old container (it failed before removing it): just start it
		_, err = e.api.ContainerStart(ctx, snap.ID, client.ContainerStartOptions{})
		return err
	}
	cloned := dockerclone.CloneContainerConfig(snap, snap.Image)
	cloned.Create.Name = name
	created, err := e.api.ContainerCreate(ctx, cloned.Create)
	if err != nil {
		return err
	}
	for netName, ep := range cloned.ExtraNetworks {
		if _, err := e.api.NetworkConnect(ctx, netName, client.NetworkConnectOptions{Container: created.ID, EndpointConfig: ep}); err != nil {
			return err
		}
	}
	_, err = e.api.ContainerStart(ctx, created.ID, client.ContainerStartOptions{})
	return err
}

func (e *Engine) newContainers(ctx context.Context, project, service string, olds []container.Summary) ([]string, error) {
	cur, err := e.serviceContainers(ctx, project, service)
	if err != nil {
		return nil, err
	}
	old := map[string]bool{}
	for _, c := range olds {
		old[c.ID] = true
	}
	var out []string
	for _, c := range cur {
		if !old[c.ID] {
			out = append(out, c.ID)
		}
	}
	return out, nil
}

func (e *Engine) removeNew(ctx context.Context, project, service string, olds []container.Summary) {
	ids, _ := e.newContainers(ctx, project, service, olds)
	for _, id := range ids {
		_, _ = e.api.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
	}
}

// waitHealthy watches a new container. parallel (Mode A): done once healthy, or after
// rollingStable of running without a healthcheck, failing after rollingHealthyLimit.
// Otherwise (Mode B): it must keep running and not turn unhealthy for rollingWatch.
// It returns why the container failed, "" when it is fine.
func (e *Engine) waitHealthy(ctx context.Context, id string, parallel bool, limit time.Duration) string {
	start := time.Now()
	for {
		r, err := e.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			return "disappeared"
		}
		st := r.Container.State
		if st == nil {
			return "has no state"
		}
		switch {
		case st.Restarting:
			return "keeps restarting"
		case !st.Running:
			return fmt.Sprintf("exited with code %d", st.ExitCode)
		case st.Health != nil && st.Health.Status == container.Unhealthy:
			return "turned unhealthy"
		}
		elapsed := time.Since(start)
		hasHC := st.Health != nil && st.Health.Status != container.NoHealthcheck
		if parallel {
			if hasHC && st.Health.Status == container.Healthy {
				return ""
			}
			if !hasHC && elapsed >= rollingStable {
				return ""
			}
			if elapsed >= limit {
				return fmt.Sprintf("wasn't healthy within %s", limit)
			}
		} else if elapsed >= limit {
			return ""
		}
		if !sleepCtx(ctx, rollingPoll) {
			return "cancelled"
		}
	}
}

func stopTimeout(sc types.ServiceConfig) int {
	if sc.StopGracePeriod != nil {
		return max(1, int(time.Duration(*sc.StopGracePeriod).Seconds()))
	}
	return 10
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
