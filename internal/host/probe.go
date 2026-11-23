package host

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// ProbeRequest asks an agent to run one network check (http, tcp, ping or dns) from its
// own network. Settings are the check's kind-specific settings as stored on the hub.
type ProbeRequest struct {
	Kind     string          `json:"kind"`
	Target   string          `json:"target"`
	Timeout  int             `json:"timeout"` // seconds; the agent caps it
	Settings json.RawMessage `json:"settings,omitempty"`
}

// ProbeResult is the agent's answer to a ProbeRequest.
type ProbeResult struct {
	OK        bool   `json:"ok"`
	LatencyMS int    `json:"latencyMs"`
	Msg       string `json:"msg"`
}

// Prober runs probes on the agent. It enforces its own limits (kinds, timeout cap,
// concurrency) so a hub can't use an agent beyond them.
type Prober interface {
	Probe(ctx context.Context, req ProbeRequest) (ProbeResult, error)
}

// maxProbeRequest bounds the request body: a check definition is a few hundred bytes.
const maxProbeRequest = 64 << 10

// probeHandler serves POST /probe. A nil prober (MOORAGE_PROBES=false) answers with
// the probes_disabled code, so the hub can tell it apart from an older agent (404).
// Probing changes nothing on the host, so it is allowed on a read-only agent too.
func probeHandler(p Prober) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p == nil {
			agentErr(w, ErrProbesDisabled)
			return
		}
		var req ProbeRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, maxProbeRequest)).Decode(&req); err != nil {
			http.Error(w, `{"code":"internal","error":"bad request"}`, http.StatusBadRequest)
			return
		}
		reply(w)(p.Probe(r.Context(), req))
	}
}

// Probe asks the agent to run a check from its network. wait bounds the whole call
// (the check's timeout plus a margin for the tunnel). Older agents answer 404, which
// IsUnsupported recognises; an agent with probes off answers ErrProbesDisabled.
func (rm *Remote) Probe(ctx context.Context, req ProbeRequest, wait time.Duration) (ProbeResult, error) {
	var out ProbeResult
	err := rm.callTimeout(ctx, wait, http.MethodPost, "/probe", req, &out)
	return out, err
}

// IsProbesDisabled reports whether the agent refused a probe because MOORAGE_PROBES is off.
func IsProbesDisabled(err error) bool { return errors.Is(err, ErrProbesDisabled) }
