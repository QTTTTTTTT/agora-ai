package attribution

import (
	"fmt"
	"sort"
	"strings"

	"github.com/fundai/server/internal/repository"
)

// ---------------------------------------------------------------------------
// Lesson generation (pure)
// ---------------------------------------------------------------------------

// LessonOptions control thresholds. Zero values fall back to the
// Default* constants below so callers can pass `LessonOptions{}`
// for "production defaults".
//
// The (sleeve, regime) loser axis is split into THREE tiers so
// the lesson reads the way a real research team writes a journal,
// not a kill-switch:
//
//   * observing  (5–10 lots, sub-par win rate)        → severity=info
//   * throttle   (10–30 lots, win rate below 40%)     → severity=warning
//   * pause      (30+ lots, win rate below 35%,
//                 cumulative P&L < 0)                 → severity=critical
//
// The single "loser" cutoff (35% on n≥5) we used to ship was
// statistically too aggressive for the sample sizes we actually
// see per fund per month — 6 trades is well below the 1.5σ
// "decisive" bound the textbook 35%/65% rule assumes. The tiered
// shape lets us *say something useful* about small samples
// without recommending a strategy change off six trades.
type LessonOptions struct {
	MaxLessons int

	// Observing tier: small-sample journal entry.
	ObservingMinSampleSize int
	ObservingMaxSampleSize int     // strictly less than this for observing tier
	ObservingWinRateMax    float64 // win-rate strictly below this fires observing

	// Throttle tier: medium-sample size reduction.
	ThrottleMinSampleSize int
	ThrottleMaxSampleSize int     // strictly less than this for throttle tier
	ThrottleWinRateMax    float64

	// Pause tier: large-sample combination shutoff.
	PauseMinSampleSize int
	PauseWinRateMax    float64
	PausePnLMax        float64 // total_pnl strictly less than this for pause

	// Winners use the original symmetric rule.
	WinnerMinSampleSize int
	WinnerWinRateMin    float64

	// Sleeve-overall fallback. Fires only when the regime
	// detector returned "" / "unspecified" for every row of a
	// given sleeve (i.e. the per-regime view is useless). At
	// that point we surface the sleeve-wide rollup so the
	// operator at least sees the overall picture instead of
	// total silence. Body text directs them to fix the regime
	// detector before drawing further conclusions.
	SleeveOverallMinSampleSize int
	SleeveOverallWinRateMax    float64
	SleeveOverallPnLMax        float64 // total_pnl strictly less than this fires the fallback

	// Back-compat knobs. Old call sites passed MinSampleSize /
	// LossWinRateMax and expected the single-tier behaviour. We
	// still honour these as a floor for the observing tier so a
	// caller that pins MinSampleSize=10 doesn't suddenly start
	// seeing the 5–9 observing lessons. The lesson generator
	// treats them as "raise the observing floor to this".
	MinSampleSize  int
	LossWinRateMax float64
}

func (o LessonOptions) effective() LessonOptions {
	out := o
	if out.MaxLessons <= 0 {
		out.MaxLessons = DefaultMaxLessonsPerRun
	}
	if out.WinnerMinSampleSize <= 0 {
		out.WinnerMinSampleSize = DefaultMinSampleSize
	}
	if out.WinnerWinRateMin <= 0 {
		out.WinnerWinRateMin = 0.65
	}

	if out.ObservingMinSampleSize <= 0 {
		out.ObservingMinSampleSize = 5
	}
	if out.ObservingMaxSampleSize <= 0 {
		out.ObservingMaxSampleSize = 10
	}
	if out.ObservingWinRateMax <= 0 {
		out.ObservingWinRateMax = 0.50 // below break-even, but no statistical claim
	}

	if out.ThrottleMinSampleSize <= 0 {
		out.ThrottleMinSampleSize = 10
	}
	if out.ThrottleMaxSampleSize <= 0 {
		out.ThrottleMaxSampleSize = 30
	}
	if out.ThrottleWinRateMax <= 0 {
		out.ThrottleWinRateMax = 0.40
	}

	if out.PauseMinSampleSize <= 0 {
		out.PauseMinSampleSize = 30
	}
	if out.PauseWinRateMax <= 0 {
		out.PauseWinRateMax = 0.35
	}
	// PausePnLMax default = 0 (strictly negative cumulative P&L)
	// is intentionally a zero-valued comparison; the meetsPause
	// check uses strict-less-than so a PausePnLMax of zero means
	// "cumulative P&L < 0".

	if out.SleeveOverallMinSampleSize <= 0 {
		out.SleeveOverallMinSampleSize = 5
	}
	if out.SleeveOverallWinRateMax <= 0 {
		out.SleeveOverallWinRateMax = 0.40
	}
	// SleeveOverallPnLMax default = 0 (strictly negative cumulative
	// P&L). The fallback only fires when the sleeve is actually
	// hurting; a sleeve with a 35% win rate but positive P&L means
	// the lottery payoff structure is working and we shouldn't
	// confuse the user with an "underperforming" lesson.

	// Honour legacy MinSampleSize / LossWinRateMax as a floor on
	// the OBSERVING tier (the smallest sample bucket). Callers
	// that explicitly raised these never saw small-sample lessons;
	// we keep that contract.
	if out.MinSampleSize > out.ObservingMinSampleSize {
		out.ObservingMinSampleSize = out.MinSampleSize
	}
	if out.LossWinRateMax > 0 && out.LossWinRateMax < out.ObservingWinRateMax {
		out.ObservingWinRateMax = out.LossWinRateMax
	}
	return out
}

// GenerateLessons turns an attribution report into a slice of
// deterministic Lesson records. Pure function: same report →
// same lessons (in the same order). The Service deduplicates
// against the memory store before persisting, but order matters
// for the dashboard's "top N most-actionable" ranking.
//
// Cross-tab rows whose regime is missing / "unspecified" are
// SKIPPED: a regime label is a categorical claim, and surfacing a
// lesson against a placeholder ("(unspecified)" shows up in the
// title) is worse than silence — it implies the regime detector
// ran when in fact it didn't. The fund-wide BySleeve rollup still
// captures the same trades so nothing is lost; the user just sees
// "sleeve X overall" instead of "sleeve X in (unspecified)".
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
	// Track which sleeves had at least one row with a real
	// (non-unspecified) regime label. If a sleeve was classified
	// AT ALL, the per-regime tiered lessons above are the right
	// view; we don't fall back to the sleeve-overall lesson for
	// those. A sleeve whose every BySleeveRegime row has
	// regime="" / "unspecified" is the one that needs the
	// fallback — the regime detector simply didn't run for it.
	sleeveHasClassifiedRegime := map[string]bool{}
	for _, s := range report.BySleeveRegime {
		if !isUnspecifiedRegime(s.Regime) {
			sleeveHasClassifiedRegime[s.Sleeve] = true
		}
	}
	for _, s := range sortedLosers {
		if isUnspecifiedRegime(s.Regime) {
			continue
		}
		// Pick the HIGHEST-severity tier the row satisfies. Order
		// matters: a 32-trade row that fits BOTH throttle and
		// pause windows should produce one pause lesson, not two.
		switch {
		case meetsPauseThreshold(s, o):
			lessons = append(lessons, buildPauseLesson(s))
		case meetsThrottleThreshold(s, o):
			lessons = append(lessons, buildThrottleLesson(s))
		case meetsObservingThreshold(s, o):
			lessons = append(lessons, buildObservingLesson(s))
		}
	}

	// ---- Sleeve-overall fallback ----
	// Only fires for sleeves whose every BySleeveRegime row was
	// "unspecified" (so the per-regime pass above produced
	// nothing). The user's reported case: OCS-fund, llm_pm sleeve,
	// 6 losing lots, all stamped regime=unspecified. Without this
	// pass the dashboard goes silent; with it, the operator sees
	// "calibrate your regime detector — meanwhile, here's the
	// sleeve-wide picture". We DO NOT emit overall lessons for
	// sleeves that did get classified — those are already covered
	// by the tiered per-regime lessons.
	sortedOverall := append([]repository.SleeveStat(nil), report.BySleeve...)
	sort.SliceStable(sortedOverall, func(i, j int) bool {
		if sortedOverall[i].WinRate != sortedOverall[j].WinRate {
			return sortedOverall[i].WinRate < sortedOverall[j].WinRate
		}
		return sortedOverall[i].TotalPnL < sortedOverall[j].TotalPnL
	})
	for _, s := range sortedOverall {
		if sleeveHasClassifiedRegime[s.Sleeve] {
			continue
		}
		if !meetsSleeveOverallThreshold(s, o) {
			continue
		}
		lessons = append(lessons, buildSleeveOverallLesson(s))
	}

	sortedWinners := append([]repository.SleeveRegimeStat(nil), report.BySleeveRegime...)
	sort.SliceStable(sortedWinners, func(i, j int) bool {
		if sortedWinners[i].WinRate != sortedWinners[j].WinRate {
			return sortedWinners[i].WinRate > sortedWinners[j].WinRate
		}
		return sortedWinners[i].TotalPnL > sortedWinners[j].TotalPnL
	})
	for _, s := range sortedWinners {
		if isUnspecifiedRegime(s.Regime) {
			continue
		}
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

// isUnspecifiedRegime is the canonical placeholder check. We treat
// empty string AND the literal "unspecified" (any case) as missing
// — the regime detector writes one of those two when it can't
// classify the day, and either way it's not a real categorical
// label we should reason about per-regime.
func isUnspecifiedRegime(regime string) bool {
	trimmed := strings.TrimSpace(regime)
	if trimmed == "" {
		return true
	}
	return strings.EqualFold(trimmed, "unspecified")
}

// meetsObservingThreshold fires on the SMALLEST sample bucket:
// 5–9 trades (default), win rate below ObservingWinRateMax (50%
// = below break-even). Severity is info; the lesson reads
// "tracking this combination, sample too small for a directive".
func meetsObservingThreshold(s repository.SleeveRegimeStat, o LessonOptions) bool {
	return s.TradeCount >= o.ObservingMinSampleSize &&
		s.TradeCount < o.ObservingMaxSampleSize &&
		s.WinRate < o.ObservingWinRateMax
}

// meetsThrottleThreshold fires on the MEDIUM sample bucket:
// 10–29 trades (default), win rate below ThrottleWinRateMax
// (40%). A real PM at this sample size would reduce position
// size or raise the confidence floor — not pause outright.
func meetsThrottleThreshold(s repository.SleeveRegimeStat, o LessonOptions) bool {
	return s.TradeCount >= o.ThrottleMinSampleSize &&
		s.TradeCount < o.ThrottleMaxSampleSize &&
		s.WinRate < o.ThrottleWinRateMax &&
		s.TotalPnL < 0
}

// meetsPauseThreshold fires on the LARGE sample bucket: 30+
// trades, win rate below PauseWinRateMax (35%), AND cumulative
// P&L strictly negative. Only at this sample is the win-rate
// shortfall statistically meaningful enough to recommend
// pausing — and only when the dollar damage is real, not a
// "wins rarely but big" lottery distribution.
func meetsPauseThreshold(s repository.SleeveRegimeStat, o LessonOptions) bool {
	return s.TradeCount >= o.PauseMinSampleSize &&
		s.WinRate < o.PauseWinRateMax &&
		s.TotalPnL < o.PausePnLMax
}

func meetsWinThreshold(s repository.SleeveRegimeStat, o LessonOptions) bool {
	return s.TradeCount >= o.WinnerMinSampleSize &&
		s.WinRate > o.WinnerWinRateMin &&
		s.TotalPnL > 0
}

// meetsSleeveOverallThreshold fires the regime-detector fallback
// lesson on a single sleeve when the per-regime view above
// produced nothing for that sleeve (because every row was
// unspecified). Win-rate ceiling is intentionally tighter than
// the observing tier (40% vs 50%) — we only emit the fallback
// when the sleeve is clearly hurting, not just running slightly
// sub-par; otherwise the operator gets noise instead of signal.
func meetsSleeveOverallThreshold(s repository.SleeveStat, o LessonOptions) bool {
	return s.TradeCount >= o.SleeveOverallMinSampleSize &&
		s.WinRate < o.SleeveOverallWinRateMax &&
		s.TotalPnL < o.SleeveOverallPnLMax
}

// Template-key constants. All keys live in one block so a future
// "rename + bump version" patch is one diff. The shape is enforced
// by the migration 085 CHECK constraint:
//
//	^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+(\.v[0-9]+)?$
//
// Add a `.v2` suffix the first time the payload's field set changes
// in a non-additive way (rename, retype, remove). Keep the unversioned
// key for the original schema; the frontend dictionary keeps both
// until the older memories age out of the 30-day replay window.
//
// Each constant is paired with a payload-shape comment so the
// frontend (lessonRenderer.tsx) can be written against this contract
// without round-tripping through me.
const (
	// templateLoser: LEGACY single-tier loser. New runs no longer
	// emit this — the frontend dictionary keeps the key alive so
	// memory rows persisted before the tiering refactor still
	// render. See attribution.go::LessonSleeveRegimeLoser.
	templateLoser = "attribution.lesson.sleeve_regime_loser"
	// templateObserving: attribution.lesson.sleeve_regime_observing
	// payload: { sleeve, regime, trade_count, win_rate, total_pnl,
	//            avg_pnl_pct, avg_holding_days }
	// Small-sample journal entry; advisory body suggests
	// continuing to watch and capturing context, NOT changing
	// portfolio weights.
	templateObserving = "attribution.lesson.sleeve_regime_observing"
	// templateThrottle: attribution.lesson.sleeve_regime_throttle
	// payload: same shape as observing.
	// Medium-sample risk-management lesson; body suggests
	// reducing sizing / raising the confidence floor / trying a
	// shorter-horizon variant of the sleeve.
	templateThrottle = "attribution.lesson.sleeve_regime_throttle"
	// templatePause: attribution.lesson.sleeve_regime_pause
	// payload: same shape as observing.
	// Large-sample decisive lesson; body recommends pausing the
	// (sleeve, regime) pair until the regime changes — this is
	// the only tier that recommends a portfolio change.
	templatePause = "attribution.lesson.sleeve_regime_pause"
	// templateWinner: attribution.lesson.sleeve_regime_winner
	// payload: { sleeve, regime, trade_count, win_rate, total_pnl,
	//            avg_pnl_pct, avg_holding_days }
	templateWinner = "attribution.lesson.sleeve_regime_winner"
	// templateSleeveOverall: attribution.lesson.sleeve_overall
	// payload: { sleeve, trade_count, win_rate, total_pnl,
	//            avg_pnl_pct, median_hold_days }
	//
	// Differs from the regime tiers in TWO ways:
	//   1) NO `regime` field (this lesson exists precisely because
	//      regime classification failed).
	//   2) `median_hold_days` instead of `avg_holding_days` —
	//      SleeveStat only carries the median (the cross-tab carries
	//      the average). The semantic difference is small for a body
	//      that already says "calibrate your detector first".
	//
	// Severity=warning. Body explicitly tells the operator the
	// regime detector didn't classify any of this sleeve's lots and
	// that the rollup is therefore the only available view.
	templateSleeveOverall = "attribution.lesson.sleeve_overall"
	// templateInsufficientWatching:
	//   attribution.lesson.insufficient_data.watching
	// payload: { open_lot_count, earliest_opened_at, window_days }
	// Fires when there are open lots AND we know the earliest open
	// date — gives the user a concrete "we are watching X since Y".
	templateInsufficientWatching = "attribution.lesson.insufficient_data.watching"
	// templateInsufficientWatchingNoDate:
	//   attribution.lesson.insufficient_data.watching_no_date
	// payload: { open_lot_count, window_days }
	// Same situation but the earliest date is unknown (legacy data).
	templateInsufficientWatchingNoDate = "attribution.lesson.insufficient_data.watching_no_date"
	// templateInsufficientEmpty:
	//   attribution.lesson.insufficient_data.empty
	// payload: { window_days }
	// No closed AND no open lots — brand-new fund, no observation yet.
	templateInsufficientEmpty = "attribution.lesson.insufficient_data.empty"
)

// loserPayload + winnerPayload share the same field set; we keep
// two helpers so a future divergence (e.g. only winners get a
// "scale_suggestion" hint) doesn't have to thread a flag through.
func sleeveRegimePayload(s repository.SleeveRegimeStat) map[string]any {
	return map[string]any{
		"sleeve":           s.Sleeve,
		"regime":           s.Regime,
		"trade_count":      s.TradeCount,
		"win_rate":         s.WinRate, // raw 0..1 ratio; UI multiplies by 100
		"total_pnl":        s.TotalPnL,
		"avg_pnl_pct":      s.AvgPnLPct,
		"avg_holding_days": s.AvgHoldingDays,
	}
}

// buildObservingLesson — 5-to-(observingMax-1) closed lots, win
// rate below 50% (i.e. below break-even). Severity=info so the UI
// renders it as a journal entry, not an alert. Title and body
// stay deliberately understated ("watching", not "losing money")
// because the sample is too small for any directive.
//
// The English Title/Body are fallback text — the UI prefers the
// i18n template (zh-CN / en-US) keyed off TemplateKey.
func buildObservingLesson(s repository.SleeveRegimeStat) Lesson {
	return Lesson{
		Kind:     LessonSleeveRegimeObserving,
		Severity: SeverityInfo,
		Title: fmt.Sprintf(
			"Watching sleeve %q under regime %q — %d trades, win-rate %.0f%% (small sample)",
			s.Sleeve, s.Regime, s.TradeCount, s.WinRate*100,
		),
		Body: fmt.Sprintf(
			"Across %d closed lots in regime %s, the %s sleeve is currently at a %.0f%% win rate "+
				"(realised P&L %.2f, avg pnl pct %.3f, avg holding %.1f days). The sample is too "+
				"small to recommend a portfolio change — keep tracking entries / exits, and revisit "+
				"once the sample grows past %d trades or the regime shifts.",
			s.TradeCount, s.Regime, s.Sleeve, s.WinRate*100, s.TotalPnL, s.AvgPnLPct, s.AvgHoldingDays, 10,
		),
		Tags:           []string{"observing", "sleeve:" + s.Sleeve, "regime:" + s.Regime},
		Sleeve:         s.Sleeve,
		Regime:         s.Regime,
		TradeCount:     s.TradeCount,
		WinRate:        s.WinRate,
		TotalPnL:       s.TotalPnL,
		AvgPnLPct:      s.AvgPnLPct,
		AvgHoldingDays: s.AvgHoldingDays,
		TemplateKey:    templateObserving,
		Payload:        sleeveRegimePayload(s),
	}
}

// buildThrottleLesson — 10-to-29 closed lots, win-rate below 40%,
// P&L negative. Severity=warning so the UI shows it amber. Body
// suggests concrete actions a real PM would take (reduce size,
// raise confidence threshold, try a shorter horizon) instead of
// the binary "pause" wording the legacy loser lesson used.
func buildThrottleLesson(s repository.SleeveRegimeStat) Lesson {
	return Lesson{
		Kind:     LessonSleeveRegimeThrottle,
		Severity: SeverityWarning,
		Title: fmt.Sprintf(
			"Sleeve %q is underperforming in regime %q (%d trades, win-rate %.0f%%, PnL %.2f) — reduce sizing",
			s.Sleeve, s.Regime, s.TradeCount, s.WinRate*100, s.TotalPnL,
		),
		Body: fmt.Sprintf(
			"Across %d closed lots in regime %s, the %s sleeve recorded a %.0f%% win rate and a "+
				"cumulative realised P&L of %.2f (avg pnl pct: %.3f, avg holding %.1f days). The "+
				"sample now supports a risk-management response: consider (a) cutting position size "+
				"on this (sleeve, regime) pair by ~30%%, (b) raising the entry confidence threshold "+
				"so only higher-conviction signals fire, or (c) trying a shorter-horizon variant "+
				"(e.g. intraday) before deciding whether to pause the combination at the 30-trade mark.",
			s.TradeCount, s.Regime, s.Sleeve, s.WinRate*100, s.TotalPnL, s.AvgPnLPct, s.AvgHoldingDays,
		),
		Tags:           []string{"throttle", "sleeve:" + s.Sleeve, "regime:" + s.Regime},
		Sleeve:         s.Sleeve,
		Regime:         s.Regime,
		TradeCount:     s.TradeCount,
		WinRate:        s.WinRate,
		TotalPnL:       s.TotalPnL,
		AvgPnLPct:      s.AvgPnLPct,
		AvgHoldingDays: s.AvgHoldingDays,
		TemplateKey:    templateThrottle,
		Payload:        sleeveRegimePayload(s),
	}
}

// buildPauseLesson — 30+ closed lots, win-rate below 35%, P&L
// strictly negative. Severity=critical. This is the only tier
// that recommends actually pausing the combination — by that
// sample size the win-rate shortfall is statistically
// meaningful AND the dollar damage is real.
func buildPauseLesson(s repository.SleeveRegimeStat) Lesson {
	return Lesson{
		Kind:     LessonSleeveRegimePause,
		Severity: SeverityCritical,
		Title: fmt.Sprintf(
			"Sleeve %q is decisively losing in regime %q (%d trades, win-rate %.0f%%, PnL %.2f) — pause this pair",
			s.Sleeve, s.Regime, s.TradeCount, s.WinRate*100, s.TotalPnL,
		),
		Body: fmt.Sprintf(
			"Across %d closed lots in regime %s, the %s sleeve recorded a %.0f%% win rate and a "+
				"cumulative realised P&L of %.2f (avg pnl pct: %.3f, avg holding %.1f days). At "+
				"this sample the underperformance is statistically meaningful AND has cost the fund "+
				"real money: pause the (sleeve, regime) combination in the fund's strategy sleeves "+
				"config until the regime changes, and capture a post-mortem of why the entry filter "+
				"misfired so the next iteration of the sleeve avoids the same setup.",
			s.TradeCount, s.Regime, s.Sleeve, s.WinRate*100, s.TotalPnL, s.AvgPnLPct, s.AvgHoldingDays,
		),
		Tags:           []string{"pause", "sleeve:" + s.Sleeve, "regime:" + s.Regime},
		Sleeve:         s.Sleeve,
		Regime:         s.Regime,
		TradeCount:     s.TradeCount,
		WinRate:        s.WinRate,
		TotalPnL:       s.TotalPnL,
		AvgPnLPct:      s.AvgPnLPct,
		AvgHoldingDays: s.AvgHoldingDays,
		TemplateKey:    templatePause,
		Payload:        sleeveRegimePayload(s),
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
			Tags:        []string{"insufficient_data", "observing"},
			TemplateKey: templateInsufficientWatching,
			Payload: map[string]any{
				"open_lot_count": report.OpenLotCount,
				// ISO-8601 UTC midnight; UI re-formats to its locale.
				"earliest_opened_at": earliest,
				"window_days":        report.Window.Days,
			},
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
			Tags:        []string{"insufficient_data", "observing"},
			TemplateKey: templateInsufficientWatchingNoDate,
			Payload: map[string]any{
				"open_lot_count": report.OpenLotCount,
				"window_days":    report.Window.Days,
			},
		}
	default:
		return Lesson{
			Kind:        LessonInsufficientData,
			Severity:    SeverityInfo,
			Title:       fmt.Sprintf("No closed trades in the last %d days", report.Window.Days),
			Body:        "Attribution will populate once the fund has produced its first realized P&L.",
			Tags:        []string{"insufficient_data"},
			TemplateKey: templateInsufficientEmpty,
			Payload: map[string]any{
				"window_days": report.Window.Days,
			},
		}
	}
}

func pluralLot(n int) string {
	if n == 1 {
		return "lot"
	}
	return "lots"
}

// sleeveOverallPayload is the i18n contract for templateSleeveOverall.
// Keep it disjoint from sleeveRegimePayload — the regime detector
// failed, so there's no regime to surface; we use MedianHoldDays
// from SleeveStat since AvgHoldingDays isn't carried at the sleeve
// rollup level.
func sleeveOverallPayload(s repository.SleeveStat) map[string]any {
	return map[string]any{
		"sleeve":           s.Sleeve,
		"trade_count":      s.TradeCount,
		"win_rate":         s.WinRate,
		"total_pnl":        s.TotalPnL,
		"avg_pnl_pct":      s.AvgPnLPct,
		"median_hold_days": s.MedianHoldDays,
	}
}

// buildSleeveOverallLesson — fund-wide rollup for a single sleeve.
// Fires ONLY when GenerateLessons determined no per-regime row for
// this sleeve was actionable (every regime was unspecified, or
// every classified row sat below sample threshold). The body
// explicitly names "regime detector returned unspecified" as the
// reason so the operator's first action is to fix the detector,
// not the strategy. Severity=warning — not critical, because
// without a regime breakdown we can't confidently recommend a
// portfolio change, just a "look here" flag.
func buildSleeveOverallLesson(s repository.SleeveStat) Lesson {
	return Lesson{
		Kind:     LessonSleeveOverall,
		Severity: SeverityWarning,
		Title: fmt.Sprintf(
			"Sleeve %q is underperforming overall — regime detector returned unspecified (%d trades, win-rate %.0f%%, PnL %.2f)",
			s.Sleeve, s.TradeCount, s.WinRate*100, s.TotalPnL,
		),
		Body: fmt.Sprintf(
			"Across %d closed lots, the %s sleeve recorded a %.0f%% win rate and a cumulative "+
				"realised P&L of %.2f (avg pnl pct: %.3f, median holding %.1f days). The regime "+
				"detector did not classify any of these lots (regime=\"unspecified\"), so a per-"+
				"regime view is unavailable. First action: calibrate the regime detector (check "+
				"feature inputs, lookback window, and threshold config) so future runs can "+
				"distinguish trending vs choppy days. Until then, treat the sleeve-wide loss as "+
				"a flag to investigate, not a directive to pause — the regime breakdown may "+
				"reveal this is a single-regime problem rather than a sleeve-wide one.",
			s.TradeCount, s.Sleeve, s.WinRate*100, s.TotalPnL, s.AvgPnLPct, s.MedianHoldDays,
		),
		Tags: []string{"overall", "regime_detector_unavailable", "sleeve:" + s.Sleeve},

		Sleeve:         s.Sleeve,
		TradeCount:     s.TradeCount,
		WinRate:        s.WinRate,
		TotalPnL:       s.TotalPnL,
		AvgPnLPct:      s.AvgPnLPct,
		AvgHoldingDays: s.MedianHoldDays,
		TemplateKey:    templateSleeveOverall,
		Payload:        sleeveOverallPayload(s),
	}
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
		TemplateKey:    templateWinner,
		Payload:        sleeveRegimePayload(s),
	}
}
