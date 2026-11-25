package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"moorage/internal/auth"
	"moorage/internal/store"
)

func hashAPIToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func (s *Server) listAPITokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.store.ListAPITokens(r.Context())
	if err != nil {
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

func (s *Server) createAPIToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string   `json:"name"`
		Role        string   `json:"role"`
		Scopes      []string `json:"scopes"`
		ExpiresDays int      `json:"expiresDays"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len([]rune(name)) > 64 {
		writeErr(w, http.StatusBadRequest, "Token name must be 1–64 characters.")
		return
	}
	role := strings.ToLower(strings.TrimSpace(req.Role))
	if role == "" {
		role = "admin"
	}
	if role != "admin" && role != "viewer" {
		writeErr(w, http.StatusBadRequest, "Role must be admin or viewer.")
		return
	}
	var scopes []string
	for _, sc := range req.Scopes {
		sc = strings.ToLower(strings.TrimSpace(sc))
		switch sc {
		case "read", "deploy", "admin":
			if !slices.Contains(scopes, sc) {
				scopes = append(scopes, sc)
			}
		case "":
		default:
			writeErr(w, http.StatusBadRequest, "Invalid scope: must be read, deploy, or admin.")
			return
		}
	}
	if len(scopes) == 0 {
		if role == "viewer" {
			scopes = []string{"read"}
		} else {
			scopes = []string{"admin"}
		}
	}
	if role == "viewer" {
		scopes = []string{"read"}
	}

	rawBytes := make([]byte, 24)
	if _, err := rand.Read(rawBytes); err != nil {
		writeErr(w, http.StatusInternalServerError, "Failed to generate token.")
		return
	}
	secret := "mrg_" + hex.EncodeToString(rawBytes)
	prefix := secret[:12]

	now := time.Now().UTC()
	var exp time.Time
	if req.ExpiresDays > 0 {
		exp = now.Add(time.Duration(req.ExpiresDays) * 24 * time.Hour)
	}

	saved, err := s.store.CreateAPIToken(r.Context(), store.APIToken{
		Name:      name,
		Prefix:    prefix,
		TokenHash: hashAPIToken(secret),
		Role:      role,
		Scopes:    scopes,
		CreatedBy: currentUser(r).Username,
		CreatedAt: now,
		ExpiresAt: exp,
	})
	if err != nil {
		s.engErr(w, err)
		return
	}
	setAuditTarget(r, saved.Name)
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":  saved,
		"secret": secret,
	})
}

func (s *Server) deleteAPIToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "Invalid token id.")
		return
	}
	if err := s.store.DeleteAPIToken(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "API token not found.")
			return
		}
		s.engErr(w, err)
		return
	}
	setAuditTarget(r, strconv.FormatInt(id, 10))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) authenticateBearer(r *http.Request) (auth.User, []string, bool) {
	hdr := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(hdr) <= 7 || !strings.EqualFold(hdr[:7], "Bearer ") {
		return auth.User{}, nil, false
	}
	secret := strings.TrimSpace(hdr[7:])
	if secret == "" {
		return auth.User{}, nil, false
	}
	tok, err := s.store.LookupAPITokenByHash(r.Context(), hashAPIToken(secret))
	if err != nil {
		return auth.User{}, nil, false
	}
	if !tok.ExpiresAt.IsZero() && time.Now().After(tok.ExpiresAt) {
		return auth.User{}, nil, false
	}
	if tok.LastUsedAt.IsZero() || time.Since(tok.LastUsedAt) > time.Minute {
		go s.store.TouchAPIToken(context.Background(), tok.ID, time.Now().UTC())
	}
	u := auth.User{
		ID:          tok.ID,
		Username:    "token:" + tok.Name,
		Role:        tok.Role,
		Source:      "token",
		DisplayName: tok.Name,
	}
	return u, tok.Scopes, true
}

func tokenAllowsRequest(scopes []string, r *http.Request, adminRequired bool) bool {
	if slices.Contains(scopes, "admin") {
		return true
	}
	isRead := r.Method == http.MethodGet || r.Method == http.MethodHead
	if slices.Contains(scopes, "deploy") {
		if isRead && !adminRequired {
			return true
		}
		p := r.URL.Path
		if strings.HasPrefix(p, "/api/hosts/") {
			if strings.Contains(p, "/stacks") || strings.Contains(p, "/coolify") ||
				strings.Contains(p, "/external") || strings.Contains(p, "/containers") ||
				strings.Contains(p, "/images/") || strings.Contains(p, "/deleted-stacks") {
				return true
			}
		}
		if p == "/api/convert" || p == "/api/image-lookup" || strings.HasPrefix(p, "/api/templates") || strings.HasPrefix(p, "/api/updates") {
			return true
		}
		return false
	}
	if slices.Contains(scopes, "read") {
		if adminRequired {
			return false
		}
		if isRead {
			return true
		}
		if r.URL.Path == "/api/convert" || strings.HasSuffix(r.URL.Path, "/validate") {
			return true
		}
		return false
	}
	return false
}
