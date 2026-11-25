package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"moorage/internal/auth"
	"moorage/internal/sso"
	"moorage/internal/store"
)

// Actions of M10 written by handlers (public routes and the OIDC callback, a GET).
const (
	auditInviteAccept = "auth.invite_accept"
	auditOIDCLogin    = "auth.oidc_login"
	auditOIDCDenied   = "auth.oidc_denied"
	auditProxyLogin   = "auth.proxy_login"
)

const oidcStateCookie = "moorage_oidc_state"

type tokenKey struct{}

// sessionTokenOf is the session token of the request, also when forward-auth created it
// during this request (then the browser doesn't have the cookie yet).
func sessionTokenOf(r *http.Request) string {
	t, _ := r.Context().Value(tokenKey{}).(string)
	return t
}

func clientOf(r *http.Request) auth.Client {
	return auth.Client{IP: clientIP(r), UserAgent: r.UserAgent()}
}

// SetOIDC replaces the OIDC provider (tests use a fake IdP).
func (s *Server) SetOIDC(o *sso.OIDC) {
	s.oidcMu.Lock()
	s.oidc = o
	s.oidcMu.Unlock()
}

// StartSSO discovers the OIDC provider in the background.
func (s *Server) StartSSO(ctx context.Context) { s.getOIDC().Start(ctx) }

// setAuditTarget names the target of a request whose path holds only an id.
func setAuditTarget(r *http.Request, target string) {
	if st := auditStateOf(r); st != nil {
		st.target = target
	}
}

// sessionToken returns the session token, signing in through a trusted proxy first when
// it names a user. With the header, the proxy decides: a refused user gets no fallback to
// an older cookie session.
func (s *Server) sessionToken(w http.ResponseWriter, r *http.Request) (string, error) {
	cookie := ""
	if c, err := r.Cookie(cookieName); err == nil {
		cookie = c.Value
	}
	id, role, ok := s.proxy.FromRequest(r)
	if !ok {
		return cookie, nil
	}
	tok, u, fresh, err := s.auth.ProxySession(r.Context(), clientOf(r), id, role, cookie)
	if err != nil {
		if cookie != "" {
			s.auth.Logout(r.Context(), cookie) //nolint:errcheck // best effort
		}
		return "", err
	}
	if fresh {
		s.setCookie(w, r, tok)
		s.audit(r.Context(), store.AuditEntry{Actor: u.Username, IP: clientIP(r), Action: auditProxyLogin})
	}
	return tok, nil
}

// externalRefusal is the plain-words answer to a refused OIDC or proxy sign-in.
func externalRefusal(err error) (code, msg string) {
	switch {
	case errors.Is(err, auth.ErrNoAccess):
		return "denied", "Your account has no access to Moorage. Ask an admin to add you to a Moorage group."
	case errors.Is(err, auth.ErrAccountDisabled):
		return "disabled", "An admin disabled your Moorage account."
	case errors.Is(err, auth.ErrSetupFirst):
		return "setup", "Moorage isn't set up yet. The first admin must finish setup with a local account."
	}
	return "failed", "Sign-in failed. Check the Moorage log."
}

// ---- invite and reset links (public) ---------------------------------------

func (s *Server) inviteInspect(w http.ResponseWriter, r *http.Request) {
	var req struct{ Token string }
	if !readJSON(w, r, &req) {
		return
	}
	info, err := s.auth.InviteInspect(r.Context(), clientIP(r), strings.TrimSpace(req.Token))
	if err != nil {
		s.authErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) inviteStart(w http.ResponseWriter, r *http.Request) {
	var req struct{ Token, Password string }
	if !readJSON(w, r, &req) {
		return
	}
	secret, u, err := s.auth.InviteStart(r.Context(), clientIP(r), strings.TrimSpace(req.Token), req.Password)
	if err != nil {
		s.authErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"secret": secret, "otpauthUrl": u})
}

func (s *Server) inviteFinish(w http.ResponseWriter, r *http.Request) {
	var req struct{ Token, Code string }
	if !readJSON(w, r, &req) {
		return
	}
	tok, u, kind, err := s.auth.InviteFinish(r.Context(), clientOf(r), strings.TrimSpace(req.Token), req.Code)
	var refused auth.RefusedError
	if err != nil && kind != "" && errors.As(err, &refused) {
		// The link was used (the password changed) even though no session follows.
		s.audit(r.Context(), store.AuditEntry{Actor: u.Username, IP: clientIP(r), Action: auditInviteAccept, Target: kind})
	}
	if err != nil {
		s.authErr(w, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{Actor: u.Username, IP: clientIP(r), Action: auditInviteAccept, Target: kind})
	s.setCookie(w, r, tok)
	writeJSON(w, http.StatusOK, u)
}

// ---- OIDC ------------------------------------------------------------------

func (s *Server) loginRedirect(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/#/login?sso="+url.QueryEscape(code), http.StatusSeeOther)
}

func (s *Server) oidcStart(w http.ResponseWriter, r *http.Request) {
	oidcProv := s.getOIDC()
	if !oidcProv.Ready() {
		s.loginRedirect(w, r, "unavailable")
		return
	}
	target, f, err := oidcProv.Begin()
	if err != nil {
		s.loginRedirect(w, r, "unavailable")
		return
	}
	// Lax, not Strict: the provider's redirect back is a cross-site top-level navigation.
	http.SetCookie(w, &http.Cookie{
		Name: oidcStateCookie, Value: f.State, Path: "/api/auth/oidc", MaxAge: 600,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureCookie(r),
	})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	oidcProv := s.getOIDC()
	http.SetCookie(w, &http.Cookie{Name: oidcStateCookie, Value: "", Path: "/api/auth/oidc", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureCookie(r)})
	q := r.URL.Query()
	if q.Get("error") != "" {
		s.loginRedirect(w, r, "idp") // cancelled or refused at the provider
		return
	}
	cookieState := ""
	if c, err := r.Cookie(oidcStateCookie); err == nil {
		cookieState = c.Value
	}
	res, err := oidcProv.Finish(r.Context(), q.Get("state"), cookieState, q.Get("code"))
	if errors.Is(err, sso.ErrFlow) {
		s.loginRedirect(w, r, "expired")
		return
	}
	if err != nil {
		s.log.Warn("OIDC sign-in failed", "err", err)
		oidcProv.NoteError(err.Error())
		s.audit(r.Context(), store.AuditEntry{Actor: "unknown", IP: clientIP(r), Action: auditOIDCDenied,
			Target: "token rejected", Outcome: store.AuditFailed})
		s.loginRedirect(w, r, "failed")
		return
	}
	tok, u, err := s.auth.ExternalLogin(r.Context(), clientOf(r), res.Identity, res.Role)
	if err != nil {
		code, _ := externalRefusal(err)
		name := res.Identity.PreferredUsername
		if name == "" {
			name = res.Identity.Subject
		}
		s.audit(r.Context(), store.AuditEntry{Actor: triedName(name), IP: clientIP(r), Action: auditOIDCDenied,
			Target: code + ": sub " + res.Identity.Subject, Outcome: store.AuditFailed})
		if code == "failed" {
			s.log.Error("OIDC sign-in", "err", err)
		}
		s.loginRedirect(w, r, code)
		return
	}
	s.audit(r.Context(), store.AuditEntry{Actor: u.Username, IP: clientIP(r), Action: auditOIDCLogin, Target: u.Role})
	s.setCookie(w, r, tok)
	http.Redirect(w, r, "/#/", http.StatusSeeOther)
}

// ---- users (admin) ---------------------------------------------------------

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	list, err := s.auth.ListUsers(r.Context())
	if err != nil {
		s.authErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"users": list, "self": currentUser(r).ID, "oidc": s.getOIDC().Status(), "proxy": s.proxy.Status(),
		"publicUrl": s.effectivePublicURL(r.Context()),
	})
}

func (s *Server) linkResp(info auth.UserInfo, l auth.Link) map[string]any {
	resp := map[string]any{"user": info, "link": l}
	if pub := s.effectivePublicURL(context.Background()); pub != "" {
		resp["url"] = pub + "/#/invite/" + l.Token
	}
	return resp
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Role, DisplayName string }
	if !readJSON(w, r, &req) {
		return
	}
	info, link, err := s.auth.CreateUser(r.Context(), currentUser(r), req.Username, req.Role, req.DisplayName)
	if err != nil {
		s.authErr(w, err)
		return
	}
	setAuditTarget(r, info.Username)
	writeJSON(w, http.StatusCreated, s.linkResp(info, link))
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusNotFound, "User not found.")
		return 0, false
	}
	return id, true
}

// userChange runs one admin change and names the user in the audit row.
func (s *Server) userChange(fn func(ctx context.Context, admin auth.User, id int64) (store.User, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		u, err := fn(r.Context(), currentUser(r), id)
		if err != nil {
			s.authErr(w, err)
			return
		}
		setAuditTarget(r, u.Username)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) setUserRole(w http.ResponseWriter, r *http.Request) {
	var req struct{ Role string }
	if !readJSON(w, r, &req) {
		return
	}
	s.userChange(func(ctx context.Context, admin auth.User, id int64) (store.User, error) {
		return s.auth.SetRole(ctx, admin, id, req.Role)
	})(w, r)
}

func (s *Server) disableUser(w http.ResponseWriter, r *http.Request) {
	s.userChange(func(ctx context.Context, admin auth.User, id int64) (store.User, error) {
		return s.auth.SetDisabled(ctx, admin, id, true)
	})(w, r)
}

func (s *Server) enableUser(w http.ResponseWriter, r *http.Request) {
	s.userChange(func(ctx context.Context, admin auth.User, id int64) (store.User, error) {
		return s.auth.SetDisabled(ctx, admin, id, false)
	})(w, r)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	s.userChange(s.auth.DeleteUser)(w, r)
}

func (s *Server) revokeUserSessions(w http.ResponseWriter, r *http.Request) {
	s.userChange(s.auth.RevokeSessions)(w, r)
}

func (s *Server) resetUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	info, link, err := s.auth.IssueReset(r.Context(), currentUser(r), id)
	if err != nil {
		s.authErr(w, err)
		return
	}
	setAuditTarget(r, info.Username)
	writeJSON(w, http.StatusOK, s.linkResp(info, link))
}

// ---- own account -----------------------------------------------------------

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) {
	acc, err := s.auth.Account(r.Context(), sessionTokenOf(r))
	if err != nil {
		s.authErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, acc)
}

func (s *Server) setAccountPreferences(w http.ResponseWriter, r *http.Request) {
	var req auth.UserPreferences
	if !readJSON(w, r, &req) {
		return
	}
	saved, err := s.auth.SetPreferences(r.Context(), sessionTokenOf(r), req)
	if err != nil {
		s.authErr(w, err)
		return
	}
	setAuditTarget(r, currentUser(r).Username)
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var req struct{ Current, Password string }
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.auth.ChangePassword(r.Context(), sessionTokenOf(r), req.Current, req.Password); err != nil {
		s.authErr(w, err)
		return
	}
	setAuditTarget(r, currentUser(r).Username)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) totpStart(w http.ResponseWriter, r *http.Request) {
	var req struct{ Password, Code string }
	if !readJSON(w, r, &req) {
		return
	}
	e, err := s.auth.TOTPStart(r.Context(), sessionTokenOf(r), req.Password, req.Code)
	if err != nil {
		s.authErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) totpFinish(w http.ResponseWriter, r *http.Request) {
	var req struct{ Secret, Ticket, Code string }
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.auth.TOTPFinish(r.Context(), sessionTokenOf(r), req.Secret, req.Ticket, req.Code); err != nil {
		s.authErr(w, err)
		return
	}
	setAuditTarget(r, currentUser(r).Username)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) revokeOwnSession(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusNotFound, "Session not found.")
		return
	}
	if err := s.auth.RevokeSession(r.Context(), sessionTokenOf(r), id); err != nil {
		s.authErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) revokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	n, err := s.auth.RevokeOtherSessions(r.Context(), sessionTokenOf(r))
	if err != nil {
		s.authErr(w, err)
		return
	}
	setAuditTarget(r, "others")
	writeJSON(w, http.StatusOK, map[string]int64{"revoked": n})
}

// loginInfo tells the sign-in page which ways to sign in exist.
type loginInfo struct {
	SSO        bool   `json:"sso"`
	SSOLabel   string `json:"ssoLabel,omitempty"`
	LocalLogin bool   `json:"localLogin"`
	Proxy      bool   `json:"proxy"`
}

func (s *Server) loginInfo() loginInfo {
	oidcProv := s.getOIDC()
	li := loginInfo{LocalLogin: oidcProv.LocalLogin(), Proxy: s.proxy.Enabled()}
	if oidcProv.Ready() {
		li.SSO, li.SSOLabel = true, oidcProv.Label()
	}
	return li
}

// endSessionURL is the IdP logout for an OIDC user (RP-initiated logout, with client_id
// and no post-logout redirect, so nothing extra must be registered at the provider).
func (s *Server) endSessionURL(u auth.User) string {
	oidcProv := s.getOIDC()
	ep := oidcProv.EndSessionURL()
	if u.Source != store.SourceOIDC || ep == "" {
		return ""
	}
	pu, err := url.Parse(ep)
	if err != nil {
		return ""
	}
	q := pu.Query()
	q.Set("client_id", oidcProv.Status().ClientID)
	pu.RawQuery = q.Encode()
	return pu.String()
}
