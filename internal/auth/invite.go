package auth

import (
	"context"
	"errors"
	"time"

	"moorage/internal/store"
)

// ErrBadLink is the one answer for a wrong, used, revoked or expired invite or reset
// link, so the answer doesn't tell which links exist.
var ErrBadLink = errors.New("this link doesn't work: it is wrong, already used or expired. Ask an admin for a new one")

// InviteInfo is what the invitee sees before choosing a password.
type InviteInfo struct {
	Username    string    `json:"username"`
	DisplayName string    `json:"displayName,omitempty"`
	Role        string    `json:"role"`
	Kind        string    `json:"kind"` // "invite" or "reset"
	Expires     time.Time `json:"expires"`
}

// Links share the per-IP key with password sign-in, so guessing links and passwords
// draw from the same budget. Only wrong attempts count (see limiter.blocked).
func linkKey(ip string) string { return "ip:" + ip }

func (s *Service) openLink(ctx context.Context, ip, token string) (store.UserToken, store.User, error) {
	if s.limiter.blocked(linkKey(ip)) {
		return store.UserToken{}, store.User{}, ErrRateLimited
	}
	t, err := s.db.UserTokenByHash(ctx, hashToken(token))
	if err == nil {
		var u store.User
		u, err = s.db.UserByID(ctx, t.UserID)
		if err == nil && u.AuthSource == store.SourceLocal {
			return t, u, nil
		}
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return t, store.User{}, err
	}
	s.limiter.fail(linkKey(ip))
	return t, store.User{}, ErrBadLink
}

// InviteInspect checks a link and tells who it is for.
func (s *Service) InviteInspect(ctx context.Context, ip, token string) (InviteInfo, error) {
	t, u, err := s.openLink(ctx, ip, token)
	if err != nil {
		return InviteInfo{}, err
	}
	return InviteInfo{Username: u.Username, DisplayName: u.DisplayName, Role: u.Role, Kind: t.Kind, Expires: t.ExpiresAt}, nil
}

// InviteStart takes the new password and returns a new TOTP secret. Nothing changes on
// the account yet: both wait on the link until the code confirms the authenticator.
// TOTP is mandatory for every local user, viewers too: they can read compose files,
// .env files and logs, which often hold secrets.
func (s *Service) InviteStart(ctx context.Context, ip, token, password string) (secret, otpURL string, err error) {
	_, u, err := s.openLink(ctx, ip, token)
	if err != nil {
		return "", "", err
	}
	if err := checkPassword(password); err != nil {
		return "", "", err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return "", "", err
	}
	if secret, err = NewTOTPSecret(); err != nil {
		return "", "", err
	}
	if err := s.db.SetTokenPending(ctx, hashToken(token), hash, secret); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", "", ErrBadLink
		}
		return "", "", err
	}
	return secret, TOTPURL(secret, u.Username), nil
}

// InviteFinish confirms the code, activates the account (or replaces password and TOTP
// for a reset), ends every older session of the user and signs them in.
func (s *Service) InviteFinish(ctx context.Context, c Client, token, code string) (string, User, string, error) {
	t, _, err := s.openLink(ctx, c.IP, token)
	if err != nil {
		return "", User{}, "", err
	}
	if t.PendingTOTPSecret == "" {
		return "", User{}, "", ValidationError{"Choose your password first."}
	}
	counter, ok := matchTOTP(t.PendingTOTPSecret, code, s.now())
	if !ok {
		s.limiter.fail(linkKey(c.IP))
		return "", User{}, "", ValidationError{"That code doesn't match. Check the time on your phone and try the next code."}
	}
	u, err := s.db.ConsumeUserToken(ctx, hashToken(token), counter)
	if errors.Is(err, store.ErrNotFound) { // finished in another tab meanwhile
		return "", User{}, "", ErrBadLink
	}
	if err != nil {
		return "", User{}, "", err
	}
	s.log.Info("link finished", "user", u.Username, "kind", t.Kind)
	if u.Disabled {
		return "", publicUser(u), t.Kind, RefusedError{"Your password is set, but an admin disabled this account. Ask them to enable it."}
	}
	tok, err := s.newSession(ctx, u.ID, c)
	if err != nil {
		return "", User{}, t.Kind, err
	}
	if err := s.db.SetLastLogin(ctx, u.ID); err != nil {
		s.log.Warn("record sign-in", "err", err)
	}
	return tok, publicUser(u), t.Kind, nil
}
