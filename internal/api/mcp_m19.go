package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"moorage/internal/engine"
	"moorage/internal/store"
)

// ---- MCP additions (M19E) ----------------------------------------------------------
//
// Read tools for external agents (same data as the stack pages): service links, the
// port map, Problems, incidents, stored image updates and container stats. Plus MCP
// prompts (ready-made procedures) and resources (hosts and stacks), all filtered by the
// token's ACL. Nothing existing is renamed or removed.

func mcpM19ToolSchemas() []map[string]any {
	t := func(name, desc string, props map[string]any, req ...string) map[string]any {
		return map[string]any{"name": name, "description": desc, "inputSchema": map[string]any{"type": "object", "properties": props, "required": req}}
	}
	hs := map[string]any{"hostId": map[string]string{"type": "string"}, "stack": map[string]string{"type": "string"}}
	return []map[string]any{
		t("get_service_links", "Links to the services of a stack: default address, other addresses, and why a service has no link.", hs, "hostId", "stack"),
		t("get_ports", "The host's port map: taken host ports and who holds them.", map[string]any{"hostId": map[string]string{"type": "string"}}, "hostId"),
		t("get_problems", "The Problems card of a stack: detected problems, cause and fixes.", hs, "hostId", "stack"),
		t("list_incidents", "Recent incidents (outages) of a check with their timeline.", map[string]any{"checkId": map[string]string{"type": "integer"}}, "checkId"),
		t("list_updates", "Stored image update results (no registry query).", map[string]any{"hostId": map[string]string{"type": "string"}}),
		t("get_stack_stats", "CPU and memory of a stack's containers.", hs, "hostId", "stack"),
	}
}

// execMCPToolM19 runs the new tools; handled=false means not one of them.
func (s *Server) execMCPToolM19(ctx context.Context, tok store.MCPToken, tool string, args map[string]any) (any, bool, error) {
	str := func(k string) string {
		v, _ := args[k].(string)
		return strings.TrimSpace(v)
	}
	hostID, stack := str("hostId"), str("stack")
	deny := errors.New("forbidden by MCP token ACL")
	switch tool {
	case "get_service_links":
		if !mcpAllowed(tok, hostID, stack, "read") {
			return nil, true, deny
		}
		h, _, err := s.reg.Get(ctx, hostID)
		if err != nil {
			return nil, true, err
		}
		st, err := h.GetStack(ctx, stack)
		if err != nil {
			return nil, true, err
		}
		return s.decorateStackLinks(ctx, hostID, h, &st), true, nil
	case "get_ports":
		if !mcpAllowed(tok, hostID, "", "read") {
			return nil, true, deny
		}
		h, _, err := s.reg.Get(ctx, hostID)
		if err != nil {
			return nil, true, err
		}
		rep, err := s.hostPorts(ctx, hostID, h, false)
		return rep, true, err
	case "get_problems":
		if !mcpAllowed(tok, hostID, stack, "read") {
			return nil, true, deny
		}
		h, _, err := s.reg.Get(ctx, hostID)
		if err != nil {
			return nil, true, err
		}
		pr, err := s.stackProblems(ctx, hostID, h, stack)
		return pr, true, err
	case "list_incidents":
		id, _ := args["checkId"].(float64)
		c, err := s.store.CheckByID(ctx, int64(id))
		if err != nil || !mcpCheckAllowed(tok, c) {
			return nil, true, deny // same answer for unknown and forbidden checks
		}
		inc, err := s.store.Incidents(ctx, c.ID, 20)
		if err != nil {
			return nil, true, err
		}
		env := ""
		if s.reg != nil && c.Stack != "" {
			if h, _, herr := s.reg.Get(ctx, c.HostID); herr == nil {
				if st, serr := h.GetStack(ctx, c.Stack); serr == nil {
					env = st.Env
				}
			}
		}
		for i := range inc {
			inc[i].Context = maskSecrets(inc[i].Context, env)
		}
		return inc, true, nil
	case "list_updates":
		if !mcpAllowed(tok, hostID, "", "read") {
			return nil, true, deny
		}
		rows, err := s.store.ImageChecks(ctx, hostID, "")
		if err != nil {
			return nil, true, err
		}
		var out []store.ImageCheckRow
		for _, r := range rows {
			if mcpAllowed(tok, r.HostID, r.Stack, "read") {
				out = append(out, r)
			}
		}
		return out, true, nil
	case "get_stack_stats":
		if !mcpAllowed(tok, hostID, stack, "read") {
			return nil, true, deny
		}
		h, _, err := s.reg.Get(ctx, hostID)
		if err != nil {
			return nil, true, err
		}
		st, err := h.Stats(ctx, stack)
		if st == nil {
			st = []engine.Stat{}
		}
		return st, true, err
	}
	return nil, false, nil
}

// mcpCheckAllowed: a check bound to a stack needs that stack; one without a stack
// (plain URL or host check) only a rule for every stack.
func mcpCheckAllowed(tok store.MCPToken, c store.Check) bool {
	if c.Stack != "" {
		return mcpAllowed(tok, c.HostID, c.Stack, "read")
	}
	for _, rule := range tok.ACL {
		if (rule.HostID == "*" || rule.HostID == "" || rule.HostID == c.HostID) &&
			(rule.StackPattern == "" || rule.StackPattern == "*") && scopeRank(rule.Scope) >= scopeRank("read") {
			return true
		}
	}
	return false
}

// ---- prompts and resources ----

var mcpPrompts = []map[string]any{
	{"name": "diagnose_stack", "description": "Find out why a stack fails and how to fix it.",
		"arguments": []map[string]any{{"name": "hostId", "required": true}, {"name": "stack", "required": true}}},
	{"name": "add_service", "description": "Add a new service as a stack, with generated secrets and a free port.",
		"arguments": []map[string]any{{"name": "hostId", "required": true}, {"name": "what", "required": true, "description": "The service to add, e.g. 'Jellyfin'."}}},
}

func mcpPromptText(name string, args map[string]string) (string, bool) {
	switch name {
	case "diagnose_stack":
		return "Diagnose the stack " + args["stack"] + " on host " + args["hostId"] + ". Call get_problems first, then get_stack and get_logs if needed. " +
			"Explain the cause in plain words and propose the smallest fix (update_stack creates a draft the user approves).", true
	case "add_service":
		return "Add " + args["what"] + " on host " + args["hostId"] + " as a new stack. Call get_ports to pick a free port, write a compose.yaml with " +
			"@generate(password, 32) for secrets, and create it with create_stack (a draft the user approves).", true
	}
	return "", false
}

// mcpResources lists hosts and stacks the token may read.
func (s *Server) mcpResources(ctx context.Context, tok store.MCPToken) []map[string]any {
	out := []map[string]any{}
	hosts, err := s.reg.List(ctx)
	if err != nil {
		return out
	}
	for _, he := range hosts {
		if !mcpAllowed(tok, he.ID, "", "read") {
			continue
		}
		out = append(out, map[string]any{"uri": "moorage://hosts/" + he.ID, "name": "Host " + he.Name, "mimeType": "application/json"})
		h, _, err := s.reg.Get(ctx, he.ID)
		if err != nil {
			continue
		}
		list, err := h.ListStacks(ctx)
		if err != nil {
			continue
		}
		for _, sm := range list {
			if mcpAllowed(tok, he.ID, sm.Name, "read") {
				out = append(out, map[string]any{"uri": "moorage://hosts/" + he.ID + "/stacks/" + sm.Name, "name": "Stack " + sm.Name + " on " + he.Name, "mimeType": "application/json"})
			}
		}
	}
	return out
}

func (s *Server) mcpReadResource(ctx context.Context, tok store.MCPToken, uri string) (any, error) {
	rest, ok := strings.CutPrefix(uri, "moorage://hosts/")
	if !ok {
		return nil, errors.New("unknown resource")
	}
	hostID, stackPart, hasStack := strings.Cut(rest, "/stacks/")
	if !hasStack {
		if !mcpAllowed(tok, hostID, "", "read") {
			return nil, errors.New("forbidden by MCP token ACL")
		}
		hosts, err := s.reg.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, he := range hosts {
			if he.ID == hostID {
				return he, nil
			}
		}
		return nil, errors.New("unknown host")
	}
	if !mcpAllowed(tok, hostID, stackPart, "read") {
		return nil, errors.New("forbidden by MCP token ACL")
	}
	h, _, err := s.reg.Get(ctx, hostID)
	if err != nil {
		return nil, err
	}
	st, err := h.GetStack(ctx, stackPart)
	if err != nil {
		return nil, err
	}
	st.Env = maskEnvText(st.Env)
	links := s.decorateStackLinks(ctx, hostID, h, &st)
	return map[string]any{"stack": st, "serviceLinks": links}, nil
}

// mcpM19Methods answers prompts/* and resources/*; handled=false for other methods.
func (s *Server) mcpM19Methods(w http.ResponseWriter, ctx context.Context, tok store.MCPToken, req rpcReq) bool {
	ok := func(result any) { writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}) }
	switch req.Method {
	case "prompts/list":
		ok(map[string]any{"prompts": mcpPrompts})
	case "prompts/get":
		var p struct {
			Name      string            `json:"name"`
			Arguments map[string]string `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		text, found := mcpPromptText(p.Name, p.Arguments)
		if !found {
			writeJSON(w, http.StatusOK, rpcErr(req.ID, -32602, "Unknown prompt"))
			return true
		}
		ok(map[string]any{"messages": []map[string]any{{"role": "user", "content": map[string]string{"type": "text", "text": text}}}})
	case "resources/list":
		ok(map[string]any{"resources": s.mcpResources(ctx, tok)})
	case "resources/read":
		var p struct {
			URI string `json:"uri"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v, err := s.mcpReadResource(ctx, tok, p.URI)
		if err != nil {
			writeJSON(w, http.StatusOK, rpcErr(req.ID, -32602, err.Error()))
			return true
		}
		b, _ := json.MarshalIndent(v, "", "  ")
		ok(map[string]any{"contents": []map[string]string{{"uri": p.URI, "mimeType": "application/json", "text": string(b)}}})
	default:
		return false
	}
	return true
}
