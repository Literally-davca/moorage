package monitor

import (
	"context"
	"encoding/json"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/store"
)

// Limits an agent enforces on probes it runs for the hub. They live on the agent, so a
// hub (even a compromised one) can't use an agent beyond them.
const (
	MaxRemoteTimeout = 30 // seconds
	MaxRemoteProbes  = 8  // probes at once; the next one is refused as busy
	maxRemoteTarget  = 500
)

// RemoteKinds are the check kinds that can run from several locations. Stack and log
// checks never run remotely: they already run where their containers are.
var RemoteKinds = map[string]bool{KindHTTP: true, KindTCP: true, KindPing: true, KindDNS: true}

// AgentProber runs HTTP, TCP, ping and DNS probes on an agent for the hub (POST /probe).
// It uses the same probe code as the hub, so the network guard (no cloud metadata or
// link-local unless the check allows it) and the response body limit apply here too.
type AgentProber struct {
	sem chan struct{}
	run func(ctx context.Context, c store.Check) Outcome // replaceable in tests
}

// NewAgentProber creates the agent-side prober.
func NewAgentProber() *AgentProber {
	return &AgentProber{
		sem: make(chan struct{}, MaxRemoteProbes),
		run: func(ctx context.Context, c store.Check) Outcome { return Probe(ctx, c, nil) },
	}
}

// Probe validates req, applies the agent's limits and runs it.
func (p *AgentProber) Probe(ctx context.Context, req host.ProbeRequest) (host.ProbeResult, error) {
	c, err := remoteCheck(req)
	if err != nil {
		return host.ProbeResult{}, err
	}
	select {
	case p.sem <- struct{}{}:
	default:
		// Refuse at once instead of queueing: the hub counts a busy agent as no answer,
		// which is better than a late answer that looks like a timeout (a false down).
		return host.ProbeResult{}, engine.ErrBusy
	}
	defer func() { <-p.sem }()
	o := p.run(ctx, c)
	if o.cantRun {
		// This agent can't run the probe at all (no ICMP permission): not a vote.
		return host.ProbeResult{}, engine.ValidationError{Msg: o.Msg}
	}
	return host.ProbeResult{OK: o.OK, LatencyMS: int(o.Latency.Milliseconds()), Msg: o.Msg}, nil
}

// remoteCheck turns a probe request into a check definition, refusing what an agent
// must never run and capping the timeout.
func remoteCheck(req host.ProbeRequest) (store.Check, error) {
	if !RemoteKinds[req.Kind] {
		return store.Check{}, engine.ValidationError{Msg: "agents run only HTTP, TCP, ping and DNS checks; stack and log checks run where their containers are"}
	}
	if req.Target == "" || len(req.Target) > maxRemoteTarget {
		return store.Check{}, engine.ValidationError{Msg: "the check's target is missing or too long"}
	}
	settings := ""
	if len(req.Settings) > 0 && string(req.Settings) != "null" {
		var st Settings
		if err := json.Unmarshal(req.Settings, &st); err != nil {
			return store.Check{}, engine.ValidationError{Msg: "the check's settings aren't valid"}
		}
		b, _ := json.Marshal(st) // keep only the fields the probe knows
		settings = string(b)
	}
	return store.Check{Kind: req.Kind, Target: req.Target, Timeout: clampRemoteTimeout(req.Timeout), Settings: settings}, nil
}

func clampRemoteTimeout(s int) int { return min(max(s, 1), MaxRemoteTimeout) }
