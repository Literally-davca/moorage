package selfupdate

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"moorage/internal/engine"
)

// HelperOptions configure a one-shot container replacement or rollback run.
type HelperOptions struct {
	TargetID   string
	NewImage   string
	FromVer    string
	ToVer      string
	DataDir    string
	IsHub      bool
	ListenAddr string
	Rollback   bool
	// StartDelay is waited after the helper reached the engine and before it stops the
	// target, so the node can confirm the helper is alive and answer the hub first.
	StartDelay time.Duration

	HealthTimeout time.Duration // defaults to 120s
	PollInterval  time.Duration // defaults to 1500ms
	HTTPClient    *http.Client
	Now           func() time.Time
	Sleep         func(time.Duration)
}

// ParseHelperFlags parses CLI flags passed to `moorage self-update-helper`.
func ParseHelperFlags(args []string) (HelperOptions, error) {
	var opt HelperOptions
	fs := flag.NewFlagSet("self-update-helper", flag.ContinueOnError)
	fs.StringVar(&opt.TargetID, "target", "", "target container ID or name")
	fs.StringVar(&opt.NewImage, "image", "", "verified image@digest")
	fs.StringVar(&opt.FromVer, "from", "", "previous version")
	fs.StringVar(&opt.ToVer, "to", "", "target version")
	fs.StringVar(&opt.DataDir, "data-dir", "", "node data directory")
	fs.BoolVar(&opt.IsHub, "hub", false, "target is the hub/all-in-one container")
	fs.StringVar(&opt.ListenAddr, "listen", ":5080", "hub listen address")
	fs.BoolVar(&opt.Rollback, "rollback", false, "roll back to <name>-moorage-prev")
	fs.DurationVar(&opt.StartDelay, "start-delay", helperStartDelay, "wait before stopping the target")
	if err := fs.Parse(args); err != nil {
		return opt, err
	}
	if opt.TargetID == "" {
		return opt, fmt.Errorf("--target is required")
	}
	if !opt.Rollback && opt.NewImage == "" {
		return opt, fmt.Errorf("--image is required")
	}
	return opt, nil
}

// RunHelper executes the container swap (or rollback) against eng. It is invoked by the
// one-shot helper container created from the new image (`moorage.role=updater`), and
// also directly in unit tests against a fake ContainerEngine.
func RunHelper(ctx context.Context, eng ContainerEngine, opt HelperOptions) error {
	if eng == nil {
		return fmt.Errorf("no container engine")
	}
	if opt.HealthTimeout <= 0 {
		opt.HealthTimeout = healthTimeout
	}
	if opt.PollInterval <= 0 {
		opt.PollInterval = 500 * time.Millisecond
	}
	nowFn := opt.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	sleepFn := opt.Sleep
	if sleepFn == nil {
		sleepFn = time.Sleep
	}

	// Reach the engine first, so a helper that can't (socket permissions, network) exits
	// at once and the node, still watching it, reports the failure to the hub.
	if _, err := eng.ContainerInspect(ctx, opt.TargetID, client.ContainerInspectOptions{}); err != nil {
		return fmt.Errorf("inspect target container %s: %w", opt.TargetID, err)
	}
	if opt.StartDelay > 0 {
		sleepFn(opt.StartDelay)
	}

	if opt.Rollback {
		return runContainerRollback(ctx, eng, opt, nowFn, sleepFn)
	}
	return runContainerUpgrade(ctx, eng, opt, nowFn, sleepFn)
}

// helperStartDelay must stay above ContainerApplier's default HelperCheckDelay (1.5 s).
const helperStartDelay = 2500 * time.Millisecond

// setRestartPolicy changes a container's restart policy, best effort.
func setRestartPolicy(ctx context.Context, eng ContainerEngine, id string, p container.RestartPolicy) error {
	_, err := eng.ContainerUpdate(ctx, id, client.ContainerUpdateOptions{RestartPolicy: &p})
	return err
}

func runContainerUpgrade(ctx context.Context, eng ContainerEngine, opt HelperOptions, nowFn func() time.Time, sleepFn func(time.Duration)) error {
	startedAt := nowFn()

	inspRes, err := eng.ContainerInspect(ctx, opt.TargetID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect target container %s: %w", opt.TargetID, err)
	}
	insp := inspRes.Container
	origName := strings.TrimPrefix(insp.Name, "/")
	if origName == "" {
		origName = opt.TargetID
	}
	prevName := origName + engine.PrevContainerSuffix

	// Remove any older kept rollback container so the new -moorage-prev name is free.
	if _, err := eng.ContainerInspect(ctx, prevName, client.ContainerInspectOptions{}); err == nil {
		_, _ = eng.ContainerRemove(ctx, prevName, client.ContainerRemoveOptions{Force: true})
	}

	stopTimeout := 15
	if _, err := eng.ContainerStop(ctx, opt.TargetID, client.ContainerStopOptions{Timeout: &stopTimeout}); err != nil {
		return fmt.Errorf("stop target container %s: %w", origName, err)
	}

	if _, err := eng.ContainerRename(ctx, opt.TargetID, client.ContainerRenameOptions{NewName: prevName}); err != nil {
		_, _ = eng.ContainerStart(ctx, opt.TargetID, client.ContainerStartOptions{})
		return fmt.Errorf("rename target container to %s: %w", prevName, err)
	}

	// The kept container must not come back on its own: with "always" a daemon restart
	// would start it next to the new one (same ports, same data). Its policy is kept in
	// the prev record and restored on rollback.
	var origPolicy container.RestartPolicy
	if insp.HostConfig != nil {
		origPolicy = insp.HostConfig.RestartPolicy
	}
	if err := setRestartPolicy(ctx, eng, opt.TargetID, container.RestartPolicy{Name: container.RestartPolicyDisabled}); err != nil {
		_, _ = eng.ContainerRename(ctx, opt.TargetID, client.ContainerRenameOptions{NewName: origName})
		_, _ = eng.ContainerStart(ctx, opt.TargetID, client.ContainerStartOptions{})
		return fmt.Errorf("disable restart policy of %s: %w", prevName, err)
	}

	restorePrev := func(reason string) error {
		if opt.DataDir != "" {
			writeRollbackNote(opt.DataDir, RollbackNote{
				FromVersion: opt.FromVer,
				ToVersion:   opt.ToVer,
				Reason:      "rolled back: " + reason,
				At:          nowFn(),
			})
			_ = os.Remove(filepath.Join(opt.DataDir, prevMetaFile))
		}
		_ = setRestartPolicy(ctx, eng, opt.TargetID, origPolicy)
		_, _ = eng.ContainerRename(ctx, opt.TargetID, client.ContainerRenameOptions{NewName: origName})
		_, _ = eng.ContainerStart(ctx, opt.TargetID, client.ContainerStartOptions{})
		return fmt.Errorf("rolled back: %s", reason)
	}

	cloned := CloneContainerConfig(insp, opt.NewImage)
	cloned.Create.Name = origName

	created, err := eng.ContainerCreate(ctx, cloned.Create)
	if err != nil {
		return restorePrev("create new container: " + err.Error())
	}
	for netName, ep := range cloned.ExtraNetworks {
		if _, err := eng.NetworkConnect(ctx, netName, client.NetworkConnectOptions{
			Container:      created.ID,
			EndpointConfig: ep,
		}); err != nil {
			_, _ = eng.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true})
			return restorePrev("attach network " + netName + ": " + err.Error())
		}
	}
	if _, err := eng.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		_, _ = eng.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true})
		return restorePrev("start new container: " + err.Error())
	}

	reason := waitContainerHealthy(ctx, eng, created.ID, startedAt, opt, nowFn, sleepFn)
	if reason != "" {
		_, _ = eng.ContainerStop(ctx, created.ID, client.ContainerStopOptions{Timeout: &stopTimeout})
		_, _ = eng.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true})
		return restorePrev(reason)
	}

	if opt.DataDir != "" {
		prevImage := ""
		if insp.Config != nil {
			prevImage = insp.Config.Image
		}
		writePrevRecord(opt.DataDir, PrevRecord{
			Mode:              "container",
			Version:           opt.FromVer,
			ToVersion:         opt.ToVer,
			SavedAt:           nowFn(),
			ContainerID:       created.ID,
			ContainerName:     origName,
			PrevContainer:     prevName,
			PrevImage:         prevImage,
			Compose:           cloned.Compose,
			PrevRestartPolicy: &origPolicy,
		})
	}
	return nil
}

func runContainerRollback(ctx context.Context, eng ContainerEngine, opt HelperOptions, nowFn func() time.Time, sleepFn func(time.Duration)) error {
	curRes, err := eng.ContainerInspect(ctx, opt.TargetID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect current container: %w", err)
	}
	origName := strings.TrimPrefix(curRes.Container.Name, "/")
	prevName := origName + engine.PrevContainerSuffix
	prevRes, err := eng.ContainerInspect(ctx, prevName, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect previous container %s: %w", prevName, err)
	}

	stopTimeout := 15
	if _, err := eng.ContainerStop(ctx, opt.TargetID, client.ContainerStopOptions{Timeout: &stopTimeout}); err != nil {
		return fmt.Errorf("stop current container: %w", err)
	}
	failedTempName := origName + "-moorage-rollback-tmp"
	if _, err := eng.ContainerRename(ctx, opt.TargetID, client.ContainerRenameOptions{NewName: failedTempName}); err != nil {
		_, _ = eng.ContainerStart(ctx, opt.TargetID, client.ContainerStartOptions{})
		return fmt.Errorf("rename current container: %w", err)
	}
	if _, err := eng.ContainerRename(ctx, prevRes.Container.ID, client.ContainerRenameOptions{NewName: origName}); err != nil {
		_, _ = eng.ContainerRename(ctx, opt.TargetID, client.ContainerRenameOptions{NewName: origName})
		_, _ = eng.ContainerStart(ctx, opt.TargetID, client.ContainerStartOptions{})
		return fmt.Errorf("rename previous container back: %w", err)
	}
	// The kept container had its restart policy disabled; give it back the original one
	// (from the prev record, else the current container's, which was cloned from it).
	_ = setRestartPolicy(ctx, eng, prevRes.Container.ID, keptRestartPolicy(opt.DataDir, curRes.Container))
	if _, err := eng.ContainerStart(ctx, prevRes.Container.ID, client.ContainerStartOptions{}); err != nil {
		_ = setRestartPolicy(ctx, eng, prevRes.Container.ID, container.RestartPolicy{Name: container.RestartPolicyDisabled})
		_, _ = eng.ContainerRename(ctx, prevRes.Container.ID, client.ContainerRenameOptions{NewName: prevName})
		_, _ = eng.ContainerRename(ctx, opt.TargetID, client.ContainerRenameOptions{NewName: origName})
		_, _ = eng.ContainerStart(ctx, opt.TargetID, client.ContainerStartOptions{})
		return fmt.Errorf("start restored container: %w", err)
	}

	_, _ = eng.ContainerRemove(ctx, opt.TargetID, client.ContainerRemoveOptions{Force: true})
	if opt.DataDir != "" {
		_ = os.Remove(filepath.Join(opt.DataDir, prevMetaFile))
	}

	// Briefly confirm restored container stays running.
	deadline := nowFn().Add(minDuration(15*time.Second, opt.HealthTimeout))
	for !nowFn().After(deadline) {
		stRes, err := eng.ContainerInspect(ctx, prevRes.Container.ID, client.ContainerInspectOptions{})
		if err == nil && stRes.Container.State != nil && stRes.Container.State.Running {
			if stRes.Container.State.Health == nil || stRes.Container.State.Health.Status == container.Healthy || stRes.Container.State.Health.Status == container.NoHealthcheck {
				return nil
			}
		}
		sleepFn(opt.PollInterval)
	}
	return nil
}

func keptRestartPolicy(dataDir string, current container.InspectResponse) container.RestartPolicy {
	if rec, ok := readPrevRecord(dataDir); ok && rec.PrevRestartPolicy != nil {
		return *rec.PrevRestartPolicy
	}
	if current.HostConfig != nil {
		return current.HostConfig.RestartPolicy
	}
	return container.RestartPolicy{Name: container.RestartPolicyDisabled}
}

func waitContainerHealthy(ctx context.Context, eng ContainerEngine, newID string, startedAt time.Time, opt HelperOptions, nowFn func() time.Time, sleepFn func(time.Duration)) string {
	deadline := startedAt.Add(opt.HealthTimeout)
	var runningSince time.Time
	httpCli := opt.HTTPClient
	if httpCli == nil {
		httpCli = &http.Client{Timeout: 3 * time.Second}
	}

	for {
		inspRes, err := eng.ContainerInspect(ctx, newID, client.ContainerInspectOptions{})
		if err == nil {
			insp := inspRes.Container
			st := insp.State
			if st != nil {
				if st.Restarting || st.Dead || (!st.Running && nowFn().Sub(startedAt) > 3*time.Second) {
					return fmt.Sprintf("new container failed to stay running (status %s, exit %d)", st.Status, st.ExitCode)
				}
				if st.Running {
					if runningSince.IsZero() {
						runningSince = nowFn()
					}
					if st.Health != nil && st.Health.Status == container.Unhealthy {
						return "new container reported unhealthy"
					}
					healthOK := st.Health == nil || st.Health.Status == container.Healthy || st.Health.Status == container.NoHealthcheck
					if healthOK {
						if opt.IsHub {
							if pollContainerSessionHealth(ctx, httpCli, insp, opt.ListenAddr, opt.ToVer) {
								return ""
							}
						} else if opt.DataDir != "" {
							// Agent with persistent /update-data writes healthy mark when tunnel handshakes.
							if mark, ok := readHealthyMark(opt.DataDir); ok &&
								strings.TrimPrefix(mark.Version, "v") == strings.TrimPrefix(opt.ToVer, "v") &&
								!mark.At.Before(startedAt.Add(-time.Second)) {
								return ""
							}
						} else if nowFn().Sub(runningSince) >= 10*time.Second {
							return ""
						}
					}
				} else {
					runningSince = time.Time{}
				}
			}
		}

		if !nowFn().Before(deadline) {
			return fmt.Sprintf("timed out waiting %s for new container %s to become healthy", opt.HealthTimeout, opt.ToVer)
		}
		select {
		case <-ctx.Done():
			return "update helper cancelled"
		default:
		}
		sleepFn(opt.PollInterval)
	}
}

func pollContainerSessionHealth(ctx context.Context, cli *http.Client, insp container.InspectResponse, listenAddr, wantVersion string) bool {
	_, port, err := net.SplitHostPort(strings.TrimSpace(listenAddr))
	if err != nil || port == "" {
		port = "5080"
	}
	var candidates []string
	if insp.NetworkSettings != nil {
		for _, ep := range insp.NetworkSettings.Networks {
			if ep != nil && ep.IPAddress.IsValid() {
				candidates = append(candidates, "http://"+net.JoinHostPort(ep.IPAddress.String(), port)+"/api/session")
			}
		}
	}
	candidates = append(candidates, "http://"+net.JoinHostPort("127.0.0.1", port)+"/api/session")

	for _, u := range candidates {
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, u, nil)
		if err != nil {
			cancel()
			continue
		}
		resp, err := cli.Do(req)
		if err != nil {
			cancel()
			continue
		}
		var s struct {
			Version string `json:"version"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&s)
		_ = resp.Body.Close()
		cancel()
		if resp.StatusCode == http.StatusOK && strings.TrimPrefix(s.Version, "v") == strings.TrimPrefix(wantVersion, "v") {
			return true
		}
	}
	return false
}

// CleanupExpiredPrev removes kept moorage.prev binaries and -moorage-prev containers after 24 h.
func CleanupExpiredPrev(ctx context.Context, eng ContainerEngine, dataDir string, now time.Time) {
	if dataDir == "" {
		return
	}
	rec, ok := readPrevRecord(dataDir)
	if !ok || rec.SavedAt.IsZero() || now.Sub(rec.SavedAt) < prevRetention {
		return
	}
	switch rec.Mode {
	case "binary":
		if rec.PrevPath != "" {
			_ = os.Remove(rec.PrevPath)
		}
		_ = os.Remove(filepath.Join(dataDir, prevMetaFile))
	case "container":
		if eng != nil && rec.PrevContainer != "" {
			if _, err := eng.ContainerRemove(ctx, rec.PrevContainer, client.ContainerRemoveOptions{Force: true}); err == nil || isNotFoundErr(err) {
				_ = os.Remove(filepath.Join(dataDir, prevMetaFile))
			}
		}
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
