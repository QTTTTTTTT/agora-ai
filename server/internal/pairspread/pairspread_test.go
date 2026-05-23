package pairspread

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/fundai/server/internal/ohlc"
)

// stubFetcher is a deterministic ohlc.Fetcher that returns
// pre-seeded bar slices keyed on canonical "MARKET|SYMBOL".
type stubFetcher struct {
	byKey  map[string][]ohlc.Bar
	errors map[string]error
}

func (f stubFetcher) Fetch(_ context.Context, req ohlc.FetchRequest) ([]ohlc.Bar, error) {
	key := keyForSymbol(req.Symbol, req.Market)
	if f.errors != nil {
		if e, ok := f.errors[key]; ok {
			return nil, e
		}
	}
	bars, ok := f.byKey[key]
	if !ok {
		return nil, errors.New("not seeded")
	}
	return bars, nil
}

// barsFromCloses turns a slice of closes into the bar shape the
// fetcher returns. Bar Time is monotonically forward in days so
// the ohlc layer's own sanity checks (if any) pass.
func barsFromCloses(closes []float64) []ohlc.Bar {
	out := make([]ohlc.Bar, len(closes))
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, c := range closes {
		out[i] = ohlc.Bar{
			Time:   start.Add(time.Duration(i) * 24 * time.Hour),
			Open:   c,
			High:   c * 1.001,
			Low:    c * 0.999,
			Close:  c,
			Volume: 1e6,
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Pure math: computePairSpread
// ---------------------------------------------------------------------------

func TestComputePairSpreadIdenticalSeriesYieldsZeroZ(t *testing.T) {
	closes := make([]float64, 70)
	for i := range closes {
		closes[i] = 100 + float64(i)
	}
	ps, ok := computePairSpread(closes, closes, 60)
	if !ok {
		t.Fatal("expected ok")
	}
	if math.Abs(ps.Spread) > 1e-9 {
		t.Errorf("spread = %v, want 0 (identical series)", ps.Spread)
	}
	if math.Abs(ps.SpreadZ) > 1e-9 {
		t.Errorf("z = %v, want 0", ps.SpreadZ)
	}
}

func TestComputePairSpreadDivergenceProducesPositiveZ(t *testing.T) {
	// 60 bars where left == right, then the last bar Left
	// jumps +20% relative to Right → strong positive Z.
	left := make([]float64, 70)
	right := make([]float64, 70)
	for i := range left {
		left[i] = 100 + float64(i)*0.1
		right[i] = 100 + float64(i)*0.1
	}
	left[len(left)-1] = left[len(left)-1] * 1.20 // Left rich
	ps, ok := computePairSpread(left, right, 60)
	if !ok {
		t.Fatal("expected ok")
	}
	if ps.SpreadZ <= 0 {
		t.Errorf("expected positive z (Left rich), got %v", ps.SpreadZ)
	}
	if ps.Spread <= 0 {
		t.Errorf("expected positive spread (log(Left/Right) > 0), got %v", ps.Spread)
	}
}

func TestComputePairSpreadCheapLegProducesNegativeZ(t *testing.T) {
	left := make([]float64, 70)
	right := make([]float64, 70)
	for i := range left {
		left[i] = 100 + float64(i)*0.1
		right[i] = 100 + float64(i)*0.1
	}
	left[len(left)-1] = left[len(left)-1] * 0.80 // Left cheap
	ps, ok := computePairSpread(left, right, 60)
	if !ok {
		t.Fatal("expected ok")
	}
	if ps.SpreadZ >= 0 {
		t.Errorf("expected negative z (Left cheap), got %v", ps.SpreadZ)
	}
}

func TestComputePairSpreadRejectsTooShortSeries(t *testing.T) {
	closes := make([]float64, 30)
	for i := range closes {
		closes[i] = 100 + float64(i)
	}
	if _, ok := computePairSpread(closes, closes, 60); ok {
		t.Error("expected !ok on too-short series")
	}
}

func TestComputePairSpreadRejectsTinyLookback(t *testing.T) {
	closes := make([]float64, 100)
	if _, ok := computePairSpread(closes, closes, 1); ok {
		t.Error("expected !ok on lookback < 2")
	}
}

func TestComputePairSpreadZeroStdevReturnsZeroZ(t *testing.T) {
	// Both series identical and constant → log spread is
	// constant → stdev = 0 → z must be 0 (we DON'T divide).
	closes := make([]float64, 70)
	for i := range closes {
		closes[i] = 100
	}
	ps, ok := computePairSpread(closes, closes, 60)
	if !ok {
		t.Fatal("expected ok")
	}
	if ps.SpreadZ != 0 {
		t.Errorf("expected z=0 on zero-stdev series, got %v", ps.SpreadZ)
	}
}

// ---------------------------------------------------------------------------
// normalisePairs
// ---------------------------------------------------------------------------

func TestNormalisePairsDropsDegenerateAndDuplicates(t *testing.T) {
	got := normalisePairs([]PairRequest{
		{Left: "AAPL", Right: "MSFT", Market: "us_equity", Rho: 0.85},
		{Left: " aapl ", Right: "msft ", Market: "US_EQUITY"}, // dup after normalisation
		{Left: "MSFT", Right: "AAPL", Market: "us_equity", Rho: 0.85}, // dup reversed
		{Left: "GOOG", Right: "GOOG", Market: "us_equity"}, // degenerate
		{Left: "", Right: "AAPL", Market: "us_equity"},     // empty
		{Left: "AAPL", Right: "GOOG", Market: "us_equity", Rho: 0.7},
	})
	if len(got) != 2 {
		t.Fatalf("expected 2 unique pairs, got %d (%+v)", len(got), got)
	}
}

func TestNormalisePairsKeepsLeftRightOrderForOutput(t *testing.T) {
	got := normalisePairs([]PairRequest{
		{Left: "MSFT", Right: "AAPL", Market: "us_equity", Rho: 0.85}, // M comes first; keep as-is
	})
	if len(got) != 1 || got[0].Left != "MSFT" || got[0].Right != "AAPL" {
		t.Errorf("expected MSFT/AAPL preserved, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Service.Build end-to-end
// ---------------------------------------------------------------------------

func TestBuildReturnsNilOnNilFetcher(t *testing.T) {
	svc := NewService(nil, Options{})
	if got := svc.Build(context.Background(), []PairRequest{
		{Left: "A", Right: "B", Market: "us_equity"},
	}); got != nil {
		t.Errorf("expected nil with nil fetcher, got %+v", got)
	}
}

func TestBuildReturnsNilOnEmptyPairs(t *testing.T) {
	svc := NewService(stubFetcher{byKey: map[string][]ohlc.Bar{}}, Options{})
	if got := svc.Build(context.Background(), nil); got != nil {
		t.Errorf("expected nil on no pairs, got %+v", got)
	}
}

func TestBuildComputesPairsAndSortsByAbsZ(t *testing.T) {
	// 3 pairs: one tight (z≈0), one mildly divergent (z≈1.5),
	// one extreme (z≈4). After Build the order must be:
	// extreme first, mild second, tight last.
	tight := make([]float64, 80)
	for i := range tight {
		tight[i] = 100 + float64(i)*0.1
	}
	tightB := make([]float64, 80)
	copy(tightB, tight)

	mildA := make([]float64, 80)
	mildB := make([]float64, 80)
	copy(mildA, tight)
	copy(mildB, tight)
	// Final bar: mildA bumps modestly relative to mildB
	mildA[len(mildA)-1] *= 1.01

	extremeA := make([]float64, 80)
	extremeB := make([]float64, 80)
	copy(extremeA, tight)
	copy(extremeB, tight)
	extremeA[len(extremeA)-1] *= 1.5 // big jump

	bars := map[string][]ohlc.Bar{
		keyForSymbol("TA", "us_equity"): barsFromCloses(tight),
		keyForSymbol("TB", "us_equity"): barsFromCloses(tightB),
		keyForSymbol("MA", "us_equity"): barsFromCloses(mildA),
		keyForSymbol("MB", "us_equity"): barsFromCloses(mildB),
		keyForSymbol("EA", "us_equity"): barsFromCloses(extremeA),
		keyForSymbol("EB", "us_equity"): barsFromCloses(extremeB),
	}
	svc := NewService(stubFetcher{byKey: bars}, Options{LookbackBars: 60, ZThreshold: 2.0})
	snap := svc.Build(context.Background(), []PairRequest{
		{Left: "TA", Right: "TB", Market: "us_equity", Rho: 0.9},
		{Left: "MA", Right: "MB", Market: "us_equity", Rho: 0.95},
		{Left: "EA", Right: "EB", Market: "us_equity", Rho: 0.93},
	})
	if snap == nil {
		t.Fatal("expected snapshot")
	}
	if len(snap.PairsByAbsZ) != 3 {
		t.Fatalf("expected 3 pairs, got %d", len(snap.PairsByAbsZ))
	}
	// First must be extreme, last must be tight.
	if snap.PairsByAbsZ[0].Left != "EA" {
		t.Errorf("expected EA first (highest |z|), got %s (|z|=%v)",
			snap.PairsByAbsZ[0].Left, math.Abs(snap.PairsByAbsZ[0].SpreadZ))
	}
	if snap.PairsByAbsZ[2].Left != "TA" {
		t.Errorf("expected TA last (lowest |z|), got %s (|z|=%v)",
			snap.PairsByAbsZ[2].Left, math.Abs(snap.PairsByAbsZ[2].SpreadZ))
	}
	// HasSignal must fire because |z| of extreme >> 2.0.
	if !snap.HasSignal() {
		t.Error("expected HasSignal true (extreme pair |z| >> 2.0)")
	}
}

func TestBuildSkipsPairsWithMissingData(t *testing.T) {
	closes := make([]float64, 80)
	for i := range closes {
		closes[i] = 100 + float64(i)*0.1
	}
	bars := map[string][]ohlc.Bar{
		keyForSymbol("A", "us_equity"): barsFromCloses(closes),
		// "B" intentionally missing → pair (A,B) drops.
		keyForSymbol("C", "us_equity"): barsFromCloses(closes),
		keyForSymbol("D", "us_equity"): barsFromCloses(closes),
	}
	svc := NewService(stubFetcher{byKey: bars}, Options{LookbackBars: 60})
	snap := svc.Build(context.Background(), []PairRequest{
		{Left: "A", Right: "B", Market: "us_equity", Rho: 0.85}, // dropped
		{Left: "C", Right: "D", Market: "us_equity", Rho: 0.85}, // kept
	})
	if snap == nil {
		t.Fatal("expected snapshot from the surviving pair")
	}
	if len(snap.PairsByAbsZ) != 1 {
		t.Fatalf("expected 1 pair (B missing), got %d", len(snap.PairsByAbsZ))
	}
	if snap.PairsByAbsZ[0].Left != "C" {
		t.Errorf("expected C/D to survive, got %+v", snap.PairsByAbsZ[0])
	}
}

func TestBuildHonoursMaxPairsCap(t *testing.T) {
	closes := make([]float64, 80)
	for i := range closes {
		closes[i] = 100 + float64(i)*0.1
	}
	bars := map[string][]ohlc.Bar{}
	pairs := make([]PairRequest, 0, 5)
	for i, leg := range []string{"A", "B", "C", "D", "E"} {
		for j, other := range []string{"V", "W", "X", "Y", "Z"} {
			if i != j {
				continue
			}
			// Slight tweak so spreads differ.
			a := append([]float64(nil), closes...)
			a[len(a)-1] *= 1 + 0.001*float64(i+1)
			b := append([]float64(nil), closes...)
			bars[keyForSymbol(leg, "us_equity")] = barsFromCloses(a)
			bars[keyForSymbol(other, "us_equity")] = barsFromCloses(b)
			pairs = append(pairs, PairRequest{
				Left: leg, Right: other, Market: "us_equity", Rho: 0.9,
			})
		}
	}
	svc := NewService(stubFetcher{byKey: bars}, Options{LookbackBars: 60, MaxPairs: 2})
	snap := svc.Build(context.Background(), pairs)
	if snap == nil {
		t.Fatal("expected snapshot")
	}
	if len(snap.PairsByAbsZ) != 2 {
		t.Errorf("expected exactly 2 pairs (MaxPairs=2), got %d", len(snap.PairsByAbsZ))
	}
}

func TestSnapshotHasSignalFalseWhenAllBelowThreshold(t *testing.T) {
	closes := make([]float64, 80)
	for i := range closes {
		closes[i] = 100 + float64(i)*0.1
	}
	bars := map[string][]ohlc.Bar{
		keyForSymbol("A", "us_equity"): barsFromCloses(closes),
		keyForSymbol("B", "us_equity"): barsFromCloses(closes),
	}
	svc := NewService(stubFetcher{byKey: bars}, Options{LookbackBars: 60, ZThreshold: 5.0})
	snap := svc.Build(context.Background(), []PairRequest{
		{Left: "A", Right: "B", Market: "us_equity", Rho: 0.95},
	})
	if snap == nil {
		t.Fatal("expected snapshot (pair was computable)")
	}
	if snap.HasSignal() {
		t.Errorf("expected HasSignal false (z below 5σ threshold), got %v", snap.PairsByAbsZ[0].SpreadZ)
	}
}

// ---------------------------------------------------------------------------
// Defaults + Options
// ---------------------------------------------------------------------------

func TestOptionsDefaults(t *testing.T) {
	o := Options{}.withDefaults()
	if o.LookbackBars != 60 || o.ZThreshold != 2.0 || o.MaxPairs != 10 {
		t.Errorf("defaults wrong: %+v", o)
	}
}

func TestOptionsRespectExplicitValues(t *testing.T) {
	o := Options{LookbackBars: 90, ZThreshold: 1.5, MaxPairs: 5}.withDefaults()
	if o.LookbackBars != 90 || o.ZThreshold != 1.5 || o.MaxPairs != 5 {
		t.Errorf("overrides lost: %+v", o)
	}
}

// ---------------------------------------------------------------------------
// formatWindow + intoString
// ---------------------------------------------------------------------------

func TestFormatWindow(t *testing.T) {
	cases := map[int]string{
		1: "1 daily bar", 60: "60 daily bars", 0: "0 daily bars",
	}
	for in, want := range cases {
		if got := formatWindow(in); got != want {
			t.Errorf("formatWindow(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestIntoStringHandlesNegativeAndZero(t *testing.T) {
	cases := map[int]string{0: "0", 1: "1", -7: "-7", 42: "42", -123: "-123"}
	for in, want := range cases {
		if got := intoString(in); got != want {
			t.Errorf("intoString(%d) = %q, want %q", in, got, want)
		}
	}
}
