package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"moorage/internal/config"
)

// newTestEngine builds an Engine without a Docker connection, enough for file and validation logic.
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	return &Engine{
		cfg:  config.Config{StacksDir: t.TempDir(), PUID: -1, PGID: -1},
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		busy: map[string]bool{},
		ops:  newOpRegistry(),
	}
}

func TestStackNameValidation(t *testing.T) {
	e := newTestEngine(t)
	for _, bad := range []string{"", "../etc", "a/b", "Upper", "-x", strings.Repeat("a", 64), "x y"} {
		if _, err := e.stackDir(bad); !errors.Is(err, ErrInvalidName) {
			t.Errorf("%q should be rejected", bad)
		}
	}
	for _, ok := range []string{"a", "web-1", "my_app", "0x"} {
		if _, err := e.stackDir(ok); err != nil {
			t.Errorf("%q should be accepted: %v", ok, err)
		}
	}
}

func TestValidateCompose(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	if err := e.ValidateCompose(ctx, "ok", "services:\n  web:\n    image: nginx:${TAG}\n", "TAG=1.27\n"); err != nil {
		t.Fatalf("valid compose rejected: %v", err)
	}
	err := e.ValidateCompose(ctx, "bad", "services:\n  job:\n    imagez: busybox\n", "")
	var ve ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError, got %v", err)
	}
	if strings.Contains(ve.Msg, e.cfg.StacksDir) || !strings.Contains(ve.Msg, "services.job.imagez") {
		t.Fatalf("unfriendly message: %q", ve.Msg)
	}
	if err := e.ValidateCompose(ctx, "empty", "  \n", ""); !errors.As(err, &ve) {
		t.Fatal("empty compose must fail")
	}
}

func TestSaveStackCreateAndUpdate(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	c := "services:\n  web:\n    image: nginx\n"
	if err := e.SaveStack(ctx, "web", c, "A=1\n", "ann", true); err != nil {
		t.Fatal(err)
	}
	if err := e.SaveStack(ctx, "web", c, "", "ann", true); !errors.Is(err, ErrExists) {
		t.Fatalf("second create: want ErrExists, got %v", err)
	}
	if err := e.SaveStack(ctx, "missing", c, "", "ann", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of missing stack: want ErrNotFound, got %v", err)
	}
	// clearing .env removes the file
	if err := e.SaveStack(ctx, "web", c, "", "ann", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.StacksDir, "web", ".env")); !os.IsNotExist(err) {
		t.Fatal(".env should be removed")
	}
	b, err := os.ReadFile(filepath.Join(e.cfg.StacksDir, "web", "compose.yaml"))
	if err != nil || string(b) != c {
		t.Fatalf("compose not written: %q %v", b, err)
	}
}

func TestStackStatus(t *testing.T) {
	three, zero := 3, 0
	cases := []struct {
		cs   []Container
		want string
	}{
		{nil, StatusInactive},
		{[]Container{{State: "running"}, {State: "running"}}, StatusUp},
		{[]Container{{State: "running", Health: "unhealthy"}}, StatusDegraded},
		{[]Container{{State: "running"}, {State: "exited", ExitCode: &three}}, StatusDegraded},
		{[]Container{{State: "exited", ExitCode: &three}}, StatusDown},
		{[]Container{{State: "exited", ExitCode: &zero}}, StatusStopped},
		{[]Container{{State: "restarting"}}, StatusDown},
	}
	for i, c := range cases {
		if got, _ := stackStatus(c.cs); got != c.want {
			t.Errorf("case %d: got %s want %s", i, got, c.want)
		}
	}
}

func TestLogFilter(t *testing.T) {
	if _, err := (LogFilter{}).Compile(); err == nil {
		t.Fatal("empty pattern accepted")
	}
	if _, err := (LogFilter{Pattern: strings.Repeat("a", MaxPatternLen+1)}).Compile(); err == nil {
		t.Fatal("overlong pattern accepted")
	}
	if _, err := (LogFilter{Pattern: "[unclosed", Regex: true}).Compile(); err == nil {
		t.Fatal("invalid regex accepted")
	}

	subInsensitive, err := (LogFilter{Pattern: "fatal"}).Compile()
	if err != nil || !subInsensitive("FATAL: boom") || !subInsensitive("fatal error") || subInsensitive("all good") {
		t.Fatalf("case-insensitive substring failed: %v", err)
	}
	subSensitive, err := (LogFilter{Pattern: "FATAL", CaseSensitive: true}).Compile()
	if err != nil || !subSensitive("FATAL: boom") || subSensitive("fatal error") {
		t.Fatalf("case-sensitive substring failed: %v", err)
	}
	reInsensitive, err := (LogFilter{Pattern: `err(or)?:\s+\d+`, Regex: true}).Compile()
	if err != nil || !reInsensitive("ERROR: 502") || !reInsensitive("err: 42") || reInsensitive("ERROR: none") {
		t.Fatalf("case-insensitive regex failed: %v", err)
	}
	reSensitive, err := (LogFilter{Pattern: `^FATAL`, Regex: true, CaseSensitive: true}).Compile()
	if err != nil || !reSensitive("FATAL: disk") || reSensitive("fatal: disk") || reSensitive("not FATAL") {
		t.Fatalf("case-sensitive regex failed: %v", err)
	}
}
