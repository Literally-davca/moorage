// Package assistant runs the built-in AI assistant (M18G): a one-off chat whose history
// lives in the browser, a tool loop on the hub and provider adapters behind one
// interface. Tools and their permission checks are supplied by the API package; the
// tools for forbidden actions don't exist at all.
package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
}

// NewSession starts a conversation: a static system prompt (cached), the session
// context after it, the browser history and the tools.
func NewSession(c Config, system, sessionCtx string, history []Message, tools []ToolDef) (Session, error) {
	if c.Model == "" {
		c.Model = DefaultModel[c.Provider]
	}
	if c.Model == "" {
		return nil, errors.New("choose a model in the assistant settings")
	}
	switch c.Provider {
	case ProviderAnthropic:
		if c.APIKey == "" {
			return nil, errors.New("add an Anthropic API key in the assistant settings")
		}
		return newAnthropic(c, system, sessionCtx, history, tools), nil
	case ProviderOpenAI, ProviderGemini, ProviderOllama, ProviderCompat:
		if c.BaseURL == "" {
			c.BaseURL = defaultBase[c.Provider]
		}
		if c.BaseURL == "" {
			return nil, errors.New("set the endpoint URL in the assistant settings")
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

func newAnthropic(c Config, system, sessionCtx string, history []Message, tools []ToolDef) *anthropicSession {
	opts := []option.RequestOption{option.WithAPIKey(c.APIKey), option.WithMaxRetries(2)}
	if c.HTTP != nil {
		opts = append(opts, option.WithHTTPClient(c.HTTP))
	}
	if c.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(c.BaseURL))
	}
	s := &anthropicSession{client: anthropic.NewClient(opts...)}
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
	resp, err := s.client.Beta.Messages.New(ctx, p)
	if err != nil {
		return Turn{}, friendlyErr(err)
	}
	s.messages = append(s.messages, resp.ToParam())
	t := Turn{InTokens: resp.Usage.InputTokens + resp.Usage.CacheReadInputTokens + resp.Usage.CacheCreationInputTokens, OutTokens: resp.Usage.OutputTokens}
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

func (s *compatSession) Next(ctx context.Context) (Turn, error) {
	body := map[string]any{"model": s.c.Model, "messages": s.messages}
	if len(s.tools) > 0 {
		body["tools"] = s.tools
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(s.c.BaseURL, "/")+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return Turn{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.c.APIKey)
	}
	hc := s.c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Turn{}, fmt.Errorf("can't reach the provider: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		return Turn{}, statusErr(resp.StatusCode, raw)
	}
	var out struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return Turn{}, errors.New("the provider sent an answer Moorage can't read")
	}
	ch := out.Choices[0]
	t := Turn{Text: ch.Message.Content, InTokens: out.Usage.Prompt, OutTokens: out.Usage.Completion, Truncated: ch.FinishReason == "length"}
	msg := map[string]any{"role": "assistant", "content": ch.Message.Content}
	if len(ch.Message.ToolCalls) > 0 && !t.Truncated {
		var calls []map[string]any
		for _, tc := range ch.Message.ToolCalls {
			args := tc.Function.Arguments
			if args == "" {
				args = "{}"
			}
			t.Calls = append(t.Calls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Input: json.RawMessage(args)})
			calls = append(calls, map[string]any{"id": tc.ID, "type": "function", "function": map[string]any{"name": tc.Function.Name, "arguments": args}})
		}
		msg["tool_calls"] = calls
	}
	s.messages = append(s.messages, msg)
	return t, nil
}

func (s *compatSession) AddResults(results []ToolResult) {
	for _, r := range results {
		s.messages = append(s.messages, map[string]any{"role": "tool", "tool_call_id": r.ID, "content": r.Content})
	}
}

// ---- errors ---------------------------------------------------------------------------

// ProviderError is shown in the chat with a link to the settings.
type ProviderError struct {
	Status int
	Msg    string
}

func (e *ProviderError) Error() string { return e.Msg }

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
	return &ProviderError{Status: code, Msg: describeStatus(code, detail)}
}

func describeStatus(code int, detail string) string {
	msg := fmt.Sprintf("The provider answered %d", code)
	switch code {
	case 401, 403:
		msg = "The provider refused the API key"
	case 429:
		msg = "The provider's rate limit or credit is exhausted"
	case 404:
		msg = "The provider doesn't know this model or endpoint"
	}
	if detail != "" {
		msg += ": " + detail
	}
	return msg + "."
}

func friendlyErr(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return &ProviderError{Status: apiErr.StatusCode, Msg: describeStatus(apiErr.StatusCode, "")}
	}
	return fmt.Errorf("can't reach the provider: %w", err)
}
