package templates

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"

	"moorage/internal/store"
)

// indexServer serves an index over TLS under the name example.com (the httptest
// certificate's name), which the fetcher's resolver hook maps to loopback.
type indexServer struct {
	mu    sync.Mutex
	files map[string]string
	fail  bool
	srv   *httptest.Server
}

func newIndexServer(t *testing.T) *indexServer {
	t.Helper()
	is := &indexServer{files: map[string]string{}}
	is.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		is.mu.Lock()
		defer is.mu.Unlock()
		if is.fail {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		if r.URL.Path == "/redirect-http" {
			http.Redirect(w, r, "http://example.com/index.json", http.StatusFound)
			return
		}
		if r.URL.Path == "/redirect-https" {
			http.Redirect(w, r, "/index.json", http.StatusFound)
			return
		}
		body, ok := is.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body)) //nolint:errcheck
	}))
	t.Cleanup(is.srv.Close)
	return is
}

func (is *indexServer) set(path, body string) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.files[path] = body
}

func (is *indexServer) url(path string) string {
	u, _ := url.Parse(is.srv.URL)
	return "https://example.com:" + u.Port() + path
}

func (is *indexServer) fetcher() *Fetcher {
	pool := x509.NewCertPool()
	pool.AddCert(is.srv.Certificate())
	return &Fetcher{
		RootCAs: pool,
		LookupIP: func(_ context.Context, host string) ([]netip.Addr, error) {
			if host == "example.com" {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			return nil, fmt.Errorf("unknown host %s", host)
		},
	}
}

const remoteMeta = "id: %s\nname: Remote %s\nversion: \"3\"\ncategory: Web\n"

func (is *indexServer) addTemplate(id string) {
	is.set("/t/"+id+"/template.yaml", fmt.Sprintf(remoteMeta, id, id))
	is.set("/t/"+id+"/compose.yaml", "services:\n  web:\n    image: nginx:1.27-alpine\n")
}

func indexJSON(ids ...string) string {
	var parts []string
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf(`{"id":%q,"files":{"template":"t/%s/template.yaml","compose":"t/%s/compose.yaml"}}`, id, id, id))
	}
	return `{"format":1,"name":"Test index","templates":[` + strings.Join(parts, ",") + `]}`
}

func TestFetchIndex(t *testing.T) {
	is := newIndexServer(t)
	is.addTemplate("one")
	is.addTemplate("two")
	is.set("/t/two/template.yaml", "id: wrong\nname: Wrong\n") // listed with its error
	is.set("/index.json", indexJSON("one", "two", "missing"))
	f := is.fetcher()
	ctx := context.Background()

	idx, list, err := f.Fetch(ctx, is.url("/index.json"), true)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Name != "Test index" || len(list) != 3 {
		t.Fatalf("index: %+v %+v", idx, list)
	}
	if list[0].Err != "" || !strings.Contains(list[0].Meta, "Remote one") {
		t.Errorf("one: %+v", list[0])
	}
	if !strings.Contains(list[1].Err, "doesn't match") || !strings.Contains(list[2].Err, "404") {
		t.Errorf("bad templates: %+v", list[1:])
	}

	// Private (here loopback) targets are refused unless the index allows the local network.
	if _, _, err := f.Fetch(ctx, is.url("/index.json"), false); err == nil || !strings.Contains(err.Error(), "allow local network") {
		t.Fatalf("loopback without permission: %v", err)
	}
	// A literal IP is checked the same way as a resolved name.
	u, _ := url.Parse(is.srv.URL)
	if _, _, err := f.Fetch(ctx, "https://127.0.0.1:"+u.Port()+"/index.json", false); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("literal loopback: %v", err)
	}
}

func TestFetchRefusals(t *testing.T) {
	is := newIndexServer(t)
	f := is.fetcher()
	ctx := context.Background()

	if _, _, err := f.Fetch(ctx, "http://example.com/index.json", true); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("http index: %v", err)
	}
	for _, bad := range []string{"http://example.com/i.json", "ftp://x/y", "https://user:pw@example.com/i.json", "https:///i.json", ""} {
		if _, err := ValidateIndexURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if u, err := ValidateIndexURL(" https://example.com/i.json#x "); err != nil || u != "https://example.com/i.json" {
		t.Errorf("valid URL: %q %v", u, err)
	}

	// Redirect to http refused; a redirect that stays on https is followed.
	if _, _, err := f.Fetch(ctx, is.url("/redirect-http"), true); err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Errorf("redirect to http: %v", err)
	}
	is.set("/index.json", indexJSON())
	if _, _, err := f.Fetch(ctx, is.url("/redirect-https"), true); err != nil {
		t.Errorf("https redirect: %v", err)
	}

	// Size limits: an index over 1 MiB, a file over 1 MiB, more than 200 templates.
	is.set("/big.json", `{"format":1,"name":"`+strings.Repeat("x", MaxFileBytes)+`","templates":[]}`)
	if _, _, err := f.Fetch(ctx, is.url("/big.json"), true); err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Errorf("big index: %v", err)
	}
	is.addTemplate("huge")
	is.set("/t/huge/compose.yaml", strings.Repeat("#", MaxFileBytes+1))
	is.set("/huge.json", indexJSON("huge"))
	_, list, err := f.Fetch(ctx, is.url("/huge.json"), true)
	if err != nil || len(list) != 1 || !strings.Contains(list[0].Err, "1 MiB") {
		t.Errorf("big file: %+v %v", list, err)
	}
	ids := make([]string, MaxIndexTemplates+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("t%d", i)
	}
	is.set("/many.json", indexJSON(ids...))
	if _, _, err := f.Fetch(ctx, is.url("/many.json"), true); err == nil || !strings.Contains(err.Error(), "limit is 200") {
		t.Errorf("too many templates: %v", err)
	}

	// Files must stay on the index's host.
	is.set("/other.json", `{"format":1,"templates":[{"id":"x","files":{"template":"https://evil.example.net/t.yaml","compose":"c.yaml"}}]}`)
	_, list, err = f.Fetch(ctx, is.url("/other.json"), true)
	if err != nil || len(list) != 1 || !strings.Contains(list[0].Err, "same https host") {
		t.Errorf("foreign file host: %+v %v", list, err)
	}

	is.set("/v2.json", `{"format":2,"templates":[]}`)
	if _, _, err := f.Fetch(ctx, is.url("/v2.json"), true); err == nil || !strings.Contains(err.Error(), "format") {
		t.Errorf("unknown format: %v", err)
	}
}

// Metadata and link-local addresses are refused even when the local network is allowed.
func TestFetchMetadataAlwaysBlocked(t *testing.T) {
	f := &Fetcher{LookupIP: func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
	}}
	if _, _, err := f.Fetch(context.Background(), "https://meta.example/index.json", true); err == nil || !strings.Contains(err.Error(), "link-local") {
		t.Fatalf("metadata: %v", err)
	}
}

// A failing refresh keeps the last good copy and records the error.
func TestRefreshKeepsCache(t *testing.T) {
	is := newIndexServer(t)
	is.addTemplate("one")
	is.set("/index.json", indexJSON("one"))
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := New(db, "", quietLog())
	s.Fetch = is.fetcher()
	ctx := context.Background()

	id, err := s.AddSource(ctx, is.url("/index.json"), true)
	if err != nil {
		t.Fatal(err)
	}
	s.Wait() // AddSource fetches in the background
	if _, err := s.AddSource(ctx, is.url("/index.json"), false); err == nil {
		t.Fatal("duplicate URL accepted")
	}
	key := RemoteKey(id)
	tpl, e, err := s.Get(ctx, key, "one")
	if err != nil || e.SourceLabel != "from example.com" || tpl.Compose == "" || e.Version != "3" {
		t.Fatalf("after first refresh: %+v %+v %v", e, tpl, err)
	}

	is.mu.Lock()
	is.fail = true
	is.mu.Unlock()
	if err := s.RefreshNow(ctx, id); err == nil {
		t.Fatal("refresh against a failing server succeeded")
	}
	if _, _, err := s.Get(ctx, key, "one"); err != nil {
		t.Fatalf("cache lost after a failed refresh: %v", err)
	}
	c, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var src SourceInfo
	for _, si := range c.Sources {
		if si.Key == key {
			src = si
		}
	}
	if src.Count != 1 || !strings.Contains(src.Error, "500") || src.LastOK.IsZero() || src.Label != "from example.com" {
		t.Fatalf("source after failure: %+v", src)
	}
	list, err := s.Sources(ctx)
	if err != nil || len(list) != 1 || list[0].Count != 1 || list[0].LastError == "" || list[0].Host != "example.com" {
		t.Fatalf("admin sources: %+v %v", list, err)
	}

	// A new URL is a different index: the old cache goes.
	if err := s.UpdateSource(ctx, id, is.url("/other-index.json"), true); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	if _, _, err := s.Get(ctx, key, "one"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old cache kept after URL change: %v", err)
	}
	if err := s.DeleteSource(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSource(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}
