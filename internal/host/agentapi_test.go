package host

import (
	"context"
	"net"
	"net/http/httptest"
	"testing"

	"moorage/internal/engine"
)

// plainHost implements only Host (the methods are never called here).
type plainHost struct{ Host }

// factsHost also reports dependency facts.
type factsHost struct {
	Host
	f engine.DepFacts
}

func (h factsHost) DepFacts(context.Context) (engine.DepFacts, error) { return h.f, nil }

func remoteFor(t *testing.T, h Host) *Remote {
	t.Helper()
	srv := httptest.NewServer(AgentHandler(h, true, "test"))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	return NewRemote(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	})
}

// The facts endpoint is read-only, so a read-only agent serves it too.
func TestAgentDepFacts(t *testing.T) {
	want := engine.DepFacts{Stacks: []engine.StackFacts{{Name: "pg", Networks: []engine.NetworkFact{{Name: "pg_default"}}}}}
	got, err := remoteFor(t, factsHost{f: want}).DepFacts(context.Background())
	if err != nil || len(got.Stacks) != 1 || got.Stacks[0].Networks[0].Name != "pg_default" {
		t.Fatalf("facts: %+v, %v", got, err)
	}
}

// A host without the capability (like an older agent) answers 404, seen as unsupported.
func TestAgentDepFactsUnsupported(t *testing.T) {
	_, err := remoteFor(t, plainHost{}).DepFacts(context.Background())
	if !IsUnsupported(err) {
		t.Fatalf("want unsupported, got %v", err)
	}
}

// notedHost records which create path the agent used.
type notedHost struct {
	Host
	note  *string
	plain *bool
}

func (h notedHost) Save(context.Context, string, string, string, string, bool) error {
	*h.plain = true
	return nil
}

func (h notedHost) CreateNoted(_ context.Context, _, _, _, _, note string) error {
	*h.note = note
	return nil
}

// The template note travels to the agent; a plain create keeps using Save.
func TestAgentCreateNoted(t *testing.T) {
	var note string
	var plain bool
	rm := writableRemote(t, notedHost{note: &note, plain: &plain})
	if err := rm.CreateNoted(context.Background(), "pg", "services: {}", "", "ann", "Created from template postgres (bundled)"); err != nil {
		t.Fatal(err)
	}
	if note != "Created from template postgres (bundled)" || plain {
		t.Fatalf("note %q plain %v", note, plain)
	}
	note = ""
	if err := rm.Save(context.Background(), "pg", "services: {}", "", "ann", true); err != nil {
		t.Fatal(err)
	}
	if note != "" || !plain {
		t.Fatalf("plain create: note %q plain %v", note, plain)
	}
}

// An older agent (a host without CreateNoted) still creates the stack, without the note.
func TestAgentCreateNotedOlderAgent(t *testing.T) {
	var plain bool
	rm := writableRemote(t, plainSaver{plain: &plain})
	if err := rm.CreateNoted(context.Background(), "pg", "services: {}", "", "ann", "note"); err != nil || !plain {
		t.Fatalf("err %v plain %v", err, plain)
	}
}

type plainSaver struct {
	Host
	plain *bool
}

func (h plainSaver) Save(_ context.Context, _, _, _, _ string, create bool) error {
	*h.plain = create
	return nil
}

func writableRemote(t *testing.T, h Host) *Remote {
	t.Helper()
	srv := httptest.NewServer(AgentHandler(h, false, "test"))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	return NewRemote(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	})
}
