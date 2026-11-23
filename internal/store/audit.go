package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Audit outcomes.
const (
	AuditOK     = "ok"
	AuditFailed = "failed"
)

// AuditActorSystem is the actor of actions the hub takes on its own (auto-update, agent pairing).
const AuditActorSystem = "system"

// AuditEntry is one row of the audit log: who did what, where, and whether it worked.
// It never holds request bodies or anything secret, only keys and path values.
type AuditEntry struct {
	ID      int64     `json:"id"`
	At      time.Time `json:"at"`
	Actor   string    `json:"actor"`
	IP      string    `json:"ip"`
	Action  string    `json:"action"`
	HostID  string    `json:"hostId"`
	Stack   string    `json:"stack"`
	Target  string    `json:"target"`
	Outcome string    `json:"outcome"`
}

// AuditFilter narrows AuditEntries. Empty fields don't filter.
type AuditFilter struct {
	HostID       string
	Stack        string
	Actor        string
	ActionPrefix string
	BeforeID     int64 // paging: only rows with a smaller id
	Limit        int
}

// maxAuditField caps every text column, so a long username tried at sign-in can't bloat the table.
const maxAuditField = 200

func clip(s string) string {
	if len(s) <= maxAuditField {
		return s
	}
	return s[:maxAuditField]
}

// Audit appends one row to the audit log. A zero At means now; an empty outcome means ok.
func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	if e.Outcome != AuditFailed {
		e.Outcome = AuditOK
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (at, actor, ip, action, host_id, stack, target, outcome) VALUES (?,?,?,?,?,?,?,?)`,
		e.At.Unix(), clip(e.Actor), clip(e.IP), clip(e.Action), clip(e.HostID), clip(e.Stack), clip(e.Target), e.Outcome)
	if err != nil {
		return fmt.Errorf("audit %s: %w", e.Action, err)
	}
	return nil
}

// AuditEntries lists audit rows newest first. Paging uses the id, which only grows,
// so a clock change can't skip or repeat rows between pages.
func (s *Store) AuditEntries(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	var where []string
	var args []any
	add := func(cond string, v any) {
		where = append(where, cond)
		args = append(args, v)
	}
	if f.HostID != "" {
		add(`host_id = ?`, f.HostID)
	}
	if f.Stack != "" {
		add(`stack = ?`, f.Stack)
	}
	if f.Actor != "" {
		add(`actor = ?`, f.Actor)
	}
	if f.ActionPrefix != "" {
		// substr instead of LIKE: no escaping of % and _ needed.
		where = append(where, `substr(action, 1, ?) = ?`)
		args = append(args, len(f.ActionPrefix), f.ActionPrefix)
	}
	if f.BeforeID > 0 {
		add(`id < ?`, f.BeforeID)
	}
	q := `SELECT id, at, actor, ip, action, host_id, stack, target, outcome FROM audit_log`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, f.Limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit log: %w", err)
	}
	defer rows.Close()
	out := []AuditEntry{}
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

// PruneAudit removes audit rows older than cutoff (1 year).
func (s *Store) PruneAudit(ctx context.Context, cutoff time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM audit_log WHERE at < ?`, cutoff.Unix())
	return err
}
