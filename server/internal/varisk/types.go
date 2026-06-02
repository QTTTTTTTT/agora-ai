// Package varisk computes portfolio-level Value-at-Risk and
// Conditional Value-at-Risk (a.k.a. Expected Shortfall) from a
// time series of daily returns.
//
// Why "varisk" and not "var" — "var" is a reserved keyword in Go.
// Package name has to be something else; we picked "varisk" =
// "value at risk" without the dot.
//
// Three methods are supported. They consume the same input
// (a slice of daily returns) but make different distributional
// assumptions:
//
//   - Historical: non-parametric. Sorts the realised returns
//     and picks the percentile that matches `1 - confidence`.
//     Robust to fat tails but only sees what's actually
//     happened.
//
//   - Parametric: assumes daily returns are normally distributed
//     with mean μ and std σ. VaR = -(μ - z·σ); CVaR follows the
//     Phi/phi closed form. Fast and stable, but understates
//     real tail risk because returns are leptokurtic.
//
//   - Monte Carlo: draws N samples from N(μ, σ) and takes the
//     empirical percentile. With normal sampling it converges
//     to parametric; useful as the scaffolding for later
//     migration to non-normal distributions (Student-t, EWMA).
//
// All three return Result values that share the same sign
// convention: VaR < 0 means "we expect a loss of |VaR| at the
// confidence level". CVaR is always at least as negative as
// VaR (the conditional tail expectation can't be smaller in
// absolute value than the threshold itself).
package varisk

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Method enumerates the supported computation methods. String
// values match the CHECK constraint on portfolio_var_snapshots.method.
type Method string

const (
	MethodHistorical Method = "historical"
	MethodParametric Method = "parametric"
	MethodMonteCarlo Method = "monte_carlo"
)

// AllMethods is the canonical order the UI / docs render. Stable
// across builds so dashboards stay deterministic.
var AllMethods = []Method{MethodHistorical, MethodParametric, MethodMonteCarlo}

// IsValid for Method.
func (m Method) IsValid() bool {
	switch m {
	case MethodHistorical, MethodParametric, MethodMonteCarlo:
		return true
	}
	return false
}

// ParseMethod accepts a case-insensitive name and returns the
// canonical Method or "" + false. Accepts both "monte_carlo"
// and "montecarlo" as a convenience for URL query strings.
func ParseMethod(s string) (Method, bool) {
	candidate := strings.ToLower(strings.TrimSpace(s))
	candidate = strings.ReplaceAll(candidate, "-", "_")
	if candidate == "montecarlo" {
		candidate = string(MethodMonteCarlo)
	}
	m := Method(candidate)
	if !m.IsValid() {
		return "", false
	}
	return m, true
}

// Confidence is a confidence level in (0, 1). We constrain to
// the canonical set {0.90, 0.95, 0.99} both here and at the DB
// layer so trend lines stay comparable.
type Confidence float64

const (
	Confidence90 Confidence = 0.90
	Confidence95 Confidence = 0.95
	Confidence99 Confidence = 0.99
)

// AllConfidences is the canonical render order.
var AllConfidences = []Confidence{Confidence90, Confidence95, Confidence99}

// IsValid for Confidence; mirrors the CHECK constraint.
func (c Confidence) IsValid() bool {
	switch c {
	case Confidence90, Confidence95, Confidence99:
		return true
	}
	return false
}

// ZScore returns the one-sided z-score for the parametric
// method. The values are hard-coded for the canonical
// confidence set to avoid a math/erfinv dependency.
//
//	0.90 → 1.2815515655446004
//	0.95 → 1.6448536269514722
//	0.99 → 2.326347874040841
//
// These match scipy.stats.norm.ppf(c) to 16 digits.
func (c Confidence) ZScore() float64 {
	switch c {
	case Confidence90:
		return 1.2815515655446004
	case Confidence95:
		return 1.6448536269514722
	case Confidence99:
		return 2.326347874040841
	}
	return 0
}

// Result is one (method, confidence, horizon) computation output.
//
// Sign convention. Var <= 0; CVar <= Var (always at least as
// negative as VaR). Coverage info exposes "how many daily
// returns went into this number" so the UI can flag "too few
// samples" before the PM relies on it.
type Result struct {
	Method     Method
	Confidence Confidence
	Horizon    int
	Var        float64 // negative; e.g. -0.023 → 2.3% one-day loss at the confidence
	CVar       float64 // negative; conditional tail expectation
	Mean       float64 // mean daily return of the sample
	Std        float64 // std deviation of the sample
	SampleSize int
	// SampleWindowStart / End mark the slice of nav_snapshots
	// the sample was drawn from (historical only); parametric +
	// monte_carlo also use the same window so we populate
	// these uniformly.
	SampleWindowStart time.Time
	SampleWindowEnd   time.Time
	// Only populated for Method == MonteCarlo so the snapshot
	// can be reproduced.
	MonteCarloSeed  *int64
	MonteCarloPaths *int
}

// Snapshot is the bundled response when the live endpoint
// computes all (method, confidence) combinations for one
// horizon — typically what the UI displays.
type Snapshot struct {
	FundID        string
	GeneratedAt   time.Time
	Horizon       int
	LookbackDays  int
	SampleSize    int
	Mean          float64
	Std           float64
	Results       []Result
}

// DailyReturn is one row of input. The engine sorts these
// internally; callers can hand them in any order.
type DailyReturn struct {
	Date  time.Time
	Value float64
}

// percentile returns the linear-interpolated p-th percentile of
// the sorted slice. p ∈ [0, 1]. Defined here so the engine
// doesn't depend on gonum just for one quantile.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	// NumPy "linear" convention: position = p * (n - 1).
	pos := p * float64(n-1)
	low := int(math.Floor(pos))
	high := int(math.Ceil(pos))
	if low == high {
		return sorted[low]
	}
	weight := pos - float64(low)
	return sorted[low]*(1-weight) + sorted[high]*weight
}

// stdev computes the sample standard deviation (ddof=1).
func stdev(values []float64, mean float64) float64 {
	if len(values) < 2 {
		return 0
	}
	var ssq float64
	for _, v := range values {
		d := v - mean
		ssq += d * d
	}
	return math.Sqrt(ssq / float64(len(values)-1))
}

// meanOf returns the arithmetic mean of values. Returns 0 for
// empty input.
func meanOf(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var s float64
	for _, v := range values {
		s += v
	}
	return s / float64(len(values))
}

// sortAsc returns a sorted copy of the input so the engine never
// mutates the caller's slice.
func sortAsc(values []float64) []float64 {
	out := make([]float64, len(values))
	copy(out, values)
	sort.Float64s(out)
	return out
}

// formatErrInvalidConf is shared by callers that validate
// confidence externally.
func formatErrInvalidConf(c Confidence) error {
	return fmt.Errorf("varisk: invalid confidence %v (allowed: 0.90, 0.95, 0.99)", c)
}
