package strategy

import (
	"fmt"
	"math"

	"github.com/fundai/server/internal/ohlc"
	"github.com/fundai/server/internal/regime"
)

// ---------------------------------------------------------------------------
// Time-series momentum sleeve (TSMOM)
// ---------------------------------------------------------------------------
//
// Signal: "Moskowitz / Ooi / Pedersen 2012" 12-1 month per-name
// momentum. Unlike CrossSectionalMomentum (which ranks bundles
// across the universe and buys the top quintile / sells the
// bottom), TSMOM treats each bundle in isolation:
//
//     score(bundle) = (price[t-skip] - price[t-lookback]) /
//                      price[t-lookback]
//
// With defaults lookback=240 daily bars (~12 months), skip=21
// (~1 month). The signal is a SIGNED scalar — sign determines
// the side, magnitude relative to realised volatility determines
// confidence:
//
//     score > 0 AND |t| > threshold → BUY
//     score < 0 AND |t| > threshold → SELL (on existing long)
//
// where t is the t-statistic of the lookback return scaled by
// the daily-return realised volatility over the same window
// (annualised → daily, then taken raw):
//
//     t = score / (σ_daily · sqrt(N_returns))
//
// Why TSMOM in addition to XSMOMentum? XSMOM is RELATIVE — when
// every name in the universe is selling off it still buys the
// "least bad" decile, which is the recipe for catching falling
// knives during regime switches. TSMOM is ABSOLUTE — when every
// name has score<0 it fires NO buys, only sells. The two are
// known to be anti-correlated in real PnL, especially across
// bear-market transitions. AQR's Managed Futures strategy ran
// on TSMOM for decades; the Moskowitz et al. 2012 paper found
// the per-name signal alone produced a Sharpe of ~1.0 net of
// fees across 58 instruments and 25 years.
//
// Regime gate: pinned to TrendUp / TrendDown for the same reason
// xs_momentum is — both are trend strategies. We DO fire on
// TrendDown bundles (with action=sell) because that's exactly
// where TSMOM earns its keep: it's one of the few sleeves with
// any business closing longs into a confirmed downtrend.

const (
	// tsmomSignalSource is the persisted signal_source tag for
	// every Proposal this sleeve emits. The "12_1" naming
	// matches the canonical academic shorthand.
	tsmomSignalSource = "tsmom_12_1"
)

// TSMomentumParams tunes the per-name TSMOM sleeve. Zero values
// fall back to defaults via EffectivePolicy.
type TSMomentumParams struct {
	// LookbackBars is the upper end of the momentum window
	// counted backward from the most recent bar. 240 daily
	// bars ≈ 11.5 months (chosen to match XSMomentum's default
	// so the wiring layer can satisfy both sleeves with the
	// same OHLC fetch budget).
	LookbackBars int `json:"lookbackBars,omitempty"`
	// SkipBars excludes the most recent N bars from the
	// momentum calculation. The classic 21 daily bars implements
	// the "skip last month to dodge short-term reversal" trick.
	// Set to 0 to disable.
	SkipBars int `json:"skipBars,omitempty"`
	// MinAbsT is the minimum |t-statistic| of the lookback
	// return below which we refuse to fire. Defaults to 0.5
	// (loose: filters out only the noisiest signals). Raise to
	// 1.0 or higher for a tighter sleeve that fires less often
	// but with more conviction.
	MinAbsT float64 `json:"minAbsT,omitempty"`
	// MaxSatT is the |t-statistic| above which Confidence
	// saturates at 0.95. Linear ramp between MinAbsT and
	// MaxSatT lifts Confidence from 0.55 → 0.95. Defaults to
	// 2.0 — i.e. a 2-σ momentum signal is treated as "high
	// conviction".
	MaxSatT float64 `json:"maxSatT,omitempty"`
	// StopLossPct hint to the exit manager. 0 leaves it unset.
	StopLossPct float64 `json:"stopLossPct,omitempty"`
}

// defaultTSMomentum returns the ship defaults. Lookback / skip
// mirror XSMomentum so a fund can enable BOTH sleeves without
// blowing the OHLC fetch budget.
func defaultTSMomentum() TSMomentumParams {
	return TSMomentumParams{
		LookbackBars: 240,
		SkipBars:     21,
		MinAbsT:      0.5,
		MaxSatT:      2.0,
		StopLossPct:  0.06,
	}
}

// TSMomentumSleeve implements the Sleeve interface — per-bundle
// dispatch, NOT batch (unlike CrossSectionalMomentumSleeve).
type TSMomentumSleeve struct {
	params TSMomentumParams
}

// NewTSMomentumSleeve builds a sleeve with the supplied params.
// Like the other constructors in this package, zero values are
// NOT back-filled — the caller is expected to pass an
// EffectivePolicy-normalised params struct.
func NewTSMomentumSleeve(params TSMomentumParams) *TSMomentumSleeve {
	return &TSMomentumSleeve{params: params}
}

// Name implements Sleeve. "tsmom" rather than "ts_momentum" is
// shorter and matches the academic shorthand TSMOM operators
// already use in conversation.
func (t *TSMomentumSleeve) Name() string { return "tsmom" }

// PreferredRegimes pins the sleeve to confirmed trends. Range /
// chop produce small per-name moves with high noise → low |t|
// → almost always sub-threshold anyway; we gate explicitly so
// the attribution row stays clean.
func (t *TSMomentumSleeve) PreferredRegimes() []regime.Regime {
	return []regime.Regime{regime.TrendUp, regime.TrendDown}
}

// Evaluate implements Sleeve. Returns nil for "no opinion".
func (t *TSMomentumSleeve) Evaluate(b Bundle) *Proposal {
	p := t.params
	if p.LookbackBars <= 1 {
		return nil
	}
	// We need len(bars) ≥ lookback+1 because the score uses
	// price[len-1-skip] AND price[len-1-lookback], and both
	// indices must be valid.
	if len(b.Bars) < p.LookbackBars+1 {
		return nil
	}
	if !AllowsRegime(t.PreferredRegimes(), b.Regime) {
		return nil
	}
	score, ok := tsmomScore(b.Bars, p.LookbackBars, p.SkipBars)
	if !ok {
		return nil
	}
	tStat, ok := tsmomTStat(b.Bars, p.LookbackBars, p.SkipBars, score)
	if !ok {
		return nil
	}
	absT := math.Abs(tStat)
	if absT < p.MinAbsT {
		return nil
	}
	conf := tsmomConfidence(absT, p.MinAbsT, p.MaxSatT)
	if conf <= 0 {
		return nil
	}
	action := ActionBuy
	side := "long"
	if score < 0 {
		action = ActionSell
		side = "short"
	}
	lastClose := b.Bars[len(b.Bars)-1].Close
	proposal := &Proposal{
		Action:       action,
		Confidence:   conf,
		SignalSource: tsmomSignalSource,
		Reasoning: fmt.Sprintf(
			"tsmom_12_1: %d-bar return %.2f%% (skip=%d, side=%s, |t|=%.2f, conf=%.2f)",
			p.LookbackBars, score*100, p.SkipBars, side, absT, conf,
		),
	}
	if p.StopLossPct > 0 && lastClose > 0 && action == ActionBuy {
		// Stop hint only meaningful on the long side; on the
		// sell side the exit manager owns the close anyway.
		proposal.StopLoss = lastClose * (1 - p.StopLossPct)
	}
	return proposal
}

// ---------------------------------------------------------------------------
// Math helpers
// ---------------------------------------------------------------------------

// tsmomScore returns (price[t-skip] - price[t-lookback]) /
// price[t-lookback]. Same formula as XSMomentum so a fund
// running both sleeves sees the same numerator — only the
// downstream gating differs.
//
// Returns (0, false) when the window doesn't fit or the
// reference price is degenerate.
func tsmomScore(bars []ohlc.Bar, lookbackBars, skipBars int) (float64, bool) {
	if lookbackBars <= 1 || len(bars) <= lookbackBars {
		return 0, false
	}
	if skipBars < 0 {
		skipBars = 0
	}
	if skipBars >= lookbackBars {
		return 0, false
	}
	last := len(bars) - 1
	nowIdx := last - skipBars
	thenIdx := last - lookbackBars
	if nowIdx < 0 || thenIdx < 0 || nowIdx >= len(bars) || thenIdx >= len(bars) {
		return 0, false
	}
	now := bars[nowIdx].Close
	then := bars[thenIdx].Close
	if now <= 0 || then <= 0 || math.IsNaN(now) || math.IsNaN(then) {
		return 0, false
	}
	return (now - then) / then, true
}

// tsmomTStat scales the cumulative return by the realised
// volatility of the daily return series over the same window:
//
//     σ_daily = stdev(log-returns over the window)
//     t       = score / (σ_daily · sqrt(N))
//
// where N is the number of valid log-returns inside the window.
// The square-root scaling is the standard "compounded return /
// total-period vol" normalisation: a sustained drift produces
// a high t even at modest daily vol; a random walk's t collapses
// toward zero.
//
// Returns (0, false) when the window doesn't fit or vol is zero
// (constant series — degenerate, no signal).
func tsmomTStat(bars []ohlc.Bar, lookbackBars, skipBars int, score float64) (float64, bool) {
	if lookbackBars <= 1 || len(bars) <= lookbackBars {
		return 0, false
	}
	if skipBars < 0 {
		skipBars = 0
	}
	if skipBars >= lookbackBars {
		return 0, false
	}
	last := len(bars) - 1
	endIdx := last - skipBars
	startIdx := last - lookbackBars
	if startIdx < 0 || endIdx <= startIdx || endIdx >= len(bars) {
		return 0, false
	}
	// Collect log-returns inside (startIdx, endIdx].
	rets := make([]float64, 0, endIdx-startIdx)
	for i := startIdx + 1; i <= endIdx; i++ {
		prev := bars[i-1].Close
		curr := bars[i].Close
		if prev <= 0 || curr <= 0 || math.IsNaN(prev) || math.IsNaN(curr) {
			continue
		}
		rets = append(rets, math.Log(curr/prev))
	}
	if len(rets) < 2 {
		return 0, false
	}
	var sum, sumSq float64
	for _, r := range rets {
		sum += r
		sumSq += r * r
	}
	mean := sum / float64(len(rets))
	variance := sumSq/float64(len(rets)) - mean*mean
	if variance <= 0 {
		return 0, false
	}
	sigma := math.Sqrt(variance)
	denom := sigma * math.Sqrt(float64(len(rets)))
	if denom <= 0 {
		return 0, false
	}
	return score / denom, true
}

// tsmomConfidence linearly ramps Confidence between MinAbsT and
// MaxSatT so a barely-above-threshold signal fires with 0.55 and
// a saturated signal fires with 0.95. Below MinAbsT returns 0
// (caller is expected to gate); above MaxSatT clamps at 0.95.
//
// The 0.55 floor matches the trend / mean-reversion sleeves'
// "minimum to fire" so cross-sleeve MinConfidence policies don't
// silently kill TSMOM signals that the operator left at the
// default 0.5 floor.
func tsmomConfidence(absT, minAbsT, maxSatT float64) float64 {
	if absT < minAbsT {
		return 0
	}
	if maxSatT <= minAbsT {
		// Degenerate config (caller bug): saturate immediately
		// so we still surface a usable signal.
		return 0.95
	}
	const minConf = 0.55
	const maxConf = 0.95
	if absT >= maxSatT {
		return maxConf
	}
	ratio := (absT - minAbsT) / (maxSatT - minAbsT)
	return minConf + ratio*(maxConf-minConf)
}

