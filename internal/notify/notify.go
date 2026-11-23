// Package notify delivers alerts to webhook, ntfy, Telegram, Discord and e-mail.
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
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
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
}

// WantsEvent reports whether the channel is subscribed to eventKind.
func (c Config) WantsEvent(eventKind string) bool {
	if len(c.Events) == 0 {
		return eventKind == "checks"
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

// Message is one notification. Text is plain text; Logs are shown as a block.
type Message struct {
	Status string   `json:"status"` // down | up | test
	Title  string   `json:"title"`
	Text   string   `json:"text"`
	Logs   []string `json:"logs,omitempty"`
	URL    string   `json:"url,omitempty"`
	Data   any      `json:"data,omitempty"` // structured details for webhooks
}

func (m Message) plain(maxLen int) string {
	var b strings.Builder
	b.WriteString(m.Text)
	if len(m.Logs) > 0 {
		b.WriteString("\n\nLast log lines:\n")
		b.WriteString(strings.Join(m.Logs, "\n"))
	}
	if m.URL != "" {
		b.WriteString("\n\n" + m.URL)
	}
	return clip(b.String(), maxLen)
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

// Send delivers m through a channel of the given kind.
func Send(ctx context.Context, kind string, c Config, m Message) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	switch kind {
	case "webhook":
		return sendWebhook(ctx, c, m)
	case "ntfy":
		return sendNtfy(ctx, c, m)
	case "telegram":
		return sendTelegram(ctx, c, m)
	case "discord":
		return sendDiscord(ctx, c, m)
	case "smtp":
		return sendSMTP(ctx, c, m)
	case "slack":
		return sendSlack(ctx, c, m)
	case "gotify":
		return sendGotify(ctx, c, m)
	case "pushover":
		return sendPushover(ctx, c, m)
	case "matrix":
		return sendMatrix(ctx, c, m)
	}
	return fmt.Errorf("unknown channel type %q", kind)
}

func post(ctx context.Context, u, contentType string, body []byte, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "Moorage")
	for k, v := range headers {
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

func sendWebhook(ctx context.Context, c Config, m Message) error {
	body, err := json.Marshal(map[string]any{
		"event": m.Status, "title": m.Title, "text": m.Text, "logs": m.Logs, "url": m.URL, "data": m.Data,
		"sentAt": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	h := map[string]string{}
	for k, v := range c.Headers {
		if k != "" && v != "" {
			h[headerSafe(k)] = headerSafe(v)
		}
	}
	if c.Token != "" {
		h["Authorization"] = "Bearer " + c.Token
	}
	return post(ctx, c.URL, "application/json", body, h)
}

func sendSlack(ctx context.Context, c Config, m Message) error {
	color := "#FF7A45"
	switch m.Status {
	case "down":
		color = "#F2555A"
	case "up":
		color = "#4CC38A"
	}
	body, err := json.Marshal(map[string]any{
		"text": m.Title,
		"attachments": []map[string]any{{
			"color": color,
			"text":  m.plain(3000),
		}},
	})
	if err != nil {
		return err
	}
	return post(ctx, c.URL, "application/json", body, nil)
}

func sendGotify(ctx context.Context, c Config, m Message) error {
	prio := 5
	if m.Status == "down" {
		prio = 8
	} else if m.Status == "up" {
		prio = 4
	}
	body, err := json.Marshal(map[string]any{
		"title":    m.Title,
		"message":  m.plain(4000),
		"priority": prio,
	})
	if err != nil {
		return err
	}
	u := strings.TrimRight(c.URL, "/") + "/message"
	return post(ctx, u, "application/json", body, map[string]string{"X-Gotify-Key": c.Token})
}

func sendPushover(ctx context.Context, c Config, m Message) error {
	prio := "0"
	if m.Status == "down" {
		prio = "1"
	}
	form := url.Values{
		"token":    {c.Token},
		"user":     {c.ChatID},
		"title":    {clip(m.Title, 250)},
		"message":  {m.plain(1024)},
		"priority": {prio},
	}
	if m.URL != "" {
		form.Set("url", m.URL)
	}
	return post(ctx, "https://api.pushover.net/1/messages.json", "application/x-www-form-urlencoded", []byte(form.Encode()), nil)
}

func sendMatrix(ctx context.Context, c Config, m Message) error {
	var rb [8]byte
	_, _ = rand.Read(rb[:])
	txnID := hex.EncodeToString(rb[:])
	u := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		strings.TrimRight(c.URL, "/"), url.PathEscape(c.ChatID), txnID)
	body, err := json.Marshal(map[string]string{
		"msgtype": "m.text",
		"body":    m.Title + "\n\n" + m.plain(4000),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("User-Agent", "Moorage")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("matrix answered %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

func sendNtfy(ctx context.Context, c Config, m Message) error {
	h := map[string]string{"Title": mime.BEncoding.Encode("utf-8", headerSafe(m.Title))} // ntfy decodes RFC 2047
	switch m.Status {
	case "down":
		h["Priority"], h["Tags"] = "5", "rotating_light"
	case "up":
		h["Priority"], h["Tags"] = "3", "white_check_mark"
	default:
		h["Priority"], h["Tags"] = "2", "test_tube"
	}
	if m.URL != "" {
		h["Click"] = headerSafe(m.URL)
	}
	if c.Token != "" {
		h["Authorization"] = "Bearer " + c.Token
	}
	m2 := m
	m2.URL = "" // already a click action
	return post(ctx, c.URL, "text/plain; charset=utf-8", []byte(m2.plain(3500)), h)
}

func sendTelegram(ctx context.Context, c Config, m Message) error {
	body, err := json.Marshal(map[string]any{
		"chat_id":                  c.ChatID,
		"text":                     clip(m.Title+"\n\n"+m.plain(3900), 4000), // plain text: no parse mode, nothing to inject
		"disable_web_page_preview": true,
	})
	if err != nil {
		return err
	}
	return post(ctx, "https://api.telegram.org/bot"+c.Token+"/sendMessage", "application/json", body, nil)
}

func sendDiscord(ctx context.Context, c Config, m Message) error {
	var b strings.Builder
	b.WriteString("**" + discordEscape(m.Title) + "**\n" + discordEscape(m.Text))
	if len(m.Logs) > 0 {
		logs := strings.ReplaceAll(strings.Join(m.Logs, "\n"), "```", "ˋˋˋ")
		b.WriteString("\n```\n" + clip(logs, 1200) + "\n```")
	}
	if m.URL != "" {
		b.WriteString("\n" + m.URL)
	}
	body, err := json.Marshal(map[string]any{
		"content":          clip(b.String(), 1990),
		"allowed_mentions": map[string]any{"parse": []string{}}, // log lines must never ping @everyone
	})
	if err != nil {
		return err
	}
	return post(ctx, c.URL, "application/json", body, nil)
}

func discordEscape(s string) string {
	r := strings.NewReplacer("*", "\\*", "_", "\\_", "~", "\\~", "`", "\\`", "|", "\\|", ">", "\\>")
	return r.Replace(s)
}

func sendSMTP(ctx context.Context, c Config, m Message) error {
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
	from, _ := mail.ParseAddress(c.From)
	if err := cl.Mail(from.Address); err != nil {
		return err
	}
	var to []string
	for _, r := range recipients(c.To) {
		a, _ := mail.ParseAddress(r)
		if err := cl.Rcpt(a.Address); err != nil {
			return err
		}
		to = append(to, a.String())
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	id := make([]byte, 12)
	rand.Read(id) //nolint:errcheck
	var msg strings.Builder
	msg.WriteString("From: " + from.String() + "\r\n")
	msg.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	msg.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", headerSafe(m.Title)) + "\r\n")
	msg.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	msg.WriteString("Message-ID: <" + hex.EncodeToString(id) + "@moorage>\r\n")
	msg.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	body := strings.ReplaceAll(m.plain(100_000), "\r\n", "\n")
	msg.WriteString(strings.ReplaceAll(body, "\n", "\r\n") + "\r\n")
	if _, err := io.WriteString(w, msg.String()); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return cl.Quit()
}

// headerSafe removes line breaks so text can't inject headers.
func headerSafe(s string) string {
	return clip(strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r < 0x20 {
			return ' '
		}
		return r
	}, s), 200)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
