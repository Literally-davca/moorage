package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"moorage/internal/host"
	"moorage/internal/monitor"
	"moorage/internal/store"
)

// ---- planned maintenance windows -------------------------------------------

type maintInput struct {
	Title     string                 `json:"title"`
	Message   string                 `json:"message"`
	Scope     store.MaintenanceScope `json:"scope"`
	Kind      string                 `json:"kind"`
	StartsAt  time.Time              `json:"startsAt,omitzero"`
	EndsAt    time.Time              `json:"endsAt,omitzero"`
	Weekdays  []int                  `json:"weekdays"`
	StartTime string                 `json:"startTime,omitempty"`
	Duration  int                    `json:"duration,omitempty"` // minutes
	TimeZone  string                 `json:"timeZone,omitempty"`
	Enabled   bool                   `json:"enabled"`
}

// Window states shown in the UI.
const (
	maintActive   = "active"
	maintUpcoming = "upcoming"
	maintPast     = "past"
	maintDisabled = "disabled"
)

type maintDTO struct {
	maintInput
	ID        int64     `json:"id"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	// State is active, upcoming, past or disabled.
	State   string              `json:"state"`
	Current *monitor.Occurrence `json:"current,omitempty"`
	Next    *monitor.Occurrence `json:"next,omitempty"`
	Last    *monitor.Occurrence `json:"last,omitempty"`
	// ScopeText names what the window covers, for lists.
	ScopeText []string `json:"scopeText"`
}

func maintState(w store.MaintenanceWindow, now time.Time) (state string, cur, next, last *monitor.Occurrence) {
	w.Enabled = true // compute the schedule even for a disabled window, to show it
	for _, o := range monitor.Occurrences(w, now.Add(-8*24*time.Hour), now.Add(8*24*time.Hour)) {
		switch {
		case o.Active(now):
			cur = &o
		case o.Start.After(now) && next == nil:
			next = &o
		case !o.End.After(now):
			last = &o
		}
	}
	if w.Kind == store.MaintenanceOnce && last == nil && !w.EndsAt.After(now) && !w.EndsAt.IsZero() {
		last = &monitor.Occurrence{Start: w.StartsAt.UTC(), End: w.EndsAt.UTC()}
	}
	if w.Kind == store.MaintenanceOnce && next == nil && cur == nil && w.StartsAt.After(now) {
		next = &monitor.Occurrence{Start: w.StartsAt.UTC(), End: w.EndsAt.UTC()}
	}
	switch {
	case cur != nil:
		state = maintActive
	case next != nil:
		state = maintUpcoming
	default:
		state = maintPast
	}
	return state, cur, next, last
}

func (s *Server) toMaintDTO(w store.MaintenanceWindow, now time.Time, checks map[int64]store.Check, hosts map[string]string) maintDTO {
	d := maintDTO{
		maintInput: maintInput{
			Title: w.Title, Message: w.Message, Scope: w.Scope, Kind: w.Kind,
			Weekdays: w.Weekdays, StartTime: w.StartTime, Duration: w.Duration, TimeZone: w.TZ, Enabled: w.Enabled,
		},
		ID: w.ID, CreatedBy: w.CreatedBy, CreatedAt: w.CreatedAt,
	}
	if w.Kind == store.MaintenanceOnce {
		d.StartsAt, d.EndsAt = w.StartsAt.UTC(), w.EndsAt.UTC()
	}
	if d.Weekdays == nil {
		d.Weekdays = []int{}
	}
	// Checks deleted since the window was saved drop out of its scope.
	var kept []int64
	for _, id := range d.Scope.Checks {
		if _, ok := checks[id]; ok {
			kept = append(kept, id)
		}
	}
	d.Scope.Checks = kept
	d.State, d.Current, d.Next, d.Last = maintState(w, now)
	if !w.Enabled {
		d.State, d.Current = maintDisabled, nil
	}
	d.ScopeText = scopeText(d.Scope, checks, hosts)
	return d
}

func scopeText(sc store.MaintenanceScope, checks map[int64]store.Check, hosts map[string]string) []string {
	if sc.All {
		return []string{"All checks and hosts"}
	}
	out := []string{}
	for _, st := range sc.Stacks {
		name := hosts[st.HostID]
		if name == "" {
			name = st.HostID
		}
		out = append(out, st.Stack+" on "+name)
	}
	for _, id := range sc.Checks {
		if c, ok := checks[id]; ok {
			out = append(out, c.Name)
		}
	}
	return out
}

func (s *Server) checksByID(ctx context.Context) (map[int64]store.Check, error) {
	list, err := s.store.Checks(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]store.Check, len(list))
	for _, c := range list {
		out[c.ID] = c
	}
	return out, nil
}

// hubTimeZone is the zone weekly windows use when none is given.
func hubTimeZone() string {
	return time.Local.String()
}

func (s *Server) listMaintenance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	windows, err := s.store.MaintenanceWindows(ctx)
	if err != nil {
		s.engErr(w, err)
		return
	}
	checks, err := s.checksByID(ctx)
	if err != nil {
		s.engErr(w, err)
		return
	}
	hosts := s.hostNames(ctx)
	now := time.Now()
	out := make([]maintDTO, 0, len(windows))
	for _, mw := range windows {
		out = append(out, s.toMaintDTO(mw, now, checks, hosts))
	}
	// Active first, then upcoming by start, then disabled and past, newest first.
	rank := map[string]int{maintActive: 0, maintUpcoming: 1, maintDisabled: 2, maintPast: 3}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if rank[a.State] != rank[b.State] {
			return rank[a.State] < rank[b.State]
		}
		if a.State == maintUpcoming {
			return a.Next.Start.Before(b.Next.Start)
		}
		return a.ID > b.ID
	})
	writeJSON(w, http.StatusOK, map[string]any{"windows": out, "hubTimeZone": hubTimeZone()})
}

func (s *Server) getMaintenance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	mw, err := s.store.MaintenanceWindowByID(ctx, checkID(r))
	if err != nil {
		writeErr(w, http.StatusNotFound, "Maintenance window not found.")
		return
	}
	checks, err := s.checksByID(ctx)
	if err != nil {
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toMaintDTO(mw, time.Now(), checks, s.hostNames(ctx)))
}

// validateMaintenance checks the input and turns it into a window. isNew rejects a
// one-off window that has already ended (editing an old window keeps working).
func (s *Server) validateMaintenance(ctx context.Context, in maintInput, isNew bool, now time.Time) (store.MaintenanceWindow, error) {
	out := store.MaintenanceWindow{Title: strings.TrimSpace(in.Title), Message: strings.TrimSpace(in.Message), Kind: in.Kind, Enabled: in.Enabled}
	switch {
	case out.Title == "" || len([]rune(out.Title)) > 80:
		return out, errors.New("Give the window a title of 1–80 characters.")
	case len([]rune(out.Message)) > 500:
		return out, errors.New("The message can be at most 500 characters.")
	}
	switch in.Kind {
	case store.MaintenanceOnce:
		if in.StartsAt.IsZero() || in.EndsAt.IsZero() {
			return out, errors.New("Pick when the maintenance starts and ends.")
		}
		if !in.EndsAt.After(in.StartsAt) {
			return out, errors.New("The end must be after the start.")
		}
		if err := checkDuration(in.EndsAt.Sub(in.StartsAt)); err != nil {
			return out, err
		}
		if isNew && !in.EndsAt.After(now) {
			return out, errors.New("That time has already passed.")
		}
		out.StartsAt, out.EndsAt = in.StartsAt.UTC().Truncate(time.Second), in.EndsAt.UTC().Truncate(time.Second)
	case store.MaintenanceWeekly:
		seen := map[int]bool{}
		for _, d := range in.Weekdays {
			if d < 0 || d > 6 {
				return out, errors.New("Weekdays go from 0 (Sunday) to 6 (Saturday).")
			}
			if !seen[d] {
				seen[d] = true
				out.Weekdays = append(out.Weekdays, d)
			}
		}
		sort.Ints(out.Weekdays)
		if len(out.Weekdays) == 0 {
			return out, errors.New("Pick at least one weekday.")
		}
		if _, ok := monitor.ParseClock(in.StartTime); !ok {
			return out, errors.New("Enter the start time as HH:MM, for example 02:00.")
		}
		if err := checkDuration(time.Duration(in.Duration) * time.Minute); err != nil {
			return out, err
		}
		tz := strings.TrimSpace(in.TimeZone)
		if tz == "" && hubTimeZone() != "Local" {
			tz = hubTimeZone()
		}
		if _, err := monitor.Location(tz); err != nil {
			return out, fmt.Errorf("Unknown time zone %q. Use an IANA name such as Europe/Prague.", tz)
		}
		out.StartTime, out.Duration, out.TZ = in.StartTime, in.Duration, tz
	default:
		return out, errors.New("Pick a one-off or a weekly window.")
	}
	sc, err := s.validateScope(ctx, in.Scope)
	if err != nil {
		return out, err
	}
	out.Scope = sc
	return out, nil
}

func checkDuration(d time.Duration) error {
	if d < monitor.MaintenanceMinDuration || d > monitor.MaintenanceMaxDuration {
		return errors.New("A maintenance window lasts from 5 minutes to 7 days.")
	}
	return nil
}

func (s *Server) validateScope(ctx context.Context, in store.MaintenanceScope) (store.MaintenanceScope, error) {
	if in.All {
		return store.MaintenanceScope{All: true}, nil
	}
	var out store.MaintenanceScope
	seenStack := map[store.MaintenanceStack]bool{}
	for _, st := range in.Stacks {
		if st.HostID == "" || !stackNameRe.MatchString(st.Stack) {
			return out, errors.New("Pick the host and the stack for every stack in the scope.")
		}
		if s.reg != nil {
			if _, _, err := s.reg.Get(ctx, st.HostID); errors.Is(err, host.ErrUnknownHost) {
				return out, errors.New("A host in the scope doesn't exist.")
			}
		}
		if !seenStack[st] {
			seenStack[st] = true
			out.Stacks = append(out.Stacks, st)
		}
	}
	seenCheck := map[int64]bool{}
	for _, id := range in.Checks {
		if seenCheck[id] {
			continue
		}
		if _, err := s.store.CheckByID(ctx, id); err != nil {
			return out, fmt.Errorf("Check %d doesn't exist.", id)
		}
		seenCheck[id] = true
		out.Checks = append(out.Checks, id)
	}
	if len(out.Stacks) == 0 && len(out.Checks) == 0 {
		return out, errors.New("Pick what the maintenance covers: everything, stacks or checks.")
	}
	return out, nil
}

func (s *Server) saveMaintenance(w http.ResponseWriter, r *http.Request) {
	var in maintInput
	if !readJSON(w, r, &in) {
		return
	}
	ctx := r.Context()
	id := int64(0)
	if r.Method == http.MethodPut {
		id = checkID(r)
	}
	mw, err := s.validateMaintenance(ctx, in, id == 0, time.Now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	mw.ID = id
	mw.CreatedBy = currentUser(r).Username
	newID, err := s.store.SaveMaintenanceWindow(ctx, mw)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Maintenance window not found.")
		return
	}
	if err != nil {
		s.engErr(w, err)
		return
	}
	s.maintenanceChanged(ctx)
	s.log.Info("maintenance window saved", "window", newID, "title", mw.Title, "user", currentUser(r).Username)
	writeJSON(w, http.StatusOK, map[string]int64{"id": newID})
}

func (s *Server) deleteMaintenance(w http.ResponseWriter, r *http.Request) {
	err := s.store.DeleteMaintenanceWindow(r.Context(), checkID(r))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Maintenance window not found.")
		return
	}
	if err != nil {
		s.engErr(w, err)
		return
	}
	s.maintenanceChanged(r.Context())
	s.log.Info("maintenance window deleted", "window", checkID(r), "user", currentUser(r).Username)
	w.WriteHeader(http.StatusNoContent)
}

// maintenanceChanged applies an edit right away: the monitor's index, status page caches
// and the agents' copy of an "everything" window.
func (s *Server) maintenanceChanged(ctx context.Context) {
	if s.mon != nil {
		s.mon.ReloadMaintenance(context.WithoutCancel(ctx))
	}
	s.status.Invalidate()
	if s.hub != nil {
		go s.hub.PushWatchdog(context.Background())
	}
}

type stackMaintEvent struct {
	At     time.Time `json:"at"`
	Detail string    `json:"detail"`
}

type stackMaintDTO struct {
	Active *activeMaintDTO   `json:"active,omitempty"`
	Events []stackMaintEvent `json:"events"`
}

type activeMaintDTO struct {
	ID    int64     `json:"id"`
	Title string    `json:"title"`
	Until time.Time `json:"until"`
}

// stackMaintenance returns the window running over a stack (for the pill) and the
// stack's maintenance events of the last 30 days (for its Logbook).
func (s *Server) stackMaintenance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hostID, stack := r.PathValue("host"), r.PathValue("name")
	out := stackMaintDTO{Events: []stackMaintEvent{}}
	now := time.Now()
	if s.mon != nil {
		for _, a := range s.mon.ActiveWindows(now) {
			if monitor.CoversStack(a.Window, hostID, stack) && (out.Active == nil || a.End.After(out.Active.Until)) {
				out.Active = &activeMaintDTO{ID: a.Window.ID, Title: a.Window.Title, Until: a.End}
			}
		}
	}
	evs, err := s.store.Events(ctx, hostID, stack, now.Add(-30*24*time.Hour), monitor.EventMaintenance)
	if err != nil {
		s.engErr(w, err)
		return
	}
	for i := len(evs) - 1; i >= 0 && len(out.Events) < 20; i-- {
		out.Events = append(out.Events, stackMaintEvent{At: evs[i].TS, Detail: evs[i].Detail})
	}
	writeJSON(w, http.StatusOK, out)
}
