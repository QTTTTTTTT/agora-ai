// Package calibration tracks how well agents' stated confidences
// match observed outcomes. It is the W2-7 building block for
// "is the bull agent's 0.85 actually about right, or is it more
// like a 0.55?".
//
// METRICS
// -------
// We compute three standard scores from a stream of (forecast,
// outcome) pairs:
//
//  1. **Brier score** — the mean squared error between the
//     forecasted probability and the realised binary outcome.
//     Lower is better; perfect = 0, worst = 1.
//
//  2. **Reliability diagram** — bin forecasts by their stated
//     probability (default: 10 buckets of width 0.1) and report
//     mean(forecast) vs mean(outcome) per bucket. A perfectly
//     calibrated agent's curve hugs the y=x diagonal.
//
//  3. **Expected Calibration Error (ECE)** — the
//     count-weighted mean absolute gap between mean(forecast)
//     and mean(outcome) across the buckets. One scalar that
//     summarises the reliability diagram; useful for alerts.
//
// SCOPE
// -----
// This package owns:
//   * the Forecast value object;
//   * the Tracker (in-memory, thread-safe);
//   * the Aggregate report shape returned to callers.
//
// It does NOT own:
//   * Persistence — the Wave-2 wiring layer will store summaries
//     (per-agent rolling Brier / ECE) into a small table once the
//     plan_outcome resolver from W1-5 starts producing data. The
//     Tracker is in-memory for the same reason agentreputation
//     works that way: it's a streaming aggregator, the persisted
//     row is a snapshot, not the source of truth.
//   * Score-boundary policy — "what counts as a hit?" is left to
//     the caller. The caller maps a plan's outcome (e.g. realised
//     return > 0, alpha > 0, win-rate > 0.5) to a 0/1.
package calibration

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// Forecast records one prediction-outcome pair. AgentID groups
// the stream by agent so per-agent reliability diagrams can be
// computed independently. Confidence is the agent's stated
// probability in [0, 1]; values outside this range are clamped
// at Record time. Outcome is 0 (miss) or 1 (hit); any non-zero
// value is treated as 1.
type Forecast struct {
	AgentID    string
	Confidence float64
	Outcome    float64
	At         time.Time
}

// Bucket is one row in the reliability diagram. UpperBound is
// exclusive except for the last bucket, which is inclusive of
// 1.0. MeanForecast is the average stated probability of the
// observations that fell in this bucket; MeanOutcome is the
// fraction of those that were hits. Count lets the caller draw
// the diagram with bar widths proportional to sample size.
type Bucket struct {
	LowerBound   float64 `json:"lowerBound"`
	UpperBound   float64 `json:"upperBound"`
	Count        int     `json:"count"`
	MeanForecast float64 `json:"meanForecast"`
	MeanOutcome  float64 `json:"meanOutcome"`
}

// Aggregate is the summary report produced by Tracker.Snapshot
// (or by the standalone Aggregate function for one-off
// computations). All scores honour the caller-supplied bucket
// edges; if none are supplied we default to deciles.
type Aggregate struct {
	AgentID     string    `json:"agentId,omitempty"`
	SampleCount int       `json:"sampleCount"`
	BrierScore  float64   `json:"brierScore"`
	ECE         float64   `json:"ece"`
	MeanForecast float64  `json:"meanForecast"`
	MeanOutcome  float64  `json:"meanOutcome"`
	Buckets      []Bucket `json:"buckets,omitempty"`
}

// DefaultBucketEdges is the reference 10-bucket schedule.
// Provided so callers don't have to memorise the convention.
var DefaultBucketEdges = []float64{0.0, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0}

// Tracker is an in-memory aggregator over a stream of
// Forecasts. Safe for concurrent Record calls.
type Tracker struct {
	mu        sync.RWMutex
	byAgent   map[string][]Forecast
	maxPerKey int
}

// NewTracker creates an empty Tracker. maxPerKey caps the per-
// agent buffer so a long-running aggregator doesn't grow
// unbounded; pass 0 for "no cap". When the cap is hit, the
// oldest forecast is evicted (FIFO) so the per-agent ECE/Brier
// stays a "rolling window" rather than a lifetime average.
func NewTracker(maxPerKey int) *Tracker {
	return &Tracker{byAgent: make(map[string][]Forecast), maxPerKey: maxPerKey}
}

// Record appends one forecast. Confidence is clamped to [0,1];
// Outcome is normalised to {0,1}. Empty AgentID is permitted —
// global aggregation is also a valid use case.
func (t *Tracker) Record(f Forecast) {
	if t == nil {
		return
	}
	clean := Forecast{
		AgentID:    f.AgentID,
		Confidence: clamp01(f.Confidence),
		Outcome:    binarise(f.Outcome),
		At:         f.At,
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	buf := t.byAgent[clean.AgentID]
	buf = append(buf, clean)
	if t.maxPerKey > 0 && len(buf) > t.maxPerKey {
		// Drop the oldest entry. Doing this with a slice copy
		// is O(n) but n is bounded by maxPerKey (typically
		// ≤ 1000), so this is still cheap.
		buf = buf[len(buf)-t.maxPerKey:]
	}
	t.byAgent[clean.AgentID] = buf
}

// Snapshot computes the Aggregate for one agent. Returns an
// empty Aggregate (SampleCount==0) when the agent has no
// recorded forecasts. Pass an empty agentID to aggregate across
// all agents in the tracker.
func (t *Tracker) Snapshot(agentID string, edges []float64) Aggregate {
	if t == nil {
		return Aggregate{AgentID: agentID}
	}
	t.mu.RLock()
	defer t.mu.RUnlock()

	if agentID == "" {
		var all []Forecast
		for _, v := range t.byAgent {
			all = append(all, v...)
		}
		return Aggregate{AgentID: ""}.merge(computeAggregate(all, edges))
	}
	return Aggregate{AgentID: agentID}.merge(computeAggregate(t.byAgent[agentID], edges))
}

// AgentIDs returns the sorted list of agent ids tracked. Useful
// for an admin panel that wants to render a table of every
// agent's calibration in one go.
func (t *Tracker) AgentIDs() []string {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	ids := make([]string, 0, len(t.byAgent))
	for id := range t.byAgent {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Aggregate computes the report for an arbitrary slice. Useful
// for batch jobs that load forecasts from the database in bulk
// without going through a Tracker. Invariant: an empty input
// returns SampleCount==0 and zero scores.
func ComputeAggregate(forecasts []Forecast, edges []float64) Aggregate {
	return computeAggregate(forecasts, edges)
}

func computeAggregate(forecasts []Forecast, edges []float64) Aggregate {
	if len(forecasts) == 0 {
		return Aggregate{}
	}
	if len(edges) < 2 {
		edges = DefaultBucketEdges
	}
	if !isMonotonic(edges) {
		// Misconfigured caller — fall back to defaults rather
		// than panic. We could panic instead but a periodic
		// metrics emitter shouldn't be able to crash on
		// pathological input.
		edges = DefaultBucketEdges
	}

	bucketCount := len(edges) - 1
	bucketSum := make([]float64, bucketCount)
	bucketHits := make([]float64, bucketCount)
	bucketN := make([]int, bucketCount)

	var brierSum, fcSum, hitSum float64
	for _, f := range forecasts {
		// Brier: (forecast - outcome)²
		diff := f.Confidence - f.Outcome
		brierSum += diff * diff
		fcSum += f.Confidence
		hitSum += f.Outcome

		// Place into a bucket. searchBucket returns the index
		// for which edges[i] <= confidence < edges[i+1] (with
		// the last bucket being closed on the upper end).
		idx := searchBucket(edges, f.Confidence)
		bucketSum[idx] += f.Confidence
		bucketHits[idx] += f.Outcome
		bucketN[idx]++
	}
	n := len(forecasts)

	out := Aggregate{
		SampleCount:  n,
		BrierScore:   brierSum / float64(n),
		MeanForecast: fcSum / float64(n),
		MeanOutcome:  hitSum / float64(n),
	}
	out.Buckets = make([]Bucket, 0, bucketCount)
	var ece float64
	for i := 0; i < bucketCount; i++ {
		if bucketN[i] == 0 {
			continue
		}
		mf := bucketSum[i] / float64(bucketN[i])
		mo := bucketHits[i] / float64(bucketN[i])
		out.Buckets = append(out.Buckets, Bucket{
			LowerBound:   edges[i],
			UpperBound:   edges[i+1],
			Count:        bucketN[i],
			MeanForecast: mf,
			MeanOutcome:  mo,
		})
		ece += (float64(bucketN[i]) / float64(n)) * math.Abs(mf-mo)
	}
	out.ECE = ece
	return out
}

// merge stamps an Aggregate's AgentID through any subsequent
// recompute step. Returning by value keeps Snapshot's caller
// from accidentally mutating the tracker's internal state.
func (a Aggregate) merge(other Aggregate) Aggregate {
	other.AgentID = a.AgentID
	return other
}

// Validate checks an Aggregate for impossible values, useful in
// tests and for guarding the wire export of a snapshot. Returns
// an error for: out-of-range Brier (>1), out-of-range ECE (>1),
// or buckets with MeanForecast / MeanOutcome outside [0,1].
func (a Aggregate) Validate() error {
	if a.BrierScore < 0 || a.BrierScore > 1 {
		return fmt.Errorf("calibration: BrierScore out of range: %v", a.BrierScore)
	}
	if a.ECE < 0 || a.ECE > 1 {
		return fmt.Errorf("calibration: ECE out of range: %v", a.ECE)
	}
	for _, b := range a.Buckets {
		if b.MeanForecast < 0 || b.MeanForecast > 1 {
			return fmt.Errorf("calibration: bucket MeanForecast out of range: %v", b.MeanForecast)
		}
		if b.MeanOutcome < 0 || b.MeanOutcome > 1 {
			return fmt.Errorf("calibration: bucket MeanOutcome out of range: %v", b.MeanOutcome)
		}
	}
	return nil
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func binarise(v float64) float64 {
	if v >= 0.5 {
		return 1
	}
	return 0
}

func isMonotonic(edges []float64) bool {
	for i := 1; i < len(edges); i++ {
		if edges[i] <= edges[i-1] {
			return false
		}
	}
	return true
}

func searchBucket(edges []float64, v float64) int {
	// Edges has bucketCount+1 entries. We want the largest i
	// such that edges[i] <= v. The last bucket is closed on
	// the right (so v=1.0 lands in bucket bucketCount-1, not in
	// bucketCount).
	if v >= edges[len(edges)-1] {
		return len(edges) - 2
	}
	idx := sort.SearchFloat64s(edges, v)
	if idx > 0 && edges[idx] > v {
		return idx - 1
	}
	if idx == 0 {
		return 0
	}
	return idx
}
