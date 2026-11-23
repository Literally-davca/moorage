package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Maintenance window kinds.
const (
	MaintenanceOnce   = "once"
	MaintenanceWeekly = "weekly"
)

// MaintenanceStack is one host/stack pair in a window's scope.
type MaintenanceStack struct {
	HostID string `json:"hostId"`
	Stack  string `json:"stack"`
}

// MaintenanceScope says what a window covers: everything, or the listed stacks and checks.
type MaintenanceScope struct {
	All    bool               `json:"all,omitempty"`
	Stacks []MaintenanceStack `json:"stacks,omitempty"`
	Checks []int64            `json:"checks,omitempty"`
}

// MaintenanceWindow is a planned maintenance: one-off (StartsAt–EndsAt) or weekly
// (Weekdays at StartTime in TZ for Duration minutes).
type MaintenanceWindow struct {
	ID        int64
	Title     string
	Message   string // shown publicly on status pages
	Scope     MaintenanceScope
	Kind      string
	StartsAt  time.Time // once
	EndsAt    time.Time // once
	Weekdays  []int     // weekly: 0 = Sunday … 6 = Saturday
	StartTime string    // weekly: "HH:MM" local time in TZ
	Duration  int       // weekly: minutes
	TZ        string    // weekly: IANA zone; empty = the hub's zone
	Enabled   bool
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

const maintenanceCols = `id, title, message, scope, kind, starts_at, ends_at, weekdays, start_time, duration_m, tz, enabled, created_by, created_at, updated_at`

func scanMaintenance(r scanner) (MaintenanceWindow, error) {
	var w MaintenanceWindow
	var scope string
	var starts, ends, created, updated int64
	var days, enabled int
	if err := r.Scan(&w.ID, &w.Title, &w.Message, &scope, &w.Kind, &starts, &ends, &days, &w.StartTime, &w.Duration, &w.TZ, &enabled, &w.CreatedBy, &created, &updated); err != nil {
		return MaintenanceWindow{}, err
	}
	if err := json.Unmarshal([]byte(scope), &w.Scope); err != nil {
		return MaintenanceWindow{}, fmt.Errorf("maintenance window %d scope: %w", w.ID, err)
	}
	if starts > 0 {
		w.StartsAt = time.Unix(starts, 0)
	}
	if ends > 0 {
		w.EndsAt = time.Unix(ends, 0)
	}
	for d := range 7 {
		if days&(1<<d) != 0 {
			w.Weekdays = append(w.Weekdays, d)
		}
	}
	w.Enabled = enabled == 1
	w.CreatedAt = time.Unix(created, 0)
	w.UpdatedAt = time.Unix(updated, 0)
	return w, nil
}

// MaintenanceWindows lists all windows, oldest first.
func (s *Store) MaintenanceWindows(ctx context.Context) ([]MaintenanceWindow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+maintenanceCols+` FROM maintenance_windows ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MaintenanceWindow
	for rows.Next() {
		w, err := scanMaintenance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// MaintenanceWindowByID returns one window.
func (s *Store) MaintenanceWindowByID(ctx context.Context, id int64) (MaintenanceWindow, error) {
	w, err := scanMaintenance(s.db.QueryRowContext(ctx, `SELECT `+maintenanceCols+` FROM maintenance_windows WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return MaintenanceWindow{}, ErrNotFound
	}
	return w, err
}

// SaveMaintenanceWindow inserts (ID 0) or updates a window. CreatedBy is kept on update.
func (s *Store) SaveMaintenanceWindow(ctx context.Context, w MaintenanceWindow) (int64, error) {
	scope, err := json.Marshal(w.Scope)
	if err != nil {
		return 0, err
	}
	days := 0
	for _, d := range w.Weekdays {
		if d >= 0 && d < 7 {
			days |= 1 << d
		}
	}
	var starts, ends int64
	if !w.StartsAt.IsZero() {
		starts = w.StartsAt.Unix()
	}
	if !w.EndsAt.IsZero() {
		ends = w.EndsAt.Unix()
	}
	enabled := 0
	if w.Enabled {
		enabled = 1
	}
	now := time.Now().Unix()
	if w.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO maintenance_windows
			(title, message, scope, kind, starts_at, ends_at, weekdays, start_time, duration_m, tz, enabled, created_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			w.Title, w.Message, string(scope), w.Kind, starts, ends, days, w.StartTime, w.Duration, w.TZ, enabled, w.CreatedBy, now, now)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE maintenance_windows SET title=?, message=?, scope=?, kind=?, starts_at=?, ends_at=?,
		weekdays=?, start_time=?, duration_m=?, tz=?, enabled=?, updated_at=? WHERE id=?`,
		w.Title, w.Message, string(scope), w.Kind, starts, ends, days, w.StartTime, w.Duration, w.TZ, enabled, now, w.ID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	return w.ID, nil
}

// DeleteMaintenanceWindow removes a window.
func (s *Store) DeleteMaintenanceWindow(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM maintenance_windows WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// LastResult returns the newest result of a check, or ErrNotFound.
func (s *Store) LastResult(ctx context.Context, checkID int64) (Result, error) {
	rs, err := s.RecentResults(ctx, checkID, 1)
	if err != nil {
		return Result{}, err
	}
	if len(rs) == 0 {
		return Result{}, ErrNotFound
	}
	return rs[0], nil
}
