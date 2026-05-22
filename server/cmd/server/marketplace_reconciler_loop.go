package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/fundai/server/internal/marketplace"
)

// MarketplaceReconcilerLeaseName is the row id used in scheduler_leases
// for the marketplace reconciler.
const MarketplaceReconcilerLeaseName = "marketplace-reconciler"

// marketplaceReconcilerLoop drives the marketplace.Reconciler on a fixed
// interval. It runs as a background goroutine alongside the workflow
// scheduler and is stopped during graceful shutdown.
//
// The interval is intentionally larger than the PendingCutoff inside the
// reconciler so that orders only become candidates after they've had a
// fair chance to commit normally.
type marketplaceReconcilerLoop struct {
	reconciler *marketplace.Reconciler
	leader     leaderChecker
	metrics    *serverMetrics
	interval   time.Duration

	stopCh  chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex
	started bool
}

func newMarketplaceReconcilerLoop(reconciler *marketplace.Reconciler, metrics *serverMetrics) *marketplaceReconcilerLoop {
	return &marketplaceReconcilerLoop{
		reconciler: reconciler,
		metrics:    metrics,
		interval:   2 * time.Minute,
		stopCh:     make(chan struct{}),
	}
}

// SetLeaderChecker installs the lease manager used to gate the loop.
// Must be called before Start.
func (l *marketplaceReconcilerLoop) SetLeaderChecker(checker leaderChecker) {
	if l == nil {
		return
	}
	l.leader = checker
}

func (l *marketplaceReconcilerLoop) isLeader() bool {
	if l == nil || l.leader == nil {
		return true
	}
	return l.leader.IsLeader(MarketplaceReconcilerLeaseName)
}

func (l *marketplaceReconcilerLoop) Start() {
	if l == nil || l.reconciler == nil {
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
		ticker := time.NewTicker(l.interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				if !l.isLeader() {
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				summary, err := l.reconciler.Run(ctx)
				cancel()
				if err != nil {
					l.metrics.ObserveMarketplaceReconciliation("failed", 0, 0, 0, 1)
					slog.Error("marketplace reconciler run failed", "error", err)
					continue
				}
				l.metrics.ObserveMarketplaceReconciliation("succeeded", summary.Inspected, summary.MarkedFailed, summary.UnresolvedFindings, summary.Errored)
				if summary.Inspected == 0 {
					continue
				}
				slog.Info(
					"marketplace reconciler pass",
					"inspected", summary.Inspected,
					"markedFailed", summary.MarkedFailed,
					"unresolved", summary.UnresolvedFindings,
					"noChange", summary.NoChange,
					"errored", summary.Errored,
				)
			}
		}
	}()
}

func (l *marketplaceReconcilerLoop) Stop() {
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
