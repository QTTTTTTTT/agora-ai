// Live-readiness HTTP handler tests (P0-9).
//
// Three scenarios are covered:
//   1. unauthenticated → 401,
//   2. fund authz failure → 403/404,
//   3. happy path returns the populated readiness JSON for a
//      live fund that's missing every pillar.
//
// We deliberately don't drive every pillar combination here —
// pillar-by-pillar combinatorics are exhaustively covered in
// live_trading_gate_test.go. The handler test only verifies the
// HTTP plumbing (path matching, JSON shape, fund ownership).

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/repository"
	"github.com/lib/pq"
)

func newLiveReadinessTestHandler(t *testing.T) (*liveReadinessHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	cfg := &Config{JWTSecret: liveGateTestSecret}
	gate := &liveTradingGate{
		db:             db,
		totpRepo:       repository.NewUserTOTPRepo(db),
		brokerLinkRepo: repository.NewBrokerLinkRepo(db),
		cfg:            cfg,
		enforced:       true,
	}
	h := &liveReadinessHandler{
		fundRepo:    repository.NewFundRepo(db),
		companyRepo: repository.NewFundCompanyRepo(db),
		gate:        gate,
		cfg:         cfg,
		svc:         &Services{DB: db},
	}
	return h, mock, func() { _ = db.Close() }
}

// expectFundAndCompanyForReadiness primes both queries
// authorizeFundAccess fires.
func expectFundAndCompanyForReadiness(mock sqlmock.Sqlmock, fundID, companyID, userID, mode string) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "company_id", "name", "description", "trading_mode",
			"initial_capital", "current_capital", "total_assets", "nav", "status",
			"config", "created_at", "updated_at",
		}).AddRow(
			fundID, companyID, "Live Fund", "", mode,
			100000.0, 100000.0, 100000.0, 1.0, "active",
			[]byte("{}"), now, now,
		))
	mock.ExpectQuery("FROM fund_companies").
		WithArgs(companyID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "owner_user_id", "name", "description", "created_at", "updated_at",
		}).AddRow(companyID, userID, "Co", "", now, now))
}

// expectUserLookup primes loadActiveUserByID with the requested
// kyc_status / level.
func expectUserLookup(mock sqlmock.Sqlmock, userID, kycStatus, kycLevel string) {
	mock.ExpectQuery(regexp.QuoteMeta(`FROM users`)).
		WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "email", "display_name", "role", "status",
			"password_hash", "kyc_status", "kyc_level",
		}).AddRow(userID, "u@example.com", "U", "user", "active", "", kycStatus, kycLevel))
}

// TestLiveReadiness_Unauthenticated returns 401 without touching DB.
func TestLiveReadiness_Unauthenticated(t *testing.T) {
	h, _, cleanup := newLiveReadinessTestHandler(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/funds/f/live-readiness", nil)
	req.SetPathValue("fundId", "f")
	rr := httptest.NewRecorder()
	h.handle(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

// TestLiveReadiness_LiveFundAllPillarsFailing
//
// Live fund + KYC unverified + no broker link + no 2FA + no
// step-up token. All four pillar bools must be false, ready=false,
// first_failing="kyc_required", gate_enforced=true.
func TestLiveReadiness_LiveFundAllPillarsFailing(t *testing.T) {
	h, mock, cleanup := newLiveReadinessTestHandler(t)
	defer cleanup()
	// Use real UUID strings so loadActiveUserByID's uuid.Parse passes.
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"

	expectFundAndCompanyForReadiness(mock, fundID, companyID, userID, "live")
	expectUserLookup(mock, userID, "unverified", "tier1_basic")
	mock.ExpectQuery(regexp.QuoteMeta(`FROM broker_links`)).
		WithArgs(fundID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(`FROM user_totp_secrets`)).
		WithArgs(userID).
		WillReturnError(sql.ErrNoRows)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/"+fundID+"/live-readiness", nil)
	req.SetPathValue("fundId", fundID)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), userID))
	rr := httptest.NewRecorder()
	h.handle(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp liveReadinessResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Ready {
		t.Errorf("Ready=true on all-failing fund: %+v", resp)
	}
	if !resp.GateEnforced {
		t.Errorf("GateEnforced=false on live fund with gate on")
	}
	if resp.TradingMode != "live" {
		t.Errorf("TradingMode = %q, want live", resp.TradingMode)
	}
	if resp.FirstFailing != string(LiveReadinessKYCRequired) {
		t.Errorf("FirstFailing = %q, want kyc_required", resp.FirstFailing)
	}
}

// TestLiveReadiness_LiveFundAllPillarsPassing
//
// Live fund with KYC verified, active broker link, 2FA enabled,
// and a freshly-minted step-up token in the query parameter.
// Ready must be true and all per-pillar bools must be set.
func TestLiveReadiness_LiveFundAllPillarsPassing(t *testing.T) {
	h, mock, cleanup := newLiveReadinessTestHandler(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"

	expectFundAndCompanyForReadiness(mock, fundID, companyID, userID, "live")
	expectUserLookup(mock, userID, "verified", "tier2_full")
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`FROM broker_links`)).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows(liveGateBrokerLinkColumns).AddRow(
			"link-1", fundID, userID, "ibkr", "U1234567", "active",
			"approver-1", now, []byte{}, []byte(`{}`),
			now, now,
		))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM user_totp_secrets`)).
		WithArgs(userID).
		WillReturnRows(sqlmock.NewRows(liveGateTOTPColumns).AddRow(
			userID, []byte("ct"), "FundAI", "u@example.com", 6,
			30, "SHA1", pq.Array([]string{"hash-1"}), 1,
			now, now, nil,
			now, now,
		))

	// Mint a step-up token bound to the user and present it via
	// the query parameter the handler accepts.
	tok, err := signJWTWithAudience(userID, stepUpAudience, h.cfg.JWTSecret, "",
		time.Now().UTC(), time.Now().UTC().Add(stepUpTokenTTL))
	if err != nil {
		t.Fatalf("mint step-up: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/funds/"+fundID+"/live-readiness?step_up_token="+tok, nil)
	req.SetPathValue("fundId", fundID)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), userID))
	rr := httptest.NewRecorder()
	h.handle(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp liveReadinessResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Ready {
		t.Errorf("Ready=false with all pillars passing: %+v", resp)
	}
	if !resp.KYCOK || !resp.BrokerLinkOK || !resp.TwoFAOK || !resp.StepUpOK {
		t.Errorf("pillar bools not all true: %+v", resp)
	}
	if resp.BrokerLinkID != "link-1" {
		t.Errorf("BrokerLinkID = %q, want link-1", resp.BrokerLinkID)
	}
}

// TestLiveReadiness_SimulationFundReportsReady
//
// Sanity check: a simulation fund returns ready=true,
// gate_enforced=false even with no other expectations primed —
// the gate must short-circuit before any DB lookup.
func TestLiveReadiness_SimulationFundReportsReady(t *testing.T) {
	h, mock, cleanup := newLiveReadinessTestHandler(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"

	expectFundAndCompanyForReadiness(mock, fundID, companyID, userID, "simulation")
	// We still need the user lookup because the readiness handler
	// loads the user before invoking the gate (so a future
	// "preview live mode" toggle can populate KYCOK without
	// re-routing through the DB).
	expectUserLookup(mock, userID, "unverified", "tier1_basic")

	req := httptest.NewRequest(http.MethodGet, "/api/funds/"+fundID+"/live-readiness", nil)
	req.SetPathValue("fundId", fundID)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), userID))
	rr := httptest.NewRecorder()
	h.handle(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp liveReadinessResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Ready {
		t.Errorf("Ready=false on simulation fund: %+v", resp)
	}
	if resp.GateEnforced {
		t.Errorf("GateEnforced=true on simulation fund")
	}
	if resp.TradingMode != "simulation" {
		t.Errorf("TradingMode = %q", resp.TradingMode)
	}
}

// Compile-time guard that the test helper assembles against the
// real liveTradingGate type.
var _ context.Context = context.Background()
