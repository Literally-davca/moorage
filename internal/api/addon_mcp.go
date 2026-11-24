package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/lint"
	"moorage/internal/store"
)

type mcpLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

var mcpRate = &mcpLimiter{hits: map[string][]time.Time{}}

func (l *mcpLimiter) allow(key string, limit int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-time.Minute)
	var kept []time.Time
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= limit {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}

func scopeRank(scope string) int {
	switch scope {
	case "deploy":
		return 3
	case "operate":
		return 2
	case "read":
		return 1
	default:
		return 0
	}
}

func matchStackPattern(pat, stack string) bool {
	if pat == "" || pat == "*" {
		return true
	}
	if ok, err := filepath.Match(pat, stack); err == nil && ok {
		return true
	}
	return pat == stack
}

func mcpAllowed(tok store.MCPToken, hostID, stack, needScope string) bool {
	need := scopeRank(needScope)
	for _, rule := range tok.ACL {
		if (rule.HostID == "*" || rule.HostID == "" || rule.HostID == hostID) &&
			(stack == "" || matchStackPattern(rule.StackPattern, stack)) &&
			scopeRank(rule.Scope) >= need {
			return true
		}
	}
	return false
}

func maskEnvText(raw string) string {
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if k, _, ok := strings.Cut(t, "="); ok {
			out = append(out, strings.TrimSpace(k)+"=********")
		}
	}
	return strings.Join(out, "\n")
}

func generateMCPSecret(kind string) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	switch kind {
	case "hex32":
		return hex.EncodeToString(b[:16])
	case "uuid":
		b[6] = (b[6] & 0x0f) | 0x40
		b[8] = (b[8] & 0x3f) | 0x80
		return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	default:
		return hex.EncodeToString(b[:18])
	}
}

func mergeWriteOnlyEnv(existingEnv string, updates map[string]string, generate map[string]string) (string, []string) {
	m := parseEnvLinesMap(existingEnv)
	var touched []string
	for k, v := range updates {
		k = strings.TrimSpace(k)
		if k == "" || v == "********" {
			continue
		}
		m[k] = v
		touched = append(touched, k)
	}
	for k, kind := range generate {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		m[k] = generateMCPSecret(kind)
		touched = append(touched, k)
	}
	var lines []string
	for k, v := range m {
		lines = append(lines, formatEnvAssignment(k, v))
	}
	out := strings.Join(lines, "\n")
	if out != "" {
		out += "\n"
	}
	return out, touched
}

// ---- Admin CRUD for MCP tokens & drafts ------------------------------------------------

func (s *Server) listMCPTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.store.MCPTokens(r.Context())
	if err != nil {
		s.engErr(w, err)
		return
	}
	if tokens == nil {
		tokens = []store.MCPToken{}
	}
	pub := strings.TrimRight(s.effectivePublicURL(r.Context()), "/")
	endpoint := "/mcp"
	if pub != "" {
		endpoint = pub + "/mcp"
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens, "endpoint": endpoint})
}

func (s *Server) createMCPToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name            string             `json:"name"`
		ACL             []store.MCPACLRule `json:"acl"`
		RequireApproval bool               `json:"requireApproval"`
		ExpiresDays     int                `json:"expiresDays"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "Token name is required.")
		return
	}
	if len(req.ACL) == 0 {
		req.ACL = []store.MCPACLRule{{HostID: "*", StackPattern: "*", Scope: "read"}}
	}
	for i := range req.ACL {
		if scopeRank(req.ACL[i].Scope) == 0 {
			writeErr(w, http.StatusBadRequest, "ACL scope must be read, operate or deploy.")
			return
		}
		if req.ACL[i].HostID == "" {
			req.ACL[i].HostID = "*"
		}
		if req.ACL[i].StackPattern == "" {
			req.ACL[i].StackPattern = "*"
		}
	}
	var exp time.Time
	if req.ExpiresDays > 0 {
		exp = time.Now().Add(time.Duration(req.ExpiresDays) * 24 * time.Hour)
	}
	plain, hash := generateRandomToken("mr_mcp_")
	tok, err := s.store.CreateMCPToken(r.Context(), req.Name, hash, req.ACL, req.RequireApproval, exp, currentUser(r).Username)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	setAuditTarget(r, tok.Name)
	writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "secret": plain})
}

func (s *Server) deleteMCPToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "Invalid token id.")
		return
	}
	if err := s.store.DeleteMCPToken(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Token not found.")
		return
	} else if err != nil {
		s.engErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listMCPDrafts(w http.ResponseWriter, r *http.Request) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	drafts, err := s.store.MCPDrafts(r.Context(), status)
	if err != nil {
		s.engErr(w, err)
		return
	}
	if drafts == nil {
		drafts = []store.MCPDraft{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"drafts": drafts})
}

func (s *Server) approveMCPDraft(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "Invalid draft id.")
		return
	}
	ctx := r.Context()
	d, err := s.store.MCPDraftByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Draft not found.")
		return
	}
	if err != nil {
		s.engErr(w, err)
		return
	}
	if d.Status != "pending" {
		writeErr(w, http.StatusConflict, "Draft was already decided.")
		return
	}

	h, meta, err := s.reg.Get(ctx, d.HostID)
	if err != nil {
		status, msg := s.errStatus(err)
		writeErr(w, status, msg)
		return
	}
	if meta.ReadOnly {
		writeErr(w, http.StatusForbidden, host.ErrHostReadOnly.Error())
		return
	}
	user := currentUser(r).Username
	author := "mcp:" + d.TokenName
	if d.Compose != "" {
		if d.CreateNew {
			if nc, ok := h.(host.NotedCreator); ok {
				err = nc.CreateNoted(ctx, d.Stack, d.Compose, d.Env, author, d.Note)
			} else {
				err = h.Save(ctx, d.Stack, d.Compose, d.Env, author, true)
			}
		} else {
			err = h.Save(ctx, d.Stack, d.Compose, d.Env, author, false)
		}
		if err != nil {
			s.engErr(w, err)
			return
		}
	}
	if d.Deploy {
		action := s.deployAction(ctx, d.HostID, d.Stack, "up", "")
		if op, oerr := h.RunAction(ctx, d.Stack, action, author); oerr != nil {
			s.engErr(w, oerr)
			return
		} else if action == "rolling" && op.ID != "" {
			s.watchRollback(h, d.HostID, d.Stack, op.ID)
		}
	}
	_ = s.store.DecideMCPDraft(ctx, d.ID, "approved", user)
	setAuditStack(r, d.Stack)
	setAuditTarget(r, d.Tool)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) rejectMCPDraft(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "Invalid draft id.")
		return
	}
	if err := s.store.DecideMCPDraft(r.Context(), id, "rejected", currentUser(r).Username); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Pending draft not found.")
		return
	} else if err != nil {
		s.engErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- Streamable HTTP JSON-RPC 2.0 MCP endpoint (/mcp) ----------------------------------

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.store == nil {
		writeErr(w, http.StatusNotFound, "Not found.")
		return
	}
	on, _ := s.store.AddonEnabled(ctx, addonMCPServer)
	if !on {
		writeErr(w, http.StatusNotFound, "Not found.")
		return
	}
	cnt, _ := s.store.ActiveMCPTokenCount(ctx)
	if cnt == 0 {
		writeErr(w, http.StatusNotFound, "Not found.")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "Use POST for Streamable HTTP MCP JSON-RPC.")
		return
	}

	authHdr := strings.TrimSpace(r.Header.Get("Authorization"))
	bearer, ok := strings.CutPrefix(authHdr, "Bearer ")
	if !ok || strings.TrimSpace(bearer) == "" {
		writeErr(w, http.StatusUnauthorized, "Missing Bearer token.")
		return
	}
	tok, err := s.store.MCPTokenByHash(ctx, hashToken(strings.TrimSpace(bearer)))
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "Invalid MCP token.")
		return
	}
	if !tok.ExpiresAt.IsZero() && time.Now().After(tok.ExpiresAt) {
		writeErr(w, http.StatusUnauthorized, "MCP token expired.")
		return
	}
	if !mcpRate.allow(tok.Name, 60) || !mcpRate.allow("ip:"+clientIP(r), 120) {
		writeErr(w, http.StatusTooManyRequests, "Rate limit exceeded.")
		return
	}
	s.store.TouchMCPToken(ctx, tok.ID)

	var req rpcReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*engine.MaxFileSize)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "Parse error"}})
		return
	}

	switch req.Method {
	case "initialize":
		writeJSON(w, http.StatusOK, map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "moorage-mcp", "version": s.version},
			},
		})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		writeJSON(w, http.StatusOK, map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  map[string]any{"tools": mcpToolSchemas()},
		})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeJSON(w, http.StatusOK, rpcErr(req.ID, -32602, "Invalid tool call params"))
			return
		}
		res, toolErr := s.execMCPTool(ctx, r, tok, p.Name, p.Arguments)
		if toolErr != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"isError": true,
					"content": []map[string]string{{"type": "text", "text": toolErr.Error()}},
				},
			})
			return
		}
		b, _ := json.MarshalIndent(res, "", "  ")
		writeJSON(w, http.StatusOK, map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]any{
				"content": []map[string]string{{"type": "text", "text": string(b)}},
			},
		})
	default:
		writeJSON(w, http.StatusOK, rpcErr(req.ID, -32601, "Method not found: "+req.Method))
	}
}

func rpcErr(id any, code int, msg string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": msg},
	}
}

func mcpToolSchemas() []map[string]any {
	t := func(name, desc string, props map[string]any, req ...string) map[string]any {
		return map[string]any{
			"name":        name,
			"description": desc,
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": props,
				"required":   req,
			},
		}
	}
	return []map[string]any{
		t("list_hosts", "List connected Docker hosts.", map[string]any{}),
		t("list_stacks", "List stacks on a host.", map[string]any{"hostId": map[string]string{"type": "string"}}, "hostId"),
		t("get_stack", "Read stack compose.yaml and masked .env keys (secrets never exposed).", map[string]any{"hostId": map[string]string{"type": "string"}, "stack": map[string]string{"type": "string"}}, "hostId", "stack"),
		t("get_logs", "Tail recent logs of a stack.", map[string]any{"hostId": map[string]string{"type": "string"}, "stack": map[string]string{"type": "string"}, "lines": map[string]string{"type": "integer"}}, "hostId", "stack"),
		t("list_checks", "List uptime checks and current status.", map[string]any{}),
		t("list_deps", "List stack dependency edges.", map[string]any{"hostId": map[string]string{"type": "string"}}, "hostId"),
		t("lint_compose", "Validate compose.yaml and .env with Moorage linter.", map[string]any{"hostId": map[string]string{"type": "string"}, "stack": map[string]string{"type": "string"}, "compose": map[string]string{"type": "string"}, "env": map[string]string{"type": "string"}}, "hostId", "stack", "compose"),
		t("stack_action", "Run restart, start, stop or pull on a stack (never down/delete, never self/add-ons).", map[string]any{"hostId": map[string]string{"type": "string"}, "stack": map[string]string{"type": "string"}, "action": map[string]string{"type": "string"}}, "hostId", "stack", "action"),
		t("check_images", "Check image updates for a stack.", map[string]any{"hostId": map[string]string{"type": "string"}, "stack": map[string]string{"type": "string"}}, "hostId", "stack"),
		t("create_stack", "Create a new managed stack (supports write-only envUpdates & generateSecrets).", map[string]any{"hostId": map[string]string{"type": "string"}, "stack": map[string]string{"type": "string"}, "compose": map[string]string{"type": "string"}, "note": map[string]string{"type": "string"}}, "hostId", "stack", "compose"),
		t("update_stack", "Update compose.yaml and write-only .env keys of a managed stack.", map[string]any{"hostId": map[string]string{"type": "string"}, "stack": map[string]string{"type": "string"}, "compose": map[string]string{"type": "string"}}, "hostId", "stack", "compose"),
		t("deploy_stack", "Deploy a managed stack (standard or rolling).", map[string]any{"hostId": map[string]string{"type": "string"}, "stack": map[string]string{"type": "string"}}, "hostId", "stack"),
		t("rollback_stack", "Restore a previous version hash of a managed stack.", map[string]any{"hostId": map[string]string{"type": "string"}, "stack": map[string]string{"type": "string"}, "hash": map[string]string{"type": "string"}}, "hostId", "stack", "hash"),
	}
}

func (s *Server) execMCPTool(ctx context.Context, r *http.Request, tok store.MCPToken, tool string, rawArgs json.RawMessage) (any, error) {
	var args map[string]any
	if len(rawArgs) > 0 {
		_ = json.Unmarshal(rawArgs, &args)
	}
	if args == nil {
		args = map[string]any{}
	}
	strArg := func(k string) string {
		v, _ := args[k].(string)
		return strings.TrimSpace(v)
	}
	hostID := strArg("hostId")
	stack := strArg("stack")

	recordAudit := func(outcome string) {
		_ = s.store.Audit(ctx, store.AuditEntry{
			At:      time.Now(),
			Actor:   "mcp:" + tok.Name,
			IP:      clientIP(r),
			Action:  "mcp." + tool,
			HostID:  hostID,
			Stack:   stack,
			Target:  tool,
			Outcome: outcome,
		})
	}

	switch tool {
	case "list_hosts":
		if !mcpAllowed(tok, "", "", "read") {
			return nil, errors.New("forbidden by MCP token ACL")
		}
		return s.reg.List(ctx)

	case "list_stacks":
		if !mcpAllowed(tok, hostID, "", "read") {
			return nil, errors.New("forbidden by MCP token ACL")
		}
		h, _, err := s.reg.Get(ctx, hostID)
		if err != nil {
			return nil, err
		}
		all, err := h.ListStacks(ctx)
		if err != nil {
			return nil, err
		}
		var visible []engine.StackSummary
		for _, sm := range all {
			if mcpAllowed(tok, hostID, sm.Name, "read") {
				visible = append(visible, sm)
			}
		}
		return visible, nil

	case "get_stack":
		if !mcpAllowed(tok, hostID, stack, "read") {
			return nil, errors.New("forbidden by MCP token ACL")
		}
		h, _, err := s.reg.Get(ctx, hostID)
		if err != nil {
			return nil, err
		}
		st, err := h.GetStack(ctx, stack)
		if err != nil {
			return nil, err
		}
		// Never expose plaintext .env secret values over MCP!
		st.Env = maskEnvText(st.Env)
		return st, nil

	case "get_logs":
		if !mcpAllowed(tok, hostID, stack, "read") {
			return nil, errors.New("forbidden by MCP token ACL")
		}
		h, _, err := s.reg.Get(ctx, hostID)
		if err != nil {
			return nil, err
		}
		n := 100
		if fv, ok := args["lines"].(float64); ok && fv > 0 {
			n = min(int(fv), 200)
		}
		return h.TailLogs(ctx, stack, n)

	case "list_checks":
		if !mcpAllowed(tok, "", "", "read") {
			return nil, errors.New("forbidden by MCP token ACL")
		}
		return s.store.Checks(ctx)

	case "list_deps":
		if !mcpAllowed(tok, hostID, "", "read") {
			return nil, errors.New("forbidden by MCP token ACL")
		}
		return s.store.StackDepsOfHost(ctx, hostID)

	case "lint_compose":
		if !mcpAllowed(tok, hostID, stack, "read") {
			return nil, errors.New("forbidden by MCP token ACL")
		}
		compose := strArg("compose")
		env := strArg("env")
		return lint.Lint(ctx, compose, env, lint.Options{Name: stack}), nil

	case "stack_action":
		action := strArg("action")
		if action != "restart" && action != "start" && action != "stop" && action != "pull" {
			return nil, errors.New("MCP stack_action only allows restart, start, stop, or pull (never down/delete)")
		}
		if stack == addonsStackName {
			return nil, errors.New("add-ons stack cannot be controlled via MCP")
		}
		if !mcpAllowed(tok, hostID, stack, "operate") {
			return nil, errors.New("forbidden by MCP token ACL")
		}
		h, meta, err := s.reg.Get(ctx, hostID)
		if err != nil {
			return nil, err
		}
		if meta.ReadOnly {
			return nil, host.ErrHostReadOnly
		}
		op, err := h.RunAction(ctx, stack, action, "mcp:"+tok.Name)
		if err != nil {
			recordAudit(store.AuditFailed)
			return nil, err
		}
		recordAudit(store.AuditOK)
		return op, nil

	case "check_images":
		if !mcpAllowed(tok, hostID, stack, "operate") {
			return nil, errors.New("forbidden by MCP token ACL")
		}
		h, _, err := s.reg.Get(ctx, hostID)
		if err != nil {
			return nil, err
		}
		return h.CheckImages(ctx, stack)

	case "create_stack", "update_stack", "deploy_stack":
		if stack == addonsStackName {
			return nil, errors.New("add-ons stack cannot be modified via MCP")
		}
		if !mcpAllowed(tok, hostID, stack, "deploy") {
			return nil, errors.New("forbidden by MCP token ACL")
		}
		h, meta, err := s.reg.Get(ctx, hostID)
		if err != nil {
			return nil, err
		}
		if meta.ReadOnly {
			return nil, host.ErrHostReadOnly
		}
		if st, serr := h.GetStack(ctx, stack); serr == nil && st.Self {
			return nil, engine.ErrSelfStack
		}

		compose := strArg("compose")
		note := strArg("note")
		envUpdates := map[string]string{}
		if rawMap, ok := args["envUpdates"].(map[string]any); ok {
			for k, v := range rawMap {
				if vs, ok := v.(string); ok {
					envUpdates[k] = vs
				}
			}
		}
		genMap := map[string]string{}
		if rawGen, ok := args["generateSecrets"].(map[string]any); ok {
			for k, v := range rawGen {
				if vs, ok := v.(string); ok {
					genMap[k] = vs
				}
			}
		}

		existingEnv := ""
		if cur, cerr := h.GetStack(ctx, stack); cerr == nil {
			existingEnv = cur.Env
		}
		nextEnv, touchedKeys := mergeWriteOnlyEnv(existingEnv, envUpdates, genMap)

		if tok.RequireApproval {
			d, derr := s.store.CreateMCPDraft(ctx, store.MCPDraft{
				TokenName: tok.Name,
				Tool:      tool,
				HostID:    hostID,
				Stack:     stack,
				Compose:   compose,
				Env:       nextEnv,
				EnvKeys:   touchedKeys,
				Note:      note,
				Deploy:    tool == "deploy_stack",
				CreateNew: tool == "create_stack",
			})
			if derr != nil {
				return nil, derr
			}
			recordAudit(store.AuditOK)
			return map[string]any{
				"status":  "pending_approval",
				"draftId": d.ID,
				"message": "Queued for human approval in Moorage Settings -> Add-ons.",
			}, nil
		}

		author := "mcp:" + tok.Name
		if tool == "create_stack" || tool == "update_stack" {
			if compose == "" {
				return nil, errors.New("compose is required")
			}
			if tool == "create_stack" {
				if nc, ok := h.(host.NotedCreator); ok && note != "" {
					err = nc.CreateNoted(ctx, stack, compose, nextEnv, author, note)
				} else {
					err = h.Save(ctx, stack, compose, nextEnv, author, true)
				}
			} else {
				err = h.Save(ctx, stack, compose, nextEnv, author, false)
			}
			if err != nil {
				recordAudit(store.AuditFailed)
				return nil, err
			}
			recordAudit(store.AuditOK)
			return map[string]any{"ok": true, "stack": stack}, nil
		}

		action := s.deployAction(ctx, hostID, stack, "up", "")
		op, err := h.RunAction(ctx, stack, action, author)
		if err != nil {
			recordAudit(store.AuditFailed)
			return nil, err
		}
		if action == "rolling" && op.ID != "" {
			s.watchRollback(h, hostID, stack, op.ID)
		}
		recordAudit(store.AuditOK)
		return op, nil

	case "rollback_stack":
		if stack == addonsStackName {
			return nil, errors.New("add-ons stack cannot be modified via MCP")
		}
		if !mcpAllowed(tok, hostID, stack, "deploy") {
			return nil, errors.New("forbidden by MCP token ACL")
		}
		h, meta, err := s.reg.Get(ctx, hostID)
		if err != nil {
			return nil, err
		}
		if meta.ReadOnly {
			return nil, host.ErrHostReadOnly
		}
		hash := strArg("hash")
		if hash == "" {
			return nil, errors.New("hash is required")
		}
		v, err := h.Restore(ctx, stack, hash, "mcp:"+tok.Name)
		if err != nil {
			recordAudit(store.AuditFailed)
			return nil, err
		}
		recordAudit(store.AuditOK)
		return v, nil

	default:
		return nil, errors.New("unknown or disallowed MCP tool: " + tool)
	}
}
