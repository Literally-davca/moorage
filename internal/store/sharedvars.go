package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// SharedVar is a reusable variable offered to templates and stack .env editors.
// HostID "" means global across all hosts.
type SharedVar struct {
	ID        int64     `json:"id"`
	Key       string    `json:"key"`
	Value     string    `json:"value,omitempty"`
	HostID    string    `json:"hostId"`
	Secret    bool      `json:"secret"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (s *Store) ListSharedVars(ctx context.Context) ([]SharedVar, error) {
	if s == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, key, value, host_id, secret, updated_at FROM shared_vars ORDER BY host_id, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SharedVar
	for rows.Next() {
		var v SharedVar
		var sec int
		var upd int64
		if err := rows.Scan(&v.ID, &v.Key, &v.Value, &v.HostID, &sec, &upd); err != nil {
			return nil, err
		}
		v.Secret = sec != 0
		v.UpdatedAt = time.Unix(upd, 0).UTC()
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) SharedVarByID(ctx context.Context, id int64) (SharedVar, error) {
	var v SharedVar
	var sec int
	var upd int64
	err := s.db.QueryRowContext(ctx, `SELECT id, key, value, host_id, secret, updated_at FROM shared_vars WHERE id = ?`, id).
		Scan(&v.ID, &v.Key, &v.Value, &v.HostID, &sec, &upd)
	if errors.Is(err, sql.ErrNoRows) {
		return SharedVar{}, ErrNotFound
	}
	if err != nil {
		return SharedVar{}, err
	}
	v.Secret = sec != 0
	v.UpdatedAt = time.Unix(upd, 0).UTC()
	return v, nil
}

// UpsertSharedVar creates or updates a shared variable by (key, host_id).
// When keepExistingSecret is true and value is empty on an existing secret row, its stored value is preserved.
func (s *Store) UpsertSharedVar(ctx context.Context, v SharedVar, keepExistingSecret bool) (SharedVar, error) {
	key := strings.TrimSpace(v.Key)
	hostID := strings.TrimSpace(v.HostID)
	sec := 0
	if v.Secret {
		sec = 1
	}
	now := time.Now().Unix()
	val := v.Value
	if keepExistingSecret && v.Secret && val == "" {
		var cur string
		if err := s.db.QueryRowContext(ctx, `SELECT value FROM shared_vars WHERE key = ? AND host_id = ?`, key, hostID).Scan(&cur); err == nil {
			val = cur
		}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO shared_vars (key, value, host_id, secret, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(key, host_id) DO UPDATE SET
			value = excluded.value,
			secret = excluded.secret,
			updated_at = excluded.updated_at`,
		key, val, hostID, sec, now,
	)
	if err != nil {
		return SharedVar{}, err
	}
	var out SharedVar
	var outSec int
	var outUpd int64
	if err := s.db.QueryRowContext(ctx, `SELECT id, key, value, host_id, secret, updated_at FROM shared_vars WHERE key = ? AND host_id = ?`, key, hostID).
		Scan(&out.ID, &out.Key, &out.Value, &out.HostID, &outSec, &outUpd); err != nil {
		return SharedVar{}, err
	}
	out.Secret = outSec != 0
	out.UpdatedAt = time.Unix(outUpd, 0).UTC()
	return out, nil
}

func (s *Store) DeleteSharedVar(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM shared_vars WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
