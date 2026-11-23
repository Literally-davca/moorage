package selfupdate

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"moorage/internal/config"
	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/monitor"
	"moorage/internal/store"
)

type stubHost struct {
	ver string
	ro  bool
}

func (s stubHost) Info(context.Context) engine.HostInfo {
	return engine.HostInfo{Name: "stub", DockerVersion: "29.8", Reachable: true}
}
func (s stubHost) ListStacks(context.Context) ([]engine.StackSummary, error) { return nil, nil }
func (s stubHost) GetStack(context.Context, string) (engine.Stack, error) {
	return engine.Stack{}, engine.ErrNotFound
}
func (s stubHost) Stats(context.Context, string) ([]engine.Stat, error)   { return nil, nil }
func (s stubHost) Validate(context.Context, string, string, string) error { return nil }
func (s stubHost) Save(context.Context, string, string, string, string, bool) error {
	return nil
}
func (s stubHost) Delete(context.Context, string, string) error { return nil }
func (s stubHost) RunAction(context.Context, string, string, string) (engine.OpInfo, error) {
	return engine.OpInfo{}, nil
}
func (s stubHost) FollowOp(context.Context, string, func(host.OpEvent) error) error { return nil }
func (s stubHost) StreamLogs(context.Context, string, string, int, func(engine.LogLine)) error {
	return nil
}
func (s stubHost) WatchLogs(context.Context, string, string, engine.LogFilter, func(engine.LogLine)) error {
	return nil
}
func (s stubHost) TailLogs(context.Context, string, int) ([]engine.LogLine, error) { return nil, nil }
func (s stubHost) History(context.Context, string) ([]engine.Version, error)       { return nil, nil }
func (s stubHost) Version(context.Context, string, string) (engine.VersionDetail, error) {
	return engine.VersionDetail{}, engine.ErrNoHistory
}
func (s stubHost) Restore(context.Context, string, string, string) (*engine.Version, error) {
	return nil, nil
}
func (s stubHost) CheckImages(context.Context, string) ([]engine.ServiceImage, error) {
	return nil, nil
}
func (s stubHost) StackImages(context.Context, string) (map[string]engine.RunningImage, error) {
	return nil, nil
}
func (s stubHost) RollbackImages(context.Context, string, map[string]engine.RollbackTarget, string) (engine.OpInfo, error) {
	return engine.OpInfo{}, nil
}
func (s stubHost) ChangeImage(context.Context, string, string, string, string, string) (*engine.Version, error) {
	return nil, nil
}
func (s stubHost) Terminal(context.Context, string, string, string, int, int) (engine.TerminalSession, engine.Container, error) {
	return nil, engine.Container{}, host.ErrTerminalDisabled
}

func startRemoteAgentHandler(t *testing.T, h http.Handler) *host.Remote {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})
	addr := ln.Addr().String()
	return host.NewRemote(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	})
}

func seedAgent(t *testing.T, st *store.Store, id, name string) {
	t.Helper()
	tokHash := id + "-tok-hash"
	if err := st.CreateJoinToken(context.Background(), tokHash, name, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RedeemJoinToken(context.Background(), tokHash, id, name+".local"); err != nil {
		t.Fatal(err)
	}
}

func TestAgentRefusesUnsignedAndMismatchedArtifactsEvenFromHub(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	dataDir := filepath.Join(dir, "data")
	_ = os.MkdirAll(binDir, 0o755)
	exePath := filepath.Join(binDir, "moorage")
	origBytes := []byte("agent-0.0.1")
	_ = os.WriteFile(exePath, origBytes, 0o755)

	newBytes := []byte("agent-0.0.2")
	fx := makeSignedFixture(t, "0.0.2-test", "moorage-linux-amd64", "linux/amd64", newBytes, "", "")
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)

	re := RuntimeEnv{
		GOOS:           "linux",
		GOARCH:         "amd64",
		Executable:     func() (string, error) { return exePath, nil },
		ReadFile:       func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Stat:           func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		Hostname:       func() (string, error) { return "agent1", nil },
		CheckWritable:  probeDirWritable,
		SelfUpdateFlag: true,
	}

	// Read-only agent WITH MOORAGE_SELF_UPDATE=true, pinned to fx.pub.
	node := &NodeUpdater{
		DataDir:        dataDir,
		CurrentVersion: "0.0.1-test",
		SelfUpdate:     true,
		PubKey:         fx.pub,
		GOOS:           "linux",
		GOARCH:         "amd64",
		Runtime:        &re,
		SpawnWatchFn:   func(_, _ string) error { return nil },
		ExecFn:         func(string, []string, []string) error { return nil },
	}
	rm := startRemoteAgentHandler(t, host.NewAgentHandler(stubHost{}, host.AgentOptions{
		ReadOnly:   true,
		Version:    "0.0.1-test",
		SelfUpdate: node,
	}))

	// 1. Wrong-key manifest pushed by a compromised hub -> refused!
	wrongFX := makeSignedFixture(t, "0.0.2-test", "moorage-linux-amd64", "linux/amd64", newBytes, "", "")
	if err := rm.ApplyBinaryUpdate(context.Background(), wrongFX.manifest, bytes.NewReader(newBytes)); !errors.Is(err, host.ErrSelfUpdateInvalid) {
		t.Fatalf("expected agent to refuse wrong-key manifest from hub, got %v", err)
	}
	if !bytes.Equal(mustRead(t, exePath), origBytes) {
		t.Fatal("agent binary modified on wrong-key manifest")
	}

	// 2. Good signature but tampered binary bytes sent by compromised hub -> refused!
	if err := rm.ApplyBinaryUpdate(context.Background(), fx.manifest, bytes.NewReader([]byte("evil-binary"))); !errors.Is(err, host.ErrSelfUpdateInvalid) {
		t.Fatalf("expected agent to refuse tampered binary from hub, got %v", err)
	}
	if !bytes.Equal(mustRead(t, exePath), origBytes) {
		t.Fatal("agent binary modified on tampered payload")
	}

	// 3. Container mode: compromised hub supplies unsigned image digest -> refused!
	targetID := "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	eng := newFakeContainerEngine()
	eng.addContainer(sampleTargetInspect(targetID))
	reCtr := RuntimeEnv{
		GOOS:   "linux",
		GOARCH: "amd64",
		Stat: func(p string) (os.FileInfo, error) {
			if p == "/.dockerenv" {
				return os.Stat(dir)
			}
			return nil, os.ErrNotExist
		},
		ReadFile: func(p string) ([]byte, error) {
			if p == "/proc/self/mountinfo" {
				return []byte("1 1 0:1 / /var/lib/docker/containers/" + targetID + "/resolv.conf rw\n"), nil
			}
			return nil, os.ErrNotExist
		},
		Hostname:       func() (string, error) { return targetID[:12], nil },
		SelfUpdateFlag: true,
	}
	nodeCtr := &NodeUpdater{
		DataDir:        t.TempDir(),
		CurrentVersion: "0.0.1-test",
		SelfUpdate:     true,
		Eng:            eng,
		PubKey:         fx.pub,
		GOOS:           "linux",
		GOARCH:         "amd64",
		Runtime:        &reCtr,
	}
	rmCtr := startRemoteAgentHandler(t, host.NewAgentHandler(stubHost{}, host.AgentOptions{
		ReadOnly:   true,
		Version:    "0.0.1-test",
		SelfUpdate: nodeCtr,
	}))
	badCtrManifest := fx.manifest
	badCtrManifest.Digest = "sha256:badbadbadbad"
	if err := rmCtr.ApplyContainerUpdate(context.Background(), badCtrManifest); !errors.Is(err, host.ErrSelfUpdateInvalid) {
		t.Fatalf("expected agent to refuse mismatched container digest from hub, got %v", err)
	}

	// 4. MOORAGE_SELF_UPDATE=false enforced by agent itself -> refused!
	nodeOff := &NodeUpdater{
		DataDir:        t.TempDir(),
		CurrentVersion: "0.0.1-test",
		SelfUpdate:     false,
		PubKey:         otherPub,
		GOOS:           "linux",
		GOARCH:         "amd64",
		Runtime:        &re,
	}
	rmOff := startRemoteAgentHandler(t, host.NewAgentHandler(stubHost{}, host.AgentOptions{
		ReadOnly:   false,
		Version:    "0.0.1-test",
		SelfUpdate: nodeOff,
	}))
	if err := rmOff.ApplyBinaryUpdate(context.Background(), fx.manifest, bytes.NewReader(newBytes)); !errors.Is(err, host.ErrSelfUpdateDisabled) {
		t.Fatalf("expected MOORAGE_SELF_UPDATE=false refusal on agent, got %v", err)
	}

	// 5. Read-only agent WITH MOORAGE_SELF_UPDATE=true IS allowed to apply valid signed update!
	if err := rm.ApplyBinaryUpdate(context.Background(), fx.manifest, bytes.NewReader(newBytes)); err != nil {
		t.Fatalf("expected read-only agent with MOORAGE_SELF_UPDATE=true to allow signed self-update, got %v", err)
	}
	if !bytes.Equal(mustRead(t, exePath), newBytes) {
		t.Fatal("read-only agent did not apply valid signed update")
	}
}

func TestSequentialUpdateAllStopsOnFirstFailureSilencesAlertsAndOlderAgents(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	reg := host.NewRegistry(stubHost{}, "hub", false, st)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mon := monitor.New(st, reg, log, "")

	chID, err := st.SaveChannel(context.Background(), store.Channel{
		Name: "wd", Kind: "webhook", Config: `{"url":"https://example.invalid"}`, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	seedAgent(t, st, "ag1", "alpha")
	seedAgent(t, st, "ag2", "beta")
	seedAgent(t, st, "ag3", "gamma")
	seedAgent(t, st, "ag_old", "legacy")
	_ = st.SaveWatchdog(context.Background(), store.WatchdogSettings{Enabled: true, ChannelID: chID, Grace: 10}, map[string]bool{"ag1": true})

	bin002 := []byte("moorage-bin-0.0.2")
	fx := makeSignedFixture(t, "0.0.2-test", "moorage-linux-amd64", "linux/amd64", bin002, "", "")

	// Fake release HTTP server for Checker.
	var relSrv *httptest.Server
	relSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases":
			json.NewEncoder(w).Encode([]map[string]any{{
				"tag_name": "v0.0.2-test",
				"assets": []map[string]string{
					{"name": "SHA256SUMS", "browser_download_url": relSrv.URL + "/SHA256SUMS"},
					{"name": "SHA256SUMS.sig", "browser_download_url": relSrv.URL + "/SHA256SUMS.sig"},
					{"name": "images.json", "browser_download_url": relSrv.URL + "/images.json"},
					{"name": "moorage-linux-amd64", "browser_download_url": relSrv.URL + "/moorage-linux-amd64"},
				},
			}})
		case "/SHA256SUMS":
			w.Write(fx.sums)
		case "/SHA256SUMS.sig":
			w.Write(fx.sigB64)
		case "/images.json":
			w.Write(fx.imagesJSON)
		case "/moorage-linux-amd64":
			w.Write(bin002)
		}
	}))
	defer relSrv.Close()

	var sysSvc *Service

	// Agent 1: binary mode, simulates reconnect as 0.0.2-test shortly after accepting!
	dir1 := t.TempDir()
	exe1 := filepath.Join(dir1, "moorage")
	_ = os.WriteFile(exe1, []byte("old-1"), 0o755)
	re1 := RuntimeEnv{
		GOOS: "linux", GOARCH: "amd64",
		Executable:    func() (string, error) { return exe1, nil },
		ReadFile:      func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Stat:          func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		Hostname:      func() (string, error) { return "alpha", nil },
		CheckWritable: probeDirWritable, SelfUpdateFlag: true,
	}
	var rm1 *host.Remote
	n1 := &NodeUpdater{
		DataDir: t.TempDir(), CurrentVersion: "0.0.1-test", SelfUpdate: true, PubKey: fx.pub,
		GOOS: "linux", GOARCH: "amd64", Runtime: &re1,
		ShutdownWait: 10 * time.Millisecond,
		SpawnWatchFn: func(_, _ string) error { return nil },
		ShutdownFn: func() {
			reg.Detach("ag1", rm1)
			mon.CheckWatchdogAgents(context.Background(), time.Now().Add(time.Minute))
			reg.Attach("ag1", rm1, host.AgentInfo{HostInfo: stubHost{}.Info(context.Background()), Version: "0.0.2-test"}, nil)
			sysSvc.OnAgentConnect("ag1")
		},
	}
	rm1 = startRemoteAgentHandler(t, host.NewAgentHandler(stubHost{}, host.AgentOptions{Version: "0.0.1-test", SelfUpdate: n1}))
	reg.Attach("ag1", rm1, host.AgentInfo{HostInfo: stubHost{}.Info(context.Background()), Version: "0.0.1-test"}, nil)

	// Agent 2: binary mode, FAILS during transfer (tampered/unreachable or wrong pubkey -> agent rejects).
	dir2 := t.TempDir()
	exe2 := filepath.Join(dir2, "moorage")
	_ = os.WriteFile(exe2, []byte("old-2"), 0o755)
	re2 := re1
	re2.Executable = func() (string, error) { return exe2, nil }
	wrongPub, _, _ := ed25519.GenerateKey(rand.Reader)
	n2 := &NodeUpdater{
		DataDir: t.TempDir(), CurrentVersion: "0.0.1-test", SelfUpdate: true, PubKey: wrongPub,
		GOOS: "linux", GOARCH: "amd64", Runtime: &re2,
	}
	rm2 := startRemoteAgentHandler(t, host.NewAgentHandler(stubHost{}, host.AgentOptions{Version: "0.0.1-test", SelfUpdate: n2}))
	reg.Attach("ag2", rm2, host.AgentInfo{HostInfo: stubHost{}.Info(context.Background()), Version: "0.0.1-test"}, nil)

	// Agent 3: healthy agent that MUST NOT be touched once ag2 fails!
	dir3 := t.TempDir()
	exe3 := filepath.Join(dir3, "moorage")
	_ = os.WriteFile(exe3, []byte("old-3"), 0o755)
	re3 := re1
	re3.Executable = func() (string, error) { return exe3, nil }
	var shutdown3Called atomic.Bool
	n3 := &NodeUpdater{
		DataDir: t.TempDir(), CurrentVersion: "0.0.1-test", SelfUpdate: true, PubKey: fx.pub,
		GOOS: "linux", GOARCH: "amd64", Runtime: &re3,
		ShutdownFn: func() { shutdown3Called.Store(true) },
	}
	rm3 := startRemoteAgentHandler(t, host.NewAgentHandler(stubHost{}, host.AgentOptions{Version: "0.0.1-test", SelfUpdate: n3}))
	reg.Attach("ag3", rm3, host.AgentInfo{HostInfo: stubHost{}.Info(context.Background()), Version: "0.0.1-test"}, nil)

	// Agent ag_old: older agent without /system/update/status (returns 404).
	rmOld := startRemoteAgentHandler(t, host.NewAgentHandler(stubHost{}, host.AgentOptions{Version: "0.0.0-old"}))
	reg.Attach("ag_old", rmOld, host.AgentInfo{HostInfo: stubHost{}.Info(context.Background()), Version: "0.0.0-old"}, nil)

	cfg := config.Config{DataDir: t.TempDir(), UpdateURL: relSrv.URL + "/releases", UpdateChannel: "pre", SelfUpdate: false}
	sysSvc = NewService(cfg, "0.0.2-test", false, st, reg, mon, nil, nil, nil, log)
	sysSvc.PubKey = fx.pub
	sysSvc.Checker.PubKey = fx.pub
	sysSvc.Checker.Client = relSrv.Client()
	sysSvc.HealthTimeout = 5 * time.Second

	ov, err := sysSvc.CheckNow(context.Background())
	if err != nil || ov.Release == nil {
		t.Fatalf("CheckNow failed: err=%v ov=%+v", err, ov)
	}

	// Verify older agent reported manual command.
	var foundOld bool
	for _, n := range ov.Nodes {
		if n.ID == "ag_old" {
			foundOld = true
			if !n.OlderAgent || !strings.Contains(n.ManualCommand, "0.0.2-test") {
				t.Fatalf("unexpected older agent node: %+v", n)
			}
		}
	}
	if !foundOld {
		t.Fatal("missing ag_old in overview")
	}

	// Start update on ag1, ag2, ag3 sequentially -> ag1 succeeds, ag2 fails, ag3 skipped!
	if _, _, _, err := sysSvc.Apply(context.Background(), []string{"ag1", "ag2", "ag3"}, "admin", "127.0.0.1"); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m, _ := st.LatestSystemUpdateByNode(context.Background())
		if m["ag1"].Status == store.SysUpdateDone && m["ag2"].Status == store.SysUpdateFailed && m["ag3"].Status == store.SysUpdateFailed {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	m, _ := st.LatestSystemUpdateByNode(context.Background())
	if m["ag1"].Status != store.SysUpdateDone {
		t.Fatalf("ag1 want done, got %+v", m["ag1"])
	}
	if m["ag2"].Status != store.SysUpdateFailed {
		t.Fatalf("ag2 want failed, got %+v", m["ag2"])
	}
	if m["ag3"].Status != store.SysUpdateFailed || !strings.HasPrefix(m["ag3"].Message, "Skipped:") {
		t.Fatalf("ag3 should stop/skip on first failure, got %+v", m["ag3"])
	}
	if shutdown3Called.Load() {
		t.Fatal("ag3 should never have started shutting down after ag2 failed")
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
