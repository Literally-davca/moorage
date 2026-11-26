package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
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
	addonCoolify           = "coolify-integration"
	addonStackScheduler    = "stack-scheduler"
	addonSocketProxyCheck  = "socket-proxy-check"
	addonRegistryWebhooks  = "registry-webhooks"
	addonMCPServer         = "mcp-server"
	addonsStackName        = "add-ons"
)

var builtinAddonIDs = map[string]bool{
	addonProxyLabels:       true,
	addonOfficialTemplates: true,
	addonPrometheusGrafana: true,
	addonPrometheusLegacy:  true,
	addonCoolify:           true,
	addonStackScheduler:    true,
	addonSocketProxyCheck:  true,
	addonRegistryWebhooks:  true,
	addonMCPServer:         true,
}

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
	HostName    string `json:"hostName,omitempty"`
	StackName   string `json:"stackName,omitempty"`
	Enabled     bool   `json:"enabled"`
	Installed   bool   `json:"installed"`
	Toggleable  bool   `json:"toggleable"`
	Removable   bool   `json:"removable"`
	Custom      bool   `json:"custom,omitempty"`
	Source      string `json:"source,omitempty"`
	Detail      string `json:"detail,omitempty"`
	LinkHref    string `json:"linkHref,omitempty"`
	LinkLabel   string `json:"linkLabel,omitempty"`
	DocsSlug    string `json:"docsSlug,omitempty"`
	DocsURL     string `json:"docsUrl,omitempty"`
}

// AddonCatalogEntry is one entry in moorage-addons/index.json or a custom addon_source.
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
	Readme       string `json:"readme,omitempty"`
	Compose      string `json:"compose,omitempty"`
	SourceID     int64  `json:"sourceId,omitempty"`
	SourceName   string `json:"sourceName,omitempty"`
	SourceURL    string `json:"sourceUrl,omitempty"`
	AllowPrivate bool   `json:"-"`
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
	coolifyOn, _ := s.store.AddonEnabled(ctx, addonCoolify)
	schedOn, _ := s.store.AddonEnabled(ctx, addonStackScheduler)
	sockOn, _ := s.store.AddonEnabled(ctx, addonSocketProxyCheck)
	webhookOn, _ := s.store.AddonEnabled(ctx, addonRegistryWebhooks)
	mcpOn, _ := s.store.AddonEnabled(ctx, addonMCPServer)

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
	metricsDetail := "Enable to generate a bearer token for /metrics (or set MOORAGE_METRICS_TOKEN in environment) and unlock the Grafana, VictoriaMetrics and Homelab generators."
	if metricsTok != "" {
		metricsDetail = "Serving Prometheus metrics at " + metricsURL + " (bearer auth)."
	}

	list := []AddonInfo{
		{
			ID:          addonProxyLabels,
			Name:        "Proxy & dashboard label detection",
			Description: "Automatically detect service web links from Traefik (traefik.http.routers.*.rule), Caddy (caddy*) and Homepage (homepage.href) labels, deduplicating HTTP/HTTPS routers.",
			Category:    "Integrations",
			Kind:        "builtin",
			RAM:         "~0 MB",
			CPU:         "~0%",
			Enabled:     proxyOn,
			Installed:   true,
			Toggleable:  true,
			DocsSlug:    "proxy-labels",
			DocsURL:     "https://github.com/Literally-davca/moorage/blob/main/docs/addons.md#proxy-labels",
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
			DocsSlug:    "official-templates",
			DocsURL:     "https://github.com/Literally-davca/moorage/blob/main/docs/addons.md#official-templates",
		},
		{
			ID:          addonPrometheusGrafana,
			Name:        "Prometheus, Grafana & Homelab generators",
			Description: "Exposes /metrics for Prometheus/VictoriaMetrics and generates tailored Grafana dashboards (with 1-click Push to Grafana), scrape/alert rules and Homepage/Glance/Homarr configs.",
			Category:    "Observability",
			Kind:        "builtin",
			RAM:         "+1–3 MB",
			CPU:         "< 1%",
			Enabled:     metricsTok != "",
			Installed:   true,
			Toggleable:  s.cfg.MetricsToken == "",
			Detail:      metricsDetail,
			DocsSlug:    "prometheus-grafana",
			DocsURL:     "https://github.com/Literally-davca/moorage/blob/main/docs/addons.md#prometheus-grafana",
		},
		{
			ID:          addonCoolify,
			Name:        "Coolify integration",
			Description: "Reads Coolify labels and /api/v1 resources: human display names over stable UUIDs, collapsible project groups, Start/Stop/Restart/Redeploy actions, deployment logs and .env sync.",
			Category:    "Integrations",
			Kind:        "builtin",
			RAM:         "+2–5 MB",
			CPU:         "< 1%",
			Enabled:     coolifyOn,
			Installed:   true,
			Toggleable:  true,
			LinkHref:    "#/settings/connections",
			LinkLabel:   "Configure Coolify API",
			DocsSlug:    "coolify",
			DocsURL:     "https://github.com/Literally-davca/moorage/blob/main/docs/coolify.md",
		},
		{
			ID:          addonStackScheduler,
			Name:        "Stack scheduler (cron tasks)",
			Description: "Run scheduled restart, start, stop, pull + up, or container exec jobs on 5-field cron schedules with Logbook, audit trail and failure alerts.",
			Category:    "Automation",
			Kind:        "builtin",
			RAM:         "+1 MB",
			CPU:         "~0% idle",
			Enabled:     schedOn,
			Installed:   true,
			Toggleable:  true,
			DocsSlug:    "stack-scheduler",
			DocsURL:     "https://github.com/Literally-davca/moorage/blob/main/docs/addons.md#stack-scheduler",
		},
		{
			ID:          addonSocketProxyCheck,
			Name:        "Docker socket security guard",
			Description: "Audits stacks and containers for raw /var/run/docker.sock mounts and unencrypted TCP :2375 sockets, with a guided tecnativa/docker-socket-proxy generator.",
			Category:    "Security",
			Kind:        "builtin",
			RAM:         "~0 MB",
			CPU:         "~0%",
			Enabled:     sockOn,
			Installed:   true,
			Toggleable:  true,
			DocsSlug:    "socket-proxy-check",
			DocsURL:     "https://github.com/Literally-davca/moorage/blob/main/docs/addons.md#socket-proxy-check",
		},
		{
			ID:          addonRegistryWebhooks,
			Name:        "Instant registry push webhooks",
			Description: "Unique webhook endpoints for GitHub Container Registry, Docker Hub and Gitea/Harbor that trigger immediate image checks and optional auto-updates after CI push.",
			Category:    "Automation",
			Kind:        "builtin",
			RAM:         "~0 MB",
			CPU:         "~0% idle",
			Enabled:     webhookOn,
			Installed:   true,
			Toggleable:  true,
			DocsSlug:    "registry-webhooks",
			DocsURL:     "https://github.com/Literally-davca/moorage/blob/main/docs/addons.md#registry-webhooks",
		},
		{
			ID:          addonMCPServer,
			Name:        "Scoped MCP server for AI assistants",
			Description: "Exposes Streamable HTTP /mcp with per-host/stack ACL tokens, write-only .env secret handling, and optional human-approval deploy drafts.",
			Category:    "AI & Automation",
			Kind:        "builtin",
			RAM:         "+1–2 MB",
			CPU:         "~0% idle",
			Enabled:     mcpOn,
			Installed:   true,
			Toggleable:  true,
			DocsSlug:    "mcp-server",
			DocsURL:     "https://github.com/Literally-davca/moorage/blob/main/docs/addons.md#mcp-server",
		},
	}

	hostNames := s.hostNames(ctx)
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
			HostName:    hostNames[ia.HostID],
			StackName:   ia.StackName,
			Enabled:     ia.Enabled,
			Installed:   true,
			Toggleable:  true,
			Removable:   true,
			Custom:      ia.Custom,
			Source:      ia.Source,
			LinkHref:    href,
			LinkLabel:   label,
		})
	}

	resp := map[string]any{"addons": list}
	if r.URL.Query().Get("catalog") == "1" {
		cat, cerr := s.fetchAllAddonCatalogs(ctx, r.URL.Query().Get("refresh") == "1")
		if cerr != nil && len(cat) == 0 {
			resp["catalogError"] = cerr.Error()
		} else {
			resp["catalog"] = cat
			if cerr != nil {
				resp["catalogError"] = cerr.Error()
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) installAddon(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID      string   `json:"id"`
		HostID  string   `json:"hostId"`
		HostIDs []string `json:"hostIds"`
		Source  string   `json:"source"`
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

	if builtinAddonIDs[id] && id != addonPrometheusGrafana && id != addonPrometheusLegacy {
		if err := s.toggleBuiltinAddon(ctx, id, true); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		setAuditTarget(r, id)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	cat, err := s.fetchAllAddonCatalogs(ctx, false)
	if err != nil && len(cat) == 0 {
		writeErr(w, http.StatusBadGateway, "Couldn't load the add-ons catalog: "+err.Error())
		return
	}
	var entry *AddonCatalogEntry
	for i := range cat {
		if cat[i].ID == id && (req.Source == "" || cat[i].SourceURL == req.Source || cat[i].SourceName == req.Source) {
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

	var targetHosts []string
	seenH := map[string]bool{}
	for _, hid := range append(req.HostIDs, req.HostID) {
		hid = strings.TrimSpace(hid)
		if hid != "" && !seenH[hid] {
			seenH[hid] = true
			targetHosts = append(targetHosts, hid)
		}
	}
	if len(targetHosts) == 0 {
		hosts, _ := s.reg.List(ctx)
		for _, h := range hosts {
			if h.Connected && !h.ReadOnly {
				targetHosts = append(targetHosts, h.ID)
				break
			}
		}
	}
	if len(targetHosts) == 0 {
		writeErr(w, http.StatusBadRequest, "Select at least one connected writable host.")
		return
	}

	indexBaseURL := entry.SourceURL
	if indexBaseURL == "" {
		indexBaseURL = AddonsIndexURL
	}
	composeURL, err := resolveCatalogURL(indexBaseURL, entry.Compose)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	composeRaw, err := fetchCatalogBytesOpts(ctx, composeURL, entry.AllowPrivate)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Couldn't fetch compose.yaml: "+err.Error())
		return
	}

	user := currentUser(r).Username
	var envRes addonEnvResult
	for _, hostID := range targetHosts {
		if _, err := s.store.InstalledAddonByHost(ctx, id, hostID); err == nil {
			writeErr(w, http.StatusConflict, fmt.Sprintf("Add-on %q is already installed on host %s.", id, hostID))
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
		// Zero-config: fill the host-specific variables the fragment uses into add-ons/.env.
		prevEnv := ""
		if cur, err := h.GetStack(ctx, addonsStackName); err == nil {
			prevEnv = cur.Env
		}
		nextEnv, res := withAddonEnvDefaults(prevEnv, string(composeRaw), s.addonEnvDefaults(ctx, hostID, h, meta))
		envRes.Added = append(envRes.Added, res.Added...)
		envRes.Generated = append(envRes.Generated, res.Generated...)
		if err := s.deploySidecarToHost(ctx, h, hostID, entry.ID, "Installed add-on "+entry.Name, composeRaw, nextEnv, nextEnv != prevEnv, user); err != nil {
			s.engErr(w, err)
			return
		}
		srcLabel := entry.SourceName
		if srcLabel == "" {
			srcLabel = "official"
		}
		if err := s.store.UpsertInstalledAddon(ctx, store.InstalledAddon{
			ID:              entry.ID,
			HostID:          hostID,
			Name:            entry.Name,
			Kind:            entry.Kind,
			Category:        entry.Category,
			Description:     entry.Description,
			Version:         entry.Version,
			StackName:       addonsStackName,
			RAM:             entry.Resources.RAM,
			CPU:             entry.Resources.CPU,
			Enabled:         true,
			InstalledAt:     time.Now(),
			ComposeFragment: string(composeRaw),
			Source:          srcLabel,
		}); err != nil {
			s.engErr(w, err)
			return
		}
	}

	setAuditTarget(r, id)
	writeJSON(w, http.StatusCreated, envRes)
}

func (s *Server) deploySidecarToHost(
	ctx context.Context,
	h host.Host,
	hostID string,
	addonID, note string,
	composeFragment []byte,
	nextEnv string,
	overrideEnv bool,
	user string,
) error {
	stackName := addonsStackName
	var prevCompose, prevEnv string
	existed := false
	if cur, err := h.GetStack(ctx, stackName); err == nil {
		existed = true
		prevCompose = cur.Compose
		prevEnv = cur.Env
	}
	targetEnv := prevEnv
	if overrideEnv {
		targetEnv = nextEnv
	}
	// Replace any existing definition of this addonID cleanly when updating a custom sidecar.
	baseCompose := prevCompose
	if baseCompose != "" {
		if stripped, _, err := removeAddonFromCompose(baseCompose, addonID); err == nil {
			baseCompose = stripped
		}
	}
	merged, err := mergeAddonCompose(baseCompose, addonID, composeFragment)
	if err != nil {
		return engine.ValidationError{Msg: err.Error()}
	}
	if !existed {
		if nc, ok := h.(host.NotedCreator); ok {
			err = nc.CreateNoted(ctx, stackName, merged, targetEnv, user, note)
		} else {
			err = h.Save(ctx, stackName, merged, targetEnv, user, true)
		}
	} else {
		err = h.Save(ctx, stackName, merged, targetEnv, user, false)
	}
	if err != nil {
		return err
	}
	if err := runAndWait(ctx, h, stackName, "up", user); err != nil {
		if existed {
			_ = h.Save(ctx, stackName, prevCompose, prevEnv, user, false)
			_ = runAndWait(ctx, h, stackName, "up", user)
		} else {
			_ = h.Delete(ctx, stackName, user)
		}
		return err
	}
	if !existed {
		s.ensureAddonsAutoPolicy(ctx, hostID)
	}
	return nil
}

func (s *Server) resolveInstalledSidecar(ctx context.Context, id, hostQuery string) (store.InstalledAddon, error) {
	if hostQuery != "" {
		return s.store.InstalledAddonByHost(ctx, id, hostQuery)
	}
	all, err := s.store.InstalledAddonsByID(ctx, id)
	if err != nil {
		return store.InstalledAddon{}, err
	}
	if len(all) == 0 {
		return store.InstalledAddon{}, store.ErrNotFound
	}
	if len(all) > 1 {
		return store.InstalledAddon{}, errors.New("specify ?host=<hostId> because this add-on is installed on multiple hosts")
	}
	return all[0], nil
}

func (s *Server) setAddon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	hostQuery := strings.TrimSpace(r.URL.Query().Get("host"))
	var req struct {
		Enabled bool   `json:"enabled"`
		HostID  string `json:"hostId"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if hostQuery == "" {
		hostQuery = strings.TrimSpace(req.HostID)
	}
	ctx := r.Context()

	switch id {
	case addonProxyLabels, addonOfficialTemplates, addonCoolify, addonStackScheduler, addonSocketProxyCheck, addonRegistryWebhooks, addonMCPServer:
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
		ia, err := s.resolveInstalledSidecar(ctx, id, hostQuery)
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "Add-on not found.")
			return
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
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
		setAuditTarget(r, id+"@"+ia.HostID)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) uninstallAddon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	hostQuery := strings.TrimSpace(r.URL.Query().Get("host"))
	ctx := r.Context()

	if builtinAddonIDs[id] {
		writeErr(w, http.StatusConflict, "Built-in add-ons can be disabled, not uninstalled.")
		return
	}

	ia, err := s.resolveInstalledSidecar(ctx, id, hostQuery)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Add-on not found.")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
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
					nextEnv := removeEnvKeys(cur.Env, ia.EnvKeys)
					if empty {
						if op, err := h.RunAction(ctx, ia.StackName, "down", user); err == nil && op.ID != "" {
							_ = h.FollowOp(ctx, op.ID, func(host.OpEvent) error { return nil })
						}
						if err := h.Delete(ctx, ia.StackName, user); err != nil && !errors.Is(err, engine.ErrNotFound) {
							s.engErr(w, err)
							return
						}
					} else {
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
						if err := h.Save(ctx, ia.StackName, nextCompose, nextEnv, user, false); err != nil {
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

	if err := s.store.DeleteInstalledAddon(ctx, id, ia.HostID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.engErr(w, err)
		return
	}
	setAuditTarget(r, id+"@"+ia.HostID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) toggleBuiltinAddon(ctx context.Context, id string, enabled bool) error {
	switch id {
	case addonProxyLabels, addonCoolify, addonStackScheduler, addonSocketProxyCheck, addonRegistryWebhooks, addonMCPServer:
		if err := s.store.SetAddon(ctx, id, enabled); err != nil {
			return err
		}
		if id == addonCoolify && enabled && s.coolify != nil {
			s.coolify.TriggerSync()
		}
		return nil
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

// ---- Add-on catalogs (official + custom HTTPS addon_sources) ---------------------------

func (s *Server) fetchAllAddonCatalogs(ctx context.Context, refresh bool) ([]AddonCatalogEntry, error) {
	var all []AddonCatalogEntry
	var errs []string

	official, err := fetchAddonCatalog(ctx, refresh)
	if err != nil {
		errs = append(errs, "official: "+err.Error())
	} else {
		for _, item := range official {
			item.SourceName = "Official"
			item.SourceURL = AddonsIndexURL
			all = append(all, item)
		}
	}

	if s.store != nil {
		srcs, _ := s.store.AddonSources(ctx)
		for _, src := range srcs {
			if !src.Enabled {
				continue
			}
			entries, serr := s.fetchCustomAddonSource(ctx, src, refresh)
			if serr != nil {
				errs = append(errs, src.Name+": "+serr.Error())
			}
			all = append(all, entries...)
		}
	}

	var combinedErr error
	if len(errs) > 0 {
		combinedErr = errors.New(strings.Join(errs, "; "))
	}
	return all, combinedErr
}

func (s *Server) fetchCustomAddonSource(ctx context.Context, src store.AddonSource, refresh bool) ([]AddonCatalogEntry, error) {
	rawStr := src.Cache
	var fetchErr error
	if refresh || rawStr == "" || time.Since(src.FetchedAt) >= 10*time.Minute {
		b, err := fetchCatalogBytesOpts(ctx, src.URL, src.AllowPrivate)
		if err != nil {
			fetchErr = err
			_ = s.store.UpdateAddonSourceFetch(ctx, src.ID, "", err.Error())
		} else {
			idx, perr := parseAddonIndex(b)
			if perr != nil {
				fetchErr = perr
				_ = s.store.UpdateAddonSourceFetch(ctx, src.ID, "", perr.Error())
			} else {
				rawStr = string(b)
				_ = s.store.UpdateAddonSourceFetch(ctx, src.ID, rawStr, "")
				return decorateCatalogEntries(idx.Addons, src), nil
			}
		}
	}
	if rawStr != "" {
		if idx, err := parseAddonIndex([]byte(rawStr)); err == nil {
			return decorateCatalogEntries(idx.Addons, src), fetchErr
		}
	}
	return nil, fetchErr
}

func decorateCatalogEntries(entries []AddonCatalogEntry, src store.AddonSource) []AddonCatalogEntry {
	out := make([]AddonCatalogEntry, 0, len(entries))
	for _, e := range entries {
		e.SourceID = src.ID
		e.SourceName = src.Name
		e.SourceURL = src.URL
		e.AllowPrivate = src.AllowPrivate
		out = append(out, e)
	}
	return out
}

func parseAddonIndex(raw []byte) (addonIndex, error) {
	var idx addonIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return idx, errors.New("invalid add-ons index.json")
	}
	if idx.Format != 1 {
		return idx, fmt.Errorf("unsupported add-ons index format %d", idx.Format)
	}
	return idx, nil
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
		addonCatMu.Lock()
		cached := append([]AddonCatalogEntry(nil), addonCatEntries...)
		addonCatMu.Unlock()
		if len(cached) > 0 {
			return cached, err
		}
		return nil, err
	}
	idx, err := parseAddonIndex(raw)
	if err != nil {
		return nil, err
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
	return fetchCatalogBytesOpts(ctx, rawURL, false)
}

func fetchCatalogBytesOpts(ctx context.Context, rawURL string, allowPrivate bool) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return nil, errors.New("only HTTPS URLs are allowed")
	}
	c := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: netguard.DialContext(10*time.Second, allowPrivate),
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

// ---- Addon sources HTTP handlers -------------------------------------------------------

func (s *Server) listAddonSources(w http.ResponseWriter, r *http.Request) {
	srcs, err := s.store.AddonSources(r.Context())
	if err != nil {
		s.engErr(w, err)
		return
	}
	if srcs == nil {
		srcs = []store.AddonSource{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": srcs})
}

func (s *Server) addAddonSource(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string `json:"name"`
		URL          string `json:"url"`
		AllowPrivate bool   `json:"allowPrivate"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	rawURL := strings.TrimSpace(req.URL)
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		writeErr(w, http.StatusBadRequest, "Enter a valid HTTPS URL pointing to index.json.")
		return
	}
	if name == "" {
		name = u.Hostname()
	}
	b, err := fetchCatalogBytesOpts(r.Context(), rawURL, req.AllowPrivate)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Couldn't fetch index.json: "+err.Error())
		return
	}
	idx, err := parseAddonIndex(b)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if req.Name == "" && idx.Name != "" {
		name = idx.Name
	}
	src, err := s.store.AddAddonSource(r.Context(), name, rawURL, req.AllowPrivate, string(b))
	if errors.Is(err, store.ErrAddonSourceExists) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		s.engErr(w, err)
		return
	}
	setAuditTarget(r, name)
	writeJSON(w, http.StatusCreated, src)
}

func (s *Server) updateAddonSource(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "Invalid source id.")
		return
	}
	var req struct {
		Name         string `json:"name"`
		URL          string `json:"url"`
		AllowPrivate bool   `json:"allowPrivate"`
		Enabled      bool   `json:"enabled"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	rawURL := strings.TrimSpace(req.URL)
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		writeErr(w, http.StatusBadRequest, "Enter a valid HTTPS URL pointing to index.json.")
		return
	}
	if name == "" {
		name = u.Hostname()
	}
	src, err := s.store.UpdateAddonSource(r.Context(), id, name, rawURL, req.AllowPrivate, req.Enabled)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Add-on source not found.")
		return
	}
	if errors.Is(err, store.ErrAddonSourceExists) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, src)
}

func (s *Server) deleteAddonSource(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "Invalid source id.")
		return
	}
	if err := s.store.DeleteAddonSource(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Add-on source not found.")
		return
	} else if err != nil {
		s.engErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) refreshAddonSource(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "Invalid source id.")
		return
	}
	src, err := s.store.AddonSourceByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Add-on source not found.")
		return
	}
	if err != nil {
		s.engErr(w, err)
		return
	}
	b, err := fetchCatalogBytesOpts(r.Context(), src.URL, src.AllowPrivate)
	if err != nil {
		_ = s.store.UpdateAddonSourceFetch(r.Context(), src.ID, "", err.Error())
		writeErr(w, http.StatusBadGateway, "Refresh failed: "+err.Error())
		return
	}
	if _, err := parseAddonIndex(b); err != nil {
		_ = s.store.UpdateAddonSourceFetch(r.Context(), src.ID, "", err.Error())
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	_ = s.store.UpdateAddonSourceFetch(r.Context(), src.ID, string(b), "")
	updated, _ := s.store.AddonSourceByID(r.Context(), src.ID)
	writeJSON(w, http.StatusOK, updated)
}

// ---- YAML compose merging helpers ------------------------------------------------------

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
		delete(svc, "profiles")
		if disabled {
			svc["scale"] = 0
		} else {
			delete(svc, "scale")
		}
		if dep := asMap(svc["deploy"]); dep != nil {
			delete(dep, "replicas")
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
