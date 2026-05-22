package main

import (
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
	"github.com/fundai/server/internal/subscription"
)

func TestAdminOverviewRequiresSuperAdmin(t *testing.T) {
	handler := newAdminHandler(&Services{DB: &sql.DB{}, SubscriptionService: &subscription.SubscriptionService{}})
	if handler == nil {
		t.Fatal("expected admin handler")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/overview", nil)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), "user-1"))
	req = req.WithContext(api.WithAuthenticatedUserRole(req.Context(), userRoleUser))
	rr := httptest.NewRecorder()

	handler.handleOverview(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusForbidden, rr.Code, rr.Body.String())
	}
}

func TestRequireSuperAdminAllowsSuperAdmin(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/admin/platform-settings", nil)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), "user-1"))
	req = req.WithContext(api.WithAuthenticatedUserRole(req.Context(), userRoleSuperAdmin))
	rr := httptest.NewRecorder()

	if !requireSuperAdmin(rr, req) {
		t.Fatal("expected super admin to pass")
	}
}

func TestAuthMiddlewareInjectsUserRole(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	token, _, err := issueSessionToken("11111111-1111-4111-8111-111111111111", "secret", time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta(`
			SELECT id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status, COALESCE(password_hash, ''), COALESCE(kyc_status, 'unverified'), COALESCE(kyc_level, 'tier1_basic')
			FROM users
			WHERE id = $1
			LIMIT 1
		`)).
		WithArgs("11111111-1111-4111-8111-111111111111").
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "display_name", "role", "status", "password_hash", "kyc_status", "kyc_level"}).
			AddRow("11111111-1111-4111-8111-111111111111", "founder@example.com", "Founder", userRoleSuperAdmin, userStatusActive, "$2a$10$abcdefghijklmnopqrstuv", "verified", "tier3_enterprise"))

	var capturedRole string
	handler := authMiddleware(db, "secret")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedRole, _ = api.AuthenticatedUserRole(r)
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/companies", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusNoContent, rr.Code, rr.Body.String())
	}
	if capturedRole != userRoleSuperAdmin {
		t.Fatalf("expected captured role %q, got %q", userRoleSuperAdmin, capturedRole)
	}
	assertMockExpectations(t, mock)
}

func TestAdminRechargeWalletRequiresSuperAdmin(t *testing.T) {
	handler := newAdminHandler(&Services{DB: &sql.DB{}, SubscriptionService: &subscription.SubscriptionService{}})
	if handler == nil {
		t.Fatal("expected admin handler")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/admin/wallets/recharge", strings.NewReader(`{"user_id":"user-1","amount_minor":100}`))
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), "user-1"))
	req = req.WithContext(api.WithAuthenticatedUserRole(req.Context(), userRoleUser))
	rr := httptest.NewRecorder()

	handler.handleRechargeWallet(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusForbidden, rr.Code, rr.Body.String())
	}
}

func TestAdminRechargeWalletRejectsNonPositiveAmount(t *testing.T) {
	handler := newAdminHandler(&Services{DB: &sql.DB{}, SubscriptionService: &subscription.SubscriptionService{}})
	if handler == nil {
		t.Fatal("expected admin handler")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/admin/wallets/recharge", strings.NewReader(`{"user_id":"user-1","amount_minor":0}`))
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), "admin-1"))
	req = req.WithContext(api.WithAuthenticatedUserRole(req.Context(), userRoleSuperAdmin))
	rr := httptest.NewRecorder()

	handler.handleRechargeWallet(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusBadRequest, rr.Code, rr.Body.String())
	}
}

func TestAdminListKYCApplicationsIncludesUserAndDocuments(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	handler := newAdminHandler(&Services{DB: db, SubscriptionService: &subscription.SubscriptionService{}})
	if handler == nil {
		t.Fatal("expected admin handler")
	}
	appID := "33333333-3333-4333-8333-333333333333"
	userID := "22222222-2222-4222-8222-222222222222"
	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT r.id, r.user_id, COALESCE(u.email, ''), COALESCE(u.display_name, ''), r.kyc_level, r.status,
		       r.full_name, r.id_document_type, r.id_document_number, COALESCE(r.document_image_urls, '[]'::jsonb),
		       COALESCE(r.rejection_reason, ''), r.created_at, r.updated_at
		FROM user_kyc_records r
		LEFT JOIN users u ON u.id = r.user_id
		WHERE r.status = $1
		ORDER BY r.created_at ASC
		LIMIT $2 OFFSET $3
	`)).
		WithArgs("pending", 100, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "email", "display_name", "kyc_level", "status", "full_name", "id_document_type", "id_document_number", "document_image_urls", "rejection_reason", "created_at", "updated_at"}).
			AddRow(appID, userID, "user@example.com", "Alice", "tier2_advanced", "pending", "Alice Doe", "passport", "P123456", []byte(`["https://example.test/passport.png"]`), "", now, now))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO data_access_log (actor_user_id, action, resource_type, resource_id, details)
			 VALUES ($1, $2, $3, $4, $5)`)).
		WithArgs("admin-1", "read", "kyc_applications", "pending", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	req := httptest.NewRequest(http.MethodGet, "/api/admin/kyc-applications", nil)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), "admin-1"))
	req = req.WithContext(api.WithAuthenticatedUserRole(req.Context(), userRoleSuperAdmin))
	rr := httptest.NewRecorder()

	handler.handleListKYCApplications(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	var apps []kycApplicationResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &apps); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(apps) != 1 || apps[0].UserEmail != "user@example.com" || apps[0].UserDisplayName != "Alice" || len(apps[0].DocumentImageURLs) != 1 {
		t.Fatalf("unexpected applications: %#v", apps)
	}
	assertMockExpectations(t, mock)
}

func TestAdminApproveKYCApplicationRecordsAudit(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	handler := newAdminHandler(&Services{DB: db, SubscriptionService: &subscription.SubscriptionService{}})
	if handler == nil {
		t.Fatal("expected admin handler")
	}
	adminID := "11111111-1111-4111-8111-111111111111"
	userID := "22222222-2222-4222-8222-222222222222"
	appID := "33333333-3333-4333-8333-333333333333"

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT user_id, kyc_level, status FROM user_kyc_records WHERE id = $1 FOR UPDATE`)).
		WithArgs(appID).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "kyc_level", "status"}).AddRow(userID, "tier2_advanced", "pending"))
	mock.ExpectExec(regexp.QuoteMeta(`
		UPDATE user_kyc_records 
		SET status = $1, rejection_reason = $2, reviewed_by = $3, reviewed_at = NOW(), updated_at = NOW() 
		WHERE id = $4`)).
		WithArgs("approved", "", adminID, appID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE users SET kyc_status = $1, kyc_level = $2, updated_at = NOW() WHERE id = $3`)).
		WithArgs("verified", "tier2_advanced", userID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO data_access_log (actor_user_id, action, resource_type, resource_id, details)
			 VALUES ($1, $2, $3, $4, $5)`)).
		WithArgs(adminID, "approve", "kyc_application", appID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	req := httptest.NewRequest(http.MethodPost, "/api/admin/kyc-applications/"+appID+"/approve", strings.NewReader(`{"action":"approve"}`))
	req.SetPathValue("id", appID)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), adminID))
	req = req.WithContext(api.WithAuthenticatedUserRole(req.Context(), userRoleSuperAdmin))
	rr := httptest.NewRecorder()

	handler.handleApproveKYCApplication(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	assertMockExpectations(t, mock)
}
