package sso

import (
	"net"
	"net/http"
	"net/netip"
	"strings"

	"moorage/internal/auth"
	"moorage/internal/config"
	"moorage/internal/store"
)

// ProxyIssuer is the identity issuer of forward-auth users.
const ProxyIssuer = "proxy"

// Proxy is forward-auth: a reverse proxy in front of Moorage (Authelia, an Authentik
// outpost, oauth2-proxy) signs the user in and passes their name in a header.
type Proxy struct{ cfg config.ProxyConfig }

// NewProxy wraps the settings; it is off when no proxy is trusted.
func NewProxy(cfg config.ProxyConfig) *Proxy { return &Proxy{cfg: cfg} }

// Enabled reports whether any proxy is trusted.
func (p *Proxy) Enabled() bool { return p != nil && len(p.cfg.Trusted) > 0 && p.cfg.UserHeader != "" }

// Trusted reports whether the TCP peer is a trusted proxy. Only r.RemoteAddr counts:
// X-Forwarded-For is set by whoever sent the request and proves nothing.
func (p *Proxy) Trusted(r *http.Request) bool {
	if !p.Enabled() {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, pre := range p.cfg.Trusted {
		if pre.Contains(a) {
			return true
		}
	}
	return false
}

// Strip removes the identity headers, so nothing behind this point can read a header
// that an untrusted client sent.
func (p *Proxy) Strip(r *http.Request) {
	if p == nil {
		return
	}
	for _, h := range []string{p.cfg.UserHeader, p.cfg.GroupsHeader} {
		if h != "" {
			r.Header.Del(h)
		}
	}
}

// FromRequest reads the user named by a trusted proxy. ok is false without the header
// (then the session cookie rules apply) or when the peer isn't trusted.
func (p *Proxy) FromRequest(r *http.Request) (id auth.Identity, role string, ok bool) {
	if !p.Trusted(r) {
		return id, "", false
	}
	name := strings.TrimSpace(r.Header.Get(p.cfg.UserHeader))
	if name == "" || len(name) > 256 || strings.ContainsFunc(name, func(c rune) bool { return c < ' ' || c == 0x7f }) {
		return id, "", false
	}
	var groups []string
	if p.cfg.GroupsHeader != "" {
		for _, g := range strings.Split(r.Header.Get(p.cfg.GroupsHeader), ",") {
			if g = strings.TrimSpace(g); g != "" {
				groups = append(groups, g)
			}
		}
	}
	id = auth.Identity{Source: store.SourceProxy, Issuer: ProxyIssuer, Subject: name, PreferredUsername: name}
	if strings.Contains(name, "@") {
		id.Email = name
	}
	return id, Role(p.cfg.Roles, groups), true
}

// ProxyStatus is the read-only forward-auth panel for admins.
type ProxyStatus struct {
	Enabled      bool     `json:"enabled"`
	Trusted      []string `json:"trusted"`
	UserHeader   string   `json:"userHeader,omitempty"`
	GroupsHeader string   `json:"groupsHeader,omitempty"`
	AdminGroups  []string `json:"adminGroups"`
	ViewerGroups []string `json:"viewerGroups"`
	DefaultRole  string   `json:"defaultRole,omitempty"`
}

// Status describes the forward-auth settings.
func (p *Proxy) Status() ProxyStatus {
	st := ProxyStatus{Enabled: p.Enabled(), Trusted: []string{}, AdminGroups: []string{}, ViewerGroups: []string{}}
	if !st.Enabled {
		return st
	}
	for _, t := range p.cfg.Trusted {
		st.Trusted = append(st.Trusted, t.String())
	}
	st.UserHeader, st.GroupsHeader = p.cfg.UserHeader, p.cfg.GroupsHeader
	st.AdminGroups = append(st.AdminGroups, p.cfg.Roles.AdminGroups...)
	st.ViewerGroups = append(st.ViewerGroups, p.cfg.Roles.ViewerGroups...)
	st.DefaultRole = p.cfg.Roles.DefaultRole
	return st
}
