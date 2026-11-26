package api

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/lint"
)

// ---- generated values on save (M18E) ---------------------------------------------
//
// Every path that saves a managed stack (editor, new stack, templates, MCP, deploy of a
// stack edited outside Moorage) runs materializeStack first: @generate(...) keywords and
// Coolify magic variables become ordinary .env values, once. Existing values are never
// replaced and deploys never generate anything new except when a keyword was left in the
// files by an edit outside Moorage.

// genPortLocks serializes port choice per host so two saves at the same time can't pick
// the same free port.
var genPortLocks sync.Map // hostID -> *sync.Mutex

// maxPortSearch is how far past the preferred port the search goes.
const maxPortSearch = 1000

// materializeStack generates the values a save needs. ok is false when nothing was done
// (no keywords, a Coolify stack); err is meant for the admin (bad keyword, no free port).
func (s *Server) materializeStack(ctx context.Context, hostID string, h host.Host, name, compose, env string) (lint.GenResult, error) {
	if !lint.HasKeywords(compose, env) {
		return lint.GenResult{Compose: compose, Env: env}, nil
	}
	mu, _ := genPortLocks.LoadOrStore(hostID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	taken := map[int]bool{}
	portsKnown := false
	if rep, err := s.hostPorts(ctx, hostID, h, true); err == nil {
		portsKnown = true
		for _, p := range portUses(rep, name) {
			taken[p.Port] = true
		}
	}
	// ports this compose publishes literally count as taken too
	for _, d := range lintDeclared(ctx, name, compose, env) {
		taken[d] = true
	}
	o := lint.GenOptions{}
	o.HostAddress, _ = s.hostLinkAddresses(ctx, hostID)
	if portsKnown {
		o.FreePort = func(pref int) (int, error) {
			if pref <= 0 {
				pref = 8000
			}
			for p := pref; p <= pref+maxPortSearch && p <= 65535; p++ {
				if p < 1024 && pref >= 1024 {
					continue
				}
				if !taken[p] {
					taken[p] = true
					return p, nil
				}
			}
			return 0, fmt.Errorf("no free host port from %d to %d – see Ports for what uses them", pref, min(pref+maxPortSearch, 65535))
		}
	}
	res, err := lint.Materialize(compose, env, o)
	if err == nil && len(res.Generated) > 0 {
		keys := make([]string, 0, len(res.Generated))
		for _, g := range res.Generated {
			keys = append(keys, g.Key)
		}
		s.log.Info("values generated", "host", hostID, "stack", name, "keys", strings.Join(keys, ","))
	}
	if err == nil && res.Changed {
		// the saved ports count as taken right away (port map, M17B)
		hostPortCache.mu.Lock()
		delete(hostPortCache.m, hostID)
		hostPortCache.mu.Unlock()
	}
	return res, err
}

// lintDeclared lists the literal host ports compose publishes (keywords excluded).
func lintDeclared(ctx context.Context, name, compose, env string) []int {
	if strings.Contains(compose, "@generate(") {
		return nil // keywords in ports: the loader can't read the draft yet
	}
	var out []int
	for _, d := range engine.DeclaredPorts(ctx, name, compose, env) {
		out = append(out, d.Port)
	}
	return out
}

// generatedSummary is the one-line note for the UI: "Generated: POSTGRES_PASSWORD
// (password, 32) · SERVICE_PORT_WEB = 8081 (8080 is taken)". Secrets never appear.
func generatedSummary(g []lint.GeneratedValue) string {
	if len(g) == 0 {
		return ""
	}
	parts := make([]string, 0, len(g))
	for _, v := range g {
		p := v.Key + " (" + v.Kind + ")"
		if v.Value != "" {
			p = v.Key + " = " + v.Value
		}
		if v.Note != "" {
			p += " (" + v.Note + ")"
		}
		parts = append(parts, p)
	}
	return "Generated: " + strings.Join(parts, " · ")
}

// materializeSaved generates the values a saved stack still lacks (keywords written by
// the file editor, git or by hand) and saves them as their own version before a deploy.
func (s *Server) materializeSaved(ctx context.Context, hostID string, h host.Host, name, user string) error {
	st, err := h.GetStack(ctx, name)
	if err != nil || !st.Managed || st.Self || st.Compose == "" || !lint.HasKeywords(st.Compose, st.Env) {
		return nil
	}
	if _, meta, err := s.reg.Get(ctx, hostID); err == nil && meta.ReadOnly {
		return nil
	}
	res, err := s.materializeStack(ctx, hostID, h, name, st.Compose, st.Env)
	if err != nil {
		return err
	}
	if !res.Changed {
		return nil
	}
	return h.Save(ctx, name, res.Compose, res.Env, user, false)
}
