package monitor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"moorage/internal/host"
	"moorage/internal/store"
)

func TestLatencyLimit(t *testing.T) {
	cases := []struct {
		limit, lat int
		ok         bool
		wantOK     bool
	}{
		{0, 5000, true, true},     // no limit
		{1000, 999, true, true},   // under
		{1000, 1000, true, true},  // at the limit
		{1000, 1001, true, false}, // slow
		{1000, 5000, false, false},
	}
	for i, c := range cases {
		ok, msg := LatencyLimit(c.limit, c.ok, c.lat, "HTTP 200")
		if ok != c.wantOK {
			t.Errorf("%d: ok=%v", i, ok)
		}
		if c.ok && !ok && !strings.HasPrefix(msg, "slow: answered in 1001 ms, the limit is 1000 ms (HTTP 200)") {
			t.Errorf("%d: msg %q", i, msg)
		}
		if !c.ok && msg != "HTTP 200" {
			t.Errorf("%d: a failure's message changed: %q", i, msg)
		}
	}
}

// A slow answer fails the check, on the hub and on an agent (the agent gets the
// setting with the probe request and runs the same code).
func TestProbeLatencyLimit(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
	}))
	defer slow.Close()
	c := store.Check{Kind: KindHTTP, Target: slow.URL, Timeout: 5, Settings: `{"maxLatencyMs":20}`}
	if o := Probe(context.Background(), c, nil); o.OK || !strings.HasPrefix(o.Msg, "slow: answered in") {
		t.Fatalf("slow answer passed: %+v", o)
	}
	c.Settings = `{"maxLatencyMs":4000}`
	if o := Probe(context.Background(), c, nil); !o.OK {
		t.Fatalf("answer under the limit failed: %+v", o)
	}

	p := NewAgentProber()
	res, err := p.Probe(context.Background(), host.ProbeRequest{Kind: KindHTTP, Target: slow.URL, Timeout: 5, Settings: json.RawMessage(`{"maxLatencyMs":20}`)})
	if err != nil || res.OK || !strings.Contains(res.Msg, "slow") {
		t.Fatalf("agent probe: %+v %v", res, err)
	}
}
