package factorlab

import (
	"math"
	"sort"
	"strings"
	"time"
)

// Simulator runs N strategies side-by-side on the same fixture
// and produces per-strategy NavPoint series + headline metrics.
// All strategies share the same start NAV (default 1.0) so the
// equity curves are directly comparable.
//
// The model:
//   - Each strategy carries weights for symbol → fraction of NAV.
//   - On day t, the strategy returns its desired weights for END
//     of day t. We assume those weights are achieved at the close
//     of day t and held overnight, so the realised return for the
//     strategy on day t+1 is sum(weight_s * symbol_return_{t+1}).
//   - Trading costs are modelled as a flat one-sided
//     SlippageBps (default 5 bps) charged on every dollar of
//     turnover at rebalance. Cash earns zero. Long-only.
//
// This is intentionally a SIMPLE model. The point is to compare
// factors against EACH OTHER on the same friction model, not to
// produce production-grade PnL numbers. We don't model bid-ask
// asymmetry, lot-size constraints, dividends, or partial fills.
type Simulator struct {
	// SlippageBps is the one-sided cost applied to turnover at
	// rebalance, in basis points. Default 5 bps.
	SlippageBps float64
	// StartNav scales the equity curve. Default 1.0 → NAV starts
	// at $1 so all metrics are unit-free.
	StartNav float64
	// WarmupDays skips the first N trading days from the result
	// series (most factors need some lookback before they emit
	// meaningful signal). Default = 252 + 21 = momentum lookback.
	WarmupDays int
}

// Run executes every strategy against the fixture in one pass.
// Returns one Result per strategy, in the same order as the
// input slice. nil fixture or empty strategies → empty result.
func (s *Simulator) Run(fixture *Fixture, strategies []Strategy) []Result {
	if fixture == nil || len(strategies) == 0 {
		return nil
	}
	startNav := s.StartNav
	if startNav <= 0 {
		startNav = 1.0
	}
	slipBps := s.SlippageBps
	if slipBps < 0 {
		slipBps = 0
	}
	warmup := s.WarmupDays
	if warmup <= 0 {
		warmup = 273
	}

	days := fixture.TradingDays()
	if len(days) < warmup+10 {
		// Not enough data for any factor to settle. Return
		// empty results — the CLI will surface that as
		// "fixture too short".
		return nil
	}
	results := make([]Result, len(strategies))
	for i, strat := range strategies {
		results[i] = runOne(fixture, strat, days, startNav, slipBps, warmup)
	}
	return results
}

func runOne(fix *Fixture, strat Strategy, days []time.Time, startNav, slipBps float64, warmup int) Result {
	res := Result{
		Strategy:   strat.Name(),
		StartDate:  days[warmup],
		EndDate:    days[len(days)-1],
		StartNav:   startNav,
		Slippage:   slipBps,
		Equity:     make([]NavPoint, 0, len(days)-warmup),
		DailyR:     make([]float64, 0, len(days)-warmup),
	}
	weights := map[string]float64{}
	nav := startNav

	for i := warmup; i < len(days); i++ {
		day := days[i]
		dayReturn := 0.0
		if i > warmup {
			prevDay := days[i-1]
			dayReturn = portfolioDayReturn(fix, weights, prevDay, day)
			nav *= (1 + dayReturn)
		}
		// Strategy decides weights based on yesterday's close
		// (look-ahead-free).
		asOf := day
		if i > 0 {
			asOf = days[i-1]
		}
		target := strat.Weights(fix, asOf)
		if target != nil {
			normalised := normaliseWeights(target)
			cost := turnoverCost(weights, normalised, slipBps)
			nav -= cost
			weights = normalised
		}
		res.Equity = append(res.Equity, NavPoint{Date: day, Nav: nav})
		res.DailyR = append(res.DailyR, dayReturn)
	}
	res.applyMetrics()
	return res
}

// portfolioDayReturn computes the weighted next-day return
// using the close-to-close return for each held symbol.
// Symbols absent from the fixture for one of the two dates
// contribute zero (the position is "cash" for that name).
func portfolioDayReturn(f *Fixture, weights map[string]float64, prev, curr time.Time) float64 {
	if len(weights) == 0 {
		return 0
	}
	var total float64
	for sym, w := range weights {
		prevPx, ok1 := f.CloseAt(sym, prev)
		currPx, ok2 := f.CloseAt(sym, curr)
		if !ok1 || !ok2 || prevPx <= 0 {
			continue
		}
		total += w * (currPx/prevPx - 1.0)
	}
	return total
}

// normaliseWeights clamps to [0,1] and scales DOWN so the total
// weight ≤ 1 (cash = remainder). Never scales UP — a strategy
// that emits {A:0.1, B:0.1} is allowed to be 80% cash.
func normaliseWeights(w map[string]float64) map[string]float64 {
	if len(w) == 0 {
		return map[string]float64{}
	}
	var sum float64
	out := make(map[string]float64, len(w))
	for s, v := range w {
		if v <= 0 {
			continue
		}
		if v > 1 {
			v = 1
		}
		key := strings.ToUpper(strings.TrimSpace(s))
		out[key] = v
		sum += v
	}
	if sum > 1 {
		for s := range out {
			out[s] = out[s] / sum
		}
	}
	return out
}

// turnoverCost charges slippageBps on every dollar of one-sided
// turnover (the absolute change between old and new weights,
// summed across the union of both maps).
func turnoverCost(oldW, newW map[string]float64, slipBps float64) float64 {
	if slipBps <= 0 {
		return 0
	}
	syms := unionKeys(oldW, newW)
	var turnover float64
	for _, s := range syms {
		delta := math.Abs(newW[s] - oldW[s])
		turnover += delta
	}
	return turnover * slipBps / 10000.0
}

func unionKeys(a, b map[string]float64) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
