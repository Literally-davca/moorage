package notify

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMentionsDiscordAndSlack(t *testing.T) {
	rules := []MentionRule{
		{Who: []string{"role:123"}, Events: []string{MentionDown}},
		{Who: []string{"user:456"}, Events: []string{MentionDown}, Stacks: []string{"postgres"}, FirstOnly: true},
	}
	dc := Config{URL: "https://discord.example/hook", Mentions: rules}
	m := Message{Event: EventChecks, Status: "down", Stack: "postgres", Title: "DOWN: db", Logs: []string{"@everyone <@&999> hi"}}
	req, err := Render("discord", dc, m)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Content string `json:"content"`
		Allowed struct {
			Parse []string `json:"parse"`
			Roles []string `json:"roles"`
			Users []string `json:"users"`
		} `json:"allowed_mentions"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(body.Content, "<@&123> <@456>") || len(body.Allowed.Parse) != 0 ||
		strings.Join(body.Allowed.Roles, ",") != "123" || strings.Join(body.Allowed.Users, ",") != "456" {
		t.Fatalf("discord: %+v", body)
	}
	// a follow-up skips FirstOnly rules; another stack skips the stack rule
	m.Repeat = true
	if got := dc.mentionsFor(m); strings.Join(got, ",") != "role:123" {
		t.Fatalf("repeat: %v", got)
	}
	if got := dc.mentionsFor(Message{Event: EventChecks, Status: "up"}); len(got) != 0 {
		t.Fatalf("up must not mention: %v", got)
	}
	if got := (Config{}).mentionsFor(m); got != nil {
		t.Fatal("no rules = nobody")
	}
	sc := Config{URL: "https://hooks.slack.example/x", Mentions: []MentionRule{{Who: []string{"group:S1", "here"}, Events: []string{MentionDown}}}}
	sm := Message{Event: EventChecks, Status: "down", Title: "<!channel> from a log"}
	req, err = Render("slack", sc, sm)
	if err != nil {
		t.Fatal(err)
	}
	var sb struct {
		Text string `json:"text"`
		Att  []struct {
			Fallback string `json:"fallback"`
			Title    string `json:"title"`
		} `json:"attachments"`
	}
	_ = json.Unmarshal(req.Body, &sb)
	if !strings.HasPrefix(sb.Text, "<!subteam^S1> <!here> ") || strings.Contains(sb.Text, "<!channel>") || strings.Contains(sb.Att[0].Fallback, "<!channel>") {
		t.Fatalf("slack: %s", req.Body)
	}
	for who, ok := range map[string]bool{"role:123": true, "role:abc": false, "here": true, "channel": false} {
		if ValidMention("discord", who) != ok {
			t.Errorf("discord %q", who)
		}
	}
}
