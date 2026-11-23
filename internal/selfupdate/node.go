package selfupdate

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"moorage/internal/host"
)

// NodeUpdater implements host.AgentSelfUpdater on a Moorage node (hub, all-in-one, or agent).
// It enforces MOORAGE_SELF_UPDATE, verifies Ed25519 signatures and checksums locally, and
// coordinates binary re-exec or container helper launches.
type NodeUpdater struct {
	DataDir        string
	CurrentVersion string
	SelfUpdate     bool
	IsHub          bool
	ListenAddr     string
	Eng            ContainerEngine // non-nil when engine socket is available
	PubKey         ed25519.PublicKey
	ShutdownFn     func() // triggers graceful shutdown so binary re-exec can proceed
	Log            *slog.Logger

	// Test hooks.
	GOOS, GOARCH string
	Runtime      *RuntimeEnv
	ExecFn       func(path string, argv, envv []string) error
	SpawnWatchFn func(prevPath, statePath string) error
	ShutdownWait time.Duration // delay before invoking ShutdownFn so HTTP 202 flushes; default 250ms

	mu          sync.Mutex
	busy        bool
	pendingExec string
	healthyOnce sync.Once
}

func (n *NodeUpdater) shutdownDelay() time.Duration {
	if n.ShutdownWait > 0 {
		return n.ShutdownWait
	}
	return 250 * time.Millisecond
}

// Status inspects runtime mode, writability/socket availability, MOORAGE_SELF_UPDATE,
// kept rollback artifacts, and any recent automatic rollback note.
func (n *NodeUpdater) Status(ctx context.Context) (host.SelfUpdateStatus, error) {
	re := n.runtimeEnv()
	return DetectStatus(ctx, n.Eng, n.DataDir, n.CurrentVersion, n.SelfUpdate, &re), nil
}

func (n *NodeUpdater) runtimeEnv() RuntimeEnv {
	var re RuntimeEnv
	if n.Runtime != nil {
		re = *n.Runtime
	} else {
		re = defaultRuntimeEnv(n.SelfUpdate)
	}
	re.SelfUpdateFlag = n.SelfUpdate
	if n.GOOS != "" {
		re.GOOS = n.GOOS
	}
	if n.GOARCH != "" {
		re.GOARCH = n.GOARCH
	}
	return re
}

// ConfirmHealthy marks the running version healthy in <DataDir>/self-update-healthy.json
// once the agent serves /info or hub starts cleanly.
func (n *NodeUpdater) ConfirmHealthy(ver string) {
	if n == nil || n.DataDir == "" {
		return
	}
	if ver == "" {
		ver = n.CurrentVersion
	}
	n.healthyOnce.Do(func() {
		writeHealthyMark(n.DataDir, HealthyMark{Version: ver, At: time.Now()})
		// An agent that answers the hub over the tunnel is healthy: confirm a pending
		// binary update itself, so a later crash (with the watcher gone, e.g. under
		// systemd) no longer rolls it back. The hub confirms after serving for a while
		// (Service.confirmHubJob) instead.
		if !n.IsHub {
			FinalizePending(n.DataDir, ver, time.Now())
		}
	})
}

func (n *NodeUpdater) acquireBusy() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.busy {
		return false
	}
	n.busy = true
	return true
}

func (n *NodeUpdater) releaseBusy() {
	n.mu.Lock()
	n.busy = false
	n.mu.Unlock()
}

// ApplyBinary enforces MOORAGE_SELF_UPDATE, verifies Ed25519 signature + SHA-256 of bin,
// saves moorage.prev, swaps atomically, spawns the rollback watcher, and schedules restart.
func (n *NodeUpdater) ApplyBinary(ctx context.Context, m host.SelfUpdateManifest, bin io.Reader) error {
	if !n.SelfUpdate {
		return host.ErrSelfUpdateDisabled
	}
	if !n.acquireBusy() {
		return host.ErrSelfUpdateBusy
	}
	st := DetectStatus(ctx, n.Eng, n.DataDir, n.CurrentVersion, n.SelfUpdate, ptrRuntime(n.runtimeEnv()))
	if st.Mode != "binary" {
		n.releaseBusy()
		return fmt.Errorf("%w: node runs in %s mode, not binary mode", host.ErrSelfUpdateUnsupported, st.Mode)
	}
	if !st.Supported {
		n.releaseBusy()
		return fmt.Errorf("%w: %s", host.ErrSelfUpdateUnsupported, st.Reason)
	}

	applier := &BinaryApplier{
		DataDir:        n.DataDir,
		CurrentVersion: n.CurrentVersion,
		IsHub:          n.IsHub,
		ListenAddr:     n.ListenAddr,
		PubKey:         n.PubKey,
		GOOS:           n.goos(),
		GOARCH:         n.goarch(),
		Log:            n.Log,
		ExecFn:         n.ExecFn,
		SpawnWatchFn:   n.SpawnWatchFn,
	}
	if n.Runtime != nil && n.Runtime.Executable != nil {
		if p, err := n.Runtime.Executable(); err == nil {
			applier.ExePath = p
		}
	}

	exePath, err := applier.StageAndSwap(m, bin)
	if err != nil {
		n.releaseBusy()
		return err
	}

	n.mu.Lock()
	n.pendingExec = exePath
	n.mu.Unlock()

	if n.Log != nil {
		n.Log.Info("self-update binary staged, restarting", "from", n.CurrentVersion, "to", m.Version)
	}
	if n.ShutdownFn != nil {
		delay := n.shutdownDelay()
		go func() {
			time.Sleep(delay)
			n.ShutdownFn()
		}()
	}
	return nil
}

// ApplyContainer enforces MOORAGE_SELF_UPDATE, verifies the signed image digest against
// its own embedded Ed25519 key, pulls the image if needed, and starts the helper container.
func (n *NodeUpdater) ApplyContainer(ctx context.Context, m host.SelfUpdateManifest) error {
	if !n.SelfUpdate {
		return host.ErrSelfUpdateDisabled
	}
	if !n.acquireBusy() {
		return host.ErrSelfUpdateBusy
	}
	st := DetectStatus(ctx, n.Eng, n.DataDir, n.CurrentVersion, n.SelfUpdate, ptrRuntime(n.runtimeEnv()))
	if st.Mode != "container" {
		n.releaseBusy()
		return fmt.Errorf("%w: node runs in %s mode, not container mode", host.ErrSelfUpdateUnsupported, st.Mode)
	}
	if !st.Supported {
		n.releaseBusy()
		return fmt.Errorf("%w: %s", host.ErrSelfUpdateUnsupported, st.Reason)
	}

	ca := &ContainerApplier{
		Eng:            n.Eng,
		DataDir:        n.DataDir,
		CurrentVersion: n.CurrentVersion,
		IsHub:          n.IsHub,
		ListenAddr:     n.ListenAddr,
		PubKey:         n.PubKey,
		GOOS:           n.goos(),
		GOARCH:         n.goarch(),
		Runtime:        ptrRuntime(n.runtimeEnv()),
	}
	if err := ca.PrepareAndLaunchHelper(ctx, m); err != nil {
		n.releaseBusy()
		return err
	}
	// Release busy after 3 min in case an external helper crashed before stopping this container.
	go func() {
		time.Sleep(3 * time.Minute)
		n.releaseBusy()
	}()
	return nil
}

// Rollback restores the kept previous binary or container when MOORAGE_SELF_UPDATE allows it.
func (n *NodeUpdater) Rollback(ctx context.Context) error {
	if !n.SelfUpdate {
		return host.ErrSelfUpdateDisabled
	}
	if !n.acquireBusy() {
		return host.ErrSelfUpdateBusy
	}
	st := DetectStatus(ctx, n.Eng, n.DataDir, n.CurrentVersion, n.SelfUpdate, ptrRuntime(n.runtimeEnv()))
	if !st.Supported {
		n.releaseBusy()
		return fmt.Errorf("%w: %s", host.ErrSelfUpdateUnsupported, st.Reason)
	}

	switch st.Mode {
	case "binary":
		applier := &BinaryApplier{
			DataDir:        n.DataDir,
			CurrentVersion: n.CurrentVersion,
			IsHub:          n.IsHub,
			ListenAddr:     n.ListenAddr,
			ExecFn:         n.ExecFn,
			Log:            n.Log,
		}
		if n.Runtime != nil && n.Runtime.Executable != nil {
			if p, err := n.Runtime.Executable(); err == nil {
				applier.ExePath = p
			}
		}
		exePath, err := applier.PrepareRollback()
		if err != nil {
			n.releaseBusy()
			return err
		}
		n.mu.Lock()
		n.pendingExec = exePath
		n.mu.Unlock()
		if n.Log != nil {
			n.Log.Info("self-update binary rollback staged, restarting", "from", n.CurrentVersion, "to", st.PrevVersion)
		}
		if n.ShutdownFn != nil {
			delay := n.shutdownDelay()
			go func() {
				time.Sleep(delay)
				n.ShutdownFn()
			}()
		}
		return nil

	case "container":
		ca := &ContainerApplier{
			Eng:            n.Eng,
			DataDir:        n.DataDir,
			CurrentVersion: n.CurrentVersion,
			IsHub:          n.IsHub,
			ListenAddr:     n.ListenAddr,
			Runtime:        ptrRuntime(n.runtimeEnv()),
		}
		if err := ca.LaunchRollbackHelper(ctx); err != nil {
			n.releaseBusy()
			return err
		}
		go func() {
			time.Sleep(3 * time.Minute)
			n.releaseBusy()
		}()
		return nil

	default:
		n.releaseBusy()
		return fmt.Errorf("%w: cannot roll back in mode %s", host.ErrSelfUpdateUnsupported, st.Mode)
	}
}

// PendingExec returns the staged binary path to exec after graceful shutdown, if any.
func (n *NodeUpdater) PendingExec() string {
	if n == nil {
		return ""
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.pendingExec
}

// ExecPending replaces the current process with the staged binary when one is queued.
func (n *NodeUpdater) ExecPending() error {
	p := n.PendingExec()
	if p == "" {
		return nil
	}
	applier := &BinaryApplier{ExecFn: n.ExecFn}
	return applier.ExecPending(p)
}

// StartCleanupLoop removes expired moorage.prev / -moorage-prev rollback artifacts after 24 h.
func (n *NodeUpdater) StartCleanupLoop(ctx context.Context) {
	if n == nil {
		return
	}
	CleanupExpiredPrev(ctx, n.Eng, n.DataDir, time.Now())
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				CleanupExpiredPrev(ctx, n.Eng, n.DataDir, time.Now())
			}
		}
	}()
}

func (n *NodeUpdater) goos() string {
	if n.GOOS != "" {
		return n.GOOS
	}
	if n.Runtime != nil && n.Runtime.GOOS != "" {
		return n.Runtime.GOOS
	}
	return ""
}

func (n *NodeUpdater) goarch() string {
	if n.GOARCH != "" {
		return n.GOARCH
	}
	if n.Runtime != nil && n.Runtime.GOARCH != "" {
		return n.Runtime.GOARCH
	}
	return ""
}

func ptrRuntime(re RuntimeEnv) *RuntimeEnv {
	return &re
}
