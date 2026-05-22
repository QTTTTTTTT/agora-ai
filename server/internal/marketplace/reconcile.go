// Package marketplace contains background workers that keep marketplace
// state consistent with the wallet ledger.
//
// The Reconciler scans for orders that have been stuck in `pending` past a
// configurable cutoff. For each one it inspects the wallet ledger for the
// matching debit and credit entries (keyed by reference_id == order_id) and
// classifies the divergence:
//
//   - no ledger entries        -> mark order as `failed`. The buyer never
//                                 paid. Listing remains active.
//   - debit only / credit only -> record an unresolved finding. A human
//                                 operator must investigate.
//   - both entries present     -> the commit was lost mid-flight; promote
//                                 the order to `completed` (only if the
//                                 delivered agent id is set) or flag.
//
// Findings are appended to marketplace_reconcile_log so audits can replay
// the timeline of every order.
package marketplace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fundai/server/internal/repository"
)

// Reconciler is the cron entry point.
type Reconciler struct {
	db              *sql.DB
	uow             repository.UnitOfWork
	marketplaceRepo *repository.MarketplaceRepo

	// PendingCutoff is the minimum age at which an order in `pending`
	// becomes a reconcile candidate. Defaults to 5 minutes.
	PendingCutoff time.Duration

	// BatchSize caps how many pending orders the cron processes per run.
	BatchSize int
}

// NewReconciler wires the cron with sensible defaults. The caller is
// expected to drive Run from a scheduler.
func NewReconciler(db *sql.DB, marketplaceRepo *repository.MarketplaceRepo) *Reconciler {
	return &Reconciler{
		db:              db,
		uow:             repository.NewUnitOfWork(db),
		marketplaceRepo: marketplaceRepo,
		PendingCutoff:   5 * time.Minute,
		BatchSize:       100,
	}
}

// Run executes a single reconciliation pass.
func (r *Reconciler) Run(ctx context.Context) (Summary, error) {
	if r == nil || r.marketplaceRepo == nil {
		return Summary{}, errors.New("marketplace.reconcile: not initialised")
	}
	cutoff := time.Now().Add(-r.PendingCutoff)
	orders, err := r.marketplaceRepo.ListPendingOrdersOlderThan(ctx, cutoff, r.BatchSize)
	if err != nil {
		return Summary{}, fmt.Errorf("marketplace.reconcile: list pending: %w", err)
	}
	summary := Summary{Inspected: len(orders)}
	for i := range orders {
		outcome, err := r.processOrder(ctx, &orders[i])
		if err != nil {
			summary.Errored++
			detail, _ := json.Marshal(map[string]string{"reason": err.Error()})
			_ = r.marketplaceRepo.RecordReconcileFinding(ctx, orders[i].ID, orders[i].ListingID, "process_error", detail, false)
			continue
		}
		switch outcome {
		case outcomeFailed:
			summary.MarkedFailed++
		case outcomeUnresolved:
			summary.UnresolvedFindings++
		case outcomeNoChange:
			summary.NoChange++
		}
	}
	return summary, nil
}

// Summary is a structured trace of the cron's last run, useful in metrics.
type Summary struct {
	Inspected          int
	MarkedFailed       int
	UnresolvedFindings int
	NoChange           int
	Errored            int
}

type outcome int

const (
	outcomeNoChange outcome = iota
	outcomeFailed
	outcomeUnresolved
)

// processOrder inspects the ledger for a single pending order and decides
// what to do. Each branch records a finding so the operator-facing
// dashboard always has an audit row to refer to.
func (r *Reconciler) processOrder(ctx context.Context, order *repository.AgentMarketOrder) (outcome, error) {
	debit, credit, err := r.lookupLedgerEntries(ctx, order.ID)
	if err != nil {
		return outcomeNoChange, err
	}
	switch {
	case debit == 0 && credit == 0:
		// No money moved. Safe to fail the order so the listing flips
		// back to active via the cron's secondary pass (or stays sold,
		// depending on operator policy — we leave the listing alone here
		// since flipping it back is a more invasive change).
		txErr := r.uow.WithinTx(ctx, func(tx *sql.Tx) error {
			return r.marketplaceRepo.MarkOrderFailedWithTx(ctx, tx, order.ID, "no ledger entries within cutoff")
		})
		if txErr != nil {
			return outcomeNoChange, txErr
		}
		detail, _ := json.Marshal(map[string]any{
			"order_id":     order.ID,
			"listing_id":   order.ListingID,
			"buyer":        order.BuyerUserID,
			"amount_minor": order.AmountMinor,
		})
		_ = r.marketplaceRepo.RecordReconcileFinding(ctx, order.ID, order.ListingID, "no_ledger_entries", detail, true)
		return outcomeFailed, nil
	case debit != credit:
		detail, _ := json.Marshal(map[string]any{
			"order_id":     order.ID,
			"debit_count":  debit,
			"credit_count": credit,
		})
		_ = r.marketplaceRepo.RecordReconcileFinding(ctx, order.ID, order.ListingID, "ledger_mismatch", detail, false)
		return outcomeUnresolved, nil
	default:
		// Both sides booked but order is still pending: the outer commit
		// likely failed after the wallet rows were inserted by a prior
		// process (should not happen with the WithinTx flow, but kept as
		// a defensive branch). Leave a finding for human review — we
		// cannot blindly complete because we don't know the delivered
		// agent id.
		detail, _ := json.Marshal(map[string]any{
			"order_id":     order.ID,
			"debit_count":  debit,
			"credit_count": credit,
			"reason":       "ledger booked but order pending",
		})
		_ = r.marketplaceRepo.RecordReconcileFinding(ctx, order.ID, order.ListingID, "split_brain_pending", detail, false)
		return outcomeUnresolved, nil
	}
}

func (r *Reconciler) lookupLedgerEntries(ctx context.Context, orderID string) (int, int, error) {
	var debit, credit int
	err := r.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN amount_minor < 0 THEN 1 ELSE 0 END), 0) AS debit,
			COALESCE(SUM(CASE WHEN amount_minor > 0 THEN 1 ELSE 0 END), 0) AS credit
		FROM wallet_ledger_entries
		WHERE reference_type = 'agent_market_order' AND reference_id = $1
	`, orderID).Scan(&debit, &credit)
	if err != nil {
		return 0, 0, fmt.Errorf("marketplace.reconcile: lookup ledger: %w", err)
	}
	return debit, credit, nil
}
