package api

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"moorage/internal/auth"
	"moorage/internal/config"
	"moorage/internal/store"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// M18I: refused sign-ins are logged for CrowdSec/fail2ban, and secrets never reach the
// log – not passwords, API keys, storage secrets or query strings.
func TestLogNeverContainsSecrets(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	createTestUserSession(t, db, "admin", "admin", "tok-admin")
	var buf syncBuf
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a, err := auth.New(db, log)
	if err != nil {
		t.Fatal(err)
	}
	e := &auditEnv{db: db, srv: New(config.Config{}, a, db, nil, nil, nil, log, "test")}
	e.h = e.srv.Handler()

	secrets := []string{"Pa55word-SECRET-1", "sk-ant-SECRET-KEY-2", "S3-SECRET-ACCESS-3", "QUERY-SECRET-4", "typed-password-as-name"}
	e.req("POST", "/api/login", `{"username":"admin","password":"Pa55word-SECRET-1","code":"123456"}`, "")
	e.req("POST", "/api/login", `{"username":"typed-password-as-name","password":"x","code":"1"}`, "")
	e.req("PUT", "/api/addons/assistant/settings", `{"provider":"anthropic","model":"claude-opus-5-5","levels":{"stack":"propose","compose":"propose","generate":"propose"},"data":{"logs":"ask","audit":"ask"},"apiKey":"sk-ant-SECRET-KEY-2"}`, "tok-admin")
	e.req("PUT", "/api/addons/status-snapshot/config", `{"provider":"s3","s3Endpoint":"https://s3.example.com","s3Bucket":"b","s3AccessKey":"AK","s3SecretKey":"S3-SECRET-ACCESS-3","intervalMinutes":5,"staleMinutes":15}`, "tok-admin")
	e.req("GET", "/api/session?token=QUERY-SECRET-4", "", "tok-admin")

	out := buf.String()
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("secret %q in the log:\n%s", s, out)
		}
	}
	if !strings.Contains(out, `msg="sign-in failed"`) || !strings.Contains(out, "user=admin") {
		t.Errorf("refused sign-in not logged with the existing account:\n%s", out)
	}
	if !strings.Contains(out, `msg=request`) {
		t.Errorf("debug access log missing:\n%s", out)
	}
}
