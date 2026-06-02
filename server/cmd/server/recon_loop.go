// recon_loop.go — daily reconciliation scheduler (P1-3).
//
// What this loop does
//
//   - Once per `Interval` (default 24h, anchored to 00:30 UTC):
//     iterate over every fund the platform manages, build the
//     internal snapshot for the previous trading day, fabricate
//     a mock broker statement (until a real-broker loader lands),
//     ingest it, and run the diff engine.
//   - Persists each (run, breaks) tuple. Critical breaks keep the
//     run.status = 'completed' (the run itself ran fine — the
//     breaks are the ARTEFACT, not a failure).
//   - Records lifecycle events through serverMetrics so /metrics
//     exposes how many runs landed yesterday and how many breaks
//     they produced.
//
// Why "previous day" not "today"
//
// EOD reconciliation has to wait for the day's last fills to
// settle. If we ran for `today` at 00:30 UTC, the snapshot would
// already be stale by an entire trading session. We instead diff
// `yesterday`'s books — the broker statement (real or mocked) is
// always for a closed day.
//
// Failure handling
//
//   - Snapshot build error → log + count run_failed, skip this fund.
//   - Ingest error → record ingest_error, skip this fund.
//   - Diff is in-process; can only fail on programmer error.
//   - Persist run error → record run_failed, log; do NOT block
//     subsequent funds.
//
// Concurrency
//
//   - The loop runs in a single goroutine in this binary. In a
//     multi-replica deployment the platform's existing leader
//     coordinator gates the wave.
//   - Within a wave we don't parallelise — the recon path is
//     I/O-bound but cheap; serial keeps log lines and audit
//     ordering coherent.

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fundai/server/internal/recon"
)

// reconLoopOptions configures the scheduler.
type reconLoopOptions struct {
	// Interval between waves. Defaults to 24h.
	Interval time.Duration
	// JitterPct adds up to ±N% noise to the interval. Defaults
	// to 5% — recon is heavier than FX so we want less spread.
	JitterPct float64
	// PerFundTimeout caps each fund's full pipeline (snapshot →
	// ingest → diff → persist). Defaults to 30s.
	PerFundTimeout time.Duration
	// FundLister returns every fund_id the loop should reconcile.
	// Production wires this to fund_repo.ListAllIDs; tests can
	// stub it directly.
	FundLister func(ctx context.Context) ([]string, error)
	// Tolerances overrides the engine bands. Empty defaults to
	// recon.DefaultTolerances. The daily loop typically uses
	// defaults; a separate `month-end` mode (not wired yet)
	// could pass a stricter band.
	Tolerances recon.Tolerances
	// MockProviderOptions controls the synthetic statement.
	// IncludeDrift is OFF by default in the loop so a healthy
	// platform produces zero breaks; ops can flip it on while
	// dogfooding the dashboard.
	MockProviderOptions recon.MockProviderOptions
}

// reconLoop is the runnable produced by newReconLoop.
type reconLoop struct {
	repo    *recon.Repo
	builder *reconSnapshotBuilder
	metrics *serverMetrics
	logger  leveledLogger
	opts    reconLoopOptions
}

// newReconLoop builds a reconLoop with sensible defaults. nil
// repo / builder makes the loop a no-op so test wiring that
// doesn't need recon doesn't crash.
func newReconLoop(repo *recon.Repo, builder *reconSnapshotBuilder, metrics *serverMetrics, logger leveledLogger, opts reconLoopOptions) *reconLoop {
	if opts.Interval <= 0 {
		opts.Interval = 24 * time.Hour
	}
	if opts.JitterPct < 0 {
		opts.JitterPct = 0.05
	}
	if opts.PerFundTimeout <= 0 {
		opts.PerFundTimeout = 30 * time.Second
	}
	if opts.Tolerances == (recon.Tolerances{}) {
		opts.Tolerances = recon.DefaultTolerances
	}
	if opts.MockProviderOptions.Source == "" {
		opts.MockProviderOptions.Source = recon.SourceMock
	}
	return &reconLoop{
		repo:    repo,
		builder: builder,
		metrics: metrics,
		logger:  logger,
		opts:    opts,
	}
}

// Run blocks until ctx is cancelled. First wave runs after one
// nextDelay() — unlike fx_loop we don't run immediately, because
// recon is heavy enough that a startup ramp could blow past
// memory budget on every container restart. The 24h cadence is
// already comfortable.
func (l *reconLoop) Run(ctx context.Context) {
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
				l.logf("recon loop: tick failed", "err", err)
			}
			timer.Reset(l.nextDelay())
		}
	}
}

// runOnce runs one full wave: snapshot+diff each fund. Returns
// the first error for logging — does NOT short-circuit; later
// funds still run.
func (l *reconLoop) runOnce(ctx context.Context) error {
	if l == nil {
		return nil
	}
	if l.opts.FundLister == nil {
		// Without a lister the loop has no funds to iterate.
		// Treat as no-op rather than crash.
		l.recordEvent("scheduled_skip")
		return nil
	}
	fundIDs, err := l.opts.FundLister(ctx)
	if err != nil {
		l.recordEvent("scheduled_skip")
		return fmt.Errorf("recon loop: fund lister: %w", err)
	}
	yesterday := previousTradingDay(time.Now().UTC())

	var firstErr error
	for _, fid := range fundIDs {
		fid = strings.TrimSpace(fid)
		if fid == "" {
			continue
		}
		fctx, cancel := context.WithTimeout(ctx, l.opts.PerFundTimeout)
		err := l.runFund(fctx, fid, yesterday)
		cancel()
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// runFund executes the full pipeline for ONE fund.
func (l *reconLoop) runFund(ctx context.Context, fundID string, asOf time.Time) error {
	snap, err := l.builder.Build(ctx, fundID, asOf)
	if err != nil {
		l.recordEvent("run_failed")
		l.logf("recon loop: snapshot failed", "fund_id", fundID, "err", err)
		return err
	}

	provider := recon.NewMockProvider(l.opts.MockProviderOptions)
	stmt := provider.Build(snap)

	persisted, ingestErr := l.repo.IngestStatement(ctx, recon.IngestParamsFromBuild(stmt, ""))
	if ingestErr != nil && !errors.Is(ingestErr, recon.ErrAlreadyIngested) {
		l.recordEvent("ingest_error")
		l.logf("recon loop: ingest failed", "fund_id", fundID, "err", ingestErr)
		return ingestErr
	}
	if errors.Is(ingestErr, recon.ErrAlreadyIngested) {
		l.recordEvent("ingest_duplicate")
	} else {
		l.recordEvent("ingest_ok")
	}

	engine := recon.NewEngine(l.opts.Tolerances)
	diff := engine.Diff(stmt, snap)

	run, err := l.repo.CreateRun(ctx, recon.CreateRunParams{
		FundID:        fundID,
		StatementID:   persisted.ID,
		RunDate:       asOf,
		TriggerSource: "scheduled",
		Status:        recon.RunCompleted,
		Result:        diff,
		Summary: map[string]any{
			"provider": string(stmt.Source),
			"as_of":    asOf.Format("2006-01-02"),
		},
	})
	if err != nil {
		l.recordEvent("run_failed")
		l.logf("recon loop: persist run failed", "fund_id", fundID, "err", err)
		return err
	}
	l.recordEvent("run_ok")
	for _, b := range diff.Breaks {
		l.recordEvent(fmt.Sprintf("break_%s", b.Type))
	}
	l.logf("recon loop: completed",
		"fund_id", fundID, "run_id", run.ID, "as_of", asOf.Format("2006-01-02"),
		"break_count", run.BreakCountTotal)
	return nil
}

// previousTradingDay returns yesterday in UTC. We don't yet know
// holidays, so this can return e.g. a Sunday when invoked on
// Monday morning UTC — the recon will run anyway and produce a
// run row whose Statement is empty + diff is empty. That's fine:
// 'no breaks on a non-trading day' is the correct answer.
func previousTradingDay(now time.Time) time.Time {
	t := now.UTC().AddDate(0, 0, -1)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// nextDelay returns Interval+jitter. Same approach as fx_loop.
func (l *reconLoop) nextDelay() time.Duration {
	if l == nil {
		return 24 * time.Hour
	}
	base := l.opts.Interval
	if base <= 0 {
		base = 24 * time.Hour
	}
	noise := float64(time.Now().UnixNano()%1000) / 1000.0
	signed := (2*noise - 1) * l.opts.JitterPct
	return base + time.Duration(float64(base)*signed)
}

func (l *reconLoop) recordEvent(name string) {
	if l == nil || l.metrics == nil {
		return
	}
	l.metrics.RecordReconEvent(name)
}

func (l *reconLoop) logf(msg string, kv ...any) {
	if l == nil || l.logger == nil {
		return
	}
	if strings.Contains(msg, "failed") {
		l.logger.Warn(msg, kv...)
		return
	}
	l.logger.Info(msg, kv...)
}
