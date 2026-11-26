package tunnel

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base32"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"moorage/internal/host"
	"moorage/internal/notify"
	"moorage/internal/store"
)

// JoinTokenTTL is how long a pairing token stays valid.
const JoinTokenTTL = 24 * time.Hour

// Hub accepts agent connections on its own TLS port (separate from the UI port,
// so a reverse proxy in front of the UI can't strip the client certificate).
type Hub struct {
	CA           *CA
	St           *store.Store
	Reg          *host.Registry
	Log          *slog.Logger
	HubName      string
	pubMu        sync.RWMutex
	PublicURL    string
	OnConnect    func(id string)
	OnDisconnect func(id string)
	// MaintenanceUntil returns the end of a running planned maintenance window that covers
	// everything (zero if none). It extends the watchdog pause pushed to the agents.
	MaintenanceUntil func(now time.Time) time.Time
}

// SetPublicURL updates the hub public URL live and pushes updated watchdog links to agents.
func (h *Hub) SetPublicURL(u string) {
	if h == nil {
		return
	}
	h.pubMu.Lock()
	h.PublicURL = strings.TrimRight(u, "/")
	h.pubMu.Unlock()
	go h.PushWatchdog(context.Background())
}

func (h *Hub) getPublicURL() string {
	h.pubMu.RLock()
	defer h.pubMu.RUnlock()
	return strings.TrimRight(h.PublicURL, "/")
}

// NewJoinToken creates a one-time token for pairing an agent called name.
func (h *Hub) NewJoinToken(ctx context.Context, name string) (string, time.Time, error) {
	tok, hash, err := NewJoinToken(h.CA)
	if err != nil {
		return "", time.Time{}, err
	}
	exp := time.Now().Add(JoinTokenTTL)
	return tok, exp, h.St.CreateJoinToken(ctx, hash, name, exp)
}

// Serve listens on addr until ctx ends.
func (h *Hub) Serve(ctx context.Context, addr string) error {
	cert, err := h.CA.ServerCertificate()
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	pool.AddCert(h.CA.Cert)
	srv := &http.Server{
		Addr:              addr,
		Handler:           h.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(h.Log.Handler(), slog.LevelDebug),
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.VerifyClientCertIfGiven, // enrolment comes without a certificate
			ClientCAs:    pool,
		},
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	h.Log.Info("agent port listening", "addr", ln.Addr().String())
	err = srv.ServeTLS(ln, "", "")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (h *Hub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /enroll", h.enroll)
	mux.HandleFunc("GET /connect", h.connect)
	return mux
}

type enrollReq struct {
	Secret   string `json:"secret"`
	CSR      []byte `json:"csr"`
	Hostname string `json:"hostname"`
}

type enrollResp struct {
	AgentID string `json:"agentId"`
	Cert    []byte `json:"cert"`
	CA      []byte `json:"ca"`
}

func (h *Hub) enroll(w http.ResponseWriter, r *http.Request) {
	var req enrollReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	hash := HashSecret(req.Secret)
	a, err := h.St.RedeemJoinToken(r.Context(), hash, newAgentID(), cleanHostname(req.Hostname))
	if errors.Is(err, store.ErrNotFound) {
		h.Log.Warn("agent enrolment refused", "remote", r.RemoteAddr)
		http.Error(w, "the join token is unknown, used or expired; create a new one on the hub", http.StatusForbidden)
		return
	}
	if err != nil {
		h.Log.Error("enroll", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// A retry after a lost answer gets the same agent (and a new certificate for it).
	id, retry := a.ID, a.Retried
	der, err := h.CA.SignAgent(req.CSR, id)
	if err != nil {
		if !retry {
			h.St.ReleaseJoinToken(r.Context(), hash, id) //nolint:errcheck
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if retry {
		h.Log.Info("agent pairing retried", "agent", id, "name", a.Name, "remote", r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(enrollResp{AgentID: id, Cert: der, CA: h.CA.Cert.Raw}) //nolint:errcheck
		return
	}
	h.Log.Info("agent paired", "agent", id, "name", a.Name, "hostname", a.Hostname, "remote", r.RemoteAddr)
	// The hub pairs the agent on its own (the admin only created the token), so actor is system.
	ip, _, splitErr := net.SplitHostPort(r.RemoteAddr)
	if splitErr != nil {
		ip = r.RemoteAddr
	}
	if err := h.St.Audit(r.Context(), store.AuditEntry{
		Actor: store.AuditActorSystem, IP: ip, Action: "host.paired", HostID: id,
	}); err != nil {
		h.Log.Warn("audit agent pairing", "err", err)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(enrollResp{AgentID: id, Cert: der, CA: h.CA.Cert.Raw}) //nolint:errcheck
}

func (h *Hub) connect(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		http.Error(w, "client certificate required", http.StatusUnauthorized)
		return
	}
	id := r.TLS.VerifiedChains[0][0].Subject.CommonName
	if _, err := h.St.AgentByID(r.Context(), id); err != nil {
		h.Log.Warn("connection from removed agent refused", "agent", id, "remote", r.RemoteAddr)
		http.Error(w, "this agent was removed from the hub; pair it again", http.StatusForbidden)
		return
	}
	// The agent proved it holds its certificate: its join token can't be used again.
	if err := h.St.ConfirmAgentPaired(r.Context(), id); err != nil {
		h.Log.Warn("confirm pairing", "agent", id, "err", err)
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(maxFrame)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	nc := websocket.NetConn(ctx, c, websocket.MessageBinary)
	sess, err := yamux.Client(nc, muxConfig())
	if err != nil {
		nc.Close()
		return
	}
	defer sess.Close()

	remote := host.NewRemote(func(context.Context) (net.Conn, error) { return sess.Open() })
	ictx, icancel := context.WithTimeout(ctx, 15*time.Second)
	info, err := remote.AgentInfo(ictx)
	icancel()
	if err != nil {
		h.Log.Warn("agent handshake failed", "agent", id, "err", err)
		return
	}
	h.Reg.Attach(id, remote, info, sess.Close)
	h.St.TouchAgent(ctx, id, info.Name, time.Now()) //nolint:errcheck
	if info.WatchdogAddr != "" {
		h.St.SetAgentWatchdogAddr(ctx, id, info.WatchdogAddr) //nolint:errcheck
	}
	h.Log.Info("agent connected", "agent", id, "hostname", info.Name, "readOnly", info.ReadOnly, "version", info.Version, "remote", r.RemoteAddr)
	if h.OnConnect != nil {
		h.OnConnect(id)
	}
	go h.PushWatchdog(ctx)

	select {
	case <-sess.CloseChan():
	case <-ctx.Done():
	}
	// A reconnect may have attached a new session for this agent already: then this old
	// one is not the agent going away, and OnDisconnect (offline alerts) must not run.
	if !h.Reg.Detach(id, remote) {
		h.Log.Debug("old agent session ended after a reconnect", "agent", id)
		return
	}
	h.St.TouchAgent(context.Background(), id, "", time.Now()) //nolint:errcheck
	if h.OnDisconnect != nil {
		h.OnDisconnect(id)
	}
	h.Log.Info("agent disconnected", "agent", id)
}

// PushWatchdog distributes the current watchdog configuration (and renewed peer
// certificates) to all connected agents. Non-participating agents receive enabled=false.
func (h *Hub) PushWatchdog(ctx context.Context) {
	if h == nil || h.St == nil || h.Reg == nil || h.CA == nil {
		return
	}
	ws, err := h.St.Watchdog(ctx)
	if err != nil {
		return
	}
	agents, err := h.St.Agents(ctx)
	if err != nil {
		return
	}
	var ch store.Channel
	var chCfg notify.Config
	hasChannel := false
	if ws.Enabled && ws.ChannelID > 0 {
		if c, err := h.St.ChannelByID(ctx, ws.ChannelID); err == nil && c.Enabled {
			if json.Unmarshal([]byte(c.Config), &chCfg) == nil {
				ch = c
				hasChannel = true
			}
		}
	}
	enabled := ws.Enabled && hasChannel

	var peers []WatchdogPeer
	for _, a := range agents {
		if !a.Watchdog {
			continue
		}
		addr := a.WatchdogAddr
		if _, info, ok := h.Reg.Remote(a.ID); ok && info.WatchdogAddr != "" {
			addr = info.WatchdogAddr
		}
		peers = append(peers, WatchdogPeer{ID: a.ID, Name: a.Name, Addr: addr})
	}
	if len(peers) == 0 {
		enabled = false
	}

	hubName := h.HubName
	if hubName == "" {
		if list, err := h.Reg.List(ctx); err == nil {
			for _, e := range list {
				if e.ID == host.LocalID && e.Name != "" {
					hubName = e.Name
					break
				}
			}
		}
	}
	if hubName == "" {
		hubName = "Moorage"
	}

	now := time.Now()
	maintUntil := ws.MaintenanceUntil
	if h.MaintenanceUntil != nil {
		if planned := h.MaintenanceUntil(now); planned.After(maintUntil) {
			maintUntil = planned
		}
	}
	for _, a := range agents {
		rm, info, ok := h.Reg.Remote(a.ID)
		if !ok || !info.Watchdog {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if !enabled || !a.Watchdog {
			_ = rm.PushWatchdog(pctx, WatchdogConfig{Enabled: false})
			cancel()
			continue
		}
		cert := a.WatchdogCert
		if !PeerCertValid(cert, a.ID, now) {
			csr, err := rm.WatchdogCSR(pctx)
			if err != nil {
				cancel()
				if !host.IsUnsupported(err) && h.Log != nil {
					h.Log.Warn("watchdog CSR failed", "agent", a.ID, "err", err)
				}
				continue
			}
			cert, err = h.CA.SignPeer(csr, a.ID)
			if err != nil {
				cancel()
				if h.Log != nil {
					h.Log.Warn("watchdog sign peer cert failed", "agent", a.ID, "err", err)
				}
				continue
			}
			_ = h.St.SetAgentWatchdogCert(pctx, a.ID, cert)
		}
		cfg := WatchdogConfig{
			Enabled:          true,
			Grace:            ws.Grace,
			MaintenanceUntil: maintUntil,
			HubName:          hubName,
			PublicURL:        h.getPublicURL(),
			ChannelKind:      ch.Kind,
			ChannelConfig:    chCfg,
			Peers:            peers,
			PeerCert:         cert,
			IconBase:         notify.IconBase(),
		}
		cfg.ConfigVersion = cfg.Hash()
		if err := rm.PushWatchdog(pctx, cfg); err != nil && !host.IsUnsupported(err) && h.Log != nil {
			h.Log.Warn("watchdog push failed", "agent", a.ID, "err", err)
		}
		cancel()
	}
}

// TestWatchdog asks one connected participating agent to send a test notification
// directly to the given channel, returning the agent's display name.
func (h *Hub) TestWatchdog(ctx context.Context, ch store.Channel) (string, error) {
	if h == nil || h.St == nil || h.Reg == nil {
		return "", errors.New("the agent port is off")
	}
	var chCfg notify.Config
	if err := json.Unmarshal([]byte(ch.Config), &chCfg); err != nil {
		return "", err
	}
	agents, err := h.St.Agents(ctx)
	if err != nil {
		return "", err
	}
	hubName := h.HubName
	if hubName == "" {
		hubName = "Moorage"
	}
	// Prefer an enabled participating agent; fall back to any connected watchdog-capable agent.
	for pass := range 2 {
		for _, a := range agents {
			if pass == 0 && !a.Watchdog {
				continue
			}
			rm, info, ok := h.Reg.Remote(a.ID)
			if !ok || !info.Watchdog {
				continue
			}
			err := rm.TestWatchdog(ctx, map[string]any{
				"hubName":       hubName,
				"publicUrl":     h.getPublicURL(),
				"channelKind":   ch.Kind,
				"channelConfig": chCfg,
			})
			if err != nil {
				return a.Name, err
			}
			return a.Name, nil
		}
	}
	return "", errors.New("no connected agent supports the watchdog")
}

func newAgentID() string {
	b := make([]byte, 8)
	rand.Read(b) //nolint:errcheck // crypto/rand never fails on supported platforms
	return "ag_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

func cleanHostname(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// maxFrame bounds one WebSocket message; yamux frames are far smaller.
const maxFrame = 4 << 20

func muxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.LogOutput = io.Discard
	c.KeepAliveInterval = 15 * time.Second
	c.ConnectionWriteTimeout = 15 * time.Second
	c.MaxStreamWindowSize = 1 << 20
	return c
}
