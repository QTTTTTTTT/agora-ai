package lineage

// MemoryGraph is an in-memory Graph implementation used by tests and by
// callers that don't need durable persistence (small platforms, dry
// runs, lineage-graph previews before commit). It maintains both the
// edge set and the transitive closure incrementally so Ancestors /
// Descendants are O(|set|) lookups rather than recomputation.

import (
	"sync"
)

type MemoryGraph struct {
	mu sync.RWMutex
	// edges: child → set of direct parents
	parents map[string]map[string]struct{}
	// closure: descendant → set of ancestors at any depth >= 1
	ancestors map[string]map[string]struct{}
	// reverse closure: ancestor → set of descendants
	descendants map[string]map[string]struct{}
}

// NewMemoryGraph returns a ready-to-use empty graph.
func NewMemoryGraph() *MemoryGraph {
	return &MemoryGraph{
		parents:     make(map[string]map[string]struct{}),
		ancestors:   make(map[string]map[string]struct{}),
		descendants: make(map[string]map[string]struct{}),
	}
}

// Ancestors returns a copy of the ancestor set so callers can't mutate
// internal state.
func (g *MemoryGraph) Ancestors(agentID string) (map[string]struct{}, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return copySet(g.ancestors[agentID]), nil
}

// Descendants returns a copy of the descendant set.
func (g *MemoryGraph) Descendants(agentID string) (map[string]struct{}, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return copySet(g.descendants[agentID]), nil
}

// AddEdge appends a new child→parent edge and incrementally extends the
// closure. Returns *ErrCycle if the edge would create a cycle. Adding
// an edge that already exists is a no-op (idempotent).
func (g *MemoryGraph) AddEdge(e Edge) error {
	if err := e.Validate(); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	// Idempotency: if parent is already a direct parent of child, do
	// nothing.
	if direct, ok := g.parents[e.ChildAgentID]; ok {
		if _, exists := direct[e.ParentAgentID]; exists {
			return nil
		}
	}

	// Cycle check: if child is already an ancestor of parent, the new
	// edge would close a loop.
	if anc, ok := g.ancestors[e.ParentAgentID]; ok {
		if _, looped := anc[e.ChildAgentID]; looped {
			return &ErrCycle{
				ChildAgentID:  e.ChildAgentID,
				ParentAgentID: e.ParentAgentID,
				Path:          []string{e.ParentAgentID, e.ChildAgentID},
			}
		}
	}

	// Record the direct edge.
	addToSet(g.parents, e.ChildAgentID, e.ParentAgentID)

	// Closure update:
	//   ancestors of child   gains: parent ∪ ancestors(parent)
	//   descendants of parent gains: child  ∪ descendants(child)
	// Iterate cross-product so anyone formerly above parent now sees
	// child as a new descendant (and any descendant of child sees them
	// as a new ancestor).
	newAncestors := map[string]struct{}{e.ParentAgentID: {}}
	for a := range g.ancestors[e.ParentAgentID] {
		newAncestors[a] = struct{}{}
	}
	newDescendants := map[string]struct{}{e.ChildAgentID: {}}
	for d := range g.descendants[e.ChildAgentID] {
		newDescendants[d] = struct{}{}
	}

	for desc := range newDescendants {
		for anc := range newAncestors {
			if anc == desc {
				// Defensive: would imply a cycle that we should have
				// caught above. Skip rather than corrupt.
				continue
			}
			addToSet(g.ancestors, desc, anc)
			addToSet(g.descendants, anc, desc)
		}
	}

	return nil
}

// MapOwnerLookup is a trivial OwnerLookup backed by a static map. Tests
// use it; production wiring uses the agent_repo.GetByID owner column
// instead.
type MapOwnerLookup map[string]string

// OwnerOfAgent returns the registered owner, or the empty string if the
// agent is unknown — never an error in this simple implementation.
func (m MapOwnerLookup) OwnerOfAgent(agentID string) (string, error) {
	return m[agentID], nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func addToSet(m map[string]map[string]struct{}, key, value string) {
	s, ok := m[key]
	if !ok {
		s = make(map[string]struct{})
		m[key] = s
	}
	s[value] = struct{}{}
}

func copySet(s map[string]struct{}) map[string]struct{} {
	if len(s) == 0 {
		return map[string]struct{}{}
	}
	out := make(map[string]struct{}, len(s))
	for k := range s {
		out[k] = struct{}{}
	}
	return out
}
