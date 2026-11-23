package api

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"moorage/internal/auth"
	"moorage/internal/config"
	"moorage/internal/monitor"
	"moorage/internal/status"
	"moorage/internal/store"
)

func newStatusTestEnv(t *testing.T, publicURL string) (*Server, *store.Store, *monitor.Monitor, http.Handler, http.Handler) {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := auth.New(db, log)
	if err != nil {
		t.Fatal(err)
	}
	mon := monitor.New(db, nil, log, publicURL)
	srv := New(config.Config{PublicURL: publicURL, StatusListen: ":5085"}, a, db, nil, nil, mon, log, "test")
	return srv, db, mon, srv.Handler(), srv.StatusHandler()
}

func doHost(h http.Handler, method, hostHeader, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if hostHeader != "" {
		r.Host = hostHeader
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// 1. No leak (most important): internal check names, targets, host IDs, stack names,
// result messages, incident causes and log lines must NEVER appear in the public HTML,
// RSS feed or preview JSON.
func TestStatusPageNoDataLeakAndEscaping(t *testing.T) {
	srv, db, mon, h, _ := newStatusTestEnv(t, "https://moorage.example.com")
	ctx := context.Background()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	srv.StatusService().Now = func() time.Time { return now }

	chk1, err := db.SaveCheck(ctx, store.Check{
		Name: "internal-db-check", Kind: monitor.KindHTTP, Target: "http://secret.lan:8080/health",
		Interval: 60, Timeout: 5, Retries: 2, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	chk2, err := db.SaveCheck(ctx, store.Check{
		Name: "internal-stack-probe", Kind: monitor.KindStack, Target: "privstack",
		HostID: "pi-home", Stack: "privstack",
		Interval: 60, Timeout: 5, Retries: 1, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	mon.SetState(chk1, monitor.State{
		Status: monitor.StatusDown,
		Since:  now.Add(-5 * time.Minute),
		Last:   &monitor.LastProbe{At: now, Status: monitor.StatusDown, LatencyMS: 42, Msg: "password=hunter2"},
	})
	mon.SetState(chk2, monitor.State{
		Status: monitor.StatusUp,
		Since:  now.Add(-time.Hour),
		Last:   &monitor.LastProbe{At: now, Status: monitor.StatusUp, LatencyMS: 12, Msg: "1/1 running on pi-home"},
	})

	if err := db.AddResult(ctx, store.Result{CheckID: chk1, TS: now.Add(-2 * time.Minute), Status: monitor.StatusDown, LatencyMS: 42, Msg: "password=hunter2"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddResult(ctx, store.Result{CheckID: chk2, TS: now.Add(-2 * time.Minute), Status: monitor.StatusUp, LatencyMS: 12, Msg: "secret-stack-msg"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.OpenIncident(ctx, chk1, now.Add(-5*time.Minute), "password=hunter2 db connection failed", `{"host":"pi-home","stack":"privstack","logs":[{"c":"secret-ctr","m":"logline-xyz"}]}`); err != nil {
		t.Fatal(err)
	}

	pageID, err := db.SaveStatusPage(ctx, store.StatusPage{
		Slug:          "public-status",
		Title:         `Service Status <script>alert(1)</script> & "x"`,
		Description:   "Public overview",
		Domain:        "status.example.com",
		Published:     true,
		ShowIncidents: true,
		Items: []store.StatusPageItem{
			{Position: 0, Section: "Core", DisplayName: "Database", CheckID: chk1},
			{Position: 1, Section: "Core", DisplayName: `Website <script>alert(2)</script> & "y"`, HostID: "pi-home", Stack: "privstack"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	htmlRes := doHost(h, "GET", "moorage.example.com", "/status/public-status", "", nil)
	if htmlRes.Code != http.StatusOK {
		t.Fatalf("GET /status/public-status: got %d", htmlRes.Code)
	}
	feedRes := doHost(h, "GET", "moorage.example.com", "/status/public-status/feed.xml", "", nil)
	if feedRes.Code != http.StatusOK {
		t.Fatalf("GET /status/public-status/feed.xml: got %d", feedRes.Code)
	}
	prevData, err := srv.StatusService().Preview(ctx, pageID)
	if err != nil {
		t.Fatal(err)
	}
	prevBytes, _ := json.Marshal(prevData)

	forbidden := []string{
		"internal-db-check",
		"internal-stack-probe",
		"secret.lan",
		"8080/health",
		"privstack",
		"pi-home",
		"password=hunter2",
		"hunter2",
		"secret-stack-msg",
		"secret-ctr",
		"logline-xyz",
		"<script>alert(1)</script>",
		"<script>alert(2)</script>",
	}
	for _, corpus := range []struct {
		name string
		raw  string
	}{
		{"HTML", htmlRes.Body.String()},
		{"RSS", feedRes.Body.String()},
		{"PreviewJSON", string(prevBytes)},
	} {
		for _, secret := range forbidden {
			if strings.Contains(corpus.raw, secret) {
				t.Errorf("%s leaked forbidden string %q:\n%s", corpus.name, secret, corpus.raw)
			}
		}
		if !strings.Contains(corpus.raw, "Database") {
			t.Errorf("%s missing public display name Database", corpus.name)
		}
	}

	// Verify HTML escaping and XML validity
	if !strings.Contains(htmlRes.Body.String(), "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("HTML did not escape script tag in title:\n%s", htmlRes.Body.String())
	}
	var parsedXML any
	if err := xml.Unmarshal(feedRes.Body.Bytes(), &parsedXML); err != nil {
		t.Fatalf("RSS with special characters did not parse as valid XML: %v\n%s", err, feedRes.Body.String())
	}
}

// 2. Routing, custom domain isolation, separate status listener, uniform 404, and strict headers.
func TestStatusPageRoutingDomainsListenerAndHeaders(t *testing.T) {
	_, db, _, mainH, statusH := newStatusTestEnv(t, "https://moorage.example.com")
	ctx := context.Background()

	_, err := db.SaveStatusPage(ctx, store.StatusPage{
		Slug: "main", Title: "Main Status", Domain: "status.example.com", Published: true, ShowIncidents: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.SaveStatusPage(ctx, store.StatusPage{
		Slug: "other", Title: "Other Status", Published: true, ShowIncidents: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.SaveStatusPage(ctx, store.StatusPage{
		Slug: "draft", Title: "Draft Status", Domain: "draft.example.com", Published: false, ShowIncidents: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Host: status.example.com serves / and /feed.xml and /status-assets/*
	wRoot := doHost(mainH, "GET", "status.example.com", "/", "", nil)
	if wRoot.Code != http.StatusOK || !strings.Contains(wRoot.Body.String(), "Main Status") {
		t.Fatalf("custom domain /: code=%d body=%s", wRoot.Code, wRoot.Body.String())
	}
	// Headers check: strict CSP, no Set-Cookie
	if csp := wRoot.Header().Get("Content-Security-Policy"); !strings.HasPrefix(csp, "default-src 'none';") {
		t.Fatalf("unexpected CSP on status page: %q", csp)
	}
	if sc := wRoot.Header().Get("Set-Cookie"); sc != "" {
		t.Fatalf("status page must never set cookies, got %q", sc)
	}
	// POST to a status path is refused (405)
	if wPost := doHost(mainH, "POST", "status.example.com", "/", "{}", nil); wPost.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST to custom domain / should be 405, got %d", wPost.Code)
	}
	if wPost2 := doHost(mainH, "POST", "moorage.example.com", "/status/main", "{}", nil); wPost2.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST to /status/main should be 405, got %d", wPost2.Code)
	}

	// Host with uppercase and port matches: STATUS.example.com:443
	wUpper := doHost(mainH, "GET", "STATUS.example.com:443", "/feed.xml", "", nil)
	if wUpper.Code != http.StatusOK || wUpper.Header().Get("Content-Type") != "application/rss+xml; charset=utf-8" {
		t.Fatalf("uppercase Host with port /feed.xml failed: code=%d ct=%q", wUpper.Code, wUpper.Header().Get("Content-Type"))
	}

	// Everything else on status.example.com returns 404 (including /api/session, SPA assets, /status/other)
	for _, blockedPath := range []string{"/api/session", "/api/hosts", "/status/other", "/status/main", "/assets/index.js", "/login"} {
		w := doHost(mainH, "GET", "status.example.com", blockedPath, "", nil)
		if w.Code != http.StatusNotFound {
			t.Errorf("custom domain path %s should return 404, got %d", blockedPath, w.Code)
		}
	}

	// Separate status listener serves /status/main and custom domain, but NEVER /api/... or the SPA at /
	if w := doHost(statusH, "GET", "any.host", "/status/main", "", nil); w.Code != http.StatusOK {
		t.Fatalf("status listener /status/main: got %d", w.Code)
	}
	if w := doHost(statusH, "GET", "status.example.com", "/", "", nil); w.Code != http.StatusOK {
		t.Fatalf("status listener custom domain /: got %d", w.Code)
	}
	for _, p := range []string{"/", "/api/session", "/api/checks"} {
		if w := doHost(statusH, "GET", "moorage.example.com", p, "", nil); w.Code != http.StatusNotFound {
			t.Errorf("status listener path %s on admin host must return 404, got %d", p, w.Code)
		}
	}

	// Unknown slug, unpublished page, and unknown domain on status listener -> identical 404 body and status.
	wUnknownSlug := doHost(mainH, "GET", "moorage.example.com", "/status/does-not-exist", "", nil)
	wDraftSlug := doHost(mainH, "GET", "moorage.example.com", "/status/draft", "", nil)
	wDraftDom := doHost(mainH, "GET", "draft.example.com", "/", "", nil)
	wUnknownDom := doHost(statusH, "GET", "unknown.example.com", "/", "", nil)

	for _, w := range []*httptest.ResponseRecorder{wUnknownSlug, wDraftSlug, wDraftDom, wUnknownDom} {
		if w.Code != http.StatusNotFound || w.Body.String() != wUnknownSlug.Body.String() {
			t.Fatalf("non-uniform 404 response: code=%d body=%q vs %q", w.Code, w.Body.String(), wUnknownSlug.Body.String())
		}
	}
}

// 3. Validation: domain equal to MOORAGE_PUBLIC_URL host refused, duplicate slug/domain refused, bad slug refused.
func TestStatusPageValidation(t *testing.T) {
	srv, db, _, _, _ := newStatusTestEnv(t, "https://moorage.example.com:8443")
	ctx := context.Background()

	_, err := db.SaveStatusPage(ctx, store.StatusPage{
		Slug: "taken", Title: "Taken", Domain: "taken.example.com", Published: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		in   statusPageInput
		want string
	}{
		{"bad_slug_upper", statusPageInput{Title: "A", Slug: "BadSlug"}, "slug"},
		{"bad_slug_hyphen_start", statusPageInput{Title: "A", Slug: "-bad"}, "slug"},
		{"bad_slug_slash", statusPageInput{Title: "A", Slug: "a/b"}, "slug"},
		{"dup_slug", statusPageInput{Title: "A", Slug: "taken"}, "already uses that URL slug"},
		{"dup_domain", statusPageInput{Title: "A", Slug: "ok", Domain: "taken.example.com"}, "already uses that domain"},
		{"domain_matches_public_url", statusPageInput{Title: "A", Slug: "ok", Domain: "moorage.example.com"}, "different from MOORAGE_PUBLIC_URL"},
		{"domain_with_scheme", statusPageInput{Title: "A", Slug: "ok", Domain: "https://status.example.com"}, "without https://"},
		{"domain_with_port", statusPageInput{Title: "A", Slug: "ok", Domain: "status.example.com:8080"}, "without https://"},
	}
	for _, tc := range cases {
		err := srv.validateStatusPage(ctx, 0, &tc.in)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got err=%v, want substring %q", tc.name, err, tc.want)
		}
	}
}

// 4. States, daily bars (UTC day boundary 23:59:59 vs 00:00:00, silenced day not red, 30 bars),
// cascade on check deletion, and RSS durations (40s -> "40 s", 2m -> "2 min", ongoing incident).
func TestStatusPageStatesDailyBarsCascadeAndRSS(t *testing.T) {
	srv, db, mon, h, _ := newStatusTestEnv(t, "https://moorage.example.com")
	ctx := context.Background()
	// Fixed UTC clock: 2026-11-22 12:00:00 UTC
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	srv.StatusService().Now = func() time.Time { return now }

	mkCheck := func(name, hostID, stack string, enabled bool) int64 {
		id, err := db.SaveCheck(ctx, store.Check{
			Name: name, Kind: monitor.KindHTTP, Target: "https://x", HostID: hostID, Stack: stack,
			Interval: 60, Timeout: 5, Retries: 2, Enabled: enabled,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	cPending := mkCheck("c-pending", "", "", true)
	cSilenced := mkCheck("c-silenced", "local", "s1", true)
	cDisabled := mkCheck("c-disabled", "", "", false)
	cStackUp := mkCheck("c-stack-up", "local", "app", true)
	cStackDown := mkCheck("c-stack-down", "local", "app", true)
	cToDelete := mkCheck("c-delete", "", "", true)

	mon.SetState(cPending, monitor.State{Status: monitor.StatusPending})
	mon.SetState(cSilenced, monitor.State{
		Status: monitor.StatusUp,
		Last:   &monitor.LastProbe{At: now, Status: monitor.StatusSilenced},
	})
	mon.SetState(cDisabled, monitor.State{Status: monitor.StatusDown}) // disabled check must show No data even if monitor had state
	mon.SetState(cStackUp, monitor.State{Status: monitor.StatusUp})
	mon.SetState(cStackDown, monitor.State{Status: monitor.StatusDown})
	mon.SetState(cToDelete, monitor.State{Status: monitor.StatusUp})

	// Day boundary test on cPending:
	// Result at 2026-11-21 23:59:59 UTC (down) vs 2026-11-22 00:00:00 UTC (up).
	dayYesterdayEnd := time.Date(2026, 11, 21, 23, 59, 59, 0, time.UTC)
	dayTodayStart := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	_ = db.AddResult(ctx, store.Result{CheckID: cPending, TS: dayYesterdayEnd, Status: monitor.StatusDown})
	_ = db.AddResult(ctx, store.Result{CheckID: cPending, TS: dayTodayStart, Status: monitor.StatusUp})

	// Day with only silenced results on cSilenced must not be down (red) and must not lower uptime:
	_ = db.AddResult(ctx, store.Result{CheckID: cSilenced, TS: dayYesterdayEnd, Status: monitor.StatusSilenced})
	_ = db.AddResult(ctx, store.Result{CheckID: cSilenced, TS: dayTodayStart, Status: monitor.StatusUp})

	// Incidents on cPending:
	// 1) Closed 40-second incident -> must say "40 s", never "0 min"
	inc1, _ := db.OpenIncident(ctx, cPending, now.Add(-20*time.Minute), "secret", "{}")
	_ = db.CloseIncident(ctx, inc1, now.Add(-20*time.Minute+40*time.Second))
	// 2) Closed 2-minute incident -> must say "2 min"
	inc2, _ := db.OpenIncident(ctx, cPending, now.Add(-10*time.Minute), "secret", "{}")
	_ = db.CloseIncident(ctx, inc2, now.Add(-8*time.Minute))
	// 3) Ongoing incident -> only the "down" RSS item, no "back" item
	_, _ = db.OpenIncident(ctx, cPending, now.Add(-3*time.Minute), "secret", "{}")

	pageID, err := db.SaveStatusPage(ctx, store.StatusPage{
		Slug: "ops", Title: "Ops", Published: true, ShowIncidents: true,
		Items: []store.StatusPageItem{
			{Position: 0, DisplayName: "PendingSvc", CheckID: cPending},
			{Position: 1, DisplayName: "SilencedSvc", CheckID: cSilenced},
			{Position: 2, DisplayName: "DisabledSvc", CheckID: cDisabled},
			{Position: 3, DisplayName: "AppStack", HostID: "local", Stack: "app"},
			{Position: 4, DisplayName: "EphemeralSvc", CheckID: cToDelete},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Delete cToDelete -> ON DELETE CASCADE removes EphemeralSvc from the status page.
	if err := db.DeleteCheck(ctx, cToDelete); err != nil {
		t.Fatal(err)
	}
	srv.StatusService().Invalidate()

	pub, err := srv.StatusService().Preview(ctx, pageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pub.Sections) != 1 || len(pub.Sections[0].Items) != 4 {
		t.Fatalf("expected 4 items after cascade delete, got %+v", pub.Sections)
	}
	items := pub.Sections[0].Items
	if items[0].Status != status.StateOperational {
		t.Errorf("pending check should map to operational, got %s", items[0].Status)
	}
	if items[1].Status != status.StateMaintenance || items[1].Uptime30dStr != "100 %" {
		t.Errorf("silenced check: status=%s uptime30d=%s", items[1].Status, items[1].Uptime30dStr)
	}
	if items[2].Status != status.StateNoData {
		t.Errorf("disabled check should map to nodata, got %s", items[2].Status)
	}
	if items[3].Status != status.StateDown {
		t.Errorf("stack item should take worst status (down), got %s", items[3].Status)
	}

	// Verify 30 daily bars and UTC day boundary on PendingSvc:
	// index 28 = yesterday (down), index 29 = today (operational).
	days := items[0].Days
	if len(days) != 30 {
		t.Fatalf("want 30 daily bars, got %d", len(days))
	}
	if days[28].Status != status.StateDown || days[29].Status != status.StateOperational {
		t.Fatalf("UTC day boundary failed: yesterday=%+v today=%+v", days[28], days[29])
	}
	// SilencedSvc yesterday had only silenced results -> maintenance (yellow), not down (red).
	if items[1].Days[28].Status != status.StateMaintenance {
		t.Fatalf("day with only silenced results should be maintenance, got %+v", items[1].Days[28])
	}

	// RSS verification
	w1 := doHost(h, "GET", "moorage.example.com", "/status/ops/feed.xml", "", nil)
	w2 := doHost(h, "GET", "moorage.example.com", "/status/ops/feed.xml", "", nil)
	if w1.Code != http.StatusOK || w1.Header().Get("Content-Type") != "application/rss+xml; charset=utf-8" {
		t.Fatalf("feed response: code=%d ct=%q", w1.Code, w1.Header().Get("Content-Type"))
	}
	var doc1, doc2 struct {
		Channel struct {
			Items []struct {
				Title string `xml:"title"`
				GUID  string `xml:"guid"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(w1.Body.Bytes(), &doc1); err != nil {
		t.Fatalf("unmarshal RSS: %v", err)
	}
	_ = xml.Unmarshal(w2.Body.Bytes(), &doc2)
	// 2 closed incidents (2 items each = 4) + 1 ongoing incident (1 down item) = 5 items.
	if len(doc1.Channel.Items) != 5 {
		t.Fatalf("want 5 RSS items (ongoing has only down), got %d: %+v", len(doc1.Channel.Items), doc1.Channel.Items)
	}
	var titles []string
	for i, it := range doc1.Channel.Items {
		titles = append(titles, it.Title)
		if it.GUID == "" || it.GUID != doc2.Channel.Items[i].GUID {
			t.Fatalf("unstable GUID at %d: %q vs %q", i, it.GUID, doc2.Channel.Items[i].GUID)
		}
	}
	joinedTitles := strings.Join(titles, " | ")
	if !strings.Contains(joinedTitles, "PendingSvc is back after 40 s") {
		t.Errorf("40s outage missing or rounded to 0 min: %s", joinedTitles)
	}
	if !strings.Contains(joinedTitles, "PendingSvc is back after 2 min") {
		t.Errorf("2m outage missing: %s", joinedTitles)
	}
}

// 5. Cache singleflight (100 concurrent requests build once), invalidation on edit, and TTL expiry.
func TestStatusPageCacheStampedeInvalidationAndTTL(t *testing.T) {
	srv, db, _, h, _ := newStatusTestEnv(t, "https://moorage.example.com")
	ctx := context.Background()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	srv.StatusService().Now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	srv.StatusService().TTL = 60 * time.Second

	var builds atomic.Int32
	srv.StatusService().OnRebuild = func(int64) {
		builds.Add(1)
		time.Sleep(15 * time.Millisecond) // widen window so concurrent callers overlap
	}

	pageID, err := db.SaveStatusPage(ctx, store.StatusPage{
		Slug: "cached", Title: "Title v1", Published: true, ShowIncidents: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 100 concurrent requests within TTL -> exactly 1 build
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := doHost(h, "GET", "moorage.example.com", "/status/cached", "", nil)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Title v1") {
				t.Errorf("unexpected response: %d", w.Code)
			}
		}()
	}
	wg.Wait()
	if got := builds.Load(); got != 1 {
		t.Fatalf("100 concurrent requests triggered %d builds, want 1", got)
	}

	// Edit page and invalidate -> next request immediately reflects "Title v2"
	_, err = db.SaveStatusPage(ctx, store.StatusPage{
		ID: pageID, Slug: "cached", Title: "Title v2", Published: true, ShowIncidents: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.StatusService().Invalidate()

	w2 := doHost(h, "GET", "moorage.example.com", "/status/cached", "", nil)
	if !strings.Contains(w2.Body.String(), "Title v2") || builds.Load() != 2 {
		t.Fatalf("after edit+invalidate: builds=%d bodyHasV2=%v", builds.Load(), strings.Contains(w2.Body.String(), "Title v2"))
	}

	// Advance clock past TTL -> rebuilds once
	clockMu.Lock()
	now = now.Add(61 * time.Second)
	clockMu.Unlock()

	_ = doHost(h, "GET", "moorage.example.com", "/status/cached", "", nil)
	_ = doHost(h, "GET", "moorage.example.com", "/status/cached", "", nil)
	if got := builds.Load(); got != 3 {
		t.Fatalf("after TTL expiry: builds=%d, want 3", got)
	}
}

// Review fixes (Claude): a page with its own domain must never link to the admin
// URL; a stack item shows one outage for overlapping incidents of its checks, its
// uptime is its weakest check, and feed GUIDs don't depend on the item's position.
func TestStatusPageReviewRegressions(t *testing.T) {
	srv, db, mon, h, _ := newStatusTestEnv(t, "https://admin.example.com")
	ctx := context.Background()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	srv.StatusService().Now = func() time.Time { return now }

	stackChk, _ := db.SaveCheck(ctx, store.Check{Name: "c-stack", Kind: monitor.KindStack, HostID: "h1", Stack: "web", Interval: 60, Timeout: 5, Retries: 1, Enabled: true})
	logChk, _ := db.SaveCheck(ctx, store.Check{Name: "c-log", Kind: monitor.KindLog, HostID: "h1", Stack: "web", Interval: 60, Timeout: 5, Retries: 1, Enabled: true})
	mon.SetState(stackChk, monitor.State{Status: monitor.StatusDown})
	mon.SetState(logChk, monitor.State{Status: monitor.StatusUp})
	for i := 0; i < 10; i++ {
		ts := now.Add(-time.Duration(i+1) * time.Minute)
		_ = db.AddResult(ctx, store.Result{CheckID: stackChk, TS: ts, Status: monitor.StatusDown})
		_ = db.AddResult(ctx, store.Result{CheckID: logChk, TS: ts, Status: monitor.StatusUp})
	}
	// Two overlapping incidents of the same stack (stack check + log rule) …
	inc1, _ := db.OpenIncident(ctx, stackChk, now.Add(-50*time.Minute), "x", "{}")
	inc2, _ := db.OpenIncident(ctx, logChk, now.Add(-40*time.Minute), "x", "{}")
	_ = db.CloseIncident(ctx, inc2, now.Add(-35*time.Minute))
	_ = db.CloseIncident(ctx, inc1, now.Add(-30*time.Minute))
	// … and a later separate one.
	inc3, _ := db.OpenIncident(ctx, logChk, now.Add(-10*time.Minute), "x", "{}")

	save := func(items []statusPageItemDTO) {
		body, _ := json.Marshal(statusPageInput{Slug: "p", Title: "P", Domain: "status.example.com", Published: true, ShowIncidents: true, Items: items})
		pageID := int64(0)
		if pages, _ := db.StatusPages(ctx); len(pages) > 0 {
			pageID = pages[0].ID
		}
		in := statusPageInput{}
		_ = json.Unmarshal(body, &in)
		if err := srv.validateStatusPage(ctx, pageID, &in); err != nil {
			t.Fatal(err)
		}
		if _, err := db.SaveStatusPage(ctx, toStoreStatusPage(pageID, in)); err != nil {
			t.Fatal(err)
		}
		srv.StatusService().Invalidate()
	}
	web := statusPageItemDTO{DisplayName: "Web", HostID: "h1", Stack: "web"}
	other := statusPageItemDTO{DisplayName: "Logs", CheckID: logChk}
	save([]statusPageItemDTO{web, other})

	feed := doHost(h, "GET", "status.example.com", "/feed.xml", "", nil).Body.String()
	if strings.Contains(feed, "admin.example.com") {
		t.Fatalf("feed reveals the admin URL:\n%s", feed)
	}
	if !strings.Contains(feed, "https://status.example.com/") {
		t.Fatalf("feed should link to the page's domain:\n%s", feed)
	}
	slugFeed := doHost(h, "GET", "admin.example.com", "/status/p/feed.xml", "", nil).Body.String()
	if strings.Contains(slugFeed, "admin.example.com") {
		t.Fatalf("feed via slug reveals the admin URL")
	}

	pub, err := srv.StatusService().Preview(ctx, mustPageID(t, db))
	if err != nil {
		t.Fatal(err)
	}
	var webIncs int
	for _, in := range pub.Incidents {
		if in.DisplayName == "Web" {
			webIncs++
		}
	}
	if webIncs != 2 {
		t.Fatalf("stack item: want 2 outages (merged + later one), got %d: %+v", webIncs, pub.Incidents)
	}
	if u := pub.Sections[0].Items[0].Uptime24h; u == nil || *u != 0 {
		t.Fatalf("stack uptime must be its weakest check (0 %%), got %v", u)
	}

	guids := func() map[string]bool {
		out := map[string]bool{}
		var doc struct {
			Items []struct {
				GUID  string `xml:"guid"`
				Title string `xml:"title"`
			} `xml:"channel>item"`
		}
		body := doHost(h, "GET", "status.example.com", "/feed.xml", "", nil).Body.Bytes()
		if err := xml.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		for _, it := range doc.Items {
			out[it.Title+"|"+it.GUID] = true
		}
		return out
	}
	before := guids()
	save([]statusPageItemDTO{other, web}) // reorder
	after := guids()
	if len(before) == 0 || len(before) != len(after) {
		t.Fatalf("feed changed size after reorder: %v vs %v", before, after)
	}
	for k := range before {
		if !after[k] {
			t.Fatalf("GUID changed after reordering items: %s not in %v", k, after)
		}
	}
	_ = inc3
}

func mustPageID(t *testing.T, db *store.Store) int64 {
	t.Helper()
	pages, err := db.StatusPages(context.Background())
	if err != nil || len(pages) == 0 {
		t.Fatal("no status page", err)
	}
	return pages[0].ID
}

// Preview must never save: previewing edits of a published page used to publish them.
func TestStatusPageDraftPreviewDoesNotSave(t *testing.T) {
	srv, db, _, h, _ := newStatusTestEnv(t, "")
	ctx := context.Background()
	id, err := db.SaveStatusPage(ctx, store.StatusPage{Slug: "live", Title: "Old title", Published: true, ShowIncidents: true})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"id":` + strconv.FormatInt(id, 10) + `,"slug":"live","title":"New title","published":true,"showIncidents":true,"domain":"Status.Example.COM","items":[]}`
	// The route is admin-only; call the handler directly (auth is covered elsewhere).
	res := doHost(http.HandlerFunc(srv.previewDraft), "POST", "", "/api/status-pages/preview", body, nil)
	if unauth := doHost(h, "POST", "", "/api/status-pages/preview", body, map[string]string{"X-Moorage": "1"}); unauth.Code != http.StatusUnauthorized {
		t.Fatalf("draft preview without sign-in: got %d", unauth.Code)
	}
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "New title") {
		t.Fatalf("draft preview: %d %s", res.Code, res.Body.String())
	}
	p, _ := db.StatusPageByID(ctx, id)
	if p.Title != "Old title" || p.Domain != "" {
		t.Fatalf("preview saved the page: %+v", p)
	}
	pub := doHost(h, "GET", "", "/status/live", "", nil).Body.String()
	if strings.Contains(pub, "New title") {
		t.Fatal("unsaved title is public")
	}
}
