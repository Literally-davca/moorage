package sso

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"moorage/internal/auth"
	"moorage/internal/config"
	"moorage/internal/store"
)

// CallbackPath is where the provider sends the browser back. The redirect URI to register
// at the provider is MOORAGE_PUBLIC_URL + CallbackPath.
const CallbackPath = "/api/auth/oidc/callback"

const (
	flowTTL    = 10 * time.Minute // how long a started sign-in may take at the provider
	maxFlows   = 2000             // pending sign-ins kept in memory; bounds a flood of starts
	clockSkew  = time.Minute      // leeway for exp and iat
	retryFirst = time.Minute      // discovery retry after a failure, doubling up to retryMax
	retryMax   = 10 * time.Minute
)

// ErrFlow is a callback whose state is unknown, used or expired, or doesn't match the
// browser that started the sign-in.
var ErrFlow = errors.New("sign-in expired or was started in another browser")

// Flow is one started sign-in. The state goes to the provider and into a cookie; the
// nonce and the PKCE verifier stay on the server.
type Flow struct {
	State    string
	nonce    string
	verifier string
	created  time.Time
}

// Result is a verified sign-in.
type Result struct {
	Identity auth.Identity
	Groups   []string
	Role     string // "" = no access
}

// OIDC is the configured provider. It is safe for concurrent use.
type OIDC struct {
	cfg      config.OIDCConfig
	redirect string
	client   *http.Client
	log      *slog.Logger
	now      func() time.Time
	jwksGap  time.Duration

	mu        sync.Mutex
	p         *provider // nil until discovery worked
	permanent string    // a config error: retrying can't help
	lastErr   string
	lastErrAt time.Time
	flows     map[string]Flow
}

type provider struct {
	oauth       oauth2.Config
	verifier    *oidc.IDTokenVerifier
	op          *oidc.Provider
	keys        *keySet
	hasUserinfo bool
	endSession  string
}

// Options are test hooks.
type Options struct {
	Client  *http.Client     // for the provider's endpoints; default has a 10 s timeout
	Now     func() time.Time // clock for token checks
	JWKSGap time.Duration    // minimum time between JWKS refetches, default 30 s
}

// NewOIDC checks the settings that can't change at runtime. Call Start to discover the
// provider. A broken setting is kept as the status error; it never fails the start.
func NewOIDC(cfg config.OIDCConfig, publicURL string, log *slog.Logger, opt Options) *OIDC {
	o := &OIDC{cfg: cfg, client: opt.Client, log: log, now: opt.Now, jwksGap: opt.JWKSGap, flows: map[string]Flow{}}
	if o.client == nil {
		o.client = &http.Client{Timeout: 10 * time.Second}
	}
	if o.now == nil {
		o.now = time.Now
	}
	if o.jwksGap == 0 {
		o.jwksGap = 30 * time.Second
	}
	if !cfg.Configured() {
		return o
	}
	redirect, err := validate(cfg, publicURL)
	o.redirect = redirect
	if err != nil {
		o.permanent = err.Error()
		o.setErr(err.Error())
		log.Error("OIDC sign-in is off: fix the settings and restart", "err", err)
	}
	return o
}

func validate(cfg config.OIDCConfig, publicURL string) (string, error) {
	var redirect string
	if pu, err := url.Parse(strings.TrimRight(publicURL, "/")); err == nil && (pu.Scheme == "https" || pu.Scheme == "http") && pu.Host != "" {
		redirect = pu.String() + CallbackPath
	}
	switch {
	case cfg.Err != "":
		return redirect, errors.New(cfg.Err)
	case cfg.Issuer == "":
		return redirect, errors.New("set MOORAGE_OIDC_ISSUER")
	case cfg.ClientID == "":
		return redirect, errors.New("set MOORAGE_OIDC_CLIENT_ID")
	case redirect == "":
		return redirect, errors.New("set MOORAGE_PUBLIC_URL to the address users open Moorage at (https://…); the redirect URI is built from it")
	}
	iu, err := url.Parse(cfg.Issuer)
	if err != nil || iu.Host == "" {
		return redirect, fmt.Errorf("MOORAGE_OIDC_ISSUER %q is not a URL", cfg.Issuer)
	}
	if iu.Scheme != "https" && !(iu.Scheme == "http" && isLoopback(iu.Hostname())) {
		return redirect, errors.New("MOORAGE_OIDC_ISSUER must use https (http only for localhost, for development)")
	}
	return redirect, nil
}

func isLoopback(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Start discovers the provider in the background and retries until it works, so an IdP
// that is down while Moorage starts doesn't need a restart.
func (o *OIDC) Start(ctx context.Context) {
	if !o.cfg.Configured() || o.permanent != "" {
		return
	}
	go func() {
		wait := retryFirst
		for {
			if err := o.Discover(ctx); err == nil || ctx.Err() != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			wait = min(wait*2, retryMax)
		}
	}()
}

// Discover loads the discovery document once. Start calls it; tests call it directly.
func (o *OIDC) Discover(ctx context.Context) error {
	if o.permanent != "" {
		return errors.New(o.permanent)
	}
	dctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, o.client), 15*time.Second)
	defer cancel()
	op, err := oidc.NewProvider(dctx, o.cfg.Issuer)
	if err != nil {
		err = fmt.Errorf("load the discovery document of %s: %w", o.cfg.Issuer, err)
		o.setErr(err.Error())
		o.log.Error("OIDC provider unreachable, retrying", "err", err)
		return err
	}
	var meta struct {
		JWKS       string `json:"jwks_uri"`
		Userinfo   string `json:"userinfo_endpoint"`
		EndSession string `json:"end_session_endpoint"`
	}
	if err := op.Claims(&meta); err != nil || meta.JWKS == "" {
		err = fmt.Errorf("discovery document of %s has no jwks_uri", o.cfg.Issuer)
		o.setErr(err.Error())
		return err
	}
	keys := &keySet{url: meta.JWKS, client: o.client, minGap: o.jwksGap}
	p := &provider{
		op:   op,
		keys: keys,
		oauth: oauth2.Config{
			ClientID: o.cfg.ClientID, ClientSecret: o.cfg.ClientSecret, Endpoint: op.Endpoint(),
			RedirectURL: o.redirect, Scopes: o.cfg.Scopes,
		},
		verifier: oidc.NewVerifier(o.cfg.Issuer, keys, &oidc.Config{
			ClientID: o.cfg.ClientID, SupportedSigningAlgs: allowedAlgNames,
			// exp gets the same leeway as nbf and iat, for clocks a little apart.
			Now: func() time.Time { return o.now().Add(-clockSkew) },
		}),
		hasUserinfo: meta.Userinfo != "",
		endSession:  meta.EndSession,
	}
	o.mu.Lock()
	o.p = p
	o.lastErr, o.lastErrAt = "", time.Time{}
	o.mu.Unlock()
	o.log.Info("OIDC sign-in ready", "issuer", o.cfg.Issuer, "redirect_uri", o.redirect)
	return nil
}

func (o *OIDC) setErr(msg string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.lastErr, o.lastErrAt = msg, o.now()
}

// NoteError records a failed sign-in for the admin's status panel.
func (o *OIDC) NoteError(msg string) { o.setErr(msg) }

func (o *OIDC) provider() *provider {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.p
}

// Ready reports whether the SSO button can be shown.
func (o *OIDC) Ready() bool { return o.provider() != nil }

// Label is the button text.
func (o *OIDC) Label() string { return o.cfg.Label }

// LocalLogin reports whether the password form is shown. It is always shown while SSO
// isn't working, so a broken IdP setting can't lock everyone out.
func (o *OIDC) LocalLogin() bool { return o.cfg.LocalLogin || !o.Ready() }

// EndSessionURL is the provider's RP-initiated logout endpoint, "" when it has none.
func (o *OIDC) EndSessionURL() string {
	if p := o.provider(); p != nil {
		return p.endSession
	}
	return ""
}

// Status is the read-only SSO panel for admins. It never holds the client secret.
type Status struct {
	Configured   bool       `json:"configured"`
	Ready        bool       `json:"ready"`
	Issuer       string     `json:"issuer,omitempty"`
	ClientID     string     `json:"clientId,omitempty"`
	RedirectURI  string     `json:"redirectUri,omitempty"`
	Scopes       []string   `json:"scopes,omitempty"`
	GroupsClaim  string     `json:"groupsClaim,omitempty"`
	AdminGroups  []string   `json:"adminGroups"`
	ViewerGroups []string   `json:"viewerGroups"`
	DefaultRole  string     `json:"defaultRole,omitempty"`
	LocalLogin   bool       `json:"localLogin"`
	Label        string     `json:"label,omitempty"`
	LastError    string     `json:"lastError,omitempty"`
	LastErrorAt  *time.Time `json:"lastErrorAt,omitempty"`
}

// Status describes the configuration and its last error.
func (o *OIDC) Status() Status {
	o.mu.Lock()
	defer o.mu.Unlock()
	st := Status{Configured: o.cfg.Configured(), LocalLogin: true, AdminGroups: []string{}, ViewerGroups: []string{}}
	if !st.Configured {
		return st
	}
	st.Ready = o.p != nil
	st.Issuer, st.ClientID, st.RedirectURI = o.cfg.Issuer, o.cfg.ClientID, o.redirect
	st.Scopes, st.GroupsClaim, st.Label = o.cfg.Scopes, o.cfg.GroupsClaim, o.cfg.Label
	st.AdminGroups = append(st.AdminGroups, o.cfg.Roles.AdminGroups...)
	st.ViewerGroups = append(st.ViewerGroups, o.cfg.Roles.ViewerGroups...)
	st.DefaultRole = o.cfg.Roles.DefaultRole
	st.LocalLogin = o.cfg.LocalLogin || o.p == nil
	st.LastError = o.lastErr
	if !o.lastErrAt.IsZero() {
		t := o.lastErrAt
		st.LastErrorAt = &t
	}
	return st
}

// Begin starts a sign-in: it returns the provider URL to redirect to and the flow,
// whose State the caller puts in a short-lived cookie.
func (o *OIDC) Begin() (string, Flow, error) {
	p := o.provider()
	if p == nil {
		return "", Flow{}, errors.New("SSO is not available")
	}
	f := Flow{State: randomString(), nonce: randomString(), verifier: oauth2.GenerateVerifier(), created: o.now()}
	o.mu.Lock()
	o.pruneFlows()
	o.flows[f.State] = f
	o.mu.Unlock()
	u := p.oauth.AuthCodeURL(f.State, oidc.Nonce(f.nonce), oauth2.S256ChallengeOption(f.verifier))
	return u, f, nil
}

// pruneFlows drops expired flows, then the oldest ones over the cap. Called with mu held.
func (o *OIDC) pruneFlows() {
	now := o.now()
	for k, f := range o.flows {
		if now.Sub(f.created) > flowTTL {
			delete(o.flows, k)
		}
	}
	for len(o.flows) >= maxFlows {
		oldest := ""
		for k, f := range o.flows {
			if oldest == "" || f.created.Before(o.flows[oldest].created) {
				oldest = k
			}
		}
		delete(o.flows, oldest)
	}
}

// takeFlow removes and returns a flow: a state works once.
func (o *OIDC) takeFlow(state, cookieState string) (Flow, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	f, ok := o.flows[state]
	if !ok || state == "" {
		return Flow{}, ErrFlow
	}
	delete(o.flows, state)
	if subtle.ConstantTimeCompare([]byte(state), []byte(cookieState)) != 1 || o.now().Sub(f.created) > flowTTL {
		return Flow{}, ErrFlow
	}
	return f, nil
}

// Finish redeems the code with the PKCE verifier, verifies the ID token and maps the
// groups to a role. cookieState is the state from the browser's cookie.
func (o *OIDC) Finish(ctx context.Context, state, cookieState, code string) (Result, error) {
	p := o.provider()
	if p == nil {
		return Result{}, errors.New("SSO is not available")
	}
	f, err := o.takeFlow(state, cookieState)
	if err != nil {
		return Result{}, err
	}
	ctx = oidc.ClientContext(ctx, o.client)
	tok, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(f.verifier))
	if err != nil {
		return Result{}, fmt.Errorf("redeem the code at the provider: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return Result{}, errors.New("the provider sent no ID token (is the openid scope allowed?)")
	}
	idt, claims, err := o.verify(ctx, p, raw, f.nonce)
	if err != nil {
		return Result{}, err
	}
	groups, err := o.groups(ctx, p, tok, idt.Subject, claims)
	if err != nil {
		return Result{}, err
	}
	str := func(k string) string { s, _ := claims[k].(string); return s }
	res := Result{
		Identity: auth.Identity{
			Source: store.SourceOIDC, Issuer: o.cfg.Issuer, Subject: idt.Subject,
			PreferredUsername: str("preferred_username"), Email: str("email"), DisplayName: str("name"),
		},
		Groups: groups,
		Role:   Role(o.cfg.Roles, groups),
	}
	return res, nil
}

// verify checks signature (JWKS), iss, aud, exp (go-oidc), then azp, iat and nonce.
func (o *OIDC) verify(ctx context.Context, p *provider, raw, nonce string) (*oidc.IDToken, map[string]any, error) {
	idt, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, nil, fmt.Errorf("ID token rejected: %w", err)
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return nil, nil, fmt.Errorf("ID token claims: %w", err)
	}
	azp, _ := claims["azp"].(string)
	if (len(idt.Audience) > 1 || azp != "") && azp != o.cfg.ClientID {
		return nil, nil, errors.New("ID token rejected: issued to another client (azp)")
	}
	if idt.IssuedAt.After(o.now().Add(clockSkew)) {
		return nil, nil, errors.New("ID token rejected: issued in the future (check the clocks)")
	}
	if nonce == "" || subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(nonce)) != 1 {
		return nil, nil, errors.New("ID token rejected: wrong nonce")
	}
	if idt.Subject == "" {
		return nil, nil, errors.New("ID token rejected: no subject")
	}
	return idt, claims, nil
}

// groups reads the groups claim from the ID token, or from the userinfo endpoint when
// the ID token doesn't carry it (some providers keep the token small).
func (o *OIDC) groups(ctx context.Context, p *provider, tok *oauth2.Token, sub string, claims map[string]any) ([]string, error) {
	if v, ok := lookupClaim(claims, o.cfg.GroupsClaim); ok {
		return groupsOf(v), nil
	}
	if !p.hasUserinfo {
		return nil, nil
	}
	ui, err := p.op.UserInfo(ctx, oauth2.StaticTokenSource(tok))
	if err != nil {
		return nil, fmt.Errorf("read groups from the userinfo endpoint: %w", err)
	}
	if ui.Subject != sub { // OIDC Core 5.3.2: userinfo must be about the same user
		return nil, errors.New("userinfo is about another user")
	}
	var uc map[string]any
	if err := ui.Claims(&uc); err != nil {
		return nil, fmt.Errorf("userinfo claims: %w", err)
	}
	v, _ := lookupClaim(uc, o.cfg.GroupsClaim)
	return groupsOf(v), nil
}

func randomString() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
