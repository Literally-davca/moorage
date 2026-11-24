package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"moorage/internal/config"
	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/netguard"
	"moorage/internal/sso"
	"moorage/internal/store"
)

// ---- stored connection configs (Env > DB) ------------------------------------

type storedOIDC struct {
	Issuer       string   `json:"issuer"`
	ClientID     string   `json:"clientId"`
	ClientSecret string   `json:"clientSecret"`
	Scopes       []string `json:"scopes"`
	GroupsClaim  string   `json:"groupsClaim"`
	AdminGroups  []string `json:"adminGroups"`
	ViewerGroups []string `json:"viewerGroups"`
	DefaultRole  string   `json:"defaultRole"`
	Label        string   `json:"label"`
	LocalLogin   bool     `json:"localLogin"`
}

type coolifyHostConfig struct {
	HostID       string `json:"hostId"`
	URL          string `json:"url"`
	Token        string `json:"token,omitempty"`
	TokenSet     bool   `json:"tokenSet"`
	AllowPrivate bool   `json:"allowPrivate"`
}

type storedCoolify struct {
	URL          string              `json:"url"`
	Token        string              `json:"token,omitempty"`
	AllowPrivate bool                `json:"allowPrivate"`
	Hosts        []coolifyHostConfig `json:"hosts,omitempty"`
}

type registryEntry struct {
	Server   string `json:"server"`
	Username string `json:"username"`
	Password string `json:"password,omitempty"`
}

type registryEntryDTO struct {
	Server      string `json:"server"`
	Username    string `json:"username"`
	PasswordSet bool   `json:"passwordSet"`
	FromEnv     bool   `json:"fromEnv,omitempty"`
}

type sisterLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type storedSisterPanels struct {
	GrafanaURL          string       `json:"grafanaUrl"`
	GrafanaToken        string       `json:"grafanaToken,omitempty"`
	GrafanaFolderUID    string       `json:"grafanaFolderUid,omitempty"`
	GrafanaAllowPrivate bool         `json:"grafanaAllowPrivate,omitempty"`
	BeszelURL           string       `json:"beszelUrl"`
	Links               []sisterLink `json:"links,omitempty"`
}

type oidcConnDTO struct {
	Issuer          string   `json:"issuer"`
	ClientID        string   `json:"clientId"`
	ClientSecretSet bool     `json:"clientSecretSet"`
	Scopes          []string `json:"scopes"`
	GroupsClaim     string   `json:"groupsClaim"`
	AdminGroups     []string `json:"adminGroups"`
	ViewerGroups    []string `json:"viewerGroups"`
	DefaultRole     string   `json:"defaultRole"`
	Label           string   `json:"label"`
	LocalLogin      bool     `json:"localLogin"`
	FromEnv         bool     `json:"fromEnv"`
	Ready           bool     `json:"ready"`
	RedirectURI     string   `json:"redirectUri,omitempty"`
	LastError       string   `json:"lastError,omitempty"`
}

type coolifyConnDTO struct {
	URL          string              `json:"url"`
	TokenSet     bool                `json:"tokenSet"`
	AllowPrivate bool                `json:"allowPrivate"`
	FromEnv      bool                `json:"fromEnv"`
	Hosts        []coolifyHostConfig `json:"hosts"`
}

type sisterPanelsDTO struct {
	GrafanaURL          string            `json:"grafanaUrl"`
	GrafanaTokenSet     bool              `json:"grafanaTokenSet"`
	GrafanaFolderUID    string            `json:"grafanaFolderUid,omitempty"`
	GrafanaAllowPrivate bool              `json:"grafanaAllowPrivate"`
	BeszelURL           string            `json:"beszelUrl"`
	Links               []sisterLink      `json:"links"`
	FromEnv             map[string]bool   `json:"fromEnv"`
}

type connectionsResp struct {
	OIDC       oidcConnDTO        `json:"oidc"`
	Coolify    coolifyConnDTO     `json:"coolify"`
	Registries []registryEntryDTO `json:"registries"`
	Panels     sisterPanelsDTO    `json:"panels"`
}

func (s *Server) getOIDC() *sso.OIDC {
	s.oidcMu.RLock()
	defer s.oidcMu.RUnlock()
	return s.oidc
}

func (s *Server) effectiveOIDCConfig(ctx context.Context) (config.OIDCConfig, bool) {
	if s.cfg.OIDC.Configured() {
		return s.cfg.OIDC, true
	}
	if s.store == nil {
		return s.cfg.OIDC, false
	}
	raw, _ := s.store.GetSetting(ctx, store.SettingOIDCJSON)
	if raw == "" {
		return s.cfg.OIDC, false
	}
	var st storedOIDC
	if json.Unmarshal([]byte(raw), &st) != nil {
		return s.cfg.OIDC, false
	}
	scopes := st.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "profile", "email", "groups"}
	}
	gc := strings.TrimSpace(st.GroupsClaim)
	if gc == "" {
		gc = "groups"
	}
	lbl := strings.TrimSpace(st.Label)
	if lbl == "" {
		lbl = "Sign in with SSO"
	}
	defRole := strings.ToLower(strings.TrimSpace(st.DefaultRole))
	if defRole != "viewer" {
		defRole = "none"
	}
	return config.OIDCConfig{
		Issuer:       strings.TrimSpace(st.Issuer),
		ClientID:     strings.TrimSpace(st.ClientID),
		ClientSecret: st.ClientSecret,
		Scopes:       scopes,
		GroupsClaim:  gc,
		Roles: config.RoleMapping{
			AdminGroups:  st.AdminGroups,
			ViewerGroups: st.ViewerGroups,
			DefaultRole:  defRole,
		},
		Label:      lbl,
		LocalLogin: st.LocalLogin,
	}, false
}

func (s *Server) reloadOIDC(ctx context.Context) {
	cfg, _ := s.effectiveOIDCConfig(ctx)
	pubURL := s.effectivePublicURL(ctx)
	next := sso.NewOIDC(cfg, pubURL, s.log, sso.Options{})
	s.oidcMu.Lock()
	s.oidc = next
	s.oidcMu.Unlock()
	bg := s.termCfg.HubCtx
	if bg == nil {
		bg = context.Background()
	}
	next.Start(bg)
}

func (s *Server) effectiveCoolifyConfig(ctx context.Context) (storedCoolify, bool) {
	var st storedCoolify
	if s.store != nil {
		if raw, _ := s.store.GetSetting(ctx, store.SettingCoolifyJSON); raw != "" {
			_ = json.Unmarshal([]byte(raw), &st)
		}
	}
	envURL := strings.TrimRight(strings.TrimSpace(s.cfg.Env["MOORAGE_COOLIFY_URL"]), "/")
	envTok := strings.TrimSpace(s.cfg.Env["MOORAGE_COOLIFY_TOKEN"])
	fromEnv := envURL != "" || envTok != ""
	if envURL != "" {
		st.URL = envURL
	}
	if envTok != "" {
		st.Token = envTok
	}
	if strings.EqualFold(strings.TrimSpace(s.cfg.Env["MOORAGE_COOLIFY_ALLOW_PRIVATE"]), "true") {
		st.AllowPrivate = true
	}
	return st, fromEnv
}

// EffectiveCoolifyForHost resolves Coolify URL, token and allowPrivate for a specific host.
func (s *Server) EffectiveCoolifyForHost(ctx context.Context, hostID string) (baseURL, token string, allowPrivate bool) {
	st, _ := s.effectiveCoolifyConfig(ctx)
	baseURL, token, allowPrivate = strings.TrimRight(st.URL, "/"), st.Token, st.AllowPrivate
	for _, hc := range st.Hosts {
		if hc.HostID == hostID && strings.TrimSpace(hc.URL) != "" {
			baseURL = strings.TrimRight(strings.TrimSpace(hc.URL), "/")
			if hc.Token != "" {
				token = hc.Token
			}
			allowPrivate = hc.AllowPrivate
			break
		}
	}
	return baseURL, token, allowPrivate
}

func (s *Server) effectiveRegistries(ctx context.Context) ([]registryEntry, map[string]bool) {
	fromEnv := map[string]bool{}
	byServer := map[string]registryEntry{}
	var order []string
	if s.store != nil {
		if raw, _ := s.store.GetSetting(ctx, store.SettingRegistriesJSON); raw != "" {
			var list []registryEntry
			if json.Unmarshal([]byte(raw), &list) == nil {
				for _, r := range list {
					srv := normalizeRegistryServer(r.Server)
					if srv == "" {
						continue
					}
					r.Server = srv
					if _, exists := byServer[srv]; !exists {
						order = append(order, srv)
					}
					byServer[srv] = r
				}
			}
		}
	}
	if envRaw := strings.TrimSpace(s.cfg.Env["MOORAGE_REGISTRIES"]); envRaw != "" {
		var envList []registryEntry
		if json.Unmarshal([]byte(envRaw), &envList) == nil {
			for _, r := range envList {
				srv := normalizeRegistryServer(r.Server)
				if srv == "" {
					continue
				}
				r.Server = srv
				if _, exists := byServer[srv]; !exists {
					order = append(order, srv)
				}
				byServer[srv] = r
				fromEnv[srv] = true
			}
		}
	}
	out := make([]registryEntry, 0, len(order))
	for _, srv := range order {
		out = append(out, byServer[srv])
	}
	return out, fromEnv
}

func normalizeRegistryServer(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	v = strings.TrimPrefix(v, "https://")
	v = strings.TrimPrefix(v, "http://")
	v = strings.TrimRight(v, "/")
	if v == "index.docker.io" || v == "registry-1.docker.io" || v == "docker.io/v1" || v == "index.docker.io/v1" {
		return "docker.io"
	}
	return v
}

// SyncRegistries pushes configured private registry credentials to the local engine and
// every connected agent over mTLS.
func (s *Server) SyncRegistries(ctx context.Context) {
	if s.reg == nil {
		return
	}
	regs, _ := s.effectiveRegistries(ctx)
	logins := make([]engine.RegistryLogin, 0, len(regs))
	for _, r := range regs {
		logins = append(logins, engine.RegistryLogin{Server: r.Server, Username: r.Username, Password: r.Password})
	}
	hosts, _ := s.reg.List(ctx)
	for _, meta := range hosts {
		if !meta.Connected {
			continue
		}
		h, _, err := s.reg.Get(ctx, meta.ID)
		if err != nil {
			continue
		}
		if rr, ok := h.(host.RegistryReceiver); ok {
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_ = rr.SetRegistries(pctx, logins)
			cancel()
		}
	}
}

// SyncRegistriesToHost pushes configured private registry credentials to one newly connected agent.
func (s *Server) SyncRegistriesToHost(hostID string) {
	if s.reg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	regs, _ := s.effectiveRegistries(ctx)
	if len(regs) == 0 {
		return
	}
	logins := make([]engine.RegistryLogin, 0, len(regs))
	for _, r := range regs {
		logins = append(logins, engine.RegistryLogin{Server: r.Server, Username: r.Username, Password: r.Password})
	}
	h, _, err := s.reg.Get(ctx, hostID)
	if err != nil {
		return
	}
	if rr, ok := h.(host.RegistryReceiver); ok {
		_ = rr.SetRegistries(ctx, logins)
	}
}

func (s *Server) effectiveSisterPanels(ctx context.Context) (storedSisterPanels, map[string]bool) {
	var st storedSisterPanels
	if s.store != nil {
		if raw, _ := s.store.GetSetting(ctx, store.SettingSisterLinksJSON); raw != "" {
			_ = json.Unmarshal([]byte(raw), &st)
		}
	}
	fromEnv := map[string]bool{}
	if v := strings.TrimRight(strings.TrimSpace(s.cfg.Env["MOORAGE_GRAFANA_URL"]), "/"); v != "" {
		st.GrafanaURL = v
		fromEnv["grafanaUrl"] = true
	}
	if v := strings.TrimSpace(s.cfg.Env["MOORAGE_GRAFANA_TOKEN"]); v != "" {
		st.GrafanaToken = v
		fromEnv["grafanaToken"] = true
	}
	if v := strings.TrimSpace(s.cfg.Env["MOORAGE_GRAFANA_FOLDER_UID"]); v != "" {
		st.GrafanaFolderUID = v
	}
	if strings.EqualFold(strings.TrimSpace(s.cfg.Env["MOORAGE_GRAFANA_ALLOW_PRIVATE"]), "true") {
		st.GrafanaAllowPrivate = true
	}
	if v := strings.TrimRight(strings.TrimSpace(s.cfg.Env["MOORAGE_BESZEL_URL"]), "/"); v != "" {
		st.BeszelURL = v
		fromEnv["beszelUrl"] = true
	}
	if st.Links == nil {
		st.Links = []sisterLink{}
	}
	return st, fromEnv
}

// sisterLinks returns non-secret external panel links for the UI (Hosts, Monitoring, Ctrl+K).
func (s *Server) sisterLinks(w http.ResponseWriter, r *http.Request) {
	st, _ := s.effectiveSisterPanels(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"grafanaUrl": st.GrafanaURL,
		"beszelUrl":  st.BeszelURL,
		"links":      st.Links,
	})
}

func (s *Server) getConnections(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	oidcCfg, oidcEnv := s.effectiveOIDCConfig(ctx)
	oidcSt := s.getOIDC().Status()
	scopes := oidcCfg.Scopes
	if scopes == nil {
		scopes = []string{"openid", "profile", "email", "groups"}
	}
	adminGroups := oidcCfg.Roles.AdminGroups
	if adminGroups == nil {
		adminGroups = []string{}
	}
	viewerGroups := oidcCfg.Roles.ViewerGroups
	if viewerGroups == nil {
		viewerGroups = []string{}
	}
	defRole := oidcCfg.Roles.DefaultRole
	if defRole == "" {
		defRole = "none"
	}

	coolCfg, coolEnv := s.effectiveCoolifyConfig(ctx)
	maskedHosts := make([]coolifyHostConfig, 0, len(coolCfg.Hosts))
	for _, hc := range coolCfg.Hosts {
		maskedHosts = append(maskedHosts, coolifyHostConfig{
			HostID:       hc.HostID,
			URL:          hc.URL,
			TokenSet:     hc.Token != "",
			AllowPrivate: hc.AllowPrivate,
		})
	}

	regs, regEnv := s.effectiveRegistries(ctx)
	regDTOs := make([]registryEntryDTO, 0, len(regs))
	for _, rg := range regs {
		regDTOs = append(regDTOs, registryEntryDTO{
			Server:      rg.Server,
			Username:    rg.Username,
			PasswordSet: rg.Password != "",
			FromEnv:     regEnv[rg.Server],
		})
	}

	panels, panelEnv := s.effectiveSisterPanels(ctx)

	writeJSON(w, http.StatusOK, connectionsResp{
		OIDC: oidcConnDTO{
			Issuer:          oidcCfg.Issuer,
			ClientID:        oidcCfg.ClientID,
			ClientSecretSet: oidcCfg.ClientSecret != "",
			Scopes:          scopes,
			GroupsClaim:     oidcCfg.GroupsClaim,
			AdminGroups:     adminGroups,
			ViewerGroups:    viewerGroups,
			DefaultRole:     defRole,
			Label:           oidcCfg.Label,
			LocalLogin:      oidcCfg.LocalLogin,
			FromEnv:         oidcEnv,
			Ready:           oidcSt.Ready,
			RedirectURI:     oidcSt.RedirectURI,
			LastError:       oidcSt.LastError,
		},
		Coolify: coolifyConnDTO{
			URL:          coolCfg.URL,
			TokenSet:     coolCfg.Token != "",
			AllowPrivate: coolCfg.AllowPrivate,
			FromEnv:      coolEnv,
			Hosts:        maskedHosts,
		},
		Registries: regDTOs,
		Panels: sisterPanelsDTO{
			GrafanaURL:          panels.GrafanaURL,
			GrafanaTokenSet:     panels.GrafanaToken != "",
			GrafanaFolderUID:    panels.GrafanaFolderUID,
			GrafanaAllowPrivate: panels.GrafanaAllowPrivate,
			BeszelURL:           panels.BeszelURL,
			Links:               panels.Links,
			FromEnv:             panelEnv,
		},
	})
}

func (s *Server) putConnections(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OIDC *struct {
			Issuer            string   `json:"issuer"`
			ClientID          string   `json:"clientId"`
			ClientSecret      string   `json:"clientSecret"`
			ClearClientSecret bool     `json:"clearClientSecret"`
			Scopes            []string `json:"scopes"`
			GroupsClaim       string   `json:"groupsClaim"`
			AdminGroups       []string `json:"adminGroups"`
			ViewerGroups      []string `json:"viewerGroups"`
			DefaultRole       string   `json:"defaultRole"`
			Label             string   `json:"label"`
			LocalLogin        bool     `json:"localLogin"`
		} `json:"oidc"`
		Coolify *struct {
			URL          string `json:"url"`
			Token        string `json:"token"`
			ClearToken   bool   `json:"clearToken"`
			AllowPrivate bool   `json:"allowPrivate"`
			Hosts        []struct {
				HostID       string `json:"hostId"`
				URL          string `json:"url"`
				Token        string `json:"token"`
				ClearToken   bool   `json:"clearToken"`
				AllowPrivate bool   `json:"allowPrivate"`
			} `json:"hosts"`
		} `json:"coolify"`
		Registries *[]struct {
			Server   string `json:"server"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"registries"`
		Panels *struct {
			GrafanaURL          string       `json:"grafanaUrl"`
			GrafanaToken        string       `json:"grafanaToken"`
			ClearGrafanaToken   bool         `json:"clearGrafanaToken"`
			GrafanaFolderUID    string       `json:"grafanaFolderUid"`
			GrafanaAllowPrivate bool         `json:"grafanaAllowPrivate"`
			BeszelURL           string       `json:"beszelUrl"`
			Links               []sisterLink `json:"links"`
		} `json:"panels"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()

	if req.OIDC != nil && !s.cfg.OIDC.Configured() {
		curCfg, _ := s.effectiveOIDCConfig(ctx)
		sec := curCfg.ClientSecret
		if req.OIDC.ClearClientSecret {
			sec = ""
		} else if req.OIDC.ClientSecret != "" {
			sec = req.OIDC.ClientSecret
		}
		issuer := strings.TrimSpace(req.OIDC.Issuer)
		clientID := strings.TrimSpace(req.OIDC.ClientID)
		if issuer == "" && clientID == "" {
			_ = s.store.SetSetting(ctx, store.SettingOIDCJSON, "")
		} else {
			if issuer != "" {
				if err := checkHTTPURL(issuer, false); err != nil {
					writeErr(w, http.StatusBadRequest, "OIDC Issuer: "+err.Error())
					return
				}
			}
			defRole := strings.ToLower(strings.TrimSpace(req.OIDC.DefaultRole))
			if defRole != "none" && defRole != "viewer" {
				writeErr(w, http.StatusBadRequest, "OIDC default role must be none or viewer.")
				return
			}
			b, _ := json.Marshal(storedOIDC{
				Issuer:       issuer,
				ClientID:     clientID,
				ClientSecret: sec,
				Scopes:       req.OIDC.Scopes,
				GroupsClaim:  strings.TrimSpace(req.OIDC.GroupsClaim),
				AdminGroups:  req.OIDC.AdminGroups,
				ViewerGroups: req.OIDC.ViewerGroups,
				DefaultRole:  defRole,
				Label:        strings.TrimSpace(req.OIDC.Label),
				LocalLogin:   req.OIDC.LocalLogin,
			})
			if err := s.store.SetSetting(ctx, store.SettingOIDCJSON, string(b)); err != nil {
				s.engErr(w, err)
				return
			}
		}
		s.reloadOIDC(ctx)
	}

	if req.Coolify != nil {
		cur, fromEnv := s.effectiveCoolifyConfig(ctx)
		oldByHost := map[string]string{}
		for _, hc := range cur.Hosts {
			oldByHost[hc.HostID] = hc.Token
		}
		next := cur
		if !fromEnv {
			u := strings.TrimRight(strings.TrimSpace(req.Coolify.URL), "/")
			if u != "" {
				if err := checkHTTPURL(u, true); err != nil {
					writeErr(w, http.StatusBadRequest, "Coolify URL: "+err.Error())
					return
				}
			}
			next.URL = u
			if req.Coolify.ClearToken {
				next.Token = ""
			} else if req.Coolify.Token != "" {
				next.Token = strings.TrimSpace(req.Coolify.Token)
			}
			next.AllowPrivate = req.Coolify.AllowPrivate
		}
		next.Hosts = nil
		for _, h := range req.Coolify.Hosts {
			hid := strings.TrimSpace(h.HostID)
			u := strings.TrimRight(strings.TrimSpace(h.URL), "/")
			if hid == "" || u == "" {
				continue
			}
			if err := checkHTTPURL(u, true); err != nil {
				writeErr(w, http.StatusBadRequest, "Coolify host URL: "+err.Error())
				return
			}
			tok := oldByHost[hid]
			if h.ClearToken {
				tok = ""
			} else if h.Token != "" {
				tok = strings.TrimSpace(h.Token)
			}
			next.Hosts = append(next.Hosts, coolifyHostConfig{
				HostID: hid, URL: u, Token: tok, AllowPrivate: h.AllowPrivate,
			})
		}
		b, _ := json.Marshal(next)
		if err := s.store.SetSetting(ctx, store.SettingCoolifyJSON, string(b)); err != nil {
			s.engErr(w, err)
			return
		}
	}

	if req.Registries != nil {
		curRegs, envMap := s.effectiveRegistries(ctx)
		oldPw := map[string]string{}
		for _, cr := range curRegs {
			if !envMap[cr.Server] {
				oldPw[cr.Server] = cr.Password
			}
		}
		var saved []registryEntry
		for _, in := range *req.Registries {
			srv := normalizeRegistryServer(in.Server)
			if srv == "" || envMap[srv] {
				continue
			}
			pw := in.Password
			if pw == "" {
				pw = oldPw[srv]
			}
			saved = append(saved, registryEntry{
				Server:   srv,
				Username: strings.TrimSpace(in.Username),
				Password: pw,
			})
		}
		val := ""
		if len(saved) > 0 {
			b, _ := json.Marshal(saved)
			val = string(b)
		}
		if err := s.store.SetSetting(ctx, store.SettingRegistriesJSON, val); err != nil {
			s.engErr(w, err)
			return
		}
		go s.SyncRegistries(context.Background())
	}

	if req.Panels != nil {
		cur, fromEnv := s.effectiveSisterPanels(ctx)
		next := cur
		if !fromEnv["grafanaUrl"] {
			u := strings.TrimRight(strings.TrimSpace(req.Panels.GrafanaURL), "/")
			if u != "" {
				if err := checkHTTPURL(u, true); err != nil {
					writeErr(w, http.StatusBadRequest, "Grafana URL: "+err.Error())
					return
				}
			}
			next.GrafanaURL = u
		}
		if !fromEnv["grafanaToken"] {
			if req.Panels.ClearGrafanaToken {
				next.GrafanaToken = ""
			} else if req.Panels.GrafanaToken != "" {
				next.GrafanaToken = strings.TrimSpace(req.Panels.GrafanaToken)
			}
		}
		next.GrafanaFolderUID = strings.TrimSpace(req.Panels.GrafanaFolderUID)
		next.GrafanaAllowPrivate = req.Panels.GrafanaAllowPrivate
		if !fromEnv["beszelUrl"] {
			u := strings.TrimRight(strings.TrimSpace(req.Panels.BeszelURL), "/")
			if u != "" {
				if err := checkHTTPURL(u, true); err != nil {
					writeErr(w, http.StatusBadRequest, "Beszel URL: "+err.Error())
					return
				}
			}
			next.BeszelURL = u
		}
		var links []sisterLink
		for _, lk := range req.Panels.Links {
			lbl := strings.TrimSpace(lk.Label)
			u := strings.TrimSpace(lk.URL)
			if lbl == "" || u == "" {
				continue
			}
			if err := checkHTTPURL(u, true); err != nil {
				writeErr(w, http.StatusBadRequest, fmt.Sprintf("Link %q: %v", lbl, err))
				return
			}
			links = append(links, sisterLink{Label: lbl, URL: u})
		}
		next.Links = links
		b, _ := json.Marshal(next)
		if err := s.store.SetSetting(ctx, store.SettingSisterLinksJSON, string(b)); err != nil {
			s.engErr(w, err)
			return
		}
	}

	setAuditTarget(r, "connections")
	s.getConnections(w, r)
}

func checkHTTPURL(raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("must be a valid URL")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && (allowHTTP || u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1") {
		return nil
	}
	return errors.New("must use https://")
}

// ---- connection tests (auditSkip: dry-run checks only) -----------------------

func (s *Server) testOIDCConnection(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Issuer       string   `json:"issuer"`
		ClientID     string   `json:"clientId"`
		ClientSecret string   `json:"clientSecret"`
		Scopes       []string `json:"scopes"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	cur, _ := s.effectiveOIDCConfig(ctx)
	sec := req.ClientSecret
	if sec == "" {
		sec = cur.ClientSecret
	}
	scopes := req.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "profile", "email", "groups"}
	}
	pubURL := s.effectivePublicURL(ctx)
	if pubURL == "" {
		pubURL = "https://moorage.example.com"
	}
	tmp := sso.NewOIDC(config.OIDCConfig{
		Issuer:       strings.TrimSpace(req.Issuer),
		ClientID:     strings.TrimSpace(req.ClientID),
		ClientSecret: sec,
		Scopes:       scopes,
		GroupsClaim:  "groups",
		Roles:        config.RoleMapping{DefaultRole: "none"},
		LocalLogin:   true,
	}, pubURL, s.log, sso.Options{})
	if err := tmp.Discover(ctx); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	st := tmp.Status()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "redirectUri": st.RedirectURI})
}

func guardedClient(timeout time.Duration, allowPrivate bool) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: netguard.DialContext(10*time.Second, allowPrivate),
		},
	}
}

func (s *Server) testCoolifyConnection(w http.ResponseWriter, r *http.Request) {
	var req struct {
		HostID       string `json:"hostId"`
		URL          string `json:"url"`
		Token        string `json:"token"`
		AllowPrivate bool   `json:"allowPrivate"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	baseURL := strings.TrimRight(strings.TrimSpace(req.URL), "/")
	token := strings.TrimSpace(req.Token)
	if token == "" {
		_, savedTok, _ := s.EffectiveCoolifyForHost(r.Context(), req.HostID)
		token = savedTok
	}
	if baseURL == "" || token == "" {
		writeErr(w, http.StatusBadRequest, "Enter both the Coolify URL and API token.")
		return
	}
	if err := checkHTTPURL(baseURL, true); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	c := guardedClient(12*time.Second, req.AllowPrivate)
	defer c.CloseIdleConnections()

	hr, err := http.NewRequestWithContext(r.Context(), http.MethodGet, baseURL+"/api/v1/version", nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	hr.Header.Set("Authorization", "Bearer "+token)
	hr.Header.Set("Accept", "application/json")
	resp, err := c.Do(hr)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Couldn't reach Coolify: "+err.Error())
		return
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("Coolify answered HTTP %d.", resp.StatusCode))
		return
	}
	ver := strings.Trim(strings.TrimSpace(string(body)), `"`)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": ver})
}

func (s *Server) testRegistryConnection(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Server       string `json:"server"`
		Username     string `json:"username"`
		Password     string `json:"password"`
		AllowPrivate bool   `json:"allowPrivate"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	srv := normalizeRegistryServer(req.Server)
	if srv == "" {
		writeErr(w, http.StatusBadRequest, "Registry server is required.")
		return
	}
	pw := req.Password
	if pw == "" {
		regs, _ := s.effectiveRegistries(r.Context())
		for _, rg := range regs {
			if rg.Server == srv {
				pw = rg.Password
				break
			}
		}
	}
	apiHost := srv
	if srv == "docker.io" {
		apiHost = "registry-1.docker.io"
	}
	c := guardedClient(12*time.Second, req.AllowPrivate)
	defer c.CloseIdleConnections()

	v2URL := "https://" + apiHost + "/v2/"
	hr, err := http.NewRequestWithContext(r.Context(), http.MethodGet, v2URL, nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Username != "" || pw != "" {
		hr.SetBasicAuth(req.Username, pw)
	}
	resp, err := c.Do(hr)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Registry unreachable: "+err.Error())
		return
	}
	wwwAuth := resp.Header.Get("Www-Authenticate")
	resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if resp.StatusCode == http.StatusUnauthorized && strings.HasPrefix(strings.ToLower(wwwAuth), "bearer ") {
		tok, err := exchangeRegistryBearer(r.Context(), c, wwwAuth, req.Username, pw)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "Registry authentication failed: "+err.Error())
			return
		}
		hr2, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, v2URL, nil)
		hr2.Header.Set("Authorization", "Bearer "+tok)
		resp2, err := c.Do(hr2)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		resp2.Body.Close()
		if resp2.StatusCode == http.StatusOK {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("Registry answered HTTP %d after token exchange.", resp2.StatusCode))
		return
	}
	writeErr(w, http.StatusBadGateway, fmt.Sprintf("Registry answered HTTP %d.", resp.StatusCode))
}

func exchangeRegistryBearer(ctx context.Context, c *http.Client, header, username, password string) (string, error) {
	params := map[string]string{}
	rest := strings.TrimSpace(header[len("Bearer "):])
	for _, part := range strings.Split(rest, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		params[strings.ToLower(k)] = strings.Trim(v, `"`)
	}
	realm := params["realm"]
	if realm == "" {
		return "", errors.New("missing bearer realm")
	}
	u, err := url.Parse(realm)
	if err != nil || u.Scheme != "https" {
		return "", errors.New("invalid bearer realm")
	}
	q := u.Query()
	if svc := params["service"]; svc != "" {
		q.Set("service", svc)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	if username != "" || password != "" {
		basic := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
		req.Header.Set("Authorization", "Basic "+basic)
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", err
	}
	if out.Token != "" {
		return out.Token, nil
	}
	return out.AccessToken, nil
}

func (s *Server) testGrafanaConnection(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL          string `json:"url"`
		Token        string `json:"token"`
		AllowPrivate bool   `json:"allowPrivate"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	baseURL := strings.TrimRight(strings.TrimSpace(req.URL), "/")
	token := strings.TrimSpace(req.Token)
	if token == "" {
		cur, _ := s.effectiveSisterPanels(r.Context())
		token = cur.GrafanaToken
	}
	if baseURL == "" {
		writeErr(w, http.StatusBadRequest, "Enter the Grafana URL.")
		return
	}
	if err := checkHTTPURL(baseURL, true); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	c := guardedClient(12*time.Second, req.AllowPrivate)
	defer c.CloseIdleConnections()

	endpoint := baseURL + "/api/health"
	if token != "" {
		endpoint = baseURL + "/api/search?limit=1"
	}
	hr, err := http.NewRequestWithContext(r.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if token != "" {
		hr.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(hr)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Couldn't reach Grafana: "+err.Error())
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("Grafana answered HTTP %d.", resp.StatusCode))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
