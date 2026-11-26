package host

import (
	"context"
	"net/http"

	"moorage/internal/engine"
)

// PortLister is implemented by hosts that report their port map (M17B): published ports
// of every container, ports declared by stopped stacks and, in native / host-network
// mode, the host's own listening sockets.
type PortLister interface {
	Ports(ctx context.Context) (engine.PortReport, error)
}

func (l Local) Ports(ctx context.Context) (engine.PortReport, error) { return l.E.PortReport(ctx) }

// Ports asks the agent; an agent older than M17B answers 404 and the hub builds the map
// from its container list and the compose files of its stacks (no native sockets).
func (rm *Remote) Ports(ctx context.Context) (engine.PortReport, error) {
	var rep engine.PortReport
	err := rm.call(ctx, http.MethodGet, "/ports", nil, &rep)
	if err == nil || !IsUnsupported(err) {
		return rep, err
	}
	return FallbackPorts(ctx, rm)
}

// FallbackPorts builds a port map from the generic Host interface (containers through
// ContainerManager when available, otherwise the containers of each stack).
func FallbackPorts(ctx context.Context, h Host) (engine.PortReport, error) {
	var cs []engine.ContainerInfo
	cm, hasCM := h.(ContainerManager)
	if hasCM {
		list, err := cm.Containers(ctx)
		if err != nil {
			hasCM = false
		} else {
			cs = list
		}
	}
	stacks, err := h.ListStacks(ctx)
	if err != nil && !hasCM {
		return engine.PortReport{}, err
	}
	var declared []engine.StackPorts
	for _, sm := range stacks {
		st, err := h.GetStack(ctx, sm.Name)
		if err != nil {
			continue
		}
		if !hasCM {
			for _, c := range st.Containers {
				cs = append(cs, engine.ContainerInfo{Container: c, Project: sm.Name, Managed: st.Managed})
			}
		}
		if st.Managed && st.Compose != "" {
			if ps := engine.DeclaredPorts(ctx, sm.Name, st.Compose, st.Env); len(ps) > 0 {
				declared = append(declared, engine.StackPorts{Stack: sm.Name, Ports: ps})
			}
		}
	}
	return engine.BuildPortReport(cs, declared, nil, h.Info(ctx).NetMode), nil
}

func portRoutes(mux *http.ServeMux, h Host) {
	pl, ok := h.(PortLister)
	if !ok {
		return
	}
	mux.HandleFunc("GET /ports", func(w http.ResponseWriter, r *http.Request) {
		reply(w)(pl.Ports(r.Context()))
	})
}

// HostNet returns the network mode, auto-detected addresses and the data directory's host
// path of this host (M17B).
func (l Local) HostNet() (string, []engine.HostAddress, string) {
	return l.E.NetMode(), l.E.DetectedAddresses(), l.E.RuntimeNet().DataHostPath
}
