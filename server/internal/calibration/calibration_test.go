package calibration

import (
	"math"
	"testing"
	"time"
)

func TestPerfectCalibrator(t *testing.T) {
	// Forecasts equal outcomes — BrierScore must be 0, ECE must
	// be 0, and the reliability diagram has its mean(forecast) =
	// mean(outcome) in every bucket that has data.
	tracker := NewTracker(0)
	for _, c := range []float64{0.05, 0.15, 0.25, 0.35, 0.45, 0.55, 0.65, 0.75, 0.85, 0.95} {
		// 100 forecasts at each confidence level, with the
		// outcome split exactly proportional to that confidence
		// over the long run. We match the closest binary outcome
		// for the 100 obs (e.g. 0.45 → 45 hits, 55 misses).
		hits := int(math.Round(c * 100))
		misses := 100 - hits
		for i := 0; i < hits; i++ {
			tracker.Record(Forecast{AgentID: "perfect", Confidence: c, Outcome: 1, At: time.Now()})
		}
		for i := 0; i < misses; i++ {
			tracker.Record(Forecast{AgentID: "perfect", Confidence: c, Outcome: 0, At: time.Now()})
		}
	}

	agg := tracker.Snapshot("perfect", DefaultBucketEdges)
	if err := agg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if agg.SampleCount != 1000 {
		t.Errorf("SampleCount: got %d, want 1000", agg.SampleCount)
	}
	if math.Abs(agg.ECE) > 0.05 {
		t.Errorf("ECE for perfect calibrator: got %v, want ~0", agg.ECE)
	}
	if agg.BrierScore > 0.25 {
		t.Errorf("BrierScore: got %v, want ≤ 0.25 for a perfect calibrator", agg.BrierScore)
	}
}

func TestOverconfidentAgent(t *testing.T) {
	// Always reports 0.95 confidence, but only hits 50% of the
	// time. ECE should be near 0.45 (gap between 0.95 mean
	// forecast and 0.5 mean outcome in the [0.9, 1.0] bucket).
	tracker := NewTracker(0)
	for i := 0; i < 1000; i++ {
		outcome := float64(i % 2)
		tracker.Record(Forecast{AgentID: "boastful", Confidence: 0.95, Outcome: outcome})
	}
	agg := tracker.Snapshot("boastful", DefaultBucketEdges)
	if err := agg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if agg.SampleCount != 1000 {
		t.Errorf("SampleCount: got %d, want 1000", agg.SampleCount)
	}
	if math.Abs(agg.MeanForecast-0.95) > 1e-9 {
		t.Errorf("MeanForecast: got %v, want 0.95", agg.MeanForecast)
	}
	if math.Abs(agg.MeanOutcome-0.5) > 1e-9 {
		t.Errorf("MeanOutcome: got %v, want 0.5", agg.MeanOutcome)
	}
	if agg.ECE < 0.4 || agg.ECE > 0.5 {
		t.Errorf("ECE for overconfident agent: got %v, want ~0.45", agg.ECE)
	}
}

func TestRandomAgentAtPointFive(t *testing.T) {
	// Always reports 0.5; outcomes are 50/50. BrierScore must
	// equal 0.25 (the maximum-uncertainty Brier value).
	tracker := NewTracker(0)
	for i := 0; i < 1000; i++ {
		outcome := float64(i % 2)
		tracker.Record(Forecast{AgentID: "fair_coin", Confidence: 0.5, Outcome: outcome})
	}
	agg := tracker.Snapshot("fair_coin", DefaultBucketEdges)
	if math.Abs(agg.BrierScore-0.25) > 1e-9 {
		t.Errorf("BrierScore: got %v, want 0.25", agg.BrierScore)
	}
	if math.Abs(agg.ECE) > 0.05 {
		t.Errorf("ECE for fair coin: got %v, want ~0", agg.ECE)
	}
}

func TestRecordClampsAndBinarises(t *testing.T) {
	// Confidence > 1 should clamp to 1; outcome > 0.5 → 1; NaN
	// confidence → 0.
	tracker := NewTracker(0)
	tracker.Record(Forecast{Confidence: 1.5, Outcome: 0.7})
	tracker.Record(Forecast{Confidence: -2, Outcome: 0.3})
	tracker.Record(Forecast{Confidence: math.NaN(), Outcome: 1})
	agg := tracker.Snapshot("", DefaultBucketEdges)
	if agg.SampleCount != 3 {
		t.Errorf("SampleCount: got %d, want 3", agg.SampleCount)
	}
	for _, b := range agg.Buckets {
		if b.MeanForecast < 0 || b.MeanForecast > 1 {
			t.Errorf("MeanForecast out of range: %v", b.MeanForecast)
		}
		if b.MeanOutcome < 0 || b.MeanOutcome > 1 {
			t.Errorf("MeanOutcome out of range: %v", b.MeanOutcome)
		}
	}
}

func TestMaxPerKeyEvictsOldest(t *testing.T) {
	tracker := NewTracker(3)
	for i := 0; i < 5; i++ {
		tracker.Record(Forecast{AgentID: "a", Confidence: float64(i) / 10, Outcome: 1})
	}
	agg := tracker.Snapshot("a", DefaultBucketEdges)
	if agg.SampleCount != 3 {
		t.Errorf("SampleCount: got %d, want 3 (cap exceeded)", agg.SampleCount)
	}
	// The most recent three forecasts have confidences 0.2,
	// 0.3, 0.4 — mean should be ~0.3.
	if math.Abs(agg.MeanForecast-0.3) > 1e-9 {
		t.Errorf("MeanForecast after eviction: got %v, want 0.3", agg.MeanForecast)
	}
}

func TestEmptyTrackerSnapshotIsZero(t *testing.T) {
	tracker := NewTracker(0)
	agg := tracker.Snapshot("any", DefaultBucketEdges)
	if agg.SampleCount != 0 {
		t.Errorf("SampleCount: got %d, want 0", agg.SampleCount)
	}
	if agg.BrierScore != 0 {
		t.Errorf("BrierScore: got %v, want 0", agg.BrierScore)
	}
}

func TestNilTrackerIsNoOp(t *testing.T) {
	var tracker *Tracker
	tracker.Record(Forecast{Confidence: 0.5, Outcome: 1})
	agg := tracker.Snapshot("any", DefaultBucketEdges)
	if agg.SampleCount != 0 {
		t.Errorf("nil tracker should produce zero aggregate, got SampleCount=%d", agg.SampleCount)
	}
}

func TestSnapshotEmptyAgentIDAggregatesAll(t *testing.T) {
	tracker := NewTracker(0)
	tracker.Record(Forecast{AgentID: "a", Confidence: 0.7, Outcome: 1})
	tracker.Record(Forecast{AgentID: "b", Confidence: 0.3, Outcome: 0})
	tracker.Record(Forecast{AgentID: "c", Confidence: 0.9, Outcome: 1})

	all := tracker.Snapshot("", DefaultBucketEdges)
	if all.SampleCount != 3 {
		t.Errorf("global SampleCount: got %d, want 3", all.SampleCount)
	}

	a := tracker.Snapshot("a", DefaultBucketEdges)
	if a.SampleCount != 1 {
		t.Errorf("agent a SampleCount: got %d, want 1", a.SampleCount)
	}
}

func TestAgentIDsSorted(t *testing.T) {
	tracker := NewTracker(0)
	tracker.Record(Forecast{AgentID: "c", Confidence: 0.1, Outcome: 0})
	tracker.Record(Forecast{AgentID: "a", Confidence: 0.5, Outcome: 1})
	tracker.Record(Forecast{AgentID: "b", Confidence: 0.5, Outcome: 1})
	got := tracker.AgentIDs()
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("at %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSearchBucketBoundaries(t *testing.T) {
	edges := []float64{0.0, 0.5, 1.0}
	cases := map[float64]int{
		0.0:  0,
		0.25: 0,
		0.49: 0,
		0.5:  1,
		0.75: 1,
		1.0:  1,
	}
	for v, want := range cases {
		got := searchBucket(edges, v)
		if got != want {
			t.Errorf("searchBucket(edges, %v) = %d, want %d", v, got, want)
		}
	}
}

func TestComputeAggregateNoEdgesUsesDefault(t *testing.T) {
	got := ComputeAggregate([]Forecast{
		{Confidence: 0.85, Outcome: 1},
		{Confidence: 0.15, Outcome: 0},
	}, nil)
	if got.SampleCount != 2 {
		t.Errorf("SampleCount: got %d, want 2", got.SampleCount)
	}
	if len(got.Buckets) == 0 {
		t.Errorf("expected at least one bucket with default edges")
	}
}
