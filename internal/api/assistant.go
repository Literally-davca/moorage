package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"moorage/internal/assistant"
	"moorage/internal/host"
	"moorage/internal/netguard"
	"moorage/internal/store"
)

// ---- built-in add-on "AI assistant" (M18G) ------------------------------------------
//
// The chat lives in the browser; POST /api/assistant/chat runs the tool loop for one
// message and streams events (SSE) over the signed-in session. Tools act with the
// user's own permissions: viewers get read tools only. Tools for forbidden actions
// (delete, down, terminal, users, tokens, sign-in, self-update, restores, disk cleanup,
// .env values) don't exist. Proposals are MCP drafts with TokenName "assistant:<user>".

const (
	addonAssistant           = "assistant"
	settingAssistantJSON     = "assistant_settings"
	settingAssistantKey      = "assistant_api_key"
	assistantDraftPrefix     = "assistant:"
	assistantMaxMessages     = 40
	assistantMaxMessageBytes = 32 << 10
)

// Action levels and data levels.
const (
	levelOff     = "off"
	levelAdvise  = "advise"
	levelPropose = "propose"
	levelAuto    = "auto"

	dataNever   = "never"
	dataAsk     = "ask"
	dataAllowed = "allowed"
)

// assistantCategories are the kinds of actions the assistant can take (the rest of the
// M18G categories – checks, channels, add-ons – are not implemented yet).
var assistantCategories = []string{"stack", "compose", "generate"}

// assistantAutoNeedsConfirm: Auto for these needs a ticked confirmation in settings.
var assistantAutoNeedsConfirm = map[string]bool{"compose": true}

// assistantStackActions are the stack actions the assistant may run (never down/delete).
var assistantStackActions = map[string]bool{"start": true, "restart": true, "stop": true, "pull": true}

type assistantSettings struct {
	Provider          string            `json:"provider"`
	Model             string            `json:"model"`
	BaseURL           string            `json:"baseUrl"`
	Levels            map[string]string `json:"levels"`
	Data              map[string]string `json:"data"`
	AutoConfirmed     map[string]bool   `json:"autoConfirmed"`
	MonthlyTokenLimit int64             `json:"monthlyTokenLimit"`
	UsageMonth        string            `json:"usageMonth"`
	UsageTokens       int64             `json:"usageTokens"`
}

type assistantSettingsDTO struct {
	assistantSettings
	Enabled   bool `json:"enabled"`
	APIKeySet bool `json:"apiKeySet"`
	// FunctionCalling is false for providers/models known to lack tools (Advise only).
	Categories []string `json:"categories"`
}

func defaultAssistantSettings() assistantSettings {
	return assistantSettings{
		Provider:      assistant.ProviderAnthropic,
		Model:         assistant.DefaultModel[assistant.ProviderAnthropic],
		Levels:        map[string]string{"stack": levelPropose, "compose": levelPropose, "generate": levelPropose},
		Data:          map[string]string{"logs": dataAsk, "audit": dataAsk},
		AutoConfirmed: map[string]bool{},
	}
}

func (s *Server) loadAssistantSettings(ctx context.Context) assistantSettings {
	a := defaultAssistantSettings()
	if raw, err := s.store.GetSetting(ctx, settingAssistantJSON); err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &a)
	}
	def := defaultAssistantSettings()
	for k, v := range def.Levels {
		if a.Levels[k] == "" {
			a.Levels[k] = v
		}
	}
	for k, v := range def.Data {
		if a.Data[k] == "" {
			a.Data[k] = v
		}
	}
	if a.AutoConfirmed == nil {
		a.AutoConfirmed = map[string]bool{}
	}
	if m := time.Now().Format("2006-01"); a.UsageMonth != m {
		a.UsageMonth, a.UsageTokens = m, 0
	}
	return a
}

func (s *Server) saveAssistantSettings(ctx context.Context, a assistantSettings) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return s.store.SetSetting(ctx, settingAssistantJSON, string(b))
}

func (s *Server) getAssistantSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	on, _ := s.store.AddonEnabled(ctx, addonAssistant)
	key, _ := s.store.GetSetting(ctx, settingAssistantKey)
	writeJSON(w, http.StatusOK, assistantSettingsDTO{assistantSettings: s.loadAssistantSettings(ctx), Enabled: on, APIKeySet: key != "", Categories: assistantCategories})
}

func (s *Server) putAssistantSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		assistantSettings
		APIKey   string `json:"apiKey"`
		ClearKey bool   `json:"clearKey"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	cur := s.loadAssistantSettings(ctx)
	in := req.assistantSettings
	switch in.Provider {
	case assistant.ProviderAnthropic, assistant.ProviderOpenAI, assistant.ProviderGemini, assistant.ProviderOllama, assistant.ProviderCompat:
	default:
		writeErr(w, http.StatusBadRequest, "Unknown provider.")
		return
	}
	in.Model = strings.TrimSpace(in.Model)
	if len(in.Model) > 120 {
		writeErr(w, http.StatusBadRequest, "Model name is too long.")
		return
	}
	in.BaseURL = strings.TrimSpace(in.BaseURL)
	if in.BaseURL != "" {
		u, err := url.Parse(in.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			writeErr(w, http.StatusBadRequest, "The endpoint must be an http(s) URL.")
			return
		}
	}
	if in.Provider == assistant.ProviderCompat && in.BaseURL == "" {
		writeErr(w, http.StatusBadRequest, "Set the endpoint URL of the OpenAI-compatible provider.")
		return
	}
	for _, c := range assistantCategories {
		switch in.Levels[c] {
		case levelOff, levelAdvise, levelPropose:
		case levelAuto:
			if assistantAutoNeedsConfirm[c] && !in.AutoConfirmed[c] {
				writeErr(w, http.StatusBadRequest, "Confirm the warning to let the assistant change compose files on its own.")
				return
			}
		default:
			writeErr(w, http.StatusBadRequest, "Unknown action level.")
			return
		}
	}
	for _, d := range []string{"logs", "audit"} {
		switch in.Data[d] {
		case dataNever, dataAsk, dataAllowed:
		default:
			writeErr(w, http.StatusBadRequest, "Unknown data level.")
			return
		}
	}
	if in.MonthlyTokenLimit < 0 {
		in.MonthlyTokenLimit = 0
	}
	in.UsageMonth, in.UsageTokens = cur.UsageMonth, cur.UsageTokens
	if err := s.saveAssistantSettings(ctx, in); err != nil {
		s.engErr(w, err)
		return
	}
	switch {
	case req.ClearKey:
		_ = s.store.SetSetting(ctx, settingAssistantKey, "")
	case strings.TrimSpace(req.APIKey) != "":
		if err := s.store.SetSetting(ctx, settingAssistantKey, strings.TrimSpace(req.APIKey)); err != nil {
			s.engErr(w, err)
			return
		}
	}
	s.getAssistantSettings(w, r)
}

// ---- chat -------------------------------------------------------------------------------

type assistantChatReq struct {
	Messages []assistant.Message `json:"messages"`
	Context  struct {
		Origin string `json:"origin"`
		Page   string `json:"page"` // location.hash
		HostID string `json:"hostId"`
		Stack  string `json:"stack"`
	} `json:"context"`
	// Allow lists data kinds the user allowed for this chat (logs, audit).
	Allow []string `json:"allow"`
}

func (s *Server) assistantChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if on, _ := s.store.AddonEnabled(ctx, addonAssistant); !on {
		writeErr(w, http.StatusForbidden, "The assistant add-on is off.")
		return
	}
	var req assistantChatReq
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Messages) == 0 || len(req.Messages) > assistantMaxMessages {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("The conversation is too long (%d messages at most). Start a new chat.", assistantMaxMessages))
		return
	}
	total := 0
	for _, m := range req.Messages {
		total += len(m.Text)
	}
	if total > assistantMaxMessageBytes*assistantMaxMessages {
		writeErr(w, http.StatusBadRequest, "The conversation is too long. Start a new chat.")
		return
	}
	set := s.loadAssistantSettings(ctx)
	if set.MonthlyTokenLimit > 0 && set.UsageTokens >= set.MonthlyTokenLimit {
		s.log.Warn("assistant monthly token limit reached", "limit", set.MonthlyTokenLimit)
		writeErr(w, http.StatusTooManyRequests, "This month's token limit of the assistant is used up. Raise it in Settings → Add-ons → AI assistant.")
		return
	}
	key, _ := s.store.GetSetting(ctx, settingAssistantKey)
	user := currentUser(r)
	tools := s.assistantTools(r, set, req)
	sess, err := assistant.NewSession(assistant.Config{
		Provider: set.Provider, Model: set.Model, BaseURL: set.BaseURL, APIKey: key,
		HTTP: &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{DialContext: netguard.DialContext(15*time.Second, true), ForceAttemptHTTP2: true}},
	}, assistant.SystemPrompt(), s.assistantContext(ctx, user.Role, req), req.Messages, toolDefs(tools))
	if err != nil {
		writeErr(w, http.StatusBadRequest, capitalize(err.Error())+".")
		return
	}

	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "Streaming is not supported.")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	emit := func(e assistant.Event) {
		b, _ := json.Marshal(e)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
	}
	in, out, err := assistant.Run(ctx, sess, tools, emit)
	if in+out > 0 {
		cur := s.loadAssistantSettings(context.WithoutCancel(ctx))
		cur.UsageTokens += in + out
		_ = s.saveAssistantSettings(context.WithoutCancel(ctx), cur)
	}
	if err != nil {
		// the status, never the conversation
		var pe *assistant.ProviderError
		if errors.As(err, &pe) {
			s.log.Warn("assistant provider error", "provider", set.Provider, "model", set.Model, "status", pe.Status)
		} else {
			s.log.Warn("assistant provider unreachable", "provider", set.Provider, "model", set.Model)
		}
		emit(assistant.Event{Type: "error", Text: err.Error()})
	}
	emit(assistant.Event{Type: "done", InTokens: in, OutTokens: out})
}

var originRe = regexp.MustCompile(`^https?://[A-Za-z0-9.\-\[\]:]+$`)

// assistantContext is the per-request part of the prompt (after the cache breakpoint).
func (s *Server) assistantContext(ctx context.Context, role string, req assistantChatReq) string {
	var b strings.Builder
	origin := strings.TrimSuffix(req.Context.Origin, "/")
	if !originRe.MatchString(origin) {
		origin = ""
	}
	fmt.Fprintf(&b, "## Session context\n- Moorage version: %s\n- Origin (links start with it): %s\n- User role: %s\n", s.version, origin, role)
	if p := req.Context.Page; len(p) < 300 {
		fmt.Fprintf(&b, "- Current page: %s\n", p)
	}
	if s.reg != nil {
		if hosts, err := s.reg.List(ctx); err == nil {
			for _, h := range hosts {
				addr, _ := s.hostLinkAddresses(ctx, h.ID)
				fmt.Fprintf(&b, "- Host %q (id %s, %s, connected %v, read-only %v, address %s)\n", h.Name, h.ID, h.Kind, h.Connected, h.ReadOnly, addr)
			}
		}
	}
	if req.Context.HostID != "" && req.Context.Stack != "" && s.reg != nil {
		if h, _, err := s.reg.Get(ctx, req.Context.HostID); err == nil {
			if pr, err := s.stackProblems(ctx, req.Context.HostID, h, req.Context.Stack); err == nil {
				for _, p := range pr.Problems {
					fmt.Fprintf(&b, "- Problem on the open stack %s: %s (%s)\n", req.Context.Stack, p.Title, p.Service)
				}
			}
		}
	}
	return b.String()
}

func toolDefs(tools map[string]assistant.Tool) []assistant.ToolDef {
	names := make([]string, 0, len(tools))
	for n := range tools {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic order keeps the prompt cache warm
	out := make([]assistant.ToolDef, 0, len(names))
	for _, n := range names {
		out = append(out, tools[n].Def)
	}
	return out
}

func str(t string, desc string) map[string]any { return map[string]any{"type": t, "description": desc} }

// assistantTools builds the tool set for this user and request.
func (s *Server) assistantTools(r *http.Request, set assistantSettings, req assistantChatReq) map[string]assistant.Tool {
	user := currentUser(r)
	admin := user.Role == "admin"
	allowed := map[string]bool{}
	for _, a := range req.Allow {
		allowed[a] = true
	}
	tools := map[string]assistant.Tool{}
	add := func(t assistant.Tool) { tools[t.Def.Name] = t }

	add(assistant.Tool{
		Def: assistant.ToolDef{Name: "list_stacks", Description: "List hosts and their stacks with status.",
			Schema: map[string]any{"hostId": str("string", "Only this host (optional).")}},
		Run: func(ctx context.Context, in json.RawMessage, _ *assistant.State, _ func(assistant.Event)) (string, error) {
			var a struct {
				HostID string `json:"hostId"`
			}
			_ = json.Unmarshal(in, &a)
			hosts, err := s.reg.List(ctx)
			if err != nil {
				return "", err
			}
			var b strings.Builder
			for _, he := range hosts {
				if a.HostID != "" && he.ID != a.HostID {
					continue
				}
				fmt.Fprintf(&b, "Host %s (id %s, connected %v, read-only %v)\n", he.Name, he.ID, he.Connected, he.ReadOnly)
				h, _, err := s.reg.Get(ctx, he.ID)
				if err != nil {
					continue
				}
				list, err := h.ListStacks(ctx)
				if err != nil {
					fmt.Fprintf(&b, "  (stacks unavailable: %v)\n", err)
					continue
				}
				for _, st := range list {
					fmt.Fprintf(&b, "  - %s: %s, %d/%d running, managed %v, own Moorage stack %v\n", st.Name, st.Status, st.Running, st.Total, st.Managed, st.Self)
				}
			}
			return b.String(), nil
		},
	})
	add(assistant.Tool{
		Def: assistant.ToolDef{Name: "get_stack", Description: "Read a stack: containers, the Problems card, compose.yaml and the names of .env variables (values are never shown).",
			Schema:   map[string]any{"hostId": str("string", "Host id."), "stack": str("string", "Stack name.")},
			Required: []string{"hostId", "stack"}},
		Run: func(ctx context.Context, in json.RawMessage, _ *assistant.State, _ func(assistant.Event)) (string, error) {
			hostID, stack, h, err := s.assistantStack(ctx, in)
			if err != nil {
				return "", err
			}
			st, err := h.GetStack(ctx, stack)
			if err != nil {
				return "", err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Stack %s on %s: managed %v, own Moorage stack %v\nContainers:\n", stack, hostID, st.Managed, st.Self)
			for _, c := range st.Containers {
				fmt.Fprintf(&b, "- %s (service %s, image %s): %s, %s %s\n", c.Name, c.Service, c.Image, c.State, c.Status, c.Health)
			}
			if pr, err := s.stackProblems(ctx, hostID, h, stack); err == nil {
				for _, p := range pr.Problems {
					fmt.Fprintf(&b, "Problem [%s] %s – %s: %s. Evidence: %s\n", p.Severity, p.Service, p.Title, p.Detail, strings.Join(p.Evidence, " | "))
				}
			}
			fmt.Fprintf(&b, ".env variables (values hidden): %s\ncompose.yaml:\n%s\n", strings.Join(envKeyNames(st.Env), ", "), maskEnvValues(st.Compose, st.Env))
			return b.String(), nil
		},
	})
	if set.Data["logs"] != dataNever {
		add(assistant.Tool{
			Foreign: true,
			Def: assistant.ToolDef{Name: "get_logs", Description: "Read the last log lines of a stack (at most 200). Values from .env and secrets are masked.",
				Schema:   map[string]any{"hostId": str("string", "Host id."), "stack": str("string", "Stack name."), "lines": str("integer", "How many lines (max 200).")},
				Required: []string{"hostId", "stack"}},
			Run: func(ctx context.Context, in json.RawMessage, _ *assistant.State, emit func(assistant.Event)) (string, error) {
				if set.Data["logs"] == dataAsk && !allowed["logs"] {
					emit(assistant.Event{Type: "permission", Permission: "logs"})
					return "The user has not allowed reading logs in this chat yet. Ask them to press Allow, then try again.", nil
				}
				var a struct {
					Lines int `json:"lines"`
				}
				_ = json.Unmarshal(in, &a)
				if a.Lines <= 0 || a.Lines > 200 {
					a.Lines = 200
				}
				_, stack, h, err := s.assistantStack(ctx, in)
				if err != nil {
					return "", err
				}
				st, _ := h.GetStack(ctx, stack)
				lines, err := h.TailLogs(ctx, stack, a.Lines)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				for _, l := range lines {
					fmt.Fprintf(&b, "%s %s: %s\n", l.Time, l.Container, l.Message)
				}
				return maskSecrets(b.String(), st.Env), nil
			},
		})
	}

	// ---- actions (admins only, per category level) ----
	if !admin {
		return tools
	}
	level := func(cat string) string { return set.Levels[cat] }
	if level("stack") != levelOff {
		add(assistant.Tool{
			Def: assistant.ToolDef{Name: "stack_action", Description: "Start, restart, stop or pull a stack. Never down or delete.",
				Schema: map[string]any{"hostId": str("string", "Host id."), "stack": str("string", "Stack name."),
					"action": map[string]any{"type": "string", "enum": []string{"start", "restart", "stop", "pull"}}},
				Required: []string{"hostId", "stack", "action"}},
			Run: func(ctx context.Context, in json.RawMessage, st *assistant.State, emit func(assistant.Event)) (string, error) {
				var a struct {
					Action string `json:"action"`
				}
				_ = json.Unmarshal(in, &a)
				if !assistantStackActions[a.Action] {
					return "", errors.New("only start, restart, stop and pull are allowed")
				}
				hostID, stack, h, err := s.assistantWritableStack(ctx, in)
				if err != nil {
					return "", err
				}
				_ = h
				d := store.MCPDraft{TokenName: assistantDraftPrefix + user.Username, Tool: "stack_action", HostID: hostID, Stack: stack, Note: a.Action}
				return s.assistantAct(ctx, r, set, "stack", st, d, assistant.Action{Kind: "stack_action", Title: capitalize(a.Action) + " stack " + stack, HostID: hostID, Stack: stack}, emit)
			},
		})
	}
	if level("compose") != levelOff {
		add(assistant.Tool{
			Def: assistant.ToolDef{Name: "propose_compose", Description: "Change compose.yaml of an existing stack: saved as a new version (and deployed when deploy is true). For new secrets write @generate(...) values. .env values can't be changed.",
				Schema: map[string]any{"hostId": str("string", "Host id."), "stack": str("string", "Stack name."),
					"compose": str("string", "The complete new compose.yaml."), "note": str("string", "Short reason."), "deploy": str("boolean", "Deploy after saving.")},
				Required: []string{"hostId", "stack", "compose"}},
			Run: func(ctx context.Context, in json.RawMessage, st *assistant.State, emit func(assistant.Event)) (string, error) {
				var a struct {
					Compose string `json:"compose"`
					Note    string `json:"note"`
					Deploy  bool   `json:"deploy"`
				}
				if err := json.Unmarshal(in, &a); err != nil || strings.TrimSpace(a.Compose) == "" {
					return "", errors.New("compose is required")
				}
				hostID, stack, h, err := s.assistantWritableStack(ctx, in)
				if err != nil {
					return "", err
				}
				cur, err := h.GetStack(ctx, stack)
				if err != nil {
					return "", err
				}
				if !cur.Managed {
					return "", errors.New("Moorage doesn't manage this stack's files")
				}
				d := store.MCPDraft{TokenName: assistantDraftPrefix + user.Username, Tool: "update_stack", HostID: hostID, Stack: stack,
					Compose: a.Compose, Env: cur.Env, EnvKeys: []string{baseMarker(cur.Compose, cur.Env)}, Note: trimNote(a.Note), Deploy: a.Deploy}
				return s.assistantAct(ctx, r, set, "compose", st, d, assistant.Action{Kind: "compose", Title: "Change compose.yaml of " + stack + deployWord(a.Deploy),
					HostID: hostID, Stack: stack, Diff: lineDiff(cur.Compose, a.Compose), Undo: "Restore the previous version in the stack's History tab."}, emit)
			},
		})
	}
	if level("generate") != levelOff {
		add(assistant.Tool{
			Def: assistant.ToolDef{Name: "create_stack", Description: "Create a new stack from a compose.yaml you wrote. Use @generate(...) for passwords and free ports.",
				Schema: map[string]any{"hostId": str("string", "Host id."), "stack": str("string", "New stack name (lowercase, digits, - and _)."),
					"compose": str("string", "The compose.yaml."), "note": str("string", "Short description."), "deploy": str("boolean", "Start it after creating.")},
				Required: []string{"hostId", "stack", "compose"}},
			Run: func(ctx context.Context, in json.RawMessage, st *assistant.State, emit func(assistant.Event)) (string, error) {
				var a struct {
					Compose string `json:"compose"`
					Note    string `json:"note"`
					Deploy  bool   `json:"deploy"`
				}
				if err := json.Unmarshal(in, &a); err != nil || strings.TrimSpace(a.Compose) == "" {
					return "", errors.New("compose is required")
				}
				hostID, stack, h, err := s.assistantWritableStack(ctx, in)
				if err != nil {
					return "", err
				}
				if _, err := h.GetStack(ctx, stack); err == nil {
					return "", errors.New("a stack with this name exists: use propose_compose")
				}
				d := store.MCPDraft{TokenName: assistantDraftPrefix + user.Username, Tool: "create_stack", HostID: hostID, Stack: stack,
					Compose: a.Compose, Note: trimNote(a.Note), Deploy: a.Deploy, CreateNew: true}
				return s.assistantAct(ctx, r, set, "generate", st, d, assistant.Action{Kind: "create", Title: "Create stack " + stack + deployWord(a.Deploy),
					HostID: hostID, Stack: stack, Diff: lineDiff("", a.Compose), Undo: "Delete the stack yourself if you don't want it."}, emit)
			},
		})
	}
	return tools
}

// assistantAct applies the category level: advise answers in words, propose creates a
// draft card, auto runs it (a proposal when foreign content was read in this turn).
func (s *Server) assistantAct(ctx context.Context, r *http.Request, set assistantSettings, cat string, st *assistant.State, d store.MCPDraft, a assistant.Action, emit func(assistant.Event)) (string, error) {
	user := currentUser(r).Username
	switch set.Levels[cat] {
	case levelAdvise:
		return "The user allows advice only for this kind of change: explain the steps and give the link instead of acting.", nil
	case levelAuto:
		if st.ReadForeign {
			a.Demoted = true
			break // a proposal: logs or outside content were read in this turn
		}
		if _, err := s.applyDraft(ctx, d, user+" via assistant"); err != nil {
			return "", err
		}
		s.auditAssistant(r, d, "assistant."+a.Kind)
		s.log.Info("assistant action done", "user", user, "kind", a.Kind, "host", d.HostID, "stack", d.Stack)
		a.Done, a.Result = true, "Done."
		emit(assistant.Event{Type: "action", Action: &a})
		return "Done: " + a.Title + ".", nil
	}
	saved, err := s.store.CreateMCPDraft(ctx, d)
	if err != nil {
		return "", err
	}
	a.DraftID = saved.ID
	s.log.Info("assistant action proposed", "user", user, "kind", a.Kind, "host", d.HostID, "stack", d.Stack, "draft", saved.ID, "demoted", a.Demoted)
	emit(assistant.Event{Type: "action", Action: &a})
	msg := "Proposed as a card in the chat; the user applies it with Apply."
	if a.Demoted {
		msg += " (Auto was turned into a proposal because outside content was read in this turn.)"
	}
	return msg, nil
}

func (s *Server) auditAssistant(r *http.Request, d store.MCPDraft, action string) {
	s.audit(context.WithoutCancel(r.Context()), store.AuditEntry{Actor: currentUser(r).Username + " via assistant", IP: clientIP(r),
		Action: action, HostID: d.HostID, Stack: d.Stack, Target: d.Tool, Outcome: store.AuditOK})
	if s.mon != nil {
		detail := "via assistant"
		if d.Tool == "stack_action" {
			detail = d.Note + " via assistant"
		}
		s.mon.StackEvent(d.HostID, d.Stack, "edit", currentUser(r).Username, detail)
	}
}

func (s *Server) assistantStack(ctx context.Context, in json.RawMessage) (string, string, host.Host, error) {
	var a struct {
		HostID string `json:"hostId"`
		Stack  string `json:"stack"`
	}
	if err := json.Unmarshal(in, &a); err != nil || a.HostID == "" || a.Stack == "" {
		return "", "", nil, errors.New("hostId and stack are required")
	}
	h, _, err := s.reg.Get(ctx, a.HostID)
	if err != nil {
		return "", "", nil, err
	}
	return a.HostID, a.Stack, h, nil
}

func (s *Server) assistantWritableStack(ctx context.Context, in json.RawMessage) (string, string, host.Host, error) {
	hostID, stack, h, err := s.assistantStack(ctx, in)
	if err != nil {
		return "", "", nil, err
	}
	if _, meta, err := s.reg.Get(ctx, hostID); err == nil && meta.ReadOnly {
		return "", "", nil, errors.New("the host is read-only: changes are off")
	}
	if stack == addonsStackName {
		return "", "", nil, errors.New("the add-ons stack is managed by Moorage")
	}
	if st, err := h.GetStack(ctx, stack); err == nil && st.Self {
		return "", "", nil, errors.New("Moorage's own stack can't be changed by the assistant")
	}
	return hostID, stack, h, nil
}

// ---- apply / reject proposals ---------------------------------------------------------

func (s *Server) assistantDraft(w http.ResponseWriter, r *http.Request) (store.MCPDraft, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "Invalid action.")
		return store.MCPDraft{}, false
	}
	d, err := s.store.MCPDraftByID(r.Context(), id)
	if err != nil || !strings.HasPrefix(d.TokenName, assistantDraftPrefix) {
		writeErr(w, http.StatusNotFound, "Action not found.")
		return store.MCPDraft{}, false
	}
	if d.Status != "pending" {
		writeErr(w, http.StatusConflict, "The action was already decided.")
		return store.MCPDraft{}, false
	}
	return d, true
}

func (s *Server) assistantApply(w http.ResponseWriter, r *http.Request) {
	d, ok := s.assistantDraft(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	// stale proposal: the stack changed since the assistant looked at it
	if d.Tool == "update_stack" && len(d.EnvKeys) == 1 && strings.HasPrefix(d.EnvKeys[0], "base:") {
		h, _, err := s.reg.Get(ctx, d.HostID)
		if err != nil {
			s.engErr(w, err)
			return
		}
		cur, err := h.GetStack(ctx, d.Stack)
		if err != nil || baseMarker(cur.Compose, cur.Env) != d.EnvKeys[0] {
			writeErr(w, http.StatusConflict, "The stack changed since this was proposed. Ask the assistant again.")
			return
		}
		d.Env = cur.Env
	}
	user := currentUser(r).Username
	if code, err := s.applyDraft(ctx, d, user+" via assistant"); err != nil {
		if code != 0 {
			writeErr(w, code, err.Error())
		} else {
			s.engErr(w, err)
		}
		return
	}
	_ = s.store.DecideMCPDraft(ctx, d.ID, "approved", user)
	s.log.Info("assistant proposal applied", "user", user, "tool", d.Tool, "host", d.HostID, "stack", d.Stack, "draft", d.ID)
	s.auditAssistant(r, d, "assistant.apply")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) assistantReject(w http.ResponseWriter, r *http.Request) {
	d, ok := s.assistantDraft(w, r)
	if !ok {
		return
	}
	_ = s.store.DecideMCPDraft(r.Context(), d.ID, "rejected", currentUser(r).Username)
	w.WriteHeader(http.StatusNoContent)
}

// ---- helpers -------------------------------------------------------------------------

func baseMarker(compose, env string) string {
	h := sha256.Sum256([]byte(compose + "\x00" + env))
	return "base:" + hex.EncodeToString(h[:8])
}

func trimNote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func deployWord(d bool) string {
	if d {
		return " and deploy"
	}
	return ""
}

func envKeyNames(env string) []string {
	var out []string
	for _, l := range strings.Split(env, "\n") {
		l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "export "))
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if k, _, ok := strings.Cut(l, "="); ok {
			out = append(out, strings.TrimSpace(k))
		}
	}
	return out
}

func envValues(env string) []string {
	var out []string
	for _, l := range strings.Split(env, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if _, v, ok := strings.Cut(l, "="); ok {
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			if len(v) >= 4 {
				out = append(out, v)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// maskEnvValues hides .env values that appear literally in a text.
func maskEnvValues(text, env string) string {
	for _, v := range envValues(env) {
		text = strings.ReplaceAll(text, v, "********")
	}
	return text
}

var secretLikeRe = regexp.MustCompile(`(?i)((?:password|passwd|pwd|secret|token|api[_-]?key|authorization)["']?\s*[:=]\s*["']?(?:bearer\s+)?)([^\s"',;]+)|(bearer\s+)([A-Za-z0-9._\-]+)|([a-z][a-z0-9+.\-]*://[^\s:/@]+:)([^\s@/]+)(@)`)

// maskSecrets hides .env values and common secret patterns in logs before they leave
// the server.
func maskSecrets(text, env string) string {
	text = maskEnvValues(text, env)
	return secretLikeRe.ReplaceAllStringFunc(text, func(m string) string {
		sm := secretLikeRe.FindStringSubmatch(m)
		switch {
		case sm[1] != "":
			return sm[1] + "********"
		case sm[3] != "":
			return sm[3] + "********"
		case sm[5] != "":
			return sm[5] + "********" + sm[7]
		}
		return m
	})
}

// lineDiff is a compact changed-lines view for the action card.
func lineDiff(a, b string) string {
	al := strings.Split(a, "\n")
	bl := strings.Split(b, "\n")
	i := 0
	for i < len(al) && i < len(bl) && al[i] == bl[i] {
		i++
	}
	ea, eb := len(al)-1, len(bl)-1
	for ea >= i && eb >= i && al[ea] == bl[eb] {
		ea--
		eb--
	}
	var out strings.Builder
	for k := i; k <= ea; k++ {
		out.WriteString("- " + al[k] + "\n")
	}
	for k := i; k <= eb; k++ {
		out.WriteString("+ " + bl[k] + "\n")
	}
	if out.Len() > 16<<10 {
		return out.String()[:16<<10] + "…"
	}
	return out.String()
}

// assistantStatus tells the shell whether to show the panel (enabled and configured).
func (s *Server) assistantStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	on, _ := s.store.AddonEnabled(ctx, addonAssistant)
	set := s.loadAssistantSettings(ctx)
	key, _ := s.store.GetSetting(ctx, settingAssistantKey)
	configured := key != "" || set.Provider == assistant.ProviderOllama || (set.Provider == assistant.ProviderCompat && set.BaseURL != "")
	writeJSON(w, http.StatusOK, map[string]any{"enabled": on, "configured": configured, "admin": currentUser(r).Role == "admin"})
}
