package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"moorage/internal/auth"
	"moorage/internal/config"
	"moorage/internal/store"
)

type auditEnv struct {
	db  *store.Store
	srv *Server
	h   http.Handler
}

func newAuditEnv(t *testing.T) *auditEnv {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	createTestUserSession(t, db, "admin", "admin", "tok-admin")
	createTestUserSession(t, db, "viewer", "viewer", "tok-viewer")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := auth.New(db, log)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(config.Config{}, a, db, nil, nil, nil, log, "test")
	return &auditEnv{db: db, srv: srv, h: srv.Handler()}
}

func (e *auditEnv) req(method, path, body, token string) *httptest.ResponseRecorder {
	hdr := map[string]string{csrfHeader: "1", "Content-Type": "application/json"}
	if token != "" {
		hdr["Cookie"] = cookieName + "=" + token
	}
	return do(e.h, method, path, body, hdr)
}

func auditRows(t *testing.T, db *store.Store) []store.AuditEntry {
	t.Helper()
	rows, err := db.AuditEntries(context.Background(), store.AuditFilter{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// A mutating route missing from auditActions fails here, so no new route skips the audit;
// a stale entry for a removed route fails too.
func TestAuditEveryMutatingRouteMapped(t *testing.T) {
	env := newAuditEnv(t)
	registered := map[string]bool{}
	for _, p := range env.srv.routes {
		registered[p] = true
		method, path, ok := strings.Cut(p, " ")
		if !ok || !isMutating(method) || !strings.HasPrefix(path, "/api/") {
			continue
		}
		if _, mapped := auditActions[p]; !mapped {
			t.Errorf("mutating route %q has no audit action in auditActions", p)
		}
	}
	if len(registered) < 40 {
		t.Fatalf("expected the route list to be captured, got %d patterns", len(registered))
	}
	for p := range auditActions {
		if !registered[p] {
			t.Errorf("auditActions has %q, which is not a registered route", p)
		}
	}
}

// The middleware against the real route patterns, with stub handlers for each outcome:
// 2xx -> ok, refused 4xx -> nothing, 5xx -> failed. Bodies never reach the log.
func TestAuditMiddlewareOutcomesAndNoBody(t *testing.T) {
	env := newAuditEnv(t)
	s := env.srv
	mux := &routeMux{ServeMux: http.NewServeMux()}
	mux.Handle("POST /api/hosts/{host}/stacks/{name}/actions/{action}", s.admin(func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("name") {
		case "busy":
			writeErr(w, http.StatusConflict, "busy")
		case "invalid":
			writeErr(w, http.StatusUnprocessableEntity, "bad compose")
		case "boom":
			writeErr(w, http.StatusBadGateway, "docker error")
		default:
			writeJSON(w, http.StatusAccepted, map[string]string{"id": "op1"})
		}
	}))
	mux.Handle("PUT /api/hosts/{host}/stacks/{name}", s.admin(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) //nolint:errcheck
		w.WriteHeader(http.StatusNoContent)
	}))
	h := s.secure(s.withAudit(mux))
	send := func(method, path, body, token string) int {
		hdr := map[string]string{csrfHeader: "1", "Cookie": cookieName + "=" + token}
		return do(h, method, path, body, hdr).Code
	}

	if c := send("POST", "/api/hosts/local/stacks/web/actions/stop", "", "tok-admin"); c != http.StatusAccepted {
		t.Fatalf("stop: %d", c)
	}
	send("POST", "/api/hosts/local/stacks/busy/actions/up", "", "tok-admin")
	send("POST", "/api/hosts/local/stacks/invalid/actions/up", "", "tok-admin")
	send("POST", "/api/hosts/local/stacks/web/actions/up", "", "tok-viewer") // 403
	send("POST", "/api/hosts/local/stacks/web/actions/up", "", "forged")     // 401
	send("POST", "/api/hosts/agent1/stacks/boom/actions/pull", "", "tok-admin")
	secret := "SUPER_SECRET_DB_PASSWORD_42"
	body := `{"compose":"services:\n  db:\n    environment:\n      PASSWORD: ` + secret + `","env":"TOKEN=` + secret + `"}`
	if c := send("PUT", "/api/hosts/local/stacks/web?token="+secret, body, "tok-admin"); c != http.StatusNoContent {
		t.Fatalf("save: %d", c)
	}

	rows := auditRows(t, env.db)
	if len(rows) != 3 {
		t.Fatalf("want 3 rows (stop ok, pull failed, save ok), got %+v", rows)
	}
	save, pull, stop := rows[0], rows[1], rows[2]
	if stop.Action != "stack.stop" || stop.Outcome != store.AuditOK || stop.Actor != "admin" || stop.HostID != "local" || stop.Stack != "web" || stop.IP == "" {
		t.Errorf("stop row: %+v", stop)
	}
	if pull.Action != "stack.pull" || pull.Outcome != store.AuditFailed || pull.HostID != "agent1" || pull.Stack != "boom" {
		t.Errorf("pull row: %+v", pull)
	}
	if save.Action != "stack.save" || save.Outcome != store.AuditOK || save.Stack != "web" || save.Target != "" {
		t.Errorf("save row: %+v", save)
	}
	b, _ := json.Marshal(rows)
	if strings.Contains(string(b), secret) {
		t.Fatalf("request body or query leaked into the audit log: %s", b)
	}
}

// Real handlers end to end: channel create/update/delete with a secret token, a validation
// refusal, and the {id} target.
func TestAuditRealRoutes(t *testing.T) {
	env := newAuditEnv(t)
	secret := "WEBHOOK_BEARER_SECRET_77"
	w := env.req("POST", "/api/channels", `{"name":"hook","kind":"webhook","enabled":true,"config":{"url":"https://hooks.example.com/abc","token":"`+secret+`"}}`, "tok-admin")
	if w.Code != http.StatusOK {
		t.Fatalf("create channel: %d %s", w.Code, w.Body)
	}
	var created struct{ ID int64 }
	json.Unmarshal(w.Body.Bytes(), &created) //nolint:errcheck
	if w := env.req("POST", "/api/channels", `{"name":"","kind":"webhook"}`, "tok-admin"); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid channel: %d", w.Code)
	}
	if w := env.req("DELETE", "/api/channels/"+itoa(created.ID), "", "tok-admin"); w.Code != http.StatusNoContent {
		t.Fatalf("delete channel: %d", w.Code)
	}
	if w := env.req("POST", "/api/convert", `{"command":"docker run nginx"}`, "tok-admin"); w.Code != http.StatusOK {
		t.Fatalf("convert: %d", w.Code)
	}

	rows := auditRows(t, env.db)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (create, delete), got %+v", rows)
	}
	if rows[1].Action != "channel.create" || rows[1].Target != "" || rows[1].Outcome != store.AuditOK {
		t.Errorf("create row: %+v", rows[1])
	}
	if rows[0].Action != "channel.delete" || rows[0].Target != itoa(created.ID) {
		t.Errorf("delete row: %+v", rows[0])
	}
	b, _ := json.Marshal(rows)
	if strings.Contains(string(b), secret) || strings.Contains(string(b), "hooks.example.com") {
		t.Fatalf("channel config leaked into the audit log: %s", b)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func setupTokenFrom(t *testing.T, log string) string {
	t.Helper()
	m := regexp.MustCompile(`setup_token=([0-9a-f]+)`).FindStringSubmatch(log)
	if m == nil {
		t.Fatalf("no setup token in log: %s", log)
	}
	return m[1]
}

// totpNow computes an RFC 6238 code (SHA-1, 6 digits, 30 s), like an authenticator app.
func totpNow(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secret))
	if err != nil {
		t.Fatal(err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(at.Unix()/30))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", bin%1_000_000)
}

// Sign-in rows: ok, wrong credentials (with the username tried and the IP), logout.
// The password and the TOTP code never land in the log.
func TestAuditLoginLogout(t *testing.T) {
	env := newAuditEnv(t)
	password := "correct-horse-battery-staple"
	// "admin" exists from newAuditEnv with a dummy hash; a fresh user is enrolled through auth.
	if w := env.req("POST", "/api/login", `{"username":"admin","password":"`+password+`","code":"123456"}`, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong login: %d %s", w.Code, w.Body)
	}
	if w := env.req("POST", "/api/login", `{"username":"nobody-here","password":"x","code":"1"}`, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown user: %d", w.Code)
	}
	if w := env.req("POST", "/api/login", `not json`, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("bad body: %d", w.Code)
	}
	if w := env.req("POST", "/api/logout", "", "tok-viewer"); w.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", w.Code)
	}
	if w := env.req("POST", "/api/logout", "", "tok-viewer"); w.Code != http.StatusNoContent { // stale cookie now
		t.Fatalf("second logout: %d", w.Code)
	}

	rows := auditRows(t, env.db)
	if len(rows) != 3 {
		t.Fatalf("want 2 failed logins + 1 logout, got %+v", rows)
	}
	if rows[2].Action != auditLoginFailed || rows[2].Actor != "admin" || rows[2].Outcome != store.AuditFailed || rows[2].IP == "" {
		t.Errorf("failed login row: %+v", rows[2])
	}
	if rows[1].Action != auditLoginFailed || rows[1].Actor != "nobody-here" {
		t.Errorf("unknown-user row: %+v", rows[1])
	}
	if rows[0].Action != auditLogout || rows[0].Actor != "viewer" || rows[0].Outcome != store.AuditOK || rows[0].IP == "" {
		t.Errorf("logout row: %+v", rows[0])
	}
	b, _ := json.Marshal(rows)
	if strings.Contains(string(b), password) || strings.Contains(string(b), "123456") {
		t.Fatalf("credentials leaked into the audit log: %s", b)
	}
}

// A successful sign-in through setup and login writes auth.setup and auth.login.
func TestAuditSetupAndLoginOK(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	logBuf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logBuf, nil))
	a, err := auth.New(db, log)
	if err != nil {
		t.Fatal(err)
	}
	h := New(config.Config{}, a, db, nil, nil, nil, log, "test").Handler()
	token := setupTokenFrom(t, logBuf.String())
	csrf := map[string]string{csrfHeader: "1"}
	password := "a-long-enough-password-1"
	w := do(h, "POST", "/api/setup/start", `{"token":"`+token+`","username":"boss","password":"`+password+`"}`, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("setup start: %d %s", w.Code, w.Body)
	}
	var st struct{ Secret string }
	json.Unmarshal(w.Body.Bytes(), &st) //nolint:errcheck
	code := totpNow(t, st.Secret, time.Now())
	if w := do(h, "POST", "/api/setup/finish", `{"token":"`+token+`","username":"boss","code":"`+code+`"}`, csrf); w.Code != http.StatusOK {
		t.Fatalf("setup finish: %d %s", w.Code, w.Body)
	}
	// The setup code was used for this time step; sign in with the next one.
	code2 := totpNow(t, st.Secret, time.Now().Add(30*time.Second))
	if w := do(h, "POST", "/api/login", `{"username":"boss","password":"`+password+`","code":"`+code2+`"}`, csrf); w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	rows := auditRows(t, db)
	if len(rows) != 2 || rows[1].Action != auditSetup || rows[0].Action != auditLogin || rows[0].Actor != "boss" || rows[0].IP == "" {
		t.Fatalf("want setup + login rows, got %+v", rows)
	}
}

// Viewers get 403; filters and paging work; the host name is resolved.
func TestAuditListAPI(t *testing.T) {
	env := newAuditEnv(t)
	ctx := context.Background()
	add := func(actor, action, host, stack string) {
		if err := env.db.Audit(ctx, store.AuditEntry{Actor: actor, IP: "10.0.0.1", Action: action, HostID: host, Stack: stack}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		add("admin", "stack.up", "local", "web")
	}
	add("ops", "stack.stop", "local", "db")
	add("admin", "check.save", "", "")
	add("system", "image.auto_update", "local", "web")

	if w := env.req("GET", "/api/audit", "", "tok-viewer"); w.Code != http.StatusForbidden {
		t.Fatalf("viewer: want 403, got %d", w.Code)
	}
	if w := env.req("GET", "/api/audit", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: want 401, got %d", w.Code)
	}
	type page struct {
		Entries []auditEntryDTO
		More    bool
	}
	get := func(q string) page {
		t.Helper()
		w := env.req("GET", "/api/audit"+q, "", "tok-admin")
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", q, w.Code, w.Body)
		}
		var p page
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := get(""); len(p.Entries) != 8 || p.More {
		t.Fatalf("all: %d more=%v", len(p.Entries), p.More)
	}
	if p := get("?host=local&stack=web"); len(p.Entries) != 6 {
		t.Fatalf("stack filter: %d", len(p.Entries))
	}
	if p := get("?actor=ops"); len(p.Entries) != 1 || p.Entries[0].Action != "stack.stop" {
		t.Fatalf("actor filter: %+v", p.Entries)
	}
	if p := get("?action=stack."); len(p.Entries) != 6 {
		t.Fatalf("action prefix: %d", len(p.Entries))
	}
	if p := get("?action=st%25"); len(p.Entries) != 0 {
		t.Fatalf("prefix must be literal, got %d", len(p.Entries))
	}
	// Several actions or hosts match any of them (repeated or comma-separated).
	if p := get("?action=stack.stop,check."); len(p.Entries) != 2 {
		t.Fatalf("two action prefixes: %d", len(p.Entries))
	}
	if p := get("?action=stack.stop&action=image."); len(p.Entries) != 2 {
		t.Fatalf("repeated action: %d", len(p.Entries))
	}
	if p := get("?host=local,other"); len(p.Entries) != 7 {
		t.Fatalf("two hosts: %d", len(p.Entries))
	}
	p1 := get("?limit=3")
	if len(p1.Entries) != 3 || !p1.More {
		t.Fatalf("page 1: %d more=%v", len(p1.Entries), p1.More)
	}
	p2 := get("?limit=3&before=" + itoa(p1.Entries[2].ID))
	if len(p2.Entries) != 3 || !p2.More || p2.Entries[0].ID >= p1.Entries[2].ID {
		t.Fatalf("page 2: %+v", p2)
	}
	p3 := get("?limit=3&before=" + itoa(p2.Entries[2].ID))
	if len(p3.Entries) != 2 || p3.More {
		t.Fatalf("page 3: %d more=%v", len(p3.Entries), p3.More)
	}
	if w := env.req("GET", "/api/audit?limit=abc", "", "tok-admin"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad limit: %d", w.Code)
	}
	if p := get("?limit=100000"); len(p.Entries) != 8 {
		t.Fatalf("limit is capped, not refused: %d", len(p.Entries))
	}
}
