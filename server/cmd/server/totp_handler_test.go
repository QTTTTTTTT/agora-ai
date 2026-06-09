package main

import (
	"bytes"
	"crypto/rand"
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
	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/totp"
	pqotp "github.com/pquerna/otp/totp"
)

// totpTestKey is the 32-byte AES key reused across tests. NEVER
// use it in production — checked into the repo intentionally.
var totpTestKey = []byte("0123456789abcdef0123456789abcdef")

// totpRowColumns mirrors the SELECT projection in
// repository.UserTOTPRepo.GetByUserID. Kept by hand here so a
// mismatch surfaces as "could not match actual sql".
var totpRowColumns = []string{
	"user_id", "secret_encrypted", "issuer", "account_label", "digits",
	"period_seconds", "algorithm", "recovery_codes_hashed", "enrolment_attempts",
	"enabled_at", "last_verified_at", "last_used_recovery_at",
	"created_at", "updated_at",
}

func newTOTPTestEnv(t *testing.T) (*totpHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock := newMockDB(t)
	c, err := totp.NewCipher(totpTestKey)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	cfg := &Config{JWTSecret: "test-secret"}
	h := &totpHandler{
		repo:        repository.NewUserTOTPRepo(db),
		cipher:      c,
		auditLogger: nil, // skip audit for unit tests
		cfg:         cfg,
		db:          db,
	}
	return h, mock, func() { _ = db.Close() }
}

// totpEncryptForTest produces a ciphertext compatible with the
// handler's cipher. We replicate the AES-GCM (nonce || ciphertext)
// layout the totp package's private encrypt() uses so the handler
// can decrypt it on the verify path.
func totpEncryptForTest(t *testing.T, plain string) []byte {
	t.Helper()
	c, err := totp.NewCipher(totpTestKey)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	nonce := make([]byte, c.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return append(nonce, c.Seal(nil, nonce, []byte(plain), nil)...)
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

func TestTOTPHandler_Status_Unauthenticated(t *testing.T) {
	h, _, cleanup := newTOTPTestEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/2fa/status", nil)
	rr := httptest.NewRecorder()
	h.handleStatus(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestTOTPHandler_Status_NotEnrolled(t *testing.T) {
	h, mock, cleanup := newTOTPTestEnv(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta(`FROM user_totp_secrets`)).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows(totpRowColumns)) // no rows
	req := httptest.NewRequest(http.MethodGet, "/api/auth/2fa/status", nil)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	h.handleStatus(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	var body totpStatusResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Enabled || body.EnrolmentPending {
		t.Errorf("body = %+v, want all-false (no row)", body)
	}
}

func TestTOTPHandler_Status_Enabled(t *testing.T) {
	h, mock, cleanup := newTOTPTestEnv(t)
	defer cleanup()

	enabledAt := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`FROM user_totp_secrets`)).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows(totpRowColumns).AddRow(
			"user-1", []byte("ciphertext"), "FundAI", "user@example.com", 6,
			30, "SHA1", "{}", 0,
			enabledAt, enabledAt, nil,
			enabledAt, enabledAt,
		))
	req := httptest.NewRequest(http.MethodGet, "/api/auth/2fa/status", nil)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	h.handleStatus(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var body totpStatusResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if !body.Enabled {
		t.Errorf("Enabled = false, want true")
	}
	if body.LastVerifiedAt == "" {
		t.Errorf("LastVerifiedAt empty")
	}
}

// ---------------------------------------------------------------------------
// Verify
// ---------------------------------------------------------------------------

// TestTOTPHandler_Verify_HappyPath bypasses the setup endpoint by
// pre-encrypting a known secret. We then submit the freshly-derived
// 6-digit code and assert MarkEnabled was called.
func TestTOTPHandler_Verify_HappyPath(t *testing.T) {
	h, mock, cleanup := newTOTPTestEnv(t)
	defer cleanup()

	plainSecret := "JBSWY3DPEHPK3PXP" // RFC 6238 test vector
	encrypted := totpEncryptForTest(t, plainSecret)
	now := time.Now()

	mock.ExpectQuery(regexp.QuoteMeta(`FROM user_totp_secrets`)).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows(totpRowColumns).AddRow(
			"user-1", encrypted, "FundAI", "u", 6, 30, "SHA1", "{}", 0,
			nil, nil, nil, now, now,
		))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE user_totp_secrets`)).
		WithArgs("user-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	code, err := pqotp.GenerateCode(plainSecret, time.Now())
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}
	body, _ := json.Marshal(totpVerifyRequest{Code: code})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/2fa/verify",
		bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	h.handleVerify(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !regexp.MustCompile(`"enabled"\s*:\s*true`).Match(rr.Body.Bytes()) {
		t.Errorf("body missing enabled=true: %s", rr.Body.String())
	}
}

// TestTOTPHandler_Verify_BadCodeBumpsCounter ensures a wrong code
// during enrolment is logged as an attempt and surfaced as 401.
func TestTOTPHandler_Verify_BadCodeBumpsCounter(t *testing.T) {
	h, mock, cleanup := newTOTPTestEnv(t)
	defer cleanup()

	encrypted := totpEncryptForTest(t, "JBSWY3DPEHPK3PXP")
	mock.ExpectQuery(regexp.QuoteMeta(`FROM user_totp_secrets`)).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows(totpRowColumns).AddRow(
			"user-1", encrypted, "FundAI", "u", 6, 30, "SHA1", "{}", 0,
			nil, nil, nil, time.Now(), time.Now(),
		))
	mock.ExpectQuery(regexp.QuoteMeta(`UPDATE user_totp_secrets`)).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows([]string{"enrolment_attempts"}).AddRow(1))

	body, _ := json.Marshal(totpVerifyRequest{Code: "000000"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/2fa/verify",
		bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	h.handleVerify(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
}

// TestTOTPHandler_Verify_NotEnrolled returns 404 when there's no
// pending enrolment row.
func TestTOTPHandler_Verify_NotEnrolled(t *testing.T) {
	h, mock, cleanup := newTOTPTestEnv(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta(`FROM user_totp_secrets`)).
		WithArgs("user-1").
		WillReturnError(sql.ErrNoRows)

	body, _ := json.Marshal(totpVerifyRequest{Code: "111111"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/2fa/verify",
		bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	h.handleVerify(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no enrolment); body=%s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Challenge flow
// ---------------------------------------------------------------------------

// TestTOTPHandler_Challenge_RejectsRegularSessionToken protects
// against session-token / challenge-token confusion: a session
// token signed under the same secret should be REJECTED on
// /api/auth/2fa/challenge because its audience claim is missing
// (sessions don't carry one).
func TestTOTPHandler_Challenge_RejectsRegularSessionToken(t *testing.T) {
	h, _, cleanup := newTOTPTestEnv(t)
	defer cleanup()

	sessionTok, _, err := issueSessionTokenWithKid("u", "test-secret", "", time.Hour)
	if err != nil {
		t.Fatalf("issueSessionTokenWithKid: %v", err)
	}
	body, _ := json.Marshal(totpChallengeRequest{Challenge: sessionTok, Code: "111111"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/2fa/challenge",
		bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rr := httptest.NewRecorder()
	h.handleChallenge(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (audience mismatch); body=%s", rr.Code, rr.Body.String())
	}
}

func TestTOTPHandler_Challenge_RejectsMissingFields(t *testing.T) {
	h, _, cleanup := newTOTPTestEnv(t)
	defer cleanup()

	cases := []string{
		`{}`,
		`{"challenge":""}`,
		`{"code":"123456"}`,
		`{"challenge":"abc"}`,
	}
	for _, raw := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/2fa/challenge",
			strings.NewReader(raw))
		req.ContentLength = int64(len(raw))
		rr := httptest.NewRecorder()
		h.handleChallenge(rr, req)
		if rr.Code != http.StatusBadRequest && rr.Code != http.StatusUnauthorized {
			t.Errorf("body=%q: status = %d, want 400 or 401", raw, rr.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers (issueTwoFAChallenge round-trip)
// ---------------------------------------------------------------------------

func TestIssueAndParseTwoFAChallenge(t *testing.T) {
	cfg := &Config{JWTSecret: "test-secret"}
	tok, exp, err := issueTwoFAChallenge("user-7", cfg)
	if err != nil {
		t.Fatalf("issueTwoFAChallenge: %v", err)
	}
	if exp.Before(time.Now()) {
		t.Errorf("exp = %v, should be in the future", exp)
	}
	h := &totpHandler{cfg: cfg}
	uid, err := h.parseChallenge(tok)
	if err != nil {
		t.Fatalf("parseChallenge: %v", err)
	}
	if uid != "user-7" {
		t.Errorf("uid = %q, want user-7", uid)
	}
}

// TestParseChallenge_RejectsAudienceMismatch is the unit-level
// counterpart of TestTOTPHandler_Challenge_RejectsRegularSessionToken
// — same protection, smaller blast radius.
func TestParseChallenge_RejectsAudienceMismatch(t *testing.T) {
	cfg := &Config{JWTSecret: "test-secret"}
	sessionTok, _, err := issueSessionTokenWithKid("u", "test-secret", "", time.Hour)
	if err != nil {
		t.Fatalf("issueSessionTokenWithKid: %v", err)
	}
	h := &totpHandler{cfg: cfg}
	if _, err := h.parseChallenge(sessionTok); err == nil {
		t.Errorf("parseChallenge accepted a session token (no aud)")
	}
}

// ---------------------------------------------------------------------------
// A5 — super_admin forced enrollment via enrollment grant
// ---------------------------------------------------------------------------

// TestIssueAndParseEnrollmentGrant guards the round-trip: a freshly
// minted enrollment grant decodes back to the same userID. Without
// this, the entire enrollment flow becomes silently broken when
// signJWTWithAudience or the audience constant drifts.
func TestIssueAndParseEnrollmentGrant(t *testing.T) {
	cfg := &Config{JWTSecret: "test-secret"}
	tok, exp, err := issueTwoFAEnrollmentGrant("admin-1", cfg)
	if err != nil {
		t.Fatalf("issueTwoFAEnrollmentGrant: %v", err)
	}
	if exp.Before(time.Now()) {
		t.Errorf("exp = %v, should be in the future", exp)
	}
	h := &totpHandler{cfg: cfg}
	uid, err := h.parseEnrollmentGrant(tok)
	if err != nil {
		t.Fatalf("parseEnrollmentGrant: %v", err)
	}
	if uid != "admin-1" {
		t.Errorf("uid = %q, want admin-1", uid)
	}
}

// TestParseEnrollmentGrant_RejectsChallengeToken makes sure the
// audience separation between challenge and enrollment grants is
// actually enforced. Without this, a stolen challenge token could
// be passed off as an enrollment grant and trigger a re-enrollment
// against an existing 2FA-enabled account.
func TestParseEnrollmentGrant_RejectsChallengeToken(t *testing.T) {
	cfg := &Config{JWTSecret: "test-secret"}
	challengeTok, _, err := issueTwoFAChallenge("admin-1", cfg)
	if err != nil {
		t.Fatalf("issueTwoFAChallenge: %v", err)
	}
	h := &totpHandler{cfg: cfg}
	if _, err := h.parseEnrollmentGrant(challengeTok); err == nil {
		t.Errorf("parseEnrollmentGrant accepted a 2fa_challenge audience token")
	}
}

// TestParseEnrollmentGrant_RejectsSessionToken is the symmetric guard
// for the bigger-blast-radius case: a stolen session token must not
// be promoted into an enrollment grant (which would let the attacker
// reset 2FA for their own account, the inverse of what we want).
func TestParseEnrollmentGrant_RejectsSessionToken(t *testing.T) {
	cfg := &Config{JWTSecret: "test-secret"}
	sessionTok, _, err := issueSessionTokenWithKid("u", "test-secret", "", time.Hour)
	if err != nil {
		t.Fatalf("issueSessionTokenWithKid: %v", err)
	}
	h := &totpHandler{cfg: cfg}
	if _, err := h.parseEnrollmentGrant(sessionTok); err == nil {
		t.Errorf("parseEnrollmentGrant accepted a regular session token")
	}
}

// TestTOTPHandler_EnrollStart_RejectsBadGrant covers the simplest
// negative path: a junk token in the body returns 401, not a 500
// or (worse) a 200 with a leaked QR.
func TestTOTPHandler_EnrollStart_RejectsBadGrant(t *testing.T) {
	h, _, cleanup := newTOTPTestEnv(t)
	defer cleanup()

	body, _ := json.Marshal(enrollGrantRequest{Grant: "not-a-jwt"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/2fa/enroll-start",
		bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rr := httptest.NewRecorder()
	h.handleEnrollStart(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
}

// TestTOTPHandler_EnrollComplete_RejectsMissingFields guards the
// shape contract: both grant + code are mandatory. Without the code
// the handler would happily mint a session against a half-enrolled
// row, defeating the whole point of forcing enrollment.
func TestTOTPHandler_EnrollComplete_RejectsMissingFields(t *testing.T) {
	h, _, cleanup := newTOTPTestEnv(t)
	defer cleanup()

	cases := []string{
		`{}`,
		`{"grant":""}`,
		`{"code":"123456"}`,
		`{"grant":"abc"}`, // grant present but no code
	}
	for _, raw := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/2fa/enroll-complete",
			strings.NewReader(raw))
		req.ContentLength = int64(len(raw))
		rr := httptest.NewRecorder()
		h.handleEnrollComplete(rr, req)
		if rr.Code != http.StatusBadRequest && rr.Code != http.StatusUnauthorized {
			t.Errorf("body=%q: status = %d, want 400 or 401", raw, rr.Code)
		}
	}
}

// TestIsSuperAdminUser is small but matters: the role string is the
// gate that decides whether a user is forced through enrollment. A
// case-sensitivity regression here would silently disable the gate
// for any deployment that emits "SUPER_ADMIN" or "Super_Admin" from
// its SSO claim mapping.
func TestIsSuperAdminUser(t *testing.T) {
	cases := []struct {
		role string
		want bool
	}{
		{"super_admin", true},
		{"SUPER_ADMIN", true},
		{"  super_admin  ", true},
		{"Super_Admin", true},
		{"admin", false},
		{"user", false},
		{"", false},
	}
	for _, c := range cases {
		got := isSuperAdminUser(&authenticatedUser{Role: c.role})
		if got != c.want {
			t.Errorf("role=%q: got %v, want %v", c.role, got, c.want)
		}
	}
	// Nil receiver is the boot-time degenerate case (handleLogin
	// shortcircuits before we get here, but the helper should be
	// safe to call regardless).
	if isSuperAdminUser(nil) {
		t.Errorf("isSuperAdminUser(nil) = true, want false")
	}
}

// TestTotpRowFullyEnabled covers the nil + half-enrolled cases.
// A row that exists but never had MarkEnabled called MUST count as
// NOT enabled; otherwise an admin who abandoned an enrollment would
// be allowed through on next login with no 2FA in effect.
func TestTotpRowFullyEnabled(t *testing.T) {
	if totpRowFullyEnabled(nil) {
		t.Errorf("nil row should not count as enabled")
	}
	// A row with no EnabledAt is pending enrollment.
	pending := &repository.UserTOTP{}
	if totpRowFullyEnabled(pending) {
		t.Errorf("pending row should not count as enabled")
	}
	// A row with EnabledAt populated is the only valid "enabled" state.
	enabled := &repository.UserTOTP{EnabledAt: sql.NullTime{Time: time.Now(), Valid: true}}
	if !totpRowFullyEnabled(enabled) {
		t.Errorf("row with EnabledAt should count as enabled")
	}
}
