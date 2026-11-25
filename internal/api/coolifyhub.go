package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/netguard"
)

// coolifyRes is one Coolify resource (application, service or database) as Moorage shows it.
type coolifyRes struct {
	UUID        string `json:"uuid"`
	Name        string `json:"name"`
	Project     string `json:"project,omitempty"`
	Environment string `json:"environment,omitempty"`
	Type        string `json:"type,omitempty"` // application | service | database
	Status      string `json:"status,omitempty"`
	PullRequest string `json:"pullRequest,omitempty"`
	URL         string `json:"url,omitempty"` // the resource in Coolify's UI
	FQDN        string `json:"fqdn,omitempty"`
}

// coolifyHub keeps Coolify resources per host synced via Coolify REST API /api/v1
// and caches Layer-1 label metadata seen on containers so UIDs resolve globally.
type coolifyHub struct {
	srv         *Server
	mu          sync.RWMutex
	byHost      map[string]map[string]coolifyRes // hostID -> uuid -> res (API)
	labelByHost map[string]map[string]coolifyRes // hostID -> project/uuid -> res (container labels)
	lastAt      map[string]time.Time
	wake        chan struct{}
}

func newCoolifyHub(s *Server) *coolifyHub {
	return &coolifyHub{
		srv:         s,
		byHost:      map[string]map[string]coolifyRes{},
		labelByHost: map[string]map[string]coolifyRes{},
		lastAt:      map[string]time.Time{},
		wake:        make(chan struct{}, 1),
	}
}

// StartCoolify runs the 60s background Coolify API sync when the coolify-integration add-on is on.
func (s *Server) StartCoolify(ctx context.Context) {
	if s.coolify == nil {
		s.coolify = newCoolifyHub(s)
	}
	go s.coolify.run(ctx)
}

func (c *coolifyHub) TriggerSync() {
	if c == nil || c.wake == nil {
		return
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *coolifyHub) run(ctx context.Context) {
	c.syncAll(ctx)
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
			c.syncAll(ctx)
		case <-t.C:
			c.syncAll(ctx)
		}
	}
}

func (c *coolifyHub) syncAll(ctx context.Context) {
	if c == nil || c.srv == nil || c.srv.store == nil || c.srv.reg == nil {
		return
	}
	on, err := c.srv.store.AddonEnabled(ctx, addonCoolify)
	if err != nil || !on {
		return
	}
	hosts, err := c.srv.reg.List(ctx)
	if err != nil {
		return
	}
	for _, h := range hosts {
		baseURL, token, allowPriv := c.srv.EffectiveCoolifyForHost(ctx, h.ID)
		if baseURL == "" || token == "" {
			continue
		}
		_ = c.syncHost(ctx, h.ID, baseURL, token, allowPriv)
	}
}

func (c *coolifyHub) syncHost(ctx context.Context, hostID, baseURL, token string, allowPriv bool) error {
	client := &http.Client{
		Timeout: 12 * time.Second,
		Transport: &http.Transport{
			DialContext: netguard.DialContext(8*time.Second, allowPriv),
		},
	}
	defer client.CloseIdleConnections()

	resMap := map[string]coolifyRes{}
	for _, ep := range []struct {
		path string
		kind string
	}{
		{"/api/v1/applications", "application"},
		{"/api/v1/services", "service"},
		{"/api/v1/databases", "database"},
	} {
		raw, err := coolifyGetJSON(ctx, client, baseURL+ep.path, token)
		if err != nil {
			continue
		}
		var arr []map[string]any
		if json.Unmarshal(raw, &arr) != nil {
			continue
		}
		for _, item := range arr {
			uuid, _ := item["uuid"].(string)
			if uuid == "" {
				continue
			}
			name := firstNonEmptyStr(item, "name", "description")
			if name == "" {
				name = uuid
			}
			proj := nestedStr(item, "environment", "project", "name")
			if proj == "" {
				proj = firstNonEmptyStr(item, "project_name")
			}
			env := nestedStr(item, "environment", "name")
			if env == "" {
				env = firstNonEmptyStr(item, "environment_name")
			}
			status, _ := item["status"].(string)
			fqdn, _ := item["fqdn"].(string)
			pr := ""
			if v, ok := item["pull_request_id"].(float64); ok && v > 0 {
				pr = fmt.Sprintf("PR #%d", int(v))
			}
			uiURL := ""
			if projUUID := nestedStr(item, "environment", "project", "uuid"); projUUID != "" {
				if envUUID := nestedStr(item, "environment", "uuid"); envUUID != "" {
					uiURL = fmt.Sprintf("%s/project/%s/%s/%s/%s", baseURL, projUUID, envUUID, ep.kind, uuid)
				}
			}
			resMap[uuid] = coolifyRes{
				UUID:        uuid,
				Name:        name,
				Project:     proj,
				Environment: env,
				Type:        ep.kind,
				Status:      status,
				PullRequest: pr,
				URL:         uiURL,
				FQDN:        fqdn,
			}
		}
	}

	c.mu.Lock()
	c.byHost[hostID] = resMap
	c.lastAt[hostID] = time.Now()
	c.mu.Unlock()
	return nil
}

func (c *coolifyHub) resource(ctx context.Context, hostID, uuid string) *coolifyRes {
	if c == nil || uuid == "" {
		return nil
	}
	c.mu.RLock()
	m := c.byHost[hostID]
	last := c.lastAt[hostID]
	c.mu.RUnlock()
	if m == nil || time.Since(last) > 45*time.Second {
		if baseURL, token, allowPriv := c.srv.EffectiveCoolifyForHost(ctx, hostID); baseURL != "" && token != "" {
			_ = c.syncHost(ctx, hostID, baseURL, token, allowPriv)
			c.mu.RLock()
			m = c.byHost[hostID]
			c.mu.RUnlock()
		}
	}
	if r, ok := m[uuid]; ok {
		cp := r
		return &cp
	}
	return nil
}

func (c *coolifyHub) listHost(hostID string) []coolifyRes {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	m := c.byHost[hostID]
	out := make([]coolifyRes, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	return out
}

func (s *Server) coolifyResFor(ctx context.Context, hostID, uuid string) *coolifyRes {
	if s.coolify == nil {
		return nil
	}
	return s.coolify.resource(ctx, hostID, uuid)
}

func (s *Server) recordCoolifyLabels(hostID string, lblMeta map[string]coolifyRes) {
	if s.coolify == nil || len(lblMeta) == 0 {
		return
	}
	s.coolify.mu.Lock()
	if s.coolify.labelByHost == nil {
		s.coolify.labelByHost = map[string]map[string]coolifyRes{}
	}
	m := s.coolify.labelByHost[hostID]
	if m == nil {
		m = map[string]coolifyRes{}
		s.coolify.labelByHost[hostID] = m
	}
	for proj, r := range lblMeta {
		if r.Name != "" {
			m[proj] = r
			if r.UUID != "" && r.UUID != proj {
				m[r.UUID] = r
			}
		}
	}
	s.coolify.mu.Unlock()
}

// CoolifyMetaOf resolves a Coolify stack's human-readable metadata from API cache or label cache.
func (s *Server) CoolifyMetaOf(ctx context.Context, hostID, stack string) *coolifyRes {
	if s == nil || s.store == nil || stack == "" {
		return nil
	}
	on, _ := s.store.AddonEnabled(ctx, addonCoolify)
	if !on {
		return nil
	}
	if cr := s.coolifyResFor(ctx, hostID, stack); cr != nil {
		return cr
	}
	if s.coolify != nil {
		s.coolify.mu.RLock()
		r, ok := s.coolify.labelByHost[hostID][stack]
		s.coolify.mu.RUnlock()
		if ok && r.Name != "" {
			cp := r
			return &cp
		}
	}
	return nil
}

// StackDisplayName returns the Coolify human name for hostID/stack, or "" if none.
func (s *Server) StackDisplayName(ctx context.Context, hostID, stack string) string {
	if cr := s.CoolifyMetaOf(ctx, hostID, stack); cr != nil && cr.Name != "" && cr.Name != stack {
		return cr.Name
	}
	return ""
}

// coolifyNames returns a global hostId/stack -> display metadata map for the UI.
func (s *Server) coolifyNames(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	on, _ := s.store.AddonEnabled(ctx, addonCoolify)
	out := map[string]coolifyRes{}
	if !on || s.coolify == nil {
		writeJSON(w, http.StatusOK, map[string]any{"names": out})
		return
	}
	s.coolify.mu.RLock()
	for hid, m := range s.coolify.labelByHost {
		for k, v := range m {
			if v.Name != "" {
				out[hid+"/"+k] = v
			}
		}
	}
	for hid, m := range s.coolify.byHost {
		for k, v := range m {
			if v.Name != "" {
				out[hid+"/"+k] = v
			}
		}
	}
	s.coolify.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"names": out})
}

func (s *Server) coolifyOwns(ctx context.Context, hostID, project string) bool {
	if s.store == nil || project == "" {
		return false
	}
	on, _ := s.store.AddonEnabled(ctx, addonCoolify)
	if !on {
		return false
	}
	return s.coolifyResFor(ctx, hostID, project) != nil
}

func (s *Server) enrichCoolifySummary(
	ctx context.Context,
	hostID string,
	sm *engine.StackSummary,
	lblMeta map[string]coolifyRes,
	deploying map[string]bool,
) {
	if sm == nil {
		return
	}
	lm, hasLbl := lblMeta[sm.Name]
	uuid := lm.UUID
	if uuid == "" {
		uuid = sm.Name
	}
	cr := s.coolifyResFor(ctx, hostID, uuid)
	if !hasLbl && cr == nil {
		return
	}
	if cr != nil {
		sm.DisplayName = cr.Name
		sm.CoolifyUUID = cr.UUID
		sm.CoolifyType = cr.Type
		sm.CoolifyProject = cr.Project
		sm.CoolifyEnv = cr.Environment
		sm.CoolifyURL = cr.URL
		sm.CoolifyPR = cr.PullRequest
	} else {
		sm.DisplayName = lm.Name
		sm.CoolifyUUID = lm.UUID
		sm.CoolifyType = lm.Type
		sm.CoolifyProject = lm.Project
		sm.CoolifyEnv = lm.Environment
		sm.CoolifyPR = lm.PullRequest
	}
	if deploying[sm.Name] || deploying[sm.CoolifyUUID] {
		sm.CoolifyDeploying = true
		if s.mon != nil {
			s.mon.StackEvent(hostID, sm.Name, "deploy", "coolify", "Coolify rolling deploy detected")
		}
	}
}

func coolifyGetJSON(ctx context.Context, c *http.Client, fullURL, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func firstNonEmptyStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func nestedStr(m map[string]any, path ...string) string {
	cur := m
	for i, k := range path {
		if i == len(path)-1 {
			s, _ := cur[k].(string)
			return strings.TrimSpace(s)
		}
		next, ok := cur[k].(map[string]any)
		if !ok {
			return ""
		}
		cur = next
	}
	return ""
}

// isCoolifyHelperContainer detects ephemeral Coolify build/deploy helpers and /artifacts/* folders.
func isCoolifyHelperContainer(c engine.ContainerInfo) bool {
	if strings.HasPrefix(c.WorkingDir, "/artifacts/") || strings.Contains(c.Image, "coolify-helper") {
		return true
	}
	if strings.HasPrefix(c.Name, "coolify-helper") || strings.HasPrefix(c.Project, "coolify-helper") {
		return true
	}
	return false
}

// labelCoolifyMeta extracts Layer-1 Coolify metadata directly from container coolify.* labels.
func labelCoolifyMeta(ctrs []engine.ContainerInfo) (byProject map[string]coolifyRes, deploying map[string]bool) {
	byProject = map[string]coolifyRes{}
	deploying = map[string]bool{}
	seenNamesPerProj := map[string]int{}

	for _, c := range ctrs {
		if isCoolifyHelperContainer(c) {
			if targetUUID := c.Labels["coolify.applicationId"]; targetUUID != "" {
				deploying[targetUUID] = true
			}
			if strings.Contains(c.WorkingDir, "/artifacts/") && c.State == "running" {
				// Mark any active artifact build; project match happens if label present
				deploying["*"] = true
			}
			continue
		}
		proj := c.Project
		if proj == "" {
			continue
		}
		uuid := c.Labels["coolify.applicationId"]
		if uuid == "" {
			uuid = c.Labels["coolify.serviceId"]
		}
		if uuid == "" && c.Labels["coolify.managed"] != "true" {
			continue
		}
		if uuid == "" {
			uuid = proj
		}
		if c.State == "running" {
			seenNamesPerProj[proj]++
		}
		// Overlapping <uuid>-<oldTs> and <uuid>-<newTs> containers indicate a Coolify rolling swap.
		if strings.HasPrefix(c.Name, proj+"-") && c.State == "running" && seenNamesPerProj[proj] > 1 {
			deploying[proj] = true
		}
		if _, exists := byProject[proj]; !exists {
			typ := c.Labels["coolify.type"]
			if typ == "" {
				if c.Labels["coolify.serviceId"] != "" {
					typ = "service"
				} else {
					typ = "application"
				}
			}
			name := c.Labels["coolify.resourceName"]
			if name == "" {
				name = c.Labels["coolify.name"]
			}
			pr := ""
			if pid := c.Labels["coolify.pullRequestId"]; pid != "" && pid != "0" {
				pr = "PR #" + pid
			}
			byProject[proj] = coolifyRes{
				UUID:        uuid,
				Name:        name,
				Project:     c.Labels["coolify.projectName"],
				Environment: c.Labels["coolify.environmentName"],
				Type:        typ,
				PullRequest: pr,
			}
		}
	}
	_ = host.LocalID
	return byProject, deploying
}
