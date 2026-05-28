package factorlab

import (
	"fmt"
	"sort"
	"strings"
)

// RenderMarkdown returns a side-by-side comparison table of the
// supplied results, intended for piping into a markdown viewer
// or pasting into a PR description.
//
// The strategy with the highest Sharpe is annotated with `*` so
// the eye lands on it immediately; strategies whose Sharpe is
// strictly LOWER than the equal_weight_long baseline are
// annotated with `!` (the cross-section is paying you nothing —
// the factor is either noise or its alpha got eaten by
// turnover).
func RenderMarkdown(results []Result) string {
	if len(results) == 0 {
		return "_no results_\n"
	}
	var (
		bestSharpe = results[0].Sharpe
		baseline   = 0.0
	)
	for _, r := range results {
		if r.Sharpe > bestSharpe {
			bestSharpe = r.Sharpe
		}
		if r.Strategy == "equal_weight_long" {
			baseline = r.Sharpe
		}
	}

	sorted := make([]Result, len(results))
	copy(sorted, results)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Sharpe > sorted[j].Sharpe
	})

	var b strings.Builder
	b.WriteString("# Factorlab MVP Backtest Report\n\n")
	if len(sorted) > 0 {
		b.WriteString(fmt.Sprintf("- Window: `%s` → `%s` (%d trading days)\n",
			sorted[0].StartDate.Format("2006-01-02"),
			sorted[0].EndDate.Format("2006-01-02"),
			sorted[0].TradingDays))
		b.WriteString(fmt.Sprintf("- Start NAV: `%.2f`, slippage: `%.1f bps` per turnover dollar\n",
			sorted[0].StartNav, sorted[0].Slippage))
	}
	b.WriteString("\n")
	b.WriteString("| Rank | Strategy | TotalRet | AnnRet | AnnVol | Sharpe | MaxDD | HitRate | Worst | Best |\n")
	b.WriteString("|------|----------|---------:|-------:|-------:|-------:|------:|--------:|------:|-----:|\n")
	for i, r := range sorted {
		marker := ""
		if r.Sharpe == bestSharpe {
			marker = " *"
		} else if r.Strategy != "equal_weight_long" && r.Sharpe < baseline {
			marker = " !"
		}
		b.WriteString(fmt.Sprintf("| %d | `%s`%s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			i+1,
			r.Strategy, marker,
			pct(r.TotalReturn),
			pct(r.AnnualReturn),
			pct(r.AnnualVol),
			f3(r.Sharpe),
			pct(r.MaxDrawdown),
			pct(r.HitRate),
			pct(r.WorstDay),
			pct(r.BestDay),
		))
	}
	b.WriteString("\nLegend: `*` = best Sharpe in cohort. `!` = Sharpe < equal_weight_long baseline.\n")
	b.WriteString("\n")
	b.WriteString("## Methodology\n\n")
	b.WriteString("- All strategies run on the same fixture, same trading-day cadence, same start NAV.\n")
	b.WriteString("- Long-only; cash earns zero. Turnover charged at the configured slippage rate.\n")
	b.WriteString("- Sharpe is annualised assuming zero risk-free rate (suitable for sleeve comparison; absolute Sharpe is sensitive to r_f assumption).\n")
	b.WriteString("- MaxDD is peak-to-trough on the equity curve, reported as a negative number.\n")
	b.WriteString("- Synthetic-fixture results are illustrative — they prove the math but cannot validate the SIGN of alpha vs the real cross-section. Use a frozen real-OHLC fixture for production validation.\n")
	return b.String()
}

func pct(v float64) string {
	if v == 0 {
		return "0.0%"
	}
	return fmt.Sprintf("%.2f%%", v*100)
}

func f3(v float64) string {
	return fmt.Sprintf("%.3f", v)
}
