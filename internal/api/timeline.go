package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
)

// Timeline entry kinds. Their string order is part of the sort key (time, kind, id), so
// paging stays stable when entries of several sources share a timestamp.
const (
	tlAudit    = "audit"
	tlEvent    = "event"
	tlIncident = "incident"
	tlTerminal = "terminal"
	tlUpdate   = "update"
	tlVersion  = "version"
)

var timelineKinds = []string{tlVersion, tlEvent, tlIncident, tlUpdate, tlAudit, tlTerminal}

// adminKinds are never sent to viewers: the server leaves them out, not the UI.
var adminKinds = map[string]bool{tlAudit: true, tlTerminal: true}

const (
	timelineDefaultLimit = 100
	timelineMaxLimit     = 500
)

// tlKey is the sort key of an entry: time in ms, kind and an id that sorts within the kind.
// Numeric ids are zero-padded so the string order equals the numeric order.
type tlKey struct {
	at   int64
	kind string
	id   string
}

func numID(n int64) string { return fmt.Sprintf("%019d", n) }

// cmp orders keys oldest first; the timeline lists them in reverse.
func (k tlKey) cmp(o tlKey) int {
	switch {
	case k.at != o.at:
		if k.at < o.at {
			return -1
		}
		return 1
	case k.kind != o.kind:
		return strings.Compare(k.kind, o.kind)
	}
	return strings.Compare(k.id, o.id)
}

// String is the cursor form of the key: "<ms>~<kind>~<id>".
func (k tlKey) String() string { return strconv.FormatInt(k.at, 10) + "~" + k.kind + "~" + k.id }

var (
	numIDRe     = regexp.MustCompile(`^[0-9]{1,19}$`)
	versionIDRe = regexp.MustCompile(`^[0-9a-f]{40,64}$`)
)

func parseCursor(s string) (tlKey, error) {
	parts := strings.Split(s, "~")
	if len(parts) != 3 {
		return tlKey{}, errors.New("bad cursor")
	}
	at, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || at <= 0 {
		return tlKey{}, errors.New("bad cursor time")
	}
	k := tlKey{at: at, kind: parts[1], id: parts[2]}
	switch {
	case k.kind == tlVersion && versionIDRe.MatchString(k.id):
	case k.kind != tlVersion && isTimelineKind(k.kind) && numIDRe.MatchString(k.id):
		n, _ := strconv.ParseInt(k.id, 10, 64)
		k.id = numID(n) // a short form still sorts right
	default:
		return tlKey{}, errors.New("bad cursor kind or id")
	}
	return k, nil
}

func isTimelineKind(k string) bool {
	for _, x := range timelineKinds {
		if x == k {
			return true
		}
	}
	return false
}

// tlQuery is one timeline request after parsing.
type tlQuery struct {
	hostID, stack string
	kinds         map[string]bool
	cursor        tlKey // zero: from the newest entry
	sinceMs       int64 // 0: no lower bound
	limit         int
}

// parseTimelineQuery reads before/until/since/limit/kinds. Admin-only kinds are dropped
// for viewers here, so nothing later can fetch them by mistake.
func parseTimelineQuery(r *http.Request, admin bool) (tlQuery, error) {
	q := r.URL.Query()
	tq := tlQuery{hostID: r.PathValue("host"), stack: r.PathValue("name"), kinds: map[string]bool{}, limit: timelineDefaultLimit}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return tq, errors.New("limit must be a positive number")
		}
		tq.limit = min(n, timelineMaxLimit)
	}
	if v := strings.TrimSpace(q.Get("kinds")); v != "" {
		for _, k := range strings.Split(v, ",") {
			k = strings.TrimSpace(k)
			if !isTimelineKind(k) {
				return tq, fmt.Errorf("unknown kind %q", k)
			}
			tq.kinds[k] = true
		}
	} else {
		for _, k := range timelineKinds {
			tq.kinds[k] = true
		}
	}
	if !admin {
		for k := range adminKinds {
			delete(tq.kinds, k)
		}
	}
	if v := q.Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return tq, errors.New("since must be a unix time in seconds")
		}
		tq.sinceMs = n * 1000
	}
	switch {
	case q.Get("before") != "":
		c, err := parseCursor(q.Get("before"))
		if err != nil {
			return tq, errors.New("before must be a cursor from an earlier page")
		}
		tq.cursor = c
	case q.Get("until") != "":
		n, err := strconv.ParseInt(q.Get("until"), 10, 64)
		if err != nil || n <= 0 {
			return tq, errors.New("until must be a unix time in seconds")
		}
		// Kind "" sorts below every kind: everything up to the end of that second is kept.
		tq.cursor = tlKey{at: (n + 1) * 1000}
	}
	return tq, nil
}

// stackTimeline serves GET /api/hosts/{host}/stacks/{name}/timeline: versions, stack
// events, incidents, image updates and (for admins) audit rows and terminal sessions,
// merged into one list newest first.
func (s *Server) stackTimeline(w http.ResponseWriter, r *http.Request) {
	if !stackNameRe.MatchString(r.PathValue("name")) {
		writeErr(w, http.StatusBadRequest, "Invalid stack name.")
		return
	}
	tq, err := parseTimelineQuery(r, currentUser(r).Role == "admin")
	if err != nil {
		writeErr(w, http.StatusBadRequest, sentence(err))
		return
	}
	versions, ok := s.timelineVersions(w, r)
	if !ok {
		return
	}
	page, err := s.buildTimeline(r.Context(), tq, versions)
	if err != nil {
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// versionSource lists a stack's versions newest first (bounded by the host: 200).
type versionSource func(ctx context.Context) ([]engine.Version, error)

// timelineVersions picks where versions come from. An offline host still gets a timeline
// of the hub-side sources; only an unknown host is a 404.
func (s *Server) timelineVersions(w http.ResponseWriter, r *http.Request) (versionSource, bool) {
	name := r.PathValue("name")
	if s.reg == nil {
		return func(context.Context) ([]engine.Version, error) { return nil, host.ErrUnknownHost }, true
	}
	h, e, err := s.reg.Get(r.Context(), r.PathValue("host"))
	switch {
	case errors.Is(err, host.ErrUnknownHost):
		writeErr(w, http.StatusNotFound, "Host not found.")
		return nil, false
	case errors.Is(err, host.ErrOffline):
		return func(context.Context) ([]engine.Version, error) {
			return nil, fmt.Errorf("%s is offline", e.Name)
		}, true
	case err != nil:
		return func(context.Context) ([]engine.Version, error) { return nil, err }, true
	}
	return func(ctx context.Context) ([]engine.Version, error) { return h.History(ctx, name) }, true
}

// versionsUnavailable turns a failed version listing into the note the UI shows.
func versionsUnavailable(err error) string {
	switch {
	case host.IsUnsupported(err):
		return "Versions are unavailable: this agent doesn't list them."
	case errors.Is(err, engine.ErrNotFound):
		return "Versions are unavailable: the stack isn't in the stacks folder."
	case errors.Is(err, host.ErrUnknownHost):
		return "Versions are unavailable on this host."
	}
	return "Versions are unavailable: " + sentence(err)
}

// timelineWindow is how far apart an audit row, a stack event and the version or image
// update of one action may be recorded and still be shown as one entry.
const timelineWindow = 10 * time.Second
