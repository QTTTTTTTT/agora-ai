package quality

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/fundai/server/internal/fundamental"
)

// staticFetcher is a fundamental.Fetcher that returns
// pre-seeded metrics keyed on uppercase symbol.
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
	if o.ROEWeight != 0.5 || o.OperatingMarginWeight != 0.3 || o.ProfitMarginWeight != 0.2 {
		t.Errorf("profitability defaults wrong: %+v", o)
	}
	if o.EarningsGrowthWeight != 0.6 || o.RevenueGrowthWeight != 0.4 {
		t.Errorf("growth defaults wrong: %+v", o)
	}
	if o.ProfitabilityWeight != 0.4 || o.GrowthWeight != 0.3 || o.SafetyWeight != 0.3 {
		t.Errorf("composite defaults wrong: %+v", o)
	}
	if o.MinUniverse != 3 || o.PerFactorMin != 3 {
		t.Errorf("min floors wrong: %+v", o)
	}
}

func TestOptionsPartialOverrideKeepsOtherDefaults(t *testing.T) {
	o := Options{ROEWeight: 1.0}.withDefaults()
	// Override one weight inside the profitability triplet → the
	// other two SHOULD still be 0 (no defaults), because the
	// triplet was explicitly opted into.
	if o.ROEWeight != 1.0 {
		t.Errorf("ROEWeight override lost: %v", o.ROEWeight)
	}
	if o.OperatingMarginWeight != 0 || o.ProfitMarginWeight != 0 {
		t.Errorf("partial override should not reset sibling weights: %+v", o)
	}
	// Other groups should still be defaulted.
	if o.EarningsGrowthWeight == 0 || o.GrowthWeight == 0 {
		t.Errorf("orthogonal groups should be defaulted: %+v", o)
	}
}

func TestNewServiceTreatsNilFetcherAsNoSignal(t *testing.T) {
	svc := NewService(nil, Options{})
	got := svc.BuildScores(context.Background(), []SymbolRequest{{Symbol: "AAPL"}})
	if got != nil {
		t.Errorf("expected nil scores with nil fetcher, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// crossSectionalZ math
// ---------------------------------------------------------------------------

func TestCrossSectionalZBasicCase(t *testing.T) {
	got := crossSectionalZ([]float64{10, 20, 30}, 3)
	// mean = 20, sample stdev = 10 → z = (-1, 0, +1)
	want := []float64{-1, 0, 1}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-9 {
			t.Errorf("[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestCrossSectionalZSkipsNaNValues(t *testing.T) {
	got := crossSectionalZ([]float64{10, math.NaN(), 30, 20}, 3)
	// mean of {10, 20, 30} = 20, stdev = 10
	// NaN propagates: position 1 stays NaN.
	if !math.IsNaN(got[1]) {
		t.Errorf("expected NaN at index 1, got %v", got[1])
	}
	if math.Abs(got[0]-(-1.0)) > 1e-9 {
		t.Errorf("z[0] = %v, want -1", got[0])
	}
	if math.Abs(got[3]-0.0) > 1e-9 {
		t.Errorf("z[3] = %v, want 0", got[3])
	}
	if math.Abs(got[2]-1.0) > 1e-9 {
		t.Errorf("z[2] = %v, want +1", got[2])
	}
}

func TestCrossSectionalZSubMinSamplesAllNaN(t *testing.T) {
	got := crossSectionalZ([]float64{10, 20}, 3)
	for i, g := range got {
		if !math.IsNaN(g) {
			t.Errorf("[%d] = %v, want NaN (sub-min sample)", i, g)
		}
	}
}

func TestCrossSectionalZZeroStdevReturnsZeros(t *testing.T) {
	got := crossSectionalZ([]float64{5, 5, 5, 5}, 3)
	for i, g := range got {
		if g != 0 {
			t.Errorf("[%d] = %v, want 0 (zero stdev)", i, g)
		}
	}
}

// ---------------------------------------------------------------------------
// blend (weight redistribution)
// ---------------------------------------------------------------------------

func TestBlendWeightedAverage(t *testing.T) {
	got, ok := blend([]float64{1, 2, 3}, []float64{0.5, 0.3, 0.2})
	if !ok {
		t.Fatal("expected blend to fire")
	}
	want := 1*0.5 + 2*0.3 + 3*0.2 // 1.7
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("blend = %v, want %v", got, want)
	}
}

func TestBlendNaNRedistributesWeight(t *testing.T) {
	got, ok := blend([]float64{1, math.NaN(), 3}, []float64{0.5, 0.3, 0.2})
	if !ok {
		t.Fatal("expected blend to fire")
	}
	// Weight on NaN is dropped; total weight = 0.7; numerator = 0.5 + 0.6 = 1.1.
	want := (0.5*1 + 0.2*3) / 0.7
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("blend = %v, want %v", got, want)
	}
}

func TestBlendAllNaNReturnsFalse(t *testing.T) {
	_, ok := blend([]float64{math.NaN(), math.NaN()}, []float64{0.5, 0.5})
	if ok {
		t.Error("blend should return ok=false when every entry is NaN")
	}
}

func TestBlendIgnoresNegativeWeights(t *testing.T) {
	got, ok := blend([]float64{1, 2}, []float64{0.5, -0.3})
	if !ok {
		t.Fatal("expected blend to fire on the surviving positive weight")
	}
	// Only the first entry contributes (negative weight is ignored).
	if math.Abs(got-1.0) > 1e-9 {
		t.Errorf("blend = %v, want 1.0", got)
	}
}

// ---------------------------------------------------------------------------
// BuildScores end-to-end
// ---------------------------------------------------------------------------

func TestBuildScoresEmptyUniverseReturnsNil(t *testing.T) {
	svc := NewService(staticFetcher{byKey: map[string]fundamental.Metrics{}}, Options{})
	got := svc.BuildScores(context.Background(), []SymbolRequest{
		{Symbol: "AAPL"}, {Symbol: "MSFT"},
	})
	if got != nil {
		t.Errorf("expected nil on empty universe, got %+v", got)
	}
}

func TestBuildScoresSubMinUniverseReturnsNil(t *testing.T) {
	// Min default = 3; we feed 2 symbols → nil.
	svc := NewService(staticFetcher{byKey: map[string]fundamental.Metrics{
		"AAPL": {Symbol: "AAPL", ReturnOnEquity: 0.4},
		"MSFT": {Symbol: "MSFT", ReturnOnEquity: 0.3},
	}}, Options{})
	got := svc.BuildScores(context.Background(), []SymbolRequest{
		{Symbol: "AAPL"}, {Symbol: "MSFT"},
	})
	if got != nil {
		t.Errorf("expected nil below MinUniverse, got %+v", got)
	}
}

func TestBuildScoresProducesDescendingCompositeOrder(t *testing.T) {
	// Best quality (BEST) vs flat (MID) vs poor (WORST). All three
	// sub-factors aligned so the descending order is unambiguous.
	svc := NewService(staticFetcher{byKey: map[string]fundamental.Metrics{
		"BEST":  {Symbol: "BEST", ReturnOnEquity: 0.5, OperatingMargin: 0.4, ProfitMargin: 0.3, EarningsGrowth: 0.4, RevenueGrowth: 0.3, DebtToEquity: 0.1},
		"MID":   {Symbol: "MID", ReturnOnEquity: 0.3, OperatingMargin: 0.2, ProfitMargin: 0.15, EarningsGrowth: 0.15, RevenueGrowth: 0.12, DebtToEquity: 0.5},
		"WORST": {Symbol: "WORST", ReturnOnEquity: 0.05, OperatingMargin: 0.04, ProfitMargin: 0.02, EarningsGrowth: -0.1, RevenueGrowth: -0.05, DebtToEquity: 2.0},
	}}, Options{})
	got := svc.BuildScores(context.Background(), []SymbolRequest{
		{Symbol: "BEST"}, {Symbol: "MID"}, {Symbol: "WORST"},
	})
	if len(got) != 3 {
		t.Fatalf("expected 3 scores, got %d", len(got))
	}
	if got[0].Symbol != "BEST" {
		t.Errorf("expected BEST first, got %s (composite=%.3f)", got[0].Symbol, got[0].CompositeZ)
	}
	if got[2].Symbol != "WORST" {
		t.Errorf("expected WORST last, got %s (composite=%.3f)", got[2].Symbol, got[2].CompositeZ)
	}
	if got[0].CompositeZ <= got[1].CompositeZ || got[1].CompositeZ <= got[2].CompositeZ {
		t.Errorf("expected strict descending composite, got %.3f %.3f %.3f",
			got[0].CompositeZ, got[1].CompositeZ, got[2].CompositeZ)
	}
}

func TestBuildScoresSafetyInvertedFromDebt(t *testing.T) {
	// Same profitability + growth; LOW debt should win on safety.
	svc := NewService(staticFetcher{byKey: map[string]fundamental.Metrics{
		"LOWDEBT":  {Symbol: "LOWDEBT", ReturnOnEquity: 0.2, OperatingMargin: 0.2, ProfitMargin: 0.1, DebtToEquity: 0.2},
		"MIDDEBT":  {Symbol: "MIDDEBT", ReturnOnEquity: 0.2, OperatingMargin: 0.2, ProfitMargin: 0.1, DebtToEquity: 1.0},
		"HIGHDEBT": {Symbol: "HIGHDEBT", ReturnOnEquity: 0.2, OperatingMargin: 0.2, ProfitMargin: 0.1, DebtToEquity: 3.0},
	}}, Options{})
	got := svc.BuildScores(context.Background(), []SymbolRequest{
		{Symbol: "LOWDEBT"}, {Symbol: "MIDDEBT"}, {Symbol: "HIGHDEBT"},
	})
	if len(got) != 3 {
		t.Fatalf("expected 3 scores, got %d", len(got))
	}
	// LOWDEBT must have HIGHER SafetyZ than HIGHDEBT.
	var low, high float64
	for _, s := range got {
		if s.Symbol == "LOWDEBT" {
			low = s.SafetyZ
		}
		if s.Symbol == "HIGHDEBT" {
			high = s.SafetyZ
		}
	}
	if !(low > high) {
		t.Errorf("LOWDEBT safety (%v) should exceed HIGHDEBT safety (%v) after the negation", low, high)
	}
}

func TestBuildScoresDropsSymbolWithNoData(t *testing.T) {
	// EMPTY has every metric at zero (treated as "not reported").
	svc := NewService(staticFetcher{byKey: map[string]fundamental.Metrics{
		"AAPL":  {Symbol: "AAPL", ReturnOnEquity: 0.4, OperatingMargin: 0.3, ProfitMargin: 0.2},
		"MSFT":  {Symbol: "MSFT", ReturnOnEquity: 0.35, OperatingMargin: 0.28, ProfitMargin: 0.22},
		"GOOG":  {Symbol: "GOOG", ReturnOnEquity: 0.25, OperatingMargin: 0.20, ProfitMargin: 0.18},
		"EMPTY": {Symbol: "EMPTY"},
	}}, Options{})
	got := svc.BuildScores(context.Background(), []SymbolRequest{
		{Symbol: "AAPL"}, {Symbol: "MSFT"}, {Symbol: "GOOG"}, {Symbol: "EMPTY"},
	})
	if len(got) != 3 {
		t.Fatalf("expected 3 scores (EMPTY dropped), got %d", len(got))
	}
	for _, s := range got {
		if s.Symbol == "EMPTY" {
			t.Errorf("EMPTY symbol must be dropped, got %+v", s)
		}
	}
}

func TestBuildScoresPartialDataStillProducesScore(t *testing.T) {
	// AAPL has only profitability; MSFT only growth; GOOG only safety.
	// All three should appear with ComponentsAvailable = 1.
	svc := NewService(staticFetcher{byKey: map[string]fundamental.Metrics{
		"AAPL": {Symbol: "AAPL", ReturnOnEquity: 0.4, OperatingMargin: 0.3, ProfitMargin: 0.2},
		"MSFT": {Symbol: "MSFT", EarningsGrowth: 0.2, RevenueGrowth: 0.15},
		"GOOG": {Symbol: "GOOG", DebtToEquity: 0.3},
		// 3 more so the per-factor z-score has a comparison group.
		"AMZN": {Symbol: "AMZN", ReturnOnEquity: 0.2, EarningsGrowth: 0.1, DebtToEquity: 0.5},
		"META": {Symbol: "META", ReturnOnEquity: 0.3, EarningsGrowth: 0.05, DebtToEquity: 0.4},
		"NVDA": {Symbol: "NVDA", ReturnOnEquity: 0.5, EarningsGrowth: 0.3, DebtToEquity: 0.2},
	}}, Options{})
	got := svc.BuildScores(context.Background(), []SymbolRequest{
		{Symbol: "AAPL"}, {Symbol: "MSFT"}, {Symbol: "GOOG"},
		{Symbol: "AMZN"}, {Symbol: "META"}, {Symbol: "NVDA"},
	})
	if len(got) != 6 {
		t.Fatalf("expected 6 scores, got %d", len(got))
	}
	byName := map[string]Score{}
	for _, s := range got {
		byName[s.Symbol] = s
	}
	if byName["AAPL"].ComponentsAvailable != 1 {
		t.Errorf("AAPL components = %d, want 1", byName["AAPL"].ComponentsAvailable)
	}
	if byName["MSFT"].ComponentsAvailable != 1 {
		t.Errorf("MSFT components = %d, want 1", byName["MSFT"].ComponentsAvailable)
	}
	if byName["GOOG"].ComponentsAvailable != 1 {
		t.Errorf("GOOG components = %d, want 1", byName["GOOG"].ComponentsAvailable)
	}
	if byName["NVDA"].ComponentsAvailable != 3 {
		t.Errorf("NVDA components = %d, want 3", byName["NVDA"].ComponentsAvailable)
	}
}

func TestBuildScoresQuartileBucketing(t *testing.T) {
	// 8 symbols → 4 quartiles × 2 each.
	metrics := map[string]fundamental.Metrics{}
	for i, sym := range []string{"S1", "S2", "S3", "S4", "S5", "S6", "S7", "S8"} {
		// Vary ROE strongly so composite ordering is deterministic.
		metrics[sym] = fundamental.Metrics{
			Symbol:         sym,
			ReturnOnEquity: 0.5 - 0.05*float64(i),
			OperatingMargin: 0.4 - 0.04*float64(i),
		}
	}
	svc := NewService(staticFetcher{byKey: metrics}, Options{})
	got := svc.BuildScores(context.Background(), []SymbolRequest{
		{Symbol: "S1"}, {Symbol: "S2"}, {Symbol: "S3"}, {Symbol: "S4"},
		{Symbol: "S5"}, {Symbol: "S6"}, {Symbol: "S7"}, {Symbol: "S8"},
	})
	if len(got) != 8 {
		t.Fatalf("expected 8 scores, got %d", len(got))
	}
	// Top symbol must be Q1, bottom must be Q4.
	if got[0].Quartile != 1 {
		t.Errorf("top symbol quartile = %d, want 1", got[0].Quartile)
	}
	if got[len(got)-1].Quartile != 4 {
		t.Errorf("bottom symbol quartile = %d, want 4", got[len(got)-1].Quartile)
	}
}

func TestBuildScoresSmallUniverseNoQuartile(t *testing.T) {
	svc := NewService(staticFetcher{byKey: map[string]fundamental.Metrics{
		"A": {Symbol: "A", ReturnOnEquity: 0.5},
		"B": {Symbol: "B", ReturnOnEquity: 0.3},
		"C": {Symbol: "C", ReturnOnEquity: 0.1},
	}}, Options{})
	got := svc.BuildScores(context.Background(), []SymbolRequest{
		{Symbol: "A"}, {Symbol: "B"}, {Symbol: "C"},
	})
	if len(got) != 3 {
		t.Fatalf("expected 3 scores, got %d", len(got))
	}
	for _, s := range got {
		if s.Quartile != 0 {
			t.Errorf("expected quartile=0 below universe floor, got %d for %s", s.Quartile, s.Symbol)
		}
	}
}

func TestBuildScoresErrorSymbolsSilentlyDropped(t *testing.T) {
	svc := NewService(erringFetcher{}, Options{})
	got := svc.BuildScores(context.Background(), []SymbolRequest{
		{Symbol: "AAPL"}, {Symbol: "MSFT"}, {Symbol: "GOOG"}, {Symbol: "AMZN"},
	})
	if got != nil {
		t.Errorf("expected nil when every fetch errors, got %+v", got)
	}
}

func TestBuildScoresDedupsCaseAndWhitespace(t *testing.T) {
	svc := NewService(staticFetcher{byKey: map[string]fundamental.Metrics{
		"AAPL": {Symbol: "AAPL", ReturnOnEquity: 0.4},
		"MSFT": {Symbol: "MSFT", ReturnOnEquity: 0.3},
		"GOOG": {Symbol: "GOOG", ReturnOnEquity: 0.2},
	}}, Options{})
	got := svc.BuildScores(context.Background(), []SymbolRequest{
		{Symbol: "aapl  "}, {Symbol: "AAPL"}, // dup
		{Symbol: "msft"}, {Symbol: "goog"},
	})
	if len(got) != 3 {
		t.Fatalf("expected 3 unique scores, got %d (%+v)", len(got), got)
	}
}
