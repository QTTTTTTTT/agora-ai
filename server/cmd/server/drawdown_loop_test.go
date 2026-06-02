package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fundai/server/internal/drawdown"
)

func TestDrawdownLoop_DefaultsApplied(t *testing.T) {
	loop := newDrawdownLoop(nil, nil, nil, nil, drawdownLoopOptions{
		JitterPct: 0.05,
	})
	if loop.opts.Interval != 5*time.Minute {
		t.Errorf("interval = %v, want 5m", loop.opts.Interval)
	}
	if loop.opts.PerFundTimeout != 30*time.Second {
		t.Errorf("per-fund timeout = %v, want 30s", loop.opts.PerFundTimeout)
	}
	if loop.opts.JitterPct != 0.05 {
		t.Errorf("jitter = %v, want 0.05", loop.opts.JitterPct)
	}
	if loop.engine == nil {
		t.Error("engine must default to drawdown.NewEngine")
	}
}

func TestDrawdownLoop_ExplicitZeroJitterPreserved(t *testing.T) {
	loop := newDrawdownLoop(nil, nil, nil, nil, drawdownLoopOptions{
		JitterPct: 0,
	})
	if loop.opts.JitterPct != 0 {
		t.Errorf("explicit zero jitter overridden: %v", loop.opts.JitterPct)
	}
}

func TestDrawdownLoop_NilBuilderIsNoop(t *testing.T) {
	loop := newDrawdownLoop(nil, nil, nil, nil, drawdownLoopOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	loop.Run(ctx)
}

func TestDrawdownLoop_RunOnce_NilFundLister(t *testing.T) {
	loop := newDrawdownLoop(&drawdown.Repo{}, &drawdownSnapshotBuilder{}, newServerMetrics(), nil, drawdownLoopOptions{})
	loop.opts.FundLister = nil
	if err := loop.runOnce(context.Background()); err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

func TestDrawdownLoop_RunOnce_ListerError(t *testing.T) {
	loop := newDrawdownLoop(&drawdown.Repo{}, &drawdownSnapshotBuilder{}, newServerMetrics(), nil, drawdownLoopOptions{
		FundLister: func(ctx context.Context) ([]string, error) {
			return nil, errors.New("boom")
		},
	})
	if err := loop.runOnce(context.Background()); err == nil {
		t.Error("expected lister error to bubble")
	}
}

func TestDrawdownLoop_NextDelay_BoundedByJitter(t *testing.T) {
	loop := newDrawdownLoop(nil, nil, nil, nil, drawdownLoopOptions{
		Interval:  5 * time.Minute,
		JitterPct: 0.05,
	})
	for i := 0; i < 50; i++ {
		d := loop.nextDelay()
		if d < 4*time.Minute+30*time.Second || d > 5*time.Minute+30*time.Second {
			t.Errorf("delay outside expected ±5%%: %v", d)
		}
	}
}
