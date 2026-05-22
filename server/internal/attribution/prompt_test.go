package attribution

import (
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/repository"
)

func mkStat(sleeve, regime string, n int, wr, pnl, avgPct float64) repository.SleeveRegimeStat {
	return repository.SleeveRegimeStat{
		Sleeve:     sleeve,
		Regime:     regime,
		TradeCount: n,
		WinRate:    wr,
		TotalPnL:   pnl,
		AvgPnLPct:  avgPct,
	}
}

func mkReport(rows ...repository.SleeveRegimeStat) AttributionReport {
	return AttributionReport{
		Window:         Window{Days: 30, Since: time.Now()},
		BySleeveRegime: rows,
	}
}

func TestBuildPromptScorecardEmptyWhenReportHasNoData(t *testing.T) {
	r := AttributionReport{Window: Window{Days: 30}}
	got := BuildPromptScorecard(r, PromptScorecardOptions{})
	if got.Summary != "" || len(got.Rows) != 0 {
		t.Fatalf("expected zero value on empty report, got %+v", got)
	}
}

func TestBuildPromptScorecardFiltersBelowMinSample(t *testing.T) {
	r := mkReport(
		mkStat("trend", "trend_up", 2, 1.0, 500, 0.05), // n < 5: dropped
		mkStat("mean_reversion", "range", 7, 0.71, 350, 0.02),
	)
	got := BuildPromptScorecard(r, PromptScorecardOptions{})
	if len(got.Rows) != 1 {
		t.Fatalf("expected 1 row after min-sample filter, got %d: %+v", len(got.Rows), got.Rows)
	}
	if got.Rows[0].Sleeve != "mean_reversion" {
		t.Fatalf("expected mean_reversion survived, got %+v", got.Rows[0])
	}
}

func TestBuildPromptScorecardCapsTopAndBottom(t *testing.T) {
	r := mkReport(
		mkStat("trend", "trend_up", 10, 0.7, 1000, 0.05),
		mkStat("trend", "trend_down", 10, 0.6, 800, 0.04),
		mkStat("mean_reversion", "range", 10, 0.65, 600, 0.03),
		mkStat("trend", "range", 10, 0.5, 200, 0.01),         // breakeven-ish positive: 4th winner — dropped by TopN=3
		mkStat("trend", "chop", 10, 0.3, -500, -0.03),        // loser
		mkStat("mean_reversion", "trend_up", 10, 0.3, -800, -0.04),
		mkStat("mean_reversion", "chop", 10, 0.2, -1200, -0.06),
		mkStat("trend", "trend_strong_down", 10, 0.2, -1500, -0.07), // worst loser
	)
	got := BuildPromptScorecard(r, PromptScorecardOptions{TopN: 3, BottomN: 3})
	if len(got.Rows) != 6 {
		t.Fatalf("expected TopN(3) + BottomN(3) = 6 rows, got %d: %+v", len(got.Rows), got.Rows)
	}
	// Winners must be the top 3 by PnL.
	if got.Rows[0].TotalPnL != 1000 || got.Rows[1].TotalPnL != 800 || got.Rows[2].TotalPnL != 600 {
		t.Fatalf("top 3 winners not preserved by PnL ordering: %+v", got.Rows[:3])
	}
	// Losers must include the worst 3.
	if got.Rows[5].TotalPnL != -1500 {
		t.Fatalf("worst loser must land in scorecard, got %+v", got.Rows[5])
	}
}

func TestBuildPromptScorecardSummaryMentionsKeyFields(t *testing.T) {
	r := mkReport(
		mkStat("trend", "trend_up", 10, 0.7, 1000, 0.05),
		mkStat("mean_reversion", "chop", 10, 0.25, -800, -0.05),
	)
	got := BuildPromptScorecard(r, PromptScorecardOptions{})
	if got.Summary == "" {
		t.Fatal("expected non-empty summary")
	}
	want := []string{
		"Strategy scorecard",
		"last 30 days",
		"Winners",
		"Losers",
		"sleeve=trend",
		"regime=trend_up",
		"sleeve=mean_reversion",
		"regime=chop",
		"$+1000.00",
		"$-800.00",
	}
	for _, s := range want {
		if !strings.Contains(got.Summary, s) {
			t.Fatalf("summary missing %q; full summary:\n%s", s, got.Summary)
		}
	}
}

func TestBuildPromptScorecardOmitsRowsWithBlankLabels(t *testing.T) {
	// A row produced by a misconfigured sleeve / unknown regime
	// shouldn't be forwarded — the LLM would see garbage labels
	// like "sleeve=  regime=  " and could anchor on them.
	r := mkReport(
		mkStat("", "trend_up", 10, 0.7, 1000, 0.05),
		mkStat("trend", "", 10, 0.7, 1000, 0.05),
		mkStat("trend", "trend_up", 10, 0.7, 1000, 0.05),
	)
	got := BuildPromptScorecard(r, PromptScorecardOptions{})
	if len(got.Rows) != 1 {
		t.Fatalf("expected blank-labelled rows dropped, got %d rows: %+v", len(got.Rows), got.Rows)
	}
}

func TestBuildPromptScorecardDeterministicOrdering(t *testing.T) {
	// Two rows with identical PnL must be ordered by (sleeve,
	// regime) so the LLM's view doesn't shuffle between runs —
	// otherwise output JSON cache-keying breaks.
	r := mkReport(
		mkStat("mean_reversion", "range", 10, 0.7, 500, 0.03),
		mkStat("trend", "trend_up", 10, 0.7, 500, 0.03),
	)
	got1 := BuildPromptScorecard(r, PromptScorecardOptions{})
	got2 := BuildPromptScorecard(r, PromptScorecardOptions{})
	if got1.Summary != got2.Summary {
		t.Fatal("expected deterministic Summary across repeated calls")
	}
	if got1.Rows[0].Sleeve != "mean_reversion" {
		t.Fatalf("tie-break by sleeve alpha-asc; got first sleeve %q", got1.Rows[0].Sleeve)
	}
}

func TestBuildPromptScorecardOnlyLosersStillProduces(t *testing.T) {
	r := mkReport(
		mkStat("trend", "chop", 7, 0.2, -400, -0.03),
		mkStat("mean_reversion", "trend_up", 6, 0.15, -800, -0.05),
	)
	got := BuildPromptScorecard(r, PromptScorecardOptions{})
	if len(got.Rows) != 2 {
		t.Fatalf("expected 2 loser rows, got %d", len(got.Rows))
	}
	if !strings.Contains(got.Summary, "Losers") {
		t.Fatalf("summary should include Losers section, got:\n%s", got.Summary)
	}
	if strings.Contains(got.Summary, "Winners") {
		t.Fatalf("summary should omit Winners section when none exist, got:\n%s", got.Summary)
	}
}
