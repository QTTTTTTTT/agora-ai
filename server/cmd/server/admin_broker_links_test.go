// Admin broker_link 4-eye approval tests (P1-6).
//
// Three scenarios are critical:
//   1. happy path — different super_admin approves a pending row → active;
//   2. 4-eye violation — same user can't approve their own request;
//   3. reject path requires reason and emits the right audit shape.
//
// We exercise the handlers directly (not via mux) so each test
// can drive its own sqlmock script without ServeMux overhead.

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
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/repository"
)

// adminBrokerLinkEnv bundles a wired adminHandler + sqlmock so
// each test stays compact.
type adminBrokerLinkEnv struct {
	t       *testing.T
	mock    sqlmock.Sqlmock
	handler *adminHandler
}

func newAdminBrokerLinkEnv(t *testing.T) *adminBrokerLinkEnv {
	t.Helper()
	db, mock := newMockDB(t)
	t.Cleanup(func() { _ = db.Close() })
	h := &adminHandler{
		db:          db,
		auditLogger: audit.NewDBLogger(db),
	}
	return &adminBrokerLinkEnv{t: t, mock: mock, handler: h}
}

// withSuperAdmin attaches the auth context the requireSuperAdmin
// middleware checks.
func withSuperAdmin(req *http.Request, userID string) *http.Request {
	ctx := req.Context()
	ctx = api.WithAuthenticatedUserID(ctx, userID)
	ctx = api.WithAuthenticatedUserRole(ctx, userRoleSuperAdmin)
	return req.WithContext(ctx)
}

// expectLookupBrokerLink primes the SELECT FROM broker_links
// WHERE id = $1 LIMIT 1 query that h.lookupBrokerLink fires.
func (e *adminBrokerLinkEnv) expectLookupBrokerLink(linkID, requesterID, status string) {
	now := time.Now()
	e.mock.ExpectQuery(regexp.QuoteMeta(`FROM broker_links`)).
		WithArgs(linkID).
		WillReturnRows(sqlmock.NewRows(brokerLinkColumns).AddRow(
			linkID, "fund-1", requesterID, "ibkr", "U1234567", status,
			nil, nil, nil, []byte(`{}`),
			now, now,
		))
}

// expectLookupBrokerLink_NotFound primes a sql.ErrNoRows so the
// handler returns 404.
func (e *adminBrokerLinkEnv) expectLookupBrokerLinkNotFound(linkID string) {
	e.mock.ExpectQuery(regexp.QuoteMeta(`FROM broker_links`)).
		WithArgs(linkID).
		WillReturnRows(sqlmock.NewRows(brokerLinkColumns)) // empty
}

// expectAuditMutationGenesis primes the audit hash-chain INSERT.
func (e *adminBrokerLinkEnv) expectAuditMutationGenesis(actor, action, targetType, targetID string) {
	expectAuditMutationGenesisBL(e.mock, actor, action, targetType, targetID)
}

// expectAuditAccessLog primes a read-style audit row insert.
// Mirrors the data_access_log shape the audit package uses
// (genesis row + 10-column INSERT).
func (e *adminBrokerLinkEnv) expectAuditAccessLog(actor, action, resourceType, resourceID string) {
	e.mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT row_hash
		FROM data_access_log
		WHERE row_hash IS NOT NULL
		ORDER BY created_at DESC, id DESC
		LIMIT 1`)).
		WillReturnError(sql.ErrNoRows)
	e.mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO data_access_log`)).
		WithArgs(
			sqlmock.AnyArg(), // id
			actor, action, resourceType, resourceID,
			sqlmock.AnyArg(), // details json
			sqlmock.AnyArg(), // created_at
			nil,              // prev_hash
			sqlmock.AnyArg(), // row_hash
			sqlmock.AnyArg(), // details_hash
		).WillReturnResult(sqlmock.NewResult(0, 1))
}

// TestAdmin_ApproveBrokerLink_HappyPath
//
// Different super_admin approves a pending row → DB UPDATE
// fires, audit row written, response 200 + status="active".
func TestAdmin_ApproveBrokerLink_HappyPath(t *testing.T) {
	e := newAdminBrokerLinkEnv(t)
	const requesterID = "11111111-1111-1111-1111-111111111111"
	const approverID = "22222222-2222-2222-2222-222222222222"
	const linkID = "33333333-3333-3333-3333-333333333333"

	e.expectLookupBrokerLink(linkID, requesterID, "pending")
	e.mock.ExpectExec(regexp.QuoteMeta(`UPDATE broker_links`)).
		WithArgs(linkID, approverID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.expectAuditMutationGenesis(approverID, "broker_link.approve", "broker_link", linkID)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/broker-links/"+linkID+"/approve",
		strings.NewReader(`{"note":"Verified ticket #4221"}`))
	req.ContentLength = int64(len(`{"note":"Verified ticket #4221"}`))
	req.SetPathValue("id", linkID)
	req = withSuperAdmin(req, approverID)
	rr := httptest.NewRecorder()
	e.handler.handleApproveBrokerLink(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"active"`) {
		t.Errorf("body missing status=active: %s", rr.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestAdmin_ApproveBrokerLink_FourEyeViolation
//
// Same user (super_admin who also created the request) tries to
// approve → 403 with error="four_eye_violation". An access-log
// row records the attempt for the audit chain.
func TestAdmin_ApproveBrokerLink_FourEyeViolation(t *testing.T) {
	e := newAdminBrokerLinkEnv(t)
	const userID = "11111111-1111-1111-1111-111111111111"
	const linkID = "33333333-3333-3333-3333-333333333333"

	e.expectLookupBrokerLink(linkID, userID, "pending")
	e.expectAuditAccessLog(userID, "broker_link.approve_blocked_4eye", "broker_link", linkID)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/broker-links/"+linkID+"/approve", strings.NewReader(`{}`))
	req.ContentLength = 2
	req.SetPathValue("id", linkID)
	req = withSuperAdmin(req, userID)
	rr := httptest.NewRecorder()
	e.handler.handleApproveBrokerLink(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "four_eye_violation") {
		t.Errorf("body missing four_eye_violation: %s", rr.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestAdmin_ApproveBrokerLink_NotFound returns 404 cleanly.
func TestAdmin_ApproveBrokerLink_NotFound(t *testing.T) {
	e := newAdminBrokerLinkEnv(t)
	const approverID = "22222222-2222-2222-2222-222222222222"
	const linkID = "33333333-3333-3333-3333-333333333333"
	e.expectLookupBrokerLinkNotFound(linkID)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/broker-links/"+linkID+"/approve", strings.NewReader(`{}`))
	req.ContentLength = 2
	req.SetPathValue("id", linkID)
	req = withSuperAdmin(req, approverID)
	rr := httptest.NewRecorder()
	e.handler.handleApproveBrokerLink(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// TestAdmin_RejectBrokerLink_RequiresReason
//
// Reject without a `reason` body field → 400.
func TestAdmin_RejectBrokerLink_RequiresReason(t *testing.T) {
	e := newAdminBrokerLinkEnv(t)
	const approverID = "22222222-2222-2222-2222-222222222222"
	const linkID = "33333333-3333-3333-3333-333333333333"
	req := httptest.NewRequest(http.MethodPost, "/api/admin/broker-links/"+linkID+"/reject",
		strings.NewReader(`{"reason":""}`))
	req.ContentLength = int64(len(`{"reason":""}`))
	req.SetPathValue("id", linkID)
	req = withSuperAdmin(req, approverID)
	rr := httptest.NewRecorder()
	e.handler.handleRejectBrokerLink(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// TestAdmin_RejectBrokerLink_HappyPath
//
// Reject with a reason → row moves to revoked, audit row written.
func TestAdmin_RejectBrokerLink_HappyPath(t *testing.T) {
	e := newAdminBrokerLinkEnv(t)
	const requesterID = "11111111-1111-1111-1111-111111111111"
	const approverID = "22222222-2222-2222-2222-222222222222"
	const linkID = "33333333-3333-3333-3333-333333333333"

	e.expectLookupBrokerLink(linkID, requesterID, "pending")
	e.mock.ExpectExec(regexp.QuoteMeta(`UPDATE broker_links`)).
		WithArgs(linkID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.expectAuditMutationGenesis(approverID, "broker_link.reject", "broker_link", linkID)

	body := `{"reason":"missing recent statement"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/broker-links/"+linkID+"/reject", strings.NewReader(body))
	req.ContentLength = int64(len(body))
	req.SetPathValue("id", linkID)
	req = withSuperAdmin(req, approverID)
	rr := httptest.NewRecorder()
	e.handler.handleRejectBrokerLink(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), repository.BrokerLinkStatusRevoked) {
		t.Errorf("body missing status=revoked: %s", rr.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestAdmin_ListBrokerLinks_DefaultsToPending
//
// No status query param → handler defaults to status='pending'
// and SELECTs only those rows.
func TestAdmin_ListBrokerLinks_DefaultsToPending(t *testing.T) {
	e := newAdminBrokerLinkEnv(t)
	const adminID = "22222222-2222-2222-2222-222222222222"

	now := time.Now()
	e.mock.ExpectQuery(regexp.QuoteMeta(`FROM broker_links`)).
		WithArgs("pending").
		WillReturnRows(sqlmock.NewRows(brokerLinkColumns).AddRow(
			"link-1", "fund-1", "user-1", "ibkr", "U1234567", "pending",
			nil, nil, nil, []byte(`{}`),
			now, now,
		))
	e.expectAuditAccessLog(adminID, "read", "broker_link", "pending")

	req := httptest.NewRequest(http.MethodGet, "/api/admin/broker-links", nil)
	req = withSuperAdmin(req, adminID)
	rr := httptest.NewRecorder()
	e.handler.handleListBrokerLinksAdmin(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"pending"`) {
		t.Errorf("body missing status=pending: %s", rr.Body.String())
	}
	// Account id MUST be redacted in the response.
	if strings.Contains(rr.Body.String(), "U1234567") {
		t.Errorf("body leaked full account id: %s", rr.Body.String())
	}
}

// TestIsValidBrokerLinkStatus locks the closed vocabulary in
// place — adding a new status MUST also update this helper.
func TestIsValidBrokerLinkStatus(t *testing.T) {
	for _, s := range []string{"pending", "active", "suspended", "revoked"} {
		if !isValidBrokerLinkStatus(s) {
			t.Errorf("isValidBrokerLinkStatus(%q) = false", s)
		}
	}
	for _, s := range []string{"", "deleted", "unknown", "PENDING"} {
		if isValidBrokerLinkStatus(s) {
			t.Errorf("isValidBrokerLinkStatus(%q) = true (should reject)", s)
		}
	}
}
