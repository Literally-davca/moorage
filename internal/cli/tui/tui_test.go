package tui

import (
	"strings"
	"testing"

	"moorage/internal/cli/client"
)

func TestModelNavigationFilterAndConfirm(t *testing.T) {
	m := NewModel("unix:///data/moorage.sock", "0.1.4", "local")
	m.Hosts = []client.HostEntry{{ID: "local", Name: "Hub", Connected: true}}
	m.Stacks = []client.StackSummary{
		{Name: "traefik", Status: "up", Running: 1, Total: 1, Managed: true},
		{Name: "abc-uuid-123", DisplayName: "Plausible", Status: "up", Running: 3, Total: 3, Managed: true},
	}

	// Move down to Plausible
	cmd, redraw := m.HandleKey(KeyEvent{Kind: KeyRune, Rune: 'j'})
	if !redraw || !cmd.LoadStack || m.Selected != 1 {
		t.Fatalf("expected cursor=1 + LoadStack, got sel=%d cmd=%+v", m.Selected, cmd)
	}

	// Prompt restart -> confirm modal -> accept
	_, redraw = m.HandleKey(KeyEvent{Kind: KeyRune, Rune: 'r'})
	if !redraw || m.Confirm == nil || m.Confirm.Action != "restart" {
		t.Fatalf("expected restart confirm dialog, got %+v", m.Confirm)
	}
	cmd, _ = m.HandleKey(KeyEvent{Kind: KeyRune, Rune: 'y'})
	if cmd.RunOp == nil || cmd.RunOp.Action != "restart" || cmd.RunOp.Stack != "abc-uuid-123" {
		t.Fatalf("expected RunOp restart, got %+v", cmd.RunOp)
	}

	// Filter by Coolify DisplayName
	m.HandleKey(KeyEvent{Kind: KeyRune, Rune: '/'})
	for _, r := range "plaus" {
		m.HandleKey(KeyEvent{Kind: KeyRune, Rune: r})
	}
	if got := m.FilteredStacks(); len(got) != 1 || got[0].Name != "abc-uuid-123" {
		t.Fatalf("expected Plausible match, got %+v", got)
	}
	m.HandleKey(KeyEvent{Kind: KeyEsc})
	if m.FilterMode || m.FilterText != "" {
		t.Fatalf("expected filter cleared on Esc")
	}

	// Render both wide and compact layouts cleanly
	wide := Render(m, 120, 36)
	if !strings.Contains(wide, "Plausible") || !strings.Contains(wide, "traefik") {
		t.Fatalf("wide render missing stack names")
	}
	compact := Render(m, 72, 22)
	if !strings.Contains(compact, "Moorage CLI") {
		t.Fatalf("compact render missing header")
	}
}
