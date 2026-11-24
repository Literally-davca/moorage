package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"moorage/internal/dockerclone"
)

// Docker outside the stacks folder (MOORAGE_MANAGE_EXTERNAL, MOORAGE_EXTRA_STACKS_DIRS).
//
// Level 1, through the Docker Engine API only: start, stop and restart of any compose
// project and of standalone containers, and "Pull & Recreate" of a container with all its
// settings kept. Level 2: a stack whose folder lies in MOORAGE_EXTRA_STACKS_DIRS (or whose
// compose working_dir is readable and writable here, and isn't a throw-away build folder)
// becomes a fully managed stack: edit, lint, deploy, history.

// Errors of the host security locks (env only on the host).
var (
	ErrExternalLocked     = errors.New("set MOORAGE_MANAGE_EXTERNAL=true on this host to manage containers outside the stacks folder")
	ErrHostTerminalLocked = errors.New("set MOORAGE_HOST_TERMINAL=true on this host to open a shell on the host itself")
	ErrExecLocked         = errors.New("set MOORAGE_EXEC=true (or MOORAGE_TERMINAL=true) on this host to run commands in containers")
)

// ContainerActions are the actions on a single container.
var ContainerActions = map[string]bool{"start": true, "stop": true, "restart": true, "recreate": true}

// ContainerInfo is one container of the host for the global views (logs, terminal,
// external management).
type ContainerInfo struct {
	Container
	Project    string            `json:"project,omitempty"`    // compose project, "" for standalone containers
	Standalone bool              `json:"standalone,omitempty"` // not started by compose
	Managed    bool              `json:"managed,omitempty"`    // part of a stack Moorage manages
	WorkingDir string            `json:"workingDir,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"` // coolify.* and moorage.* only
}

type extCache struct {
	mu   sync.Mutex
	dirs map[string]string // project -> working dir from compose labels
}

var externalDirs = &extCache{dirs: map[string]string{}}

// AllContainers lists every container of the host, compose or not (Moorage's internal
// helpers excepted).
func (e *Engine) AllContainers(ctx context.Context) ([]ContainerInfo, error) {
	res, err := e.api.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}
	out := make([]ContainerInfo, 0, len(res.Items))
	for _, s := range res.Items {
		if IsInternalContainer(s) {
			continue
		}
		ci := ContainerInfo{Container: toContainer(s), Project: s.Labels[labelProject], WorkingDir: s.Labels[labelWorkingDir]}
		ci.Standalone = ci.Project == ""
		if !ci.Standalone && nameRe.MatchString(ci.Project) {
			ci.Managed = composeFile(filepath.Join(e.cfg.StacksDir, ci.Project)) != "" || e.externalStackDir(ci.Project) != ""
		}
		for k, v := range s.Labels {
			if strings.HasPrefix(k, "coolify.") || strings.HasPrefix(k, "moorage.") {
				if ci.Labels == nil {
					ci.Labels = map[string]string{}
				}
				ci.Labels[k] = v
			}
		}
		out = append(out, ci)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// ManageExternal reports whether this host allows managing Docker outside the stacks folder.
func (e *Engine) ManageExternal() bool { return e.cfg.ManageExternal && !e.cfg.ReadOnly }

// externalStackDir finds the folder of a stack outside MOORAGE_STACKS_DIR that may be
// fully managed: <extra dir>/<name>, or the compose working_dir when it is usable here.
func (e *Engine) externalStackDir(name string) string {
	if !e.ManageExternal() {
		return ""
	}
	for _, root := range e.cfg.ExtraStacksDirs {
		p := filepath.Join(root, name)
		if within(root, p) && composeFile(p) != "" {
			return p
		}
	}
	externalDirs.mu.Lock()
	dir := externalDirs.dirs[name]
	externalDirs.mu.Unlock()
	if usableWorkingDir(dir) {
		return dir
	}
	return ""
}

// rememberWorkingDirs keeps the working dirs that compose labels report, for externalStackDir.
func rememberWorkingDirs(dirs map[string]string) {
	externalDirs.mu.Lock()
	defer externalDirs.mu.Unlock()
	for p, d := range dirs {
		externalDirs.dirs[p] = d
	}
}

// usableWorkingDir: an absolute, existing, writable folder with a compose file that is not
// a throw-away build folder (Coolify's /artifacts, its generated /data/coolify files) and
// not a system folder.
func usableWorkingDir(dir string) bool {
	if dir == "" {
		return false
	}
	usableMu.Lock()
	c, ok := usableCache[dir]
	usableMu.Unlock()
	if ok && time.Since(c.at) < time.Minute {
		return c.ok
	}
	v := checkWorkingDir(dir)
	usableMu.Lock()
	usableCache[dir] = usableResult{v, time.Now()}
	usableMu.Unlock()
	return v
}

type usableResult struct {
	ok bool
	at time.Time
}

var (
	usableMu    sync.Mutex
	usableCache = map[string]usableResult{}
)

func checkWorkingDir(dir string) bool {
	if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || strings.Contains(dir, "..") {
		return false
	}
	if strings.HasPrefix(dir, "/artifacts/") || strings.Contains(dir, "/data/coolify/") || strings.HasPrefix(dir, "/tmp/") {
		return false
	}
	for _, sys := range []string{"/", "/etc", "/usr", "/bin", "/sbin", "/lib", "/proc", "/sys", "/dev", "/boot", "/root", "/var/lib/docker"} {
		if dir == sys || strings.HasPrefix(dir, "/proc/") || strings.HasPrefix(dir, "/sys/") || strings.HasPrefix(dir, "/var/lib/docker/") {
			return false
		}
	}
	if real, err := filepath.EvalSymlinks(dir); err != nil || real != dir {
		return false
	}
	if composeFile(dir) == "" {
		return false
	}
	f, err := os.CreateTemp(dir, ".moorage-w-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

// within reports whether p is inside root after cleaning (no path traversal).
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

// inStacksDir reports whether dir is a stack of MOORAGE_STACKS_DIR itself.
func (e *Engine) inStacksDir(dir string) bool { return within(e.cfg.StacksDir, dir) }

// ExternalStackAction starts, stops or restarts every container of a compose project
// Moorage doesn't manage, through the Docker API (no compose file needed).
func (e *Engine) ExternalStackAction(ctx context.Context, project, action string) (*Op, error) {
	if !e.ManageExternal() {
		return nil, ErrExternalLocked
	}
	if e.IsSelf(project) {
		return nil, ErrSelfStack
	}
	if action != "start" && action != "stop" && action != "restart" {
		return nil, fmt.Errorf("unknown action %q", action)
	}
	byProject, _, err := e.projectContainers(ctx, project)
	if err != nil {
		return nil, err
	}
	cs := byProject[project]
	if len(cs) == 0 {
		return nil, ErrNotFound
	}
	if !e.lock(project) {
		return nil, ErrBusy
	}
	op := e.ops.add(project, action)
	go func() {
		defer e.unlock(project)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		var firstErr error
		for _, c := range cs {
			if err := e.containerOp(ctx, c.ID, action); err != nil {
				fmt.Fprintf(op, "✗ %s %s: %v\n", action, c.Name, err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			fmt.Fprintf(op, "%s %s\n", pastTense(action), c.Name)
		}
		if firstErr == nil {
			fmt.Fprintf(op, "✓ %s finished\n", action)
		}
		op.finish(firstErr)
	}()
	return op, nil
}

func pastTense(a string) string {
	switch a {
	case "start":
		return "Started"
	case "stop":
		return "Stopped"
	case "restart":
		return "Restarted"
	}
	return a
}

func (e *Engine) containerOp(ctx context.Context, id, action string) error {
	timeout := 15
	var err error
	switch action {
	case "start":
		_, err = e.api.ContainerStart(ctx, id, client.ContainerStartOptions{})
	case "stop":
		_, err = e.api.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &timeout})
	case "restart":
		_, err = e.api.ContainerRestart(ctx, id, client.ContainerRestartOptions{Timeout: &timeout})
	default:
		err = fmt.Errorf("unknown action %q", action)
	}
	return err
}

// ContainerAction runs an action on one container that isn't part of a managed stack
// (those go through the stack's own actions). "recreate" pulls the image and recreates
// the container with every setting kept; if the new one doesn't keep running for 10 s the
// previous container comes back.
func (e *Engine) ContainerAction(ctx context.Context, id, action string) (*Op, error) {
	if !e.ManageExternal() {
		return nil, ErrExternalLocked
	}
	if !ContainerActions[action] {
		return nil, fmt.Errorf("unknown action %q", action)
	}
	ir, err := e.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return nil, ErrNotFound
	}
	insp := ir.Container
	project := ""
	if insp.Config != nil {
		project = insp.Config.Labels[labelProject]
	}
	if project != "" && e.IsSelf(project) {
		return nil, ErrSelfStack
	}
	if insp.Config != nil && insp.Config.Labels[LabelRole] != "" {
		return nil, ErrSelfStack // Moorage's own helpers
	}
	name := strings.TrimPrefix(insp.Name, "/")
	key := "ctr:" + insp.ID
	if !e.lock(key) {
		return nil, ErrBusy
	}
	op := e.ops.add(name, action)
	go func() {
		defer e.unlock(key)
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		var err error
		if action == "recreate" {
			err = e.recreateContainer(ctx, op, insp)
		} else {
			err = e.containerOp(ctx, insp.ID, action)
		}
		if err != nil {
			fmt.Fprintf(op, "✗ %s failed: %v\n", action, err)
		} else {
			fmt.Fprintf(op, "✓ %s finished\n", action)
		}
		op.finish(err)
	}()
	return op, nil
}

// recreateContainer is "Pull & Recreate": pull the image the container was started from,
// then swap the container for a clone on the new image, keeping the old one as
// <name>-moorage-prev until the new one has run for 10 s.
func (e *Engine) recreateContainer(ctx context.Context, op *Op, insp container.InspectResponse) error {
	if insp.Config == nil || insp.Config.Image == "" {
		return errors.New("the container has no image reference to pull")
	}
	ref := insp.Config.Image
	fmt.Fprintf(op, "Pulling %s\n", ref)
	creds, _ := readRegistryAuth(ref)
	if err := e.imageBackend().PullImage(ctx, ref, creds); err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	name := strings.TrimPrefix(insp.Name, "/")
	prev := name + PrevContainerSuffix
	if _, err := e.api.ContainerInspect(ctx, prev, client.ContainerInspectOptions{}); err == nil {
		_, _ = e.api.ContainerRemove(ctx, prev, client.ContainerRemoveOptions{Force: true})
	}
	wasRunning := insp.State != nil && insp.State.Running
	timeout := 15
	if _, err := e.api.ContainerStop(ctx, insp.ID, client.ContainerStopOptions{Timeout: &timeout}); err != nil {
		return err
	}
	if _, err := e.api.ContainerRename(ctx, insp.ID, client.ContainerRenameOptions{NewName: prev}); err != nil {
		if wasRunning {
			_, _ = e.api.ContainerStart(ctx, insp.ID, client.ContainerStartOptions{})
		}
		return err
	}
	restore := func(reason string) error {
		_, _ = e.api.ContainerRename(ctx, insp.ID, client.ContainerRenameOptions{NewName: name})
		if wasRunning {
			_, _ = e.api.ContainerStart(ctx, insp.ID, client.ContainerStartOptions{})
		}
		fmt.Fprintf(op, "Put the previous container back: %s\n", reason)
		return fmt.Errorf("%s; %w", reason, ErrRolledBack)
	}
	cloned := dockerclone.CloneContainerConfig(insp, ref)
	cloned.Create.Name = name
	created, err := e.api.ContainerCreate(ctx, cloned.Create)
	if err != nil {
		return restore("create: " + err.Error())
	}
	for netName, ep := range cloned.ExtraNetworks {
		if _, err := e.api.NetworkConnect(ctx, netName, client.NetworkConnectOptions{Container: created.ID, EndpointConfig: ep}); err != nil {
			_, _ = e.api.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true})
			return restore("network " + netName + ": " + err.Error())
		}
	}
	if !wasRunning {
		_, _ = e.api.ContainerRemove(ctx, insp.ID, client.ContainerRemoveOptions{})
		fmt.Fprintln(op, "Recreated (left stopped, like before)")
		return nil
	}
	if _, err := e.api.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		_, _ = e.api.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true})
		return restore("start: " + err.Error())
	}
	reason := e.waitHealthy(ctx, created.ID, false, 10*time.Second)
	if reason != "" {
		_, _ = e.api.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true})
		return restore("new container " + reason)
	}
	_, _ = e.api.ContainerRemove(ctx, insp.ID, client.ContainerRemoveOptions{})
	return nil
}
