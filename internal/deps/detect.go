// Package deps keeps the dependency graph between stacks: edges detected from the compose
// model of each host plus edges added by admins, and the graph the monitor uses to fold
// cascading alerts into one message.
package deps

import (
	"sort"
	"strings"

	"moorage/internal/engine"
	"moorage/internal/store"
)

// Detection reasons.
const (
	ReasonNetwork  = "network"
	ReasonHostname = "hostname"
)

// Compute turns one host's compose facts into detected edges (From depends on To). Only
// stacks of that host are considered; detection never crosses hosts.
//
//   - Shared network: stack A declares network N (not external) and stack B uses N as
//     external (matched by the real Docker name, so name: overrides count) → B depends on A.
//     When several stacks declare the same name, the owner is unclear and no edge is made.
//   - Host name: a service of B refers to a service or container name of A (links,
//     external_links, or an env value like host:port or a URL) and the two services share a
//     network. Names B itself answers to are skipped, and so are names that more than one
//     other stack answers to: detection stays conservative.
func Compute(hostID string, f engine.DepFacts) []store.StackDep {
	edges := map[[2]string]store.StackDep{}
	add := func(from, to, reason, detail string) {
		if from == to {
			return
		}
		k := [2]string{from, to}
		if old, ok := edges[k]; ok && (old.Reason == ReasonNetwork || reason != ReasonNetwork) {
			return // a network edge wins over a host name edge of the same pair
		}
		edges[k] = store.StackDep{
			From: store.StackRef{HostID: hostID, Stack: from}, To: store.StackRef{HostID: hostID, Stack: to},
			Source: store.DepDetected, Reason: reason, Detail: detail,
		}
	}
	networkEdges(f, add)
	hostnameEdges(f, add)
	out := make([]store.StackDep, 0, len(edges))
	for _, e := range edges {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From.Stack != out[j].From.Stack {
			return out[i].From.Stack < out[j].From.Stack
		}
		return out[i].To.Stack < out[j].To.Stack
	})
	return out
}

func networkEdges(f engine.DepFacts, add func(from, to, reason, detail string)) {
	owners := map[string][]string{} // Docker network name -> stacks that create it
	for _, s := range f.Stacks {
		for _, n := range s.Networks {
			if !n.External {
				owners[n.Name] = append(owners[n.Name], s.Name)
			}
		}
	}
	for _, s := range f.Stacks {
		for _, n := range s.Networks {
			if !n.External || len(owners[n.Name]) != 1 {
				continue
			}
			add(s.Name, owners[n.Name][0], ReasonNetwork, "external network "+n.Name)
		}
	}
}

// nameOwner is a service of a stack that answers to a host name.
type nameOwner struct {
	stack    string
	networks []string
}

func hostnameEdges(f engine.DepFacts, add func(from, to, reason, detail string)) {
	byName := map[string][]nameOwner{}
	own := map[string]map[string]bool{} // stack -> names its own services answer to
	for _, s := range f.Stacks {
		own[s.Name] = map[string]bool{}
		for _, svc := range s.Services {
			for _, n := range serviceNames(s.Name, svc) {
				own[s.Name][n] = true
				byName[n] = append(byName[n], nameOwner{stack: s.Name, networks: svc.Networks})
			}
		}
	}
	for _, s := range f.Stacks {
		for _, svc := range s.Services {
			for _, h := range svc.Hosts {
				if own[s.Name][h] {
					continue // refers to its own stack
				}
				targets := map[string]bool{}
				for _, o := range byName[h] {
					if o.stack != s.Name && shareNetwork(o.networks, svc.Networks) {
						targets[o.stack] = true
					}
				}
				if len(targets) != 1 {
					continue // unknown or ambiguous
				}
				for t := range targets {
					add(s.Name, t, ReasonHostname, "service "+svc.Name+" refers to "+h)
				}
			}
		}
	}
}

// serviceNames are the DNS names a compose service answers to on its networks: the
// service name, an explicit container_name, and the default container names.
func serviceNames(stack string, svc engine.ServiceFact) []string {
	names := []string{strings.ToLower(svc.Name), strings.ToLower(stack + "-" + svc.Name + "-1"), strings.ToLower(stack + "_" + svc.Name + "_1")}
	if svc.ContainerName != "" {
		names = append(names, strings.ToLower(svc.ContainerName))
	}
	return names
}

func shareNetwork(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}
