package api

import (
	"net/http/httptest"
	"net/netip"
	"testing"

	"moorage/internal/config"
)

func TestClientIPBehindProxy(t *testing.T) {
	cases := []struct {
		name, peer, xff, want string
		cfg                   config.Config
	}{
		{"no proxy trusted", "203.0.113.9:1234", "1.2.3.4", "203.0.113.9", config.Config{}},
		{"untrusted peer spoofs", "203.0.113.9:1234", "1.2.3.4", "203.0.113.9", config.Config{Proxy: config.ProxyConfig{Trusted: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}}},
		{"trusted proxy, last entry wins", "10.0.0.2:1234", "6.6.6.6, 198.51.100.7", "198.51.100.7", config.Config{Proxy: config.ProxyConfig{Trusted: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}}},
		{"tailscale serve on loopback", "127.0.0.1:1234", "100.64.0.5", "100.64.0.5", config.Config{TailscaleServe: true}},
		{"loopback without tailscale serve", "127.0.0.1:1234", "100.64.0.5", "127.0.0.1", config.Config{}},
		{"trusted proxy, garbage header", "10.0.0.2:1234", "nonsense", "10.0.0.2", config.Config{Proxy: config.ProxyConfig{Trusted: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}}},
	}
	for _, c := range cases {
		s := &Server{cfg: c.cfg}
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.peer
		r.Header.Set("X-Forwarded-For", c.xff)
		if got := clientIP(s.withClientIP(r)); got != c.want {
			t.Errorf("%s: clientIP = %q, want %q", c.name, got, c.want)
		}
	}
}
