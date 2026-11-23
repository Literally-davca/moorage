package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// LocationResult is one location's answer in a multi-location check run.
// Voted is false when the location didn't answer (offline, unsupported, busy, timeout);
// OK is meaningful only when Voted.
type LocationResult struct {
	CheckID   int64
	TS        time.Time
	Location  string
	Voted     bool
	OK        bool
	LatencyMS int
	Msg       string
}

// AddLocationResults stores the per-location answers of one run in one transaction.
func (s *Store) AddLocationResults(ctx context.Context, rs []LocationResult) error {
	if len(rs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, r := range rs {
		var ok any // NULL = no vote
		if r.Voted {
			ok = boolInt(r.OK)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO check_location_results (check_id, ts, location, ok, latency_ms, msg) VALUES (?, ?, ?, ?, ?, ?)`,
			r.CheckID, r.TS.UnixMilli(), r.Location, ok, r.LatencyMS, r.Msg); err != nil {
			return fmt.Errorf("location result: %w", err)
		}
	}
	return tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// RecentLocationResults returns the newest n per-location rows of a check, oldest first.
func (s *Store) RecentLocationResults(ctx context.Context, checkID int64, n int) ([]LocationResult, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT check_id, ts, location, ok, latency_ms, msg FROM
		(SELECT * FROM check_location_results WHERE check_id = ? ORDER BY ts DESC LIMIT ?) ORDER BY ts`, checkID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LocationResult
	for rows.Next() {
		var r LocationResult
		var ts int64
		var ok sql.NullInt64
		if err := rows.Scan(&r.CheckID, &ts, &r.Location, &ok, &r.LatencyMS, &r.Msg); err != nil {
			return nil, err
		}
		r.TS = time.UnixMilli(ts)
		r.Voted, r.OK = ok.Valid, ok.Valid && ok.Int64 == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneLocationResults deletes per-location rows older than t (same retention as results).
func (s *Store) PruneLocationResults(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM check_location_results WHERE ts < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
