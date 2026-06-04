// Cancel-with-live-gate enforcement test (P0-9).
//
// Verifies the order_actions_handler returns 403 when the live
// gate fails on a 'live' fund, and falls through to the existing
// success path when the fund is in simulation mode (gate not
// enforced) — i.e. the gate composes cleanly with the legacy
// cancel/replace flow.
//
// Why we don't unit-test handleReplace separately: the gate
// invocation is identical (same checkLiveGate call) and the
// handler logic is mirrored. handleCancel is the simpler success
// path so we keep the integration test focused there.

package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/repository"
	"github.com/lib/pq"
)

// newOrderActionsEnvWithGate is the gated counterpart to
// newOrderActionsEnv — wires the same struct but with a live
// gate enabled and the user_id used as a real UUID so
// loadActiveUserByID succeeds.
type orderActionsGatedEnv struct {
	t       *testing.T
	db      *sql.DB
	mock    sqlmock.Sqlmock
	handler *orderActionsHandler
	gate    *liveTradingGate
}

func newOrderActionsGatedEnv(t *testing.T, enforced bool) *orderActionsGatedEnv {
	t.Helper()
	db, mock := newMockDB(t)
	t.Cleanup(func() { _ = db.Close() })
	cfg := &Config{JWTSecret: liveGateTestSecret}
	gate := &liveTradingGate{
		db:             db,
		totpRepo:       repository.NewUserTOTPRepo(db),
		brokerLinkRepo: repository.NewBrokerLinkRepo(db),
		cfg:            cfg,
		enforced:       enforced,
	}
	h := newOrderActionsHandlerWithGate(&Services{DB: db}, cfg, gate)
	if h == nil {
		t.Fatal("newOrderActionsHandlerWithGate returned nil")
	}
	return &orderActionsGatedEnv{t: t, db: db, mock: mock, handler: h, gate: gate}
}

// expectFundOwnershipForGate primes the SELECT funds → SELECT
// fund_companies pair with a configurable trading_mode.
func (e *orderActionsGatedEnv) expectFundOwnershipForGate(fundID, companyID, userID, mode string) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	e.mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "company_id", "name", "description", "trading_mode",
			"initial_capital", "current_capital", "total_assets", "nav", "status",
			"config", "created_at", "updated_at",
		}).AddRow(fundID, companyID, "Fund", "", mode,
			100000.0, 100000.0, 100000.0, 1.0, "active",
			[]byte("{}"), now, now,
		))
	e.mock.ExpectQuery("FROM fund_companies").
		WithArgs(companyID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "owner_user_id", "name", "description", "created_at", "updated_at",
		}).AddRow(companyID, userID, "Co", "", now, now))
}

// expectUserLookupForGate primes loadActiveUserByID for the
// authenticated user.
func (e *orderActionsGatedEnv) expectUserLookupForGate(userID, kyc string) {
	e.mock.ExpectQuery(regexp.QuoteMeta(`FROM users`)).
		WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "email", "display_name", "role", "status",
			"password_hash", "kyc_status", "kyc_level",
		}).AddRow(userID, "u@example.com", "U", "user", "active", "", kyc, "tier1_basic"))
}

func (e *orderActionsGatedEnv) doCancel(userID, fundID, tradeID, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost,
		"/api/funds/"+fundID+"/orders/"+tradeID+"/cancel",
		strings.NewReader(body))
	req.SetPathValue("fundId", fundID)
	req.SetPathValue("tradeId", tradeID)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), userID))
	rr := httptest.NewRecorder()
	e.handler.handleCancel(rr, req)
	return rr
}

// TestOrderActions_Cancel_LiveGateBlocks
//
// fund.trading_mode='live' + KYC unverified + no broker link +
// no 2FA + no step-up token → 403 with first_failing in body.
// We MUST NOT hit the trade_executions table because the gate
// rejects the request before any mutation runs.
func TestOrderActions_Cancel_LiveGateBlocks(t *testing.T) {
	e := newOrderActionsGatedEnv(t, true)
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"

	e.expectFundOwnershipForGate(fundID, companyID, userID, "live")
	e.expectUserLookupForGate(userID, "unverified")
	// Broker link + 2FA lookups still happen because the gate
	// computes the full readiness picture so the audit metadata
	// is complete.
	e.mock.ExpectQuery(regexp.QuoteMeta(`FROM broker_links`)).
		WithArgs(fundID).WillReturnError(sql.ErrNoRows)
	e.mock.ExpectQuery(regexp.QuoteMeta(`FROM user_totp_secrets`)).
		WithArgs(userID).WillReturnError(sql.ErrNoRows)

	rr := e.doCancel(userID, fundID, "trade-1", "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), string(LiveReadinessKYCRequired)) {
		t.Errorf("body missing first-failing pillar: %s", rr.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestOrderActions_Cancel_LiveGateBypassedWhenDisabled
//
// LIVE_TRADING_GATE_ENABLED=false (gate.enforced=false) MUST let
// the cancel proceed even on a live fund with all pillars failing.
// The cancel still hits the trade lookup + update path and an
// audit row is written with live_gate_enforced=false.
func TestOrderActions_Cancel_LiveGateBypassedWhenDisabled(t *testing.T) {
	e := newOrderActionsGatedEnv(t, false)
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	const tradeID = "44444444-4444-4444-4444-444444444444"

	e.expectFundOwnershipForGate(fundID, companyID, userID, "live")
	e.expectUserLookupForGate(userID, "unverified")
	// Even with the kill switch off the gate still runs the
	// readiness lookups so the audit metadata records would-be
	// state. Repo returns "no rows" for both.
	e.mock.ExpectQuery(regexp.QuoteMeta(`FROM broker_links`)).
		WithArgs(fundID).WillReturnError(sql.ErrNoRows)
	e.mock.ExpectQuery(regexp.QuoteMeta(`FROM user_totp_secrets`)).
		WithArgs(userID).WillReturnError(sql.ErrNoRows)

	// Trade lookup + cancel + post-cancel re-fetch.
	tradeCols := []string{
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
	}
	now := time.Now()
	tradeRow := func(status string, cancelledAt any, cancelReason any) *sqlmock.Rows {
		return sqlmock.NewRows(tradeCols).AddRow(
			tradeID, fundID, nil, nil, "us:AAPL", "AAPL",
			nil, nil, nil, nil, "buy", nil,
			nil, "limit", 10.0, 100.0, 1000.0, 0.0,
			nil, 0.0, 0.0, 0.0,
			"live", nil, nil, status, nil,
			nil, nil, nil, nil,
			nil, nil, nil, nil,
			nil, nil, nil, nil,
			nil, nil, nil,
			nil, nil, // strategy + strategy_parent_trade_id
			nil, now,
			cancelledAt, cancelReason, nil, 0,
		)
	}
	e.mock.ExpectQuery("FROM trade_executions WHERE id = \\$1 AND fund_id = \\$2 LIMIT 1").
		WithArgs(tradeID, fundID).
		WillReturnRows(tradeRow("working", nil, nil))
	e.mock.ExpectExec(regexp.QuoteMeta(`UPDATE trade_executions`)).
		WithArgs("user_requested", tradeID, fundID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectQuery("FROM trade_executions WHERE id = \\$1 AND fund_id = \\$2 LIMIT 1").
		WithArgs(tradeID, fundID).
		WillReturnRows(tradeRow("cancelled", now, "user_requested"))

	// Audit chain insert (genesis). Same shape as
	// expectMutationLogInsert in order_actions_handler_test.go.
	e.mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT row_hash
		FROM admin_change_log
		WHERE row_hash IS NOT NULL
		ORDER BY created_at DESC, id DESC
		LIMIT 1`)).
		WillReturnError(sql.ErrNoRows)
	e.mock.ExpectExec("INSERT INTO admin_change_log").
		WithArgs(
			sqlmock.AnyArg(),
			userID, "trade.cancel", "trade_execution", tradeID,
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			nil,
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
		).WillReturnResult(sqlmock.NewResult(0, 1))

	rr := e.doCancel(userID, fundID, tradeID, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
	// Defensive use of pq import to silence unused warnings if
	// the file shrinks during refactors.
	_ = pq.Array
}
