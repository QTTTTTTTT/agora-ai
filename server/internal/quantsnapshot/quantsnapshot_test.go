package quantsnapshot

// Tests pin the Sprint A #1 contract:
//
//   - Snapshot.HasSignal drops the all-zero rows the PM prompt is
//     supposed to omit so we don't bloat the prompt JSON with N
//     no-op entries when OHLC is half-wired.
//   - Options.withDefaults yields the production-quality numbers
//     used by NewBuilder so future tuning happens in one place.
//   - Builder.BuildBatch dedupes (symbol, market), tolerates a nil
//     regime service / nil fetcher / fetch error, and produces a
//     correct ATR + ceiling for a known-good bar series.
//   - The position-size ceiling is bounded by [MinPositionPct,
//     MaxPositionPct]; we hit both ends explicitly so a tuning typo
//     can't silently lift the cap above the prompt's 10% NAV rule.
//
// The bar generators are deterministic so the ATR math is checked
// to the 4th decimal without floating-point drift.

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/fundai/server/internal/ohlc"
	"github.com/fundai/server/internal/regime"
)

// fakeFetcher returns a fixed bar slice per (symbol, market) pair
// and counts invocations so the tests can assert dedup + cache reuse.
type fakeFetcher struct {
	bars  map[string][]ohlc.Bar
	err   map[string]error
	calls map[string]int
}

func newFakeFetcher() *fakeFetcher {
	return &fakeFetcher{
		bars:  map[string][]ohlc.Bar{},
		err:   map[string]error{},
		calls: map[string]int{},
	}
}

func (f *fakeFetcher) key(req ohlc.FetchRequest) string {
	return req.Symbol + "|" + req.Market
}

func (f *fakeFetcher) Fetch(_ context.Context, req ohlc.FetchRequest) ([]ohlc.Bar, error) {
	k := f.key(req)
	f.calls[k]++
	if err, ok := f.err[k]; ok && err != nil {
		return nil, err
	}
	if bars, ok := f.bars[k]; ok {
		return bars, nil
	}
	return nil, ohlc.ErrNoData
}

// genTrendBars returns `n` daily bars in a quiet uptrend with a
// stable +closeStep close-to-close move and a fixed +trueRange high
// minus low. ATR(period) under Wilder's smoothing converges to
// trueRange when every TR matches; useful for asserting exact ATR
// without retrofitting the production formula.
func genTrendBars(n int, startClose, closeStep, trueRange float64) []ohlc.Bar {
	bars := make([]ohlc.Bar, n)
	start := time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		close := startClose + float64(i)*closeStep
		bars[i] = ohlc.Bar{
			Time:   start.AddDate(0, 0, i),
			Open:   close - closeStep/2,
			High:   close + trueRange/2,
			Low:    close - trueRange/2,
			Close:  close,
			Volume: 1_000_000,
		}
	}
	return bars
}

// HasSignal must be false on a bare-Symbol Snapshot — that's the
// signal the wiring layer uses to drop a row from the prompt.
func TestSnapshotHasSignalRejectsBareSymbol(t *testing.T) {
	if (Snapshot{Symbol: "AAPL"}).HasSignal() {
		t.Errorf("bare Symbol must NOT count as signal — the prompt would receive a useless row")
	}
}

// Any one of Regime / ATR / ATRPct / Ceiling flips HasSignal true
// so a partial fetch (regime OK, bars too short for ATR) still
// surfaces the regime tag to the PM.
func TestSnapshotHasSignalAcceptsAnyPopulatedField(t *testing.T) {
	cases := []struct {
		name string
		snap Snapshot
	}{
		{"regime only", Snapshot{Symbol: "X", Regime: "trend_up"}},
		{"atr only", Snapshot{Symbol: "X", ATR14: 1.2}},
		{"atrPct only", Snapshot{Symbol: "X", ATRPct: 2.4}},
		{"ceiling only", Snapshot{Symbol: "X", PositionSizeCeilingPct: 0.05}},
	}
	for _, c := range cases {
		if !c.snap.HasSignal() {
			t.Errorf("%s: HasSignal should be true", c.name)
		}
	}
}

// Options.withDefaults must hand back production-grade numbers
// even on a zero-value Options — that's the contract NewBuilder
// relies on so callers can pass Options{} for "use defaults".
func TestOptionsWithDefaultsProductionNumbers(t *testing.T) {
	got := Options{}.withDefaults()
	want := Options{
		LookbackBars:    60,
		ATRPeriod:       14,
		RiskBudgetPct:   0.005,
		StopATRMultiple: 2.0,
		MinPositionPct:  0.005,
		MaxPositionPct:  0.10,
	}
	if got != want {
		t.Errorf("Options{}.withDefaults() = %+v, want %+v", got, want)
	}
}

// Override one knob, defaults fill the rest.
func TestOptionsWithDefaultsPreservesExplicitFields(t *testing.T) {
	in := Options{ATRPeriod: 21, RiskBudgetPct: 0.01}
	got := in.withDefaults()
	if got.ATRPeriod != 21 || got.RiskBudgetPct != 0.01 {
		t.Errorf("explicit fields not preserved: got %+v", got)
	}
	if got.LookbackBars != 60 || got.MaxPositionPct != 0.10 {
		t.Errorf("defaults not applied to unset fields: got %+v", got)
	}
}

// MaxPositionPct < MinPositionPct is a config typo; the helper
// snaps MaxPositionPct up to MinPositionPct so the final clamp
// produces a degenerate-but-deterministic ceiling instead of
// silently rejecting every snapshot.
func TestOptionsWithDefaultsHealsInvertedClamp(t *testing.T) {
	in := Options{MinPositionPct: 0.02, MaxPositionPct: 0.01}
	got := in.withDefaults()
	if got.MaxPositionPct != got.MinPositionPct {
		t.Errorf("inverted Min/Max should heal to equal: got Min=%v Max=%v", got.MinPositionPct, got.MaxPositionPct)
	}
}

// Nil builder / nil fetcher must produce nil — the wiring layer
// builds the snapshot unconditionally and relies on this to omit
// the prompt block when OHLC isn't wired.
func TestBuildBatchNilSafe(t *testing.T) {
	var b *Builder
	if got := b.BuildBatch(context.Background(), []SymbolRequest{{Symbol: "AAPL"}}); got != nil {
		t.Errorf("nil builder: expected nil, got %+v", got)
	}
	b2 := NewBuilder(nil, nil, Options{})
	if got := b2.BuildBatch(context.Background(), []SymbolRequest{{Symbol: "AAPL"}}); got != nil {
		t.Errorf("nil fetcher: expected nil, got %+v", got)
	}
}

// Happy path: a 30-bar uptrend yields a Snapshot with a positive
// ATR, a finite ATRPct, and a ceiling inside [Min, Max].
func TestBuildBatchProducesSnapshotForCleanBars(t *testing.T) {
	fetcher := newFakeFetcher()
	bars := genTrendBars(60, 100.0, 0.5, 2.0)
	fetcher.bars["AAPL|us_equity"] = bars

	b := NewBuilder(nil, fetcher, Options{})
	out := b.BuildBatch(context.Background(), []SymbolRequest{{Symbol: "AAPL", Market: "us_equity"}})

	if len(out) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(out))
	}
	s := out[0]
	if s.Symbol != "AAPL" {
		t.Errorf("Symbol = %q, want AAPL", s.Symbol)
	}
	if s.ATR14 <= 0 {
		t.Errorf("ATR14 must be positive on a clean 60-bar series, got %v", s.ATR14)
	}
	if s.ATRPct <= 0 {
		t.Errorf("ATRPct must be positive, got %v", s.ATRPct)
	}
	// True range is constant 2.0; Wilder's ATR seeds with SMA of
	// first 14 TRs (the first TR is high-low = 2.0) and then
	// smooths a constant 2.0 stream, so it stays at 2.0 exactly.
	if math.Abs(s.ATR14-2.0) > 1e-9 {
		t.Errorf("ATR14 = %v, want 2.0 (constant TR), diff=%v", s.ATR14, math.Abs(s.ATR14-2.0))
	}
	// With ATR=2.0 and the latest close at startClose+59*0.5 = 129.5,
	// ATRPct = 2 / 129.5 * 100 ≈ 1.5444%.
	wantATRPct := 2.0 / 129.5 * 100.0
	if math.Abs(s.ATRPct-wantATRPct) > 1e-6 {
		t.Errorf("ATRPct = %v, want ≈%v", s.ATRPct, wantATRPct)
	}
	// Ceiling formula: 0.005 / (2 * (2/129.5)) = 0.005 / 0.030888...
	// = 0.16188... → clamped to MaxPositionPct = 0.10.
	if math.Abs(s.PositionSizeCeilingPct-0.10) > 1e-9 {
		t.Errorf("ceiling should clamp to MaxPositionPct=0.10 on this low-vol series, got %v", s.PositionSizeCeilingPct)
	}
}

// High-vol series triggers the lower-bound clamp: ATR equal to
// close (effectively 100% daily TR) drives ceiling well below
// MinPositionPct, and the builder must snap it back up to Min.
func TestBuildBatchClampsCeilingToMinOnExtremeVolatility(t *testing.T) {
	fetcher := newFakeFetcher()
	// trueRange = startClose so ATR ≈ 100, close ≈ 100, ATRPct ≈ 100.
	// Ceiling raw = 0.005 / (2 * 1.0) = 0.0025 → snapped to Min = 0.005.
	bars := genTrendBars(60, 100.0, 0.0, 100.0)
	fetcher.bars["WILD|crypto"] = bars

	b := NewBuilder(nil, fetcher, Options{})
	out := b.BuildBatch(context.Background(), []SymbolRequest{{Symbol: "WILD", Market: "crypto"}})
	if len(out) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(out))
	}
	if math.Abs(out[0].PositionSizeCeilingPct-0.005) > 1e-9 {
		t.Errorf("extreme-vol ceiling should snap to MinPositionPct=0.005, got %v", out[0].PositionSizeCeilingPct)
	}
}

// Dedup: same (symbol, market) twice → one Snapshot, one Fetch
// call. Different markets → distinct entries.
func TestBuildBatchDedupesAndPreservesDistinctMarkets(t *testing.T) {
	fetcher := newFakeFetcher()
	fetcher.bars["AAPL|us_equity"] = genTrendBars(60, 100, 0.5, 2.0)
	fetcher.bars["AAPL|hk_stock"] = genTrendBars(60, 200, 0.3, 4.0)

	b := NewBuilder(nil, fetcher, Options{})
	out := b.BuildBatch(context.Background(), []SymbolRequest{
		{Symbol: "AAPL", Market: "us_equity"},
		{Symbol: "AAPL", Market: "us_equity"}, // dup, dropped
		{Symbol: "aapl", Market: "US_EQUITY"}, // dup after normalisation
		{Symbol: "AAPL", Market: "hk_stock"},  // distinct → kept
	})
	if len(out) != 2 {
		t.Fatalf("expected 2 snapshots after dedup, got %d (%+v)", len(out), out)
	}
	if fetcher.calls["AAPL|us_equity"] != 1 {
		t.Errorf("us_equity should fetch once, got %d", fetcher.calls["AAPL|us_equity"])
	}
	if fetcher.calls["AAPL|hk_stock"] != 1 {
		t.Errorf("hk_stock should fetch once, got %d", fetcher.calls["AAPL|hk_stock"])
	}
}

// Fetch error → Snapshot returns with whatever regime info the
// classifier can supply (here: nothing, because regime svc is nil)
// and no ATR fields. The wiring layer then drops it via HasSignal.
func TestBuildBatchTolerantOfFetchError(t *testing.T) {
	fetcher := newFakeFetcher()
	fetcher.err["XYZ|us_equity"] = errors.New("upstream 500")

	b := NewBuilder(nil, fetcher, Options{})
	out := b.BuildBatch(context.Background(), []SymbolRequest{{Symbol: "XYZ", Market: "us_equity"}})
	if len(out) != 1 {
		t.Fatalf("expected 1 snapshot (empty) even on fetch error, got %d", len(out))
	}
	if out[0].ATR14 != 0 || out[0].ATRPct != 0 || out[0].PositionSizeCeilingPct != 0 {
		t.Errorf("ATR fields should be zero when fetch errors, got %+v", out[0])
	}
	if out[0].HasSignal() {
		t.Errorf("empty Snapshot should NOT signal — the wiring drops it")
	}
}

// Too-short bar history (< ATRPeriod+1) yields a bare Snapshot with
// no ATR. Same drop-via-HasSignal contract.
func TestBuildBatchSkipsATRForShortHistory(t *testing.T) {
	fetcher := newFakeFetcher()
	fetcher.bars["NEW|us_equity"] = genTrendBars(10, 50.0, 0.2, 0.5) // 10 bars < ATR period 14

	b := NewBuilder(nil, fetcher, Options{})
	out := b.BuildBatch(context.Background(), []SymbolRequest{{Symbol: "NEW", Market: "us_equity"}})
	if len(out) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(out))
	}
	if out[0].ATR14 != 0 {
		t.Errorf("ATR14 should be zero on short history, got %v", out[0].ATR14)
	}
}

// Trim + uppercase ensure callers don't accidentally produce two
// entries for "AAPL" and "  aapl ".
func TestBuildBatchNormalisesSymbol(t *testing.T) {
	fetcher := newFakeFetcher()
	fetcher.bars["AAPL|us_equity"] = genTrendBars(30, 100.0, 0.1, 1.0)

	b := NewBuilder(nil, fetcher, Options{})
	out := b.BuildBatch(context.Background(), []SymbolRequest{
		{Symbol: "  aapl  ", Market: "  US_EQUITY  "},
	})
	if len(out) != 1 || out[0].Symbol != "AAPL" {
		t.Fatalf("symbol should be trimmed + uppercased, got %+v", out)
	}
}

// Builder.Options exposes the resolved struct so tests + observers
// can confirm what numbers a given builder is actually using.
func TestBuilderOptionsReturnsResolvedDefaults(t *testing.T) {
	b := NewBuilder(nil, nil, Options{ATRPeriod: 21})
	got := b.Options()
	if got.ATRPeriod != 21 {
		t.Errorf("ATRPeriod override not surfaced: got %+v", got)
	}
	if got.MaxPositionPct != 0.10 {
		t.Errorf("default MaxPositionPct not applied: got %+v", got)
	}
}

// Nil builder Options() returns the defaults so callers can inspect
// the contract without risking a nil dereference.
func TestNilBuilderOptionsReturnsDefaults(t *testing.T) {
	var b *Builder
	got := b.Options()
	if got.ATRPeriod != 14 || got.MaxPositionPct != 0.10 {
		t.Errorf("nil-builder Options should return defaults, got %+v", got)
	}
}

// Compile-time assertion that regime.Service still satisfies the
// regimeSvc field in Builder — if the regime package ever changes
// Classify's signature this test breaks at build time, surfacing
// the silent-regression risk early.
var _ = func(s *regime.Service, fetcher ohlc.Fetcher) *Builder {
	return NewBuilder(s, fetcher, Options{})
}
