// Package lineage models the derivation graph between agents.
//
// Every time the marketplace clones an agent (buyout) or subscribes one
// account to another's agent (subscribe), an edge child→parent is added
// to the lineage graph. The platform uses this graph for two purposes:
//
//  1. Provenance: "where did this agent come from?" — surfacing an
//     agent's full ancestry on its profile page, so buyers see whether
//     they are looking at original work or a derivative.
//
//  2. Anti-matryoshka: refusing listings that would launder a
//     previously-bought agent back to its upstream owner. Without this
//     check, A → buys B's agent → trivially modifies → lists derivative
//     → B buys it back → A repeats. The cycle inflates platform GMV
//     while delivering nothing of value.
//
// This package owns the *algorithms* (cycle detection, ancestor
// computation, edge/closure maintenance) as pure functions over an
// abstract Graph interface. The SQL-backed implementation lives in
// internal/repository — that is the only place agent_lineage and
// agent_lineage_closure tables get touched.

package lineage

import (
	"errors"
	"fmt"
)

// DerivedVia classifies how one agent came from another.
type DerivedVia string

const (
	ViaBuyout      DerivedVia = "buyout"
	ViaSubscribe   DerivedVia = "subscribe"
	ViaABTestClone DerivedVia = "abtest_clone"
	ViaManualCopy  DerivedVia = "manual_copy"
)

// IsValid reports whether the value is one of the recognised derivation
// kinds.
func (d DerivedVia) IsValid() bool {
	switch d {
	case ViaBuyout, ViaSubscribe, ViaABTestClone, ViaManualCopy:
		return true
	}
	return false
}

// Edge is a child→parent derivation event. The child is the newly
// created agent; the parent is the one it was derived from.
type Edge struct {
	ChildAgentID  string
	ParentAgentID string
	Via           DerivedVia
	// SourceListingID and SourceSubscriptionID are optional citations
	// back to the marketplace event that produced this edge. Both empty
	// means "out-of-band" derivation (manual copy, abtest, etc.).
	SourceListingID      string
	SourceSubscriptionID string
}

// Validate checks structural well-formedness of the edge — it does NOT
// consult the graph for cycles. CycleCheck does that.
func (e Edge) Validate() error {
	if e.ChildAgentID == "" {
		return errors.New("lineage: child agent id required")
	}
	if e.ParentAgentID == "" {
		return errors.New("lineage: parent agent id required")
	}
	if e.ChildAgentID == e.ParentAgentID {
		return errors.New("lineage: child cannot equal parent")
	}
	if !e.Via.IsValid() {
		return fmt.Errorf("lineage: invalid derived_via: %q", e.Via)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Graph interface
// ---------------------------------------------------------------------------

// Graph abstracts the storage backing the lineage graph. SQL-backed
// implementations should provide constant-ish lookups via the closure
// table; in-memory implementations (used in tests and small platforms)
// can compute on the fly.
//
// Two query primitives:
//
//   - Ancestors(id): the full set of agents that id is derived from,
//     directly or transitively. Excludes id itself.
//   - Descendants(id): the inverse.
//
// The mutation primitive AddEdge atomically appends an edge AND extends
// the closure with all newly-implied (ancestor, descendant) pairs. It
// must reject edges that would create a cycle.
type Graph interface {
	Ancestors(agentID string) (map[string]struct{}, error)
	Descendants(agentID string) (map[string]struct{}, error)
	AddEdge(e Edge) error
}

// ---------------------------------------------------------------------------
// Cycle detection
// ---------------------------------------------------------------------------

// ErrCycle is returned when adding an edge would create a cycle.
type ErrCycle struct {
	ChildAgentID  string
	ParentAgentID string
	// Path is the discovered ancestry chain that proves the cycle:
	// parent → ... → child. Empty when the cycle is the trivial
	// self-edge (child==parent), which Validate already rejects.
	Path []string
}

func (e *ErrCycle) Error() string {
	if len(e.Path) > 0 {
		return fmt.Sprintf(
			"lineage: cycle would form between child=%s and parent=%s (path=%v)",
			e.ChildAgentID, e.ParentAgentID, e.Path,
		)
	}
	return fmt.Sprintf(
		"lineage: cycle would form between child=%s and parent=%s",
		e.ChildAgentID, e.ParentAgentID,
	)
}

// CheckNoCycle returns nil if appending the edge would keep the graph
// acyclic, otherwise an *ErrCycle describing the would-be cycle.
//
// The rule: an edge child→parent creates a cycle iff `child` is already
// an ancestor of `parent`. Equivalently: parent is a descendant of
// child. We query Descendants(child) and look for parent.
//
// We also reject the case where child already has parent as an ancestor
// (duplicate edge) — not a cycle, but adding it implies a redundancy
// that callers usually want to avoid. To keep semantics tight we only
// enforce cycles here; duplicate detection is out of scope.
func CheckNoCycle(g Graph, e Edge) error {
	if err := e.Validate(); err != nil {
		return err
	}
	descendants, err := g.Descendants(e.ChildAgentID)
	if err != nil {
		return fmt.Errorf("lineage: descendants lookup: %w", err)
	}
	if _, ok := descendants[e.ParentAgentID]; ok {
		return &ErrCycle{
			ChildAgentID:  e.ChildAgentID,
			ParentAgentID: e.ParentAgentID,
			Path:          []string{e.ParentAgentID, e.ChildAgentID},
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Anti-matryoshka check
// ---------------------------------------------------------------------------

// MatryoshkaCheck inspects whether listing the given agent would create
// a "laundering" loop: i.e. the seller's agent (or any of its ancestors)
// was originally derived from an agent now owned by a candidate buyer.
//
// More precisely, returns an error if any ancestor of `sellerAgentID`
// belongs to `forbiddenOwners` — the set of users who must not be able
// to repurchase this lineage's derivative.
//
// The owner-of-agent lookup is delegated to OwnerLookup so this package
// stays storage-agnostic.
type OwnerLookup interface {
	OwnerOfAgent(agentID string) (userID string, err error)
}

// ErrMatryoshka is returned when listing/purchase would create a
// matryoshka loop.
type ErrMatryoshka struct {
	SellerAgentID  string
	OffendingAgent string // ancestor that traces back to forbidden owner
	OffendingOwner string
}

func (e *ErrMatryoshka) Error() string {
	return fmt.Sprintf(
		"lineage: matryoshka loop — agent %s descends from agent %s owned by %s",
		e.SellerAgentID, e.OffendingAgent, e.OffendingOwner,
	)
}

// CheckNoMatryoshka returns nil if no ancestor of sellerAgentID is owned
// by anyone in forbiddenOwners.
//
// Typical usage at CreateListing time:
//
//	forbidden := {buyer-of-record from any prior order whose
//	              delivered agent appears in this lineage}
//	CheckNoMatryoshka(graph, owner, sellerAgent, forbidden)
//
// PurchaseListing should call it with forbiddenOwners = {prospective
// buyer} so a buyer cannot repurchase a descendant of an agent they
// themselves authored.
func CheckNoMatryoshka(g Graph, owner OwnerLookup, sellerAgentID string, forbiddenOwners map[string]struct{}) error {
	if sellerAgentID == "" {
		return errors.New("lineage: seller agent id required")
	}
	if len(forbiddenOwners) == 0 {
		return nil
	}
	ancestors, err := g.Ancestors(sellerAgentID)
	if err != nil {
		return fmt.Errorf("lineage: ancestors lookup: %w", err)
	}
	// The seller's own agent should also be checked: if the seller is
	// listing an agent they don't actually own (clone+rename trick),
	// that's caught by upstream auth, so here we just walk strict
	// ancestors.
	for ancestorID := range ancestors {
		ownerID, err := owner.OwnerOfAgent(ancestorID)
		if err != nil {
			return fmt.Errorf("lineage: owner lookup for %s: %w", ancestorID, err)
		}
		if _, forbidden := forbiddenOwners[ownerID]; forbidden {
			return &ErrMatryoshka{
				SellerAgentID:  sellerAgentID,
				OffendingAgent: ancestorID,
				OffendingOwner: ownerID,
			}
		}
	}
	return nil
}
