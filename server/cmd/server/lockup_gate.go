// lockup_gate.go — production implementation of broker.LockupGate.
//
// Glues the broker pre-trade hook to the lockup engine. The gate
// loads:
//
//   - the active lockup records for (fund, instrument) at now()
//   - the current position quantity from holding_positions
//
// then asks lockup.Engine.Evaluate for a verdict, persists the
// admin event, and returns to the simulator.
//
// Fail-open
//
// Same posture as the market-status gate: if the DB lookup, the
// snapshot build, or the engine evaluation errors, the gate
// returns Rejected=false, logs a warning and emits a metric.
// Trading should not stop because a metadata table hiccupped;
// real lock-up enforcement is the real broker's job once we
// migrate off the simulator.

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fundai/server/internal/broker"
	"github.com/fundai/server/internal/lockup"
)

// lockupGate is the cmd/server adapter implementing
// broker.LockupGate.
type lockupGate struct {
	db      *sql.DB
	repo    *lockup.Repo
	engine  *lockup.Engine
	metrics lockupMetricsRecorder
	logger  leveledLogger
	now     func() time.Time
}

// lockupMetricsRecorder is the slice of *serverMetrics this
// gate needs. Decoupling it via an interface keeps lockupGate
// nilable in tests.
type lockupMetricsRecorder interface {
	RecordLockupEvent(event string)
}

// newLockupGate constructs the gate. db is required; metrics
// and logger are nil-safe.
func newLockupGate(db *sql.DB, metrics lockupMetricsRecorder, logger leveledLogger) *lockupGate {
	return &lockupGate{
		db:      db,
		repo:    lockup.NewRepo(db),
		engine:  lockup.NewEngine(),
		metrics: metrics,
		logger:  logger,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// CheckOrder implements broker.LockupGate.
func (g *lockupGate) CheckOrder(ctx context.Context, probe broker.LockupProbe) broker.LockupVerdict {
	if g == nil {
		return broker.LockupVerdict{}
	}
	side := strings.ToLower(strings.TrimSpace(probe.Side))
	// Buys are never blocked by lock-ups. Short-circuit before
	// the DB to avoid a wasted round-trip.
	if side != "sell" {
		g.recordMetric("check_allow_non_sell")
		return broker.LockupVerdict{}
	}
	if g.repo == nil || g.db == nil {
		// No store wired → equivalent to "no lock-ups configured".
		// Allow the order; recordMetric is best-effort.
		g.recordMetric("check_no_repo")
		return broker.LockupVerdict{}
	}

	asOf := g.now()

	// Load active records — the hot DB query covered by the
	// (fund_id, instrument_key, locked_until) partial index.
	records, err := g.repo.ListActiveFor(ctx, probe.FundID, probe.InstrumentKey, asOf)
	if err != nil {
		g.recordMetric("gate_lookup_failed")
		g.warnf("lockup gate: list active: %v", err)
		return broker.LockupVerdict{}
	}
	if len(records) == 0 {
		g.recordMetric("check_allow_no_lockup")
		return broker.LockupVerdict{}
	}

	// Load position quantity. Returns 0 when the row doesn't
	// exist (sql.ErrNoRows); the engine then reports
	// reject_no_position which the simulator converts to
	// ErrLockupRejected.
	posQty, err := g.fetchPositionQty(ctx, probe.FundID, probe.InstrumentKey)
	if err != nil {
		g.recordMetric("position_lookup_failed")
		g.warnf("lockup gate: fetch position: %v", err)
		// Fail-open: when we can't read the position, we cannot
		// accurately enforce the lock-up. Allow the order with a
		// warning so downstream risk can react. This deliberately
		// errs on the side of executing rather than blocking on
		// metadata gaps.
		return broker.LockupVerdict{
			Warnings: []string{"lock-up gate: position lookup failed; check skipped"},
		}
	}

	d := g.engine.Evaluate(lockup.Probe{
		FundID:        probe.FundID,
		InstrumentKey: probe.InstrumentKey,
		Symbol:        probe.Symbol,
		Side:          side,
		Quantity:      probe.Quantity,
		PositionQty:   posQty,
		AsOf:          asOf,
	}, lockup.Snapshot{Records: records})

	verdict := broker.LockupVerdict{}
	switch d.Kind {
	case lockup.DecisionAllow:
		g.recordMetric("check_allow")
		// Add a warning when we're within 7 days of the next
		// unlock, so the operator notices the position is partly
		// constrained even though this order made it through.
		if d.NextUnlockAt != nil && d.NextUnlockAt.Sub(asOf) <= 7*24*time.Hour {
			verdict.Warnings = append(verdict.Warnings, fmt.Sprintf(
				"lock-up: %v shares unlock at %s",
				d.LockedQty, d.NextUnlockAt.UTC().Format(time.RFC3339),
			))
		}
	case lockup.DecisionAllowNonSell:
		// Should not reach here because we filtered on "sell"
		// above; defensive metric for completeness.
		g.recordMetric("check_allow_non_sell")
	case lockup.DecisionAllowNoLockup:
		// Same — the records-empty check above already handled
		// this; defensive.
		g.recordMetric("check_allow_no_lockup")
	case lockup.DecisionRejectLocked:
		g.recordMetric("check_reject_locked")
		verdict.Rejected = true
		verdict.RejectReason = d.Reason
	case lockup.DecisionRejectNoPos:
		g.recordMetric("check_reject_no_position")
		verdict.Rejected = true
		verdict.RejectReason = d.Reason
	default:
		g.recordMetric("check_unknown")
		verdict.Warnings = append(verdict.Warnings, "lock-up: unknown decision; allowing")
	}
	return verdict
}

// fetchPositionQty pulls the position quantity for a fund×
// instrument. We query just the column we need to keep this
// hot-path read cheap — the gate is invoked on every sell order.
func (g *lockupGate) fetchPositionQty(ctx context.Context, fundID, instrumentKey string) (float64, error) {
	if g == nil || g.db == nil {
		return 0, errors.New("lockup gate: nil db")
	}
	var qty float64
	err := g.db.QueryRowContext(ctx, `
		SELECT COALESCE(quantity, 0)
		  FROM holding_positions
		 WHERE fund_id = $1 AND instrument_key = $2
	`, fundID, instrumentKey).Scan(&qty)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return qty, nil
}

func (g *lockupGate) recordMetric(event string) {
	if g == nil || g.metrics == nil {
		return
	}
	g.metrics.RecordLockupEvent(event)
}

func (g *lockupGate) warnf(format string, args ...any) {
	if g == nil || g.logger == nil {
		return
	}
	g.logger.Warn(fmt.Sprintf(format, args...))
}
