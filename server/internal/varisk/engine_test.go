package varisk

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

func mkReturns(t *testing.T, values []float64) []DailyReturn {
	t.Helper()
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]DailyReturn, len(values))
	for i, v := range values {
		out[i] = DailyReturn{Date: start.AddDate(0, 0, i), Value: v}
	}
	return out
}

func newDeterministicEngine() *Engine {
	return &Engine{
		Now:     func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
		NewRand: func(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) },
	}
}

func TestComputeAll_RejectsTooSmallSample(t *testing.T) {
	e := newDeterministicEngine()
	values := make([]float64, MinSampleSize-1)
	for i := range values {
		values[i] = -0.001
	}
	_, err := e.ComputeAll(ComputeOptions{
		FundID:       "fund-1",
		Returns:      mkReturns(t, values),
		LookbackDays: MinSampleSize - 1,
		Horizon:      1,
	})
	if err == nil {
		t.Fatalf("expected error for sample size %d, got nil", MinSampleSize-1)
	}
}

func TestComputeAll_RejectsBadHorizon(t *testing.T) {
	e := newDeterministicEngine()
	values := make([]float64, 50)
	for i := range values {
		values[i] = 0.001
	}
	for _, h := range []int{0, -1, 21, 100} {
		_, err := e.ComputeAll(ComputeOptions{
			FundID:  "fund-1",
			Returns: mkReturns(t, values),
			Horizon: h,
		})
		if err == nil {
			t.Errorf("expected error for horizon %d", h)
		}
	}
}

func TestComputeAll_RejectsBadMethodOrConfidence(t *testing.T) {
	e := newDeterministicEngine()
	values := make([]float64, 50)
	for i := range values {
		values[i] = 0.001
	}
	if _, err := e.ComputeAll(ComputeOptions{
		Returns: mkReturns(t, values),
		Horizon: 1,
		Methods: []Method{"bogus"},
	}); err == nil {
		t.Fatal("expected method error")
	}
	if _, err := e.ComputeAll(ComputeOptions{
		Returns:     mkReturns(t, values),
		Horizon:     1,
		Confidences: []Confidence{0.5},
	}); err == nil {
		t.Fatal("expected confidence error")
	}
}

// Historical VaR on a known-distribution sample. With 100
// observations uniformly spaced from -0.1 to +0.0099, the 5th
// percentile is -0.0951 (linear interpolation, NumPy convention).
func TestComputeAll_Historical_KnownPercentile(t *testing.T) {
	e := newDeterministicEngine()
	values := make([]float64, 100)
	for i := 0; i < 100; i++ {
		// returns evenly spaced from -0.10 to -0.001
		values[i] = -0.10 + 0.001*float64(i)
	}
	snap, err := e.ComputeAll(ComputeOptions{
		FundID:      "fund-1",
		Returns:     mkReturns(t, values),
		Horizon:     1,
		Methods:     []Method{MethodHistorical},
		Confidences: []Confidence{Confidence95},
	})
	if err != nil {
		t.Fatalf("ComputeAll: %v", err)
	}
	if len(snap.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(snap.Results))
	}
	got := snap.Results[0]
	// NumPy linear percentile of the sorted array at p=0.05 with
	// n=100: pos = p * (n-1) = 4.95; low=4, high=5, w=0.95
	//   = sorted[4]*(1-0.95) + sorted[5]*0.95
	//   = (-0.096)*0.05 + (-0.095)*0.95
	//   = -0.09505
	want := -0.09505
	if math.Abs(got.Var-want) > 1e-9 {
		t.Errorf("VaR_95: want %f, got %f", want, got.Var)
	}
	// CVaR <= VaR.
	if got.CVar > got.Var+1e-12 {
		t.Errorf("CVaR (%f) should be <= VaR (%f)", got.CVar, got.Var)
	}
}

// Parametric VaR for a μ=0, σ=0.01 distribution at 95% should
// match -0.01 · z₀.₉₅ = -0.01644853...
func TestComputeAll_Parametric_KnownClosedForm(t *testing.T) {
	e := newDeterministicEngine()
	values := make([]float64, 100)
	rng := rand.New(rand.NewSource(42))
	for i := range values {
		values[i] = 0.01 * rng.NormFloat64()
	}
	snap, err := e.ComputeAll(ComputeOptions{
		FundID:      "fund-1",
		Returns:     mkReturns(t, values),
		Horizon:     1,
		Methods:     []Method{MethodParametric},
		Confidences: []Confidence{Confidence95},
	})
	if err != nil {
		t.Fatalf("ComputeAll: %v", err)
	}
	got := snap.Results[0]
	// Closed-form: VaR = μ - z·σ. We don't know μ/σ exactly
	// (sample-dependent) so just check the relationship.
	want := snap.Mean - Confidence95.ZScore()*snap.Std
	if math.Abs(got.Var-want) > 1e-9 {
		t.Errorf("parametric VaR_95: want %f, got %f", want, got.Var)
	}
	if got.CVar > got.Var+1e-12 {
		t.Errorf("CVaR (%f) should be <= VaR (%f)", got.CVar, got.Var)
	}
}

// Monte Carlo with a fixed seed must be deterministic across
// runs. Two ComputeAll invocations with identical inputs return
// identical results.
func TestComputeAll_MonteCarlo_DeterministicWithSeed(t *testing.T) {
	e := newDeterministicEngine()
	values := make([]float64, 100)
	for i := range values {
		values[i] = 0.0001 * float64(i-50)
	}
	opts := ComputeOptions{
		FundID:          "fund-1",
		Returns:         mkReturns(t, values),
		Horizon:         1,
		Methods:         []Method{MethodMonteCarlo},
		Confidences:     []Confidence{Confidence95},
		MonteCarloSeed:  20260601,
		MonteCarloPaths: 5_000,
	}
	a, err := e.ComputeAll(opts)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.ComputeAll(opts)
	if err != nil {
		t.Fatal(err)
	}
	if a.Results[0].Var != b.Results[0].Var {
		t.Errorf("non-deterministic VaR: a=%f b=%f", a.Results[0].Var, b.Results[0].Var)
	}
	if a.Results[0].CVar != b.Results[0].CVar {
		t.Errorf("non-deterministic CVaR: a=%f b=%f", a.Results[0].CVar, b.Results[0].CVar)
	}
	if a.Results[0].MonteCarloSeed == nil || *a.Results[0].MonteCarloSeed != opts.MonteCarloSeed {
		t.Errorf("expected MC seed echoed back, got %v", a.Results[0].MonteCarloSeed)
	}
}

// With a large normal sample, parametric and Monte Carlo should
// converge. They agree to ~0.5σ-of-the-VaR-quantile.
func TestComputeAll_MonteCarlo_ConvergesToParametric(t *testing.T) {
	e := newDeterministicEngine()
	rng := rand.New(rand.NewSource(7))
	values := make([]float64, 500)
	for i := range values {
		values[i] = 0.001 + 0.01*rng.NormFloat64()
	}
	snap, err := e.ComputeAll(ComputeOptions{
		FundID:          "fund-1",
		Returns:         mkReturns(t, values),
		Horizon:         1,
		Methods:         []Method{MethodParametric, MethodMonteCarlo},
		Confidences:     []Confidence{Confidence95},
		MonteCarloSeed:  1,
		MonteCarloPaths: 100_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	var pVar, mcVar float64
	for _, r := range snap.Results {
		switch r.Method {
		case MethodParametric:
			pVar = r.Var
		case MethodMonteCarlo:
			mcVar = r.Var
		}
	}
	// Within 5% relative (MC noise + asymptotic agreement).
	rel := math.Abs(pVar-mcVar) / math.Abs(pVar)
	if rel > 0.05 {
		t.Errorf("MC %f vs parametric %f relative diff %f > 5%%", mcVar, pVar, rel)
	}
}

// Horizon scaling: VaR_5d = VaR_1d * sqrt(5). We use the
// parametric branch which has no sampling noise so the
// relationship is exact (modulo float).
func TestComputeAll_HorizonScalingExact(t *testing.T) {
	e := newDeterministicEngine()
	rng := rand.New(rand.NewSource(11))
	values := make([]float64, 200)
	for i := range values {
		values[i] = 0.01 * rng.NormFloat64()
	}
	one, err := e.ComputeAll(ComputeOptions{
		FundID:      "fund-1",
		Returns:     mkReturns(t, values),
		Horizon:     1,
		Methods:     []Method{MethodParametric},
		Confidences: []Confidence{Confidence99},
	})
	if err != nil {
		t.Fatal(err)
	}
	five, err := e.ComputeAll(ComputeOptions{
		FundID:      "fund-1",
		Returns:     mkReturns(t, values),
		Horizon:     5,
		Methods:     []Method{MethodParametric},
		Confidences: []Confidence{Confidence99},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := one.Results[0].Var * math.Sqrt(5)
	got := five.Results[0].Var
	if math.Abs(want-got) > 1e-12 {
		t.Errorf("horizon scaling: want %f, got %f", want, got)
	}
}

// All methods produce VaR <= 0 and CVaR <= VaR.
func TestComputeAll_SignConventionInvariants(t *testing.T) {
	e := newDeterministicEngine()
	rng := rand.New(rand.NewSource(0))
	values := make([]float64, 250)
	for i := range values {
		values[i] = 0.0005 + 0.012*rng.NormFloat64()
	}
	snap, err := e.ComputeAll(ComputeOptions{
		FundID:          "fund-1",
		Returns:         mkReturns(t, values),
		Horizon:         1,
		MonteCarloSeed:  101,
		MonteCarloPaths: 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range snap.Results {
		if r.Var > 0 {
			t.Errorf("VaR should be <= 0 for method=%s conf=%v, got %f", r.Method, r.Confidence, r.Var)
		}
		if r.CVar > r.Var+1e-9 {
			t.Errorf("CVaR (%f) > VaR (%f) for method=%s conf=%v", r.CVar, r.Var, r.Method, r.Confidence)
		}
	}
	// Snapshot result count: 3 methods × 3 confidences.
	if len(snap.Results) != 9 {
		t.Errorf("expected 9 results, got %d", len(snap.Results))
	}
}

// Caller's returns slice must NOT be mutated by the engine.
func TestComputeAll_DoesNotMutateInput(t *testing.T) {
	e := newDeterministicEngine()
	values := []float64{0.01, -0.02, 0.005, -0.03, 0.001, 0.004, -0.001, 0.012, -0.011, 0.007,
		0.002, -0.004, 0.008, -0.006, 0.003, -0.009, 0.013, -0.014, 0.006, -0.002,
		0.009, -0.005, 0.011, -0.013, 0.0001, 0.0004, -0.0006, 0.0021, -0.0014, 0.0033}
	input := mkReturns(t, values)
	clone := make([]DailyReturn, len(input))
	copy(clone, input)
	_, err := e.ComputeAll(ComputeOptions{
		FundID:  "fund-1",
		Returns: input,
		Horizon: 1,
		Methods: []Method{MethodHistorical},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range input {
		if v != clone[i] {
			t.Errorf("engine mutated input at i=%d", i)
		}
	}
}

func TestParseMethod(t *testing.T) {
	cases := map[string]Method{
		"historical":  MethodHistorical,
		"Historical":  MethodHistorical,
		"parametric":  MethodParametric,
		"monte_carlo": MethodMonteCarlo,
		"montecarlo":  MethodMonteCarlo,
		"Monte-Carlo": MethodMonteCarlo,
	}
	for in, want := range cases {
		got, ok := ParseMethod(in)
		if !ok || got != want {
			t.Errorf("ParseMethod(%q) = (%q, %v), want (%q, true)", in, got, ok, want)
		}
	}
	if _, ok := ParseMethod("garch"); ok {
		t.Error("ParseMethod should reject garch")
	}
}
