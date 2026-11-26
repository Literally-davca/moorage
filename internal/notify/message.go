package notify

import (
	"strings"
	"sync"
	"time"
)

// Message is one notification. Title and Text are the fixed wording composed in code;
// the structured parts below let each channel render a rich message and let a channel's
// preset decide what is shown (M17B). Text stays the plain-text body for compatibility.
type Message struct {
	Status string   `json:"status"` // down | up | test
	Title  string   `json:"title"`
	Text   string   `json:"text"`
	Logs   []string `json:"logs,omitempty"`
	URL    string   `json:"url,omitempty"`
	Data   any      `json:"data,omitempty"` // structured details for webhooks

	// Event is the event kind (EventChecks, EventAutoUpdate, …, EventWatchdog, EventTest).
	Event string `json:"event,omitempty"`
	// HostID is the Moorage host the event happened on ("" = the hub or several hosts);
	// the sender is resolved from it when Sender is empty.
	HostID string `json:"hostId,omitempty"`
	Sender Sender `json:"sender,omitzero"`
	// IconBase overrides the logo base URL for this message (the watchdog gets it from
	// watchdog.json); "" uses SetIconBase / the default.
	IconBase string `json:"-"`

	Host     string        `json:"host,omitempty"`  // host name the event is about
	Hosts    []string      `json:"hosts,omitempty"` // several hosts (grouped alerts)
	Stack    string        `json:"stack,omitempty"`
	Cause    string        `json:"cause,omitempty"`
	Duration time.Duration `json:"duration,omitempty"` // how long it was down (recoveries)
	Since    time.Time     `json:"since,omitzero"`
	Until    time.Time     `json:"until,omitzero"`
	Images   []ImageChange `json:"images,omitempty"`
	Checks   []CheckDetail `json:"checks,omitempty"`
	// Changes is what changed in the stack before an outage (M18A causal timeline).
	Changes []string `json:"changes,omitempty"`
	// LikelyCause is from the container diagnosis (M18F), e.g. "Out of memory
	// (exit 137)". Detailed preset only; never log lines.
	LikelyCause string `json:"likelyCause,omitempty"`
}

// Sender is who a message comes from: the affected host and the role of the Moorage
// process on it.
type Sender struct {
	Host string `json:"host,omitempty"`
	Role string `json:"role,omitempty"` // hub | agent | watchdog
}

// Sender roles.
const (
	RoleHub      = "hub"
	RoleAgent    = "agent"
	RoleWatchdog = "watchdog"
)

// Name is "<host> • Moorage <role>", or "Moorage <role>" without a host.
func (s Sender) Name() string {
	role := s.Role
	if role == "" {
		role = RoleHub
	}
	if h := strings.TrimSpace(s.Host); h != "" {
		return h + " • Moorage " + role
	}
	return "Moorage " + role
}

// ImageChange is one image replaced by an update or a rollback.
type ImageChange struct {
	Service string `json:"service,omitempty"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
}

func (c ImageChange) String() string {
	s := c.From + " → " + c.To
	switch {
	case c.From == "":
		s = c.To
	case c.To == "":
		s = c.From
	}
	if c.Service != "" {
		s = c.Service + ": " + s
	}
	return s
}

// CheckDetail is the detail of one check in a check alert.
type CheckDetail struct {
	Name      string           `json:"name"`
	Kind      string           `json:"kind,omitempty"`
	Target    string           `json:"target,omitempty"`
	Cause     string           `json:"cause,omitempty"`
	Locations []LocationDetail `json:"locations,omitempty"`
}

// LocationDetail is the answer of one location (agent) of a multi-location check.
type LocationDetail struct {
	Name      string `json:"name"`
	Vote      string `json:"vote"` // up | down | none
	LatencyMS int    `json:"latencyMs,omitempty"`
	Msg       string `json:"msg,omitempty"`
}

func (c CheckDetail) summary() string {
	var b strings.Builder
	what := strings.TrimSpace(c.Kind + " " + c.Target)
	b.WriteString(what)
	if c.Cause != "" {
		if what != "" {
			b.WriteString(": ")
		}
		b.WriteString(c.Cause)
	}
	for _, l := range c.Locations {
		b.WriteString("\n" + l.Name + " " + l.Vote)
		if l.Vote != "none" && l.LatencyMS > 0 {
			b.WriteString(" · " + itoa(l.LatencyMS) + " ms")
		}
		if l.Msg != "" {
			b.WriteString(" (" + l.Msg + ")")
		}
	}
	return b.String()
}

// ---- process-wide sender identity and logo -------------------------------------------

// DefaultIconBase hosts the Moorage logos (hub.png, agent.png, watchdog.png, 256×256).
// Settings → Notifications can point it elsewhere (own hosting, rebranding).
const DefaultIconBase = "https://cdn.jsdelivr.net/gh/Literally-davca/moorage@main/docs/assets/notify"

var (
	identityMu sync.RWMutex
	iconBase   = DefaultIconBase
	resolver   func(hostID string) Sender
)

// SetIconBase sets the base URL of the logos ("" = DefaultIconBase).
func SetIconBase(u string) {
	u = strings.TrimRight(strings.TrimSpace(u), "/")
	if u == "" {
		u = DefaultIconBase
	}
	identityMu.Lock()
	iconBase = u
	identityMu.Unlock()
}

// IconBase is the current logo base URL.
func IconBase() string {
	identityMu.RLock()
	defer identityMu.RUnlock()
	return iconBase
}

// SetSenderResolver tells the hub how to name senders: fn("") is the hub itself (events
// of the hub or of several hosts), fn(hostID) the host an event happened on.
func SetSenderResolver(fn func(hostID string) Sender) {
	identityMu.Lock()
	resolver = fn
	identityMu.Unlock()
}

// resolvedSender is the sender of m: explicit, from HostID, or the hub.
func (m Message) resolvedSender() Sender {
	if m.Sender.Role != "" || m.Sender.Host != "" {
		s := m.Sender
		if s.Role == "" {
			s.Role = RoleHub
		}
		return s
	}
	identityMu.RLock()
	fn := resolver
	identityMu.RUnlock()
	if fn != nil {
		if m.HostID != "" {
			if s := fn(m.HostID); s.Host != "" {
				if s.Role == "" {
					s.Role = RoleAgent
				}
				return s
			}
		}
		if s := fn(""); s.Host != "" {
			s.Role = RoleHub
			return s
		}
	}
	return Sender{Role: RoleHub}
}

// iconURL is the logo for the sender's role.
func (m Message) iconURL(s Sender) string {
	base := strings.TrimRight(m.IconBase, "/")
	if base == "" {
		base = IconBase()
	}
	role := s.Role
	if role != RoleAgent && role != RoleWatchdog {
		role = RoleHub
	}
	return base + "/" + role + ".png"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// HumanDuration is a short duration for messages ("45 s", "12 min", "3 h 5 min", "2 d 4 h").
func HumanDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return ""
	case d < time.Minute:
		return itoa(max(int(d.Seconds()), 1)) + " s"
	case d < time.Hour:
		return itoa(int(d.Minutes())) + " min"
	case d < 48*time.Hour:
		return itoa(int(d.Hours())) + " h " + itoa(int(d.Minutes())%60) + " min"
	default:
		return itoa(int(d.Hours())/24) + " d " + itoa(int(d.Hours())%24) + " h"
	}
}

// SampleMessage is the example used by "Send test" and the preview: every part is set so
// the preset of a channel shows what it keeps.
func SampleMessage(publicURL string) Message {
	now := time.Date(2026, 11, 26, 12, 0, 5, 0, time.UTC)
	return Message{
		Status: "test",
		Event:  EventTest,
		Title:  "Moorage test notification",
		Text:   "If you can read this, alerts from Moorage will reach you here. Real alerts look like this one.",
		Logs: []string{
			"12:00:01 web-1 | GET /health 503 Service Unavailable",
			"12:00:02 web-1 | upstream db:5432 refused the connection",
		},
		URL:      strings.TrimRight(publicURL, "/"),
		Host:     "nas",
		Stack:    "web",
		Cause:    "HTTP 503 from https://web.example.com/health",
		Duration: 4*time.Minute + 12*time.Second,
		Since:    now.Add(-4*time.Minute - 12*time.Second),
		Until:    now,
		Images:   []ImageChange{{Service: "app", From: "nginx:1.27.2", To: "nginx:1.27.3"}},
		Checks: []CheckDetail{{Name: "web", Kind: "HTTP", Target: "https://web.example.com/health", Cause: "HTTP 503",
			Locations: []LocationDetail{{Name: "nas", Vote: "down", LatencyMS: 120, Msg: "503"}, {Name: "vps", Vote: "down", LatencyMS: 85, Msg: "503"}}}},
	}
}
