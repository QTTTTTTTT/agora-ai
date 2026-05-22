// Package memory implements the structured memory layer for fund agents:
// importance scoring, similarity-based recall, and reflexion (lesson
// distillation) — the three pieces that the legacy memories table was
// missing.
//
// The package is deliberately persistence-agnostic. Callers feed it value
// structs (Item, ScoredItem, Embedding) and receive computed scores, ranked
// recalls, and distilled reflections; how the results are stored (pgvector,
// JSONB, plain Postgres rows) is a wiring concern.
//
// Why this matters
//
// Before this PR the only "memory" the system had was an unindexed text
// table queried by ILIKE. PMAgent prompts contained an empty MemoryContext
// because there was no recall mechanism to populate it. Reflexion-style
// long-term lessons were a planned-but-never-written `long_term` layer.
// This package gives the wiring layer a small, well-tested set of building
// blocks to fix that:
//
//   - Embedding cosine math + zero-dependency in-process embedding fallback
//     so unit tests run without an LLM.
//   - Importance scoring: a deterministic formula combining tag salience,
//     daily return magnitude, and an LLM-rated score (when available).
//   - Recall: recency × importance × similarity ranking in the spirit of
//     the Generative Agents paper.
//   - Reflexion: tiny shape that lets a job take N raw memories and group
//     them into bucketed, distilled lessons.
package memory

import (
	"math"
	"time"
)

// Embedding is a fixed-dimension vector used for similarity search.
// It is stored in Postgres via pgvector; this package operates on plain
// []float32 so it has no DB dependency.
type Embedding []float32

// Cosine returns the cosine similarity between a and b in [-1, 1]. Returns
// 0 when either vector has zero magnitude or unequal length.
func Cosine(a, b Embedding) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	denom := math.Sqrt(na * nb)
	if denom == 0 {
		return 0
	}
	return dot / denom
}

// Item is a memory record as far as this package is concerned. The Embedding
// may be nil when the memory was created before embeddings were available;
// recall falls back to importance/recency only in that case.
type Item struct {
	ID              string
	FundID          string
	AgentID         string
	OwnerUserID     string
	Visibility      string // private / fund / marketplace
	Sensitivity     string // public / internal / secret
	OriginKind      string // native / imported_from_marketplace
	SourceListingID string
	Layer           string // raw / daily / agent / analysis / long_term / reflection
	Kind            string // raw / lesson / reflection / summary
	Title           string
	Content         string
	Tags            []string
	Importance      float64 // [0, 1]
	AccessCount     int
	CreatedAt       time.Time
	LastAccessedAt  time.Time
	Embedding       Embedding
	ParentID        string // for reflections, points back to the source memory
}

// Policy interface defines access control rules for memory retrieval.
type Policy interface {
	CanRecall(actorUserID string, item Item) bool
}

// DefaultPolicy is a simple implementation of Policy.
type DefaultPolicy struct{}

func (p DefaultPolicy) CanRecall(actorUserID string, item Item) bool {
	// 1. If no owner is set or actor matches owner, allow
	if item.OwnerUserID == "" || item.OwnerUserID == actorUserID {
		return true
	}
	// 2. If it's a secret, only the owner can see it
	if item.Sensitivity == "secret" {
		return false
	}
	// 3. Otherwise, check visibility
	if item.Visibility == "marketplace" {
		return true
	}
	if item.Visibility == "fund" {
		// In a real implementation, we'd check if actorUserID is in the fund
		// For now, we assume this is handled at a higher level or return false
		return false
	}
	return false
}

// ScoredItem augments Item with the per-component recall scores.
type ScoredItem struct {
	Item
	Recency    float64 // [0, 1]
	Importance float64 // [0, 1]
	Similarity float64 // [-1, 1]; 0 when no embedding
	Score      float64 // weighted combination
}
