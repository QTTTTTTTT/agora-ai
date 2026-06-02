package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
)

// auth helpers ---------------------------------------------------------------

func withAdminCtx(req *http.Request) *http.Request {
	ctx := api.WithAuthenticatedUserID(req.Context(), "admin-1")
	ctx = api.WithAuthenticatedUserRole(ctx, userRoleSuperAdmin)
	return req.WithContext(ctx)
}

func withUserCtx(req *http.Request) *http.Request {
	ctx := api.WithAuthenticatedUserID(req.Context(), "user-1")
	ctx = api.WithAuthenticatedUserRole(ctx, userRoleUser)
	return req.WithContext(ctx)
}

// ---------------------------------------------------------------------------
// Auth gating
// ---------------------------------------------------------------------------

func TestAuditVerify_ForbidsNonSuperAdmin(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	h := newAuditVerifyHandler(&Services{DB: db})
	if h == nil {
		t.Fatal("expected handler")
	}

	cases := []struct {
		path    string
		handler http.HandlerFunc
	}{
		{"/api/admin/audit/chain/verify", h.handleVerifyAll},
		{"/api/admin/audit/chain/verify/access", h.handleVerifyAccess},
		{"/api/admin/audit/chain/verify/admin", h.handleVerifyMutation},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			req := withUserCtx(httptest.NewRequest(http.MethodGet, c.path, nil))
			rr := httptest.NewRecorder()
			c.handler(rr, req)
			if rr.Code != http.StatusForbidden {
				t.Errorf("expected 403, got %d body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestAuditVerify_NilHandler_NoOpRegister(t *testing.T) {
	var h *auditVerifyHandler
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
}

func TestNewAuditVerifyHandler_NilOnMissingDB(t *testing.T) {
	if newAuditVerifyHandler(nil) != nil {
		t.Errorf("expected nil handler when svc is nil")
	}
	if newAuditVerifyHandler(&Services{}) != nil {
		t.Errorf("expected nil handler when DB is nil")
	}
}

// ---------------------------------------------------------------------------
// Happy path: empty chain
// ---------------------------------------------------------------------------

func TestAuditVerifyAccess_EmptyChain(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, actor_user_id, action, resource_type`)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "actor_user_id", "action", "resource_type", "resource_id",
			"details", "created_at", "prev_hash", "row_hash", "details_hash",
		}))

	h := newAuditVerifyHandler(&Services{DB: db})
	req := withAdminCtx(httptest.NewRequest(http.MethodGet, "/api/admin/audit/chain/verify/access", nil))
	rr := httptest.NewRecorder()
	h.handleVerifyAccess(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["status"] != "empty" {
		t.Errorf("status = %v, want empty", got["status"])
	}
	if got["table"] != "data_access_log" {
		t.Errorf("table = %v, want data_access_log", got["table"])
	}
}

func TestAuditVerifyAll_BothEmpty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	emptyAccess := sqlmock.NewRows([]string{
		"id", "actor_user_id", "action", "resource_type", "resource_id",
		"details", "created_at", "prev_hash", "row_hash", "details_hash",
	})
	emptyMutation := sqlmock.NewRows([]string{
		"id", "actor_user_id", "action", "target_type", "target_id", "request_id",
		"before_snapshot", "after_snapshot", "metadata", "created_at",
		"prev_hash", "row_hash", "before_hash", "after_hash", "metadata_hash",
	})

	mock.ExpectQuery(regexp.QuoteMeta(`FROM data_access_log WHERE row_hash IS NULL`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, actor_user_id, action, resource_type`)).
		WillReturnRows(emptyAccess)
	mock.ExpectQuery(regexp.QuoteMeta(`FROM admin_change_log WHERE row_hash IS NULL`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM admin_change_log`)).
		WillReturnRows(emptyMutation)

	h := newAuditVerifyHandler(&Services{DB: db})
	req := withAdminCtx(httptest.NewRequest(http.MethodGet, "/api/admin/audit/chain/verify", nil))
	rr := httptest.NewRecorder()
	h.handleVerifyAll(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Overall  string         `json:"overall"`
		Access   map[string]any `json:"access"`
		Mutation map[string]any `json:"mutation"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Overall != "empty" {
		t.Errorf("overall = %s, want empty", got.Overall)
	}
	if got.Access["status"] != "empty" || got.Mutation["status"] != "empty" {
		t.Errorf("expected both per-chain statuses empty, got access=%v mutation=%v",
			got.Access["status"], got.Mutation["status"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// combinedStatus
// ---------------------------------------------------------------------------

func TestCombinedStatus(t *testing.T) {
	cases := []struct {
		a, b audit.VerificationStatus
		want audit.VerificationStatus
	}{
		{audit.VerificationOK, audit.VerificationOK, audit.VerificationOK},
		{audit.VerificationEmpty, audit.VerificationEmpty, audit.VerificationEmpty},
		{audit.VerificationEmpty, audit.VerificationOK, audit.VerificationOK},
		{audit.VerificationOK, audit.VerificationEmpty, audit.VerificationOK},
		{audit.VerificationFailed, audit.VerificationOK, audit.VerificationFailed},
		{audit.VerificationOK, audit.VerificationFailed, audit.VerificationFailed},
		{audit.VerificationFailed, audit.VerificationEmpty, audit.VerificationFailed},
		{audit.VerificationEmpty, audit.VerificationFailed, audit.VerificationFailed},
		{audit.VerificationFailed, audit.VerificationFailed, audit.VerificationFailed},
	}
	for _, c := range cases {
		got := combinedStatus(c.a, c.b)
		if got != c.want {
			t.Errorf("combinedStatus(%s, %s) = %s, want %s", c.a, c.b, got, c.want)
		}
	}
}

// keep the imports honest on builds where only some tests are running.
var _ = sql.ErrNoRows
