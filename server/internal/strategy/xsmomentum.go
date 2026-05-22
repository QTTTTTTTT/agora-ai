package strategy

import (
	"fmt"
	"math"
	"sort"

	"github.com/fundai/server/internal/ohlc"
	"github.com/fundai/server/internal/regime"
)

// ---------------------------------------------------------------------------
// Cross-sectional momentum sleeve
// ---------------------------------------------------------------------------
//
// Unlike `trend`, `mean_reversion`, and `dual_ma` — all per-instrument
// signals — cross-sectional momentum needs the full set of bundles
// to make a ranking decision. We surface that capability through the
// BatchSleeve interface (see service.go); strategy.Service detects
// it and routes the batch through EvaluateBatch instead of calling
// Evaluate per bundle.
//
// Signal: "12-1" momentum (classical academic anomaly).
//
//   score(bundle) = (price[t-skip] - price[t-(lookback)]) / price[t-(lookback)]
//
//   With defaults skip=21, lookback=252 (daily bars):
//     - 12 months back to 1 month ago: drop the latest month to
//       dodge short-term reversal (Jegadeesh, 1990; Asness, 1994).
//
// Action selection:
//   - Rank bundles by score in descending order.
//   - Top `Quintile` fraction of valid bundles → BUY proposal
//   - Bottom `Quintile` fraction                → SELL proposal
//     (only meaningful when the fund already holds the name; the
//     merge layer drops orphan SELLs the same way it does for the
//     trend and mean_reversion sleeves).
//   - Middle band                                → nil
//
// Regime gate: cross-sectional momentum is famously a trend
// anomaly — it underperforms during regime switches and noisy
// chop. We require TrendUp or TrendDown for the bundle's regime
// tag to fire. Note the gate is per-bundle, not global: a
// momentum winner that's just rolled into a "chop" regime is
// dropped from the BUY bucket even if its score still ranks.

const (
	// xsMomentumSignalSource is the persisted signal_source tag.
	xsMomentumSignalSource = "xs_momentum_12_1"
)

// CrossSectionalMomentumSleeve implements both Sleeve and
// BatchSleeve. The per-bundle Evaluate falls back to "no opinion"
// because the signal is only meaningful in a cross section; the
// real work happens in EvaluateBatch.
type CrossSectionalMomentumSleeve struct {
	params CrossSectionalMomentumParams
}

// NewCrossSectionalMomentumSleeve constructs a sleeve with the
// supplied params. The caller is expected to pass an
// EffectivePolicy-normalised CrossSectionalMomentumParams (or
// defaultXSMomentum()); zero values are NOT back-filled here.
func NewCrossSectionalMomentumSleeve(params CrossSectionalMomentumParams) *CrossSectionalMomentumSleeve {
	return &CrossSectionalMomentumSleeve{params: params}
}

// Name implements Sleeve.
func (x *CrossSectionalMomentumSleeve) Name() string { return "xs_momentum" }

// PreferredRegimes pins the sleeve to confirmed trends. See the
// package doc comment for the rationale.
func (x *CrossSectionalMomentumSleeve) PreferredRegimes() []regime.Regime {
	return []regime.Regime{regime.TrendUp, regime.TrendDown}
}

// Evaluate implements Sleeve. Always returns nil — cross-
// sectional momentum is meaningful only as a ranking, not a
// per-instrument signal. The Service's BatchSleeve detection
// uses EvaluateBatch for this sleeve and never calls Evaluate.
//
// We still implement it (as a deterministic nil) so the sleeve
// satisfies Sleeve and can be slotted into the per-bundle
// loop in case a caller forgets the BatchSleeve dispatch — that
// path returns "no opinion" rather than crashing.
func (x *CrossSectionalMomentumSleeve) Evaluate(_ Bundle) *Proposal {
	return nil
}

// momentumScore computes the (priceNow - pricePast) / pricePast
// using close prices. Returns (score, true) on success, (0, false)
// when there isn't enough history for the configured window.
//
// "now" is at index len-1-skip; "then" is at index len-1-lookback.
// Both indices have to be valid; the skip window means the most
// recent `skip` bars are intentionally ignored.
func (x *CrossSectionalMomentumSleeve) momentumScore(bars []ohlc.Bar) (float64, bool) {
	p := x.params
	if len(bars) <= p.LookbackBars {
		return 0, false
	}
	last := len(bars) - 1
	nowIdx := last - p.SkipBars
	thenIdx := last - p.LookbackBars
	if nowIdx <= thenIdx || nowIdx <= 0 || thenIdx < 0 {
		return 0, false
	}
	pricePast := bars[thenIdx].Close
	priceNow := bars[nowIdx].Close
	if pricePast <= 0 || priceNow <= 0 || math.IsNaN(pricePast) || math.IsNaN(priceNow) {
		return 0, false
	}
	return (priceNow - pricePast) / pricePast, true
}

// EvaluateBatch implements BatchSleeve. Returns a slice the same
// length as `bundles`, with proposal[i] == nil for any bundle the
// sleeve has no opinion on. The Service uses this 1:1 alignment
// to merge per-instrument metadata; we don't carry per-bundle
// identity inside the proposals themselves.
//
// Panic safety: if the sleeve is misconfigured at the boundary
// where Quintile=0, every bundle lands in the middle band and
// the function returns all-nil. That's intentional — a
// fat-fingered config should silently no-op, not crash.
func (x *CrossSectionalMomentumSleeve) EvaluateBatch(bundles []Bundle) []*Proposal {
	out := make([]*Proposal, len(bundles))
	if x == nil || len(bundles) < x.params.MinUniverseSize {
		return out
	}

	type ranked struct {
		idx     int
		score   float64
		regimeR regime.Regime
	}
	scored := make([]ranked, 0, len(bundles))
	for i, b := range bundles {
		if b.Symbol == "" {
			continue
		}
		score, ok := x.momentumScore(b.Bars)
		if !ok {
			continue
		}
		scored = append(scored, ranked{idx: i, score: score, regimeR: b.Regime})
	}
	if len(scored) < x.params.MinUniverseSize {
		return out
	}

	// Sort by score descending. Stable sort so ties keep their
	// original ordering — important for determinism: replaying
	// the same bundle slice always emits the same proposals.
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	// Quintile cuts. We use ceil() so a 5-name universe with
	// quintile=0.2 yields exactly 1 BUY and 1 SELL — the
	// expected "extremes only" behaviour. A 4-name universe
	// with quintile=0.2 would yield 1+1 too, which is also the
	// classroom answer (top and bottom name).
	bucket := int(math.Ceil(float64(len(scored)) * x.params.Quintile))
	if bucket < 1 {
		bucket = 1
	}
	// Defensive: don't let BUY and SELL buckets overlap on a
	// tiny universe. With quintile clamped to 0.5 upstream this
	// only kicks in when the user manually fed us a degenerate
	// list; we still want to gracefully bail.
	if 2*bucket > len(scored) {
		bucket = len(scored) / 2
		if bucket < 1 {
			return out
		}
	}

	// Score spread, used by xsMomentumConfidence so an extreme
	// winner in a flat cross section gets a higher confidence
	// than a top name in a tightly-bunched group.
	bestScore := scored[0].score
	worstScore := scored[len(scored)-1].score
	spread := bestScore - worstScore
	if spread <= 0 {
		spread = 1e-9
	}

	for rankIdx := 0; rankIdx < bucket; rankIdx++ {
		r := scored[rankIdx]
		if !AllowsRegime(x.PreferredRegimes(), r.regimeR) {
			continue
		}
		strength := (r.score - worstScore) / spread // 1.0 at the top
		conf := xsMomentumConfidence(strength)
		b := bundles[r.idx]
		closeNow := lastCloseOf(b.Bars)
		reasoning := fmt.Sprintf(
			"xs_momentum(12-1): %s ranks #%d/%d by %d-bar return (skip %d), score %.2f%% (regime=%s, top quintile %.0f%%)",
			b.Symbol, rankIdx+1, len(scored), x.params.LookbackBars, x.params.SkipBars, r.score*100, r.regimeR, x.params.Quintile*100,
		)
		out[r.idx] = &Proposal{
			Action:       ActionBuy,
			Confidence:   conf,
			Reasoning:    reasoning,
			StopLoss:     stopLossPrice(closeNow, x.params.StopLossPct, ActionBuy),
			SignalSource: xsMomentumSignalSource,
		}
	}

	// Bottom bucket: SELL proposals. Symmetric strength formula
	// (distance from the best, normalised by spread) so the
	// most-negative momentum name gets the highest sell
	// confidence.
	for rankFromBottom := 0; rankFromBottom < bucket; rankFromBottom++ {
		rankIdx := len(scored) - 1 - rankFromBottom
		r := scored[rankIdx]
		if !AllowsRegime(x.PreferredRegimes(), r.regimeR) {
			continue
		}
		strength := (bestScore - r.score) / spread // 1.0 at the bottom
		conf := xsMomentumConfidence(strength)
		b := bundles[r.idx]
		closeNow := lastCloseOf(b.Bars)
		reasoning := fmt.Sprintf(
			"xs_momentum(12-1): %s ranks #%d/%d FROM THE BOTTOM by %d-bar return (skip %d), score %.2f%% (regime=%s, bottom quintile %.0f%%)",
			b.Symbol, rankFromBottom+1, len(scored), x.params.LookbackBars, x.params.SkipBars, r.score*100, r.regimeR, x.params.Quintile*100,
		)
		out[r.idx] = &Proposal{
			Action:       ActionSell,
			Confidence:   conf,
			Reasoning:    reasoning,
			StopLoss:     stopLossPrice(closeNow, x.params.StopLossPct, ActionSell),
			SignalSource: xsMomentumSignalSource,
		}
	}
	return out
}

// xsMomentumConfidence maps normalised rank-strength (1.0 at the
// extreme of the cross section, 0.0 at the middle) to the same
// [0.55, 0.95] confidence band the per-instrument sleeves use.
// A linear ramp keeps the formula easy to reason about; future
// iterations could swap in a sigmoid if attribution shows the
// extremes underperform.
func xsMomentumConfidence(rankStrength float64) float64 {
	if rankStrength <= 0 {
		return 0.55
	}
	if rankStrength >= 1 {
		return 0.95
	}
	return 0.55 + rankStrength*(0.95-0.55)
}

// lastCloseOf returns the close of the most recent bar, or 0 if
// the slice is empty. Used so the stop-loss helper has something
// to anchor against without re-running indicator.Closes for one
// number.
func lastCloseOf(bars []ohlc.Bar) float64 {
	if len(bars) == 0 {
		return 0
	}
	return bars[len(bars)-1].Close
}
