package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"moorage/internal/stacksnap"
)

// ---- stack and container snapshots (M18F) -------------------------------------------
//
// A snapshot is one <data>/snapshots/<stack>/<time>-<id>.tar.zst with manifest.json
// inside and next to it. A helper container from Moorage's own image copies the data
// (`moorage snapshot-helper`), mounting volumes read-only, so Moorage never reads
// application data itself. Containers are paused (default), stopped or left running
// during the copy; a pause always ends, also after a crash (RecoverSnapshots).

// RoleSnapshot labels the snapshot helper containers.
const RoleSnapshot = "snapshot"

// SnapshotRetainAuto is how many automatic snapshots are kept per stack by default.
const SnapshotRetainAuto = 5

// SnapshotMinFreePct: a snapshot is refused when less than this would stay free.
const SnapshotMinFreePct = 10

// ErrSnapshotSpace is returned when the disk would get too full.
var ErrSnapshotSpace = errors.New("not enough free disk space for the snapshot")

// SnapshotSource is one directory a snapshot can include.
type SnapshotSource struct {
	Kind     string   `json:"kind"`   // volume | bind
	Source   string   `json:"source"` // volume name or host path
	Target   string   `json:"target"`
	Service  string   `json:"service"`
	Users    []string `json:"users,omitempty"` // other services using it
	External bool     `json:"external"`        // a bind mount outside the stack folder: off by default
	Size     int64    `json:"size"`            // -1 = unknown
}

// SnapshotPlan is what the dialog shows before taking a snapshot.
type SnapshotPlan struct {
	Sources   []SnapshotSource `json:"sources"`
	Estimate  int64            `json:"estimate"`  // bytes, uncompressed
	FreeBytes int64            `json:"freeBytes"` // -1 = unknown
	FreePct   float64          `json:"freePct"`   // -1 = unknown
}

// SnapshotRequest starts a snapshot.
type SnapshotRequest struct {
	Service    string   `json:"service,omitempty"` // container snapshot
	Mode       string   `json:"mode"`              // pause | stop | live
	Kind       string   `json:"kind,omitempty"`    // manual (default) | auto | pre-restore
	ExtraBinds []string `json:"extraBinds,omitempty"`
	Note       string   `json:"note,omitempty"`
	Version    string   `json:"version,omitempty"`
	Author     string   `json:"author,omitempty"`
	MinFreePct int      `json:"minFreePct,omitempty"`
	RetainAuto int      `json:"retainAuto,omitempty"`
}

// RestoreRequest restores a snapshot.
type RestoreRequest struct {
	Scope      string `json:"scope"` // all | data | config
	PreRestore bool   `json:"preRestore"`
	Author     string `json:"author,omitempty"`
	Version    string `json:"version,omitempty"`
}

func (e *Engine) snapshotRoot() string { return filepath.Join(e.cfg.DataDir, "snapshots") }

func (e *Engine) snapshotDir(stack string) (string, error) {
	if !nameRe.MatchString(stack) {
		return "", ErrInvalidName
	}
	return filepath.Join(e.snapshotRoot(), stack), nil
}

func validSnapID(id string) bool {
	if id == "" || len(id) > 80 || strings.ContainsAny(id, `/\.`) {
		return false
	}
	return true
}

// SnapshotPlan lists what a snapshot of the stack (or one service) would contain.
func (e *Engine) SnapshotPlan(ctx context.Context, stack, service string) (SnapshotPlan, error) {
	plan := SnapshotPlan{FreeBytes: -1, FreePct: -1}
	srcs, err := e.snapshotSources(ctx, stack, service)
	if err != nil {
		return plan, err
	}
	plan.Sources = srcs
	for _, s := range srcs {
		if !s.External && s.Size > 0 {
			plan.Estimate += s.Size
		}
	}
	plan.FreeBytes, plan.FreePct = diskFree(e.cfg.DataDir)
	return plan, nil
}

// snapshotSources reads the mounts of the stack's containers. Volumes are always in,
// bind mounts inside the stack folder too; other bind mounts are offered, off.
func (e *Engine) snapshotSources(ctx context.Context, stack, service string) ([]SnapshotSource, error) {
	if e.IsSelf(stack) {
		return nil, ErrSelfStack
	}
	st, err := e.GetStack(ctx, stack)
	if err != nil {
		return nil, err
	}
	dir, _ := e.stackDir(stack)
	type key struct{ kind, src string }
	seen := map[key]int{}
	var out []SnapshotSource
	for _, c := range st.Containers {
		ir, err := e.api.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if err != nil {
			continue
		}
		for _, m := range ir.Container.Mounts {
			var s SnapshotSource
			switch m.Type {
			case mount.TypeVolume:
				s = SnapshotSource{Kind: "volume", Source: m.Name, Target: m.Destination, Service: c.Service}
			case mount.TypeBind:
				s = SnapshotSource{Kind: "bind", Source: m.Source, Target: m.Destination, Service: c.Service,
					External: dir == "" || !pathWithin(dir, m.Source)}
				if m.Source == "/var/run/docker.sock" || m.Source == "/run/docker.sock" {
					continue
				}
			default:
				continue // tmpfs, npipe
			}
			k := key{s.Kind, s.Source}
			if i, ok := seen[k]; ok {
				if out[i].Service != c.Service && !contains(out[i].Users, c.Service) {
					out[i].Users = append(out[i].Users, c.Service)
				}
				continue
			}
			if service != "" && c.Service != service {
				continue
			}
			s.Size = -1
			if s.Kind == "bind" {
				if fi, err := os.Stat(s.Source); err == nil && fi.IsDir() {
					s.Size = stacksnap.DirSize(s.Source)
				} else if err == nil {
					s.Size = fi.Size()
				}
			}
			seen[k] = len(out)
			out = append(out, s)
		}
	}
	// volume sizes from docker system df would be slow; the helper reports exact sizes
	if sz, err := e.volumeSizes(ctx); err == nil {
		for i := range out {
			if out[i].Kind == "volume" {
				if n, ok := sz[out[i].Source]; ok {
					out[i].Size = n
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return !out[i].External && out[j].External })
	return out, nil
}

func (e *Engine) volumeSizes(ctx context.Context) (map[string]int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	du, err := e.api.DiskUsage(ctx, client.DiskUsageOptions{Volumes: true})
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, v := range du.Volumes.Items {
		if v.UsageData != nil {
			out[v.Name] = v.UsageData.Size
		}
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func pathWithin(root, p string) bool {
	r, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) && !filepath.IsAbs(r)
}

// ListSnapshots returns the stack's snapshots, newest first.
func (e *Engine) ListSnapshots(stack string) ([]stacksnap.Manifest, error) {
	dir, err := e.snapshotDir(stack)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []stacksnap.Manifest{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []stacksnap.Manifest{}
	for _, en := range entries {
		if !strings.HasSuffix(en.Name(), ".json") || strings.HasPrefix(en.Name(), "job-") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, en.Name()))
		if err != nil {
			continue
		}
		var m stacksnap.Manifest
		if json.Unmarshal(b, &m) != nil || m.ID == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, m.File)); err != nil {
			continue // manifest without its file
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

func (e *Engine) findSnapshot(stack, id string) (stacksnap.Manifest, string, error) {
	if !validSnapID(id) {
		return stacksnap.Manifest{}, "", ErrNotFound
	}
	list, err := e.ListSnapshots(stack)
	if err != nil {
		return stacksnap.Manifest{}, "", err
	}
	dir, _ := e.snapshotDir(stack)
	for _, m := range list {
		if m.ID == id {
			return m, filepath.Join(dir, m.File), nil
		}
	}
	return stacksnap.Manifest{}, "", ErrNotFound
}

// OpenSnapshot opens a snapshot file for download.
func (e *Engine) OpenSnapshot(stack, id string) (io.ReadCloser, stacksnap.Manifest, error) {
	m, file, err := e.findSnapshot(stack, id)
	if err != nil {
		return nil, m, err
	}
	f, err := os.Open(file)
	return f, m, err
}

// DeleteSnapshot removes a snapshot and its manifest.
func (e *Engine) DeleteSnapshot(stack, id string) error {
	_, file, err := e.findSnapshot(stack, id)
	if err != nil {
		return err
	}
	if err := os.Remove(file); err != nil {
		return err
	}
	return os.Remove(stacksnap.ManifestPath(file))
}

// PinSnapshot pins or unpins a snapshot (pinned ones are never pruned).
func (e *Engine) PinSnapshot(stack, id string, pinned bool) error {
	m, file, err := e.findSnapshot(stack, id)
	if err != nil {
		return err
	}
	m.Pinned = pinned
	b, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(stacksnap.ManifestPath(file), b, 0o600)
}

// pruneAuto keeps the newest n automatic snapshots (pinned ones stay).
func (e *Engine) pruneAuto(stack string, n int) {
	if n <= 0 {
		n = SnapshotRetainAuto
	}
	list, err := e.ListSnapshots(stack)
	if err != nil {
		return
	}
	kept := 0
	for _, m := range list {
		if m.Kind != stacksnap.KindAuto || m.Pinned {
			continue
		}
		kept++
		if kept > n {
			_ = e.DeleteSnapshot(stack, m.ID)
		}
	}
}

// CreateSnapshot starts a snapshot as a stack operation (one operation per stack).
func (e *Engine) CreateSnapshot(ctx context.Context, stack string, req SnapshotRequest) (*Op, error) {
	if e.IsSelf(stack) {
		return nil, ErrSelfStack
	}
	if req.Mode == "" {
		req.Mode = stacksnap.ModePause
	}
	if req.Mode != stacksnap.ModePause && req.Mode != stacksnap.ModeStop && req.Mode != stacksnap.ModeLive {
		return nil, fmt.Errorf("unknown mode %q", req.Mode)
	}
	if req.Kind == "" {
		req.Kind = stacksnap.KindManual
	}
	if !e.lock(stack) {
		return nil, ErrBusy
	}
	op := e.ops.add(stack, "snapshot")
	go func() {
		defer e.unlock(stack)
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		start := time.Now()
		m, err := e.takeSnapshot(ctx, op, stack, req)
		if err != nil {
			e.log.Warn("snapshot failed", "stack", stack, "kind", req.Kind, "mode", req.Mode, "err", err)
			fmt.Fprintf(op, "✗ snapshot failed: %v\n", err)
		} else {
			e.log.Info("snapshot created", "stack", stack, "id", m.ID, "kind", req.Kind, "mode", req.Mode, "bytes", m.Size, "ms", time.Since(start).Milliseconds())
			fmt.Fprintf(op, "✓ snapshot %s saved (%s)\n", m.ID, humanBytes(m.Size))
		}
		op.finish(err)
	}()
	return op, nil
}

// takeSnapshot takes a snapshot in the caller's goroutine (also before an update or a
// restore, which already hold the stack lock).
func (e *Engine) takeSnapshot(ctx context.Context, op io.Writer, stack string, req SnapshotRequest) (stacksnap.Manifest, error) {
	var m stacksnap.Manifest
	srcs, err := e.snapshotSources(ctx, stack, req.Service)
	if err != nil {
		return m, err
	}
	extra := map[string]bool{}
	for _, b := range req.ExtraBinds {
		extra[filepath.Clean(b)] = true
	}
	var parts []stacksnap.Part
	var mounts []mount.Mount
	dirs := map[string]string{}
	var est int64
	for i, s := range srcs {
		if s.External && !extra[filepath.Clean(s.Source)] {
			continue
		}
		name := fmt.Sprintf("binds/%d", i)
		if s.Kind == "volume" {
			name = "volumes/" + s.Source
		}
		at := fmt.Sprintf("/src/%d", i)
		typ := mount.TypeBind
		if s.Kind == "volume" {
			typ = mount.TypeVolume
		}
		mounts = append(mounts, mount.Mount{Type: typ, Source: s.Source, Target: at, ReadOnly: true})
		dirs[name] = at
		parts = append(parts, stacksnap.Part{Name: name, Kind: s.Kind, Source: s.Source, Target: s.Target, Service: s.Service, Users: s.Users})
		if s.Size > 0 {
			est += s.Size
		}
	}
	// configuration: compose.yaml and .env of the stack (also for a container snapshot,
	// so a restore can show the diff)
	snapDir, err := e.snapshotDir(stack)
	if err != nil {
		return m, err
	}
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		return m, err
	}
	free, pct := diskFree(e.cfg.DataDir)
	minPct := req.MinFreePct
	if minPct <= 0 {
		minPct = SnapshotMinFreePct
	}
	if free >= 0 && pct >= 0 {
		total := float64(free) / (pct / 100)
		if pct <= 0 || float64(free-est)/total*100 < float64(minPct) {
			return m, fmt.Errorf("%w: about %s needed, %s free (at least %d %% must stay free)", ErrSnapshotSpace, humanBytes(est), humanBytes(free), minPct)
		}
	}
	id := time.Now().UTC().Format("20060102-150405") + "-" + randHex(3)
	cfgDir := filepath.Join(snapDir, ".cfg-"+id)
	if err := e.writeSnapshotConfig(stack, cfgDir); err == nil {
		parts = append(parts, stacksnap.Part{Name: "config", Kind: stacksnap.PartConfig, Source: "compose.yaml + .env"})
		dirs["config"] = cfgDir
	}
	defer os.RemoveAll(cfgDir)

	m = stacksnap.Manifest{ID: id, Stack: stack, Service: req.Service, Kind: req.Kind, Mode: req.Mode, Note: req.Note,
		Time: time.Now().UTC(), Version: req.Version, Parts: parts, Images: map[string]string{}}
	if vs, err := e.StackHistory(stack, 1); err == nil && len(vs) > 0 {
		m.Commit = vs[0].Short
	}
	st, _ := e.GetStack(ctx, stack)
	var ids []string
	for _, c := range st.Containers {
		if req.Service != "" && c.Service != req.Service {
			continue
		}
		m.Images[c.Service] = c.Image
		if c.State == "running" {
			ids = append(ids, c.ID)
		}
	}
	fmt.Fprintf(op, "Snapshot %s of %s: %d parts, about %s\n", id, stack, len(parts), humanBytes(est))

	switch req.Mode {
	case stacksnap.ModePause:
		if err := e.pauseForSnapshot(ctx, op, ids); err != nil {
			return m, err
		}
		defer e.unpauseAfterSnapshot(op, ids)
	case stacksnap.ModeStop:
		for _, id := range ids {
			t := 30
			if _, err := e.api.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &t}); err != nil {
				return m, fmt.Errorf("stop: %w", err)
			}
		}
		fmt.Fprintf(op, "Stopped %d containers\n", len(ids))
		defer func() {
			for _, id := range ids {
				_, _ = e.api.ContainerStart(context.Background(), id, client.ContainerStartOptions{})
			}
			fmt.Fprintf(op, "Started %d containers again\n", len(ids))
		}()
	}

	file := filepath.Join(snapDir, id+".tar.zst")
	job := stacksnap.Job{Op: "create", Manifest: m, Dirs: dirs, File: file}
	if err := e.runSnapshotHelper(ctx, op, snapDir, job, mounts); err != nil {
		_ = os.Remove(file + stacksnap.PartialSuffix)
		return m, err
	}
	b, err := os.ReadFile(stacksnap.ManifestPath(file))
	if err == nil {
		_ = json.Unmarshal(b, &m)
	}
	if req.Kind == stacksnap.KindAuto {
		e.pruneAuto(stack, req.RetainAuto)
	}
	return m, nil
}

// TakeSnapshot is the synchronous form for callers that hold the stack lock (updates).
func (e *Engine) TakeSnapshot(ctx context.Context, op io.Writer, stack string, req SnapshotRequest) (stacksnap.Manifest, error) {
	return e.takeSnapshot(ctx, op, stack, req)
}

func (e *Engine) writeSnapshotConfig(stack, dir string) error {
	sdir, err := e.stackDir(stack)
	if err != nil {
		return err
	}
	file := composeFile(sdir)
	if file == "" {
		return ErrNotFound
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(sdir, file))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), b, 0o600); err != nil {
		return err
	}
	if env, err := os.ReadFile(filepath.Join(sdir, ".env")); err == nil {
		return os.WriteFile(filepath.Join(dir, ".env"), env, 0o600)
	}
	return nil
}

// ---- pause bookkeeping --------------------------------------------------------------

func (e *Engine) pausedPath() string { return filepath.Join(e.snapshotRoot(), "paused.json") }

func (e *Engine) pauseForSnapshot(ctx context.Context, op io.Writer, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_ = os.MkdirAll(e.snapshotRoot(), 0o700)
	b, _ := json.Marshal(ids)
	if err := os.WriteFile(e.pausedPath(), b, 0o600); err != nil {
		return err
	}
	for i, id := range ids {
		if _, err := e.api.ContainerPause(ctx, id, client.ContainerPauseOptions{}); err != nil {
			e.unpauseAfterSnapshot(op, ids[:i])
			return fmt.Errorf("pause: %w", err)
		}
	}
	fmt.Fprintf(op, "Paused %d containers for the copy\n", len(ids))
	return nil
}

// unpauseAfterSnapshot always runs (defer), with its own context so a cancelled
// operation still ends the pause.
func (e *Engine) unpauseAfterSnapshot(op io.Writer, ids []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, id := range ids {
		if _, err := e.api.ContainerUnpause(ctx, id, client.ContainerUnpauseOptions{}); err != nil && op != nil {
			fmt.Fprintf(op, "unpause %s: %v\n", shortID(id), err)
		}
	}
	_ = os.Remove(e.pausedPath())
	if op != nil && len(ids) > 0 {
		fmt.Fprintf(op, "Resumed %d containers\n", len(ids))
	}
}

// RecoverSnapshots runs at start: containers left paused by a crash are resumed and
// unfinished snapshot files removed.
func (e *Engine) RecoverSnapshots(ctx context.Context) {
	if b, err := os.ReadFile(e.pausedPath()); err == nil {
		var ids []string
		if json.Unmarshal(b, &ids) == nil && len(ids) > 0 {
			e.log.Warn("resuming containers paused by an interrupted snapshot", "count", len(ids))
			e.unpauseAfterSnapshot(nil, ids)
		}
	}
	_ = filepath.WalkDir(e.snapshotRoot(), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && strings.HasPrefix(d.Name(), ".cfg-") {
			_ = os.RemoveAll(p)
			return filepath.SkipDir
		}
		if strings.HasSuffix(p, stacksnap.PartialSuffix) || (strings.HasPrefix(d.Name(), "job-") && strings.HasSuffix(p, ".json")) {
			_ = os.Remove(p)
		}
		return nil
	})
	if e.api != nil {
		e.sweepSnapshotHelpers(ctx)
	}
}

func (e *Engine) sweepSnapshotHelpers(ctx context.Context) {
	f := make(client.Filters)
	f.Add("label", LabelRole+"="+RoleSnapshot)
	list, err := e.api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: f})
	if err != nil {
		return
	}
	for _, c := range list.Items {
		_, _ = e.api.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true})
	}
}

// ---- the helper container -------------------------------------------------------------

func (e *Engine) runSnapshotHelper(ctx context.Context, op io.Writer, snapDir string, job stacksnap.Job, mounts []mount.Mount) error {
	img, err := e.findOwnContainerImage(ctx)
	if err != nil {
		return fmt.Errorf("snapshots need Moorage's image on this host: %w", err)
	}
	jobFile := filepath.Join(snapDir, "job-"+randHex(4)+".json")
	b, _ := json.Marshal(job)
	if err := os.WriteFile(jobFile, b, 0o600); err != nil {
		return err
	}
	defer os.Remove(jobFile)
	hc := &container.HostConfig{Mounts: mounts, NetworkMode: "none"}
	// the snapshot folder: the helper sees Moorage's data dir at the same path
	if own := e.ownContainerID(ctx); own != "" {
		hc.VolumesFrom = []string{own + ":rw"}
	} else {
		hc.Binds = []string{e.cfg.DataDir + ":" + e.cfg.DataDir}
	}
	// restore targets need write access, everything else stays read-only
	created, err := e.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      img,
			Entrypoint: []string{"/moorage", "snapshot-helper", jobFile},
			User:       "0:0",
			Labels:     map[string]string{LabelRole: RoleSnapshot},
		},
		HostConfig: hc,
	})
	if err != nil {
		return fmt.Errorf("create helper: %w", err)
	}
	id := created.ID
	defer func() {
		_, _ = e.api.ContainerRemove(context.Background(), id, client.ContainerRemoveOptions{Force: true})
	}()
	if _, err := e.api.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start helper: %w", err)
	}
	wait := e.api.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case err := <-wait.Error:
		return fmt.Errorf("helper: %w", err)
	case res := <-wait.Result:
		if res.StatusCode != 0 {
			msg := e.helperLogs(id)
			return fmt.Errorf("helper exited with %d: %s", res.StatusCode, msg)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (e *Engine) helperLogs(id string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rc, err := e.api.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: "5"})
	if err != nil {
		return "no output"
	}
	defer rc.Close()
	b, _ := io.ReadAll(io.LimitReader(rc, 4096))
	return strings.TrimSpace(stripDockerLogHeaders(b))
}

// stripDockerLogHeaders removes the 8-byte frame headers of multiplexed logs.
func stripDockerLogHeaders(b []byte) string {
	var out strings.Builder
	for len(b) >= 8 && (b[0] == 1 || b[0] == 2) {
		n := int(b[4])<<24 | int(b[5])<<16 | int(b[6])<<8 | int(b[7])
		b = b[8:]
		if n > len(b) {
			n = len(b)
		}
		out.Write(b[:n])
		b = b[n:]
	}
	out.Write(b)
	return out.String()
}

// ownContainerID is the container Moorage runs in ("" for a native install).
func (e *Engine) ownContainerID(ctx context.Context) string {
	hn, err := os.Hostname()
	if err != nil || hn == "" {
		return ""
	}
	ir, err := e.api.ContainerInspect(ctx, hn, client.ContainerInspectOptions{})
	if err != nil {
		return ""
	}
	return ir.Container.ID
}

// ---- restore --------------------------------------------------------------------------

// RestoreSnapshot restores data and/or configuration: an automatic pre-restore snapshot
// first (unless turned off), then stop, restore, save the configuration as a new
// version and up.
func (e *Engine) RestoreSnapshot(ctx context.Context, stack, id string, req RestoreRequest) (*Op, error) {
	if e.IsSelf(stack) {
		return nil, ErrSelfStack
	}
	m, file, err := e.findSnapshot(stack, id)
	if err != nil {
		return nil, err
	}
	if req.Scope == "" {
		req.Scope = "all"
	}
	if req.Scope != "all" && req.Scope != "data" && req.Scope != "config" {
		return nil, fmt.Errorf("unknown scope %q", req.Scope)
	}
	if !e.lock(stack) {
		return nil, ErrBusy
	}
	op := e.ops.add(stack, "restore-snapshot")
	go func() {
		defer e.unlock(stack)
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		err := e.restoreFromSnapshot(ctx, op, stack, m, file, req)
		if err != nil {
			e.log.Error("snapshot restore failed", "stack", stack, "id", m.ID, "scope", req.Scope, "err", err)
			fmt.Fprintf(op, "✗ restore failed: %v\n", err)
		} else {
			e.log.Info("snapshot restored", "stack", stack, "id", m.ID, "scope", req.Scope, "user", req.Author)
			fmt.Fprintf(op, "✓ restored snapshot %s\n", m.ID)
		}
		op.finish(err)
	}()
	return op, nil
}

func (e *Engine) restoreFromSnapshot(ctx context.Context, op *Op, stack string, m stacksnap.Manifest, file string, req RestoreRequest) error {
	if req.PreRestore {
		fmt.Fprintln(op, "Taking a pre-restore snapshot…")
		if _, err := e.takeSnapshot(ctx, op, stack, SnapshotRequest{Mode: stacksnap.ModePause, Kind: stacksnap.KindPreRestore, Service: m.Service,
			Version: req.Version, Note: "before restoring " + m.ID}); err != nil {
			return fmt.Errorf("pre-restore snapshot: %w", err)
		}
	}
	dir, err := e.stackDir(stack)
	if err != nil {
		return err
	}
	cf := composeFile(dir)
	data := req.Scope == "all" || req.Scope == "data"
	cfg := (req.Scope == "all" || req.Scope == "config") && m.Service == ""
	if data {
		// the stack's current mounts: a restore writes only into these, never into a
		// path or volume the manifest names on its own
		srcs, _ := e.snapshotSources(ctx, stack, "")
		current := map[string]bool{}
		for _, s := range srcs {
			current[s.Kind+"\x00"+s.Source] = true
		}
		// stop the services whose data comes back (all of them for a stack snapshot)
		if cf != "" {
			if err := e.runCompose(ctx, op, stack, dir, cf, "stop"); err != nil {
				return fmt.Errorf("stop: %w", err)
			}
		}
		var mounts []mount.Mount
		dirs := map[string]string{}
		for i, p := range m.Parts {
			if p.Kind == stacksnap.PartConfig {
				continue
			}
			at := fmt.Sprintf("/dst/%d", i)
			src := p.Source
			if p.Kind == stacksnap.PartVolume {
				// anonymous volumes get new names after down/recreate: map by service + target
				for _, s := range srcs {
					if s.Kind == "volume" && s.Service == p.Service && s.Target == p.Target {
						src = s.Source
					}
				}
			}
			kind := "bind"
			if p.Kind == stacksnap.PartVolume {
				kind = "volume"
			}
			if !current[kind+"\x00"+src] {
				fmt.Fprintf(op, "Skipping %s %s: the stack doesn't mount it now\n", kind, src)
				continue
			}
			if p.Kind == stacksnap.PartVolume {
				mounts = append(mounts, mount.Mount{Type: mount.TypeVolume, Source: src, Target: at})
			} else {
				mounts = append(mounts, mount.Mount{Type: mount.TypeBind, Source: src, Target: at})
			}
			dirs[p.Name] = at
		}
		if len(dirs) > 0 {
			fmt.Fprintf(op, "Restoring %d data parts…\n", len(dirs))
			if err := e.runSnapshotHelper(ctx, op, filepath.Dir(file), stacksnap.Job{Op: "restore", Dirs: dirs, File: file}, mounts); err != nil {
				return err
			}
		}
	}
	if cfg {
		compose, env, err := snapshotConfig(file)
		if err != nil {
			return fmt.Errorf("read configuration from the snapshot: %w", err)
		}
		if compose != "" {
			author := req.Author
			if author == "" {
				author = "moorage"
			}
			if err := e.saveStack(ctx, stack, compose, env, author, false, "Restore snapshot "+m.ID); err != nil {
				return fmt.Errorf("save configuration: %w", err)
			}
			fmt.Fprintln(op, "Configuration saved as a new version")
			cf = composeFile(dir)
		}
	}
	if cf != "" {
		return e.runCompose(ctx, op, stack, dir, cf, "up")
	}
	return nil
}

// snapshotConfig reads compose.yaml and .env from the config part of a snapshot.
func snapshotConfig(file string) (string, string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	tmp, err := os.MkdirTemp("", "moorage-snapcfg-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(tmp)
	if err := stacksnap.Extract(f, map[string]string{"config": tmp}); err != nil {
		return "", "", err
	}
	c, _ := os.ReadFile(filepath.Join(tmp, "compose.yaml"))
	env, _ := os.ReadFile(filepath.Join(tmp, ".env"))
	return string(c), string(env), nil
}

// SnapshotConfig returns compose.yaml and .env stored in a snapshot (restore preview).
func (e *Engine) SnapshotConfig(stack, id string) (string, string, error) {
	_, file, err := e.findSnapshot(stack, id)
	if err != nil {
		return "", "", err
	}
	return snapshotConfig(file)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func humanBytes(n int64) string {
	switch {
	case n < 0:
		return "unknown"
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
