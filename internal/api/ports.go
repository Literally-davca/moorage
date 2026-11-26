package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/lint"
)

// ---- port map (M17B): GET /api/hosts/{host}/ports and the linter's port-taken check ----

// portCacheTTL keeps live validation cheap while typing: one collection per host and
// a few seconds, read on request only (no background loop).
const portCacheTTL = 5 * time.Second

type portCache struct {
	mu sync.Mutex
	m  map[string]cachedPorts
}

type cachedPorts struct {
	at  time.Time
	rep engine.PortReport
}

var hostPortCache = &portCache{m: map[string]cachedPorts{}}

// hostPorts collects (or reuses) the port map of a host.
func (s *Server) hostPorts(ctx context.Context, hostID string, h host.Host, fresh bool) (engine.PortReport, error) {
	if !fresh {
		hostPortCache.mu.Lock()
		c, ok := hostPortCache.m[hostID]
		hostPortCache.mu.Unlock()
		if ok && time.Since(c.at) < portCacheTTL {
			return c.rep, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var rep engine.PortReport
	var err error
	if pl, ok := h.(host.PortLister); ok {
		rep, err = pl.Ports(ctx)
	} else {
		rep, err = host.FallbackPorts(ctx, h)
	}
	if err != nil {
		return engine.PortReport{}, err
	}
	hostPortCache.mu.Lock()
	hostPortCache.m[hostID] = cachedPorts{at: time.Now(), rep: rep}
	hostPortCache.mu.Unlock()
	return rep, nil
}

func (s *Server) listHostPorts(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	rep, err := s.hostPorts(r.Context(), r.PathValue("host"), h, r.URL.Query().Get("fresh") == "1")
	if err != nil {
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// portUses turns a port map into the linter's input, leaving out the linted stack itself.
func portUses(rep engine.PortReport, except string) []lint.PortUse {
	var out []lint.PortUse
	for _, p := range rep.Ports {
		if p.Stack != "" && p.Stack == except {
			continue
		}
		if p.Source != engine.PortSourceHost && p.Source != engine.PortSourceCompose && p.State != engine.PortStateRunning {
			continue
		}
		out = append(out, lint.PortUse{IP: p.IP, Port: p.Port, Proto: p.Proto, Owner: p.Owner(), Reserved: p.State == engine.PortStateReserved})
	}
	return out
}
