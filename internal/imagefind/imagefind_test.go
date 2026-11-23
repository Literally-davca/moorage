package imagefind

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeRegistry serves a token service, an image index with one amd64 manifest and its
// config, plus a GitHub-like API and raw file host for the source repository.
func fakeRegistry(t *testing.T, label string, compose string) (*httptest.Server, *Finder) {
	t.Helper()
	var srv *httptest.Server
	cfg := map[string]any{"config": map[string]any{
		"Labels":       map[string]string{"org.opencontainers.image.source": label},
		"ExposedPorts": map[string]any{"8080/tcp": struct{}{}, "53/udp": struct{}{}},
		"Volumes":      map[string]any{"/data": struct{}{}},
		"Env":          []string{"PATH=/usr/bin", "APP_PORT=8080", "NODE_VERSION=22", "ADMIN_PASSWORD="},
		"User":         "1000",
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("scope") != "repository:owner/app:pull" {
			http.Error(w, "bad scope", 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"token": "t0k"})
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t0k" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="fake"`)
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/v2/owner/app/manifests/1.2":
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Write([]byte(`{"manifests":[{"digest":"sha256:arm","platform":{"os":"linux","architecture":"arm64"}},{"digest":"sha256:amd","platform":{"os":"linux","architecture":"amd64"}}]}`))
		case "/v2/owner/app/manifests/sha256:amd":
			w.Write([]byte(`{"config":{"digest":"sha256:cfg"}}`))
		case "/v2/owner/app/blobs/sha256:cfg":
			json.NewEncoder(w).Encode(cfg)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/api/repos/owner/app", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"default_branch":"main"}`))
	})
	mux.HandleFunc("/raw/owner/app/main/docker-compose.yml", func(w http.ResponseWriter, r *http.Request) {
		if compose == "" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(compose))
	})
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	f := &Finder{
		Client: srv.Client(),
		GitHosts: map[string]GitHost{"github.com": {
			DefaultBranch: func(o, r string) string { return srv.URL + "/api/repos/" + o + "/" + r },
			Raw:           func(o, r, b, file string) string { return srv.URL + "/raw/" + o + "/" + r + "/" + b + "/" + file },
		}},
		Templates: func(_ context.Context, repo string) []TemplateMatch {
			if repo == "owner/app" {
				return []TemplateMatch{{Source: "bundled", ID: "app", Name: "App", SourceLabel: "bundled"}}
			}
			return nil
		},
	}
	return srv, f
}

func TestLookupAllSources(t *testing.T) {
	compose := "services:\n  app:\n    build: .\n    env_file: .env\n    volumes:\n      - ./conf:/conf\n"
	srv, f := fakeRegistry(t, "https://github.com/owner/app", compose)
	image := strings.TrimPrefix(srv.URL, "https://") + "/owner/app:1.2"
	res, err := f.Lookup(context.Background(), image)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Problems) != 0 {
		t.Fatalf("problems: %v", res.Problems)
	}
	if res.Name != "app" {
		t.Fatalf("name %q", res.Name)
	}
	src := map[string]Candidate{}
	for _, c := range res.Candidates {
		src[c.Source] = c
	}
	if src["template"].TemplateID != "app" {
		t.Fatal("template candidate missing")
	}
	repo := src["repo"]
	if !strings.Contains(repo.Compose, "image: "+image) || strings.Contains(repo.Compose, "build:") {
		t.Fatalf("the building service must use the image:\n%s", repo.Compose)
	}
	joined := strings.Join(repo.Warnings, "\n")
	if !strings.Contains(joined, "env_file") || !strings.Contains(joined, "./conf") {
		t.Fatalf("repo warnings: %v", repo.Warnings)
	}
	gen := src["generated"]
	for _, want := range []string{"image: " + image, `"8080:8080"`, `"53:53/udp"`, "app-data:/data", "volumes:\n  app-data:"} {
		if !strings.Contains(gen.Compose, want) {
			t.Errorf("generated compose lacks %q:\n%s", want, gen.Compose)
		}
	}
	if !strings.Contains(gen.Env, "# APP_PORT=8080") || strings.Contains(gen.Env, "PATH=") || strings.Contains(gen.Env, "NODE_VERSION") {
		t.Fatalf("generated .env: %q", gen.Env)
	}
	if !strings.Contains(gen.Detail, "user 1000") {
		t.Fatalf("detail: %q", gen.Detail)
	}
}

// A source label on a host that isn't a known public code host is never contacted.
func TestLookupIgnoresUnknownSourceHost(t *testing.T) {
	srv, f := fakeRegistry(t, "https://169.254.169.254/owner/app", "services: {}")
	res, err := f.Lookup(context.Background(), strings.TrimPrefix(srv.URL, "https://")+"/owner/app:1.2")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Candidates {
		if c.Source == "repo" {
			t.Fatal("an unknown source host must not be fetched")
		}
	}
}

func TestLookupProblemsAndBadName(t *testing.T) {
	srv, f := fakeRegistry(t, "", "")
	res, err := f.Lookup(context.Background(), strings.TrimPrefix(srv.URL, "https://")+"/owner/missing:1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Problems) == 0 || !strings.Contains(res.Problems[0], "Registry") {
		t.Fatalf("a missing image is a problem, not an error: %+v", res)
	}
	if _, err := f.Lookup(context.Background(), "Not An Image!"); err != ErrBadImage {
		t.Fatalf("want ErrBadImage, got %v", err)
	}
}

func TestAdaptRepoComposeSeveralBuilders(t *testing.T) {
	text := "services:\n  a:\n    build: ./a\n  b:\n    build: ./b\n"
	out, notes := adaptRepoCompose(text, "x/y:1")
	if out != text || !strings.Contains(strings.Join(notes, " "), "Several services build") {
		t.Fatalf("%q %v", out, notes)
	}
}

func TestStackName(t *testing.T) {
	for in, want := range map[string]string{
		"vaultwarden/server": "vaultwarden", "library/postgres": "postgres", "owner/My.App": "my-app", "app": "app",
	} {
		if got := stackName(in); got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
}
