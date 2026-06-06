// Package consistency measures decision similarity across
// repeated runs of the same DecisionInput, so a CI gate can
// fail the build if the LLM (or our prompt) regressed into
// non-deterministic verdicts.
//
// MOTIVATION
// ----------
// Two PM-stage runs over the *same* DecisionInput should
// produce nearly the same trade list. Some variation is
// expected (LLM sampling, timestamp drift, ranker tie-breaks).
// What is NOT expected: yesterday's snapshot says "BUY AAPL,
// SELL MSFT" and today's snapshot — same inputs, no code
// changes — says "BUY GOOGL, HOLD AAPL". When that happens the
// most likely culprits are:
//
//   1. A non-deterministic upstream signal block (an unsorted
//      map iteration, a `time.Now()` baked into a prompt).
//   2. An LLM provider drift (model quietly upgraded, sampling
//      temperature changed, system prompt reformatted).
//   3. A regression in the prompt builder (tier-shuffle bug,
//      provenance fields leaking RNG into the prompt).
//
// Detecting these post-hoc through portfolio P&L is too slow.
// W2-14 introduces a CI fixture that:
//
//   * Loads a checked-in DecisionInput JSON.
//   * Calls the decision pipeline N times (default 5).
//   * Computes pairwise Jaccard similarity over the resulting
//     trade lists.
//   * Asserts the median ≥ a configurable floor (default 0.80).
//
// The Jaccard score is over the (symbol, direction) pair set.
// Confidence and weight differences are measured separately so
// a "same trades, slightly different sizes" run is not
// penalised.
//
// SCOPE
// -----
//   * Owns Trade, Run, Result, JaccardOf, MedianPairwise.
//   * Does NOT own the actual LLM call — the test fixture
//     supplies a Runner closure so the production model can be
//     swapped for a mock in unit tests.
package consistency

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Trade is the minimal post-decision view used for similarity
// scoring. We deliberately do NOT include weight or confidence
// in the Jaccard set so those can drift without failing the
// gate (they are reported separately).
type Trade struct {
	Symbol     string  `json:"symbol"`
	Direction  string  `json:"direction"`  // "buy" | "sell" | "hold"
	Weight     float64 `json:"weight"`
	Confidence float64 `json:"confidence"`
}

// Run is one decision-pipeline call result.
type Run struct {
	Index   int     `json:"index"`
	Trades  []Trade `json:"trades"`
}

// Result is the aggregate report.
type Result struct {
	RunCount        int       `json:"runCount"`
	Pairs           []Pair    `json:"pairs"`
	MedianJaccard   float64   `json:"medianJaccard"`
	MinJaccard      float64   `json:"minJaccard"`
	WeightDriftMean float64   `json:"weightDriftMean"`
	WeightDriftMax  float64   `json:"weightDriftMax"`
	ConfDriftMean   float64   `json:"confDriftMean"`
	ConfDriftMax    float64   `json:"confDriftMax"`
}

// Pair is one i,j similarity record.
type Pair struct {
	I            int     `json:"i"`
	J            int     `json:"j"`
	Jaccard      float64 `json:"jaccard"`
	WeightDrift  float64 `json:"weightDrift"`
	ConfDrift    float64 `json:"confDrift"`
}

// Compare runs the consistency analysis over a set of runs.
//
// Inputs:
//   * runs — at least 2; fewer returns a degenerate Result.
//
// Pure: same runs ↦ same result.
func Compare(runs []Run) Result {
	out := Result{RunCount: len(runs)}
	if len(runs) < 2 {
		out.MedianJaccard = 1.0
		out.MinJaccard = 1.0
		return out
	}
	pairs := make([]Pair, 0, len(runs)*(len(runs)-1)/2)
	for i := 0; i < len(runs); i++ {
		for j := i + 1; j < len(runs); j++ {
			p := Pair{
				I:           runs[i].Index,
				J:           runs[j].Index,
				Jaccard:     JaccardOf(runs[i].Trades, runs[j].Trades),
				WeightDrift: weightDrift(runs[i].Trades, runs[j].Trades),
				ConfDrift:   confDrift(runs[i].Trades, runs[j].Trades),
			}
			pairs = append(pairs, p)
		}
	}
	out.Pairs = pairs
	out.MedianJaccard = median(jaccards(pairs))
	out.MinJaccard = minOf(jaccards(pairs))
	out.WeightDriftMean, out.WeightDriftMax = meanMax(weightDrifts(pairs))
	out.ConfDriftMean, out.ConfDriftMax = meanMax(confDrifts(pairs))
	return out
}

// Assert returns nil iff the consistency report meets the
// configured thresholds. Designed for `if err := r.Assert(...);
// err != nil { t.Fatal(err) }` use in CI fixtures.
func (r Result) Assert(jaccardFloor, weightDriftMax float64) error {
	if r.RunCount < 2 {
		return fmt.Errorf("consistency: need at least 2 runs, got %d", r.RunCount)
	}
	if r.MedianJaccard < jaccardFloor {
		return fmt.Errorf(
			"consistency: median jaccard %.3f below floor %.3f (min=%.3f)",
			r.MedianJaccard, jaccardFloor, r.MinJaccard,
		)
	}
	if weightDriftMax > 0 && r.WeightDriftMax > weightDriftMax {
		return fmt.Errorf(
			"consistency: weight drift %.4f exceeds max %.4f",
			r.WeightDriftMax, weightDriftMax,
		)
	}
	return nil
}

// JaccardOf computes |A ∩ B| / |A ∪ B| over the (symbol,
// direction) pairs.
func JaccardOf(a, b []Trade) float64 {
	setA := make(map[string]struct{}, len(a))
	for _, t := range a {
		setA[tradeKey(t)] = struct{}{}
	}
	setB := make(map[string]struct{}, len(b))
	for _, t := range b {
		setB[tradeKey(t)] = struct{}{}
	}
	if len(setA) == 0 && len(setB) == 0 {
		return 1.0
	}
	if len(setA) == 0 || len(setB) == 0 {
		return 0
	}
	intersect := 0
	for k := range setA {
		if _, ok := setB[k]; ok {
			intersect++
		}
	}
	union := len(setA) + len(setB) - intersect
	if union == 0 {
		return 0
	}
	return float64(intersect) / float64(union)
}

// MedianPairwise is a small convenience: the median jaccard
// across all (i,j) pairs of runs. Equivalent to Compare(...).MedianJaccard.
func MedianPairwise(runs []Run) float64 {
	return Compare(runs).MedianJaccard
}

func tradeKey(t Trade) string {
	return strings.ToLower(strings.TrimSpace(t.Symbol)) + "|" +
		strings.ToLower(strings.TrimSpace(t.Direction))
}

func weightDrift(a, b []Trade) float64 {
	mapA := mapTradesByKey(a)
	mapB := mapTradesByKey(b)
	maxDelta := 0.0
	for k, ta := range mapA {
		tb, ok := mapB[k]
		if !ok {
			continue
		}
		d := math.Abs(ta.Weight - tb.Weight)
		if d > maxDelta {
			maxDelta = d
		}
	}
	return maxDelta
}

func confDrift(a, b []Trade) float64 {
	mapA := mapTradesByKey(a)
	mapB := mapTradesByKey(b)
	maxDelta := 0.0
	for k, ta := range mapA {
		tb, ok := mapB[k]
		if !ok {
			continue
		}
		d := math.Abs(ta.Confidence - tb.Confidence)
		if d > maxDelta {
			maxDelta = d
		}
	}
	return maxDelta
}

func mapTradesByKey(ts []Trade) map[string]Trade {
	out := make(map[string]Trade, len(ts))
	for _, t := range ts {
		out[tradeKey(t)] = t
	}
	return out
}

func jaccards(p []Pair) []float64 {
	out := make([]float64, len(p))
	for i, q := range p {
		out[i] = q.Jaccard
	}
	return out
}

func weightDrifts(p []Pair) []float64 {
	out := make([]float64, len(p))
	for i, q := range p {
		out[i] = q.WeightDrift
	}
	return out
}

func confDrifts(p []Pair) []float64 {
	out := make([]float64, len(p))
	for i, q := range p {
		out[i] = q.ConfDrift
	}
	return out
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 0 {
		return 0.5 * (sorted[mid-1] + sorted[mid])
	}
	return sorted[mid]
}

func minOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	m := xs[0]
	for _, v := range xs[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func meanMax(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	sum := 0.0
	max := xs[0]
	for _, v := range xs {
		sum += v
		if v > max {
			max = v
		}
	}
	return sum / float64(len(xs)), max
}
