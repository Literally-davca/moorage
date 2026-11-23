package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"moorage/internal/monitor"
	"moorage/internal/status"
	"moorage/internal/store"
)

type maintEnv struct {
	srv *Server
	db  *store.Store
	mon *monitor.Monitor
	h   http.Handler
}

func newMaintEnv(t *testing.T) *maintEnv {
	t.Helper()
	srv, db, mon, h, _ := newStatusTestEnv(t, "https://moorage.example.com")
	createTestUserSession(t, db, "admin", "admin", "tok-admin")
	createTestUserSession(t, db, "viewer", "viewer", "tok-viewer")
	return &maintEnv{srv: srv, db: db, mon: mon, h: h}
}

func (e *maintEnv) req(method, path, body, token string) *maintResp {
	hdr := map[string]string{csrfHeader: "1", "Content-Type": "application/json", "Cookie": cookieName + "=" + token}
	w := do(e.h, method, path, body, hdr)
	return &maintResp{Code: w.Code, Body: w.Body.String()}
}

// maintResp keeps the code and body of a recorded response.
type maintResp struct {
	Code int
	Body string
}

func jsonBody(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func onceBody(title string, start, end time.Time, scope store.MaintenanceScope) string {
	return jsonBody(map[string]any{
		"title": title, "message": "We upgrade the database.", "kind": "once",
		"startsAt": start, "endsAt": end, "scope": scope, "enabled": true,
	})
}

func TestMaintenanceAPIValidationRolesAndAudit(t *testing.T) {
	e := newMaintEnv(t)
	ctx := context.Background()
	chk, err := e.db.SaveCheck(ctx, store.Check{Name: "web", Kind: monitor.KindHTTP, Target: "https://x", Interval: 60, Timeout: 5, Retries: 1, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	stackScope := store.MaintenanceScope{Stacks: []store.MaintenanceStack{{HostID: "local", Stack: "web"}}}

	bad := map[string]string{
		"end before start":   onceBody("x", now.Add(time.Hour), now, stackScope),
		"too short":          onceBody("x", now, now.Add(4*time.Minute), stackScope),
		"too long":           onceBody("x", now, now.Add(7*24*time.Hour+time.Minute), stackScope),
		"already over":       onceBody("x", now.Add(-2*time.Hour), now.Add(-time.Hour), stackScope),
		"empty scope":        onceBody("x", now, now.Add(time.Hour), store.MaintenanceScope{}),
		"unknown check":      onceBody("x", now, now.Add(time.Hour), store.MaintenanceScope{Checks: []int64{chk + 99}}),
		"bad stack":          onceBody("x", now, now.Add(time.Hour), store.MaintenanceScope{Stacks: []store.MaintenanceStack{{HostID: "local", Stack: "Bad Name"}}}),
		"no title":           onceBody(" ", now, now.Add(time.Hour), stackScope),
		"no weekdays":        jsonBody(map[string]any{"title": "x", "kind": "weekly", "startTime": "02:00", "duration": 60, "scope": stackScope}),
		"bad weekday":        jsonBody(map[string]any{"title": "x", "kind": "weekly", "weekdays": []int{7}, "startTime": "02:00", "duration": 60, "scope": stackScope}),
		"bad time":           jsonBody(map[string]any{"title": "x", "kind": "weekly", "weekdays": []int{0}, "startTime": "25:00", "duration": 60, "scope": stackScope}),
		"weekly too long":    jsonBody(map[string]any{"title": "x", "kind": "weekly", "weekdays": []int{0}, "startTime": "02:00", "duration": 7*24*60 + 1, "scope": stackScope}),
		"unknown time zone":  jsonBody(map[string]any{"title": "x", "kind": "weekly", "weekdays": []int{0}, "startTime": "02:00", "duration": 60, "timeZone": "Mars/Olympus", "scope": stackScope}),
		"unknown kind":       jsonBody(map[string]any{"title": "x", "kind": "daily", "scope": stackScope}),
		"missing start time": onceBody("x", time.Time{}, now.Add(time.Hour), stackScope),
	}
	for name, body := range bad {
		if r := e.req("POST", "/api/maintenance", body, "tok-admin"); r.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d %s", name, r.Code, r.Body)
		}
	}

	// viewers can list but not change
	if r := e.req("POST", "/api/maintenance", onceBody("x", now, now.Add(time.Hour), stackScope), "tok-viewer"); r.Code != http.StatusForbidden {
		t.Fatalf("viewer create: %d", r.Code)
	}

	r := e.req("POST", "/api/maintenance", onceBody("DB upgrade", now.Add(-time.Minute), now.Add(time.Hour), store.MaintenanceScope{Checks: []int64{chk}, Stacks: stackScope.Stacks}), "tok-admin")
	if r.Code != http.StatusOK {
		t.Fatalf("create: %d %s", r.Code, r.Body)
	}
	var created struct{ ID int64 }
	_ = json.Unmarshal([]byte(r.Body), &created)
	weeklyBody := jsonBody(map[string]any{"title": "Weekly backup", "kind": "weekly", "weekdays": []int{0, 0, 3}, "startTime": "02:30",
		"duration": 90, "timeZone": "Europe/Prague", "scope": map[string]any{"all": true, "checks": []int64{chk}}, "enabled": true})
	if r := e.req("POST", "/api/maintenance", weeklyBody, "tok-admin"); r.Code != http.StatusOK {
		t.Fatalf("create weekly: %d %s", r.Code, r.Body)
	}

	r = e.req("GET", "/api/maintenance", "", "tok-viewer")
	if r.Code != http.StatusOK {
		t.Fatalf("viewer list: %d", r.Code)
	}
	var list struct {
		Windows []maintDTO `json:"windows"`
	}
	if err := json.Unmarshal([]byte(r.Body), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Windows) != 2 || list.Windows[0].State != maintActive || list.Windows[0].Current == nil {
		t.Fatalf("list: %+v", list.Windows)
	}
	var wk maintDTO
	for _, w := range list.Windows {
		if w.Title == "Weekly backup" {
			wk = w
		}
	}
	if (wk.State != maintUpcoming && wk.State != maintActive) || wk.Next == nil && wk.Current == nil || !wk.Scope.All || len(wk.Scope.Checks) != 0 || len(wk.Weekdays) != 2 {
		t.Fatalf("weekly window: %+v", wk)
	}
	var db maintDTO
	for _, w := range list.Windows {
		if w.Title == "DB upgrade" {
			db = w
		}
	}
	if got := strings.Join(db.ScopeText, "|"); db.State != maintActive || got != "web on local|web" {
		t.Fatalf("scope text: %q", got)
	}

	// the check page and the stack page show the running window
	r = e.req("GET", fmt.Sprintf("/api/checks/%d", chk), "", "tok-viewer")
	if !strings.Contains(r.Body, `"maintenance":{"id":`+fmt.Sprint(created.ID)) {
		t.Fatalf("check DTO without maintenance: %s", r.Body)
	}
	r = e.req("GET", "/api/hosts/local/stacks/web/maintenance", "", "tok-viewer")
	if r.Code != http.StatusOK || !strings.Contains(r.Body, `"title":"DB upgrade"`) || !strings.Contains(r.Body, "Started: DB upgrade") {
		t.Fatalf("stack maintenance: %d %s", r.Code, r.Body)
	}

	// edit, then delete; an unknown id is 404
	path := fmt.Sprintf("/api/maintenance/%d", created.ID)
	if r := e.req("PUT", path, onceBody("DB upgrade v2", now.Add(-time.Minute), now.Add(2*time.Hour), stackScope), "tok-admin"); r.Code != http.StatusOK {
		t.Fatalf("update: %d %s", r.Code, r.Body)
	}
	if r := e.req("PUT", path, onceBody("x", now, now.Add(time.Hour), stackScope), "tok-viewer"); r.Code != http.StatusForbidden {
		t.Fatalf("viewer update: %d", r.Code)
	}
	if r := e.req("DELETE", path, "", "tok-viewer"); r.Code != http.StatusForbidden {
		t.Fatalf("viewer delete: %d", r.Code)
	}
	if r := e.req("DELETE", path, "", "tok-admin"); r.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", r.Code)
	}
	if r := e.req("DELETE", path, "", "tok-admin"); r.Code != http.StatusNotFound {
		t.Fatalf("delete again: %d", r.Code)
	}
	if r := e.req("PUT", "/api/maintenance/999", onceBody("x", now, now.Add(time.Hour), stackScope), "tok-admin"); r.Code != http.StatusNotFound {
		t.Fatalf("update unknown: %d", r.Code)
	}
	// the cut-short window ended early in the stack's logbook
	r = e.req("GET", "/api/hosts/local/stacks/web/maintenance", "", "tok-viewer")
	if strings.Contains(r.Body, `"active"`) || !strings.Contains(r.Body, "Ended early: DB upgrade") {
		t.Fatalf("after delete: %s", r.Body)
	}

	rows := auditRows(t, e.db)
	var actions []string
	for _, a := range rows {
		actions = append(actions, a.Action+":"+a.Target)
	}
	got := strings.Join(actions, " ")
	for _, want := range []string{"maintenance.create:", "maintenance.save:" + fmt.Sprint(created.ID), "maintenance.delete:" + fmt.Sprint(created.ID)} {
		if !strings.Contains(got, want) {
			t.Errorf("audit rows %q lack %q", got, want)
		}
	}
	if len(rows) != 4 { // 2 creates, 1 save, 1 delete; refusals are not recorded
		t.Errorf("want 4 audit rows, got %d: %q", len(rows), got)
	}
}

func TestMaintenanceStatusPage(t *testing.T) {
	e := newMaintEnv(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	e.srv.StatusService().Now = func() time.Time { return now }

	inStack, _ := e.db.SaveCheck(ctx, store.Check{Name: "web-stack", Kind: monitor.KindStack, Target: "web", HostID: "local", Stack: "web",
		Interval: 60, Timeout: 5, Retries: 1, Enabled: true})
	other, _ := e.db.SaveCheck(ctx, store.Check{Name: "mail", Kind: monitor.KindHTTP, Target: "https://mail", Interval: 60, Timeout: 5, Retries: 1, Enabled: true})
	hidden, _ := e.db.SaveCheck(ctx, store.Check{Name: "hidden", Kind: monitor.KindHTTP, Target: "https://h", Interval: 60, Timeout: 5, Retries: 1, Enabled: true})
	_ = e.db.AddResult(ctx, store.Result{CheckID: inStack, TS: now.Add(-time.Minute), Status: monitor.StatusMaintenance})
	_ = e.db.AddResult(ctx, store.Result{CheckID: inStack, TS: now.Add(-2 * time.Minute), Status: monitor.StatusUp})
	_ = e.db.AddResult(ctx, store.Result{CheckID: other, TS: now.Add(-time.Minute), Status: monitor.StatusUp})
	e.mon.SetState(inStack, monitor.State{Status: monitor.StatusMaintenance, Since: now.Add(-time.Minute)})
	e.mon.SetState(other, monitor.State{Status: monitor.StatusUp, Since: now.Add(-time.Hour)})

	pageID, err := e.db.SaveStatusPage(ctx, store.StatusPage{Slug: "acme", Title: "Acme", Published: true, ShowIncidents: false,
		Items: []store.StatusPageItem{
			{Position: 0, DisplayName: "Website", HostID: "local", Stack: "web"},
			{Position: 1, DisplayName: "Mail", CheckID: other},
		}})
	if err != nil {
		t.Fatal(err)
	}
	post := func(body string) {
		t.Helper()
		if r := e.req("POST", "/api/maintenance", body, "tok-admin"); r.Code != http.StatusOK {
			t.Fatalf("create: %d %s", r.Code, r.Body)
		}
	}
	post(onceBody("Database upgrade", now.Add(-10*time.Minute), now.Add(50*time.Minute),
		store.MaintenanceScope{Stacks: []store.MaintenanceStack{{HostID: "local", Stack: "web"}}}))
	post(onceBody("Mail server move", now.Add(24*time.Hour), now.Add(26*time.Hour), store.MaintenanceScope{Checks: []int64{other}}))
	post(onceBody("Secret internal work", now.Add(-time.Minute), now.Add(time.Hour), store.MaintenanceScope{Checks: []int64{hidden}}))
	post(onceBody("Far future", now.Add(8*24*time.Hour), now.Add(8*24*time.Hour+time.Hour), store.MaintenanceScope{Checks: []int64{other}}))

	pub, err := e.srv.StatusService().Preview(ctx, pageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pub.Maintenance) != 1 || pub.Maintenance[0].Title != "Database upgrade" || pub.Maintenance[0].Message != "We upgrade the database." ||
		!pub.Maintenance[0].EndsAt.Equal(now.Add(50*time.Minute)) || strings.Join(pub.Maintenance[0].Items, ",") != "Website" {
		t.Fatalf("active maintenance: %+v", pub.Maintenance)
	}
	if len(pub.Planned) != 1 || pub.Planned[0].Title != "Mail server move" {
		t.Fatalf("planned maintenance (next 7 days only): %+v", pub.Planned)
	}
	items := pub.Sections[0].Items
	if items[0].Status != status.StateMaintenance || items[0].StatusLabel != "Maintenance" || items[1].Status != status.StateOperational {
		t.Fatalf("item states: %s %s", items[0].Status, items[1].Status)
	}
	if pub.BannerStatus != status.StateMaintenance {
		t.Fatalf("banner: %s %s", pub.BannerStatus, pub.BannerText)
	}
	if d := items[0].Days[29]; d.Status != status.StateMaintenance {
		t.Fatalf("today's bar: %+v", d)
	}
	// uptime: the maintenance result doesn't count, the up one does
	if items[0].Uptime24hStr != "100 %" {
		t.Fatalf("uptime counted maintenance: %s", items[0].Uptime24hStr)
	}

	html := doHost(e.h, "GET", "moorage.example.com", "/status/acme", "", nil).Body.String()
	for _, want := range []string{"Database upgrade", "We upgrade the database.", "Planned end", "Planned maintenance", "Mail server move"} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML lacks %q", want)
		}
	}
	feed := doHost(e.h, "GET", "moorage.example.com", "/status/acme/feed.xml", "", nil).Body.String()
	if !strings.Contains(feed, "Maintenance: Database upgrade") || !strings.Contains(feed, "maint-") || strings.Contains(feed, "Mail server move") {
		t.Errorf("feed: %s", feed)
	}
	// a window covering nothing on the page never shows there
	for _, body := range []string{html, feed} {
		if strings.Contains(body, "Secret internal work") || strings.Contains(body, "Far future") {
			t.Fatal("a window outside the page leaked onto it")
		}
	}
}
