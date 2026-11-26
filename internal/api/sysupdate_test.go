package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"moorage/internal/auth"
	"moorage/internal/config"
	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/selfupdate"
	"moorage/internal/store"
)

type apiDummyHost struct{}

func (apiDummyHost) Info(context.Context) engine.HostInfo {
	return engine.HostInfo{Name: "local", DockerVersion: "29.8", Reachable: true}
}
func (apiDummyHost) ListStacks(context.Context) ([]engine.StackSummary, error) { return nil, nil }
func (apiDummyHost) GetStack(context.Context, string) (engine.Stack, error) {
	return engine.Stack{}, engine.ErrNotFound
}
func (apiDummyHost) Stats(context.Context, string) ([]engine.Stat, error)             { return nil, nil }
func (apiDummyHost) Validate(context.Context, string, string, string) error           { return nil }
func (apiDummyHost) Save(context.Context, string, string, string, string, bool) error { return nil }
func (apiDummyHost) Delete(context.Context, string, string) error                     { return nil }
func (apiDummyHost) RunAction(context.Context, string, string, string) (engine.OpInfo, error) {
	return engine.OpInfo{}, nil
}
func (apiDummyHost) FollowOp(context.Context, string, func(host.OpEvent) error) error { return nil }
func (apiDummyHost) StreamLogs(context.Context, string, string, int, func(engine.LogLine)) error {
	return nil
}
func (apiDummyHost) WatchLogs(context.Context, string, string, engine.LogFilter, func(engine.LogLine)) error {
	return nil
}
func (apiDummyHost) TailLogs(context.Context, string, int) ([]engine.LogLine, error) { return nil, nil }
func (apiDummyHost) History(context.Context, string) ([]engine.Version, error)       { return nil, nil }
func (apiDummyHost) Version(context.Context, string, string) (engine.VersionDetail, error) {
	return engine.VersionDetail{}, engine.ErrNoHistory
}
func (apiDummyHost) Restore(context.Context, string, string, string) (*engine.Version, error) {
	return nil, nil
}
func (apiDummyHost) CheckImages(context.Context, string) ([]engine.ServiceImage, error) {
	return nil, nil
}
func (apiDummyHost) StackImages(context.Context, string) (map[string]engine.RunningImage, error) {
	return nil, nil
}
func (apiDummyHost) RollbackImages(context.Context, string, map[string]engine.RollbackTarget, string) (engine.OpInfo, error) {
	return engine.OpInfo{}, nil
}
func (apiDummyHost) ChangeImage(context.Context, string, string, string, string, string) (*engine.Version, error) {
	return nil, nil
}
func (apiDummyHost) Terminal(context.Context, string, string, string, int, int) (engine.TerminalSession, engine.Container, error) {
	return nil, engine.Container{}, host.ErrTerminalDisabled
}

func makeAPISignedReleaseServer(t *testing.T, version, minHub, minAgent string, binData []byte) (*httptest.Server, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	binSum := sha256.Sum256(binData)
	binHex := hex.EncodeToString(binSum[:])
	imgDoc := selfupdate.ImagesManifest{
		Version:  version,
		Image:    "ghcr.io/literally-davca/moorage",
		Digests:  map[string]string{"linux/amd64": "sha256:" + binHex},
		MinHub:   minHub,
		MinAgent: minAgent,
	}
	imgBytes, _ := json.Marshal(imgDoc)
	imgSum := sha256.Sum256(imgBytes)
	imgHex := hex.EncodeToString(imgSum[:])
	sums := fmt.Appendf(nil, "%s  images.json\n%s  moorage-linux-amd64\n", imgHex, binHex)
	sig := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sums)) + "\n")

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases":
			json.NewEncoder(w).Encode([]map[string]any{{
				"tag_name": "v" + version,
				"html_url": srv.URL + "/tag",
				"body":     "Changes in " + version,
				"assets": []map[string]string{
					{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/SHA256SUMS"},
					{"name": "SHA256SUMS.sig", "browser_download_url": srv.URL + "/SHA256SUMS.sig"},
					{"name": "images.json", "browser_download_url": srv.URL + "/images.json"},
					{"name": "moorage-linux-amd64", "browser_download_url": srv.URL + "/moorage-linux-amd64"},
				},
			}})
		case "/SHA256SUMS":
			w.Write(sums)
		case "/SHA256SUMS.sig":
			w.Write(sig)
		case "/images.json":
			w.Write(imgBytes)
		case "/moorage-linux-amd64":
			w.Write(binData)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, pub
}

func TestSystemUpdatesRBACGatesAuditAndReconcile(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createTestUserSession(t, db, "admin", "admin", "tok-admin")
	createTestUserSession(t, db, "viewer", "viewer", "tok-viewer")

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := auth.New(db, log)
	if err != nil {
		t.Fatal(err)
	}
	reg := host.NewRegistry(apiDummyHost{}, "hub", false, db)

	newBin := []byte("moorage-hub-0.0.3")
	relSrv, pub := makeAPISignedReleaseServer(t, "0.0.3-test", "0.0.2-test", "", newBin)

	// Pair an online agent over a real loopback AgentHandler to test min_hub gate cleanly.
	_ = db.CreateJoinToken(context.Background(), "h1", "agent-a", time.Now().Add(time.Hour))
	_, _ = db.RedeemJoinToken(context.Background(), "h1", "ag_1", "agent-a.local")
	agentDir := t.TempDir()
	agentExe := filepath.Join(agentDir, "moorage")
	_ = os.WriteFile(agentExe, []byte("agent-0.0.1"), 0o755)
	agentNode := &selfupdate.NodeUpdater{
		DataDir: t.TempDir(), CurrentVersion: "0.0.1-test", SelfUpdate: true, PubKey: pub,
		GOOS: "linux", GOARCH: "amd64",
		Runtime: &selfupdate.RuntimeEnv{
			GOOS: "linux", GOARCH: "amd64",
			Executable:     func() (string, error) { return agentExe, nil },
			ReadFile:       func(string) ([]byte, error) { return nil, os.ErrNotExist },
			Stat:           func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
			Hostname:       func() (string, error) { return "agent-a", nil },
			CheckWritable:  func(string) error { return nil },
			SelfUpdateFlag: true,
		},
	}
	agLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	agSrv := &http.Server{Handler: host.NewAgentHandler(apiDummyHost{}, host.AgentOptions{Version: "0.0.1-test", SelfUpdate: agentNode})}
	go func() { _ = agSrv.Serve(agLn) }()
	t.Cleanup(func() { _ = agSrv.Close(); _ = agLn.Close() })
	agAddr := agLn.Addr().String()
	agRM := host.NewRemote(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", agAddr)
	})
	reg.Attach("ag_1", agRM, host.AgentInfo{HostInfo: apiDummyHost{}.Info(context.Background()), Version: "0.0.1-test"}, nil)

	binDir := filepath.Join(dir, "bin")
	_ = os.MkdirAll(binDir, 0o755)
	exePath := filepath.Join(binDir, "moorage")
	_ = os.WriteFile(exePath, []byte("moorage-hub-0.0.1"), 0o755)

	cfg := config.Config{DataDir: dir, UpdateURL: relSrv.URL + "/releases", UpdateChannel: "pre", SelfUpdate: false}
	node := &selfupdate.NodeUpdater{
		DataDir:        dir,
		CurrentVersion: "0.0.1-test",
		SelfUpdate:     false,
		IsHub:          true,
		PubKey:         pub,
		GOOS:           "linux",
		GOARCH:         "amd64",
		Runtime: &selfupdate.RuntimeEnv{
			GOOS: "linux", GOARCH: "amd64",
			Executable:     func() (string, error) { return exePath, nil },
			ReadFile:       func(string) ([]byte, error) { return nil, os.ErrNotExist },
			Stat:           func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
			Hostname:       func() (string, error) { return "hub", nil },
			CheckWritable:  func(string) error { return nil },
			SelfUpdateFlag: false,
		},
	}

	srv := New(cfg, a, db, reg, nil, nil, log, "0.0.1-test")
	su := selfupdate.NewService(cfg, "0.0.1-test", true, db, reg, nil, nil, node, func(c context.Context, e store.AuditEntry) {
		_ = db.Audit(c, e)
	}, log)
	su.PubKey = pub
	su.Checker.PubKey = pub
	su.Checker.Client = relSrv.Client()
	su.StatusTTL = time.Nanosecond // the test flips MOORAGE_SELF_UPDATE between requests
	srv.SetSelfUpdate(su)
	h := srv.Handler()

	req := func(method, path, body, tok string) *httptest.ResponseRecorder {
		hdr := map[string]string{csrfHeader: "1", "Content-Type": "application/json", "Cookie": cookieName + "=" + tok}
		return do(h, method, path, body, hdr)
	}

	// 1. Viewer 403 on all system update routes.
	for _, tc := range []struct{ m, p, b string }{
		{"GET", "/api/system/updates", ""},
		{"POST", "/api/system/updates/check", "{}"},
		{"POST", "/api/system/updates/apply", `{"targets":["hub"]}`},
		{"POST", "/api/system/updates/hub/rollback", "{}"},
	} {
		rec := req(tc.m, tc.p, tc.b, "tok-viewer")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s as viewer = %d, want 403", tc.m, tc.p, rec.Code)
		}
	}

	// 2. Admin Check writes update.check audit row.
	if rec := req("POST", "/api/system/updates/check", "{}", "tok-admin"); rec.Code != http.StatusOK {
		t.Fatalf("admin check = %d: %s", rec.Code, rec.Body.String())
	}

	// 3. min_hub gate blocks agent update (409) without writing an audit row.
	recGate := req("POST", "/api/system/updates/apply", `{"targets":["ag_1"]}`, "tok-admin")
	if recGate.Code != http.StatusConflict || !strings.Contains(recGate.Body.String(), "update the hub first") {
		t.Fatalf("expected 409 min_hub gate, got %d: %s", recGate.Code, recGate.Body.String())
	}

	// 4. MOORAGE_SELF_UPDATE=false blocks hub apply (403) without writing an audit row.
	recDisabled := req("POST", "/api/system/updates/apply", `{"targets":["hub"]}`, "tok-admin")
	if recDisabled.Code != http.StatusForbidden {
		t.Fatalf("expected 403 disabled self-update, got %d: %s", recDisabled.Code, recDisabled.Body.String())
	}

	rows, _ := db.AuditEntries(context.Background(), store.AuditFilter{Limit: 50})
	if len(rows) != 1 || rows[0].Action != "update.check" {
		t.Fatalf("expected only update.check in audit log after refused applies, got %+v", rows)
	}

	// 5. Enable MOORAGE_SELF_UPDATE=true, apply hub update, check audit update.apply + SQLite restart state.
	node.SelfUpdate = true
	node.ShutdownWait = 5 * time.Millisecond
	node.SpawnWatchFn = func(_, _ string) error { return nil }
	// On Windows the clock advances in ticks of up to ~16 ms, so with StatusTTL = 1 ns the
	// status cached by step 4 can still look fresh; wait one tick before asking again.
	time.Sleep(20 * time.Millisecond)
	recApply := req("POST", "/api/system/updates/apply", `{"targets":["hub"]}`, "tok-admin")
	if recApply.Code != http.StatusAccepted {
		t.Fatalf("expected 202 apply hub, got %d: %s", recApply.Code, recApply.Body.String())
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m, _ := db.LatestSystemUpdateByNode(context.Background())
		if m["hub"].Status == store.SysUpdateRestarting {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	m, _ := db.LatestSystemUpdateByNode(context.Background())
	if m["hub"].Status != store.SysUpdateRestarting {
		t.Fatalf("expected SQLite hub job status restarting, got %+v", m["hub"])
	}

	// 6. Simulate hub reboot onto 0.0.3-test -> ReconcileAfterRestart records done + update.done audit row!
	cfg2 := cfg
	cfg2.SelfUpdate = true
	su2 := selfupdate.NewService(cfg2, "0.0.3-test", true, db, reg, nil, nil, nil, func(c context.Context, e store.AuditEntry) {
		_ = db.Audit(c, e)
	}, log)
	su2.HubConfirmAfter = 50 * time.Millisecond
	su2.ReconcileAfterRestart(context.Background())
	// Not done before the new hub serves: a crash now must still roll back.
	time.Sleep(100 * time.Millisecond)
	if m, _ := db.LatestSystemUpdateByNode(context.Background()); m["hub"].Status != store.SysUpdateRestarting {
		t.Fatalf("hub job settled before serving: %+v", m["hub"])
	}
	su2.MarkServing()
	deadline = time.Now().Add(3 * time.Second)
	var m2 map[string]store.SystemUpdate
	for time.Now().Before(deadline) {
		m2, _ = db.LatestSystemUpdateByNode(context.Background())
		if m2["hub"].Status == store.SysUpdateDone {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if m2["hub"].Status != store.SysUpdateDone {
		t.Fatalf("expected reconciled status done, got %+v", m2["hub"])
	}
	// The pending state was finalised into a kept previous version.
	if _, err := os.Stat(filepath.Join(dir, "self-update-pending.json")); !os.IsNotExist(err) {
		t.Fatalf("pending state should be finalised, stat err=%v", err)
	}

	// 7. Also simulate a rolled-back restart reconciliation!
	_, _ = db.CreateSystemUpdate(context.Background(), store.SystemUpdate{
		NodeID: "hub", NodeName: "Hub", Action: store.SysUpdateActionApply,
		FromVersion: "0.0.3-test", ToVersion: "0.0.4-test", Mode: "binary", Status: store.SysUpdateRestarting, Actor: "admin", IP: "127.0.0.1",
	})
	_ = os.WriteFile(filepath.Join(dir, "self-update-rollback.json"), []byte(`{"reason":"rolled back: health timeout"}`), 0o600)
	su3 := selfupdate.NewService(cfg2, "0.0.3-test", true, db, reg, nil, nil, nil, func(c context.Context, e store.AuditEntry) {
		_ = db.Audit(c, e)
	}, log)
	su3.ReconcileAfterRestart(context.Background())
	m3, _ := db.LatestSystemUpdateByNode(context.Background())
	if m3["hub"].Status != store.SysUpdateRolledBack {
		t.Fatalf("expected reconciled status rolled-back, got %+v", m3["hub"])
	}

	allRows, _ := db.AuditEntries(context.Background(), store.AuditFilter{Limit: 50})
	actions := map[string]bool{}
	for _, r := range allRows {
		actions[r.Action] = true
	}
	for _, want := range []string{"update.check", "update.apply", "update.done", "update.rolled_back"} {
		if !actions[want] {
			t.Errorf("missing audit action %q in %+v", want, actions)
		}
	}
}
