package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"moorage/internal/host"
)

// probeResources checks a host's disks and memory against the thresholds (M19M). Over
// the warning threshold the check stays up with a note; over the critical one it is down.
func probeResources(ctx context.Context, reg *host.Registry, hostID string, st Settings) Outcome {
	if reg == nil {
		return fail("hosts aren't available")
	}
	h, e, err := reg.Get(ctx, hostID)
	if errors.Is(err, host.ErrOffline) {
		return fail("host %s is offline", e.Name)
	}
	if err != nil {
		return fail("host not found")
	}
	r := h.Info(ctx).Resources
	if r == nil {
		return fail("the host doesn't report disk and memory (update the agent)")
	}
	warn, crit, memCrit := orDefault(st.DiskWarnPct, 85), orDefault(st.DiskCritPct, 95), orDefault(st.MemCritPct, 90)
	var bad, notes []string
	for _, d := range r.Disks {
		switch {
		case d.UsedPct >= float64(crit):
			bad = append(bad, fmt.Sprintf("%s %.0f %% full", d.Label, d.UsedPct))
		case d.UsedPct >= float64(warn):
			notes = append(notes, fmt.Sprintf("%s %.0f %% full (warning)", d.Label, d.UsedPct))
		}
	}
	if r.MemTotal > 0 && r.MemUsedPct >= float64(memCrit) {
		bad = append(bad, fmt.Sprintf("memory %.0f %% used", r.MemUsedPct))
	}
	if len(bad) > 0 {
		return fail("%s", strings.Join(append(bad, notes...), ", "))
	}
	msg := fmt.Sprintf("memory %.0f %% used", r.MemUsedPct)
	for _, d := range r.Disks {
		msg += fmt.Sprintf(", %s %.0f %%", strings.ToLower(d.Label), d.UsedPct)
	}
	if len(notes) > 0 {
		msg = strings.Join(notes, ", ")
	}
	return Outcome{OK: true, Msg: msg}
}

func orDefault(v, d int) int {
	if v <= 0 || v > 100 {
		return d
	}
	return v
}
