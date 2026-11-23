package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/notify"
	"moorage/internal/store"
)

func TestDecideQuorum(t *testing.T) {
	cases := []struct {
		quorum   string
		up, down int
		want     string
	}{
		// majority = strictly more than half of the voters
		{QuorumMajority, 1, 0, StatusUp},
		{QuorumMajority, 0, 1, StatusDown},
		{QuorumMajority, 1, 1, StatusUp}, // 1 of 2 failing is no majority
		{QuorumMajority, 0, 2, StatusDown},
		{QuorumMajority, 2, 1, StatusUp},
		{QuorumMajority, 1, 2, StatusDown},
		{QuorumMajority, 2, 2, StatusUp}, // tie: 2 of 4 failing is not a majority
		{QuorumMajority, 1, 3, StatusDown},
		{"", 1, 2, StatusDown}, // empty = majority
		{QuorumAny, 3, 1, StatusDown},
		{QuorumAny, 3, 0, StatusUp},
		{QuorumAll, 1, 3, StatusUp},
		{QuorumAll, 0, 3, StatusDown},
		// abstentions don't vote: only the answering locations count
		{QuorumAll, 0, 1, StatusDown},
		// nobody answered
		{QuorumMajority, 0, 0, StatusUnknown},
		{QuorumAny, 0, 0, StatusUnknown},
		{QuorumAll, 0, 0, StatusUnknown},
	}
	for _, c := range cases {
		if got := Decide(c.quorum, c.up, c.down); got != c.want {
			t.Errorf("%s up=%d down=%d: got %s, want %s", c.quorum, c.up, c.down, got, c.want)
		}
	}
}

func TestMultiLocationOnlyForNetworkChecks(t *testing.T) {
	cases := []struct {
		c    store.Check
		want bool
	}{
		{store.Check{Kind: KindHTTP}, false},
		{store.Check{Kind: KindHTTP, Locations: []string{LocationHub}}, false},
		{store.Check{Kind: KindHTTP, Locations: []string{"ag1"}}, true},
		{store.Check{Kind: KindDNS, Locations: []string{LocationHub, "ag1"}}, true},
		{store.Check{Kind: KindStack, Locations: []string{LocationHub, "ag1"}}, false},
		{store.Check{Kind: KindLog, Locations: []string{"ag1"}}, false},
	}
	for _, c := range cases {
		if got := MultiLocation(c.c); got != c.want {
			t.Errorf("%s %v: got %v", c.c.Kind, c.c.Locations, got)
		}
	}
}

func TestAgentProberLimits(t *testing.T) {
	p := NewAgentProber()
	var got store.Check
	p.run = func(_ context.Context, c store.Check) Outcome {
		got = c
		return Outcome{OK: true, Latency: 12 * time.Millisecond, Msg: "HTTP 200"}
	}
	ctx := context.Background()
	for _, k := range []string{KindStack, KindLog, "exec"} {
		var ve engine.ValidationError
		if _, err := p.Probe(ctx, host.ProbeRequest{Kind: k, Target: "web"}); !errors.As(err, &ve) {
			t.Errorf("%s probe not refused: %v", k, err)
		}
	}
	res, err := p.Probe(ctx, host.ProbeRequest{Kind: KindHTTP, Target: "http://x", Timeout: 300, Settings: json.RawMessage(`{"keyword":"ok","bogus":1}`)})
	if err != nil || !res.OK || res.LatencyMS != 12 {
		t.Fatalf("probe: %+v %v", res, err)
	}
	if got.Timeout != MaxRemoteTimeout {
		t.Fatalf("timeout not capped: %d", got.Timeout)
	}
	if got.Settings != `{"keyword":"ok"}` {
		t.Fatalf("settings: %s", got.Settings)
	}
	if _, err := p.Probe(ctx, host.ProbeRequest{Kind: KindTCP, Target: "x:1", Timeout: 0}); err != nil || got.Timeout != 1 {
		t.Fatalf("zero timeout: %d %v", got.Timeout, err)
	}

	// The concurrency limit: with 8 probes running the 9th is busy at once.
	for range MaxRemoteProbes {
		p.sem <- struct{}{}
	}
	if _, err := p.Probe(ctx, host.ProbeRequest{Kind: KindTCP, Target: "x:1", Timeout: 5}); !errors.Is(err, engine.ErrBusy) {
		t.Fatalf("9th probe: %v", err)
	}
	<-p.sem
	if _, err := p.Probe(ctx, host.ProbeRequest{Kind: KindTCP, Target: "x:1", Timeout: 5}); err != nil {
		t.Fatalf("probe after a slot freed: %v", err)
	}

	// A system that can't send ping gives no vote, not a down.
	for len(p.sem) > 0 {
		<-p.sem
	}
	p.run = func(context.Context, store.Check) Outcome { return Outcome{Msg: errNoICMP.Error(), cantRun: true} }
	var ve engine.ValidationError
	if _, err := p.Probe(ctx, host.ProbeRequest{Kind: KindPing, Target: "1.1.1.1", Timeout: 5}); !errors.As(err, &ve) {
		t.Fatalf("no ICMP should be a refusal, got %v", err)
	}
}

// remoteTo builds a hub-side client for an agent handler.
func remoteTo(t *testing.T, h http.Handler) *host.Remote {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	return host.NewRemote(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	})
}

type nopHost struct{ host.Host }

func (nopHost) Info(context.Context) engine.HostInfo { return engine.HostInfo{Name: "agent"} }

func TestAgentProbeEndpoint(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer target.Close()
	ctx := context.Background()
	req := host.ProbeRequest{Kind: KindHTTP, Target: target.URL, Timeout: 5}

	// A read-only agent still probes: probing changes nothing on its host.
	ro := remoteTo(t, host.NewAgentHandler(nopHost{}, host.AgentOptions{ReadOnly: true, Version: "t", Prober: NewAgentProber()}))
	res, err := ro.Probe(ctx, req, 10*time.Second)
	if err != nil || !res.OK || res.Msg != "HTTP 204" {
		t.Fatalf("read-only agent probe: %+v %v", res, err)
	}
	if info, err := ro.AgentInfo(ctx); err != nil || !info.Probes || !info.ReadOnly {
		t.Fatalf("info: %+v %v", info, err)
	}
	// stack and log checks are refused by the agent itself
	_, err = ro.Probe(ctx, host.ProbeRequest{Kind: KindStack, Target: "web", Timeout: 5}, 10*time.Second)
	var ve engine.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("stack probe: %v", err)
	}

	// MOORAGE_PROBES=false: refused with its own code, not a 404
	off := remoteTo(t, host.NewAgentHandler(nopHost{}, host.AgentOptions{Version: "t"}))
	if _, err := off.Probe(ctx, req, 10*time.Second); !host.IsProbesDisabled(err) || host.IsUnsupported(err) {
		t.Fatalf("probes off: %v", err)
	}
	if info, _ := off.AgentInfo(ctx); info.Probes {
		t.Fatal("info says probes are on")
	}

	// busy maps to engine.ErrBusy across the tunnel
	busy := NewAgentProber()
	for range MaxRemoteProbes {
		busy.sem <- struct{}{}
	}
	b := remoteTo(t, host.NewAgentHandler(nopHost{}, host.AgentOptions{Prober: busy}))
	if _, err := b.Probe(ctx, req, 10*time.Second); !errors.Is(err, engine.ErrBusy) {
		t.Fatalf("busy: %v", err)
	}

	// An older agent has no /probe route: 404 is unsupported.
	old := remoteTo(t, http.NotFoundHandler())
	if _, err := old.Probe(ctx, req, 10*time.Second); !host.IsUnsupported(err) {
		t.Fatalf("old agent: %v", err)
	}
}

// locEnv is a monitor with three registered agents: wd1 (probes, its answer is fixed by
// the test), wd2 (an older agent without /probe) and wd3 (offline).
type locEnv struct {
	m      *Monitor
	st     *store.Store
	wd1    *AgentProber
	answer Outcome
}

func newLocEnv(t *testing.T) *locEnv {
	t.Helper()
	m, st, _ := newTestMonitor(t)
	m.reg = host.NewRegistry(nil, "", false, st)
	ctx := context.Background()
	for _, a := range [][2]string{{"ag1", "wd1"}, {"ag2", "wd2"}, {"ag3", "wd3"}} {
		_ = st.CreateJoinToken(ctx, "h"+a[0], a[1], time.Now().Add(time.Hour))
		if _, err := st.RedeemJoinToken(ctx, "h"+a[0], a[0], a[1]); err != nil {
			t.Fatal(err)
		}
	}
	e := &locEnv{m: m, st: st, wd1: NewAgentProber()}
	e.wd1.run = func(context.Context, store.Check) Outcome { return e.answer }
	m.reg.Attach("ag1", remoteTo(t, host.NewAgentHandler(nopHost{}, host.AgentOptions{ReadOnly: true, Prober: e.wd1})), host.AgentInfo{Probes: true}, func() error { return nil })
	m.reg.Attach("ag2", remoteTo(t, http.NotFoundHandler()), host.AgentInfo{}, func() error { return nil })
	return e
}

func (e *locEnv) check(t *testing.T, quorum, target string, locs ...string) store.Check {
	t.Helper()
	ctx := context.Background()
	chID, _ := e.st.SaveChannel(ctx, store.Channel{Name: "hook", Kind: "webhook", Config: `{"url":"https://h"}`, Enabled: true})
	id, err := e.st.SaveCheck(ctx, store.Check{Name: "site", Kind: KindHTTP, Target: target, Interval: 60, Timeout: 2, Retries: 1,
		Enabled: true, Channels: []int64{chID}, Locations: locs, Quorum: quorum})
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.st.CheckByID(ctx, id)
	if err != nil || len(c.Locations) != len(locs) || c.Quorum != quorum {
		t.Fatalf("stored check: %+v %v", c, err)
	}
	return c
}

func TestMultiLocationRunVotesAndMessages(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer up.Close()
	e := newLocEnv(t)
	e.answer = Outcome{Msg: "timed out", Latency: 2 * time.Second}
	ctx := context.Background()
	all := []string{LocationHub, "ag1", "ag2", "ag3"}

	// majority: hub up, wd1 down, wd2 and wd3 don't vote -> 1 of 2 failing is no majority
	o := e.m.probeLocations(ctx, e.check(t, QuorumMajority, up.URL, all...))
	if !o.OK || o.Unknown {
		t.Fatalf("majority: %+v", o)
	}
	for _, want := range []string{"up from 1 of 2 locations (HTTP 200)", "failing: wd1 (timed out)", "wd2 (agent too old to run checks)", "wd3 (agent offline)"} {
		if !strings.Contains(o.Msg, want) {
			t.Errorf("message %q lacks %q", o.Msg, want)
		}
	}
	if len(o.Locations) != 4 || o.Locations[0].Name != "hub" || o.Locations[1].Vote != VoteDown || o.Locations[2].Vote != VoteNone {
		t.Fatalf("locations: %+v", o.Locations)
	}

	// any: the same answers are down
	o = e.m.probeLocations(ctx, e.check(t, QuorumAny, up.URL, all...))
	if o.OK || o.Unknown || !strings.HasPrefix(o.Msg, "down from 1 of 2 locations: wd1 (timed out); up from hub") {
		t.Fatalf("any: %+v", o)
	}

	// all: hub up -> up; both failing -> down
	if o = e.m.probeLocations(ctx, e.check(t, QuorumAll, up.URL, LocationHub, "ag1")); !o.OK {
		t.Fatalf("all, one up: %+v", o)
	}
	if o = e.m.probeLocations(ctx, e.check(t, QuorumAll, "http://127.0.0.1:1/", LocationHub, "ag1")); o.OK ||
		!strings.Contains(o.Msg, "down from 2 of 2 locations: hub (connection refused), wd1 (timed out)") {
		t.Fatalf("all, both down: %+v", o)
	}
}

func TestMultiLocationNobodyAnswersIsUnknownWithoutAlert(t *testing.T) {
	e := newLocEnv(t)
	ctx := context.Background()
	c := e.check(t, QuorumMajority, "http://x", "ag2", "ag3")
	o := e.m.probeLocations(ctx, c)
	if !o.Unknown || o.OK || !strings.HasPrefix(o.Msg, "no location could run the check") {
		t.Fatalf("outcome: %+v", o)
	}
	now := time.Now()
	e.m.handle(c, o, now)
	e.m.handle(c, o, now.Add(time.Minute))
	if s := e.m.State(c.ID); s.Status != StatusUnknown || s.Last == nil || s.Last.Status != StatusUnknown || len(s.Locations) != 2 {
		t.Fatalf("state: %+v", s)
	}
	if open, _ := e.st.OpenIncidents(ctx); len(open) != 0 {
		t.Fatalf("unknown opened an incident: %v", open)
	}
	e.m.flush(groupKey(c))
	if up, n, _ := e.st.Uptime(ctx, c.ID, now.Add(-time.Hour)); n != 0 {
		t.Fatalf("unknown counted in uptime: %v over %d", up, n)
	}

	// An announced down stays down through unknown runs: no recovery, no second alert.
	down := Outcome{Msg: "down from 1 of 1 location: wd1 (HTTP 502)", Locations: []LocationResult{{Location: "ag1", Name: "wd1", Vote: VoteDown, Msg: "HTTP 502"}}}
	e.m.handle(c, down, now.Add(2*time.Minute))
	e.m.handle(c, o, now.Add(3*time.Minute))
	if s := e.m.State(c.ID); s.Status != StatusDown {
		t.Fatalf("unknown ended a down: %s", s.Status)
	}
	if open, _ := e.st.OpenIncidents(ctx); len(open) != 1 {
		t.Fatalf("incident: %v", open)
	}
}

func TestMultiLocationRowsAlertBreakdownAndPrune(t *testing.T) {
	e := newLocEnv(t)
	ctx := context.Background()
	e.answer = Outcome{Msg: "connection refused", Latency: 3 * time.Millisecond}
	c := e.check(t, QuorumAny, "http://127.0.0.1:1/", LocationHub, "ag1", "ag3")
	var msgs []string
	e.m.send = func(_ context.Context, _ store.Channel, m notify.Message) error {
		msgs = append(msgs, m.Title+"\n"+m.Text)
		return nil
	}
	o := e.m.probeLocations(ctx, c)
	now := time.Now()
	e.m.handle(c, o, now)
	e.m.flush(groupKey(c))

	rows, err := e.st.RecentLocationResults(ctx, c.ID, 10)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows: %+v %v", rows, err)
	}
	byLoc := map[string]store.LocationResult{}
	for _, r := range rows {
		byLoc[r.Location] = r
	}
	if r := byLoc["ag3"]; r.Voted || r.Msg != "agent offline" {
		t.Fatalf("offline row: %+v", r)
	}
	if r := byLoc["ag1"]; !r.Voted || r.OK {
		t.Fatalf("down row: %+v", r)
	}
	ticks, _ := e.m.LocationTicks(ctx, c, 90)
	if len(ticks) != 3 || ticks["ag3"][0].Status != StatusUnknown || ticks["ag1"][0].Status != StatusDown {
		t.Fatalf("location ticks: %+v", ticks)
	}

	if len(msgs) != 1 || !strings.Contains(msgs[0], "By location: hub down (connection refused") ||
		!strings.Contains(msgs[0], "wd1 down (connection refused, 3 ms)") || !strings.Contains(msgs[0], "wd3 no answer (agent offline)") {
		t.Fatalf("alert: %q", msgs)
	}
	open, _ := e.st.OpenIncidents(ctx)
	var ac AlertContext
	if err := json.Unmarshal([]byte(open[c.ID].Context), &ac); err != nil || len(ac.Locations) != 3 {
		t.Fatalf("incident context: %s %v", open[c.ID].Context, err)
	}
	if s := e.m.State(c.ID); len(s.Locations) != 3 {
		t.Fatalf("state locations: %+v", s.Locations)
	}

	// same retention as results
	if n, err := e.st.PruneLocationResults(ctx, now.Add(time.Second)); err != nil || n != 3 {
		t.Fatalf("prune: %d %v", n, err)
	}
	// deleting the check removes its rows
	e.m.handle(c, o, now.Add(time.Minute))
	_ = e.st.DeleteCheck(ctx, c.ID)
	if rows, _ := e.st.RecentLocationResults(ctx, c.ID, 10); len(rows) != 0 {
		t.Fatalf("rows after delete: %d", len(rows))
	}
}

// An agent that stops answering mid-run doesn't hold the run beyond its timeout.
func TestMultiLocationHungAgentDoesNotBlock(t *testing.T) {
	old := locationMargin
	locationMargin = 300 * time.Millisecond
	defer func() { locationMargin = old }()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer up.Close()
	e := newLocEnv(t)
	// ag1's tunnel accepts the request and then never answers (the agent went away).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	e.m.reg.Attach("ag1", host.NewRemote(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", ln.Addr().String())
	}), host.AgentInfo{Probes: true}, func() error { return nil })
	// ag2's tunnel can't even be opened and ignores cancellation.
	block := make(chan struct{})
	defer close(block)
	e.m.reg.Attach("ag2", host.NewRemote(func(context.Context) (net.Conn, error) {
		<-block
		return nil, errors.New("closed")
	}), host.AgentInfo{Probes: true}, func() error { return nil })

	c := e.check(t, QuorumMajority, up.URL, LocationHub, "ag1", "ag2")
	start := time.Now()
	o := e.m.probeLocations(context.Background(), c)
	took := time.Since(start)
	limit := time.Duration(c.Timeout)*time.Second + locationMargin + 1500*time.Millisecond
	if took > limit {
		t.Fatalf("run took %s, limit %s", took, limit)
	}
	if !o.OK || o.Locations[1].Vote != VoteNone || o.Locations[2].Vote != VoteNone || !strings.Contains(o.Msg, "up from 1 of 1 location") {
		t.Fatalf("outcome: %+v", o)
	}
}
