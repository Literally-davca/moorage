package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An OpenAI-compatible stream: text deltas go out as they come, tool call fragments are
// assembled, reasoning goes to OnThinking and usage is read from the last chunk.
func TestCompatStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			t.Error("stream not requested")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range []string{
			`{"choices":[{"delta":{"reasoning":"hmm "}}]}`,
			`{"choices":[{"delta":{"content":"Hel"}}]}`,
			`{"choices":[{"delta":{"content":"lo"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"get_stack","arguments":"{\"stack\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"immich\"}"}}]}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	var deltas, thinking strings.Builder
	s, err := NewSession(Config{Provider: ProviderOllama, Model: "m", BaseURL: srv.URL,
		OnDelta: func(s string) { deltas.WriteString(s) }, OnThinking: func(s string) { thinking.WriteString(s) }},
		"sys", "", []Message{{Role: "user", Text: "hi"}}, []ToolDef{{Name: "get_stack"}})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := s.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if turn.Text != "Hello" || deltas.String() != "Hello" || !turn.Streamed || thinking.String() != "hmm " {
		t.Fatalf("text %q deltas %q thinking %q", turn.Text, deltas.String(), thinking.String())
	}
	if len(turn.Calls) != 1 || turn.Calls[0].Name != "get_stack" || string(turn.Calls[0].Input) != `{"stack":"immich"}` {
		t.Fatalf("calls %+v", turn.Calls)
	}
	if turn.InTokens != 10 || turn.OutTokens != 5 {
		t.Fatalf("usage %d %d", turn.InTokens, turn.OutTokens)
	}
}

func TestDescribeStatusCodes(t *testing.T) {
	for code, want := range map[int]string{401: CodeAuth, 403: CodeForbidden, 429: CodeRateLimit, 402: CodeQuota, 404: CodeModel, 504: CodeTimeout, 503: CodeUnreachable} {
		if got := describeStatus(code, "").Code; got != want {
			t.Errorf("%d: got %q want %q", code, got, want)
		}
	}
	if describeStatus(400, "Your credit balance is too low").Code != CodeQuota {
		t.Error("credit text is a quota error")
	}
	if e := unreachableErr(Config{Provider: ProviderOllama, BaseURL: "http://localhost:11434/v1"}, fmt.Errorf("refused")); !strings.Contains(e.Error(), "host.docker.internal") {
		t.Errorf("localhost hint missing: %v", e)
	}
}

type scriptSession struct {
	turns   []Turn
	results [][]ToolResult
}

func (s *scriptSession) Next(context.Context) (Turn, error) {
	if len(s.turns) == 0 {
		return Turn{}, nil
	}
	t := s.turns[0]
	s.turns = s.turns[1:]
	return t, nil
}
func (s *scriptSession) AddResults(r []ToolResult) { s.results = append(s.results, r) }

// A missing permission is not outside content: no wrapper, no flag (M19E), and
// ask_choice ends the turn with a choice event.
func TestRunPermissionAndChoice(t *testing.T) {
	var foreignSeen bool
	tools := map[string]Tool{
		"get_logs": {Def: ToolDef{Name: "get_logs"}, Foreign: true, Run: func(context.Context, json.RawMessage, *State, func(Event)) (string, error) {
			return "", PermissionNeeded{What: "logs"}
		}},
		"act": {Def: ToolDef{Name: "act"}, Run: func(_ context.Context, _ json.RawMessage, st *State, _ func(Event)) (string, error) {
			foreignSeen = st.ReadForeign
			return "ok", nil
		}},
	}
	s := &scriptSession{turns: []Turn{
		{Calls: []ToolCall{{ID: "1", Name: "get_logs", Input: json.RawMessage(`{}`)}, {ID: "2", Name: "act", Input: json.RawMessage(`{}`)}}},
		{Calls: []ToolCall{{ID: "3", Name: "ask_choice", Input: json.RawMessage(`{"question":"Which?","options":["A","B"]}`)}}},
		{Text: "never reached"},
	}}
	var events []Event
	if _, _, err := Run(context.Background(), s, tools, func(e Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if foreignSeen {
		t.Fatal("a missing permission must not set the outside-content flag")
	}
	if strings.Contains(s.results[0][0].Content, "untrusted-data") {
		t.Fatal("a missing permission must not be wrapped as untrusted data")
	}
	var perm, choice bool
	for _, e := range events {
		perm = perm || e.Type == "permission"
		choice = choice || (e.Type == "choice" && e.Choice != nil && len(e.Choice.Options) == 2)
		if e.Type == "text" && e.Text == "never reached" {
			t.Fatal("the turn must end after ask_choice")
		}
	}
	if !perm || !choice {
		t.Fatalf("events: %+v", events)
	}
	// a foreign start (screen notices quoting logs) is honoured
	s2 := &scriptSession{turns: []Turn{{Calls: []ToolCall{{ID: "1", Name: "act", Input: json.RawMessage(`{}`)}}}}}
	foreignSeen = false
	_, _, _ = Run(context.Background(), s2, tools, func(Event) {}, true)
	if !foreignSeen {
		t.Fatal("foreign start must set the flag")
	}
	if _, err := parseChoice(json.RawMessage(`{"question":"x","options":["one"]}`)); err == nil {
		t.Fatal("one option must be refused")
	}
}
