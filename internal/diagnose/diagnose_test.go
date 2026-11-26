package diagnose

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 26, 14, 10, 0, 0, time.UTC)

func ctx() Context { return Context{Now: now, DiskFreePct: -1} }

func only(t *testing.T, ps []Problem, rule string) Problem {
	t.Helper()
	if len(ps) != 1 || ps[0].Rule != rule {
		t.Fatalf("want one %s, got %+v", rule, ps)
	}
	return ps[0]
}

func TestHealthyAndOneOff(t *testing.T) {
	ps := Diagnose([]Facts{
		{Name: "web", State: "running", Health: "healthy", Detailed: true},
		{Name: "migrate", State: "exited", ExitCode: 0, RestartPolicy: "no", Detailed: true},
		{Name: "paused", State: "paused", Detailed: true},
	}, ctx())
	if len(ps) != 0 {
		t.Fatalf("%+v", ps)
	}
	p := only(t, Diagnose([]Facts{{Name: "x", State: "running", Health: "starting", Detailed: true}}, ctx()), "starting")
	if p.Severity != Info {
		t.Error(p)
	}
}

func TestOOMCrashLoop(t *testing.T) {
	c := ctx()
	c.Events = []Event{{Service: "server", Type: "die", Time: now.Add(-2 * time.Minute), Count: 3}}
	p := only(t, Diagnose([]Facts{{Name: "immich-server", Service: "server", State: "restarting", ExitCode: 137, OOMKilled: true, MemLimit: 512 << 20, Detailed: true}}, c), "oom")
	if !strings.Contains(p.Detail, "3×") || p.Fixes[0].Kind != "set-mem-limit" || p.Fixes[0].Value != "1g" || p.Cause != "Out of memory (exit 137)" {
		t.Errorf("%+v", p)
	}
}

func TestRules(t *testing.T) {
	cases := []struct {
		f    Facts
		c    func(*Context)
		rule string
		fix  string
	}{
		{Facts{Name: "a", State: "exited", ExitCode: 127, Error: `exec: "foo": executable file not found in $PATH`}, nil, "command-not-found", "open-compose"},
		{Facts{Name: "a", State: "exited", ExitCode: 1}, func(c *Context) { c.Logs = map[string][]string{"a": {"exec /app: exec format error"}} }, "wrong-arch", "open-images"},
		{Facts{Name: "a", State: "created", Error: "driver failed programming external connectivity: Bind for 0.0.0.0:8080 failed: port is already allocated"}, func(c *Context) { c.PortOwners = map[int]string{8080: "stack jellyfin"} }, "port-in-use", "use-free-port"},
		{Facts{Name: "a", State: "exited", ExitCode: 1}, func(c *Context) {
			c.Logs = map[string][]string{"a": {"Error: environment variable DATABASE_URL is not set"}}
		}, "missing-variable", "add-env-var"},
		{Facts{Name: "a", State: "exited", ExitCode: 1}, func(c *Context) {
			c.Logs = map[string][]string{"a": {"mkdir: cannot create directory '/config/cache': Permission denied"}}
		}, "permission-denied", "open-terminal"},
		{Facts{Name: "a", State: "exited", ExitCode: 1}, func(c *Context) {
			c.LastOpError = "Error response from daemon: manifest for postgres:99 not found: manifest unknown"
		}, "pull", "open-images"},
		{Facts{Name: "a", State: "exited", ExitCode: 1}, func(c *Context) { c.LastOpError = "toomanyrequests: You have reached your pull rate limit" }, "pull", "open-registries"},
		{Facts{Name: "a", State: "exited", ExitCode: 1}, func(c *Context) { c.Logs = map[string][]string{"a": {"write /data/x: no space left on device"}} }, "no-space", "open-disk"},
		{Facts{Name: "a", State: "exited", ExitCode: 1}, func(c *Context) {
			c.Logs = map[string][]string{"a": {"Error: connect ECONNREFUSED: connection refused to host db"}}
		}, "dependency-down", "open-deps"},
		{Facts{Name: "a", State: "running", Health: "unhealthy", HealthLog: []string{"/bin/sh: curl: not found"}}, nil, "unhealthy", "add-healthcheck"},
		{Facts{Name: "a", State: "dead", Error: "removal in progress"}, nil, "not-started", "restart"},
		{Facts{Name: "a", State: "exited", ExitCode: 2}, nil, "exited", "open-logs"},
	}
	for _, tc := range cases {
		c := ctx()
		if tc.c != nil {
			tc.c(&c)
		}
		ps := Diagnose([]Facts{tc.f}, c)
		p := only(t, ps, tc.rule)
		if p.Fixes[0].Kind != tc.fix {
			t.Errorf("%s: first fix %s, want %s", tc.rule, p.Fixes[0].Kind, tc.fix)
		}
		if len(p.Evidence) == 0 || p.Cause == "" || p.Title == "" {
			t.Errorf("%s: incomplete %+v", tc.rule, p)
		}
		if p.Evidence[len(p.Evidence)-1] != "Update the agent for a detailed diagnosis." {
			t.Errorf("%s: basic diagnosis not marked", tc.rule)
		}
	}
}

func TestPortTitleAndOneshot(t *testing.T) {
	c := ctx()
	c.PortOwners = map[int]string{8080: "stack jellyfin"}
	p := only(t, Diagnose([]Facts{{Name: "a", State: "created", Error: "Bind for 0.0.0.0:8080 failed: port is already allocated", Detailed: true}}, c), "port-in-use")
	if p.Title != "Port 8080 is already in use by stack jellyfin" || p.Fixes[0].Value != "8080" {
		t.Errorf("%+v", p)
	}
	c = ctx()
	c.Events = []Event{{Service: "job", Type: "restart", Time: now.Add(-time.Minute), Count: 4}}
	p = only(t, Diagnose([]Facts{{Name: "j", Service: "job", State: "restarting", ExitCode: 0, RestartPolicy: "always", Detailed: true}}, c), "oneshot-restarted")
	if p.Fixes[0].Kind != "set-restart-no" {
		t.Error(p)
	}
}

func TestStartedAfterChange(t *testing.T) {
	c := ctx()
	c.Changes = []Change{{Time: now.Add(-10 * time.Minute), Message: "update to postgres:17.2"}}
	p := only(t, Diagnose([]Facts{{Name: "db", State: "exited", ExitCode: 1, FinishedAt: now.Add(-5 * time.Minute), Detailed: true}}, c), "exited")
	if !strings.Contains(strings.Join(p.Evidence, "|"), "Started after update to postgres:17.2 at 14:00") || p.Fixes[len(p.Fixes)-1].Kind != "rollback" {
		t.Errorf("%+v", p)
	}
	// a change older than 15 minutes before the problem is not blamed
	c.Changes[0].Time = now.Add(-40 * time.Minute)
	p = only(t, Diagnose([]Facts{{Name: "db", State: "exited", ExitCode: 1, FinishedAt: now.Add(-5 * time.Minute), Detailed: true}}, c), "exited")
	if strings.Contains(strings.Join(p.Evidence, "|"), "Started after") {
		t.Errorf("%+v", p)
	}
}
