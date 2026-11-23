package metrics

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/monitor"
	"moorage/internal/store"
)

// ---- exposition format checker ----------------------------------------------

type series struct {
	name   string
	labels map[string]string
	value  float64
}

var (
	metricNameRe = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNameRe  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	sampleRe     = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{.*\})? (\S+)$`)
)

// parseExposition checks text against the Prometheus text format 0.0.4 rules that
// matter here and returns the samples. It fails the test on: bad metric or label names,
// bad label syntax or escapes, HELP/TYPE missing, repeated or after samples, a family
// split in two, and duplicate series.
func parseExposition(t *testing.T, text string) []series {
	t.Helper()
	var out []series
	help, typ := map[string]bool{}, map[string]string{}
	sampled := map[string]bool{}
	done := map[string]bool{} // families that ended (another family started after them)
	seen := map[string]bool{}
	current := ""
	if text != "" && !strings.HasSuffix(text, "\n") {
		t.Fatalf("output doesn't end with a newline")
	}
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		fail := func(f string, a ...any) { t.Fatalf("line %d %q: %s", i+1, line, fmt.Sprintf(f, a...)) }
		if line == "" {
			fail("empty line")
		}
		if strings.HasPrefix(line, "#") {
			parts := strings.SplitN(line, " ", 4)
			if len(parts) < 4 || (parts[1] != "HELP" && parts[1] != "TYPE") {
				fail("bad comment")
			}
			name := parts[2]
			if !metricNameRe.MatchString(name) {
				fail("bad metric name")
			}
			if sampled[name] {
				fail("%s after samples", parts[1])
			}
			if parts[1] == "HELP" {
				if help[name] {
					fail("second HELP")
				}
				help[name] = true
			} else {
				if typ[name] != "" {
					fail("second TYPE")
				}
				if parts[3] != "gauge" && parts[3] != "counter" {
					fail("unexpected type")
				}
				typ[name] = parts[3]
			}
			continue
		}
		m := sampleRe.FindStringSubmatch(line)
		if m == nil {
			fail("not a sample")
		}
		name := m[1]
		if typ[name] == "" || !help[name] {
			fail("sample before HELP and TYPE")
		}
		if typ[name] == "counter" && !strings.HasSuffix(name, "_total") {
			fail("counter without _total")
		}
		if name != current {
			if done[name] {
				fail("family %s split", name)
			}
			if current != "" {
				done[current] = true
			}
			current = name
		}
		sampled[name] = true
		labels := map[string]string{}
		if m[2] != "" {
			labels = parseLabels(t, m[2][1:len(m[2])-1], fail)
		}
		v, err := parseValue(m[3])
		if err != nil {
			fail("bad value: %v", err)
		}
		keys := make([]string, 0, len(labels))
		for k, val := range labels {
			keys = append(keys, k+"="+strconv.Quote(val))
		}
		sort.Strings(keys)
		key := name + "{" + strings.Join(keys, ",") + "}"
		if seen[key] {
			fail("duplicate series")
		}
		seen[key] = true
		out = append(out, series{name: name, labels: labels, value: v})
	}
	return out
}

func parseLabels(t *testing.T, s string, fail func(string, ...any)) map[string]string {
	t.Helper()
	out := map[string]string{}
	for s != "" {
		eq := strings.Index(s, `="`)
		if eq < 0 {
			fail("label without =\"")
		}
		name := s[:eq]
		if !labelNameRe.MatchString(name) {
			fail("bad label name %q", name)
		}
		if _, dup := out[name]; dup {
			fail("label %s twice", name)
		}
		s = s[eq+2:]
		var val strings.Builder
		closed := false
		for len(s) > 0 && !closed {
			c := s[0]
			s = s[1:]
			switch c {
			case '\\':
				if s == "" {
					fail("dangling backslash")
				}
				switch s[0] {
				case '\\':
					val.WriteByte('\\')
				case '"':
					val.WriteByte('"')
				case 'n':
					val.WriteByte('\n')
				default:
					fail("bad escape \\%c", s[0])
				}
				s = s[1:]
			case '"':
				closed = true
			case '\n':
				fail("raw newline in label")
			default:
				val.WriteByte(c)
			}
		}
		if !closed {
			fail("unterminated label value")
		}
		out[name] = val.String()
		if s != "" {
			if s[0] != ',' {
				fail("expected comma between labels")
			}
			s = s[1:]
		}
	}
	return out
}

func parseValue(s string) (float64, error) {
	switch s {
	case "NaN":
		return math.NaN(), nil
	case "+Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	}
	return strconv.ParseFloat(s, 64)
}

// find returns the samples of name whose labels include want.
func find(all []series, name string, want ...string) []series {
	var out []series
	for _, s := range all {
		if s.name != name {
			continue
		}
		ok := true
		for i := 0; i+1 < len(want); i += 2 {
			if v, has := s.labels[want[i]]; !has || v != want[i+1] {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, s)
		}
	}
	return out
}

func one(t *testing.T, all []series, name string, want ...string) float64 {
	t.Helper()
	got := find(all, name, want...)
	if len(got) != 1 {
		t.Fatalf("%s%v: want 1 series, got %d", name, want, len(got))
	}
	return got[0].value
}

// ---- fakes -----------------------------------------------------------------

// fakeHost implements the calls the collector makes; any other call panics on the
// embedded nil interface, which would show the collector doing more than it should.
type fakeHost struct {
	host.Host
	lists   atomic.Int64
	gets    atomic.Int64
	block   chan struct{} // when set, ListStacks waits for it and ignores its context
	stacks  []engine.StackSummary
	details map[string][]engine.Container
}

func (f *fakeHost) Info(context.Context) engine.HostInfo {
	return engine.HostInfo{Name: "box", Reachable: true}
}

func (f *fakeHost) ListStacks(context.Context) ([]engine.StackSummary, error) {
	f.lists.Add(1)
	if f.block != nil {
		<-f.block
	}
	return f.stacks, nil
}

func (f *fakeHost) GetStack(_ context.Context, name string) (engine.Stack, error) {
	f.gets.Add(1)
	cs, ok := f.details[name]
	if !ok {
		return engine.Stack{}, engine.ErrNotFound
	}
	return engine.Stack{StackSummary: engine.StackSummary{Name: name, Total: len(cs)}, Containers: cs}, nil
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		stacks: []engine.StackSummary{
			{Name: "web", Total: 2, Running: 1},
			{Name: "idle", Total: 0},
		},
		details: map[string][]engine.Container{
			"web": {
				{Name: "web-app-1", Service: "app", State: "running"},
				{Name: "web-db-1", Service: "db", State: "exited"},
			},
		},
	}
}

type env struct {
	st  *store.Store
	reg *host.Registry
	mon *monitor.Monitor
	c   *Collector
	h   *fakeHost
}

func newEnv(t *testing.T, h *fakeHost) *env {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	reg := host.NewRegistry(h, "box", false, st)
	mon := monitor.New(st, reg, slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := New(st, reg, mon, "1.2.3")
	c.SetContext(ctx)
	c.RefreshEvery = time.Hour // tests decide when the snapshot is refreshed
	return &env{st: st, reg: reg, mon: mon, c: c, h: h}
}

func scrape(t *testing.T, c *Collector) []series {
	t.Helper()
	var buf bytes.Buffer
	if err := c.Write(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	return parseExposition(t, buf.String())
}

// ---- tests -----------------------------------------------------------------

func TestEscapeLabel(t *testing.T) {
	cases := map[string]string{
		"plain":           "plain",
		`back\slash`:      `back\\slash`,
		`say "hi"`:        `say \"hi\"`,
		"two\nlines":      `two\nlines`,
		"all\\\"\n":       `all\\\"\n`,
		"unicode ✓ stays": "unicode ✓ stays",
	}
	for in, want := range cases {
		if got := escapeLabel(in); got != want {
			t.Errorf("escapeLabel(%q) = %q, want %q", in, got, want)
		}
	}
	if got := escapeHelp("a\\b\nc\"d"); got != `a\\b\nc"d` {
		t.Errorf("escapeHelp = %q", got)
	}
	// round trip through the checker
	var buf bytes.Buffer
	w := newWriter(&buf)
	w.family("x_info", "gauge", "Help with \\ and\nnewline.")
	w.sample(1, "v", "a\\b\"c\nd")
	w.flush() //nolint:errcheck
	got := parseExposition(t, buf.String())
	if len(got) != 1 || got[0].labels["v"] != "a\\b\"c\nd" {
		t.Fatalf("round trip: %#v\n%s", got, buf.String())
	}
}

func TestWriterSkipsEmptyFamiliesAndDuplicates(t *testing.T) {
	var buf bytes.Buffer
	w := newWriter(&buf)
	w.family("empty", "gauge", "Never sampled.")
	w.family("dup", "gauge", "Twice the same series.")
	w.sample(1, "a", "1")
	w.sample(2, "a", "1")
	w.sample(math.NaN(), "a", "2")
	w.flush() //nolint:errcheck
	out := buf.String()
	if strings.Contains(out, "empty") {
		t.Errorf("empty family written:\n%s", out)
	}
	s := parseExposition(t, out)
	if len(s) != 2 || s[0].value != 1 || !math.IsNaN(s[1].value) {
		t.Errorf("samples: %#v", s)
	}
}

func TestScrapeContentAndValues(t *testing.T) {
	e := newEnv(t, newFakeHost())
	ctx := context.Background()

	// an agent that never connected: offline, no container series
	if err := e.st.CreateJoinToken(ctx, "hash", "pi", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.RedeemJoinToken(ctx, "hash", "agent1", "pi-host"); err != nil {
		t.Fatal(err)
	}

	name := "we\"b\\check\nx"
	id, err := e.st.SaveCheck(ctx, store.Check{Name: name, Kind: monitor.KindHTTP, Target: "https://x",
		HostID: "local", Stack: "web", Interval: 60, Timeout: 5, Retries: 1, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	stackID, err := e.st.SaveCheck(ctx, store.Check{Name: "stack", Kind: monitor.KindStack, Target: "web",
		HostID: "local", Interval: 60, Timeout: 5, Retries: 1, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, st := range []string{"up", "up", "up", "down", "silenced"} {
		e.st.AddResult(ctx, store.Result{CheckID: id, TS: now.Add(-time.Duration(i+1) * time.Minute), Status: st}) //nolint:errcheck
	}
	e.st.AddResult(ctx, store.Result{CheckID: id, TS: now.Add(-10 * 24 * time.Hour), Status: "down"}) //nolint:errcheck
	e.mon.SetState(id, monitor.State{Status: monitor.StatusUp, Last: &monitor.LastProbe{At: now, Status: "up", LatencyMS: 123}})
	e.mon.SetState(stackID, monitor.State{Status: monitor.StatusMaintenance})

	if err := e.st.SaveStackImageChecks(ctx, "local", "web", []store.ImageCheckRow{
		{HostID: "local", Stack: "web", Service: "app", Ref: "nginx:1", Status: engine.ImageUpdateAvailable},
		{HostID: "local", Stack: "web", Service: "db", Ref: "postgres:16", Status: engine.ImageUpToDate},
	}); err != nil {
		t.Fatal(err)
	}

	s := scrape(t, e.c)

	if one(t, s, "moorage_build_info", "version", "1.2.3") != 1 {
		t.Error("build_info")
	}
	if one(t, s, "moorage_host_up", "host_id", "local", "host", "box") != 1 {
		t.Error("local host should be up")
	}
	if one(t, s, "moorage_host_up", "host_id", "agent1", "host", "pi") != 0 {
		t.Error("offline agent should be down")
	}
	if one(t, s, "moorage_host_read_only", "host_id", "local") != 0 {
		t.Error("read_only")
	}
	if len(find(s, "moorage_agent_info")) != 0 {
		t.Error("agent_info for an offline agent")
	}
	for _, n := range []string{"moorage_stack_containers", "moorage_container_up"} {
		if len(find(s, n, "host_id", "agent1")) != 0 {
			t.Errorf("%s for the offline host", n)
		}
	}

	// checks: state from the monitor, the name escaped and read back intact
	lbl := []string{"check_id", strconv.FormatInt(id, 10), "check", name, "type", "http", "host_id", "local", "host", "box", "stack", "web"}
	if one(t, s, "moorage_check_up", lbl...) != 1 {
		t.Error("check_up")
	}
	if v := one(t, s, "moorage_check_latency_seconds", lbl...); v != 0.123 {
		t.Errorf("latency %v", v)
	}
	if v := one(t, s, "moorage_check_uptime_ratio", append(lbl, "window", "24h")...); v != 0.75 {
		t.Errorf("uptime 24h %v, want 0.75 (3 up, 1 down; silenced doesn't count)", v)
	}
	if v := one(t, s, "moorage_check_uptime_ratio", append(lbl, "window", "30d")...); v != 0.6 {
		t.Errorf("uptime 30d %v, want 0.6", v)
	}
	if one(t, s, "moorage_check_state", append(lbl, "state", "up")...) != 1 ||
		one(t, s, "moorage_check_state", append(lbl, "state", "down")...) != 0 {
		t.Error("check_state of the up check")
	}
	sid := strconv.FormatInt(stackID, 10)
	if one(t, s, "moorage_check_up", "check_id", sid, "stack", "web", "type", "stack") != 0 {
		t.Error("a check in maintenance isn't up")
	}
	if one(t, s, "moorage_check_state", "check_id", sid, "state", "maintenance") != 1 {
		t.Error("maintenance state")
	}
	if len(find(s, "moorage_check_latency_seconds", "check_id", sid)) != 0 {
		t.Error("latency without a probe")
	}
	if len(find(s, "moorage_check_uptime_ratio", "check_id", sid)) != 0 {
		t.Error("uptime without results")
	}

	// stacks and containers
	if one(t, s, "moorage_stack_containers", "host_id", "local", "stack", "web", "state", "running") != 1 ||
		one(t, s, "moorage_stack_containers", "host_id", "local", "stack", "web", "state", "exited") != 1 ||
		one(t, s, "moorage_stack_containers", "host_id", "local", "stack", "web", "state", "dead") != 0 {
		t.Error("stack container counts")
	}
	if one(t, s, "moorage_stack_containers", "host_id", "local", "stack", "idle", "state", "running") != 0 {
		t.Error("stack without containers")
	}
	if one(t, s, "moorage_container_up", "host_id", "local", "stack", "web", "service", "app", "container", "web-app-1") != 1 ||
		one(t, s, "moorage_container_up", "service", "db", "container", "web-db-1") != 0 {
		t.Error("container_up")
	}
	if e.h.gets.Load() != 1 {
		t.Errorf("GetStack called %d times, want 1 (only stacks with containers)", e.h.gets.Load())
	}

	// image updates
	if one(t, s, "moorage_image_update_available", "host_id", "local", "stack", "web", "service", "app") != 1 ||
		one(t, s, "moorage_image_update_available", "host_id", "local", "stack", "web", "service", "db") != 0 {
		t.Error("image_update_available")
	}

	// no unbounded labels anywhere
	for _, x := range s {
		for k := range x.labels {
			switch k {
			case "version", "host_id", "host", "check_id", "check", "type", "stack", "state", "window",
				"service", "container", "channel_type":
			default:
				t.Errorf("unexpected label %s on %s", k, x.name)
			}
		}
	}
}

func TestScrapeReusesFreshSnapshot(t *testing.T) {
	e := newEnv(t, newFakeHost())
	scrape(t, e.c)
	scrape(t, e.c)
	scrape(t, e.c)
	if n := e.h.lists.Load(); n != 1 {
		t.Fatalf("ListStacks called %d times for 3 scrapes with a fresh snapshot, want 1", n)
	}
}

func TestRefreshLoopRunsWhileScrapedAndStopsWhenIdle(t *testing.T) {
	e := newEnv(t, newFakeHost())
	e.c.RefreshEvery = 20 * time.Millisecond
	e.c.IdleStop = 100 * time.Millisecond
	scrape(t, e.c)
	waitFor(t, func() bool { return e.h.lists.Load() >= 3 }, "background refresh while scraped")
	// no more scrapes: the loop must stop after IdleStop
	waitFor(t, func() bool {
		e.c.mu.Lock()
		defer e.c.mu.Unlock()
		return !e.c.running
	}, "refresh loop stopped after idle")
	n := e.h.lists.Load()
	time.Sleep(100 * time.Millisecond)
	if e.h.lists.Load() != n {
		t.Fatal("host still polled after the idle stop")
	}
}

func TestSlowHostDoesNotBlockScrape(t *testing.T) {
	h := newFakeHost()
	h.block = make(chan struct{})
	t.Cleanup(func() { close(h.block) })
	e := newEnv(t, h)
	e.c.FirstWait = 100 * time.Millisecond
	e.c.HostTimeout = 200 * time.Millisecond

	start := time.Now()
	s := scrape(t, e.c)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("scrape took %v with a hung host", d)
	}
	if one(t, s, "moorage_host_up", "host_id", "local") != 1 {
		t.Error("host_up comes from the registry, not the snapshot")
	}
	if len(find(s, "moorage_container_up")) != 0 {
		t.Error("container series without an answer from the host")
	}

	// the refresh gives up on the hung host and still publishes the rest
	waitFor(t, func() bool {
		e.c.mu.Lock()
		defer e.c.mu.Unlock()
		return e.c.snap != nil
	}, "snapshot without the hung host")
	start = time.Now()
	s = scrape(t, e.c)
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("second scrape took %v", d)
	}
	if len(find(s, "moorage_container_up")) != 0 || len(find(s, "moorage_stack_containers")) != 0 {
		t.Error("container series for a host that didn't answer")
	}
	if len(find(s, "moorage_snapshot_age_seconds")) != 1 {
		t.Error("snapshot age missing")
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHandlerAuth(t *testing.T) {
	e := newEnv(t, newFakeHost())
	const token = "0123456789abcdef-secret"
	get := func(h http.Handler, method, auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/metrics", nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	off := e.c.Handler("")
	if w := get(off, "GET", "Bearer "+token); w.Code != http.StatusNotFound {
		t.Errorf("without a token configured: %d, want 404", w.Code)
	}

	on := e.c.Handler(token)
	for _, a := range []string{"", "Bearer", "Bearer ", "Bearer wrong-token-of-length", "Basic " + token, token, "Bearer " + token + "x"} {
		w := get(on, "GET", a)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q: %d, want 401", a, w.Code)
		}
		if w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("Authorization %q: no WWW-Authenticate", a)
		}
	}
	if w := get(on, "POST", "Bearer "+token); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
	for _, a := range []string{"Bearer " + token, "bearer " + token} {
		w := get(on, "GET", a)
		if w.Code != http.StatusOK {
			t.Fatalf("Authorization %q: %d", a, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != ContentType {
			t.Errorf("content type %q", ct)
		}
		if strings.Contains(w.Body.String(), token) {
			t.Error("the token appears in the output")
		}
		parseExposition(t, w.Body.String())
	}
	if w := get(on, "HEAD", "Bearer "+token); w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("HEAD: %d, %d bytes", w.Code, w.Body.Len())
	}
}

// TestConcurrentScrapes runs scrapes in parallel with refreshes (go test -race catches
// data races where CGO allows it; here it at least checks nothing deadlocks).
func TestConcurrentScrapes(t *testing.T) {
	e := newEnv(t, newFakeHost())
	e.c.RefreshEvery = 5 * time.Millisecond
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				var buf bytes.Buffer
				if err := e.c.Write(context.Background(), &buf); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// Multi-location checks get one up and latency series per location that voted.
func TestScrapeLocationSeries(t *testing.T) {
	e := newEnv(t, newFakeHost())
	ctx := context.Background()
	id, err := e.st.SaveCheck(ctx, store.Check{Name: "site", Kind: monitor.KindHTTP, Target: "https://x",
		Interval: 60, Timeout: 5, Retries: 1, Enabled: true, Locations: []string{"hub", "ag1", "ag2"}, Quorum: "majority"})
	if err != nil {
		t.Fatal(err)
	}
	e.mon.SetState(id, monitor.State{Status: monitor.StatusUp, Locations: []monitor.LocationResult{
		{Location: "hub", Name: "hub", Vote: monitor.VoteUp, LatencyMS: 20},
		{Location: "ag1", Name: "wd1", Vote: monitor.VoteDown, LatencyMS: 5000},
		{Location: "ag2", Name: "wd2", Vote: monitor.VoteNone, Msg: "agent offline"},
	}})
	s := scrape(t, e.c)
	lbl := []string{"check_id", strconv.FormatInt(id, 10), "check", "site", "type", "http"}
	if one(t, s, "moorage_check_location_up", append(lbl, "location", "hub", "location_name", "hub")...) != 1 {
		t.Error("hub location up")
	}
	if one(t, s, "moorage_check_location_up", append(lbl, "location", "ag1", "location_name", "wd1")...) != 0 {
		t.Error("wd1 location down")
	}
	if v := one(t, s, "moorage_check_location_latency_seconds", append(lbl, "location", "ag1")...); v != 5 {
		t.Errorf("wd1 latency %v", v)
	}
	if len(find(s, "moorage_check_location_up", "location", "ag2")) != 0 {
		t.Error("a location without an answer must have no series")
	}
}
