// Package config reads Moorage settings from environment variables.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Config holds runtime settings. All fields come from MOORAGE_* variables.
type Config struct {
	Listen           string // MOORAGE_LISTEN, default ":5080"
	DataDir          string // MOORAGE_DATA_DIR, default "/data"
	StacksDir        string // MOORAGE_STACKS_DIR, default "/opt/stacks"
	SecureCookies    bool   // MOORAGE_SECURE_COOKIES, auto-detected from HTTPS when unset
	SecureCookiesSet bool   // true when MOORAGE_SECURE_COOKIES was explicitly set in env
	PublicURL        string // MOORAGE_PUBLIC_URL, e.g. https://moorage.example.com, used for links in alerts
	PublicURLSet     bool   // true when MOORAGE_PUBLIC_URL was set in env (otherwise loaded from Settings at start)
	PUID, PGID       int    // MOORAGE_PUID / MOORAGE_PGID, owner of written stack files (-1 = leave as is)

	// hub
	AgentListen  string // MOORAGE_AGENT_LISTEN, default ":5081"; "off" disables pairing agents
	StatusListen string // MOORAGE_STATUS_LISTEN, optional separate listen address for public status pages only (e.g. ":5085")
	MetricsToken string // MOORAGE_METRICS_TOKEN, enables GET /metrics for Prometheus with this bearer token (min 16 chars)
	TemplatesDir string // MOORAGE_TEMPLATES_DIR, local stack templates (one subfolder each), default <data dir>/templates

	// agent
	Hub            string // MOORAGE_HUB, host:port of the hub's agent port
	JoinToken      string // MOORAGE_JOIN_TOKEN, needed for the first start only
	ReadOnly       bool   // MOORAGE_READ_ONLY, refuse every change on this host
	Terminal       bool   // MOORAGE_TERMINAL, allow interactive container shells on this host (default false)
	WatchdogListen string // MOORAGE_WATCHDOG_LISTEN, optional peer mTLS listen address, e.g. ":5082"
	WatchdogAddr   string // MOORAGE_WATCHDOG_ADDR, host:port other agents dial to reach this agent's watchdog
	// Probes is MOORAGE_PROBES (default true): run HTTP/TCP/ping/DNS checks for the hub from
	// this host's network. Allowed on a read-only agent, because a probe changes nothing here.
	Probes bool

	// Self-update (MOORAGE_UPDATE_URL, MOORAGE_UPDATE_CHANNEL, MOORAGE_SELF_UPDATE).
	UpdateURL        string // MOORAGE_UPDATE_URL, default GitHub releases API of Literally-davca/moorage
	UpdateChannel    string // MOORAGE_UPDATE_CHANNEL: "stable" (default) or "pre"
	UpdateChannelSet bool   // true when MOORAGE_UPDATE_CHANNEL was explicitly set in env
	SelfUpdate       bool   // MOORAGE_SELF_UPDATE (default true), enforced on each node

	// OIDC sign-in (MOORAGE_OIDC_*). A broken setting never stops Moorage: it is kept in
	// OIDC.Err, logged, and shown to admins, and the SSO button stays hidden.
	OIDC OIDCConfig
	// Forward-auth from a trusted reverse proxy (MOORAGE_TRUSTED_PROXIES, MOORAGE_PROXY_*).
	// Off when Trusted is empty. A broken setting stops Moorage, because guessing what a
	// security setting meant would be worse than not starting.
	Proxy ProxyConfig

	// Host security locks. They are env only on the machine itself: never settable from
	// the web, so a compromised account can't widen what a host allows.
	ManageExternal  bool     // MOORAGE_MANAGE_EXTERNAL: control compose projects and containers outside the stacks folder
	ExtraStacksDirs []string // MOORAGE_EXTRA_STACKS_DIRS: more folders with fully managed stacks (":"-separated)
	HostTerminal    bool     // MOORAGE_HOST_TERMINAL: a shell in the host's OS (never with MOORAGE_READ_ONLY)
	Exec            bool     // MOORAGE_EXEC: run commands in containers for scheduled tasks

	// Network addresses of this hub and host (Settings shows them; env wins over the UI).
	StatusPublicURL string // MOORAGE_STATUS_PUBLIC_URL
	AgentPublicAddr string // MOORAGE_AGENT_PUBLIC_ADDR, host:port agents dial (prefills MOORAGE_HUB)
	HostIP          string // MOORAGE_HOST_IP, primary IP or host name of this host (service links)
	BaseDomain      string // MOORAGE_BASE_DOMAIN, prefilled domain for templates on this host

	// Env reads every MOORAGE_* variable of the process (for "Set via environment" in
	// Settings). Values of secret variables are never sent to the browser.
	Env map[string]string
}

// LockedVars are the host security locks that Settings shows but can never change.
var LockedVars = []string{"MOORAGE_READ_ONLY", "MOORAGE_MANAGE_EXTERNAL", "MOORAGE_EXTRA_STACKS_DIRS", "MOORAGE_TERMINAL",
	"MOORAGE_HOST_TERMINAL", "MOORAGE_EXEC", "MOORAGE_TRUSTED_PROXIES", "MOORAGE_LISTEN"}

// RoleMapping turns external groups into a Moorage role: admin groups win over viewer
// groups, then the default role ("none" refuses the sign-in, or "viewer").
type RoleMapping struct {
	AdminGroups  []string
	ViewerGroups []string
	DefaultRole  string // "none" or "viewer"
}

// OIDCConfig is MOORAGE_OIDC_* as read; the sso package validates it.
type OIDCConfig struct {
	Issuer       string   // MOORAGE_OIDC_ISSUER
	ClientID     string   // MOORAGE_OIDC_CLIENT_ID
	ClientSecret string   // MOORAGE_OIDC_CLIENT_SECRET or the file in MOORAGE_OIDC_CLIENT_SECRET_FILE
	Scopes       []string // MOORAGE_OIDC_SCOPES, default "openid profile email groups"
	GroupsClaim  string   // MOORAGE_OIDC_GROUPS_CLAIM, default "groups"; a dotted path for nested claims
	Roles        RoleMapping
	Label        string // MOORAGE_OIDC_LABEL, default "Sign in with SSO"
	LocalLogin   bool   // MOORAGE_OIDC_LOCAL_LOGIN, default true
	Err          string // why the settings can't be used, "" when fine
}

// Configured reports whether any OIDC setting is present.
func (c OIDCConfig) Configured() bool { return c.Issuer != "" || c.ClientID != "" }

// ProxyConfig is the forward-auth setup.
type ProxyConfig struct {
	Trusted      []netip.Prefix // MOORAGE_TRUSTED_PROXIES
	UserHeader   string         // MOORAGE_PROXY_USER_HEADER
	GroupsHeader string         // MOORAGE_PROXY_GROUPS_HEADER, optional
	Roles        RoleMapping    // MOORAGE_PROXY_ADMIN_GROUPS / _VIEWER_GROUPS / _DEFAULT_ROLE
}

// DefaultUpdateURL points at the upstream Moorage GitHub releases API.
const DefaultUpdateURL = "https://api.github.com/repos/Literally-davca/moorage/releases"

// FromEnv builds a Config and checks that the directories are usable.
func FromEnv() (Config, error) {
	c := Config{
		Listen:    env("MOORAGE_LISTEN", ":5080"),
		DataDir:   env("MOORAGE_DATA_DIR", "/data"),
		StacksDir: env("MOORAGE_STACKS_DIR", "/opt/stacks"),
		PUID:      -1,
		PGID:      -1,

		AgentListen:      env("MOORAGE_AGENT_LISTEN", ":5081"),
		StatusListen:     os.Getenv("MOORAGE_STATUS_LISTEN"),
		PublicURL:        os.Getenv("MOORAGE_PUBLIC_URL"),
		Hub:              os.Getenv("MOORAGE_HUB"),
		JoinToken:        os.Getenv("MOORAGE_JOIN_TOKEN"),
		WatchdogListen:   env("MOORAGE_WATCHDOG_LISTEN", ":5082"),
		WatchdogAddr:     os.Getenv("MOORAGE_WATCHDOG_ADDR"),
		UpdateURL:        strings.TrimRight(env("MOORAGE_UPDATE_URL", DefaultUpdateURL), "/"),
		UpdateChannel:    strings.ToLower(strings.TrimSpace(env("MOORAGE_UPDATE_CHANNEL", "stable"))),
		UpdateChannelSet: strings.TrimSpace(os.Getenv("MOORAGE_UPDATE_CHANNEL")) != "",
		PublicURLSet:     strings.TrimSpace(os.Getenv("MOORAGE_PUBLIC_URL")) != "",
	}
	if c.WatchdogListen == "off" {
		c.WatchdogListen = ""
	}
	if c.UpdateChannel != "stable" && c.UpdateChannel != "pre" {
		return c, fmt.Errorf("MOORAGE_UPDATE_CHANNEL must be stable or pre, not %q", c.UpdateChannel)
	}
	if c.AgentListen == "off" {
		c.AgentListen = ""
	}
	var err error
	if strings.TrimSpace(os.Getenv("MOORAGE_SECURE_COOKIES")) != "" {
		c.SecureCookiesSet = true
	}
	if c.SecureCookies, err = boolEnv("MOORAGE_SECURE_COOKIES"); err != nil {
		return c, err
	}
	if c.ReadOnly, err = boolEnv("MOORAGE_READ_ONLY"); err != nil {
		return c, err
	}
	if c.Terminal, err = boolEnv("MOORAGE_TERMINAL"); err != nil {
		return c, err
	}
	if c.Probes, err = boolEnvDefault("MOORAGE_PROBES", true); err != nil {
		return c, err
	}
	if c.SelfUpdate, err = boolEnvDefault("MOORAGE_SELF_UPDATE", true); err != nil {
		return c, err
	}
	if c.PUID, err = intEnv("MOORAGE_PUID", -1); err != nil {
		return c, err
	}
	if c.PGID, err = intEnv("MOORAGE_PGID", -1); err != nil {
		return c, err
	}
	if (c.PUID < 0) != (c.PGID < 0) {
		return c, fmt.Errorf("MOORAGE_PUID and MOORAGE_PGID must be set together")
	}
	if c.MetricsToken, err = metricsToken(os.Getenv("MOORAGE_METRICS_TOKEN")); err != nil {
		return c, err
	}
	if c.ManageExternal, err = boolEnv("MOORAGE_MANAGE_EXTERNAL"); err != nil {
		return c, err
	}
	if c.HostTerminal, err = boolEnv("MOORAGE_HOST_TERMINAL"); err != nil {
		return c, err
	}
	if c.Exec, err = boolEnv("MOORAGE_EXEC"); err != nil {
		return c, err
	}
	if c.ReadOnly {
		// read-only wins over every lock that would allow a change
		c.ManageExternal, c.HostTerminal, c.Exec = false, false, false
	}
	for _, d := range filepath.SplitList(os.Getenv("MOORAGE_EXTRA_STACKS_DIRS")) {
		if d = strings.TrimSpace(d); d == "" {
			continue
		}
		abs, err := filepath.Abs(d)
		if err != nil {
			return c, fmt.Errorf("MOORAGE_EXTRA_STACKS_DIRS: %w", err)
		}
		c.ExtraStacksDirs = append(c.ExtraStacksDirs, filepath.Clean(abs))
	}
	c.StatusPublicURL = strings.TrimRight(os.Getenv("MOORAGE_STATUS_PUBLIC_URL"), "/")
	c.AgentPublicAddr = strings.TrimSpace(os.Getenv("MOORAGE_AGENT_PUBLIC_ADDR"))
	c.HostIP = strings.TrimSpace(os.Getenv("MOORAGE_HOST_IP"))
	c.BaseDomain = strings.TrimSpace(os.Getenv("MOORAGE_BASE_DOMAIN"))
	c.Env = map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "MOORAGE_") {
			c.Env[k] = v
		}
	}
	c.OIDC = oidcFromEnv()
	if c.Proxy, err = proxyFromEnv(); err != nil {
		return c, err
	}

	for _, d := range []*string{&c.DataDir, &c.StacksDir} {
		abs, err := filepath.Abs(*d)
		if err != nil {
			return c, err
		}
		*d = abs
		if err := os.MkdirAll(abs, 0o750); err != nil {
			return c, fmt.Errorf("create %s: %w", abs, err)
		}
	}
	// The templates folder is not created: it is the admin's, and a missing folder only
	// means no local templates (the catalogue says so).
	c.TemplatesDir = env("MOORAGE_TEMPLATES_DIR", filepath.Join(c.DataDir, "templates"))
	if c.TemplatesDir, err = filepath.Abs(c.TemplatesDir); err != nil {
		return c, fmt.Errorf("MOORAGE_TEMPLATES_DIR: %w", err)
	}
	return c, nil
}

// MinMetricsToken is the shortest accepted MOORAGE_METRICS_TOKEN. The endpoint has no
// rate limit, so the token itself must be too long to guess.
const MinMetricsToken = 16

// metricsToken validates MOORAGE_METRICS_TOKEN. Empty means the endpoint is off.
func metricsToken(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if len(v) < MinMetricsToken {
		return "", fmt.Errorf("MOORAGE_METRICS_TOKEN must be at least %d characters", MinMetricsToken)
	}
	// It travels in an HTTP header: whitespace or control characters would never match
	// what Prometheus sends, so refuse them instead of failing every scrape.
	for _, r := range v {
		if r <= ' ' || r == 0x7f {
			return "", fmt.Errorf("MOORAGE_METRICS_TOKEN must not contain spaces or control characters")
		}
	}
	return v, nil
}

// oidcFromEnv reads MOORAGE_OIDC_*. Problems go to Err instead of failing the start.
func oidcFromEnv() OIDCConfig {
	c := OIDCConfig{
		Issuer:       strings.TrimSpace(os.Getenv("MOORAGE_OIDC_ISSUER")),
		ClientID:     strings.TrimSpace(os.Getenv("MOORAGE_OIDC_CLIENT_ID")),
		ClientSecret: os.Getenv("MOORAGE_OIDC_CLIENT_SECRET"),
		Scopes:       strings.Fields(env("MOORAGE_OIDC_SCOPES", "openid profile email groups")),
		GroupsClaim:  strings.TrimSpace(env("MOORAGE_OIDC_GROUPS_CLAIM", "groups")),
		Label:        strings.TrimSpace(env("MOORAGE_OIDC_LABEL", "Sign in with SSO")),
		LocalLogin:   true,
	}
	var errs []string
	if f := os.Getenv("MOORAGE_OIDC_CLIENT_SECRET_FILE"); f != "" {
		if c.ClientSecret != "" {
			errs = append(errs, "set MOORAGE_OIDC_CLIENT_SECRET or MOORAGE_OIDC_CLIENT_SECRET_FILE, not both")
		}
		b, err := os.ReadFile(f)
		if err != nil {
			errs = append(errs, fmt.Sprintf("MOORAGE_OIDC_CLIENT_SECRET_FILE: %v", err))
		}
		c.ClientSecret = strings.TrimSpace(string(b))
	}
	if !slices.Contains(c.Scopes, "openid") {
		c.Scopes = append([]string{"openid"}, c.Scopes...)
	}
	var err error
	if c.Roles, err = roleMapping("MOORAGE_OIDC"); err != nil {
		errs = append(errs, err.Error())
	}
	if c.LocalLogin, err = boolEnvDefault("MOORAGE_OIDC_LOCAL_LOGIN", true); err != nil {
		c.LocalLogin = true // the safe reading of a typo: keep the password form
		errs = append(errs, err.Error())
	}
	c.Err = strings.Join(errs, "; ")
	return c
}

// proxyFromEnv reads the forward-auth settings.
func proxyFromEnv() (ProxyConfig, error) {
	var c ProxyConfig
	for _, f := range strings.FieldsFunc(os.Getenv("MOORAGE_TRUSTED_PROXIES"), func(r rune) bool { return r == ',' || r == ' ' }) {
		p, err := parsePrefix(f)
		if err != nil {
			return c, fmt.Errorf("MOORAGE_TRUSTED_PROXIES: %q is not an IP address or CIDR range", f)
		}
		c.Trusted = append(c.Trusted, p)
	}
	c.UserHeader = strings.TrimSpace(os.Getenv("MOORAGE_PROXY_USER_HEADER"))
	c.GroupsHeader = strings.TrimSpace(os.Getenv("MOORAGE_PROXY_GROUPS_HEADER"))
	var err error
	if c.Roles, err = roleMapping("MOORAGE_PROXY"); err != nil {
		return c, err
	}
	if len(c.Trusted) > 0 && c.UserHeader == "" {
		return c, fmt.Errorf("MOORAGE_TRUSTED_PROXIES is set: also set MOORAGE_PROXY_USER_HEADER, for example Remote-User")
	}
	return c, nil
}

// parsePrefix accepts a CIDR range or a single address.
func parsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		return p.Masked(), err
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// roleMapping reads <prefix>_ADMIN_GROUPS, _VIEWER_GROUPS and _DEFAULT_ROLE.
func roleMapping(prefix string) (RoleMapping, error) {
	m := RoleMapping{
		AdminGroups:  splitList(os.Getenv(prefix + "_ADMIN_GROUPS")),
		ViewerGroups: splitList(os.Getenv(prefix + "_VIEWER_GROUPS")),
		DefaultRole:  strings.ToLower(strings.TrimSpace(env(prefix+"_DEFAULT_ROLE", "none"))),
	}
	if m.DefaultRole != "none" && m.DefaultRole != "viewer" {
		bad := m.DefaultRole
		m.DefaultRole = "none"
		return m, fmt.Errorf("%s_DEFAULT_ROLE must be none or viewer, not %q", prefix, bad)
	}
	return m, nil
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func boolEnv(key string) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

// boolEnvDefault is boolEnv with a default for an unset or empty variable.
func boolEnvDefault(key string, def bool) (bool, error) {
	if os.Getenv(key) == "" {
		return def, nil
	}
	return boolEnv(key)
}

func intEnv(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}
