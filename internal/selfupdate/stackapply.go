package selfupdate

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"moorage/internal/engine"
)

// Apply & restart of the stack Moorage itself runs from (M13). Compose recreates the
// Moorage container, so `compose up` can't run inside it: a one-shot helper container
// from the same image runs it from outside, waits for Moorage to come back and, if it
// doesn't, puts the previous compose/.env back and runs `compose up` again.

// DetectSelfStack finds the compose stack of the running Moorage container. nil means
// none: a binary install, or a container not started by compose.
func DetectSelfStack(ctx context.Context, eng ContainerEngine, dataDir string) (*engine.SelfStack, *container.InspectResponse) {
	re := defaultRuntimeEnv(true)
	if !inContainerEnv(re) {
		return nil, nil
	}
	insp, _ := findSelfContainer(ctx, eng, re)
	if insp == nil || insp.Config == nil {
		return nil, nil
	}
	project := insp.Config.Labels["com.docker.compose.project"]
	service := insp.Config.Labels["com.docker.compose.service"]
	if project == "" || service == "" {
		return nil, nil
	}
	s := &engine.SelfStack{Project: project, Service: service}
	if m, ok := socketMountSpec(insp); ok {
		s.SocketTarget = m.Target
	}
	s.DataTarget = longestMountTarget(insp, dataDir)
	return s, insp
}

// longestMountTarget is the container path of the mount holding dir.
func longestMountTarget(insp *container.InspectResponse, dir string) string {
	best := ""
	clean := filepath.Clean(dir)
	for _, m := range insp.Mounts {
		d := filepath.Clean(m.Destination)
		if (clean == d || strings.HasPrefix(clean, d+"/")) && len(d) > len(best) {
			best = d
		}
	}
	return best
}

// StackApplyLaunch says how to start the helper for one apply.
type StackApplyLaunch struct {
	Self       *engine.SelfStack
	StacksDir  string
	DataDir    string
	IsHub      bool
	ListenAddr string
	Version    string
	DockerHost string
}

const roleStackApply = "stack-apply"

// LaunchStackApplyHelper starts the helper container with the Moorage container's own
// mounts (stacks folder, data, socket at the same paths), network, user and security
// options, from the image the Moorage container runs now.
func LaunchStackApplyHelper(ctx context.Context, eng ContainerEngine, selfInsp *container.InspectResponse, l StackApplyLaunch) error {
	name := strings.TrimPrefix(selfInsp.Name, "/") + "-moorage-apply"
	if err := refuseRunningApplyHelper(ctx, eng, name); err != nil {
		return err
	}
	args := []string{
		"self-stack-helper",
		"--project", l.Self.Project,
		"--service", l.Self.Service,
		"--stack-dir", filepath.Join(l.StacksDir, l.Self.Project),
		"--data-dir", l.DataDir,
		"--version", l.Version,
	}
	if l.IsHub {
		args = append(args, "--hub", "--listen", l.ListenAddr)
	}
	var env []string
	if l.DockerHost != "" {
		env = append(env, "DOCKER_HOST="+l.DockerHost)
	}
	var mounts []mount.Mount
	for _, mp := range selfInsp.Mounts {
		mounts = append(mounts, mountPointToMount(mp))
	}
	netMode, netCfg := helperNetwork(selfInsp)
	cfg := &container.Config{
		Image:      selfInsp.Image, // the image ID the Moorage container runs, not a tag that may have moved
		Entrypoint: []string{"/moorage"},
		Cmd:        args,
		Env:        env,
		Labels: map[string]string{
			engine.LabelRole:  engine.RoleUpdater,
			labelUpdateTarget: strings.TrimPrefix(selfInsp.Name, "/"),
			"moorage.task":    roleStackApply,
		},
	}
	hc := &container.HostConfig{AutoRemove: true, Mounts: mounts, NetworkMode: netMode}
	if selfInsp.Config != nil {
		cfg.User = selfInsp.Config.User
	}
	if selfInsp.HostConfig != nil {
		hc.GroupAdd = append([]string(nil), selfInsp.HostConfig.GroupAdd...)
		hc.SecurityOpt = append([]string(nil), selfInsp.HostConfig.SecurityOpt...)
	}
	created, err := eng.ContainerCreate(ctx, client.ContainerCreateOptions{Name: name, Config: cfg, HostConfig: hc, NetworkingConfig: netCfg})
	if err != nil {
		return fmt.Errorf("create apply helper container: %w", err)
	}
	if _, err := eng.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		_, _ = eng.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true})
		return fmt.Errorf("start apply helper container: %w", err)
	}
	return nil
}

// refuseRunningApplyHelper refuses a second apply while one runs; an exited leftover
// (AutoRemove failed) is removed.
func refuseRunningApplyHelper(ctx context.Context, eng ContainerEngine, name string) error {
	res, err := eng.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		return nil // not found
	}
	if res.Container.State != nil && (res.Container.State.Running || res.Container.State.Status == container.StateCreated) {
		return engine.ErrBusy
	}
	_, _ = eng.ContainerRemove(ctx, res.Container.ID, client.ContainerRemoveOptions{Force: true})
	return nil
}

// StackApplyOptions configure `moorage self-stack-helper`.
type StackApplyOptions struct {
	Project, Service, StackDir, DataDir, Version, ListenAddr string
	IsHub                                                    bool

	StartDelay    time.Duration // let the node answer the HTTP request first
	HealthTimeout time.Duration // 120 s
	StableFor     time.Duration // the new container must keep running this long
	PollInterval  time.Duration
	HTTPClient    *http.Client
	Now           func() time.Time
	Sleep         func(time.Duration)
}

// ParseStackApplyFlags parses the helper's flags.
func ParseStackApplyFlags(args []string) (StackApplyOptions, error) {
	var o StackApplyOptions
	fs := flag.NewFlagSet("self-stack-helper", flag.ContinueOnError)
	fs.StringVar(&o.Project, "project", "", "compose project")
	fs.StringVar(&o.Service, "service", "", "compose service of Moorage")
	fs.StringVar(&o.StackDir, "stack-dir", "", "stack folder")
	fs.StringVar(&o.DataDir, "data-dir", "", "data folder")
	fs.StringVar(&o.Version, "version", "", "Moorage version")
	fs.BoolVar(&o.IsHub, "hub", false, "the target serves the web UI")
	fs.StringVar(&o.ListenAddr, "listen", ":5080", "hub listen address")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if o.Project == "" || o.Service == "" || o.StackDir == "" || o.DataDir == "" {
		return o, fmt.Errorf("--project, --service, --stack-dir and --data-dir are required")
	}
	return o, nil
}

// RunStackApplyHelper runs compose up (via up), waits for Moorage and rolls back.
func RunStackApplyHelper(ctx context.Context, eng ContainerEngine, up func(ctx context.Context, out io.Writer) error, o StackApplyOptions) error {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	if o.HealthTimeout == 0 {
		o.HealthTimeout = 120 * time.Second
	}
	if o.StableFor == 0 {
		o.StableFor = 15 * time.Second
	}
	if o.PollInterval == 0 {
		o.PollInterval = 1500 * time.Millisecond
	}
	if o.StartDelay == 0 {
		o.StartDelay = 2 * time.Second
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 3 * time.Second}
	}
	o.Sleep(o.StartDelay)
	out := os.Stdout

	reason := ""
	if err := up(ctx, out); err != nil {
		reason = "compose up failed: " + err.Error()
	} else if why := waitStackHealthy(ctx, eng, o); why != "" {
		reason = why
	}
	if reason == "" {
		writeStackApplyResult(o.DataDir, engine.SelfApplyResult{At: o.Now(), OK: true, Message: "Applied and Moorage came back."})
		_ = os.RemoveAll(engine.SelfBackupDir(o.DataDir))
		return nil
	}

	fmt.Fprintln(out, "rolling back:", reason)
	res := engine.SelfApplyResult{At: o.Now(), RolledBack: true, Message: "Rolled back: " + reason}
	if err := engine.RestoreSelfBackup(o.StackDir, engine.SelfBackupDir(o.DataDir)); err != nil {
		res.RolledBack = false
		res.Message = "Apply failed (" + reason + ") and the previous files couldn't be restored: " + err.Error()
	} else if err := up(ctx, out); err != nil {
		res.Message += "; compose up with the previous files failed too: " + err.Error()
	}
	writeStackApplyResult(o.DataDir, res)
	return fmt.Errorf("%s", res.Message)
}

// waitStackHealthy waits for a running Moorage container of the project's service that
// stays up for StableFor (and answers /api/session when it is the hub).
func waitStackHealthy(ctx context.Context, eng ContainerEngine, o StackApplyOptions) string {
	deadline := o.Now().Add(o.HealthTimeout)
	var stableSince time.Time
	var stableKey string
	last := "the Moorage container didn't start"
	for o.Now().Before(deadline) {
		insp, why := currentServiceContainer(ctx, eng, o.Project, o.Service)
		if insp == nil {
			last, stableSince = why, time.Time{}
		} else {
			key := insp.ID + "/" + insp.State.StartedAt
			if key != stableKey {
				stableKey, stableSince = key, o.Now()
			}
			healthy := !o.IsHub || pollContainerSessionHealth(ctx, o.HTTPClient, *insp, o.ListenAddr, o.Version)
			if !healthy {
				last = "Moorage doesn't answer on its web port"
			} else if o.Now().Sub(stableSince) >= o.StableFor {
				return ""
			} else {
				last = "the Moorage container keeps restarting"
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err().Error()
		default:
		}
		o.Sleep(o.PollInterval)
	}
	return last + " within " + o.HealthTimeout.String()
}

func currentServiceContainer(ctx context.Context, eng ContainerEngine, project, service string) (*container.InspectResponse, string) {
	list, err := eng.ContainerList(ctx, client.ContainerListOptions{
		All: true,
		Filters: make(client.Filters).
			Add("label", "com.docker.compose.project="+project).
			Add("label", "com.docker.compose.service="+service),
	})
	if err != nil {
		return nil, "can't list containers: " + err.Error()
	}
	for _, c := range list.Items {
		// Check the labels too: the filter is a hint some compat APIs ignore.
		if c.State != container.StateRunning || c.Labels["com.docker.compose.project"] != project || c.Labels["com.docker.compose.service"] != service {
			continue
		}
		res, err := eng.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if err == nil && res.Container.State != nil && res.Container.State.Running && !res.Container.State.Restarting {
			return &res.Container, ""
		}
	}
	return nil, "the Moorage container isn't running"
}

func writeStackApplyResult(dataDir string, r engine.SelfApplyResult) {
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dataDir, engine.SelfStackResultFile), b, 0o644)
}
