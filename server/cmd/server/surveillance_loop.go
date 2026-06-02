// surveillance_loop.go — periodic intraday surveillance scheduler
// (P1-7).
//
// What this loop does
//
//   - Once per `Interval` (default 1h while market hours, 4h
//     overnight, but we keep it simple at one cadence and rely on
//     idempotent fingerprint dedup), iterate every active fund,
//     load the day's filled trades, run the rules engine, persist
//     each new event, and book a `surveillance_run` row.
//
//   - Detection is idempotent: re-running over the same window
//     simply hits the unique fingerprint and the repo returns
//     Inserted=false. Operators see the events queue only grow
//     when something genuinely new fires.
//
// Why hourly rather than at-trade
//
// We could in principle hook the surveillance hot path into the
// trade execution pipeline and detect synchronously. Two reasons
// not to:
//
//  1. Surveillance is best-effort review-bait, not a hard gate. A
//     1-hour delay on review is acceptable and reviewers couldn't
//     work faster anyway.
//  2. Hot-path latency budget. Cancel/replace/order entry already
//     have tight deadlines; pushing a new sliding-window check in
//     there is a regression risk we don't need.
//
// Failure handling — same shape as recon: log, count, skip, move on.

package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fundai/server/internal/surveillance"
)

// surveillanceLoopOptions configures the scheduler.
type surveillanceLoopOptions struct {
	// Interval between scans. Defaults to 1h.
	Interval time.Duration
	// JitterPct adds ±N% noise. Default 5%.
	JitterPct float64
	// PerFundTimeout caps a single fund's pipeline. Default 30s.
	PerFundTimeout time.Duration
	// FundLister returns every fund_id to scan. Production wires
	// this to fund_repo.ListActive; tests stub directly.
	FundLister func(ctx context.Context) ([]string, error)
	// Engine is the rules engine to drive. nil → DefaultRules.
	Engine *surveillance.Engine
	// SessionCloseHourUTC overrides the session close used for
	// marking-close detection. 20 (8PM UTC) is a passable
	// US-market 4PM ET stand-in. The cleaner long-term fix is a
	// per-fund / per-instrument exchange-calendar lookup.
	SessionCloseHourUTC int
}

// surveillanceLoop is the runnable produced by newSurveillanceLoop.
type surveillanceLoop struct {
	repo    *surveillance.Repo
	builder *surveillanceSnapshotBuilder
	metrics *serverMetrics
	logger  leveledLogger
	opts    surveillanceLoopOptions
	engine  *surveillance.Engine
}

// newSurveillanceLoop builds the loop with sensible defaults. nil
// repo / builder makes the loop a no-op so test wiring that doesn't
// need surveillance doesn't crash.
func newSurveillanceLoop(repo *surveillance.Repo, builder *surveillanceSnapshotBuilder, metrics *serverMetrics, logger leveledLogger, opts surveillanceLoopOptions) *surveillanceLoop {
	if opts.Interval <= 0 {
		opts.Interval = 1 * time.Hour
	}
	if opts.JitterPct < 0 {
		opts.JitterPct = 0.05
	}
	if opts.PerFundTimeout <= 0 {
		opts.PerFundTimeout = 30 * time.Second
	}
	if opts.SessionCloseHourUTC <= 0 || opts.SessionCloseHourUTC > 23 {
		opts.SessionCloseHourUTC = 20
	}
	engine := opts.Engine
	if engine == nil {
		engine = surveillance.NewEngine(surveillance.DefaultRules()...)
	}
	return &surveillanceLoop{
		repo:    repo,
		builder: builder,
		metrics: metrics,
		logger:  logger,
		opts:    opts,
		engine:  engine,
	}
}

// Run blocks until ctx is cancelled. First wave fires after one
// nextDelay() so a container restart doesn't blast a scan over
// every fund the second the binary boots.
func (l *surveillanceLoop) Run(ctx context.Context) {
	if l == nil || l.repo == nil || l.builder == nil {
		return
	}
	timer := time.NewTimer(l.nextDelay())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := l.runOnce(ctx); err != nil {
				l.logf("surveillance loop: tick failed", "err", err)
			}
			timer.Reset(l.nextDelay())
		}
	}
}

// runOnce runs one wave: scan each fund's day-of trades.
func (l *surveillanceLoop) runOnce(ctx context.Context) error {
	if l == nil {
		return nil
	}
	if l.opts.FundLister == nil {
		l.recordEvent("scheduled_skip")
		return nil
	}
	fundIDs, err := l.opts.FundLister(ctx)
	if err != nil {
		l.recordEvent("scheduled_skip")
		return fmt.Errorf("surveillance loop: fund lister: %w", err)
	}
	now := time.Now().UTC()
	winStart, winEnd := startOfDay(now), endOfDay(now)
	close := time.Date(now.Year(), now.Month(), now.Day(), l.opts.SessionCloseHourUTC, 0, 0, 0, time.UTC)

	var firstErr error
	for _, fid := range fundIDs {
		fid = strings.TrimSpace(fid)
		if fid == "" {
			continue
		}
		fctx, cancel := context.WithTimeout(ctx, l.opts.PerFundTimeout)
		err := l.runFund(fctx, fid, winStart, winEnd, close)
		cancel()
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// runFund executes the full pipeline for ONE fund.
func (l *surveillanceLoop) runFund(ctx context.Context, fundID string, winStart, winEnd, close time.Time) error {
	started := time.Now()
	snap, err := l.builder.Load(ctx, surveillanceLoadParams{
		FundID:      fundID,
		WindowStart: winStart,
		WindowEnd:   winEnd,
	})
	if err != nil {
		l.recordEvent("run_failed")
		l.logf("surveillance loop: snapshot failed", "fund_id", fundID, "err", err)
		return err
	}
	res := l.engine.Run(snap, defaultMarketContext(close))

	persisted, deduped := 0, 0
	for _, ev := range res.Events {
		ir, ierr := l.repo.InsertEvent(ctx, ev)
		if ierr != nil {
			l.recordEvent("insert_error")
			l.logf("surveillance loop: insert failed", "fund_id", fundID, "rule", string(ev.RuleCode), "err", ierr)
			continue
		}
		if ir.Inserted {
			persisted++
			l.recordEvent(fmt.Sprintf("event_%s", ev.RuleCode))
			l.recordEvent(fmt.Sprintf("severity_%s", ev.Severity))
		} else {
			deduped++
		}
	}
	durationMS := int(time.Since(started) / time.Millisecond)

	run, err := l.repo.CreateRun(ctx, surveillance.CreateRunParams{
		FundID:        fundID,
		TriggerSource: "scheduled",
		WindowStart:   winStart,
		WindowEnd:     winEnd,
		TradeCount:    len(snap),
		Result:        res,
		DurationMS:    durationMS,
		Status:        "completed",
		Summary: map[string]any{
			"persisted": persisted,
			"deduped":   deduped,
			"close":     close.Format(time.RFC3339),
		},
	})
	if err != nil {
		l.recordEvent("run_failed")
		l.logf("surveillance loop: persist run failed", "fund_id", fundID, "err", err)
		return err
	}
	l.recordEvent("run_ok")
	l.logf("surveillance loop: completed",
		"fund_id", fundID, "run_id", run.ID, "events", run.EventCountTotal,
		"trade_count", len(snap), "duration_ms", durationMS,
	)
	return nil
}

// nextDelay returns Interval+jitter. Same approach as fx / recon
// loops to keep the scheduling story uniform.
func (l *surveillanceLoop) nextDelay() time.Duration {
	if l == nil {
		return 1 * time.Hour
	}
	base := l.opts.Interval
	if base <= 0 {
		base = 1 * time.Hour
	}
	noise := float64(time.Now().UnixNano()%1000) / 1000.0
	signed := (2*noise - 1) * l.opts.JitterPct
	return base + time.Duration(float64(base)*signed)
}

func (l *surveillanceLoop) recordEvent(name string) {
	if l == nil || l.metrics == nil {
		return
	}
	l.metrics.RecordSurveillanceEvent(name)
}

func (l *surveillanceLoop) logf(msg string, kv ...any) {
	if l == nil || l.logger == nil {
		return
	}
	if strings.Contains(msg, "failed") {
		l.logger.Warn(msg, kv...)
		return
	}
	l.logger.Info(msg, kv...)
}
