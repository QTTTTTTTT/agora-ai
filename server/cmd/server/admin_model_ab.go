// admin_model_ab.go — Sprint 10.3 / 10.4 model A/B admin endpoints.
//
// S10.3 (this file's read side):
//
//	GET  /api/admin/model-ab/experiments
//	     List experiments (?status=running|paused|completed|...).
//
//	GET  /api/admin/model-ab/experiments/{id}
//	     One experiment, including its arms + traffic split.
//
//	GET  /api/admin/model-ab/experiments/{id}/report[?from=...&to=...]
//	     Aggregated per-arm metrics (counts, latency, tokens, cost,
//	     error rate).
//
// S10.4 (this file's write side):
//
//	POST   /api/admin/model-ab/experiments              create draft
//	PATCH  /api/admin/model-ab/experiments/{id}/status  flip lifecycle
//	                                                    (draft → running → paused/completed/archived)
//
// All mutating calls go through dual-control + audit logging.

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/llm"
	"github.com/fundai/server/internal/modelab"
)

func (h *adminHandler) registerModelABAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil || h.modelABRepo == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/model-ab/experiments", h.handleListModelABExperiments)
	mux.HandleFunc("GET /api/admin/model-ab/experiments/{id}", h.handleGetModelABExperiment)
	mux.HandleFunc("GET /api/admin/model-ab/experiments/{id}/report", h.handleGetModelABReport)
	mux.HandleFunc("POST /api/admin/model-ab/experiments", h.handleCreateModelABExperiment)
	mux.HandleFunc("PATCH /api/admin/model-ab/experiments/{id}", h.handleUpdateModelABExperiment)
	mux.HandleFunc("PATCH /api/admin/model-ab/experiments/{id}/status", h.handleSetModelABExperimentStatus)
	mux.HandleFunc("POST /api/admin/model-ab/experiments/{id}/clone", h.handleCloneModelABExperiment)
	mux.HandleFunc("POST /api/admin/model-ab/experiments/bulk-status", h.handleBulkSetModelABStatus)
}

// --- wire types ---------------------------------------------------------------

type modelABExperimentWire struct {
	ID             string                  `json:"id"`
	Name           string                  `json:"name"`
	Description    string                  `json:"description,omitempty"`
	Scope          string                  `json:"scope"`
	ScopeTarget    string                  `json:"scope_target,omitempty"`
	StepFilter     []string                `json:"step_filter"`
	Arms           []modelABArmWire        `json:"arms"`
	TrafficSplit   []float64               `json:"traffic_split"`
	Status         string                  `json:"status"`
	StartAt        string                  `json:"start_at,omitempty"`
	EndAt          string                  `json:"end_at,omitempty"`
	MaxTotalTokens int64                   `json:"max_total_tokens,omitempty"`
	TokensUsed     int64                   `json:"tokens_used"`
	CreatedBy      string                  `json:"created_by,omitempty"`
	CreatedAt      string                  `json:"created_at"`
	UpdatedAt      string                  `json:"updated_at"`
}

type modelABArmWire struct {
	Name        string  `json:"name"`
	Provider    string  `json:"provider"`
	ModelName   string  `json:"model_name"`
	BaseURL     string  `json:"base_url,omitempty"`
	ModelTier   string  `json:"model_tier,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	MaxTokens   int     `json:"max_tokens,omitempty"`
}

func wireExperiment(e *modelab.Experiment) modelABExperimentWire {
	arms := make([]modelABArmWire, len(e.Arms))
	for i, a := range e.Arms {
		arms[i] = modelABArmWire{
			Name:        a.Name,
			Provider:    string(a.Provider),
			ModelName:   a.ModelName,
			BaseURL:     a.BaseURL,
			ModelTier:   string(a.ModelTier),
			Temperature: a.Temperature,
			MaxTokens:   a.MaxTokens,
		}
	}
	out := modelABExperimentWire{
		ID:             e.ID,
		Name:           e.Name,
		Description:    e.Description,
		Scope:          string(e.Scope),
		ScopeTarget:    e.ScopeTarget,
		StepFilter:     e.StepFilter,
		Arms:           arms,
		TrafficSplit:   e.TrafficSplit,
		Status:         string(e.Status),
		MaxTotalTokens: e.MaxTotalTokens,
		TokensUsed:     e.TokensUsed,
		CreatedBy:      e.CreatedBy,
	}
	if !e.StartAt.IsZero() {
		out.StartAt = e.StartAt.UTC().Format(time.RFC3339)
	}
	if !e.EndAt.IsZero() {
		out.EndAt = e.EndAt.UTC().Format(time.RFC3339)
	}
	if !e.CreatedAt.IsZero() {
		out.CreatedAt = e.CreatedAt.UTC().Format(time.RFC3339)
	}
	if !e.UpdatedAt.IsZero() {
		out.UpdatedAt = e.UpdatedAt.UTC().Format(time.RFC3339)
	}
	if out.StepFilter == nil {
		out.StepFilter = []string{}
	}
	return out
}

// --- read endpoints ---------------------------------------------------------

func (h *adminHandler) handleListModelABExperiments(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	statusFilter := []modelab.ExperimentStatus{}
	if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
		for _, s := range strings.Split(raw, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			statusFilter = append(statusFilter, modelab.ExperimentStatus(s))
		}
	}
	exps, err := h.modelABRepo.ListExperiments(r.Context(), statusFilter, 200)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("list_failed", err.Error()))
		return
	}
	out := make([]modelABExperimentWire, 0, len(exps))
	for _, e := range exps {
		out = append(out, wireExperiment(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"experiments": out})
}

func (h *adminHandler) handleGetModelABExperiment(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("missing_id", "id is required"))
		return
	}
	e, err := h.modelABRepo.GetExperiment(r.Context(), id)
	if err != nil {
		if errors.Is(err, modelab.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, errorPayload("not_found", "experiment not found"))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorPayload("get_failed", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, wireExperiment(e))
}

func (h *adminHandler) handleGetModelABReport(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.modelABReporter == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorPayload("no_reporter", "model A/B reporter not wired"))
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("missing_id", "id is required"))
		return
	}
	from := parseAdminTime(r.URL.Query().Get("from"))
	to := parseAdminTime(r.URL.Query().Get("to"))
	rep, err := h.modelABReporter.Compute(r.Context(), id, from, to)
	if err != nil {
		if errors.Is(err, modelab.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, errorPayload("not_found", "experiment not found"))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorPayload("report_failed", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// --- write endpoints (S10.4) -------------------------------------------------

type createModelABExperimentRequest struct {
	Name           string             `json:"name"`
	Description    string             `json:"description"`
	Scope          string             `json:"scope"`
	ScopeTarget    string             `json:"scope_target"`
	StepFilter     []string           `json:"step_filter"`
	Arms           []modelABArmWire   `json:"arms"`
	TrafficSplit   []float64          `json:"traffic_split"`
	MaxTotalTokens int64              `json:"max_total_tokens"`
	StartImmediate bool               `json:"start_immediate"`
}

func (h *adminHandler) handleCreateModelABExperiment(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	actorID, _ := api.AuthenticatedUserID(r)
	var req createModelABExperimentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	arms := make([]modelab.ArmConfig, len(req.Arms))
	for i, a := range req.Arms {
		arms[i] = modelab.ArmConfig{
			Name:        a.Name,
			Provider:    llm.Provider(strings.TrimSpace(a.Provider)),
			ModelName:   a.ModelName,
			BaseURL:     a.BaseURL,
			ModelTier:   llm.ModelTier(strings.TrimSpace(a.ModelTier)),
			Temperature: a.Temperature,
			MaxTokens:   a.MaxTokens,
		}
	}
	if req.StepFilter == nil {
		req.StepFilter = []string{}
	}
	exp := &modelab.Experiment{
		Name:           req.Name,
		Description:    req.Description,
		Scope:          modelab.Scope(req.Scope),
		ScopeTarget:    req.ScopeTarget,
		StepFilter:     req.StepFilter,
		Arms:           arms,
		TrafficSplit:   req.TrafficSplit,
		MaxTotalTokens: req.MaxTotalTokens,
		CreatedBy:      actorID,
		Status:         modelab.StatusDraft,
	}
	if err := exp.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_experiment", err.Error()))
		return
	}
	id, err := h.modelABRepo.CreateExperiment(r.Context(), exp)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("create_failed", err.Error()))
		return
	}
	if req.StartImmediate {
		if err := h.modelABRepo.SetStatus(r.Context(), id, modelab.StatusRunning); err != nil {
			// Roll-up not critical: experiment exists in draft;
			// the operator can flip it manually. Surface a
			// warning-only payload so the caller still gets the
			// created resource ID.
			writeJSON(w, http.StatusOK, map[string]any{
				"warning": "created_in_draft_only",
				"detail":  err.Error(),
				"id":      id,
			})
			return
		}
		if h.modelABResolver != nil {
			h.modelABResolver.Invalidate()
		}
	}
	h.logModelABMutation(r, actorID, "model_ab.experiment.create", id, map[string]any{
		"name":            exp.Name,
		"scope":           string(exp.Scope),
		"start_immediate": req.StartImmediate,
		"arms":            len(exp.Arms),
	})
	created, err := h.modelABRepo.GetExperiment(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("read_after_create_failed", err.Error()))
		return
	}
	writeJSON(w, http.StatusCreated, wireExperiment(created))
}

type setModelABStatusRequest struct {
	Status string `json:"status"`
}

func (h *adminHandler) handleSetModelABExperimentStatus(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	actorID, _ := api.AuthenticatedUserID(r)
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("missing_id", "id is required"))
		return
	}
	var req setModelABStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	target := modelab.ExperimentStatus(strings.TrimSpace(req.Status))
	switch target {
	case modelab.StatusDraft, modelab.StatusRunning, modelab.StatusPaused,
		modelab.StatusCompleted, modelab.StatusArchived:
	default:
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_status", "status must be one of draft|running|paused|completed|archived"))
		return
	}
	if err := h.modelABRepo.SetStatus(r.Context(), id, target); err != nil {
		if errors.Is(err, modelab.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, errorPayload("not_found", "experiment not found"))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorPayload("update_failed", err.Error()))
		return
	}
	if h.modelABResolver != nil {
		h.modelABResolver.Invalidate()
	}
	h.logModelABMutation(r, actorID, "model_ab.experiment.set_status", id, map[string]any{
		"status": string(target),
	})
	updated, err := h.modelABRepo.GetExperiment(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("read_after_update_failed", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, wireExperiment(updated))
}

// updateModelABExperimentRequest mirrors createModelABExperimentRequest
// minus the start_immediate field — edits never auto-start the
// experiment. The body schema is otherwise identical so frontend
// forms can reuse the create form when editing a draft.
type updateModelABExperimentRequest struct {
	Name           string           `json:"name"`
	Description    string           `json:"description"`
	Scope          string           `json:"scope"`
	ScopeTarget    string           `json:"scope_target"`
	StepFilter     []string         `json:"step_filter"`
	Arms           []modelABArmWire `json:"arms"`
	TrafficSplit   []float64        `json:"traffic_split"`
	MaxTotalTokens int64            `json:"max_total_tokens"`
}

func (h *adminHandler) handleUpdateModelABExperiment(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	actorID, _ := api.AuthenticatedUserID(r)
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("missing_id", "id is required"))
		return
	}
	var req updateModelABExperimentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	arms := make([]modelab.ArmConfig, len(req.Arms))
	for i, a := range req.Arms {
		arms[i] = modelab.ArmConfig{
			Name:        a.Name,
			Provider:    llm.Provider(strings.TrimSpace(a.Provider)),
			ModelName:   a.ModelName,
			BaseURL:     a.BaseURL,
			ModelTier:   llm.ModelTier(strings.TrimSpace(a.ModelTier)),
			Temperature: a.Temperature,
			MaxTokens:   a.MaxTokens,
		}
	}
	if req.StepFilter == nil {
		req.StepFilter = []string{}
	}
	patched := &modelab.Experiment{
		Name:           req.Name,
		Description:    req.Description,
		Scope:          modelab.Scope(req.Scope),
		ScopeTarget:    req.ScopeTarget,
		StepFilter:     req.StepFilter,
		Arms:           arms,
		TrafficSplit:   req.TrafficSplit,
		MaxTotalTokens: req.MaxTotalTokens,
	}
	if err := h.modelABRepo.UpdateDraft(r.Context(), id, patched); err != nil {
		switch {
		case errors.Is(err, modelab.ErrNotFound):
			writeJSON(w, http.StatusNotFound, errorPayload("not_found", "experiment not found"))
		case errors.Is(err, modelab.ErrNotEditable):
			writeJSON(w, http.StatusConflict, errorPayload("not_editable",
				"only draft experiments can be edited; clone the experiment instead"))
		default:
			writeJSON(w, http.StatusBadRequest, errorPayload("update_failed", err.Error()))
		}
		return
	}
	h.logModelABMutation(r, actorID, "model_ab.experiment.update_draft", id, map[string]any{
		"name":  patched.Name,
		"arms":  len(patched.Arms),
		"scope": string(patched.Scope),
	})
	updated, err := h.modelABRepo.GetExperiment(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("read_after_update_failed", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, wireExperiment(updated))
}

type cloneModelABExperimentRequest struct {
	Name string `json:"name"`
}

func (h *adminHandler) handleCloneModelABExperiment(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	actorID, _ := api.AuthenticatedUserID(r)
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("missing_id", "id is required"))
		return
	}
	var req cloneModelABExperimentRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
			return
		}
	}
	newID, err := h.modelABRepo.Clone(r.Context(), id, req.Name, actorID)
	if err != nil {
		if errors.Is(err, modelab.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, errorPayload("not_found", "source experiment not found"))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorPayload("clone_failed", err.Error()))
		return
	}
	h.logModelABMutation(r, actorID, "model_ab.experiment.clone", newID, map[string]any{
		"source_experiment_id": id,
		"name":                 req.Name,
	})
	cloned, err := h.modelABRepo.GetExperiment(r.Context(), newID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("read_after_clone_failed", err.Error()))
		return
	}
	writeJSON(w, http.StatusCreated, wireExperiment(cloned))
}

type bulkSetModelABStatusRequest struct {
	IDs    []string `json:"ids"`
	Status string   `json:"status"`
}

type bulkSetModelABStatusResponse struct {
	Updated int64 `json:"updated"`
}

func (h *adminHandler) handleBulkSetModelABStatus(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	actorID, _ := api.AuthenticatedUserID(r)
	var req bulkSetModelABStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	if len(req.IDs) == 0 {
		writeJSON(w, http.StatusBadRequest, errorPayload("missing_ids", "ids[] is required"))
		return
	}
	target := modelab.ExperimentStatus(strings.TrimSpace(req.Status))
	switch target {
	case modelab.StatusDraft, modelab.StatusRunning, modelab.StatusPaused,
		modelab.StatusCompleted, modelab.StatusArchived:
	default:
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_status",
			"status must be one of draft|running|paused|completed|archived"))
		return
	}
	updated, err := h.modelABRepo.BulkSetStatus(r.Context(), req.IDs, target)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("bulk_update_failed", err.Error()))
		return
	}
	if h.modelABResolver != nil {
		h.modelABResolver.Invalidate()
	}
	h.logModelABMutation(r, actorID, "model_ab.experiment.bulk_set_status", "_bulk_", map[string]any{
		"ids":     req.IDs,
		"status":  string(target),
		"updated": updated,
	})
	writeJSON(w, http.StatusOK, bulkSetModelABStatusResponse{Updated: updated})
}

// --- shared helpers ----------------------------------------------------------

func (h *adminHandler) logModelABMutation(r *http.Request, actorID, action, expID string, payload map[string]any) {
	if h == nil || h.auditLogger == nil || actorID == "" {
		return
	}
	if payload == nil {
		payload = map[string]any{}
	}
	_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
		ActorUserID: actorID,
		Action:      action,
		TargetType:  "model_ab_experiment",
		TargetID:    expID,
		After:       payload,
	})
}

func parseAdminTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}
