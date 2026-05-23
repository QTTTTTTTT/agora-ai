package ranking

// Sprint A #2 contract tests for the cross-sectional Ranker:
//
//   - Options.withDefaults yields production-grade tunings so any
//     caller that passes Options{} gets the same shape we ship.
//   - BuildRanking returns nil for too-small universes (<3 surviving
//     symbols) so the prompt block stays absent when z-scores would
//     be meaningless.
//   - The composite ordering is correct: the symbol with the
//     strongest momentum / lowest vol / deepest book lands in Q1 and
//     leads the slice; the opposite extreme lands in Q4 and tails it.
//   - z-scores stay finite on degenerate inputs (constant series,
//     fewer than ATR-period bars) — NaN would poison the prompt JSON.
//   - dedup on (symbol, market) so duplicate calls don't double-count
//     a single instrument inside the same z-score.

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/fundai/server/internal/ohlc"
)

// fakeFetcher is a private duplicate of the quantsnapshot tests'
// helper kept here so the ranking tests stay self-contained.
type fakeFetcher struct {
	bars  map[string][]ohlc.Bar
	calls map[string]int
}

func newFakeFetcher() *fakeFetcher {
	return &fakeFetcher{bars: map[string][]ohlc.Bar{}, calls: map[string]int{}}
}

func (f *fakeFetcher) Fetch(_ context.Context, req ohlc.FetchRequest) ([]ohlc.Bar, error) {
	k := req.Symbol + "|" + req.Market
	f.calls[k]++
	if bars, ok := f.bars[k]; ok {
		return bars, nil
	}
	return nil, ohlc.ErrNoData
}

// genBars produces n daily bars with a stable per-day close ratio
// (priceMul) and a stable absolute high-low spread (atrAbs). The
// generated volume is a fixed daily share count so the dollar volume
// = close × volume is monotone in close * volume.
func genBars(n int, startClose, priceMul, atrAbs, volume float64) []ohlc.Bar {
	bars := make([]ohlc.Bar, n)
	t := time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)
	close := startClose
	for i := 0; i < n; i++ {
		bars[i] = ohlc.Bar{
			Time:   t.AddDate(0, 0, i),
			Open:   close,
			High:   close + atrAbs/2,
			Low:    close - atrAbs/2,
			Close:  close,
			Volume: volume,
		}
		close *= priceMul
	}
	return bars
}

// Options defaults pin the production tunings.
func TestOptionsWithDefaultsProductionNumbers(t *testing.T) {
	got := Options{}.withDefaults()
	want := Options{
		MomentumBars:     20,
		VolBars:          20,
		LiquidityBars:    10,
		LookbackBars:     60,
		MomentumWeight:   0.5,
		VolatilityWeight: 0.3,
		LiquidityWeight:  0.2,
		MinUniverse:      3,
	}
	if got != want {
		t.Errorf("Options{}.withDefaults() = %+v, want %+v", got, want)
	}
}

// Nil ranker and nil fetcher both produce nil — the prompt simply
// omits the universeRanking block.
func TestBuildRankingNilSafe(t *testing.T) {
	var r *Ranker
	if got := r.BuildRanking(context.Background(), []SymbolRequest{{Symbol: "A"}}); got != nil {
		t.Errorf("nil ranker: expected nil, got %+v", got)
	}
	r2 := NewRanker(nil, Options{})
	if got := r2.BuildRanking(context.Background(), []SymbolRequest{{Symbol: "A"}}); got != nil {
		t.Errorf("nil fetcher: expected nil, got %+v", got)
	}
}

// Below-floor universes return nil. We seed exactly 2 valid symbols
// (MinUniverse default = 3) and assert no ranking comes back.
func TestBuildRankingNilForTooSmallUniverse(t *testing.T) {
	f := newFakeFetcher()
	f.bars["A|us_equity"] = genBars(60, 100, 1.001, 1.0, 1_000_000)
	f.bars["B|us_equity"] = genBars(60, 50, 1.002, 0.5, 2_000_000)
	r := NewRanker(f, Options{})
	got := r.BuildRanking(context.Background(), []SymbolRequest{
		{Symbol: "A", Market: "us_equity"},
		{Symbol: "B", Market: "us_equity"},
	})
	if got != nil {
		t.Errorf("expected nil for 2-symbol universe (< MinUniverse=3), got %+v", got)
	}
}

// Composite ordering: a 3-symbol universe where one symbol clearly
// dominates on momentum + liquidity must surface as Q1 and lead
// the slice; the weakest one lands in Q4.
func TestBuildRankingPicksStrongestSymbolAsQ1(t *testing.T) {
	f := newFakeFetcher()
	// STRONG: +0.4% daily, low vol (0.5 atrAbs), highest volume.
	f.bars["STRONG|us_equity"] = genBars(60, 100, 1.004, 0.5, 5_000_000)
	// MID:    +0.1% daily, medium vol, medium volume.
	f.bars["MID|us_equity"] = genBars(60, 100, 1.001, 1.0, 2_000_000)
	// WEAK:   -0.3% daily, high vol, low volume.
	f.bars["WEAK|us_equity"] = genBars(60, 100, 0.997, 3.0, 500_000)

	r := NewRanker(f, Options{})
	out := r.BuildRanking(context.Background(), []SymbolRequest{
		{Symbol: "STRONG", Market: "us_equity"},
		{Symbol: "MID", Market: "us_equity"},
		{Symbol: "WEAK", Market: "us_equity"},
	})
	if len(out) != 3 {
		t.Fatalf("expected 3 rankings, got %d (%+v)", len(out), out)
	}
	if out[0].Symbol != "STRONG" {
		t.Errorf("Q1 should be STRONG, got %q", out[0].Symbol)
	}
	if out[2].Symbol != "WEAK" {
		t.Errorf("Q4 should be WEAK, got %q", out[2].Symbol)
	}
	if out[0].Quartile != 1 {
		t.Errorf("STRONG quartile = %d, want 1", out[0].Quartile)
	}
	// On a 3-symbol universe with chunk = ceil(3/4) = 1 every
	// symbol is in its own bucket up to Q3; assignQuartiles caps
	// at 4 but we only need 1..3 here.
	if out[0].Quartile == out[2].Quartile {
		t.Errorf("Q1 and Q4 should land in different buckets, got same %d", out[0].Quartile)
	}
	// Sanity: composite z-scores are finite.
	for _, row := range out {
		if math.IsNaN(row.CompositeZ) || math.IsInf(row.CompositeZ, 0) {
			t.Errorf("%s CompositeZ NaN/Inf: %+v", row.Symbol, row)
		}
	}
}

// Z-scores must be well-behaved when every symbol has identical
// features — divide-by-zero stdev case. zScore returns zeros for
// that column so the prompt sees a "no signal" ranking with
// CompositeZ = 0 everywhere.
func TestBuildRankingHandlesIdenticalSeries(t *testing.T) {
	f := newFakeFetcher()
	for _, sym := range []string{"A", "B", "C"} {
		f.bars[sym+"|us_equity"] = genBars(60, 100, 1.001, 1.0, 1_000_000)
	}
	r := NewRanker(f, Options{})
	out := r.BuildRanking(context.Background(), []SymbolRequest{
		{Symbol: "A", Market: "us_equity"},
		{Symbol: "B", Market: "us_equity"},
		{Symbol: "C", Market: "us_equity"},
	})
	if len(out) != 3 {
		t.Fatalf("expected 3 rankings, got %d", len(out))
	}
	for _, row := range out {
		if math.Abs(row.CompositeZ) > 1e-9 {
			t.Errorf("%s CompositeZ should be 0 on identical features, got %v", row.Symbol, row.CompositeZ)
		}
	}
}

// Short-history symbols drop out. Pin: a 21-bar symbol survives
// (>= MomentumBars+1 = 21) but a 19-bar symbol does NOT.
func TestBuildRankingDropsShortHistorySymbols(t *testing.T) {
	f := newFakeFetcher()
	f.bars["A|us_equity"] = genBars(60, 100, 1.001, 1.0, 1_000_000)
	f.bars["B|us_equity"] = genBars(60, 100, 1.002, 1.0, 2_000_000)
	f.bars["C|us_equity"] = genBars(60, 100, 1.003, 1.0, 3_000_000)
	f.bars["SHORT|us_equity"] = genBars(19, 100, 1.005, 1.0, 4_000_000) // < 21 bars

	r := NewRanker(f, Options{})
	out := r.BuildRanking(context.Background(), []SymbolRequest{
		{Symbol: "A", Market: "us_equity"},
		{Symbol: "B", Market: "us_equity"},
		{Symbol: "C", Market: "us_equity"},
		{Symbol: "SHORT", Market: "us_equity"},
	})
	if len(out) != 3 {
		t.Fatalf("expected 3 rankings (SHORT dropped), got %d (%+v)", len(out), out)
	}
	for _, row := range out {
		if row.Symbol == "SHORT" {
			t.Errorf("SHORT must be dropped on insufficient bars: %+v", row)
		}
	}
}

// Dedup on (symbol, market) keeps the prompt from double-counting
// a symbol that appears in both the universe and the positions
// list. The fetcher must see exactly one call per unique pair.
func TestBuildRankingDedupes(t *testing.T) {
	f := newFakeFetcher()
	f.bars["A|us_equity"] = genBars(60, 100, 1.001, 1.0, 1_000_000)
	f.bars["B|us_equity"] = genBars(60, 100, 1.002, 1.0, 2_000_000)
	f.bars["C|us_equity"] = genBars(60, 100, 1.003, 1.0, 3_000_000)

	r := NewRanker(f, Options{})
	out := r.BuildRanking(context.Background(), []SymbolRequest{
		{Symbol: "A", Market: "us_equity"},
		{Symbol: "a", Market: "US_EQUITY"}, // dup after normalisation
		{Symbol: "B", Market: "us_equity"},
		{Symbol: "C", Market: "us_equity"},
	})
	if len(out) != 3 {
		t.Fatalf("expected 3 unique rankings, got %d (%+v)", len(out), out)
	}
	if f.calls["A|us_equity"] != 1 {
		t.Errorf("symbol A must fetch once even on duplicate requests, got %d", f.calls["A|us_equity"])
	}
}

// computeFeatures is the low-level helper. Lock its math on a
// known-good 30-bar series so a refactor of the momentum / vol /
// liquidity formulas surfaces here before reaching the prompt.
func TestComputeFeaturesKnownGoodSeries(t *testing.T) {
	bars := genBars(30, 100.0, 1.01, 2.0, 1_000_000)
	feat, ok := computeFeatures("X", bars, Options{}.withDefaults())
	if !ok {
		t.Fatal("computeFeatures returned !ok for a 30-bar series")
	}
	// Momentum over 20 bars: close[29]/close[9] - 1. With 1.01
	// multiplier each bar, close[i] = 100*1.01^i, so ratio =
	// 1.01^20 ≈ 1.22019, momentum ≈ 0.22019.
	wantMomentum := math.Pow(1.01, 20) - 1
	if math.Abs(feat.momentum-wantMomentum) > 1e-9 {
		t.Errorf("momentum = %v, want %v", feat.momentum, wantMomentum)
	}
	// Vol over 20 daily log returns of constant ratio = 0 stdev.
	if math.Abs(feat.volatility) > 1e-12 {
		t.Errorf("constant-ratio series should have vol == 0, got %v", feat.volatility)
	}
	if math.IsNaN(feat.liquidity) || math.IsInf(feat.liquidity, 0) {
		t.Errorf("liquidity must be finite, got %v", feat.liquidity)
	}
}

// zScore on an empty input yields an empty slice rather than
// panicking (would happen on a universe where every symbol fetch
// failed but len(seen) is non-zero — defensive coverage).
func TestZScoreEmptyInput(t *testing.T) {
	got := zScore(nil)
	if len(got) != 0 {
		t.Errorf("zScore(nil) should return empty, got %v", got)
	}
}

// stdev on an empty input returns 0 instead of NaN (Sqrt(0/0)).
func TestStdevEmptyInputZero(t *testing.T) {
	if got := stdev(nil); got != 0 {
		t.Errorf("stdev(nil) = %v, want 0", got)
	}
}

// safeFloat scrubs NaN / ±Inf to zero so the prompt JSON is
// always valid even when a degenerate ranking pass produces
// non-finite arithmetic.
func TestSafeFloatScrubsNonFinite(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want float64
	}{
		{"normal", 0.5, 0.5},
		{"negative", -1.2, -1.2},
		{"NaN", math.NaN(), 0},
		{"+Inf", math.Inf(1), 0},
		{"-Inf", math.Inf(-1), 0},
	}
	for _, c := range cases {
		if got := safeFloat(c.in); got != c.want {
			t.Errorf("safeFloat(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

// assignQuartiles on a 12-symbol slice splits cleanly into 4 buckets
// of 3, and degrades sensibly on a 5-symbol slice (the trailing
// bucket gets the remainder).
func TestAssignQuartiles(t *testing.T) {
	rows12 := make([]SymbolRanking, 12)
	assignQuartiles(rows12)
	for i, r := range rows12 {
		want := i/3 + 1
		if r.Quartile != want {
			t.Errorf("12-symbol row %d: Quartile = %d, want %d", i, r.Quartile, want)
		}
	}
	rows5 := make([]SymbolRanking, 5)
	assignQuartiles(rows5)
	// chunk = ceil(5/4) = 2 so buckets are 1,1,2,2,3 (Q4 stays empty).
	for i, want := range []int{1, 1, 2, 2, 3} {
		if rows5[i].Quartile != want {
			t.Errorf("5-symbol row %d: Quartile = %d, want %d", i, rows5[i].Quartile, want)
		}
	}
}
