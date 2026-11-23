package deps

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/store"
)

func facts(t *testing.T, stacks map[string]string) engine.DepFacts {
	t.Helper()
	var f engine.DepFacts
	for name, compose := range stacks {
		sf, err := engine.ComposeFacts(context.Background(), name, "", compose, "")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		f.Stacks = append(f.Stacks, sf)
	}
	return f
}

func edgeSet(es []store.StackDep) map[string]string {
	out := map[string]string{}
	for _, e := range es {
		out[e.From.Stack+">"+e.To.Stack] = e.Reason
	}
	return out
}

func TestComputeSharedNetworkByName(t *testing.T) {
	f := facts(t, map[string]string{
		"pg": `
services:
  db:
    image: postgres
    networks: [backend]
networks:
  backend: {}
`,
		"app": `
services:
  web:
    image: nginx
    networks: [pg_backend]
networks:
  pg_backend:
    external: true
`,
		"other": `
services:
  web:
    image: nginx
`,
	})
	got := edgeSet(Compute("local", f))
	if len(got) != 1 || got["app>pg"] != ReasonNetwork {
		t.Fatalf("edges: %v", got)
	}
}

func TestComputeSharedNetworkByNameOverride(t *testing.T) {
	f := facts(t, map[string]string{
		"proxy": `
services:
  traefik:
    image: traefik
    networks: [web]
networks:
  web:
    name: public
`,
		"blog": `
services:
  ghost:
    image: ghost
    networks: [front]
networks:
  front:
    external: true
    name: public
`,
	})
	got := edgeSet(Compute("local", f))
	if len(got) != 1 || got["blog>proxy"] != ReasonNetwork {
		t.Fatalf("edges: %v", got)
	}
}

// Host names in env count only when the two services share a network.
func TestComputeHostnameNeedsSharedNetwork(t *testing.T) {
	shared := `
services:
  db:
    image: postgres
    networks: [shared]
networks:
  shared:
    external: true
`
	f := facts(t, map[string]string{
		"pg": shared,
		"app": `
services:
  web:
    image: nginx
    environment:
      DATABASE_URL: postgres://u:secret@db:5432/app
    networks: [shared]
networks:
  shared:
    external: true
`,
		"lonely": `
services:
  web:
    image: nginx
    environment:
      DB_HOST: db:5432
`,
	})
	got := edgeSet(Compute("local", f))
	if len(got) != 1 || got["app>pg"] != ReasonHostname {
		t.Fatalf("edges: %v", got)
	}
}

// A name the stack itself answers to, or that two other stacks answer to, makes no edge.
func TestComputeHostnameOwnAndAmbiguous(t *testing.T) {
	net := `
networks:
  shared:
    external: true
`
	f := facts(t, map[string]string{
		"a": `
services:
  cache:
    image: redis
    networks: [shared]
` + net,
		"b": `
services:
  cache:
    image: redis
    networks: [shared]
` + net,
		"c": `
services:
  db:
    image: postgres
    networks: [shared]
  web:
    image: nginx
    environment:
      - REDIS=cache:6379
      - PG=db:5432
    networks: [shared]
` + net,
	})
	if got := edgeSet(Compute("local", f)); len(got) != 0 {
		t.Fatalf("want no edges, got %v", got)
	}
}

func TestComputeLinksAndContainerName(t *testing.T) {
	f := facts(t, map[string]string{
		"mq": `
services:
  rabbit:
    image: rabbitmq
    container_name: broker
    networks: [bus]
networks:
  bus:
    name: bus
`,
		"worker": `
services:
  w:
    image: busybox
    external_links: ["broker:mq"]
    networks: [bus]
networks:
  bus:
    external: true
`,
	})
	got := edgeSet(Compute("local", f))
	// the network edge wins over the host name edge of the same pair
	if len(got) != 1 || got["worker>mq"] != ReasonNetwork {
		t.Fatalf("edges: %v", got)
	}
}

func TestGraphDepthAndCycles(t *testing.T) {
	ref := func(s string) store.StackRef { return store.StackRef{HostID: "h", Stack: s} }
	e := func(a, b string) store.StackDep { return store.StackDep{From: ref(a), To: ref(b)} }
	g := Build([]store.StackDep{e("a", "root"), e("b", "a"), e("c", "b"), e("d", "c"), e("x", "y"), e("y", "x"), e("z", "x"),
		{From: ref("ign"), To: ref("root"), Ignored: true}})
	up := g.Upstream(ref("d"), MaxDepth)
	if len(up) != 3 || up[0].Stack != "c" || up[2].Stack != "a" {
		t.Fatalf("upstream of d: %v", up)
	}
	down := g.Downstream(ref("root"), MaxDepth)
	if len(down) != 3 || down[0].Stack != "a" {
		t.Fatalf("downstream of root (ignored edge must not count): %v", down)
	}
	if len(g.Upstream(ref("x"), MaxDepth)) != 0 || len(g.Upstream(ref("y"), MaxDepth)) != 0 {
		t.Fatal("cycle edges must be skipped")
	}
	if up := g.Upstream(ref("z"), MaxDepth); len(up) != 1 || up[0].Stack != "x" {
		t.Fatalf("edge into a cycle still counts: %v", up)
	}
	if cs := g.Cycles(); len(cs) != 1 || len(cs[0]) != 2 || !g.InCycle(ref("x")) || g.InCycle(ref("z")) {
		t.Fatalf("cycles: %v", cs)
	}
}

// ---- service with a store and agents -------------------------------------------------

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// agent serves handler as a connected agent "a1" in a registry.
func agent(t *testing.T, st *store.Store, handler http.Handler) *host.Registry {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	rm := host.NewRemote(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	})
	reg := host.NewRegistry(nil, "", false, st)
	reg.Attach("a1", rm, host.AgentInfo{}, func() error { return nil })
	return reg
}

func factsHandler(f *engine.DepFacts) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /deps/facts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(f) //nolint:errcheck
	})
	return mux
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestDetectionKeepsIgnoredAndManualEdges(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	f := facts(t, map[string]string{
		"pg":  "services:\n  db:\n    image: postgres\n    networks: [b]\nnetworks:\n  b: {}\n",
		"app": "services:\n  web:\n    image: nginx\n    networks: [pg_b]\nnetworks:\n  pg_b:\n    external: true\n",
		"api": "services:\n  web:\n    image: nginx\n    networks: [pg_b]\nnetworks:\n  pg_b:\n    external: true\n",
	})
	reg := agent(t, st, factsHandler(&f))
	s := New(st, reg, quiet())
	if d := s.Detect(ctx, "a1"); d.Error != "" || d.Unsupported || d.Stacks != 3 {
		t.Fatalf("detection: %+v", d)
	}
	edges, _ := st.StackDepsOfHost(ctx, "a1")
	if len(edges) != 2 {
		t.Fatalf("want 2 detected edges, got %+v", edges)
	}
	// ignore app>pg, add a manual edge api>cache, then detect again
	var appPg store.StackDep
	for _, e := range edges {
		if e.From.Stack == "app" {
			appPg = e
		}
	}
	if err := st.SetDepIgnored(ctx, appPg.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddManualDep(ctx, store.StackRef{HostID: "a1", Stack: "api"}, store.StackRef{HostID: "a1", Stack: "cache"}, "ann"); err != nil {
		t.Fatal(err)
	}
	s.Detect(ctx, "a1")
	if err := s.Reload(ctx); err != nil { // the API reloads after every edge change
		t.Fatal(err)
	}
	edges, _ = st.StackDepsOfHost(ctx, "a1")
	byPair := map[string]store.StackDep{}
	for _, e := range edges {
		byPair[e.From.Stack+">"+e.To.Stack] = e
	}
	if e := byPair["app>pg"]; !e.Ignored || e.Source != store.DepDetected {
		t.Fatalf("ignored edge lost its flag: %+v", e)
	}
	if e := byPair["api>cache"]; e.Source != store.DepManual {
		t.Fatalf("manual edge lost: %+v", byPair)
	}
	if s.Graph().Upstream(store.StackRef{HostID: "a1", Stack: "app"}, MaxDepth) != nil {
		t.Fatal("an ignored edge must not be in the graph")
	}

	// api no longer uses the network: its detected edge goes, the manual one stays;
	// app's ignored edge stays ignored even though it is still detected.
	f = facts(t, map[string]string{
		"pg":  "services:\n  db:\n    image: postgres\n    networks: [b]\nnetworks:\n  b: {}\n",
		"app": "services:\n  web:\n    image: nginx\n    networks: [pg_b]\nnetworks:\n  pg_b:\n    external: true\n",
		"api": "services:\n  web:\n    image: nginx\n",
	})
	s.Detect(ctx, "a1")
	edges, _ = st.StackDepsOfHost(ctx, "a1")
	byPair = map[string]store.StackDep{}
	for _, e := range edges {
		byPair[e.From.Stack+">"+e.To.Stack] = e
	}
	if _, ok := byPair["api>pg"]; ok {
		t.Fatal("a detected edge that disappeared must be removed")
	}
	if !byPair["app>pg"].Ignored || byPair["api>cache"].Source != store.DepManual {
		t.Fatalf("after re-detection: %+v", byPair)
	}
}

// A manual edge on a pair that is also detected stays manual when detection drops it.
func TestManualOverDetectedSurvives(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	f := facts(t, map[string]string{
		"pg":  "services:\n  db:\n    image: postgres\n    networks: [b]\nnetworks:\n  b: {}\n",
		"app": "services:\n  web:\n    image: nginx\n    networks: [pg_b]\nnetworks:\n  pg_b:\n    external: true\n",
	})
	reg := agent(t, st, factsHandler(&f))
	s := New(st, reg, quiet())
	s.Detect(ctx, "a1")
	if _, err := st.AddManualDep(ctx, store.StackRef{HostID: "a1", Stack: "app"}, store.StackRef{HostID: "a1", Stack: "pg"}, "ann"); err != nil {
		t.Fatal(err)
	}
	f = facts(t, map[string]string{"pg": "services:\n  db:\n    image: postgres\n", "app": "services:\n  web:\n    image: nginx\n"})
	s.Detect(ctx, "a1")
	edges, _ := st.StackDepsOfHost(ctx, "a1")
	if len(edges) != 1 || edges[0].Source != store.DepManual || edges[0].CreatedBy != "ann" {
		t.Fatalf("edges: %+v", edges)
	}
}

// A stack whose compose file doesn't load keeps its detected edges.
func TestBrokenStackKeepsEdges(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	f := facts(t, map[string]string{
		"pg":  "services:\n  db:\n    image: postgres\n    networks: [b]\nnetworks:\n  b: {}\n",
		"app": "services:\n  web:\n    image: nginx\n    networks: [pg_b]\nnetworks:\n  pg_b:\n    external: true\n",
	})
	reg := agent(t, st, factsHandler(&f))
	s := New(st, reg, quiet())
	s.Detect(ctx, "a1")
	for i := range f.Stacks {
		if f.Stacks[i].Name == "app" {
			f.Stacks[i] = engine.StackFacts{Name: "app", Error: "bad yaml"}
		}
	}
	d := s.Detect(ctx, "a1")
	edges, _ := st.StackDepsOfHost(ctx, "a1")
	if len(edges) != 1 || len(d.Broken) != 1 {
		t.Fatalf("edges %+v, detection %+v", edges, d)
	}
}

// An older agent answers 404: no detected edges, the UI is told, manual edges still work.
func TestOlderAgentHasNoDetection(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	reg := agent(t, st, http.NewServeMux()) // knows no /deps/facts
	s := New(st, reg, quiet())
	d := s.Detect(ctx, "a1")
	if !d.Unsupported || d.Error != "" {
		t.Fatalf("detection: %+v", d)
	}
	if edges, _ := st.StackDepsOfHost(ctx, "a1"); len(edges) != 0 {
		t.Fatalf("edges: %+v", edges)
	}
	if _, err := st.AddManualDep(ctx, store.StackRef{HostID: "a1", Stack: "app"}, store.StackRef{HostID: "a1", Stack: "pg"}, "ann"); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	s.Detect(ctx, "a1")
	if up := s.Upstream(store.StackRef{HostID: "a1", Stack: "app"}); len(up) != 1 || up[0].Stack != "pg" {
		t.Fatalf("manual edge on an old agent: %v", up)
	}
	if st := s.Status("a1"); !st.Unsupported {
		t.Fatalf("status: %+v", st)
	}
}

// Triggers while a run is going collapse into one follow-up run.
func TestTriggerRunsOneAtATime(t *testing.T) {
	st := newStore(t)
	release := make(chan struct{})
	started := make(chan struct{}, 100)
	var active, peak atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /deps/facts", func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		if n > peak.Load() {
			peak.Store(n)
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
		w.Write([]byte(`{"stacks":[]}`)) //nolint:errcheck
	})
	reg := agent(t, st, mux)
	s := New(st, reg, quiet())
	for range 5 {
		s.Trigger("a1")
	}
	<-started // the first run is in progress
	for range 5 {
		s.Trigger("a1")
	}
	close(release)
	<-started // exactly one follow-up
	deadline := time.Now().Add(5 * time.Second)
	for s.Status("a1").Running && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(started) != 0 || peak.Load() != 1 {
		t.Fatalf("extra runs: %d, peak %d", len(started), peak.Load())
	}
	if s.Status("a1").At.IsZero() {
		t.Fatal("status not recorded")
	}
}
