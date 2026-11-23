// Package ssotest is a fake OpenID Connect provider for tests: discovery, JWKS, a token
// endpoint that checks PKCE, and userinfo, on an httptest TLS server. Only tests import it.
package ssotest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// ClientID is the client the fake expects.
const ClientID = "moorage"

// IdP is the fake provider. Exported fields may be changed between sign-ins; lock with Mu.
type IdP struct {
	Srv *httptest.Server

	Mu sync.Mutex
	// Claims are added to (and override) the default ID token claims of the next tokens.
	Claims map[string]any
	// Mutate changes the final ID token claims (wrong iss, aud, exp, nonce…).
	Mutate func(map[string]any)
	// Raw replaces the signed ID token entirely (alg none and other forgeries).
	Raw func(claims map[string]any) string
	// SignKid is the key that signs; it must exist in Keys.
	SignKid string
	// Keys holds every key; Published lists the kids served in the JWKS.
	Keys      map[string]*rsa.PrivateKey
	Published []string
	// Userinfo is served at the userinfo endpoint (sub is added).
	Userinfo map[string]any
	// JWKSHits and PKCEChecked count requests, for assertions.
	JWKSHits    int
	PKCEChecked int

	codes map[string]pending
}

type pending struct {
	challenge, method, nonce, redirect string
}

// New starts the fake with one published key "k1".
func New(t *testing.T) *IdP {
	t.Helper()
	i := &IdP{Claims: map[string]any{}, Keys: map[string]*rsa.PrivateKey{}, codes: map[string]pending{}}
	i.AddKey(t, "k1", true)
	i.SignKid = "k1"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", i.discovery)
	mux.HandleFunc("GET /jwks", i.jwks)
	mux.HandleFunc("POST /token", i.token)
	mux.HandleFunc("GET /userinfo", i.userinfo)
	i.Srv = httptest.NewTLSServer(mux)
	t.Cleanup(i.Srv.Close)
	return i
}

// Issuer is the issuer URL.
func (i *IdP) Issuer() string { return i.Srv.URL }

// Client trusts the fake's certificate.
func (i *IdP) Client() *http.Client { return i.Srv.Client() }

// AddKey creates a key; publish puts it in the JWKS.
func (i *IdP) AddKey(t *testing.T, kid string, publish bool) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i.Mu.Lock()
	defer i.Mu.Unlock()
	i.Keys[kid] = k
	if publish {
		i.Published = append(i.Published, kid)
	}
}

// Authorize plays the user signing in at the provider: it reads the authorization URL
// Moorage redirected to and returns the code and state the provider sends back.
func (i *IdP) Authorize(t *testing.T, authURL string) (code, state string) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("client_id") != ClientID || q.Get("response_type") != "code" {
		t.Fatalf("bad authorization request: %s", authURL)
	}
	code = randString()
	i.Mu.Lock()
	i.codes[code] = pending{challenge: q.Get("code_challenge"), method: q.Get("code_challenge_method"), nonce: q.Get("nonce"), redirect: q.Get("redirect_uri")}
	i.Mu.Unlock()
	return code, q.Get("state")
}

func (i *IdP) discovery(w http.ResponseWriter, _ *http.Request) {
	b := i.Issuer()
	writeJSON(w, map[string]any{
		"issuer": b, "authorization_endpoint": b + "/authorize", "token_endpoint": b + "/token",
		"jwks_uri": b + "/jwks", "userinfo_endpoint": b + "/userinfo", "end_session_endpoint": b + "/logout",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (i *IdP) jwks(w http.ResponseWriter, _ *http.Request) {
	i.Mu.Lock()
	defer i.Mu.Unlock()
	i.JWKSHits++
	var set jose.JSONWebKeySet
	for _, kid := range i.Published {
		set.Keys = append(set.Keys, jose.JSONWebKey{Key: &i.Keys[kid].PublicKey, KeyID: kid, Algorithm: "RS256", Use: "sig"})
	}
	writeJSON(w, set)
}

func (i *IdP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	i.Mu.Lock()
	p, ok := i.codes[r.PostForm.Get("code")]
	delete(i.codes, r.PostForm.Get("code"))
	i.Mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || p.method != "S256" || base64.RawURLEncoding.EncodeToString(sum[:]) != p.challenge {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"}) //nolint:errcheck
		return
	}
	i.Mu.Lock()
	i.PKCEChecked++
	claims := map[string]any{
		"iss": i.Issuer(), "aud": ClientID, "sub": "user-1", "nonce": p.nonce,
		"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(),
	}
	for k, v := range i.Claims {
		claims[k] = v
	}
	if i.Mutate != nil {
		i.Mutate(claims)
	}
	var idt string
	if i.Raw != nil {
		idt = i.Raw(claims)
	} else {
		idt = i.sign(i.SignKid, claims)
	}
	i.Mu.Unlock()
	writeJSON(w, map[string]any{"access_token": "at-" + randString(), "token_type": "Bearer", "id_token": idt, "expires_in": 300})
}

// Sign signs claims with the key kid (call with Mu held or before the server is used).
func (i *IdP) sign(kid string, claims map[string]any) string {
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: i.Keys[kid]},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid))
	if err != nil {
		panic(err)
	}
	b, _ := json.Marshal(claims)
	obj, err := s.Sign(b)
	if err != nil {
		panic(err)
	}
	out, _ := obj.CompactSerialize()
	return out
}

// SignWith signs with any key under any kid header (for bad-signature tests). Mu is held
// by the token handler when Raw runs.
func (i *IdP) SignWith(key *rsa.PrivateKey, kid string, claims map[string]any) string {
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid))
	if err != nil {
		panic(err)
	}
	b, _ := json.Marshal(claims)
	obj, _ := s.Sign(b)
	out, _ := obj.CompactSerialize()
	return out
}

// Unsigned builds an "alg: none" token.
func Unsigned(claims map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	b, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding.EncodeToString
	return enc(h) + "." + enc(b) + "."
}

func (i *IdP) userinfo(w http.ResponseWriter, _ *http.Request) {
	i.Mu.Lock()
	defer i.Mu.Unlock()
	out := map[string]any{"sub": "user-1"}
	if s, ok := i.Claims["sub"]; ok {
		out["sub"] = s
	}
	for k, v := range i.Userinfo {
		out[k] = v
	}
	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func randString() string {
	b := make([]byte, 16)
	rand.Read(b) //nolint:errcheck
	return base64.RawURLEncoding.EncodeToString(b)
}
