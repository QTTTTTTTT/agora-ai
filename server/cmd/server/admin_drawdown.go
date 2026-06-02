// admin_drawdown.go — admin REST surface for the drawdown soft
// circuit breaker (P3-5).
//
// Endpoints
//
//   GET    /api/admin/drawdown/funds/{fundId}/policy           current tier list
//   PUT    /api/admin/drawdown/funds/{fundId}/policy/tiers/{tier}  upsert one tier
//   DELETE /api/admin/drawdown/funds/{fundId}/policy/tiers/{tier}  remove one tier
//   GET    /api/admin/drawdown/funds/{fundId}/status            live DD + applicable tier preview
//   POST   /api/admin/drawdown/funds/{fundId}/check             on-demand check (records event if breach)
//   GET    /api/admin/drawdown/events                           list events (?fund_id, ?status, ?limit)
//   GET    /api/admin/drawdown/events/{id}                      one event with trim_plan
//   POST   /api/admin/drawdown/events/{id}/review               body {status, note} — approve/dismiss/superseded
//
// AuthZ: requireAdmin. Audit logged.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/drawdown"
)

// drawdownTierWire is the on-wire shape for one tier row.
type drawdownTierWire struct {
	Tier          int     `json:"tier"`
	DDPct         float64 `json:"dd_pct"`
	Action        string  `json:"action"`
	TrimRatio     float64 `json:"trim_ratio"`
	CooldownHours int     `json:"cooldown_hours"`
	AutoExecute   bool    `json:"auto_execute"`
	Note          string  `json:"note,omitempty"`
}

// drawdownPolicyWire is the policy response.
type drawdownPolicyWire struct {
	FundID string             `json:"fund_id"`
	Tiers  []drawdownTierWire `json:"tiers"`
}

// drawdownEventWire is the on-wire event row. trim_plan is left
// untyped because the schema is small (symbol/side/qty/reason)
// and the UI renders raw.
type drawdownEventWire struct {
	ID              string                  `json:"id"`
	FundID          string                  `json:"fund_id"`
	Tier            int                     `json:"tier"`
	CurrentDDPct    float64                 `json:"current_dd_pct"`
	PeakNAV         float64                 `json:"peak_nav"`
	CurrentNAV      float64                 `json:"current_nav"`
	Action          string                  `json:"action"`
	TrimPlan        []drawdown.TrimPlanItem `json:"trim_plan"`
	TradeIDs        []string                `json:"trade_ids,omitempty"`
	Status          string                  `json:"status"`
	ReviewNote      string                  `json:"review_note,omitempty"`
	ReviewedBy      string                  `json:"reviewed_by,omitempty"`
	ReviewedAt      string                  `json:"reviewed_at,omitempty"`
	NavSnapshotID   string                  `json:"nav_snapshot_id,omitempty"`
	DetectedAt      string                  `json:"detected_at"`
	DetectorVersion string                  `json:"detector_version,omitempty"`
	Metadata        map[string]any          `json:"metadata,omitempty"`
	CreatedAt       string                  `json:"created_at"`
}

// drawdownStatusWire renders "current DD vs policy" without
// persisting anything; the admin uses this to decide whether to
// trigger a check.
type drawdownStatusWire struct {
	FundID         string             `json:"fund_id"`
	PeakNAV        float64            `json:"peak_nav"`
	CurrentNAV     float64            `json:"current_nav"`
	CurrentDDPct   float64            `json:"current_dd_pct"`
	HasPolicy      bool               `json:"has_policy"`
	Tiers          []drawdownTierWire `json:"tiers"`
	BreachedTier   int                `json:"breached_tier,omitempty"`
	BreachedAction string             `json:"breached_action,omitempty"`
	WouldEmit      *drawdownEventWire `json:"would_emit,omitempty"`
}

func projectTier(t drawdown.Tier) drawdownTierWire {
	return drawdownTierWire{
		Tier:          t.Tier,
		DDPct:         t.DDPct,
		Action:        string(t.Action),
		TrimRatio:     t.TrimRatio,
		CooldownHours: t.CooldownHours,
		AutoExecute:   t.AutoExecute,
		Note:          t.Note,
	}
}

func projectEvent(d drawdown.EventDetail) drawdownEventWire {
	out := drawdownEventWire{
		ID:              d.ID,
		FundID:          d.FundID,
		Tier:            d.Tier,
		CurrentDDPct:    d.CurrentDDPct,
		PeakNAV:         d.PeakNAV,
		CurrentNAV:      d.CurrentNAV,
		Action:          string(d.Action),
		TrimPlan:        d.TrimPlan,
		TradeIDs:        d.TradeIDs,
		Status:          string(d.Status),
		ReviewNote:      d.ReviewNote,
		ReviewedBy:      d.ReviewedBy,
		NavSnapshotID:   d.NavSnapshotID,
		DetectedAt:      d.DetectedAt.UTC().Format(time.RFC3339Nano),
		DetectorVersion: d.DetectorVersion,
		Metadata:        d.Metadata,
		CreatedAt:       d.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if d.ReviewedAt != nil {
		out.ReviewedAt = d.ReviewedAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}

// registerDrawdownAdminRoutes wires the admin endpoints. Called
// from registerAdminRoutes alongside the recon and surveillance
// route registrations.
func (h *adminHandler) registerDrawdownAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil || h.db == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/drawdown/funds/{fundId}/policy", h.handleGetDrawdownPolicy)
	mux.HandleFunc("PUT /api/admin/drawdown/funds/{fundId}/policy/tiers/{tier}", h.handleUpsertDrawdownTier)
	mux.HandleFunc("DELETE /api/admin/drawdown/funds/{fundId}/policy/tiers/{tier}", h.handleDeleteDrawdownTier)
	mux.HandleFunc("GET /api/admin/drawdown/funds/{fundId}/status", h.handleGetDrawdownStatus)
	mux.HandleFunc("POST /api/admin/drawdown/funds/{fundId}/check", h.handleTriggerDrawdownCheck)
	mux.HandleFunc("GET /api/admin/drawdown/events", h.handleListDrawdownEvents)
	mux.HandleFunc("GET /api/admin/drawdown/events/{id}", h.handleGetDrawdownEvent)
	mux.HandleFunc("POST /api/admin/drawdown/events/{id}/review", h.handleReviewDrawdownEvent)
}

// ----- policy: get -----

func (h *adminHandler) handleGetDrawdownPolicy(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_fund", "fund_id required"))
		return
	}
	repo := drawdown.NewRepo(h.db)
	policy, err := repo.GetPolicy(r.Context(), fundID)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := drawdownPolicyWire{FundID: policy.FundID, Tiers: make([]drawdownTierWire, 0, len(policy.Tiers))}
	for _, t := range policy.Tiers {
		out.Tiers = append(out.Tiers, projectTier(t))
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"policy": out})
}

// ----- policy: upsert tier -----

type upsertDrawdownTierRequest struct {
	DDPct         float64 `json:"dd_pct"`
	Action        string  `json:"action"`
	TrimRatio     float64 `json:"trim_ratio"`
	CooldownHours int     `json:"cooldown_hours"`
	AutoExecute   bool    `json:"auto_execute"`
	Note          string  `json:"note,omitempty"`
}

func (h *adminHandler) handleUpsertDrawdownTier(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	tierStr := strings.TrimSpace(r.PathValue("tier"))
	if fundID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_fund", "fund_id required"))
		return
	}
	tierNum, err := strconv.Atoi(tierStr)
	if err != nil || tierNum < 1 || tierNum > 5 {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_tier", "tier must be 1..5"))
		return
	}
	var req upsertDrawdownTierRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	repo := drawdown.NewRepo(h.db)
	t := drawdown.Tier{
		Tier:          tierNum,
		DDPct:         req.DDPct,
		Action:        drawdown.Action(strings.ToLower(strings.TrimSpace(req.Action))),
		TrimRatio:     req.TrimRatio,
		CooldownHours: req.CooldownHours,
		AutoExecute:   req.AutoExecute,
		Note:          req.Note,
	}
	if err := repo.UpsertTier(r.Context(), fundID, t); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("upsert_failed", err.Error()))
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "drawdown_policy.upsert",
			TargetType:  "drawdown_tier",
			TargetID:    fmt.Sprintf("%s:tier-%d", fundID, tierNum),
			After: map[string]any{
				"fund_id":        fundID,
				"tier":           tierNum,
				"dd_pct":         req.DDPct,
				"action":         req.Action,
				"trim_ratio":     req.TrimRatio,
				"cooldown_hours": req.CooldownHours,
				"auto_execute":   req.AutoExecute,
			},
		})
	}
	if h.metrics != nil {
		h.metrics.RecordDrawdownEvent("policy_upsert")
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"tier": projectTier(t)})
}

// ----- policy: delete tier -----

func (h *adminHandler) handleDeleteDrawdownTier(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	tierNum, err := strconv.Atoi(strings.TrimSpace(r.PathValue("tier")))
	if fundID == "" || err != nil || tierNum < 1 || tierNum > 5 {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_tier", "tier must be 1..5"))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	repo := drawdown.NewRepo(h.db)
	if err := repo.DeleteTier(r.Context(), fundID, tierNum); err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "drawdown_policy.delete",
			TargetType:  "drawdown_tier",
			TargetID:    fmt.Sprintf("%s:tier-%d", fundID, tierNum),
			After: map[string]any{
				"fund_id": fundID, "tier": tierNum,
			},
		})
	}
	if h.metrics != nil {
		h.metrics.RecordDrawdownEvent("policy_delete")
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ----- status (preview) -----

func (h *adminHandler) handleGetDrawdownStatus(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_fund", "fund_id required"))
		return
	}
	repo := drawdown.NewRepo(h.db)
	policy, err := repo.GetPolicy(r.Context(), fundID)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	builder := newDrawdownSnapshotBuilder(h.db, repo)
	if builder == nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", "snapshot builder unavailable"))
		return
	}
	snap, err := builder.Build(r.Context(), fundID, time.Now().UTC())
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("snapshot_failed", err.Error()))
		return
	}
	out := drawdownStatusWire{
		FundID:       fundID,
		PeakNAV:      snap.PeakNAV,
		CurrentNAV:   snap.CurrentNAV,
		CurrentDDPct: drawdown.ComputeDD(snap.PeakNAV, snap.CurrentNAV),
		HasPolicy:    len(policy.Tiers) > 0,
		Tiers:        make([]drawdownTierWire, 0, len(policy.Tiers)),
	}
	for _, t := range policy.Tiers {
		out.Tiers = append(out.Tiers, projectTier(t))
	}
	if out.HasPolicy {
		engine := drawdown.NewEngine()
		ev, evalErr := engine.Evaluate(snap, policy)
		if evalErr == nil && ev != nil {
			out.BreachedTier = ev.Tier
			out.BreachedAction = string(ev.Action)
			preview := projectEvent(drawdown.EventDetail{
				FundID:          ev.FundID,
				Tier:            ev.Tier,
				CurrentDDPct:    ev.CurrentDDPct,
				PeakNAV:         ev.PeakNAV,
				CurrentNAV:      ev.CurrentNAV,
				Action:          ev.Action,
				TrimPlan:        ev.TrimPlan,
				Status:          drawdown.StatusProposed,
				NavSnapshotID:   ev.NavSnapshotID,
				DetectedAt:      ev.DetectedAt,
				DetectorVersion: ev.DetectorVersion,
				Metadata:        ev.Metadata,
			})
			out.WouldEmit = &preview
		}
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"status": out})
}

// ----- on-demand check -----

func (h *adminHandler) handleTriggerDrawdownCheck(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_fund", "fund_id required"))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	repo := drawdown.NewRepo(h.db)
	policy, err := repo.GetPolicy(r.Context(), fundID)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if len(policy.Tiers) == 0 {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("no_policy", "fund has no drawdown tiers configured"))
		return
	}
	builder := newDrawdownSnapshotBuilder(h.db, repo)
	if builder == nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", "snapshot builder unavailable"))
		return
	}
	snap, err := builder.Build(r.Context(), fundID, time.Now().UTC())
	if err != nil {
		if h.metrics != nil {
			h.metrics.RecordDrawdownEvent("check_failed")
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("snapshot_failed", err.Error()))
		return
	}
	engine := drawdown.NewEngine()
	ev, err := engine.Evaluate(snap, policy)
	if err != nil {
		if h.metrics != nil {
			h.metrics.RecordDrawdownEvent("check_failed")
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("evaluate_failed", err.Error()))
		return
	}
	if h.metrics != nil {
		h.metrics.RecordDrawdownEvent("check_ok")
	}
	if ev == nil {
		writeOrderActionJSON(w, http.StatusOK, map[string]any{"breach": false})
		return
	}

	// Persist as proposed; auto_execute path is wired in the
	// scheduler — manual on-demand always lands as 'proposed'
	// so the operator confirms with eyes-on.
	id, err := repo.InsertEvent(r.Context(), *ev, drawdown.StatusProposed)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("insert_failed", err.Error()))
		return
	}
	if h.metrics != nil {
		h.metrics.RecordDrawdownEvent(fmt.Sprintf("breach_tier_%d", ev.Tier))
		h.metrics.RecordDrawdownEvent(fmt.Sprintf("action_%s", ev.Action))
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "drawdown_check.trigger",
			TargetType:  "drawdown_event",
			TargetID:    id,
			After: map[string]any{
				"fund_id":         fundID,
				"tier":            ev.Tier,
				"current_dd_pct":  ev.CurrentDDPct,
				"action":          string(ev.Action),
				"trim_plan_count": len(ev.TrimPlan),
			},
		})
	}
	persisted, _ := repo.GetEvent(r.Context(), id)
	if persisted == nil {
		writeOrderActionJSON(w, http.StatusOK, map[string]any{"breach": true, "event_id": id})
		return
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{
		"breach": true,
		"event":  projectEvent(*persisted),
	})
}

// ----- list events -----

func (h *adminHandler) handleListDrawdownEvents(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(strings.TrimSpace(q.Get("limit")))
	offset, _ := strconv.Atoi(strings.TrimSpace(q.Get("offset")))
	repo := drawdown.NewRepo(h.db)
	events, err := repo.ListEvents(r.Context(), drawdown.ListEventsParams{
		FundID: strings.TrimSpace(q.Get("fund_id")),
		Status: drawdown.Status(strings.TrimSpace(q.Get("status"))),
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]drawdownEventWire, 0, len(events))
	for _, ev := range events {
		out = append(out, projectEvent(ev))
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"events": out})
}

// ----- get event -----

func (h *adminHandler) handleGetDrawdownEvent(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_id", "id required"))
		return
	}
	repo := drawdown.NewRepo(h.db)
	d, err := repo.GetEvent(r.Context(), id)
	if err != nil {
		if errors.Is(err, drawdown.ErrEventNotFound) {
			writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_found", "event not found"))
			return
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"event": projectEvent(*d)})
}

// ----- review event -----

type reviewDrawdownEventRequest struct {
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

func (h *adminHandler) handleReviewDrawdownEvent(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_id", "id required"))
		return
	}
	var req reviewDrawdownEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	status := strings.TrimSpace(strings.ToLower(req.Status))
	switch drawdown.Status(status) {
	case drawdown.StatusProposed, drawdown.StatusApproved, drawdown.StatusDismissed, drawdown.StatusSuperseded:
	case drawdown.StatusExecuted:
		// `executed` should be set ONLY by the auto-execute path
		// which submits the orders and back-fills trade_ids. The
		// admin UI's "approve" action sets `approved` and the
		// execution worker promotes to `executed` when orders are
		// queued. Reject the operator setting executed by hand.
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_status",
			"cannot manually set 'executed' — let the execution worker promote"))
		return
	default:
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_status",
			"status must be one of proposed|approved|dismissed|superseded"))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	repo := drawdown.NewRepo(h.db)
	if err := repo.UpdateStatus(r.Context(), drawdown.UpdateStatusParams{
		ID:         id,
		NewStatus:  drawdown.Status(status),
		Note:       req.Note,
		ReviewedBy: userID,
	}); err != nil {
		if errors.Is(err, drawdown.ErrEventNotFound) {
			writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_found", "event not found"))
			return
		}
		if errors.Is(err, drawdown.ErrInvalidStatus) {
			writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_status", err.Error()))
			return
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	updated, err := repo.GetEvent(r.Context(), id)
	if err != nil {
		writeOrderActionJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "drawdown_event.review",
			TargetType:  "drawdown_event",
			TargetID:    id,
			After: map[string]any{
				"status":   status,
				"note":     req.Note,
				"event_id": id,
				"fund_id":  updated.FundID,
				"tier":     updated.Tier,
			},
		})
	}
	if h.metrics != nil {
		h.metrics.RecordDrawdownEvent("review_" + status)
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"event": projectEvent(*updated)})
}
