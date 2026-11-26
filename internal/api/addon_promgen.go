package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/netguard"
	"moorage/internal/store"
)

type promDashboardReq struct {
	Scope         string
	HostID        string
	Stacks        []string
	CheckIDs      []int64
	DatasourceUID string
}

func parsePromDashboardReq(r *http.Request) promDashboardReq {
	q := r.URL.Query()
	scope := strings.TrimSpace(q.Get("scope"))
	if scope != "host" && scope != "custom" {
		scope = "all"
	}
	req := promDashboardReq{
		Scope:         scope,
		HostID:        strings.TrimSpace(q.Get("host")),
		DatasourceUID: strings.TrimSpace(q.Get("datasourceUid")),
	}
	if req.DatasourceUID == "" {
		req.DatasourceUID = "${DS_PROMETHEUS}"
	}
	if s := q.Get("stacks"); s != "" {
		for _, p := range strings.Split(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				req.Stacks = append(req.Stacks, p)
			}
		}
	}
	if c := q.Get("checks"); c != "" {
		for _, p := range strings.Split(c, ",") {
			p = strings.TrimSpace(p)
			var id int64
			if _, err := fmt.Sscanf(p, "%d", &id); err == nil && id > 0 {
				req.CheckIDs = append(req.CheckIDs, id)
			}
		}
	}
	return req
}

func (s *Server) buildGrafanaDashboard(ctx context.Context, req promDashboardReq) map[string]any {
	pubURL := strings.TrimRight(s.effectivePublicURL(ctx), "/")
	ds := map[string]any{"type": "prometheus", "uid": req.DatasourceUID}

	hostFilter := ""
	if req.Scope == "host" && req.HostID != "" {
		hostFilter = fmt.Sprintf(`host_id="%s"`, escapePromLabel(req.HostID))
	} else if req.Scope == "custom" && len(req.Stacks) > 0 {
		hostFilter = fmt.Sprintf(`stack=~"%s"`, escapePromLabel(strings.Join(req.Stacks, "|")))
	}
	wrapFilter := func(base, extra string) string {
		var parts []string
		if base != "" {
			parts = append(parts, base)
		}
		if extra != "" {
			parts = append(parts, extra)
		}
		if len(parts) == 0 {
			return ""
		}
		return "{" + strings.Join(parts, ",") + "}"
	}

	stackDataLinks := []map[string]any{}
	checkDataLinks := []map[string]any{}
	if pubURL != "" {
		stackDataLinks = append(stackDataLinks, map[string]any{
			"title": "Open stack in Moorage",
			"url":   pubURL + "/#/h/${__field.labels.host_id}/stacks/${__field.labels.stack}",
		})
		checkDataLinks = append(checkDataLinks, map[string]any{
			"title": "Open check in Moorage",
			"url":   pubURL + "/#/checks/${__field.labels.check_id}",
		})
	}

	title := "Moorage Overview"
	uid := "moorage-overview"
	if req.Scope == "host" && req.HostID != "" {
		title = "Moorage · " + req.HostID
		uid = "moorage-host-" + safeSlug(req.HostID)
	} else if req.Scope == "custom" {
		title = "Moorage · Selected Services"
		uid = "moorage-custom"
	}

	panels := []map[string]any{
		{
			"id":         1,
			"type":       "stat",
			"title":      "Hosts online",
			"gridPos":    map[string]int{"x": 0, "y": 0, "w": 6, "h": 4},
			"datasource": ds,
			"targets": []map[string]any{{
				"expr":         "sum(moorage_host_up" + wrapFilter("", hostLabelOnly(req)) + ")",
				"legendFormat": "Online",
				"refId":        "A",
			}},
			"fieldConfig": map[string]any{
				"defaults": map[string]any{
					"color": map[string]any{"mode": "thresholds"},
					"thresholds": map[string]any{
						"mode": "absolute",
						"steps": []map[string]any{
							{"color": "red", "value": nil},
							{"color": "green", "value": 1},
						},
					},
				},
			},
		},
		{
			"id":         2,
			"type":       "stat",
			"title":      "Stacks up",
			"gridPos":    map[string]int{"x": 6, "y": 0, "w": 6, "h": 4},
			"datasource": ds,
			"targets": []map[string]any{{
				"expr":         "count(moorage_stack_state" + wrapFilter(hostFilter, `state="up"`) + ") or vector(0)",
				"legendFormat": "Up",
				"refId":        "A",
			}},
			"fieldConfig": map[string]any{
				"defaults": map[string]any{
					"color": map[string]any{"mode": "fixed", "fixedColor": "green"},
				},
			},
		},
		{
			"id":         3,
			"type":       "stat",
			"title":      "Checks degraded (Partially down)",
			"gridPos":    map[string]int{"x": 12, "y": 0, "w": 6, "h": 4},
			"datasource": ds,
			"targets": []map[string]any{{
				"expr":         `count(moorage_check_state{state="degraded"} == 1) or vector(0)`,
				"legendFormat": "Partially down",
				"refId":        "A",
			}},
			"fieldConfig": map[string]any{
				"defaults": map[string]any{
					"color": map[string]any{"mode": "thresholds"},
					"thresholds": map[string]any{
						"mode": "absolute",
						"steps": []map[string]any{
							{"color": "green", "value": nil},
							{"color": "yellow", "value": 1},
						},
					},
				},
			},
		},
		{
			"id":         4,
			"type":       "stat",
			"title":      "Checks down",
			"gridPos":    map[string]int{"x": 18, "y": 0, "w": 6, "h": 4},
			"datasource": ds,
			"targets": []map[string]any{{
				"expr":         `count(moorage_check_state{state="down"} == 1) or vector(0)`,
				"legendFormat": "Down",
				"refId":        "A",
			}},
			"fieldConfig": map[string]any{
				"defaults": map[string]any{
					"color": map[string]any{"mode": "thresholds"},
					"thresholds": map[string]any{
						"mode": "absolute",
						"steps": []map[string]any{
							{"color": "green", "value": nil},
							{"color": "red", "value": 1},
						},
					},
				},
			},
		},
		{
			"id":         5,
			"type":       "table",
			"title":      "Stacks status",
			"gridPos":    map[string]int{"x": 0, "y": 4, "w": 12, "h": 9},
			"datasource": ds,
			"targets": []map[string]any{{
				"expr":    "moorage_stack_state" + wrapFilter(hostFilter, "") + " == 1",
				"format":  "table",
				"instant": true,
				"refId":   "A",
			}},
			"fieldConfig": map[string]any{
				"defaults": map[string]any{
					"links": stackDataLinks,
				},
			},
		},
		{
			"id":         6,
			"type":       "timeseries",
			"title":      "Check latency (ms)",
			"gridPos":    map[string]int{"x": 12, "y": 4, "w": 12, "h": 9},
			"datasource": ds,
			"targets": []map[string]any{{
				"expr":         "moorage_check_latency_ms" + wrapFilter(hostLabelOnly(req), ""),
				"legendFormat": "{{name}}",
				"refId":        "A",
			}},
			"fieldConfig": map[string]any{
				"defaults": map[string]any{
					"unit":  "ms",
					"links": checkDataLinks,
				},
			},
		},
	}

	return map[string]any{
		"uid":           uid,
		"title":         title,
		"tags":          []string{"moorage", "docker"},
		"timezone":      "browser",
		"schemaVersion": 39,
		"version":       1,
		"refresh":       "30s",
		"panels":        panels,
	}
}

func hostLabelOnly(req promDashboardReq) string {
	if req.Scope == "host" && req.HostID != "" {
		return fmt.Sprintf(`host_id="%s"`, escapePromLabel(req.HostID))
	}
	return ""
}

func escapePromLabel(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return v
}

func safeSlug(v string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(v) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// GET /api/addons/prometheus-grafana/dashboard
func (s *Server) promDashboardJSON(w http.ResponseWriter, r *http.Request) {
	req := parsePromDashboardReq(r)
	dash := s.buildGrafanaDashboard(r.Context(), req)
	writeJSON(w, http.StatusOK, dash)
}

// POST /api/addons/prometheus-grafana/push
func (s *Server) pushGrafanaDashboard(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Scope         string   `json:"scope"`
		HostID        string   `json:"hostId"`
		Stacks        []string `json:"stacks"`
		CheckIDs      []int64  `json:"checkIds"`
		DatasourceUID string   `json:"datasourceUid"`
	}
	_ = readJSON(w, r, &body)

	ctx := r.Context()
	panelsCfg, _ := s.effectiveSisterPanels(ctx)
	if panelsCfg.GrafanaURL == "" || panelsCfg.GrafanaToken == "" {
		writeErr(w, http.StatusBadRequest, "Configure Grafana URL and API token in Settings -> Connections first.")
		return
	}

	dsUID := strings.TrimSpace(body.DatasourceUID)
	if dsUID == "" {
		dsUID = "${DS_PROMETHEUS}"
	}
	dash := s.buildGrafanaDashboard(ctx, promDashboardReq{
		Scope:         body.Scope,
		HostID:        body.HostID,
		Stacks:        body.Stacks,
		CheckIDs:      body.CheckIDs,
		DatasourceUID: dsUID,
	})
	payload := map[string]any{
		"dashboard": dash,
		"overwrite": true,
	}
	if panelsCfg.GrafanaFolderUID != "" {
		payload["folderUid"] = panelsCfg.GrafanaFolderUID
	}
	raw, _ := json.Marshal(payload)

	c := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: netguard.DialContext(10*time.Second, panelsCfg.GrafanaAllowPrivate),
		},
	}
	defer c.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(panelsCfg.GrafanaURL, "/")+"/api/dashboards/db", bytes.NewReader(raw))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+panelsCfg.GrafanaToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Grafana request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("Grafana returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))))
		return
	}
	var out struct {
		UID    string `json:"uid"`
		URL    string `json:"url"`
		Status string `json:"status"`
	}
	_ = json.Unmarshal(respBody, &out)
	fullURL := out.URL
	if fullURL != "" && strings.HasPrefix(fullURL, "/") {
		fullURL = strings.TrimRight(panelsCfg.GrafanaURL, "/") + fullURL
	}
	setAuditTarget(r, out.UID)
	writeJSON(w, http.StatusOK, map[string]any{
		"uid":    out.UID,
		"url":    fullURL,
		"status": out.Status,
	})
}

// GET /api/addons/prometheus-grafana/scrape
func (s *Server) promScrapeConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pub := strings.TrimRight(s.effectivePublicURL(ctx), "/")
	scheme := "http"
	target := "moorage:5080"
	if pub != "" {
		if u, err := url.Parse(pub); err == nil && u.Host != "" {
			scheme = u.Scheme
			target = u.Host
		}
	}

	vmScrape := fmt.Sprintf(`scrape_configs:
  - job_name: moorage
    scrape_interval: 30s
    scheme: %s
    metrics_path: /metrics
    bearer_token: "%%{MOORAGE_METRICS_TOKEN}"
    static_configs:
      - targets: ["%s"]`, scheme, target)

	vmAlerts := `groups:
  - name: moorage
    rules:
      - alert: MoorageHostOffline
        expr: moorage_host_up == 0
        for: 2m
        labels:
          severity: critical
        annotations:
          summary: "Moorage host {{ $labels.host_name }} is offline"
      - alert: MoorageCheckDown
        expr: moorage_check_state{state="down"} == 1
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "Check {{ $labels.name }} is down"
      - alert: MoorageCheckPartiallyDown
        expr: moorage_check_state{state="degraded"} == 1
        for: 2m
        labels:
          severity: warning
        annotations:
          summary: "Check {{ $labels.name }} is partially down"
      - alert: MoorageAutoUpdateFailed
        expr: increase(moorage_auto_update_failures_total[15m]) > 0
        labels:
          severity: warning
        annotations:
          summary: "Auto-update failed on {{ $labels.stack }}"`

	promScrape := fmt.Sprintf(`scrape_configs:
  - job_name: moorage
    scrape_interval: 30s
    scheme: %s
    metrics_path: /metrics
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/moorage_token
    static_configs:
      - targets: ["%s"]`, scheme, target)

	promRules := fmt.Sprintf(`apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: moorage-alerts
spec:
%s`, indentBlock(vmAlerts, 2))

	writeJSON(w, http.StatusOK, map[string]any{
		"victoriametrics": map[string]string{
			"scrape": vmScrape,
			"alerts": vmAlerts,
		},
		"prometheus": map[string]string{
			"scrape": promScrape,
			"alerts": promRules,
		},
	})
}

func indentBlock(s string, spaces int) string {
	pad := strings.Repeat(" ", spaces)
	lines := strings.Split(s, "\n")
	for i := range lines {
		if lines[i] != "" {
			lines[i] = pad + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

// GET /api/addons/prometheus-grafana/homelab
func (s *Server) promHomelabSnippets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pub := strings.TrimRight(s.effectivePublicURL(ctx), "/")

	type svcItem struct {
		Group string `json:"group"`
		Name  string `json:"name"`
		URL   string `json:"url"`
		Desc  string `json:"description"`
	}
	var items []svcItem

	if s.reg != nil {
		hosts, _ := s.reg.List(ctx)
		proxyOn, _ := s.store.AddonEnabled(ctx, addonProxyLabels)
		for _, hMeta := range hosts {
			if !hMeta.Connected {
				continue
			}
			h, _, err := s.reg.Get(ctx, hMeta.ID)
			if err != nil {
				continue
			}
			stacks, err := h.ListStacks(ctx)
			if err != nil {
				continue
			}
			hostIP := s.hostEffectiveIP(ctx, hMeta.ID)
			for _, st := range stacks {
				urls := append([]string(nil), st.URLs...)
				if proxyOn && len(st.ProxyURLs) > 0 {
					urls = mergeURLs(urls, st.ProxyURLs, 5)
				}
				if len(urls) == 0 && hostIP != "" {
					if cm, ok := h.(host.ContainerManager); ok {
						if ctrs, cerr := cm.Containers(ctx); cerr == nil {
							var ports []string
							for _, c := range ctrs {
								if c.Project == st.Name && c.State == "running" {
									ports = append(ports, c.Ports...)
								}
							}
							urls = mergeURLs(urls, hostPortURLs(hostIP, ports), 5)
						}
					}
				}
				if len(urls) == 0 {
					continue
				}
				display := st.Name
				grp := hMeta.Name
				if cr := s.coolifyResFor(ctx, hMeta.ID, st.Name); cr != nil {
					if cr.Name != "" {
						display = cr.Name
					}
					if cr.Project != "" {
						grp = hMeta.Name + " · " + cr.Project
					}
				}
				items = append(items, svcItem{
					Group: grp,
					Name:  display,
					URL:   urls[0],
					Desc:  fmt.Sprintf("%s (%d/%d running)", st.Status, st.Running, st.Total),
				})
			}
		}
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Group != items[j].Group {
			return items[i].Group < items[j].Group
		}
		return items[i].Name < items[j].Name
	})

	// Homepage services.yaml
	var hp strings.Builder
	curGroup := ""
	for _, it := range items {
		if it.Group != curGroup {
			curGroup = it.Group
			hp.WriteString(fmt.Sprintf("- %s:\n", curGroup))
		}
		hp.WriteString(fmt.Sprintf("    - %s:\n        href: %s\n        description: %s\n        siteMonitor: %s\n",
			it.Name, it.URL, it.Desc, it.URL))
	}
	if hp.Len() == 0 {
		hp.WriteString("# No service URLs detected yet. Add moorage.url labels or configure Host IP.\n")
	}

	// Glance glance.yml
	var gl strings.Builder
	gl.WriteString("- type: monitor\n  cache: 1m\n  title: Moorage Services\n  sites:\n")
	for _, it := range items {
		gl.WriteString(fmt.Sprintf("    - title: %s\n      url: %s\n", it.Name, it.URL))
	}
	if pub != "" {
		gl.WriteString(fmt.Sprintf("\n# Moorage status page widget:\n# url: %s/api/nav-summary\n", pub))
	}

	// Homarr JSON export
	homarrBytes, _ := json.MarshalIndent(map[string]any{
		"schema":   "moorage-homarr-v1",
		"moorage":  pub,
		"services": items,
	}, "", "  ")

	_ = store.ErrNotFound
	_ = engine.StatusUp
	writeJSON(w, http.StatusOK, map[string]string{
		"homepage": hp.String(),
		"glance":   gl.String(),
		"homarr":   string(homarrBytes),
	})
}
