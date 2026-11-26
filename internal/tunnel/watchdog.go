package tunnel

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"moorage/internal/notify"
)

// WatchdogPeer is one participating agent in the watchdog configuration.
type WatchdogPeer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Addr string `json:"addr,omitempty"`
}

// WatchdogConfig is the configuration pushed from the hub to participating agents
// and persisted in <Agent.Dir>/watchdog.json (mode 0600).
type WatchdogConfig struct {
	Enabled          bool           `json:"enabled"`
	Grace            int            `json:"grace"` // seconds
	MaintenanceUntil time.Time      `json:"maintenanceUntil,omitzero"`
	HubName          string         `json:"hubName,omitempty"`
	PublicURL        string         `json:"publicUrl,omitempty"`
	ChannelKind      string         `json:"channelKind,omitempty"`
	ChannelConfig    notify.Config  `json:"channelConfig,omitzero"`
	Peers            []WatchdogPeer `json:"peers,omitempty"`
	PeerCert         []byte         `json:"peerCert,omitempty"` // DER-encoded peer certificate
	ConfigVersion    string         `json:"configVersion,omitempty"`
	// IconBase is the hub's logo base URL for notifications (M17B; "" = default).
	IconBase string `json:"iconBase,omitempty"`
}

// Hash computes a short fingerprint of the shared watchdog parameters (excluding PeerCert)
// so peers can tell whether another agent runs on a stale configuration.
func (c WatchdogConfig) Hash() string {
	type canon struct {
		Enabled     bool
		Grace       int
		Maint       int64
		HubName     string
		PublicURL   string
		ChannelKind string
		Peers       []WatchdogPeer
	}
	var maint int64
	if !c.MaintenanceUntil.IsZero() {
		maint = c.MaintenanceUntil.Unix()
	}
	b, _ := json.Marshal(canon{
		Enabled: c.Enabled, Grace: c.Grace, Maint: maint,
		HubName: c.HubName, PublicURL: c.PublicURL, ChannelKind: c.ChannelKind,
		Peers: c.Peers,
	})
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(sum[:8])
}

// WatchdogStatus is what GET /watchdog/status returns between peers.
type WatchdogStatus struct {
	ID            string    `json:"id"`
	HubReachable  bool      `json:"hubReachable"`
	HubDownSince  time.Time `json:"hubDownSince,omitzero"`
	ReportedDown  time.Time `json:"reportedDown,omitzero"`
	RecoverySent  time.Time `json:"recoverySent,omitzero"` // outage whose "hub is back" notice this agent sent
	ConfigVersion string    `json:"configVersion,omitempty"`
}

// PeerReading is a status sample polled from a peer along with its age at evaluation time.
type PeerReading struct {
	Status WatchdogStatus
	Age    time.Duration
}

// WatchdogState is the agent's persisted outage state.
type WatchdogState struct {
	HubReachable bool      `json:"-"`
	HubBackSince time.Time `json:"-"`
	HubDownSince time.Time `json:"hubDownSince,omitzero"`
	ReportedDown time.Time `json:"reportedDown,omitzero"` // non-zero if this agent sent the down notice
	ObservedDown time.Time `json:"observedDown,omitzero"` // non-zero if this agent or a peer reported the outage
	ObservedBy   string    `json:"observedBy,omitempty"`  // peer ID that reported the outage
	// RecoverySent is the outage this agent sent the "hub is back" notice for. Peers
	// see it, so a reporter that comes back after another agent already sent the
	// recovery (because the reporter was down) doesn't send a second one.
	RecoverySent time.Time `json:"recoverySent,omitzero"`
}

// DecisionKind is the action chosen by DecideWatchdog.
type DecisionKind int

const (
	DecisionNone     DecisionKind = iota
	DecisionDown                  // send the emergency hub-down notice
	DecisionRecovery              // send the hub-is-back notice
)

// WatchdogDecision is the pure output of DecideWatchdog.
type WatchdogDecision struct {
	Kind    DecisionKind
	Next    WatchdogState
	Outage  time.Time
	DownFor time.Duration
	Message notify.Message
}

// DefaultMaxStatusAge is the maximum age of a peer's status to count in a quorum or recovery check.
const DefaultMaxStatusAge = 45 * time.Second

// OutageID normalises an outage start time to the minute. Peers copy the
// reporter's value rather than computing their own, so equality holds; it is cut
// down, not rounded, because the "back after X min" duration is measured from it
// and rounding up made a 2-minute outage read as 1 minute.
func OutageID(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC().Truncate(time.Minute)
}

// DecideWatchdog is a pure, deterministic coordination function. Given this agent's
// own ID and state, the latest peer readings, the pushed config, the freshness bound
// and the current clock, it decides whether to send a down notice, a recovery notice,
// or nothing, and returns the updated state to persist.
func DecideWatchdog(selfID string, self WatchdogState, peers []PeerReading, cfg WatchdogConfig, maxAge time.Duration, now time.Time) WatchdogDecision {
	next := self
	if !cfg.Enabled || len(cfg.Peers) == 0 {
		return WatchdogDecision{Kind: DecisionNone, Next: WatchdogState{HubReachable: self.HubReachable}}
	}
	if maxAge <= 0 {
		maxAge = DefaultMaxStatusAge
	}
	grace := time.Duration(max(cfg.Grace, 1)) * time.Second

	peerNames := make(map[string]string, len(cfg.Peers))
	inPeers := false
	for _, p := range cfg.Peers {
		peerNames[p.ID] = p.Name
		if p.ID == selfID {
			inPeers = true
		}
	}
	if !inPeers {
		return WatchdogDecision{Kind: DecisionNone, Next: next}
	}

	// Index fresh readings from current peers only.
	fresh := make(map[string]WatchdogStatus, len(peers))
	for _, pr := range peers {
		if pr.Age > maxAge || pr.Status.ID == "" || pr.Status.ID == selfID {
			continue
		}
		if _, ok := peerNames[pr.Status.ID]; !ok {
			continue
		}
		fresh[pr.Status.ID] = pr.Status
	}

	// ---- Hub is reachable: check if we need to send a recovery notice ----
	if self.HubReachable {
		next.HubDownSince = time.Time{}
		outage := next.ReportedDown
		if outage.IsZero() {
			outage = next.ObservedDown
		}
		if outage.IsZero() {
			return WatchdogDecision{Kind: DecisionNone, Next: next}
		}
		if !cfg.MaintenanceUntil.IsZero() && (now.Before(cfg.MaintenanceUntil) || !outage.After(cfg.MaintenanceUntil)) {
			next.ReportedDown = time.Time{}
			next.ObservedDown = time.Time{}
			next.ObservedBy = ""
			return WatchdogDecision{Kind: DecisionNone, Next: next}
		}

		// Did we report it ourselves? Then send, unless a peer already did it for us
		// while we were away.
		shouldSend := !self.ReportedDown.IsZero()
		if shouldSend {
			for _, st := range fresh {
				if OutageID(st.RecoverySent).Equal(OutageID(self.ReportedDown)) {
					shouldSend = false
				}
			}
		} else {
			// Otherwise, wait until we have seen the hub back for at least one poll window so the original
			// reporter can send recovery first; then take over if the reporter is unreachable and no lower-ID
			// peer that also knows about the outage is alive.
			reporterAlive := false
			if next.ObservedBy != "" {
				if _, ok := fresh[next.ObservedBy]; ok {
					reporterAlive = true
				}
			}
			lowerAlive := false
			for id, st := range fresh {
				if !st.ReportedDown.IsZero() {
					reporterAlive = true
				}
				if id < selfID {
					lowerAlive = true
				}
			}
			if !reporterAlive && !lowerAlive {
				shouldSend = true
			}
		}

		next.ReportedDown = time.Time{}
		next.ObservedDown = time.Time{}
		next.ObservedBy = ""
		if !shouldSend {
			return WatchdogDecision{Kind: DecisionNone, Next: next}
		}
		downFor := max(now.Sub(outage), time.Second)
		next.RecoverySent = outage
		return WatchdogDecision{
			Kind:    DecisionRecovery,
			Next:    next,
			Outage:  outage,
			DownFor: downFor,
			Message: composeWatchdogUp(cfg, downFor),
		}
	}

	// ---- Hub is not reachable for this agent ----
	if next.HubDownSince.IsZero() {
		next.HubDownSince = now
	}
	// While the hub is down for us, remember if any peer reported the outage so we can send recovery if that peer dies.
	for id, st := range fresh {
		if !st.ReportedDown.IsZero() && next.ObservedDown.IsZero() {
			next.ObservedDown = OutageID(st.ReportedDown)
			next.ObservedBy = id
		}
	}
	if now.Sub(next.HubDownSince) < grace {
		return WatchdogDecision{Kind: DecisionNone, Next: next}
	}

	// Collect all participants (self + fresh peers) whose hub connection has been down for at least grace.
	type voter struct {
		id        string
		name      string
		downSince time.Time
		reported  time.Time
	}
	voters := []voter{{
		id:        selfID,
		name:      peerNames[selfID],
		downSince: next.HubDownSince,
		reported:  next.ReportedDown,
	}}
	earliest := next.HubDownSince
	for id, st := range fresh {
		if !st.HubReachable && !st.HubDownSince.IsZero() && now.Sub(st.HubDownSince) >= grace {
			voters = append(voters, voter{
				id:        id,
				name:      peerNames[id],
				downSince: st.HubDownSince,
				reported:  st.ReportedDown,
			})
			if st.HubDownSince.Before(earliest) {
				earliest = st.HubDownSince
			}
		}
	}

	total := len(cfg.Peers)
	if len(voters) <= total/2 {
		return WatchdogDecision{Kind: DecisionNone, Next: next}
	}

	outage := OutageID(earliest)
	// Suppress if currently in maintenance, or if the outage started during maintenance.
	if !cfg.MaintenanceUntil.IsZero() && (now.Before(cfg.MaintenanceUntil) || !earliest.After(cfg.MaintenanceUntil)) {
		return WatchdogDecision{Kind: DecisionNone, Next: next}
	}

	// If we or any fresh peer already reported this outage, record ObservedDown and don't send again.
	if !next.ReportedDown.IsZero() {
		next.ObservedDown = OutageID(next.ReportedDown)
		return WatchdogDecision{Kind: DecisionNone, Next: next}
	}
	for id, st := range fresh {
		if !st.ReportedDown.IsZero() {
			next.ObservedDown = OutageID(st.ReportedDown)
			next.ObservedBy = id
			return WatchdogDecision{Kind: DecisionNone, Next: next}
		}
	}

	// Deterministic reporter: lowest agent ID among the down voters.
	sort.Slice(voters, func(i, j int) bool { return voters[i].id < voters[j].id })
	if voters[0].id != selfID {
		return WatchdogDecision{Kind: DecisionNone, Next: next}
	}

	var names []string
	for _, v := range voters {
		if v.name != "" {
			names = append(names, v.name)
		} else {
			names = append(names, v.id)
		}
	}
	next.ReportedDown = outage
	next.ObservedDown = outage
	return WatchdogDecision{
		Kind:    DecisionDown,
		Next:    next,
		Outage:  outage,
		Message: composeWatchdogDown(cfg, earliest, len(voters), total, names),
	}
}

func composeWatchdogDown(cfg WatchdogConfig, since time.Time, seen, total int, names []string) notify.Message {
	hub := cfg.HubName
	if hub == "" {
		hub = "Moorage"
	}
	word := "agents"
	if total == 1 {
		word = "agent"
	}
	return notify.Message{
		Status: "down",
		Event:  notify.EventWatchdog,
		Host:   hub,
		Since:  since,
		Cause:  fmt.Sprintf("seen by %d of %d %s", seen, total, word),
		Title:  fmt.Sprintf("Moorage hub %s is unreachable", hub),
		Text: fmt.Sprintf("Since %s (seen by %d of %d %s: %s). Stacks keep running. Alerts from Moorage are paused until the hub is back.",
			since.Local().Format("15:04"), seen, total, word, strings.Join(names, ", ")),
		URL: cfg.PublicURL,
	}
}

func composeWatchdogUp(cfg WatchdogConfig, downFor time.Duration) notify.Message {
	hub := cfg.HubName
	if hub == "" {
		hub = "Moorage"
	}
	return notify.Message{
		Status:   "up",
		Event:    notify.EventWatchdog,
		Host:     hub,
		Duration: downFor,
		Title:    fmt.Sprintf("Moorage hub %s is back after %s", hub, humanDuration(downFor)),
		Text:     fmt.Sprintf("Moorage hub %s is back after %s.", hub, humanDuration(downFor)),
		URL:      cfg.PublicURL,
	}
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", max(int(d.Seconds()), 1))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
	}
}

// ---- Agent-side watchdog runner --------------------------------------------

// WatchdogFile is the JSON structure persisted at <Agent.Dir>/watchdog.json (mode 0600).
type WatchdogFile struct {
	Config WatchdogConfig `json:"config"`
	State  WatchdogState  `json:"state"`
}

// AgentWatchdog runs the peer mTLS listener, peer polling loop, and coordination
// logic on an agent.
type AgentWatchdog struct {
	Dir    string
	Listen string // MOORAGE_WATCHDOG_LISTEN, e.g. ":5082" (optional when N=1)
	Log    *slog.Logger

	// Timings and hooks (injectable for tests).
	PollInterval time.Duration // default 15 s
	PollTimeout  time.Duration // default 5 s
	MaxStatusAge time.Duration // default 45 s
	Now          func() time.Time
	Send         func(ctx context.Context, kind string, cfg notify.Config, m notify.Message) error

	mu           sync.Mutex
	selfID       string
	ca           *x509.Certificate
	peerKey      *ecdsa.PrivateKey
	cfg          WatchdogConfig
	state        WatchdogState
	peers        map[string]peerSample
	hubConnected bool
	hubBackSince time.Time
	everConn     bool
	wake         chan struct{}

	lnMu sync.Mutex
	ln   net.Listener
}

type peerSample struct {
	status WatchdogStatus
	at     time.Time
}

func (w *AgentWatchdog) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *AgentWatchdog) send(ctx context.Context, kind string, cfg notify.Config, m notify.Message) error {
	// The sender is this agent in its watchdog role, with the hub's logo base (M17B).
	w.mu.Lock()
	name, base := w.selfName(), w.cfg.IconBase
	w.mu.Unlock()
	if m.Sender.Role == "" {
		m.Sender = notify.Sender{Host: name, Role: notify.RoleWatchdog}
	}
	if m.IconBase == "" {
		m.IconBase = base
	}
	if m.Event == "" {
		m.Event = notify.EventWatchdog
	}
	if w.Send != nil {
		return w.Send(ctx, kind, cfg, m)
	}
	return notify.Send(ctx, kind, cfg, m)
}

func (w *AgentWatchdog) path(name string) string { return filepath.Join(w.Dir, name) }

// selfName is this agent's name in the watchdog peer list (its ID when unknown). Call
// with w.mu held.
func (w *AgentWatchdog) selfName() string {
	for _, p := range w.cfg.Peers {
		if p.ID == w.selfID && p.Name != "" {
			return p.Name
		}
	}
	return w.selfID
}

// Init loads the agent's identity, CA certificate, and any persisted watchdog.json.
func (w *AgentWatchdog) Init() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.peers == nil {
		w.peers = map[string]peerSample{}
	}
	if w.wake == nil {
		w.wake = make(chan struct{}, 1)
	}
	if cert, err := readCert(w.path("agent.crt")); err == nil {
		w.selfID = cert.Subject.CommonName
	}
	if ca, err := readCert(w.path("ca.crt")); err == nil {
		w.ca = ca
	}
	if k, err := readKey(w.path("watchdog.key")); err == nil {
		w.peerKey = k
	}
	if b, err := os.ReadFile(w.path("watchdog.json")); err == nil {
		var f WatchdogFile
		if json.Unmarshal(b, &f) == nil && f.Config.Enabled {
			w.cfg = f.Config
			w.state = f.State
		}
	}
	return nil
}

// CSR generates (or loads) watchdog.key and returns a DER-encoded CSR for the peer certificate.
func (w *AgentWatchdog) CSR() ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.selfID == "" {
		if cert, err := readCert(w.path("agent.crt")); err == nil {
			w.selfID = cert.Subject.CommonName
		}
	}
	if w.peerKey == nil {
		k, err := readKey(w.path("watchdog.key"))
		if err != nil {
			k, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				return nil, err
			}
			if err := writeKey(w.path("watchdog.key"), k); err != nil {
				return nil, err
			}
		}
		w.peerKey = k
	}
	return x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: w.selfID},
		DNSNames: []string{w.selfID},
	}, w.peerKey)
}

// ApplyConfig stores or removes the pushed watchdog config.
func (w *AgentWatchdog) ApplyConfig(cfg WatchdogConfig) error {
	w.mu.Lock()
	if !cfg.Enabled {
		w.cfg = WatchdogConfig{}
		w.state = WatchdogState{HubReachable: w.hubConnected}
		w.peers = map[string]peerSample{}
		p := w.path("watchdog.json")
		w.mu.Unlock()
		err := os.Remove(p)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if cfg.ConfigVersion == "" {
		cfg.ConfigVersion = cfg.Hash()
	}
	w.cfg = cfg
	err := w.saveLocked()
	w.mu.Unlock()
	w.trigger()
	return err
}

// SetHubReachable updates whether the agent's tunnel session to the hub is up.
func (w *AgentWatchdog) SetHubReachable(up bool) {
	now := w.now()
	w.mu.Lock()
	changed := w.hubConnected != up || !w.everConn
	w.hubConnected = up
	w.state.HubReachable = up
	if up {
		if !w.everConn || w.hubBackSince.IsZero() {
			w.hubBackSince = now
		}
		w.everConn = true
		w.state.HubDownSince = time.Time{}
	} else {
		w.hubBackSince = time.Time{}
		if w.state.HubDownSince.IsZero() {
			w.state.HubDownSince = now
		}
	}
	if w.cfg.Enabled {
		_ = w.saveLocked()
	}
	w.mu.Unlock()
	if changed {
		w.trigger()
	}
}

func (w *AgentWatchdog) trigger() {
	w.mu.Lock()
	ch := w.wake
	w.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (w *AgentWatchdog) saveLocked() error {
	if !w.cfg.Enabled {
		return nil
	}
	b, err := json.Marshal(WatchdogFile{Config: w.cfg, State: w.state})
	if err != nil {
		return err
	}
	tmp := w.path("watchdog.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, w.path("watchdog.json"))
}

// Status returns this agent's current view for GET /watchdog/status.
func (w *AgentWatchdog) Status() WatchdogStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return WatchdogStatus{
		ID:            w.selfID,
		HubReachable:  w.hubConnected,
		HubDownSince:  w.state.HubDownSince,
		ReportedDown:  w.state.ReportedDown,
		RecoverySent:  w.state.RecoverySent,
		ConfigVersion: w.cfg.ConfigVersion,
	}
}

// SendTest delivers a test notification directly from this agent using the provided channel.
func (w *AgentWatchdog) SendTest(ctx context.Context, hubName, publicURL, kind string, cfg notify.Config) error {
	if hubName == "" {
		hubName = "Moorage"
	}
	w.mu.Lock()
	id := w.selfID
	for _, p := range w.cfg.Peers {
		if p.ID == w.selfID && p.Name != "" {
			id = p.Name
		}
	}
	w.mu.Unlock()
	msg := notify.Message{
		Status: "test",
		Event:  notify.EventTest,
		Host:   hubName,
		Title:  "Moorage hub watchdog test",
		Text:   fmt.Sprintf("Sent directly by agent %s watching hub %s. If the hub goes down, this agent can reach you here.", id, hubName),
		URL:    publicURL,
	}
	return w.send(ctx, kind, cfg, msg)
}

// ListenAddr returns the bound peer listener address (useful when Listen is "127.0.0.1:0" in tests).
func (w *AgentWatchdog) ListenAddr() string {
	w.lnMu.Lock()
	defer w.lnMu.Unlock()
	if w.ln == nil {
		return ""
	}
	return w.ln.Addr().String()
}

// StartListener binds the peer mTLS listener if Listen is set and not already bound.
func (w *AgentWatchdog) StartListener(ctx context.Context) error {
	if w.Listen == "" {
		return nil
	}
	w.lnMu.Lock()
	defer w.lnMu.Unlock()
	if w.ln != nil {
		return nil
	}
	ln, err := net.Listen("tcp", w.Listen)
	if err != nil {
		return fmt.Errorf("watchdog listen %s: %w", w.Listen, err)
	}
	w.ln = ln
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go w.servePeers(ctx, ln)
	return nil
}

// Run starts the peer mTLS listener (if Listen != "") and the periodic coordination loop.
func (w *AgentWatchdog) Run(ctx context.Context) error {
	if err := w.Init(); err != nil {
		return err
	}
	if err := w.StartListener(ctx); err != nil {
		return err
	}

	poll := w.PollInterval
	if poll <= 0 {
		poll = 15 * time.Second
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.wake:
			w.step(ctx)
		case <-t.C:
			w.step(ctx)
		}
	}
}

func (w *AgentWatchdog) step(ctx context.Context) {
	w.mu.Lock()
	cfg := w.cfg
	selfID := w.selfID
	everConn := w.everConn
	hasOngoing := !w.state.HubDownSince.IsZero() || !w.state.ReportedDown.IsZero() || !w.state.ObservedDown.IsZero()
	w.mu.Unlock()
	if !cfg.Enabled || selfID == "" || (!everConn && !hasOngoing) {
		return
	}

	// Poll all other participating peers in parallel.
	w.pollPeers(ctx, cfg, selfID)

	now := w.now()
	w.mu.Lock()
	var readings []PeerReading
	for _, pr := range w.peers {
		if !w.hubBackSince.IsZero() && pr.at.Before(w.hubBackSince) {
			continue
		}
		readings = append(readings, PeerReading{Status: pr.status, Age: max(now.Sub(pr.at), 0)})
	}
	state := w.state
	state.HubReachable = w.hubConnected
	state.HubBackSince = w.hubBackSince
	dec := DecideWatchdog(selfID, state, readings, w.cfg, w.MaxStatusAge, now)
	// Persist state right away if no message needs sending; if a message needs sending,
	// send first and persist ReportedDown once sent so a transient send failure retries on the next tick.
	if dec.Kind == DecisionNone {
		if dec.Next != w.state {
			w.state = dec.Next
			_ = w.saveLocked()
		}
		w.mu.Unlock()
		return
	}
	chKind, chCfg := w.cfg.ChannelKind, w.cfg.ChannelConfig
	w.mu.Unlock()

	if err := w.send(ctx, chKind, chCfg, dec.Message); err != nil {
		if w.Log != nil {
			w.Log.Warn("watchdog notification failed", "err", err)
		}
		return
	}
	w.mu.Lock()
	w.state = dec.Next
	_ = w.saveLocked()
	w.mu.Unlock()
	if w.Log != nil {
		w.Log.Info("watchdog notification sent", "kind", dec.Message.Status, "title", dec.Message.Title)
	}
}

func (w *AgentWatchdog) pollPeers(ctx context.Context, cfg WatchdogConfig, selfID string) {
	tlsCfg, ok := w.peerClientTLS()
	if !ok {
		return
	}
	timeout := w.PollTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	var wg sync.WaitGroup
	for _, p := range cfg.Peers {
		if p.ID == selfID || p.Addr == "" {
			continue
		}
		wg.Add(1)
		go func(peer WatchdogPeer) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			ctls := tlsCfg.Clone()
			ctls.ServerName = peer.ID
			tr := &http.Transport{TLSClientConfig: ctls, DisableKeepAlives: true}
			defer tr.CloseIdleConnections()
			cl := &http.Client{Transport: tr, Timeout: timeout}
			req, err := http.NewRequestWithContext(pctx, http.MethodGet, "https://"+peer.Addr+"/watchdog/status", nil)
			if err != nil {
				return
			}
			resp, err := cl.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return
			}
			var st WatchdogStatus
			if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&st); err != nil || st.ID != peer.ID {
				return
			}
			now := w.now()
			w.mu.Lock()
			w.peers[peer.ID] = peerSample{status: st, at: now}
			w.mu.Unlock()
		}(p)
	}
	wg.Wait()
}

func (w *AgentWatchdog) peerClientTLS() (*tls.Config, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ca == nil || w.peerKey == nil || len(w.cfg.PeerCert) == 0 {
		return nil, false
	}
	pool := x509.NewCertPool()
	pool.AddCert(w.ca)
	cert := tls.Certificate{Certificate: [][]byte{w.cfg.PeerCert}, PrivateKey: w.peerKey}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
	}, true
}

func (w *AgentWatchdog) servePeers(ctx context.Context, ln net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /watchdog/status", func(rw http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			http.Error(rw, "client certificate required", http.StatusUnauthorized)
			return
		}
		cn := r.TLS.VerifiedChains[0][0].Subject.CommonName
		if !w.isAllowedPeer(cn) {
			http.Error(rw, "forbidden", http.StatusForbidden)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(w.Status()) //nolint:errcheck
	})

	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAndVerifyClientCert,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			w.mu.Lock()
			ca, key, raw := w.ca, w.peerKey, w.cfg.PeerCert
			w.mu.Unlock()
			if ca == nil || key == nil || len(raw) == 0 {
				return nil, errors.New("watchdog peer TLS not configured")
			}
			pool := x509.NewCertPool()
			pool.AddCert(ca)
			return &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{{Certificate: [][]byte{raw}, PrivateKey: key}},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    pool,
				VerifyPeerCertificate: func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
					if len(verifiedChains) == 0 || len(verifiedChains[0]) == 0 {
						return errors.New("no verified chain")
					}
					cn := verifiedChains[0][0].Subject.CommonName
					if !w.isAllowedPeer(cn) {
						return fmt.Errorf("peer %s is not in the current watchdog peer list", cn)
					}
					return nil
				},
			}, nil
		},
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         tlsCfg,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	if w.Log != nil {
		srv.ErrorLog = slog.NewLogLogger(w.Log.Handler(), slog.LevelDebug)
	}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	_ = srv.Serve(tls.NewListener(ln, tlsCfg))
}

func (w *AgentWatchdog) isAllowedPeer(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.cfg.Enabled || id == "" {
		return false
	}
	for _, p := range w.cfg.Peers {
		if p.ID == id {
			return true
		}
	}
	return false
}

// WrapHandler attaches the agent's tunnel watchdog endpoints (/watchdog, /watchdog/csr, /watchdog/test)
// onto an existing AgentHandler and injects WatchdogAddr into GET /info responses.
func (w *AgentWatchdog) WrapHandler(next http.Handler, watchdogAddr string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /watchdog/csr", func(rw http.ResponseWriter, r *http.Request) {
		csr, err := w.CSR()
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(map[string][]byte{"csr": csr}) //nolint:errcheck
	})
	mux.HandleFunc("PUT /watchdog", func(rw http.ResponseWriter, r *http.Request) {
		var cfg WatchdogConfig
		if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&cfg); err != nil {
			http.Error(rw, "bad request", http.StatusBadRequest)
			return
		}
		if err := w.ApplyConfig(cfg); err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /watchdog/test", func(rw http.ResponseWriter, r *http.Request) {
		var req struct {
			HubName       string        `json:"hubName"`
			PublicURL     string        `json:"publicUrl"`
			ChannelKind   string        `json:"channelKind"`
			ChannelConfig notify.Config `json:"channelConfig"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
			http.Error(rw, "bad request", http.StatusBadRequest)
			return
		}
		if err := w.SendTest(r.Context(), req.HubName, req.PublicURL, req.ChannelKind, req.ChannelConfig); err != nil {
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(rw).Encode(map[string]string{"code": "internal", "error": err.Error()}) //nolint:errcheck
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /info", func(rw http.ResponseWriter, r *http.Request) {
		rec := &infoRecorder{ResponseWriter: rw, hdr: http.Header{}}
		next.ServeHTTP(rec, r)
		var m map[string]any
		if json.Unmarshal(rec.buf.Bytes(), &m) == nil {
			m["watchdog"] = true
			if watchdogAddr != "" {
				m["watchdogAddr"] = watchdogAddr
			}
			for k, vs := range rec.hdr {
				rw.Header()[k] = vs
			}
			rw.WriteHeader(http.StatusOK)
			json.NewEncoder(rw).Encode(m) //nolint:errcheck
			return
		}
		for k, vs := range rec.hdr {
			rw.Header()[k] = vs
		}
		if rec.code != 0 {
			rw.WriteHeader(rec.code)
		}
		rw.Write(rec.buf.Bytes()) //nolint:errcheck
	})
	mux.Handle("/", next)
	return mux
}

type infoRecorder struct {
	http.ResponseWriter
	hdr  http.Header
	buf  bytes.Buffer
	code int
}

func (r *infoRecorder) Header() http.Header         { return r.hdr }
func (r *infoRecorder) WriteHeader(code int)        { r.code = code }
func (r *infoRecorder) Write(b []byte) (int, error) { return r.buf.Write(b) }
