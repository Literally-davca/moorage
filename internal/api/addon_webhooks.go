package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"moorage/internal/store"
)

type webhookDebounce struct {
	mu     sync.Mutex
	timers map[string]*time.Timer
}

var whDebouncer = &webhookDebounce{timers: map[string]*time.Timer{}}

// whIPRate limits the public webhook endpoint per client address.
var whIPRate = &mcpLimiter{hits: map[string][]time.Time{}}

func (d *webhookDebounce) schedule(key string, delay time.Duration, fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if old, ok := d.timers[key]; ok {
		old.Stop()
	}
	d.timers[key] = time.AfterFunc(delay, func() {
		d.mu.Lock()
		delete(d.timers, key)
		d.mu.Unlock()
		fn()
	})
}

func generateRandomToken(prefix string) (plain, hash string) {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	plain = prefix + hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(plain))
	hash = hex.EncodeToString(sum[:])
	return plain, hash
}

func hashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// verifyHubSignature256 validates X-Hub-Signature-256: sha256=<hex>.
func verifyHubSignature256(secret string, body []byte, sigHeader string) bool {
	if secret == "" {
		return true
	}
	sigHex, ok := strings.CutPrefix(strings.TrimSpace(sigHeader), "sha256=")
	if !ok || sigHex == "" {
		return false
	}
	got, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := mac.Sum(nil)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// parseRegistryWebhookRepos extracts pushed image repository names from GHCR, Docker Hub,
// and Gitea/Harbor webhook JSON payloads.
func parseRegistryWebhookRepos(provider string, body []byte) []string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return nil
	}
	var repos []string
	add := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" {
			repos = append(repos, s)
		}
	}

	// Docker Hub: { "repository": { "repo_name": "user/app" }, "push_data": { "tag": "latest" } }
	if repo := asMap(m["repository"]); repo != nil {
		add(firstNonEmptyStr(repo, "repo_name", "full_name", "name"))
	}
	// GitHub GHCR: { "package": { "name": "app", "package_type": "container", "package_version": { "package_url": "pkg:docker/ghcr.io/..." } } }
	if pkg := asMap(m["package"]); pkg != nil {
		add(firstNonEmptyStr(pkg, "name", "namespace"))
		if pv := asMap(pkg["package_version"]); pv != nil {
			if pu, ok := pv["package_url"].(string); ok {
				pu = strings.TrimPrefix(pu, "pkg:docker/")
				if idx := strings.IndexByte(pu, '@'); idx > 0 {
					pu = pu[:idx]
				}
				add(pu)
			}
		}
	}
	// Harbor: { "event_data": { "resources": [ { "resource_url": "harbor.example.com/proj/app:tag" } ] } }
	if ed := asMap(m["event_data"]); ed != nil {
		if resList, ok := ed["resources"].([]any); ok {
			for _, item := range resList {
				if rm := asMap(item); rm != nil {
					if ru, ok := rm["resource_url"].(string); ok {
						if idx := strings.LastIndexByte(ru, ':'); idx > 0 {
							ru = ru[:idx]
						}
						add(ru)
					}
				}
			}
		}
	}
	_ = provider
	return repos
}

// ---- Admin CRUD ------------------------------------------------------------------------

func (s *Server) listRegistryWebhooks(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.RegistryWebhooks(r.Context())
	if err != nil {
		s.engErr(w, err)
		return
	}
	if list == nil {
		list = []store.RegistryWebhook{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": list})
}

func (s *Server) createRegistryWebhook(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		Provider string `json:"provider"`
		Secret   string `json:"secret"`
		ApplyNow bool   `json:"applyNow"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Provider = strings.TrimSpace(req.Provider)
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "Webhook name is required.")
		return
	}
	if req.Provider == "" {
		req.Provider = "generic"
	}
	plain, hash := generateRandomToken("mrw_")
	wh, err := s.store.CreateRegistryWebhook(r.Context(), req.Name, req.Provider, hash, strings.TrimSpace(req.Secret), req.ApplyNow)
	if err != nil {
		s.engErr(w, err)
		return
	}
	pub := strings.TrimRight(s.effectivePublicURL(r.Context()), "/")
	hookURL := "/api/webhooks/registry/" + plain
	if pub != "" {
		hookURL = pub + hookURL
	}
	setAuditTarget(r, wh.Name)
	writeJSON(w, http.StatusCreated, map[string]any{
		"webhook": wh,
		"token":   plain,
		"url":     hookURL,
	})
}

func (s *Server) updateRegistryWebhook(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "Invalid webhook id.")
		return
	}
	var req struct {
		Name       string `json:"name"`
		Provider   string `json:"provider"`
		Secret     string `json:"secret"`
		KeepSecret bool   `json:"keepSecret"`
		ApplyNow   bool   `json:"applyNow"`
		Enabled    bool   `json:"enabled"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	wh, err := s.store.UpdateRegistryWebhook(r.Context(), id, strings.TrimSpace(req.Name), strings.TrimSpace(req.Provider), strings.TrimSpace(req.Secret), req.KeepSecret, req.ApplyNow, req.Enabled)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Webhook not found.")
		return
	}
	if err != nil {
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wh)
}

func (s *Server) deleteRegistryWebhook(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "Invalid webhook id.")
		return
	}
	if err := s.store.DeleteRegistryWebhook(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Webhook not found.")
		return
	} else if err != nil {
		s.engErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- Public webhook receiver -----------------------------------------------------------

// handleRegistryWebhook handles POST /api/webhooks/registry/{token} outside session/CSRF.
func (s *Server) handleRegistryWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "Method not allowed.")
		return
	}
	ctx := r.Context()
	if s.store == nil {
		writeErr(w, http.StatusNotFound, "Not found.")
		return
	}
	if on, _ := s.store.AddonEnabled(ctx, addonRegistryWebhooks); !on {
		writeErr(w, http.StatusNotFound, "Not found.")
		return
	}
	if !whIPRate.allow(clientIP(r), 30) {
		writeErr(w, http.StatusTooManyRequests, "Too many webhook requests.")
		return
	}

	token := strings.TrimSpace(r.PathValue("token"))
	if token == "" {
		writeErr(w, http.StatusNotFound, "Not found.")
		return
	}
	wh, err := s.store.RegistryWebhookByHash(ctx, hashToken(token))
	if err != nil || !wh.Enabled {
		writeErr(w, http.StatusNotFound, "Not found.")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Couldn't read payload.")
		return
	}
	if wh.Secret != "" && !verifyHubSignature256(wh.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
		_ = s.store.RecordWebhookHit(ctx, wh.ID, "invalid signature")
		writeErr(w, http.StatusForbidden, "Invalid signature.")
		return
	}

	repos := parseRegistryWebhookRepos(wh.Provider, body)
	summary := "accepted"
	if len(repos) > 0 {
		summary = "repos: " + strings.Join(repos, ", ")
	}
	_ = s.store.RecordWebhookHit(ctx, wh.ID, summary)
	_ = s.store.Audit(ctx, store.AuditEntry{
		At:      time.Now(),
		Actor:   "webhook:" + wh.Name,
		IP:      clientIP(r),
		Action:  "webhook.trigger",
		Target:  summary,
		Outcome: store.AuditOK,
	})

	whDebouncer.schedule("wh:"+strconv.FormatInt(wh.ID, 10), 15*time.Second, func() {
		s.triggerWebhookUpdateCheck(repos, wh.ApplyNow)
	})

	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "debouncedSeconds": 15, "repos": repos})
}

func (s *Server) triggerWebhookUpdateCheck(repos []string, applyNow bool) {
	if s.upd == nil || s.reg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	hosts, err := s.reg.List(ctx)
	if err != nil {
		return
	}
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
		for _, sm := range stacks {
			if !sm.Managed {
				continue
			}
			polRow, _ := s.store.StackUpdatePolicy(ctx, hMeta.ID, sm.Name)
			pol := polRow.Policy
			if pol == "" || pol == store.PolicyOff {
				continue
			}
			if len(repos) > 0 {
				st, err := h.GetStack(ctx, sm.Name)
				if err != nil {
					continue
				}
				lowerCompose := strings.ToLower(st.Compose)
				matched := false
				for _, rp := range repos {
					if strings.Contains(lowerCompose, rp) {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			}
			_, _ = s.upd.CheckNow(ctx, hMeta.ID, sm.Name)
			if applyNow && pol == store.PolicyAuto && !hMeta.ReadOnly {
				action := s.deployAction(ctx, hMeta.ID, sm.Name, "update", "")
				if op, oerr := h.RunAction(ctx, sm.Name, action, "webhook"); oerr == nil && op.ID != "" {
					if action == "rolling-update" {
						s.watchRollback(h, hMeta.ID, sm.Name, op.ID)
					}
				}
			}
		}
	}
}
