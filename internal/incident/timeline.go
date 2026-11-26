// Package incident builds the causal timeline of an incident and runs Rollback & Verify
// (M18A): what changed before a check went down, and an undo that is only called done
// when the check itself reports the service up again.
package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/store"
)

// Window is how far before an incident the timeline looks.
const Window = 15 * time.Minute

// buildTimeout bounds the work done while the incident opens (history comes from agents
// over the tunnel); what isn't there in time is left out.
const buildTimeout = 5 * time.Second

// Source types.
const (
	SourceCompose = "compose_change"
	SourceImage   = "image_update"
	SourceDocker  = "docker_event"
	SourceLog     = "log_match"
	SourceProbe   = "probe_failure"
)

// TimelineSource is one entry of the causal timeline.
type TimelineSource struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Title     string    `json:"title"`
	Detail    string    `json:"detail,omitempty"`
	Stack     string    `json:"stack,omitempty"`
	HostID    string    `json:"hostId,omitempty"`
	SourceRef string    `json:"sourceRef,omitempty"` // commit hash, update#id, container id
	Severity  string    `json:"severity"`            // info | warning | critical
	Actor     string    `json:"actor,omitempty"`
}

// CausalTimeline is the timeline stored with an incident.
type CausalTimeline struct {
	IncidentID  int64            `json:"incidentId"`
	CheckID     int64            `json:"checkId"`
	CheckName   string           `json:"checkName"`
	CheckKind   string           `json:"checkKind"`
	WindowStart time.Time        `json:"windowStart"`
	WindowEnd   time.Time        `json:"windowEnd"`
	Sources     []TimelineSource `json:"sources"`
	GeneratedAt time.Time        `json:"generatedAt"`
}

// Hosts is the part of host.Registry the package uses (a fake in tests).
type Hosts interface {
	Get(ctx context.Context, id string) (host.Host, host.Entry, error)
}

// TimelineBuilder builds causal timelines from history.git, image updates, recorded
// container events, log matches and probe results.
type TimelineBuilder struct {
	st  *store.Store
	reg Hosts
	log *slog.Logger
}

// NewTimelineBuilder creates a builder. reg may be nil (no compose history then).
func NewTimelineBuilder(st *store.Store, reg Hosts, log *slog.Logger) *TimelineBuilder {
	return &TimelineBuilder{st: st, reg: reg, log: log}
}

// Build collects what happened to the check's stack in the Window before at. matches are
// the log lines of a log check that triggered the incident.
func (b *TimelineBuilder) Build(ctx context.Context, c store.Check, at time.Time, matches []engine.LogLine) *CausalTimeline {
	ctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	tl := &CausalTimeline{
		CheckID: c.ID, CheckName: c.Name, CheckKind: c.Kind,
		WindowStart: at.Add(-Window), WindowEnd: at, Sources: []TimelineSource{}, GeneratedAt: time.Now(),
	}
	if c.Stack != "" && c.HostID != "" {
		b.addComposeChanges(ctx, c, tl)
		b.addImageUpdates(ctx, c, tl)
		b.addContainerEvents(ctx, c, tl)
	}
	addLogMatches(c, matches, at, tl)
	b.addProbeFailures(ctx, c, tl)
	sort.SliceStable(tl.Sources, func(i, j int) bool { return tl.Sources[i].Timestamp.Before(tl.Sources[j].Timestamp) })
	return tl
}

func inWindow(t time.Time, tl *CausalTimeline) bool {
	return !t.Before(tl.WindowStart) && !t.After(tl.WindowEnd)
}

func (b *TimelineBuilder) addComposeChanges(ctx context.Context, c store.Check, tl *CausalTimeline) {
	if b.reg == nil {
		return
	}
	h, _, err := b.reg.Get(ctx, c.HostID)
	if err != nil {
		return
	}
	versions, err := h.History(ctx, c.Stack)
	if err != nil {
		b.log.Debug("timeline: stack history", "stack", c.Stack, "err", err)
		return
	}
	for _, v := range versions {
		if !inWindow(v.Time, tl) || !touchesStackFiles(v.Files) {
			continue
		}
		title := "Configuration changed"
		if len(v.Files) > 0 {
			title = "Changed " + strings.Join(v.Files, ", ")
		}
		tl.Sources = append(tl.Sources, TimelineSource{
			Type: SourceCompose, Timestamp: v.Time, Title: title, Detail: v.Message,
			Stack: c.Stack, HostID: c.HostID, SourceRef: v.Hash, Severity: "warning", Actor: v.Author,
		})
	}
}

// touchesStackFiles reports whether a version changed the compose file or .env (a version
// without a file list counts: older agents don't send one).
func touchesStackFiles(files []string) bool {
	if len(files) == 0 {
		return true
	}
	for _, f := range files {
		if f == ".env" || strings.HasSuffix(f, ".yaml") || strings.HasSuffix(f, ".yml") {
			return true
		}
	}
	return false
}

func (b *TimelineBuilder) addImageUpdates(ctx context.Context, c store.Check, tl *CausalTimeline) {
	updates, err := b.st.ImageUpdates(ctx, c.HostID, c.Stack, 20)
	if err != nil {
		return
	}
	for _, u := range updates {
		at := updateTime(u)
		if !inWindow(at, tl) {
			continue
		}
		parts := changedImages(u)
		if len(parts) == 0 {
			continue
		}
		tl.Sources = append(tl.Sources, TimelineSource{
			Type: SourceImage, Timestamp: at, Title: "Image update (" + u.Result + ")", Detail: strings.Join(parts, "; "),
			Stack: c.Stack, HostID: c.HostID, SourceRef: fmt.Sprintf("update#%d", u.ID), Severity: "warning", Actor: u.Actor,
		})
	}
}

func updateTime(u store.ImageUpdate) time.Time {
	if !u.FinishedAt.IsZero() {
		return u.FinishedAt
	}
	return u.StartedAt
}

func changedImages(u store.ImageUpdate) []string {
	var parts []string
	for _, s := range u.Services {
		if (s.OldDigest != "" || s.OldID != "") && (s.OldDigest != s.NewDigest || s.OldID != s.NewID) {
			parts = append(parts, fmt.Sprintf("%s: %s → %s", s.Service, shortDigest(s.OldDigest, s.OldID), shortDigest(s.NewDigest, s.NewID)))
		}
	}
	return parts
}

func (b *TimelineBuilder) addContainerEvents(ctx context.Context, c store.Check, tl *CausalTimeline) {
	events, err := b.st.ContainerEvents(ctx, c.HostID, c.Stack, tl.WindowStart, tl.WindowEnd)
	if err != nil {
		return
	}
	for _, e := range events {
		src := TimelineSource{Type: SourceDocker, Timestamp: e.Time, Stack: c.Stack, HostID: c.HostID, SourceRef: e.ContainerID, Detail: e.Detail}
		switch e.Type {
		case store.ContainerDie:
			src.Title, src.Severity = fmt.Sprintf("%s stopped", e.Service), "warning"
			if e.ExitCode != nil && *e.ExitCode != 0 {
				src.Title, src.Severity = fmt.Sprintf("%s exited with code %d", e.Service, *e.ExitCode), "critical"
				if *e.ExitCode == 137 {
					src.Detail = "killed (exit 137): often out of memory"
				}
			}
		case store.ContainerRestart:
			src.Title, src.Severity = fmt.Sprintf("%s is restarting", e.Service), "critical"
		case store.ContainerUnhealthy:
			if e.Detail != "unhealthy" {
				continue // back to healthy is no cause
			}
			src.Title, src.Severity, src.Detail = fmt.Sprintf("%s turned unhealthy", e.Service), "critical", ""
		default:
			continue
		}
		if e.Count > 1 {
			src.Title += fmt.Sprintf(" (%d×)", e.Count)
		}
		tl.Sources = append(tl.Sources, src)
	}
}

func addLogMatches(c store.Check, matches []engine.LogLine, at time.Time, tl *CausalTimeline) {
	const maxLines = 5
	if len(matches) > maxLines {
		matches = matches[len(matches)-maxLines:]
	}
	for _, l := range matches {
		ts, err := time.Parse(time.RFC3339Nano, l.Time)
		if err != nil || !inWindow(ts, tl) {
			ts = at
		}
		tl.Sources = append(tl.Sources, TimelineSource{
			Type: SourceLog, Timestamp: ts, Title: "Log match in " + firstNonEmpty(l.Container, c.Stack),
			Detail: clip(l.Message, 300), Stack: c.Stack, HostID: c.HostID, Severity: "warning",
		})
	}
}

func (b *TimelineBuilder) addProbeFailures(ctx context.Context, c store.Check, tl *CausalTimeline) {
	results, err := b.st.CheckResultsInWindow(ctx, c.ID, tl.WindowStart, tl.WindowEnd)
	if err != nil {
		return
	}
	var locs map[int64][]string // per probe time: failing locations
	if len(c.Locations) > 0 {
		rows, _ := b.st.LocationResultsInWindow(ctx, c.ID, tl.WindowStart, tl.WindowEnd)
		locs = map[int64][]string{}
		for _, l := range rows {
			if l.OK != nil && !*l.OK {
				k := l.TS.UnixMilli()
				locs[k] = append(locs[k], l.Location+": "+l.Msg)
			}
		}
	}
	for _, r := range results {
		if r.Status != "down" && r.Status != "degraded" && r.Status != "pending" {
			continue
		}
		detail := ""
		if l := locs[r.TS.UnixMilli()]; len(l) > 0 {
			detail = strings.Join(l, "; ")
		}
		tl.Sources = append(tl.Sources, TimelineSource{
			Type: SourceProbe, Timestamp: r.TS, Title: "Check failed: " + clip(r.Msg, 120), Detail: detail,
			Stack: c.Stack, HostID: c.HostID, SourceRef: fmt.Sprintf("check#%d", c.ID), Severity: "warning",
		})
	}
}

// Changes is the short "what changed before the outage" list for notifications: compose
// changes, image updates and container crashes, newest last, at most max lines.
func (tl *CausalTimeline) Changes(max int) []string {
	var out []string
	for _, s := range tl.Sources {
		switch s.Type {
		case SourceCompose, SourceImage:
		case SourceDocker:
			if s.Severity != "critical" {
				continue
			}
		default:
			continue
		}
		line := s.Timestamp.UTC().Format("15:04") + " UTC " + s.Title
		if s.Detail != "" && s.Type != SourceCompose {
			line += ": " + s.Detail
		}
		if s.Actor != "" {
			line += " (" + s.Actor + ")"
		}
		out = append(out, line)
	}
	if len(out) > max {
		out = out[len(out)-max:]
	}
	return out
}

// SaveTimeline stores tl with the incident.
func SaveTimeline(ctx context.Context, st *store.Store, incidentID int64, tl *CausalTimeline) error {
	tl.IncidentID = incidentID
	b, err := json.Marshal(tl)
	if err != nil {
		return err
	}
	return st.SetIncidentTimeline(ctx, incidentID, string(b))
}

// LoadTimeline returns the stored timeline of an incident (empty for incidents from
// before M18A).
func LoadTimeline(ctx context.Context, st *store.Store, incidentID int64) (*CausalTimeline, error) {
	data, err := st.IncidentTimeline(ctx, incidentID)
	if err != nil {
		return nil, err
	}
	tl := &CausalTimeline{IncidentID: incidentID, Sources: []TimelineSource{}}
	if data == "" || data == "{}" {
		return tl, nil
	}
	if err := json.Unmarshal([]byte(data), tl); err != nil {
		return nil, err
	}
	if tl.Sources == nil {
		tl.Sources = []TimelineSource{}
	}
	return tl, nil
}

func shortDigest(digest, id string) string {
	if d := strings.TrimPrefix(digest, "sha256:"); d != "" {
		return d[:min(len(d), 12)]
	}
	if i := strings.TrimPrefix(id, "sha256:"); i != "" {
		return i[:min(len(i), 12)]
	}
	return "unknown"
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
