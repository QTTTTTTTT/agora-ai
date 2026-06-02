// drawdown_loop.go — periodic drawdown scan scheduler (P3-5).
//
// What this loop does
//
//   - Every `Interval` (default 5min during market hours):
//     iterate every active fund, build a Snapshot, evaluate the
//     Policy, and persist any breach event.
//   - Auto-execute path is OPT-IN per tier: if a tier has
//     `auto_execute=true` AND fires, the loop persists with
//     status='approved' and queues the trim orders. Otherwise
//     the event lands as 'proposed' for operator review.
//
// Why 5 minutes
//
// DD breaches don't move in seconds; a slower cadence reduces
// load + churn. Faster than 1h so the loop can react to a
// session-during slide before the day's last hour. The interval
// is cheap to retune later.
//
// Why "auto_execute" is conservative
//
// Even when auto_execute=true, the trim plan still flows through
// the order pipeline's risk gates (P0 framework) and the audit
// chain (P0-8). Auto-execute is a TIME-SAVER for funds where the
// operator has already pre-approved the policy intent, not a
// "skip the safeties" knob.

package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fundai/server/internal/drawdown"
)

// drawdownLoopOptions configures the scheduler.
type drawdownLoopOptions struct {
	// Interval between waves. Default 5min.
	Interval time.Duration
	// JitterPct adds ±N% noise. Default 5%.
	JitterPct float64
	// PerFundTimeout caps a single fund's pipeline. Default 30s.
	PerFundTimeout time.Duration
	// FundLister returns every fund_id to scan. Production wires
	// this to fund_repo.ListActive; tests stub directly.
	FundLister func(ctx context.Context) ([]string, error)
	// AutoExecuteHandler is the hook the loop calls when an event
	// fires AND the matched tier has auto_execute=true. The
	// concrete implementation queues market-sell orders through
	// the order pipeline. nil → loop skips auto-execute, persisting
	// the event as 'proposed' instead of 'approved'.
	AutoExecuteHandler func(ctx context.Context, eventID string, ev drawdown.BreachEvent) error
}

// drawdownLoop is the runnable produced by newDrawdownLoop.
type drawdownLoop struct {
	repo    *drawdown.Repo
	builder *drawdownSnapshotBuilder
	metrics *serverMetrics
	logger  leveledLogger
	opts    drawdownLoopOptions
	engine  *drawdown.Engine
}

// newDrawdownLoop builds the loop with sensible defaults. nil
// repo / builder makes the loop a no-op.
func newDrawdownLoop(repo *drawdown.Repo, builder *drawdownSnapshotBuilder, metrics *serverMetrics, logger leveledLogger, opts drawdownLoopOptions) *drawdownLoop {
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Minute
	}
	if opts.JitterPct < 0 {
		opts.JitterPct = 0.05
	}
	if opts.PerFundTimeout <= 0 {
		opts.PerFundTimeout = 30 * time.Second
	}
	return &drawdownLoop{
		repo:    repo,
		builder: builder,
		metrics: metrics,
		logger:  logger,
		opts:    opts,
		engine:  drawdown.NewEngine(),
	}
}

// Run blocks until ctx is cancelled. First wave fires after one
// nextDelay() so a container restart doesn't blast a scan over
// every fund the second the binary boots.
func (l *drawdownLoop) Run(ctx context.Context) {
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
				l.logf("drawdown loop: tick failed", "err", err)
			}
			timer.Reset(l.nextDelay())
		}
	}
}

// runOnce runs one wave: scan each fund's drawdown.
func (l *drawdownLoop) runOnce(ctx context.Context) error {
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
		return fmt.Errorf("drawdown loop: fund lister: %w", err)
	}
	now := time.Now().UTC()
	var firstErr error
	for _, fid := range fundIDs {
		fid = strings.TrimSpace(fid)
		if fid == "" {
			continue
		}
		fctx, cancel := context.WithTimeout(ctx, l.opts.PerFundTimeout)
		err := l.runFund(fctx, fid, now)
		cancel()
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// runFund executes the full pipeline for ONE fund. Skips funds
// without a policy (no-op).
func (l *drawdownLoop) runFund(ctx context.Context, fundID string, asOf time.Time) error {
	policy, err := l.repo.GetPolicy(ctx, fundID)
	if err != nil {
		l.recordEvent("check_failed")
		l.logf("drawdown loop: get policy failed", "fund_id", fundID, "err", err)
		return err
	}
	if len(policy.Tiers) == 0 {
		// No policy → nothing to do for this fund. Don't bump
		// check_ok either; we skip silently.
		return nil
	}
	snap, err := l.builder.Build(ctx, fundID, asOf)
	if err != nil {
		l.recordEvent("check_failed")
		l.logf("drawdown loop: snapshot failed", "fund_id", fundID, "err", err)
		return err
	}
	ev, err := l.engine.Evaluate(snap, policy)
	if err != nil {
		l.recordEvent("check_failed")
		l.logf("drawdown loop: evaluate failed", "fund_id", fundID, "err", err)
		return err
	}
	l.recordEvent("check_ok")
	if ev == nil {
		return nil
	}

	// Determine initial status: auto_execute on the matched
	// tier wins over the loop default.
	initial := drawdown.StatusProposed
	autoExecute := false
	for _, t := range policy.Tiers {
		if t.Tier == ev.Tier && t.AutoExecute {
			autoExecute = true
			break
		}
	}
	if autoExecute && l.opts.AutoExecuteHandler != nil {
		initial = drawdown.StatusApproved
	}
	id, err := l.repo.InsertEvent(ctx, *ev, initial)
	if err != nil {
		l.recordEvent("check_failed")
		l.logf("drawdown loop: insert event failed", "fund_id", fundID, "err", err)
		return err
	}
	l.recordEvent(fmt.Sprintf("breach_tier_%d", ev.Tier))
	l.recordEvent(fmt.Sprintf("action_%s", ev.Action))

	if autoExecute && l.opts.AutoExecuteHandler != nil {
		if err := l.opts.AutoExecuteHandler(ctx, id, *ev); err != nil {
			// Handler failed; promote to executed will be
			// retried by the operator in the admin UI. We log
			// but do not flip the lifecycle here — the event
			// stays 'approved' until orders actually queue.
			l.recordEvent("check_failed")
			l.logf("drawdown loop: auto-execute handler failed",
				"fund_id", fundID, "event_id", id, "err", err)
		} else {
			l.recordEvent("auto_executed")
		}
	}
	l.logf("drawdown loop: breach recorded",
		"fund_id", fundID, "event_id", id, "tier", ev.Tier,
		"action", string(ev.Action), "auto_execute", autoExecute,
	)
	return nil
}

// nextDelay returns Interval+jitter. Same approach as fx / recon
// / surveillance loops — keeps the scheduling story uniform.
func (l *drawdownLoop) nextDelay() time.Duration {
	if l == nil {
		return 5 * time.Minute
	}
	base := l.opts.Interval
	if base <= 0 {
		base = 5 * time.Minute
	}
	noise := float64(time.Now().UnixNano()%1000) / 1000.0
	signed := (2*noise - 1) * l.opts.JitterPct
	return base + time.Duration(float64(base)*signed)
}

func (l *drawdownLoop) recordEvent(name string) {
	if l == nil || l.metrics == nil {
		return
	}
	l.metrics.RecordDrawdownEvent(name)
}

func (l *drawdownLoop) logf(msg string, kv ...any) {
	if l == nil || l.logger == nil {
		return
	}
	if strings.Contains(msg, "failed") {
		l.logger.Warn(msg, kv...)
		return
	}
	l.logger.Info(msg, kv...)
}
