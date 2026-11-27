package selfupdate

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"moorage/internal/config"
	"moorage/internal/host"
	"moorage/internal/monitor"
	"moorage/internal/store"
	"moorage/internal/tunnel"
)

// AuditFunc writes background self-update completion/failure rows into Moorage's audit log.
type AuditFunc func(ctx context.Context, entry store.AuditEntry)

// RemoteAgentClient abstracts the tunnel Remote calls used by Service (implemented by *host.Remote).
type RemoteAgentClient interface {
	SelfUpdateStatus(ctx context.Context) (host.SelfUpdateStatus, error)
}

// Service orchestrates Moorage release checks, node status inspection, self-updates, and
// rollbacks across the hub and all paired agents.
type Service struct {
	Cfg     config.Config
	Version string
	IsAllIn bool // true in all-in-one mode, false in hub mode
	St      *store.Store
	Reg     *host.Registry
	Mon     *monitor.Monitor
	Hub     *tunnel.Hub
	Local   *NodeUpdater
	Checker *Checker
	PubKey  ed25519.PublicKey
	Audit   AuditFunc
	Log     *slog.Logger

	HealthTimeout time.Duration // defaults to 120s
	Now           func() time.Time

	// AgentWait is how long the hub waits for an updated agent to come back. It is longer
	// than the agent's own 120 s health window so an agent-side rollback is seen.
	AgentWait time.Duration // default 180 s
	// HubRestartTimeout fails a hub job still "restarting" while this (old) process
	// lives on, e.g. when the update helper died before stopping the hub.
	HubRestartTimeout time.Duration // default 5 min
	// HubConfirmAfter is how long a restarted hub must serve HTTP before it confirms its
	// own update when no watcher/helper confirmed it first.
	HubConfirmAfter time.Duration // default 30 s
	// AgentReconcileGrace is how long open agent jobs found after a hub restart wait for
	// their agent to reconnect before they count as failed.
	AgentReconcileGrace time.Duration // default 3 min
	// StatusTTL caches node self-update status for the overview.
	StatusTTL time.Duration // default 30 s

	mu            sync.Mutex
	running       map[string]int64         // nodeID -> SQLite job id
	waiters       map[string][]chan string // agentID -> reconnect version channels
	hubMaintUntil time.Time
	statusCache   map[string]cachedStatus

	serving     chan struct{}
	servingOnce sync.Once
}

type cachedStatus struct {
	st  host.SelfUpdateStatus
	err error
	at  time.Time
}

const (
	defaultAgentWait         = 180 * time.Second
	defaultHubRestartTimeout = 5 * time.Minute
	defaultHubConfirmAfter   = 30 * time.Second
	defaultAgentGrace        = 3 * time.Minute
	defaultStatusTTL         = 30 * time.Second
	hostSilence              = 3 * time.Minute
)

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// NewService constructs the hub self-update coordinator.
func NewService(cfg config.Config, version string, isAllIn bool, st *store.Store, reg *host.Registry, mon *monitor.Monitor, hub *tunnel.Hub, local *NodeUpdater, audit AuditFunc, log *slog.Logger) *Service {
	pub, _ := DefaultPublicKey()
	return &Service{
		Cfg:           cfg,
		Version:       version,
		IsAllIn:       isAllIn,
		St:            st,
		Reg:           reg,
		Mon:           mon,
		Hub:           hub,
		Local:         local,
		Checker:       NewChecker(cfg.UpdateURL, cfg.UpdateChannel, version, pub),
		PubKey:        pub,
		Audit:         audit,
		Log:           log,
		HealthTimeout: healthTimeout,
		Now:           time.Now,
		running:       map[string]int64{},
		waiters:       map[string][]chan string{},
		statusCache:   map[string]cachedStatus{},
		serving:       make(chan struct{}),
	}
}

// MarkServing tells the service that this hub process now serves HTTP. A hub update is
// only confirmed after that (see confirmHubJob).
func (s *Service) MarkServing() {
	s.servingOnce.Do(func() { close(s.serving) })
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) healthTimeout() time.Duration {
	if s.HealthTimeout > 0 {
		return s.HealthTimeout
	}
	return healthTimeout
}

// Start reconciles any interrupted SQLite update jobs from before a hub restart, starts
// the daily release checker, and starts 24 h cleanup of kept previous versions.
func (s *Service) Start(ctx context.Context) {
	if s.Local != nil {
		s.Local.StartCleanupLoop(ctx)
		go func() {
			select {
			case <-s.serving:
				s.Local.ConfirmHealthy(s.Version)
			case <-ctx.Done():
			}
		}()
	}
	s.ReconcileAfterRestart(ctx)
	go s.Checker.RunPeriodic(ctx)
}

// ReconcileAfterRestart finishes open system_updates rows left running/restarting when the
// hub process restarted, recording the outcome and audit trail. Outcomes that need the
// new hub to serve, or an agent to reconnect, are settled in the background; their node
// stays locked meanwhile so no second update starts.
func (s *Service) ReconcileAfterRestart(ctx context.Context) {
	if s.St == nil {
		return
	}
	open, err := s.St.OpenSystemUpdates(ctx)
	if err != nil {
		if s.Log != nil {
			s.Log.Warn("reconcile system updates", "err", err)
		}
		return
	}
	now := s.now()
	for _, job := range open {
		if job.NodeID != "hub" {
			s.lockNode(job.NodeID, job.ID)
			go s.resolveAgentJobAfterRestart(ctx, job)
			continue
		}
		note, hasNote := readRollbackNote(s.Cfg.DataDir)
		switch {
		case normVersion(s.Version) != "" && normVersion(s.Version) == normVersion(job.ToVersion):
			// Running the target version is not yet success: the watcher/helper (or
			// BootCheck) may still roll it back. Confirm once it serves.
			s.lockNode("hub", job.ID)
			go s.confirmHubJob(ctx, job)
		case hasNote:
			_ = s.St.UpdateSystemUpdate(ctx, job.ID, store.SysUpdateRolledBack, note.Reason, now)
			clearRollbackNote(s.Cfg.DataDir)
			s.emitAudit(ctx, job, "update.rolled_back", store.AuditFailed)
		default:
			msg := fmt.Sprintf("Hub restarted at %s before reaching %s", s.Version, job.ToVersion)
			_ = s.St.UpdateSystemUpdate(ctx, job.ID, store.SysUpdateFailed, msg, now)
			s.emitAudit(ctx, job, "update.failed", store.AuditFailed)
		}
	}
}

func (s *Service) lockNode(nodeID string, jobID int64) {
	s.mu.Lock()
	s.running[nodeID] = jobID
	s.mu.Unlock()
}

// confirmHubJob marks the restarted hub's open job done once this process serves HTTP
// and the update is confirmed: the binary watcher removed the pending state, the
// container helper recorded the kept previous container, or the hub has served for
// HubConfirmAfter (container: the helper's whole health window) without being rolled
// back. If a rollback happens first this process dies, and the restored old hub finds
// the job still open plus the rollback note (update.rolled_back).
func (s *Service) confirmHubJob(ctx context.Context, job store.SystemUpdate) {
	defer s.clearLock("hub")
	select {
	case <-s.serving:
	case <-ctx.Done():
		return
	}
	fallback := orDefault(s.HubConfirmAfter, defaultHubConfirmAfter)
	if job.Mode == "container" && job.Action == store.SysUpdateActionApply {
		fallback = s.healthTimeout() + fallback
	}
	poll := minDuration(time.Second, fallback/4+time.Millisecond)
	servingSince := time.Now()
	for {
		if s.hubConfirmed(job) || time.Since(servingSince) >= fallback {
			FinalizePending(s.Cfg.DataDir, s.Version, s.now())
			verb := "Updated to " + job.ToVersion
			if job.Action == store.SysUpdateActionRollback {
				verb = "Rolled back to " + job.ToVersion
			}
			s.finishJob(context.WithoutCancel(ctx), job, store.SysUpdateDone, verb, "update.done", store.AuditOK)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
	}
}

func (s *Service) hubConfirmed(job store.SystemUpdate) bool {
	if job.Action == store.SysUpdateActionRollback {
		return true
	}
	if PendingExists(s.Cfg.DataDir) {
		return false
	}
	if job.Mode == "container" {
		rec, ok := readPrevRecord(s.Cfg.DataDir)
		return ok && normVersion(rec.ToVersion) == normVersion(job.ToVersion) && !rec.SavedAt.Before(job.StartedAt)
	}
	return true
}

// resolveAgentJobAfterRestart settles an agent job left open by a hub restart. The agent
// may still be restarting or rolling back, so it gets AgentReconcileGrace to reconnect
// before the job counts as failed.
func (s *Service) resolveAgentJobAfterRestart(ctx context.Context, job store.SystemUpdate) {
	defer s.clearLock(job.NodeID)
	waitCh, cancelWait := s.subscribeReconnect(job.NodeID)
	defer cancelWait()
	deadline := time.NewTimer(orDefault(s.AgentReconcileGrace, defaultAgentGrace))
	defer deadline.Stop()
	poll := time.NewTicker(5 * time.Second)
	defer poll.Stop()
	bg := context.WithoutCancel(ctx)
	for {
		if status, msg, ok := s.agentJobOutcome(ctx, job); ok {
			s.finishJobStatus(bg, job, status, msg)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			if status, msg, ok := s.agentJobOutcome(ctx, job); ok {
				s.finishJobStatus(bg, job, status, msg)
				return
			}
			s.finishJob(bg, job, store.SysUpdateFailed, "Hub restarted during the update and the agent did not come back as "+job.ToVersion, "update.failed", store.AuditFailed)
			return
		case <-waitCh:
		case <-poll.C:
		}
	}
}

// agentJobOutcome reports a final outcome when the agent is connected and either runs
// the job's target version or reports an automatic rollback.
func (s *Service) agentJobOutcome(ctx context.Context, job store.SystemUpdate) (string, string, bool) {
	if s.Reg == nil {
		return "", "", false
	}
	rm, info, ok := s.Reg.Remote(job.NodeID)
	if !ok {
		return "", "", false
	}
	if normVersion(info.Version) == normVersion(job.ToVersion) {
		if job.Action == store.SysUpdateActionRollback {
			return store.SysUpdateDone, "Rolled back to " + job.ToVersion, true
		}
		return store.SysUpdateDone, "Updated to " + job.ToVersion, true
	}
	if job.Action == store.SysUpdateActionRollback {
		return "", "", false
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if st, err := rm.SelfUpdateStatus(qctx); err == nil && st.LastRollback != "" {
		return store.SysUpdateRolledBack, st.LastRollback, true
	}
	return "", "", false
}

// finishJobStatus finishes a job with the audit action matching its final status.
func (s *Service) finishJobStatus(ctx context.Context, job store.SystemUpdate, status, msg string) {
	switch status {
	case store.SysUpdateDone:
		s.finishJob(ctx, job, status, msg, "update.done", store.AuditOK)
	case store.SysUpdateRolledBack:
		s.finishJob(ctx, job, status, msg, "update.rolled_back", store.AuditFailed)
	default:
		s.finishJob(ctx, job, store.SysUpdateFailed, msg, "update.failed", store.AuditFailed)
	}
}

func (s *Service) emitAudit(ctx context.Context, job store.SystemUpdate, action, outcome string) {
	if s.Audit == nil {
		return
	}
	target := fmt.Sprintf("%s: %s -> %s", job.NodeID, job.FromVersion, job.ToVersion)
	var hostID string
	if job.NodeID != "hub" {
		hostID = job.NodeID
	}
	s.Audit(context.WithoutCancel(ctx), store.AuditEntry{
		Actor:   fallbackActor(job.Actor),
		IP:      job.IP,
		Action:  action,
		HostID:  hostID,
		Target:  target,
		Outcome: outcome,
	})
}

func fallbackActor(actor string) string {
	if actor == "" {
		return store.AuditActorSystem
	}
	return actor
}

// HubUpdateUntil extends the watchdog maintenance pause across a planned hub self-update so
// watching agents do not page when the hub stops to restart.
func (s *Service) HubUpdateUntil(now time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Before(s.hubMaintUntil) {
		return s.hubMaintUntil
	}
	return time.Time{}
}

// OnAgentConnect wakes any in-progress update watcher waiting for agentID to come back.
func (s *Service) OnAgentConnect(agentID string) {
	s.invalidateStatus(agentID)
	if s.Reg == nil {
		return
	}
	_, info, ok := s.Reg.Remote(agentID)
	if !ok {
		return
	}
	s.mu.Lock()
	chans := s.waiters[agentID]
	s.mu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- info.Version:
		default:
		}
	}
}

// ReleaseDTO is returned inside GET /api/system/updates.
type ReleaseDTO struct {
	Version     string            `json:"version"`
	Tag         string            `json:"tag"`
	PreRelease  bool              `json:"prerelease"`
	PublishedAt time.Time         `json:"publishedAt,omitzero"`
	HTMLURL     string            `json:"htmlUrl,omitempty"`
	Notes       string            `json:"notes,omitempty"`
	NotesFull   bool              `json:"notesTruncated,omitempty"`
	Image       string            `json:"image,omitempty"`
	IndexDigest string            `json:"indexDigest,omitempty"`
	MinHub      string            `json:"minHub,omitempty"`
	MinAgent    string            `json:"minAgent,omitempty"`
	Digests     map[string]string `json:"digests,omitempty"`
}

// JobDTO describes the active or most recent update/rollback job for one node.
type JobDTO struct {
	ID          int64     `json:"id"`
	NodeID      string    `json:"nodeId"`
	Action      string    `json:"action"` // apply | rollback
	FromVersion string    `json:"fromVersion"`
	ToVersion   string    `json:"toVersion"`
	Status      string    `json:"status"` // running | restarting | done | failed | rolled-back
	Message     string    `json:"message,omitempty"`
	StartedAt   time.Time `json:"startedAt,omitzero"`
	FinishedAt  time.Time `json:"finishedAt,omitzero"`
}

// NodeStatusDTO represents one row (hub or an agent) in GET /api/system/updates.
type NodeStatusDTO struct {
	ID             string    `json:"id"` // "hub" or agent ID
	Name           string    `json:"name"`
	Kind           string    `json:"kind"` // hub | all-in-one | agent
	Connected      bool      `json:"connected"`
	ReadOnly       bool      `json:"readOnly,omitempty"`
	Version        string    `json:"version"`
	Mode           string    `json:"mode"` // container | binary | unknown
	Reason         string    `json:"reason,omitempty"`
	Supported      bool      `json:"supported"`
	SelfUpdate     bool      `json:"selfUpdate"`
	OlderAgent     bool      `json:"olderAgent,omitempty"`
	ManualCommand  string    `json:"manualCommand,omitempty"`
	Platform       string    `json:"platform,omitempty"`
	ComposeProject string    `json:"composeProject,omitempty"`
	ContainerName  string    `json:"containerName,omitempty"`
	Image          string    `json:"image,omitempty"`
	Latest         string    `json:"latest,omitempty"`
	UpdateAvail    bool      `json:"updateAvailable"`
	CanUpdate      bool      `json:"canUpdate"`
	Compat         string    `json:"compatibility,omitempty"` // ok | update-hub-first | agent-too-old | dev-build
	CompatNote     string    `json:"compatNote,omitempty"`
	PrevVersion    string    `json:"prevVersion,omitempty"`
	PrevSavedAt    time.Time `json:"prevSavedAt,omitzero"`
	LastRollback   string    `json:"lastRollback,omitempty"`
	Job            *JobDTO   `json:"job,omitempty"`
}

// OverviewResponse is the payload of GET /api/system/updates.
type OverviewResponse struct {
	Channel     string          `json:"channel"`
	UpdateURL   string          `json:"updateUrl"`
	LastChecked time.Time       `json:"lastChecked,omitzero"`
	CheckError  string          `json:"checkError,omitempty"`
	Release     *ReleaseDTO     `json:"release,omitempty"`
	KeyReady    bool            `json:"keyReady"`
	Nodes       []NodeStatusDTO `json:"nodes"`
}

// CheckNow triggers an immediate release check and returns the updated OverviewResponse.
func (s *Service) CheckNow(ctx context.Context) (OverviewResponse, error) {
	_, err := s.Checker.Refresh(ctx)
	ov, ovErr := s.Overview(ctx)
	if ovErr != nil {
		return ov, ovErr
	}
	return ov, err
}

// Overview gathers self-update readiness, compatibility, and recent job state for the hub
// and every paired agent.
func (s *Service) Overview(ctx context.Context) (OverviewResponse, error) {
	cs := s.Checker.Cached()
	latestJobs := map[string]store.SystemUpdate{}
	if s.St != nil {
		if m, err := s.St.LatestSystemUpdateByNode(ctx); err == nil {
			latestJobs = m
		}
	}

	var relDTO *ReleaseDTO
	if cs.Release != nil {
		r := cs.Release
		relDTO = &ReleaseDTO{
			Version:     r.Version,
			Tag:         r.Tag,
			PreRelease:  r.PreRelease,
			PublishedAt: r.PublishedAt,
			HTMLURL:     r.HTMLURL,
			Notes:       r.Notes,
			NotesFull:   r.NotesFull,
			Image:       r.Image,
			IndexDigest: r.IndexDigest,
			MinHub:      r.MinHub,
			MinAgent:    r.MinAgent,
			Digests:     r.Digests,
		}
	}

	hubKind := "hub"
	if s.IsAllIn || (s.Reg != nil && s.Reg.HasLocal()) {
		hubKind = "all-in-one"
	}
	hubName := s.Cfg.PublicURL
	if hubName == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			hubName = h
		} else {
			hubName = "Moorage Hub"
		}
	}

	var nodes []NodeStatusDTO
	hubNode := NodeStatusDTO{
		ID:         "hub",
		Name:       hubName,
		Kind:       hubKind,
		Connected:  true,
		Version:    s.Version,
		Mode:       "unknown",
		SelfUpdate: s.Cfg.SelfUpdate,
	}
	if s.Local != nil {
		if st, err := s.cachedNodeStatus(ctx, "hub", s.Local.Status); err == nil {
			applySelfStatusToDTO(&hubNode, st)
		}
	}
	hubNode.Name = "Hub (" + hubName + ")"
	if s.IsAllIn || hubKind == "all-in-one" {
		hubNode.Name = "This server (" + hubKind + ")"
	}
	applyCompatToNode(&hubNode, s.Version, cs.Release, true)
	if j, ok := latestJobs["hub"]; ok {
		hubNode.Job = toJobDTO(j)
	}
	nodes = append(nodes, hubNode)

	if s.Reg != nil {
		list, err := s.Reg.List(ctx)
		if err != nil {
			return OverviewResponse{}, err
		}
		var agents []NodeStatusDTO
		var wg sync.WaitGroup
		for _, e := range list {
			if e.ID == host.LocalID {
				continue // already represented by hub/all-in-one row
			}
			agents = append(agents, NodeStatusDTO{})
			i := len(agents) - 1
			wg.Add(1)
			// One slow agent must not stall the page: agents are asked in parallel.
			go func() {
				defer wg.Done()
				agents[i] = s.agentNode(ctx, e, cs.Release)
			}()
		}
		wg.Wait()
		for i := range agents {
			if j, ok := latestJobs[agents[i].ID]; ok {
				agents[i].Job = toJobDTO(j)
			}
		}
		nodes = append(nodes, agents...)
	}

	_, keyErr := DefaultPublicKey()
	if keyErr != nil && len(s.PubKey) == ed25519.PublicKeySize {
		keyErr = nil
	}

	return OverviewResponse{
		Channel:     s.Checker.CurrentChannel(),
		UpdateURL:   s.Cfg.UpdateURL,
		LastChecked: cs.LastChecked,
		CheckError:  cs.Error,
		Release:     relDTO,
		KeyReady:    keyErr == nil,
		Nodes:       nodes,
	}, nil
}

func (s *Service) agentNode(ctx context.Context, e host.Entry, rel *ReleaseInfo) NodeStatusDTO {
	node := NodeStatusDTO{
		ID:        e.ID,
		Name:      e.Name,
		Kind:      "agent",
		Connected: e.Connected,
		ReadOnly:  e.ReadOnly,
		Version:   e.Version,
		Mode:      "unknown",
	}
	if !e.Connected {
		node.Reason = "Agent is offline"
	} else if rm, _, ok := s.Reg.Remote(e.ID); ok {
		st, err := s.cachedNodeStatus(ctx, e.ID, func(ctx context.Context) (host.SelfUpdateStatus, error) {
			qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return rm.SelfUpdateStatus(qctx)
		})
		switch {
		case err == nil:
			applySelfStatusToDTO(&node, st)
			// The registry knows the version the agent connected with right now; a cached
			// status may predate a reconnect.
			if e.Version != "" {
				node.Version = e.Version
			}
		case host.IsUnsupported(err):
			node.OlderAgent = true
			node.Mode = inferAgentMode(e)
			node.Reason = "Agent version does not support in-app self-updates"
			node.ManualCommand = buildManualCommand(node.Mode, node.Platform, rel, s.Cfg.UpdateURL)
		default:
			node.Reason = err.Error()
		}
	}
	applyCompatToNode(&node, s.Version, rel, false)
	return node
}

// cachedNodeStatus returns a node's self-update status, asking the node at most once
// per StatusTTL. Apply, rollback, job completion and agent reconnects invalidate it.
func (s *Service) cachedNodeStatus(ctx context.Context, id string, fetch func(context.Context) (host.SelfUpdateStatus, error)) (host.SelfUpdateStatus, error) {
	ttl := orDefault(s.StatusTTL, defaultStatusTTL)
	s.mu.Lock()
	c, ok := s.statusCache[id]
	s.mu.Unlock()
	if ok && time.Since(c.at) < ttl {
		return c.st, c.err
	}
	st, err := fetch(ctx)
	if ctx.Err() != nil {
		return st, err // a cancelled request says nothing about the node
	}
	s.mu.Lock()
	if s.statusCache == nil {
		s.statusCache = map[string]cachedStatus{}
	}
	s.statusCache[id] = cachedStatus{st: st, err: err, at: time.Now()}
	s.mu.Unlock()
	return st, err
}

func (s *Service) invalidateStatus(ids ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statusCache == nil {
		s.statusCache = map[string]cachedStatus{}
	}
	if len(ids) == 0 {
		clear(s.statusCache)
		return
	}
	for _, id := range ids {
		delete(s.statusCache, id)
	}
}

func applySelfStatusToDTO(node *NodeStatusDTO, st host.SelfUpdateStatus) {
	if st.Mode != "" {
		node.Mode = st.Mode
	}
	node.Reason = st.Reason
	node.Supported = st.Supported
	node.SelfUpdate = st.SelfUpdate
	node.Platform = st.Platform
	if st.Version != "" {
		node.Version = st.Version
	}
	node.PrevVersion = st.PrevVersion
	node.PrevSavedAt = st.PrevSavedAt
	node.ComposeProject = st.ComposeProject
	node.ContainerName = st.ContainerName
	node.Image = st.Image
	node.LastRollback = st.LastRollback
}

func applyCompatToNode(node *NodeStatusDTO, hubVersion string, rel *ReleaseInfo, isHub bool) {
	if rel == nil {
		return
	}
	node.Latest = rel.Version
	newer, err := IsNewer(rel.Version, node.Version)
	if err != nil {
		node.Compat = "dev-build"
		node.CompatNote = "Running custom/unversioned build (" + node.Version + ")"
		return
	}
	node.UpdateAvail = newer

	if !isHub && rel.MinAgent != "" {
		if ok, err := SatisfiesMin(node.Version, rel.MinAgent); err == nil && !ok {
			node.Compat = "agent-too-old"
			node.CompatNote = fmt.Sprintf("Agent %s is older than minimum %s required by %s", node.Version, rel.MinAgent, rel.Version)
		}
	}

	if !isHub && newer && rel.MinHub != "" {
		if ok, err := SatisfiesMin(hubVersion, rel.MinHub); err == nil && !ok {
			node.Compat = "update-hub-first"
			node.CompatNote = fmt.Sprintf("Release %s requires hub >= %s (update the hub first)", rel.Version, rel.MinHub)
			node.CanUpdate = false
			return
		}
	}

	if node.Compat == "" {
		node.Compat = "ok"
	}
	node.CanUpdate = newer && node.Connected && node.Supported && !node.OlderAgent && (isHub || !node.ReadOnly || node.Supported)
}

func inferAgentMode(e host.Entry) string {
	// Older agents don't report mode; suggest container command if engine/rootless looks like typical Docker agent.
	if e.DockerVersion != "" || e.Engine != "" {
		return "container"
	}
	return "binary"
}

const defaultImage = "ghcr.io/literally-davca/moorage"

// buildManualCommand returns the shell steps to update a node by hand (older agents).
// Binary: download the asset for the host's architecture (detected with uname when the
// agent doesn't report it) from the same release source the hub uses, check it against
// SHA256SUMS, swap it in and restart. Container: the compose file pins the image, so the
// tag must be changed there before `up -d`, or compose recreates the old version.
func buildManualCommand(mode, platform string, rel *ReleaseInfo, updateURL string) string {
	ver := "latest"
	if rel != nil && rel.Version != "" {
		ver = rel.Version
	}
	if mode == "binary" {
		base := releaseDownloadBase(rel, updateURL)
		asset := assetFromPlatform(platform)
		arch := ""
		if asset == "" {
			arch = `ARCH=$(uname -m | sed -e s/x86_64/amd64/ -e s/aarch64/arm64/ -e s/armv7l/armv7/) && `
			asset = "moorage-linux-$ARCH"
		}
		return arch + "cd \"$(mktemp -d)\" && " +
			"curl -fsSLO " + base + "/" + asset + " && curl -fsSLO " + base + "/SHA256SUMS && " +
			"sha256sum -c --ignore-missing SHA256SUMS && " +
			"sudo install -m 0755 " + asset + " /usr/local/bin/moorage.new && " +
			"sudo mv /usr/local/bin/moorage.new /usr/local/bin/moorage && " +
			"sudo systemctl restart moorage-agent"
	}
	image := defaultImage
	if rel != nil && rel.Image != "" {
		image = rel.Image
	}
	return fmt.Sprintf("# In your compose file set  image: %s:%s  then run:\ndocker compose pull && docker compose up -d", image, ver)
}

// releaseDownloadBase is the URL prefix of the release's assets: taken from the release
// index the hub fetched (so MOORAGE_UPDATE_URL mirrors are honoured), else derived from a
// GitHub API update URL, else the official repository.
func releaseDownloadBase(rel *ReleaseInfo, updateURL string) string {
	if rel != nil {
		for name, u := range rel.Assets {
			if strings.HasSuffix(u, "/"+name) {
				return strings.TrimSuffix(u, "/"+name)
			}
		}
	}
	repo := "https://github.com/Literally-davca/moorage"
	const ghAPI = "https://api.github.com/repos/"
	if rest, ok := strings.CutPrefix(updateURL, ghAPI); ok {
		parts := strings.Split(rest, "/")
		if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
			repo = "https://github.com/" + parts[0] + "/" + parts[1]
		}
	}
	switch {
	case rel != nil && rel.Tag != "":
		return repo + "/releases/download/" + rel.Tag
	case rel != nil && rel.Version != "":
		return repo + "/releases/download/v" + rel.Version
	default:
		return repo + "/releases/latest/download"
	}
}

func toJobDTO(j store.SystemUpdate) *JobDTO {
	return &JobDTO{
		ID:          j.ID,
		NodeID:      j.NodeID,
		Action:      j.Action,
		FromVersion: j.FromVersion,
		ToVersion:   j.ToVersion,
		Status:      j.Status,
		Message:     j.Message,
		StartedAt:   j.StartedAt,
		FinishedAt:  j.FinishedAt,
	}
}

// ValidationError marks a synchronous 4xx refusal before any background job begins.
type RequestError struct {
	Status int
	Msg    string
}

func (e RequestError) Error() string { return e.Msg }

// Apply validates the requested targets, enforces min_hub / MOORAGE_SELF_UPDATE / busy
// locks, persists job rows in SQLite, and executes the updates asynchronously.
// Returns jobs started plus a flag indicating whether the hub itself will restart.
func (s *Service) Apply(ctx context.Context, targets []string, actor, ip string) (jobs []JobDTO, includesHub bool, auditTarget string, err error) {
	if len(targets) == 0 {
		return nil, false, "", RequestError{Status: 400, Msg: "Select at least one node to update."}
	}

	ov, err := s.Overview(ctx)
	if err != nil {
		return nil, false, "", err
	}
	rel := s.Checker.Cached().Release
	if rel == nil {
		return nil, false, "", RequestError{Status: 409, Msg: "No signed release is loaded yet. Run Check now first."}
	}

	nodeMap := make(map[string]NodeStatusDTO, len(ov.Nodes))
	var allAgents []string
	for _, n := range ov.Nodes {
		nodeMap[n.ID] = n
		if n.ID != "hub" {
			allAgents = append(allAgents, n.ID)
		}
	}

	// Expand aliases ("all-agents", "all") cleanly without duplicates, running agents before hub.
	explicit := map[string]bool{}
	seen := map[string]bool{}
	var orderedIDs []string
	wantHub := false
	addNode := func(id string, isExplicit bool) error {
		if id == "hub" {
			wantHub = true
			if isExplicit {
				explicit["hub"] = true
			}
			return nil
		}
		if _, ok := nodeMap[id]; !ok {
			return RequestError{Status: 404, Msg: "Unknown host " + id + "."}
		}
		if isExplicit {
			explicit[id] = true
		}
		if !seen[id] {
			seen[id] = true
			orderedIDs = append(orderedIDs, id)
		}
		return nil
	}
	for _, t := range targets {
		switch t {
		case "all-agents":
			for _, id := range allAgents {
				_ = addNode(id, false)
			}
		case "all":
			for _, id := range allAgents {
				_ = addNode(id, false)
			}
			wantHub = true
		default:
			if err := addNode(t, true); err != nil {
				return nil, false, "", err
			}
		}
	}
	if wantHub {
		orderedIDs = append(orderedIDs, "hub")
		seen["hub"] = true
	}
	if len(orderedIDs) == 0 {
		return nil, false, "", RequestError{Status: 400, Msg: "No matching nodes to update."}
	}

	// Pre-flight check on every target BEFORE starting or auditing any work.
	// Explicit targets fail fast with their exact reason; bulk targets ("all"/"all-agents")
	// skip already up-to-date or non-participating nodes while still enforcing min_hub.
	var eligible []string
	var firstSkipStatus int
	var firstSkipMsg string
	noteSkip := func(status int, msg string) {
		if firstSkipStatus == 0 {
			firstSkipStatus = status
			firstSkipMsg = msg
		}
	}
	for _, id := range orderedIDs {
		n := nodeMap[id]
		isExplicit := explicit[id]
		if !n.Connected {
			if isExplicit {
				return nil, false, "", RequestError{Status: 503, Msg: n.Name + " is offline."}
			}
			noteSkip(503, n.Name+" is offline.")
			continue
		}
		if n.OlderAgent {
			if isExplicit {
				return nil, false, "", RequestError{Status: 422, Msg: n.Name + " runs an older agent without remote self-update. Update it manually."}
			}
			noteSkip(422, n.Name+" runs an older agent without remote self-update.")
			continue
		}
		if !n.SelfUpdate {
			if isExplicit {
				return nil, false, "", RequestError{Status: 403, Msg: n.Name + " is set to manual updates only (MOORAGE_SELF_UPDATE=false). Update it on the host: docker compose pull && docker compose up -d."}
			}
			noteSkip(403, n.Name+" is set to manual updates only.")
			continue
		}
		if !n.Supported {
			reason := n.Reason
			if reason == "" {
				reason = "self-update is not supported on this host"
			}
			if isExplicit {
				return nil, false, "", RequestError{Status: 422, Msg: n.Name + ": " + reason + "."}
			}
			noteSkip(422, n.Name+": "+reason+".")
			continue
		}
		if id != "hub" && n.Compat == "update-hub-first" {
			// Refused for bulk targets too, hub included: agents run before the hub, and an
			// agent on a release the hub doesn't support yet would be cut off.
			return nil, false, "", RequestError{Status: 409, Msg: fmt.Sprintf(
				"%s can't be updated yet: release %s needs hub %s or newer (this hub runs %s), so update the hub first, then the agents.",
				n.Name, rel.Version, rel.MinHub, s.Version)}
		}
		if !n.UpdateAvail {
			if isExplicit {
				return nil, false, "", RequestError{Status: 409, Msg: fmt.Sprintf("%s is already on %s (or newer).", n.Name, n.Version)}
			}
			noteSkip(409, fmt.Sprintf("All selected nodes are already on %s.", n.Version))
			continue
		}
		eligible = append(eligible, id)
	}
	if len(eligible) == 0 {
		if firstSkipStatus != 0 {
			return nil, false, "", RequestError{Status: firstSkipStatus, Msg: firstSkipMsg}
		}
		return nil, false, "", RequestError{Status: 409, Msg: "No eligible nodes to update."}
	}
	orderedIDs = eligible
	wantHub = false
	for _, id := range orderedIDs {
		if id == "hub" {
			wantHub = true
		}
	}

	// Acquire hub-side per-node locks atomically so duplicate clicks return 409 without side effects.
	s.mu.Lock()
	for _, id := range orderedIDs {
		if s.running[id] != 0 {
			s.mu.Unlock()
			return nil, false, "", RequestError{Status: 409, Msg: nodeMap[id].Name + " already has an update in progress."}
		}
	}
	// Reserve slots before DB writes.
	for _, id := range orderedIDs {
		s.running[id] = -1
	}
	s.mu.Unlock()

	rollbackLocks := func() {
		s.mu.Lock()
		for _, id := range orderedIDs {
			delete(s.running, id)
		}
		s.mu.Unlock()
	}

	now := s.now()
	var created []store.SystemUpdate
	for _, id := range orderedIDs {
		n := nodeMap[id]
		su, err := s.St.CreateSystemUpdate(ctx, store.SystemUpdate{
			NodeID:      id,
			NodeName:    n.Name,
			Action:      store.SysUpdateActionApply,
			FromVersion: n.Version,
			ToVersion:   rel.Version,
			Mode:        n.Mode,
			Status:      store.SysUpdateRunning,
			Message:     "Queued for update to " + rel.Version,
			Actor:       actor,
			IP:          ip,
			StartedAt:   now,
		})
		if err != nil {
			rollbackLocks()
			return nil, false, "", err
		}
		s.mu.Lock()
		s.running[id] = su.ID
		s.mu.Unlock()
		created = append(created, su)
		jobs = append(jobs, *toJobDTO(su))
	}

	if len(orderedIDs) == 1 {
		n := nodeMap[orderedIDs[0]]
		auditTarget = fmt.Sprintf("%s: %s -> %s", orderedIDs[0], n.Version, rel.Version)
	} else {
		auditTarget = fmt.Sprintf("%d nodes -> %s", len(orderedIDs), rel.Version)
	}

	s.invalidateStatus(orderedIDs...)
	// Execute sequentially in background detached from HTTP request context.
	go s.runApplyBatch(context.Background(), created, nodeMap, rel)
	return jobs, wantHub, auditTarget, nil
}

func (s *Service) runApplyBatch(ctx context.Context, jobs []store.SystemUpdate, nodes map[string]NodeStatusDTO, rel *ReleaseInfo) {
	stoppedEarly := false
	stopReason := ""
	// Download each required binary platform at most once during the batch, streaming on disk.
	tempFiles := map[string]string{}
	defer func() {
		for _, p := range tempFiles {
			CleanupTemp(p)
		}
	}()

	for _, job := range jobs {
		if stoppedEarly {
			s.finishJob(ctx, job, store.SysUpdateFailed, "Skipped: "+stopReason, "update.failed", store.AuditFailed)
			continue
		}
		n := nodes[job.NodeID]
		manifest, tempPath, err := s.prepareArtifact(ctx, n, rel, tempFiles)
		if err != nil {
			s.finishJob(ctx, job, store.SysUpdateFailed, err.Error(), "update.failed", store.AuditFailed)
			stoppedEarly = true
			stopReason = err.Error()
			continue
		}

		if job.NodeID == "hub" {
			s.setHubMaintenance(ctx, true)
			_ = s.St.UpdateSystemUpdate(ctx, job.ID, store.SysUpdateRestarting, "Installing "+rel.Version+" and restarting hub…", time.Time{})
			if err := s.applyLocal(ctx, n.Mode, manifest, tempPath); err != nil {
				s.setHubMaintenance(ctx, false)
				s.finishJob(ctx, job, store.SysUpdateFailed, err.Error(), "update.failed", store.AuditFailed)
				return
			}
			// The job stays "restarting" in SQLite for the restarted hub to settle, and the
			// hub lock stays held until this process exits, so a second apply is refused.
			s.watchHubRestart(job)
			return
		}

		// Agent update: silence host-offline alerts for the whole restart window. The
		// silence is left to expire on its own: an agent that came back may still roll
		// back (and disconnect again) within it.
		if s.Mon != nil {
			s.Mon.SilenceHost(job.NodeID, hostSilence)
		}
		status, msg, sent := s.applyRemoteAgent(ctx, job, n, manifest, tempPath)
		if !sent && s.Mon != nil {
			s.Mon.ClearHostSilence(job.NodeID)
		}
		switch status {
		case store.SysUpdateDone:
			s.finishJob(ctx, job, store.SysUpdateDone, msg, "update.done", store.AuditOK)
		case store.SysUpdateRolledBack:
			s.finishJob(ctx, job, store.SysUpdateRolledBack, msg, "update.rolled_back", store.AuditFailed)
			stoppedEarly = true
			stopReason = n.Name + " rolled back"
		default:
			s.finishJob(ctx, job, store.SysUpdateFailed, msg, "update.failed", store.AuditFailed)
			stoppedEarly = true
			stopReason = n.Name + ": " + msg
		}
	}
}

// setHubMaintenance silences (or unsilences) watching agents' hub-down alerts for the
// hub's own restart window.
func (s *Service) setHubMaintenance(ctx context.Context, on bool) {
	s.mu.Lock()
	if on {
		s.hubMaintUntil = s.now().Add(hostSilence)
	} else {
		s.hubMaintUntil = time.Time{}
	}
	s.mu.Unlock()
	if s.Hub != nil {
		s.Hub.PushWatchdog(ctx)
	}
}

// watchHubRestart fails a hub job that is still "restarting" after HubRestartTimeout
// while this old process still runs: binary mode would have exec'd the new version and
// a container helper would have stopped this container, so the update never happened
// (e.g. the helper died first). Until then the hub lock stays held.
func (s *Service) watchHubRestart(job store.SystemUpdate) {
	time.AfterFunc(orDefault(s.HubRestartTimeout, defaultHubRestartTimeout), func() {
		ctx := context.Background()
		if s.St != nil {
			open, err := s.St.OpenSystemUpdates(ctx)
			if err != nil {
				return
			}
			stillOpen := false
			for _, o := range open {
				stillOpen = stillOpen || o.ID == job.ID
			}
			if !stillOpen {
				s.clearLock("hub")
				return
			}
		}
		s.setHubMaintenance(ctx, false)
		if s.Local != nil {
			s.Local.releaseBusy()
		}
		s.finishJob(ctx, job, store.SysUpdateFailed,
			fmt.Sprintf("The hub did not restart within %s and still runs %s; the update was not applied (did the update helper fail?)", orDefault(s.HubRestartTimeout, defaultHubRestartTimeout), s.Version),
			"update.failed", store.AuditFailed)
	})
}

func (s *Service) prepareArtifact(ctx context.Context, n NodeStatusDTO, rel *ReleaseInfo, cache map[string]string) (host.SelfUpdateManifest, string, error) {
	m := host.SelfUpdateManifest{
		Version:    rel.Version,
		Sums:       rel.RawSums,
		Sig:        rel.RawSig,
		ImagesJSON: rel.RawImages,
	}
	switch n.Mode {
	case "container":
		digest := rel.Digests[n.Platform]
		if digest == "" {
			return m, "", fmt.Errorf("release %s has no signed image digest for %s", rel.Version, n.Platform)
		}
		m.Image = rel.Image
		m.Digest = digest
		return m, "", nil
	case "binary":
		assetName := assetFromPlatform(n.Platform)
		if assetName == "" {
			return m, "", fmt.Errorf("unsupported binary platform %q", n.Platform)
		}
		if p, ok := cache[assetName]; ok {
			return m, p, nil
		}
		p, err := s.Checker.DownloadBinaryToTemp(ctx, rel, assetName, os.TempDir())
		if err != nil {
			return m, "", err
		}
		cache[assetName] = p
		return m, p, nil
	default:
		return m, "", fmt.Errorf("unsupported mode %q", n.Mode)
	}
}

func assetFromPlatform(platform string) string {
	switch platform {
	case "linux/amd64":
		return "moorage-linux-amd64"
	case "linux/arm64":
		return "moorage-linux-arm64"
	case "linux/arm/v7":
		return "moorage-linux-armv7"
	default:
		return ""
	}
}

func (s *Service) applyLocal(ctx context.Context, mode string, m host.SelfUpdateManifest, binPath string) error {
	if s.Local == nil {
		return host.ErrSelfUpdateUnsupported
	}
	if mode == "container" {
		return s.Local.ApplyContainer(ctx, m)
	}
	f, err := os.Open(binPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return s.Local.ApplyBinary(ctx, m, f)
}

// applyRemoteAgent sends the update to an agent and waits for the outcome. sent reports
// whether the agent accepted the update (and so restarts).
func (s *Service) applyRemoteAgent(ctx context.Context, job store.SystemUpdate, n NodeStatusDTO, m host.SelfUpdateManifest, binPath string) (status, msg string, sent bool) {
	rm, _, ok := s.Reg.Remote(job.NodeID)
	if !ok {
		return store.SysUpdateFailed, "agent is offline", false
	}

	waitCh, cancelWait := s.subscribeReconnect(job.NodeID)
	defer cancelWait()

	var sendErr error
	if n.Mode == "container" {
		_ = s.St.UpdateSystemUpdate(ctx, job.ID, store.SysUpdateRestarting, "Pulling verified image and replacing container…", time.Time{})
		sendErr = rm.ApplyContainerUpdate(ctx, m)
	} else {
		_ = s.St.UpdateSystemUpdate(ctx, job.ID, store.SysUpdateRestarting, "Streaming signed binary and restarting agent…", time.Time{})
		f, err := os.Open(binPath)
		if err != nil {
			return store.SysUpdateFailed, err.Error(), false
		}
		sendErr = rm.ApplyBinaryUpdate(ctx, m, f)
		_ = f.Close()
	}
	if sendErr != nil {
		return store.SysUpdateFailed, sendErr.Error(), false
	}

	status, msg = s.waitAgentOutcome(ctx, job.NodeID, job.FromVersion, job.ToVersion, waitCh)
	return status, msg, true
}

// waitAgentOutcome waits up to AgentWait (180 s, longer than the agent's 120 s health
// window) for the agent to reconnect as wantVer, or to come back on fromVer with a
// rollback note.
func (s *Service) waitAgentOutcome(ctx context.Context, agentID, fromVer, wantVer string, waitCh chan string) (string, string) {
	wait := orDefault(s.AgentWait, defaultAgentWait)
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	poll := time.NewTicker(time.Second)
	defer poll.Stop()

	wantNorm := strings.TrimPrefix(wantVer, "v")
	fromNorm := strings.TrimPrefix(fromVer, "v")
	sawDisconnect := false

	for {
		_, info, ok := s.Reg.Remote(agentID)
		if !ok {
			sawDisconnect = true
		} else {
			curNorm := strings.TrimPrefix(info.Version, "v")
			if curNorm == wantNorm {
				return store.SysUpdateDone, "Updated to " + info.Version
			}
			// Check if agent reconnected with the old version after an automatic rollback.
			if sawDisconnect && (curNorm == fromNorm || fromNorm == "") {
				if rm, _, ok := s.Reg.Remote(agentID); ok {
					qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
					st, err := rm.SelfUpdateStatus(qctx)
					cancel()
					if err == nil && st.LastRollback != "" {
						return store.SysUpdateRolledBack, st.LastRollback
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			return store.SysUpdateFailed, "cancelled"
		case <-deadline.C:
			// Last look: the agent may have rolled back without the hub noticing a
			// disconnect (e.g. a fast restart between two polls).
			if rm, info, ok := s.Reg.Remote(agentID); ok {
				if normVersion(info.Version) == wantNorm {
					return store.SysUpdateDone, "Updated to " + info.Version
				}
				qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				st, err := rm.SelfUpdateStatus(qctx)
				cancel()
				if err == nil && st.LastRollback != "" {
					return store.SysUpdateRolledBack, st.LastRollback
				}
			}
			return store.SysUpdateFailed, fmt.Sprintf("timed out waiting %s for agent to reconnect as %s", wait, wantVer)
		case <-waitCh:
			// A (re)connect after the update was sent means the agent restarted, even
			// when it came back between two polls and no disconnect was seen.
			sawDisconnect = true
		case <-poll.C:
		}
	}
}

func (s *Service) subscribeReconnect(agentID string) (chan string, func()) {
	ch := make(chan string, 4)
	s.mu.Lock()
	s.waiters[agentID] = append(s.waiters[agentID], ch)
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		list := s.waiters[agentID]
		for i, c := range list {
			if c == ch {
				s.waiters[agentID] = append(list[:i], list[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}
}

// Rollback initiates a manual rollback of nodeID ("hub" or agent ID) to its kept previous
// binary or -moorage-prev container.
func (s *Service) Rollback(ctx context.Context, nodeID, actor, ip string) (JobDTO, string, error) {
	ov, err := s.Overview(ctx)
	if err != nil {
		return JobDTO{}, "", err
	}
	var node *NodeStatusDTO
	for i := range ov.Nodes {
		if ov.Nodes[i].ID == nodeID {
			node = &ov.Nodes[i]
			break
		}
	}
	if node == nil {
		return JobDTO{}, "", RequestError{Status: 404, Msg: "Host " + nodeID + " not found."}
	}
	if !node.Connected {
		return JobDTO{}, "", RequestError{Status: 503, Msg: node.Name + " is offline."}
	}
	if !node.SelfUpdate {
		return JobDTO{}, "", RequestError{Status: 403, Msg: node.Name + " has MOORAGE_SELF_UPDATE=false."}
	}
	if !node.Supported {
		return JobDTO{}, "", RequestError{Status: 422, Msg: node.Name + " does not support self-update rollback."}
	}
	if node.PrevVersion == "" {
		return JobDTO{}, "", RequestError{Status: 409, Msg: "No previous version is kept on " + node.Name + "."}
	}

	s.mu.Lock()
	if s.running[nodeID] != 0 {
		s.mu.Unlock()
		return JobDTO{}, "", RequestError{Status: 409, Msg: node.Name + " already has an update job in progress."}
	}
	s.running[nodeID] = -1
	s.mu.Unlock()

	job, err := s.St.CreateSystemUpdate(ctx, store.SystemUpdate{
		NodeID:      nodeID,
		NodeName:    node.Name,
		Action:      store.SysUpdateActionRollback,
		FromVersion: node.Version,
		ToVersion:   node.PrevVersion,
		Mode:        node.Mode,
		Status:      store.SysUpdateRestarting,
		Message:     "Rolling back to " + node.PrevVersion + "…",
		Actor:       actor,
		IP:          ip,
		StartedAt:   s.now(),
	})
	if err != nil {
		s.clearLock(nodeID)
		return JobDTO{}, "", err
	}
	s.mu.Lock()
	s.running[nodeID] = job.ID
	s.mu.Unlock()

	auditTarget := fmt.Sprintf("%s: %s -> %s", nodeID, node.Version, node.PrevVersion)
	s.invalidateStatus(nodeID)

	go func() {
		bg := context.Background()
		if nodeID == "hub" {
			if s.Local == nil {
				s.finishJob(bg, job, store.SysUpdateFailed, host.ErrSelfUpdateUnsupported.Error(), "update.failed", store.AuditFailed)
				return
			}
			s.setHubMaintenance(bg, true)
			if err := s.Local.Rollback(bg); err != nil {
				s.setHubMaintenance(bg, false)
				s.finishJob(bg, job, store.SysUpdateFailed, err.Error(), "update.failed", store.AuditFailed)
				return
			}
			s.watchHubRestart(job)
			return
		}

		if s.Mon != nil {
			s.Mon.SilenceHost(nodeID, hostSilence)
		}
		unsilence := func() {
			if s.Mon != nil {
				s.Mon.ClearHostSilence(nodeID)
			}
		}
		waitCh, cancelWait := s.subscribeReconnect(nodeID)
		defer cancelWait()

		rm, _, ok := s.Reg.Remote(nodeID)
		if !ok {
			unsilence()
			s.finishJob(bg, job, store.SysUpdateFailed, "agent is offline", "update.failed", store.AuditFailed)
			return
		}
		if err := rm.RollbackSelfUpdate(bg); err != nil {
			unsilence()
			s.finishJob(bg, job, store.SysUpdateFailed, err.Error(), "update.failed", store.AuditFailed)
			return
		}
		status, msg := s.waitAgentOutcome(bg, nodeID, node.Version, node.PrevVersion, waitCh)
		if status == store.SysUpdateDone {
			s.finishJob(bg, job, store.SysUpdateDone, "Rolled back to "+node.PrevVersion, "update.done", store.AuditOK)
			return
		}
		if msg == "" {
			msg = "rollback failed"
		}
		s.finishJob(bg, job, store.SysUpdateFailed, msg, "update.failed", store.AuditFailed)
	}()

	return *toJobDTO(job), auditTarget, nil
}

func (s *Service) finishJob(ctx context.Context, job store.SystemUpdate, status, msg, auditAction, auditOutcome string) {
	if s.St != nil {
		_ = s.St.UpdateSystemUpdate(ctx, job.ID, status, msg, s.now())
	}
	s.invalidateStatus(job.NodeID)
	s.clearLock(job.NodeID)
	s.emitAudit(ctx, job, auditAction, auditOutcome)
}

func (s *Service) clearLock(nodeID string) {
	s.mu.Lock()
	delete(s.running, nodeID)
	s.mu.Unlock()
}

// IsRequestError extracts HTTP status and user message if err is RequestError.
func IsRequestError(err error) (int, string, bool) {
	var re RequestError
	if errors.As(err, &re) {
		return re.Status, re.Msg, true
	}
	return 0, "", false
}
