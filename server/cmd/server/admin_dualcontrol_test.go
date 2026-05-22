package main

import (
	"context"
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
	"github.com/fundai/server/internal/audit"
)

// withUserID overrides the authenticated user on a fixture request so
// tests can have alice submit + bob approve without rebuilding the
// whole request context.
func withUserID(r *http.Request, userID string) *http.Request {
	ctx := api.WithAuthenticatedUserID(r.Context(), userID)
	return r.WithContext(ctx)
}

// newDualControlAdminHandler builds an adminHandler wired with
// sqlmock-backed dual-control plumbing. The returned handler skips
// the SubscriptionService dependency on newAdminHandler so we can
// exercise the dual-control HTTP surface in isolation.
func newDualControlAdminHandler(t *testing.T) (*adminHandler, sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	frozen := time.Date(2026, time.May, 18, 12, 0, 0, 0, time.UTC)
	logger := audit.NopLogger{}
	dc := audit.NewDualControlService(db, logger, stubAdminChecker{"alice": true, "bob": true}, time.Hour)
	overrideNow(dc, frozen)
	h := &adminHandler{
		db:          db,
		auditLogger: logger,
		dualControl: dc,
	}
	dc.Register("noop", func(ctx context.Context, tx *sql.Tx, req audit.AdminRequest) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	return h, mock, db
}

// stubAdminChecker is a SuperAdminChecker that returns canned answers
// without touching the DB — keeps the HTTP tests fast and deterministic.
type stubAdminChecker map[string]bool

func (s stubAdminChecker) IsSuperAdmin(_ context.Context, userID string) (bool, error) {
	return s[userID], nil
}

// overrideNow swaps the clock on a DualControlService via reflection-free
// helper. The service's now field is unexported but the test package
// (same module) can access it through the indirection: we reproduce the
// frozen-time pattern by constructing the row's expires_at to match.
// In practice the dualcontrol_test.go in audit/ proves now-override is
// safe; here we only need the clock to be in the past relative to fixture
// expires_at, which we control on each fixture row.
func overrideNow(_ *audit.DualControlService, _ time.Time) {
	// No-op shim. Tests in this file craft fixtures with expires_at far
	// enough in the future that the real time.Now() comparison passes.
}

// TestHandleSubmitAdminRequestRejectsAnonymous proves the route is
// gated by requireSuperAdmin. Without the role middleware the endpoint
// would happily insert rows on behalf of anyone with a token.
func TestHandleSubmitAdminRequestRejectsAnonymous(t *testing.T) {
	h, _, db := newDualControlAdminHandler(t)
	defer db.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/requests", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	h.handleSubmitAdminRequest(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestHandleSubmitAdminRequestRejectsUnknownAction maps the audit
// sentinel onto HTTP 400. A pending row with no handler would just
// rot in the queue, so we reject at submit time.
func TestHandleSubmitAdminRequestRejectsUnknownAction(t *testing.T) {
	h, _, db := newDualControlAdminHandler(t)
	defer db.Close()
	body := `{"action":"unknown_action","target_type":"x","target_id":"y","payload":{}}`
	req := adminSuperRequest(http.MethodPost, "/api/admin/requests", body)
	rr := httptest.NewRecorder()
	h.handleSubmitAdminRequest(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestHandleSubmitAdminRequestHappyPath confirms the JSON shape and
// 201 response. Frontends rely on `id` + `expires_at` to render the
// pending-approvals widget, so missing fields would break the UX.
func TestHandleSubmitAdminRequestHappyPath(t *testing.T) {
	h, mock, db := newDualControlAdminHandler(t)
	defer db.Close()

	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO admin_requests`)).
		WithArgs("alice", "noop", "platform_settings", "_singleton_", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "updated_at"}).AddRow("req-1", now, now))

	body := `{"action":"noop","target_type":"platform_settings","target_id":"_singleton_","payload":{"k":"v"},"reason":"test"}`
	req := adminSuperRequest(http.MethodPost, "/api/admin/requests", body)
	req = withUserID(req, "alice")
	rr := httptest.NewRecorder()
	h.handleSubmitAdminRequest(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["id"] != "req-1" || resp["status"] != "pending" {
		t.Errorf("unexpected response: %+v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestHandleApproveAdminRequestSelfApprovalReturns403 verifies the
// app-layer self-approval guard surfaces as 403 (not 500). Self-approval
// is a privilege-escalation attack vector, so the error must be
// loud and distinct from generic auth failures.
func TestHandleApproveAdminRequestSelfApprovalReturns403(t *testing.T) {
	h, mock, db := newDualControlAdminHandler(t)
	defer db.Close()

	future := time.Now().Add(time.Hour)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, requester_user_id, action, target_type, target_id, payload, reason, status")).
		WithArgs("req-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "requester_user_id", "action", "target_type", "target_id", "payload", "reason", "status",
			"approver_user_id", "approved_at", "executed_at", "execution_error", "expires_at", "created_at", "updated_at",
		}).AddRow(
			"req-1", "alice", "noop", "platform_settings", "_singleton_", []byte(`{}`), sql.NullString{}, "pending",
			sql.NullString{}, sql.NullTime{}, sql.NullTime{}, sql.NullString{}, future, time.Now(), time.Now(),
		))
	mock.ExpectRollback()

	req := adminSuperRequest(http.MethodPost, "/api/admin/requests/req-1/approve", "")
	req = withUserID(req, "alice") // same user as requester
	req.SetPathValue("id", "req-1")
	rr := httptest.NewRecorder()
	h.handleApproveAdminRequest(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "requester") {
		t.Errorf("error body should hint at self-approval: %s", rr.Body.String())
	}
}

// TestHandleApproveAdminRequestExecutesHandler is the end-to-end happy
// path — a different super_admin approves, the registered handler runs
// inside the TX, and the result payload bubbles back to the caller.
func TestHandleApproveAdminRequestExecutesHandler(t *testing.T) {
	h, mock, db := newDualControlAdminHandler(t)
	defer db.Close()

	future := time.Now().Add(time.Hour)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, requester_user_id, action, target_type, target_id, payload, reason, status")).
		WithArgs("req-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "requester_user_id", "action", "target_type", "target_id", "payload", "reason", "status",
			"approver_user_id", "approved_at", "executed_at", "execution_error", "expires_at", "created_at", "updated_at",
		}).AddRow(
			"req-1", "alice", "noop", "platform_settings", "_singleton_", []byte(`{}`), sql.NullString{}, "pending",
			sql.NullString{}, sql.NullTime{}, sql.NullTime{}, sql.NullString{}, future, time.Now(), time.Now(),
		))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE admin_requests SET status='approved'`)).
		WithArgs("req-1", "bob").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE admin_requests SET status='executed'`)).
		WithArgs("req-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	req := adminSuperRequest(http.MethodPost, "/api/admin/requests/req-1/approve", "")
	req = withUserID(req, "bob") // different from requester alice
	req.SetPathValue("id", "req-1")
	rr := httptest.NewRecorder()
	h.handleApproveAdminRequest(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "executed" {
		t.Errorf("expected executed status, got %v", resp["status"])
	}
	if resp["result"] == nil {
		t.Errorf("expected handler result to be echoed back, got %+v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestStatusForDualControlErrorMapping locks in the HTTP error code
// translation so future sentinel additions force an explicit decision.
func TestStatusForDualControlErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{audit.ErrRequestNotFound, http.StatusNotFound},
		{audit.ErrRequestSelfApproval, http.StatusForbidden},
		{audit.ErrRequesterNotSuperAdmin, http.StatusForbidden},
		{audit.ErrApproverNotSuperAdmin, http.StatusForbidden},
		{audit.ErrRequestAlreadyFinal, http.StatusConflict},
		{audit.ErrRequestExpired, http.StatusConflict},
		{audit.ErrUnknownAdminAction, http.StatusBadRequest},
	}
	for _, c := range cases {
		got := statusForDualControlError(c.err)
		if got != c.want {
			t.Errorf("status for %v: want %d, got %d", c.err, c.want, got)
		}
	}
}
