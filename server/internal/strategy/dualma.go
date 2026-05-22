package strategy

import (
	"fmt"
	"math"

	"github.com/fundai/server/internal/indicator"
	"github.com/fundai/server/internal/regime"
)

// ---------------------------------------------------------------------------
// Dual-EMA crossover sleeve
// ---------------------------------------------------------------------------
//
// Signal: EMA(fast) crossing EMA(slow). Defaults are 12 / 26 — the
// MACD constituents — so attribution scorecards can compare this
// sleeve's contribution against the MACD line at the same window.
//
//   LONG (golden cross) fires when:
//     ema_fast[t]   >  ema_slow[t]
//     AND ema_fast[t-1] <= ema_slow[t-1]
//     AND close[t] > 0
//
//   SHORT (death cross — emits a SELL on existing long, never
//   opens a short lot, same convention as the trend sleeve)
//   fires when:
//     ema_fast[t]   <  ema_slow[t]
//     AND ema_fast[t-1] >= ema_slow[t-1]
//
// Why fire ONLY on the cross day, not every bar where fast > slow?
//
// The Donchian-breakout `trend` sleeve already covers the "we're
// in an established trend" use case. If DualMA also fired every
// bar inside an established up-trend it would simply rubber-stamp
// the same instruments, doubling attribution rows without adding
// signal. By limiting to the cross transition we get a distinct,
// auditable event that complements (rather than echoes) trend.
//
// Confidence model: how cleanly the fast line crosses through the
// slow line, measured in ATR units. A 0.5-ATR gap is a meaningful
// cross; a 0.1-ATR gap on the edge of indicator noise is barely
// a signal. The same [0.55, 0.95] band the other sleeves use is
// reused so MinConfidence has consistent semantics across the
// whole policy.

const (
	// dualMASignalSource is the persisted signal_source tag for
	// every Proposal this sleeve emits. The period is configurable,
	// but the tag stays stable across periods so attribution
	// rolls up cleanly.
	dualMASignalSource = "dual_ma_cross"
)

// DualMASleeve implements the Sleeve interface using two
// exponential moving averages. Instances are cheap; the wiring
// layer constructs one per fund.
type DualMASleeve struct {
	params DualMAParams
}

// NewDualMASleeve builds a DualMASleeve. The caller is expected to
// pass an EffectivePolicy-normalised DualMAParams (or
// defaultDualMA()); zero / inverted values are NOT back-filled
// here. The strategy.Service runs that normalisation for us at
// construction time.
func NewDualMASleeve(params DualMAParams) *DualMASleeve {
	return &DualMASleeve{params: params}
}

// Name implements Sleeve.
func (d *DualMASleeve) Name() string { return "dual_ma" }

// PreferredRegimes pins the sleeve to confirmed trends. EMA
// crossovers in regime=range produce constant whipsaw losses
// (every Bollinger fade triggers a fake cross); regime=chop is
// even worse. Both are gated off here.
func (d *DualMASleeve) PreferredRegimes() []regime.Regime {
	return []regime.Regime{regime.TrendUp, regime.TrendDown}
}

// Evaluate implements Sleeve. Returns nil for "no opinion".
func (d *DualMASleeve) Evaluate(b Bundle) *Proposal {
	p := d.params
	// EMA's first valid index is period-1; we need the previous
	// bar's EMA too, so the window has to reach at least
	// SlowEMA+1 bars. Adding a small buffer keeps the EMA
	// stabilised — the indicator seeds with an SMA over the
	// first `period` closes and ramps from there.
	need := p.SlowEMA + 5
	if len(b.Bars) < need {
		return nil
	}
	if !AllowsRegime(d.PreferredRegimes(), b.Regime) {
		return nil
	}

	closes := indicator.Closes(b.Bars)
	highs := indicator.Highs(b.Bars)
	lows := indicator.Lows(b.Bars)
	last := len(closes) - 1
	close := closes[last]
	if close <= 0 || math.IsNaN(close) {
		return nil
	}

	fast := indicator.EMA(closes, p.FastEMA)
	slow := indicator.EMA(closes, p.SlowEMA)
	fastNow, fastPrev := fast[last], fast[last-1]
	slowNow, slowPrev := slow[last], slow[last-1]
	if fastNow <= 0 || slowNow <= 0 || fastPrev <= 0 || slowPrev <= 0 {
		return nil
	}

	atr := indicator.ATR(highs, lows, closes, 14)
	atrLast := atr[last]

	// ----- LONG: golden cross today --------------------------------
	if b.Regime == regime.TrendUp && fastNow > slowNow && fastPrev <= slowPrev {
		gap := fastNow - slowNow
		strength := 0.0
		if atrLast > 0 {
			strength = gap / atrLast
		}
		conf := dualMAConfidence(strength)
		reasoning := fmt.Sprintf(
			"dual_ma(%d/%d): golden cross today — EMA%d %.4f rose above EMA%d %.4f (regime=%s, ATR_strength=%.2f)",
			p.FastEMA, p.SlowEMA, p.FastEMA, fastNow, p.SlowEMA, slowNow, b.Regime, strength,
		)
		return &Proposal{
			Action:       ActionBuy,
			Confidence:   conf,
			Reasoning:    reasoning,
			StopLoss:     stopLossPrice(close, p.StopLossPct, ActionBuy),
			TakeProfit:   takeProfitPrice(close, p.TakeProfitPct, ActionBuy),
			SignalSource: dualMASignalSource,
		}
	}

	// ----- SHORT (= SELL existing long): death cross today ---------
	if b.Regime == regime.TrendDown && fastNow < slowNow && fastPrev >= slowPrev {
		gap := slowNow - fastNow
		strength := 0.0
		if atrLast > 0 {
			strength = gap / atrLast
		}
		conf := dualMAConfidence(strength)
		reasoning := fmt.Sprintf(
			"dual_ma(%d/%d): death cross today — EMA%d %.4f fell below EMA%d %.4f (regime=%s, ATR_strength=%.2f)",
			p.FastEMA, p.SlowEMA, p.FastEMA, fastNow, p.SlowEMA, slowNow, b.Regime, strength,
		)
		return &Proposal{
			Action:       ActionSell,
			Confidence:   conf,
			Reasoning:    reasoning,
			StopLoss:     stopLossPrice(close, p.StopLossPct, ActionSell),
			SignalSource: dualMASignalSource,
		}
	}
	return nil
}

// dualMAConfidence maps ATR-gap of the cross to confidence in
// [0.55, 0.95]. A 0.1-ATR gap barely cleared indicator noise →
// 0.55; a 1.5-ATR gap is a decisive cross → 0.95. Linear ramp
// between the two extremes.
func dualMAConfidence(atrStrength float64) float64 {
	if atrStrength <= 0.1 {
		return 0.55
	}
	if atrStrength >= 1.5 {
		return 0.95
	}
	return 0.55 + (atrStrength-0.1)*(0.95-0.55)/(1.5-0.1)
}
