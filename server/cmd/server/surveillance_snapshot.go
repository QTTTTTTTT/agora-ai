// surveillance_snapshot.go — adapter that pulls trade_executions
// for a fund + window into surveillance.TradeSnapshot, mirroring
// the recon snapshot adapter.
//
// What this does NOT do
//
//   - It does NOT compute average daily notional or VWAP. Those
//     are MarketContext fields that the caller fills from the
//     marketdata layer or fixture data. We surface a placeholder
//     populator (defaultMarketContext) so the loop and admin API
//     have a single attribution point when we wire those in.

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fundai/server/internal/surveillance"
)

// surveillanceSnapshotBuilder loads TradeSnapshot batches for the
// engine. Stateless after wiring.
type surveillanceSnapshotBuilder struct {
	db *sql.DB
}

func newSurveillanceSnapshotBuilder(db *sql.DB) *surveillanceSnapshotBuilder {
	if db == nil {
		return nil
	}
	return &surveillanceSnapshotBuilder{db: db}
}

// LoadParams scopes a single read.
type surveillanceLoadParams struct {
	FundID      string
	WindowStart time.Time
	WindowEnd   time.Time
}

// Load returns trade snapshots inside [start, end] for the fund.
// FundID empty = all funds (used by the cross-fund self-trade
// rule once we ship cross-trade prevention; today every rule
// scopes by fund so the caller passes a concrete FundID).
func (b *surveillanceSnapshotBuilder) Load(ctx context.Context, p surveillanceLoadParams) ([]surveillance.TradeSnapshot, error) {
	if b == nil || b.db == nil {
		return nil, errors.New("surveillance_snapshot: nil builder")
	}
	if p.WindowStart.IsZero() || p.WindowEnd.IsZero() {
		return nil, errors.New("surveillance_snapshot: window required")
	}
	args := []any{p.WindowStart.UTC(), p.WindowEnd.UTC()}
	where := "WHERE COALESCE(executed_at, created_at) >= $1 AND COALESCE(executed_at, created_at) <= $2 AND status IN ('filled', 'partial')"
	if strings.TrimSpace(p.FundID) != "" {
		args = append(args, p.FundID)
		where += fmt.Sprintf(" AND fund_id = $%d", len(args))
	}
	q := fmt.Sprintf(`
		SELECT id::text, fund_id::text,
		       COALESCE(symbol, ''),
		       COALESCE(instrument_key, ''),
		       COALESCE(side, ''),
		       COALESCE(filled_qty, quantity, 0),
		       COALESCE(filled_price, price, 0),
		       COALESCE(executed_at, created_at),
		       COALESCE(status, '')
		  FROM trade_executions
		  %s
		 ORDER BY COALESCE(executed_at, created_at), id
		 LIMIT 5000
	`, where)
	rows, err := b.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("surveillance_snapshot: query: %w", err)
	}
	defer rows.Close()
	var out []surveillance.TradeSnapshot
	for rows.Next() {
		var t surveillance.TradeSnapshot
		if err := rows.Scan(&t.ID, &t.FundID, &t.Symbol, &t.InstrumentKey,
			&t.Side, &t.Quantity, &t.Price, &t.ExecutedAt, &t.Status); err != nil {
			return nil, err
		}
		t.Notional = t.Quantity * t.Price
		out = append(out, t)
	}
	return out, rows.Err()
}

// defaultMarketContext returns a context whose AvgDailyNotional /
// RecentVWAP maps are empty. The marking-close rule will simply
// not flag the size or vwap halves until those are populated.
//
// Why surface this in code rather than at the call sites
//
// Future wiring will replace this with a marketdata-backed loader;
// keeping it as a single function lets us upgrade in one place.
func defaultMarketContext(close time.Time) *surveillance.MarketContext {
	return &surveillance.MarketContext{
		SessionClose:     close,
		AvgDailyNotional: map[string]float64{},
		RecentVWAP:       map[string]float64{},
	}
}
