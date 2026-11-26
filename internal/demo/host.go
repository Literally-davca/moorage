// Package demo backs the read-only demo mode (MOORAGE_DEMO=true, M18C): a fake Docker
// host with sample stacks, containers, logs and history, and sample checks, a channel and
// a status page in the database. No Docker socket is ever opened and nothing can change.
package demo

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
)

// ErrDemo is returned by every write on the demo host. The API refuses writes before
// they get here; this is the second line.
var ErrDemo = fmt.Errorf("%w: this is a read-only demo", engine.ErrReadOnly)

// HostName is the name of the demo host in the UI.
const HostName = "demo-host"

type service struct {
	name, image, state, health string
	ports                      []string
	exit                       *int
	cpu                        float64
	mem                        uint64
}

type stack struct {
	name, compose, env string
	services           []service
}

func exitCode(n int) *int { return &n }

// stacks is the sample data. Statuses follow from the containers like on a real host.
var stacks = []stack{
	{
		name: "nextcloud",
		compose: `services:
  app:
    image: nextcloud:30-apache
    restart: unless-stopped
    ports:
      - "8080:80"
    environment:
      POSTGRES_HOST: db
      POSTGRES_DB: nextcloud
      POSTGRES_USER: nextcloud
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
      REDIS_HOST: redis
    volumes:
      - ./html:/var/www/html
    depends_on: [db, redis]
  db:
    image: postgres:17
    restart: unless-stopped
    environment:
      POSTGRES_DB: nextcloud
      POSTGRES_USER: nextcloud
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
    volumes:
      - ./db:/var/lib/postgresql/data
  redis:
    image: redis:7-alpine
    restart: unless-stopped
`,
		env: "POSTGRES_PASSWORD=demo-not-a-real-secret\n",
		services: []service{
			{name: "app", image: "nextcloud:30-apache", state: "running", ports: []string{"8080:80/tcp"}, cpu: 3.2, mem: 310 << 20},
			{name: "db", image: "postgres:17", state: "running", health: "healthy", cpu: 1.1, mem: 120 << 20},
			{name: "redis", image: "redis:7-alpine", state: "running", cpu: 0.3, mem: 12 << 20},
		},
	},
	{
		name: "jellyfin",
		compose: `services:
  jellyfin:
    image: jellyfin/jellyfin:10.10.3
    restart: unless-stopped
    ports:
      - "8096:8096"
    volumes:
      - ./config:/config
      - /srv/media:/media:ro
`,
		services: []service{
			{name: "jellyfin", image: "jellyfin/jellyfin:10.10.3", state: "running", health: "healthy", ports: []string{"8096:8096/tcp"}, cpu: 7.5, mem: 480 << 20},
		},
	},
	{
		name: "vaultwarden",
		compose: `services:
  vaultwarden:
    image: vaultwarden/server:1.32.5
    restart: unless-stopped
    ports:
      - "127.0.0.1:8081:80"
    environment:
      SIGNUPS_ALLOWED: "false"
    volumes:
      - ./data:/data
`,
		services: []service{
			{name: "vaultwarden", image: "vaultwarden/server:1.32.5", state: "running", health: "healthy", ports: []string{"127.0.0.1:8081:80/tcp"}, cpu: 0.2, mem: 28 << 20},
		},
	},
	{
		name: "immich",
		compose: `services:
  server:
    image: ghcr.io/immich-app/immich-server:v1.120.2
    restart: unless-stopped
    ports:
      - "2283:2283"
    env_file: .env
    depends_on: [database, redis]
  machine-learning:
    image: ghcr.io/immich-app/immich-machine-learning:v1.120.2
    restart: unless-stopped
    env_file: .env
  redis:
    image: redis:7-alpine
    restart: unless-stopped
  database:
    image: tensorchord/pgvecto-rs:pg16-v0.2.0
    restart: unless-stopped
    environment:
      POSTGRES_PASSWORD: ${DB_PASSWORD}
      POSTGRES_USER: postgres
      POSTGRES_DB: immich
`,
		env: "DB_PASSWORD=demo-not-a-real-secret\nUPLOAD_LOCATION=./library\n",
		services: []service{
			{name: "server", image: "ghcr.io/immich-app/immich-server:v1.120.2", state: "running", health: "healthy", ports: []string{"2283:2283/tcp"}, cpu: 4.1, mem: 390 << 20},
			{name: "machine-learning", image: "ghcr.io/immich-app/immich-machine-learning:v1.120.2", state: "restarting", exit: exitCode(137), cpu: 0, mem: 0},
			{name: "redis", image: "redis:7-alpine", state: "running", cpu: 0.2, mem: 9 << 20},
			{name: "database", image: "tensorchord/pgvecto-rs:pg16-v0.2.0", state: "running", health: "healthy", cpu: 0.8, mem: 140 << 20},
		},
	},
	{
		name: "homeassistant",
		compose: `services:
  homeassistant:
    image: ghcr.io/home-assistant/home-assistant:2024.11
    restart: unless-stopped
    network_mode: host
    volumes:
      - ./config:/config
`,
		services: []service{
			{name: "homeassistant", image: "ghcr.io/home-assistant/home-assistant:2024.11", state: "exited", exit: exitCode(0)},
		},
	},
}

// Host is the demo Docker host.
type Host struct {
	started time.Time
}

// NewHost returns the demo host.
func NewHost() *Host { return &Host{started: time.Now()} }

var _ host.Host = (*Host)(nil)
var _ host.ContainerManager = (*Host)(nil)

func find(name string) (stack, bool) {
	for _, s := range stacks {
		if s.name == name {
			return s, true
		}
	}
	return stack{}, false
}

func (h *Host) containers(s stack) []engine.Container {
	out := make([]engine.Container, 0, len(s.services))
	for i, svc := range s.services {
		c := engine.Container{
			ID:      fmt.Sprintf("deadbe%06x", len(s.name)*100+i),
			Name:    s.name + "-" + svc.name + "-1",
			Service: svc.name,
			Image:   svc.image,
			State:   svc.state,
			Health:  svc.health,
			Ports:   svc.ports,
			Created: h.started.Add(-72 * time.Hour).Unix(),
			ExitCode: func() *int {
				if svc.state == "running" {
					return nil
				}
				return svc.exit
			}(),
		}
		switch svc.state {
		case "running":
			c.Status = "Up 3 days"
			if svc.health != "" {
				c.Status += " (" + svc.health + ")"
			}
		case "restarting":
			c.Status = "Restarting (137) 12 seconds ago"
		default:
			c.Status = "Exited (0) 2 hours ago"
		}
		if s.name == "homeassistant" {
			c.HostNetwork = true
		}
		out = append(out, c)
	}
	return out
}

func summary(s stack, cs []engine.Container) engine.StackSummary {
	running, bad := 0, false
	for _, c := range cs {
		if c.State == "running" {
			running++
		}
		if c.State != "running" && c.State != "exited" || c.Health == "unhealthy" {
			bad = true
		}
	}
	st := engine.StatusUp
	switch {
	case running == 0:
		st = engine.StatusStopped
	case running < len(cs) || bad:
		st = engine.StatusDegraded
	}
	return engine.StackSummary{Name: s.name, Managed: true, Status: st, Running: running, Total: len(cs)}
}

// Info describes the demo host.
func (h *Host) Info(context.Context) engine.HostInfo {
	return engine.HostInfo{
		Name: HostName, DockerVersion: "27.3.1", Engine: "docker", EngineVersion: "27.3.1",
		StacksDir: "/opt/stacks", Reachable: true, Locks: engine.HostLocks{ReadOnly: true},
	}
}

// ListStacks returns the sample stacks.
func (h *Host) ListStacks(context.Context) ([]engine.StackSummary, error) {
	out := make([]engine.StackSummary, 0, len(stacks))
	for _, s := range stacks {
		out = append(out, summary(s, h.containers(s)))
	}
	return out, nil
}

// GetStack returns one sample stack.
func (h *Host) GetStack(_ context.Context, name string) (engine.Stack, error) {
	s, ok := find(name)
	if !ok {
		return engine.Stack{}, engine.ErrNotFound
	}
	cs := h.containers(s)
	return engine.Stack{StackSummary: summary(s, cs), ComposeFile: "compose.yaml", Compose: s.compose, Env: s.env, Containers: cs}, nil
}

// Stats returns slowly changing sample numbers.
func (h *Host) Stats(_ context.Context, name string) ([]engine.Stat, error) {
	s, ok := find(name)
	if !ok {
		return nil, engine.ErrNotFound
	}
	wave := 1 + 0.25*math.Sin(float64(time.Since(h.started).Seconds())/20)
	var out []engine.Stat
	for i, c := range h.containers(s) {
		if c.State != "running" {
			continue
		}
		svc := s.services[i]
		out = append(out, engine.Stat{ID: c.ID, CPU: math.Round(svc.cpu*wave*10) / 10, MemUsed: uint64(float64(svc.mem) * (0.95 + 0.05*wave)), MemLimit: 2 << 30})
	}
	return out, nil
}

// Validate accepts nothing: the demo can't save.
func (h *Host) Validate(context.Context, string, string, string) error { return ErrDemo }

// Save is refused.
func (h *Host) Save(context.Context, string, string, string, string, bool) error { return ErrDemo }

// Delete is refused.
func (h *Host) Delete(context.Context, string, string) error { return ErrDemo }

// RunAction is refused: the demo never touches Docker.
func (h *Host) RunAction(context.Context, string, string, string) (engine.OpInfo, error) {
	return engine.OpInfo{}, ErrDemo
}

// FollowOp finds no operations.
func (h *Host) FollowOp(context.Context, string, func(host.OpEvent) error) error {
	return host.ErrOpNotFound
}

var logTemplates = map[string][]string{
	"app":              {`127.0.0.1 - - "GET /status.php HTTP/1.1" 200 1123`, `Cron job ran in 412 ms`, `127.0.0.1 - - "PROPFIND /remote.php/dav/files/demo HTTP/1.1" 207 4021`},
	"db":               {"checkpoint starting: time", "checkpoint complete: wrote 42 buffers (0.3%)"},
	"redis":            {"Background saving started by pid 31", "DB saved on disk"},
	"jellyfin":         {"[INF] Scheduled task Scan Media Library completed", "[INF] Playback started: Big Buck Bunny"},
	"vaultwarden":      {"[INFO] Rocket has launched from http://0.0.0.0:80", "[INFO] GET /alive 200 OK"},
	"server":           {"[Nest] LOG [Microservices:JobService] Thumbnail generation finished", "[Nest] WARN [Api:MachineLearning] Machine learning is unreachable, retrying"},
	"machine-learning": {"Loading clip model 'ViT-B-32__openai'", "Killed"},
	"database":         {"database system is ready to accept connections"},
	"homeassistant":    {"Home Assistant stopped"},
}

func (h *Host) lines(s stack, at time.Time, n int) []engine.LogLine {
	var out []engine.LogLine
	for i := n - 1; i >= 0; i-- {
		for j, svc := range s.services {
			tpl := logTemplates[svc.name]
			if len(tpl) == 0 {
				continue
			}
			stream := "stdout"
			msg := tpl[(i+j)%len(tpl)]
			if strings.Contains(msg, "WARN") || msg == "Killed" {
				stream = "stderr"
			}
			t := at.Add(-time.Duration(i*37+j*5) * time.Second)
			out = append(out, engine.LogLine{Time: t.UTC().Format(time.RFC3339Nano), Container: s.name + "-" + svc.name + "-1", Project: s.name, Service: svc.name, Stream: stream, Message: msg})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out
}

func (h *Host) stacksFor(name string) []stack {
	if name == "" {
		return stacks
	}
	if s, ok := find(name); ok {
		return []stack{s}
	}
	return nil
}

// StreamLogs replays sample lines and adds one every few seconds until ctx ends.
func (h *Host) StreamLogs(ctx context.Context, name, _ string, tail int, emit func(engine.LogLine)) error {
	list := h.stacksFor(name)
	if len(list) == 0 {
		return engine.ErrNotFound
	}
	if tail <= 0 || tail > 50 {
		tail = 20
	}
	for _, s := range list {
		for _, l := range h.lines(s, time.Now(), tail/len(s.services)+1) {
			emit(l)
		}
	}
	t := time.NewTicker(4 * time.Second)
	defer t.Stop()
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return nil
		case now := <-t.C:
			s := list[i%len(list)]
			if ls := h.lines(s, now, 1); len(ls) > 0 {
				emit(ls[i%len(ls)])
			}
		}
	}
}

// WatchLogs never matches: the sample logs hold no alerts.
func (h *Host) WatchLogs(ctx context.Context, _, _ string, _ engine.LogFilter, _ func(engine.LogLine)) error {
	<-ctx.Done()
	return nil
}

// TailLogs returns the last sample lines of a stack.
func (h *Host) TailLogs(_ context.Context, name string, n int) ([]engine.LogLine, error) {
	s, ok := find(name)
	if !ok {
		return nil, engine.ErrNotFound
	}
	if n <= 0 || n > 200 {
		n = 50
	}
	ls := h.lines(s, time.Now(), n/len(s.services)+1)
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return ls, nil
}

func (h *Host) versions(name string) []engine.Version {
	created := h.started.Add(-30 * 24 * time.Hour)
	edited := h.started.Add(-3 * 24 * time.Hour)
	hash := func(n int) string { return fmt.Sprintf("%08x%032x", len(name)*7919+n, n) }
	return []engine.Version{
		{Hash: hash(2), Short: hash(2)[:7], Message: "Edit compose.yaml", Author: "demo", Time: edited, Kind: engine.KindEdit, Files: []string{"compose.yaml"}, Current: true},
		{Hash: hash(1), Short: hash(1)[:7], Message: "Create stack", Author: "demo", Time: created, Kind: engine.KindCreate, Files: []string{"compose.yaml", ".env"}},
	}
}

// History returns two sample versions.
func (h *Host) History(_ context.Context, name string) ([]engine.Version, error) {
	if _, ok := find(name); !ok {
		return nil, engine.ErrNotFound
	}
	return h.versions(name), nil
}

// Version returns a sample version without a diff.
func (h *Host) Version(_ context.Context, name, hash string) (engine.VersionDetail, error) {
	for _, v := range h.versions(name) {
		if v.Hash == hash || v.Short == hash {
			return engine.VersionDetail{Version: v, Diff: []engine.FileDiff{}}, nil
		}
	}
	return engine.VersionDetail{}, engine.ErrNotFound
}

// Restore is refused.
func (h *Host) Restore(context.Context, string, string, string) (*engine.Version, error) {
	return nil, ErrDemo
}

// CheckImages reports every image as up to date.
func (h *Host) CheckImages(_ context.Context, name string) ([]engine.ServiceImage, error) {
	s, ok := find(name)
	if !ok {
		return nil, engine.ErrNotFound
	}
	var out []engine.ServiceImage
	for _, svc := range s.services {
		out = append(out, engine.ServiceImage{Service: svc.name, Ref: svc.image, Status: engine.ImageUpToDate})
	}
	return out, nil
}

// StackImages describes the running images.
func (h *Host) StackImages(_ context.Context, name string) (map[string]engine.RunningImage, error) {
	s, ok := find(name)
	if !ok {
		return nil, engine.ErrNotFound
	}
	out := map[string]engine.RunningImage{}
	for i, svc := range s.services {
		out[svc.name] = engine.RunningImage{Service: svc.name, Ref: svc.image, ImageID: fmt.Sprintf("sha256:%064x", len(s.name)*31+i)}
	}
	return out, nil
}

// RollbackImages is refused.
func (h *Host) RollbackImages(context.Context, string, map[string]engine.RollbackTarget, string) (engine.OpInfo, error) {
	return engine.OpInfo{}, ErrDemo
}

// ChangeImage is refused.
func (h *Host) ChangeImage(context.Context, string, string, string, string, string) (*engine.Version, error) {
	return nil, ErrDemo
}

// Terminal is refused: no shells in the demo.
func (h *Host) Terminal(context.Context, string, string, string, int, int) (engine.TerminalSession, engine.Container, error) {
	return nil, engine.Container{}, ErrDemo
}

// Containers lists every sample container (logs page, container lists).
func (h *Host) Containers(context.Context) ([]engine.ContainerInfo, error) {
	var out []engine.ContainerInfo
	for _, s := range stacks {
		for _, c := range h.containers(s) {
			out = append(out, engine.ContainerInfo{Container: c, Project: s.name, Managed: true, WorkingDir: "/opt/stacks/" + s.name})
		}
	}
	return out, nil
}

// ContainerAction is refused.
func (h *Host) ContainerAction(context.Context, string, string, string) (engine.OpInfo, error) {
	return engine.OpInfo{}, ErrDemo
}

// ExternalStackAction is refused.
func (h *Host) ExternalStackAction(context.Context, string, string, string) (engine.OpInfo, error) {
	return engine.OpInfo{}, ErrDemo
}

// IsDemo reports whether err comes from the demo host.
func IsDemo(err error) bool { return errors.Is(err, ErrDemo) }
