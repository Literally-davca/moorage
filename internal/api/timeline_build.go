package api

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"moorage/internal/engine"
	"moorage/internal/store"
)

// TimelineEntry is one line of the stack timeline.
type TimelineEntry struct {
	Key     string    `json:"key"` // the cursor of this entry, also a stable id for the UI
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Title   string    `json:"title"`
	Actor   string    `json:"actor,omitempty"`
	IP      string    `json:"ip,omitempty"` // only from audit rows and terminal sessions (admins)
	Detail  string    `json:"detail,omitempty"`
	Outcome string    `json:"outcome,omitempty"`
	Ref     tlRef     `json:"ref"`
	// Merged names the other sources folded into this entry ("event", "audit").
	Merged    []string    `json:"merged,omitempty"`
	EventKind string      `json:"eventKind,omitempty"` // stack event kind (own or merged)
	Action    string      `json:"action,omitempty"`    // audit action key (own or merged)
	Version   *tlVersionX `json:"version,omitempty"`
	Update    *tlUpdateX  `json:"update,omitempty"`
	Incident  *tlIncidX   `json:"incident,omitempty"`
}

// tlRef holds the ids of the rows behind an entry.
type tlRef struct {
	Hash       string `json:"hash,omitempty"`
	CheckID    int64  `json:"checkId,omitempty"`
	IncidentID int64  `json:"incidentId,omitempty"`
	UpdateID   int64  `json:"updateId,omitempty"`
	EventID    int64  `json:"eventId,omitempty"`
	AuditID    int64  `json:"auditId,omitempty"`
	SessionID  int64  `json:"sessionId,omitempty"`
}

type tlVersionX struct {
	Short    string   `json:"short"`
	Kind     string   `json:"kind"`
	Current  bool     `json:"current"`
	Restored string   `json:"restored,omitempty"`
	Files    []string `json:"files,omitempty"`
}

type tlUpdateX struct {
	Result      string `json:"result"`
	CanRollback bool   `json:"canRollback"`
}

type tlIncidX struct {
	Phase     string    `json:"phase"` // open | close
	CheckName string    `json:"checkName"`
	CheckKind string    `json:"checkKind"`
	StartedAt time.Time `json:"startedAt"`
	EndedAt   time.Time `json:"endedAt,omitzero"`
	Cause     string    `json:"cause"`
	// CausedBy is the upstream stack the incident's alert was folded into (R5 cascade).
	CausedBy *store.StackRef `json:"causedBy,omitempty"`
}

// TimelinePage is one response of the timeline endpoint.
type TimelinePage struct {
	Entries []TimelineEntry `json:"entries"`
	More    bool            `json:"more"`
	Next    string          `json:"next,omitempty"`
	// VersionsError says why versions are missing (agent offline, too old, history off).
	VersionsError string `json:"versionsError,omitempty"`
}

// tlItem is a row of any source while a page is built.
type tlItem struct {
	key   tlKey
	entry TimelineEntry
	actor string // who did it, for pairing
	class string // event kind, audit action or version kind, for pairing
	into  *tlItem
	cand  bool // fetched for this page (not only as pairing context)
}

func (it *tlItem) root() *tlItem {
	r := it
	for i := 0; r.into != nil && i < 4; i++ {
		r = r.into
	}
	return r
}

// tlSource is one bounded fetch: the rows of one kind that sort below the cursor.
type tlSource struct {
	items     []*tlItem
	truncated bool // more rows exist below the last one
}

// rangeFor turns the cursor into the per-source SQL bound: rows at the cursor's
// millisecond are kept when the source's kind sorts below the cursor's kind.
func rangeFor(q tlQuery, kind string) store.TimelineRange {
	r := store.TimelineRange{HostID: q.hostID, Stack: q.stack, FromMs: q.sinceMs, Limit: q.limit + 1}
	if q.cursor.at == 0 {
		return r
	}
	r.BeforeMs = q.cursor.at
	switch c := strings.Compare(kind, q.cursor.kind); {
	case c < 0:
		r.BeforeID = math.MaxInt64
	case c == 0:
		// ParseInt, not Sscan: Sscan reads the zero-padded id as octal.
		r.BeforeID, _ = strconv.ParseInt(q.cursor.id, 10, 64) // validated by parseCursor
	}
	return r
}

// buildTimeline merges one page. Every source is asked for at most limit+1 rows below the
// cursor. A page then only reaches down to the highest "last row" of any source that had
// more, so no row of a source can be skipped by rows of another one.
func (s *Server) buildTimeline(ctx context.Context, q tlQuery, versions versionSource) (TimelinePage, error) {
	page := TimelinePage{Entries: []TimelineEntry{}}
	var allVersions []engine.Version
	var sources []tlSource
	add := func(src tlSource, err error) error {
		if err != nil {
			return err
		}
		sources = append(sources, src)
		return nil
	}
	if q.kinds[tlVersion] {
		list, err := versions(ctx)
		if err != nil {
			page.VersionsError = versionsUnavailable(err)
		} else {
			allVersions = list
			sources = append(sources, versionPage(q, list))
		}
	}
	if q.kinds[tlEvent] {
		if err := add(s.eventSource(ctx, rangeFor(q, tlEvent))); err != nil {
			return page, err
		}
	}
	if q.kinds[tlIncident] {
		r := rangeFor(q, tlIncident)
		for _, ended := range []bool{false, true} {
			if err := add(s.incidentSource(ctx, r, ended)); err != nil {
				return page, err
			}
		}
	}
	if q.kinds[tlUpdate] {
		if err := add(s.updateSource(ctx, rangeFor(q, tlUpdate))); err != nil {
			return page, err
		}
	}
	if q.kinds[tlAudit] {
		if err := add(s.auditSource(ctx, rangeFor(q, tlAudit))); err != nil {
			return page, err
		}
	}
	if q.kinds[tlTerminal] {
		if err := add(s.terminalSource(ctx, rangeFor(q, tlTerminal))); err != nil {
			return page, err
		}
	}

	var cands []*tlItem
	var boundary *tlKey
	for _, src := range sources {
		for _, it := range src.items {
			it.cand = true
		}
		cands = append(cands, src.items...)
		if src.truncated && len(src.items) > 0 {
			last := src.items[len(src.items)-1].key
			if boundary == nil || last.cmp(*boundary) > 0 {
				boundary = &last
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].key.cmp(cands[j].key) > 0 })
	if boundary != nil {
		n := sort.Search(len(cands), func(i int) bool { return cands[i].key.cmp(*boundary) < 0 })
		cands = cands[:n]
	}

	if err := s.mergeDuplicates(ctx, q, cands, allVersions); err != nil {
		return page, err
	}

	consumed := 0
	for _, it := range cands {
		if len(page.Entries) == q.limit {
			break
		}
		consumed++
		if it.into == nil {
			page.Entries = append(page.Entries, it.entry)
		}
	}
	page.More = consumed < len(cands) || boundary != nil
	if page.More && consumed > 0 {
		page.Next = cands[consumed-1].key.String()
	}
	return page, nil
}

// ---- sources ---------------------------------------------------------------

func versionPage(q tlQuery, list []engine.Version) tlSource {
	all := versionItems(list)
	sort.Slice(all, func(i, j int) bool { return all[i].key.cmp(all[j].key) > 0 })
	var items []*tlItem
	for _, it := range all {
		if q.cursor.at != 0 && it.key.cmp(q.cursor) >= 0 {
			continue
		}
		if (q.sinceMs > 0 && it.key.at < q.sinceMs) || len(items) == q.limit+1 {
			break
		}
		items = append(items, it)
	}
	// Like the SQL sources: limit+1 rows means "maybe more".
	return sourceOf(items, q.limit)
}

var versionTitles = map[string]string{
	engine.KindCreate: "Created", engine.KindEdit: "Edited", engine.KindRestore: "Rolled back",
	engine.KindExternal: "Changed outside Moorage", engine.KindDelete: "Deleted", engine.KindImport: "Found on disk",
}

func versionItems(list []engine.Version) []*tlItem {
	out := make([]*tlItem, 0, len(list))
	for _, v := range list {
		k := tlKey{at: v.Time.UnixMilli(), kind: tlVersion, id: v.Hash}
		title := versionTitles[v.Kind]
		if title == "" {
			title = v.Kind
		}
		actor := v.Author
		if actor == engine.SystemAuthor {
			actor = ""
		}
		out = append(out, &tlItem{key: k, actor: v.Author, class: v.Kind, entry: TimelineEntry{
			Key: k.String(), At: time.UnixMilli(k.at), Kind: tlVersion, Title: title, Actor: actor, Detail: v.Message,
			Ref:     tlRef{Hash: v.Hash},
			Version: &tlVersionX{Short: v.Short, Kind: v.Kind, Current: v.Current, Restored: v.Restored, Files: v.Files},
		}})
	}
	return out
}

func sourceOf(items []*tlItem, limit int) tlSource {
	return tlSource{items: items, truncated: len(items) > limit}
}

// Terminal stack events are never listed: the terminal session row says the same and is
// admin-only, while stack events are shown to viewers.
func (s *Server) eventSource(ctx context.Context, r store.TimelineRange) (tlSource, error) {
	rows, err := s.store.TimelineEvents(ctx, r, "terminal")
	if err != nil {
		return tlSource{}, err
	}
	return sourceOf(eventItems(rows), r.Limit-1), nil
}

var eventTitles = map[string]string{
	"create": "Created", "edit": "Edited", "delete": "Deleted", "deploy": "Deployed", "update": "Updated",
	"restart": "Restarted", "stop": "Stopped", "down": "Taken down", "pull": "Pulled images", "rollback": "Rolled back",
}

func eventItems(rows []store.Event) []*tlItem {
	out := make([]*tlItem, 0, len(rows))
	for _, e := range rows {
		k := tlKey{at: e.TS.UnixMilli(), kind: tlEvent, id: numID(e.ID)}
		title, detail := eventTitles[e.Kind], e.Detail
		if e.Kind == "maintenance" {
			title, detail = maintenanceTitle(e.Detail)
		}
		if title == "" {
			title = e.Kind
		}
		out = append(out, &tlItem{key: k, actor: e.Actor, class: e.Kind, entry: TimelineEntry{
			Key: k.String(), At: time.UnixMilli(k.at), Kind: tlEvent, Title: title, Actor: e.Actor, Detail: detail,
			EventKind: e.Kind, Ref: tlRef{EventID: e.ID},
		}})
	}
	return out
}

// maintenanceTitle splits a maintenance event detail ("Started: title") into a title and the rest.
func maintenanceTitle(detail string) (string, string) {
	for _, p := range []struct{ prefix, title string }{
		{"Started: ", "Maintenance started"}, {"Ended early: ", "Maintenance ended early"}, {"Ended: ", "Maintenance ended"},
	} {
		if rest, ok := strings.CutPrefix(detail, p.prefix); ok {
			return p.title, rest
		}
	}
	return "Maintenance", detail
}

func (s *Server) incidentSource(ctx context.Context, r store.TimelineRange, ended bool) (tlSource, error) {
	rows, err := s.store.TimelineIncidents(ctx, r, ended)
	if err != nil {
		return tlSource{}, err
	}
	out := make([]*tlItem, 0, len(rows))
	for _, in := range rows {
		x := &tlIncidX{Phase: "open", CheckName: in.CheckName, CheckKind: in.CheckKind, StartedAt: in.StartedAt, EndedAt: in.EndedAt, Cause: in.Cause}
		k := tlKey{at: in.StartedAt.UnixMilli(), kind: tlIncident, id: numID(in.ID * 2)}
		title, detail := in.CheckName+" went down", in.Cause
		if ref, ok := store.ParseStackRef(in.CausedBy); ok {
			x.CausedBy = &ref
			detail = strings.TrimSpace(detail + " · probably caused by " + ref.Stack)
		}
		if ended {
			x.Phase = "close"
			k = tlKey{at: in.EndedAt.UnixMilli(), kind: tlIncident, id: numID(in.ID*2 + 1)}
			title, detail = in.CheckName+" recovered", "Down for "+shortDuration(in.EndedAt.Sub(in.StartedAt))
		}
		out = append(out, &tlItem{key: k, entry: TimelineEntry{
			Key: k.String(), At: time.UnixMilli(k.at), Kind: tlIncident, Title: title, Detail: detail,
			Ref: tlRef{CheckID: in.CheckID, IncidentID: in.ID}, Incident: x,
		}})
	}
	return sourceOf(out, r.Limit-1), nil
}

func shortDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min %d s", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
}

func (s *Server) updateSource(ctx context.Context, r store.TimelineRange) (tlSource, error) {
	rows, err := s.store.TimelineImageUpdates(ctx, r)
	if err != nil {
		return tlSource{}, err
	}
	return sourceOf(updateItems(rows), r.Limit-1), nil
}

func updateItems(rows []store.ImageUpdate) []*tlItem {
	out := make([]*tlItem, 0, len(rows))
	for _, u := range rows {
		k := tlKey{at: u.StartedAt.UnixMilli(), kind: tlUpdate, id: numID(u.ID)}
		canRollback := false
		if u.Result == "ok" || u.Result == "up-failed" {
			for _, svc := range u.Services {
				if svc.OldID != "" || svc.OldDigest != "" {
					canRollback = true
				}
			}
		}
		out = append(out, &tlItem{key: k, actor: u.Actor, entry: TimelineEntry{
			Key: k.String(), At: time.UnixMilli(k.at), Kind: tlUpdate, Title: "Image update (" + u.Result + ")",
			Actor: u.Actor, Detail: updateSummary(u), Outcome: u.Result,
			Ref: tlRef{UpdateID: u.ID}, Update: &tlUpdateX{Result: u.Result, CanRollback: canRollback},
		}})
	}
	return out
}

// updateSummary lists only the services whose image changed: a record lists them all.
func updateSummary(u store.ImageUpdate) string {
	short := func(d string) string {
		d = strings.TrimPrefix(d, "sha256:")
		return d[:min(7, len(d))]
	}
	var parts []string
	for _, s := range u.Services {
		old, nw := s.OldDigest, s.NewDigest
		if old == "" {
			old = s.OldID
		}
		if nw == "" {
			nw = s.NewID
		}
		if old != nw {
			parts = append(parts, s.Service+": "+short(old)+" → "+short(nw))
		}
	}
	return strings.Join(parts, ", ")
}

// terminal.open audit rows are left out: the terminal session row written at the same
// moment carries the same facts and more (container, shell, how it ended).
func (s *Server) auditSource(ctx context.Context, r store.TimelineRange) (tlSource, error) {
	rows, err := s.store.TimelineAudit(ctx, r, auditTermOpen)
	if err != nil {
		return tlSource{}, err
	}
	return sourceOf(auditItems(rows), r.Limit-1), nil
}

func auditItems(rows []store.AuditEntry) []*tlItem {
	out := make([]*tlItem, 0, len(rows))
	for _, a := range rows {
		k := tlKey{at: a.At.UnixMilli(), kind: tlAudit, id: numID(a.ID)}
		outcome := ""
		if a.Outcome == store.AuditFailed {
			outcome = store.AuditFailed
		}
		out = append(out, &tlItem{key: k, actor: a.Actor, class: a.Action, entry: TimelineEntry{
			Key: k.String(), At: time.UnixMilli(k.at), Kind: tlAudit, Title: a.Action, Action: a.Action,
			Actor: a.Actor, IP: a.IP, Detail: a.Target, Outcome: outcome, Ref: tlRef{AuditID: a.ID},
		}})
	}
	return out
}

func (s *Server) terminalSource(ctx context.Context, r store.TimelineRange) (tlSource, error) {
	rows, err := s.store.TimelineTerminals(ctx, r)
	if err != nil {
		return tlSource{}, err
	}
	out := make([]*tlItem, 0, len(rows))
	for _, t := range rows {
		k := tlKey{at: t.StartedAt.UnixMilli(), kind: tlTerminal, id: numID(t.ID)}
		out = append(out, &tlItem{key: k, actor: t.User, entry: TimelineEntry{
			Key: k.String(), At: time.UnixMilli(k.at), Kind: tlTerminal, Title: "Terminal session (" + terminalEnd(t) + ")",
			Actor: t.User, IP: t.IP, Detail: t.ContainerName + " · " + t.Shell + terminalLength(t), Ref: tlRef{SessionID: t.ID},
		}})
	}
	return sourceOf(out, r.Limit-1), nil
}

func terminalEnd(t store.TerminalAuditRow) string {
	switch {
	case t.Reason == "":
		return "open"
	case t.ExitCode == nil:
		return t.Reason
	case t.Reason == store.TermReasonExit:
		return fmt.Sprintf("exit %d", *t.ExitCode)
	}
	return fmt.Sprintf("%s · exit %d", t.Reason, *t.ExitCode)
}

func terminalLength(t store.TerminalAuditRow) string {
	if t.EndedAt.IsZero() {
		return ""
	}
	return " · " + shortDuration(t.EndedAt.Sub(t.StartedAt))
}
