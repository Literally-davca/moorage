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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"moorage/internal/dockerclone"
	"moorage/internal/engine"
	"moorage/internal/lint"
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
	// HostNetwork runs the helper with network_mode: host (Linux Docker; not Docker
	// Desktop or rootless Podman, where the host doesn't see container IPs).
	HostNetwork bool
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
	// M18H: the helper runs in the host's network, so it reaches the published ports
	// and the container IPs the way users and proxies do, and doesn't depend on a
	// network the new configuration may remove.
	hostNet := l.HostNetwork
	if hostNet {
		args = append(args, "--host-net")
	}
	var env []string
	if l.DockerHost != "" {
		env = append(env, "DOCKER_HOST="+l.DockerHost)
	}
	var mounts []mount.Mount
	for _, mp := range selfInsp.Mounts {
		mounts = append(mounts, dockerclone.MountPointToMount(mp))
	}
	netMode, netCfg := helperNetwork(selfInsp)
	if hostNet {
		netMode, netCfg = "host", nil
	}
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
	if err != nil && hostNet {
		// host networking refused (policy, rootless): the container's network and the
		// older check, as before
		l.HostNetwork = false
		return LaunchStackApplyHelper(ctx, eng, selfInsp, l)
	}
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
	HostNet                                                  bool // the helper runs in the host's network (M18H probe)
	// RecordRollback writes the restored files as a history version.
	RecordRollback func(reason string)

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
	fs.BoolVar(&o.HostNet, "host-net", false, "the helper runs in the host's network")
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
	// the log survives the helper container (AutoRemove): <data>/self-stack-apply.log
	var out io.Writer = os.Stdout
	logPath := filepath.Join(o.DataDir, engine.SelfApplyLogFile)
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		defer f.Close()
		out = io.MultiWriter(os.Stdout, &cappedWriter{w: f, max: 256 << 10})
	}
	fmt.Fprintf(out, "Apply & restart of %s/%s at %s\n", o.Project, o.Service, o.Now().Format(time.RFC3339))
	proxyNets := map[string]string{}
	if o.HostNet && o.IsHub {
		proxyNets = proxyNetworks(ctx, eng, o.Project, o.Service)
		for n, p := range proxyNets {
			fmt.Fprintf(out, "network %s is shared with proxy %s\n", n, p)
		}
	}

	startedAt := o.Now()
	reason := ""
	var lastChecked []string
	unverified := false
	if o.HostNet {
		if why := HostPortsTaken(ctx, eng, o.Project, o.Service, o.StackDir, out); why != "" {
			// M19B: nothing was started; the old container keeps running
			res := engine.SelfApplyResult{At: o.Now(), Message: "Not applied: " + why, Reason: why, Log: logPath}
			if engine.RestoreSelfBackup(o.StackDir, engine.SelfBackupDir(o.DataDir)) != nil {
				res.Message += " (the new files stay on disk; Moorage still runs the previous configuration)"
			}
			fmt.Fprintln(out, res.Message)
			writeStackApplyResult(o.DataDir, res)
			return fmt.Errorf("%s", res.Message)
		}
	}
	if err := up(ctx, out); err != nil {
		reason = "compose up failed: " + err.Error()
	} else if why, checked, unv := waitStackHealthy(ctx, eng, o, startedAt, proxyNets, out); why != "" {
		reason = why
	} else {
		lastChecked, unverified = checked, unv
	}
	if reason == "" {
		res := engine.SelfApplyResult{At: o.Now(), OK: true, Message: "Applied and Moorage came back.", Checked: lastChecked, Log: logPath, Unverified: unverified}
		if unverified {
			res.Message = "Applied – Moorage started, but it wasn't reachable from the helper. Check that you can open it."
		}
		writeStackApplyResult(o.DataDir, res)
		// M19A: this configuration is the one to return to next time
		if err := engine.CopySelfFiles(o.StackDir, engine.SelfGoodDir(o.DataDir)); err != nil {
			fmt.Fprintln(out, "can't keep the working configuration:", err)
		}
		_ = os.RemoveAll(engine.SelfBackupDir(o.DataDir))
		return nil
	}

	fmt.Fprintln(out, "rolling back:", reason)
	res := engine.SelfApplyResult{At: o.Now(), RolledBack: true, Message: "Rolled back: " + reason, Reason: reason, Log: logPath}
	bdir := engine.SelfBackupDir(o.DataDir)
	if _, err := os.Stat(bdir); err != nil || engine.SameSelfFiles(bdir, o.StackDir) {
		// M19A: never claim a rollback that changes nothing
		res.RolledBack = false
		res.Message = "Apply failed (" + reason + ") and there is no earlier working configuration to return to – see docs/self-stack.md"
		fmt.Fprintln(out, res.Message)
		writeStackApplyResult(o.DataDir, res)
		return fmt.Errorf("%s", res.Message)
	}
	if err := engine.RestoreSelfBackup(o.StackDir, bdir); err != nil {
		res.RolledBack = false
		res.Message = "Apply failed (" + reason + ") and the previous files couldn't be restored: " + err.Error()
	} else {
		if o.RecordRollback != nil {
			o.RecordRollback(reason)
		}
		if err := up(ctx, out); err != nil {
			res.Message += "; compose up with the previous files failed too: " + err.Error() + " – see docs/self-stack.md for the manual recovery"
		}
	}
	writeStackApplyResult(o.DataDir, res)
	return fmt.Errorf("%s", res.Message)
}

// waitStackHealthy waits for a running Moorage container of the project's service that
// stays up for StableFor and answers where users reach it. In the host's network (M18H)
// the hub must answer on every published TCP port, on its listen port with
// network_mode: host, and on its IP in every network it shared with a reverse proxy;
// ports: that network_mode: host ignores fail at once. Otherwise /api/session on the
// container or a fresh healthy mark in /data, as before.
func waitStackHealthy(ctx context.Context, eng ContainerEngine, o StackApplyOptions, startedAt time.Time, proxyNets map[string]string, out io.Writer) (string, []string, bool) {
	deadline := o.Now().Add(o.HealthTimeout)
	var stableSince time.Time
	var stableKey string
	last := "the Moorage container didn't start"
	var checked []string
	markOnly := false
	if o.HostNet && o.IsHub {
		if why := ignoredPorts(ctx, o); why != "" {
			return why, nil, false
		}
	}
	for o.Now().Before(deadline) {
		insp, why := currentServiceContainer(ctx, eng, o.Project, o.Service)
		if insp == nil {
			last, stableSince = why, time.Time{}
		} else {
			key := insp.ID + "/" + insp.State.StartedAt
			if key != stableKey {
				stableKey, stableSince = key, o.Now()
			}
			var healthy bool
			if o.HostNet && o.IsHub {
				var fail string
				checked, fail = probeEndpoints(ctx, o, *insp, proxyNets)
				healthy = fail == ""
				if !healthy {
					last = fail
				}
			} else {
				// Do not require matching the old o.Version: Apply & restart is commonly used to change the Moorage image tag.
				healthy = !o.IsHub || pollContainerSessionHealth(ctx, o.HTTPClient, *insp, o.ListenAddr, "")
				markOnly = false
				if !healthy && o.DataDir != "" {
					if mark, ok := readHealthyMark(o.DataDir); ok && !mark.At.Before(startedAt.Add(-time.Second)) {
						// M19D: alive by its own mark, but the helper couldn't reach it
						healthy, markOnly = true, o.IsHub
					}
				}
				if !healthy {
					last = "Moorage doesn't answer on its web port"
				}
			}
			if healthy && o.Now().Sub(stableSince) >= o.StableFor {
				fmt.Fprintf(out, "healthy: %s\n", strings.Join(checked, ", "))
				if markOnly {
					fmt.Fprintln(out, "unverified: only the health mark in the data folder answered")
				}
				return "", checked, markOnly
			} else if healthy {
				last = "the Moorage container keeps restarting"
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err().Error(), checked, false
		default:
		}
		o.Sleep(o.PollInterval)
	}
	fmt.Fprintf(out, "not healthy: %s\n", last)
	return last + " within " + o.HealthTimeout.String(), checked, false
}

// ignoredPorts reads the new compose file: ports: on the Moorage service with
// network_mode: host are dropped by Docker, so users on those ports lose the UI.
func ignoredPorts(ctx context.Context, o StackApplyOptions) string {
	file := engine.ComposeFileIn(o.StackDir)
	if file == "" {
		return ""
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	env, _ := os.ReadFile(filepath.Join(o.StackDir, ".env"))
	for _, is := range lint.Lint(ctx, string(b), string(env), lint.Options{Name: o.Project}) {
		if is.Code == "ports-ignored" && is.Service == o.Service {
			return "Moorage would not answer on its published ports (network_mode: host ignores ports:)"
		}
	}
	return ""
}

// probeEndpoints checks every address users and proxies reach Moorage at. It returns
// the addresses checked and the first failure ("" = all answered).
func probeEndpoints(ctx context.Context, o StackApplyOptions, insp container.InspectResponse, proxyNets map[string]string) ([]string, string) {
	_, listenPort, err := net.SplitHostPort(strings.TrimSpace(o.ListenAddr))
	if err != nil || listenPort == "" {
		listenPort = "5080"
	}
	type target struct{ url, fail string }
	var ts []target
	hostMode := insp.HostConfig != nil && insp.HostConfig.NetworkMode.IsHost()
	if hostMode {
		ts = append(ts, target{"http://127.0.0.1:" + listenPort + "/api/session", "Moorage doesn't answer on port " + listenPort + " of the host"})
	} else if insp.NetworkSettings != nil {
		for p, binds := range insp.NetworkSettings.Ports {
			if p.Proto() != "tcp" {
				continue
			}
			for _, b := range binds {
				ip := "127.0.0.1"
				if b.HostIP.IsValid() && !b.HostIP.IsUnspecified() {
					ip = b.HostIP.String()
				} else if b.HostIP.Is6() {
					continue // the same port is checked on 127.0.0.1
				}
				ts = append(ts, target{"http://" + net.JoinHostPort(ip, b.HostPort) + "/api/session", "Moorage doesn't answer on port " + b.HostPort})
			}
		}
	}
	for netName, proxy := range proxyNets {
		var ep *network.EndpointSettings
		if insp.NetworkSettings != nil {
			ep = insp.NetworkSettings.Networks[netName]
		}
		if ep == nil || !ep.IPAddress.IsValid() {
			return nil, "Moorage left network " + netName + " that " + proxy + " uses"
		}
		ts = append(ts, target{"http://" + net.JoinHostPort(ep.IPAddress.String(), listenPort) + "/api/session", "Moorage doesn't answer in network " + netName + " that " + proxy + " uses"})
	}
	if len(ts) == 0 {
		// only reachable from inside (no published ports, no proxy): the container check
		if pollContainerSessionHealth(ctx, o.HTTPClient, insp, o.ListenAddr, "") {
			return []string{"container"}, ""
		}
		return nil, "Moorage doesn't answer on its web port"
	}
	var checked []string
	for _, t := range ts {
		if !probeURL(ctx, o.HTTPClient, t.url) {
			return checked, t.fail
		}
		checked = append(checked, strings.TrimSuffix(strings.TrimPrefix(t.url, "http://"), "/api/session"))
	}
	return checked, ""
}

func probeURL(ctx context.Context, cli *http.Client, u string) bool {
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, u, nil)
	if err != nil {
		return false
	}
	resp, err := cli.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}

// proxyImageRe matches reverse proxies by image name.
var proxyImageRe = regexp.MustCompile(`(?i)(traefik|caddy|nginx-proxy|haproxy|envoy|swag|cloudflared|pangolin|zoraxy)`)

// proxyNetworks lists the networks the current Moorage container shares with a reverse
// proxy container (network -> proxy name).
func proxyNetworks(ctx context.Context, eng ContainerEngine, project, service string) map[string]string {
	out := map[string]string{}
	insp, _ := currentServiceContainer(ctx, eng, project, service)
	if insp == nil || insp.NetworkSettings == nil {
		return out
	}
	list, err := eng.ContainerList(ctx, client.ContainerListOptions{})
	if err != nil {
		return out
	}
	for _, c := range list.Items {
		if c.ID == insp.ID || c.NetworkSettings == nil || !proxyImageRe.MatchString(c.Image) {
			continue
		}
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		for n := range c.NetworkSettings.Networks {
			if _, shared := insp.NetworkSettings.Networks[n]; shared && n != "host" && n != "bridge" {
				out[n] = name
			}
		}
	}
	return out
}

// cappedWriter keeps the log under max bytes.
type cappedWriter struct {
	w   io.Writer
	n   int
	max int
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.n >= c.max {
		return len(p), nil
	}
	q := p
	if c.n+len(q) > c.max {
		q = q[:c.max-c.n]
	}
	n, err := c.w.Write(q)
	c.n += n
	if err != nil {
		return n, err
	}
	return len(p), nil
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

// HostPortsTaken is the helper's last check before compose up (M19B): with
// network_mode: host in the new configuration, the listen ports of Moorage must be
// free in the host's network. It reads /proc/net in the host's namespace, so it also
// sees native processes (tailscaled, another server) that Moorage in a bridge network
// can't. Ports the current Moorage container holds are ignored: they free up when it
// is recreated. "" means go ahead.
func HostPortsTaken(ctx context.Context, eng ContainerEngine, project, service, stackDir string, out io.Writer) string {
	file := engine.ComposeFileIn(stackDir)
	if file == "" {
		return ""
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	env, _ := os.ReadFile(filepath.Join(stackDir, ".env"))
	native := engine.ReadNativePorts()
	if native == nil {
		return "" // not Linux, or /proc unreadable: the API check is all we have
	}
	var running []lint.PortUse
	for _, p := range native {
		running = append(running, lint.PortUse{IP: p.IP, Port: p.Port, Proto: p.Proto, Owner: "a process on the host"})
	}
	var own []int
	if insp, _ := currentServiceContainer(ctx, eng, project, service); insp != nil {
		own = containerOwnPorts(insp)
	}
	for _, is := range lint.Lint(ctx, string(b), string(env), lint.Options{Name: project, Running: running, Self: &lint.SelfLint{Service: service, OwnPorts: own}}) {
		if is.Code == "self-port-taken" || is.Code == "self-listen-invalid" {
			fmt.Fprintln(out, "refused before compose up:", is.Message)
			return is.Message
		}
	}
	return ""
}

// containerOwnPorts are the host ports a Moorage container holds: its published ports,
// and its listen ports when it runs in the host's network.
func containerOwnPorts(insp *container.InspectResponse) []int {
	var own []int
	if insp.NetworkSettings != nil {
		for _, binds := range insp.NetworkSettings.Ports {
			for _, b := range binds {
				if n, err := strconv.Atoi(b.HostPort); err == nil {
					own = append(own, n)
				}
			}
		}
	}
	if insp.HostConfig != nil && insp.HostConfig.NetworkMode.IsHost() && insp.Config != nil {
		for _, kv := range insp.Config.Env {
			k, v, _ := strings.Cut(kv, "=")
			if k == "MOORAGE_LISTEN" || k == "MOORAGE_AGENT_LISTEN" || k == "MOORAGE_WATCHDOG_LISTEN" {
				if _, p, _, ok, _ := lint.ParseListen(v); ok && p > 0 {
					own = append(own, p)
				}
			}
		}
		own = append(own, 5080, 5081) // defaults when not set
	}
	return own
}
