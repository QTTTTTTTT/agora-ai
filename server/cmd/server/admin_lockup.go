// admin_lockup.go — admin REST surface for the S6.3 IPO /
// private-placement / restricted-share lock-up store.
//
// Endpoints
//
//   GET    /api/admin/lockups                 list (?fund_id, ?instrument_key, ?status)
//   GET    /api/admin/lockups/{id}            one row
//   POST   /api/admin/lockups                 create
//   PATCH  /api/admin/lockups/{id}            edit qty / until / reason / note
//   DELETE /api/admin/lockups/{id}            hard delete (typo fix)
//   POST   /api/admin/lockups/{id}/release    early release with reason (audit-logged)
//
// Conventions match the other Sprint-6 sections: writes go
// through h.requireAdmin, audit-log the mutation, bump
// fundai_lockup_events_total{event="admin_*"}.

package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/lockup"
)

// lockupWire is the on-wire shape for one lock-up row. Pointer
// fields render as omitempty so the UI can tell "no early-
// release" apart from "released_reason set but empty".
type lockupWire struct {
	ID             string  `json:"id"`
	FundID         string  `json:"fund_id"`
	InstrumentKey  string  `json:"instrument_key"`
	Symbol         string  `json:"symbol"`
	LockedQty      float64 `json:"locked_qty"`
	LockedUntil    string  `json:"locked_until"`
	Reason         string  `json:"reason"`
	SourceLotID    string  `json:"source_lot_id,omitempty"`
	Note           string  `json:"note,omitempty"`
	ReleasedAt     string  `json:"released_at,omitempty"`
	ReleasedReason string  `json:"released_reason,omitempty"`
	ReleasedBy     string  `json:"released_by,omitempty"`
	CreatedBy      string  `json:"created_by,omitempty"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
	// Status is a derived field for UI filtering — saves the
	// frontend from re-implementing the active/expired/released
	// classification.
	Status string `json:"status"`
}

func projectLockup(r lockup.Record, asOf time.Time) lockupWire {
	w := lockupWire{
		ID:            r.ID,
		FundID:        r.FundID,
		InstrumentKey: r.InstrumentKey,
		Symbol:        r.Symbol,
		LockedQty:     r.LockedQty,
		LockedUntil:   r.LockedUntil.UTC().Format(time.RFC3339Nano),
		Reason:        string(r.Reason),
		Note:          r.Note,
		CreatedBy:     r.CreatedBy,
		CreatedAt:     r.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:     r.UpdatedAt.UTC().Format(time.RFC3339Nano),
		Status:        deriveLockupStatus(r, asOf),
	}
	if r.SourceLotID != nil {
		w.SourceLotID = *r.SourceLotID
	}
	if r.ReleasedAt != nil {
		w.ReleasedAt = r.ReleasedAt.UTC().Format(time.RFC3339Nano)
		w.ReleasedReason = r.ReleasedReason
		w.ReleasedBy = r.ReleasedBy
	}
	return w
}

func deriveLockupStatus(r lockup.Record, asOf time.Time) string {
	if r.ReleasedAt != nil {
		return "released"
	}
	if r.LockedUntil.After(asOf) {
		return "active"
	}
	return "expired"
}

// registerLockupAdminRoutes wires the routes. Called from
// registerAdminRoutes.
func (h *adminHandler) registerLockupAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/lockups", h.handleListLockups)
	mux.HandleFunc("GET /api/admin/lockups/{id}", h.handleGetLockup)
	mux.HandleFunc("POST /api/admin/lockups", h.handleCreateLockup)
	mux.HandleFunc("PATCH /api/admin/lockups/{id}", h.handleUpdateLockup)
	mux.HandleFunc("DELETE /api/admin/lockups/{id}", h.handleDeleteLockup)
	mux.HandleFunc("POST /api/admin/lockups/{id}/release", h.handleReleaseLockup)
}

// ----- list -----

func (h *adminHandler) handleListLockups(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.lockupRepo == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "lockup not wired"))
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	asOf := time.Now().UTC()
	rows, err := h.lockupRepo.List(r.Context(), lockup.ListFilter{
		FundID:        strings.TrimSpace(q.Get("fund_id")),
		InstrumentKey: strings.TrimSpace(q.Get("instrument_key")),
		Status:        strings.TrimSpace(q.Get("status")),
		AsOf:          asOf,
		Limit:         limit,
		Offset:        offset,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]lockupWire, 0, len(rows))
	for _, rec := range rows {
		out = append(out, projectLockup(rec, asOf))
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{
		"lockups": out,
		"total":   len(out),
	})
}

// ----- get -----

func (h *adminHandler) handleGetLockup(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.lockupRepo == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "lockup not wired"))
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_id", "id required"))
		return
	}
	rec, err := h.lockupRepo.GetByID(r.Context(), id)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if rec == nil {
		writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_found", "no lockup row"))
		return
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{
		"lockup": projectLockup(*rec, time.Now().UTC()),
	})
}

// ----- create -----

type createLockupRequest struct {
	FundID        string  `json:"fund_id"`
	InstrumentKey string  `json:"instrument_key"`
	Symbol        string  `json:"symbol"`
	LockedQty     float64 `json:"locked_qty"`
	LockedUntil   string  `json:"locked_until"`
	Reason        string  `json:"reason,omitempty"`
	SourceLotID   string  `json:"source_lot_id,omitempty"`
	Note          string  `json:"note,omitempty"`
}

func (h *adminHandler) handleCreateLockup(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.lockupRepo == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "lockup not wired"))
		return
	}
	var req createLockupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	until, err := time.Parse(time.RFC3339, strings.TrimSpace(req.LockedUntil))
	if err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_locked_until", "locked_until must be RFC3339"))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	rec, err := h.lockupRepo.Create(r.Context(), lockup.CreateParams{
		FundID:        req.FundID,
		InstrumentKey: req.InstrumentKey,
		Symbol:        req.Symbol,
		LockedQty:     req.LockedQty,
		LockedUntil:   until,
		Reason:        req.Reason,
		SourceLotID:   req.SourceLotID,
		Note:          req.Note,
		CreatedBy:     userID,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("create_failed", err.Error()))
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "lockup.create",
			TargetType:  "position_lockup",
			TargetID:    rec.ID,
			After: map[string]any{
				"fund_id":        rec.FundID,
				"instrument_key": rec.InstrumentKey,
				"locked_qty":     rec.LockedQty,
				"locked_until":   rec.LockedUntil,
				"reason":         rec.Reason,
			},
		})
	}
	if h.metrics != nil {
		h.metrics.RecordLockupEvent("admin_create")
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{
		"lockup": projectLockup(*rec, time.Now().UTC()),
	})
}

// ----- update -----

type updateLockupRequest struct {
	LockedQty   *float64 `json:"locked_qty,omitempty"`
	LockedUntil *string  `json:"locked_until,omitempty"`
	Reason      *string  `json:"reason,omitempty"`
	Note        *string  `json:"note,omitempty"`
}

func (h *adminHandler) handleUpdateLockup(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.lockupRepo == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "lockup not wired"))
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_id", "id required"))
		return
	}
	var req updateLockupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	params := lockup.UpdateParams{
		ID:        id,
		LockedQty: req.LockedQty,
		Reason:    req.Reason,
		Note:      req.Note,
	}
	if req.LockedUntil != nil {
		until, err := time.Parse(time.RFC3339, strings.TrimSpace(*req.LockedUntil))
		if err != nil {
			writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_locked_until", "locked_until must be RFC3339"))
			return
		}
		params.LockedUntil = &until
	}
	userID, _ := api.AuthenticatedUserID(r)
	params.UpdatedBy = userID
	rec, err := h.lockupRepo.Update(r.Context(), params)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_found", "lockup not found or already released"))
			return
		}
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("update_failed", err.Error()))
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "lockup.update",
			TargetType:  "position_lockup",
			TargetID:    id,
			After: map[string]any{
				"locked_qty":   params.LockedQty,
				"locked_until": params.LockedUntil,
				"reason":       params.Reason,
			},
		})
	}
	if h.metrics != nil {
		h.metrics.RecordLockupEvent("admin_update")
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{
		"lockup": projectLockup(*rec, time.Now().UTC()),
	})
}

// ----- delete -----

func (h *adminHandler) handleDeleteLockup(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.lockupRepo == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "lockup not wired"))
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_id", "id required"))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	if err := h.lockupRepo.Delete(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_found", "no lockup row"))
			return
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "lockup.delete",
			TargetType:  "position_lockup",
			TargetID:    id,
		})
	}
	if h.metrics != nil {
		h.metrics.RecordLockupEvent("admin_delete")
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ----- release (early) -----

type releaseLockupRequest struct {
	Reason string `json:"reason"`
}

func (h *adminHandler) handleReleaseLockup(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.lockupRepo == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "lockup not wired"))
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_id", "id required"))
		return
	}
	var req releaseLockupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", "reason required"))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	rec, err := h.lockupRepo.Release(r.Context(), id, req.Reason, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_found", "lockup not found or already released"))
			return
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "lockup.release",
			TargetType:  "position_lockup",
			TargetID:    id,
			After: map[string]any{
				"reason": req.Reason,
			},
		})
	}
	if h.metrics != nil {
		h.metrics.RecordLockupEvent("admin_release")
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{
		"lockup": projectLockup(*rec, time.Now().UTC()),
	})
}
