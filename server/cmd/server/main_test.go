package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/llm"
	"github.com/fundai/server/internal/marketdata"
	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/workflow"
	"github.com/lib/pq"
)

func TestHandleHealthReportsDependencyState(t *testing.T) {
	handler := handleHealth(&Services{})
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status %d, got %d", http.StatusServiceUnavailable, rr.Code)
	}

	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal health response: %v", err)
	}
	if payload["status"] != "degraded" {
		t.Fatalf("expected degraded status, got %#v", payload["status"])
	}

	deps, ok := payload["dependencies"].(map[string]any)
	if !ok {
		t.Fatalf("expected dependencies map, got %#v", payload["dependencies"])
	}
	for _, key := range []string{"database", "llm_runtime", "usage_tracker"} {
		dep, ok := deps[key].(map[string]any)
		if !ok {
			t.Fatalf("expected dependency %s map, got %#v", key, deps[key])
		}
		if dep["status"] != "degraded" {
			t.Fatalf("expected %s degraded, got %#v", key, dep["status"])
		}
	}
}

func TestAuthMiddlewarePreservesRequestIDOnUnauthorized(t *testing.T) {
	handler := requestLogger(nil, authMiddleware(nil, "secret")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))

	req := httptest.NewRequest(http.MethodGet, "/api/companies", nil)
	req.Header.Set(requestIDHeader, "req-fixed")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, rr.Code)
	}
	if got := rr.Header().Get(requestIDHeader); got != "req-fixed" {
		t.Fatalf("expected response request id %q, got %q", "req-fixed", got)
	}

	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal unauthorized response: %v", err)
	}
	if payload["request_id"] != "req-fixed" {
		t.Fatalf("expected body request id %q, got %#v", "req-fixed", payload["request_id"])
	}
	if payload["detail"] != "当前请求缺少 Bearer Token 或登录会话 Cookie。" {
		t.Fatalf("unexpected detail: %#v", payload["detail"])
	}
}

func TestAuthMiddlewareGeneratesRequestIDWhenMissing(t *testing.T) {
	handler := requestLogger(nil, authMiddleware(nil, "secret")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))

	req := httptest.NewRequest(http.MethodGet, "/api/companies", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, rr.Code)
	}
	requestID := rr.Header().Get(requestIDHeader)
	if requestID == "" {
		t.Fatal("expected generated request id header")
	}

	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal unauthorized response: %v", err)
	}
	if payload["request_id"] != requestID {
		t.Fatalf("expected body request id %q, got %#v", requestID, payload["request_id"])
	}
}

func TestIssueSessionTokenRoundTrip(t *testing.T) {
	token, expiresAt, err := issueSessionToken("11111111-1111-4111-8111-111111111111", "secret", time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	claims, err := validateJWT(token, []byte("secret"))
	if err != nil {
		t.Fatalf("validate token: %v", err)
	}
	if claims.Subject != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("unexpected subject: %s", claims.Subject)
	}
	if claims.ExpiresAt != expiresAt.Unix() {
		t.Fatalf("expected exp %d, got %d", expiresAt.Unix(), claims.ExpiresAt)
	}
}

func TestHandleLoginRejectsInvalidEmail(t *testing.T) {
	handler := handleLogin(&Services{}, &Config{JWTSecret: "secret", SessionTTL: time.Hour})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(`{"email":"bad-email","password":"Passw0rd!"}`))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rr.Code)
	}
}

func TestHandleSessionReturnsUnauthorizedWithoutSession(t *testing.T) {
	handler := handleSession(&Services{}, &Config{JWTSecret: "secret", SessionTTL: time.Hour})
	req := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, rr.Code)
	}
}

func TestHandleSessionReturnsServiceUnavailableWithToken(t *testing.T) {
	token, _, err := issueSessionToken("11111111-1111-4111-8111-111111111111", "secret", time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	handler := handleSession(&Services{}, &Config{JWTSecret: "secret", SessionTTL: time.Hour})
	req := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status %d, got %d", http.StatusServiceUnavailable, rr.Code)
	}

	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal service unavailable response: %v", err)
	}
	if payload["error"] != "service unavailable" {
		t.Fatalf("expected service unavailable error, got %#v", payload["error"])
	}
}

func TestHandleRegisterAssignsSuperAdminToFirstUser(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	passwordInsertMatcher := regexp.QuoteMeta(`
			INSERT INTO users (id, username, display_name, email, password_hash, status, role)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status
		`)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM users WHERE LOWER(email) = LOWER($1) LIMIT 1`)).
		WithArgs("founder@example.com").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM users WHERE role = $1`)).
		WithArgs(userRoleSuperAdmin).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(passwordInsertMatcher).
		WithArgs(sqlmock.AnyArg(), "founder@example.com", "Founder", "founder@example.com", sqlmock.AnyArg(), userStatusActive, userRoleSuperAdmin).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "display_name", "role", "status"}).AddRow("11111111-1111-4111-8111-111111111111", "founder@example.com", "Founder", userRoleSuperAdmin, userStatusActive))
	mock.ExpectCommit()

	handler := handleRegister(&Services{DB: db}, &Config{JWTSecret: "secret", SessionTTL: time.Hour})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewBufferString(`{"email":"Founder@example.com","password":"Passw0rd!","displayName":"Founder"}`))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal register response: %v", err)
	}
	if payload["role"] != userRoleSuperAdmin {
		t.Fatalf("expected role %q, got %#v", userRoleSuperAdmin, payload["role"])
	}
	if payload["email"] != "founder@example.com" {
		t.Fatalf("expected normalized email, got %#v", payload["email"])
	}

	assertMockExpectations(t, mock)
}

func TestHandleRegisterAssignsUserWhenSuperAdminExists(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	insertMatcher := regexp.QuoteMeta(`
			INSERT INTO users (id, username, display_name, email, password_hash, status, role)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status
		`)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM users WHERE LOWER(email) = LOWER($1) LIMIT 1`)).
		WithArgs("member@example.com").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM users WHERE role = $1`)).
		WithArgs(userRoleSuperAdmin).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(insertMatcher).
		WithArgs(sqlmock.AnyArg(), "member@example.com", "Member", "member@example.com", sqlmock.AnyArg(), userStatusActive, userRoleUser).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "display_name", "role", "status"}).AddRow("22222222-2222-4222-8222-222222222222", "member@example.com", "Member", userRoleUser, userStatusActive))
	mock.ExpectCommit()

	handler := handleRegister(&Services{DB: db}, &Config{JWTSecret: "secret", SessionTTL: time.Hour})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewBufferString(`{"email":"member@example.com","password":"Passw0rd!","displayName":"Member"}`))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal register response: %v", err)
	}
	if payload["role"] != userRoleUser {
		t.Fatalf("expected role %q, got %#v", userRoleUser, payload["role"])
	}

	assertMockExpectations(t, mock)
}

func TestHandleRegisterRejectsDuplicateEmail(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM users WHERE LOWER(email) = LOWER($1) LIMIT 1`)).
		WithArgs("taken@example.com").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("33333333-3333-4333-8333-333333333333"))
	mock.ExpectRollback()

	handler := handleRegister(&Services{DB: db}, &Config{JWTSecret: "secret", SessionTTL: time.Hour})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewBufferString(`{"email":"taken@example.com","password":"Passw0rd!","displayName":"Taken"}`))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusConflict, rr.Code, rr.Body.String())
	}

	assertMockExpectations(t, mock)
}

func TestHandleLoginRejectsWrongPassword(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	hash, err := hashPassword("Passw0rd!")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status, COALESCE(password_hash, ''), COALESCE(kyc_status, 'unverified'), COALESCE(kyc_level, 'tier1_basic')
		FROM users
		WHERE LOWER(email) = LOWER($1)
		LIMIT 1
	`)).
		WithArgs("member@example.com").
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "display_name", "role", "status", "password_hash", "kyc_status", "kyc_level"}).
			AddRow("22222222-2222-4222-8222-222222222222", "member@example.com", "Member", userRoleUser, userStatusActive, hash, "verified", "tier1_basic"))

	handler := handleLogin(&Services{DB: db}, &Config{JWTSecret: "secret", SessionTTL: time.Hour})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(`{"email":"member@example.com","password":"WrongPass1!"}`))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusUnauthorized, rr.Code, rr.Body.String())
	}

	assertMockExpectations(t, mock)
}

func TestHandleSessionReturnsAuthenticatedUser(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	token, expiresAt, err := issueSessionToken("11111111-1111-4111-8111-111111111111", "secret", time.Hour)
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

	handler := handleSession(&Services{DB: db}, &Config{JWTSecret: "secret", SessionTTL: time.Hour})
	req := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal session response: %v", err)
	}
	if payload["email"] != "founder@example.com" {
		t.Fatalf("expected email, got %#v", payload["email"])
	}
	if payload["role"] != userRoleSuperAdmin {
		t.Fatalf("expected role, got %#v", payload["role"])
	}
	if payload["kyc_status"] != "verified" {
		t.Fatalf("expected kyc_status verified, got %#v", payload["kyc_status"])
	}
	if payload["kyc_level"] != "tier3_enterprise" {
		t.Fatalf("expected kyc_level tier3_enterprise, got %#v", payload["kyc_level"])
	}
	if payload["expires_at"] != expiresAt.UTC().Format(time.RFC3339) {
		t.Fatalf("expected expires_at %q, got %#v", expiresAt.UTC().Format(time.RFC3339), payload["expires_at"])
	}

	assertMockExpectations(t, mock)
}

func TestHandleSessionRejectsMissingUserForValidToken(t *testing.T) {
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
		WillReturnError(sql.ErrNoRows)

	handler := handleSession(&Services{DB: db}, &Config{JWTSecret: "secret", SessionTTL: time.Hour})
	req := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusUnauthorized, rr.Code, rr.Body.String())
	}

	assertMockExpectations(t, mock)
}

func TestHandleGetAccountKYCIncludesDocumentURLs(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	userID := "11111111-1111-4111-8111-111111111111"
	appID := "22222222-2222-4222-8222-222222222222"
	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status, COALESCE(password_hash, ''), COALESCE(kyc_status, 'unverified'), COALESCE(kyc_level, 'tier1_basic')
		FROM users
		WHERE id = $1
		LIMIT 1
	`)).
		WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "display_name", "role", "status", "password_hash", "kyc_status", "kyc_level"}).
			AddRow(userID, "user@example.com", "User", userRoleUser, userStatusActive, "", "pending", "tier1_basic"))
	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT id, user_id, kyc_level, status, full_name, id_document_type, id_document_number,
		       COALESCE(document_image_urls, '[]'::jsonb), COALESCE(rejection_reason, ''), created_at, updated_at
		FROM user_kyc_records
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`)).
		WithArgs(userID, 10).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "kyc_level", "status", "full_name", "id_document_type", "id_document_number", "document_image_urls", "rejection_reason", "created_at", "updated_at"}).
			AddRow(appID, userID, "tier1_basic", "pending", "Alice Doe", "passport", "P123456", []byte(`["https://example.test/passport.png"]`), "", now, now))
	expectAccessLogInsert(mock, userID, "read", "account_kyc", userID)

	req := httptest.NewRequest(http.MethodGet, "/api/account/kyc", nil)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), userID))
	rr := httptest.NewRecorder()

	handleGetAccountKYC(&Services{DB: db}).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	var payload struct {
		Applications []accountKYCApplication `json:"applications"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Applications) != 1 || len(payload.Applications[0].DocumentImageURLs) != 1 || payload.Applications[0].DocumentImageURLs[0] != "https://example.test/passport.png" {
		t.Fatalf("unexpected applications: %#v", payload.Applications)
	}
	assertMockExpectations(t, mock)
}

func TestHandleSubmitAccountKYCRecordsApplicationAndAudit(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	userID := "11111111-1111-4111-8111-111111111111"
	appID := "22222222-2222-4222-8222-222222222222"
	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status, COALESCE(password_hash, ''), COALESCE(kyc_status, 'unverified'), COALESCE(kyc_level, 'tier1_basic')
		FROM users
		WHERE id = $1
		LIMIT 1
	`)).
		WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "display_name", "role", "status", "password_hash", "kyc_status", "kyc_level"}).
			AddRow(userID, "user@example.com", "User", userRoleUser, userStatusActive, "", "unverified", "tier1_basic"))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(1) FROM user_kyc_records WHERE user_id = $1 AND status = 'pending'`)).
		WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`
			INSERT INTO user_kyc_records (user_id, kyc_level, status, full_name, id_document_type, id_document_number, document_image_urls)
			VALUES ($1, $2, 'pending', $3, $4, $5, $6)
			RETURNING id, user_id, kyc_level, status, full_name, id_document_type, id_document_number, COALESCE(rejection_reason, ''), created_at, updated_at
		`)).
		WithArgs(userID, "tier2_advanced", "Alice Doe", "passport", "P123456", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "kyc_level", "status", "full_name", "id_document_type", "id_document_number", "rejection_reason", "created_at", "updated_at"}).
			AddRow(appID, userID, "tier2_advanced", "pending", "Alice Doe", "passport", "P123456", "", now, now))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE users SET kyc_status = CASE WHEN kyc_status = 'verified' THEN kyc_status ELSE 'pending' END, updated_at = NOW() WHERE id = $1`)).
		WithArgs(userID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	expectAccessLogInsert(mock, userID, "submit", "kyc_application", appID)

	req := httptest.NewRequest(http.MethodPost, "/api/account/kyc", strings.NewReader(`{"kyc_level":"tier2_advanced","full_name":"Alice Doe","id_document_type":"passport","id_document_number":"P123456","document_image_urls":["https://example.test/passport.png"]}`))
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), userID))
	rr := httptest.NewRecorder()

	handleSubmitAccountKYC(&Services{DB: db}).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	assertMockExpectations(t, mock)
}

func newMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	return db, mock
}

func assertMockExpectations(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func assertAnError(message string) error {
	return &testError{message: message}
}

type testError struct{ message string }

func (e *testError) Error() string { return e.message }

func TestMarketServiceAdapterGetNewsDigestReturnsProviderNotesOnPartialFailure(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	now := time.Now().UTC()
	serpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"news_results":[{"title":"local first","link":"https://example.com/local","source":"serp"}]}`))
	}))
	defer serpServer.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"us_equity","exchange":"NASDAQ"}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, owner_user_id, name, description, created_at, updated_at
		 FROM fund_companies WHERE id = $1`)).
		WithArgs("company-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "owner_user_id", "name", "description", "created_at", "updated_at"}).
			AddRow("company-1", "user-1", "Alpha Company", nil, now, now))

	service := newMarketServiceAdapter(db, marketdata.NewService(marketdata.Config{
		WebSearchURL:   "http://127.0.0.1:1",
		SerpAPIKeys:    []string{"serp-key"},
		SerpAPIBaseURL: serpServer.URL,
		NewsProviders:  []string{"web-search", "local-search"},
	}), nil)

	digest, err := service.GetNewsDigest("user-1", "fund-1", []string{"AAPL"}, 3)
	if err != nil {
		t.Fatalf("get news digest: %v", err)
	}
	if len(digest.Items) != 1 || digest.Items[0].Title != "local first" {
		t.Fatalf("expected local news item, got %#v", digest.Items)
	}
	if len(digest.ProviderNotes) != 1 || !strings.Contains(digest.ProviderNotes[0], "AAPL") {
		t.Fatalf("expected provider note for failed symbol, got %#v", digest.ProviderNotes)
	}

	assertMockExpectations(t, mock)
}

func TestMarketServiceAdapterGetNewsDigestReturnsUpstreamUnavailableWhenAllProvidersFail(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"us_equity","exchange":"NASDAQ"}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, owner_user_id, name, description, created_at, updated_at
		 FROM fund_companies WHERE id = $1`)).
		WithArgs("company-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "owner_user_id", "name", "description", "created_at", "updated_at"}).
			AddRow("company-1", "user-1", "Alpha Company", nil, now, now))

	service := newMarketServiceAdapter(db, marketdata.NewService(marketdata.Config{
		WebSearchURL:  "http://127.0.0.1:1",
		SerpAPIKeys:   []string{"serp-key"},
		NewsProviders: []string{"web-search", "local-search"},
	}), nil)

	_, err := service.GetNewsDigest("user-1", "fund-1", []string{"AAPL"}, 3)
	if !errors.Is(err, api.ErrUpstreamUnavailable) {
		t.Fatalf("expected upstream unavailable, got %v", err)
	}

	assertMockExpectations(t, mock)
}

func TestExtractLearningSummaryFromJSON(t *testing.T) {
	summary := extractLearningSummary(`{"summary":"交易员在高波动窗口保持了较好的成交节奏。"}`)
	if summary != "交易员在高波动窗口保持了较好的成交节奏。" {
		t.Fatalf("unexpected summary: %q", summary)
	}
}

func TestApplyLearningToEvolutionConfigMergesRecentLessons(t *testing.T) {
	tradingDate := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	dailyReturn := 0.018
	updated, err := applyLearningToEvolutionConfig(map[string]any{
		"recentLessons": []any{"保留已有经验"},
	}, learningResult{
		Summary:     "新的学习摘要",
		Lessons:     []string{"优先拆单", "保留已有经验"},
		Adjustments: []string{"减少追价"},
		Tags:        []string{"self_learning", "trader"},
	}, tradingDate, dailyReturn)
	if err != nil {
		t.Fatalf("apply learning config: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(updated, &payload); err != nil {
		t.Fatalf("unmarshal updated config: %v", err)
	}
	lessons, ok := payload["recentLessons"].([]any)
	if !ok || len(lessons) != 2 {
		t.Fatalf("unexpected recent lessons payload: %#v", payload["recentLessons"])
	}
	if payload["lastLearningSummary"] != "新的学习摘要" {
		t.Fatalf("unexpected summary: %#v", payload["lastLearningSummary"])
	}
	if payload["lastLearningDate"] != "2026-05-11" {
		t.Fatalf("unexpected learning date: %#v", payload["lastLearningDate"])
	}
	if payload["lastDailyReturn"] != dailyReturn {
		t.Fatalf("unexpected daily return: %#v", payload["lastDailyReturn"])
	}
}

func TestParseEvolutionLearningConfigHonorsDisableFlags(t *testing.T) {
	config, enabled, autoApply, maxLessons := parseEvolutionLearningConfig(json.RawMessage(`{"dailyLearningEnabled":false,"autoApplyAdjustments":false,"maxLessonsPerDay":5}`))
	if enabled {
		t.Fatal("expected daily learning to be disabled")
	}
	if autoApply {
		t.Fatal("expected auto apply to be disabled")
	}
	if maxLessons != 5 {
		t.Fatalf("expected max lessons 5, got %d", maxLessons)
	}
	if _, ok := config["dailyLearningEnabled"]; !ok {
		t.Fatalf("expected config to preserve original fields: %#v", config)
	}
}

func TestResolveSkillsMatchesRoleFocusAndStep(t *testing.T) {
	agent := &repository.Agent{
		Role:        "researcher",
		Focus:       sql.NullString{String: "macro", Valid: true},
		SkillConfig: json.RawMessage(`{"enabled":true,"skills":[{"key":"macro-checklist","name":"宏观检查","content":"先检查 CPI 和 FOMC 风险。","priority":80,"match":{"roles":["researcher"],"focuses":["macro"],"workflowSteps":["macro_brief"]}}]}`),
	}
	skills := resolveSkills(agent, skillScenario{AgentRole: "researcher", AgentFocus: "macro", WorkflowStep: "macro_brief"})
	if len(skills) != 1 {
		t.Fatalf("expected one matched skill, got %#v", skills)
	}
	if skills[0].Key != "macro-checklist" {
		t.Fatalf("unexpected skill key: %#v", skills[0])
	}
}

func TestResolveSkillsReturnsEmptyWhenScenarioDoesNotMatch(t *testing.T) {
	agent := &repository.Agent{
		Role:        "trader",
		SkillConfig: json.RawMessage(`{"enabled":true,"skills":[{"key":"trade-checklist","content":"拆单执行","match":{"roles":["trader"],"workflowSteps":["trade_execution"],"scenarioKeywords":["尾盘"]}}]}`),
	}
	skills := resolveSkills(agent, skillScenario{AgentRole: "trader", WorkflowStep: "trade_execution", Keywords: []string{"盘中平稳"}})
	if len(skills) != 0 {
		t.Fatalf("expected no matched skills, got %#v", skills)
	}
}

func TestResolveSkillsReturnsEmptyWhenConfigDisabled(t *testing.T) {
	agent := &repository.Agent{
		Role:        "researcher",
		SkillConfig: json.RawMessage(`{"enabled":false,"skills":[{"key":"macro-checklist","content":"ignored"}]}`),
	}
	if skills := resolveSkills(agent, skillScenario{AgentRole: "researcher", WorkflowStep: "macro_brief"}); len(skills) != 0 {
		t.Fatalf("expected disabled config to resolve no skills, got %#v", skills)
	}
}

func TestResolveSkillsOrdersByPriority(t *testing.T) {
	agent := &repository.Agent{
		Role:        "pm",
		SkillConfig: json.RawMessage(`{"enabled":true,"skills":[{"key":"lower","content":"second","priority":10,"match":{"roles":["pm"],"workflowSteps":["pm_plan"]}},{"key":"higher","content":"first","priority":100,"match":{"roles":["pm"],"workflowSteps":["pm_plan"]}}]}`),
	}
	skills := resolveSkills(agent, skillScenario{AgentRole: "pm", WorkflowStep: "pm_plan"})
	if len(skills) != 2 {
		t.Fatalf("expected two skills, got %#v", skills)
	}
	if skills[0].Key != "higher" || skills[1].Key != "lower" {
		t.Fatalf("expected priority order, got %#v", skills)
	}
}

func TestBuildTraderSkillContextUsesTradeExecutionScenario(t *testing.T) {
	agent := &repository.Agent{
		Role:        "trader",
		SkillConfig: json.RawMessage(`{"enabled":true,"skills":[{"key":"trade-checklist","content":"拆单执行并检查流动性","match":{"roles":["trader"],"workflowSteps":["trade_execution"],"scenarioKeywords":["AAPL"]}}]}`),
	}
	context := buildTraderSkillContext(UserLanguageZH, agent, &repository.InvestmentPlan{Reasoning: sql.NullString{String: "关注 AAPL 成交冲击", Valid: true}}, []repository.PlanAction{{Symbol: "AAPL", Action: "buy"}})
	if !strings.Contains(context, "拆单执行并检查流动性") {
		t.Fatalf("expected trader skill content in context, got %q", context)
	}
	if !strings.Contains(context, "匹配技能：") {
		t.Fatalf("expected zh header in context, got %q", context)
	}
	if strings.Contains(buildTraderSkillContext(UserLanguageZH, agent, &repository.InvestmentPlan{}, []repository.PlanAction{{Symbol: "MSFT", Action: "buy"}}), "拆单执行并检查流动性") {
		t.Fatal("expected unmatched trader scenario to omit skill context")
	}
	enContext := buildTraderSkillContext(UserLanguageEN, agent, &repository.InvestmentPlan{Reasoning: sql.NullString{String: "关注 AAPL 成交冲击", Valid: true}}, []repository.PlanAction{{Symbol: "AAPL", Action: "buy"}})
	if !strings.Contains(enContext, "Matched skills:") {
		t.Fatalf("expected en header in context, got %q", enContext)
	}
}

func TestBuildRiskSkillContextUsesRiskReviewScenario(t *testing.T) {
	agent := &repository.Agent{
		Role:        "risk",
		SkillConfig: json.RawMessage(`{"enabled":true,"skills":[{"key":"risk-checklist","content":"重点检查单票权重与流动性","match":{"roles":["risk"],"workflowSteps":["risk_review"],"scenarioKeywords":["AAPL"]}}]}`),
	}
	context := buildRiskSkillContext(UserLanguageZH, agent, &repository.InvestmentPlan{Reasoning: sql.NullString{String: "AAPL 仓位调整", Valid: true}}, []repository.PlanAction{{Symbol: "AAPL", Action: "buy"}}, nil)
	if !strings.Contains(context, "重点检查单票权重与流动性") {
		t.Fatalf("expected risk skill content in context, got %q", context)
	}
}

func TestAppendSkillContextPreservesBaseReasoning(t *testing.T) {
	combined := appendSkillContext("原始推理", "Matched skills:\n- 技能\n说明")
	if !strings.Contains(combined, "原始推理") || !strings.Contains(combined, "Matched skills:") {
		t.Fatalf("expected combined context, got %q", combined)
	}
}

func TestRenderSkillContextIncludesMatchedSkillContent(t *testing.T) {
	context := renderSkillContext(UserLanguageZH, []resolvedSkill{{
		Key:         "trade-checklist",
		Name:        "交易清单",
		Description: "执行前检查",
		Content:     "先确认流动性，再决定是否拆单。",
		Priority:    100,
	}})
	if !strings.Contains(context, "交易清单") {
		t.Fatalf("expected skill name in context, got %q", context)
	}
	if !strings.Contains(context, "先确认流动性") {
		t.Fatalf("expected skill content in context, got %q", context)
	}
}

type containsAllArg []string

func (m containsAllArg) Match(v driver.Value) bool {
	var text string
	switch value := v.(type) {
	case string:
		text = value
	case []byte:
		text = string(value)
	default:
		return false
	}
	for _, expected := range m {
		if !strings.Contains(text, expected) {
			return false
		}
	}
	return true
}

func TestRuntimeRiskAgentReviewPlanPersistsMatchedSkillContext(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, reasoning, risk_score, expected_return, risk_review, discussion_snapshot, roundtable_id, pm_agent_id, confidence, created_at, updated_at
			 FROM investment_plans WHERE id = $1`)).
		WithArgs("plan-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "reasoning", "risk_score", "expected_return", "risk_review", "discussion_snapshot", "roundtable_id", "pm_agent_id", "confidence", "created_at", "updated_at"}).
			AddRow("plan-1", "fund-1", now, "pending_user", "AAPL 仓位调整", 0.1, 0.2, []byte(`{}`), []byte(`{}`), nil, nil, nil, now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, plan_id, instrument_key, symbol, market, exchange, asset_class, instrument_type, action, position_side, open_close, quantity, price, amount, stop_loss, take_profit, reasoning, confidence, supported_by, opposed_by, execution_status, sort_order, quote_currency, settlement_currency, margin_mode, leverage, contract_multiplier, expiry_date, reduce_only, quote_refreshed_at, auto_executed_at, sleeve, regime_tag, signal_source, exit_reason, strategy
			 FROM plan_actions WHERE plan_id = $1 ORDER BY sort_order, id`)).
		WithArgs("plan-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "plan_id", "instrument_key", "symbol", "market", "exchange", "asset_class", "instrument_type", "action", "position_side", "open_close", "quantity", "price", "amount", "stop_loss", "take_profit", "reasoning", "confidence", "supported_by", "opposed_by", "execution_status", "sort_order", "quote_currency", "settlement_currency", "margin_mode", "leverage", "contract_multiplier", "expiry_date", "reduce_only", "quote_refreshed_at", "auto_executed_at", "sleeve", "regime_tag", "signal_source", "exit_reason", "strategy"}).
			AddRow("action-1", "plan-1", "AAPL", "AAPL", nil, nil, nil, nil, "buy", nil, nil, 10, 100.0, 1000.0, nil, nil, "原始推理", 0.8, "{}", "{}", "pending", 0, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, instrument_key, symbol, name, market, exchange, asset_class, instrument_type, position_side, quote_currency, settlement_currency, margin_mode, quantity, available_qty, cost_price, current_price, market_value, weight, leverage, contract_multiplier, expiry_date, unrealized_pnl, margin_used, updated_at
			 FROM holding_positions WHERE fund_id = $1 ORDER BY instrument_key`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "instrument_key", "symbol", "name", "market", "exchange", "asset_class", "instrument_type", "position_side", "quote_currency", "settlement_currency", "margin_mode", "quantity", "available_qty", "cost_price", "current_price", "market_value", "weight", "leverage", "contract_multiplier", "expiry_date", "unrealized_pnl", "margin_used", "updated_at"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, agent_id, role, focus, joined_at, status, updated_at
			 FROM fund_team_members WHERE fund_id = $1 ORDER BY joined_at`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "agent_id", "role", "focus", "joined_at", "status", "updated_at"}).
			AddRow("member-1", "fund-1", "agent-risk-1", "risk", nil, now, "active", now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
			 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"us_equity","primaryDirection":"stocks","universe":{"symbols":["AAPL","MSFT"],"themes":["AI infra"]}}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, user_id, name, role, focus, llm_model, model_provider, model_name, system_prompt, skill_config, domain_config, evolution_config, pending_marketplace_snapshot, marketplace_snapshot_imported_at, status, created_at, updated_at
			 FROM agents WHERE id = $1`)).
		WithArgs("agent-risk-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "name", "role", "focus", "llm_model", "model_provider", "model_name", "system_prompt", "skill_config", "domain_config", "evolution_config", "pending_marketplace_snapshot", "marketplace_snapshot_imported_at", "status", "created_at", "updated_at"}).
			AddRow("agent-risk-1", "user-1", "Risk Agent", "risk", nil, nil, nil, nil, nil, []byte(`{"enabled":true,"skills":[{"key":"risk-checklist","content":"重点检查单票权重与流动性","match":{"roles":["risk"],"workflowSteps":["risk_review"],"scenarioKeywords":["AAPL"]}}]}`), []byte(`{}`), []byte(`{}`), []byte(`{}`), nil, "active", now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
			 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"us_equity","primaryDirection":"stocks","universe":{"symbols":["AAPL","MSFT"],"themes":["AI infra"]}}`), now, now))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE investment_plans SET risk_review = $1, updated_at = NOW() WHERE id = $2`)).
		WithArgs(containsAllArg{"重点检查单票权重与流动性", "市场：us_equity", "主要方向：stocks", "标的池代码：AAPL、MSFT", `"matchedSkills":true`}, "plan-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	risk := &runtimeRiskAgent{
		planRepo:     repository.NewPlanRepo(db),
		fundRepo:     repository.NewFundRepo(db),
		positionRepo: repository.NewPositionRepo(db),
		teamRepo:     repository.NewTeamRepo(db),
		agentRepo:    repository.NewAgentRepo(db),
	}
	approved, remarks, err := risk.ReviewPlan(context.Background(), &workflow.InvestmentPlanResult{ID: "plan-1", FundID: "fund-1"})
	if err != nil {
		t.Fatalf("review plan: %v", err)
	}
	if !approved {
		t.Fatal("expected risk review to approve plan")
	}
	if !strings.Contains(remarks, "重点检查单票权重与流动性") {
		t.Fatalf("expected remarks to include matched skill context, got %q", remarks)
	}
	if !strings.Contains(remarks, "市场：us_equity") || !strings.Contains(remarks, "主要方向：stocks") || !strings.Contains(remarks, "标的池代码：AAPL、MSFT") {
		t.Fatalf("expected remarks to include fund focus context, got %q", remarks)
	}

	assertMockExpectations(t, mock)
}

func TestRuntimeTradingEngineExecutePersistsMatchedSkillContextBeforeExecution(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, reasoning, risk_score, expected_return, risk_review, discussion_snapshot, roundtable_id, pm_agent_id, confidence, created_at, updated_at
			 FROM investment_plans WHERE id = $1`)).
		WithArgs("plan-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "reasoning", "risk_score", "expected_return", "risk_review", "discussion_snapshot", "roundtable_id", "pm_agent_id", "confidence", "created_at", "updated_at"}).
			AddRow("plan-1", "fund-1", now, "approved", "AAPL 执行计划", 0.1, 0.2, []byte(`{}`), []byte(`{}`), nil, nil, nil, now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
			 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"us_equity","primaryDirection":"stocks","universe":{"symbols":["AAPL","MSFT"],"themes":["AI infra"],"customFilters":["marketCap>1T"]}}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, plan_id, instrument_key, symbol, market, exchange, asset_class, instrument_type, action, position_side, open_close, quantity, price, amount, stop_loss, take_profit, reasoning, confidence, supported_by, opposed_by, execution_status, sort_order, quote_currency, settlement_currency, margin_mode, leverage, contract_multiplier, expiry_date, reduce_only, quote_refreshed_at, auto_executed_at, sleeve, regime_tag, signal_source, exit_reason, strategy
			 FROM plan_actions WHERE plan_id = $1 ORDER BY sort_order, id`)).
		WithArgs("plan-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "plan_id", "instrument_key", "symbol", "market", "exchange", "asset_class", "instrument_type", "action", "position_side", "open_close", "quantity", "price", "amount", "stop_loss", "take_profit", "reasoning", "confidence", "supported_by", "opposed_by", "execution_status", "sort_order", "quote_currency", "settlement_currency", "margin_mode", "leverage", "contract_multiplier", "expiry_date", "reduce_only", "quote_refreshed_at", "auto_executed_at", "sleeve", "regime_tag", "signal_source", "exit_reason", "strategy"}).
			AddRow("action-1", "plan-1", "AAPL", "AAPL", nil, nil, nil, nil, "buy", nil, nil, 10, 100.0, 1000.0, nil, nil, "原始推理", 0.8, "{}", "{}", "pending", 0, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, agent_id, role, focus, joined_at, status, updated_at
			 FROM fund_team_members WHERE fund_id = $1 ORDER BY joined_at`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "agent_id", "role", "focus", "joined_at", "status", "updated_at"}).
			AddRow("member-1", "fund-1", "agent-trader-1", "trader", nil, now, "active", now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, user_id, name, role, focus, llm_model, model_provider, model_name, system_prompt, skill_config, domain_config, evolution_config, pending_marketplace_snapshot, marketplace_snapshot_imported_at, status, created_at, updated_at
			 FROM agents WHERE id = $1`)).
		WithArgs("agent-trader-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "name", "role", "focus", "llm_model", "model_provider", "model_name", "system_prompt", "skill_config", "domain_config", "evolution_config", "pending_marketplace_snapshot", "marketplace_snapshot_imported_at", "status", "created_at", "updated_at"}).
			AddRow("agent-trader-1", "user-1", "Trader Agent", "trader", nil, nil, nil, nil, nil, []byte(`{"enabled":true,"skills":[{"key":"trade-checklist","content":"拆单执行并检查流动性","match":{"roles":["trader"],"workflowSteps":["trade_execution"],"scenarioKeywords":["AAPL"]}}]}`), []byte(`{}`), []byte(`{}`), []byte(`{}`), nil, "active", now, now))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE plan_actions SET reasoning = $1 WHERE id = $2 AND plan_id = $3`)).
		WithArgs(containsAllArg{"原始推理", "拆单执行并检查流动性", "匹配技能："}, "action-1", "plan-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, instrument_key, symbol, name, market, exchange, asset_class, instrument_type, position_side, quote_currency, settlement_currency, margin_mode, quantity, available_qty, cost_price, current_price, market_value, weight, leverage, contract_multiplier, expiry_date, unrealized_pnl, margin_used, updated_at
			 FROM holding_positions WHERE fund_id = $1 ORDER BY instrument_key`)).
		WithArgs("fund-1").
		WillReturnError(assertAnError("position_repo: boom"))

	engine := &runtimeTradingEngine{
		planRepo:     repository.NewPlanRepo(db),
		fundRepo:     repository.NewFundRepo(db),
		tradeRepo:    repository.NewTradeRepo(db),
		positionRepo: repository.NewPositionRepo(db),
		teamRepo:     repository.NewTeamRepo(db),
		agentRepo:    repository.NewAgentRepo(db),
	}
	err := engine.Execute(context.Background(), "plan-1")
	if err == nil {
		t.Fatal("expected execution to stop after position repo failure")
	}
	if !strings.Contains(err.Error(), "position_repo: boom") {
		t.Fatalf("expected position repo failure, got %v", err)
	}

	assertMockExpectations(t, mock)
}

func TestRuntimeTradingEngineExecutePlanActionOpensFuturesLong(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta(`
		WITH ins AS (
			INSERT INTO trade_executions (
				fund_id, plan_id, plan_action_id, instrument_key, symbol,
				market, exchange, asset_class, instrument_type, side,
				position_side, open_close, order_type, quantity, price, amount,
				trading_mode, broker_order_id, mcp_server_id, status, executed_at,
				quote_currency, settlement_currency, margin_mode, leverage,
				contract_multiplier, expiry_date, reduce_only,
				stop_price, trail_amount, trail_percent, display_qty,
				time_in_force, good_till_date, parent_trade_id,
				client_idempotency_key
			)
			VALUES (
				$1, $2, $3, $4, $5,
				$6, $7, $8, $9, $10,
				$11, $12, $13, $14, $15, $16,
				$17, $18, $19, $20, $21,
				$22, $23, $24, $25,
				$26, $27, $28,
				$29, $30, $31, $32,
				$33, $34, $35,
				$36
			)
			ON CONFLICT (client_idempotency_key)
				WHERE client_idempotency_key IS NOT NULL
				DO NOTHING
			RETURNING id
		)
		SELECT id FROM ins
		UNION ALL
		SELECT id FROM trade_executions
			WHERE client_idempotency_key = $36
				AND $36 IS NOT NULL
		LIMIT 1`)).
		WithArgs(
			"fund-1",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			"CME:ESU2026",
			"ESU2026",
			sql.NullString{String: "futures", Valid: true},
			sql.NullString{String: "CME", Valid: true},
			sql.NullString{String: "futures", Valid: true},
			sql.NullString{},
			"buy",
			sql.NullString{String: "long", Valid: true},
			sql.NullString{String: "open", Valid: true},
			"limit",
			2.0,
			sql.NullFloat64{Float64: 100, Valid: true},
			sql.NullFloat64{Float64: 2000, Valid: true},
			"simulation",
			sql.NullString{},
			sql.NullString{},
			"filled",
			sqlmock.AnyArg(),
			sql.NullString{String: "USD", Valid: true},
			sql.NullString{String: "USD", Valid: true},
			sql.NullString{String: "cross", Valid: true},
			sql.NullFloat64{Float64: 5, Valid: true},
			sql.NullFloat64{Float64: 10, Valid: true},
			sql.NullTime{Time: now, Valid: true},
			sql.NullBool{Bool: true, Valid: true},
			sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{},
			sql.NullString{}, sql.NullTime{}, sql.NullString{},
			sql.NullString{String: "trade:action-1:buy:2", Valid: true},
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("trade-1"))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE trade_executions
		 SET status = $1, filled_qty = $2, filled_price = $3, fee_commission = $4, fee_stamp_tax = $5, fee_transfer = $6, slippage_pct = $7, executed_at = NOW()
		 WHERE id = $8`)).
		WithArgs("filled", 2.0, sql.NullFloat64{Float64: 100, Valid: true}, 2.0, 0.0, 0.0004, sqlmock.AnyArg(), "trade-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	engine := &runtimeTradingEngine{tradeRepo: repository.NewTradeRepo(db)}
	availableCash := 10000.0
	positions := map[string]repository.HoldingPosition{}
	action := repository.PlanAction{
		ID:                 "action-1",
		InstrumentKey:      "CME:ESU2026",
		Symbol:             "ESU2026",
		Market:             sql.NullString{String: "futures", Valid: true},
		Exchange:           sql.NullString{String: "CME", Valid: true},
		AssetClass:         sql.NullString{String: "futures", Valid: true},
		Action:             "buy",
		PositionSide:       sql.NullString{String: "long", Valid: true},
		OpenClose:          sql.NullString{String: "open", Valid: true},
		Quantity:           sql.NullFloat64{Float64: 2, Valid: true},
		Price:              sql.NullFloat64{Float64: 100, Valid: true},
		QuoteCurrency:      sql.NullString{String: "USD", Valid: true},
		SettlementCurrency: sql.NullString{String: "USD", Valid: true},
		MarginMode:         sql.NullString{String: "cross", Valid: true},
		Leverage:           sql.NullFloat64{Float64: 5, Valid: true},
		ContractMultiplier: sql.NullFloat64{Float64: 10, Valid: true},
		ExpiryDate:         sql.NullTime{Time: now, Valid: true},
		ReduceOnly:         sql.NullBool{Bool: true, Valid: true},
	}
	status, err := engine.executePlanAction(context.Background(), &repository.Fund{ID: "fund-1", TradingMode: "simulation", TotalAssets: 100000}, &repository.InvestmentPlan{ID: "plan-1"}, action, positions, &availableCash, &hardRiskState{TotalAssets: 100000}, executePlanActionOptions{})
	if err != nil {
		t.Fatalf("execute futures open: %v", err)
	}
	if status != "filled" {
		t.Fatalf("expected status filled, got %q", status)
	}
	position, ok := positions[positionMapKey(action.InstrumentKey, action.Symbol)]
	if !ok {
		t.Fatal("expected futures position to be created")
	}
	if position.Quantity != 2 || position.AvailableQty != 2 {
		t.Fatalf("unexpected futures quantity: %#v", position)
	}
	if !position.MarginUsed.Valid || position.MarginUsed.Float64 != 400 {
		t.Fatalf("unexpected futures margin used: %#v", position.MarginUsed)
	}
	if !position.UnrealizedPnL.Valid || position.UnrealizedPnL.Float64 != 0 {
		t.Fatalf("unexpected futures pnl: %#v", position.UnrealizedPnL)
	}
	if availableCash != 9597.9996 {
		t.Fatalf("unexpected available cash: %v", availableCash)
	}

	assertMockExpectations(t, mock)
}

func TestRuntimeTradingEngineExecutePlanActionClosesFuturesShort(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`
		WITH ins AS (
			INSERT INTO trade_executions (
				fund_id, plan_id, plan_action_id, instrument_key, symbol,
				market, exchange, asset_class, instrument_type, side,
				position_side, open_close, order_type, quantity, price, amount,
				trading_mode, broker_order_id, mcp_server_id, status, executed_at,
				quote_currency, settlement_currency, margin_mode, leverage,
				contract_multiplier, expiry_date, reduce_only,
				stop_price, trail_amount, trail_percent, display_qty,
				time_in_force, good_till_date, parent_trade_id,
				client_idempotency_key
			)
			VALUES (
				$1, $2, $3, $4, $5,
				$6, $7, $8, $9, $10,
				$11, $12, $13, $14, $15, $16,
				$17, $18, $19, $20, $21,
				$22, $23, $24, $25,
				$26, $27, $28,
				$29, $30, $31, $32,
				$33, $34, $35,
				$36
			)
			ON CONFLICT (client_idempotency_key)
				WHERE client_idempotency_key IS NOT NULL
				DO NOTHING
			RETURNING id
		)
		SELECT id FROM ins
		UNION ALL
		SELECT id FROM trade_executions
			WHERE client_idempotency_key = $36
				AND $36 IS NOT NULL
		LIMIT 1`)).
		WithArgs(
			"fund-1",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			"CME:NQU2026",
			"NQU2026",
			sql.NullString{String: "futures", Valid: true},
			sql.NullString{String: "CME", Valid: true},
			sql.NullString{String: "futures", Valid: true},
			sql.NullString{},
			"buy",
			sql.NullString{String: "short", Valid: true},
			sql.NullString{String: "close", Valid: true},
			"limit",
			2.0,
			sql.NullFloat64{Float64: 90, Valid: true},
			sql.NullFloat64{Float64: 1800, Valid: true},
			"simulation",
			sql.NullString{},
			sql.NullString{},
			"filled",
			sqlmock.AnyArg(),
			sql.NullString{},
			sql.NullString{},
			sql.NullString{},
			sql.NullFloat64{Float64: 5, Valid: true},
			sql.NullFloat64{Float64: 10, Valid: true},
			sql.NullTime{},
			sql.NullBool{},
			sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{},
			sql.NullString{}, sql.NullTime{}, sql.NullString{},
			sql.NullString{String: "trade:action-2:buy:2", Valid: true},
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("trade-2"))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE trade_executions
		 SET status = $1, filled_qty = $2, filled_price = $3, fee_commission = $4, fee_stamp_tax = $5, fee_transfer = $6, slippage_pct = $7, executed_at = NOW()
		 WHERE id = $8`)).
		WithArgs("filled", 2.0, sql.NullFloat64{Float64: 90, Valid: true}, 1.8, 1.8, 0.0004, sqlmock.AnyArg(), "trade-2").
		WillReturnResult(sqlmock.NewResult(0, 1))

	engine := &runtimeTradingEngine{tradeRepo: repository.NewTradeRepo(db)}
	availableCash := 1000.0
	positions := map[string]repository.HoldingPosition{
		positionMapKey("CME:NQU2026", "NQU2026"): {
			FundID:             "fund-1",
			InstrumentKey:      "CME:NQU2026",
			Symbol:             "NQU2026",
			Market:             sql.NullString{String: "futures", Valid: true},
			Exchange:           sql.NullString{String: "CME", Valid: true},
			AssetClass:         sql.NullString{String: "futures", Valid: true},
			PositionSide:       sql.NullString{String: "short", Valid: true},
			Quantity:           3,
			AvailableQty:       3,
			CostPrice:          100,
			CurrentPrice:       100,
			Leverage:           sql.NullFloat64{Float64: 5, Valid: true},
			ContractMultiplier: sql.NullFloat64{Float64: 10, Valid: true},
		},
	}
	action := repository.PlanAction{
		ID:                 "action-2",
		InstrumentKey:      "CME:NQU2026",
		Symbol:             "NQU2026",
		Market:             sql.NullString{String: "futures", Valid: true},
		Exchange:           sql.NullString{String: "CME", Valid: true},
		AssetClass:         sql.NullString{String: "futures", Valid: true},
		Action:             "reduce",
		PositionSide:       sql.NullString{String: "short", Valid: true},
		OpenClose:          sql.NullString{String: "close", Valid: true},
		Quantity:           sql.NullFloat64{Float64: 2, Valid: true},
		Price:              sql.NullFloat64{Float64: 90, Valid: true},
		Leverage:           sql.NullFloat64{Float64: 5, Valid: true},
		ContractMultiplier: sql.NullFloat64{Float64: 10, Valid: true},
	}
	status, err := engine.executePlanAction(context.Background(), &repository.Fund{ID: "fund-1", TradingMode: "simulation", TotalAssets: 100000}, &repository.InvestmentPlan{ID: "plan-1"}, action, positions, &availableCash, &hardRiskState{TotalAssets: 100000}, executePlanActionOptions{})
	if err != nil {
		t.Fatalf("execute futures close: %v", err)
	}
	if status != "filled" {
		t.Fatalf("expected status filled, got %q", status)
	}
	position, ok := positions[positionMapKey(action.InstrumentKey, action.Symbol)]
	if !ok {
		t.Fatal("expected futures position to remain after partial close")
	}
	if position.Quantity != 1 || position.AvailableQty != 1 {
		t.Fatalf("unexpected remaining futures quantity: %#v", position)
	}
	if !position.MarginUsed.Valid || position.MarginUsed.Float64 != 180 {
		t.Fatalf("unexpected remaining margin used: %#v", position.MarginUsed)
	}
	if !position.UnrealizedPnL.Valid || position.UnrealizedPnL.Float64 != 100 {
		t.Fatalf("unexpected remaining unrealized pnl: %#v", position.UnrealizedPnL)
	}
	if availableCash != 1596.3996 {
		t.Fatalf("unexpected available cash after close: %v", availableCash)
	}

	assertMockExpectations(t, mock)
}

func TestRuntimeResearcherPoolMacroBriefIncludesMatchedSkillContext(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, agent_id, role, focus, joined_at, status, updated_at
			 FROM fund_team_members WHERE fund_id = $1 ORDER BY joined_at`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "agent_id", "role", "focus", "joined_at", "status", "updated_at"}).
			AddRow("member-1", "fund-1", "agent-researcher-1", "researcher", "macro", now, "active", now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
			 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Macro Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"us_equity","primaryDirection":"stocks","universe":{"symbols":["SPY","QQQ"],"themes":["macro"]}}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, user_id, name, role, focus, llm_model, model_provider, model_name, system_prompt, skill_config, domain_config, evolution_config, pending_marketplace_snapshot, marketplace_snapshot_imported_at, status, created_at, updated_at
			 FROM agents WHERE id = $1`)).
		WithArgs("agent-researcher-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "name", "role", "focus", "llm_model", "model_provider", "model_name", "system_prompt", "skill_config", "domain_config", "evolution_config", "pending_marketplace_snapshot", "marketplace_snapshot_imported_at", "status", "created_at", "updated_at"}).
			AddRow("agent-researcher-1", "user-1", "Macro Researcher", "researcher", "macro", nil, nil, nil, nil, []byte(`{"enabled":true,"skills":[{"key":"macro-checklist","content":"重点跟踪 CPI 与 FOMC 之前的风险资产定价","match":{"roles":["researcher"],"focuses":["macro"],"workflowSteps":["macro_brief"]}}]}`), []byte(`{}`), []byte(`{}`), []byte(`{}`), nil, "active", now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
			 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Macro Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"us_equity","primaryDirection":"stocks","universe":{"symbols":["SPY","QQQ"],"themes":["macro"]}}`), now, now))

	pool := runtimeResearcherPool{
		fundRepo:  repository.NewFundRepo(db),
		teamRepo:  repository.NewTeamRepo(db),
		agentRepo: repository.NewAgentRepo(db),
	}
	report, err := pool.MacroBrief(context.Background(), "fund-1", "2026-05-11")
	if err != nil {
		t.Fatalf("macro brief: %v", err)
	}
	if !strings.Contains(report.Content, "宏观简报暂不可用") {
		t.Fatalf("expected unavailable macro brief content, got %q", report.Content)
	}
	if !strings.Contains(report.Content, "重点跟踪 CPI 与 FOMC 之前的风险资产定价") {
		t.Fatalf("expected researcher skill context, got %q", report.Content)
	}
	if !strings.Contains(report.Content, "匹配技能：") {
		t.Fatalf("expected matched skills header, got %q", report.Content)
	}
	if !strings.Contains(report.Content, "市场：us_equity") || !strings.Contains(report.Content, "主要方向：stocks") || !strings.Contains(report.Content, "标的池代码：SPY、QQQ") {
		t.Fatalf("expected report to include fund focus context, got %q", report.Content)
	}

	assertMockExpectations(t, mock)
}

func TestRuntimeResearcherPoolMacroBriefIncludesFundFocusContext(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, agent_id, role, focus, joined_at, status, updated_at
			 FROM fund_team_members WHERE fund_id = $1 ORDER BY joined_at`)).
		WithArgs("fund-2").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "agent_id", "role", "focus", "joined_at", "status", "updated_at"}).
			AddRow("member-1", "fund-2", "agent-researcher-2", "researcher", "macro", now, "active", now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
			 FROM funds WHERE id = $1`)).
		WithArgs("fund-2").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-2", "company-1", "Sector Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"hk_equity","assetClass":"equity","primaryDirection":"stocks","universe":{"mode":"manual","symbols":["0700.HK"],"themes":["AI"],"sectors":["technology"]}}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, user_id, name, role, focus, llm_model, model_provider, model_name, system_prompt, skill_config, domain_config, evolution_config, pending_marketplace_snapshot, marketplace_snapshot_imported_at, status, created_at, updated_at
			 FROM agents WHERE id = $1`)).
		WithArgs("agent-researcher-2").
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "name", "role", "focus", "llm_model", "model_provider", "model_name", "system_prompt", "skill_config", "domain_config", "evolution_config", "pending_marketplace_snapshot", "marketplace_snapshot_imported_at", "status", "created_at", "updated_at"}).
			AddRow("agent-researcher-2", "user-1", "Macro Researcher", "researcher", "macro", nil, nil, nil, nil, []byte(`{"enabled":true,"skills":[{"key":"macro-checklist","content":"关注科技权重的宏观敏感度","match":{"roles":["researcher"],"focuses":["macro"],"workflowSteps":["macro_brief"]}}]}`), []byte(`{}`), []byte(`{}`), []byte(`{}`), nil, "active", now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
			 FROM funds WHERE id = $1`)).
		WithArgs("fund-2").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-2", "company-1", "Sector Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"hk_equity","assetClass":"equity","primaryDirection":"stocks","universe":{"mode":"manual","symbols":["0700.HK"],"themes":["AI"],"sectors":["technology"]}}`), now, now))

	pool := runtimeResearcherPool{
		fundRepo:  repository.NewFundRepo(db),
		teamRepo:  repository.NewTeamRepo(db),
		agentRepo: repository.NewAgentRepo(db),
	}
	report, err := pool.MacroBrief(context.Background(), "fund-2", "2026-05-11")
	if err != nil {
		t.Fatalf("macro brief: %v", err)
	}
	for _, expected := range []string{"市场：hk_equity", "资产类别：equity", "主要方向：stocks", "标的池模式：manual", "标的池代码：0700.HK", "标的池主题：AI", "标的池行业：technology", "关注科技权重的宏观敏感度"} {
		if !strings.Contains(report.Content, expected) {
			t.Fatalf("expected %q in report content, got %q", expected, report.Content)
		}
	}

	assertMockExpectations(t, mock)
}

func TestRuntimePMAgentGeneratePlanPersistsMatchedSkillContext(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	now := time.Now().UTC()
	roundtable := &workflow.RoundtableResult{
		ID:        "round-1",
		Consensus: []string{"新能源主线延续，考虑增配龙头仓位"},
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, agent_id, role, focus, joined_at, status, updated_at
			 FROM fund_team_members WHERE fund_id = $1 ORDER BY joined_at`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "agent_id", "role", "focus", "joined_at", "status", "updated_at"}).
			AddRow("member-1", "fund-1", "agent-pm-1", "pm", nil, now, "active", now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, user_id, name, role, focus, llm_model, model_provider, model_name, system_prompt, skill_config, domain_config, evolution_config, pending_marketplace_snapshot, marketplace_snapshot_imported_at, status, created_at, updated_at
			 FROM agents WHERE id = $1`)).
		WithArgs("agent-pm-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "name", "role", "focus", "llm_model", "model_provider", "model_name", "system_prompt", "skill_config", "domain_config", "evolution_config", "pending_marketplace_snapshot", "marketplace_snapshot_imported_at", "status", "created_at", "updated_at"}).
			AddRow("agent-pm-1", "user-1", "PM Agent", "pm", nil, nil, nil, nil, nil, []byte(`{"enabled":true,"skills":[{"key":"pm-checklist","content":"先校验主题集中度与仓位节奏","match":{"roles":["pm"],"workflowSteps":["pm_plan"],"scenarioKeywords":["新能源"]}}]}`), []byte(`{}`), []byte(`{}`), []byte(`{}`), nil, "active", now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
				 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"us_equity","assetClass":"equity","primaryDirection":"stocks","universe":{"symbols":["NVDA"]}}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
			 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"us_equity","assetClass":"equity","primaryDirection":"stocks","universe":{"symbols":["NVDA"]}}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, instrument_key, symbol, name, market, exchange, asset_class, instrument_type, position_side, quote_currency, settlement_currency, margin_mode, quantity, available_qty, cost_price, current_price, market_value, weight, leverage, contract_multiplier, expiry_date, unrealized_pnl, margin_used, updated_at
			 FROM holding_positions WHERE fund_id = $1 ORDER BY instrument_key`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "instrument_key", "symbol", "name", "market", "exchange", "asset_class", "instrument_type", "position_side", "quote_currency", "settlement_currency", "margin_mode", "quantity", "available_qty", "cost_price", "current_price", "market_value", "weight", "leverage", "contract_multiplier", "expiry_date", "unrealized_pnl", "margin_used", "updated_at"}))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO investment_plans (fund_id, trading_date, status, reasoning, risk_score, expected_return, roundtable_id, pm_agent_id, risk_review, discussion_snapshot, confidence)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			 RETURNING id`)).
		WithArgs("fund-1", sqlmock.AnyArg(), string(workflow.PlanStatusPendingUser), containsAllArg{"新能源主线延续", "先校验主题集中度与仓位节奏", "匹配技能："}, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("plan-1"))
	actionInsert := mock.ExpectPrepare(regexp.QuoteMeta(`INSERT INTO plan_actions (plan_id, instrument_key, symbol, market, exchange, asset_class, instrument_type, action, position_side, open_close, quantity, price, amount, stop_loss, take_profit, reasoning, confidence, supported_by, opposed_by, execution_status, sort_order, quote_currency, settlement_currency, margin_mode, leverage, contract_multiplier, expiry_date, reduce_only, sleeve, regime_tag, signal_source, exit_reason, strategy)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33)`))
	// Quote-unavailable contract (production-grade as of 2026-06-03):
	// when the quote service can't price the symbol we DOWNGRADE the
	// LLM-generated buy to a "watch" action with no quantity / price
	// / amount set. The previous behaviour stamped the PM budget
	// (here $9,700 = NAV × 10% × PlanBudgetSafetyMargin 0.97) into
	// the Price column with quantity=1, which the broker simulator
	// faithfully honoured as a limit order — that's how the
	// 96,226.4188 CNY/share 301308 fill on 2026-06-02 happened.
	// Reasoning must still mention "quote unavailable" and the
	// dollar budget on the table so operators can audit the
	// downgrade.
	//
	// The four trailing AnyArg slots are sleeve / regime_tag /
	// signal_source / exit_reason (Phase 3A-1). stampDefaultAttribution
	// stamps sleeve="llm_pm" + signal_source="llm_pm" on this
	// LLM-generated action regardless of whether it executes.
	actionInsert.ExpectExec().
		WithArgs("plan-1", sqlmock.AnyArg(), "NVDA", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "watch", sqlmock.AnyArg(), sqlmock.AnyArg(), nil, nil, nil, sqlmock.AnyArg(), sqlmock.AnyArg(), containsAllArg{"新能源主线延续", "quote unavailable", "NVDA", "downgraded to watch"}, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "pending", 0, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	agent := &runtimePMAgent{
		planRepo:     repository.NewPlanRepo(db),
		fundRepo:     repository.NewFundRepo(db),
		positionRepo: repository.NewPositionRepo(db),
		teamRepo:     repository.NewTeamRepo(db),
		agentRepo:    repository.NewAgentRepo(db),
	}
	plan, err := agent.GeneratePlan(context.Background(), "fund-1", "2026-05-10", roundtable)
	if err != nil {
		t.Fatalf("generate plan: %v", err)
	}
	if plan.ID != "plan-1" {
		t.Fatalf("expected plan id %q, got %q", "plan-1", plan.ID)
	}
	if plan.RoundtableID != "round-1" {
		t.Fatalf("expected roundtable id %q, got %q", "round-1", plan.RoundtableID)
	}

	assertMockExpectations(t, mock)
}

func TestRuntimeResearcherPoolPersistResearchMemoryUsesAnalysisLayer(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	now := time.Now().UTC()
	research := &marketdata.ResearchContext{
		Instrument: marketdata.InstrumentRef{Symbol: "AAPL", Market: "us_equity", AssetClass: "equity", QuoteCurrency: "USD"},
		Quote:      &marketdata.QuoteSnapshot{Symbol: "AAPL", Price: 123.45, QuoteCurrency: "USD", Source: "quantdinger", AsOf: now},
		Summary:    "earnings revision improving",
		Signals:    []string{"momentum positive"},
		News:       []marketdata.NewsItem{{Title: "AAPL supplier checks improve", Source: "web-search", PublishedAt: now}},
	}
	content := formatResearchContextBlock(UserLanguageZH, "AAPL", research)
	for _, expected := range []string{"AAPL：earnings revision improving", "报价 123.4500 USD（来源：quantdinger", "技术信号：momentum positive", "新闻：AAPL supplier checks improve"} {
		if !strings.Contains(content, expected) {
			t.Fatalf("expected %q in research content %q", expected, content)
		}
	}
	enContent := formatResearchContextBlock(UserLanguageEN, "AAPL", research)
	for _, expected := range []string{"AAPL: earnings revision improving", "Quote 123.4500 USD (source: quantdinger", "Signals: momentum positive", "News: AAPL supplier checks improve"} {
		if !strings.Contains(enContent, expected) {
			t.Fatalf("expected %q in english research content %q", expected, enContent)
		}
	}

	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO memories`)).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("memory-1"))

	pool := runtimeResearcherPool{memoryRepo: repository.NewMemoryRepo(db)}
	pool.persistResearchMemory(context.Background(), "fund-1", "agent-researcher-1", "2026-05-11", research)

	assertMockExpectations(t, mock)
}

func TestRuntimePMAgentBuildSkillContextIncludesFundFocusContext(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	now := time.Now().UTC()
	roundtable := &workflow.RoundtableResult{Consensus: []string{"光模块主线继续强化"}}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, agent_id, role, focus, joined_at, status, updated_at
			 FROM fund_team_members WHERE fund_id = $1 ORDER BY joined_at`)).
		WithArgs("fund-pm-focus").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "agent_id", "role", "focus", "joined_at", "status", "updated_at"}).
			AddRow("member-1", "fund-pm-focus", "agent-pm-focus", "pm", nil, now, "active", now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
			 FROM funds WHERE id = $1`)).
		WithArgs("fund-pm-focus").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-pm-focus", "company-1", "Focus Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"us_equity","primaryDirection":"stocks","universe":{"symbols":["NVDA","AVGO"],"themes":["CPO","AI infra"]}}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, user_id, name, role, focus, llm_model, model_provider, model_name, system_prompt, skill_config, domain_config, evolution_config, pending_marketplace_snapshot, marketplace_snapshot_imported_at, status, created_at, updated_at
			 FROM agents WHERE id = $1`)).
		WithArgs("agent-pm-focus").
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "name", "role", "focus", "llm_model", "model_provider", "model_name", "system_prompt", "skill_config", "domain_config", "evolution_config", "pending_marketplace_snapshot", "marketplace_snapshot_imported_at", "status", "created_at", "updated_at"}).
			AddRow("agent-pm-focus", "user-1", "PM Agent", "pm", nil, nil, nil, nil, nil, []byte(`{"enabled":true,"skills":[{"key":"pm-checklist","content":"先校验主题集中度与仓位节奏","match":{"roles":["pm"],"workflowSteps":["pm_plan"],"scenarioKeywords":["光模块"]}}]}`), []byte(`{}`), []byte(`{}`), []byte(`{}`), nil, "active", now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
			 FROM funds WHERE id = $1`)).
		WithArgs("fund-pm-focus").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-pm-focus", "company-1", "Focus Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"us_equity","primaryDirection":"stocks","universe":{"symbols":["NVDA","AVGO"],"themes":["CPO","AI infra"]}}`), now, now))

	agent := &runtimePMAgent{
		fundRepo:  repository.NewFundRepo(db),
		teamRepo:  repository.NewTeamRepo(db),
		agentRepo: repository.NewAgentRepo(db),
	}
	context := agent.buildSkillContext(context.Background(), "fund-pm-focus", roundtable)
	for _, expected := range []string{"先校验主题集中度与仓位节奏", "市场：us_equity", "主要方向：stocks", "标的池代码：NVDA、AVGO", "标的池主题：CPO、AI infra"} {
		if !strings.Contains(context, expected) {
			t.Fatalf("expected %q in PM skill context, got %q", expected, context)
		}
	}

	assertMockExpectations(t, mock)
}

func TestRuntimeRiskAgentReviewPlanPersistsFundFocusContext(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, reasoning, risk_score, expected_return, risk_review, discussion_snapshot, roundtable_id, pm_agent_id, confidence, created_at, updated_at
			 FROM investment_plans WHERE id = $1`)).
		WithArgs("plan-2").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "reasoning", "risk_score", "expected_return", "risk_review", "discussion_snapshot", "roundtable_id", "pm_agent_id", "confidence", "created_at", "updated_at"}).
			AddRow("plan-2", "fund-2", now, "pending_user", "0700.HK 仓位调整", 0.1, 0.2, []byte(`{}`), []byte(`{}`), nil, nil, nil, now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, plan_id, instrument_key, symbol, market, exchange, asset_class, instrument_type, action, position_side, open_close, quantity, price, amount, stop_loss, take_profit, reasoning, confidence, supported_by, opposed_by, execution_status, sort_order, quote_currency, settlement_currency, margin_mode, leverage, contract_multiplier, expiry_date, reduce_only, quote_refreshed_at, auto_executed_at, sleeve, regime_tag, signal_source, exit_reason, strategy
			 FROM plan_actions WHERE plan_id = $1 ORDER BY sort_order, id`)).
		WithArgs("plan-2").
		WillReturnRows(sqlmock.NewRows([]string{"id", "plan_id", "instrument_key", "symbol", "market", "exchange", "asset_class", "instrument_type", "action", "position_side", "open_close", "quantity", "price", "amount", "stop_loss", "take_profit", "reasoning", "confidence", "supported_by", "opposed_by", "execution_status", "sort_order", "quote_currency", "settlement_currency", "margin_mode", "leverage", "contract_multiplier", "expiry_date", "reduce_only", "quote_refreshed_at", "auto_executed_at", "sleeve", "regime_tag", "signal_source", "exit_reason", "strategy"}).
			AddRow("action-2", "plan-2", "0700.HK", "0700.HK", nil, nil, nil, nil, "hold", nil, nil, 10, 100.0, 1000.0, nil, nil, "原始推理", 0.8, "{}", "{}", "pending", 0, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, instrument_key, symbol, name, market, exchange, asset_class, instrument_type, position_side, quote_currency, settlement_currency, margin_mode, quantity, available_qty, cost_price, current_price, market_value, weight, leverage, contract_multiplier, expiry_date, unrealized_pnl, margin_used, updated_at
			 FROM holding_positions WHERE fund_id = $1 ORDER BY instrument_key`)).
		WithArgs("fund-2").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "instrument_key", "symbol", "name", "market", "exchange", "asset_class", "instrument_type", "position_side", "quote_currency", "settlement_currency", "margin_mode", "quantity", "available_qty", "cost_price", "current_price", "market_value", "weight", "leverage", "contract_multiplier", "expiry_date", "unrealized_pnl", "margin_used", "updated_at"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, agent_id, role, focus, joined_at, status, updated_at
			 FROM fund_team_members WHERE fund_id = $1 ORDER BY joined_at`)).
		WithArgs("fund-2").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "agent_id", "role", "focus", "joined_at", "status", "updated_at"}).
			AddRow("member-1", "fund-2", "agent-risk-2", "risk", nil, now, "active", now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
			 FROM funds WHERE id = $1`)).
		WithArgs("fund-2").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-2", "company-1", "Sector Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"hk_equity","assetClass":"equity","primaryDirection":"stocks","universe":{"mode":"manual","symbols":["0700.HK"],"themes":["AI"],"sectors":["technology"]}}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, user_id, name, role, focus, llm_model, model_provider, model_name, system_prompt, skill_config, domain_config, evolution_config, pending_marketplace_snapshot, marketplace_snapshot_imported_at, status, created_at, updated_at
			 FROM agents WHERE id = $1`)).
		WithArgs("agent-risk-2").
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "name", "role", "focus", "llm_model", "model_provider", "model_name", "system_prompt", "skill_config", "domain_config", "evolution_config", "pending_marketplace_snapshot", "marketplace_snapshot_imported_at", "status", "created_at", "updated_at"}).
			AddRow("agent-risk-2", "user-1", "Risk Agent", "risk", nil, nil, nil, nil, nil, []byte(`{"enabled":true,"skills":[{"key":"risk-checklist","content":"检查港股单票流动性","match":{"roles":["risk"],"workflowSteps":["risk_review"],"scenarioKeywords":["0700.HK"]}}]}`), []byte(`{}`), []byte(`{}`), []byte(`{}`), nil, "active", now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
			 FROM funds WHERE id = $1`)).
		WithArgs("fund-2").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-2", "company-1", "Sector Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"hk_equity","assetClass":"equity","primaryDirection":"stocks","universe":{"mode":"manual","symbols":["0700.HK"],"themes":["AI"],"sectors":["technology"]}}`), now, now))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE investment_plans SET risk_review = $1, updated_at = NOW() WHERE id = $2`)).
		WithArgs(containsAllArg{"检查港股单票流动性", "市场：hk_equity", "资产类别：equity", "主要方向：stocks", "标的池代码：0700.HK", "标的池主题：AI", "标的池行业：technology", `"matchedSkills":true`}, "plan-2").
		WillReturnResult(sqlmock.NewResult(0, 1))

	risk := &runtimeRiskAgent{
		planRepo:     repository.NewPlanRepo(db),
		fundRepo:     repository.NewFundRepo(db),
		positionRepo: repository.NewPositionRepo(db),
		teamRepo:     repository.NewTeamRepo(db),
		agentRepo:    repository.NewAgentRepo(db),
	}
	approved, remarks, err := risk.ReviewPlan(context.Background(), &workflow.InvestmentPlanResult{ID: "plan-2", FundID: "fund-2"})
	if err != nil {
		t.Fatalf("review plan: %v", err)
	}
	if !approved {
		t.Fatal("expected risk review to approve plan")
	}
	for _, expected := range []string{"检查港股单票流动性", "市场：hk_equity", "资产类别：equity", "主要方向：stocks", "标的池代码：0700.HK", "标的池主题：AI", "标的池行业：technology"} {
		if !strings.Contains(remarks, expected) {
			t.Fatalf("expected %q in remarks, got %q", expected, remarks)
		}
	}

	assertMockExpectations(t, mock)
}

func TestRuntimePMAgentBuildSkillContextIncludesSpecializationContext(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	now := time.Now().UTC()
	roundtable := &workflow.RoundtableResult{Consensus: []string{"光模块主线继续强化"}}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, agent_id, role, focus, joined_at, status, updated_at
				 FROM fund_team_members WHERE fund_id = $1 ORDER BY joined_at`)).
		WithArgs("fund-pm-spec").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "agent_id", "role", "focus", "joined_at", "status", "updated_at"}).
			AddRow("member-1", "fund-pm-spec", "agent-pm-spec", "pm", nil, now, "active", now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
				 FROM funds WHERE id = $1`)).
		WithArgs("fund-pm-spec").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-pm-spec", "company-1", "Focus Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"us_equity","primaryDirection":"stocks","universe":{"symbols":["NVDA","AVGO"],"themes":["CPO"]},"specialization":{"team":{"themes":["CPO"],"instruments":["NVDA","AVGO"],"styleHints":["growth"]}}}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, user_id, name, role, focus, llm_model, model_provider, model_name, system_prompt, skill_config, domain_config, evolution_config, pending_marketplace_snapshot, marketplace_snapshot_imported_at, status, created_at, updated_at
				 FROM agents WHERE id = $1`)).
		WithArgs("agent-pm-spec").
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "name", "role", "focus", "llm_model", "model_provider", "model_name", "system_prompt", "skill_config", "domain_config", "evolution_config", "pending_marketplace_snapshot", "marketplace_snapshot_imported_at", "status", "created_at", "updated_at"}).
			AddRow("agent-pm-spec", "user-1", "PM Agent", "pm", nil, nil, nil, nil, nil, []byte(`{"enabled":true,"skills":[{"key":"pm-checklist","content":"先校验主题集中度与仓位节奏","match":{"roles":["pm"],"workflowSteps":["pm_plan"],"scenarioKeywords":["光模块"]}}]}`), []byte(`{"specialization":{"themes":["CPO"],"instruments":["NVDA"],"patterns":["concentrated growth"]}}`), []byte(`{"specializationLearning":{"themes":{"CPO":1.2},"instruments":{"NVDA":0.8},"recentLessons":["theme CPO ideas translated into stronger plan quality today"],"lastAdjustments":["keep sizing discipline on secondary names"]}}`), []byte(`{}`), nil, "active", now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
				 FROM funds WHERE id = $1`)).
		WithArgs("fund-pm-spec").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-pm-spec", "company-1", "Focus Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"us_equity","primaryDirection":"stocks","universe":{"symbols":["NVDA","AVGO"],"themes":["CPO"]},"specialization":{"team":{"themes":["CPO"],"instruments":["NVDA","AVGO"],"styleHints":["growth"]}}}`), now, now))

	agent := &runtimePMAgent{
		fundRepo:  repository.NewFundRepo(db),
		teamRepo:  repository.NewTeamRepo(db),
		agentRepo: repository.NewAgentRepo(db),
	}
	context := agent.buildSkillContext(context.Background(), "fund-pm-spec", roundtable)
	for _, expected := range []string{"先校验主题集中度与仓位节奏", "团队擅长主题：CPO", "成员擅长主题：CPO", "成员擅长标的：NVDA", "成员模式标签：concentrated growth", "近期学习优势：", "themes=CPO(+1.20)", "近期学习调整：keep sizing discipline on secondary names"} {
		if !strings.Contains(context, expected) {
			t.Fatalf("expected %q in PM specialization context, got %q", expected, context)
		}
	}

	assertMockExpectations(t, mock)
}

func TestTeamAgentMatchScorePrefersSpecializationAffinity(t *testing.T) {
	fund := &repository.Fund{Config: json.RawMessage(`{"market":"us_equity","assetClass":"equity","primaryDirection":"stocks","universe":{"symbols":["NVDA","AVGO"],"themes":["CPO"]},"specialization":{"team":{"themes":["CPO"],"instruments":["NVDA"],"styleHints":["growth"]}}}`)}
	member := repository.TeamMember{Role: "researcher", Status: "active"}
	agentA := &repository.Agent{
		Status:          "active",
		DomainConfig:    json.RawMessage(`{"coverage":{"markets":["us_equity"],"assetClasses":["equity"],"directions":["stocks"]},"specialization":{"themes":["CPO"],"instruments":["NVDA"],"styleHints":["growth"]}}`),
		EvolutionConfig: json.RawMessage(`{"specializationLearning":{"themes":{"CPO":1.1},"instruments":{"NVDA":0.9}}}`),
	}
	agentB := &repository.Agent{
		Status:       "active",
		DomainConfig: json.RawMessage(`{"coverage":{"markets":["us_equity"],"assetClasses":["equity"],"directions":["stocks"]},"specialization":{"themes":["banks"],"instruments":["JPM"]}}`),
	}
	scoreA, okA := teamAgentMatchScore(member, agentA, "stock", fund)
	scoreB, okB := teamAgentMatchScore(member, agentB, "stock", fund)
	if !okA || !okB {
		t.Fatalf("expected both agents to match coverage, got okA=%v okB=%v", okA, okB)
	}
	if scoreA <= scoreB {
		t.Fatalf("expected specialization-aligned agent to score higher, got %d <= %d", scoreA, scoreB)
	}
}

func TestApplyLearningToEvolutionConfigPreservesExistingFieldsAndAddsSpecializationLearning(t *testing.T) {
	updated, err := applyLearningToEvolutionConfig(map[string]any{
		"dailyLearningEnabled": true,
		"specializationLearning": map[string]any{
			"themes": map[string]any{"CPO": 0.5},
		},
	}, learningResult{
		Summary:     "new summary",
		Lessons:     []string{"theme CPO ideas translated into stronger plan quality today"},
		Adjustments: []string{"keep sizing discipline on secondary names"},
		Tags:        []string{"self_learning", "pm", "specialization"},
		Specialization: &specializationLearningSummary{
			Themes:          map[string]float64{"CPO": 0.8},
			Instruments:     map[string]float64{"NVDA": 0.4},
			RecentLessons:   []string{"theme CPO ideas translated into stronger plan quality today"},
			LastAdjustments: []string{"keep sizing discipline on secondary names"},
		},
	}, time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC), 0.02)
	if err != nil {
		t.Fatalf("apply learning to evolution config: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(updated, &payload); err != nil {
		t.Fatalf("unmarshal updated config: %v", err)
	}
	if payload["dailyLearningEnabled"] != true {
		t.Fatalf("expected existing flag to be preserved, got %#v", payload["dailyLearningEnabled"])
	}
	specializationLearning, ok := payload["specializationLearning"].(map[string]any)
	if !ok {
		t.Fatalf("expected specializationLearning map, got %#v", payload["specializationLearning"])
	}
	themes, ok := specializationLearning["themes"].(map[string]any)
	if !ok {
		t.Fatalf("expected themes map, got %#v", specializationLearning["themes"])
	}
	if themes["CPO"] != 1.3 {
		t.Fatalf("expected merged CPO score 1.3, got %#v", themes["CPO"])
	}
	if _, ok := specializationLearning["instruments"].(map[string]any)["NVDA"]; !ok {
		t.Fatalf("expected instruments map to include NVDA, got %#v", specializationLearning["instruments"])
	}
	if payload["lastLearningSummary"] != "new summary" {
		t.Fatalf("expected lastLearningSummary to be updated, got %#v", payload["lastLearningSummary"])
	}
}

func TestRuntimeMemorySystemBuildLearningContextUsesRequestedTradingDate(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	targetDate := time.Date(2026, time.May, 13, 0, 0, 0, 0, time.UTC)
	otherDate := time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)
	now := time.Now().UTC()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"us_equity","exchange":"NASDAQ"}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at
		 FROM workflow_runs
		 WHERE fund_id = $1 AND trading_date = $2
		 ORDER BY created_at DESC
		 LIMIT 1`)).
		WithArgs("fund-1", targetDate).
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "current_step", "step_results", "started_at", "completed_at", "created_at"}).
			AddRow("run-1", "fund-1", targetDate, "completed", sql.NullString{String: "daily_review", Valid: true}, []byte(`{}`), sql.NullTime{}, sql.NullTime{}, now))
	positionsJSON := []byte(`[{"fundId":"fund-1","instrumentKey":"NVDA","symbol":"NVDA","quantity":10,"availableQty":10,"costPrice":100,"currentPrice":110,"marketValue":1100,"weight":0.11,"updatedAt":"2026-05-13T16:00:00Z"}]`)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, nav, total_assets, total_market_value, available_cash, daily_return, total_return, positions_snapshot, created_at
		 FROM nav_snapshots WHERE fund_id = $1 AND trading_date = $2
		 LIMIT 1`)).
		WithArgs("fund-1", targetDate).
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "nav", "total_assets", "total_market_value", "available_cash", "daily_return", "total_return", "positions_snapshot", "created_at"}).
			AddRow("nav-1", "fund-1", targetDate, 1.05, 105000.0, 1100.0, 103900.0, 0.02, 0.05, positionsJSON, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, reasoning, risk_score, expected_return, risk_review, discussion_snapshot, roundtable_id, pm_agent_id, confidence, created_at, updated_at
		 FROM investment_plans WHERE fund_id = $1
		 ORDER BY trading_date DESC, created_at DESC LIMIT $2 OFFSET $3`)).
		WithArgs("fund-1", 20, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "reasoning", "risk_score", "expected_return", "risk_review", "discussion_snapshot", "roundtable_id", "pm_agent_id", "confidence", "created_at", "updated_at"}).
			AddRow("plan-2", "fund-1", otherDate, "approved", sql.NullString{}, sql.NullFloat64{}, sql.NullFloat64{}, []byte(`{}`), []byte(`{}`), sql.NullString{}, sql.NullString{}, sql.NullFloat64{}, now.Add(2*time.Hour), now.Add(2*time.Hour)).
			AddRow("plan-1", "fund-1", targetDate, "approved", sql.NullString{String: "target plan", Valid: true}, sql.NullFloat64{}, sql.NullFloat64{}, []byte(`{}`), []byte(`{}`), sql.NullString{}, sql.NullString{}, sql.NullFloat64{}, now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, plan_id, instrument_key, symbol, market, exchange, asset_class, instrument_type, action, position_side, open_close, quantity, price, amount, stop_loss, take_profit, reasoning, confidence, supported_by, opposed_by, execution_status, sort_order, quote_currency, settlement_currency, margin_mode, leverage, contract_multiplier, expiry_date, reduce_only, quote_refreshed_at, auto_executed_at, sleeve, regime_tag, signal_source, exit_reason, strategy
		 FROM plan_actions WHERE plan_id = $1 ORDER BY sort_order, id`)).
		WithArgs("plan-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "plan_id", "instrument_key", "symbol", "market", "exchange", "asset_class", "instrument_type", "action", "position_side", "open_close", "quantity", "price", "amount", "stop_loss", "take_profit", "reasoning", "confidence", "supported_by", "opposed_by", "execution_status", "sort_order", "quote_currency", "settlement_currency", "margin_mode", "leverage", "contract_multiplier", "expiry_date", "reduce_only", "quote_refreshed_at", "auto_executed_at", "sleeve", "regime_tag", "signal_source", "exit_reason", "strategy"}).
			AddRow("action-1", "plan-1", "NVDA", "NVDA", sql.NullString{}, sql.NullString{}, sql.NullString{}, sql.NullString{}, "buy", sql.NullString{}, sql.NullString{}, sql.NullFloat64{Float64: 10, Valid: true}, sql.NullFloat64{Float64: 100, Valid: true}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullString{}, sql.NullFloat64{}, pq.Array([]string{}), pq.Array([]string{}), "pending", 1, sql.NullString{}, sql.NullString{}, sql.NullString{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullTime{}, sql.NullBool{}, sql.NullTime{}, sql.NullTime{}, sql.NullString{}, sql.NullString{}, sql.NullString{}, sql.NullString{}, sql.NullString{}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT 
	id, fund_id, plan_id, plan_action_id, instrument_key, symbol,
	market, exchange, asset_class, instrument_type, side, position_side,
	open_close, order_type, quantity, price, amount, filled_qty,
	filled_price, fee_commission, fee_stamp_tax, fee_transfer,
	trading_mode, broker_order_id, mcp_server_id, status, executed_at,
	quote_currency, settlement_currency, margin_mode, leverage,
	contract_multiplier, expiry_date, reduce_only, slippage_pct,
	stop_price, trail_amount, trail_percent, display_qty,
	time_in_force, good_till_date, parent_trade_id,
	client_idempotency_key, created_at,
	cancelled_at, cancel_reason, replaced_at, replace_count
		 FROM trade_executions
		 WHERE plan_id = $1
		 ORDER BY created_at DESC, id DESC`)).
		WithArgs("plan-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "plan_id", "plan_action_id", "instrument_key", "symbol", "market", "exchange", "asset_class", "instrument_type", "side", "position_side", "open_close", "order_type", "quantity", "price", "amount", "filled_qty", "filled_price", "fee_commission", "fee_stamp_tax", "fee_transfer", "trading_mode", "broker_order_id", "mcp_server_id", "status", "executed_at", "quote_currency", "settlement_currency", "margin_mode", "leverage", "contract_multiplier", "expiry_date", "reduce_only", "slippage_pct", "stop_price", "trail_amount", "trail_percent", "display_qty", "time_in_force", "good_till_date", "parent_trade_id", "client_idempotency_key", "created_at", "cancelled_at", "cancel_reason", "replaced_at", "replace_count"}).
			AddRow("trade-1", "fund-1", sql.NullString{String: "plan-1", Valid: true}, sql.NullString{String: "action-1", Valid: true}, "NVDA", "NVDA", sql.NullString{}, sql.NullString{}, sql.NullString{}, sql.NullString{}, "buy", sql.NullString{}, sql.NullString{}, "limit", 10.0, sql.NullFloat64{Float64: 100, Valid: true}, sql.NullFloat64{}, 10.0, sql.NullFloat64{Float64: 100, Valid: true}, 0.0, 0.0, 0.0, "simulation", sql.NullString{}, sql.NullString{}, "filled", sql.NullTime{Time: targetDate.Add(15 * time.Hour), Valid: true}, sql.NullString{}, sql.NullString{}, sql.NullString{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullTime{}, sql.NullBool{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullString{}, sql.NullTime{}, sql.NullString{}, sql.NullString{}, now, sql.NullTime{}, sql.NullString{}, sql.NullTime{}, 0))

	system := &runtimeMemorySystem{
		fundRepo:     repository.NewFundRepo(db),
		planRepo:     repository.NewPlanRepo(db),
		tradeRepo:    repository.NewTradeRepo(db),
		positionRepo: repository.NewPositionRepo(db),
		navRepo:      repository.NewNavSnapshotRepo(db),
		workflowRepo: repository.NewWorkflowRunRepo(db),
	}

	ctx, err := system.buildLearningContext(context.Background(), "fund-1", targetDate)
	if err != nil {
		t.Fatalf("build learning context: %v", err)
	}
	if ctx.workflowRun == nil || !sameTradingDate(ctx.workflowRun.TradingDate, targetDate) {
		t.Fatalf("expected workflow run for target date, got %#v", ctx.workflowRun)
	}
	if ctx.nav == nil || !sameTradingDate(ctx.nav.TradingDate, targetDate) {
		t.Fatalf("expected nav snapshot for target date, got %#v", ctx.nav)
	}
	if ctx.plan == nil || ctx.plan.ID != "plan-1" {
		t.Fatalf("expected target-date plan, got %#v", ctx.plan)
	}
	if len(ctx.trades) != 1 || !ctx.trades[0].PlanID.Valid || ctx.trades[0].PlanID.String != "plan-1" {
		t.Fatalf("expected trades from target plan, got %#v", ctx.trades)
	}
	if len(ctx.positions) != 1 || ctx.positions[0].Symbol != "NVDA" {
		t.Fatalf("expected positions from target nav snapshot, got %#v", ctx.positions)
	}

	assertMockExpectations(t, mock)
}

func TestValidateConfigRejectsProductionFallbackDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cfg := &Config{
		Env:                     "production",
		DatabaseURL:             legacyDatabaseURLFallback(),
		JWTSecret:               strings.Repeat("a", 32),
		ModelConfigAPIKeySecret: strings.Repeat("b", 32),
		CORSOrigins:             []string{"https://app.example.com"},
	}

	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL must be explicitly set") {
		t.Fatalf("expected explicit DATABASE_URL error, got %v", err)
	}
}

func TestValidateConfigRejectsProductionDemoDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://fundai:fundai_secret_change_me@db.example.com:5432/fundai?sslmode=require")
	cfg := &Config{
		Env:                     "production",
		DatabaseURL:             os.Getenv("DATABASE_URL"),
		JWTSecret:               strings.Repeat("a", 32),
		ModelConfigAPIKeySecret: strings.Repeat("b", 32),
		CORSOrigins:             []string{"https://app.example.com"},
	}

	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "placeholder or demo credentials") {
		t.Fatalf("expected placeholder DATABASE_URL error, got %v", err)
	}
}

func TestValidateConfigRejectsProductionWithoutTLSDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://user:pass@db.example.com:5432/fundai?sslmode=disable")
	cfg := &Config{
		Env:                     "production",
		DatabaseURL:             os.Getenv("DATABASE_URL"),
		JWTSecret:               strings.Repeat("a", 32),
		ModelConfigAPIKeySecret: strings.Repeat("b", 32),
		CORSOrigins:             []string{"https://app.example.com"},
	}

	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "sslmode=disable") {
		t.Fatalf("expected TLS enforcement error, got %v", err)
	}
}

func TestValidateConfigAcceptsProductionInternalComposeDatabaseURLWhenExplicitlyAllowed(t *testing.T) {
	t.Setenv("RUNNING_IN_CONTAINER", "1")
	t.Setenv("ALLOW_INTERNAL_COMPOSE_DB", "1")
	t.Setenv("DATABASE_URL", "postgres://prod_user:strong_prod_password@postgres:5432/fundai?sslmode=disable")
	cfg := &Config{
		Env:                     "production",
		DatabaseURL:             os.Getenv("DATABASE_URL"),
		JWTSecret:               strings.Repeat("a", 32),
		ModelConfigAPIKeySecret: strings.Repeat("b", 32),
		CORSOrigins:             []string{"https://app.example.com"},
	}

	if err := validateConfig(cfg); err != nil {
		t.Fatalf("expected internal compose database URL to be valid when explicitly allowed, got %v", err)
	}
}

func TestValidateConfigRejectsProductionInternalComposeDatabaseURLWithoutExplicitAllow(t *testing.T) {
	t.Setenv("RUNNING_IN_CONTAINER", "1")
	t.Setenv("ALLOW_INTERNAL_COMPOSE_DB", "")
	t.Setenv("DATABASE_URL", "postgres://prod_user:strong_prod_password@postgres:5432/fundai?sslmode=disable")
	cfg := &Config{
		Env:                     "production",
		DatabaseURL:             os.Getenv("DATABASE_URL"),
		JWTSecret:               strings.Repeat("a", 32),
		ModelConfigAPIKeySecret: strings.Repeat("b", 32),
		CORSOrigins:             []string{"https://app.example.com"},
	}

	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "ALLOW_INTERNAL_COMPOSE_DB=1") {
		t.Fatalf("expected explicit internal compose allow error, got %v", err)
	}
}

func TestValidateConfigRejectsProductionExternalDatabaseWithoutTLSEvenWhenInternalComposeAllowed(t *testing.T) {
	t.Setenv("RUNNING_IN_CONTAINER", "1")
	t.Setenv("ALLOW_INTERNAL_COMPOSE_DB", "1")
	t.Setenv("DATABASE_URL", "postgres://prod_user:strong_prod_password@db.example.com:5432/fundai?sslmode=disable")
	cfg := &Config{
		Env:                     "production",
		DatabaseURL:             os.Getenv("DATABASE_URL"),
		JWTSecret:               strings.Repeat("a", 32),
		ModelConfigAPIKeySecret: strings.Repeat("b", 32),
		CORSOrigins:             []string{"https://app.example.com"},
	}

	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "sslmode=disable") {
		t.Fatalf("expected TLS enforcement for external production database, got %v", err)
	}
}

func TestValidateConfigRejectsSharedSecretsInProduction(t *testing.T) {
	secret := strings.Repeat("a", 32)
	t.Setenv("DATABASE_URL", "postgres://user:pass@db.example.com:5432/fundai?sslmode=require")
	cfg := &Config{
		Env:                     "production",
		DatabaseURL:             os.Getenv("DATABASE_URL"),
		JWTSecret:               secret,
		ModelConfigAPIKeySecret: secret,
		CORSOrigins:             []string{"https://app.example.com"},
	}

	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "must differ from JWT_SECRET") {
		t.Fatalf("expected separate secrets error, got %v", err)
	}
}

func TestValidateConfigAcceptsProductionSafeConfig(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://user:pass@db.example.com:5432/fundai?sslmode=require")
	cfg := &Config{
		Env:                     "production",
		DatabaseURL:             os.Getenv("DATABASE_URL"),
		JWTSecret:               strings.Repeat("a", 32),
		ModelConfigAPIKeySecret: strings.Repeat("b", 32),
		CORSOrigins:             []string{"https://app.example.com"},
	}

	if err := validateConfig(cfg); err != nil {
		t.Fatalf("expected valid production config, got %v", err)
	}
}

func TestLoadConfigReadsEnvDrivenLLMDefaults(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "claude")
	t.Setenv("CLAUDE_MODEL", "claude-sonnet-4-20250514")
	t.Setenv("ANTHROPIC_API_KEY", "anth-key")
	t.Setenv("ANTHROPIC_BASE_URL", "https://anthropic.example/v1")
	t.Setenv("LLM_STANDARD_PROVIDER", "openai")
	t.Setenv("OPENAI_MODEL", "gpt-4o-mini")
	t.Setenv("OPENAI_API_KEY", "openai-key")
	t.Setenv("OPENAI_BASE_URL", "https://openai.example/v1")
	t.Setenv("LLM_SIMPLE_MODEL", "gpt-4o-mini")

	cfg := LoadConfig()
	if cfg.LLMDefaults.Global.Provider != "claude" {
		t.Fatalf("expected global provider claude, got %q", cfg.LLMDefaults.Global.Provider)
	}
	if cfg.LLMDefaults.Global.Model != "claude-sonnet-4-20250514" {
		t.Fatalf("expected global claude model, got %q", cfg.LLMDefaults.Global.Model)
	}
	if cfg.LLMDefaults.Global.APIKey != "anth-key" {
		t.Fatalf("expected global anthropic key, got %q", cfg.LLMDefaults.Global.APIKey)
	}
	if cfg.LLMDefaults.Standard.Provider != "openai" || cfg.LLMDefaults.Standard.Model != "gpt-4o-mini" {
		t.Fatalf("expected standard tier to use openai mini, got %#v", cfg.LLMDefaults.Standard)
	}
	if cfg.LLMDefaults.Critical.Provider != "claude" || cfg.LLMDefaults.Critical.Model != "claude-sonnet-4-20250514" {
		t.Fatalf("expected critical tier to inherit global claude defaults, got %#v", cfg.LLMDefaults.Critical)
	}
}

func TestLoadConfigReadsGeminiDefaultsFromEnv(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "gemini")
	t.Setenv("GEMINI_MODEL", "gemini-3.1-pro-preview")
	t.Setenv("GEMINI_API_KEY", "gem-key")
	t.Setenv("GEMINI_BASE_URL", "https://gemini.example/v1beta")
	t.Setenv("LLM_SIMPLE_PROVIDER", "google")

	cfg := LoadConfig()
	if cfg.LLMDefaults.Global.Provider != "gemini" {
		t.Fatalf("expected global provider gemini, got %q", cfg.LLMDefaults.Global.Provider)
	}
	if cfg.LLMDefaults.Global.Model != "gemini-3.1-pro-preview" {
		t.Fatalf("expected global gemini model, got %q", cfg.LLMDefaults.Global.Model)
	}
	if cfg.LLMDefaults.Global.APIKey != "gem-key" {
		t.Fatalf("expected global gemini key, got %q", cfg.LLMDefaults.Global.APIKey)
	}
	if cfg.LLMDefaults.Global.BaseURL != "https://gemini.example/v1beta" {
		t.Fatalf("expected global gemini base url, got %q", cfg.LLMDefaults.Global.BaseURL)
	}
	if cfg.LLMDefaults.Simple.Provider != "gemini" {
		t.Fatalf("expected simple tier google alias to normalize to gemini, got %#v", cfg.LLMDefaults.Simple)
	}
}

func TestBuildPlatformDefaultModelsChoosesProviderFallbackModel(t *testing.T) {
	defaults := LLMDefaultsConfig{
		Global:   LLMEnvModelConfig{Provider: "openai"},
		Critical: LLMEnvModelConfig{Provider: "openai"},
		Standard: LLMEnvModelConfig{Provider: "openai"},
		Simple:   LLMEnvModelConfig{Provider: "openai"},
	}

	models := buildPlatformDefaultModels(defaults)
	if models[llm.TierStandard].Provider != llm.ProviderOpenAI {
		t.Fatalf("expected standard provider openai, got %s", models[llm.TierStandard].Provider)
	}
	if models[llm.TierStandard].ModelName != "gpt-4o-mini" {
		t.Fatalf("expected standard openai fallback model gpt-4o-mini, got %q", models[llm.TierStandard].ModelName)
	}
}

func TestBuildPlatformDefaultModelsChoosesGeminiFallbackModel(t *testing.T) {
	defaults := LLMDefaultsConfig{
		Global:   LLMEnvModelConfig{Provider: "gemini"},
		Critical: LLMEnvModelConfig{Provider: "gemini"},
		Standard: LLMEnvModelConfig{Provider: "gemini"},
		Simple:   LLMEnvModelConfig{Provider: "gemini"},
	}

	models := buildPlatformDefaultModels(defaults)
	if models[llm.TierStandard].Provider != llm.ProviderGemini {
		t.Fatalf("expected standard provider gemini, got %s", models[llm.TierStandard].Provider)
	}
	if models[llm.TierStandard].ModelName != "gemini-3.1-pro-preview" {
		t.Fatalf("expected standard gemini fallback model, got %q", models[llm.TierStandard].ModelName)
	}
	if models[llm.TierStandard].BaseURL != "https://generativelanguage.googleapis.com/v1beta" {
		t.Fatalf("expected standard gemini base url, got %q", models[llm.TierStandard].BaseURL)
	}
}

func TestLoadConfigReadsMarketProviderChains(t *testing.T) {
	t.Setenv("MARKETDATA_QUOTE_PROVIDERS", "quantdinger,china-stock,akshare")
	t.Setenv("MARKETDATA_NEWS_PROVIDERS", "local-search,tavily,web-search")
	t.Setenv("SERPAPI_KEYS", "key-1,key-2")
	t.Setenv("TAVILY_API_KEYS", "tav-1")
	t.Setenv("SERPAPI_BASE_URL", "https://serpapi.example")
	t.Setenv("TAVILY_BASE_URL", "https://tavily.example")

	cfg := LoadConfig()
	if strings.Join(cfg.MarketData.QuoteProviders, ",") != "quantdinger,china-stock,akshare" {
		t.Fatalf("unexpected quote providers: %#v", cfg.MarketData.QuoteProviders)
	}
	if strings.Join(cfg.MarketData.NewsProviders, ",") != "local-search,tavily,web-search" {
		t.Fatalf("unexpected news providers: %#v", cfg.MarketData.NewsProviders)
	}
	if len(cfg.MarketData.SerpAPIKeys) != 2 || cfg.MarketData.SerpAPIKeys[1] != "key-2" {
		t.Fatalf("unexpected serpapi keys: %#v", cfg.MarketData.SerpAPIKeys)
	}
	if len(cfg.MarketData.TavilyAPIKeys) != 1 || cfg.MarketData.TavilyAPIKeys[0] != "tav-1" {
		t.Fatalf("unexpected tavily keys: %#v", cfg.MarketData.TavilyAPIKeys)
	}
	if cfg.MarketData.SerpAPIBaseURL != "https://serpapi.example" {
		t.Fatalf("unexpected serpapi base url: %#v", cfg.MarketData.SerpAPIBaseURL)
	}
	if cfg.MarketData.TavilyBaseURL != "https://tavily.example" {
		t.Fatalf("unexpected tavily base url: %#v", cfg.MarketData.TavilyBaseURL)
	}
}

func TestRequestLoggerAddsTraceHeadersAndMetrics(t *testing.T) {
	metrics := newServerMetrics()
	handler := requestLogger(metrics, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := requestIDFromContext(r.Context()); got != "req-fixed" {
			t.Fatalf("expected request id in context, got %q", got)
		}
		if got := traceIDFromContext(r.Context()); got != "trace-fixed" {
			t.Fatalf("expected trace id in context, got %q", got)
		}
		if got := spanIDFromContext(r.Context()); got == "" {
			t.Fatal("expected span id in context")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	req.Header.Set(requestIDHeader, "req-fixed")
	req.Header.Set(traceIDHeader, "trace-fixed")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if got := rr.Header().Get(requestIDHeader); got != "req-fixed" {
		t.Fatalf("expected request id header %q, got %q", "req-fixed", got)
	}
	if got := rr.Header().Get(traceIDHeader); got != "trace-fixed" {
		t.Fatalf("expected trace id header %q, got %q", "trace-fixed", got)
	}
	if got := rr.Header().Get(spanIDHeader); got == "" {
		t.Fatal("expected span id header")
	}

	output := metrics.ExportPrometheus()
	if !bytes.Contains([]byte(output), []byte("fundai_http_requests_total{method=\"GET\",path=\"/api/version\",status=\"201\"} 1")) {
		t.Fatalf("expected metrics export to include request counter, got %s", output)
	}
	if !bytes.Contains([]byte(output), []byte("fundai_http_request_duration_seconds_bucket{method=\"GET\",path=\"/api/version\",status=\"201\",le=\"+Inf\"} 1")) {
		t.Fatalf("expected metrics export to include request duration histogram, got %s", output)
	}
}

func TestResponseRecorderIgnoresDuplicateWriteHeader(t *testing.T) {
	rr := httptest.NewRecorder()
	recorder := &responseRecorder{ResponseWriter: rr}

	recorder.WriteHeader(http.StatusAccepted)
	recorder.WriteHeader(http.StatusInternalServerError)
	_, _ = recorder.Write([]byte("ok"))

	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d", http.StatusAccepted, rr.Code)
	}
	if recorder.status != http.StatusAccepted {
		t.Fatalf("expected recorder status %d, got %d", http.StatusAccepted, recorder.status)
	}
}

// flushTrackingWriter records every Flush() call so we can assert the
// responseRecorder delegates instead of swallowing the call. Implements
// http.Flusher but NOT http.Hijacker, mirroring the typical stdlib
// response writer behaviour for non-upgrade requests.
type flushTrackingWriter struct {
	http.ResponseWriter
	flushes int
}

func (w *flushTrackingWriter) Flush() { w.flushes++ }

// TestResponseRecorderExposesFlusher is the regression test for the
// "团队实时活动 / Team Live Activity SSE 一直 reconnect" bug. The handler
// does `w.(http.Flusher)`; the wrapper used to swallow that interface,
// the handler short-circuited with 500 "sse unsupported", and the
// browser entered an exponential backoff loop. Adding Flush() to the
// recorder MUST keep the type assertion succeeding *and* forward each
// call so events actually leave the box.
func TestResponseRecorderExposesFlusher(t *testing.T) {
	tracker := &flushTrackingWriter{ResponseWriter: httptest.NewRecorder()}
	recorder := &responseRecorder{ResponseWriter: tracker}

	flusher, ok := http.ResponseWriter(recorder).(http.Flusher)
	if !ok {
		t.Fatal("responseRecorder must satisfy http.Flusher so SSE handlers can stream")
	}
	flusher.Flush()
	flusher.Flush()
	if tracker.flushes != 2 {
		t.Fatalf("expected 2 forwarded flushes, got %d", tracker.flushes)
	}
	// Flush() also marks the status as 200 if the handler hasn't set one
	// explicitly. SSE handlers do call WriteHeader(200) themselves first,
	// but defensively we still want the request log to show 200 instead
	// of 0 when a handler flushes before WriteHeader fires.
	if recorder.status != http.StatusOK {
		t.Fatalf("expected Flush() to imply 200, got %d", recorder.status)
	}
}

// hijackTrackingWriter is a stand-in for the real net/http response
// writer's Hijacker capability. We don't actually need a usable
// net.Conn — just to assert the wrapper forwards the call.
type hijackTrackingWriter struct {
	http.ResponseWriter
	called bool
}

func (w *hijackTrackingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.called = true
	// Return a sentinel error: the test only cares that the wrapper
	// surfaced the Hijacker interface and routed the call here.
	return nil, nil, errors.New("hijack stub")
}

// TestResponseRecorderExposesHijacker proves the same pass-through
// principle for connection-upgrade handlers (WebSocket today, anything
// using http.ResponseController tomorrow). Without this, a future
// /ws/* endpoint silently 500s with "response writer does not
// implement http.Hijacker" the same way SSE did.
func TestResponseRecorderExposesHijacker(t *testing.T) {
	tracker := &hijackTrackingWriter{ResponseWriter: httptest.NewRecorder()}
	recorder := &responseRecorder{ResponseWriter: tracker}

	hijacker, ok := http.ResponseWriter(recorder).(http.Hijacker)
	if !ok {
		t.Fatal("responseRecorder must satisfy http.Hijacker so upgrade handlers can take over the conn")
	}
	if _, _, err := hijacker.Hijack(); err == nil || err.Error() != "hijack stub" {
		t.Fatalf("expected hijack stub error to propagate, got %v", err)
	}
	if !tracker.called {
		t.Fatal("expected Hijack() call to be forwarded to the underlying writer")
	}
	if recorder.status != http.StatusSwitchingProtocols {
		t.Fatalf("expected Hijack() to imply 101, got %d", recorder.status)
	}
}

// hijackOnlyWriter implements Hijacker but NOT Flusher. The
// responseRecorder.Flush() implementation must be defensive about
// this and become a no-op instead of panicking — production traffic
// hitting `/api/health` and friends never flushes, so Flush is only
// ever called by the SSE handler, but defensive coding belongs here.
type hijackOnlyWriter struct {
	http.ResponseWriter
}

func (w *hijackOnlyWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("unused")
}

func TestResponseRecorderFlushIsNoOpWhenUnderlyingWriterDoesNotFlush(t *testing.T) {
	recorder := &responseRecorder{ResponseWriter: &hijackOnlyWriter{ResponseWriter: httptest.NewRecorder()}}
	// Should not panic even though the underlying writer has no Flush().
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("Flush() panicked when underlying writer was not a Flusher: %v", rec)
		}
	}()
	recorder.Flush()
}

// TestRequestLoggerPreservesSSEFlushing is the end-to-end regression:
// it boots an httptest.Server with the production middleware chain
// (recoverer → requestLogger → mux), registers a tiny SSE-style
// handler that asserts `w.(http.Flusher)`, and reads bytes off the
// wire to confirm the chunk arrived. This is the exact failure mode
// that produced status 500 "sse unsupported" in the prod logs at
// /api/funds/.../team/activity/stream.
func TestRequestLoggerPreservesSSEFlushing(t *testing.T) {
	flushed := make(chan struct{}, 1)
	sseHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, ": connected\n\n"); err != nil {
			t.Errorf("write ': connected': %v", err)
			return
		}
		flusher.Flush()
		if _, err := io.WriteString(w, "event: ping\ndata: hello\n\n"); err != nil {
			t.Errorf("write event: %v", err)
			return
		}
		flusher.Flush()
		select {
		case flushed <- struct{}{}:
		default:
		}
	})

	metrics := newServerMetrics()
	server := httptest.NewServer(recoverer(metrics, requestLogger(metrics, sseHandler)))
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/sse-probe", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK from SSE probe (this is the original bug — wrapper hid Flusher), got %d body=%q",
			resp.StatusCode, string(body))
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("expected text/event-stream content-type, got %q", got)
	}

	buf := make([]byte, 256)
	n, err := resp.Body.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read SSE bytes: %v", err)
	}
	if n == 0 || !strings.Contains(string(buf[:n]), "event: ping") {
		t.Fatalf("expected event:ping frame in first read, got %q", string(buf[:n]))
	}

	select {
	case <-flushed:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never reached the post-flush signal — Flush() likely silently dropped data")
	}
}

func TestHandleMetricsExportsPrometheus(t *testing.T) {
	metrics := newServerMetrics()
	metrics.ObserveHTTP(http.MethodGet, "/api/health", http.StatusOK, 25*time.Millisecond)
	handler := handleMetrics(&Services{Metrics: metrics})
	req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("unexpected content type: %q", got)
	}
	body := rr.Body.String()
	if !bytes.Contains([]byte(body), []byte("fundai_http_requests_total")) {
		t.Fatalf("expected metrics body to include http metric, got %s", body)
	}
	if !bytes.Contains([]byte(body), []byte("fundai_http_request_duration_seconds")) {
		t.Fatalf("expected metrics body to include http duration histogram, got %s", body)
	}
}

func TestHandleMetricsReturnsServiceUnavailableWhenUnavailable(t *testing.T) {
	cases := []struct {
		name string
		svc  *Services
	}{
		{name: "nil services", svc: nil},
		{name: "nil metrics", svc: &Services{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := handleMetrics(tc.svc)
			req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected status %d, got %d", http.StatusServiceUnavailable, rr.Code)
			}
			if body := rr.Body.String(); body != "metrics unavailable\n" {
				t.Fatalf("unexpected response body: %q", body)
			}
		})
	}
}

func TestRecovererRecordsPanicMetric(t *testing.T) {
	metrics := newServerMetrics()
	handler := recoverer(metrics, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/panic", nil)
	req.Header.Set(requestIDHeader, "req-panic")
	req.Header.Set(traceIDHeader, "trace-panic")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d", http.StatusInternalServerError, rr.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal panic response: %v", err)
	}
	if payload["request_id"] != "req-panic" {
		t.Fatalf("expected request id %q, got %#v", "req-panic", payload["request_id"])
	}
	if payload["trace_id"] != "trace-panic" {
		t.Fatalf("expected trace id %q, got %#v", "trace-panic", payload["trace_id"])
	}
	output := metrics.ExportPrometheus()
	if !bytes.Contains([]byte(output), []byte("fundai_http_panics_total{path=\"/api/panic\"} 1")) {
		t.Fatalf("expected panic metric in export, got %s", output)
	}
}

func TestMetricsExportIncludesHardRiskRejections(t *testing.T) {
	metrics := newServerMetrics()
	metrics.RecordHardRiskRejection("hard_max_order_notional", "AAPL")
	output := metrics.ExportPrometheus()
	if !bytes.Contains([]byte(output), []byte("fundai_hard_risk_rejections_total{rule=\"hard_max_order_notional\",symbol=\"AAPL\"} 1")) {
		t.Fatalf("expected hard risk rejection metric, got %s", output)
	}
}

// Sprint D #1: ObserveDecisionInput recorder + Prometheus
// exposition. Each label set must show up exactly once and
// match the counter values, and the helper-line header must be
// emitted so /api/metrics scrapes are valid Prometheus text.
func TestMetricsExportIncludesDecisionInputSignals(t *testing.T) {
	metrics := newServerMetrics()
	metrics.ObserveDecisionInput(
		[]string{"bullCase", "exposure"},
		[]string{"riskBudget"},
		[]string{"single_name", "cash_floor"},
		3,
		[]string{"MU", "SNDK"},
		"drawdown_throttle",
	)
	metrics.ObserveDecisionInput(
		[]string{"bullCase"},
		[]string{"riskBudget"},
		nil,
		0,
		nil,
		"",
	)
	output := metrics.ExportPrometheus()
	mustContain := []string{
		"# TYPE fundai_decision_input_calls_total counter",
		"fundai_decision_input_calls_total 2",
		"# TYPE fundai_decision_input_blocks_total counter",
		"fundai_decision_input_blocks_total{block=\"bullCase\",present=\"true\"} 2",
		"fundai_decision_input_blocks_total{block=\"exposure\",present=\"true\"} 1",
		"fundai_decision_input_blocks_total{block=\"riskBudget\",present=\"false\"} 2",
		"# TYPE fundai_decision_exposure_breaches_total counter",
		"fundai_decision_exposure_breaches_total{kind=\"single_name\"} 1",
		"fundai_decision_exposure_breaches_total{kind=\"cash_floor\"} 1",
		"# TYPE fundai_decision_correlation_high_pairs_total counter",
		"fundai_decision_correlation_high_pairs_total 3",
		"# TYPE fundai_decision_cooldown_vetos_total counter",
		"fundai_decision_cooldown_vetos_total{symbol=\"MU\"} 1",
		"fundai_decision_cooldown_vetos_total{symbol=\"SNDK\"} 1",
		"# TYPE fundai_decision_risk_budget_throttled_total counter",
		"fundai_decision_risk_budget_throttled_total{reason=\"drawdown_throttle\"} 1",
	}
	for _, want := range mustContain {
		if !bytes.Contains([]byte(output), []byte(want)) {
			t.Errorf("ExportPrometheus missing line: %q", want)
		}
	}
}

// TestServerMetrics_RecordCorpActionExports pins the Card-G
// metrics surface end-to-end: the Record* methods feed counters
// and gauges that ExportPrometheus stamps with stable names. If
// any of these names change, the operator's PromQL alerts in
// docs/PROMETHEUS_QUERIES.md silently break — this test catches
// that at compile/CI time.
func TestServerMetrics_RecordCorpActionExports(t *testing.T) {
	metrics := newServerMetrics()
	// Drive every Record* method at least once with a couple of
	// distinct labels so the export contains both the label-cardinality
	// shape and the canonical label values.
	metrics.RecordCorpActionTick("ok")
	metrics.RecordCorpActionTick("skipped_not_leader")
	metrics.RecordCorpActionProviderError("a_share", "transient")
	metrics.RecordCorpActionProviderError("us_equity", "fatal")
	metrics.RecordCorpActionRetry("a_share", "succeeded")
	metrics.RecordCorpActionRetry("a_share", "exhausted")
	metrics.RecordCorpActionEvent("split", "upserted")
	metrics.RecordCorpActionEvent("cash_dividend", "upsert_error")
	metrics.RecordCorpActionApply("applied")
	metrics.RecordCorpActionApply("missing")
	metrics.RecordCorpActionApply("error")

	output := metrics.ExportPrometheus()
	mustContain := []string{
		"# TYPE fundai_corp_action_ingest_ticks_total counter",
		"fundai_corp_action_ingest_ticks_total{status=\"ok\"} 1",
		"fundai_corp_action_ingest_ticks_total{status=\"skipped_not_leader\"} 1",
		"# TYPE fundai_corp_action_ingest_provider_errors_total counter",
		"fundai_corp_action_ingest_provider_errors_total{market=\"a_share\",outcome=\"transient\"} 1",
		"fundai_corp_action_ingest_provider_errors_total{market=\"us_equity\",outcome=\"fatal\"} 1",
		"# TYPE fundai_corp_action_ingest_retries_total counter",
		"fundai_corp_action_ingest_retries_total{market=\"a_share\",outcome=\"succeeded\"} 1",
		"fundai_corp_action_ingest_retries_total{market=\"a_share\",outcome=\"exhausted\"} 1",
		"# TYPE fundai_corp_action_ingest_events_total counter",
		"fundai_corp_action_ingest_events_total{action=\"split\",phase=\"upserted\"} 1",
		"fundai_corp_action_ingest_events_total{action=\"cash_dividend\",phase=\"upsert_error\"} 1",
		"# TYPE fundai_corp_action_ingest_apply_total counter",
		"fundai_corp_action_ingest_apply_total{outcome=\"applied\"} 1",
		"fundai_corp_action_ingest_apply_total{outcome=\"missing\"} 1",
		"fundai_corp_action_ingest_apply_total{outcome=\"error\"} 1",
		"# TYPE fundai_corp_action_ingest_last_tick_unix gauge",
		"# TYPE fundai_corp_action_ingest_last_success_unix gauge",
	}
	for _, want := range mustContain {
		if !bytes.Contains([]byte(output), []byte(want)) {
			t.Errorf("ExportPrometheus missing line: %q\n%s", want, output)
		}
	}
	// last_tick_unix should be > 0 after the Record calls; we don't
	// pin the exact value because Now() drift is allowed.
	if !bytes.Contains([]byte(output), []byte("fundai_corp_action_ingest_last_tick_unix ")) {
		t.Error("missing last_tick_unix gauge")
	}
	// last_success should also be advanced because we recorded an
	// "ok" tick.
	if bytes.Contains([]byte(output), []byte("fundai_corp_action_ingest_last_success_unix 0")) {
		t.Error("last_success_unix should be > 0 after RecordCorpActionTick(ok)")
	}
}

// TestServerMetrics_CorpActionNilSafe pins the contract that
// every Card-G recorder is no-op on a nil receiver. The
// production wiring guarantees a non-nil registry, but tests
// often build subsystems with no metrics injected and we don't
// want a stray ":" in a label string to crash a deep call path.
func TestServerMetrics_CorpActionNilSafe(t *testing.T) {
	var metrics *serverMetrics
	// no panic = pass
	metrics.RecordCorpActionTick("ok")
	metrics.RecordCorpActionProviderError("a_share", "fatal")
	metrics.RecordCorpActionRetry("a_share", "succeeded")
	metrics.RecordCorpActionEvent("split", "upserted")
	metrics.RecordCorpActionApply("applied")
}

// TestServerMetrics_RecordABShadowLLMCallExports pins K-5: every
// outcome label that the LLM-shadow decider emits must show up
// in `fundai_ab_shadow_llm_calls_total{outcome=...}` after a
// single-pass export. Operator dashboards (cost burn, fallback
// rate) parse the label set; if the names drift this test will
// fail loudly.
func TestServerMetrics_RecordABShadowLLMCallExports(t *testing.T) {
	metrics := newServerMetrics()
	for _, outcome := range []string{
		"decided_by_llm",
		"fallback_llm_error",
		"fallback_parse_error",
		"fallback_budget_cap",
		"recap_decided_by_llm",
		"recap_fallback_llm_error",
		"recap_fallback_parse_error",
	} {
		metrics.RecordABShadowLLMCall(outcome)
	}
	// Bump one of them twice so we know counters accumulate
	// rather than overwrite.
	metrics.RecordABShadowLLMCall("decided_by_llm")

	export := metrics.ExportPrometheus()
	if !strings.Contains(export, "# TYPE fundai_ab_shadow_llm_calls_total counter") {
		t.Errorf("export missing TYPE comment for fundai_ab_shadow_llm_calls_total\n%s", export)
	}
	for _, want := range []string{
		`fundai_ab_shadow_llm_calls_total{outcome="decided_by_llm"} 2`,
		`fundai_ab_shadow_llm_calls_total{outcome="fallback_llm_error"} 1`,
		`fundai_ab_shadow_llm_calls_total{outcome="fallback_parse_error"} 1`,
		`fundai_ab_shadow_llm_calls_total{outcome="fallback_budget_cap"} 1`,
		`fundai_ab_shadow_llm_calls_total{outcome="recap_decided_by_llm"} 1`,
		`fundai_ab_shadow_llm_calls_total{outcome="recap_fallback_llm_error"} 1`,
		`fundai_ab_shadow_llm_calls_total{outcome="recap_fallback_parse_error"} 1`,
	} {
		if !strings.Contains(export, want) {
			t.Errorf("export missing line %q\n--- export ---\n%s", want, export)
		}
	}
	// Empty/whitespace outcomes are coerced to "unknown" so the
	// label cardinality stays bounded.
	metrics.RecordABShadowLLMCall("   ")
	if !strings.Contains(metrics.ExportPrometheus(), `fundai_ab_shadow_llm_calls_total{outcome="unknown"}`) {
		t.Errorf("blank outcome must coerce to unknown")
	}
}

func TestServerMetrics_ABShadowLLMCallNilSafe(t *testing.T) {
	var metrics *serverMetrics
	// no panic = pass
	metrics.RecordABShadowLLMCall("decided_by_llm")
	metrics.RecordABShadowLLMCall("fallback_budget_cap")
}

// Receiver nil-guard: ObserveDecisionInput must be safe on a
// nil *serverMetrics so test wirings that omit the registry can
// still call into PM code paths.
func TestObserveDecisionInputNilSafe(t *testing.T) {
	var metrics *serverMetrics
	metrics.ObserveDecisionInput(
		[]string{"bullCase"},
		[]string{"riskBudget"},
		[]string{"single_name"},
		1,
		[]string{"MU"},
		"drawdown_throttle",
	)
	// no panic = pass
}

func BenchmarkRequestLogger(b *testing.B) {
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.Cleanup(func() { slog.SetDefault(oldLogger) })

	metrics := newServerMetrics()
	handler := requestLogger(metrics, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
		req.Header.Set(requestIDHeader, "req-fixed")
		req.Header.Set(traceIDHeader, "trace-fixed")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
	}
}

func BenchmarkMetricsExportPrometheus(b *testing.B) {
	metrics := newServerMetrics()
	for i := 0; i < 100; i++ {
		metrics.ObserveHTTP(http.MethodGet, "/api/version", http.StatusOK, time.Duration(i+1)*time.Millisecond)
		metrics.ObserveLLM("openai", "gpt-4o", "chat", "success", time.Duration(i+1)*time.Millisecond)
		metrics.ObserveWorkflow("fund-1", "running", "roundtable")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		output := metrics.ExportPrometheus()
		if len(output) == 0 {
			b.Fatal("expected metrics export")
		}
	}
}
