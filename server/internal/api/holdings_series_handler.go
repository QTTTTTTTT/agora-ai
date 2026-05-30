package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
)

// HoldingSeriesDTO is one holding's normalized close-price line
// over the requested window.
//
// Same shape as BenchmarkSeriesDTO so the Web layer can reuse the
// same Chart component to render either side. We deliberately
// don't merge the two types — a per-holding chart and a per-
// benchmark chart have different click-through navigation rules
// and the slight redundancy lets them evolve independently.
type HoldingSeriesDTO struct {
	InstrumentKey string             `json:"instrumentKey"`
	Symbol        string             `json:"symbol"`
	Name          string             `json:"name,omitempty"`
	Market        string             `json:"market,omitempty"`
	// EntryPrice is the holding's cost basis at the start of the
	// window. Used by the UI to render an entry-line annotation
	// and compute a "vs entry" delta. Zero means cost basis was
	// unknown at fetch time (legacy holdings); the UI hides the
	// annotation in that case.
	EntryPrice float64            `json:"entryPrice"`
	Points     []BenchmarkPointDTO `json:"points"`
}

// HoldingsSeriesResponse is the envelope for
// GET /api/funds/{fundId}/holdings/series.
//
// Each item is independently rebased to start = 100. We don't
// project them onto a single shared start day because the holdings
// table can mix instruments that have different first-trading-day
// constraints (a recent IPO vs. a long-listed name).
type HoldingsSeriesResponse struct {
	FundID string             `json:"fundId"`
	From   string             `json:"from"`
	To     string             `json:"to"`
	Items  []HoldingSeriesDTO `json:"items"`
	// PartialFailures lists holdings whose price history couldn't be
	// fetched. The UI shows a small banner rather than blanking the
	// whole grid.
	PartialFailures []BenchmarkPartialFailure `json:"partialFailures,omitempty"`
}

// HoldingsSeriesService is the service-layer contract behind the
// holdings-series endpoint. nil-safe: handler returns 503 when
// unset so deployments without ohlc providers stay healthy.
type HoldingsSeriesService interface {
	HoldingsSeries(ctx context.Context, userID, fundID string, days int) (HoldingsSeriesResponse, error)
}

// WithHoldingsSeriesService wires the service. Idempotent.
func (h *FundHandler) WithHoldingsSeriesService(svc HoldingsSeriesService) *FundHandler {
	if h != nil {
		h.holdingsSeries = svc
	}
	return h
}

// GetHoldingsSeries implements
//
//	GET /api/funds/{fundId}/holdings/series?days=N
//
// Behaviour mirrors GetBenchmarkHistory — soft clamping on `days`,
// 503 when the service is unwired, fund-membership enforced server-
// side via the canonical ErrForbidden mapping.
func (h *FundHandler) GetHoldingsSeries(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	if h.holdingsSeries == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "holdings series service unavailable",
		})
		return
	}

	days := 90
	if raw := strings.TrimSpace(r.URL.Query().Get("days")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			switch {
			case parsed < 7:
				days = 7
			case parsed > 1825:
				days = 1825
			default:
				days = parsed
			}
		}
	}

	resp, err := h.holdingsSeries.HoldingsSeries(r.Context(), userID, fundID, days)
	if err != nil {
		handleServiceError(w, err, "holdings series")
		return
	}
	if resp.Items == nil {
		resp.Items = []HoldingSeriesDTO{}
	}
	writeJSON(w, http.StatusOK, resp)
}
