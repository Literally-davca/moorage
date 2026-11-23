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
	"sync"
	"time"

	"moorage/internal/store"
)

// SessionTTL is how long a sign-in lasts.
const SessionTTL = 7 * 24 * time.Hour

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

var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,31}$`)

// User is the public view of an account.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

// Service implements authentication.
type Service struct {
	db  *store.Store
	log *slog.Logger

	mu         sync.Mutex
	setupToken string // empty once an active user exists

	limiter *limiter
	dummy   string // hash used when the user does not exist, keeps timing similar
}

// New creates the service and, on first run, prints a one-time setup token to the log.
// The token stops anyone else on the network from claiming a fresh instance.
func New(db *store.Store, log *slog.Logger) (*Service, error) {
	dummy, err := HashPassword("moorage-timing-equaliser")
	if err != nil {
		return nil, err
	}
	s := &Service{db: db, log: log, limiter: newLimiter(5, time.Minute), dummy: dummy}
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
	if !usernameRe.MatchString(username) {
		return "", "", ValidationError{"Username needs 3–32 characters: lowercase letters, digits, dot, dash or underscore."}
	}
	if len([]rune(password)) < MinPasswordLen {
		return "", "", ValidationError{fmt.Sprintf("Password needs at least %d characters.", MinPasswordLen)}
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

// SetupFinish confirms the TOTP code, activates the admin and signs them in.
func (s *Service) SetupFinish(ctx context.Context, ip, token, username, code string) (string, User, error) {
	if !s.limiter.allow("setup:" + ip) {
		return "", User{}, ErrRateLimited
	}
	if err := s.checkSetupToken(token); err != nil {
		return "", User{}, err
	}
	u, err := s.db.UserByName(ctx, username)
	if err != nil || u.Active {
		return "", User{}, ValidationError{"Start the setup again."}
	}
	counter, ok := matchTOTP(u.TOTPSecret, code, time.Now())
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
	tok, err := s.newSession(ctx, u.ID)
	return tok, publicUser(u), err
}

// Login checks password and TOTP code and returns a session token.
func (s *Service) Login(ctx context.Context, ip, username, password, code string) (string, User, error) {
	if !s.limiter.allow("ip:"+ip) || !s.limiter.allow("user:"+username) {
		return "", User{}, ErrRateLimited
	}
	u, err := s.db.UserByName(ctx, username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			VerifyPassword(password, s.dummy) //nolint:errcheck // timing only
			return "", User{}, ErrInvalidCredentials
		}
		return "", User{}, err
	}
	ok, err := VerifyPassword(password, u.PasswordHash)
	if err != nil || !ok || !u.Active {
		return "", User{}, ErrInvalidCredentials
	}
	counter, ok := matchTOTP(u.TOTPSecret, code, time.Now())
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
	tok, err := s.newSession(ctx, u.ID)
	if err != nil {
		return "", User{}, err
	}
	s.log.Info("signed in", "user", u.Username, "ip", ip)
	return tok, publicUser(u), nil
}

// SessionUser resolves a session token to its user.
func (s *Service) SessionUser(ctx context.Context, token string) (User, error) {
	if token == "" {
		return User{}, ErrNoSession
	}
	sess, err := s.db.SessionByHash(ctx, hashToken(token))
	if err != nil {
		return User{}, ErrNoSession
	}
	u, err := s.db.UserByID(ctx, sess.UserID)
	if err != nil || !u.Active {
		return User{}, ErrNoSession
	}
	return publicUser(u), nil
}

// Logout ends a session.
func (s *Service) Logout(ctx context.Context, token string) error {
	return s.db.DeleteSession(ctx, hashToken(token))
}

func (s *Service) newSession(ctx context.Context, userID int64) (string, error) {
	tok := randomHex(32)
	if err := s.db.PruneSessions(ctx); err != nil {
		s.log.Warn("prune sessions", "err", err)
	}
	return tok, s.db.CreateSession(ctx, hashToken(tok), userID, time.Now().Add(SessionTTL))
}

func publicUser(u store.User) User { return User{ID: u.ID, Username: u.Username, Role: u.Role} }

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

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
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
		l.hits[key] = &window{start: now, n: 1}
		return true
	}
	w.n++
	return w.n <= l.max
}
