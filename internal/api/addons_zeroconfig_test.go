package api

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"moorage/internal/config"
	"moorage/internal/store"
)

const resticFragment = `services:
  restic:
    image: restic/restic:latest
    environment:
      RESTIC_REPOSITORY: /backup/repo
      RESTIC_PASSWORD: ${RESTIC_PASSWORD}
      TZ: ${TZ:-UTC}
    volumes:
      - ${MOORAGE_STACKS_DIR:-/opt/stacks}:/backup/stacks:ro
      - ${MOORAGE_BACKUPS_DIR:-restic-repo}:/backup/repo
volumes:
  restic-repo:
`

func TestWithAddonEnvDefaults(t *testing.T) {
	defaults := map[string]string{
		"MOORAGE_STACKS_DIR":  "/opt/stacks",
		"MOORAGE_BACKUPS_DIR": "/srv/moorage/backups",
		"HOST_IP":             "192.168.1.10", // not used by the fragment: not added
		"TZ":                  "Europe/Prague",
	}
	prev := "TZ=America/New_York\nOTHER=1\n"
	env, res := withAddonEnvDefaults(prev, resticFragment, defaults)
	m := parseEnvLinesMap(env)
	if m["TZ"] != "America/New_York" || m["OTHER"] != "1" {
		t.Fatalf("user values must stay: %q", env)
	}
	if m["MOORAGE_STACKS_DIR"] != "/opt/stacks" || m["MOORAGE_BACKUPS_DIR"] != "/srv/moorage/backups" {
		t.Fatalf("defaults missing: %q", env)
	}
	if _, ok := m["HOST_IP"]; ok {
		t.Fatal("unused defaults must not be added")
	}
	if len(m["RESTIC_PASSWORD"]) < 24 || len(res.Generated) != 1 || res.Generated[0] != "RESTIC_PASSWORD" {
		t.Fatalf("password: %q %+v", m["RESTIC_PASSWORD"], res)
	}
	if !strings.Contains(env, "keep a copy") {
		t.Fatal("generated secret needs the backup note")
	}

	// A second install keeps everything: nothing new to add, the password isn't rotated.
	env2, res2 := withAddonEnvDefaults(env, resticFragment, defaults)
	if env2 != env || len(res2.Added) != 0 {
		t.Fatalf("second run changed .env: %+v", res2)
	}

	// Named-volume fallback: an empty MOORAGE_BACKUPS_DIR is still written, so the
	// template's ${MOORAGE_BACKUPS_DIR:-restic-repo} uses the volume.
	env3, _ := withAddonEnvDefaults("", resticFragment, map[string]string{"MOORAGE_BACKUPS_DIR": ""})
	if !strings.Contains(env3, "MOORAGE_BACKUPS_DIR=\n") {
		t.Fatalf("empty default: %q", env3)
	}
}

func TestEnsureAddonsAutoPolicy(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := New(config.Config{}, nil, db, nil, nil, nil, slog.Default(), "test")

	srv.ensureAddonsAutoPolicy(ctx, "local")
	p, _ := db.StackUpdatePolicy(ctx, "local", addonsStackName)
	if p.Policy != store.PolicyAuto {
		t.Fatalf("first creation: %q", p.Policy)
	}
	// The user switches to notify; recreating the stack later doesn't override it.
	p.Policy = store.PolicyNotify
	if err := db.SaveStackUpdatePolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	srv.ensureAddonsAutoPolicy(ctx, "local")
	p, _ = db.StackUpdatePolicy(ctx, "local", addonsStackName)
	if p.Policy != store.PolicyNotify {
		t.Fatalf("user choice overridden: %q", p.Policy)
	}
}
