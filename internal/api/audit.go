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
	"POST /api/setup/start":  auditSkip, // only a pending account; setup/finish records auth.setup
	"POST /api/setup/finish": auditByHandler,
	"POST /api/login":        auditByHandler,
	"POST /api/logout":       auditByHandler,

	"POST /api/hosts/{host}/stacks/{name}/validate": auditSkip,
	"POST /api/convert":                             auditSkip,
	"POST /api/checks/test":                         auditSkip, // a one-off probe, nothing is stored
	"POST /api/status-pages/preview":                auditSkip,
	"POST /api/templates/render":                    auditSkip, // fills variables, writes nothing; the create is audited

	"POST /api/hosts/{host}/stacks":                          "stack.create",
	"PUT /api/hosts/{host}/stacks/{name}":                    "stack.save",
	"DELETE /api/hosts/{host}/stacks/{name}":                 "stack.delete",
	"POST /api/hosts/{host}/stacks/{name}/actions/{action}":  "stack.{action}",
	"POST /api/hosts/{host}/stacks/{name}/restore":           "stack.restore",
	"POST /api/agents":                                       "host.add",
	"PATCH /api/hosts/{host}":                                "host.rename",
	"DELETE /api/hosts/{host}":                               "host.remove",
	"POST /api/checks":                                       "check.create",
	"PUT /api/checks/{id}":                                   "check.save",
	"DELETE /api/checks/{id}":                                "check.delete",
	"POST /api/channels":                                     "channel.create",
	"POST /api/channels/test":                                "channel.test",
	"PUT /api/channels/{id}":                                 "channel.save",
	"DELETE /api/channels/{id}":                              "channel.delete",
	"PUT /api/watchdog":                                      "watchdog.save",
	"POST /api/watchdog/maintenance":                         "watchdog.maintenance",
	"POST /api/watchdog/test":                                "watchdog.test",
	"POST /api/status-pages":                                 "statuspage.create",
	"PUT /api/status-pages/{id}":                             "statuspage.save",
	"DELETE /api/status-pages/{id}":                          "statuspage.delete",
	"PUT /api/updates/settings":                              "updates.settings",
	"POST /api/updates/check":                                "updates.check",
	"PUT /api/hosts/{host}/stacks/{name}/images/policy":      "image.policy",
	"POST /api/hosts/{host}/stacks/{name}/images/check":      "image.check",
	"POST /api/hosts/{host}/stacks/{name}/images/update":     "image.update",
	"POST /api/hosts/{host}/stacks/{name}/images/change-tag": "image.change_tag",
	"POST /api/hosts/{host}/stacks/{name}/images/rollback":   "image.rollback",
	"POST /api/maintenance":                                  "maintenance.create",
	"PUT /api/maintenance/{id}":                              "maintenance.save",
	"DELETE /api/maintenance/{id}":                           "maintenance.delete",
	"POST /api/hosts/{host}/deps/detect":                     "dep.detect",
	"POST /api/hosts/{host}/stacks/{name}/deps":              "dep.add",
	"DELETE /api/hosts/{host}/stacks/{name}/deps/{id}":       "dep.remove",
	"PUT /api/hosts/{host}/stacks/{name}/deps/{id}/ignored":  "dep.ignore",
	"POST /api/templates/sources":                            "template_source.create",
	"PUT /api/templates/sources/{id}":                        "template_source.save",
	"DELETE /api/templates/sources/{id}":                     "template_source.delete",
	"POST /api/templates/sources/{id}/refresh":               "template_source.refresh",
}

// Actions written outside the middleware.
const (
	auditLogin       = "auth.login"
	auditLoginFailed = "auth.login_failed"
	auditLogout      = "auth.logout"
	auditSetup       = "auth.setup"
	auditTermOpen    = "terminal.open"
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
	actor string
	req   *http.Request
	stack string // set by a handler when the stack isn't in the path (create)
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
	return e, true
}

// audit writes one row; a failure is logged and never fails the request.
func (s *Server) audit(ctx context.Context, e store.AuditEntry) {
	if s.store == nil {
		return
	}
	if err := s.store.Audit(ctx, e); err != nil {
		s.log.Warn("audit", "err", err)
	}
}

// ---- GET /api/audit --------------------------------------------------------

type auditEntryDTO struct {
	store.AuditEntry
	HostName string `json:"hostName,omitempty"`
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.AuditFilter{
		HostID:       strings.TrimSpace(q.Get("host")),
		Stack:        strings.TrimSpace(q.Get("stack")),
		Actor:        strings.TrimSpace(q.Get("actor")),
		ActionPrefix: strings.TrimSpace(q.Get("action")),
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
		out = append(out, auditEntryDTO{AuditEntry: e, HostName: names[e.HostID]})
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
