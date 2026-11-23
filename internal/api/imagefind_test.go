package api

import "testing"

func TestUsesImage(t *testing.T) {
	compose := "services:\n  db:\n    image: \"postgres:17-alpine\"\n  app:\n    image: ghcr.io/owner/app@sha256:abc\n"
	for _, c := range []struct {
		repo, short string
		want        bool
	}{
		{"library/postgres", "postgres", true},
		{"owner/app", "owner/app", true},
		{"library/redis", "redis", false},
		{"owner/ap", "owner/ap", false},
	} {
		if got := usesImage(compose, c.repo, c.short); got != c.want {
			t.Errorf("%s: got %v", c.repo, got)
		}
	}
}
