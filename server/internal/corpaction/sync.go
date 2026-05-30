// Package corpaction also exposes the high-level "sync from
// provider → ledger → applier" pipeline so it can be driven by:
//
//   - the corpactionsync CLI (operator one-shot sweeps), and
//   - the daily scheduler loop in cmd/server (P1-1).
//
// Why a shared file rather than duplicating logic in both callers:
// the rules around idempotency, partial-failure isolation, and
// "non-zero-quantity holders only" are easy to drift on. Pinning
// them here and giving both callers a thin wrapper means a fix to
// the algorithm lands in both surfaces in one PR.
//
// This file only provides building blocks; it does not own the
// loop / cron — the daemon orchestrates timing because that's a
// deployment concern, not a domain concern.

package corpaction

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// FundResolver returns the fund IDs that currently hold a given
// instrument with a non-zero quantity. Callers pass it in instead
// of hard-coding the SQL so that:
//
//   - tests can supply a stub (no DB),
//   - the scheduler can layer additional filtering (e.g. only
//     active funds in this region) without forking the SQL here,
//   - the corpactionsync CLI can reuse the default DB-backed
//     resolver via DefaultFundResolver(db).
type FundResolver func(ctx context.Context, instrumentKey string) ([]string, error)

// DefaultFundResolver returns a resolver backed by holding_positions.
// quantity > 0 is intentional — funds that already cleared a position
// shouldn't get a delayed corp-action applied to a 0-share holding.
func DefaultFundResolver(db *sql.DB) FundResolver {
	return func(ctx context.Context, instrumentKey string) ([]string, error) {
		rows, err := db.QueryContext(ctx,
			`SELECT DISTINCT fund_id FROM holding_positions
			  WHERE instrument_key = $1 AND quantity > 0`,
			instrumentKey,
		)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := make([]string, 0, 4)
		for rows.Next() {
			var fid string
			if err := rows.Scan(&fid); err != nil {
				return nil, err
			}
			out = append(out, fid)
		}
		return out, rows.Err()
	}
}

// ApplyOutcome captures what the per-event apply pass did. Used by
// the scheduler loop's structured log + by the CLI report.
type ApplyOutcome struct {
	// AppliedFunds is the count of fund IDs the applier touched
	// successfully (including idempotent no-ops — the downstream
	// invariant is satisfied either way).
	AppliedFunds int
	// SkippedZeroPosition is the count of fund IDs that had a row
	// in holding_positions when we resolved holders but no rows in
	// position_lots / a zeroed quantity by the time the applier
	// took the row lock. Soft signal — not an error, but worth
	// logging if it grows over time (could indicate a race with
	// the trade settlement path).
	SkippedZeroPosition int
	// Errors captures every non-recoverable error per fund. Returning
	// these as a slice (rather than the first one) lets callers log
	// a fan-out summary without losing visibility into the others.
	Errors []FundApplyError
}

// FundApplyError ties an apply error to the fund it failed on so
// the caller can render a useful audit trail.
type FundApplyError struct {
	FundID string
	Err    error
}

// ApplyEventToFunds is the inner fan-out that the CLI's
// applyEventToFunds and the scheduler's loop both call. It encodes:
//
//   - idempotent re-application is treated as success (Applier
//     already handled the gate; we count it under AppliedFunds);
//   - ErrPositionMissing → SkippedZeroPosition (race with settlement);
//   - any other error is collected into Errors and the loop continues
//     so one bad fund doesn't poison the rest of the fan-out.
//
// The event MUST already carry a non-empty ID (i.e., it has been
// upserted into corporate_actions). The CLI's Upsert path provides
// this; the scheduler's wrapper does the same.
func ApplyEventToFunds(ctx context.Context, db *sql.DB, evt Event, fundIDs []string) ApplyOutcome {
	out := ApplyOutcome{}
	if evt.ID == "" {
		out.Errors = append(out.Errors, FundApplyError{
			Err: fmt.Errorf("corpaction: ApplyEventToFunds called with empty Event.ID"),
		})
		return out
	}
	for _, fid := range fundIDs {
		evt := evt
		_, err := ApplyEvent(ctx, db, evt, fid)
		if err != nil {
			if errors.Is(err, ErrPositionMissing) {
				out.SkippedZeroPosition++
				continue
			}
			out.Errors = append(out.Errors, FundApplyError{
				FundID: fid,
				Err:    err,
			})
			continue
		}
		out.AppliedFunds++
	}
	return out
}
