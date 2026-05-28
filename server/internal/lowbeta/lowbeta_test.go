package lowbeta

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/fundai/server/internal/ohlc"
)

// stubFetcher returns pre-seeded bars keyed by upper-cased
// symbol. Bars are constructed by genBars below.
type stubFetcher struct {
	byKey map[string][]ohlc.Bar
}

func (f stubFetcher) Fetch(_ context.Context, req ohlc.FetchRequest) ([]ohlc.Bar, error) {
	bars, ok := f.byKey[req.Symbol]
	if !ok {
		return nil, nil
	}
	return bars, nil
}

// ---------------------------------------------------------------------------
// Options + Service construction
// ---------------------------------------------------------------------------

func TestOptionsDefaultsAppliedWhenZero(t *testing.T) {
	o := Options{}.withDefaults()
	if o.LookbackBars != 60 {
		t.Errorf("LookbackBars default wrong: %d", o.LookbackBars)
	}
	if o.BetaWeight != 0.6 || o.VolatilityWeight != 0.4 {
		t.Errorf("composite weight defaults wrong: %+v", o)
	}
	if o.MinUniverse != 3 {
		t.Errorf("MinUniverse default wrong: %d", o.MinUniverse)
	}
}

func TestMarketIndexForCoversBuiltinMarkets(t *testing.T) {
	o := Options{}.withDefaults()
	cases := map[string]string{
		"us_equity": "SPY",
		"us":        "SPY",
		"a_share":   "510300.SS",
		"cn":        "510300.SS",
		"hk_equity": "2800.HK",
		"hk":        "2800.HK",
		"crypto":    "",
		"unknown":   "",
		"":          "SPY", // empty defaults to US
	}
	for in, want := range cases {
		if got := o.MarketIndexFor(in); got != want {
			t.Errorf("MarketIndexFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMarketIndexForOverrideWins(t *testing.T) {
	// A sector-specialist fund overrides the US benchmark to
	// XLK (tech-only ETF). The override must beat the SPY
	// built-in.
	o := Options{
		MarketIndexBySymbol: map[string]string{
			"us_equity": "XLK",
		},
	}.withDefaults()
	if got := o.MarketIndexFor("us_equity"); got != "XLK" {
		t.Errorf("override lost: %q", got)
	}
	// Unaffected market still uses the built-in default.
	if got := o.MarketIndexFor("a_share"); got != "510300.SS" {
		t.Errorf("non-overridden market got broken: %q", got)
	}
}

func TestNewServiceNilFetcherReturnsNil(t *testing.T) {
	svc := NewService(nil, Options{})
	got := svc.BuildScores(context.Background(), []SymbolRequest{{Symbol: "AAPL"}})
	if got != nil {
		t.Errorf("nil fetcher must yield nil, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Math primitives
// ---------------------------------------------------------------------------

func TestLogReturnsBasic(t *testing.T) {
	in := []float64{100, 110, 121, 121, 109}
	got := logReturns(in)
	want := []float64{
		math.Log(1.10),
		math.Log(1.10),
		math.Log(1),
		math.Log(109.0 / 121.0),
	}
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %d want %d", len(got), len(want))
	}
	for i := range got {
		if math.Abs(got[i]-want[i]) > 1e-9 {
			t.Errorf("index %d: got %v want %v", i, got[i], want[i])
		}
	}
}

func TestRealizedBetaMarketLikeYieldsOne(t *testing.T) {
	// stock returns == market returns → beta = 1.
	rets := []float64{0.01, -0.02, 0.005, 0.015, -0.01, 0.02}
	beta := realizedBeta(rets, rets)
	if math.Abs(beta-1) > 1e-9 {
		t.Errorf("beta on identical series should be 1, got %v", beta)
	}
}

func TestRealizedBetaLeveredScalesLinearly(t *testing.T) {
	// stock returns = 2 × market returns → beta = 2.
	market := []float64{0.01, -0.02, 0.005, 0.015, -0.01}
	stock := make([]float64, len(market))
	for i, r := range market {
		stock[i] = 2 * r
	}
	beta := realizedBeta(stock, market)
	if math.Abs(beta-2) > 1e-9 {
		t.Errorf("expected beta=2 for 2× scaled returns, got %v", beta)
	}
}

func TestRealizedBetaZeroVarianceReturnsNaN(t *testing.T) {
	// Flat market → can't compute beta.
	flat := []float64{0, 0, 0, 0}
	stock := []float64{0.01, -0.01, 0.005, -0.005}
	got := realizedBeta(stock, flat)
	if !math.IsNaN(got) {
		t.Errorf("flat market should yield NaN beta, got %v", got)
	}
}

func TestRealizedStdevKnown(t *testing.T) {
	// stdev of [1,2,3,4,5] is sqrt(2.5).
	got := realizedStdev([]float64{1, 2, 3, 4, 5})
	want := math.Sqrt(2.5)
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestNegatedZFlipsSign(t *testing.T) {
	// Symmetric input around 5; the highest raw should land at
	// the most NEGATIVE z (lowest defensive score) and the
	// lowest raw at the most POSITIVE z.
	in := []float64{1, 5, 9}
	got := negatedZ(in, 3)
	if got[0] < 0 || got[2] > 0 {
		t.Errorf("negated z signs wrong: %v", got)
	}
	if math.Abs(got[1]) > 1e-9 {
		t.Errorf("center value should map to z=0, got %v", got[1])
	}
}

func TestNegatedZBelowMinSampleReturnsAllNaN(t *testing.T) {
	got := negatedZ([]float64{1, math.NaN(), 2}, 5)
	for i, v := range got {
		if !math.IsNaN(v) {
			t.Errorf("idx %d: expected NaN under min-sample floor, got %v", i, v)
		}
	}
}

// ---------------------------------------------------------------------------
// BuildScores end-to-end
// ---------------------------------------------------------------------------

func TestBuildScoresEndToEnd(t *testing.T) {
	// 4 names with different beta / vol profiles + SPY as
	// market index. Expectation:
	//   - DEFENSIVE: low beta + low vol → Q1
	//   - AGGRESSIVE: high beta + high vol → Q4
	market := genBars("SPY", 100, 0.005, 0, 80)
	defensive := genScaledFrom(market, 0.3) // beta ~ 0.3
	neutral := genScaledFrom(market, 1.0)   // beta ~ 1.0
	aggressive := genScaledFrom(market, 1.8) // beta ~ 1.8
	mixed := genScaledFrom(market, 2.5)      // beta ~ 2.5

	bars := map[string][]ohlc.Bar{
		"SPY":        market,
		"DEFENSIVE":  toBars("DEFENSIVE", defensive),
		"NEUTRAL":    toBars("NEUTRAL", neutral),
		"AGGRESSIVE": toBars("AGGRESSIVE", aggressive),
		"MIXED":      toBars("MIXED", mixed),
	}
	svc := NewService(stubFetcher{byKey: bars}, Options{})
	got := svc.BuildScores(context.Background(), []SymbolRequest{
		{Symbol: "DEFENSIVE", Market: "us_equity"},
		{Symbol: "NEUTRAL", Market: "us_equity"},
		{Symbol: "AGGRESSIVE", Market: "us_equity"},
		{Symbol: "MIXED", Market: "us_equity"},
	})
	if len(got) != 4 {
		t.Fatalf("expected 4 scores, got %d", len(got))
	}
	// DEFENSIVE should land at Q1 (top defensive); MIXED at Q4.
	if got[0].Symbol != "DEFENSIVE" {
		t.Errorf("most defensive should be DEFENSIVE, got %q", got[0].Symbol)
	}
	if got[len(got)-1].Symbol != "MIXED" {
		t.Errorf("most aggressive should be MIXED, got %q", got[len(got)-1].Symbol)
	}
	if got[0].Quartile != 1 || got[len(got)-1].Quartile != 4 {
		t.Errorf("quartile spread wrong: top=%d bottom=%d",
			got[0].Quartile, got[len(got)-1].Quartile)
	}
	// Defensive should report Beta < 1; aggressive > 1.
	if got[0].Beta > 0.7 {
		t.Errorf("defensive Beta should be low (~0.3), got %v", got[0].Beta)
	}
	if got[len(got)-1].Beta < 1.5 {
		t.Errorf("aggressive Beta should be high (~2.5), got %v", got[len(got)-1].Beta)
	}
	// All four had market data → ComponentsAvailable=2 each.
	for _, s := range got {
		if s.ComponentsAvailable != 2 {
			t.Errorf("%s expected 2 components, got %d", s.Symbol, s.ComponentsAvailable)
		}
	}
}

func TestBuildScoresFallsBackToVolWhenMarketIndexMissing(t *testing.T) {
	// No SPY bars → beta is NaN for every symbol. The composite
	// should still fire on volatility alone.
	market := genBars("ignored", 100, 0.005, 0, 80)
	a := genScaledFrom(market, 0.3)
	b := genScaledFrom(market, 1.0)
	c := genScaledFrom(market, 2.0)
	bars := map[string][]ohlc.Bar{
		"A": toBars("A", a),
		"B": toBars("B", b),
		"C": toBars("C", c),
	}
	svc := NewService(stubFetcher{byKey: bars}, Options{})
	got := svc.BuildScores(context.Background(), []SymbolRequest{
		{Symbol: "A", Market: "us_equity"},
		{Symbol: "B", Market: "us_equity"},
		{Symbol: "C", Market: "us_equity"},
	})
	if len(got) != 3 {
		t.Fatalf("expected 3 vol-only scores, got %d", len(got))
	}
	for _, s := range got {
		if s.ComponentsAvailable != 1 {
			t.Errorf("%s should have 1 component (vol only), got %d",
				s.Symbol, s.ComponentsAvailable)
		}
		if s.BetaZ != 0 {
			t.Errorf("%s BetaZ should be 0 (NaN→0), got %v", s.Symbol, s.BetaZ)
		}
	}
}

func TestBuildScoresBelowMinUniverseReturnsNil(t *testing.T) {
	market := genBars("SPY", 100, 0.005, 0, 80)
	bars := map[string][]ohlc.Bar{
		"SPY":  market,
		"AAPL": toBars("AAPL", genScaledFrom(market, 1)),
		"MSFT": toBars("MSFT", genScaledFrom(market, 1.2)),
	}
	svc := NewService(stubFetcher{byKey: bars}, Options{})
	got := svc.BuildScores(context.Background(), []SymbolRequest{
		{Symbol: "AAPL", Market: "us_equity"},
		{Symbol: "MSFT", Market: "us_equity"},
	})
	if got != nil {
		t.Errorf("expected nil under MinUniverse=3, got %+v", got)
	}
}

func TestBuildScoresDropsSymbolsWithInsufficientBars(t *testing.T) {
	market := genBars("SPY", 100, 0.005, 0, 80)
	bars := map[string][]ohlc.Bar{
		"SPY":   market,
		"OK1":   toBars("OK1", genScaledFrom(market, 0.5)),
		"OK2":   toBars("OK2", genScaledFrom(market, 1.0)),
		"OK3":   toBars("OK3", genScaledFrom(market, 1.5)),
		// SHORT only has 1 bar — must be dropped (need >= 2 for return).
		"SHORT": {{Time: time.Now(), Close: 100}},
	}
	svc := NewService(stubFetcher{byKey: bars}, Options{})
	got := svc.BuildScores(context.Background(), []SymbolRequest{
		{Symbol: "OK1", Market: "us_equity"},
		{Symbol: "OK2", Market: "us_equity"},
		{Symbol: "OK3", Market: "us_equity"},
		{Symbol: "SHORT", Market: "us_equity"},
	})
	if len(got) != 3 {
		t.Errorf("expected 3 scores (SHORT dropped), got %d", len(got))
	}
}

func TestBuildScoresDedupsRepeatedRequest(t *testing.T) {
	market := genBars("SPY", 100, 0.005, 0, 80)
	bars := map[string][]ohlc.Bar{
		"SPY": market,
		"A":   toBars("A", genScaledFrom(market, 0.5)),
		"B":   toBars("B", genScaledFrom(market, 1.0)),
		"C":   toBars("C", genScaledFrom(market, 1.5)),
	}
	svc := NewService(stubFetcher{byKey: bars}, Options{})
	got := svc.BuildScores(context.Background(), []SymbolRequest{
		{Symbol: "A", Market: "us_equity"},
		{Symbol: "a", Market: "us_equity"}, // case dup
		{Symbol: "B", Market: "us_equity"},
		{Symbol: "C", Market: "us_equity"},
	})
	if len(got) != 3 {
		t.Errorf("expected 3 unique scores, got %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// genBars builds a synthetic series with a given drift + vol.
// Deterministic given the same seed; we use a tiny LCG so tests
// stay self-contained (no math/rand global state needed).
func genBars(symbol string, start, drift float64, seed int64, n int) []ohlc.Bar {
	out := make([]ohlc.Bar, n)
	price := start
	state := uint64(seed*2862933555777941757 + 3037000493)
	for i := 0; i < n; i++ {
		state = state*6364136223846793005 + 1442695040888963407
		// Map state into a roughly N(0,1) noise via the Box-Muller
		// equivalent trick: take two uniforms and combine.
		u1 := float64((state>>11)&0xFFFFFF) / float64(1<<24)
		state = state*6364136223846793005 + 1442695040888963407
		u2 := float64((state>>11)&0xFFFFFF) / float64(1<<24)
		// Clamp u1 away from zero for the log.
		if u1 < 1e-9 {
			u1 = 1e-9
		}
		z := math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
		shock := drift * z
		price = price * math.Exp(shock)
		out[i] = ohlc.Bar{
			Time:  time.Unix(int64(i*86400), 0).UTC(),
			Close: price,
		}
	}
	_ = symbol
	return out
}

// genScaledFrom rebuilds a series whose per-bar returns are
// `scale` × the source series' per-bar returns — gives an
// (approximately) `scale`-beta synthetic stock vs the source
// treated as the market.
func genScaledFrom(market []ohlc.Bar, scale float64) []ohlc.Bar {
	if len(market) == 0 {
		return nil
	}
	out := make([]ohlc.Bar, len(market))
	price := 100.0
	out[0] = ohlc.Bar{Time: market[0].Time, Close: price}
	for i := 1; i < len(market); i++ {
		if market[i-1].Close <= 0 || market[i].Close <= 0 {
			out[i] = ohlc.Bar{Time: market[i].Time, Close: price}
			continue
		}
		mRet := math.Log(market[i].Close / market[i-1].Close)
		price = price * math.Exp(scale*mRet)
		out[i] = ohlc.Bar{Time: market[i].Time, Close: price}
	}
	return out
}

// toBars returns a deep copy of the bars under a fresh symbol
// (the stub fetcher keys on symbol, not embedded in Bar).
func toBars(_ string, in []ohlc.Bar) []ohlc.Bar {
	out := make([]ohlc.Bar, len(in))
	copy(out, in)
	return out
}
