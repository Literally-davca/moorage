package engine

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

const (
	labelProject    = "com.docker.compose.project"
	labelService    = "com.docker.compose.service"
	labelWorkingDir = "com.docker.compose.project.working_dir"
)

// Container is the UI view of one container.
type Container struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Service  string   `json:"service"`
	Image    string   `json:"image"`
	ImageID  string   `json:"imageId,omitempty"`
	State    string   `json:"state"`  // running, exited, restarting, paused, created, dead
	Status   string   `json:"status"` // human text from Docker, e.g. "Up 3 hours"
	ExitCode *int     `json:"exitCode,omitempty"`
	Health   string   `json:"health,omitempty"` // healthy, unhealthy, starting
	Created  int64    `json:"created"`
	Ports    []string `json:"ports,omitempty"`
}

var exitRe = regexp.MustCompile(`^Exited \((-?\d+)\)`)

func toContainer(s container.Summary) Container {
	c := Container{
		ID:      s.ID,
		Service: s.Labels[labelService],
		Image:   s.Image,
		ImageID: s.ImageID,
		State:   normalizeState(string(s.State)),
		Status:  s.Status,
		Created: s.Created,
	}
	if len(s.Names) > 0 {
		c.Name = strings.TrimPrefix(s.Names[0], "/")
	}
	if m := exitRe.FindStringSubmatch(s.Status); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			c.ExitCode = &n
		}
	}
	if s.Health != nil {
		c.Health = string(s.Health.Status)
	}
	if c.Health == "" {
		c.Health = healthFromStatus(s.Status)
	}
	seen := map[string]bool{}
	for _, p := range s.Ports {
		if p.PublicPort == 0 {
			continue
		}
		v := strconv.Itoa(int(p.PublicPort)) + ":" + strconv.Itoa(int(p.PrivatePort)) + "/" + string(p.Type)
		if !seen[v] {
			seen[v] = true
			c.Ports = append(c.Ports, v)
		}
	}
	return c
}

// projectContainers returns all compose containers grouped by project name,
// plus each project's working directory label.
func (e *Engine) projectContainers(ctx context.Context, project string) (map[string][]Container, map[string]string, error) {
	f := make(client.Filters)
	if project == "" {
		f.Add("label", labelProject)
	} else {
		f.Add("label", labelProject+"="+project)
	}
	res, err := e.api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: f})
	if err != nil {
		return nil, nil, err
	}
	byProject := map[string][]Container{}
	dirs := map[string]string{}
	for _, s := range res.Items {
		p := s.Labels[labelProject]
		byProject[p] = append(byProject[p], toContainer(s))
		if d := s.Labels[labelWorkingDir]; d != "" {
			dirs[p] = d
		}
	}
	podman := e.isPodman(ctx)
	for _, cs := range byProject {
		sort.Slice(cs, func(i, j int) bool { return cs[i].Name < cs[j].Name })
		if podman {
			e.podmanHealth(ctx, cs)
		}
	}
	return byProject, dirs, nil
}

// Status values shown in the UI.
const (
	StatusUp       = "up"       // every container running (and not unhealthy)
	StatusDegraded = "degraded" // some running, or running but unhealthy
	StatusDown     = "down"     // nothing running and at least one exited with an error
	StatusStopped  = "stopped"  // nothing running, stopped cleanly
	StatusInactive = "inactive" // no containers (never deployed or removed with Down)
)

func stackStatus(cs []Container) (status string, running int) {
	if len(cs) == 0 {
		return StatusInactive, 0
	}
	unhealthy, failed := false, false
	for _, c := range cs {
		switch c.State {
		case "running":
			running++
			if c.Health == "unhealthy" {
				unhealthy = true
			}
		case "restarting", "dead":
			failed = true
		case "exited":
			if c.ExitCode != nil && *c.ExitCode != 0 {
				failed = true
			}
		}
	}
	switch {
	case running == len(cs) && !unhealthy:
		return StatusUp, running
	case running > 0:
		return StatusDegraded, running
	case failed:
		return StatusDown, 0
	default:
		return StatusStopped, 0
	}
}
