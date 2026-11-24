package status

import (
	"fmt"
	"sort"
	"time"

	"moorage/internal/monitor"
	"moorage/internal/store"
)

// plannedHorizon is how far ahead a status page lists planned maintenance.
const plannedHorizon = 7 * 24 * time.Hour

// maxWindows bounds the maintenance windows a status page lists, running and planned
// together: running windows first (earliest start first), the rest filled with the
// nearest planned ones. Coverage of the items is computed before this cut.
const maxWindows = 3

type pageMaint struct {
	active  []PublicMaintenance
	planned []PublicMaintenance
	feed    []FeedEvent
	covered map[int]bool // item index -> under an active window
}

// pageMaintenance finds the windows that cover items of a page: running now, starting
// within 7 days, and started within 30 days (for the RSS feed).
func (s *Service) pageMaintenance(items []store.StatusPageItem, itemChecks [][]store.Check, now time.Time) pageMaint {
	out := pageMaint{covered: map[int]bool{}}
	if s.mon == nil || len(items) == 0 {
		return out
	}
	windows := s.mon.MaintenanceWindows()
	for _, w := range windows {
		var names []string
		var idx []int
		for i, it := range items {
			if itemCovered(w, it, itemChecks[i]) {
				names = append(names, it.DisplayName)
				idx = append(idx, i)
			}
		}
		if len(idx) == 0 {
			continue
		}
		for _, o := range monitor.Occurrences(w, now.Add(-30*24*time.Hour), now.Add(plannedHorizon)) {
			pm := publicMaint(w, o, names)
			switch {
			case o.Active(now):
				out.active = append(out.active, pm)
				for _, i := range idx {
					out.covered[i] = true
				}
			case o.Start.After(now):
				out.planned = append(out.planned, pm)
			}
			if !o.Start.After(now) {
				out.feed = append(out.feed, FeedEvent{
					GUID:    fmt.Sprintf("maint-%d-%d", w.ID, o.Start.Unix()),
					Title:   "Maintenance: " + w.Title,
					Desc:    maintDesc(pm),
					PubDate: o.Start.UTC(),
				})
			}
		}
	}
	byStart := func(list []PublicMaintenance) {
		sort.SliceStable(list, func(i, j int) bool { return list[i].StartsAt.Before(list[j].StartsAt) })
	}
	byStart(out.active)
	byStart(out.planned)
	if len(out.active) > maxWindows {
		out.active = out.active[:maxWindows]
	}
	if room := maxWindows - len(out.active); len(out.planned) > room {
		out.planned = out.planned[:room]
	}
	return out
}

// itemCovered reports whether a window covers a page item: a check item through the check,
// a stack item through the stack itself or any of its checks.
func itemCovered(w store.MaintenanceWindow, it store.StatusPageItem, checks []store.Check) bool {
	if w.Scope.All {
		return true
	}
	if it.CheckID == 0 && monitor.CoversStack(w, it.HostID, it.Stack) {
		return true
	}
	for _, c := range checks {
		if monitor.CoversCheck(w, c) {
			return true
		}
	}
	return false
}

func publicMaint(w store.MaintenanceWindow, o monitor.Occurrence, names []string) PublicMaintenance {
	return PublicMaintenance{
		Title:      w.Title,
		Message:    w.Message,
		StartsAt:   o.Start.UTC(),
		StartsText: o.Start.UTC().Format("2 Jan 2006, 15:04 UTC"),
		EndsAt:     o.End.UTC(),
		EndsText:   o.End.UTC().Format("2 Jan 2006, 15:04 UTC"),
		Items:      names,
	}
}

func maintDesc(pm PublicMaintenance) string {
	d := fmt.Sprintf("Planned maintenance from %s to %s.", pm.StartsText, pm.EndsText)
	if pm.Message != "" {
		d += " " + pm.Message
	}
	return d
}

// mergeFeed joins incident and maintenance feed items, newest first, at most 50.
func mergeFeed(a, b []FeedEvent) []FeedEvent {
	if len(b) == 0 {
		return a
	}
	out := append(append([]FeedEvent(nil), a...), b...)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].PubDate.Equal(out[j].PubDate) {
			return out[i].PubDate.After(out[j].PubDate)
		}
		return out[i].GUID > out[j].GUID
	})
	if len(out) > 50 {
		out = out[:50]
	}
	return out
}
