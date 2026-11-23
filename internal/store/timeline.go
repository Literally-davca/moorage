package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// TimelineRange selects one page of a stack's rows for the stack timeline. The timeline
// orders entries by (time, kind, id) newest first, so every source can be asked for the
// rows that sort below the cursor without loading the whole table.
type TimelineRange struct {
	HostID string
	Stack  string
	// BeforeMs and BeforeID form the cursor: rows with at < BeforeMs, or at == BeforeMs and
	// sort id < BeforeID. BeforeMs == 0 means no cursor. BeforeID 0 excludes every row at
	// BeforeMs; math.MaxInt64 keeps them all.
	BeforeMs int64
	BeforeID int64
	// FromMs and ToMs bound the time range, both inclusive; 0 leaves that side open.
	FromMs int64
	ToMs   int64
	Limit  int
}

// pageWhere builds the conditions of r for a time column (atCol, in seconds when secs is
// true, else milliseconds) and a sort id expression.
func (r TimelineRange) pageWhere(atCol string, secs bool, idExpr string) (string, []any) {
	ms := atCol
	if secs {
		ms = atCol + `*1000`
	}
	var conds []string
	var args []any
	if r.BeforeMs > 0 {
		// The plain column bound lets SQLite use the (host, stack, at) indexes.
		upper := r.BeforeMs
		if secs {
			upper = r.BeforeMs/1000 + 1
		}
		conds = append(conds, atCol+` <= ?`, `(`+ms+` < ? OR (`+ms+` = ? AND `+idExpr+` < ?))`)
		args = append(args, upper, r.BeforeMs, r.BeforeMs, r.BeforeID)
	}
	if r.FromMs > 0 {
		conds = append(conds, ms+` >= ?`)
		args = append(args, r.FromMs)
	}
	if r.ToMs > 0 {
		conds = append(conds, ms+` <= ?`)
		args = append(args, r.ToMs)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return ` AND ` + strings.Join(conds, ` AND `), args
}

func (r TimelineRange) limit() int {
	if r.Limit <= 0 {
		return 100
	}
	return r.Limit
}

// TimelineEvents lists a stack's events newest first, without the kinds in skip.
func (s *Store) TimelineEvents(ctx context.Context, r TimelineRange, skip ...string) ([]Event, error) {
	q := `SELECT id, ts, host_id, stack, kind, actor, detail FROM events WHERE host_id = ? AND stack = ?`
	args := []any{r.HostID, r.Stack}
	for _, k := range skip {
		q += ` AND kind != ?`
		args = append(args, k)
	}
	w, wa := r.pageWhere(`ts`, false, `id`)
	args = append(append(args, wa...), r.limit())
	rows, err := s.db.QueryContext(ctx, q+w+` ORDER BY ts DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("timeline events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var ts int64
		if err := rows.Scan(&e.ID, &ts, &e.HostID, &e.Stack, &e.Kind, &e.Actor, &e.Detail); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e.TS = time.UnixMilli(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// TimelineAudit lists a stack's audit rows newest first, without the actions in skip.
func (s *Store) TimelineAudit(ctx context.Context, r TimelineRange, skip ...string) ([]AuditEntry, error) {
	q := `SELECT id, at, actor, ip, action, host_id, stack, target, outcome FROM audit_log WHERE host_id = ? AND stack = ?`
	args := []any{r.HostID, r.Stack}
	for _, a := range skip {
		q += ` AND action != ?`
		args = append(args, a)
	}
	w, wa := r.pageWhere(`at`, true, `id`)
	args = append(append(args, wa...), r.limit())
	rows, err := s.db.QueryContext(ctx, q+w+` ORDER BY at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("timeline audit: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.IP, &e.Action, &e.HostID, &e.Stack, &e.Target, &e.Outcome); err != nil {
			return nil, fmt.Errorf("scan audit row: %w", err)
		}
		e.At = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// TimelineImageUpdates lists a stack's image updates newest first by start time. The log
// tail is left out: the timeline never shows it.
func (s *Store) TimelineImageUpdates(ctx context.Context, r TimelineRange) ([]ImageUpdate, error) {
	q := `SELECT id, host_id, stack, started_at, finished_at, actor, result, services_json FROM image_updates
		WHERE host_id = ? AND stack = ?`
	w, wa := r.pageWhere(`started_at`, true, `id`)
	args := append(append([]any{r.HostID, r.Stack}, wa...), r.limit())
	rows, err := s.db.QueryContext(ctx, q+w+` ORDER BY started_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("timeline image updates: %w", err)
	}
	defer rows.Close()
	var out []ImageUpdate
	for rows.Next() {
		var u ImageUpdate
		var started, finished int64
		var svcs string
		if err := rows.Scan(&u.ID, &u.HostID, &u.Stack, &started, &finished, &u.Actor, &u.Result, &svcs); err != nil {
			return nil, fmt.Errorf("scan image update: %w", err)
		}
		u.StartedAt = time.Unix(started, 0)
		if finished > 0 {
			u.FinishedAt = time.Unix(finished, 0)
		}
		_ = json.Unmarshal([]byte(svcs), &u.Services)
		if u.Services == nil {
			u.Services = []ImageUpdateService{}
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// TimelineTerminals lists a stack's terminal sessions newest first by start time.
func (s *Store) TimelineTerminals(ctx context.Context, r TimelineRange) ([]TerminalAuditRow, error) {
	q := `SELECT id, user, ip, host_id, stack, container_name, service, shell, started_at, ended_at, reason, exit_code
		FROM terminal_sessions WHERE host_id = ? AND stack = ?`
	w, wa := r.pageWhere(`started_at`, true, `id`)
	args := append(append([]any{r.HostID, r.Stack}, wa...), r.limit())
	rows, err := s.db.QueryContext(ctx, q+w+` ORDER BY started_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("timeline terminals: %w", err)
	}
	defer rows.Close()
	var out []TerminalAuditRow
	for rows.Next() {
		var t TerminalAuditRow
		var started, ended int64
		var code sql.NullInt64
		if err := rows.Scan(&t.ID, &t.User, &t.IP, &t.HostID, &t.Stack, &t.ContainerName, &t.Service, &t.Shell,
			&started, &ended, &t.Reason, &code); err != nil {
			return nil, fmt.Errorf("scan terminal session: %w", err)
		}
		t.StartedAt = time.Unix(started, 0)
		if ended > 0 {
			t.EndedAt = time.Unix(ended, 0)
		}
		if code.Valid {
			c := int(code.Int64)
			t.ExitCode = &c
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TimelineIncident is an incident of one of a stack's checks, with the check's name.
type TimelineIncident struct {
	Incident
	CheckName string
	CheckKind string
}

// TimelineIncidents lists incidents of the stack's checks newest first. With ended false
// the rows are ordered by start (sort id 2*id), with ended true only closed incidents are
// listed, ordered by their end (sort id 2*id+1): a start and an end are separate entries.
func (s *Store) TimelineIncidents(ctx context.Context, r TimelineRange, ended bool) ([]TimelineIncident, error) {
	atCol, idExpr := `i.started_at`, `i.id*2`
	q := `SELECT i.id, i.check_id, c.name, c.kind, i.started_at, COALESCE(i.ended_at, 0), i.cause, i.caused_by
		FROM incidents i JOIN checks c ON c.id = i.check_id WHERE c.host_id = ? AND c.stack = ?`
	if ended {
		atCol, idExpr = `i.ended_at`, `i.id*2+1`
		q += ` AND i.ended_at IS NOT NULL`
	}
	w, wa := r.pageWhere(atCol, true, idExpr)
	args := append(append([]any{r.HostID, r.Stack}, wa...), r.limit())
	rows, err := s.db.QueryContext(ctx, q+w+` ORDER BY `+atCol+` DESC, i.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("timeline incidents: %w", err)
	}
	defer rows.Close()
	var out []TimelineIncident
	for rows.Next() {
		var in TimelineIncident
		var started, endedAt int64
		if err := rows.Scan(&in.ID, &in.CheckID, &in.CheckName, &in.CheckKind, &started, &endedAt, &in.Cause, &in.CausedBy); err != nil {
			return nil, fmt.Errorf("scan incident: %w", err)
		}
		in.StartedAt = time.Unix(started, 0)
		if endedAt > 0 {
			in.EndedAt = time.Unix(endedAt, 0)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}
