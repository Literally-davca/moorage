package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"moorage/internal/auth"
	"moorage/internal/config"
	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/monitor"
	"moorage/internal/store"
)

// fakeExecSession simulates a live container TTY stream with close/resize counters.
type fakeExecSession struct {
	inR       *io.PipeReader
	inW       *io.PipeWriter
	outR      *io.PipeReader
	outW      *io.PipeWriter
	closeCnt  atomic.Int32
	closeOnce sync.Once
	closedCh  chan struct{}

	mu       sync.Mutex
	resizes  [][2]int
	exitCode int
	waitErr  error
}

func newFakeExecSession() *fakeExecSession {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	return &fakeExecSession{
		inR:      inR,
		inW:      inW,
		outR:     outR,
		outW:     outW,
		closedCh: make(chan struct{}),
	}
}

func (s *fakeExecSession) Read(p []byte) (int, error) {
	return s.outR.Read(p)
}

func (s *fakeExecSession) Write(p []byte) (int, error) {
	return s.inW.Write(p)
}

func (s *fakeExecSession) Resize(_ context.Context, cols, rows int) error {
	s.mu.Lock()
	s.resizes = append(s.resizes, [2]int{cols, rows})
	s.mu.Unlock()
	return nil
}

func (s *fakeExecSession) Wait(_ context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitCode, s.waitErr
}

func (s *fakeExecSession) Close() error {
	s.closeCnt.Add(1)
	s.closeOnce.Do(func() {
		close(s.closedCh)
		_ = s.inW.Close()
		_ = s.inR.Close()
		_ = s.outW.Close()
		_ = s.outR.Close()
	})
	return nil
}

func (s *fakeExecSession) finishShell(code int, err error) {
	s.mu.Lock()
	s.exitCode = code
	s.waitErr = err
	s.mu.Unlock()
	_ = s.outW.Close()
}

type fakeExecBackend struct {
	mu         sync.Mutex
	containers map[string][]engine.Container
	onStart    func(containerID, shellPath string, cols, rows int) (engine.TerminalSession, error)
}

func (b *fakeExecBackend) StackContainers(_ context.Context, stack string) ([]engine.Container, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]engine.Container(nil), b.containers[stack]...), nil
}

func (b *fakeExecBackend) StartExec(_ context.Context, containerID, shellPath string, cols, rows int) (engine.TerminalSession, error) {
	b.mu.Lock()
	fn := b.onStart
	b.mu.Unlock()
	if fn != nil {
		return fn(containerID, shellPath, cols, rows)
	}
	return newFakeExecSession(), nil
}

type termTestEnv struct {
	db          *store.Store
	srv         *Server
	ts          *httptest.Server
	eng         *engine.Engine
	execBack    *fakeExecBackend
	reg         *host.Registry
	mon         *monitor.Monitor
	logBuf      *syncBuffer
	adminToken  string
	adminID     int64
	viewerToken string
	viewerID    int64
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func createTestUserSession(t *testing.T, db *store.Store, username, role, token string) int64 {
	t.Helper()
	ctx := context.Background()
	if err := db.UpsertPendingUser(ctx, store.User{
		Username: username, PasswordHash: "dummy", Role: role, TOTPSecret: "JBSWY3DPEHPK3PXP",
	}); err != nil {
		t.Fatal(err)
	}
	u, err := db.UserByName(ctx, username)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ActivateUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, sha256Hex(token), u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func newTermTestEnv(t *testing.T, allowLocalTerm bool, localRO bool) *termTestEnv {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	adminID := createTestUserSession(t, db, "admin", "admin", "tok-admin")
	viewerID := createTestUserSession(t, db, "viewer", "viewer", "tok-viewer")

	logBuf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logBuf, nil))
	authSvc, err := auth.New(db, log)
	if err != nil {
		t.Fatal(err)
	}

	fb := &fakeExecBackend{
		containers: map[string][]engine.Container{
			"whoami": {
				{ID: "ctr-running-123456", Name: "whoami-web-1", Service: "web", State: "running"},
				{ID: "ctr-stopped-654321", Name: "whoami-old-1", Service: "old", State: "exited"},
			},
			"other": {
				{ID: "ctr-other-999999", Name: "other-db-1", Service: "db", State: "running"},
			},
		},
	}
	eng := &engine.Engine{ExecBackend: fb}
	loc := host.Local{E: eng, ReadOnly: localRO, AllowTTY: allowLocalTerm && !localRO}
	reg := host.NewRegistry(loc, "local", localRO, db)
	mon := monitor.New(db, reg, log, "")
	srv := New(config.Config{DataDir: dir, StacksDir: dir}, authSvc, db, reg, nil, mon, log, "test")

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &termTestEnv{
		db:          db,
		srv:         srv,
		ts:          ts,
		eng:         eng,
		execBack:    fb,
		reg:         reg,
		mon:         mon,
		logBuf:      logBuf,
		adminToken:  "tok-admin",
		adminID:     adminID,
		viewerToken: "tok-viewer",
		viewerID:    viewerID,
	}
}

func newAgentRemote(t *testing.T, h http.Handler) *host.Remote {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() {
		srv.Close()
		ln.Close()
	})
	return host.NewRemote(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", ln.Addr().String())
	})
}

func dialTermWS(ctx context.Context, baseURL, hostID, stack, query, token, origin string) (*websocket.Conn, *http.Response, error) {
	wsURL := "ws" + strings.TrimPrefix(baseURL, "http") + "/api/hosts/" + hostID + "/stacks/" + stack + "/terminal?" + query
	hdr := http.Header{}
	if token != "" {
		hdr.Set("Cookie", cookieName+"="+token)
	}
	if origin != "" {
		hdr.Set("Origin", origin)
	}
	return websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
}

// expectTermRefusal dials a terminal that the hub or host must refuse: the WebSocket opens
// (a browser can't read an HTTP status), carries one error frame with want, then closes.
func expectTermRefusal(t *testing.T, ctx context.Context, baseURL, hostID, stack, query, token, want string) {
	t.Helper()
	ws, _, err := dialTermWS(ctx, baseURL, hostID, stack, query, token, "")
	if err != nil {
		t.Fatalf("%s: dial: %v", query, err)
	}
	defer ws.CloseNow()
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	typ, data, err := ws.Read(rctx)
	if err != nil || typ != websocket.MessageText {
		t.Fatalf("%s: want an error frame, got type=%v err=%v", query, typ, err)
	}
	var ctrl host.TermControl
	if json.Unmarshal(data, &ctrl) != nil || ctrl.Type != "error" || !strings.Contains(ctrl.Message, want) {
		t.Fatalf("%s: frame = %s, want error containing %q", query, data, want)
	}
	if _, _, err := ws.Read(rctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("%s: want close 1008 after the refusal, got %v", query, err)
	}
}

// Section 6 #1: Switch (agent with MOORAGE_TERMINAL unset/false refuses with terminal_disabled;
// read-only agent with MOORAGE_TERMINAL=true refuses too; AgentInfo.terminal reflects both).
func TestTerminalSwitchAndAgentInfo(t *testing.T) {
	ctx := context.Background()
	fb := &fakeExecBackend{
		containers: map[string][]engine.Container{
			"whoami": {{ID: "c1", Name: "whoami-web-1", Service: "web", State: "running"}},
		},
	}
	eng := &engine.Engine{ExecBackend: fb}

	// 1. Agent with terminal=false (default)
	hOff := host.AgentHandler(host.Local{E: eng, AllowTTY: false}, false, "v1", false)
	rmOff := newAgentRemote(t, hOff)
	infoOff, err := rmOff.AgentInfo(ctx)
	if err != nil || infoOff.Terminal {
		t.Fatalf("expected AgentInfo.Terminal=false when unset, got %+v (err=%v)", infoOff, err)
	}
	if _, _, err := rmOff.Terminal(ctx, "whoami", "whoami-web-1", "sh", 80, 24); !errors.Is(err, host.ErrTerminalDisabled) {
		t.Fatalf("expected ErrTerminalDisabled when off, got %v", err)
	}

	// 2. Read-only agent with MOORAGE_TERMINAL=true
	hRO := host.AgentHandler(host.Local{E: eng, ReadOnly: true, AllowTTY: true}, true, "v1", true)
	rmRO := newAgentRemote(t, hRO)
	infoRO, err := rmRO.AgentInfo(ctx)
	if err != nil || infoRO.Terminal || !infoRO.ReadOnly {
		t.Fatalf("expected AgentInfo.Terminal=false on read-only agent, got %+v (err=%v)", infoRO, err)
	}
	if _, _, err := rmRO.Terminal(ctx, "whoami", "whoami-web-1", "sh", 80, 24); !errors.Is(err, host.ErrTerminalDisabled) {
		t.Fatalf("expected ErrTerminalDisabled on read-only agent even with MOORAGE_TERMINAL=true, got %v", err)
	}

	// 3. Writable agent with MOORAGE_TERMINAL=true
	hOn := host.AgentHandler(host.Local{E: eng, AllowTTY: true}, false, "v1", true)
	rmOn := newAgentRemote(t, hOn)
	infoOn, err := rmOn.AgentInfo(ctx)
	if err != nil || !infoOn.Terminal {
		t.Fatalf("expected AgentInfo.Terminal=true when enabled, got %+v (err=%v)", infoOn, err)
	}
	sess, ctr, err := rmOn.Terminal(ctx, "whoami", "whoami-web-1", "sh", 80, 24)
	if err != nil {
		t.Fatalf("expected Terminal to succeed when enabled, got %v", err)
	}
	_ = sess.Close()
	if ctr.Name != "whoami-web-1" || ctr.Service != "web" {
		t.Fatalf("unexpected container metadata from agent: %+v", ctr)
	}
}

// Section 6 #2 & #9: Access guards (viewer 403, foreign Origin rejected, container not in stack refused by agent,
// stopped container refused, unknown shell 400) and older agent 404.
func TestTerminalAccessGuardsAndOlderAgent(t *testing.T) {
	env := newTermTestEnv(t, true, false)
	ctx := context.Background()

	// Attach a remote agent to test agent-enforced checks and older agent 404.
	agentHandler := host.AgentHandler(host.Local{E: env.eng, AllowTTY: true}, false, "v1", true)
	rmAgent := newAgentRemote(t, agentHandler)
	env.reg.Attach("ag_1", rmAgent, host.AgentInfo{Terminal: true, Version: "v1"}, func() error { return nil })

	// 1. Viewer gets 403.
	_, resp, err := dialTermWS(ctx, env.ts.URL, "ag_1", "whoami", "container=whoami-web-1&shell=sh", env.viewerToken, "")
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer should get 403, got resp=%v err=%v", resp, err)
	}

	// 2. Request with foreign Origin is rejected.
	_, resp, err = dialTermWS(ctx, env.ts.URL, "ag_1", "whoami", "container=whoami-web-1&shell=sh", env.adminToken, "https://evil.example.com")
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Origin should be rejected with 403, got resp=%v err=%v", resp, err)
	}

	// 3. Container that belongs to another stack ("other-db-1"), or by its ID, is refused by the agent.
	expectTermRefusal(t, ctx, env.ts.URL, "ag_1", "whoami", "container=other-db-1&shell=sh", env.adminToken, "isn't part of the stack")
	expectTermRefusal(t, ctx, env.ts.URL, "ag_1", "whoami", "container=ctr-other-999999&shell=sh", env.adminToken, "isn't part of the stack")
	if _, _, err := rmAgent.Terminal(ctx, "whoami", "other-db-1", "sh", 80, 24); !errors.Is(err, engine.ErrContainerNotInStack) {
		t.Fatalf("agent must refuse a container of another stack, got %v", err)
	}

	// 4. Stopped container is refused.
	expectTermRefusal(t, ctx, env.ts.URL, "ag_1", "whoami", "container=whoami-old-1&shell=sh", env.adminToken, "isn't running")

	// 5. Unknown shell name is refused.
	expectTermRefusal(t, ctx, env.ts.URL, "ag_1", "whoami", "container=whoami-web-1&shell=zsh", env.adminToken, "sh, bash or ash")

	// 5b. A host with the terminal off refuses it.
	env.reg.Attach("ag_off", newAgentRemote(t, host.AgentHandler(host.Local{E: env.eng}, false, "v1", false)), host.AgentInfo{Version: "v1"}, func() error { return nil })
	expectTermRefusal(t, ctx, env.ts.URL, "ag_off", "whoami", "container=whoami-web-1&shell=sh", env.adminToken, "disabled")
	// Also test directly on the agent handler so even if the hub didn't check, the agent returns 400.
	if _, _, err := rmAgent.Terminal(ctx, "whoami", "whoami-web-1", "/bin/sh -c id", 80, 24); !errors.Is(err, engine.ErrInvalidShell) {
		t.Fatalf("agent must reject arbitrary command shell with ErrInvalidShell, got %v", err)
	}

	// 6. Older agent without /stacks/{name}/terminal returns 404 -> hub reports "Update the agent to use the terminal."
	oldMux := http.NewServeMux()
	rmOld := newAgentRemote(t, oldMux)
	env.reg.Attach("ag_old", rmOld, host.AgentInfo{Version: "v0"}, func() error { return nil })
	expectTermRefusal(t, ctx, env.ts.URL, "ag_old", "whoami", "container=whoami-web-1&shell=sh", env.adminToken, "Update the agent to use the terminal")
}

// Section 6 #3: Data path (bytes in both directions arrive unchanged incl. binary/UTF-8;
// resize reaches exec; shell exit sends {"type":"exit","code":N} and closes).
func TestTerminalDataPathAndExit(t *testing.T) {
	env := newTermTestEnv(t, true, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fakeSess := newFakeExecSession()
	env.execBack.onStart = func(_, shellPath string, _, _ int) (engine.TerminalSession, error) {
		if shellPath != "/bin/bash" {
			t.Errorf("shellPath = %q, want /bin/bash", shellPath)
		}
		return fakeSess, nil
	}

	// Run through remote agent tunnel leg to test browser ⇄ hub ⇄ agent ⇄ fakeExec end-to-end!
	agentHandler := host.AgentHandler(host.Local{E: env.eng, AllowTTY: true}, false, "v1", true)
	rmAgent := newAgentRemote(t, agentHandler)
	env.reg.Attach("ag_1", rmAgent, host.AgentInfo{Terminal: true, Version: "v1"}, func() error { return nil })

	ws, _, err := dialTermWS(ctx, env.ts.URL, "ag_1", "whoami", "container=whoami-web-1&shell=bash&cols=100&rows=30", env.adminToken, "")
	if err != nil {
		t.Fatalf("dialTermWS: %v", err)
	}
	defer ws.CloseNow()

	// 1. Browser -> exec (UTF-8 + binary bytes).
	wantIn := []byte("echo 'příštav ⚓'\x00\xff\n")
	go func() {
		_ = ws.Write(ctx, websocket.MessageBinary, wantIn)
	}()
	gotIn := make([]byte, len(wantIn))
	if _, err := io.ReadFull(fakeSess.inR, gotIn); err != nil {
		t.Fatalf("read from fakeSess.inR: %v", err)
	}
	if !bytes.Equal(gotIn, wantIn) {
		t.Fatalf("input mismatch: got %x, want %x", gotIn, wantIn)
	}

	// 2. Exec -> browser.
	wantOut := []byte("příštav ⚓\r\n\x1b[32mok\x1b[0m")
	go func() {
		_, _ = fakeSess.outW.Write(wantOut)
	}()
	typ, gotOut, err := ws.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || !bytes.Equal(gotOut, wantOut) {
		t.Fatalf("output mismatch: typ=%v got=%x want=%x err=%v", typ, gotOut, wantOut, err)
	}

	// 3. Resize control message (clamped to 500).
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":132,"rows":999}`)); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	resized := false
	for time.Now().Before(deadline) {
		fakeSess.mu.Lock()
		if len(fakeSess.resizes) > 0 {
			last := fakeSess.resizes[len(fakeSess.resizes)-1]
			if last == [2]int{132, 500} {
				resized = true
			}
		}
		fakeSess.mu.Unlock()
		if resized {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !resized {
		t.Fatalf("resize did not reach exec with clamped 132x500")
	}

	// 4. Shell exit with code 42 sends {"type":"exit","code":42} and closes.
	fakeSess.finishShell(42, nil)
	typ, exitData, err := ws.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		t.Fatalf("expected text exit frame, got typ=%v err=%v", typ, err)
	}
	var ctrl host.TermControl
	if err := json.Unmarshal(exitData, &ctrl); err != nil || ctrl.Type != "exit" || ctrl.Code != 42 {
		t.Fatalf("unexpected exit control frame: %s (%v)", string(exitData), err)
	}
}

// Section 6 #4 & #5: Cleanup and Goroutine Leak Check across all 6 termination paths,
// plus verifying that the idle timer is reset ONLY by input, never by output.
func TestTerminalCleanupPathsAndIdleTimer(t *testing.T) {
	env := newTermTestEnv(t, true, false)
	ctx := context.Background()

	waitClosed := func(t *testing.T, s *fakeExecSession, label string) {
		t.Helper()
		select {
		case <-s.closedCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: fake exec session was not closed", label)
		}
	}

	// Settle baseline goroutine count.
	time.Sleep(50 * time.Millisecond)
	baseGoroutines := runtime.NumGoroutine()

	// Path 1: Browser close.
	{
		s1 := newFakeExecSession()
		env.execBack.onStart = func(string, string, int, int) (engine.TerminalSession, error) { return s1, nil }
		ws, _, err := dialTermWS(ctx, env.ts.URL, "local", "whoami", "container=whoami-web-1&shell=sh", env.adminToken, "")
		if err != nil {
			t.Fatal(err)
		}
		_ = ws.Close(websocket.StatusNormalClosure, "bye")
		waitClosed(t, s1, "browser close")
	}

	// Path 2: Agent/tunnel close.
	{
		s2 := newFakeExecSession()
		started := make(chan struct{})
		env.execBack.onStart = func(string, string, int, int) (engine.TerminalSession, error) {
			close(started)
			return s2, nil
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		agentSrv := &http.Server{Handler: host.AgentHandler(host.Local{E: env.eng, AllowTTY: true}, false, "v1", true)}
		go agentSrv.Serve(ln) //nolint:errcheck
		var activeConns []net.Conn
		var connMu sync.Mutex
		rm := host.NewRemote(func(dctx context.Context) (net.Conn, error) {
			var d net.Dialer
			c, err := d.DialContext(dctx, "tcp", ln.Addr().String())
			if err == nil {
				connMu.Lock()
				activeConns = append(activeConns, c)
				connMu.Unlock()
			}
			return c, err
		})
		env.reg.Attach("ag_drop", rm, host.AgentInfo{Terminal: true, Version: "v1"}, func() error { return nil })

		ws, _, err := dialTermWS(ctx, env.ts.URL, "ag_drop", "whoami", "container=whoami-web-1&shell=sh", env.adminToken, "")
		if err != nil {
			t.Fatal(err)
		}
		// The upgrade comes before the host is asked, so wait until the agent opened the exec.
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("agent never opened the exec")
		}
		time.Sleep(50 * time.Millisecond)
		// Abruptly close the tunnel connection and agent server.
		connMu.Lock()
		for _, c := range activeConns {
			_ = c.Close()
		}
		connMu.Unlock()
		_ = agentSrv.Close()
		_ = ln.Close()
		waitClosed(t, s2, "agent/tunnel close")
		_ = ws.CloseNow()
	}

	// Path 3 & Section 6 #5: Idle timeout — shell prints output every 20 ms, but without browser input it STILL times out!
	{
		env.srv.SetTerminalLimits(TerminalLimits{
			MaxConcurrent:  4,
			IdleTimeout:    120 * time.Millisecond,
			HardLimit:      10 * time.Second,
			RevokeCheckInt: 10 * time.Second,
		})
		s3 := newFakeExecSession()
		env.execBack.onStart = func(string, string, int, int) (engine.TerminalSession, error) { return s3, nil }
		ws, _, err := dialTermWS(ctx, env.ts.URL, "local", "whoami", "container=whoami-web-1&shell=sh", env.adminToken, "")
		if err != nil {
			t.Fatal(err)
		}
		stopSpam := make(chan struct{})
		go func() {
			t := time.NewTicker(20 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-stopSpam:
					return
				case <-s3.closedCh:
					return
				case <-t.C:
					_, _ = s3.outW.Write([]byte("tick\r\n"))
				}
			}
		}()
		// Read output frames until idle timeout closes the session.
		go func() {
			for {
				if _, _, err := ws.Read(ctx); err != nil {
					return
				}
			}
		}()
		waitClosed(t, s3, "idle timeout despite continuous output")
		close(stopSpam)
		_ = ws.CloseNow()
	}

	// Path 4: Hard limit timeout (even when browser sends input!).
	{
		env.srv.SetTerminalLimits(TerminalLimits{
			MaxConcurrent:  4,
			IdleTimeout:    10 * time.Second,
			HardLimit:      100 * time.Millisecond,
			RevokeCheckInt: 10 * time.Second,
		})
		s4 := newFakeExecSession()
		go func() { _, _ = io.Copy(io.Discard, s4.inR) }()
		env.execBack.onStart = func(string, string, int, int) (engine.TerminalSession, error) { return s4, nil }
		ws, _, err := dialTermWS(ctx, env.ts.URL, "local", "whoami", "container=whoami-web-1&shell=sh", env.adminToken, "")
		if err != nil {
			t.Fatal(err)
		}
		waitClosed(t, s4, "hard limit")
		_ = ws.CloseNow()
	}

	// Path 5: Shell exit.
	{
		env.srv.SetTerminalLimits(TerminalLimits{}) // restore defaults
		s5 := newFakeExecSession()
		env.execBack.onStart = func(string, string, int, int) (engine.TerminalSession, error) { return s5, nil }
		ws, _, err := dialTermWS(ctx, env.ts.URL, "local", "whoami", "container=whoami-web-1&shell=sh", env.adminToken, "")
		if err != nil {
			t.Fatal(err)
		}
		s5.finishShell(0, nil)
		waitClosed(t, s5, "shell exit")
		_ = ws.CloseNow()
	}

	// Path 6: Hub context cancel.
	{
		hubCtx, hubCancel := context.WithCancel(context.Background())
		env.srv.SetTerminalLimits(TerminalLimits{HubCtx: hubCtx})
		s6 := newFakeExecSession()
		env.execBack.onStart = func(string, string, int, int) (engine.TerminalSession, error) { return s6, nil }
		ws, _, err := dialTermWS(ctx, env.ts.URL, "local", "whoami", "container=whoami-web-1&shell=sh", env.adminToken, "")
		if err != nil {
			t.Fatal(err)
		}
		hubCancel()
		waitClosed(t, s6, "hub context cancel")
		_ = ws.CloseNow()
		env.srv.SetTerminalLimits(TerminalLimits{})
	}

	// Verify no goroutine leak after all 6 sessions ended.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseGoroutines+2 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > baseGoroutines+2 {
		t.Fatalf("goroutine leak: before=%d, after=%d", baseGoroutines, after)
	}
}

// Section 6 #6: Concurrency limit (4 allowed, 5th refused with 429; after one closes, a new one is allowed).
func TestTerminalConcurrencyLimit(t *testing.T) {
	env := newTermTestEnv(t, true, false)
	ctx := context.Background()

	env.execBack.onStart = func(string, string, int, int) (engine.TerminalSession, error) {
		return newFakeExecSession(), nil
	}

	var conns []*websocket.Conn
	for i := range 4 {
		ws, _, err := dialTermWS(ctx, env.ts.URL, "local", "whoami", "container=whoami-web-1&shell=sh", env.adminToken, "")
		if err != nil {
			t.Fatalf("open terminal %d: %v", i+1, err)
		}
		conns = append(conns, ws)
	}
	defer func() {
		for _, c := range conns {
			_ = c.CloseNow()
		}
	}()

	// The 5th concurrent terminal is refused.
	expectTermRefusal(t, ctx, env.ts.URL, "local", "whoami", "container=whoami-web-1&shell=sh", env.adminToken, "Too many open terminals")

	// Close one terminal and wait a brief moment for the slot to be released.
	_ = conns[0].Close(websocket.StatusNormalClosure, "")
	var ws5 *websocket.Conn
	deadline := time.Now().Add(2 * time.Second)
	for ws5 == nil && time.Now().Before(deadline) {
		c, _, derr := dialTermWS(ctx, env.ts.URL, "local", "whoami", "container=whoami-web-1&shell=sh", env.adminToken, "")
		if derr != nil {
			t.Fatal(derr)
		}
		// A refusal arrives at once as an error frame; an open session stays silent (the fake prints nothing).
		rctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		_, _, rerr := c.Read(rctx)
		cancel()
		if errors.Is(rerr, context.DeadlineExceeded) {
			ws5 = c
			break
		}
		_ = c.CloseNow()
		time.Sleep(20 * time.Millisecond)
	}
	if ws5 == nil {
		t.Fatalf("expected new terminal to succeed after closing one")
	}
	_ = ws5.CloseNow()
}

// Section 6 #7: Session revocation (sign-out or role change to viewer ends an open terminal with reason "revoked").
func TestTerminalSessionRevocation(t *testing.T) {
	env := newTermTestEnv(t, true, false)
	ctx := context.Background()

	env.srv.SetTerminalLimits(TerminalLimits{
		MaxConcurrent:  4,
		IdleTimeout:    10 * time.Second,
		HardLimit:      10 * time.Second,
		RevokeCheckInt: 40 * time.Millisecond,
	})

	// 1. Sign-out revokes active terminal.
	s1 := newFakeExecSession()
	env.execBack.onStart = func(string, string, int, int) (engine.TerminalSession, error) { return s1, nil }
	ws1, _, err := dialTermWS(ctx, env.ts.URL, "local", "whoami", "container=whoami-web-1&shell=sh", env.adminToken, "")
	if err != nil {
		t.Fatal(err)
	}
	// Delete the admin session (sign-out).
	_ = env.db.DeleteSession(ctx, sha256Hex(env.adminToken))
	select {
	case <-s1.closedCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("terminal did not close after session logout")
	}
	_ = ws1.CloseNow()

	// Wait for audit row update and check reason == "revoked".
	time.Sleep(40 * time.Millisecond)
	rows, err := env.db.TerminalSessions(ctx, "local", "whoami", 10)
	if err != nil || len(rows) == 0 || rows[0].Reason != store.TermReasonRevoked {
		t.Fatalf("expected audit row reason=%q after logout, got %+v (err=%v)", store.TermReasonRevoked, rows, err)
	}

	// 2. Role change from admin to viewer revokes active terminal.
	_ = env.db.CreateSession(ctx, sha256Hex("tok-admin-2"), env.adminID, time.Now().Add(time.Hour))
	s2 := newFakeExecSession()
	env.execBack.onStart = func(string, string, int, int) (engine.TerminalSession, error) { return s2, nil }
	ws2, _, err := dialTermWS(ctx, env.ts.URL, "local", "whoami", "container=whoami-web-1&shell=sh", "tok-admin-2", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.db.UpdateUserRole(ctx, env.adminID, "viewer"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s2.closedCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("terminal did not close after user role changed to viewer")
	}
	_ = ws2.CloseNow()

	time.Sleep(40 * time.Millisecond)
	rows, _ = env.db.TerminalSessions(ctx, "local", "whoami", 10)
	if len(rows) < 2 || rows[0].Reason != store.TermReasonRevoked {
		t.Fatalf("expected audit row reason=%q after role change, got %+v", store.TermReasonRevoked, rows[0])
	}
}

// Section 6 #8: Audit log (row exists while running; gets ended_at, reason, exit_code at end;
// abandoned rows closed with hub-stopped on start; StackEvent has kind "terminal" and "terminal" is not in DeployKinds;
// no typed or printed bytes appear in the DB row, event, or slog output).
func TestTerminalAuditAndZeroCommandLeak(t *testing.T) {
	if monitor.DeployKinds["terminal"] {
		t.Fatalf("'terminal' must NOT be in monitor.DeployKinds")
	}

	env := newTermTestEnv(t, true, false)
	ctx := context.Background()

	// 1. Pre-insert an unfinished row and verify CloseAbandonedTerminals marks it "hub-stopped".
	abandonedID, err := env.db.StartTerminalSession(ctx, store.TerminalAuditRow{
		User: "admin", IP: "10.0.0.1", HostID: "local", Stack: "whoami",
		ContainerName: "whoami-web-1", Service: "web", Shell: "sh", StartedAt: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.db.CloseAbandonedTerminals(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	abRow, err := env.db.TerminalSessionByID(ctx, abandonedID)
	if err != nil || abRow.EndedAt.IsZero() || abRow.Reason != store.TermReasonHubStopped {
		t.Fatalf("expected abandoned row closed with hub-stopped, got %+v (err=%v)", abRow, err)
	}

	// 2. Open a live session, verify row exists while running, type a secret password and print a secret token.
	fakeSess := newFakeExecSession()
	env.execBack.onStart = func(string, string, int, int) (engine.TerminalSession, error) { return fakeSess, nil }

	ws, _, err := dialTermWS(ctx, env.ts.URL, "local", "whoami", "container=whoami-web-1&shell=sh", env.adminToken, "")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()

	// Check audit row while running. The upgrade comes before the host is asked, so wait for it.
	var rows []store.TerminalAuditRow
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if rows, err = env.db.TerminalSessions(ctx, "local", "whoami", 10); err != nil || len(rows) >= 2 {
			break
		}
	}
	if err != nil || len(rows) < 2 {
		t.Fatalf("expected active audit row while running, got %+v (%v)", rows, err)
	}
	liveRow := rows[0]
	if !liveRow.EndedAt.IsZero() || liveRow.Reason != "" || liveRow.User != "admin" || liveRow.ContainerName != "whoami-web-1" {
		t.Fatalf("unexpected running audit row: %+v", liveRow)
	}

	// R1: opening the shell also writes one row in the global audit log.
	auditList, err := env.db.AuditEntries(ctx, store.AuditFilter{HostID: "local", Stack: "whoami", ActionPrefix: "terminal."})
	if err != nil || len(auditList) != 1 || auditList[0].Action != "terminal.open" || auditList[0].Actor != "admin" ||
		auditList[0].Target != "whoami-web-1 · sh" || auditList[0].IP == "" || auditList[0].Outcome != store.AuditOK {
		t.Fatalf("expected one terminal.open audit row, got %+v (err=%v)", auditList, err)
	}

	secretInput := "TOP_SECRET_TYPED_PASSWORD_987654"
	secretOutput := "TOP_SECRET_PRINTED_TOKEN_123456"

	go func() {
		_ = ws.Write(ctx, websocket.MessageBinary, []byte(secretInput+"\n"))
	}()
	buf := make([]byte, 128)
	_, _ = fakeSess.inR.Read(buf)

	go func() {
		_, _ = fakeSess.outW.Write([]byte(secretOutput + "\r\n"))
	}()
	_, _, _ = ws.Read(ctx)

	// Finish shell with exit code 7.
	fakeSess.finishShell(7, nil)
	_, _, _ = ws.Read(ctx) // read exit frame

	// Wait for session end to be recorded in SQLite.
	deadline := time.Now().Add(2 * time.Second)
	var finishedRow store.TerminalAuditRow
	for time.Now().Before(deadline) {
		r, err := env.db.TerminalSessionByID(ctx, liveRow.ID)
		if err == nil && !r.EndedAt.IsZero() {
			finishedRow = r
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if finishedRow.EndedAt.IsZero() || finishedRow.Reason != store.TermReasonExit || finishedRow.ExitCode == nil || *finishedRow.ExitCode != 7 {
		t.Fatalf("expected finished audit row with reason=exit and exitCode=7, got %+v", finishedRow)
	}

	// Verify StackEvent was recorded with kind "terminal".
	evs, err := env.db.Events(ctx, "local", "whoami", time.Now().Add(-time.Minute), "terminal")
	if err != nil || len(evs) != 1 || evs[0].Actor != "admin" || evs[0].Detail != "web · sh" {
		t.Fatalf("expected 1 terminal StackEvent, got %+v (err=%v)", evs, err)
	}

	// Verify zero leakage of typed or printed bytes in audit row, stack events, or slog output.
	rowJSON, _ := json.Marshal(finishedRow)
	evJSON, _ := json.Marshal(evs)
	logs := env.logBuf.String()
	for _, secret := range []string{secretInput, secretOutput} {
		if strings.Contains(string(rowJSON), secret) {
			t.Fatalf("secret %q leaked into terminal_sessions row: %s", secret, string(rowJSON))
		}
		if strings.Contains(string(evJSON), secret) {
			t.Fatalf("secret %q leaked into events table: %s", secret, string(evJSON))
		}
		if strings.Contains(logs, secret) {
			t.Fatalf("secret %q leaked into slog output:\n%s", secret, logs)
		}
	}
}
