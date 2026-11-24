package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Setting keys stored in app_settings.
const (
	SettingPublicURL        = "public_url"
	SettingStatusPublicURL  = "status_public_url"
	SettingAgentPublicAddr  = "agent_public_addr"
	SettingMetricsToken     = "metrics_token"
	SettingUpdateChannel    = "update_channel"
	SettingCleanupEnabled   = "cleanup_enabled"
	SettingCleanupSchedule  = "cleanup_schedule"
	SettingCleanupVolumes   = "cleanup_volumes"
	SettingOIDCJSON         = "oidc_config"
	SettingCoolifyJSON      = "coolify_config"
	SettingRegistriesJSON   = "registries_config"
	SettingBackupRemoteJSON = "backup_remote_config"
	SettingBackupLastStatus = "backup_remote_last"
	SettingSisterLinksJSON  = "sister_links"
)

// HostIPKey and BaseDomainKey store per-host network overrides in app_settings.
func HostIPKey(hostID string) string     { return "host_ip:" + hostID }
func BaseDomainKey(hostID string) string { return "base_domain:" + hostID }

// GetSetting returns a single setting value or "" if not set.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	if s == nil {
		return "", nil
	}
	var val string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE key = ?`, key).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return val, err
}

// SetSetting inserts or updates a setting in app_settings. An empty value removes the key.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	if value == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM app_settings WHERE key = ?`, key)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO app_settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UnixMilli(),
	)
	return err
}

// AllSettings returns all key/value pairs from app_settings.
func (s *Store) AllSettings(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	if s == nil {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM app_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// BackupTo writes a consistent snapshot of the SQLite database to destPath using VACUUM INTO.
func (s *Store) BackupTo(ctx context.Context, destPath string) error {
	escaped := strings.ReplaceAll(destPath, "'", "''")
	_, err := s.db.ExecContext(ctx, "VACUUM INTO '"+escaped+"'")
	return err
}

// AutoRollback records an automatic rollback triggered after a failed post-update check.
type AutoRollback struct {
	ID           int64     `json:"id"`
	HostID       string    `json:"hostId"`
	Stack        string    `json:"stack"`
	UpdateID     int64     `json:"updateId"`
	CheckName    string    `json:"checkName"`
	Reason       string    `json:"reason"`
	At           time.Time `json:"at"`
	Acknowledged bool      `json:"acknowledged"`
}

func (s *Store) HasAutoRollbackForUpdate(ctx context.Context, updateID int64) (bool, error) {
	if s == nil {
		return false, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auto_rollbacks WHERE update_id = ?`, updateID).Scan(&n)
	return n > 0, err
}

// SetAutoRollbackReason replaces the reason of a recorded auto-rollback (e.g. when it failed).
func (s *Store) SetAutoRollbackReason(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE auto_rollbacks SET reason = ? WHERE id = ?`, reason, id)
	return err
}

func (s *Store) InsertAutoRollback(ctx context.Context, r AutoRollback) (int64, error) {
	if r.At.IsZero() {
		r.At = time.Now()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO auto_rollbacks (host_id, stack, update_id, check_name, reason, at, acknowledged)
		 VALUES (?, ?, ?, ?, ?, ?, 0)`,
		r.HostID, r.Stack, r.UpdateID, r.CheckName, r.Reason, r.At.UnixMilli(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListUnackedAutoRollbacks(ctx context.Context) ([]AutoRollback, error) {
	if s == nil {
		return []AutoRollback{}, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, host_id, stack, update_id, check_name, reason, at, acknowledged
		 FROM auto_rollbacks WHERE acknowledged = 0 ORDER BY at DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AutoRollback{}
	for rows.Next() {
		var r AutoRollback
		var atMs int64
		var ack int
		if err := rows.Scan(&r.ID, &r.HostID, &r.Stack, &r.UpdateID, &r.CheckName, &r.Reason, &atMs, &ack); err != nil {
			return nil, err
		}
		r.At = time.UnixMilli(atMs).UTC()
		r.Acknowledged = ack != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) AckAutoRollback(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE auto_rollbacks SET acknowledged = 1 WHERE id = ?`, id)
	return err
}
