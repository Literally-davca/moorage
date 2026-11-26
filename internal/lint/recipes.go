package lint

import (
	_ "embed"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"

	"moorage/internal/gen"
)

// ---- recipes: what popular images need (M18E) ----------------------------------------
//
// A recipe is data only (nothing is evaluated or run): an image name pattern plus the
// environment variables the image needs, with a generator or a default value, and a
// short note. Findings from recipes always come with a quick fix that inserts
// @generate(...) keywords, so recipes and generators never diverge. A service that
// already sets a variable (with any value) never gets a suggestion.

//go:embed recipes.yaml
var bundledRecipes []byte

// Recipe describes one image family.
type Recipe struct {
	Image string            `yaml:"image" json:"image"` // glob: postgres, */postgres, lscr.io/linuxserver/*
	Env   map[string]string `yaml:"env" json:"env"`     // KEY: generator spec ("password") or "=value"
	Note  string            `yaml:"note,omitempty" json:"note,omitempty"`
}

// MaxRecipes and MaxRecipeEnv bound a catalog.
const (
	MaxRecipes   = 500
	MaxRecipeEnv = 30
)

// ParseRecipes reads and validates a recipes catalog (unknown keys are errors).
func ParseRecipes(b []byte) ([]Recipe, error) {
	if len(b) > MaxFileSize {
		return nil, fmt.Errorf("recipes: larger than %d bytes", MaxFileSize)
	}
	var doc struct {
		Recipes []Recipe `yaml:"recipes"`
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("recipes: %w", err)
	}
	if len(doc.Recipes) > MaxRecipes {
		return nil, fmt.Errorf("recipes: more than %d entries", MaxRecipes)
	}
	for i, r := range doc.Recipes {
		if strings.TrimSpace(r.Image) == "" {
			return nil, fmt.Errorf("recipes[%d]: image is required", i)
		}
		if _, err := path.Match(r.Image, "x"); err != nil {
			return nil, fmt.Errorf("recipes[%d]: bad image pattern %q", i, r.Image)
		}
		if len(r.Env) > MaxRecipeEnv {
			return nil, fmt.Errorf("recipes[%d]: more than %d variables", i, MaxRecipeEnv)
		}
		for k, v := range r.Env {
			if !validEnvKey(k) {
				return nil, fmt.Errorf("recipes[%d]: bad variable %q", i, k)
			}
			if strings.HasPrefix(v, "=") {
				continue
			}
			spec, _, err := gen.ParseGenerate("@generate(" + v + ")")
			if err != nil || spec.Kind == gen.KindPort {
				return nil, fmt.Errorf("recipes[%d] %s: %q is not a generator (write =value for a fixed default)", i, k, v)
			}
		}
	}
	return doc.Recipes, nil
}

var defaultRecipes = func() []Recipe {
	r, err := ParseRecipes(bundledRecipes)
	if err != nil {
		panic(err)
	}
	return r
}()

// imageName strips the tag and digest and the docker.io/library prefix.
func imageName(ref string) string {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i, j := strings.LastIndex(ref, ":"), strings.LastIndex(ref, "/"); i > j {
		ref = ref[:i]
	}
	ref = strings.TrimPrefix(ref, "docker.io/")
	ref = strings.TrimPrefix(ref, "library/")
	return ref
}

func recipeFor(recipes []Recipe, image string) *Recipe {
	name := imageName(image)
	for i := range recipes {
		if ok, _ := path.Match(recipes[i].Image, name); ok {
			return &recipes[i]
		}
	}
	return nil
}

// recipeIssues suggests the variables a recipe lists that the service doesn't set.
func recipeIssues(doc *yaml.Node, recipes []Recipe, add func(Issue)) {
	for _, sn := range serviceNodes(doc) {
		img := mapGet(sn.node, "image")
		if img == nil || img.val.Kind != yaml.ScalarNode || strings.Contains(img.val.Value, "$") {
			continue
		}
		if mapGet(sn.node, "env_file") != nil {
			continue // variables may come from the file
		}
		r := recipeFor(recipes, img.val.Value)
		if r == nil {
			continue
		}
		set := serviceEnvKeys(sn.node)
		var missing []string
		for k := range r.Env {
			if !set[k] {
				missing = append(missing, k)
			}
		}
		if len(missing) == 0 {
			continue
		}
		sort.Strings(missing)
		keys := make([]string, 0, len(missing))
		for _, k := range missing {
			keys = append(keys, k+"="+r.Env[k])
		}
		msg := fmt.Sprintf("%s usually needs %s.", sn.name, strings.Join(missing, ", "))
		if r.Note != "" {
			msg += " " + r.Note
		}
		add(Issue{Severity: Info, Code: "recipe-env", Service: sn.name, Line: sn.line, Message: msg,
			Fix: &Fix{Kind: "add-env-generate", Label: "Add " + plural(len(missing), "variable", "variables"), Service: sn.name, Line: sn.line, Keys: keys}})
	}
}

// serviceEnvKeys lists the variables a service sets in environment: (any value, also an
// empty passthrough).
func serviceEnvKeys(svc *yaml.Node) map[string]bool {
	out := map[string]bool{}
	env := mapGet(svc, "environment")
	if env == nil {
		return out
	}
	switch env.val.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(env.val.Content); i += 2 {
			out[env.val.Content[i].Value] = true
		}
	case yaml.SequenceNode:
		for _, it := range env.val.Content {
			k, _, _ := strings.Cut(it.Value, "=")
			out[strings.TrimSpace(k)] = true
		}
	}
	return out
}

// addEnvGenerate inserts KEY: "@generate(spec)" (or KEY: value for "=value") into the
// service's environment. f.Keys are "KEY=spec" pairs.
func addEnvGenerate(t *textDoc, doc *yaml.Node, f Fix) error {
	sn, ok := findService(doc, f.Service)
	if !ok || sn.node.Kind != yaml.MappingNode || sn.node.Style&yaml.FlowStyle != 0 || len(sn.node.Content) == 0 {
		return ErrNoFix
	}
	set := serviceEnvKeys(sn.node)
	indent := strings.Repeat(" ", sn.node.Content[0].Column-1)
	env := mapGet(sn.node, "environment")
	list := env != nil && env.val.Kind == yaml.SequenceNode
	var lines []string
	for _, kv := range f.Keys {
		k, spec, _ := strings.Cut(kv, "=")
		k = strings.TrimSpace(k)
		if !validEnvKey(k) || set[k] {
			continue
		}
		val := "@generate(" + spec + ")"
		if strings.HasPrefix(spec, "=") {
			val = strings.TrimPrefix(spec, "=")
		} else if _, _, err := gen.ParseGenerate(val); err != nil {
			return ErrNoFix
		}
		if list {
			lines = append(lines, "- "+quoteYAML(k+"="+val))
		} else {
			lines = append(lines, k+": "+quoteYAML(val))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	switch {
	case env == nil:
		block := []string{indent + "environment:"}
		for _, l := range lines {
			block = append(block, indent+"  "+l)
		}
		i := sn.key.Line
		t.lines = append(t.lines[:i], append(block, t.lines[i:]...)...)
	case env.val.Style&yaml.FlowStyle != 0 || len(env.val.Content) == 0:
		return ErrNoFix
	default:
		first := env.val.Content[0]
		in := strings.Repeat(" ", first.Column-1)
		if list {
			in = strings.Repeat(" ", max(first.Column-3, 0))
		}
		i := lastLine(env.val.Content[len(env.val.Content)-1])
		block := make([]string, 0, len(lines))
		for _, l := range lines {
			block = append(block, in+l)
		}
		t.lines = append(t.lines[:i], append(block, t.lines[i:]...)...)
	}
	return nil
}

func lastLine(n *yaml.Node) int {
	l := n.Line
	for _, c := range n.Content {
		if x := lastLine(c); x > l {
			l = x
		}
	}
	return l
}

// useFreePort turns the host port of a port entry into @generate(port, <same port>), so
// the save picks the nearest free port. f.Keys[0] is the taken host port.
func useFreePort(t *textDoc, doc *yaml.Node, f Fix) error {
	sn, ok := findService(doc, f.Service)
	if !ok || len(f.Keys) == 0 {
		return ErrNoFix
	}
	port, err := strconv.Atoi(f.Keys[0])
	if err != nil {
		return ErrNoFix
	}
	ports := mapGet(sn.node, "ports")
	if ports == nil || ports.val.Kind != yaml.SequenceNode {
		return ErrNoFix
	}
	for _, p := range ports.val.Content {
		if p.Kind != yaml.ScalarNode {
			continue
		}
		parts := strings.Split(p.Value, ":")
		if len(parts) != 2 || parts[0] != strconv.Itoa(port) {
			continue // short syntax published:target only; IP-bound entries are edited by hand
		}
		parts[0] = "@generate(port, " + strconv.Itoa(port) + ")"
		ln, s, e, ok := t.scalarSpan(p)
		if !ok {
			return ErrNoFix
		}
		t.replace(ln, s, e, quoteYAML(strings.Join(parts, ":")))
		return nil
	}
	return ErrNoFix
}

var memValueRe = regexp.MustCompile(`^\d{1,6}[bkmg]?$`)

// setServiceKey sets mem_limit (set-mem-limit, Value like "1g") or restart: "no"
// (set-restart-no) on a service: replaces the existing line or adds one after the
// service's key. A deploy.resources.limits.memory is replaced the same way.
func setServiceKey(t *textDoc, doc *yaml.Node, f Fix) error {
	sn, ok := findService(doc, f.Service)
	if !ok || sn.node.Kind != yaml.MappingNode || sn.node.Style&yaml.FlowStyle != 0 || len(sn.node.Content) == 0 {
		return ErrNoFix
	}
	key, val := "restart", `"no"`
	if f.Kind == "set-mem-limit" {
		v := strings.ToLower(strings.TrimSpace(f.Value))
		if !memValueRe.MatchString(v) {
			return ErrNoFix
		}
		key, val = "mem_limit", v
		if d := mapGet(sn.node, "deploy"); d != nil {
			if r := mapGet(d.val, "resources"); r != nil {
				if l := mapGet(r.val, "limits"); l != nil {
					if m := mapGet(l.val, "memory"); m != nil {
						ln, s, e, ok := t.scalarSpan(m.val)
						if !ok {
							return ErrNoFix
						}
						t.replace(ln, s, e, val)
						return nil
					}
				}
			}
		}
	}
	if cur := mapGet(sn.node, key); cur != nil {
		ln, s, e, ok := t.scalarSpan(cur.val)
		if !ok {
			return ErrNoFix
		}
		t.replace(ln, s, e, val)
		return nil
	}
	indent := strings.Repeat(" ", sn.node.Content[0].Column-1)
	i := sn.key.Line
	t.lines = append(t.lines[:i], append([]string{indent + key + ": " + val}, t.lines[i:]...)...)
	return nil
}
