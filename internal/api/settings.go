package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strings"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/store"
)

type diagnosticItem struct {
	Level  string `json:"level"` // warn | info
	ID     string `json:"id"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

type hostLockRow struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Kind              string `json:"kind"`
	Connected         bool   `json:"connected"`
	ReadOnly          bool   `json:"readOnly"`
	ManageExternal    bool   `json:"manageExternal"`
	ContainerTerminal bool   `json:"containerTerminal"`
	HostTerminal      bool   `json:"hostTerminal"`
	Exec              bool   `json:"exec"`
	SelfUpdate        bool   `json:"selfUpdate"`
}

type settingsConfigResp struct {
	PublicURL        string           `json:"publicUrl"`
	PublicURLRestart bool             `json:"publicUrlRestart"`
	StatusPublicURL  string           `json:"statusPublicUrl"`
	AgentPublicAddr  string           `json:"agentPublicAddr"`
	MetricsTokenSet  bool             `json:"metricsTokenSet"`
	MetricsToken     string           `json:"metricsToken,omitempty"`
	MetricsURL       string           `json:"metricsUrl"`
	UpdateChannel    string           `json:"updateChannel"`
	CleanupEnabled   bool             `json:"cleanupEnabled"`
	CleanupSchedule  string           `json:"cleanupSchedule"`
	CleanupVolumes   bool             `json:"cleanupVolumes"`
	FromEnv          map[string]bool  `json:"fromEnv"`
	EnvLocks         map[string]bool  `json:"envLocks"`
	HostLocks        []hostLockRow    `json:"hostLocks"`
	Diagnostics      []diagnosticItem `json:"diagnostics"`
}

func (s *Server) effectivePublicURL(ctx context.Context) string {
	if s.cfg.PublicURLSet {
		return strings.TrimRight(s.cfg.PublicURL, "/")
	}
	if s.store != nil {
		if v, err := s.store.GetSetting(ctx, store.SettingPublicURL); err == nil && v != "" {
			return strings.TrimRight(v, "/")
		}
	}
	return strings.TrimRight(s.cfg.PublicURL, "/")
}

func (s *Server) effectiveStatusPublicURL(ctx context.Context) string {
	if s.cfg.StatusPublicURL != "" {
		return strings.TrimRight(s.cfg.StatusPublicURL, "/")
	}
	if s.store != nil {
		if v, err := s.store.GetSetting(ctx, store.SettingStatusPublicURL); err == nil && v != "" {
			return strings.TrimRight(v, "/")
		}
	}
	return ""
}

func (s *Server) effectiveAgentPublicAddr(ctx context.Context) string {
	if s.cfg.AgentPublicAddr != "" {
		return s.cfg.AgentPublicAddr
	}
	if s.store != nil {
		if v, err := s.store.GetSetting(ctx, store.SettingAgentPublicAddr); err == nil && v != "" {
			return v
		}
	}
	return ""
}

func (s *Server) effectiveHostNetwork(ctx context.Context, hostID, envIP, envDomain string) (ip, domain string, ipEnv, domEnv bool) {
	if strings.TrimSpace(envIP) != "" {
		ip, ipEnv = strings.TrimSpace(envIP), true
	} else if s.store != nil {
		if saved, ok := s.savedHostAddresses(ctx, hostID); ok {
			for _, a := range mergeHostAddresses("", "", saved, true, nil) {
				if a.IsDefault {
					ip = a.Address
				}
			}
		} else {
			ip, _ = s.store.GetSetting(ctx, store.HostIPKey(hostID))
		}
	}
	if strings.TrimSpace(envDomain) != "" {
		domain, domEnv = strings.TrimSpace(envDomain), true
	} else if s.store != nil {
		domain, _ = s.store.GetSetting(ctx, store.BaseDomainKey(hostID))
	}
	return ip, domain, ipEnv, domEnv
}

// hostPortURLs builds http://<hostIP>:<port> links from published container port strings
// (e.g. "8080:80/tcp", "127.0.0.1:8080:80/tcp", "100.87.100.2:8090:8090/tcp"), skipping
// loopback-bound ports (127.0.0.1 / ::1) and UDP ports. A port bound to one address is
// linked on that address, since it isn't reachable on the host's default one.
func hostPortURLs(hostIP string, ports []string) []string {
	hostIP = strings.TrimSpace(hostIP)
	if hostIP == "" || len(ports) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, raw := range ports {
		b, ok := engine.ParsePortString(raw)
		if !ok {
			continue
		}
		addr := hostIP
		if b.IP != "" && !engine.IsLoopbackIP(b.IP) {
			addr = b.IP
		}
		u := portURL(addr, raw)
		if u != "" && !seen[u] && len(out) < 5 {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

func (s *Server) applyLivePublicURL(ctx context.Context, pubURL string) {
	pubURL = strings.TrimRight(pubURL, "/")
	s.cfg.PublicURL = pubURL
	if s.mon != nil {
		s.mon.SetPublicURL(pubURL)
	}
	if s.upd != nil {
		s.upd.SetPublicURL(pubURL)
	}
	if s.status != nil {
		s.status.SetPublicURL(pubURL)
	}
	if s.push != nil {
		s.push.SetPublicURL(pubURL)
	}
	if s.hub != nil {
		s.hub.SetPublicURL(pubURL)
	}
	s.reloadOIDC(ctx)
}

func (s *Server) effectiveMetricsToken(ctx context.Context) string {
	if s.cfg.MetricsToken != "" {
		return s.cfg.MetricsToken
	}
	if v, err := s.store.GetSetting(ctx, store.SettingMetricsToken); err == nil {
		return v
	}
	return ""
}

func (s *Server) effectiveUpdateChannel(ctx context.Context) string {
	if s.cfg.UpdateChannelSet {
		return s.cfg.UpdateChannel
	}
	if v, err := s.store.GetSetting(ctx, store.SettingUpdateChannel); err == nil && (v == "stable" || v == "pre") {
		return v
	}
	return s.cfg.UpdateChannel
}

func (s *Server) collectHostLocks(ctx context.Context) []hostLockRow {
	if s.reg == nil {
		return []hostLockRow{{
			ID: host.LocalID, Name: "Hub", Kind: "hub", Connected: true,
			ReadOnly: s.cfg.ReadOnly, ManageExternal: s.cfg.ManageExternal && !s.cfg.ReadOnly,
			ContainerTerminal: s.cfg.Terminal && !s.cfg.ReadOnly, HostTerminal: s.cfg.HostTerminal && !s.cfg.ReadOnly,
			Exec: (s.cfg.Exec || s.cfg.Terminal) && !s.cfg.ReadOnly, SelfUpdate: s.cfg.SelfUpdate,
		}}
	}
	hosts, _ := s.reg.List(ctx)
	out := make([]hostLockRow, 0, len(hosts))
	for _, h := range hosts {
		row := hostLockRow{
			ID:                h.ID,
			Name:              h.Name,
			Kind:              h.Kind,
			Connected:         h.Connected,
			ReadOnly:          h.ReadOnly,
			ContainerTerminal: h.Terminal && !h.ReadOnly,
			SelfUpdate:        s.cfg.SelfUpdate,
		}
		if h.Locks != nil {
			row.ReadOnly = h.Locks.ReadOnly
			row.ManageExternal = h.Locks.ManageExternal && !h.Locks.ReadOnly
			row.ContainerTerminal = h.Locks.Terminal && !h.Locks.ReadOnly
			row.HostTerminal = h.Locks.HostTerminal && !h.Locks.ReadOnly
			row.Exec = (h.Locks.Exec || h.Locks.Terminal) && !h.Locks.ReadOnly
			row.SelfUpdate = h.Locks.SelfUpdate
		}
		out = append(out, row)
	}
	return out
}

func (s *Server) getSettingsConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	all, err := s.store.AllSettings(ctx)
	if err != nil {
		s.engErr(w, err)
		return
	}
	pubURL := s.effectivePublicURL(ctx)
	statusPubURL := s.effectiveStatusPublicURL(ctx)
	agentPubAddr := s.effectiveAgentPublicAddr(ctx)
	tok := s.effectiveMetricsToken(ctx)
	ch := s.effectiveUpdateChannel(ctx)
	oidcCfg, _ := s.effectiveOIDCConfig(ctx)

	cleanupEnabled := all[store.SettingCleanupEnabled] == "true"
	cleanupSched := all[store.SettingCleanupSchedule]
	if cleanupSched == "" {
		cleanupSched = "03:30"
	}
	cleanupVolumes := all[store.SettingCleanupVolumes] == "true"

	metricsURL := "/metrics"
	if pubURL != "" {
		metricsURL = pubURL + "/metrics"
	}

	resp := settingsConfigResp{
		PublicURL:        pubURL,
		PublicURLRestart: false,
		StatusPublicURL:  statusPubURL,
		AgentPublicAddr:  agentPubAddr,
		MetricsTokenSet:  tok != "",
		MetricsURL:       metricsURL,
		UpdateChannel:    ch,
		CleanupEnabled:   cleanupEnabled,
		CleanupSchedule:  cleanupSched,
		CleanupVolumes:   cleanupVolumes,
		FromEnv: map[string]bool{
			"publicUrl":       s.cfg.PublicURLSet,
			"statusPublicUrl": s.cfg.StatusPublicURL != "",
			"agentPublicAddr": s.cfg.AgentPublicAddr != "",
			"metricsToken":    s.cfg.MetricsToken != "",
			"updateChannel":   s.cfg.UpdateChannelSet,
		},
		EnvLocks: map[string]bool{
			"readOnly":       s.cfg.ReadOnly,
			"manageExternal": s.cfg.ManageExternal && !s.cfg.ReadOnly,
			"terminal":       s.cfg.Terminal && !s.cfg.ReadOnly,
			"hostTerminal":   s.cfg.HostTerminal && !s.cfg.ReadOnly,
			"exec":           (s.cfg.Exec || s.cfg.Terminal) && !s.cfg.ReadOnly,
			"trustedProxies": len(s.cfg.Proxy.Trusted) > 0,
			"oidc":           oidcCfg.Configured(),
			"selfUpdate":     s.cfg.SelfUpdate,
		},
		HostLocks:   s.collectHostLocks(ctx),
		Diagnostics: s.buildDiagnostics(ctx, pubURL),
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) putSettingsConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PublicURL       *string `json:"publicUrl"`
		StatusPublicURL *string `json:"statusPublicUrl"`
		AgentPublicAddr *string `json:"agentPublicAddr"`
		MetricsToken    *string `json:"metricsToken"`
		UpdateChannel   *string `json:"updateChannel"`
		CleanupEnabled  *bool   `json:"cleanupEnabled"`
		CleanupSchedule *string `json:"cleanupSchedule"`
		CleanupVolumes  *bool   `json:"cleanupVolumes"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()

	if req.PublicURL != nil && !s.cfg.PublicURLSet {
		v := strings.TrimRight(strings.TrimSpace(*req.PublicURL), "/")
		if v != "" {
			u, err := url.Parse(v)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				writeErr(w, http.StatusBadRequest, "Public URL must be a valid http:// or https:// URL.")
				return
			}
		}
		if err := s.store.SetSetting(ctx, store.SettingPublicURL, v); err != nil {
			s.engErr(w, err)
			return
		}
		s.applyLivePublicURL(ctx, v)
	}

	if req.StatusPublicURL != nil && s.cfg.StatusPublicURL == "" {
		v := strings.TrimRight(strings.TrimSpace(*req.StatusPublicURL), "/")
		if v != "" {
			u, err := url.Parse(v)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				writeErr(w, http.StatusBadRequest, "Status Page URL must be a valid http:// or https:// URL.")
				return
			}
		}
		if err := s.store.SetSetting(ctx, store.SettingStatusPublicURL, v); err != nil {
			s.engErr(w, err)
			return
		}
		if s.status != nil {
			s.status.SetStatusPublicURL(v)
		}
	}

	if req.AgentPublicAddr != nil && s.cfg.AgentPublicAddr == "" {
		v := strings.TrimSpace(*req.AgentPublicAddr)
		if v != "" {
			if h, p, err := net.SplitHostPort(v); err != nil || h == "" || p == "" {
				writeErr(w, http.StatusBadRequest, "Agent Connect Address must be host:port (for example 100.87.100.2:5081).")
				return
			}
		}
		if err := s.store.SetSetting(ctx, store.SettingAgentPublicAddr, v); err != nil {
			s.engErr(w, err)
			return
		}
	}

	if req.MetricsToken != nil && s.cfg.MetricsToken == "" {
		v := strings.TrimSpace(*req.MetricsToken)
		if v != "" && len(v) < 16 {
			writeErr(w, http.StatusBadRequest, "Metrics token must be at least 16 characters (or empty to disable).")
			return
		}
		if err := s.store.SetSetting(ctx, store.SettingMetricsToken, v); err != nil {
			s.engErr(w, err)
			return
		}
	}

	if req.UpdateChannel != nil && !s.cfg.UpdateChannelSet {
		v := strings.ToLower(strings.TrimSpace(*req.UpdateChannel))
		if v != "stable" && v != "pre" {
			writeErr(w, http.StatusBadRequest, "Update channel must be stable or pre.")
			return
		}
		if err := s.store.SetSetting(ctx, store.SettingUpdateChannel, v); err != nil {
			s.engErr(w, err)
			return
		}
		if s.sysUpd != nil && s.sysUpd.Checker != nil {
			s.sysUpd.Checker.SetChannel(v)
		}
	}

	if req.CleanupEnabled != nil {
		v := "false"
		if *req.CleanupEnabled {
			v = "true"
		}
		if err := s.store.SetSetting(ctx, store.SettingCleanupEnabled, v); err != nil {
			s.engErr(w, err)
			return
		}
	}
	if req.CleanupSchedule != nil {
		v := strings.TrimSpace(*req.CleanupSchedule)
		if len(v) != 5 || v[2] != ':' {
			writeErr(w, http.StatusBadRequest, "Cleanup schedule must be HH:MM.")
			return
		}
		if err := s.store.SetSetting(ctx, store.SettingCleanupSchedule, v); err != nil {
			s.engErr(w, err)
			return
		}
	}
	if req.CleanupVolumes != nil {
		v := "false"
		if *req.CleanupVolumes {
			v = "true"
		}
		if err := s.store.SetSetting(ctx, store.SettingCleanupVolumes, v); err != nil {
			s.engErr(w, err)
			return
		}
	}

	setAuditTarget(r, "config")
	s.getSettingsConfig(w, r)
}

func (s *Server) generateMetricsToken(w http.ResponseWriter, r *http.Request) {
	if s.cfg.MetricsToken != "" {
		writeErr(w, http.StatusConflict, "MOORAGE_METRICS_TOKEN is set in environment.")
		return
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		s.engErr(w, err)
		return
	}
	tok := hex.EncodeToString(b)
	if err := s.store.SetSetting(r.Context(), store.SettingMetricsToken, tok); err != nil {
		s.engErr(w, err)
		return
	}
	setAuditTarget(r, "metrics-token")
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}

func (s *Server) buildDiagnostics(ctx context.Context, pubURL string) []diagnosticItem {
	out := []diagnosticItem{}
	if pubURL == "" {
		out = append(out, diagnosticItem{
			Level:  "warn",
			ID:     "missing_public_url",
			Title:  "Public URL not configured",
			Detail: "Alert links, invite URLs and RSS feeds will not have an absolute URL until Public URL is set.",
		})
	} else if strings.HasPrefix(pubURL, "https://") && s.cfg.SecureCookiesSet && !s.cfg.SecureCookies {
		out = append(out, diagnosticItem{
			Level:  "warn",
			ID:     "https_no_secure_cookies",
			Title:  "MOORAGE_SECURE_COOKIES is false over HTTPS",
			Detail: "Public URL uses HTTPS, but MOORAGE_SECURE_COOKIES=false was explicitly forced in environment.",
		})
	}
	if st := s.getOIDC().Status(); st.Configured && st.LastError != "" {
		out = append(out, diagnosticItem{
			Level:  "warn",
			ID:     "oidc_error",
			Title:  "OIDC configuration error",
			Detail: st.LastError,
		})
	}
	if s.hub != nil {
		if wd, err := s.store.Watchdog(ctx); err == nil && wd.Enabled {
			agents, _ := s.store.Agents(ctx)
			var participating int
			var missingAddr []string
			for _, a := range agents {
				if a.Watchdog {
					participating++
					if a.WatchdogAddr == "" {
						missingAddr = append(missingAddr, a.Name)
					}
				}
			}
			if participating >= 2 && len(missingAddr) > 0 {
				out = append(out, diagnosticItem{
					Level:  "warn",
					ID:     "watchdog_missing_addr",
					Title:  "Watchdog peer address missing",
					Detail: "Set MOORAGE_WATCHDOG_ADDR on agents so they can reach each other: " + strings.Join(missingAddr, ", "),
				})
			}
		}
	}
	return out
}
