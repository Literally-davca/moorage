package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Terminal session end reasons.
const (
	TermReasonExit       = "exit"
	TermReasonClosed     = "closed"
	TermReasonIdle       = "idle"
	TermReasonLimit      = "limit"
	TermReasonRevoked    = "revoked"
	TermReasonError      = "error"
	TermReasonHubStopped = "hub-stopped"
)

// TerminalAuditRow records one browser terminal session (who, where, when, how it ended).
// Typed commands and terminal output are NEVER recorded.
type TerminalAuditRow struct {
	ID            int64     `json:"id"`
	User          string    `json:"user"`
	IP            string    `json:"ip"`
	HostID        string    `json:"hostId"`
	Stack         string    `json:"stack"`
	ContainerName string    `json:"containerName"`
	Service       string    `json:"service,omitempty"`
	Shell         string    `json:"shell"`
	StartedAt     time.Time `json:"startedAt"`
	EndedAt       time.Time `json:"endedAt,omitzero"`
	Reason        string    `json:"reason,omitempty"`
	ExitCode      *int      `json:"exitCode,omitempty"`
}

// StartTerminalSession inserts a new audit row when a terminal session begins.
func (s *Store) StartTerminalSession(ctx context.Context, r TerminalAuditRow) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO terminal_sessions (user, ip, host_id, stack, container_name, service, shell, started_at, ended_at, reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, '')`,
		r.User, r.IP, r.HostID, r.Stack, r.ContainerName, r.Service, r.Shell, r.StartedAt.Unix(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// EndTerminalSession records when and why a terminal session ended, plus its exit code if known.
func (s *Store) EndTerminalSession(ctx context.Context, id int64, endedAt time.Time, reason string, exitCode *int) error {
	var code sql.NullInt64
	if exitCode != nil {
		code = sql.NullInt64{Int64: int64(*exitCode), Valid: true}
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE terminal_sessions SET ended_at = ?, reason = ?, exit_code = ? WHERE id = ?`,
		endedAt.Unix(), reason, code, id,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CloseAbandonedTerminals marks any terminal sessions that had no end (e.g. from a previous
// hub crash or stop) as ended with reason "hub-stopped".
func (s *Store) CloseAbandonedTerminals(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE terminal_sessions SET ended_at = ?, reason = ? WHERE ended_at = 0`,
		now.Unix(), TermReasonHubStopped,
	)
	return err
}

// TerminalSessionByID loads one terminal audit row by ID.
func (s *Store) TerminalSessionByID(ctx context.Context, id int64) (TerminalAuditRow, error) {
	var r TerminalAuditRow
	var started, ended int64
	var code sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user, ip, host_id, stack, container_name, service, shell, started_at, ended_at, reason, exit_code
		 FROM terminal_sessions WHERE id = ?`, id,
	).Scan(&r.ID, &r.User, &r.IP, &r.HostID, &r.Stack, &r.ContainerName, &r.Service, &r.Shell, &started, &ended, &r.Reason, &code)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	r.StartedAt = time.Unix(started, 0)
	if ended > 0 {
		r.EndedAt = time.Unix(ended, 0)
	}
	if code.Valid {
		c := int(code.Int64)
		r.ExitCode = &c
	}
	return r, nil
}

// TerminalSessions lists terminal session audit rows, newest first.
// Pass hostID and stack to filter to a single stack, or "" to list globally.
func (s *Store) TerminalSessions(ctx context.Context, hostID, stack string, limit int) ([]TerminalAuditRow, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT id, user, ip, host_id, stack, container_name, service, shell, started_at, ended_at, reason, exit_code
	      FROM terminal_sessions`
	var args []any
	if hostID != "" && stack != "" {
		q += ` WHERE host_id = ? AND stack = ?`
		args = append(args, hostID, stack)
	} else if hostID != "" {
		q += ` WHERE host_id = ?`
		args = append(args, hostID)
	}
	q += ` ORDER BY started_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TerminalAuditRow{}
	for rows.Next() {
		var r TerminalAuditRow
		var started, ended int64
		var code sql.NullInt64
		if err := rows.Scan(
			&r.ID, &r.User, &r.IP, &r.HostID, &r.Stack, &r.ContainerName, &r.Service, &r.Shell,
			&started, &ended, &r.Reason, &code,
		); err != nil {
			return nil, err
		}
		r.StartedAt = time.Unix(started, 0)
		if ended > 0 {
			r.EndedAt = time.Unix(ended, 0)
		}
		if code.Valid {
			c := int(code.Int64)
			r.ExitCode = &c
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneTerminalSessions removes terminal audit rows older than cutoff (1 year).
func (s *Store) PruneTerminalSessions(ctx context.Context, cutoff time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM terminal_sessions WHERE started_at < ?`, cutoff.Unix())
	return err
}
