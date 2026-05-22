package attribution

import (
	"fmt"
	"sort"
	"strings"

	"github.com/fundai/server/internal/repository"
)

// PromptScorecard is the LLM-facing slice of an AttributionReport.
// It's the read-only "lessons learned so far" view the PMAgent
// hands to the LLM decision engine each call. Two design rules:
//
//   - Compact. The LLM has a finite attention budget; we forward
//     at most TopN winners + BottomN losers from the (sleeve,
//     regime) cross-tab, not the entire table. Single-axis
//     rollups (BySleeve / ByRegime) are dropped — they're already
//     summed inside the cross-tab and pure repetition wastes
//     tokens.
//   - Deterministic. Same input ↦ same output. Sorting is by
//     TotalPnL DESC then Sleeve/Regime so the order is stable
//     between runs.
//
// The text in Summary is what we paste into the prompt. Rows is
// kept as structured data for tests and for the future case
// where we want to surface the same scorecard on the dashboard
// without re-aggregating.
type PromptScorecard struct {
	// Window labels the time range the stats cover. "last 30 days".
	Window string

	// Rows are the survivors after the win/loss filter — at most
	// TopN positives and BottomN negatives. Already sorted by
	// PnL descending.
	Rows []PromptScorecardRow

	// Summary is a multi-line, prompt-ready rendering of Rows.
	// Empty when Rows is empty. The wiring layer should
	// short-circuit on the empty case rather than emit a
	// "no data" header (the LLM doesn't need to know we
	// looked and found nothing).
	Summary string
}

// PromptScorecardRow is one row of the (sleeve, regime) cross-tab
// that we deemed worth showing the LLM. We strip down to the
// stats the LLM actually needs to reason about — win rate, total
// P&L, and trade count for the "is this sample big enough?" check.
type PromptScorecardRow struct {
	Sleeve     string
	Regime     string
	TradeCount int
	WinRate    float64 // 0..1
	TotalPnL   float64 // in fund base currency
	AvgPnLPct  float64 // average per-lot return as decimal (0.012 = 1.2%)
}

// PromptScorecardOptions tunes BuildPromptScorecard. Zero values
// fall back to defaults via effective().
type PromptScorecardOptions struct {
	// TopN is the maximum count of positive-P&L rows we forward
	// to the LLM. Default: 3.
	TopN int
	// BottomN is the maximum count of negative-P&L rows. Default: 3.
	BottomN int
	// MinSampleSize is the smallest trade count a row must reach
	// before we forward it. Filters noise: a row with 1 trade
	// is ~50% likely to be a fluke regardless of which side it's
	// on. Default: matches DefaultMinSampleSize (5).
	MinSampleSize int
}

func (o PromptScorecardOptions) effective() PromptScorecardOptions {
	out := o
	if out.TopN <= 0 {
		out.TopN = 3
	}
	if out.BottomN <= 0 {
		out.BottomN = 3
	}
	if out.MinSampleSize <= 0 {
		out.MinSampleSize = DefaultMinSampleSize
	}
	return out
}

// BuildPromptScorecard distils an AttributionReport into the
// prompt-friendly view. Returns the zero value when the report
// has no usable data — the caller should test Summary != "" before
// pasting into the LLM prompt.
//
// Algorithm:
//
//  1. Filter the cross-tab to rows that meet MinSampleSize.
//  2. Sort by TotalPnL descending.
//  3. Take the head TopN (positive PnL) and the tail BottomN
//     (negative PnL). A row sitting at zero gets bucketed with
//     the positive side — "broke even" is informative enough.
//  4. Render Summary as a two-section markdown block: winners
//     first, losers second. The LLM scans top-to-bottom; we
//     want it to internalise the wins before it sees the
//     warnings, mirroring how a human PM reviews scorecards.
func BuildPromptScorecard(report AttributionReport, opts PromptScorecardOptions) PromptScorecard {
	o := opts.effective()
	if !report.HasData() {
		return PromptScorecard{}
	}

	// Step 1: filter on sample size.
	filtered := make([]repository.SleeveRegimeStat, 0, len(report.BySleeveRegime))
	for _, s := range report.BySleeveRegime {
		if s.TradeCount < o.MinSampleSize {
			continue
		}
		if strings.TrimSpace(s.Sleeve) == "" || strings.TrimSpace(s.Regime) == "" {
			continue
		}
		filtered = append(filtered, s)
	}
	if len(filtered) == 0 {
		return PromptScorecard{}
	}

	// Step 2: sort by TotalPnL DESC, tie-break by Sleeve+Regime
	// for deterministic output.
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].TotalPnL != filtered[j].TotalPnL {
			return filtered[i].TotalPnL > filtered[j].TotalPnL
		}
		if filtered[i].Sleeve != filtered[j].Sleeve {
			return filtered[i].Sleeve < filtered[j].Sleeve
		}
		return filtered[i].Regime < filtered[j].Regime
	})

	// Step 3: split into winners / losers.
	winners := make([]repository.SleeveRegimeStat, 0)
	losers := make([]repository.SleeveRegimeStat, 0)
	for _, s := range filtered {
		if s.TotalPnL >= 0 {
			winners = append(winners, s)
		} else {
			losers = append(losers, s)
		}
	}
	if len(winners) > o.TopN {
		winners = winners[:o.TopN]
	}
	// Losers are at the tail; take the last BottomN preserving
	// descending order then reverse so the prompt reads from
	// "least bad" to "worst" — a small but consistent narrative.
	if len(losers) > o.BottomN {
		losers = losers[len(losers)-o.BottomN:]
	}

	rows := make([]PromptScorecardRow, 0, len(winners)+len(losers))
	for _, s := range winners {
		rows = append(rows, toScorecardRow(s))
	}
	for _, s := range losers {
		rows = append(rows, toScorecardRow(s))
	}
	if len(rows) == 0 {
		return PromptScorecard{}
	}

	return PromptScorecard{
		Window:  windowLabel(report.Window.Days),
		Rows:    rows,
		Summary: renderSummary(report.Window.Days, winners, losers),
	}
}

func toScorecardRow(s repository.SleeveRegimeStat) PromptScorecardRow {
	return PromptScorecardRow{
		Sleeve:     s.Sleeve,
		Regime:     s.Regime,
		TradeCount: s.TradeCount,
		WinRate:    s.WinRate,
		TotalPnL:   s.TotalPnL,
		AvgPnLPct:  s.AvgPnLPct,
	}
}

func windowLabel(days int) string {
	if days <= 0 {
		return ""
	}
	return fmt.Sprintf("last %d days", days)
}

// renderSummary turns the survivors into a prompt-ready Markdown
// snippet. The exact format is part of the contract with the
// system prompt — the system prompt instructs the LLM how to
// read this — so changes here MUST be matched with updates to
// the system prompt rules.
func renderSummary(days int, winners, losers []repository.SleeveRegimeStat) string {
	if len(winners) == 0 && len(losers) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Strategy scorecard (last %d days, closed roundtrips only):\n", days)
	if len(winners) > 0 {
		sb.WriteString("Winners (this combo has historically paid off; lean in when conditions match):\n")
		for _, w := range winners {
			fmt.Fprintf(&sb,
				"  - sleeve=%s regime=%s n=%d win_rate=%.0f%% total_pnl=$%+.2f avg_pnl=%+.2f%%\n",
				w.Sleeve, w.Regime, w.TradeCount, w.WinRate*100, w.TotalPnL, w.AvgPnLPct*100,
			)
		}
	}
	if len(losers) > 0 {
		sb.WriteString("Losers (this combo has bled money; require strong independent evidence to override):\n")
		for _, l := range losers {
			fmt.Fprintf(&sb,
				"  - sleeve=%s regime=%s n=%d win_rate=%.0f%% total_pnl=$%+.2f avg_pnl=%+.2f%%\n",
				l.Sleeve, l.Regime, l.TradeCount, l.WinRate*100, l.TotalPnL, l.AvgPnLPct*100,
			)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}
