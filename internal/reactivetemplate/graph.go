package reactivetemplate

import (
	"sort"

	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/sequencer"
)

// dependencyGraph is rebuilt from the last executed reads. Literal IR hints
// are used only before the first render or after a failed render.
type dependencyGraph struct {
	edges [][]int          // consumer -> producers
	order []graphComponent // producers precede their consumers
	cost  int
}
type graphComponent struct {
	members []int
	cyclic  bool
	depth   int
}

func buildDependencyGraph(definitions []Definition, snapshot sequencer.Snapshot, reads map[ProgramID][]template.Dependency) (dependencyGraph, error) {
	n := len(definitions)
	g := dependencyGraph{edges: make([][]int, n), cost: n}
	producers := make(map[domain.HAAlias]int)
	for i, definition := range definitions {
		if !definition.hasTarget {
			continue
		}
		record, ok := snapshot.Registry[definition.target]
		if !ok || record.Retired() {
			return dependencyGraph{}, ErrInvalidProjection
		}
		_, _, aliases, err := record.Aliases()
		if err != nil {
			return dependencyGraph{}, err
		}
		for _, alias := range aliases {
			if _, err := alias.Value(); err != nil {
				return dependencyGraph{}, ErrInvalidProjection
			}
			if _, exists := producers[alias]; exists {
				return dependencyGraph{}, ErrInvalidProjection
			}
			producers[alias] = i
			g.cost++
		}
	}
	for i, definition := range definitions {
		deps, known := reads[definition.id]
		if !known {
			deps = definition.static
		}
		seen := make(map[int]struct{})
		for _, dep := range deps {
			g.cost++
			alias, ok := dep.Alias()
			if !ok || dep.Kind() == template.DependencyMembership {
				continue
			}
			producer, ok := producers[alias]
			if !ok {
				continue
			}
			seen[producer] = struct{}{}
		}
		for producer := range seen {
			g.edges[i] = append(g.edges[i], producer)
			g.cost++
		}
		sort.Ints(g.edges[i])
	}
	// Tarjan uses the small, admitted 64-node graph. A component is emitted
	// after every component it depends on.
	index := 0
	indices, low := make([]int, n), make([]int, n)
	onStack := make([]bool, n)
	stack := make([]int, 0, n)
	for i := range indices {
		indices[i] = -1
	}
	var components []graphComponent
	var visit func(int)
	visit = func(v int) {
		indices[v], low[v] = index, index
		index++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range g.edges[v] {
			if indices[w] == -1 {
				visit(w)
				if low[w] < low[v] {
					low[v] = low[w]
				}
			} else if onStack[w] && indices[w] < low[v] {
				low[v] = indices[w]
			}
		}
		if low[v] != indices[v] {
			return
		}
		component := graphComponent{}
		for {
			last := len(stack) - 1
			w := stack[last]
			stack = stack[:last]
			onStack[w] = false
			component.members = append(component.members, w)
			if w == v {
				break
			}
		}
		sort.Ints(component.members)
		component.cyclic = len(component.members) > 1
		if !component.cyclic {
			for _, w := range g.edges[v] {
				if w == v {
					component.cyclic = true
					break
				}
			}
		}
		components = append(components, component)
	}
	for i := 0; i < n; i++ {
		if indices[i] == -1 {
			visit(i)
		}
	}
	owner := make([]int, n)
	for i, component := range components {
		for _, node := range component.members {
			owner[node] = i
		}
	}
	// Deterministic Kahn order: choose the smallest original program index
	// among currently ready components.
	chosen := make([]bool, len(components))
	depths := make([]int, len(components))
	for len(g.order) < len(components) {
		pick := -1
		for i, component := range components {
			if chosen[i] {
				continue
			}
			ready := true
			for _, node := range component.members {
				for _, producer := range g.edges[node] {
					if owner[producer] != i && !chosen[owner[producer]] {
						ready = false
					}
				}
			}
			if ready && (pick < 0 || component.members[0] < components[pick].members[0]) {
				pick = i
			}
		}
		if pick < 0 {
			return dependencyGraph{}, ErrInvalidPlan
		}
		depth := 1
		for _, node := range components[pick].members {
			for _, producer := range g.edges[node] {
				if owner[producer] != pick && depths[owner[producer]]+1 > depth {
					depth = depths[owner[producer]] + 1
				}
			}
		}
		depths[pick] = depth
		component := components[pick]
		component.depth = depth
		g.order = append(g.order, component)
		chosen[pick] = true
		g.cost += len(component.members) + 1
	}
	return g, nil
}

func (g dependencyGraph) sameEdges(other dependencyGraph) bool {
	if len(g.edges) != len(other.edges) {
		return false
	}
	for i := range g.edges {
		if len(g.edges[i]) != len(other.edges[i]) {
			return false
		}
		for j := range g.edges[i] {
			if g.edges[i][j] != other.edges[i][j] {
				return false
			}
		}
	}
	return true
}
