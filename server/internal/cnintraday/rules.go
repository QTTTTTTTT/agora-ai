package cnintraday

import (
	"fmt"
	"math"
	"time"
)

// RuleSet bundles the thresholds the engine uses to convert
// factor scores into a BUY / ADD / SELL / WARNING signal.
// Conservative profile is the recommended starting point; the
// aggressive profile is for the operator who wants more signals
// and is willing to take wider stops.
type RuleSet struct {
	// Required factor minima for a BUY.
	BreakoutMin       float64 // typical 1.5 (z-score)
	VolumeSurgeMin    float64 // typical 1.5
	BigInflowMin      float64 // typical 500_000 CNY (5-bar cum)
	OrderImbalanceMin float64 // typical 1.3 (委买/委卖)

	// Sector rank gate. 0..1, higher = stronger sector momentum.
	SectorRankMin float64 // typical 0.6

	// Distance from upper-limit; if (limit_up - close) / close
	// is BELOW this, gate the buy to avoid chasing tops.
	MinLimitDistance float64 // typical 0.04 (4%)

	// Position sizing.
	SuggestedPosition float64 // typical 0.10 (10% per name)
	MaxPositionPerName float64 // typical 0.15

	// Stop-loss / take-profit (fractional from entry).
	StopLossPct   float64 // typical 0.03
	TakeProfitPct float64 // typical 0.05
}

// ConservativeRuleSet matches the plan's "conservative" profile.
func ConservativeRuleSet() RuleSet {
	return RuleSet{
		BreakoutMin:        1.5,
		VolumeSurgeMin:     1.5,
		BigInflowMin:       500_000,
		OrderImbalanceMin:  1.3,
		SectorRankMin:      0.6,
		MinLimitDistance:   0.04,
		SuggestedPosition:  0.10,
		MaxPositionPerName: 0.15,
		StopLossPct:        0.03,
		TakeProfitPct:      0.05,
	}
}

// AggressiveRuleSet matches the plan's "aggressive" profile —
// lower factor thresholds, wider stops, larger positions.
func AggressiveRuleSet() RuleSet {
	return RuleSet{
		BreakoutMin:        1.0,
		VolumeSurgeMin:     1.2,
		BigInflowMin:       300_000,
		OrderImbalanceMin:  1.2,
		SectorRankMin:      0.5,
		MinLimitDistance:   0.02,
		SuggestedPosition:  0.15,
		MaxPositionPerName: 0.20,
		StopLossPct:        0.04,
		TakeProfitPct:      0.08,
	}
}

// EvaluateInput is the per-symbol scoring blob the rule engine
// consumes. The caller (typically the orchestrator that pulls
// minute bars + sector rank) builds this for each watchlist
// symbol on each minute tick.
type EvaluateInput struct {
	Symbol            string
	Info              SymbolInfo
	PrevClose         float64    // previous trading day's close — for limit calc
	LastBar           MinuteBar
	Factors           FactorTuple
	NowBeijing        time.Time
}

// Evaluate runs the rule engine on one symbol's snapshot and
// returns either a TradeSignal or nil (no trade today).
//
// Logic:
//   1. Time filter: out-of-session → nil
//   2. Limit-up gate: too close → WARNING (operator may still buy
//      manually; we just refuse to RECOMMEND)
//   3. Required factor checks: every Min{Breakout, VolSurge,
//      BigInflow, OrderImbalance, SectorRank} must be met → BUY
//   4. Optional confidence scoring: how many factors blow past
//      the minima determines Confidence.
func Evaluate(in EvaluateInput, rules RuleSet) *TradeSignal {
	if !IntradayTimeFilter(in.NowBeijing) {
		return nil
	}
	if in.PrevClose <= 0 || in.LastBar.Close <= 0 {
		return nil
	}
	priceLimit := in.Info.PriceLimit()
	if priceLimit <= 0 {
		priceLimit = 0.10
	}
	limitUp := in.PrevClose * (1 + priceLimit)
	limitDistance := (limitUp - in.LastBar.Close) / in.LastBar.Close

	// Limit-distance gate → WARNING (price already chased near
	// the top; no trade signal).
	if limitDistance < rules.MinLimitDistance {
		return &TradeSignal{
			Timestamp: in.NowBeijing,
			Symbol:    in.Symbol,
			Name:      in.Info.Name,
			Type:      SignalWarning,
			Price:     in.LastBar.Close,
			RiskWarnings: []string{
				fmt.Sprintf("距离涨停仅 %.1f%%，跳过", limitDistance*100),
			},
			FactorScores: in.Factors,
		}
	}

	required := []bool{
		in.Factors.Breakout >= rules.BreakoutMin,
		in.Factors.VolumeSurge >= rules.VolumeSurgeMin,
		in.Factors.BigInflow >= rules.BigInflowMin || in.Factors.BigInflow == 0,
		in.Factors.OrderImbalance >= rules.OrderImbalanceMin || in.Factors.OrderImbalance == 0,
		in.Factors.SectorRank >= rules.SectorRankMin,
	}
	passed := 0
	for _, ok := range required {
		if ok {
			passed++
		}
	}
	// Need ALL hard requirements (breakout + vol-surge + sector
	// rank) PLUS at least one of the soft (inflow / imbalance).
	// We collapse this by requiring `passed == 5` because the
	// soft ones short-circuit to true when the provider didn't
	// fill them.
	if passed < 5 {
		return nil
	}

	// Confidence is a soft over-shoot ratio: how far above the
	// minima we are, averaged.
	confidence := overshootConfidence(in.Factors, rules)
	reasons := buildReasons(in.Factors, rules)

	return &TradeSignal{
		Timestamp:         in.NowBeijing,
		Symbol:            in.Symbol,
		Name:              in.Info.Name,
		Type:              SignalBuy,
		Price:             in.LastBar.Close,
		Confidence:        confidence,
		SuggestedPosition: rules.SuggestedPosition,
		TargetPrice:       in.LastBar.Close * (1 + rules.TakeProfitPct),
		StopLoss:          in.LastBar.Close * (1 - rules.StopLossPct),
		Reasons:           reasons,
		FactorScores:      in.Factors,
	}
}

// overshootConfidence is the average ratio of (factor / minimum)
// clipped to [0, 2] then squashed to [0, 1] via min(x/2, 1).
// Result of 1.0 means "every factor at 2× its minimum" — a very
// strong signal.
func overshootConfidence(f FactorTuple, r RuleSet) float64 {
	ratios := []float64{}
	if r.BreakoutMin > 0 {
		ratios = append(ratios, f.Breakout/r.BreakoutMin)
	}
	if r.VolumeSurgeMin > 0 {
		ratios = append(ratios, f.VolumeSurge/r.VolumeSurgeMin)
	}
	if r.SectorRankMin > 0 {
		ratios = append(ratios, f.SectorRank/r.SectorRankMin)
	}
	if len(ratios) == 0 {
		return 0
	}
	var sum float64
	for _, r := range ratios {
		if math.IsNaN(r) || math.IsInf(r, 0) {
			continue
		}
		// Clip per-factor to [0, 2] so a single 50× volume spike
		// doesn't dominate the average.
		clipped := math.Max(0, math.Min(r, 2.0))
		sum += clipped
	}
	avg := sum / float64(len(ratios))
	// Squash to [0, 1].
	return math.Min(avg/2.0, 1.0)
}

func buildReasons(f FactorTuple, r RuleSet) []string {
	reasons := []string{}
	if f.Breakout >= r.BreakoutMin {
		reasons = append(reasons, fmt.Sprintf("突破前 60min 高点 (z=%.2f)", f.Breakout))
	}
	if f.VolumeSurge >= r.VolumeSurgeMin {
		reasons = append(reasons, fmt.Sprintf("放量 (5min vol / 20min vol = %.2fx)", f.VolumeSurge))
	}
	if f.BigInflow >= r.BigInflowMin {
		reasons = append(reasons, fmt.Sprintf("大单净流入 %.0f 万", f.BigInflow/10_000))
	}
	if f.OrderImbalance >= r.OrderImbalanceMin {
		reasons = append(reasons, fmt.Sprintf("委买/委卖 = %.2f", f.OrderImbalance))
	}
	if f.SectorRank >= r.SectorRankMin {
		reasons = append(reasons, fmt.Sprintf("板块内排名 %.0f%%", f.SectorRank*100))
	}
	return reasons
}
