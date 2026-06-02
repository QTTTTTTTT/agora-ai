// Admin funding-request 4-eye approval tests (P1-2).

package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/audit"
)

type adminFundingEnv struct {
	t       *testing.T
	mock    sqlmock.Sqlmock
	handler *adminHandler
}

func newAdminFundingEnv(t *testing.T) *adminFundingEnv {
	t.Helper()
	db, mock := newMockDB(t)
	t.Cleanup(func() { _ = db.Close() })
	h := &adminHandler{
		db:          db,
		auditLogger: audit.NewDBLogger(db),
	}
	return &adminFundingEnv{t: t, mock: mock, handler: h}
}

// expectLookupForApproval primes the FOR UPDATE select on
// funding_requests.
func (e *adminFundingEnv) expectLookupForApproval(id, fundID, requesterID, direction, status string, amount float64) {
	now := time.Now().UTC()
	e.mock.ExpectQuery(regexp.QuoteMeta(`FROM funding_requests WHERE id = $1 FOR UPDATE`)).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows(fundingHandlerColumns).AddRow(
			id, fundID, direction, amount, "USD", "wire",
			nil, status, requesterID, nil,
			nil, nil, nil, nil,
			nil, nil, nil, []byte(`{}`),
			now, now,
		))
}

func (e *adminFundingEnv) expectGetByID(id, fundID, requesterID, direction, status string, amount float64) {
	now := time.Now().UTC()
	e.mock.ExpectQuery(regexp.QuoteMeta(`FROM funding_requests WHERE id = $1`)).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows(fundingHandlerColumns).AddRow(
			id, fundID, direction, amount, "USD", "wire",
			nil, status, requesterID, nil,
			nil, nil, nil, nil,
			nil, nil, nil, []byte(`{}`),
			now, now,
		))
}

// TestAdmin_ApproveFunding_FourEyeViolation
//
// Same user attempting to approve their own request must produce
// a 403 four_eye_violation. Critical because the table CHECK is
// the last line of defence; this validates the handler catches
// it cleanly with a useful error code.
func TestAdmin_ApproveFunding_FourEyeViolation(t *testing.T) {
	e := newAdminFundingEnv(t)
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const reqID = "33333333-3333-3333-3333-333333333333"

	e.mock.ExpectBegin()
	e.expectLookupForApproval(reqID, fundID, userID, "deposit", "pending", 1000)
	// Audit access row for the 4-eye block.
	e.mock.ExpectQuery(regexp.QuoteMeta("FROM data_access_log")).
		WillReturnRows(sqlmock.NewRows([]string{"row_hash"}))
	e.mock.ExpectExec(regexp.QuoteMeta("INSERT INTO data_access_log")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectRollback()

	req := httptest.NewRequest(http.MethodPost,
		"/api/admin/funding-requests/"+reqID+"/approve",
		bytes.NewReader([]byte(`{}`)))
	req.SetPathValue("id", reqID)
	req = withSuperAdmin(req, userID) // SAME user as requester
	rr := httptest.NewRecorder()
	e.handler.handleApproveFunding(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "four_eye_violation") {
		t.Errorf("body=%s", rr.Body.String())
	}
}

// TestAdmin_ApproveFunding_NotFound returns 404 when the
// funding_request id doesn't match.
func TestAdmin_ApproveFunding_NotFound(t *testing.T) {
	e := newAdminFundingEnv(t)
	const approverID = "22222222-2222-2222-2222-222222222222"
	const reqID = "missing"

	e.mock.ExpectBegin()
	e.mock.ExpectQuery(regexp.QuoteMeta(`FROM funding_requests WHERE id = $1 FOR UPDATE`)).
		WithArgs(reqID).
		WillReturnRows(sqlmock.NewRows(fundingHandlerColumns)) // empty
	e.mock.ExpectRollback()

	req := httptest.NewRequest(http.MethodPost,
		"/api/admin/funding-requests/"+reqID+"/approve",
		bytes.NewReader([]byte(`{}`)))
	req.SetPathValue("id", reqID)
	req = withSuperAdmin(req, approverID)
	rr := httptest.NewRecorder()
	e.handler.handleApproveFunding(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// TestAdmin_ApproveFunding_DepositHappyPath
//
// Different admin approves a deposit. Tx flow:
//   BEGIN → lookup pending → cash_ledger insert → funds UPDATE
//   → mark approved → COMMIT → audit log.
func TestAdmin_ApproveFunding_DepositHappyPath(t *testing.T) {
	e := newAdminFundingEnv(t)
	const requesterID = "11111111-1111-1111-1111-111111111111"
	const approverID = "22222222-2222-2222-2222-222222222222"
	const fundID = "33333333-3333-3333-3333-333333333333"
	const reqID = "44444444-4444-4444-4444-444444444444"

	e.mock.ExpectBegin()
	e.expectLookupForApproval(reqID, fundID, requesterID, "deposit", "pending", 1000)
	// cash_ledger insert returns its id; signed amount is +1000.
	e.mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO cash_ledger")).
		WithArgs(
			fundID,
			sqlmock.AnyArg(), // posted_at
			sqlmock.AnyArg(), // trading_date
			"funding_deposit",
			1000.0,
			"USD",
			"",                  // trade_id
			"",                  // plan_id
			"",                  // plan_action_id
			"",                  // corp_action_id
			"",                  // broker_link_id
			sqlmock.AnyArg(),    // description
			sqlmock.AnyArg(),    // metadata
			approverID,          // created_by
			"funding:"+reqID,    // idempotency_key
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ledger-1"))
	// funds.current_capital += 1000.
	e.mock.ExpectExec(regexp.QuoteMeta("UPDATE funds")).
		WithArgs(fundID, 1000.0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// flip request to approved.
	e.mock.ExpectExec(regexp.QuoteMeta(`SET status = 'approved'`)).
		WithArgs(reqID, approverID, "ledger-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectCommit()
	expectAuditMutationGenesisFunding(e.mock)

	req := httptest.NewRequest(http.MethodPost,
		"/api/admin/funding-requests/"+reqID+"/approve",
		bytes.NewReader([]byte(`{}`)))
	req.SetPathValue("id", reqID)
	req = withSuperAdmin(req, approverID)
	rr := httptest.NewRecorder()
	e.handler.handleApproveFunding(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "approved") {
		t.Errorf("body=%s", rr.Body.String())
	}
}

// TestAdmin_ApproveFunding_WithdrawalInsufficientCash
//
// Withdrawal amount > current_capital must reject with 409
// insufficient_cash and roll the tx back without touching cash_ledger.
func TestAdmin_ApproveFunding_WithdrawalInsufficientCash(t *testing.T) {
	e := newAdminFundingEnv(t)
	const requesterID = "11111111-1111-1111-1111-111111111111"
	const approverID = "22222222-2222-2222-2222-222222222222"
	const fundID = "33333333-3333-3333-3333-333333333333"
	const reqID = "44444444-4444-4444-4444-444444444444"

	e.mock.ExpectBegin()
	e.expectLookupForApproval(reqID, fundID, requesterID, "withdrawal", "pending", 5000)
	// fund balance 1000 < requested 5000.
	e.mock.ExpectQuery(regexp.QuoteMeta(`SELECT current_capital FROM funds WHERE id = $1 FOR UPDATE`)).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows([]string{"current_capital"}).AddRow(1000.0))
	e.mock.ExpectRollback()

	req := httptest.NewRequest(http.MethodPost,
		"/api/admin/funding-requests/"+reqID+"/approve",
		bytes.NewReader([]byte(`{}`)))
	req.SetPathValue("id", reqID)
	req = withSuperAdmin(req, approverID)
	rr := httptest.NewRecorder()
	e.handler.handleApproveFunding(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "insufficient_cash") {
		t.Errorf("body=%s", rr.Body.String())
	}
}

// TestAdmin_RejectFunding_RequiresReason
func TestAdmin_RejectFunding_RequiresReason(t *testing.T) {
	e := newAdminFundingEnv(t)
	const approverID = "22222222-2222-2222-2222-222222222222"
	req := httptest.NewRequest(http.MethodPost,
		"/api/admin/funding-requests/some-id/reject",
		bytes.NewReader([]byte(`{}`)))
	req.SetPathValue("id", "some-id")
	req = withSuperAdmin(req, approverID)
	rr := httptest.NewRecorder()
	e.handler.handleRejectFunding(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "reason_required") {
		t.Errorf("body=%s", rr.Body.String())
	}
}

// TestAdmin_RejectFunding_FourEye blocks self-rejection.
func TestAdmin_RejectFunding_FourEye(t *testing.T) {
	e := newAdminFundingEnv(t)
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const reqID = "33333333-3333-3333-3333-333333333333"

	e.expectGetByID(reqID, fundID, userID, "deposit", "pending", 1000)
	e.mock.ExpectQuery(regexp.QuoteMeta("FROM data_access_log")).
		WillReturnRows(sqlmock.NewRows([]string{"row_hash"}))
	e.mock.ExpectExec(regexp.QuoteMeta("INSERT INTO data_access_log")).
		WillReturnResult(sqlmock.NewResult(0, 1))

	req := httptest.NewRequest(http.MethodPost,
		"/api/admin/funding-requests/"+reqID+"/reject",
		bytes.NewReader([]byte(`{"reason":"duplicate"}`)))
	req.SetPathValue("id", reqID)
	req = withSuperAdmin(req, userID) // same user
	rr := httptest.NewRecorder()
	e.handler.handleRejectFunding(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "four_eye_violation") {
		t.Errorf("body=%s", rr.Body.String())
	}
}

// TestAdmin_RejectFunding_HappyPath
func TestAdmin_RejectFunding_HappyPath(t *testing.T) {
	e := newAdminFundingEnv(t)
	const requesterID = "11111111-1111-1111-1111-111111111111"
	const approverID = "22222222-2222-2222-2222-222222222222"
	const fundID = "33333333-3333-3333-3333-333333333333"
	const reqID = "44444444-4444-4444-4444-444444444444"

	e.expectGetByID(reqID, fundID, requesterID, "withdrawal", "pending", 500)
	e.mock.ExpectBegin()
	e.mock.ExpectExec(regexp.QuoteMeta(`SET status = 'rejected'`)).
		WithArgs(reqID, approverID, "duplicate ticket").
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectCommit()
	expectAuditMutationGenesisFunding(e.mock)

	req := httptest.NewRequest(http.MethodPost,
		"/api/admin/funding-requests/"+reqID+"/reject",
		bytes.NewReader([]byte(`{"reason":"duplicate ticket"}`)))
	req.SetPathValue("id", reqID)
	req = withSuperAdmin(req, approverID)
	rr := httptest.NewRecorder()
	e.handler.handleRejectFunding(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "rejected") {
		t.Errorf("body=%s", rr.Body.String())
	}
}

// TestAdmin_ListFundingAdmin_DefaultsToPending
func TestAdmin_ListFundingAdmin_DefaultsToPending(t *testing.T) {
	e := newAdminFundingEnv(t)
	const approverID = "22222222-2222-2222-2222-222222222222"
	now := time.Now().UTC()

	e.mock.ExpectQuery(regexp.QuoteMeta(`WHERE status = 'pending'`)).
		WithArgs(200).
		WillReturnRows(sqlmock.NewRows(fundingHandlerColumns).AddRow(
			"fr-1", "fund-1", "deposit", 1000.0, "USD", "wire",
			nil, "pending", "user-1", nil,
			nil, nil, nil, nil,
			nil, nil, "first", []byte(`{}`),
			now, now,
		))
	e.mock.ExpectQuery(regexp.QuoteMeta("FROM data_access_log")).
		WillReturnRows(sqlmock.NewRows([]string{"row_hash"}))
	e.mock.ExpectExec(regexp.QuoteMeta("INSERT INTO data_access_log")).
		WillReturnResult(sqlmock.NewResult(0, 1))

	req := httptest.NewRequest(http.MethodGet, "/api/admin/funding-requests", nil)
	req = withSuperAdmin(req, approverID)
	rr := httptest.NewRecorder()
	e.handler.handleListFundingAdmin(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"status":"pending"`) {
		t.Errorf("body=%s", rr.Body.String())
	}
}

func TestIsValidFundingStatus(t *testing.T) {
	cases := map[string]bool{
		"pending":   true,
		"approved":  true,
		"rejected":  true,
		"cancelled": true,
		"posted":    true,
		"":          false,
		"unknown":   false,
	}
	for in, want := range cases {
		if got := isValidFundingStatus(in); got != want {
			t.Errorf("isValidFundingStatus(%q)=%v, want %v", in, got, want)
		}
	}
}
