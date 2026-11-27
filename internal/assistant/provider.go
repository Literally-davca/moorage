// Package assistant runs the built-in AI assistant (M18G): a one-off chat whose history
// lives in the browser, a tool loop on the hub and provider adapters behind one
// interface. Tools and their permission checks are supplied by the API package; the
// tools for forbidden actions don't exist at all.
package assistant

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// Providers.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
	ProviderGemini    = "gemini"
	ProviderOllama    = "ollama"
	ProviderCompat    = "openai-compatible"
)

// DefaultModel per provider.
var DefaultModel = map[string]string{
	ProviderAnthropic: "claude-opus-5-5",
	ProviderOpenAI:    "gpt-5",
	ProviderGemini:    "gemini-2.5-pro",
	ProviderOllama:    "llama3.1",
	ProviderCompat:    "",
}

// defaultBase is the OpenAI-compatible base URL of the providers that have one.
var defaultBase = map[string]string{
	ProviderOpenAI: "https://api.openai.com/v1",
	ProviderGemini: "https://generativelanguage.googleapis.com/v1beta/openai",
	ProviderOllama: "http://localhost:11434/v1",
}

// BaseURL is the endpoint a provider config uses (its own or the provider's default).
func BaseURL(provider, base string) string {
	if base != "" {
		return base
	}
	return defaultBase[provider]
}

// Message is one chat message from the browser (text only).
type Message struct {
	Role string `json:"role"` // user | assistant
	Text string `json:"text"`
}

// ToolDef describes a tool to the model.
type ToolDef struct {
	Name        string
	Description string
	Schema      map[string]any // JSON schema properties
	Required    []string
}

// ToolCall is a call the model asked for.
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult answers a call.
type ToolResult struct {
	ID      string
	Content string
	IsError bool
}

// Turn is one model response.
type Turn struct {
	Text      string
	Calls     []ToolCall
	InTokens  int64
	OutTokens int64
	Refused   bool
	Truncated bool
	// Streamed: the text already went out as deltas (M19E).
	Streamed bool
}

// Session is a conversation with one provider inside one chat request.
type Session interface {
	Next(ctx context.Context) (Turn, error)
	AddResults(results []ToolResult)
}

// Config selects and configures the provider.
type Config struct {
	Provider string
	Model    string
	BaseURL  string
	APIKey   string
	HTTP     *http.Client // netguard-aware client
	// OnDelta receives the answer text as it streams (M19E); OnThinking the model's
	// thinking where the provider returns it (M19I). Nil = no streaming display.
	OnDelta    func(string)
	OnThinking func(string)
	// Thinking asks for the model's thinking where the provider needs to be asked
	// (Anthropic); off saves tokens.
	Thinking bool
}

// NewSession starts a conversation: a static system prompt (cached), the session
// context after it, the browser history and the tools.
func NewSession(c Config, system, sessionCtx string, history []Message, tools []ToolDef) (Session, error) {
	if c.Model == "" {
		c.Model = DefaultModel[c.Provider]
	}
	if c.Model == "" {
		return nil, &ProviderError{Code: CodeModel, Msg: "choose a model in the assistant settings"}
	}
	switch c.Provider {
	case ProviderAnthropic:
		if c.APIKey == "" {
			return nil, &ProviderError{Code: CodeAuth, Msg: "add an Anthropic API key in the assistant settings"}
		}
		return newAnthropic(c, system, sessionCtx, history, tools), nil
	case ProviderOpenAI, ProviderGemini, ProviderOllama, ProviderCompat:
		c.BaseURL = BaseURL(c.Provider, c.BaseURL)
		if c.BaseURL == "" {
			return nil, &ProviderError{Code: CodeUnreachable, Msg: "set the endpoint URL in the assistant settings"}
		}
		return newCompat(c, system+"\n\n"+sessionCtx, history, tools), nil
	}
	return nil, fmt.Errorf("unknown provider %q", c.Provider)
}

// ---- Anthropic (official Go SDK) -------------------------------------------------------

type anthropicSession struct {
	client   anthropic.Client
	params   anthropic.BetaMessageNewParams
	messages []anthropic.BetaMessageParam
	c        Config
}

// fallbackModels are the models that take the server-side refusal fallback.
func takesFallback(model string) bool {
	for _, p := range []string{"claude-opus-5", "claude-fable-5", "claude-mythos-5", "claude-sonnet-5-5"} {
		if strings.HasPrefix(model, p) {
			return true
		}
	}
	return false
}

// adaptiveThinking: the model family takes thinking: {type: adaptive}.
func adaptiveThinking(model string) bool {
	return strings.HasPrefix(model, "claude-") && !strings.HasPrefix(model, "claude-3")
}

func newAnthropic(c Config, system, sessionCtx string, history []Message, tools []ToolDef) *anthropicSession {
	opts := []option.RequestOption{option.WithAPIKey(c.APIKey), option.WithMaxRetries(2)}
	if c.HTTP != nil {
		opts = append(opts, option.WithHTTPClient(c.HTTP))
	}
	if c.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(c.BaseURL))
	}
	s := &anthropicSession{client: anthropic.NewClient(opts...), c: c}
	var ts []anthropic.BetaToolUnionParam
	for _, t := range tools {
		tp := anthropic.BetaToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: anthropic.BetaToolInputSchemaParam{Properties: t.Schema, Required: t.Required},
		}
		ts = append(ts, anthropic.BetaToolUnionParam{OfTool: &tp})
	}
	s.params = anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(c.Model),
		MaxTokens: 16000,
		// static part cached; the session context after the breakpoint
		System: []anthropic.BetaTextBlockParam{
			{Text: system, CacheControl: anthropic.NewBetaCacheControlEphemeralParam()},
			{Text: sessionCtx},
		},
		Tools: ts,
	}
	if takesFallback(c.Model) {
		s.params.Betas = []anthropic.AnthropicBeta{"server-side-fallback-2026-07-01"}
		s.params.Fallbacks = anthropic.BetaFallbacksParamOfDefault()
	}
	if c.Thinking && adaptiveThinking(c.Model) {
		s.params.Thinking = anthropic.BetaThinkingConfigParamUnion{OfAdaptive: &anthropic.BetaThinkingConfigAdaptiveParam{
			Display: anthropic.BetaThinkingConfigAdaptiveDisplaySummarized,
		}}
	}
	for _, m := range history {
		if strings.TrimSpace(m.Text) == "" {
			continue
		}
		if m.Role == "assistant" {
			s.messages = append(s.messages, anthropic.BetaMessageParam{OfAssistant: &anthropic.BetaAssistantMessageParam{Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(m.Text)}}})
		} else {
			s.messages = append(s.messages, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(m.Text)))
		}
	}
	return s
}

func (s *anthropicSession) Next(ctx context.Context) (Turn, error) {
	p := s.params
	p.Messages = s.messages
	stream := s.client.Beta.Messages.NewStreaming(ctx, p)
	var resp anthropic.BetaMessage
	streamed := false
	for stream.Next() {
		ev := stream.Current()
		if err := resp.Accumulate(ev); err != nil {
			return Turn{}, friendlyErr(err)
		}
		if d, ok := ev.AsAny().(anthropic.BetaRawContentBlockDeltaEvent); ok {
			switch dd := d.Delta.AsAny().(type) {
			case anthropic.BetaTextDelta:
				if s.c.OnDelta != nil && dd.Text != "" {
					s.c.OnDelta(dd.Text)
					streamed = true
				}
			case anthropic.BetaThinkingDelta:
				if s.c.OnThinking != nil && dd.Thinking != "" {
					s.c.OnThinking(dd.Thinking)
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return Turn{}, friendlyErr(err)
	}
	s.messages = append(s.messages, resp.ToParam())
	t := Turn{InTokens: resp.Usage.InputTokens + resp.Usage.CacheReadInputTokens + resp.Usage.CacheCreationInputTokens, OutTokens: resp.Usage.OutputTokens, Streamed: streamed}
	switch resp.StopReason {
	case anthropic.BetaStopReasonRefusal:
		t.Refused = true
	case anthropic.BetaStopReasonMaxTokens:
		t.Truncated = true
	}
	for _, b := range resp.Content {
		switch v := b.AsAny().(type) {
		case anthropic.BetaTextBlock:
			t.Text += v.Text
		case anthropic.BetaToolUseBlock:
			t.Calls = append(t.Calls, ToolCall{ID: v.ID, Name: v.Name, Input: json.RawMessage(v.JSON.Input.Raw())})
		}
	}
	if t.Truncated {
		t.Calls = nil // never run a tool from a cut-off answer
	}
	return t, nil
}

func (s *anthropicSession) AddResults(results []ToolResult) {
	var blocks []anthropic.BetaContentBlockParamUnion
	for _, r := range results {
		blocks = append(blocks, anthropic.NewBetaToolResultBlock(r.ID, r.Content, r.IsError))
	}
	if len(blocks) > 0 {
		s.messages = append(s.messages, anthropic.NewBetaUserMessage(blocks...))
	}
}

// ---- OpenAI-compatible Chat Completions (OpenAI, Gemini, Ollama, OpenRouter…) --------

type compatSession struct {
	c        Config
	messages []map[string]any
	tools    []map[string]any
	// noStreamOptions: the endpoint refused stream_options (usage in the stream).
	noStreamOptions bool
}

func newCompat(c Config, system string, history []Message, tools []ToolDef) *compatSession {
	s := &compatSession{c: c, messages: []map[string]any{{"role": "system", "content": system}}}
	for _, m := range history {
		role := "user"
		if m.Role == "assistant" {
			role = "assistant"
		}
		if strings.TrimSpace(m.Text) != "" {
			s.messages = append(s.messages, map[string]any{"role": role, "content": m.Text})
		}
	}
	for _, t := range tools {
		params := map[string]any{"type": "object", "properties": t.Schema}
		if len(t.Required) > 0 {
			params["required"] = t.Required
		}
		s.tools = append(s.tools, map[string]any{"type": "function", "function": map[string]any{
			"name": t.Name, "description": t.Description, "parameters": params}})
	}
	return s
}

func (s *compatSession) httpClient() *http.Client {
	if s.c.HTTP != nil {
		return s.c.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (s *compatSession) post(ctx context.Context, body map[string]any) (*http.Response, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(s.c.BaseURL, "/")+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.c.APIKey)
	}
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, unreachableErr(s.c, err)
	}
	return resp, nil
}

func (s *compatSession) Next(ctx context.Context) (Turn, error) {
	body := map[string]any{"model": s.c.Model, "messages": s.messages, "stream": true}
	if len(s.tools) > 0 {
		body["tools"] = s.tools
	}
	if !s.noStreamOptions {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	resp, err := s.post(ctx, body)
	if err != nil {
		return Turn{}, err
	}
	if resp.StatusCode == http.StatusBadRequest && !s.noStreamOptions {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if bytes.Contains(raw, []byte("stream_options")) {
			s.noStreamOptions = true
			return s.Next(ctx)
		}
		return Turn{}, statusErr(resp.StatusCode, raw)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return Turn{}, statusErr(resp.StatusCode, raw)
	}
	var t Turn
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t, err = s.readStream(resp.Body)
	} else {
		t, err = s.readWhole(resp.Body)
	}
	if err != nil {
		return Turn{}, err
	}
	msg := map[string]any{"role": "assistant", "content": t.Text}
	if len(t.Calls) > 0 && !t.Truncated {
		var calls []map[string]any
		for _, c := range t.Calls {
			calls = append(calls, map[string]any{"id": c.ID, "type": "function", "function": map[string]any{"name": c.Name, "arguments": string(c.Input)}})
		}
		msg["tool_calls"] = calls
	} else {
		t.Calls = nil
	}
	s.messages = append(s.messages, msg)
	return t, nil
}

type compatToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type compatUsage struct {
	Prompt     int64 `json:"prompt_tokens"`
	Completion int64 `json:"completion_tokens"`
}

// readWhole parses a non-streamed answer (endpoints that ignore stream: true).
func (s *compatSession) readWhole(r io.Reader) (Turn, error) {
	raw, _ := io.ReadAll(io.LimitReader(r, 8<<20))
	var out struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string           `json:"content"`
				Reasoning        string           `json:"reasoning"`
				ReasoningContent string           `json:"reasoning_content"`
				ToolCalls        []compatToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage compatUsage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return Turn{}, &ProviderError{Code: CodeUnreachable, Msg: "the provider sent an answer Moorage can't read"}
	}
	ch := out.Choices[0]
	if th := ch.Message.Reasoning + ch.Message.ReasoningContent; th != "" && s.c.OnThinking != nil {
		s.c.OnThinking(th)
	}
	t := Turn{Text: ch.Message.Content, InTokens: out.Usage.Prompt, OutTokens: out.Usage.Completion, Truncated: ch.FinishReason == "length"}
	for _, tc := range ch.Message.ToolCalls {
		t.Calls = append(t.Calls, toolCall(tc))
	}
	return t, nil
}

func toolCall(tc compatToolCall) ToolCall {
	args := tc.Function.Arguments
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	return ToolCall{ID: tc.ID, Name: tc.Function.Name, Input: json.RawMessage(args)}
}

// readStream parses Server-Sent Events of a streamed answer: text deltas go out as they
// come, tool call fragments are assembled by index.
func (s *compatSession) readStream(r io.Reader) (Turn, error) {
	var t Turn
	var text strings.Builder
	calls := map[int]*compatToolCall{}
	var order []int
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	gotAny := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var ev struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Content          string           `json:"content"`
					Reasoning        string           `json:"reasoning"`
					ReasoningContent string           `json:"reasoning_content"`
					ToolCalls        []compatToolCall `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *compatUsage `json:"usage"`
			Error any          `json:"error"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		if ev.Error != nil {
			raw, _ := json.Marshal(map[string]any{"error": ev.Error})
			return Turn{}, statusErr(http.StatusBadGateway, raw)
		}
		gotAny = true
		if ev.Usage != nil {
			t.InTokens, t.OutTokens = ev.Usage.Prompt, ev.Usage.Completion
		}
		for _, ch := range ev.Choices {
			if ch.FinishReason == "length" {
				t.Truncated = true
			}
			if th := ch.Delta.Reasoning + ch.Delta.ReasoningContent; th != "" && s.c.OnThinking != nil {
				s.c.OnThinking(th)
			}
			if ch.Delta.Content != "" {
				text.WriteString(ch.Delta.Content)
				if s.c.OnDelta != nil {
					s.c.OnDelta(ch.Delta.Content)
					t.Streamed = true
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				c, ok := calls[tc.Index]
				if !ok {
					c = &compatToolCall{Index: tc.Index}
					calls[tc.Index] = c
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					c.ID = tc.ID
				}
				if tc.Function.Name != "" {
					c.Function.Name = tc.Function.Name
				}
				c.Function.Arguments += tc.Function.Arguments
			}
		}
	}
	if err := sc.Err(); err != nil {
		return Turn{}, &ProviderError{Code: CodeUnreachable, Msg: "the connection to the provider broke off", Detail: err.Error()}
	}
	if !gotAny {
		return Turn{}, &ProviderError{Code: CodeUnreachable, Msg: "the provider sent an empty answer"}
	}
	t.Text = text.String()
	for i, idx := range order {
		c := calls[idx]
		if c.ID == "" {
			c.ID = fmt.Sprintf("call_%d", i)
		}
		t.Calls = append(t.Calls, toolCall(*c))
	}
	return t, nil
}

func (s *compatSession) AddResults(results []ToolResult) {
	for _, r := range results {
		s.messages = append(s.messages, map[string]any{"role": "tool", "tool_call_id": r.ID, "content": r.Content})
	}
}

// ---- models and connection test (M19E) -----------------------------------------------

// ModelInfo is one model a provider offers.
type ModelInfo struct {
	ID string `json:"id"`
	// Tools is false when the provider says the model can't call functions (Ollama
	// capabilities); nil = unknown, assumed yes.
	Tools *bool `json:"tools,omitempty"`
}

// ListModels asks the provider which models it has: Ollama /api/tags (+ /api/show
// capabilities), OpenAI-compatible /models, Anthropic models API.
func ListModels(ctx context.Context, c Config) ([]ModelInfo, error) {
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	get := func(u string, hdr map[string]string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := hc.Do(req)
		if err != nil {
			return nil, unreachableErr(c, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if resp.StatusCode >= 300 {
			return nil, statusErr(resp.StatusCode, raw)
		}
		return raw, nil
	}
	switch c.Provider {
	case ProviderAnthropic:
		base := strings.TrimSuffix(c.BaseURL, "/")
		if base == "" {
			base = "https://api.anthropic.com"
		}
		raw, err := get(base+"/v1/models?limit=1000", map[string]string{"x-api-key": c.APIKey, "anthropic-version": "2023-06-01"})
		if err != nil {
			return nil, err
		}
		var out struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(raw, &out)
		var ms []ModelInfo
		for _, d := range out.Data {
			ms = append(ms, ModelInfo{ID: d.ID})
		}
		return ms, nil
	case ProviderOllama:
		root := strings.TrimSuffix(strings.TrimSuffix(BaseURL(c.Provider, c.BaseURL), "/"), "/v1")
		raw, err := get(root+"/api/tags", nil)
		if err != nil {
			return nil, err
		}
		var out struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		_ = json.Unmarshal(raw, &out)
		var ms []ModelInfo
		for _, m := range out.Models {
			ms = append(ms, ModelInfo{ID: m.Name, Tools: ollamaTools(ctx, hc, root, m.Name)})
		}
		return ms, nil
	}
	base := strings.TrimSuffix(BaseURL(c.Provider, c.BaseURL), "/")
	hdr := map[string]string{}
	if c.APIKey != "" {
		hdr["Authorization"] = "Bearer " + c.APIKey
	}
	raw, err := get(base+"/models", hdr)
	if err != nil {
		return nil, err
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &out)
	var ms []ModelInfo
	for _, d := range out.Data {
		ms = append(ms, ModelInfo{ID: strings.TrimPrefix(d.ID, "models/")})
	}
	return ms, nil
}

// ollamaTools reads /api/show capabilities; nil when Ollama doesn't say (older version).
func ollamaTools(ctx context.Context, hc *http.Client, root, model string) *bool {
	b, _ := json.Marshal(map[string]string{"model": model})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, root+"/api/show", bytes.NewReader(b))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out struct {
		Capabilities []string `json:"capabilities"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out) != nil || out.Capabilities == nil {
		return nil
	}
	has := false
	for _, c := range out.Capabilities {
		if c == "tools" {
			has = true
		}
	}
	return &has
}

// Test sends a short question without tools and reports a human error.
func Test(ctx context.Context, c Config) error {
	c.OnDelta, c.OnThinking, c.Thinking = nil, nil, false
	s, err := NewSession(c, "Answer with the single word OK.", "", []Message{{Role: "user", Text: "Say OK."}}, nil)
	if err != nil {
		return err
	}
	_, err = s.Next(ctx)
	return err
}

// ---- errors ---------------------------------------------------------------------------

// Error codes for a human sentence in the chat (M19E).
const (
	CodeUnreachable = "unreachable"
	CodeAuth        = "auth"
	CodeForbidden   = "forbidden"
	CodeRateLimit   = "rate_limit"
	CodeQuota       = "quota"
	CodeModel       = "model"
	CodeTimeout     = "timeout"
)

// ProviderError is shown in the chat with a link to the settings.
type ProviderError struct {
	Status int
	Code   string
	Msg    string
	// Detail is the technical message (shown under "Details").
	Detail string
}

func (e *ProviderError) Error() string { return e.Msg }

// ErrorCode classifies an error from a session ("" = unknown).
func ErrorCode(err error) (code, detail string) {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.Code, pe.Detail
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return CodeTimeout, err.Error()
	}
	return "", err.Error()
}

func statusErr(code int, raw []byte) error {
	var body struct {
		Error any `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	detail := ""
	switch v := body.Error.(type) {
	case string:
		detail = v
	case map[string]any:
		if m, ok := v["message"].(string); ok {
			detail = m
		}
	}
	return describeStatus(code, detail)
}

func describeStatus(code int, detail string) *ProviderError {
	e := &ProviderError{Status: code, Detail: detail, Msg: fmt.Sprintf("The provider answered %d", code)}
	low := strings.ToLower(detail)
	switch {
	case code == 401:
		e.Code, e.Msg = CodeAuth, "The provider refused the API key"
	case code == 403:
		e.Code, e.Msg = CodeForbidden, "The provider doesn't allow this key to use the model"
	case code == 402 || strings.Contains(low, "credit") || strings.Contains(low, "quota") || strings.Contains(low, "billing"):
		e.Code, e.Msg = CodeQuota, "The provider account has no credit left"
	case code == 429:
		e.Code, e.Msg = CodeRateLimit, "The provider's rate limit is reached – wait a moment"
	case code == 404 || strings.Contains(low, "model") && (strings.Contains(low, "not found") || strings.Contains(low, "does not exist")):
		e.Code, e.Msg = CodeModel, "The provider doesn't know this model or endpoint"
	case code == 408 || code == 504:
		e.Code, e.Msg = CodeTimeout, "The provider took too long to answer"
	case code >= 500:
		e.Code, e.Msg = CodeUnreachable, "The provider has a problem right now"
	}
	if e.Detail == "" {
		e.Detail = fmt.Sprintf("HTTP %d", code)
	}
	return e
}

// unreachableErr explains a failed connection; localhost from a container gets a hint.
func unreachableErr(c Config, err error) error {
	e := &ProviderError{Code: CodeUnreachable, Msg: "Moorage can't reach the provider", Detail: err.Error()}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		e.Code, e.Msg = CodeTimeout, "The provider took too long to answer"
	}
	base := strings.ToLower(BaseURL(c.Provider, c.BaseURL))
	if strings.Contains(base, "localhost") || strings.Contains(base, "127.0.0.1") || strings.Contains(base, "[::1]") {
		e.Msg += " – in a container, localhost is the container itself: use the host's address or host.docker.internal"
	}
	return e
}

func friendlyErr(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return describeStatus(apiErr.StatusCode, "")
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	return unreachableErr(Config{Provider: ProviderAnthropic}, err)
}
