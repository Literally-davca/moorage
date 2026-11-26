package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/cmd/display"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
)

// Actions a user can run on a managed stack.
// "rolling" and "rolling-update" deploy service by service with an automatic rollback
// (rolling.go); both pull first.
var Actions = map[string]bool{"up": true, "down": true, "stop": true, "restart": true, "pull": true, "update": true, "rolling": true, "rolling-update": true}

// opTimeout bounds one compose operation (large image pulls on a Pi can be slow).
const opTimeout = 30 * time.Minute

// Op is one running or finished compose operation with its output.
type Op struct {
	ID       string    `json:"id"`
	Stack    string    `json:"stack"`
	Action   string    `json:"action"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`
	Done     bool      `json:"done"`
	Error    string    `json:"error,omitempty"`

	mu    sync.Mutex
	lines []string
	part  []byte
	subs  map[chan struct{}]struct{}
}

// Snapshot returns the output lines from index from, and whether the op is done.
func (o *Op) Snapshot(from int) (lines []string, done bool, errMsg string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if from < len(o.lines) {
		lines = append(lines, o.lines[from:]...)
	}
	return lines, o.Done, o.Error
}

// Subscribe returns a channel that receives a tick on every change.
func (o *Op) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	o.mu.Lock()
	o.subs[ch] = struct{}{}
	o.mu.Unlock()
	return ch, func() {
		o.mu.Lock()
		delete(o.subs, ch)
		o.mu.Unlock()
	}
}

func (o *Op) notifyLocked() {
	for ch := range o.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Write implements io.Writer and splits compose output into lines.
func (o *Op) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.part = append(o.part, p...)
	for {
		i := bytes.IndexAny(o.part, "\r\n")
		if i < 0 {
			break
		}
		if line := string(bytes.TrimRight(o.part[:i], " ")); line != "" {
			o.lines = append(o.lines, line)
		}
		o.part = o.part[i+1:]
	}
	if len(o.lines) > 5000 { // keep memory bounded on huge pulls
		o.lines = o.lines[len(o.lines)-5000:]
	}
	o.notifyLocked()
	return len(p), nil
}

func (o *Op) finish(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.part) > 0 {
		o.lines = append(o.lines, string(o.part))
		o.part = nil
	}
	o.Done = true
	o.Finished = time.Now()
	if err != nil {
		o.Error = err.Error()
	}
	o.notifyLocked()
}

type opRegistry struct {
	mu  sync.Mutex
	seq int
	ops []*Op // newest last, capped
}

func newOpRegistry() *opRegistry { return &opRegistry{} }

func (r *opRegistry) add(stack, action string) *Op {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	op := &Op{ID: fmt.Sprintf("%d-%d", time.Now().Unix(), r.seq), Stack: stack, Action: action, Started: time.Now(), subs: map[chan struct{}]struct{}{}}
	r.ops = append(r.ops, op)
	if len(r.ops) > 50 {
		r.ops = r.ops[len(r.ops)-50:]
	}
	return op
}

// Op returns an operation by id.
func (e *Engine) Op(id string) (*Op, bool) {
	e.ops.mu.Lock()
	defer e.ops.mu.Unlock()
	for _, op := range e.ops.ops {
		if op.ID == id {
			return op, true
		}
	}
	return nil, false
}

// RunAction starts a compose operation in the background and returns at once.
func (e *Engine) RunAction(name, action string) (*Op, error) {
	if !Actions[action] {
		return nil, fmt.Errorf("unknown action %q", action)
	}
	if selfBlocked[action] && e.IsSelf(name) {
		return nil, ErrSelfStack
	}
	dir, err := e.stackDir(name)
	if err != nil {
		return nil, err
	}
	file := composeFile(dir)
	if file == "" {
		if _, err := os.Stat(dir); err != nil {
			return nil, ErrReadOnly
		}
		return nil, ErrNotFound
	}
	if !e.lock(name) {
		return nil, ErrBusy
	}
	op := e.ops.add(name, action)
	go func() {
		defer e.unlock(name)
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		err := e.runCompose(ctx, op, name, dir, file, action)
		if err != nil {
			fmt.Fprintf(op, "✗ %s failed: %v\n", action, err)
			if hint := podmanOpHint(err); hint != "" && e.isPodman(ctx) {
				fmt.Fprintln(op, hint)
			}
		} else {
			fmt.Fprintf(op, "✓ %s finished\n", action)
		}
		op.finish(err)
		if err != nil {
			e.log.Warn("compose operation failed", "stack", name, "action", action, "err", err)
		} else {
			e.log.Info("compose operation", "stack", name, "action", action)
		}
	}()
	return op, nil
}

func (e *Engine) runCompose(ctx context.Context, op *Op, name, dir, file, action string) error {
	svc, err := compose.NewComposeService(e.cli,
		compose.WithStreams(op, op, nil),
		compose.WithEventProcessor(display.Plain(op)),
		compose.WithPrompt(compose.AlwaysOkPrompt()),
	)
	if err != nil {
		return err
	}
	load := api.ProjectLoadOptions{ProjectName: name, ConfigPaths: []string{file}, WorkingDir: dir}
	if _, err := os.Stat(filepath.Join(dir, ".env")); err == nil {
		load.EnvFiles = []string{filepath.Join(dir, ".env")}
	}
	project, err := svc.LoadProject(ctx, load)
	if err != nil {
		return err
	}
	if (action == "up" || action == "up-local" || action == "update" || action == "restart" || action == "rolling" || action == "rolling-update") && e.isPodman(ctx) {
		for _, w := range podmanComposeWarnings(project, e.rootless(ctx)) {
			fmt.Fprintln(op, w)
		}
	}

	switch action {
	case "up":
		return svc.Up(ctx, project, api.UpOptions{Start: api.StartOptions{Project: project}})
	case "up-local": // rollback: use the images that are local now, never pull
		for n, sc := range project.Services {
			if sc.Build == nil {
				sc.PullPolicy = types.PullPolicyNever
				project.Services[n] = sc
			}
		}
		return svc.Up(ctx, project, api.UpOptions{Start: api.StartOptions{Project: project}})
	case "down":
		return svc.Down(ctx, name, api.DownOptions{Project: project})
	case "stop":
		return svc.Stop(ctx, name, api.StopOptions{Project: project})
	case "restart":
		return svc.Restart(ctx, name, api.RestartOptions{Project: project})
	case "pull":
		return svc.Pull(ctx, project, api.PullOptions{IgnoreBuildable: true})
	case "rolling", "rolling-update":
		return e.rollingUp(ctx, op, svc, project)
	case "update": // pull new images, then recreate what changed
		if err := svc.Pull(ctx, project, api.PullOptions{IgnoreBuildable: true}); err != nil {
			return err
		}
		return svc.Up(ctx, project, api.UpOptions{Start: api.StartOptions{Project: project}})
	}
	return fmt.Errorf("unknown action %q", action)
}

// OpInfo is a copy of an operation's state, safe to pass around and encode.
type OpInfo struct {
	ID      string    `json:"id"`
	Stack   string    `json:"stack"`
	Action  string    `json:"action"`
	Started time.Time `json:"started"`
	Done    bool      `json:"done"`
	Error   string    `json:"error,omitempty"`
}

// Info returns the current state of the operation.
func (o *Op) Info() OpInfo {
	o.mu.Lock()
	defer o.mu.Unlock()
	return OpInfo{ID: o.ID, Stack: o.Stack, Action: o.Action, Started: o.Started, Done: o.Done, Error: o.Error}
}
