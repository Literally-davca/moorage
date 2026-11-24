package host

import (
	"context"
	"moorage/internal/engine"
	"sort"
	"sync"
	"time"

	"moorage/internal/store"
)

// LocalID is the id of the Docker host the hub process runs on (all-in-one mode).
const LocalID = "local"

// Entry describes a host for the UI.
type Entry struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Kind          string    `json:"kind"` // local | agent
	Connected     bool      `json:"connected"`
	LastSeen      time.Time `json:"lastSeen,omitzero"`
	ReadOnly      bool      `json:"readOnly"`
	Terminal      bool      `json:"terminal,omitempty"`
	Version       string    `json:"version,omitempty"`
	DockerVersion string    `json:"dockerVersion,omitempty"`
	Engine        string    `json:"engine,omitempty"` // docker | podman; empty from older agents
	EngineVersion string    `json:"engineVersion,omitempty"`
	Rootless      bool      `json:"rootless,omitempty"`
	Hostname      string    `json:"hostname,omitempty"`
	StacksDir     string    `json:"stacksDir,omitempty"`
	Watchdog      bool      `json:"watchdog,omitempty"`
	WatchdogAddr  string    `json:"watchdogAddr,omitempty"`
	Probes        bool      `json:"probes,omitempty"` // the agent runs checks for the hub (multi-location checks)
	// Locks are the host's own security switches (from its env; older agents send none).
	Locks             *engine.HostLocks `json:"locks,omitempty"`
	HostIP            string            `json:"hostIp,omitempty"`
	BaseDomain        string            `json:"baseDomain,omitempty"`
	HostIPFromEnv     bool              `json:"hostIpFromEnv,omitempty"`
	BaseDomainFromEnv bool              `json:"baseDomainFromEnv,omitempty"`
}

type conn struct {
	remote *Remote
	info   AgentInfo
	close  func() error
	since  time.Time
}

// Registry knows every host: the local one (optional) and paired agents.
type Registry struct {
	local     Host
	localName string
	localRO   bool
	localTerm bool
	st        *store.Store

	mu    sync.Mutex
	conns map[string]*conn
}

// NewRegistry creates a registry. local may be nil (hub-only mode).
func NewRegistry(local Host, localName string, localReadOnly bool, st *store.Store) *Registry {
	localTerm := false
	if loc, ok := local.(Local); ok {
		localTerm = loc.AllowTTY && !loc.ReadOnly && !localReadOnly
	}
	return &Registry{local: local, localName: localName, localRO: localReadOnly, localTerm: localTerm, st: st, conns: map[string]*conn{}}
}

// SetLocalTerminal sets whether the local host offers the web terminal.
func (r *Registry) SetLocalTerminal(allow bool) {
	r.localTerm = allow && !r.localRO
}

// HasLocal reports whether this process manages a local Docker host.
func (r *Registry) HasLocal() bool { return r.local != nil }

// Get returns a usable host, ErrUnknownHost, or ErrOffline.
func (r *Registry) Get(ctx context.Context, id string) (Host, Entry, error) {
	if id == LocalID {
		if r.local == nil {
			return nil, Entry{}, ErrUnknownHost
		}
		return r.local, r.localEntry(), nil
	}
	r.mu.Lock()
	c := r.conns[id]
	r.mu.Unlock()
	if c != nil {
		return c.remote, r.entryOf(id, c), nil
	}
	a, err := r.st.AgentByID(ctx, id)
	if err != nil {
		return nil, Entry{}, ErrUnknownHost
	}
	return nil, Entry{ID: a.ID, Name: a.Name, Kind: "agent", LastSeen: a.LastSeen, Hostname: a.Hostname}, ErrOffline
}

func (r *Registry) entryOf(id string, c *conn) Entry {
	return Entry{
		ID: id, Kind: "agent", Connected: true, LastSeen: time.Now(),
		ReadOnly: c.info.ReadOnly, Terminal: c.info.Terminal && !c.info.ReadOnly,
		Version: c.info.Version, DockerVersion: c.info.DockerVersion,
		Engine: c.info.Engine, EngineVersion: c.info.EngineVersion, Rootless: c.info.Rootless,
		Hostname: c.info.Name, StacksDir: c.info.StacksDir,
		Watchdog: c.info.Watchdog, WatchdogAddr: c.info.WatchdogAddr,
		Probes: c.info.Probes,
		Locks:  agentLocks(c.info), HostIP: c.info.HostIP, BaseDomain: c.info.BaseDomain,
	}
}

// agentLocks returns the locks an agent reported; an agent too old to report them only
// tells read-only and terminal.
func agentLocks(i AgentInfo) *engine.HostLocks {
	l := i.Locks
	l.ReadOnly = l.ReadOnly || i.ReadOnly
	l.Terminal = i.Terminal && !i.ReadOnly
	return &l
}

func (r *Registry) localEntry() Entry {
	e := Entry{ID: LocalID, Name: r.localName, Kind: "local", Connected: true, ReadOnly: r.localRO, Terminal: r.localTerm && !r.localRO}
	if lk, ok := r.local.(interface{ Locks() engine.HostLocks }); ok {
		l := lk.Locks()
		e.Locks = &l
	}
	if hn, ok := r.local.(interface{ Network() (string, string) }); ok {
		e.HostIP, e.BaseDomain = hn.Network()
	}
	return e
}

// Remote returns the live tunnel client for a connected agent, or nil if offline.
func (r *Registry) Remote(id string) (*Remote, AgentInfo, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.conns[id]
	if c == nil {
		return nil, AgentInfo{}, false
	}
	return c.remote, c.info, true
}

// List returns all hosts: local first, then agents by name.
func (r *Registry) List(ctx context.Context) ([]Entry, error) {
	var out []Entry
	if r.local != nil {
		out = append(out, r.localEntry())
	}
	agents, err := r.st.Agents(ctx)
	if err != nil {
		return out, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var list []Entry
	for _, a := range agents {
		e := Entry{
			ID: a.ID, Name: a.Name, Kind: "agent", LastSeen: a.LastSeen, Hostname: a.Hostname,
			WatchdogAddr: a.WatchdogAddr,
		}
		if c := r.conns[a.ID]; c != nil {
			e = r.entryOf(a.ID, c)
			e.Name = a.Name
			if e.WatchdogAddr == "" {
				e.WatchdogAddr = a.WatchdogAddr
			}
		}
		list = append(list, e)
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return append(out, list...), nil
}

// Attach registers a live agent connection, closing an older one of the same agent.
func (r *Registry) Attach(id string, remote *Remote, info AgentInfo, closeFn func() error) {
	r.mu.Lock()
	old := r.conns[id]
	r.conns[id] = &conn{remote: remote, info: info, close: closeFn, since: time.Now()}
	r.mu.Unlock()
	if old != nil {
		old.close() //nolint:errcheck
	}
}

// Detach forgets a connection if it is still the current one.
func (r *Registry) Detach(id string, remote *Remote) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.conns[id]; c != nil && c.remote == remote {
		delete(r.conns, id)
	}
}

// Disconnect drops a live connection (used when an agent is removed).
func (r *Registry) Disconnect(id string) {
	r.mu.Lock()
	c := r.conns[id]
	delete(r.conns, id)
	r.mu.Unlock()
	if c != nil {
		c.close() //nolint:errcheck
	}
}
