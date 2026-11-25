package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"moorage/internal/host"
	"moorage/internal/monitor"
	"moorage/internal/notify"
	"moorage/internal/store"
)

// ---- checks ----------------------------------------------------------------

type checkInput struct {
	Name     string           `json:"name"`
	Kind     string           `json:"kind"`
	Target   string           `json:"target"`
	HostID   string           `json:"hostId"`
	Stack    string           `json:"stack"`
	Interval int              `json:"interval"`
	Timeout  int              `json:"timeout"`
	Retries  int              `json:"retries"`
	Settings monitor.Settings `json:"settings"`
	Enabled  bool             `json:"enabled"`
	Channels []int64          `json:"channels"`
	// Locations are where an HTTP/TCP/ping/DNS check runs from ("hub" and agent ids);
	// empty means the hub only. Quorum: majority, any or all (only with 2+ locations).
	Locations []string `json:"locations"`
	Quorum    string   `json:"quorum"`
}

type checkDTO struct {
	checkInput
	ID        int64            `json:"id"`
	State     monitor.State    `json:"state"`
	Uptime24h *float64         `json:"uptime24h"`
	Ticks            []monitor.Tick   `json:"ticks"`
	HostName         string           `json:"hostName,omitempty"`
	StackDisplayName string           `json:"stackDisplayName,omitempty"`
	Incidents        []incidentDTO    `json:"incidents,omitempty"`
	Anchors   []monitor.Anchor `json:"anchors,omitempty"`
	Uptime7d  *float64         `json:"uptime7d,omitempty"`
	Uptime30d *float64         `json:"uptime30d,omitempty"`
	// Maintenance is the planned window running over the check now, if any.
	Maintenance *activeMaintDTO `json:"maintenance,omitempty"`
	// LocationTicks are the newest results per location of a multi-location check.
	LocationTicks map[string][]monitor.Tick `json:"locationTicks,omitempty"`
}

type incidentDTO struct {
	ID        int64                 `json:"id"`
	StartedAt time.Time             `json:"startedAt"`
	EndedAt   time.Time             `json:"endedAt,omitzero"`
	Cause     string                `json:"cause"`
	Context   *monitor.AlertContext `json:"context,omitempty"`
}

var stackNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

func (s *Server) validateCheck(ctx context.Context, in *checkInput) error {
	in.Name = strings.TrimSpace(in.Name)
	in.Target = strings.TrimSpace(in.Target)
	switch {
	case in.Name == "" || len([]rune(in.Name)) > 60:
		return errors.New("Give the check a name of 1–60 characters.")
	case !monitor.Kinds[in.Kind]:
		return errors.New("Pick a check type.")
	case in.Kind != monitor.KindLog && (in.Target == "" || len(in.Target) > 500):
		return errors.New("Fill in what to check.")
	case len(in.Target) > 500:
		return errors.New("The container name is too long.")
	case in.Interval < 20 || in.Interval > 86400:
		return errors.New("The interval must be between 20 seconds and 24 hours.")
	case in.Timeout < 1 || in.Timeout > 60 || in.Timeout >= in.Interval:
		return errors.New("The timeout must be 1–60 seconds and shorter than the interval.")
	case in.Retries < 1 || in.Retries > 10:
		return errors.New("Alert after 1–10 failed checks in a row.")
	}
	st := &in.Settings
	switch in.Kind {
	case monitor.KindHTTP:
		if err := normalizeURLs(in); err != nil {
			return err
		}
		if st.Method != "" && st.Method != http.MethodGet && st.Method != http.MethodHead {
			return errors.New("Use GET or HEAD.")
		}
		if !monitor.ValidStatusSpec(st.Status) {
			return errors.New("Accepted status codes look like 200-299 or 200,301.")
		}
		if (st.JSONPath == "") != (st.JSONValue == "") && st.JSONPath == "" {
			return errors.New("Fill in the JSON path for the expected value.")
		}
		st.RecordType, st.Resolver, st.Expect = "", "", ""
	case monitor.KindTCP:
		h, p, err := net.SplitHostPort(in.Target)
		if n, perr := strconv.Atoi(p); err != nil || h == "" || perr != nil || n < 1 || n > 65535 {
			return errors.New("Use host:port, for example db.lan:5432.")
		}
		*st = monitor.Settings{AllowLinkLocal: st.AllowLinkLocal}
	case monitor.KindPing:
		if strings.ContainsAny(in.Target, "/: ") && net.ParseIP(in.Target) == nil {
			return errors.New("Ping needs a host name or IP address.")
		}
		*st = monitor.Settings{AllowLinkLocal: st.AllowLinkLocal}
	case monitor.KindDNS:
		switch strings.ToUpper(st.RecordType) {
		case "", "A", "AAAA", "CNAME", "MX", "TXT", "NS":
			st.RecordType = strings.ToUpper(st.RecordType)
		default:
			return errors.New("Pick a record type: A, AAAA, CNAME, MX, TXT or NS.")
		}
		if st.Resolver != "" {
			r := st.Resolver
			if hh, _, err := net.SplitHostPort(r); err == nil {
				r = hh
			}
			if _, err := netip.ParseAddr(r); err != nil {
				return errors.New("The DNS server must be an IP address.")
			}
		}
		*st = monitor.Settings{RecordType: st.RecordType, Resolver: st.Resolver, Expect: st.Expect, AllowLinkLocal: st.AllowLinkLocal}
	case monitor.KindStack:
		stacks := st.Stacks
		*st = monitor.Settings{}
		if err := s.normalizeStacks(ctx, in, stacks); err != nil {
			return err
		}
		if in.HostID == "" || !stackNameRe.MatchString(in.Target) {
			return errors.New("Pick the host and the stack to watch.")
		}
		in.Stack = in.Target // silence and anchors follow the (first) watched stack
	case monitor.KindLog:
		if in.HostID == "" || !stackNameRe.MatchString(in.Stack) {
			return errors.New("Pick the host and the stack to watch.")
		}
		if st.Pattern == "" {
			return errors.New("Enter a pattern to watch for.")
		}
		if len([]rune(st.Pattern)) > 256 {
			return errors.New("The pattern can be at most 256 characters.")
		}
		if _, err := st.LogFilter().Compile(); err != nil {
			return errors.New("Invalid regular expression: " + err.Error())
		}
		if st.Threshold == 0 {
			st.Threshold = 1
		}
		if st.Threshold < 1 || st.Threshold > 1000 {
			return errors.New("The match count must be between 1 and 1000.")
		}
		if st.Window == 0 {
			st.Window = 300
		}
		if st.Window < 10 || st.Window > 86400 {
			return errors.New("The window must be between 10 seconds and 24 hours.")
		}
		*st = monitor.Settings{
			Pattern: st.Pattern, Regex: st.Regex, CaseSensitive: st.CaseSensitive,
			Threshold: st.Threshold, Window: st.Window,
		}
	}
	if in.Kind != monitor.KindHTTP {
		st.URLs = nil
	}
	if in.Stack != "" && !stackNameRe.MatchString(in.Stack) {
		return errors.New("The linked stack name isn't valid.")
	}
	if in.Stack != "" && in.HostID == "" {
		return errors.New("Pick the host of the linked stack.")
	}
	if in.HostID != "" && s.reg != nil {
		if _, _, err := s.reg.Get(ctx, in.HostID); errors.Is(err, host.ErrUnknownHost) {
			return errors.New("That host doesn't exist.")
		}
	}
	if in.Stack == "" {
		in.HostID = ""
	}
	return s.validateLocations(ctx, in)
}

// normalizeURLs checks the URL(s) of an HTTP check. One URL is stored in Target only (as
// before multi-URL checks); 2–20 URLs are stored in settings.urls with the first one
// mirrored in Target.
func normalizeURLs(in *checkInput) error {
	urls := in.Settings.URLs
	if len(urls) == 0 {
		urls = []string{in.Target}
	}
	seen := map[string]bool{}
	var out []string
	for _, raw := range urls {
		raw = strings.TrimSpace(raw)
		if raw == "" || seen[raw] {
			continue
		}
		if len(raw) > 500 {
			return errors.New("A URL can be at most 500 characters.")
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%s: the URL must start with http:// or https://.", raw)
		}
		seen[raw] = true
		out = append(out, raw)
	}
	switch {
	case len(out) == 0:
		return errors.New("Fill in what to check.")
	case len(out) > monitor.MaxTargets:
		return fmt.Errorf("One check can watch at most %d URLs.", monitor.MaxTargets)
	}
	in.Target = out[0]
	in.Settings.URLs = nil
	if len(out) > 1 {
		in.Settings.URLs = out
	}
	return nil
}

// normalizeStacks checks the stacks of a stack check: like URLs, one stack lives in
// HostID/Target only, 2–20 (on any hosts) in settings.stacks.
func (s *Server) normalizeStacks(ctx context.Context, in *checkInput, stacks []monitor.StackTarget) error {
	if len(stacks) == 0 {
		return nil
	}
	seen := map[monitor.StackTarget]bool{}
	var out []monitor.StackTarget
	for _, t := range stacks {
		t.HostID, t.Stack = strings.TrimSpace(t.HostID), strings.TrimSpace(t.Stack)
		if seen[t] {
			continue
		}
		if t.HostID == "" || !stackNameRe.MatchString(t.Stack) {
			return errors.New("Pick the host and the stack to watch.")
		}
		if s.reg != nil {
			if _, _, err := s.reg.Get(ctx, t.HostID); errors.Is(err, host.ErrUnknownHost) {
				return errors.New("That host doesn't exist.")
			}
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) > monitor.MaxTargets {
		return fmt.Errorf("One check can watch at most %d stacks.", monitor.MaxTargets)
	}
	in.HostID, in.Target = out[0].HostID, out[0].Stack
	if len(out) > 1 {
		in.Settings.Stacks = out
	}
	return nil
}

// validateLocations normalises the locations of a check: only HTTP/TCP/ping/DNS checks
// run elsewhere, and a hub-only list is stored empty so such a check behaves exactly as
// before. Offline agents may be picked (they just don't vote until they are back).
func (s *Server) validateLocations(ctx context.Context, in *checkInput) error {
	if !monitor.RemoteKinds[in.Kind] {
		in.Locations, in.Quorum = nil, ""
		return nil
	}
	seen := map[string]bool{}
	var locs []string
	for _, l := range in.Locations {
		l = strings.TrimSpace(l)
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		if l != monitor.LocationHub {
			if s.reg == nil {
				return errors.New("Agents aren't available for checks here.")
			}
			_, e, err := s.reg.Get(ctx, l)
			if errors.Is(err, host.ErrUnknownHost) || (err == nil && e.Kind != "agent") {
				return errors.New("Pick the hub or agents as locations.")
			}
		}
		locs = append(locs, l)
	}
	if len(in.Locations) > 0 && len(locs) == 0 {
		return errors.New("Pick at least one location to check from.")
	}
	if len(locs) > monitor.MaxLocations {
		return fmt.Errorf("A check can run from at most %d locations.", monitor.MaxLocations)
	}
	if len(locs) == 1 && locs[0] == monitor.LocationHub {
		locs = nil
	}
	in.Locations = locs
	switch {
	case len(locs) < 2:
		in.Quorum = ""
	case in.Quorum == "":
		in.Quorum = monitor.QuorumMajority
	case !monitor.Quorums[in.Quorum]:
		return errors.New("Pick when the check counts as down: majority, any or all.")
	}
	return nil
}

func toStoreCheck(id int64, in checkInput) store.Check {
	b, _ := json.Marshal(in.Settings)
	return store.Check{
		ID: id, Name: in.Name, Kind: in.Kind, Target: in.Target, HostID: in.HostID, Stack: in.Stack,
		Interval: in.Interval, Timeout: in.Timeout, Retries: in.Retries, Settings: string(b), Enabled: in.Enabled, Channels: in.Channels,
		Locations: in.Locations, Quorum: in.Quorum,
	}
}

func (s *Server) checkDTO(ctx context.Context, c store.Check, ticks int) checkDTO {
	d := checkDTO{
		checkInput: checkInput{
			Name: c.Name, Kind: c.Kind, Target: c.Target, HostID: c.HostID, Stack: c.Stack,
			Interval: c.Interval, Timeout: c.Timeout, Retries: c.Retries, Settings: monitor.ParseSettings(c.Settings),
			Enabled: c.Enabled, Channels: c.Channels, Locations: c.Locations, Quorum: c.Quorum,
		},
		ID: c.ID, State: s.mon.State(c.ID),
	}
	if d.Channels == nil {
		d.Channels = []int64{}
	}
	if d.Locations == nil {
		d.Locations = []string{}
	}
	if a, ok := s.mon.MaintenanceFor(c, time.Now()); ok {
		d.Maintenance = &activeMaintDTO{ID: a.Window.ID, Title: a.Window.Title, Until: a.End}
	}
	if up, n, err := s.store.Uptime(ctx, c.ID, time.Now().Add(-24*time.Hour)); err == nil && n > 0 {
		d.Uptime24h = &up
	}
	d.Ticks, _ = s.mon.Ticks(ctx, c.ID, ticks)
	if d.Ticks == nil {
		d.Ticks = []monitor.Tick{}
	}
	if c.HostID != "" && s.reg != nil {
		_, e, _ := s.reg.Get(ctx, c.HostID)
		d.HostName = e.Name
	}
	if c.HostID != "" && c.Stack != "" {
		d.StackDisplayName = s.StackDisplayName(ctx, c.HostID, c.Stack)
	}
	return d
}

func (s *Server) listChecks(w http.ResponseWriter, r *http.Request) {
	checks, err := s.store.Checks(r.Context())
	if err != nil {
		s.engErr(w, err)
		return
	}
	out := make([]checkDTO, 0, len(checks))
	for _, c := range checks {
		out = append(out, s.checkDTO(r.Context(), c, 40))
	}
	writeJSON(w, http.StatusOK, out)
}

func checkID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func (s *Server) getCheck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := s.store.CheckByID(ctx, checkID(r))
	if err != nil {
		writeErr(w, http.StatusNotFound, "Check not found.")
		return
	}
	d := s.checkDTO(ctx, c, 90)
	for _, p := range []struct {
		dst **float64
		d   time.Duration
	}{{&d.Uptime7d, 7 * 24 * time.Hour}, {&d.Uptime30d, 30 * 24 * time.Hour}} {
		if up, n, err := s.store.Uptime(ctx, c.ID, time.Now().Add(-p.d)); err == nil && n > 0 {
			*p.dst = &up
		}
	}
	incs, _ := s.store.Incidents(ctx, c.ID, 20)
	d.Incidents = []incidentDTO{}
	for _, in := range incs {
		dto := incidentDTO{ID: in.ID, StartedAt: in.StartedAt, EndedAt: in.EndedAt, Cause: in.Cause}
		var ac monitor.AlertContext
		if json.Unmarshal([]byte(in.Context), &ac) == nil && (ac.Stack != "" || len(ac.Logs) > 0 || len(ac.Matches) > 0 || len(ac.Locations) > 0) {
			dto.Context = &ac
		}
		d.Incidents = append(d.Incidents, dto)
	}
	if c.Stack != "" && len(d.Ticks) > 0 {
		d.Anchors, _ = s.mon.Anchors(ctx, c.HostID, c.Stack, d.Ticks[0].At)
	}
	d.LocationTicks, _ = s.mon.LocationTicks(ctx, c, 90)
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) saveCheck(w http.ResponseWriter, r *http.Request) {
	var in checkInput
	if !readJSON(w, r, &in) {
		return
	}
	if err := s.validateCheck(r.Context(), &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id := int64(0)
	if r.Method == http.MethodPut {
		id = checkID(r)
	}
	newID, err := s.store.SaveCheck(r.Context(), toStoreCheck(id, in))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Check not found.")
		return
	}
	if err != nil {
		s.engErr(w, err)
		return
	}
	s.mon.Reload(newID)
	s.status.Invalidate()
	s.log.Info("check saved", "check", newID, "name", in.Name, "user", currentUser(r).Username)
	writeJSON(w, http.StatusOK, map[string]int64{"id": newID})
}

func (s *Server) deleteCheck(w http.ResponseWriter, r *http.Request) {
	id := checkID(r)
	if err := s.store.DeleteCheck(r.Context(), id); err != nil {
		s.engErr(w, err)
		return
	}
	s.mon.Reload(id)
	s.status.Invalidate()
	s.log.Info("check deleted", "check", id, "user", currentUser(r).Username)
	w.WriteHeader(http.StatusNoContent)
}

// testCheck runs a check definition once, without saving it.
func (s *Server) testCheck(w http.ResponseWriter, r *http.Request) {
	var in checkInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.Interval == 0 {
		in.Interval = 60
	}
	if in.Retries == 0 {
		in.Retries = 1
	}
	if err := s.validateCheck(r.Context(), &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out := s.mon.Test(r.Context(), toStoreCheck(0, in))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": out.OK, "paused": out.Paused, "msg": out.Msg,
		"latencyMs": out.Latency.Milliseconds(), "matches": out.Matches,
		"unknown": out.Unknown, "locations": out.Locations,
	})
}

// stackHealth returns the checks of one stack with their strips and deploy anchors.
func (s *Server) stackHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hostID, stack := r.PathValue("host"), r.PathValue("name")
	checks, err := s.store.Checks(ctx)
	if err != nil {
		s.engErr(w, err)
		return
	}
	out := []checkDTO{}
	from := time.Now()
	for _, c := range checks {
		if c.HostID != hostID || c.Stack != stack {
			continue
		}
		d := s.checkDTO(ctx, c, 60)
		if len(d.Ticks) > 0 && d.Ticks[0].At.Before(from) {
			from = d.Ticks[0].At
		}
		out = append(out, d)
	}
	anchors := []monitor.Anchor{}
	if len(out) > 0 {
		if a, err := s.mon.Anchors(ctx, hostID, stack, from); err == nil && a != nil {
			anchors = a
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"checks": out, "anchors": anchors})
}

// ---- channels --------------------------------------------------------------

type channelInput struct {
	ID      int64         `json:"id"`
	Name    string        `json:"name"`
	Kind    string        `json:"kind"`
	Config  notify.Config `json:"config"`
	Enabled bool          `json:"enabled"`
}

func channelDTO(c store.Channel) channelInput {
	var cfg notify.Config
	json.Unmarshal([]byte(c.Config), &cfg) //nolint:errcheck
	return channelInput{ID: c.ID, Name: c.Name, Kind: c.Kind, Config: cfg.Masked(c.Kind), Enabled: c.Enabled}
}

func (s *Server) listChannels(w http.ResponseWriter, r *http.Request) {
	chs, err := s.store.Channels(r.Context())
	if err != nil {
		s.engErr(w, err)
		return
	}
	out := make([]channelInput, 0, len(chs))
	for _, c := range chs {
		out = append(out, channelDTO(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// channelFromRequest validates input and restores secrets the UI sent back masked.
func (s *Server) channelFromRequest(ctx context.Context, in channelInput) (store.Channel, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len([]rune(in.Name)) > 60 {
		return store.Channel{}, errors.New("Give the channel a name of 1–60 characters.")
	}
	if in.ID != 0 {
		old, err := s.store.ChannelByID(ctx, in.ID)
		if err != nil {
			return store.Channel{}, errors.New("Channel not found.")
		}
		var oldCfg notify.Config
		json.Unmarshal([]byte(old.Config), &oldCfg) //nolint:errcheck
		if old.Kind == in.Kind {
			in.Config.KeepSecrets(in.Kind, oldCfg)
		}
	}
	if err := notify.Validate(in.Kind, in.Config); err != nil {
		return store.Channel{}, errors.New(sentence(err))
	}
	b, _ := json.Marshal(in.Config)
	return store.Channel{ID: in.ID, Name: in.Name, Kind: in.Kind, Config: string(b), Enabled: in.Enabled}, nil
}

func (s *Server) saveChannel(w http.ResponseWriter, r *http.Request) {
	var in channelInput
	if !readJSON(w, r, &in) {
		return
	}
	in.ID = 0
	if r.Method == http.MethodPut {
		in.ID = checkID(r)
	}
	ch, err := s.channelFromRequest(r.Context(), in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := s.store.SaveChannel(r.Context(), ch)
	if err != nil {
		s.engErr(w, err)
		return
	}
	s.log.Info("notification channel saved", "channel", id, "kind", ch.Kind, "user", currentUser(r).Username)
	if s.hub != nil {
		go s.hub.PushWatchdog(context.Background())
	}
	writeJSON(w, http.StatusOK, map[string]int64{"id": id})
}

func (s *Server) deleteChannel(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteChannel(r.Context(), checkID(r)); err != nil {
		s.engErr(w, err)
		return
	}
	if s.hub != nil {
		go s.hub.PushWatchdog(context.Background())
	}
	w.WriteHeader(http.StatusNoContent)
}

// testChannel sends a test message with the (possibly unsaved) form values.
func (s *Server) testChannel(w http.ResponseWriter, r *http.Request) {
	var in channelInput
	if !readJSON(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		in.Name = "test"
	}
	ch, err := s.channelFromRequest(r.Context(), in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.mon.TestChannel(r.Context(), ch); err != nil {
		writeErr(w, http.StatusBadGateway, "Sending failed: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// stackEvent feeds the monitor (deploy silence, strip anchors) when it runs.
func (s *Server) stackEvent(r *http.Request, stack, kind, detail string) {
	if s.mon != nil {
		s.mon.StackEvent(r.PathValue("host"), stack, kind, currentUser(r).Username, detail)
	}
}

// actionEvent maps compose actions to stack events.
var actionEvent = map[string]string{"up": "deploy", "rolling": "deploy", "rolling-update": "update", "update": "update", "restart": "restart", "stop": "stop", "down": "down", "pull": "pull"}

// ---- hub watchdog ----------------------------------------------------------

type watchdogAgentDTO struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Connected    bool   `json:"connected"`
	Participates bool   `json:"participates"`
	Addr         string `json:"addr,omitempty"`
	Version      string `json:"version,omitempty"`
	Supported    bool   `json:"supported"`
}

type watchdogDTO struct {
	Enabled          bool               `json:"enabled"`
	ChannelID        int64              `json:"channelId"`
	Grace            int                `json:"grace"`
	MaintenanceUntil time.Time          `json:"maintenanceUntil,omitzero"`
	Agents           []watchdogAgentDTO `json:"agents"`
}

func (s *Server) getWatchdog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ws, err := s.store.Watchdog(ctx)
	if err != nil {
		s.engErr(w, err)
		return
	}
	agents, err := s.store.Agents(ctx)
	if err != nil {
		s.engErr(w, err)
		return
	}
	out := watchdogDTO{
		Enabled:          ws.Enabled,
		ChannelID:        ws.ChannelID,
		Grace:            ws.Grace,
		MaintenanceUntil: ws.MaintenanceUntil,
		Agents:           make([]watchdogAgentDTO, 0, len(agents)),
	}
	for _, a := range agents {
		dto := watchdogAgentDTO{
			ID:           a.ID,
			Name:         a.Name,
			Participates: a.Watchdog,
			Addr:         a.WatchdogAddr,
			Supported:    len(a.WatchdogCert) > 0 || a.WatchdogAddr != "",
		}
		if s.reg != nil {
			if _, info, ok := s.reg.Remote(a.ID); ok {
				dto.Connected = true
				dto.Version = info.Version
				dto.Supported = info.Watchdog
				if info.WatchdogAddr != "" {
					dto.Addr = info.WatchdogAddr
				}
			}
		}
		out.Agents = append(out.Agents, dto)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) saveWatchdog(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled   bool            `json:"enabled"`
		ChannelID int64           `json:"channelId"`
		Grace     int             `json:"grace"`
		Agents    map[string]bool `json:"agents"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Grace == 0 {
		in.Grace = 120
	}
	if in.Grace < 60 || in.Grace > 3600 {
		writeErr(w, http.StatusBadRequest, "The grace period must be between 1 and 60 minutes.")
		return
	}
	ctx := r.Context()
	if in.Enabled && in.ChannelID <= 0 {
		writeErr(w, http.StatusBadRequest, "Pick a notification channel for the watchdog.")
		return
	}
	if in.ChannelID > 0 {
		ch, err := s.store.ChannelByID(ctx, in.ChannelID)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "That notification channel doesn't exist.")
			return
		}
		if in.Enabled && !ch.Enabled {
			writeErr(w, http.StatusBadRequest, "Enable the notification channel first.")
			return
		}
	}
	agents, err := s.store.Agents(ctx)
	if err != nil {
		s.engErr(w, err)
		return
	}
	byID := map[string]store.Agent{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	selected := 0
	var missingAddr []string
	for id, on := range in.Agents {
		if !on {
			continue
		}
		a, ok := byID[id]
		if !ok {
			continue
		}
		selected++
		addr := a.WatchdogAddr
		if s.reg != nil {
			if _, info, ok := s.reg.Remote(id); ok {
				if !info.Watchdog {
					writeErr(w, http.StatusBadRequest, a.Name+" doesn't support the watchdog yet. Update its agent.")
					return
				}
				if info.WatchdogAddr != "" {
					addr = info.WatchdogAddr
				}
			}
		}
		if addr == "" {
			missingAddr = append(missingAddr, a.Name)
		}
	}
	if in.Enabled && selected == 0 {
		writeErr(w, http.StatusBadRequest, "Pick at least one agent to watch the hub.")
		return
	}
	if selected >= 2 && len(missingAddr) > 0 {
		writeErr(w, http.StatusBadRequest, "With 2 or more watching agents, every participant needs MOORAGE_WATCHDOG_ADDR ("+strings.Join(missingAddr, ", ")+").")
		return
	}
	cur, _ := s.store.Watchdog(ctx)
	ws := store.WatchdogSettings{
		Enabled:          in.Enabled,
		ChannelID:        in.ChannelID,
		Grace:            in.Grace,
		MaintenanceUntil: cur.MaintenanceUntil,
	}
	if err := s.store.SaveWatchdog(ctx, ws, in.Agents); err != nil {
		s.engErr(w, err)
		return
	}
	if s.hub != nil {
		go s.hub.PushWatchdog(context.Background())
	}
	s.log.Info("watchdog settings saved", "enabled", in.Enabled, "agents", selected, "user", currentUser(r).Username)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) watchdogMaintenance(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Minutes int `json:"minutes"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Minutes < 0 || in.Minutes > 7*24*60 {
		writeErr(w, http.StatusBadRequest, "Maintenance can be at most 7 days.")
		return
	}
	var until time.Time
	if in.Minutes > 0 {
		until = time.Now().Add(time.Duration(in.Minutes) * time.Minute)
	}
	if err := s.store.SetWatchdogMaintenance(r.Context(), until); err != nil {
		s.engErr(w, err)
		return
	}
	if s.hub != nil {
		go s.hub.PushWatchdog(context.Background())
	}
	s.log.Info("watchdog maintenance updated", "minutes", in.Minutes, "user", currentUser(r).Username)
	writeJSON(w, http.StatusOK, map[string]any{"maintenanceUntil": until})
}

func (s *Server) testWatchdog(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ChannelID int64 `json:"channelId"`
	}
	if r.ContentLength != 0 {
		if !readJSON(w, r, &in) {
			return
		}
	}
	ctx := r.Context()
	if in.ChannelID <= 0 {
		ws, err := s.store.Watchdog(ctx)
		if err != nil {
			s.engErr(w, err)
			return
		}
		in.ChannelID = ws.ChannelID
	}
	if in.ChannelID <= 0 {
		writeErr(w, http.StatusBadRequest, "Pick a notification channel first.")
		return
	}
	ch, err := s.store.ChannelByID(ctx, in.ChannelID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "That notification channel doesn't exist.")
		return
	}
	if s.hub == nil {
		writeErr(w, http.StatusConflict, "The agent port is off.")
		return
	}
	agentName, err := s.hub.TestWatchdog(ctx, ch)
	if err != nil {
		if agentName != "" {
			writeErr(w, http.StatusBadGateway, agentName+" couldn't send to "+ch.Name+": "+err.Error())
			return
		}
		writeErr(w, http.StatusBadGateway, sentence(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"agent": agentName})
}
