// Package regime classifies the recent market state of a single
// instrument into one of four canonical buckets:
//
//	trend_up   - higher highs / higher lows, MA stack pointing up
//	trend_down - lower highs / lower lows,   MA stack pointing down
//	range      - low-volatility horizontal drift (no clear trend)
//	chop       - high-volatility but trendless mess (uncertainty)
//
// The regime tag is stamped onto every plan_action / position_lot /
// closed_lot row so attribution queries can answer questions like:
//
//	"What's the win rate of the momentum sleeve when regime=trend_up?"
//	"Do we lose more on stop_loss exits when regime=chop?"
//
// The classifier is intentionally deterministic and stateless: given
// the same input bars it always returns the same Regime. State (the
// recent regime per instrument) lives in Service, not here.
//
// Methodology: a layered MA + volatility cascade rather than the
// canonical ADX/DI+/- triplet. The reasons:
//
//  1. Same indicator family (MA50 / MA200) already powers the
//     classical strategy sleeves we'll add in Phase 3A-4 — the
//     regime classifier and the strategy will see the SAME state.
//  2. Explainable: each leg ("close above MA50", "MA50 above MA200",
//     "ATR% above 2.5%") is something an operator can read straight
//     off a chart, which matters for the dashboard trace.
//  3. ADX needs +DI / -DI smoothing chains that double the code
//     size for a marginal accuracy win at the daily timeframe.
package regime

import (
	"math"

	"github.com/fundai/server/internal/indicator"
	"github.com/fundai/server/internal/ohlc"
)

// Regime is the canonical market-state tag. The lowercase string
// form goes straight into plan_actions.regime_tag and friends; we
// expose typed constants so the rest of the codebase doesn't have
// to magic-string compare.
type Regime string

const (
	// Unknown is the zero value, returned when the input bars are
	// missing / too short / malformed. Callers should treat it as
	// "skip the regime stamp" rather than as a meaningful state.
	Unknown Regime = ""
	// TrendUp is a confirmed uptrend: price above MA50, MA50 above
	// MA200, and MA50 itself sloping up over the last few weeks.
	TrendUp Regime = "trend_up"
	// TrendDown is the mirror image: price below MA50, MA50 below
	// MA200, MA50 sloping down.
	TrendDown Regime = "trend_down"
	// Range is the calm-but-trendless state: the MA stack is mixed
	// or close to flat, and realised volatility (ATR / close) is
	// inside the low-noise band. Mean-reversion strategies belong
	// here; trend strategies should sit out.
	Range Regime = "range"
	// Chop is the dangerous state: high realised volatility AND
	// no clean trend signal. Everything is signal noise; size down
	// or stay flat. Most exit-manager fires happen in chop.
	Chop Regime = "chop"
)

// String lets the enum participate in fmt.Stringer without a
// detour through type assertion.
func (r Regime) String() string { return string(r) }

// IsKnown reports whether the regime is one of the four real
// buckets (vs the Unknown sentinel). Useful guard for the wiring
// layer when deciding whether to stamp the tag.
func (r Regime) IsKnown() bool {
	switch r {
	case TrendUp, TrendDown, Range, Chop:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Tunable thresholds. Defaults match the typical equity day-bar
// timeframe; the Service constructor lets callers override.
// ---------------------------------------------------------------------------

// Params holds the classifier's tunable knobs. Zero-value Params is
// NOT useful — always start from DefaultParams() and override the
// fields you care about.
type Params struct {
	// FastMA is the short trend reference. 50 by default — a
	// quarter of trading-year bars.
	FastMA int
	// SlowMA is the long trend reference. 200 by default — a full
	// trading-year. Classic golden-cross / death-cross math.
	SlowMA int
	// SlopeLookback is how many bars back we measure FastMA
	// slope. 20 ≈ one trading month. Picks up regime *transitions*
	// — a fresh trend reversal won't be missed because we wait
	// for MA50 to actually re-slope, not just for one tick to
	// cross the line.
	SlopeLookback int
	// MinSlopePct gates "is the MA actually moving?". Below this
	// magnitude we treat the MA as flat (Range / Chop), which
	// prevents a sideways MA from masquerading as a trend just
	// because it ticked up half a basis point.
	MinSlopePct float64
	// ATRPeriod is Wilder's ATR window. 14 is the textbook default.
	ATRPeriod int
	// HighVolThresholdPct is the ATR-over-close cutoff for "high
	// volatility". 0.025 (2.5% daily true range as a fraction of
	// close) is roughly the line where US large caps stop being
	// boring; tune lower for low-vol indices, higher for crypto.
	HighVolThresholdPct float64
	// MinBars is the minimum bar count we accept. Anything below
	// SlowMA + SlopeLookback + 1 is short enough that the MA200
	// is still bootstrapping. We refuse to guess and return Unknown.
	MinBars int
}

// DefaultParams returns the calibrated defaults for daily equity
// bars. The Service constructor falls back to these when nil is
// passed.
func DefaultParams() Params {
	return Params{
		FastMA:              50,
		SlowMA:              200,
		SlopeLookback:       20,
		MinSlopePct:         0.0005, // 0.05% — filters MA noise but catches real moves
		ATRPeriod:           14,
		HighVolThresholdPct: 0.025,
		MinBars:             221, // SlowMA + SlopeLookback + 1
	}
}

// ---------------------------------------------------------------------------
// Pure classifier
// ---------------------------------------------------------------------------

// Classify inspects the trailing window of bars and returns the
// matching Regime. The bars MUST be sorted oldest-first, which is
// the contract every ohlc.Provider already obeys.
//
// Returns Unknown when:
//
//   - bars is nil / empty
//   - len(bars) < params.MinBars
//   - latest close <= 0 (corrupt feed)
//   - ATR or MA values can't be derived (degenerate input)
//
// Otherwise returns one of TrendUp / TrendDown / Range / Chop.
// The function is allocation-light (one slice per indicator) and
// safe to call thousands of times per pass.
func Classify(bars []ohlc.Bar, params Params) Regime {
	if params == (Params{}) {
		params = DefaultParams()
	}
	if len(bars) < params.MinBars {
		return Unknown
	}
	closes := indicator.Closes(bars)
	highs := indicator.Highs(bars)
	lows := indicator.Lows(bars)

	last := len(closes) - 1
	close := closes[last]
	if close <= 0 || math.IsNaN(close) {
		return Unknown
	}

	maFast := indicator.SMA(closes, params.FastMA)
	maSlow := indicator.SMA(closes, params.SlowMA)
	atr := indicator.ATR(highs, lows, closes, params.ATRPeriod)

	fast := maFast[last]
	slow := maSlow[last]
	atrLast := atr[last]
	if fast <= 0 || slow <= 0 {
		return Unknown
	}
	prevIdx := last - params.SlopeLookback
	if prevIdx < 0 {
		return Unknown
	}
	fastPrev := maFast[prevIdx]
	if fastPrev <= 0 {
		return Unknown
	}
	slope := (fast - fastPrev) / fastPrev

	atrPct := 0.0
	if atrLast > 0 {
		atrPct = atrLast / close
	}

	// --- Trend leg --------------------------------------------------
	// Both alignment AND slope must agree. This guards against the
	// "MA just barely crossed" false positive that wrecks naive
	// dual-MA systems on choppy ranges.
	trendUp := close > fast && fast > slow && slope >= params.MinSlopePct
	trendDown := close < fast && fast < slow && slope <= -params.MinSlopePct

	if trendUp {
		return TrendUp
	}
	if trendDown {
		return TrendDown
	}

	// --- No-trend leg: separate Range from Chop by realised vol. ----
	// Chop = noisy enough that mean-reversion is dangerous too.
	// Range = quiet enough that mean-reversion is profitable.
	if atrPct >= params.HighVolThresholdPct {
		return Chop
	}
	return Range
}
