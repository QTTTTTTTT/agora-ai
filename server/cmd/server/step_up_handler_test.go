package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/api"
)

// stepUpTestCfg returns a config with a fixed JWT secret so tokens
// minted in one helper round-trip through verify in another.
func stepUpTestCfg() *Config {
	return &Config{JWTSecret: "step-up-test-secret"}
}

// TestStepUp_HandleStepUp_HappyPath
//
// A successful POST /api/auth/step-up should return:
//   - a non-empty token,
//   - an expires_at that's strictly in the future,
//   - a ttl_seconds matching stepUpTokenTTL.
//
// The token must round-trip through verifyStepUpToken when
// re-presented as the X-Step-Up-Token header.
func TestStepUp_HandleStepUp_HappyPath(t *testing.T) {
	cfg := stepUpTestCfg()
	h := newStepUpHandler(cfg)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/step-up", strings.NewReader(`{}`))
	req.ContentLength = 2
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), "user-42"))
	rr := httptest.NewRecorder()
	h.handleStepUp(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp stepUpResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Token == "" {
		t.Errorf("Token is empty")
	}
	if resp.TTLSec != int(stepUpTokenTTL.Seconds()) {
		t.Errorf("TTLSec = %d, want %d", resp.TTLSec, int(stepUpTokenTTL.Seconds()))
	}
	exp, err := time.Parse(time.RFC3339, resp.ExpiresAt)
	if err != nil {
		t.Fatalf("parse expires_at: %v", err)
	}
	if !exp.After(time.Now()) {
		t.Errorf("ExpiresAt = %s, should be in the future", resp.ExpiresAt)
	}

	// Now round-trip the token through verifyStepUpToken.
	verifyReq := httptest.NewRequest(http.MethodPost, "/api/funds/x/orders/y/cancel", nil)
	verifyReq.Header.Set(stepUpHeader, resp.Token)
	verifyReq = verifyReq.WithContext(api.WithAuthenticatedUserID(verifyReq.Context(), "user-42"))
	v := verifyStepUpToken(verifyReq, cfg)
	if !v.Valid {
		t.Fatalf("verifyStepUpToken returned invalid: %+v", v)
	}
	if v.UserID != "user-42" {
		t.Errorf("UserID = %q, want user-42", v.UserID)
	}
	if v.Audience != stepUpAudience {
		t.Errorf("Audience = %q, want %q", v.Audience, stepUpAudience)
	}
}

func TestStepUp_HandleStepUp_Unauthenticated(t *testing.T) {
	h := newStepUpHandler(stepUpTestCfg())
	req := httptest.NewRequest(http.MethodPost, "/api/auth/step-up", nil)
	rr := httptest.NewRecorder()
	h.handleStepUp(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

// TestStepUp_Verify_MissingHeader
//
// Absence of the X-Step-Up-Token header is reported as
// {Valid:false, Reason:"missing"} — NOT an error.
func TestStepUp_Verify_MissingHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	v := verifyStepUpToken(req, stepUpTestCfg())
	if v.Valid {
		t.Errorf("Valid = true on missing header")
	}
	if v.Reason != "missing" {
		t.Errorf("Reason = %q, want %q", v.Reason, "missing")
	}
}

// TestStepUp_Verify_RejectsRegularSessionToken
//
// A regular session token (no aud claim) MUST NOT be accepted as a
// step-up token even though both are signed under the same JWT
// secret. The audience-mismatch check is the load-bearing defence.
func TestStepUp_Verify_RejectsRegularSessionToken(t *testing.T) {
	cfg := stepUpTestCfg()
	tok, _, err := issueSessionTokenWithKid("user-42", cfg.JWTSecret, "", time.Hour)
	if err != nil {
		t.Fatalf("issueSessionTokenWithKid: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set(stepUpHeader, tok)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), "user-42"))
	v := verifyStepUpToken(req, cfg)
	if v.Valid {
		t.Errorf("session token accepted as step-up token")
	}
	if v.Reason == "" {
		t.Errorf("Reason should be populated, got empty")
	}
}

// TestStepUp_Verify_RejectsTwoFAChallengeToken
//
// A 2FA challenge token (audience="2fa_challenge") MUST also be
// rejected on the step-up path — same secret, different purpose.
func TestStepUp_Verify_RejectsTwoFAChallengeToken(t *testing.T) {
	cfg := stepUpTestCfg()
	tok, _, err := issueTwoFAChallenge("user-42", cfg)
	if err != nil {
		t.Fatalf("issueTwoFAChallenge: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set(stepUpHeader, tok)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), "user-42"))
	v := verifyStepUpToken(req, cfg)
	if v.Valid {
		t.Errorf("2fa challenge token accepted as step-up token")
	}
	if v.Reason != "audience mismatch" {
		t.Errorf("Reason = %q, want %q", v.Reason, "audience mismatch")
	}
}

// TestStepUp_Verify_RejectsSubjectMismatch
//
// A step-up token minted for user A must not be accepted on a
// request authenticated as user B. Defence in depth against a
// stolen token leaking across accounts.
func TestStepUp_Verify_RejectsSubjectMismatch(t *testing.T) {
	cfg := stepUpTestCfg()
	h := newStepUpHandler(cfg)

	// Mint a token under user-A.
	mintReq := httptest.NewRequest(http.MethodPost, "/api/auth/step-up", nil)
	mintReq = mintReq.WithContext(api.WithAuthenticatedUserID(mintReq.Context(), "user-A"))
	rr := httptest.NewRecorder()
	h.handleStepUp(rr, mintReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("mint failed: %d %s", rr.Code, rr.Body.String())
	}
	var resp stepUpResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)

	// Re-present it on a request authenticated as user-B.
	verifyReq := httptest.NewRequest(http.MethodPost, "/", nil)
	verifyReq.Header.Set(stepUpHeader, resp.Token)
	verifyReq = verifyReq.WithContext(api.WithAuthenticatedUserID(verifyReq.Context(), "user-B"))
	v := verifyStepUpToken(verifyReq, cfg)
	if v.Valid {
		t.Errorf("token minted under user-A accepted on user-B request")
	}
	if v.Reason != "subject mismatch" {
		t.Errorf("Reason = %q, want %q", v.Reason, "subject mismatch")
	}
}

// TestStepUpAuditMetadata_Shape pins the wire shape of the
// metadata helper so a downstream audit reader can rely on the
// "step_up" boolean key.
func TestStepUpAuditMetadata_Shape(t *testing.T) {
	valid := stepUpAuditMetadata(stepUpVerification{Valid: true, IssuedAt: time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)})
	if v, ok := valid["step_up"].(bool); !ok || !v {
		t.Errorf("valid metadata missing step_up=true: %#v", valid)
	}
	if _, ok := valid["step_up_issued_at"]; !ok {
		t.Errorf("valid metadata missing step_up_issued_at: %#v", valid)
	}

	invalid := stepUpAuditMetadata(stepUpVerification{Reason: "missing"})
	if v, ok := invalid["step_up"].(bool); !ok || v {
		t.Errorf("invalid metadata missing step_up=false: %#v", invalid)
	}
	if invalid["step_up_reason"] != "missing" {
		t.Errorf("invalid metadata missing reason: %#v", invalid)
	}
}

// TestMergeAuditMetadata_RightBias confirms later maps win on key
// collision — the documented contract that lets call sites layer
// step-up flags on top without worrying about ordering surprises.
func TestMergeAuditMetadata_RightBias(t *testing.T) {
	got := mergeAuditMetadata(
		map[string]any{"a": 1, "b": 2},
		map[string]any{"b": 3, "c": 4},
	)
	if got["a"] != 1 || got["b"] != 3 || got["c"] != 4 {
		t.Errorf("merge = %#v, want {a:1,b:3,c:4}", got)
	}
}
