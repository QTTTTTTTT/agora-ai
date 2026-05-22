// Package sizing turns a sleeve's "buy this instrument" verdict
// into a concrete share count by anchoring position size to a
// fixed dollar risk per trade and the instrument's recent
// volatility (ATR).
//
// Why this exists. Phase 3A-4 sleeves produce buy proposals with
// no quantity — the wiring layer historically reached for a
// hard-coded "spend 25% of NAV on every buy" heuristic. That
// translates to identical dollar exposure on every name, which
// is wrong for two reasons:
//
//   1. A buy on a low-volatility utility risks ~0.5% of NAV per
//      day; the same dollar buy on a high-volatility small-cap
//      risks 3%+ per day. The fund effectively concentrates risk
//      in whichever names happen to move the most.
//   2. There is no relationship between the position size and
//      where the sleeve says the stop-loss should sit. A position
//      whose stop is 1 ATR away is risking a small fraction of
//      its notional; one whose stop is 4 ATR away is risking
//      multiples — but the dollar buy is the same.
//
// PR-3A6 fixes both by making position size = R / risk_per_share,
// where:
//
//   R               = NAV * PerTradeRiskPct (e.g. 0.5%)
//   risk_per_share  = Price - StopPrice (if sleeve gave a stop)
//                     OR K * ATR (if no sleeve stop)
//
// Output Quantity is then clipped against MaxNotionalPctOfNAV so
// a low-volatility position can't single-handedly load the book.
//
// Architecture. Like the strategy package, sizing is a pure
// function over a value-type Input — no I/O, no clock, no DB
// reads. The wiring layer is responsible for plumbing NAV and
// OHLC bars; this keeps sizing trivially testable and free of
// dependency on the platform's repos.
//
// Opt-in. Policy.Enabled defaults to false so legacy funds
// continue to get the historical 25% NAV behaviour. Operators
// flip it on per-fund via fund.config.riskSizing.
package sizing

import (
	"fmt"
	"math"

	"github.com/fundai/server/internal/indicator"
	"github.com/fundai/server/internal/ohlc"
)

// ---------------------------------------------------------------------------
// Input / Result
// ---------------------------------------------------------------------------

// Input is the minimum the sizer needs. The wiring layer
// constructs it from Bundle + position metadata + sleeve hints.
type Input struct {
	// NAV is the fund's current portfolio dollar value. Used as
	// the denominator for both the per-trade risk budget and
	// the notional cap. Required > 0; otherwise sizing is
	// skipped (Result.Applied == false).
	NAV float64

	// Price is the entry price the order will (approximately)
	// execute at. The wiring layer hands the latest close in
	// the bar series, or the most recent quote — whichever it
	// has freshly available.
	Price float64

	// Bars is the recent OHLCV history the ATR is computed
	// from. The series MUST be sorted oldest-first; sizing
	// reads the LAST non-nil ATR value (i.e. the most recent
	// one indicator.ATR was able to produce).
	Bars []ohlc.Bar

	// ExistingStop is the StopLoss price the sleeve already
	// produced, if any. When > 0 AND below Price, the sizer
	// uses it for risk_per_share = Price - ExistingStop. When
	// 0 or invalid, the sizer falls back to the ATR-based
	// stop = Price - K * ATR.
	ExistingStop float64
}

// Result is the sizer's verdict. Even on Applied=false the
// Reason string explains why so the wiring layer can write it
// into PlanAction.Reasoning for the audit trail.
type Result struct {
	// Applied is true when the sizer produced a usable
	// Quantity / StopPrice pair. False when the policy is
	// disabled, the inputs are degenerate, or the computation
	// produced a non-positive quantity. The wiring layer
	// should fall back to the legacy heuristic on false.
	Applied bool

	// Quantity is the recommended share count BEFORE any
	// per-market lot-size normalisation. The wiring layer is
	// expected to run it through instrument.NormalizeBuyQty
	// before persisting. Float64 so a hand-off to a lot
	// normaliser that rounds DOWN doesn't lose precision.
	Quantity float64

	// StopPrice is the absolute price the wiring layer should
	// stamp into PlanAction.StopLoss. It equals ExistingStop
	// when the sleeve gave one and the sizer accepted it;
	// otherwise it's Price - K * ATR.
	StopPrice float64

	// RiskDollars is the actual $ at risk on this position
	// (≈ Quantity * (Price - StopPrice)). Persisted into
	// Reasoning so attribution can see the policy in action.
	RiskDollars float64

	// ATR is the volatility number the sizer used. Zero when
	// ATR wasn't computable (Applied will be false).
	ATR float64

	// Reason is a single-line human-readable explanation of
	// the sizing decision. The wiring layer concatenates it
	// onto PlanAction.Reasoning so the operator (and the
	// downstream attribution agent) can see why this size was
	// chosen.
	Reason string
}

// ---------------------------------------------------------------------------
// Size: the entire algorithm
// ---------------------------------------------------------------------------

// Size is the only public function. Pure: same inputs ↦ same
// outputs. The function is intentionally allocation-free on the
// disabled / unable-to-size paths so the wiring layer can call
// it cheaply for every sleeve action.
//
// The five gating checks at the top short-circuit the common
// "nothing to do" cases before allocating the ATR slice. Each
// branch sets Reason so the operator can immediately see why
// sizing didn't apply.
func Size(p Policy, in Input) Result {
	p = p.EffectivePolicy()
	if !p.Enabled {
		return Result{Reason: "sizing disabled"}
	}
	if in.NAV <= 0 {
		return Result{Reason: "sizing skipped: nav <= 0"}
	}
	if in.Price <= 0 {
		return Result{Reason: "sizing skipped: price <= 0"}
	}
	if len(in.Bars) < p.ATRLookback+1 {
		return Result{Reason: fmt.Sprintf("sizing skipped: need %d bars for ATR(%d), have %d", p.ATRLookback+1, p.ATRLookback, len(in.Bars))}
	}

	atr := mostRecentATR(in.Bars, p.ATRLookback)
	if atr <= 0 {
		return Result{Reason: "sizing skipped: ATR <= 0 (flat-line history?)"}
	}

	// Pick the risk-per-share: prefer the sleeve's explicit
	// stop when it's strictly below the entry price and within
	// a sane band, otherwise fall back to the ATR stop. We cap
	// the sleeve stop at AtMost(K*ATR) so a runaway-tight stop
	// from a misconfigured sleeve doesn't blow the quantity up.
	riskPerShare, stopPrice, stopReason := pickRiskPerShare(p, in, atr)
	if riskPerShare <= 0 {
		return Result{Reason: "sizing skipped: non-positive risk per share"}
	}

	riskBudget := in.NAV * p.PerTradeRiskPct
	rawQty := riskBudget / riskPerShare

	notional := rawQty * in.Price
	capNotional := in.NAV * p.MaxNotionalPctOfNAV
	clipped := false
	if capNotional > 0 && notional > capNotional {
		rawQty = capNotional / in.Price
		clipped = true
	}

	// Round DOWN — buying one extra share would push the
	// dollar risk above the budget. The wiring layer's
	// NormalizeBuyQty will further snap to the instrument's
	// lot size, but we leave that to the boundary.
	qty := math.Floor(rawQty)
	if qty <= 0 {
		return Result{
			Reason: fmt.Sprintf(
				"sizing skipped: budget $%.2f / risk_per_share $%.4f rounds to 0 shares",
				riskBudget, riskPerShare,
			),
		}
	}

	reason := fmt.Sprintf(
		"ATR-sized %.0f shares @ %.2f (risk $%.2f / share %s, stop @ %.2f, ATR(%d)=%.4f, R=%.2f%% NAV=%.2f$)",
		qty, in.Price, riskPerShare, stopReason, stopPrice,
		p.ATRLookback, atr,
		p.PerTradeRiskPct*100, riskBudget,
	)
	if clipped {
		reason += fmt.Sprintf(" — clipped to %.0f%% NAV notional cap", p.MaxNotionalPctOfNAV*100)
	}

	return Result{
		Applied:     true,
		Quantity:    qty,
		StopPrice:   stopPrice,
		RiskDollars: qty * riskPerShare,
		ATR:         atr,
		Reason:      reason,
	}
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

// pickRiskPerShare encapsulates the "use sleeve stop OR ATR
// stop" decision. Pulled out for testability — the rules are
// subtle enough that we want them as a separate unit-tested
// surface.
func pickRiskPerShare(p Policy, in Input, atr float64) (riskPerShare float64, stopPrice float64, reason string) {
	atrStop := in.Price - p.ATRStopMultiplier*atr
	atrRisk := p.ATRStopMultiplier * atr

	if in.ExistingStop > 0 && in.ExistingStop < in.Price {
		sleeveRisk := in.Price - in.ExistingStop
		// Sanity: a sleeve stop tighter than 0.5 ATR is
		// often a typo or fat-finger. We accept it but clamp
		// risk_per_share to MIN 0.5*ATR so quantity doesn't
		// explode toward infinity on a near-zero stop.
		minRisk := 0.5 * atr
		if sleeveRisk < minRisk {
			return minRisk, in.Price - minRisk, "from-sleeve-clamped"
		}
		return sleeveRisk, in.ExistingStop, "from-sleeve"
	}
	return atrRisk, atrStop, fmt.Sprintf("from-ATR×%.1f", p.ATRStopMultiplier)
}

// mostRecentATR pulls the last non-zero entry from indicator.ATR.
// indicator.ATR seeds at index = period (returns 0 for earlier
// indices), so for a well-sized history this is bars[len-1].
// On the boundary case where the most recent bar is exactly at
// the seed index we still get the seed ATR — better than zero.
func mostRecentATR(bars []ohlc.Bar, period int) float64 {
	if len(bars) < period+1 {
		return 0
	}
	highs := make([]float64, len(bars))
	lows := make([]float64, len(bars))
	closes := make([]float64, len(bars))
	for i, b := range bars {
		highs[i] = b.High
		lows[i] = b.Low
		closes[i] = b.Close
	}
	series := indicator.ATR(highs, lows, closes, period)
	for i := len(series) - 1; i >= 0; i-- {
		if series[i] > 0 {
			return series[i]
		}
	}
	return 0
}
