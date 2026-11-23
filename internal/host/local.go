package host

import (
	"context"
	"io"

	"moorage/internal/engine"
)

// Local runs everything on the Docker daemon of this process.
type Local struct {
	E        *engine.Engine
	ReadOnly bool
	AllowTTY bool
}

func (l Local) Info(ctx context.Context) engine.HostInfo { return l.E.Host(ctx) }

func (l Local) ListStacks(ctx context.Context) ([]engine.StackSummary, error) {
	return l.E.ListStacks(ctx)
}

func (l Local) GetStack(ctx context.Context, name string) (engine.Stack, error) {
	return l.E.GetStack(ctx, name)
}

func (l Local) Stats(ctx context.Context, name string) ([]engine.Stat, error) {
	return l.E.Stats(ctx, name)
}

func (l Local) Validate(ctx context.Context, name, compose, env string) error {
	return l.E.ValidateCompose(ctx, name, compose, env)
}

func (l Local) Save(ctx context.Context, name, compose, env, author string, create bool) error {
	return l.E.SaveStack(ctx, name, compose, env, author, create)
}

// CreateNoted creates a stack whose first version message is note.
func (l Local) CreateNoted(ctx context.Context, name, compose, env, author, note string) error {
	return l.E.CreateStackNoted(ctx, name, compose, env, author, note)
}

// SelfApply saves and applies the stack Moorage runs from through the helper container.
func (l Local) SelfApply(ctx context.Context, name, compose, env, author string) error {
	return l.E.SelfApply(ctx, name, compose, env, author)
}

func (l Local) ExportHistory(_ context.Context, w io.Writer) error {
	return l.E.ExportHistory(w)
}

func (l Local) DeletedStacks(_ context.Context) ([]engine.DeletedStack, error) {
	return l.E.DeletedStacks()
}

func (l Local) PrunePreview(ctx context.Context) (engine.PrunePreview, error) {
	return l.E.PrunePreview(ctx)
}

func (l Local) PruneDisk(ctx context.Context, req engine.PruneRequest) (engine.PruneResult, error) {
	if l.ReadOnly {
		return engine.PruneResult{}, ErrHostReadOnly
	}
	return l.E.PruneDisk(ctx, req)
}

func (l Local) Delete(ctx context.Context, name, author string) error {
	return l.E.DeleteStack(ctx, name, author)
}

func (l Local) RunAction(_ context.Context, name, action, _ string) (engine.OpInfo, error) {
	op, err := l.E.RunAction(name, action)
	if err != nil {
		return engine.OpInfo{}, err
	}
	return op.Info(), nil
}

func (l Local) FollowOp(ctx context.Context, id string, emit func(OpEvent) error) error {
	op, ok := l.E.Op(id)
	if !ok {
		return ErrOpNotFound
	}
	ch, unsub := op.Subscribe()
	defer unsub()
	sent := 0
	for {
		lines, done, errMsg := op.Snapshot(sent)
		if len(lines) > 0 || done {
			sent += len(lines)
			if err := emit(OpEvent{Lines: lines, Done: done, Error: errMsg}); err != nil {
				return err
			}
			if done {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

func (l Local) StreamLogs(ctx context.Context, stack, container string, tail int, emit func(engine.LogLine)) error {
	return l.E.StreamLogs(ctx, stack, container, tail, emit)
}

func (l Local) WatchLogs(ctx context.Context, stack, container string, f engine.LogFilter, emit func(engine.LogLine)) error {
	return l.E.WatchLogs(ctx, stack, container, f, emit)
}

func (l Local) History(_ context.Context, name string) ([]engine.Version, error) {
	return l.E.StackHistory(name, 200)
}

func (l Local) Version(_ context.Context, name, hash string) (engine.VersionDetail, error) {
	return l.E.StackVersion(name, hash)
}

func (l Local) Restore(ctx context.Context, name, hash, author string) (*engine.Version, error) {
	return l.E.RestoreStack(ctx, name, hash, author)
}

func (l Local) TailLogs(ctx context.Context, stack string, n int) ([]engine.LogLine, error) {
	return l.E.TailLogs(ctx, stack, n)
}

func (l Local) CheckImages(ctx context.Context, stack string) ([]engine.ServiceImage, error) {
	return l.E.CheckImages(ctx, stack)
}

func (l Local) StackImages(ctx context.Context, stack string) (map[string]engine.RunningImage, error) {
	return l.E.StackImages(ctx, stack)
}

func (l Local) RollbackImages(ctx context.Context, stack string, targets map[string]engine.RollbackTarget, author string) (engine.OpInfo, error) {
	return l.E.RollbackImages(ctx, stack, targets, author)
}

func (l Local) ChangeImage(ctx context.Context, stack, service, oldRef, newRef, author string) (*engine.Version, error) {
	return l.E.ChangeServiceImage(ctx, stack, service, oldRef, newRef, author)
}

func (l Local) Terminal(ctx context.Context, stack, container, shell string, cols, rows int) (engine.TerminalSession, engine.Container, error) {
	if l.ReadOnly {
		return nil, engine.Container{}, ErrHostReadOnly
	}
	if !l.AllowTTY {
		return nil, engine.Container{}, ErrTerminalDisabled
	}
	return l.E.Terminal(ctx, stack, container, shell, cols, rows)
}

// DepFacts reads the dependency facts of the local stacks.
func (l Local) DepFacts(ctx context.Context) (engine.DepFacts, error) {
	return l.E.DepFacts(ctx)
}
