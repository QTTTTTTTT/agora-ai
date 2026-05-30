package api

import (
	"context"
	"net/http"
)

// ABOperationalAttributionService is the read-only contract
// behind GET /api/abtests/{testId}/operational-attribution.
//
// It pivots ab_test_variant_trades into a per-symbol
// attribution table so the comparison UI can answer
// "on which tickers did B out-trade A, and by how much?".
// All maths happens in the implementation — the wire
// shape is already aggregation-ready.
//
// Permissions: implementations MUST authorise the caller
// against BOTH the control fund and the treatment fund of
// the test (matches the existing AB pattern).
//
// nil-safety: the handler returns 503 when this field is
// not wired so legacy deployments degrade cleanly.
type ABOperationalAttributionService interface {
	OperationalAttribution(ctx context.Context, userID, testID string) (ABTestOperationalAttribution, error)
}

// ABTestOperationalAttribution is the wire envelope.
// BySymbol is bounded server-side (top 50 by |gap|) so
// the response is safe to render in a table without
// virtualisation; clients that want more should paginate
// through trades directly.
type ABTestOperationalAttribution struct {
	TestID   string                       `json:"testId"`
	TotalA   ABAttributionTotals          `json:"totalA"`
	TotalB   ABAttributionTotals          `json:"totalB"`
	BySymbol []ABAttributionSymbolRow     `json:"bySymbol"`
}

// ABAttributionTotals is the variant-level rollup. WinTradeRate
// is in [0, 1]; AvgPnL is realized PnL / trade count (0 when
// no trades). Turnover is the absolute notional sum.
type ABAttributionTotals struct {
	TradeCount   int     `json:"tradeCount"`
	Turnover     float64 `json:"turnover"`
	RealizedPnL  float64 `json:"realizedPnL"`
	WinTradeRate float64 `json:"winTradeRate"`
	AvgPnL       float64 `json:"avgPnL"`
}

// ABAttributionSymbolRow is one symbol's A vs B side-by-side.
// PnLGap is B − A (positive = B did better); Winner is the
// canonical "A" / "B" / "tie" string the UI uses for badges.
//
// GapPctOfNotional normalises the absolute gap by max(turnover)
// so small-notional trades don't get inflated to the top of the
// "biggest gap" sort.
type ABAttributionSymbolRow struct {
	Symbol           string  `json:"symbol"`
	TradeCountA      int     `json:"tradeCountA"`
	TradeCountB      int     `json:"tradeCountB"`
	RealizedPnLA     float64 `json:"realizedPnLA"`
	RealizedPnLB     float64 `json:"realizedPnLB"`
	TurnoverA        float64 `json:"turnoverA"`
	TurnoverB        float64 `json:"turnoverB"`
	PnLGap           float64 `json:"pnlGap"`
	GapPctOfNotional float64 `json:"gapPctOfNotional"`
	Winner           string  `json:"winner"`
}

// WithABOperationalAttributionService injects the
// attribution service. Idempotent. Safe to call with
// nil to disable the endpoint (handler returns 503).
func (h *FundHandler) WithABOperationalAttributionService(svc ABOperationalAttributionService) *FundHandler {
	if h != nil {
		h.abAttribution = svc
	}
	return h
}

// GetABOperationalAttribution implements
//
//	GET /api/abtests/{testId}/operational-attribution
//
// Same auth + 503 + 404 + 403 contract as the other AB
// read endpoints (GetABShadowAgents, ListABTestLearningPromotions).
// No query parameters today — the response is fully
// server-paged.
func (h *FundHandler) GetABOperationalAttribution(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	testID := pathValue(r, "testId")
	if !requireNonEmpty(w, testID, "testId") {
		return
	}
	if h.abAttribution == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "ab attribution service unavailable",
		})
		return
	}

	resp, err := h.abAttribution.OperationalAttribution(r.Context(), userID, testID)
	if err != nil {
		handleServiceError(w, err, "A/B operational attribution")
		return
	}
	if resp.BySymbol == nil {
		resp.BySymbol = []ABAttributionSymbolRow{}
	}
	writeJSON(w, http.StatusOK, resp)
}
