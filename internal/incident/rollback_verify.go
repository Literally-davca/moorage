package incident

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/store"
)

// Errors of StartRollbackVerify.
var (
	ErrNoRollbackTarget = errors.New("there is no earlier version to roll back to")
	ErrChangedSince     = errors.New("the stack changed after the outage began; roll back from the stack's History instead")
	ErrRollbackRunning  = errors.New("a Rollback & Verify is already running for this incident")
	ErrIncidentClosed   = errors.New("the incident is already closed")
	ErrNoStack          = errors.New("this check isn't tied to a stack, so there is nothing to roll back")
)

// RollbackType says what to undo.
type RollbackType string

const (
	RollbackTypeImage   RollbackType = "image"
	RollbackTypeCompose RollbackType = "compose"
)

// Phases of a Rollback & Verify.
const (
	PhaseVerifying = "verifying"
	PhaseDone      = "done"
	PhaseFailed    = "failed"
)

// Monitor is what Rollback & Verify needs from the check monitor: the monitor decides
// whether the check is up again (and closes the incident, sends the recovery alert and
// updates status pages); this package only asks it to probe now.
type Monitor interface {
	KickCheck(id int64)
	StackEvent(hostID, stack, kind, actor, detail string)
	RollbackFailed(c store.Check, reason string)
}

// RollbackVerifyState is stored with the incident.
type RollbackVerifyState struct {
	Active             bool      `json:"active"`
	StartedAt          time.Time `json:"startedAt,omitzero"`
	FinishedAt         time.Time `json:"finishedAt,omitzero"`
	Phase              string    `json:"phase,omitempty"`
	RollbackType       string    `json:"rollbackType,omitempty"`
	TargetRef          string    `json:"targetRef,omitempty"`
	TargetLabel        string    `json:"targetLabel,omitempty"`
	TriggeredBy        string    `json:"triggeredBy,omitempty"`
	LastCheckAt        time.Time `json:"lastCheckAt,omitzero"`
	VerificationPassed bool      `json:"verificationPassed,omitempty"`
	Error              string    `json:"error,omitempty"`
	IncidentClosed     bool      `json:"incidentClosed,omitempty"`
}

// RollbackOption is a rollback the UI offers.
type RollbackOption struct {
	Type        RollbackType `json:"type"`
	Label       string       `json:"label"`
	Description string       `json:"description"`
	TargetRef   string       `json:"targetRef"`
}

// RollbackVerify undoes the last image update or compose change of a check's stack and
// waits until the check reports the service up again.
type RollbackVerify struct {
	st  *store.Store
	reg Hosts
	mon Monitor
	log *slog.Logger

	VerifyTimeout time.Duration // 10 min
	PollInterval  time.Duration // 15 s

	mu      sync.Mutex
	ctx     context.Context
	running map[int64]bool
}

// NewRollbackVerify creates the handler; call Start before using it.
func NewRollbackVerify(st *store.Store, reg Hosts, mon Monitor, log *slog.Logger) *RollbackVerify {
	return &RollbackVerify{st: st, reg: reg, mon: mon, log: log, VerifyTimeout: 10 * time.Minute, PollInterval: 15 * time.Second,
		ctx: context.Background(), running: map[int64]bool{}}
}

// Start sets the context background verification runs in and marks verifications that
// a restart interrupted as failed.
func (rv *RollbackVerify) Start(ctx context.Context) {
	rv.mu.Lock()
	rv.ctx = ctx
	rv.mu.Unlock()
	ids, err := rv.st.IncidentsWithActiveRollback(ctx)
	if err != nil {
		rv.log.Warn("rollback & verify: interrupted runs", "err", err)
		return
	}
	for _, id := range ids {
		s, err := LoadState(ctx, rv.st, id)
		if err != nil {
			continue
		}
		s.Active, s.Phase, s.FinishedAt = false, PhaseFailed, time.Now()
		s.Error = "Moorage restarted before the check was verified; check the stack and roll back again if needed"
		_ = saveState(ctx, rv.st, id, s)
	}
}

// pickComposeTarget returns the version to restore: the one before the newest version
// (which runs now). versions are newest first. It refuses when the stack was changed
// after the outage began, because restoring would also throw those changes away.
func pickComposeTarget(versions []engine.Version, startedAt time.Time) (running, target engine.Version, err error) {
	if len(versions) < 2 {
		return engine.Version{}, engine.Version{}, ErrNoRollbackTarget
	}
	if versions[0].Time.After(startedAt) {
		return engine.Version{}, engine.Version{}, ErrChangedSince
	}
	return versions[0], versions[1], nil
}

// pickImageUpdate returns the newest successful image update before the outage. updates
// are newest first. A successful update after the outage began makes it ambiguous.
func pickImageUpdate(updates []store.ImageUpdate, startedAt time.Time) (store.ImageUpdate, error) {
	for _, u := range updates {
		if u.Result != store.UpdateOK || len(changedImages(u)) == 0 {
			continue
		}
		if updateTime(u).After(startedAt) {
			return store.ImageUpdate{}, ErrChangedSince
		}
		return u, nil
	}
	return store.ImageUpdate{}, ErrNoRollbackTarget
}

func imageTargets(u store.ImageUpdate) map[string]engine.RollbackTarget {
	targets := map[string]engine.RollbackTarget{}
	for _, s := range u.Services {
		if s.OldID == "" && s.OldDigest == "" {
			continue
		}
		targets[s.Service] = engine.RollbackTarget{Ref: s.Ref, OldID: s.OldID, OldDigest: s.OldDigest}
	}
	return targets
}

func stamp(t time.Time) string { return t.Local().Format("Jan 2 15:04") }

func (rv *RollbackVerify) load(ctx context.Context, incidentID int64) (store.Incident, store.Check, error) {
	inc, err := rv.st.IncidentByID(ctx, incidentID)
	if err != nil {
		return inc, store.Check{}, err
	}
	c, err := rv.st.CheckByID(ctx, inc.CheckID)
	return inc, c, err
}

// GetRollbackOptions lists what can be rolled back for an open incident.
func (rv *RollbackVerify) GetRollbackOptions(ctx context.Context, incidentID int64) ([]RollbackOption, error) {
	inc, c, err := rv.load(ctx, incidentID)
	if err != nil {
		return nil, err
	}
	options := []RollbackOption{}
	if !inc.EndedAt.IsZero() || c.Stack == "" || c.HostID == "" {
		return options, nil
	}
	if updates, err := rv.st.ImageUpdates(ctx, c.HostID, c.Stack, 10); err == nil {
		if u, err := pickImageUpdate(updates, inc.StartedAt); err == nil {
			options = append(options, RollbackOption{
				Type:        RollbackTypeImage,
				Label:       "Roll back the image update of " + stamp(updateTime(u)),
				Description: strings.Join(changedImages(u), "; "),
				TargetRef:   fmt.Sprintf("update#%d", u.ID),
			})
		}
	}
	if rv.reg != nil {
		if h, _, err := rv.reg.Get(ctx, c.HostID); err == nil {
			if versions, err := h.History(ctx, c.Stack); err == nil {
				if running, target, err := pickComposeTarget(versions, inc.StartedAt); err == nil {
					options = append(options, RollbackOption{
						Type:        RollbackTypeCompose,
						Label:       "Undo the configuration change of " + stamp(running.Time),
						Description: fmt.Sprintf("Restore version %s (%s) and redeploy.", target.Short, target.Message),
						TargetRef:   target.Hash,
					})
				}
			}
		}
	}
	return options, nil
}

// StartRollbackVerify runs the rollback and verifies it in the background.
func (rv *RollbackVerify) StartRollbackVerify(ctx context.Context, incidentID int64, kind RollbackType, user string) (*RollbackVerifyState, error) {
	rv.mu.Lock()
	if rv.running[incidentID] {
		rv.mu.Unlock()
		return nil, ErrRollbackRunning
	}
	rv.running[incidentID] = true
	bg := rv.ctx
	rv.mu.Unlock()
	release := func() {
		rv.mu.Lock()
		delete(rv.running, incidentID)
		rv.mu.Unlock()
	}

	inc, c, err := rv.load(ctx, incidentID)
	if err != nil {
		release()
		return nil, err
	}
	if !inc.EndedAt.IsZero() {
		release()
		return nil, ErrIncidentClosed
	}
	if c.Stack == "" || c.HostID == "" {
		release()
		return nil, ErrNoStack
	}
	if rv.reg == nil {
		release()
		return nil, errors.New("hosts aren't available")
	}
	h, _, err := rv.reg.Get(ctx, c.HostID)
	if err != nil {
		release()
		return nil, err
	}
	author := user + " (Rollback & Verify)"
	var op engine.OpInfo
	var ref, label string
	switch kind {
	case RollbackTypeImage:
		updates, err := rv.st.ImageUpdates(ctx, c.HostID, c.Stack, 10)
		if err != nil {
			release()
			return nil, err
		}
		u, err := pickImageUpdate(updates, inc.StartedAt)
		if err != nil {
			release()
			return nil, err
		}
		if op, err = h.RollbackImages(ctx, c.Stack, imageTargets(u), author); err != nil {
			release()
			return nil, err
		}
		ref, label = fmt.Sprintf("update#%d", u.ID), "image update of "+stamp(updateTime(u))
	case RollbackTypeCompose:
		versions, err := h.History(ctx, c.Stack)
		if err != nil {
			release()
			return nil, err
		}
		running, target, err := pickComposeTarget(versions, inc.StartedAt)
		if err != nil {
			release()
			return nil, err
		}
		if _, err := h.Restore(ctx, c.Stack, target.Hash, author); err != nil {
			release()
			return nil, err
		}
		if op, err = h.RunAction(ctx, c.Stack, "up", author); err != nil {
			release()
			return nil, err
		}
		ref, label = target.Hash, fmt.Sprintf("configuration change of %s (back to %s)", stamp(running.Time), target.Short)
	default:
		release()
		return nil, fmt.Errorf("unknown rollback type %q", kind)
	}
	if rv.mon != nil {
		// a deploy-like event: silences the stack's checks for a while and shows in the Logbook
		rv.mon.StackEvent(c.HostID, c.Stack, "rollback", user, fmt.Sprintf("Rollback & Verify for incident #%d: %s", incidentID, label))
	}
	state := &RollbackVerifyState{Active: true, StartedAt: time.Now(), Phase: PhaseVerifying, RollbackType: string(kind),
		TargetRef: ref, TargetLabel: label, TriggeredBy: user}
	if err := saveState(ctx, rv.st, incidentID, state); err != nil {
		rv.log.Warn("rollback & verify: save state", "err", err)
	}
	snapshot := *state
	go func() {
		defer release()
		rv.verify(bg, incidentID, c, h, op, &snapshot)
	}()
	return state, nil
}

// verify waits for the rollback operation, then asks the monitor to probe the check until
// the monitor closes the incident (the check passed) and the stack's containers run.
func (rv *RollbackVerify) verify(ctx context.Context, incidentID int64, c store.Check, h host.Host, op engine.OpInfo, s *RollbackVerifyState) {
	if op.ID != "" {
		opErr := ""
		err := h.FollowOp(ctx, op.ID, func(ev host.OpEvent) error {
			if ev.Done && ev.Error != "" {
				opErr = ev.Error
			}
			return nil
		})
		if ctx.Err() != nil {
			return // shutting down: Start marks it interrupted next time
		}
		if err != nil {
			opErr = err.Error()
		}
		if opErr != "" {
			rv.finish(ctx, incidentID, c, s, "the rollback itself failed: "+opErr)
			return
		}
	}
	deadline := time.Now().Add(rv.VerifyTimeout)
	why := "the check didn't pass"
	t := time.NewTicker(rv.PollInterval)
	defer t.Stop()
	for {
		if rv.mon != nil {
			rv.mon.KickCheck(c.ID)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.LastCheckAt = time.Now()
		inc, err := rv.st.IncidentByID(ctx, incidentID)
		switch {
		case err != nil:
			why = "can't read the incident: " + err.Error()
		case inc.EndedAt.IsZero():
			why = "the check still fails"
		default:
			if bad := unhealthyContainers(ctx, h, c.Stack); bad != "" {
				why = "the check passes but " + bad
			} else {
				s.VerificationPassed, s.IncidentClosed = true, true
				rv.finish(ctx, incidentID, c, s, "")
				return
			}
		}
		_ = saveState(ctx, rv.st, incidentID, s)
		if time.Now().After(deadline) {
			rv.finish(ctx, incidentID, c, s, why+" within "+rv.VerifyTimeout.String())
			return
		}
	}
}

// unhealthyContainers describes containers of the stack that don't run or are unhealthy
// ("" when all run; a stack without containers counts as not running).
func unhealthyContainers(ctx context.Context, h host.Host, stack string) string {
	st, err := h.GetStack(ctx, stack)
	if err != nil {
		return "the stack can't be read: " + err.Error()
	}
	if len(st.Containers) == 0 {
		return "the stack has no containers"
	}
	var bad []string
	for _, ct := range st.Containers {
		if ct.State != "running" || ct.Health == "unhealthy" {
			bad = append(bad, fmt.Sprintf("%s is %s", firstNonEmpty(ct.Service, ct.Name), firstNonEmpty(ct.Health, ct.State)))
		}
	}
	return strings.Join(bad, ", ")
}

func (rv *RollbackVerify) finish(ctx context.Context, incidentID int64, c store.Check, s *RollbackVerifyState, failure string) {
	s.Active, s.FinishedAt = false, time.Now()
	outcome := "ok"
	if failure == "" {
		s.Phase = PhaseDone
	} else {
		s.Phase, s.Error, outcome = PhaseFailed, failure, "failed"
	}
	if err := saveState(ctx, rv.st, incidentID, s); err != nil {
		rv.log.Warn("rollback & verify: save state", "err", err)
	}
	_ = rv.st.Audit(ctx, store.AuditEntry{
		At: time.Now(), Actor: s.TriggeredBy, Action: "incident.rollback_verify.result", HostID: c.HostID, Stack: c.Stack,
		Target: fmt.Sprintf("incident #%d: %s", incidentID, s.TargetLabel), Outcome: outcome,
	})
	if failure == "" {
		rv.log.Info("rollback & verify passed", "incident", incidentID, "check", c.Name)
		return
	}
	rv.log.Error("rollback & verify failed", "incident", incidentID, "check", c.Name, "reason", failure)
	if rv.mon != nil {
		rv.mon.RollbackFailed(c, failure)
	}
}

func saveState(ctx context.Context, st *store.Store, incidentID int64, s *RollbackVerifyState) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return st.SetIncidentRollback(ctx, incidentID, string(b))
}

// LoadState returns the Rollback & Verify state of an incident (zero when none ran).
func LoadState(ctx context.Context, st *store.Store, incidentID int64) (*RollbackVerifyState, error) {
	data, err := st.IncidentRollback(ctx, incidentID)
	if err != nil {
		return nil, err
	}
	s := &RollbackVerifyState{}
	if data == "" || data == "{}" {
		return s, nil
	}
	return s, json.Unmarshal([]byte(data), s)
}
