package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// InstalledAddon is a user-installed add-on tracked in SQLite (keyed by id + host_id).
type InstalledAddon struct {
	ID              string    `json:"id"`
	HostID          string    `json:"hostId,omitempty"`
	Name            string    `json:"name"`
	Kind            string    `json:"kind"` // builtin | sidecar
	Category        string    `json:"category"`
	Description     string    `json:"description"`
	Version         string    `json:"version"`
	StackName       string    `json:"stackName,omitempty"`
	RAM             string    `json:"ram,omitempty"`
	CPU             string    `json:"cpu,omitempty"`
	Enabled         bool      `json:"enabled"`
	InstalledAt     time.Time `json:"installedAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
	Custom          bool      `json:"custom,omitempty"`
	ComposeFragment string    `json:"composeFragment,omitempty"`
	EnvKeys         []string  `json:"envKeys,omitempty"`
	Source          string    `json:"source,omitempty"`
}

// ErrAddonSourceExists is returned when adding a duplicate add-on catalog URL.
var ErrAddonSourceExists = errors.New("this add-on source URL is already added")

// AddonSource is a custom HTTPS add-on catalog index configured by the admin.
type AddonSource struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	URL          string    `json:"url"`
	AllowPrivate bool      `json:"allowPrivate"`
	Enabled      bool      `json:"enabled"`
	Cache        string    `json:"-"`
	FetchedAt    time.Time `json:"fetchedAt,omitzero"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

// AddonEnabled reports whether an optional built-in add-on is turned on (default false).
func (s *Store) AddonEnabled(ctx context.Context, id string) (bool, error) {
	if s == nil || s.db == nil {
		return false, nil
	}
	var enabled int
	err := s.db.QueryRowContext(ctx, `SELECT enabled FROM addons WHERE id = ?`, id).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return enabled != 0, nil
}

// SetAddon stores the enabled state of an optional built-in add-on.
func (s *Store) SetAddon(ctx context.Context, id string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO addons (id, enabled, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET enabled = excluded.enabled, updated_at = excluded.updated_at`,
		id, v, time.Now().Unix(),
	)
	return err
}

func scanInstalledAddon(scan func(dest ...any) error) (InstalledAddon, error) {
	var a InstalledAddon
	var enabled, custom int
	var inst, upd int64
	var rawEnvKeys string
	if err := scan(
		&a.ID, &a.HostID, &a.Name, &a.Kind, &a.Category, &a.Description, &a.Version,
		&a.StackName, &a.RAM, &a.CPU, &enabled, &inst, &upd,
		&custom, &a.ComposeFragment, &rawEnvKeys, &a.Source,
	); err != nil {
		return InstalledAddon{}, err
	}
	a.Enabled = enabled != 0
	a.Custom = custom != 0
	a.InstalledAt = time.Unix(inst, 0)
	a.UpdatedAt = time.Unix(upd, 0)
	if strings.TrimSpace(rawEnvKeys) != "" {
		if strings.HasPrefix(strings.TrimSpace(rawEnvKeys), "[") {
			_ = json.Unmarshal([]byte(rawEnvKeys), &a.EnvKeys)
		} else {
			for _, k := range strings.Split(rawEnvKeys, ",") {
				if k = strings.TrimSpace(k); k != "" {
					a.EnvKeys = append(a.EnvKeys, k)
				}
			}
		}
	}
	return a, nil
}

const selectInstalledAddonCols = `
	SELECT id, host_id, name, kind, category, description, version,
	       stack_name, ram, cpu, enabled, installed_at, updated_at,
	       custom, compose_fragment, env_keys, source
	FROM installed_addons`

// InstalledAddons returns all installed add-ons sorted by name and host_id.
func (s *Store) InstalledAddons(ctx context.Context) ([]InstalledAddon, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, selectInstalledAddonCols+` ORDER BY name, host_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InstalledAddon
	for rows.Next() {
		a, err := scanInstalledAddon(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// InstalledAddonByID returns one installed add-on matching id (preferring hostID when non-empty).
func (s *Store) InstalledAddonByID(ctx context.Context, id string) (InstalledAddon, error) {
	if s == nil || s.db == nil {
		return InstalledAddon{}, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, selectInstalledAddonCols+` WHERE id = ? ORDER BY host_id LIMIT 1`, id)
	a, err := scanInstalledAddon(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return InstalledAddon{}, ErrNotFound
	}
	return a, err
}

// InstalledAddonByHost returns the installed add-on for a specific (id, host_id) pair.
func (s *Store) InstalledAddonByHost(ctx context.Context, id, hostID string) (InstalledAddon, error) {
	if s == nil || s.db == nil {
		return InstalledAddon{}, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, selectInstalledAddonCols+` WHERE id = ? AND host_id = ?`, id, hostID)
	a, err := scanInstalledAddon(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return InstalledAddon{}, ErrNotFound
	}
	return a, err
}

// InstalledAddonsByID returns all host installations of an add-on ID.
func (s *Store) InstalledAddonsByID(ctx context.Context, id string) ([]InstalledAddon, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, selectInstalledAddonCols+` WHERE id = ? ORDER BY host_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InstalledAddon
	for rows.Next() {
		a, err := scanInstalledAddon(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpsertInstalledAddon saves or updates an installed add-on record keyed by (id, host_id).
func (s *Store) UpsertInstalledAddon(ctx context.Context, a InstalledAddon) error {
	now := time.Now().Unix()
	inst := a.InstalledAt.Unix()
	if a.InstalledAt.IsZero() {
		inst = now
	}
	en := 0
	if a.Enabled {
		en = 1
	}
	cust := 0
	if a.Custom {
		cust = 1
	}
	envKeysJSON := ""
	if len(a.EnvKeys) > 0 {
		if b, err := json.Marshal(a.EnvKeys); err == nil {
			envKeysJSON = string(b)
		}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO installed_addons (
			id, host_id, name, kind, category, description, version,
			stack_name, ram, cpu, enabled, installed_at, updated_at,
			custom, compose_fragment, env_keys, source
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id, host_id) DO UPDATE SET
			name = excluded.name,
			kind = excluded.kind,
			category = excluded.category,
			description = excluded.description,
			version = excluded.version,
			stack_name = excluded.stack_name,
			ram = excluded.ram,
			cpu = excluded.cpu,
			enabled = excluded.enabled,
			updated_at = excluded.updated_at,
			custom = excluded.custom,
			compose_fragment = excluded.compose_fragment,
			env_keys = excluded.env_keys,
			source = excluded.source`,
		a.ID, a.HostID, a.Name, a.Kind, a.Category, a.Description, a.Version,
		a.StackName, a.RAM, a.CPU, en, inst, now,
		cust, a.ComposeFragment, envKeysJSON, a.Source,
	)
	return err
}

// DeleteInstalledAddon removes an add-on record for (id, hostID), or all hosts when hostID == "".
func (s *Store) DeleteInstalledAddon(ctx context.Context, id string, hostID ...string) error {
	var (
		res sql.Result
		err error
	)
	if len(hostID) > 0 && hostID[0] != "" {
		res, err = s.db.ExecContext(ctx, `DELETE FROM installed_addons WHERE id = ? AND host_id = ?`, id, hostID[0])
	} else {
		res, err = s.db.ExecContext(ctx, `DELETE FROM installed_addons WHERE id = ?`, id)
	}
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- Custom HTTPS add-on sources (addon_sources) ----------------------------------------

func scanAddonSource(scan func(dest ...any) error) (AddonSource, error) {
	var src AddonSource
	var allowPriv, enabled int
	var fetchedAt, createdAt int64
	if err := scan(&src.ID, &src.Name, &src.URL, &allowPriv, &enabled, &src.Cache, &fetchedAt, &src.Error, &createdAt); err != nil {
		return AddonSource{}, err
	}
	src.AllowPrivate = allowPriv != 0
	src.Enabled = enabled != 0
	if fetchedAt > 0 {
		src.FetchedAt = time.Unix(fetchedAt, 0)
	}
	src.CreatedAt = time.Unix(createdAt, 0)
	return src, nil
}

// AddonSources lists all custom add-on catalog sources ordered by name.
func (s *Store) AddonSources(ctx context.Context) ([]AddonSource, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, url, allow_private, enabled, cache, fetched_at, error, created_at
		FROM addon_sources ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AddonSource
	for rows.Next() {
		src, err := scanAddonSource(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// AddonSourceByID returns one custom add-on source by ID.
func (s *Store) AddonSourceByID(ctx context.Context, id int64) (AddonSource, error) {
	if s == nil || s.db == nil {
		return AddonSource{}, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, url, allow_private, enabled, cache, fetched_at, error, created_at
		FROM addon_sources WHERE id = ?`, id)
	src, err := scanAddonSource(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return AddonSource{}, ErrNotFound
	}
	return src, err
}

// AddAddonSource inserts a new custom add-on source.
func (s *Store) AddAddonSource(ctx context.Context, name, rawURL string, allowPrivate bool, cache string) (AddonSource, error) {
	now := time.Now().Unix()
	ap := 0
	if allowPrivate {
		ap = 1
	}
	fetched := int64(0)
	if cache != "" {
		fetched = now
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO addon_sources (name, url, allow_private, enabled, cache, fetched_at, error, created_at)
		VALUES (?, ?, ?, 1, ?, ?, '', ?)`,
		name, rawURL, ap, cache, fetched, now,
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return AddonSource{}, ErrAddonSourceExists
		}
		return AddonSource{}, err
	}
	id, _ := res.LastInsertId()
	return s.AddonSourceByID(ctx, id)
}

// UpdateAddonSource updates name, url, allow_private, and enabled of an add-on source.
func (s *Store) UpdateAddonSource(ctx context.Context, id int64, name, rawURL string, allowPrivate, enabled bool) (AddonSource, error) {
	ap, en := 0, 0
	if allowPrivate {
		ap = 1
	}
	if enabled {
		en = 1
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE addon_sources
		SET name = ?, url = ?, allow_private = ?, enabled = ?
		WHERE id = ?`,
		name, rawURL, ap, en, id,
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return AddonSource{}, ErrAddonSourceExists
		}
		return AddonSource{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return AddonSource{}, ErrNotFound
	}
	return s.AddonSourceByID(ctx, id)
}

// UpdateAddonSourceFetch records the latest index.json fetch result (preserving previous cache on error).
func (s *Store) UpdateAddonSourceFetch(ctx context.Context, id int64, cache string, fetchErr string) error {
	if cache != "" {
		_, err := s.db.ExecContext(ctx, `
			UPDATE addon_sources SET cache = ?, fetched_at = ?, error = ? WHERE id = ?`,
			cache, time.Now().Unix(), fetchErr, id,
		)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE addon_sources SET error = ? WHERE id = ?`, fetchErr, id)
	return err
}

// DeleteAddonSource deletes a custom add-on source.
func (s *Store) DeleteAddonSource(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM addon_sources WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
