package notify

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

var allKinds = []string{"webhook", "discord", "slack", "telegram", "matrix", "gotify", "ntfy", "pushover", "smtp"}

func fixedClock(t *testing.T) {
	t.Helper()
	oldNow, oldRand := nowFn, randFn
	nowFn = func() time.Time { return time.Date(2026, 11, 26, 12, 0, 5, 0, time.UTC) }
	randFn = func(n int) string { return strings.Repeat("ab", n) }
	SetIconBase("")
	t.Cleanup(func() { nowFn, randFn = oldNow, oldRand; SetIconBase("") })
}

// goldenMessage is a check alert with every part set and hostile text from a container.
func goldenMessage() Message {
	since := time.Date(2026, 11, 26, 11, 55, 53, 0, time.UTC)
	return Message{
		Status: "down",
		Event:  EventChecks,
		Sender: Sender{Host: "nas", Role: RoleAgent},
		Title:  "DOWN: web *prod* (stack web on nas)",
		Text:   "web: HTTP 503 <b>Service Unavailable</b> (HTTP https://web.example.com/health)\nContainer web-1 exited with code 137.",
		Logs: []string{
			"11:55:50 web-1 | @everyone <script>alert(1)</script>",
			"11:55:51 web-1 | ```break out``` **bold** [link](https://evil.example)",
			"11:55:52 web-1 | Killed",
		},
		URL:    "https://moorage.example.com/#/h/nas/stacks/web",
		Host:   "nas",
		Stack:  "web",
		Cause:  "HTTP 503",
		Since:  since,
		Until:  since.Add(4 * time.Minute),
		Images: []ImageChange{{Service: "app", From: "nginx:1.27.2", To: "nginx:1.27.3"}},
		Checks: []CheckDetail{{Name: "web", Kind: "HTTP", Target: "https://web.example.com/health", Cause: "HTTP 503",
			Locations: []LocationDetail{{Name: "nas", Vote: "down", LatencyMS: 120, Msg: "503"}, {Name: "vps", Vote: "up", LatencyMS: 40}}}},
		Duration: 4 * time.Minute,
	}
}

func channelConfig(kind string) Config {
	switch kind {
	case "webhook":
		return Config{URL: "https://hooks.example.com/moorage", Token: "wh-token", Headers: map[string]string{"X-Env": "prod"}}
	case "discord":
		return Config{URL: "https://discord.com/api/webhooks/1/secret"}
	case "slack":
		return Config{URL: "https://hooks.slack.com/services/T/B/secret"}
	case "telegram":
		return Config{Token: "123:abc", ChatID: "42"}
	case "matrix":
		return Config{URL: "https://matrix.example.com", Token: "mx-token", ChatID: "!room:example.com"}
	case "gotify":
		return Config{URL: "https://gotify.example.com", Token: "gt-token"}
	case "ntfy":
		return Config{URL: "https://ntfy.sh/moorage-alerts", Token: "nt-token"}
	case "pushover":
		return Config{Token: "po-token", ChatID: "po-user"}
	case "smtp":
		return Config{Host: "smtp.example.com", Port: 587, From: "Alerts <alerts@example.com>", To: "ops@example.com"}
	}
	return Config{}
}

var presets = map[string]Config{
	PresetMinimal:  {Preset: PresetMinimal},
	PresetStandard: {},
	PresetDetailed: {Preset: PresetDetailed},
	PresetCustom:   {Preset: PresetCustom, Include: map[string]bool{"stack": true, "logs": true, "image": true, "link": true}},
}

func dump(r Request) string {
	var b strings.Builder
	b.WriteString(r.Method + " " + r.URL + "\n")
	if r.ContentType != "" {
		b.WriteString("Content-Type: " + r.ContentType + "\n")
	}
	keys := make([]string, 0, len(r.Headers))
	for k := range r.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(k + ": " + r.Headers[k] + "\n")
	}
	b.WriteString("\n")
	switch {
	case strings.HasPrefix(r.ContentType, "application/json"):
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, r.Body, "", "  "); err != nil {
			b.Write(r.Body)
		} else {
			b.Write(pretty.Bytes())
		}
	case strings.HasPrefix(r.ContentType, "application/x-www-form-urlencoded"):
		vals, _ := url.ParseQuery(string(r.Body))
		ks := make([]string, 0, len(vals))
		for k := range vals {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			b.WriteString(k + "=" + vals.Get(k) + "\n")
		}
	default:
		b.WriteString(strings.ReplaceAll(string(r.Body), "\r\n", "\n"))
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func TestGoldenRenders(t *testing.T) {
	fixedClock(t)
	for _, kind := range allKinds {
		for name, p := range presets {
			c := channelConfig(kind)
			c.Preset, c.Include = p.Preset, p.Include
			r, err := Render(kind, c, goldenMessage())
			if err != nil {
				t.Fatalf("%s/%s: %v", kind, name, err)
			}
			checkGolden(t, kind+"_"+name, dump(r))
		}
		if SupportsRich(kind) {
			c := channelConfig(kind)
			c.Format = FormatPlain
			r, err := Render(kind, c, goldenMessage())
			if err != nil {
				t.Fatalf("%s/plain: %v", kind, err)
			}
			checkGolden(t, kind+"_standard_plain", dump(r))
		}
	}
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (run go test ./internal/notify -run Golden -update)", name, err)
	}
	if w := strings.ReplaceAll(string(want), "\r\n", "\n"); w != got {
		t.Errorf("%s differs from %s:\n--- got ---\n%s", name, path, got)
	}
}

func TestPresetParts(t *testing.T) {
	m := goldenMessage()
	min := BuildView(Config{Preset: PresetMinimal}, m)
	if min.Description != "" || len(min.Logs) != 0 || len(min.Fields) != 1 || min.Fields[0].Name != "Host" || min.URL == "" {
		t.Fatalf("minimal: %+v", min)
	}
	std := BuildView(Config{}, m)
	names := func(v View) string {
		var n []string
		for _, f := range v.Fields {
			n = append(n, f.Name)
		}
		return strings.Join(n, ",")
	}
	if names(std) != "Host,Stack,Duration" || len(std.Logs) != 3 || std.Description == "" || !std.Timestamp.IsZero() {
		t.Fatalf("standard: %s %+v", names(std), std)
	}
	det := BuildView(Config{Preset: PresetDetailed}, m)
	if names(det) != "Host,Stack,Duration,Image,web,Started,Ended" || det.Timestamp.IsZero() {
		t.Fatalf("detailed: %s", names(det))
	}
	cus := BuildView(Config{Preset: PresetCustom, Include: map[string]bool{"image": true}}, m)
	if names(cus) != "Image" || cus.URL != "" || cus.Title == "" {
		t.Fatalf("custom: %s %+v", names(cus), cus)
	}
	// An existing channel (no preset saved) is Standard.
	if (Config{}).Parts() != (Config{Preset: PresetStandard}).Parts() {
		t.Fatal("missing preset must be standard")
	}
	if err := Validate("ntfy", Config{URL: "https://ntfy.sh/x", Preset: "loud"}); err == nil {
		t.Fatal("unknown preset accepted")
	}
	if err := Validate("ntfy", Config{URL: "https://ntfy.sh/x", Preset: PresetCustom, Include: map[string]bool{"secrets": true}}); err == nil {
		t.Fatal("unknown part accepted")
	}
	if err := Validate("ntfy", Config{URL: "https://ntfy.sh/x", Format: "fancy"}); err == nil {
		t.Fatal("unknown format accepted")
	}
}

func TestEscaping(t *testing.T) {
	fixedClock(t)
	m := goldenMessage()
	body := func(kind string, c Config) string {
		r, err := Render(kind, c, m)
		if err != nil {
			t.Fatal(err)
		}
		return string(r.Body)
	}
	var d struct {
		AllowedMentions struct{ Parse []string } `json:"allowed_mentions"`
		Embeds          []struct {
			Title       string
			Description string
		}
	}
	if err := json.Unmarshal([]byte(body("discord", channelConfig("discord"))), &d); err != nil {
		t.Fatal(err)
	}
	if d.AllowedMentions.Parse == nil || len(d.AllowedMentions.Parse) != 0 {
		t.Fatal("discord must disable mentions")
	}
	desc := d.Embeds[0].Description
	if strings.Count(desc, "```") != 2 || strings.Contains(desc, "```break") {
		t.Fatalf("a log line broke out of the code block: %q", desc)
	}
	if !strings.Contains(d.Embeds[0].Title, `\*prod\*`) {
		t.Fatalf("title not escaped: %q", d.Embeds[0].Title)
	}

	tg := body("telegram", channelConfig("telegram"))
	var tm struct{ Text string }
	json.Unmarshal([]byte(tg), &tm) //nolint:errcheck
	if strings.Contains(tm.Text, "<script>") || !strings.Contains(tm.Text, "&lt;script&gt;") || strings.Contains(tm.Text, "<b>Service") {
		t.Fatalf("telegram HTML not escaped: %q", tm.Text)
	}

	var mx struct {
		FormattedBody string `json:"formatted_body"`
	}
	json.Unmarshal([]byte(body("matrix", channelConfig("matrix"))), &mx) //nolint:errcheck
	if strings.Contains(mx.FormattedBody, "<script>") || strings.Contains(mx.FormattedBody, "<b>Service") {
		t.Fatalf("matrix HTML not escaped: %q", mx.FormattedBody)
	}

	var gm struct{ Message string }
	json.Unmarshal([]byte(body("gotify", channelConfig("gotify"))), &gm) //nolint:errcheck
	for kind, b := range map[string]string{"gotify": gm.Message, "ntfy": body("ntfy", channelConfig("ntfy"))} {
		outside := b
		if i := strings.Index(b, "```"); i >= 0 {
			outside = b[:i] // log lines stay inside the code block
		}
		if strings.Contains(outside, "<b>Service") || !strings.Contains(outside, `\<b\>Service`) {
			t.Fatalf("%s: markdown/HTML from a container not escaped: %q", kind, outside)
		}
		if strings.Count(b, "```") != 2 {
			t.Fatalf("%s: a log line broke out of the code block: %q", kind, b)
		}
	}

	po := body("pushover", channelConfig("pushover"))
	vals, _ := url.ParseQuery(po)
	if vals.Get("html") != "1" || strings.Contains(vals.Get("message"), "<script>") {
		t.Fatalf("pushover: %+v", vals)
	}
	mail := body("smtp", channelConfig("smtp"))
	htmlPart := mail[strings.Index(mail, "Content-Type: text/html"):]
	if strings.Contains(htmlPart, "<script>") || !strings.Contains(htmlPart, "&lt;script&gt;") {
		t.Fatal("e-mail HTML not escaped")
	}
	if !strings.Contains(mail, "From: \"nas • Moorage agent\" <alerts@example.com>") && !strings.Contains(mail, "From: =?utf-8?") {
		t.Fatalf("e-mail sender: %q", strings.SplitN(mail, "\r\n", 2)[0])
	}
}

func TestDiscordUsername(t *testing.T) {
	cases := map[string]string{
		"nas • Moorage agent":          "nas • Moorage agent",
		"Discord-box • Moorage hub":    "-box • Moorage hub",
		"clyde • Moorage hub":          "Moorage hub",
		"@everyone":                    "Moorage",
		"a#b:c • Moorage hub":          "abc • Moorage hub",
		strings.Repeat("x", 120):       strings.Repeat("x", 79) + "…",
		"DISCORDclyde • Moorage agent": "Moorage agent",
	}
	for in, want := range cases {
		if got := discordUsername(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestLimits(t *testing.T) {
	fixedClock(t)
	m := goldenMessage()
	m.Text = strings.Repeat("A very long cause line. ", 400)
	m.Logs = nil
	for i := 0; i < 500; i++ {
		m.Logs = append(m.Logs, strings.Repeat("log ", 100))
	}
	for i := 0; i < 60; i++ {
		m.Checks = append(m.Checks, CheckDetail{Name: "check " + itoa(i), Kind: "HTTP", Target: strings.Repeat("t", 1500), Cause: "down"})
	}
	c := Config{Preset: PresetDetailed}

	r, err := Render("discord", withKind(c, "discord"), m)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Username string
		Embeds   []struct {
			Title, Description string
			Footer             struct{ Text string }
			Author             struct{ Name string }
			Fields             []struct{ Name, Value string }
		}
	}
	if err := json.Unmarshal(r.Body, &d); err != nil {
		t.Fatal(err)
	}
	e := d.Embeds[0]
	total := utf8.RuneCountInString(e.Title) + utf8.RuneCountInString(e.Description) + utf8.RuneCountInString(e.Footer.Text) + utf8.RuneCountInString(e.Author.Name)
	for _, f := range e.Fields {
		total += utf8.RuneCountInString(f.Name) + utf8.RuneCountInString(f.Value)
		if utf8.RuneCountInString(f.Value) > 1024 || utf8.RuneCountInString(f.Name) > 256 {
			t.Fatalf("field too long: %d", utf8.RuneCountInString(f.Value))
		}
	}
	if total > 6000 || utf8.RuneCountInString(e.Description) > 4096 || len(e.Fields) > 25 || len(e.Fields) == 0 {
		t.Fatalf("discord limits: total %d desc %d fields %d", total, utf8.RuneCountInString(e.Description), len(e.Fields))
	}
	if last := e.Fields[len(e.Fields)-1]; !strings.HasPrefix(last.Value, "and ") {
		t.Fatalf("dropped fields must be counted: %+v", last)
	}

	r, _ = Render("telegram", withKind(c, "telegram"), m)
	var tg struct {
		Text      string
		ParseMode string `json:"parse_mode"`
	}
	json.Unmarshal(r.Body, &tg) //nolint:errcheck
	if len(tg.Text) > 4096 || tg.Text == "" || tg.ParseMode != "HTML" {
		t.Fatalf("telegram: %d bytes", len(tg.Text))
	}

	r, _ = Render("slack", withKind(c, "slack"), m)
	var sl struct {
		Attachments []struct {
			Text   string
			Fields []struct{ Value string }
		}
	}
	json.Unmarshal(r.Body, &sl) //nolint:errcheck
	for _, f := range sl.Attachments[0].Fields {
		if utf8.RuneCountInString(f.Value) > 2000 {
			t.Fatal("slack field over 2000")
		}
	}

	r, _ = Render("pushover", withKind(c, "pushover"), m)
	vals, _ := url.ParseQuery(string(r.Body))
	if utf8.RuneCountInString(vals.Get("message")) > 1024 || vals.Get("message") == "" || utf8.RuneCountInString(vals.Get("title")) > 250 {
		t.Fatalf("pushover: %d", utf8.RuneCountInString(vals.Get("message")))
	}

	r, _ = Render("ntfy", withKind(c, "ntfy"), m)
	if len(r.Body) > 4096 {
		t.Fatalf("ntfy body %d bytes would become an attachment", len(r.Body))
	}
}

func withKind(c Config, kind string) Config {
	base := channelConfig(kind)
	base.Preset, base.Include, base.Format = c.Preset, c.Include, c.Format
	return base
}

func TestSenderResolution(t *testing.T) {
	t.Cleanup(func() { SetSenderResolver(nil) })
	SetSenderResolver(func(id string) Sender {
		switch id {
		case "a1":
			return Sender{Host: "nas", Role: RoleAgent}
		case "":
			return Sender{Host: "hubbox", Role: RoleHub}
		}
		return Sender{}
	})
	if got := (Message{HostID: "a1"}).resolvedSender().Name(); got != "nas • Moorage agent" {
		t.Fatal(got)
	}
	if got := (Message{}).resolvedSender().Name(); got != "hubbox • Moorage hub" {
		t.Fatal(got)
	}
	if got := (Message{HostID: "gone"}).resolvedSender().Name(); got != "hubbox • Moorage hub" {
		t.Fatal(got)
	}
	if got := (Message{Sender: Sender{Host: "pi", Role: RoleWatchdog}}).resolvedSender().Name(); got != "pi • Moorage watchdog" {
		t.Fatal(got)
	}
	SetIconBase("https://img.example.com/logos/")
	if u := (Message{}).iconURL(Sender{Role: RoleWatchdog}); u != "https://img.example.com/logos/watchdog.png" {
		t.Fatal(u)
	}
	if u := (Message{IconBase: "https://w.example"}).iconURL(Sender{Role: "weird"}); u != "https://w.example/hub.png" {
		t.Fatal(u)
	}
	SetIconBase("")
	if IconBase() != DefaultIconBase {
		t.Fatal("reset to default")
	}
}
