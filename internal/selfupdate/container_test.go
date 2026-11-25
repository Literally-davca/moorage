package selfupdate

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"moorage/internal/engine"
)

type fakeCtr struct {
	insp      container.InspectResponse
	failStart error
}

type fakeContainerEngine struct {
	mu         sync.Mutex
	seq        int
	ctrs       map[string]*fakeCtr
	images     map[string]client.ImageInspectResult
	created    []client.ContainerCreateOptions
	connected  []string
	removed    []string
	stopped    []string
	pulledRefs []string
	pullErr    error
	// helperExits makes started updater helpers exit at once (engine unreachable).
	helperExits bool
}

func newFakeContainerEngine() *fakeContainerEngine {
	return &fakeContainerEngine{
		ctrs:   map[string]*fakeCtr{},
		images: map[string]client.ImageInspectResult{},
	}
}

func (f *fakeContainerEngine) addContainer(c container.InspectResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctrs[c.ID] = &fakeCtr{insp: c}
}

func (f *fakeContainerEngine) byIDOrName(idOrName string) (*fakeCtr, error) {
	if c, ok := f.ctrs[idOrName]; ok {
		return c, nil
	}
	clean := strings.TrimPrefix(idOrName, "/")
	for _, c := range f.ctrs {
		if strings.TrimPrefix(c.insp.Name, "/") == clean || strings.HasPrefix(c.insp.ID, clean) {
			return c, nil
		}
	}
	return nil, errors.New("Error response from daemon: No such container: " + idOrName)
}

func (f *fakeContainerEngine) ContainerList(_ context.Context, _ client.ContainerListOptions) (client.ContainerListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var items []container.Summary
	for _, c := range f.ctrs {
		var labels map[string]string
		var img string
		if c.insp.Config != nil {
			labels = c.insp.Config.Labels
			img = c.insp.Config.Image
		}
		var state container.ContainerState
		if c.insp.State != nil {
			state = c.insp.State.Status
		}
		items = append(items, container.Summary{
			ID:     c.insp.ID,
			Names:  []string{c.insp.Name},
			Image:  img,
			Labels: labels,
			State:  state,
		})
	}
	return client.ContainerListResult{Items: items}, nil
}

func (f *fakeContainerEngine) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.byIDOrName(id)
	if err != nil {
		return client.ContainerInspectResult{}, err
	}
	return client.ContainerInspectResult{Container: c.insp}, nil
}

func (f *fakeContainerEngine) ContainerCreate(_ context.Context, opt client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := "/" + strings.TrimPrefix(opt.Name, "/")
	for _, c := range f.ctrs {
		if c.insp.Name == name {
			return client.ContainerCreateResult{}, errors.New("conflict: name already in use")
		}
	}
	f.seq++
	id := strings.Repeat("a", 50) + strings.ReplaceAll(time.Unix(int64(f.seq), 0).Format("150405"), ":", "") + "12345678"
	if len(id) > 64 {
		id = id[:64]
	}
	var nets map[string]*network.EndpointSettings
	if opt.NetworkingConfig != nil {
		nets = map[string]*network.EndpointSettings{}
		for k, v := range opt.NetworkingConfig.EndpointsConfig {
			cp := *v
			nets[k] = &cp
		}
	}
	f.created = append(f.created, opt)
	f.ctrs[id] = &fakeCtr{
		insp: container.InspectResponse{
			ID:              id,
			Name:            name,
			Config:          opt.Config,
			HostConfig:      opt.HostConfig,
			State:           &container.State{Status: "created", Running: false},
			NetworkSettings: &container.NetworkSettings{Networks: nets},
		},
	}
	return client.ContainerCreateResult{ID: id}, nil
}

func (f *fakeContainerEngine) ContainerStop(_ context.Context, id string, _ client.ContainerStopOptions) (client.ContainerStopResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.byIDOrName(id)
	if err != nil {
		return client.ContainerStopResult{}, err
	}
	f.stopped = append(f.stopped, c.insp.ID)
	c.insp.State = &container.State{Status: "exited", Running: false}
	return client.ContainerStopResult{}, nil
}

func (f *fakeContainerEngine) ContainerRename(_ context.Context, id string, opt client.ContainerRenameOptions) (client.ContainerRenameResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.byIDOrName(id)
	if err != nil {
		return client.ContainerRenameResult{}, err
	}
	c.insp.Name = "/" + strings.TrimPrefix(opt.NewName, "/")
	return client.ContainerRenameResult{}, nil
}

func (f *fakeContainerEngine) ContainerStart(_ context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.byIDOrName(id)
	if err != nil {
		return client.ContainerStartResult{}, err
	}
	if c.failStart != nil {
		return client.ContainerStartResult{}, c.failStart
	}
	if f.helperExits && c.insp.Config != nil && c.insp.Config.Labels[engine.LabelRole] == engine.RoleUpdater {
		c.insp.State = &container.State{Status: "exited", ExitCode: 1}
		return client.ContainerStartResult{}, nil
	}
	c.insp.State = &container.State{
		Status:  "running",
		Running: true,
		Health:  &container.Health{Status: container.Healthy},
	}
	return client.ContainerStartResult{}, nil
}

func (f *fakeContainerEngine) ContainerRemove(_ context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.byIDOrName(id)
	if err != nil {
		return client.ContainerRemoveResult{}, err
	}
	f.removed = append(f.removed, c.insp.Name)
	delete(f.ctrs, c.insp.ID)
	return client.ContainerRemoveResult{}, nil
}

func (f *fakeContainerEngine) ContainerUpdate(_ context.Context, id string, opt client.ContainerUpdateOptions) (client.ContainerUpdateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.byIDOrName(id)
	if err != nil {
		return client.ContainerUpdateResult{}, err
	}
	if opt.RestartPolicy != nil {
		if c.insp.HostConfig == nil {
			c.insp.HostConfig = &container.HostConfig{}
		}
		hc := *c.insp.HostConfig
		hc.RestartPolicy = *opt.RestartPolicy
		c.insp.HostConfig = &hc
	}
	return client.ContainerUpdateResult{}, nil
}

func (f *fakeContainerEngine) NetworkConnect(_ context.Context, netID string, opt client.NetworkConnectOptions) (client.NetworkConnectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.byIDOrName(opt.Container)
	if err != nil {
		return client.NetworkConnectResult{}, err
	}
	if c.insp.NetworkSettings == nil {
		c.insp.NetworkSettings = &container.NetworkSettings{}
	}
	if c.insp.NetworkSettings.Networks == nil {
		c.insp.NetworkSettings.Networks = map[string]*network.EndpointSettings{}
	}
	c.insp.NetworkSettings.Networks[netID] = opt.EndpointConfig
	f.connected = append(f.connected, netID)
	return client.NetworkConnectResult{}, nil
}

func (f *fakeContainerEngine) ImageInspect(_ context.Context, ref string, _ ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.images[ref]
	if !ok {
		return client.ImageInspectResult{}, errors.New("no such image: " + ref)
	}
	return r, nil
}

func (f *fakeContainerEngine) ImagePull(_ context.Context, ref string, _ client.ImagePullOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulledRefs = append(f.pulledRefs, ref)
	return f.pullErr
}

func sampleTargetInspect(id string) container.InspectResponse {
	stopTimeout := 30
	return container.InspectResponse{
		ID:   id,
		Name: "/moorage-agent",
		Config: &container.Config{
			Hostname:    id[:12],
			User:        "1000:1000",
			Env:         []string{"MOORAGE_HUB=hub:5081", "MOORAGE_SELF_UPDATE=true"},
			Cmd:         []string{"agent"},
			Entrypoint:  []string{"/moorage"},
			WorkingDir:  "/opt",
			Image:       "ghcr.io/literally-davca/moorage:0.0.1-test",
			StopSignal:  "SIGTERM",
			StopTimeout: &stopTimeout,
			Healthcheck: &container.HealthConfig{Test: []string{"CMD", "/moorage", "version"}},
			Labels: map[string]string{
				"com.docker.compose.project": "moorage",
				"com.docker.compose.service": "agent",
				"custom.label":               "keep-me",
			},
		},
		HostConfig: &container.HostConfig{
			Binds:           []string{"/var/run/docker.sock:/var/run/docker.sock", "/opt/stacks:/opt/stacks"},
			NetworkMode:     "front",
			RestartPolicy:   container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			CapAdd:          []string{"NET_RAW"},
			CapDrop:         []string{"MKNOD"},
			SecurityOpt:     []string{"no-new-privileges:true"},
			ReadonlyRootfs:  false,
			ExtraHosts:      []string{"host.docker.internal:host-gateway"},
			DNS:             []netip.Addr{netip.MustParseAddr("1.1.1.1")},
			Tmpfs:           map[string]string{"/tmp": "rw,noexec"},
			Sysctls:         map[string]string{"net.ipv4.ping_group_range": "0 2147483647"},
			LogConfig:       container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "10m"}},
			PortBindings:    network.PortMap{network.MustParsePort("5082/tcp"): {{HostPort: "5082"}}},
			PublishAllPorts: false,
		},
		Mounts: []container.MountPoint{
			{Type: mount.TypeBind, Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock", RW: true},
			{Type: mount.TypeVolume, Name: "moorage-data", Destination: "/data", RW: true},
			{Type: mount.TypeVolume, Name: "anon-vol-1", Destination: "/cache", RW: true},
		},
		NetworkSettings: &container.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{
				"front": {Aliases: []string{id[:12], "moorage-agent"}, IPAddress: netip.MustParseAddr("172.18.0.5")},
				"back":  {Aliases: []string{"agent-internal"}, DriverOpts: map[string]string{"com.docker.network.driver.mtu": "1400"}},
			},
		},
		State: &container.State{Status: "running", Running: true},
	}
}

func TestContainerCloneHelperRollbackAndModeDetect(t *testing.T) {
	targetID := "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	eng := newFakeContainerEngine()
	eng.addContainer(sampleTargetInspect(targetID))

	dataDir := t.TempDir()

	// 1. Test DetectStatus identifies container mode, compose label, socket, and unsupported hub-only nil engine.
	re := RuntimeEnv{
		GOOS:   "linux",
		GOARCH: "amd64",
		Stat: func(p string) (os.FileInfo, error) {
			if p == "/.dockerenv" {
				return os.Stat(dataDir)
			}
			return nil, os.ErrNotExist
		},
		ReadFile: func(p string) ([]byte, error) {
			if p == "/proc/self/mountinfo" {
				return []byte("1 1 0:1 / /var/lib/docker/containers/" + targetID + "/resolv.conf rw\n"), nil
			}
			return nil, os.ErrNotExist
		},
		Hostname:       func() (string, error) { return targetID[:12], nil },
		Getenv:         func(string) string { return "" },
		SelfUpdateFlag: true,
	}
	stNoEng := DetectStatus(context.Background(), nil, dataDir, "0.0.1-test", true, &re)
	if stNoEng.Mode != "container" || stNoEng.Supported {
		t.Fatalf("expected unsupported container mode when engine is nil, got %+v", stNoEng)
	}
	st := DetectStatus(context.Background(), eng, dataDir, "0.0.1-test", true, &re)
	if st.Mode != "container" || !st.Supported || st.ComposeProject != "moorage" || st.ContainerName != "moorage-agent" {
		t.Fatalf("unexpected container DetectStatus: %+v", st)
	}

	// 2. Test PrepareAndLaunchHelper verifies digest, pulls image, creates helper with AutoRemove & updater label.
	fx := makeSignedFixture(t, "0.0.2-test", "moorage-linux-amd64", "linux/amd64", []byte("unused"), "", "")
	ca := &ContainerApplier{
		Eng:            eng,
		DataDir:        dataDir,
		CurrentVersion: "0.0.1-test",
		PubKey:         fx.pub,
		GOOS:           "linux",
		GOARCH:         "amd64",
		Runtime:        &re,

		HelperCheckDelay: time.Millisecond,
	}
	if err := ca.PrepareAndLaunchHelper(context.Background(), fx.manifest); err != nil {
		t.Fatalf("PrepareAndLaunchHelper failed: %v", err)
	}
	if len(eng.pulledRefs) != 1 || eng.pulledRefs[0] != fx.imageRepo+"@"+fx.digest {
		t.Fatalf("expected pull of %s@%s, got %v", fx.imageRepo, fx.digest, eng.pulledRefs)
	}
	if len(eng.created) != 1 {
		t.Fatalf("expected 1 helper created, got %d", len(eng.created))
	}
	helperCreate := eng.created[0]
	if !helperCreate.HostConfig.AutoRemove || helperCreate.Config.Labels[engine.LabelRole] != engine.RoleUpdater {
		t.Fatalf("helper container missing AutoRemove or moorage.role=updater: %+v %+v", helperCreate.Config.Labels, helperCreate.HostConfig)
	}
	if helperCreate.Config.Labels["com.docker.compose.project"] != "" {
		t.Fatal("helper container must NOT carry compose labels")
	}

	// 3. Test RunHelper upgrade copies all fields, compose labels, extra network, anon volume, and keeps -moorage-prev.
	now := time.Unix(1_700_000_000, 0)
	_ = writeJSONFile(filepath.Join(dataDir, healthyMarkFile), HealthyMark{Version: "0.0.2-test", At: now.Add(time.Second)}, 0o600)

	err := RunHelper(context.Background(), eng, HelperOptions{
		TargetID:      targetID,
		NewImage:      fx.imageRepo + "@" + fx.digest,
		FromVer:       "0.0.1-test",
		ToVer:         "0.0.2-test",
		DataDir:       dataDir,
		HealthTimeout: 5 * time.Second,
		PollInterval:  10 * time.Millisecond,
		Now:           func() time.Time { return now },
		Sleep:         func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("RunHelper upgrade failed: %v", err)
	}
	prevRes, err := eng.ContainerInspect(context.Background(), "moorage-agent-moorage-prev", client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("expected moorage-agent-moorage-prev kept, got %v", err)
	}
	if prevRes.Container.State.Running {
		t.Fatal("expected prev container to be stopped")
	}
	newRes, err := eng.ContainerInspect(context.Background(), "moorage-agent", client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("expected new moorage-agent container, got %v", err)
	}
	nc := newRes.Container
	if nc.Config.Image != fx.imageRepo+"@"+fx.digest ||
		nc.Config.Labels["com.docker.compose.project"] != "moorage" ||
		nc.Config.Labels["custom.label"] != "keep-me" ||
		nc.Config.User != "1000:1000" ||
		nc.HostConfig.RestartPolicy.Name != container.RestartPolicyUnlessStopped ||
		len(nc.HostConfig.CapAdd) != 1 || nc.HostConfig.CapAdd[0] != "NET_RAW" ||
		nc.Config.Hostname != "" ||
		len(nc.NetworkSettings.Networks) != 2 {
		t.Fatalf("cloned container config mismatch: %+v %+v %+v", nc.Config, nc.HostConfig, nc.NetworkSettings)
	}
	if !containsString(nc.HostConfig.Binds, "anon-vol-1:/cache") {
		t.Fatalf("expected anonymous volume preserved in Binds, got %v", nc.HostConfig.Binds)
	}
	if containsString(nc.NetworkSettings.Networks["front"].Aliases, targetID[:12]) {
		t.Fatalf("old short container ID alias should be stripped, got %v", nc.NetworkSettings.Networks["front"].Aliases)
	}

	// Verify IsInternalContainer ignores both helper updater and -moorage-prev!
	list, _ := eng.ContainerList(context.Background(), client.ContainerListOptions{All: true})
	var visibleNames []string
	for _, s := range list.Items {
		if !engine.IsInternalContainer(s) {
			visibleNames = append(visibleNames, s.Names[0])
		}
	}
	if len(visibleNames) != 1 || visibleNames[0] != "/moorage-agent" {
		t.Fatalf("expected only /moorage-agent visible outside updater/-moorage-prev, got %v", visibleNames)
	}

	// 4. Test auto-rollback on health timeout!
	nowTimeout := time.Unix(1_800_000_000, 0)
	_ = writeJSONFile(filepath.Join(dataDir, healthyMarkFile), HealthyMark{Version: "0.0.2-test", At: nowTimeout.Add(-time.Hour)}, 0o600)
	var tick int
	err = RunHelper(context.Background(), eng, HelperOptions{
		TargetID:      nc.ID,
		NewImage:      fx.imageRepo + "@sha256:badnew",
		FromVer:       "0.0.2-test",
		ToVer:         "0.0.3-test",
		DataDir:       dataDir,
		HealthTimeout: 2 * time.Second,
		PollInterval:  10 * time.Millisecond,
		Now: func() time.Time {
			tick++
			return nowTimeout.Add(time.Duration(tick) * time.Second)
		},
		Sleep: func(time.Duration) {},
	})
	if err == nil || !strings.Contains(err.Error(), "rolled back:") {
		t.Fatalf("expected rolled back error on health timeout, got %v", err)
	}
	restoredRes, err := eng.ContainerInspect(context.Background(), "moorage-agent", client.ContainerInspectOptions{})
	if err != nil || !restoredRes.Container.State.Running || restoredRes.Container.ID != nc.ID {
		t.Fatalf("expected previous container renamed back and started, got %+v err=%v", restoredRes.Container, err)
	}

	// 5. Test manual rollback (--rollback) and 24 h cleanup!
	eng2 := newFakeContainerEngine()
	eng2.addContainer(sampleTargetInspect(targetID))
	_ = writeJSONFile(filepath.Join(dataDir, healthyMarkFile), HealthyMark{Version: "0.0.2-test", At: now}, 0o600)
	if err := RunHelper(context.Background(), eng2, HelperOptions{
		TargetID:      targetID,
		NewImage:      fx.imageRepo + "@" + fx.digest,
		FromVer:       "0.0.1-test",
		ToVer:         "0.0.2-test",
		DataDir:       dataDir,
		HealthTimeout: 5 * time.Second,
		PollInterval:  10 * time.Millisecond,
		Now:           func() time.Time { return now },
		Sleep:         func(time.Duration) {},
	}); err != nil {
		t.Fatal(err)
	}

	// 24h cleanup removes moorage-agent-moorage-prev cleanly
	CleanupExpiredPrev(context.Background(), eng2, dataDir, now.Add(25*time.Hour))
	if _, err := eng2.ContainerInspect(context.Background(), "moorage-agent-moorage-prev", client.ContainerInspectOptions{}); err == nil {
		t.Fatal("expected -moorage-prev container removed after 24h")
	}

	// Also test LaunchRollbackHelper -> RunHelper(--rollback) restores old container ID.
	eng3 := newFakeContainerEngine()
	eng3.addContainer(sampleTargetInspect(targetID))
	_ = writeJSONFile(filepath.Join(dataDir, healthyMarkFile), HealthyMark{Version: "0.0.2-test", At: now}, 0o600)
	if err := RunHelper(context.Background(), eng3, HelperOptions{
		TargetID:      targetID,
		NewImage:      fx.imageRepo + "@" + fx.digest,
		FromVer:       "0.0.1-test",
		ToVer:         "0.0.2-test",
		DataDir:       dataDir,
		HealthTimeout: 5 * time.Second,
		PollInterval:  10 * time.Millisecond,
		Now:           func() time.Time { return now },
		Sleep:         func(time.Duration) {},
	}); err != nil {
		t.Fatal(err)
	}
	curNow, _ := eng3.ContainerInspect(context.Background(), "moorage-agent", client.ContainerInspectOptions{})
	if err := RunHelper(context.Background(), eng3, HelperOptions{
		TargetID:      curNow.Container.ID,
		Rollback:      true,
		DataDir:       dataDir,
		HealthTimeout: 5 * time.Second,
		PollInterval:  10 * time.Millisecond,
		Now:           func() time.Time { return now },
		Sleep:         func(time.Duration) {},
	}); err != nil {
		t.Fatalf("manual container rollback failed: %v", err)
	}
	afterRb, _ := eng3.ContainerInspect(context.Background(), "moorage-agent", client.ContainerInspectOptions{})
	if afterRb.Container.ID != targetID || !afterRb.Container.State.Running {
		t.Fatalf("expected original container ID %s running after manual rollback, got %s running=%v",
			targetID, afterRb.Container.ID, afterRb.Container.State.Running)
	}

	// 6. Hub upgrade confirms via healthy mark when HTTP port isn't reachable from helper network.
	engHub := newFakeContainerEngine()
	engHub.addContainer(sampleTargetInspect(targetID))
	hubData := t.TempDir()
	nowHub := time.Unix(1_800_001_000, 0)
	_ = writeJSONFile(filepath.Join(hubData, healthyMarkFile), HealthyMark{Version: "0.0.2-test", At: nowHub.Add(time.Second)}, 0o600)
	if err := RunHelper(context.Background(), engHub, HelperOptions{
		TargetID:      targetID,
		NewImage:      fx.imageRepo + "@" + fx.digest,
		FromVer:       "0.0.1-test",
		ToVer:         "0.0.2-test",
		DataDir:       hubData,
		IsHub:         true,
		ListenAddr:    ":59999",
		HealthTimeout: 3 * time.Second,
		PollInterval:  10 * time.Millisecond,
		Now:           func() time.Time { return nowHub },
		Sleep:         func(time.Duration) {},
	}); err != nil {
		t.Fatalf("hub helper should confirm via healthy mark fallback: %v", err)
	}
}
