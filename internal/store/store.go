// Package store keeps Moorage's own state (users, sessions) in SQLite.
// Logs are never stored here.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, keeps the binary static
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps the SQLite database.
type Store struct{ db *sql.DB }

// User is an account that can sign in.
type User struct {
	ID              int64
	Username        string
	PasswordHash    string
	Role            string // "admin" or "viewer"
	TOTPSecret      string
	TOTPLastCounter int64
	Active          bool // false until TOTP enrolment is confirmed

	AuthSource  string // "local", "oidc" or "proxy"
	Disabled    bool   // set by an admin; wins over the IdP and the proxy
	CreatedBy   string // username of the admin who invited them, "" for setup and external users
	LastLoginAt time.Time
	DisplayName string // informational (OIDC claims or the invite)
	Email       string // informational (OIDC claims)
	CreatedAt   time.Time
}

// Session is a signed-in browser.
type Session struct {
	ID         int64 // rowid, shown to the owner to sign one session out
	UserID     int64
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
	IP         string
	UserAgent  string
}

var migrations = []string{
	`CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		username TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL,
		role TEXT NOT NULL CHECK (role IN ('admin','viewer')),
		totp_secret TEXT NOT NULL,
		totp_last_counter INTEGER NOT NULL DEFAULT 0,
		active INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE sessions (
		token_hash TEXT PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL
	);
	CREATE INDEX sessions_expires ON sessions(expires_at);`,
	// M3: agents paired with this hub, and one-time join tokens
	`CREATE TABLE agents (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		hostname TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		last_seen INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE join_tokens (
		token_hash TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL
	);`,
	// M4: checks, their results, incidents, notification channels, stack events
	`CREATE TABLE checks (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		kind TEXT NOT NULL,
		target TEXT NOT NULL,
		host_id TEXT NOT NULL DEFAULT '',
		stack TEXT NOT NULL DEFAULT '',
		interval_s INTEGER NOT NULL,
		timeout_s INTEGER NOT NULL,
		retries INTEGER NOT NULL,
		settings TEXT NOT NULL DEFAULT '{}',
		enabled INTEGER NOT NULL DEFAULT 1,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE check_results (
		check_id INTEGER NOT NULL REFERENCES checks(id) ON DELETE CASCADE,
		ts INTEGER NOT NULL,
		status TEXT NOT NULL,
		latency_ms INTEGER NOT NULL,
		msg TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX check_results_check_ts ON check_results(check_id, ts);
	CREATE TABLE incidents (
		id INTEGER PRIMARY KEY,
		check_id INTEGER NOT NULL REFERENCES checks(id) ON DELETE CASCADE,
		started_at INTEGER NOT NULL,
		ended_at INTEGER,
		cause TEXT NOT NULL,
		context TEXT NOT NULL DEFAULT '{}'
	);
	CREATE INDEX incidents_check ON incidents(check_id, started_at);
	CREATE TABLE channels (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		kind TEXT NOT NULL,
		config TEXT NOT NULL,
		enabled INTEGER NOT NULL DEFAULT 1,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE check_channels (
		check_id INTEGER NOT NULL REFERENCES checks(id) ON DELETE CASCADE,
		channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
		PRIMARY KEY (check_id, channel_id)
	);
	CREATE TABLE events (
		id INTEGER PRIMARY KEY,
		ts INTEGER NOT NULL,
		host_id TEXT NOT NULL,
		stack TEXT NOT NULL,
		kind TEXT NOT NULL,
		actor TEXT NOT NULL DEFAULT '',
		detail TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX events_stack ON events(host_id, stack, ts);`,
	// M6: coordinated hub watchdog settings and per-agent participation
	`CREATE TABLE watchdog (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		enabled INTEGER NOT NULL DEFAULT 0,
		channel_id INTEGER REFERENCES channels(id) ON DELETE SET NULL,
		grace_s INTEGER NOT NULL DEFAULT 120,
		maintenance_until INTEGER NOT NULL DEFAULT 0
	);
	INSERT INTO watchdog (id, enabled, grace_s, maintenance_until) VALUES (1, 0, 120, 0);
	ALTER TABLE agents ADD COLUMN watchdog INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE agents ADD COLUMN watchdog_addr TEXT NOT NULL DEFAULT '';
	ALTER TABLE agents ADD COLUMN watchdog_cert BLOB;`,
	// M7: public status pages and their items
	`CREATE TABLE status_pages (
		id INTEGER PRIMARY KEY,
		slug TEXT NOT NULL UNIQUE,
		title TEXT NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		domain TEXT UNIQUE,
		published INTEGER NOT NULL DEFAULT 0,
		show_incidents INTEGER NOT NULL DEFAULT 1,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);
	CREATE TABLE status_page_items (
		id INTEGER PRIMARY KEY,
		page_id INTEGER NOT NULL REFERENCES status_pages(id) ON DELETE CASCADE,
		position INTEGER NOT NULL,
		section TEXT NOT NULL DEFAULT '',
		display_name TEXT NOT NULL,
		check_id INTEGER REFERENCES checks(id) ON DELETE CASCADE,
		host_id TEXT NOT NULL DEFAULT '',
		stack TEXT NOT NULL DEFAULT '',
		CHECK ((check_id IS NOT NULL AND host_id = '' AND stack = '') OR (check_id IS NULL AND host_id != '' AND stack != ''))
	);
	CREATE INDEX status_page_items_page ON status_page_items(page_id, position);`,
	// M8: image auto-update settings, per-stack policies, persisted check state, and update history
	`CREATE TABLE update_settings (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		enabled INTEGER NOT NULL DEFAULT 1,
		interval_s INTEGER NOT NULL DEFAULT 21600,
		window_start TEXT NOT NULL DEFAULT '03:00',
		window_end TEXT NOT NULL DEFAULT '05:00',
		channels TEXT NOT NULL DEFAULT '[]'
	);
	INSERT INTO update_settings (id, enabled, interval_s, window_start, window_end, channels)
	VALUES (1, 1, 21600, '03:00', '05:00', '[]');
	CREATE TABLE stack_update_policies (
		host_id TEXT NOT NULL,
		stack TEXT NOT NULL,
		policy TEXT NOT NULL DEFAULT 'notify' CHECK (policy IN ('off', 'notify', 'auto')),
		skipped_digests TEXT NOT NULL DEFAULT '{}',
		last_auto_at INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (host_id, stack)
	);
	CREATE TABLE image_checks (
		host_id TEXT NOT NULL,
		stack TEXT NOT NULL,
		service TEXT NOT NULL,
		ref TEXT NOT NULL DEFAULT '',
		local_id TEXT NOT NULL DEFAULT '',
		local_digest TEXT NOT NULL DEFAULT '',
		running_id TEXT NOT NULL DEFAULT '',
		remote_digest TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT '',
		error TEXT NOT NULL DEFAULT '',
		newer_tag TEXT NOT NULL DEFAULT '',
		newer_major TEXT NOT NULL DEFAULT '',
		checked_at INTEGER NOT NULL DEFAULT 0,
		announced_digest TEXT NOT NULL DEFAULT '',
		announced_tag TEXT NOT NULL DEFAULT '',
		announced_major TEXT NOT NULL DEFAULT '',
		announced_fail_digest TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (host_id, stack, service)
	);
	CREATE TABLE image_updates (
		id INTEGER PRIMARY KEY,
		host_id TEXT NOT NULL,
		stack TEXT NOT NULL,
		started_at INTEGER NOT NULL,
		finished_at INTEGER NOT NULL DEFAULT 0,
		actor TEXT NOT NULL,
		result TEXT NOT NULL,
		services_json TEXT NOT NULL DEFAULT '[]',
		log_tail TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX image_updates_stack ON image_updates(host_id, stack, started_at DESC);`,
	// M9: web terminal session audit log (1-year retention; never records typed input or output)
	`CREATE TABLE terminal_sessions (
		id INTEGER PRIMARY KEY,
		user TEXT NOT NULL,
		ip TEXT NOT NULL,
		host_id TEXT NOT NULL,
		stack TEXT NOT NULL,
		container_name TEXT NOT NULL,
		service TEXT NOT NULL DEFAULT '',
		shell TEXT NOT NULL,
		started_at INTEGER NOT NULL,
		ended_at INTEGER NOT NULL DEFAULT 0,
		reason TEXT NOT NULL DEFAULT '',
		exit_code INTEGER
	);
	CREATE INDEX terminal_sessions_started ON terminal_sessions(started_at DESC);
	CREATE INDEX terminal_sessions_stack ON terminal_sessions(host_id, stack, started_at DESC);`,
	// R1: audit log of every change made through Moorage (1-year retention; never request bodies)
	`CREATE TABLE audit_log (
		id INTEGER PRIMARY KEY,
		at INTEGER NOT NULL,
		actor TEXT NOT NULL,
		ip TEXT NOT NULL DEFAULT '',
		action TEXT NOT NULL,
		host_id TEXT NOT NULL DEFAULT '',
		stack TEXT NOT NULL DEFAULT '',
		target TEXT NOT NULL DEFAULT '',
		outcome TEXT NOT NULL CHECK (outcome IN ('ok', 'failed'))
	);
	CREATE INDEX audit_log_at ON audit_log(at);
	CREATE INDEX audit_log_stack ON audit_log(host_id, stack, at);`,
	// R2: planned maintenance windows. The scope is one JSON column, because it is always
	// read and written whole together with its window and never queried by item.
	`CREATE TABLE maintenance_windows (
		id INTEGER PRIMARY KEY,
		title TEXT NOT NULL,
		message TEXT NOT NULL DEFAULT '',
		scope TEXT NOT NULL DEFAULT '{}',
		kind TEXT NOT NULL CHECK (kind IN ('once', 'weekly')),
		starts_at INTEGER NOT NULL DEFAULT 0,
		ends_at INTEGER NOT NULL DEFAULT 0,
		weekdays INTEGER NOT NULL DEFAULT 0,
		start_time TEXT NOT NULL DEFAULT '',
		duration_m INTEGER NOT NULL DEFAULT 0,
		tz TEXT NOT NULL DEFAULT '',
		enabled INTEGER NOT NULL DEFAULT 1,
		created_by TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);`,
	// R5: dependency edges between stacks (from depends on to), and the cascade root an
	// incident was folded into ("host/stack", empty when it alerted on its own).
	`CREATE TABLE stack_deps (
		id INTEGER PRIMARY KEY,
		from_host TEXT NOT NULL,
		from_stack TEXT NOT NULL,
		to_host TEXT NOT NULL,
		to_stack TEXT NOT NULL,
		source TEXT NOT NULL CHECK (source IN ('detected', 'manual')),
		reason TEXT NOT NULL DEFAULT '',
		detail TEXT NOT NULL DEFAULT '',
		ignored INTEGER NOT NULL DEFAULT 0,
		created_by TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		UNIQUE (from_host, from_stack, to_host, to_stack),
		CHECK (NOT (from_host = to_host AND from_stack = to_stack))
	);
	CREATE INDEX stack_deps_to ON stack_deps(to_host, to_stack);
	ALTER TABLE incidents ADD COLUMN caused_by TEXT NOT NULL DEFAULT '';`,
	// R6: checks from several locations. locations is a JSON list of location ids ("hub"
	// and agent ids); empty means the hub only, exactly as before. Per-location answers
	// get their own table with the same retention as check_results; ok is NULL when the
	// location didn't vote (offline, unsupported, busy, no answer).
	`ALTER TABLE checks ADD COLUMN locations TEXT NOT NULL DEFAULT '';
	ALTER TABLE checks ADD COLUMN quorum TEXT NOT NULL DEFAULT '';
	CREATE TABLE check_location_results (
		check_id INTEGER NOT NULL REFERENCES checks(id) ON DELETE CASCADE,
		ts INTEGER NOT NULL,
		location TEXT NOT NULL,
		ok INTEGER,
		latency_ms INTEGER NOT NULL DEFAULT 0,
		msg TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX check_location_results_check_ts ON check_location_results(check_id, ts);`,
	// R7: remote stack template indexes added by admins, and the last good copy of each
	// index's templates. The cache is replaced only by a successful refresh, so a failing
	// index keeps serving what it had. meta is the raw template.yaml, parsed on read.
	`CREATE TABLE template_sources (
		id INTEGER PRIMARY KEY,
		url TEXT NOT NULL UNIQUE,
		allow_local INTEGER NOT NULL DEFAULT 0,
		name TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		last_attempt INTEGER NOT NULL DEFAULT 0,
		last_ok INTEGER NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE template_cache (
		source_id INTEGER NOT NULL REFERENCES template_sources(id) ON DELETE CASCADE,
		tpl_id TEXT NOT NULL,
		meta TEXT NOT NULL DEFAULT '',
		compose TEXT NOT NULL DEFAULT '',
		env TEXT NOT NULL DEFAULT '',
		error TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (source_id, tpl_id)
	);`,
	// M10: user management, OIDC and forward-auth sign-in. auth_source says who owns the
	// credentials (local password+TOTP, an OIDC provider, or a trusted proxy). External
	// accounts are found only by (issuer, subject), never by name or e-mail; forward-auth
	// users use the issuer "proxy". user_tokens are one-time invite and reset links, stored
	// hashed; the password and TOTP secret chosen while finishing wait on the token row, so
	// a reset replaces nothing until it is finished. Sessions learn when and from where
	// they were used, for the Account page.
	`ALTER TABLE users ADD COLUMN auth_source TEXT NOT NULL DEFAULT 'local' CHECK (auth_source IN ('local', 'oidc', 'proxy'));
	ALTER TABLE users ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE users ADD COLUMN created_by TEXT NOT NULL DEFAULT '';
	ALTER TABLE users ADD COLUMN last_login_at INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE users ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
	ALTER TABLE users ADD COLUMN email TEXT NOT NULL DEFAULT '';
	CREATE UNIQUE INDEX users_username_nocase ON users(username COLLATE NOCASE);
	CREATE TABLE user_identities (
		issuer TEXT NOT NULL,
		subject TEXT NOT NULL,
		user_id INTEGER NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
		created_at INTEGER NOT NULL,
		PRIMARY KEY (issuer, subject)
	);
	CREATE TABLE user_tokens (
		token_hash TEXT PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		kind TEXT NOT NULL CHECK (kind IN ('invite', 'reset')),
		created_by TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		pending_password_hash TEXT NOT NULL DEFAULT '',
		pending_totp_secret TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX user_tokens_user ON user_tokens(user_id);
	ALTER TABLE sessions ADD COLUMN last_seen_at INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sessions ADD COLUMN ip TEXT NOT NULL DEFAULT '';
	ALTER TABLE sessions ADD COLUMN user_agent TEXT NOT NULL DEFAULT '';
	CREATE INDEX sessions_user ON sessions(user_id);`,
	// M12: Moorage self-update job state persisted across hub restarts so an in-progress
	// hub update can reconcile its outcome ("updated to X" / "rolled back") and write
	// the delayed audit row once the restarted process comes back.
	`CREATE TABLE system_updates (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		node_id      TEXT    NOT NULL,
		node_name    TEXT    NOT NULL,
		action       TEXT    NOT NULL,
		from_version TEXT    NOT NULL,
		to_version   TEXT    NOT NULL,
		mode         TEXT    NOT NULL DEFAULT '',
		status       TEXT    NOT NULL,
		message      TEXT    NOT NULL DEFAULT '',
		actor        TEXT    NOT NULL DEFAULT '',
		ip           TEXT    NOT NULL DEFAULT '',
		started_at   INTEGER NOT NULL,
		finished_at  INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX idx_system_updates_node ON system_updates(node_id, started_at DESC);`,
	`CREATE TABLE addons (
		id         TEXT    PRIMARY KEY,
		enabled    INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);
	CREATE TABLE installed_addons (
		id           TEXT    PRIMARY KEY,
		name         TEXT    NOT NULL,
		kind         TEXT    NOT NULL,
		category     TEXT    NOT NULL DEFAULT '',
		description  TEXT    NOT NULL DEFAULT '',
		version      TEXT    NOT NULL DEFAULT '1',
		host_id      TEXT    NOT NULL DEFAULT '',
		stack_name   TEXT    NOT NULL DEFAULT '',
		ram          TEXT    NOT NULL DEFAULT '',
		cpu          TEXT    NOT NULL DEFAULT '',
		enabled      INTEGER NOT NULL DEFAULT 1,
		installed_at INTEGER NOT NULL,
		updated_at   INTEGER NOT NULL
	);`,
	`CREATE TABLE app_settings (
		key        TEXT    PRIMARY KEY,
		value      TEXT    NOT NULL,
		updated_at INTEGER NOT NULL
	);`,
	`CREATE TABLE auto_rollbacks (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		host_id      TEXT    NOT NULL,
		stack        TEXT    NOT NULL,
		update_id    INTEGER NOT NULL,
		check_name   TEXT    NOT NULL,
		reason       TEXT    NOT NULL DEFAULT '',
		at           INTEGER NOT NULL,
		acknowledged INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX idx_auto_rollbacks_unacked ON auto_rollbacks(acknowledged, at DESC);`,
	// Web Push: one row per browser that allowed notifications.
	`CREATE TABLE push_subscriptions (
		id       INTEGER PRIMARY KEY,
		user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		endpoint TEXT    NOT NULL UNIQUE,
		p256dh   TEXT    NOT NULL,
		auth     TEXT    NOT NULL,
		created  INTEGER NOT NULL
	);
	CREATE INDEX idx_push_subscriptions_user ON push_subscriptions(user_id);`,
	// Per-stack settings: the deploy strategy (standard recreate or rolling / safe-swap).
	`CREATE TABLE stack_settings (
		host_id  TEXT NOT NULL,
		stack    TEXT NOT NULL,
		strategy TEXT NOT NULL DEFAULT 'standard',
		PRIMARY KEY (host_id, stack)
	);`,
	// Dependencies: restart a dependent stack when the stack it depends on recovers or deploys.
	`ALTER TABLE stack_deps ADD COLUMN auto_restart TEXT NOT NULL DEFAULT 'off';`,
	// Sidecar add-ons install per host: the key becomes (id, host_id). Custom add-ons keep
	// their own compose fragment and the .env keys they own.
	`CREATE TABLE installed_addons_v2 (
		id               TEXT    NOT NULL,
		host_id          TEXT    NOT NULL DEFAULT '',
		name             TEXT    NOT NULL,
		kind             TEXT    NOT NULL,
		category         TEXT    NOT NULL DEFAULT '',
		description      TEXT    NOT NULL DEFAULT '',
		version          TEXT    NOT NULL DEFAULT '1',
		stack_name       TEXT    NOT NULL DEFAULT '',
		ram              TEXT    NOT NULL DEFAULT '',
		cpu              TEXT    NOT NULL DEFAULT '',
		enabled          INTEGER NOT NULL DEFAULT 1,
		installed_at     INTEGER NOT NULL,
		updated_at       INTEGER NOT NULL,
		custom           INTEGER NOT NULL DEFAULT 0,
		compose_fragment TEXT    NOT NULL DEFAULT '',
		env_keys         TEXT    NOT NULL DEFAULT '',
		source           TEXT    NOT NULL DEFAULT '',
		PRIMARY KEY (id, host_id)
	);
	INSERT INTO installed_addons_v2 (id, host_id, name, kind, category, description, version, stack_name, ram, cpu, enabled, installed_at, updated_at)
		SELECT id, host_id, name, kind, category, description, version, stack_name, ram, cpu, enabled, installed_at, updated_at FROM installed_addons;
	DROP TABLE installed_addons;
	ALTER TABLE installed_addons_v2 RENAME TO installed_addons;`,
	// Custom HTTPS catalogues of add-ons (like template sources).
	`CREATE TABLE addon_sources (
		id            INTEGER PRIMARY KEY,
		name          TEXT    NOT NULL,
		url           TEXT    NOT NULL UNIQUE,
		allow_private INTEGER NOT NULL DEFAULT 0,
		enabled       INTEGER NOT NULL DEFAULT 1,
		cache         TEXT    NOT NULL DEFAULT '',
		fetched_at    INTEGER NOT NULL DEFAULT 0,
		error         TEXT    NOT NULL DEFAULT '',
		created_at    INTEGER NOT NULL
	);`,
	// MCP server: tokens (only their SHA-256 is stored) and drafts waiting for approval.
	`CREATE TABLE mcp_tokens (
		id               INTEGER PRIMARY KEY,
		name             TEXT    NOT NULL UNIQUE,
		token_hash       TEXT    NOT NULL UNIQUE,
		acl              TEXT    NOT NULL DEFAULT '[]',
		require_approval INTEGER NOT NULL DEFAULT 0,
		expires_at       INTEGER NOT NULL DEFAULT 0,
		created_at       INTEGER NOT NULL,
		created_by       TEXT    NOT NULL DEFAULT '',
		last_used_at     INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE mcp_drafts (
		id          INTEGER PRIMARY KEY,
		token_name  TEXT    NOT NULL,
		tool        TEXT    NOT NULL,
		host_id     TEXT    NOT NULL,
		stack       TEXT    NOT NULL,
		compose     TEXT    NOT NULL DEFAULT '',
		env         TEXT    NOT NULL DEFAULT '',
		env_keys    TEXT    NOT NULL DEFAULT '[]',
		note        TEXT    NOT NULL DEFAULT '',
		deploy      INTEGER NOT NULL DEFAULT 0,
		create_new  INTEGER NOT NULL DEFAULT 0,
		status      TEXT    NOT NULL DEFAULT 'pending',
		created_at  INTEGER NOT NULL,
		decided_by  TEXT    NOT NULL DEFAULT '',
		decided_at  INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX idx_mcp_drafts_status ON mcp_drafts(status, created_at DESC);`,
	// Scheduled tasks (stack-scheduler add-on).
	`CREATE TABLE scheduled_tasks (
		id          INTEGER PRIMARY KEY,
		name        TEXT    NOT NULL,
		host_id     TEXT    NOT NULL,
		stack       TEXT    NOT NULL,
		cron        TEXT    NOT NULL,
		action      TEXT    NOT NULL,
		service     TEXT    NOT NULL DEFAULT '',
		command     TEXT    NOT NULL DEFAULT '',
		enabled     INTEGER NOT NULL DEFAULT 1,
		channels    TEXT    NOT NULL DEFAULT '[]',
		last_run_at INTEGER NOT NULL DEFAULT 0,
		last_status TEXT    NOT NULL DEFAULT '',
		last_output TEXT    NOT NULL DEFAULT '',
		created_at  INTEGER NOT NULL
	);`,
	// Registry webhooks (registry-webhooks add-on). The token is stored as a SHA-256; the
	// optional signing secret has to stay readable to verify X-Hub-Signature-256.
	`CREATE TABLE registry_webhooks (
		id          INTEGER PRIMARY KEY,
		name        TEXT    NOT NULL,
		provider    TEXT    NOT NULL,
		token_hash  TEXT    NOT NULL UNIQUE,
		secret      TEXT    NOT NULL DEFAULT '',
		apply_now   INTEGER NOT NULL DEFAULT 0,
		enabled     INTEGER NOT NULL DEFAULT 1,
		created_at  INTEGER NOT NULL,
		last_hit_at INTEGER NOT NULL DEFAULT 0,
		last_result TEXT    NOT NULL DEFAULT ''
	);`,
	// Shared variables offered to templates and .env editors.
	`CREATE TABLE shared_vars (
		id         INTEGER PRIMARY KEY,
		key        TEXT    NOT NULL,
		value      TEXT    NOT NULL DEFAULT '',
		host_id    TEXT    NOT NULL DEFAULT '',
		secret     INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL,
		UNIQUE (key, host_id)
	);`,
}

// Open opens (and migrates) moorage.db in dataDir.
func Open(dataDir string) (*Store, error) {
	dbPath := filepath.Join(dataDir, "moorage.db")
	restorePath := filepath.Join(dataDir, "moorage.db.restore")
	if _, err := os.Stat(restorePath); err == nil {
		if _, err := os.Stat(dbPath); err == nil {
			_ = os.Rename(dbPath, dbPath+".bak")
		}
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
		if err := os.Rename(restorePath, dbPath); err != nil {
			return nil, fmt.Errorf("apply staged restore: %w", err)
		}
	}
	dsn := "file:" + dbPath +
		"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: one writer, avoids SQLITE_BUSY
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (v INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(v),0) FROM schema_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version (v) VALUES (?)`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// CountActiveUsers returns how many users finished enrolment.
func (s *Store) CountActiveUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE active = 1`).Scan(&n)
	return n, err
}

// UpsertPendingUser creates or replaces a user that has not finished enrolment.
// Active users are never replaced.
func (s *Store) UpsertPendingUser(ctx context.Context, u User) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active int
	err = tx.QueryRowContext(ctx, `SELECT active FROM users WHERE username = ?`, u.Username).Scan(&active)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case active == 1:
		return errors.New("user already exists")
	default:
		if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE username = ?`, u.Username); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, role, totp_secret, active, created_at) VALUES (?,?,?,?,0,?)`,
		u.Username, u.PasswordHash, u.Role, u.TOTPSecret, time.Now().Unix())
	if err != nil {
		return err
	}
	return tx.Commit()
}
