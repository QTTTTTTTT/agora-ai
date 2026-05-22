package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/fundai/server/internal/promotion"
)

// PromotionDecayLeaseName is the scheduler_leases row id that
// gates the decay-monitor loop. Only one server in the pool runs
// it; followers tick idly and re-check leadership so a leader
// election doesn't delay decay sampling beyond one interval.
const PromotionDecayLeaseName = "promotion-decay-monitor"

// promotionDecayLoop runs the Phase 2L DecayMonitor on a fixed
// cadence. Defaults: 1 hour interval, 30s warmup after boot. The
// warmup gives the lease manager time to settle and avoids
// thundering against the DB during a multi-replica deploy.
//
// The loop is leader-gated. On a follower it ticks idly; on the
// leader it asks DecayMonitor.SampleAll for every live promotion
// across all funds. SampleAll itself swallows per-promotion
// errors so one bad fund doesn't block the rest.
type promotionDecayLoop struct {
	monitor  *promotion.DecayMonitor
	leader   leaderChecker
	interval time.Duration

	stopCh  chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex
	started bool
}

func newPromotionDecayLoop(monitor *promotion.DecayMonitor) *promotionDecayLoop {
	return &promotionDecayLoop{
		monitor:  monitor,
		interval: time.Hour,
		stopCh:   make(chan struct{}),
	}
}

// SetLeaderChecker wires the distributed leader-election gate.
// Nil is permitted — single-binary smoke runs operate as the
// permanent leader.
func (l *promotionDecayLoop) SetLeaderChecker(checker leaderChecker) {
	if l == nil {
		return
	}
	l.leader = checker
}

func (l *promotionDecayLoop) isLeader() bool {
	if l == nil || l.leader == nil {
		return true
	}
	return l.leader.IsLeader(PromotionDecayLeaseName)
}

// Start launches the background goroutine. Idempotent.
func (l *promotionDecayLoop) Start() {
	if l == nil || l.monitor == nil {
		return
	}
	l.mu.Lock()
	if l.started {
		l.mu.Unlock()
		return
	}
	if l.stopCh == nil {
		l.stopCh = make(chan struct{})
	}
	stopCh := l.stopCh
	l.started = true
	l.wg.Add(1)
	l.mu.Unlock()

	go func() {
		defer l.wg.Done()
		warmup := time.NewTimer(30 * time.Second)
		select {
		case <-stopCh:
			warmup.Stop()
			return
		case <-warmup.C:
		}
		l.runOnce()

		ticker := time.NewTicker(l.interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				l.runOnce()
			}
		}
	}()
}

// runOnce is the body of a single decay sweep. Pulled out so a
// test can drive it deterministically without spinning the
// background goroutine.
func (l *promotionDecayLoop) runOnce() {
	if !l.isLeader() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	snaps, err := l.monitor.SampleAll(ctx)
	if err != nil {
		slog.Warn("promotion decay: sample-all returned error", "error", err, "snapshots_taken", len(snaps))
		return
	}
	flagged := 0
	for _, s := range snaps {
		if s.DecayFlag {
			flagged++
		}
	}
	slog.Info("promotion decay pass",
		"snapshots", len(snaps),
		"decay_flagged", flagged,
	)
}

func (l *promotionDecayLoop) Stop() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if !l.started {
		l.mu.Unlock()
		return
	}
	stopCh := l.stopCh
	l.stopCh = nil
	l.started = false
	l.mu.Unlock()
	close(stopCh)
	l.wg.Wait()
}
