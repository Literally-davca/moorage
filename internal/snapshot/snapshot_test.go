package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testSecret   = "S3CR3T-never-in-errors"
	testPassword = "dav-pass-never-in-errors"
	testToken    = "ghp_token_never_in_errors"
)

// ---- fake S3 ------------------------------------------------------------------------

type fakeS3 struct {
	mu      sync.Mutex
	objects map[string]string // "bucket/key" -> body
	types   map[string]string
	badSig  int
}

func newFakeS3(t *testing.T) (*fakeS3, *httptest.Server) {
	f := &fakeS3{objects: map[string]string{}, types: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Verify the signature as the server sees the request (path, host, date).
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKID/") {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<?xml version="1.0"?><Error><Code>InvalidAccessKeyId</Code><Message>nope</Message></Error>`)
			return
		}
		check := r.Clone(context.Background())
		check.URL.Host = r.Host
		at, _ := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
		signS3(check, body, "auto", "AKID", testSecret, at)
		if check.Header.Get("Authorization") != auth {
			f.mu.Lock()
			f.badSig++
			f.mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<Error><Code>SignatureDoesNotMatch</Code></Error>`)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/")
		if !strings.HasPrefix(key, "status-bucket/") {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `<Error><Code>NoSuchBucket</Code></Error>`)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			f.objects[key] = string(body)
			f.types[key] = r.Header.Get("Content-Type")
		case http.MethodDelete:
			delete(f.objects, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func s3Config(endpoint string) Config {
	return Config{Provider: ProviderS3, S3Endpoint: endpoint, S3Bucket: "status-bucket", S3AccessKey: "AKID", S3SecretKey: testSecret, S3Prefix: "/site/"}
}

func pageFiles() []File {
	return []File{
		{Path: "main/status.json", ContentType: "application/json; charset=utf-8", Data: []byte(`{"format":1}`)},
		{Path: "main/index.html", ContentType: "text/html; charset=utf-8", Data: []byte("<!doctype html><title>Status ✓</title>")},
	}
}

func TestS3PublishAndTest(t *testing.T) {
	f, srv := newFakeS3(t)
	c := s3Config(srv.URL)
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.S3Prefix != "site/" || c.S3Region != "auto" {
		t.Fatalf("normalised: %+v", c)
	}
	tg, err := NewTarget(c)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := tg.Test(ctx); err != nil {
		t.Fatalf("test: %v", err)
	}
	if len(f.objects) != 0 {
		t.Fatalf("test left objects behind: %v", f.objects)
	}
	if err := tg.Publish(ctx, pageFiles()); err != nil {
		t.Fatal(err)
	}
	if f.objects["status-bucket/site/main/index.html"] != "<!doctype html><title>Status ✓</title>" ||
		f.types["status-bucket/site/main/index.html"] != "text/html; charset=utf-8" ||
		f.objects["status-bucket/site/main/status.json"] == "" || f.badSig != 0 {
		t.Fatalf("objects: %v types: %v badSig: %d", f.objects, f.types, f.badSig)
	}

	// A missing bucket is reported (never created), without the secret.
	c.S3Bucket = "missing-bucket"
	tg, _ = NewTarget(c)
	err = tg.Test(ctx)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") || !strings.Contains(err.Error(), "must already exist") {
		t.Fatalf("missing bucket: %v", err)
	}
	c.S3Bucket, c.S3AccessKey = "status-bucket", "WRONG"
	tg, _ = NewTarget(c)
	err = tg.Publish(ctx, pageFiles())
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "InvalidAccessKeyId") || strings.Contains(err.Error(), testSecret) {
		t.Fatalf("bad key: %v", err)
	}
}

// ---- fake WebDAV ----------------------------------------------------------------------

type fakeDAV struct {
	mu    sync.Mutex
	files map[string]string
	dirs  map[string]bool
}

func newFakeDAV(t *testing.T) (*fakeDAV, *httptest.Server) {
	f := &fakeDAV{files: map[string]string{}, dirs: map[string]bool{"/dav/site": true}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "ann" || p != testPassword {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		p := strings.TrimSuffix(r.URL.Path, "/")
		parent := p[:strings.LastIndex(p, "/")]
		switch r.Method {
		case http.MethodPut:
			if !f.dirs[parent] {
				w.WriteHeader(http.StatusConflict)
				return
			}
			f.files[p] = string(body)
			w.WriteHeader(http.StatusCreated)
		case "MKCOL":
			if !f.dirs[parent] {
				w.WriteHeader(http.StatusConflict)
				return
			}
			if f.dirs[p] {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			f.dirs[p] = true
			w.WriteHeader(http.StatusCreated)
		case http.MethodDelete:
			delete(f.files, p)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func TestWebDAVPublishAndTest(t *testing.T) {
	f, srv := newFakeDAV(t)
	c := Config{Provider: ProviderWebDAV, WebDAVURL: srv.URL + "/dav/site/", WebDAVUsername: "ann", WebDAVPassword: testPassword}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	tg, _ := NewTarget(c)
	ctx := context.Background()
	if err := tg.Test(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.files) != 0 {
		t.Fatalf("test left files: %v", f.files)
	}
	// The page folder is created inside the configured folder; publishing twice works.
	for range 2 {
		if err := tg.Publish(ctx, pageFiles()); err != nil {
			t.Fatal(err)
		}
	}
	if !f.dirs["/dav/site/main"] || f.files["/dav/site/main/status.json"] != `{"format":1}` || f.files["/dav/site/main/index.html"] == "" {
		t.Fatalf("dirs %v files %v", f.dirs, f.files)
	}

	// The configured folder itself is never created.
	c.WebDAVURL = srv.URL + "/dav/missing"
	tg, _ = NewTarget(c)
	if err := tg.Publish(ctx, pageFiles()); err == nil || !strings.Contains(err.Error(), "MKCOL") {
		t.Fatalf("missing folder: %v", err)
	}
	if f.dirs["/dav/missing"] {
		t.Fatal("the configured folder was created")
	}
	c.WebDAVURL, c.WebDAVPassword = srv.URL+"/dav/site", "wrong"
	tg, _ = NewTarget(c)
	if err := tg.Test(ctx); err == nil || !strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), testPassword) {
		t.Fatalf("wrong password: %v", err)
	}
}

// ---- fake GitHub ----------------------------------------------------------------------

type fakeGitHub struct {
	mu        sync.Mutex
	head      string
	commits   map[string]string // commit sha -> tree sha
	trees     map[string]map[string]string
	conflicts int // PATCH answers 422 this many times (someone pushed meanwhile)
	n         int
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, *httptest.Server) {
	f := &fakeGitHub{head: "c0", commits: map[string]string{"c0": "t0"}, trees: map[string]map[string]string{"t0": {"README.md": "hi"}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"message":"Bad credentials"}`)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, "/repos/acme/status")
		if p == r.URL.Path {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)                 //nolint:errcheck
		out := func(v any) { json.NewEncoder(w).Encode(v) } //nolint:errcheck
		f.n++
		id := func(prefix string) string { return prefix + string(rune('a'+f.n)) }
		switch {
		case r.Method == http.MethodGet && p == "/git/ref/heads/gh-pages":
			out(map[string]any{"object": map[string]string{"sha": f.head}})
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/git/commits/"):
			out(map[string]any{"tree": map[string]string{"sha": f.commits[strings.TrimPrefix(p, "/git/commits/")]}})
		case r.Method == http.MethodPost && p == "/git/trees":
			base := f.trees[in["base_tree"].(string)]
			tree := map[string]string{}
			for k, v := range base {
				tree[k] = v
			}
			for _, e := range in["tree"].([]any) {
				m := e.(map[string]any)
				tree[m["path"].(string)] = m["content"].(string)
			}
			sha := id("t")
			f.trees[sha] = tree
			w.WriteHeader(http.StatusCreated)
			out(map[string]string{"sha": sha})
		case r.Method == http.MethodPost && p == "/git/commits":
			parents := in["parents"].([]any)
			if parents[0].(string) == "" {
				w.WriteHeader(http.StatusUnprocessableEntity)
				return
			}
			sha := id("c")
			f.commits[sha] = in["tree"].(string)
			w.WriteHeader(http.StatusCreated)
			out(map[string]string{"sha": sha})
		case r.Method == http.MethodPatch && p == "/git/refs/heads/gh-pages":
			if f.conflicts > 0 {
				f.conflicts--
				f.head = "c0" // someone else pushed (same tree, new head would be fine too)
				w.WriteHeader(http.StatusUnprocessableEntity)
				io.WriteString(w, `{"message":"Update is not a fast forward"}`)
				return
			}
			f.head = in["sha"].(string)
			out(map[string]any{"object": map[string]string{"sha": f.head}})
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"Not Found"}`)
		}
	}))
	t.Cleanup(srv.Close)
	old := GitHubAPI
	GitHubAPI = srv.URL
	t.Cleanup(func() { GitHubAPI = old })
	return f, srv
}

func TestGitHubPublishAndTest(t *testing.T) {
	f, _ := newFakeGitHub(t)
	c := Config{Provider: ProviderGitHub, GitHubRepo: "acme/status", GitHubPath: "status", GitHubToken: testToken, Interval: 5, Stale: 10}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.GitHubBranch != "gh-pages" || c.Interval != MinGitHubInterval || c.Stale != MinGitHubStale || c.MinGap() < 6*time.Minute {
		t.Fatalf("GitHub limits not applied: %+v", c)
	}
	tg, _ := NewTarget(c)
	ctx := context.Background()
	if err := tg.Test(ctx); err != nil {
		t.Fatal(err)
	}
	if f.head != "c0" {
		t.Fatalf("test moved the branch to %s", f.head)
	}
	f.conflicts = 1
	if err := tg.Publish(ctx, pageFiles()); err != nil {
		t.Fatal(err)
	}
	tree := f.trees[f.commits[f.head]]
	if f.head == "c0" || tree["status/main/index.html"] == "" || tree["status/main/status.json"] != `{"format":1}` || tree["README.md"] != "hi" {
		t.Fatalf("head %s tree %v", f.head, tree)
	}
	if _, ok := tree["status/"+testFile]; ok {
		t.Fatal("the test file reached the branch")
	}

	c.GitHubRepo = "acme/missing"
	tg, _ = NewTarget(c)
	if err := tg.Test(ctx); err == nil || !strings.Contains(err.Error(), "must already exist") {
		t.Fatalf("missing repo: %v", err)
	}
	c.GitHubRepo, c.GitHubToken = "acme/status", "wrong"
	tg, _ = NewTarget(c)
	if err := tg.Test(ctx); err == nil || !strings.Contains(err.Error(), "Bad credentials") || strings.Contains(err.Error(), testToken) {
		t.Fatalf("bad token: %v", err)
	}
	if err := (&githubTarget{c: c, hc: client(), api: GitHubAPI}).Publish(ctx, []File{{Path: "x.bin", Data: []byte{0xff, 0xfe}}}); err == nil {
		t.Fatal("binary content accepted")
	}
}

// ---- config -------------------------------------------------------------------------

func TestConfigNormalizeAndMerge(t *testing.T) {
	bad := []Config{
		{Provider: ""},
		{Provider: "ftp"},
		{Provider: ProviderS3, S3Endpoint: "ftp://x", S3Bucket: "b-1", S3AccessKey: "a", S3SecretKey: "s"},
		{Provider: ProviderS3, S3Endpoint: "https://s3.example.com", S3Bucket: "Bad_Bucket", S3AccessKey: "a", S3SecretKey: "s"},
		{Provider: ProviderS3, S3Endpoint: "https://s3.example.com", S3Bucket: "bucket", S3AccessKey: "a"},
		{Provider: ProviderS3, S3Endpoint: "https://s3.example.com", S3Bucket: "bucket", S3AccessKey: "a", S3SecretKey: "s", S3Prefix: "a/../b"},
		{Provider: ProviderWebDAV, WebDAVURL: "https://user:pw@dav.example.com/x"},
		{Provider: ProviderWebDAV, WebDAVURL: "https://dav.example.com/x?token=1"},
		{Provider: ProviderGitHub, GitHubRepo: "not a repo", GitHubToken: "t"},
		{Provider: ProviderGitHub, GitHubRepo: "acme/status", GitHubToken: "t", GitHubBranch: "../main"},
		{Provider: ProviderGitHub, GitHubRepo: "acme/status"},
		{Provider: ProviderWebDAV, WebDAVURL: "https://dav.example.com", Interval: 10, Stale: 10},
		{Provider: ProviderWebDAV, WebDAVURL: "https://dav.example.com", Interval: 61},
	}
	for i, c := range bad {
		if err := c.Normalize(); err == nil {
			t.Errorf("%d: %+v accepted", i, c)
		}
	}
	ok := Config{Provider: ProviderWebDAV, WebDAVURL: "https://dav.example.com/site/", Pages: []int64{3, 3, 0, 4}}
	if err := ok.Normalize(); err != nil || ok.Interval != DefaultInterval || ok.Stale != DefaultStale || len(ok.Pages) != 2 || ok.WebDAVURL != "https://dav.example.com/site" {
		t.Fatalf("%+v %v", ok, err)
	}

	saved := Config{S3Endpoint: "https://s3.example.com", S3AccessKey: "AK", S3SecretKey: "old", WebDAVURL: "https://dav.example.com/x", WebDAVPassword: "pw", GitHubRepo: "acme/status", GitHubToken: "tok"}
	same := Config{S3Endpoint: "https://s3.example.com/", S3AccessKey: "AK", WebDAVURL: "https://dav.example.com/x", GitHubRepo: "acme/status"}
	same.Merge(saved)
	if same.S3SecretKey != "old" || same.WebDAVPassword != "pw" || same.GitHubToken != "tok" {
		t.Fatalf("secrets not kept: %+v", same)
	}
	moved := Config{S3Endpoint: "https://evil.example.com", S3AccessKey: "AK", WebDAVURL: "https://evil.example.com/x", GitHubRepo: "evil/status"}
	moved.Merge(saved)
	if moved.S3SecretKey != "" || moved.WebDAVPassword != "" || moved.GitHubToken != "" {
		t.Fatalf("a saved secret would go to another place: %+v", moved)
	}
	if d := (Config{Provider: ProviderS3, S3Bucket: "b", S3Prefix: "p/", S3SecretKey: testSecret}).Describe(); d != "s3://b/p/" {
		t.Fatalf("describe: %q", d)
	}
}

// ---- publisher ----------------------------------------------------------------------

type countTarget struct {
	n    atomic.Int32
	fail atomic.Bool
}

func (c *countTarget) Publish(context.Context, []File) error {
	c.n.Add(1)
	if c.fail.Load() {
		return errors.New("S3 PUT main/index.html: HTTP 503")
	}
	return nil
}
func (c *countTarget) Test(context.Context) error { return nil }

func newTestPublisher(tg *countTarget, on *atomic.Bool) *Publisher {
	return &Publisher{
		Load: func(context.Context) (Config, bool, error) {
			return Config{Provider: ProviderS3, S3Bucket: "b", Interval: 5, Stale: 15}, on.Load(), nil
		},
		Render: func(context.Context, Config) ([]File, []string, error) {
			return pageFiles(), []string{"main"}, nil
		},
		Target:   func(Config) (Target, error) { return tg, nil },
		Debounce: 50 * time.Millisecond,
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPublisherDebounceGapAndRetry(t *testing.T) {
	tg := &countTarget{}
	var on atomic.Bool
	on.Store(true)
	p := newTestPublisher(tg, &on)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	// A burst of changes right after start gives one publish (the start publish), and the
	// 30 s minimum gap keeps further kicks from publishing again during this test.
	for range 10 {
		p.Kick("change")
	}
	waitFor(t, "first publish", func() bool { return tg.n.Load() >= 1 })
	for range 5 {
		p.Kick("change")
	}
	time.Sleep(300 * time.Millisecond)
	if n := tg.n.Load(); n != 1 {
		t.Fatalf("publishes: %d, want 1 (debounce and minimum gap)", n)
	}
	l := p.Last()
	if !l.OK || l.LastSuccess.IsZero() || len(l.Pages) != 1 || l.Pages[0] != "main" || l.Target != "s3://b/" {
		t.Fatalf("last: %+v", l)
	}

	// A failure counts up, keeps the last success and plans a retry.
	tg.fail.Store(true)
	l, err := p.PublishNow(ctx)
	if err == nil || l.OK || l.Failures != 1 || l.LastSuccess.IsZero() || !strings.Contains(l.Error, "HTTP 503") || l.Trigger != "manual" {
		t.Fatalf("failed publish: %+v %v", l, err)
	}
	if d := p.runOnce(ctx, "retry"); d != time.Minute || p.Last().Failures != 2 || p.Last().NextRetry.IsZero() {
		t.Fatalf("retry in %s, last %+v", d, p.Last())
	}
	tg.fail.Store(false)
	if d := p.runOnce(ctx, "retry"); d != 5*time.Minute || !p.Last().OK || p.Last().Failures != 0 {
		t.Fatalf("after recovery: next in %s, last %+v", d, p.Last())
	}

	// Switched off: nothing is published, the button says why.
	on.Store(false)
	before := tg.n.Load()
	if d := p.runOnce(ctx, "periodic"); d != time.Minute || tg.n.Load() != before {
		t.Fatalf("disabled add-on published (%s)", d)
	}
	if _, err := p.PublishNow(ctx); err == nil || !strings.Contains(err.Error(), "turn the add-on on") {
		t.Fatalf("publish now while off: %v", err)
	}
}

func TestRetryDelay(t *testing.T) {
	iv := 5 * time.Minute
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if got := retryDelay(i+1, iv); got != w {
			t.Errorf("failure %d: %s, want %s", i+1, got, w)
		}
	}
}
