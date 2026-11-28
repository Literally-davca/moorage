package store

import (
	"context"
	"testing"
	"time"
)

func TestMCPDraftCleanup(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mk := func(name string) int64 {
		d, err := s.CreateMCPDraft(ctx, MCPDraft{TokenName: name, Tool: "update_stack", HostID: "local", Stack: "web"})
		if err != nil {
			t.Fatal(err)
		}
		return d.ID
	}
	mk("assistant:bob")
	bobby, tok := mk("assistant:bobby"), mk("claude")
	// a new chat of bob rejects only bob's proposals
	if err := s.RejectPendingMCPDrafts(ctx, "assistant:bob", time.Now().Add(time.Second), "new chat"); err != nil {
		t.Fatal(err)
	}
	pending, _ := s.MCPDrafts(ctx, "pending")
	if len(pending) != 2 || pending[0].ID != tok || pending[1].ID != bobby {
		t.Fatalf("pending after bob's new chat = %+v, want claude and bobby", pending)
	}
	// decided drafts older than the cutoff are deleted, pending ones stay
	if err := s.PruneMCPDrafts(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	all, _ := s.MCPDrafts(ctx, "")
	if len(all) != 2 {
		t.Fatalf("after prune = %d drafts, want 2 pending", len(all))
	}
}
