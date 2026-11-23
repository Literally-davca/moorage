// Package host abstracts a Docker host: the local engine or a remote agent
// reached through the hub tunnel. The HTTP API works only with this interface.
package host

import (
	"context"
	"errors"
	"net/http"

	"moorage/internal/engine"
)

// OpEvent is a batch of output from a running compose operation.
type OpEvent struct {
	Lines []string `json:"lines"`
	Done  bool     `json:"done"`
	Error string   `json:"error,omitempty"`
}

// Host is everything the hub can do with one Docker host.
type Host interface {
	Info(ctx context.Context) engine.HostInfo
	ListStacks(ctx context.Context) ([]engine.StackSummary, error)
	GetStack(ctx context.Context, name string) (engine.Stack, error)
	Stats(ctx context.Context, name string) ([]engine.Stat, error)
	Validate(ctx context.Context, name, compose, env string) error
	Save(ctx context.Context, name, compose, env, author string, create bool) error
	Delete(ctx context.Context, name, author string) error
	RunAction(ctx context.Context, name, action, author string) (engine.OpInfo, error)
	// FollowOp emits output batches of an operation until it is done.
	FollowOp(ctx context.Context, id string, emit func(OpEvent) error) error
	StreamLogs(ctx context.Context, stack, container string, tail int, emit func(engine.LogLine)) error
	// WatchLogs follows only new log lines that match f, without replaying history.
	WatchLogs(ctx context.Context, stack, container string, f engine.LogFilter, emit func(engine.LogLine)) error
	// TailLogs returns the last n lines of a stack's containers without following.
	TailLogs(ctx context.Context, stack string, n int) ([]engine.LogLine, error)
	History(ctx context.Context, name string) ([]engine.Version, error)
	Version(ctx context.Context, name, hash string) (engine.VersionDetail, error)
	Restore(ctx context.Context, name, hash, author string) (*engine.Version, error)
	CheckImages(ctx context.Context, stack string) ([]engine.ServiceImage, error)
	StackImages(ctx context.Context, stack string) (map[string]engine.RunningImage, error)
	RollbackImages(ctx context.Context, stack string, targets map[string]engine.RollbackTarget, author string) (engine.OpInfo, error)
	ChangeImage(ctx context.Context, stack, service, oldRef, newRef, author string) (*engine.Version, error)
	Terminal(ctx context.Context, stack, container, shell string, cols, rows int) (engine.TerminalSession, engine.Container, error)
}

// DepFactsSource is implemented by hosts that can report the compose wiring between their
// stacks (Local and Remote). It is a separate interface so test fakes and older code paths
// don't have to implement it; a host without it simply yields no detected dependencies.
type DepFactsSource interface {
	DepFacts(ctx context.Context) (engine.DepFacts, error)
}

// NotedCreator is implemented by hosts that can create a stack with a message for its
// first version (Local and Remote), e.g. the template it was created from. An older
// agent ignores the note and records the plain "Create stack".
type NotedCreator interface {
	CreateNoted(ctx context.Context, name, compose, env, author, note string) error
}

// SelfApplier is implemented by hosts that can apply changes to the stack Moorage itself
// runs from (Local and Remote): save, then a helper container runs compose up and rolls
// back if Moorage doesn't come back. An older agent answers 404 (IsUnsupported).
type SelfApplier interface {
	SelfApply(ctx context.Context, name, compose, env, author string) error
}

// DeletedStacksSource is implemented by hosts that can list deleted stacks in history.git.
type DeletedStacksSource interface {
	DeletedStacks(ctx context.Context) ([]engine.DeletedStack, error)
}

// DiskPruner is implemented by hosts that support disk cleanup preview and execution.
type DiskPruner interface {
	PrunePreview(ctx context.Context) (engine.PrunePreview, error)
	PruneDisk(ctx context.Context, req engine.PruneRequest) (engine.PruneResult, error)
}

// Errors specific to hosts (engine errors pass through unchanged).
var (
	ErrOffline               = errors.New("this host is offline")
	ErrHostReadOnly          = errors.New("this host's agent runs read-only, changes are disabled")
	ErrTerminalDisabled      = errors.New("the web terminal is disabled on this host")
	ErrUnknownHost           = errors.New("host not found")
	ErrOpNotFound            = errors.New("operation not found")
	ErrProbesDisabled        = errors.New("probing from this agent is turned off (MOORAGE_PROBES=false)")
	ErrSelfUpdateDisabled    = errors.New("self-update is disabled on this host (MOORAGE_SELF_UPDATE=false)")
	ErrSelfUpdateBusy        = errors.New("a self-update is already running on this host")
	ErrSelfUpdateInvalid     = errors.New("invalid or unverified self-update")
	ErrSelfUpdateUnsupported = errors.New("self-update is not supported in this runtime mode")
)

// Error codes carry sentinel errors across the agent tunnel.
var codes = []struct {
	code   string
	err    error
	status int
}{
	{"not_found", engine.ErrNotFound, http.StatusNotFound},
	{"read_only", engine.ErrReadOnly, http.StatusForbidden},
	{"busy", engine.ErrBusy, http.StatusConflict},
	{"exists", engine.ErrExists, http.StatusConflict},
	{"has_running", engine.ErrHasRunning, http.StatusConflict},
	{"invalid_name", engine.ErrInvalidName, http.StatusBadRequest},
	{"no_history", engine.ErrNoHistory, http.StatusNotFound},
	{"history_off", engine.ErrHistoryOff, http.StatusServiceUnavailable},
	{"host_read_only", ErrHostReadOnly, http.StatusForbidden},
	{"op_not_found", ErrOpNotFound, http.StatusNotFound},
	{"offline", ErrOffline, http.StatusServiceUnavailable},
	{"manual_edit", engine.ErrManualEdit, http.StatusUnprocessableEntity},
	{"terminal_disabled", ErrTerminalDisabled, http.StatusForbidden},
	{"invalid_shell", engine.ErrInvalidShell, http.StatusBadRequest},
	{"not_running", engine.ErrContainerNotRunning, http.StatusConflict},
	{"not_in_stack", engine.ErrContainerNotInStack, http.StatusNotFound},
	{"probes_disabled", ErrProbesDisabled, http.StatusForbidden},
	{"self_stack", engine.ErrSelfStack, http.StatusConflict},
	{"self_update_disabled", ErrSelfUpdateDisabled, http.StatusForbidden},
	{"self_update_busy", ErrSelfUpdateBusy, http.StatusConflict},
	{"self_update_invalid", ErrSelfUpdateInvalid, http.StatusBadRequest},
	{"self_update_unsupported", ErrSelfUpdateUnsupported, http.StatusUnprocessableEntity},
}

// ErrorCode returns the wire code and HTTP status for err.
func ErrorCode(err error) (string, int) {
	var ve engine.ValidationError
	if errors.As(err, &ve) {
		return "validation", http.StatusUnprocessableEntity
	}
	for _, c := range codes {
		if errors.Is(err, c.err) {
			return c.code, c.status
		}
	}
	return "internal", http.StatusBadGateway
}

// FromCode rebuilds an error received from an agent.
func FromCode(code, msg string) error {
	if code == "validation" {
		return engine.ValidationError{Msg: msg}
	}
	for _, c := range codes {
		if c.code == code {
			if msg != "" && msg != c.err.Error() && isSelfUpdateCode(code) {
				return codedError{sentinel: c.err, msg: msg}
			}
			return c.err
		}
	}
	return remoteError(msg)
}

func isSelfUpdateCode(code string) bool {
	switch code {
	case "self_update_disabled", "self_update_busy", "self_update_invalid", "self_update_unsupported":
		return true
	}
	return false
}

type codedError struct {
	sentinel error
	msg      string
}

func (e codedError) Error() string { return e.msg }
func (e codedError) Unwrap() error { return e.sentinel }

type remoteError string

func (e remoteError) Error() string { return string(e) }
