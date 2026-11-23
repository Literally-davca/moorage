package metrics

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// ContentType is the Prometheus text exposition format.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Handler serves /metrics. Without a token the endpoint doesn't exist (404); with one,
// every request needs "Authorization: Bearer <token>". It is not behind the session
// cookie because Prometheus can't sign in.
func (c *Collector) Handler(token string) http.Handler {
	return c.HandlerFunc(func() string { return token })
}

// HandlerFunc serves /metrics using a dynamic token lookup (env or DB).
func (c *Collector) HandlerFunc(tokenFn func() string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		token := ""
		if tokenFn != nil {
			token = tokenFn()
		}
		if token == "" {
			http.Error(w, "404 page not found", http.StatusNotFound)
			return
		}
		want := sha256.Sum256([]byte(token))
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
			return
		}
		if !authorized(r, want) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="moorage metrics"`)
			http.Error(w, "Unauthorized.", http.StatusUnauthorized)
			return
		}
		// Render into memory first: a failure then becomes a clean 500 instead of a
		// truncated body that Prometheus would half-ingest.
		var buf bytes.Buffer
		if err := c.Write(r.Context(), &buf); err != nil {
			http.Error(w, "Metrics couldn't be collected.", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", ContentType)
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			w.Write(buf.Bytes()) //nolint:errcheck // scraper went away
		}
	})
}

// authorized compares the bearer token in constant time. Both sides are hashed first so
// the comparison doesn't leak the token's length either.
func authorized(r *http.Request, want [32]byte) bool {
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(h[len(prefix):])))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}
