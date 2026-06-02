// Funding handler tests (P1-2 user side).

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
)

func newFundingHandlerEnv(t *testing.T) (*fundingHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock := newMockDB(t)
	h := newFundingHandler(&Services{DB: db})
	if h == nil {
		t.Fatal("newFundingHandler returned nil")
	}
	return h, mock, func() { _ = db.Close() }
}

// expectFundOwnershipFunding is a local copy of the helper used
// by other handlers — keeps the funding tests self-contained.
func expectFundOwnershipFunding(mock sqlmock.Sqlmock, fundID, companyID, userID string) {
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

// expectAuditMutationGenesisFunding primes the audit hash-chain
// genesis insert. Lifted from the broker-link handler tests.
func expectAuditMutationGenesisFunding(mock sqlmock.Sqlmock) {
	mock.ExpectQuery("FROM admin_change_log").
		WillReturnRows(sqlmock.NewRows([]string{"row_hash"}))
	mock.ExpectExec("INSERT INTO admin_change_log").
		WillReturnResult(sqlmock.NewResult(1, 1))
}

func TestFunding_Create_Unauthenticated(t *testing.T) {
	h, _, cleanup := newFundingHandlerEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodPost, "/api/funds/f1/funding-requests",
		bytes.NewReader([]byte(`{"direction":"deposit","amount":1000,"method":"wire"}`)))
	req.SetPathValue("fundId", "f1")
	rr := httptest.NewRecorder()
	h.handleCreate(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestFunding_Create_RejectsBadDirection(t *testing.T) {
	h, mock, cleanup := newFundingHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipFunding(mock, fundID, companyID, userID)
	req := httptest.NewRequest(http.MethodPost,
		"/api/funds/"+fundID+"/funding-requests",
		bytes.NewReader([]byte(`{"direction":"send","amount":100,"method":"wire"}`)))
	req.SetPathValue("fundId", fundID)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), userID))
	rr := httptest.NewRecorder()
	h.handleCreate(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestFunding_Create_HappyPath(t *testing.T) {
	h, mock, cleanup := newFundingHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"

	expectFundOwnershipFunding(mock, fundID, companyID, userID)
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO funding_requests")).
		WithArgs(fundID, "deposit", 1000.0, "USD", "wire",
			"", userID, "test", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("fr-1"))
	expectAuditMutationGenesisFunding(mock)

	req := httptest.NewRequest(http.MethodPost,
		"/api/funds/"+fundID+"/funding-requests",
		bytes.NewReader([]byte(`{"direction":"deposit","amount":1000,"method":"wire","notes":"test"}`)))
	req.SetPathValue("fundId", fundID)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), userID))
	rr := httptest.NewRecorder()
	h.handleCreate(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["id"] != "fr-1" || resp["status"] != "pending" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestFunding_List_HappyPath(t *testing.T) {
	h, mock, cleanup := newFundingHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	now := time.Now().UTC()

	expectFundOwnershipFunding(mock, fundID, companyID, userID)
	mock.ExpectQuery(regexp.QuoteMeta("FROM funding_requests WHERE fund_id = $1")).
		WithArgs(fundID, 100).
		WillReturnRows(sqlmock.NewRows(fundingHandlerColumns).
			AddRow(
				"fr-1", fundID, "deposit", 1000.0, "USD", "wire",
				nil, "pending", userID, nil,
				nil, nil, nil, nil,
				nil, nil, "first", []byte(`{}`),
				now, now,
			))

	req := httptest.NewRequest(http.MethodGet,
		"/api/funds/"+fundID+"/funding-requests", nil)
	req.SetPathValue("fundId", fundID)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), userID))
	rr := httptest.NewRecorder()
	h.handleList(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestFunding_Cancel_NotFound(t *testing.T) {
	h, mock, cleanup := newFundingHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipFunding(mock, fundID, companyID, userID)
	mock.ExpectQuery(regexp.QuoteMeta("FROM funding_requests WHERE id = $1")).
		WithArgs("fr-missing").
		WillReturnRows(sqlmock.NewRows(fundingHandlerColumns)) // empty

	req := httptest.NewRequest(http.MethodPost,
		"/api/funds/"+fundID+"/funding-requests/fr-missing/cancel", nil)
	req.SetPathValue("fundId", fundID)
	req.SetPathValue("id", "fr-missing")
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), userID))
	rr := httptest.NewRecorder()
	h.handleCancel(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestFunding_Cancel_HappyPath(t *testing.T) {
	h, mock, cleanup := newFundingHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	now := time.Now().UTC()

	expectFundOwnershipFunding(mock, fundID, companyID, userID)
	mock.ExpectQuery(regexp.QuoteMeta("FROM funding_requests WHERE id = $1")).
		WithArgs("fr-1").
		WillReturnRows(sqlmock.NewRows(fundingHandlerColumns).AddRow(
			"fr-1", fundID, "deposit", 1000.0, "USD", "wire",
			nil, "pending", userID, nil,
			nil, nil, nil, nil,
			nil, nil, nil, []byte(`{}`),
			now, now,
		))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE funding_requests")).
		WithArgs("fr-1", userID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectAuditMutationGenesisFunding(mock)

	req := httptest.NewRequest(http.MethodPost,
		"/api/funds/"+fundID+"/funding-requests/fr-1/cancel", nil)
	req.SetPathValue("fundId", fundID)
	req.SetPathValue("id", "fr-1")
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), userID))
	rr := httptest.NewRecorder()
	h.handleCancel(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "cancelled") {
		t.Errorf("body=%s", rr.Body.String())
	}
}

// fundingHandlerColumns mirrors fundingSelectColumns from the
// repo. Local copy keeps the test independent.
var fundingHandlerColumns = []string{
	"id", "fund_id", "direction", "amount", "currency", "method",
	"external_reference", "status", "requested_by", "approved_by",
	"approved_at", "rejected_by", "rejected_at", "rejection_reason",
	"cancelled_at", "cash_ledger_entry_id", "notes", "metadata",
	"created_at", "updated_at",
}
