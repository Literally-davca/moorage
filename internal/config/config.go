// Package config reads Moorage settings from environment variables.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Config holds runtime settings. All fields come from MOORAGE_* variables.
type Config struct {
	Listen        string // MOORAGE_LISTEN, default ":5080"
	DataDir       string // MOORAGE_DATA_DIR, default "/data"
	StacksDir     string // MOORAGE_STACKS_DIR, default "/opt/stacks"
	SecureCookies bool   // MOORAGE_SECURE_COOKIES, set true behind HTTPS
	PublicURL     string // MOORAGE_PUBLIC_URL, e.g. https://moorage.example.com, used for links in alerts
	PUID, PGID    int    // MOORAGE_PUID / MOORAGE_PGID, owner of written stack files (-1 = leave as is)

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
}

// FromEnv builds a Config and checks that the directories are usable.
func FromEnv() (Config, error) {
	c := Config{
		Listen:    env("MOORAGE_LISTEN", ":5080"),
		DataDir:   env("MOORAGE_DATA_DIR", "/data"),
		StacksDir: env("MOORAGE_STACKS_DIR", "/opt/stacks"),
		PUID:      -1,
		PGID:      -1,

		AgentListen:    env("MOORAGE_AGENT_LISTEN", ":5081"),
		StatusListen:   os.Getenv("MOORAGE_STATUS_LISTEN"),
		PublicURL:      os.Getenv("MOORAGE_PUBLIC_URL"),
		Hub:            os.Getenv("MOORAGE_HUB"),
		JoinToken:      os.Getenv("MOORAGE_JOIN_TOKEN"),
		WatchdogListen: os.Getenv("MOORAGE_WATCHDOG_LISTEN"),
		WatchdogAddr:   os.Getenv("MOORAGE_WATCHDOG_ADDR"),
	}
	if c.AgentListen == "off" {
		c.AgentListen = ""
	}
	var err error
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
