package api

import (
	"fmt"
	"net/http"
	"testing"

	"moorage/internal/engine"
	"moorage/internal/host"
)

// Refusing to stop Moorage's own stack is a 409 with the explanation, locally and when the
// refusal comes back from an agent (the wire code turns it back into ErrSelfStack).
func TestSelfStackRefusalStatus(t *testing.T) {
	s := &Server{}
	remote := host.FromCode("self_stack", "")
	for _, err := range []error{engine.ErrSelfStack, fmt.Errorf("run: %w", engine.ErrSelfStack), remote} {
		code, msg := s.errStatus(err)
		if code != http.StatusConflict || msg == "" {
			t.Fatalf("%v: got %d %q", err, code, msg)
		}
	}
	if c, _ := host.ErrorCode(engine.ErrSelfStack); c != "self_stack" {
		t.Fatalf("wire code: %q", c)
	}
}
