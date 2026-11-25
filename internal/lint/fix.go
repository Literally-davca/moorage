package lint

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.yaml.in/yaml/v4"
)

// Fixes edit compose.yaml as text, line by line, so comments, order and formatting of
// everything they don't touch stay exactly as written (re-encoding the YAML would not).

// Conflict is a key that .env already has with a different, non-empty value.
type Conflict struct {
	Key          string `json:"key"`
	EnvValue     string `json:"envValue"`
	ComposeValue string `json:"composeValue"`
	Service      string `json:"service"`
}

// Result is the outcome of a fix or of moving environment values into .env.
type Result struct {
	Compose   string     `json:"compose"`
	Env       string     `json:"env"`
	Moved     []string   `json:"moved,omitempty"`     // .env keys written by the move
	Conflicts []Conflict `json:"conflicts,omitempty"` // left in compose.yaml unless overwrite
	Changed   bool       `json:"changed"`
}

// ErrNoFix: the fix doesn't apply (the file changed, or the form isn't supported).
var ErrNoFix = errors.New("this fix can't be applied to the file as it is now; edit it by hand")

type textDoc struct {
	lines []string
	crlf  bool
}

func newText(s string) *textDoc {
	crlf := strings.Contains(s, "\r\n")
	return &textDoc{lines: strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n"), crlf: crlf}
}

func (t *textDoc) String() string {
	s := strings.Join(t.lines, "\n")
	if t.crlf {
		s = strings.ReplaceAll(s, "\n", "\r\n")
	}
	return s
}

// scalarSpan finds the text of a single-line scalar node: its start (rune index in the
// line) and end. ok is false for block, multi-line or flow-embedded scalars.
func (t *textDoc) scalarSpan(n *yaml.Node) (line int, start, end int, ok bool) {
	if n.Kind != yaml.ScalarNode || n.Line < 1 || n.Line > len(t.lines) || n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return 0, 0, 0, false
	}
	r := []rune(t.lines[n.Line-1])
	start = n.Column - 1
	if start < 0 || start >= len(r) {
		return 0, 0, 0, false
	}
	switch {
	case n.Style&yaml.DoubleQuotedStyle != 0:
		if r[start] != '"' {
			return 0, 0, 0, false
		}
		for i := start + 1; i < len(r); i++ {
			if r[i] == '\\' {
				i++
				continue
			}
			if r[i] == '"' {
				return n.Line, start, i + 1, true
			}
		}
		return 0, 0, 0, false
	case n.Style&yaml.SingleQuotedStyle != 0:
		if r[start] != '\'' {
			return 0, 0, 0, false
		}
		for i := start + 1; i < len(r); i++ {
			if r[i] == '\'' {
				if i+1 < len(r) && r[i+1] == '\'' {
					i++
					continue
				}
				return n.Line, start, i + 1, true
			}
		}
		return 0, 0, 0, false
	default:
		end = len(r)
		for i := start; i < len(r); i++ {
			if r[i] == '#' && i > start && (r[i-1] == ' ' || r[i-1] == '\t') {
				end = i
				break
			}
		}
		for end > start && (r[end-1] == ' ' || r[end-1] == '\t') {
			end--
		}
		if string(r[start:end]) != n.Value { // continues on the next line
			return 0, 0, 0, false
		}
		return n.Line, start, end, true
	}
}

func (t *textDoc) replace(line, start, end int, with string) {
	r := []rune(t.lines[line-1])
	t.lines[line-1] = string(r[:start]) + with + string(r[end:])
}

// quoteYAML writes s as a YAML scalar that is safe anywhere: plain when harmless, else
// double quoted.
func quoteYAML(s string) string {
	if s != "" && !strings.ContainsAny(s, ":#{}[],&*!|>'\"%@`\\") && strings.TrimSpace(s) == s {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// ---- .env text ------------------------------------------------------------------

type envFile struct {
	t    *textDoc
	vals map[string]string
	line map[string]int // key -> 0-based line of its last definition
}

func parseEnvFile(env string) *envFile {
	if env != "" && !strings.HasSuffix(env, "\n") {
		env += "\n"
	}
	e := &envFile{t: newText(env), vals: map[string]string{}, line: map[string]int{}}
	for i, l := range e.t.lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		t = strings.TrimPrefix(t, "export ")
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		e.vals[k] = unquoteEnv(strings.TrimSpace(v))
		e.line[k] = i
	}
	return e
}

func unquoteEnv(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		inner := v[1 : len(v)-1]
		if v[0] == '"' {
			inner = strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n").Replace(inner)
		}
		return inner
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

// formatEnvValue mirrors web/src/lib/dotenv.ts formatValue.
func formatEnvValue(v string) string {
	if v == "" {
		return ""
	}
	if !strings.ContainsAny(v, " \t\n#\"'\\$`") {
		return v
	}
	if !strings.ContainsAny(v, "'\n") {
		return "'" + v + "'"
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v) + `"`
}

// set writes KEY=value: replaces the existing definition or appends a new line.
func (e *envFile) set(k, v string) {
	line := k + "=" + formatEnvValue(v)
	if i, ok := e.line[k]; ok {
		e.t.lines[i] = line
	} else {
		// before the final empty element that the trailing newline leaves
		n := len(e.t.lines)
		if n > 0 && e.t.lines[n-1] == "" {
			e.t.lines = append(e.t.lines[:n-1], line, "")
		} else {
			e.t.lines = append(e.t.lines, line, "")
		}
		e.line[k] = len(e.t.lines) - 2
	}
	e.vals[k] = v
}

func (e *envFile) remove(keys []string) {
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
	}
	var out []string
	for _, l := range e.t.lines {
		t := strings.TrimPrefix(strings.TrimSpace(l), "export ")
		k, _, ok := strings.Cut(t, "=")
		if ok && !strings.HasPrefix(t, "#") && drop[strings.TrimSpace(k)] {
			continue
		}
		out = append(out, l)
	}
	e.t.lines = out
}

func (e *envFile) String() string {
	s := e.t.String()
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return s
}

// ---- moving environment values into .env ------------------------------------------

type envEntry struct {
	svc, key, value string
	node            *yaml.Node
	list            bool // "- KEY=value" form
}

// literalEntries finds environment entries with a literal value. Values with $ (a
// reference, an escape) or none (passthrough "- FOO", "FOO:") are left alone.
func literalEntries(doc *yaml.Node) []envEntry {
	var out []envEntry
	for _, sn := range serviceNodes(doc) {
		env := mapGet(sn.node, "environment")
		if env == nil || env.val.Style&yaml.FlowStyle != 0 {
			continue
		}
		switch env.val.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(env.val.Content); i += 2 {
				k, v := env.val.Content[i], env.val.Content[i+1]
				if k.Value == "<<" || v.Kind != yaml.ScalarNode || v.Tag == "!!null" || v.Value == "" || strings.Contains(v.Value, "$") {
					continue
				}
				out = append(out, envEntry{svc: sn.name, key: k.Value, value: v.Value, node: v})
			}
		case yaml.SequenceNode:
			for _, it := range env.val.Content {
				if it.Kind != yaml.ScalarNode {
					continue
				}
				k, v, ok := strings.Cut(it.Value, "=")
				if !ok || v == "" || strings.Contains(v, "$") || strings.TrimSpace(k) == "" {
					continue
				}
				out = append(out, envEntry{svc: sn.name, key: strings.TrimSpace(k), value: v, node: it, list: true})
			}
		}
	}
	return out
}

// MoveEnv moves the literal environment values of compose.yaml into .env and replaces
// them with ${KEY}. One key with the same value in several services becomes one variable;
// different values become SERVICE_KEY. A key .env already has with a different non-empty
// value is a conflict: it stays in compose.yaml unless overwrite is set.
func MoveEnv(compose, env string, overwrite bool) (Result, error) {
	res := Result{Compose: compose, Env: env}
	doc, err := parseNode(compose)
	if err != nil {
		return res, nil // a broken file is the linter's job
	}
	entries := literalEntries(doc)
	if len(entries) == 0 {
		return res, nil
	}
	values := map[string]map[string]bool{}
	for _, e := range entries {
		if values[e.key] == nil {
			values[e.key] = map[string]bool{}
		}
		values[e.key][e.value] = true
	}
	t := newText(compose)
	ef := parseEnvFile(env)
	moved := map[string]bool{}
	for _, e := range entries {
		name := e.key
		if len(values[e.key]) > 1 {
			name = envName(e.svc) + "_" + e.key
		}
		if !validEnvKey(name) {
			continue
		}
		if cur, has := ef.vals[name]; has && cur != e.value {
			if strings.TrimSpace(cur) != "" && !overwrite {
				res.Conflicts = append(res.Conflicts, Conflict{Key: name, EnvValue: cur, ComposeValue: e.value, Service: e.svc})
				continue
			}
		}
		ln, s, end, ok := t.scalarSpan(e.node)
		if !ok {
			continue
		}
		repl := "${" + name + "}"
		if e.list {
			repl = quoteYAML(e.key + "=${" + name + "}")
		}
		t.replace(ln, s, end, repl)
		if ef.vals[name] != e.value || !hasKey(ef, name) {
			ef.set(name, e.value)
		}
		moved[name] = true
	}
	if len(moved) == 0 {
		return res, nil
	}
	for k := range moved {
		res.Moved = append(res.Moved, k)
	}
	sort.Strings(res.Moved)
	res.Compose, res.Env, res.Changed = t.String(), ef.String(), true
	return res, nil
}

func hasKey(ef *envFile, k string) bool { _, ok := ef.vals[k]; return ok }

func validEnvKey(k string) bool {
	if k == "" || (k[0] >= '0' && k[0] <= '9') {
		return false
	}
	for _, r := range k {
		if !(r == '_' || r == '.' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// ---- quick fixes --------------------------------------------------------------------

// Apply runs one quick fix and returns the new files.
func Apply(compose, env string, f Fix) (Result, error) {
	res := Result{Compose: compose, Env: env}
	switch f.Kind {
	case "move-env":
		return MoveEnv(compose, env, false)
	case "remove-unused":
		ef := parseEnvFile(env)
		ef.remove(f.Keys)
		res.Env, res.Changed = ef.String(), true
		return res, nil
	case "add-env-var":
		if len(f.Keys) == 0 {
			return res, ErrNoFix
		}
		ef := parseEnvFile(env)
		changed := false
		for _, k := range f.Keys {
			k = strings.TrimSpace(k)
			if validEnvKey(k) && !hasKey(ef, k) {
				ef.set(k, "")
				changed = true
			}
		}
		if !changed {
			return res, nil
		}
		res.Env, res.Changed = ef.String(), true
		return res, nil
	}
	doc, err := parseNode(compose)
	if err != nil {
		return res, ErrNoFix
	}
	t := newText(compose)
	switch f.Kind {
	case "remove-version":
		v := mapGet(doc, "version")
		if v == nil || v.val.Kind != yaml.ScalarNode || v.val.Line != v.key.Line {
			return res, ErrNoFix
		}
		i := v.key.Line - 1
		t.lines = append(t.lines[:i], t.lines[i+1:]...)
		// drop one blank line left at the top
		if i < len(t.lines) && i == 0 && strings.TrimSpace(t.lines[0]) == "" {
			t.lines = t.lines[1:]
		}
	case "add-restart":
		sn, ok := findService(doc, f.Service)
		if !ok || sn.node.Kind != yaml.MappingNode || sn.node.Style&yaml.FlowStyle != 0 || len(sn.node.Content) == 0 {
			return res, ErrNoFix
		}
		if mapGet(sn.node, "restart") != nil {
			return res, nil
		}
		indent := strings.Repeat(" ", sn.node.Content[0].Column-1)
		i := sn.key.Line // insert after the service's key line
		t.lines = append(t.lines[:i], append([]string{indent + "restart: unless-stopped"}, t.lines[i:]...)...)
	case "bind-localhost":
		sn, ok := findService(doc, f.Service)
		if !ok {
			return res, ErrNoFix
		}
		ports := mapGet(sn.node, "ports")
		if ports == nil || ports.val.Kind != yaml.SequenceNode {
			return res, ErrNoFix
		}
		done := false
		for _, p := range ports.val.Content {
			nv, ok := bindLocal(p.Value)
			if p.Kind != yaml.ScalarNode || !ok {
				continue
			}
			ln, s, end, ok := t.scalarSpan(p)
			if !ok {
				continue
			}
			t.replace(ln, s, end, `"`+nv+`"`)
			done = true
		}
		if !done {
			return res, ErrNoFix
		}
	case "extract-secret":
		sn, ok := findService(doc, f.Service)
		if !ok {
			return res, ErrNoFix
		}
		ef := parseEnvFile(env)
		name := secretVarName(f.Service)
		for n := 2; hasKey(ef, name); n++ {
			name = fmt.Sprintf("%s_%d", secretVarName(f.Service), n)
		}
		done := false
		for _, field := range []string{"command", "entrypoint"} {
			fn := mapGet(sn.node, field)
			if fn == nil {
				continue
			}
			for _, s := range scalars(fn.val) {
				secret := findSecret(s.Value)
				if secret == "" || (f.Line != 0 && s.Line != f.Line) {
					continue
				}
				ln, st, end, ok := t.scalarSpan(s)
				if !ok {
					continue
				}
				r := []rune(t.lines[ln-1])
				text := strings.Replace(string(r[st:end]), secret, "${"+name+"}", -1)
				t.replace(ln, st, end, text)
				ef.set(name, secret)
				done = true
				break
			}
			if done {
				break
			}
		}
		if !done {
			return res, ErrNoFix
		}
		res.Env = ef.String()
		res.Moved = []string{name}
	case "add-mem-limit":
		sn, ok := findService(doc, f.Service)
		if !ok || sn.node.Kind != yaml.MappingNode || sn.node.Style&yaml.FlowStyle != 0 || len(sn.node.Content) == 0 {
			return res, ErrNoFix
		}
		if mapGet(sn.node, "mem_limit") != nil {
			return res, nil
		}
		indent := strings.Repeat(" ", sn.node.Content[0].Column-1)
		i := sn.key.Line
		t.lines = append(t.lines[:i], append([]string{indent + "mem_limit: 512m"}, t.lines[i:]...)...)
	case "add-healthcheck":
		sn, ok := findService(doc, f.Service)
		if !ok || sn.node.Kind != yaml.MappingNode || sn.node.Style&yaml.FlowStyle != 0 || len(sn.node.Content) == 0 {
			return res, ErrNoFix
		}
		if mapGet(sn.node, "healthcheck") != nil {
			return res, nil
		}
		indent := strings.Repeat(" ", sn.node.Content[0].Column-1)
		hcLines := healthcheckBlock(indent, sn.node)
		i := sn.key.Line
		t.lines = append(t.lines[:i], append(hcLines, t.lines[i:]...)...)
	case "pin-tag":
		if strings.TrimSpace(f.TargetTag) == "" {
			return res, ErrNoFix
		}
		sn, ok := findService(doc, f.Service)
		if !ok {
			return res, ErrNoFix
		}
		img := mapGet(sn.node, "image")
		if img == nil || img.val.Kind != yaml.ScalarNode {
			return res, ErrNoFix
		}
		ln, st, end, ok := t.scalarSpan(img.val)
		if !ok {
			return res, ErrNoFix
		}
		pinned := replaceImageTag(img.val.Value, strings.TrimSpace(f.TargetTag))
		repl := pinned
		if img.val.Style == yaml.DoubleQuotedStyle {
			repl = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(pinned) + `"`
		} else if img.val.Style == yaml.SingleQuotedStyle {
			repl = "'" + strings.ReplaceAll(pinned, "'", "''") + "'"
		} else if strings.ContainsAny(pinned, " \t#{}[],&*!|>'\"%@`\\") {
			repl = quoteYAML(pinned)
		}
		t.replace(ln, st, end, repl)
	default:
		return res, ErrNoFix
	}
	res.Compose, res.Changed = t.String(), true
	return res, nil
}

// ServiceImage returns the raw image: string of service in compose, or "".
func ServiceImage(compose, service string) string {
	doc, err := parseNode(compose)
	if err != nil {
		return ""
	}
	sn, ok := findService(doc, service)
	if !ok {
		return ""
	}
	if img := mapGet(sn.node, "image"); img != nil && img.val.Kind == yaml.ScalarNode {
		return strings.TrimSpace(img.val.Value)
	}
	return ""
}

func replaceImageTag(ref, newTag string) string {
	ref = strings.TrimSpace(ref)
	if idx := strings.Index(ref, "@"); idx >= 0 {
		ref = ref[:idx]
	}
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash {
		ref = ref[:lastColon]
	}
	return ref + ":" + newTag
}

func healthcheckBlock(indent string, svc *yaml.Node) []string {
	img := ""
	if im := mapGet(svc, "image"); im != nil && im.val.Kind == yaml.ScalarNode {
		img = strings.ToLower(im.val.Value)
	}
	testCmd := `["CMD-SHELL", "exit 0"] # adjust check command`
	switch {
	case strings.Contains(img, "postgres"):
		testCmd = `["CMD-SHELL", "pg_isready -U $${POSTGRES_USER:-postgres}"]`
	case strings.Contains(img, "mariadb") || strings.Contains(img, "mysql"):
		testCmd = `["CMD-SHELL", "mysqladmin ping -h 127.0.0.1 || exit 1"]`
	case strings.Contains(img, "redis") || strings.Contains(img, "valkey"):
		testCmd = `["CMD", "redis-cli", "ping"]`
	case strings.Contains(img, "mongo"):
		testCmd = `["CMD", "mongosh", "--eval", "db.adminCommand('ping')"]`
	default:
		if port := firstServicePort(svc); port != "" {
			testCmd = fmt.Sprintf(`["CMD-SHELL", "wget -qO- http://127.0.0.1:%s/ >/dev/null || exit 1"]`, port)
		}
	}
	sub := indent + "  "
	return []string{
		indent + "healthcheck:",
		sub + "test: " + testCmd,
		sub + "interval: 30s",
		sub + "timeout: 5s",
		sub + "retries: 3",
		sub + "start_period: 15s",
	}
}

func firstServicePort(svc *yaml.Node) string {
	pn := mapGet(svc, "ports")
	if pn == nil || pn.val.Kind != yaml.SequenceNode || len(pn.val.Content) == 0 {
		return ""
	}
	first := pn.val.Content[0]
	if first.Kind != yaml.ScalarNode {
		return ""
	}
	raw, _, _ := strings.Cut(strings.TrimSpace(first.Value), "/")
	parts := strings.Split(raw, ":")
	target := strings.TrimSpace(parts[len(parts)-1])
	for _, r := range target {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return target
}

func findService(doc *yaml.Node, name string) (svcNode, bool) {
	for _, sn := range serviceNodes(doc) {
		if sn.name == name {
			return sn, true
		}
	}
	return svcNode{}, false
}

// bindLocal rewrites a short port mapping of a database port to listen on 127.0.0.1.
// "5432:5432" -> "127.0.0.1:5432:5432", "5432" -> "127.0.0.1::5432". Mappings that already
// name an IP, and ports that aren't database ports, are left alone.
func bindLocal(spec string) (string, bool) {
	s := strings.TrimSpace(spec)
	proto := ""
	if i := strings.Index(s, "/"); i >= 0 {
		s, proto = s[:i], s[i:]
	}
	parts := strings.Split(s, ":")
	target := parts[len(parts)-1]
	port := 0
	fmt.Sscanf(target, "%d", &port)
	if _, isDB := dbPorts[port]; !isDB {
		return "", false
	}
	switch len(parts) {
	case 1:
		return "127.0.0.1::" + s + proto, true
	case 2:
		return "127.0.0.1:" + s + proto, true
	}
	return "", false
}
