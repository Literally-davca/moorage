package lint

import (
	"context"
	"strings"
	"testing"
)

func selfCodes(t *testing.T, compose, env string, o Options) map[string]Issue {
	t.Helper()
	out := map[string]Issue{}
	for _, is := range Lint(context.Background(), compose, env, o) {
		if strings.HasPrefix(is.Code, "self-") {
			out[is.Code] = is
		}
	}
	return out
}

const selfHostCompose = `services:
  moorage:
    image: ghcr.io/literally-davca/moorage:0.1.7
    network_mode: host
    environment:
      MOORAGE_LISTEN: "${LISTEN:-:5080}"
`

func TestSelfListenInvalid(t *testing.T) {
	got := selfCodes(t, selfHostCompose, "LISTEN=5080\n", Options{Name: "moorage", Self: &SelfLint{Service: "moorage"}})
	is, ok := got["self-listen-invalid"]
	if !ok || is.Severity != DeployError || !strings.Contains(is.Message, `":5080"`) {
		t.Fatalf("want self-listen-invalid with :5080 hint, got %+v", got)
	}
}

func TestSelfPortTakenByAddress(t *testing.T) {
	tailscale := []PortUse{{IP: "100.87.100.2", Port: 5080, Proto: "tcp", Owner: "a process on the host"}}
	// :5080 collides with a listener on one address and suggests 127.0.0.1
	got := selfCodes(t, selfHostCompose, "", Options{Name: "moorage", Running: tailscale, Self: &SelfLint{Service: "moorage"}})
	is, ok := got["self-port-taken"]
	if !ok || !strings.Contains(is.Message, "MOORAGE_LISTEN=127.0.0.1:5080") {
		t.Fatalf("want self-port-taken with the 127.0.0.1 hint, got %+v", got)
	}
	// 127.0.0.1:5080 doesn't collide with 100.87.100.2:5080
	got = selfCodes(t, selfHostCompose, "LISTEN=127.0.0.1:5080\n", Options{Name: "moorage", Running: tailscale, Self: &SelfLint{Service: "moorage"}})
	if _, ok := got["self-port-taken"]; ok {
		t.Fatalf("127.0.0.1 must not collide with another address: %+v", got)
	}
	// the port the running Moorage holds frees up on recreate
	got = selfCodes(t, selfHostCompose, "", Options{Name: "moorage", Running: tailscale, Self: &SelfLint{Service: "moorage", OwnPorts: []int{5080}}})
	if _, ok := got["self-port-taken"]; ok {
		t.Fatalf("own port must be ignored: %+v", got)
	}
	// the agent port defaults to :5081 on a hub
	agentTaken := []PortUse{{Port: 5081, Proto: "tcp", Owner: "a process on the host"}}
	got = selfCodes(t, selfHostCompose, "", Options{Name: "moorage", Running: agentTaken, Self: &SelfLint{Service: "moorage"}})
	if is, ok := got["self-port-taken"]; !ok || !strings.Contains(is.Message, "MOORAGE_AGENT_LISTEN") {
		t.Fatalf("want agent port taken, got %+v", got)
	}
}

func TestSelfAgentAndBridge(t *testing.T) {
	agent := `services:
  agent:
    image: ghcr.io/literally-davca/moorage:0.1.7
    network_mode: host
    environment:
      MOORAGE_HUB: hub.example.com:5081
`
	taken := []PortUse{{Port: 5080, Proto: "tcp"}, {Port: 5081, Proto: "tcp"}}
	if got := selfCodes(t, agent, "", Options{Name: "a", Running: taken, Self: &SelfLint{Service: "agent"}}); len(got) != 0 {
		t.Fatalf("an agent listens on neither port: %+v", got)
	}
	bridge := strings.Replace(selfHostCompose, "    network_mode: host\n", "", 1)
	if got := selfCodes(t, bridge, "", Options{Name: "m", Running: taken, Self: &SelfLint{Service: "moorage", HostNetUnverifiable: true}}); len(got) != 0 {
		t.Fatalf("bridge mode: ports are checked by port-taken, got %+v", got)
	}
	got := selfCodes(t, selfHostCompose, "", Options{Name: "m", Self: &SelfLint{Service: "moorage", HostNetUnverifiable: true}})
	if got["self-host-network-unverified"].Severity != Warning {
		t.Fatalf("want host network warning, got %+v", got)
	}
}

func TestParseListen(t *testing.T) {
	for in, want := range map[string]bool{":5080": true, "127.0.0.1:5080": true, "[::1]:5080": true, "off": true, "5080": false, "host": false, ":0": false} {
		if _, _, _, ok, _ := ParseListen(in); ok != want {
			t.Errorf("ParseListen(%q) ok=%v, want %v", in, ok, want)
		}
	}
}
