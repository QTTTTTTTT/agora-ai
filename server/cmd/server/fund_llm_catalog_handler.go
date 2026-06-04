// fund_llm_catalog_handler.go — read-only "what providers/models can
// I pick for this fund" endpoint, scoped to the fund's owner.
//
//	GET /api/funds/{fundId}/llm-catalog
//
// Why this lives outside /api/admin/llm-providers:
//   The admin endpoints expose the full provider row (API key
//   ciphertext, fingerprint, health probe payloads, etc.) and are
//   gated by `requireAdmin`. The fund owner is NOT necessarily a
//   platform admin — they only need a tiny safe-for-everyone projection
//   of (provider, label, model_tier, model_name) so the UI can render
//   <select> options when the operator picks an LLM for an A/B variant
//   or a fund override. We render that projection here and reuse the
//   same `authorizeFundAccess` chain the rest of /api/funds/* uses.
//
// What we deliberately omit from the DTO:
//   * api_key_encrypted / api_key_fingerprint  — credential material
//   * cost_per_1m / pricing                    — admin pricing surface
//   * last_health_check_*                      — operational telemetry
//   * created_by / updated_by                  — audit trail
//   * status (always "active" because we filter on it)
//
// What we keep:
//   provider · label · model_tier · model_name · is_platform_default

package main

import (
	"net/http"
	"strings"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/repository"
)

type fundLLMCatalogHandler struct {
	providerRepo *repository.PlatformLLMProviderRepo
	fundRepo     *repository.FundRepo
	companyRepo  *repository.FundCompanyRepo
}

// newFundLLMCatalogHandler returns nil when the platform provider
// repo is unwired so the route never registers — the UI then falls
// back to a free-text input which is the legacy behaviour.
func newFundLLMCatalogHandler(svc *Services) *fundLLMCatalogHandler {
	if svc == nil || svc.PlatformLLMProviderRepo == nil || svc.DB == nil {
		return nil
	}
	return &fundLLMCatalogHandler{
		providerRepo: svc.PlatformLLMProviderRepo,
		fundRepo:     repository.NewFundRepo(svc.DB),
		companyRepo:  repository.NewFundCompanyRepo(svc.DB),
	}
}

func (h *fundLLMCatalogHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/funds/{fundId}/llm-catalog", h.handleList)
}

// fundLLMCatalogEntry is the minimal "pick one of these" shape the
// UI consumes. Field order intentionally matches the <option> render
// order (provider → label → model_name) so a quick visual scan tells
// the operator "I'm picking openai openai-prod gpt-4o" in one read.
type fundLLMCatalogEntry struct {
	Provider          string `json:"provider"`
	Label             string `json:"label,omitempty"`
	ModelTier         string `json:"model_tier,omitempty"`
	ModelName         string `json:"model_name,omitempty"`
	IsPlatformDefault bool   `json:"is_platform_default,omitempty"`
}

func (h *fundLLMCatalogHandler) handleList(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "authentication required"))
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fund id required"))
		return
	}
	if _, err := authorizeFundAccess(r.Context(), h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		// Re-using fund_llm_overrides_handler's auth-error mapper
		// would couple the two files; the response codes are short
		// enough to inline.
		switch {
		case err == api.ErrForbidden:
			writeJSON(w, http.StatusForbidden, errorPayload("forbidden", "no access to this fund"))
		case err == api.ErrNotFound:
			writeJSON(w, http.StatusNotFound, errorPayload("not_found", "fund not found"))
		default:
			writeJSON(w, http.StatusInternalServerError, errorPayload("auth_failed", err.Error()))
		}
		return
	}
	rows, err := h.providerRepo.ListAll(r.Context(), repository.ListFilters{Status: "active"})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("list_failed", err.Error()))
		return
	}
	out := make([]fundLLMCatalogEntry, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		entry := fundLLMCatalogEntry{
			Provider:          row.Provider,
			Label:             row.Label,
			ModelName:         row.ModelName,
			IsPlatformDefault: row.IsPlatformDefault,
		}
		if row.ModelTier.Valid {
			entry.ModelTier = row.ModelTier.String
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}
