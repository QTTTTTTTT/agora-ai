// Package strategy hosts the deterministic classical-quant
// strategy sleeves the platform runs alongside the LLM PM.
//
// Phase 3A-4 ships two sleeves:
//
//	trend          - Donchian-20 breakout + MA50/MA200 trend
//	                 confirmation. Best in regime=trend_up /
//	                 trend_down.
//	mean_reversion - RSI(14) + Bollinger(20,2) reversion to mean.
//	                 Best in regime=range; explicitly OFF in
//	                 regime=chop where mean reversion is the
//	                 most dangerous trade.
//
// Each sleeve is a deterministic function (Sleeve.Evaluate) of a
// recent bar history. Sleeves do NOT see fund state, LLM output,
// or one another's proposals — they live downstream of any
// learning loop and upstream of the merger that decides which
// proposal wins on a given (instrument, decision slot).
//
// Why bother with sleeves when the LLM PM can already say "buy
// NVDA"?
//
//   1. Auditable. A Donchian breakout is a one-liner ("price
//      crossed above the 20-day high"); an LLM's "buy NVDA" is
//      not. Attribution agent can carve win-rate by sleeve.
//   2. Diversifier. Trend and mean-reversion are anti-correlated:
//      when one drawdown's, the other is usually winning.
//   3. Regime gate. Sleeves explicitly declare which regimes
//      they're allowed to fire in (PreferredRegimes), giving us
//      a deterministic "right strategy for the weather" overlay
//      that the LLM does NOT have to know about.
//   4. Sandbox. fund.config.strategySleeves.enabled defaults to
//      false so legacy funds are unchanged. Operators flip it on
//      to start collecting attribution data.
//
// Architectural contract:
//
//   - Sleeve implementations live in this package (trend.go,
//     meanreversion.go).
//   - Service is the wiring-side façade that takes a Bundle of
//     (instrument, bars, regime) inputs and returns a slice of
//     SleeveAction values ready for the PMAgent to merge into
//     plan_actions.
//   - The package depends on indicator + regime + ohlc but
//     nothing else from the broader codebase. The PMAgent does
//     all I/O (OHLC fetch, position lookup) and hands fully-
//     hydrated Bundle values to Service.Evaluate.
package strategy

import (
	"strings"
	"time"

	"github.com/fundai/server/internal/ohlc"
	"github.com/fundai/server/internal/regime"
)

// ---------------------------------------------------------------------------
// Core types
// ---------------------------------------------------------------------------

// Action is the side the sleeve recommends. Mirrors the
// plan_actions.action vocabulary so the wiring layer can paste
// straight through without translation.
type Action string

const (
	// ActionBuy opens or adds to a long position.
	ActionBuy Action = "buy"
	// ActionSell closes or trims a long position. The classical
	// sleeves NEVER open new short positions in Phase 3A-4;
	// short exposure stays under the LLM PM's purview until the
	// lot ledger gains short-side support.
	ActionSell Action = "sell"
	// ActionHold is the default no-op. Sleeve.Evaluate returns
	// nil for "no opinion"; ActionHold is reserved for the
	// future case where a sleeve wants to actively flag "I
	// looked, and the answer is hold" vs the absence of an
	// opinion entirely.
	ActionHold Action = "hold"
)

// Bundle is the per-instrument input the Service hands to each
// enabled sleeve. The wiring layer pre-fetches OHLC bars and the
// regime tag so sleeves stay pure functions of their input.
//
// Bars MUST be sorted oldest-first (the contract every
// ohlc.Provider already obeys). The classifier params are
// implicit in DefaultParams + the per-sleeve overrides; we don't
// thread them through Bundle to keep the input shape simple.
type Bundle struct {
	InstrumentKey string
	Symbol        string
	Market        string
	AssetClass    string
	// Bars is the recent OHLCV history. Length should clear the
	// largest indicator window any enabled sleeve needs
	// (DefaultParams asks for ~250 daily bars).
	Bars []ohlc.Bar
	// Regime is the precomputed market state. Sleeves use it
	// to gate themselves: a mean-reversion sleeve refuses to
	// fire in regime=trend_*, a trend sleeve refuses to fire
	// in regime=range. Unknown is treated as "no gate" — the
	// sleeve decides whether to skip on lack of regime data.
	Regime regime.Regime
	// AsOf is the timestamp of the most recent bar.
	// Currently informational only; future iterations can use
	// it to assert the bars are fresh enough for live trading.
	AsOf time.Time
}

// Proposal is one sleeve's verdict on one Bundle. Returning nil
// from Evaluate signals "no opinion" — far more common than a
// firing proposal — and lets the Service skip allocation for the
// 95% no-trade case.
//
// Stop / target prices are OPTIONAL hints to the exit manager:
// the exit manager owns the final stop-loss math; sleeves only
// suggest. When both StopLoss and the fund's exit_policy
// stop_loss fire on the same lot, the exit_policy threshold wins
// (it's the position-level safety net, not a strategy opinion).
type Proposal struct {
	Action       Action
	Confidence   float64
	Reasoning    string
	StopLoss     float64
	TakeProfit   float64
	// SignalSource is the human-readable identifier for the
	// concrete signal that fired ("donchian_20", "rsi_bb_14_20").
	// Persisted to plan_action.signal_source and propagated into
	// position_lots.signal_source. The attribution agent groups
	// by this column to spot which signals consistently lose
	// money.
	SignalSource string
}

// ---------------------------------------------------------------------------
// Sleeve interface
// ---------------------------------------------------------------------------

// Sleeve is the contract every strategy implementation satisfies.
// Two trivial sleeves ship in this package; the SignalSource
// field on Proposal lets future sleeves declare multiple sub-
// strategies (e.g. donchian_20 + donchian_55) inside one Sleeve.
type Sleeve interface {
	// Name is the persistent identifier — stored verbatim into
	// plan_actions.sleeve. Examples: "trend", "mean_reversion".
	// Lower-snake-case, stable, NOT a display string.
	Name() string

	// PreferredRegimes returns the regimes where this sleeve is
	// allowed to fire. Empty slice means "any regime", including
	// Unknown. Sleeves typically declare a narrow set:
	//   trend:          {TrendUp, TrendDown}
	//   mean_reversion: {Range}
	// The Service uses this list as a hard gate — a sleeve
	// CANNOT propose in a regime it didn't declare, even if the
	// underlying indicator screams.
	PreferredRegimes() []regime.Regime

	// Evaluate runs the deterministic indicator logic against
	// the bundle and returns at most one Proposal. nil = "no
	// opinion this bar". Implementations MUST be pure: same
	// input → same output. State (cooldowns, recent fires)
	// lives one layer up, in the wiring code that calls
	// Service.Evaluate.
	Evaluate(b Bundle) *Proposal
}

// ---------------------------------------------------------------------------
// Service output
// ---------------------------------------------------------------------------

// SleeveAction is the Service's per-(sleeve, instrument) output,
// ready for the PMAgent to translate into a plan_action row. It
// carries enough metadata that the wiring layer doesn't need to
// re-derive symbol / market / asset_class from the Bundle.
type SleeveAction struct {
	Sleeve        string
	Symbol        string
	InstrumentKey string
	Market        string
	AssetClass    string
	Regime        regime.Regime
	Proposal      Proposal
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// AllowsRegime is a small helper sleeve implementations call from
// inside Evaluate to short-circuit on a regime gate. The Service
// also enforces the gate before calling Evaluate, but we expose
// it here so direct callers (tests, future per-sleeve wiring)
// don't accidentally bypass the rule.
func AllowsRegime(preferred []regime.Regime, r regime.Regime) bool {
	if len(preferred) == 0 {
		return true
	}
	for _, p := range preferred {
		if p == r {
			return true
		}
	}
	return false
}

// normalize trims and lowercases a name field. Sleeves run this
// at construction time so look-ups against the policy's
// EnabledSleeves slice are case/whitespace insensitive.
func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
