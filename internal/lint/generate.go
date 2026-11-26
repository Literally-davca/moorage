package lint

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"

	"moorage/internal/gen"
)

// ---- generated values: @generate(...) and Coolify magic variables (M18E) ------------
//
// Keywords are materialized once, when a stack is saved: the value is written to .env as
// an ordinary variable and compose.yaml refers to it with ${KEY}, so the stack also runs
// with plain `docker compose`. A key .env already has with a non-empty value (not a
// keyword) is never overwritten: changing a database password would break the database.

// GenOptions supply what the generator can't know itself.
type GenOptions struct {
	// FreePort returns a free host port at or after preferred (0 = any) and counts it as
	// taken for the rest of this call. nil means ports can't be generated here.
	FreePort func(preferred int) (int, error)
	// HostAddress is the host's default address (M17B) for SERVICE_URL_* / SERVICE_FQDN_*;
	// empty leaves them unresolved.
	HostAddress string
}

// GeneratedValue describes one value written to .env. Secrets are never included: the
// preview shows names and kinds only. Port, URL and FQDN values are not secret and are
// shown.
type GeneratedValue struct {
	Key   string `json:"key"`
	Kind  string `json:"kind"`            // "password, 32", "port, 8080", "url"…
	Value string `json:"value,omitempty"` // ports, URLs and host names only
	Note  string `json:"note,omitempty"`  // "8080 is taken by jellyfin"…
}

// GenResult is the outcome of Materialize.
type GenResult struct {
	Compose   string           `json:"compose"`
	Env       string           `json:"env"`
	Changed   bool             `json:"changed"`
	Generated []GeneratedValue `json:"generated,omitempty"`
	// Skipped explains why nothing was generated (a Coolify-managed stack).
	Skipped string `json:"skipped,omitempty"`
}

var (
	portKwRe       = regexp.MustCompile(`^\s*(@generate\([^)]*\))(.*)$`)
	magicRefRe     = regexp.MustCompile(`\$\{(SERVICE_[A-Z0-9_]+)(?::?-([^}]*))?\}`)
	portKwInlineRe = regexp.MustCompile(`@generate\(\s*port\s*(?:,\s*\d+\s*)?\)`)
	urlPortRe      = regexp.MustCompile(`^(.*)_(\d{1,5})$`)
)

// HasKeywords reports whether the files contain anything Materialize would act on.
func HasKeywords(compose, env string) bool {
	if gen.ContainsGenerate(compose) || gen.ContainsGenerate(env) {
		return true
	}
	ef := parseEnvFile(env)
	for _, m := range magicRefRe.FindAllStringSubmatch(compose, -1) {
		if _, ok := gen.CoolifySpec(m[1]); ok && strings.TrimSpace(ef.vals[m[1]]) == "" {
			return true
		}
	}
	return false
}

// CoolifyManaged reports a stack Coolify manages (coolify.* labels): Coolify generates its
// SERVICE_* values itself, so Moorage must not write them.
func CoolifyManaged(compose string) bool { return coolifyLabelRe.MatchString(compose) }

var coolifyLabelRe = regexp.MustCompile(`(?m)^\s*-?\s*["']?coolify\.[a-zA-Z]`)

type genNeed struct {
	key  string
	spec gen.GenSpec
	pref int // ports: preferred
}

// Materialize generates the missing values and rewrites the keywords. It returns the
// files unchanged (Changed false) when there is nothing to do; an error names the first
// invalid keyword or a port that could not be found.
func Materialize(compose, env string, o GenOptions) (GenResult, error) {
	res := GenResult{Compose: compose, Env: env}
	if !HasKeywords(compose, env) {
		return res, nil
	}
	if CoolifyManaged(compose) {
		res.Skipped = "Coolify manages this stack and generates its SERVICE_* values itself."
		return res, nil
	}
	doc, err := parseNode(compose)
	if err != nil {
		return res, fmt.Errorf("compose.yaml: %s", cleanYAMLErr(err))
	}
	ef := parseEnvFile(env)
	t := newText(compose)
	var needs []genNeed
	needed := map[string]bool{}
	has := func(k string) bool {
		v, ok := ef.vals[k]
		if !ok || strings.TrimSpace(v) == "" {
			return false
		}
		_, isKw, _ := gen.ParseGenerate(v)
		return !isKw
	}
	need := func(k string, spec gen.GenSpec) {
		if needed[k] || has(k) {
			return
		}
		needed[k] = true
		n := genNeed{key: k, spec: spec}
		if spec.Kind == gen.KindPort {
			n.pref = spec.PreferredPort()
		}
		needs = append(needs, n)
	}

	// 1. KEY=@generate(...) in .env
	envKeys := make([]string, 0, len(ef.vals))
	for k := range ef.vals {
		envKeys = append(envKeys, k)
	}
	sort.Slice(envKeys, func(i, j int) bool { return ef.line[envKeys[i]] < ef.line[envKeys[j]] })
	for _, k := range envKeys {
		spec, ok, err := gen.ParseGenerate(ef.vals[k])
		if !ok {
			continue
		}
		if err != nil {
			return res, fmt.Errorf(".env %s: %w", k, err)
		}
		delete(ef.vals, k) // a keyword is not a value
		need(k, spec)
	}

	// 2. environment values and ports in compose.yaml
	portKeys := map[string]int{}
	for _, sn := range serviceNodes(doc) {
		if envN := mapGet(sn.node, "environment"); envN != nil && envN.val.Style&yaml.FlowStyle == 0 {
			switch envN.val.Kind {
			case yaml.MappingNode:
				for i := 0; i+1 < len(envN.val.Content); i += 2 {
					k, v := envN.val.Content[i], envN.val.Content[i+1]
					spec, ok, err := gen.ParseGenerate(v.Value)
					if v.Kind != yaml.ScalarNode || !ok {
						continue
					}
					if err != nil {
						return res, fmt.Errorf("service %s, %s: %w", sn.name, k.Value, err)
					}
					if !validEnvKey(k.Value) {
						return res, fmt.Errorf("service %s: %q can't be a .env variable", sn.name, k.Value)
					}
					ln, s, e, ok := t.scalarSpan(v)
					if !ok {
						return res, fmt.Errorf("service %s, %s: write @generate(...) on one line", sn.name, k.Value)
					}
					t.replace(ln, s, e, "${"+k.Value+"}")
					need(k.Value, spec)
				}
			case yaml.SequenceNode:
				for _, it := range envN.val.Content {
					key, val, ok := strings.Cut(it.Value, "=")
					if it.Kind != yaml.ScalarNode || !ok {
						continue
					}
					key = strings.TrimSpace(key)
					spec, isKw, err := gen.ParseGenerate(val)
					if !isKw {
						continue
					}
					if err != nil {
						return res, fmt.Errorf("service %s, %s: %w", sn.name, key, err)
					}
					if !validEnvKey(key) {
						return res, fmt.Errorf("service %s: %q can't be a .env variable", sn.name, key)
					}
					ln, s, e, ok := t.scalarSpan(it)
					if !ok {
						return res, fmt.Errorf("service %s, %s: write @generate(...) on one line", sn.name, key)
					}
					t.replace(ln, s, e, quoteYAML(key+"=${"+key+"}"))
					need(key, spec)
				}
			}
		}
		if ports := mapGet(sn.node, "ports"); ports != nil && ports.val.Kind == yaml.SequenceNode {
			for _, p := range ports.val.Content {
				if p.Kind != yaml.ScalarNode || !gen.ContainsGenerate(p.Value) {
					continue
				}
				m := portKwRe.FindStringSubmatch(p.Value)
				if m == nil {
					return res, fmt.Errorf("service %s: in ports: write @generate(port, 8080) as the host port, e.g. \"@generate(port, 8080):80\"", sn.name)
				}
				spec, _, err := gen.ParseGenerate(m[1])
				if err != nil {
					return res, fmt.Errorf("service %s, ports: %w", sn.name, err)
				}
				if spec.Kind != gen.KindPort {
					return res, fmt.Errorf("service %s: only @generate(port, ...) works in ports:", sn.name)
				}
				key := "SERVICE_PORT_" + envName(sn.name)
				portKeys[key]++
				if portKeys[key] > 1 {
					key += "_" + strconv.Itoa(portKeys[key])
				}
				ln, s, e, ok := t.scalarSpan(p)
				if !ok {
					return res, fmt.Errorf("service %s: write the port on one line", sn.name)
				}
				t.replace(ln, s, e, quoteYAML("${"+key+"}"+m[2]))
				need(key, spec)
			}
		}
	}

	// 3. Coolify magic variables referenced anywhere in compose.yaml
	var urls []genNeed
	for _, m := range magicRefRe.FindAllStringSubmatch(t.String(), -1) {
		spec, ok := gen.CoolifySpec(m[1])
		if !ok {
			continue
		}
		switch spec.Kind {
		case gen.KindPort:
			pref, _ := strconv.Atoi(strings.TrimSpace(m[2]))
			spec.Params = []string{}
			if pref > 0 && pref <= 65535 {
				spec.Params = []string{strconv.Itoa(pref)}
			}
			need(m[1], spec)
		case "url", "fqdn":
			if !needed[m[1]] && !has(m[1]) {
				needed[m[1]] = true
				urls = append(urls, genNeed{key: m[1], spec: spec})
			}
		default:
			need(m[1], spec)
		}
	}

	// 4. generate
	for _, n := range needs {
		g := GeneratedValue{Key: n.key, Kind: n.spec.String()}
		if n.spec.Kind == gen.KindPort {
			if o.FreePort == nil {
				return res, fmt.Errorf("%s: free ports can't be chosen on this host", n.key)
			}
			port, err := o.FreePort(n.pref)
			if err != nil {
				return res, fmt.Errorf("%s: %w", n.key, err)
			}
			g.Value = strconv.Itoa(port)
			if n.pref > 0 && port != n.pref {
				g.Note = fmt.Sprintf("%d is taken", n.pref)
			}
			ef.set(n.key, g.Value)
		} else {
			v, err := n.spec.Value()
			if err != nil {
				return res, fmt.Errorf("%s: %w", n.key, err)
			}
			ef.set(n.key, v)
		}
		res.Generated = append(res.Generated, g)
	}
	for _, u := range urls {
		if o.HostAddress == "" {
			continue // left unresolved; the linter says why
		}
		id := strings.TrimPrefix(strings.TrimPrefix(u.key, "SERVICE_URL_"), "SERVICE_FQDN_")
		port := ""
		if pm := urlPortRe.FindStringSubmatch(id); pm != nil {
			id, port = pm[1], pm[2]
		} else if p := ef.vals["SERVICE_PORT_"+id]; p != "" {
			port = p
		}
		v := o.HostAddress
		if u.spec.Kind == "url" {
			v = "http://" + o.HostAddress
			if port != "" && port != "80" {
				v += ":" + port
			}
		}
		ef.set(u.key, v)
		res.Generated = append(res.Generated, GeneratedValue{Key: u.key, Kind: u.spec.Kind, Value: v})
	}
	if len(res.Generated) == 0 && t.String() == compose {
		return res, nil
	}
	res.Compose, res.Env, res.Changed = t.String(), ef.String(), true
	return res, nil
}

// ---- linter rules -----------------------------------------------------------------

// lintCompose replaces port keywords with their preferred port so the compose loader can
// read a draft that still has @generate(port, …) in ports:.
func lintCompose(compose string) string {
	if !gen.ContainsGenerate(compose) {
		return compose
	}
	return portKwInlineRe.ReplaceAllStringFunc(compose, func(s string) string {
		spec, _, err := gen.ParseGenerate(s)
		if err != nil || spec.PreferredPort() == 0 {
			return "49152"
		}
		return strconv.Itoa(spec.PreferredPort())
	})
}

// generateIssues reports keywords that are invalid, misplaced or still waiting for a save.
func generateIssues(doc *yaml.Node, compose, env string, add func(Issue)) {
	if !HasKeywords(compose, env) {
		return
	}
	coolify := CoolifyManaged(compose)
	ef := parseEnvFile(env)
	pending := func(key, kind string, line int, svc string) {
		msg := fmt.Sprintf("%s will be generated into .env when you save (%s).", key, kind)
		if coolify {
			msg = fmt.Sprintf("%s is left for Coolify to generate (the stack is managed by Coolify).", key)
		}
		add(Issue{Severity: Info, Code: "generate-pending", Service: svc, Line: line, Message: msg})
	}
	for k, v := range ef.vals {
		spec, ok, err := gen.ParseGenerate(v)
		if !ok {
			continue
		}
		if err != nil {
			add(Issue{Severity: Error, Code: "generate-invalid", Message: ".env " + k + ": " + err.Error()})
			continue
		}
		pending(k, spec.String(), 0, "")
	}
	for _, sn := range serviceNodes(doc) {
		allowed := map[*yaml.Node]bool{}
		if envN := mapGet(sn.node, "environment"); envN != nil {
			for _, c := range envN.val.Content {
				allowed[c] = true
				if c.Kind != yaml.ScalarNode {
					continue
				}
				val := c.Value
				key := ""
				if envN.val.Kind == yaml.SequenceNode {
					key, val, _ = strings.Cut(c.Value, "=")
				}
				spec, ok, err := gen.ParseGenerate(val)
				if !ok {
					continue
				}
				if err != nil {
					add(Issue{Severity: Error, Code: "generate-invalid", Service: sn.name, Line: c.Line, Message: err.Error()})
					continue
				}
				if key == "" {
					// mapping: the key node precedes the value node
					for i := 0; i+1 < len(envN.val.Content); i += 2 {
						if envN.val.Content[i+1] == c {
							key = envN.val.Content[i].Value
						}
					}
				}
				pending(strings.TrimSpace(key), spec.String(), c.Line, sn.name)
			}
		}
		if ports := mapGet(sn.node, "ports"); ports != nil {
			for _, p := range ports.val.Content {
				allowed[p] = true
				if p.Kind != yaml.ScalarNode || !gen.ContainsGenerate(p.Value) {
					continue
				}
				m := portKwRe.FindStringSubmatch(p.Value)
				spec, _, err := gen.ParseGenerate(firstOr(m, 1))
				switch {
				case m == nil:
					add(Issue{Severity: Error, Code: "generate-invalid", Service: sn.name, Line: p.Line, Message: "In ports: write @generate(port, 8080) as the host port, e.g. \"@generate(port, 8080):80\"."})
				case err != nil:
					add(Issue{Severity: Error, Code: "generate-invalid", Service: sn.name, Line: p.Line, Message: err.Error()})
				case spec.Kind != gen.KindPort:
					add(Issue{Severity: Error, Code: "generate-invalid", Service: sn.name, Line: p.Line, Message: "Only @generate(port, …) works in ports:."})
				default:
					pending("SERVICE_PORT_"+envName(sn.name), spec.String(), p.Line, sn.name)
				}
			}
		}
		walkScalars(sn.node, func(n *yaml.Node) {
			if gen.ContainsGenerate(n.Value) && !allowed[n] {
				add(Issue{Severity: Error, Code: "generate-misplaced", Service: sn.name, Line: n.Line,
					Message: "@generate works only in environment values and ports:."})
			}
		})
	}
	seen := map[string]bool{}
	for _, m := range magicRefRe.FindAllStringSubmatch(compose, -1) {
		spec, ok := gen.CoolifySpec(m[1])
		if !ok || seen[m[1]] || strings.TrimSpace(ef.vals[m[1]]) != "" {
			continue
		}
		seen[m[1]] = true
		kind := spec.String()
		if spec.Kind == "url" || spec.Kind == "fqdn" {
			kind += " from the host's default address"
		}
		pending(m[1], kind, 0, "")
	}
}

func firstOr(m []string, i int) string {
	if i < len(m) {
		return m[i]
	}
	return ""
}

func walkScalars(n *yaml.Node, f func(*yaml.Node)) {
	if n == nil {
		return
	}
	if n.Kind == yaml.ScalarNode {
		f(n)
		return
	}
	for _, c := range n.Content {
		walkScalars(c, f)
	}
}

// generatedKey reports a variable that a save will generate (missing-var must not block it).
func generatedKey(k string) bool {
	_, ok := gen.CoolifySpec(k)
	return ok
}
