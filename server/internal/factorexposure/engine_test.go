package factorexposure

import (
	"math"
	"testing"
	"time"
)

// almostEqual returns whether two floats agree within 1e-9. Used
// to compare weighted sums where standard library float ops
// introduce sub-pico ULP drift.
func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

// findRow returns the PortfolioExposure for the requested factor
// or fails the test. Snapshots always emit rows in AllFactors
// order so this is just a convenience lookup.
func findRow(t *testing.T, snap Snapshot, f Factor) PortfolioExposure {
	t.Helper()
	for _, row := range snap.Exposures {
		if row.Factor == f {
			return row
		}
	}
	t.Fatalf("factor %q not found in snapshot", f)
	return PortfolioExposure{}
}

func TestEngineEmptyHoldings(t *testing.T) {
	e := &Engine{}
	snap := e.Compute("fund-1", nil, nil)
	if snap.FundID != "fund-1" {
		t.Errorf("fund id = %q", snap.FundID)
	}
	if len(snap.Exposures) != len(AllFactors) {
		t.Fatalf("expected %d rows, got %d", len(AllFactors), len(snap.Exposures))
	}
	for _, row := range snap.Exposures {
		if row.NetExposure != 0 || row.GrossExposure != 0 || row.CapitalPct != 0 || row.HoldingCount != 0 {
			t.Errorf("factor %s should be empty: %+v", row.Factor, row)
		}
	}
}

func TestEngineLongOnlySingleHolding(t *testing.T) {
	e := &Engine{Now: func() time.Time { return time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC) }}
	hs := []Holding{
		{InstrumentKey: "US:AAPL", Symbol: "AAPL", MarketValue: 100_000},
	}
	asof := time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC)
	loadings := map[LoadingKey]InstrumentLoading{
		{InstrumentKey: "US:AAPL", Factor: FactorMomentum}: {
			InstrumentKey: "US:AAPL",
			Factor:        FactorMomentum,
			Loading:       1.2,
			AsOf:          asof,
			Source:        LoadingSourceManual,
		},
	}
	snap := e.Compute("fund-1", hs, loadings)
	if snap.NAV != 100_000 {
		t.Errorf("nav = %f", snap.NAV)
	}
	if snap.HoldingsTotal != 1 || snap.HoldingsCovered != 1 {
		t.Errorf("holdings counts = %d / %d", snap.HoldingsTotal, snap.HoldingsCovered)
	}
	row := findRow(t, snap, FactorMomentum)
	// weight = 100k/100k = 1; contrib = 1 * 1.2 = 1.2
	if !almostEqual(row.NetExposure, 1.2) {
		t.Errorf("momentum net = %f, want 1.2", row.NetExposure)
	}
	if !almostEqual(row.GrossExposure, 1.2) {
		t.Errorf("momentum gross = %f, want 1.2", row.GrossExposure)
	}
	if !almostEqual(row.CapitalPct, 1.0) {
		t.Errorf("momentum coverage = %f, want 1.0", row.CapitalPct)
	}
	if row.HoldingCount != 1 {
		t.Errorf("momentum holding count = %d", row.HoldingCount)
	}
	if !row.LoadingsAsOf.Equal(asof) {
		t.Errorf("loadings asof = %v, want %v", row.LoadingsAsOf, asof)
	}
	// Other factors have no loadings; should be empty.
	empty := findRow(t, snap, FactorSize)
	if empty.NetExposure != 0 || empty.HoldingCount != 0 {
		t.Errorf("size row should be empty: %+v", empty)
	}
}

func TestEngineMultiHoldingMultiFactor(t *testing.T) {
	e := &Engine{}
	hs := []Holding{
		{InstrumentKey: "US:AAPL", MarketValue: 60_000},
		{InstrumentKey: "US:MSFT", MarketValue: 40_000},
	}
	asof := time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	loadings := map[LoadingKey]InstrumentLoading{
		{InstrumentKey: "US:AAPL", Factor: FactorMomentum}: {Loading: 1.0, AsOf: asof},
		{InstrumentKey: "US:MSFT", Factor: FactorMomentum}: {Loading: 0.5, AsOf: asof},
		{InstrumentKey: "US:AAPL", Factor: FactorQuality}:  {Loading: 0.8, AsOf: asof},
		// MSFT has no quality loading → quality coverage should be 60% (AAPL only).
	}
	snap := e.Compute("f-multi", hs, loadings)
	if snap.NAV != 100_000 {
		t.Errorf("nav = %f", snap.NAV)
	}
	mom := findRow(t, snap, FactorMomentum)
	// 0.6 * 1.0 + 0.4 * 0.5 = 0.8 net
	if !almostEqual(mom.NetExposure, 0.8) {
		t.Errorf("momentum net = %f", mom.NetExposure)
	}
	// gross = |0.6*1.0| + |0.4*0.5| = 0.8 (all long, all same sign)
	if !almostEqual(mom.GrossExposure, 0.8) {
		t.Errorf("momentum gross = %f", mom.GrossExposure)
	}
	if !almostEqual(mom.CapitalPct, 1.0) {
		t.Errorf("momentum coverage = %f", mom.CapitalPct)
	}
	if mom.HoldingCount != 2 {
		t.Errorf("momentum holding count = %d", mom.HoldingCount)
	}
	qual := findRow(t, snap, FactorQuality)
	// only AAPL contributes: 0.6 * 0.8 = 0.48
	if !almostEqual(qual.NetExposure, 0.48) {
		t.Errorf("quality net = %f", qual.NetExposure)
	}
	if !almostEqual(qual.CapitalPct, 0.6) {
		t.Errorf("quality coverage = %f, want 0.6", qual.CapitalPct)
	}
	if qual.HoldingCount != 1 {
		t.Errorf("quality holding count = %d", qual.HoldingCount)
	}
	if snap.HoldingsCovered != 2 {
		t.Errorf("covered holdings = %d (AAPL contributes to momentum + quality, MSFT only momentum)", snap.HoldingsCovered)
	}
}

func TestEngineLongShortNetVsGross(t *testing.T) {
	// Classic market-neutral pair: long AAPL, short META, both
	// with positive momentum loadings. Net momentum exposure ≈ 0
	// (factor-neutral) but gross > 0 (still exposed to factor
	// volatility).
	e := &Engine{}
	hs := []Holding{
		{InstrumentKey: "US:AAPL", MarketValue: 100_000},
		{InstrumentKey: "US:META", MarketValue: -100_000},
	}
	asof := time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC)
	loadings := map[LoadingKey]InstrumentLoading{
		{InstrumentKey: "US:AAPL", Factor: FactorMomentum}: {Loading: 1.0, AsOf: asof},
		{InstrumentKey: "US:META", Factor: FactorMomentum}: {Loading: 1.0, AsOf: asof},
	}
	snap := e.Compute("f-ls", hs, loadings)
	mom := findRow(t, snap, FactorMomentum)
	// w_AAPL = +100k/200k = 0.5; w_META = -100k/200k = -0.5
	// net = 0.5*1.0 + (-0.5)*1.0 = 0
	if !almostEqual(mom.NetExposure, 0.0) {
		t.Errorf("ls net = %f, want 0", mom.NetExposure)
	}
	// gross = |0.5*1.0| + |-0.5*1.0| = 1.0
	if !almostEqual(mom.GrossExposure, 1.0) {
		t.Errorf("ls gross = %f, want 1.0", mom.GrossExposure)
	}
	if mom.HoldingCount != 2 {
		t.Errorf("ls holding count = %d", mom.HoldingCount)
	}
}

func TestEnginePartialCoverage(t *testing.T) {
	e := &Engine{}
	hs := []Holding{
		{InstrumentKey: "US:A", MarketValue: 50_000},
		{InstrumentKey: "US:B", MarketValue: 50_000},
	}
	asof := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	// Only A has a loading for momentum; B is uncovered.
	loadings := map[LoadingKey]InstrumentLoading{
		{InstrumentKey: "US:A", Factor: FactorMomentum}: {Loading: 1.0, AsOf: asof},
	}
	snap := e.Compute("f-pc", hs, loadings)
	mom := findRow(t, snap, FactorMomentum)
	// 50% of book * 1.0 = 0.5
	if !almostEqual(mom.NetExposure, 0.5) {
		t.Errorf("net = %f", mom.NetExposure)
	}
	if !almostEqual(mom.CapitalPct, 0.5) {
		t.Errorf("coverage = %f, want 0.5 (only A contributes)", mom.CapitalPct)
	}
	if snap.HoldingsCovered != 1 {
		t.Errorf("covered = %d", snap.HoldingsCovered)
	}
}

func TestEngineStaleLoadingsAsOf(t *testing.T) {
	e := &Engine{}
	old := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	mid := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	new_ := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	hs := []Holding{
		{InstrumentKey: "US:A", MarketValue: 100},
	}
	loadings := map[LoadingKey]InstrumentLoading{
		{InstrumentKey: "US:A", Factor: FactorMomentum}: {Loading: 1, AsOf: new_},
		{InstrumentKey: "US:A", Factor: FactorSize}:     {Loading: 1, AsOf: mid},
		{InstrumentKey: "US:A", Factor: FactorValue}:    {Loading: 1, AsOf: old},
	}
	snap := e.Compute("f", hs, loadings)
	// OldestLoadingAsOf is min(newest_per_factor across factors that had data)
	if !snap.OldestLoadingAsOf.Equal(old) {
		t.Errorf("oldest asof = %v, want %v", snap.OldestLoadingAsOf, old)
	}
}

func TestEngineZeroMVHoldings(t *testing.T) {
	e := &Engine{}
	hs := []Holding{
		{InstrumentKey: "US:Z", MarketValue: 0},
	}
	loadings := map[LoadingKey]InstrumentLoading{
		{InstrumentKey: "US:Z", Factor: FactorMomentum}: {Loading: 5},
	}
	snap := e.Compute("f-z", hs, loadings)
	if snap.NAV != 0 {
		t.Errorf("nav = %f, want 0", snap.NAV)
	}
	mom := findRow(t, snap, FactorMomentum)
	if mom.NetExposure != 0 || mom.GrossExposure != 0 {
		t.Errorf("zero-MV holding should not contribute: %+v", mom)
	}
}

func TestEngineDeterministicOrder(t *testing.T) {
	// Same input twice → exactly identical Snapshot.Exposures order.
	e := &Engine{Now: func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) }}
	hs := []Holding{
		{InstrumentKey: "US:A", MarketValue: 100},
		{InstrumentKey: "US:B", MarketValue: 200},
	}
	loadings := map[LoadingKey]InstrumentLoading{
		{InstrumentKey: "US:A", Factor: FactorMomentum}: {Loading: 1.0},
		{InstrumentKey: "US:B", Factor: FactorMomentum}: {Loading: -0.5},
	}
	snap1 := e.Compute("f", hs, loadings)
	snap2 := e.Compute("f", hs, loadings)
	if len(snap1.Exposures) != len(snap2.Exposures) {
		t.Fatalf("length mismatch")
	}
	for i := range snap1.Exposures {
		if snap1.Exposures[i].Factor != snap2.Exposures[i].Factor {
			t.Errorf("position %d: %s != %s", i, snap1.Exposures[i].Factor, snap2.Exposures[i].Factor)
		}
		if !almostEqual(snap1.Exposures[i].NetExposure, snap2.Exposures[i].NetExposure) {
			t.Errorf("position %d net mismatch", i)
		}
	}
}

func TestParseFactor(t *testing.T) {
	for _, tc := range []struct {
		in     string
		want   Factor
		ok     bool
	}{
		{"size", FactorSize, true},
		{"SIZE", FactorSize, true},
		{" momentum ", FactorMomentum, true},
		{"market_beta", FactorMarketBeta, true},
		{"sector", "", false},
		{"", "", false},
	} {
		got, ok := ParseFactor(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseFactor(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
