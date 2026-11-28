package api

import (
	"net/http"
	"strings"
	"time"

	"moorage/internal/engine"
	"moorage/internal/monitor"
	"moorage/internal/store"
	"moorage/internal/updater"
)

// navSummaryDTO feeds the badges of the top navigation. It is read from memory and SQLite
// only (never from registries or hosts), so the UI can poll it cheaply.
type navSummaryDTO struct {
	// ChecksFailing: enabled checks outside maintenance that are down or partially down.
	ChecksFailing int `json:"checksFailing"`
	// MaintenanceActive: maintenance windows running now.
	MaintenanceActive int `json:"maintenanceActive"`
	// UpdatesFailed: stacks with policy auto whose last automatic update failed.
	UpdatesFailed int `json:"updatesFailed"`
	// UpdatesAvailable: services with a new digest or a newer tag in stacks with policy
	// notify or auto.
	UpdatesAvailable int `json:"updatesAvailable"`
	// Failing lists the failing checks (at most 50) so the UI can toast new ones.
	Failing []navFailingCheck `json:"failing"`
	// MCPDrafts is the number of pending MCP drafts waiting for human approval.
	MCPDrafts int `json:"mcpDrafts"`
}

type navFailingCheck struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Msg    string `json:"msg,omitempty"`
}

func (s *Server) navSummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	out := navSummaryDTO{Failing: []navFailingCheck{}}
	if checks, err := s.store.Checks(ctx); err == nil && s.mon != nil {
		for _, c := range checks {
			if !c.Enabled {
				continue
			}
			st := s.mon.State(c.ID)
			switch st.Status {
			case monitor.StatusDown, monitor.StatusDegraded:
				out.ChecksFailing++
				if len(out.Failing) < 50 {
					f := navFailingCheck{ID: c.ID, Name: c.Name, Status: st.Status}
					if st.Last != nil {
						f.Msg = st.Last.Msg
					}
					out.Failing = append(out.Failing, f)
				}
			}
		}
	}
	if s.mon != nil {
		seen := map[int64]bool{}
		for _, a := range s.mon.ActiveWindows(now) {
			seen[a.Window.ID] = true
		}
		out.MaintenanceActive = len(seen)
	}
	policies, _ := s.store.AllStackUpdatePolicies(ctx)
	policyOf := func(hostID, stack string) string {
		if p, ok := policies[hostID+"/"+stack]; ok && p.Policy != "" {
			return p.Policy
		}
		return store.PolicyNotify
	}
	if rows, err := s.store.ImageChecks(ctx, "", ""); err == nil {
		for _, row := range rows {
			if policyOf(row.HostID, row.Stack) == store.PolicyOff {
				continue
			}
			if row.Status == engine.ImageUpdateAvailable || row.Status == engine.ImagePulledNotApplied || row.NewerTag != "" {
				out.UpdatesAvailable++
			}
		}
	}
	if recent, err := s.store.ImageUpdates(ctx, "", "", 200); err == nil {
		last := map[string]bool{}
		for _, u := range recent { // newest first
			key := u.HostID + "/" + u.Stack
			if last[key] {
				continue
			}
			last[key] = true
			if u.Actor != updater.ActorAuto || policyOf(u.HostID, u.Stack) != store.PolicyAuto {
				continue
			}
			switch u.Result {
			case store.UpdatePullFailed, store.UpdateUpFailed, store.UpdateRolledBack:
				out.UpdatesFailed++
			}
		}
	}
	if currentUser(r).Role == "admin" {
		if drafts, err := s.store.MCPDrafts(ctx, "pending"); err == nil {
			for _, d := range drafts {
				if !strings.HasPrefix(d.TokenName, assistantDraftPrefix) { // M20G: only MCP token drafts
					out.MCPDrafts++
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}
