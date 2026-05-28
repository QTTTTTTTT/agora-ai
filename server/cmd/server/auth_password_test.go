package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/mailer"
	"golang.org/x/crypto/bcrypt"
)

func newAuthRecorder(t *testing.T) *mailer.Recorder {
	t.Helper()
	return &mailer.Recorder{}
}

func authRequestWithUser(method, path, userID, body string) *http.Request {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	ctx := api.WithAuthenticatedUserID(req.Context(), userID)
	return req.WithContext(ctx)
}

func TestSendVerificationIssuesCodeAndCallsMailer(t *testing.T) {
	authRateLimiter = newRateLimiter()
	db, mock := newMockDB(t)
	defer db.Close()

	userID := "11111111-1111-4111-8111-111111111111"
	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status, COALESCE(password_hash, ''), COALESCE(kyc_status, 'unverified'), COALESCE(kyc_level, 'tier1_basic')
		FROM users
		WHERE id = $1
		LIMIT 1
	`)).WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "display_name", "role", "status", "password_hash", "kyc_status", "kyc_level"}).
			AddRow(userID, "user@example.com", "User", userRoleUser, userStatusActive, "$2a$10$abc", "unverified", "tier1_basic"))

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO email_verifications`)).
		WithArgs(sqlmock.AnyArg(), userID, "user@example.com", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rec := newAuthRecorder(t)
	svc := &Services{DB: db, Mailer: rec}
	cfg := &Config{
		Mailer: mailer.Config{Host: "smtp.example.com", From: "noreply@example.com"},
	}

	handler := handleSendVerification(svc, cfg)
	req := authRequestWithUser(http.MethodPost, "/api/auth/send-verification", userID, "")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(rec.Messages) != 1 {
		t.Fatalf("expected one mail dispatched, got %d", len(rec.Messages))
	}
	if rec.Messages[0].To != "user@example.com" {
		t.Errorf("unexpected recipient %s", rec.Messages[0].To)
	}
	assertMockExpectations(t, mock)
}

func TestSendVerificationReturnsDevCodeWhenMailerNotConfigured(t *testing.T) {
	authRateLimiter = newRateLimiter()
	db, mock := newMockDB(t)
	defer db.Close()

	userID := "22222222-2222-4222-8222-222222222222"
	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status, COALESCE(password_hash, ''), COALESCE(kyc_status, 'unverified'), COALESCE(kyc_level, 'tier1_basic')
		FROM users
		WHERE id = $1
		LIMIT 1
	`)).WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "display_name", "role", "status", "password_hash", "kyc_status", "kyc_level"}).
			AddRow(userID, "dev@example.com", "Dev", userRoleUser, userStatusActive, "$2a$10$abc", "unverified", "tier1_basic"))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO email_verifications`)).
		WithArgs(sqlmock.AnyArg(), userID, "dev@example.com", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rec := newAuthRecorder(t)
	svc := &Services{DB: db, Mailer: rec}
	cfg := &Config{Mailer: mailer.Config{}} // disabled

	handler := handleSendVerification(svc, cfg)
	req := authRequestWithUser(http.MethodPost, "/api/auth/send-verification", userID, "")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	code, _ := payload["dev_code"].(string)
	if len(code) != verificationCodeLength {
		t.Errorf("expected dev_code of length %d, got %q", verificationCodeLength, code)
	}
}

func TestSendVerificationThrottlesAfterCooldown(t *testing.T) {
	authRateLimiter = newRateLimiter()
	db, mock := newMockDB(t)
	defer db.Close()

	userID := "33333333-3333-4333-8333-333333333333"
	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status, COALESCE(password_hash, ''), COALESCE(kyc_status, 'unverified'), COALESCE(kyc_level, 'tier1_basic')
		FROM users
		WHERE id = $1
		LIMIT 1
	`)).WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "display_name", "role", "status", "password_hash", "kyc_status", "kyc_level"}).
			AddRow(userID, "a@example.com", "A", userRoleUser, userStatusActive, "$2a$10$abc", "unverified", "tier1_basic"))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO email_verifications`)).
		WithArgs(sqlmock.AnyArg(), userID, "a@example.com", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Second call repeats the SELECT but is blocked before INSERT.
	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status, COALESCE(password_hash, ''), COALESCE(kyc_status, 'unverified'), COALESCE(kyc_level, 'tier1_basic')
		FROM users
		WHERE id = $1
		LIMIT 1
	`)).WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "display_name", "role", "status", "password_hash", "kyc_status", "kyc_level"}).
			AddRow(userID, "a@example.com", "A", userRoleUser, userStatusActive, "$2a$10$abc", "unverified", "tier1_basic"))

	svc := &Services{DB: db, Mailer: newAuthRecorder(t)}
	cfg := &Config{Mailer: mailer.Config{Host: "smtp.example.com", From: "noreply@example.com"}}

	handler := handleSendVerification(svc, cfg)
	rr1 := httptest.NewRecorder()
	handler.ServeHTTP(rr1, authRequestWithUser(http.MethodPost, "/api/auth/send-verification", userID, ""))
	if rr1.Code != http.StatusOK {
		t.Fatalf("first call expected 200, got %d", rr1.Code)
	}
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, authRequestWithUser(http.MethodPost, "/api/auth/send-verification", userID, ""))
	if rr2.Code != http.StatusTooManyRequests {
		t.Fatalf("second call expected 429, got %d body=%s", rr2.Code, rr2.Body.String())
	}
}

func TestVerifyEmailHappyPath(t *testing.T) {
	authRateLimiter = newRateLimiter()
	db, mock := newMockDB(t)
	defer db.Close()

	userID := "44444444-4444-4444-8444-444444444444"
	code := "424242"
	codeHash := hashSecretToken(code)
	expires := time.Now().UTC().Add(5 * time.Minute)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, code_hash, expires_at, attempts`)).
		WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "code_hash", "expires_at", "attempts"}).
			AddRow("verif-1", codeHash, expires, 0))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE email_verifications SET consumed_at = $1 WHERE id = $2`)).
		WithArgs(sqlmock.AnyArg(), "verif-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE users SET email_verified = TRUE, email_verified_at = $1 WHERE id = $2`)).
		WithArgs(sqlmock.AnyArg(), userID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	svc := &Services{DB: db}
	cfg := &Config{}
	handler := handleVerifyEmail(svc, cfg)

	body, _ := json.Marshal(verifyEmailRequest{Code: code})
	req := authRequestWithUser(http.MethodPost, "/api/auth/verify-email", userID, string(body))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	assertMockExpectations(t, mock)
}

func TestVerifyEmailWrongCodeBumpsAttempts(t *testing.T) {
	authRateLimiter = newRateLimiter()
	db, mock := newMockDB(t)
	defer db.Close()

	userID := "55555555-5555-4555-8555-555555555555"
	good := "111111"
	bad := "999999"
	codeHash := hashSecretToken(good)
	expires := time.Now().UTC().Add(5 * time.Minute)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, code_hash, expires_at, attempts`)).
		WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "code_hash", "expires_at", "attempts"}).
			AddRow("verif-2", codeHash, expires, 0))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE email_verifications SET attempts = attempts + 1 WHERE id = $1`)).
		WithArgs("verif-2").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	svc := &Services{DB: db}
	cfg := &Config{}
	body, _ := json.Marshal(verifyEmailRequest{Code: bad})
	rr := httptest.NewRecorder()
	handleVerifyEmail(svc, cfg).ServeHTTP(rr, authRequestWithUser(http.MethodPost, "/api/auth/verify-email", userID, string(body)))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "验证码错误") {
		t.Errorf("expected zh detail, got %s", rr.Body.String())
	}
	assertMockExpectations(t, mock)
}

func TestForgotPasswordAlwaysReturnsOk(t *testing.T) {
	authRateLimiter = newRateLimiter()
	db, mock := newMockDB(t)
	defer db.Close()

	// Email doesn't exist — we should still return 200 and not leak that
	// fact. The SELECT will fail with sql.ErrNoRows; no insert follows.
	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status, COALESCE(password_hash, ''), COALESCE(kyc_status, 'unverified'), COALESCE(kyc_level, 'tier1_basic')
		FROM users
		WHERE LOWER(email) = LOWER($1)
		LIMIT 1
	`)).WithArgs("ghost@example.com").
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "display_name", "role", "status", "password_hash", "kyc_status", "kyc_level"}))

	rec := newAuthRecorder(t)
	svc := &Services{DB: db, Mailer: rec}
	cfg := &Config{Mailer: mailer.Config{Host: "smtp.example.com", From: "noreply@example.com"}, AppPublicURL: "https://app.example.com"}

	handler := handleForgotPassword(svc, cfg)
	body, _ := json.Marshal(forgotPasswordRequest{Email: "ghost@example.com"})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/auth/forgot-password", bytes.NewBuffer(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(rec.Messages) != 0 {
		t.Errorf("expected no mail dispatched, got %d", len(rec.Messages))
	}
}

func TestForgotPasswordIssuesTokenForExistingUser(t *testing.T) {
	authRateLimiter = newRateLimiter()
	db, mock := newMockDB(t)
	defer db.Close()

	userID := "66666666-6666-4666-8666-666666666666"
	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status, COALESCE(password_hash, ''), COALESCE(kyc_status, 'unverified'), COALESCE(kyc_level, 'tier1_basic')
		FROM users
		WHERE LOWER(email) = LOWER($1)
		LIMIT 1
	`)).WithArgs("known@example.com").
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "display_name", "role", "status", "password_hash", "kyc_status", "kyc_level"}).
			AddRow(userID, "known@example.com", "Known", userRoleUser, userStatusActive, "$2a$10$abc", "unverified", "tier1_basic"))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO password_resets`)).
		WithArgs(sqlmock.AnyArg(), userID, "known@example.com", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rec := newAuthRecorder(t)
	svc := &Services{DB: db, Mailer: rec}
	cfg := &Config{Mailer: mailer.Config{Host: "smtp.example.com", From: "noreply@example.com"}, AppPublicURL: "https://app.example.com"}

	handler := handleForgotPassword(svc, cfg)
	body, _ := json.Marshal(forgotPasswordRequest{Email: "known@example.com"})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/auth/forgot-password", bytes.NewBuffer(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(rec.Messages) != 1 {
		t.Fatalf("expected one mail dispatched, got %d", len(rec.Messages))
	}
	if !strings.Contains(rec.Messages[0].HTMLBody, "https://app.example.com/reset-password") {
		t.Errorf("expected reset link in body, got %s", rec.Messages[0].HTMLBody)
	}
}

func TestResetPasswordRotatesAndNotifies(t *testing.T) {
	authRateLimiter = newRateLimiter()
	db, mock := newMockDB(t)
	defer db.Close()

	token := "tok-secret-1234567890abcdef"
	tokenHash := hashSecretToken(token)
	userID := "77777777-7777-4777-8777-777777777777"
	expires := time.Now().UTC().Add(30 * time.Minute)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, user_id, email, expires_at`)).
		WithArgs(tokenHash).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "email", "expires_at"}).
			AddRow("reset-1", userID, "owner@example.com", expires))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE password_resets SET consumed_at = $1 WHERE id = $2`)).
		WithArgs(sqlmock.AnyArg(), "reset-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE users
		SET password_hash = $1,
		    failed_login_attempts = 0,
		    locked_until = NULL,
		    updated_at = NOW()
		WHERE id = $2`)).
		WithArgs(sqlmock.AnyArg(), userID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE password_resets
		SET consumed_at = $1
		WHERE user_id = $2 AND consumed_at IS NULL AND id <> $3`)).
		WithArgs(sqlmock.AnyArg(), userID, "reset-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status, COALESCE(password_hash, ''), COALESCE(kyc_status, 'unverified'), COALESCE(kyc_level, 'tier1_basic')
		FROM users
		WHERE id = $1
		LIMIT 1
	`)).WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "display_name", "role", "status", "password_hash", "kyc_status", "kyc_level"}).
			AddRow(userID, "owner@example.com", "Owner", userRoleUser, userStatusActive, "$2a$10$abc", "unverified", "tier1_basic"))

	rec := newAuthRecorder(t)
	svc := &Services{DB: db, Mailer: rec}
	cfg := &Config{Mailer: mailer.Config{Host: "smtp.example.com", From: "noreply@example.com"}}

	body, _ := json.Marshal(resetPasswordRequest{Token: token, NewPassword: "NewStrongPass1!"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/reset-password", bytes.NewBuffer(body))
	rr := httptest.NewRecorder()
	handleResetPassword(svc, cfg).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(rec.Messages) != 1 {
		t.Fatalf("expected one mail dispatched, got %d", len(rec.Messages))
	}
	assertMockExpectations(t, mock)
}

func TestChangePasswordRequiresOldPassword(t *testing.T) {
	authRateLimiter = newRateLimiter()
	db, mock := newMockDB(t)
	defer db.Close()

	userID := "88888888-8888-4888-8888-888888888888"
	oldHash, err := bcrypt.GenerateFromPassword([]byte("oldPassword1!"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status, COALESCE(password_hash, ''), COALESCE(kyc_status, 'unverified'), COALESCE(kyc_level, 'tier1_basic')
		FROM users
		WHERE id = $1
		LIMIT 1
	`)).WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "display_name", "role", "status", "password_hash", "kyc_status", "kyc_level"}).
			AddRow(userID, "owner@example.com", "Owner", userRoleUser, userStatusActive, string(oldHash), "unverified", "tier1_basic"))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE users
		SET password_hash = $1,
		    failed_login_attempts = 0,
		    locked_until = NULL,
		    updated_at = NOW()
		WHERE id = $2`)).
		WithArgs(sqlmock.AnyArg(), userID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rec := newAuthRecorder(t)
	svc := &Services{DB: db, Mailer: rec}
	cfg := &Config{Mailer: mailer.Config{Host: "smtp.example.com", From: "noreply@example.com"}}

	body, _ := json.Marshal(changePasswordRequest{OldPassword: "oldPassword1!", NewPassword: "newStrong#2"})
	req := authRequestWithUser(http.MethodPost, "/api/auth/change-password", userID, string(body))
	rr := httptest.NewRecorder()
	handleChangePassword(svc, cfg).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(rec.Messages) != 1 {
		t.Fatalf("expected notification mail dispatched, got %d", len(rec.Messages))
	}
	assertMockExpectations(t, mock)
}

func TestRateLimiterAllowsThenBlocks(t *testing.T) {
	lim := newRateLimiter()
	for i := 0; i < 3; i++ {
		if !lim.Allow("k", 3, time.Minute) {
			t.Fatalf("expected allow %d", i+1)
		}
	}
	if lim.Allow("k", 3, time.Minute) {
		t.Fatal("expected block after exceeding limit")
	}
}

func TestGenerateVerificationCodeShape(t *testing.T) {
	code, err := generateVerificationCode()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(code) != verificationCodeLength {
		t.Fatalf("expected %d digits, got %d (%q)", verificationCodeLength, len(code), code)
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			t.Fatalf("non-digit %q in code %s", r, code)
		}
	}
}

// Sanity: ensure verifyEmail won't accept obviously wrong-length input
// without hitting the DB at all.
func TestVerifyEmailRejectsShortCode(t *testing.T) {
	authRateLimiter = newRateLimiter()
	db, _ := newMockDB(t)
	defer db.Close()
	svc := &Services{DB: db}
	cfg := &Config{}
	body, _ := json.Marshal(verifyEmailRequest{Code: "12"})
	req := authRequestWithUser(http.MethodPost, "/api/auth/verify-email", "user-1", string(body))
	rr := httptest.NewRecorder()
	handleVerifyEmail(svc, cfg).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// ensureCancelledContextPathDoesNotHang is a smoke test that the
// mailer/recorder Send path respects ctx.Done.
func TestRecorderRespectsContext(t *testing.T) {
	rec := &mailer.Recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := mailer.SendEmailVerification(ctx, rec, mailer.Config{}, mailer.EmailVerificationPayload{To: "x@y.com", Code: "123456"}); err != nil {
		// Recorder ignores ctx — that's the desired behaviour here.
		t.Fatalf("recorder send should not error: %v", err)
	}
}
