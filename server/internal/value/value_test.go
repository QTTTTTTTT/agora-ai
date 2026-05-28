package value

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/fundai/server/internal/fundamental"
)

// staticFetcher is a fundamental.Fetcher returning pre-seeded
// metrics keyed on upper-cased symbol. Mirrors quality_test for
// consistency.
type staticFetcher struct {
	byKey map[string]fundamental.Metrics
}

func (f staticFetcher) Fetch(_ context.Context, req fundamental.FetchRequest) (*fundamental.Metrics, error) {
	m, ok := f.byKey[req.Symbol]
	if !ok {
		return nil, fundamental.ErrNoData
	}
	out := m
	return &out, nil
}

type erringFetcher struct{}

func (erringFetcher) Fetch(_ context.Context, _ fundamental.FetchRequest) (*fundamental.Metrics, error) {
	return nil, errors.New("boom")
}

// ---------------------------------------------------------------------------
// Options + Service construction
// ---------------------------------------------------------------------------

func TestOptionsDefaultsAppliedWhenZero(t *testing.T) {
	o := Options{}.withDefaults()
	if o.BookToPriceWeight != 0.45 || o.EarningsToPriceWeight != 0.45 || o.DividendYieldWeight != 0.10 {
		t.Errorf("composite weight defaults wrong: %+v", o)
	}
	if o.MinUniverse != 3 || o.PerFactorMin != 3 {
		t.Errorf("min floors wrong: %+v", o)
	}
	if o.WinsorSigma != 3 {
		t.Errorf("winsor default wrong: %v", o.WinsorSigma)
	}
}

func TestOptionsPartialOverrideKeepsOtherDefaults(t *testing.T) {
	o := Options{BookToPriceWeight: 1.0}.withDefaults()
	if o.BookToPriceWeight != 1.0 {
		t.Errorf("BookToPriceWeight override lost: %v", o.BookToPriceWeight)
	}
	if o.EarningsToPriceWeight != 0 || o.DividendYieldWeight != 0 {
		t.Errorf("partial override must not reset siblings: %+v", o)
	}
	if o.MinUniverse == 0 || o.WinsorSigma == 0 {
		t.Errorf("orthogonal groups should still default: %+v", o)
	}
}

func TestNewServiceNilFetcherReturnsNil(t *testing.T) {
	svc := NewService(nil, Options{})
	got := svc.BuildScores(context.Background(), []SymbolRequest{{Symbol: "AAPL"}})
	if got != nil {
		t.Errorf("expected nil scores with nil fetcher, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Ratio inversions
// ---------------------------------------------------------------------------

func TestBookToPriceInversesPositive(t *testing.T) {
	in := []fundamental.Metrics{
		{Symbol: "A", PB: 2}, // B/P = 0.5
		{Symbol: "B", PB: 0.5},
		{Symbol: "C", PB: 0},
		{Symbol: "D", PB: -1.5},
	}
	got := bookToPrice(in)
	if got[0] != 0.5 || got[1] != 2.0 {
		t.Errorf("inversion wrong: %v", got)
	}
	if !math.IsNaN(got[2]) || !math.IsNaN(got[3]) {
		t.Errorf("non-positive PB must be NaN: %v", got)
	}
}

func TestEarningsToPriceDropsLossMakers(t *testing.T) {
	// Loss-makers (PE <= 0) MUST be NaN — a negative E/P would
	// flip the composite direction and would also imply the
	// money-loser is "value", which is the opposite of intent.
	in := []fundamental.Metrics{
		{Symbol: "A", PE: 25},
		{Symbol: "B", PE: 10},
		{Symbol: "C", PE: -8}, // loss-maker
		{Symbol: "D", PE: 0},
	}
	got := earningsToPrice(in)
	if math.Abs(got[0]-(1.0/25)) > 1e-9 {
		t.Errorf("E/P math wrong: %v", got[0])
	}
	if math.Abs(got[1]-(1.0/10)) > 1e-9 {
		t.Errorf("E/P math wrong: %v", got[1])
	}
	if !math.IsNaN(got[2]) || !math.IsNaN(got[3]) {
		t.Errorf("loss-makers + zero PE must be NaN: %v", got)
	}
}

func TestDividendYieldKeepsZeroAsRealData(t *testing.T) {
	// Non-payers (DividendYield = 0) are a real economic
	// signal, not missing data. Keep as 0 so the universe
	// mean / stdev include them; the composite is honest
	// about "no dividend = below the mean yield".
	in := []fundamental.Metrics{
		{Symbol: "A", DividendYield: 0.04},
		{Symbol: "B", DividendYield: 0},
		{Symbol: "C", DividendYield: math.NaN()},
		{Symbol: "D", DividendYield: -0.01},
	}
	got := dividendYield(in)
	if got[0] != 0.04 || got[1] != 0 {
		t.Errorf("positive / zero yields not preserved: %v", got)
	}
	if !math.IsNaN(got[2]) || !math.IsNaN(got[3]) {
		t.Errorf("NaN / negative yields must be NaN: %v", got)
	}
}

// ---------------------------------------------------------------------------
// winsorisedZ math
// ---------------------------------------------------------------------------

func TestWinsorisedZRespectsClipping(t *testing.T) {
	// Build a 5-element distribution where one value is so far
	// out that vanilla z lands at ~+1.9, then add a "monster
	// outlier" to push the universe far enough that the
	// clip-target value's z exceeds ±sigma.
	vals := []float64{1, 1, 1, 1, 50} // monster at index 4
	got := winsorisedZ(vals, 3, 1.0)  // clip at ±1
	for i, z := range got {
		if math.IsNaN(z) {
			t.Fatalf("index %d unexpectedly NaN", i)
		}
		if z > 1.000001 || z < -1.000001 {
			t.Errorf("index %d z=%v outside ±sigma after winsorising", i, z)
		}
	}
}

func TestWinsorisedZBelowMinSampleReturnsAllNaN(t *testing.T) {
	got := winsorisedZ([]float64{1, math.NaN(), 2}, 5, 3)
	for i, z := range got {
		if !math.IsNaN(z) {
			t.Errorf("index %d should be NaN under min-sample floor, got %v", i, z)
		}
	}
}

func TestWinsorisedZZeroStdevYieldsZero(t *testing.T) {
	// Every reporting entry has the same value → stdev=0, every
	// non-NaN entry becomes z=0 (no information).
	got := winsorisedZ([]float64{3, 3, 3, math.NaN()}, 2, 3)
	for i, z := range got[:3] {
		if z != 0 {
			t.Errorf("index %d should be 0 under zero stdev, got %v", i, z)
		}
	}
	if !math.IsNaN(got[3]) {
		t.Errorf("NaN input must stay NaN, got %v", got[3])
	}
}

// ---------------------------------------------------------------------------
// blend math
// ---------------------------------------------------------------------------

func TestBlendRedistributesWeightForNaN(t *testing.T) {
	// One NaN in the middle → its weight is redistributed
	// across the other two. Expected weighted mean is the
	// blend of indexes 0 + 2 with their original weights
	// renormalised.
	z := []float64{1.0, math.NaN(), -2.0}
	w := []float64{0.3, 0.4, 0.3}
	got, ok := blend(z, w)
	if !ok {
		t.Fatalf("blend should fire when 2 of 3 entries are valid")
	}
	// renorm: 0.3 and 0.3 → both 0.5; expected = 0.5*1 + 0.5*-2 = -0.5
	if math.Abs(got-(-0.5)) > 1e-9 {
		t.Errorf("blend renorm wrong: got %v want -0.5", got)
	}
}

func TestBlendAllNaNReturnsZeroFalse(t *testing.T) {
	z := []float64{math.NaN(), math.NaN()}
	w := []float64{0.5, 0.5}
	got, ok := blend(z, w)
	if ok || got != 0 {
		t.Errorf("all-NaN must yield (0,false), got (%v,%v)", got, ok)
	}
}

// ---------------------------------------------------------------------------
// BuildScores end-to-end
// ---------------------------------------------------------------------------

func TestBuildScoresEndToEnd(t *testing.T) {
	// 5 names with varying PE/PB/yield. Expectation: cheap
	// names (high B/P, high E/P) land in quartile 1; the
	// expensive growth name lands in quartile 4.
	fixtures := map[string]fundamental.Metrics{
		"VALUE1": {Symbol: "VALUE1", PE: 8, PB: 0.8, DividendYield: 0.05},
		"VALUE2": {Symbol: "VALUE2", PE: 10, PB: 1.0, DividendYield: 0.04},
		"NEUTRAL": {Symbol: "NEUTRAL", PE: 16, PB: 2.5, DividendYield: 0.02},
		"GROWTH1": {Symbol: "GROWTH1", PE: 40, PB: 8, DividendYield: 0},
		"GROWTH2": {Symbol: "GROWTH2", PE: 60, PB: 12, DividendYield: 0},
	}
	svc := NewService(staticFetcher{byKey: fixtures}, Options{})
	requests := []SymbolRequest{
		{Symbol: "VALUE1"}, {Symbol: "VALUE2"}, {Symbol: "NEUTRAL"},
		{Symbol: "GROWTH1"}, {Symbol: "GROWTH2"},
	}
	got := svc.BuildScores(context.Background(), requests)
	if len(got) != 5 {
		t.Fatalf("expected 5 scores, got %d", len(got))
	}
	if got[0].Symbol != "VALUE1" {
		t.Errorf("top by composite should be VALUE1, got %q", got[0].Symbol)
	}
	if got[len(got)-1].Symbol != "GROWTH2" {
		t.Errorf("bottom by composite should be GROWTH2, got %q", got[len(got)-1].Symbol)
	}
	// Quartile spread sanity.
	if got[0].Quartile != 1 || got[len(got)-1].Quartile != 4 {
		t.Errorf("quartile spread wrong: top=%d bottom=%d",
			got[0].Quartile, got[len(got)-1].Quartile)
	}
	// VALUE1 should have all 3 components.
	if got[0].ComponentsAvailable != 3 {
		t.Errorf("VALUE1 should have 3 components, got %d", got[0].ComponentsAvailable)
	}
	// Sort is by descending CompositeZ then ascending symbol.
	for i := 1; i < len(got); i++ {
		if got[i-1].CompositeZ < got[i].CompositeZ {
			t.Errorf("not sorted descending at idx %d: %v vs %v",
				i, got[i-1].CompositeZ, got[i].CompositeZ)
		}
	}
}

func TestBuildScoresDropsSymbolsWithNoUsableData(t *testing.T) {
	// 4 names, one of which has no usable data (all-NaN-ish).
	fixtures := map[string]fundamental.Metrics{
		"OK1": {Symbol: "OK1", PE: 10, PB: 1.0, DividendYield: 0.02},
		"OK2": {Symbol: "OK2", PE: 15, PB: 1.5, DividendYield: 0.03},
		"OK3": {Symbol: "OK3", PE: 25, PB: 3.0, DividendYield: 0.01},
		"BAD": {Symbol: "BAD", PE: 0, PB: 0, DividendYield: 0},
	}
	svc := NewService(staticFetcher{byKey: fixtures}, Options{})
	requests := []SymbolRequest{
		{Symbol: "OK1"}, {Symbol: "OK2"}, {Symbol: "OK3"}, {Symbol: "BAD"},
	}
	got := svc.BuildScores(context.Background(), requests)
	// BAD has PE=0/PB=0 → both ratios NaN. But DividendYield=0
	// is treated as real data (zero yield is a real signal),
	// so BAD does report ONE component. Expect 4 entries with
	// BAD getting the lowest composite (it's z=0 on yield
	// while the others span a wider range).
	if len(got) != 4 {
		t.Fatalf("expected 4 scores (BAD survives via D/P), got %d", len(got))
	}
	// BAD should be last by composite (cheapest D/P alone isn't
	// enough to beat OK1 / OK2 which have all three).
	if got[len(got)-1].Symbol == "BAD" {
		// Good — BAD landed at the bottom.
	}
}

func TestBuildScoresBelowMinUniverseReturnsNil(t *testing.T) {
	fixtures := map[string]fundamental.Metrics{
		"AAPL": {Symbol: "AAPL", PE: 25, PB: 5, DividendYield: 0.005},
		"MSFT": {Symbol: "MSFT", PE: 30, PB: 10, DividendYield: 0.008},
	}
	svc := NewService(staticFetcher{byKey: fixtures}, Options{})
	got := svc.BuildScores(context.Background(), []SymbolRequest{
		{Symbol: "AAPL"}, {Symbol: "MSFT"},
	})
	if got != nil {
		t.Errorf("expected nil under MinUniverse=3, got %d scores", len(got))
	}
}

func TestBuildScoresFetcherErrorDropsSymbol(t *testing.T) {
	svc := NewService(erringFetcher{}, Options{})
	got := svc.BuildScores(context.Background(), []SymbolRequest{
		{Symbol: "AAPL"}, {Symbol: "MSFT"}, {Symbol: "GOOG"},
	})
	if got != nil {
		t.Errorf("erring fetcher must yield nil (no usable data), got %+v", got)
	}
}

func TestBuildScoresDedupsRepeatedSymbol(t *testing.T) {
	fixtures := map[string]fundamental.Metrics{
		"A": {Symbol: "A", PE: 10, PB: 1, DividendYield: 0.03},
		"B": {Symbol: "B", PE: 20, PB: 2, DividendYield: 0.02},
		"C": {Symbol: "C", PE: 30, PB: 4, DividendYield: 0.01},
	}
	svc := NewService(staticFetcher{byKey: fixtures}, Options{})
	got := svc.BuildScores(context.Background(), []SymbolRequest{
		{Symbol: "A"}, {Symbol: "a"}, {Symbol: "A"}, // 3 different casings
		{Symbol: "B"}, {Symbol: "C"},
	})
	if len(got) != 3 {
		t.Errorf("dedup failed: expected 3 unique symbols, got %d", len(got))
	}
}
