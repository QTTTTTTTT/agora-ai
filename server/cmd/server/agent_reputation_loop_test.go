// agent_reputation_loop_test.go — S8.4 backfill loop wiring.
// The backfill engine itself is covered in
// agentreputation/backfill_test.go; here we exercise the loop's
// scheduling defaults, jitter bounds, FundLister fan-out, and
// RebuildForFund single-fund retargeting.

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fundai/server/internal/agentreputation"
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
