package tunnel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/store"
)

// fakeHost answers like an engine without Docker.
type fakeHost struct{}

func (fakeHost) Info(context.Context) engine.HostInfo {
	return engine.HostInfo{Name: "pi", DockerVersion: "29.8.0", Reachable: true}
}
func (fakeHost) ListStacks(context.Context) ([]engine.StackSummary, error) {
	return []engine.StackSummary{{Name: "web", Managed: true, Status: "up", Running: 1, Total: 1}}, nil
}
func (fakeHost) GetStack(_ context.Context, n string) (engine.Stack, error) {
	if n != "web" {
		return engine.Stack{}, engine.ErrNotFound
	}
	return engine.Stack{StackSummary: engine.StackSummary{Name: "web"}, Compose: "services: {}\n"}, nil
}
func (fakeHost) Stats(context.Context, string) ([]engine.Stat, error) { return nil, nil }
func (fakeHost) Validate(_ context.Context, _, c, _ string) error {
	if c == "" {
		return engine.ValidationError{Msg: "Compose file is empty."}
	}
	return nil
}
func (fakeHost) Save(context.Context, string, string, string, string, bool) error { return nil }
func (fakeHost) Delete(context.Context, string, string) error                     { return nil }
func (fakeHost) RunAction(_ context.Context, n, a, _ string) (engine.OpInfo, error) {
	return engine.OpInfo{ID: "1-1", Stack: n, Action: a}, nil
}
func (fakeHost) FollowOp(_ context.Context, id string, emit func(host.OpEvent) error) error {
	if id != "1-1" {
		return host.ErrOpNotFound
	}
	if err := emit(host.OpEvent{Lines: []string{"Container web-1 Started"}}); err != nil {
		return err
	}
	return emit(host.OpEvent{Lines: []string{"✓ up finished"}, Done: true})
}
func (fakeHost) StreamLogs(_ context.Context, _, _ string, _ int, emit func(engine.LogLine)) error {
	for i := range 3 {
		emit(engine.LogLine{Container: "web", Message: "line " + string(rune('a'+i))})
	}
	return nil
}
func (fakeHost) WatchLogs(_ context.Context, _, _ string, f engine.LogFilter, emit func(engine.LogLine)) error {
	match, err := f.Compile()
	if err != nil {
		return err
	}
	for _, msg := range []string{"tick 1", "FATAL: disk full", "tick 2", "fatal: permission denied"} {
		if match(msg) {
			emit(engine.LogLine{Container: "web", Message: msg})
		}
	}
	return nil
}
func (fakeHost) TailLogs(context.Context, string, int) ([]engine.LogLine, error) {
	return []engine.LogLine{{Container: "web", Message: "boom"}}, nil
}
func (fakeHost) History(context.Context, string) ([]engine.Version, error) { return nil, nil }
func (fakeHost) Version(context.Context, string, string) (engine.VersionDetail, error) {
	return engine.VersionDetail{}, engine.ErrNoHistory
}
func (fakeHost) Restore(context.Context, string, string, string) (*engine.Version, error) {
	return nil, nil
}
func (fakeHost) CheckImages(context.Context, string) ([]engine.ServiceImage, error) {
	return []engine.ServiceImage{{Service: "web", Ref: "nginx:1.27", Status: engine.ImageUpToDate}}, nil
}
func (fakeHost) StackImages(context.Context, string) (map[string]engine.RunningImage, error) {
	return map[string]engine.RunningImage{"web": {Service: "web", Ref: "nginx:1.27", ImageID: "sha256:111"}}, nil
}
func (fakeHost) RollbackImages(_ context.Context, n string, _ map[string]engine.RollbackTarget, _ string) (engine.OpInfo, error) {
	return engine.OpInfo{ID: "1-1", Stack: n, Action: "rollback"}, nil
}
func (fakeHost) ChangeImage(context.Context, string, string, string, string, string) (*engine.Version, error) {
	return &engine.Version{Short: "abc1234", Message: "Change web image"}, nil
}
func (fakeHost) Terminal(context.Context, string, string, string, int, int) (engine.TerminalSession, engine.Container, error) {
	return nil, engine.Container{}, host.ErrTerminalDisabled
}

func freeAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

type hubEnv struct {
	hub  *Hub
	reg  *host.Registry
	st   *store.Store
	addr string
}

func startHub(t *testing.T, ctx context.Context) hubEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ca, err := LoadOrCreateCA(dir + "/ca")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := host.NewRegistry(nil, "", false, st)
	hub := &Hub{CA: ca, St: st, Reg: reg, Log: log}
	addr := freeAddr(t)
	go hub.Serve(ctx, addr) //nolint:errcheck
	time.Sleep(100 * time.Millisecond)
	return hubEnv{hub: hub, reg: reg, st: st, addr: addr}
}

func waitConnected(t *testing.T, reg *host.Registry) host.Entry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
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

func TestPairConnectAndProxy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := startHub(t, ctx)

	tok, _, err := env.hub.NewJoinToken(ctx, "Raspberry Pi")
	if err != nil {
		t.Fatal(err)
	}
	agentDir := t.TempDir()
	agent := &Agent{
		Hub: env.addr, JoinToken: tok, Dir: agentDir,
		Handler: host.AgentHandler(fakeHost{}, true, "test"),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	go agent.Run(ctx) //nolint:errcheck

	e := waitConnected(t, env.reg)
	if e.Name != "Raspberry Pi" || !e.ReadOnly || e.DockerVersion != "29.8.0" {
		t.Fatalf("entry: %+v", e)
	}
	// R1: the hub pairs the agent on its own, so the audit row's actor is "system".
	audit, err := env.st.AuditEntries(ctx, store.AuditFilter{ActionPrefix: "host.paired"})
	if err != nil || len(audit) != 1 || audit[0].Actor != store.AuditActorSystem || audit[0].HostID != e.ID || audit[0].IP == "" {
		t.Fatalf("pairing audit row: %+v (%v)", audit, err)
	}
	h, _, err := env.reg.Get(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}

	stacks, err := h.ListStacks(ctx)
	if err != nil || len(stacks) != 1 || stacks[0].Name != "web" {
		t.Fatalf("stacks: %+v %v", stacks, err)
	}
	if _, err := h.GetStack(ctx, "nope"); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("missing stack should map to ErrNotFound, got %v", err)
	}
	var ve engine.ValidationError
	if err := h.Validate(ctx, "web", "", ""); !errors.As(err, &ve) || ve.Msg != "Compose file is empty." {
		t.Fatalf("validation error lost in transit: %v", err)
	}
	// read-only is enforced by the agent itself, not only by the hub
	if err := h.Save(ctx, "web", "services: {}", "", "ann", false); !errors.Is(err, host.ErrHostReadOnly) {
		t.Fatalf("read-only agent accepted a save: %v", err)
	}
	if _, err := h.RunAction(ctx, "web", "up", "ann"); !errors.Is(err, host.ErrHostReadOnly) {
		t.Fatalf("read-only agent accepted an action: %v", err)
	}

	var lines []string
	if err := h.StreamLogs(ctx, "web", "", 10, func(l engine.LogLine) { lines = append(lines, l.Message) }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, ",") != "line a,line b,line c" {
		t.Fatalf("logs: %v", lines)
	}
	var watched []string
	if err := h.WatchLogs(ctx, "web", "", engine.LogFilter{Pattern: "FATAL", CaseSensitive: true}, func(l engine.LogLine) {
		watched = append(watched, l.Message)
	}); err != nil {
		t.Fatal(err)
	}
	if len(watched) != 1 || watched[0] != "FATAL: disk full" {
		t.Fatalf("watch logs: %v", watched)
	}
	var out []string
	err = h.FollowOp(ctx, "1-1", func(ev host.OpEvent) error { out = append(out, ev.Lines...); return nil })
	if err != nil || len(out) != 2 {
		t.Fatalf("op: %v %v", out, err)
	}

	// the token was single-use
	other := &Agent{Hub: env.addr, JoinToken: tok, Dir: t.TempDir(), Handler: host.AgentHandler(fakeHost{}, false, "test"), Log: agent.Log}
	if err := other.Run(ctx); err == nil || !strings.Contains(err.Error(), "unknown, used or expired") {
		t.Fatalf("reused token: %v", err)
	}

	// removing the agent drops the connection and refuses reconnects
	if err := env.st.DeleteAgent(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	env.reg.Disconnect(e.ID)
	time.Sleep(1500 * time.Millisecond)
	if _, _, err := env.reg.Get(ctx, e.ID); !errors.Is(err, host.ErrUnknownHost) {
		t.Fatalf("removed agent still reachable: %v", err)
	}
}

func TestEnrollRejectsWrongHub(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := startHub(t, ctx)
	tok, _, _ := env.hub.NewJoinToken(ctx, "x")
	// same secret, fingerprint of a different CA: the agent must refuse this hub
	otherCA, _ := LoadOrCreateCA(t.TempDir())
	parts := strings.Split(tok, ".")
	forged := parts[0] + "." + parts[1] + "." + otherCA.Fingerprint()
	a := &Agent{Hub: env.addr, JoinToken: forged, Dir: t.TempDir(), Handler: host.AgentHandler(fakeHost{}, false, "t"),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	err := a.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "doesn't match the join token") {
		t.Fatalf("agent trusted a hub with another CA: %v", err)
	}
	if _, _, err := ParseJoinToken("mj1.short.x"); err == nil {
		t.Fatal("malformed token accepted")
	}
}
