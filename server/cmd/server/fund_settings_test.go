package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/repository"
)

func newFundSettingsEnv(t *testing.T) (*fundSettingsHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	h := &fundSettingsHandler{
		fundRepo:    repository.NewFundRepo(db),
		companyRepo: repository.NewFundCompanyRepo(db),
		auditLogger: audit.NewDBLogger(db),
	}
	return h, mock, func() { _ = db.Close() }
}

// expectFundOwnershipFS primes the fund + company SELECTs the
// authorize gate fires. Lifted from cash_ledger_handler_test.go
// to avoid cross-file coupling.
func expectFundOwnershipFS(mock sqlmock.Sqlmock, fundID, companyID, userID string) {
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

func TestFundSettings_SetBaseCurrency_Unauthenticated(t *testing.T) {
	h, _, cleanup := newFundSettingsEnv(t)
	defer cleanup()
	body, _ := json.Marshal(map[string]any{"base_currency": "USD"})
	req := httptest.NewRequest(http.MethodPost, "/api/funds/x/settings/base-currency", bytes.NewReader(body))
	req.SetPathValue("fundId", "x")
	rr := httptest.NewRecorder()
	h.handleSetBaseCurrency(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestFundSettings_SetBaseCurrency_RejectsBadCurrency(t *testing.T) {
	h, mock, cleanup := newFundSettingsEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipFS(mock, fundID, companyID, userID)
	body, _ := json.Marshal(map[string]any{"base_currency": "BTC"})
	req := httptest.NewRequest(http.MethodPost, "/api/funds/"+fundID+"/settings/base-currency", bytes.NewReader(body)).
		WithContext(api.WithAuthenticatedUserID(httptest.NewRequest(http.MethodGet, "/", nil).Context(), userID))
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleSetBaseCurrency(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "invalid_currency") {
		t.Errorf("body = %s", rr.Body.String())
	}
}

func TestFundSettings_SetBaseCurrency_HappyPath(t *testing.T) {
	h, mock, cleanup := newFundSettingsEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"

	expectFundOwnershipFS(mock, fundID, companyID, userID)
	// Read previous base currency.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT base_currency FROM funds WHERE id = $1`)).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows([]string{"base_currency"}).AddRow("USD"))
	// Update.
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE funds`)).
		WithArgs(fundID, "CNY").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Audit chain — genesis lookup + insert.
	mock.ExpectQuery("FROM admin_change_log").
		WillReturnRows(sqlmock.NewRows([]string{"row_hash"}))
	mock.ExpectExec("INSERT INTO admin_change_log").
		WillReturnResult(sqlmock.NewResult(1, 1))

	body, _ := json.Marshal(map[string]any{"base_currency": "CNY"})
	req := httptest.NewRequest(http.MethodPost, "/api/funds/"+fundID+"/settings/base-currency", bytes.NewReader(body)).
		WithContext(api.WithAuthenticatedUserID(httptest.NewRequest(http.MethodGet, "/", nil).Context(), userID))
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleSetBaseCurrency(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"ok":true`) {
		t.Errorf("body = %s", rr.Body.String())
	}
}

func TestFundSettings_SetBaseCurrency_NoOpWhenSame(t *testing.T) {
	h, mock, cleanup := newFundSettingsEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipFS(mock, fundID, companyID, userID)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT base_currency FROM funds WHERE id = $1`)).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows([]string{"base_currency"}).AddRow("USD"))

	body, _ := json.Marshal(map[string]any{"base_currency": "USD"})
	req := httptest.NewRequest(http.MethodPost, "/api/funds/"+fundID+"/settings/base-currency", bytes.NewReader(body)).
		WithContext(api.WithAuthenticatedUserID(httptest.NewRequest(http.MethodGet, "/", nil).Context(), userID))
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleSetBaseCurrency(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"noop":true`) {
		t.Errorf("body = %s", rr.Body.String())
	}
}
