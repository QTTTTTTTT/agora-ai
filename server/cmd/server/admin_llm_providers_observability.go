// admin_llm_providers_observability.go — S14.A endpoints feeding
// the admin "看板" (observability) tab.
//
//	GET /api/admin/llm-providers/health?range=24h
//	    → per-provider health summary over the window + sparkline points
//	GET /api/admin/llm-providers/cost?range=7d
//	    → per-provider totals + per-day series for charts
//	GET /api/admin/llm-providers/{id}/history?range=24h&limit=500
//	    → raw ping rows for a single provider (detailed inspection)
//
// All three are admin-only (requireAdmin) and read-only. Errors that
// would mean "no data yet" (empty tables) intentionally return 200
// with an empty array so the UI doesn't need a separate "not yet
// initialised" state on first boot.

package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (h *adminHandler) registerLLMProviderObservabilityRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	// We register even if the repos are nil — the handler returns
	// empty payloads in that case. This keeps the API contract
	// stable for the frontend (no 404 vs 200 toggling).
	mux.HandleFunc("GET /api/admin/llm-providers/health", h.handleProviderHealthDashboard)
	mux.HandleFunc("GET /api/admin/llm-providers/cost", h.handleProviderCostDashboard)
	mux.HandleFunc("GET /api/admin/llm-providers/{id}/history", h.handleProviderHistory)
}

// ----- helpers ---------------------------------------------------------------

// parseRange parses the ?range= query (e.g. "24h", "7d", "30d").
// Returns the corresponding lookback duration. Unknown values fall
// back to the supplied default so the dashboard can't 400 itself by
// passing a typo'd range.
func parseRange(raw, fallback string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = fallback
	}
	switch raw {
	case "1h":
		return 1 * time.Hour
	case "6h":
		return 6 * time.Hour
	case "24h", "1d":
		return 24 * time.Hour
	case "7d":
		return 7 * 24 * time.Hour
	case "30d":
		return 30 * 24 * time.Hour
	}
	// "90d" / unknown → 30d cap (we only keep 30d of pings anyway
	// — anything longer is meaningless for the health dashboard).
	return 30 * 24 * time.Hour
}

// ----- /health ---------------------------------------------------------------

type providerHealthSparklinePoint struct {
	CheckedAt time.Time `json:"checked_at"`
	OK        bool      `json:"ok"`
	LatencyMS int       `json:"latency_ms"`
}

type providerHealthDashboardRow struct {
	ProviderID    string                          `json:"provider_id"`
	Provider      string                          `json:"provider"`
	Label         string                          `json:"label"`
	Checks        int                             `json:"checks"`
	Successes     int                             `json:"successes"`
	Failures      int                             `json:"failures"`
	SuccessRate   float64                         `json:"success_rate"`
	LatencyP50    int                             `json:"latency_p50_ms"`
	LatencyP95    int                             `json:"latency_p95_ms"`
	LatencyMax    int                             `json:"latency_max_ms"`
	LastCheckedAt *time.Time                      `json:"last_checked_at,omitempty"`
	LastOK        *bool                           `json:"last_ok,omitempty"`
	Sparkline     []providerHealthSparklinePoint  `json:"sparkline"`
}

type providerHealthDashboardResponse struct {
	WindowStart  time.Time                      `json:"window_start"`
	WindowEnd    time.Time                      `json:"window_end"`
	ProbeTicks   int64                          `json:"probe_ticks_since_boot"`
	Rows         []providerHealthDashboardRow   `json:"rows"`
}

func (h *adminHandler) handleProviderHealthDashboard(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	// Empty payload when wired without repos — keep the API shape
	// stable for the frontend.
	resp := providerHealthDashboardResponse{
		WindowStart: time.Now(), WindowEnd: time.Now(),
		Rows: []providerHealthDashboardRow{},
	}
	if h.healthProbeLoop != nil {
		resp.ProbeTicks = h.healthProbeLoop.Probes()
	}
	if h.providerHealthHistoryRepo == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	lookback := parseRange(r.URL.Query().Get("range"), "24h")
	since := time.Now().Add(-lookback)
	resp.WindowStart = since
	resp.WindowEnd = time.Now()

	summaries, err := h.providerHealthHistoryRepo.SummariseByProvider(r.Context(), since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("summarise_failed", err.Error()))
		return
	}
	for _, s := range summaries {
		row := providerHealthDashboardRow{
			ProviderID:  s.ProviderID.String(),
			Provider:    s.Provider,
			Label:       s.Label,
			Checks:      s.Checks,
			Successes:   s.Successes,
			Failures:    s.Failures,
			SuccessRate: s.SuccessRate(),
			LatencyP50:  s.LatencyP50,
			LatencyP95:  s.LatencyP95,
			LatencyMax:  s.LatencyMax,
			Sparkline:   []providerHealthSparklinePoint{},
		}
		if !s.LastCheckedAt.IsZero() {
			ts := s.LastCheckedAt
			row.LastCheckedAt = &ts
		}
		if s.LastOK.Valid {
			lo := s.LastOK.Bool
			row.LastOK = &lo
		}
		// Sparkline: cap at 144 points (12h @ 5min ticks) so the
		// payload stays small. Newer-first from the DB; reverse so
		// the chart can render left→right by time.
		points, err := h.providerHealthHistoryRepo.ListRecent(r.Context(), s.ProviderID, since, 144)
		if err == nil {
			out := make([]providerHealthSparklinePoint, 0, len(points))
			for i := len(points) - 1; i >= 0; i-- {
				out = append(out, providerHealthSparklinePoint{
					CheckedAt: points[i].CheckedAt,
					OK:        points[i].OK,
					LatencyMS: points[i].LatencyMS,
				})
			}
			row.Sparkline = out
		}
		resp.Rows = append(resp.Rows, row)
	}
	writeJSON(w, http.StatusOK, resp)
}

// ----- /cost -----------------------------------------------------------------

type providerCostTotalDTO struct {
	Provider     string  `json:"provider"`
	Calls        int64   `json:"calls"`
	TotalTokens  int64   `json:"total_tokens"`
	CostCents    float64 `json:"cost_cents"`
	CostUSD      float64 `json:"cost_usd"`
	DaysInWindow int     `json:"days_in_window"`
}

type providerCostDailyDTO struct {
	Provider     string    `json:"provider"`
	ModelName    string    `json:"model_name"`
	Day          string    `json:"day"` // YYYY-MM-DD UTC
	Calls        int64     `json:"calls"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	TotalTokens  int64     `json:"total_tokens"`
	CostCents    float64   `json:"cost_cents"`
	CostUSD      float64   `json:"cost_usd"`
	LastRolledAt time.Time `json:"last_rolled_at"`
}

type providerCostDashboardResponse struct {
	WindowStartDay string                 `json:"window_start_day"`
	WindowEndDay   string                 `json:"window_end_day"`
	RollupTicks    int64                  `json:"rollup_ticks_since_boot"`
	Totals         []providerCostTotalDTO `json:"totals"`
	Daily          []providerCostDailyDTO `json:"daily"`
}

func (h *adminHandler) handleProviderCostDashboard(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	resp := providerCostDashboardResponse{
		Totals: []providerCostTotalDTO{},
		Daily:  []providerCostDailyDTO{},
	}
	if h.costRollupLoop != nil {
		resp.RollupTicks = h.costRollupLoop.Ticks()
	}
	if h.providerDailyRollupRepo == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	lookback := parseRange(r.URL.Query().Get("range"), "7d")
	now := time.Now().UTC()
	to := now
	from := now.Add(-lookback)
	// Normalise to date boundaries (the rollup table is keyed by
	// DATE so we want fromDay..toDay inclusive).
	fromDay := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	toDay := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	resp.WindowStartDay = fromDay.Format("2006-01-02")
	resp.WindowEndDay = toDay.Format("2006-01-02")

	totals, err := h.providerDailyRollupRepo.SumByProvider(r.Context(), fromDay, toDay)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("sum_failed", err.Error()))
		return
	}
	for _, t := range totals {
		resp.Totals = append(resp.Totals, providerCostTotalDTO{
			Provider:     t.Provider,
			Calls:        t.Calls,
			TotalTokens:  t.TotalTokens,
			CostCents:    t.CostCents,
			CostUSD:      t.CostCents / 100.0,
			DaysInWindow: t.DaysInWindow,
		})
	}
	providerFilter := strings.TrimSpace(r.URL.Query().Get("provider"))
	daily, err := h.providerDailyRollupRepo.ListByDayRange(r.Context(), providerFilter, fromDay, toDay)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("list_failed", err.Error()))
		return
	}
	for _, d := range daily {
		resp.Daily = append(resp.Daily, providerCostDailyDTO{
			Provider:     d.Provider,
			ModelName:    d.ModelName,
			Day:          d.Day.Format("2006-01-02"),
			Calls:        d.Calls,
			InputTokens:  d.InputTokens,
			OutputTokens: d.OutputTokens,
			TotalTokens:  d.TotalTokens,
			CostCents:    d.CostCents,
			CostUSD:      d.CostCents / 100.0,
			LastRolledAt: d.LastRolledAt,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// ----- /{id}/history ---------------------------------------------------------

type providerHistoryRowDTO struct {
	ID         string    `json:"id"`
	CheckedAt  time.Time `json:"checked_at"`
	OK         bool      `json:"ok"`
	LatencyMS  int       `json:"latency_ms"`
	HTTPStatus int       `json:"http_status"`
	Message    string    `json:"message,omitempty"`
	ModelName  string    `json:"model_name,omitempty"`
}

type providerHistoryResponse struct {
	ProviderID   string                  `json:"provider_id"`
	WindowStart  time.Time               `json:"window_start"`
	WindowEnd    time.Time               `json:"window_end"`
	Rows         []providerHistoryRowDTO `json:"rows"`
}

func (h *adminHandler) handleProviderHistory(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	resp := providerHistoryResponse{Rows: []providerHistoryRowDTO{}}
	if h.providerHealthHistoryRepo == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	idStr := r.PathValue("id")
	id, err := uuid.Parse(strings.TrimSpace(idStr))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_id", fmt.Sprintf("provider id: %v", err)))
		return
	}
	resp.ProviderID = id.String()
	lookback := parseRange(r.URL.Query().Get("range"), "24h")
	since := time.Now().Add(-lookback)
	resp.WindowStart = since
	resp.WindowEnd = time.Now()

	limit := 1000
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 5000 {
			limit = n
		}
	}
	rows, err := h.providerHealthHistoryRepo.ListRecent(r.Context(), id, since, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("list_failed", err.Error()))
		return
	}
	for _, row := range rows {
		out := providerHistoryRowDTO{
			ID:         row.ID.String(),
			CheckedAt:  row.CheckedAt,
			OK:         row.OK,
			LatencyMS:  row.LatencyMS,
			HTTPStatus: row.HTTPStatus,
		}
		if row.Message.Valid {
			out.Message = row.Message.String
		}
		if row.ModelName.Valid {
			out.ModelName = row.ModelName.String
		}
		resp.Rows = append(resp.Rows, out)
	}
	writeJSON(w, http.StatusOK, resp)
}
