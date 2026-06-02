// borrow_gate.go — production implementation of broker.BorrowGate.
//
// Glues the broker pre-trade hook to the securitiesborrow
// engine. The gate's responsibilities:
//
//   1. Determine how many shares the order would *borrow*. A
//      pure long-close order needs no locate; a sell that
//      grows a short position needs locate equal to the
//      surplus over the existing long.
//
//   2. Look up the borrow rate / availability calibration
//      from the in-memory cache (no DB on the hot path).
//
//   3. Ask the LocateEngine for an allow / reject decision.
//
//   4. Persist a security_locate_events row for audit, even
//      on allow (so PMs can see how many bps of locate fee
//      they're paying over time).
//
//   5. Convert the decision into a broker.BorrowVerdict.
//
// Fail-open
//
// Same posture as the other Sprint-6 gates: any error inside
// the adapter (cache miss, DB write failure, panic in the
// engine) returns Rejected=false plus a warning. Trading should
// not stop because a metadata table hiccupped; real-broker
// integration enforces locate server-side anyway. Every
// fail-open path bumps a metric.
//
// "No calibration" handling
//
// When the cache returns nil, we have a policy choice: fail
// open (allow with a warning) or fail closed (reject every
// short until calibration is loaded). The default is fail-open
// → log a warning + bump `no_calibration` metric. Operators
// can switch to fail-closed in production by flipping
// `RejectOnNoCalibration` once they're confident the
// calibration loader has populated rows for every shortable
// instrument.

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fundai/server/internal/broker"
	"github.com/fundai/server/internal/securitiesborrow"
)

// borrowGate is the cmd/server adapter implementing
// broker.BorrowGate.
type borrowGate struct {
	db                    *sql.DB
	repo                  *securitiesborrow.Repo
	cache                 *securitiesborrow.Cache
	engine                *securitiesborrow.LocateEngine
	metrics               borrowMetricsRecorder
	logger                leveledLogger
	now                   func() time.Time
	rejectOnNoCalibration bool
}

// borrowMetricsRecorder is the slice of *serverMetrics this
// gate needs.
type borrowMetricsRecorder interface {
	RecordBorrowEvent(event string)
}

// newBorrowGate constructs the gate. db + repo are needed for
// the locate-events audit log; cache is the hot lookup; engine
// is pure.
func newBorrowGate(
	db *sql.DB,
	repo *securitiesborrow.Repo,
	cache *securitiesborrow.Cache,
	metrics borrowMetricsRecorder,
	logger leveledLogger,
) *borrowGate {
	return &borrowGate{
		db:      db,
		repo:    repo,
		cache:   cache,
		engine:  securitiesborrow.NewLocateEngine(),
		metrics: metrics,
		logger:  logger,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// CheckOrder implements broker.BorrowGate.
func (g *borrowGate) CheckOrder(ctx context.Context, probe broker.BorrowProbe) broker.BorrowVerdict {
	if g == nil {
		return broker.BorrowVerdict{}
	}
	side := strings.ToLower(strings.TrimSpace(probe.Side))

	// Determine the short-borrow quantity. A buy never borrows;
	// a sell only borrows the surplus over existing long
	// position. The adapter has to load the position to compute
	// this.
	if side != "sell" {
		g.recordMetric("check_allow_non_sell")
		return broker.BorrowVerdict{}
	}
	posQty, err := g.fetchPositionQty(ctx, probe.FundID, probe.InstrumentKey)
	if err != nil {
		g.recordMetric("position_lookup_failed")
		g.warnf("borrow gate: fetch position: %v", err)
		// Fail-open with warning.
		return broker.BorrowVerdict{
			Warnings: []string{"borrow gate: position lookup failed; check skipped"},
		}
	}
	longSize := 0.0
	if posQty > 0 {
		longSize = posQty
	}
	shortQtyNeeded := probe.Quantity - longSize
	if shortQtyNeeded <= 0 {
		// Order is purely closing a long. No borrow needed.
		g.recordMetric("check_allow_no_borrow")
		return broker.BorrowVerdict{}
	}

	// Look up the calibration in the cache.
	rate := g.cacheLookup(probe.InstrumentKey)
	if rate == nil {
		g.recordMetric("no_calibration")
		// Audit-log even the "no calibration" path so operators
		// can grep "what shorts went through without supply
		// data".
		g.logEvent(ctx, probe, shortQtyNeeded, securitiesborrow.LocateNoCalibration, nil, "no calibration row for instrument")
		if g.rejectOnNoCalibration {
			return broker.BorrowVerdict{
				Rejected:     true,
				RejectReason: "borrow: no calibration data for instrument",
			}
		}
		return broker.BorrowVerdict{
			Warnings: []string{"borrow: no calibration data; allowing with default rate"},
		}
	}

	d := g.engine.Evaluate(securitiesborrow.LocateProbe{
		FundID:        probe.FundID,
		InstrumentKey: probe.InstrumentKey,
		Symbol:        probe.Symbol,
		RequestedQty:  shortQtyNeeded,
		IntendedPrice: probe.IntendedPrice,
		ClientOrderID: probe.ClientOrderID,
	}, rate)

	g.logEvent(ctx, probe, shortQtyNeeded, d.Kind, &d, d.Reason)
	g.recordMetricForDecision(d.Kind)

	verdict := broker.BorrowVerdict{}
	switch d.Kind {
	case securitiesborrow.LocateAllow:
		verdict.LocateFee = d.LocateFeeAmount
		switch rate.Availability {
		case securitiesborrow.AvailabilityHard:
			verdict.Warnings = append(verdict.Warnings, fmt.Sprintf(
				"borrow: hard-to-borrow at %.2f%%/yr",
				rate.BorrowRateBpsAnnual/100.0,
			))
		case securitiesborrow.AvailabilityRestricted:
			verdict.Warnings = append(verdict.Warnings, fmt.Sprintf(
				"borrow: restricted, borrow at %.2f%%/yr (admin override)",
				rate.BorrowRateBpsAnnual/100.0,
			))
		}
		if d.LocateFeeAmount > 0 {
			verdict.Warnings = append(verdict.Warnings, fmt.Sprintf(
				"locate fee: %.2f bps × %.2f notional = %.2f",
				rate.LocateFeeBps, d.Notional, d.LocateFeeAmount,
			))
		}
	case securitiesborrow.LocateRejectUnavail,
		securitiesborrow.LocateRejectInsuff,
		securitiesborrow.LocateRejectBelowMin,
		securitiesborrow.LocateRejectAboveMax:
		verdict.Rejected = true
		verdict.RejectReason = d.Reason
	default:
		// Unknown kind → defensive allow with a warning.
		verdict.Warnings = append(verdict.Warnings, fmt.Sprintf("borrow: unknown decision %q; allowing", d.Kind))
	}
	return verdict
}

// fetchPositionQty mirrors the lock-up gate's helper. We share
// the SQL pattern; could be lifted into a small holdings-read
// package later if a third gate needs it.
func (g *borrowGate) fetchPositionQty(ctx context.Context, fundID, instrumentKey string) (float64, error) {
	if g == nil || g.db == nil {
		return 0, errors.New("borrow gate: nil db")
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

func (g *borrowGate) cacheLookup(instrumentKey string) *securitiesborrow.BorrowRate {
	if g == nil || g.cache == nil {
		return nil
	}
	return g.cache.Lookup(instrumentKey)
}

func (g *borrowGate) logEvent(
	ctx context.Context,
	probe broker.BorrowProbe,
	requestedQty float64,
	kind securitiesborrow.LocateDecisionKind,
	d *securitiesborrow.LocateDecision,
	reason string,
) {
	if g == nil || g.repo == nil {
		return
	}
	p := securitiesborrow.LogLocateEventParams{
		FundID:        probe.FundID,
		InstrumentKey: probe.InstrumentKey,
		Symbol:        probe.Symbol,
		RequestedQty:  requestedQty,
		Decision:      kind,
		Reason:        reason,
		ClientOrderID: probe.ClientOrderID,
	}
	if probe.IntendedPrice > 0 {
		v := probe.IntendedPrice
		p.IntendedPrice = &v
	}
	if d != nil {
		if d.BorrowRateBps > 0 {
			v := d.BorrowRateBps
			p.RateBpsAnnual = &v
		}
		if d.LocateFeeBps > 0 {
			v := d.LocateFeeBps
			p.LocateFeeBps = &v
		}
		if d.LocateFeeAmount > 0 {
			v := d.LocateFeeAmount
			p.LocateFeeAmount = &v
		}
		if d.Notional > 0 {
			v := d.Notional
			p.Notional = &v
		}
	}
	if _, err := g.repo.LogLocateEvent(ctx, p); err != nil {
		g.recordMetric("audit_log_failed")
		g.warnf("borrow gate: log locate event: %v", err)
	}
}

func (g *borrowGate) recordMetric(event string) {
	if g == nil || g.metrics == nil {
		return
	}
	g.metrics.RecordBorrowEvent(event)
}

func (g *borrowGate) recordMetricForDecision(kind securitiesborrow.LocateDecisionKind) {
	switch kind {
	case securitiesborrow.LocateAllow:
		g.recordMetric("check_allow_short")
	case securitiesborrow.LocateRejectUnavail:
		g.recordMetric("check_reject_unavailable")
	case securitiesborrow.LocateRejectInsuff:
		g.recordMetric("check_reject_insufficient")
	case securitiesborrow.LocateRejectBelowMin:
		g.recordMetric("check_reject_below_min")
	case securitiesborrow.LocateRejectAboveMax:
		g.recordMetric("check_reject_above_max")
	default:
		g.recordMetric("check_unknown")
	}
}

func (g *borrowGate) warnf(format string, args ...any) {
	if g == nil || g.logger == nil {
		return
	}
	g.logger.Warn(fmt.Sprintf(format, args...))
}
