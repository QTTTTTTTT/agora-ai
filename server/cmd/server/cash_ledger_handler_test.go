// Cash ledger HTTP handler tests (P1-1).
//
// We exercise:
//   - the auth + path guards (unauthenticated, missing fundId)
//   - the happy-path list call with default params
//   - cursor + summary + balance flag combinations
//   - 400 paths for invalid type, invalid cursor

package main

import (
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

func newCashLedgerHandlerEnv(t *testing.T) (*cashLedgerHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock := newMockDB(t)
	h := newCashLedgerHandler(&Services{DB: db})
	if h == nil {
		t.Fatal("newCashLedgerHandler returned nil")
	}
	return h, mock, func() { _ = db.Close() }
}

// expectFundOwnershipCL primes the fund + company SELECTs that
// authorizeFundAccess fires. Mirrors the helper used by the
// broker-link tests but kept local so the two modules stay
// independent.
func expectFundOwnershipCL(mock sqlmock.Sqlmock, fundID, companyID, userID string) {
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "company_id", "name", "description", "trading_mode",
			"initial_capital", "current_capital", "total_assets", "nav", "status",
			"config", "created_at", "updated_at",
		}).AddRow(fundID, companyID, "Fund", "", "simulation",
			100000.0, 100000.0, 100000.0, 1.0, "active",
			[]byte("{}"), now, now,
		))
	mock.ExpectQuery("FROM fund_companies").
		WithArgs(companyID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "owner_user_id", "name", "description", "created_at", "updated_at",
		}).AddRow(companyID, userID, "Co", "", now, now))
}

func TestCashLedger_List_Unauthenticated(t *testing.T) {
	h, _, cleanup := newCashLedgerHandlerEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/funds/f1/cash-ledger", nil)
	req.SetPathValue("fundId", "f1")
	rr := httptest.NewRecorder()
	h.handleList(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestCashLedger_List_HappyPath(t *testing.T) {
	h, mock, cleanup := newCashLedgerHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	now := time.Now().UTC()

	expectFundOwnershipCL(mock, fundID, companyID, userID)
	// Default limit: 100. Two rows returned.
	mock.ExpectQuery(regexp.QuoteMeta(`FROM cash_ledger WHERE fund_id = $1`)).
		WithArgs(fundID, 100).
		WillReturnRows(sqlmock.NewRows(cashLedgerHandlerColumns).
			AddRow(
				"l-1", fundID, now, nil, "trade_buy_notional",
				-1000.0, "USD", "t-1", nil, nil,
				nil, nil, "buy AAPL", []byte(`{"symbol":"AAPL"}`),
				nil, "trade:t-1:notional", now, now,
			).
			AddRow(
				"l-2", fundID, now.Add(-time.Hour), nil, "trade_buy_commission",
				-1.0, "USD", "t-1", nil, nil,
				nil, nil, "buy AAPL", []byte(`{}`),
				nil, "trade:t-1:commission", now, now,
			),
		)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/"+fundID+"/cash-ledger", nil)
	req.SetPathValue("fundId", fundID)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), userID))
	rr := httptest.NewRecorder()
	h.handleList(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	var resp cashLedgerListResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(resp.Entries))
	}
	if resp.Entries[0].EntryType != "trade_buy_notional" {
		t.Errorf("first type = %q", resp.Entries[0].EntryType)
	}
	if resp.NextCursor != "" {
		t.Errorf("next_cursor = %q, want empty (page not full)", resp.NextCursor)
	}
}

func TestCashLedger_List_RejectsBadType(t *testing.T) {
	h, mock, cleanup := newCashLedgerHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipCL(mock, fundID, companyID, userID)

	req := httptest.NewRequest(http.MethodGet,
		"/api/funds/"+fundID+"/cash-ledger?type=garbage", nil)
	req.SetPathValue("fundId", fundID)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), userID))
	rr := httptest.NewRecorder()
	h.handleList(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "invalid_type") {
		t.Errorf("body = %s", rr.Body.String())
	}
}

func TestCashLedger_List_BadCursor(t *testing.T) {
	h, mock, cleanup := newCashLedgerHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipCL(mock, fundID, companyID, userID)

	req := httptest.NewRequest(http.MethodGet,
		"/api/funds/"+fundID+"/cash-ledger?cursor=not-a-cursor", nil)
	req.SetPathValue("fundId", fundID)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), userID))
	rr := httptest.NewRecorder()
	h.handleList(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "invalid_cursor") {
		t.Errorf("body = %s", rr.Body.String())
	}
}

func TestCashLedger_List_WithSummaryAndBalance(t *testing.T) {
	h, mock, cleanup := newCashLedgerHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	now := time.Now().UTC()

	expectFundOwnershipCL(mock, fundID, companyID, userID)
	mock.ExpectQuery(regexp.QuoteMeta(`FROM cash_ledger WHERE fund_id = $1`)).
		WithArgs(fundID, 100).
		WillReturnRows(sqlmock.NewRows(cashLedgerHandlerColumns).AddRow(
			"l-1", fundID, now, nil, "dividend_cash",
			50.0, "USD", nil, nil, nil,
			"corp-1", nil, "dividend", []byte(`{}`),
			nil, "corp:corp-1:fund-x", now, now,
		))
	// FX-aware base currency lookup (P1-4).
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT base_currency FROM funds WHERE id = $1`)).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows([]string{"base_currency"}).AddRow("USD"))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT entry_type, SUM(amount) FROM cash_ledger WHERE fund_id = $1`)).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows([]string{"entry_type", "sum"}).
			AddRow("dividend_cash", 50.0).
			AddRow("trade_buy_notional", -1000.0))
	// FX-aware balance: per-currency SUM, then convert.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT currency, SUM(amount) FROM cash_ledger WHERE fund_id = $1`)).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows([]string{"currency", "sum"}).
			AddRow("USD", -950.0))

	req := httptest.NewRequest(http.MethodGet,
		"/api/funds/"+fundID+"/cash-ledger?summary=1&balance=1", nil)
	req.SetPathValue("fundId", fundID)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), userID))
	rr := httptest.NewRecorder()
	h.handleList(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp cashLedgerListResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Subtotals["dividend_cash"] != 50.0 {
		t.Errorf("subtotal dividend = %v", resp.Subtotals["dividend_cash"])
	}
	if resp.Balance == nil || *resp.Balance != -950.0 {
		t.Errorf("balance = %v", resp.Balance)
	}
}

func TestCashLedger_CursorRoundtrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	id := "12345678-1234-1234-1234-123456789abc"
	encoded := encodeCashLedgerCursor(now, id)
	gotTs, gotID, err := decodeCashLedgerCursor(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !gotTs.Equal(now) {
		t.Errorf("ts mismatch: got %v want %v", gotTs, now)
	}
	if gotID != id {
		t.Errorf("id = %q, want %q", gotID, id)
	}
}

// cashLedgerHandlerColumns is a copy of repository.cashLedgerColumns
// so the handler tests stay self-contained (we can't import the
// internal/repository test variable).
var cashLedgerHandlerColumns = []string{
	"id", "fund_id", "posted_at", "trading_date", "entry_type",
	"amount", "currency", "trade_id", "plan_id", "plan_action_id",
	"corp_action_id", "broker_link_id", "description", "metadata",
	"created_by", "idempotency_key", "created_at", "updated_at",
}
