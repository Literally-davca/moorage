package templates

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"moorage/internal/engine"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const goodMeta = `id: app
name: App
version: 2
description: An app.
category: Web
website: https://example.com
notes: "Open port ${PORT}, password ${PASS}"
variables:
  - name: PORT
    default: 8080
    required: true
    pattern: "[0-9]{1,5}"
  - name: PASS
    label: Password
    secret: true
    generate: password
    required: true
`

func TestParseMeta(t *testing.T) {
	m, err := ParseMeta([]byte(goodMeta))
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "app" || m.Version != "2" || len(m.Variables) != 2 || m.Variables[0].Default != "8080" || m.Variables[0].Label != "PORT" {
		t.Fatalf("parsed: %+v", m)
	}
	bad := map[string]string{
		"bad pattern":     strings.Replace(goodMeta, `"[0-9]{1,5}"`, `"[0-9"`, 1),
		"unknown kind":    strings.Replace(goodMeta, "generate: password", "generate: base64", 1),
		"missing id":      strings.Replace(goodMeta, "id: app\n", "", 1),
		"bad id":          strings.Replace(goodMeta, "id: app", "id: ../App", 1),
		"missing name":    strings.Replace(goodMeta, "name: App\n", "", 1),
		"unknown key":     strings.Replace(goodMeta, "required: true\n    pattern", "requried: true\n    pattern", 1),
		"default pattern": strings.Replace(goodMeta, "default: 8080", "default: http", 1),
		"duplicate var":   strings.Replace(goodMeta, "name: PASS", "name: PORT", 1),
		"bad var name":    strings.Replace(goodMeta, "name: PASS", "name: pass-word", 1),
		"website scheme":  strings.Replace(goodMeta, "https://example.com", "javascript:alert(1)", 1),
		"empty":           "",
		"not yaml":        "{{{",
	}
	for name, src := range bad {
		if _, err := ParseMeta([]byte(src)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_, err = ParseMeta([]byte(strings.Replace(goodMeta, "generate: password", "generate: base64", 1)))
	if err == nil || !strings.Contains(err.Error(), "base64") || !strings.Contains(err.Error(), "PASS") {
		t.Errorf("unknown kind message: %v", err)
	}
}

func mustTemplate(t *testing.T, meta, compose, env string) Template {
	t.Helper()
	m, err := ParseMeta([]byte(meta))
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := NewTemplate(m, compose, env)
	if err != nil {
		t.Fatal(err)
	}
	return tpl
}

func TestSubstitute(t *testing.T) {
	vals := map[string]string{"PORT": "80", "PASS": "a$b"}
	in := "p=${PORT} q=${PASS} keep=$$PORT esc=$${PORT} other=${FOO} bare=$PORT def=${PORT:-1} open=${PORT"
	got := Substitute(in, vals, true)
	want := "p=80 q=a$$b keep=$$PORT esc=$${PORT} other=${FOO} bare=$PORT def=${PORT:-1} open=${PORT"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if got := Substitute("${PASS}", vals, false); got != "a$b" {
		t.Fatalf("unescaped: %q", got)
	}
}

func TestRender(t *testing.T) {
	tpl := mustTemplate(t, goodMeta, "services:\n  app:\n    image: nginx:1.27-alpine\n    ports:\n      - \"${PORT}:80\"\n    environment:\n      PASS: ${APP_PASS}\n", "APP_PASS=${PASS}\nOTHER=${UNDECLARED}\n")
	r1, err := Render(tpl, map[string]string{"PORT": "9000", "EXTRA": "ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r1.Compose, `"9000:80"`) || !strings.Contains(r1.Compose, "${APP_PASS}") {
		t.Fatalf("compose: %s", r1.Compose)
	}
	if !strings.Contains(r1.Env, "OTHER=${UNDECLARED}") || !strings.HasPrefix(r1.Env, "APP_PASS="+r1.Values["PASS"]+"\n") {
		t.Fatalf("env: %s", r1.Env)
	}
	if _, ok := r1.Values["EXTRA"]; ok {
		t.Fatal("undeclared value kept")
	}
	if len(r1.Secrets) != 1 || r1.Secrets[0] != "PASS" {
		t.Fatalf("secrets: %v", r1.Secrets)
	}
	if strings.Contains(r1.Notes, r1.Values["PASS"]) || !strings.Contains(r1.Notes, "port 9000") || !strings.Contains(r1.Notes, "${PASS}") {
		t.Fatalf("notes must not show secrets: %q", r1.Notes)
	}
	r2, err := Render(tpl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Values["PASS"] == r2.Values["PASS"] {
		t.Fatal("generated secrets must differ per render")
	}
	if r2.Values["PORT"] != "8080" {
		t.Fatalf("default not used: %v", r2.Values)
	}
	// The admin's own value wins over a generator.
	r3, err := Render(tpl, map[string]string{"PASS": "mine"})
	if err != nil || r3.Values["PASS"] != "mine" {
		t.Fatalf("own value: %v %v", r3.Values, err)
	}

	for name, vals := range map[string]map[string]string{
		"pattern":  {"PORT": "80x"},
		"newline":  {"PASS": "a\nprivileged: true"},
		"too long": {"PASS": strings.Repeat("a", 1025)},
	} {
		_, err := Render(tpl, vals)
		var fe *FieldError
		if !errors.As(err, &fe) {
			t.Errorf("%s: want FieldError, got %v", name, err)
		}
	}
	req := mustTemplate(t, "id: x\nname: X\nvariables:\n  - name: DOMAIN\n    required: true\n", "services: {}\n", "")
	if _, err := Render(req, map[string]string{"DOMAIN": "  "}); err == nil {
		t.Fatal("missing required value accepted")
	}
}

func TestGenerate(t *testing.T) {
	cases := map[string]*regexp.Regexp{
		GenPassword: regexp.MustCompile(`^[A-Za-z0-9]{32}$`),
		GenHex32:    regexp.MustCompile(`^[0-9a-f]{32}$`),
		GenUUID:     regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`),
	}
	for kind, re := range cases {
		seen := map[string]bool{}
		for range 200 {
			v, err := Generate(kind)
			if err != nil || !re.MatchString(v) {
				t.Fatalf("%s: %q %v", kind, v, err)
			}
			if seen[v] {
				t.Fatalf("%s repeated %q", kind, v)
			}
			seen[v] = true
		}
	}
	// Every character class shows up in passwords over many draws (the alphabet is used).
	all := ""
	for range 100 {
		v, _ := Generate(GenPassword)
		all += v
	}
	if !regexp.MustCompile(`[A-Z]`).MatchString(all) || !regexp.MustCompile(`[a-z]`).MatchString(all) || !regexp.MustCompile(`[0-9]`).MatchString(all) {
		t.Fatal("password alphabet not used")
	}
	if _, err := Generate("base64"); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestWarnings(t *testing.T) {
	compose := `services:
  a:
    image: x
    privileged: true
    network_mode: host
  b:
    image: x
    pid: host
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
  c:
    image: x
    volumes:
      - type: bind
        source: /run/docker.sock
        target: /sock
  d:
    image: x
    privileged: false
    volumes:
      - data:/data
`
	w := Warnings(compose)
	joined := strings.Join(w, "\n")
	for _, want := range []string{"a runs privileged", "a uses the host network", "b shares the host's process", "b mounts the Docker socket", "c mounts the Docker socket"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing warning %q in %v", want, w)
		}
	}
	if strings.Contains(joined, "Service d") || len(w) != 5 {
		t.Errorf("unexpected warnings: %v", w)
	}
	if len(Warnings("not: [valid")) != 0 {
		t.Error("broken yaml must give no warnings, not a panic")
	}
}

// A broken local template is listed with its error and doesn't hide the others.
func TestLocalFolder(t *testing.T) {
	dir := t.TempDir()
	write := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("good/template.yaml", "id: good\nname: Good\ncategory: Web\n")
	write("good/compose.yaml", "services:\n  web:\n    image: nginx:1.27-alpine\n")
	write("broken/template.yaml", "id: broken\nname: Broken\nvariables:\n  - name: X\n    generate: magic\n")
	write("broken/compose.yaml", "services: {}\n")
	write("nocompose/template.yaml", "id: nocompose\nname: No compose\n")
	write("wrongid/template.yaml", "id: other\nname: Wrong id\n")
	write("wrongid/compose.yaml", "services: {}\n")
	write("README.md", "not a template")
	write(".git/config", "x")

	s := New(nil, dir, quietLog())
	c, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	local := map[string]Entry{}
	for _, e := range c.Templates {
		if e.Source == SourceLocal {
			local[e.ID] = e
		}
	}
	if len(local) != 4 {
		t.Fatalf("want 4 local templates (hidden folders and files skipped), got %+v", local)
	}
	if local["good"].Error != "" || local["good"].Name != "Good" || local["good"].SourceLabel != "Local folder" {
		t.Errorf("good: %+v", local["good"])
	}
	for _, id := range []string{"broken", "nocompose", "wrongid"} {
		if local[id].Error == "" {
			t.Errorf("%s: error missing", id)
		}
	}
	if _, _, err := s.Get(context.Background(), SourceLocal, "good"); err != nil {
		t.Fatalf("get good: %v", err)
	}
	var be BrokenError
	if _, _, err := s.Get(context.Background(), SourceLocal, "broken"); !errors.As(err, &be) || !strings.Contains(be.Msg, "magic") {
		t.Fatalf("get broken: %v", err)
	}
	if _, _, err := s.Get(context.Background(), SourceLocal, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}

	// A missing folder is an empty source with a hint, not a failure.
	s = New(nil, filepath.Join(dir, "nope"), quietLog())
	c, err = s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range c.Sources {
		if src.Key == SourceLocal && (src.Count != 0 || src.Error == "") {
			t.Fatalf("missing folder: %+v", src)
		}
	}
}

func TestLoadFoldersFS(t *testing.T) {
	fsys := fstest.MapFS{
		"big/template.yaml": {Data: []byte("id: big\nname: Big\n")},
		"big/compose.yaml":  {Data: make([]byte, MaxFileBytes+1)},
	}
	for _, full := range []bool{false, true} {
		items, err := loadFolders(fsys, full)
		if err != nil || len(items) != 1 || !strings.Contains(items[0].err, "1 MiB") {
			t.Fatalf("full=%v: %+v %v", full, items, err)
		}
	}
}

// Every bundled template renders with its defaults and passes the same compose
// validation as the editor and the create path.
func TestBundledTemplates(t *testing.T) {
	s := New(nil, "", quietLog())
	if len(s.bundled) < 6 || len(s.bundled) > 8 {
		t.Fatalf("want 6–8 bundled templates, got %d", len(s.bundled))
	}
	var e engine.Engine
	for _, it := range s.bundled {
		if it.err != "" {
			t.Errorf("%s: %s", it.id, it.err)
			continue
		}
		r, err := Render(it.t, nil)
		if err != nil {
			t.Errorf("%s: render: %v", it.id, err)
			continue
		}
		if err := e.ValidateCompose(context.Background(), it.id, r.Compose, r.Env); err != nil {
			t.Errorf("%s: compose invalid: %v\n%s", it.id, err, r.Compose)
		}
		if len(r.Warnings) != 0 {
			t.Errorf("%s: bundled templates must not need warnings: %v", it.id, r.Warnings)
		}
		if strings.Contains(r.Compose, ":latest") {
			t.Errorf("%s: pin a version, not latest", it.id)
		}
		for _, v := range it.t.Variables {
			if v.Secret && strings.Contains(r.Compose, r.Values[v.Name]) {
				t.Errorf("%s: secret %s should live in .env, not compose.yaml", it.id, v.Name)
			}
		}
		_, entry, err := s.Get(context.Background(), SourceBundled, it.id)
		if err != nil || entry.Name == "" || entry.Category == "" {
			t.Errorf("%s: get: %+v %v", it.id, entry, err)
		}
	}
}

func TestNote(t *testing.T) {
	cases := map[string]Entry{
		"Created from template postgres (bundled, version 1)":     {Source: SourceBundled, SourceLabel: "Bundled", ID: "postgres", Version: "1"},
		"Created from template app (local folder)":                {Source: SourceLocal, SourceLabel: "Local folder", ID: "app"},
		"Created from template app (from example.com, version 2)": {Source: "remote-1", SourceLabel: "from example.com", ID: "app", Version: "2"},
	}
	for want, e := range cases {
		if got := Note(e); got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
}
