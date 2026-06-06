// Package signalbudget caps the prompt context at top-K most
// relevant signal blocks, with a fallback chain when the
// nominal budget is too tight.
//
// MOTIVATION
// ----------
// The DecisionInput grew organically: roundtable stance, bull /
// bear / quant cases, fundamentals, sector rotation, news
// sentiment, sleeve scorecard, lesson replay, instrument hints,
// quant snapshots, universe ranking, three factor scores, PEAD,
// cooldowns, risk budget, news catalysts, earnings calendar,
// exposure, correlations, pair spreads, agent skills, recent
// lessons, long-term reflections, macro briefing — north of 25
// blocks. We were stuffing everything we had into every prompt.
//
// Two costs we now pay for the kitchen-sink approach:
//
//   1. Token cost. A "fully loaded" decision is 18-22k input
//      tokens. Most blocks contribute marginal alpha but full
//      token weight.
//   2. Signal-to-noise. Dilution: lessons that were highly
//      relevant get drowned in pages of low-information blocks
//      the LLM has to skim before reaching them. Calibration
//      experiments at 8-10 blocks consistently beat 20+ on
//      thesis-quality and confidence-realisation correlation.
//
// W2-10 introduces a budgeter that picks the top-K relevant
// blocks for a given decision. Blocks NOT in the top-K are
// dropped from the prompt. The budget is policy-tuned, not
// hard-coded.
//
// RELEVANCE SCORE
// ---------------
// score(block) = static_priority(block)
//              + dynamic_density(block)
//              + historical_alpha(block) * alpha_weight
//
// static_priority comes from BlockPriority. Higher = always
// included if present. Three tiers:
//
//   * Tier-1 (always-on): roundtableStance, bullCase, bearCase,
//     quantCase, riskBudget, exposure (when breached).
//     These are core decision substrate — dropping them changes
//     the kind of decision being asked for.
//   * Tier-2 (high-relevance-when-present): symbolVerdicts,
//     instrumentHints, universeRanking, agentSkills,
//     recentLessons, newsCatalysts, earningsCalendar.
//   * Tier-3 (top-up if budget remains): fundamentalSummary,
//     sectorRotation, newsSentiment, sleeveScorecard,
//     macroBriefing, qualityScores, valueScores, lowBetaScores,
//     pead, cooldowns, correlations, pairSpreads,
//     longTermReflections, lessonReplay, quantSnapshots.
//
// dynamic_density comes from BlockDensity — typically a row
// count or "this block has actionable content right now"
// indicator. The wiring layer fills it in from the trace's
// Counts struct.
//
// historical_alpha (optional): the empirical effect on
// realised alpha when this block was present versus absent,
// measured over the W1-4/W1-5 provenance × outcome join. The
// caller supplies this as a map; absent entries are treated
// as zero (i.e. no historical signal one way or the other).
//
// FALLBACK CHAIN
// --------------
// When TargetK is too tight to admit all Tier-1 blocks, the
// budgeter ALWAYS keeps Tier-1 — the policy chooses
// "contextual truth" over "token cost". The TargetK is treated
// as a soft target, not a hard maximum, when keeping it would
// drop a tier-1 block. The result reports this overflow so the
// caller can log / alert on chronic budget pressure.
//
// SCOPE
// -----
//   * Owns the BlockTier enum, BlockPriority/BlockDensity types,
//     SelectionPolicy and Result.
//   * Does NOT mutate DecisionInput. The caller takes the
//     Selected list and elides absent blocks at prompt-build
//     time.
package signalbudget

import (
	"math"
	"sort"
)

// BlockTier classifies a signal block by how much we trust its
// importance a priori.
type BlockTier int

const (
	TierUnknown BlockTier = iota
	Tier1AlwaysOn
	Tier2HighRelevance
	Tier3TopUp
)

// BlockPriority maps the canonical signal-block names (matching
// the strings returned by decision.Trace.PresentBlocks) to
// their tier and a static priority score within the tier.
//
// Mutating this table is a deliberate design change: it shifts
// the "what does the LLM always see?" contract.
var BlockPriority = map[string]struct {
	Tier  BlockTier
	Score float64
}{
	// Tier-1 — always included if present.
	"roundtableStance": {Tier1AlwaysOn, 100},
	"riskBudget":       {Tier1AlwaysOn, 95},
	"bullCase":         {Tier1AlwaysOn, 90},
	"bearCase":         {Tier1AlwaysOn, 90},
	"quantCase":        {Tier1AlwaysOn, 88},
	"exposure":         {Tier1AlwaysOn, 92},

	// Tier-2 — high relevance when populated.
	"symbolVerdicts":   {Tier2HighRelevance, 80},
	"instrumentHints":  {Tier2HighRelevance, 78},
	"universeRanking":  {Tier2HighRelevance, 76},
	"newsCatalysts":    {Tier2HighRelevance, 75},
	"earningsCalendar": {Tier2HighRelevance, 74},
	"agentSkills":      {Tier2HighRelevance, 72},
	"recentLessons":    {Tier2HighRelevance, 70},

	// Tier-3 — included when budget allows.
	"fundamentalSummary":  {Tier3TopUp, 60},
	"sectorRotation":      {Tier3TopUp, 58},
	"sleeveScorecard":     {Tier3TopUp, 56},
	"macroBriefing":       {Tier3TopUp, 55},
	"newsSentiment":       {Tier3TopUp, 50},
	"qualityScores":       {Tier3TopUp, 48},
	"valueScores":         {Tier3TopUp, 46},
	"lowBetaScores":       {Tier3TopUp, 44},
	"pead":                {Tier3TopUp, 42},
	"cooldowns":           {Tier3TopUp, 40},
	"correlations":        {Tier3TopUp, 38},
	"pairSpreads":         {Tier3TopUp, 36},
	"lessonReplay":        {Tier3TopUp, 34},
	"longTermReflections": {Tier3TopUp, 32},
	"quantSnapshots":      {Tier3TopUp, 30},
}

// SelectionPolicy controls how aggressively the budgeter trims.
type SelectionPolicy struct {
	// TargetK is the soft target number of blocks to include.
	// Tier-1 always-on blocks may overflow this.
	// 0 means "use default of 10".
	TargetK int
	// AlphaWeight scales the historical-alpha contribution.
	// Set 0 to ignore historical alpha entirely; default 25.
	AlphaWeight float64
	// DensityWeight scales BlockDensity. Higher means denser
	// blocks crowd out less-populated peers. Default 0.5.
	DensityWeight float64
}

// DefaultPolicy is the production-safe baseline.
func DefaultPolicy() SelectionPolicy {
	return SelectionPolicy{
		TargetK:       10,
		AlphaWeight:   25,
		DensityWeight: 0.5,
	}
}

// Selection is the per-block decision the budgeter emits.
type Selection struct {
	Block    string    `json:"block"`
	Tier     BlockTier `json:"tier"`
	Score    float64   `json:"score"`
	Density  float64   `json:"density"`
	AlphaPP  float64   `json:"alphaPP"`  // historical alpha contribution
	Selected bool      `json:"selected"`
	Reason   string    `json:"reason"`
}

// Result is the budgeter's verdict.
type Result struct {
	Policy           SelectionPolicy `json:"policy"`
	Present          int             `json:"present"`
	Selected         []Selection     `json:"selected"`
	Dropped          []Selection     `json:"dropped"`
	OverflowOverK    bool            `json:"overflowOverK"`
	OverflowReason   string          `json:"overflowReason,omitempty"`
}

// SelectedBlocks is the convenient flat view of Result.Selected:
// just the block names, in the same order.
func (r Result) SelectedBlocks() []string {
	out := make([]string, 0, len(r.Selected))
	for _, s := range r.Selected {
		if s.Selected {
			out = append(out, s.Block)
		}
	}
	return out
}

// Inputs is the budgeter's per-call payload.
type Inputs struct {
	// PresentBlocks is the order-stable block list from
	// decision.Trace.PresentBlocks().
	PresentBlocks []string
	// Density maps block name → density signal (typically a
	// row count, normalised by a soft cap inside the budgeter).
	Density map[string]float64
	// HistoricalAlpha maps block name → mean realised alpha
	// when this block was present, in fraction units. Negative
	// values mean "this block hurts on average". Optional.
	HistoricalAlpha map[string]float64
}

// Select computes the budget for one decision input.
func Select(in Inputs, policy SelectionPolicy) Result {
	policy = normalisePolicy(policy)
	res := Result{Policy: policy, Present: len(in.PresentBlocks)}

	type scored struct {
		Selection
	}
	candidates := make([]scored, 0, len(in.PresentBlocks))
	for _, name := range in.PresentBlocks {
		meta, ok := BlockPriority[name]
		tier := meta.Tier
		base := meta.Score
		if !ok {
			tier = TierUnknown
			base = 20 // unknown blocks fall to the bottom
		}
		density := normaliseDensity(in.Density[name])
		alpha := in.HistoricalAlpha[name]
		score := base + density*policy.DensityWeight + alpha*policy.AlphaWeight
		candidates = append(candidates, scored{Selection{
			Block:   name,
			Tier:    tier,
			Score:   score,
			Density: density,
			AlphaPP: alpha,
		}})
	}

	// Sort: tier 1 first, then by descending score, then by
	// block name for determinism.
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Tier != b.Tier {
			return tierRank(a.Tier) < tierRank(b.Tier)
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return a.Block < b.Block
	})

	// Walk in order. Always keep tier-1; for tier-2/3, stop
	// once we've hit the target K.
	selected := make([]Selection, 0, len(candidates))
	dropped := make([]Selection, 0)
	keptCount := 0
	for _, c := range candidates {
		take := false
		reason := ""
		switch {
		case c.Tier == Tier1AlwaysOn:
			take = true
			reason = "tier1_always_on"
		case keptCount < policy.TargetK:
			take = true
			reason = "within_budget"
		default:
			reason = "below_budget_cutoff"
		}
		s := c.Selection
		s.Reason = reason
		if take {
			s.Selected = true
			selected = append(selected, s)
			keptCount++
		} else {
			s.Selected = false
			dropped = append(dropped, s)
		}
	}

	res.Selected = selected
	res.Dropped = dropped
	if len(selected) > policy.TargetK {
		res.OverflowOverK = true
		res.OverflowReason = "tier1_overflow"
	}
	return res
}

func tierRank(t BlockTier) int {
	switch t {
	case Tier1AlwaysOn:
		return 0
	case Tier2HighRelevance:
		return 1
	case Tier3TopUp:
		return 2
	default:
		return 3
	}
}

// normaliseDensity squashes raw densities (often counts up to
// hundreds) into a [0, 1] range using a saturating curve. Keeps
// score additions comparable across very-dense and sparse
// blocks. The "10" inflection point is empirical: most blocks
// stop adding signal past ~10 items.
func normaliseDensity(v float64) float64 {
	if v <= 0 || math.IsNaN(v) {
		return 0
	}
	return v / (v + 10)
}

func normalisePolicy(p SelectionPolicy) SelectionPolicy {
	if p.TargetK <= 0 {
		p.TargetK = DefaultPolicy().TargetK
	}
	if p.AlphaWeight < 0 {
		p.AlphaWeight = 0
	}
	if p.DensityWeight < 0 {
		p.DensityWeight = 0
	}
	return p
}

// FallbackBlocks returns the canonical block list to keep when
// the caller has no Trace at all (e.g. an early-stage failure
// before signals were gathered). These are the bare minimum
// blocks the LLM needs to produce a coherent (if conservative)
// recommendation.
func FallbackBlocks() []string {
	return []string{"roundtableStance", "riskBudget", "exposure"}
}
