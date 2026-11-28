package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ---- scheduled_tasks (stack-scheduler) -------------------------------------------------

type ScheduledTask struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	HostID     string    `json:"hostId"`
	Stack      string    `json:"stack"`
	Cron       string    `json:"cron"`
	Action     string    `json:"action"` // restart | start | stop | pull-up | exec
	Service    string    `json:"service,omitempty"`
	Command    string    `json:"command,omitempty"`
	Enabled    bool      `json:"enabled"`
	Channels   []int64   `json:"channels"`
	LastRunAt  time.Time `json:"lastRunAt,omitzero"`
	LastStatus string    `json:"lastStatus,omitempty"`
	LastOutput string    `json:"lastOutput,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

func scanScheduledTask(scan func(dest ...any) error) (ScheduledTask, error) {
	var t ScheduledTask
	var en int
	var chRaw string
	var lastRun, created int64
	if err := scan(
		&t.ID, &t.Name, &t.HostID, &t.Stack, &t.Cron, &t.Action, &t.Service, &t.Command,
		&en, &chRaw, &lastRun, &t.LastStatus, &t.LastOutput, &created,
	); err != nil {
		return ScheduledTask{}, err
	}
	t.Enabled = en != 0
	if chRaw != "" {
		_ = json.Unmarshal([]byte(chRaw), &t.Channels)
	}
	if t.Channels == nil {
		t.Channels = []int64{}
	}
	if lastRun > 0 {
		t.LastRunAt = time.Unix(lastRun, 0)
	}
	t.CreatedAt = time.Unix(created, 0)
	return t, nil
}

func (s *Store) ScheduledTasks(ctx context.Context) ([]ScheduledTask, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, host_id, stack, cron, action, service, command, enabled, channels, last_run_at, last_status, last_output, created_at
		FROM scheduled_tasks ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduledTask
	for rows.Next() {
		t, err := scanScheduledTask(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) ScheduledTaskByID(ctx context.Context, id int64) (ScheduledTask, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, host_id, stack, cron, action, service, command, enabled, channels, last_run_at, last_status, last_output, created_at
		FROM scheduled_tasks WHERE id = ?`, id)
	t, err := scanScheduledTask(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ScheduledTask{}, ErrNotFound
	}
	return t, err
}

func (s *Store) SaveScheduledTask(ctx context.Context, t ScheduledTask) (ScheduledTask, error) {
	en := 0
	if t.Enabled {
		en = 1
	}
	chJSON, _ := json.Marshal(t.Channels)
	if t.ID > 0 {
		res, err := s.db.ExecContext(ctx, `
			UPDATE scheduled_tasks
			SET name = ?, host_id = ?, stack = ?, cron = ?, action = ?, service = ?, command = ?, enabled = ?, channels = ?
			WHERE id = ?`,
			t.Name, t.HostID, t.Stack, t.Cron, t.Action, t.Service, t.Command, en, string(chJSON), t.ID,
		)
		if err != nil {
			return ScheduledTask{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ScheduledTask{}, ErrNotFound
		}
		return s.ScheduledTaskByID(ctx, t.ID)
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO scheduled_tasks (name, host_id, stack, cron, action, service, command, enabled, channels, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Name, t.HostID, t.Stack, t.Cron, t.Action, t.Service, t.Command, en, string(chJSON), time.Now().Unix(),
	)
	if err != nil {
		return ScheduledTask{}, err
	}
	id, _ := res.LastInsertId()
	return s.ScheduledTaskByID(ctx, id)
}

func (s *Store) RecordScheduledTaskRun(ctx context.Context, id int64, status, output string) error {
	if len(output) > 4096 {
		output = output[:4096]
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE scheduled_tasks SET last_run_at = ?, last_status = ?, last_output = ? WHERE id = ?`,
		time.Now().Unix(), status, output, id,
	)
	return err
}

func (s *Store) DeleteScheduledTask(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM scheduled_tasks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- registry_webhooks -----------------------------------------------------------------

type RegistryWebhook struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Provider   string    `json:"provider"` // ghcr | dockerhub | gitea | generic
	TokenHash  string    `json:"-"`
	Secret     string    `json:"-"`
	SecretSet  bool      `json:"secretSet"`
	ApplyNow   bool      `json:"applyNow"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"createdAt"`
	LastHitAt  time.Time `json:"lastHitAt,omitzero"`
	LastResult string    `json:"lastResult,omitempty"`
}

func scanRegistryWebhook(scan func(dest ...any) error) (RegistryWebhook, error) {
	var w RegistryWebhook
	var applyNow, enabled int
	var created, lastHit int64
	if err := scan(&w.ID, &w.Name, &w.Provider, &w.TokenHash, &w.Secret, &applyNow, &enabled, &created, &lastHit, &w.LastResult); err != nil {
		return RegistryWebhook{}, err
	}
	w.SecretSet = w.Secret != ""
	w.ApplyNow = applyNow != 0
	w.Enabled = enabled != 0
	w.CreatedAt = time.Unix(created, 0)
	if lastHit > 0 {
		w.LastHitAt = time.Unix(lastHit, 0)
	}
	return w, nil
}

func (s *Store) RegistryWebhooks(ctx context.Context) ([]RegistryWebhook, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, provider, token_hash, secret, apply_now, enabled, created_at, last_hit_at, last_result
		FROM registry_webhooks ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RegistryWebhook
	for rows.Next() {
		w, err := scanRegistryWebhook(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) RegistryWebhookByHash(ctx context.Context, tokenHash string) (RegistryWebhook, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, provider, token_hash, secret, apply_now, enabled, created_at, last_hit_at, last_result
		FROM registry_webhooks WHERE token_hash = ?`, tokenHash)
	w, err := scanRegistryWebhook(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return RegistryWebhook{}, ErrNotFound
	}
	return w, err
}

func (s *Store) CreateRegistryWebhook(ctx context.Context, name, provider, tokenHash, secret string, applyNow bool) (RegistryWebhook, error) {
	an := 0
	if applyNow {
		an = 1
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO registry_webhooks (name, provider, token_hash, secret, apply_now, enabled, created_at)
		VALUES (?, ?, ?, ?, ?, 1, ?)`,
		name, provider, tokenHash, secret, an, time.Now().Unix(),
	)
	if err != nil {
		return RegistryWebhook{}, err
	}
	id, _ := res.LastInsertId()
	return s.RegistryWebhookByID(ctx, id)
}

func (s *Store) RegistryWebhookByID(ctx context.Context, id int64) (RegistryWebhook, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, provider, token_hash, secret, apply_now, enabled, created_at, last_hit_at, last_result
		FROM registry_webhooks WHERE id = ?`, id)
	w, err := scanRegistryWebhook(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return RegistryWebhook{}, ErrNotFound
	}
	return w, err
}

func (s *Store) UpdateRegistryWebhook(ctx context.Context, id int64, name, provider, secret string, keepSecret, applyNow, enabled bool) (RegistryWebhook, error) {
	cur, err := s.RegistryWebhookByID(ctx, id)
	if err != nil {
		return RegistryWebhook{}, err
	}
	if keepSecret {
		secret = cur.Secret
	}
	an, en := 0, 0
	if applyNow {
		an = 1
	}
	if enabled {
		en = 1
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE registry_webhooks SET name = ?, provider = ?, secret = ?, apply_now = ?, enabled = ? WHERE id = ?`,
		name, provider, secret, an, en, id,
	)
	if err != nil {
		return RegistryWebhook{}, err
	}
	return s.RegistryWebhookByID(ctx, id)
}

func (s *Store) RecordWebhookHit(ctx context.Context, id int64, result string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE registry_webhooks SET last_hit_at = ?, last_result = ? WHERE id = ?`, time.Now().Unix(), result, id)
	return err
}

func (s *Store) DeleteRegistryWebhook(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM registry_webhooks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- mcp_tokens & mcp_drafts -----------------------------------------------------------

type MCPACLRule struct {
	HostID       string `json:"hostId"`       // "*" or host ID
	StackPattern string `json:"stackPattern"` // "*" or glob/prefix
	Scope        string `json:"scope"`        // read | operate | deploy
}

type MCPToken struct {
	ID              int64        `json:"id"`
	Name            string       `json:"name"`
	TokenHash       string       `json:"-"`
	ACL             []MCPACLRule `json:"acl"`
	RequireApproval bool         `json:"requireApproval"`
	ExpiresAt       time.Time    `json:"expiresAt,omitzero"`
	CreatedAt       time.Time    `json:"createdAt"`
	CreatedBy       string       `json:"createdBy"`
	LastUsedAt      time.Time    `json:"lastUsedAt,omitzero"`
}

type MCPDraft struct {
	ID        int64     `json:"id"`
	TokenName string    `json:"tokenName"`
	Tool      string    `json:"tool"`
	HostID    string    `json:"hostId"`
	Stack     string    `json:"stack"`
	Compose   string    `json:"compose"`
	Env       string    `json:"env"`
	EnvKeys   []string  `json:"envKeys"`
	Note      string    `json:"note"`
	Deploy    bool      `json:"deploy"`
	CreateNew bool      `json:"createNew"`
	Status    string    `json:"status"` // pending | approved | rejected
	CreatedAt time.Time `json:"createdAt"`
	DecidedBy string    `json:"decidedBy,omitempty"`
	DecidedAt time.Time `json:"decidedAt,omitzero"`
}

func scanMCPToken(scan func(dest ...any) error) (MCPToken, error) {
	var t MCPToken
	var aclRaw string
	var reqAppr int
	var exp, created, lastUsed int64
	if err := scan(&t.ID, &t.Name, &t.TokenHash, &aclRaw, &reqAppr, &exp, &created, &t.CreatedBy, &lastUsed); err != nil {
		return MCPToken{}, err
	}
	_ = json.Unmarshal([]byte(aclRaw), &t.ACL)
	if t.ACL == nil {
		t.ACL = []MCPACLRule{}
	}
	t.RequireApproval = reqAppr != 0
	if exp > 0 {
		t.ExpiresAt = time.Unix(exp, 0)
	}
	t.CreatedAt = time.Unix(created, 0)
	if lastUsed > 0 {
		t.LastUsedAt = time.Unix(lastUsed, 0)
	}
	return t, nil
}

func (s *Store) MCPTokens(ctx context.Context) ([]MCPToken, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, token_hash, acl, require_approval, expires_at, created_at, created_by, last_used_at
		FROM mcp_tokens ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPToken
	for rows.Next() {
		t, err := scanMCPToken(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) ActiveMCPTokenCount(ctx context.Context) (int, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_tokens WHERE expires_at = 0 OR expires_at > ?`, time.Now().Unix()).Scan(&n)
	return n, err
}

func (s *Store) MCPTokenByHash(ctx context.Context, hash string) (MCPToken, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, token_hash, acl, require_approval, expires_at, created_at, created_by, last_used_at
		FROM mcp_tokens WHERE token_hash = ?`, hash)
	t, err := scanMCPToken(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return MCPToken{}, ErrNotFound
	}
	return t, err
}

func (s *Store) CreateMCPToken(ctx context.Context, name, hash string, acl []MCPACLRule, requireApproval bool, expiresAt time.Time, createdBy string) (MCPToken, error) {
	aclJSON, _ := json.Marshal(acl)
	ra := 0
	if requireApproval {
		ra = 1
	}
	exp := int64(0)
	if !expiresAt.IsZero() {
		exp = expiresAt.Unix()
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO mcp_tokens (name, token_hash, acl, require_approval, expires_at, created_at, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		name, hash, string(aclJSON), ra, exp, time.Now().Unix(), createdBy,
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return MCPToken{}, errors.New("an MCP token with this name already exists")
		}
		return MCPToken{}, err
	}
	id, _ := res.LastInsertId()
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, token_hash, acl, require_approval, expires_at, created_at, created_by, last_used_at
		FROM mcp_tokens WHERE id = ?`, id)
	return scanMCPToken(row.Scan)
}

func (s *Store) TouchMCPToken(ctx context.Context, id int64) {
	_, _ = s.db.ExecContext(ctx, `UPDATE mcp_tokens SET last_used_at = ? WHERE id = ?`, time.Now().Unix(), id)
}

func (s *Store) DeleteMCPToken(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM mcp_tokens WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanMCPDraft(scan func(dest ...any) error) (MCPDraft, error) {
	var d MCPDraft
	var envKeysRaw string
	var deploy, createNew int
	var created, decided int64
	if err := scan(
		&d.ID, &d.TokenName, &d.Tool, &d.HostID, &d.Stack, &d.Compose, &d.Env,
		&envKeysRaw, &d.Note, &deploy, &createNew, &d.Status, &created, &d.DecidedBy, &decided,
	); err != nil {
		return MCPDraft{}, err
	}
	_ = json.Unmarshal([]byte(envKeysRaw), &d.EnvKeys)
	if d.EnvKeys == nil {
		d.EnvKeys = []string{}
	}
	d.Deploy = deploy != 0
	d.CreateNew = createNew != 0
	d.CreatedAt = time.Unix(created, 0)
	if decided > 0 {
		d.DecidedAt = time.Unix(decided, 0)
	}
	return d, nil
}

func (s *Store) CreateMCPDraft(ctx context.Context, d MCPDraft) (MCPDraft, error) {
	ek, _ := json.Marshal(d.EnvKeys)
	dep, cr := 0, 0
	if d.Deploy {
		dep = 1
	}
	if d.CreateNew {
		cr = 1
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO mcp_drafts (token_name, tool, host_id, stack, compose, env, env_keys, note, deploy, create_new, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
		d.TokenName, d.Tool, d.HostID, d.Stack, d.Compose, d.Env, string(ek), d.Note, dep, cr, time.Now().Unix(),
	)
	if err != nil {
		return MCPDraft{}, err
	}
	id, _ := res.LastInsertId()
	return s.MCPDraftByID(ctx, id)
}

func (s *Store) MCPDraftByID(ctx context.Context, id int64) (MCPDraft, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, token_name, tool, host_id, stack, compose, env, env_keys, note, deploy, create_new, status, created_at, decided_by, decided_at
		FROM mcp_drafts WHERE id = ?`, id)
	d, err := scanMCPDraft(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return MCPDraft{}, ErrNotFound
	}
	return d, err
}

func (s *Store) MCPDrafts(ctx context.Context, status string) ([]MCPDraft, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	q := `SELECT id, token_name, tool, host_id, stack, compose, env, env_keys, note, deploy, create_new, status, created_at, decided_by, decided_at FROM mcp_drafts`
	var args []any
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY id DESC LIMIT 100`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPDraft
	for rows.Next() {
		d, err := scanMCPDraft(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RejectPendingMCPDrafts rejects pending drafts whose token name matches the GLOB
// pattern and that were created before the cutoff (M20G: forgotten assistant proposals).
func (s *Store) RejectPendingMCPDrafts(ctx context.Context, nameGlob string, before time.Time, by string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE mcp_drafts SET status = 'rejected', decided_by = ?, decided_at = ?
		WHERE status = 'pending' AND token_name GLOB ? AND created_at < ?`,
		by, time.Now().Unix(), nameGlob, before.Unix())
	return err
}

// PruneMCPDrafts deletes decided drafts older than the cutoff; each one carries a whole
// compose file and .env.
func (s *Store) PruneMCPDrafts(ctx context.Context, cutoff time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM mcp_drafts WHERE status != 'pending' AND decided_at < ?`, cutoff.Unix())
	return err
}

func (s *Store) DecideMCPDraft(ctx context.Context, id int64, status, decidedBy string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE mcp_drafts SET status = ?, decided_by = ?, decided_at = ? WHERE id = ? AND status = 'pending'`,
		status, decidedBy, time.Now().Unix(), id,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
