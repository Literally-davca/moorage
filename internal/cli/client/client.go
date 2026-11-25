package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Options configures how the CLI/TUI client locates and authenticates with Moorage.
type Options struct {
	URL     string
	Token   string
	Socket  string
	DataDir string
	Host    string
}

// APIError represents an HTTP error returned by the Moorage API.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// Client speaks HTTP and WebSocket over either a local Unix domain socket or remote HTTP(S)+Bearer.
type Client struct {
	baseURL    string
	wsBaseURL  string
	token      string
	socketPath string
	httpClient *http.Client
	isUnix     bool

	mu           sync.RWMutex
	coolifyNames map[string]CoolifyNameMeta
}

// New resolves transport from flags and MOORAGE_* environment variables.
func New(opt Options) (*Client, error) {
	rawURL := strings.TrimRight(firstNonEmpty(opt.URL, os.Getenv("MOORAGE_URL")), "/")
	token := firstNonEmpty(opt.Token, os.Getenv("MOORAGE_TOKEN"))

	if rawURL != "" {
		u, err := url.Parse(rawURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("invalid --url %q: expected http:// or https://", rawURL)
		}
		wsScheme := "ws"
		if u.Scheme == "https" {
			wsScheme = "wss"
		}
		wsU := *u
		wsU.Scheme = wsScheme
		return &Client{
			baseURL:      rawURL,
			wsBaseURL:    strings.TrimRight(wsU.String(), "/"),
			token:        token,
			httpClient:   &http.Client{Timeout: 60 * time.Second},
			coolifyNames: map[string]CoolifyNameMeta{},
		}, nil
	}

	sock := resolveSocketPath(opt.Socket, opt.DataDir)
	if sock == "" {
		return nil, errors.New("no Moorage server configured: pass --url/--token or run on a host with a Moorage socket (--socket)")
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", sock)
		},
	}
	return &Client{
		baseURL:      "http://moorage.local",
		wsBaseURL:    "ws://moorage.local",
		socketPath:   sock,
		isUnix:       true,
		httpClient:   &http.Client{Transport: tr, Timeout: 60 * time.Second},
		coolifyNames: map[string]CoolifyNameMeta{},
	}, nil
}

func resolveSocketPath(flagSock, flagDataDir string) string {
	candidates := []string{
		flagSock,
		os.Getenv("MOORAGE_SOCKET"),
	}
	if flagDataDir != "" {
		candidates = append(candidates, filepath.Join(flagDataDir, "moorage.sock"))
	}
	if envData := os.Getenv("MOORAGE_DATA_DIR"); envData != "" {
		candidates = append(candidates, filepath.Join(envData, "moorage.sock"))
	}
	candidates = append(candidates, "/data/moorage.sock", filepath.Join(".", "data", "moorage.sock"))

	var firstNonEmptyCand string
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" || strings.EqualFold(c, "off") {
			continue
		}
		if firstNonEmptyCand == "" {
			firstNonEmptyCand = c
		}
		if fi, err := os.Stat(c); err == nil && (fi.Mode()&os.ModeSocket != 0 || !fi.IsDir()) {
			return c
		}
	}
	return firstNonEmptyCand
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return strings.TrimSpace(a)
	}
	return strings.TrimSpace(b)
}

func (c *Client) TransportDescription() string {
	if c.isUnix {
		return "unix://" + c.socketPath
	}
	return c.baseURL
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Moorage", "1")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil && c.isUnix {
		return nil, fmt.Errorf("cannot reach Moorage at %s (%w); start Moorage or pass --url / --token", c.socketPath, err)
	}
	return resp, err
}

func decodeAPIError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &payload) == nil && payload.Error != "" {
		return &APIError{Status: resp.StatusCode, Message: payload.Error}
	}
	msg := strings.TrimSpace(string(raw))
	if msg == "" {
		msg = resp.Status
	}
	return &APIError{Status: resp.StatusCode, Message: msg}
}

func (c *Client) DoJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	headers := map[string]string{"Accept": "application/json"}
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
		headers["Content-Type"] = "application/json"
	}
	resp, err := c.do(ctx, method, path, body, headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return decodeAPIError(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.DoJSON(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) Post(ctx context.Context, path string, in, out any) error {
	return c.DoJSON(ctx, http.MethodPost, path, in, out)
}

func (c *Client) Put(ctx context.Context, path string, in, out any) error {
	return c.DoJSON(ctx, http.MethodPut, path, in, out)
}

func (c *Client) Patch(ctx context.Context, path string, in, out any) error {
	return c.DoJSON(ctx, http.MethodPatch, path, in, out)
}

func (c *Client) Delete(ctx context.Context, path string, out any) error {
	return c.DoJSON(ctx, http.MethodDelete, path, nil, out)
}

// PostRaw sends raw bytes (e.g. .mbk backup upload) and returns the response stream or error.
func (c *Client) PostRaw(ctx context.Context, method, path string, body io.Reader, headers map[string]string) (*http.Response, error) {
	resp, err := c.do(ctx, method, path, body, headers)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, decodeAPIError(resp)
	}
	return resp, nil
}

// DialWS opens a WebSocket stream against Moorage using the same Unix/HTTPS + Bearer transport.
func (c *Client) DialWS(ctx context.Context, path string) (*websocket.Conn, error) {
	hdr := http.Header{}
	if c.token != "" {
		hdr.Set("Authorization", "Bearer "+c.token)
	}
	opts := &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: c.httpClient.Transport},
		HTTPHeader: hdr,
	}
	conn, _, err := websocket.Dial(ctx, c.wsBaseURL+path, opts)
	return conn, err
}

// ResolveHost returns hostFlag if set, otherwise "local" if present, else the first connected host.
func (c *Client) ResolveHost(ctx context.Context, hostFlag string) (string, error) {
	if strings.TrimSpace(hostFlag) != "" {
		return strings.TrimSpace(hostFlag), nil
	}
	hosts, err := c.Hosts(ctx)
	if err != nil {
		return "", err
	}
	if len(hosts) == 0 {
		return "", errors.New("no hosts configured in Moorage")
	}
	for _, h := range hosts {
		if h.ID == "local" {
			return "local", nil
		}
	}
	for _, h := range hosts {
		if h.Connected {
			return h.ID, nil
		}
	}
	return hosts[0].ID, nil
}

func (c *Client) Hosts(ctx context.Context) ([]HostEntry, error) {
	var res struct {
		Hosts []HostEntry `json:"hosts"`
	}
	if err := c.Get(ctx, "/api/hosts", &res); err != nil {
		return nil, err
	}
	return res.Hosts, nil
}

func (c *Client) LoadCoolifyNames(ctx context.Context) {
	var res struct {
		Names map[string]CoolifyNameMeta `json:"names"`
	}
	if err := c.Get(ctx, "/api/coolify/names", &res); err == nil && res.Names != nil {
		c.mu.Lock()
		c.coolifyNames = res.Names
		c.mu.Unlock()
	}
}

func (c *Client) StackDisplayName(hostID, rawStack, fallback string) string {
	if fallback != "" && fallback != rawStack {
		return fallback
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if hostID != "" {
		if m, ok := c.coolifyNames[hostID+"/"+rawStack]; ok && m.Name != "" {
			return m.Name
		}
	}
	suffix := "/" + rawStack
	for k, v := range c.coolifyNames {
		if strings.HasSuffix(k, suffix) && v.Name != "" {
			return v.Name
		}
	}
	return rawStack
}
