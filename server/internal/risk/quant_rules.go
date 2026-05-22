// VaR (Value-at-Risk), correlation, and stress-test rules.
package risk

import (
	"context"
	"fmt"
	"math"
	"sort"
)

// HistoricalVaRLimit fails when the portfolio's historical VaR exceeds a
// configured fraction of TotalAssets.
//
// VaR is computed at a configured confidence level (e.g. 0.95 = 95% VaR)
// using the historical-simulation method on per-symbol return series. The
// portfolio's daily P&L distribution is the post-trade-weighted sum of each
// symbol's historical return series. The VaR is the negative of the
// confidence-level quantile of that distribution.
//
// MinSamples is the minimum length of every symbol's return series required
// to run the rule; below that, the rule emits an info finding instead.
type HistoricalVaRLimit struct {
	Confidence float64 // e.g. 0.95
	Max        float64 // e.g. 0.05 = 5% of NAV
	MinSamples int     // default 20
}

func (r HistoricalVaRLimit) Name() string { return "historical_var" }

func (r HistoricalVaRLimit) Evaluate(_ context.Context, pc PlanContext) ([]Finding, error) {
	if pc.TotalAssets <= 0 {
		return nil, nil
	}
	conf := r.Confidence
	if conf <= 0 || conf >= 1 {
		conf = 0.95
	}
	minN := r.MinSamples
	if minN <= 0 {
		minN = 20
	}

	exposure := projectedExposurePostTrade(pc)
	weights := map[string]float64{}
	totalAbs := 0.0
	for sym, mv := range exposure {
		if mv == 0 {
			continue
		}
		weights[sym] = mv / pc.TotalAssets
		totalAbs += math.Abs(weights[sym])
	}
	if totalAbs == 0 {
		return []Finding{{
			Rule:     r.Name(),
			Severity: SeverityInfo,
			Message:  "no exposure - VaR not applicable",
		}}, nil
	}

	// Determine the common sample count: the shortest series across symbols
	// that have weight. Any symbol with no series is treated as missing
	// data and downgrades the rule to info.
	commonN := math.MaxInt32
	for sym := range weights {
		series, ok := pc.Market.HistoricalReturns[sym]
		if !ok || len(series) < minN {
			return []Finding{{
				Rule:      r.Name(),
				Severity:  SeverityInfo,
				Symbol:    sym,
				Threshold: r.Max,
				Message:   fmt.Sprintf("VaR skipped: insufficient return history for %s", sym),
			}}, nil
		}
		if len(series) < commonN {
			commonN = len(series)
		}
	}
	if commonN == math.MaxInt32 {
		return nil, nil
	}

	// Build the portfolio's daily-return distribution.
	pnl := make([]float64, commonN)
	for sym, w := range weights {
		series := pc.Market.HistoricalReturns[sym]
		// Align tails of the series to the common window.
		offset := len(series) - commonN
		for i := 0; i < commonN; i++ {
			pnl[i] += w * series[offset+i]
		}
	}
	sort.Float64s(pnl)
	idx := int(math.Floor((1.0 - conf) * float64(len(pnl))))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(pnl) {
		idx = len(pnl) - 1
	}
	v := -pnl[idx] // VaR is positive when there's loss
	if v < 0 {
		v = 0
	}

	f := Finding{
		Rule:      r.Name(),
		Current:   v,
		Threshold: r.Max,
		Message:   fmt.Sprintf("%.0f%% historical VaR is %s of NAV", conf*100, fmtPct(v)),
	}
	if v > r.Max {
		f.Severity = SeverityFail
		f.Message = fmt.Sprintf("%.0f%% historical VaR %s exceeds %s limit", conf*100, fmtPct(v), fmtPct(r.Max))
		f.Suggestion = "Reduce position sizes or hedge exposure"
	} else {
		f.Severity = SeverityInfo
	}
	return []Finding{f}, nil
}

// CorrelationLimit warns when two newly bought symbols both exceed a weight
// threshold AND their pairwise return correlation exceeds Max. The intent is
// to flag accidental "all-in on one factor" plans.
type CorrelationLimit struct {
	Max         float64 // e.g. 0.85
	MinWeight   float64 // e.g. 0.05 - only flag pairs that materially overlap
	MinSamples  int     // default 20
}

func (r CorrelationLimit) Name() string { return "correlation_limit" }

func (r CorrelationLimit) Evaluate(_ context.Context, pc PlanContext) ([]Finding, error) {
	if pc.TotalAssets <= 0 {
		return nil, nil
	}
	minN := r.MinSamples
	if minN <= 0 {
		minN = 20
	}
	minW := r.MinWeight
	if minW <= 0 {
		minW = 0.01
	}

	// Collect post-trade weights for symbols touched by trades.
	post := projectedExposurePostTrade(pc)
	touched := map[string]bool{}
	for _, t := range pc.Trades {
		if !t.Side.IsSell() {
			touched[t.Symbol] = true
		}
	}
	weights := map[string]float64{}
	for sym := range touched {
		w := post[sym] / pc.TotalAssets
		if w >= minW {
			weights[sym] = w
		}
	}
	if len(weights) < 2 {
		return nil, nil
	}

	// Sorted symbol list for deterministic output.
	syms := make([]string, 0, len(weights))
	for s := range weights {
		syms = append(syms, s)
	}
	sort.Strings(syms)

	var out []Finding
	for i := 0; i < len(syms); i++ {
		for j := i + 1; j < len(syms); j++ {
			a, b := syms[i], syms[j]
			c, ok := lookupOrComputeCorrelation(pc.Market, a, b, minN)
			if !ok {
				continue
			}
			if c <= r.Max {
				continue
			}
			out = append(out, Finding{
				Rule:      r.Name(),
				Severity:  SeverityWarn,
				Symbol:    a + "+" + b,
				Current:   c,
				Threshold: r.Max,
				Message: fmt.Sprintf("%s and %s correlation %.2f exceeds %.2f (weights %s, %s)",
					a, b, c, r.Max, fmtPct(weights[a]), fmtPct(weights[b])),
				Suggestion: fmt.Sprintf("Reduce overlap between %s and %s or hedge", a, b),
			})
		}
	}
	return out, nil
}

func lookupOrComputeCorrelation(m MarketSnapshot, a, b string, minN int) (float64, bool) {
	if m.Correlations != nil {
		if row, ok := m.Correlations[a]; ok {
			if c, ok := row[b]; ok {
				return c, true
			}
		}
		if row, ok := m.Correlations[b]; ok {
			if c, ok := row[a]; ok {
				return c, true
			}
		}
	}
	xa, oka := m.HistoricalReturns[a]
	xb, okb := m.HistoricalReturns[b]
	if !oka || !okb {
		return 0, false
	}
	n := len(xa)
	if len(xb) < n {
		n = len(xb)
	}
	if n < minN {
		return 0, false
	}
	xa = xa[len(xa)-n:]
	xb = xb[len(xb)-n:]
	return pearson(xa, xb), true
}

func pearson(x, y []float64) float64 {
	n := float64(len(x))
	if n == 0 {
		return 0
	}
	var sx, sy float64
	for i := range x {
		sx += x[i]
		sy += y[i]
	}
	mx, my := sx/n, sy/n
	var num, dx2, dy2 float64
	for i := range x {
		dx, dy := x[i]-mx, y[i]-my
		num += dx * dy
		dx2 += dx * dx
		dy2 += dy * dy
	}
	denom := math.Sqrt(dx2 * dy2)
	if denom == 0 {
		return 0
	}
	return num / denom
}

// StressTestLimit applies named stress shocks (typically from MarketSnapshot)
// to the post-trade portfolio and warns/fails when the worst-case loss
// exceeds Max as a fraction of TotalAssets.
//
// Each scenario in shocks is a map from sector (or "*" for portfolio-wide)
// to a fractional shock (e.g. -0.20). The rule sums sector-weighted P&L and
// reports the largest loss across scenarios.
type StressTestLimit struct {
	Max      float64
	FailAt   float64 // optional; when set, losses above FailAt mark as fail
	Scenarios []string // optional whitelist; empty means use everything in MarketSnapshot.StressShocks
}

func (r StressTestLimit) Name() string { return "stress_test" }

func (r StressTestLimit) Evaluate(_ context.Context, pc PlanContext) ([]Finding, error) {
	if pc.TotalAssets <= 0 || len(pc.Market.StressShocks) == 0 {
		return nil, nil
	}
	scenarios := r.Scenarios
	if len(scenarios) == 0 {
		for k := range pc.Market.StressShocks {
			scenarios = append(scenarios, k)
		}
		sort.Strings(scenarios)
	}
	sectorExp := sectorExposurePostTrade(pc)
	totalExp := portfolioValue(pc)
	for _, t := range pc.Trades {
		delta := t.Notional()
		if t.Side.IsSell() {
			delta = -delta
		}
		totalExp += delta
	}

	var out []Finding
	for _, name := range scenarios {
		shocks, ok := pc.Market.StressShocks[name]
		if !ok {
			continue
		}
		loss := 0.0
		// Sector-specific shocks
		for sector, ratio := range shocks {
			if sector == "*" {
				continue
			}
			loss += sectorExp[sector] * ratio
		}
		// Portfolio-wide fallback
		if pwShock, ok := shocks["*"]; ok {
			loss += totalExp * pwShock
		}
		lossPct := -loss / pc.TotalAssets // positive = loss
		f := Finding{
			Rule:      r.Name(),
			Symbol:    name,
			Current:   lossPct,
			Threshold: r.Max,
			Message:   fmt.Sprintf("scenario %q stress loss %s of NAV", name, fmtPct(lossPct)),
		}
		switch {
		case r.FailAt > 0 && lossPct > r.FailAt:
			f.Severity = SeverityFail
			f.Message = fmt.Sprintf("scenario %q stress loss %s exceeds fail threshold %s", name, fmtPct(lossPct), fmtPct(r.FailAt))
			f.Suggestion = "Reduce or hedge the most exposed sectors"
		case lossPct > r.Max:
			f.Severity = SeverityWarn
			f.Message = fmt.Sprintf("scenario %q stress loss %s exceeds %s limit", name, fmtPct(lossPct), fmtPct(r.Max))
			f.Suggestion = "Consider reducing exposure to sectors driving the loss"
		default:
			f.Severity = SeverityInfo
		}
		out = append(out, f)
	}
	return out, nil
}
