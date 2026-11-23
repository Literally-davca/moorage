package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"moorage/internal/config"
)

type fakeImageBackend struct {
	mu          sync.Mutex
	local       map[string]fakeLocalImage
	remote      map[string]fakeRemoteImage
	containers  map[string][]Container
	remoteCalls map[string]int
	pulled      []string
	tagged      [][2]string
	pullErr     error
	tagErr      error
}

type fakeLocalImage struct {
	id          string
	repoDigests []string
	found       bool
	err         error
}

type fakeRemoteImage struct {
	digest     string
	err        error
	newerTag   string
	newerMajor string
}

func newFakeImageBackend() *fakeImageBackend {
	return &fakeImageBackend{
		local:       map[string]fakeLocalImage{},
		remote:      map[string]fakeRemoteImage{},
		containers:  map[string][]Container{},
		remoteCalls: map[string]int{},
	}
}

func (f *fakeImageBackend) InspectLocal(_ context.Context, ref string) (string, []string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if li, ok := f.local[ref]; ok {
		return li.id, li.repoDigests, li.found, li.err
	}
	return "", nil, false, nil
}

func (f *fakeImageBackend) InspectRemote(_ context.Context, ref string, _ RegistryCreds) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remoteCalls[ref]++
	if ri, ok := f.remote[ref]; ok {
		return ri.digest, ri.err
	}
	return "", errors.New("404 not found")
}

func (f *fakeImageBackend) SelectRemoteTags(_ context.Context, ref, _ string, _ RegistryCreds) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ri, ok := f.remote[ref]; ok {
		return ri.newerTag, ri.newerMajor, nil
	}
	return "", "", nil
}

func (f *fakeImageBackend) PullImage(_ context.Context, ref string, _ RegistryCreds) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulled = append(f.pulled, ref)
	return f.pullErr
}

func (f *fakeImageBackend) TagImage(_ context.Context, sourceID, targetRef string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tagged = append(f.tagged, [2]string{sourceID, targetRef})
	return f.tagErr
}

func (f *fakeImageBackend) RunningContainers(_ context.Context, stack string) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.containers[stack], nil
}

func newTestImageEngine(t *testing.T, fb *fakeImageBackend) *Engine {
	t.Helper()
	dir := t.TempDir()
	stacksDir := filepath.Join(dir, "stacks")
	if err := os.MkdirAll(stacksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hist, err := openHistory(filepath.Join(dir, "history.git"))
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{
		cfg:          config.Config{DataDir: dir, StacksDir: stacksDir, PUID: -1, PGID: -1},
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		busy:         map[string]bool{},
		ops:          newOpRegistry(),
		hist:         hist,
		ImageBackend: fb,
	}
}

func writeTestStack(t *testing.T, e *Engine, stack, compose string) {
	t.Helper()
	dir := filepath.Join(e.cfg.StacksDir, stack)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Part A #1: Digest comparison & statuses + deduplication across stacks.
func TestCheckImagesStatusesAndDeduplication(t *testing.T) {
	fb := newFakeImageBackend()
	e := newTestImageEngine(t, fb)

	compose := `services:
  up:
    image: nginx:1.27
  upd:
    image: redis:7.4
  pulled:
    image: traefik:v3.1
  pinned:
    image: alpine:3.20@sha256:1111111111111111111111111111111111111111111111111111111111111111
  built:
    build: .
    image: myapp:local
  localonly:
    image: custom/box:1.0
  unreach:
    image: ghcr.io/private/app:1.0
`
	writeTestStack(t, e, "demo", compose)

	fb.local["nginx:1.27"] = fakeLocalImage{
		id:          "sha256:aaa111",
		repoDigests: []string{"nginx@sha256:d111"},
		found:       true,
	}
	fb.remote["nginx:1.27"] = fakeRemoteImage{digest: "sha256:d111"}

	fb.local["redis:7.4"] = fakeLocalImage{
		id:          "sha256:bbb111",
		repoDigests: []string{"redis@sha256:d222"},
		found:       true,
	}
	fb.remote["redis:7.4"] = fakeRemoteImage{digest: "sha256:d333"}

	fb.local["traefik:v3.1"] = fakeLocalImage{
		id:          "sha256:ccc222",
		repoDigests: []string{"traefik@sha256:d444"},
		found:       true,
	}
	fb.remote["traefik:v3.1"] = fakeRemoteImage{digest: "sha256:d444"}

	fb.local["custom/box:1.0"] = fakeLocalImage{
		id:          "sha256:loc111",
		repoDigests: nil, // local-only
		found:       true,
	}

	fb.local["ghcr.io/private/app:1.0"] = fakeLocalImage{
		id:          "sha256:priv1",
		repoDigests: []string{"ghcr.io/private/app@sha256:d555"},
		found:       true,
	}
	fb.remote["ghcr.io/private/app:1.0"] = fakeRemoteImage{err: errors.New("401 unauthorized")}

	fb.containers["demo"] = []Container{
		{Service: "up", Image: "nginx:1.27", ImageID: "sha256:aaa111", State: "running"},
		{Service: "upd", Image: "redis:7.4", ImageID: "sha256:bbb111", State: "running"},
		{Service: "pulled", Image: "traefik:v3.1", ImageID: "sha256:ccc111", State: "running"}, // running ID != local ID
	}

	res, err := e.CheckImages(context.Background(), "demo")
	if err != nil {
		t.Fatalf("CheckImages: %v", err)
	}
	byService := map[string]ServiceImage{}
	for _, r := range res {
		byService[r.Service] = r
	}

	wantStatus := map[string]string{
		"up":        ImageUpToDate,
		"upd":       ImageUpdateAvailable,
		"pulled":    ImagePulledNotApplied,
		"pinned":    ImagePinned,
		"built":     ImageBuilt,
		"localonly": ImageLocalOnly,
		"unreach":   ImageUnreachable,
	}
	for svc, want := range wantStatus {
		got := byService[svc].Status
		if got != want {
			t.Errorf("service %s: status = %q, want %q (err=%q)", svc, got, want, byService[svc].Error)
		}
	}
	if byService["unreach"].Error != "unauthorized" {
		t.Errorf("unreach error = %q, want unauthorized", byService["unreach"].Error)
	}
	if fb.remoteCalls["alpine:3.20@sha256:1111111111111111111111111111111111111111111111111111111111111111"] != 0 {
		t.Errorf("pinned image must not make remote calls")
	}
	if fb.remoteCalls["myapp:local"] != 0 {
		t.Errorf("built image must not make remote calls")
	}
	if fb.remoteCalls["custom/box:1.0"] != 0 {
		t.Errorf("local-only image must not make remote calls")
	}

	// Deduplication: 3 stacks sharing postgres:16 in one run must result in 1 remote lookup.
	fb.local["postgres:16"] = fakeLocalImage{id: "sha256:pg16", repoDigests: []string{"postgres@sha256:pgd1"}, found: true}
	fb.remote["postgres:16"] = fakeRemoteImage{digest: "sha256:pgd1"}
	for _, s := range []string{"s1", "s2", "s3"} {
		writeTestStack(t, e, s, "services:\n  db:\n    image: postgres:16\n")
		if _, err := e.CheckImages(context.Background(), s); err != nil {
			t.Fatalf("CheckImages(%s): %v", s, err)
		}
	}
	if got := fb.remoteCalls["postgres:16"]; got != 1 {
		t.Errorf("remoteCalls[postgres:16] = %d, want 1 (deduplicated across 3 stacks)", got)
	}
	// Services are checked in parallel: two services of one stack with the same reference
	// still make one lookup.
	e.ResetImageCache()
	writeTestStack(t, e, "pair", "services:\n  a:\n    image: postgres:16\n  b:\n    image: postgres:16\n")
	if _, err := e.CheckImages(context.Background(), "pair"); err != nil {
		t.Fatalf("CheckImages(pair): %v", err)
	}
	if got := fb.remoteCalls["postgres:16"]; got != 2 {
		t.Errorf("remoteCalls[postgres:16] = %d, want 2 (one more for the pair)", got)
	}
}

// Part A #6: Credentials from auths vs credHelpers/credsStore.
func TestDockerConfigCredentials(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	rawAuth := base64.StdEncoding.EncodeToString([]byte("alice:secret123"))
	cfg := map[string]any{
		"auths": map[string]any{
			"https://index.docker.io/v1/": map[string]any{"auth": rawAuth},
			"ghcr.io":                     map[string]any{"username": "bob", "password": "ghp_tok"},
			"quay.io":                     map[string]any{},
		},
		"credHelpers": map[string]string{
			"123456789.dkr.ecr.eu-central-1.amazonaws.com": "ecr-login",
		},
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Docker Hub matches index.docker.io/v1/
	creds, err := readRegistryAuthFromFile(cfgPath, "nginx:1.27")
	if err != nil {
		t.Fatalf("docker hub auth: %v", err)
	}
	if creds.Username != "alice" || creds.Password != "secret123" || creds.EncodedAuth == "" {
		t.Fatalf("unexpected docker hub creds: %+v", creds)
	}

	// 2. ghcr.io direct username/password
	creds, err = readRegistryAuthFromFile(cfgPath, "ghcr.io/org/app:1.0")
	if err != nil || creds.Username != "bob" || creds.Password != "ghp_tok" {
		t.Fatalf("ghcr.io creds: %+v, err=%v", creds, err)
	}

	// 3. credHelpers entry returns clear "credential helper not supported" without executing any binary
	_, err = readRegistryAuthFromFile(cfgPath, "123456789.dkr.ecr.eu-central-1.amazonaws.com/app:1.0")
	if !errors.Is(err, errCredHelper) || classifyRegistryError(err) != "credential helper not supported" {
		t.Fatalf("expected credential helper not supported, got %v", err)
	}

	// 4. Global credsStore with empty auth entry returns "credential helper not supported"
	cfg["credsStore"] = "desktop"
	b, _ = json.Marshal(cfg)
	_ = os.WriteFile(cfgPath, b, 0o600)
	_, err = readRegistryAuthFromFile(cfgPath, "quay.io/org/app:1.0")
	if !errors.Is(err, errCredHelper) {
		t.Fatalf("expected credential helper not supported for global credsStore, got %v", err)
	}

	// 5. Global credsStore but no entry for this registry: anonymous, like docker pull.
	// (Docker Desktop always sets credsStore; public images elsewhere must still be checked.)
	creds, err = readRegistryAuthFromFile(cfgPath, "registry.gitlab.com/org/app:1.0")
	if err != nil || creds.EncodedAuth != "" {
		t.Fatalf("public registry with global credsStore: creds=%+v err=%v", creds, err)
	}
}

func TestRepoDigestForPicksOwnRepository(t *testing.T) {
	digests := []string{"ghcr.io/other/app@sha256:aaa", "nginx@sha256:bbb"}
	if got := repoDigestFor("nginx:1.27", digests); got != "sha256:bbb" {
		t.Fatalf("docker hub short name: got %q", got)
	}
	if got := repoDigestFor("docker.io/library/nginx:1.27", digests); got != "sha256:bbb" {
		t.Fatalf("docker hub long name: got %q", got)
	}
	if got := repoDigestFor("unknown/x:1", digests); got != "sha256:aaa" {
		t.Fatalf("fallback to the first entry: got %q", got)
	}
}

// Part A #4: Rollback re-tags local image ID or pulls by old digest if pruned.
func TestEngineRollbackImages(t *testing.T) {
	fb := newFakeImageBackend()
	e := newTestImageEngine(t, fb)
	writeTestStack(t, e, "app", "services:\n  web:\n    image: nginx:1.27\n  worker:\n    image: worker:1.0\n")

	// web's old image ID is still local; worker's old image ID was pruned so it must be pulled by digest.
	fb.local["sha256:oldweb"] = fakeLocalImage{id: "sha256:oldweb", found: true}
	fb.local["sha256:oldworker"] = fakeLocalImage{found: false}
	fb.local["worker@sha256:deadbeef"] = fakeLocalImage{id: "sha256:pulledworker", found: true}

	opInfo, err := e.RollbackImages(context.Background(), "app", map[string]RollbackTarget{
		"web":    {Ref: "nginx:1.27", OldID: "sha256:oldweb", OldDigest: "sha256:webdig"},
		"worker": {Ref: "worker:1.0", OldID: "sha256:oldworker", OldDigest: "sha256:deadbeef"},
	}, "admin")
	if err != nil {
		t.Fatalf("RollbackImages: %v", err)
	}
	op, ok := e.Op(opInfo.ID)
	if !ok {
		t.Fatalf("op not found")
	}
	ch, unsub := op.Subscribe()
	defer unsub()
	for {
		_, done, errMsg := op.Snapshot(0)
		if done {
			if errMsg != "" {
				t.Fatalf("rollback op failed: %s", errMsg)
			}
			break
		}
		<-ch
	}

	if len(fb.pulled) != 1 || fb.pulled[0] != "worker@sha256:deadbeef" {
		t.Fatalf("expected pull of worker@sha256:deadbeef only, got %v", fb.pulled)
	}
	if len(fb.tagged) != 2 {
		t.Fatalf("expected 2 image tags, got %v", fb.tagged)
	}
}

// Part B #1: Version parser and candidate selection.
func TestParseVersionTagAndCandidates(t *testing.T) {
	// 1. 1.27 -> 1.28 (same major), 2.1 (newer major). 1.28.0 (3 parts), 1.28-alpine (suffix), v1.28 (prefix), 1.28-rc1 (pre-release) rejected.
	nginxTags := []string{
		"latest", "stable", "edge", "main", "nightly", "bookworm", "alpine",
		"20260419", "20260420",
		"1.26", "1.27", "1.28", "1.28.0", "1.28-alpine", "1.28-rc1", "1.29-beta.1", "v1.28",
		"2.0", "2.1", "2.1.0", "2.2-rc1",
	}
	same, major := SelectNewerTags("1.27", nginxTags)
	if same != "1.28" || major != "2.1" {
		t.Errorf("for 1.27: same=%q (want 1.28), major=%q (want 2.1)", same, major)
	}

	// 2. v1.10 -> v1.11 (same major), v2.0 (newer major).
	whoamiTags := []string{"latest", "1.11", "v1.9", "v1.10", "v1.11", "v1.11.0", "v1.12-rc1", "v2.0"}
	same, major = SelectNewerTags("v1.10", whoamiTags)
	if same != "v1.11" || major != "v2.0" {
		t.Errorf("for v1.10: same=%q (want v1.11), major=%q (want v2.0)", same, major)
	}

	// 3. 16.4-alpine -> 16.5-alpine (same major), 17.1-alpine (newer major).
	pgTags := []string{"16.4-alpine", "16.5-alpine", "16.5", "16.5-bookworm", "16.6-alpine-rc1", "17.0-alpine", "17.1-alpine", "17.2"}
	same, major = SelectNewerTags("16.4-alpine", pgTags)
	if same != "16.5-alpine" || major != "17.1-alpine" {
		t.Errorf("for 16.4-alpine: same=%q (want 16.5-alpine), major=%q (want 17.1-alpine)", same, major)
	}

	// 4. Rolling/word/date tags are rejected by ParseVersionTag.
	for _, nonVer := range []string{"latest", "stable", "edge", "bookworm", "20260419", "sha-abcdef", "01.2"} {
		if _, ok := ParseVersionTag(nonVer); ok {
			t.Errorf("ParseVersionTag(%q) should be false", nonVer)
		}
		s, m := SelectNewerTags(nonVer, nginxTags)
		if s != "" || m != "" {
			t.Errorf("SelectNewerTags(%q) = (%q, %q), want empty", nonVer, s, m)
		}
	}
}

// Part B #2: Registry v2 tag listing (Bearer auth, Link pagination, 10,000 cap, netguard SSRF block).
func TestSelectNewerTagsFromRegistry(t *testing.T) {
	var srv *httptest.Server
	page2Called := false
	tokenIssued := false

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			user, pass, ok := r.BasicAuth()
			if !ok || user != "testuser" || pass != "testpass" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.URL.Query().Get("service") != "test-reg" || r.URL.Query().Get("scope") != "repository:org/app:pull" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			tokenIssued = true
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "secret-jwt"})

		case "/v2/org/app/tags/list":
			if r.Header.Get("Authorization") != "Bearer secret-jwt" {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="test-reg",scope="repository:org/app:pull"`, srv.URL))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.URL.Query().Get("last") == "page1" {
				page2Called = true
				_ = json.NewEncoder(w).Encode(map[string]any{
					"name": "org/app",
					"tags": []string{"v1.11", "v2.0", "v1.12-rc1"},
				})
				return
			}
			w.Header().Set("Link", `</v2/org/app/tags/list?n=1000&last=page1>; rel="next"`)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "org/app",
				"tags": []string{"v1.9", "v1.10", "latest"},
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	regAddr := strings.TrimPrefix(srv.URL, "http://")
	creds := RegistryCreds{Username: "testuser", Password: "testpass"}
	same, major, err := SelectNewerTagsFromRegistry(context.Background(), regAddr+"/org/app:v1.10", "v1.10", creds, srv.Client())
	if err != nil {
		t.Fatalf("SelectNewerTagsFromRegistry: %v", err)
	}
	if !tokenIssued || !page2Called {
		t.Fatalf("tokenIssued=%v page2Called=%v", tokenIssued, page2Called)
	}
	if same != "v1.11" || major != "v2.0" {
		t.Fatalf("got same=%q major=%q, want v1.11 / v2.0", same, major)
	}

	// 10,000 tag cap across paginated responses.
	pagesFetched := 0
	bigSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pagesFetched++
		tags := make([]string, 3000)
		for i := range tags {
			tags[i] = fmt.Sprintf("1.%d", pagesFetched*3000+i)
		}
		w.Header().Set("Link", fmt.Sprintf(`</v2/lib/big/tags/list?page=%d>; rel="next"`, pagesFetched+1))
		_ = json.NewEncoder(w).Encode(map[string]any{"tags": tags})
	}))
	defer bigSrv.Close()
	bigAddr := strings.TrimPrefix(bigSrv.URL, "http://")
	same, _, err = SelectNewerTagsFromRegistry(context.Background(), bigAddr+"/lib/big:1.1", "1.1", RegistryCreds{}, bigSrv.Client())
	if err != nil {
		t.Fatalf("big registry: %v", err)
	}
	if pagesFetched != 4 { // 4 pages * 3000 = 12,000 streamed, capped at 10,000
		t.Fatalf("pagesFetched = %d, want 4 (stopped at 10,000 tag cap)", pagesFetched)
	}
	if same == "" {
		t.Fatalf("expected non-empty candidate from big tag list")
	}

	// SSRF protection: link-local metadata IP 169.254.169.254 must be blocked by netguard.
	_, _, err = SelectNewerTagsFromRegistry(context.Background(), "169.254.169.254/x:1.0", "1.0", RegistryCreds{}, nil)
	if err == nil || classifyRegistryError(err) != "blocked address" {
		t.Fatalf("expected netguard blocked address error for 169.254.169.254, got %v", err)
	}
}

// Part B #3: Compose rewrite and git version commit.
func TestRewriteComposeImageAndCommit(t *testing.T) {
	src := "services:\n" +
		"  # comment mentioning image: traefik/whoami:v1.10\n" +
		"  other:\n" +
		"    image: postgres:16.4-alpine\n" +
		"  web:\n" +
		"    image: \"traefik/whoami:v1.10\" # inline comment\n" +
		"    restart: unless-stopped\n"

	out, err := RewriteComposeImage(src, "web", "traefik/whoami:v1.10", "traefik/whoami:v1.11")
	if err != nil {
		t.Fatalf("RewriteComposeImage: %v", err)
	}
	if !strings.Contains(out, `image: "traefik/whoami:v1.11" # inline comment`) {
		t.Fatalf("expected quotes and inline comment preserved, got:\n%s", out)
	}
	if !strings.Contains(out, `image: postgres:16.4-alpine`) {
		t.Fatalf("other service was modified:\n%s", out)
	}

	// CRLF preservation.
	crlfSrc := strings.ReplaceAll(src, "\n", "\r\n")
	crlfOut, err := RewriteComposeImage(crlfSrc, "web", "traefik/whoami:v1.10", "traefik/whoami:v1.11")
	if err != nil {
		t.Fatalf("RewriteComposeImage CRLF: %v", err)
	}
	if !strings.Contains(crlfOut, "\r\n") || strings.Contains(strings.ReplaceAll(crlfOut, "\r\n", ""), "\n") {
		t.Fatalf("CRLF line endings were not preserved")
	}

	// Variable ${TAG} must be refused with ErrManualEdit ("Edit compose.yaml manually").
	varSrc := "services:\n  web:\n    image: traefik/whoami:${TAG}\n"
	if _, err := RewriteComposeImage(varSrc, "web", "traefik/whoami:v1.10", "traefik/whoami:v1.11"); !errors.Is(err, ErrManualEdit) {
		t.Fatalf("expected ErrManualEdit for ${TAG}, got %v", err)
	}

	// Duplicate image values across services must be refused.
	dupSrc := "services:\n  web:\n    image: traefik/whoami:v1.10\n  worker:\n    image: traefik/whoami:v1.10\n"
	if _, err := RewriteComposeImage(dupSrc, "web", "traefik/whoami:v1.10", "traefik/whoami:v1.11"); !errors.Is(err, ErrManualEdit) {
		t.Fatalf("expected ErrManualEdit for duplicate image value, got %v", err)
	}

	// End-to-end ChangeServiceImage validates and records a git version with the right message and author.
	fb := newFakeImageBackend()
	e := newTestImageEngine(t, fb)
	if err := e.SaveStack(context.Background(), "whoami", src, "", "alice", true); err != nil {
		t.Fatalf("SaveStack: %v", err)
	}
	ver, err := e.ChangeServiceImage(context.Background(), "whoami", "web", "traefik/whoami:v1.10", "traefik/whoami:v1.11", "bob")
	if err != nil {
		t.Fatalf("ChangeServiceImage: %v", err)
	}
	if ver == nil {
		t.Fatalf("expected git version commit")
	}
	wantMsg := "Change web image traefik/whoami v1.10 → v1.11"
	if ver.Message != wantMsg || ver.Author != "bob" {
		t.Fatalf("version = %+v, want msg=%q author=bob", ver, wantMsg)
	}
}
