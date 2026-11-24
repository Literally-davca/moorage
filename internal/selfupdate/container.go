package selfupdate

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"moorage/internal/dockerclone"
	"moorage/internal/engine"
	"moorage/internal/host"
)

// ContainerEngine is the minimal moby client subset needed for self-update in container mode.
type ContainerEngine interface {
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStop(ctx context.Context, containerID string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRename(ctx context.Context, containerID string, options client.ContainerRenameOptions) (client.ContainerRenameResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	ContainerUpdate(ctx context.Context, containerID string, options client.ContainerUpdateOptions) (client.ContainerUpdateResult, error)
	NetworkConnect(ctx context.Context, networkID string, options client.NetworkConnectOptions) (client.NetworkConnectResult, error)
	ImageInspect(ctx context.Context, imageID string, inspectOpts ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ImagePull(ctx context.Context, refStr string, options client.ImagePullOptions) error
}

// MobyEngine adapts a live moby client.APIClient to ContainerEngine.
type MobyEngine struct {
	API client.APIClient
}

func (m MobyEngine) ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
	return m.API.ContainerList(ctx, options)
}
func (m MobyEngine) ContainerInspect(ctx context.Context, id string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return m.API.ContainerInspect(ctx, id, options)
}
func (m MobyEngine) ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	return m.API.ContainerCreate(ctx, options)
}
func (m MobyEngine) ContainerStop(ctx context.Context, id string, options client.ContainerStopOptions) (client.ContainerStopResult, error) {
	return m.API.ContainerStop(ctx, id, options)
}
func (m MobyEngine) ContainerRename(ctx context.Context, id string, options client.ContainerRenameOptions) (client.ContainerRenameResult, error) {
	return m.API.ContainerRename(ctx, id, options)
}
func (m MobyEngine) ContainerStart(ctx context.Context, id string, options client.ContainerStartOptions) (client.ContainerStartResult, error) {
	return m.API.ContainerStart(ctx, id, options)
}
func (m MobyEngine) ContainerRemove(ctx context.Context, id string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	return m.API.ContainerRemove(ctx, id, options)
}
func (m MobyEngine) ContainerUpdate(ctx context.Context, id string, options client.ContainerUpdateOptions) (client.ContainerUpdateResult, error) {
	return m.API.ContainerUpdate(ctx, id, options)
}
func (m MobyEngine) NetworkConnect(ctx context.Context, networkID string, options client.NetworkConnectOptions) (client.NetworkConnectResult, error) {
	return m.API.NetworkConnect(ctx, networkID, options)
}
func (m MobyEngine) ImageInspect(ctx context.Context, imageID string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	return m.API.ImageInspect(ctx, imageID, opts...)
}
func (m MobyEngine) ImagePull(ctx context.Context, refStr string, options client.ImagePullOptions) error {
	rc, err := m.API.ImagePull(ctx, refStr, options)
	if err != nil {
		return err
	}
	defer rc.Close()
	return rc.Wait(ctx)
}

// ContainerApplier prepares signed container upgrades and launches the one-shot updater helper.
type ContainerApplier struct {
	Eng            ContainerEngine
	DataDir        string
	CurrentVersion string
	IsHub          bool
	ListenAddr     string
	PubKey         ed25519.PublicKey
	GOOS           string
	GOARCH         string
	Runtime        *RuntimeEnv
	// HelperCheckDelay is how long to watch the started helper before reporting success
	// (default 1.5 s). The helper waits helperStartDelay before it stops this container, so
	// a helper that dies at once (e.g. can't reach the engine) is caught here.
	HelperCheckDelay time.Duration
}

// labelUpdateTarget names the container an updater helper works on, so a second update
// of the same container is refused while a helper still runs.
const labelUpdateTarget = "moorage.update-target"

func (c *ContainerApplier) platform() (string, error) {
	goos := c.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	goarch := c.GOARCH
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	return PlatformFor(goos, goarch)
}

func (c *ContainerApplier) runtimeEnv() RuntimeEnv {
	re := defaultRuntimeEnv(true)
	if c.Runtime != nil {
		re = *c.Runtime
	}
	if re.Getenv == nil {
		re.Getenv = os.Getenv
	}
	return re
}

// PrepareAndLaunchHelper verifies the signed image digest, pulls the image if not already
// cached locally, inspects the running Moorage container, and starts the one-shot helper
// container (`moorage.role=updater`, AutoRemove=true) from the new image.
func (c *ContainerApplier) PrepareAndLaunchHelper(ctx context.Context, m host.SelfUpdateManifest) error {
	if c.Eng == nil {
		return host.ErrSelfUpdateUnsupported
	}
	if _, err := ParseVersion(m.Version); err != nil {
		return fmt.Errorf("%w: %v", host.ErrSelfUpdateInvalid, err)
	}
	if cur, err := ParseVersion(c.CurrentVersion); err == nil {
		target, _ := ParseVersion(m.Version)
		cmp := compareSemVer(target, cur)
		if cmp < 0 {
			return fmt.Errorf("%w: refusing downgrade from %s to %s", host.ErrSelfUpdateInvalid, c.CurrentVersion, m.Version)
		}
		if cmp == 0 {
			return fmt.Errorf("%w: already running version %s", host.ErrSelfUpdateInvalid, c.CurrentVersion)
		}
	}

	platform, err := c.platform()
	if err != nil {
		return fmt.Errorf("%w: %v", host.ErrSelfUpdateUnsupported, err)
	}
	pub := c.PubKey
	if len(pub) == 0 {
		pub, err = DefaultPublicKey()
		if err != nil {
			return fmt.Errorf("%w: %v", host.ErrSelfUpdateInvalid, err)
		}
	}
	imgManifest, wantDigest, err := VerifyContainerDigest(m, platform, pub)
	if err != nil {
		return err
	}

	re := c.runtimeEnv()
	selfInsp, reason := findSelfContainer(ctx, c.Eng, re)
	if selfInsp == nil {
		return fmt.Errorf("%w: %s", host.ErrSelfUpdateUnsupported, reason)
	}
	if !hasContainerSocket(selfInsp, re.Getenv("DOCKER_HOST")) {
		return fmt.Errorf("%w: engine socket not mounted in container", host.ErrSelfUpdateUnsupported)
	}
	if err := c.refuseIfHelperRunning(ctx, selfInsp); err != nil {
		return err
	}

	imageRef := imgManifest.Image + "@" + wantDigest
	resolvedRef, err := ensureImagePresent(ctx, c.Eng, imageRef, wantDigest)
	if err != nil {
		return err
	}

	clearRollbackNote(c.DataDir)
	return c.launchHelper(ctx, selfInsp, resolvedRef, m.Version, false)
}

// LaunchRollbackHelper starts a one-shot helper container using the current image to
// restore `<name>-moorage-prev`.
func (c *ContainerApplier) LaunchRollbackHelper(ctx context.Context) error {
	if c.Eng == nil {
		return host.ErrSelfUpdateUnsupported
	}
	rec, ok := readPrevRecord(c.DataDir)
	if !ok || rec.Mode != "container" || rec.PrevContainer == "" {
		return fmt.Errorf("%w: no previous container kept for rollback", host.ErrSelfUpdateUnsupported)
	}
	if _, err := c.Eng.ContainerInspect(ctx, rec.PrevContainer, client.ContainerInspectOptions{}); err != nil {
		if isNotFoundErr(err) {
			return fmt.Errorf("%w: previous container %s no longer exists", host.ErrSelfUpdateUnsupported, rec.PrevContainer)
		}
		return err
	}
	selfInsp, reason := findSelfContainer(ctx, c.Eng, c.runtimeEnv())
	if selfInsp == nil {
		return fmt.Errorf("%w: %s", host.ErrSelfUpdateUnsupported, reason)
	}
	if err := c.refuseIfHelperRunning(ctx, selfInsp); err != nil {
		return err
	}
	// The helper runs from the image ID this container was created from: a tag (even
	// image@digest of a mirror) may have moved or been removed since.
	imageRef := selfInsp.Image
	if imageRef == "" && selfInsp.Config != nil {
		imageRef = selfInsp.Config.Image
	}
	if imageRef == "" {
		return fmt.Errorf("%w: current container has no image reference", host.ErrSelfUpdateUnsupported)
	}
	clearRollbackNote(c.DataDir)
	return c.launchHelper(ctx, selfInsp, imageRef, rec.Version, true)
}

func helperNameFor(target *container.InspectResponse) string {
	return strings.TrimPrefix(target.Name, "/") + "-moorage-updater"
}

// refuseIfHelperRunning returns ErrSelfUpdateBusy while an updater helper for this
// container still runs (e.g. waiting for health after a previous update). Force-removing
// it mid-swap could leave the node renamed and stopped. Exited leftovers are removed.
func (c *ContainerApplier) refuseIfHelperRunning(ctx context.Context, target *container.InspectResponse) error {
	list, err := c.Eng.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", engine.LabelRole+"="+engine.RoleUpdater),
	})
	if err != nil {
		return fmt.Errorf("list update helpers: %w", err)
	}
	name := strings.TrimPrefix(target.Name, "/")
	helperName := helperNameFor(target)
	for _, s := range list.Items {
		if s.Labels[engine.LabelRole] != engine.RoleUpdater {
			continue
		}
		isOurs := s.Labels[labelUpdateTarget] == name || s.Labels[labelUpdateTarget] == target.ID
		for _, n := range s.Names {
			if strings.TrimPrefix(n, "/") == helperName {
				isOurs = true
			}
		}
		if !isOurs {
			continue
		}
		if s.State == container.StateRunning || s.State == container.StateRestarting || s.State == container.StateCreated {
			return fmt.Errorf("%w: update helper %s is still running for this container", host.ErrSelfUpdateBusy, strings.TrimPrefix(firstName(s.Names), "/"))
		}
		_, _ = c.Eng.ContainerRemove(ctx, s.ID, client.ContainerRemoveOptions{})
	}
	return nil
}

func firstName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func (c *ContainerApplier) launchHelper(ctx context.Context, selfInsp *container.InspectResponse, helperImage, toVersion string, rollback bool) error {
	args := []string{
		"self-update-helper",
		"--target", selfInsp.ID,
		"--image", helperImage,
		"--from", c.CurrentVersion,
		"--to", toVersion,
	}
	if c.IsHub {
		args = append(args, "--hub", "--listen", c.ListenAddr)
	}
	if rollback {
		args = append(args, "--rollback")
	}

	var mounts []mount.Mount
	// Pass DOCKER_HOST if remote/custom socket, otherwise copy the socket bind mount so the
	// helper container reaches the same Docker/Podman daemon as this container.
	var env []string
	if dockerHost := c.runtimeEnv().Getenv("DOCKER_HOST"); dockerHost != "" {
		env = append(env, "DOCKER_HOST="+dockerHost)
	}
	if sockBind, ok := socketMountSpec(selfInsp); ok {
		mounts = append(mounts, sockBind)
	}

	// Share /data with the helper so self-update-healthy.json, self-update-prev.json, and
	// self-update-rollback.json persist directly in the node's data directory.
	if dataMount, dataTarget, ok := dataMountSpec(selfInsp, c.DataDir); ok {
		mounts = append(mounts, dataMount)
		args = append(args, "--data-dir", dataTarget)
	}

	netMode, netCfg := helperNetwork(selfInsp)
	cfg := &container.Config{
		Image:      helperImage,
		Entrypoint: []string{"/moorage"},
		Cmd:        args,
		Env:        env,
		Labels: map[string]string{
			engine.LabelRole:  engine.RoleUpdater,
			labelUpdateTarget: strings.TrimPrefix(selfInsp.Name, "/"),
		},
	}
	hc := &container.HostConfig{
		AutoRemove:  true,
		Mounts:      mounts,
		NetworkMode: netMode,
	}
	// Run the helper with the target's identity and confinement: the same user and extra
	// groups (e.g. the docker socket's group) reach the engine socket and write status
	// files the target can read; the same security options keep it no less confined.
	if selfInsp.Config != nil {
		cfg.User = selfInsp.Config.User
	}
	if selfInsp.HostConfig != nil {
		hc.GroupAdd = append([]string(nil), selfInsp.HostConfig.GroupAdd...)
		hc.SecurityOpt = append([]string(nil), selfInsp.HostConfig.SecurityOpt...)
	}

	created, err := c.Eng.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:             helperNameFor(selfInsp),
		Config:           cfg,
		HostConfig:       hc,
		NetworkingConfig: netCfg,
	})
	if err != nil {
		return fmt.Errorf("create self-update helper container: %w", err)
	}
	if _, err := c.Eng.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		_, _ = c.Eng.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true})
		return fmt.Errorf("start self-update helper container: %w", err)
	}
	return c.confirmHelperAlive(ctx, created.ID)
}

// confirmHelperAlive watches the helper briefly so the node only reports the update as
// accepted when the helper got going. A helper that can't reach the engine (socket
// permissions, DOCKER_HOST not reachable from its network) exits within milliseconds;
// AutoRemove then deletes it, so "gone" counts as failed too.
func (c *ContainerApplier) confirmHelperAlive(ctx context.Context, id string) error {
	delay := c.HelperCheckDelay
	if delay <= 0 {
		delay = 1500 * time.Millisecond
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
	}
	res, err := c.Eng.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		if isNotFoundErr(err) {
			return fmt.Errorf("%w: update helper exited immediately; check that it can reach the container engine", host.ErrSelfUpdateUnsupported)
		}
		return fmt.Errorf("inspect self-update helper: %w", err)
	}
	if st := res.Container.State; st != nil && !st.Running {
		_, _ = c.Eng.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
		return fmt.Errorf("%w: update helper exited immediately (status %s, exit %d); check that it can reach the container engine", host.ErrSelfUpdateUnsupported, st.Status, st.ExitCode)
	}
	return nil
}

// helperNetwork puts the helper into the target's network context: the same host/none/
// container:* mode, or the target's primary network. The hub helper polls the new hub's
// HTTP port there, and a DOCKER_HOST like tcp://socket-proxy:2375 only resolves there.
func helperNetwork(target *container.InspectResponse) (container.NetworkMode, *network.NetworkingConfig) {
	if target.HostConfig != nil {
		mode := target.HostConfig.NetworkMode
		if mode.IsHost() || mode.IsNone() || mode.IsContainer() {
			return mode, nil
		}
	}
	if target.NetworkSettings == nil || len(target.NetworkSettings.Networks) == 0 {
		return "", nil
	}
	names := make([]string, 0, len(target.NetworkSettings.Networks))
	for n := range target.NetworkSettings.Networks {
		names = append(names, n)
	}
	sort.Strings(names)
	primary := names[0]
	if target.HostConfig != nil {
		if _, ok := target.NetworkSettings.Networks[string(target.HostConfig.NetworkMode)]; ok {
			primary = string(target.HostConfig.NetworkMode)
		}
	}
	return container.NetworkMode(primary), &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{primary: {}},
	}
}

func ensureImagePresent(ctx context.Context, eng ContainerEngine, imageRef, wantDigest string) (string, error) {
	// Check if image@digest already exists locally.
	if _, err := eng.ImageInspect(ctx, imageRef); err == nil {
		return imageRef, nil
	}
	// Also support content-addressed local image IDs (used in offline/local tests) ONLY when
	// wantDigest matches the inspected image's ID or RepoDigests.
	if res, err := eng.ImageInspect(ctx, wantDigest); err == nil {
		if res.ID == wantDigest || containsString(res.RepoDigests, imageRef) {
			return wantDigest, nil
		}
	}
	if err := eng.ImagePull(ctx, imageRef, client.ImagePullOptions{}); err != nil {
		return "", fmt.Errorf("pull verified image %s (host needs registry access): %w", imageRef, err)
	}
	return imageRef, nil
}

func containsString(list []string, target string) bool {
	for _, v := range list {
		if v == target {
			return true
		}
	}
	return false
}

func socketMountSpec(insp *container.InspectResponse) (mount.Mount, bool) {
	if insp == nil {
		return mount.Mount{}, false
	}
	for _, m := range insp.Mounts {
		if strings.HasSuffix(m.Destination, "docker.sock") || strings.HasSuffix(m.Destination, "podman.sock") {
			return dockerclone.MountPointToMount(m), true
		}
	}
	return mount.Mount{}, false
}

func dataMountSpec(insp *container.InspectResponse, dataDir string) (mount.Mount, string, bool) {
	if insp == nil || dataDir == "" {
		return mount.Mount{}, "", false
	}
	cleanData := path.Clean(filepath.ToSlash(dataDir))
	bestLen := -1
	var bestMP container.MountPoint
	var bestTarget string
	for _, m := range insp.Mounts {
		cleanDest := path.Clean(filepath.ToSlash(m.Destination))
		var target string
		switch {
		case cleanData == cleanDest:
			target = "/update-data"
		case strings.HasPrefix(cleanData, cleanDest+"/"):
			rel := strings.TrimPrefix(cleanData, cleanDest+"/")
			target = path.Join("/update-data", rel)
		default:
			continue
		}
		if len(cleanDest) > bestLen {
			bestLen = len(cleanDest)
			bestMP = m
			bestTarget = target
		}
	}
	if bestLen < 0 {
		return mount.Mount{}, "", false
	}
	mp := dockerclone.MountPointToMount(bestMP)
	mp.Target = "/update-data"
	return mp, bestTarget, true
}

func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	if errdefs.IsNotFound(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such container") || strings.Contains(msg, "no such image") || strings.Contains(msg, "not found")
}

// ClonedContainerConfig holds the cloned Docker create options (see dockerclone).
type ClonedContainerConfig = dockerclone.ClonedContainerConfig

// CloneContainerConfig copies the target container's configuration onto newImage.
func CloneContainerConfig(insp container.InspectResponse, newImage string) ClonedContainerConfig {
	return dockerclone.CloneContainerConfig(insp, newImage)
}
