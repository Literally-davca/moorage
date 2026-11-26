package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChannels(t *testing.T) {
	var got struct {
		path, auth, title, prio string
		body                    []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.auth, got.title, got.prio = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Title"), r.Header.Get("Priority")
		got.body, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()
	ctx := context.Background()
	m := Message{Status: "down", Title: "DOWN: web\r\nX-Injected: 1", Text: "HTTP 502", Logs: []string{"@everyone ``` boom"}, URL: "https://m/#/x"}

	if err := Send(ctx, "ntfy", Config{URL: srv.URL + "/alerts", Token: "tk"}, m); err != nil {
		t.Fatal(err)
	}
	if got.auth != "Bearer tk" || got.prio != "5" || strings.Contains(got.title, "\n") || !strings.Contains(string(got.body), "boom") {
		t.Fatalf("ntfy: %+v %q", got, got.body)
	}

	if err := Send(ctx, "discord", Config{URL: srv.URL + "/hook"}, m); err != nil {
		t.Fatal(err)
	}
	var d struct {
		Content         string
		AllowedMentions struct{ Parse []string } `json:"allowed_mentions"`
		Embeds          []struct{ Description string }
	}
	json.Unmarshal(got.body, &d) //nolint:errcheck
	if d.AllowedMentions.Parse == nil || len(d.AllowedMentions.Parse) != 0 {
		t.Fatalf("discord must disable mentions: %s", got.body)
	}
	if len(d.Embeds) != 1 || strings.Count(d.Embeds[0].Description, "```") != 2 {
		t.Fatalf("log line broke out of the code block: %s", got.body)
	}
	if err := Send(ctx, "discord", Config{URL: srv.URL + "/hook", Format: FormatPlain}, m); err != nil {
		t.Fatal(err)
	}
	d.Content = ""
	json.Unmarshal(got.body, &d) //nolint:errcheck
	if strings.Count(d.Content, "```") != 2 {
		t.Fatalf("plain: log line broke out of the code block: %q", d.Content)
	}

	if err := Send(ctx, "webhook", Config{URL: srv.URL + "/w"}, m); err != nil {
		t.Fatal(err)
	}
	var wh map[string]any
	if json.Unmarshal(got.body, &wh) != nil || wh["event"] != "down" {
		t.Fatalf("webhook: %s", got.body)
	}

	srvErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) }))
	defer srvErr.Close()
	if err := Send(ctx, "webhook", Config{URL: srvErr.URL}, m); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("want 403 error, got %v", err)
	}
	if err := Send(ctx, "webhook", Config{URL: "http://169.254.169.254/"}, m); err == nil || !strings.Contains(err.Error(), "link-local") {
		t.Fatalf("metadata webhook allowed: %v", err)
	}
}

func TestSecretsMasking(t *testing.T) {
	c := Config{URL: "https://discord.com/api/webhooks/1/secret"}
	masked := c.Masked("discord")
	if masked.URL != SecretMask {
		t.Fatal("discord URL is a secret")
	}
	back := masked
	back.KeepSecrets("discord", c)
	if back.URL != c.URL {
		t.Fatal("mask sent back should keep the stored secret")
	}
	if err := Validate("smtp", Config{Host: "smtp.x", Port: 587, From: "a@x.cz", To: "b@x.cz, nope"}); err == nil {
		t.Fatal("invalid recipient accepted")
	}
	if err := Validate("telegram", Config{Token: "1:abc", ChatID: "42"}); err != nil {
		t.Fatal(err)
	}
}
