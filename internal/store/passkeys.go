package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrPasskeyTaken is a credential id that is already registered (to anyone).
var ErrPasskeyTaken = errors.New("passkey already registered")

// Passkey is one WebAuthn credential of a local user.
type Passkey struct {
	ID           int64
	UserID       int64
	CredentialID string // base64url
	UserHandle   string // base64url
	RPID         string // the relying party id it was registered for
	Name         string
	DataJSON     string
	Passwordless bool
	CreatedAt    time.Time
	LastUsedAt   time.Time
}

const passkeyCols = `SELECT id, user_id, credential_id, user_handle, rp_id, name, data_json, passwordless,
	created_at, last_used_at FROM webauthn_credentials`

func scanPasskey(row rowScanner) (Passkey, error) {
	var p Passkey
	var pwless int
	var created, used int64
	err := row.Scan(&p.ID, &p.UserID, &p.CredentialID, &p.UserHandle, &p.RPID, &p.Name, &p.DataJSON, &pwless, &created, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	p.Passwordless = pwless == 1
	p.CreatedAt, p.LastUsedAt = unixOrZero(created), unixOrZero(used)
	return p, err
}

// AddPasskey stores a new credential and returns its row id.
func (s *Store) AddPasskey(ctx context.Context, p Passkey) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO webauthn_credentials (user_id, credential_id, user_handle, rp_id, name,
		data_json, passwordless, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.UserID, p.CredentialID, p.UserHandle, p.RPID, p.Name, p.DataJSON, boolInt(p.Passwordless), time.Now().Unix())
	if err != nil {
		if isUnique(err) {
			return 0, ErrPasskeyTaken
		}
		return 0, fmt.Errorf("add passkey: %w", err)
	}
	return res.LastInsertId()
}

// UserPasskeys lists a user's credentials, oldest first.
func (s *Store) UserPasskeys(ctx context.Context, userID int64) ([]Passkey, error) {
	rows, err := s.db.QueryContext(ctx, passkeyCols+` WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list passkeys: %w", err)
	}
	defer rows.Close()
	var out []Passkey
	for rows.Next() {
		p, err := scanPasskey(rows)
		if err != nil {
			return nil, fmt.Errorf("scan passkey: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PasskeyByCredentialID loads a credential by its base64url id.
func (s *Store) PasskeyByCredentialID(ctx context.Context, credentialID string) (Passkey, error) {
	return scanPasskey(s.db.QueryRowContext(ctx, passkeyCols+` WHERE credential_id = ?`, credentialID))
}

// PasskeyUsed stores the credential record after a sign-in (new sign counter and flags)
// and the time of use.
func (s *Store) PasskeyUsed(ctx context.Context, id int64, dataJSON string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE webauthn_credentials SET data_json = ?, last_used_at = ? WHERE id = ?`,
		dataJSON, time.Now().Unix(), id)
	return err
}

// SetPasskeyOptions renames a user's credential and turns passwordless sign-in on or off.
func (s *Store) SetPasskeyOptions(ctx context.Context, userID, id int64, name string, passwordless bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE webauthn_credentials SET name = ?, passwordless = ? WHERE id = ? AND user_id = ?`,
		name, boolInt(passwordless), id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

// DeletePasskey removes one of a user's credentials.
func (s *Store) DeletePasskey(ctx context.Context, userID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM webauthn_credentials WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}
