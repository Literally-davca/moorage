package auth

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"moorage/internal/store"
)

// An OIDC role comes from the IdP only at sign-in, so OIDC sessions must be short;
// forward-auth re-checks the proxy header on every request and keeps the long TTL.
func TestExternalSessionTTL(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := New(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.setupToken = "" // external sign-in is refused before setup
	s.mu.Unlock()

	check := func(tok string, want time.Duration) {
		t.Helper()
		sess, err := db.SessionByHash(ctx, hashToken(tok))
		if err != nil {
			t.Fatal(err)
		}
		left := time.Until(sess.ExpiresAt)
		if left > want || left < want-time.Minute {
			t.Fatalf("session lasts %v, want about %v", left, want)
		}
	}
	tok, _, err := s.ExternalLogin(ctx, Client{IP: "127.0.0.1"}, Identity{Source: store.SourceOIDC, Issuer: "https://idp", Subject: "s1", PreferredUsername: "kim"}, "viewer")
	if err != nil {
		t.Fatal(err)
	}
	check(tok, OIDCSessionTTL)
	tok, _, _, err = s.ProxySession(ctx, Client{IP: "127.0.0.1"}, Identity{Source: store.SourceProxy, Issuer: "proxy", Subject: "bob", PreferredUsername: "bob"}, "viewer", "")
	if err != nil {
		t.Fatal(err)
	}
	check(tok, SessionTTL)
}
