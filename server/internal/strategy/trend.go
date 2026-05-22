package strategy

import (
	"fmt"
	"math"

	"github.com/fundai/server/internal/indicator"
	"github.com/fundai/server/internal/regime"
)

// ---------------------------------------------------------------------------
// Trend-following sleeve
// ---------------------------------------------------------------------------
//
// Signal: Donchian-N breakout, gated by MA trend alignment.
//
//   LONG  fires when:
//     close[t] > donchian.upper[t-1]   (today closes above
//                                       yesterday's N-day high)
//     AND ma_fast > ma_slow            (medium-term uptrend)
//     AND ma_fast slope > 0            (confirmation)
//
//   SHORT fires when the mirror image holds AND we already
//     have a long lot — sleeve produces a SELL action to close
//     the long position. Phase 3A-4 deliberately does NOT open
//     short lots; that arrives in PR-3A-X with the lot ledger's
//     short-side support.
//
// Why "yesterday's high" rather than "today's high"?
//
// Using yesterday's level means the breakout level is FIXED at
// the start of today's bar — no look-ahead. If we used today's
// rolling upper, a single intraday spike could trigger a "look,
// we broke out!" buy after the spike already faded.
//
// Confidence model: graduated by how clean the breakout is.
//   - close exactly on the level  → 0.55 (minimum to fire)
//   - close > level by 1 ATR      → 0.85 (high confidence)
//   - close > level by 2+ ATR     → 0.95 (saturated)
//
// We CAP at 0.95 because a deterministic indicator should never
// claim certainty — the runtime risk gates and the LLM PM
// retain veto authority.

const (
	// trendSignalSource is the persisted signal_source tag for
	// every Proposal this sleeve emits. The Donchian period is
	// configurable, but the tag stays stable across periods so
	// attribution queries don't get fragmented.
	trendSignalSource = "donchian_20"
)

// TrendSleeve implements the Sleeve interface. Instances are
// cheap to construct; the wiring layer creates one per
// configured fund.
type TrendSleeve struct {
	params TrendParams
}

// NewTrendSleeve builds a TrendSleeve with the supplied params.
// Zero values in `params` are NOT back-filled — the caller is
// expected to pass an EffectivePolicy-normalised TrendParams or
// defaultTrend(). The Service does this for us.
func NewTrendSleeve(params TrendParams) *TrendSleeve {
	return &TrendSleeve{params: params}
}

// Name implements Sleeve.
func (t *TrendSleeve) Name() string { return "trend" }

// PreferredRegimes restricts the sleeve to confirmed trends.
// In regime=range the Donchian breakout will produce constant
// whipsaw losses; in regime=chop the noise floor is too high
// to trust a breakout. Both are gated off here.
func (t *TrendSleeve) PreferredRegimes() []regime.Regime {
	return []regime.Regime{regime.TrendUp, regime.TrendDown}
}

// Evaluate implements Sleeve. Returns nil for "no opinion".
func (t *TrendSleeve) Evaluate(b Bundle) *Proposal {
	p := t.params
	// Required window: max(DonchianPeriod, SlowMA) + 2 (we read
	// upper[last-1] and the slope reference one window back).
	need := p.DonchianPeriod + 1
	if p.SlowMA+1 > need {
		need = p.SlowMA + 1
	}
	if len(b.Bars) < need {
		return nil
	}
	if !AllowsRegime(t.PreferredRegimes(), b.Regime) {
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

	upper, _, lower := indicator.DonchianChannel(highs, lows, p.DonchianPeriod)
	// Use PREVIOUS bar's channel — the breakout level is fixed
	// at session start, no look-ahead. last-1 is guaranteed to
	// be a fully-formed channel value because we checked the
	// window above.
	refUpper := upper[last-1]
	refLower := lower[last-1]
	if refUpper <= 0 || refLower <= 0 {
		return nil
	}

	maFast := indicator.SMA(closes, p.FastMA)
	maSlow := indicator.SMA(closes, p.SlowMA)
	fast := maFast[last]
	slow := maSlow[last]
	if fast <= 0 || slow <= 0 {
		return nil
	}
	// Slope reference: same lookback as the regime classifier so
	// the two stay consistent. If FastMA dipped from N bars ago,
	// the trend hasn't actually accelerated — skip the breakout.
	slopeRefIdx := last - 20
	if slopeRefIdx < 0 {
		return nil
	}
	slopeRef := maFast[slopeRefIdx]
	if slopeRef <= 0 {
		return nil
	}
	slope := (fast - slopeRef) / slopeRef

	// True ATR for confidence scaling. Period stays 14 (the
	// classifier's default) to keep one volatility yardstick
	// across the system.
	atr := indicator.ATR(highs, lows, closes, 14)
	atrLast := atr[last]

	// ----- LONG path ------------------------------------------------
	if b.Regime == regime.TrendUp && fast > slow && slope > 0 && close > refUpper {
		strength := 0.0
		if atrLast > 0 {
			strength = (close - refUpper) / atrLast
		}
		conf := trendConfidence(strength)
		reasoning := fmt.Sprintf(
			"trend(donchian_%d): close %.4f > prev %d-day high %.4f (regime=%s, MA%d=%.4f > MA%d=%.4f, slope=%.2f%%, ATR_strength=%.2f)",
			p.DonchianPeriod, close, p.DonchianPeriod, refUpper,
			b.Regime, p.FastMA, fast, p.SlowMA, slow, slope*100, strength,
		)
		return &Proposal{
			Action:       ActionBuy,
			Confidence:   conf,
			Reasoning:    reasoning,
			StopLoss:     stopLossPrice(close, p.StopLossPct, ActionBuy),
			TakeProfit:   takeProfitPrice(close, p.TakeProfitPct, ActionBuy),
			SignalSource: trendSignalSource,
		}
	}

	// ----- SHORT (= SELL existing long) path ------------------------
	// Mirror of LONG. In Phase 3A-4 we only emit SELL when the
	// fund is presumed long; the merge layer takes care of
	// silently dropping the sell when no long exists.
	if b.Regime == regime.TrendDown && fast < slow && slope < 0 && close < refLower {
		strength := 0.0
		if atrLast > 0 {
			strength = (refLower - close) / atrLast
		}
		conf := trendConfidence(strength)
		reasoning := fmt.Sprintf(
			"trend(donchian_%d): close %.4f < prev %d-day low %.4f (regime=%s, MA%d=%.4f < MA%d=%.4f, slope=%.2f%%, ATR_strength=%.2f)",
			p.DonchianPeriod, close, p.DonchianPeriod, refLower,
			b.Regime, p.FastMA, fast, p.SlowMA, slow, slope*100, strength,
		)
		return &Proposal{
			Action:       ActionSell,
			Confidence:   conf,
			Reasoning:    reasoning,
			StopLoss:     stopLossPrice(close, p.StopLossPct, ActionSell),
			SignalSource: trendSignalSource,
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// trendConfidence maps "ATR-strength" of the breakout to a
// confidence in [0.55, 0.95]. Linear ramp between strength=0.1
// and strength=2.0, saturated outside.
func trendConfidence(atrStrength float64) float64 {
	if atrStrength <= 0.1 {
		return 0.55
	}
	if atrStrength >= 2.0 {
		return 0.95
	}
	// linear interpolate
	return 0.55 + (atrStrength-0.1)*(0.95-0.55)/(2.0-0.1)
}

// stopLossPrice converts a per-side stop-loss percentage into an
// absolute price level. Returns 0 when pct <= 0 (the
// EffectivePolicy normaliser leaves StopLossPct at 0 if the
// operator wants no per-proposal stop hint).
func stopLossPrice(close, pct float64, action Action) float64 {
	if pct <= 0 || close <= 0 {
		return 0
	}
	if action == ActionBuy {
		return close * (1 - pct)
	}
	if action == ActionSell {
		return close * (1 + pct) // stop on a short = upside
	}
	return 0
}

func takeProfitPrice(close, pct float64, action Action) float64 {
	if pct <= 0 || close <= 0 {
		return 0
	}
	if action == ActionBuy {
		return close * (1 + pct)
	}
	if action == ActionSell {
		return close * (1 - pct)
	}
	return 0
}
