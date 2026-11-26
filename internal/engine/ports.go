package engine

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
)

// ---- published port strings -------------------------------------------------------

// PortBinding is one published port of a container.
type PortBinding struct {
	IP      string `json:"ip,omitempty"` // "" = every interface
	Public  int    `json:"public"`
	Private int    `json:"private"`
	Proto   string `json:"proto"`
}

// FormatPortString renders a published port as "8080:80/tcp" (every interface),
// "127.0.0.1:8080:80/tcp" or "[fd00::1]:8080:80/tcp".
func FormatPortString(ip string, public, private int, proto string) string {
	if proto == "" {
		proto = "tcp"
	}
	v := strconv.Itoa(public) + ":" + strconv.Itoa(private) + "/" + proto
	ip = normalizeBindIP(ip)
	if ip == "" {
		return v
	}
	if strings.Contains(ip, ":") {
		return "[" + ip + "]:" + v
	}
	return ip + ":" + v
}

// ParsePortString reads the strings FormatPortString writes (and the older "8080:80/tcp").
func ParsePortString(s string) (PortBinding, bool) {
	s = strings.TrimSpace(s)
	base, proto, _ := strings.Cut(s, "/")
	if proto == "" {
		proto = "tcp"
	}
	b := PortBinding{Proto: strings.ToLower(proto)}
	if strings.HasPrefix(base, "[") {
		end := strings.Index(base, "]")
		if end < 0 || len(base) <= end+1 || base[end+1] != ':' {
			return PortBinding{}, false
		}
		b.IP = base[1:end]
		base = base[end+2:]
	} else if strings.Count(base, ":") == 2 {
		ip, rest, _ := strings.Cut(base, ":")
		b.IP, base = ip, rest
	}
	pub, priv, ok := strings.Cut(base, ":")
	if !ok {
		return PortBinding{}, false
	}
	var err error
	if b.Public, err = strconv.Atoi(pub); err != nil || b.Public <= 0 || b.Public > 65535 {
		return PortBinding{}, false
	}
	if b.Private, err = strconv.Atoi(priv); err != nil {
		return PortBinding{}, false
	}
	b.IP = normalizeBindIP(b.IP)
	return b, true
}

// normalizeBindIP maps every "all interfaces" spelling to "".
func normalizeBindIP(ip string) string {
	ip = strings.Trim(strings.TrimSpace(ip), "[]")
	switch ip {
	case "", "0.0.0.0", "::", "*":
		return ""
	}
	if a, err := netip.ParseAddr(ip); err == nil {
		return a.Unmap().String()
	}
	return ip
}

// BindIPsOverlap reports whether two bind addresses would collide on one port: either
// listens on every interface, or both on the same address.
func BindIPsOverlap(a, b string) bool {
	a, b = normalizeBindIP(a), normalizeBindIP(b)
	return a == "" || b == "" || a == b
}

// IsLoopbackIP reports whether ip is 127.0.0.0/8, ::1 or "localhost".
func IsLoopbackIP(ip string) bool {
	ip = normalizeBindIP(ip)
	if ip == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(ip)
	return err == nil && a.IsLoopback()
}

// ---- port map ---------------------------------------------------------------------

// Sources of a port map entry.
const (
	PortSourceStack     = "stack"     // a container of a compose project
	PortSourceContainer = "container" // a standalone container
	PortSourceCompose   = "compose"   // declared in compose.yaml of a managed stack, not running
	PortSourceHost      = "host"      // a process of the host itself (/proc/net)
)

// States of a port map entry.
const (
	PortStateRunning   = "running"
	PortStateReserved  = "reserved" // declared by a stopped stack
	PortStateListening = "listening"
)

// PortEntry is one taken (or reserved) host port.
type PortEntry struct {
	Port      int    `json:"port"`
	Proto     string `json:"proto"`
	IP        string `json:"ip,omitempty"` // bind address, "" = every interface
	Target    int    `json:"target,omitempty"`
	Source    string `json:"source"`
	Stack     string `json:"stack,omitempty"`
	Service   string `json:"service,omitempty"`
	Container string `json:"container,omitempty"`
	State     string `json:"state"`
	Note      string `json:"note,omitempty"`
}

// Owner is a short label of who holds the port ("stack/container", a container or "host").
func (p PortEntry) Owner() string {
	switch {
	case p.Source == PortSourceHost:
		return "a process on the host"
	case p.Stack != "" && p.Container != "":
		return p.Stack + "/" + p.Container
	case p.Stack != "" && p.Service != "":
		return p.Stack + "/" + p.Service
	case p.Stack != "":
		return p.Stack
	}
	return p.Container
}

// Network modes of the Moorage process (PortReport.NetMode, HostInfo.NetMode).
const (
	NetModeNative = "native" // binary on the host
	NetModeHost   = "host"   // container with network_mode: host
	NetModeBridge = "bridge" // container on its own network namespace
)

// PortReport is the port map of a host.
type PortReport struct {
	Ports []PortEntry `json:"ports"`
	// NetMode says what Moorage can see: in bridge mode the host's own listening ports
	// are invisible (Native=false).
	NetMode string `json:"netMode,omitempty"`
	Native  bool   `json:"native"`
	// HostNetwork lists containers with network_mode: host; they have no mappings and
	// their ports are only visible through /proc/net in native/host mode.
	HostNetwork []string `json:"hostNetwork,omitempty"`
}

// DeclaredPort is a host port a compose service publishes.
type DeclaredPort struct {
	Service string
	IP      string
	Port    int
	Target  int
	Proto   string
}

// StackPorts are the declared ports of one managed stack.
type StackPorts struct {
	Stack string
	Ports []DeclaredPort
}

// NativePort is a socket the host listens on (from /proc/net).
type NativePort struct {
	IP    string
	Port  int
	Proto string
}

// DeclaredPorts lists the host ports compose.yaml publishes (port ranges expanded, at
// most 1000 per entry). Unresolvable files yield nothing.
func DeclaredPorts(ctx context.Context, name, compose, env string) []DeclaredPort {
	vars := map[string]string{}
	if strings.TrimSpace(env) != "" {
		if parsed, err := dotenv.UnmarshalBytesWithLookup([]byte(env), nil); err == nil {
			vars = parsed
		}
	}
	dir := filepath.Join(os.TempDir(), "moorage-ports", name)
	proj, err := loader.LoadWithContext(ctx, types.ConfigDetails{
		WorkingDir:  dir,
		ConfigFiles: []types.ConfigFile{{Filename: filepath.Join(dir, "compose.yaml"), Content: []byte(compose)}},
		Environment: vars,
	}, func(o *loader.Options) {
		o.SetProjectName(name, true)
		o.SkipResolveEnvironment = true
		o.SkipInclude = true
	})
	if err != nil {
		return nil
	}
	var out []DeclaredPort
	for _, svc := range proj.ServiceNames() {
		s := proj.Services[svc]
		if s.NetworkMode == "host" {
			continue
		}
		for _, p := range s.Ports {
			lo, hi := publishedRange(p.Published)
			for port := lo; port > 0 && port <= hi; port++ {
				t := int(p.Target)
				if hi > lo {
					t += port - lo
				}
				out = append(out, DeclaredPort{Service: svc, IP: normalizeBindIP(p.HostIP), Port: port, Target: t, Proto: protoOr(p.Protocol)})
			}
		}
	}
	return out
}

func publishedRange(published string) (int, int) {
	lo, hi, isRange := strings.Cut(strings.TrimSpace(published), "-")
	a, err := strconv.Atoi(lo)
	if err != nil || a <= 0 || a > 65535 {
		return 0, -1
	}
	b := a
	if isRange {
		if b, err = strconv.Atoi(hi); err != nil || b < a || b-a > 1000 || b > 65535 {
			return a, a
		}
	}
	return a, b
}

func protoOr(p string) string {
	if p == "" {
		return "tcp"
	}
	return strings.ToLower(p)
}

// BuildPortReport merges containers, declared compose ports and native sockets into one
// port map. A declared port is shown only when no container publishes it; a native socket
// only when no container or declaration explains it (docker-proxy listens on published
// ports in the host namespace).
func BuildPortReport(containers []ContainerInfo, stacks []StackPorts, native []NativePort, netMode string) PortReport {
	r := PortReport{Ports: []PortEntry{}, NetMode: netMode, Native: native != nil}
	type key struct {
		stack string
		port  int
		proto string
	}
	published := map[key]bool{}
	for _, c := range containers {
		if c.HostNetwork {
			r.HostNetwork = append(r.HostNetwork, c.Name)
			continue
		}
		for _, ps := range c.Ports {
			b, ok := ParsePortString(ps)
			if !ok {
				continue
			}
			e := PortEntry{Port: b.Public, Proto: b.Proto, IP: b.IP, Target: b.Private, Container: c.Name, Service: c.Service, State: PortStateRunning}
			if c.State != "running" {
				e.State = c.State
			}
			if c.Project != "" {
				e.Source, e.Stack = PortSourceStack, c.Project
			} else {
				e.Source = PortSourceContainer
			}
			r.Ports = append(r.Ports, e)
			published[key{c.Project, b.Public, b.Proto}] = true
		}
	}
	for _, sp := range stacks {
		for _, d := range sp.Ports {
			if published[key{sp.Stack, d.Port, d.Proto}] {
				continue
			}
			r.Ports = append(r.Ports, PortEntry{Port: d.Port, Proto: d.Proto, IP: d.IP, Target: d.Target, Source: PortSourceCompose,
				Stack: sp.Stack, Service: d.Service, State: PortStateReserved, Note: "not running"})
		}
	}
	if len(native) > 0 {
		seen := map[string]bool{}
		for _, n := range native {
			explained := false
			for _, e := range r.Ports {
				if e.Port == n.Port && e.Proto == n.Proto && BindIPsOverlap(e.IP, n.IP) {
					explained = true
					break
				}
			}
			k := n.Proto + "|" + n.IP + "|" + strconv.Itoa(n.Port)
			if explained || seen[k] {
				continue
			}
			seen[k] = true
			r.Ports = append(r.Ports, PortEntry{Port: n.Port, Proto: n.Proto, IP: n.IP, Source: PortSourceHost, State: PortStateListening})
		}
	}
	sort.SliceStable(r.Ports, func(i, j int) bool {
		a, b := r.Ports[i], r.Ports[j]
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		if a.Proto != b.Proto {
			return a.Proto < b.Proto
		}
		return a.IP < b.IP
	})
	sort.Strings(r.HostNetwork)
	return r
}

// PortReport collects the port map of this host on request (no background loop).
func (e *Engine) PortReport(ctx context.Context) (PortReport, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cs, err := e.AllContainers(ctx)
	if err != nil {
		return PortReport{}, err
	}
	var stacks []StackPorts
	if list, err := e.ListStacks(ctx); err == nil {
		for _, sm := range list {
			dir, derr := e.stackDir(sm.Name)
			if derr != nil {
				continue
			}
			f := composeFile(dir)
			if f == "" {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			env, _ := os.ReadFile(filepath.Join(dir, ".env"))
			if ps := DeclaredPorts(ctx, sm.Name, string(b), string(env)); len(ps) > 0 {
				stacks = append(stacks, StackPorts{Stack: sm.Name, Ports: ps})
			}
		}
	}
	mode := e.NetMode()
	var native []NativePort
	if mode == NetModeNative || mode == NetModeHost {
		native = ReadNativePorts()
	}
	return BuildPortReport(cs, stacks, native, mode), nil
}
