package templates

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"go.yaml.in/yaml/v4"
)

// Rendered is the result of filling a template's variables. It has no side effects:
// nothing is written until the admin creates the stack from these files.
type Rendered struct {
	Compose  string            `json:"compose"`
	Env      string            `json:"env"`
	Values   map[string]string `json:"values"`  // final value of every variable (generated ones included)
	Secrets  []string          `json:"secrets"` // names of secret variables, for masking in the UI
	Notes    string            `json:"notes,omitempty"`
	Warnings []string          `json:"warnings"`
}

// FieldError names the variable whose value was refused.
type FieldError struct {
	Name string
	Msg  string
}

func (e *FieldError) Error() string { return e.Name + ": " + e.Msg }

// Render fills the template's variables. A value missing from values falls back to a
// generated secret (for variables with generate), then to the default. Values are only
// substituted as text, never evaluated.
func Render(t Template, values map[string]string) (Rendered, error) {
	final := make(map[string]string, len(t.Variables))
	r := Rendered{Values: final, Secrets: []string{}}
	for i := range t.Variables {
		v := &t.Variables[i]
		val, err := resolve(v, values[v.Name])
		if err != nil {
			return Rendered{}, err
		}
		final[v.Name] = val
		if v.Secret {
			r.Secrets = append(r.Secrets, v.Name)
		}
	}
	r.Compose = Substitute(t.Compose, final, true)
	r.Env = Substitute(t.Env, final, true)
	// Notes are shown in the UI, so secrets stay out of them even there.
	public := map[string]string{}
	for _, v := range t.Variables {
		if !v.Secret {
			public[v.Name] = final[v.Name]
		}
	}
	r.Notes = Substitute(t.Notes, public, false)
	r.Warnings = Warnings(r.Compose)
	return r, nil
}

func resolve(v *Variable, given string) (string, error) {
	val := strings.TrimSpace(given)
	if val == "" && v.Generate != "" {
		g, err := Generate(v.Generate)
		if err != nil {
			return "", err
		}
		val = g
	}
	if val == "" {
		val = v.Default
	}
	if val == "" {
		if v.Required {
			return "", &FieldError{v.Name, "a value is required"}
		}
		return "", nil
	}
	if err := v.check(val); err != nil {
		return "", &FieldError{v.Name, err.Error()}
	}
	return val, nil
}

// GenerateDefaults returns a fresh value for every variable that has a generator, so the
// form can show them pre-filled (and the admin can still change them).
func GenerateDefaults(t Template) (map[string]string, error) {
	out := map[string]string{}
	for _, v := range t.Variables {
		if v.Generate == "" {
			continue
		}
		g, err := Generate(v.Generate)
		if err != nil {
			return nil, err
		}
		out[v.Name] = g
	}
	return out, nil
}

// Substitute replaces ${NAME} for the names in vals and nothing else:
//   - "$$" is compose's escape for a literal "$" and is copied unchanged;
//   - ${OTHER}, $OTHER and ${NAME:-x} are left for compose's own interpolation from .env.
//
// With escape, a "$" inside a value becomes "$$", so compose and its .env reader keep it
// as a literal dollar instead of reading it as a variable.
func Substitute(text string, vals map[string]string, escape bool) string {
	if !strings.Contains(text, "$") {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		c := text[i]
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		if strings.HasPrefix(text[i:], "$$") {
			b.WriteString("$$")
			i += 2
			continue
		}
		if strings.HasPrefix(text[i:], "${") {
			if end := strings.IndexByte(text[i+2:], '}'); end >= 0 {
				name := text[i+2 : i+2+end]
				if val, ok := vals[name]; ok {
					if escape {
						val = strings.ReplaceAll(val, "$", "$$")
					}
					b.WriteString(val)
					i += end + 3
					continue
				}
			}
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// Generate creates a random value of the given kind with crypto/rand.
func Generate(kind string) (string, error) {
	switch kind {
	case GenPassword:
		return randomString(alnum, 32)
	case GenHex32:
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return "", fmt.Errorf("random: %w", err)
		}
		return hex.EncodeToString(b), nil
	case GenUUID:
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return "", fmt.Errorf("random: %w", err)
		}
		b[6] = (b[6] & 0x0f) | 0x40 // version 4
		b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
		h := hex.EncodeToString(b)
		return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
	}
	return "", fmt.Errorf("unknown generate kind %q", kind)
}

// randomString draws n characters uniformly from alphabet (rejection sampling, no modulo bias).
func randomString(alphabet string, n int) (string, error) {
	limit := 256 - 256%len(alphabet)
	out := make([]byte, 0, n)
	buf := make([]byte, n*2)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("random: %w", err)
		}
		for _, x := range buf {
			if int(x) < limit && len(out) < n {
				out = append(out, alphabet[int(x)%len(alphabet)])
			}
		}
	}
	return string(out), nil
}

// Warnings lists risky settings in a compose file: they give a container control over
// the host. They are shown to the admin, not blocked; the admin decides.
func Warnings(compose string) []string {
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(compose), &doc); err != nil {
		return []string{}
	}
	names := make([]string, 0, len(doc.Services))
	for n := range doc.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	out := []string{}
	for _, n := range names {
		svc := doc.Services[n]
		if isTrue(svc["privileged"]) {
			out = append(out, fmt.Sprintf("Service %s runs privileged: it has full access to the host.", n))
		}
		if str(svc["network_mode"]) == "host" {
			out = append(out, fmt.Sprintf("Service %s uses the host network.", n))
		}
		if str(svc["pid"]) == "host" {
			out = append(out, fmt.Sprintf("Service %s shares the host's process namespace (pid: host).", n))
		}
		if mountsDockerSocket(svc["volumes"]) {
			out = append(out, fmt.Sprintf("Service %s mounts the Docker socket: it can control every container on the host.", n))
		}
	}
	return out
}

func isTrue(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(strings.TrimSpace(x), "true")
	}
	return false
}

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func mountsDockerSocket(v any) bool {
	list, _ := v.([]any)
	for _, m := range list {
		var src string
		switch x := m.(type) {
		case string:
			src, _, _ = strings.Cut(x, ":")
		case map[string]any:
			src = str(x["source"])
		}
		if strings.HasSuffix(strings.TrimSpace(src), "docker.sock") {
			return true
		}
	}
	return false
}
