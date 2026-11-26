package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"moorage/internal/store"
)

// totpTicketTTL is how long the second step of a TOTP re-enrolment may wait.
const totpTicketTTL = 10 * time.Minute

var errNotLocal = RefusedError{"You sign in through your identity provider, so Moorage has no password or two-factor code for you."}

// SessionInfo is one of the user's own sessions on the Account page. The token and its
// hash never leave the server.
type SessionInfo struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	LastSeen  time.Time `json:"lastSeen"`
	ExpiresAt time.Time `json:"expiresAt"`
	IP        string    `json:"ip"`
	UserAgent string    `json:"userAgent"`
	Current   bool      `json:"current"`
}

// Account is the Account page.
type Account struct {
	User     User          `json:"user"`
	Email    string        `json:"email,omitempty"`
	TOTP     bool          `json:"totp"`
	Sessions []SessionInfo `json:"sessions"`
	Passkeys []PasskeyInfo `json:"passkeys"`
}

// Account returns the signed-in user's own account and sessions.
func (s *Service) Account(ctx context.Context, token string) (Account, error) {
	sess, u, err := s.sessionAndUser(ctx, token)
	if err != nil {
		return Account{}, err
	}
	list, err := s.db.UserSessions(ctx, u.ID)
	if err != nil {
		return Account{}, err
	}
	out := Account{User: publicUser(u), Email: u.Email, TOTP: u.TOTPSecret != "", Sessions: make([]SessionInfo, 0, len(list))}
	for _, x := range list {
		out.Sessions = append(out.Sessions, SessionInfo{
			ID: x.ID, CreatedAt: x.CreatedAt, LastSeen: x.LastSeenAt, ExpiresAt: x.ExpiresAt,
			IP: x.IP, UserAgent: x.UserAgent, Current: x.ID == sess.ID,
		})
	}
	if out.Passkeys, err = s.passkeyInfos(ctx, u.ID); err != nil {
		return Account{}, err
	}
	return out, nil
}

// localAccount resolves the session to a local user and checks their current password.
// Wrong passwords count against a per-user limit, so a stolen session can't be used to
// guess the password.
func (s *Service) localAccount(ctx context.Context, token, password string) (store.User, error) {
	_, u, err := s.sessionAndUser(ctx, token)
	if err != nil {
		return u, err
	}
	if u.AuthSource != store.SourceLocal {
		return u, errNotLocal
	}
	key := "acct:" + strconv.FormatInt(u.ID, 10)
	if s.limiter.blocked(key) {
		return u, ErrRateLimited
	}
	if ok, err := VerifyPassword(password, u.PasswordHash); err != nil || !ok {
		s.limiter.fail(key)
		return u, ValidationError{"Your current password is wrong."}
	}
	return u, nil
}

// ChangePassword sets a new password and signs out every other session.
func (s *Service) ChangePassword(ctx context.Context, token, current, next string) error {
	u, err := s.localAccount(ctx, token, current)
	if err != nil {
		return err
	}
	if err := checkPassword(next); err != nil {
		return err
	}
	hash, err := HashPassword(next)
	if err != nil {
		return err
	}
	return s.db.SetPassword(ctx, u.ID, hash, hashToken(token))
}

// TOTPEnrolment is the first step of a TOTP re-enrolment. The ticket proves the second
// step belongs to a first step with the right password and current code; the server keeps
// no state between the two.
type TOTPEnrolment struct {
	Secret     string `json:"secret"`
	OTPAuthURL string `json:"otpauthUrl"`
	Ticket     string `json:"ticket"`
}

// TOTPStart checks the password and the current code and returns a new secret.
func (s *Service) TOTPStart(ctx context.Context, token, password, code string) (TOTPEnrolment, error) {
	u, err := s.localAccount(ctx, token, password)
	if err != nil {
		return TOTPEnrolment{}, err
	}
	counter, ok := matchTOTP(u.TOTPSecret, code, s.now())
	fresh := false
	if ok {
		if fresh, err = s.db.UseTOTPCounter(ctx, u.ID, counter); err != nil {
			return TOTPEnrolment{}, err
		}
	}
	if !fresh {
		s.limiter.fail("acct:" + strconv.FormatInt(u.ID, 10))
		return TOTPEnrolment{}, ValidationError{"That code from your current authenticator doesn't match. Wait for the next one."}
	}
	secret, err := NewTOTPSecret()
	if err != nil {
		return TOTPEnrolment{}, err
	}
	exp := s.now().Add(totpTicketTTL).Unix()
	return TOTPEnrolment{Secret: secret, OTPAuthURL: TOTPURL(secret, u.Username), Ticket: s.ticket(u.ID, secret, exp)}, nil
}

func (s *Service) ticket(userID int64, secret string, exp int64) string {
	mac := hmac.New(sha256.New, s.ticketKey)
	fmt.Fprintf(mac, "totp|%d|%s|%d", userID, secret, exp)
	return strconv.FormatInt(exp, 10) + "." + hex.EncodeToString(mac.Sum(nil))
}

func (s *Service) checkTicket(userID int64, secret, ticket string) bool {
	expS, _, ok := strings.Cut(ticket, ".")
	exp, err := strconv.ParseInt(expS, 10, 64)
	if !ok || err != nil || s.now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(ticket), []byte(s.ticket(userID, secret, exp)))
}

// TOTPFinish confirms the new authenticator, replaces the secret and signs out every
// other session (a lost phone is the usual reason to re-enrol).
func (s *Service) TOTPFinish(ctx context.Context, token, secret, ticket, code string) error {
	_, u, err := s.sessionAndUser(ctx, token)
	if err != nil {
		return err
	}
	if u.AuthSource != store.SourceLocal {
		return errNotLocal
	}
	if !s.checkTicket(u.ID, secret, ticket) {
		return ValidationError{"This step expired. Start again."}
	}
	counter, ok := matchTOTP(secret, code, s.now())
	if !ok {
		return ValidationError{"That code doesn't match. Check the time on your phone and try the next code."}
	}
	if err := s.db.SetTOTPSecret(ctx, u.ID, secret, counter); err != nil {
		return err
	}
	_, err = s.db.DeleteUserSessions(ctx, u.ID, hashToken(token))
	return err
}

// ErrSessionNotFound is a session id that isn't one of the user's.
var ErrSessionNotFound = errors.New("session not found")

// RevokeSession signs out one of the user's own sessions.
func (s *Service) RevokeSession(ctx context.Context, token string, id int64) error {
	_, u, err := s.sessionAndUser(ctx, token)
	if err != nil {
		return err
	}
	if err := s.db.DeleteUserSession(ctx, u.ID, id); errors.Is(err, store.ErrNotFound) {
		return ErrSessionNotFound
	} else if err != nil {
		return err
	}
	return nil
}

// RevokeOtherSessions signs out every session of the user but this one.
func (s *Service) RevokeOtherSessions(ctx context.Context, token string) (int64, error) {
	_, u, err := s.sessionAndUser(ctx, token)
	if err != nil {
		return 0, err
	}
	return s.db.DeleteUserSessions(ctx, u.ID, hashToken(token))
}

// SetPreferences saves normalized UX preferences for the signed-in user.
func (s *Service) SetPreferences(ctx context.Context, token string, prefs UserPreferences) (UserPreferences, error) {
	_, u, err := s.sessionAndUser(ctx, token)
	if err != nil {
		return UserPreferences{}, err
	}
	norm := prefs.Normalize()
	b, err := json.Marshal(norm)
	if err != nil {
		return UserPreferences{}, err
	}
	if err := s.db.SetUserPreferences(ctx, u.ID, string(b)); err != nil {
		return UserPreferences{}, err
	}
	return norm, nil
}
