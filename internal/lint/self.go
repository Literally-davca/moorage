package lint

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

// SelfLint turns on the rules for the stack Moorage itself runs from (M19B, M19D).
type SelfLint struct {
	// Service is the compose service of the Moorage container.
	Service string
	// OwnPorts are host ports the running Moorage container holds now; they are free
	// once compose recreates it.
	OwnPorts []int
	// HostNetUnverifiable: the apply helper can't run in the host's network (Docker
	// Desktop, rootless), so network_mode: host may leave Moorage unreachable.
	HostNetUnverifiable bool
}

// SelfListen is one address the Moorage service listens on in host network mode.
type SelfListen struct {
	Env  string // MOORAGE_LISTEN or MOORAGE_AGENT_LISTEN
	IP   string // "" = every address
	Port int
}

// ParseListen checks a listen address: "host:port", ":port" or "off". ok=false means
// a bad value; fix is the corrected form when it's obvious (a bare port).
func ParseListen(v string) (ip string, port int, off, ok bool, fix string) {
	v = strings.TrimSpace(v)
	if strings.EqualFold(v, "off") {
		return "", 0, true, true, ""
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
		return "", 0, false, false, ":" + v
	}
	h, p, err := net.SplitHostPort(v)
	if err != nil {
		return "", 0, false, false, ""
	}
	n, err := strconv.Atoi(p)
	if err != nil || n <= 0 || n > 65535 {
		return "", 0, false, false, ""
	}
	if h == "0.0.0.0" || h == "::" {
		h = ""
	}
	return h, n, false, true, ""
}

// SelfListens lists the listen addresses of the Moorage service as the new
// configuration defines them, and issues for values that would crash it at start.
func SelfListens(s types.ServiceConfig) ([]SelfListen, []Issue) {
	get := func(k string) (string, bool) {
		v, ok := s.Environment[k]
		if !ok || v == nil {
			return "", false
		}
		return *v, true
	}
	hub := true
	if v, ok := get("MOORAGE_HUB"); ok && strings.TrimSpace(v) != "" {
		hub = false // an agent dials out; it serves no web UI or agent port
	}
	var out []SelfListen
	var issues []Issue
	check := func(env, def string, use bool) {
		v, set := get(env)
		if !set || strings.TrimSpace(v) == "" {
			if !use || def == "" {
				return
			}
			v = def
		}
		ip, port, off, ok, fix := ParseListen(v)
		if !ok {
			msg := fmt.Sprintf("%s=%q isn't a listen address: use host:port or :port", env, v)
			if fix != "" {
				msg = fmt.Sprintf("%s=%q is missing the colon: use %q (all addresses) or \"127.0.0.1%s\". Moorage would keep restarting with \"missing port in address\".", env, v, fix, fix)
			}
			issues = append(issues, Issue{Severity: DeployError, Code: "self-listen-invalid", Service: s.Name, Message: msg + " See docs/troubleshooting.md."})
			return
		}
		if off || !use {
			return
		}
		out = append(out, SelfListen{Env: env, IP: ip, Port: port})
	}
	webOn := true
	if v, ok := get("MOORAGE_WEB_UI"); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			webOn = b
		}
	}
	_, listenSet := get("MOORAGE_LISTEN")
	webDefault := ""
	if webOn && hub {
		webDefault = ":5080"
	}
	check("MOORAGE_LISTEN", webDefault, hub && webOn && (listenSet || webDefault != ""))
	check("MOORAGE_AGENT_LISTEN", ":5081", hub)
	return out, issues
}

// selfIssues are the self-stack rules on the resolved project.
func selfIssues(proj *types.Project, o Options, line func(string) int, add func(Issue)) {
	if o.Self == nil {
		return
	}
	s, ok := proj.Services[o.Self.Service]
	if !ok {
		return
	}
	ln := line(s.Name)
	listens, issues := SelfListens(s)
	for _, is := range issues {
		is.Line = ln
		add(is)
	}
	if s.NetworkMode != "host" {
		return
	}
	if o.Self.HostNetUnverifiable {
		add(Issue{Severity: Warning, Code: "self-host-network-unverified", Service: s.Name, Line: ln,
			Message: "network_mode: host on Docker Desktop or rootless Docker/Podman: host networking is often off there, and Moorage may not be reachable from this computer after the restart."})
	}
	own := map[int]bool{}
	for _, p := range o.Self.OwnPorts {
		own[p] = true
	}
	for _, l := range listens {
		if own[l.Port] {
			continue
		}
		var clash []string
		for _, r := range o.Running {
			if r.Port != l.Port || protoOf(r.Proto) != "tcp" || r.Reserved || !ipsOverlap(r.IP, l.IP) {
				continue
			}
			who := r.Owner
			if r.IP != "" {
				who += " on " + r.IP
			}
			clash = append(clash, who)
		}
		if len(clash) == 0 {
			continue
		}
		msg := fmt.Sprintf("Port %d on the host is in use by %s, so Moorage (%s, network_mode: host) couldn't start.", l.Port, strings.Join(dedupe(clash), ", "), l.Env)
		onlySpecific := true
		for _, r := range o.Running {
			if r.Port == l.Port && protoOf(r.Proto) == "tcp" && !r.Reserved && r.IP == "" {
				onlySpecific = false
			}
		}
		if l.IP == "" && onlySpecific {
			msg += fmt.Sprintf(" The port is taken only on other addresses (for example by tailscale serve): set %s=127.0.0.1:%d.", l.Env, l.Port)
		}
		add(Issue{Severity: DeployError, Code: "self-port-taken", Service: s.Name, Line: ln, Message: msg + " See docs/troubleshooting.md."})
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
