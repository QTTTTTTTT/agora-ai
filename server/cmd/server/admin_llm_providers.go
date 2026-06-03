// admin_llm_providers.go — S13 platform LLM provider admin endpoints.
//
//	GET    /api/admin/llm-providers              list rows (filterable)
//	GET    /api/admin/llm-providers/{id}         single row
//	PUT    /api/admin/llm-providers              create or update (id in body)
//	DELETE /api/admin/llm-providers/{id}         remove row
//	POST   /api/admin/llm-providers/{id}/default promote to platform default
//	POST   /api/admin/llm-providers/test         dry-run a config (no persist)
//
// Every successful mutation triggers an in-process router reload
// (no app restart) and emits an admin_change_log row. API keys
// never round-trip — responses carry only the 8-char fingerprint
// and a masked preview; the plaintext lives in the encrypted column
// and the operator's browser memory after submission.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/llm"
	"github.com/fundai/server/internal/modelab"
	"github.com/fundai/server/internal/repository"
	"github.com/google/uuid"
)

// providerReloader is the narrow callback the admin handler invokes
// after every mutation. wiring_adapters supplies a real
// implementation that re-reads the table and pushes a fresh
// (systemAPIKeys, tierDefaults) pair into the router; tests pass a
// no-op. Errors are surfaced as 5xx so the operator knows the DB
// change landed but the router is stale.
type providerReloader interface {
	ReloadPlatformProviders(ctx context.Context) error
}

func (h *adminHandler) registerLLMProviderRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil || h.platformLLMProviderRepo == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/llm-providers", h.handleListLLMProviders)
	mux.HandleFunc("GET /api/admin/llm-providers/{id}", h.handleGetLLMProvider)
	mux.HandleFunc("PUT /api/admin/llm-providers", h.handleUpsertLLMProvider)
	mux.HandleFunc("DELETE /api/admin/llm-providers/{id}", h.handleDeleteLLMProvider)
	mux.HandleFunc("POST /api/admin/llm-providers/{id}/default", h.handleSetDefaultLLMProvider)
	mux.HandleFunc("POST /api/admin/llm-providers/test", h.handleTestLLMProvider)
}

// --- DTOs --------------------------------------------------------------------

// llmProviderDTO is the JSON shape returned to the admin UI. API key
// material is NEVER serialised — only the fingerprint, masked preview,
// and the encrypted column's presence indicator. The fingerprint is
// stable per plaintext so the UI can render "sk-…a3f2" and the operator
// can verify "yes that's the key I pasted last week".
type llmProviderDTO struct {
	ID                    string         `json:"id"`
	Provider              string         `json:"provider"`
	Label                 string         `json:"label"`
	ModelTier             string         `json:"model_tier,omitempty"`
	ModelName             string         `json:"model_name"`
	BaseURL               string         `json:"base_url"`
	APIKeyFingerprint     string         `json:"api_key_fingerprint"`
	APIKeyMaskedPreview   string         `json:"api_key_masked_preview"`
	APIKeyConfigured      bool           `json:"api_key_configured"`
	MaxTokens             int            `json:"max_tokens"`
	Temperature           float64        `json:"temperature"`
	InputPricePer1M       *float64       `json:"input_price_per_1m,omitempty"`
	OutputPricePer1M      *float64       `json:"output_price_per_1m,omitempty"`
	CostPer1M             *float64       `json:"cost_per_1m,omitempty"`
	Status                string         `json:"status"`
	IsPlatformDefault     bool           `json:"is_platform_default"`
	LastHealthCheckAt     *time.Time     `json:"last_health_check_at,omitempty"`
	LastHealthCheckResult map[string]any `json:"last_health_check_result,omitempty"`
	Source                string         `json:"source"`
	CreatedAt             time.Time      `json:"created_at"`
	UpdatedAt             time.Time      `json:"updated_at"`
}

func dtoFromRow(row *repository.PlatformLLMProviderRow) llmProviderDTO {
	d := llmProviderDTO{
		ID:                  row.ID.String(),
		Provider:            row.Provider,
		Label:               row.Label,
		ModelName:           row.ModelName,
		BaseURL:             row.BaseURL,
		APIKeyFingerprint:   row.APIKeyFingerprint,
		APIKeyMaskedPreview: maskKeyPreview(row.APIKeyFingerprint),
		APIKeyConfigured:    strings.TrimSpace(row.APIKeyEncrypted) != "",
		MaxTokens:           row.MaxTokens,
		Temperature:         row.Temperature,
		Status:              row.Status,
		IsPlatformDefault:   row.IsPlatformDefault,
		Source:              row.Source,
		CreatedAt:           row.CreatedAt,
		UpdatedAt:           row.UpdatedAt,
	}
	if row.ModelTier.Valid {
		d.ModelTier = row.ModelTier.String
	}
	if row.InputPricePer1M.Valid {
		v := row.InputPricePer1M.Float64
		d.InputPricePer1M = &v
	}
	if row.OutputPricePer1M.Valid {
		v := row.OutputPricePer1M.Float64
		d.OutputPricePer1M = &v
	}
	if row.CostPer1M.Valid {
		v := row.CostPer1M.Float64
		d.CostPer1M = &v
	}
	if row.LastHealthCheckAt.Valid {
		t := row.LastHealthCheckAt.Time
		d.LastHealthCheckAt = &t
	}
	if len(row.LastHealthCheckResult) > 0 {
		var m map[string]any
		if err := json.Unmarshal(row.LastHealthCheckResult, &m); err == nil {
			d.LastHealthCheckResult = m
		}
	}
	return d
}

// maskKeyPreview renders "sk-…<fingerprint>" for the UI. We do NOT
// reveal any plaintext slice — the fingerprint alone is enough for
// the operator to recognise "yes that's the key I pasted".
func maskKeyPreview(fingerprint string) string {
	if fingerprint == "" {
		return ""
	}
	return "sk-…" + fingerprint
}

type upsertLLMProviderRequest struct {
	ID               string   `json:"id,omitempty"` // empty = create
	Provider         string   `json:"provider"`
	Label            string   `json:"label"`
	ModelTier        string   `json:"model_tier,omitempty"`
	ModelName        string   `json:"model_name"`
	BaseURL          string   `json:"base_url"`
	APIKey           string   `json:"api_key,omitempty"` // empty on update = keep
	MaxTokens        int      `json:"max_tokens,omitempty"`
	Temperature      *float64 `json:"temperature,omitempty"`
	InputPricePer1M  *float64 `json:"input_price_per_1m,omitempty"`
	OutputPricePer1M *float64 `json:"output_price_per_1m,omitempty"`
	CostPer1M        *float64 `json:"cost_per_1m,omitempty"`
	Status           string   `json:"status,omitempty"`
}

type testLLMProviderRequest struct {
	ID        string `json:"id,omitempty"` // when set, reuses stored encrypted key if APIKey empty
	Provider  string `json:"provider"`
	ModelName string `json:"model_name"`
	BaseURL   string `json:"base_url"`
	APIKey    string `json:"api_key,omitempty"`
}

type testLLMProviderResponse struct {
	OK          bool   `json:"ok"`
	LatencyMS   int64  `json:"latency_ms"`
	HTTPStatus  int    `json:"http_status,omitempty"`
	Message     string `json:"message,omitempty"`
	EchoedModel string `json:"echoed_model,omitempty"`
}

// --- handlers ----------------------------------------------------------------

func (h *adminHandler) handleListLLMProviders(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	filters := repository.ListFilters{
		Provider: strings.TrimSpace(r.URL.Query().Get("provider")),
		Status:   strings.TrimSpace(r.URL.Query().Get("status")),
		OnlyTier: strings.TrimSpace(r.URL.Query().Get("tier")),
	}
	rows, err := h.platformLLMProviderRepo.ListAll(r.Context(), filters)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("list_failed", err.Error()))
		return
	}
	out := make([]llmProviderDTO, 0, len(rows))
	for i := range rows {
		out = append(out, dtoFromRow(&rows[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"providers":           out,
		"reload_generation":   llm.ReloadGeneration(),
		"router_active_keys":  h.routerActiveKeysOrNil(),
		"router_default_keys": nil, // reserved for future per-tier diagnostics
	})
}

func (h *adminHandler) handleGetLLMProvider(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id, err := uuid.Parse(strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_id", "id must be a UUID"))
		return
	}
	row, err := h.platformLLMProviderRepo.Get(r.Context(), id)
	if errors.Is(err, repository.ErrPlatformLLMProviderNotFound) {
		writeJSON(w, http.StatusNotFound, errorPayload("not_found", "provider not found"))
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("get_failed", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, dtoFromRow(row))
}

func (h *adminHandler) handleUpsertLLMProvider(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var req upsertLLMProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("decode_failed", err.Error()))
		return
	}
	in := repository.UpsertInput{
		Provider:    req.Provider,
		Label:       req.Label,
		ModelTier:   req.ModelTier,
		ModelName:   req.ModelName,
		BaseURL:     req.BaseURL,
		APIKeyPlaintext: req.APIKey,
		MaxTokens:   req.MaxTokens,
		Status:      req.Status,
		Source:      "admin",
	}
	if req.Temperature != nil {
		in.Temperature = *req.Temperature
	} else {
		in.Temperature = 0.7
	}
	if req.InputPricePer1M != nil {
		in.InputPricePer1M = sql.NullFloat64{Float64: *req.InputPricePer1M, Valid: true}
	}
	if req.OutputPricePer1M != nil {
		in.OutputPricePer1M = sql.NullFloat64{Float64: *req.OutputPricePer1M, Valid: true}
	}
	if req.CostPer1M != nil {
		in.CostPer1M = sql.NullFloat64{Float64: *req.CostPer1M, Valid: true}
	}
	if v := strings.TrimSpace(req.ID); v != "" {
		parsed, err := uuid.Parse(v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorPayload("invalid_id", "id must be a UUID"))
			return
		}
		in.ID = parsed
	}
	actorUUID, _ := h.actorUUID(r)
	in.ActorUserID = actorUUID

	row, err := h.platformLLMProviderRepo.Upsert(r.Context(), in)
	if errors.Is(err, repository.ErrPlatformLLMProviderNotFound) {
		writeJSON(w, http.StatusNotFound, errorPayload("not_found", "provider not found"))
		return
	}
	if err != nil {
		// Validation errors carry a recognisable prefix; map to 422.
		if isValidationErr(err) {
			writeJSON(w, http.StatusUnprocessableEntity, errorPayload("validation_failed", err.Error()))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorPayload("upsert_failed", err.Error()))
		return
	}

	if err := h.reloadProviders(r.Context()); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("reload_failed", err.Error()))
		return
	}

	h.auditProviderChange(r, "platform_llm_provider_upsert", row, map[string]any{
		"reload_generation": llm.ReloadGeneration(),
	})
	writeJSON(w, http.StatusOK, dtoFromRow(row))
}

func (h *adminHandler) handleDeleteLLMProvider(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id, err := uuid.Parse(strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_id", "id must be a UUID"))
		return
	}
	// Capture the pre-delete row so the audit log carries a
	// proper before-snapshot. Errors here are non-fatal: the
	// delete still runs and we log a stripped event.
	before, _ := h.platformLLMProviderRepo.Get(r.Context(), id)
	err = h.platformLLMProviderRepo.Delete(r.Context(), id)
	if errors.Is(err, repository.ErrPlatformLLMProviderNotFound) {
		writeJSON(w, http.StatusNotFound, errorPayload("not_found", "provider not found"))
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("delete_failed", err.Error()))
		return
	}
	if err := h.reloadProviders(r.Context()); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("reload_failed", err.Error()))
		return
	}
	h.auditProviderChange(r, "platform_llm_provider_delete", before, map[string]any{
		"deleted_id":        id.String(),
		"reload_generation": llm.ReloadGeneration(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted_id": id.String()})
}

func (h *adminHandler) handleSetDefaultLLMProvider(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id, err := uuid.Parse(strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_id", "id must be a UUID"))
		return
	}
	actor, _ := h.actorUUID(r)
	if err := h.platformLLMProviderRepo.SetDefault(r.Context(), id, actor); err != nil {
		if errors.Is(err, repository.ErrPlatformLLMProviderNotFound) {
			writeJSON(w, http.StatusNotFound, errorPayload("not_found", "provider not found"))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorPayload("set_default_failed", err.Error()))
		return
	}
	if err := h.reloadProviders(r.Context()); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("reload_failed", err.Error()))
		return
	}
	row, _ := h.platformLLMProviderRepo.Get(r.Context(), id)
	h.auditProviderChange(r, "platform_llm_provider_set_default", row, map[string]any{
		"reload_generation": llm.ReloadGeneration(),
	})
	if row == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeJSON(w, http.StatusOK, dtoFromRow(row))
}

func (h *adminHandler) handleTestLLMProvider(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var req testLLMProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("decode_failed", err.Error()))
		return
	}
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	baseURL := strings.TrimSpace(req.BaseURL)
	model := strings.TrimSpace(req.ModelName)
	apiKey := strings.TrimSpace(req.APIKey)

	// When the operator hits "Test" on a saved row without
	// re-entering the key, the request carries the row ID and an
	// empty api_key — re-use the stored encrypted value.
	if apiKey == "" && strings.TrimSpace(req.ID) != "" {
		if id, err := uuid.Parse(req.ID); err == nil {
			row, getErr := h.platformLLMProviderRepo.Get(r.Context(), id)
			if getErr == nil && row != nil {
				if pt, err := row.PlainAPIKey(); err == nil {
					apiKey = pt
					if provider == "" {
						provider = row.Provider
					}
					if baseURL == "" {
						baseURL = row.BaseURL
					}
					if model == "" {
						model = row.ModelName
					}
				}
			}
		}
	}
	if provider == "" || baseURL == "" || model == "" || apiKey == "" {
		writeJSON(w, http.StatusUnprocessableEntity, errorPayload("missing_fields", "provider, base_url, model_name and api_key are required"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	result := runProviderPing(ctx, provider, baseURL, model, apiKey)

	// Persist a snapshot when we can identify the row.
	if strings.TrimSpace(req.ID) != "" {
		if id, err := uuid.Parse(req.ID); err == nil {
			_ = h.platformLLMProviderRepo.TouchHealth(r.Context(), id, map[string]any{
				"ok":           result.OK,
				"latency_ms":   result.LatencyMS,
				"http_status":  result.HTTPStatus,
				"message":      result.Message,
				"echoed_model": result.EchoedModel,
				"checked_at":   time.Now().UTC().Format(time.RFC3339),
			})
		}
	}

	writeJSON(w, http.StatusOK, result)
}

// --- audit / helpers ---------------------------------------------------------

func (h *adminHandler) auditProviderChange(r *http.Request, action string, row *repository.PlatformLLMProviderRow, extra map[string]any) {
	if h == nil || h.auditLogger == nil {
		return
	}
	actor, _ := h.actorUUID(r)
	meta := map[string]any{}
	for k, v := range extra {
		meta[k] = v
	}
	targetID := ""
	if row != nil {
		targetID = row.ID.String()
		meta["provider"] = row.Provider
		meta["label"] = row.Label
		meta["model_name"] = row.ModelName
		meta["base_url"] = row.BaseURL
		meta["api_key_fingerprint"] = row.APIKeyFingerprint
		meta["is_platform_default"] = row.IsPlatformDefault
		meta["status"] = row.Status
		if row.ModelTier.Valid {
			meta["model_tier"] = row.ModelTier.String
		}
	}
	_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
		ActorUserID: nullUUIDString(actor),
		Action:      action,
		TargetType:  "platform_llm_providers",
		TargetID:    targetID,
		Metadata:    meta,
	})
}

func (h *adminHandler) actorUUID(r *http.Request) (uuid.NullUUID, string) {
	if h == nil {
		return uuid.NullUUID{}, ""
	}
	userID, _ := api.AuthenticatedUserID(r)
	if userID == "" {
		return uuid.NullUUID{}, ""
	}
	if parsed, err := uuid.Parse(userID); err == nil {
		return uuid.NullUUID{UUID: parsed, Valid: true}, userID
	}
	return uuid.NullUUID{}, userID
}

func (h *adminHandler) reloadProviders(ctx context.Context) error {
	if h == nil || h.providerReloader == nil {
		return nil
	}
	return h.providerReloader.ReloadPlatformProviders(ctx)
}

func (h *adminHandler) routerActiveKeysOrNil() map[string]bool {
	if h == nil || h.modelRouter == nil {
		return nil
	}
	snap := h.modelRouter.SystemAPIKeySnapshot()
	out := make(map[string]bool, len(snap))
	for p, v := range snap {
		out[string(p)] = strings.TrimSpace(v) != ""
	}
	return out
}

func nullUUIDString(u uuid.NullUUID) string {
	if !u.Valid {
		return ""
	}
	return u.UUID.String()
}

// missingProviderKeys returns providers (lowercased) referenced
// by the given arm configs that the in-process router has no
// active key for. Used by the A/B experiment create/update gate
// to prevent the silent "two arms map to the same provider"
// failure mode that motivated S13.
//
// Precedence:
//  1. If the router has the key in-process, we accept (handles
//     the unit-test path where the DB isn't wired).
//  2. Otherwise, if a row exists with status='active' for that
//     provider, we accept (the router will pick it up on the
//     next reload).
//  3. Else: report as missing.
func (h *adminHandler) missingProviderKeys(ctx context.Context, arms []modelab.ArmConfig) []string {
	if h == nil || len(arms) == 0 {
		return nil
	}
	// When neither the in-process router nor the DB-backed repo
	// is wired, the gate has no source of truth to enforce against.
	// Production always has both; this guard keeps legacy/pre-S13
	// tests (which inject only a modelABRepo) from being rejected
	// outright. The backend remains correct in production because
	// both fields are populated by newAdminHandler + attachLLMRuntime.
	if h.modelRouter == nil && h.platformLLMProviderRepo == nil {
		return nil
	}
	missingSet := map[string]struct{}{}
	out := []string{}
	for _, arm := range arms {
		provider := strings.ToLower(strings.TrimSpace(string(arm.Provider)))
		if provider == "" {
			// Empty provider arms are caught by Experiment.Validate().
			continue
		}
		if _, seen := missingSet[provider]; seen {
			continue
		}
		if h.modelRouter != nil && h.modelRouter.HasProviderKey(llm.Provider(provider)) {
			continue
		}
		if h.platformLLMProviderRepo != nil {
			rows, err := h.platformLLMProviderRepo.ListAll(ctx, repository.ListFilters{
				Provider: provider,
				Status:   "active",
			})
			if err == nil && len(rows) > 0 {
				continue
			}
		}
		missingSet[provider] = struct{}{}
		out = append(out, provider)
	}
	return out
}

func isValidationErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"required", "invalid provider", "invalid model_tier",
		"invalid status", "invalid source", "api_key_plaintext required",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
