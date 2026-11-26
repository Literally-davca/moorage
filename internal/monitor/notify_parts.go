package monitor

import (
	"context"
	"strings"

	"moorage/internal/notify"
)

// structure fills the structured parts of an alert (M17B) for rich messages and channel
// presets: event, sender host, host(s), stack, cause, duration, times and check details.
func (m *Monitor) structure(msg *notify.Message, group []alert) {
	first := group[0]
	msg.Event = notify.EventChecks
	if first.check.Kind == "host" {
		msg.Event = notify.EventAgents
	}
	var hostIDs, hostNames []string
	seenID, seenName := map[string]bool{}, map[string]bool{}
	stack := stackOf(first.check)
	for _, a := range group {
		if id := a.check.HostID; id != "" && !seenID[id] {
			seenID[id] = true
			hostIDs = append(hostIDs, id)
		}
		name := ""
		switch {
		case a.check.Kind == "host":
			name = a.check.Name
		case a.ctx != nil && a.ctx.Host != "":
			name = a.ctx.Host
		default:
			name = m.hostName(a.check.HostID)
		}
		if name != "" && !seenName[name] {
			seenName[name] = true
			hostNames = append(hostNames, name)
		}
		if stackOf(a.check) != stack {
			stack = ""
		}
	}
	// A check alert comes from the host it is about; agents going offline / coming back
	// and alerts over several hosts come from the hub.
	if len(hostIDs) == 1 && first.check.Kind != "host" {
		msg.HostID = hostIDs[0]
	}
	if len(hostNames) == 1 {
		msg.Host = hostNames[0]
	} else {
		msg.Hosts = hostNames
	}
	msg.Stack = stack
	if len(group) == 1 {
		msg.Cause = first.cause
	}
	if first.up {
		msg.Duration = first.downFor
		msg.Until = first.at
		if first.downFor > 0 {
			msg.Since = first.at.Add(-first.downFor)
		}
	} else {
		msg.Since = first.at
	}
	for _, a := range group {
		d := notify.CheckDetail{Name: a.check.Name, Kind: kindLabel(a.check.Kind), Target: a.check.Target, Cause: a.cause}
		if a.check.Kind == KindLog {
			d.Target = strings.Trim(a.check.Stack+"/"+a.check.Target, "/")
		}
		if a.ctx != nil {
			for _, l := range a.ctx.Locations {
				d.Locations = append(d.Locations, notify.LocationDetail{Name: l.Name, Vote: l.Vote, LatencyMS: l.LatencyMS, Msg: clip(l.Msg, 80)})
			}
		}
		msg.Checks = append(msg.Checks, d)
	}
}

func (m *Monitor) hostName(id string) string {
	if m.reg == nil || id == "" {
		return ""
	}
	if _, e, _ := m.reg.Get(context.Background(), id); e.Name != "" {
		return e.Name
	}
	return ""
}
