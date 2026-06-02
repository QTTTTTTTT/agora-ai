package brinson

import (
	"math"
	"testing"
	"time"
)

func mkBucket(key string, weight, ret float64) Bucket {
	return Bucket{Key: key, Weight: weight, ReturnPct: ret}
}

func engine() *Engine {
	return &Engine{Now: func() time.Time { return time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) }}
}

// The textbook two-bucket Brinson example:
//   portfolio: equity 70% @ 12%, bond 30% @ 4%
//   benchmark: equity 60% @ 10%, bond 40% @ 5%
//
// Manual computation:
//   r_p - r_b = (0.70*0.12 + 0.30*0.04) - (0.60*0.10 + 0.40*0.05)
//             = 0.096 - 0.080 = 0.016
//
//   allocation(eq) = (0.70 - 0.60) * 0.10 = 0.010
//   allocation(bd) = (0.30 - 0.40) * 0.05 = -0.005
//   total alloc    = 0.005
//
//   selection(eq)  = 0.60 * (0.12 - 0.10) = 0.012
//   selection(bd)  = 0.40 * (0.04 - 0.05) = -0.004
//   total sel      = 0.008
//
//   interaction(eq) = (0.70 - 0.60) * (0.12 - 0.10) = 0.002
//   interaction(bd) = (0.30 - 0.40) * (0.04 - 0.05) = 0.001
//   total inter     = 0.003
//
//   alloc + sel + inter = 0.005 + 0.008 + 0.003 = 0.016 = r_p - r_b ✓
func TestEngine_Compute_TextbookExample(t *testing.T) {
	e := engine()
	p := Composition{Dimension: DimAssetClass, Buckets: []Bucket{
		mkBucket("equity", 0.70, 0.12),
		mkBucket("bond", 0.30, 0.04),
	}}
	b := Composition{BenchmarkID: "60-40", Dimension: DimAssetClass, Buckets: []Bucket{
		mkBucket("equity", 0.60, 0.10),
		mkBucket("bond", 0.40, 0.05),
	}}
	r := e.Compute(p, b)
	if math.Abs(r.PortfolioReturn-0.096) > 1e-9 {
		t.Errorf("portfolio_return = %f", r.PortfolioReturn)
	}
	if math.Abs(r.BenchmarkReturn-0.080) > 1e-9 {
		t.Errorf("benchmark_return = %f", r.BenchmarkReturn)
	}
	if math.Abs(r.ActiveReturn-0.016) > 1e-9 {
		t.Errorf("active_return = %f", r.ActiveReturn)
	}
	if math.Abs(r.AllocationTotal-0.005) > 1e-9 {
		t.Errorf("allocation_total = %f, want 0.005", r.AllocationTotal)
	}
	if math.Abs(r.SelectionTotal-0.008) > 1e-9 {
		t.Errorf("selection_total = %f, want 0.008", r.SelectionTotal)
	}
	if math.Abs(r.InteractionTotal-0.003) > 1e-9 {
		t.Errorf("interaction_total = %f, want 0.003", r.InteractionTotal)
	}
	// Identity check
	sum := r.AllocationTotal + r.SelectionTotal + r.InteractionTotal
	if math.Abs(sum-r.ActiveReturn) > 1e-9 {
		t.Errorf("decomposition identity broken: alloc+sel+inter=%f, active=%f", sum, r.ActiveReturn)
	}
}

// Bucket appearing only on the benchmark side (portfolio has 0
// weight) must still contribute to the identity. Equity weight 0
// + benchmark equity weight 0.5 @ return 0.10 should yield
// allocation = (0 - 0.5)*0.10 = -0.05.
func TestEngine_Compute_BenchmarkOnlyBucket(t *testing.T) {
	e := engine()
	p := Composition{Dimension: DimAssetClass, Buckets: []Bucket{
		mkBucket("bond", 1.0, 0.05),
	}}
	b := Composition{Dimension: DimAssetClass, Buckets: []Bucket{
		mkBucket("equity", 0.5, 0.10),
		mkBucket("bond", 0.5, 0.05),
	}}
	r := e.Compute(p, b)
	// active_return = 0.05 - (0.5*0.10 + 0.5*0.05) = 0.05 - 0.075 = -0.025
	if math.Abs(r.ActiveReturn-(-0.025)) > 1e-9 {
		t.Errorf("active_return = %f, want -0.025", r.ActiveReturn)
	}
	// alloc(eq) = (0 - 0.5) * 0.10 = -0.05
	// alloc(bd) = (1.0 - 0.5) * 0.05 = 0.025
	// total alloc = -0.025
	if math.Abs(r.AllocationTotal-(-0.025)) > 1e-9 {
		t.Errorf("allocation_total = %f, want -0.025", r.AllocationTotal)
	}
	// Identity holds
	sum := r.AllocationTotal + r.SelectionTotal + r.InteractionTotal
	if math.Abs(sum-r.ActiveReturn) > 1e-9 {
		t.Errorf("decomposition broken: sum=%f active=%f", sum, r.ActiveReturn)
	}
}

// Case-insensitive bucket matching: "Equity" on one side and
// "equity" on the other should match.
func TestEngine_Compute_CaseInsensitiveMatching(t *testing.T) {
	e := engine()
	p := Composition{Dimension: DimAssetClass, Buckets: []Bucket{
		mkBucket("Equity", 1.0, 0.10),
	}}
	b := Composition{Dimension: DimAssetClass, Buckets: []Bucket{
		mkBucket("equity", 1.0, 0.05),
	}}
	r := e.Compute(p, b)
	// With one bucket matched: alloc = 0, sel = 1.0 * 0.05 = 0.05,
	// inter = 0; active = 0.05.
	if r.BucketCount != 1 {
		t.Errorf("bucket_count = %d, want 1", r.BucketCount)
	}
	if math.Abs(r.SelectionTotal-0.05) > 1e-9 {
		t.Errorf("selection_total = %f, want 0.05", r.SelectionTotal)
	}
}

// Buckets sorted by total-effect magnitude descending.
func TestEngine_Compute_BucketsSortedByMagnitude(t *testing.T) {
	e := engine()
	p := Composition{Dimension: DimAssetClass, Buckets: []Bucket{
		mkBucket("small_effect", 0.10, 0.001),
		mkBucket("huge_effect", 0.80, 0.20),
		mkBucket("medium_effect", 0.10, 0.05),
	}}
	b := Composition{Dimension: DimAssetClass, Buckets: []Bucket{
		mkBucket("small_effect", 0.10, 0.001),
		mkBucket("huge_effect", 0.50, 0.05),
		mkBucket("medium_effect", 0.40, 0.02),
	}}
	r := e.Compute(p, b)
	if r.Buckets[0].Key != "huge_effect" {
		t.Errorf("expected huge_effect first, got %s", r.Buckets[0].Key)
	}
}

// Validate scenario:
func TestComposition_Validate(t *testing.T) {
	// Good
	c := Composition{Dimension: DimAssetClass, Buckets: []Bucket{
		mkBucket("equity", 0.7, 0.1),
		mkBucket("bond", 0.3, 0.04),
	}}
	if err := c.Validate(); err != nil {
		t.Errorf("good composition rejected: %v", err)
	}
	// Bad dim
	if err := (Composition{Dimension: "bogus", Buckets: []Bucket{mkBucket("a", 1, 0)}}).Validate(); err == nil {
		t.Error("expected error for bad dim")
	}
	// Empty buckets
	if err := (Composition{Dimension: DimAssetClass}).Validate(); err == nil {
		t.Error("expected error for empty buckets")
	}
	// Negative weight
	if err := (Composition{Dimension: DimAssetClass, Buckets: []Bucket{mkBucket("a", -0.5, 0)}}).Validate(); err == nil {
		t.Error("expected error for negative weight")
	}
	// Weight too big (probably percent vs fraction)
	if err := (Composition{Dimension: DimAssetClass, Buckets: []Bucket{mkBucket("a", 50, 0)}}).Validate(); err == nil {
		t.Error("expected error for percentage-instead-of-fraction")
	}
	// Weights don't sum to ~1
	if err := (Composition{Dimension: DimAssetClass, Buckets: []Bucket{
		mkBucket("a", 0.3, 0),
		mkBucket("b", 0.3, 0),
	}}).Validate(); err == nil {
		t.Error("expected error for weights summing to 0.6")
	}
	// Duplicate key
	if err := (Composition{Dimension: DimAssetClass, Buckets: []Bucket{
		mkBucket("a", 0.5, 0),
		mkBucket("A", 0.5, 0),
	}}).Validate(); err == nil {
		t.Error("expected error for duplicate key (case-insensitive)")
	}
}

// PortfolioFromHoldings aggregates correctly.
func TestPortfolioFromHoldings_HappyPath(t *testing.T) {
	asof := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	rows := []HoldingInput{
		{Bucket: "equity", MarketValue: 7000, ReturnPct: 0.12},
		{Bucket: "equity", MarketValue: 3000, ReturnPct: 0.08},
		{Bucket: "bond", MarketValue: 5000, ReturnPct: 0.04},
	}
	c := PortfolioFromHoldings(DimAssetClass, rows, asof)
	if len(c.Buckets) != 2 {
		t.Fatalf("len buckets = %d", len(c.Buckets))
	}
	// equity weight = 10000 / 15000 = 0.6667
	// equity return = (7000*0.12 + 3000*0.08) / 10000 = 0.108
	// bond weight = 5000 / 15000 = 0.3333
	// bond return = 0.04
	var eq, bd Bucket
	for _, b := range c.Buckets {
		switch b.Key {
		case "equity":
			eq = b
		case "bond":
			bd = b
		}
	}
	if math.Abs(eq.Weight-2.0/3.0) > 1e-9 {
		t.Errorf("equity weight = %f, want 2/3", eq.Weight)
	}
	if math.Abs(eq.ReturnPct-0.108) > 1e-9 {
		t.Errorf("equity return = %f, want 0.108", eq.ReturnPct)
	}
	if math.Abs(bd.Weight-1.0/3.0) > 1e-9 {
		t.Errorf("bond weight = %f, want 1/3", bd.Weight)
	}
	// Validate the produced composition (weights sum to ~1)
	if err := c.Validate(); err != nil {
		t.Errorf("produced composition failed validate: %v", err)
	}
}

// Short positions contribute |MV| to bucket notional.
func TestPortfolioFromHoldings_ShortLeg(t *testing.T) {
	asof := time.Now()
	rows := []HoldingInput{
		{Bucket: "equity", MarketValue: 10000, ReturnPct: 0.10},
		{Bucket: "equity", MarketValue: -5000, ReturnPct: -0.05},
	}
	c := PortfolioFromHoldings(DimAssetClass, rows, asof)
	if len(c.Buckets) != 1 {
		t.Fatalf("len buckets = %d", len(c.Buckets))
	}
	// total MV = |10000| + |-5000| = 15000; equity weight = 15000/15000 = 1.0
	// return = (10000*0.10 + 5000*-0.05) / 15000 ≈ 0.0333
	want := (10000*0.10 + 5000*-0.05) / 15000
	if math.Abs(c.Buckets[0].ReturnPct-want) > 1e-9 {
		t.Errorf("equity return = %f, want %f", c.Buckets[0].ReturnPct, want)
	}
}

func TestPortfolioFromHoldings_NoHoldings(t *testing.T) {
	c := PortfolioFromHoldings(DimAssetClass, nil, time.Now())
	if len(c.Buckets) != 0 {
		t.Errorf("expected empty composition")
	}
}
