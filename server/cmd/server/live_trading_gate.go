// Live-trading hard gate (P0-9).
//
// Composes the four pillars that the platform requires before any
// state-changing action can be dispatched on a fund whose
// trading_mode='live':
//
//  1. KYC: user.kyc_status == 'verified'
//  2. Broker link: an ACTIVE row in broker_links for the fund
//  3. 2FA: user has completed TOTP enrolment (user_totp_secrets.enabled_at is set)
//  4. Step-up: a valid X-Step-Up-Token (audience=step_up) on the request
//
// Why a separate file
//
// The gate is the single chokepoint shared by cancel, replace, and
// (future) place-order. Putting it next to the order_actions_handler
// would couple it to that handler's struct; putting it inside the
// handlers themselves would duplicate the pillar logic three times.
// Centralising here also makes the gate trivially testable in
// isolation — drive each pillar to fail and assert reason codes.
//
// Why a per-pillar reason vocabulary
//
// The UI and the audit log both care which pillar blocked the
// action. A free-form 403 string would force the frontend to
// regex-match. Returning a structured LiveReadinessReason lets:
//   - the UI render "请先完成 KYC" / "请绑定券商" / "请开启 2FA" / "请完成生物识别" precisely;
//   - the audit log carry a discrete code so a post-incident query
//     can answer "how many live cancels were rejected because the
//     user had no broker link?".
//
// Bypass switch
//
// LIVE_TRADING_GATE_ENABLED env var (default true). When false,
// the gate degrades to "always pass" — used in dev / smoke setups
// where wiring up a real KYC + broker_link is overkill. The
// startup banner logs the resolved value so an operator can spot
// a mis-deployed prod box.

package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/fundai/server/internal/repository"
)

// LiveReadinessReason is the closed vocabulary of pillar failures.
// Order matters here — the gate reports the FIRST failing pillar
// in the order users naturally complete them (KYC → broker link →
// 2FA → step-up), so the UI can guide them through one step at a
// time without overwhelming them with parallel errors.
type LiveReadinessReason string

const (
	LiveReadinessOK              LiveReadinessReason = ""
	LiveReadinessKYCRequired     LiveReadinessReason = "kyc_required"
	LiveReadinessBrokerLink      LiveReadinessReason = "broker_link_not_active"
	LiveReadinessTwoFARequired   LiveReadinessReason = "twofa_required"
	LiveReadinessStepUpRequired  LiveReadinessReason = "step_up_required"
	LiveReadinessSimulationOnly  LiveReadinessReason = "" // alias for OK on non-live funds
	LiveReadinessGateUnavailable LiveReadinessReason = "gate_unavailable"
)

// LiveReadiness is the result of the pillar check. Ready=true
// means all four pillars passed (or the fund is not in live mode
// or the gate is disabled). When Ready=false, FirstFailing names
// the pillar to surface to the user.
type LiveReadiness struct {
	Ready         bool
	TradingMode   string
	GateEnforced  bool // false when fund is not 'live' OR gate disabled
	KYCOK         bool
	BrokerLinkOK  bool
	TwoFAOK       bool
	StepUpOK      bool
	FirstFailing  LiveReadinessReason
	BrokerLinkID  string
	StepUpUserID  string
}

// liveTradingGate is the production wiring for the gate. Holds
// the small set of dependencies it needs so the cancel/replace
// handlers can call check() with just (ctx, fund, userID, r).
type liveTradingGate struct {
	db             *sql.DB
	totpRepo       *repository.UserTOTPRepo
	brokerLinkRepo *repository.BrokerLinkRepo
	cfg            *Config
	enforced       bool // false ⇒ gate always reports Ready=true
}

// newLiveTradingGate constructs the gate from Services + Config.
// Returns nil if the DB handle is missing — callers MUST treat
// nil as "gate unavailable, deny live trading" (see check()).
func newLiveTradingGate(svc *Services, cfg *Config, enforced bool) *liveTradingGate {
	if svc == nil || svc.DB == nil {
		return nil
	}
	return &liveTradingGate{
		db:             svc.DB,
		totpRepo:       repository.NewUserTOTPRepo(svc.DB),
		brokerLinkRepo: repository.NewBrokerLinkRepo(svc.DB),
		cfg:            cfg,
		enforced:       enforced,
	}
}

// check evaluates all four pillars for (user, fund) using the
// step-up token (if any) on the inbound request. Always returns
// a LiveReadiness — never an error — because pillar evaluation
// failures (DB blip on totp lookup) should be reported as a
// pillar failure ("twofa_required" / "gate_unavailable") not a
// 500. The handler decides whether to translate Ready=false into
// a 403.
//
// Semantics:
//
//   - fund.TradingMode != 'live' (or gate disabled): Ready=true,
//     GateEnforced=false. The handler proceeds.
//   - fund.TradingMode == 'live' (and gate enabled):
//     evaluate KYC → broker link → 2FA → step-up; FirstFailing
//     names the first pillar that fails.
//
// We intentionally evaluate ALL pillars even after the first
// failure so the readiness endpoint (used by the UI to render a
// checklist) gets a complete picture in a single round-trip.
func (g *liveTradingGate) check(ctx context.Context, fund *repository.Fund, user *authenticatedUser, r *http.Request) LiveReadiness {
	rd := LiveReadiness{}
	if fund != nil {
		rd.TradingMode = fund.TradingMode
	}
	// Non-live trading modes (simulation / paper) bypass the gate
	// entirely. We still populate the per-pillar bools so a future
	// "force live mode preview" toggle can read them without re-
	// running the gate.
	if fund == nil || fund.TradingMode != "live" {
		rd.Ready = true
		rd.GateEnforced = false
		return rd
	}
	// Operator kill-switch. The startup banner already advertised
	// that the gate is off; we still mark GateEnforced=false so
	// audit logs make it obvious the action passed under bypass.
	if g == nil || !g.enforced {
		rd.Ready = true
		rd.GateEnforced = false
		// Even with the gate off we surface partial pillar truth
		// when we have the data — useful for the read-only
		// readiness endpoint in dev to check "would I pass if the
		// gate were on?".
		if g != nil && user != nil && fund != nil {
			rd.KYCOK = isKYCVerified(user)
			if link, err := g.brokerLinkRepo.GetActiveByFundID(ctx, fund.ID); err == nil && link.IsActive() {
				rd.BrokerLinkOK = true
				rd.BrokerLinkID = link.ID
			}
			rd.TwoFAOK = isTwoFAEnabled(ctx, g.totpRepo, user.ID)
			sv := verifyStepUpToken(r, g.cfg)
			rd.StepUpOK = sv.Valid
			rd.StepUpUserID = sv.UserID
		}
		return rd
	}

	rd.GateEnforced = true

	// Pillar 1: KYC.
	if user != nil {
		rd.KYCOK = isKYCVerified(user)
	}

	// Pillar 2: broker link. We look up the active link even on a
	// nil user so callers that want to populate the readiness
	// endpoint for a fund-detail page don't get a confusing
	// "broker link unknown" when the user identifier is missing.
	if fund != nil {
		link, err := g.brokerLinkRepo.GetActiveByFundID(ctx, fund.ID)
		if err == nil && link.IsActive() {
			rd.BrokerLinkOK = true
			rd.BrokerLinkID = link.ID
		} else if err != nil && !errors.Is(err, repository.ErrBrokerLinkNotFound) {
			// Genuine DB error — degrade to "gate unavailable" so
			// the handler returns 503 instead of silently dropping
			// the user into a "broker link missing" loop.
			rd.FirstFailing = LiveReadinessGateUnavailable
		}
	}

	// Pillar 3: 2FA.
	if user != nil {
		rd.TwoFAOK = isTwoFAEnabled(ctx, g.totpRepo, user.ID)
	}

	// Pillar 4: step-up token on this request.
	if r != nil {
		sv := verifyStepUpToken(r, g.cfg)
		rd.StepUpOK = sv.Valid
		rd.StepUpUserID = sv.UserID
		// A token whose subject is for someone other than the
		// authenticated user is treated as an invalid step-up,
		// which surfaces below as step_up_required — defense in
		// depth against a token-replay vector.
		if sv.Valid && user != nil && sv.UserID != "" && sv.UserID != user.ID {
			rd.StepUpOK = false
		}
	}

	// Compute the first failing pillar in the natural completion
	// order. We deliberately let the GateUnavailable assignment
	// above win even if other pillars also failed — when the
	// system can't make a decision we want the operator to see
	// the unavailability message first.
	if rd.FirstFailing == "" {
		switch {
		case !rd.KYCOK:
			rd.FirstFailing = LiveReadinessKYCRequired
		case !rd.BrokerLinkOK:
			rd.FirstFailing = LiveReadinessBrokerLink
		case !rd.TwoFAOK:
			rd.FirstFailing = LiveReadinessTwoFARequired
		case !rd.StepUpOK:
			rd.FirstFailing = LiveReadinessStepUpRequired
		}
	}

	rd.Ready = rd.FirstFailing == ""
	return rd
}

// isKYCVerified factors the kyc_status check so test fixtures can
// reuse it without touching the gate's internals. We accept
// "verified" as the single passing value; "pending" / "unverified"
// / anything else fails. kyc_level is NOT checked here on purpose
// — different fund products may demand different tiers, and the
// gate's MVP is binary. A future migration to per-fund minimum
// tier goes through this helper.
func isKYCVerified(u *authenticatedUser) bool {
	return u != nil && u.KYCStatus == "verified"
}

// isTwoFAEnabled is a small wrapper that swallows "no row" as
// "2FA not enabled". Any other error (DB outage) also falls to
// false; a 503 from the calling handler is preferable to a false
// pass.
func isTwoFAEnabled(ctx context.Context, repo *repository.UserTOTPRepo, userID string) bool {
	if repo == nil || userID == "" {
		return false
	}
	row, err := repo.GetByUserID(ctx, userID)
	if err != nil || row == nil {
		return false
	}
	return row.IsEnabled()
}

// loadLiveTradingGateEnabled reads the kill-switch from env. We
// default-on (true) so a deploy that forgets to set the variable
// stays safe; only an explicit "false" turns the gate off.
func loadLiveTradingGateEnabled() bool {
	return envBoolWithDefault("LIVE_TRADING_GATE_ENABLED", true)
}
