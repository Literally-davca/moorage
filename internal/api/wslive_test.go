package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"moorage/internal/config"
	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/sso"
	"moorage/internal/store"
)

// apiCall sends one signed-in API request (cookie session or "Bearer <secret>").
func apiCall(t *testing.T, baseURL, method, path, cred string, body any) *http.Response {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, baseURL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Close = true
	req.Header.Set(csrfHeader, "1")
	req.Header.Set("Content-Type", "application/json")
	if strings.HasPrefix(cred, "Bearer ") {
		req.Header.Set("Authorization", cred)
	} else if cred != "" {
		req.Header.Set("Cookie", cookieName+"="+cred)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// expectSessionEnded reads a terminal until it closes and wants the "Session ended"
// error frame and close code 4401.
func expectSessionEnded(t *testing.T, ws *websocket.Conn, what string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sawFrame := false
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			if got := websocket.CloseStatus(err); got != closeSessionEnded {
				t.Fatalf("%s: close status %d (%v), want 4401", what, got, err)
			}
			break
		}
		var ctrl host.TermControl
		if typ == websocket.MessageText && json.Unmarshal(data, &ctrl) == nil && ctrl.Type == "error" && strings.Contains(ctrl.Message, "Session ended") {
			sawFrame = true
		}
	}
	if !sawFrame {
		t.Fatalf("%s: no \"Session ended\" frame before the close", what)
	}
}

func idleTermSession(env *termTestEnv) *fakeExecSession {
	s := newFakeExecSession()
	go func() {
		buf := make([]byte, 1024)
		for {
			if _, err := s.inR.Read(buf); err != nil {
				return
			}
		}
	}()
	env.execBack.onStart = func(string, string, int, int) (engine.TerminalSession, error) { return s, nil }
	return s
}

// The ticket opens one terminal once, for 30 s, for one target, and only while the
// sign-in it was made for still holds; the cookie alone never opens a terminal.
func TestTerminalTicketRules(t *testing.T) {
	env := newTermTestEnv(t, true, false)
	ctx := context.Background()
	idleTermSession(env)
	q := "container=whoami-web-1&shell=sh"
	req := terminalTicketReq{Kind: "stack", Host: "local", Stack: "whoami", Container: "whoami-web-1"}

	tk, _, err := termTicketFor(ctx, env.ts.URL, env.adminToken, req)
	if err != nil {
		t.Fatal(err)
	}
	ws, _, err := dialTermWSTicket(ctx, env.ts.URL, "local", "whoami", q+"&ticket="+tk, "")
	if err != nil {
		t.Fatalf("valid ticket refused: %v", err)
	}
	ws.CloseNow()

	refused := func(what, query string, hdr http.Header) {
		t.Helper()
		wsURL := "ws" + strings.TrimPrefix(env.ts.URL, "http") + "/api/hosts/local/stacks/whoami/terminal?" + query
		_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
		if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: want 401 before the upgrade, got resp=%v err=%v", what, resp, err)
		}
	}
	refused("used ticket", q+"&ticket="+tk, nil)
	refused("cookie without ticket", q, http.Header{"Cookie": {cookieName + "=" + env.adminToken}})
	refused("made-up ticket", q+"&ticket=deadbeef", nil)

	// Another container than the ticket names.
	tk, _, _ = termTicketFor(ctx, env.ts.URL, env.adminToken, req)
	refused("other target", "container=whoami-old-1&shell=sh&ticket="+tk, nil)

	// Expired after 30 s.
	tk, _, _ = termTicketFor(ctx, env.ts.URL, env.adminToken, req)
	env.srv.tickets.mu.Lock()
	env.srv.tickets.now = func() time.Time { return time.Now().Add(ticketTTL + time.Second) }
	env.srv.tickets.mu.Unlock()
	refused("expired ticket", q+"&ticket="+tk, nil)
	env.srv.tickets.mu.Lock()
	env.srv.tickets.now = nil
	env.srv.tickets.mu.Unlock()

	// The session ended between the ticket and the upgrade.
	_ = env.db.CreateSession(ctx, sha256Hex("tok-admin-short"), env.adminID, time.Now().Add(time.Hour))
	tk, _, _ = termTicketFor(ctx, env.ts.URL, "tok-admin-short", req)
	_ = env.db.DeleteSession(ctx, sha256Hex("tok-admin-short"))
	refused("signed out after the ticket", q+"&ticket="+tk, nil)

	// Viewers get no ticket at all; two panels get two different tickets.
	if _, resp, err := termTicketFor(ctx, env.ts.URL, env.viewerToken, req); err == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer ticket: %v %v", resp, err)
	}
	a, _, _ := termTicketFor(ctx, env.ts.URL, env.adminToken, req)
	b, _, _ := termTicketFor(ctx, env.ts.URL, env.adminToken, req)
	if a == "" || a == b {
		t.Fatalf("tickets must differ: %q %q", a, b)
	}
	if resp := apiCall(t, env.ts.URL, http.MethodPost, "/api/terminal/ticket", env.adminToken, terminalTicketReq{Kind: "stack", Host: "nope", Stack: "x"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown host: %d", resp.StatusCode)
	}
}

// Origin: the request Host always; the host of the public URL; X-Forwarded-Host only from
// a trusted proxy; anything else 403.
func TestTerminalOriginBehindProxy(t *testing.T) {
	env := newTermTestEnv(t, true, false)
	ctx := context.Background()
	idleTermSession(env)
	req := terminalTicketReq{Kind: "stack", Host: "local", Stack: "whoami", Container: "whoami-web-1"}
	dial := func(origin, fwdHost string) (int, error) {
		tk, _, err := termTicketFor(ctx, env.ts.URL, env.adminToken, req)
		if err != nil {
			t.Fatal(err)
		}
		hdr := http.Header{"Origin": {origin}}
		if fwdHost != "" {
			hdr.Set("X-Forwarded-Host", fwdHost)
		}
		wsURL := "ws" + strings.TrimPrefix(env.ts.URL, "http") + "/api/hosts/local/stacks/whoami/terminal?container=whoami-web-1&ticket=" + tk
		ws, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
		if err == nil {
			ws.CloseNow()
			return http.StatusSwitchingProtocols, nil
		}
		if resp == nil {
			return 0, err
		}
		return resp.StatusCode, err
	}
	if code, err := dial(env.ts.URL, ""); code != http.StatusSwitchingProtocols {
		t.Fatalf("same origin: %d %v", code, err)
	}
	if code, _ := dial("https://moorage.example.com", ""); code != http.StatusForbidden {
		t.Fatalf("foreign origin without MOORAGE_PUBLIC_URL: %d", code)
	}
	env.srv.cfg.PublicURL, env.srv.cfg.PublicURLSet = "https://moorage.example.com/", true
	if code, err := dial("https://moorage.example.com", ""); code != http.StatusSwitchingProtocols {
		t.Fatalf("origin of MOORAGE_PUBLIC_URL: %d %v", code, err)
	}
	if code, _ := dial("https://evil.example.com", ""); code != http.StatusForbidden {
		t.Fatalf("other origin with a public URL: %d", code)
	}
	// X-Forwarded-Host from an untrusted peer proves nothing.
	if code, _ := dial("https://proxy.example.com", "proxy.example.com"); code != http.StatusForbidden {
		t.Fatalf("X-Forwarded-Host from an untrusted peer: %d", code)
	}
	env.srv.proxy = sso.NewProxy(config.ProxyConfig{Trusted: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}, UserHeader: "Remote-User"})
	if code, err := dial("https://proxy.example.com", "proxy.example.com, inner.local"); code != http.StatusSwitchingProtocols {
		t.Fatalf("X-Forwarded-Host from a trusted proxy: %d %v", code, err)
	}
	// A glob in the header never widens the rule.
	if code, _ := dial("https://evil.example.com", "*"); code != http.StatusForbidden {
		t.Fatalf("glob X-Forwarded-Host: %d", code)
	}
}

// Sign-out, a role change to viewer and a deleted API token close the open terminals at
// once (4401), not at the next periodic check.
func TestLiveConnectionsClosedOnRevocation(t *testing.T) {
	env := newTermTestEnv(t, true, false)
	ctx := context.Background()
	env.srv.SetTerminalLimits(TerminalLimits{MaxConcurrent: 8, IdleTimeout: time.Minute, HardLimit: time.Minute, RevokeCheckInt: time.Hour})
	idleTermSession(env)
	q := "container=whoami-web-1&shell=sh"

	// 1. Sign-out.
	_ = env.db.CreateSession(ctx, sha256Hex("tok-a1"), env.adminID, time.Now().Add(time.Hour))
	ws, _, err := dialTermWS(ctx, env.ts.URL, "local", "whoami", q, "tok-a1", "")
	if err != nil {
		t.Fatal(err)
	}
	if resp := apiCall(t, env.ts.URL, http.MethodPost, "/api/logout", "tok-a1", nil); resp.StatusCode/100 != 2 {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	expectSessionEnded(t, ws, "sign-out")

	// 2. Another admin is demoted to viewer.
	opsID := createTestUserSession(t, env.db, "ops", "admin", "tok-ops")
	idleTermSession(env) // each terminal needs its own fake shell
	ws, _, err = dialTermWS(ctx, env.ts.URL, "local", "whoami", q, "tok-ops", "")
	if err != nil {
		t.Fatal(err)
	}
	if resp := apiCall(t, env.ts.URL, http.MethodPut, "/api/users/"+strconv.FormatInt(opsID, 10)+"/role", env.adminToken, map[string]string{"role": "viewer"}); resp.StatusCode/100 != 2 {
		t.Fatalf("set role: %d", resp.StatusCode)
	}
	expectSessionEnded(t, ws, "demoted to viewer")

	// 3. A revoked API token.
	secret := "mrg_live_test_token_secret_0123456789"
	tok, err := env.db.CreateAPIToken(ctx, store.APIToken{Name: "ci", TokenHash: hashAPIToken(secret), Role: "admin", Scopes: []string{"admin"}, CreatedBy: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	tk, _, err := termTicketFor(ctx, env.ts.URL, "", terminalTicketReq{Kind: "stack", Host: "local", Stack: "whoami", Container: "whoami-web-1"})
	if err == nil {
		t.Fatalf("ticket without sign-in: %q", tk)
	}
	resp := apiCall(t, env.ts.URL, http.MethodPost, "/api/terminal/ticket", "Bearer "+secret, terminalTicketReq{Kind: "stack", Host: "local", Stack: "whoami", Container: "whoami-web-1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bearer ticket: %d", resp.StatusCode)
	}
	// apiCall drops the body; ask again keeping it.
	tk = bearerTicket(t, env.ts.URL, secret)
	idleTermSession(env)
	ws, _, err = dialTermWSTicket(ctx, env.ts.URL, "local", "whoami", q+"&ticket="+tk, "")
	if err != nil {
		t.Fatal(err)
	}
	if resp := apiCall(t, env.ts.URL, http.MethodDelete, "/api/tokens/"+strconv.FormatInt(tok.ID, 10), env.adminToken, nil); resp.StatusCode/100 != 2 {
		t.Fatalf("delete token: %d", resp.StatusCode)
	}
	expectSessionEnded(t, ws, "revoked API token")

	deadline := time.Now().Add(2 * time.Second)
	for env.srv.live.Len() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := env.srv.live.Len(); n != 0 {
		t.Fatalf("%d live connections left registered", n)
	}
}

func bearerTicket(t *testing.T, baseURL, secret string) string {
	t.Helper()
	b, _ := json.Marshal(terminalTicketReq{Kind: "stack", Host: "local", Stack: "whoami", Container: "whoami-web-1"})
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/terminal/ticket", bytes.NewReader(b))
	req.Close = true
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct{ Ticket string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Ticket == "" {
		t.Fatalf("bearer ticket: %d %v", resp.StatusCode, err)
	}
	return out.Ticket
}

// The registry closes exactly the sockets whose sign-in no longer holds (logs and
// operation streams go through the same path as terminals).
func TestRevalidateLiveClosesOnlyRevoked(t *testing.T) {
	env := newTermTestEnv(t, true, false)
	ctx := context.Background()
	_ = env.db.CreateSession(ctx, sha256Hex("tok-keep"), env.adminID, time.Now().Add(time.Hour))
	_ = env.db.CreateSession(ctx, sha256Hex("tok-drop"), env.viewerID, time.Now().Add(time.Hour))
	var kept, dropped, local atomic.Int32
	reqWith := func(a authRef) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		return r.WithContext(context.WithValue(r.Context(), authRefKey{}, a))
	}
	defer env.srv.trackWS(reqWith(authRef{kind: "session", secret: "tok-keep", userID: env.adminID}), "logs", false, func() { kept.Add(1) })()
	defer env.srv.trackWS(reqWith(authRef{kind: "session", secret: "tok-drop", userID: env.viewerID}), "logs", false, func() { dropped.Add(1) })()
	defer env.srv.trackWS(reqWith(authRef{kind: "local", username: "cli:local"}), "op", false, func() { local.Add(1) })()
	if n := env.srv.revalidateLive(ctx); n != 0 {
		t.Fatalf("closed %d with every sign-in valid", n)
	}
	_ = env.db.DeleteSession(ctx, sha256Hex("tok-drop"))
	if n := env.srv.revalidateLive(ctx); n != 1 {
		t.Fatalf("closed %d, want 1", n)
	}
	deadline := time.Now().Add(time.Second)
	for dropped.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if kept.Load() != 0 || dropped.Load() != 1 || local.Load() != 0 {
		t.Fatalf("kept=%d dropped=%d local=%d", kept.Load(), dropped.Load(), local.Load())
	}
}

// Regression guard: every WebSocket of the web API goes through acceptWS (Origin rules,
// no InsecureSkipVerify, no "*"), so a new endpoint can't silently skip the CSWSH check.
func TestEveryWebSocketAcceptChecksOrigin(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	accepts := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if strings.Contains(src, "InsecureSkipVerify:") {
			t.Errorf("%s: InsecureSkipVerify turns the Origin check off", f)
		}
		if strings.Contains(src, `OriginPatterns: []string{"*"}`) {
			t.Errorf("%s: an Origin pattern of * allows every site", f)
		}
		n := strings.Count(src, "websocket.Accept(")
		if n > 0 && f != "wslive.go" {
			t.Errorf("%s: calls websocket.Accept directly; use s.acceptWS", f)
		}
		accepts += n
	}
	if accepts != 1 {
		t.Fatalf("want exactly one websocket.Accept (in acceptWS), found %d", accepts)
	}
}

// Regression guard: the session cookie is HttpOnly and SameSite=Strict, Secure behind
// HTTPS (also when only the proxy speaks HTTPS), and the security headers stay on.
func TestSessionCookieAndSecurityHeaders(t *testing.T) {
	env := newTermTestEnv(t, true, false)
	check := func(r *http.Request, wantSecure bool) {
		t.Helper()
		w := httptest.NewRecorder()
		env.srv.setCookie(w, r, "tok")
		c := w.Result().Cookies()
		if len(c) != 1 || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode || c[0].Secure != wantSecure || c[0].Path != "/" {
			t.Fatalf("cookie %+v, want HttpOnly SameSite=Strict Secure=%v", c, wantSecure)
		}
	}
	check(httptest.NewRequest(http.MethodPost, "http://moorage.lan/api/login", nil), false)
	r := httptest.NewRequest(http.MethodPost, "http://moorage.lan/api/login", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	check(r, true)
	check(httptest.NewRequest(http.MethodPost, "https://moorage.lan/api/login", nil), true)

	// Sign-out clears it with the same flags.
	w := httptest.NewRecorder()
	lr := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	lr.Header.Set(csrfHeader, "1")
	env.srv.Handler().ServeHTTP(w, lr)
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName && (!c.HttpOnly || c.SameSite != http.SameSiteStrictMode) {
			t.Fatalf("logout cookie %+v", c)
		}
	}

	for _, p := range []string{"/", "/api/session"} {
		w := httptest.NewRecorder()
		env.srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		h := w.Header()
		csp := h.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "script-src 'self'", "connect-src 'self'", "frame-ancestors 'none'", "base-uri 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP %q lacks %q", p, csp, want)
			}
		}
		if strings.Contains(csp, "unsafe-eval") || strings.Contains(strings.SplitN(csp, "script-src", 2)[1], "unsafe-inline") {
			t.Errorf("%s: CSP allows inline or eval scripts: %q", p, csp)
		}
		if h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: missing X-Frame-Options / nosniff: %v", p, h)
		}
	}
}
