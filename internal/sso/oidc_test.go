package sso

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"moorage/internal/config"
	"moorage/internal/sso/ssotest"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func oidcCfg(issuer string) config.OIDCConfig {
	return config.OIDCConfig{
		Issuer: issuer, ClientID: ssotest.ClientID, ClientSecret: "s3cret", Scopes: []string{"openid", "profile", "groups"},
		GroupsClaim: "groups", Label: "SSO", LocalLogin: true,
		Roles: config.RoleMapping{AdminGroups: []string{"moorage-admins"}, ViewerGroups: []string{"moorage-viewers"}, DefaultRole: "none"},
	}
}

func newTestOIDC(t *testing.T, idp *ssotest.IdP, mod func(*config.OIDCConfig), gap time.Duration) *OIDC {
	t.Helper()
	cfg := oidcCfg(idp.Issuer())
	if mod != nil {
		mod(&cfg)
	}
	o := NewOIDC(cfg, "https://moorage.example.com/", quiet, Options{Client: idp.Client(), JWKSGap: gap})
	if err := o.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	return o
}

// signIn runs one sign-in through the fake: Begin, the provider, Finish.
func signIn(t *testing.T, o *OIDC, idp *ssotest.IdP) (Result, error) {
	t.Helper()
	u, f, err := o.Begin()
	if err != nil {
		t.Fatal(err)
	}
	code, state := idp.Authorize(t, u)
	if state != f.State {
		t.Fatalf("state in URL %q != flow %q", state, f.State)
	}
	return o.Finish(context.Background(), state, f.State, code)
}

func TestOIDCHappyPathWithPKCE(t *testing.T) {
	idp := ssotest.New(t)
	idp.Claims = map[string]any{"sub": "abc", "preferred_username": "Alice", "email": "alice@example.com", "name": "Alice A", "groups": []string{"moorage-admins"}}
	o := newTestOIDC(t, idp, nil, time.Nanosecond)
	if !o.Ready() || o.Status().RedirectURI != "https://moorage.example.com/api/auth/oidc/callback" {
		t.Fatalf("status: %+v", o.Status())
	}
	u, _, _ := o.Begin()
	if !strings.Contains(u, "code_challenge_method=S256") || !strings.Contains(u, "nonce=") {
		t.Fatalf("no PKCE or nonce in %s", u)
	}
	res, err := signIn(t, o, idp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Identity.Subject != "abc" || res.Identity.Issuer != idp.Issuer() || res.Role != "admin" || res.Identity.PreferredUsername != "Alice" {
		t.Fatalf("result: %+v", res)
	}
	if idp.PKCEChecked != 2-1 {
		t.Fatalf("the fake should have checked PKCE once, got %d", idp.PKCEChecked)
	}
	if strings.Contains(o.Status().LastError+o.Status().ClientID, "s3cret") {
		t.Fatal("client secret in status")
	}
}

func TestOIDCRejectsBadTokens(t *testing.T) {
	idp := ssotest.New(t)
	other := ssotest.New(t) // a key nobody published
	cases := map[string]func(){
		"bad signature": func() {
			idp.Raw = func(c map[string]any) string { return idp.SignWith(other.Keys["k1"], "k1", c) }
		},
		"wrong aud":     func() { idp.Mutate = func(c map[string]any) { c["aud"] = "someone-else" } },
		"wrong iss":     func() { idp.Mutate = func(c map[string]any) { c["iss"] = "https://evil.example.com" } },
		"expired":       func() { idp.Mutate = func(c map[string]any) { c["exp"] = time.Now().Add(-10 * time.Minute).Unix() } },
		"wrong nonce":   func() { idp.Mutate = func(c map[string]any) { c["nonce"] = "not-the-nonce" } },
		"missing nonce": func() { idp.Mutate = func(c map[string]any) { delete(c, "nonce") } },
		"alg none":      func() { idp.Raw = ssotest.Unsigned },
		"future iat":    func() { idp.Mutate = func(c map[string]any) { c["iat"] = time.Now().Add(time.Hour).Unix() } },
		"azp other": func() {
			idp.Mutate = func(c map[string]any) { c["aud"] = []string{ssotest.ClientID, "api"}; c["azp"] = "api" }
		},
		"several aud no azp": func() { idp.Mutate = func(c map[string]any) { c["aud"] = []string{ssotest.ClientID, "api"} } },
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			idp.Mu.Lock()
			idp.Raw, idp.Mutate = nil, nil
			idp.Mu.Unlock()
			o := newTestOIDC(t, idp, nil, time.Nanosecond)
			idp.Mu.Lock()
			set()
			idp.Mu.Unlock()
			if res, err := signIn(t, o, idp); err == nil {
				t.Fatalf("accepted: %+v", res)
			}
		})
	}
	// azp equal to the client with several audiences is fine.
	idp.Raw = nil
	idp.Mutate = func(c map[string]any) { c["aud"] = []string{ssotest.ClientID, "api"}; c["azp"] = ssotest.ClientID }
	if _, err := signIn(t, newTestOIDC(t, idp, nil, time.Nanosecond), idp); err != nil {
		t.Fatalf("azp = client: %v", err)
	}
}

func TestOIDCReplayedState(t *testing.T) {
	idp := ssotest.New(t)
	o := newTestOIDC(t, idp, nil, time.Nanosecond)
	u, f, _ := o.Begin()
	code, state := idp.Authorize(t, u)
	if _, err := o.Finish(context.Background(), state, f.State, code); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Finish(context.Background(), state, f.State, code); err != ErrFlow {
		t.Fatalf("replayed state: %v", err)
	}
	// A state from another browser (cookie mismatch) is refused, and burnt.
	u, f, _ = o.Begin()
	code, state = idp.Authorize(t, u)
	if _, err := o.Finish(context.Background(), state, "other-browser", code); err != ErrFlow {
		t.Fatalf("cookie mismatch: %v", err)
	}
	if _, err := o.Finish(context.Background(), state, f.State, code); err != ErrFlow {
		t.Fatalf("state must be one use even after a mismatch: %v", err)
	}
}

// A token signed with a key that appeared after the JWKS was cached makes exactly one
// refetch; an unknown kid inside the refetch gap makes none.
func TestOIDCUnknownKidRefetchOnce(t *testing.T) {
	idp := ssotest.New(t)
	o := newTestOIDC(t, idp, nil, time.Nanosecond)
	if _, err := signIn(t, o, idp); err != nil {
		t.Fatal(err)
	}
	before := idp.JWKSHits
	idp.AddKey(t, "k2", true)
	idp.Mu.Lock()
	idp.SignKid = "k2"
	idp.Mu.Unlock()
	if _, err := signIn(t, o, idp); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
	if idp.JWKSHits != before+1 {
		t.Fatalf("want one refetch, got %d", idp.JWKSHits-before)
	}
	if _, err := signIn(t, o, idp); err != nil || idp.JWKSHits != before+1 {
		t.Fatalf("known key must not refetch: %v hits=%d", err, idp.JWKSHits-before)
	}

	// Rate limit: with a gap, a stream of unknown kids fetches once at most.
	idp2 := ssotest.New(t)
	o2 := newTestOIDC(t, idp2, nil, time.Hour)
	if _, err := signIn(t, o2, idp2); err != nil {
		t.Fatal(err)
	}
	idp2.AddKey(t, "ghost", false)
	idp2.Mu.Lock()
	idp2.SignKid = "ghost"
	hits := idp2.JWKSHits
	idp2.Mu.Unlock()
	for i := 0; i < 3; i++ {
		if _, err := signIn(t, o2, idp2); err == nil {
			t.Fatal("unpublished key accepted")
		}
	}
	if idp2.JWKSHits != hits {
		t.Fatalf("refetch inside the gap: %d", idp2.JWKSHits-hits)
	}
	// A bad signature under a known kid never refetches.
	o.keysForTest().fetches = 0
	idp.Mu.Lock()
	idp.Raw = func(c map[string]any) string { return idp.SignWith(idp2.Keys["ghost"], "k2", c) }
	idp.Mu.Unlock()
	if _, err := signIn(t, o, idp); err == nil || o.keysForTest().fetches != 0 {
		t.Fatalf("bad signature: err=%v fetches=%d", err, o.keysForTest().fetches)
	}
}

func (o *OIDC) keysForTest() *keySet { return o.provider().keys }

func TestOIDCGroups(t *testing.T) {
	idp := ssotest.New(t)
	// nested claim path (Keycloak realm roles)
	idp.Claims = map[string]any{"realm_access": map[string]any{"roles": []string{"offline", "moorage-viewers"}}}
	o := newTestOIDC(t, idp, func(c *config.OIDCConfig) { c.GroupsClaim = "realm_access.roles" }, time.Nanosecond)
	if res, err := signIn(t, o, idp); err != nil || res.Role != "viewer" {
		t.Fatalf("nested: %+v %v", res, err)
	}
	// a single string, with a Keycloak full path
	idp.Claims = map[string]any{"groups": "/moorage-admins"}
	o = newTestOIDC(t, idp, nil, time.Nanosecond)
	if res, err := signIn(t, o, idp); err != nil || res.Role != "admin" {
		t.Fatalf("string: %+v %v", res, err)
	}
	// absent from the ID token: read from userinfo
	idp.Claims = map[string]any{}
	idp.Userinfo = map[string]any{"groups": []string{"moorage-admins"}}
	if res, err := signIn(t, o, idp); err != nil || res.Role != "admin" {
		t.Fatalf("userinfo: %+v %v", res, err)
	}
	// no group, default none: no role
	idp.Userinfo = map[string]any{"groups": []string{"other"}}
	if res, err := signIn(t, o, idp); err != nil || res.Role != "" {
		t.Fatalf("none: %+v %v", res, err)
	}
	o = newTestOIDC(t, idp, func(c *config.OIDCConfig) { c.Roles.DefaultRole = "viewer" }, time.Nanosecond)
	if res, _ := signIn(t, o, idp); res.Role != "viewer" {
		t.Fatalf("default viewer: %+v", res)
	}
	// userinfo about another subject is refused
	idp.Userinfo = map[string]any{"sub": "intruder", "groups": []string{"moorage-admins"}}
	if _, err := signIn(t, o, idp); err == nil {
		t.Fatal("userinfo for another sub accepted")
	}
}

// A broken setting or an unreachable provider never fails; the button stays hidden and
// the status says why.
func TestOIDCBrokenConfig(t *testing.T) {
	for name, cfg := range map[string]config.OIDCConfig{
		"http issuer": oidcCfg("http://idp.example.com"),
		"no client":   {Issuer: "https://idp.example.com"},
		"env error": func() config.OIDCConfig {
			c := oidcCfg("https://idp.example.com")
			c.Err = "bad default role"
			return c
		}(),
		"not a url": oidcCfg("https://"),
	} {
		o := NewOIDC(cfg, "https://m.example.com", quiet, Options{})
		o.Start(context.Background())
		if o.Ready() || o.Status().LastError == "" || !o.LocalLogin() {
			t.Errorf("%s: ready=%v status=%+v", name, o.Ready(), o.Status())
		}
	}
	o := NewOIDC(oidcCfg("https://idp.example.com"), "", quiet, Options{})
	if !strings.Contains(o.Status().LastError, "MOORAGE_PUBLIC_URL") {
		t.Errorf("missing public URL: %+v", o.Status())
	}
	// localhost may use http (development)
	o = NewOIDC(oidcCfg("http://127.0.0.1:1"), "http://localhost:5080", quiet, Options{})
	if o.permanent != "" {
		t.Errorf("http on loopback refused: %s", o.permanent)
	}
	// unreachable: Discover fails, error recorded, local login stays on
	dead := httptest.NewTLSServer(http.NotFoundHandler())
	dead.Close()
	cfg := oidcCfg(dead.URL)
	cfg.LocalLogin = false
	o = NewOIDC(cfg, "https://m.example.com", quiet, Options{Client: dead.Client()})
	if err := o.Discover(context.Background()); err == nil || o.Ready() || o.Status().LastError == "" || !o.LocalLogin() {
		t.Fatalf("unreachable: %v %+v", err, o.Status())
	}
	if _, _, err := o.Begin(); err == nil {
		t.Fatal("Begin without a provider")
	}
	// not configured at all: nothing shown, no error
	o = NewOIDC(config.OIDCConfig{LocalLogin: true}, "", quiet, Options{})
	if st := o.Status(); st.Configured || st.LastError != "" {
		t.Fatalf("unconfigured: %+v", st)
	}
}

func TestFlowsBounded(t *testing.T) {
	idp := ssotest.New(t)
	o := newTestOIDC(t, idp, nil, time.Nanosecond)
	for i := 0; i < maxFlows+50; i++ {
		if _, _, err := o.Begin(); err != nil {
			t.Fatal(err)
		}
	}
	if len(o.flows) > maxFlows {
		t.Fatalf("flows not bounded: %d", len(o.flows))
	}
}

func TestRoleAndClaims(t *testing.T) {
	m := config.RoleMapping{AdminGroups: []string{"a"}, ViewerGroups: []string{"v"}, DefaultRole: "none"}
	if Role(m, []string{"v", "a"}) != "admin" || Role(m, []string{"v"}) != "viewer" || Role(m, []string{"x"}) != "" || Role(m, nil) != "" {
		t.Fatal("mapping")
	}
	claims := map[string]any{"realm_access.roles": []any{"top"}, "realm_access": map[string]any{"roles": []any{"nested"}}}
	if v, _ := lookupClaim(claims, "realm_access.roles"); groupsOf(v)[0] != "top" {
		t.Fatal("a literal dotted claim wins")
	}
	if _, ok := lookupClaim(claims, "realm_access.missing"); ok {
		t.Fatal("missing path found")
	}
}

func TestProxyTrust(t *testing.T) {
	p := NewProxy(config.ProxyConfig{
		Trusted: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("::1/128")}, UserHeader: "Remote-User", GroupsHeader: "Remote-Groups",
		Roles: config.RoleMapping{AdminGroups: []string{"admins"}, DefaultRole: "none"},
	})
	req := func(remote string, hdr map[string]string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return r
	}
	if id, role, ok := p.FromRequest(req("10.1.2.3:4000", map[string]string{"Remote-User": "bob", "Remote-Groups": "dev, admins"})); !ok || id.Subject != "bob" || role != "admin" {
		t.Fatalf("trusted: %+v %q %v", id, role, ok)
	}
	if _, _, ok := p.FromRequest(req("[::1]:4000", map[string]string{"Remote-User": "bob"})); !ok {
		t.Fatal("trusted v6")
	}
	if _, _, ok := p.FromRequest(req("192.0.2.1:4000", map[string]string{"Remote-User": "bob", "X-Forwarded-For": "10.1.2.3", "X-Real-Ip": "10.1.2.3"})); ok {
		t.Fatal("untrusted peer accepted (X-Forwarded-For spoof)")
	}
	if _, _, ok := p.FromRequest(req("10.1.2.3:4000", map[string]string{"Remote-User": "bad\x01name"})); ok {
		t.Fatal("control characters accepted")
	}
	if _, role, ok := p.FromRequest(req("10.1.2.3:4000", map[string]string{"Remote-User": "eve"})); !ok || role != "" {
		t.Fatal("no group with default none must give no role")
	}
	r := req("192.0.2.1:4000", map[string]string{"Remote-User": "bob", "Remote-Groups": "admins"})
	p.Strip(r)
	if r.Header.Get("Remote-User") != "" || r.Header.Get("Remote-Groups") != "" {
		t.Fatal("strip")
	}
	if NewProxy(config.ProxyConfig{}).Enabled() {
		t.Fatal("off by default")
	}
}
