package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// StatusPage is a configurable public status page.
type StatusPage struct {
	ID            int64
	Slug          string
	Title         string
	Description   string
	Domain        string // optional custom domain (stored as NULL when empty)
	Published     bool
	ShowIncidents bool
	Items         []StatusPageItem
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// StatusPageItem is one row on a status page: either a single check (CheckID > 0)
// or a stack (HostID != "" && Stack != "").
type StatusPageItem struct {
	ID          int64
	PageID      int64
	Position    int
	Section     string
	DisplayName string
	CheckID     int64
	HostID      string
	Stack       string
}

// DailyCheckCounts holds aggregated result counts for one check on one UTC day (ts_ms / 86_400_000).
type DailyCheckCounts struct {
	CheckID     int64
	Day         int64 // UTC day index since Unix epoch
	Up          int
	Down        int
	Silenced    int
	Paused      int
	Maintenance int // failures during a planned maintenance window
	Degraded    int // multi-target checks partially down
}

// IncidentSummary is a stripped incident row without Cause or Context, safe for status page computation.
type IncidentSummary struct {
	ID        int64
	CheckID   int64
	StartedAt time.Time
	EndedAt   time.Time // zero while open
}

const statusPageCols = `id, slug, title, description, COALESCE(domain, ''), published, show_incidents, created_at, updated_at`

func scanStatusPage(r scanner) (StatusPage, error) {
	var p StatusPage
	var pub, inc int
	var created, updated int64
	if err := r.Scan(&p.ID, &p.Slug, &p.Title, &p.Description, &p.Domain, &pub, &inc, &created, &updated); err != nil {
		return StatusPage{}, err
	}
	p.Published = pub == 1
	p.ShowIncidents = inc == 1
	p.CreatedAt = time.Unix(created, 0)
	p.UpdatedAt = time.Unix(updated, 0)
	return p, nil
}

// StatusPages lists all status pages with their ordered items.
func (s *Store) StatusPages(ctx context.Context) ([]StatusPage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+statusPageCols+` FROM status_pages ORDER BY title COLLATE NOCASE, id`)
	if err != nil {
		return nil, err
	}
	var out []StatusPage
	for rows.Next() {
		p, err := scanStatusPage(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	items, err := s.statusPageItems(ctx, 0)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Items = items[out[i].ID]
		if out[i].Items == nil {
			out[i].Items = []StatusPageItem{}
		}
	}
	return out, nil
}

func (s *Store) oneStatusPage(ctx context.Context, where string, arg any) (StatusPage, error) {
	p, err := scanStatusPage(s.db.QueryRowContext(ctx, `SELECT `+statusPageCols+` FROM status_pages WHERE `+where, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return StatusPage{}, ErrNotFound
	}
	if err != nil {
		return StatusPage{}, err
	}
	items, err := s.statusPageItems(ctx, p.ID)
	if err != nil {
		return StatusPage{}, err
	}
	p.Items = items[p.ID]
	if p.Items == nil {
		p.Items = []StatusPageItem{}
	}
	return p, nil
}

// StatusPageByID loads one status page by ID.
func (s *Store) StatusPageByID(ctx context.Context, id int64) (StatusPage, error) {
	return s.oneStatusPage(ctx, `id = ?`, id)
}

// StatusPageBySlug loads one status page by slug.
func (s *Store) StatusPageBySlug(ctx context.Context, slug string) (StatusPage, error) {
	return s.oneStatusPage(ctx, `slug = ?`, slug)
}

// StatusPageByDomain loads one status page by custom domain.
func (s *Store) StatusPageByDomain(ctx context.Context, domain string) (StatusPage, error) {
	if domain == "" {
		return StatusPage{}, ErrNotFound
	}
	return s.oneStatusPage(ctx, `domain = ?`, domain)
}

func (s *Store) statusPageItems(ctx context.Context, pageID int64) (map[int64][]StatusPageItem, error) {
	q := `SELECT id, page_id, position, section, display_name, COALESCE(check_id, 0), host_id, stack FROM status_page_items`
	var args []any
	if pageID > 0 {
		q += ` WHERE page_id = ?`
		args = append(args, pageID)
	}
	q += ` ORDER BY page_id, position, id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64][]StatusPageItem{}
	for rows.Next() {
		var it StatusPageItem
		if err := rows.Scan(&it.ID, &it.PageID, &it.Position, &it.Section, &it.DisplayName, &it.CheckID, &it.HostID, &it.Stack); err != nil {
			return nil, err
		}
		m[it.PageID] = append(m[it.PageID], it)
	}
	return m, rows.Err()
}

// SaveStatusPage inserts (ID 0) or updates a status page and replaces its items.
func (s *Store) SaveStatusPage(ctx context.Context, p StatusPage) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	pub, inc := 0, 0
	if p.Published {
		pub = 1
	}
	if p.ShowIncidents {
		inc = 1
	}
	var dom any
	if p.Domain != "" {
		dom = p.Domain
	}
	now := time.Now().Unix()
	if p.ID == 0 {
		res, err := tx.ExecContext(ctx, `INSERT INTO status_pages (slug, title, description, domain, published, show_incidents, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			p.Slug, p.Title, p.Description, dom, pub, inc, now, now)
		if err != nil {
			return 0, err
		}
		p.ID, _ = res.LastInsertId()
	} else {
		res, err := tx.ExecContext(ctx, `UPDATE status_pages SET slug=?, title=?, description=?, domain=?, published=?, show_incidents=?, updated_at=? WHERE id=?`,
			p.Slug, p.Title, p.Description, dom, pub, inc, now, p.ID)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return 0, ErrNotFound
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM status_page_items WHERE page_id = ?`, p.ID); err != nil {
		return 0, err
	}
	for i, it := range p.Items {
		var checkID any
		hostID, stack := it.HostID, it.Stack
		if it.CheckID > 0 {
			checkID = it.CheckID
			hostID, stack = "", ""
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO status_page_items (page_id, position, section, display_name, check_id, host_id, stack)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			p.ID, i, it.Section, it.DisplayName, checkID, hostID, stack); err != nil {
			return 0, err
		}
	}
	return p.ID, tx.Commit()
}

// DeleteStatusPage deletes a status page and its items.
func (s *Store) DeleteStatusPage(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM status_pages WHERE id = ?`, id)
	return err
}

// DailyCheckResults aggregates check_results per check and UTC day since the given timestamp
// in a single pass using the check_results(check_id, ts) index.
func (s *Store) DailyCheckResults(ctx context.Context, checkIDs []int64, since time.Time) ([]DailyCheckCounts, error) {
	if len(checkIDs) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(checkIDs)+1)
	for _, id := range checkIDs {
		args = append(args, id)
	}
	args = append(args, since.UnixMilli())
	q := `SELECT check_id, ts / 86400000 AS day,
		COALESCE(SUM(status = 'up'), 0),
		COALESCE(SUM(status = 'down'), 0),
		COALESCE(SUM(status = 'silenced'), 0),
		COALESCE(SUM(status = 'paused'), 0),
		COALESCE(SUM(status = 'maintenance'), 0),
		COALESCE(SUM(status = 'degraded'), 0)
		FROM check_results
		WHERE check_id IN (?` + strings.Repeat(",?", len(checkIDs)-1) + `) AND ts >= ?
		GROUP BY check_id, day`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DailyCheckCounts
	for rows.Next() {
		var d DailyCheckCounts
		if err := rows.Scan(&d.CheckID, &d.Day, &d.Up, &d.Down, &d.Silenced, &d.Paused, &d.Maintenance, &d.Degraded); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CheckUptimesSince returns (up, total) counts of ('up','down') results since t for each check ID.
func (s *Store) CheckUptimesSince(ctx context.Context, checkIDs []int64, since time.Time) (map[int64][2]int, error) {
	out := map[int64][2]int{}
	if len(checkIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(checkIDs)+1)
	for _, id := range checkIDs {
		args = append(args, id)
	}
	args = append(args, since.UnixMilli())
	q := `SELECT check_id, COALESCE(SUM(status = 'up'), 0), COUNT(*)
		FROM check_results
		WHERE check_id IN (?` + strings.Repeat(",?", len(checkIDs)-1) + `) AND ts >= ? AND status IN ('up', 'down', 'degraded')
		GROUP BY check_id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var up, total int
		if err := rows.Scan(&id, &up, &total); err != nil {
			return nil, err
		}
		out[id] = [2]int{up, total}
	}
	return out, rows.Err()
}

// IncidentSummariesSince returns stripped incidents (no Cause, no Context) for checkIDs
// that started or were active since t, newest first.
func (s *Store) IncidentSummariesSince(ctx context.Context, checkIDs []int64, since time.Time, limit int) ([]IncidentSummary, error) {
	if len(checkIDs) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(checkIDs)+2)
	for _, id := range checkIDs {
		args = append(args, id)
	}
	args = append(args, since.Unix(), limit)
	q := `SELECT id, check_id, started_at, COALESCE(ended_at, 0)
		FROM incidents
		WHERE check_id IN (?` + strings.Repeat(",?", len(checkIDs)-1) + `)
		  AND (started_at >= ? OR ended_at IS NULL OR ended_at >= ?)
		ORDER BY started_at DESC, id DESC LIMIT ?`
	// Adjust args for the two since parameters
	args = args[:len(checkIDs)]
	args = append(args, since.Unix(), since.Unix(), limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IncidentSummary
	for rows.Next() {
		var in IncidentSummary
		var started, ended int64
		if err := rows.Scan(&in.ID, &in.CheckID, &started, &ended); err != nil {
			return nil, err
		}
		in.StartedAt = time.Unix(started, 0).UTC()
		if ended > 0 {
			in.EndedAt = time.Unix(ended, 0).UTC()
		}
		out = append(out, in)
	}
	return out, rows.Err()
}
