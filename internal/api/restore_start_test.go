package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"moorage/internal/backupcrypt"
	"moorage/internal/store"
)

// makeBackup writes a format 3 .mbk with one user, a session, a local stack and a CA.
func makeBackup(t *testing.T, password string) string {
	t.Helper()
	ctx := context.Background()
	src := t.TempDir()
	db, err := store.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateInvitedUser(ctx, "admin", "admin", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, "hash", id, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	dbCopy := filepath.Join(t.TempDir(), "copy.db")
	if err := db.BackupTo(ctx, dbCopy); err != nil {
		t.Fatal(err)
	}
	db.Close()
	raw, _ := os.ReadFile(dbCopy)

	var buf bytes.Buffer
	enc, err := backupcrypt.NewWriter(&buf, password)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)
	m, _ := json.Marshal(backupManifest{Format: backupFormat, SchemaVersion: store.SchemaVersion()})
	for name, data := range map[string][]byte{
		"moorage.db":                      raw,
		"stacks/local/web/compose.yaml":   []byte("services:\n  web:\n    image: nginx\n    volumes:\n      - /old/stacks/web/data:/data\n"),
		"stacks/local/web/.env":           []byte("PASS=x\n"),
		"stacks/local/taken/compose.yaml": []byte("services: {}\n"),
		"ca/ca.crt":                       []byte("cert"),
		"ca/ca.key":                       []byte("key"),
		"manifest.json":                   m,
	} {
		if err := tarBytes(tw, name, data); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	enc.Close()
	p := filepath.Join(t.TempDir(), "b.mbk")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRestoreAtStart(t *testing.T) {
	ctx := context.Background()
	file := makeBackup(t, "correct horse")
	data, stacks := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(stacks, "taken"), 0o755) // an existing folder is never overwritten
	opt := RestoreOptions{File: file, Password: "wrong password", DataDir: data, StacksDir: stacks}

	// a wrong password stops the start and leaves no database behind
	if _, err := RestoreAtStart(ctx, opt, nil); err == nil || fileExists(filepath.Join(data, "moorage.db")) {
		t.Fatalf("wrong password: err %v, database written %v", err, fileExists(filepath.Join(data, "moorage.db")))
	}

	opt.Password = "correct horse"
	rep, err := RestoreAtStart(ctx, opt, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Stacks != 1 || len(rep.Skipped) != 1 || !rep.AgentCA || len(rep.OldPaths) != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if b, _ := os.ReadFile(filepath.Join(stacks, "web", ".env")); string(b) != "PASS=x\n" {
		t.Errorf(".env = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "ca", "ca.key")); string(b) != "key" {
		t.Errorf("ca.key = %q", b)
	}
	if !fileExists(filepath.Join(data, "restored-from.json")) {
		t.Error("restored-from.json missing")
	}
	db, err := store.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	u, err := db.UserByName(ctx, "admin")
	if err != nil {
		t.Fatalf("restored user: %v", err)
	}
	if ss, _ := db.UserSessions(ctx, u.ID); len(ss) != 0 {
		t.Errorf("sessions of the old installation survived: %d", len(ss))
	}
	db.Close()

	// the next start with the variable still set changes nothing
	if _, err := RestoreAtStart(ctx, opt, nil); !errors.Is(err, ErrRestoreSkipped) {
		t.Fatalf("second start: %v", err)
	}
}
