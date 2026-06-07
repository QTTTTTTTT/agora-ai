// agent_reputation_loop_test.go — S8.4 backfill loop wiring.
// The backfill engine itself is covered in
// agentreputation/backfill_test.go; here we exercise the loop's
// scheduling defaults, jitter bounds, FundLister fan-out, and
// RebuildForFund single-fund retargeting.

package main

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/fundai/server/internal/agentreputation"
	"github.com/fundai/server/internal/ohlc"
)

type stubPanelSource struct {
	rows []agentreputation.PanelRow
}

func (s *stubPanelSource) ListPanelsForBackfill(_ context.Context, _ string, _ time.Time, _ int) ([]agentreputation.PanelRow, error) {
	return s.rows, nil
}

type stubDebateSource struct {
	rows []agentreputation.DebateRow
}

func (s *stubDebateSource) ListDebatesForBackfill(_ context.Context, _ string, _ time.Time, _ int) ([]agentreputation.DebateRow, error) {
	return s.rows, nil
}

func TestAgentReputationLoop_NilRepoReturnsNil(t *testing.T) {
	loop := newAgentReputationLoop(nil, nil, nil, nil, agentReputationLoopOptions{})
	if loop != nil {
		t.Errorf("expected nil loop, got %+v", loop)
	}
}

func TestAgentReputationLoop_Defaults(t *testing.T) {
	repo := agentreputation.NewRepo(nil) // exercise default normalisation only
	loop := newAgentReputationLoop(repo, nil, nil, nil, agentReputationLoopOptions{})
	if loop == nil {
		t.Fatal("expected non-nil loop")
	}
	if loop.opts.Interval <= 0 {
		t.Errorf("interval = %v", loop.opts.Interval)
	}
	if loop.opts.PerFundTimeout <= 0 {
		t.Errorf("per fund timeout = %v", loop.opts.PerFundTimeout)
	}
	if loop.opts.LookbackDays != 30 {
		t.Errorf("lookback = %d", loop.opts.LookbackDays)
	}
	if len(loop.opts.Horizons) != 3 {
		t.Errorf("horizons = %v", loop.opts.Horizons)
	}
	if loop.returns == nil {
		t.Error("returns fn defaulted to nil")
	}
}

func TestAgentReputationLoop_RebuildForFund_ListerErrorFallsThrough(t *testing.T) {
	repo := agentreputation.NewRepo(nil)
	loop := newAgentReputationLoop(repo, nil, nil, nil, agentReputationLoopOptions{
		FundLister: func(_ context.Context) ([]string, error) {
			return nil, errors.New("lister down")
		},
	})
	if loop == nil {
		t.Fatal("expected non-nil loop")
	}
	// Returns 0 and no error because lister error is logged + swallowed.
	n, err := loop.RebuildForFund(context.Background(), "")
	if err != nil {
		t.Errorf("rebuild err = %v", err)
	}
	if n != 0 {
		t.Errorf("rebuild n = %d", n)
	}
}

func TestAgentReputationLoop_RebuildForFund_RetargetsLister(t *testing.T) {
	repo := agentreputation.NewRepo(nil)
	called := []string{}
	loop := newAgentReputationLoop(repo, nil, nil, nil, agentReputationLoopOptions{
		FundLister: func(_ context.Context) ([]string, error) {
			called = append(called, "default")
			return []string{"a", "b"}, nil
		},
	})
	if loop == nil {
		t.Fatal("expected non-nil loop")
	}
	// Single-fund rebuild should not invoke the default lister.
	_, _ = loop.RebuildForFund(context.Background(), "only-this-fund")
	for _, c := range called {
		if c == "default" {
			t.Error("default lister should not have been invoked for single-fund rebuild")
		}
	}
}

func TestNullRealisedReturn(t *testing.T) {
	r, b, ok, err := nullRealisedReturn(context.Background(), "f1", "AAPL", time.Now(), 5)
	if err != nil || ok || r != 0 || b != 0 {
		t.Errorf("expected zeros+ok=false, got r=%v b=%v ok=%v err=%v", r, b, ok, err)
	}
}

func TestFormatHorizons(t *testing.T) {
	got := formatHorizons([]int{1, 5, 21})
	want := "1d,5d,21d"
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

// W16-2 — realisedReturnFromOHLC happy path. With a 5-day horizon
// and a clean rising series for both symbol and benchmark, the
// fetcher returns the exact decimal returns over the same window.
func TestRealisedReturnFromOHLC_HappyPath(t *testing.T) {
	asof := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	bars := func(start float64, slope float64, n int) []ohlc.Bar {
		out := make([]ohlc.Bar, n)
		for i := 0; i < n; i++ {
			out[i] = ohlc.Bar{
				Time:  asof.AddDate(0, 0, i-3),
				Close: start + slope*float64(i),
			}
		}
		return out
	}
	fetcher := &stubFetcher{bars: map[string][]ohlc.Bar{
		"AAPL": bars(100.0, 1.0, 30), // 100..129 across the window
		"SPY":  bars(50.0, 0.25, 30), // 50..57.25 across the window
	}}
	lookup := func(_ context.Context, _ string) (string, string, bool) {
		return "us_equity", "SPY", true
	}
	fn := realisedReturnFromOHLC(fetcher, lookup)
	realised, bench, ok, err := fn(context.Background(), "f1", "AAPL", asof, 5)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true on full coverage")
	}
	// Entry: bar at asof-0 (index 3, close=103). Exit: bar at
	// asof+5 (index 8, close=108). Realised = (108-103)/103.
	wantRealised := (108.0 - 103.0) / 103.0
	if math.Abs(realised-wantRealised) > 1e-9 {
		t.Errorf("realised = %v, want %v", realised, wantRealised)
	}
	// Benchmark entry close index 3 = 50.75; exit index 8 = 52.0.
	wantBench := (52.0 - 50.75) / 50.75
	if math.Abs(bench-wantBench) > 1e-9 {
		t.Errorf("bench = %v, want %v", bench, wantBench)
	}
}

// When the symbol fetch fails (no data) the helper must degrade to
// ok=false rather than returning bogus values or propagating the
// error — the loop's contract is that a per-row failure skips, not
// aborts.
func TestRealisedReturnFromOHLC_SilentlySkipsOnNoData(t *testing.T) {
	asof := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	fetcher := &stubFetcher{bars: map[string][]ohlc.Bar{
		"SPY": {{Time: asof, Close: 100.0}},
	}}
	lookup := func(_ context.Context, _ string) (string, string, bool) {
		return "us_equity", "SPY", true
	}
	fn := realisedReturnFromOHLC(fetcher, lookup)
	_, _, ok, err := fn(context.Background(), "f1", "GHOST", asof, 5)
	if err != nil {
		t.Errorf("err must be nil on missing-symbol skip, got %v", err)
	}
	if ok {
		t.Error("expected ok=false on missing-symbol skip")
	}
}

// Missing benchmark symbol from the lookup is treated as "no
// alpha measurement possible" — skip rather than emit a row with
// benchmarkReturn = 0 (which would silently print the alpha as
// realisedReturn alone).
func TestRealisedReturnFromOHLC_RequiresBenchmarkFromLookup(t *testing.T) {
	fetcher := &stubFetcher{}
	lookup := func(_ context.Context, _ string) (string, string, bool) {
		return "us_equity", "", true
	}
	fn := realisedReturnFromOHLC(fetcher, lookup)
	_, _, ok, _ := fn(context.Background(), "f1", "AAPL", time.Now(), 5)
	if ok {
		t.Error("expected ok=false when lookup returns empty benchmark")
	}
}

// nil fetcher / nil lookup degrade safely to ok=false. Defensive
// path so a partial wiring (only one of the two pieces present)
// behaves like the legacy nullRealisedReturn.
func TestRealisedReturnFromOHLC_NilDependenciesDegrade(t *testing.T) {
	fn1 := realisedReturnFromOHLC(nil, func(_ context.Context, _ string) (string, string, bool) { return "x", "Y", true })
	if _, _, ok, _ := fn1(context.Background(), "f1", "AAPL", time.Now(), 5); ok {
		t.Error("expected ok=false with nil fetcher")
	}
	fn2 := realisedReturnFromOHLC(&stubFetcher{}, nil)
	if _, _, ok, _ := fn2(context.Background(), "f1", "AAPL", time.Now(), 5); ok {
		t.Error("expected ok=false with nil lookup")
	}
}

// closeAtOrBefore returns the most recent close not after target;
// a target before every bar surfaces ok=false.
func TestCloseAtOrBefore(t *testing.T) {
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	bars := []ohlc.Bar{
		{Time: t0, Close: 100},
		{Time: t0.AddDate(0, 0, 1), Close: 101},
		{Time: t0.AddDate(0, 0, 2), Close: 102},
		{Time: t0.AddDate(0, 0, 5), Close: 110}, // weekend gap on day 3-4
	}
	got, ok := closeAtOrBefore(bars, t0.AddDate(0, 0, 3))
	if !ok || got != 102 {
		t.Errorf("weekend gap: got %v ok=%v, want 102 ok=true", got, ok)
	}
	got, ok = closeAtOrBefore(bars, t0)
	if !ok || got != 100 {
		t.Errorf("exact match: got %v ok=%v, want 100 ok=true", got, ok)
	}
	if _, ok := closeAtOrBefore(bars, t0.AddDate(0, 0, -10)); ok {
		t.Errorf("target before series should report ok=false")
	}
	if _, ok := closeAtOrBefore(nil, t0); ok {
		t.Errorf("nil bars should report ok=false")
	}
}
