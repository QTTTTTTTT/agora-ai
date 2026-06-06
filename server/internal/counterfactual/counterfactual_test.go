package counterfactual

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
)

type fakeReplayer struct {
	answers map[string]float64 // input id → cf alpha
	failOn  string
	calls   int32
}

func (f *fakeReplayer) CounterfactualAlpha(ctx context.Context, planID string, missing Input) (float64, bool, error) {
	atomic.AddInt32(&f.calls, 1)
	if missing.ID == f.failOn {
		return 0, false, errors.New("synthetic failure")
	}
	v, ok := f.answers[missing.ID]
	return v, ok, nil
}

func TestRunComputesContribution(t *testing.T) {
	r := &fakeReplayer{answers: map[string]float64{
		"L1": 0.01,
		"L2": 0.04,
	}}
	inputs := []Input{
		{Kind: InputLesson, ID: "L1"},
		{Kind: InputLesson, ID: "L2"},
	}
	got, err := Run(context.Background(), "p1", 0.05, inputs, r, DefaultConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.RealisedAlpha != 0.05 {
		t.Errorf("RealisedAlpha: got %v, want 0.05", got.RealisedAlpha)
	}
	if len(got.Attributions) != 2 {
		t.Fatalf("Attributions: got %d, want 2", len(got.Attributions))
	}
	// Sorted by contribution descending: L1 with cf 0.01 has
	// 0.04 contribution; L2 with cf 0.04 has 0.01 contribution.
	if got.Attributions[0].Input.ID != "L1" {
		t.Errorf("first attribution: got %q, want L1", got.Attributions[0].Input.ID)
	}
	if math.Abs(got.Attributions[0].Contribution-0.04) > 1e-9 {
		t.Errorf("L1 contribution: got %v, want 0.04", got.Attributions[0].Contribution)
	}
}

func TestRunHandlesUnavailable(t *testing.T) {
	r := &fakeReplayer{answers: map[string]float64{
		"L1": 0.01,
		// L2 not in answers — replayer returns ok=false.
	}}
	inputs := []Input{
		{Kind: InputLesson, ID: "L1"},
		{Kind: InputLesson, ID: "L2"},
	}
	got, err := Run(context.Background(), "p1", 0.05, inputs, r, DefaultConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var l1, l2 Attribution
	for _, a := range got.Attributions {
		if a.Input.ID == "L1" {
			l1 = a
		}
		if a.Input.ID == "L2" {
			l2 = a
		}
	}
	if l1.Confidence == "unavailable" {
		t.Errorf("L1 should be available")
	}
	if l2.Confidence != "unavailable" {
		t.Errorf("L2 confidence: got %q, want unavailable", l2.Confidence)
	}
}

func TestRunHandlesError(t *testing.T) {
	r := &fakeReplayer{
		answers: map[string]float64{"L1": 0.01},
		failOn:  "L2",
	}
	inputs := []Input{
		{Kind: InputLesson, ID: "L1"},
		{Kind: InputLesson, ID: "L2"},
	}
	got, err := Run(context.Background(), "p1", 0.05, inputs, r, DefaultConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, a := range got.Attributions {
		if a.Input.ID == "L2" && a.Confidence != "unavailable" {
			t.Errorf("L2 should be unavailable on replayer error, got %q",
				a.Confidence)
		}
	}
}

func TestRunRejectsBadArgs(t *testing.T) {
	r := &fakeReplayer{}
	if _, err := Run(context.Background(), "", 0, nil, r, DefaultConfig()); err == nil {
		t.Errorf("empty planID should error")
	}
	if _, err := Run(context.Background(), "p1", 0, nil, nil, DefaultConfig()); err == nil {
		t.Errorf("nil replayer should error")
	}
}

func TestRunWithEmptyInputs(t *testing.T) {
	r := &fakeReplayer{}
	got, err := Run(context.Background(), "p1", 0.04, nil, r, DefaultConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got.Attributions) != 0 {
		t.Errorf("Attributions for empty inputs: got %d, want 0",
			len(got.Attributions))
	}
}

func TestNetContributionExcludesUnavailable(t *testing.T) {
	r := &fakeReplayer{answers: map[string]float64{
		"L1": 0.01,
	}}
	inputs := []Input{
		{Kind: InputLesson, ID: "L1"},
		{Kind: InputLesson, ID: "L2"},
	}
	got, err := Run(context.Background(), "p1", 0.05, inputs, r, DefaultConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if math.Abs(got.NetContribution-0.04) > 1e-9 {
		t.Errorf("NetContribution: got %v, want 0.04", got.NetContribution)
	}
}

func TestConfidenceClassification(t *testing.T) {
	cases := []struct {
		c    float64
		want string
	}{
		{0.01, "high"},
		{-0.01, "high"},
		{0.001, "low"},
		{0, "low"},
	}
	for _, tc := range cases {
		got := confidenceFor(tc.c)
		if got != tc.want {
			t.Errorf("confidenceFor(%v): got %q, want %q", tc.c, got, tc.want)
		}
	}
}

func TestNullReplayerAlwaysUnavailable(t *testing.T) {
	got, ok, err := NullReplayer{}.CounterfactualAlpha(context.Background(), "p", Input{})
	if err != nil || ok || got != 0 {
		t.Errorf("NullReplayer: got (%v, %v, %v), want (0, false, nil)", got, ok, err)
	}
}

func TestHistoricalBaselineReplayer(t *testing.T) {
	r := HistoricalBaselineReplayer{
		BaselineByInput: map[string]float64{
			"lesson/L1": 0.02,
			"skill/S1":  0.01,
		},
	}
	got, ok, err := r.CounterfactualAlpha(context.Background(), "p1", Input{Kind: InputLesson, ID: "L1"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ok || got != 0.02 {
		t.Errorf("got (%v, %v), want (0.02, true)", got, ok)
	}
	_, ok, _ = r.CounterfactualAlpha(context.Background(), "p1", Input{Kind: InputLesson, ID: "missing"})
	if ok {
		t.Errorf("missing key should return ok=false")
	}
}

func TestRunRespectsConcurrencyCap(t *testing.T) {
	r := &fakeReplayer{
		answers: map[string]float64{
			"a": 0, "b": 0, "c": 0, "d": 0, "e": 0,
		},
	}
	inputs := []Input{
		{Kind: InputLesson, ID: "a"},
		{Kind: InputLesson, ID: "b"},
		{Kind: InputLesson, ID: "c"},
		{Kind: InputLesson, ID: "d"},
		{Kind: InputLesson, ID: "e"},
	}
	cfg := DefaultConfig()
	cfg.MaxConcurrency = 2
	_, err := Run(context.Background(), "p", 0.0, inputs, r, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if atomic.LoadInt32(&r.calls) != 5 {
		t.Errorf("calls: got %d, want 5", atomic.LoadInt32(&r.calls))
	}
}
