package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// InstalledAddon is a user-installed add-on tracked in SQLite.
type InstalledAddon struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"` // builtin | sidecar
	Category    string    `json:"category"`
	Description string    `json:"description"`
	Version     string    `json:"version"`
	HostID      string    `json:"hostId,omitempty"`
	StackName   string    `json:"stackName,omitempty"`
	RAM         string    `json:"ram,omitempty"`
	CPU         string    `json:"cpu,omitempty"`
	Enabled     bool      `json:"enabled"`
	InstalledAt time.Time `json:"installedAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// AddonEnabled reports whether an optional add-on is turned on (default false).
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

// SetAddon stores the enabled state of an optional add-on.
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

// InstalledAddons returns all installed add-ons sorted by name.
func (s *Store) InstalledAddons(ctx context.Context) ([]InstalledAddon, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, kind, category, description, version, host_id, stack_name, ram, cpu, enabled, installed_at, updated_at
		FROM installed_addons ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InstalledAddon
	for rows.Next() {
		var a InstalledAddon
		var enabled int
		var inst, upd int64
		if err := rows.Scan(&a.ID, &a.Name, &a.Kind, &a.Category, &a.Description, &a.Version, &a.HostID, &a.StackName, &a.RAM, &a.CPU, &enabled, &inst, &upd); err != nil {
			return nil, err
		}
		a.Enabled = enabled != 0
		a.InstalledAt = time.Unix(inst, 0)
		a.UpdatedAt = time.Unix(upd, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}

// InstalledAddonByID returns one installed add-on or ErrNotFound.
func (s *Store) InstalledAddonByID(ctx context.Context, id string) (InstalledAddon, error) {
	if s == nil || s.db == nil {
		return InstalledAddon{}, ErrNotFound
	}
	var a InstalledAddon
	var enabled int
	var inst, upd int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, kind, category, description, version, host_id, stack_name, ram, cpu, enabled, installed_at, updated_at
		FROM installed_addons WHERE id = ?`, id).Scan(
		&a.ID, &a.Name, &a.Kind, &a.Category, &a.Description, &a.Version, &a.HostID, &a.StackName, &a.RAM, &a.CPU, &enabled, &inst, &upd,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return InstalledAddon{}, ErrNotFound
	}
	if err != nil {
		return InstalledAddon{}, err
	}
	a.Enabled = enabled != 0
	a.InstalledAt = time.Unix(inst, 0)
	a.UpdatedAt = time.Unix(upd, 0)
	return a, nil
}

// UpsertInstalledAddon saves or updates an installed add-on record.
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
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO installed_addons (id, name, kind, category, description, version, host_id, stack_name, ram, cpu, enabled, installed_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			kind = excluded.kind,
			category = excluded.category,
			description = excluded.description,
			version = excluded.version,
			host_id = excluded.host_id,
			stack_name = excluded.stack_name,
			ram = excluded.ram,
			cpu = excluded.cpu,
			enabled = excluded.enabled,
			updated_at = excluded.updated_at`,
		a.ID, a.Name, a.Kind, a.Category, a.Description, a.Version, a.HostID, a.StackName, a.RAM, a.CPU, en, inst, now,
	)
	return err
}

// DeleteInstalledAddon removes an add-on record.
func (s *Store) DeleteInstalledAddon(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM installed_addons WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
