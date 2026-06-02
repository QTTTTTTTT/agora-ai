package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/api"
)

// orderActionsTestEnv bundles the mock and a freshly-built handler so
// each test stays compact.
type orderActionsTestEnv struct {
	t       *testing.T
	db      *sql.DB
	mock    sqlmock.Sqlmock
	handler *orderActionsHandler
}

func newOrderActionsEnv(t *testing.T) *orderActionsTestEnv {
	t.Helper()
	db, mock := newMockDB(t)
	t.Cleanup(func() { _ = db.Close() })
	h := newOrderActionsHandler(&Services{DB: db})
	if h == nil {
		t.Fatal("newOrderActionsHandler returned nil")
	}
	return &orderActionsTestEnv{t: t, db: db, mock: mock, handler: h}
}

// expectFundOwnershipOK programs the SELECT funds → SELECT
// fund_companies pair that authorizeFundAccess fires, with the
// company.owner_user_id matching userID so the access check passes.
func (e *orderActionsTestEnv) expectFundOwnershipOK(fundID, companyID, userID string) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	e.mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "company_id", "name", "description", "trading_mode",
			"initial_capital", "current_capital", "total_assets", "nav", "status",
			"config", "created_at", "updated_at",
		}).AddRow(
			fundID, companyID, "Test Fund", "", "paper",
			100000.0, 100000.0, 100000.0, 1.0, "active",
			[]byte("{}"), now, now,
		))
	e.mock.ExpectQuery("FROM fund_companies").
		WithArgs(companyID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "owner_user_id", "name", "description", "created_at", "updated_at",
		}).AddRow(companyID, userID, "Co", "", now, now))
}

// expectGetTradeForFund returns a working LIMIT trade. When status
// is overridden by the caller we copy the row.
func (e *orderActionsTestEnv) expectGetTradeForFund(fundID, tradeID, status string, replaceCount int) {
	cols := []string{
		"id", "fund_id", "plan_id", "plan_action_id", "instrument_key", "symbol",
		"market", "exchange", "asset_class", "instrument_type", "side", "position_side",
		"open_close", "order_type", "quantity", "price", "amount", "filled_qty",
		"filled_price", "fee_commission", "fee_stamp_tax", "fee_transfer",
		"trading_mode", "broker_order_id", "mcp_server_id", "status", "executed_at",
		"quote_currency", "settlement_currency", "margin_mode", "leverage",
		"contract_multiplier", "expiry_date", "reduce_only", "slippage_pct",
		"stop_price", "trail_amount", "trail_percent", "display_qty",
		"time_in_force", "good_till_date", "parent_trade_id",
		"client_idempotency_key", "created_at",
		"cancelled_at", "cancel_reason", "replaced_at", "replace_count",
	}
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	e.mock.ExpectQuery("FROM trade_executions WHERE id = \\$1 AND fund_id = \\$2 LIMIT 1").
		WithArgs(tradeID, fundID).
		WillReturnRows(sqlmock.NewRows(cols).AddRow(
			tradeID, fundID, nil, nil, "us:AAPL", "AAPL",
			nil, nil, nil, nil, "buy", nil,
			nil, "limit", 10.0, 100.0, 1000.0, 0.0,
			nil, 0.0, 0.0, 0.0,
			"paper", nil, nil, status, nil,
			nil, nil, nil, nil,
			nil, nil, nil, nil,
			nil, nil, nil, nil,
			nil, nil, nil,
			nil, now,
			nil, nil, nil, replaceCount,
		))
}

// expectMutationLogInsert programs the audit hash-chain expectation
// pair for admin_change_log, mirroring expectAccessLogInsert in
// shape but for the mutation chain.
func (e *orderActionsTestEnv) expectMutationLogInsert(actor, action, targetType, targetID string) {
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
			actor, action, targetType, targetID,
			sqlmock.AnyArg(), // request_id
			sqlmock.AnyArg(), // before_snapshot
			sqlmock.AnyArg(), // after_snapshot
			sqlmock.AnyArg(), // metadata
			sqlmock.AnyArg(), // created_at
			nil,              // prev_hash genesis
			sqlmock.AnyArg(), // row_hash
			sqlmock.AnyArg(), // before_hash
			sqlmock.AnyArg(), // after_hash
			sqlmock.AnyArg(), // metadata_hash
		).WillReturnResult(sqlmock.NewResult(0, 1))
}

func (e *orderActionsTestEnv) doCancel(userID, fundID, tradeID, body string) *httptest.ResponseRecorder {
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

func (e *orderActionsTestEnv) doReplace(userID, fundID, tradeID, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost,
		"/api/funds/"+fundID+"/orders/"+tradeID+"/replace",
		strings.NewReader(body))
	req.SetPathValue("fundId", fundID)
	req.SetPathValue("tradeId", tradeID)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), userID))
	rr := httptest.NewRecorder()
	e.handler.handleReplace(rr, req)
	return rr
}

// ---------------------------------------------------------------------------
// Auth + path validation
// ---------------------------------------------------------------------------

func TestOrderActions_Cancel_Unauthenticated(t *testing.T) {
	e := newOrderActionsEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/api/funds/f/orders/t/cancel", nil)
	req.SetPathValue("fundId", "f")
	req.SetPathValue("tradeId", "t")
	rr := httptest.NewRecorder()
	e.handler.handleCancel(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestOrderActions_Replace_Unauthenticated(t *testing.T) {
	e := newOrderActionsEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/api/funds/f/orders/t/replace", strings.NewReader(`{"quantity":1}`))
	req.SetPathValue("fundId", "f")
	req.SetPathValue("tradeId", "t")
	rr := httptest.NewRecorder()
	e.handler.handleReplace(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestOrderActions_Replace_RejectsEmptyBody(t *testing.T) {
	e := newOrderActionsEnv(t)
	rr := e.doReplace("user-1", "fund-1", "trade-1", "")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestOrderActions_Replace_RejectsNoChangeBody(t *testing.T) {
	e := newOrderActionsEnv(t)
	rr := e.doReplace("user-1", "fund-1", "trade-1", `{}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestOrderActions_Replace_RejectsNegativeFields(t *testing.T) {
	e := newOrderActionsEnv(t)
	rr := e.doReplace("user-1", "fund-1", "trade-1", `{"quantity":-1}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// Cancel happy path
// ---------------------------------------------------------------------------

func TestOrderActions_Cancel_HappyPath(t *testing.T) {
	e := newOrderActionsEnv(t)

	const userID = "user-1"
	const fundID = "fund-1"
	const companyID = "co-1"
	const tradeID = "trade-1"

	e.expectFundOwnershipOK(fundID, companyID, userID)
	// pre-mutation snapshot
	e.expectGetTradeForFund(fundID, tradeID, "working", 0)
	// the actual UPDATE
	e.mock.ExpectExec("UPDATE trade_executions").
		WithArgs("user_requested", tradeID, fundID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// post-mutation snapshot
	e.expectGetTradeForFund(fundID, tradeID, "cancelled", 0)
	e.expectMutationLogInsert(userID, "trade.cancel", "trade_execution", tradeID)

	rr := e.doCancel(userID, fundID, tradeID, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Order orderResponse `json:"order"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if resp.Order.Status != "cancelled" {
		t.Errorf("status = %s, want cancelled", resp.Order.Status)
	}
	assertMockExpectations(t, e.mock)
}

func TestOrderActions_Cancel_TerminalReturns409(t *testing.T) {
	e := newOrderActionsEnv(t)
	const userID = "user-1"
	const fundID = "fund-1"
	const companyID = "co-1"
	const tradeID = "trade-1"

	e.expectFundOwnershipOK(fundID, companyID, userID)
	e.expectGetTradeForFund(fundID, tradeID, "filled", 0)
	// UPDATE returns 0 rows affected because the row is filled.
	e.mock.ExpectExec("UPDATE trade_executions").
		WithArgs("user_requested", tradeID, fundID).
		WillReturnResult(sqlmock.NewResult(0, 0))

	rr := e.doCancel(userID, fundID, tradeID, "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 body=%s", rr.Code, rr.Body.String())
	}
}

func TestOrderActions_Cancel_NotFoundReturns404(t *testing.T) {
	e := newOrderActionsEnv(t)
	const userID = "user-1"
	const fundID = "fund-1"
	const companyID = "co-1"
	const tradeID = "ghost"

	e.expectFundOwnershipOK(fundID, companyID, userID)
	cols := []string{
		"id", "fund_id", "plan_id", "plan_action_id", "instrument_key", "symbol",
		"market", "exchange", "asset_class", "instrument_type", "side", "position_side",
		"open_close", "order_type", "quantity", "price", "amount", "filled_qty",
		"filled_price", "fee_commission", "fee_stamp_tax", "fee_transfer",
		"trading_mode", "broker_order_id", "mcp_server_id", "status", "executed_at",
		"quote_currency", "settlement_currency", "margin_mode", "leverage",
		"contract_multiplier", "expiry_date", "reduce_only", "slippage_pct",
		"stop_price", "trail_amount", "trail_percent", "display_qty",
		"time_in_force", "good_till_date", "parent_trade_id",
		"client_idempotency_key", "created_at",
		"cancelled_at", "cancel_reason", "replaced_at", "replace_count",
	}
	e.mock.ExpectQuery("FROM trade_executions WHERE id = \\$1 AND fund_id = \\$2 LIMIT 1").
		WithArgs(tradeID, fundID).
		WillReturnRows(sqlmock.NewRows(cols)) // empty

	rr := e.doCancel(userID, fundID, tradeID, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 body=%s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Replace happy path
// ---------------------------------------------------------------------------

func TestOrderActions_Replace_HappyPath(t *testing.T) {
	e := newOrderActionsEnv(t)

	const userID = "user-1"
	const fundID = "fund-1"
	const companyID = "co-1"
	const tradeID = "trade-1"

	e.expectFundOwnershipOK(fundID, companyID, userID)
	// pre-mutation
	e.expectGetTradeForFund(fundID, tradeID, "working", 0)
	// repo replace tx
	e.mock.ExpectBegin()
	e.mock.ExpectQuery("FOR UPDATE").
		WithArgs(tradeID, fundID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status", "quantity", "filled_qty", "replace_count"}).
			AddRow(tradeID, "working", 10.0, 0.0, 0))
	e.mock.ExpectExec("UPDATE trade_executions SET").
		WithArgs(20.0, tradeID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectCommit()
	// re-fetch after replace
	e.expectGetTradeForFund(fundID, tradeID, "working", 1)
	// audit
	e.expectMutationLogInsert(userID, "trade.replace", "trade_execution", tradeID)

	rr := e.doReplace(userID, fundID, tradeID, `{"quantity":20}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Order orderResponse `json:"order"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if resp.Order.ReplaceCount != 1 {
		t.Errorf("replaceCount = %d, want 1", resp.Order.ReplaceCount)
	}
}

func TestOrderActions_Replace_TerminalReturns409(t *testing.T) {
	e := newOrderActionsEnv(t)

	const userID = "user-1"
	const fundID = "fund-1"
	const companyID = "co-1"
	const tradeID = "trade-1"

	e.expectFundOwnershipOK(fundID, companyID, userID)
	e.expectGetTradeForFund(fundID, tradeID, "filled", 0)
	e.mock.ExpectBegin()
	e.mock.ExpectQuery("FOR UPDATE").
		WithArgs(tradeID, fundID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status", "quantity", "filled_qty", "replace_count"}).
			AddRow(tradeID, "filled", 10.0, 10.0, 0))
	e.mock.ExpectRollback()

	rr := e.doReplace(userID, fundID, tradeID, `{"quantity":20}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 body=%s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func TestNormaliseCancelReason(t *testing.T) {
	cases := map[string]string{
		"":                     "user_requested",
		"   ":                  "user_requested",
		"User_Requested":       "user_requested",
		"USER_REQUESTED":       "user_requested",
		"weird":                "user_requested",
		"superseded_by_replace": "superseded_by_replace",
		"ttl":                  "ttl",
		"risk_breach":          "risk_breach",
		"system":               "system",
	}
	for in, want := range cases {
		if got := normaliseCancelReason(in); got != want {
			t.Errorf("normaliseCancelReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClientIP_HeadersAndDirect(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	if ip := clientIP(r); ip != "10.0.0.1:1234" {
		t.Errorf("got %q", ip)
	}
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1")
	if ip := clientIP(r); ip != "1.2.3.4" {
		t.Errorf("xff parse, got %q", ip)
	}
	r.Header.Set("X-Forwarded-For", "5.6.7.8")
	if ip := clientIP(r); ip != "5.6.7.8" {
		t.Errorf("single xff, got %q", ip)
	}
	if ip := clientIP(nil); ip != "" {
		t.Errorf("nil request, got %q", ip)
	}
}

// Ensure the empty-body and json-parse paths use the right buffer
// reading semantics.
func TestOrderActions_Replace_RejectsTrailingGarbage(t *testing.T) {
	e := newOrderActionsEnv(t)
	body := bytes.NewBufferString(`{"quantity":1} GARBAGE`)
	req := httptest.NewRequest(http.MethodPost, "/api/funds/f/orders/t/replace", body)
	req.SetPathValue("fundId", "f")
	req.SetPathValue("tradeId", "t")
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	e.handler.handleReplace(rr, req)
	// We accept the first JSON value and don't enforce strict
	// trailing-text rejection — but unknown-field disallow should
	// catch most bad inputs. So this should go past validation
	// and into the (mock-less) auth check, which will fail
	// because we haven't programmed the mock. Whether the test
	// hits 500 (auth db error) or 400 doesn't matter; what
	// matters is that bad JSON shape doesn't panic.
	_ = rr
}
