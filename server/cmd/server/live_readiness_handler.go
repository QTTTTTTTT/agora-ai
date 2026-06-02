// Live-readiness HTTP handler (P0-9).
//
// Exposes:
//
//	GET /api/funds/{fundId}/live-readiness
//
// Returns the fund's readiness for live trading — i.e. which of
// the four pillars (KYC, broker_link, 2FA, step-up) currently
// pass. The UI uses the response to render a checklist that
// guides the user through the missing steps.
//
// Why a separate file
//
// The order_actions_handler is the WRITE side of the gate;
// readiness is the READ side. Putting both in one file would
// blur the separation and force tests for one to pull in
// dependencies of the other.
//
// Security
//
// Authenticated; requires fund-ownership (same authorizeFundAccess
// guard as cancel/replace). We do NOT short-circuit on missing
// step-up — the UI calls this endpoint BEFORE prompting for
// biometrics, so step_up_ok=false is a normal answer. The
// handler honours an optional `?step_up_token=...` URL
// parameter for clients that prefer not to set the
// X-Step-Up-Token header on a GET (some browsers strip
// non-standard headers from same-site GETs in cached page
// navigations); the helper still falls back to the header when
// the query param is absent.

package main

import (
	"net/http"
	"strings"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/repository"
)

type liveReadinessHandler struct {
	fundRepo    *repository.FundRepo
	companyRepo *repository.FundCompanyRepo
	gate        *liveTradingGate
	cfg         *Config
	svc         *Services
}

// newLiveReadinessHandler returns nil when the wiring is missing —
// matches the rest of the handler-registration pattern in main.go.
func newLiveReadinessHandler(svc *Services, cfg *Config, gate *liveTradingGate) *liveReadinessHandler {
	if svc == nil || svc.DB == nil {
		return nil
	}
	return &liveReadinessHandler{
		fundRepo:    repository.NewFundRepo(svc.DB),
		companyRepo: repository.NewFundCompanyRepo(svc.DB),
		gate:        gate,
		cfg:         cfg,
		svc:         svc,
	}
}

func (h *liveReadinessHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/funds/{fundId}/live-readiness", h.handle)
}

// liveReadinessResponse is the JSON shape the frontend consumes.
// Mirrors the fields of LiveReadiness with snake_case for the
// wire — matches the rest of our REST conventions.
type liveReadinessResponse struct {
	TradingMode      string `json:"trading_mode"`
	Ready            bool   `json:"ready"`
	GateEnforced     bool   `json:"gate_enforced"`
	KYCOK            bool   `json:"kyc_ok"`
	BrokerLinkOK     bool   `json:"broker_link_ok"`
	TwoFAOK          bool   `json:"two_fa_ok"`
	StepUpOK         bool   `json:"step_up_ok"`
	FirstFailing     string `json:"first_failing,omitempty"`
	BrokerLinkID     string `json:"broker_link_id,omitempty"`
	StepUpUserID     string `json:"step_up_user_id,omitempty"`
}

func (h *liveReadinessHandler) handle(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fundId required"))
		return
	}
	ctx := r.Context()
	fund, err := authorizeFundAccess(ctx, h.fundRepo, h.companyRepo, userID, fundID)
	if err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}
	// Step-up: prefer the header (which the cancel/replace path
	// uses) but fall back to a query parameter so a freshly-
	// minted token from /api/auth/step-up can be probed without
	// needing a custom header on a same-site GET.
	if r.Header.Get(stepUpHeader) == "" {
		if q := strings.TrimSpace(r.URL.Query().Get("step_up_token")); q != "" {
			r.Header.Set(stepUpHeader, q)
		}
	}

	var rd LiveReadiness
	if h.gate != nil {
		// Same shape as order_actions_handler.checkLiveGate but
		// without the deny-by-default user fallback — the
		// readiness endpoint is informational, so a missing user
		// row degrades to "kyc unknown" rather than synthesizing
		// a permissive identity.
		user, lookupErr := loadActiveUserByID(ctx, h.gate.db, userID)
		if lookupErr != nil {
			user = &authenticatedUser{ID: userID}
		}
		rd = h.gate.check(ctx, fund, user, r)
	} else {
		// Gate not wired — return the simulation-mode shape so
		// the UI doesn't render a misleading "ready=false".
		rd = LiveReadiness{Ready: true, TradingMode: fund.TradingMode, GateEnforced: false}
	}

	writeOrderActionJSON(w, http.StatusOK, liveReadinessResponse{
		TradingMode:  rd.TradingMode,
		Ready:        rd.Ready,
		GateEnforced: rd.GateEnforced,
		KYCOK:        rd.KYCOK,
		BrokerLinkOK: rd.BrokerLinkOK,
		TwoFAOK:      rd.TwoFAOK,
		StepUpOK:     rd.StepUpOK,
		FirstFailing: string(rd.FirstFailing),
		BrokerLinkID: rd.BrokerLinkID,
		StepUpUserID: rd.StepUpUserID,
	})
}
