package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// APIToken is a long-lived bearer credential for CLI, TUI, and remote API clients.
type APIToken struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Prefix     string    `json:"prefix"`
	TokenHash  string    `json:"-"`
	Role       string    `json:"role"`
	Scopes     []string  `json:"scopes"`
	CreatedBy  string    `json:"createdBy"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt,omitempty"`
	LastUsedAt time.Time `json:"lastUsedAt,omitempty"`
}

func (s *Store) CreateAPIToken(ctx context.Context, t APIToken) (APIToken, error) {
	if len(t.Scopes) == 0 {
		t.Scopes = []string{t.Role}
	}
	sc, err := json.Marshal(t.Scopes)
	if err != nil {
		return t, err
	}
	now := t.CreatedAt
	if now.IsZero() {
		now = time.Now()
		t.CreatedAt = now
	}
	var expUnix int64
	if !t.ExpiresAt.IsZero() {
		expUnix = t.ExpiresAt.Unix()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO api_tokens (name, prefix, token_hash, role, scopes_json, created_by, created_at, expires_at) VALUES (?,?,?,?,?,?,?,?)`,
		t.Name, t.Prefix, t.TokenHash, t.Role, string(sc), t.CreatedBy, now.Unix(), expUnix)
	if err != nil {
		return t, fmt.Errorf("create api token: %w", err)
	}
	t.ID, _ = res.LastInsertId()
	return t, nil
}

func (s *Store) ListAPITokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, prefix, role, scopes_json, created_by, created_at, expires_at, last_used_at
		 FROM api_tokens ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		var t APIToken
		var scJSON string
		var created, exp, lastUsed int64
		if err := rows.Scan(&t.ID, &t.Name, &t.Prefix, &t.Role, &scJSON, &t.CreatedBy, &created, &exp, &lastUsed); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(scJSON), &t.Scopes)
		t.CreatedAt = time.Unix(created, 0).UTC()
		if exp > 0 {
			t.ExpiresAt = time.Unix(exp, 0).UTC()
		}
		if lastUsed > 0 {
			t.LastUsedAt = time.Unix(lastUsed, 0).UTC()
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) LookupAPITokenByHash(ctx context.Context, hash string) (APIToken, error) {
	var t APIToken
	var scJSON string
	var created, exp, lastUsed int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, prefix, token_hash, role, scopes_json, created_by, created_at, expires_at, last_used_at
		 FROM api_tokens WHERE token_hash = ?`, hash).
		Scan(&t.ID, &t.Name, &t.Prefix, &t.TokenHash, &t.Role, &scJSON, &t.CreatedBy, &created, &exp, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, err
	}
	_ = json.Unmarshal([]byte(scJSON), &t.Scopes)
	t.CreatedAt = time.Unix(created, 0).UTC()
	if exp > 0 {
		t.ExpiresAt = time.Unix(exp, 0).UTC()
	}
	if lastUsed > 0 {
		t.LastUsedAt = time.Unix(lastUsed, 0).UTC()
	}
	return t, nil
}

func (s *Store) TouchAPIToken(ctx context.Context, id int64, at time.Time) {
	_, _ = s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, at.Unix(), id)
}

func (s *Store) DeleteAPIToken(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
