// Package decision is the seam between the PMAgent and the LLM-driven
// portfolio-decision logic introduced in Phase 2A of the auto-execute
// + decision overhaul. The PMAgent no longer hardcodes if/else for
// "buy the first universe symbol vs reduce the first held position".
// Instead it calls a DecisionEngine that consumes the current
// portfolio state, the roundtable consensus, and a set of market
// signals, and returns a structured list of intended actions plus a
// plan-level confidence score.
//
// This package intentionally does NOT import server/cmd/server or
// server/internal/repository — it sits below the wiring layer so the
// fallback adapter in wiring_adapters.go can lazily import this
// package without creating a cycle. The wiring code converts
// repository.HoldingPosition / repository.Fund into the lightweight
// types here, calls Decide, and translates DecisionAction back into
// repository.PlanAction with all the lot-size / T+1 / sellable-today
// safety normalizations.
//
// Future phases (2B / 2C / 2D) will inject richer signals into
// DecisionInput (technical indicators, fundamental snapshots, sector
// flow, multi-agent debate output). The Engine interface is wide
// enough to absorb those without a breaking change because all of
// them go through the optional Signals map / Reports slice.
package decision

import (
	"context"
	"time"
)

// DecisionInput is the full context handed to the engine for one
// decision call. Every field is optional from the engine's
// perspective; an empty DecisionInput is a degenerate case that the
// engine handles by emitting a "watch" / hold-everything plan rather
// than crashing.
type DecisionInput struct {
	// FundID and TradingDate identify the call; the engine may log
	// them or include them in the prompt for traceability.
	FundID      string
	TradingDate time.Time

	// Market context. Market is the canonical lowercase tag (a_share /
	// us_equity / crypto / futures / hk_stock). BaseCurrency,
	// PrimaryDirection, and Benchmark surface portfolio-level
	// constraints so the LLM doesn't propose, e.g., a JPY-denominated
	// trade in a USD fund.
	Market           string
	BaseCurrency     string
	PrimaryDirection string
	Benchmark        string

	// Portfolio snapshot. TotalAssets is NAV in BaseCurrency. The
	// engine uses it together with the per-fund risk policy to bound
	// recommended trade notionals.
	TotalAssets   float64
	AvailableCash float64
	Positions     []DecisionPosition

	// Candidate universe + per-symbol hints. Universe is the ordered
	// list the operator configured under fund.config.universe.symbols
	// (or inherited from team specialization). InstrumentHints carry
	// market/exchange/asset-class metadata so the engine can pick a
	// reasonable default for first-time buys without re-classifying.
	Universe        []string
	InstrumentHints map[string]InstrumentHint

	// Soft inputs from the rest of the workflow.
	RoundtableConsensus []string // bullet points produced by RoundtableResult.Consensus
	MacroBriefing       string   // raw macro analyst output (one paragraph)
	StockReports        []string // optional per-symbol research blurbs

	// Phase 2B roundtable enrichment. Populated when the debate
	// orchestrator ran (fund.config.researchTier == "advanced");
	// empty when the legacy text-concat consensus produced the
	// roundtable. The LLMDecisionEngine consumes these so the PM
	// sees the bull/bear cases and the per-symbol verdicts, not
	// just a flat bullet list. The fallback engine ignores them
	// (it operates on RoundtableConsensus only).
	RoundtableStance    string                   // one-line overall stance from the debate
	BullCase            string                   // bull researcher's locked stance
	BearCase            string                   // bear researcher's locked stance
	QuantCase           string                   // quant researcher's locked stance
	SymbolVerdicts      []RoundtableSymbolVerdict

	// Phase 2D macro / valuation / sentiment enrichment. Each is a
	// preformatted multi-line string the LLMDecisionEngine pastes
	// verbatim into the prompt under a labelled section. Empty
	// strings cause the section to be omitted from the prompt — so
	// deployments without fundamentals / sentiment / sector flow
	// providers still see a clean prompt.
	//
	// FundamentalSummary: one line per symbol with PE/PB/margins/
	// growth. Lets the PM weigh "stretched valuation" vs "earnings
	// re-acceleration" with concrete numbers.
	//
	// SectorRotation: top/bottom sector returns + (where available)
	// money-flow net inflows. Lets the PM avoid sizing into a
	// sector that's bleeding capital across the board.
	//
	// NewsSentiment: market-level mood + per-symbol bull/bear bias
	// with the loudest catalysts. Lets the PM raise the bar for a
	// symbol whose sentiment opposes the debate stance.
	FundamentalSummary string
	SectorRotation     string
	NewsSentiment      string

	// Phase 3A-7 attribution scorecard. A multi-line block
	// rendered upstream by attribution.BuildPromptScorecard that
	// surfaces the top winning + top losing (sleeve, regime)
	// cells the fund has closed in the recent window. The LLM
	// PM uses it as a soft prior — "this combination has paid
	// off / bled money in the past, weigh accordingly". The
	// hard mute set is still maintained inside strategy.Service
	// (Phase 3A-5); the scorecard is the LLM's *advisory*
	// channel into the same feedback loop. Empty when the fund
	// has no closed lots, when no row meets the sample-size
	// floor, or when attribution isn't wired in.
	SleeveScorecard string

	// Phase 3A-10 lesson replay. A multi-line block rendered
	// upstream by attribution.BuildLessonReplay that paraphrases
	// the most-actionable recent attribution memories (the same
	// rows the AgentLearning dashboard surfaces). Where
	// SleeveScorecard is the structured numeric channel,
	// LessonReplay is the textual channel — short sentences
	// like "Sleeve trend is losing money in regime chop;
	// consider pausing the combination". The LLM PM treats it
	// as a soft prior alongside the scorecard; see the system
	// prompt for the exact reading rules. Empty when no
	// attribution memories exist or none survive the dedup +
	// lookback filter.
	LessonReplay string

	// Operational constraints.
	BuyBudget float64  // max absolute buy notional this plan can propose; 0 = no cap
	RiskNotes []string // contextual notes (e.g., "A-share T+1 active") that the engine must respect

	// Now lets tests freeze the engine's "today". nil = use time.Now().
	Now time.Time
}

// DecisionPosition is the engine-facing view of a single holding. Use
// it to seed the system prompt with positions and to detect which
// symbols already have exposure (so e.g. the engine prefers "reduce"
// over "sell-all" when proposing de-risking moves).
type DecisionPosition struct {
	Symbol        string
	InstrumentKey string
	Market        string
	Exchange      string
	AssetClass    string
	Quantity      float64
	AvailableQty  float64 // sellable today after T+1 lock; for T+0 markets equals Quantity
	CurrentPrice  float64
	CostPrice     float64
	UnrealizedPnL float64
}

// InstrumentHint is a fragment of metadata the wiring layer already
// has (from repository / marketdata) and forwards to the engine so it
// can write better prompts without re-deriving everything.
type InstrumentHint struct {
	Market         string
	Exchange       string
	AssetClass     string
	InstrumentType string
	QuoteCurrency  string
}

// RoundtableSymbolVerdict mirrors workflow.RoundtableSymbolVerdict so
// the decision package stays free of a circular import on workflow.
// Verdict is one of "bull"/"bear"/"neutral". DissentVotes counts the
// agents that voted against Verdict — the LLMDecisionEngine prompt
// surfaces high-dissent symbols to the PM so it can demand higher
// confidence before sizing into them.
type RoundtableSymbolVerdict struct {
	Symbol       string
	Verdict      string
	BullCase     string
	BearCase     string
	QuantCase    string
	DissentVotes int
}

// DecisionAction is the engine's recommendation for a single
// instrument. The wiring layer is responsible for translating this
// into repository.PlanAction, applying lot-size + sellable-today
// normalisation, and stamping the "selected by LLMDecisionEngine"
// audit reasoning.
//
// QtyPct semantics:
//   - For buy/add: fraction of NAV (0.05 = 5% of total assets) to
//     allocate; the wiring layer divides by live price and normalises
//     to the appropriate lot size.
//   - For reduce: fraction of the *current position* to sell. 1.0
//     means "sell all sellable today". 0.5 means "sell half".
//   - For sell: ignored (the wiring layer sells the full AvailableQty).
//   - For hold/watch: ignored.
//
// Confidence is the engine's confidence in this single action, in
// [0,1]. Plan-level confidence (DecisionOutput.Confidence) is the
// aggregate the auto-execute gate compares against the configured
// floor.
type DecisionAction struct {
	Symbol     string
	Action     string // "buy" | "sell" | "hold" | "reduce" | "add" | "watch"
	QtyPct     float64
	Reasoning  string
	Confidence float64
}

// DecisionOutput is the structured result of one Decide call. The
// engine is allowed to return zero actions (e.g. "the market is
// uninvestable today, stand pat") in which case the wiring layer
// emits a single "watch" PlanAction so the workflow still completes.
type DecisionOutput struct {
	Actions    []DecisionAction
	Confidence float64 // plan-level confidence, 0..1
	Stance     string  // optional one-liner the UI can show ("net long, defensive")
}

// DecisionEngine is the abstraction PMAgent depends on. There are
// currently two implementations:
//
//   - LLMDecisionEngine (this package, llm_engine.go): the real
//     LLM-driven engine. Phase 2A onwards is built around it.
//   - FallbackEngine     (this package, fallback_engine.go): a
//     deterministic, no-LLM-required engine that mirrors the legacy
//     "buy first universe symbol, reduce first position" heuristic.
//     Used as the safety net when the LLM call errors out, when the
//     LLM client is not wired, and as the default in unit tests that
//     don't want to mock an LLM.
//
// Phase 2B will add a DebateEngine that wraps LLMDecisionEngine with
// a multi-round Bull/Bear/Quant agent consensus before the final
// PM decision; that engine satisfies the same interface so the
// wiring layer doesn't change.
type DecisionEngine interface {
	Decide(ctx context.Context, input DecisionInput) (*DecisionOutput, error)
}
