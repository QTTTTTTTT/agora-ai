// Package votepenalty discounts cross-agent votes that are
// near-clones of one another, so the panel decision reflects
// the number of *independent* opinions rather than the raw
// vote count.
//
// MOTIVATION
// ----------
// The roundtable runs three or more analyst-style agents (bull,
// bear, quant, optionally sector-specialist). Today their
// votes are aggregated by simple count: "3 of 4 say BUY → buy
// with high confidence". When the LLM agents share the same
// upstream context block (recent news catalysts, same sector
// rotation table), they tend to converge on the same answer
// for the same reason. The panel looks like 3-of-4 but is
// really 1-of-1 with two echoes. Aggregating naively
// over-counts the signal — we get high "confidence" on
// decisions that were really one analyst speaking thrice.
//
// W2-12 introduces an effective-independence weighting:
//
//   * Each pair of agents (i, j) has a similarity score
//     sim_ij ∈ [0, 1]. Sim is computed from rationale text
//     overlap (Jaccard over rationale tokens) and structural
//     overlap (same direction, same conviction bucket, same
//     primary thesis tag).
//   * The effective weight of agent i is:
//        w_i = 1 / (1 + Σ_j≠i sim_ij^p)
//     for a power p (default 2). This shrinks each vote in
//     proportion to how many other agents are "saying the
//     same thing".
//   * The panel score is then Σ_i w_i * direction_i
//     (with direction in {-1, 0, +1}), clamped to [-1, 1].
//
// AUDITABILITY
// ------------
// The package is pure / deterministic. The wiring layer keeps
// the per-pair similarities in the audit trail so a reviewer
// can see why "3-of-4" got down-weighted to (say) 0.62.
//
// TEXT SIMILARITY
// ---------------
// The Jaccard overlap is computed from *content tokens* only:
// punctuation stripped, lowercased, stop-words removed. We do
// NOT use embeddings here — they would make the score
// non-deterministic across deploys and dependent on the
// embed-quota worker (which the W4-23 backpressure work
// reshapes).
//
// SCOPE
// -----
//   * Owns Vote, Penalty, Aggregate types.
//   * Does NOT own the panel orchestration; the wiring layer
//     gathers Vote inputs from the agent.RunInput results and
//     consumes the Aggregate score.
package votepenalty

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// Direction is the canonical agent verdict.
type Direction int

const (
	DirNeutral Direction = 0
	DirBuy     Direction = 1
	DirSell    Direction = -1
)

// Conviction is the agent's stated conviction bucket. Compared
// with the Direction it classifies the SHAPE of the vote so
// votes with same direction-and-conviction look more similar
// than same-direction-different-conviction.
type Conviction int

const (
	ConvictionLow Conviction = iota
	ConvictionMedium
	ConvictionHigh
)

// Vote is one agent's verdict on a single decision.
type Vote struct {
	AgentID     string
	Direction   Direction
	Conviction  Conviction
	Confidence  float64 // 0..1
	Rationale   string
	ThesisTags  []string // canonical tags, e.g. "earnings_beat", "macro_tailwind"
}

// Penalty is the per-agent effective weight after similarity
// shrinkage.
type Penalty struct {
	AgentID         string  `json:"agentId"`
	EffectiveWeight float64 `json:"effectiveWeight"`
	RawWeight       float64 `json:"rawWeight"`
	NeighbourSim    float64 `json:"neighbourSim"` // sum of sim_ij^p for j≠i
}

// PairSim is the similarity between two agents.
type PairSim struct {
	AgentA     string  `json:"agentA"`
	AgentB     string  `json:"agentB"`
	Similarity float64 `json:"similarity"`
	JaccardSim float64 `json:"jaccardSim"`
	StructSim  float64 `json:"structSim"`
}

// Aggregate is the panel-level result.
type Aggregate struct {
	Penalties     []Penalty `json:"penalties"`
	Pairs         []PairSim `json:"pairs"`
	PanelScore    float64   `json:"panelScore"`    // ∈ [-1, 1]
	PanelDir      Direction `json:"panelDirection"`
	EffectiveN    float64   `json:"effectiveN"`    // 1 / Σ w_i² , the "ENC"
	RawCount      int       `json:"rawCount"`
}

// Config tunes the penalty curve.
type Config struct {
	// SimPower is the exponent applied to each pairwise sim
	// when summing neighbours: w_i = 1 / (1 + Σ sim^p).
	// Larger = more aggressive penalty for highly similar
	// neighbours, milder for moderately similar ones.
	// Default 2.0.
	SimPower float64
	// JaccardWeight ∈ [0, 1] is the share of total similarity
	// attributed to text overlap. The remainder is structural
	// (direction + conviction + thesis tag overlap). Default 0.5.
	JaccardWeight float64
	// PanelDirThreshold is the |PanelScore| at which the
	// panel direction is recorded as Buy/Sell rather than
	// Neutral. Default 0.20.
	PanelDirThreshold float64
}

// DefaultConfig is the production-safe baseline.
func DefaultConfig() Config {
	return Config{
		SimPower:          2.0,
		JaccardWeight:     0.5,
		PanelDirThreshold: 0.20,
	}
}

// Compute runs the penalty algorithm. Pure: same inputs ↦
// same output, no time / RNG / I/O dependencies.
func Compute(votes []Vote, cfg Config) Aggregate {
	cfg = normalise(cfg)
	n := len(votes)
	out := Aggregate{RawCount: n}
	if n == 0 {
		return out
	}
	if n == 1 {
		w := 1.0
		if votes[0].Confidence > 0 {
			w = clamp01(votes[0].Confidence)
		}
		out.Penalties = []Penalty{{AgentID: votes[0].AgentID, EffectiveWeight: w, RawWeight: w}}
		out.PanelScore = w * float64(votes[0].Direction)
		out.PanelDir = directionFromScore(out.PanelScore, cfg.PanelDirThreshold)
		out.EffectiveN = 1
		return out
	}

	// Pairwise similarities.
	pairs := make([]PairSim, 0, n*(n-1)/2)
	tokens := make([][]string, n)
	for i := range votes {
		tokens[i] = tokenise(votes[i].Rationale)
	}

	neighbour := make([]float64, n)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			ja := jaccard(tokens[i], tokens[j])
			st := structuralSim(votes[i], votes[j])
			sim := cfg.JaccardWeight*ja + (1-cfg.JaccardWeight)*st
			pairs = append(pairs, PairSim{
				AgentA: votes[i].AgentID, AgentB: votes[j].AgentID,
				Similarity: sim, JaccardSim: ja, StructSim: st,
			})
			contribution := math.Pow(sim, cfg.SimPower)
			neighbour[i] += contribution
			neighbour[j] += contribution
		}
	}

	out.Pairs = pairs
	out.Penalties = make([]Penalty, n)
	totalEffective := 0.0
	for i, v := range votes {
		raw := 1.0
		if v.Confidence > 0 {
			raw = clamp01(v.Confidence)
		}
		eff := raw / (1.0 + neighbour[i])
		out.Penalties[i] = Penalty{
			AgentID:         v.AgentID,
			EffectiveWeight: eff,
			RawWeight:       raw,
			NeighbourSim:    neighbour[i],
		}
		totalEffective += eff
	}

	// PanelScore: weighted directional sum, then clamped.
	score := 0.0
	for i, v := range votes {
		score += out.Penalties[i].EffectiveWeight * float64(v.Direction)
	}
	if totalEffective > 0 {
		score /= totalEffective
	}
	if score > 1 {
		score = 1
	}
	if score < -1 {
		score = -1
	}
	out.PanelScore = score
	out.PanelDir = directionFromScore(score, cfg.PanelDirThreshold)

	// Effective number of independent agents.
	//
	// ENC = N / (1 + (N-1) * avg_pairwise_sim)
	//
	// We deliberately do NOT use the more common (Σw)²/Σw²
	// concentration formula — it is invariant to "three equal
	// clones vs three equal independents" because both have the
	// same weight distribution. The similarity-aware formula
	// here collapses N near-clones to ENC ≈ 1 and leaves N
	// independents at ENC ≈ N, which matches the operator's
	// intuition: "panel of three clones counts as one analyst".
	if len(pairs) > 0 {
		sumSim := 0.0
		for _, p := range pairs {
			sumSim += p.Similarity
		}
		avgSim := sumSim / float64(len(pairs))
		denom := 1.0 + float64(n-1)*avgSim
		if denom <= 0 {
			denom = 1
		}
		out.EffectiveN = float64(n) / denom
	} else {
		out.EffectiveN = float64(n)
	}
	return out
}

// directionFromScore quantises the score back to a Direction
// using the threshold band.
func directionFromScore(score, threshold float64) Direction {
	if score > threshold {
		return DirBuy
	}
	if score < -threshold {
		return DirSell
	}
	return DirNeutral
}

func structuralSim(a, b Vote) float64 {
	// Weighting note: direction-match alone is *not* a strong
	// independence signal. Three honest analysts with very
	// different reasoning will often arrive at the same buy /
	// sell verdict — the panel intent is precisely to surface
	// such convergence. We therefore weight rationale-tag
	// overlap heavier than raw direction match. Three clones
	// with same tags and same rationale still saturate to 1.0
	// once Jaccard joins in (in the caller's mix).
	sim := 0.0
	if a.Direction == b.Direction {
		sim += 0.25
	}
	if a.Conviction == b.Conviction {
		sim += 0.10
	}
	tagSim := jaccard(normaliseTags(a.ThesisTags), normaliseTags(b.ThesisTags))
	sim += 0.65 * tagSim
	if sim > 1 {
		sim = 1
	}
	return sim
}

func normaliseTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		clean := strings.TrimSpace(strings.ToLower(t))
		if clean != "" {
			out = append(out, clean)
		}
	}
	sort.Strings(out)
	return out
}

func tokenise(text string) []string {
	if text == "" {
		return nil
	}
	lower := strings.ToLower(text)
	fields := strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(fields))
	for _, w := range fields {
		if len(w) < 3 {
			continue // strip stop-word-ish glue
		}
		if _, stop := stopWords[w]; stop {
			continue
		}
		out = append(out, w)
	}
	return out
}

var stopWords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "with": {}, "this": {}, "that": {},
	"from": {}, "into": {}, "than": {}, "but": {}, "are": {}, "has": {},
	"have": {}, "had": {}, "was": {}, "were": {}, "will": {}, "would": {},
	"because": {}, "though": {}, "while": {}, "due": {},
}

func jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	setA := make(map[string]struct{}, len(a))
	for _, t := range a {
		setA[t] = struct{}{}
	}
	setB := make(map[string]struct{}, len(b))
	for _, t := range b {
		setB[t] = struct{}{}
	}
	intersect := 0
	for t := range setA {
		if _, ok := setB[t]; ok {
			intersect++
		}
	}
	union := len(setA) + len(setB) - intersect
	if union == 0 {
		return 0
	}
	return float64(intersect) / float64(union)
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func normalise(cfg Config) Config {
	d := DefaultConfig()
	if cfg.SimPower <= 0 {
		cfg.SimPower = d.SimPower
	}
	if cfg.JaccardWeight < 0 || cfg.JaccardWeight > 1 {
		cfg.JaccardWeight = d.JaccardWeight
	}
	if cfg.PanelDirThreshold < 0 || cfg.PanelDirThreshold > 1 {
		cfg.PanelDirThreshold = d.PanelDirThreshold
	}
	return cfg
}
