package main

import (
	"testing"
	"time"

	"github.com/fundai/server/internal/modelab"
)

// TestNewPromotionScanLoop_NilSafe confirms that any missing
// dependency yields a nil loop — the wiring layer relies on this
// to gracefully no-op when the DB isn't wired up.
func TestNewPromotionScanLoop_NilSafe(t *testing.T) {
	if l := newPromotionScanLoop(nil, nil, nil, promotionScanLoopOptions{}); l != nil {
		t.Fatalf("expected nil loop when reporter is nil")
	}
	// Reporter alone isn't enough — we need repo + drafts too.
	rep := &modelab.Reporter{}
	if l := newPromotionScanLoop(rep, nil, nil, promotionScanLoopOptions{}); l != nil {
		t.Fatalf("expected nil loop when repo is nil")
	}
}

// TestNewPromotionScanLoop_AppliesDefaults verifies that omitted
// options get production-safe defaults.
func TestNewPromotionScanLoop_AppliesDefaults(t *testing.T) {
	rep := &modelab.Reporter{Repo: &modelab.Repo{}}
	drafts := &modelab.DraftRepo{}
	l := newPromotionScanLoop(rep, rep.Repo, drafts, promotionScanLoopOptions{})
	if l == nil {
		t.Fatalf("expected non-nil loop")
	}
	if l.opts.Interval != 24*time.Hour {
		t.Fatalf("expected default Interval=24h, got %s", l.opts.Interval)
	}
	if l.opts.JitterPct <= 0 {
		t.Fatalf("expected default JitterPct > 0")
	}
	if l.opts.PerScanTimeout <= 0 {
		t.Fatalf("expected default PerScanTimeout > 0")
	}
	if l.opts.Criteria.MinStreakDays != 7 {
		t.Fatalf("expected default streak=7, got %d", l.opts.Criteria.MinStreakDays)
	}
}

// TestPromotionScanLoop_RunOnce_NilSafeReturnsZero confirms RunOnce
// on a nil receiver doesn't crash and reports zero work.
func TestPromotionScanLoop_RunOnce_NilSafeReturnsZero(t *testing.T) {
	var l *promotionScanLoop
	got, err := l.RunOnce(nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if got != 0 {
		t.Fatalf("expected 0 upserts, got %d", got)
	}
}
