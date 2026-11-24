// Package updater schedules image update checks, applies auto-updates inside the
// configured time window, sends batched update notifications, and handles rollback.
package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/notify"
	"moorage/internal/store"
)

const (
	ActorAuto       = "auto-update"
	CheckCooldown   = time.Minute
	maxLogTailLines = 20
)

// ErrRateLimited is returned when "Check now" is called more than once per minute for the same stack.
var ErrRateLimited = errors.New("Images were checked less than a minute ago. Wait a moment before checking again.")

// StackEventRecorder is implemented by monitor.Monitor.
type StackEventRecorder interface {
	StackEvent(hostID, stack, kind, actor, detail string)
}

// ApplyInput is the pure decision input for ShouldApply.
type ApplyInput struct {
	Policy        string
	Status        string
	WindowStart   string
	WindowEnd     string
	Now           time.Time
	Busy          bool
	Stopped       bool
	ReadOnly      bool
	Managed       bool
	RemoteDigest  string
	SkippedDigest string
	LastAutoAt    time.Time
}

// ShouldApply is a pure function deciding whether a service with an available digest update
// should be auto-updated right now.
func ShouldApply(in ApplyInput) bool {
	if in.Policy != store.PolicyAuto {
		return false
	}
	if in.ReadOnly || !in.Managed || in.Busy || in.Stopped {
		return false
	}
	if in.Status != engine.ImageUpdateAvailable && in.Status != engine.ImagePulledNotApplied {
		return false
	}
	if in.RemoteDigest != "" && in.RemoteDigest == in.SkippedDigest {
		return false
	}
	inWin, winStart := InWindow(in.Now, in.WindowStart, in.WindowEnd)
	if !inWin {
		return false
	}
	if !in.LastAutoAt.IsZero() && !in.LastAutoAt.Before(winStart) {
		return false
	}
	return true
}

// ParseHHMM parses "HH:MM" into hours (0..23) and minutes (0..59).
func ParseHHMM(s string) (hour, min int, ok bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(s[:2])
	m, err2 := strconv.Atoi(s[3:])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// InWindow reports whether now (in its Location) falls inside [startHHMM, endHHMM),
// and returns the start timestamp of the active window instance.
// Supports overnight windows that cross midnight (e.g. "23:00" to "02:00") and DST transitions.
func InWindow(now time.Time, startHHMM, endHHMM string) (bool, time.Time) {
	sh, sm, ok1 := ParseHHMM(startHHMM)
	eh, em, ok2 := ParseHHMM(endHHMM)
	if !ok1 || !ok2 {
		return false, time.Time{}
	}
	startMins := sh*60 + sm
	endMins := eh*60 + em
	if startMins == endMins {
		return false, time.Time{}
	}

	loc := now.Location()
	y, mo, d := now.Date()
	curMins := now.Hour()*60 + now.Minute()

	if startMins < endMins {
		// Same-day window, e.g. 03:00 to 05:00.
		if curMins >= startMins && curMins < endMins {
			ws := time.Date(y, mo, d, sh, sm, 0, 0, loc)
			return true, ws
		}
		return false, time.Time{}
	}

	// Overnight window wrapping past midnight, e.g. 23:00 to 02:00.
	if curMins >= startMins {
		ws := time.Date(y, mo, d, sh, sm, 0, 0, loc)
		return true, ws
	}
	if curMins < endMins {
		prev := now.AddDate(0, 0, -1)
		py, pmo, pd := prev.Date()
		ws := time.Date(py, pmo, pd, sh, sm, 0, 0, loc)
		return true, ws
	}
	return false, time.Time{}
}

// NextWindowStart returns the next time the apply window opens after now.
// If now is already inside the window, it returns the next window opening (tomorrow or today).
func NextWindowStart(now time.Time, startHHMM, endHHMM string) (time.Time, bool) {
	sh, sm, ok1 := ParseHHMM(startHHMM)
	_, _, ok2 := ParseHHMM(endHHMM)
	if !ok1 || !ok2 || startHHMM == endHHMM {
		return time.Time{}, false
	}
	loc := now.Location()
	y, mo, d := now.Date()
	todayStart := time.Date(y, mo, d, sh, sm, 0, 0, loc)
	if todayStart.After(now) {
		return todayStart, true
	}
	nextDay := now.AddDate(0, 0, 1)
	ny, nmo, nd := nextDay.Date()
	return time.Date(ny, nmo, nd, sh, sm, 0, 0, loc), true
}

// Updater coordinates periodic image checks, apply-window updates, notifications, and rollbacks.
type Updater struct {
	st        *store.Store
	reg       *host.Registry
	events    StackEventRecorder
	log       *slog.Logger
	publicURL string

	mu         sync.Mutex
	rollbackMu sync.Mutex // serializes auto-rollback decisions (several checks can fail at once)
	// Push receives auto-update and rollback notices for Web Push.
	Push        func(ctx context.Context, event string, msg notify.Message)
	ctx         context.Context
	jobs        sync.WaitGroup // follow-ups of updates and rollbacks running in the background
	wake        chan struct{}
	lastManual  map[string]time.Time // "hostID/stack" or "*" -> last manual check time
	unsupported map[string]time.Time // hostID -> when 404 was seen
	nowFn       func() time.Time
	sendFn      func(ctx context.Context, ch store.Channel, m notify.Message) error
	// Strategy returns the deploy strategy of a stack: "rolling" runs updates as rolling
	// deploys with an automatic rollback. Nil means standard.
	Strategy func(ctx context.Context, hostID, stack string) string
}

// New creates an Updater.
func New(st *store.Store, reg *host.Registry, events StackEventRecorder, log *slog.Logger, publicURL string) *Updater {
	u := &Updater{
		st:          st,
		reg:         reg,
		events:      events,
		log:         log,
		publicURL:   strings.TrimRight(publicURL, "/"),
		wake:        make(chan struct{}, 1),
		lastManual:  map[string]time.Time{},
		unsupported: map[string]time.Time{},
		nowFn:       time.Now,
	}
	u.sendFn = func(ctx context.Context, ch store.Channel, msg notify.Message) error {
		var cfg notify.Config
		if err := json.Unmarshal([]byte(ch.Config), &cfg); err != nil {
			return err
		}
		return notify.Send(ctx, ch.Kind, cfg, msg)
	}
	return u
}

// SetClock overrides the clock in tests.
func (u *Updater) SetClock(fn func() time.Time) {
	u.mu.Lock()
	u.nowFn = fn
	u.mu.Unlock()
}

// SetSender overrides the notification sender in tests.
func (u *Updater) SetSender(fn func(ctx context.Context, ch store.Channel, m notify.Message) error) {
	u.mu.Lock()
	u.sendFn = fn
	u.mu.Unlock()
}

func (u *Updater) now() time.Time {
	u.mu.Lock()
	fn := u.nowFn
	u.mu.Unlock()
	return fn()
}

// bg is the context for work that outlives the HTTP request that started it.
func (u *Updater) bg() context.Context {
	if u.ctx != nil {
		return u.ctx
	}
	return context.Background()
}

// Wait blocks until background follow-ups (update records, rollback results, re-checks) are done.
func (u *Updater) Wait() { u.jobs.Wait() }

// Start launches the background scheduler loop.
func (u *Updater) Start(ctx context.Context) {
	u.ctx = ctx
	go u.loop(ctx)
}

// Reload wakes the scheduler after settings or policies change.
func (u *Updater) Reload() {
	select {
	case u.wake <- struct{}{}:
	default:
	}
}

func (u *Updater) loop(ctx context.Context) {
	// Initial delay after startup so hub boot stays quiet.
	initTimer := time.NewTimer(25 * time.Second)
	select {
	case <-ctx.Done():
		initTimer.Stop()
		return
	case <-initTimer.C:
	}

	var lastCheckRun time.Time
	var nextGap time.Duration // interval ±10 %, so many hubs don't hit Docker Hub in step
	for {
		if ctx.Err() != nil {
			return
		}
		st, err := u.st.UpdateSettings(ctx)
		if err != nil {
			u.log.Warn("load update settings", "err", err)
			st = store.UpdateSettings{Enabled: true, Interval: 21600, WindowStart: "03:00", WindowEnd: "05:00"}
		}

		now := u.now()
		interval := time.Duration(max(st.Interval, 3600)) * time.Second
		if st.Enabled {
			inWin, winStart := InWindow(now, st.WindowStart, st.WindowEnd)
			if lastCheckRun.IsZero() || now.Sub(lastCheckRun) >= nextGap || (inWin && lastCheckRun.Before(winStart)) {
				lastCheckRun = now
				nextGap = jittered(interval)
				u.RunScheduled(ctx)
				now = u.now()
			}
		}

		wait := interval
		if st.Enabled {
			if !lastCheckRun.IsZero() {
				rem := nextGap - now.Sub(lastCheckRun)
				if rem < time.Minute {
					rem = time.Minute
				}
				wait = rem
			}
			if nextWin, ok := NextWindowStart(now, st.WindowStart, st.WindowEnd); ok {
				if d := nextWin.Sub(now); d > 0 && d < wait {
					wait = d
				}
			}
		} else {
			wait = time.Hour
		}

		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-u.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

func jittered(d time.Duration) time.Duration {
	return d + time.Duration((rand.Float64()*0.2-0.1)*float64(d))
}

// pendingUpdate reports whether a check row has a same-tag update waiting.
func pendingUpdate(r store.ImageCheckRow) bool {
	return r.Status == engine.ImageUpdateAvailable || r.Status == engine.ImagePulledNotApplied
}

type pendingNotice struct {
	hostID     string
	hostName   string
	stack      string
	service    string
	ref        string
	newDigest  string
	newerTag   string
	newerMajor string
}

// RunScheduled executes one scheduled check run across all connected hosts, sends one batched
// notice for newly discovered updates, and applies auto-updates if now is inside the apply window.
func (u *Updater) RunScheduled(ctx context.Context) {
	st, err := u.st.UpdateSettings(ctx)
	if err != nil || !st.Enabled {
		return
	}
	if u.reg == nil {
		return
	}
	hosts, err := u.reg.List(ctx)
	if err != nil {
		return
	}
	policies, _ := u.st.AllStackUpdatePolicies(ctx)
	now := u.now()

	var notices []pendingNotice

	for _, he := range hosts {
		if !he.Connected {
			continue
		}
		h, _, err := u.reg.Get(ctx, he.ID)
		if err != nil {
			continue
		}
		stacks, err := h.ListStacks(ctx)
		if err != nil {
			continue
		}
		for _, sm := range stacks {
			pol, ok := policies[he.ID+"/"+sm.Name]
			if !ok {
				pol = store.StackUpdatePolicy{
					HostID:         he.ID,
					Stack:          sm.Name,
					Policy:         store.PolicyNotify,
					SkippedDigests: map[string]string{},
				}
			}
			if pol.Policy == store.PolicyOff {
				continue
			}

			rows, err := u.checkOneStack(ctx, h, he, sm, now)
			if err != nil {
				continue
			}

			// Decide whether any service in this stack should be auto-applied right now.
			// An update pulls every service of the stack, so a digest the user rolled back
			// (or that failed to start) blocks the whole stack, not only its own service.
			blocked := false
			for _, r := range rows {
				if pendingUpdate(r) && r.RemoteDigest != "" && r.RemoteDigest == pol.SkippedDigests[r.Service] {
					blocked = true
				}
			}
			wantAuto := false
			for _, r := range rows {
				if blocked {
					break
				}
				if ShouldApply(ApplyInput{
					Policy:        pol.Policy,
					Status:        r.Status,
					WindowStart:   st.WindowStart,
					WindowEnd:     st.WindowEnd,
					Now:           now,
					Busy:          sm.Busy,
					Stopped:       sm.Running == 0,
					ReadOnly:      he.ReadOnly,
					Managed:       sm.Managed,
					RemoteDigest:  r.RemoteDigest,
					SkippedDigest: pol.SkippedDigests[r.Service],
					LastAutoAt:    pol.LastAutoAt,
				}) {
					wantAuto = true
					break
				}
			}

			if wantAuto {
				u.applyAutoUpdate(ctx, h, he, sm.Name, st, pol, rows, now)
				// Still check if any service has a newly discovered version tag (Part B is notify-only even when policy is auto!).
				rowsAfter, _ := u.st.ImageChecks(ctx, he.ID, sm.Name)
				changed := false
				for i := range rowsAfter {
					r := &rowsAfter[i]
					nTag, nMaj := "", ""
					if r.NewerTag != "" && r.NewerTag != r.AnnouncedTag {
						nTag = r.NewerTag
						r.AnnouncedTag = r.NewerTag
						changed = true
					}
					if r.NewerMajor != "" && r.NewerMajor != r.AnnouncedMajor {
						nMaj = r.NewerMajor
						r.AnnouncedMajor = r.NewerMajor
						changed = true
					}
					if nTag != "" || nMaj != "" {
						notices = append(notices, pendingNotice{
							hostID: he.ID, hostName: he.Name, stack: sm.Name,
							service: r.Service, ref: r.Ref, newerTag: nTag, newerMajor: nMaj,
						})
					}
				}
				if changed {
					_ = u.st.SaveStackImageChecks(ctx, he.ID, sm.Name, rowsAfter)
				}
				continue
			}

			// Not auto-applying now: collect any unannounced digest or tag updates.
			changed := false
			for i := range rows {
				r := &rows[i]
				nDig, nTag, nMaj := "", "", ""
				if (r.Status == engine.ImageUpdateAvailable || r.Status == engine.ImagePulledNotApplied) &&
					r.RemoteDigest != "" && r.RemoteDigest != r.AnnouncedDigest && r.RemoteDigest != pol.SkippedDigests[r.Service] {
					nDig = r.RemoteDigest
					r.AnnouncedDigest = r.RemoteDigest
					changed = true
				}
				if r.NewerTag != "" && r.NewerTag != r.AnnouncedTag {
					nTag = r.NewerTag
					r.AnnouncedTag = r.NewerTag
					changed = true
				}
				if r.NewerMajor != "" && r.NewerMajor != r.AnnouncedMajor {
					nMaj = r.NewerMajor
					r.AnnouncedMajor = r.NewerMajor
					changed = true
				}
				if nDig != "" || nTag != "" || nMaj != "" {
					notices = append(notices, pendingNotice{
						hostID: he.ID, hostName: he.Name, stack: sm.Name,
						service: r.Service, ref: r.Ref,
						newDigest: nDig, newerTag: nTag, newerMajor: nMaj,
					})
				}
			}
			if changed {
				_ = u.st.SaveStackImageChecks(ctx, he.ID, sm.Name, rows)
			}
		}
	}

	if len(notices) > 0 && len(st.Channels) > 0 {
		u.sendDiscoveryBatch(ctx, st.Channels, notices)
	}
}

// CheckNow triggers an immediate check for one stack (or all stacks when hostID == "" and stack == "").
// Per M8 rules, "Check now" NEVER triggers an auto-update and is rate-limited to once per minute per stack.
func (u *Updater) CheckNow(ctx context.Context, hostID, stack string) ([]store.ImageCheckRow, error) {
	now := u.now()
	rateKey := "*"
	if hostID != "" && stack != "" {
		rateKey = hostID + "/" + stack
	}
	u.mu.Lock()
	if last, ok := u.lastManual[rateKey]; ok && now.Sub(last) < CheckCooldown {
		u.mu.Unlock()
		return nil, ErrRateLimited
	}
	u.lastManual[rateKey] = now
	u.mu.Unlock()

	if u.reg == nil {
		return nil, host.ErrUnknownHost
	}

	if hostID != "" && stack != "" {
		h, he, err := u.reg.Get(ctx, hostID)
		if err != nil {
			return nil, err
		}
		if loc, ok := h.(host.Local); ok && loc.E != nil {
			loc.E.ResetImageCache()
		}
		st, err := h.GetStack(ctx, stack)
		if err != nil {
			return nil, err
		}
		return u.checkOneStack(ctx, h, he, st.StackSummary, now)
	}

	hosts, err := u.reg.List(ctx)
	if err != nil {
		return nil, err
	}
	var all []store.ImageCheckRow
	for _, he := range hosts {
		if !he.Connected {
			continue
		}
		h, _, err := u.reg.Get(ctx, he.ID)
		if err != nil {
			continue
		}
		if loc, ok := h.(host.Local); ok && loc.E != nil {
			loc.E.ResetImageCache()
		}
		stacks, err := h.ListStacks(ctx)
		if err != nil {
			continue
		}
		for _, sm := range stacks {
			rows, err := u.checkOneStack(ctx, h, he, sm, now)
			if err == nil {
				all = append(all, rows...)
			}
		}
	}
	return all, nil
}

func (u *Updater) checkOneStack(ctx context.Context, h host.Host, he host.Entry, sm engine.StackSummary, now time.Time) ([]store.ImageCheckRow, error) {
	existing, _ := u.st.ImageChecks(ctx, he.ID, sm.Name)
	byService := map[string]store.ImageCheckRow{}
	for _, r := range existing {
		byService[r.Service] = r
	}

	imgs, err := h.CheckImages(ctx, sm.Name)
	if err != nil {
		if host.IsUnsupported(err) {
			u.mu.Lock()
			u.unsupported[he.ID] = now
			u.mu.Unlock()
			rows := u.markUnsupported(ctx, h, he.ID, sm.Name, byService, now)
			_ = u.st.SaveStackImageChecks(ctx, he.ID, sm.Name, rows)
			return rows, nil
		}
		return nil, err
	}

	u.mu.Lock()
	delete(u.unsupported, he.ID)
	u.mu.Unlock()

	rows := make([]store.ImageCheckRow, 0, len(imgs))
	for _, im := range imgs {
		prev := byService[im.Service]
		row := store.ImageCheckRow{
			HostID:              he.ID,
			Stack:               sm.Name,
			Service:             im.Service,
			Ref:                 im.Ref,
			LocalID:             im.LocalID,
			LocalDigest:         im.LocalDigest,
			RunningID:           im.RunningID,
			RemoteDigest:        im.RemoteDigest,
			Status:              im.Status,
			Error:               im.Error,
			NewerTag:            im.NewerTag,
			NewerMajor:          im.NewerMajor,
			CheckedAt:           now,
			AnnouncedDigest:     prev.AnnouncedDigest,
			AnnouncedTag:        prev.AnnouncedTag,
			AnnouncedMajor:      prev.AnnouncedMajor,
			AnnouncedFailDigest: prev.AnnouncedFailDigest,
		}
		if im.Status == engine.ImageUpToDate && im.RemoteDigest != "" {
			row.AnnouncedDigest = im.RemoteDigest
		}
		rows = append(rows, row)
	}
	if err := u.st.SaveStackImageChecks(ctx, he.ID, sm.Name, rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func (u *Updater) markUnsupported(ctx context.Context, h host.Host, hostID, stack string, prev map[string]store.ImageCheckRow, now time.Time) []store.ImageCheckRow {
	var rows []store.ImageCheckRow
	if st, err := h.GetStack(ctx, stack); err == nil {
		seen := map[string]bool{}
		for _, c := range st.Containers {
			svc := c.Service
			if svc == "" {
				svc = c.Name
			}
			if seen[svc] {
				continue
			}
			seen[svc] = true
			p := prev[svc]
			rows = append(rows, store.ImageCheckRow{
				HostID:              hostID,
				Stack:               stack,
				Service:             svc,
				Ref:                 c.Image,
				Status:              engine.ImageUnsupported,
				Error:               "Update the agent to check images",
				CheckedAt:           now,
				AnnouncedDigest:     p.AnnouncedDigest,
				AnnouncedTag:        p.AnnouncedTag,
				AnnouncedMajor:      p.AnnouncedMajor,
				AnnouncedFailDigest: p.AnnouncedFailDigest,
			})
		}
	}
	if len(rows) == 0 {
		p := prev["stack"]
		rows = append(rows, store.ImageCheckRow{
			HostID:              hostID,
			Stack:               stack,
			Service:             "stack",
			Status:              engine.ImageUnsupported,
			Error:               "Update the agent to check images",
			CheckedAt:           now,
			AnnouncedDigest:     p.AnnouncedDigest,
			AnnouncedTag:        p.AnnouncedTag,
			AnnouncedMajor:      p.AnnouncedMajor,
			AnnouncedFailDigest: p.AnnouncedFailDigest,
		})
	}
	return rows
}

func (u *Updater) applyAutoUpdate(
	ctx context.Context,
	h host.Host,
	he host.Entry,
	stack string,
	st store.UpdateSettings,
	pol store.StackUpdatePolicy,
	rows []store.ImageCheckRow,
	now time.Time,
) {
	rec, err := u.RunUpdate(ctx, he.ID, stack, ActorAuto)
	if errors.Is(err, engine.ErrSelfStack) {
		// Moorage's own stack is never recreated from inside: it behaves like Notify, and
		// Moorage itself is updated through Moorage updates.
		u.log.Info("auto-update skipped on Moorage's own stack; notify only", "host", he.ID, "stack", stack)
		return
	}
	if err != nil {
		u.log.Warn("auto-update failed to start", "host", he.ID, "stack", stack, "err", err)
		return
	}
	u.auditAutoUpdate(ctx, he.ID, stack, rec)

	// Mark that auto-update ran in this window so it doesn't repeat in the same window.
	pol.LastAutoAt = now
	if pol.SkippedDigests == nil {
		pol.SkippedDigests = map[string]string{}
	}

	switch rec.Result {
	case store.UpdateOK:
		if len(st.Channels) > 0 && len(changedServices(rec)) > 0 {
			u.sendAutoAppliedNotice(ctx, st.Channels, stack, rec)
		}
	case store.UpdatePullFailed:
		// Nothing changed; the next window tries again, but the notice goes out once per digest.
		// The rows were re-checked after the update, so mark those, not the stale copy.
		fresh, _ := u.st.ImageChecks(ctx, he.ID, stack)
		shouldNotify := false
		for i := range fresh {
			if pendingUpdate(fresh[i]) && fresh[i].RemoteDigest != "" && fresh[i].RemoteDigest != fresh[i].AnnouncedFailDigest {
				fresh[i].AnnouncedFailDigest = fresh[i].RemoteDigest
				shouldNotify = true
			}
		}
		_ = u.st.SaveStackImageChecks(ctx, he.ID, stack, fresh)
		if shouldNotify && len(st.Channels) > 0 {
			u.sendFailureNotice(ctx, st.Channels, stack, rec)
		}
	case store.UpdateUpFailed:
		// The new image doesn't start. Retrying it every window would restart the stack into
		// the same failure each night, so it waits for the user (Update now or a newer digest).
		for _, r := range rows {
			if pendingUpdate(r) && r.RemoteDigest != "" {
				pol.SkippedDigests[r.Service] = r.RemoteDigest
			}
		}
		if len(st.Channels) > 0 {
			u.sendFailureNotice(ctx, st.Channels, stack, rec)
		}
	}
	_ = u.st.SaveStackUpdatePolicy(ctx, pol)
}

// AuditAutoUpdate is the audit action of an update the scheduler applied on its own.
const AuditAutoUpdate = "image.auto_update"

// auditAutoUpdate records an applied auto-update with actor "system". It runs only once the
// update really started (RunUpdate returned a record), so a busy stack leaves no row.
func (u *Updater) auditAutoUpdate(ctx context.Context, hostID, stack string, rec store.ImageUpdate) {
	outcome := store.AuditOK
	if rec.Result != store.UpdateOK {
		outcome = store.AuditFailed
	}
	target := ""
	if rec.ID > 0 {
		target = fmt.Sprintf("update #%d", rec.ID)
	}
	err := u.st.Audit(context.WithoutCancel(ctx), store.AuditEntry{
		Actor: store.AuditActorSystem, Action: AuditAutoUpdate, HostID: hostID, Stack: stack, Target: target, Outcome: outcome,
	})
	if err != nil {
		u.log.Warn("audit auto-update", "err", err)
	}
}

// RunUpdate runs an image update (pull + up) and waits for its record. The scheduler uses it.
func (u *Updater) RunUpdate(ctx context.Context, hostID, stack, actor string) (store.ImageUpdate, error) {
	_, done, err := u.StartUpdate(ctx, hostID, stack, actor)
	if err != nil {
		return store.ImageUpdate{}, err
	}
	select {
	case rec := <-done:
		return rec, nil
	case <-ctx.Done():
		return store.ImageUpdate{}, ctx.Err()
	}
}

// StartUpdate starts an image update (pull + up) and returns its operation right away, so the
// UI shows the output like for any other action. In the background it follows the operation,
// stores the update record with before/after image IDs and digests, re-checks the stack and
// then sends the record on the channel. A pull failure of an auto-update is not stored:
// nothing changed.
func (u *Updater) StartUpdate(ctx context.Context, hostID, stack, actor string) (engine.OpInfo, <-chan store.ImageUpdate, error) {
	if u.reg == nil {
		return engine.OpInfo{}, nil, host.ErrUnknownHost
	}
	h, he, err := u.reg.Get(ctx, hostID)
	if err != nil {
		return engine.OpInfo{}, nil, err
	}
	if he.ReadOnly {
		return engine.OpInfo{}, nil, host.ErrHostReadOnly
	}

	before, err := h.StackImages(ctx, stack)
	if err != nil {
		return engine.OpInfo{}, nil, err
	}
	checks, _ := u.st.ImageChecks(ctx, hostID, stack)
	remoteByService := map[string]string{}
	for _, c := range checks {
		if c.RemoteDigest != "" {
			remoteByService[c.Service] = c.RemoteDigest
		}
	}

	started := u.now()
	action := "update"
	if u.Strategy != nil && u.Strategy(ctx, hostID, stack) == store.StrategyRolling {
		action = "rolling-update"
	}
	op, err := h.RunAction(ctx, stack, action, actor)
	if err != nil {
		return engine.OpInfo{}, nil, err
	}
	// Only after the action really started: a busy stack must not get an anchor and 3 min of silence.
	if u.events != nil {
		u.events.StackEvent(hostID, stack, "update", actor, buildUpdateDetail(before, remoteByService))
	}

	done := make(chan store.ImageUpdate, 1)
	u.jobs.Add(1)
	go func() {
		defer u.jobs.Done()
		done <- u.finishUpdate(u.bg(), h, he, stack, actor, op, before, remoteByService, started)
	}()
	return op, done, nil
}

func (u *Updater) finishUpdate(
	ctx context.Context,
	h host.Host,
	he host.Entry,
	stack, actor string,
	op engine.OpInfo,
	before map[string]engine.RunningImage,
	remoteByService map[string]string,
	started time.Time,
) store.ImageUpdate {
	var tail []string
	var opErr string
	sawRecreated := false
	_ = h.FollowOp(ctx, op.ID, func(ev host.OpEvent) error {
		for _, line := range ev.Lines {
			for _, sub := range strings.Split(strings.TrimRight(line, "\r\n"), "\n") {
				sub = strings.TrimSpace(sub)
				if sub == "" {
					continue
				}
				if strings.HasPrefix(sub, "Recreated ") || strings.HasPrefix(sub, "Container ") || strings.HasPrefix(sub, "Creating ") || strings.HasPrefix(sub, "Starting ") {
					sawRecreated = true
				}
				tail = appendTail(tail, sub)
			}
		}
		if ev.Done && ev.Error != "" {
			opErr = ev.Error
		}
		return nil
	})

	after, _ := h.StackImages(ctx, stack)
	svcs := buildUpdateServices(before, after, remoteByService)

	result := store.UpdateOK
	logTail := ""
	if opErr != "" {
		if len(tail) == 0 || tail[len(tail)-1] != opErr {
			tail = appendTail(tail, opErr)
		}
		logTail = strings.Join(tail, "\n")
		switch {
		case strings.Contains(opErr, engine.ErrRolledBack.Error()):
			// a rolling deploy already put the previous containers back
			result = store.UpdateRolledBack
		case !sawRecreated && imagesUnchanged(before, after):
			result = store.UpdatePullFailed
		default:
			result = store.UpdateUpFailed
		}
	}

	rec := store.ImageUpdate{
		HostID:     he.ID,
		Stack:      stack,
		StartedAt:  started,
		FinishedAt: u.now(),
		Actor:      actor,
		Result:     result,
		Services:   svcs,
		LogTail:    logTail,
	}
	if !(actor == ActorAuto && result == store.UpdatePullFailed) {
		if id, err := u.st.AddImageUpdate(ctx, rec); err == nil {
			rec.ID = id
		}
	}
	if result == store.UpdateRolledBack {
		// A rolling update put the previous containers back: like a rollback, the stack
		// switches to notify and skips the new digests, so the next window doesn't retry.
		pol, _ := u.st.StackUpdatePolicy(ctx, he.ID, stack)
		pol.HostID, pol.Stack = he.ID, stack
		if actor == ActorAuto {
			pol.Policy = store.PolicyNotify
		}
		if pol.SkippedDigests == nil {
			pol.SkippedDigests = map[string]string{}
		}
		for _, svc := range rec.Services {
			if svc.NewDigest != "" && svc.NewDigest != svc.OldDigest {
				pol.SkippedDigests[svc.Service] = svc.NewDigest
			}
		}
		_ = u.st.SaveStackUpdatePolicy(ctx, pol)
		if u.events != nil {
			u.events.StackEvent(he.ID, stack, "rollback", "moorage", "Auto-rollback after a failed rolling update: "+opErr)
		}
	}
	// Refresh the stored check, so the UI doesn't keep offering the update that just ran.
	_, _ = u.checkOneStack(ctx, h, he, engine.StackSummary{Name: stack}, u.now())
	return rec
}

func appendTail(tail []string, line string) []string {
	// Compose and the op both print the final error; keep it once.
	if len(tail) > 0 && strings.HasSuffix(tail[len(tail)-1], line) {
		return tail
	}
	if len(tail) >= maxLogTailLines {
		copy(tail, tail[1:])
		tail[len(tail)-1] = line
		return tail
	}
	return append(tail, line)
}

// RecheckAfter re-checks a stack once an operation (e.g. the update after a tag change) ends.
func (u *Updater) RecheckAfter(hostID, stack, opID string) {
	u.jobs.Add(1)
	go func() {
		defer u.jobs.Done()
		ctx := u.bg()
		h, he, err := u.reg.Get(ctx, hostID)
		if err != nil {
			return
		}
		_ = h.FollowOp(ctx, opID, func(host.OpEvent) error { return nil })
		_, _ = u.checkOneStack(ctx, h, he, engine.StackSummary{Name: stack}, u.now())
	}()
}

// Rollback reverts an image update record back to its previous image IDs (pulling by digest if pruned),
// runs compose up without pull, switches the stack policy to notify, and records the rolled-back digest
// as skipped_digest so the next window does not re-apply it.
func (u *Updater) Rollback(ctx context.Context, hostID, stack string, updateID int64, actor string) (engine.OpInfo, error) {
	if u.reg == nil {
		return engine.OpInfo{}, host.ErrUnknownHost
	}
	h, he, err := u.reg.Get(ctx, hostID)
	if err != nil {
		return engine.OpInfo{}, err
	}
	if he.ReadOnly {
		return engine.OpInfo{}, host.ErrHostReadOnly
	}

	var rec store.ImageUpdate
	if updateID > 0 {
		rec, err = u.st.ImageUpdateByID(ctx, updateID)
		if err != nil || rec.HostID != hostID || rec.Stack != stack {
			return engine.OpInfo{}, store.ErrNotFound
		}
	} else {
		list, err := u.st.ImageUpdates(ctx, hostID, stack, 10)
		if err != nil || len(list) == 0 {
			return engine.OpInfo{}, store.ErrNotFound
		}
		found := false
		for _, item := range list {
			if item.Result == store.UpdateOK || item.Result == store.UpdateUpFailed {
				rec = item
				found = true
				break
			}
		}
		if !found {
			rec = list[0]
		}
	}

	targets := map[string]engine.RollbackTarget{}
	for _, svc := range rec.Services {
		if svc.OldID == "" && svc.OldDigest == "" {
			continue
		}
		targets[svc.Service] = engine.RollbackTarget{
			Ref:       svc.Ref,
			OldID:     svc.OldID,
			OldDigest: svc.OldDigest,
		}
	}
	if len(targets) == 0 {
		return engine.OpInfo{}, errors.New("this update record has no previous image to roll back to")
	}

	op, err := h.RollbackImages(ctx, stack, targets, actor)
	if err != nil {
		return engine.OpInfo{}, err
	}

	// Record StackEvent immediately for deploy silence and strip anchor.
	if u.events != nil {
		u.events.StackEvent(hostID, stack, "rollback", actor, fmt.Sprintf("update #%d", rec.ID))
	}

	// The record says "rolled-back" only once the containers really run the old images.
	u.jobs.Add(1)
	go func() {
		defer u.jobs.Done()
		bctx := u.bg()
		failed := true
		_ = h.FollowOp(bctx, op.ID, func(ev host.OpEvent) error {
			if ev.Done {
				failed = ev.Error != ""
			}
			return nil
		})
		if !failed {
			_ = u.st.SetImageUpdateResult(bctx, rec.ID, store.UpdateRolledBack)
		}
		_, _ = u.checkOneStack(bctx, h, he, engine.StackSummary{Name: stack}, u.now())
	}()

	// Switch the policy to notify and skip the rolled-back digests right away, even if the
	// rollback fails: the next window must not install the same image again.
	pol, _ := u.st.StackUpdatePolicy(ctx, hostID, stack)
	pol.HostID = hostID
	pol.Stack = stack
	pol.Policy = store.PolicyNotify
	if pol.SkippedDigests == nil {
		pol.SkippedDigests = map[string]string{}
	}
	for _, svc := range rec.Services {
		if svc.NewDigest != "" && svc.NewDigest != svc.OldDigest {
			pol.SkippedDigests[svc.Service] = svc.NewDigest
		}
	}
	_ = u.st.SaveStackUpdatePolicy(ctx, pol)

	return op, nil
}

func buildUpdateDetail(before map[string]engine.RunningImage, remoteByService map[string]string) string {
	var parts []string
	names := make([]string, 0, len(before))
	for svc := range before {
		names = append(names, svc)
	}
	sort.Strings(names)
	for _, svc := range names {
		b := before[svc]
		rem := remoteByService[svc]
		if rem != "" && b.Digest != "" && rem != b.Digest {
			parts = append(parts, fmt.Sprintf("%s (%s → %s)", b.Ref, ShortDigest(b.Digest), ShortDigest(rem)))
		} else if b.Ref != "" {
			parts = append(parts, b.Ref)
		}
	}
	return strings.Join(parts, ", ")
}

func buildUpdateServices(before, after map[string]engine.RunningImage, remoteByService map[string]string) []store.ImageUpdateService {
	seen := map[string]bool{}
	var names []string
	for svc := range before {
		seen[svc] = true
		names = append(names, svc)
	}
	for svc := range after {
		if !seen[svc] {
			seen[svc] = true
			names = append(names, svc)
		}
	}
	sort.Strings(names)

	out := make([]store.ImageUpdateService, 0, len(names))
	for _, svc := range names {
		b := before[svc]
		a := after[svc]
		ref := b.Ref
		if ref == "" {
			ref = a.Ref
		}
		newDigest := a.Digest
		if newDigest == "" {
			newDigest = remoteByService[svc]
		}
		newID := a.ImageID
		if newID == "" {
			newID = b.ImageID
		}
		out = append(out, store.ImageUpdateService{
			Service:   svc,
			Ref:       ref,
			OldID:     b.ImageID,
			OldDigest: b.Digest,
			NewID:     newID,
			NewDigest: newDigest,
		})
	}
	return out
}

func imagesUnchanged(before, after map[string]engine.RunningImage) bool {
	if len(after) == 0 {
		return true
	}
	for svc, b := range before {
		if a, ok := after[svc]; ok && a.ImageID != "" && b.ImageID != "" && a.ImageID != b.ImageID {
			return false
		}
	}
	return true
}

// ShortDigest returns the first 7 hex characters of a sha256:... digest or ID.
func ShortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 7 {
		return d[:7]
	}
	if d == "" {
		return "unknown"
	}
	return d
}

// ---- Notifications ---------------------------------------------------------

func (u *Updater) deliverToChannels(ctx context.Context, wantIDs []int64, msg notify.Message) {
	if len(wantIDs) == 0 {
		return
	}
	want := map[int64]bool{}
	for _, id := range wantIDs {
		want[id] = true
	}
	chs, err := u.st.Channels(ctx)
	if err != nil {
		return
	}
	u.mu.Lock()
	send := u.sendFn
	u.mu.Unlock()
	for _, ch := range chs {
		if !want[ch.ID] || !ch.Enabled {
			continue
		}
		if err := send(ctx, ch, msg); err != nil {
			u.log.Warn("update notification failed", "channel", ch.Name, "err", err)
		}
	}
}

func (u *Updater) sendDiscoveryBatch(ctx context.Context, channelIDs []int64, notices []pendingNotice) {
	// One line per finding, e.g. "whoami · web: traefik/whoami:v1.10 has a new build".
	var lines []string
	for _, n := range notices {
		where := n.stack
		if n.hostName != "" && n.hostID != host.LocalID {
			where = n.stack + " on " + n.hostName
		}
		prefix := fmt.Sprintf("%s · %s: ", where, n.service)
		if n.newDigest != "" {
			lines = append(lines, prefix+n.ref+" has a new build")
		}
		if n.newerTag != "" {
			lines = append(lines, prefix+n.newerTag+" available")
		}
		if n.newerMajor != "" {
			lines = append(lines, prefix+n.newerMajor+" is a new major version")
		}
	}
	title := fmt.Sprintf("%d image updates available", len(lines))
	if len(lines) == 1 {
		title = "1 image update available"
	}
	linkURL := ""
	pubURL := u.getPublicURL()
	if pubURL != "" {
		linkURL = pubURL + "/#/updates"
		if len(notices) == 1 {
			linkURL = u.stackURL(notices[0].hostID, notices[0].stack)
		}
	}
	u.deliverToChannels(ctx, channelIDs, notify.Message{
		Status: "up",
		Title:  title,
		Text:   strings.Join(lines, "\n"),
		URL:    linkURL,
	})
}

// SetPublicURL updates the public URL used in update notifications live.
func (u *Updater) SetPublicURL(pub string) {
	u.mu.Lock()
	u.publicURL = strings.TrimRight(pub, "/")
	u.mu.Unlock()
}

func (u *Updater) getPublicURL() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.publicURL
}

func (u *Updater) stackURL(hostID, stack string) string {
	pub := u.getPublicURL()
	if pub == "" {
		return ""
	}
	return fmt.Sprintf("%s/#/h/%s/stacks/%s", pub, hostID, stack)
}

// changedServices lists the services whose image really changed, as
// "web traefik/whoami:v1.10 (sha256:1a2b3c4 → 5d6e7f8)".
func changedServices(rec store.ImageUpdate) []string {
	var parts []string
	for _, s := range rec.Services {
		if s.OldID != "" && s.OldID == s.NewID {
			continue
		}
		if s.OldDigest != "" && s.OldDigest == s.NewDigest {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s (sha256:%s → %s)", s.Service, s.Ref, ShortDigest(s.OldDigest), ShortDigest(s.NewDigest)))
	}
	return parts
}

// autoRollbackWindow is how long after an auto-update a DOWN check still rolls it back:
// the monitor's 3-minute deploy silence plus 10 minutes of real checking.
const autoRollbackWindow = 13 * time.Minute

// HandleStackCheckDown triggers an automatic rollback when a check on a stack fails
// shortly after an automatic image update.
func (u *Updater) HandleStackCheckDown(hostID, stack, checkName, cause string) {
	if u == nil || stack == "" {
		return
	}
	ctx, cancel := context.WithTimeout(u.bg(), 2*time.Minute)
	defer cancel()

	u.rollbackMu.Lock()
	defer u.rollbackMu.Unlock()

	updates, err := u.st.ImageUpdates(ctx, hostID, stack, 1)
	if err != nil || len(updates) == 0 {
		return
	}
	rec := updates[0]
	if rec.Result != store.UpdateOK || rec.Actor != ActorAuto {
		return
	}
	refTime := rec.FinishedAt
	if refTime.IsZero() {
		refTime = rec.StartedAt
	}
	// Failures inside the deploy silence never reach here (the monitor marks them
	// silenced), so the window is the silence plus 10 minutes of real checking.
	if u.now().Sub(refTime) > autoRollbackWindow {
		return
	}
	already, err := u.st.HasAutoRollbackForUpdate(ctx, rec.ID)
	if err != nil || already {
		return
	}

	// Record the attempt before rolling back: a failing rollback must not be retried on
	// every following DOWN.
	rbID, err := u.st.InsertAutoRollback(ctx, store.AutoRollback{
		HostID:    hostID,
		Stack:     stack,
		UpdateID:  rec.ID,
		CheckName: checkName,
		Reason:    cause,
		At:        u.now(),
	})
	if err != nil {
		if u.log != nil {
			u.log.Warn("auto-rollback: record attempt", "host", hostID, "stack", stack, "err", err)
		}
		return
	}

	// Rollback also switches the stack's policy to notify and skips the bad digests.
	if _, err := u.Rollback(ctx, hostID, stack, rec.ID, "auto-rollback"); err != nil {
		if u.log != nil {
			u.log.Warn("auto-rollback failed", "host", hostID, "stack", stack, "err", err)
		}
		_ = u.st.SetAutoRollbackReason(ctx, rbID, fmt.Sprintf("%s (rollback failed: %v)", cause, err))
		_ = u.st.Audit(ctx, store.AuditEntry{
			At:      u.now(),
			Actor:   "system",
			Action:  "stack.auto_rollback",
			HostID:  hostID,
			Stack:   stack,
			Target:  fmt.Sprintf("update #%d (%s): %v", rec.ID, checkName, err),
			Outcome: "error",
		})
		return
	}
	_ = u.st.Audit(ctx, store.AuditEntry{
		At:      u.now(),
		Actor:   "system",
		Action:  "stack.auto_rollback",
		HostID:  hostID,
		Stack:   stack,
		Target:  fmt.Sprintf("update #%d (%s)", rec.ID, checkName),
		Outcome: "ok",
	})

	var ids []int64
	seen := map[int64]bool{}
	if us, err := u.st.UpdateSettings(ctx); err == nil {
		for _, id := range us.Channels {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if chs, err := u.st.Channels(ctx); err == nil {
		for _, ch := range chs {
			if !ch.Enabled || seen[ch.ID] {
				continue
			}
			var cfg notify.Config
			if json.Unmarshal([]byte(ch.Config), &cfg) == nil && cfg.WantsEvent("rollback") {
				seen[ch.ID] = true
				ids = append(ids, ch.ID)
			}
		}
	}
	rbMsg := notify.Message{
		Status: "down",
		Title:  fmt.Sprintf("Auto-rollback: %s (%s)", stack, checkName),
		Text:   fmt.Sprintf("Check %q failed after auto-update (%s). Rolled back to previous images and switched update policy to notify.", checkName, cause),
		URL:    u.stackURL(hostID, stack),
	}
	if len(ids) > 0 {
		u.deliverToChannels(ctx, ids, rbMsg)
	}
	if u.Push != nil {
		u.Push(ctx, notify.EventRollback, rbMsg)
	}
}

func (u *Updater) sendAutoAppliedNotice(ctx context.Context, channelIDs []int64, stack string, rec store.ImageUpdate) {
	seen := map[int64]bool{}
	var ids []int64
	for _, id := range channelIDs {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if chs, err := u.st.Channels(ctx); err == nil {
		for _, ch := range chs {
			if !ch.Enabled || seen[ch.ID] {
				continue
			}
			var cfg notify.Config
			if json.Unmarshal([]byte(ch.Config), &cfg) == nil && cfg.WantsEvent("auto_update") {
				seen[ch.ID] = true
				ids = append(ids, ch.ID)
			}
		}
	}
	u.deliverToChannels(ctx, ids, notify.Message{
		Status: "up",
		Title:  fmt.Sprintf("Updated %s: %s", stack, strings.Join(changedServices(rec), ", ")),
		URL:    u.stackURL(rec.HostID, stack),
	})
}

func (u *Updater) sendFailureNotice(ctx context.Context, channelIDs []int64, stack string, rec store.ImageUpdate) {
	var logs []string
	if rec.LogTail != "" {
		logs = strings.Split(rec.LogTail, "\n")
	}
	first := "unknown error"
	if len(logs) > 0 {
		first = logs[len(logs)-1] // the operation's error is the last line of the tail
	}
	text := "Nothing was changed; the running containers keep the old image."
	if rec.Result == store.UpdateUpFailed {
		text = "The stack may be degraded. Open it in Moorage to roll the update back."
	}
	u.deliverToChannels(ctx, channelIDs, notify.Message{
		Status: "down",
		Title:  fmt.Sprintf("Update of %s failed: %s", stack, first),
		Text:   text,
		Logs:   logs,
		URL:    u.stackURL(rec.HostID, stack),
	})
}
