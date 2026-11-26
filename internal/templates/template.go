// Package templates is the catalogue of compose stack templates: a few bundled examples,
// a local folder the admin controls, and remote indexes added by HTTPS URL.
//
// A template is only data. Its compose file and .env are text with ${VAR} placeholders
// that are replaced by the admin's values or by generated secrets; nothing in a template
// is ever evaluated or run on the hub. The admin always reviews the final files before a
// stack is created through the normal create path.
package templates

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v4"

	"moorage/internal/gen"
)

// Limits for template content. Compose and .env must also fit the editor's limit
// (engine.MaxFileSize, 512 KB); that is checked when the stack is created.
const (
	MaxFileBytes  = 1 << 20 // one template file (template.yaml, compose.yaml, .env)
	MaxVariables  = 50
	maxValueRunes = 1024
	maxPattern    = 256
)

// Generator kinds for variables whose value Moorage creates with crypto/rand.
const (
	GenPassword = "password" // 32 characters from [A-Za-z0-9]
	GenHex32    = "hex32"    // 32 hex characters (16 random bytes)
	GenUUID     = "uuid"     // random UUID v4
)

// IDPattern is the template id rule. It matches the stack name rule, so an id is
// always a usable default stack name.
var IDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

var varNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// Variable is one value the admin fills in (or Moorage generates) before deploying.
type Variable struct {
	Name        string `yaml:"name" json:"name"`
	Label       string `yaml:"label" json:"label"`
	Description string `yaml:"description" json:"description,omitempty"`
	Default     string `yaml:"default" json:"default,omitempty"`
	Secret      bool   `yaml:"secret" json:"secret"`
	Generate    string `yaml:"generate" json:"generate,omitempty"`
	Required    bool   `yaml:"required" json:"required"`
	Pattern     string `yaml:"pattern" json:"pattern,omitempty"`

	re *regexp.Regexp
}

// Meta is the parsed template.yaml.
type Meta struct {
	ID          string     `yaml:"id" json:"id"`
	Name        string     `yaml:"name" json:"name"`
	Version     string     `yaml:"version" json:"version,omitempty"`
	Description string     `yaml:"description" json:"description"`
	Category    string     `yaml:"category" json:"category"`
	Website     string     `yaml:"website" json:"website,omitempty"`
	Notes       string     `yaml:"notes" json:"notes,omitempty"`
	Variables   []Variable `yaml:"variables" json:"variables"`
}

// Template is a parsed template with its files.
type Template struct {
	Meta
	Compose string
	Env     string
}

// ParseMeta parses and validates template.yaml. Unknown keys are an error, so a typo
// like "requried" doesn't silently drop a rule.
func ParseMeta(data []byte) (Meta, error) {
	var m Meta
	if len(data) > MaxFileBytes {
		return m, errors.New("template.yaml is larger than 1 MiB")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return m, errors.New("template.yaml is empty")
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("template.yaml: %w", err)
	}
	if err := m.validate(); err != nil {
		return m, err
	}
	return m, nil
}

func (m *Meta) validate() error {
	m.ID = strings.TrimSpace(m.ID)
	m.Name = strings.TrimSpace(m.Name)
	m.Category = strings.TrimSpace(m.Category)
	m.Version = strings.TrimSpace(m.Version)
	switch {
	case !IDPattern.MatchString(m.ID):
		return errors.New("id: use 1–63 characters: lowercase letters, digits, dash or underscore")
	case m.Name == "" || utf8.RuneCountInString(m.Name) > 80:
		return errors.New("name: give the template a name of 1–80 characters")
	case utf8.RuneCountInString(m.Description) > 1000:
		return errors.New("description: at most 1000 characters")
	case utf8.RuneCountInString(m.Category) > 40:
		return errors.New("category: at most 40 characters")
	case utf8.RuneCountInString(m.Version) > 32 || hasControl(m.Version):
		return errors.New("version: at most 32 characters on one line")
	case utf8.RuneCountInString(m.Notes) > 4000:
		return errors.New("notes: at most 4000 characters")
	case len(m.Variables) > MaxVariables:
		return fmt.Errorf("variables: at most %d", MaxVariables)
	}
	if m.Website != "" {
		u, err := url.Parse(m.Website)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return errors.New("website: use an http or https URL")
		}
	}
	seen := map[string]bool{}
	for i := range m.Variables {
		v := &m.Variables[i]
		if err := v.validate(); err != nil {
			return fmt.Errorf("variable %s: %w", variableRef(v.Name, i), err)
		}
		if seen[v.Name] {
			return fmt.Errorf("variable %s: declared twice", v.Name)
		}
		seen[v.Name] = true
	}
	if m.Variables == nil {
		m.Variables = []Variable{}
	}
	return nil
}

func variableRef(name string, i int) string {
	if name == "" {
		return fmt.Sprintf("#%d", i+1)
	}
	return name
}

func (v *Variable) validate() error {
	v.Name = strings.TrimSpace(v.Name)
	if !varNameRe.MatchString(v.Name) {
		return errors.New("name must be letters, digits and underscores, starting with a letter or underscore")
	}
	if v.Label = strings.TrimSpace(v.Label); v.Label == "" {
		v.Label = v.Name
	}
	switch v.Generate {
	case "", GenPassword, GenHex32, GenUUID:
	default:
		// M18E: the shared generator's kinds with parameters ("password, 48", "base64key").
		spec, _, err := gen.ParseGenerate("@generate(" + v.Generate + ")")
		if err != nil || spec.Kind == gen.KindPort {
			return fmt.Errorf("unknown generate kind %q (use password, alnum, hex, base64, base64key, uuid or user)", v.Generate)
		}
	}
	if v.Pattern != "" {
		if len(v.Pattern) > maxPattern {
			return fmt.Errorf("pattern is longer than %d characters", maxPattern)
		}
		// Go regexps are RE2: linear time, so a pattern from a template can't hang the hub.
		// The pattern must match the whole value.
		re, err := regexp.Compile(`^(?:` + v.Pattern + `)$`)
		if err != nil {
			return fmt.Errorf("pattern doesn't compile: %w", err)
		}
		v.re = re
	}
	if v.Default != "" {
		if err := v.check(v.Default); err != nil {
			return fmt.Errorf("default: %w", err)
		}
	}
	return nil
}

// check validates one value against the variable's rules (not "required").
func (v *Variable) check(val string) error {
	if utf8.RuneCountInString(val) > maxValueRunes {
		return fmt.Errorf("at most %d characters", maxValueRunes)
	}
	// One line only: a newline would let a value add keys to compose.yaml or .env.
	if hasControl(val) {
		return errors.New("must be one line without control characters")
	}
	if v.re != nil && !v.re.MatchString(val) {
		return fmt.Errorf("doesn't match the pattern %s", v.Pattern)
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// NewTemplate checks the files of a template whose template.yaml is already parsed.
func NewTemplate(m Meta, compose, env string) (Template, error) {
	t := Template{Meta: m, Compose: compose, Env: env}
	switch {
	case strings.TrimSpace(compose) == "":
		return t, errors.New("compose.yaml is missing or empty")
	case len(compose) > MaxFileBytes:
		return t, errors.New("compose.yaml is larger than 1 MiB")
	case len(env) > MaxFileBytes:
		return t, errors.New(".env is larger than 1 MiB")
	case !utf8.ValidString(compose) || !utf8.ValidString(env):
		return t, errors.New("files must be UTF-8 text")
	}
	return t, nil
}
