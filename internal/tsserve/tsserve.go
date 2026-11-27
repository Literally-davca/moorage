// Package tsserve publishes services in the tailnet with Tailscale Serve (M19J). It talks
// to tailscaled's LocalAPI over its unix socket with plain HTTP – no tailscale.com
// library (memory limit). Only tailnet serving (HTTPS on the node's MagicDNS name), never
// Funnel. Moorage changes only the entries it created (kept in a file next to its data);
// entries made by hand are shown, never touched. Writes use the config's ETag so a
// change made meanwhile isn't overwritten.
package tsserve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultSocket is where tailscaled listens on most Linux installs.
const DefaultSocket = "/var/run/tailscale/tailscaled.sock"

// ErrDisabled: MOORAGE_TAILSCALE_SERVE is off on this node.
var ErrDisabled = errors.New("Tailscale Serve is off on this host: set MOORAGE_TAILSCALE_SERVE=true and mount /var/run/tailscale/tailscaled.sock")

// Entry is one served port.
type Entry struct {
	Port    int    `json:"port"`
	URL     string `json:"url"`    // https://node.tailnet.ts.net:port
	Target  string `json:"target"` // http://127.0.0.1:3001
	Moorage bool   `json:"moorage"`
}

// Status is what the UI shows.
type Status struct {
	Available bool    `json:"available"`
	Reason    string  `json:"reason,omitempty"`
	DNSName   string  `json:"dnsName,omitempty"`
	Entries   []Entry `json:"entries"`
}

// Client is a LocalAPI client for one node.
type Client struct {
	Socket  string
	DataDir string // the list of entries Moorage created
	mu      sync.Mutex
	hc      *http.Client
}

func (c *Client) http() *http.Client {
	if c.hc == nil {
		sock := c.Socket
		if sock == "" {
			sock = DefaultSocket
		}
		c.hc = &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}}
	}
	return c.hc
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, hdr map[string]string) (*http.Response, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://local-tailscaled.sock/localapi/v0/"+path, rd)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Sec-Tailscale", "localapi")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("can't reach tailscaled: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return resp, b, fmt.Errorf("tailscaled answered %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp, b, nil
}

// serveConfig is the part of ipn.ServeConfig Moorage reads and writes; unknown fields
// are kept as raw JSON so nothing made by hand is lost.
type serveConfig struct {
	TCP  map[string]json.RawMessage `json:"TCP,omitempty"`
	Web  map[string]json.RawMessage `json:"Web,omitempty"`
	rest map[string]json.RawMessage
}

func (s *serveConfig) UnmarshalJSON(b []byte) error {
	all := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	if v, ok := all["TCP"]; ok {
		_ = json.Unmarshal(v, &s.TCP)
		delete(all, "TCP")
	}
	if v, ok := all["Web"]; ok {
		_ = json.Unmarshal(v, &s.Web)
		delete(all, "Web")
	}
	s.rest = all
	return nil
}

func (s serveConfig) MarshalJSON() ([]byte, error) {
	all := map[string]any{}
	for k, v := range s.rest {
		all[k] = v
	}
	if len(s.TCP) > 0 {
		all["TCP"] = s.TCP
	}
	if len(s.Web) > 0 {
		all["Web"] = s.Web
	}
	return json.Marshal(all)
}

func (c *Client) dnsName(ctx context.Context) (string, error) {
	_, b, err := c.do(ctx, http.MethodGet, "status?peers=false", nil, nil)
	if err != nil {
		return "", err
	}
	var st struct {
		BackendState string
		Self         struct{ DNSName string }
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return "", err
	}
	if st.BackendState != "Running" {
		return "", fmt.Errorf("Tailscale isn't running (%s)", st.BackendState)
	}
	name := strings.TrimSuffix(st.Self.DNSName, ".")
	if name == "" {
		return "", errors.New("this node has no MagicDNS name: turn on MagicDNS and HTTPS certificates in the tailnet settings")
	}
	return name, nil
}

func (c *Client) config(ctx context.Context) (serveConfig, string, error) {
	resp, b, err := c.do(ctx, http.MethodGet, "serve-config", nil, nil)
	if err != nil {
		return serveConfig{}, "", err
	}
	var sc serveConfig
	if len(bytes.TrimSpace(b)) > 0 && string(bytes.TrimSpace(b)) != "null" {
		if err := json.Unmarshal(b, &sc); err != nil {
			return sc, "", err
		}
	}
	return sc, resp.Header.Get("Etag"), nil
}

func (c *Client) ownFile() string { return filepath.Join(c.DataDir, "tailscale-serve.json") }

func (c *Client) own() map[int]bool {
	out := map[int]bool{}
	var ports []int
	if b, err := os.ReadFile(c.ownFile()); err == nil && json.Unmarshal(b, &ports) == nil {
		for _, p := range ports {
			out[p] = true
		}
	}
	return out
}

func (c *Client) saveOwn(m map[int]bool) error {
	var ports []int
	for p := range m {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	b, _ := json.Marshal(ports)
	return os.WriteFile(c.ownFile(), b, 0o600)
}

type webHandlers struct {
	Handlers map[string]struct {
		Proxy string `json:"Proxy,omitempty"`
	} `json:"Handlers"`
}

// Status lists the served ports and whether Serve can be used here.
func (c *Client) Status(ctx context.Context) Status {
	st := Status{Entries: []Entry{}}
	name, err := c.dnsName(ctx)
	if err != nil {
		st.Reason = err.Error()
		return st
	}
	st.DNSName, st.Available = name, true
	sc, _, err := c.config(ctx)
	if err != nil {
		st.Available, st.Reason = false, err.Error()
		return st
	}
	own := c.own()
	for hp, raw := range sc.Web {
		_, portS, ok := strings.Cut(hp, ":")
		if !ok {
			continue
		}
		port, _ := strconv.Atoi(portS)
		var wh webHandlers
		_ = json.Unmarshal(raw, &wh)
		target := ""
		if h, ok := wh.Handlers["/"]; ok {
			target = h.Proxy
		}
		st.Entries = append(st.Entries, Entry{Port: port, URL: "https://" + hp, Target: target, Moorage: own[port]})
	}
	sort.Slice(st.Entries, func(i, j int) bool { return st.Entries[i].Port < st.Entries[j].Port })
	return st
}

// Serve publishes target (http://127.0.0.1:port) at https://<node>:port in the tailnet.
func (c *Client) Serve(ctx context.Context, port int, target string) (Entry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if port <= 0 || port > 65535 || !strings.HasPrefix(target, "http://") {
		return Entry{}, errors.New("give a port and an http:// target")
	}
	name, err := c.dnsName(ctx)
	if err != nil {
		return Entry{}, err
	}
	sc, etag, err := c.config(ctx)
	if err != nil {
		return Entry{}, err
	}
	key := strconv.Itoa(port)
	hp := name + ":" + key
	own := c.own()
	if _, taken := sc.TCP[key]; taken && !own[port] {
		return Entry{}, fmt.Errorf("port %d is already served by Tailscale and Moorage didn't create it – change it with the tailscale command", port)
	}
	if sc.TCP == nil {
		sc.TCP = map[string]json.RawMessage{}
	}
	if sc.Web == nil {
		sc.Web = map[string]json.RawMessage{}
	}
	sc.TCP[key] = json.RawMessage(`{"HTTPS":true}`)
	web, _ := json.Marshal(map[string]any{"Handlers": map[string]any{"/": map[string]string{"Proxy": target}}})
	sc.Web[hp] = web
	if err := c.put(ctx, sc, etag); err != nil {
		return Entry{}, err
	}
	own[port] = true
	_ = c.saveOwn(own)
	return Entry{Port: port, URL: "https://" + hp, Target: target, Moorage: true}, nil
}

// Stop removes an entry Moorage created.
func (c *Client) Stop(ctx context.Context, port int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	own := c.own()
	if !own[port] {
		return fmt.Errorf("Moorage didn't create the entry for port %d – remove it with the tailscale command", port)
	}
	sc, etag, err := c.config(ctx)
	if err != nil {
		return err
	}
	key := strconv.Itoa(port)
	delete(sc.TCP, key)
	for hp := range sc.Web {
		if strings.HasSuffix(hp, ":"+key) {
			delete(sc.Web, hp)
		}
	}
	if err := c.put(ctx, sc, etag); err != nil {
		return err
	}
	delete(own, port)
	return c.saveOwn(own)
}

func (c *Client) put(ctx context.Context, sc serveConfig, etag string) error {
	b, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	hdr := map[string]string{"Content-Type": "application/json"}
	if etag != "" {
		hdr["If-Match"] = etag
	}
	resp, _, err := c.do(ctx, http.MethodPost, "serve-config", b, hdr)
	if resp != nil && resp.StatusCode == http.StatusPreconditionFailed {
		return errors.New("the Tailscale Serve settings changed meanwhile – try again")
	}
	return err
}
