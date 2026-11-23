package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"

	"moorage/internal/store"
)

type diagnosticItem struct {
	Level  string `json:"level"` // warn | info
	ID     string `json:"id"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

type settingsConfigResp struct {
	PublicURL       string           `json:"publicUrl"`
	MetricsTokenSet bool             `json:"metricsTokenSet"`
	MetricsToken    string           `json:"metricsToken,omitempty"`
	MetricsURL      string           `json:"metricsUrl"`
	UpdateChannel   string           `json:"updateChannel"`
	CleanupEnabled  bool             `json:"cleanupEnabled"`
	CleanupSchedule string           `json:"cleanupSchedule"`
	CleanupVolumes  bool             `json:"cleanupVolumes"`
	FromEnv         map[string]bool  `json:"fromEnv"`
	EnvLocks        map[string]bool  `json:"envLocks"`
	Diagnostics     []diagnosticItem `json:"diagnostics"`
}

func (s *Server) effectivePublicURL(ctx context.Context) string {
	if s.cfg.PublicURL != "" {
		return strings.TrimRight(s.cfg.PublicURL, "/")
	}
	if v, err := s.store.GetSetting(ctx, store.SettingPublicURL); err == nil && v != "" {
		return strings.TrimRight(v, "/")
	}
	return ""
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

func (s *Server) getSettingsConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	all, err := s.store.AllSettings(ctx)
	if err != nil {
		s.engErr(w, err)
		return
	}
	pubURL := s.effectivePublicURL(ctx)
	tok := s.effectiveMetricsToken(ctx)
	ch := s.effectiveUpdateChannel(ctx)

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
		PublicURL:       pubURL,
		MetricsTokenSet: tok != "",
		MetricsURL:      metricsURL,
		UpdateChannel:   ch,
		CleanupEnabled:  cleanupEnabled,
		CleanupSchedule: cleanupSched,
		CleanupVolumes:  cleanupVolumes,
		FromEnv: map[string]bool{
			"publicUrl":     s.cfg.PublicURL != "",
			"metricsToken":  s.cfg.MetricsToken != "",
			"updateChannel": s.cfg.UpdateChannelSet,
		},
		EnvLocks: map[string]bool{
			"readOnly":       s.cfg.ReadOnly,
			"terminal":       s.cfg.Terminal,
			"trustedProxies": len(s.cfg.Proxy.Trusted) > 0,
			"oidc":           s.cfg.OIDC.Configured(),
			"selfUpdate":     s.cfg.SelfUpdate,
		},
		Diagnostics: s.buildDiagnostics(ctx, pubURL),
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) putSettingsConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PublicURL       *string `json:"publicUrl"`
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

	if req.PublicURL != nil && s.cfg.PublicURL == "" {
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
	var out []diagnosticItem
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
	if s.cfg.OIDC.Err != "" {
		out = append(out, diagnosticItem{
			Level:  "warn",
			ID:     "oidc_error",
			Title:  "OIDC configuration error",
			Detail: s.cfg.OIDC.Err,
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
