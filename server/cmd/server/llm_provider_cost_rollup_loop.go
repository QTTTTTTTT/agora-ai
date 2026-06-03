// llm_provider_cost_rollup_loop.go — S14.A: hourly job that folds
// recent usage_entries into platform_llm_provider_daily_rollups.
//
// Why hourly (not real-time):
//   * Real-time materialised view would either re-aggregate on
//     every read (expensive) or require a write-side trigger that
//     mutates rollups on every LLM call (extra IOPS on the hot
//     path). Hourly latency is fine for a cost dashboard — the
//     business wants to know "what did Tuesday cost" by Wednesday
//     morning, not by 10:01am.
//   * The repo's RecomputeWindow re-derives the FULL day for any
//     bucket that saw activity in the window, so re-runs are
//     idempotent. A missed tick (process restart, DB blip) just
//     means the next tick catches up.
//
// Catch-up at startup: we pass a 25-hour window the first time so
// a server that's been off all day picks up everything from yesterday
// + today. Any later runs use a 90-minute window (slight overlap
// with the previous tick is intentional belt-and-suspenders).

package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/fundai/server/internal/repository"
)

type llmCostRollupLoop struct {
	rollupRepo  *repository.ProviderDailyRollupRepo
	interval    time.Duration
	startupBack time.Duration
	tickBack    time.Duration
	logger      *slog.Logger
	ticks       atomic.Int64
}

func newLLMCostRollupLoop(rollupRepo *repository.ProviderDailyRollupRepo, logger *slog.Logger) *llmCostRollupLoop {
	if logger == nil {
		logger = slog.Default()
	}
	return &llmCostRollupLoop{
		rollupRepo:  rollupRepo,
		interval:    1 * time.Hour,
		startupBack: 25 * time.Hour,
		tickBack:    90 * time.Minute,
		logger:      logger,
	}
}

// Start kicks off the loop and runs a catch-up rollup immediately
// so the dashboard isn't empty for a freshly-booted server.
func (l *llmCostRollupLoop) Start(ctx context.Context) {
	if l == nil || l.rollupRepo == nil {
		slog.Info("llm_cost_rollup_loop: skipped (deps unwired)")
		return
	}
	go l.run(ctx)
}

// Ticks returns the number of completed rollup iterations.
func (l *llmCostRollupLoop) Ticks() int64 {
	if l == nil {
		return 0
	}
	return l.ticks.Load()
}

func (l *llmCostRollupLoop) run(ctx context.Context) {
	// Initial catch-up.
	l.runOnce(ctx, l.startupBack)
	l.ticks.Add(1)

	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.runOnce(ctx, l.tickBack)
			l.ticks.Add(1)
		}
	}
}

func (l *llmCostRollupLoop) runOnce(ctx context.Context, lookback time.Duration) {
	from := time.Now().Add(-lookback)
	to := time.Now()
	start := time.Now()
	n, err := l.rollupRepo.RecomputeWindow(ctx, from, to)
	if err != nil {
		l.logger.Warn("llm_cost_rollup_loop: recompute", "err", err, "from", from, "to", to)
		return
	}
	l.logger.Info("llm_cost_rollup_loop: ran",
		"buckets", n, "lookback", lookback.String(), "duration_ms", time.Since(start).Milliseconds())
}
