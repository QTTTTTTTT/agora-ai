// fund_llm_overrides_handler.go — S14.B per-fund LLM provider
// override admin endpoints.
//
//	GET    /api/funds/{fundId}/llm-overrides
//	PUT    /api/funds/{fundId}/llm-overrides            (create or update; id in body)
//	DELETE /api/funds/{fundId}/llm-overrides/{id}
//
// These live on the fund (NOT under /api/admin/) because the strategy
// owner — who may not be a platform admin — owns the fund's LLM
// budget and therefore decides which provider its agents use. Auth
// is scoped via authorizeFundAccess: only the fund's company owner
// can manage its overrides. Platform admins also have access via the
// same auth chain because they own all funds at the company layer.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/repository"
	"github.com/google/uuid"
)

type fundLLMOverridesHandler struct {
	overrideRepo *repository.FundLLMOverrideRepo
	providerRepo *repository.PlatformLLMProviderRepo
	fundRepo     *repository.FundRepo
	companyRepo  *repository.FundCompanyRepo
	// auditLogger writes a mutation event for every upsert/delete.
	// Nil = best-effort skip (handler still works, just no audit).
	// In production wiring this is always a DBLogger; tests can
	// stub with audit.NopLogger or a fake to assert calls.
	auditLogger audit.Logger
}

// newFundLLMOverridesHandler wires the dependencies. Returns nil
// when the override repo is missing — the routes then stay
// unregistered (UI section degrades to "feature unavailable").
func newFundLLMOverridesHandler(svc *Services) *fundLLMOverridesHandler {
	if svc == nil || svc.FundLLMOverrideRepo == nil || svc.DB == nil {
		return nil
	}
	return &fundLLMOverridesHandler{
		overrideRepo: svc.FundLLMOverrideRepo,
		providerRepo: svc.PlatformLLMProviderRepo,
		fundRepo:     repository.NewFundRepo(svc.DB),
		companyRepo:  repository.NewFundCompanyRepo(svc.DB),
		// One audit logger per handler; the underlying DBLogger
		// is stateless beyond its sql.DB handle so creating a new
		// instance is cheap. We mirror the pattern in
		// admin_llm_providers.go for consistency.
		auditLogger: audit.NewDBLogger(svc.DB),
	}
}

func (h *fundLLMOverridesHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/funds/{fundId}/llm-overrides", h.handleList)
	mux.HandleFunc("PUT /api/funds/{fundId}/llm-overrides", h.handleUpsert)
	mux.HandleFunc("DELETE /api/funds/{fundId}/llm-overrides/{id}", h.handleDelete)
}

// --- DTOs --------------------------------------------------------------------

// fundLLMOverrideDTO mirrors a row + computed "effective" fields so
// the UI can show what the override actually resolves to (e.g. when
// label is NULL, what's the platform default we'd pick). API key
// material is NEVER serialised; the dashboard uses the fingerprint
// pattern from S13 if it needs to show "which key".
type fundLLMOverrideDTO struct {
	ID        string  `json:"id"`
	FundID    string  `json:"fund_id"`
	AgentID   *string `json:"agent_id,omitempty"`
	Role      string  `json:"role,omitempty"`
	ModelTier string  `json:"model_tier,omitempty"`

	Provider  string `json:"provider"`
	Label     string `json:"label,omitempty"`
	ModelName string `json:"model_name,omitempty"`

	// Effective resolution shown alongside the raw row. The router
	// uses these (computed at request time) — exposing them here
	// lets the operator preview "this override would resolve to
	// openai/openai-prod/gpt-4o" before committing.
	EffectiveProvider  string `json:"effective_provider,omitempty"`
	EffectiveLabel     string `json:"effective_label,omitempty"`
	EffectiveModelName string `json:"effective_model_name,omitempty"`

	Enabled bool   `json:"enabled"`
	Note    string `json:"note,omitempty"`

	Specificity int    `json:"specificity"`
	CreatedAt   string `json:"created_at,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

type upsertFundLLMOverrideRequest struct {
	ID        string  `json:"id,omitempty"` // empty = create
	AgentID   *string `json:"agent_id,omitempty"`
	Role      string  `json:"role,omitempty"`
	ModelTier string  `json:"model_tier,omitempty"`
	Provider  string  `json:"provider"`
	Label     string  `json:"label,omitempty"`
	ModelName string  `json:"model_name,omitempty"`
	Enabled   bool    `json:"enabled"`
	Note      string  `json:"note,omitempty"`
}

// --- Handlers ----------------------------------------------------------------

// errUnauthorizedFundOverride is a sentinel for "no bearer token" so
// authorize() can return distinct values that writeFundOverrideAuthError
// can map. We use a local sentinel because the api package only has
// ErrForbidden / ErrNotFound — not an explicit "unauthenticated" type.
var errUnauthorizedFundOverride = errors.New("fund_override: unauthenticated")
var errBadFundIDFundOverride = errors.New("fund_override: fundId required")

func (h *fundLLMOverridesHandler) authorize(r *http.Request) (string, string, error) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		return "", "", errUnauthorizedFundOverride
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		return "", "", errBadFundIDFundOverride
	}
	if _, err := authorizeFundAccess(r.Context(), h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		return "", "", err
	}
	return userID, fundID, nil
}

func (h *fundLLMOverridesHandler) handleList(w http.ResponseWriter, r *http.Request) {
	_, fundID, err := h.authorize(r)
	if err != nil {
		writeFundOverrideAuthError(w, err)
		return
	}
	fundUUID, perr := uuid.Parse(fundID)
	if perr != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fund id is not a UUID"))
		return
	}
	rows, err := h.overrideRepo.ListByFund(r.Context(), fundUUID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("list_failed", err.Error()))
		return
	}
	out := make([]fundLLMOverrideDTO, 0, len(rows))
	for i := range rows {
		out = append(out, h.dtoFromRow(r, &rows[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"overrides": out})
}

func (h *fundLLMOverridesHandler) handleUpsert(w http.ResponseWriter, r *http.Request) {
	userID, fundID, err := h.authorize(r)
	if err != nil {
		writeFundOverrideAuthError(w, err)
		return
	}
	fundUUID, perr := uuid.Parse(fundID)
	if perr != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fund id is not a UUID"))
		return
	}
	var req upsertFundLLMOverrideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("decode_failed", err.Error()))
		return
	}
	if strings.TrimSpace(req.Provider) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, errorPayload("validation", "provider is required"))
		return
	}

	params := repository.UpsertParams{
		FundID:    fundUUID,
		Role:      req.Role,
		ModelTier: req.ModelTier,
		Provider:  req.Provider,
		Label:     req.Label,
		ModelName: req.ModelName,
		Enabled:   req.Enabled,
		Note:      req.Note,
	}
	// Capture "before" state for the audit row BEFORE the write so
	// the chain reflects the actual transition. For creates (no ID)
	// before stays nil.
	var before *repository.FundLLMOverrideRow
	if req.ID != "" {
		id, err := uuid.Parse(req.ID)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorPayload("invalid_id", "id is not a UUID"))
			return
		}
		params.ID = &id
		if prev, gerr := h.overrideRepo.Get(r.Context(), id); gerr == nil {
			before = prev
		}
		// A Get error here is non-fatal: the row may not exist yet
		// (treated as create) or the DB may be transiently unhappy.
		// We do NOT block the mutation on audit-state collection.
	}
	if req.AgentID != nil && strings.TrimSpace(*req.AgentID) != "" {
		agentID, err := uuid.Parse(strings.TrimSpace(*req.AgentID))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorPayload("invalid_agent_id", "agent_id is not a UUID"))
			return
		}
		params.AgentID = &agentID
	}
	if actorUUID, err := uuid.Parse(strings.TrimSpace(userID)); err == nil {
		params.ActorID = &actorUUID
	}

	row, err := h.overrideRepo.Upsert(r.Context(), params)
	if err != nil {
		if errors.Is(err, repository.ErrFundLLMOverrideInvalidProvider) {
			writeJSON(w, http.StatusUnprocessableEntity,
				errorPayload("invalid_provider", err.Error()))
			return
		}
		if errors.Is(err, repository.ErrFundLLMOverrideNotFound) {
			writeJSON(w, http.StatusNotFound, errorPayload("not_found", err.Error()))
			return
		}
		// Common path: unique index conflict on (fund, agent, role, tier).
		if strings.Contains(err.Error(), "uniq_fund_llm_overrides_scope") {
			writeJSON(w, http.StatusConflict,
				errorPayload("conflict_scope",
					"an override with this (agent, role, tier) scope already exists for this fund"))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorPayload("upsert_failed", err.Error()))
		return
	}
	// Emit audit: distinguish create (before=nil) from update
	// (before!=nil). Marketplace economics demand we can answer
	// "who replaced gemini with claude on fund X at 14:32?" — that
	// answer lives in admin_change_log.
	action := "fund_llm_override.create"
	if before != nil {
		action = "fund_llm_override.update"
	}
	h.auditChange(r.Context(), r, action, userID, fundUUID, row, before, row)
	writeJSON(w, http.StatusOK, h.dtoFromRow(r, row))
}

func (h *fundLLMOverridesHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	userID, fundID, err := h.authorize(r)
	if err != nil {
		writeFundOverrideAuthError(w, err)
		return
	}
	fundUUID, perr := uuid.Parse(fundID)
	if perr != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fund id is not a UUID"))
		return
	}
	idStr := strings.TrimSpace(r.PathValue("id"))
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_id", "id is not a UUID"))
		return
	}
	// Capture before-snapshot for the audit row. Missing-row case
	// is handled below (the Delete itself returns ErrNotFound).
	before, _ := h.overrideRepo.Get(r.Context(), id)
	if err := h.overrideRepo.Delete(r.Context(), id); err != nil {
		if errors.Is(err, repository.ErrFundLLMOverrideNotFound) {
			writeJSON(w, http.StatusNotFound, errorPayload("not_found", err.Error()))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorPayload("delete_failed", err.Error()))
		return
	}
	h.auditChange(r.Context(), r, "fund_llm_override.delete", userID, fundUUID, before, before, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted_id": id.String()})
}

// auditChange writes one mutation event for a fund-llm-override
// create/update/delete. Failures are intentionally swallowed: an
// audit row going missing is bad, but it MUST NOT roll back the
// mutation we already committed (admin_change_log is append-only
// observability, not a control-plane gate).
//
// rowForTarget is whichever row best identifies the resource for
// the TargetID column — for delete that's the pre-delete row, for
// upsert it's the new row.
func (h *fundLLMOverridesHandler) auditChange(ctx context.Context, r *http.Request, action, actorUserID string, fundID uuid.UUID, rowForTarget *repository.FundLLMOverrideRow, before, after *repository.FundLLMOverrideRow) {
	if h == nil || h.auditLogger == nil {
		return
	}
	targetID := ""
	if rowForTarget != nil {
		targetID = rowForTarget.ID.String()
	}
	meta := map[string]any{
		"fund_id": fundID.String(),
	}
	// IP and user-agent help an operator trace a hostile change
	// back to a specific session. Both are best-effort; missing
	// values just degrade the diagnostic, never break the audit.
	if r != nil {
		if ip := strings.TrimSpace(clientIP(r)); ip != "" {
			meta["client_ip"] = ip
		}
		if ua := strings.TrimSpace(r.UserAgent()); ua != "" {
			meta["user_agent"] = ua
		}
	}
	_ = h.auditLogger.LogMutation(ctx, audit.MutationEvent{
		ActorUserID: strings.TrimSpace(actorUserID),
		Action:      action,
		TargetType:  "fund_llm_overrides",
		TargetID:    targetID,
		Before:      fundOverrideAuditPayload(before),
		After:       fundOverrideAuditPayload(after),
		Metadata:    meta,
	})
}

// fundOverrideAuditPayload projects a row into a stable JSON map for
// the audit log. We omit unconstrained free-text (note) is INCLUDED
// because operators sometimes record "why" in the note and the audit
// is the only place we keep that. created_by/updated_by are also
// included so a forensic reader can correlate to admin_change_log.
func fundOverrideAuditPayload(row *repository.FundLLMOverrideRow) any {
	if row == nil {
		return nil
	}
	out := map[string]any{
		"id":         row.ID.String(),
		"fund_id":    row.FundID.String(),
		"provider":   row.Provider,
		"enabled":    row.Enabled,
		"created_at": row.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		"updated_at": row.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if row.AgentID.Valid {
		out["agent_id"] = row.AgentID.UUID.String()
	}
	if row.Role.Valid {
		out["role"] = row.Role.String
	}
	if row.ModelTier.Valid {
		out["model_tier"] = row.ModelTier.String
	}
	if row.Label.Valid {
		out["label"] = row.Label.String
	}
	if row.ModelName.Valid {
		out["model_name"] = row.ModelName.String
	}
	if row.Note.Valid {
		out["note"] = row.Note.String
	}
	if row.CreatedBy.Valid {
		out["created_by"] = row.CreatedBy.UUID.String()
	}
	if row.UpdatedBy.Valid {
		out["updated_by"] = row.UpdatedBy.UUID.String()
	}
	return out
}

// --- Helpers -----------------------------------------------------------------

// dtoFromRow projects a repository row into the API DTO. The
// "effective" fields are resolved against platform_llm_providers
// so the UI can show "this override → openai/openai-prod" instead
// of "openai/(default)" which is ambiguous.
func (h *fundLLMOverridesHandler) dtoFromRow(r *http.Request, row *repository.FundLLMOverrideRow) fundLLMOverrideDTO {
	d := fundLLMOverrideDTO{
		ID:          row.ID.String(),
		FundID:      row.FundID.String(),
		Provider:    row.Provider,
		Enabled:     row.Enabled,
		Specificity: row.Specificity(),
		CreatedAt:   row.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:   row.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if row.AgentID.Valid {
		s := row.AgentID.UUID.String()
		d.AgentID = &s
	}
	if row.Role.Valid {
		d.Role = row.Role.String
	}
	if row.ModelTier.Valid {
		d.ModelTier = row.ModelTier.String
	}
	if row.Label.Valid {
		d.Label = row.Label.String
	}
	if row.ModelName.Valid {
		d.ModelName = row.ModelName.String
	}
	if row.Note.Valid {
		d.Note = row.Note.String
	}
	// Best-effort effective resolution. Errors are silently dropped:
	// the UI just shows the raw row, and the live router still does
	// the lookup at request time.
	if h.providerRepo != nil {
		var pr *repository.PlatformLLMProviderRow
		var perr error
		if row.Label.Valid && strings.TrimSpace(row.Label.String) != "" {
			pr, perr = h.providerRepo.GetByProviderLabel(r.Context(), row.Provider, row.Label.String)
		} else {
			pr, perr = h.providerRepo.GetActiveDefaultForProvider(r.Context(), row.Provider)
		}
		if perr == nil && pr != nil {
			d.EffectiveProvider = pr.Provider
			d.EffectiveLabel = pr.Label
			effModel := pr.ModelName
			if row.ModelName.Valid && strings.TrimSpace(row.ModelName.String) != "" {
				effModel = row.ModelName.String
			}
			d.EffectiveModelName = effModel
		}
	}
	return d
}

func writeFundOverrideAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errUnauthorizedFundOverride):
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
	case errors.Is(err, errBadFundIDFundOverride):
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_path", err.Error()))
	case errors.Is(err, api.ErrForbidden):
		writeJSON(w, http.StatusForbidden, errorPayload("forbidden", "you do not own this fund"))
	case errors.Is(err, api.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorPayload("not_found", "fund not found"))
	case errors.Is(err, api.ErrBadInput):
		writeJSON(w, http.StatusBadRequest, errorPayload("bad_request", err.Error()))
	default:
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
	}
}
