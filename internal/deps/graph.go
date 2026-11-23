package deps

import (
	"sort"

	"moorage/internal/store"
)

// MaxDepth bounds how far up or down the graph a cascade looks.
const MaxDepth = 3

// Graph is an immutable snapshot of the active (not ignored) edges. Edges inside a cycle
// are kept for display but left out of Upstream and Downstream, so cascade grouping never
// loops and never picks a "cause" that is itself caused by the stack.
type Graph struct {
	up     map[string][]string // stack key -> keys it depends on
	down   map[string][]string // stack key -> keys that depend on it
	cyclic map[string]bool     // stack keys that are part of a cycle
	cycles [][]store.StackRef
}

// Build makes a graph from stored edges; ignored edges don't count.
func Build(edges []store.StackDep) *Graph {
	all := map[string][]string{}
	for _, e := range edges {
		if e.Ignored || e.From == e.To {
			continue
		}
		all[e.From.Key()] = append(all[e.From.Key()], e.To.Key())
	}
	g := &Graph{up: map[string][]string{}, down: map[string][]string{}, cyclic: map[string]bool{}}
	comp := components(all)
	for _, members := range comp {
		if len(members) < 2 {
			continue
		}
		var refs []store.StackRef
		for _, k := range members {
			g.cyclic[k] = true
			if r, ok := store.ParseStackRef(k); ok {
				refs = append(refs, r)
			}
		}
		sort.Slice(refs, func(i, j int) bool { return refs[i].Key() < refs[j].Key() })
		g.cycles = append(g.cycles, refs)
	}
	sort.Slice(g.cycles, func(i, j int) bool { return g.cycles[i][0].Key() < g.cycles[j][0].Key() })
	sccOf := map[string]int{}
	for i, members := range comp {
		for _, k := range members {
			sccOf[k] = i
		}
	}
	for from, tos := range all {
		for _, to := range tos {
			if sccOf[from] == sccOf[to] {
				continue // an edge inside a cycle
			}
			g.up[from] = append(g.up[from], to)
			g.down[to] = append(g.down[to], from)
		}
	}
	for _, m := range []map[string][]string{g.up, g.down} {
		for k := range m {
			sort.Strings(m[k])
		}
	}
	return g
}

// Upstream returns the stacks r depends on, directly or transitively up to depth, nearest
// first. Cycle edges are skipped.
func (g *Graph) Upstream(r store.StackRef, depth int) []store.StackRef {
	return g.walk(g.up, r, depth)
}

// Downstream returns the stacks that depend on r, nearest first.
func (g *Graph) Downstream(r store.StackRef, depth int) []store.StackRef {
	return g.walk(g.down, r, depth)
}

func (g *Graph) walk(adj map[string][]string, r store.StackRef, depth int) []store.StackRef {
	if g == nil {
		return nil
	}
	start := r.Key()
	seen := map[string]bool{start: true}
	level := []string{start}
	var out []store.StackRef
	for d := 0; d < depth && len(level) > 0; d++ {
		var next []string
		for _, k := range level {
			for _, n := range adj[k] {
				if seen[n] {
					continue
				}
				seen[n] = true
				next = append(next, n)
				if ref, ok := store.ParseStackRef(n); ok {
					out = append(out, ref)
				}
			}
		}
		level = next
	}
	return out
}

// Cycles lists the groups of stacks that depend on each other in a loop.
func (g *Graph) Cycles() [][]store.StackRef {
	if g == nil {
		return nil
	}
	return g.cycles
}

// InCycle reports whether a stack is part of a cycle.
func (g *Graph) InCycle(r store.StackRef) bool {
	return g != nil && g.cyclic[r.Key()]
}

// components returns the strongly connected components of adj (recursive Tarjan; the
// recursion depth is bounded by the number of stacks, which stays small).
func components(adj map[string][]string) [][]string {
	var nodes []string
	seenNode := map[string]bool{}
	for from, tos := range adj {
		for _, k := range append([]string{from}, tos...) {
			if !seenNode[k] {
				seenNode[k] = true
				nodes = append(nodes, k)
			}
		}
	}
	sort.Strings(nodes)
	index, low := map[string]int{}, map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var out [][]string
	next := 0
	var visit func(v string)
	visit = func(v string) {
		index[v], low[v] = next, next
		next++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range adj[v] {
			if _, ok := index[w]; !ok {
				visit(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] == index[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			sort.Strings(comp)
			out = append(out, comp)
		}
	}
	for _, v := range nodes {
		if _, ok := index[v]; !ok {
			visit(v)
		}
	}
	return out
}
