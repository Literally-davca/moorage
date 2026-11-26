package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Agent is a Docker host paired with this hub.
type Agent struct {
	ID           string
	Name         string
	Hostname     string
	CreatedAt    time.Time
	LastSeen     time.Time // zero = never connected
	Watchdog     bool
	WatchdogAddr string
	WatchdogCert []byte
	// Retried is set by RedeemJoinToken when a join token was used again by an agent
	// that never connected (its first answer was lost).
	Retried bool
}

// WatchdogSettings holds the hub's coordinated watchdog configuration.
type WatchdogSettings struct {
	Enabled          bool
	ChannelID        int64
	Grace            int       // seconds
	MaintenanceUntil time.Time // zero = not in maintenance
}

// CreateJoinToken stores the hash of a one-time pairing token.
func (s *Store) CreateJoinToken(ctx context.Context, tokenHash, name string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO join_tokens (token_hash, name, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		tokenHash, name, time.Now().Unix(), expires.Unix())
	return err
}

// JoinRetryWindow is how long after its first use a join token can be used again by an
// agent that never connected: the hub's answer with the certificate may have been lost
// (network, proxy timeout), and without a retry the token was burnt and the host stuck.
const JoinRetryWindow = 15 * time.Minute

// RedeemJoinToken creates the agent for a join token. A token is used once: the first
// redeem creates the agent and remembers it; a second redeem within JoinRetryWindow, while
// that agent has never connected, returns the same agent again (so the hub signs a new
// certificate for it). The token is deleted when the agent first connects
// (ConfirmAgentPaired). It returns ErrNotFound for an unknown, used or expired token.
func (s *Store) RedeemJoinToken(ctx context.Context, tokenHash, agentID, hostname string) (Agent, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Agent{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	now := time.Now()
	var (
		name, prevID string
		redeemed     int64
	)
	err = tx.QueryRowContext(ctx, `SELECT name, agent_id, redeemed_at FROM join_tokens WHERE token_hash = ? AND expires_at > ?`,
		tokenHash, now.Unix()).Scan(&name, &prevID, &redeemed)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	if err != nil {
		return Agent{}, err
	}
	if prevID != "" {
		// A retry: only for an agent that never connected, and only for a short while.
		a, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id = ?`, prevID))
		if err != nil || !a.LastSeen.IsZero() || now.Sub(time.Unix(redeemed, 0)) > JoinRetryWindow {
			return Agent{}, ErrNotFound
		}
		if hostname != "" && hostname != a.Hostname {
			if _, err := tx.ExecContext(ctx, `UPDATE agents SET hostname = ? WHERE id = ?`, hostname, a.ID); err != nil {
				return Agent{}, err
			}
			a.Hostname = hostname
		}
		a.Retried = true
		return a, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agents (id, name, hostname, created_at) VALUES (?, ?, ?, ?)`,
		agentID, name, hostname, now.Unix()); err != nil {
		return Agent{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE join_tokens SET agent_id = ?, redeemed_at = ? WHERE token_hash = ?`,
		agentID, now.Unix(), tokenHash); err != nil {
		return Agent{}, err
	}
	return Agent{ID: agentID, Name: name, Hostname: hostname, CreatedAt: now}, tx.Commit()
}

// ReleaseJoinToken undoes a first redeem whose certificate could not be signed: the
// agent row goes and the token is unused again.
func (s *Store) ReleaseJoinToken(ctx context.Context, tokenHash, agentID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `UPDATE join_tokens SET agent_id = '', redeemed_at = 0 WHERE token_hash = ? AND agent_id = ?`, tokenHash, agentID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agents WHERE id = ? AND last_seen = 0`, agentID); err != nil {
		return err
	}
	return tx.Commit()
}

// ConfirmAgentPaired deletes the join token of an agent once it connected with its
// certificate: from now on the token can't be used again.
func (s *Store) ConfirmAgentPaired(ctx context.Context, agentID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM join_tokens WHERE agent_id = ?`, agentID)
	return err
}

// JoinToken is a pairing token that is still open: not used yet, or used by an agent
// that hasn't connected yet.
type JoinToken struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	AgentID    string    `json:"agentId,omitempty"`
	RedeemedAt time.Time `json:"redeemedAt,omitzero"`
}

// JoinTokens lists the open join tokens, newest first. Expired ones are deleted here.
func (s *Store) JoinTokens(ctx context.Context) ([]JoinToken, error) {
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM join_tokens WHERE expires_at <= ?`, now); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT rowid, name, created_at, expires_at, agent_id, redeemed_at FROM join_tokens ORDER BY created_at DESC, rowid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []JoinToken{}
	for rows.Next() {
		var t JoinToken
		var created, expires, redeemed int64
		if err := rows.Scan(&t.ID, &t.Name, &created, &expires, &t.AgentID, &redeemed); err != nil {
			return nil, err
		}
		t.CreatedAt, t.ExpiresAt = time.Unix(created, 0), time.Unix(expires, 0)
		if redeemed > 0 {
			t.RedeemedAt = time.Unix(redeemed, 0)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteJoinToken cancels an open join token. An agent it created that never connected
// goes with it (it could only ever be a dead entry). It returns the token's name.
func (s *Store) DeleteJoinToken(ctx context.Context, id int64) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback() //nolint:errcheck
	var name, agentID string
	err = tx.QueryRowContext(ctx, `DELETE FROM join_tokens WHERE rowid = ? RETURNING name, agent_id`, id).Scan(&name, &agentID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if agentID != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM agents WHERE id = ? AND last_seen = 0`, agentID); err != nil {
			return "", err
		}
	}
	return name, tx.Commit()
}

const agentCols = `id, name, hostname, created_at, last_seen, watchdog, watchdog_addr, watchdog_cert`

// Agents lists paired agents by name.
func (s *Store) Agents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+agentCols+` FROM agents ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AgentByID returns one agent.
func (s *Store) AgentByID(ctx context.Context, id string) (Agent, error) {
	a, err := scanAgent(s.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	return a, err
}

// TouchAgent records that the agent was connected at t.
func (s *Store) TouchAgent(ctx context.Context, id, hostname string, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET last_seen = ?, hostname = CASE WHEN ? = '' THEN hostname ELSE ? END WHERE id = ?`,
		t.Unix(), hostname, hostname, id)
	return err
}

// SetAgentWatchdogAddr stores the peer address reported by an agent on connect.
func (s *Store) SetAgentWatchdogAddr(ctx context.Context, id, addr string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET watchdog_addr = ? WHERE id = ?`, addr, id)
	return err
}

// SetAgentWatchdogCert caches the DER-encoded peer certificate issued for an agent.
func (s *Store) SetAgentWatchdogCert(ctx context.Context, id string, cert []byte) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET watchdog_cert = ? WHERE id = ?`, cert, id)
	return err
}

// Watchdog loads the watchdog settings and participating agent IDs.
func (s *Store) Watchdog(ctx context.Context) (WatchdogSettings, error) {
	var w WatchdogSettings
	var enabled int
	var ch sql.NullInt64
	var maint int64
	err := s.db.QueryRowContext(ctx, `SELECT enabled, channel_id, grace_s, maintenance_until FROM watchdog WHERE id = 1`).
		Scan(&enabled, &ch, &w.Grace, &maint)
	if err != nil {
		return WatchdogSettings{Grace: 120}, err
	}
	w.Enabled = enabled == 1
	if ch.Valid {
		w.ChannelID = ch.Int64
	}
	if maint > 0 {
		w.MaintenanceUntil = time.Unix(maint, 0)
	}
	return w, nil
}

// SaveWatchdog updates the watchdog settings and each agent's participation flag.
func (s *Store) SaveWatchdog(ctx context.Context, w WatchdogSettings, agents map[string]bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	enabled := 0
	if w.Enabled {
		enabled = 1
	}
	var ch any
	if w.ChannelID > 0 {
		ch = w.ChannelID
	}
	var maint int64
	if !w.MaintenanceUntil.IsZero() {
		maint = w.MaintenanceUntil.Unix()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE watchdog SET enabled = ?, channel_id = ?, grace_s = ?, maintenance_until = ? WHERE id = 1`,
		enabled, ch, w.Grace, maint); err != nil {
		return err
	}
	if agents != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE agents SET watchdog = 0`); err != nil {
			return err
		}
		for id, on := range agents {
			if on {
				if _, err := tx.ExecContext(ctx, `UPDATE agents SET watchdog = 1 WHERE id = ?`, id); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}

// SetWatchdogMaintenance sets or clears the maintenance window.
func (s *Store) SetWatchdogMaintenance(ctx context.Context, until time.Time) error {
	var maint int64
	if !until.IsZero() {
		maint = until.Unix()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE watchdog SET maintenance_until = ? WHERE id = 1`, maint)
	return err
}

// RenameAgent changes the display name.
func (s *Store) RenameAgent(ctx context.Context, id, name string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE agents SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteAgent removes the agent; its certificate stops working on the next connect.
func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM join_tokens WHERE agent_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE id = ?`, id)
	return err
}

type scanner interface{ Scan(dest ...any) error }

func scanAgent(r scanner) (Agent, error) {
	var a Agent
	var created, seen int64
	var wd int
	if err := r.Scan(&a.ID, &a.Name, &a.Hostname, &created, &seen, &wd, &a.WatchdogAddr, &a.WatchdogCert); err != nil {
		return Agent{}, err
	}
	a.CreatedAt = time.Unix(created, 0)
	if seen > 0 {
		a.LastSeen = time.Unix(seen, 0)
	}
	a.Watchdog = wd == 1
	return a, nil
}
