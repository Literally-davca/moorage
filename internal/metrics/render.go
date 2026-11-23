package metrics

import (
	"context"
	"io"
	"sort"
	"strconv"

	"moorage/internal/host"
	"moorage/internal/monitor"
	"moorage/internal/store"
)

// hostLabel is how a host appears in labels: its ID (stable, unique) and its name.
type hostLabel struct{ id, name string }

// Write renders all metrics to out.
func (c *Collector) Write(ctx context.Context, out io.Writer) error {
	snap := c.current()
	w := newWriter(out)

	w.family("moorage_build_info", "gauge", "Moorage version of this hub, always 1.")
	w.sample(1, "version", c.version)

	hosts := c.writeHosts(ctx, w)
	c.writeChecks(ctx, w, hosts, snap)
	if snap != nil {
		w.family("moorage_snapshot_age_seconds", "gauge", "Age of the container, uptime and image data in this scrape.")
		w.sample(max(c.now().Sub(snap.at).Seconds(), 0))
		// A snapshot older than the idle timeout is from before a pause in scraping;
		// containers may have changed since, so leave them out rather than show old truth.
		if c.now().Sub(snap.at) <= c.IdleStop {
			writeStacks(w, hosts, snap)
		}
		writeImages(w, hosts, snap)
	}
	c.writeAlerts(w)
	return w.flush()
}

// writeHosts writes the host series and returns the known hosts by ID.
func (c *Collector) writeHosts(ctx context.Context, w *writer) map[string]host.Entry {
	byID := map[string]host.Entry{}
	if c.reg == nil {
		return byID
	}
	entries, _ := c.reg.List(ctx)
	for _, e := range entries {
		byID[e.ID] = e
	}
	w.family("moorage_host_up", "gauge", "1 if the host is connected to the hub (the local host always is).")
	for _, e := range entries {
		w.sample(boolValue(e.Connected), "host_id", e.ID, "host", e.Name)
	}
	w.family("moorage_host_read_only", "gauge", "1 if Moorage may not change anything on the host.")
	for _, e := range entries {
		w.sample(boolValue(e.ReadOnly), "host_id", e.ID, "host", e.Name)
	}
	w.family("moorage_agent_info", "gauge", "Version of a connected agent, always 1.")
	for _, e := range entries {
		if e.Kind == "agent" && e.Connected {
			w.sample(1, "host_id", e.ID, "host", e.Name, "version", e.Version)
		}
	}
	return byID
}

func checkStack(c store.Check) string {
	if c.Kind == monitor.KindStack {
		return c.Target
	}
	return c.Stack
}

func (c *Collector) writeChecks(ctx context.Context, w *writer, hosts map[string]host.Entry, snap *snapshot) {
	if c.st == nil {
		return
	}
	checks, err := c.st.Checks(ctx)
	if err != nil {
		return
	}
	type row struct {
		labels []string
		state  monitor.State
		id     int64
	}
	rows := make([]row, 0, len(checks))
	for _, ch := range checks {
		st := monitor.State{Status: monitor.StatusUnknown}
		if c.mon != nil {
			st = c.mon.State(ch.ID)
		}
		labels := []string{
			"check_id", strconv.FormatInt(ch.ID, 10), "check", ch.Name, "type", ch.Kind,
			"host_id", ch.HostID, "host", hosts[ch.HostID].Name, "stack", checkStack(ch),
		}
		rows = append(rows, row{labels: labels, state: st, id: ch.ID})
	}

	w.family("moorage_check_up", "gauge", "1 if the check's state is up, 0 in every other state (see moorage_check_state).")
	for _, r := range rows {
		w.sample(boolValue(r.state.Status == monitor.StatusUp), r.labels...)
	}
	w.family("moorage_check_state", "gauge", "1 for the check's current state: up, down, pending, maintenance, silenced, paused or unknown.")
	for _, r := range rows {
		for _, s := range checkStates {
			w.sample(boolValue(r.state.Status == s), append(r.labels[:len(r.labels):len(r.labels)], "state", s)...)
		}
	}
	w.family("moorage_check_latency_seconds", "gauge", "Duration of the check's last probe.")
	for _, r := range rows {
		if r.state.Last != nil {
			w.sample(float64(r.state.Last.LatencyMS)/1000, r.labels...)
		}
	}
	// Multi-location checks: one series per location that voted in the last run. A location
	// that didn't answer (offline, unsupported, busy) has no series, because it is neither
	// up nor down. At most monitor.MaxLocations series per check.
	w.family("moorage_check_location_up", "gauge", "1 if the location's last answer was up, 0 if down (multi-location checks; no series while a location doesn't answer).")
	for _, r := range rows {
		for _, l := range r.state.Locations {
			if l.Vote != monitor.VoteNone {
				w.sample(boolValue(l.Vote == monitor.VoteUp), locationLabels(r.labels, l)...)
			}
		}
	}
	w.family("moorage_check_location_latency_seconds", "gauge", "Duration of the location's last probe (multi-location checks).")
	for _, r := range rows {
		for _, l := range r.state.Locations {
			if l.Vote != monitor.VoteNone {
				w.sample(float64(l.LatencyMS)/1000, locationLabels(r.labels, l)...)
			}
		}
	}
	if snap == nil {
		return
	}
	w.family("moorage_check_uptime_ratio", "gauge", "Share of up results among up and down results in the window (0-1).")
	for _, win := range []struct {
		name   string
		counts map[int64][2]int
	}{{"24h", snap.up24h}, {"30d", snap.up30d}} {
		for _, r := range rows {
			if n := win.counts[r.id]; n[1] > 0 {
				w.sample(float64(n[0])/float64(n[1]), append(r.labels[:len(r.labels):len(r.labels)], "window", win.name)...)
			}
		}
	}
}

// locationLabels adds the location id ("hub" or an agent id) and its name to a check's labels.
func locationLabels(labels []string, l monitor.LocationResult) []string {
	return append(labels[:len(labels):len(labels)], "location", l.Location, "location_name", l.Name)
}

// writeStacks writes container counts per stack and per-container state for hosts that
// are connected now and answered the last refresh.
func writeStacks(w *writer, hosts map[string]host.Entry, snap *snapshot) {
	ids := make([]string, 0, len(snap.hosts))
	for id := range snap.hosts {
		if e, ok := hosts[id]; ok && e.Connected {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	w.family("moorage_stack_containers", "gauge", "Number of a stack's containers in each Docker state.")
	for _, id := range ids {
		name := hosts[id].Name
		for _, s := range snap.hosts[id] {
			counts := map[string]int{}
			for _, ct := range s.containers {
				counts[ct.state]++
			}
			for _, state := range containerStates {
				w.sample(float64(counts[state]), "host_id", id, "host", name, "stack", s.name, "state", state)
			}
		}
	}
	w.family("moorage_container_up", "gauge", "1 if the container is running.")
	for _, id := range ids {
		name := hosts[id].Name
		for _, s := range snap.hosts[id] {
			for _, ct := range s.containers {
				w.sample(boolValue(ct.state == "running"),
					"host_id", id, "host", name, "stack", s.name, "service", ct.service, "container", ct.name)
			}
		}
	}
}

// writeImages writes the result of the last image update check per service.
func writeImages(w *writer, hosts map[string]host.Entry, snap *snapshot) {
	w.family("moorage_image_update_available", "gauge", "1 if a newer image with the same tag is available (or pulled but not applied) for the service.")
	for _, im := range snap.images {
		e, ok := hosts[im.hostID]
		if !ok {
			continue // the host was removed; its rows are stale
		}
		w.sample(boolValue(im.update), "host_id", im.hostID, "host", e.Name, "stack", im.stack, "service", im.service)
	}
}

func (c *Collector) writeAlerts(w *writer) {
	if c.mon == nil {
		return
	}
	sent, failed := c.mon.AlertCounts()
	for _, f := range []struct {
		name, help string
		counts     map[string]uint64
	}{
		{"moorage_alerts_sent_total", "Alert messages delivered since the hub started, per channel type.", sent},
		{"moorage_alerts_failed_total", "Alert messages that failed to send since the hub started, per channel type.", failed},
	} {
		kinds := make([]string, 0, len(f.counts))
		for k := range f.counts {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		w.family(f.name, "counter", f.help)
		for _, k := range kinds {
			w.sample(float64(f.counts[k]), "channel_type", k)
		}
	}
}
