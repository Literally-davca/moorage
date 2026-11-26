//go:build pssgate

package pssgate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"moorage/internal/config"
	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/monitor"
	"moorage/internal/tunnel"
)

// Settings come from pss-gate.sh.
//
//	PSS_SOCKET         Moorage's local socket (admin "cli:local", no sign-in needed)
//	PSS_AGENT_ADDR     the hub's agent port, host:port
//	PSS_READY_FILE     written once the whole load runs
//	PSS_STOP_FILE      the load ends when this file appears (or after PSS_HOLD)
//	PSS_LOG_CONTAINER  a container that keeps printing lines, for the log streams
//	PSS_AGENTS / PSS_CHECKS / PSS_LOG_CLIENTS  sizes (15 / 100 / 2)
//	PSS_HOLD           longest time to keep the load up (default 10m)
func env(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Fatalf("%s is not set; run this through scripts/dev/pss-gate.sh", k)
	}
	return v
}

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil && n > 0 {
		return n
	}
	return def
}

type sockAPI struct {
	c *http.Client
}

func newSockAPI(path string) sockAPI {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}
	return sockAPI{c: &http.Client{Transport: tr, Timeout: 30 * time.Second}}
}

func (a sockAPI) do(t *testing.T, method, path string, body, out any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://moorage"+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		t.Fatalf("%s %s: %s %s", method, path, resp.Status, msg)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
}

func TestPSSLoad(t *testing.T) {
	sock := env(t, "PSS_SOCKET")
	agentAddr := env(t, "PSS_AGENT_ADDR")
	readyFile := env(t, "PSS_READY_FILE")
	stopFile := env(t, "PSS_STOP_FILE")
	logContainer := env(t, "PSS_LOG_CONTAINER")
	nAgents, nChecks, nLogs := envInt("PSS_AGENTS", 15), envInt("PSS_CHECKS", 100), envInt("PSS_LOG_CLIENTS", 2)
	hold := 10 * time.Minute
	if d, err := time.ParseDuration(os.Getenv("PSS_HOLD")); err == nil && d > 0 {
		hold = d
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := newSockAPI(sock)

	// 1. Agents: real tunnel connections (mTLS WebSocket + yamux) to the hub's agent port.
	// They serve the runner's Docker read-only through one shared engine in this process.
	work := t.TempDir()
	eng, err := engine.New(ctx, config.Config{DataDir: filepath.Join(work, "eng"), StacksDir: filepath.Join(work, "stacks")}, quiet)
	if err != nil {
		t.Fatalf("engine for the fake agents: %v", err)
	}
	for i := 0; i < nAgents; i++ {
		var tok struct{ Token string }
		api.do(t, http.MethodPost, "/api/agents", map[string]string{"name": fmt.Sprintf("pss-agent-%02d", i+1)}, &tok)
		a := &tunnel.Agent{
			Hub: agentAddr, JoinToken: tok.Token, Dir: filepath.Join(work, fmt.Sprintf("agent-%02d", i+1)),
			Handler: host.NewAgentHandler(host.Local{E: eng, ReadOnly: true}, host.AgentOptions{
				ReadOnly: true, Version: "pssgate", Prober: monitor.NewAgentProber(),
			}),
			Log: quiet,
		}
		go a.Run(ctx) //nolint:errcheck
	}
	var agentIDs []string
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var hs struct {
			Hosts []struct {
				ID        string `json:"id"`
				Connected bool   `json:"connected"`
			} `json:"hosts"`
		}
		api.do(t, http.MethodGet, "/api/hosts", nil, &hs)
		agentIDs = agentIDs[:0]
		for _, h := range hs.Hosts {
			if h.Connected && h.ID != host.LocalID {
				agentIDs = append(agentIDs, h.ID)
			}
		}
		if len(agentIDs) >= nAgents {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d agents connected", len(agentIDs), nAgents)
		}
		time.Sleep(time.Second)
	}
	t.Logf("%d agents connected", len(agentIDs))

	// 2. Checks: HTTP checks at the shortest interval against a target in this process;
	// every fourth also runs from an agent (hub + agent, through the tunnel).
	var hits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer target.Close()
	for i := 0; i < nChecks; i++ {
		c := map[string]any{
			"name": fmt.Sprintf("pss-check-%03d", i+1), "kind": "http", "target": fmt.Sprintf("%s/c/%d", target.URL, i+1),
			"interval": 20, "timeout": 5, "retries": 1, "enabled": true, "settings": map[string]any{}, "channels": []int64{},
		}
		if i%4 == 0 {
			c["locations"] = []string{"hub", agentIDs[i%len(agentIDs)]}
			c["quorum"] = "majority"
		}
		api.do(t, http.MethodPost, "/api/checks", c, nil)
	}
	t.Logf("%d checks created", nChecks)

	// 3. Log streams: clients following a container that keeps printing.
	var lines atomic.Int64
	for i := 0; i < nLogs; i++ {
		ws, _, err := websocket.Dial(ctx, "ws://moorage/api/hosts/"+host.LocalID+"/ws/logs?container="+logContainer+"&tail=200",
			&websocket.DialOptions{HTTPClient: api.c})
		if err != nil {
			t.Fatalf("log stream %d: %v", i+1, err)
		}
		ws.SetReadLimit(8 << 20)
		go func() {
			defer ws.CloseNow()
			for {
				_, data, err := ws.Read(ctx)
				if err != nil {
					return
				}
				var msg struct {
					Lines []json.RawMessage `json:"lines"`
				}
				if json.Unmarshal(data, &msg) == nil {
					lines.Add(int64(len(msg.Lines)))
				}
			}
		}()
	}

	if err := os.WriteFile(readyFile, []byte("ready\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("load is up")

	end := time.Now().Add(hold)
	for time.Now().Before(end) {
		if _, err := os.Stat(stopFile); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	// A load that didn't really run would make the measurement worthless.
	if lines.Load() == 0 {
		t.Errorf("the log streams received no lines")
	}
	if hits.Load() == 0 {
		t.Errorf("no check reached the target")
	}
	t.Logf("log lines streamed: %d, check requests: %d", lines.Load(), hits.Load())
}
