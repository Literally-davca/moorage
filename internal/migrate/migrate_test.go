package migrate

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"moorage/internal/monitor"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func checkByName(t *testing.T, p *Plan, name string) CheckItem {
	t.Helper()
	for _, c := range p.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q missing; have %v", name, checkNames(p))
	return CheckItem{}
}

func checkNames(p *Plan) []string {
	var out []string
	for _, c := range p.Checks {
		out = append(out, c.Name)
	}
	return out
}

func skippedNames(p *Plan, kind string) []string {
	var out []string
	for _, s := range p.Skipped {
		if s.Kind == kind {
			out = append(out, s.Name)
		}
	}
	return out
}

func TestParseKuma(t *testing.T) {
	p, err := ParseKuma(readFixture(t, "kuma-backup.json"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Source != SourceKuma || len(p.Pages) != 0 || len(p.Notes) == 0 {
		t.Fatalf("source/pages/notes: %+v", p)
	}

	// Channels: Teams has no Moorage counterpart.
	if len(p.Channels) != 3 {
		t.Fatalf("channels: %+v", p.Channels)
	}
	byKey := map[string]ChannelItem{}
	for _, c := range p.Channels {
		byKey[c.Key] = c
	}
	if c := byKey["channel:1"]; c.Kind != "discord" || c.Config.URL != "https://discord.com/api/webhooks/123/secret-token" || c.Summary != "discord.com" {
		t.Errorf("discord: %+v", c)
	}
	if c := byKey["channel:2"]; c.Kind != "ntfy" || c.Config.URL != "https://ntfy.example.com/homelab" || c.Config.Token != "tk_abc" || !c.Enabled {
		t.Errorf("ntfy: %+v", c)
	}
	if c := byKey["channel:4"]; c.Kind != "smtp" || c.Config.Security != "tls" || c.Config.Port != 465 || c.Enabled || len(c.Warnings) != 1 {
		t.Errorf("smtp: %+v", c)
	}
	if got := skippedNames(p, "channel"); !reflect.DeepEqual(got, []string{"Teams"}) {
		t.Errorf("skipped channels: %v", got)
	}
	// A summary never carries a secret.
	for _, c := range p.Channels {
		if strings.Contains(c.Summary, "secret") || strings.Contains(c.Summary, "tk_") || strings.Contains(c.Summary, "pw") {
			t.Errorf("summary leaks a secret: %q", c.Summary)
		}
	}

	want := []string{"Website", "API health", "Postgres", "Router", "DNS", "Status JSON", "Website (2)"}
	if got := checkNames(p); !reflect.DeepEqual(got, want) {
		t.Fatalf("checks: got %v want %v", got, want)
	}
	web := checkByName(t, p, "Website")
	if web.Kind != monitor.KindHTTP || web.Settings.Status != "200-299,301" || web.Retries != 3 || web.Interval != 60 || web.Timeout != 48 ||
		!reflect.DeepEqual(web.Channels, []string{"channel:1", "channel:2"}) || !web.Enabled {
		t.Errorf("website: %+v", web)
	}
	if c := checkByName(t, p, "API health"); c.Settings.Keyword != "ok" || c.Retries != 1 || c.Interval != 30 || c.Timeout != 24 {
		t.Errorf("keyword: %+v", c)
	}
	if c := checkByName(t, p, "Postgres"); c.Kind != monitor.KindTCP || c.Target != "db.lan:5432" || c.Enabled || c.Retries != 2 {
		t.Errorf("tcp: %+v", c)
	}
	if c := checkByName(t, p, "Router"); c.Kind != monitor.KindPing || c.Interval != 20 || c.Timeout != 10 || c.Retries != 10 {
		t.Errorf("ping: %+v", c)
	}
	if c := checkByName(t, p, "DNS"); c.Kind != monitor.KindDNS || c.Settings.RecordType != "AAAA" || c.Settings.Resolver != "1.1.1.1" || c.Target != "example.com" {
		t.Errorf("dns: %+v", c)
	}
	if c := checkByName(t, p, "Status JSON"); c.Settings.JSONPath != "data.status" || c.Settings.JSONValue != "up" {
		t.Errorf("json: %+v", c)
	}
	if c := checkByName(t, p, "Website (2)"); c.Settings.Method != "HEAD" || c.Key != "check:11" {
		t.Errorf("duplicate name: %+v", c)
	}
	if got := skippedNames(p, "check"); !reflect.DeepEqual(got, []string{"Containers", "Nginx container", "Backup job", "Admin panel", "Postgres DB"}) {
		t.Errorf("skipped checks: %v", got)
	}
}

func TestParseKumaRejects(t *testing.T) {
	for name, in := range map[string]string{
		"not json":     "monitorList: []",
		"empty object": "{}",
		"wrong type":   `{"monitorList": [{"id": "x1"}]}`,
		"two values":   `{"monitorList": []} {"monitorList": []}`,
		"bad bool":     `{"monitorList": [{"id": 1, "active": "maybe"}]}`,
	} {
		if _, err := ParseKuma([]byte(in)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	big := []byte(`{"monitorList":[` + strings.Repeat(`{"id":1,"type":"ping","hostname":"x"},`, MaxChecks) + `{"id":2}]}`)
	if _, err := ParseKuma(big); !errors.Is(err, ErrInvalid) {
		t.Errorf("too many monitors: %v", err)
	}
}

func TestParseGatus(t *testing.T) {
	p, err := ParseGatus(readFixture(t, "gatus.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Channels) != 2 || p.Channels[0].Key != "channel:discord" || p.Channels[1].Kind != "smtp" || p.Channels[1].Config.Security != "starttls" {
		t.Fatalf("channels: %+v", p.Channels)
	}
	if got := skippedNames(p, "channel"); !reflect.DeepEqual(got, []string{"Gatus pagerduty", "Gatus telegram"}) {
		t.Errorf("skipped channels: %v", got)
	}
	want := []string{"website", "api (public)", "api (internal)", "postgres", "router", "dns"}
	if got := checkNames(p); !reflect.DeepEqual(got, want) {
		t.Fatalf("checks: got %v want %v", got, want)
	}
	web := checkByName(t, p, "website")
	if web.Interval != 120 || web.Settings.Status != "200" || web.Settings.Keyword != "Welcome" || web.Retries != 5 ||
		!reflect.DeepEqual(web.Channels, []string{"channel:discord"}) || len(web.Warnings) != 2 {
		t.Errorf("website: %+v", web)
	}
	api := checkByName(t, p, "api (public)")
	if api.Settings.Status != "200-399" || api.Settings.JSONPath != "status" || api.Settings.JSONValue != "UP" || api.Retries != 2 || len(api.Warnings) != 1 {
		t.Errorf("api: %+v", api)
	}
	if c := checkByName(t, p, "api (internal)"); !c.Settings.IgnoreTLS || c.Timeout != 5 || len(c.Channels) != 0 {
		t.Errorf("internal api: %+v", c)
	}
	if c := checkByName(t, p, "postgres"); c.Kind != monitor.KindTCP || c.Target != "db.lan:5432" || c.Interval != 30 || len(c.Warnings) != 0 {
		t.Errorf("tcp: %+v", c)
	}
	if c := checkByName(t, p, "router"); c.Kind != monitor.KindPing || c.Enabled || c.Interval != 20 {
		t.Errorf("ping: %+v", c)
	}
	if c := checkByName(t, p, "dns"); c.Kind != monitor.KindDNS || c.Target != "example.com" || c.Settings.Resolver != "8.8.8.8" ||
		c.Settings.Expect != "93.184.215.14" || c.Settings.RecordType != "A" || len(c.Warnings) != 0 {
		t.Errorf("dns: %+v", c)
	}
	if got := skippedNames(p, "check"); !reflect.DeepEqual(got, []string{"backups", "login", "ssh", "secret"}) {
		t.Errorf("skipped checks: %v", got)
	}
	if len(p.Pages) != 1 || p.Pages[0].Title != "Homelab status" || len(p.Pages[0].Entries) != 6 || p.Pages[0].Entries[0].Section != "public" {
		t.Fatalf("page: %+v", p.Pages)
	}
	if !strings.Contains(strings.Join(p.Notes, " "), "storage") {
		t.Errorf("notes should name storage: %v", p.Notes)
	}
}

func TestParseGatusRejects(t *testing.T) {
	for name, in := range map[string]string{
		"empty":          "",
		"list":           "- a\n- b\n",
		"no endpoints":   "alerting: {}\n",
		"wrong type":     "endpoints: yes\n",
		"duplicate keys": "endpoints: []\nendpoints: []\n",
		"two documents":  "endpoints: []\n---\nendpoints: []\n",
		"broken":         "endpoints: [\n",
	} {
		if _, err := ParseGatus([]byte(in)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

func TestGatusStatusConditions(t *testing.T) {
	for _, tc := range []struct {
		conds []string
		want  string
	}{
		{[]string{"[STATUS] == 200"}, "200"},
		{[]string{"[STATUS] == any(200, 201)"}, "200,201"},
		{[]string{"[STATUS] < 300"}, "100-299"},
		{[]string{"[STATUS] >= 200", "[STATUS] <= 299"}, ""}, // 200-299 is the default
		{[]string{"[STATUS] > 500", "[STATUS] < 400"}, ""},   // impossible: default with a warning
	} {
		it := CheckItem{Kind: monitor.KindHTTP}
		gatusConditions(&it, tc.conds)
		if it.Settings.Status != tc.want {
			t.Errorf("%v: got %q want %q", tc.conds, it.Settings.Status, tc.want)
		}
	}
}

func TestTiming(t *testing.T) {
	for _, tc := range []struct{ in, inT, iv, to int }{
		{0, 0, 60, 10},
		{5, 0, 20, 10},
		{100000, 90, 86400, 60},
		{20, 30, 20, 19},
	} {
		if iv, to := timing(tc.in, tc.inT); iv != tc.iv || to != tc.to {
			t.Errorf("timing(%d,%d) = %d,%d want %d,%d", tc.in, tc.inT, iv, to, tc.iv, tc.to)
		}
	}
}
