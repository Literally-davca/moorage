package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
)

// SchemaVersion is the schema this build migrates to.
func SchemaVersion() int { return len(migrations) }

// BackupInfo summarizes a database file found in a backup, for the import preview.
type BackupInfo struct {
	SchemaVersion int `json:"schemaVersion"`
	Users         int `json:"users"`
	Agents        int `json:"agents"`
	Checks        int `json:"checks"`
	Channels      int `json:"channels"`
	StatusPages   int `json:"statusPages"`
}

// ErrBackupNewer means the backup comes from a newer Moorage: this build can't migrate it.
var ErrBackupNewer = errors.New("the backup comes from a newer Moorage version; update Moorage first")

// InspectBackup opens a database file read-only and checks it is a Moorage database this
// build can use (integrity, schema version not newer than ours).
func InspectBackup(ctx context.Context, path string) (BackupInfo, error) {
	var info BackupInfo
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return info, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var ok string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&ok); err != nil || ok != "ok" {
		return info, errors.New("the database in the backup is damaged or not SQLite")
	}
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(v),0) FROM schema_version`).Scan(&info.SchemaVersion); err != nil {
		return info, errors.New("the database in the backup is not a Moorage database")
	}
	if info.SchemaVersion < 1 {
		return info, errors.New("the database in the backup is empty")
	}
	if info.SchemaVersion > SchemaVersion() {
		return info, ErrBackupNewer
	}
	for _, c := range []struct {
		table string
		dst   *int
	}{
		{"users", &info.Users}, {"agents", &info.Agents}, {"checks", &info.Checks},
		{"channels", &info.Channels}, {"status_pages", &info.StatusPages},
	} {
		// Tables added by later migrations may be missing from an older backup.
		if err := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s`, c.table)).Scan(c.dst); err != nil {
			*c.dst = 0
		}
	}
	if info.Users == 0 {
		return info, errors.New("the database in the backup has no users")
	}
	return info, nil
}
