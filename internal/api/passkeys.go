package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"moorage/internal/auth"
	"moorage/internal/store"
)

// Passkeys (M18B): a second factor next to TOTP and, per passkey, an optional
// passwordless sign-in. The relying party comes only from the configured public URL
// (auth.RelyingPartyFor), never from the request's Host header.

// passkeyRP is the relying party of this instance, or why passkeys are unavailable.
func (s *Server) passkeyRP(ctx context.Context) (auth.RelyingParty, error) {
	return auth.RelyingPartyFor(s.effectivePublicURL(ctx))
}

// passkeyStatus tells the UI whether passkeys work and at which address.
type passkeyStatus struct {
	Available bool   `json:"available"`
	RPID      string `json:"rpId,omitempty"`
	Origin    string `json:"origin,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func (s *Server) passkeyStatus(ctx context.Context) passkeyStatus {
	rp, err := s.passkeyRP(ctx)
	if err != nil {
		return passkeyStatus{Reason: sentence(err)}
	}
	return passkeyStatus{Available: true, RPID: rp.ID, Origin: rp.Origin}
}

// requirePasskeyRP answers 409 when passkeys are unavailable.
func (s *Server) requirePasskeyRP(w http.ResponseWriter, r *http.Request) (auth.RelyingParty, bool) {
	rp, err := s.passkeyRP(r.Context())
	if err != nil {
		s.authErr(w, err)
		return rp, false
	}
	return rp, true
}

// ---- sign-in (public) ------------------------------------------------------

func (s *Server) passkeyLoginOptions(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.requirePasskeyRP(w, r)
	if !ok {
		return
	}
	c, err := s.auth.PasskeyLoginOptions(clientIP(r), rp)
	if err != nil {
		s.authErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// passkeyLogin is the passwordless sign-in. The second factor after a password goes
// through POST /api/login with a passkey instead of the code.
func (s *Server) passkeyLogin(w http.ResponseWriter, r *http.Request) {
	var req auth.PasskeyAssertion
	if !readJSON(w, r, &req) {
		return
	}
	tok, u, err := s.auth.PasskeyLogin(r.Context(), clientOf(r), req)
	if err != nil {
		var refused auth.RefusedError
		if errors.Is(err, auth.ErrInvalidCredentials) || errors.As(err, &refused) {
			actor := u.Username
			if actor == "" {
				actor = "unknown"
			}
			s.audit(r.Context(), store.AuditEntry{Actor: actor, IP: clientIP(r), Action: auditLoginFailed,
				Target: "passwordless passkey", Outcome: store.AuditFailed})
		}
		s.authErr(w, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{Actor: u.Username, IP: clientIP(r), Action: auditLogin, Target: "passwordless passkey"})
	s.setCookie(w, r, tok)
	writeJSON(w, http.StatusOK, u)
}

// ---- own passkeys ----------------------------------------------------------

func (s *Server) passkeyRegisterStart(w http.ResponseWriter, r *http.Request) {
	var req struct{ Password, Code string }
	if !readJSON(w, r, &req) {
		return
	}
	rp, ok := s.requirePasskeyRP(w, r)
	if !ok {
		return
	}
	c, err := s.auth.PasskeyRegisterStart(r.Context(), sessionTokenOf(r), req.Password, req.Code, rp)
	if err != nil {
		s.authErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ceremony     string          `json:"ceremony"`
		Name         string          `json:"name"`
		Passwordless bool            `json:"passwordless"`
		Response     json.RawMessage `json:"response"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	p, err := s.auth.PasskeyRegisterFinish(r.Context(), sessionTokenOf(r), req.Ceremony, req.Name, req.Passwordless, req.Response)
	if err != nil {
		s.authErr(w, err)
		return
	}
	setAuditTarget(r, passkeyTarget(currentUser(r), p))
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) passkeyUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := passkeyID(w, r)
	if !ok {
		return
	}
	var req struct {
		Name         string `json:"name"`
		Passwordless bool   `json:"passwordless"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	p, err := s.auth.PasskeyUpdate(r.Context(), sessionTokenOf(r), id, req.Name, req.Passwordless)
	if err != nil {
		s.authErr(w, err)
		return
	}
	setAuditTarget(r, passkeyTarget(currentUser(r), p))
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) passkeyDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := passkeyID(w, r)
	if !ok {
		return
	}
	p, err := s.auth.PasskeyDelete(r.Context(), sessionTokenOf(r), id)
	if err != nil {
		s.authErr(w, err)
		return
	}
	setAuditTarget(r, passkeyTarget(currentUser(r), p))
	w.WriteHeader(http.StatusNoContent)
}

func passkeyID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusNotFound, "Passkey not found.")
		return 0, false
	}
	return id, true
}

// passkeyTarget names the account and the passkey in the audit row, plus whether it
// signs in without a password.
func passkeyTarget(u auth.User, p auth.PasskeyInfo) string {
	t := u.Username + ": " + p.Name
	if p.Passwordless {
		t += " (passwordless)"
	}
	return t
}
