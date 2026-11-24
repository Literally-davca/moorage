// Package lint checks compose.yaml and .env before a save or a deploy and offers quick
// fixes. It is pure (no Docker, no files): the hub runs it for every host, and the engine
// runs the blocking part again when it saves, so an agent refuses a broken file too.
//
// Severities:
//   - error: blocks Save and Deploy (broken YAML or schema, secrets written into commands…)
//   - deploy_error: the file may be saved as work in progress but not deployed (a required
//     variable is missing, a host port is taken by a running container)
//   - warning and info: advice, most with a one-click fix
package lint

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"go.yaml.in/yaml/v4"
)

// Severities.
const (
	Error       = "error"
	DeployError = "deploy_error"
	Warning     = "warning"
	Info        = "info"
)

// MaxFileSize mirrors engine.MaxFileSize.
const MaxFileSize = 512 * 1024

// Issue is one finding.
type Issue struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Service  string `json:"service,omitempty"`
	Line     int    `json:"line,omitempty"` // 1-based line in compose.yaml, 0 = unknown
	Message  string `json:"message"`
	Fix      *Fix   `json:"fix,omitempty"`
}

// Fix is a quick fix the UI can apply with POST .../lint/fix.
type Fix struct {
	Kind    string   `json:"kind"` // bind-localhost, add-restart, remove-version, extract-secret, remove-unused, move-env
	Label   string   `json:"label"`
	Service string   `json:"service,omitempty"`
	Line    int      `json:"line,omitempty"`
	Keys    []string `json:"keys,omitempty"` // remove-unused
}

// PortUse is a host port a running container (outside the linted stack) publishes.
type PortUse struct {
	IP    string `json:"ip"`
	Port  int    `json:"port"`
	Proto string `json:"proto"`
	Owner string `json:"owner"` // "stack/container" or container name
}

// Options tune a run.
type Options struct {
	// Name is the project name (for the compose loader).
	Name string
	// Running are host ports taken by other running containers (deploy-time check).
	Running []PortUse
}

// Blocking reports whether issues stop a save (deploy=false) or a deploy (deploy=true).
func Blocking(issues []Issue, deploy bool) *Issue {
	for i := range issues {
		if issues[i].Severity == Error || (deploy && issues[i].Severity == DeployError) {
			return &issues[i]
		}
	}
	return nil
}

// HasErrors reports whether any issue blocks a save.
func HasErrors(issues []Issue) bool { return Blocking(issues, false) != nil }

// dbPorts are container ports of databases and caches that shouldn't face the network.
var dbPorts = map[int]string{5432: "PostgreSQL", 3306: "MySQL/MariaDB", 6379: "Redis/Valkey", 27017: "MongoDB", 9200: "Elasticsearch", 11211: "Memcached"}

// Lint checks compose and env. It never fails: problems are issues.
func Lint(ctx context.Context, compose, env string, o Options) []Issue {
	var out []Issue
	add := func(i Issue) { out = append(out, i) }
	switch {
	case len(compose) > MaxFileSize || len(env) > MaxFileSize:
		return []Issue{{Severity: Error, Code: "too-large", Message: "File is larger than 512 KB."}}
	case strings.TrimSpace(compose) == "":
		return []Issue{{Severity: Error, Code: "empty", Message: "Compose file is empty."}}
	}
	vars, err := dotenv.UnmarshalBytesWithLookup([]byte(env), nil)
	if err != nil {
		add(Issue{Severity: Error, Code: "env-syntax", Message: ".env: " + err.Error()})
		vars = map[string]string{}
	}
	doc, err := parseNode(compose)
	if err != nil {
		return append(out, Issue{Severity: Error, Code: "yaml", Message: "compose.yaml: " + cleanYAMLErr(err)})
	}
	proj, err := load(ctx, o.Name, compose, vars)
	if err != nil {
		add(Issue{Severity: Error, Code: "schema", Message: FriendlyComposeError(err.Error())})
	}
	svcNodes := serviceNodes(doc)

	// ---- structural rules on the raw document (work even when the loader failed) ----
	if v := mapGet(doc, "version"); v != nil {
		add(Issue{Severity: Info, Code: "version", Line: v.key.Line, Message: "The top-level version: key is obsolete; Compose ignores it.",
			Fix: &Fix{Kind: "remove-version", Label: "Remove version:", Line: v.key.Line}})
	}
	for _, sn := range svcNodes {
		svc := sn.name
		if mapGet(sn.node, "image") == nil && mapGet(sn.node, "build") == nil && mapGet(sn.node, "extends") == nil {
			add(Issue{Severity: Error, Code: "no-image", Service: svc, Line: sn.line, Message: fmt.Sprintf("Service %s has neither image nor build.", svc)})
		}
		if mapGet(sn.node, "restart") == nil && mapGet(sn.node, "deploy") == nil {
			add(Issue{Severity: Info, Code: "no-restart", Service: svc, Line: sn.line, Message: fmt.Sprintf("Service %s has no restart policy: it stays down after a crash or reboot.", svc),
				Fix: &Fix{Kind: "add-restart", Label: "Add restart: unless-stopped", Service: svc, Line: sn.line}})
		}
		for _, field := range []string{"command", "entrypoint"} {
			if f := mapGet(sn.node, field); f != nil {
				for _, s := range scalars(f.val) {
					if secret := findSecret(s.Value); secret != "" {
						add(Issue{Severity: Error, Code: "secret-in-command", Service: svc, Line: s.Line,
							Message: fmt.Sprintf("Service %s has a password or token written into %s. Move it to .env.", svc, field),
							Fix:     &Fix{Kind: "extract-secret", Label: "Move to .env as ${" + secretVarName(svc) + "}", Service: svc, Line: s.Line}})
					}
				}
			}
		}
		if hc := mapGet(sn.node, "healthcheck"); hc != nil {
			if t := mapGet(hc.val, "test"); t != nil {
				for _, s := range scalars(t.val) {
					if findSecret(s.Value) != "" {
						add(Issue{Severity: Error, Code: "secret-in-command", Service: svc, Line: s.Line,
							Message: fmt.Sprintf("Service %s has a password or token written into its healthcheck. Use $$VAR from the environment instead.", svc)})
					}
				}
			}
		}
	}

	// ---- variables ----
	refs := References(compose)
	if !refs.EnvFile {
		var unused []string
		for _, k := range envKeys(env) {
			if !refs.All[k] {
				unused = append(unused, k)
			}
		}
		if len(unused) > 0 {
			add(Issue{Severity: Info, Code: "unused-env", Message: fmt.Sprintf("%d unused %s in .env: %s", len(unused), plural(len(unused), "variable", "variables"), strings.Join(unused, ", ")),
				Fix: &Fix{Kind: "remove-unused", Label: "Remove from .env", Keys: unused}})
		}
	}
	var missing []string
	for k := range refs.Required {
		if strings.TrimSpace(vars[k]) == "" {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	for _, k := range missing {
		add(Issue{Severity: DeployError, Code: "missing-var", Message: fmt.Sprintf("${%s} has no default and isn't set in .env.", k)})
	}

	if proj == nil {
		return out
	}

	// ---- rules on the resolved project ----
	type pk struct {
		ip, proto string
		port      int
	}
	seen := map[pk]string{}
	names := make([]string, 0, len(proj.Services))
	for n := range proj.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	line := func(svc string) int {
		for _, sn := range svcNodes {
			if sn.name == svc {
				return sn.line
			}
		}
		return 0
	}
	for _, name := range names {
		s := proj.Services[name]
		ln := line(name)
		for _, p := range s.Ports {
			if p.Published == "" {
				continue
			}
			lo, hi := portRange(p.Published)
			for port := lo; port <= hi && port > 0; port++ {
				ip := p.HostIP
				k := pk{ip, protoOf(p.Protocol), port}
				if other, dup := seen[k]; dup {
					add(Issue{Severity: Error, Code: "duplicate-port", Service: name, Line: ln,
						Message: fmt.Sprintf("Host port %d/%s is published by both %s and %s.", port, k.proto, other, name)})
				} else {
					seen[k] = name
				}
				for _, r := range o.Running {
					if r.Port == port && protoOf(r.Proto) == k.proto && ipsOverlap(r.IP, ip) {
						add(Issue{Severity: DeployError, Code: "port-taken", Service: name, Line: ln,
							Message: fmt.Sprintf("Host port %d/%s is already used by %s.", port, k.proto, r.Owner)})
					}
				}
			}
			if what, isDB := dbPorts[int(p.Target)]; isDB && !isLoopback(p.HostIP) {
				add(Issue{Severity: Warning, Code: "db-port-public", Service: name, Line: ln,
					Message: fmt.Sprintf("%s port %d of %s is published on every interface. Bind it to 127.0.0.1 unless other machines need it.", what, p.Target, name),
					Fix:     &Fix{Kind: "bind-localhost", Label: "Bind to 127.0.0.1", Service: name, Line: ln}})
			}
		}
		if s.Privileged {
			add(Issue{Severity: Warning, Code: "privileged", Service: name, Line: ln, Message: fmt.Sprintf("%s runs privileged: it has full access to the host.", name)})
		}
		if s.NetworkMode == "host" {
			add(Issue{Severity: Warning, Code: "network-host", Service: name, Line: ln, Message: fmt.Sprintf("%s uses network_mode: host and bypasses container network isolation.", name)})
		}
		if s.Pid == "host" {
			add(Issue{Severity: Warning, Code: "pid-host", Service: name, Line: ln, Message: fmt.Sprintf("%s shares the host's process namespace (pid: host).", name)})
		}
		for _, v := range s.Volumes {
			if v.Type == types.VolumeTypeBind && (v.Source == "/var/run/docker.sock" || v.Source == "/run/docker.sock") {
				add(Issue{Severity: Warning, Code: "docker-sock", Service: name, Line: ln,
					Message: fmt.Sprintf("%s mounts the Docker socket: whoever controls it controls the host. Consider a socket proxy.", name)})
			}
		}
		if s.Image != "" && s.Build == nil {
			if tag := imageTag(s.Image); tag == "" || tag == "latest" {
				add(Issue{Severity: Info, Code: "latest-tag", Service: name, Line: ln, Message: fmt.Sprintf("%s uses a moving tag (%s): pin a version so updates are deliberate.", name, s.Image)})
			}
		}
		if s.MemLimit == 0 && (s.Deploy == nil || s.Deploy.Resources.Limits == nil || s.Deploy.Resources.Limits.MemoryBytes == 0) {
			add(Issue{Severity: Info, Code: "no-mem-limit", Service: name, Line: ln, Message: fmt.Sprintf("%s has no memory limit.", name)})
		}
		if s.HealthCheck == nil {
			add(Issue{Severity: Info, Code: "no-healthcheck", Service: name, Line: ln, Message: fmt.Sprintf("%s has no healthcheck in compose.yaml (the image may still define one).", name)})
		}
	}
	return out
}

// ---- compose loading -----------------------------------------------------------

func load(ctx context.Context, name, compose string, vars map[string]string) (*types.Project, error) {
	if name == "" {
		name = "lint"
	}
	dir := filepath.Join(os.TempDir(), "moorage-lint", name)
	details := types.ConfigDetails{
		WorkingDir:  dir,
		ConfigFiles: []types.ConfigFile{{Filename: filepath.Join(dir, "compose.yaml"), Content: []byte(compose)}},
		Environment: vars,
	}
	return loader.LoadWithContext(ctx, details, func(o *loader.Options) {
		o.SetProjectName(name, true)
		o.SkipResolveEnvironment = true
	})
}

var (
	validatingPrefix = regexp.MustCompile(`^validating [^:]+: `)
	falseSchema      = regexp.MustCompile(`^(\S+) false schema$`)
	additionalProp   = regexp.MustCompile(`^(\S+) additional properties? '([^']+)' not allowed$`)
)

// FriendlyComposeError hides host paths and rephrases the most common schema errors.
func FriendlyComposeError(msg string) string {
	msg = validatingPrefix.ReplaceAllString(msg, "")
	if m := falseSchema.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("%s isn't a known compose option. Check the spelling.", m[1])
	}
	if m := additionalProp.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("%s.%s isn't a known compose option. Check the spelling.", m[1], m[2])
	}
	return msg
}

func cleanYAMLErr(err error) string {
	return strings.TrimPrefix(err.Error(), "yaml: ")
}

// ---- variables --------------------------------------------------------------------

// Refs are the variables compose.yaml interpolates.
type Refs struct {
	All      map[string]bool
	Required map[string]bool // no default (${X} or $X, not ${X:-d} / ${X-d} / ${X:+v})
	EnvFile  bool            // the file loads .env into containers (env_file: .env)
}

var varRe = regexp.MustCompile(`\$(?:\{([A-Za-z_][A-Za-z0-9_]*)([^}]*)\}|([A-Za-z_][A-Za-z0-9_]*))`)
var envFileRe = regexp.MustCompile(`(?m)env_file\s*:[\s\S]*?(^|[\s'"/-])\.env(['"\s]|$)`)

// References lists the variables of compose, skipping $$ escapes and comment lines.
func References(compose string) Refs {
	r := Refs{All: map[string]bool{}, Required: map[string]bool{}}
	for _, l := range strings.Split(compose, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "#") {
			continue
		}
		l = strings.ReplaceAll(l, "$$", "")
		for _, m := range varRe.FindAllStringSubmatch(l, -1) {
			name := m[1]
			if name == "" {
				name = m[3]
			}
			r.All[name] = true
			mod := m[2]
			if !(strings.HasPrefix(mod, ":-") || strings.HasPrefix(mod, "-") || strings.HasPrefix(mod, ":+") || strings.HasPrefix(mod, "+")) {
				r.Required[name] = true
			}
		}
	}
	r.EnvFile = envFileRe.MatchString(compose)
	return r
}

// ---- secrets ----------------------------------------------------------------------

var (
	credURLRe  = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s/:@'"]+:([^\s/@'"]+)@`)
	secretArgs = regexp.MustCompile(`(?i)(?:--?(?:password|passwd|pass|token|secret|api[-_]?key|auth[-_]?token)[= ]|\b[A-Z0-9_]*(?:PASSWORD|PASSWD|SECRET|TOKEN|API_KEY)=)("[^"]+"|'[^']+'|[^\s'"]+)`)
)

// findSecret returns a literal secret written into s (a command), "" when none. Values
// taken from the environment ($VAR, ${VAR}, $$VAR) don't count.
func findSecret(s string) string {
	for _, m := range credURLRe.FindAllStringSubmatch(s, -1) {
		if !strings.HasPrefix(m[1], "$") {
			return m[1]
		}
	}
	for _, m := range secretArgs.FindAllStringSubmatch(s, -1) {
		v := strings.Trim(m[1], `"'`)
		if v != "" && !strings.HasPrefix(v, "$") && len(v) >= 3 {
			return v
		}
	}
	return ""
}

func secretVarName(svc string) string { return envName(svc) + "_SECRET" }

// envName turns a service name into an env prefix: web-app -> WEB_APP.
func envName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" || (out[0] >= '0' && out[0] <= '9') {
		out = "S_" + out
	}
	return out
}

// ---- ports ------------------------------------------------------------------------

func portRange(published string) (int, int) {
	lo, hi, isRange := strings.Cut(published, "-")
	a, err := strconv.Atoi(lo)
	if err != nil {
		return 0, -1
	}
	b := a
	if isRange {
		if b, err = strconv.Atoi(hi); err != nil || b < a || b-a > 1000 {
			return a, a
		}
	}
	return a, b
}

func protoOf(p string) string {
	if p == "" {
		return "tcp"
	}
	return strings.ToLower(p)
}

func isLoopback(ip string) bool {
	return ip == "127.0.0.1" || ip == "::1" || strings.HasPrefix(ip, "127.") || ip == "localhost"
}

// ipsOverlap: two bindings collide when either listens on every interface or both on one.
func ipsOverlap(a, b string) bool {
	any := func(s string) bool { return s == "" || s == "0.0.0.0" || s == "::" || s == "[::]" }
	return any(a) || any(b) || a == b
}

func imageTag(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		return "digest"
	}
	last := ref[strings.LastIndex(ref, "/")+1:]
	if i := strings.LastIndex(last, ":"); i >= 0 {
		return last[i+1:]
	}
	return ""
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ---- YAML nodes ---------------------------------------------------------------------

type kv struct{ key, val *yaml.Node }

type svcNode struct {
	name string
	line int
	key  *yaml.Node
	node *yaml.Node
}

func parseNode(compose string) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(compose), &doc); err != nil {
		return nil, err
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0], nil
	}
	return &yaml.Node{Kind: yaml.MappingNode}, nil
}

func mapGet(m *yaml.Node, key string) *kv {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return &kv{m.Content[i], m.Content[i+1]}
		}
	}
	return nil
}

func serviceNodes(doc *yaml.Node) []svcNode {
	s := mapGet(doc, "services")
	if s == nil || s.val.Kind != yaml.MappingNode {
		return nil
	}
	var out []svcNode
	for i := 0; i+1 < len(s.val.Content); i += 2 {
		k, v := s.val.Content[i], s.val.Content[i+1]
		if v.Kind == yaml.AliasNode {
			v = v.Alias
		}
		out = append(out, svcNode{name: k.Value, line: k.Line, key: k, node: v})
	}
	return out
}

// scalars returns the scalar nodes of a scalar or a sequence of scalars.
func scalars(n *yaml.Node) []*yaml.Node {
	switch n.Kind {
	case yaml.ScalarNode:
		return []*yaml.Node{n}
	case yaml.SequenceNode:
		var out []*yaml.Node
		for _, c := range n.Content {
			if c.Kind == yaml.ScalarNode {
				out = append(out, c)
			}
		}
		return out
	}
	return nil
}

func envKeys(env string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range strings.Split(strings.ReplaceAll(env, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		t = strings.TrimPrefix(t, "export ")
		k, _, ok := strings.Cut(t, "=")
		k = strings.TrimSpace(k)
		if ok && k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}
