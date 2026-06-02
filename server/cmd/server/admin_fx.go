// admin_fx.go — admin REST surface for FX rates (P1-4).
//
// Endpoints
//
//   GET  /api/admin/fx-rates                    list latest N rows, optional ?pair=USD/CNY
//   POST /api/admin/fx-rates                    manual upsert (operator override)
//
// AuthZ: same admin gate the rest of /api/admin/* uses.
// Audit:  every manual upsert lands in admin_change_log so the
//         hash chain captures who corrected which rate when.
//
// We deliberately do NOT expose a delete endpoint — historical
// rates are part of the audit story for past NAVs and should
// never be silently removed. If a row is wrong, the operator
// inserts a new "manual" row at the same rate_at; the read
// path prefers manual > yahoo automatically.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/fx"
)

// fxAdminWire is the on-wire shape for fx_rates rows.
type fxAdminWire struct {
	Base   string  `json:"base"`
	Quote  string  `json:"quote"`
	Rate   float64 `json:"rate"`
	RateAt string  `json:"rate_at"`
	Source string  `json:"source"`
}

func projectFXRate(r fx.Rate) fxAdminWire {
	return fxAdminWire{
		Base:   r.Base,
		Quote:  r.Quote,
		Rate:   r.Rate,
		RateAt: r.RateAt.UTC().Format(time.RFC3339Nano),
		Source: r.Source,
	}
}

// registerFXAdminRoutes wires the admin FX endpoints onto the
// shared *http.ServeMux. Called from registerAdminRoutes so the
// ordering w.r.t. funding/KYC/etc. stays deterministic.
//
// We register method-prefixed routes (Go 1.22+) so the
// scripts/validate-api-contract.mjs validator can see the
// (method, path) pair without parsing handler bodies. The
// switch-style `handleFXRates` entry point is retained because
// admin_fx_test.go calls it directly to exercise the
// MethodNotAllowed branch on DELETE.
func (h *adminHandler) registerFXAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil || h.db == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/fx-rates", h.handleListFXRates)
	mux.HandleFunc("POST /api/admin/fx-rates", h.handleUpsertFXRate)
}

func (h *adminHandler) handleFXRates(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.handleListFXRates(w, r)
	case http.MethodPost:
		h.handleUpsertFXRate(w, r)
	default:
		writeOrderActionJSON(w, http.StatusMethodNotAllowed, errorPayload("method_not_allowed", r.Method))
	}
}

func (h *adminHandler) handleListFXRates(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(strings.TrimSpace(q.Get("limit")))
	if limit <= 0 {
		limit = 100
	}
	repo := fx.NewRepo(h.db)
	rows, err := repo.ListRecent(r.Context(), fx.ListRecentParams{
		Limit: limit,
		Pair:  strings.TrimSpace(q.Get("pair")),
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]fxAdminWire, 0, len(rows))
	for _, r := range rows {
		out = append(out, projectFXRate(r))
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{
		"rates":      out,
		"currencies": fx.SupportedCurrencies,
	})
}

type fxUpsertRequest struct {
	Base   string  `json:"base"`
	Quote  string  `json:"quote"`
	Rate   float64 `json:"rate"`
	RateAt string  `json:"rate_at,omitempty"`
	Source string  `json:"source,omitempty"`
	Note   string  `json:"note,omitempty"`
}

func (h *adminHandler) handleUpsertFXRate(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	var req fxUpsertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	if !fx.IsSupported(req.Base) || !fx.IsSupported(req.Quote) {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_currency",
			fmt.Sprintf("currencies must be one of %v", fx.SupportedCurrencies)))
		return
	}
	if req.Rate <= 0 {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_rate", "rate must be > 0"))
		return
	}
	source := strings.TrimSpace(strings.ToLower(req.Source))
	if source == "" {
		source = "manual"
	}
	if source != "manual" && source != "override" {
		writeOrderActionJSON(w, http.StatusBadRequest,
			errorPayload("invalid_source", "manual upsert source must be 'manual' or 'override'"))
		return
	}
	rateAt := time.Now().UTC()
	if s := strings.TrimSpace(req.RateAt); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeOrderActionJSON(w, http.StatusBadRequest,
				errorPayload("invalid_rate_at", "must be RFC3339"))
			return
		}
		rateAt = t.UTC()
	}
	metadata := map[string]any{}
	if note := strings.TrimSpace(req.Note); note != "" {
		metadata["note"] = note
	}
	if userID != "" {
		metadata["uploaded_by"] = userID
	}

	repo := fx.NewRepo(h.db)
	id, err := repo.Upsert(r.Context(), fx.UpsertParams{
		Base:      req.Base,
		Quote:     req.Quote,
		Rate:      req.Rate,
		RateAt:    rateAt,
		Source:    source,
		CreatedBy: userID,
		Metadata:  metadata,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("upsert_failed", err.Error()))
		return
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "fx_rate.upsert",
			TargetType:  "fx_rate",
			TargetID:    id,
			After: map[string]any{
				"base":    strings.ToUpper(req.Base),
				"quote":   strings.ToUpper(req.Quote),
				"rate":    req.Rate,
				"source":  source,
				"rate_at": rateAt.Format(time.RFC3339Nano),
				"note":    req.Note,
			},
		})
	}
	if h.metrics != nil {
		h.metrics.RecordFXEvent("upsert_" + source)
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"id": id})
}

// requireAdmin proxies through h's existing admin gate. We
// keep it private to this file so the FX handler doesn't reach
// across into other admin modules' internals.
func (h *adminHandler) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing token"))
		return false
	}
	allowed, err := h.userIsAdmin(r.Context(), userID)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return false
	}
	if !allowed {
		writeOrderActionJSON(w, http.StatusForbidden, errorPayload("forbidden", "admin role required"))
		return false
	}
	return true
}

// userIsAdmin checks the users.role column. Mirrors the gate the
// other admin handlers use; kept as a private helper here so the
// FX route stays self-contained.
func (h *adminHandler) userIsAdmin(ctx context.Context, userID string) (bool, error) {
	if h == nil || h.db == nil {
		return false, errors.New("admin: nil services")
	}
	var role string
	err := h.db.QueryRowContext(ctx, `SELECT role FROM users WHERE id = $1`, userID).Scan(&role)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(role, "admin") || strings.EqualFold(role, adminRoleSuperAdmin), nil
}
