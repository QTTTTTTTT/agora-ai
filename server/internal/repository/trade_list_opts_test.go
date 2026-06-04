package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// TestListByFundPageOpts_ExcludeChildSlices_PinsWhereClause asserts
// that the ExcludeChildSlices=true variant adds the
// "strategy_parent_trade_id IS NULL" filter to the WHERE clause.
// The query template uses a parametrised `$6 = false OR
// strategy_parent_trade_id IS NULL` short-circuit so a single
// query plan covers both call modes — but a regression that
// dropped the OR clause would silently return EVERY row
// regardless of the flag. The mock's strict SQL-text check
// catches that.
func TestListByFundPageOpts_ExcludeChildSlices_PinsWhereClause(t *testing.T) {
	repo, mock, cleanup := newMockedTradeRepo(t)
	defer cleanup()

	expectedSQL := regexp.QuoteMeta(`SELECT ` + tradeExecutionColumns + `
		 FROM trade_executions
		 WHERE fund_id = $1 AND created_at >= $2 AND created_at <= $3
		   AND ($6 = false OR strategy_parent_trade_id IS NULL)
		 ORDER BY created_at DESC LIMIT $4 OFFSET $5`)

	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(expectedSQL).
		WithArgs("fund-1", from, to, 100, 0, true).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "plan_id", "plan_action_id", "instrument_key", "symbol",
			"market", "exchange", "asset_class", "instrument_type", "side", "position_side",
			"open_close", "order_type", "quantity", "price", "amount", "filled_qty",
			"filled_price", "fee_commission", "fee_stamp_tax", "fee_transfer",
			"trading_mode", "broker_order_id", "mcp_server_id", "status", "executed_at",
			"quote_currency", "settlement_currency", "margin_mode", "leverage",
			"contract_multiplier", "expiry_date", "reduce_only", "slippage_pct",
			"stop_price", "trail_amount", "trail_percent", "display_qty",
			"time_in_force", "good_till_date", "parent_trade_id",
			"strategy", "strategy_parent_trade_id",
			"client_idempotency_key", "created_at",
			"cancelled_at", "cancel_reason", "replaced_at", "replace_count",
		})) // empty result is fine — we only care about the args + SQL match

	_, err := repo.ListByFundPageOpts(context.Background(), "fund-1", from, to, 100, 0,
		TradeListOpts{ExcludeChildSlices: true})
	if err != nil {
		t.Fatalf("ListByFundPageOpts: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestListByFundPage_PassesExcludeFalse exercises the
// backwards-compatibility path: the unsuffixed ListByFundPage
// must wire ExcludeChildSlices=false so existing callers see the
// full row set (including child slices) and pre-088 callers don't
// regress when migration 088 lands but no caller has been
// updated yet.
func TestListByFundPage_PassesExcludeFalse(t *testing.T) {
	repo, mock, cleanup := newMockedTradeRepo(t)
	defer cleanup()

	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery("FROM trade_executions").
		WithArgs("fund-1", from, to, 100, 0, false).
		WillReturnRows(sqlmock.NewRows([]string{"id"})) // shape doesn't matter for this assertion

	// We expect the scan to fail (insufficient columns) — what
	// matters is that the WithArgs($6=false) matched. Treat the
	// post-query scan error as "didn't reach the WithArgs check"
	// only if mock.ExpectationsWereMet flagged it.
	_, _ = repo.ListByFundPage(context.Background(), "fund-1", from, to, 100, 0)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestListByPlanOpts_ExcludeChildSlices_PinsWhereClause is the
// per-plan twin of the per-fund test above. Same safety reasoning
// applies — a regression dropping the OR would leak child rows.
func TestListByPlanOpts_ExcludeChildSlices_PinsWhereClause(t *testing.T) {
	repo, mock, cleanup := newMockedTradeRepo(t)
	defer cleanup()

	expectedSQL := regexp.QuoteMeta(`SELECT ` + tradeExecutionColumns + `
		 FROM trade_executions
		 WHERE plan_id = $1
		   AND ($2 = false OR strategy_parent_trade_id IS NULL)
		 ORDER BY created_at DESC, id DESC`)

	mock.ExpectQuery(expectedSQL).
		WithArgs("plan-1", true).
		WillReturnRows(sqlmock.NewRows([]string{"id"})) // shape doesn't matter

	_, _ = repo.ListByPlanOpts(context.Background(), "plan-1",
		TradeListOpts{ExcludeChildSlices: true})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestListChildrenByStrategyParent_PinsWhereClause asserts the
// child-drilldown query targets the strategy_parent_trade_id
// index (migration 088) — a regression that filtered on the
// wrong column (e.g. the legacy bracket parent_trade_id) would
// return OCO siblings instead of TWAP slices, mixing two unrelated
// parent-child semantics.
func TestListChildrenByStrategyParent_PinsWhereClause(t *testing.T) {
	repo, mock, cleanup := newMockedTradeRepo(t)
	defer cleanup()

	expectedSQL := regexp.QuoteMeta(`SELECT ` + tradeExecutionColumns + `
		 FROM trade_executions
		 WHERE strategy_parent_trade_id = $1
		 ORDER BY created_at, id`)

	mock.ExpectQuery(expectedSQL).
		WithArgs("trade-parent-1").
		WillReturnRows(sqlmock.NewRows([]string{"id"})) // empty shape

	_, _ = repo.ListChildrenByStrategyParent(context.Background(), "trade-parent-1")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestListChildrenByStrategyParent_EmptyIDIsZeroErrorPath: an
// empty parent ID is a programming error in the caller, but the
// helper degrades to "no children" rather than blowing up so the
// UI drilldown path is robust to a stale / missing parameter.
func TestListChildrenByStrategyParent_EmptyIDIsZeroErrorPath(t *testing.T) {
	repo, _, cleanup := newMockedTradeRepo(t)
	defer cleanup()

	rows, err := repo.ListChildrenByStrategyParent(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected empty result, got %d rows", len(rows))
	}
}
