package api

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"moorage/internal/store"
)

// Route values in auditActions that are not recorded by the middleware.
const (
	// auditSkip: the request changes nothing (a dry run, a preview, a conversion).
	auditSkip = ""
	// auditByHandler: a public route whose handler writes its own rows (sign-in, setup),
	// because it has no session for the middleware to take the actor from.
	auditByHandler = "-"
)

// auditActions maps every mutating route pattern to a stable action key. A mutating route
// missing here fails TestAuditEveryMutatingRouteMapped, so new routes can't skip the audit.
// "{action}" in a key is replaced with that path value.
var auditActions = map[string]string{
	"POST /api/setup/start":           auditSkip, // only a pending account; setup/finish records auth.setup
	"POST /api/setup/finish":          auditByHandler,
	"POST /api/login":                 auditByHandler,
	"POST /api/login/passkey/options": auditSkip, // a challenge only, nothing is stored
	"POST /api/login/passkey":         auditByHandler,
	"POST /api/logout":                auditByHandler,

	"POST /api/hosts/{host}/stacks/{name}/validate":           auditSkip,
	"POST /api/hosts/{host}/stacks/{name}/lint/fix":           auditSkip, // returns edited text, saves nothing
	"POST /api/hosts/{host}/stacks/{name}/env/extract":        auditSkip, // returns edited text, saves nothing
	"PUT /api/hosts/{host}/stacks/{name}/strategy":            "stack.strategy",
	"POST /api/hosts/{host}/containers/{id}/actions/{action}": "container.{action}",
	"POST /api/hosts/{host}/external/{name}/actions/{action}": "external.{action}",
	"POST /api/convert":              auditSkip,
	"POST /api/image-lookup":         auditSkip, // reads registries, changes nothing
	"POST /api/terminal/ticket":      auditSkip, // the terminal it opens is audited (terminal.open)
	"POST /api/checks/test":          auditSkip, // a one-off probe, nothing is stored
	"POST /api/channels/preview":     auditSkip, // renders a sample message, sends nothing
	"POST /api/status-pages/preview": auditSkip,
	"POST /api/templates/render":     auditSkip, // fills variables, writes nothing; the create is audited

	"POST /api/hosts/{host}/stacks":                               "stack.create",
	"PUT /api/hosts/{host}/stacks/{name}":                         "stack.save",
	"POST /api/hosts/{host}/stacks/{name}/self-apply":             "stack.self_apply",
	"PUT /api/hosts/{host}/stacks/{name}/links":                   "stack.link_set",
	"POST /api/hosts/{host}/stacks/{name}/ports/fix":              "stack.port_fix",
	"PUT /api/hosts/{host}/stacks/{name}/files/content":            "stack.file_save",
	"POST /api/hosts/{host}/tailscale/serve":                      "host.tailscale_serve",
	"DELETE /api/hosts/{host}/tailscale/serve/{port}":             "host.tailscale_stop",
	"POST /api/hosts/{host}/stacks/{name}/move-to-addons":         "stack.move_to_addons",
	"DELETE /api/hosts/{host}/stacks/{name}":                      "stack.delete",
	"POST /api/hosts/{host}/stacks/{name}/actions/{action}":       "stack.{action}",
	"POST /api/hosts/{host}/stacks/{name}/restore":                "stack.restore",
	"POST /api/agents":                                            "host.add",
	"DELETE /api/agents/join-tokens/{id}":                         "host.join_token_cancel",
	"PATCH /api/hosts/{host}":                                     "host.rename",
	"DELETE /api/hosts/{host}":                                    "host.remove",
	"PUT /api/hosts/{host}/addresses":                             "host.addresses",
	"PUT /api/notifications/settings":                             "notify.settings",
	"POST /api/checks":                                            "check.create",
	"PUT /api/checks/{id}":                                        "check.save",
	"DELETE /api/checks/{id}":                                     "check.delete",
	"POST /api/checks/{id}/incidents/{incidentId}/rollback":       "incident.rollback_verify",
	"POST /api/channels":                                          "channel.create",
	"POST /api/channels/test":                                     "channel.test",
	"PUT /api/channels/{id}":                                      "channel.save",
	"DELETE /api/channels/{id}":                                   "channel.delete",
	"PUT /api/watchdog":                                           "watchdog.save",
	"POST /api/watchdog/maintenance":                              "watchdog.maintenance",
	"POST /api/watchdog/test":                                     "watchdog.test",
	"POST /api/status-pages":                                      "statuspage.create",
	"PUT /api/status-pages/{id}":                                  "statuspage.save",
	"DELETE /api/status-pages/{id}":                               "statuspage.delete",
	"PUT /api/updates/settings":                                   "updates.settings",
	"POST /api/updates/check":                                     "updates.check",
	"POST /api/system/updates/check":                              "update.check",
	"POST /api/system/updates/apply":                              "update.apply",
	"POST /api/system/updates/{node}/rollback":                    "update.rollback",
	"PUT /api/hosts/{host}/stacks/{name}/images/policy":           "image.policy",
	"POST /api/hosts/{host}/stacks/{name}/images/check":           "image.check",
	"POST /api/hosts/{host}/stacks/{name}/images/update":          "image.update",
	"POST /api/hosts/{host}/stacks/{name}/images/change-tag":      "image.change_tag",
	"POST /api/hosts/{host}/stacks/{name}/images/rollback":        "image.rollback",
	"POST /api/maintenance":                                       "maintenance.create",
	"PUT /api/maintenance/{id}":                                   "maintenance.save",
	"DELETE /api/maintenance/{id}":                                "maintenance.delete",
	"POST /api/hosts/{host}/deps/detect":                          "dep.detect",
	"POST /api/hosts/{host}/stacks/{name}/deps":                   "dep.add",
	"DELETE /api/hosts/{host}/stacks/{name}/deps/{id}":            "dep.remove",
	"PUT /api/hosts/{host}/stacks/{name}/deps/{id}/ignored":       "dep.ignore",
	"PUT /api/hosts/{host}/stacks/{name}/deps/{id}/auto-restart":  "dep.auto_restart",
	"POST /api/templates/sources":                                 "template_source.create",
	"PUT /api/templates/sources/{id}":                             "template_source.save",
	"DELETE /api/templates/sources/{id}":                          "template_source.delete",
	"POST /api/templates/sources/{id}/refresh":                    "template_source.refresh",
	"POST /api/addons/install":                                    "addon.install",
	"POST /api/addons/custom":                                     "addon.custom_create",
	"PUT /api/addons/custom/{id}":                                 "addon.custom_save",
	"PUT /api/addons/{id}":                                        "addon.toggle",
	"DELETE /api/addons/{id}":                                     "addon.uninstall",
	"POST /api/addons/sources":                                    "addon_source.create",
	"PUT /api/addons/sources/{id}":                                "addon_source.save",
	"DELETE /api/addons/sources/{id}":                             "addon_source.delete",
	"POST /api/addons/sources/{id}/refresh":                       "addon_source.refresh",
	"POST /api/addons/prometheus-grafana/push":                    "addon.grafana_push",
	"POST /api/addons/scheduler/tasks":                            "scheduler.create",
	"PUT /api/addons/scheduler/tasks/{id}":                        "scheduler.save",
	"DELETE /api/addons/scheduler/tasks/{id}":                     "scheduler.delete",
	"POST /api/addons/scheduler/tasks/{id}/run":                   "scheduler.run",
	"POST /api/hosts/{host}/stacks/{name}/snapshots":              "stack.snapshot_create",
	"PUT /api/hosts/{host}/stacks/{name}/snapshots/settings":      "stack.snapshot_settings",
	"POST /api/hosts/{host}/stacks/{name}/snapshots/{id}/restore": "stack.snapshot_restore",
	"PUT /api/hosts/{host}/stacks/{name}/snapshots/{id}/pin":      "stack.snapshot_pin",
	"DELETE /api/hosts/{host}/stacks/{name}/snapshots/{id}":       "stack.snapshot_delete",
	"PUT /api/hosts/{host}/stacks/{name}/problems/ignored":        "stack.problem_ignore",
	"POST /api/hosts/{host}/self-apply/ack":                       "stack.self_apply_ack",
	"PUT /api/addons/assistant/settings":                          "addon.assistant_settings",
	"POST /api/assistant/chat":                                    auditSkip,
	"PUT /api/assistant/commands/{name}":                          "assistant.command_save",
	"DELETE /api/assistant/commands/{name}":                       "assistant.command_delete",
	"POST /api/addons/assistant/test":                             auditSkip, // a test question, nothing changes
	"POST /api/addons/assistant/models":                           auditSkip, // reads the model list with unsaved form values
	"POST /api/assistant/actions/{id}/apply":                      auditByHandler,
	"POST /api/assistant/actions/{id}/reject":                     "assistant.reject",
	"PUT /api/addons/status-snapshot/config":                      "status_snapshot.config",
	"POST /api/addons/status-snapshot/test":                       "status_snapshot.test",
	"POST /api/addons/status-snapshot/publish":                    "status_snapshot.publish",
	"POST /api/addons/webhooks":                                   "webhook.create",
	"PUT /api/addons/webhooks/{id}":                               "webhook.save",
	"DELETE /api/addons/webhooks/{id}":                            "webhook.delete",
	"POST /api/addons/mcp/tokens":                                 "mcp.token_create",
	"DELETE /api/addons/mcp/tokens/{id}":                          "mcp.token_delete",
	"POST /api/addons/mcp/drafts/{id}/approve":                    "mcp.draft_approve",
	"POST /api/addons/mcp/drafts/{id}/reject":                     "mcp.draft_reject",
	"POST /api/hosts/{host}/coolify/{uuid}/actions/{action}":      "coolify.{action}",
	"PUT /api/hosts/{host}/coolify/{uuid}/envs":                   "coolify.envs_save",
	"PUT /api/settings/config":                                    "settings.update",
	"POST /api/settings/metrics-token":                            "settings.metrics_token",
	"POST /api/settings/shared-vars":                              "settings.shared_var_save",
	"DELETE /api/settings/shared-vars/{id}":                       "settings.shared_var_delete",
	"PUT /api/settings/connections":                               "settings.connections",
	"POST /api/settings/connections/oidc/test":                    auditSkip,
	"POST /api/settings/connections/coolify/test":                 auditSkip,
	"POST /api/settings/connections/registry/test":                auditSkip,
	"POST /api/settings/connections/grafana/test":                 auditSkip,
	"PUT /api/settings/backup/remote":                             "backup.remote_config",
	"POST /api/settings/backup/remote/run":                        "backup.remote_run",
	"POST /api/settings/backup/export":                            "backup.export",
	"POST /api/settings/backup/import/preview":                    auditSkip, // only stages the upload
	"POST /api/settings/backup/import/apply":                      "backup.import",
	"POST /api/import/portainer/preview":                          auditSkip, // reads Portainer, changes nothing
	"POST /api/import/portainer/apply":                            "import.portainer",
	"POST /api/import/monitors/preview":                           auditSkip, // parses the upload, changes nothing
	"POST /api/import/monitors/apply":                             "import.monitors",
	"POST /api/hosts/{host}/deleted-stacks/{name}/restore":        "stack.restore",
	"POST /api/hosts/{host}/prune":                                "host.prune",
	"POST /api/auto-rollbacks/{id}/ack":                           "stack.auto_rollback_ack",
	"POST /api/push/subscriptions":                                "push.subscribe",
	"POST /api/push/subscriptions/delete":                         "push.unsubscribe",
	"POST /api/push/test":                                         auditSkip, // sends a test only

	// M10: invite links are public (the handler records auth.invite_accept); the OIDC
	// routes are GETs and record auth.oidc_login / auth.oidc_denied themselves.
	"POST /api/invite/inspect":          auditSkip, // reads the link, changes nothing
	"POST /api/invite/start":            auditSkip, // pending only; finish records the change
	"POST /api/invite/finish":           auditByHandler,
	"PUT /api/account/preferences":      "account.preferences",
	"PUT /api/account/password":         "account.password",
	"POST /api/account/totp/start":      auditSkip, // pending only; PUT /api/account/totp records it
	"PUT /api/account/totp":             "account.totp",
	"POST /api/account/passkeys/start":  auditSkip, // a challenge only; POST /api/account/passkeys records the add
	"POST /api/account/passkeys":        "account.passkey_add",
	"PUT /api/account/passkeys/{id}":    "account.passkey_save",
	"DELETE /api/account/passkeys/{id}": "account.passkey_remove",
	"DELETE /api/account/sessions":      "account.session_revoke",
	"DELETE /api/account/sessions/{id}": "account.session_revoke",
	"POST /api/users":                   "user.create",
	"PUT /api/users/{id}/role":          "user.role",
	"POST /api/users/{id}/disable":      "user.disable",
	"POST /api/users/{id}/enable":       "user.enable",
	"DELETE /api/users/{id}":            "user.delete",
	"POST /api/users/{id}/reset":        "user.reset",
	"DELETE /api/users/{id}/sessions":   "user.sessions_revoke",
	"POST /api/tokens":                  "token.create",
	"DELETE /api/tokens/{id}":           "token.delete",
}

// Actions written outside the middleware.
const (
	auditLogin        = "auth.login"
	auditLoginFailed  = "auth.login_failed"
	auditLogout       = "auth.logout"
	auditSetup        = "auth.setup"
	auditTermOpen     = "terminal.open"
	auditHostTermOpen = "terminal.host_open"
	auditLogsDownload = "logs.download"
)

func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// routeMux is a ServeMux that remembers its patterns, so a test can check that every
// mutating route has an audit action.
type routeMux struct {
	*http.ServeMux
	patterns []string
}

func (m *routeMux) Handle(pattern string, h http.Handler) {
	m.patterns = append(m.patterns, pattern)
	m.ServeMux.Handle(pattern, h)
}

func (m *routeMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.Handle(pattern, http.HandlerFunc(h))
}

type auditKey struct{}

// auditState travels in the request context. requireRole fills it in with the signed-in
// user and the request the mux matched (which carries r.Pattern and the path values).
type auditState struct {
	actor  string
	req    *http.Request
	stack  string // set by a handler when the stack isn't in the path (create)
	target string // set by a handler to name the target (a username) instead of the path id
}

func auditStateOf(r *http.Request) *auditState {
	st, _ := r.Context().Value(auditKey{}).(*auditState)
	return st
}

// setAuditStack names the stack of a request whose path doesn't hold it (stack.create).
// The name is validated by then and is never secret.
func setAuditStack(r *http.Request, stack string) {
	if st := auditStateOf(r); st != nil {
		st.stack = stack
	}
}

// statusWriter remembers the status code the handler sent.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// auditOutcome turns a status code into an outcome. Refusals (4xx) changed nothing and
// are not recorded at all.
func auditOutcome(code int) (string, bool) {
	switch {
	case code == 0, code >= 200 && code < 300:
		return store.AuditOK, true
	case code >= 500:
		return store.AuditFailed, true
	}
	return "", false
}

// withAudit records every mutating /api/ request after its handler ran. Only the action key,
// the path values, the actor and the IP are stored: never the body or the query.
func (s *Server) withAudit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isMutating(r.Method) || !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		st := &auditState{}
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), auditKey{}, st)))
		if st.req == nil {
			return // refused before a handler ran (no session, wrong role) or a public route
		}
		outcome, record := auditOutcome(sw.code)
		if !record {
			return
		}
		e, ok := s.auditEntryFor(st)
		if !ok {
			return
		}
		e.IP = clientIP(r)
		e.Outcome = outcome
		s.audit(context.WithoutCancel(r.Context()), e)
	})
}

var actionSegment = regexp.MustCompile(`^[a-z][a-z0-9-]{0,19}$`)

// auditEntryFor builds the row from the matched route pattern and its path values.
func (s *Server) auditEntryFor(st *auditState) (store.AuditEntry, bool) {
	r := st.req
	action, mapped := auditActions[r.Pattern]
	if !mapped {
		// The mapping test catches this; at runtime the change still gets a row.
		s.log.Error("audit: route has no action", "pattern", r.Pattern)
		action = "unmapped " + r.Pattern
	}
	if action == auditSkip || action == auditByHandler {
		return store.AuditEntry{}, false
	}
	if strings.Contains(action, "{action}") {
		a := r.PathValue("action")
		if !actionSegment.MatchString(a) {
			a = "other"
		}
		action = strings.ReplaceAll(action, "{action}", a)
	}
	e := store.AuditEntry{Actor: st.actor, Action: action, HostID: r.PathValue("host"), Target: r.PathValue("id")}
	if strings.Contains(r.Pattern, "/stacks/{name}") {
		e.Stack = r.PathValue("name")
	}
	if e.Stack == "" {
		e.Stack = st.stack
	}
	if st.target != "" {
		e.Target = st.target
	}
	return e, true
}

// audit writes one row; a failure is logged and never fails the request.
func (s *Server) audit(ctx context.Context, e store.AuditEntry) {
	if s.store == nil {
		return
	}
	if err := s.store.Audit(ctx, e); err != nil {
		s.log.Error("audit", "err", err)
	}
}

// ---- GET /api/audit --------------------------------------------------------

type auditEntryDTO struct {
	store.AuditEntry
	HostName         string `json:"hostName,omitempty"`
	StackDisplayName string `json:"stackDisplayName,omitempty"`
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.AuditFilter{
		Stack: strings.TrimSpace(q.Get("stack")),
		Actor: strings.TrimSpace(q.Get("actor")),
		// host and action take several values, repeated or comma-separated.
		HostIDs:        listParam(q["host"]),
		ActionPrefixes: listParam(q["action"]),
	}
	if len(f.HostIDs) > 20 || len(f.ActionPrefixes) > 30 {
		writeErr(w, http.StatusBadRequest, "Too many filter values.")
		return
	}
	if v := q.Get("before"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			writeErr(w, http.StatusBadRequest, "before must be an entry id.")
			return
		}
		f.BeforeID = id
	}
	f.Limit = 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "limit must be a positive number.")
			return
		}
		f.Limit = min(n, 500)
	}
	want := f.Limit
	f.Limit++ // one extra row tells whether there is another page
	rows, err := s.store.AuditEntries(r.Context(), f)
	if err != nil {
		s.engErr(w, err)
		return
	}
	more := len(rows) > want
	if more {
		rows = rows[:want]
	}
	names := s.hostNames(r.Context())
	out := make([]auditEntryDTO, 0, len(rows))
	for _, e := range rows {
		dto := auditEntryDTO{AuditEntry: e, HostName: names[e.HostID]}
		if e.Stack != "" {
			dto.StackDisplayName = s.StackDisplayName(r.Context(), e.HostID, e.Stack)
		}
		out = append(out, dto)
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out, "more": more})
}

// hostNames maps host ids to their current names (removed hosts are missing).
func (s *Server) hostNames(ctx context.Context) map[string]string {
	names := map[string]string{}
	if s.reg == nil {
		return names
	}
	if hs, err := s.reg.List(ctx); err == nil {
		for _, h := range hs {
			names[h.ID] = h.Name
		}
	}
	return names
}

// listParam splits repeated and comma-separated query values, dropping empties.
func listParam(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}
