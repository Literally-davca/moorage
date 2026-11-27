package tsserve

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeTailscaled serves the two LocalAPI endpoints Moorage uses.
func fakeTailscaled(t *testing.T) (sock string, cfg *string) {
	t.Helper()
	dir := t.TempDir()
	sock = filepath.Join(dir, "ts.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("unix sockets not available:", err)
	}
	var mu sync.Mutex
	conf := `{"TCP":{"443":{"HTTPS":true}},"Web":{"pi.tail1.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:80"}}}},"AllowFunnel":{}}`
	etag := 1
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/localapi/v0/status":
			_, _ = io.WriteString(w, `{"BackendState":"Running","Self":{"DNSName":"pi.tail1.ts.net."}}`)
		case "/localapi/v0/serve-config":
			if r.Method == http.MethodGet {
				w.Header().Set("Etag", strings.Repeat("e", etag))
				_, _ = io.WriteString(w, conf)
				return
			}
			if r.Header.Get("If-Match") != strings.Repeat("e", etag) {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			b, _ := io.ReadAll(r.Body)
			conf = string(b)
			etag++
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock, &conf
}

func TestServeAndStop(t *testing.T) {
	sock, conf := fakeTailscaled(t)
	c := &Client{Socket: sock, DataDir: t.TempDir()}
	ctx := context.Background()
	e, err := c.Serve(ctx, 3001, "http://127.0.0.1:3001")
	if err != nil {
		t.Fatal(err)
	}
	if e.URL != "https://pi.tail1.ts.net:3001" {
		t.Fatalf("url %q", e.URL)
	}
	var got map[string]json.RawMessage
	_ = json.Unmarshal([]byte(*conf), &got)
	if _, ok := got["AllowFunnel"]; !ok {
		t.Fatal("unknown fields of the config must be kept")
	}
	st := c.Status(ctx)
	if !st.Available || len(st.Entries) != 2 {
		t.Fatalf("status %+v", st)
	}
	// the entry made by hand stays untouched
	if err := c.Stop(ctx, 443); err == nil {
		t.Fatal("must refuse to remove an entry Moorage didn't create")
	}
	if _, err := c.Serve(ctx, 443, "http://127.0.0.1:1"); err == nil {
		t.Fatal("must refuse to take over an entry made by hand")
	}
	if err := c.Stop(ctx, 3001); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(*conf, "3001") {
		t.Fatalf("not removed: %s", *conf)
	}
	if !strings.Contains(*conf, "pi.tail1.ts.net:443") {
		t.Fatal("the manual entry must stay")
	}
}
