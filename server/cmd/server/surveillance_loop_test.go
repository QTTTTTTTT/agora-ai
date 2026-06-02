package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fundai/server/internal/surveillance"
)

func TestSurveillanceLoop_DefaultsApplied(t *testing.T) {
	loop := newSurveillanceLoop(nil, nil, nil, nil, surveillanceLoopOptions{
		JitterPct: 0.05,
	})
	if loop.opts.Interval != 1*time.Hour {
		t.Errorf("interval = %v, want 1h", loop.opts.Interval)
	}
	if loop.opts.PerFundTimeout != 30*time.Second {
		t.Errorf("per-fund timeout = %v, want 30s", loop.opts.PerFundTimeout)
	}
	if loop.opts.JitterPct != 0.05 {
		t.Errorf("jitter = %v, want 0.05", loop.opts.JitterPct)
	}
	if loop.opts.SessionCloseHourUTC != 20 {
		t.Errorf("close hour = %d, want 20", loop.opts.SessionCloseHourUTC)
	}
	if loop.engine == nil {
		t.Error("engine must default to DefaultRules")
	}
}

func TestSurveillanceLoop_SessionCloseClampedToValidRange(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want int
	}{
		{0, 20},
		{-1, 20},
		{24, 20},
		{99, 20},
		{8, 8},
		{23, 23},
	} {
		loop := newSurveillanceLoop(nil, nil, nil, nil, surveillanceLoopOptions{
			SessionCloseHourUTC: tc.in,
		})
		if loop.opts.SessionCloseHourUTC != tc.want {
			t.Errorf("in=%d got %d want %d", tc.in, loop.opts.SessionCloseHourUTC, tc.want)
		}
	}
}

func TestSurveillanceLoop_NilBuilderIsNoop(t *testing.T) {
	loop := newSurveillanceLoop(nil, nil, nil, nil, surveillanceLoopOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately
	loop.Run(ctx)
	// We didn't crash — that's the test.
}

func TestSurveillanceLoop_RunOnce_NilFundLister(t *testing.T) {
	loop := newSurveillanceLoop(&surveillance.Repo{}, &surveillanceSnapshotBuilder{}, newServerMetrics(), nil, surveillanceLoopOptions{})
	loop.opts.FundLister = nil
	if err := loop.runOnce(context.Background()); err != nil {
		t.Errorf("err = %v, want nil (no-op when lister missing)", err)
	}
}

func TestSurveillanceLoop_RunOnce_ListerError(t *testing.T) {
	loop := newSurveillanceLoop(&surveillance.Repo{}, &surveillanceSnapshotBuilder{}, newServerMetrics(), nil, surveillanceLoopOptions{
		FundLister: func(ctx context.Context) ([]string, error) {
			return nil, errors.New("boom")
		},
	})
	if err := loop.runOnce(context.Background()); err == nil {
		t.Error("expected error from lister to bubble up")
	}
}

func TestSurveillanceLoop_NextDelay_BoundedByJitter(t *testing.T) {
	loop := newSurveillanceLoop(nil, nil, nil, nil, surveillanceLoopOptions{
		Interval:  1 * time.Hour,
		JitterPct: 0.05,
	})
	for i := 0; i < 50; i++ {
		d := loop.nextDelay()
		if d < 55*time.Minute || d > 65*time.Minute {
			t.Errorf("delay outside expected ±5%%: %v", d)
		}
	}
}

func TestSurveillanceLoop_ExplicitZeroJitterPreserved(t *testing.T) {
	loop := newSurveillanceLoop(nil, nil, nil, nil, surveillanceLoopOptions{
		JitterPct: 0,
	})
	if loop.opts.JitterPct != 0 {
		t.Errorf("explicit zero jitter overridden: %v", loop.opts.JitterPct)
	}
}
