package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"moorage/internal/auth"
)

// ---- Origin rules for every WebSocket upgrade -------------------------------
//
// Cross-site WebSocket hijacking (CSWSH: Dozzle, Nginx-UI) works because a browser sends
// cookies with a WebSocket upgrade from any site. Every websocket.Accept in this package
// goes through acceptWS, which keeps coder/websocket's own check (the Origin host must be
// the request Host) and adds only two more hosts: the host of MOORAGE_PUBLIC_URL (the
// address users open) and X-Forwarded-Host when the request comes from a trusted proxy
// (MOORAGE_TRUSTED_PROXIES, the same rule as forward-auth). Never InsecureSkipVerify, never
// "*". api_ws_origin_test.go keeps it that way.

// wsOriginHosts are the extra Origin hosts allowed for this request.
func (s *Server) wsOriginHosts(r *http.Request) []string {
	var out []string
	add := func(h string) {
		h = strings.ToLower(strings.TrimSpace(h))
		// The patterns go to path.Match: a host never has glob characters, so one that
		// does is refused instead of widening the rule.
		if h == "" || strings.ContainsAny(h, "*?[]\\/ ,") {
			return
		}
		for _, x := range out {
			if x == h {
				return
			}
		}
		out = append(out, h)
	}
	if pub := s.effectivePublicURL(r.Context()); pub != "" {
		if u, err := url.Parse(pub); err == nil {
			add(u.Host)
		}
	}
	if s.proxy != nil && s.proxy.Trusted(r) {
		// A proxy chain lists several hosts; the first one is what the browser used.
		if fh, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ","); fh != "" {
			add(fh)
		}
	}
	return out
}

// acceptWS upgrades to a WebSocket with the Origin rules above. On a refused Origin
// coder/websocket answers 403 itself and err is set.
func (s *Server) acceptWS(w http.ResponseWriter, r *http.Request, mode websocket.CompressionMode) (*websocket.Conn, error) {
	return websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: mode,
		OriginPatterns:  s.wsOriginHosts(r),
	})
}

// ---- how a request signed in -------------------------------------------------

type authRefKey struct{}

// authRef is how a request was authenticated, kept so a long-lived WebSocket can be
// checked again later (sign-out, password change, role change, revoked token) without
// the request's cookie, which a forward-auth session may never have had.
type authRef struct {
	kind     string // "session", "bearer" or "local"
	secret   string // session token or API token secret
	userID   int64
	username string
}

func authRefOf(r *http.Request) authRef {
	a, _ := r.Context().Value(authRefKey{}).(authRef)
	return a
}

// stillAllowed tells whether a's sign-in is still valid and, when admin is set, still an
// admin. Any doubt (a database error included) ends the connection.
func (s *Server) stillAllowed(ctx context.Context, a authRef, admin bool) bool {
	switch a.kind {
	case "local":
		return true
	case "bearer":
		if s.store == nil {
			return false
		}
		tok, err := s.store.LookupAPITokenByHash(ctx, hashAPIToken(a.secret))
		if err != nil || (!tok.ExpiresAt.IsZero() && time.Now().After(tok.ExpiresAt)) {
			return false
		}
		return !admin || tok.Role == "admin"
	case "session":
		if s.auth == nil {
			return false
		}
		u, err := s.auth.SessionUser(ctx, a.secret)
		if err != nil || u.ID != a.userID {
			return false
		}
		return !admin || u.Role == "admin"
	}
	return false
}

// ---- live WebSockets ----------------------------------------------------------

// closeSessionEnded is the close code of a WebSocket whose sign-in was revoked; the UI
// shows "Session ended, sign in again".
const closeSessionEnded websocket.StatusCode = 4401

const sessionEndedReason = "Session ended, sign in again."

type liveConn struct {
	auth  authRef
	admin bool   // the route needs an admin
	kind  string // logs, op, terminal, host-terminal
	end   func() // closes the socket with closeSessionEnded
}

// liveConns is every open WebSocket (logs, operations, terminals) by its sign-in, so a
// revocation can close them at once instead of waiting for the next periodic check.
type liveConns struct {
	mu    sync.Mutex
	next  uint64
	conns map[uint64]liveConn
}

// add registers a socket; the returned func removes it again (call it when it closes).
func (l *liveConns) add(c liveConn) func() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conns == nil {
		l.conns = map[uint64]liveConn{}
	}
	l.next++
	id := l.next
	l.conns[id] = c
	return func() {
		l.mu.Lock()
		delete(l.conns, id)
		l.mu.Unlock()
	}
}

func (l *liveConns) snapshot() map[uint64]liveConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[uint64]liveConn, len(l.conns))
	for k, v := range l.conns {
		out[k] = v
	}
	return out
}

// Len is the number of open WebSockets (tests, metrics).
func (l *liveConns) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.conns)
}

// trackWS registers a WebSocket of request r. end must close the socket with
// closeSessionEnded; it runs at most once, from revalidateLive.
func (s *Server) trackWS(r *http.Request, kind string, admin bool, end func()) func() {
	var once sync.Once
	return s.live.add(liveConn{auth: authRefOf(r), admin: admin, kind: kind, end: func() { once.Do(end) }})
}

// revalidateLive checks every open WebSocket against the store and closes the ones whose
// sign-in no longer holds. It runs after every change that can revoke access: sign-out,
// password change, user removed, disabled or demoted, sessions or an API token revoked.
// It returns how many sockets it closed.
func (s *Server) revalidateLive(ctx context.Context) int {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	n := 0
	for id, c := range s.live.snapshot() {
		if s.stillAllowed(ctx, c.auth, c.admin) {
			continue
		}
		n++
		s.log.Debug("closing live connection", "kind", c.kind, "user", c.auth.username, "id", id)
		go c.end()
	}
	return n
}

// revokes wraps a handler that can revoke access: after a successful answer (2xx) every
// open WebSocket is checked again, so the affected ones close now.
func (s *Server) revokes(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h(rec, r)
		if rec.status >= 200 && rec.status < 300 {
			s.revalidateLive(r.Context())
		}
	}
}

// statusRecorder remembers the status code a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// ---- one-time terminal tickets --------------------------------------------------

// ticketTTL is how long a terminal ticket can be used.
const ticketTTL = 30 * time.Second

// maxTickets bounds the open tickets (each admin click makes one).
const maxTickets = 1000

type termTicket struct {
	auth   authRef
	user   auth.User
	target string
	exp    time.Time
}

// termTickets are one-time tickets for terminal WebSockets. POST /api/terminal/ticket is an
// ordinary signed-in API call (cookie, OIDC, forward-auth or token), so the upgrade itself
// needs no cookie: it presents ?ticket=, which works once, for 30 s, for one user and
// session and one target.
type termTickets struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[string]termTicket
}

func (t *termTickets) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func (t *termTickets) issue(a authRef, u auth.User, target string) (string, time.Time) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clock()
	if t.m == nil {
		t.m = map[string]termTicket{}
	}
	for k, v := range t.m {
		if now.After(v.exp) {
			delete(t.m, k)
		}
	}
	if len(t.m) >= maxTickets {
		// Drop the oldest; a flood of tickets only ever hurts its own clicks.
		var oldK string
		var oldT time.Time
		for k, v := range t.m {
			if oldK == "" || v.exp.Before(oldT) {
				oldK, oldT = k, v.exp
			}
		}
		delete(t.m, oldK)
	}
	exp := now.Add(ticketTTL)
	t.m[tok] = termTicket{auth: a, user: u, target: target, exp: exp}
	return tok, exp
}

// redeem uses a ticket up. It is gone after the first try, also a wrong one, so a ticket
// that leaked into a log is useless.
func (t *termTickets) redeem(tok, target string) (termTicket, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	v, ok := t.m[tok]
	if !ok || tok == "" {
		return termTicket{}, false
	}
	delete(t.m, tok)
	if t.clock().After(v.exp) || v.target != target {
		return termTicket{}, false
	}
	return v, true
}

// Terminal targets. A ticket opens exactly one of them.
func stackTermTarget(hostID, stack, container string) string {
	return "stack\x00" + hostID + "\x00" + stack + "\x00" + container
}
func containerTermTarget(hostID, id string) string { return "container\x00" + hostID + "\x00" + id }
func hostTermTarget(hostID string) string          { return "host\x00" + hostID }
