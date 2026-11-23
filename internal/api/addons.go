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

	metricsURL := "/metrics"
	if s.cfg.PublicURL != "" {
		metricsURL = strings.TrimRight(s.cfg.PublicURL, "/") + "/metrics"
	}
	metricsDetail := "Set MOORAGE_METRICS_TOKEN (min. 16 characters) in the hub environment to enable /metrics. Grafana dashboard JSON lives in deploy/grafana/moorage-dashboard.json."
	if s.cfg.MetricsToken != "" {
		metricsDetail = "Serving Prometheus metrics at " + metricsURL + " (bearer auth). Grafana dashboard JSON: deploy/grafana/moorage-dashboard.json."
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
			Enabled:     s.cfg.MetricsToken != "",
			Installed:   true,
			Toggleable:  false,
			Detail:      metricsDetail,
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

	stackName := "addon-" + entry.ID
	user := currentUser(r).Username
	note := "Installed add-on " + entry.Name
	if nc, ok := h.(host.NotedCreator); ok {
		err = nc.CreateNoted(ctx, stackName, string(composeRaw), "", user, note)
	} else {
		err = h.Save(ctx, stackName, string(composeRaw), "", user, true)
	}
	if err != nil {
		s.engErr(w, err)
		return
	}
	if _, err := h.RunAction(ctx, stackName, "up", user); err != nil {
		_ = h.Delete(ctx, stackName, user)
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
		writeErr(w, http.StatusConflict, "Prometheus metrics are configured via MOORAGE_METRICS_TOKEN on the hub.")

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
			action := "stop"
			if req.Enabled {
				action = "up"
			}
			if _, err := h.RunAction(ctx, ia.StackName, action, currentUser(r).Username); err != nil {
				s.engErr(w, err)
				return
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
			if op, err := h.RunAction(ctx, ia.StackName, "down", currentUser(r).Username); err == nil && op.ID != "" {
				_ = h.FollowOp(ctx, op.ID, func(host.OpEvent) error { return nil })
			}
			if err := h.Delete(ctx, ia.StackName, currentUser(r).Username); err != nil && !errors.Is(err, engine.ErrNotFound) {
				s.engErr(w, err)
				return
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
