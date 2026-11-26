package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/store"
)

// Multi-location checks: an HTTP, TCP, ping or DNS check can run from the hub and from
// chosen agents at once, and counts as down only when enough of them agree. A blip in
// one network then doesn't become a false "down".

// LocationHub is the location id of the hub process itself.
const LocationHub = "hub"

// Quorum rules.
const (
	QuorumMajority = "majority" // down when more than half of the answering locations fail
	QuorumAny      = "any"      // down when any answering location fails
	QuorumAll      = "all"      // down only when every answering location fails
)

// Quorums lists the valid quorum rules.
var Quorums = map[string]bool{QuorumMajority: true, QuorumAny: true, QuorumAll: true}

// MaxLocations bounds the locations of one check, and with them the per-location rows
// and metric series a check produces.
const MaxLocations = 8

// locationMargin is added to the check's timeout for the tunnel round trip to an agent.
var locationMargin = 5 * time.Second // a var so tests can shorten it

// Votes of one location.
const (
	VoteUp   = "up"
	VoteDown = "down"
	VoteNone = "none" // offline, unsupported, busy or no answer: doesn't count
)

// LocationResult is one location's answer in a multi-location run.
type LocationResult struct {
	Location  string `json:"location"`
	Name      string `json:"name"`
	Vote      string `json:"vote"`
	LatencyMS int    `json:"latencyMs"`
	Msg       string `json:"msg"`
}

// MultiLocation reports whether c runs anywhere else than only on the hub. A check
// without locations (or with just the hub) runs exactly as before this feature.
func MultiLocation(c store.Check) bool {
	if !RemoteKinds[c.Kind] || len(c.Locations) == 0 {
		return false
	}
	return len(c.Locations) > 1 || c.Locations[0] != LocationHub
}

// Decide applies a quorum to the votes and returns StatusUp, StatusDown or StatusUnknown
// (nobody voted). Majority means strictly more than half: 2 of 4 failing is not a majority,
// so a tie stays up; with 2 voters one failure is not enough, both must fail.
func Decide(quorum string, up, down int) string {
	voters := up + down
	if voters == 0 {
		return StatusUnknown
	}
	switch quorum {
	case QuorumAny:
		if down > 0 {
			return StatusDown
		}
	case QuorumAll:
		if up == 0 {
			return StatusDown
		}
	default: // majority, also for an empty value
		if down*2 > voters {
			return StatusDown
		}
	}
	return StatusUp
}

// probeLocations runs c from all its locations in parallel and combines the answers.
// It returns within the check's timeout plus a margin, whatever the agents do.
func (m *Monitor) probeLocations(ctx context.Context, c store.Check) Outcome {
	names := m.locationNames(ctx)
	wait := time.Duration(max(c.Timeout, 1))*time.Second + locationMargin
	rctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	type answer struct {
		i int
		r LocationResult
	}
	answers := make(chan answer, len(c.Locations)) // buffered: a late goroutine never blocks
	results := make([]LocationResult, len(c.Locations))
	for i, loc := range c.Locations {
		results[i] = LocationResult{Location: loc, Vote: VoteNone, Msg: fmt.Sprintf("no answer within %s", human(wait))}
		go func() { answers <- answer{i, m.probeAt(rctx, c, loc, wait)} }()
	}
	deadline := time.NewTimer(wait + time.Second)
	defer deadline.Stop()
collect:
	for n := len(c.Locations); n > 0; n-- {
		select {
		case a := <-answers:
			results[a.i] = a.r
		case <-deadline.C:
			break collect
		case <-ctx.Done():
			break collect
		}
	}
	for i := range results {
		results[i].Name = locationName(names, results[i].Location)
	}
	return aggregate(c, results)
}

// probeAt asks one location. It never returns an error: a location that can't answer
// gets VoteNone with the reason.
func (m *Monitor) probeAt(ctx context.Context, c store.Check, loc string, wait time.Duration) LocationResult {
	r := LocationResult{Location: loc, Vote: VoteNone}
	if loc == LocationHub {
		o := Probe(ctx, c, m.reg)
		if o.cantRun {
			r.Msg = o.Msg
			return r
		}
		return voted(r, o.OK, int(o.Latency.Milliseconds()), o.Msg)
	}
	if m.reg == nil {
		r.Msg = "hosts aren't available"
		return r
	}
	rm, _, ok := m.reg.Remote(loc)
	if !ok {
		r.Msg = "agent offline"
		return r
	}
	req := host.ProbeRequest{Kind: c.Kind, Target: c.Target, Timeout: c.Timeout}
	if c.Settings != "" {
		req.Settings = json.RawMessage(c.Settings)
	}
	res, err := rm.Probe(ctx, req, wait)
	if err == nil {
		// An agent older than the latency limit ignores it: apply it here too.
		ok, msg := LatencyLimit(ParseSettings(c.Settings).MaxLatencyMS, res.OK, res.LatencyMS, res.Msg)
		return voted(r, ok, res.LatencyMS, msg)
	}
	var ve engine.ValidationError
	switch {
	case host.IsUnsupported(err):
		r.Msg = "agent too old to run checks"
	case host.IsProbesDisabled(err):
		r.Msg = "checks are turned off on this agent"
	case errors.Is(err, engine.ErrBusy):
		r.Msg = "agent busy with other checks"
	case errors.As(err, &ve):
		r.Msg = ve.Msg
	case ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded):
		r.Msg = fmt.Sprintf("no answer within %s", human(wait))
	default:
		r.Msg = "agent unreachable"
	}
	return r
}

func voted(r LocationResult, ok bool, latencyMS int, msg string) LocationResult {
	r.Vote, r.LatencyMS, r.Msg = VoteDown, latencyMS, msg
	if ok {
		r.Vote = VoteUp
	}
	return r
}

// aggregate turns the per-location answers into one outcome with a message that names
// the failing locations, e.g. "down from 2 of 3 locations: wd1 (timed out), hub (connection refused)".
func aggregate(c store.Check, results []LocationResult) Outcome {
	var up, down, none []LocationResult
	var lat []int
	for _, r := range results {
		switch r.Vote {
		case VoteUp:
			up = append(up, r)
			lat = append(lat, r.LatencyMS)
		case VoteDown:
			down = append(down, r)
			lat = append(lat, r.LatencyMS)
		default:
			none = append(none, r)
		}
	}
	out := Outcome{Locations: results, Latency: time.Duration(median(lat)) * time.Millisecond}
	voters := len(up) + len(down)
	var parts []string
	switch Decide(c.Quorum, len(up), len(down)) {
	case StatusUnknown:
		out.Unknown = true
		parts = append(parts, "no location could run the check: "+listLocations(none))
		none = nil
	case StatusDown:
		parts = append(parts, fmt.Sprintf("down from %d of %d %s: %s", len(down), voters, plural(voters, "location", "locations"), listLocations(down)))
		if len(up) > 0 {
			parts = append(parts, "up from "+joinNames(up))
		}
	default:
		out.OK = true
		parts = append(parts, fmt.Sprintf("up from %d of %d %s (%s)", len(up), voters, plural(voters, "location", "locations"), up[0].Msg))
		if len(down) > 0 {
			parts = append(parts, "failing: "+listLocations(down))
		}
	}
	if len(none) > 0 {
		parts = append(parts, "no answer: "+listLocations(none))
	}
	out.Msg = clip(strings.Join(parts, "; "), 400)
	return out
}

func listLocations(rs []LocationResult) string {
	s := make([]string, len(rs))
	for i, r := range rs {
		s[i] = fmt.Sprintf("%s (%s)", r.Name, clip(r.Msg, 80))
	}
	return strings.Join(s, ", ")
}

func joinNames(rs []LocationResult) string {
	s := make([]string, len(rs))
	for i, r := range rs {
		s[i] = r.Name
	}
	return strings.Join(s, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func median(xs []int) int {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int(nil), xs...)
	sort.Ints(s)
	return s[len(s)/2]
}

// locationNames maps location ids to display names: "hub" and the agents' names.
func (m *Monitor) locationNames(ctx context.Context) map[string]string {
	out := map[string]string{LocationHub: "hub"}
	if m.reg == nil {
		return out
	}
	entries, _ := m.reg.List(ctx)
	for _, e := range entries {
		if e.Kind == "agent" && e.Name != "" {
			out[e.ID] = e.Name
		}
	}
	return out
}

func locationName(names map[string]string, id string) string {
	if n, ok := names[id]; ok {
		return n
	}
	return id
}

// locationLine is the per-location breakdown of an alert, e.g.
// "By location: hub down (connection refused, 2 ms), wd1 up (HTTP 200, 40 ms), wd2 no answer (agent offline)."
func locationLine(rs []LocationResult) string {
	s := make([]string, len(rs))
	for i, r := range rs {
		switch r.Vote {
		case VoteNone:
			s[i] = fmt.Sprintf("%s no answer (%s)", r.Name, clip(r.Msg, 80))
		default:
			s[i] = fmt.Sprintf("%s %s (%s, %d ms)", r.Name, r.Vote, clip(r.Msg, 80), r.LatencyMS)
		}
	}
	return "By location: " + strings.Join(s, ", ") + "."
}

// storeLocations converts a run's answers to rows for check_location_results.
func storeLocations(checkID int64, at time.Time, rs []LocationResult) []store.LocationResult {
	out := make([]store.LocationResult, len(rs))
	for i, r := range rs {
		out[i] = store.LocationResult{
			CheckID: checkID, TS: at, Location: r.Location,
			Voted: r.Vote != VoteNone, OK: r.Vote == VoteUp, LatencyMS: r.LatencyMS, Msg: clip(r.Msg, 200),
		}
	}
	return out
}

// LocationTicks returns the newest results of each location of a check, oldest first,
// for the per-location strips. A row without a vote is an "unknown" tick.
func (m *Monitor) LocationTicks(ctx context.Context, c store.Check, perLocation int) (map[string][]Tick, error) {
	out := map[string][]Tick{}
	if !MultiLocation(c) {
		return out, nil
	}
	rows, err := m.st.RecentLocationResults(ctx, c.ID, perLocation*len(c.Locations))
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		status := StatusUnknown
		if r.Voted {
			status = StatusDown
			if r.OK {
				status = StatusUp
			}
		}
		out[r.Location] = append(out[r.Location], Tick{At: r.TS, Status: status, LatencyMS: r.LatencyMS, Msg: r.Msg})
	}
	return out, nil
}
