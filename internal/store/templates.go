package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrDuplicate is returned when a unique value (a template index URL) already exists.
var ErrDuplicate = errors.New("already exists")

// TemplateSource is a remote template index an admin added by URL.
type TemplateSource struct {
	ID          int64
	URL         string
	AllowLocal  bool // the admin allowed private and loopback addresses for this index
	Name        string
	CreatedAt   time.Time
	LastAttempt time.Time // zero = never fetched
	LastOK      time.Time // zero = never fetched successfully
	LastError   string
}

// CachedTemplate is one template of a remote index as last fetched successfully.
// Err is set when that one template couldn't be fetched or parsed; it is kept so the
// catalogue can show it with its problem instead of hiding it.
type CachedTemplate struct {
	SourceID int64
	ID       string
	Meta     string // raw template.yaml
	Compose  string
	Env      string
	Err      string
}

const templateSourceCols = `SELECT id, url, allow_local, name, created_at, last_attempt, last_ok, last_error FROM template_sources`

func scanTemplateSource(sc interface{ Scan(...any) error }) (TemplateSource, error) {
	var t TemplateSource
	var allow int
	var created, attempt, ok int64
	if err := sc.Scan(&t.ID, &t.URL, &allow, &t.Name, &created, &attempt, &ok, &t.LastError); err != nil {
		return t, err
	}
	t.AllowLocal = allow == 1
	t.CreatedAt = time.Unix(created, 0)
	if attempt > 0 {
		t.LastAttempt = time.Unix(attempt, 0)
	}
	if ok > 0 {
		t.LastOK = time.Unix(ok, 0)
	}
	return t, nil
}

// TemplateSources lists the remote template indexes in the order they were added.
func (s *Store) TemplateSources(ctx context.Context) ([]TemplateSource, error) {
	rows, err := s.db.QueryContext(ctx, templateSourceCols+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TemplateSource
	for rows.Next() {
		t, err := scanTemplateSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TemplateSourceByID loads one remote template index.
func (s *Store) TemplateSourceByID(ctx context.Context, id int64) (TemplateSource, error) {
	t, err := scanTemplateSource(s.db.QueryRowContext(ctx, templateSourceCols+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// AddTemplateSource stores a new remote index URL. ErrDuplicate when the URL is known.
func (s *Store) AddTemplateSource(ctx context.Context, url string, allowLocal bool) (int64, error) {
	if s.templateURLTaken(ctx, url, 0) {
		return 0, ErrDuplicate
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO template_sources (url, allow_local, created_at) VALUES (?, ?, ?)`,
		url, boolInt(allowLocal), time.Now().Unix())
	if err != nil {
		return 0, fmt.Errorf("add template source: %w", err)
	}
	return res.LastInsertId()
}

func (s *Store) templateURLTaken(ctx context.Context, url string, except int64) bool {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM template_sources WHERE url = ? AND id <> ?`, url, except).Scan(&n)
	return err == nil && n > 0
}

// UpdateTemplateSource changes the URL or the local-network permission. A new URL is a
// different index, so its old cache and status are dropped.
func (s *Store) UpdateTemplateSource(ctx context.Context, id int64, url string, allowLocal bool) error {
	cur, err := s.TemplateSourceByID(ctx, id)
	if err != nil {
		return err
	}
	if s.templateURLTaken(ctx, url, id) {
		return ErrDuplicate
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `UPDATE template_sources SET url = ?, allow_local = ? WHERE id = ?`, url, boolInt(allowLocal), id); err != nil {
		return fmt.Errorf("update template source: %w", err)
	}
	if cur.URL != url {
		if _, err := tx.ExecContext(ctx, `UPDATE template_sources SET name = '', last_attempt = 0, last_ok = 0, last_error = '' WHERE id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM template_cache WHERE source_id = ?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteTemplateSource removes an index and its cached templates.
func (s *Store) DeleteTemplateSource(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM template_sources WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TemplateFetchFailed records a failed refresh. The cached templates stay untouched.
func (s *Store) TemplateFetchFailed(ctx context.Context, id int64, at time.Time, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE template_sources SET last_attempt = ?, last_error = ? WHERE id = ?`, at.Unix(), msg, id)
	return err
}

// TemplateFetchOK replaces the cached templates of an index after a successful refresh.
// The URL guard makes a refresh that raced with a URL change a no-op.
func (s *Store) TemplateFetchOK(ctx context.Context, id int64, url string, at time.Time, name string, list []CachedTemplate) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := tx.ExecContext(ctx, `UPDATE template_sources SET name = ?, last_attempt = ?, last_ok = ?, last_error = '' WHERE id = ? AND url = ?`,
		name, at.Unix(), at.Unix(), id, url)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM template_cache WHERE source_id = ?`, id); err != nil {
		return err
	}
	for _, t := range list {
		if _, err := tx.ExecContext(ctx, `INSERT INTO template_cache (source_id, tpl_id, meta, compose, env, error) VALUES (?, ?, ?, ?, ?, ?)`,
			id, t.ID, t.Meta, t.Compose, t.Env, t.Err); err != nil {
			return fmt.Errorf("cache template %s: %w", t.ID, err)
		}
	}
	return tx.Commit()
}

// CachedTemplateMetas lists the cached templates of every index without their files
// (the catalogue only needs template.yaml).
func (s *Store) CachedTemplateMetas(ctx context.Context) ([]CachedTemplate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source_id, tpl_id, meta, error FROM template_cache ORDER BY source_id, tpl_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CachedTemplate
	for rows.Next() {
		var t CachedTemplate
		if err := rows.Scan(&t.SourceID, &t.ID, &t.Meta, &t.Err); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CachedTemplate loads one cached template with its files.
func (s *Store) CachedTemplate(ctx context.Context, sourceID int64, id string) (CachedTemplate, error) {
	t := CachedTemplate{SourceID: sourceID, ID: id}
	err := s.db.QueryRowContext(ctx, `SELECT meta, compose, env, error FROM template_cache WHERE source_id = ? AND tpl_id = ?`, sourceID, id).
		Scan(&t.Meta, &t.Compose, &t.Env, &t.Err)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}
