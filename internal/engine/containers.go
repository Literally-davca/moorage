package engine

import (
	"context"
	"net"
	"net/url"
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

	// LabelRole and RoleUpdater mark temporary Moorage helper containers created for
	// container self-updates so stack discovery, stats, logs, and dependencies ignore them.
	LabelRole           = "moorage.role"
	RoleUpdater         = "updater"
	PrevContainerSuffix = "-moorage-prev"
)

// IsInternalContainer reports whether s is a temporary updater helper or a kept
// rollback container created by Moorage self-update.
func IsInternalContainer(s container.Summary) bool {
	if s.Labels[LabelRole] == RoleUpdater {
		return true
	}
	for _, n := range s.Names {
		if strings.HasSuffix(strings.TrimPrefix(n, "/"), PrevContainerSuffix) {
			return true
		}
	}
	return false
}

// Container is the UI view of one container.
type Container struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Service   string   `json:"service"`
	Image     string   `json:"image"`
	ImageID   string   `json:"imageId,omitempty"`
	State     string   `json:"state"`  // running, exited, restarting, paused, created, dead
	Status    string   `json:"status"` // human text from Docker, e.g. "Up 3 hours"
	ExitCode  *int     `json:"exitCode,omitempty"`
	Health    string   `json:"health,omitempty"` // healthy, unhealthy, starting
	Created   int64    `json:"created"`
	Ports     []string `json:"ports,omitempty"`
	URLs      []string `json:"urls,omitempty"`
	ProxyURLs []string `json:"proxyUrls,omitempty"`
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
	c.URLs, c.ProxyURLs = extractContainerURLs(s.Labels)
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
		if IsInternalContainer(s) {
			continue
		}
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

func collectStackURLs(cs []Container) (coreURLs, proxyURLs []string) {
	seen := map[string]bool{}
	for _, c := range cs {
		for _, u := range c.URLs {
			if !seen[u] && len(coreURLs) < 10 {
				seen[u] = true
				coreURLs = append(coreURLs, u)
			}
		}
		for _, u := range c.ProxyURLs {
			if !seen[u] && len(proxyURLs) < 10 {
				seen[u] = true
				proxyURLs = append(proxyURLs, u)
			}
		}
	}
	return coreURLs, proxyURLs
}

const maxContainerURLs = 5

var (
	traefikRuleRe   = regexp.MustCompile(`^traefik\.http\.routers\.([^.]+)\.rule$`)
	traefikHostRe   = regexp.MustCompile("Host\\(([^)]+)\\)")
	traefikPrefixRe = regexp.MustCompile("PathPrefix\\(([^)]+)\\)")
	traefikArgRe    = regexp.MustCompile("`([^`]*)`")
	caddyKeyRe      = regexp.MustCompile(`^caddy(_\d+)?$`)
)

// extractContainerURLs returns deduplicated http/https URLs split into core
// explicit labels (moorage.url) and optional proxy/dashboard labels (homepage, Traefik, Caddy).
func extractContainerURLs(labels map[string]string) (coreURLs, proxyURLs []string) {
	if len(labels) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	add := func(dst *[]string, raw string) {
		if len(*dst) >= maxContainerURLs {
			return
		}
		u := sanitizeServiceURL(raw)
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		*dst = append(*dst, u)
	}

	if v := labels["moorage.url"]; v != "" {
		for _, tok := range splitURLTokens(v) {
			add(&coreURLs, tok)
		}
	}
	if v := labels["homepage.href"]; v != "" {
		add(&proxyURLs, v)
	}

	var traefikKeys []string
	var caddyKeys []string
	for k := range labels {
		if traefikRuleRe.MatchString(k) {
			traefikKeys = append(traefikKeys, k)
		} else if caddyKeyRe.MatchString(k) {
			caddyKeys = append(caddyKeys, k)
		}
	}
	sort.Strings(traefikKeys)
	for _, k := range traefikKeys {
		m := traefikRuleRe.FindStringSubmatch(k)
		router := m[1]
		rule := labels[k]
		if strings.Contains(rule, "HostRegexp(") {
			continue
		}
		var hosts []string
		for _, hm := range traefikHostRe.FindAllStringSubmatch(rule, -1) {
			for _, arg := range traefikArgRe.FindAllStringSubmatch(hm[1], -1) {
				h := strings.TrimSpace(arg[1])
				if h != "" && !strings.Contains(h, "*") {
					hosts = append(hosts, h)
				}
			}
		}
		if len(hosts) == 0 {
			continue
		}
		prefix := ""
		if pm := traefikPrefixRe.FindStringSubmatch(rule); pm != nil {
			if args := traefikArgRe.FindAllStringSubmatch(pm[1], -1); len(args) == 1 {
				p := strings.TrimSpace(args[0][1])
				if strings.HasPrefix(p, "/") && !strings.Contains(p, "*") {
					prefix = p
				}
			}
		}
		tlsExplicit := hasTraefikTLS(labels, router)
		for _, h := range hosts {
			scheme := "http"
			if tlsExplicit || !isLocalOrIPHost(h) {
				scheme = "https"
			}
			add(&proxyURLs, scheme+"://"+h+prefix)
		}
	}

	sort.Strings(caddyKeys)
	for _, k := range caddyKeys {
		for _, tok := range splitURLTokens(labels[k]) {
			if strings.HasPrefix(tok, ":") || strings.ContainsAny(tok, "*{}") {
				continue
			}
			if !strings.Contains(tok, "://") {
				hostOnly := tok
				if idx := strings.IndexByte(hostOnly, '/'); idx >= 0 {
					hostOnly = hostOnly[:idx]
				}
				scheme := "https"
				if isLocalOrIPHost(hostOnly) {
					scheme = "http"
				}
				tok = scheme + "://" + tok
			}
			add(&proxyURLs, tok)
		}
	}
	return coreURLs, proxyURLs
}

func hasTraefikTLS(labels map[string]string, router string) bool {
	prefix := "traefik.http.routers." + router + ".tls"
	if strings.EqualFold(strings.TrimSpace(labels[prefix]), "true") {
		return true
	}
	dotPrefix := prefix + "."
	for k := range labels {
		if strings.HasPrefix(k, dotPrefix) {
			return true
		}
	}
	return false
}

func isLocalOrIPHost(hostport string) bool {
	h := hostport
	if parsed, _, err := net.SplitHostPort(hostport); err == nil {
		h = parsed
	}
	h = strings.Trim(h, "[]")
	if strings.EqualFold(h, "localhost") || strings.HasSuffix(strings.ToLower(h), ".localhost") {
		return true
	}
	if net.ParseIP(h) != nil {
		return true
	}
	return !strings.Contains(h, ".")
}

func splitURLTokens(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}

func sanitizeServiceURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 512 {
		return ""
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Hostname() == "" || u.User != nil {
		return ""
	}
	u.Scheme = scheme
	return u.String()
}
