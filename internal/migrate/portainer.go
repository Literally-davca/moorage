package migrate

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"

	"moorage/internal/netguard"
)

// Portainer CE import: the admin gives the Portainer URL and an access token, Moorage
// reads the stacks through the Portainer API and saves the chosen ones as compose.yaml +
// .env. The token lives only in this client for the duration of one request: it is never
// stored, logged, put in an error message or written to the audit log.

// MaxPortainerStacks caps one listing.
const MaxPortainerStacks = 200

// Response size limits: a stack list with Env and the per-stack file. A compose file over
// MaxComposeSize is refused (the stack save path accepts 512 KB).
const (
	maxListBytes   = 8 << 20
	maxFileBytes   = 2 << 20
	MaxComposeSize = 512 * 1024
)

// Portainer stack types.
const (
	PortainerSwarm      = 1
	PortainerCompose    = 2
	PortainerKubernetes = 3
)

// PortainerStack is one stack as GET /api/stacks returns it (fields the import reads).
type PortainerStack struct {
	ID         int    `json:"Id"`
	Name       string `json:"Name"`
	Type       int    `json:"Type"`
	EndpointID int    `json:"EndpointId"`
	Status     int    `json:"Status"` // 1 active, 2 inactive
	Env        []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"Env"`
	GitConfig *struct {
		URL string `json:"URL"`
	} `json:"GitConfig"`
}

// Portainer is a minimal read-only Portainer API client.
type Portainer struct {
	base  string
	token string
	http  *http.Client
}

// PortainerError is a refusal or failure worth showing to the admin as is.
type PortainerError struct{ Msg string }

func (e *PortainerError) Error() string { return e.Msg }

func perr(format string, a ...any) error { return &PortainerError{Msg: fmt.Sprintf(format, a...)} }

var tokenRe = regexp.MustCompile(`^[\x21-\x7e]{1,512}$`)

// NewPortainer checks the URL and token and builds a client whose connections go
// through netguard (no link-local / metadata addresses; LAN addresses are fine, Portainer
// usually lives there). insecure accepts a self-signed certificate (Portainer's default
// on :9443). Redirects are never followed, so the token can't be forwarded elsewhere.
func NewPortainer(rawURL, token string, insecure bool) (*Portainer, error) {
	base, err := normalizeBase(rawURL)
	if err != nil {
		return nil, err
	}
	token = strings.TrimSpace(token)
	if !tokenRe.MatchString(token) {
		return nil, perr("Paste a Portainer access token (My account → Access tokens).")
	}
	tr := &http.Transport{
		DialContext:           netguard.DialContext(10*time.Second, false),
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		MaxIdleConns:          2,
		IdleConnTimeout:       30 * time.Second,
	}
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit admin choice for self-signed Portainer
	}
	return newPortainerWith(base, token, &http.Client{Transport: tr, Timeout: 60 * time.Second}), nil
}

// newPortainerWith is used by tests with an httptest client.
func newPortainerWith(base, token string, c *http.Client) *Portainer {
	cp := *c
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Portainer{base: base, token: token, http: &cp}
}

// Close drops idle connections.
func (p *Portainer) Close() { p.http.CloseIdleConnections() }

func normalizeBase(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", perr("The Portainer URL must start with http:// or https://, for example https://portainer.lan:9443.")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", perr("Give the Portainer URL without user name, query or #fragment.")
	}
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/api")
	return strings.TrimRight(u.String(), "/"), nil
}

func (p *Portainer) get(ctx context.Context, path string, limit int64, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+path, nil)
	if err != nil {
		return perr("The Portainer URL can't be used.")
	}
	req.Header.Set("X-API-Key", p.token)
	req.Header.Set("Accept", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		var ge *netguard.Error
		if errors.As(err, &ge) {
			return perr("Portainer's address is blocked: %s.", ge.Reason)
		}
		if ctx.Err() != nil {
			return perr("Portainer didn't answer in time.")
		}
		return perr("Can't reach Portainer: %s.", cleanNetErr(err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return perr("Portainer refused the access token (HTTP %d). Create one under My account → Access tokens; an admin token sees every stack.", resp.StatusCode)
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return perr("Portainer answered with a redirect (HTTP %d). Use the address it redirects to, usually https://…:9443.", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return perr("No Portainer API at this address (HTTP 404).")
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return perr("Portainer returned HTTP %d.", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return perr("Reading Portainer's answer failed.")
	}
	if int64(len(body)) > limit {
		return perr("Portainer's answer is too large.")
	}
	if err := json.Unmarshal(body, v); err != nil {
		return perr("Portainer's answer isn't what Moorage expects. Is this a Portainer CE 2.x URL?")
	}
	return nil
}

// cleanNetErr keeps the reason of a transport error without the full URL.
func cleanNetErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return err.Error()
}

// Stacks lists every stack the token can see.
func (p *Portainer) Stacks(ctx context.Context) ([]PortainerStack, error) {
	var out []PortainerStack
	if err := p.get(ctx, "/api/stacks", maxListBytes, &out); err != nil {
		return nil, err
	}
	if len(out) > MaxPortainerStacks {
		return nil, perr("Portainer lists %d stacks; at most %d can be imported at once.", len(out), MaxPortainerStacks)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Endpoints returns environment names by id (best effort for the preview).
func (p *Portainer) Endpoints(ctx context.Context) map[int]string {
	var list []struct {
		ID   int    `json:"Id"`
		Name string `json:"Name"`
	}
	if err := p.get(ctx, "/api/endpoints?excludeSnapshots=true", maxListBytes, &list); err != nil {
		return map[int]string{}
	}
	out := make(map[int]string, len(list))
	for _, e := range list {
		out[e.ID] = e.Name
	}
	return out
}

// StackFile returns the compose file of a stack ("" when Portainer has none).
func (p *Portainer) StackFile(ctx context.Context, id int) (string, error) {
	var f struct {
		StackFileContent string `json:"StackFileContent"`
	}
	if err := p.get(ctx, "/api/stacks/"+strconv.Itoa(id)+"/file", maxFileBytes, &f); err != nil {
		return "", err
	}
	return f.StackFileContent, nil
}

// ---- planning ---------------------------------------------------------------

// StackPreview is one Portainer stack in the import preview.
type StackPreview struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`   // as in Portainer
	Target   string `json:"target"` // proposed Moorage stack name
	Endpoint string `json:"endpoint"`
	Type     string `json:"type"` // compose | swarm | kubernetes | unknown
	Git      bool   `json:"git"`
	Active   bool   `json:"active"`
	// Status: ready, exists (a managed stack has the name), unsupported, invalid.
	Status   string   `json:"status"`
	Reason   string   `json:"reason,omitempty"`
	Warnings []string `json:"warnings"`
	Compose  string   `json:"compose,omitempty"`
	EnvKeys  []string `json:"envKeys"`
}

// Preview statuses.
const (
	StatusReady       = "ready"
	StatusExists      = "exists"
	StatusUnsupported = "unsupported"
	StatusInvalid     = "invalid"
)

var stackNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ValidStackName reports whether name is a valid Moorage stack (compose project) name.
func ValidStackName(name string) bool { return stackNameRe.MatchString(name) }

// StackName turns a Portainer stack name into a valid Moorage stack name ("" if impossible).
func StackName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	s := strings.TrimLeft(b.String(), "_-")
	if len(s) > 63 {
		s = s[:63]
	}
	s = strings.TrimRight(s, "_-")
	if !stackNameRe.MatchString(s) {
		return ""
	}
	return s
}

func stackType(t int) string {
	switch t {
	case PortainerCompose:
		return "compose"
	case PortainerSwarm:
		return "swarm"
	case PortainerKubernetes:
		return "kubernetes"
	}
	return "unknown"
}

// Unsupported says why a stack can't be imported at all ("" when it can, as far as the
// listing shows).
func (s PortainerStack) Unsupported() string {
	switch s.Type {
	case PortainerCompose:
		return ""
	case PortainerSwarm:
		return "Swarm stacks aren't supported: Moorage runs Compose stacks only."
	case PortainerKubernetes:
		return "Kubernetes stacks aren't supported."
	}
	return "Unknown Portainer stack type."
}

// PreviewStack builds the preview of a compose stack from its file (fileErr is the error
// of fetching it). managed are the stack names Moorage already manages on the target host,
// running the compose projects that run there without being managed (Portainer's own).
func PreviewStack(s PortainerStack, endpoint, file string, fileErr error, managed, running map[string]bool) StackPreview {
	pv := StackPreview{
		ID: s.ID, Name: s.Name, Endpoint: endpoint, Type: stackType(s.Type), Git: s.GitConfig != nil,
		Active: s.Status == 1, Target: StackName(s.Name), Status: StatusReady, Warnings: []string{}, EnvKeys: []string{},
	}
	for _, e := range s.Env {
		pv.EnvKeys = append(pv.EnvKeys, e.Name)
	}
	if why := s.Unsupported(); why != "" {
		pv.Status, pv.Reason = StatusUnsupported, why
		return pv
	}
	switch {
	case fileErr != nil:
		pv.Status, pv.Reason = StatusUnsupported, "Portainer didn't return the compose file: "+fileErr.Error()
		return pv
	case strings.TrimSpace(file) == "" && pv.Git:
		pv.Status, pv.Reason = StatusUnsupported, "Git-based stack without a stored compose file. Clone the repository and create the stack by hand."
		return pv
	case strings.TrimSpace(file) == "":
		pv.Status, pv.Reason = StatusUnsupported, "Portainer returned an empty compose file."
		return pv
	case len(file) > MaxComposeSize:
		pv.Status, pv.Reason = StatusUnsupported, "The compose file is larger than 512 KB."
		return pv
	}
	pv.Compose = file
	if pv.Git {
		pv.Warnings = append(pv.Warnings, "Deployed from Git in Portainer: Moorage saves the current file only, the repository link isn't kept.")
	}
	pv.Warnings = append(pv.Warnings, ComposeWarnings(file)...)
	if _, envWarn := EnvFile(s); len(envWarn) > 0 {
		pv.Warnings = append(pv.Warnings, envWarn...)
	}
	switch {
	case pv.Target == "":
		pv.Status, pv.Reason = StatusInvalid, "The name can't be turned into a stack name; pick one."
	case managed[pv.Target]:
		pv.Status, pv.Reason = StatusExists, "A stack with this name already exists in Moorage; pick another name."
	case running[pv.Target]:
		pv.Warnings = append(pv.Warnings, "Its containers run now as project "+pv.Target+" (started by Portainer). Stop the stack in Portainer before you deploy it from Moorage.")
	}
	if pv.Target != "" && pv.Target != s.Name {
		pv.Warnings = append(pv.Warnings, "Renamed to "+pv.Target+" (stack names are lowercase letters, digits, - and _).")
	}
	return pv
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// EnvFile renders the stack's Portainer environment variables as a .env file, the place
// Moorage keeps stack secrets. Invalid names are left out with a warning.
func EnvFile(s PortainerStack) (string, []string) {
	var b strings.Builder
	var warn []string
	seen := map[string]bool{}
	for _, e := range s.Env {
		k := strings.TrimSpace(e.Name)
		if !envKeyRe.MatchString(k) {
			warn = append(warn, fmt.Sprintf("Environment variable %q has an invalid name and isn't imported.", clip(k, 40)))
			continue
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		b.WriteString(k + "=" + formatEnvValue(e.Value) + "\n")
	}
	return b.String(), warn
}

// formatEnvValue mirrors lint.formatEnvValue / web/src/lib/dotenv.ts formatValue.
func formatEnvValue(v string) string {
	if v == "" {
		return ""
	}
	if !strings.ContainsAny(v, " \t\n\r#\"'\\$`") {
		return v
	}
	if !strings.ContainsAny(v, "'\n\r") {
		return "'" + v + "'"
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "$", `\$`).Replace(v) + `"`
}

// ComposeWarnings points out what a compose file from Portainer needs besides itself:
// files next to it (build contexts, env_file, relative bind mounts and configs), which
// live in Portainer's data folder and aren't part of the API.
func ComposeWarnings(compose string) []string {
	var doc struct {
		Services map[string]struct {
			Build   any   `yaml:"build"`
			EnvFile any   `yaml:"env_file"`
			Volumes []any `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(compose), &doc); err != nil {
		return nil // the save path reports parse errors
	}
	var warn []string
	names := make([]string, 0, len(doc.Services))
	for n := range doc.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	var rel, build, envFile, stackEnv []string
	for _, n := range names {
		svc := doc.Services[n]
		if svc.Build != nil {
			build = append(build, n)
		}
		for _, f := range envFiles(svc.EnvFile) {
			if strings.TrimPrefix(f, "./") == "stack.env" {
				stackEnv = append(stackEnv, n)
			} else {
				envFile = append(envFile, n)
			}
		}
		for _, v := range svc.Volumes {
			src := ""
			switch vv := v.(type) {
			case string:
				src, _, _ = strings.Cut(vv, ":")
			case map[string]any:
				src, _ = vv["source"].(string)
			}
			if strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../") || src == "." {
				rel = append(rel, n)
				break
			}
		}
	}
	if len(stackEnv) > 0 {
		warn = append(warn, "env_file: stack.env ("+strings.Join(stackEnv, ", ")+"): Moorage writes the variables to .env, change env_file to .env before deploying.")
	}
	if len(envFile) > 0 {
		warn = append(warn, "env_file ("+strings.Join(envFile, ", ")+"): copy those files into the stack folder.")
	}
	if len(build) > 0 {
		warn = append(warn, "build ("+strings.Join(build, ", ")+"): copy the build context into the stack folder.")
	}
	if len(rel) > 0 {
		warn = append(warn, "Relative bind mounts ("+strings.Join(rel, ", ")+"): they pointed into Portainer's data folder; copy the data into the stack folder first.")
	}
	return warn
}

func envFiles(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, x := range t {
			switch y := x.(type) {
			case string:
				out = append(out, y)
			case map[string]any:
				if p, ok := y["path"].(string); ok {
					out = append(out, p)
				}
			}
		}
		return out
	}
	return nil
}
