// advisor_byok_handler.go — Phase B-4 HTTP surface for the user
// BYOK key store. Sits next to the /api/advisor/* routes because
// it's part of the advisor mode UX (only /advisor flows use the
// user_llm_keys table at routing time).
//
// Five endpoints:
//
//   POST   /api/advisor/byok/keys              create a new key
//   GET    /api/advisor/byok/keys              list user's keys
//   PUT    /api/advisor/byok/keys/{id}/budget  edit monthly cap
//   PUT    /api/advisor/byok/keys/{id}/active  pause / resume
//   DELETE /api/advisor/byok/keys/{id}         soft-revoke
//
// Authorisation chain (in order):
//   1. authenticated user (api.AuthenticatedUserID)
//   2. advisor_mode feature flag (server-side gate — handled by
//      featureGateMiddleware upstream, same as the rest of
//      /api/advisor/*)
//   3. byok_advisor feature flag (Phase B-4 new flag registered
//      in migration 097's feature_flags table via runtime seed)
//   4. user's effective plan has AllowAdvisorBYOK = true
//
// Failing #3 or #4 returns HTTP 403 with a structured payload so
// the SPA can render an "upgrade for BYOK" prompt instead of the
// raw error.

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/subscription"
	"github.com/fundai/server/internal/userbyok"
)

type advisorBYOKHandler struct {
	repo                *userbyok.Repo
	subscriptionService *subscription.SubscriptionService
	flags               *featureFlagCache
	now                 func() time.Time
}

// newAdvisorBYOKHandler returns nil when the repo isn't wired so
// the router leaves the BYOK routes unregistered in degraded boots
// (test main, missing crypto secret). The feature-flag cache is
// optional: passing nil disables the per-flag gate and the handler
// falls back to the plan-only check, matching the behaviour of
// other surfaces that boot before the admin handler is wired.
func newAdvisorBYOKHandler(svc *Services, flags *featureFlagCache) *advisorBYOKHandler {
	if svc == nil || svc.UserBYOKRepo == nil {
		return nil
	}
	return &advisorBYOKHandler{
		repo:                svc.UserBYOKRepo,
		subscriptionService: svc.SubscriptionService,
		flags:               flags,
		now:                 time.Now,
	}
}

func (h *advisorBYOKHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("POST /api/advisor/byok/keys", h.handleCreate)
	mux.HandleFunc("GET /api/advisor/byok/keys", h.handleList)
	mux.HandleFunc("PUT /api/advisor/byok/keys/{id}/budget", h.handleUpdateBudget)
	mux.HandleFunc("PUT /api/advisor/byok/keys/{id}/active", h.handleSetActive)
	mux.HandleFunc("DELETE /api/advisor/byok/keys/{id}", h.handleDelete)
	mux.HandleFunc("GET /api/advisor/byok/info", h.handleInfo)
}

// --- Wire shapes -----------------------------------------------------------

type advisorBYOKKeyWire struct {
	ID                    string `json:"id"`
	Provider              string `json:"provider"`
	Label                 string `json:"label"`
	APIKeyFingerprint     string `json:"api_key_fingerprint"`
	APIKeyPreview         string `json:"api_key_preview"`
	BaseURL               string `json:"base_url,omitempty"`
	ModelName             string `json:"model_name,omitempty"`
	MonthlyBudgetCentsUSD int    `json:"monthly_budget_cents_usd"`
	IsActive              bool   `json:"is_active"`
	LastUsedAt            string `json:"last_used_at,omitempty"`
	LastVerifiedAt        string `json:"last_verified_at,omitempty"`
	RevokedAt             string `json:"revoked_at,omitempty"`
	RevokedReason         string `json:"revoked_reason,omitempty"`
	CreatedAt             string `json:"created_at"`
}

type advisorBYOKCreateRequest struct {
	Provider              string `json:"provider"`
	Label                 string `json:"label"`
	APIKey                string `json:"api_key"`
	BaseURL               string `json:"base_url"`
	ModelName             string `json:"model_name"`
	MonthlyBudgetCentsUSD int    `json:"monthly_budget_cents_usd"`
}

type advisorBYOKUpdateBudgetRequest struct {
	MonthlyBudgetCentsUSD int `json:"monthly_budget_cents_usd"`
}

type advisorBYOKSetActiveRequest struct {
	IsActive bool `json:"is_active"`
}

type advisorBYOKDeleteRequest struct {
	Reason string `json:"reason"`
}

// --- Handlers --------------------------------------------------------------

func (h *advisorBYOKHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.authorise(w, r)
	if !ok {
		return
	}
	var req advisorBYOKCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	key, err := h.repo.Create(r.Context(), userbyok.CreateRequest{
		UserID:                userID,
		Provider:              req.Provider,
		Label:                 req.Label,
		PlaintextAPIKey:       req.APIKey,
		BaseURL:               req.BaseURL,
		ModelName:             req.ModelName,
		MonthlyBudgetCentsUSD: req.MonthlyBudgetCentsUSD,
	})
	if err != nil {
		h.writeBYOKError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, projectAdvisorBYOKKey(key))
}

func (h *advisorBYOKHandler) handleList(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.authorise(w, r)
	if !ok {
		return
	}
	keys, err := h.repo.List(r.Context(), userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("list_failed", err.Error()))
		return
	}
	out := make([]advisorBYOKKeyWire, 0, len(keys))
	for _, k := range keys {
		out = append(out, projectAdvisorBYOKKey(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (h *advisorBYOKHandler) handleUpdateBudget(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.authorise(w, r)
	if !ok {
		return
	}
	keyID := strings.TrimSpace(r.PathValue("id"))
	if keyID == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "key id required"))
		return
	}
	var req advisorBYOKUpdateBudgetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	if err := h.repo.UpdateBudget(r.Context(), userID, keyID, req.MonthlyBudgetCentsUSD); err != nil {
		h.writeBYOKError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *advisorBYOKHandler) handleSetActive(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.authorise(w, r)
	if !ok {
		return
	}
	keyID := strings.TrimSpace(r.PathValue("id"))
	if keyID == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "key id required"))
		return
	}
	var req advisorBYOKSetActiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	if err := h.repo.SetActive(r.Context(), userID, keyID, req.IsActive); err != nil {
		h.writeBYOKError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "is_active": req.IsActive})
}

func (h *advisorBYOKHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.authorise(w, r)
	if !ok {
		return
	}
	keyID := strings.TrimSpace(r.PathValue("id"))
	if keyID == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "key id required"))
		return
	}
	var req advisorBYOKDeleteRequest
	// Body is optional on DELETE; ignore decode errors.
	_ = json.NewDecoder(r.Body).Decode(&req)
	if strings.TrimSpace(req.Reason) == "" {
		req.Reason = "user_revoked"
	}
	if err := h.repo.Delete(r.Context(), userID, keyID, req.Reason); err != nil {
		h.writeBYOKError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- Helpers ---------------------------------------------------------------

// authorise enforces all four gates and writes the response on
// any failure. Returns (userID, true) only when every check passes.
func (h *advisorBYOKHandler) authorise(w http.ResponseWriter, r *http.Request) (string, bool) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return "", false
	}
	// Phase B-4 second feature flag — "advisor_byok" is a master
	// switch that lets ops disable BYOK platform-wide without
	// dropping migrations or de-wiring the hook. Defaults to ON
	// for unknown flags (see featureFlagCache.IsEnabled) so
	// existing dev environments don't break when the seed
	// migration hasn't run yet; production seeds it explicitly.
	if h.flags != nil && !h.flags.IsEnabled(r.Context(), "advisor_byok") {
		writeJSON(w, http.StatusForbidden, errorPayload("byok_disabled",
			"advisor BYOK is disabled by the platform"))
		return "", false
	}
	// Plan check.
	if h.subscriptionService != nil {
		plan, err := h.subscriptionService.GetEffectivePlan(r.Context(), userID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorPayload("plan_lookup_failed", err.Error()))
			return "", false
		}
		if plan == nil || !plan.AllowAdvisorBYOK {
			payload := map[string]any{
				"error":          "byok_not_allowed_on_plan",
				"plan_tier":      "",
				"upgrade_suggested": string(subscription.PlanPro),
				"message":        "BYOK requires a paid plan",
			}
			if plan != nil {
				payload["plan_tier"] = string(plan.Tier)
			}
			writeJSON(w, http.StatusForbidden, payload)
			return "", false
		}
	}
	return userID, true
}

// handleInfo returns the platform-side BYOK metadata that powers
// the security guide ("paste this IP into your OpenAI dashboard
// IP allow-list"). Auth-required because we only want this in
// front of logged-in eyes, but otherwise public per-user — no
// secrets, just env-driven configuration.
func (h *advisorBYOKHandler) handleInfo(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	// We deliberately don't gate this on the BYOK feature flag —
	// the user might be reading the security guide to decide
	// whether to enable BYOK in the first place, and we want
	// them to see the IP they'd need to whitelist before
	// committing.
	egress := strings.TrimSpace(os.Getenv("ADVISOR_EGRESS_IP"))
	if egress == "" {
		egress = "(set ADVISOR_EGRESS_IP)"
	}
	supportEmail := strings.TrimSpace(os.Getenv("ADVISOR_BYOK_SUPPORT_EMAIL"))
	if supportEmail == "" {
		supportEmail = "support@fundai.example"
	}
	encryptedAtRest := strings.TrimSpace(os.Getenv("MODEL_CONFIG_API_KEY_SECRET")) != ""
	writeJSON(w, http.StatusOK, map[string]any{
		"egress_ip":         egress,
		"support_email":     supportEmail,
		"encrypted_at_rest": encryptedAtRest,
		"providers_supported": []string{
			"openai", "anthropic", "deepseek", "kimi", "doubao", "qwen",
		},
	})
}

func (h *advisorBYOKHandler) writeBYOKError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, userbyok.ErrUnsupportedProvider):
		writeJSON(w, http.StatusBadRequest, errorPayload("unsupported_provider", err.Error()))
	case errors.Is(err, userbyok.ErrEmptyAPIKey):
		writeJSON(w, http.StatusBadRequest, errorPayload("empty_api_key", err.Error()))
	case errors.Is(err, userbyok.ErrAlreadyActive):
		writeJSON(w, http.StatusConflict, errorPayload("already_active",
			"you already have an active key for this provider — revoke it first or update the existing row"))
	case errors.Is(err, userbyok.ErrEncryptionUnconfigured):
		writeJSON(w, http.StatusServiceUnavailable, errorPayload("encryption_unconfigured",
			"server-side BYOK encryption is not configured (MODEL_CONFIG_API_KEY_SECRET unset)"))
	case errors.Is(err, userbyok.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorPayload("not_found", "key not found"))
	default:
		writeJSON(w, http.StatusInternalServerError, errorPayload("byok_request_failed", err.Error()))
	}
}

func projectAdvisorBYOKKey(k *userbyok.Key) advisorBYOKKeyWire {
	out := advisorBYOKKeyWire{
		ID:                    k.ID,
		Provider:              k.Provider,
		Label:                 k.Label,
		APIKeyFingerprint:     k.APIKeyFingerprint,
		APIKeyPreview:         k.APIKeyPreview,
		BaseURL:               k.BaseURL,
		ModelName:             k.ModelName,
		MonthlyBudgetCentsUSD: k.MonthlyBudgetCentsUSD,
		IsActive:              k.IsActive,
		RevokedReason:         k.RevokedReason,
		CreatedAt:             k.CreatedAt.UTC().Format(time.RFC3339),
	}
	if k.LastUsedAt.Valid {
		out.LastUsedAt = k.LastUsedAt.Time.UTC().Format(time.RFC3339)
	}
	if k.LastVerifiedAt.Valid {
		out.LastVerifiedAt = k.LastVerifiedAt.Time.UTC().Format(time.RFC3339)
	}
	if k.RevokedAt.Valid {
		out.RevokedAt = k.RevokedAt.Time.UTC().Format(time.RFC3339)
	}
	return out
}
