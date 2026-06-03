// admin_llm_health.go — Sprint 11.4 LLM health admin endpoints.
//
//	GET /api/admin/llm-health/summary[?window_hours=24]
//	    Returns {sources: [...], categories: [...], window_hours}.
//
//	GET /api/admin/llm-health/recent-fallbacks[?window_hours=24&limit=50]
//	    Returns the N most recent fallback rows in the window, with the
//	    raw provider summary visible (admin-only surface).
//
// These endpoints back the AdminLLMHealthSection React component and
// the SRE alert query (`fallback rate > 5% over 30 minutes`).
//
// Authorisation: requireAdmin. The data exposed here — particularly
// the raw fallback_reason.summary — must NEVER leak to non-admin
// surfaces, which is why no fund-scoped variant of these routes
// exists. The user-facing chip in DecisionCenter.tsx uses a stripped
// projection (PlanFallbackReason) without the summary field.

package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (h *adminHandler) registerLLMHealthAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil || h.llmHealthRepo == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/llm-health/summary", h.handleLLMHealthSummary)
	mux.HandleFunc("GET /api/admin/llm-health/recent-fallbacks", h.handleLLMHealthRecentFallbacks)
}

// --- wire types ---------------------------------------------------------------

type llmHealthSourceWire struct {
	Source string `json:"source"`
	Count  int64  `json:"count"`
}

type llmHealthCategoryWire struct {
	Category string `json:"category"`
	Provider string `json:"provider,omitempty"`
	Count    int64  `json:"count"`
}

type llmHealthRecentWire struct {
	PlanID    string `json:"plan_id"`
	FundID    string `json:"fund_id"`
	Source    string `json:"source"`
	Category  string `json:"category,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	Summary   string `json:"summary,omitempty"`
	CreatedAt string `json:"created_at"`
}

type llmHealthSummaryWire struct {
	WindowHours int                     `json:"window_hours"`
	Sources     []llmHealthSourceWire   `json:"sources"`
	Categories  []llmHealthCategoryWire `json:"categories"`
}

// --- handlers -----------------------------------------------------------------

func (h *adminHandler) handleLLMHealthSummary(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	window := parseHealthWindow(r.URL.Query().Get("window_hours"))
	sources, err := h.llmHealthRepo.AggregateBySource(r.Context(), window)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("aggregate_failed", err.Error()))
		return
	}
	categories, err := h.llmHealthRepo.AggregateByCategory(r.Context(), window)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("aggregate_failed", err.Error()))
		return
	}
	out := llmHealthSummaryWire{
		WindowHours: int(window / time.Hour),
		Sources:     make([]llmHealthSourceWire, 0, len(sources)),
		Categories:  make([]llmHealthCategoryWire, 0, len(categories)),
	}
	for _, s := range sources {
		out.Sources = append(out.Sources, llmHealthSourceWire{Source: s.Source, Count: s.Count})
	}
	for _, c := range categories {
		out.Categories = append(out.Categories, llmHealthCategoryWire{
			Category: c.Category,
			Provider: c.Provider,
			Count:    c.Count,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *adminHandler) handleLLMHealthRecentFallbacks(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	window := parseHealthWindow(r.URL.Query().Get("window_hours"))
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	rows, err := h.llmHealthRepo.RecentFallbacks(r.Context(), window, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("recent_failed", err.Error()))
		return
	}
	out := make([]llmHealthRecentWire, 0, len(rows))
	for _, p := range rows {
		out = append(out, llmHealthRecentWire{
			PlanID:    p.PlanID,
			FundID:    p.FundID,
			Source:    p.Source,
			Category:  p.Category,
			Provider:  p.Provider,
			Model:     p.Model,
			Summary:   p.Summary,
			CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"window_hours": int(window / time.Hour),
		"items":        out,
	})
}

// parseHealthWindow turns the query parameter into a duration. Empty
// or invalid input maps to 24h. The repo enforces its own ceiling /
// floor on top.
func parseHealthWindow(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 24 * time.Hour
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 24 * time.Hour
	}
	return time.Duration(n) * time.Hour
}
