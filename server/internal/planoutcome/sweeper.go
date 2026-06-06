package planoutcome

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// PendingPlanLister is the narrow read-side contract the sweeper
// needs: enumerate plan IDs whose outcome has not been resolved
// AND whose created_at is older than `cutoff`. Implemented by
// repository.PlanRepo.ListPendingOutcomePlans; pulled out as an
// interface so the sweeper can be tested without a database.
type PendingPlanLister interface {
	ListPendingOutcomePlans(ctx context.Context, cutoff time.Time, limit int) ([]string, error)
}

// PlanOutcomeWriter is the narrow write-side contract: persist
// the marshalled outcome bytes for a plan. Implemented by
// repository.PlanRepo.SetPlanOutcome.
type PlanOutcomeWriter interface {
	SetPlanOutcome(ctx context.Context, planID string, payload []byte) error
}

// SweeperConfig configures one resolver sweep. Sensible defaults:
//
//   * MaxAge of 5 trading days (the most common short-term
//     window). The sweep only considers plans older than this,
//     because younger plans haven't had time to mature even for
//     the shortest fixed-window resolver.
//   * BatchSize of 200 — big enough that a daily sweep clears
//     the backlog of a single fund's worth of plans, small
//     enough that one tick can't lock the table.
//
// PerPlanTimeout caps how long any single ResolveForPlan call
// can run. A misbehaving market-data fetch shouldn't stall the
// rest of the sweep.
type SweeperConfig struct {
	MaxAge         time.Duration
	BatchSize      int
	PerPlanTimeout time.Duration
}

// DefaultSweeperConfig is the production-safe baseline.
func DefaultSweeperConfig() SweeperConfig {
	return SweeperConfig{
		MaxAge:         5 * 24 * time.Hour,
		BatchSize:      200,
		PerPlanTimeout: 30 * time.Second,
	}
}

// SweepStats summarises one sweep tick. Returned to the caller
// so a metrics observer can record outcome resolution
// throughput / failure counts.
type SweepStats struct {
	Scanned   int
	Resolved  int
	Pending   int // plan was scanned but the resolver said "window not yet elapsed"
	Errors    int
	WroteRows int
}

// Sweeper drives the post-decision outcome capture. One Sweeper
// is held by the wiring layer alongside other periodic workers
// (refreshers, reconcilers); a Tick() is fired on a schedule.
//
// The struct is deliberately small — the meaty logic (P&L
// computation, window-elapsed predicate, market-data joins)
// lives in the Resolver implementation. This decouples the
// schedule + persistence concerns from the resolver-specific
// business logic.
type Sweeper struct {
	Lister   PendingPlanLister
	Writer   PlanOutcomeWriter
	Resolver Resolver
	Config   SweeperConfig
}

// NewSweeper wires a Sweeper. Callers can replace any of the
// dependencies with stubs in tests; the production wiring path
// passes a *repository.PlanRepo for both Lister and Writer plus
// a real PnL-aware Resolver.
func NewSweeper(lister PendingPlanLister, writer PlanOutcomeWriter, resolver Resolver, cfg SweeperConfig) *Sweeper {
	if cfg.MaxAge == 0 {
		cfg.MaxAge = DefaultSweeperConfig().MaxAge
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = DefaultSweeperConfig().BatchSize
	}
	if cfg.PerPlanTimeout == 0 {
		cfg.PerPlanTimeout = DefaultSweeperConfig().PerPlanTimeout
	}
	return &Sweeper{Lister: lister, Writer: writer, Resolver: resolver, Config: cfg}
}

// Tick runs one sweep cycle: list pending plans older than the
// configured cutoff → call the Resolver for each → persist any
// resolved outcomes. Returns aggregate stats so the caller can
// emit metrics. A nil Sweeper / Resolver is a no-op (returns
// zero stats) so production builds with no resolver wired stay
// safe.
func (s *Sweeper) Tick(ctx context.Context, now time.Time) (SweepStats, error) {
	var stats SweepStats
	if s == nil || s.Lister == nil || s.Writer == nil || s.Resolver == nil {
		return stats, nil
	}
	cutoff := now.Add(-s.Config.MaxAge)
	ids, err := s.Lister.ListPendingOutcomePlans(ctx, cutoff, s.Config.BatchSize)
	if err != nil {
		return stats, fmt.Errorf("planoutcome.Sweeper.Tick: list pending: %w", err)
	}
	stats.Scanned = len(ids)

	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			// Caller cancelled; surface the partial stats and
			// the cancellation error.
			return stats, err
		}
		s.resolveOne(ctx, id, &stats)
	}
	return stats, nil
}

func (s *Sweeper) resolveOne(parent context.Context, planID string, stats *SweepStats) {
	ctx, cancel := context.WithTimeout(parent, s.Config.PerPlanTimeout)
	defer cancel()

	out, ready, err := s.Resolver.ResolveForPlan(ctx, planID)
	if err != nil {
		stats.Errors++
		slog.Warn("planoutcome.Sweeper: ResolveForPlan failed", "planId", planID, "err", err)
		return
	}
	if !ready {
		stats.Pending++
		return
	}
	stats.Resolved++

	payload, err := Marshal(out)
	if err != nil {
		stats.Errors++
		slog.Warn("planoutcome.Sweeper: Marshal failed", "planId", planID, "err", err)
		return
	}
	if len(payload) == 0 {
		// Resolver claimed ready but produced a zero outcome.
		// Most likely a bug in the resolver; we choose to skip
		// the write rather than persist an empty {} that would
		// confuse the Wave-2 trackers' "is this resolved?"
		// query.
		stats.Errors++
		slog.Warn("planoutcome.Sweeper: resolver returned ready but zero outcome", "planId", planID)
		return
	}
	if err := s.Writer.SetPlanOutcome(ctx, planID, payload); err != nil {
		stats.Errors++
		slog.Warn("planoutcome.Sweeper: SetPlanOutcome failed", "planId", planID, "err", err)
		return
	}
	stats.WroteRows++
}
