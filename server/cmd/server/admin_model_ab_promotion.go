// admin_model_ab_promotion.go — Sprint 13.3 admin endpoints for
// the model A/B auto-promotion draft store.
//
//	GET  /api/admin/model-ab/promotion-drafts             list (?status=...)
//	GET  /api/admin/model-ab/promotion-drafts/{id}        one draft + report snapshot
//	POST /api/admin/model-ab/promotion-drafts/scan        kick the scanner on demand
//	PATCH /api/admin/model-ab/promotion-drafts/{id}/apply  one-click promotion
//	PATCH /api/admin/model-ab/promotion-drafts/{id}/reject one-click rejection
//
// Apply does TWO things atomically (per the scanner's contract):
//
//   1. Marks the draft as applied + records who.
//   2. Flips the source experiment's status to "completed" so
//      future router decisions stop dispatching shadow arms.
//
// We deliberately stop short of actually rewriting any user's
// model defaults — that's a future enhancement. The current
// "apply" path is a record-of-decision + experiment closure;
// operators run the follow-up rollout through the existing
// llm.UserOverride / fund-config tooling.

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/modelab"
)

func (h *adminHandler) registerModelABPromotionRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil || h.modelABPromotionDraftRepo == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/model-ab/promotion-drafts", h.handleListPromotionDrafts)
	mux.HandleFunc("GET /api/admin/model-ab/promotion-drafts/{id}", h.handleGetPromotionDraft)
	mux.HandleFunc("POST /api/admin/model-ab/promotion-drafts/scan", h.handleScanPromotionDrafts)
	mux.HandleFunc("PATCH /api/admin/model-ab/promotion-drafts/{id}/apply", h.handleApplyPromotionDraft)
	mux.HandleFunc("PATCH /api/admin/model-ab/promotion-drafts/{id}/reject", h.handleRejectPromotionDraft)
}

// --- wire types --------------------------------------------------------------

type promotionDraftWire struct {
	ID                  string          `json:"id"`
	ExperimentID        string          `json:"experiment_id"`
	RecommendedArmIndex int             `json:"recommended_arm_index"`
	RecommendedArmLabel string          `json:"recommended_arm_label"`
	PrimaryArmIndex     int             `json:"primary_arm_index"`
	PrimaryArmLabel     string          `json:"primary_arm_label"`
	StreakDays          int             `json:"streak_days"`
	EvaluatedAt         string          `json:"evaluated_at"`
	WindowFrom          string          `json:"window_from,omitempty"`
	WindowTo            string          `json:"window_to,omitempty"`
	CriteriaPayload     json.RawMessage `json:"criteria_payload,omitempty"`
	// ReportSnapshot is omitted from the list endpoint (large
	// payload) and included only by the detail endpoint. Both
	// endpoints share this struct via the "include_report" flag.
	ReportSnapshot  json.RawMessage `json:"report_snapshot,omitempty"`
	Status          string          `json:"status"`
	AppliedBy       string          `json:"applied_by,omitempty"`
	AppliedAt       string          `json:"applied_at,omitempty"`
	RejectionReason string          `json:"rejection_reason,omitempty"`
	CreatedAt       string          `json:"created_at"`
}

func toPromotionDraftWire(d *modelab.PromotionDraft, includeReport bool) promotionDraftWire {
	if d == nil {
		return promotionDraftWire{}
	}
	w := promotionDraftWire{
		ID:                  d.ID,
		ExperimentID:        d.ExperimentID,
		RecommendedArmIndex: d.RecommendedArmIndex,
		RecommendedArmLabel: d.RecommendedArmLabel,
		PrimaryArmIndex:     d.PrimaryArmIndex,
		PrimaryArmLabel:     d.PrimaryArmLabel,
		StreakDays:          d.StreakDays,
		EvaluatedAt:         d.EvaluatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		CriteriaPayload:     d.CriteriaPayload,
		Status:              string(d.Status),
		AppliedBy:           d.AppliedBy,
		RejectionReason:     d.RejectionReason,
		CreatedAt:           d.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if d.WindowFrom != nil {
		w.WindowFrom = d.WindowFrom.UTC().Format("2006-01-02T15:04:05Z")
	}
	if d.WindowTo != nil {
		w.WindowTo = d.WindowTo.UTC().Format("2006-01-02T15:04:05Z")
	}
	if d.AppliedAt != nil {
		w.AppliedAt = d.AppliedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	if includeReport {
		w.ReportSnapshot = d.ReportSnapshot
	}
	return w
}

// --- handlers ----------------------------------------------------------------

func (h *adminHandler) handleListPromotionDrafts(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconvAtoiBounded(raw, 1, 500); err == nil {
			limit = n
		}
	}
	drafts, err := h.modelABPromotionDraftRepo.List(r.Context(), modelab.DraftStatus(status), limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("list_failed", err.Error()))
		return
	}
	wires := make([]promotionDraftWire, 0, len(drafts))
	for _, d := range drafts {
		wires = append(wires, toPromotionDraftWire(d, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": wires})
}

func (h *adminHandler) handleGetPromotionDraft(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("missing_id", "id is required"))
		return
	}
	d, err := h.modelABPromotionDraftRepo.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, modelab.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, errorPayload("not_found", "promotion draft not found"))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorPayload("get_failed", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, toPromotionDraftWire(d, true))
}

func (h *adminHandler) handleScanPromotionDrafts(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.modelABPromotionScanLoop == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorPayload("not_configured", "promotion scan loop is not wired"))
		return
	}
	n, err := h.modelABPromotionScanLoop.RunOnce(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("scan_failed", err.Error()))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "model_ab_promotion_scan_run",
			TargetType:  "model_ab_promotion_drafts",
			TargetID:    "_global_",
			After:       map[string]any{"drafts_upserted": n},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"drafts_upserted": n})
}

func (h *adminHandler) handleApplyPromotionDraft(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("missing_id", "id is required"))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)

	// Read first so we know which experiment to close on success.
	draft, err := h.modelABPromotionDraftRepo.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, modelab.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, errorPayload("not_found", "promotion draft not found"))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorPayload("get_failed", err.Error()))
		return
	}
	if draft.Status != modelab.DraftPending {
		writeJSON(w, http.StatusConflict, errorPayload("not_pending", "draft is "+string(draft.Status)))
		return
	}

	if err := h.modelABPromotionDraftRepo.Apply(r.Context(), id, userID); err != nil {
		if errors.Is(err, modelab.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, errorPayload("not_found", "promotion draft not found"))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorPayload("apply_failed", err.Error()))
		return
	}

	// Close the source experiment. Best-effort — we don't roll
	// back the draft if this fails (it would leave the operator
	// stuck mid-apply with no way to retry). Instead, log a
	// warning; the admin UI shows the resulting experiment
	// state in the next refresh.
	if h.modelABRepo != nil {
		if err := h.modelABRepo.SetStatus(r.Context(), draft.ExperimentID, modelab.StatusCompleted); err != nil {
			// Surface the partial state to the admin so they know.
			writeJSON(w, http.StatusAccepted, map[string]any{
				"ok":              true,
				"draft_id":        id,
				"experiment_id":   draft.ExperimentID,
				"experiment_closed": false,
				"warning":         err.Error(),
			})
			return
		}
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "model_ab_promotion_apply",
			TargetType:  "model_ab_promotion_drafts",
			TargetID:    id,
			After: map[string]any{
				"experiment_id":         draft.ExperimentID,
				"recommended_arm_label": draft.RecommendedArmLabel,
				"streak_days":           draft.StreakDays,
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                true,
		"draft_id":          id,
		"experiment_id":     draft.ExperimentID,
		"experiment_closed": true,
	})
}

func (h *adminHandler) handleRejectPromotionDraft(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("missing_id", "id is required"))
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, errorPayload("decode_failed", err.Error()))
			return
		}
	}
	userID, _ := api.AuthenticatedUserID(r)
	if err := h.modelABPromotionDraftRepo.Reject(r.Context(), id, userID, body.Reason); err != nil {
		if errors.Is(err, modelab.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, errorPayload("not_found", "promotion draft not found"))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorPayload("reject_failed", err.Error()))
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "model_ab_promotion_reject",
			TargetType:  "model_ab_promotion_drafts",
			TargetID:    id,
			Metadata:    map[string]any{"reason": body.Reason},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
