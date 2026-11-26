// Package notify delivers alerts to webhook, ntfy, Telegram, Discord, Slack, Gotify,
// Pushover, Matrix and e-mail, rendered per channel (render.go) with the channel's
// content preset and format (preset.go).
//
// Everything is done with Go's HTTP and SMTP clients: no external programs
// (Uptime Kuma's Apprise integration was a code-execution risk) and no user
// templates (no template injection). Connections go through netguard.
package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"moorage/internal/netguard"
)

// Kinds of channels.
var Kinds = map[string]bool{
	"webhook":  true,
	"ntfy":     true,
	"telegram": true,
	"discord":  true,
	"smtp":     true,
	"slack":    true,
	"gotify":   true,
	"pushover": true,
	"matrix":   true,
}

// Config is the kind-specific configuration of a channel.
type Config struct {
	URL      string            `json:"url,omitempty"`      // webhook, ntfy topic URL, discord/slack webhook, gotify/matrix server
	Token    string            `json:"token,omitempty"`    // webhook bearer, ntfy/gotify/matrix/pushover token, telegram bot token
	ChatID   string            `json:"chatId,omitempty"`   // telegram chat ID, pushover user key, matrix room ID
	Host     string            `json:"host,omitempty"`     // smtp
	Port     int               `json:"port,omitempty"`     // smtp
	Security string            `json:"security,omitempty"` // smtp: starttls (default) | tls
	Username string            `json:"username,omitempty"` // smtp
	Password string            `json:"password,omitempty"` // smtp
	From     string            `json:"from,omitempty"`     // smtp
	To       string            `json:"to,omitempty"`       // smtp, comma separated
	Headers  map[string]string `json:"headers,omitempty"`  // custom HTTP headers for webhook
	Events   []string          `json:"events,omitempty"`   // subscribed event kinds (empty = checks only)

	// Presentation (M17B). Missing values = Standard + rich, what channels showed before.
	Preset  string          `json:"preset,omitempty"`  // minimal | standard | detailed | custom
	Include map[string]bool `json:"include,omitempty"` // parts shown by the Custom preset (PartKeys)
	Format  string          `json:"format,omitempty"`  // rich | plain
}

// Event kinds a channel can subscribe to (Config.Events).
const (
	EventChecks       = "checks"        // DOWN / back up of the checks the channel is attached to
	EventAutoUpdate   = "auto_update"   // an image update was installed automatically
	EventRollback     = "rollback"      // an automatic rollback after a failed update
	EventDeployFailed = "deploy_failed" // a stack operation (up, restart, pull…) failed
	EventAgents       = "agents"        // an agent went offline / came back
)

// Kinds of messages that aren't channel subscriptions (Message.Event only).
const (
	EventWatchdog = "watchdog" // the agent watchdog reports the hub down / back
	EventTest     = "test"     // "Send test" and previews
	EventUpdates  = "updates"  // image updates are available (per-stack notify policy)
)

// Events lists the known event kinds.
var Events = []string{EventChecks, EventAutoUpdate, EventRollback, EventDeployFailed, EventAgents}

// WantsEvent reports whether the channel is subscribed to eventKind.
func (c Config) WantsEvent(eventKind string) bool {
	if len(c.Events) == 0 {
		return eventKind == EventChecks
	}
	return slices.Contains(c.Events, eventKind)
}

// SecretMask replaces secrets in API responses; sending it back keeps the stored value.
const SecretMask = "••••••••"

// secretFields per kind: never shown again after saving.
func secretFields(kind string) []string {
	switch kind {
	case "webhook":
		return []string{"token"}
	case "ntfy":
		return []string{"token"}
	case "telegram":
		return []string{"token"}
	case "discord":
		return []string{"url"} // the webhook URL is the secret
	case "slack":
		return []string{"url"}
	case "gotify", "pushover", "matrix":
		return []string{"token"}
	case "smtp":
		return []string{"password"}
	}
	return nil
}

func (c *Config) field(name string) *string {
	switch name {
	case "url":
		return &c.URL
	case "token":
		return &c.Token
	case "password":
		return &c.Password
	}
	return nil
}

// Masked returns a copy safe to show in the UI.
func (c Config) Masked(kind string) Config {
	for _, f := range secretFields(kind) {
		if p := c.field(f); p != nil && *p != "" {
			*p = SecretMask
		}
	}
	return c
}

// KeepSecrets copies stored secrets into c where the UI sent the mask back.
func (c *Config) KeepSecrets(kind string, old Config) {
	for _, f := range secretFields(kind) {
		if p := c.field(f); p != nil && *p == SecretMask {
			*p = *old.field(f)
		}
	}
}

// Validate checks a configuration before it is stored.
func Validate(kind string, c Config) error {
	if err := validatePresentation(c); err != nil {
		return err
	}
	httpURL := func(s, what string) error {
		u, err := url.Parse(s)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("%s must be an http:// or https:// URL", what)
		}
		return nil
	}
	switch kind {
	case "webhook":
		return httpURL(c.URL, "The webhook URL")
	case "ntfy":
		return httpURL(c.URL, "The topic URL (for example https://ntfy.sh/my-alerts)")
	case "discord":
		return httpURL(c.URL, "The Discord webhook URL")
	case "slack":
		return httpURL(c.URL, "The Slack webhook URL")
	case "gotify":
		if err := httpURL(c.URL, "The Gotify server URL"); err != nil {
			return err
		}
		if c.Token == "" {
			return errors.New("Gotify needs an application token")
		}
	case "pushover":
		if c.Token == "" || c.ChatID == "" {
			return errors.New("Pushover needs an app token and a user key")
		}
	case "matrix":
		if err := httpURL(c.URL, "The Matrix homeserver URL"); err != nil {
			return err
		}
		if c.ChatID == "" || c.Token == "" {
			return errors.New("Matrix needs a room ID and an access token")
		}
	case "telegram":
		if c.Token == "" || c.ChatID == "" {
			return errors.New("Telegram needs the bot token and the chat ID")
		}
		if strings.ContainsAny(c.Token, "/?#") {
			return errors.New("the bot token looks wrong, copy it again from @BotFather")
		}
	case "smtp":
		if c.Host == "" || c.Port <= 0 || c.Port > 65535 {
			return errors.New("SMTP needs a server and a port")
		}
		if _, err := mail.ParseAddress(c.From); err != nil {
			return errors.New("the From address isn't valid")
		}
		if len(recipients(c.To)) == 0 {
			return errors.New("add at least one recipient")
		}
		for _, r := range recipients(c.To) {
			if _, err := mail.ParseAddress(r); err != nil {
				return fmt.Errorf("recipient %q isn't valid", r)
			}
		}
		if c.Security != "" && c.Security != "starttls" && c.Security != "tls" {
			return errors.New("security must be starttls or tls")
		}
	default:
		return fmt.Errorf("unknown channel type %q", kind)
	}
	return nil
}

func recipients(s string) []string {
	var out []string
	for _, r := range strings.Split(s, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

const sendTimeout = 20 * time.Second

var httpClient = &http.Client{
	Timeout: sendTimeout,
	Transport: &http.Transport{
		DialContext:       netguard.DialContext(10*time.Second, false),
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
		Proxy:             http.ProxyFromEnvironment,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Send delivers m through a channel of the given kind, rendered with the channel's
// preset and format.
func Send(ctx context.Context, kind string, c Config, m Message) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := Render(kind, c, m)
	if err != nil {
		return err
	}
	if req.Method == "SMTP" {
		return sendSMTP(ctx, c, req.Body)
	}
	return do(ctx, req)
}

func do(ctx context.Context, r Request) error {
	req, err := http.NewRequestWithContext(ctx, r.Method, r.URL, bytes.NewReader(r.Body))
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("%s", ue.Err) // the URL may contain a secret, don't echo it
		}
		return errors.New("the channel URL isn't valid")
	}
	req.Header.Set("Content-Type", r.ContentType)
	req.Header.Set("User-Agent", "Moorage")
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("%s", ue.Err) // the URL may contain a secret, don't echo it
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("the service answered %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) //nolint:errcheck
	return nil
}

func cryptoRand(b []byte) (int, error) { return rand.Read(b) }

func parseAddress(s string) (*mail.Address, error) {
	a, err := mail.ParseAddress(s)
	if err != nil {
		return nil, fmt.Errorf("address %q isn't valid", s)
	}
	return a, nil
}

func sendSMTP(ctx context.Context, c Config, message []byte) error {
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	d := netguard.Dialer(10*time.Second, false)
	var conn net.Conn
	var err error
	if c.Security == "tls" {
		conn, err = (&tls.Dialer{NetDialer: d, Config: &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl) //nolint:errcheck
	}
	cl, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer cl.Close()
	if c.Security != "tls" {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return errors.New("the server doesn't offer STARTTLS; Moorage won't send credentials or alerts unencrypted")
		}
		if err := cl.StartTLS(&tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if c.Username != "" {
		if err := cl.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
			return err
		}
	}
	from, err := parseAddress(c.From)
	if err != nil {
		return err
	}
	if err := cl.Mail(from.Address); err != nil {
		return err
	}
	for _, r := range recipients(c.To) {
		a, err := parseAddress(r)
		if err != nil {
			return err
		}
		if err := cl.Rcpt(a.Address); err != nil {
			return err
		}
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(message); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return cl.Quit()
}
