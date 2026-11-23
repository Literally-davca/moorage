package engine

import (
	"context"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
)

// DepFacts is what an agent knows about the wiring between its stacks, read from the
// compose model. The hub turns these facts into dependency edges. Environment values
// never leave the agent: only the host names found in them are reported.
type DepFacts struct {
	Stacks []StackFacts `json:"stacks"`
}

// StackFacts describes one managed stack.
type StackFacts struct {
	Name     string        `json:"name"`
	Error    string        `json:"error,omitempty"` // the compose file couldn't be loaded
	Networks []NetworkFact `json:"networks,omitempty"`
	Services []ServiceFact `json:"services,omitempty"`
}

// NetworkFact is a top-level network of a stack with its real Docker name.
type NetworkFact struct {
	Name     string `json:"name"`
	External bool   `json:"external,omitempty"`
}

// ServiceFact lists the names a service answers to and the host names it refers to.
type ServiceFact struct {
	Name          string   `json:"name"`
	ContainerName string   `json:"containerName,omitempty"`
	Networks      []string `json:"networks,omitempty"` // real Docker network names
	Hosts         []string `json:"hosts,omitempty"`    // from links, external_links and env values
}

// Limits keep the reply small even for odd compose files.
const (
	maxDepHostsPerService = 50
	maxDepHostLen         = 253
)

// DepFacts loads every managed stack's compose model. A stack whose file doesn't load is
// reported with its error, so the hub can tell it apart from a stack without wiring.
func (e *Engine) DepFacts(ctx context.Context) (DepFacts, error) {
	entries, err := os.ReadDir(e.cfg.StacksDir)
	if err != nil {
		return DepFacts{}, err
	}
	out := DepFacts{Stacks: []StackFacts{}}
	for _, ent := range entries {
		if ctx.Err() != nil {
			return DepFacts{}, ctx.Err()
		}
		if !ent.IsDir() || !nameRe.MatchString(ent.Name()) {
			continue
		}
		dir := filepath.Join(e.cfg.StacksDir, ent.Name())
		cf := composeFile(dir)
		if cf == "" {
			continue
		}
		out.Stacks = append(out.Stacks, stackFactsFromDir(ctx, ent.Name(), dir, cf))
	}
	return out, nil
}

func stackFactsFromDir(ctx context.Context, name, dir, cf string) StackFacts {
	b, err := os.ReadFile(cf)
	if err != nil {
		return StackFacts{Name: name, Error: "compose file unreadable"}
	}
	env := ""
	if eb, err := os.ReadFile(filepath.Join(dir, ".env")); err == nil {
		env = string(eb)
	}
	sf, err := ComposeFacts(ctx, name, dir, string(b), env)
	if err != nil {
		return StackFacts{Name: name, Error: friendlyComposeError(err.Error())}
	}
	return sf
}

// ComposeFacts parses one compose file (with its .env) into dependency facts. dir is the
// working directory for includes; it may be empty in tests.
func ComposeFacts(ctx context.Context, name, dir, compose, env string) (StackFacts, error) {
	vars := map[string]string{}
	if strings.TrimSpace(env) != "" {
		if parsed, err := dotenv.UnmarshalBytesWithLookup([]byte(env), nil); err == nil {
			vars = parsed
		}
	}
	filename := "compose.yaml"
	if dir != "" {
		filename = filepath.Join(dir, filename)
	}
	proj, err := loader.LoadWithContext(ctx, types.ConfigDetails{
		WorkingDir:  dir,
		ConfigFiles: []types.ConfigFile{{Filename: filename, Content: []byte(compose)}},
		Environment: vars,
	}, func(o *loader.Options) {
		o.SetProjectName(name, true)
		// env_file is never read here: only the compose model itself is used.
		o.SkipResolveEnvironment = true
	})
	if err != nil {
		return StackFacts{}, err
	}
	return projectFacts(name, proj), nil
}

func projectFacts(name string, proj *types.Project) StackFacts {
	sf := StackFacts{Name: name}
	realName := map[string]string{} // network key -> Docker name
	for key, n := range proj.Networks {
		nm := n.Name
		if nm == "" {
			nm = key
			if !n.External {
				nm = name + "_" + key
			}
		}
		realName[key] = nm
		sf.Networks = append(sf.Networks, NetworkFact{Name: nm, External: bool(n.External)})
	}
	sort.Slice(sf.Networks, func(i, j int) bool { return sf.Networks[i].Name < sf.Networks[j].Name })
	for svcName, s := range proj.Services {
		f := ServiceFact{Name: svcName, ContainerName: s.ContainerName}
		for key := range s.Networks {
			if nm, ok := realName[key]; ok {
				f.Networks = append(f.Networks, nm)
			}
		}
		sort.Strings(f.Networks)
		f.Hosts = serviceHosts(s)
		sf.Services = append(sf.Services, f)
	}
	sort.Slice(sf.Services, func(i, j int) bool { return sf.Services[i].Name < sf.Services[j].Name })
	return sf
}

// serviceHosts collects the host names a service refers to: link targets and hosts in
// env values that look like host:port or a URL. depends_on is left out on purpose: it
// only names services of the same stack.
func serviceHosts(s types.ServiceConfig) []string {
	seen := map[string]bool{}
	add := func(h string) {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" || len(h) > maxDepHostLen || seen[h] || len(seen) >= maxDepHostsPerService || !plausibleHost(h) {
			return
		}
		seen[h] = true
	}
	for _, l := range append(append([]string{}, s.Links...), s.ExternalLinks...) {
		target, _, _ := strings.Cut(l, ":")
		add(target)
	}
	keys := make([]string, 0, len(s.Environment))
	for k := range s.Environment {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic when the cap is reached
	for _, k := range keys {
		if v := s.Environment[k]; v != nil {
			for _, h := range HostsInValue(*v) {
				add(h)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

var hostPortRe = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9_.-]*):([0-9]{1,5})$`)

// HostsInValue returns host names from an env value that is a URL (scheme://host…) or a
// host:port pair, or a list of them separated by commas or spaces. Anything else (plain
// words, paths, IP addresses) is ignored, to keep detection conservative.
func HostsInValue(v string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		if strings.Contains(part, "://") {
			u, err := url.Parse(part)
			if err != nil || u.Host == "" {
				continue
			}
			if h := u.Hostname(); h != "" {
				out = append(out, h)
			}
			continue
		}
		if m := hostPortRe.FindStringSubmatch(part); m != nil {
			if p, err := strconv.Atoi(m[2]); err == nil && p > 0 && p < 65536 {
				out = append(out, m[1])
			}
		}
	}
	return out
}

// plausibleHost drops IPs and names that never point at another container.
func plausibleHost(h string) bool {
	if net.ParseIP(h) != nil {
		return false
	}
	switch h {
	case "localhost", "host.docker.internal", "gateway.docker.internal":
		return false
	}
	return true
}
