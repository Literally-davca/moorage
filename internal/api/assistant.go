package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"moorage/internal/assistant"
	"moorage/internal/host"
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
	// M19E: several providers and the default pair; M19I: show the model's thinking.
	Providers    []assistantProvider `json:"providers"`
	Default      assistantDefault    `json:"default"`
	ShowThinking *bool               `json:"showThinking,omitempty"`
}

func (a assistantSettings) thinking() bool { return a.ShowThinking == nil || *a.ShowThinking }

type assistantSettingsDTO struct {
	assistantSettings
	Enabled   bool `json:"enabled"`
	APIKeySet bool `json:"apiKeySet"`
	// KeysSet says which providers have a key (M19E).
	KeysSet map[string]bool `json:"keysSet"`
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
	normalizeProviders(&a)
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
	a := s.loadAssistantSettings(ctx)
	keys := map[string]bool{}
	for _, p := range a.Providers {
		keys[p.ID] = s.providerKey(ctx, p.ID) != ""
	}
	writeJSON(w, http.StatusOK, assistantSettingsDTO{assistantSettings: a, Enabled: on, APIKeySet: keys[a.Default.Provider], KeysSet: keys, Categories: assistantCategories})
}

func (s *Server) putAssistantSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		assistantSettings
		APIKey   string `json:"apiKey"`
		ClearKey bool   `json:"clearKey"`
		// M19E: keys per provider id; ClearKeys removes them.
		Keys      map[string]string `json:"keys"`
		ClearKeys []string          `json:"clearKeys"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	cur := s.loadAssistantSettings(ctx)
	in := req.assistantSettings
	if len(in.Providers) == 0 {
		// an older client: the single provider fields
		switch in.Provider {
		case assistant.ProviderAnthropic, assistant.ProviderOpenAI, assistant.ProviderGemini, assistant.ProviderOllama, assistant.ProviderCompat:
		default:
			writeErr(w, http.StatusBadRequest, "Unknown provider.")
			return
		}
		in.Providers = []assistantProvider{{ID: "default", Name: providerLabel(in.Provider), Type: in.Provider, BaseURL: strings.TrimSpace(in.BaseURL)}}
		if paidProvider(in.Provider) && strings.TrimSpace(in.Model) != "" {
			in.Providers[0].Models = []string{strings.TrimSpace(in.Model)}
		}
		in.Default = assistantDefault{Provider: "default", Model: strings.TrimSpace(in.Model)}
	}
	if msg := validateProviders(&in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	normalizeProviders(&in)
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
	if req.Keys == nil {
		req.Keys = map[string]string{}
	}
	if strings.TrimSpace(req.APIKey) != "" {
		req.Keys[in.Default.Provider] = req.APIKey
	}
	if req.ClearKey {
		req.ClearKeys = append(req.ClearKeys, in.Default.Provider)
	}
	for _, id := range req.ClearKeys {
		_ = s.store.SetSetting(ctx, settingAssistantKey+":"+id, "")
		if id == "default" || id == in.Default.Provider {
			_ = s.store.SetSetting(ctx, settingAssistantKey, "")
		}
	}
	for id, k := range req.Keys {
		if k = strings.TrimSpace(k); k == "" || in.providerByID(id) == nil {
			continue
		}
		if err := s.store.SetSetting(ctx, settingAssistantKey+":"+id, k); err != nil {
			s.engErr(w, err)
			return
		}
		if id == in.Default.Provider {
			_ = s.store.SetSetting(ctx, settingAssistantKey, k) // older versions read this
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
	// M19E: an admin may pick the provider and model of this chat.
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// M19I: banners, errors and warnings the user sees right now.
	Notices []pageNotice `json:"notices"`
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
		if len(m.Text) > assistantMaxMessageBytes {
			writeErr(w, http.StatusBadRequest, "A message is longer than 32 KiB. Shorten it or attach less log text.")
			return
		}
		total += len(m.Text)
	}
	if total > assistantMaxMessageBytes*assistantMaxMessages {
		writeErr(w, http.StatusBadRequest, "The conversation is too long. Start a new chat.")
		return
	}
	req.Notices = cleanNotices(req.Notices)
	set := s.loadAssistantSettings(ctx)
	if set.MonthlyTokenLimit > 0 && set.UsageTokens >= set.MonthlyTokenLimit {
		s.log.Warn("assistant monthly token limit reached", "limit", set.MonthlyTokenLimit)
		writeErr(w, http.StatusTooManyRequests, "This month's token limit of the assistant is used up. Raise it in Settings → Add-ons → AI assistant.")
		return
	}
	user := currentUser(r)
	prov, model, note := s.pickModel(ctx, set, user.Role == "admin", req.Provider, req.Model)
	tools := s.assistantTools(r, set, req)

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
	modelName := prov.Name + " · " + model
	emit(assistant.Event{Type: "model", Model: modelName, Text: note})

	cfg := s.providerConfig(ctx, prov, model)
	cfg.OnDelta = func(t string) { emit(assistant.Event{Type: "delta", Text: t}) }
	if set.thinking() {
		cfg.Thinking = true
		cfg.OnThinking = func(t string) { emit(assistant.Event{Type: "thinking", Text: t}) }
	}
	defs := toolDefs(tools)
	if len(defs) > 0 {
		defs = append(defs, assistant.AskChoiceTool)
	}
	sess, err := assistant.NewSession(cfg, assistant.SystemPrompt(), s.assistantContext(ctx, user.Role, req), req.Messages, defs)
	var in, out int64
	if err == nil {
		in, out, err = assistant.Run(ctx, sess, tools, emit, noticesForeign(req.Notices))
	}
	if in+out > 0 {
		cur := s.loadAssistantSettings(context.WithoutCancel(ctx))
		cur.UsageTokens += in + out
		_ = s.saveAssistantSettings(context.WithoutCancel(ctx), cur)
	}
	if err != nil && ctx.Err() == nil {
		// the status, never the conversation
		code, detail := assistant.ErrorCode(err)
		s.log.Warn("assistant provider error", "provider", prov.Type, "model", model, "code", code)
		emit(assistant.Event{Type: "error", Text: capitalize(err.Error()) + ".", Code: code, Detail: detail, Model: modelName})
	}
	emit(assistant.Event{Type: "done", InTokens: in, OutTokens: out, Model: modelName})
}

// pageNotice is a banner, error or warning the user sees right now (M19I).
type pageNotice struct {
	ID       string `json:"id"`
	Severity string `json:"severity"` // error | warning | info
	Title    string `json:"title"`
	Text     string `json:"text"`
	HostID   string `json:"hostId,omitempty"`
	Stack    string `json:"stack,omitempty"`
	Fix      string `json:"fix,omitempty"`
	// Foreign: the text quotes container output (logs); treated like logs.
	Foreign bool `json:"foreign,omitempty"`
}

// cleanNotices bounds the screen context: at most 20 notices of 2 KiB each.
func cleanNotices(in []pageNotice) []pageNotice {
	var out []pageNotice
	cut := func(s string, n int) string {
		s = strings.TrimSpace(s)
		if len(s) > n {
			return s[:n] + "…"
		}
		return s
	}
	for _, n := range in {
		if len(out) == 20 {
			break
		}
		n.Title, n.Text, n.Fix = cut(n.Title, 200), cut(n.Text, 2048), cut(n.Fix, 1024)
		switch n.Severity {
		case "error", "warning", "info":
		default:
			n.Severity = "info"
		}
		if n.Title == "" && n.Text == "" {
			continue
		}
		out = append(out, n)
	}
	return out
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
	if len(req.Notices) > 0 {
		// M19I: what the user sees; quoted container output stays marked as data
		b.WriteString("\n## Screen notices (what the user sees now)\n")
		for _, n := range req.Notices {
			where := ""
			if n.Stack != "" {
				where = " (stack " + n.Stack + ", host " + n.HostID + ")"
			}
			text := n.Text
			if n.Foreign {
				text = "<untrusted-data>" + text + "</untrusted-data>"
			}
			fmt.Fprintf(&b, "- [%s] %s%s: %s", n.Severity, n.Title, where, text)
			if n.Fix != "" {
				fmt.Fprintf(&b, " Suggested fix: %s", n.Fix)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// noticesForeign: a screen notice quotes container output, so Auto turns into Propose.
func noticesForeign(ns []pageNotice) bool {
	for _, n := range ns {
		if n.Foreign {
			return true
		}
	}
	return false
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
		Label:  "Looking at stacks",
		Target: func(json.RawMessage) string { return "stacks" },
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
		Label:  "Reading the stack",
		Target: func(in json.RawMessage) string { return "stack " + inputStack(in) + " · Problems · compose.yaml (values hidden)" },
		Def: assistant.ToolDef{Name: "get_stack", Description: "Read a stack: containers, the Problems card, compose.yaml and the names of .env variables (values are never shown).",
			Schema:   map[string]any{"hostId": str("string", "Host id."), "stack": str("string", "Stack name.")},
			Required: []string{"hostId", "stack"}},
		Run: func(ctx context.Context, in json.RawMessage, ast *assistant.State, _ func(assistant.Event)) (string, error) {
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
				b.WriteString(s.problemsText(pr.Problems, set, allowed, ast, st.Env))
			}
			fmt.Fprintf(&b, ".env variables (values hidden): %s\ncompose.yaml:\n%s\n", strings.Join(envKeyNames(st.Env), ", "), maskEnvValues(st.Compose, st.Env))
			return b.String(), nil
		},
	})
	if set.Data["logs"] != dataNever {
		add(assistant.Tool{
			Foreign: true,
			Label:   "Reading logs",
			Target: func(in json.RawMessage) string {
				var a struct {
					Lines int `json:"lines"`
				}
				_ = json.Unmarshal(in, &a)
				if a.Lines <= 0 || a.Lines > 200 {
					a.Lines = 200
				}
				return fmt.Sprintf("%d log lines of %s (masked)", a.Lines, inputStack(in))
			},
			Def: assistant.ToolDef{Name: "get_logs", Description: "Read the last log lines of a stack (at most 200). Values from .env and secrets are masked.",
				Schema:   map[string]any{"hostId": str("string", "Host id."), "stack": str("string", "Stack name."), "lines": str("integer", "How many lines (max 200).")},
				Required: []string{"hostId", "stack"}},
			Run: func(ctx context.Context, in json.RawMessage, _ *assistant.State, emit func(assistant.Event)) (string, error) {
				if set.Data["logs"] == dataAsk && !allowed["logs"] {
					return "", assistant.PermissionNeeded{What: "logs"}
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

	s.addReadTools(add, set, allowed)

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
				impact := ""
				if cur, err := h.GetStack(ctx, stack); err == nil && (a.Action == "stop" || a.Action == "restart") {
					verb := "Stops"
					if a.Action == "restart" {
						verb = "Restarts"
					}
					impact = fmt.Sprintf("%s %d %s", verb, cur.Running, plural(cur.Running, "container", "containers"))
				}
				d := store.MCPDraft{TokenName: assistantDraftPrefix + user.Username, Tool: "stack_action", HostID: hostID, Stack: stack, Note: a.Action}
				return s.assistantAct(ctx, r, set, "stack", st, d, assistant.Action{Kind: "stack_action", Title: capitalize(a.Action) + " stack " + stack, HostID: hostID, Host: s.hostName(ctx, hostID), Stack: stack, Impact: impact}, emit)
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
				// M19I: Moorage's own stack gets a proposal with Apply & restart (never Auto)
				if res, handled, err := s.assistantSelfProposal(ctx, r, set, in, a.Compose, a.Note, emit); handled {
					return res, err
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
					HostID: hostID, Host: s.hostName(ctx, hostID), Stack: stack, Diff: lineDiff(cur.Compose, a.Compose), Undo: "Restore the previous version in the stack's History tab."}, emit)
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
	if d.Tool == draftSelfApply {
		s.applySelfDraft(w, r, d) // M19I
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

/// assistantStatus tells the shell whether to show the panel (enabled and configured),
// the default model and, for admins, the models they can switch to (M19E).
func (s *Server) assistantStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	on, _ := s.store.AddonEnabled(ctx, addonAssistant)
	set := s.loadAssistantSettings(ctx)
	def := set.providerByID(set.Default.Provider)
	configured := s.providerKey(ctx, def.ID) != "" || def.Type == assistant.ProviderOllama || (def.Type == assistant.ProviderCompat && def.BaseURL != "")
	admin := currentUser(r).Role == "admin"
	resp := map[string]any{
		"enabled": on, "configured": configured, "admin": admin,
		"model":        def.Name + " · " + set.Default.Model,
		"default":      set.Default,
		"showThinking": set.thinking(),
		"tokenLimit":   set.MonthlyTokenLimit,
		"tokensUsed":   set.UsageTokens,
	}
	if admin && on && r.URL.Query().Get("models") == "1" {
		type group struct {
			ID     string   `json:"id"`
			Name   string   `json:"name"`
			Models []string `json:"models"`
			Error  string   `json:"error,omitempty"`
		}
		var groups []group
		for i := range set.Providers {
			p := &set.Providers[i]
			g := group{ID: p.ID, Name: p.Name, Models: s.allowedModels(ctx, set, p)}
			if !paidProvider(p.Type) {
				g.Error = s.detectModels(ctx, p, false).err
			}
			groups = append(groups, g)
		}
		resp["models"] = groups
	}
	writeJSON(w, http.StatusOK, resp)
}
