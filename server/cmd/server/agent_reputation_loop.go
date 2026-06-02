// agent_reputation_loop.go — S8.4 nightly backfill driver.
//
// Iterates over every fund, asks the backfill driver to read
// recent analyst panels + debate transcripts, materialises one
// Outcome row per (agent, symbol, asof, horizon), and refreshes
// the rolling agent_reputation_stats summary.
//
// The realised-return source is plug-and-play (RealisedReturnFn).
// If no source is wired the loop becomes a no-op and the
// reputation tables stay empty — handlers continue to serve the
// (empty) listing without 500s.
//
// Concurrency: single goroutine per binary. In a multi-replica
// deployment the leader gate already in main.go gates the wave.
// The backfill itself uses UPSERT semantics so a late retry is
// harmless.

package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/fundai/server/internal/agentreputation"
)

// agentReputationLoopOptions configures the scheduler.
type agentReputationLoopOptions struct {
	// Interval between backfill waves. Defaults to 24h.
	Interval time.Duration
	// JitterPct adds up to ±N% noise to the interval to spread
	// load across a multi-replica deployment. Defaults to 5%.
	JitterPct float64
	// PerFundTimeout caps each fund's backfill pipeline.
	// Defaults to 60s.
	PerFundTimeout time.Duration
	// FundLister returns every fund_id the loop should backfill.
	// Production wires fund_repo.ListAllIDs.
	FundLister func(ctx context.Context) ([]string, error)
	// LookbackDays sets how far back we scan analyst panels +
	// debate transcripts each wave. Defaults to 30 days.
	LookbackDays int
	// Horizons is the list of forward windows (in days) the
	// backfill produces outcomes for. Defaults to {1, 5, 21}.
	Horizons []int
	// LessonWriter is the S9.1 sink. Nil = no alpha-tagged
	// memory rows are minted (still safe; the reputation table
	// is the source of truth).
	LessonWriter agentreputation.LessonWriter
}

// agentReputationLoop is the runnable produced by newAgentReputationLoop.
type agentReputationLoop struct {
	repo     *agentreputation.Repo
	panels   agentreputation.PanelSource
	debates  agentreputation.DebateSource
	returns  agentreputation.RealisedReturnFn
	opts     agentReputationLoopOptions
	rand     *rand.Rand
}

// newAgentReputationLoop wires the loop. Returns nil when the
// repo is absent so the wiring layer can no-op without crashing
// on startup.
func newAgentReputationLoop(
	repo *agentreputation.Repo,
	panels agentreputation.PanelSource,
	debates agentreputation.DebateSource,
	returns agentreputation.RealisedReturnFn,
	opts agentReputationLoopOptions,
) *agentReputationLoop {
	if repo == nil {
		return nil
	}
	if returns == nil {
		returns = nullRealisedReturn
	}
	if opts.Interval <= 0 {
		opts.Interval = 24 * time.Hour
	}
	if opts.JitterPct <= 0 {
		opts.JitterPct = 0.05
	}
	if opts.PerFundTimeout <= 0 {
		opts.PerFundTimeout = 60 * time.Second
	}
	if opts.LookbackDays <= 0 {
		opts.LookbackDays = 30
	}
	if len(opts.Horizons) == 0 {
		opts.Horizons = []int{1, 5, 21}
	}
	return &agentReputationLoop{
		repo: repo, panels: panels, debates: debates, returns: returns, opts: opts,
		rand: rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(time.Now().UnixNano()>>32))),
	}
}

// Run blocks until ctx is cancelled, scheduling a backfill wave
// every `Interval` ± jitter.
func (l *agentReputationLoop) Run(ctx context.Context) {
	if l == nil {
		return
	}
	slog.Info("agent_reputation_loop.start",
		"interval", l.opts.Interval, "lookback_days", l.opts.LookbackDays,
		"horizons", l.opts.Horizons)
	for {
		select {
		case <-ctx.Done():
			slog.Info("agent_reputation_loop.stop", "reason", ctx.Err())
			return
		case <-time.After(l.nextWaitWithJitter()):
		}
		l.runWave(ctx)
	}
}

// RunOnce executes one wave synchronously. Useful for tests +
// the admin "rebuild reputation" button.
func (l *agentReputationLoop) RunOnce(ctx context.Context) (int, error) {
	if l == nil {
		return 0, nil
	}
	return l.runWave(ctx), nil
}

// RebuildForFund satisfies agentReputationRebuildSink. When
// fundID is empty the call falls through to the configured
// FundLister (i.e. rebuilds every fund). When fundID is set the
// lister is temporarily narrowed to that single id so the
// rebuild stays scoped + the rest of the loop's state (timeouts,
// horizons) is reused as-is.
func (l *agentReputationLoop) RebuildForFund(ctx context.Context, fundID string) (int, error) {
	if l == nil {
		return 0, nil
	}
	fundID = strings.TrimSpace(fundID)
	if fundID == "" {
		return l.RunOnce(ctx)
	}
	prev := l.opts.FundLister
	l.opts.FundLister = func(_ context.Context) ([]string, error) { return []string{fundID}, nil }
	defer func() { l.opts.FundLister = prev }()
	return l.RunOnce(ctx)
}

func (l *agentReputationLoop) runWave(ctx context.Context) int {
	if l == nil || l.opts.FundLister == nil {
		return 0
	}
	listerCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	fundIDs, err := l.opts.FundLister(listerCtx)
	if err != nil {
		slog.Warn("agent_reputation_loop.lister_failed", "err", err.Error())
		return 0
	}
	bf := agentreputation.NewBackfill(l.repo, l.panels, l.debates, l.returns)
	if l.opts.LessonWriter != nil {
		bf = bf.WithLessonWriter(l.opts.LessonWriter)
	}
	since := time.Now().Add(-time.Duration(l.opts.LookbackDays) * 24 * time.Hour)
	total := 0
	for _, fid := range fundIDs {
		fundCtx, cancelFund := context.WithTimeout(ctx, l.opts.PerFundTimeout)
		n, err := bf.Run(fundCtx, fid, agentreputation.BackfillConfig{
			Horizons: l.opts.Horizons,
			Since:    since,
		})
		cancelFund()
		if err != nil {
			slog.Warn("agent_reputation_loop.fund_failed", "fund_id", fid, "err", err.Error())
			continue
		}
		total += n
		if n > 0 {
			slog.Info("agent_reputation_loop.fund_backfilled", "fund_id", fid, "outcomes", n)
		}
	}
	slog.Info("agent_reputation_loop.wave_done", "funds", len(fundIDs), "outcomes", total)
	return total
}

func (l *agentReputationLoop) nextWaitWithJitter() time.Duration {
	base := l.opts.Interval
	if base <= 0 || l.opts.JitterPct <= 0 {
		return base
	}
	delta := float64(base) * l.opts.JitterPct
	noise := (l.rand.Float64()*2 - 1) * delta
	d := time.Duration(float64(base) + noise)
	if d <= 0 {
		d = base
	}
	return d
}

// --- defaults ---------------------------------------------------------------

// nullRealisedReturn is the safe default — returns ok=false so
// the backfill driver skips every (symbol, asof, horizon). The
// loop still runs; the reputation tables simply stay empty until
// a real price source is wired.
func nullRealisedReturn(_ context.Context, _, _ string, _ time.Time, _ int) (float64, float64, bool, error) {
	return 0, 0, false, nil
}

// helper for log lines — keeps the loop testable by avoiding
// time formatting in hot path.
func formatHorizons(hs []int) string {
	parts := make([]string, 0, len(hs))
	for _, h := range hs {
		parts = append(parts, fmt.Sprintf("%dd", h))
	}
	return strings.Join(parts, ",")
}
