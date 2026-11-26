package tunnel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"moorage/internal/host"
	"moorage/internal/store"
)

func TestCleanHubAddr(t *testing.T) {
	for in, want := range map[string]string{
		"hub.example.com:5081":               "hub.example.com:5081",
		"  hub.example.com:5081  ":           "hub.example.com:5081",
		"https://hub.example.com:5081/":      "hub.example.com:5081",
		"https://wss://hub.example.com:5081": "hub.example.com:5081",
		"WSS://hub.example.com:5081/connect": "hub.example.com:5081",
		`"hub.example.com:5081"`:             "hub.example.com:5081",
		"hub.example.com":                    "hub.example.com:5081",
		"192.168.1.10":                       "192.168.1.10:5081",
		"[fd00::1]:6000":                     "[fd00::1]:6000",
		"fd00::1":                            "[fd00::1]:5081",
		"":                                   "",
		"https://":                           "",
	} {
		if got := CleanHubAddr(in); got != want {
			t.Errorf("CleanHubAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

// A lost enrolment answer leaves the token usable for the same, never-connected agent;
// the first connection burns it; a removed or connected agent can't reuse it.
func TestRedeemJoinTokenRetryUntilFirstConnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := startHub(t, ctx)
	tok, _, err := env.hub.NewJoinToken(ctx, "nas")
	if err != nil {
		t.Fatal(err)
	}
	secret, _, _ := ParseJoinToken(tok)
	hash := HashSecret(secret)
	a1, err := env.st.RedeemJoinToken(ctx, hash, "ag_first", "nas")
	if err != nil || a1.Retried {
		t.Fatalf("first redeem: %+v %v", a1, err)
	}
	open, _ := env.st.JoinTokens(ctx)
	if len(open) != 1 || open[0].AgentID != "ag_first" || open[0].RedeemedAt.IsZero() {
		t.Fatalf("open tokens after first redeem: %+v", open)
	}
	a2, err := env.st.RedeemJoinToken(ctx, hash, "ag_second", "nas.lan")
	if err != nil || a2.ID != "ag_first" || !a2.Retried || a2.Hostname != "nas.lan" {
		t.Fatalf("retry: %+v %v", a2, err)
	}
	if err := env.st.ConfirmAgentPaired(ctx, "ag_first"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.st.RedeemJoinToken(ctx, hash, "ag_third", "nas"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("token after the first connection: %v", err)
	}
	if open, _ := env.st.JoinTokens(ctx); len(open) != 0 {
		t.Fatalf("token still open after connect: %+v", open)
	}

	// Cancelling an open, used-but-never-connected token removes the dead agent too.
	tok2, _, _ := env.hub.NewJoinToken(ctx, "pi")
	s2, _, _ := ParseJoinToken(tok2)
	if _, err := env.st.RedeemJoinToken(ctx, HashSecret(s2), "ag_pi", "pi"); err != nil {
		t.Fatal(err)
	}
	open, _ = env.st.JoinTokens(ctx)
	if len(open) != 1 {
		t.Fatalf("open: %+v", open)
	}
	if name, err := env.st.DeleteJoinToken(ctx, open[0].ID); err != nil || name != "pi" {
		t.Fatalf("delete: %q %v", name, err)
	}
	if _, err := env.st.AgentByID(ctx, "ag_pi"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("dead agent kept: %v", err)
	}
	if _, err := env.st.DeleteJoinToken(ctx, open[0].ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

// The agent keeps trying to pair while the hub isn't up yet instead of exiting.
func TestEnrollRetriesUntilHubIsUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	env := startHub(t, ctx)
	tok, _, _ := env.hub.NewJoinToken(ctx, "late")

	// A port where nothing listens for the first seconds, then a proxy to the hub.
	addr := freeAddr(t)
	var accepted atomic.Int32
	go func() {
		time.Sleep(2500 * time.Millisecond)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		go func() { <-ctx.Done(); ln.Close() }()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() {
				up, err := net.Dial("tcp", env.addr)
				if err != nil {
					c.Close()
					return
				}
				go func() { _, _ = io.Copy(up, c); up.Close() }()
				_, _ = io.Copy(c, up)
				c.Close()
			}()
		}
	}()
	agent := &Agent{
		Hub: "https://" + addr + "/", JoinToken: tok, Dir: t.TempDir(),
		Handler: host.AgentHandler(fakeHost{}, false, "test"),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	go agent.Run(ctx) //nolint:errcheck
	e := waitConnectedWithin(t, env.reg, 15*time.Second)
	if e.Name != "late" || accepted.Load() == 0 {
		t.Fatalf("entry %+v accepted=%d", e, accepted.Load())
	}
}

// An agent removed on the hub, with its old certificate still in the volume and a new
// MOORAGE_JOIN_TOKEN set, pairs again on its own after the 403.
func TestReenrollAfterRemovalWithNewToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	env := startHub(t, ctx)
	tok, _, _ := env.hub.NewJoinToken(ctx, "first")
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	actx, acancel := context.WithCancel(ctx)
	agent := &Agent{Hub: env.addr, JoinToken: tok, Dir: dir, Handler: host.AgentHandler(fakeHost{}, false, "test"), Log: log}
	done := make(chan struct{})
	go func() { agent.Run(actx); close(done) }() //nolint:errcheck
	first := waitConnected(t, env.reg)
	acancel()
	<-done

	if err := env.st.DeleteAgent(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	env.reg.Disconnect(first.ID)
	tok2, _, _ := env.hub.NewJoinToken(ctx, "second")
	again := &Agent{Hub: env.addr, JoinToken: tok2, Dir: dir, Handler: host.AgentHandler(fakeHost{}, false, "test"), Log: log}
	go again.Run(ctx) //nolint:errcheck
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		list, _ := env.reg.List(ctx)
		for _, e := range list {
			if e.Connected && e.ID != first.ID && e.Name == "second" {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the removed agent did not pair again with the new token")
}

func waitConnectedWithin(t *testing.T, reg *host.Registry, d time.Duration) host.Entry {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		list, _ := reg.List(context.Background())
		for _, e := range list {
			if e.Connected {
				return e
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("agent did not connect")
	return host.Entry{}
}

// A reconnect attaches the new session before the old one ends: Detach of the old one
// must report that it was replaced (the hub then skips OnDisconnect).
func TestDetachReportsReplacedSession(t *testing.T) {
	reg := host.NewRegistry(nil, "", false, nil)
	oldR := host.NewRemote(func(context.Context) (net.Conn, error) { return nil, errors.New("x") })
	newR := host.NewRemote(func(context.Context) (net.Conn, error) { return nil, errors.New("x") })
	reg.Attach("ag", oldR, host.AgentInfo{}, func() error { return nil })
	reg.Attach("ag", newR, host.AgentInfo{}, func() error { return nil })
	if reg.Detach("ag", oldR) {
		t.Fatal("old session detached the new one")
	}
	if !reg.Detach("ag", newR) {
		t.Fatal("current session not detached")
	}
}
