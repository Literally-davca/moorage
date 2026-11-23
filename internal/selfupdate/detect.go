package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"moorage/internal/engine"
	"moorage/internal/host"
)

const (
	prevMetaFile     = "self-update-prev.json"
	pendingStateFile = "self-update-pending.json"
	rollbackNoteFile = "self-update-rollback.json"
	healthyMarkFile  = "self-update-healthy.json"
	prevRetention    = 24 * time.Hour
)

// PrevRecord records the kept previous version (binary or container) on disk.
type PrevRecord struct {
	Mode          string    `json:"mode"` // binary | container
	Version       string    `json:"version"`
	ToVersion     string    `json:"toVersion,omitempty"`
	SavedAt       time.Time `json:"savedAt"`
	ExePath       string    `json:"exePath,omitempty"`
	PrevPath      string    `json:"prevPath,omitempty"`
	ContainerID   string    `json:"containerId,omitempty"`
	ContainerName string    `json:"containerName,omitempty"`
	PrevContainer string    `json:"prevContainer,omitempty"`
	PrevImage     string    `json:"prevImage,omitempty"`
	Compose       bool      `json:"compose,omitempty"`
	// PrevRestartPolicy is the kept container's original restart policy. The kept
	// container is set to "no" so a daemon restart doesn't start it next to the new one;
	// a rollback restores this policy.
	PrevRestartPolicy *container.RestartPolicy `json:"prevRestartPolicy,omitempty"`
}

// PendingState tracks an in-progress binary re-exec or container swap across restart.
type PendingState struct {
	Action        string    `json:"action"` // apply | rollback
	Mode          string    `json:"mode"`
	FromVersion   string    `json:"fromVersion"`
	ToVersion     string    `json:"toVersion"`
	ExePath       string    `json:"exePath,omitempty"`
	PrevPath      string    `json:"prevPath,omitempty"`
	DataDir       string    `json:"dataDir"`
	IsHub         bool      `json:"isHub"`
	ListenAddr    string    `json:"listenAddr,omitempty"`
	PID           int       `json:"pid,omitempty"`
	Args          []string  `json:"args,omitempty"`
	StartedAt     time.Time `json:"startedAt"`
	Deadline      time.Time `json:"deadline"`
	TargetID      string    `json:"targetId,omitempty"`
	TargetName    string    `json:"targetName,omitempty"`
	PrevContainer string    `json:"prevContainer,omitempty"`
	// BootAttempts counts starts of the new binary without confirmed health (BootCheck).
	BootAttempts int `json:"bootAttempts,omitempty"`
}

// RollbackNote records an automatic rollback reason so the hub/UI can surface it once
// the restored previous version comes back.
type RollbackNote struct {
	FromVersion string    `json:"fromVersion"`
	ToVersion   string    `json:"toVersion"`
	Reason      string    `json:"reason"`
	At          time.Time `json:"at"`
}

// HealthyMark is written by a newly started agent once its tunnel /info check responds.
type HealthyMark struct {
	Version string    `json:"version"`
	At      time.Time `json:"at"`
}

// RuntimeEnv allows unit tests to override OS paths and process identity during mode detection.
type RuntimeEnv struct {
	GOOS           string
	GOARCH         string
	Executable     func() (string, error)
	ReadFile       func(string) ([]byte, error)
	Stat           func(string) (os.FileInfo, error)
	Hostname       func() (string, error)
	Getenv         func(string) string
	CheckWritable  func(dir string) error
	SelfUpdateFlag bool
}

func defaultRuntimeEnv(selfUpdate bool) RuntimeEnv {
	return RuntimeEnv{
		GOOS:           runtime.GOOS,
		GOARCH:         runtime.GOARCH,
		Executable:     os.Executable,
		ReadFile:       os.ReadFile,
		Stat:           os.Stat,
		Hostname:       os.Hostname,
		Getenv:         os.Getenv,
		CheckWritable:  probeDirWritable,
		SelfUpdateFlag: selfUpdate,
	}
}

func probeDirWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".moorage-write-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

// DetectStatus inspects the current runtime environment and kept rollback state.
func DetectStatus(ctx context.Context, eng ContainerEngine, dataDir, ver string, selfUpdate bool, env *RuntimeEnv) host.SelfUpdateStatus {
	var re RuntimeEnv
	if env != nil {
		re = *env
	} else {
		re = defaultRuntimeEnv(selfUpdate)
	}
	if re.GOOS == "" {
		re.GOOS = runtime.GOOS
	}
	if re.GOARCH == "" {
		re.GOARCH = runtime.GOARCH
	}
	if re.Executable == nil {
		re.Executable = os.Executable
	}
	if re.ReadFile == nil {
		re.ReadFile = os.ReadFile
	}
	if re.Stat == nil {
		re.Stat = os.Stat
	}
	if re.Hostname == nil {
		re.Hostname = os.Hostname
	}
	if re.Getenv == nil {
		re.Getenv = os.Getenv
	}
	if re.CheckWritable == nil {
		re.CheckWritable = probeDirWritable
	}

	platform, platErr := PlatformFor(re.GOOS, re.GOARCH)
	st := host.SelfUpdateStatus{
		SelfUpdate: re.SelfUpdateFlag,
		Arch:       re.GOARCH,
		Platform:   platform,
		Version:    ver,
	}

	if note, ok := readRollbackNote(dataDir); ok {
		st.LastRollback = note.Reason
	}

	inCtr := inContainerEnv(re)
	if inCtr {
		st.Mode = "container"
		if platErr != nil {
			st.Reason = platErr.Error()
			fillPrevStatus(ctx, eng, dataDir, &st)
			return st
		}
		if eng == nil {
			st.Reason = "Hub-only container has no Docker socket mounted; update from the container host"
			fillPrevStatus(ctx, eng, dataDir, &st)
			return st
		}
		selfInsp, reason := findSelfContainer(ctx, eng, re)
		if selfInsp == nil {
			st.Mode = "unknown"
			st.Reason = reason
			return st
		}
		st.ContainerName = strings.TrimPrefix(selfInsp.Name, "/")
		if selfInsp.Config != nil {
			st.Image = selfInsp.Config.Image
			st.ComposeProject = selfInsp.Config.Labels["com.docker.compose.project"]
		}
		if !hasContainerSocket(selfInsp, re.Getenv("DOCKER_HOST")) {
			st.Reason = "Container has no engine socket mounted; mount /var/run/docker.sock to allow self-update"
			fillPrevStatus(ctx, eng, dataDir, &st)
			return st
		}
		if !re.SelfUpdateFlag {
			st.Reason = host.ErrSelfUpdateDisabled.Error()
			fillPrevStatus(ctx, eng, dataDir, &st)
			return st
		}
		st.Supported = true
		fillPrevStatus(ctx, eng, dataDir, &st)
		return st
	}

	// Non-container mode: check executable path & writability.
	st.Mode = "binary"
	if platErr != nil {
		st.Reason = platErr.Error()
		fillPrevStatus(ctx, eng, dataDir, &st)
		return st
	}
	exePath, err := re.Executable()
	if err != nil {
		st.Mode = "unknown"
		st.Reason = "cannot resolve executable path: " + err.Error()
		return st
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	if err := re.CheckWritable(filepath.Dir(exePath)); err != nil {
		st.Reason = "binary folder is not writable: " + err.Error()
		fillPrevStatus(ctx, eng, dataDir, &st)
		return st
	}
	if !re.SelfUpdateFlag {
		st.Reason = host.ErrSelfUpdateDisabled.Error()
		fillPrevStatus(ctx, eng, dataDir, &st)
		return st
	}
	st.Supported = true
	fillPrevStatus(ctx, eng, dataDir, &st)
	return st
}

func inContainerEnv(re RuntimeEnv) bool {
	if _, err := re.Stat("/.dockerenv"); err == nil {
		return true
	}
	if _, err := re.Stat("/run/.containerenv"); err == nil {
		return true
	}
	if b, err := re.ReadFile("/proc/self/cgroup"); err == nil && len(extractContainerIDs(string(b))) > 0 {
		return true
	}
	if b, err := re.ReadFile("/proc/self/mountinfo"); err == nil && len(extractContainerIDs(string(b))) > 0 {
		return true
	}
	return false
}

// findSelfContainer identifies the container this process runs in. A container only
// counts when it is running and runs the Moorage binary: replacing the wrong container
// (a sidecar sharing our hostname, say) would destroy someone else's workload.
func findSelfContainer(ctx context.Context, eng ContainerEngine, re RuntimeEnv) (*container.InspectResponse, string) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	list, err := eng.ContainerList(cctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, "cannot query engine for self container: " + err.Error()
	}

	inspectMoorage := func(id string) (*container.InspectResponse, bool) {
		insp, err := eng.ContainerInspect(cctx, id, client.ContainerInspectOptions{})
		if err != nil || !runsMoorage(insp.Container) {
			return nil, false
		}
		return &insp.Container, true
	}

	// Container IDs from mountinfo / cgroup are exact: the first match that runs
	// Moorage is us.
	var hints []string
	if b, err := re.ReadFile("/proc/self/mountinfo"); err == nil {
		hints = append(hints, extractContainerIDs(string(b))...)
	}
	if b, err := re.ReadFile("/proc/self/cgroup"); err == nil {
		hints = append(hints, extractContainerIDs(string(b))...)
	}
	for _, h := range hints {
		for _, c := range list.Items {
			if c.ID == h || strings.HasPrefix(c.ID, h) || strings.HasPrefix(h, c.ID) {
				if insp, ok := inspectMoorage(c.ID); ok {
					return insp, ""
				}
			}
		}
	}

	// Hostname fallbacks are ambiguous (custom hostnames repeat, short IDs may collide):
	// collect every candidate and refuse unless exactly one runs Moorage.
	hn, _ := re.Hostname()
	if hn == "" {
		return nil, "running inside a container, but self container ID could not be matched against engine"
	}
	candidates := map[string]*container.InspectResponse{}
	for _, c := range list.Items {
		if engine.IsInternalContainer(c) {
			continue
		}
		match := len(hn) >= 12 && strings.HasPrefix(c.ID, hn)
		for _, n := range c.Names {
			if strings.TrimPrefix(n, "/") == hn {
				match = true
			}
		}
		if !match && c.State != container.StateRunning {
			continue
		}
		insp, ok := inspectMoorage(c.ID)
		if !ok {
			continue
		}
		if match || (insp.Config != nil && insp.Config.Hostname == hn) {
			candidates[insp.ID] = insp
		}
	}
	switch len(candidates) {
	case 0:
		return nil, "running inside a container, but self container ID could not be matched against engine"
	case 1:
		for _, insp := range candidates {
			return insp, ""
		}
	}
	return nil, fmt.Sprintf("hostname %q matches %d running Moorage containers; cannot tell which one is this node", hn, len(candidates))
}

// runsMoorage reports whether a container is running the Moorage binary: the official
// image's entrypoint is /moorage.
func runsMoorage(c container.InspectResponse) bool {
	if c.State == nil || !c.State.Running {
		return false
	}
	if path.Base(c.Path) == "moorage" {
		return true
	}
	if c.Config != nil && len(c.Config.Entrypoint) > 0 && path.Base(c.Config.Entrypoint[0]) == "moorage" {
		return true
	}
	return false
}

func extractContainerIDs(text string) []string {
	var ids []string
	seen := map[string]bool{}
	add := func(cand string) {
		cand = strings.ToLower(cand)
		if len(cand) >= 12 && len(cand) <= 64 && isHex(cand) && !seen[cand] {
			seen[cand] = true
			ids = append(ids, cand)
		}
	}
	for line := range strings.SplitSeq(text, "\n") {
		for _, marker := range []string{"/docker/containers/", "/overlay-containers/", "/userdata/"} {
			if idx := strings.Index(line, marker); idx >= 0 {
				rest := line[idx+len(marker):]
				if slash := strings.IndexByte(rest, '/'); slash >= 12 {
					add(rest[:slash])
				}
			}
		}
		for _, marker := range []string{"docker-", "libpod-"} {
			if idx := strings.Index(line, marker); idx >= 0 {
				rest := line[idx+len(marker):]
				rest = strings.TrimSuffix(rest, ".scope")
				if dot := strings.IndexByte(rest, '.'); dot >= 12 {
					rest = rest[:dot]
				}
				if len(rest) >= 12 {
					add(rest)
				}
			}
		}
		// Match cgroup v1/v2 trailing 64-hex container ID path component.
		fields := strings.Split(strings.TrimSpace(line), "/")
		last := fields[len(fields)-1]
		if len(last) == 64 && isHex(last) {
			add(last)
		}
	}
	return ids
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return len(s) > 0
}

func hasContainerSocket(insp *container.InspectResponse, dockerHost string) bool {
	if strings.TrimSpace(dockerHost) != "" {
		return true
	}
	if insp == nil {
		return false
	}
	for _, m := range insp.Mounts {
		if strings.HasSuffix(m.Destination, "docker.sock") || strings.HasSuffix(m.Destination, "podman.sock") {
			return true
		}
	}
	return false
}

func fillPrevStatus(ctx context.Context, eng ContainerEngine, dataDir string, st *host.SelfUpdateStatus) {
	rec, ok := readPrevRecord(dataDir)
	if !ok {
		return
	}
	switch rec.Mode {
	case "binary":
		if _, err := os.Stat(rec.PrevPath); err != nil {
			return
		}
	case "container":
		if eng == nil || rec.PrevContainer == "" {
			return
		}
		if _, err := eng.ContainerInspect(ctx, rec.PrevContainer, client.ContainerInspectOptions{}); err != nil {
			return
		}
	}
	st.PrevVersion = rec.Version
	st.PrevSavedAt = rec.SavedAt
}

func readJSONFile[T any](path string) (T, bool) {
	var zero T
	if path == "" {
		return zero, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return zero, false
	}
	var v T
	if json.Unmarshal(b, &v) != nil {
		return zero, false
	}
	return v, true
}

func writeJSONFile(path string, v any, mode os.FileMode) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readPrevRecord(dataDir string) (PrevRecord, bool) {
	return readJSONFile[PrevRecord](filepath.Join(dataDir, prevMetaFile))
}

func readRollbackNote(dataDir string) (RollbackNote, bool) {
	return readJSONFile[RollbackNote](filepath.Join(dataDir, rollbackNoteFile))
}

func readHealthyMark(dataDir string) (HealthyMark, bool) {
	return readJSONFile[HealthyMark](filepath.Join(dataDir, healthyMarkFile))
}

// Status files below hold no secrets. They are written 0644 and owned like the data dir
// so the node reads them even when an update helper running as another user wrote them.
func writeRollbackNote(dataDir string, note RollbackNote) {
	writeSharedJSON(dataDir, rollbackNoteFile, note)
}

func writePrevRecord(dataDir string, rec PrevRecord) {
	writeSharedJSON(dataDir, prevMetaFile, rec)
}

func writeHealthyMark(dataDir string, mark HealthyMark) {
	writeSharedJSON(dataDir, healthyMarkFile, mark)
}

func writeSharedJSON(dataDir, name string, v any) {
	if dataDir == "" {
		return
	}
	path := filepath.Join(dataDir, name)
	if err := writeJSONFile(path, v, 0o644); err != nil {
		return
	}
	if fi, err := os.Stat(dataDir); err == nil {
		chownLike(path, fi)
	}
}

func clearRollbackNote(dataDir string) {
	if dataDir != "" {
		_ = os.Remove(filepath.Join(dataDir, rollbackNoteFile))
	}
}
