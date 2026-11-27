package notify

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ---- Mentions on Discord and Slack (M19L) -------------------------------------------
//
// A channel can mention people under rules: who (Discord role, user or @here; Slack user
// group, user, @here or @channel), on which events, for which stacks or checks and
// optionally only on the first alert of an outage. A new channel mentions nobody. Only
// the chosen IDs can ping: Discord gets allowed_mentions with exactly those IDs and Slack
// text from logs is escaped, so a log line can never ping anyone.

// Mention events.
const (
	MentionDown           = "down"
	MentionUp             = "up"
	MentionDegraded       = "degraded"
	MentionAgentOffline   = "agent_offline"
	MentionUpdateFailed   = "update_failed"
	MentionRolledBack     = "rolled_back"
	MentionSelfRolledBack = "self_rolled_back"
)

// MentionEvents lists the events a rule can match.
var MentionEvents = []string{MentionDown, MentionUp, MentionDegraded, MentionAgentOffline, MentionUpdateFailed, MentionRolledBack, MentionSelfRolledBack}

// MentionRule is one rule of a channel.
type MentionRule struct {
	// Who: Discord "role:<id>", "user:<id>", "here"; Slack "group:<id>", "user:<id>",
	// "here", "channel".
	Who    []string `json:"who"`
	Events []string `json:"events"`
	// Stacks and Checks narrow the rule (empty = everything).
	Stacks []string `json:"stacks,omitempty"`
	Checks []string `json:"checks,omitempty"`
	// FirstOnly: only the first alert of an outage, not reminders or "still down".
	FirstOnly bool `json:"firstOnly,omitempty"`
}

var mentionIDRe = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)

// ValidMention reports whether a "who" value is well formed for the channel type.
func ValidMention(kind, who string) bool {
	switch who {
	case "here":
		return true
	case "channel":
		return kind == "slack"
	}
	pfx, id, ok := strings.Cut(who, ":")
	if !ok || !mentionIDRe.MatchString(id) {
		return false
	}
	switch kind {
	case "discord":
		return (pfx == "role" || pfx == "user") && isDigits(id)
	case "slack":
		return pfx == "group" || pfx == "user"
	}
	return false
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// mentionEvent maps a message to a rule event ("" = never mentioned: tests, updates).
func mentionEvent(m Message) string {
	switch m.Event {
	case "", EventChecks:
		switch m.Status {
		case "down":
			return MentionDown
		case "up":
			return MentionUp
		case "degraded":
			return MentionDegraded
		}
	case EventAgents, EventWatchdog:
		if m.Status == "down" {
			return MentionAgentOffline
		}
	case EventDeployFailed:
		return MentionUpdateFailed
	case EventRollback:
		return MentionRolledBack
	case EventSelfRolledBack:
		return MentionSelfRolledBack
	}
	return ""
}

// EventSelfRolledBack marks a rolled-back Apply & restart of Moorage's own stack.
const EventSelfRolledBack = "self_rolled_back"

// mentionsFor returns who to mention for this message (deduplicated, in rule order).
func (c Config) mentionsFor(m Message) []string {
	ev := mentionEvent(m)
	if ev == "" || len(c.Mentions) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range c.Mentions {
		if !contains(r.Events, ev) {
			continue
		}
		if r.FirstOnly && m.Repeat {
			continue
		}
		if len(r.Stacks) > 0 && !contains(r.Stacks, m.Stack) {
			continue
		}
		if len(r.Checks) > 0 && !anyCheck(r.Checks, m) {
			continue
		}
		for _, w := range r.Who {
			if !seen[w] {
				seen[w] = true
				out = append(out, w)
			}
		}
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

func anyCheck(names []string, m Message) bool {
	for _, c := range m.Checks {
		if contains(names, c.Name) {
			return true
		}
	}
	return false
}

// discordMentions renders the mention text and the allowed_mentions object.
func discordMentions(who []string) (string, map[string]any) {
	allowed := map[string]any{"parse": []string{}}
	if len(who) == 0 {
		return "", allowed
	}
	var parts, roles, users []string
	var parse []string
	for _, w := range who {
		switch {
		case w == "here":
			parts = append(parts, "@here")
			parse = []string{"everyone"} // Discord gates @here under "everyone"
		case strings.HasPrefix(w, "role:"):
			id := strings.TrimPrefix(w, "role:")
			parts = append(parts, "<@&"+id+">")
			roles = append(roles, id)
		case strings.HasPrefix(w, "user:"):
			id := strings.TrimPrefix(w, "user:")
			parts = append(parts, "<@"+id+">")
			users = append(users, id)
		}
	}
	if parse != nil {
		allowed["parse"] = parse
	}
	if len(roles) > 0 {
		allowed["roles"] = roles
	}
	if len(users) > 0 {
		allowed["users"] = users
	}
	return strings.Join(parts, " "), allowed
}

// slackMentions renders the Slack mention text.
func slackMentions(who []string) string {
	var parts []string
	for _, w := range who {
		switch {
		case w == "here":
			parts = append(parts, "<!here>")
		case w == "channel":
			parts = append(parts, "<!channel>")
		case strings.HasPrefix(w, "group:"):
			parts = append(parts, "<!subteam^"+strings.TrimPrefix(w, "group:")+">")
		case strings.HasPrefix(w, "user:"):
			parts = append(parts, "<@"+strings.TrimPrefix(w, "user:")+">")
		}
	}
	return strings.Join(parts, " ")
}

// MentionPreview says who a test message of this channel would mention for a DOWN alert.
func (c Config) MentionPreview() []string {
	return c.mentionsFor(Message{Event: EventChecks, Status: "down"})
}

// validateMentions checks the rules of a channel.
func validateMentions(kind string, rules []MentionRule) error {
	if len(rules) == 0 {
		return nil
	}
	if kind != "discord" && kind != "slack" {
		return errors.New("mentions work only on Discord and Slack channels")
	}
	if len(rules) > 10 {
		return errors.New("at most 10 mention rules per channel")
	}
	for i, r := range rules {
		if len(r.Who) == 0 || len(r.Who) > 10 {
			return fmt.Errorf("mention rule %d: choose 1 to 10 roles, groups or users", i+1)
		}
		for _, w := range r.Who {
			if !ValidMention(kind, w) {
				return fmt.Errorf("mention rule %d: %q isn't a valid %s mention (use the numeric ID of a role or user)", i+1, w, kind)
			}
		}
		if len(r.Events) == 0 {
			return fmt.Errorf("mention rule %d: choose at least one event", i+1)
		}
		for _, e := range r.Events {
			if !contains(MentionEvents, e) {
				return fmt.Errorf("mention rule %d: unknown event %q", i+1, e)
			}
		}
		if len(r.Stacks) > 50 || len(r.Checks) > 50 {
			return fmt.Errorf("mention rule %d: at most 50 stacks and 50 checks", i+1)
		}
	}
	return nil
}
