package attribution

import (
	"testing"

	"github.com/fundai/server/internal/repository"
)

func TestNormalizeKeyFoldsNullsAndCase(t *testing.T) {
	in := SleeveRegimeKey{Sleeve: "  TREND  ", Regime: ""}
	got := NormalizeKey(in)
	if got.Sleeve != "trend" {
		t.Fatalf("sleeve: got %q, want trend", got.Sleeve)
	}
	if got.Regime != "(unspecified)" {
		t.Fatalf("regime: got %q, want (unspecified)", got.Regime)
	}
}

func TestIndexSleeveRegimeMergesDuplicates(t *testing.T) {
	stats := []repository.SleeveRegimeStat{
		{Sleeve: "Trend", Regime: "Trend_Up", TradeCount: 10, WinCount: 7, LossCount: 3, TotalPnL: 100, AvgPnLPct: 0.05, AvgHoldingDays: 3},
		{Sleeve: "trend", Regime: "trend_up", TradeCount: 5, WinCount: 2, LossCount: 3, TotalPnL: -20, AvgPnLPct: -0.01, AvgHoldingDays: 2},
	}
	idx := IndexSleeveRegime(stats)
	if len(idx) != 1 {
		t.Fatalf("expected merge to one cell, got %d: %+v", len(idx), idx)
	}
	got := idx[SleeveRegimeKey{Sleeve: "trend", Regime: "trend_up"}]
	if got.TradeCount != 15 || got.WinCount != 9 || got.LossCount != 6 {
		t.Fatalf("counts: got %+v", got)
	}
	if got.TotalPnL != 80 {
		t.Fatalf("totalPnL: got %v, want 80", got.TotalPnL)
	}
	// Weighted avg pnl pct: (10*0.05 + 5*-0.01) / 15 = 0.45 / 15 = 0.03
	if got.AvgPnLPct < 0.029 || got.AvgPnLPct > 0.031 {
		t.Fatalf("avgPnLPct: got %v, want ~0.03", got.AvgPnLPct)
	}
	if got.WinRate != 9.0/15.0 {
		t.Fatalf("winRate: got %v, want 0.6", got.WinRate)
	}
}

func TestSortedSleeveRegimeStable(t *testing.T) {
	idx := map[SleeveRegimeKey]repository.SleeveRegimeStat{
		{Sleeve: "trend", Regime: "trend_up"}:           {Sleeve: "trend", Regime: "trend_up"},
		{Sleeve: "mean_reversion", Regime: "range"}:    {Sleeve: "mean_reversion", Regime: "range"},
		{Sleeve: "trend", Regime: "trend_down"}:         {Sleeve: "trend", Regime: "trend_down"},
	}
	got := SortedSleeveRegime(idx)
	if len(got) != 3 {
		t.Fatalf("expected 3 cells, got %d", len(got))
	}
	want := []struct{ s, r string }{
		{"mean_reversion", "range"},
		{"trend", "trend_down"},
		{"trend", "trend_up"},
	}
	for i, w := range want {
		if got[i].Sleeve != w.s || got[i].Regime != w.r {
			t.Fatalf("position %d: got (%s,%s), want (%s,%s)", i, got[i].Sleeve, got[i].Regime, w.s, w.r)
		}
	}
}

func TestAttributionReportHasData(t *testing.T) {
	empty := AttributionReport{}
	if empty.HasData() {
		t.Fatal("empty report must report no data")
	}
	nonEmpty := AttributionReport{
		BySleeve: []repository.SleeveStat{{Sleeve: "trend", TradeCount: 1}},
	}
	if !nonEmpty.HasData() {
		t.Fatal("populated BySleeve should count as data")
	}
}
