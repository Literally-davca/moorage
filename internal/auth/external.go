package auth

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"moorage/internal/store"
)

// Refusals of an external (OIDC or proxy) sign-in. The API turns them into plain words.
var (
	ErrNoAccess        = errors.New("no access")          // no group maps to a role and the default role is none
	ErrAccountDisabled = errors.New("account disabled")   // an admin disabled the account
	ErrSetupFirst      = errors.New("setup not finished") // the local admin must exist first
)

// Identity is a user as an OIDC provider or a trusted proxy names them.
type Identity struct {
	Source            string // store.SourceOIDC or store.SourceProxy
	Issuer            string // the OIDC issuer URL, or "proxy"
	Subject           string // the stable id at the issuer: never the e-mail
	PreferredUsername string
	Email             string
	DisplayName       string
}

// ExternalLogin signs in the user of an external identity and creates the account on
// the first sign-in. role comes from the group mapping at every sign-in; "" refuses.
func (s *Service) ExternalLogin(ctx context.Context, c Client, id Identity, role string) (string, User, error) {
	u, err := s.resolveExternal(ctx, id, role)
	if err != nil {
		return "", User{}, err
	}
	tok, err := s.newSessionTTL(ctx, u.ID, c, OIDCSessionTTL)
	if err != nil {
		return "", User{}, err
	}
	s.log.Info("signed in", "user", u.Username, "source", u.AuthSource, "ip", c.IP)
	return tok, publicUser(u), nil
}

// ProxySession is ExternalLogin for forward-auth, which runs on every request: the
// current session is kept when it belongs to the same user, and dropped when the proxy
// now names someone else. fresh reports a new session (to set the cookie and audit).
func (s *Service) ProxySession(ctx context.Context, c Client, id Identity, role, token string) (string, User, bool, error) {
	u, err := s.resolveExternal(ctx, id, role)
	if err != nil {
		return "", User{}, false, err
	}
	if token != "" {
		if sess, err := s.db.SessionByHash(ctx, hashToken(token)); err == nil {
			if sess.UserID == u.ID {
				return token, publicUser(u), false, nil
			}
			if err := s.db.DeleteSession(ctx, hashToken(token)); err != nil {
				return "", User{}, false, err
			}
		}
	}
	tok, err := s.newSession(ctx, u.ID, c)
	if err != nil {
		return "", User{}, false, err
	}
	if err := s.db.SetLastLogin(ctx, u.ID); err != nil {
		s.log.Warn("record sign-in", "err", err)
	}
	s.log.Info("signed in", "user", u.Username, "source", u.AuthSource, "ip", c.IP)
	return tok, publicUser(u), true, nil
}

// resolveExternal finds the account by (issuer, subject) only, never by name or e-mail,
// which would let an IdP user take over a local account with the same name.
func (s *Service) resolveExternal(ctx context.Context, id Identity, role string) (store.User, error) {
	if s.SetupRequired() {
		return store.User{}, ErrSetupFirst
	}
	if id.Subject == "" || id.Issuer == "" {
		return store.User{}, errors.New("identity without subject")
	}
	u, err := s.db.UserByIdentity(ctx, id.Issuer, id.Subject)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if role == "" {
			return u, ErrNoAccess
		}
		base := externalUsername(id)
		return s.db.CreateExternalUser(ctx, id.Issuer, id.Subject, store.User{
			Role: role, AuthSource: id.Source, DisplayName: clipRunes(id.DisplayName, 64), Email: clipRunes(id.Email, 254),
		}, func(i int) string { return numbered(base, i) })
	case err != nil:
		return u, err
	}
	if u.Disabled {
		return u, ErrAccountDisabled
	}
	if role == "" {
		return u, ErrNoAccess
	}
	name, email := clipRunes(id.DisplayName, 64), clipRunes(id.Email, 254)
	if u.Role != role || u.DisplayName != name || u.Email != email || id.Source == store.SourceOIDC {
		if err := s.db.UpdateExternalLogin(ctx, u.ID, role, name, email); err != nil {
			return u, err
		}
		u.Role, u.DisplayName, u.Email = role, name, email
	}
	return u, nil
}

// externalUsername picks the username of a new external user: preferred_username, then
// the local part of the e-mail, then the subject, sanitised to usernameRe.
func externalUsername(id Identity) string {
	local, _, _ := strings.Cut(id.Email, "@")
	for _, c := range []string{id.PreferredUsername, local, id.Subject} {
		if n := SanitizeUsername(c); n != "" {
			return n
		}
	}
	return "user"
}

// SanitizeUsername maps any name to usernameRe: lowercase, other characters become a
// dash, at most 28 characters (room for a numeric suffix), padded to 3. "" when nothing
// usable is left.
func SanitizeUsername(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.TrimLeft(b.String(), "._-")
	if len(out) > 28 {
		out = out[:28]
	}
	if out == "" {
		return ""
	}
	for len(out) < 3 {
		out += "0"
	}
	return out
}

func numbered(base string, i int) string {
	if i == 0 {
		return base
	}
	return base + "-" + strconv.Itoa(i+1)
}

func clipRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
