// Package dag provides a directed acyclic graph for step dependency scheduling.
// It supports cycle detection, ready-set calculation, transitive ancestor queries,
// and splice operations for dynamic plan modification.
package dag

import "fmt"

// Graph is a directed acyclic graph of node IDs.
// Edges point from dependency to dependent (parent → child).
type Graph struct {
	nodes    map[string]bool
	children map[string][]string // node → nodes that depend on it
	parents  map[string][]string // node → nodes it depends on
}

// New builds a Graph from node IDs and their dependency edges.
// edges maps each node to the IDs it depends on (its parents).
// Returns an error if the resulting graph contains a cycle or
// references an unknown node.
func New(nodeIDs []string, dependsOn map[string][]string) (*Graph, error) {
	g := &Graph{
		nodes:    make(map[string]bool, len(nodeIDs)),
		children: make(map[string][]string, len(nodeIDs)),
		parents:  make(map[string][]string, len(nodeIDs)),
	}
	for _, id := range nodeIDs {
		if g.nodes[id] {
			return nil, fmt.Errorf("duplicate node %q", id)
		}
		g.nodes[id] = true
	}
	for id, deps := range dependsOn {
		if !g.nodes[id] {
			return nil, fmt.Errorf("depends_on references unknown node %q", id)
		}
		for _, dep := range deps {
			if !g.nodes[dep] {
				return nil, fmt.Errorf("node %q depends on unknown node %q", id, dep)
			}
			g.parents[id] = append(g.parents[id], dep)
			g.children[dep] = append(g.children[dep], id)
		}
	}
	if cycle := g.detectCycle(); cycle != "" {
		return nil, fmt.Errorf("cycle detected involving node %q", cycle)
	}
	return g, nil
}

// Ready returns node IDs whose parents are all in the completed set
// and that are not themselves completed.
func (g *Graph) Ready(completed map[string]bool) []string {
	var ready []string
	for id := range g.nodes {
		if completed[id] {
			continue
		}
		allMet := true
		for _, dep := range g.parents[id] {
			if !completed[dep] {
				allMet = false
				break
			}
		}
		if allMet {
			ready = append(ready, id)
		}
	}
	return ready
}

// Ancestors returns all transitive ancestors (dependencies) of a node,
// not including the node itself. Order is not guaranteed.
func (g *Graph) Ancestors(id string) []string {
	visited := make(map[string]bool)
	g.walkAncestors(id, visited)
	delete(visited, id)
	result := make([]string, 0, len(visited))
	for v := range visited {
		result = append(result, v)
	}
	return result
}

func (g *Graph) walkAncestors(id string, visited map[string]bool) {
	if visited[id] {
		return
	}
	visited[id] = true
	for _, p := range g.parents[id] {
		g.walkAncestors(p, visited)
	}
}

// Children returns the direct dependents of a node.
func (g *Graph) Children(id string) []string {
	return g.children[id]
}

// Splice inserts a new node between `after` and the dependents listed in
// `rewire`. Each node in `rewire` must currently depend on `after`; that
// edge is replaced with after → newID → rewireNode.
// Returns an error if the insertion would create a cycle or if any
// rewire target does not depend on `after`.
func (g *Graph) Splice(newID, after string, rewire []string) error {
	if g.nodes[newID] {
		return fmt.Errorf("node %q already exists", newID)
	}
	if !g.nodes[after] {
		return fmt.Errorf("after node %q does not exist", after)
	}

	childSet := make(map[string]bool, len(g.children[after]))
	for _, c := range g.children[after] {
		childSet[c] = true
	}
	for _, r := range rewire {
		if !childSet[r] {
			return fmt.Errorf("node %q does not depend on %q", r, after)
		}
	}

	g.nodes[newID] = true
	g.parents[newID] = []string{after}
	g.children[after] = appendUnique(g.children[after], newID)

	for _, r := range rewire {
		g.parents[r] = replaceInSlice(g.parents[r], after, newID)
		g.children[after] = removeFromSlice(g.children[after], r)
		g.children[newID] = appendUnique(g.children[newID], r)
	}

	if cycle := g.detectCycle(); cycle != "" {
		g.removeSplice(newID, after, rewire)
		return fmt.Errorf("splice would create cycle involving node %q", cycle)
	}
	return nil
}

// removeSplice undoes a splice operation (used when cycle is detected).
func (g *Graph) removeSplice(newID, after string, rewire []string) {
	for _, r := range rewire {
		g.parents[r] = replaceInSlice(g.parents[r], newID, after)
		g.children[after] = appendUnique(g.children[after], r)
	}
	delete(g.nodes, newID)
	delete(g.parents, newID)
	delete(g.children, newID)
	g.children[after] = removeFromSlice(g.children[after], newID)
}

// detectCycle returns a node ID involved in a cycle, or "" if acyclic.
// Uses iterative DFS with white/gray/black coloring.
func (g *Graph) detectCycle() string {
	const (
		white = 0 // unvisited
		gray  = 1 // in current path
		black = 2 // fully explored
	)
	color := make(map[string]int, len(g.nodes))

	for id := range g.nodes {
		if color[id] != white {
			continue
		}
		type frame struct {
			id       string
			childIdx int
		}
		stack := []frame{{id: id}}
		color[id] = gray

		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			kids := g.children[top.id]
			if top.childIdx >= len(kids) {
				color[top.id] = black
				stack = stack[:len(stack)-1]
				continue
			}
			child := kids[top.childIdx]
			top.childIdx++

			switch color[child] {
			case gray:
				return child
			case white:
				color[child] = gray
				stack = append(stack, frame{id: child})
			}
		}
	}
	return ""
}

func replaceInSlice(s []string, old, new string) []string {
	for i, v := range s {
		if v == old {
			s[i] = new
			return s
		}
	}
	return s
}

func removeFromSlice(s []string, val string) []string {
	for i, v := range s {
		if v == val {
			return append(s[:i], s[i+1:]...)
		}
	}
	return s
}

func appendUnique(s []string, val string) []string {
	for _, v := range s {
		if v == val {
			return s
		}
	}
	return append(s, val)
}
