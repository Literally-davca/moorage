package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"moorage/internal/auth"
	"moorage/internal/config"
	"moorage/internal/metrics"
	"moorage/internal/monitor"
	"moorage/internal/store"
)

const testMetricsToken = "metrics-token-0123456789"

func newMetricsServer(t *testing.T, token string) (*Server, *store.Store) {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := auth.New(db, log)
	if err != nil {
		t.Fatal(err)
	}
	mon := monitor.New(db, nil, log, "")
	cfg := config.Config{MetricsToken: token, StatusListen: ":5085", PublicURL: "https://moorage.example.com"}
	srv := New(cfg, a, db, nil, nil, mon, log, "test")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv.SetHubContext(ctx)
	return srv, db
}

func TestMetricsOffByDefault(t *testing.T) {
	srv, _ := newMetricsServer(t, "")
	h := srv.Handler()
	for _, a := range []string{"", "Bearer " + testMetricsToken} {
		w := do(h, "GET", "/metrics", "", map[string]string{"Authorization": a})
		if w.Code != http.StatusNotFound {
			t.Errorf("metrics off, Authorization %q: %d, want 404", a, w.Code)
		}
		if strings.Contains(w.Body.String(), "<html") || strings.Contains(w.Body.String(), "moorage_") {
			t.Errorf("metrics off served something: %.80s", w.Body.String())
		}
	}
	if info := srv.metricsInfo(context.Background()); info["enabled"] != false {
		t.Errorf("metricsInfo %v", info)
	}
}

func TestMetricsTokenAndListeners(t *testing.T) {
	srv, db := newMetricsServer(t, testMetricsToken)
	h, statusH := srv.Handler(), srv.StatusHandler()
	bearer := map[string]string{"Authorization": "Bearer " + testMetricsToken}

	if w := do(h, "GET", "/metrics", "", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("missing token: %d", w.Code)
	}
	if w := do(h, "GET", "/metrics", "", map[string]string{"Authorization": "Bearer wrong-but-long-token"}); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", w.Code)
	}
	// a session cookie is not a way in
	if w := do(h, "GET", "/metrics", "", map[string]string{"Cookie": cookieName + "=whatever"}); w.Code != http.StatusUnauthorized {
		t.Errorf("cookie instead of token: %d", w.Code)
	}
	w := do(h, "GET", "/metrics", "", bearer)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != metrics.ContentType {
		t.Fatalf("right token: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if !strings.Contains(w.Body.String(), `moorage_build_info{version="test"} 1`) {
		t.Errorf("body:\n%s", w.Body.String())
	}

	// the status listener never serves it, with or without the token
	if w := do(statusH, "GET", "/metrics", "", bearer); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "moorage_") {
		t.Errorf("status listener /metrics: %d", w.Code)
	}
	// nor does a status page domain on the main listener
	if _, err := db.SaveStatusPage(context.Background(), store.StatusPage{
		Slug: "main", Title: "Main", Domain: "status.example.com", Published: true,
	}); err != nil {
		t.Fatal(err)
	}
	srv.StatusService().Invalidate()
	for _, hd := range []http.Handler{h, statusH} {
		w := doHost(hd, "GET", "status.example.com", "/metrics", "", bearer)
		if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "moorage_") {
			t.Errorf("status domain /metrics: %d %.80s", w.Code, w.Body.String())
		}
	}
	// the main listener on its own name still works
	if w := do(h, "GET", "/metrics", "", bearer); w.Code != http.StatusOK {
		t.Errorf("after adding a status domain: %d", w.Code)
	}

	info := srv.metricsInfo(context.Background())
	if info["enabled"] != true || info["url"] != "https://moorage.example.com/metrics" {
		t.Errorf("metricsInfo %v", info)
	}
	for _, v := range info {
		if s, ok := v.(string); ok && strings.Contains(s, testMetricsToken) {
			t.Error("metricsInfo leaks the token")
		}
	}
}
