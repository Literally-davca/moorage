package monitor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"moorage/internal/store"
)

func TestCombineTargets(t *testing.T) {
	up := TargetResult{Label: "a", Status: StatusUp, LatencyMS: 40}
	up2 := TargetResult{Label: "b", Status: StatusUp, LatencyMS: 90}
	down := TargetResult{Label: "api.example", Status: StatusDown, Msg: "HTTP 502"}
	maint := TargetResult{Label: "m", Status: StatusMaintenance}
	silenced := TargetResult{Label: "s", Status: StatusSilenced}

	o := combineTargets([]TargetResult{up, up2})
	if !o.OK || o.Degraded || o.Latency != 90*time.Millisecond {
		t.Fatalf("all up: %+v", o)
	}
	o = combineTargets([]TargetResult{up, up2, down, up})
	if o.OK || !o.Degraded || !strings.HasPrefix(o.Msg, "1/4 down: api.example (HTTP 502)") {
		t.Fatalf("partial: %+v", o)
	}
	o = combineTargets([]TargetResult{down, down})
	if o.OK || o.Degraded {
		t.Fatalf("all down: %+v", o)
	}
	// a target in maintenance or a deploy doesn't count
	o = combineTargets([]TargetResult{up, maint, silenced})
	if !o.OK || !strings.Contains(o.Msg, "2 skipped") {
		t.Fatalf("quiet targets: %+v", o)
	}
	o = combineTargets([]TargetResult{down, maint})
	if o.OK || o.Degraded {
		t.Fatalf("only active target down: %+v", o)
	}
	o = combineTargets([]TargetResult{maint, silenced})
	if o.Quiet != StatusMaintenance {
		t.Fatalf("all quiet: %+v", o)
	}
	o = combineTargets([]TargetResult{silenced})
	if o.Quiet != StatusSilenced {
		t.Fatalf("all silenced: %+v", o)
	}
}

func TestMultiTargetStackSilenceAndStates(t *testing.T) {
	m, st, sent := newTestMonitor(t)
	ctx := context.Background()
	chID, _ := st.SaveChannel(ctx, store.Channel{Name: "ntfy", Kind: "ntfy", Config: `{"url":"https://ntfy.sh/x"}`, Enabled: true})
	set, _ := json.Marshal(Settings{Stacks: []StackTarget{{"local", "db"}, {"pi", "web"}}})
	id, err := st.SaveCheck(ctx, store.Check{Name: "group", Kind: KindStack, Target: "db", HostID: "local", Stack: "db",
		Interval: 30, Timeout: 5, Retries: 1, Enabled: true, Channels: []int64{chID}, Settings: string(set)})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := st.CheckByID(ctx, id)
	if !MultiTarget(c) || len(subChecks(c)) != 2 {
		t.Fatal("two stacks make a multi-target check")
	}
	if sc := subChecks(c)[1]; sc.HostID != "pi" || sc.Stack != "web" || strings.Contains(sc.Settings, "stacks") {
		t.Fatalf("sub-check: %+v", sc)
	}

	// deploying one stack of the group: that stack sits the run out (no registry: the
	// other one is "host not found", so the check is down, not partially down)
	m.StackEvent("pi", "web", "deploy", "ann", "")
	o := m.probeTargets(ctx, c, time.Now())
	if o.Targets[1].Status != StatusSilenced || o.Targets[0].Status != StatusDown {
		t.Fatalf("targets: %+v", o.Targets)
	}

	now := time.Now()
	partial := Outcome{Degraded: true, Msg: "1/2 down: db (exited)", Targets: []TargetResult{{Label: "db", Status: StatusDown}, {Label: "web", Status: StatusUp}}}
	m.handle(c, partial, now)
	if s := m.State(id); s.Status != StatusDegraded || len(s.Targets) != 2 {
		t.Fatalf("want degraded, got %+v", s)
	}
	m.flush(groupKey(c))
	if len(*sent) != 1 || !strings.HasPrefix((*sent)[0].Title, "PARTIALLY DOWN: group") {
		t.Fatalf("partial alert: %+v", *sent)
	}
	// worse: down escalates and updates the incident
	m.handle(c, Outcome{Msg: "2/2 down: db, web"}, now.Add(30*time.Second))
	if s := m.State(id); s.Status != StatusDown {
		t.Fatalf("want down, got %s", s.Status)
	}
	m.flush(groupKey(c))
	if len(*sent) != 2 || !strings.HasPrefix((*sent)[1].Title, "DOWN (was partially down): group") {
		t.Fatalf("escalation: %+v", *sent)
	}
	open, _ := st.OpenIncidents(ctx)
	if in, ok := open[id]; !ok || in.Cause != "2/2 down: db, web" {
		t.Fatalf("incident: %+v", open)
	}
	// all up closes it
	m.handle(c, Outcome{OK: true, Msg: "2/2 up"}, now.Add(60*time.Second))
	m.flush(groupKey(c))
	if len(*sent) != 3 || (*sent)[2].Status != "up" {
		t.Fatalf("recovery: %+v", *sent)
	}
	if open, _ := st.OpenIncidents(ctx); len(open) != 0 {
		t.Fatalf("incident stays open: %v", open)
	}
}
