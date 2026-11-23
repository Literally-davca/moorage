// Package api serves the JSON/WebSocket API and the embedded web UI.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"moorage/internal/auth"
	"moorage/internal/config"
	"moorage/internal/convert"
	"moorage/internal/deps"
	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/imagefind"
	"moorage/internal/metrics"
	"moorage/internal/monitor"
	"moorage/internal/selfupdate"
	"moorage/internal/sso"
	"moorage/internal/status"
	"moorage/internal/store"
	"moorage/internal/templates"
	"moorage/internal/tunnel"
	"moorage/internal/updater"
	"moorage/internal/web"
)

const cookieName = "moorage_session"

// csrfHeader must be present on every state-changing request. Browsers can't add
// custom headers cross-site without a CORS preflight, which Moorage never allows.
const csrfHeader = "X-Moorage"

// Server wires the HTTP layer.
type Server struct {
	cfg        config.Config
	auth       *auth.Service
	store      *store.Store
	reg        *host.Registry
	hub        *tunnel.Hub // nil when the agent port is off
	mon        *monitor.Monitor
	upd        *updater.Updater
	status     *status.Service
	metrics    *metrics.Collector
	deps       *deps.Service
	tpl        *templates.Service
	finder     *imagefind.Finder
	finderOnce sync.Once
	sysUpd     *selfupdate.Service
	oidc       *sso.OIDC
	proxy      *sso.Proxy
	log        *slog.Logger
	version    string

	termSem chan struct{}
	termCfg TerminalLimits
	termWG  sync.WaitGroup // open terminal handlers, so shutdown can close their audit rows

	routes []string // registered route patterns, for the audit mapping test
}

// TerminalLimits allows overriding terminal timeouts and concurrency limits in tests.
type TerminalLimits struct {
	MaxConcurrent  int
	IdleTimeout    time.Duration
	HardLimit      time.Duration
	RevokeCheckInt time.Duration
	HubCtx         context.Context
}

// New creates the API server.
func New(cfg config.Config, a *auth.Service, st *store.Store, reg *host.Registry, hub *tunnel.Hub, mon *monitor.Monitor, log *slog.Logger, version string) *Server {
	s := &Server{
		cfg: cfg, auth: a, store: st, reg: reg, hub: hub, mon: mon,
		upd:     updater.New(st, reg, mon, log, cfg.PublicURL),
		status:  status.New(st, mon, cfg.PublicURL),
		metrics: metrics.New(st, reg, mon, version),
		deps:    deps.New(st, reg, log),
		tpl:     templates.New(st, cfg.TemplatesDir, log),
		oidc:    sso.NewOIDC(cfg.OIDC, cfg.PublicURL, log, sso.Options{}),
		proxy:   sso.NewProxy(cfg.Proxy),
		log:     log,
		version: version,
		termSem: make(chan struct{}, 4),
		termCfg: TerminalLimits{
			MaxConcurrent:  4,
			IdleTimeout:    15 * time.Minute,
			HardLimit:      8 * time.Hour,
			RevokeCheckInt: 30 * time.Second,
		},
	}
	if mon != nil && s.upd != nil {
		mon.OnStackDown = s.upd.HandleStackCheckDown
	}
	return s
}

// SetTerminalLimits overrides terminal timeouts and concurrency limits (used in tests).
func (s *Server) SetTerminalLimits(lim TerminalLimits) {
	if lim.MaxConcurrent <= 0 {
		lim.MaxConcurrent = 4
	}
	if lim.IdleTimeout <= 0 {
		lim.IdleTimeout = 15 * time.Minute
	}
	if lim.HardLimit <= 0 {
		lim.HardLimit = 8 * time.Hour
	}
	if lim.RevokeCheckInt <= 0 {
		lim.RevokeCheckInt = 30 * time.Second
	}
	s.termCfg = lim
	s.termSem = make(chan struct{}, lim.MaxConcurrent)
}

// SetHubContext ends open terminals (reason hub-stopped) when ctx is cancelled.
func (s *Server) SetHubContext(ctx context.Context) {
	s.termCfg.HubCtx = ctx
	s.metrics.SetContext(ctx)
}

// Metrics exposes the Prometheus collector (e.g. for test timing overrides).
func (s *Server) Metrics() *metrics.Collector {
	return s.metrics
}

// WaitTerminals waits until open terminals have ended and written their audit rows, or ctx ends.
func (s *Server) WaitTerminals(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		s.termWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Updater exposes the image updater service.
func (s *Server) Updater() *updater.Updater {
	return s.upd
}

// SetSelfUpdate wires the Moorage self-update coordinator into the API server.
func (s *Server) SetSelfUpdate(su *selfupdate.Service) {
	s.sysUpd = su
}

// StatusHandler returns an HTTP handler that serves only public status pages
// (by path and by custom domain), never the admin UI or API. Used for MOORAGE_STATUS_LISTEN.
func (s *Server) StatusHandler() http.Handler {
	return s.status.Handler()
}

// StatusService exposes the status service (e.g. for test clock/TTL injection).
func (s *Server) StatusService() *status.Service {
	return s.status
}

type ctxKey struct{}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler {
	mux := &routeMux{ServeMux: http.NewServeMux()}

	// public
	mux.HandleFunc("GET /api/session", s.session)
	mux.HandleFunc("POST /api/setup/start", s.setupStart)
	mux.HandleFunc("POST /api/setup/finish", s.setupFinish)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("POST /api/invite/inspect", s.inviteInspect)
	mux.HandleFunc("POST /api/invite/start", s.inviteStart)
	mux.HandleFunc("POST /api/invite/finish", s.inviteFinish)
	mux.HandleFunc("GET /api/auth/oidc/start", s.oidcStart)
	mux.HandleFunc("GET "+sso.CallbackPath, s.oidcCallback)

	// signed in
	mux.Handle("GET /api/hosts", s.user(s.listHosts))
	mux.Handle("GET /api/hosts/{host}/info", s.user(s.hostInfo))
	mux.Handle("GET /api/hosts/{host}/stacks", s.user(s.listStacks))
	mux.Handle("GET /api/hosts/{host}/stacks/{name}", s.user(s.getStack))
	mux.Handle("GET /api/hosts/{host}/stacks/{name}/stats", s.user(s.stackStats))
	mux.Handle("GET /api/hosts/{host}/stacks/{name}/history", s.user(s.history))
	mux.Handle("GET /api/hosts/{host}/stacks/{name}/history/{hash}", s.user(s.stackVersion))
	mux.Handle("GET /api/hosts/{host}/stacks/{name}/timeline", s.user(s.stackTimeline))
	mux.Handle("POST /api/hosts/{host}/stacks/{name}/validate", s.user(s.validate))
	mux.Handle("GET /api/hosts/{host}/ws/logs", s.user(s.wsLogs))
	mux.Handle("GET /api/hosts/{host}/ws/ops/{id}", s.user(s.wsOp))
	mux.Handle("POST /api/convert", s.user(s.convert))
	mux.Handle("POST /api/image-lookup", s.admin(s.imageLookup))
	mux.Handle("GET /api/checks", s.user(s.listChecks))
	mux.Handle("GET /api/checks/{id}", s.user(s.getCheck))
	mux.Handle("GET /api/hosts/{host}/stacks/{name}/health", s.user(s.stackHealth))
	mux.Handle("GET /api/status-pages", s.user(s.listStatusPages))
	mux.Handle("GET /api/status-pages/{id}", s.user(s.getStatusPage))
	mux.Handle("GET /api/status-pages/{id}/preview", s.user(s.previewStatusPage))
	mux.Handle("GET /api/updates", s.user(s.listUpdates))
	mux.Handle("GET /api/hosts/{host}/stacks/{name}/images", s.user(s.getStackImages))
	mux.Handle("GET /api/hosts/{host}/stacks/{name}/maintenance", s.user(s.stackMaintenance))
	mux.Handle("GET /api/maintenance", s.user(s.listMaintenance))
	mux.Handle("GET /api/maintenance/{id}", s.user(s.getMaintenance))
	mux.Handle("GET /api/deps", s.user(s.allDeps))
	mux.Handle("GET /api/hosts/{host}/deps", s.user(s.hostDeps))
	mux.Handle("GET /api/templates", s.user(s.listTemplates))
	mux.Handle("GET /api/templates/{source}/{id}", s.user(s.getTemplate))
	mux.Handle("GET /api/account", s.user(s.getAccount))
	mux.Handle("PUT /api/account/password", s.user(s.changePassword))
	mux.Handle("POST /api/account/totp/start", s.user(s.totpStart))
	mux.Handle("PUT /api/account/totp", s.user(s.totpFinish))
	mux.Handle("DELETE /api/account/sessions", s.user(s.revokeOtherSessions))
	mux.Handle("DELETE /api/account/sessions/{id}", s.user(s.revokeOwnSession))

	// admin only
	mux.Handle("POST /api/hosts/{host}/stacks", s.admin(s.createStack))
	mux.Handle("PUT /api/hosts/{host}/stacks/{name}", s.admin(s.updateStack))
	mux.Handle("DELETE /api/hosts/{host}/stacks/{name}", s.admin(s.deleteStack))
	mux.Handle("POST /api/hosts/{host}/stacks/{name}/self-apply", s.admin(s.selfApply))
	mux.Handle("POST /api/hosts/{host}/stacks/{name}/actions/{action}", s.admin(s.runAction))
	mux.Handle("POST /api/hosts/{host}/stacks/{name}/restore", s.admin(s.restore))
	mux.Handle("POST /api/agents", s.admin(s.addAgent))
	mux.Handle("PATCH /api/hosts/{host}", s.admin(s.renameHost))
	mux.Handle("DELETE /api/hosts/{host}", s.admin(s.removeHost))
	mux.Handle("POST /api/checks", s.admin(s.saveCheck))
	mux.Handle("POST /api/checks/test", s.admin(s.testCheck))
	mux.Handle("PUT /api/checks/{id}", s.admin(s.saveCheck))
	mux.Handle("DELETE /api/checks/{id}", s.admin(s.deleteCheck))
	mux.Handle("GET /api/channels", s.admin(s.listChannels))
	mux.Handle("POST /api/channels", s.admin(s.saveChannel))
	mux.Handle("POST /api/channels/test", s.admin(s.testChannel))
	mux.Handle("PUT /api/channels/{id}", s.admin(s.saveChannel))
	mux.Handle("DELETE /api/channels/{id}", s.admin(s.deleteChannel))
	mux.Handle("GET /api/watchdog", s.admin(s.getWatchdog))
	mux.Handle("PUT /api/watchdog", s.admin(s.saveWatchdog))
	mux.Handle("POST /api/watchdog/maintenance", s.admin(s.watchdogMaintenance))
	mux.Handle("POST /api/watchdog/test", s.admin(s.testWatchdog))
	mux.Handle("POST /api/status-pages", s.admin(s.saveStatusPage))
	mux.Handle("POST /api/status-pages/preview", s.admin(s.previewDraft))
	mux.Handle("PUT /api/status-pages/{id}", s.admin(s.saveStatusPage))
	mux.Handle("DELETE /api/status-pages/{id}", s.admin(s.deleteStatusPage))
	mux.Handle("PUT /api/updates/settings", s.admin(s.saveUpdateSettings))
	mux.Handle("POST /api/updates/check", s.admin(s.checkUpdatesNow))
	mux.Handle("PUT /api/hosts/{host}/stacks/{name}/images/policy", s.admin(s.saveStackImagePolicy))
	mux.Handle("POST /api/hosts/{host}/stacks/{name}/images/check", s.admin(s.checkStackImagesNow))
	mux.Handle("POST /api/hosts/{host}/stacks/{name}/images/update", s.admin(s.updateStackImagesNow))
	mux.Handle("POST /api/hosts/{host}/stacks/{name}/images/change-tag", s.admin(s.changeStackImageTag))
	mux.Handle("POST /api/hosts/{host}/stacks/{name}/images/rollback", s.admin(s.rollbackStackImages))
	mux.Handle("GET /api/hosts/{host}/stacks/{name}/terminal", s.admin(s.wsTerminal))
	mux.Handle("GET /api/hosts/{host}/stacks/{name}/terminal/sessions", s.admin(s.listStackTerminals))
	mux.Handle("GET /api/terminal/sessions", s.admin(s.listTerminalSessions))
	mux.Handle("GET /api/audit", s.admin(s.listAudit))
	mux.Handle("GET /api/system/updates", s.admin(s.systemUpdates))
	mux.Handle("POST /api/system/updates/check", s.admin(s.checkSystemUpdates))
	mux.Handle("POST /api/system/updates/apply", s.admin(s.applySystemUpdates))
	mux.Handle("POST /api/system/updates/{node}/rollback", s.admin(s.rollbackSystemUpdate))
	mux.Handle("POST /api/maintenance", s.admin(s.saveMaintenance))
	mux.Handle("PUT /api/maintenance/{id}", s.admin(s.saveMaintenance))
	mux.Handle("DELETE /api/maintenance/{id}", s.admin(s.deleteMaintenance))
	mux.Handle("POST /api/hosts/{host}/deps/detect", s.admin(s.detectDeps))
	mux.Handle("POST /api/hosts/{host}/stacks/{name}/deps", s.admin(s.addDep))
	mux.Handle("DELETE /api/hosts/{host}/stacks/{name}/deps/{id}", s.admin(s.removeDep))
	mux.Handle("PUT /api/hosts/{host}/stacks/{name}/deps/{id}/ignored", s.admin(s.ignoreDep))
	mux.Handle("POST /api/templates/render", s.admin(s.renderTemplate))
	mux.Handle("GET /api/templates/sources", s.admin(s.listTemplateSources))
	mux.Handle("POST /api/templates/sources", s.admin(s.addTemplateSource))
	mux.Handle("PUT /api/templates/sources/{id}", s.admin(s.updateTemplateSource))
	mux.Handle("DELETE /api/templates/sources/{id}", s.admin(s.deleteTemplateSource))
	mux.Handle("POST /api/templates/sources/{id}/refresh", s.admin(s.refreshTemplateSource))
	mux.Handle("GET /api/users", s.admin(s.listUsers))
	mux.Handle("GET /api/addons", s.admin(s.listAddons))
	mux.Handle("POST /api/addons/install", s.admin(s.installAddon))
	mux.Handle("PUT /api/addons/{id}", s.admin(s.setAddon))
	mux.Handle("DELETE /api/addons/{id}", s.admin(s.uninstallAddon))
	mux.Handle("GET /api/settings/config", s.admin(s.getSettingsConfig))
	mux.Handle("PUT /api/settings/config", s.admin(s.putSettingsConfig))
	mux.Handle("POST /api/settings/metrics-token", s.admin(s.generateMetricsToken))
	mux.Handle("GET /api/settings/backup/export", s.admin(s.exportBackup))
	mux.Handle("POST /api/settings/backup/import", s.admin(s.importBackup))
	mux.Handle("GET /api/hosts/{host}/deleted-stacks", s.admin(s.listDeletedStacks))
	mux.Handle("POST /api/hosts/{host}/deleted-stacks/{name}/restore", s.admin(s.restoreDeletedStack))
	mux.Handle("GET /api/hosts/{host}/prune/preview", s.admin(s.prunePreview))
	mux.Handle("POST /api/hosts/{host}/prune", s.admin(s.pruneDisk))
	mux.Handle("GET /api/auto-rollbacks", s.user(s.listAutoRollbacks))
	mux.Handle("POST /api/auto-rollbacks/{id}/ack", s.admin(s.ackAutoRollback))
	mux.Handle("POST /api/users", s.admin(s.createUser))
	mux.Handle("PUT /api/users/{id}/role", s.admin(s.setUserRole))
	mux.Handle("POST /api/users/{id}/disable", s.admin(s.disableUser))
	mux.Handle("POST /api/users/{id}/enable", s.admin(s.enableUser))
	mux.Handle("DELETE /api/users/{id}", s.admin(s.deleteUser))
	mux.Handle("POST /api/users/{id}/reset", s.admin(s.resetUser))
	mux.Handle("DELETE /api/users/{id}/sessions", s.admin(s.revokeUserSessions))

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "Unknown API endpoint.")
	})
	mux.Handle("/", spa(web.Dist()))

	s.routes = mux.patterns
	adminHandler := s.secure(s.withAudit(mux))
	// /metrics sits outside the session and CSRF layers: Prometheus authenticates with
	// a bearer token. It comes after the custom-domain check, so a status page domain
	// never serves it (and the status listener has its own handler without it).
	metricsHandler := s.metrics.HandlerFunc(func() string { return s.effectiveMetricsToken(context.Background()) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.status.ServeCustomDomain(w, r) {
			return
		}
		if r.URL.Path == "/metrics" {
			metricsHandler.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/status-assets/") {
			s.status.ServeAsset(w, r)
			return
		}
		if r.URL.Path == "/status" || strings.HasPrefix(r.URL.Path, "/status/") {
			s.status.ServeSlugRoute(w, r)
			return
		}
		// Forward-auth headers count only from a trusted proxy; from anyone else they are
		// removed here, before any handler could read them.
		if !s.proxy.Trusted(r) {
			s.proxy.Strip(r)
		}
		adminHandler.ServeHTTP(w, r)
	})
}

// secure adds security headers and the CSRF check.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
			"script-src 'self'; connect-src 'self'; font-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) != "1" {
			writeErr(w, http.StatusForbidden, "Missing request header.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) user(h http.HandlerFunc) http.Handler  { return s.requireRole(h, false) }
func (s *Server) admin(h http.HandlerFunc) http.Handler { return s.requireRole(h, true) }

func (s *Server) requireRole(h http.HandlerFunc, admin bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, err := s.sessionToken(w, r)
		if err != nil {
			_, msg := externalRefusal(err)
			writeErr(w, http.StatusForbidden, msg)
			return
		}
		if tok == "" {
			writeErr(w, http.StatusUnauthorized, "Sign in to continue.")
			return
		}
		u, err := s.auth.SessionUser(r.Context(), tok)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "Your session expired. Sign in again.")
			return
		}
		if admin && u.Role != "admin" {
			writeErr(w, http.StatusForbidden, "Only admins can do this.")
			return
		}
		// The audit middleware sits outside the mux, so it learns the actor and the matched
		// request (pattern and path values) only from here.
		if st := auditStateOf(r); st != nil {
			st.actor = u.Username
			st.req = r
		}
		ctx := context.WithValue(context.WithValue(r.Context(), ctxKey{}, u), tokenKey{}, tok)
		h(w, r.WithContext(ctx))
	})
}

func currentUser(r *http.Request) auth.User {
	u, _ := r.Context().Value(ctxKey{}).(auth.User)
	return u
}

// ---- session & setup -------------------------------------------------------

type sessionResp struct {
	SetupRequired bool       `json:"setupRequired"`
	User          *auth.User `json:"user"`
	Version       string     `json:"version"`
	Login         loginInfo  `json:"login"`
	// ProxyError is why a user named by the trusted proxy was refused.
	ProxyError string `json:"proxyError,omitempty"`
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	resp := sessionResp{SetupRequired: s.auth.SetupRequired(), Version: s.version, Login: s.loginInfo()}
	tok, err := s.sessionToken(w, r)
	if err != nil {
		_, resp.ProxyError = externalRefusal(err)
	} else if tok != "" {
		if u, err := s.auth.SessionUser(r.Context(), tok); err == nil {
			resp.User = &u
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) setupStart(w http.ResponseWriter, r *http.Request) {
	var req struct{ Token, Username, Password string }
	if !readJSON(w, r, &req) {
		return
	}
	secret, url, err := s.auth.SetupStart(r.Context(), clientIP(r), strings.TrimSpace(req.Token), strings.TrimSpace(req.Username), req.Password)
	if err != nil {
		s.authErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"secret": secret, "otpauthUrl": url})
}

func (s *Server) setupFinish(w http.ResponseWriter, r *http.Request) {
	var req struct{ Token, Username, Code string }
	if !readJSON(w, r, &req) {
		return
	}
	tok, u, err := s.auth.SetupFinish(r.Context(), clientOf(r), strings.TrimSpace(req.Token), strings.TrimSpace(req.Username), req.Code)
	if err != nil {
		s.authErr(w, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{Actor: u.Username, IP: clientIP(r), Action: auditSetup})
	s.setCookie(w, r, tok)
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password, Code string }
	if !readJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Username)
	tok, u, err := s.auth.Login(r.Context(), clientOf(r), name, req.Password, req.Code)
	if err != nil {
		// Only a real wrong-credentials answer is a failed sign-in. Rate-limited and
		// malformed attempts never reached the check (and the limiter bounds the rows).
		if errors.Is(err, auth.ErrInvalidCredentials) {
			s.audit(r.Context(), store.AuditEntry{Actor: triedName(name), IP: clientIP(r), Action: auditLoginFailed, Outcome: store.AuditFailed})
		}
		s.authErr(w, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{Actor: u.Username, IP: clientIP(r), Action: auditLogin})
	s.setCookie(w, r, tok)
	writeJSON(w, http.StatusOK, u)
}

// triedName shortens the username of a failed sign-in: it is free text from anyone.
func triedName(name string) string {
	if r := []rune(name); len(r) > 64 {
		return string(r[:64])
	}
	return name
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	redirect := ""
	if c, err := r.Cookie(cookieName); err == nil {
		// Only a live session is a sign-out; a stale cookie changes nothing.
		if u, err := s.auth.SessionUser(r.Context(), c.Value); err == nil {
			s.audit(r.Context(), store.AuditEntry{Actor: u.Username, IP: clientIP(r), Action: auditLogout})
			redirect = s.endSessionURL(u)
		}
		s.auth.Logout(r.Context(), c.Value) //nolint:errcheck // best effort
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.secureCookie(r)})
	if redirect != "" { // OIDC user: the UI may continue to the provider's logout
		writeJSON(w, http.StatusOK, map[string]string{"redirect": redirect})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: s.secureCookie(r), Expires: time.Now().Add(auth.SessionTTL),
	})
}

func (s *Server) secureCookie(r *http.Request) bool {
	if s.cfg.SecureCookiesSet {
		return s.cfg.SecureCookies
	}
	if r.TLS != nil {
		return true
	}
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return true
	}
	return strings.HasPrefix(s.effectivePublicURL(r.Context()), "https://")
}

func (s *Server) authErr(w http.ResponseWriter, err error) {
	var ve auth.ValidationError
	var re auth.RefusedError
	switch {
	case errors.As(err, &ve):
		writeErr(w, http.StatusBadRequest, ve.Msg)
	case errors.As(err, &re):
		writeErr(w, http.StatusConflict, re.Msg)
	case errors.Is(err, auth.ErrBadLink):
		writeErr(w, http.StatusBadRequest, sentence(err))
	case errors.Is(err, auth.ErrUserNotFound):
		writeErr(w, http.StatusNotFound, "User not found.")
	case errors.Is(err, auth.ErrSessionNotFound):
		writeErr(w, http.StatusNotFound, "Session not found.")
	case errors.Is(err, auth.ErrNoSession):
		writeErr(w, http.StatusUnauthorized, "Your session expired. Sign in again.")
	case errors.Is(err, auth.ErrRateLimited):
		writeErr(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, auth.ErrInvalidCredentials), errors.Is(err, auth.ErrBadSetupToken):
		writeErr(w, http.StatusUnauthorized, sentence(err))
	case errors.Is(err, auth.ErrSetupClosed):
		writeErr(w, http.StatusConflict, sentence(err))
	default:
		s.log.Error("auth", "err", err)
		writeErr(w, http.StatusInternalServerError, "Something went wrong. Check the Moorage log.")
	}
}

// ---- hosts -----------------------------------------------------------------

// hostOf resolves {host} from the path. write=true also refuses read-only agents.
func (s *Server) hostOf(w http.ResponseWriter, r *http.Request, write bool) (host.Host, bool) {
	h, code, msg := s.resolveHost(r, write)
	if h == nil {
		writeErr(w, code, msg)
		return nil, false
	}
	return h, true
}

// resolveHost is hostOf without writing the response, for the terminal WebSocket,
// which reports refusals as frames after the upgrade.
func (s *Server) resolveHost(r *http.Request, write bool) (host.Host, int, string) {
	if s.reg == nil {
		return nil, http.StatusNotFound, "Host not found."
	}
	h, e, err := s.reg.Get(r.Context(), r.PathValue("host"))
	switch {
	case errors.Is(err, host.ErrUnknownHost):
		return nil, http.StatusNotFound, "Host not found."
	case errors.Is(err, host.ErrOffline):
		msg := e.Name + " hasn't connected yet. Start its agent with the join token."
		if !e.LastSeen.IsZero() {
			msg = e.Name + " is offline, last seen " + e.LastSeen.Format("2 Jan 15:04") + "."
		}
		return nil, http.StatusServiceUnavailable, msg
	case err != nil:
		code, msg := s.errStatus(err)
		return nil, code, msg
	}
	if write && e.ReadOnly {
		return nil, http.StatusForbidden, sentence(host.ErrHostReadOnly)
	}
	return h, 0, ""
}

func (s *Server) listHosts(w http.ResponseWriter, r *http.Request) {
	if s.reg == nil {
		writeJSON(w, http.StatusOK, map[string]any{"hosts": []host.Entry{}})
		return
	}
	list, err := s.reg.List(r.Context())
	if err != nil {
		s.engErr(w, err)
		return
	}
	if list == nil {
		list = []host.Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hosts": list, "agentPort": s.agentPort(), "acceptsAgents": s.hub != nil,
		"metrics": s.metricsInfo(r.Context()),
	})
}

// metricsInfo tells the UI whether /metrics is on and where, never the token. Without
// MOORAGE_PUBLIC_URL the UI builds the URL from the address it was opened at.
func (s *Server) metricsInfo(ctx context.Context) map[string]any {
	info := map[string]any{"enabled": s.effectiveMetricsToken(ctx) != ""}
	if pub := s.effectivePublicURL(ctx); pub != "" {
		info["url"] = pub + "/metrics"
	}
	return info
}

func (s *Server) agentPort() string {
	_, port, err := net.SplitHostPort(s.cfg.AgentListen)
	if err != nil {
		return ""
	}
	return port
}

func (s *Server) hostInfo(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, h.Info(r.Context()))
}

func hostName(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req struct{ Name string }
	if !readJSON(w, r, &req) {
		return "", false
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len([]rune(name)) > 40 {
		writeErr(w, http.StatusBadRequest, "Give the host a name of 1–40 characters.")
		return "", false
	}
	return name, true
}

// addAgent creates a one-time join token for a new host.
func (s *Server) addAgent(w http.ResponseWriter, r *http.Request) {
	if s.hub == nil {
		writeErr(w, http.StatusConflict, "The agent port is off. Set MOORAGE_AGENT_LISTEN and restart Moorage.")
		return
	}
	name, ok := hostName(w, r)
	if !ok {
		return
	}
	tok, exp, err := s.hub.NewJoinToken(r.Context(), name)
	if err != nil {
		s.engErr(w, err)
		return
	}
	s.log.Info("agent join token created", "name", name, "user", currentUser(r).Username)
	writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "expires": exp, "agentPort": s.agentPort()})
}

func (s *Server) renameHost(w http.ResponseWriter, r *http.Request) {
	name, ok := hostName(w, r)
	if !ok {
		return
	}
	if err := s.store.RenameAgent(r.Context(), r.PathValue("host"), name); err != nil {
		writeErr(w, http.StatusNotFound, "Host not found.")
		return
	}
	if s.hub != nil {
		go s.hub.PushWatchdog(context.Background())
	}
	w.WriteHeader(http.StatusNoContent)
}

// removeHost unpairs an agent: its certificate is no longer accepted.
func (s *Server) removeHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("host")
	if id == host.LocalID {
		writeErr(w, http.StatusBadRequest, "The local host can't be removed.")
		return
	}
	if _, err := s.store.AgentByID(r.Context(), id); err != nil {
		writeErr(w, http.StatusNotFound, "Host not found.")
		return
	}
	if err := s.store.DeleteAgent(r.Context(), id); err != nil {
		s.engErr(w, err)
		return
	}
	s.reg.Disconnect(id)
	s.deps.HostRemoved(id)
	if s.hub != nil {
		go s.hub.PushWatchdog(context.Background())
	}
	s.log.Info("agent removed", "agent", id, "user", currentUser(r).Username)
	w.WriteHeader(http.StatusNoContent)
}

// ---- stacks ----------------------------------------------------------------

func (s *Server) listStacks(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	list, err := h.ListStacks(r.Context())
	proxyOn, _ := s.store.AddonEnabled(r.Context(), addonProxyLabels)
	for i := range list {
		if proxyOn && len(list[i].ProxyURLs) > 0 {
			list[i].URLs = mergeURLs(list[i].URLs, list[i].ProxyURLs, 10)
		}
		list[i].ProxyURLs = nil
	}
	resp := map[string]any{"stacks": list}
	if list == nil {
		resp["stacks"] = []engine.StackSummary{}
	}
	if err != nil {
		resp["error"] = "Docker isn't reachable, showing stacks from the folder only."
		s.log.Warn("list stacks", "host", r.PathValue("host"), "err", err)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) getStack(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	st, err := h.GetStack(r.Context(), r.PathValue("name"))
	if err != nil {
		s.engErr(w, err)
		return
	}
	proxyOn, _ := s.store.AddonEnabled(r.Context(), addonProxyLabels)
	if proxyOn && len(st.ProxyURLs) > 0 {
		st.URLs = mergeURLs(st.URLs, st.ProxyURLs, 10)
	}
	st.ProxyURLs = nil
	for i := range st.Containers {
		if proxyOn && len(st.Containers[i].ProxyURLs) > 0 {
			st.Containers[i].URLs = mergeURLs(st.Containers[i].URLs, st.Containers[i].ProxyURLs, 5)
		}
		st.Containers[i].ProxyURLs = nil
	}
	writeJSON(w, http.StatusOK, st)
}

func mergeURLs(base, extra []string, max int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(base)+len(extra))
	for _, u := range base {
		if !seen[u] && len(out) < max {
			seen[u] = true
			out = append(out, u)
		}
	}
	for _, u := range extra {
		if !seen[u] && len(out) < max {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

func (s *Server) stackStats(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	stats, err := h.Stats(r.Context(), r.PathValue("name"))
	if err != nil {
		s.engErr(w, err)
		return
	}
	if stats == nil {
		stats = []engine.Stat{}
	}
	writeJSON(w, http.StatusOK, stats)
}

type stackFiles struct {
	Name    string `json:"name"`
	Compose string `json:"compose"`
	Env     string `json:"env"`
}

func (s *Server) validate(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	var req stackFiles
	if !readJSON(w, r, &req) {
		return
	}
	err := h.Validate(r.Context(), r.PathValue("name"), req.Compose, req.Env)
	var ve engine.ValidationError
	if errors.As(err, &ve) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": ve.Msg})
		return
	}
	if err != nil {
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) createStack(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, true)
	if !ok {
		return
	}
	var req struct {
		stackFiles
		// Template is set when the files were rendered from a template. It only changes
		// the first version message; the files are the admin's reviewed ones.
		Template *templateRef `json:"template"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	note, ok := s.templateNote(r, req.Template)
	if !ok {
		writeErr(w, http.StatusBadRequest, "Unknown template.")
		return
	}
	name, user := strings.TrimSpace(req.Name), currentUser(r).Username
	var err error
	if nc, isNoted := h.(host.NotedCreator); isNoted && note != "" {
		err = nc.CreateNoted(r.Context(), name, req.Compose, req.Env, user, note)
	} else {
		err = h.Save(r.Context(), name, req.Compose, req.Env, user, true)
	}
	if err != nil {
		s.engErr(w, err)
		return
	}
	s.log.Info("stack created", "host", r.PathValue("host"), "stack", name, "user", user, "template", req.Template != nil)
	detail := ""
	if req.Template != nil {
		detail = "from template " + req.Template.ID
	}
	s.stackEvent(r, name, "create", detail)
	setAuditStack(r, name)
	writeJSON(w, http.StatusCreated, map[string]string{"name": name})
}

func (s *Server) updateStack(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, true)
	if !ok {
		return
	}
	var req stackFiles
	if !readJSON(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	if err := h.Save(r.Context(), name, req.Compose, req.Env, currentUser(r).Username, false); err != nil {
		s.engErr(w, err)
		return
	}
	s.log.Info("stack saved", "host", r.PathValue("host"), "stack", name, "user", currentUser(r).Username)
	s.stackEvent(r, name, "edit", "")
	w.WriteHeader(http.StatusNoContent)
}

// selfApply saves the stack Moorage runs from and hands `compose up` to a helper
// container, which restarts Moorage and rolls back if it doesn't come back.
func (s *Server) selfApply(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, true)
	if !ok {
		return
	}
	sa, ok := h.(host.SelfApplier)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "This host can't apply changes to Moorage's own stack.")
		return
	}
	var req stackFiles
	if !readJSON(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	if err := sa.SelfApply(r.Context(), name, req.Compose, req.Env, currentUser(r).Username); err != nil {
		if host.IsUnsupported(err) {
			writeErr(w, http.StatusNotImplemented, "Update the agent to apply changes to the stack it runs from.")
			return
		}
		s.engErr(w, err)
		return
	}
	s.log.Info("self stack apply started", "host", r.PathValue("host"), "stack", name, "user", currentUser(r).Username)
	s.stackEvent(r, name, "up", "Apply & restart")
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) deleteStack(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, true)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if err := h.Delete(r.Context(), name, currentUser(r).Username); err != nil {
		s.engErr(w, err)
		return
	}
	s.log.Info("stack deleted", "host", r.PathValue("host"), "stack", name, "user", currentUser(r).Username)
	s.stackEvent(r, name, "delete", "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) runAction(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, true)
	if !ok {
		return
	}
	name, action, user := r.PathValue("name"), r.PathValue("action"), currentUser(r).Username
	op, err := h.RunAction(r.Context(), name, action, user)
	if err != nil {
		s.engErr(w, err)
		return
	}
	s.log.Info("stack action", "host", r.PathValue("host"), "stack", name, "action", action, "user", user)
	if kind := actionEvent[action]; kind != "" {
		s.stackEvent(r, name, kind, "")
	}
	writeJSON(w, http.StatusAccepted, op)
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	list, err := h.History(r.Context(), r.PathValue("name"))
	if err != nil {
		s.engErr(w, err)
		return
	}
	if list == nil {
		list = []engine.Version{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) stackVersion(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	v, err := h.Version(r.Context(), r.PathValue("name"), r.PathValue("hash"))
	if err != nil {
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// restore writes an older version back and optionally deploys it right away.
func (s *Server) restore(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, true)
	if !ok {
		return
	}
	var req struct {
		Hash   string `json:"hash"`
		Deploy bool   `json:"deploy"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name, user := r.PathValue("name"), currentUser(r).Username
	v, err := h.Restore(r.Context(), name, req.Hash, user)
	if err != nil {
		s.engErr(w, err)
		return
	}
	s.log.Info("stack restored", "host", r.PathValue("host"), "stack", name, "version", req.Hash, "user", user)
	short := req.Hash
	if len(short) > 7 {
		short = short[:7]
	}
	if req.Deploy {
		s.stackEvent(r, name, "rollback", "to "+short)
	} else {
		s.stackEvent(r, name, "edit", "restored "+short)
	}
	resp := map[string]any{"version": v}
	if req.Deploy {
		op, err := h.RunAction(r.Context(), name, "up", user)
		if err != nil {
			resp["deployError"] = sentence(err)
		} else {
			resp["op"] = op
			s.log.Info("stack action", "host", r.PathValue("host"), "stack", name, "action", "up", "user", user)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) convert(w http.ResponseWriter, r *http.Request) {
	var req struct{ Command string }
	if !readJSON(w, r, &req) {
		return
	}
	res, err := convert.DockerRun(req.Command)
	if err != nil {
		writeErr(w, http.StatusBadRequest, sentence(err))
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) engErr(w http.ResponseWriter, err error) {
	code, msg := s.errStatus(err)
	writeErr(w, code, msg)
}

// errStatus maps an engine or host error to an HTTP status and a user-facing message.
func (s *Server) errStatus(err error) (int, string) {
	var ve engine.ValidationError
	switch {
	case errors.As(err, &ve):
		return http.StatusUnprocessableEntity, ve.Msg
	case errors.Is(err, engine.ErrNotFound), errors.Is(err, engine.ErrNoHistory), errors.Is(err, host.ErrOpNotFound),
		errors.Is(err, engine.ErrContainerNotInStack):
		return http.StatusNotFound, sentence(err)
	case errors.Is(err, engine.ErrInvalidName):
		return http.StatusBadRequest, "Stack name: " + err.Error() + "."
	case errors.Is(err, engine.ErrBusy), errors.Is(err, engine.ErrExists), errors.Is(err, engine.ErrHasRunning):
		return http.StatusConflict, sentence(err)
	case errors.Is(err, engine.ErrSelfStack):
		return http.StatusConflict, sentence(err)
	case errors.Is(err, engine.ErrHistoryOff), errors.Is(err, host.ErrOffline):
		return http.StatusServiceUnavailable, sentence(err)
	case errors.Is(err, engine.ErrReadOnly), errors.Is(err, host.ErrHostReadOnly), errors.Is(err, host.ErrTerminalDisabled):
		return http.StatusForbidden, sentence(err)
	case errors.Is(err, engine.ErrInvalidShell):
		return http.StatusBadRequest, sentence(err)
	case errors.Is(err, engine.ErrContainerNotRunning):
		return http.StatusConflict, sentence(err)
	case errors.Is(err, engine.ErrManualEdit):
		return http.StatusUnprocessableEntity, err.Error()
	default:
		s.log.Error("engine", "err", err)
		return http.StatusBadGateway, "Docker returned an error: " + err.Error()
	}
}

// ---- helpers ---------------------------------------------------------------

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2*engine.MaxFileSize+4096)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "The request couldn't be read.")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck // client went away
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// sentence capitalises an error message and ends it with a period.
func sentence(err error) string {
	m := err.Error()
	if m == "" {
		return m
	}
	m = strings.ToUpper(m[:1]) + m[1:]
	if !strings.HasSuffix(m, ".") {
		m += "."
	}
	return m
}

// clientIP uses the TCP peer address; forwarded headers are not trusted.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// spa serves the embedded UI and falls back to index.html for client routes.
func spa(dist fs.FS) http.Handler {
	files := http.FileServerFS(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(dist, p); err != nil {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
			p = "index.html"
		}
		if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
