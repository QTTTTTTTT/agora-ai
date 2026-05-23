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

	"github.com/fundai/server/internal/cooldown"
	"github.com/fundai/server/internal/correlation"
	"github.com/fundai/server/internal/earnings"
	"github.com/fundai/server/internal/exposure"
	"github.com/fundai/server/internal/newsrecall"
	"github.com/fundai/server/internal/pairspread"
	"github.com/fundai/server/internal/quality"
	"github.com/fundai/server/internal/quantsnapshot"
	"github.com/fundai/server/internal/ranking"
	"github.com/fundai/server/internal/riskbudget"
)

// SymbolQuantSnapshot is the prompt-facing shape for the per-symbol
// regime + ATR + position-size-ceiling block. It is a strict alias
// for quantsnapshot.Snapshot — kept here so the decision package can
// declare DecisionInput.QuantSnapshots without forcing every caller
// to import the helper package. The wiring layer fills it via the
// shared Builder; tests can set it inline.
type SymbolQuantSnapshot = quantsnapshot.Snapshot

// SymbolRanking is the prompt-facing shape for the Sprint A #2
// cross-sectional ranking table. Alias for ranking.SymbolRanking
// so the decision package stays the single import point for
// prompt-facing types.
type SymbolRanking = ranking.SymbolRanking

// SymbolCooldown is the prompt-facing shape for the Sprint B #1
// event-driven cooldown locks. Alias for cooldown.Lock so the
// decision package stays the single import point for prompt-facing
// types. Each entry tells the PM "this symbol just had a fill —
// don't flip it again unless the catalyst is extreme".
type SymbolCooldown = cooldown.Lock

// RiskBudgetSnapshot is the prompt-facing shape for the Sprint B
// #2 dynamic risk-budget throttle. Alias for riskbudget.Snapshot
// so the decision package stays the single import point. The
// snapshot tells the PM the fund's realised vol, peak-to-trough
// drawdown, the resulting volatility / drawdown scalars, and the
// effective per-trade R% to size with.
type RiskBudgetSnapshot = riskbudget.Snapshot

// SymbolNewsCatalysts is the prompt-facing shape for the Sprint B
// #3 per-symbol news catalyst list. Alias for
// newsrecall.SymbolCatalysts so the decision package stays the
// single import point. Each entry carries 1..K (default 3) recent
// news hits the PM should weigh against the candidate symbol's
// debate verdict.
type SymbolNewsCatalysts = newsrecall.SymbolCatalysts

// NewsHit is the single-news-item alias paired with
// SymbolNewsCatalysts so callers (the wiring layer + tests)
// can construct DecisionInput.NewsCatalysts without importing the
// newsrecall package directly.
type NewsHit = newsrecall.Hit

// EarningsCalendarSnapshot is the prompt-facing shape for the
// Sprint E #2 earnings calendar block. Alias for earnings.Snapshot
// so the decision package stays the single import point for
// prompt-facing types. Each entry tells the PM the NEAREST
// upcoming earnings release per symbol inside the configured
// forward horizon, including the BMO/AMC tag for time-of-day
// shading.
type EarningsCalendarSnapshot = earnings.Snapshot

// EarningsEvent is the single-event alias paired with the
// snapshot so callers (the wiring layer + tests) can build
// DecisionInput.EarningsCalendar without importing the earnings
// package directly.
type EarningsEvent = earnings.Event

// SymbolQualityScore is the prompt-facing shape for the Sprint
// E #3 cross-sectional quality factor block. Alias for
// quality.Score so the decision package stays the single import
// point for prompt-facing types. Carries the per-symbol
// Profitability / Growth / Safety z-scores plus the composite
// the PM sorts by.
type SymbolQualityScore = quality.Score

// PairSpreadSnapshot is the prompt-facing shape for the Sprint
// E #4 pairs-spread monitor. Alias for pairspread.Snapshot so
// the decision package stays the single import point. Carries
// the top-K extended pairs (by |z|) inside the configured
// rolling window, with each pair's last log-spread, mean,
// stdev, z-score, and the upstream rho.
type PairSpreadSnapshot = pairspread.Snapshot

// PairSpreadRow is the single-pair alias paired with the
// snapshot so callers (the wiring layer + tests) can construct
// DecisionInput.PairSpreads without importing the pairspread
// package directly.
type PairSpreadRow = pairspread.PairSpread

// ExposureSnapshot is the prompt-facing shape for the Sprint C
// #1 portfolio-level concentration check. Alias for
// exposure.Snapshot so the decision package stays the single
// import point. Carries per-symbol / per-sector weights with their
// caps, top-3 cluster %, cash %, and any breach lines the PM
// must respect.
type ExposureSnapshot = exposure.Snapshot

// SymbolWeight + SectorWeight expose the per-row exposure shapes
// so the wiring layer + tests can construct ExposureSnapshot
// without importing the exposure package directly.
type SymbolWeight = exposure.SymbolWeight
type SectorWeight = exposure.SectorWeight

// CorrelationSnapshot is the prompt-facing shape for the Sprint C
// #2 pairwise correlation matrix. Alias for correlation.Snapshot
// so the decision package stays the single import point. The
// snapshot carries the high-corr pair list, per-candidate worst
// correlation to any held name, and the held cluster statistics.
type CorrelationSnapshot = correlation.Snapshot

// HighCorrPair / CorrCandidateSummary / HeldCluster expose the
// per-row correlation shapes so callers can construct the
// snapshot without importing the correlation package directly.
type HighCorrPair = correlation.HighCorrPair
type CorrCandidateSummary = correlation.SymbolCorrSummary
type HeldClusterStats = correlation.HeldClusterStats

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

	// Sprint A #1 (regime + ATR position-size ceiling). Per-symbol
	// quantitative snapshot the PM prompt consumes verbatim. Each
	// entry carries the symbol's current regime
	// (trend_up/trend_down/range/chop/unknown), its 14-bar Wilder
	// ATR in price units, the ATR as a fraction of close (so the
	// LLM can reason about cross-symbol relative volatility), and
	// a position-size ceiling derived from
	//   risk_budget / (stop_atr_multiple * ATR_pct)
	// which the prompt instructs the model to treat as an upper
	// bound on any single buy/add qtyPct for that symbol. Empty
	// slice = no quant snapshot wired (legacy behaviour); a slice
	// with zero usable signal per Snapshot.HasSignal is dropped
	// upstream before the prompt is built. Sprint A research
	// rationale: Van Tharp / Kelly position sizing literature
	// consistently shows ATR-anchored ceilings out-Sharpe both
	// fixed-fractional and equal-weight on multi-regime universes.
	QuantSnapshots []SymbolQuantSnapshot

	// Sprint A #2 (cross-sectional ranking). One row per universe
	// symbol with z-scored momentum / volatility / liquidity plus a
	// composite ranking and 1..4 quartile bucket. Lets the PM ask
	// "which of these 12 names look strongest relative to the rest
	// today?" — the workhorse signal of every cross-sectional
	// strategy at AQR / Two Sigma / Renaissance. The prompt
	// instructs the model to prefer Q1 names for buys and treat Q4
	// names as the default watch list, complementing the per-symbol
	// QuantSnapshots filter. Empty / nil = no ranking (insufficient
	// universe size or OHLC unwired); the prompt simply omits the
	// block.
	UniverseRanking []SymbolRanking

	// Sprint E #3 (cross-sectional quality factor). One row per
	// universe symbol with sub-factor z-scores (Profitability /
	// Growth / Safety) plus the composite CompositeZ and a
	// 1..4 quartile bucket. Built from the same fundamental.Metrics
	// the upstream FundamentalSummary string is rendered from —
	// turning the prose into a structured channel the LLM can
	// sort / cite / cross-reference against the momentum-based
	// UniverseRanking. The two rankings are intentionally
	// orthogonal: UniverseRanking captures price-driven signals
	// (the "market thinks"), QualityScores captures the
	// fundamental quality (the "company is"). Top-quartile names
	// in BOTH are the highest-conviction longs in classical
	// quality-momentum combo strategies (the AQR QMJ + UMD
	// double-overlay). Empty / nil = no scores (insufficient
	// universe coverage or fundamentals unwired); the prompt
	// simply omits the block.
	QualityScores []SymbolQualityScore

	// Sprint B #1 (event-driven cooldown). Per-symbol re-entry locks
	// computed from the fund's own trade_executions: any symbol with
	// a fill inside the rolling window (default 24h) shows up here
	// with the rationale (side, hours since fill, hours remaining).
	// The system prompt teaches the PM to force action=watch on
	// those names unless an extreme catalyst overrides — directly
	// suppressing the "buy Monday, reduce Tuesday, add back
	// Wednesday" churn pattern the famous quant systems explicitly
	// gate against. Empty / nil = no active cooldowns (or service
	// unwired); the prompt omits the block.
	Cooldowns []SymbolCooldown

	// Sprint B #2 (dynamic risk budget). Single per-fund snapshot
	// of realised portfolio vol vs target, peak-to-trough drawdown
	// vs ceiling, and the resulting effective per-trade R%. The
	// PM prompt instructs the model to size with
	// effectivePerTradeRiskPct rather than its baseline R, so the
	// fund scales DOWN automatically in a drawdown and scales UP
	// when realised vol is well below target (the AHL / Bridgewater
	// vol-target playbook). nil = no snapshot (service unwired or
	// insufficient NAV history); the prompt omits the block and the
	// PM falls back to its static R prior.
	RiskBudget *RiskBudgetSnapshot

	// Sprint B #3 (per-symbol news catalysts). Top-K (default 3)
	// recent news items per universe / position symbol fetched from
	// the existing marketdata.Service. Sorted most-recent first and
	// bounded by MaxAge (default 7 days) so the prompt only sees
	// catalysts the PM can actually use to weigh a fresh debate
	// verdict. The system prompt teaches the PM to (a) downgrade
	// a buy when a < 48h catalyst contradicts the bullCase, and
	// (b) cite the catalyst when it tips an action. Empty / nil =
	// no fresh catalysts (or service unwired); the prompt omits
	// the block.
	NewsCatalysts []SymbolNewsCatalysts

	// Sprint E #2 (earnings calendar). Per-symbol next scheduled
	// earnings release inside the configured forward horizon
	// (default 14 days). The system prompt teaches the PM to
	// (a) refuse to OPEN a fresh position with an earnings event
	// ≤ T+2 unless the debate carries a concrete catalyst-aware
	// case, (b) prefer "reduce" / "watch" on existing positions
	// the day before earnings, and (c) cite the date + AMC/BMO
	// tag in reasoning when the action is gated by it. nil = no
	// calendar (service unwired / no events in horizon); the
	// prompt omits the block. Structured intentionally distinct
	// from NewsCatalysts: a scheduled earnings release is a HARD
	// catalyst (date is known); news is a SOFT one (we just
	// saw a headline).
	EarningsCalendar *EarningsCalendarSnapshot

	// Sprint C #1 (portfolio exposure check). Fund-level
	// concentration snapshot: per-symbol / per-sector weight
	// rollups with their caps, top-3 cluster, cash %, and a
	// deterministic list of breach strings. The system prompt
	// teaches the PM to (a) refuse buys that would push a
	// bucket past cap, (b) prefer "reduce" on the heaviest
	// breaching name to release dry powder, and (c) honour
	// the cash floor before adding any new exposure. A
	// snapshot whose HasSignal returns false (zero NAV) means
	// the prompt block is omitted entirely.
	Exposure ExposureSnapshot

	// Sprint C #2 (pairwise correlation matrix). Snapshot of
	// rolling daily-return correlations across universe ∪
	// positions: high-correlation pairs (|rho| >= 0.7 by
	// default), per-candidate worst correlation against the
	// held set, and the held-cluster avg / max pairwise. The
	// system prompt teaches the PM to halve (or refuse)
	// sizing on any candidate whose worst held-correlation
	// exceeds the threshold and to skip new cluster adds when
	// the held set's average is already tight. nil = no
	// snapshot (service unwired or insufficient OHLC data);
	// the prompt omits the block.
	Correlations *CorrelationSnapshot

	// Sprint E #4 (pairs spread monitor). Builds on the
	// correlation snapshot above: for each high-correlation
	// pair (|rho| ≥ correlation.HighCorrThreshold) we compute
	// the rolling log-spread z-score over the same lookback
	// window. The system prompt teaches the PM:
	//
	//   |z| < 1   = pair trading near its long-run ratio;
	//               no special action
	//   |z| 1..2  = mild divergence; size new entries with
	//               caution but no forced exits
	//   |z| ≥ 2   = 2-σ divergence; the CHEAP leg (negative
	//               z = Left cheap vs Right) is a candidate
	//               "add" and the RICH leg is a candidate
	//               "reduce" for long-only mean-reversion
	//
	// The block is intentionally distinct from Correlations:
	// the correlation block tells the PM what's TIGHT (the
	// long-run relationship), while PairSpreads tells the PM
	// what's CURRENTLY EXTENDED on top of that relationship.
	// nil = no snapshot (no high-correlation input pairs or
	// the spread service is unwired); the prompt omits the
	// block.
	PairSpreads *PairSpreadSnapshot

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

	// LLM routing hints. The PMAgent loaded by the wiring layer knows
	// its model_provider / model_name from the agents table; the
	// LLMDecisionEngine forwards PMAgentID into ChatRequest.AgentID so
	// llm.ModelRouter.ResolveModel can look up the per-agent default
	// (stored in router.agentDefaults, sourced either from
	// user_model_configs OR — after the P2 fix — from agents.* with an
	// alias mapping the operator-typed model_name to a relay-supported
	// variant). Empty fields fall back to the engine's static UserID /
	// AgentID / platform default, preserving every existing behaviour.
	// FallbackEngine ignores both.
	UserID    string
	PMAgentID string

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
