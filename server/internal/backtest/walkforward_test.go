package backtest

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/decision"
	"github.com/fundai/server/internal/ohlc"
)

// PlanWalkForward: anchored 4-fold split tiles the window exactly.
func TestPlanWalkForwardAnchored4Folds(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(400 * 24 * time.Hour) // ~13 months
	folds, err := PlanWalkForward(start, end, WalkForwardSpec{
		NumFolds: 4, TrainRatio: 0.5, Mode: WalkForwardAnchored,
	})
	if err != nil {
		t.Fatalf("PlanWalkForward: %v", err)
	}
	if len(folds) != 4 {
		t.Fatalf("got %d folds, want 4", len(folds))
	}
	// Test windows must be contiguous, non-overlapping, and tile [start, end].
	if !folds[0].TestStart.Equal(start) {
		t.Errorf("fold[0].TestStart = %v, want %v", folds[0].TestStart, start)
	}
	if !folds[len(folds)-1].TestEnd.Equal(end) {
		t.Errorf("last fold.TestEnd = %v, want %v", folds[len(folds)-1].TestEnd, end)
	}
	for i := 1; i < len(folds); i++ {
		if !folds[i].TestStart.Equal(folds[i-1].TestEnd) {
			t.Errorf("fold[%d].TestStart (%v) != fold[%d].TestEnd (%v)",
				i, folds[i].TestStart, i-1, folds[i-1].TestEnd)
		}
	}
	// Anchored: train always starts at global start.
	for i, f := range folds {
		if !f.TrainStart.Equal(start) {
			t.Errorf("fold[%d].TrainStart = %v, want %v", i, f.TrainStart, start)
		}
	}
	// First fold's train side is degenerate (no lookback yet) so
	// TrainStart == TrainEnd == TestStart == start.
	if !folds[0].TrainStart.Equal(folds[0].TrainEnd) {
		t.Errorf("fold[0] train should be empty (start == end), got [%v, %v]", folds[0].TrainStart, folds[0].TrainEnd)
	}
}

// Rolling mode: train + test windows are equal length, both slide.
func TestPlanWalkForwardRollingHasSlidingTrain(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(400 * 24 * time.Hour)
	folds, err := PlanWalkForward(start, end, WalkForwardSpec{
		NumFolds: 4, TrainRatio: 0.5, Mode: WalkForwardRolling,
	})
	if err != nil {
		t.Fatalf("PlanWalkForward: %v", err)
	}
	if len(folds) != 4 {
		t.Fatalf("got %d folds, want 4", len(folds))
	}
	// trainEnd of fold[i] should equal testStart of fold[i].
	for i, f := range folds {
		if !f.TrainEnd.Equal(f.TestStart) {
			t.Errorf("fold[%d] trainEnd != testStart", i)
		}
	}
	// With ratio=0.5, train length ≈ stepDur (so train+test
	// covers two stepDur each fold). The last fold's train
	// reaches one stepDur back from its test start.
	stepDur := end.Sub(start) / 4
	last := folds[len(folds)-1]
	wantTrainStart := last.TestStart.Add(-stepDur)
	if last.TrainStart.Before(wantTrainStart.Add(-time.Hour)) || last.TrainStart.After(wantTrainStart.Add(time.Hour)) {
		t.Errorf("last fold trainStart = %v, want ≈ %v", last.TrainStart, wantTrainStart)
	}
}

// trainRatio=0 in rolling mode collapses train side to empty,
// fold = pure test slice.
func TestPlanWalkForwardRollingZeroTrainRatio(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(40 * 24 * time.Hour)
	folds, err := PlanWalkForward(start, end, WalkForwardSpec{
		NumFolds: 4, TrainRatio: 0, Mode: WalkForwardRolling,
	})
	if err != nil {
		t.Fatalf("PlanWalkForward: %v", err)
	}
	for i, f := range folds {
		if !f.TrainStart.Equal(f.TrainEnd) {
			t.Errorf("fold[%d] train should be empty when rolling+ratio=0, got [%v,%v]", i, f.TrainStart, f.TrainEnd)
		}
	}
}

// Mode is normalised to anchored when empty / unknown.
func TestPlanWalkForwardDefaultsToAnchored(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(40 * 24 * time.Hour)
	folds, err := PlanWalkForward(start, end, WalkForwardSpec{NumFolds: 3, TrainRatio: 0.5, Mode: ""})
	if err != nil {
		t.Fatalf("PlanWalkForward: %v", err)
	}
	for _, f := range folds {
		if !f.TrainStart.Equal(start) {
			t.Errorf("empty mode should behave as anchored; trainStart = %v, want %v", f.TrainStart, start)
		}
	}
}

// Invalid inputs.
func TestPlanWalkForwardRejectsInvalid(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(100 * 24 * time.Hour)
	cases := []struct {
		name string
		s, e time.Time
		spec WalkForwardSpec
	}{
		{"reverse window", end, start, WalkForwardSpec{NumFolds: 3}},
		{"fold=1", start, end, WalkForwardSpec{NumFolds: 1}},
		{"fold>max", start, end, WalkForwardSpec{NumFolds: MaxWalkForwardFolds + 1}},
		{"negative ratio", start, end, WalkForwardSpec{NumFolds: 3, TrainRatio: -0.1}},
		{"ratio=1.0", start, end, WalkForwardSpec{NumFolds: 3, TrainRatio: 1.0}},
		{"window too short", start, start.Add(time.Hour), WalkForwardSpec{NumFolds: 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PlanWalkForward(tc.s, tc.e, tc.spec)
			if !errors.Is(err, ErrWalkForwardInvalid) {
				t.Errorf("want ErrWalkForwardInvalid, got %v", err)
			}
		})
	}
}

// foldReturn handles zero initial gracefully.
func TestFoldReturnGuardsZero(t *testing.T) {
	if got := foldReturn(0, 100); got != 0 {
		t.Errorf("got %f, want 0", got)
	}
	if got := foldReturn(100, 110); math.Abs(got-0.1) > 1e-9 {
		t.Errorf("got %f, want 0.1", got)
	}
}

// aggregateFoldReturns reports mean/worst/best.
func TestAggregateFoldReturns(t *testing.T) {
	folds := []FoldRunSummary{
		{Return: 0.1}, {Return: -0.2}, {Return: 0.3},
	}
	mean, worst, best := aggregateFoldReturns(folds)
	if math.Abs(mean-(0.1-0.2+0.3)/3) > 1e-9 {
		t.Errorf("mean = %f", mean)
	}
	if worst != -0.2 {
		t.Errorf("worst = %f, want -0.2", worst)
	}
	if best != 0.3 {
		t.Errorf("best = %f, want 0.3", best)
	}
}

// aggregateFoldReturns: empty slice returns zeros (no Inf leak).
func TestAggregateFoldReturnsEmpty(t *testing.T) {
	mean, worst, best := aggregateFoldReturns(nil)
	if mean != 0 || worst != 0 || best != 0 {
		t.Errorf("non-zero on empty: %f %f %f", mean, worst, best)
	}
}

// Engine integration: WalkForwardRunner with a synthetic OHLC
// fetcher + deterministic fallback engine produces N folds, each
// with non-empty NAV, stitched into one continuous curve.
func TestWalkForwardRunnerEndToEnd(t *testing.T) {
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC) // Monday
	// 40 calendar days = ~28 trading days
	end := start.Add(40 * 24 * time.Hour)
	fetcher := newSyntheticFetcher(start, end, []string{"AAPL"}, 100.0)
	inner := &Runner{OHLC: fetcher, Decide: decision.FallbackEngine{}}
	runner := &WalkForwardRunner{Inner: inner}

	req := Request{
		FundID:      "fund-1",
		Symbols:     []string{"AAPL"},
		Start:       start,
		End:         end,
		InitialCash: 10_000,
		EngineKind:  "fallback",
		WalkForward: &WalkForwardSpec{
			NumFolds:   4,
			TrainRatio: 0.5,
			Mode:       WalkForwardAnchored,
		},
	}
	progress := &Progress{}
	result, err := runner.Run(context.Background(), req, progress)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.WalkForward == nil {
		t.Fatal("expected WalkForward result")
	}
	if len(result.WalkForward.Folds) != 4 {
		t.Errorf("got %d fold summaries, want 4", len(result.WalkForward.Folds))
	}
	if len(result.WalkForward.FoldBoundaries) != 4 {
		t.Errorf("got %d boundaries, want 4", len(result.WalkForward.FoldBoundaries))
	}
	if result.WalkForward.FoldBoundaries[0] != 0 {
		t.Errorf("first boundary should be 0, got %d", result.WalkForward.FoldBoundaries[0])
	}
	// Boundaries strictly increase.
	for i := 1; i < len(result.WalkForward.FoldBoundaries); i++ {
		if result.WalkForward.FoldBoundaries[i] < result.WalkForward.FoldBoundaries[i-1] {
			t.Errorf("boundaries not monotonic: %v", result.WalkForward.FoldBoundaries)
		}
	}
	// Stitched NAV should be non-empty and end-to-end portfolio
	// continuous: result.FinalNav == last point of NAV curve.
	if len(result.NavCurve) == 0 {
		t.Fatal("empty NavCurve")
	}
	if math.Abs(result.FinalNav-result.NavCurve[len(result.NavCurve)-1].Nav) > 1e-6 {
		t.Errorf("FinalNav %f != last NAV %f", result.FinalNav, result.NavCurve[len(result.NavCurve)-1].Nav)
	}
	// OOSReturn == (FinalNav - InitialCash) / InitialCash
	expected := (result.FinalNav - 10_000) / 10_000
	if math.Abs(result.WalkForward.OOSReturn-expected) > 1e-6 {
		t.Errorf("OOSReturn = %f, want %f", result.WalkForward.OOSReturn, expected)
	}
}

// When no WalkForward spec is set, the runner transparently
// delegates to the inner Runner.
func TestWalkForwardRunnerDelegatesWithoutSpec(t *testing.T) {
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	end := start.Add(20 * 24 * time.Hour)
	fetcher := newSyntheticFetcher(start, end, []string{"AAPL"}, 100.0)
	inner := &Runner{OHLC: fetcher, Decide: decision.FallbackEngine{}}
	runner := &WalkForwardRunner{Inner: inner}

	req := Request{
		FundID:      "fund-1",
		Symbols:     []string{"AAPL"},
		Start:       start,
		End:         end,
		InitialCash: 10_000,
		EngineKind:  "fallback",
		// No WalkForward set → fall through to inner Runner.
	}
	result, err := runner.Run(context.Background(), req, &Progress{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.WalkForward != nil {
		t.Errorf("expected no WalkForward result when spec is nil")
	}
}

// Cancellation between folds is honoured.
func TestWalkForwardRunnerCancellation(t *testing.T) {
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	end := start.Add(40 * 24 * time.Hour)
	fetcher := newSyntheticFetcher(start, end, []string{"AAPL"}, 100.0)
	inner := &Runner{OHLC: fetcher, Decide: decision.FallbackEngine{}}
	runner := &WalkForwardRunner{Inner: inner}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel
	_, err := runner.Run(ctx, Request{
		FundID: "fund-1", Symbols: []string{"AAPL"},
		Start: start, End: end, InitialCash: 10_000,
		EngineKind:  "fallback",
		WalkForward: &WalkForwardSpec{NumFolds: 4, TrainRatio: 0.5},
	}, &Progress{})
	if !errors.Is(err, ErrCancelled) {
		t.Errorf("want ErrCancelled, got %v", err)
	}
}

// helpers ------------------------------------------------------

// syntheticFetcher returns flat-line bars for a given window so
// the runner can exercise its loop without any external service.
// We use the same trick the existing tests use.
type syntheticFetcher struct {
	bars map[string][]ohlc.Bar
}

func (s *syntheticFetcher) Fetch(_ context.Context, req ohlc.FetchRequest) ([]ohlc.Bar, error) {
	if b, ok := s.bars[strings.ToUpper(req.Symbol)]; ok {
		out := make([]ohlc.Bar, len(b))
		copy(out, b)
		return out, nil
	}
	return nil, ohlc.ErrNoData
}

// newSyntheticFetcher constructs a fetcher with one synthetic
// price series per symbol — flat at `price`, daily bars in
// [start, end].
func newSyntheticFetcher(start, end time.Time, symbols []string, price float64) *syntheticFetcher {
	out := &syntheticFetcher{bars: map[string][]ohlc.Bar{}}
	for _, sym := range symbols {
		bars := []ohlc.Bar{}
		for t := start; !t.After(end); t = t.Add(24 * time.Hour) {
			// Skip weekends so the runner's trading-day filter
			// sees a realistic series.
			if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
				continue
			}
			bars = append(bars, ohlc.Bar{
				Time: t, Open: price, High: price * 1.005, Low: price * 0.995, Close: price, Volume: 1_000_000,
			})
		}
		out.bars[strings.ToUpper(sym)] = bars
	}
	return out
}
