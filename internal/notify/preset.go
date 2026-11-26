package notify

import (
	"fmt"
	"strings"
	"time"
)

// Content presets of a channel (Config.Preset).
const (
	PresetMinimal  = "minimal"  // status, title, host, link
	PresetStandard = "standard" // + stack, cause, duration, last 20 log lines (the default, as before M17B)
	PresetDetailed = "detailed" // + image change, check details, start and end time
	PresetCustom   = "custom"   // Config.Include picks the parts
)

// Formats of a channel (Config.Format).
const (
	FormatRich  = "rich"  // embeds / HTML / Markdown where the service supports it
	FormatPlain = "plain" // plain text only
)

// Message parts a Custom preset can switch (Config.Include keys). Status and title are
// always shown.
var PartKeys = []string{"host", "stack", "cause", "duration", "logs", "image", "checkDetails", "link", "timestamps"}

// Parts says which parts of a message a channel shows.
type Parts struct {
	Host, Stack, Cause, Duration, Logs, Image, CheckDetails, Link, Timestamps bool
}

// MaxLogLines is how many log lines the Standard and Detailed presets attach.
const MaxLogLines = 20

// Parts resolves the preset (missing = Standard).
func (c Config) Parts() Parts {
	switch c.Preset {
	case PresetMinimal:
		return Parts{Host: true, Link: true}
	case PresetDetailed:
		return Parts{Host: true, Stack: true, Cause: true, Duration: true, Logs: true, Image: true, CheckDetails: true, Link: true, Timestamps: true}
	case PresetCustom:
		in := c.Include
		return Parts{Host: in["host"], Stack: in["stack"], Cause: in["cause"], Duration: in["duration"], Logs: in["logs"],
			Image: in["image"], CheckDetails: in["checkDetails"], Link: in["link"], Timestamps: in["timestamps"]}
	}
	return Parts{Host: true, Stack: true, Cause: true, Duration: true, Logs: true, Link: true}
}

// richKinds can render rich messages; the others ignore Config.Format.
var richKinds = map[string]bool{"discord": true, "slack": true, "telegram": true, "matrix": true, "gotify": true, "ntfy": true, "pushover": true, "smtp": true}

// SupportsRich reports whether a channel kind has a rich format (the UI shows the switch).
func SupportsRich(kind string) bool { return richKinds[kind] }

func (c Config) rich(kind string) bool { return richKinds[kind] && c.Format != FormatPlain }

func validatePresentation(c Config) error {
	switch c.Preset {
	case "", PresetMinimal, PresetStandard, PresetDetailed, PresetCustom:
	default:
		return fmt.Errorf("unknown preset %q", c.Preset)
	}
	switch c.Format {
	case "", FormatRich, FormatPlain:
	default:
		return fmt.Errorf("format must be rich or plain")
	}
	for k := range c.Include {
		known := false
		for _, p := range PartKeys {
			known = known || p == k
		}
		if !known {
			return fmt.Errorf("unknown message part %q", k)
		}
	}
	return nil
}

// ---- the rendered view, shared by all channels --------------------------------------

// Field is one labelled value of a message.
type Field struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

// View is a message after the channel's preset was applied: plain text, escaped later by
// each renderer for its format.
type View struct {
	Status      string    `json:"status"`
	Event       string    `json:"event,omitempty"`
	Sender      string    `json:"sender"`
	SenderHost  string    `json:"senderHost,omitempty"`
	SenderRole  string    `json:"senderRole"`
	IconURL     string    `json:"iconUrl"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Fields      []Field   `json:"fields,omitempty"`
	Logs        []string  `json:"logs,omitempty"`
	URL         string    `json:"url,omitempty"`
	Timestamp   time.Time `json:"timestamp,omitzero"`
	Footer      string    `json:"footer"`
	Preset      string    `json:"preset"`
}

var eventLabels = map[string]string{
	EventChecks: "Checks", EventAutoUpdate: "Auto-update", EventRollback: "Rollback", EventDeployFailed: "Deploy failed",
	EventAgents: "Agents", EventWatchdog: "Watchdog", EventTest: "Test", EventUpdates: "Updates",
}

// BuildView applies the channel's preset to m.
func BuildView(c Config, m Message) View {
	p := c.Parts()
	s := m.resolvedSender()
	v := View{Status: m.Status, Event: m.Event, Sender: s.Name(), SenderHost: s.Host, SenderRole: s.Role,
		IconURL: m.iconURL(s), Title: strings.TrimSpace(m.Title), Preset: c.Preset}
	if v.Preset == "" {
		v.Preset = PresetStandard
	}
	if v.Title == "" {
		v.Title = "Moorage"
	}
	if p.Cause {
		v.Description = strings.TrimSpace(m.Text)
		if v.Description == "" {
			v.Description = strings.TrimSpace(m.Cause)
		}
	}
	add := func(name, value string, inline bool) {
		if value = strings.TrimSpace(value); value != "" {
			v.Fields = append(v.Fields, Field{Name: name, Value: value, Inline: inline})
		}
	}
	if p.Host {
		if len(m.Hosts) > 1 {
			add("Hosts", strings.Join(m.Hosts, ", "), true)
		} else {
			h := m.Host
			if h == "" && len(m.Hosts) == 1 {
				h = m.Hosts[0]
			}
			add("Host", h, true)
		}
	}
	if p.Stack {
		add("Stack", m.Stack, true)
	}
	if p.Duration && m.Duration > 0 {
		name := "Duration"
		if m.Status == "up" {
			name = "Down for"
		}
		add(name, HumanDuration(m.Duration), true)
	}
	if p.Image {
		for _, img := range m.Images {
			add("Image", img.String(), false)
		}
	}
	if p.CheckDetails {
		for _, ck := range m.Checks {
			add(firstNonEmpty(ck.Name, "Check"), ck.summary(), false)
		}
	}
	if p.Timestamps {
		if !m.Since.IsZero() {
			add("Started", m.Since.UTC().Format("2006-01-02 15:04:05 UTC"), true)
		}
		if !m.Until.IsZero() {
			add("Ended", m.Until.UTC().Format("2006-01-02 15:04:05 UTC"), true)
		}
		v.Timestamp = m.Until
		if v.Timestamp.IsZero() {
			v.Timestamp = m.Since
		}
	}
	if p.Logs && len(m.Logs) > 0 {
		logs := m.Logs
		if len(logs) > MaxLogLines {
			logs = logs[len(logs)-MaxLogLines:]
		}
		v.Logs = append([]string(nil), logs...)
	}
	if p.Link {
		v.URL = m.URL
	}
	v.Footer = "Moorage"
	if l := eventLabels[m.Event]; l != "" {
		v.Footer += " · " + l
	}
	return v
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// plainText renders a view as plain text (no markup): title is left to the caller when
// the service has its own title field.
func (v View) plainText(withTitle bool) string {
	var b strings.Builder
	if withTitle {
		b.WriteString(v.Title)
	}
	para := func(s string) {
		if s == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(s)
	}
	para(v.Description)
	var fl []string
	for _, f := range v.Fields {
		fl = append(fl, f.Name+": "+f.Value)
	}
	para(strings.Join(fl, "\n"))
	if len(v.Logs) > 0 {
		para("Last log lines:\n" + strings.Join(v.Logs, "\n"))
	}
	para(v.URL)
	return b.String()
}
