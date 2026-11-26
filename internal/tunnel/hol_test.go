package tunnel

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"moorage/internal/engine"
	"moorage/internal/host"
)

// Head-of-line blocking between bulk logs and control requests in the hub–agent tunnel
// (M18D). The tunnel is one yamux session over one WebSocket: every request is its own
// yamux stream, but all streams share one write queue and one TCP connection. These
// tests build the real tunnel stack in process (WebSocket + yamux + the agent HTTP API)
// over a loopback connection whose agent→hub direction is throttled like a home uplink,
// flood it with a followed log stream and measure how long a small control request
// (ListStacks) takes meanwhile.
//
// The assertions are deliberately loose (no tight timing on CI or Windows); the numbers
// are logged. MOORAGE_HOL_MEASURE=1 runs more samples on slower links for PLAN.md.

// floodHost follows logs that never end: as many lines as the agent buffers take.
type floodHost struct{ fakeHost }

func (floodHost) StreamLogs(ctx context.Context, _, _ string, _ int, emit func(engine.LogLine)) error {
	msg := strings.Repeat("x", 100)
	for i := 0; ctx.Err() == nil; i++ {
		emit(engine.LogLine{Container: "web", Time: "2026-11-26T12:00:00.000000000Z", Message: msg})
		if i%500 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	return nil
}

// throttledConn models a link of rate bytes/s in the write direction with a small
// send buffer (like a kernel socket buffer sized to the bandwidth-delay product): a
// write returns once its bytes fit into the buffer, so a writer sees back-pressure.
type throttledConn struct {
	net.Conn
	rate   float64 // bytes per second
	buffer time.Duration

	mu    sync.Mutex
	next  time.Time // when the link has sent everything written so far
	total int64     // bytes written
	first time.Time // first write
}

func (c *throttledConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	now := time.Now()
	c.total += int64(len(p))
	if c.first.IsZero() {
		c.first = now
	}
	if c.next.Before(now) {
		c.next = now
	}
	c.next = c.next.Add(time.Duration(float64(len(p)) / c.rate * float64(time.Second)))
	wait := time.Until(c.next) - c.buffer
	c.mu.Unlock()
	if wait > 0 {
		time.Sleep(wait)
	}
	return c.Conn.Write(p)
}

type holResult struct {
	idle, flood []time.Duration
	wireRate    float64 // bytes/s the agent sent over the whole run (link use)
}

// measureHOL runs one tunnel with the agent's uplink limited to rate bytes/s and returns
// control request latencies without and during a log flood. chunked turns the
// mitigation (bounded writes per stream) on for both sides.
func measureHOL(t *testing.T, rate float64, samples int, chunked bool) holResult {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sessCh := make(chan *yamux.Session, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(maxFrame)
		nc := websocket.NetConn(ctx, c, websocket.MessageBinary)
		sess, err := yamux.Client(nc, muxConfig())
		if err != nil {
			return
		}
		sessCh <- sess
		<-ctx.Done()
		sess.Close()
	}))
	defer srv.Close()

	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(maxFrame)
	agentConn := &throttledConn{Conn: websocket.NetConn(ctx, c, websocket.MessageBinary), rate: rate, buffer: 50 * time.Millisecond}
	agentSess, err := yamux.Server(agentConn, muxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer agentSess.Close()
	var ln net.Listener = agentSess
	if chunked {
		ln = boundedListener{agentSess}
	}
	agentSrv := &http.Server{Handler: host.AgentHandler(floodHost{}, true, "test"), ReadHeaderTimeout: 10 * time.Second}
	go agentSrv.Serve(ln) //nolint:errcheck
	defer agentSrv.Close()

	var hubSess *yamux.Session
	select {
	case hubSess = <-sessCh:
	case <-time.After(10 * time.Second):
		t.Fatal("tunnel did not come up")
	}
	dial := func(context.Context) (net.Conn, error) { return hubSess.Open() }
	if chunked {
		dial = func(context.Context) (net.Conn, error) { return openBounded(hubSess) }
	}
	rm := host.NewRemote(dial)

	control := func() time.Duration {
		cctx, ccancel := context.WithTimeout(ctx, 60*time.Second)
		defer ccancel()
		start := time.Now()
		if _, err := rm.ListStacks(cctx); err != nil {
			t.Fatalf("control request failed: %v", err)
		}
		return time.Since(start)
	}

	var res holResult
	for range samples {
		res.idle = append(res.idle, control())
		time.Sleep(20 * time.Millisecond)
	}

	floodCtx, stopFlood := context.WithCancel(ctx)
	floodDone := make(chan struct{})
	go func() {
		defer close(floodDone)
		_ = rm.StreamLogs(floodCtx, "web", "", 0, func(engine.LogLine) {})
	}()
	// Let the flood fill the link first.
	time.Sleep(time.Second)
	for range samples {
		res.flood = append(res.flood, control())
		time.Sleep(100 * time.Millisecond)
	}
	agentConn.mu.Lock()
	res.wireRate = float64(agentConn.total) / time.Since(agentConn.first).Seconds()
	agentConn.mu.Unlock()
	stopFlood()
	<-floodDone
	return res
}

func pct(d []time.Duration, p float64) time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(float64(len(s)-1) * p)
	return s[i]
}

func summary(d []time.Duration) string {
	return fmt.Sprintf("p50 %s, p95 %s, max %s", pct(d, 0.5).Round(time.Millisecond), pct(d, 0.95).Round(time.Millisecond), pct(d, 1).Round(time.Millisecond))
}

// TestTunnelLogFloodHeadOfLine measures a control request during a log flood with and
// without bounded stream writes. It only fails when a control request can't get through
// at all or the mitigation makes things clearly worse.
func TestTunnelLogFloodHeadOfLine(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement")
	}
	rates := []float64{2e6} // 16 Mbit/s uplink
	samples := 8
	if os.Getenv("MOORAGE_HOL_MEASURE") == "1" {
		rates = []float64{1e6, 2e6, 5e6}
		samples = 30
	}
	for _, rate := range rates {
		plain := measureHOL(t, rate, samples, false)
		bounded := measureHOL(t, rate, samples, true)
		t.Logf("link %.0f Mbit/s, plain yamux:    idle %s | during log flood %s | link use %.0f %%", rate*8/1e6, summary(plain.idle), summary(plain.flood), plain.wireRate/rate*100)
		t.Logf("link %.0f Mbit/s, bounded writes: idle %s | during log flood %s | link use %.0f %%", rate*8/1e6, summary(bounded.idle), summary(bounded.flood), bounded.wireRate/rate*100)
		// Loose sanity bound: the mitigation must not make control traffic slower in a
		// way that matters (allow generous noise on busy CI machines).
		if pct(bounded.flood, 0.5) > pct(plain.flood, 0.5)+2*time.Second {
			t.Fatalf("bounded writes slowed control requests down: %s vs %s", summary(bounded.flood), summary(plain.flood))
		}
	}
}
