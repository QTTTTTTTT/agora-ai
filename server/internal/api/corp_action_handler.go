package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CorpActionService is the service-layer contract the FundHandler
// uses to surface a fund's corporate-action timeline. The interface
// is intentionally narrow — list-only — because that is the entire
// user-facing surface for this feature; admin write paths live in
// cmd/server.
//
// Implementations are nil-safe at the handler boundary (the
// GetCorpActions endpoint returns 503 when the field is unset),
// so deployments without corp-action wiring degrade cleanly.
type CorpActionService interface {
	// ApplicationsForFund returns the most-recent N corporate
	// actions that have been applied to the fund's holdings.
	// The userID parameter exists so the implementation can run
	// fund-membership checks (return ErrForbidden when the user
	// is not in the fund's company).
	//
	// Newest-first ordering. Limit is capped server-side; pass
	// 0 to use the default (50).
	ApplicationsForFund(ctx context.Context, userID, fundID string, limit int) ([]CorpActionApplicationDTO, error)
}

// CorpActionApplicationDTO is the JSON wire shape of a single
// per-fund application receipt. Fields mirror the columns of
// corp_action_applications + the parent corporate_actions row,
// flattened so the UI doesn't have to JOIN on the client.
type CorpActionApplicationDTO struct {
	InstrumentKey string    `json:"instrumentKey"`
	ExDate        time.Time `json:"exDate"`
	ActionType    string    `json:"actionType"` // split | cash_dividend | stock_dividend | combined
	SplitRatio    float64   `json:"splitRatio"`
	CashDividend  float64   `json:"cashDividend"`
	AppliedAt     time.Time `json:"appliedAt"`
	PreQuantity   float64   `json:"preQuantity"`
	PostQuantity  float64   `json:"postQuantity"`
	PreCostPrice  float64   `json:"preCostPrice"`
	PostCostPrice float64   `json:"postCostPrice"`
	CashCredit    float64   `json:"cashCredit"`
}

// WithCorpActionService injects the corp-action service. Idempotent.
// nil disables the GetCorpActions endpoint (handler returns 503
// without consulting any backing store).
func (h *FundHandler) WithCorpActionService(svc CorpActionService) *FundHandler {
	if h != nil {
		h.corpActions = svc
	}
	return h
}

// GetCorpActions implements GET /api/funds/{fundId}/corp-actions.
//
// Returns the per-fund timeline of split / dividend events that
// have been mathematically applied to the fund's holdings, so the
// UI can answer "what corp action moved my cost basis on this date?".
//
// Authorisation:
//   - Bearer token required (handled by the existing auth middleware
//     and surfaced via requireAuthenticatedUserID).
//   - The service call enforces fund-membership; non-members get a
//     forbidden response via handleServiceError.
//
// Query string:
//   - limit (optional, default 50, max 200) — page size cap.
func (h *FundHandler) GetCorpActions(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	if h.corpActions == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "corp action service unavailable",
		})
		return
	}

	// limit is parsed defensively — anything non-numeric or out of
	// range falls back to the default. The service applies its own
	// hard cap so a manipulated query string can't overstress the
	// repository layer.
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 200 {
			limit = parsed
		}
	}

	items, err := h.corpActions.ApplicationsForFund(r.Context(), userID, fundID, limit)
	if err != nil {
		handleServiceError(w, err, "corp actions")
		return
	}
	if items == nil {
		items = []CorpActionApplicationDTO{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"count": len(items),
	})
}

// errCorpActionForbidden is the canonical sentinel adapters can wrap
// when a user doesn't have access to the fund. It maps to 403 via
// handleServiceError's forbidden branch.
var errCorpActionForbidden = errors.New("corp action: forbidden")
