package metrics

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

// TestGrafanaDashboard checks that the shipped dashboard is valid JSON, takes its data
// source from a variable and only queries metrics this package writes.
func TestGrafanaDashboard(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/grafana/moorage-dashboard.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Panels []struct {
			Title      string `json:"title"`
			Type       string `json:"type"`
			Datasource *struct {
				UID string `json:"uid"`
			} `json:"datasource"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
		Templating struct {
			List []struct {
				Name, Type string
			} `json:"list"`
		} `json:"templating"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("dashboard is not valid JSON: %v", err)
	}
	hasDS := false
	for _, v := range d.Templating.List {
		if v.Name == "datasource" && v.Type == "datasource" {
			hasDS = true
		}
	}
	if !hasDS {
		t.Error("no datasource variable")
	}
	known := map[string]bool{}
	for _, n := range []string{
		"moorage_build_info", "moorage_snapshot_age_seconds", "moorage_host_up", "moorage_host_read_only",
		"moorage_agent_info", "moorage_check_up", "moorage_check_state", "moorage_check_latency_seconds",
		"moorage_check_uptime_ratio", "moorage_stack_containers", "moorage_container_up",
		"moorage_image_update_available", "moorage_alerts_sent_total", "moorage_alerts_failed_total",
	} {
		known[n] = true
	}
	nameRe := regexp.MustCompile(`moorage_[a-z_]+`)
	for _, p := range d.Panels {
		if p.Type == "row" {
			continue
		}
		if p.Datasource == nil || p.Datasource.UID != "${datasource}" {
			t.Errorf("panel %q doesn't use the datasource variable", p.Title)
		}
		if len(p.Targets) == 0 {
			t.Errorf("panel %q has no query", p.Title)
		}
		for _, tg := range p.Targets {
			for _, n := range nameRe.FindAllString(tg.Expr, -1) {
				if !known[n] {
					t.Errorf("panel %q queries unknown metric %s", p.Title, n)
				}
			}
		}
	}
}
