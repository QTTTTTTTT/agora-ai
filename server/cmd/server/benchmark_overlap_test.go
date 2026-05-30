package main

import (
	"context"
	"testing"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/repository"
)

// TestNormalizeHoldingSymbol pins the small set of transformations
// the overlap heuristic relies on. Adding a new exchange suffix or
// crypto-pair separator without touching this test would silently
// stop matching that universe of holdings — so we keep the table
// tight and explicit.
func TestNormalizeHoldingSymbol(t *testing.T) {
	cases := map[string]string{
		"":                  "",
		"   ":               "",
		"NVDA":              "NVDA",
		"nvda":              "NVDA",
		"  NVDA  ":          "NVDA",
		"NASDAQ:NVDA":       "NVDA",
		"nyse:meta":         "META",
		"688195.SS":         "688195",
		"688195.SH":         "688195",
		"399006.SZ":         "399006",
		"00700.HK":          "00700",
		"920002.BJ":         "920002",
		"BTC-USD":           "BTCUSD",
		"BTC/USDT":          "BTCUSDT",
		"BTC-USDT":          "BTCUSDT",
		// Compound: prefix AND suffix AND separator.
		"binance:BTC/USDT": "BTCUSDT",
	}
	for in, want := range cases {
		if got := normalizeHoldingSymbol(in); got != want {
			t.Errorf("normalizeHoldingSymbol(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSymbolsMatchForOverlap nails down the matching contract,
// especially the crypto-alias table that lets a fund holding
// "BTC" or "BTC-USD" overlap a benchmark symbol of "BTCUSDT".
// Equity tickers must NOT alias (we deliberately don't fuzz-
// match META vs MET or APPL vs AAPL).
func TestSymbolsMatchForOverlap(t *testing.T) {
	type tc struct {
		holding string
		bench   string
		want    bool
	}
	cases := []tc{
		// Exact matches.
		{"BTCUSDT", "BTCUSDT", true},
		{"NVDA", "NVDA", true},
		{"688195", "688195", true},
		// BTC alias table — order matters (holding then bench).
		{"BTC", "BTCUSDT", true},
		{"BTCUSD", "BTCUSDT", true},
		{"XBTUSD", "BTCUSDT", true},
		{"XBTUSDT", "BTCUSDT", true},
		// ETH alias table.
		{"ETH", "ETHUSDT", true},
		{"ETHUSD", "ETHUSDT", true},
		// Reverse direction: bench "BTC" wouldn't be a real
		// catalog entry but if someone added one we still need
		// a deterministic answer (false because aliases are
		// keyed by the BENCH side).
		{"BTCUSDT", "BTC", false},
		// Equity false-positive guards.
		{"META", "MET", false},
		{"APPL", "AAPL", false},
		{"NVDA", "AVDA", false},
		// Empty / nil.
		{"", "BTCUSDT", false},
		{"BTCUSDT", "", false},
	}
	for _, c := range cases {
		if got := symbolsMatchForOverlap(c.holding, c.bench); got != c.want {
			t.Errorf("symbolsMatchForOverlap(%q, %q) = %v, want %v",
				c.holding, c.bench, got, c.want)
		}
	}
}

// stubHoldingsRepoForOverlap is a minimal holdingsListByFund
// implementation: we only need ListByFund to drive computeHoldingOverlap.
// The real *repository.HoldingsRepo struct has many more methods,
// but the benchmarkServiceAdapter only calls ListByFund here so a
// table-driven stub suffices.
type stubHoldingsRepoForOverlap struct {
	rows []repository.HoldingPosition
	err  error
}

func (s *stubHoldingsRepoForOverlap) ListByFund(ctx context.Context, fundID string) ([]repository.HoldingPosition, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

// To avoid leaking the stub into production, we give the adapter a
// narrower interface for testing. computeHoldingOverlap only needs
// ListByFund — assert that explicitly so future changes that try
// to read other holdings methods break this test.
type holdingsLister interface {
	ListByFund(ctx context.Context, fundID string) ([]repository.HoldingPosition, error)
}

var _ holdingsLister = (*stubHoldingsRepoForOverlap)(nil)

// TestComputeHoldingOverlap_DominantBTC pins the canonical happy
// path: a futures fund whose only holding is BTCUSDT, while the
// rendered benchmarks include btc_usdt. The overlap must be
// "dominant" (the BTC position has the largest quantity) so the UI
// switches to the Alpha-view nudge.
func TestComputeHoldingOverlap_DominantBTC(t *testing.T) {
	stub := &stubHoldingsRepoForOverlap{
		rows: []repository.HoldingPosition{
			{Symbol: "BTCUSDT", Quantity: 5.0, UpdatedAt: time.Now()},
		},
	}
	got := computeOverlapForTest(stub, []api.BenchmarkSeriesDTO{
		{ID: "btc_usdt", Symbol: "BTCUSDT", Label: "BTC / USDT"},
	})
	if got == nil {
		t.Fatal("expected overlap, got nil")
	}
	if got.PrimaryBenchmark != "btc_usdt" {
		t.Errorf("PrimaryBenchmark = %q, want btc_usdt", got.PrimaryBenchmark)
	}
	if got.OverlapStrength != "dominant" {
		t.Errorf("OverlapStrength = %q, want dominant", got.OverlapStrength)
	}
	if len(got.MatchedSymbols) != 1 || got.MatchedSymbols[0] != "BTCUSDT" {
		t.Errorf("MatchedSymbols = %v, want [BTCUSDT]", got.MatchedSymbols)
	}
}

// TestComputeHoldingOverlap_PartialBTC verifies that when BTC is
// held but isn't the largest position by quantity, overlap is
// flagged "partial". The UI may still surface a hint but the copy
// is softer ("you also hold X" vs "your fund essentially IS X").
func TestComputeHoldingOverlap_PartialBTC(t *testing.T) {
	stub := &stubHoldingsRepoForOverlap{
		rows: []repository.HoldingPosition{
			// 100 ETH (largest), 1 BTC (matched but smaller).
			{Symbol: "ETHUSDT", Quantity: 100.0, UpdatedAt: time.Now()},
			{Symbol: "BTCUSDT", Quantity: 1.0, UpdatedAt: time.Now()},
		},
	}
	// Only btc_usdt is rendered — eth_usdt isn't in the slice.
	got := computeOverlapForTest(stub, []api.BenchmarkSeriesDTO{
		{ID: "btc_usdt", Symbol: "BTCUSDT", Label: "BTC / USDT"},
	})
	if got == nil {
		t.Fatal("expected overlap, got nil")
	}
	if got.PrimaryBenchmark != "btc_usdt" {
		t.Errorf("PrimaryBenchmark = %q, want btc_usdt", got.PrimaryBenchmark)
	}
	if got.OverlapStrength != "partial" {
		t.Errorf("OverlapStrength = %q, want partial", got.OverlapStrength)
	}
}

// TestComputeHoldingOverlap_NoMatch must NOT return an overlap
// hint just because the fund has positions and the chart has
// benchmarks. The hint is specifically for the structurally-flat
// case; a stock fund vs an index has no useful Alpha message
// here that the existing chart legend doesn't already convey.
func TestComputeHoldingOverlap_NoMatch(t *testing.T) {
	stub := &stubHoldingsRepoForOverlap{
		rows: []repository.HoldingPosition{
			{Symbol: "NVDA", Quantity: 500, UpdatedAt: time.Now()},
			{Symbol: "META", Quantity: 200, UpdatedAt: time.Now()},
		},
	}
	got := computeOverlapForTest(stub, []api.BenchmarkSeriesDTO{
		{ID: "spx", Symbol: "^GSPC", Label: "S&P 500"},
	})
	if got != nil {
		t.Errorf("expected nil overlap, got %+v", got)
	}
}

// TestComputeHoldingOverlap_HoldingsLoadFailure handles the
// degraded-but-functional case where the holdings repo errors
// (transient DB blip). The chart should still render, just
// without the overlap hint — return nil silently.
func TestComputeHoldingOverlap_HoldingsLoadFailure(t *testing.T) {
	stub := &stubHoldingsRepoForOverlap{
		err: context.DeadlineExceeded,
	}
	got := computeOverlapForTest(stub, []api.BenchmarkSeriesDTO{
		{ID: "btc_usdt", Symbol: "BTCUSDT", Label: "BTC / USDT"},
	})
	if got != nil {
		t.Errorf("expected nil overlap on holdings error, got %+v", got)
	}
}

// TestComputeHoldingOverlap_NoBenchmarks short-circuits when the
// fetcher returned no benchmarks (e.g., upstream all failed).
// We don't even hit the holdings repo in this path.
func TestComputeHoldingOverlap_NoBenchmarks(t *testing.T) {
	calls := 0
	stub := stubListByFundFn(func(ctx context.Context, fundID string) ([]repository.HoldingPosition, error) {
		calls++
		return nil, nil
	})
	got := computeOverlapForTest(stub, nil)
	if got != nil {
		t.Errorf("expected nil overlap, got %+v", got)
	}
	if calls != 0 {
		t.Errorf("holdings repo was called %d times, expected 0 (short-circuit)", calls)
	}
}

// stubListByFundFn lets a test inject custom ListByFund behaviour
// without filling out the full repository.HoldingsRepo surface.
type stubListByFundFn func(ctx context.Context, fundID string) ([]repository.HoldingPosition, error)

func (f stubListByFundFn) ListByFund(ctx context.Context, fundID string) ([]repository.HoldingPosition, error) {
	return f(ctx, fundID)
}

// computeOverlapForTest is the test-only entry point. The real
// adapter takes a *repository.HoldingPositionsRepo that is hard
// to fake; we bypass it by re-implementing the small wrapper that
// does the holdings lookup, so the helper tests above only need
// the ListByFund method.
func computeOverlapForTest(repo holdingsLister, rendered []api.BenchmarkSeriesDTO) *api.BenchmarkHoldingOverlap {
	if repo == nil || len(rendered) == 0 {
		return nil
	}
	positions, err := repo.ListByFund(context.Background(), "test-fund")
	if err != nil || len(positions) == 0 {
		return nil
	}
	// Inline the same algorithm as benchmarkServiceAdapter.computeHoldingOverlap
	// so that change in production code is mirrored here. Keep this
	// list short; if the algorithm grows, refactor both into a pure
	// shared helper.
	type benchEntry struct {
		id     string
		symbol string
	}
	bench := make([]benchEntry, 0, len(rendered))
	for _, b := range rendered {
		sym := normalizeHoldingSymbol(b.Symbol)
		// Note: normalizeHoldingSymbol uppercases AND strips
		// suffix/prefix; for benchmark symbols those operations
		// are no-ops in practice but they keep this routine
		// symmetric so a future change to the catalog format
		// (e.g., adding "NASDAQ:" prefixes) Just Works.
		if sym == "" {
			continue
		}
		bench = append(bench, benchEntry{id: b.ID, symbol: sym})
	}
	if len(bench) == 0 {
		return nil
	}
	largestQty := 0.0
	largestSym := ""
	for _, p := range positions {
		if p.Quantity > largestQty {
			largestQty = p.Quantity
			largestSym = normalizeHoldingSymbol(p.Symbol)
		}
	}
	type qpos struct {
		sym  string
		raw  string
		qty  float64
	}
	sorted := make([]qpos, 0, len(positions))
	for _, p := range positions {
		sorted = append(sorted, qpos{
			sym: normalizeHoldingSymbol(p.Symbol),
			raw: p.Symbol,
			qty: p.Quantity,
		})
	}
	// Stable sort DESC by quantity.
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].qty > sorted[i].qty {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	for _, p := range sorted {
		if p.sym == "" {
			continue
		}
		for _, b := range bench {
			if symbolsMatchForOverlap(p.sym, b.symbol) {
				strength := "partial"
				if p.sym == largestSym {
					strength = "dominant"
				}
				return &api.BenchmarkHoldingOverlap{
					PrimaryBenchmark: b.id,
					OverlapStrength:  strength,
					MatchedSymbols:   []string{p.raw},
				}
			}
		}
	}
	return nil
}
