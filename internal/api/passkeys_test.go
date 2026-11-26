package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"

	"moorage/internal/auth"
	"moorage/internal/store"
)

const (
	pkOrigin = "https://moorage.example.com"
	pkRPID   = "moorage.example.com"
)

var b64u = base64.RawURLEncoding

// softKey is a software authenticator: one ES256 passkey, "none" attestation.
type softKey struct {
	t      *testing.T
	priv   *ecdsa.PrivateKey
	id     []byte
	handle []byte
	rpID   string
	origin string
	count  uint32
	uv     bool // the authenticator verifies the user (PIN, biometrics)
}

func newSoftKey(t *testing.T) *softKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 32)
	rand.Read(id) //nolint:errcheck
	return &softKey{t: t, priv: priv, id: id, rpID: pkRPID, origin: pkOrigin, uv: true}
}

func (k *softKey) clientData(typ, challenge string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": k.origin, "crossOrigin": false})
	return b
}

func (k *softKey) authData(attested []byte) []byte {
	h := sha256.Sum256([]byte(k.rpID))
	flags := byte(0x01) // UP
	if k.uv {
		flags |= 0x04
	}
	if attested != nil {
		flags |= 0x40 // AT
	}
	out := append([]byte{}, h[:]...)
	out = append(out, flags)
	out = binary.BigEndian.AppendUint32(out, k.count)
	return append(out, attested...)
}

// create answers navigator.credentials.create options (the JSON the server sent).
func (k *softKey) create(options json.RawMessage) json.RawMessage {
	k.t.Helper()
	var o struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			User      struct {
				ID string `json:"id"`
			} `json:"user"`
			RP struct {
				ID string `json:"id"`
			} `json:"rp"`
			AuthenticatorSelection struct {
				ResidentKey string `json:"residentKey"`
			} `json:"authenticatorSelection"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(options, &o); err != nil {
		k.t.Fatal(err)
	}
	if o.PublicKey.RP.ID != pkRPID || o.PublicKey.AuthenticatorSelection.ResidentKey != "required" {
		k.t.Fatalf("creation options: %s", options)
	}
	handle, err := b64u.DecodeString(o.PublicKey.User.ID)
	if err != nil {
		k.t.Fatal(err)
	}
	k.handle = handle
	cose, err := webauthncbor.Marshal(map[int64]any{
		1: int64(2), 3: int64(-7), -1: int64(1),
		-2: k.priv.X.FillBytes(make([]byte, 32)), -3: k.priv.Y.FillBytes(make([]byte, 32)),
	})
	if err != nil {
		k.t.Fatal(err)
	}
	attested := make([]byte, 16) // zero AAGUID
	attested = binary.BigEndian.AppendUint16(attested, uint16(len(k.id)))
	attested = append(attested, k.id...)
	attested = append(attested, cose...)
	att, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": k.authData(attested)})
	if err != nil {
		k.t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"id": b64u.EncodeToString(k.id), "rawId": b64u.EncodeToString(k.id), "type": "public-key",
		"response": map[string]any{
			"attestationObject": b64u.EncodeToString(att),
			"clientDataJSON":    b64u.EncodeToString(k.clientData("webauthn.create", o.PublicKey.Challenge)),
		},
	})
	return b
}

// get answers navigator.credentials.get options.
func (k *softKey) get(options json.RawMessage) json.RawMessage {
	k.t.Helper()
	var o struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			RPID      string `json:"rpId"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(options, &o); err != nil {
		k.t.Fatal(err)
	}
	if o.PublicKey.RPID != pkRPID {
		k.t.Fatalf("request options: %s", options)
	}
	k.count++
	ad := k.authData(nil)
	cd := k.clientData("webauthn.get", o.PublicKey.Challenge)
	cdh := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad...), cdh[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, k.priv, digest[:])
	if err != nil {
		k.t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"id": b64u.EncodeToString(k.id), "rawId": b64u.EncodeToString(k.id), "type": "public-key",
		"response": map[string]any{
			"authenticatorData": b64u.EncodeToString(ad),
			"clientDataJSON":    b64u.EncodeToString(cd),
			"signature":         b64u.EncodeToString(sig),
			"userHandle":        b64u.EncodeToString(k.handle),
		},
	})
	return b
}

type pkCeremony struct {
	Ceremony string          `json:"ceremony"`
	Options  json.RawMessage `json:"options"`
}

func newPasskeyEnv(t *testing.T) *auditEnv {
	t.Helper()
	env := newAuditEnv(t)
	env.srv.cfg.PublicURL, env.srv.cfg.PublicURLSet = pkOrigin+"/", true
	return env
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// registerPasskey adds k to the account of the session tok; code is a fresh TOTP code.
func (e *auditEnv) registerPasskey(t *testing.T, tok, password, code string, k *softKey, name string, passwordless bool) *http.Response {
	t.Helper()
	w := e.req("POST", "/api/account/passkeys/start", mustJSON(map[string]string{"password": password, "code": code}), tok)
	if w.Code != http.StatusOK {
		t.Fatalf("register start: %d %s", w.Code, w.Body)
	}
	c := decode[pkCeremony](t, w)
	w = e.req("POST", "/api/account/passkeys", mustJSON(map[string]any{
		"ceremony": c.Ceremony, "name": name, "passwordless": passwordless, "response": k.create(c.Options),
	}), tok)
	return w.Result()
}

func (e *auditEnv) loginOptions(t *testing.T) pkCeremony {
	t.Helper()
	w := e.req("POST", "/api/login/passkey/options", `{}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("login options: %d %s", w.Code, w.Body)
	}
	return decode[pkCeremony](t, w)
}

func auditHas(t *testing.T, db *store.Store, action, target string) bool {
	t.Helper()
	for _, r := range auditRows(t, db) {
		if r.Action == action && (target == "" || r.Target == target) {
			return true
		}
	}
	return false
}

// A passkey is a second factor next to TOTP: registered with password and code, used
// instead of the code at sign-in, passwordless only when the user turns it on, and the
// changes land in the audit log.
func TestPasskeySecondFactorAndPasswordless(t *testing.T) {
	env := newPasskeyEnv(t)
	ctx := context.Background()
	l := env.invite(t, "frank", "admin")
	password := "frank-long-password-1"
	tok, secret := env.acceptInvite(t, l.Link.Token, password)

	acc := decode[struct {
		Passkey  passkeyStatus
		Passkeys []auth.PasskeyInfo
	}](t, env.req("GET", "/api/account", "", tok))
	if !acc.Passkey.Available || acc.Passkey.RPID != pkRPID || acc.Passkey.Origin != pkOrigin || len(acc.Passkeys) != 0 {
		t.Fatalf("account passkey status: %+v", acc)
	}

	// Without the current TOTP code nothing starts: a stolen session plus password can't add a factor.
	if w := env.req("POST", "/api/account/passkeys/start", mustJSON(map[string]string{"password": password, "code": "000000"}), tok); w.Code != http.StatusBadRequest {
		t.Fatalf("start without code: %d", w.Code)
	}
	if w := env.req("POST", "/api/account/passkeys/start", mustJSON(map[string]string{"password": "wrong-password-123", "code": "000000"}), tok); w.Code != http.StatusBadRequest {
		t.Fatalf("start with wrong password: %d", w.Code)
	}

	k := newSoftKey(t)
	code := totpNow(t, secret, time.Now().Add(30*time.Second))
	if res := env.registerPasskey(t, tok, password, code, k, "Laptop", false); res.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d", res.StatusCode)
	}
	if !auditHas(t, env.db, "account.passkey_add", "frank: Laptop") {
		t.Fatalf("no audit row for the added passkey: %+v", auditRows(t, env.db))
	}
	rows, _ := env.db.UserPasskeys(ctx, l.User.ID)
	if len(rows) != 1 || rows[0].RPID != pkRPID || rows[0].Passwordless {
		t.Fatalf("stored passkeys: %+v", rows)
	}

	// Password + passkey instead of the code.
	c := env.loginOptions(t)
	body := mustJSON(map[string]any{"username": "frank", "password": password,
		"passkey": map[string]any{"ceremony": c.Ceremony, "response": k.get(c.Options)}})
	w := call(env.h, "POST", "/api/login", body, "", "198.51.100.20:1", nil)
	if w.Code != http.StatusOK || sessionCookie(w) == "" {
		t.Fatalf("login with passkey: %d %s", w.Code, w.Body)
	}
	if !auditHas(t, env.db, "auth.login", "passkey") {
		t.Fatal("no auth.login row with target passkey")
	}

	// A challenge is answered once.
	w = call(env.h, "POST", "/api/login", body, "", "198.51.100.21:1", nil)
	if w.Code == http.StatusOK {
		t.Fatal("a used challenge signed in again")
	}

	// A wrong password with a valid passkey is still a failed sign-in.
	c = env.loginOptions(t)
	w = call(env.h, "POST", "/api/login", mustJSON(map[string]any{"username": "frank", "password": "not-the-password-1",
		"passkey": map[string]any{"ceremony": c.Ceremony, "response": k.get(c.Options)}}), "", "198.51.100.22:1", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password + passkey: %d", w.Code)
	}

	// The passkey is a second factor only, so it can't sign in alone yet.
	c = env.loginOptions(t)
	w = call(env.h, "POST", "/api/login/passkey", mustJSON(map[string]any{"ceremony": c.Ceremony, "response": k.get(c.Options)}), "", "198.51.100.23:1", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("passwordless with a 2FA-only passkey: %d %s", w.Code, w.Body)
	}
	if !auditHas(t, env.db, "auth.login_failed", "passwordless passkey") {
		t.Fatal("no failed passwordless sign-in row")
	}

	// Turned on, it signs in without a password.
	if w := env.req("PUT", "/api/account/passkeys/"+itoa(rows[0].ID), `{"name":"Laptop","passwordless":true}`, tok); w.Code != http.StatusOK {
		t.Fatalf("enable passwordless: %d %s", w.Code, w.Body)
	}
	if !auditHas(t, env.db, "account.passkey_save", "frank: Laptop (passwordless)") {
		t.Fatal("no audit row for the passkey change")
	}
	c = env.loginOptions(t)
	w = call(env.h, "POST", "/api/login/passkey", mustJSON(map[string]any{"ceremony": c.Ceremony, "response": k.get(c.Options)}), "", "198.51.100.24:1", nil)
	if w.Code != http.StatusOK || sessionCookie(w) == "" {
		t.Fatalf("passwordless login: %d %s", w.Code, w.Body)
	}
	if !auditHas(t, env.db, "auth.login", "passwordless passkey") {
		t.Fatal("no passwordless auth.login row")
	}

	// A disabled account can't sign in with its passkey either.
	if w := env.req("POST", "/api/users/"+itoa(l.User.ID)+"/disable", "", "tok-admin"); w.Code != http.StatusNoContent {
		t.Fatal(w.Code)
	}
	c = env.loginOptions(t)
	w = call(env.h, "POST", "/api/login/passkey", mustJSON(map[string]any{"ceremony": c.Ceremony, "response": k.get(c.Options)}), "", "198.51.100.25:1", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("disabled passwordless login: %d", w.Code)
	}
	if w := env.req("POST", "/api/users/"+itoa(l.User.ID)+"/enable", "", "tok-admin"); w.Code != http.StatusNoContent {
		t.Fatal(w.Code)
	}

	// A sign counter that goes back (a cloned authenticator) is refused.
	k.count = 0
	c = env.loginOptions(t)
	w = call(env.h, "POST", "/api/login/passkey", mustJSON(map[string]any{"ceremony": c.Ceremony, "response": k.get(c.Options)}), "", "198.51.100.26:1", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("counter regression: %d", w.Code)
	}

	// The admin list shows the count. Disabling ended the sessions; a good counter signs
	// in again and gives a fresh session.
	users := decode[struct{ Users []auth.UserInfo }](t, env.req("GET", "/api/users", "", "tok-admin"))
	for _, u := range users.Users {
		if u.Username == "frank" && (u.Passkeys != 1 || !u.TOTP) {
			t.Fatalf("passkeys and TOTP in the user list: %+v", u)
		}
	}
	k.count = 50
	c = env.loginOptions(t)
	w = call(env.h, "POST", "/api/login/passkey", mustJSON(map[string]any{"ceremony": c.Ceremony, "response": k.get(c.Options)}), "", "198.51.100.27:1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("passwordless login after the refused clone: %d %s", w.Code, w.Body)
	}
	tok = sessionCookie(w)
	if w := env.req("DELETE", "/api/account/passkeys/"+itoa(rows[0].ID), "", tok); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if !auditHas(t, env.db, "account.passkey_remove", "frank: Laptop (passwordless)") {
		t.Fatal("no audit row for the removed passkey")
	}
	if w := env.req("DELETE", "/api/account/passkeys/"+itoa(rows[0].ID), "", tok); w.Code != http.StatusNotFound {
		t.Fatalf("delete twice: %d", w.Code)
	}
	// Removing the last passkey leaves TOTP as it was (TOTP sign-in itself is covered by
	// the other login tests; a second fresh code isn't available within one time step).
	if u, _ := env.db.UserByID(ctx, l.User.ID); u.TOTPSecret != secret {
		t.Fatal("removing a passkey touched TOTP")
	}
	k.count = 100
	c = env.loginOptions(t)
	w = call(env.h, "POST", "/api/login/passkey", mustJSON(map[string]any{"ceremony": c.Ceremony, "response": k.get(c.Options)}), "", "198.51.100.28:1", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("removed passkey still signs in: %d", w.Code)
	}
}

// One user's passkey can't be the second factor of another account, and one user can't
// change another user's passkeys.
func TestPasskeyBoundToItsAccount(t *testing.T) {
	env := newPasskeyEnv(t)
	ctx := context.Background()
	la := env.invite(t, "gina", "viewer")
	tokA, secretA := env.acceptInvite(t, la.Link.Token, "gina-long-password-1")
	lb := env.invite(t, "hugo", "viewer")
	tokB, _ := env.acceptInvite(t, lb.Link.Token, "hugo-long-password-1")

	k := newSoftKey(t)
	if res := env.registerPasskey(t, tokA, "gina-long-password-1", totpNow(t, secretA, time.Now().Add(30*time.Second)), k, "Key", true); res.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d", res.StatusCode)
	}
	c := env.loginOptions(t)
	w := call(env.h, "POST", "/api/login", mustJSON(map[string]any{"username": "hugo", "password": "hugo-long-password-1",
		"passkey": map[string]any{"ceremony": c.Ceremony, "response": k.get(c.Options)}}), "", "198.51.100.30:1", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("hugo with gina's passkey: %d", w.Code)
	}
	rows, _ := env.db.UserPasskeys(ctx, la.User.ID)
	if len(rows) != 1 {
		t.Fatalf("passkeys: %+v", rows)
	}
	if w := env.req("DELETE", "/api/account/passkeys/"+itoa(rows[0].ID), "", tokB); w.Code != http.StatusNotFound {
		t.Fatalf("hugo deletes gina's passkey: %d", w.Code)
	}
	if w := env.req("PUT", "/api/account/passkeys/"+itoa(rows[0].ID), `{"name":"x","passwordless":false}`, tokB); w.Code != http.StatusNotFound {
		t.Fatalf("hugo edits gina's passkey: %d", w.Code)
	}
}

// Passwordless sign-in needs an authenticator that verifies the user.
func TestPasskeyPasswordlessNeedsUserVerification(t *testing.T) {
	env := newPasskeyEnv(t)
	l := env.invite(t, "ivan", "viewer")
	tok, secret := env.acceptInvite(t, l.Link.Token, "ivan-long-password-1")
	k := newSoftKey(t)
	k.uv = false
	if res := env.registerPasskey(t, tok, "ivan-long-password-1", totpNow(t, secret, time.Now().Add(30*time.Second)), k, "U2F key", false); res.StatusCode != http.StatusCreated {
		t.Fatalf("2FA-only without UV: %d", res.StatusCode)
	}
	rows, _ := env.db.UserPasskeys(context.Background(), l.User.ID)
	if w := env.req("PUT", "/api/account/passkeys/"+itoa(rows[0].ID), `{"name":"U2F key","passwordless":true}`, tok); w.Code != http.StatusBadRequest {
		t.Fatalf("turn on passwordless without UV: %d", w.Code)
	}

	l2 := env.invite(t, "ivana", "viewer")
	tok2, secret2 := env.acceptInvite(t, l2.Link.Token, "ivana-long-password-1")
	if res := env.registerPasskey(t, tok2, "ivana-long-password-1", totpNow(t, secret2, time.Now().Add(30*time.Second)), k, "U2F key", true); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("passwordless without UV: %d", res.StatusCode)
	}
}

// Passkeys are bound to the public URL; without one they are off and say why.
func TestPasskeysNeedPublicURL(t *testing.T) {
	env := newAuditEnv(t) // no public URL
	if w := env.req("POST", "/api/login/passkey/options", `{}`, ""); w.Code != http.StatusConflict {
		t.Fatalf("options without public URL: %d", w.Code)
	}
	s := decode[sessionResp](t, env.req("GET", "/api/session", "", ""))
	if s.Login.PasskeyOrigin != "" {
		t.Fatalf("passkey origin without public URL: %+v", s.Login)
	}
	env.srv.cfg.PublicURL, env.srv.cfg.PublicURLSet = pkOrigin, true
	s = decode[sessionResp](t, env.req("GET", "/api/session", "", ""))
	if s.Login.PasskeyOrigin != pkOrigin {
		t.Fatalf("passkey origin: %+v", s.Login)
	}
}

// A passkey made at another site (a phishing proxy, an old host name) doesn't count.
func TestPasskeyWrongOriginRefused(t *testing.T) {
	env := newPasskeyEnv(t)
	l := env.invite(t, "jana", "viewer")
	tok, secret := env.acceptInvite(t, l.Link.Token, "jana-long-password-1")
	k := newSoftKey(t)
	if res := env.registerPasskey(t, tok, "jana-long-password-1", totpNow(t, secret, time.Now().Add(30*time.Second)), k, "Key", true); res.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d", res.StatusCode)
	}
	k.origin = "https://moorage.example.com.evil.test"
	c := env.loginOptions(t)
	w := call(env.h, "POST", "/api/login/passkey", mustJSON(map[string]any{"ceremony": c.Ceremony, "response": k.get(c.Options)}), "", "198.51.100.40:1", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("assertion from another origin: %d", w.Code)
	}
}

// Finishing an admin's reset link removes the user's passkeys: the reset is the way back
// in after a lost or stolen device, and TOTP is set up again in the same step.
func TestResetLinkRemovesPasskeys(t *testing.T) {
	env := newPasskeyEnv(t)
	ctx := context.Background()
	l := env.invite(t, "karl", "viewer")
	tok, secret := env.acceptInvite(t, l.Link.Token, "karl-long-password-1")
	if res := env.registerPasskey(t, tok, "karl-long-password-1", totpNow(t, secret, time.Now().Add(30*time.Second)), newSoftKey(t), "Phone", false); res.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d", res.StatusCode)
	}
	w := env.req("POST", "/api/users/"+itoa(l.User.ID)+"/reset", "", "tok-admin")
	if w.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	link := decode[linkResp](t, w).Link
	if rows, _ := env.db.UserPasskeys(ctx, l.User.ID); len(rows) != 1 {
		t.Fatal("issuing the link must change nothing yet")
	}
	env.acceptInvite(t, link.Token, "karl-new-password-12")
	if rows, _ := env.db.UserPasskeys(ctx, l.User.ID); len(rows) != 0 {
		t.Fatalf("passkeys after reset: %+v", rows)
	}
}

func TestRelyingPartyFor(t *testing.T) {
	ok := map[string]auth.RelyingParty{
		"https://moorage.example.com":          {ID: "moorage.example.com", Origin: "https://moorage.example.com"},
		"https://Moorage.Example.com:443/sub/": {ID: "moorage.example.com", Origin: "https://moorage.example.com"},
		"https://moorage.example.com:8443":     {ID: "moorage.example.com", Origin: "https://moorage.example.com:8443"},
		"http://localhost:5080":                {ID: "localhost", Origin: "http://localhost:5080"},
		"http://moorage.localhost":             {ID: "moorage.localhost", Origin: "http://moorage.localhost"},
	}
	for in, want := range ok {
		got, err := auth.RelyingPartyFor(in)
		if err != nil || got != want {
			t.Errorf("RelyingPartyFor(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "http://moorage.example.com", "https://192.168.1.10", "https://[::1]:5080",
		"ftp://moorage.example.com", "https://user:pw@moorage.example.com", "moorage.example.com"} {
		if rp, err := auth.RelyingPartyFor(in); err == nil {
			t.Errorf("RelyingPartyFor(%q) = %+v, want an error", in, rp)
		} else if !strings.Contains(err.Error(), "asskey") {
			t.Errorf("RelyingPartyFor(%q): unclear error %v", in, err)
		}
	}
}
