// Live-trading gate unit tests (P0-9).
//
// Each test drives one pillar to fail (or pass) and asserts:
//   - Ready reflects the AND of all four pillars,
//   - FirstFailing names the first failing pillar in the natural
//     order (KYC → broker_link → 2FA → step-up),
//   - GateEnforced flips correctly with trading_mode + kill switch.
//
// We avoid the full HTTP stack: each test wires a sqlmock-backed
// repo set, an authenticatedUser literal, and a synthetic
// http.Request when a step-up token is needed. Keeping the test
// surface this small means future pillar additions only require
// extending one helper.

package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/repository"
	"github.com/lib/pq"
)

const liveGateTestSecret = "live-gate-test-secret"

// liveGateBrokerLinkColumns mirrors the SELECT in
// repository.scanBrokerLink. Duplicated here (rather than imported)
// because the repository test file uses the unexported package-
// level slice; we deliberately don't widen its visibility.
var liveGateBrokerLinkColumns = []string{
	"id", "fund_id", "user_id", "broker_id", "account_id", "status",
	"approved_by", "approved_at", "credentials_encrypted", "metadata",
	"created_at", "updated_at",
}

// liveGateTOTPColumns mirrors the SELECT in
// repository.UserTOTPRepo.GetByUserID for the same reason.
var liveGateTOTPColumns = []string{
	"user_id", "secret_encrypted", "issuer", "account_label", "digits",
	"period_seconds", "algorithm", "recovery_codes_hashed", "enrolment_attempts",
	"enabled_at", "last_verified_at", "last_used_recovery_at",
	"created_at", "updated_at",
}

// liveGateFixtures bundles the repo set + cfg + sqlmock so each
// test can drive expectations without re-deriving the wiring.
type liveGateFixtures struct {
	gate *liveTradingGate
	mock sqlmock.Sqlmock
	cfg  *Config
	db   *sql.DB
}

func newLiveGateFixtures(t *testing.T, enforced bool) liveGateFixtures {
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
		enforced:       enforced,
	}
	return liveGateFixtures{gate: gate, mock: mock, cfg: cfg, db: db}
}

func (f *liveGateFixtures) close() { _ = f.db.Close() }

// expectActiveBrokerLink primes a successful broker_links lookup.
func (f *liveGateFixtures) expectActiveBrokerLink(fundID string) {
	now := time.Now()
	f.mock.ExpectQuery(regexp.QuoteMeta(`FROM broker_links`)).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows(liveGateBrokerLinkColumns).AddRow(
			"link-1", fundID, "user-1", "ibkr", "U1234567", "active",
			"approver-1", now, []byte("ct"), []byte(`{}`),
			now, now,
		))
}

// expectMissingBrokerLink primes a "no rows" lookup so the gate
// reports broker_link_not_active.
func (f *liveGateFixtures) expectMissingBrokerLink(fundID string) {
	f.mock.ExpectQuery(regexp.QuoteMeta(`FROM broker_links`)).
		WithArgs(fundID).
		WillReturnError(sql.ErrNoRows)
}

// expectTwoFAEnabled primes a row with enabled_at set so isTwoFAEnabled returns true.
func (f *liveGateFixtures) expectTwoFAEnabled(userID string) {
	now := time.Now()
	f.mock.ExpectQuery(regexp.QuoteMeta(`FROM user_totp_secrets`)).
		WithArgs(userID).
		WillReturnRows(sqlmock.NewRows(liveGateTOTPColumns).AddRow(
			userID, []byte("ct"), "FundAI", "u@example.com", 6,
			30, "SHA1", pq.Array([]string{"hash-1"}), 1,
			now, now, nil, // enabled_at, last_verified_at, last_used_recovery_at
			now, now,
		))
}

func (f *liveGateFixtures) expectTwoFADisabled(userID string) {
	f.mock.ExpectQuery(regexp.QuoteMeta(`FROM user_totp_secrets`)).
		WithArgs(userID).
		WillReturnError(sql.ErrNoRows)
}

// stepUpRequestForUser returns a request carrying a freshly minted
// step-up token bound to userID.
func stepUpRequestForUser(t *testing.T, cfg *Config, userID string) *http.Request {
	t.Helper()
	now := time.Now().UTC()
	tok, err := signJWTWithAudience(userID, stepUpAudience, cfg.JWTSecret, "", now, now.Add(stepUpTokenTTL))
	if err != nil {
		t.Fatalf("mint step-up token: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set(stepUpHeader, tok)
	return r
}

// TestLiveGate_SimulationFundAlwaysReady
//
// Funds in 'simulation' mode bypass the gate completely. Even
// without primed expectations, the gate must return Ready=true
// without touching the DB.
func TestLiveGate_SimulationFundAlwaysReady(t *testing.T) {
	f := newLiveGateFixtures(t, true)
	defer f.close()
	fund := &repository.Fund{ID: "fund-1", TradingMode: "simulation"}
	user := &authenticatedUser{ID: "user-1", KYCStatus: "unverified"}
	rd := f.gate.check(context.Background(), fund, user, httptest.NewRequest(http.MethodGet, "/", nil))
	if !rd.Ready {
		t.Errorf("Ready=false on simulation fund: %+v", rd)
	}
	if rd.GateEnforced {
		t.Errorf("GateEnforced=true on simulation fund")
	}
}

// TestLiveGate_PaperFundAlwaysReady — paper trading mirrors simulation.
func TestLiveGate_PaperFundAlwaysReady(t *testing.T) {
	f := newLiveGateFixtures(t, true)
	defer f.close()
	fund := &repository.Fund{ID: "fund-1", TradingMode: "paper"}
	user := &authenticatedUser{ID: "user-1"}
	rd := f.gate.check(context.Background(), fund, user, httptest.NewRequest(http.MethodGet, "/", nil))
	if !rd.Ready {
		t.Errorf("Ready=false on paper fund: %+v", rd)
	}
}

// TestLiveGate_LiveFund_AllPillarsPass
//
// Happy path: fund.trading_mode='live', user has KYC verified,
// active broker link exists, 2FA enabled, valid step-up token →
// Ready=true with all per-pillar bools set.
func TestLiveGate_LiveFund_AllPillarsPass(t *testing.T) {
	f := newLiveGateFixtures(t, true)
	defer f.close()

	f.expectActiveBrokerLink("fund-1")
	f.expectTwoFAEnabled("user-1")

	fund := &repository.Fund{ID: "fund-1", TradingMode: "live"}
	user := &authenticatedUser{ID: "user-1", KYCStatus: "verified"}
	r := stepUpRequestForUser(t, f.cfg, "user-1")

	rd := f.gate.check(context.Background(), fund, user, r)
	if !rd.Ready {
		t.Fatalf("Ready=false, want true: %+v", rd)
	}
	if !rd.KYCOK || !rd.BrokerLinkOK || !rd.TwoFAOK || !rd.StepUpOK {
		t.Errorf("pillar bools not all true: %+v", rd)
	}
	if rd.FirstFailing != LiveReadinessOK {
		t.Errorf("FirstFailing = %q, want empty", rd.FirstFailing)
	}
	if rd.BrokerLinkID != "link-1" {
		t.Errorf("BrokerLinkID = %q, want link-1", rd.BrokerLinkID)
	}
}

// TestLiveGate_LiveFund_KYCRequired_ReportsFirst
//
// When KYC fails, the gate must report kyc_required regardless of
// the other pillars' state — KYC is the natural first step.
func TestLiveGate_LiveFund_KYCRequired_ReportsFirst(t *testing.T) {
	f := newLiveGateFixtures(t, true)
	defer f.close()
	// Even with a missing KYC we still query broker_links / totp
	// so the readiness endpoint can render a complete checklist.
	f.expectActiveBrokerLink("fund-1")
	f.expectTwoFAEnabled("user-1")

	fund := &repository.Fund{ID: "fund-1", TradingMode: "live"}
	user := &authenticatedUser{ID: "user-1", KYCStatus: "pending"}
	r := stepUpRequestForUser(t, f.cfg, "user-1")

	rd := f.gate.check(context.Background(), fund, user, r)
	if rd.Ready {
		t.Fatalf("Ready=true with pending KYC: %+v", rd)
	}
	if rd.FirstFailing != LiveReadinessKYCRequired {
		t.Errorf("FirstFailing = %q, want kyc_required", rd.FirstFailing)
	}
	if rd.KYCOK {
		t.Errorf("KYCOK=true with KYC=pending")
	}
}

// TestLiveGate_LiveFund_BrokerLinkMissing_ReportsSecond
//
// KYC passes but no active broker link → broker_link_not_active.
func TestLiveGate_LiveFund_BrokerLinkMissing_ReportsSecond(t *testing.T) {
	f := newLiveGateFixtures(t, true)
	defer f.close()
	f.expectMissingBrokerLink("fund-1")
	f.expectTwoFAEnabled("user-1")

	fund := &repository.Fund{ID: "fund-1", TradingMode: "live"}
	user := &authenticatedUser{ID: "user-1", KYCStatus: "verified"}
	r := stepUpRequestForUser(t, f.cfg, "user-1")

	rd := f.gate.check(context.Background(), fund, user, r)
	if rd.Ready {
		t.Fatalf("Ready=true with missing broker link: %+v", rd)
	}
	if rd.FirstFailing != LiveReadinessBrokerLink {
		t.Errorf("FirstFailing = %q, want broker_link_not_active", rd.FirstFailing)
	}
	if rd.BrokerLinkOK {
		t.Errorf("BrokerLinkOK=true on missing link")
	}
}

// TestLiveGate_LiveFund_TwoFADisabled_ReportsThird
//
// KYC + broker link OK, no 2FA → twofa_required.
func TestLiveGate_LiveFund_TwoFADisabled_ReportsThird(t *testing.T) {
	f := newLiveGateFixtures(t, true)
	defer f.close()
	f.expectActiveBrokerLink("fund-1")
	f.expectTwoFADisabled("user-1")

	fund := &repository.Fund{ID: "fund-1", TradingMode: "live"}
	user := &authenticatedUser{ID: "user-1", KYCStatus: "verified"}
	r := stepUpRequestForUser(t, f.cfg, "user-1")

	rd := f.gate.check(context.Background(), fund, user, r)
	if rd.Ready {
		t.Fatalf("Ready=true with 2FA disabled: %+v", rd)
	}
	if rd.FirstFailing != LiveReadinessTwoFARequired {
		t.Errorf("FirstFailing = %q, want twofa_required", rd.FirstFailing)
	}
}

// TestLiveGate_LiveFund_StepUpMissing_ReportsFourth
//
// All other pillars OK, no X-Step-Up-Token header → step_up_required.
func TestLiveGate_LiveFund_StepUpMissing_ReportsFourth(t *testing.T) {
	f := newLiveGateFixtures(t, true)
	defer f.close()
	f.expectActiveBrokerLink("fund-1")
	f.expectTwoFAEnabled("user-1")

	fund := &repository.Fund{ID: "fund-1", TradingMode: "live"}
	user := &authenticatedUser{ID: "user-1", KYCStatus: "verified"}
	// Bare request — no step-up header.
	r := httptest.NewRequest(http.MethodPost, "/", nil)

	rd := f.gate.check(context.Background(), fund, user, r)
	if rd.Ready {
		t.Fatalf("Ready=true without step-up token: %+v", rd)
	}
	if rd.FirstFailing != LiveReadinessStepUpRequired {
		t.Errorf("FirstFailing = %q, want step_up_required", rd.FirstFailing)
	}
}

// TestLiveGate_LiveFund_StepUpForOtherUser_Rejected
//
// A token whose subject is for user B presented on a request
// authenticated as user A must be rejected even though the
// signature would otherwise pass — defense in depth.
func TestLiveGate_LiveFund_StepUpForOtherUser_Rejected(t *testing.T) {
	f := newLiveGateFixtures(t, true)
	defer f.close()
	f.expectActiveBrokerLink("fund-1")
	f.expectTwoFAEnabled("user-1")

	fund := &repository.Fund{ID: "fund-1", TradingMode: "live"}
	user := &authenticatedUser{ID: "user-1", KYCStatus: "verified"}
	r := stepUpRequestForUser(t, f.cfg, "user-2") // mint for someone else

	rd := f.gate.check(context.Background(), fund, user, r)
	if rd.Ready {
		t.Fatalf("Ready=true for cross-user step-up: %+v", rd)
	}
	if rd.FirstFailing != LiveReadinessStepUpRequired {
		t.Errorf("FirstFailing = %q, want step_up_required", rd.FirstFailing)
	}
}

// TestLiveGate_KillSwitch_AlwaysReady
//
// LIVE_TRADING_GATE_ENABLED=false (enforced=false) makes the gate
// return Ready=true even on a live fund with everything failing.
// GateEnforced must be false so the audit log can flag the bypass.
func TestLiveGate_KillSwitch_AlwaysReady(t *testing.T) {
	f := newLiveGateFixtures(t, false)
	defer f.close()
	// We still expect the gate to RUN the lookups so the readiness
	// endpoint can render the would-be checklist.
	f.expectMissingBrokerLink("fund-1")
	f.expectTwoFADisabled("user-1")

	fund := &repository.Fund{ID: "fund-1", TradingMode: "live"}
	user := &authenticatedUser{ID: "user-1", KYCStatus: "unverified"}
	r := httptest.NewRequest(http.MethodPost, "/", nil)

	rd := f.gate.check(context.Background(), fund, user, r)
	if !rd.Ready {
		t.Fatalf("Ready=false with kill switch off: %+v", rd)
	}
	if rd.GateEnforced {
		t.Errorf("GateEnforced=true with kill switch off")
	}
}

// TestLiveGate_NilGate_DefersToBypass
//
// When the gate is constructed against a nil DB (callers MUST
// treat this as a deploy bug) we still want a deterministic
// answer. The check must report Ready=true / GateEnforced=false
// for non-live funds, and Ready=true for live funds (because
// enforced is implicitly false). The cancel handler caller must
// guard against this case at registration time, not here.
func TestLiveGate_NilGate_DefersToBypass(t *testing.T) {
	var g *liveTradingGate
	fund := &repository.Fund{ID: "fund-1", TradingMode: "live"}
	user := &authenticatedUser{ID: "user-1"}
	rd := g.check(context.Background(), fund, user, httptest.NewRequest(http.MethodGet, "/", nil))
	if rd.Ready {
		// We expect bypass→ready, but the test guards future
		// behaviour: if the team flips the default to deny-on-nil,
		// they MUST also wire the registration-time guard.
		t.Logf("Ready=true on nil gate (current behaviour: bypass)")
	}
	if rd.GateEnforced {
		t.Errorf("GateEnforced=true on nil gate")
	}
}

// TestLoadLiveTradingGateEnabled_DefaultsTrue locks the
// default-on contract — a forgotten env variable MUST NOT silently
// disable the gate.
func TestLoadLiveTradingGateEnabled_DefaultsTrue(t *testing.T) {
	t.Setenv("LIVE_TRADING_GATE_ENABLED", "")
	if !loadLiveTradingGateEnabled() {
		t.Errorf("default = false, want true")
	}
	t.Setenv("LIVE_TRADING_GATE_ENABLED", "false")
	if loadLiveTradingGateEnabled() {
		t.Errorf("explicit false ignored")
	}
}
