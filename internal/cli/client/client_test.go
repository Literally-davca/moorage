package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRemoteClientBearerAndError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Moorage") != "1" {
			t.Errorf("missing X-Moorage header")
		}
		if r.Header.Get("Authorization") != "Bearer mrg_testsecret" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"bad token"}`))
			return
		}
		switch r.URL.Path {
		case "/api/hosts":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"hosts": []HostEntry{
					{ID: "agent-1", Name: "node-1", Connected: true},
					{ID: "local", Name: "hub", Connected: true},
				},
			})
		case "/api/coolify/names":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"names": map[string]CoolifyNameMeta{
					"local/uuid-123": {UUID: "uuid-123", Name: "Plausible"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	defer srv.Close()

	c, err := New(Options{URL: srv.URL, Token: "mrg_testsecret"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h, err := c.ResolveHost(ctx, "")
	if err != nil || h != "local" {
		t.Fatalf("expected local host, got %q err=%v", h, err)
	}
	c.LoadCoolifyNames(ctx)
	if got := c.StackDisplayName("local", "uuid-123", ""); got != "Plausible" {
		t.Fatalf("expected Plausible, got %q", got)
	}

	var out any
	err = c.Get(ctx, "/api/missing", &out)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 || apiErr.Message != "not found" {
		t.Fatalf("expected 404 APIError, got %v", err)
	}

	unauth, _ := New(Options{URL: srv.URL, Token: "wrong"})
	err = unauth.Get(ctx, "/api/hosts", &out)
	if !errors.As(err, &apiErr) || apiErr.Status != 401 {
		t.Fatalf("expected 401 APIError, got %v", err)
	}
}
