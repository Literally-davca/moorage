package tunnel

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

// Agent keeps a connection to the hub open and serves Handler on it.
type Agent struct {
	Hub            string // host:port of the hub's agent port
	JoinToken      string // needed only for the first start
	Dir            string // where the agent keeps its key and certificates
	Handler        http.Handler
	Log            *slog.Logger
	WatchdogListen string         // optional MOORAGE_WATCHDOG_LISTEN address for peer mTLS
	WatchdogAddr   string         // optional MOORAGE_WATCHDOG_ADDR advertised to the hub
	Watchdog       *AgentWatchdog // optional custom watchdog (created automatically if nil)
}

// errRejected means the hub refused this agent; retrying soon won't help.
var errRejected = errors.New("rejected by hub")

// DefaultAgentPort is the hub's agent port (MOORAGE_AGENT_LISTEN default).
const DefaultAgentPort = "5081"

// CleanHubAddr turns what people paste into MOORAGE_HUB into host:port: spaces, quotes,
// a scheme (even twice: "https://wss://hub"), a path and a trailing slash are dropped,
// and a missing port becomes the default agent port 5081. The agent port speaks its own
// TLS, never through the UI's reverse proxy, so a URL there is always a mistake.
func CleanHubAddr(s string) string {
	s = strings.Trim(strings.TrimSpace(s), `"'`)
	for {
		lower := strings.ToLower(s)
		cut := false
		for _, p := range []string{"https://", "http://", "wss://", "ws://"} {
			if strings.HasPrefix(lower, p) {
				s, cut = s[len(p):], true
				break
			}
		}
		if !cut {
			break
		}
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(s); err != nil {
		// No port (or a bare IPv6 address): add the default.
		host := strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
		s = net.JoinHostPort(host, DefaultAgentPort)
	}
	return s
}

// Run pairs the agent if needed and then stays connected until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	if clean := CleanHubAddr(a.Hub); clean != a.Hub {
		if a.Log != nil {
			a.Log.Info("using cleaned-up hub address", "MOORAGE_HUB", a.Hub, "hub", clean)
		}
		a.Hub = clean
	}
	if err := os.MkdirAll(a.Dir, 0o700); err != nil {
		return err
	}
	paired := true
	if _, err := os.Stat(a.path("agent.crt")); errors.Is(err, os.ErrNotExist) {
		paired = false
	}
	switch {
	case !paired && a.JoinToken == "":
		return errors.New("this agent isn't paired yet: set MOORAGE_JOIN_TOKEN to a token from the hub (Hosts → Add host)")
	case !paired:
		if err := a.enrollRetry(ctx); err != nil {
			return fmt.Errorf("pairing with %s failed: %w", a.Hub, err)
		}
	case a.tokenForOtherHub():
		// The stored certificate is from another hub (or another CA of this one) and the
		// token names the hub to use now: pair again instead of failing the TLS check.
		a.Log.Info("join token is for another hub than the stored certificate, pairing again", "hub", a.Hub)
		if err := a.enrollRetry(ctx); err != nil {
			return fmt.Errorf("pairing with %s failed: %w", a.Hub, err)
		}
	}
	tlsCfg, err := a.clientTLS()
	if err != nil {
		return err
	}
	if a.Watchdog == nil {
		a.Watchdog = &AgentWatchdog{Dir: a.Dir, Listen: a.WatchdogListen, Log: a.Log}
	} else {
		if a.Watchdog.Dir == "" {
			a.Watchdog.Dir = a.Dir
		}
		if a.Watchdog.Listen == "" {
			a.Watchdog.Listen = a.WatchdogListen
		}
		if a.Watchdog.Log == nil {
			a.Watchdog.Log = a.Log
		}
	}
	if err := a.Watchdog.Init(); err != nil {
		return err
	}
	if err := a.Watchdog.StartListener(ctx); err != nil {
		return err
	}
	go a.Watchdog.Run(ctx) //nolint:errcheck

	backoff := time.Second
	reenrolled := false
	for ctx.Err() == nil {
		start := time.Now()
		err := a.session(ctx, tlsCfg)
		a.Watchdog.SetHubReachable(false)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		// Removed on the hub (403) with an old certificate, but a new MOORAGE_JOIN_TOKEN is
		// set: pair again once with it instead of waiting for someone to delete the volume.
		if errors.Is(err, errRejected) && a.JoinToken != "" && !reenrolled {
			reenrolled = true
			a.Log.Info("hub refused the stored certificate, pairing again with MOORAGE_JOIN_TOKEN", "hub", a.Hub)
			if perr := a.enroll(ctx); perr == nil {
				if cfg, cerr := a.clientTLS(); cerr == nil {
					tlsCfg = cfg
					continue
				}
			} else {
				a.Log.Warn("pairing again failed", "hub", a.Hub, "err", perr)
			}
		}
		wait := backoff
		if errors.Is(err, errRejected) {
			wait = time.Minute
		}
		a.Log.Warn("hub connection lost, retrying", "hub", a.Hub, "err", err, "in", wait)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
	return nil
}

func (a *Agent) path(name string) string { return filepath.Join(a.Dir, name) }

// session runs one connection: dial, then serve hub requests until it breaks.
func (a *Agent) session(ctx context.Context, tlsCfg *tls.Config) error {
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	c, resp, err := websocket.Dial(dctx, "wss://"+a.Hub+"/connect", &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}},
	})
	cancel()
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized) {
			return fmt.Errorf("%w: this agent was removed on the hub; set a new MOORAGE_JOIN_TOKEN to pair it again (or delete %s)", errRejected, a.Dir)
		}
		return err
	}
	c.SetReadLimit(maxFrame)
	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	nc := websocket.NetConn(sctx, c, websocket.MessageBinary)
	sess, err := yamux.Server(nc, muxConfig())
	if err != nil {
		nc.Close()
		return err
	}
	defer sess.Close()
	a.Log.Info("connected to hub", "hub", a.Hub)
	if a.Watchdog != nil {
		a.Watchdog.SetHubReachable(true)
	}

	handler := a.Handler
	if a.Watchdog != nil {
		addr := a.WatchdogAddr
		if addr == "" {
			addr = a.Watchdog.ListenAddr()
		}
		handler = a.Watchdog.WrapHandler(handler, addr)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return sctx }}
	go func() {
		<-sctx.Done()
		sess.Close()
	}()
	err = srv.Serve(sess)
	if sess.IsClosed() {
		return errors.New("connection closed")
	}
	return err
}

// tokenForOtherHub reports whether MOORAGE_JOIN_TOKEN names another hub CA than the
// stored ca.crt. A malformed token or an unreadable CA is not a reason to pair again.
func (a *Agent) tokenForOtherHub() bool {
	if a.JoinToken == "" {
		return false
	}
	_, fp, err := ParseJoinToken(a.JoinToken)
	if err != nil {
		return false
	}
	ca, err := readCert(a.path("ca.crt"))
	return err == nil && fingerprint(ca.Raw) != fp
}

// permanentError is an enrolment failure that retrying can't fix (the hub refused the
// token, or the hub isn't the one the token names).
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// enrollRetry pairs with the hub, retrying network, DNS, TLS and server errors with an
// exponential backoff (2 s up to 1 min) instead of ending the process: the hub may still
// be starting, or the network not up yet after a reboot. A refused token or a hub with
// another CA ends it at once.
func (a *Agent) enrollRetry(ctx context.Context) error {
	wait := 2 * time.Second
	for attempt := 1; ; attempt++ {
		err := a.enroll(ctx)
		var perm permanentError
		if err == nil || errors.As(err, &perm) || ctx.Err() != nil {
			return err
		}
		a.Log.Warn("pairing with the hub failed, retrying", "hub", a.Hub, "attempt", attempt, "err", err, "in", wait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait = min(wait*2, time.Minute)
	}
}

// enroll creates a key, sends a certificate request with the join token and
// stores the signed certificate. The hub is authenticated by the CA fingerprint
// inside the token, so the token can't be redeemed by a fake hub.
func (a *Agent) enroll(ctx context.Context) error {
	secret, fp, err := ParseJoinToken(a.JoinToken)
	if err != nil {
		return permanentError{err}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: hostname}}, key)
	if err != nil {
		return err
	}
	pinned := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // replaced by the fingerprint check below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if err := verifyPinned(raw, fp); err != nil {
				return permanentError{err}
			}
			return nil
		},
	}
	body, _ := json.Marshal(enrollReq{Secret: secret, CSR: csr, Hostname: hostname})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+a.Hub+"/enroll", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: pinned}}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		err := errors.New(strings.TrimSpace(string(msg)))
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return permanentError{err}
		}
		return fmt.Errorf("hub answered %d: %w", resp.StatusCode, err)
	}
	var er enrollResp
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return err
	}
	if fingerprint(er.CA) != fp {
		return permanentError{errors.New("the hub sent a different CA than the token names")}
	}
	if err := writeKey(a.path("agent.key"), key); err != nil {
		return err
	}
	if err := writePEM(a.path("ca.crt"), "CERTIFICATE", er.CA, 0o644); err != nil {
		return err
	}
	if err := writePEM(a.path("agent.crt"), "CERTIFICATE", er.Cert, 0o644); err != nil {
		return err
	}
	a.Log.Info("paired with hub", "hub", a.Hub, "agent", er.AgentID)
	return nil
}

// verifyPinned accepts the hub only if its chain ends in the CA named by fp.
func verifyPinned(raw [][]byte, fp string) error {
	if len(raw) == 0 {
		return errors.New("hub sent no certificate")
	}
	var ca *x509.Certificate
	for _, der := range raw {
		if fingerprint(der) == fp {
			ca, _ = x509.ParseCertificate(der)
		}
	}
	if ca == nil {
		return errors.New("the hub's certificate doesn't match the join token; check the hub address")
	}
	leaf, err := x509.ParseCertificate(raw[0])
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: HubServerName, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err
}

func (a *Agent) clientTLS() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(a.path("agent.crt"), a.path("agent.key"))
	if err != nil {
		return nil, err
	}
	ca, err := readCert(a.path("ca.crt"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   HubServerName,
	}, nil
}
