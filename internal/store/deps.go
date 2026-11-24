package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Dependency edge sources.
const (
	DepDetected = "detected"
	DepManual   = "manual"
)

// StackRef names a stack on a host.
type StackRef struct {
	HostID string `json:"hostId"`
	Stack  string `json:"stack"`
}

// Key is "host/stack", the form used in alert grouping and incidents.caused_by.
func (r StackRef) Key() string { return r.HostID + "/" + r.Stack }

// ParseStackRef reverses Key. Host ids never contain a slash.
func ParseStackRef(key string) (StackRef, bool) {
	h, s, ok := strings.Cut(key, "/")
	if !ok || h == "" || s == "" {
		return StackRef{}, false
	}
	return StackRef{HostID: h, Stack: s}, true
}

// StackDep is one edge: From depends on To.
type StackDep struct {
	ID        int64     `json:"id"`
	From      StackRef  `json:"from"`
	To        StackRef  `json:"to"`
	Source    string    `json:"source"`           // detected | manual
	Reason    string    `json:"reason,omitempty"` // network | hostname (detected edges)
	Detail    string    `json:"detail,omitempty"`
	Ignored   bool      `json:"ignored"`
	CreatedBy string    `json:"createdBy,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	// AutoRestart: off | on_recover | on_recover_and_deploy. Restart From when To recovers
	// (and, with the second, after To was deployed or updated).
	AutoRestart string `json:"autoRestart"`
}

// Auto-restart policies of an edge.
const (
	AutoRestartOff       = "off"
	AutoRestartRecover   = "on_recover"
	AutoRestartAndDeploy = "on_recover_and_deploy"
)

// ValidAutoRestart lists the policies.
var ValidAutoRestart = map[string]bool{AutoRestartOff: true, AutoRestartRecover: true, AutoRestartAndDeploy: true}

func (d StackDep) pair() string { return d.From.Key() + ">" + d.To.Key() }

const depCols = `id, from_host, from_stack, to_host, to_stack, source, reason, detail, ignored, created_by, created_at, auto_restart`

func scanDep(r scanner) (StackDep, error) {
	var d StackDep
	var ignored int
	var created int64
	if err := r.Scan(&d.ID, &d.From.HostID, &d.From.Stack, &d.To.HostID, &d.To.Stack, &d.Source, &d.Reason, &d.Detail, &ignored, &d.CreatedBy, &created, &d.AutoRestart); err != nil {
		return StackDep{}, err
	}
	d.Ignored = ignored == 1
	d.CreatedAt = time.Unix(created, 0)
	return d, nil
}

func (s *Store) queryDeps(ctx context.Context, where string, args ...any) ([]StackDep, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+depCols+` FROM stack_deps `+where+` ORDER BY from_host, from_stack, to_host, to_stack`, args...)
	if err != nil {
		return nil, fmt.Errorf("stack deps: %w", err)
	}
	defer rows.Close()
	var out []StackDep
	for rows.Next() {
		d, err := scanDep(rows)
		if err != nil {
			return nil, fmt.Errorf("scan stack dep: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// StackDeps lists every edge, ignored ones included.
func (s *Store) StackDeps(ctx context.Context) ([]StackDep, error) {
	return s.queryDeps(ctx, ``)
}

// StackDepsOfHost lists the edges that start or end on a host.
func (s *Store) StackDepsOfHost(ctx context.Context, hostID string) ([]StackDep, error) {
	return s.queryDeps(ctx, `WHERE from_host = ? OR to_host = ?`, hostID, hostID)
}

// StackDepByID returns one edge.
func (s *Store) StackDepByID(ctx context.Context, id int64) (StackDep, error) {
	d, err := scanDep(s.db.QueryRowContext(ctx, `SELECT `+depCols+` FROM stack_deps WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return StackDep{}, ErrNotFound
	}
	return d, err
}

// ReplaceDetected stores the result of one detection run on hostID. Detected edges of the
// host that were not found again are removed, unless an admin ignored them (they stay, so
// the edge stays ignored when it is detected again later). Manual edges are never touched:
// a detected edge that an admin already added by hand stays manual.
func (s *Store) ReplaceDetected(ctx context.Context, hostID string, found []StackDep) (added, removed int, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	want := map[string]StackDep{}
	for _, d := range found {
		if d.From.HostID != hostID || d.To.HostID != hostID || d.From == d.To {
			continue // detection never crosses hosts
		}
		want[d.pair()] = d
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+depCols+` FROM stack_deps WHERE from_host = ? AND to_host = ?`, hostID, hostID)
	if err != nil {
		return 0, 0, fmt.Errorf("load detected: %w", err)
	}
	var have []StackDep
	for rows.Next() {
		d, err := scanDep(rows)
		if err != nil {
			rows.Close()
			return 0, 0, err
		}
		have = append(have, d)
	}
	rows.Close()
	now := time.Now().Unix()
	for _, d := range have {
		w, still := want[d.pair()]
		delete(want, d.pair())
		switch {
		case d.Source != DepDetected:
		case !still && !d.Ignored && (d.AutoRestart == "" || d.AutoRestart == AutoRestartOff):
			if _, err := tx.ExecContext(ctx, `DELETE FROM stack_deps WHERE id = ?`, d.ID); err != nil {
				return 0, 0, fmt.Errorf("remove detected: %w", err)
			}
			removed++
		case still && (w.Reason != d.Reason || w.Detail != d.Detail):
			if _, err := tx.ExecContext(ctx, `UPDATE stack_deps SET reason = ?, detail = ? WHERE id = ?`, w.Reason, w.Detail, d.ID); err != nil {
				return 0, 0, fmt.Errorf("update detected: %w", err)
			}
		}
	}
	for _, d := range want {
		if _, err := tx.ExecContext(ctx, `INSERT INTO stack_deps (from_host, from_stack, to_host, to_stack, source, reason, detail, ignored, created_by, created_at)
			VALUES (?, ?, ?, ?, 'detected', ?, ?, 0, '', ?)`, d.From.HostID, d.From.Stack, d.To.HostID, d.To.Stack, d.Reason, d.Detail, now); err != nil {
			return 0, 0, fmt.Errorf("add detected: %w", err)
		}
		added++
	}
	return added, removed, tx.Commit()
}

// AddManualDep adds an edge by hand. An existing edge of the same pair becomes manual and
// is no longer ignored, so a later detection run keeps it.
func (s *Store) AddManualDep(ctx context.Context, from, to StackRef, by string) (StackDep, error) {
	if from == to {
		return StackDep{}, errors.New("a stack can't depend on itself")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO stack_deps (from_host, from_stack, to_host, to_stack, source, reason, detail, ignored, created_by, created_at)
		VALUES (?, ?, ?, ?, 'manual', '', '', 0, ?, ?)
		ON CONFLICT (from_host, from_stack, to_host, to_stack) DO UPDATE SET source = 'manual', ignored = 0, created_by = excluded.created_by`,
		from.HostID, from.Stack, to.HostID, to.Stack, by, time.Now().Unix())
	if err != nil {
		return StackDep{}, fmt.Errorf("add manual dep: %w", err)
	}
	d, err := scanDep(s.db.QueryRowContext(ctx, `SELECT `+depCols+` FROM stack_deps WHERE from_host = ? AND from_stack = ? AND to_host = ? AND to_stack = ?`,
		from.HostID, from.Stack, to.HostID, to.Stack))
	if err != nil {
		return StackDep{}, fmt.Errorf("read manual dep: %w", err)
	}
	return d, nil
}

// SetDepAutoRestart sets the auto-restart policy of an edge.
func (s *Store) SetDepAutoRestart(ctx context.Context, id int64, mode string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE stack_deps SET auto_restart = ? WHERE id = ?`, mode, id)
	if err != nil {
		return fmt.Errorf("auto-restart: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteDep removes one edge.
func (s *Store) DeleteDep(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM stack_deps WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete dep: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetDepIgnored marks a detected edge as rejected (or accepted again).
func (s *Store) SetDepIgnored(ctx context.Context, id int64, ignored bool) error {
	v := 0
	if ignored {
		v = 1
	}
	res, err := s.db.ExecContext(ctx, `UPDATE stack_deps SET ignored = ? WHERE id = ?`, v, id)
	if err != nil {
		return fmt.Errorf("ignore dep: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteDepsOfHost removes every edge that touches a removed host.
func (s *Store) DeleteDepsOfHost(ctx context.Context, hostID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM stack_deps WHERE from_host = ? OR to_host = ?`, hostID, hostID)
	return err
}

// DeleteDepsOfStack removes every edge that touches a deleted stack.
func (s *Store) DeleteDepsOfStack(ctx context.Context, r StackRef) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM stack_deps WHERE (from_host = ? AND from_stack = ?) OR (to_host = ? AND to_stack = ?)`,
		r.HostID, r.Stack, r.HostID, r.Stack)
	return err
}

// SetIncidentCause records the cascade root ("host/stack") an incident was folded into.
// It never overwrites a cause that is already set.
func (s *Store) SetIncidentCause(ctx context.Context, id int64, causedBy string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE incidents SET caused_by = ? WHERE id = ? AND caused_by = ''`, causedBy, id)
	return err
}
