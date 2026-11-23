package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v4"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/netguard"
	"moorage/internal/store"
	"moorage/internal/templates"
)

// OfficialTemplatesURL is the optional community/official template index offered as an add-on.
const OfficialTemplatesURL = "https://raw.githubusercontent.com/Literally-davca/moorage-templates/main/index.json"

// AddonsIndexURL is the curated add-on catalog in Literally-davca/moorage-addons.
const AddonsIndexURL = "https://raw.githubusercontent.com/Literally-davca/moorage-addons/main/index.json"

const (
	addonProxyLabels       = "proxy-labels"
	addonOfficialTemplates = "official-templates"
	addonPrometheusGrafana = "prometheus-grafana"
	addonPrometheusLegacy  = "prometheus-metrics"
	addonsStackName        = "add-ons"
)

// AddonInfo describes one built-in or installed add-on in Settings -> Add-ons.
type AddonInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Kind        string `json:"kind"` // builtin | sidecar
	Version     string `json:"version,omitempty"`
	RAM         string `json:"ram,omitempty"`
	CPU         string `json:"cpu,omitempty"`
	HostID      string `json:"hostId,omitempty"`
	StackName   string `json:"stackName,omitempty"`
	Enabled     bool   `json:"enabled"`
	Installed   bool   `json:"installed"`
	Toggleable  bool   `json:"toggleable"`
	Removable   bool   `json:"removable"`
	Detail      string `json:"detail,omitempty"`
	LinkHref    string `json:"linkHref,omitempty"`
	LinkLabel   string `json:"linkLabel,omitempty"`
}

// AddonCatalogEntry is one entry in moorage-addons/index.json.
type AddonCatalogEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Kind        string `json:"kind"` // builtin | sidecar
	Category    string `json:"category"`
	Description string `json:"description"`
	Resources   struct {
		RAM string `json:"ram"`
		CPU string `json:"cpu"`
	} `json:"resources"`
	Readme  string `json:"readme,omitempty"`
	Compose string `json:"compose,omitempty"`
}

type addonIndex struct {
	Format int                 `json:"format"`
	Name   string              `json:"name"`
	Addons []AddonCatalogEntry `json:"addons"`
}

var (
	addonCatMu      sync.Mutex
	addonCatEntries []AddonCatalogEntry
	addonCatAt      time.Time
)

func (s *Server) listAddons(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	proxyOn, err := s.store.AddonEnabled(ctx, addonProxyLabels)
	if err != nil {
		s.engErr(w, err)
		return
	}

	officialOn := false
	if s.tpl != nil {
		if srcs, err := s.tpl.Sources(ctx); err == nil {
			for _, src := range srcs {
				if src.URL == OfficialTemplatesURL {
					officialOn = true
					break
				}
			}
		}
	}

	pubURL := s.effectivePublicURL(ctx)
	metricsURL := "/metrics"
	if pubURL != "" {
		metricsURL = pubURL + "/metrics"
	}
	metricsTok := s.effectiveMetricsToken(ctx)
	metricsDetail := "Enable to generate a bearer token for /metrics (or set MOORAGE_METRICS_TOKEN in environment). Grafana dashboard: deploy/grafana/moorage-dashboard.json."
	if metricsTok != "" {
		metricsDetail = "Serving Prometheus metrics at " + metricsURL + " (bearer auth). Configure token in Settings -> General."
	}

	list := []AddonInfo{
		{
			ID:          addonProxyLabels,
			Name:        "Proxy & dashboard label detection",
			Description: "Automatically detect service web links from Traefik (traefik.http.routers.*.rule), Caddy (caddy*) and Homepage (homepage.href) labels. Native moorage.url labels always work in the core.",
			Category:    "Integrations",
			Kind:        "builtin",
			RAM:         "~0 MB",
			CPU:         "~0%",
			Enabled:     proxyOn,
			Installed:   true,
			Toggleable:  true,
		},
		{
			ID:          addonOfficialTemplates,
			Name:        "Official template catalog (moorage-templates)",
			Description: "Connects the curated Literally-davca/moorage-templates repository as a remote template index with daily refresh.",
			Category:    "Templates",
			Kind:        "builtin",
			RAM:         "+1–2 MB (refresh)",
			CPU:         "~0%",
			Enabled:     officialOn,
			Installed:   true,
			Toggleable:  true,
			Detail:      OfficialTemplatesURL,
			LinkHref:    "#/settings/templates",
			LinkLabel:   "Manage template sources",
		},
		{
			ID:          addonPrometheusGrafana,
			Name:        "Prometheus & Grafana metrics",
			Description: "Exposes host, stack, container and check metrics at /metrics for Prometheus/VictoriaMetrics and includes a Grafana dashboard.",
			Category:    "Observability",
			Kind:        "builtin",
			RAM:         "+1–3 MB",
			CPU:         "< 1%",
			Enabled:     metricsTok != "",
			Installed:   true,
			Toggleable:  s.cfg.MetricsToken == "",
			Detail:      metricsDetail,
			LinkHref:    "#/settings/general",
			LinkLabel:   "General settings",
		},
	}

	installed, err := s.store.InstalledAddons(ctx)
	if err != nil {
		s.engErr(w, err)
		return
	}
	for _, ia := range installed {
		if ia.Kind == "builtin" {
			continue
		}
		href := ""
		label := ""
		if ia.HostID != "" && ia.StackName != "" {
			href = "#/h/" + url.PathEscape(ia.HostID) + "/stacks/" + url.PathEscape(ia.StackName)
			label = "Open stack " + ia.StackName
		}
		list = append(list, AddonInfo{
			ID:          ia.ID,
			Name:        ia.Name,
			Description: ia.Description,
			Category:    ia.Category,
			Kind:        ia.Kind,
			Version:     ia.Version,
			RAM:         ia.RAM,
			CPU:         ia.CPU,
			HostID:      ia.HostID,
			StackName:   ia.StackName,
			Enabled:     ia.Enabled,
			Installed:   true,
			Toggleable:  true,
			Removable:   true,
			LinkHref:    href,
			LinkLabel:   label,
		})
	}

	resp := map[string]any{"addons": list}
	if r.URL.Query().Get("catalog") == "1" {
		cat, cerr := fetchAddonCatalog(ctx, r.URL.Query().Get("refresh") == "1")
		if cerr != nil {
			resp["catalogError"] = cerr.Error()
		} else {
			resp["catalog"] = cat
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) installAddon(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string `json:"id"`
		HostID string `json:"hostId"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "Add-on id is required.")
		return
	}
	ctx := r.Context()

	// Built-ins can simply be enabled via install as well.
	if id == addonProxyLabels || id == addonOfficialTemplates {
		if err := s.toggleBuiltinAddon(ctx, id, true); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		setAuditTarget(r, id)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if _, err := s.store.InstalledAddonByID(ctx, id); err == nil {
		writeErr(w, http.StatusConflict, "This add-on is already installed.")
		return
	}

	cat, err := fetchAddonCatalog(ctx, false)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Couldn't load the add-ons catalog: "+err.Error())
		return
	}
	var entry *AddonCatalogEntry
	for i := range cat {
		if cat[i].ID == id {
			entry = &cat[i]
			break
		}
	}
	if entry == nil {
		writeErr(w, http.StatusNotFound, "Add-on not found in catalog.")
		return
	}
	if entry.Kind != "sidecar" || entry.Compose == "" {
		writeErr(w, http.StatusBadRequest, "This add-on cannot be installed automatically.")
		return
	}
	if !templates.IDPattern.MatchString(entry.ID) || len(entry.ID) > 57 {
		writeErr(w, http.StatusBadRequest, "Invalid add-on identifier in catalog.")
		return
	}
	if s.reg == nil {
		writeErr(w, http.StatusServiceUnavailable, "No Docker host is available.")
		return
	}

	hostID := strings.TrimSpace(req.HostID)
	if hostID == "" {
		hosts, _ := s.reg.List(ctx)
		for _, h := range hosts {
			if h.Connected && !h.ReadOnly {
				hostID = h.ID
				break
			}
		}
	}
	if hostID == "" {
		writeErr(w, http.StatusBadRequest, "Select a connected writable host to install this sidecar add-on.")
		return
	}
	h, meta, err := s.reg.Get(ctx, hostID)
	if err != nil {
		status, msg := s.errStatus(err)
		writeErr(w, status, msg)
		return
	}
	if meta.ReadOnly {
		writeErr(w, http.StatusForbidden, host.ErrHostReadOnly.Error())
		return
	}

	composeURL, err := resolveCatalogURL(AddonsIndexURL, entry.Compose)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	composeRaw, err := fetchCatalogBytes(ctx, composeURL)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Couldn't fetch compose.yaml: "+err.Error())
		return
	}

	stackName := addonsStackName
	user := currentUser(r).Username
	note := "Installed add-on " + entry.Name

	var prevCompose, prevEnv string
	existed := false
	if cur, err := h.GetStack(ctx, stackName); err == nil {
		existed = true
		prevCompose = cur.Compose
		prevEnv = cur.Env
	}

	merged, err := mergeAddonCompose(prevCompose, entry.ID, composeRaw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid add-on compose.yaml: "+err.Error())
		return
	}

	if !existed {
		if nc, ok := h.(host.NotedCreator); ok {
			err = nc.CreateNoted(ctx, stackName, merged, prevEnv, user, note)
		} else {
			err = h.Save(ctx, stackName, merged, prevEnv, user, true)
		}
	} else {
		err = h.Save(ctx, stackName, merged, prevEnv, user, false)
	}
	if err != nil {
		s.engErr(w, err)
		return
	}
	// Wait for `up`: a failure must restore the previous compose.yaml so the other
	// sidecars in the shared stack keep their definitions.
	if err := runAndWait(ctx, h, stackName, "up", user); err != nil {
		if existed {
			_ = h.Save(ctx, stackName, prevCompose, prevEnv, user, false)
		} else {
			_ = h.Delete(ctx, stackName, user)
		}
		s.engErr(w, err)
		return
	}

	if err := s.store.UpsertInstalledAddon(ctx, store.InstalledAddon{
		ID:          entry.ID,
		Name:        entry.Name,
		Kind:        entry.Kind,
		Category:    entry.Category,
		Description: entry.Description,
		Version:     entry.Version,
		HostID:      hostID,
		StackName:   stackName,
		RAM:         entry.Resources.RAM,
		CPU:         entry.Resources.CPU,
		Enabled:     true,
		InstalledAt: time.Now(),
	}); err != nil {
		s.engErr(w, err)
		return
	}
	setAuditTarget(r, id)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) setAddon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()

	switch id {
	case addonProxyLabels, addonOfficialTemplates:
		if err := s.toggleBuiltinAddon(ctx, id, req.Enabled); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		setAuditTarget(r, id)
		w.WriteHeader(http.StatusNoContent)

	case addonPrometheusGrafana, addonPrometheusLegacy:
		if s.cfg.MetricsToken != "" {
			writeErr(w, http.StatusConflict, "MOORAGE_METRICS_TOKEN is set in environment.")
			return
		}
		if req.Enabled {
			if tok := s.effectiveMetricsToken(ctx); tok == "" {
				s.generateMetricsToken(w, r)
				return
			}
		} else {
			if err := s.store.SetSetting(ctx, store.SettingMetricsToken, ""); err != nil {
				s.engErr(w, err)
				return
			}
		}
		setAuditTarget(r, id)
		w.WriteHeader(http.StatusNoContent)

	default:
		ia, err := s.store.InstalledAddonByID(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "Add-on not found.")
			return
		}
		if err != nil {
			s.engErr(w, err)
			return
		}
		if ia.Kind == "sidecar" && ia.HostID != "" && ia.StackName != "" && s.reg != nil {
			h, meta, err := s.reg.Get(ctx, ia.HostID)
			if err != nil {
				status, msg := s.errStatus(err)
				writeErr(w, status, msg)
				return
			}
			if meta.ReadOnly {
				writeErr(w, http.StatusForbidden, host.ErrHostReadOnly.Error())
				return
			}
			user := currentUser(r).Username
			if ia.StackName == addonsStackName {
				if cur, err := h.GetStack(ctx, ia.StackName); err == nil {
					if nextCompose, changed, err := setAddonServiceDisabled(cur.Compose, ia.ID, !req.Enabled); err == nil && changed {
						if err := h.Save(ctx, ia.StackName, nextCompose, cur.Env, user, false); err != nil {
							s.engErr(w, err)
							return
						}
						if err := runAndWait(ctx, h, ia.StackName, "up", user); err != nil {
							s.engErr(w, err)
							return
						}
					}
				}
			} else {
				action := "stop"
				if req.Enabled {
					action = "up"
				}
				if _, err := h.RunAction(ctx, ia.StackName, action, user); err != nil {
					s.engErr(w, err)
					return
				}
			}
		}
		ia.Enabled = req.Enabled
		if err := s.store.UpsertInstalledAddon(ctx, ia); err != nil {
			s.engErr(w, err)
			return
		}
		setAuditTarget(r, id)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) uninstallAddon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	if id == addonProxyLabels || id == addonOfficialTemplates || id == addonPrometheusGrafana || id == addonPrometheusLegacy {
		writeErr(w, http.StatusConflict, "Built-in add-ons can be disabled, not uninstalled.")
		return
	}

	ia, err := s.store.InstalledAddonByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Add-on not found.")
		return
	}
	if err != nil {
		s.engErr(w, err)
		return
	}

	if ia.Kind == "sidecar" && ia.HostID != "" && ia.StackName != "" && s.reg != nil {
		h, meta, err := s.reg.Get(ctx, ia.HostID)
		if err != nil && !errors.Is(err, host.ErrUnknownHost) {
			status, msg := s.errStatus(err)
			writeErr(w, status, msg)
			return
		}
		if err == nil {
			if meta.ReadOnly {
				writeErr(w, http.StatusForbidden, host.ErrHostReadOnly.Error())
				return
			}
			user := currentUser(r).Username
			if ia.StackName == addonsStackName {
				if cur, err := h.GetStack(ctx, ia.StackName); err == nil {
					nextCompose, empty, err := removeAddonFromCompose(cur.Compose, ia.ID)
					if err != nil {
						s.engErr(w, err)
						return
					}
					if empty {
						if op, err := h.RunAction(ctx, ia.StackName, "down", user); err == nil && op.ID != "" {
							_ = h.FollowOp(ctx, op.ID, func(host.OpEvent) error { return nil })
						}
						if err := h.Delete(ctx, ia.StackName, user); err != nil && !errors.Is(err, engine.ErrNotFound) {
							s.engErr(w, err)
							return
						}
					} else {
						// `up` never removes containers of a service that left compose.yaml (no
						// --remove-orphans), so scale the add-on to 0 first, then drop its definition.
						if scaled, changed, err := setAddonServiceDisabled(cur.Compose, ia.ID, true); err == nil && changed {
							if err := h.Save(ctx, ia.StackName, scaled, cur.Env, user, false); err != nil {
								s.engErr(w, err)
								return
							}
							if err := runAndWait(ctx, h, ia.StackName, "up", user); err != nil {
								s.engErr(w, err)
								return
							}
						}
						if err := h.Save(ctx, ia.StackName, nextCompose, cur.Env, user, false); err != nil {
							s.engErr(w, err)
							return
						}
					}
				}
			} else {
				if op, err := h.RunAction(ctx, ia.StackName, "down", user); err == nil && op.ID != "" {
					_ = h.FollowOp(ctx, op.ID, func(host.OpEvent) error { return nil })
				}
				if err := h.Delete(ctx, ia.StackName, user); err != nil && !errors.Is(err, engine.ErrNotFound) {
					s.engErr(w, err)
					return
				}
			}
		}
	}

	if err := s.store.DeleteInstalledAddon(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.engErr(w, err)
		return
	}
	setAuditTarget(r, id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) toggleBuiltinAddon(ctx context.Context, id string, enabled bool) error {
	switch id {
	case addonProxyLabels:
		return s.store.SetAddon(ctx, addonProxyLabels, enabled)
	case addonOfficialTemplates:
		if s.tpl == nil {
			return errors.New("Templates service is not available.")
		}
		if enabled {
			if _, err := s.tpl.AddSource(ctx, OfficialTemplatesURL, false); err != nil {
				var ie templates.InputError
				if errors.As(err, &ie) && strings.Contains(ie.Msg, "already added") {
					return nil
				}
				return err
			}
			return nil
		}
		srcs, err := s.tpl.Sources(ctx)
		if err != nil {
			return err
		}
		for _, src := range srcs {
			if src.URL == OfficialTemplatesURL {
				if err := s.tpl.DeleteSource(ctx, src.ID); err != nil && !errors.Is(err, templates.ErrNotFound) {
					return err
				}
			}
		}
		return nil
	default:
		return errors.New("unknown built-in add-on")
	}
}

func fetchAddonCatalog(ctx context.Context, refresh bool) ([]AddonCatalogEntry, error) {
	addonCatMu.Lock()
	if !refresh && len(addonCatEntries) > 0 && time.Since(addonCatAt) < 10*time.Minute {
		out := append([]AddonCatalogEntry(nil), addonCatEntries...)
		addonCatMu.Unlock()
		return out, nil
	}
	addonCatMu.Unlock()

	raw, err := fetchCatalogBytes(ctx, AddonsIndexURL)
	if err != nil {
		return nil, err
	}
	var idx addonIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, errors.New("invalid add-ons index.json")
	}
	if idx.Format != 1 {
		return nil, fmt.Errorf("unsupported add-ons index format %d", idx.Format)
	}

	addonCatMu.Lock()
	addonCatEntries = idx.Addons
	addonCatAt = time.Now()
	addonCatMu.Unlock()
	return idx.Addons, nil
}

func resolveCatalogURL(indexURL, rel string) (string, error) {
	if strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") || strings.Contains(rel, "\\") {
		return "", errors.New("invalid add-on file path")
	}
	base, err := url.Parse(indexURL)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(rel)
	if err != nil || ref.IsAbs() {
		return "", errors.New("invalid add-on file path")
	}
	resolved := base.ResolveReference(ref)
	if resolved.Scheme != "https" || resolved.Hostname() != base.Hostname() {
		return "", errors.New("add-on file must stay on the index host over HTTPS")
	}
	return resolved.String(), nil
}

func fetchCatalogBytes(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return nil, errors.New("only HTTPS URLs are allowed")
	}
	c := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: netguard.DialContext(10*time.Second, false),
		},
	}
	defer c.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Moorage-Addons")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	const maxBytes = 1 << 20
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBytes {
		return nil, errors.New("add-on file exceeds 1 MiB")
	}
	return b, nil
}

func parseComposeMap(raw []byte) (map[string]any, error) {
	doc := map[string]any{}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return doc, nil
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func setServiceAddonLabel(svc map[string]any, addonID string) {
	switch lbl := svc["labels"].(type) {
	case map[string]any:
		lbl["moorage.addon"] = addonID
	case []any:
		out := make([]any, 0, len(lbl)+1)
		for _, item := range lbl {
			if s, ok := item.(string); ok && strings.HasPrefix(s, "moorage.addon=") {
				continue
			}
			out = append(out, item)
		}
		out = append(out, "moorage.addon="+addonID)
		svc["labels"] = out
	default:
		svc["labels"] = map[string]any{"moorage.addon": addonID}
	}
}

func hasServiceAddonLabel(svc map[string]any, addonID string) bool {
	switch lbl := svc["labels"].(type) {
	case map[string]any:
		v, _ := lbl["moorage.addon"].(string)
		return v == addonID
	case []any:
		want := "moorage.addon=" + addonID
		for _, item := range lbl {
			if s, ok := item.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

func mergeAddonCompose(existingYAML string, addonID string, addonRaw []byte) (string, error) {
	base, err := parseComposeMap([]byte(existingYAML))
	if err != nil {
		return "", err
	}
	part, err := parseComposeMap(addonRaw)
	if err != nil {
		return "", err
	}
	partSvcs := asMap(part["services"])
	if len(partSvcs) == 0 {
		return "", errors.New("compose.yaml defines no services")
	}

	baseSvcs := asMap(base["services"])
	if baseSvcs == nil {
		baseSvcs = map[string]any{}
		base["services"] = baseSvcs
	}
	for name, rawSvc := range partSvcs {
		svc := asMap(rawSvc)
		if svc == nil {
			continue
		}
		if cur := asMap(baseSvcs[name]); cur != nil && !hasServiceAddonLabel(cur, addonID) {
			return "", fmt.Errorf("service %q already exists in the add-ons stack", name)
		}
		setServiceAddonLabel(svc, addonID)
		baseSvcs[name] = svc
	}

	for _, topKey := range []string{"volumes", "networks"} {
		partTop := asMap(part[topKey])
		if len(partTop) == 0 {
			continue
		}
		baseTop := asMap(base[topKey])
		if baseTop == nil {
			baseTop = map[string]any{}
			base[topKey] = baseTop
		}
		for k, v := range partTop {
			if _, exists := baseTop[k]; !exists {
				baseTop[k] = v
			}
		}
	}

	out, err := yaml.Marshal(base)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// runAndWait starts a compose action and waits for it to finish, returning its error.
func runAndWait(ctx context.Context, h host.Host, stack, action, user string) error {
	op, err := h.RunAction(ctx, stack, action, user)
	if err != nil {
		return err
	}
	if op.ID == "" {
		return nil
	}
	var opErr string
	if err := h.FollowOp(ctx, op.ID, func(ev host.OpEvent) error {
		if ev.Done {
			opErr = ev.Error
		}
		return nil
	}); err != nil {
		return err
	}
	if opErr != "" {
		return fmt.Errorf("%s failed: %s", action, opErr)
	}
	return nil
}

// setAddonServiceDisabled scales the add-on's services to 0 (disabled) or back. Unlike a
// compose profile, scale 0 makes `up` really stop and remove the running container.
func setAddonServiceDisabled(existingYAML string, addonID string, disabled bool) (string, bool, error) {
	doc, err := parseComposeMap([]byte(existingYAML))
	if err != nil {
		return "", false, err
	}
	svcs := asMap(doc["services"])
	if len(svcs) == 0 {
		return existingYAML, false, nil
	}
	changed := false
	for name, rawSvc := range svcs {
		svc := asMap(rawSvc)
		if svc == nil {
			continue
		}
		if !hasServiceAddonLabel(svc, addonID) && name != addonID {
			continue
		}
		delete(svc, "profiles") // older builds disabled services through a profile
		if disabled {
			svc["scale"] = 0
		} else {
			delete(svc, "scale")
		}
		if dep := asMap(svc["deploy"]); dep != nil {
			delete(dep, "replicas") // would conflict with scale
		}
		changed = true
	}
	if !changed {
		return existingYAML, false, nil
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", false, err
	}
	return string(out), true, nil
}

func removeAddonFromCompose(existingYAML string, addonID string) (string, bool, error) {
	doc, err := parseComposeMap([]byte(existingYAML))
	if err != nil {
		return "", false, err
	}
	svcs := asMap(doc["services"])
	for name, rawSvc := range svcs {
		svc := asMap(rawSvc)
		if svc != nil && (hasServiceAddonLabel(svc, addonID) || name == addonID) {
			delete(svcs, name)
		}
	}
	if len(svcs) == 0 {
		return "", true, nil
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", false, err
	}
	return string(out), false, nil
}
