package correlation

// Sprint C #2 contract tests for the correlation.Service.
//
// The tests cover:
//   - Options.withDefaults installs the production tunings (60d
//     lookback, 0.7 threshold, 10 max pairs, 4 concurrency, 6s
//     timeout) and clamps every degenerate input.
//   - Nil-safe: nil service, nil fetcher, empty / single-symbol
//     request all return nil without panicking.
//   - Dedup normalises (symbol, market) and ORs the Held flag
//     across duplicates so the universe+positions intersection
//     is marked held.
//   - Compute produces deterministic output: high-corr pairs are
//     sorted DESC by |rho|, candidate summaries DESC by max
//     abs corr, and (left, right) pairs are always alphabetic.
//   - Perfectly-correlated synthetic series yield rho=1.0
//     (exercises the pearson formula on known input).
//   - Negatively-correlated series surface with the correct sign
//     in MaxRho while MaxAbsRho stays positive.
//   - Flat (zero-variance) series are dropped — pearson returns
//     (0, false) and the pair is skipped rather than reported as
//     rho=0.
//   - Held-cluster stats only fire with ≥ 2 held symbols.
//   - Per-symbol fetch failures are tolerated — the rest of the
//     universe still surfaces.

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fundai/server/internal/ohlc"
)

type fakeFetcher struct {
	calls    int64
	bySymbol map[string][]ohlc.Bar
	errFor   map[string]error
}

func newFakeFetcher() *fakeFetcher {
	return &fakeFetcher{
		bySymbol: map[string][]ohlc.Bar{},
		errFor:   map[string]error{},
	}
}

func (f *fakeFetcher) Fetch(_ context.Context, req ohlc.FetchRequest) ([]ohlc.Bar, error) {
	atomic.AddInt64(&f.calls, 1)
	key := req.Symbol + "|" + req.Market
	if err, ok := f.errFor[key]; ok {
		return nil, err
	}
	return f.bySymbol[key], nil
}

// genBars builds n daily bars whose close ratio is priceMul each
// step. The series is monotone-up when priceMul > 1, monotone-
// down when < 1, and flat when == 1.
func genBars(n int, startClose, priceMul float64) []ohlc.Bar {
	bars := make([]ohlc.Bar, n)
	t := time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)
	close := startClose
	for i := 0; i < n; i++ {
		bars[i] = ohlc.Bar{
			Time:  t.AddDate(0, 0, i),
			Open:  close,
			High:  close,
			Low:   close,
			Close: close,
		}
		close *= priceMul
	}
	return bars
}

// genBarsRandom produces n daily bars whose returns are seeded
// from the provided slice (length n-1). Lets us pin a specific
// return series for the Pearson tests.
func genBarsFromReturns(rets []float64) []ohlc.Bar {
	n := len(rets) + 1
	bars := make([]ohlc.Bar, n)
	t := time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)
	close := 100.0
	bars[0] = ohlc.Bar{Time: t, Open: close, High: close, Low: close, Close: close}
	for i, r := range rets {
		close *= (1 + r)
		bars[i+1] = ohlc.Bar{Time: t.AddDate(0, 0, i+1), Open: close, High: close, Low: close, Close: close}
	}
	return bars
}

func TestOptionsWithDefaultsProductionTunings(t *testing.T) {
	got := Options{}.withDefaults()
	want := Options{
		LookbackBars:      60,
		HighCorrThreshold: 0.7,
		MaxPairs:          10,
		Concurrency:       4,
		PerCallTimeout:    6 * time.Second,
	}
	if got != want {
		t.Errorf("Options{}.withDefaults() = %+v, want %+v", got, want)
	}
}

func TestOptionsWithDefaultsClampsBounds(t *testing.T) {
	got := Options{
		LookbackBars:      1000,
		HighCorrThreshold: 5.0,
		MaxPairs:          1000,
		Concurrency:       1000,
	}.withDefaults()
	if got.LookbackBars != 252 {
		t.Errorf("LookbackBars ceiling: got %d, want 252", got.LookbackBars)
	}
	if got.HighCorrThreshold != 0.99 {
		t.Errorf("HighCorrThreshold ceiling: got %v, want 0.99", got.HighCorrThreshold)
	}
	if got.MaxPairs != 50 {
		t.Errorf("MaxPairs ceiling: got %d, want 50", got.MaxPairs)
	}
	if got.Concurrency != 16 {
		t.Errorf("Concurrency ceiling: got %d, want 16", got.Concurrency)
	}

	got = Options{LookbackBars: 1, HighCorrThreshold: 0.01, MaxPairs: 0, Concurrency: -1}.withDefaults()
	if got.LookbackBars != 20 {
		t.Errorf("LookbackBars floor: got %d, want 20", got.LookbackBars)
	}
	if got.HighCorrThreshold != 0.3 {
		t.Errorf("HighCorrThreshold floor: got %v, want 0.3", got.HighCorrThreshold)
	}
	if got.MaxPairs != 10 {
		t.Errorf("MaxPairs floor (0 → default 10): got %d, want 10", got.MaxPairs)
	}
	if got.Concurrency != 4 {
		t.Errorf("Concurrency floor (-1 → default 4): got %d, want 4", got.Concurrency)
	}
}

func TestComputeNilSafe(t *testing.T) {
	var s *Service
	if got := s.Compute(context.Background(), []SymbolRequest{{Symbol: "A"}, {Symbol: "B"}}); got != nil {
		t.Errorf("nil receiver: got %+v, want nil", got)
	}
	s2 := NewService(nil, Options{})
	if got := s2.Compute(context.Background(), []SymbolRequest{{Symbol: "A"}, {Symbol: "B"}}); got != nil {
		t.Errorf("nil fetcher: got %+v, want nil", got)
	}
}

func TestComputeSingleSymbolReturnsNil(t *testing.T) {
	f := newFakeFetcher()
	f.bySymbol["A|us_equity"] = genBars(60, 100, 1.01)
	s := NewService(f, Options{})
	if got := s.Compute(context.Background(), []SymbolRequest{{Symbol: "A", Market: "us_equity"}}); got != nil {
		t.Errorf("single symbol: got %+v, want nil", got)
	}
}

// Dedupe normalises and ORs the Held flag.
func TestDedupeRequestsORsHeldFlag(t *testing.T) {
	got := dedupeRequests([]SymbolRequest{
		{Symbol: " aapl ", Market: "US_EQUITY", Held: false},
		{Symbol: "AAPL", Market: "us_equity", Held: true},
		{Symbol: "NVDA"},
		{Symbol: ""},
	})
	if len(got) != 2 {
		t.Fatalf("expected 2 unique requests, got %d (%+v)", len(got), got)
	}
	if got[0].Symbol != "AAPL" || !got[0].Held {
		t.Errorf("AAPL Held should be true after OR merge, got %+v", got[0])
	}
}

// Perfectly correlated series (identical returns): rho = 1.0.
func TestPearsonPerfectPositive(t *testing.T) {
	xs := []float64{0.01, -0.02, 0.005, -0.01, 0.03}
	ys := append([]float64(nil), xs...)
	rho, ok := pearson(xs, ys)
	if !ok {
		t.Fatal("pearson on identical series should be valid")
	}
	if math.Abs(rho-1.0) > 1e-9 {
		t.Errorf("identical series rho = %v, want 1.0", rho)
	}
}

// Perfectly anti-correlated: rho = -1.0.
func TestPearsonPerfectNegative(t *testing.T) {
	xs := []float64{0.01, -0.02, 0.005, -0.01, 0.03}
	ys := make([]float64, len(xs))
	for i, x := range xs {
		ys[i] = -x
	}
	rho, ok := pearson(xs, ys)
	if !ok {
		t.Fatal("pearson on anti-correlated series should be valid")
	}
	if math.Abs(rho-(-1.0)) > 1e-9 {
		t.Errorf("anti-correlated rho = %v, want -1.0", rho)
	}
}

// Flat (zero-variance) series → pearson returns (0, false).
func TestPearsonFlatSeriesRejected(t *testing.T) {
	xs := make([]float64, 30) // all zero
	ys := []float64{0.01, -0.01, 0.02, -0.02, 0.005}
	for len(ys) < len(xs) {
		ys = append(ys, ys[len(ys)%5])
	}
	if _, ok := pearson(xs, ys); ok {
		t.Error("flat xs must be rejected (variance == 0)")
	}
}

// Happy path: 3 symbols, two perfectly correlated, one
// independent. Asserts:
//   - HighCorrPairs surfaces the perfectly-correlated pair
//   - candidate summary on the non-held name picks the heaviest
//     correlation target
//   - HeldCluster fires with two held symbols
func TestComputeSurfacesHighCorrAndCluster(t *testing.T) {
	rets := []float64{0.01, -0.005, 0.02, -0.01, 0.015, -0.008, 0.012, -0.003, 0.025, -0.018,
		0.009, -0.012, 0.014, -0.007, 0.022, -0.015, 0.011, -0.006, 0.019, -0.013,
		0.008, -0.011, 0.017, -0.009, 0.013, -0.014, 0.018, -0.016, 0.021, -0.019,
		0.005, -0.004, 0.016, -0.010, 0.020, -0.017, 0.007, -0.005, 0.024, -0.020,
		0.006, -0.003, 0.015, -0.008, 0.023, -0.018, 0.010, -0.007, 0.026, -0.021,
		0.004, -0.002, 0.013, -0.006, 0.022, -0.019, 0.009, -0.004, 0.027, -0.022,
	}
	f := newFakeFetcher()
	// HELD1 and HELD2 share the same return series → rho = 1.0.
	f.bySymbol["HELD1|us_equity"] = genBarsFromReturns(rets)
	f.bySymbol["HELD2|us_equity"] = genBarsFromReturns(rets)
	// CAND has independent returns (reversed → strongly negatively
	// correlated to HELD1/HELD2).
	cand := make([]float64, len(rets))
	for i, r := range rets {
		cand[i] = -r
	}
	f.bySymbol["CAND|us_equity"] = genBarsFromReturns(cand)

	s := NewService(f, Options{LookbackBars: 30})
	got := s.Compute(context.Background(), []SymbolRequest{
		{Symbol: "HELD1", Market: "us_equity", Held: true},
		{Symbol: "HELD2", Market: "us_equity", Held: true},
		{Symbol: "CAND", Market: "us_equity"},
	})
	if got == nil {
		t.Fatal("expected snapshot, got nil")
	}
	if got.SampleSize != 3 {
		t.Errorf("SampleSize = %d, want 3", got.SampleSize)
	}
	// HighCorrPairs: HELD1<->HELD2 = 1.0; both pairs against CAND
	// are -1.0 (above |0.7| threshold) so we expect 3 entries.
	if len(got.HighCorrPairs) != 3 {
		t.Fatalf("expected 3 high-corr pairs (1 positive, 2 negative), got %d (%+v)", len(got.HighCorrPairs), got.HighCorrPairs)
	}
	// HighCorrPairs are sorted DESC by |rho|; all three have |rho|=1
	// so we only verify (left, right) alphabetic order within the pair.
	for _, p := range got.HighCorrPairs {
		if p.Left > p.Right {
			t.Errorf("pair %+v not alphabetic", p)
		}
	}
	// HeldCluster fires with two held symbols.
	if got.HeldCluster == nil {
		t.Fatal("expected HeldCluster with 2 held symbols")
	}
	if got.HeldCluster.HeldCount != 2 {
		t.Errorf("HeldCount = %d, want 2", got.HeldCluster.HeldCount)
	}
	if math.Abs(got.HeldCluster.AvgPairwise-1.0) > 1e-3 {
		t.Errorf("AvgPairwise = %v, want ~1.0", got.HeldCluster.AvgPairwise)
	}
	// Candidate summary picks any of the two held targets
	// (deterministic for the snapshot: first held wins in code
	// order, both produce |rho|=1).
	if len(got.CandidateSummaries) != 1 {
		t.Fatalf("expected 1 candidate summary, got %d", len(got.CandidateSummaries))
	}
	cs := got.CandidateSummaries[0]
	if cs.Symbol != "CAND" {
		t.Errorf("candidate Symbol = %q, want CAND", cs.Symbol)
	}
	if math.Abs(cs.MaxAbsRho-1.0) > 1e-3 {
		t.Errorf("MaxAbsRho = %v, want ~1.0", cs.MaxAbsRho)
	}
	if cs.MaxRho >= 0 {
		t.Errorf("MaxRho should be negative (CAND anti-correlated), got %v", cs.MaxRho)
	}
}

// Per-symbol fetch failures don't abort the call; the failing
// symbol is dropped.
func TestComputeTolerantOfFetchErrors(t *testing.T) {
	rets := []float64{}
	for i := 0; i < 30; i++ {
		rets = append(rets, float64((i%5)-2)*0.005)
	}
	f := newFakeFetcher()
	f.bySymbol["A|us_equity"] = genBarsFromReturns(rets)
	f.bySymbol["B|us_equity"] = genBarsFromReturns(rets)
	f.errFor["C|us_equity"] = errors.New("rate limited")

	s := NewService(f, Options{LookbackBars: 30})
	got := s.Compute(context.Background(), []SymbolRequest{
		{Symbol: "A", Market: "us_equity", Held: true},
		{Symbol: "B", Market: "us_equity", Held: true},
		{Symbol: "C", Market: "us_equity"},
	})
	if got == nil {
		t.Fatal("expected snapshot from 2 surviving symbols, got nil")
	}
	if got.SampleSize != 2 {
		t.Errorf("SampleSize = %d, want 2 (C errored)", got.SampleSize)
	}
}

// HasSignal semantics: a snapshot with sampleSize >= 2 but no
// pairs / no cluster / no candidates returns false.
func TestHasSignalSemantics(t *testing.T) {
	if (Snapshot{SampleSize: 2}).HasSignal() {
		t.Error("empty snapshot should not signal")
	}
	if !(Snapshot{SampleSize: 2, HighCorrPairs: []HighCorrPair{{Left: "A", Right: "B", Rho: 0.8}}}).HasSignal() {
		t.Error("snapshot with high-corr pair should signal")
	}
}

// round4 handles both positive and negative inputs.
func TestRound4Signed(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{0.123456, 0.1235},
		{-0.123456, -0.1235},
		{0, 0},
		{math.NaN(), 0},
		{math.Inf(1), 0},
	}
	for _, c := range cases {
		got := round4(c.in)
		if got != c.want {
			t.Errorf("round4(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// returnSeries on short / degenerate inputs returns empty.
func TestReturnSeriesEdgeCases(t *testing.T) {
	if got := returnSeries(nil); got != nil {
		t.Errorf("nil bars → got %v, want nil", got)
	}
	if got := returnSeries([]ohlc.Bar{{Close: 100}}); got != nil {
		t.Errorf("single bar → got %v, want nil", got)
	}
	// A zero-close bar contaminates BOTH adjacent returns
	// (zero-divide on the next return, zero-numerator on this
	// one). We want the helper to drop both rather than emit a
	// fake zero — locks the post-filter shape.
	bars := []ohlc.Bar{{Close: 100}, {Close: 0}, {Close: 105}}
	got := returnSeries(bars)
	if len(got) != 0 {
		t.Errorf("zero close should drop both adjacent returns: got %+v", got)
	}
	// Three clean bars produce two clean returns.
	bars = []ohlc.Bar{{Close: 100}, {Close: 110}, {Close: 99}}
	got = returnSeries(bars)
	if len(got) != 2 {
		t.Fatalf("3 bars → 2 returns, got %+v", got)
	}
	if math.Abs(got[0]-0.10) > 1e-9 {
		t.Errorf("got[0] = %v, want 0.10", got[0])
	}
	if math.Abs(got[1]-(-0.10)) > 1e-9 {
		t.Errorf("got[1] = %v, want -0.10", got[1])
	}
}

// intStr handles negatives + zero so windowLabel can't blow up.
func TestIntStr(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{60, "60"},
		{-5, "-5"},
	}
	for _, c := range cases {
		got := intStr(c.in)
		if got != c.want {
			t.Errorf("intStr(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Options accessor on nil receiver.
func TestOptionsAccessorNilSafe(t *testing.T) {
	var s *Service
	if got := s.Options(); got.LookbackBars != 60 {
		t.Errorf("nil Options(): LookbackBars = %d, want 60", got.LookbackBars)
	}
}
