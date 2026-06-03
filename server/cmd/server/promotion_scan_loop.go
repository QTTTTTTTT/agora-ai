// promotion_scan_loop.go — Sprint 13.2 nightly scanner for model
// A/B auto-promotion drafts.
//
// Once per Interval (default 24h ± jitter) the loop:
//
//	1. Lists all RUNNING model_ab_experiments.
//	2. For each experiment, asks the modelab.Scanner whether a
//	   non-primary arm has beaten the primary on the configured
//	   criteria for a long enough streak.
//	3. For each positive recommendation, calls DraftRepo.UpsertPending
//	   so the admin board surfaces a freshly-evaluated draft.
//
// The loop NEVER flips production traffic. The admin board's
// "apply" path is the only thing that actually changes state, and
// it's audited.

package main

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/fundai/server/internal/modelab"
)

type promotionScanLoopOptions struct {
	Interval         time.Duration
	JitterPct        float64
	PerScanTimeout   time.Duration
	Criteria         modelab.Criteria
}

type promotionScanLoop struct {
	reporter *modelab.Reporter
	repo     *modelab.Repo
	drafts   *modelab.DraftRepo
	opts     promotionScanLoopOptions
	rand     *rand.Rand
}

// newPromotionScanLoop wires the loop. Returns nil when any required
// dependency is missing so the wiring layer no-ops gracefully.
func newPromotionScanLoop(
	reporter *modelab.Reporter,
	repo *modelab.Repo,
	drafts *modelab.DraftRepo,
	opts promotionScanLoopOptions,
) *promotionScanLoop {
	if reporter == nil || repo == nil || drafts == nil {
		return nil
	}
	if opts.Interval <= 0 {
		opts.Interval = 24 * time.Hour
	}
	if opts.JitterPct <= 0 {
		opts.JitterPct = 0.05
	}
	if opts.PerScanTimeout <= 0 {
		opts.PerScanTimeout = 5 * time.Minute
	}
	opts.Criteria = opts.Criteria.FilledDefaults()
	return &promotionScanLoop{
		reporter: reporter,
		repo:     repo,
		drafts:   drafts,
		opts:     opts,
		rand:     rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(time.Now().UnixNano()>>32))),
	}
}

// Run blocks until ctx is cancelled, scheduling a scan every
// Interval ± jitter.
func (l *promotionScanLoop) Run(ctx context.Context) {
	if l == nil {
		return
	}
	slog.Info("promotion_scan_loop.start",
		"interval", l.opts.Interval,
		"streak_days", l.opts.Criteria.MinStreakDays,
		"min_agreement_pct", l.opts.Criteria.MinAgreementPct)
	for {
		select {
		case <-ctx.Done():
			slog.Info("promotion_scan_loop.stop", "reason", ctx.Err())
			return
		case <-time.After(l.nextWait()):
		}
		_, _ = l.RunOnce(ctx)
	}
}

// RunOnce executes one scan synchronously and returns the count of
// drafts upserted (fresh + superseded). Surfaced so the admin
// "scan now" button can trigger an on-demand evaluation.
func (l *promotionScanLoop) RunOnce(ctx context.Context) (int, error) {
	if l == nil {
		return 0, nil
	}
	scanCtx, cancel := context.WithTimeout(ctx, l.opts.PerScanTimeout)
	defer cancel()
	exps, err := l.repo.ListExperiments(scanCtx, []modelab.ExperimentStatus{modelab.StatusRunning}, 500)
	if err != nil {
		slog.Warn("promotion_scan_loop.list_failed", "err", err.Error())
		return 0, err
	}
	scanner := modelab.NewScanner(l.reporter)
	now := time.Now().UTC()
	upserts := 0
	for _, exp := range exps {
		rec, err := scanner.Evaluate(scanCtx, exp.ID, l.opts.Criteria, now)
		if err != nil {
			slog.Warn("promotion_scan_loop.scan_failed",
				"experiment_id", exp.ID, "err", err.Error())
			continue
		}
		if rec == nil {
			continue
		}
		id, fresh, err := l.drafts.UpsertPending(scanCtx, rec)
		if err != nil {
			slog.Warn("promotion_scan_loop.upsert_failed",
				"experiment_id", exp.ID, "err", err.Error())
			continue
		}
		slog.Info("promotion_scan_loop.draft_upserted",
			"experiment_id", exp.ID, "draft_id", id,
			"recommended_arm", rec.RecommendedArmLabel,
			"streak_days", rec.StreakDays, "fresh", fresh)
		upserts++
	}
	slog.Info("promotion_scan_loop.wave_done",
		"experiments_scanned", len(exps), "drafts_upserted", upserts)
	return upserts, nil
}

func (l *promotionScanLoop) nextWait() time.Duration {
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
