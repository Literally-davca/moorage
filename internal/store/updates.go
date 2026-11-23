package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Update policy constants.
const (
	PolicyOff    = "off"
	PolicyNotify = "notify"
	PolicyAuto   = "auto"
)

// Update result constants.
const (
	UpdateOK         = "ok"
	UpdatePullFailed = "pull-failed"
	UpdateUpFailed   = "up-failed"
	UpdateRolledBack = "rolled-back"
)

// UpdateSettings stores global image update checking and apply-window settings.
type UpdateSettings struct {
	Enabled     bool    `json:"enabled"`
	Interval    int     `json:"interval"`    // seconds, default 21600 (6h)
	WindowStart string  `json:"windowStart"` // HH:MM in hub local time
	WindowEnd   string  `json:"windowEnd"`   // HH:MM in hub local time
	Channels    []int64 `json:"channels"`
}

// StackUpdatePolicy stores the update policy and rollback safeguards for one stack.
type StackUpdatePolicy struct {
	HostID         string            `json:"hostId"`
	Stack          string            `json:"stack"`
	Policy         string            `json:"policy"` // off | notify | auto
	SkippedDigests map[string]string `json:"skippedDigests,omitempty"`
	LastAutoAt     time.Time         `json:"lastAutoAt,omitzero"`
}

// ImageCheckRow is the persisted check state of one service in a stack.
type ImageCheckRow struct {
	HostID              string    `json:"hostId"`
	Stack               string    `json:"stack"`
	Service             string    `json:"service"`
	Ref                 string    `json:"ref"`
	LocalID             string    `json:"localId,omitempty"`
	LocalDigest         string    `json:"localDigest,omitempty"`
	RunningID           string    `json:"runningId,omitempty"`
	RemoteDigest        string    `json:"remoteDigest,omitempty"`
	Status              string    `json:"status"`
	Error               string    `json:"error,omitempty"`
	NewerTag            string    `json:"newerTag,omitempty"`
	NewerMajor          string    `json:"newerMajor,omitempty"`
	CheckedAt           time.Time `json:"checkedAt,omitzero"`
	AnnouncedDigest     string    `json:"announcedDigest,omitempty"`
	AnnouncedTag        string    `json:"announcedTag,omitempty"`
	AnnouncedMajor      string    `json:"announcedMajor,omitempty"`
	AnnouncedFailDigest string    `json:"announcedFailDigest,omitempty"`
}

// ImageUpdateService records the before/after image IDs and digests of one service during an update.
type ImageUpdateService struct {
	Service   string `json:"service"`
	Ref       string `json:"ref"`
	OldID     string `json:"oldId,omitempty"`
	OldDigest string `json:"oldDigest,omitempty"`
	NewID     string `json:"newId,omitempty"`
	NewDigest string `json:"newDigest,omitempty"`
}

// ImageUpdate is one update execution record (auto-update or manual update).
type ImageUpdate struct {
	ID         int64                `json:"id"`
	HostID     string               `json:"hostId"`
	Stack      string               `json:"stack"`
	StartedAt  time.Time            `json:"startedAt"`
	FinishedAt time.Time            `json:"finishedAt,omitzero"`
	Actor      string               `json:"actor"`
	Result     string               `json:"result"` // ok | pull-failed | up-failed | rolled-back
	Services   []ImageUpdateService `json:"services"`
	LogTail    string               `json:"logTail,omitempty"`
}

// UpdateSettings loads the global image update settings.
func (s *Store) UpdateSettings(ctx context.Context) (UpdateSettings, error) {
	out := UpdateSettings{
		Enabled:     true,
		Interval:    21600,
		WindowStart: "03:00",
		WindowEnd:   "05:00",
		Channels:    []int64{},
	}
	var en int
	var chJSON string
	err := s.db.QueryRowContext(ctx,
		`SELECT enabled, interval_s, window_start, window_end, channels FROM update_settings WHERE id = 1`,
	).Scan(&en, &out.Interval, &out.WindowStart, &out.WindowEnd, &chJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Enabled = en == 1
	_ = json.Unmarshal([]byte(chJSON), &out.Channels)
	if out.Channels == nil {
		out.Channels = []int64{}
	}
	return out, nil
}

// SaveUpdateSettings persists the global image update settings.
func (s *Store) SaveUpdateSettings(ctx context.Context, st UpdateSettings) error {
	en := 0
	if st.Enabled {
		en = 1
	}
	if st.Channels == nil {
		st.Channels = []int64{}
	}
	chJSON, _ := json.Marshal(st.Channels)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO update_settings (id, enabled, interval_s, window_start, window_end, channels)
		 VALUES (1, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET enabled = excluded.enabled, interval_s = excluded.interval_s,
		 window_start = excluded.window_start, window_end = excluded.window_end, channels = excluded.channels`,
		en, st.Interval, st.WindowStart, st.WindowEnd, string(chJSON),
	)
	return err
}

// StackUpdatePolicy loads the policy for one stack, defaulting to PolicyNotify.
func (s *Store) StackUpdatePolicy(ctx context.Context, hostID, stack string) (StackUpdatePolicy, error) {
	out := StackUpdatePolicy{
		HostID:         hostID,
		Stack:          stack,
		Policy:         PolicyNotify,
		SkippedDigests: map[string]string{},
	}
	var skippedJSON string
	var lastAuto int64
	err := s.db.QueryRowContext(ctx,
		`SELECT policy, skipped_digests, last_auto_at FROM stack_update_policies WHERE host_id = ? AND stack = ?`,
		hostID, stack,
	).Scan(&out.Policy, &skippedJSON, &lastAuto)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	_ = json.Unmarshal([]byte(skippedJSON), &out.SkippedDigests)
	if out.SkippedDigests == nil {
		out.SkippedDigests = map[string]string{}
	}
	if lastAuto > 0 {
		out.LastAutoAt = time.Unix(lastAuto, 0)
	}
	return out, nil
}

// AllStackUpdatePolicies loads all explicitly saved stack update policies keyed by "hostID/stack".
func (s *Store) AllStackUpdatePolicies(ctx context.Context) (map[string]StackUpdatePolicy, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT host_id, stack, policy, skipped_digests, last_auto_at FROM stack_update_policies`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]StackUpdatePolicy{}
	for rows.Next() {
		var p StackUpdatePolicy
		var skippedJSON string
		var lastAuto int64
		if err := rows.Scan(&p.HostID, &p.Stack, &p.Policy, &skippedJSON, &lastAuto); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(skippedJSON), &p.SkippedDigests)
		if p.SkippedDigests == nil {
			p.SkippedDigests = map[string]string{}
		}
		if lastAuto > 0 {
			p.LastAutoAt = time.Unix(lastAuto, 0)
		}
		out[p.HostID+"/"+p.Stack] = p
	}
	return out, rows.Err()
}

// SaveStackUpdatePolicy saves the policy, skipped digests, and last auto-update timestamp for a stack.
func (s *Store) SaveStackUpdatePolicy(ctx context.Context, p StackUpdatePolicy) error {
	if p.Policy == "" {
		p.Policy = PolicyNotify
	}
	if p.SkippedDigests == nil {
		p.SkippedDigests = map[string]string{}
	}
	skippedJSON, _ := json.Marshal(p.SkippedDigests)
	var lastAuto int64
	if !p.LastAutoAt.IsZero() {
		lastAuto = p.LastAutoAt.Unix()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO stack_update_policies (host_id, stack, policy, skipped_digests, last_auto_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(host_id, stack) DO UPDATE SET
		 policy = excluded.policy,
		 skipped_digests = excluded.skipped_digests,
		 last_auto_at = excluded.last_auto_at`,
		p.HostID, p.Stack, p.Policy, string(skippedJSON), lastAuto,
	)
	return err
}

// ImageChecks loads persisted image check rows for one stack (or all stacks when hostID == "" and stack == "").
func (s *Store) ImageChecks(ctx context.Context, hostID, stack string) ([]ImageCheckRow, error) {
	query := `SELECT host_id, stack, service, ref, local_id, local_digest, running_id, remote_digest,
		status, error, newer_tag, newer_major, checked_at, announced_digest, announced_tag, announced_major, announced_fail_digest
		FROM image_checks`
	var args []any
	if hostID != "" && stack != "" {
		query += ` WHERE host_id = ? AND stack = ?`
		args = append(args, hostID, stack)
	} else if hostID != "" {
		query += ` WHERE host_id = ?`
		args = append(args, hostID)
	}
	query += ` ORDER BY host_id, stack, service`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImageCheckRow
	for rows.Next() {
		var r ImageCheckRow
		var checkedAt int64
		if err := rows.Scan(
			&r.HostID, &r.Stack, &r.Service, &r.Ref, &r.LocalID, &r.LocalDigest, &r.RunningID, &r.RemoteDigest,
			&r.Status, &r.Error, &r.NewerTag, &r.NewerMajor, &checkedAt,
			&r.AnnouncedDigest, &r.AnnouncedTag, &r.AnnouncedMajor, &r.AnnouncedFailDigest,
		); err != nil {
			return nil, err
		}
		if checkedAt > 0 {
			r.CheckedAt = time.Unix(checkedAt, 0)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveStackImageChecks replaces the service check rows for (hostID, stack) while keeping announced_* columns intact
// unless updated on the passed rows.
func (s *Store) SaveStackImageChecks(ctx context.Context, hostID, stack string, rows []ImageCheckRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Remove services that no longer exist in the stack.
	if len(rows) == 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM image_checks WHERE host_id = ? AND stack = ?`, hostID, stack); err != nil {
			return err
		}
		return tx.Commit()
	}

	keep := make([]any, 0, len(rows)+2)
	keep = append(keep, hostID, stack)
	placeholders := ""
	for i, r := range rows {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		keep = append(keep, r.Service)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM image_checks WHERE host_id = ? AND stack = ? AND service NOT IN (`+placeholders+`)`,
		keep...,
	); err != nil {
		return err
	}

	for _, r := range rows {
		var ts int64
		if !r.CheckedAt.IsZero() {
			ts = r.CheckedAt.Unix()
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO image_checks (
				host_id, stack, service, ref, local_id, local_digest, running_id, remote_digest,
				status, error, newer_tag, newer_major, checked_at,
				announced_digest, announced_tag, announced_major, announced_fail_digest
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(host_id, stack, service) DO UPDATE SET
				ref = excluded.ref,
				local_id = excluded.local_id,
				local_digest = excluded.local_digest,
				running_id = excluded.running_id,
				remote_digest = excluded.remote_digest,
				status = excluded.status,
				error = excluded.error,
				newer_tag = excluded.newer_tag,
				newer_major = excluded.newer_major,
				checked_at = excluded.checked_at,
				announced_digest = excluded.announced_digest,
				announced_tag = excluded.announced_tag,
				announced_major = excluded.announced_major,
				announced_fail_digest = excluded.announced_fail_digest`,
			hostID, stack, r.Service, r.Ref, r.LocalID, r.LocalDigest, r.RunningID, r.RemoteDigest,
			r.Status, r.Error, r.NewerTag, r.NewerMajor, ts,
			r.AnnouncedDigest, r.AnnouncedTag, r.AnnouncedMajor, r.AnnouncedFailDigest,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AddImageUpdate inserts an image update record and returns its ID.
func (s *Store) AddImageUpdate(ctx context.Context, u ImageUpdate) (int64, error) {
	if u.Services == nil {
		u.Services = []ImageUpdateService{}
	}
	svcsJSON, _ := json.Marshal(u.Services)
	var fin int64
	if !u.FinishedAt.IsZero() {
		fin = u.FinishedAt.Unix()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO image_updates (host_id, stack, started_at, finished_at, actor, result, services_json, log_tail)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		u.HostID, u.Stack, u.StartedAt.Unix(), fin, u.Actor, u.Result, string(svcsJSON), u.LogTail,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateImageUpdateResult updates the result, finished_at, services, and log_tail of an existing update record.
func (s *Store) UpdateImageUpdateResult(ctx context.Context, id int64, finishedAt time.Time, result string, services []ImageUpdateService, logTail string) error {
	if services == nil {
		services = []ImageUpdateService{}
	}
	svcsJSON, _ := json.Marshal(services)
	var fin int64
	if !finishedAt.IsZero() {
		fin = finishedAt.Unix()
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE image_updates SET finished_at = ?, result = ?, services_json = ?, log_tail = ? WHERE id = ?`,
		fin, result, string(svcsJSON), logTail, id,
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

// SetImageUpdateResult changes the result column of an image update record (e.g. to "rolled-back").
func (s *Store) SetImageUpdateResult(ctx context.Context, id int64, result string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE image_updates SET result = ? WHERE id = ?`, result, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ImageUpdateByID loads one image update record by ID.
func (s *Store) ImageUpdateByID(ctx context.Context, id int64) (ImageUpdate, error) {
	var u ImageUpdate
	var started, finished int64
	var svcsJSON string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, host_id, stack, started_at, finished_at, actor, result, services_json, log_tail
		 FROM image_updates WHERE id = ?`, id,
	).Scan(&u.ID, &u.HostID, &u.Stack, &started, &finished, &u.Actor, &u.Result, &svcsJSON, &u.LogTail)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	if err != nil {
		return u, err
	}
	u.StartedAt = time.Unix(started, 0)
	if finished > 0 {
		u.FinishedAt = time.Unix(finished, 0)
	}
	_ = json.Unmarshal([]byte(svcsJSON), &u.Services)
	if u.Services == nil {
		u.Services = []ImageUpdateService{}
	}
	return u, nil
}

// ImageUpdates returns the most recent update records for a stack (or all stacks when hostID == "" and stack == "").
func (s *Store) ImageUpdates(ctx context.Context, hostID, stack string, limit int) ([]ImageUpdate, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT id, host_id, stack, started_at, finished_at, actor, result, services_json, log_tail FROM image_updates`
	var args []any
	if hostID != "" && stack != "" {
		query += ` WHERE host_id = ? AND stack = ?`
		args = append(args, hostID, stack)
	} else if hostID != "" {
		query += ` WHERE host_id = ?`
		args = append(args, hostID)
	}
	query += ` ORDER BY started_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ImageUpdate{}
	for rows.Next() {
		var u ImageUpdate
		var started, finished int64
		var svcsJSON string
		if err := rows.Scan(&u.ID, &u.HostID, &u.Stack, &started, &finished, &u.Actor, &u.Result, &svcsJSON, &u.LogTail); err != nil {
			return nil, err
		}
		u.StartedAt = time.Unix(started, 0)
		if finished > 0 {
			u.FinishedAt = time.Unix(finished, 0)
		}
		_ = json.Unmarshal([]byte(svcsJSON), &u.Services)
		if u.Services == nil {
			u.Services = []ImageUpdateService{}
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// PruneImageUpdates deletes image_updates rows older than cutoff (90 days).
func (s *Store) PruneImageUpdates(ctx context.Context, cutoff time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM image_updates WHERE started_at < ?`, cutoff.Unix())
	return err
}
