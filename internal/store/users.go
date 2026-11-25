package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Errors of the guarded user changes.
var (
	// ErrLastAdmin refuses a change that would leave no break-glass admin.
	ErrLastAdmin = errors.New("last local admin")
	// ErrUsernameTaken is a username that already exists (case-insensitive).
	ErrUsernameTaken = errors.New("username taken")
)

// Auth sources.
const (
	SourceLocal = "local"
	SourceOIDC  = "oidc"
	SourceProxy = "proxy"
)

// breakGlass is the SQL condition of an account that can always sign in, even when the
// IdP or the proxy is down: a finished, enabled local admin with TOTP.
const breakGlass = `role = 'admin' AND auth_source = 'local' AND active = 1 AND disabled = 0 AND totp_secret != ''`

// UserByName loads a user by username, ignoring case.
func (s *Store) UserByName(ctx context.Context, username string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, userCols+` WHERE username = ? COLLATE NOCASE`, username))
}

// UserByID loads a user by id.
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, userCols+` WHERE id = ?`, id))
}

const userCols = `SELECT id, username, password_hash, role, totp_secret, totp_last_counter, active,
	auth_source, disabled, created_by, last_login_at, display_name, email, preferences_json, created_at FROM users`

type rowScanner interface{ Scan(dest ...any) error }

func scanUser(row rowScanner) (User, error) {
	var u User
	var active, disabled int
	var lastLogin, created int64
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.TOTPSecret, &u.TOTPLastCounter, &active,
		&u.AuthSource, &disabled, &u.CreatedBy, &lastLogin, &u.DisplayName, &u.Email, &u.PreferencesJSON, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	u.Active = active == 1
	u.Disabled = disabled == 1
	u.LastLoginAt = unixOrZero(lastLogin)
	u.CreatedAt = unixOrZero(created)
	return u, err
}

func unixOrZero(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0)
}

// ListUsers returns every account, oldest first, with the expiry of a pending invite or
// reset link (zero when there is none).
func (s *Store) ListUsers(ctx context.Context) ([]UserListEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT u.id, u.username, u.password_hash, u.role, u.totp_secret, u.totp_last_counter,
		u.active, u.auth_source, u.disabled, u.created_by, u.last_login_at, u.display_name, u.email, u.preferences_json, u.created_at,
		COALESCE((SELECT MAX(t.expires_at) FROM user_tokens t WHERE t.user_id = u.id AND t.expires_at > ?), 0),
		COALESCE((SELECT t.kind FROM user_tokens t WHERE t.user_id = u.id AND t.expires_at > ? ORDER BY t.expires_at DESC LIMIT 1), '')
		FROM users u ORDER BY u.id`, time.Now().Unix(), time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	var out []UserListEntry
	for rows.Next() {
		var e UserListEntry
		var active, disabled int
		var lastLogin, created, linkExp int64
		if err := rows.Scan(&e.ID, &e.Username, &e.PasswordHash, &e.Role, &e.TOTPSecret, &e.TOTPLastCounter, &active,
			&e.AuthSource, &disabled, &e.CreatedBy, &lastLogin, &e.DisplayName, &e.Email, &e.PreferencesJSON, &created, &linkExp, &e.LinkKind); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		e.Active, e.Disabled = active == 1, disabled == 1
		e.LastLoginAt, e.CreatedAt, e.LinkExpires = unixOrZero(lastLogin), unixOrZero(created), unixOrZero(linkExp)
		out = append(out, e)
	}
	return out, rows.Err()
}

// UserListEntry is a user plus its open invite or reset link, if any.
type UserListEntry struct {
	User
	LinkKind    string // "invite", "reset" or ""
	LinkExpires time.Time
}

// CreateInvitedUser adds a local user that can't sign in until the invite is finished.
func (s *Store) CreateInvitedUser(ctx context.Context, username, role, displayName, createdBy string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO users (username, password_hash, role, totp_secret, active, created_at,
		auth_source, created_by, display_name) VALUES (?, '', ?, '', 0, ?, 'local', ?, ?)`,
		username, role, time.Now().Unix(), createdBy, displayName)
	if err != nil {
		if isUnique(err) {
			return 0, ErrUsernameTaken
		}
		return 0, fmt.Errorf("create user: %w", err)
	}
	return res.LastInsertId()
}

func isUnique(err error) bool { return strings.Contains(err.Error(), "UNIQUE constraint failed") }

// ActivateUser marks enrolment as finished.
func (s *Store) ActivateUser(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET active = 1 WHERE id = ?`, id)
	return err
}

// UseTOTPCounter stores the last accepted TOTP time step. It only succeeds when
// counter is newer than the stored one, which blocks code reuse.
func (s *Store) UseTOTPCounter(ctx context.Context, id, counter int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET totp_last_counter = ? WHERE id = ? AND totp_last_counter < ?`, counter, id, counter)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// SetLastLogin records a successful sign-in.
func (s *Store) SetLastLogin(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, time.Now().Unix(), id)
	return err
}

// SetUserPreferences stores the serialized per-user UX preferences JSON.
func (s *Store) SetUserPreferences(ctx context.Context, id int64, prefsJSON string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET preferences_json = ? WHERE id = ?`, prefsJSON, id)
	return err
}

// SetPassword replaces a local user's password hash and signs out every session but keep
// (a token hash; "" signs out all).
func (s *Store) SetPassword(ctx context.Context, id int64, hash, keep string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, hash, id); err != nil {
			return fmt.Errorf("set password: %w", err)
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash != ?`, id, keep)
		return err
	})
}

// SetTOTPSecret replaces a user's TOTP secret; counter is the step of the code that
// confirmed it, so that code can't be used again to sign in.
func (s *Store) SetTOTPSecret(ctx context.Context, id int64, secret string, counter int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET totp_secret = ?, totp_last_counter = ? WHERE id = ?`, secret, counter, id)
	return err
}

// UpdateUserRole changes the role of a user without any checks (tests, migrations).
// The API goes through ChangeUser, which guards the last admin and ends sessions.
func (s *Store) UpdateUserRole(ctx context.Context, id int64, role string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, role, id)
	return err
}

// DeleteUser removes a user account and cascades its sessions, without any checks.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	return err
}

// UserChange is one admin change of another account. Exactly one field is set.
type UserChange struct {
	Role    string // new role
	Disable *bool  // disable (true) or enable (false)
	Delete  bool
}

// ChangeUser applies an admin change in one transaction, so two admins can't each remove
// the other one in a race. It refuses with ErrLastAdmin when the change would leave no
// break-glass admin, and ends the user's sessions, so the change applies to their next
// request. It returns the user as it was before.
func (s *Store) ChangeUser(ctx context.Context, id int64, c UserChange) (User, error) {
	var before User
	err := s.tx(ctx, func(tx *sql.Tx) error {
		u, err := scanUser(tx.QueryRowContext(ctx, userCols+` WHERE id = ?`, id))
		if err != nil {
			return err
		}
		before = u
		after := u
		switch {
		case c.Delete:
		case c.Disable != nil:
			after.Disabled = *c.Disable
		default:
			after.Role = c.Role
		}
		if isBreakGlass(u) && (c.Delete || !isBreakGlass(after)) {
			var others int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE `+breakGlass+` AND id != ?`, id).Scan(&others); err != nil {
				return err
			}
			if others == 0 {
				return ErrLastAdmin
			}
		}
		return applyUserChange(ctx, tx, id, c)
	})
	return before, err
}

func isBreakGlass(u User) bool {
	return u.Role == "admin" && u.AuthSource == SourceLocal && u.Active && !u.Disabled && u.TOTPSecret != ""
}

func applyUserChange(ctx context.Context, tx *sql.Tx, id int64, c UserChange) error {
	var err error
	switch {
	case c.Delete:
		_, err = tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
		return err // sessions, tokens and identities cascade
	case c.Disable != nil:
		_, err = tx.ExecContext(ctx, `UPDATE users SET disabled = ? WHERE id = ?`, boolInt(*c.Disable), id)
	default:
		_, err = tx.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, c.Role, id)
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id)
	return err
}

// CountBreakGlassAdmins counts finished, enabled local admins with TOTP.
func (s *Store) CountBreakGlassAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE `+breakGlass).Scan(&n)
	return n, err
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- invite and reset links ------------------------------------------------

// UserToken is a pending invite or reset link.
type UserToken struct {
	UserID              int64
	Kind                string // "invite" or "reset"
	ExpiresAt           time.Time
	PendingPasswordHash string
	PendingTOTPSecret   string
}

// IssueUserToken stores a new link for a user and drops any older one, so only the
// newest link works.
func (s *Store) IssueUserToken(ctx context.Context, hash string, userID int64, kind, createdBy string, expires time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_tokens WHERE user_id = ?`, userID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO user_tokens (token_hash, user_id, kind, created_by, created_at, expires_at)
			VALUES (?,?,?,?,?,?)`, hash, userID, kind, createdBy, time.Now().Unix(), expires.Unix())
		return err
	})
}

// UserTokenByHash returns a link that has not expired.
func (s *Store) UserTokenByHash(ctx context.Context, hash string) (UserToken, error) {
	var t UserToken
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT user_id, kind, expires_at, pending_password_hash, pending_totp_secret
		FROM user_tokens WHERE token_hash = ? AND expires_at > ?`, hash, time.Now().Unix()).
		Scan(&t.UserID, &t.Kind, &exp, &t.PendingPasswordHash, &t.PendingTOTPSecret)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	t.ExpiresAt = time.Unix(exp, 0)
	return t, err
}

// SetTokenPending keeps the chosen password hash and new TOTP secret on the link until
// the code confirms them.
func (s *Store) SetTokenPending(ctx context.Context, hash, passwordHash, totpSecret string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE user_tokens SET pending_password_hash = ?, pending_totp_secret = ?
		WHERE token_hash = ? AND expires_at > ?`, passwordHash, totpSecret, hash, time.Now().Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

// ConsumeUserToken finishes a link in one transaction: the pending password and TOTP
// secret replace the old ones, the user becomes active, every link of the user is
// dropped (one use only) and every session of the user ends (a reset signs out
// whoever had the old password).
func (s *Store) ConsumeUserToken(ctx context.Context, hash string, counter int64) (User, error) {
	var u User
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var userID int64
		var pw, secret string
		err := tx.QueryRowContext(ctx, `SELECT user_id, pending_password_hash, pending_totp_secret FROM user_tokens
			WHERE token_hash = ? AND expires_at > ?`, hash, time.Now().Unix()).Scan(&userID, &pw, &secret)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && (pw == "" || secret == "")) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		stmts := []struct {
			q    string
			args []any
		}{
			{`UPDATE users SET password_hash = ?, totp_secret = ?, totp_last_counter = ?, active = 1 WHERE id = ? AND auth_source = 'local'`,
				[]any{pw, secret, counter, userID}},
			{`DELETE FROM user_tokens WHERE user_id = ?`, []any{userID}},
			{`DELETE FROM sessions WHERE user_id = ?`, []any{userID}},
		}
		for _, st := range stmts {
			if _, err := tx.ExecContext(ctx, st.q, st.args...); err != nil {
				return err
			}
		}
		u, err = scanUser(tx.QueryRowContext(ctx, userCols+` WHERE id = ?`, userID))
		return err
	})
	return u, err
}

// RevokeUserTokens drops every open link of a user.
func (s *Store) RevokeUserTokens(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_tokens WHERE user_id = ?`, userID)
	return err
}

// ---- external identities ---------------------------------------------------

// UserByIdentity finds the account an OIDC (issuer, subject) or proxy user belongs to.
func (s *Store) UserByIdentity(ctx context.Context, issuer, subject string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, userCols+` WHERE id = (SELECT user_id FROM user_identities WHERE issuer = ? AND subject = ?)`,
		issuer, subject))
}

// CreateExternalUser adds an OIDC or proxy account and its identity. name(i) gives the
// i-th username to try (0 = the preferred one); a taken name moves to the next, so an
// external user never takes over an existing account with the same name.
func (s *Store) CreateExternalUser(ctx context.Context, issuer, subject string, u User, name func(int) string) (User, error) {
	var out User
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		var id int64
		for i := 0; ; i++ {
			if i >= 1000 {
				return errors.New("no free username")
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO users (username, password_hash, role, totp_secret, active, created_at,
				auth_source, display_name, email, last_login_at) VALUES (?, '', ?, '', 1, ?, ?, ?, ?, ?)`,
				name(i), u.Role, now, u.AuthSource, u.DisplayName, u.Email, now)
			if err != nil && isUnique(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("create external user: %w", err)
			}
			id, _ = res.LastInsertId()
			break
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_identities (issuer, subject, user_id, created_at) VALUES (?,?,?,?)`,
			issuer, subject, id, now); err != nil {
			return fmt.Errorf("link identity: %w", err)
		}
		var err error
		out, err = scanUser(tx.QueryRowContext(ctx, userCols+` WHERE id = ?`, id))
		return err
	})
	return out, err
}

// UpdateExternalLogin stores what the IdP or proxy said at this sign-in.
func (s *Store) UpdateExternalLogin(ctx context.Context, id int64, role, displayName, email string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET role = ?, display_name = ?, email = ?, last_login_at = ? WHERE id = ?`,
		role, displayName, email, time.Now().Unix(), id)
	return err
}

// ---- sessions --------------------------------------------------------------

// CreateSession stores a session by the hash of its token.
func (s *Store) CreateSession(ctx context.Context, tokenHash string, userID int64, expires time.Time) error {
	return s.CreateSessionMeta(ctx, tokenHash, userID, expires, "", "")
}

// CreateSessionMeta stores a session with the client's IP and user agent (shown to its owner).
func (s *Store) CreateSessionMeta(ctx context.Context, tokenHash string, userID int64, expires time.Time, ip, userAgent string) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at, last_seen_at, ip, user_agent) VALUES (?,?,?,?,?,?,?)`,
		tokenHash, userID, now, expires.Unix(), now, clipN(ip, 64), clipN(userAgent, 256))
	return err
}

func clipN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

const sessionCols = `SELECT rowid, user_id, created_at, expires_at, last_seen_at, ip, user_agent FROM sessions`

func scanSession(row rowScanner) (Session, error) {
	var sess Session
	var created, exp, seen int64
	err := row.Scan(&sess.ID, &sess.UserID, &created, &exp, &seen, &sess.IP, &sess.UserAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return sess, ErrNotFound
	}
	sess.CreatedAt, sess.ExpiresAt, sess.LastSeenAt = time.Unix(created, 0), time.Unix(exp, 0), unixOrZero(seen)
	return sess, err
}

// SessionByHash returns a non-expired session.
func (s *Store) SessionByHash(ctx context.Context, tokenHash string) (Session, error) {
	return scanSession(s.db.QueryRowContext(ctx, sessionCols+` WHERE token_hash = ? AND expires_at > ?`,
		tokenHash, time.Now().Unix()))
}

// TouchSession records that a session was used now.
func (s *Store) TouchSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`, time.Now().Unix(), tokenHash)
	return err
}

// UserSessions lists a user's live sessions, most recently used first.
func (s *Store) UserSessions(ctx context.Context, userID int64) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, sessionCols+` WHERE user_id = ? AND expires_at > ? ORDER BY last_seen_at DESC, rowid DESC`,
		userID, time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// DeleteSession removes a session (logout).
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// DeleteUserSession removes one session of a user by its id; ErrNotFound if it isn't theirs.
func (s *Store) DeleteUserSession(ctx context.Context, userID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND rowid = ?`, userID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

// DeleteUserSessions ends every session of a user except keep (a token hash; "" ends all).
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64, keep string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash != ?`, userID, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneSessions deletes expired sessions and expired invite/reset links.
func (s *Store) PruneSessions(ctx context.Context) error {
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_tokens WHERE expires_at <= ?`, now)
	return err
}
