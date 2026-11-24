package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"moorage/internal/store"
)

var sharedKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

const sharedSecretPrefix = "__MOORAGE_SHARED_SECRET_"
const sharedSecretSuffix = "__"

type sharedVarDTO struct {
	ID      int64  `json:"id"`
	Key     string `json:"key"`
	Value   string `json:"value,omitempty"`
	IsSet   bool   `json:"isSet"`
	HostID  string `json:"hostId"`
	Secret  bool   `json:"secret"`
	FromEnv bool   `json:"fromEnv,omitempty"`
}

func (s *Server) envSharedDefaults() []sharedVarDTO {
	var out []sharedVarDTO
	add := func(envKey, varKey string) {
		if v := strings.TrimSpace(s.cfg.Env[envKey]); v != "" {
			out = append(out, sharedVarDTO{
				Key: varKey, Value: v, IsSet: true, HostID: "", Secret: false, FromEnv: true,
			})
		}
	}
	add("MOORAGE_DEFAULT_TZ", "TZ")
	add("MOORAGE_ACME_EMAIL", "ACME_EMAIL")
	if s.cfg.PUID >= 0 {
		out = append(out, sharedVarDTO{Key: "PUID", Value: strconv.Itoa(s.cfg.PUID), IsSet: true, FromEnv: true})
	}
	if s.cfg.PGID >= 0 {
		out = append(out, sharedVarDTO{Key: "PGID", Value: strconv.Itoa(s.cfg.PGID), IsSet: true, FromEnv: true})
	}
	return out
}

func (s *Server) isEnvSharedKey(key, hostID string) bool {
	if hostID != "" {
		return false
	}
	for _, d := range s.envSharedDefaults() {
		if d.Key == key {
			return true
		}
	}
	return false
}

// listSharedVars returns global and per-host shared variables. Secret values are never sent
// to the browser (only isSet: true and a placeholder token for 1-click insertion).
func (s *Server) listSharedVars(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListSharedVars(r.Context())
	if err != nil {
		s.engErr(w, err)
		return
	}
	envDefs := s.envSharedDefaults()
	envKeys := map[string]bool{}
	out := make([]sharedVarDTO, 0, len(envDefs)+len(rows))
	for _, d := range envDefs {
		envKeys[d.Key] = true
		out = append(out, d)
	}
	for _, row := range rows {
		if row.HostID == "" && envKeys[row.Key] {
			continue // env wins over global DB row of the same key
		}
		dto := sharedVarDTO{
			ID:     row.ID,
			Key:    row.Key,
			HostID: row.HostID,
			Secret: row.Secret,
			IsSet:  row.Value != "",
		}
		if !row.Secret {
			dto.Value = row.Value
		}
		out = append(out, dto)
	}
	writeJSON(w, http.StatusOK, map[string]any{"vars": out})
}

func (s *Server) saveSharedVar(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key    string `json:"key"`
		Value  string `json:"value"`
		HostID string `json:"hostId"`
		Secret bool   `json:"secret"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	key := strings.TrimSpace(req.Key)
	hostID := strings.TrimSpace(req.HostID)
	if !sharedKeyRe.MatchString(key) {
		writeErr(w, http.StatusBadRequest, "Variable name must use letters, digits and underscores (starting with a letter or underscore).")
		return
	}
	if strings.ContainsAny(req.Value, "\r\n") {
		writeErr(w, http.StatusBadRequest, "Variable value must be a single line.")
		return
	}
	if s.isEnvSharedKey(key, hostID) {
		writeErr(w, http.StatusConflict, key+" is set via environment on the hub.")
		return
	}
	if hostID != "" && s.reg != nil {
		if _, _, err := s.reg.Get(r.Context(), hostID); err != nil && !errors.Is(err, store.ErrNotFound) {
			// Offline agents are still valid targets for shared vars; unknown host IDs are refused.
			if err.Error() == "unknown host" {
				writeErr(w, http.StatusNotFound, "Host not found.")
				return
			}
		}
	}
	saved, err := s.store.UpsertSharedVar(r.Context(), store.SharedVar{
		Key: key, Value: req.Value, HostID: hostID, Secret: req.Secret,
	}, true)
	if err != nil {
		s.engErr(w, err)
		return
	}
	setAuditTarget(r, key)
	dto := sharedVarDTO{ID: saved.ID, Key: saved.Key, HostID: saved.HostID, Secret: saved.Secret, IsSet: saved.Value != ""}
	if !saved.Secret {
		dto.Value = saved.Value
	}
	writeJSON(w, http.StatusOK, dto)
}

func (s *Server) deleteSharedVar(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusNotFound, "Shared variable not found.")
		return
	}
	cur, err := s.store.SharedVarByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Shared variable not found.")
		return
	}
	if err != nil {
		s.engErr(w, err)
		return
	}
	if err := s.store.DeleteSharedVar(r.Context(), id); err != nil {
		s.engErr(w, err)
		return
	}
	setAuditTarget(r, cur.Key)
	w.WriteHeader(http.StatusNoContent)
}

// resolvedSharedMap returns key -> real value for hostID (per-host overrides global, env wins over global DB).
func (s *Server) resolvedSharedMap(ctx context.Context, hostID string) map[string]store.SharedVar {
	res := map[string]store.SharedVar{}
	if s.store != nil {
		if rows, err := s.store.ListSharedVars(ctx); err == nil {
			for _, r := range rows {
				if r.HostID == "" {
					res[r.Key] = r
				}
			}
			if hostID != "" {
				for _, r := range rows {
					if r.HostID == hostID {
						res[r.Key] = r
					}
				}
			}
		}
	}
	for _, d := range s.envSharedDefaults() {
		if cur, ok := res[d.Key]; !ok || cur.HostID == "" {
			res[d.Key] = store.SharedVar{Key: d.Key, Value: d.Value}
		}
	}
	return res
}

// expandSharedSecrets replaces __MOORAGE_SHARED_SECRET_<KEY>__ placeholders in .env text
// or template values with the real stored value on the server so secrets never transit the UI.
func (s *Server) expandSharedSecrets(ctx context.Context, hostID, text string) string {
	if !strings.Contains(text, sharedSecretPrefix) {
		return text
	}
	vars := s.resolvedSharedMap(ctx, hostID)
	for k, v := range vars {
		tok := sharedSecretPrefix + k + sharedSecretSuffix
		if strings.Contains(text, tok) && v.Value != "" {
			text = strings.ReplaceAll(text, tok, v.Value)
		}
	}
	return text
}
