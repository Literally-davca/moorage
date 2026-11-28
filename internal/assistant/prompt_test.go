package assistant

import (
	"os"
	"strings"
	"testing"
)

// Every route the assistant may link to must exist in the web app's router.
func TestRoutesExist(t *testing.T) {
	b, err := os.ReadFile("../../web/src/lib/state.svelte.ts")
	if err != nil {
		t.Skip("web sources not available")
	}
	src := string(b)
	for _, r := range Routes {
		p := r.Path
		switch {
		case strings.HasPrefix(p, "h/"):
			continue // host routes are parsed by pattern
		case !strings.Contains(src, "'"+p+"'"):
			t.Errorf("route %q is not in the router", p)
		}
	}
	for _, rule := range []string{"Answer first", "At most 3 sentences", "no summary at the end"} {
		if !strings.Contains(SystemPrompt(), rule) { // M20D: short answers
			t.Errorf("prompt lost the rule %q", rule)
		}
	}
	if !strings.Contains(SystemPrompt(), "#/settings/notifications") {
		t.Error("route map missing from the prompt")
	}
}
