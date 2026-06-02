// fund_settings.go — fund-level settings endpoints (P1-4).
//
// Hosts non-trading fund settings that are too small to warrant
// their own handler file but too distinct from the bulk fund
// CRUD to bolt onto api.fundHandler.
//
// Routes
//
//   POST /api/funds/{fundId}/settings/base-currency
//
// AuthZ
//
//   - The caller must own the fund (authorizeFundAccess).
//   - The new currency must be in fx.SupportedCurrencies — the DB
//     CHECK is the last line of defence; we want to fail at the
//     handler with a structured error so the UI can render an
//     inline message rather than a 500.
//
// Audit
//
//   Every base-currency change writes a audit.MutationEvent so
//   the hash chain captures who flipped a fund's reporting
//   currency and when. Down the line this matters because moving
//   from USD → CNY changes every NAV row by ~7×.

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/fx"
	"github.com/fundai/server/internal/repository"
)

type fundSettingsHandler struct {
	fundRepo    *repository.FundRepo
	companyRepo *repository.FundCompanyRepo
	auditLogger audit.Logger
}

func newFundSettingsHandler(svc *Services) *fundSettingsHandler {
	if svc == nil || svc.DB == nil {
		return nil
	}
	return &fundSettingsHandler{
		fundRepo:    repository.NewFundRepo(svc.DB),
		companyRepo: repository.NewFundCompanyRepo(svc.DB),
		auditLogger: audit.NewDBLogger(svc.DB),
	}
}

func (h *fundSettingsHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("POST /api/funds/{fundId}/settings/base-currency", h.handleSetBaseCurrency)
}

type setBaseCurrencyRequest struct {
	BaseCurrency string `json:"base_currency"`
}

func (h *fundSettingsHandler) handleSetBaseCurrency(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing token"))
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fundId required"))
		return
	}
	ctx := r.Context()
	if _, err := authorizeFundAccess(ctx, h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}
	var req setBaseCurrencyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	cur := strings.ToUpper(strings.TrimSpace(req.BaseCurrency))
	if !fx.IsSupported(cur) {
		writeOrderActionJSON(w, http.StatusBadRequest,
			errorPayload("invalid_currency", "base_currency must be one of "+strings.Join(fx.SupportedCurrencies, ", ")))
		return
	}
	prev, err := h.fundRepo.GetBaseCurrency(ctx, fundID)
	if err != nil {
		// Soft-fail: for the audit "before" snapshot. If we can't
		// read it, fall back to a placeholder rather than failing
		// the whole flow — the UPDATE below is the source of truth.
		prev = ""
	}
	if cur == prev {
		// No-op — return early to keep the audit log clean.
		writeOrderActionJSON(w, http.StatusOK, map[string]any{"ok": true, "noop": true})
		return
	}
	if err := h.fundRepo.SetBaseCurrency(ctx, fundID, cur); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			writeOrderActionJSON(w, http.StatusNotFound, errorPayload("fund_not_found", err.Error()))
			return
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(ctx, audit.MutationEvent{
			ActorUserID: userID,
			Action:      "fund.base_currency.update",
			TargetType:  "fund",
			TargetID:    fundID,
			Before:      map[string]any{"base_currency": prev},
			After:       map[string]any{"base_currency": cur},
		})
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"ok": true})
}
