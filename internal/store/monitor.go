package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Check is a configured uptime check. Settings holds kind-specific JSON.
type Check struct {
	ID        int64
	Name      string
	Kind      string
	Target    string
	HostID    string // optional link to a stack
	Stack     string
	Interval  int // seconds
	Timeout   int // seconds
	Retries   int // failed results in a row before the check is down
	Settings  string
	Enabled   bool
	Channels  []int64
	CreatedAt time.Time
	// Locations are where the check runs from: "hub" and agent ids. Empty means the hub
	// only. Quorum decides how their answers combine (majority, any, all; empty = default).
	Locations []string
	Quorum    string
}

// Result is one probe outcome. Status: up | down | silenced | paused | maintenance.
type Result struct {
	CheckID   int64
	TS        time.Time
	Status    string
	LatencyMS int
	Msg       string
}

// Incident is a period during which a check was down.
type Incident struct {
	ID        int64
	CheckID   int64
	StartedAt time.Time
	EndedAt   time.Time // zero while open
	Cause     string
	Context   string // JSON with containers and log lines at the time of the alert
	CausedBy  string // "host/stack" of the cascade root it was folded into, or ""
}

// Channel is a notification target. Config holds kind-specific JSON (with secrets).
type Channel struct {
	ID        int64
	Name      string
	Kind      string
	Config    string
	Enabled   bool
	CreatedAt time.Time
}

// Event is something that happened to a stack (deploy, edit, rollback…).
type Event struct {
	ID     int64
	TS     time.Time
	HostID string
	Stack  string
	Kind   string
	Actor  string
	Detail string
}

// ---- checks ----------------------------------------------------------------

const checkCols = `id, name, kind, target, host_id, stack, interval_s, timeout_s, retries, settings, enabled, created_at, locations, quorum`

func scanCheck(r scanner) (Check, error) {
	var c Check
	var enabled int
	var created int64
	var locs string
	if err := r.Scan(&c.ID, &c.Name, &c.Kind, &c.Target, &c.HostID, &c.Stack, &c.Interval, &c.Timeout, &c.Retries, &c.Settings, &enabled, &created, &locs, &c.Quorum); err != nil {
		return Check{}, err
	}
	c.Enabled = enabled == 1
	c.CreatedAt = time.Unix(created, 0)
	if locs != "" {
		json.Unmarshal([]byte(locs), &c.Locations) //nolint:errcheck // written by encodeLocations
	}
	return c, nil
}

// encodeLocations stores an empty list as "" so a hub-only check keeps the old row shape.
func encodeLocations(locs []string) string {
	if len(locs) == 0 {
		return ""
	}
	b, _ := json.Marshal(locs)
	return string(b)
}

// Checks lists all checks with their channels.
func (s *Store) Checks(ctx context.Context) ([]Check, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+checkCols+` FROM checks ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		return nil, err
	}
	var out []Check
	for rows.Next() {
		c, err := scanCheck(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	links, err := s.checkChannels(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Channels = links[out[i].ID]
	}
	return out, nil
}

func (s *Store) checkChannels(ctx context.Context) (map[int64][]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT check_id, channel_id FROM check_channels ORDER BY channel_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64][]int64{}
	for rows.Next() {
		var c, ch int64
		if err := rows.Scan(&c, &ch); err != nil {
			return nil, err
		}
		m[c] = append(m[c], ch)
	}
	return m, rows.Err()
}

// CheckByID returns one check.
func (s *Store) CheckByID(ctx context.Context, id int64) (Check, error) {
	c, err := scanCheck(s.db.QueryRowContext(ctx, `SELECT `+checkCols+` FROM checks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Check{}, ErrNotFound
	}
	if err != nil {
		return Check{}, err
	}
	links, err := s.checkChannels(ctx)
	if err != nil {
		return Check{}, err
	}
	c.Channels = links[c.ID]
	return c, nil
}

// SaveCheck inserts (ID 0) or updates a check and replaces its channel links.
func (s *Store) SaveCheck(ctx context.Context, c Check) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	enabled := 0
	if c.Enabled {
		enabled = 1
	}
	if c.ID == 0 {
		res, err := tx.ExecContext(ctx, `INSERT INTO checks (name, kind, target, host_id, stack, interval_s, timeout_s, retries, settings, enabled, created_at, locations, quorum)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.Name, c.Kind, c.Target, c.HostID, c.Stack, c.Interval, c.Timeout, c.Retries, c.Settings, enabled, time.Now().Unix(), encodeLocations(c.Locations), c.Quorum)
		if err != nil {
			return 0, err
		}
		c.ID, _ = res.LastInsertId()
	} else {
		res, err := tx.ExecContext(ctx, `UPDATE checks SET name=?, kind=?, target=?, host_id=?, stack=?, interval_s=?, timeout_s=?, retries=?, settings=?, enabled=?, locations=?, quorum=? WHERE id=?`,
			c.Name, c.Kind, c.Target, c.HostID, c.Stack, c.Interval, c.Timeout, c.Retries, c.Settings, enabled, encodeLocations(c.Locations), c.Quorum, c.ID)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return 0, ErrNotFound
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM check_channels WHERE check_id = ?`, c.ID); err != nil {
		return 0, err
	}
	for _, ch := range c.Channels {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO check_channels (check_id, channel_id)
			SELECT ?, id FROM channels WHERE id = ?`, c.ID, ch); err != nil {
			return 0, err
		}
	}
	return c.ID, tx.Commit()
}

// DeleteCheck removes a check with its results and incidents.
func (s *Store) DeleteCheck(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM checks WHERE id = ?`, id)
	return err
}

// ---- results ---------------------------------------------------------------

// AddResult stores one probe outcome.
func (s *Store) AddResult(ctx context.Context, r Result) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO check_results (check_id, ts, status, latency_ms, msg) VALUES (?, ?, ?, ?, ?)`,
		r.CheckID, r.TS.UnixMilli(), r.Status, r.LatencyMS, r.Msg)
	return err
}

// RecentResults returns the newest n results of a check, oldest first.
func (s *Store) RecentResults(ctx context.Context, checkID int64, n int) ([]Result, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT check_id, ts, status, latency_ms, msg FROM
		(SELECT * FROM check_results WHERE check_id = ? ORDER BY ts DESC LIMIT ?) ORDER BY ts`, checkID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Result
	for rows.Next() {
		var r Result
		var ts int64
		if err := rows.Scan(&r.CheckID, &ts, &r.Status, &r.LatencyMS, &r.Msg); err != nil {
			return nil, err
		}
		r.TS = time.UnixMilli(ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Uptime returns the share of up results since t (silenced and paused results don't count)
// and how many results were counted.
func (s *Store) Uptime(ctx context.Context, checkID int64, since time.Time) (float64, int, error) {
	var up, total int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(status = 'up'), 0), COUNT(*) FROM check_results
		WHERE check_id = ? AND ts >= ? AND status IN ('up', 'down')`, checkID, since.UnixMilli()).Scan(&up, &total)
	if err != nil || total == 0 {
		return 0, 0, err
	}
	return float64(up) / float64(total), total, nil
}

// PruneResults deletes raw results older than t.
func (s *Store) PruneResults(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM check_results WHERE ts < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- incidents -------------------------------------------------------------

// OpenIncident starts an incident and returns its id.
func (s *Store) OpenIncident(ctx context.Context, checkID int64, at time.Time, cause, context string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO incidents (check_id, started_at, cause, context) VALUES (?, ?, ?, ?)`,
		checkID, at.Unix(), cause, context)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// CloseIncident ends an incident.
func (s *Store) CloseIncident(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE incidents SET ended_at = ? WHERE id = ? AND ended_at IS NULL`, at.Unix(), id)
	return err
}

// OpenIncidents returns the open incident per check id.
func (s *Store) OpenIncidents(ctx context.Context) (map[int64]Incident, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, check_id, started_at, cause, context, caused_by FROM incidents WHERE ended_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]Incident{}
	for rows.Next() {
		var in Incident
		var started int64
		if err := rows.Scan(&in.ID, &in.CheckID, &started, &in.Cause, &in.Context, &in.CausedBy); err != nil {
			return nil, err
		}
		in.StartedAt = time.Unix(started, 0)
		out[in.CheckID] = in
	}
	return out, rows.Err()
}

// Incidents lists the newest incidents of a check.
func (s *Store) Incidents(ctx context.Context, checkID int64, n int) ([]Incident, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, check_id, started_at, COALESCE(ended_at, 0), cause, context, caused_by
		FROM incidents WHERE check_id = ? ORDER BY started_at DESC, id DESC LIMIT ?`, checkID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		var in Incident
		var started, ended int64
		if err := rows.Scan(&in.ID, &in.CheckID, &started, &ended, &in.Cause, &in.Context, &in.CausedBy); err != nil {
			return nil, err
		}
		in.StartedAt = time.Unix(started, 0)
		if ended > 0 {
			in.EndedAt = time.Unix(ended, 0)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// ---- channels --------------------------------------------------------------

func scanChannel(r scanner) (Channel, error) {
	var c Channel
	var enabled int
	var created int64
	if err := r.Scan(&c.ID, &c.Name, &c.Kind, &c.Config, &enabled, &created); err != nil {
		return Channel{}, err
	}
	c.Enabled = enabled == 1
	c.CreatedAt = time.Unix(created, 0)
	return c, nil
}

// Channels lists notification channels.
func (s *Store) Channels(ctx context.Context) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, kind, config, enabled, created_at FROM channels ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChannelByID returns one channel.
func (s *Store) ChannelByID(ctx context.Context, id int64) (Channel, error) {
	c, err := scanChannel(s.db.QueryRowContext(ctx, `SELECT id, name, kind, config, enabled, created_at FROM channels WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, ErrNotFound
	}
	return c, err
}

// SaveChannel inserts (ID 0) or updates a channel.
func (s *Store) SaveChannel(ctx context.Context, c Channel) (int64, error) {
	enabled := 0
	if c.Enabled {
		enabled = 1
	}
	if c.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO channels (name, kind, config, enabled, created_at) VALUES (?, ?, ?, ?, ?)`,
			c.Name, c.Kind, c.Config, enabled, time.Now().Unix())
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE channels SET name=?, kind=?, config=?, enabled=? WHERE id=?`, c.Name, c.Kind, c.Config, enabled, c.ID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	return c.ID, nil
}

// DeleteChannel removes a channel and its links to checks and update settings.
func (s *Store) DeleteChannel(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, id); err != nil {
		return err
	}
	if st, err := s.UpdateSettings(ctx); err == nil && len(st.Channels) > 0 {
		var kept []int64
		for _, cid := range st.Channels {
			if cid != id {
				kept = append(kept, cid)
			}
		}
		if len(kept) != len(st.Channels) {
			st.Channels = kept
			_ = s.SaveUpdateSettings(ctx, st)
		}
	}
	return nil
}

// ---- events ----------------------------------------------------------------

// AddEvent records something that happened to a stack.
func (s *Store) AddEvent(ctx context.Context, e Event) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO events (ts, host_id, stack, kind, actor, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		e.TS.UnixMilli(), e.HostID, e.Stack, e.Kind, e.Actor, e.Detail)
	return err
}

// Events lists a stack's events since t, oldest first. kinds filters when not empty.
func (s *Store) Events(ctx context.Context, hostID, stack string, since time.Time, kinds ...string) ([]Event, error) {
	q := `SELECT id, ts, host_id, stack, kind, actor, detail FROM events WHERE host_id = ? AND stack = ? AND ts >= ?`
	args := []any{hostID, stack, since.UnixMilli()}
	if len(kinds) > 0 {
		q += ` AND kind IN (?` + strings.Repeat(",?", len(kinds)-1) + `)`
		for _, k := range kinds {
			args = append(args, k)
		}
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY ts LIMIT 500`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var ts int64
		if err := rows.Scan(&e.ID, &ts, &e.HostID, &e.Stack, &e.Kind, &e.Actor, &e.Detail); err != nil {
			return nil, err
		}
		e.TS = time.UnixMilli(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}
