package attribution

import (
	"fmt"
	"sort"

	"github.com/fundai/server/internal/repository"
)

// ---------------------------------------------------------------------------
// Lesson generation (pure)
// ---------------------------------------------------------------------------

// LessonOptions control thresholds. Zero values fall back to
// the Default* constants from attribution.go so callers can
// pass `LessonOptions{}` for "production defaults".
type LessonOptions struct {
	MinSampleSize    int
	MaxLessons       int
	LossWinRateMax   float64 // win rate strictly below this fires a loser lesson
	WinnerWinRateMin float64 // win rate strictly above this fires a winner lesson
}

func (o LessonOptions) effective() LessonOptions {
	out := o
	if out.MinSampleSize <= 0 {
		out.MinSampleSize = DefaultMinSampleSize
	}
	if out.MaxLessons <= 0 {
		out.MaxLessons = DefaultMaxLessonsPerRun
	}
	// Threshold defaults follow the textbook "decisive" cutoffs:
	// 35% / 65% is roughly 1.5σ on a 50% null hypothesis for
	// n ≈ 10–30 trades, which matches the sample sizes we get
	// per fund per month.
	if out.LossWinRateMax <= 0 {
		out.LossWinRateMax = 0.35
	}
	if out.WinnerWinRateMin <= 0 {
		out.WinnerWinRateMin = 0.65
	}
	return out
}

// GenerateLessons turns an attribution report into a slice of
// deterministic Lesson records. Pure function: same report →
// same lessons (in the same order). The Service deduplicates
// against the memory store before persisting, but order matters
// for the dashboard's "top N most-actionable" ranking.
func GenerateLessons(report AttributionReport, opts LessonOptions) []Lesson {
	o := opts.effective()
	if !report.HasData() {
		return []Lesson{buildInsufficientDataLesson(report)}
	}

	lessons := []Lesson{}

	// ---- Cross-tab pass: sleeve × regime ----
	// Sort by win_rate ASC so the worst losers surface first;
	// among ties, ORDER BY total_pnl ASC (most-negative pnl
	// first). Winners pass uses the symmetric reverse order.
	sortedLosers := append([]repository.SleeveRegimeStat(nil), report.BySleeveRegime...)
	sort.SliceStable(sortedLosers, func(i, j int) bool {
		if sortedLosers[i].WinRate != sortedLosers[j].WinRate {
			return sortedLosers[i].WinRate < sortedLosers[j].WinRate
		}
		return sortedLosers[i].TotalPnL < sortedLosers[j].TotalPnL
	})
	for _, s := range sortedLosers {
		if !meetsLossThreshold(s, o) {
			continue
		}
		lessons = append(lessons, buildLoserLesson(s))
	}

	sortedWinners := append([]repository.SleeveRegimeStat(nil), report.BySleeveRegime...)
	sort.SliceStable(sortedWinners, func(i, j int) bool {
		if sortedWinners[i].WinRate != sortedWinners[j].WinRate {
			return sortedWinners[i].WinRate > sortedWinners[j].WinRate
		}
		return sortedWinners[i].TotalPnL > sortedWinners[j].TotalPnL
	})
	for _, s := range sortedWinners {
		if !meetsWinThreshold(s, o) {
			continue
		}
		lessons = append(lessons, buildWinnerLesson(s))
	}

	if len(lessons) > o.MaxLessons {
		lessons = lessons[:o.MaxLessons]
	}
	return lessons
}

// meetsLossThreshold is the gate for LessonSleeveRegimeLoser.
// Triple-condition guard: enough sample, decisive win-rate
// shortfall, AND negative dollar P&L. We require ALL three so a
// strategy that wins rarely but big (lottery payoff) doesn't
// get flagged as a loser by win-rate alone.
func meetsLossThreshold(s repository.SleeveRegimeStat, o LessonOptions) bool {
	return s.TradeCount >= o.MinSampleSize &&
		s.WinRate < o.LossWinRateMax &&
		s.TotalPnL < 0
}

func meetsWinThreshold(s repository.SleeveRegimeStat, o LessonOptions) bool {
	return s.TradeCount >= o.MinSampleSize &&
		s.WinRate > o.WinnerWinRateMin &&
		s.TotalPnL > 0
}

func buildLoserLesson(s repository.SleeveRegimeStat) Lesson {
	return Lesson{
		Kind:     LessonSleeveRegimeLoser,
		Severity: SeverityCritical,
		Title: fmt.Sprintf(
			"Sleeve %q is losing money in regime %q (%d trades, win-rate %.0f%%, PnL %.2f)",
			s.Sleeve, s.Regime, s.TradeCount, s.WinRate*100, s.TotalPnL,
		),
		Body: fmt.Sprintf(
			"Across %d closed lots in regime %s, the %s sleeve recorded a %.0f%% win rate and a "+
				"cumulative realised P&L of %.2f (avg pnl pct: %.3f, avg holding %.1f days). "+
				"Consider pausing this (sleeve, regime) combination in fund.config.strategySleeves "+
				"until the conditions change, or instrumenting the entry filter further to understand "+
				"why the signal misfires in this regime.",
			s.TradeCount, s.Regime, s.Sleeve, s.WinRate*100, s.TotalPnL, s.AvgPnLPct, s.AvgHoldingDays,
		),
		Tags:           []string{"loser", "sleeve:" + s.Sleeve, "regime:" + s.Regime},
		Sleeve:         s.Sleeve,
		Regime:         s.Regime,
		TradeCount:     s.TradeCount,
		WinRate:        s.WinRate,
		TotalPnL:       s.TotalPnL,
		AvgPnLPct:      s.AvgPnLPct,
		AvgHoldingDays: s.AvgHoldingDays,
	}
}

// buildInsufficientDataLesson renders the "no closed trades yet"
// lesson with concrete inventory numbers when they're available
// so dashboards make the agent's activity legible. The lesson
// still fires only when the closed-lots window is empty, but
// instead of a flat "no data" line the body now reads like:
//
//	Watching 7 open lots opened since 2026-05-12. Attribution
//	will start scoring once the first sell closes a roundtrip.
//
// When the inventory is also empty (brand-new fund, no buys yet)
// we fall back to the original phrasing — telling the operator
// the agent hasn't had a chance to start observing anything.
func buildInsufficientDataLesson(report AttributionReport) Lesson {
	switch {
	case report.OpenLotCount > 0 && report.EarliestOpenedAt.Valid:
		earliest := report.EarliestOpenedAt.Time.UTC().Format("2006-01-02")
		return Lesson{
			Kind:     LessonInsufficientData,
			Severity: SeverityInfo,
			Title: fmt.Sprintf(
				"Watching %d open %s since %s — no closed roundtrip in the last %d days yet",
				report.OpenLotCount,
				pluralLot(report.OpenLotCount),
				earliest,
				report.Window.Days,
			),
			Body: fmt.Sprintf(
				"The attribution agent has %d still-open %s under observation (earliest opened on %s). "+
					"It will produce a per-sleeve / per-regime scorecard once the first sell closes one of them. "+
					"Until then, no win-rate or P&L lessons can be issued.",
				report.OpenLotCount, pluralLot(report.OpenLotCount), earliest,
			),
			Tags: []string{"insufficient_data", "observing"},
		}
	case report.OpenLotCount > 0:
		return Lesson{
			Kind:     LessonInsufficientData,
			Severity: SeverityInfo,
			Title: fmt.Sprintf(
				"Watching %d open %s — no closed roundtrip in the last %d days yet",
				report.OpenLotCount, pluralLot(report.OpenLotCount), report.Window.Days,
			),
			Body: fmt.Sprintf(
				"The attribution agent has %d still-open %s under observation. It will produce a per-sleeve / "+
					"per-regime scorecard once the first sell closes one of them.",
				report.OpenLotCount, pluralLot(report.OpenLotCount),
			),
			Tags: []string{"insufficient_data", "observing"},
		}
	default:
		return Lesson{
			Kind:     LessonInsufficientData,
			Severity: SeverityInfo,
			Title:    fmt.Sprintf("No closed trades in the last %d days", report.Window.Days),
			Body:     "Attribution will populate once the fund has produced its first realized P&L.",
			Tags:     []string{"insufficient_data"},
		}
	}
}

func pluralLot(n int) string {
	if n == 1 {
		return "lot"
	}
	return "lots"
}

func buildWinnerLesson(s repository.SleeveRegimeStat) Lesson {
	return Lesson{
		Kind:     LessonSleeveRegimeWinner,
		Severity: SeverityInfo,
		Title: fmt.Sprintf(
			"Sleeve %q is profitable in regime %q (%d trades, win-rate %.0f%%, PnL +%.2f)",
			s.Sleeve, s.Regime, s.TradeCount, s.WinRate*100, s.TotalPnL,
		),
		Body: fmt.Sprintf(
			"Across %d closed lots in regime %s, the %s sleeve recorded a %.0f%% win rate and a "+
				"cumulative realised P&L of +%.2f (avg pnl pct: %.3f, avg holding %.1f days). "+
				"This combination is contributing positively; the LLM PM may want to scale exposure "+
				"or relax confidence thresholds when regime=%s.",
			s.TradeCount, s.Regime, s.Sleeve, s.WinRate*100, s.TotalPnL, s.AvgPnLPct, s.AvgHoldingDays, s.Regime,
		),
		Tags:           []string{"winner", "sleeve:" + s.Sleeve, "regime:" + s.Regime},
		Sleeve:         s.Sleeve,
		Regime:         s.Regime,
		TradeCount:     s.TradeCount,
		WinRate:        s.WinRate,
		TotalPnL:       s.TotalPnL,
		AvgPnLPct:      s.AvgPnLPct,
		AvgHoldingDays: s.AvgHoldingDays,
	}
}
