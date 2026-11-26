package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Container event types (M18A/M18F). The recorder in monitor/containers.go writes them
// from what it sees in the stacks of checked hosts.
const (
	ContainerDie       = "die"           // a container stopped by itself (exit code set)
	ContainerRestart   = "restart"       // Docker is restarting it (restart policy)
	ContainerUnhealthy = "health_status" // Detail is the new health: unhealthy / healthy
)

// ContainerEvent is something that happened to a container of a stack.
type ContainerEvent struct {
	ID          int64
	HostID      string
	Stack       string
	Service     string
	ContainerID string
	Type        string
	Detail      string
	ExitCode    *int
	Time        time.Time
	Count       int // repeats merged into this row (crash loops)
}

const containerEventCols = `id, host_id, stack, service, container_id, type, detail, exit_code, time, count`

// containerEventMerge is how close a repeat of the same event must be to be merged.
const containerEventMerge = 10 * time.Minute

func scanContainerEvent(r scanner) (ContainerEvent, error) {
	var e ContainerEvent
	var t int64
	var ec sql.NullInt64
	if err := r.Scan(&e.ID, &e.HostID, &e.Stack, &e.Service, &e.ContainerID, &e.Type, &e.Detail, &ec, &t, &e.Count); err != nil {
		return ContainerEvent{}, err
	}
	e.Time = time.UnixMilli(t)
	if ec.Valid {
		v := int(ec.Int64)
		e.ExitCode = &v
	}
	return e, nil
}

// ContainerEvents returns the events of a stack in [since, until], newest first.
func (s *Store) ContainerEvents(ctx context.Context, hostID, stack string, since, until time.Time) ([]ContainerEvent, error) {
	q := `SELECT ` + containerEventCols + ` FROM container_events WHERE host_id = ? AND stack = ? AND time >= ? AND time <= ? ORDER BY time DESC LIMIT 500`
	rows, err := s.db.QueryContext(ctx, q, hostID, stack, since.UnixMilli(), until.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContainerEvent
	for rows.Next() {
		e, err := scanContainerEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AddContainerEvent records an event. A repeat of the same event of the same service
// (same type, detail and exit code) within 10 minutes only raises the count of the
// previous row, so a crash loop is one row and doesn't grow the database.
func (s *Store) AddContainerEvent(ctx context.Context, e ContainerEvent) error {
	var id int64
	var ec any
	if e.ExitCode != nil {
		ec = *e.ExitCode
	}
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM container_events
		 WHERE host_id = ? AND stack = ? AND service = ? AND type = ? AND detail = ? AND exit_code IS ? AND time >= ?
		 ORDER BY time DESC LIMIT 1`,
		e.HostID, e.Stack, e.Service, e.Type, e.Detail, ec, e.Time.Add(-containerEventMerge).UnixMilli(),
	).Scan(&id)
	switch {
	case err == nil:
		_, err = s.db.ExecContext(ctx, `UPDATE container_events SET count = count + 1, time = ?, container_id = ? WHERE id = ?`,
			e.Time.UnixMilli(), e.ContainerID, id)
		return err
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO container_events (host_id, stack, service, container_id, type, detail, exit_code, time, count)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		e.HostID, e.Stack, e.Service, e.ContainerID, e.Type, e.Detail, ec, e.Time.UnixMilli())
	return err
}

// PruneContainerEvents deletes events older than before.
func (s *Store) PruneContainerEvents(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM container_events WHERE time < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- incident timeline and Rollback & Verify (M18A) -----------------------------

// SetIncidentTimeline stores the causal timeline (JSON) of an incident.
func (s *Store) SetIncidentTimeline(ctx context.Context, id int64, data string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE incidents SET timeline_json = ? WHERE id = ?`, data, id)
	return err
}

// IncidentTimeline returns the causal timeline (JSON) of an incident; "{}" when none.
func (s *Store) IncidentTimeline(ctx context.Context, id int64) (string, error) {
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT timeline_json FROM incidents WHERE id = ?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return data, err
}

// SetIncidentRollback stores the Rollback & Verify state (JSON) of an incident.
func (s *Store) SetIncidentRollback(ctx context.Context, id int64, data string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE incidents SET rollback_verify_json = ? WHERE id = ?`, data, id)
	return err
}

// IncidentRollback returns the Rollback & Verify state (JSON) of an incident; "{}" when none.
func (s *Store) IncidentRollback(ctx context.Context, id int64) (string, error) {
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT rollback_verify_json FROM incidents WHERE id = ?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return data, err
}

// IncidentsWithActiveRollback returns the ids of incidents whose Rollback & Verify state
// says it is still running (used after a restart, when nothing runs any more).
func (s *Store) IncidentsWithActiveRollback(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM incidents WHERE json_extract(rollback_verify_json, '$.active') = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
