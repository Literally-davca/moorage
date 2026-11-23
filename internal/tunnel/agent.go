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

// Run pairs the agent if needed and then stays connected until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	if err := os.MkdirAll(a.Dir, 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(a.path("agent.crt")); errors.Is(err, os.ErrNotExist) {
		if a.JoinToken == "" {
			return errors.New("this agent isn't paired yet: set MOORAGE_JOIN_TOKEN to a token from the hub (Hosts → Add host)")
		}
		if err := a.enroll(ctx); err != nil {
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
			return fmt.Errorf("%w: this agent was removed on the hub, delete %s and pair it again", errRejected, a.Dir)
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

// enroll creates a key, sends a certificate request with the join token and
// stores the signed certificate. The hub is authenticated by the CA fingerprint
// inside the token, so the token can't be redeemed by a fake hub.
func (a *Agent) enroll(ctx context.Context) error {
	secret, fp, err := ParseJoinToken(a.JoinToken)
	if err != nil {
		return err
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
			return verifyPinned(raw, fp)
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
		return errors.New(strings.TrimSpace(string(msg)))
	}
	var er enrollResp
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return err
	}
	if fingerprint(er.CA) != fp {
		return errors.New("the hub sent a different CA than the token names")
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
