// Package status builds, caches and renders public status pages and their RSS feeds.
//
// Everything rendered or returned to the public comes strictly from PublicPage,
// a dedicated DTO that never holds internal check names, targets, hostnames,
// stack names, container names, probe messages, incident causes or log lines.
package status

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/fnv"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"moorage/internal/monitor"
	"moorage/internal/store"
	"moorage/internal/web"
)

// Public status codes (mapped from internal check states).
const (
	StateOperational = "operational"
	StateDown        = "down"
	StateDegraded    = "degraded" // a multi-target check where some targets fail
	StateMaintenance = "maintenance"
	StatePaused      = "paused"
	StateNoData      = "nodata"
)

// DefaultTTL is how long a built public page stays cached before querying SQLite again.
const DefaultTTL = 60 * time.Second

// PublicPage is the only data structure exposed to public HTML, RSS and preview JSON.
type PublicPage struct {
	Slug          string           `json:"slug"`
	Title         string           `json:"title"`
	Description   string           `json:"description,omitempty"`
	BannerStatus  string           `json:"bannerStatus"`
	BannerText    string           `json:"bannerText"`
	UpdatedAt     time.Time        `json:"updatedAt"`
	UpdatedText   string           `json:"updatedText"`
	ShowIncidents bool             `json:"showIncidents"`
	Sections      []PublicSection  `json:"sections"`
	Incidents     []PublicIncident `json:"incidents,omitempty"`
	// Maintenance lists the planned windows running now that cover an item of this page;
	// Planned lists those starting within the next 7 days.
	Maintenance []PublicMaintenance `json:"maintenance,omitempty"`
	Planned     []PublicMaintenance `json:"planned,omitempty"`
	FeedItems   []FeedEvent         `json:"-"`
}

// PublicMaintenance is one occurrence of a maintenance window. Title and message are
// written by the admin for the public; scope details (stacks, checks) never leave the hub.
type PublicMaintenance struct {
	Title      string    `json:"title"`
	Message    string    `json:"message,omitempty"`
	StartsAt   time.Time `json:"startsAt"`
	StartsText string    `json:"startsText"`
	EndsAt     time.Time `json:"endsAt"`
	EndsText   string    `json:"endsText"`
	Items      []string  `json:"items"` // display names of the affected items on this page
}

// PublicSection groups consecutive items that share a section heading.
type PublicSection struct {
	Name  string       `json:"name,omitempty"`
	Items []PublicItem `json:"items"`
}

// PublicItem is one service row on the public page.
type PublicItem struct {
	DisplayName  string      `json:"displayName"`
	Status       string      `json:"status"`
	StatusLabel  string      `json:"statusLabel"`
	Uptime24h    *float64    `json:"uptime24h"`
	Uptime30d    *float64    `json:"uptime30d"`
	Uptime24hStr string      `json:"uptime24hText"`
	Uptime30dStr string      `json:"uptime30dText"`
	Days         []PublicDay `json:"days"`
}

// PublicDay is one of the 30 daily bars on the status strip (UTC days).
type PublicDay struct {
	Date    string   `json:"date"`
	Status  string   `json:"status"`
	Uptime  *float64 `json:"uptime,omitempty"`
	Tooltip string   `json:"tooltip"`
}

// PublicIncident is a sanitized incident entry without Cause or Context.
type PublicIncident struct {
	DisplayName string    `json:"displayName"`
	StartedAt   time.Time `json:"startedAt"`
	StartedText string    `json:"startedText"`
	EndedAt     time.Time `json:"endedAt,omitzero"`
	EndedText   string    `json:"endedText,omitempty"`
	Ongoing     bool      `json:"ongoing"`
	Duration    string    `json:"duration"`
}

// FeedEvent is one RSS <item> derived from an incident start or recovery.
type FeedEvent struct {
	GUID    string
	Title   string
	Desc    string
	PubDate time.Time
}

// Service manages public status page building, caching, routing and rendering.
type Service struct {
	st              *store.Store
	mon             *monitor.Monitor
	publicURL       string
	statusPublicURL string

	TTL time.Duration
	Now func() time.Time

	// OnRebuild is an optional hook called whenever build() runs (used by tests to count DB builds).
	OnRebuild func(pageID int64)

	fonts map[string]fontAsset

	mu       sync.Mutex
	cache    map[int64]cacheEntry
	inflight map[int64]*flight
	index    *pageIndex // nil until loaded; reset by Invalidate
}

// pageIndex keeps every status page (with items) in memory, so routing a request –
// including every admin UI request, which must be checked against status domains –
// never queries SQLite. Pages change only through the admin API, which invalidates it.
type pageIndex struct {
	bySlug   map[string]store.StatusPage
	byDomain map[string]store.StatusPage
}

type fontAsset struct {
	distPath string
	data     []byte
}

type cacheEntry struct {
	data      PublicPage
	expiresAt time.Time
}

type flight struct {
	done chan struct{}
	data PublicPage
	err  error
}

// New creates a status page service and discovers embedded woff2 fonts from web.Dist().
func New(st *store.Store, mon *monitor.Monitor, publicURL string) *Service {
	s := &Service{
		st:        st,
		mon:       mon,
		publicURL: strings.TrimRight(publicURL, "/"),
		TTL:       DefaultTTL,
		cache:     map[int64]cacheEntry{},
		inflight:  map[int64]*flight{},
		fonts:     discoverFonts(web.Dist()),
	}
	return s
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) ttl() time.Duration {
	if s.TTL > 0 {
		return s.TTL
	}
	return DefaultTTL
}

// Invalidate clears the cached public data and domain index (called after any status page edit or delete).
func (s *Service) Invalidate() {
	s.mu.Lock()
	clear(s.cache)
	s.index = nil
	s.mu.Unlock()
}

// SetPublicURL updates the hub public URL live and clears cached feed/page links.
func (s *Service) SetPublicURL(u string) {
	s.mu.Lock()
	s.publicURL = strings.TrimRight(u, "/")
	clear(s.cache)
	s.mu.Unlock()
}

// SetStatusPublicURL updates the separate status listener public URL live.
func (s *Service) SetStatusPublicURL(u string) {
	s.mu.Lock()
	s.statusPublicURL = strings.TrimRight(u, "/")
	clear(s.cache)
	s.mu.Unlock()
}

// pages returns the in-memory page index, loading it once after an invalidation.
func (s *Service) pages(ctx context.Context) (*pageIndex, error) {
	s.mu.Lock()
	idx := s.index
	s.mu.Unlock()
	if idx != nil {
		return idx, nil
	}
	all, err := s.st.StatusPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("load status pages: %w", err)
	}
	idx = &pageIndex{bySlug: map[string]store.StatusPage{}, byDomain: map[string]store.StatusPage{}}
	for _, p := range all {
		idx.bySlug[p.Slug] = p
		if p.Domain != "" {
			idx.byDomain[p.Domain] = p
		}
	}
	s.mu.Lock()
	s.index = idx
	s.mu.Unlock()
	return idx, nil
}

// MatchDomain reports whether the request Host header matches a configured status page domain.
// A domain of an unpublished page matches too, so the caller isolates it from the admin UI/API.
func (s *Service) MatchDomain(ctx context.Context, hostHeader string) (store.StatusPage, bool) {
	h := CleanHost(hostHeader)
	if h == "" {
		return store.StatusPage{}, false
	}
	idx, err := s.pages(ctx)
	if err != nil {
		return store.StatusPage{}, false
	}
	p, ok := idx.byDomain[h]
	return p, ok
}

// CleanHost lowercases h and strips any `:port` suffix or trailing dot.
func CleanHost(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	if h == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.TrimSuffix(h, ".")
}

// Preview builds the public view for a page ID (including unpublished drafts) without caching it as published.
func (s *Service) Preview(ctx context.Context, id int64) (PublicPage, error) {
	p, err := s.st.StatusPageByID(ctx, id)
	if err != nil {
		return PublicPage{}, err
	}
	return s.build(ctx, p, s.now())
}

// PreviewDraft builds the public view of unsaved settings, so the admin can check a
// page before saving it – saving a published page would make it public right away.
func (s *Service) PreviewDraft(ctx context.Context, p store.StatusPage) (PublicPage, error) {
	return s.build(ctx, p, s.now())
}

// Cached builds or returns the cached PublicPage for p, coalescing concurrent builds (singleflight).
func (s *Service) Cached(ctx context.Context, p store.StatusPage) (PublicPage, error) {
	now := s.now()
	s.mu.Lock()
	if ent, ok := s.cache[p.ID]; ok && now.Before(ent.expiresAt) {
		s.mu.Unlock()
		return ent.data, nil
	}
	if fl, ok := s.inflight[p.ID]; ok {
		s.mu.Unlock()
		<-fl.done
		return fl.data, fl.err
	}
	fl := &flight{done: make(chan struct{})}
	s.inflight[p.ID] = fl
	s.mu.Unlock()

	data, err := s.build(ctx, p, now)

	s.mu.Lock()
	delete(s.inflight, p.ID)
	if err == nil {
		s.cache[p.ID] = cacheEntry{data: data, expiresAt: now.Add(s.ttl())}
	}
	fl.data, fl.err = data, err
	close(fl.done)
	s.mu.Unlock()
	return data, err
}

// ---- Builder ---------------------------------------------------------------

type dayKey struct {
	checkID int64
	day     int64
}

func (s *Service) build(ctx context.Context, p store.StatusPage, now time.Time) (PublicPage, error) {
	if s.OnRebuild != nil {
		s.OnRebuild(p.ID)
	}
	allChecks, err := s.st.Checks(ctx)
	if err != nil {
		return PublicPage{}, err
	}
	byID := make(map[int64]store.Check, len(allChecks))
	byStack := make(map[string][]store.Check)
	for _, c := range allChecks {
		byID[c.ID] = c
		if c.Enabled && c.HostID != "" && c.Stack != "" {
			key := c.HostID + "/" + c.Stack
			byStack[key] = append(byStack[key], c)
		}
	}

	// Resolve which enabled check IDs back each item, and collect the union of check IDs to query.
	itemChecks := make([][]store.Check, len(p.Items))
	needSet := map[int64]bool{}
	var needIDs []int64
	addNeed := func(id int64) {
		if !needSet[id] {
			needSet[id] = true
			needIDs = append(needIDs, id)
		}
	}
	for i, it := range p.Items {
		if it.CheckID > 0 {
			if c, ok := byID[it.CheckID]; ok {
				itemChecks[i] = []store.Check{c}
				if c.Enabled {
					addNeed(c.ID)
				}
			}
		} else if it.HostID != "" && it.Stack != "" {
			cs := byStack[it.HostID+"/"+it.Stack]
			itemChecks[i] = cs
			for _, c := range cs {
				addNeed(c.ID)
			}
		}
	}

	todayDay := now.UnixMilli() / 86_400_000
	firstDay := todayDay - 29
	since30d := time.UnixMilli(firstDay * 86_400_000).UTC()
	since24h := now.Add(-24 * time.Hour)

	dailyRows, err := s.st.DailyCheckResults(ctx, needIDs, since30d)
	if err != nil {
		return PublicPage{}, err
	}
	up24Map, err := s.st.CheckUptimesSince(ctx, needIDs, since24h)
	if err != nil {
		return PublicPage{}, err
	}
	up30Map, err := s.st.CheckUptimesSince(ctx, needIDs, since30d)
	if err != nil {
		return PublicPage{}, err
	}

	// Index daily counts by (checkID, day).
	dailyMap := make(map[dayKey]store.DailyCheckCounts, len(dailyRows))
	for _, r := range dailyRows {
		dailyMap[dayKey{r.CheckID, r.Day}] = r
	}

	out := PublicPage{
		Slug:          p.Slug,
		Title:         p.Title,
		Description:   p.Description,
		UpdatedAt:     now,
		UpdatedText:   now.Format("2 Jan 2006, 15:04 UTC"),
		ShowIncidents: p.ShowIncidents,
		Sections:      []PublicSection{},
	}

	maint := s.pageMaintenance(p.Items, itemChecks, now)
	out.Maintenance, out.Planned = maint.active, maint.planned

	var flatItems []PublicItem
	for i, it := range p.Items {
		pubIt := s.buildItem(it.DisplayName, itemChecks[i], firstDay, todayDay, dailyMap, up24Map, up30Map)
		if maint.covered[i] {
			pubIt.Status, pubIt.StatusLabel = StateMaintenance, PublicStatusLabel(StateMaintenance)
		}
		flatItems = append(flatItems, pubIt)
		sec := strings.TrimSpace(it.Section)
		if len(out.Sections) == 0 || out.Sections[len(out.Sections)-1].Name != sec {
			out.Sections = append(out.Sections, PublicSection{Name: sec, Items: []PublicItem{pubIt}})
		} else {
			last := &out.Sections[len(out.Sections)-1]
			last.Items = append(last.Items, pubIt)
		}
	}

	out.BannerStatus, out.BannerText = computeBanner(flatItems)

	if p.ShowIncidents && len(needIDs) > 0 {
		rawIncs, err := s.st.IncidentSummariesSince(ctx, needIDs, since30d, 100)
		if err != nil {
			return PublicPage{}, err
		}
		out.Incidents, out.FeedItems = buildPublicIncidents(p.Items, itemChecks, rawIncs, since30d, now)
	}
	out.FeedItems = mergeFeed(out.FeedItems, maint.feed)

	return out, nil
}

func (s *Service) buildItem(
	displayName string,
	checks []store.Check,
	firstDay, todayDay int64,
	dailyMap map[dayKey]store.DailyCheckCounts,
	up24Map, up30Map map[int64][2]int,
) PublicItem {
	// Enabled checks determine live status and uptime.
	var enabled []store.Check
	for _, c := range checks {
		if c.Enabled {
			enabled = append(enabled, c)
		}
	}

	status := StateNoData
	for _, c := range enabled {
		st := mapCheckStatus(s.checkLiveStatus(c.ID))
		if statusRank(st) > statusRank(status) {
			status = st
		}
	}

	// A stack is only as available as its least available check. Summing all results
	// would show a stack whose container is dead all day (stack check 0 %) next to a
	// healthy log rule (100 %) as "50 %".
	u24 := minUptime(enabled, up24Map)
	u30 := minUptime(enabled, up30Map)

	days := make([]PublicDay, 30)
	for idx := int64(0); idx < 30; idx++ {
		d := firstDay + idx
		dateStr := time.UnixMilli(d * 86_400_000).UTC().Format("2 Jan 2006")
		var up, down, degraded, silenced, paused, maint int
		var dayUp *float64
		for _, c := range enabled {
			cnt := dailyMap[dayKey{c.ID, d}]
			up += cnt.Up
			down += cnt.Down
			degraded += cnt.Degraded
			silenced += cnt.Silenced
			paused += cnt.Paused
			maint += cnt.Maintenance
			if n := cnt.Up + cnt.Down + cnt.Degraded; n > 0 {
				if v := float64(cnt.Up) / float64(n); dayUp == nil || v < *dayUp {
					dayUp = &v
				}
			}
		}
		dayState := StateNoData
		switch {
		case down > 0:
			dayState = StateDown
		case degraded > 0:
			dayState = StateDegraded
		case maint > 0, silenced > 0 && up == 0:
			dayState = StateMaintenance
		case up > 0:
			dayState = StateOperational
		case paused > 0:
			dayState = StatePaused
		}
		var tip string
		if dayUp != nil {
			tip = fmt.Sprintf("%s · %s", dateStr, formatPct(dayUp))
		} else {
			tip = fmt.Sprintf("%s · %s", dateStr, PublicStatusLabel(dayState))
		}
		days[idx] = PublicDay{
			Date:    dateStr,
			Status:  dayState,
			Uptime:  dayUp,
			Tooltip: tip,
		}
	}

	return PublicItem{
		DisplayName:  displayName,
		Status:       status,
		StatusLabel:  PublicStatusLabel(status),
		Uptime24h:    u24,
		Uptime30d:    u30,
		Uptime24hStr: formatPct(u24),
		Uptime30dStr: formatPct(u30),
		Days:         days,
	}
}

// minUptime returns the lowest uptime among the checks that have results, nil without data.
func minUptime(checks []store.Check, counts map[int64][2]int) *float64 {
	var out *float64
	for _, c := range checks {
		v, ok := counts[c.ID]
		if !ok || v[1] == 0 {
			continue
		}
		if r := float64(v[0]) / float64(v[1]); out == nil || r < *out {
			out = &r
		}
	}
	return out
}

func (s *Service) checkLiveStatus(checkID int64) string {
	if s.mon == nil {
		return monitor.StatusUnknown
	}
	st := s.mon.State(checkID)
	if st.Last != nil && st.Last.Status == monitor.StatusSilenced && st.Status != monitor.StatusDown {
		return monitor.StatusSilenced
	}
	return st.Status
}

// mapCheckStatus converts an internal monitor status into a public status token.
func mapCheckStatus(internal string) string {
	switch internal {
	case monitor.StatusUp, monitor.StatusPending:
		return StateOperational
	case monitor.StatusDown:
		return StateDown
	case monitor.StatusDegraded:
		return StateDegraded
	case monitor.StatusSilenced, monitor.StatusMaintenance:
		return StateMaintenance
	case monitor.StatusPaused:
		return StatePaused
	default:
		return StateNoData
	}
}

// statusRank orders statuses by severity: Down > Maintenance > Operational > Paused > No data.
func statusRank(st string) int {
	switch st {
	case StateDown:
		return 6
	case StateDegraded:
		return 5
	case StateMaintenance:
		return 4
	case StateOperational:
		return 3
	case StatePaused:
		return 2
	default:
		return 1
	}
}

// PublicStatusLabel returns the public English label for a status code.
func PublicStatusLabel(st string) string {
	switch st {
	case StateOperational:
		return "Operational"
	case StateDown:
		return "Down"
	case StateDegraded:
		return "Partially down"
	case StateMaintenance:
		return "Maintenance"
	case StatePaused:
		return "Paused"
	default:
		return "No data"
	}
}

func computeBanner(items []PublicItem) (status, text string) {
	if len(items) == 0 {
		return StateNoData, "No services configured yet"
	}
	var down, degraded, maint, op int
	for _, it := range items {
		switch it.Status {
		case StateDown:
			down++
		case StateDegraded:
			degraded++
		case StateMaintenance:
			maint++
		case StateOperational:
			op++
		}
	}
	switch {
	case down > 0:
		word := "services"
		if len(items) == 1 {
			word = "service"
		}
		return StateDown, fmt.Sprintf("%d of %d %s down", down, len(items), word)
	case degraded > 0:
		word := "services are"
		if degraded == 1 {
			word = "service is"
		}
		return StateDegraded, fmt.Sprintf("%d %s partially down", degraded, word)
	case maint > 0:
		return StateMaintenance, "Maintenance in progress"
	case op > 0:
		return StateOperational, "All systems operational"
	default:
		return StateNoData, "No recent data"
	}
}

func buildPublicIncidents(
	items []store.StatusPageItem,
	itemChecks [][]store.Check,
	raw []store.IncidentSummary,
	since30d, now time.Time,
) ([]PublicIncident, []FeedEvent) {
	// Map each checkID to the page item positions that include it.
	checkItems := map[int64][]int{}
	for idx, cs := range itemChecks {
		for _, c := range cs {
			if c.Enabled {
				checkItems[c.ID] = append(checkItems[c.ID], idx)
			}
		}
	}
	perItem := map[int][]store.IncidentSummary{}
	for _, in := range raw {
		for _, idx := range checkItems[in.CheckID] {
			perItem[idx] = append(perItem[idx], in)
		}
	}

	var pub []PublicIncident
	var feed []FeedEvent
	seenGUID := map[string]bool{}
	guid := func(base string) string {
		g := base
		for n := 2; seenGUID[g]; n++ {
			g = fmt.Sprintf("%s-%d", base, n)
		}
		seenGUID[g] = true
		return g
	}
	for idx := range items {
		name := items[idx].DisplayName
		// One check can back several items (its own item and its stack's item), so the
		// GUID also names the item – by a hash of its public name, never its position.
		tag := nameTag(name)
		for _, out := range mergeOutages(perItem[idx], now) {
			ongoing := out.EndedAt.IsZero()
			end := out.EndedAt
			if ongoing {
				end = now
			}
			dur := FormatDuration(end.Sub(out.StartedAt))
			pi := PublicIncident{
				DisplayName: name,
				StartedAt:   out.StartedAt.UTC(),
				StartedText: out.StartedAt.UTC().Format("2 Jan 2006, 15:04 UTC"),
				Ongoing:     ongoing,
				Duration:    dur,
			}
			if !ongoing {
				pi.EndedAt = out.EndedAt.UTC()
				pi.EndedText = out.EndedAt.UTC().Format("2 Jan 2006, 15:04 UTC")
			}
			pub = append(pub, pi)

			// The GUID comes from the outage's first incident, not from the item's position,
			// so reordering items on the page doesn't make feed readers show old events again.
			if !out.StartedAt.Before(since30d) {
				feed = append(feed, FeedEvent{
					GUID:    guid(fmt.Sprintf("inc-%d-%s-down", out.ID, tag)),
					Title:   fmt.Sprintf("%s is down", name),
					Desc:    fmt.Sprintf("%s is down since %s.", name, out.StartedAt.UTC().Format("2 Jan 2006, 15:04 UTC")),
					PubDate: out.StartedAt.UTC(),
				})
			}
			if !ongoing && !out.EndedAt.Before(since30d) {
				feed = append(feed, FeedEvent{
					GUID:    guid(fmt.Sprintf("inc-%d-%s-up", out.ID, tag)),
					Title:   fmt.Sprintf("%s is back after %s", name, dur),
					Desc:    fmt.Sprintf("%s is operational again after %s.", name, dur),
					PubDate: out.EndedAt.UTC(),
				})
			}
		}
	}

	sort.SliceStable(pub, func(i, j int) bool { return pub[i].StartedAt.After(pub[j].StartedAt) })
	if len(pub) > 30 {
		pub = pub[:30]
	}
	sort.SliceStable(feed, func(i, j int) bool {
		if !feed[i].PubDate.Equal(feed[j].PubDate) {
			return feed[i].PubDate.After(feed[j].PubDate)
		}
		return feed[i].GUID > feed[j].GUID
	})
	if len(feed) > 50 {
		feed = feed[:50]
	}
	return pub, feed
}

// nameTag is a short stable hash of an item's public name for feed GUIDs.
func nameTag(name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return fmt.Sprintf("%08x", h.Sum32())
}

// mergeOutages joins overlapping incidents of one page item into one public outage.
// A stack item has several checks (stack, HTTP, log…) that usually fail together;
// the public must see one outage of that service, not one per internal check.
// The merged outage keeps the ID of its earliest incident (stable across rebuilds,
// because incidents are opened when they start). EndedAt is zero while any part is open.
func mergeOutages(in []store.IncidentSummary, now time.Time) []store.IncidentSummary {
	if len(in) == 0 {
		return nil
	}
	sorted := append([]store.IncidentSummary(nil), in...)
	sort.Slice(sorted, func(i, j int) bool {
		if !sorted[i].StartedAt.Equal(sorted[j].StartedAt) {
			return sorted[i].StartedAt.Before(sorted[j].StartedAt)
		}
		return sorted[i].ID < sorted[j].ID
	})
	endOf := func(x store.IncidentSummary) time.Time {
		if x.EndedAt.IsZero() {
			return now
		}
		return x.EndedAt
	}
	var out []store.IncidentSummary
	cur := sorted[0]
	for _, x := range sorted[1:] {
		if x.StartedAt.After(endOf(cur)) {
			out = append(out, cur)
			cur = x
			continue
		}
		if x.EndedAt.IsZero() {
			cur.EndedAt = time.Time{}
		} else if !cur.EndedAt.IsZero() && x.EndedAt.After(cur.EndedAt) {
			cur.EndedAt = x.EndedAt
		}
	}
	return append(out, cur)
}

// FormatDuration formats an outage duration without ever rounding a sub-minute outage to "0 min".
func FormatDuration(d time.Duration) string {
	secs := int(d.Round(time.Second).Seconds())
	if secs < 60 {
		return fmt.Sprintf("%d s", max(secs, 1))
	}
	mins := int((d + 30*time.Second) / time.Minute)
	if mins < 60 {
		return fmt.Sprintf("%d min", max(mins, 1))
	}
	h := mins / 60
	m := mins % 60
	if m == 0 {
		return fmt.Sprintf("%d h", h)
	}
	return fmt.Sprintf("%d h %d min", h, m)
}

func formatPct(v *float64) string {
	if v == nil {
		return "–"
	}
	p := *v * 100
	if p == 100 {
		return "100 %"
	}
	if p >= 99 {
		return fmt.Sprintf("%.2f %%", p)
	}
	return fmt.Sprintf("%.1f %%", p)
}

// ---- HTTP serving, headers, HTML and RSS -----------------------------------

const statusCSP = "default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self' data:; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

const notFoundBody = "404 page not found\n"

func setStatusHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", statusCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
}

// WriteNotFound writes the uniform 404 response for any unknown slug, unknown domain,
// unpublished page, or disallowed path on a status domain/listener.
func WriteNotFound(w http.ResponseWriter) {
	setStatusHeaders(w)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(notFoundBody))
}

func checkMethod(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		setStatusHeaders(w)
		w.Header().Set("Allow", "GET, HEAD")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte("method not allowed\n"))
		return false
	}
	return true
}

// Handler returns an HTTP handler that serves only status pages (both by custom domain
// and by /status/<slug>), never the admin API or SPA. Used for MOORAGE_STATUS_LISTEN.
func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !checkMethod(w, r) {
			return
		}
		if strings.HasPrefix(r.URL.Path, "/status-assets/") {
			s.ServeAsset(w, r)
			return
		}
		if p, matched := s.MatchDomain(r.Context(), r.Host); matched {
			s.serveDomainPage(w, r, p)
			return
		}
		s.ServeSlugRoute(w, r)
	})
}

// ServeCustomDomain handles a request on the main listener when r.Host matches a configured
// status page domain. It returns true if r.Host is a status page domain (and therefore the
// request has been completely handled and must NEVER fall through to the admin API or SPA).
func (s *Service) ServeCustomDomain(w http.ResponseWriter, r *http.Request) bool {
	p, matched := s.MatchDomain(r.Context(), r.Host)
	if !matched {
		return false
	}
	if !checkMethod(w, r) {
		return true
	}
	if strings.HasPrefix(r.URL.Path, "/status-assets/") {
		s.ServeAsset(w, r)
		return true
	}
	s.serveDomainPage(w, r, p)
	return true
}

func (s *Service) serveDomainPage(w http.ResponseWriter, r *http.Request, p store.StatusPage) {
	if !p.Published {
		WriteNotFound(w)
		return
	}
	switch r.URL.Path {
	case "/":
		s.serveHTML(w, r, p, true)
	case "/feed.xml":
		s.serveFeed(w, r, p, true)
	case "/favicon.ico", "/favicon.svg":
		s.serveFavicon(w)
	default:
		WriteNotFound(w)
	}
}

// ServeSlugRoute serves /status/<slug> and /status/<slug>/feed.xml.
func (s *Service) ServeSlugRoute(w http.ResponseWriter, r *http.Request) {
	if !checkMethod(w, r) {
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/status/")
	if p == r.URL.Path || p == "" {
		WriteNotFound(w)
		return
	}
	slug, rest, hasRest := strings.Cut(p, "/")
	if !ValidSlug(slug) || (hasRest && rest != "feed.xml") {
		WriteNotFound(w)
		return
	}
	idx, err := s.pages(r.Context())
	if err != nil {
		WriteNotFound(w)
		return
	}
	page, ok := idx.bySlug[slug]
	if !ok || !page.Published {
		WriteNotFound(w)
		return
	}
	if hasRest {
		s.serveFeed(w, r, page, false)
		return
	}
	s.serveHTML(w, r, page, false)
}

func (s *Service) pageLinks(r *http.Request, p store.StatusPage) (pageURL, feedURL string) {
	// A page with its own domain always links there: MOORAGE_PUBLIC_URL is the admin
	// UI's address, which a public feed must not reveal when the page has a domain.
	if p.Domain != "" {
		base := "https://" + p.Domain
		return base + "/", base + "/feed.xml"
	}
	s.mu.Lock()
	base := s.statusPublicURL
	if base == "" {
		base = s.publicURL
	}
	s.mu.Unlock()
	if base == "" {
		host := strings.TrimSpace(r.Host)
		if host == "" {
			host = "localhost"
		}
		base = "http://" + host
	}
	pageURL = base + "/status/" + p.Slug
	feedURL = pageURL + "/feed.xml"
	return pageURL, feedURL
}

func (s *Service) serveHTML(w http.ResponseWriter, r *http.Request, p store.StatusPage, onCustomDomain bool) {
	data, err := s.Cached(r.Context(), p)
	if err != nil {
		WriteNotFound(w)
		return
	}
	feedPath := "/status/" + p.Slug + "/feed.xml"
	if onCustomDomain {
		feedPath = "/feed.xml"
	}
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, struct {
		Page     PublicPage
		FeedHref string
	}{Page: data, FeedHref: feedPath}); err != nil {
		WriteNotFound(w)
		return
	}
	setStatusHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(buf.Bytes())
	}
}

type rssDoc struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	AtomNS  string     `xml:"xmlns:atom,attr"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title         string    `xml:"title"`
	Link          string    `xml:"link"`
	Description   string    `xml:"description"`
	LastBuildDate string    `xml:"lastBuildDate"`
	AtomLink      atomLink  `xml:"atom:link"`
	Items         []rssItem `xml:"item"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

type rssItem struct {
	Title       string  `xml:"title"`
	Link        string  `xml:"link"`
	Description string  `xml:"description"`
	PubDate     string  `xml:"pubDate"`
	GUID        rssGUID `xml:"guid"`
}

type rssGUID struct {
	IsPermaLink string `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}

func (s *Service) serveFeed(w http.ResponseWriter, r *http.Request, p store.StatusPage, onCustomDomain bool) {
	data, err := s.Cached(r.Context(), p)
	if err != nil {
		WriteNotFound(w)
		return
	}
	pageURL, feedURL := s.pageLinks(r, p)
	desc := data.Description
	if desc == "" {
		desc = data.Title + " status updates"
	}
	// Incident items exist only when the page shows incidents (build leaves them out
	// otherwise); maintenance starts are always announced.
	items := make([]rssItem, 0, len(data.FeedItems))
	for _, ev := range data.FeedItems {
		items = append(items, rssItem{
			Title:       ev.Title,
			Link:        pageURL,
			Description: ev.Desc,
			PubDate:     ev.PubDate.UTC().Format(time.RFC1123Z),
			GUID:        rssGUID{IsPermaLink: "false", Value: ev.GUID},
		})
	}
	doc := rssDoc{
		Version: "2.0",
		AtomNS:  "http://www.w3.org/2005/Atom",
		Channel: rssChannel{
			Title:         data.Title,
			Link:          pageURL,
			Description:   desc,
			LastBuildDate: data.UpdatedAt.UTC().Format(time.RFC1123Z),
			AtomLink:      atomLink{Href: feedURL, Rel: "self", Type: "application/rss+xml"},
			Items:         items,
		},
	}
	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		WriteNotFound(w)
		return
	}
	setStatusHeaders(w)
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(xml.Header))
		_, _ = w.Write(out)
		_, _ = w.Write([]byte("\n"))
	}
}

// ---- Static assets (/status-assets/*) --------------------------------------

// ServeAsset serves /status-assets/status.css, /status-assets/status.js, favicon.svg,
// and the woff2 font files discovered from the embedded SPA build.
func (s *Service) ServeAsset(w http.ResponseWriter, r *http.Request) {
	if !checkMethod(w, r) {
		return
	}
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/status-assets/")
	setStatusHeaders(w)
	switch name {
	case "status.css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write([]byte(s.cssBytes()))
	case "status.js":
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write([]byte(statusJS))
	case "favicon.svg":
		s.serveFavicon(w)
	default:
		fa, ok := s.fonts[name]
		if !ok {
			WriteNotFound(w)
			return
		}
		w.Header().Set("Content-Type", "font/woff2")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		_, _ = w.Write(fa.data)
	}
}

func (s *Service) serveFavicon(w http.ResponseWriter) {
	setStatusHeaders(w)
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write([]byte(faviconSVG))
}

func discoverFonts(dist fs.FS) map[string]fontAsset {
	out := map[string]fontAsset{}
	want := map[string]string{
		"onest-latin-400.woff2":      "assets/onest-latin-400-normal-*.woff2",
		"onest-latin-ext-400.woff2":  "assets/onest-latin-ext-400-normal-*.woff2",
		"onest-latin-500.woff2":      "assets/onest-latin-500-normal-*.woff2",
		"onest-latin-ext-500.woff2":  "assets/onest-latin-ext-500-normal-*.woff2",
		"onest-latin-600.woff2":      "assets/onest-latin-600-normal-*.woff2",
		"onest-latin-ext-600.woff2":  "assets/onest-latin-ext-600-normal-*.woff2",
		"jbmono-latin-400.woff2":     "assets/jetbrains-mono-latin-400-normal-*.woff2",
		"jbmono-latin-ext-400.woff2": "assets/jetbrains-mono-latin-ext-400-normal-*.woff2",
		"jbmono-latin-500.woff2":     "assets/jetbrains-mono-latin-500-normal-*.woff2",
		"jbmono-latin-ext-500.woff2": "assets/jetbrains-mono-latin-ext-500-normal-*.woff2",
	}
	for alias, pattern := range want {
		matches, err := fs.Glob(dist, pattern)
		if err != nil || len(matches) == 0 {
			continue
		}
		if b, err := fs.ReadFile(dist, matches[0]); err == nil {
			out[alias] = fontAsset{distPath: matches[0], data: b}
		}
	}
	return out
}

func (s *Service) cssBytes() string {
	var b strings.Builder
	type fontSpec struct {
		family string
		weight int
		file   string
	}
	// Same ranges as @fontsource: without them the browser downloads both subsets.
	const latinRange = "U+0000-00FF,U+0131,U+0152-0153,U+02BB-02BC,U+02C6,U+02DA,U+02DC,U+0304,U+0308,U+0329,U+2000-206F,U+20AC,U+2122,U+2191,U+2193,U+2212,U+2215,U+FEFF,U+FFFD"
	const extRange = "U+0100-02BA,U+02BD-02C5,U+02C7-02CC,U+02CE-02D7,U+02DD-02FF,U+0304,U+0308,U+0329,U+1D00-1DBF,U+1E00-1E9F,U+1EF2-1EFF,U+2020,U+20A0-20AB,U+20AD-20C0,U+2113,U+2C60-2C7F,U+A720-A7FF"
	specs := []fontSpec{
		{"Onest", 400, "onest-latin-400.woff2"},
		{"Onest", 400, "onest-latin-ext-400.woff2"},
		{"Onest", 500, "onest-latin-500.woff2"},
		{"Onest", 500, "onest-latin-ext-500.woff2"},
		{"Onest", 600, "onest-latin-600.woff2"},
		{"Onest", 600, "onest-latin-ext-600.woff2"},
		{"JetBrains Mono", 400, "jbmono-latin-400.woff2"},
		{"JetBrains Mono", 400, "jbmono-latin-ext-400.woff2"},
		{"JetBrains Mono", 500, "jbmono-latin-500.woff2"},
		{"JetBrains Mono", 500, "jbmono-latin-ext-500.woff2"},
	}
	for _, sp := range specs {
		if _, ok := s.fonts[sp.file]; ok {
			b.WriteString("@font-face{font-family:'")
			b.WriteString(sp.family)
			b.WriteString("';font-style:normal;font-display:swap;font-weight:")
			b.WriteString(strconv.Itoa(sp.weight))
			b.WriteString(";src:url('/status-assets/")
			b.WriteString(sp.file)
			b.WriteString("') format('woff2');unicode-range:")
			if strings.Contains(sp.file, "-ext-") {
				b.WriteString(extRange)
			} else {
				b.WriteString(latinRange)
			}
			b.WriteString(";}\n")
		}
	}
	b.WriteString(statusBaseCSS)
	return b.String()
}

// ---- Validation helpers ----------------------------------------------------

// ValidSlug reports whether slug matches ^[a-z0-9][a-z0-9-]{0,62}$.
func ValidSlug(slug string) bool {
	if len(slug) == 0 || len(slug) > 63 {
		return false
	}
	for i := 0; i < len(slug); i++ {
		c := slug[i]
		isLower := c >= 'a' && c <= 'z'
		isDigit := c >= '0' && c <= '9'
		if i == 0 && !isLower && !isDigit {
			return false
		}
		if !isLower && !isDigit && c != '-' {
			return false
		}
	}
	return true
}

// ValidateDomain checks that domain is a clean lowercase hostname without scheme, port or path,
// and does not collide with the host of MOORAGE_PUBLIC_URL.
func ValidateDomain(domain, publicURL string) error {
	if domain == "" {
		return nil
	}
	if len(domain) > 253 || strings.ContainsAny(domain, ":/?# \t\r\n\\") || domain != strings.ToLower(domain) {
		return errors.New("Enter a lowercase domain name without https://, port or path (for example status.example.com).")
	}
	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || !strings.Contains(domain, ".") {
		return errors.New("Enter a full domain name such as status.example.com.")
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("Enter a valid domain name such as status.example.com.")
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
				return errors.New("Enter a valid domain name such as status.example.com.")
			}
		}
	}
	if pubHost := hostOfURL(publicURL); pubHost != "" && domain == pubHost {
		return errors.New("The status page domain must be different from MOORAGE_PUBLIC_URL (" + pubHost + "), otherwise the Moorage admin UI would be hidden behind the status page.")
	}
	return nil
}

func hostOfURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if _, rest, ok := strings.Cut(raw, "://"); ok {
		raw = rest
	}
	if host, _, ok := strings.Cut(raw, "/"); ok {
		raw = host
	}
	return CleanHost(raw)
}

// ---- Embedded template, CSS and JS -----------------------------------------

var pageTmpl = template.Must(template.New("status").Funcs(template.FuncMap{
	"rfc3339": func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
}).Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta http-equiv="refresh" content="60">
  <title>{{.Page.Title}}</title>
  <link rel="icon" type="image/svg+xml" href="/status-assets/favicon.svg">
  <link rel="stylesheet" href="/status-assets/status.css">
  <link rel="alternate" type="application/rss+xml" title="{{.Page.Title}} RSS" href="{{.FeedHref}}">
</head>
<body>
  <main class="wrap">
    <header class="card head">
      <div class="title-row">
        <div>
          <h1 class="title">{{.Page.Title}}</h1>
          {{- if .Page.Description}}
          <p class="desc">{{.Page.Description}}</p>
          {{- end}}
        </div>
        <span class="badge {{.Page.BannerStatus}}">
          <span class="dot {{.Page.BannerStatus}}"></span>
          <span>{{.Page.BannerText}}</span>
        </span>
      </div>
    </header>

    {{- range .Page.Maintenance}}
    <section class="card maint" aria-label="Maintenance in progress">
      <div class="inc-top">
        <span class="badge maintenance"><span class="dot maintenance"></span><span>Maintenance</span></span>
        <span class="inc-name">{{.Title}}</span>
      </div>
      {{- if .Message}}
      <p class="desc">{{.Message}}</p>
      {{- end}}
      <p class="meta">Planned end <time datetime="{{rfc3339 .EndsAt}}">{{.EndsText}}</time> · {{range $i, $n := .Items}}{{if $i}}, {{end}}{{$n}}{{end}}</p>
    </section>
    {{- end}}

    {{- range .Page.Sections}}
    <section class="card section">
      {{- if .Name}}
      <h2 class="sec-title">{{.Name}}</h2>
      {{- end}}
      <div class="items">
        {{- range .Items}}
        <div class="item">
          <div class="item-top">
            <div class="item-ident">
              <span class="dot {{.Status}}"></span>
              <span class="item-name">{{.DisplayName}}</span>
            </div>
            <div class="item-meta">
              <span class="meta num" title="Uptime over the last 24 hours">24 h {{.Uptime24hStr}}</span>
              <span class="sep">·</span>
              <span class="up30 num" title="Uptime over the last 30 days">30 d {{.Uptime30dStr}}</span>
              <span class="badge {{.Status}}">{{.StatusLabel}}</span>
            </div>
          </div>
          <div class="strip-row">
            <div class="strip" role="img" aria-label="{{.DisplayName}}: 30-day history">
              {{- range .Days}}
              <span class="tick {{.Status}}" title="{{.Tooltip}}"></span>
              {{- end}}
            </div>
            <div class="strip-labels meta">
              <span>30 days ago</span>
              <span>Today</span>
            </div>
          </div>
        </div>
        {{- end}}
      </div>
    </section>
    {{- end}}

    {{- if .Page.Planned}}
    <section class="card incidents">
      <h2 class="sec-title">Planned maintenance</h2>
      <div class="inc-list">
        {{- range .Page.Planned}}
        <article class="inc">
          <div class="inc-top">
            <span class="dot maintenance"></span>
            <span class="inc-name">{{.Title}}</span>
          </div>
          <p class="meta inc-time">
            <time datetime="{{rfc3339 .StartsAt}}">{{.StartsText}}</time>
            <span> – </span>
            <time datetime="{{rfc3339 .EndsAt}}">{{.EndsText}}</time>
          </p>
          {{- if .Message}}
          <p class="meta plan-msg">{{.Message}}</p>
          {{- end}}
          <p class="meta plan-msg">{{range $i, $n := .Items}}{{if $i}}, {{end}}{{$n}}{{end}}</p>
        </article>
        {{- end}}
      </div>
    </section>
    {{- end}}

    {{- if .Page.ShowIncidents}}
    <section class="card incidents">
      <h2 class="sec-title">Recent incidents (30 days)</h2>
      {{- if not .Page.Incidents}}
      <p class="meta empty">No incidents in the last 30 days.</p>
      {{- else}}
      <div class="inc-list">
        {{- range .Page.Incidents}}
        <article class="inc">
          <div class="inc-top">
            <span class="dot {{if .Ongoing}}down{{else}}operational{{end}}"></span>
            <span class="inc-name">{{.DisplayName}}</span>
            {{- if .Ongoing}}
            <span class="badge down">Ongoing · {{.Duration}}</span>
            {{- else}}
            <span class="meta num">Lasted {{.Duration}}</span>
            {{- end}}
          </div>
          <p class="meta inc-time">
            <time datetime="{{rfc3339 .StartedAt}}">{{.StartedText}}</time>
            {{- if not .Ongoing}}
            <span> – </span>
            <time datetime="{{rfc3339 .EndedAt}}">{{.EndedText}}</time>
            {{- end}}
          </p>
        </article>
        {{- end}}
      </div>
      {{- end}}
    </section>
    {{- end}}

    <footer class="foot meta">
      <span>Updated <time datetime="{{rfc3339 .Page.UpdatedAt}}">{{.Page.UpdatedText}}</time></span>
      <a href="{{.FeedHref}}">RSS feed</a>
    </footer>
  </main>
  <script src="/status-assets/status.js" defer></script>
</body>
</html>
`))

const statusJS = `// Rewrites UTC <time datetime="..."> elements into the visitor's local time zone.
(function () {
  try {
    var fmt = new Intl.DateTimeFormat(undefined, {
      year: 'numeric',
      month: 'short',
      day: 'numeric',
      hour: '2-digit',
      minute: '2-digit'
    });
    var nodes = document.querySelectorAll('time[datetime]');
    for (var i = 0; i < nodes.length; i++) {
      var d = new Date(nodes[i].getAttribute('datetime'));
      if (!isNaN(d.getTime())) {
        nodes[i].textContent = fmt.format(d);
      }
    }
  } catch (_) {}
})();
`

const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32">
  <rect width="32" height="32" rx="7" fill="#FF7A45"/>
  <path d="M16 7v18M11 11h10M9 19a7 7 0 0 0 14 0" fill="none" stroke="#2A1206" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"/>
</svg>`

const statusBaseCSS = `
:root {
  color-scheme: light dark;
  --deep-water: #F6F3EE;
  --pier: #FFFFFF;
  --well: #EFEBE4;
  --foam: #14232A;
  --mist: #3E5258;
  --haze: #6A7C80;
  --murk: #97A4A7;
  --rope: rgba(20, 35, 42, 0.08);
  --rope-strong: rgba(20, 35, 42, 0.12);
  --buoy: #FF7A45;
  --buoy-ink: #2A1206;
  --buoy-text: #C2410C;
  --afloat: #1E8E5A;
  --afloat-wash: rgba(30, 142, 90, 0.12);
  --afloat-text: #17734A;
  --adrift: #D63A40;
  --adrift-wash: rgba(214, 58, 64, 0.12);
  --adrift-text: #B42A30;
  --signal: #B7860B;
  --signal-wash: rgba(183, 134, 11, 0.14);
  --signal-text: #8A6508;
  --becalmed: #7D8F93;
  --becalmed-wash: rgba(106, 124, 128, 0.14);
  --lift: 0 0 0 1px var(--rope), 0 1px 2px -1px rgba(20, 35, 42, 0.06), 0 2px 4px rgba(20, 35, 42, 0.04);
  --font: 'Onest', ui-sans-serif, system-ui, sans-serif;
  --mono: 'JetBrains Mono', ui-monospace, monospace;
}

@media (prefers-color-scheme: dark) {
  :root {
    --deep-water: #0F1A1F;
    --pier: #16242A;
    --well: #0B1418;
    --foam: #E8EEEE;
    --mist: #B7C4C6;
    --haze: #7D8F93;
    --murk: #5E7074;
    --rope: rgba(232, 238, 238, 0.06);
    --rope-strong: rgba(232, 238, 238, 0.10);
    --buoy-text: #FF9A6E;
    --afloat: #4CC38A;
    --afloat-wash: rgba(76, 195, 138, 0.14);
    --afloat-text: #7FD8AB;
    --adrift: #F2555A;
    --adrift-wash: rgba(242, 85, 90, 0.15);
    --adrift-text: #FF8A8E;
    --signal: #F5C451;
    --signal-wash: rgba(245, 196, 81, 0.14);
    --signal-text: #F7D27A;
    --becalmed: #7D8F93;
    --becalmed-wash: rgba(125, 143, 147, 0.16);
    --lift: 0 0 0 1px var(--rope);
  }
}

*, *::before, *::after {
  box-sizing: border-box;
}

html, body {
  margin: 0;
  padding: 0;
  background: var(--deep-water);
  color: var(--foam);
  font: 400 14px/1.5 var(--font);
  -webkit-font-smoothing: antialiased;
}

.wrap {
  max-width: 780px;
  margin: 0 auto;
  padding: 28px 16px 44px;
  display: grid;
  gap: 12px;
}

.card {
  background: var(--pier);
  border-radius: 12px;
  box-shadow: var(--lift);
}

.head {
  padding: 20px 22px;
}

.title-row {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
}

.title {
  margin: 0;
  font-size: 22px;
  font-weight: 600;
  letter-spacing: -0.015em;
  text-wrap: balance;
}

.desc {
  margin: 6px 0 0;
  color: var(--mist);
  font-size: 14px;
  max-width: 65ch;
  text-wrap: pretty;
}

.section, .incidents, .maint {
  padding: 16px 20px;
}

.maint {
  box-shadow: var(--lift), inset 3px 0 0 var(--signal);
}

.maint .desc, .maint .meta {
  margin: 6px 0 0;
}

.plan-msg {
  margin: 4px 0 0 16px;
}

.sec-title {
  margin: 0 0 12px;
  font-size: 13px;
  font-weight: 600;
  color: var(--foam);
}

.items {
  display: grid;
  gap: 14px;
}

.item + .item {
  padding-top: 14px;
  border-top: 1px solid var(--rope);
}

.item-top {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}

.item-ident {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
}

.item-name {
  font-weight: 500;
  font-size: 14px;
  color: var(--foam);
  word-break: break-word;
}

.item-meta {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.up30 {
  font: 500 13px var(--mono);
  color: var(--foam);
}

.sep {
  color: var(--murk);
  font-size: 12px;
}

.meta {
  font-size: 12px;
  color: var(--haze);
}

.num {
  font-family: var(--mono);
  font-variant-numeric: tabular-nums;
}

.dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  flex: none;
  background: var(--becalmed);
}
.dot.operational { background: var(--afloat); }
.dot.down { background: var(--adrift); }
.dot.maintenance { background: var(--signal); }
.dot.degraded { background: var(--signal); box-shadow: inset 0 0 0 2px var(--adrift); }
.dot.paused, .dot.nodata { background: var(--becalmed); }

.badge {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 22px;
  padding: 0 9px;
  border-radius: 999px;
  font-size: 12px;
  font-weight: 500;
  background: var(--becalmed-wash);
  color: var(--mist);
  white-space: nowrap;
}
.badge .dot {
  width: 6px;
  height: 6px;
}
.badge.operational {
  background: var(--afloat-wash);
  color: var(--afloat-text);
}
.badge.down {
  background: var(--adrift-wash);
  color: var(--adrift-text);
}
.badge.maintenance {
  background: var(--signal-wash);
  color: var(--signal-text);
}
.badge.degraded {
  background: var(--signal-wash);
  color: var(--signal-text);
  font-weight: 600;
}

.strip-row {
  margin-top: 10px;
}

.strip {
  display: grid;
  grid-template-columns: repeat(30, minmax(0, 1fr));
  align-items: end;
  gap: 3px;
  height: 22px;
}

.tick {
  display: block;
  height: 18px;
  border-radius: 3px;
  background: var(--becalmed-wash);
}
.tick.operational {
  background: var(--afloat);
  height: 18px;
}
.tick.down {
  background: var(--adrift);
  height: 22px;
}
.tick.maintenance {
  background: var(--signal);
  height: 12px;
}
.tick.degraded {
  background: var(--signal);
  height: 22px;
}
.tick.paused {
  background: var(--becalmed);
  height: 12px;
}
.tick.nodata {
  background: var(--rope-strong);
  height: 10px;
}

.strip-labels {
  display: flex;
  justify-content: space-between;
  margin-top: 4px;
  font-size: 11px;
}

.inc-list {
  display: grid;
  gap: 10px;
}

.inc + .inc {
  padding-top: 10px;
  border-top: 1px solid var(--rope);
}

.inc-top {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.inc-name {
  font-weight: 500;
  color: var(--foam);
}

.inc-time {
  margin: 4px 0 0 16px;
  font-family: var(--mono);
}

.empty {
  margin: 0;
}

.foot {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
  padding: 4px 6px;
}

a {
  color: var(--buoy-text);
  text-decoration: none;
}
a:hover {
  text-decoration: underline;
}

@media (max-width: 520px) {
  .wrap {
    padding: 16px 10px 32px;
  }
  .head, .section, .incidents, .maint {
    padding: 14px 14px;
  }
  .strip {
    gap: 2px;
  }
}
`
