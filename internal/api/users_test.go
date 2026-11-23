package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"moorage/internal/auth"
	"moorage/internal/config"
	"moorage/internal/sso"
	"moorage/internal/sso/ssotest"
	"moorage/internal/store"
)

const testTOTPSecret = "JBSWY3DPEHPK3PXP" // the secret createTestUserSession gives everyone

// call sends a request with an optional session token, peer address and extra headers.
func call(h http.Handler, method, path, body, token, remote string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set(csrfHeader, "1")
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	}
	if remote != "" {
		r.RemoteAddr = remote
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func sessionCookie(w *httptest.ResponseRecorder) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName && c.MaxAge >= 0 && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", w.Body, err)
	}
	return v
}

type linkResp struct {
	User auth.UserInfo
	Link auth.Link
}

// invite creates a user as admin and returns its link token.
func (e *auditEnv) invite(t *testing.T, username, role string) linkResp {
	t.Helper()
	w := e.req("POST", "/api/users", `{"username":"`+username+`","role":"`+role+`","displayName":"`+username+` Test"}`, "tok-admin")
	if w.Code != http.StatusCreated {
		t.Fatalf("create %s: %d %s", username, w.Code, w.Body)
	}
	return decode[linkResp](t, w)
}

// acceptInvite runs inspect, start and finish and returns the new session token.
func (e *auditEnv) acceptInvite(t *testing.T, token, password string) (string, string) {
	t.Helper()
	if w := e.req("POST", "/api/invite/inspect", `{"token":"`+token+`"}`, ""); w.Code != http.StatusOK {
		t.Fatalf("inspect: %d %s", w.Code, w.Body)
	}
	w := e.req("POST", "/api/invite/start", `{"token":"`+token+`","password":"`+password+`"}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	secret := decode[struct{ Secret string }](t, w).Secret
	code := totpNow(t, secret, time.Now())
	w = e.req("POST", "/api/invite/finish", `{"token":"`+token+`","code":"`+code+`"}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("finish: %d %s", w.Code, w.Body)
	}
	return sessionCookie(w), secret
}

func TestInviteFlow(t *testing.T) {
	env := newAuditEnv(t)
	l := env.invite(t, "Alice", "admin")
	if l.User.Username != "alice" || l.User.Status != "invited" || l.Link.Token == "" || l.Link.Kind != auth.LinkInvite {
		t.Fatalf("create: %+v", l)
	}
	if time.Until(l.Link.Expires) < 47*time.Hour || time.Until(l.Link.Expires) > 49*time.Hour {
		t.Fatalf("link must last 48 h: %v", l.Link.Expires)
	}
	// An invited user can't sign in yet.
	if w := env.req("POST", "/api/login", `{"username":"alice","password":"x","code":"000000"}`, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("invited login: %d", w.Code)
	}
	// Duplicate names are refused, case-insensitively.
	if w := env.req("POST", "/api/users", `{"username":"ALICE","role":"viewer"}`, "tok-admin"); w.Code != http.StatusBadRequest {
		t.Fatalf("duplicate: %d", w.Code)
	}
	if w := env.req("POST", "/api/users", `{"username":"x!","role":"viewer"}`, "tok-admin"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad name: %d", w.Code)
	}
	info := decode[auth.InviteInfo](t, env.req("POST", "/api/invite/inspect", `{"token":"`+l.Link.Token+`"}`, ""))
	if info.Username != "alice" || info.Role != "admin" || info.Kind != "invite" {
		t.Fatalf("inspect: %+v", info)
	}
	// Short password, then finishing before the password, then a wrong code.
	if w := env.req("POST", "/api/invite/start", `{"token":"`+l.Link.Token+`","password":"short"}`, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("short password: %d", w.Code)
	}
	if w := env.req("POST", "/api/invite/finish", `{"token":"`+l.Link.Token+`","code":"123456"}`, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("finish first: %d", w.Code)
	}
	password := "alice-long-password-1"
	tok, secret := env.acceptInvite(t, l.Link.Token, password)
	if tok == "" {
		t.Fatal("no session after finishing the invite")
	}
	u, err := env.db.UserByName(context.Background(), "alice")
	if err != nil || !u.Active || u.TOTPSecret != secret || u.Role != "admin" {
		t.Fatalf("alice after invite: %+v %v", u, err)
	}
	if w := env.req("GET", "/api/users", "", tok); w.Code != http.StatusOK {
		t.Fatalf("alice is an admin now: %d", w.Code)
	}
	// Used link: the same generic error as a wrong one.
	used := env.req("POST", "/api/invite/inspect", `{"token":"`+l.Link.Token+`"}`, "")
	wrong := env.req("POST", "/api/invite/inspect", `{"token":"nope"}`, "")
	if used.Code != http.StatusBadRequest || used.Body.String() != wrong.Body.String() {
		t.Fatalf("used vs wrong: %d %s / %s", used.Code, used.Body, wrong.Body)
	}
	// The code used to finish can't sign in again (replay); the next one can.
	code := totpNow(t, secret, time.Now())
	if w := env.req("POST", "/api/login", `{"username":"alice","password":"`+password+`","code":"`+code+`"}`, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("replayed finish code: %d", w.Code)
	}
	code = totpNow(t, secret, time.Now().Add(30*time.Second))
	if w := env.req("POST", "/api/login", `{"username":"Alice","password":"`+password+`","code":"`+code+`"}`, ""); w.Code != http.StatusOK {
		t.Fatalf("login after invite: %d %s", w.Code, w.Body)
	}
}

// TOTP is mandatory for viewers too: the invite can't finish without a code.
func TestInviteViewerNeedsTOTP(t *testing.T) {
	env := newAuditEnv(t)
	l := env.invite(t, "vera", "viewer")
	env.req("POST", "/api/invite/start", `{"token":"`+l.Link.Token+`","password":"vera-long-password-1"}`, "")
	if w := env.req("POST", "/api/invite/finish", `{"token":"`+l.Link.Token+`","code":""}`, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("no code: %d", w.Code)
	}
	if u, _ := env.db.UserByName(context.Background(), "vera"); u.Active {
		t.Fatal("viewer active without TOTP")
	}
}

func TestInviteExpiredRevokedAndLimiter(t *testing.T) {
	env := newAuditEnv(t)
	ctx := context.Background()
	l := env.invite(t, "bob", "viewer")
	generic := env.req("POST", "/api/invite/inspect", `{"token":"wrong"}`, "").Body.String()

	// Re-issuing (the "new link" action) revokes the old link.
	w := env.req("POST", "/api/users/"+itoa(l.User.ID)+"/reset", "", "tok-admin")
	if w.Code != http.StatusOK {
		t.Fatalf("reissue: %d %s", w.Code, w.Body)
	}
	fresh := decode[linkResp](t, w)
	if fresh.Link.Kind != auth.LinkInvite {
		t.Fatalf("an invited user gets a new invite, got %s", fresh.Link.Kind)
	}
	if w := env.req("POST", "/api/invite/inspect", `{"token":"`+l.Link.Token+`"}`, ""); w.Body.String() != generic {
		t.Fatalf("revoked link: %d %s", w.Code, w.Body)
	}
	// Expired link.
	exp := "expired-token-value"
	other, _ := env.db.UserByName(ctx, "viewer") // issuing replaces a user's links, so not bob's
	if err := env.db.IssueUserToken(ctx, sha256Hex(exp), other.ID, "reset", "admin", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if w := env.req("POST", "/api/invite/inspect", `{"token":"`+exp+`"}`, ""); w.Body.String() != generic {
		t.Fatalf("expired link: %d %s", w.Code, w.Body)
	}
	// 5 wrong attempts per minute per IP (the first wrong one was above, plus 2 here = 3
	// so far), then 429, also for password sign-in, which shares the key.
	env.req("POST", "/api/invite/inspect", `{"token":"x2"}`, "")
	env.req("POST", "/api/invite/inspect", `{"token":"x3"}`, "")
	if w := env.req("POST", "/api/invite/inspect", `{"token":"x4"}`, ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("limiter: %d", w.Code)
	}
	if w := env.req("POST", "/api/login", `{"username":"admin","password":"x","code":"1"}`, ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("shared limiter: %d", w.Code)
	}
	// Another IP isn't affected.
	if w := call(env.h, "POST", "/api/invite/inspect", `{"token":"`+fresh.Link.Token+`"}`, "", "198.51.100.7:1", nil); w.Code != http.StatusOK {
		t.Fatalf("other IP: %d %s", w.Code, w.Body)
	}
}

func TestResetRevokesSessionsOnFinish(t *testing.T) {
	env := newAuditEnv(t)
	ctx := context.Background()
	id := createTestUserSession(t, env.db, "carl", "viewer", "tok-carl")
	w := env.req("POST", "/api/users/"+itoa(id)+"/reset", "", "tok-admin")
	if w.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	l := decode[linkResp](t, w)
	if l.Link.Kind != auth.LinkReset {
		t.Fatalf("kind: %s", l.Link.Kind)
	}
	// Issued, not finished: nothing changes yet.
	if w := env.req("GET", "/api/account", "", "tok-carl"); w.Code != http.StatusOK {
		t.Fatalf("session must survive the issue: %d", w.Code)
	}
	before, _ := env.db.UserByID(ctx, id)
	newTok, secret := env.acceptInvite(t, l.Link.Token, "carl-new-password-12")
	if w := env.req("GET", "/api/account", "", "tok-carl"); w.Code != http.StatusUnauthorized {
		t.Fatalf("old session after reset: %d", w.Code)
	}
	if w := env.req("GET", "/api/account", "", newTok); w.Code != http.StatusOK {
		t.Fatalf("new session: %d", w.Code)
	}
	after, _ := env.db.UserByID(ctx, id)
	if after.TOTPSecret != secret || after.PasswordHash == before.PasswordHash || after.TOTPSecret == before.TOTPSecret {
		t.Fatal("reset must replace password and TOTP")
	}
}

// The break-glass rule: at least one active, enabled, local admin with TOTP stays.
func TestLastLocalAdminProtection(t *testing.T) {
	env := newAuditEnv(t)
	ctx := context.Background()
	adminU, _ := env.db.UserByName(ctx, "admin")
	// carol is an OIDC admin: she can act, but she isn't break-glass.
	carol, err := env.db.CreateExternalUser(ctx, "https://idp", "carol-sub", store.User{Role: "admin", AuthSource: store.SourceOIDC},
		func(int) string { return "carol" })
	if err != nil {
		t.Fatal(err)
	}
	if err := env.db.CreateSession(ctx, sha256Hex("tok-carol"), carol.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	aid := itoa(adminU.ID)
	for _, c := range []struct{ method, path, body string }{
		{"PUT", "/api/users/" + aid + "/role", `{"role":"viewer"}`},
		{"POST", "/api/users/" + aid + "/disable", ""},
		{"DELETE", "/api/users/" + aid, ""},
	} {
		if w := env.req(c.method, c.path, c.body, "tok-carol"); w.Code != http.StatusConflict {
			t.Errorf("%s %s on the last local admin: %d %s", c.method, c.path, w.Code, w.Body)
		}
	}
	// Self actions through the list are refused, also with other admins around.
	for _, c := range []struct{ method, path, body string }{
		{"PUT", "/api/users/" + aid + "/role", `{"role":"viewer"}`},
		{"POST", "/api/users/" + aid + "/disable", ""},
		{"DELETE", "/api/users/" + aid, ""},
		{"POST", "/api/users/" + aid + "/reset", ""},
		{"DELETE", "/api/users/" + aid + "/sessions", ""},
	} {
		if w := env.req(c.method, c.path, c.body, "tok-admin"); w.Code != http.StatusConflict {
			t.Errorf("self %s %s: %d", c.method, c.path, w.Code)
		}
	}
	// OIDC roles come from the IdP.
	if w := env.req("PUT", "/api/users/"+itoa(carol.ID)+"/role", `{"role":"viewer"}`, "tok-admin"); w.Code != http.StatusConflict {
		t.Errorf("OIDC role change: %d", w.Code)
	}
	if w := env.req("POST", "/api/users/"+itoa(carol.ID)+"/reset", "", "tok-admin"); w.Code != http.StatusConflict {
		t.Errorf("OIDC reset: %d", w.Code)
	}
	// With a second local admin the first one can be demoted.
	bob := createTestUserSession(t, env.db, "bob", "admin", "tok-bob")
	if w := env.req("PUT", "/api/users/"+aid+"/role", `{"role":"viewer"}`, "tok-carol"); w.Code != http.StatusNoContent {
		t.Fatalf("demote with a spare: %d %s", w.Code, w.Body)
	}
	if w := env.req("POST", "/api/users/"+itoa(bob)+"/disable", "", "tok-carol"); w.Code != http.StatusConflict {
		t.Fatalf("bob is the last one now: %d", w.Code)
	}
	if n, _ := env.db.CountBreakGlassAdmins(ctx); n != 1 {
		t.Fatalf("break-glass admins: %d", n)
	}
}

func TestRoleDisableDeleteRevokeSessions(t *testing.T) {
	env := newAuditEnv(t)
	ctx := context.Background()
	vid, _ := env.db.UserByName(ctx, "viewer")
	id := itoa(vid.ID)
	// A role written behind the API's back applies on the next request: no cached role.
	if err := env.db.UpdateUserRole(ctx, vid.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if w := env.req("GET", "/api/users", "", "tok-viewer"); w.Code != http.StatusOK {
		t.Fatalf("promoted out of band: %d", w.Code)
	}
	env.db.UpdateUserRole(ctx, vid.ID, "viewer") //nolint:errcheck
	if w := env.req("GET", "/api/users", "", "tok-viewer"); w.Code != http.StatusForbidden {
		t.Fatalf("demoted out of band: %d", w.Code)
	}

	steps := []struct {
		method, path, body string
	}{
		{"PUT", "/api/users/" + id + "/role", `{"role":"admin"}`},
		{"POST", "/api/users/" + id + "/disable", ""},
		{"POST", "/api/users/" + id + "/enable", ""},
		{"DELETE", "/api/users/" + id + "/sessions", ""},
		{"DELETE", "/api/users/" + id, ""},
	}
	for i, st := range steps {
		tok := "tok-v" + itoa(int64(i))
		if err := env.db.CreateSession(ctx, sha256Hex(tok), vid.ID, time.Now().Add(time.Hour)); err != nil && st.method != "DELETE" {
			t.Fatal(err)
		}
		if w := env.req("GET", "/api/account", "", tok); w.Code != http.StatusOK && i != 2 { // disabled before enable
			t.Fatalf("%d: session before: %d", i, w.Code)
		}
		if w := env.req(st.method, st.path, st.body, "tok-admin"); w.Code != http.StatusNoContent {
			t.Fatalf("%s %s: %d %s", st.method, st.path, w.Code, w.Body)
		}
		if i == 2 { // enable doesn't end sessions (there are none: disable ended them)
			continue
		}
		if w := env.req("GET", "/api/account", "", tok); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s must end the session: %d", st.method, st.path, w.Code)
		}
	}
	if _, err := env.db.UserByID(ctx, vid.ID); err == nil {
		t.Fatal("user not deleted")
	}
	if w := env.req("DELETE", "/api/users/"+id, "", "tok-admin"); w.Code != http.StatusNotFound {
		t.Fatalf("delete twice: %d", w.Code)
	}
}

// Disabled users can't sign in even with the right password and code.
func TestDisabledLocalLogin(t *testing.T) {
	env := newAuditEnv(t)
	l := env.invite(t, "dora", "viewer")
	password := "dora-long-password-1"
	_, secret := env.acceptInvite(t, l.Link.Token, password)
	if w := env.req("POST", "/api/users/"+itoa(l.User.ID)+"/disable", "", "tok-admin"); w.Code != http.StatusNoContent {
		t.Fatal(w.Code)
	}
	code := totpNow(t, secret, time.Now().Add(30*time.Second))
	if w := call(env.h, "POST", "/api/login", `{"username":"dora","password":"`+password+`","code":"`+code+`"}`, "", "198.51.100.9:1", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("disabled login: %d", w.Code)
	}
}

func TestViewerForbiddenOnUserRoutes(t *testing.T) {
	env := newAuditEnv(t)
	routes := 0
	for _, p := range env.srv.routes {
		method, path, _ := strings.Cut(p, " ")
		if !strings.HasPrefix(path, "/api/users") {
			continue
		}
		routes++
		path = strings.ReplaceAll(path, "{id}", "1")
		for _, tok := range []string{"tok-viewer", ""} {
			want := http.StatusForbidden
			if tok == "" {
				want = http.StatusUnauthorized
			}
			if w := env.req(method, path, `{"role":"admin","username":"zed"}`, tok); w.Code != want {
				t.Errorf("%s %s as %q: %d, want %d", method, path, tok, w.Code, want)
			}
		}
	}
	if routes != 8 {
		t.Fatalf("expected 8 user routes, got %d", routes)
	}
	if u, _ := env.db.UserByName(context.Background(), "zed"); u.ID != 0 {
		t.Fatal("viewer created a user")
	}
}

func TestAccountPasswordTOTPAndSessions(t *testing.T) {
	env := newAuditEnv(t)
	ctx := context.Background()
	l := env.invite(t, "erin", "viewer")
	password := "erin-long-password-1"
	tok, secret := env.acceptInvite(t, l.Link.Token, password)
	other := "tok-erin-other"
	env.db.CreateSessionMeta(ctx, sha256Hex(other), l.User.ID, time.Now().Add(time.Hour), "203.0.113.5", "Firefox") //nolint:errcheck

	acc := decode[auth.Account](t, env.req("GET", "/api/account", "", tok))
	if len(acc.Sessions) != 2 || !acc.TOTP || acc.User.Source != "local" {
		t.Fatalf("account: %+v", acc)
	}
	var mine, theirs auth.SessionInfo
	for _, s := range acc.Sessions {
		if s.Current {
			mine = s
		} else {
			theirs = s
		}
	}
	if mine.ID == 0 || theirs.IP != "203.0.113.5" || theirs.UserAgent != "Firefox" {
		t.Fatalf("sessions: %+v", acc.Sessions)
	}
	// Wrong current password, then a real change: other sessions end, this one stays.
	if w := env.req("PUT", "/api/account/password", `{"current":"nope","password":"erin-newer-password-2"}`, tok); w.Code != http.StatusBadRequest {
		t.Fatalf("wrong current: %d", w.Code)
	}
	newPassword := "erin-newer-password-2"
	if w := env.req("PUT", "/api/account/password", `{"current":"`+password+`","password":"`+newPassword+`"}`, tok); w.Code != http.StatusNoContent {
		t.Fatalf("change: %d %s", w.Code, w.Body)
	}
	if env.req("GET", "/api/account", "", other).Code != http.StatusUnauthorized || env.req("GET", "/api/account", "", tok).Code != http.StatusOK {
		t.Fatal("password change must end other sessions only")
	}

	// TOTP re-enrolment: password + current code, then the new code.
	code := totpNow(t, secret, time.Now().Add(30*time.Second))
	w := env.req("POST", "/api/account/totp/start", `{"password":"`+newPassword+`","code":"`+code+`"}`, tok)
	if w.Code != http.StatusOK {
		t.Fatalf("totp start: %d %s", w.Code, w.Body)
	}
	en := decode[auth.TOTPEnrolment](t, w)
	newCode := totpNow(t, en.Secret, time.Now())
	tampered := strings.Replace(en.Ticket, en.Ticket[len(en.Ticket)-4:], "0000", 1)
	if w := env.req("PUT", "/api/account/totp", `{"secret":"`+en.Secret+`","ticket":"`+tampered+`","code":"`+newCode+`"}`, tok); w.Code != http.StatusBadRequest {
		t.Fatalf("tampered ticket: %d", w.Code)
	}
	if w := env.req("PUT", "/api/account/totp", `{"secret":"AAAAAAAAAAAAAAAA","ticket":"`+en.Ticket+`","code":"`+newCode+`"}`, tok); w.Code != http.StatusBadRequest {
		t.Fatalf("secret swapped: %d", w.Code)
	}
	if w := env.req("PUT", "/api/account/totp", `{"secret":"`+en.Secret+`","ticket":"`+en.Ticket+`","code":"`+newCode+`"}`, tok); w.Code != http.StatusNoContent {
		t.Fatalf("totp finish: %d %s", w.Code, w.Body)
	}
	if u, _ := env.db.UserByID(ctx, l.User.ID); u.TOTPSecret != en.Secret {
		t.Fatal("secret not replaced")
	}

	// Sign out one session, then all others.
	s2, s3 := "tok-erin-2", "tok-erin-3"
	env.db.CreateSession(ctx, sha256Hex(s2), l.User.ID, time.Now().Add(time.Hour)) //nolint:errcheck
	env.db.CreateSession(ctx, sha256Hex(s3), l.User.ID, time.Now().Add(time.Hour)) //nolint:errcheck
	acc = decode[auth.Account](t, env.req("GET", "/api/account", "", s2))
	var s2id int64
	for _, s := range acc.Sessions {
		if s.Current {
			s2id = s.ID
		}
	}
	// Someone else's session id is not found.
	adminSessions := decode[auth.Account](t, env.req("GET", "/api/account", "", "tok-admin")).Sessions
	if w := env.req("DELETE", "/api/account/sessions/"+itoa(adminSessions[0].ID), "", tok); w.Code != http.StatusNotFound {
		t.Fatalf("foreign session: %d", w.Code)
	}
	if w := env.req("DELETE", "/api/account/sessions/"+itoa(s2id), "", tok); w.Code != http.StatusNoContent {
		t.Fatalf("revoke one: %d", w.Code)
	}
	if env.req("GET", "/api/account", "", s2).Code != http.StatusUnauthorized || env.req("GET", "/api/account", "", s3).Code != http.StatusOK {
		t.Fatal("revoke one ended the wrong session")
	}
	if w := env.req("DELETE", "/api/account/sessions", "", tok); w.Code != http.StatusOK {
		t.Fatalf("revoke others: %d", w.Code)
	}
	if env.req("GET", "/api/account", "", s3).Code != http.StatusUnauthorized || env.req("GET", "/api/account", "", tok).Code != http.StatusOK {
		t.Fatal("revoke others")
	}
}

// Every M10 action writes its row, and no row holds a token, password or code.
func TestUserAuditRowsHaveNoSecrets(t *testing.T) {
	env := newAuditEnv(t)
	ctx := context.Background()
	l := env.invite(t, "fay", "viewer")
	password := "fay-long-password-1"
	tok, secret := env.acceptInvite(t, l.Link.Token, password)
	id := itoa(l.User.ID)
	env.req("PUT", "/api/users/"+id+"/role", `{"role":"admin"}`, "tok-admin")
	r := decode[linkResp](t, env.req("POST", "/api/users/"+id+"/reset", "", "tok-admin"))
	env.req("POST", "/api/users/"+id+"/disable", "", "tok-admin")
	env.req("POST", "/api/users/"+id+"/enable", "", "tok-admin")
	tok, secret2 := env.acceptInvite(t, r.Link.Token, password+"x")
	newPw := "fay-another-password-3"
	env.req("PUT", "/api/account/password", `{"current":"`+password+`x","password":"`+newPw+`"}`, tok)
	code := totpNow(t, secret2, time.Now().Add(30*time.Second))
	en := decode[auth.TOTPEnrolment](t, env.req("POST", "/api/account/totp/start", `{"password":"`+newPw+`","code":"`+code+`"}`, tok))
	code2 := totpNow(t, en.Secret, time.Now())
	env.req("PUT", "/api/account/totp", `{"secret":"`+en.Secret+`","ticket":"`+en.Ticket+`","code":"`+code2+`"}`, tok)
	env.db.CreateSession(ctx, sha256Hex("tok-fay-2"), l.User.ID, time.Now().Add(time.Hour)) //nolint:errcheck
	acc := decode[auth.Account](t, env.req("GET", "/api/account", "", "tok-fay-2"))
	env.req("DELETE", "/api/account/sessions/"+itoa(acc.Sessions[0].ID), "", tok)
	env.req("DELETE", "/api/account/sessions", "", tok)
	env.req("DELETE", "/api/users/"+id+"/sessions", "", "tok-admin")
	env.req("DELETE", "/api/users/"+id, "", "tok-admin")

	rows := auditRows(t, env.db)
	got := map[string]int{}
	for _, r := range rows {
		got[r.Action]++
	}
	for _, a := range []string{"user.create", "user.role", "user.reset", "user.disable", "user.enable", "user.sessions_revoke",
		"user.delete", "auth.invite_accept", "account.password", "account.totp", "account.session_revoke"} {
		if got[a] == 0 {
			t.Errorf("no %s row; got %v", a, got)
		}
	}
	if got["auth.invite_accept"] != 2 || got["account.session_revoke"] != 2 {
		t.Errorf("counts: %v", got)
	}
	for _, r := range rows {
		if r.Action == "user.create" && (r.Target != "fay" || r.Actor != "admin") {
			t.Errorf("create row: %+v", r)
		}
	}
	b, _ := json.Marshal(rows)
	for _, s := range []string{l.Link.Token, r.Link.Token, password, newPw, secret, secret2, en.Secret, en.Ticket, code, code2, tok} {
		if strings.Contains(string(b), s) {
			t.Fatalf("secret %q in the audit log: %s", s, b)
		}
	}
}

// ---- forward-auth ------------------------------------------------------------

func newProxyEnv(t *testing.T, mod func(*config.Config)) *auditEnv {
	t.Helper()
	env := newAuditEnv(t)
	cfg := config.Config{Proxy: config.ProxyConfig{
		Trusted: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, UserHeader: "Remote-User", GroupsHeader: "Remote-Groups",
		Roles: config.RoleMapping{AdminGroups: []string{"admins"}, ViewerGroups: []string{"staff"}, DefaultRole: "none"},
	}}
	if mod != nil {
		mod(&cfg)
	}
	env.srv = New(cfg, env.srv.auth, env.db, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	env.h = env.srv.Handler()
	return env
}

const trustedPeer = "10.1.2.3:40000"

func TestForwardAuth(t *testing.T) {
	env := newProxyEnv(t, nil)
	ctx := context.Background()
	hdr := map[string]string{"Remote-User": "gina", "Remote-Groups": "staff"}

	// Trusted peer: signed in as a new proxy viewer with a normal session cookie.
	w := call(env.h, "GET", "/api/session", "", "", trustedPeer, hdr)
	sess := decode[sessionResp](t, w)
	tok := sessionCookie(w)
	if sess.User == nil || sess.User.Username != "gina" || sess.User.Role != "viewer" || sess.User.Source != "proxy" || tok == "" {
		t.Fatalf("trusted: %+v cookie=%q", sess, tok)
	}
	// Same user again with the cookie: same session, no new row.
	if w := call(env.h, "GET", "/api/hosts", "", tok, trustedPeer, hdr); w.Code != http.StatusOK || sessionCookie(w) != "" {
		t.Fatalf("reuse: %d cookie=%q", w.Code, sessionCookie(w))
	}
	// Untrusted peer: header ignored, also with a spoofed X-Forwarded-For.
	for _, h := range []map[string]string{hdr, {"Remote-User": "gina", "X-Forwarded-For": "10.1.2.3", "X-Real-IP": "10.1.2.3"}} {
		w := call(env.h, "GET", "/api/session", "", "", "192.0.2.50:1", h)
		if s := decode[sessionResp](t, w); s.User != nil || sessionCookie(w) != "" {
			t.Fatalf("untrusted peer signed in: %+v", s)
		}
		if w := call(env.h, "GET", "/api/hosts", "", "", "192.0.2.50:1", h); w.Code != http.StatusUnauthorized {
			t.Fatalf("untrusted on a user route: %d", w.Code)
		}
	}
	// Groups -> role, updated on every request.
	w = call(env.h, "GET", "/api/users", "", tok, trustedPeer, map[string]string{"Remote-User": "gina", "Remote-Groups": "staff, admins"})
	if w.Code != http.StatusOK {
		t.Fatalf("admin group: %d", w.Code)
	}
	// No mapped group and default none: refused, and the old session is dropped.
	w = call(env.h, "GET", "/api/hosts", "", tok, trustedPeer, map[string]string{"Remote-User": "gina", "Remote-Groups": "nobody"})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "no access") {
		t.Fatalf("no group: %d %s", w.Code, w.Body)
	}
	if s := decode[sessionResp](t, call(env.h, "GET", "/api/session", "", tok, trustedPeer, map[string]string{"Remote-User": "gina"})); s.User != nil || s.ProxyError == "" {
		t.Fatalf("refused session: %+v", s)
	}
	// User switch drops the old session.
	w = call(env.h, "GET", "/api/session", "", "", trustedPeer, hdr)
	tokG := sessionCookie(w)
	w = call(env.h, "GET", "/api/session", "", tokG, trustedPeer, map[string]string{"Remote-User": "hank", "Remote-Groups": "staff"})
	if s := decode[sessionResp](t, w); s.User == nil || s.User.Username != "hank" || sessionCookie(w) == "" {
		t.Fatalf("switch: %+v", s)
	}
	if _, err := env.db.SessionByHash(ctx, sha256Hex(tokG)); err == nil {
		t.Fatal("the old user's session survived the switch")
	}
	// Without the header the cookie rules apply.
	if w := call(env.h, "GET", "/api/hosts", "", sessionCookie(w), trustedPeer, nil); w.Code != http.StatusOK {
		t.Fatalf("cookie without header: %d", w.Code)
	}
	// A proxy name equal to a local user never takes the local account.
	w = call(env.h, "GET", "/api/session", "", "", trustedPeer, map[string]string{"Remote-User": "admin", "Remote-Groups": "admins"})
	if s := decode[sessionResp](t, w); s.User == nil || s.User.Username == "admin" || s.User.Source != "proxy" {
		t.Fatalf("collision: %+v", s)
	}
	// Disabled wins over the proxy.
	gina, _ := env.db.UserByName(ctx, "gina")
	if w := env.req("POST", "/api/users/"+itoa(gina.ID)+"/disable", "", "tok-admin"); w.Code != http.StatusNoContent {
		t.Fatal(w.Code)
	}
	if w := call(env.h, "GET", "/api/hosts", "", "", trustedPeer, hdr); w.Code != http.StatusForbidden {
		t.Fatalf("disabled proxy user: %d", w.Code)
	}
	logins := 0
	for _, r := range auditRows(t, env.db) {
		if r.Action == auditProxyLogin {
			logins++
		}
	}
	if logins != 4 { // gina, gina again after refusal, hank, admin-2
		t.Fatalf("proxy_login rows: %d", logins)
	}
}

// /metrics, status pages and the status listener never honour the header.
func TestForwardAuthIgnoredOutsideTheApp(t *testing.T) {
	env := newProxyEnv(t, func(c *config.Config) { c.MetricsToken = "a-long-enough-metrics-token" })
	hdr := map[string]string{"Remote-User": "ivy", "Remote-Groups": "admins"}
	for _, p := range []string{"/metrics", "/status/x", "/status-assets/x.css"} {
		w := call(env.h, "GET", p, "", "", trustedPeer, hdr)
		if sessionCookie(w) != "" {
			t.Errorf("%s set a session", p)
		}
		w = call(env.srv.StatusHandler(), "GET", p, "", "", trustedPeer, hdr)
		if sessionCookie(w) != "" {
			t.Errorf("status listener %s set a session", p)
		}
	}
	// Sign-in routes are public: the header doesn't sign in there either.
	if w := call(env.h, "POST", "/api/login", `{"username":"x","password":"y","code":"1"}`, "", trustedPeer, hdr); sessionCookie(w) != "" {
		t.Error("login route used the header")
	}
	if u, _ := env.db.UserByName(context.Background(), "ivy"); u.ID != 0 {
		t.Fatal("a proxy user was created outside the app routes")
	}
}

// ---- OIDC through the API ----------------------------------------------------

func newOIDCEnv(t *testing.T, mod func(*config.OIDCConfig)) (*auditEnv, *ssotest.IdP) {
	t.Helper()
	env := newAuditEnv(t)
	idp := ssotest.New(t)
	cfg := config.OIDCConfig{
		Issuer: idp.Issuer(), ClientID: ssotest.ClientID, ClientSecret: "client-secret-xyz", Scopes: []string{"openid", "groups"},
		GroupsClaim: "groups", Label: "Sign in with Test", LocalLogin: true,
		Roles: config.RoleMapping{AdminGroups: []string{"ops"}, ViewerGroups: []string{"dev"}, DefaultRole: "none"},
	}
	if mod != nil {
		mod(&cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	o := sso.NewOIDC(cfg, "https://moorage.example.com", log, sso.Options{Client: idp.Client(), JWKSGap: time.Nanosecond})
	if err := o.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	env.srv.SetOIDC(o)
	env.srv.cfg.OIDC = cfg
	return env, idp
}

// oidcLogin goes start -> provider -> callback and returns the redirect and the session.
func oidcLogin(t *testing.T, env *auditEnv, idp *ssotest.IdP) (string, string) {
	t.Helper()
	w := call(env.h, "GET", "/api/auth/oidc/start", "", "", "", nil)
	if w.Code != http.StatusFound {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	var state *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == oidcStateCookie {
			state = c
		}
	}
	if state == nil || !state.HttpOnly || state.SameSite != http.SameSiteLaxMode {
		t.Fatalf("state cookie: %+v", state)
	}
	code, st := idp.Authorize(t, w.Header().Get("Location"))
	r := httptest.NewRequest("GET", sso.CallbackPath+"?state="+url.QueryEscape(st)+"&code="+url.QueryEscape(code), nil)
	r.AddCookie(state)
	cw := httptest.NewRecorder()
	env.h.ServeHTTP(cw, r)
	if cw.Code != http.StatusSeeOther {
		t.Fatalf("callback: %d %s", cw.Code, cw.Body)
	}
	return cw.Header().Get("Location"), sessionCookie(cw)
}

func TestOIDCLoginThroughAPI(t *testing.T) {
	env, idp := newOIDCEnv(t, nil)
	ctx := context.Background()
	s := decode[sessionResp](t, env.req("GET", "/api/session", "", ""))
	if !s.Login.SSO || s.Login.SSOLabel != "Sign in with Test" || !s.Login.LocalLogin {
		t.Fatalf("login info: %+v", s.Login)
	}
	// First login creates the user from preferred_username; the local "admin" is never
	// linked, the new user gets a suffix.
	idp.Claims = map[string]any{"sub": "sub-42", "preferred_username": "Admin", "email": "a@example.com", "name": "Ada", "groups": []string{"dev"}}
	loc, tok := oidcLogin(t, env, idp)
	if loc != "/#/" || tok == "" {
		t.Fatalf("redirect %q cookie %q", loc, tok)
	}
	acc := decode[auth.Account](t, env.req("GET", "/api/account", "", tok))
	if acc.User.Username != "admin-2" || acc.User.Role != "viewer" || acc.User.Source != "oidc" || acc.User.DisplayName != "Ada" {
		t.Fatalf("first login: %+v", acc.User)
	}
	local, _ := env.db.UserByName(ctx, "admin")
	if local.AuthSource != "local" || acc.User.ID == local.ID {
		t.Fatal("OIDC user linked to the local admin")
	}
	// No password controls for OIDC users.
	if w := env.req("PUT", "/api/account/password", `{"current":"x","password":"long-enough-pass"}`, tok); w.Code != http.StatusConflict {
		t.Fatalf("OIDC password change: %d", w.Code)
	}
	// Second login: same account, role updated from groups.
	idp.Claims["groups"] = []string{"ops"}
	_, tok2 := oidcLogin(t, env, idp)
	acc2 := decode[auth.Account](t, env.req("GET", "/api/account", "", tok2))
	if acc2.User.ID != acc.User.ID || acc2.User.Role != "admin" {
		t.Fatalf("second login: %+v", acc2.User)
	}
	// Logout of an OIDC user offers the provider's end-session URL.
	w := env.req("POST", "/api/logout", "", tok2)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "/logout?client_id=moorage") {
		t.Fatalf("logout: %d %s", w.Code, w.Body)
	}
	// Disabled wins over the IdP.
	if w := env.req("POST", "/api/users/"+itoa(acc.User.ID)+"/disable", "", "tok-admin"); w.Code != http.StatusNoContent {
		t.Fatal(w.Code)
	}
	if loc, tok := oidcLogin(t, env, idp); loc != "/#/login?sso=disabled" || tok != "" {
		t.Fatalf("disabled: %q %q", loc, tok)
	}
	// Default role none and no group: refused, audited with subject and reason.
	idp.Claims = map[string]any{"sub": "sub-99", "preferred_username": "nobody", "groups": []string{"other"}}
	if loc, tok := oidcLogin(t, env, idp); loc != "/#/login?sso=denied" || tok != "" {
		t.Fatalf("denied: %q %q", loc, tok)
	}
	if u, _ := env.db.UserByIdentity(ctx, idp.Issuer(), "sub-99"); u.ID != 0 {
		t.Fatal("refused user was created")
	}
	var denied []store.AuditEntry
	logins := 0
	for _, r := range auditRows(t, env.db) {
		switch r.Action {
		case auditOIDCDenied:
			denied = append(denied, r)
		case auditOIDCLogin:
			logins++
		}
	}
	if logins != 2 || len(denied) != 2 || !strings.Contains(denied[0].Target, "denied") || !strings.Contains(denied[0].Target, "sub-99") ||
		!strings.Contains(denied[1].Target, "disabled") {
		t.Fatalf("audit: logins=%d denied=%+v", logins, denied)
	}
	b, _ := json.Marshal(auditRows(t, env.db))
	if strings.Contains(string(b), "client-secret-xyz") || strings.Contains(string(b), "eyJ") {
		t.Fatal("secret or token in the audit log")
	}
	// The admin panel shows the config without the secret.
	w = env.req("GET", "/api/users", "", "tok-admin")
	if !strings.Contains(w.Body.String(), "https://moorage.example.com/api/auth/oidc/callback") || strings.Contains(w.Body.String(), "client-secret-xyz") {
		t.Fatalf("users: %s", w.Body)
	}
}

func TestOIDCCallbackErrors(t *testing.T) {
	env, idp := newOIDCEnv(t, nil)
	// Replayed callback: the state works once.
	w := call(env.h, "GET", "/api/auth/oidc/start", "", "", "", nil)
	state := w.Result().Cookies()[0]
	code, st := idp.Authorize(t, w.Header().Get("Location"))
	cb := func() string {
		r := httptest.NewRequest("GET", sso.CallbackPath+"?state="+url.QueryEscape(st)+"&code="+url.QueryEscape(code), nil)
		r.AddCookie(state)
		rw := httptest.NewRecorder()
		env.h.ServeHTTP(rw, r)
		return rw.Header().Get("Location")
	}
	if loc := cb(); loc != "/#/login?sso=denied" { // no groups, default none
		t.Fatalf("first: %s", loc)
	}
	if loc := cb(); loc != "/#/login?sso=expired" {
		t.Fatalf("replay: %s", loc)
	}
	// Without the state cookie (another browser): expired.
	w = call(env.h, "GET", "/api/auth/oidc/start", "", "", "", nil)
	code, st = idp.Authorize(t, w.Header().Get("Location"))
	rw := httptest.NewRecorder()
	env.h.ServeHTTP(rw, httptest.NewRequest("GET", sso.CallbackPath+"?state="+st+"&code="+code, nil))
	if loc := rw.Header().Get("Location"); loc != "/#/login?sso=expired" {
		t.Fatalf("no cookie: %s", loc)
	}
	// Provider error.
	rw = httptest.NewRecorder()
	env.h.ServeHTTP(rw, httptest.NewRequest("GET", sso.CallbackPath+"?error=access_denied", nil))
	if loc := rw.Header().Get("Location"); loc != "/#/login?sso=idp" {
		t.Fatalf("idp error: %s", loc)
	}
	// A bad token is recorded for the admin panel.
	idp.Mutate = func(c map[string]any) { c["aud"] = "other" }
	w = call(env.h, "GET", "/api/auth/oidc/start", "", "", "", nil)
	state = w.Result().Cookies()[0]
	code, st = idp.Authorize(t, w.Header().Get("Location"))
	if loc := cb(); loc != "/#/login?sso=failed" {
		t.Fatalf("bad token: %s", loc)
	}
	if s := env.srv.oidc.Status(); s.LastError == "" {
		t.Fatal("no last error recorded")
	}
}

// OIDC sign-in waits for setup: the first admin is always local.
func TestOIDCBeforeSetupRefused(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, _ := auth.New(db, log)
	idp := ssotest.New(t)
	idp.Claims = map[string]any{"groups": []string{"ops"}}
	cfg := config.OIDCConfig{Issuer: idp.Issuer(), ClientID: ssotest.ClientID, Scopes: []string{"openid"}, GroupsClaim: "groups",
		Roles: config.RoleMapping{AdminGroups: []string{"ops"}, DefaultRole: "none"}, LocalLogin: true}
	srv := New(config.Config{}, a, db, nil, nil, nil, log, "test")
	o := sso.NewOIDC(cfg, "https://m.example.com", log, sso.Options{Client: idp.Client(), JWKSGap: time.Nanosecond})
	if err := o.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv.SetOIDC(o)
	env := &auditEnv{db: db, srv: srv, h: srv.Handler()}
	if loc, tok := oidcLogin(t, env, idp); loc != "/#/login?sso=setup" || tok != "" {
		t.Fatalf("before setup: %q %q", loc, tok)
	}
}

// A broken OIDC setting: Moorage serves, the button is hidden, admins see the problem.
func TestOIDCBrokenConfigAtStartup(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	createTestUserSession(t, db, "admin", "admin", "tok-admin")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, _ := auth.New(db, log)
	cfg := config.Config{PublicURL: "https://m.example.com", OIDC: config.OIDCConfig{
		Issuer: "http://idp.example.com", ClientID: "moorage", Label: "SSO", LocalLogin: false, Scopes: []string{"openid"}}}
	srv := New(cfg, a, db, nil, nil, nil, log, "test")
	srv.StartSSO(context.Background())
	h := srv.Handler()
	s := decode[sessionResp](t, do(h, "GET", "/api/session", "", nil))
	if s.Login.SSO || !s.Login.LocalLogin {
		t.Fatalf("login info: %+v", s.Login)
	}
	w := do(h, "GET", "/api/users", "", map[string]string{"Cookie": cookieName + "=tok-admin"})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "must use https") {
		t.Fatalf("users: %d %s", w.Code, w.Body)
	}
	if w := do(h, "GET", "/api/auth/oidc/start", "", nil); w.Header().Get("Location") != "/#/login?sso=unavailable" {
		t.Fatalf("start: %v", w.Header())
	}
}

func TestSanitizeUsername(t *testing.T) {
	for in, want := range map[string]string{
		"Alice.Smith": "alice.smith", "  ": "", "__x": "x00", "Jürgen Müller": "j-rgen-m-ller",
		strings.Repeat("x", 40): strings.Repeat("x", 28),
	} {
		if got := auth.SanitizeUsername(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
