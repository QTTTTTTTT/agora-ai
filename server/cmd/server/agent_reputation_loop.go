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
	"github.com/fundai/server/internal/ohlc"
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

// fundProfileLookupFn is the minimal contract
// realisedReturnFromOHLC needs to know (a) which OHLC market a
// symbol belongs to and (b) the fund's benchmark symbol.
// Production wires it to the FundRepo + decodeFundMarketProfile
// pair; tests inject a static map. ok=false when the fund is not
// found or has no benchmark configured — both cases skip the row.
type fundProfileLookupFn func(ctx context.Context, fundID string) (market, benchmarkSymbol string, ok bool)

// realisedReturnFromOHLC builds an agentreputation.RealisedReturnFn
// closing over the shared OHLC fetcher and a thin fund-profile
// lookup.
//
// W16-2 audit: the production wiring previously plugged
// nullRealisedReturn here, which silently disabled the entire
// alpha-aware-memory pipeline (every backfill produced zero
// outcomes; the agent_reputation_stats table stayed empty;
// AgentTrackRecord in the PM prompt was always empty). This
// constructor wires the existing daily-bar OHLC fetcher and
// the fund repo so the reputation loop actually computes
// realised returns going forward.
//
// Per-call work:
//
//  1. Look up the fund's market profile (market + benchmark
//     symbol) via the injected lookup.
//  2. Fetch enough daily bars on BOTH symbol and benchmark to
//     bracket [asof, asof + horizonDays]. We pull lookback +
//     horizon + a small buffer so weekend / holiday gaps don't
//     starve the entry/exit search.
//  3. Resolve entry close = the most recent close ≤ asof on the
//     symbol's bars; exit close = the most recent close ≤ (asof
//     + horizonDays). Same alignment for the benchmark, so
//     realised and benchmark returns are computed against an
//     internally consistent pair of trading days even when the
//     symbol and benchmark trade on different calendars.
//  4. Return decimal returns ((exit - entry) / entry); ok=false
//     when any bar is missing or the entry close is non-positive
//     (a corp-action artifact — skip rather than synthesise a
//     500% number).
//
// All errors degrade to ok=false, never propagated. Agent
// reputation is a soft learning loop — a transient OHLC outage
// must not block the wave, and the next pass will retry the
// same (symbol, asof, horizon) row idempotently.
func realisedReturnFromOHLC(fetcher ohlc.Fetcher, lookup fundProfileLookupFn) agentreputation.RealisedReturnFn {
	return func(ctx context.Context, fundID, symbol string, asof time.Time, horizonDays int) (float64, float64, bool, error) {
		if fetcher == nil || lookup == nil {
			return 0, 0, false, nil
		}
		symbol = strings.TrimSpace(symbol)
		fundID = strings.TrimSpace(fundID)
		if symbol == "" || fundID == "" || horizonDays <= 0 {
			return 0, 0, false, nil
		}
		market, benchmark, ok := lookup(ctx, fundID)
		if !ok {
			return 0, 0, false, nil
		}
		market = strings.ToLower(strings.TrimSpace(market))
		benchmark = strings.TrimSpace(benchmark)
		if benchmark == "" {
			return 0, 0, false, nil
		}
		// Pull enough bars to comfortably bracket the horizon.
		// 30-day buffer absorbs Chinese New Year / Easter /
		// Thanksgiving gaps without falsely failing.
		lookback := horizonDays + 30
		endTime := asof.AddDate(0, 0, horizonDays+5)
		symBars, err := fetcher.Fetch(ctx, ohlc.FetchRequest{
			Symbol:    symbol,
			Market:    market,
			Interval:  ohlc.IntervalDay,
			LookbackN: lookback,
			EndTime:   endTime,
		})
		if err != nil || len(symBars) == 0 {
			return 0, 0, false, nil
		}
		benchBars, err := fetcher.Fetch(ctx, ohlc.FetchRequest{
			Symbol:    benchmark,
			Market:    market,
			Interval:  ohlc.IntervalDay,
			LookbackN: lookback,
			EndTime:   endTime,
		})
		if err != nil || len(benchBars) == 0 {
			return 0, 0, false, nil
		}
		exitTarget := asof.AddDate(0, 0, horizonDays)
		symEntry, ok1 := closeAtOrBefore(symBars, asof)
		symExit, ok2 := closeAtOrBefore(symBars, exitTarget)
		benchEntry, ok3 := closeAtOrBefore(benchBars, asof)
		benchExit, ok4 := closeAtOrBefore(benchBars, exitTarget)
		if !ok1 || !ok2 || !ok3 || !ok4 {
			return 0, 0, false, nil
		}
		if symEntry <= 0 || benchEntry <= 0 {
			return 0, 0, false, nil
		}
		// Skip rows where entry == exit (no horizon elapsed yet —
		// the symbol's bars haven't caught up). Equivalent to
		// "the window has not closed" rather than "alpha is
		// exactly zero".
		if symExit == symEntry && benchExit == benchEntry {
			return 0, 0, false, nil
		}
		realised := (symExit - symEntry) / symEntry
		bench := (benchExit - benchEntry) / benchEntry
		return realised, bench, true, nil
	}
}

// closeAtOrBefore returns the close of the most recent bar whose
// Time is ≤ target. ok=false when no bar predates target (target
// is before every bar in the series — typically a brand-new
// listing or a missing-history gap upstream).
func closeAtOrBefore(bars []ohlc.Bar, target time.Time) (float64, bool) {
	if len(bars) == 0 {
		return 0, false
	}
	// Bars are in chronological ascending order per the OHLC
	// Fetcher contract; iterate backwards for fast typical-case
	// match (target is usually near the end).
	for i := len(bars) - 1; i >= 0; i-- {
		if !bars[i].Time.After(target) {
			return bars[i].Close, true
		}
	}
	return 0, false
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
