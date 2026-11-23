package tunnel

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"moorage/internal/host"
	"moorage/internal/notify"
	"moorage/internal/store"
)

func TestDecideWatchdogPureScenarios(t *testing.T) {
	t0 := time.Date(2026, 11, 20, 17, 5, 0, 0, time.UTC)
	maxAge := 45 * time.Second

	cfg1 := WatchdogConfig{
		Enabled: true, Grace: 120, HubName: "home", PublicURL: "https://moorage.example",
		Peers: []WatchdogPeer{{ID: "ag_1", Name: "vps"}},
	}
	cfg2 := WatchdogConfig{
		Enabled: true, Grace: 120, HubName: "home",
		Peers: []WatchdogPeer{
			{ID: "ag_1", Name: "vps", Addr: "10.0.0.1:5082"},
			{ID: "ag_2", Name: "pi", Addr: "10.0.0.2:5082"},
		},
	}
	cfg3 := WatchdogConfig{
		Enabled: true, Grace: 120, HubName: "home", PublicURL: "https://moorage.example",
		Peers: []WatchdogPeer{
			{ID: "ag_1", Name: "vps", Addr: "10.0.0.1:5082"},
			{ID: "ag_2", Name: "pi1", Addr: "10.0.0.2:5082"},
			{ID: "ag_3", Name: "pi2", Addr: "10.0.0.3:5082"},
		},
	}

	// 1. N=1, hub lost > grace -> exactly one down notice; hub back -> exactly one recovery.
	t.Run("1_single_agent_down_and_recovery", func(t *testing.T) {
		st := WatchdogState{HubReachable: false, HubDownSince: t0}
		dec := DecideWatchdog("ag_1", st, nil, cfg1, maxAge, t0.Add(3*time.Minute))
		if dec.Kind != DecisionDown || !strings.Contains(dec.Message.Title, "Moorage hub home is unreachable") {
			t.Fatalf("expected down decision, got %+v", dec)
		}
		st = dec.Next
		// Second evaluation while still down -> nothing
		dec2 := DecideWatchdog("ag_1", st, nil, cfg1, maxAge, t0.Add(4*time.Minute))
		if dec2.Kind != DecisionNone {
			t.Fatalf("expected no duplicate down notice, got %+v", dec2)
		}
		// Hub back -> exactly one recovery
		st = dec2.Next
		st.HubReachable = true
		rec := DecideWatchdog("ag_1", st, nil, cfg1, maxAge, t0.Add(12*time.Minute))
		if rec.Kind != DecisionRecovery || !strings.Contains(rec.Message.Text, "is back after 12 min") {
			t.Fatalf("expected recovery notice, got %+v", rec)
		}
		// Next tick while still up -> nothing
		rec2 := DecideWatchdog("ag_1", rec.Next, nil, cfg1, maxAge, t0.Add(13*time.Minute))
		if rec2.Kind != DecisionNone {
			t.Fatalf("expected no second recovery notice, got %+v", rec2)
		}
	})

	// 2. N=3, all three lose the hub -> only the lowest ID sends; the other two send nothing.
	t.Run("2_three_agents_only_lowest_id_sends", func(t *testing.T) {
		now := t0.Add(3 * time.Minute)
		p1 := PeerReading{Status: WatchdogStatus{ID: "ag_1", HubReachable: false, HubDownSince: t0}, Age: 2 * time.Second}
		p2 := PeerReading{Status: WatchdogStatus{ID: "ag_2", HubReachable: false, HubDownSince: t0}, Age: 2 * time.Second}
		p3 := PeerReading{Status: WatchdogStatus{ID: "ag_3", HubReachable: false, HubDownSince: t0}, Age: 2 * time.Second}

		d1 := DecideWatchdog("ag_1", WatchdogState{HubReachable: false, HubDownSince: t0}, []PeerReading{p2, p3}, cfg3, maxAge, now)
		d2 := DecideWatchdog("ag_2", WatchdogState{HubReachable: false, HubDownSince: t0}, []PeerReading{p1, p3}, cfg3, maxAge, now)
		d3 := DecideWatchdog("ag_3", WatchdogState{HubReachable: false, HubDownSince: t0}, []PeerReading{p1, p2}, cfg3, maxAge, now)

		if d1.Kind != DecisionDown || d2.Kind != DecisionNone || d3.Kind != DecisionNone {
			t.Fatalf("decisions: d1=%v d2=%v d3=%v", d1.Kind, d2.Kind, d3.Kind)
		}
		if !strings.Contains(d1.Message.Text, "seen by 3 of 3 agents: vps, pi1, pi2") {
			t.Fatalf("message text: %q", d1.Message.Text)
		}
	})

	// 3. N=3, only one agent loses the hub (its own network) -> no hub notice from agents.
	t.Run("3_one_of_three_loses_hub_no_quorum", func(t *testing.T) {
		now := t0.Add(3 * time.Minute)
		p2 := PeerReading{Status: WatchdogStatus{ID: "ag_2", HubReachable: true}, Age: 2 * time.Second}
		p3 := PeerReading{Status: WatchdogStatus{ID: "ag_3", HubReachable: true}, Age: 2 * time.Second}
		d1 := DecideWatchdog("ag_1", WatchdogState{HubReachable: false, HubDownSince: t0}, []PeerReading{p2, p3}, cfg3, maxAge, now)
		if d1.Kind != DecisionNone {
			t.Fatalf("1 of 3 must not report hub outage, got %+v", d1)
		}
	})

	// 4. N=3, lowest-ID agent is itself down -> the next lowest that sees the hub down sends.
	t.Run("4_lowest_id_down_next_lowest_sends", func(t *testing.T) {
		now := t0.Add(3 * time.Minute)
		// ag_1 does not answer (absent from peer readings); ag_2 and ag_3 see the hub down.
		p2 := PeerReading{Status: WatchdogStatus{ID: "ag_2", HubReachable: false, HubDownSince: t0}, Age: 2 * time.Second}
		p3 := PeerReading{Status: WatchdogStatus{ID: "ag_3", HubReachable: false, HubDownSince: t0}, Age: 2 * time.Second}

		d2 := DecideWatchdog("ag_2", WatchdogState{HubReachable: false, HubDownSince: t0}, []PeerReading{p3}, cfg3, maxAge, now)
		d3 := DecideWatchdog("ag_3", WatchdogState{HubReachable: false, HubDownSince: t0}, []PeerReading{p2}, cfg3, maxAge, now)
		if d2.Kind != DecisionDown || d3.Kind != DecisionNone {
			t.Fatalf("want ag_2 to send and ag_3 silent: d2=%v d3=%v", d2.Kind, d3.Kind)
		}
		if !strings.Contains(d2.Message.Text, "seen by 2 of 3 agents: pi1, pi2") {
			t.Fatalf("message text: %q", d2.Message.Text)
		}
	})

	// 5. Hub drops for less than grace (restart) -> nothing, and no false recovery when it reconnects.
	t.Run("5_below_grace_sends_nothing", func(t *testing.T) {
		st := WatchdogState{HubReachable: false, HubDownSince: t0}
		d := DecideWatchdog("ag_1", st, nil, cfg1, maxAge, t0.Add(90*time.Second))
		if d.Kind != DecisionNone {
			t.Fatalf("below grace must not alert: %+v", d)
		}
		back := d.Next
		back.HubReachable = true
		dBack := DecideWatchdog("ag_1", back, nil, cfg1, maxAge, t0.Add(100*time.Second))
		if dBack.Kind != DecisionNone {
			t.Fatalf("reconnect after sub-grace drop must not send recovery: %+v", dBack)
		}
	})

	// 6. Reporter restarts during the outage (persisted state reloaded) -> no second down notice; recovery is still sent once.
	// Also verifies an agent restarting while hub is healthy does not send a false "hub is back".
	t.Run("6_reporter_restarts_during_outage_and_healthy_restart", func(t *testing.T) {
		now := t0.Add(3 * time.Minute)
		d1 := DecideWatchdog("ag_1", WatchdogState{HubReachable: false, HubDownSince: t0}, nil, cfg1, maxAge, now)
		if d1.Kind != DecisionDown {
			t.Fatalf("initial down failed: %+v", d1)
		}
		// Simulate agent restart: persisted state (HubDownSince, ReportedDown, ObservedDown) is reloaded from watchdog.json.
		reloaded := WatchdogState{
			HubReachable: false,
			HubDownSince: d1.Next.HubDownSince,
			ReportedDown: d1.Next.ReportedDown,
			ObservedDown: d1.Next.ObservedDown,
		}
		dAfterRestart := DecideWatchdog("ag_1", reloaded, nil, cfg1, maxAge, t0.Add(5*time.Minute))
		if dAfterRestart.Kind != DecisionNone {
			t.Fatalf("restarted reporter sent duplicate down notice: %+v", dAfterRestart)
		}
		// Hub recovers -> recovery sent once
		recovered := dAfterRestart.Next
		recovered.HubReachable = true
		dRec := DecideWatchdog("ag_1", recovered, nil, cfg1, maxAge, t0.Add(10*time.Minute))
		if dRec.Kind != DecisionRecovery {
			t.Fatalf("restarted reporter failed to send recovery: %+v", dRec)
		}
		// Agent restarts again while hub is healthy -> no false "hub is back"
		healthyReloaded := WatchdogState{
			HubReachable: true,
			HubDownSince: dRec.Next.HubDownSince,
			ReportedDown: dRec.Next.ReportedDown,
			ObservedDown: dRec.Next.ObservedDown,
		}
		dHealthy := DecideWatchdog("ag_1", healthyReloaded, nil, cfg1, maxAge, t0.Add(11*time.Minute))
		if dHealthy.Kind != DecisionNone {
			t.Fatalf("healthy agent restart sent false recovery: %+v", dHealthy)
		}
	})

	// 7. Lowest-ID agent comes back late during an outage that another agent already reported -> it does not send again.
	// And if the original reporter (ag_2) is unreachable when the hub comes back, the lowest alive peer (ag_1) sends recovery.
	t.Run("7_lowest_id_comes_back_late_and_fallback_recovery", func(t *testing.T) {
		now := t0.Add(3 * time.Minute)
		// ag_2 already reported the outage at t0.
		p2 := PeerReading{
			Status: WatchdogStatus{ID: "ag_2", HubReachable: false, HubDownSince: t0, ReportedDown: OutageID(t0)},
			Age:    3 * time.Second,
		}
		p3 := PeerReading{
			Status: WatchdogStatus{ID: "ag_3", HubReachable: false, HubDownSince: t0},
			Age:    3 * time.Second,
		}
		d1 := DecideWatchdog("ag_1", WatchdogState{HubReachable: false, HubDownSince: t0.Add(30 * time.Second)}, []PeerReading{p2, p3}, cfg3, maxAge, now)
		if d1.Kind != DecisionNone {
			t.Fatalf("late lowest-ID agent must not send a second down notice, got %+v", d1)
		}
		if d1.Next.ObservedDown.IsZero() {
			t.Fatal("late agent should record ObservedDown from peer's ReportedDown")
		}
		// Now the hub comes back, but ag_2 (the original reporter) is unreachable!
		// Between ag_1 and ag_3, ag_1 has the lowest ID and must send the recovery notice.
		st1 := d1.Next
		st1.HubReachable = true
		p3Up := PeerReading{Status: WatchdogStatus{ID: "ag_3", HubReachable: true}, Age: 2 * time.Second}
		p1Up := PeerReading{Status: WatchdogStatus{ID: "ag_1", HubReachable: true}, Age: 2 * time.Second}

		rec1 := DecideWatchdog("ag_1", st1, []PeerReading{p3Up}, cfg3, maxAge, t0.Add(10*time.Minute))
		st3 := WatchdogState{HubReachable: true, ObservedDown: OutageID(t0), ObservedBy: "ag_2"}
		rec3 := DecideWatchdog("ag_3", st3, []PeerReading{p1Up}, cfg3, maxAge, t0.Add(10*time.Minute))
		if rec1.Kind != DecisionRecovery || rec3.Kind != DecisionNone {
			t.Fatalf("fallback recovery when original reporter is dead: rec1=%v rec3=%v", rec1.Kind, rec3.Kind)
		}
	})

	// 8. Maintenance active -> nothing, also not after maintenance ends for an outage that started during it.
	t.Run("8_maintenance_suppresses_during_and_after", func(t *testing.T) {
		maintCfg := cfg1
		maintCfg.MaintenanceUntil = t0.Add(10 * time.Minute)
		st := WatchdogState{HubReachable: false, HubDownSince: t0}

		// During maintenance (t0 + 3 min)
		dDuring := DecideWatchdog("ag_1", st, nil, maintCfg, maxAge, t0.Add(3*time.Minute))
		if dDuring.Kind != DecisionNone {
			t.Fatalf("sent notice during maintenance: %+v", dDuring)
		}
		// After maintenance ends (t0 + 15 min), for the same outage that started at t0 (during maintenance)
		dAfter := DecideWatchdog("ag_1", dDuring.Next, nil, maintCfg, maxAge, t0.Add(15*time.Minute))
		if dAfter.Kind != DecisionNone {
			t.Fatalf("outage starting during maintenance must never be reported after maintenance ends: %+v", dAfter)
		}
	})

	// 9. Stale peer status (> 45 s) is ignored.
	t.Run("9_stale_peer_status_ignored", func(t *testing.T) {
		now := t0.Add(3 * time.Minute)
		stale2 := PeerReading{Status: WatchdogStatus{ID: "ag_2", HubReachable: false, HubDownSince: t0}, Age: 46 * time.Second}
		stale3 := PeerReading{Status: WatchdogStatus{ID: "ag_3", HubReachable: false, HubDownSince: t0}, Age: 60 * time.Second}
		d1 := DecideWatchdog("ag_1", WatchdogState{HubReachable: false, HubDownSince: t0}, []PeerReading{stale2, stale3}, cfg3, maxAge, now)
		if d1.Kind != DecisionNone {
			t.Fatalf("stale peer votes should be ignored (no quorum), got %+v", d1)
		}
	})

	// 10. N=2 with one agent unreachable -> no quorum, no notice (intended: quorum requires > N/2 = 2 votes).
	t.Run("10_two_agents_one_unreachable_no_quorum", func(t *testing.T) {
		now := t0.Add(3 * time.Minute)
		d1 := DecideWatchdog("ag_1", WatchdogState{HubReachable: false, HubDownSince: t0}, nil, cfg2, maxAge, now)
		if d1.Kind != DecisionNone {
			t.Fatalf("N=2 with 1 agent unreachable must not reach quorum, got %+v", d1)
		}
		// When both answer and see the hub down -> ag_1 reports
		p2 := PeerReading{Status: WatchdogStatus{ID: "ag_2", HubReachable: false, HubDownSince: t0}, Age: 2 * time.Second}
		dBoth := DecideWatchdog("ag_1", WatchdogState{HubReachable: false, HubDownSince: t0}, []PeerReading{p2}, cfg2, maxAge, now)
		if dBoth.Kind != DecisionDown {
			t.Fatalf("N=2 with both down should report, got %+v", dBoth)
		}
	})
}

func TestWatchdogConfigFilePermissionsAndCleanup(t *testing.T) {
	dir := t.TempDir()
	w := &AgentWatchdog{Dir: dir}
	if err := w.Init(); err != nil {
		t.Fatal(err)
	}
	cfg := WatchdogConfig{
		Enabled:     true,
		Grace:       120,
		HubName:     "hub",
		ChannelKind: "ntfy",
		ChannelConfig: notify.Config{
			URL:   "https://ntfy.sh/test",
			Token: "secret-token",
		},
		Peers: []WatchdogPeer{{ID: "ag_1", Name: "vps"}},
	}
	if err := w.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "watchdog.json")
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("watchdog.json not created: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("watchdog.json mode = %04o, want 0600", perm)
	}
	// Turning participation off must delete the file (and its copied channel secret).
	if err := w.ApplyConfig(WatchdogConfig{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("watchdog.json still exists after disabling: %v", err)
	}
}

func TestWatchdogPeerMTLSRejectsForeignCAAndRemovedAgent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	otherCA, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Set up agent 1 running a peer mTLS listener.
	dir1 := t.TempDir()
	writePEM(filepath.Join(dir1, "ca.crt"), "CERTIFICATE", ca.Cert.Raw, 0o644)
	k1, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csr1, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "ag_1"}}, k1)
	agCert1, _ := ca.SignAgent(csr1, "ag_1")
	writePEM(filepath.Join(dir1, "agent.crt"), "CERTIFICATE", agCert1, 0o644)

	w1 := &AgentWatchdog{Dir: dir1, Listen: "127.0.0.1:0"}
	if err := w1.Init(); err != nil {
		t.Fatal(err)
	}
	pcsr1, err := w1.CSR()
	if err != nil {
		t.Fatal(err)
	}
	pcert1, err := ca.SignPeer(pcsr1, "ag_1")
	if err != nil {
		t.Fatal(err)
	}
	if err := w1.StartListener(ctx); err != nil {
		t.Fatal(err)
	}
	addr1 := w1.ListenAddr()

	// Allow ag_1 and ag_2 initially.
	err = w1.ApplyConfig(WatchdogConfig{
		Enabled:  true,
		Grace:    120,
		PeerCert: pcert1,
		Peers: []WatchdogPeer{
			{ID: "ag_1", Name: "one", Addr: addr1},
			{ID: "ag_2", Name: "two"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	makeClient := func(signer *CA, id string) *http.Client {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		csr, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: id}, DNSNames: []string{id}}, k)
		der, _ := signer.SignPeer(csr, id)
		pool := x509.NewCertPool()
		pool.AddCert(ca.Cert)
		return &http.Client{
			Timeout: 3 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					MinVersion:   tls.VersionTLS13,
					Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: k}},
					RootCAs:      pool,
					ServerName:   "ag_1",
				},
			},
		}
	}

	// 1. Valid peer ag_2 signed by hub CA -> 200 OK
	c2 := makeClient(ca, "ag_2")
	resp, err := c2.Get("https://" + addr1 + "/watchdog/status")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("valid peer ag_2 rejected: resp=%v err=%v", resp, err)
	}
	resp.Body.Close()

	// 2. Cert signed by another CA -> TLS handshake refused
	cForeign := makeClient(otherCA, "ag_2")
	if resp, err := cForeign.Get("https://" + addr1 + "/watchdog/status"); err == nil {
		resp.Body.Close()
		t.Fatal("peer with cert from foreign CA was accepted")
	}

	// 3. Remove ag_2 from peer list via config push -> ag_2's cert is now refused
	err = w1.ApplyConfig(WatchdogConfig{
		Enabled:  true,
		Grace:    120,
		PeerCert: pcert1,
		Peers:    []WatchdogPeer{{ID: "ag_1", Name: "one", Addr: addr1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := c2.Get("https://" + addr1 + "/watchdog/status"); err == nil && resp.StatusCode == http.StatusOK {
		resp.Body.Close()
		t.Fatal("removed agent ag_2 was still accepted after config push")
	}
}

func TestWatchdogIntegrationThreeAgentsHubOutageAndRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hubDir := t.TempDir()
	st, err := store.Open(hubDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ca, err := LoadOrCreateCA(filepath.Join(hubDir, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := host.NewRegistry(nil, "", false, st)

	chID, err := st.SaveChannel(ctx, store.Channel{
		Name: "ntfy", Kind: "ntfy", Config: `{"url":"https://ntfy.sh/moorage-test","token":"sec"}`, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	hubAddr := freeAddr(t)
	hubCtx, stopHub := context.WithCancel(ctx)
	hub := &Hub{CA: ca, St: st, Reg: reg, Log: log, HubName: "alpha", PublicURL: "https://moorage.example"}
	hubDone := make(chan struct{})
	go func() {
		defer close(hubDone)
		_ = hub.Serve(hubCtx, hubAddr)
	}()
	time.Sleep(80 * time.Millisecond)

	var (
		mu   sync.Mutex
		sent []notify.Message
	)
	recordSend := func(_ context.Context, _ string, _ notify.Config, m notify.Message) error {
		mu.Lock()
		sent = append(sent, m)
		mu.Unlock()
		return nil
	}

	// Pair and start 3 agents with fast watchdog timings.
	const numAgents = 3
	var agentsWG sync.WaitGroup
	for i := range numAgents {
		tok, _, err := hub.NewJoinToken(ctx, "node-"+string(rune('a'+i)))
		if err != nil {
			t.Fatal(err)
		}
		peerAddr := freeAddr(t)
		agDir := t.TempDir()
		wd := &AgentWatchdog{
			Dir:          agDir,
			Listen:       peerAddr,
			Log:          log,
			PollInterval: 60 * time.Millisecond,
			PollTimeout:  500 * time.Millisecond,
			MaxStatusAge: 5 * time.Second,
			Send:         recordSend,
		}
		ag := &Agent{
			Hub:            hubAddr,
			JoinToken:      tok,
			Dir:            agDir,
			Handler:        host.AgentHandler(fakeHost{}, true, "test"),
			Log:            log,
			WatchdogListen: peerAddr,
			WatchdogAddr:   peerAddr,
			Watchdog:       wd,
		}
		agentsWG.Add(1)
		go func() {
			defer agentsWG.Done()
			_ = ag.Run(ctx)
		}()
	}
	// Agents write state files into their TempDirs until they stop. Cleanups run last
	// registered first, so this stops them before the TempDirs are removed (the test
	// flaked with "directory not empty" otherwise).
	t.Cleanup(func() {
		cancel()
		agentsWG.Wait()
	})

	// Wait for all 3 agents to connect.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		list, _ := reg.List(ctx)
		connCount := 0
		for _, e := range list {
			if e.Connected {
				connCount++
			}
		}
		if connCount == numAgents {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	agents, err := st.Agents(ctx)
	if err != nil || len(agents) != numAgents {
		t.Fatalf("paired agents: %d %v", len(agents), err)
	}

	// Enable watchdog for all 3 agents with a 1-second grace period and push config.
	part := map[string]bool{}
	for _, a := range agents {
		part[a.ID] = true
	}
	if err := st.SaveWatchdog(ctx, store.WatchdogSettings{Enabled: true, ChannelID: chID, Grace: 1}, part); err != nil {
		t.Fatal(err)
	}
	hub.PushWatchdog(ctx)

	// Verify test notice via agent works.
	ch, _ := st.ChannelByID(ctx, chID)
	if _, err := hub.TestWatchdog(ctx, ch); err != nil {
		t.Fatalf("TestWatchdog failed: %v", err)
	}
	mu.Lock()
	if len(sent) != 1 || sent[0].Status != "test" {
		mu.Unlock()
		t.Fatalf("expected 1 test message, got %+v", sent)
	}
	sent = nil
	mu.Unlock()

	// Stop the hub -> after 1 s grace, the 3 agents coordinate and send exactly 1 down notice.
	stopHub()
	<-hubDone

	deadline = time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(sent)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(60 * time.Millisecond)
	}
	// Let several more poll cycles run to verify no second down message is ever sent.
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	if len(sent) != 1 || sent[0].Status != "down" || !strings.Contains(sent[0].Title, "Moorage hub alpha is unreachable") {
		msgs := append([]notify.Message(nil), sent...)
		mu.Unlock()
		t.Fatalf("expected exactly 1 down message, got %d: %+v", len(msgs), msgs)
	}
	mu.Unlock()

	// Start the hub again on the same address, CA, and DB -> agents reconnect and send exactly 1 recovery message.
	hubCtx2, stopHub2 := context.WithCancel(ctx)
	defer stopHub2()
	hub2 := &Hub{CA: ca, St: st, Reg: reg, Log: log, HubName: "alpha", PublicURL: "https://moorage.example"}
	go hub2.Serve(hubCtx2, hubAddr) //nolint:errcheck

	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(sent)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(60 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 || sent[1].Status != "up" || !strings.Contains(sent[1].Title, "Moorage hub alpha is back after") {
		t.Fatalf("expected exactly 1 recovery message after down, got %d messages: %+v", len(sent), sent)
	}
}

// Found live: the reporter (ag_1) was down while the hub came back, ag_2 sent the
// recovery instead; when ag_1 returned with its persisted ReportedDown it sent a
// second "hub is back". The peers' RecoverySent must stop it.
func TestDecideWatchdogReturningReporterDoesntRepeatRecovery(t *testing.T) {
	t0 := time.Date(2026, 11, 20, 17, 5, 0, 0, time.UTC)
	cfg := WatchdogConfig{Enabled: true, Grace: 60, Peers: []WatchdogPeer{
		{ID: "ag_1", Addr: "a:1"}, {ID: "ag_2", Addr: "b:1"}, {ID: "ag_3", Addr: "c:1"},
	}}
	outage := OutageID(t0)

	// ag_2 observed ag_1's report; hub is back and ag_1 doesn't answer -> ag_2 sends
	st2 := WatchdogState{HubReachable: true, ObservedDown: outage, ObservedBy: "ag_1"}
	peers := []PeerReading{{Status: WatchdogStatus{ID: "ag_3", HubReachable: true}}}
	dec := DecideWatchdog("ag_2", st2, peers, cfg, time.Minute, t0.Add(5*time.Minute))
	if dec.Kind != DecisionRecovery || !dec.Next.RecoverySent.Equal(outage) {
		t.Fatalf("ag_2 should send the recovery and remember it, got %+v", dec)
	}

	// ag_1 comes back later, still holding ReportedDown -> sees ag_2's RecoverySent, stays silent
	st1 := WatchdogState{HubReachable: true, ReportedDown: outage, ObservedDown: outage}
	peers = []PeerReading{
		{Status: WatchdogStatus{ID: "ag_2", HubReachable: true, RecoverySent: dec.Next.RecoverySent}},
		{Status: WatchdogStatus{ID: "ag_3", HubReachable: true}},
	}
	dec1 := DecideWatchdog("ag_1", st1, peers, cfg, time.Minute, t0.Add(6*time.Minute))
	if dec1.Kind != DecisionNone || !dec1.Next.ReportedDown.IsZero() {
		t.Fatalf("returning reporter must not repeat the recovery, got %+v", dec1)
	}

	// normal path is unchanged: reporter alive, nobody else sent -> reporter sends
	peers = []PeerReading{{Status: WatchdogStatus{ID: "ag_2", HubReachable: true}}}
	if d := DecideWatchdog("ag_1", st1, peers, cfg, time.Minute, t0.Add(6*time.Minute)); d.Kind != DecisionRecovery {
		t.Fatalf("reporter should send the recovery, got %+v", d)
	}
}
