package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"moorage/internal/store"
)

// Passkeys (WebAuthn) are a second factor next to TOTP, never instead of it: every local
// account keeps its TOTP secret, so the break-glass rule (an active local admin with TOTP)
// and the admin reset link work unchanged, and losing every passkey locks no one out.
// A passkey the user marked as "passwordless" also signs in on its own, but only when the
// authenticator verified the user (PIN or biometrics).

const (
	// passkeyCeremonyTTL is how long a browser may take between the options and the answer.
	passkeyCeremonyTTL = 5 * time.Minute
	// maxCeremonies bounds the open challenges kept in memory.
	maxCeremonies = 10_000
	// maxPasskeys is the most passkeys one account may register.
	maxPasskeys = 10
	// passkeyHandleLen is the size of the random WebAuthn user handle.
	passkeyHandleLen = 32
)

// ErrPasskeyNotFound is a passkey id that isn't one of the user's.
var ErrPasskeyNotFound = errors.New("passkey not found")

var errCeremonyExpired = ValidationError{"The passkey prompt expired. Try again."}

// RelyingParty is the site passkeys are bound to.
type RelyingParty struct {
	ID     string // the host name, e.g. moorage.example.com
	Origin string // scheme, host and port, e.g. https://moorage.example.com
}

// RelyingPartyFor derives the passkey relying party from the configured public URL; the
// Host header of a request is never used, so a request can't choose the site its
// challenge is bound to. The rule (also in docs/security/threat-model.md):
//
//   - the public URL (MOORAGE_PUBLIC_URL or Settings → General) must be set;
//   - it must use https, or http only for localhost and *.localhost (browsers allow
//     WebAuthn only in a secure context);
//   - its host must be a domain name, not an IP address (WebAuthn has no IP relying parties);
//   - the relying party id is the host name without port, the only allowed origin is the
//     scheme, host and port of the URL. A path in the URL is ignored.
//
// Changing the host of the public URL makes the existing passkeys unusable (they stay
// bound to the old host); TOTP keeps working and the user registers them again.
func RelyingPartyFor(publicURL string) (RelyingParty, error) {
	publicURL = strings.TrimSpace(publicURL)
	if publicURL == "" {
		return RelyingParty{}, RefusedError{"Passkeys need the public URL of Moorage. An admin sets it in Settings → General (or MOORAGE_PUBLIC_URL)."}
	}
	u, err := url.Parse(publicURL)
	if err != nil || u.Host == "" || u.User != nil {
		return RelyingParty{}, RefusedError{"Passkeys need a valid public URL of Moorage, like https://moorage.example.com."}
	}
	host := strings.ToLower(u.Hostname())
	scheme := strings.ToLower(u.Scheme)
	localhost := host == "localhost" || strings.HasSuffix(host, ".localhost")
	switch {
	case net.ParseIP(host) != nil:
		return RelyingParty{}, RefusedError{"Passkeys don't work with an IP address. Open Moorage through a domain name and set it as the public URL."}
	case scheme == "https", scheme == "http" && localhost:
	default:
		return RelyingParty{}, RefusedError{"Passkeys need HTTPS. Set an https:// public URL for Moorage."}
	}
	if err := protocol.ValidateRPID(host); err != nil {
		return RelyingParty{}, RefusedError{"Passkeys need a valid host name in the public URL."}
	}
	origin := scheme + "://" + host
	if p := u.Port(); p != "" && !(scheme == "https" && p == "443") && !(scheme == "http" && p == "80") {
		origin += ":" + p
	}
	return RelyingParty{ID: host, Origin: origin}, nil
}

func newWebAuthn(rp RelyingParty) (*webauthn.WebAuthn, error) {
	t := webauthn.TimeoutConfig{Enforce: true, Timeout: passkeyCeremonyTTL, TimeoutUVD: passkeyCeremonyTTL}
	return webauthn.New(&webauthn.Config{
		RPID:          rp.ID,
		RPDisplayName: "Moorage",
		RPOrigins:     []string{rp.Origin},
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			UserVerification: protocol.VerificationPreferred,
		},
		Timeouts: webauthn.TimeoutsConfig{Login: t, Registration: t},
	})
}

// passkeyUser adapts an account to the webauthn.User interface.
type passkeyUser struct {
	handle  []byte
	name    string
	display string
	creds   []webauthn.Credential
}

func (u *passkeyUser) WebAuthnID() []byte                         { return u.handle }
func (u *passkeyUser) WebAuthnName() string                       { return u.name }
func (u *passkeyUser) WebAuthnDisplayName() string                { return u.display }
func (u *passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

var b64url = base64.RawURLEncoding

// loadPasskeyUser builds the WebAuthn view of an account from its stored passkeys. A user
// without passkeys gets a fresh random handle (stored with the first passkey).
func (s *Service) loadPasskeyUser(ctx context.Context, u store.User) (*passkeyUser, []store.Passkey, error) {
	rows, err := s.db.UserPasskeys(ctx, u.ID)
	if err != nil {
		return nil, nil, err
	}
	pu := &passkeyUser{name: u.Username, display: u.DisplayName}
	if pu.display == "" {
		pu.display = u.Username
	}
	for _, r := range rows {
		var c webauthn.Credential
		if err := json.Unmarshal([]byte(r.DataJSON), &c); err != nil {
			s.log.Warn("passkey record unreadable", "user", u.Username, "id", r.ID, "err", err)
			continue
		}
		pu.creds = append(pu.creds, c)
		if pu.handle == nil {
			if h, err := b64url.DecodeString(r.UserHandle); err == nil {
				pu.handle = h
			}
		}
	}
	if pu.handle == nil {
		pu.handle = make([]byte, passkeyHandleLen)
		if _, err := rand.Read(pu.handle); err != nil {
			return nil, nil, err
		}
	}
	return pu, rows, nil
}

// ---- ceremonies --------------------------------------------------------------

const (
	ceremonyRegister = "register"
	ceremonyLogin    = "login"
)

// ceremony is the server half of one WebAuthn exchange. It lives only in memory: the
// browser gets an opaque id, never the challenge state, and a restart voids open ones.
type ceremony struct {
	kind    string
	userID  int64  // register: the account the passkey is for
	handle  []byte // register: the user handle sent to the authenticator
	rp      RelyingParty
	data    webauthn.SessionData
	expires time.Time
}

type ceremonies struct {
	mu sync.Mutex
	m  map[string]ceremony
}

func (c *ceremonies) put(cer ceremony, now time.Time) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]ceremony{}
	}
	if len(c.m) >= maxCeremonies {
		for k, v := range c.m {
			if now.After(v.expires) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= maxCeremonies {
			return "", ErrRateLimited
		}
	}
	id := randomHex(24)
	cer.expires = now.Add(passkeyCeremonyTTL)
	c.m[id] = cer
	return id, nil
}

// take removes and returns a ceremony: each challenge is answered at most once.
func (c *ceremonies) take(id, kind string, now time.Time) (ceremony, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cer, ok := c.m[id]
	if !ok {
		return ceremony{}, false
	}
	delete(c.m, id)
	if cer.kind != kind || now.After(cer.expires) {
		return ceremony{}, false
	}
	return cer, true
}

// PasskeyCeremony is the first step of a passkey registration or sign-in: the browser
// passes Options to navigator.credentials and sends the answer back with Ceremony.
type PasskeyCeremony struct {
	Ceremony string `json:"ceremony"`
	Options  any    `json:"options"`
}

// PasskeyAssertion is the browser's answer to a sign-in challenge.
type PasskeyAssertion struct {
	Ceremony string          `json:"ceremony"`
	Response json.RawMessage `json:"response"`
}

// PasskeyInfo is one passkey on the Account page. Keys and counters never leave the server.
type PasskeyInfo struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	RPID         string `json:"rpId"`
	Passwordless bool   `json:"passwordless"`
	// CanPasswordless: the authenticator verified the user (PIN or biometrics) when the
	// passkey was registered, so it may be used without a password.
	CanPasswordless bool       `json:"canPasswordless"`
	Synced          bool       `json:"synced"` // backup eligible: a synced passkey (password manager, iCloud, Google)
	CreatedAt       time.Time  `json:"createdAt"`
	LastUsedAt      *time.Time `json:"lastUsedAt,omitempty"`
}

func passkeyInfo(p store.Passkey) PasskeyInfo {
	i := PasskeyInfo{ID: p.ID, Name: p.Name, RPID: p.RPID, Passwordless: p.Passwordless, CreatedAt: p.CreatedAt}
	var c webauthn.Credential
	if json.Unmarshal([]byte(p.DataJSON), &c) == nil {
		i.CanPasswordless = c.Flags.UserVerified
		i.Synced = c.Flags.BackupEligible
	}
	if !p.LastUsedAt.IsZero() {
		t := p.LastUsedAt
		i.LastUsedAt = &t
	}
	return i
}

func (s *Service) passkeyInfos(ctx context.Context, userID int64) ([]PasskeyInfo, error) {
	rows, err := s.db.UserPasskeys(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]PasskeyInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, passkeyInfo(r))
	}
	return out, nil
}

// ---- registration ------------------------------------------------------------

// PasskeyRegisterStart checks the password and a current TOTP code (like a TOTP
// re-enrolment: a stolen session alone can't add a factor) and returns the options for
// navigator.credentials.create.
func (s *Service) PasskeyRegisterStart(ctx context.Context, token, password, code string, rp RelyingParty) (PasskeyCeremony, error) {
	u, err := s.localAccount(ctx, token, password)
	if err != nil {
		return PasskeyCeremony{}, err
	}
	counter, ok := matchTOTP(u.TOTPSecret, code, s.now())
	fresh := false
	if ok {
		if fresh, err = s.db.UseTOTPCounter(ctx, u.ID, counter); err != nil {
			return PasskeyCeremony{}, err
		}
	}
	if !fresh {
		s.limiter.fail("acct:" + strconv.FormatInt(u.ID, 10))
		return PasskeyCeremony{}, ValidationError{"That code from your authenticator app doesn't match. Wait for the next one."}
	}
	pu, rows, err := s.loadPasskeyUser(ctx, u)
	if err != nil {
		return PasskeyCeremony{}, err
	}
	if len(rows) >= maxPasskeys {
		return PasskeyCeremony{}, ValidationError{"You have the most passkeys an account can have (10). Remove one first."}
	}
	wa, err := newWebAuthn(rp)
	if err != nil {
		return PasskeyCeremony{}, err
	}
	creation, data, err := wa.BeginRegistration(pu,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(webauthn.Credentials(pu.creds).CredentialDescriptors()),
	)
	if err != nil {
		return PasskeyCeremony{}, err
	}
	id, err := s.cer.put(ceremony{kind: ceremonyRegister, userID: u.ID, handle: pu.handle, rp: rp, data: *data}, s.now())
	if err != nil {
		return PasskeyCeremony{}, err
	}
	return PasskeyCeremony{Ceremony: id, Options: creation}, nil
}

// PasskeyRegisterFinish verifies the authenticator's answer and stores the passkey.
func (s *Service) PasskeyRegisterFinish(ctx context.Context, token, ceremonyID, name string, passwordless bool, response []byte) (PasskeyInfo, error) {
	_, u, err := s.sessionAndUser(ctx, token)
	if err != nil {
		return PasskeyInfo{}, err
	}
	if u.AuthSource != store.SourceLocal {
		return PasskeyInfo{}, errNotLocal
	}
	name, err = passkeyName(name)
	if err != nil {
		return PasskeyInfo{}, err
	}
	cer, ok := s.cer.take(ceremonyID, ceremonyRegister, s.now())
	if !ok || cer.userID != u.ID {
		return PasskeyInfo{}, errCeremonyExpired
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(response)
	if err != nil {
		return PasskeyInfo{}, ValidationError{"Your browser's answer couldn't be read. Try again."}
	}
	pu, _, err := s.loadPasskeyUser(ctx, u)
	if err != nil {
		return PasskeyInfo{}, err
	}
	pu.handle = cer.handle
	wa, err := newWebAuthn(cer.rp)
	if err != nil {
		return PasskeyInfo{}, err
	}
	cred, err := wa.CreateCredential(pu, cer.data, parsed)
	if err != nil {
		s.log.Info("passkey registration refused", "user", u.Username, "err", err)
		return PasskeyInfo{}, ValidationError{"The passkey couldn't be verified. Try again."}
	}
	if passwordless && !cred.Flags.UserVerified {
		return PasskeyInfo{}, ValidationError{"This authenticator didn't verify you with a PIN or biometrics, so it can only be a second factor. Register it without passwordless sign-in."}
	}
	data, err := json.Marshal(cred)
	if err != nil {
		return PasskeyInfo{}, err
	}
	p := store.Passkey{
		UserID: u.ID, CredentialID: b64url.EncodeToString(cred.ID), UserHandle: b64url.EncodeToString(cer.handle),
		RPID: cer.rp.ID, Name: name, DataJSON: string(data), Passwordless: passwordless,
	}
	id, err := s.db.AddPasskey(ctx, p)
	if errors.Is(err, store.ErrPasskeyTaken) {
		return PasskeyInfo{}, ValidationError{"This passkey is already registered."}
	}
	if err != nil {
		return PasskeyInfo{}, err
	}
	p.ID, p.CreatedAt = id, s.now()
	s.log.Info("passkey added", "user", u.Username, "passwordless", passwordless)
	return passkeyInfo(p), nil
}

func passkeyName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Passkey"
	}
	if len([]rune(name)) > 64 {
		return "", ValidationError{"A passkey name can have at most 64 characters."}
	}
	return name, nil
}

// PasskeyUpdate renames one of the user's passkeys and turns passwordless sign-in on or off.
func (s *Service) PasskeyUpdate(ctx context.Context, token string, id int64, name string, passwordless bool) (PasskeyInfo, error) {
	p, err := s.ownPasskey(ctx, token, id)
	if err != nil {
		return PasskeyInfo{}, err
	}
	if name, err = passkeyName(name); err != nil {
		return PasskeyInfo{}, err
	}
	if passwordless && !passkeyInfo(p).CanPasswordless {
		return PasskeyInfo{}, ValidationError{"This passkey didn't verify you with a PIN or biometrics when it was added, so it can only be a second factor."}
	}
	if err := s.db.SetPasskeyOptions(ctx, p.UserID, id, name, passwordless); errors.Is(err, store.ErrNotFound) {
		return PasskeyInfo{}, ErrPasskeyNotFound
	} else if err != nil {
		return PasskeyInfo{}, err
	}
	p.Name, p.Passwordless = name, passwordless
	return passkeyInfo(p), nil
}

// PasskeyDelete removes one of the user's passkeys. TOTP always stays, so removing the
// last passkey can't lock anyone out.
func (s *Service) PasskeyDelete(ctx context.Context, token string, id int64) (PasskeyInfo, error) {
	p, err := s.ownPasskey(ctx, token, id)
	if err != nil {
		return PasskeyInfo{}, err
	}
	if err := s.db.DeletePasskey(ctx, p.UserID, id); errors.Is(err, store.ErrNotFound) {
		return PasskeyInfo{}, ErrPasskeyNotFound
	} else if err != nil {
		return PasskeyInfo{}, err
	}
	return passkeyInfo(p), nil
}

func (s *Service) ownPasskey(ctx context.Context, token string, id int64) (store.Passkey, error) {
	_, u, err := s.sessionAndUser(ctx, token)
	if err != nil {
		return store.Passkey{}, err
	}
	rows, err := s.db.UserPasskeys(ctx, u.ID)
	if err != nil {
		return store.Passkey{}, err
	}
	for _, r := range rows {
		if r.ID == id {
			return r, nil
		}
	}
	return store.Passkey{}, ErrPasskeyNotFound
}

// ---- sign-in -----------------------------------------------------------------

// PasskeyLoginOptions starts a sign-in with a passkey: a discoverable challenge, so the
// answer doesn't tell which accounts or passkeys exist. The same challenge serves the
// second factor after a password and the passwordless sign-in.
func (s *Service) PasskeyLoginOptions(ip string, rp RelyingParty) (PasskeyCeremony, error) {
	if !s.pkLimiter.allow("pkopt:" + ip) {
		return PasskeyCeremony{}, ErrRateLimited
	}
	wa, err := newWebAuthn(rp)
	if err != nil {
		return PasskeyCeremony{}, err
	}
	assertion, data, err := wa.BeginDiscoverableLogin()
	if err != nil {
		return PasskeyCeremony{}, err
	}
	id, err := s.cer.put(ceremony{kind: ceremonyLogin, rp: rp, data: *data}, s.now())
	if err != nil {
		return PasskeyCeremony{}, err
	}
	return PasskeyCeremony{Ceremony: id, Options: assertion}, nil
}

// verifyAssertion checks a sign-in answer and returns the passkey that signed it and
// whether the authenticator verified the user. It stores the new sign counter.
func (s *Service) verifyAssertion(ctx context.Context, a PasskeyAssertion) (store.Passkey, bool, error) {
	cer, ok := s.cer.take(a.Ceremony, ceremonyLogin, s.now())
	if !ok {
		return store.Passkey{}, false, errCeremonyExpired
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(a.Response)
	if err != nil {
		return store.Passkey{}, false, ErrInvalidCredentials
	}
	var row store.Passkey
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		p, err := s.db.PasskeyByCredentialID(ctx, b64url.EncodeToString(rawID))
		if err != nil {
			return nil, err
		}
		if p.UserHandle != b64url.EncodeToString(userHandle) {
			return nil, errors.New("user handle mismatch")
		}
		u, err := s.db.UserByID(ctx, p.UserID)
		if err != nil {
			return nil, err
		}
		pu, _, err := s.loadPasskeyUser(ctx, u)
		if err != nil {
			return nil, err
		}
		row = p
		return pu, nil
	}
	wa, err := newWebAuthn(cer.rp)
	if err != nil {
		return store.Passkey{}, false, err
	}
	_, cred, err := wa.ValidatePasskeyLogin(handler, cer.data, parsed)
	if err != nil {
		s.log.Info("passkey sign-in refused", "err", err)
		return store.Passkey{}, false, ErrInvalidCredentials
	}
	if cred.Authenticator.CloneWarning {
		s.log.Error("passkey sign counter went back: possibly a cloned authenticator, sign-in refused", "passkey", row.ID, "user_id", row.UserID)
		return store.Passkey{}, false, ErrInvalidCredentials
	}
	if data, err := json.Marshal(cred); err == nil {
		if err := s.db.PasskeyUsed(ctx, row.ID, string(data)); err != nil {
			s.log.Warn("record passkey use", "err", err)
		}
	}
	return row, parsed.Response.AuthenticatorData.Flags.HasUserVerified(), nil
}

// LoginWithPasskey is Login with a passkey instead of the TOTP code.
func (s *Service) LoginWithPasskey(ctx context.Context, c Client, username, password string, a PasskeyAssertion) (tok string, pu User, err error) {
	defer func() { s.logSignInFailure(ctx, "password+passkey", c.IP, username, err) }()
	u, err := s.passwordStep(ctx, c, username, password)
	if err != nil {
		return "", User{}, err
	}
	p, _, err := s.verifyAssertion(ctx, a)
	if err != nil {
		return "", User{}, err
	}
	if p.UserID != u.ID { // someone else's passkey
		return "", User{}, ErrInvalidCredentials
	}
	return s.finishLogin(ctx, c, u, "passkey")
}

// PasskeyLogin signs in with a passkey alone. Only passkeys the user marked as
// passwordless count, and only when the authenticator verified the user. On a refusal
// the returned user names the account when it is known, for the audit row.
func (s *Service) PasskeyLogin(ctx context.Context, c Client, a PasskeyAssertion) (tok string, pu User, err error) {
	defer func() { s.logSignInFailure(ctx, "passkey", c.IP, pu.Username, err) }()
	if !s.limiter.allow("ip:" + c.IP) {
		return "", User{}, ErrRateLimited
	}
	p, verified, err := s.verifyAssertion(ctx, a)
	if err != nil {
		return "", User{}, err
	}
	u, err := s.db.UserByID(ctx, p.UserID)
	if err != nil {
		return "", User{}, ErrInvalidCredentials
	}
	if u.AuthSource != store.SourceLocal || !u.Active || u.Disabled || u.TOTPSecret == "" {
		return "", publicUser(u), ErrInvalidCredentials
	}
	if !p.Passwordless || !verified {
		return "", publicUser(u), RefusedError{"This passkey is a second factor only. Sign in with your password and use the passkey instead of the code."}
	}
	return s.finishLogin(ctx, c, u, "passwordless passkey")
}
