package varisk

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"
)

// MinSampleSize is the minimum number of daily returns the
// engine requires. Below this any number is statistical noise.
// We pick 20 as a low floor (1 trading month) and let the UI
// flag samples 20-30 as "low confidence".
const MinSampleSize = 20

// DefaultMonteCarloPaths is the simulation count for Monte
// Carlo. 50 000 paths gives a stable percentile to ~3 digits
// at 99% confidence without burning seconds of CPU.
const DefaultMonteCarloPaths = 50_000

// Engine computes VaR / CVaR for one fund. Stateless and safe
// for concurrent use; the only data it holds is the wall-clock
// time source (for snapshot timestamps and the default RNG).
type Engine struct {
	Now func() time.Time
	// NewRand lets tests inject a deterministic source so the
	// Monte Carlo path is reproducible without seed gymnastics.
	NewRand func(seed int64) *rand.Rand
}

// NewEngine returns a default engine using time.Now and the
// stdlib RNG.
func NewEngine() *Engine {
	return &Engine{
		Now:     func() time.Time { return time.Now().UTC() },
		NewRand: defaultNewRand,
	}
}

func defaultNewRand(seed int64) *rand.Rand {
	return rand.New(rand.NewSource(seed))
}

// ComputeOptions wraps the knobs callers pass to ComputeAll.
//
// LookbackDays controls how much history goes into the sample.
// The caller is responsible for actually pulling that many days
// from nav_snapshots; the engine reports back what it received
// in SampleSize.
type ComputeOptions struct {
	FundID       string
	Returns      []DailyReturn
	LookbackDays int
	Horizon      int
	Methods      []Method
	Confidences  []Confidence
	// MonteCarloPaths defaults to DefaultMonteCarloPaths when 0.
	MonteCarloPaths int
	// MonteCarloSeed; when 0 the engine derives a seed from
	// Now() so successive calls don't collide.
	MonteCarloSeed int64
}

// ComputeAll runs every (method × confidence) combination and
// returns a Snapshot. Returns an error if the sample is below
// MinSampleSize or any of the requested methods/confidences is
// invalid; partial failure is not acceptable for this surface.
func (e *Engine) ComputeAll(opts ComputeOptions) (Snapshot, error) {
	if opts.Horizon < 1 || opts.Horizon > 20 {
		return Snapshot{}, fmt.Errorf("varisk: horizon %d out of range [1, 20]", opts.Horizon)
	}
	if len(opts.Methods) == 0 {
		opts.Methods = AllMethods
	}
	if len(opts.Confidences) == 0 {
		opts.Confidences = AllConfidences
	}
	for _, m := range opts.Methods {
		if !m.IsValid() {
			return Snapshot{}, fmt.Errorf("varisk: invalid method %q", m)
		}
	}
	for _, c := range opts.Confidences {
		if !c.IsValid() {
			return Snapshot{}, formatErrInvalidConf(c)
		}
	}
	if len(opts.Returns) < MinSampleSize {
		return Snapshot{}, fmt.Errorf("varisk: sample size %d below minimum %d", len(opts.Returns), MinSampleSize)
	}

	// Defensive copy + chronological sort so the window
	// start/end labels are correct even if the caller hands us
	// shuffled data.
	rows := make([]DailyReturn, len(opts.Returns))
	copy(rows, opts.Returns)
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Date.Before(rows[j].Date)
	})

	values := make([]float64, len(rows))
	for i, r := range rows {
		values[i] = r.Value
	}
	mean := meanOf(values)
	std := stdev(values, mean)

	// Square-root-of-time scaling for horizon > 1d. This is the
	// "standard" approximation; valid under IID returns.
	scale := math.Sqrt(float64(opts.Horizon))

	mcPaths := opts.MonteCarloPaths
	if mcPaths <= 0 {
		mcPaths = DefaultMonteCarloPaths
	}
	mcSeed := opts.MonteCarloSeed
	if mcSeed == 0 {
		mcSeed = e.Now().UnixNano()
	}

	windowStart := rows[0].Date
	windowEnd := rows[len(rows)-1].Date

	results := make([]Result, 0, len(opts.Methods)*len(opts.Confidences))
	for _, m := range opts.Methods {
		for _, c := range opts.Confidences {
			var r Result
			switch m {
			case MethodHistorical:
				r = e.computeHistorical(values, c)
			case MethodParametric:
				r = e.computeParametric(mean, std, c)
			case MethodMonteCarlo:
				r = e.computeMonteCarlo(mean, std, c, mcPaths, mcSeed)
			}
			r.Method = m
			r.Confidence = c
			r.Horizon = opts.Horizon
			r.Var *= scale
			r.CVar *= scale
			r.Mean = mean
			r.Std = std
			r.SampleSize = len(values)
			r.SampleWindowStart = windowStart
			r.SampleWindowEnd = windowEnd
			results = append(results, r)
		}
	}

	// Stable sort by (Method order, Confidence ascending) so the
	// UI gets predictable rows.
	methodRank := map[Method]int{}
	for i, m := range AllMethods {
		methodRank[m] = i
	}
	sort.SliceStable(results, func(i, j int) bool {
		ri, rj := methodRank[results[i].Method], methodRank[results[j].Method]
		if ri != rj {
			return ri < rj
		}
		return results[i].Confidence < results[j].Confidence
	})

	return Snapshot{
		FundID:       opts.FundID,
		GeneratedAt:  e.Now(),
		Horizon:      opts.Horizon,
		LookbackDays: opts.LookbackDays,
		SampleSize:   len(values),
		Mean:         mean,
		Std:          std,
		Results:      results,
	}, nil
}

// computeHistorical sorts the realised returns and reads the
// (1 - c) percentile. CVaR is the mean of all returns at or
// below that percentile.
//
// Inputs are the raw daily returns (NOT yet scaled by horizon).
// Caller scales VaR/CVar by sqrt(horizon).
func (e *Engine) computeHistorical(values []float64, c Confidence) Result {
	sorted := sortAsc(values)
	alpha := 1.0 - float64(c)
	// VaR at the alpha quantile (e.g. the 5th percentile for c=0.95).
	v := percentile(sorted, alpha)
	// CVaR = mean of all observations <= VaR. With percentile
	// interpolation the threshold itself sits between two
	// observations; we include every observation strictly below
	// AND the floor index to mirror the textbook "expected loss
	// given worse-than-VaR" definition.
	var cvarSum float64
	var cvarCount int
	for _, r := range sorted {
		if r <= v {
			cvarSum += r
			cvarCount++
		} else {
			break
		}
	}
	var cvar float64
	if cvarCount > 0 {
		cvar = cvarSum / float64(cvarCount)
	} else {
		// Degenerate edge case (everything above the
		// quantile). Pin CVaR to VaR so the constraint
		// CVaR <= VaR still holds.
		cvar = v
	}
	return Result{Var: v, CVar: cvar}
}

// computeParametric assumes N(μ, σ) and uses closed-form
// expressions:
//
//	VaR  = μ - z·σ                       (always <= 0 when sample mean is small)
//	CVaR = μ - σ · φ(z) / (1-c)          (Expected Shortfall under normality)
//
// where φ is the standard normal PDF.
func (e *Engine) computeParametric(mean, std float64, c Confidence) Result {
	z := c.ZScore()
	v := mean - z*std
	// φ(z) = (1/√(2π)) · exp(-z²/2)
	phi := math.Exp(-z*z/2) / math.Sqrt(2*math.Pi)
	alpha := 1.0 - float64(c)
	// ES_α = μ - σ · φ(z) / α
	cvar := mean - std*phi/alpha
	// Numerical safety: never return CVaR > VaR.
	if cvar > v {
		cvar = v
	}
	return Result{Var: v, CVar: cvar}
}

// computeMonteCarlo draws `paths` samples from N(μ, σ) and
// reads the empirical percentile, just like the historical
// method. Reproducible given (mean, std, c, paths, seed).
func (e *Engine) computeMonteCarlo(mean, std float64, c Confidence, paths int, seed int64) Result {
	rng := e.NewRand(seed)
	sample := make([]float64, paths)
	for i := 0; i < paths; i++ {
		sample[i] = mean + std*rng.NormFloat64()
	}
	sort.Float64s(sample)
	alpha := 1.0 - float64(c)
	v := percentile(sample, alpha)
	var cvarSum float64
	var cvarCount int
	for _, r := range sample {
		if r <= v {
			cvarSum += r
			cvarCount++
		} else {
			break
		}
	}
	var cvar float64
	if cvarCount > 0 {
		cvar = cvarSum / float64(cvarCount)
	} else {
		cvar = v
	}
	mcSeed := seed
	mcPaths := paths
	return Result{
		Var:             v,
		CVar:            cvar,
		MonteCarloSeed:  &mcSeed,
		MonteCarloPaths: &mcPaths,
	}
}
