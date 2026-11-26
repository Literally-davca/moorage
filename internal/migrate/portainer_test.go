package migrate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "ptr_TESTtoken123"

func fakePortainer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-API-Key") != testToken {
				http.Error(w, `{"message":"Unauthorized"}`, http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /api/stacks", auth(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[
		 {"Id": 3, "Name": "Swarmy", "Type": 1, "EndpointId": 1, "Status": 1},
		 {"Id": 1, "Name": "nextcloud", "Type": 2, "EndpointId": 1, "Status": 1,
		  "Env": [{"name": "DB_PASS", "value": "s3cr$t pass"}, {"name": "bad name", "value": "x"}]},
		 {"Id": 2, "Name": "Git App", "Type": 2, "EndpointId": 2, "Status": 2,
		  "GitConfig": {"URL": "https://git.example.com/app.git"}},
		 {"Id": 4, "Name": "gitonly", "Type": 2, "EndpointId": 1, "Status": 1, "GitConfig": {"URL": "https://git.example.com/x.git"}}
		]`)
	}))
	mux.HandleFunc("GET /api/endpoints", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("excludeSnapshots") != "true" {
			t.Errorf("endpoints without excludeSnapshots")
		}
		fmt.Fprint(w, `[{"Id": 1, "Name": "local"}, {"Id": 2, "Name": "pi"}]`)
	}))
	mux.HandleFunc("GET /api/stacks/{id}/file", auth(func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("id") {
		case "1":
			fmt.Fprint(w, `{"StackFileContent": "services:\n  app:\n    image: nextcloud\n    env_file: stack.env\n    volumes:\n      - ./data:/var/www/html\n"}`)
		case "2":
			fmt.Fprint(w, `{"StackFileContent": "services:\n  web:\n    build: .\n"}`)
		case "4":
			fmt.Fprint(w, `{"StackFileContent": ""}`)
		default:
			http.NotFound(w, r)
		}
	}))
	mux.HandleFunc("GET /redirect/api/stacks", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/stacks", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestPortainerClient(t *testing.T) {
	srv := fakePortainer(t)
	ctx := context.Background()

	pc, err := NewPortainer(srv.URL+"/", testToken, false)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	stacks, err := pc.Stacks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stacks) != 4 || stacks[0].ID != 1 || stacks[0].Env[0].Value != "s3cr$t pass" {
		t.Fatalf("stacks: %+v", stacks)
	}
	eps := pc.Endpoints(ctx)
	if eps[2] != "pi" {
		t.Errorf("endpoints: %v", eps)
	}
	file, err := pc.StackFile(ctx, 1)
	if err != nil || !strings.Contains(file, "image: nextcloud") {
		t.Fatalf("file: %q %v", file, err)
	}
	if _, err := pc.StackFile(ctx, 99); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing file: %v", err)
	}

	// A wrong token is refused with a clear message that never repeats the token.
	bad, _ := NewPortainer(srv.URL, "wrong-token-value", false)
	_, err = bad.Stacks(ctx)
	var pe *PortainerError
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "wrong-token-value") {
		t.Errorf("wrong token: %v", err)
	}

	// Redirects are not followed (the token would travel with them).
	red, _ := NewPortainer(srv.URL+"/redirect", testToken, false)
	if _, err := red.Stacks(ctx); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("redirect: %v", err)
	}

	// Unreachable: the error names neither the token nor needs a URL.
	if closed, _ := NewPortainer("http://127.0.0.1:1", testToken, false); closed != nil {
		if _, err := closed.Stacks(ctx); err == nil || strings.Contains(err.Error(), testToken) {
			t.Errorf("unreachable: %v", err)
		}
	}
}

func TestPortainerInputValidation(t *testing.T) {
	for _, u := range []string{"", "portainer.lan", "ftp://x", "https://user:pw@p.lan", "https://p.lan/?a=1"} {
		if _, err := NewPortainer(u, testToken, false); err == nil {
			t.Errorf("url %q accepted", u)
		}
	}
	for _, tok := range []string{"", "has space", "line\nbreak", strings.Repeat("a", 513)} {
		if _, err := NewPortainer("https://p.lan", tok, false); err == nil {
			t.Errorf("token %q accepted", tok)
		}
	}
	p, err := NewPortainer("https://p.lan:9443/portainer/api/", testToken, true)
	if err != nil || p.base != "https://p.lan:9443/portainer" {
		t.Errorf("base: %v %v", p, err)
	}
	// Metadata / link-local addresses are refused by netguard.
	md, _ := NewPortainer("http://169.254.169.254", testToken, false)
	if _, err := md.Stacks(context.Background()); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Errorf("link-local: %v", err)
	}
}

func TestPreviewStack(t *testing.T) {
	srv := fakePortainer(t)
	ctx := context.Background()
	pc, _ := NewPortainer(srv.URL, testToken, false)
	stacks, _ := pc.Stacks(ctx)
	managed := map[string]bool{"git-app": true}
	running := map[string]bool{"nextcloud": true}
	got := map[int]StackPreview{}
	for _, s := range stacks {
		file, ferr := "", error(nil)
		if s.Unsupported() == "" {
			file, ferr = pc.StackFile(ctx, s.ID)
		}
		got[s.ID] = PreviewStack(s, "local", file, ferr, managed, running)
	}
	nc := got[1]
	if nc.Status != StatusReady || nc.Target != "nextcloud" || len(nc.EnvKeys) != 2 {
		t.Errorf("nextcloud: %+v", nc)
	}
	joined := strings.Join(nc.Warnings, "\n")
	for _, w := range []string{"stack.env", "Relative bind mounts", "invalid name", "Stop the stack in Portainer"} {
		if !strings.Contains(joined, w) {
			t.Errorf("nextcloud warnings miss %q: %v", w, nc.Warnings)
		}
	}
	if g := got[2]; g.Status != StatusExists || g.Target != "git-app" || !g.Git || !strings.Contains(strings.Join(g.Warnings, " "), "build") {
		t.Errorf("git app: %+v", g)
	}
	if g := got[3]; g.Status != StatusUnsupported || !strings.Contains(g.Reason, "Swarm") {
		t.Errorf("swarm: %+v", g)
	}
	if g := got[4]; g.Status != StatusUnsupported || !strings.Contains(g.Reason, "Git") {
		t.Errorf("git without file: %+v", g)
	}

	env, warn := EnvFile(stacks[0])
	if env != "DB_PASS='s3cr$t pass'\n" || len(warn) != 1 {
		t.Errorf("env: %q %v", env, warn)
	}
}

func TestStackNameAndEnvFormat(t *testing.T) {
	for in, want := range map[string]string{
		"nextcloud": "nextcloud", "My App": "my-app", "__x__": "x", "Ünïcode": "n-code", "---": "",
		strings.Repeat("a", 70): strings.Repeat("a", 63),
	} {
		if got := StackName(in); got != want {
			t.Errorf("StackName(%q) = %q want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"plain":     "plain",
		"a b":       "'a b'",
		"it's $x":   `"it's \$x"`,
		"two\nline": `"two\nline"`,
	} {
		if got := formatEnvValue(in); got != want {
			t.Errorf("formatEnvValue(%q) = %s want %s", in, got, want)
		}
	}
}
