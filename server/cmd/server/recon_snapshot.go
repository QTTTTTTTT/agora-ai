// recon_snapshot.go — adapter that builds a recon.InternalSnapshot
// for a fund + date, reading from holding_positions / cash_ledger /
// trade_executions.
//
// Why not put this in internal/recon?
//
// The recon package is intentionally repository-agnostic — it only
// knows the shape it consumes (`InternalSnapshot`). The adapter
// lives in cmd/server because it's the only layer that already
// owns *repository.PositionRepo / *CashLedgerRepo / *FundRepo.
//
// What this adapter does
//
//   - Pulls every row out of holding_positions for the fund and
//     maps it into recon.InternalPosition.
//   - Reads cash_ledger and groups by currency to produce a
//     recon.InternalCash slice (the "as of EOD" balance per
//     currency on the given date).
//   - Reads trade_executions for the same trading day and maps
//     each filled row into recon.InternalTrade. We use
//     broker_order_id as ExternalRef when set (real fills) and
//     fall back to client_idempotency_key (simulator-friendly).
//
// One snapshot = one moment in time. Re-reading is cheap (three
// queries) and we never cache; the daily loop calls this fresh
// on every run.

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fundai/server/internal/recon"
	"github.com/fundai/server/internal/repository"
)

// reconSnapshotBuilder constructs InternalSnapshots from the
// platform's primary stores. Stateless after wiring, so a single
// instance can be shared across the daily loop and the admin API.
type reconSnapshotBuilder struct {
	db           *sql.DB
	positionRepo *repository.PositionRepo
	cashLedger   *repository.CashLedgerRepo
}

// newReconSnapshotBuilder is the standard constructor. nil DB is
// rejected at first call rather than at construction so the wiring
// path can defer DB attachment in the same shape as the FX/funding
// builders.
func newReconSnapshotBuilder(db *sql.DB) *reconSnapshotBuilder {
	if db == nil {
		return nil
	}
	return &reconSnapshotBuilder{
		db:           db,
		positionRepo: repository.NewPositionRepo(db),
		cashLedger:   repository.NewCashLedgerRepo(db),
	}
}

// Build returns a freshly read snapshot for (fund, asOf). The
// asOf date is treated as inclusive: cash_ledger entries booked
// up to end-of-day asOf count, trade_executions executed on asOf
// count.
func (b *reconSnapshotBuilder) Build(ctx context.Context, fundID string, asOf time.Time) (*recon.InternalSnapshot, error) {
	if b == nil || b.db == nil {
		return nil, errors.New("recon_snapshot: nil builder")
	}
	if strings.TrimSpace(fundID) == "" {
		return nil, errors.New("recon_snapshot: fund_id required")
	}

	// 1) Positions.
	positions, err := b.loadPositions(ctx, fundID)
	if err != nil {
		return nil, err
	}

	// 2) Cash. We read from the start of time → end-of-day asOf
	// so the running balance reflects every booking.
	cashByCcy, err := b.cashLedger.SubtotalByCurrency(ctx, fundID,
		time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
		endOfDay(asOf),
	)
	if err != nil {
		return nil, fmt.Errorf("recon_snapshot: cash subtotal: %w", err)
	}
	cash := make([]recon.InternalCash, 0, len(cashByCcy))
	for ccy, bal := range cashByCcy {
		cash = append(cash, recon.InternalCash{
			Currency: strings.ToUpper(strings.TrimSpace(ccy)),
			Balance:  bal,
		})
	}

	// 3) Trades for the day.
	trades, err := b.loadTradesForDay(ctx, fundID, asOf)
	if err != nil {
		return nil, err
	}

	return &recon.InternalSnapshot{
		FundID:    fundID,
		AsOfDate:  asOf,
		Positions: positions,
		Cash:      cash,
		Trades:    trades,
	}, nil
}

func (b *reconSnapshotBuilder) loadPositions(ctx context.Context, fundID string) ([]recon.InternalPosition, error) {
	rows, err := b.positionRepo.ListByFund(ctx, fundID)
	if err != nil {
		return nil, fmt.Errorf("recon_snapshot: list positions: %w", err)
	}
	out := make([]recon.InternalPosition, 0, len(rows))
	for _, p := range rows {
		ccy := ""
		if p.SettlementCurrency.Valid {
			ccy = p.SettlementCurrency.String
		} else if p.QuoteCurrency.Valid {
			ccy = p.QuoteCurrency.String
		}
		out = append(out, recon.InternalPosition{
			Symbol:   p.Symbol,
			Quantity: p.Quantity,
			AvgCost:  p.CostPrice,
			Currency: strings.ToUpper(strings.TrimSpace(ccy)),
		})
	}
	return out, nil
}

func (b *reconSnapshotBuilder) loadTradesForDay(ctx context.Context, fundID string, day time.Time) ([]recon.InternalTrade, error) {
	from, to := startOfDay(day), endOfDay(day)
	// We pull only "filled" or "partial" rows since pending /
	// cancelled / rejected don't represent broker-side movement.
	rows, err := b.db.QueryContext(ctx, `
		SELECT id::text,
		       COALESCE(broker_order_id, ''),
		       COALESCE(client_idempotency_key, ''),
		       symbol,
		       side,
		       COALESCE(filled_qty, quantity),
		       COALESCE(filled_price, price, 0),
		       COALESCE(fee_commission, 0) + COALESCE(fee_stamp_tax, 0) + COALESCE(fee_transfer, 0),
		       COALESCE(quote_currency, ''),
		       COALESCE(executed_at, created_at)
		  FROM trade_executions
		 WHERE fund_id = $1
		   AND status IN ('filled', 'partial')
		   AND COALESCE(executed_at, created_at) >= $2
		   AND COALESCE(executed_at, created_at) <= $3
		 ORDER BY COALESCE(executed_at, created_at), id
	`, fundID, from, to)
	if err != nil {
		return nil, fmt.Errorf("recon_snapshot: list trades: %w", err)
	}
	defer rows.Close()

	var trades []recon.InternalTrade
	for rows.Next() {
		var t recon.InternalTrade
		var brokerOrderID, idempotencyKey string
		if err := rows.Scan(&t.ID, &brokerOrderID, &idempotencyKey,
			&t.Symbol, &t.Side, &t.Quantity, &t.Price, &t.Fee,
			&t.Currency, &t.ExecutedAt); err != nil {
			return nil, err
		}
		// Prefer broker_order_id (real-broker case); fall back to
		// idempotency key (simulator + early P0-4 work).
		t.ExternalRef = strings.TrimSpace(brokerOrderID)
		if t.ExternalRef == "" {
			t.ExternalRef = strings.TrimSpace(idempotencyKey)
		}
		t.Currency = strings.ToUpper(strings.TrimSpace(t.Currency))
		trades = append(trades, t)
	}
	return trades, rows.Err()
}

// startOfDay / endOfDay locate the [from, to] window for the
// trading day. We do this in UTC rather than fund-local time
// because trade_executions.executed_at is stored UTC. A future
// per-fund timezone would need to layer onto this — for now the
// platform's simulator + admin clock is UTC, which matches.
func startOfDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func endOfDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, int(time.Second-time.Nanosecond), time.UTC)
}
