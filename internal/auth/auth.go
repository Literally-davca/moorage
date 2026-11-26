// Package auth handles accounts, first-run setup, TOTP and sessions.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"moorage/internal/store"
)

// SessionTTL is how long a local or forward-auth sign-in lasts. Forward-auth re-checks
// the proxy's header (user and groups) on every request, so its long session is safe.
const SessionTTL = 7 * 24 * time.Hour

// OIDCSessionTTL is shorter: an OIDC role comes from the IdP's groups only at sign-in,
// and signing out at the IdP doesn't end a Moorage session. A user removed from the
// admin group at the IdP loses the role within this time without anyone in Moorage
// disabling them.
const OIDCSessionTTL = 12 * time.Hour

// touchEvery throttles the last-seen write: at most once a minute per session, never on
// every request.
const touchEvery = time.Minute

var (
	ErrInvalidCredentials = errors.New("wrong username, password or code")
	ErrRateLimited        = errors.New("too many attempts, wait a minute and try again")
	ErrSetupClosed        = errors.New("setup is already finished")
	ErrBadSetupToken      = errors.New("wrong setup token, find it in the Moorage log")
	ErrNoSession          = errors.New("not signed in")
)

// ValidationError is a user-facing input problem.
type ValidationError struct{ Msg string }

func (e ValidationError) Error() string { return e.Msg }

// RefusedError is a valid request that the safety rules refuse (last admin, own account,
// a role owned by the IdP). The API answers 409.
type RefusedError struct{ Msg string }

func (e RefusedError) Error() string { return e.Msg }

var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,31}$`)

const usernameHelp = "Username needs 3–32 characters: lowercase letters, digits, dot, dash or underscore."

// User is the public view of an account.
type User struct {
	ID          int64            `json:"id"`
	Username    string           `json:"username"`
	Role        string           `json:"role"`
	Source      string           `json:"source"`
	DisplayName string           `json:"displayName,omitempty"`
	Preferences *UserPreferences `json:"preferences,omitempty"`
}

// Client is where a request comes from, kept on the session for the Account page.
type Client struct {
	IP        string
	UserAgent string
}

// Service implements authentication.
type Service struct {
	db  *store.Store
	log *slog.Logger

	mu         sync.Mutex
	setupToken string // empty once an active user exists

	limiter   *limiter
	pkLimiter *limiter   // passkey sign-in challenges per IP
	cer       ceremonies // open passkey challenges
	dummy     string     // hash used when the user does not exist, keeps timing similar
	ticketKey []byte     // signs TOTP re-enrolment tickets; per process, a restart only voids open ones
	now       func() time.Time
}

// New creates the service and, on first run, prints a one-time setup token to the log.
// The token stops anyone else on the network from claiming a fresh instance.
func New(db *store.Store, log *slog.Logger) (*Service, error) {
	dummy, err := HashPassword("moorage-timing-equaliser")
	if err != nil {
		return nil, err
	}
	s := &Service{db: db, log: log, limiter: newLimiter(5, time.Minute), pkLimiter: newLimiter(30, time.Minute), dummy: dummy, now: time.Now}
	s.ticketKey = make([]byte, 32)
	if _, err := rand.Read(s.ticketKey); err != nil {
		return nil, err
	}
	n, err := db.CountActiveUsers(context.Background())
	if err != nil {
		return nil, err
	}
	if n == 0 {
		s.setupToken = randomHex(12)
		log.Warn("first run: open Moorage in your browser and enter this setup token", "setup_token", s.setupToken)
	}
	return s, nil
}

// SetupRequired reports whether no account exists yet.
func (s *Service) SetupRequired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setupToken != ""
}

func (s *Service) checkSetupToken(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setupToken == "" {
		return ErrSetupClosed
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.setupToken)) != 1 {
		return ErrBadSetupToken
	}
	return nil
}

// SetupStart creates the pending admin and returns its TOTP secret and otpauth URL.
func (s *Service) SetupStart(ctx context.Context, ip, token, username, password string) (secret, otpURL string, err error) {
	if !s.limiter.allow("setup:" + ip) {
		return "", "", ErrRateLimited
	}
	if err := s.checkSetupToken(token); err != nil {
		return "", "", err
	}
	username = strings.ToLower(username)
	if !usernameRe.MatchString(username) {
		return "", "", ValidationError{usernameHelp}
	}
	if err := checkPassword(password); err != nil {
		return "", "", err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return "", "", err
	}
	secret, err = NewTOTPSecret()
	if err != nil {
		return "", "", err
	}
	err = s.db.UpsertPendingUser(ctx, store.User{Username: username, PasswordHash: hash, Role: "admin", TOTPSecret: secret})
	if err != nil {
		return "", "", err
	}
	return secret, TOTPURL(secret, username), nil
}

func checkPassword(password string) error {
	if len([]rune(password)) < MinPasswordLen {
		return ValidationError{fmt.Sprintf("Password needs at least %d characters.", MinPasswordLen)}
	}
	if len(password) > 1024 { // argon2 is cheap enough, but don't hash megabytes
		return ValidationError{"Password is too long."}
	}
	return nil
}

// SetupFinish confirms the TOTP code, activates the admin and signs them in.
func (s *Service) SetupFinish(ctx context.Context, c Client, token, username, code string) (string, User, error) {
	if !s.limiter.allow("setup:" + c.IP) {
		return "", User{}, ErrRateLimited
	}
	if err := s.checkSetupToken(token); err != nil {
		return "", User{}, err
	}
	u, err := s.db.UserByName(ctx, strings.ToLower(username))
	if err != nil || u.Active {
		return "", User{}, ValidationError{"Start the setup again."}
	}
	counter, ok := matchTOTP(u.TOTPSecret, code, s.now())
	if !ok {
		return "", User{}, ValidationError{"That code doesn't match. Check the time on your phone and try the next code."}
	}
	if _, err := s.db.UseTOTPCounter(ctx, u.ID, counter); err != nil {
		return "", User{}, err
	}
	if err := s.db.ActivateUser(ctx, u.ID); err != nil {
		return "", User{}, err
	}
	s.mu.Lock()
	s.setupToken = ""
	s.mu.Unlock()
	s.log.Info("setup finished", "user", u.Username)
	if err := s.db.SetLastLogin(ctx, u.ID); err != nil {
		s.log.Warn("record sign-in", "err", err)
	}
	tok, err := s.newSession(ctx, u.ID, c)
	return tok, publicUser(u), err
}

// Login checks password and TOTP code and returns a session token. Only finished,
// enabled local accounts with TOTP can sign in this way; OIDC and proxy accounts have no
// password. LoginWithPasskey takes a passkey instead of the code.
func (s *Service) Login(ctx context.Context, c Client, username, password, code string) (tok string, pu User, err error) {
	defer func() { s.logSignInFailure(ctx, "password", c.IP, username, err) }()
	u, err := s.passwordStep(ctx, c, username, password)
	if err != nil {
		return "", User{}, err
	}
	counter, ok := matchTOTP(u.TOTPSecret, code, s.now())
	if !ok {
		return "", User{}, ErrInvalidCredentials
	}
	fresh, err := s.db.UseTOTPCounter(ctx, u.ID, counter)
	if err != nil {
		return "", User{}, err
	}
	if !fresh { // the same code was already used
		return "", User{}, ErrInvalidCredentials
	}
	return s.finishLogin(ctx, c, u, "totp")
}

// passwordStep is the first factor of a local sign-in: rate limits, the account and its
// password. Every refusal is the same ErrInvalidCredentials.
func (s *Service) passwordStep(ctx context.Context, c Client, username, password string) (store.User, error) {
	username = strings.ToLower(username)
	if !s.limiter.allow("ip:"+c.IP) || !s.limiter.allow("user:"+username) {
		return store.User{}, ErrRateLimited
	}
	u, err := s.db.UserByName(ctx, username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			VerifyPassword(password, s.dummy) //nolint:errcheck // timing only
			return store.User{}, ErrInvalidCredentials
		}
		return store.User{}, err
	}
	if u.AuthSource != store.SourceLocal || u.PasswordHash == "" {
		VerifyPassword(password, s.dummy) //nolint:errcheck // timing only
		return store.User{}, ErrInvalidCredentials
	}
	ok, err := VerifyPassword(password, u.PasswordHash)
	// An empty TOTP secret would make every code computable; such an account can't sign in.
	if err != nil || !ok || !u.Active || u.Disabled || u.TOTPSecret == "" {
		return store.User{}, ErrInvalidCredentials
	}
	return u, nil
}

// finishLogin creates the session of a sign-in that passed every factor.
func (s *Service) finishLogin(ctx context.Context, c Client, u store.User, method string) (string, User, error) {
	tok, err := s.newSession(ctx, u.ID, c)
	if err != nil {
		return "", User{}, err
	}
	if err := s.db.SetLastLogin(ctx, u.ID); err != nil {
		s.log.Warn("record sign-in", "err", err)
	}
	s.log.Info("signed in", "user", u.Username, "ip", c.IP, "method", method)
	return tok, publicUser(u), nil
}

// SessionUser resolves a session token to its user. The user is read on every request,
// so a role change or a disable applies at once, never the role from sign-in time.
func (s *Service) SessionUser(ctx context.Context, token string) (User, error) {
	_, u, err := s.sessionAndUser(ctx, token)
	if err != nil {
		return User{}, err
	}
	return publicUser(u), nil
}

func (s *Service) sessionAndUser(ctx context.Context, token string) (store.Session, store.User, error) {
	if token == "" {
		return store.Session{}, store.User{}, ErrNoSession
	}
	hash := hashToken(token)
	sess, err := s.db.SessionByHash(ctx, hash)
	if err != nil {
		return sess, store.User{}, ErrNoSession
	}
	u, err := s.db.UserByID(ctx, sess.UserID)
	if err != nil || !u.Active || u.Disabled {
		return sess, store.User{}, ErrNoSession
	}
	if s.now().Sub(sess.LastSeenAt) >= touchEvery {
		if err := s.db.TouchSession(ctx, hash); err != nil {
			s.log.Warn("touch session", "err", err)
		}
	}
	return sess, u, nil
}

// Logout ends a session.
func (s *Service) Logout(ctx context.Context, token string) error {
	return s.db.DeleteSession(ctx, hashToken(token))
}

func (s *Service) newSession(ctx context.Context, userID int64, c Client) (string, error) {
	return s.newSessionTTL(ctx, userID, c, SessionTTL)
}

func (s *Service) newSessionTTL(ctx context.Context, userID int64, c Client, ttl time.Duration) (string, error) {
	tok := randomHex(32)
	if err := s.db.PruneSessions(ctx); err != nil {
		s.log.Warn("prune sessions", "err", err)
	}
	return tok, s.db.CreateSessionMeta(ctx, hashToken(tok), userID, s.now().Add(ttl), c.IP, c.UserAgent)
}

func publicUser(u store.User) User {
	return User{
		ID:          u.ID,
		Username:    u.Username,
		Role:        u.Role,
		Source:      u.AuthSource,
		DisplayName: u.DisplayName,
		Preferences: ParsePreferences(u.PreferencesJSON),
	}
}

// Only the SHA-256 of a session token is stored, so a leaked database can't be used to sign in.
func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

// limiter is a fixed-window attempt counter kept in memory.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string]*window
}

type window struct {
	start time.Time
	n     int
}

func newLimiter(max int, w time.Duration) *limiter {
	return &limiter{max: max, window: w, hits: map[string]*window{}}
}

// allow counts an attempt and reports whether it is within the limit.
func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.current(key)
	w.n++
	return w.n <= l.max
}

// blocked reports whether key used up its attempts, without counting one. Invite links
// use it with fail, so only wrong attempts count and an honest invitee clicking through
// the steps isn't locked out.
func (l *limiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.current(key).n >= l.max
}

// fail counts one wrong attempt.
func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.current(key).n++
}

func (l *limiter) current(key string) *window {
	now := time.Now()
	if len(l.hits) > 10_000 { // bound memory under a flood
		for k, w := range l.hits {
			if now.Sub(w.start) > l.window {
				delete(l.hits, k)
			}
		}
	}
	w, ok := l.hits[key]
	if !ok || now.Sub(w.start) > l.window {
		w = &window{start: now}
		l.hits[key] = w
	}
	return w
}
