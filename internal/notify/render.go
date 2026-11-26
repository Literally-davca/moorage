package notify

import (
	"encoding/json"
	"fmt"
	"mime"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Request is a rendered notification, ready to send (or to compare in golden tests).
// SMTP messages use Method "SMTP" with the whole MIME message in Body.
type Request struct {
	Method      string            `json:"method"`
	URL         string            `json:"url"`
	ContentType string            `json:"contentType,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Body        []byte            `json:"-"`
}

// Hooks for deterministic tests.
var (
	nowFn  = time.Now
	randFn = func(n int) string {
		b := make([]byte, n)
		_, _ = cryptoRand(b)
		return fmt.Sprintf("%x", b)
	}
)

// Render builds the request a channel sends for m, after the channel's preset and format
// are applied. It never contacts the service.
func Render(kind string, c Config, m Message) (Request, error) {
	v := BuildView(c, m)
	switch kind {
	case "webhook":
		return renderWebhook(c, m, v)
	case "discord":
		return renderDiscord(c, m, v)
	case "slack":
		return renderSlack(c, m, v)
	case "telegram":
		return renderTelegram(c, m, v)
	case "matrix":
		return renderMatrix(c, m, v)
	case "gotify":
		return renderGotify(c, m, v)
	case "ntfy":
		return renderNtfy(c, m, v)
	case "pushover":
		return renderPushover(c, m, v)
	case "smtp":
		return renderEmail(c, m, v)
	}
	return Request{}, fmt.Errorf("unknown channel type %q", kind)
}

func jsonRequest(method, u string, body any, headers map[string]string) (Request, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return Request{}, err
	}
	return Request{Method: method, URL: u, ContentType: "application/json", Headers: headers, Body: b}, nil
}

// ---- fitting a view into a service's size limit ------------------------------------------

// fitView shrinks a view until render(v) fits limit (measured by size): first the log
// lines, then the fields ("…and N more"), then the description. It never refuses a
// message for its length; ok=false means even the bare title doesn't fit.
func fitView(v View, limit int, size func(string) int, render func(View) string) (string, bool) {
	out := render(v)
	base := append([]Field(nil), v.Fields...)
	keep, dropped := len(base), 0
	for i := 0; size(out) > limit && i < 400; i++ {
		switch {
		case len(v.Logs) > 1:
			v.Logs = v.Logs[len(v.Logs)/2:]
		case len(v.Logs) == 1:
			v.Logs = nil
		case keep > 0:
			keep--
			dropped++
			v.Fields = append(append([]Field(nil), base[:keep]...), Field{Name: "…", Value: "and " + strconv.Itoa(dropped) + " more"})
		case runeLen(v.Description) > 40:
			v.Description = clip(v.Description, runeLen(v.Description)*3/4)
		case v.Fields != nil:
			v.Fields = nil
		case runeLen(v.Title) > 20:
			v.Title = clip(v.Title, runeLen(v.Title)*3/4)
		default:
			return out, false
		}
		out = render(v)
	}
	return out, size(out) <= limit
}

func byteLen(s string) int { return len(s) }

// statusColor is the accent of a status (Discord / Slack).
func statusColor(status string) int {
	switch status {
	case "down":
		return 0xF2555A
	case "up":
		return 0x4CC38A
	}
	return 0xFF7A45
}

func statusMark(status string) string {
	switch status {
	case "down":
		return "🔴"
	case "up":
		return "🟢"
	}
	return "🔵"
}

// ---- webhook (generic JSON) ----------------------------------------------------------

func renderWebhook(c Config, m Message, v View) (Request, error) {
	h := map[string]string{}
	for k, val := range c.Headers {
		if k != "" && val != "" {
			h[headerSafe(k)] = headerSafe(val)
		}
	}
	if c.Token != "" {
		h["Authorization"] = "Bearer " + c.Token
	}
	fields := v.Fields
	if fields == nil {
		fields = []Field{}
	}
	// The keys before M17B stay as they were (automations rely on them); M17B only adds.
	return jsonRequest("POST", c.URL, map[string]any{
		"event": m.Status, "title": m.Title, "text": m.Text, "logs": m.Logs, "url": m.URL, "data": m.Data,
		"sentAt":    nowFn().UTC().Format(time.RFC3339),
		"eventKind": m.Event,
		"sender":    map[string]string{"name": v.Sender, "host": v.SenderHost, "role": v.SenderRole, "iconUrl": v.IconURL},
		"preset":    v.Preset,
		"fields":    fields,
	}, h)
}

// ---- Discord ----------------------------------------------------------------------------

const (
	discordTitleMax  = 256
	discordDescMax   = 4096
	discordFieldName = 256
	discordFieldMax  = 1024
	discordFields    = 25
	discordTotalMax  = 6000
	discordFooterMax = 2048
)

func renderDiscord(c Config, m Message, v View) (Request, error) {
	body := map[string]any{
		"username":         discordUsername(v.Sender),
		"avatar_url":       v.IconURL,
		"allowed_mentions": map[string]any{"parse": []string{}}, // log lines must never ping @everyone
	}
	if !c.rich("discord") {
		var b strings.Builder
		b.WriteString("**" + discordEscape(v.Title) + "**")
		rest := v
		rest.Logs = nil
		if t := rest.plainText(false); t != "" {
			b.WriteString("\n" + discordEscape(t))
		}
		if len(v.Logs) > 0 {
			logs := make([]string, len(v.Logs))
			for i, l := range v.Logs {
				logs[i] = codeBlockSafe(clipLogLine(l))
			}
			b.WriteString("\n```\n" + clip(strings.Join(logs, "\n"), 1200) + "\n```")
		}
		body["content"] = clip(b.String(), 1990)
		return jsonRequest("POST", c.URL, body, nil)
	}
	body["embeds"] = []any{discordEmbed(v)}
	return jsonRequest("POST", c.URL, body, nil)
}

type discordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

func discordEmbed(v View) map[string]any {
	title := clip(discordEscape(v.Title), discordTitleMax)
	footer := clip(v.Footer, discordFooterMax)
	desc := func(v View) string {
		d := discordEscape(v.Description)
		if len(v.Logs) > 0 {
			logs := make([]string, len(v.Logs))
			for i, l := range v.Logs {
				logs[i] = codeBlockSafe(clipLogLine(l))
			}
			block := "```\n" + strings.Join(logs, "\n") + "\n```"
			if d != "" {
				d += "\n"
			}
			d += block
		}
		return d
	}
	fieldsOf := func(v View) []discordField {
		var out []discordField
		for i, f := range v.Fields {
			if i == discordFields-1 && len(v.Fields) > discordFields {
				out = append(out, discordField{Name: "…", Value: "and " + strconv.Itoa(len(v.Fields)-i) + " more"})
				break
			}
			out = append(out, discordField{Name: clip(discordEscape(f.Name), discordFieldName), Value: clip(discordEscape(f.Value), discordFieldMax), Inline: f.Inline})
		}
		return out
	}
	// The whole embed is measured as Discord counts it: title + description + fields + footer + author.
	measure := func(v View) string {
		var b strings.Builder
		b.WriteString(title + footer + v.Sender + desc(v))
		for _, f := range fieldsOf(v) {
			b.WriteString(f.Name + f.Value)
		}
		return b.String()
	}
	fitted := v
	_, _ = fitView(fitted, discordTotalMax, runeLen, func(x View) string { fitted = x; return measure(x) })
	d := desc(fitted)
	for runeLen(d) > discordDescMax && len(fitted.Logs) > 0 {
		fitted.Logs = fitted.Logs[len(fitted.Logs)/2:]
		if len(fitted.Logs) == 1 {
			fitted.Logs = nil
		}
		d = desc(fitted)
	}
	e := map[string]any{
		"title":  title,
		"color":  statusColor(v.Status),
		"footer": map[string]string{"text": footer, "icon_url": v.IconURL},
		"author": map[string]string{"name": clip(v.Sender, 256), "icon_url": v.IconURL},
	}
	if d = clip(d, discordDescMax); d != "" {
		e["description"] = d
	}
	if fs := fieldsOf(fitted); len(fs) > 0 {
		e["fields"] = fs
	}
	if v.URL != "" {
		e["url"] = v.URL
	}
	if !v.Timestamp.IsZero() {
		e["timestamp"] = v.Timestamp.UTC().Format(time.RFC3339)
	}
	return e
}

// ---- Slack ------------------------------------------------------------------------------

func renderSlack(c Config, m Message, v View) (Request, error) {
	if !c.rich("slack") {
		return jsonRequest("POST", c.URL, map[string]any{"text": clip(v.Title+"\n\n"+v.plainText(false), 3000)}, nil)
	}
	text, _ := fitView(v, 3500, byteLen, func(x View) string { return slackText(x) })
	var fields []map[string]any
	fv := v
	_, _ = fitView(fv, 7000, byteLen, func(x View) string {
		fv = x
		var b strings.Builder
		for _, f := range x.Fields {
			b.WriteString(f.Name + f.Value)
		}
		return b.String()
	})
	for _, f := range fv.Fields {
		fields = append(fields, map[string]any{"title": slackEscape(clip(f.Name, 200)), "value": slackEscape(clip(f.Value, 2000)), "short": f.Inline})
	}
	att := map[string]any{
		"color":     fmt.Sprintf("#%06X", statusColor(v.Status)),
		"fallback":  clip(v.Title, 300),
		"title":     slackEscape(clip(v.Title, 300)),
		"footer":    slackEscape(v.Footer),
		"mrkdwn_in": []string{"text"},
	}
	if text != "" {
		att["text"] = text
	}
	if len(fields) > 0 {
		att["fields"] = fields
	}
	if v.URL != "" {
		att["title_link"] = v.URL
	}
	if !v.Timestamp.IsZero() {
		att["ts"] = v.Timestamp.Unix()
	}
	// username / icon_url only take effect on legacy incoming webhooks; Slack apps ignore them.
	return jsonRequest("POST", c.URL, map[string]any{
		"text":        slackEscape(clip(v.Title, 300)),
		"username":    v.Sender,
		"icon_url":    v.IconURL,
		"attachments": []any{att},
	}, nil)
}

func slackText(v View) string {
	d := slackEscape(v.Description)
	if len(v.Logs) > 0 {
		logs := make([]string, len(v.Logs))
		for i, l := range v.Logs {
			logs[i] = slackEscape(codeBlockSafe(clipLogLine(l)))
		}
		if d != "" {
			d += "\n"
		}
		d += "```\n" + strings.Join(logs, "\n") + "\n```"
	}
	return d
}

// ---- HTML body shared by Telegram, Matrix, Pushover and e-mail ---------------------------

type htmlStyle struct {
	br     string // line break
	pre    func(string) string
	link   func(u, text string) string
	sender func(string) string // wraps the escaped sender name
	noPre  bool                // Pushover: no <pre>, logs as lines
}

func renderHTML(v View, st htmlStyle, withSender, withTitle, withLink bool) string {
	var parts []string
	if withSender {
		parts = append(parts, st.sender(htmlEscape(v.Sender)))
	}
	if withTitle {
		parts = append(parts, statusMark(v.Status)+" <b>"+htmlEscape(v.Title)+"</b>")
	}
	head := strings.Join(parts, st.br)
	var blocks []string
	if head != "" {
		blocks = append(blocks, head)
	}
	if v.Description != "" {
		blocks = append(blocks, strings.ReplaceAll(htmlEscape(v.Description), "\n", st.br))
	}
	if len(v.Fields) > 0 {
		var fl []string
		for _, f := range v.Fields {
			fl = append(fl, "<b>"+htmlEscape(f.Name)+":</b> "+strings.ReplaceAll(htmlEscape(f.Value), "\n", st.br))
		}
		blocks = append(blocks, strings.Join(fl, st.br))
	}
	if len(v.Logs) > 0 {
		logs := make([]string, len(v.Logs))
		for i, l := range v.Logs {
			logs[i] = htmlEscape(clipLogLine(l))
		}
		if st.noPre {
			blocks = append(blocks, "<i>Last log lines</i>"+st.br+strings.Join(logs, st.br))
		} else {
			blocks = append(blocks, st.pre(strings.Join(logs, "\n")))
		}
	}
	if withLink && v.URL != "" {
		blocks = append(blocks, st.link(v.URL, "Open in Moorage"))
	}
	return strings.Join(blocks, st.br+st.br)
}

func bold(s string) string { return "<b>" + s + "</b>" }

func hrefAttr(u string) string {
	if p, err := url.Parse(u); err != nil || (p.Scheme != "http" && p.Scheme != "https") {
		return ""
	}
	return htmlEscape(u)
}

// ---- Telegram --------------------------------------------------------------------------

const telegramMax = 4096

func renderTelegram(c Config, m Message, v View) (Request, error) {
	u := "https://api.telegram.org/bot" + c.Token + "/sendMessage"
	plain := func() (Request, error) {
		return jsonRequest("POST", u, map[string]any{
			"chat_id":                  c.ChatID,
			"text":                     clip(v.Sender+"\n"+v.Title+"\n\n"+v.plainText(false), 4000), // plain text: no parse mode, nothing to inject
			"disable_web_page_preview": true,
		}, nil)
	}
	if !c.rich("telegram") {
		return plain()
	}
	button := v.URL != "" && telegramButtonURL(v.URL)
	st := htmlStyle{br: "\n", sender: bold,
		pre: func(s string) string { return "<pre>" + s + "</pre>" },
		link: func(u, text string) string {
			if h := hrefAttr(u); h != "" {
				return `<a href="` + h + `">` + text + `</a>`
			}
			return htmlEscape(u)
		}}
	text, ok := fitView(v, telegramMax-96, byteLen, func(x View) string { return renderHTML(x, st, true, true, !button) })
	if !ok {
		return plain()
	}
	body := map[string]any{
		"chat_id":                  c.ChatID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	if button {
		body["reply_markup"] = map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "Open in Moorage", "url": v.URL}}}}
	}
	return jsonRequest("POST", u, body, nil)
}

// telegramButtonURL: Telegram rejects inline buttons with localhost, IP or single-label
// hosts, which would fail the whole message; those links go into the text instead.
func telegramButtonURL(raw string) bool {
	p, err := url.Parse(raw)
	if err != nil || (p.Scheme != "https" && p.Scheme != "http") {
		return false
	}
	h := p.Hostname()
	return h != "" && strings.Contains(h, ".") && net.ParseIP(h) == nil && !strings.HasSuffix(h, ".localhost")
}

// ---- Matrix -------------------------------------------------------------------------------

func renderMatrix(c Config, m Message, v View) (Request, error) {
	u := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		strings.TrimRight(c.URL, "/"), url.PathEscape(c.ChatID), randFn(8))
	h := map[string]string{"Authorization": "Bearer " + c.Token}
	plainBody, _ := fitView(v, 16000, byteLen, func(x View) string { return x.Sender + "\n" + x.Title + "\n\n" + x.plainText(false) })
	body := map[string]any{"msgtype": "m.text", "body": plainBody}
	if c.rich("matrix") {
		st := htmlStyle{br: "<br>", sender: bold,
			pre: func(s string) string { return "<pre><code>" + s + "</code></pre>" },
			link: func(u, text string) string {
				if h := hrefAttr(u); h != "" {
					return `<a href="` + h + `">` + text + `</a>`
				}
				return htmlEscape(u)
			}}
		html, _ := fitView(v, 30000, byteLen, func(x View) string { return renderHTML(x, st, true, true, true) })
		body["format"] = "org.matrix.custom.html"
		body["formatted_body"] = html
	}
	return jsonRequest("PUT", u, body, h)
}

// ---- Gotify -------------------------------------------------------------------------------

func renderGotify(c Config, m Message, v View) (Request, error) {
	prio := 5
	if m.Status == "down" {
		prio = 8
	} else if m.Status == "up" {
		prio = 4
	}
	u := strings.TrimRight(c.URL, "/") + "/message"
	h := map[string]string{"X-Gotify-Key": c.Token}
	if !c.rich("gotify") {
		return jsonRequest("POST", u, map[string]any{"title": v.Title, "message": clip(v.plainText(false), 4000), "priority": prio}, h)
	}
	msg, _ := fitView(v, 8000, byteLen, func(x View) string { return markdownBody(x, true) })
	extras := map[string]any{"client::display": map[string]string{"contentType": "text/markdown"}}
	if v.URL != "" {
		extras["client::notification"] = map[string]any{"click": map[string]string{"url": v.URL}}
	}
	return jsonRequest("POST", u, map[string]any{"title": v.Title, "message": msg, "priority": prio, "extras": extras}, h)
}

// markdownBody is the CommonMark body of Gotify and ntfy messages.
func markdownBody(v View, withSender bool) string {
	var blocks []string
	if withSender {
		blocks = append(blocks, "**"+markdownEscape(v.Sender)+"**")
	}
	if v.Description != "" {
		blocks = append(blocks, strings.ReplaceAll(markdownEscape(v.Description), "\n", "  \n"))
	}
	if len(v.Fields) > 0 {
		var fl []string
		for _, f := range v.Fields {
			fl = append(fl, "**"+markdownEscape(f.Name)+":** "+strings.ReplaceAll(markdownEscape(f.Value), "\n", "  \n"))
		}
		blocks = append(blocks, strings.Join(fl, "  \n"))
	}
	if len(v.Logs) > 0 {
		logs := make([]string, len(v.Logs))
		for i, l := range v.Logs {
			logs[i] = codeBlockSafe(clipLogLine(l))
		}
		blocks = append(blocks, "```\n"+strings.Join(logs, "\n")+"\n```")
	}
	if v.URL != "" {
		if hrefAttr(v.URL) != "" {
			blocks = append(blocks, "[Open in Moorage](<"+strings.NewReplacer("<", "%3C", ">", "%3E", " ", "%20").Replace(v.URL)+">)")
		} else {
			blocks = append(blocks, markdownEscape(v.URL))
		}
	}
	return strings.Join(blocks, "\n\n")
}

// ---- ntfy -----------------------------------------------------------------------------------

func renderNtfy(c Config, m Message, v View) (Request, error) {
	title := v.Sender + ": " + v.Title
	h := map[string]string{"Title": mime.BEncoding.Encode("utf-8", headerSafe(title))} // ntfy decodes RFC 2047
	switch m.Status {
	case "down":
		h["Priority"], h["Tags"] = "5", "rotating_light"
	case "up":
		h["Priority"], h["Tags"] = "3", "white_check_mark"
	default:
		h["Priority"], h["Tags"] = "2", "test_tube"
	}
	if v.URL != "" {
		h["Click"] = headerSafe(v.URL)
	}
	if c.Token != "" {
		h["Authorization"] = "Bearer " + c.Token
	}
	h["Icon"] = headerSafe(v.IconURL)
	var body string
	if c.rich("ntfy") {
		h["Markdown"] = "yes"
		if v.URL != "" && hrefAttr(v.URL) != "" && !strings.ContainsAny(v.URL, ",;") {
			h["Actions"] = "view, Open, " + headerSafe(v.URL)
		}
		noLink := v
		noLink.URL = "" // already a click action and a button
		body, _ = fitView(noLink, 3500, byteLen, func(x View) string { return markdownBody(x, false) })
	} else {
		noLink := v
		noLink.URL = ""
		body, _ = fitView(noLink, 3500, byteLen, func(x View) string { return x.plainText(false) })
	}
	return Request{Method: "POST", URL: c.URL, ContentType: "text/plain; charset=utf-8", Headers: h, Body: []byte(body)}, nil
}

// ---- Pushover --------------------------------------------------------------------------------

func renderPushover(c Config, m Message, v View) (Request, error) {
	prio := "0"
	if m.Status == "down" {
		prio = "1"
	}
	form := url.Values{
		"token":    {c.Token},
		"user":     {c.ChatID},
		"title":    {clip(v.Sender+": "+v.Title, 250)},
		"priority": {prio},
	}
	noLink := v
	noLink.URL = ""
	if c.rich("pushover") {
		st := htmlStyle{br: "\n", sender: bold, noPre: true, pre: func(s string) string { return s }, link: func(string, string) string { return "" }}
		msg, ok := fitView(noLink, 1024, runeLen, func(x View) string { return renderHTML(x, st, false, false, false) })
		if ok && strings.TrimSpace(msg) != "" {
			form.Set("message", msg)
			form.Set("html", "1")
		}
	}
	if form.Get("message") == "" {
		msg, _ := fitView(noLink, 1024, runeLen, func(x View) string { return x.plainText(false) })
		if strings.TrimSpace(msg) == "" {
			msg = v.Title // Pushover needs a message
		}
		form.Set("message", clip(msg, 1024))
	}
	if v.URL != "" {
		form.Set("url", v.URL)
		form.Set("url_title", "Open in Moorage")
	}
	return Request{Method: "POST", URL: "https://api.pushover.net/1/messages.json", ContentType: "application/x-www-form-urlencoded", Body: []byte(form.Encode())}, nil
}

// ---- e-mail -------------------------------------------------------------------------------

func renderEmail(c Config, m Message, v View) (Request, error) {
	from, err := parseAddress(c.From)
	if err != nil {
		return Request{}, err
	}
	from.Name = headerSafe(v.Sender) // "nas • Moorage agent <alerts@…>"
	var to []string
	for _, r := range recipients(c.To) {
		a, err := parseAddress(r)
		if err != nil {
			return Request{}, err
		}
		to = append(to, a.String())
	}
	var msg strings.Builder
	msg.WriteString("From: " + from.String() + "\r\n")
	msg.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	msg.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", headerSafe(v.Title)) + "\r\n")
	msg.WriteString("Date: " + nowFn().Format(time.RFC1123Z) + "\r\n")
	msg.WriteString("Message-ID: <" + randFn(12) + "@moorage>\r\n")
	msg.WriteString("MIME-Version: 1.0\r\n")
	crlf := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n") }
	text := crlf(v.Title + "\n\n" + v.plainText(false))
	if !c.rich("smtp") {
		msg.WriteString("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
		msg.WriteString(text + "\r\n")
		return Request{Method: "SMTP", URL: "smtp://" + c.Host + ":" + strconv.Itoa(c.Port), Body: []byte(msg.String())}, nil
	}
	st := htmlStyle{br: "<br>\r\n", sender: func(s string) string { return `<span style="color:#888">` + s + "</span>" },
		pre: func(s string) string {
			return `<pre style="background:#f4f4f5;padding:8px;border-radius:6px;white-space:pre-wrap">` + s + "</pre>"
		},
		link: func(u, text string) string {
			if h := hrefAttr(u); h != "" {
				return `<a href="` + h + `">` + text + `</a>`
			}
			return htmlEscape(u)
		}}
	body := renderHTML(v, st, true, true, true)
	html := "<!doctype html>\r\n<html><body style=\"font-family:system-ui,sans-serif;font-size:14px;line-height:1.45\">\r\n" +
		`<div style="border-left:4px solid ` + fmt.Sprintf("#%06X", statusColor(v.Status)) + `;padding-left:12px">` + "\r\n" + crlf(body) + "\r\n</div>\r\n" +
		`<p style="color:#888;font-size:12px">` + htmlEscape(v.Footer) + "</p>\r\n</body></html>"
	boundary := "moorage-" + randFn(12)
	msg.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n\r\n")
	msg.WriteString("--" + boundary + "\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" + text + "\r\n")
	msg.WriteString("--" + boundary + "\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" + html + "\r\n")
	msg.WriteString("--" + boundary + "--\r\n")
	return Request{Method: "SMTP", URL: "smtp://" + c.Host + ":" + strconv.Itoa(c.Port), Body: []byte(msg.String())}, nil
}
