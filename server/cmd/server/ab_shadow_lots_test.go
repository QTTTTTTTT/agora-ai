// Card K-4 — unit tests for the B-side lot ledger.
//
// These tests pin the ledger's economic semantics so K-3's NAV
// recompute can rely on it without re-deriving FIFO matching,
// realized-PnL accumulation, or over-sell clamping.

package main

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/fundai/server/internal/repository"
)

// approxEqual compares two floats with a small epsilon. We use
// 1e-6 because all inputs in these tests are at most 4 decimal
// places; anything larger means a real arithmetic mistake.
func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-6
}

func TestBSideLotLedger_EmptyState(t *testing.T) {
	l := newBSideLotLedger(100_000)
	pos, cash := l.PositionsAndCash()
	if len(pos) != 0 {
		t.Errorf("expected no positions, got %v", pos)
	}
	if cash != 100_000 {
		t.Errorf("cash should equal initial, got %v", cash)
	}
	if l.RealizedPnL() != 0 {
		t.Errorf("realized pnl should be 0 on empty ledger")
	}
	if l.InitialCash() != 100_000 {
		t.Errorf("initial cash mismatch")
	}
}

func TestBSideLotLedger_SingleBuyOpensPosition(t *testing.T) {
	l := newBSideLotLedger(100_000)
	day := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	res := l.Apply(day, "AAA", "BUY", 100, 50)

	if !approxEqual(res.Applied, 100) {
		t.Errorf("applied qty: want 100, got %v", res.Applied)
	}
	if !approxEqual(res.CashDelta, -5000) {
		t.Errorf("cash delta: want -5000, got %v", res.CashDelta)
	}
	if res.RealizedPnL != 0 {
		t.Errorf("BUY should not realize PnL, got %v", res.RealizedPnL)
	}
	if res.Clamped {
		t.Errorf("BUY should never be clamped")
	}
	pos, cash := l.PositionsAndCash()
	if !approxEqual(pos["AAA"], 100) {
		t.Errorf("AAA position: want 100, got %v", pos["AAA"])
	}
	if !approxEqual(cash, 95_000) {
		t.Errorf("cash: want 95000, got %v", cash)
	}
}

func TestBSideLotLedger_RoundTripBuyThenSellRealizesPnL(t *testing.T) {
	l := newBSideLotLedger(100_000)
	day1 := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	day2 := day1.Add(48 * time.Hour)
	l.Apply(day1, "AAA", "BUY", 100, 50)
	res := l.Apply(day2, "AAA", "SELL", 100, 60)

	// Sold 100 @ 60, cost basis 50 → +1000 realized
	if !approxEqual(res.RealizedPnL, 1000) {
		t.Errorf("realized pnl: want 1000, got %v", res.RealizedPnL)
	}
	if !approxEqual(res.Applied, 100) {
		t.Errorf("applied qty: want 100, got %v", res.Applied)
	}
	if !approxEqual(res.CashDelta, 6000) {
		t.Errorf("cash delta: want +6000, got %v", res.CashDelta)
	}
	if res.Clamped {
		t.Errorf("exact-match SELL should not be clamped")
	}
	pos, cash := l.PositionsAndCash()
	if _, ok := pos["AAA"]; ok {
		t.Errorf("position should be closed, got %v", pos)
	}
	// 100k - 5k (BUY) + 6k (SELL) = 101k
	if !approxEqual(cash, 101_000) {
		t.Errorf("cash: want 101000, got %v", cash)
	}
	if !approxEqual(l.RealizedPnL(), 1000) {
		t.Errorf("ledger pnl: want 1000, got %v", l.RealizedPnL())
	}
}

func TestBSideLotLedger_PartialSellLeavesRemainder(t *testing.T) {
	l := newBSideLotLedger(100_000)
	day := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	l.Apply(day, "AAA", "BUY", 100, 50)
	res := l.Apply(day.Add(time.Hour), "AAA", "SELL", 30, 55)

	if !approxEqual(res.RealizedPnL, 30*5) {
		t.Errorf("partial pnl: want 150, got %v", res.RealizedPnL)
	}
	pos, _ := l.PositionsAndCash()
	if !approxEqual(pos["AAA"], 70) {
		t.Errorf("remainder: want 70, got %v", pos["AAA"])
	}
}

func TestBSideLotLedger_FIFOAcrossMultipleLots(t *testing.T) {
	l := newBSideLotLedger(100_000)
	day1 := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	day2 := day1.Add(24 * time.Hour)
	day3 := day1.Add(48 * time.Hour)
	// Three BUY lots at increasing prices: 50, 60, 70.
	l.Apply(day1, "AAA", "BUY", 100, 50)
	l.Apply(day2, "AAA", "BUY", 100, 60)
	l.Apply(day3, "AAA", "BUY", 100, 70)
	// SELL 150 @ 80 — FIFO should consume lot1 (100 @ 50) +
	// lot2 partial (50 @ 60). PnL = 100*(80-50) + 50*(80-60)
	// = 3000 + 1000 = 4000.
	res := l.Apply(day3.Add(time.Hour), "AAA", "SELL", 150, 80)
	if !approxEqual(res.RealizedPnL, 4000) {
		t.Errorf("FIFO pnl: want 4000, got %v", res.RealizedPnL)
	}
	pos, _ := l.PositionsAndCash()
	// 50 left in lot2 (cost 60) + 100 in lot3 (cost 70) = 150
	if !approxEqual(pos["AAA"], 150) {
		t.Errorf("FIFO remainder: want 150, got %v", pos["AAA"])
	}
	// SELL the rest (150 @ 80). Lot2 had 50 @ 60 → 50*(80-60)=1000
	// Lot3 had 100 @ 70 → 100*(80-70)=1000. Total 2000.
	res = l.Apply(day3.Add(2*time.Hour), "AAA", "SELL", 150, 80)
	if !approxEqual(res.RealizedPnL, 2000) {
		t.Errorf("FIFO drain pnl: want 2000, got %v", res.RealizedPnL)
	}
	if !approxEqual(l.RealizedPnL(), 6000) {
		t.Errorf("running pnl: want 6000, got %v", l.RealizedPnL())
	}
	pos, _ = l.PositionsAndCash()
	if _, ok := pos["AAA"]; ok {
		t.Errorf("final position should be flat, got %v", pos)
	}
}

func TestBSideLotLedger_OverSellClampedToHeld(t *testing.T) {
	l := newBSideLotLedger(100_000)
	day := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	l.Apply(day, "AAA", "BUY", 100, 50)
	// LLM tries to dump 500 shares — we only hold 100.
	res := l.Apply(day.Add(time.Hour), "AAA", "SELL", 500, 60)

	if !res.Clamped {
		t.Errorf("over-sell must set Clamped=true")
	}
	if !approxEqual(res.Applied, 100) {
		t.Errorf("applied should clamp to 100, got %v", res.Applied)
	}
	if !approxEqual(res.RealizedPnL, 100*10) {
		t.Errorf("clamped pnl: want 1000, got %v", res.RealizedPnL)
	}
	pos, _ := l.PositionsAndCash()
	if _, ok := pos["AAA"]; ok {
		t.Errorf("after clamped over-sell should be flat, got %v", pos)
	}
}

func TestBSideLotLedger_SellWithoutInventoryIsNoOp(t *testing.T) {
	l := newBSideLotLedger(100_000)
	day := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	res := l.Apply(day, "AAA", "SELL", 50, 100)
	if !res.Clamped {
		t.Errorf("naked SELL must set Clamped=true to surface the anomaly")
	}
	if res.Applied != 0 {
		t.Errorf("naked SELL must not fill, got %v", res.Applied)
	}
	if res.CashDelta != 0 {
		t.Errorf("naked SELL must not change cash, got %v", res.CashDelta)
	}
	if l.Cash() != 100_000 {
		t.Errorf("ledger cash unchanged: want 100000, got %v", l.Cash())
	}
}

func TestBSideLotLedger_DegenerateInputsAreNoOps(t *testing.T) {
	l := newBSideLotLedger(100_000)
	day := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		side  string
		qty   float64
		price float64
		sym   string
	}{
		{"zero qty", "BUY", 0, 50, "AAA"},
		{"negative qty", "BUY", -10, 50, "AAA"},
		{"negative price", "BUY", 100, -1, "AAA"},
		{"empty symbol", "BUY", 100, 50, ""},
		{"unknown side", "HOLD", 100, 50, "AAA"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := l.Cash()
			res := l.Apply(day, tc.sym, tc.side, tc.qty, tc.price)
			if res.Applied != 0 {
				t.Errorf("expected no-op apply, got %+v", res)
			}
			if l.Cash() != before {
				t.Errorf("cash should be unchanged, before=%v after=%v", before, l.Cash())
			}
		})
	}
	pos, _ := l.PositionsAndCash()
	if len(pos) != 0 {
		t.Errorf("no positions should exist, got %v", pos)
	}
	// History still records every call (for diagnostics) — 5 calls.
	if got := len(l.History()); got != 5 {
		t.Errorf("history should record all 5 no-ops, got %d", got)
	}
}

func TestBSideLotLedger_NegativeCashIsAllowed(t *testing.T) {
	// V1 design: B can overdraw. The resulting NAV will look bad,
	// which is the correct economic signal.
	l := newBSideLotLedger(100)
	day := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	res := l.Apply(day, "AAA", "BUY", 100, 50)
	if !approxEqual(res.Applied, 100) {
		t.Errorf("BUY should still apply even when overdrawing")
	}
	if l.Cash() != 100-5000 {
		t.Errorf("cash should be -4900, got %v", l.Cash())
	}
}

func TestBSideLotLedger_HeldSymbolsSorted(t *testing.T) {
	l := newBSideLotLedger(100_000)
	day := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	for _, sym := range []string{"ZZZ", "AAA", "MMM"} {
		l.Apply(day, sym, "BUY", 10, 1)
	}
	got := l.HeldSymbols()
	want := []string{"AAA", "MMM", "ZZZ"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("symbols should be sorted: want %v, got %v", want, got)
	}
}

func TestBSideLotLedger_HistoryCarriesResults(t *testing.T) {
	l := newBSideLotLedger(100_000)
	day := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	l.Apply(day, "AAA", "BUY", 100, 50)
	l.Apply(day.Add(time.Hour), "AAA", "SELL", 60, 55)

	hist := l.History()
	if len(hist) != 2 {
		t.Fatalf("history len: want 2, got %d", len(hist))
	}
	if hist[0].Side != "BUY" || hist[0].Result.Applied != 100 {
		t.Errorf("history[0] mismatch: %+v", hist[0])
	}
	if hist[1].Side != "SELL" || !approxEqual(hist[1].Result.RealizedPnL, 60*5) {
		t.Errorf("history[1] mismatch: %+v", hist[1])
	}
}

func TestBSideLotLedger_NilSafe(t *testing.T) {
	var l *bSideLotLedger
	// none of these may panic
	res := l.Apply(time.Now(), "AAA", "BUY", 1, 1)
	if res.Applied != 0 {
		t.Errorf("nil ledger Apply should return zero result, got %+v", res)
	}
	if l.Cash() != 0 {
		t.Errorf("nil ledger Cash should be 0, got %v", l.Cash())
	}
	if l.RealizedPnL() != 0 {
		t.Errorf("nil ledger RealizedPnL should be 0")
	}
	if pos, cash := l.PositionsAndCash(); len(pos) != 0 || cash != 0 {
		t.Errorf("nil ledger snapshot should be empty, got pos=%v cash=%v", pos, cash)
	}
	if got := l.History(); got != nil {
		t.Errorf("nil ledger history should be nil, got %v", got)
	}
}

// ----------------------------------------------------------------------
// applyBSideDecision — A trade × decider response → B trade
// ----------------------------------------------------------------------

func TestApplyBSideDecision_Skip(t *testing.T) {
	_, _, _, ok := applyBSideDecision("BUY", 100, 50, abBSideDecision{Skip: true, QuantityScale: 1})
	if ok {
		t.Errorf("Skip=true must produce a no-op")
	}
}

func TestApplyBSideDecision_QuantityScale(t *testing.T) {
	q, p, side, ok := applyBSideDecision("BUY", 100, 50, abBSideDecision{QuantityScale: 0.5})
	if !ok {
		t.Errorf("expected ok=true")
	}
	if !approxEqual(q, 50) {
		t.Errorf("scaled qty: want 50, got %v", q)
	}
	if !approxEqual(p, 50) {
		t.Errorf("price preserved: want 50, got %v", p)
	}
	if side != "BUY" {
		t.Errorf("side preserved: want BUY, got %s", side)
	}
}

func TestApplyBSideDecision_SideOverride(t *testing.T) {
	_, _, side, ok := applyBSideDecision("BUY", 100, 50, abBSideDecision{QuantityScale: 1, SideOverride: "SELL"})
	if !ok {
		t.Errorf("expected ok=true")
	}
	if side != "SELL" {
		t.Errorf("override should flip side: want SELL, got %s", side)
	}
}

func TestApplyBSideDecision_ZeroScaleIsNoOp(t *testing.T) {
	_, _, _, ok := applyBSideDecision("BUY", 100, 50, abBSideDecision{QuantityScale: 0})
	// Defensive: zero scale falls back to 1 inside applyBSideDecision
	// (parser should clip to >= 0.05 already), so this should NOT be a no-op.
	if !ok {
		t.Errorf("zero scale should fall back to 1, not no-op")
	}
}

func TestApplyBSideDecision_InvalidSideOverrideDropsTrade(t *testing.T) {
	_, _, _, ok := applyBSideDecision("BUY", 100, 50, abBSideDecision{QuantityScale: 1, SideOverride: "HODL"})
	if ok {
		t.Errorf("invalid side override must drop the trade")
	}
}

func TestApplyBSideDecision_DefaultPreservesA(t *testing.T) {
	q, p, side, ok := applyBSideDecision("SELL", 50, 99.5, abBSideDecision{QuantityScale: 1})
	if !ok || !approxEqual(q, 50) || !approxEqual(p, 99.5) || side != "SELL" {
		t.Errorf("default decision should be identity, got q=%v p=%v side=%s ok=%v", q, p, side, ok)
	}
}

// ----------------------------------------------------------------------
// K-3 — price timeline + B NAV recomputation
// ----------------------------------------------------------------------

func TestPriceTimeline_PriceAt(t *testing.T) {
	pt := newPriceTimeline()
	day := func(d int) time.Time {
		return time.Date(2026, 5, d, 0, 0, 0, 0, time.UTC)
	}
	pt.Add("AAA", day(10), 50)
	pt.Add("AAA", day(15), 60)
	pt.Add("AAA", day(20), 55)
	pt.Add("BBB", day(12), 100)

	cases := []struct {
		name    string
		symbol  string
		date    time.Time
		want    float64
		wantHit bool
	}{
		{"exact match returns observation", "AAA", day(15), 60, true},
		{"between observations returns last <= date", "AAA", day(17), 60, true},
		{"after last observation returns last", "AAA", day(30), 55, true},
		{"before first observation returns nothing", "AAA", day(5), 0, false},
		{"unknown symbol returns nothing", "ZZZ", day(15), 0, false},
		{"first day exact match", "BBB", day(12), 100, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := pt.PriceAt(tc.symbol, tc.date)
			if ok != tc.wantHit {
				t.Errorf("hit: want %v, got %v", tc.wantHit, ok)
			}
			if !approxEqual(got, tc.want) {
				t.Errorf("price: want %v, got %v", tc.want, got)
			}
		})
	}
}

func TestPriceTimeline_NilSafe(t *testing.T) {
	var pt *priceTimeline
	pt.Add("AAA", time.Now(), 1) // must not panic
	if _, ok := pt.PriceAt("AAA", time.Now()); ok {
		t.Errorf("nil timeline must return !ok")
	}
}

func TestPriceTimeline_RejectsBadInputs(t *testing.T) {
	pt := newPriceTimeline()
	pt.Add("", time.Now(), 1)
	pt.Add("AAA", time.Now(), 0)
	pt.Add("AAA", time.Now(), -10)
	if _, ok := pt.PriceAt("AAA", time.Now()); ok {
		t.Errorf("rejected inputs should not produce observations")
	}
}

// ----------------------------------------------------------------------
// computeBSideNAVRows — the K-3 economic core. These tests pin
// the contract that drives every chart on the AB compare page.
// ----------------------------------------------------------------------

// makeAnav is a tiny helper to build NavSnapshot rows with just
// the fields computeBSideNAVRows actually consumes (date, NAV,
// total_assets).
func makeAnav(date time.Time, nav, totalAssets float64) repository.NavSnapshot {
	return repository.NavSnapshot{
		TradingDate: date,
		NAV:         nav,
		TotalAssets: totalAssets,
	}
}

func TestComputeBSideNAVRows_EmptyNAVsReturnsNil(t *testing.T) {
	rows := computeBSideNAVRows(nil, nil, newPriceTimeline(), 100_000)
	if rows != nil {
		t.Errorf("empty A NAVs should produce nil rows, got %v", rows)
	}
}

func TestComputeBSideNAVRows_NoTradesEqualsBaselineForever(t *testing.T) {
	// B never trades → ledger never mutates → cash stays at
	// initialCash → NAV stays at baseline forever. This is the
	// degenerate-but-valid case (LLM said skip every trade).
	day := func(d int) time.Time {
		return time.Date(2026, 5, d, 0, 0, 0, 0, time.UTC)
	}
	aNavs := []repository.NavSnapshot{
		makeAnav(day(10), 1.00, 100_000),
		makeAnav(day(11), 1.05, 105_000),
		makeAnav(day(12), 0.95, 95_000),
	}
	rows := computeBSideNAVRows(nil, aNavs, newPriceTimeline(), 100_000)
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	for i, r := range rows {
		if !approxEqual(r.NAV, 1.00) {
			t.Errorf("row[%d] NAV: want 1.00 (baseline), got %v", i, r.NAV)
		}
		if !approxEqual(r.Cash, 100_000) {
			t.Errorf("row[%d] cash: want 100k, got %v", i, r.Cash)
		}
		if !approxEqual(r.CumulativeReturn, 0) {
			t.Errorf("row[%d] cumret: want 0, got %v", i, r.CumulativeReturn)
		}
	}
}

func TestComputeBSideNAVRows_BuyHoldRevaluesAtMarketPrice(t *testing.T) {
	// B buys 100 of AAA @ 50 on day 10 (cost = 5000).
	// Price observed on day 11 @ 55, day 12 @ 60.
	// Day 10: cash=95k, MV=100*50=5000 → assets=100k → NAV=1.00
	// Day 11: cash=95k, MV=100*55=5500 → assets=100.5k → NAV=1.005
	// Day 12: cash=95k, MV=100*60=6000 → assets=101.0k → NAV=1.010
	day := func(d int) time.Time {
		return time.Date(2026, 5, d, 0, 0, 0, 0, time.UTC)
	}
	history := []bSideAppliedTrade{
		{Date: day(10), Symbol: "AAA", Side: "BUY", Quantity: 100, Price: 50, Result: bSideLotApplyResult{Applied: 100, CashDelta: -5000}},
	}
	pt := newPriceTimeline()
	pt.Add("AAA", day(10), 50)
	pt.Add("AAA", day(11), 55)
	pt.Add("AAA", day(12), 60)
	aNavs := []repository.NavSnapshot{
		makeAnav(day(10), 1.00, 100_000),
		makeAnav(day(11), 1.05, 105_000),
		makeAnav(day(12), 1.10, 110_000),
	}
	rows := computeBSideNAVRows(history, aNavs, pt, 100_000)
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	expectedNAVs := []float64{1.000, 1.005, 1.010}
	for i, want := range expectedNAVs {
		if !approxEqual(rows[i].NAV, want) {
			t.Errorf("day %d NAV: want %v, got %v", i, want, rows[i].NAV)
		}
	}
	// B underperforms A here on purpose (A: +10%, B: +1%) —
	// the assertion is that B's NAV reflects ITS OWN trades,
	// not A's TotalAssets growth.
	for i := range rows {
		if approxEqual(rows[i].NAV, aNavs[i].NAV) && i > 0 {
			t.Errorf("row[%d] NAV must NOT track A's NAV: A=%v, B=%v", i, aNavs[i].NAV, rows[i].NAV)
		}
	}
}

func TestComputeBSideNAVRows_FallsBackToCostBasisWhenNoPrice(t *testing.T) {
	// B holds 100 of AAA @ cost 50; price timeline has nothing.
	// MTM should fall back to cost basis → NAV stays flat at
	// baseline (no realized PnL, no MTM gain).
	day := func(d int) time.Time {
		return time.Date(2026, 5, d, 0, 0, 0, 0, time.UTC)
	}
	history := []bSideAppliedTrade{
		{Date: day(10), Symbol: "AAA", Side: "BUY", Quantity: 100, Price: 50},
	}
	pt := newPriceTimeline() // empty on purpose
	aNavs := []repository.NavSnapshot{
		makeAnav(day(10), 1.00, 100_000),
		makeAnav(day(11), 1.05, 105_000),
	}
	rows := computeBSideNAVRows(history, aNavs, pt, 100_000)
	for i, r := range rows {
		if !approxEqual(r.NAV, 1.00) {
			t.Errorf("row[%d] NAV with cost-basis fallback: want 1.00, got %v", i, r.NAV)
		}
	}
}

func TestComputeBSideNAVRows_RealizedPnLFlowsToCash(t *testing.T) {
	// BUY 100 @ 50 on day 10, SELL 100 @ 60 on day 12. After
	// day 12, cash should be 100k - 5k + 6k = 101k → NAV=1.01.
	day := func(d int) time.Time {
		return time.Date(2026, 5, d, 0, 0, 0, 0, time.UTC)
	}
	history := []bSideAppliedTrade{
		{Date: day(10), Symbol: "AAA", Side: "BUY", Quantity: 100, Price: 50},
		{Date: day(12), Symbol: "AAA", Side: "SELL", Quantity: 100, Price: 60},
	}
	pt := newPriceTimeline()
	pt.Add("AAA", day(10), 50)
	pt.Add("AAA", day(12), 60)
	aNavs := []repository.NavSnapshot{
		makeAnav(day(10), 1.00, 100_000),
		makeAnav(day(11), 1.05, 105_000),
		makeAnav(day(12), 0.90, 90_000),
	}
	rows := computeBSideNAVRows(history, aNavs, pt, 100_000)
	if !approxEqual(rows[2].Cash, 101_000) {
		t.Errorf("day 12 cash: want 101k, got %v", rows[2].Cash)
	}
	if !approxEqual(rows[2].NAV, 1.01) {
		t.Errorf("day 12 NAV: want 1.01, got %v", rows[2].NAV)
	}
	// Day 11 should reflect MTM only (no SELL yet): cash=95k,
	// position=100*55... but priceTL has no day-11 entry, so
	// mark falls back to cost (50). MV=5000, assets=100k.
	if !approxEqual(rows[1].NAV, 1.00) {
		t.Errorf("day 11 NAV (cost basis fallback): want 1.00, got %v", rows[1].NAV)
	}
}

func TestComputeBSideNAVRows_DrawdownAndCumulative(t *testing.T) {
	// Construct a clear peak-then-drop scenario so we can pin
	// the drawdown calculation.
	day := func(d int) time.Time {
		return time.Date(2026, 5, d, 0, 0, 0, 0, time.UTC)
	}
	history := []bSideAppliedTrade{
		{Date: day(10), Symbol: "AAA", Side: "BUY", Quantity: 100, Price: 50},
	}
	pt := newPriceTimeline()
	pt.Add("AAA", day(10), 50)
	pt.Add("AAA", day(11), 60) // peak: MV=6000, assets=101k, NAV=1.01
	pt.Add("AAA", day(12), 40) // trough: MV=4000, assets=99k, NAV=0.99
	aNavs := []repository.NavSnapshot{
		makeAnav(day(10), 1.00, 100_000),
		makeAnav(day(11), 1.05, 105_000),
		makeAnav(day(12), 0.95, 95_000),
	}
	rows := computeBSideNAVRows(history, aNavs, pt, 100_000)
	if !approxEqual(rows[1].NAV, 1.01) || !approxEqual(rows[1].Drawdown, 0) {
		t.Errorf("day 11 should be the peak, NAV=1.01 Drawdown=0, got NAV=%v DD=%v", rows[1].NAV, rows[1].Drawdown)
	}
	// Day 12: NAV=0.99, peak so far=1.01 → DD = 0.99/1.01-1 ≈ -1.98%
	wantDD := 0.99/1.01 - 1
	if !approxEqual(rows[2].NAV, 0.99) || !approxEqual(rows[2].Drawdown, wantDD) {
		t.Errorf("day 12 DD: want NAV=0.99 DD=%v, got NAV=%v DD=%v", wantDD, rows[2].NAV, rows[2].Drawdown)
	}
	// Cumulative on day 12: 0.99/1.00 - 1 = -0.01
	if !approxEqual(rows[2].CumulativeReturn, -0.01) {
		t.Errorf("day 12 cumret: want -0.01, got %v", rows[2].CumulativeReturn)
	}
}

func TestComputeBSideNAVRows_HandlesZeroInitialCash(t *testing.T) {
	// Defensive: a corrupt fund with 0 starting capital must
	// not blow up the analyze run with a div-by-zero. We expect
	// rows to fall back to baseline rather than NaN.
	day := func(d int) time.Time {
		return time.Date(2026, 5, d, 0, 0, 0, 0, time.UTC)
	}
	aNavs := []repository.NavSnapshot{makeAnav(day(10), 1.00, 0)}
	rows := computeBSideNAVRows(nil, aNavs, newPriceTimeline(), 0)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if math.IsNaN(rows[0].NAV) || math.IsInf(rows[0].NAV, 0) {
		t.Errorf("zero-init NAV must not be NaN/Inf, got %v", rows[0].NAV)
	}
}

func TestComputeBSideNAVRows_FloorsAtBaselineDiv100(t *testing.T) {
	// B blows itself up: BUY 100 @ 50 on day 10, then no
	// further trades. priceTimeline drops the price to 0.01 on
	// day 11. assets = 95k + 100*0.01 = 95001 → NAV = 0.95001.
	// (Not below baseline/100 yet but still useful sanity.)
	day := func(d int) time.Time {
		return time.Date(2026, 5, d, 0, 0, 0, 0, time.UTC)
	}
	history := []bSideAppliedTrade{
		{Date: day(10), Symbol: "AAA", Side: "BUY", Quantity: 100, Price: 50},
	}
	pt := newPriceTimeline()
	pt.Add("AAA", day(10), 50)
	pt.Add("AAA", day(11), 0.01)
	aNavs := []repository.NavSnapshot{
		makeAnav(day(10), 1.00, 100_000),
		makeAnav(day(11), 1.00, 100_000),
	}
	rows := computeBSideNAVRows(history, aNavs, pt, 100_000)
	if rows[1].NAV <= 0 || math.IsNaN(rows[1].NAV) {
		t.Errorf("collapsed NAV must stay positive, got %v", rows[1].NAV)
	}
}

// TestComputeBSideNAVRows_SameDayTradeCountsForToday is the
// regression test for the K-3 day-truncation bug discovered in
// the Saturday 2026-05-30 docker smoke. Trade timestamps come
// from trade_executions.created_at (during market hours, e.g.
// 09:30 UTC) while NAV bars come from nav_snapshots.trading_date
// (midnight UTC). Without day-truncation, the BUY's full
// timestamp is "after" the same-day NAV's midnight timestamp,
// so the replay defers the trade to "tomorrow" — but tomorrow's
// NAV may not exist (e.g. test window has only 2 days), so the
// trade silently falls through and B's NAV stays at baseline.
//
// Symptom in production: B's variant_b_trades row showed BUY
// 280@239, but ab_test_variant_nav.cash equalled initialCash —
// the BUY never landed in the replay ledger.
func TestComputeBSideNAVRows_SameDayTradeCountsForToday(t *testing.T) {
	// NAV bar is at midnight UTC; trade is during market hours.
	navMidnight := time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)
	tradeIntraday := time.Date(2026, 5, 29, 9, 30, 0, 0, time.UTC)
	history := []bSideAppliedTrade{
		{Date: tradeIntraday, Symbol: "688205", Side: "BUY", Quantity: 280, Price: 239.35},
	}
	pt := newPriceTimeline()
	pt.Add("688205", tradeIntraday, 239.35)
	aNavs := []repository.NavSnapshot{
		makeAnav(time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC), 1.00, 1_002_004.73),
		makeAnav(navMidnight, 0.99, 997_000),
	}
	rows := computeBSideNAVRows(history, aNavs, pt, 1_002_004.73)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	// Day 1 — no trade yet → cash equals initial.
	if !approxEqual(rows[0].Cash, 1_002_004.73) {
		t.Errorf("day 1 cash: want initial (1_002_004.73), got %v", rows[0].Cash)
	}
	// Day 2 — the BUY MUST have applied. Cash should be debited
	// by the BUY notional regardless of whether its timestamp
	// is during market hours.
	wantCashDay2 := 1_002_004.73 - 280*239.35
	if !approxEqual(rows[1].Cash, wantCashDay2) {
		t.Errorf("day 2 cash: want %v (initial - 280*239.35), got %v — same-day trade did NOT apply, K-3 day-truncation regression", wantCashDay2, rows[1].Cash)
	}
	// Position MTM at the same observed price → total assets
	// stays at initial (cost == mark on entry day).
	if !approxEqual(rows[1].TotalAssets, 1_002_004.73) {
		t.Errorf("day 2 total_assets: want initial (cash + MV at mark), got %v", rows[1].TotalAssets)
	}
}

// TestPriceTimeline_PriceAt_SameDayTimestampHits pins the
// matching fix in the price timeline. Without day-granularity
// matching, a trade observed at 09:30 UTC on day X would NOT be
// visible to a `PriceAt(symbol, X@00:00)` lookup, so B's MTM
// would fall back to AvgCostBasis even when a fresh quote exists.
func TestPriceTimeline_PriceAt_SameDayTimestampHits(t *testing.T) {
	pt := newPriceTimeline()
	intraday := time.Date(2026, 5, 29, 9, 30, 0, 0, time.UTC)
	pt.Add("ABC", intraday, 42)
	midnight := time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)
	got, ok := pt.PriceAt("ABC", midnight)
	if !ok || !approxEqual(got, 42) {
		t.Errorf("midnight lookup of same-day intraday price should hit, got %v ok=%v", got, ok)
	}
}
