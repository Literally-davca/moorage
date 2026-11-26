package snapshot

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Last is the result of the latest publish attempt, shown in the UI.
type Last struct {
	At          time.Time `json:"at"`
	OK          bool      `json:"ok"`
	Error       string    `json:"error,omitempty"`
	Trigger     string    `json:"trigger,omitempty"` // change | periodic | manual | start | retry
	Target      string    `json:"target,omitempty"`
	Pages       []string  `json:"pages,omitempty"` // slugs
	LastSuccess time.Time `json:"lastSuccess,omitzero"`
	Failures    int       `json:"failures,omitempty"` // in a row
	NextRetry   time.Time `json:"nextRetry,omitzero"`
}

// Publisher publishes snapshots after status changes (debounced), periodically, and
// retries failures with a growing delay. All hooks are set by the caller.
type Publisher struct {
	// Load returns the saved configuration and whether the add-on is enabled.
	Load func(ctx context.Context) (Config, bool, error)
	// Render returns the files of every page to publish and their slugs.
	Render func(ctx context.Context, c Config) ([]File, []string, error)
	// Save persists the last result (optional).
	Save func(ctx context.Context, l Last)
	// Target builds the target (NewTarget when nil; tests replace it).
	Target func(c Config) (Target, error)
	Log    *slog.Logger

	Debounce time.Duration // after a change; default 10 s
	Timeout  time.Duration // one publish; default 2 min

	kick   chan string
	runMu  sync.Mutex // one publish at a time (worker and "Publish now")
	mu     sync.Mutex
	last   Last
	lastAt time.Time // last attempt, for the minimum gap
}

func (p *Publisher) init() {
	p.mu.Lock()
	if p.kick == nil {
		p.kick = make(chan string, 1)
	}
	p.mu.Unlock()
}

// SetLast restores the last result saved before a restart.
func (p *Publisher) SetLast(l Last) {
	p.mu.Lock()
	p.last = l
	p.mu.Unlock()
}

// Last returns the latest result.
func (p *Publisher) Last() Last {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

// Kick asks for a publish soon (a status changed, a page or the settings were saved).
// It never blocks.
func (p *Publisher) Kick(reason string) {
	if p == nil {
		return
	}
	p.init()
	select {
	case p.kick <- reason:
	default: // one is pending already
	}
}

// retryDelay grows 30 s, 1, 2, 4, 8 min, up to the periodic interval.
func retryDelay(failures int, interval time.Duration) time.Duration {
	d := 30 * time.Second
	for i := 1; i < failures && d < interval; i++ {
		d *= 2
	}
	return min(d, interval)
}

// Run is the worker. It publishes shortly after start, after changes (debounced and at
// least MinGap apart), every interval, and retries failures.
func (p *Publisher) Run(ctx context.Context) {
	p.init()
	debounce := p.Debounce
	if debounce <= 0 {
		debounce = 10 * time.Second
	}
	next := time.Now().Add(debounce)
	trigger := "start"
	timer := time.NewTimer(time.Until(next))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.kick:
			cfg, _, _ := p.Load(ctx)
			want := time.Now().Add(debounce)
			p.mu.Lock()
			if gap := p.lastAt.Add(cfg.MinGap()); gap.After(want) {
				want = gap
			}
			p.mu.Unlock()
			if want.Before(next) {
				next, trigger = want, "change"
				timer.Reset(time.Until(next))
			}
		case <-timer.C:
			wait := p.runOnce(ctx, trigger)
			next, trigger = time.Now().Add(wait), "periodic"
			if l := p.Last(); !l.OK && l.Failures > 0 && !l.NextRetry.IsZero() {
				trigger = "retry"
			}
			timer.Reset(wait)
		}
	}
}

// runOnce publishes if the add-on is on and configured, and returns when to try next.
func (p *Publisher) runOnce(ctx context.Context, trigger string) time.Duration {
	cfg, on, err := p.Load(ctx)
	if err != nil || !on || !cfg.Configured() {
		return time.Minute // check again later; a save kicks it anyway
	}
	interval := cfg.IntervalDuration()
	if interval <= 0 {
		interval = DefaultInterval * time.Minute
	}
	l, err := p.publish(ctx, cfg, trigger)
	if err == nil {
		return interval
	}
	d := retryDelay(l.Failures, interval)
	p.mu.Lock()
	p.last.NextRetry = time.Now().Add(d)
	l = p.last
	p.mu.Unlock()
	if p.Save != nil {
		p.Save(ctx, l)
	}
	return d
}

// PublishNow publishes right away (the "Publish now" button) and returns the result.
func (p *Publisher) PublishNow(ctx context.Context) (Last, error) {
	cfg, on, err := p.Load(ctx)
	if err != nil {
		return Last{}, err
	}
	if !on {
		return Last{}, errors.New("turn the add-on on first")
	}
	if !cfg.Configured() {
		return Last{}, errors.New("save where to publish first")
	}
	return p.publish(ctx, cfg, "manual")
}

func (p *Publisher) publish(ctx context.Context, cfg Config, trigger string) (Last, error) {
	p.runMu.Lock()
	defer p.runMu.Unlock()
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	now := time.Now().UTC()
	p.mu.Lock()
	p.lastAt = now
	prev := p.last
	p.mu.Unlock()

	l := Last{At: now, Trigger: trigger, Target: cfg.Describe(), LastSuccess: prev.LastSuccess}
	err := func() error {
		files, slugs, err := p.Render(ctx, cfg)
		l.Pages = slugs
		if err != nil {
			return err
		}
		if len(files) == 0 {
			return errNoPages
		}
		newTarget := p.Target
		if newTarget == nil {
			newTarget = NewTarget
		}
		t, err := newTarget(cfg)
		if err != nil {
			return err
		}
		return t.Publish(ctx, files)
	}()
	if err != nil {
		l.Error = err.Error()
		l.Failures = prev.Failures + 1
		if p.Log != nil {
			lvl := slog.LevelWarn
			if l.Failures >= 3 { // the public copy is getting stale
				lvl = slog.LevelError
			}
			p.Log.Log(context.Background(), lvl, "status page snapshot failed", "target", l.Target, "failures", l.Failures, "err", l.Error)
		}
	} else {
		l.OK, l.LastSuccess = true, now
		if p.Log != nil && prev.Failures > 0 {
			p.Log.Info("status page snapshot published again", "target", l.Target)
		}
	}
	p.mu.Lock()
	p.last = l
	p.mu.Unlock()
	if p.Save != nil {
		p.Save(context.WithoutCancel(ctx), l)
	}
	return l, err
}
