package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SystemUpdate statuses persisted in system_updates.
const (
	SysUpdateRunning    = "running"
	SysUpdateRestarting = "restarting"
	SysUpdateDone       = "done"
	SysUpdateFailed     = "failed"
	SysUpdateRolledBack = "rolled-back"
)

// SystemUpdate actions persisted in system_updates.
const (
	SysUpdateActionApply    = "apply"
	SysUpdateActionRollback = "rollback"
)

// SystemUpdate represents one tracked self-update or rollback job on a Moorage node.
type SystemUpdate struct {
	ID          int64
	NodeID      string
	NodeName    string
	Action      string // SysUpdateActionApply | SysUpdateActionRollback
	FromVersion string
	ToVersion   string
	Mode        string // container | binary | unknown
	Status      string // running | restarting | done | failed | rolled-back
	Message     string
	Actor       string
	IP          string
	StartedAt   time.Time
	FinishedAt  time.Time
}

// CreateSystemUpdate inserts a new self-update job and returns it with its ID populated.
func (s *Store) CreateSystemUpdate(ctx context.Context, u SystemUpdate) (SystemUpdate, error) {
	if u.StartedAt.IsZero() {
		u.StartedAt = time.Now()
	}
	var finished int64
	if !u.FinishedAt.IsZero() {
		finished = u.FinishedAt.Unix()
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO system_updates
			(node_id, node_name, action, from_version, to_version, mode, status, message, actor, ip, started_at, finished_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		u.NodeID, u.NodeName, u.Action, u.FromVersion, u.ToVersion, u.Mode, u.Status, u.Message, u.Actor, u.IP,
		u.StartedAt.Unix(), finished)
	if err != nil {
		return u, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return u, err
	}
	u.ID = id
	return u, nil
}

// UpdateSystemUpdate updates the status, message, and finish time of a tracked job.
func (s *Store) UpdateSystemUpdate(ctx context.Context, id int64, status, message string, finishedAt time.Time) error {
	var finished int64
	if !finishedAt.IsZero() {
		finished = finishedAt.Unix()
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE system_updates SET status = ?, message = ?, finished_at = ? WHERE id = ?`,
		status, message, finished, id)
	return err
}

// LatestSystemUpdateByNode returns the most recent job per node_id.
func (s *Store) LatestSystemUpdateByNode(ctx context.Context) (map[string]SystemUpdate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT su.id, su.node_id, su.node_name, su.action, su.from_version, su.to_version, su.mode,
		       su.status, su.message, su.actor, su.ip, su.started_at, su.finished_at
		FROM system_updates su
		JOIN (
			SELECT node_id, MAX(id) AS max_id FROM system_updates GROUP BY node_id
		) latest ON su.id = latest.max_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]SystemUpdate)
	for rows.Next() {
		u, err := scanSystemUpdate(rows)
		if err != nil {
			return nil, err
		}
		out[u.NodeID] = u
	}
	return out, rows.Err()
}

// OpenSystemUpdates returns all unfinished jobs (running / restarting), oldest first.
func (s *Store) OpenSystemUpdates(ctx context.Context) ([]SystemUpdate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, node_id, node_name, action, from_version, to_version, mode,
		       status, message, actor, ip, started_at, finished_at
		FROM system_updates
		WHERE status IN (?, ?)
		ORDER BY id ASC`, SysUpdateRunning, SysUpdateRestarting)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SystemUpdate
	for rows.Next() {
		u, err := scanSystemUpdate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SystemUpdateByID returns one job by ID or ErrNotFound.
func (s *Store) SystemUpdateByID(ctx context.Context, id int64) (SystemUpdate, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, node_id, node_name, action, from_version, to_version, mode,
		       status, message, actor, ip, started_at, finished_at
		FROM system_updates WHERE id = ?`, id)
	u, err := scanSystemUpdate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

func scanSystemUpdate(sc interface{ Scan(dest ...any) error }) (SystemUpdate, error) {
	var u SystemUpdate
	var started, finished int64
	err := sc.Scan(&u.ID, &u.NodeID, &u.NodeName, &u.Action, &u.FromVersion, &u.ToVersion, &u.Mode,
		&u.Status, &u.Message, &u.Actor, &u.IP, &started, &finished)
	if err != nil {
		return u, err
	}
	if started > 0 {
		u.StartedAt = time.Unix(started, 0)
	}
	if finished > 0 {
		u.FinishedAt = time.Unix(finished, 0)
	}
	return u, nil
}
