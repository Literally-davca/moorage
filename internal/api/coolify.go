package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"moorage/internal/host"
	"moorage/internal/netguard"
	"moorage/internal/store"
)

func (s *Server) coolifyClientForHost(ctx context.Context, hostID string) (baseURL, token string, client *http.Client, ok bool) {
	baseURL, token, allowPriv := s.EffectiveCoolifyForHost(ctx, hostID)
	if baseURL == "" || token == "" {
		return "", "", nil, false
	}
	c := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			DialContext: netguard.DialContext(10*time.Second, allowPriv),
		},
	}
	return baseURL, token, c, true
}

func coolifyKindPlural(t string) string {
	switch t {
	case "service":
		return "services"
	case "database":
		return "databases"
	default:
		return "applications"
	}
}

// POST /api/hosts/{host}/coolify/{uuid}/actions/{action}
func (s *Server) coolifyAction(w http.ResponseWriter, r *http.Request) {
	_, ok := s.hostOf(w, r, true)
	if !ok {
		return
	}
	ctx := r.Context()
	on, _ := s.store.AddonEnabled(ctx, addonCoolify)
	if !on {
		writeErr(w, http.StatusNotFound, "Enable the Coolify integration add-on first.")
		return
	}
	hostID := r.PathValue("host")
	uuid := strings.TrimSpace(r.PathValue("uuid"))
	action := strings.TrimSpace(r.PathValue("action"))

	var body struct {
		Force bool   `json:"force"`
		Type  string `json:"type"`
	}
	_ = readJSON(w, r, &body)

	baseURL, token, client, configured := s.coolifyClientForHost(ctx, hostID)
	if !configured {
		writeErr(w, http.StatusBadRequest, "Configure Coolify URL and token in Settings -> Connections.")
		return
	}
	defer client.CloseIdleConnections()

	resType := body.Type
	if res := s.coolifyResFor(ctx, hostID, uuid); res != nil && res.Type != "" {
		resType = res.Type
	}
	plural := coolifyKindPlural(resType)

	var targetURL string
	method := http.MethodPost
	switch action {
	case "start", "stop", "restart":
		targetURL = fmt.Sprintf("%s/api/v1/%s/%s/%s", baseURL, plural, url.PathEscape(uuid), action)
	case "redeploy":
		targetURL = fmt.Sprintf("%s/api/v1/deploy?uuid=%s", baseURL, url.QueryEscape(uuid))
		if body.Force {
			targetURL += "&force=true"
		}
	default:
		writeErr(w, http.StatusBadRequest, "Unknown Coolify action.")
		return
	}

	user := currentUser(r).Username
	if s.mon != nil && (action == "restart" || action == "stop" || action == "redeploy") {
		// Silence checks for 3 minutes during Coolify restart/redeploy.
		s.mon.StackEvent(hostID, uuid, "deploy", user, "Coolify "+action)
	}

	req, err := http.NewRequestWithContext(ctx, method, targetURL, nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Coolify API error: "+err.Error())
		return
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("Coolify returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes))))
		return
	}
	setAuditStack(r, uuid)
	if s.coolify != nil {
		s.coolify.TriggerSync()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"action": action,
		"uuid":   uuid,
	})
}

// GET /api/hosts/{host}/coolify/{uuid}/deployments
func (s *Server) coolifyDeployments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	on, _ := s.store.AddonEnabled(ctx, addonCoolify)
	if !on {
		writeJSON(w, http.StatusOK, map[string]any{"deployments": []any{}})
		return
	}
	hostID := r.PathValue("host")
	uuid := strings.TrimSpace(r.PathValue("uuid"))
	baseURL, token, client, configured := s.coolifyClientForHost(ctx, hostID)
	if !configured {
		writeJSON(w, http.StatusOK, map[string]any{"deployments": []any{}})
		return
	}
	defer client.CloseIdleConnections()

	raw, err := coolifyGetJSON(ctx, client, fmt.Sprintf("%s/api/v1/applications/%s/deployments", baseURL, url.PathEscape(uuid)), token)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"deployments": []any{}, "error": err.Error()})
		return
	}
	var list []map[string]any
	if json.Unmarshal(raw, &list) != nil {
		var wrapped struct {
			Deployments []map[string]any `json:"deployments"`
		}
		if json.Unmarshal(raw, &wrapped) == nil {
			list = wrapped.Deployments
		}
	}
	type deploymentDTO struct {
		UUID      string `json:"uuid"`
		Status    string `json:"status"`
		Commit    string `json:"commit,omitempty"`
		Logs      string `json:"logs,omitempty"`
		CreatedAt string `json:"createdAt,omitempty"`
	}
	out := make([]deploymentDTO, 0, len(list))
	for _, d := range list {
		duuid, _ := d["deployment_uuid"].(string)
		if duuid == "" {
			duuid, _ = d["uuid"].(string)
		}
		st, _ := d["status"].(string)
		commit, _ := d["commit"].(string)
		logs, _ := d["logs"].(string)
		created, _ := d["created_at"].(string)
		out = append(out, deploymentDTO{
			UUID:      duuid,
			Status:    st,
			Commit:    commit,
			Logs:      logs,
			CreatedAt: created,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployments": out})
}

// GET /api/hosts/{host}/coolify/{uuid}/envs
func (s *Server) coolifyGetEnvs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hostID := r.PathValue("host")
	uuid := strings.TrimSpace(r.PathValue("uuid"))
	baseURL, token, client, configured := s.coolifyClientForHost(ctx, hostID)
	if !configured {
		writeErr(w, http.StatusBadRequest, "Configure Coolify URL and token in Settings -> Connections.")
		return
	}
	defer client.CloseIdleConnections()

	plural := "applications"
	if res := s.coolifyResFor(ctx, hostID, uuid); res != nil && res.Type != "" {
		plural = coolifyKindPlural(res.Type)
	}
	raw, err := coolifyGetJSON(ctx, client, fmt.Sprintf("%s/api/v1/%s/%s/envs", baseURL, plural, url.PathEscape(uuid)), token)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Coolify API error: "+err.Error())
		return
	}
	var list []map[string]any
	_ = json.Unmarshal(raw, &list)
	var lines []string
	for _, e := range list {
		k, _ := e["key"].(string)
		if k == "" {
			continue
		}
		v, _ := e["value"].(string)
		lines = append(lines, formatEnvAssignment(k, v))
	}
	envText := strings.Join(lines, "\n")
	if envText != "" {
		envText += "\n"
	}
	writeJSON(w, http.StatusOK, map[string]any{"env": envText})
}

// PUT /api/hosts/{host}/coolify/{uuid}/envs
func (s *Server) coolifyPutEnvs(w http.ResponseWriter, r *http.Request) {
	_, ok := s.hostOf(w, r, true)
	if !ok {
		return
	}
	ctx := r.Context()
	hostID := r.PathValue("host")
	uuid := strings.TrimSpace(r.PathValue("uuid"))
	var req struct {
		Env      string `json:"env"`
		Redeploy bool   `json:"redeploy"`
	}
	if !readJSON(w, r, &req) {
		return
	}

	baseURL, token, client, configured := s.coolifyClientForHost(ctx, hostID)
	if !configured {
		writeErr(w, http.StatusBadRequest, "Configure Coolify URL and token in Settings -> Connections.")
		return
	}
	defer client.CloseIdleConnections()

	plural := "applications"
	if res := s.coolifyResFor(ctx, hostID, uuid); res != nil && res.Type != "" {
		plural = coolifyKindPlural(res.Type)
	}
	expanded := s.expandSharedSecrets(ctx, hostID, req.Env)
	kv := parseEnvLinesMap(expanded)
	var data []map[string]any
	for k, v := range kv {
		data = append(data, map[string]any{
			"key":        k,
			"value":      v,
			"is_preview": false,
		})
	}
	payload, _ := json.Marshal(map[string]any{"data": data})
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPatch, fmt.Sprintf("%s/api/v1/%s/%s/envs/bulk", baseURL, plural, url.PathEscape(uuid)), bytes.NewReader(payload))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	hreq.Header.Set("Authorization", "Bearer "+token)
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(hreq)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Coolify API error: "+err.Error())
		return
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("Coolify returned HTTP %d saving variables.", resp.StatusCode))
		return
	}

	setAuditStack(r, uuid)
	if req.Redeploy {
		user := currentUser(r).Username
		if s.mon != nil {
			s.mon.StackEvent(hostID, uuid, "deploy", user, "Coolify env save & redeploy")
		}
		dreq, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/v1/deploy?uuid=%s", baseURL, url.QueryEscape(uuid)), nil)
		dreq.Header.Set("Authorization", "Bearer "+token)
		if dresp, derr := client.Do(dreq); derr == nil {
			dresp.Body.Close()
		}
	}
	_ = host.LocalID
	_ = store.ErrNotFound
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "redeployed": req.Redeploy})
}
