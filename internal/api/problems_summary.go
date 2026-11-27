package api

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"moorage/internal/diagnose"
	"moorage/internal/engine"
	"moorage/internal/host"
)

// ---- Problems beyond managed stacks and the Home summary (M19K) --------------------
//
// The same diagnosis for standalone containers (docker run) and, through the stack page,
// for foreign compose projects. The Home card lists problems across hosts; the hub asks
// at most once a minute and keeps the result in memory. An offline host is "problems
// unknown", never zero.

type problemItem struct {
	diagnose.Problem
	HostID   string `json:"hostId"`
	HostName string `json:"hostName"`
	Stack    string `json:"stack,omitempty"`
	// ContainerID is set for standalone containers (no stack page).
	ContainerID string `json:"containerId,omitempty"`
}

type problemsSummary struct {
	At       time.Time     `json:"at"`
	Problems []problemItem `json:"problems"`
	More     int           `json:"more"`
	Offline  []string      `json:"offline"` // host names whose problems are unknown
}

var (
	probSumMu    sync.Mutex
	probSumCache *problemsSummary
)

var severityRank = map[string]int{"error": 0, "warning": 1, "info": 2}

// containerProblems diagnoses one standalone container from its listing (no stack logs).
func containerProblems(c engine.ContainerInfo) []diagnose.Problem {
	f := engine.BasicFacts(c.Container)
	if f.State == "running" && f.Health != "unhealthy" {
		return nil
	}
	return diagnose.Diagnose([]diagnose.Facts{f}, diagnose.Context{Now: time.Now(), DiskFreePct: -1, Logs: map[string][]string{}, PortOwners: map[int]string{}})
}

// GET /api/hosts/{host}/containers/{id}/problems
func (s *Server) standaloneProblems(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	cm, ok := h.(host.ContainerManager)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "This host can't list containers.")
		return
	}
	ctrs, err := cm.Containers(r.Context())
	if err != nil {
		s.engErr(w, err)
		return
	}
	id := r.PathValue("id")
	for _, c := range ctrs {
		if c.ID == id || (len(id) >= 12 && len(c.ID) >= len(id) && c.ID[:len(id)] == id) || c.Name == id {
			ps := containerProblems(c)
			if ps == nil {
				ps = []diagnose.Problem{}
			}
			writeJSON(w, http.StatusOK, problemsDTO{Problems: ps, Ignored: []diagnose.Problem{}, Basic: true})
			return
		}
	}
	writeErr(w, http.StatusNotFound, "Container not found.")
}

// GET /api/problems – the summary across hosts (Home).
func (s *Server) problemsSummaryHandler(w http.ResponseWriter, r *http.Request) {
	probSumMu.Lock()
	cached := probSumCache
	probSumMu.Unlock()
	if cached != nil && time.Since(cached.At) < time.Minute && r.URL.Query().Get("fresh") != "1" {
		writeJSON(w, http.StatusOK, cached)
		return
	}
	sum := s.buildProblemsSummary(r.Context())
	probSumMu.Lock()
	probSumCache = sum
	probSumMu.Unlock()
	writeJSON(w, http.StatusOK, sum)
}

func (s *Server) buildProblemsSummary(ctx context.Context) *problemsSummary {
	out := &problemsSummary{At: time.Now(), Problems: []problemItem{}, Offline: []string{}}
	if s.reg == nil {
		return out
	}
	hosts, err := s.reg.List(ctx)
	if err != nil {
		return out
	}
	var all []problemItem
	for _, he := range hosts {
		if !he.Connected && he.Kind != "local" {
			out.Offline = append(out.Offline, he.Name)
			continue
		}
		h, _, err := s.reg.Get(ctx, he.ID)
		if err != nil {
			out.Offline = append(out.Offline, he.Name)
			continue
		}
		hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		list, err := h.ListStacks(hctx)
		if err != nil {
			cancel()
			out.Offline = append(out.Offline, he.Name)
			continue
		}
		for _, sm := range list {
			if sm.Total == 0 || sm.Status == engine.StatusUp || sm.Status == engine.StatusStopped {
				continue // nothing unwell to diagnose
			}
			pr, err := s.stackProblems(hctx, he.ID, h, sm.Name)
			if err != nil {
				continue
			}
			for _, p := range pr.Problems {
				p.Evidence = nil // the card shows titles; evidence stays on the stack page
				all = append(all, problemItem{Problem: p, HostID: he.ID, HostName: he.Name, Stack: sm.Name})
			}
		}
		if cm, ok := h.(host.ContainerManager); ok {
			if ctrs, err := cm.Containers(hctx); err == nil {
				for _, c := range ctrs {
					if !c.Standalone {
						continue
					}
					for _, p := range containerProblems(c) {
						all = append(all, problemItem{Problem: p, HostID: he.ID, HostName: he.Name, ContainerID: c.ID})
					}
				}
			}
		}
		cancel()
	}
	sort.SliceStable(all, func(i, j int) bool {
		if severityRank[all[i].Severity] != severityRank[all[j].Severity] {
			return severityRank[all[i].Severity] < severityRank[all[j].Severity]
		}
		return all[i].Since.Before(all[j].Since)
	})
	if len(all) > 20 {
		out.More = len(all) - 20
		all = all[:20]
	}
	out.Problems = append(out.Problems, all...)
	return out
}
