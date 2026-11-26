// Package migrate converts what other tools export into Moorage objects (M18C):
// Portainer CE stacks (through its API), Uptime Kuma JSON backups and Gatus YAML
// configs. Converters only build a plan; the API layer validates every item with the
// same rules as the UI forms and saves what the admin selected. Nothing here deploys,
// stores secrets elsewhere than the plan, or logs secret values.
package migrate

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"moorage/internal/monitor"
	"moorage/internal/notify"
)

// Sources of a monitor import.
const (
	SourceKuma  = "uptime-kuma"
	SourceGatus = "gatus"
)

// MaxUpload is the largest Uptime Kuma backup or Gatus config accepted (5 MiB). A Kuma
// backup with a few hundred monitors is well under 1 MiB.
const MaxUpload = 5 << 20

// Item limits keep a hostile file from creating thousands of rows.
const (
	MaxChecks   = 500
	MaxChannels = 100
)

// Default check timing when the source has none (matches the check form defaults).
const (
	defaultInterval = 60
	defaultTimeout  = 10
	defaultRetries  = 1
)

// Plan is what a monitor import would create.
type Plan struct {
	Source   string        `json:"source"`
	Checks   []CheckItem   `json:"checks"`
	Channels []ChannelItem `json:"channels"`
	Pages    []PageItem    `json:"pages"`
	// Skipped lists everything in the file that has no Moorage counterpart.
	Skipped []Skipped `json:"skipped"`
	// Notes are file-wide remarks (e.g. "status pages are not part of a Kuma backup").
	Notes []string `json:"notes"`
}

// CheckItem is one check to create.
type CheckItem struct {
	Key      string           `json:"key"`
	Name     string           `json:"name"`
	Group    string           `json:"group,omitempty"`
	Kind     string           `json:"kind"`
	Target   string           `json:"target"`
	Interval int              `json:"interval"`
	Timeout  int              `json:"timeout"`
	Retries  int              `json:"retries"`
	Settings monitor.Settings `json:"settings"`
	Enabled  bool             `json:"enabled"`
	// Channels are keys of ChannelItems in the same plan.
	Channels []string `json:"channels"`
	Warnings []string `json:"warnings"`
}

// ChannelItem is one notification channel to create. Config holds the secrets from the
// file; it never leaves the server (the preview DTO shows Summary instead).
type ChannelItem struct {
	Key      string        `json:"key"`
	Name     string        `json:"name"`
	Kind     string        `json:"kind"`
	Config   notify.Config `json:"-"`
	Summary  string        `json:"summary"`
	Enabled  bool          `json:"enabled"`
	Warnings []string      `json:"warnings"`
}

// PageItem is one status page to create (always unpublished).
type PageItem struct {
	Key         string      `json:"key"`
	Title       string      `json:"title"`
	Slug        string      `json:"slug"`
	Description string      `json:"description,omitempty"`
	Entries     []PageEntry `json:"entries"`
}

// PageEntry is one row of a status page: a check of the same plan, by key.
type PageEntry struct {
	Section     string `json:"section,omitempty"`
	DisplayName string `json:"displayName"`
	CheckKey    string `json:"checkKey"`
}

// Skipped is something that couldn't be converted.
type Skipped struct {
	Kind   string `json:"kind"` // check | channel | page | setting
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

func (p *Plan) skip(kind, name, reason string) {
	p.Skipped = append(p.Skipped, Skipped{Kind: kind, Name: clip(name, 100), Reason: reason})
}

// ErrInvalid is returned for a file that isn't what the importer expects.
var ErrInvalid = errors.New("invalid file")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// clip shortens s to n runes and trims it.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return strings.TrimSpace(string(r[:n]))
	}
	return s
}

// clamp keeps v within [lo, hi].
func clamp(v, lo, hi int) int {
	return max(lo, min(v, hi))
}

// timing returns a valid interval/timeout pair (interval 20 s–24 h, timeout 1–60 s and
// shorter than the interval).
func timing(interval, timeout int) (int, int) {
	if interval <= 0 {
		interval = defaultInterval
	}
	interval = clamp(interval, 20, 86400)
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	timeout = clamp(timeout, 1, 60)
	if timeout >= interval {
		timeout = interval - 1
	}
	return interval, timeout
}

// simpleJSONPath is the path syntax HTTP checks understand: data.items[0].status.
var simpleJSONPath = regexp.MustCompile(`^[A-Za-z_$][\w$-]*(\[\d+\])*(\.[A-Za-z_$][\w$-]*(\[\d+\])*)*$`)

// urlHost returns the host of a URL for previews (no path, no query: those may carry secrets).
func urlHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Hostname()
}

// channelSummary is a harmless one-line description of a channel config.
func channelSummary(kind string, c notify.Config) string {
	switch kind {
	case "smtp":
		return fmt.Sprintf("%s:%d → %s", c.Host, c.Port, c.To)
	case "telegram":
		return "chat " + c.ChatID
	case "pushover":
		return "Pushover"
	case "matrix":
		return urlHost(c.URL) + " " + c.ChatID
	}
	if h := urlHost(c.URL); h != "" {
		return h
	}
	return kind
}

// hasEnvRef reports a ${VAR} reference the importer can't resolve.
func hasEnvRef(s string) bool { return strings.Contains(s, "${") }
