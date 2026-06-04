package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/repository"
)

// TestRecordCashLedgerFutures_OpenWritesMarginPost pins the T7
// futures cash flow contract for an OPEN trade on a fund that's
// opted into the v2 model. Expected legs:
//
//   futures_margin_post   amount = -initialMargin (debit at open)
//   trade_buy_commission  amount = -feeCommission (negative)
//   trade_buy_transfer    amount = -feeTransfer   (skipped if zero)
//   trade_buy_stamp_tax   amount = -feeStampTax   (skipped if zero)
//
// The notable absence: futures_realized_pnl is NEVER written on
// an open (you can't realize PnL by opening a position). The
// realized_pnl leg is open/close-asymmetric by design.
func TestRecordCashLedgerFutures_OpenWritesMarginPost(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	// Pre-flag-on baseline: regex pattern targets the v2 entry
	// types so the test fails if the dispatcher accidentally
	// fell back to trade_buy_notional (the v1 path).
	insertPattern := regexp.MustCompile(`INSERT INTO cash_ledger`)
	// Order matters: dispatcher writes margin_post, then
	// commission, then transfer, then stamp tax. transfer +
	// stamp legs with amount=0 are skipped, so only 2 rows.
	mock.ExpectQuery(insertPattern.String()).
		WithArgs(
			"fund-1",
			sqlmock.AnyArg(), // posted_at
			sqlmock.AnyArg(), // trading_date
			repository.CashEntryFuturesMarginPost,
			-10000.0, // margin = notional / leverage = 100000 / 10
			"USD",
			"trade-fut-1", "plan-1", "action-1",
			"", "", // corp_action_id, broker_link_id
			sqlmock.AnyArg(), // description
			sqlmock.AnyArg(), // metadata
			"",               // created_by
			sqlmock.AnyArg(), // idempotency_key
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ledger-row-margin"))
	mock.ExpectQuery(insertPattern.String()).
		WithArgs(
			"fund-1",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			repository.CashEntryTradeBuyCommission,
			-5.0,
			"USD",
			"trade-fut-1", "plan-1", "action-1",
			"", "",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			"",
			sqlmock.AnyArg(),
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ledger-row-commission"))

	engine := &runtimeTradingEngine{cashLedger: repository.NewCashLedgerRepo(db)}

	fund := &repository.Fund{
		ID:     "fund-1",
		Config: json.RawMessage(`{"futures_cash_ledger_v2": true}`),
	}
	plan := &repository.InvestmentPlan{ID: "plan-1"}
	action := repository.PlanAction{
		ID:         "action-1",
		Symbol:     "ESU2026",
		AssetClass: sql.NullString{String: "futures", Valid: true},
		OpenClose:  sql.NullString{String: "open", Valid: true},
		Leverage:   sql.NullFloat64{Float64: 10, Valid: true},
	}

	engine.recordCashLedgerForFill(
		context.Background(),
		fund, plan, action,
		"trade-fut-1",
		"buy", 10, 5000.0,
		100000.0, // notional
		5.0, 0.0, 0.0,
		time.Now(),
		sql.NullFloat64{}, // no realized PnL on open
	)
	assertMockExpectations(t, mock)
}

// TestRecordCashLedgerFutures_CloseWritesMarginReleasePlusPnL
// pins the close path: margin gets credited back to cash,
// realized PnL lands as its own signed leg, and the close-side
// commission leg uses trade_sell_commission (not trade_buy_*).
//
// PnL sign is positive here (a profitable close) — a separate
// matrix test would cover the negative case but the dispatcher
// is sign-agnostic so one happy-path test is enough to pin the
// shape.
func TestRecordCashLedgerFutures_CloseWritesMarginReleasePlusPnL(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	insertPattern := regexp.MustCompile(`INSERT INTO cash_ledger`)

	mock.ExpectQuery(insertPattern.String()).
		WithArgs(
			"fund-1",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			repository.CashEntryFuturesMarginRelease,
			10000.0, // positive = margin returned to cash
			"USD",
			"trade-fut-close", "plan-1", "action-1",
			"", "",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			"",
			sqlmock.AnyArg(),
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ledger-row-margin-rel"))
	mock.ExpectQuery(insertPattern.String()).
		WithArgs(
			"fund-1",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			repository.CashEntryTradeSellCommission,
			-5.0,
			"USD",
			"trade-fut-close", "plan-1", "action-1",
			"", "",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			"",
			sqlmock.AnyArg(),
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ledger-row-comm-close"))
	// Realized PnL leg comes LAST in the dispatcher's leg list.
	// Caller passes +1500 (profitable close); the dispatcher
	// writes it directly without sign-flipping.
	mock.ExpectQuery(insertPattern.String()).
		WithArgs(
			"fund-1",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			repository.CashEntryFuturesRealizedPnL,
			1500.0,
			"USD",
			"trade-fut-close", "plan-1", "action-1",
			"", "",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			"",
			sqlmock.AnyArg(),
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ledger-row-pnl"))

	engine := &runtimeTradingEngine{cashLedger: repository.NewCashLedgerRepo(db)}
	fund := &repository.Fund{
		ID:     "fund-1",
		Config: json.RawMessage(`{"futures_cash_ledger_v2": true}`),
	}
	plan := &repository.InvestmentPlan{ID: "plan-1"}
	action := repository.PlanAction{
		ID:         "action-1",
		Symbol:     "ESU2026",
		AssetClass: sql.NullString{String: "futures", Valid: true},
		OpenClose:  sql.NullString{String: "close", Valid: true},
		Leverage:   sql.NullFloat64{Float64: 10, Valid: true},
	}

	engine.recordCashLedgerForFill(
		context.Background(),
		fund, plan, action,
		"trade-fut-close",
		"sell", 10, 5150.0,
		100000.0,
		5.0, 0.0, 0.0,
		time.Now(),
		sql.NullFloat64{Float64: 1500.0, Valid: true},
	)
	assertMockExpectations(t, mock)
}

// TestRecordCashLedgerFutures_FlagOffKeepsLegacyPath asserts the
// safety floor: a futures action on a fund WITHOUT the v2 flag
// goes through the legacy trade_buy_notional / trade_sell_notional
// path. This is the path that all production funds use today;
// flipping it on accidentally would change funds.current_capital
// math overnight.
func TestRecordCashLedgerFutures_FlagOffKeepsLegacyPath(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	insertPattern := regexp.MustCompile(`INSERT INTO cash_ledger`)
	// Two legacy legs only: trade_buy_notional + trade_buy_commission.
	// futures_* entry types must NOT appear — sqlmock would surface
	// the unexpected query as an unmet expectation if they did.
	mock.ExpectQuery(insertPattern.String()).
		WithArgs(
			"fund-1",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			repository.CashEntryTradeBuyNotional,
			-100000.0,
			"USD",
			"trade-fut-legacy", "plan-1", "action-1",
			"", "",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			"",
			sqlmock.AnyArg(),
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("legacy-notional"))
	mock.ExpectQuery(insertPattern.String()).
		WithArgs(
			"fund-1",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			repository.CashEntryTradeBuyCommission,
			-5.0,
			"USD",
			"trade-fut-legacy", "plan-1", "action-1",
			"", "",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			"",
			sqlmock.AnyArg(),
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("legacy-commission"))

	engine := &runtimeTradingEngine{cashLedger: repository.NewCashLedgerRepo(db)}
	fund := &repository.Fund{
		ID:     "fund-1",
		Config: json.RawMessage(`{}`), // v2 flag explicitly absent
	}
	plan := &repository.InvestmentPlan{ID: "plan-1"}
	action := repository.PlanAction{
		ID:         "action-1",
		Symbol:     "ESU2026",
		AssetClass: sql.NullString{String: "futures", Valid: true},
		OpenClose:  sql.NullString{String: "open", Valid: true},
		Leverage:   sql.NullFloat64{Float64: 10, Valid: true},
	}

	engine.recordCashLedgerForFill(
		context.Background(),
		fund, plan, action,
		"trade-fut-legacy",
		"buy", 10, 5000.0,
		100000.0,
		5.0, 0.0, 0.0,
		time.Now(),
		sql.NullFloat64{},
	)
	assertMockExpectations(t, mock)
}
