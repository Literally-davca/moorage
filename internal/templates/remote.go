package templates

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"moorage/internal/netguard"
)

// Limits for remote template indexes.
const (
	IndexFormat       = 1
	MaxIndexTemplates = 200
	// maxIndexBytes caps everything one refresh downloads (index plus all files), so a
	// hostile index can't fill the database: 200 templates × 3 files × 1 MiB would be 600 MiB.
	maxIndexBytes = 16 << 20
	fetchTimeout  = 20 * time.Second
	maxRedirects  = 5
)

// Index is the index.json of a remote template source.
//
//	{"format": 1, "name": "My templates", "templates": [
//	  {"id": "postgres", "files": {"template": "postgres/template.yaml",
//	   "compose": "postgres/compose.yaml", "env": "postgres/.env"}}]}
//
// File URLs are relative to the index URL and must stay on its host.
type Index struct {
	Format    int          `json:"format"`
	Name      string       `json:"name"`
	Templates []IndexEntry `json:"templates"`
}

// IndexEntry lists the files of one template.
type IndexEntry struct {
	ID    string `json:"id"`
	Files struct {
		Template string `json:"template"`
		Compose  string `json:"compose"`
		Env      string `json:"env,omitempty"`
	} `json:"files"`
}

// ValidateIndexURL checks an index URL an admin entered and returns it normalised.
func ValidateIndexURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 {
		return "", errors.New("enter the https URL of an index.json (at most 2048 characters)")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("the URL can't be read")
	}
	switch {
	case u.Scheme != "https":
		return "", errors.New("only https URLs are allowed")
	case u.Hostname() == "":
		return "", errors.New("the URL has no host")
	case u.User != nil:
		return "", errors.New("don't put a user name or password in the URL")
	}
	u.Fragment = ""
	return u.String(), nil
}

// Fetcher downloads remote indexes with the limits above. Connections go only to public
// addresses unless the admin allowed the local network for that index; cloud metadata
// and link-local addresses are refused always.
type Fetcher struct {
	// LookupIP resolves a host name; tests replace it. nil = the system resolver.
	LookupIP func(ctx context.Context, host string) ([]netip.Addr, error)
	// RootCAs overrides the system roots (tests use the httptest certificate).
	RootCAs *x509.CertPool
	// Timeout per request; zero = 20 s.
	Timeout time.Duration
}

// FetchedTemplate is one template of a fetched index, or its error.
type FetchedTemplate struct {
	ID      string
	Meta    string
	Compose string
	Env     string
	Err     string
}

// Fetch downloads an index and all its templates. An error means the index itself is
// unusable (and the caller keeps its cache); a single bad template only gets its Err.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string, allowLocal bool) (Index, []FetchedTemplate, error) {
	base, err := url.Parse(rawURL)
	if err != nil || base.Scheme != "https" {
		return Index{}, nil, errors.New("only https URLs are allowed")
	}
	c := f.client(allowLocal)
	defer c.CloseIdleConnections()
	budget := int64(maxIndexBytes)
	body, err := f.get(ctx, c, base, &budget)
	if err != nil {
		return Index{}, nil, fmt.Errorf("index: %w", err)
	}
	var idx Index
	if err := json.Unmarshal(body, &idx); err != nil {
		return Index{}, nil, errors.New("index: not valid JSON")
	}
	if idx.Format != IndexFormat {
		return Index{}, nil, fmt.Errorf("index: unsupported format %d (this Moorage reads format %d)", idx.Format, IndexFormat)
	}
	if len(idx.Templates) > MaxIndexTemplates {
		return Index{}, nil, fmt.Errorf("index lists %d templates, the limit is %d", len(idx.Templates), MaxIndexTemplates)
	}
	idx.Name = oneLine(idx.Name, 80)
	out := make([]FetchedTemplate, 0, len(idx.Templates))
	seen := map[string]bool{}
	for _, e := range idx.Templates {
		if !IDPattern.MatchString(e.ID) || seen[e.ID] {
			continue // no usable key to show it under
		}
		seen[e.ID] = true
		ft := f.fetchTemplate(ctx, c, base, e, &budget)
		if budget <= 0 {
			return Index{}, nil, errors.New("index: the templates are larger than 16 MiB in total")
		}
		if err := ctx.Err(); err != nil {
			return Index{}, nil, fmt.Errorf("index: %w", err)
		}
		out = append(out, ft)
	}
	return idx, out, nil
}

func (f *Fetcher) fetchTemplate(ctx context.Context, c *http.Client, base *url.URL, e IndexEntry, budget *int64) FetchedTemplate {
	ft := FetchedTemplate{ID: e.ID}
	fail := func(err error) FetchedTemplate {
		ft.Meta, ft.Compose, ft.Env, ft.Err = "", "", "", err.Error()
		return ft
	}
	if e.Files.Template == "" || e.Files.Compose == "" {
		return fail(errors.New("the index entry needs files.template and files.compose"))
	}
	files := []struct {
		ref  string
		dst  *string
		name string
	}{{e.Files.Template, &ft.Meta, "template.yaml"}, {e.Files.Compose, &ft.Compose, "compose.yaml"}, {e.Files.Env, &ft.Env, ".env"}}
	for _, file := range files {
		if file.ref == "" {
			continue
		}
		u, err := resolveFile(base, file.ref)
		if err != nil {
			return fail(fmt.Errorf("%s: %w", file.name, err))
		}
		b, err := f.get(ctx, c, u, budget)
		if err != nil {
			return fail(fmt.Errorf("%s: %w", file.name, err))
		}
		*file.dst = string(b)
	}
	m, err := ParseMeta([]byte(ft.Meta))
	if err != nil {
		return fail(err)
	}
	if m.ID != e.ID {
		return fail(fmt.Errorf("id %q in template.yaml doesn't match %q in the index", m.ID, e.ID))
	}
	if _, err := NewTemplate(m, ft.Compose, ft.Env); err != nil {
		return fail(err)
	}
	return ft
}

// resolveFile resolves a file reference against the index URL. It must stay https on
// the index's own host, so an index can't point the hub at other servers.
func resolveFile(base *url.URL, ref string) (*url.URL, error) {
	r, err := url.Parse(ref)
	if err != nil {
		return nil, errors.New("file URL can't be read")
	}
	u := base.ResolveReference(r)
	if u.Scheme != "https" || u.Host != base.Host || u.User != nil {
		return nil, errors.New("file URLs must be relative to the index (same https host)")
	}
	return u, nil
}

// get downloads one file of at most MaxFileBytes and charges it to budget.
func (f *Fetcher) get(ctx context.Context, c *http.Client, u *url.URL, budget *int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", "Moorage template fetcher")
	res, err := c.Do(req)
	if err != nil {
		return nil, friendlyFetchError(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	if res.ContentLength > MaxFileBytes {
		return nil, errors.New("file is larger than 1 MiB")
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, MaxFileBytes+1))
	if err != nil {
		return nil, friendlyFetchError(err)
	}
	if len(b) > MaxFileBytes {
		return nil, errors.New("file is larger than 1 MiB")
	}
	*budget -= int64(len(b))
	return b, nil
}

func friendlyFetchError(err error) error {
	var ge *netguard.Error
	if errors.As(err, &ge) {
		return ge
	}
	var le *localError
	if errors.As(err, &le) {
		return le
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return errors.New("timed out")
		}
		return ue.Err
	}
	return err
}

func (f *Fetcher) client(allowLocal bool) *http.Client {
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = fetchTimeout
	}
	tr := &http.Transport{
		// No proxy from the environment: the address check must see the real target.
		Proxy:                 nil,
		DialContext:           f.dialer(timeout, allowLocal),
		TLSClientConfig:       &tls.Config{RootCAs: f.RootCAs, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          2,
		IdleConnTimeout:       30 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return errors.New("redirect to a non-https URL refused")
			}
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

// localError refuses a private or loopback address when the index doesn't allow them.
type localError struct{ addr string }

func (e *localError) Error() string {
	return "blocked " + e.addr + ": a private or local network address; tick \"allow local network\" for this index if it is intended"
}

// isLocal reports addresses of the local network or this machine.
func isLocal(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsPrivate() || ip.IsLoopback() || cgnat.Contains(ip)
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// dialer resolves the host itself, checks every address and connects only to an
// allowed one, so DNS rebinding can't slip a private address past the check.
func (f *Fetcher) dialer(timeout time.Duration, allowLocal bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var ips []netip.Addr
		if ip, err := netip.ParseAddr(host); err == nil {
			ips = []netip.Addr{ip}
		} else {
			lookup := f.LookupIP
			if lookup == nil {
				lookup = func(ctx context.Context, h string) ([]netip.Addr, error) {
					return net.DefaultResolver.LookupNetIP(ctx, "ip", h)
				}
			}
			if ips, err = lookup(ctx, host); err != nil {
				return nil, fmt.Errorf("resolve %s: %w", host, err)
			}
		}
		var refused error
		for _, ip := range ips {
			if err := netguard.Check(ip, false); err != nil {
				refused = err
				continue
			}
			if !allowLocal && isLocal(ip) {
				refused = &localError{addr: ip.Unmap().String()}
				continue
			}
			conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
			if err == nil {
				return conn, nil
			}
			refused = err
		}
		if refused == nil {
			refused = fmt.Errorf("no address for %s", host)
		}
		return nil, refused
	}
}

// oneLine trims s to one line of at most n runes (index names are shown in the UI).
func oneLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	var b bytes.Buffer
	for i, r := range []rune(s) {
		if i >= n {
			break
		}
		if r >= 0x20 && r != 0x7f {
			b.WriteRune(r)
		}
	}
	return b.String()
}
