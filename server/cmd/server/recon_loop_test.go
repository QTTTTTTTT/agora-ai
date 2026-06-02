package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fundai/server/internal/recon"
)

// stubReconLogger is a no-op leveledLogger.
type stubReconLogger struct{}

func (stubReconLogger) Info(string, ...any) {}
func (stubReconLogger) Warn(string, ...any) {}

func TestReconLoop_Defaults(t *testing.T) {
	l := newReconLoop(nil, nil, nil, nil, reconLoopOptions{JitterPct: 0.05})
	if l.opts.Interval != 24*time.Hour {
		t.Errorf("interval = %v", l.opts.Interval)
	}
	if l.opts.PerFundTimeout != 30*time.Second {
		t.Errorf("timeout = %v", l.opts.PerFundTimeout)
	}
	if l.opts.JitterPct != 0.05 {
		t.Errorf("jitter = %v", l.opts.JitterPct)
	}
	if l.opts.Tolerances != recon.DefaultTolerances {
		t.Errorf("tolerances = %+v", l.opts.Tolerances)
	}
	if l.opts.MockProviderOptions.Source != recon.SourceMock {
		t.Errorf("source = %v", l.opts.MockProviderOptions.Source)
	}
}

// Explicit zero jitter is honoured.
func TestReconLoop_ExplicitZeroJitterPreserved(t *testing.T) {
	l := newReconLoop(nil, nil, nil, nil, reconLoopOptions{JitterPct: 0})
	if l.opts.JitterPct != 0 {
		t.Errorf("jitter = %v, want 0", l.opts.JitterPct)
	}
}

// runOnce with a nil FundLister is a graceful no-op.
func TestReconLoop_RunOnce_NilLister_NoOp(t *testing.T) {
	repo := recon.NewRepo(nil) // nil DB; we never reach it
	builder := &reconSnapshotBuilder{}
	l := newReconLoop(repo, builder, nil, stubReconLogger{}, reconLoopOptions{})
	if err := l.runOnce(context.Background()); err != nil {
		t.Errorf("nil lister should be no-op, got %v", err)
	}
}

// runOnce surfaces lister errors.
func TestReconLoop_RunOnce_ListerError(t *testing.T) {
	repo := recon.NewRepo(nil)
	builder := &reconSnapshotBuilder{}
	wantErr := errors.New("lister boom")
	l := newReconLoop(repo, builder, nil, stubReconLogger{}, reconLoopOptions{
		FundLister: func(ctx context.Context) ([]string, error) {
			return nil, wantErr
		},
	})
	err := l.runOnce(context.Background())
	if err == nil || !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want lister boom", err)
	}
}

// nextDelay respects Interval and bounds via jitter.
func TestReconLoop_NextDelay_Bounded(t *testing.T) {
	l := newReconLoop(nil, nil, nil, nil, reconLoopOptions{
		Interval:  10 * time.Hour,
		JitterPct: 0.05,
	})
	d := l.nextDelay()
	min := 10*time.Hour - 30*time.Minute  // -5%
	max := 10*time.Hour + 30*time.Minute  // +5%
	if d < min || d > max {
		t.Errorf("delay = %v, want in [%v, %v]", d, min, max)
	}
}

// previousTradingDay returns yesterday at midnight UTC.
func TestPreviousTradingDay(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 30, 0, 0, time.UTC)
	got := previousTradingDay(now)
	want := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
