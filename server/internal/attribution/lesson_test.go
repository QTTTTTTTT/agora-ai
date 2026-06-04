package attribution

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/repository"
)

// ---------------------------------------------------------------------------
// Threshold gate
// ---------------------------------------------------------------------------

// TestGenerateLessonsEmitsObservingOnSmallSample verifies the 5–9
// trade tier produces a journal-style "observing" lesson instead of
// the old "you're losing money, pause this" critical alert. This is
// the exact size of the OCS-fund llm_pm × (unspecified) regime
// example that prompted the tiering refactor — six trades, 33% win,
// negative P&L — and a professional team would NOT recommend a
// portfolio change off that sample.
func TestGenerateLessonsEmitsObservingOnSmallSample(t *testing.T) {
	report := AttributionReport{
		Window: Window{Days: 30},
		BySleeveRegime: []repository.SleeveRegimeStat{
			{Sleeve: "llm_pm", Regime: "chop", TradeCount: 6, WinCount: 2, LossCount: 4, TotalPnL: -120, AvgPnLPct: -0.01, AvgHoldingDays: 3.6, WinRate: 1.0 / 3.0},
		},
	}
	lessons := GenerateLessons(report, LessonOptions{})
	if len(lessons) != 1 {
		t.Fatalf("expected 1 observing lesson, got %d: %+v", len(lessons), lessons)
	}
	l := lessons[0]
	if l.Kind != LessonSleeveRegimeObserving {
		t.Fatalf("kind: got %q, want %q", l.Kind, LessonSleeveRegimeObserving)
	}
	if l.Severity != SeverityInfo {
		t.Fatalf("severity: got %q, want info (small sample → journal entry, not alert)", l.Severity)
	}
	if !containsTag(l.Tags, "observing") {
		t.Fatalf("expected 'observing' tag in %v", l.Tags)
	}
}

// TestGenerateLessonsEmitsThrottleOnMidSample verifies the 10–29
// trade tier produces a warning lesson recommending size reduction,
// NOT a pause directive. A real PM at this sample size would cut
// exposure or raise the confidence floor before pulling the plug.
func TestGenerateLessonsEmitsThrottleOnMidSample(t *testing.T) {
	report := AttributionReport{
		Window: Window{Days: 30},
		BySleeveRegime: []repository.SleeveRegimeStat{
			{Sleeve: "mean_reversion", Regime: "chop", TradeCount: 12, WinCount: 3, LossCount: 9, TotalPnL: -480, AvgPnLPct: -0.04, AvgHoldingDays: 2.1, WinRate: 0.25},
		},
	}
	lessons := GenerateLessons(report, LessonOptions{})
	if len(lessons) != 1 {
		t.Fatalf("expected 1 throttle lesson, got %d: %+v", len(lessons), lessons)
	}
	l := lessons[0]
	if l.Kind != LessonSleeveRegimeThrottle {
		t.Fatalf("kind: got %q, want %q", l.Kind, LessonSleeveRegimeThrottle)
	}
	if l.Severity != SeverityWarning {
		t.Fatalf("severity: got %q, want warning", l.Severity)
	}
	if !strings.Contains(l.Title, "mean_reversion") || !strings.Contains(l.Title, "chop") {
		t.Fatalf("title missing context: %q", l.Title)
	}
	wantTags := map[string]bool{"throttle": false, "sleeve:mean_reversion": false, "regime:chop": false}
	for _, tag := range l.Tags {
		if _, ok := wantTags[tag]; ok {
			wantTags[tag] = true
		}
	}
	for tag, seen := range wantTags {
		if !seen {
			t.Fatalf("missing tag %q in %+v", tag, l.Tags)
		}
	}
}

// TestGenerateLessonsEmitsPauseOnLargeSample verifies that the
// large-sample (30+ trades) decisive case still produces a
// critical-severity lesson — this is the only tier that recommends
// pausing the combination, and statistically it has earned the
// right to.
func TestGenerateLessonsEmitsPauseOnLargeSample(t *testing.T) {
	report := AttributionReport{
		Window: Window{Days: 30},
		BySleeveRegime: []repository.SleeveRegimeStat{
			{Sleeve: "trend", Regime: "chop", TradeCount: 35, WinCount: 10, LossCount: 25, TotalPnL: -1800, AvgPnLPct: -0.06, AvgHoldingDays: 4.0, WinRate: 10.0 / 35.0},
		},
	}
	lessons := GenerateLessons(report, LessonOptions{})
	if len(lessons) != 1 {
		t.Fatalf("expected 1 pause lesson, got %d: %+v", len(lessons), lessons)
	}
	l := lessons[0]
	if l.Kind != LessonSleeveRegimePause {
		t.Fatalf("kind: got %q, want %q", l.Kind, LessonSleeveRegimePause)
	}
	if l.Severity != SeverityCritical {
		t.Fatalf("severity: got %q, want critical (large sample, decisive)", l.Severity)
	}
	if !containsTag(l.Tags, "pause") {
		t.Fatalf("expected 'pause' tag in %v", l.Tags)
	}
}

// TestGenerateLessonsSkipsUnspecifiedRegime is the regression test
// for the user-reported case "Sleeve llm_pm is losing money in
// regime (unspecified)" — a regime label of "" or "unspecified"
// means the regime detector didn't classify the day. Surfacing a
// per-regime lesson against a placeholder label implies the
// detector ran when it didn't, which is worse than silence. The
// fund-wide BySleeve rollup still captures the same trades.
func TestGenerateLessonsSkipsUnspecifiedRegime(t *testing.T) {
	cases := []string{"", "unspecified", "UNSPECIFIED", "  Unspecified  "}
	for _, regime := range cases {
		t.Run("regime="+regime, func(t *testing.T) {
			report := AttributionReport{
				Window: Window{Days: 30},
				BySleeveRegime: []repository.SleeveRegimeStat{
					{Sleeve: "llm_pm", Regime: regime, TradeCount: 35, WinCount: 10, LossCount: 25, TotalPnL: -1800, WinRate: 10.0 / 35.0},
				},
			}
			if got := GenerateLessons(report, LessonOptions{}); len(got) != 0 {
				t.Fatalf("expected no lesson when regime is unspecified (%q), got %+v", regime, got)
			}
		})
	}
}

func containsTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

func TestGenerateLessonsEmitsWinnerOnDecisiveCell(t *testing.T) {
	report := AttributionReport{
		Window: Window{Days: 30},
		BySleeveRegime: []repository.SleeveRegimeStat{
			{Sleeve: "trend", Regime: "trend_up", TradeCount: 15, WinCount: 11, LossCount: 4, TotalPnL: 1240, AvgPnLPct: 0.08, AvgHoldingDays: 7, WinRate: 11.0 / 15.0},
		},
	}
	lessons := GenerateLessons(report, LessonOptions{})
	if len(lessons) != 1 {
		t.Fatalf("expected 1 winner lesson, got %d", len(lessons))
	}
	if lessons[0].Kind != LessonSleeveRegimeWinner || lessons[0].Severity != SeverityInfo {
		t.Fatalf("unexpected kind/severity: %+v", lessons[0])
	}
}

// ---------------------------------------------------------------------------
// Threshold guards
// ---------------------------------------------------------------------------

func TestGenerateLessonsSkipsBelowSampleSize(t *testing.T) {
	report := AttributionReport{
		Window: Window{Days: 30},
		BySleeveRegime: []repository.SleeveRegimeStat{
			{Sleeve: "trend", Regime: "trend_down", TradeCount: 3, WinCount: 0, LossCount: 3, TotalPnL: -100, WinRate: 0},
		},
	}
	if got := GenerateLessons(report, LessonOptions{}); len(got) != 0 {
		t.Fatalf("expected 0 lessons (small sample), got %d: %+v", len(got), got)
	}
}

// TestGenerateLessonsDoesNotFlagLowWinRateProfitableSleeve guards
// against the classic "1 in 5 trades wins big" lottery-payoff
// false positive. Win-rate alone would flag it as a loser; the
// triple-condition gate spares it because PnL is positive.
func TestGenerateLessonsDoesNotFlagLowWinRateProfitableSleeve(t *testing.T) {
	report := AttributionReport{
		Window: Window{Days: 30},
		BySleeveRegime: []repository.SleeveRegimeStat{
			{Sleeve: "trend", Regime: "trend_up", TradeCount: 10, WinCount: 2, LossCount: 8, TotalPnL: 500, WinRate: 0.2, AvgPnLPct: 0.05},
		},
	}
	if got := GenerateLessons(report, LessonOptions{}); len(got) != 0 {
		t.Fatalf("expected 0 lessons on profitable-but-low-winrate cell, got %+v", got)
	}
}

// TestGenerateLessonsDoesNotFlagHighWinRateLosingSleeve mirror
// case: high win-rate but tiny losses each win and huge losses on
// the few defeats → TotalPnL negative. Don't tag as winner.
func TestGenerateLessonsDoesNotFlagHighWinRateLosingSleeve(t *testing.T) {
	report := AttributionReport{
		Window: Window{Days: 30},
		BySleeveRegime: []repository.SleeveRegimeStat{
			{Sleeve: "trend", Regime: "trend_up", TradeCount: 10, WinCount: 8, LossCount: 2, TotalPnL: -100, WinRate: 0.8, AvgPnLPct: -0.01},
		},
	}
	if got := GenerateLessons(report, LessonOptions{}); len(got) != 0 {
		t.Fatalf("expected 0 winner lessons (PnL negative), got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Edge cases
// ---------------------------------------------------------------------------

func TestGenerateLessonsInsufficientDataReturnsInfoLesson(t *testing.T) {
	report := AttributionReport{
		FundID:      "fund-x",
		Window:      Window{Days: 30, Since: time.Now().Add(-30 * 24 * time.Hour)},
		GeneratedAt: time.Now(),
	}
	lessons := GenerateLessons(report, LessonOptions{})
	if len(lessons) != 1 {
		t.Fatalf("expected 1 info lesson, got %d", len(lessons))
	}
	if lessons[0].Kind != LessonInsufficientData || lessons[0].Severity != SeverityInfo {
		t.Fatalf("unexpected: %+v", lessons[0])
	}
}

func TestGenerateLessonsInsufficientDataMentionsOpenLotInventory(t *testing.T) {
	earliest := time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC)
	report := AttributionReport{
		FundID:           "fund-x",
		Window:           Window{Days: 30, Since: time.Now().Add(-30 * 24 * time.Hour)},
		GeneratedAt:      time.Now(),
		OpenLotCount:     7,
		EarliestOpenedAt: sql.NullTime{Time: earliest, Valid: true},
	}
	lessons := GenerateLessons(report, LessonOptions{})
	if len(lessons) != 1 {
		t.Fatalf("expected 1 info lesson, got %d", len(lessons))
	}
	l := lessons[0]
	if l.Kind != LessonInsufficientData {
		t.Fatalf("expected insufficient_data kind, got %q", l.Kind)
	}
	if !strings.Contains(l.Title, "7 open lots") {
		t.Fatalf("title should mention 7 open lots, got %q", l.Title)
	}
	if !strings.Contains(l.Title, "2026-05-12") {
		t.Fatalf("title should mention earliest date, got %q", l.Title)
	}
	if !strings.Contains(l.Body, "7 still-open lots") {
		t.Fatalf("body should mention open lot count, got %q", l.Body)
	}
	hasObservingTag := false
	for _, tag := range l.Tags {
		if tag == "observing" {
			hasObservingTag = true
			break
		}
	}
	if !hasObservingTag {
		t.Fatalf("expected 'observing' tag in %v", l.Tags)
	}
}

func TestGenerateLessonsInsufficientDataSingularLotWording(t *testing.T) {
	earliest := time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC)
	report := AttributionReport{
		Window:           Window{Days: 30},
		OpenLotCount:     1,
		EarliestOpenedAt: sql.NullTime{Time: earliest, Valid: true},
	}
	l := GenerateLessons(report, LessonOptions{})[0]
	if !strings.Contains(l.Title, "1 open lot ") {
		t.Fatalf("singular wording missing in title: %q", l.Title)
	}
}

func TestGenerateLessonsInsufficientDataNoOpenLotsKeepsLegacyWording(t *testing.T) {
	report := AttributionReport{
		Window: Window{Days: 30},
	}
	l := GenerateLessons(report, LessonOptions{})[0]
	if l.Title != "No closed trades in the last 30 days" {
		t.Fatalf("expected legacy title when inventory is empty, got %q", l.Title)
	}
	for _, tag := range l.Tags {
		if tag == "observing" {
			t.Fatalf("'observing' tag should not appear when no open lots; tags=%v", l.Tags)
		}
	}
}

func TestGenerateLessonsRespectsMaxLessons(t *testing.T) {
	rows := []repository.SleeveRegimeStat{}
	for i := 0; i < 50; i++ {
		rows = append(rows, repository.SleeveRegimeStat{
			Sleeve: "trend", Regime: "range",
			TradeCount: 10, WinCount: 2, LossCount: 8, TotalPnL: -100 - float64(i), WinRate: 0.2,
		})
	}
	report := AttributionReport{Window: Window{Days: 30}, BySleeveRegime: rows}
	lessons := GenerateLessons(report, LessonOptions{MaxLessons: 3})
	if len(lessons) != 3 {
		t.Fatalf("expected 3 lessons (cap), got %d", len(lessons))
	}
}

func TestGenerateLessonsOrdersLosersWorstFirst(t *testing.T) {
	report := AttributionReport{
		Window: Window{Days: 30},
		BySleeveRegime: []repository.SleeveRegimeStat{
			{Sleeve: "trend", Regime: "range", TradeCount: 10, WinCount: 3, LossCount: 7, TotalPnL: -100, WinRate: 0.3},
			{Sleeve: "trend", Regime: "chop", TradeCount: 10, WinCount: 1, LossCount: 9, TotalPnL: -300, WinRate: 0.1},
		},
	}
	lessons := GenerateLessons(report, LessonOptions{})
	if len(lessons) < 2 {
		t.Fatalf("expected 2 lessons, got %d", len(lessons))
	}
	if lessons[0].Regime != "chop" {
		t.Fatalf("worst loser (chop) should be first, got %q", lessons[0].Regime)
	}
}
