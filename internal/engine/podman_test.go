package engine

// Tests for Podman support against a fake compat API. The payloads in testdata/podman are
// synthetic: written by hand from the Podman compat API docs and the compat handlers in the
// Podman source (v4.9 and main), not recorded from a real Podman. Replace them with real
// recordings (`curl --unix-socket $SOCK http://d/v1.41/version`) when a Podman host is at hand.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"moorage/internal/config"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "podman", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeCompat serves a few compat API endpoints from fixtures.
type fakeCompat struct {
	t        *testing.T
	version  string // fixture names
	info     string
	stats    []string // replies of successive stats calls, the last one repeats
	statsErr string   // when set, stats answer 500 with this message
	logs     func(w http.ResponseWriter, r *http.Request)

	statCalls atomic.Int32
	mu        sync.Mutex
	statQuery []string
}

func (f *fakeCompat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if i := strings.Index(p[1:], "/"); strings.HasPrefix(p, "/v1.") && i > 0 {
		p = p[i+1:] // drop the /v1.41 prefix
	}
	jsonReply := func(b []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}
	notFound := func(msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"cause":"%s","message":"%s","response":404}`, msg, msg)
	}
	switch {
	case p == "/version":
		jsonReply(fixture(f.t, f.version))
	case p == "/info":
		jsonReply(fixture(f.t, f.info))
	case p == "/containers/json":
		if !strings.Contains(r.URL.Query().Get("filters"), labelProject) {
			f.t.Errorf("container list without the compose project label filter: %q", r.URL.RawQuery)
		}
		jsonReply(fixture(f.t, "containers_podman49.json"))
	case strings.HasSuffix(p, "/stats"):
		f.mu.Lock()
		f.statQuery = append(f.statQuery, r.URL.RawQuery)
		f.mu.Unlock()
		if f.statsErr != "" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprintf(w, `{"cause":"x","message":"%s","response":500}`, f.statsErr)
			return
		}
		n := int(f.statCalls.Add(1)) - 1
		jsonReply(fixture(f.t, f.stats[min(n, len(f.stats)-1)]))
	case strings.HasSuffix(p, "/logs") && f.logs != nil:
		f.logs(w, r)
	case strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
		jsonReply(fixture(f.t, "inspect_podman_web.json"))
	case strings.HasPrefix(p, "/exec/"):
		notFound("no such exec session")
	case strings.HasPrefix(p, "/distribution/"):
		notFound("page not found") // Podman before the compat distribution endpoint
	default:
		notFound("page not found")
	}
}

// newCompatEngine returns an Engine talking to a fake compat API.
func newCompatEngine(t *testing.T, f *fakeCompat) *Engine {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	api, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithAPIVersion("1.41"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { api.Close() })
	return &Engine{
		cfg:  config.Config{StacksDir: t.TempDir(), PUID: -1, PGID: -1},
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		api:  api,
		busy: map[string]bool{},
		ops:  newOpRegistry(),
	}
}

func TestEngineDetectionFromVersionPayloads(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	cases := []struct {
		version, info string
		engine, ver   string
		rootless      bool
	}{
		{"version_docker29.json", "info_docker29.json", EngineDocker, "29.0.1", false},
		{"version_podman49_rootful.json", "info_podman49_rootful.json", EnginePodman, "4.9.3", false},
		{"version_podman5_rootless.json", "info_podman5_rootless.json", EnginePodman, "5.4.2", true},
	}
	for _, c := range cases {
		t.Run(c.version, func(t *testing.T) {
			e := newCompatEngine(t, &fakeCompat{version: c.version, info: c.info})
			h := e.Host(context.Background())
			if !h.Reachable || h.Engine != c.engine || h.EngineVersion != c.ver || h.Rootless != c.rootless {
				t.Fatalf("got engine=%q version=%q rootless=%v reachable=%v (%s)", h.Engine, h.EngineVersion, h.Rootless, h.Reachable, h.Error)
			}
			if h.DockerVersion != c.ver {
				t.Fatalf("dockerVersion %q, want the server version %q for older hubs", h.DockerVersion, c.ver)
			}
			if got := e.isPodman(context.Background()); got != (c.engine == EnginePodman) {
				t.Fatalf("isPodman = %v", got)
			}
		})
	}
}

func TestIdentifyEngine(t *testing.T) {
	if id := identifyEngine(client.ServerVersionResult{Version: "4.9.3", Platform: client.PlatformInfo{Name: "Podman Engine"}}); id.Kind != EnginePodman || id.Version != "4.9.3" {
		t.Fatalf("platform name fallback: %+v", id)
	}
	if id := identifyEngine(client.ServerVersionResult{}); id.Kind != EngineDocker {
		t.Fatalf("empty reply must read as Docker: %+v", id)
	}
}

func TestRootlessDetection(t *testing.T) {
	if !isRootless([]string{"name=seccomp,profile=default", "name=rootless"}) {
		t.Fatal("name=rootless missed")
	}
	if !isRootless([]string{"name=seccomp,profile=default,name=rootless"}) {
		t.Fatal("name=rootless inside a combined option missed")
	}
	if isRootless([]string{"name=apparmor", "name=rootlessish"}) {
		t.Fatal("false positive")
	}
	if !rootlessSocket("unix:///run/user/1000/podman/podman.sock") || rootlessSocket("unix:///run/podman/podman.sock") {
		t.Fatal("rootless socket path")
	}
}

func TestPodmanRootlessFromSocketPath(t *testing.T) {
	// Info without name=rootless, but the socket is the user's: still rootless.
	t.Setenv("DOCKER_HOST", "unix:///run/user/1000/podman/podman.sock")
	e := newCompatEngine(t, &fakeCompat{version: "version_podman49_rootful.json", info: "info_podman49_rootful.json"})
	if h := e.Host(context.Background()); !h.Rootless {
		t.Fatal("rootless socket path not used as fallback")
	}
}

func TestPodmanStatsRootful(t *testing.T) {
	podmanStatsGap = time.Millisecond
	defer func() { podmanStatsGap = time.Second }()
	f := &fakeCompat{version: "version_podman49_rootful.json", info: "info_podman49_rootful.json",
		stats: []string{"stats_podman49_rootful_1.json", "stats_podman49_rootful_2.json"}}
	e := newCompatEngine(t, f)
	st, err := e.Stats(context.Background(), "web")
	if err != nil || len(st) != 1 {
		t.Fatalf("stats: %v %+v", err, st)
	}
	s := st[0]
	// 0.5 s of CPU in 1 s of wall time = 50 % of one core.
	if s.CPUNote != "" || s.CPU < 49.99 || s.CPU > 50.01 {
		t.Fatalf("cpu %v note %q", s.CPU, s.CPUNote)
	}
	if s.MemNote != "" || s.MemUsed != 7340032 || s.MemLimit != 8221581312 {
		t.Fatalf("mem %+v", s)
	}
	for _, q := range f.statQuery {
		if !strings.Contains(q, "one-shot=true") || !strings.Contains(q, "stream=false") {
			t.Fatalf("podman stats must be one-shot samples, got %q", q)
		}
	}
}

func TestPodmanStatsRootlessEmptyMemory(t *testing.T) {
	podmanStatsGap = time.Millisecond
	defer func() { podmanStatsGap = time.Second }()
	e := newCompatEngine(t, &fakeCompat{version: "version_podman5_rootless.json", info: "info_podman5_rootless.json",
		stats: []string{"stats_podman5_rootless_1.json", "stats_podman5_rootless_2.json"}})
	st, err := e.Stats(context.Background(), "web")
	if err != nil || len(st) != 1 {
		t.Fatalf("stats: %v %+v", err, st)
	}
	s := st[0]
	if s.CPUNote != "" || s.CPU < 24.99 || s.CPU > 25.01 {
		t.Fatalf("cpu %v note %q", s.CPU, s.CPUNote)
	}
	if s.MemNote == "" || s.MemUsed != 0 || s.MemLimit != 0 {
		t.Fatalf("empty memory_stats must give a reason, not a number: %+v", s)
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "NaN") || !strings.Contains(string(b), `"memNote"`) {
		t.Fatalf("json %s", b)
	}
}

func TestPodmanStatsErrorGivesReason(t *testing.T) {
	e := newCompatEngine(t, &fakeCompat{version: "version_podman5_rootless.json", info: "info_podman5_rootless.json",
		statsErr: "failed to obtain Container web-web-1 stats: unable to load cgroup at /sys/fs/cgroup/user.slice: cgroup deleted"})
	st, err := e.Stats(context.Background(), "web")
	if err != nil || len(st) != 1 {
		t.Fatalf("stats: %v %+v", err, st)
	}
	if st[0].CPUNote == "" || st[0].MemNote == "" || st[0].CPU != 0 || st[0].MemUsed != 0 {
		t.Fatalf("a failed sample must carry a reason: %+v", st[0])
	}
}

func TestPodmanCPUEdgeCases(t *testing.T) {
	t0 := time.Date(2025, 5, 1, 10, 0, 0, 0, time.UTC)
	sample := func(at time.Time, total uint64) container.StatsResponse {
		var v container.StatsResponse
		v.Read = at
		v.CPUStats.CPUUsage.TotalUsage = total
		return v
	}
	if _, note := podmanCPU(sample(t0, 0), sample(t0.Add(time.Second), 0)); note == "" {
		t.Fatal("no CPU data must give a reason")
	}
	if _, note := podmanCPU(sample(t0, 10), sample(t0, 20)); note == "" {
		t.Fatal("zero wall time must give a reason")
	}
	if _, note := podmanCPU(sample(t0, 500), sample(t0.Add(time.Second), 100)); note == "" {
		t.Fatal("a restart between samples must give a reason")
	}
	if cpu, note := podmanCPU(sample(t0, 1e9), sample(t0.Add(time.Second), 1e9)); note != "" || cpu != 0 {
		t.Fatalf("idle container is 0 %%: %v %q", cpu, note)
	}
	if cpu, _ := podmanCPU(sample(t0, 0), sample(t0.Add(2*time.Second), 3e9)); cpu < 149.9 || cpu > 150.1 {
		t.Fatalf("1.5 cores busy = 150 %%, got %v", cpu)
	}
}

func TestDockerStatEmptyMemory(t *testing.T) {
	var v container.StatsResponse
	v.CPUStats.CPUUsage.TotalUsage, v.PreCPUStats.CPUUsage.TotalUsage = 2e9, 1e9
	v.CPUStats.SystemUsage, v.PreCPUStats.SystemUsage = 8e9, 4e9
	v.CPUStats.OnlineCPUs = 4
	s := dockerStat("x", v)
	if s.MemNote == "" || s.MemUsed != 0 {
		t.Fatalf("empty memory_stats must give a reason: %+v", s)
	}
	if s.CPU != 100 {
		t.Fatalf("docker cpu formula changed: %v", s.CPU)
	}
	v.MemoryStats = container.MemoryStats{Usage: 100 << 20, Limit: 1 << 30, Stats: map[string]uint64{"inactive_file": 20 << 20}}
	if s := dockerStat("x", v); s.MemNote != "" || s.MemUsed != 80<<20 || s.MemLimit != 1<<30 {
		t.Fatalf("docker memory changed: %+v", s)
	}
}

func TestPodmanContainerList(t *testing.T) {
	e := newCompatEngine(t, &fakeCompat{version: "version_podman49_rootful.json", info: "info_podman49_rootful.json"})
	byProject, dirs, err := e.projectContainers(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	cs := byProject["web"]
	if len(cs) != 3 || dirs["web"] != "/srv/stacks/web" {
		t.Fatalf("containers %+v dirs %v", cs, dirs)
	}
	byName := map[string]Container{}
	for _, c := range cs {
		byName[c.Name] = c
	}
	if c := byName["web-init-1"]; c.State != "exited" || c.ExitCode == nil || *c.ExitCode != 0 {
		t.Fatalf("podman 'stopped' must read as exited (0): %+v", c)
	}
	if c := byName["web-worker-1"]; c.State != "created" {
		t.Fatalf("podman 'configured' must read as created: %+v", c)
	}
	if c := byName["web-web-1"]; c.State != "running" || c.Health != "unhealthy" || c.Name != "web-web-1" || len(c.Ports) != 1 || c.Ports[0] != "8081:80/tcp" {
		t.Fatalf("running container: %+v", c)
	}
	if st, running := stackStatus(cs); st != StatusDegraded || running != 1 {
		t.Fatalf("status %s running %d", st, running)
	}
}

func TestNormalizeStateAndHealthText(t *testing.T) {
	for in, want := range map[string]string{"configured": "created", "stopped": "exited", "running": "running", "exited": "exited", "paused": "paused"} {
		if got := normalizeState(in); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
	for in, want := range map[string]string{"Up 3 hours (healthy)": "healthy", "Up 1 second (health: starting)": "starting", "Up 2 minutes (unhealthy)": "unhealthy", "Up 5 minutes": ""} {
		if got := healthFromStatus(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestPodmanExecGoneIsCleanExit(t *testing.T) {
	e := newCompatEngine(t, &fakeCompat{version: "version_podman5_rootless.json", info: "info_podman5_rootless.json"})
	s := &mobyTerminalSession{e: e, api: e.api, execID: "abc", shellPath: "/bin/sh"}
	code, err := s.Wait(context.Background())
	if err != nil || code != 0 {
		t.Fatalf("removed exec session: %d %v", code, err)
	}

	d := newCompatEngine(t, &fakeCompat{version: "version_docker29.json", info: "info_docker29.json"})
	s = &mobyTerminalSession{e: d, api: d.api, execID: "abc", shellPath: "/bin/sh"}
	if _, err := s.Wait(context.Background()); err == nil {
		t.Fatal("on Docker a missing exec stays an error")
	}
}

func TestUTCTimestampAndOrder(t *testing.T) {
	if got := utcTimestamp("2024-05-01T12:00:05+02:00"); got != "2024-05-01T10:00:05Z" {
		t.Fatalf("offset not converted: %s", got)
	}
	for _, same := range []string{"2024-05-01T10:00:05.123456789Z", "", "not-a-time"} {
		if got := utcTimestamp(same); got != same {
			t.Fatalf("%q changed to %q", same, got)
		}
	}
	if !logTimeLess("2024-05-01T10:00:00.5Z", "2024-05-01T10:00:01Z") || logTimeLess("2024-05-01T10:00:00.5Z", "2024-05-01T10:00:00Z") {
		t.Fatal("ordering by time")
	}
}

// logFrame writes one multiplexed (non-TTY) log frame like Podman's compat logs handler.
func logFrame(w io.Writer, stream byte, line string) {
	hdr := make([]byte, 8)
	hdr[0] = stream
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(line)))
	_, _ = w.Write(hdr)
	_, _ = io.WriteString(w, line)
}

func TestPodmanWatchLogsResumeNoDuplicates(t *testing.T) {
	// Podman 4.9 stamps lines with whole seconds and a zone offset. The first follow
	// stream ends after two lines; the resumed one repeats them (inclusive since) plus a
	// new line. Each line must be emitted once.
	stamp := time.Now().Add(2 * time.Second).Truncate(time.Second)
	ts := stamp.In(time.FixedZone("CEST", 2*3600)).Format(time.RFC3339)
	var calls atomic.Int32
	var mu sync.Mutex
	var sinces []string
	f := &fakeCompat{version: "version_podman49_rootful.json", info: "info_podman49_rootful.json"}
	f.logs = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sinces = append(sinces, r.URL.Query().Get("since"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		switch calls.Add(1) {
		case 1:
			logFrame(w, 1, ts+" first\n")
			logFrame(w, 2, ts+" second\n")
		case 2:
			logFrame(w, 1, ts+" first\n")
			logFrame(w, 2, ts+" second\n")
			logFrame(w, 1, ts+" third\n")
		default:
			<-r.Context().Done() // keep following quietly
		}
	}
	e := newCompatEngine(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	var got []string
	var gmu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.WatchLogs(ctx, "web", "web", LogFilter{Pattern: ".", Regex: true}, func(l LogLine) {
			gmu.Lock()
			got = append(got, l.Message+"@"+l.Time)
			gmu.Unlock()
			if strings.HasPrefix(l.Message, "third") {
				cancel()
			}
		})
	}()
	<-done
	gmu.Lock()
	defer gmu.Unlock()
	want := stamp.UTC().Format(time.RFC3339Nano)
	if len(got) != 3 {
		t.Fatalf("want first, second, third once each, got %v", got)
	}
	for _, g := range got {
		if !strings.HasSuffix(g, "@"+want) {
			t.Fatalf("timestamp not converted to UTC: %v", got)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sinces) < 2 {
		t.Fatalf("stream not resumed: %v", sinces)
	}
	// The resumed read starts at the last line's second (inclusive), not 1 ns later.
	if wantSince := fmt.Sprintf("%d.%09d", stamp.Unix(), 0); sinces[1] != wantSince {
		t.Fatalf("resume since %q, want %q", sinces[1], wantSince)
	}
}

func TestRemoteDigestFromRegistry(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok"})
		case "/v2/org/app/manifests/1.2":
			if r.Method != http.MethodHead {
				t.Errorf("method %s, want HEAD", r.Method)
			}
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="reg",scope="repository:org/app:pull"`, srv.URL))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if !strings.HasPrefix(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
				t.Errorf("index types must come first: %q", r.Header.Get("Accept"))
			}
			w.Header().Set("Docker-Content-Digest", digest)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	d, err := RemoteDigestFromRegistry(context.Background(), host+"/org/app:1.2", RegistryCreds{}, srv.Client())
	if err != nil || d != digest {
		t.Fatalf("digest %q err %v", d, err)
	}
	_, err = RemoteDigestFromRegistry(context.Background(), host+"/org/missing:1", RegistryCreds{}, srv.Client())
	if err == nil || classifyRegistryError(err) != "not found" {
		t.Fatalf("missing manifest: %v (%s)", err, classifyRegistryError(err))
	}
}

func TestPodmanRepoDigestsMatch(t *testing.T) {
	// Podman records fully qualified names and, for a multi-platform pull, both the
	// index and the platform manifest digest.
	idx := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	plat := "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	rds := []string{"docker.io/library/nginx@" + plat, "docker.io/library/nginx@" + idx}
	if !hasMatchingDigest(rds, idx) {
		t.Fatal("index digest not matched")
	}
	if got := repoDigestFor("nginx:1.27", rds); got != plat && got != idx {
		t.Fatalf("repo digest for short name: %q", got)
	}
}

func loadProject(t *testing.T, yaml string) *types.Project {
	t.Helper()
	p, err := loader.LoadWithContext(context.Background(), types.ConfigDetails{
		WorkingDir:  t.TempDir(),
		ConfigFiles: []types.ConfigFile{{Filename: "compose.yaml", Content: []byte(yaml)}},
	}, func(o *loader.Options) { o.SetProjectName("demo", true); o.SkipResolveEnvironment = true })
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPodmanComposeWarnings(t *testing.T) {
	p := loadProject(t, `services:
  db:
    image: postgres:17
    healthcheck:
      test: ["CMD", "pg_isready"]
  web:
    image: nginx
    ports: ["80:80", "8080:8080"]
    depends_on:
      db:
        condition: service_healthy
`)
	w := podmanComposeWarnings(p, true)
	joined := strings.Join(w, "\n")
	if len(w) != 2 || !strings.Contains(joined, "web waits for db") || !strings.Contains(joined, "port 80.") {
		t.Fatalf("warnings: %v", w)
	}
	if w := podmanComposeWarnings(p, false); len(w) != 1 {
		t.Fatalf("rootful must not warn about low ports: %v", w)
	}
	if w := podmanComposeWarnings(loadProject(t, "services:\n  a:\n    image: busybox\n"), true); len(w) != 0 {
		t.Fatalf("plain project: %v", w)
	}
}

func TestPodmanOpHint(t *testing.T) {
	err := fmt.Errorf("Error response from daemon: rootlessport cannot expose privileged port 80, you can add 'net.ipv4.ip_unprivileged_port_start=80' to /etc/sysctl.conf (currently 1024), or choose a larger port number (>= 1024): listen tcp 0.0.0.0:80: bind: permission denied")
	if h := podmanOpHint(err); !strings.Contains(h, "below 1024") {
		t.Fatalf("hint %q", h)
	}
	if podmanOpHint(nil) != "" || podmanOpHint(fmt.Errorf("network not found")) != "" {
		t.Fatal("no hint expected")
	}
	for in, want := range map[string]int{"80": 80, "8080-8081": 8080, "": 0, "x": 0, "70000": 0} {
		if got := publishedPort(in); got != want {
			t.Errorf("publishedPort(%q) = %d, want %d", in, got, want)
		}
	}
}
