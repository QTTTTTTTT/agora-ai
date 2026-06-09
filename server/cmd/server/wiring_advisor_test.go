// wiring_advisor_test.go — covers the only logic-rich glue in
// wiring_advisor.go: technicalSnapshotToBlock.
//
// The function does three things wire-side:
//
//   1. Projects a flat indicator.Snapshot onto the structured
//      agent.MasterTechnicalBlock that the LLM prompt + the
//      /daily-picks detail JSON both consume.
//   2. Classifies MA alignment (bullish / bearish / mixed) from
//      the SMA20/50/200 ordering.
//   3. Derives multi-window returns (1D / 5D / 20D / 52W-high)
//      from the bar tail.
//
// All three are pure functions of (Snapshot, []ohlc.Bar) so they
// are trivially testable without any external deps. Coverage here
// guards against regressions when we tweak the wire shape (which
// flows all the way to the React TechnicalSnapshotCard).

package main

import (
	"math"
	"testing"
	"time"

	"github.com/fundai/server/internal/indicator"
	"github.com/fundai/server/internal/ohlc"
)

// makeBars builds a synthetic OHLC tail with monotonically rising
// close prices so the multi-window returns have predictable signs.
// Closes go 100, 101, 102, … so close[N-1]/close[N-2]-1 ≈ 0.0098.
func makeBars(n int, startClose float64) []ohlc.Bar {
	out := make([]ohlc.Bar, n)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		c := startClose + float64(i)
		out[i] = ohlc.Bar{
			Time:   base.AddDate(0, 0, i),
			Open:   c - 0.5,
			High:   c + 0.7,
			Low:    c - 0.7,
			Close:  c,
			Volume: 1_000_000 + float64(i*1000),
		}
	}
	return out
}

func TestTechnicalSnapshotToBlock_NilOnEmpty(t *testing.T) {
	t.Parallel()
	// Zero snapshot + zero bars should return nil — guarded by the
	// early return so the advisor service can soft-fail and skip
	// the technical block entirely.
	if got := technicalSnapshotToBlock(indicator.Snapshot{}, nil); got != nil {
		t.Errorf("expected nil block for empty snapshot, got %+v", got)
	}
	// Even with bars, an all-zero snapshot (no LastClose) must
	// short-circuit — otherwise we'd ship a useless block to the
	// LLM and waste prompt tokens.
	if got := technicalSnapshotToBlock(indicator.Snapshot{}, makeBars(5, 100)); got != nil {
		t.Errorf("expected nil block when LastClose=0, got %+v", got)
	}
}

func TestTechnicalSnapshotToBlock_BullishAlignment(t *testing.T) {
	t.Parallel()
	bars := makeBars(260, 100) // 260 bars > 252 → triggers 52W scan
	s := indicator.Snapshot{
		BarsUsed:        260,
		LastClose:       359,
		SMA20:           350,
		SMA50:           300,
		SMA200:          250,
		RSI14:           62,
		RSI14Tag:        "", // in-band 30–70 → "neutral"
		MACDLine:        1.5,
		MACDSig:         1.2,
		MACDHist:        0.3,
		MACDCross:       "bullish",
		ATR14PctOfPx:    0.012,
		KDJK:            70,
		KDJD:            65,
		KDJJ:            80,
		LastVolume:      1_500_000,
		RelativeVolume:  1.4,
		SupportLevel:    330,
		ResistanceLevel: 370,
		SRWindow:        60,
		BreakoutState:   "none",
		Tags:            []string{"rsi:neutral", "macd:bullish_cross"},
	}
	got := technicalSnapshotToBlock(s, bars)
	if got == nil {
		t.Fatal("expected non-nil block")
	}

	// MA alignment: 20 > 50 > 200 → bullish (the classic O'Neil
	// "stage 2 uptrend" stack).
	if got.MAAlignment != "bullish" {
		t.Errorf("MAAlignment = %q, want bullish", got.MAAlignment)
	}
	// RSI zone: empty tag + 0 < RSI < anything → defaults to
	// "neutral" so the structured field is always populated.
	if got.RSI14Zone != "neutral" {
		t.Errorf("RSI14Zone = %q, want neutral", got.RSI14Zone)
	}
	// AsOf — last bar's UTC time, RFC3339.
	want := bars[len(bars)-1].Time.UTC().Format(time.RFC3339)
	if got.AsOf != want {
		t.Errorf("AsOf = %q, want %q", got.AsOf, want)
	}
	// Multi-window returns. Closes are 100, 101, …, 359.
	//   1D: 359/358 - 1 ≈ 0.002793
	//   5D: 359/354 - 1 ≈ 0.014124
	//  20D: 359/339 - 1 ≈ 0.058997
	//  52W: 359 / max(highs) — high[last]=359.7 so ≈ 359/359.7-1 ≈ -0.00195
	if !approxEq(got.PctChange1D, 359.0/358.0-1, 1e-6) {
		t.Errorf("PctChange1D = %v, want ~%v", got.PctChange1D, 359.0/358.0-1)
	}
	if !approxEq(got.PctChange5D, 359.0/354.0-1, 1e-6) {
		t.Errorf("PctChange5D = %v, want ~%v", got.PctChange5D, 359.0/354.0-1)
	}
	if !approxEq(got.PctChange20D, 359.0/339.0-1, 1e-6) {
		t.Errorf("PctChange20D = %v, want ~%v", got.PctChange20D, 359.0/339.0-1)
	}
	if got.PctChange52WHi >= 0 {
		t.Errorf("PctChange52WHi = %v, want negative (close < high)", got.PctChange52WHi)
	}
}

func TestTechnicalSnapshotToBlock_BearishAlignment(t *testing.T) {
	t.Parallel()
	bars := makeBars(50, 100)
	s := indicator.Snapshot{
		BarsUsed:  50,
		LastClose: 149,
		SMA20:     145,
		SMA50:     170,
		SMA200:    200,
		RSI14:     28,
		RSI14Tag:  "oversold",
	}
	got := technicalSnapshotToBlock(s, bars)
	if got == nil {
		t.Fatal("expected non-nil block")
	}
	// 20 < 50 < 200 → bearish stack ("stage 4 downtrend").
	if got.MAAlignment != "bearish" {
		t.Errorf("MAAlignment = %q, want bearish", got.MAAlignment)
	}
	if got.RSI14Zone != "oversold" {
		t.Errorf("RSI14Zone = %q, want oversold", got.RSI14Zone)
	}
}

func TestTechnicalSnapshotToBlock_MixedAlignment(t *testing.T) {
	t.Parallel()
	bars := makeBars(30, 100)
	s := indicator.Snapshot{
		LastClose: 129,
		SMA20:     130, // > 50
		SMA50:     120,
		SMA200:    140, // > 20 (mixed: short up, long down)
		RSI14:     75,
		RSI14Tag:  "overbought",
	}
	got := technicalSnapshotToBlock(s, bars)
	if got == nil {
		t.Fatal("expected non-nil block")
	}
	// Not bullish (200 above 20), not bearish (20 above 50) → mixed.
	if got.MAAlignment != "mixed" {
		t.Errorf("MAAlignment = %q, want mixed", got.MAAlignment)
	}
	if got.RSI14Zone != "overbought" {
		t.Errorf("RSI14Zone = %q, want overbought", got.RSI14Zone)
	}
}

func TestTechnicalSnapshotToBlock_ShortHistory(t *testing.T) {
	t.Parallel()
	// Only 3 bars — SMA50/200 are zero, MA alignment must not be
	// set (the `case s.SMA20 > 0 && s.SMA50 > 0 && s.SMA200 > 0`
	// guard ensures we don't lie about a "bullish stack" when the
	// stock is too young to have one).
	bars := makeBars(3, 100)
	s := indicator.Snapshot{
		LastClose: 102,
		SMA20:     101,
	}
	got := technicalSnapshotToBlock(s, bars)
	if got == nil {
		t.Fatal("expected non-nil block")
	}
	if got.MAAlignment != "" {
		t.Errorf("MAAlignment = %q, want empty (insufficient history)", got.MAAlignment)
	}
	// 1D should still compute (we have ≥2 bars).
	if got.PctChange1D == 0 {
		t.Errorf("PctChange1D = 0, want ~0.0099")
	}
	// 5D / 20D should be zero (insufficient lookback).
	if got.PctChange5D != 0 || got.PctChange20D != 0 {
		t.Errorf("expected zero longer-window returns; got 5D=%v 20D=%v",
			got.PctChange5D, got.PctChange20D)
	}
}

// approxEq is a tiny float helper — comparing returns at micro-pct
// granularity needs an explicit epsilon, not == which trips up on
// IEEE-754 rounding.
func approxEq(a, b, eps float64) bool {
	return math.Abs(a-b) <= eps
}
