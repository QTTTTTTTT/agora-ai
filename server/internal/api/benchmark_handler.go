package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// BenchmarkSeriesDTO is the JSON wire shape for one benchmark series
// in a benchmark-history response. The points slice is already
// normalized to start = 100, so the UI can plot multiple series
// without further transform.
type BenchmarkSeriesDTO struct {
	ID       string             `json:"id"`
	Label    string             `json:"label"`
	Symbol   string             `json:"symbol"`
	Market   string             `json:"market"`
	Currency string             `json:"currency,omitempty"`
	Points   []BenchmarkPointDTO `json:"points"`
}

// BenchmarkPointDTO is one (date, value) pair on a normalized series.
// Date is ISO yyyy-mm-dd (UTC calendar day), value is the rebased
// scalar (start = 100).
type BenchmarkPointDTO struct {
	Date  string  `json:"date"`
	Value float64 `json:"value"`
}

// BenchmarkHistoryResponse is the envelope GET /benchmark-history
// returns. The fund block carries the fund's own normalized NAV
// curve so the UI can draw fund + benchmarks in a single component
// without having to align two separate API calls on the client.
//
// Recommended is the catalog-derived default selection for the
// fund's profile; the UI treats it as a hint when the user hasn't
// actively picked a benchmark yet.
type BenchmarkHistoryResponse struct {
	FundID      string                 `json:"fundId"`
	From        string                 `json:"from"`
	To          string                 `json:"to"`
	Fund        BenchmarkSeriesDTO     `json:"fund"`
	Benchmarks  []BenchmarkSeriesDTO   `json:"benchmarks"`
	Recommended []string               `json:"recommended"`
	Available   []BenchmarkCatalogItem `json:"available"`
	// PartialFailures lists benchmark IDs the caller asked for but
	// for which fetching failed. The UI surfaces these as a small
	// "couldn't load X" toast rather than failing the whole panel.
	PartialFailures []BenchmarkPartialFailure `json:"partialFailures,omitempty"`
	// HoldingOverlap is set when the fund's actual positions
	// substantially overlap one or more of the rendered benchmarks
	// — e.g., a futures / crypto fund whose dominant holding is
	// BTCUSDT while the primary benchmark is btc_usdt. In that
	// case the fund-curve and the benchmark-curve will track each
	// other tightly in the "compare" mode, which makes the chart
	// look uninformative ("did the fund move at all?"). The UI
	// uses this hint to surface a banner pointing users at the
	// Alpha view, where the structural overlap is differenced
	// out and only the residual outperformance shows. Absent
	// (omitempty) when there is no overlap.
	HoldingOverlap *BenchmarkHoldingOverlap `json:"holdingOverlap,omitempty"`
}

// BenchmarkCatalogItem is a flattened benchmark catalog row, used
// to populate the "available benchmarks" picker in the UI without
// the client having to maintain a duplicate catalog.
type BenchmarkCatalogItem struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Symbol string `json:"symbol"`
	Market string `json:"market"`
}

// BenchmarkPartialFailure tells the UI which series couldn't be
// fetched and why. We deliberately don't include the upstream
// error verbatim — that would leak provider names. The reason is
// a UI-friendly, locale-independent code.
type BenchmarkPartialFailure struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// BenchmarkHoldingOverlap captures which benchmarks the fund's own
// positions overlap with, and how dominant that overlap is.
//
// Why this matters in the UI: when a futures fund is 100% BTC and
// the recommended benchmark is btc_usdt, the "compare" line chart
// shows two lines that track each other almost perfectly — visually
// flat, hard to read, and offering little information beyond
// "yes, BTC moved". The Alpha view (fund − benchmark) cancels the
// shared component and is then the right default. We surface the
// signal as structured data rather than a free-form text hint so
// the web AND the React-Native fund-overview can render it
// consistently (both use shared/api-client).
//
// Field semantics:
//
//   - PrimaryBenchmark: the benchmark ID whose Symbol most closely
//     matches a holding (case- and suffix-insensitive). Empty when
//     no benchmark overlaps any holding.
//   - OverlapStrength:  "dominant" when the matched holding is the
//     fund's largest by quantity (proxy for share of NAV); "partial"
//     when the holding exists but isn't dominant. We don't expose
//     a precise %-of-NAV because computing that here would require
//     looking up the latest mark, which is more complexity than
//     this hint deserves.
//   - MatchedSymbols:   verbatim symbols from fund holdings that
//     matched one of the benchmark symbols. Useful for debugging
//     / explanatory copy ("BTCUSDT in your fund matches the
//     btc_usdt benchmark").
type BenchmarkHoldingOverlap struct {
	PrimaryBenchmark string   `json:"primaryBenchmark"`
	OverlapStrength  string   `json:"overlapStrength"`
	MatchedSymbols   []string `json:"matchedSymbols,omitempty"`
}

// BenchmarkService is the service-layer contract behind the
// benchmark-history endpoint. nil-safe: the handler returns 503
// when unset so deployments without the wiring stay functional.
//
// The userID is for fund-membership checks; the implementation maps
// non-members to the canonical ErrForbidden sentinel.
type BenchmarkService interface {
	History(ctx context.Context, userID, fundID string, days int, ids []string) (BenchmarkHistoryResponse, error)
}

// WithBenchmarkService wires the service. Idempotent.
func (h *FundHandler) WithBenchmarkService(svc BenchmarkService) *FundHandler {
	if h != nil {
		h.benchmarks = svc
	}
	return h
}

// GetBenchmarkHistory implements
//
//	GET /api/funds/{fundId}/benchmark-history?days=N&series=spx,csi300
//
// Behaviour:
//
//   - Unauthenticated → 401 (uniform across fund-scope endpoints).
//   - Service unwired → 503 (deployments without ohlc providers).
//   - days unspecified or out of range → silently clamped to [7, 1825].
//     We intentionally don't error on a malformed value — the chart
//     panel is a soft surface and erroring would gate the whole
//     dashboard on a bad query string.
//   - series unspecified → service applies its own recommended
//     default (fund-profile-aware).
//   - series with unknown IDs → those are dropped silently and
//     listed in PartialFailures so the UI can show a small toast.
func (h *FundHandler) GetBenchmarkHistory(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	if h.benchmarks == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "benchmark service unavailable",
		})
		return
	}

	days := 90
	if raw := strings.TrimSpace(r.URL.Query().Get("days")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			switch {
			case parsed < 7:
				days = 7
			case parsed > 1825: // 5y hard cap — provider request size guardrail
				days = 1825
			default:
				days = parsed
			}
		}
	}

	ids := parseSeriesQuery(r.URL.Query().Get("series"))

	resp, err := h.benchmarks.History(r.Context(), userID, fundID, days, ids)
	if err != nil {
		handleServiceError(w, err, "benchmark history")
		return
	}
	// Defensive nil-to-empty so the JSON doesn't carry `null` arrays.
	if resp.Benchmarks == nil {
		resp.Benchmarks = []BenchmarkSeriesDTO{}
	}
	if resp.Recommended == nil {
		resp.Recommended = []string{}
	}
	if resp.Available == nil {
		resp.Available = []BenchmarkCatalogItem{}
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseSeriesQuery splits the comma-separated `series` query value
// into a clean slice of canonical IDs. We don't validate against
// the catalog here — the service layer drops unknown ones and
// surfaces them as PartialFailures, which is more informative than
// a 400.
//
// Returns nil (not []string{}) when the input is empty so the
// service layer can detect "no preference" and fall back to its
// recommendation.
func parseSeriesQuery(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		v := strings.ToLower(strings.TrimSpace(p))
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// errBenchmarkUnknownSeries is the canonical sentinel adapters can
// wrap for "asked for series ids that the catalog doesn't know".
// Currently unused (the service folds unknown ids into
// PartialFailures), but exposed for adapters that want strict mode.
var errBenchmarkUnknownSeries = errors.New("benchmark: unknown series id")

// FormatDateForBenchmark is exposed so the wiring adapter can format
// dates consistently with what this DTO layer expects. Centralizing
// the format string here prevents drift between the producer and
// the consumer.
func FormatDateForBenchmark(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}
