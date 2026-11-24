package engine

import (
	"slices"
	"testing"
)

func TestExtractContainerURLs(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   []string
	}{
		{
			name:   "empty",
			labels: nil,
			want:   nil,
		},
		{
			name:   "moorage.url single and multi with dedupe",
			labels: map[string]string{"moorage.url": "https://app.example.com, http://localhost:8080 https://app.example.com"},
			want:   []string{"https://app.example.com", "http://localhost:8080"},
		},
		{
			name: "homepage.href and priority over proxy labels",
			labels: map[string]string{
				"moorage.url":                 "https://primary.example.com",
				"homepage.href":               "https://home.example.com",
				"traefik.http.routers.r.rule": "Host(`traefik.example.com`)",
				"caddy":                       "caddy.example.com",
			},
			want: []string{
				"https://primary.example.com",
				"https://home.example.com",
				"https://traefik.example.com",
				"https://caddy.example.com",
			},
		},
		{
			name: "traefik Host and PathPrefix and TLS",
			labels: map[string]string{
				"traefik.http.routers.app.rule":             "Host(`app.example.com`, `alt.example.com`) && PathPrefix(`/ui`)",
				"traefik.http.routers.app.tls.certresolver": "le",
				"traefik.http.routers.local.rule":           "Host(`localhost`)",
				"traefik.http.routers.local.tls":            "false",
			},
			want: []string{
				"https://app.example.com/ui",
				"https://alt.example.com/ui",
				"http://localhost",
			},
		},
		{
			name: "traefik ignores HostRegexp and wildcards",
			labels: map[string]string{
				"traefik.http.routers.re.rule":   "HostRegexp(`{subdomain:[a-z]+}.example.com`)",
				"traefik.http.routers.wild.rule": "Host(`*.example.com`)",
			},
			want: nil,
		},
		{
			name: "caddy variants",
			labels: map[string]string{
				"caddy":   "notes.example.com, http://127.0.0.1:9000",
				"caddy_0": "https://git.example.com/org",
				"caddy_1": ":8080 *.example.com",
			},
			want: []string{
				"https://notes.example.com",
				"http://127.0.0.1:9000",
				"https://git.example.com/org",
			},
		},
		{
			name: "rejects dangerous schemes and credentials",
			labels: map[string]string{
				"moorage.url":   "javascript:alert(1) data:text/html,hi https://user:pass@evil.example.com ftp://files.example.com",
				"homepage.href": "https://safe.example.com",
			},
			want: []string{"https://safe.example.com"},
		},
		{
			name: "caps at 5 urls",
			labels: map[string]string{
				"moorage.url": "https://1.example.com https://2.example.com https://3.example.com https://4.example.com https://5.example.com https://6.example.com",
			},
			want: []string{
				"https://1.example.com",
				"https://2.example.com",
				"https://3.example.com",
				"https://4.example.com",
				"https://5.example.com",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			core, proxy := extractContainerURLs(tc.labels)
			got := append(core, proxy...)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// Coolify writes an http and an https router per domain (and sometimes PathPrefix(`/`)):
// the card must show the domain once, as https.
func TestServiceURLsDedupeHTTPAndHTTPS(t *testing.T) {
	core, proxy := extractContainerURLs(map[string]string{
		"traefik.http.routers.http-0-x.rule":     "Host(`grafana.example.com`) && PathPrefix(`/`)",
		"traefik.http.routers.https-0-x.rule":    "Host(`grafana.example.com`) && PathPrefix(`/`)",
		"traefik.http.routers.https-0-x.tls":     "true",
		"traefik.http.routers.http-1-x.rule":     "Host(`192.168.1.5`)",
		"traefik.http.routers.https-1-x.rule":    "Host(`192.168.1.5`)",
		"traefik.http.routers.https-1-x.tls":     "true",
		"traefik.http.routers.http-2-x.rule":     "Host(`GRAFANA.example.com`)",
		"traefik.http.routers.https-2-x.tls":     "true",
		"traefik.http.routers.https-2-x.service": "x",
	})
	if core != nil {
		t.Fatalf("core: %v", core)
	}
	want := []string{"https://grafana.example.com/", "https://192.168.1.5"}
	if !slices.Equal(proxy, want) {
		t.Fatalf("got %v, want %v", proxy, want)
	}
	_, merged := collectStackURLs([]Container{
		{ProxyURLs: []string{"http://app.example.com"}},
		{ProxyURLs: []string{"https://app.example.com/"}},
	})
	if !slices.Equal(merged, []string{"https://app.example.com/"}) {
		t.Fatalf("stack: %v", merged)
	}
}
