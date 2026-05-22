// Liquidity rule: warns when a trade is large relative to historical volume.
package risk

import (
	"context"
	"fmt"
)

// LiquidityLimit warns when (trade_qty / avg_daily_volume) exceeds Max.
//
// Symbols without volume metadata are reported as "liquidity unknown" with
// info severity rather than being treated as illiquid (which produced
// permanent false-positive warnings in the legacy code path).
type LiquidityLimit struct {
	Max float64 // e.g. 0.10 = trade ≤10% of avg daily volume
}

func (r LiquidityLimit) Name() string { return "liquidity_check" }

func (r LiquidityLimit) Evaluate(_ context.Context, pc PlanContext) ([]Finding, error) {
	var out []Finding
	for _, t := range pc.Trades {
		avgVol, ok := pc.Market.AvgVolume[t.Symbol]
		if !ok || avgVol <= 0 {
			out = append(out, Finding{
				Rule:      r.Name(),
				Severity:  SeverityInfo,
				Symbol:    t.Symbol,
				Threshold: r.Max,
				Message:   fmt.Sprintf("%s liquidity unknown (no avg volume data)", t.Symbol),
			})
			continue
		}
		ratio := t.Quantity / avgVol
		f := Finding{
			Rule:      r.Name(),
			Symbol:    t.Symbol,
			Current:   ratio,
			Threshold: r.Max,
			Message:   fmt.Sprintf("%s order %.0f is %s of avg volume", t.Symbol, t.Quantity, fmtPct(ratio)),
		}
		if ratio > r.Max {
			f.Severity = SeverityWarn
			f.Message = fmt.Sprintf("%s order %.0f is %s of avg volume — liquidity risk",
				t.Symbol, t.Quantity, fmtPct(ratio))
			f.Suggestion = fmt.Sprintf("Split %s order via TWAP/VWAP to reduce impact", t.Symbol)
		} else {
			f.Severity = SeverityInfo
		}
		out = append(out, f)
	}
	return out, nil
}
