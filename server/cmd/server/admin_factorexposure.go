// admin_factorexposure.go — admin REST surface for S7 / P3-1
// instrument factor-loading calibration + portfolio snapshot
// archive.
//
// Endpoints
//
//   GET    /api/admin/factor-loadings           list (?factor, ?instrument_key, ?limit, ?offset)
//   POST   /api/admin/factor-loadings           upsert one (instrument, factor, asof) row
//   DELETE /api/admin/factor-loadings           delete one (instrument, factor, asof) row
//
// Conventions match the other Sprint-6+7 sections: writes go
// through h.requireAdmin and audit-log the mutation.

package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/factorexposure"
)

// factorLoadingWire is the on-wire shape for one row of the
// instrument_factor_loadings table. asof is the calibration
// vintage (date), updated_at the write timestamp.
type factorLoadingWire struct {
	InstrumentKey string  `json:"instrument_key"`
	Factor        string  `json:"factor"`
	AsOf          string  `json:"asof"`
	Loading       float64 `json:"loading"`
	Source        string  `json:"source"`
	Note          string  `json:"note,omitempty"`
	UpdatedAt     string  `json:"updated_at"`
}

func projectFactorLoading(r factorexposure.InstrumentLoading) factorLoadingWire {
	return factorLoadingWire{
		InstrumentKey: r.InstrumentKey,
		Factor:        string(r.Factor),
		AsOf:          r.AsOf.UTC().Format("2006-01-02"),
		Loading:       r.Loading,
		Source:        string(r.Source),
		Note:          r.Note,
		UpdatedAt:     r.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// registerFactorExposureAdminRoutes wires the admin endpoints.
// Called from RegisterRoutes so the ordering w.r.t. drawdown /
// lockup stays deterministic.
func (h *adminHandler) registerFactorExposureAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil || h.factorExposureRepo == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/factor-loadings", h.handleListFactorLoadings)
	mux.HandleFunc("POST /api/admin/factor-loadings", h.handleUpsertFactorLoading)
	mux.HandleFunc("DELETE /api/admin/factor-loadings", h.handleDeleteFactorLoading)
}

func (h *adminHandler) handleListFactorLoadings(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	q := r.URL.Query()
	filter := factorexposure.ListLoadingsFilter{
		InstrumentKey: strings.TrimSpace(q.Get("instrument_key")),
	}
	if f := strings.TrimSpace(q.Get("factor")); f != "" {
		parsed, ok := factorexposure.ParseFactor(f)
		if !ok {
			writeJSON(w, http.StatusBadRequest, errorPayload("invalid_factor", f))
			return
		}
		filter.Factor = parsed
	}
	if limit, err := strconv.Atoi(strings.TrimSpace(q.Get("limit"))); err == nil && limit > 0 {
		filter.Limit = limit
	}
	if offset, err := strconv.Atoi(strings.TrimSpace(q.Get("offset"))); err == nil && offset > 0 {
		filter.Offset = offset
	}
	rows, err := h.factorExposureRepo.ListLoadings(r.Context(), filter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]factorLoadingWire, 0, len(rows))
	for _, row := range rows {
		out = append(out, projectFactorLoading(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"loadings":  out,
		"factors":   factorexposure.AllFactors,
		"row_count": len(out),
	})
}

type factorLoadingUpsertRequest struct {
	InstrumentKey string  `json:"instrument_key"`
	Factor        string  `json:"factor"`
	AsOf          string  `json:"asof"` // YYYY-MM-DD
	Loading       float64 `json:"loading"`
	Source        string  `json:"source,omitempty"`
	Note          string  `json:"note,omitempty"`
}

func (h *adminHandler) handleUpsertFactorLoading(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	var req factorLoadingUpsertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	factor, ok := factorexposure.ParseFactor(req.Factor)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_factor", req.Factor))
		return
	}
	asof, err := time.Parse("2006-01-02", strings.TrimSpace(req.AsOf))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_asof", "expected YYYY-MM-DD"))
		return
	}
	source := factorexposure.LoadingSource(strings.TrimSpace(req.Source))
	if source == "" {
		source = factorexposure.LoadingSourceManual
	}
	if !source.IsValid() {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_source", string(source)))
		return
	}
	if strings.TrimSpace(req.InstrumentKey) == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("instrument_key_required", ""))
		return
	}
	if req.Loading < -10 || req.Loading > 10 {
		writeJSON(w, http.StatusBadRequest, errorPayload("loading_out_of_range", "expected -10..10"))
		return
	}
	rec := factorexposure.InstrumentLoading{
		InstrumentKey: strings.TrimSpace(req.InstrumentKey),
		Factor:        factor,
		AsOf:          asof,
		Loading:       req.Loading,
		Source:        source,
		Note:          strings.TrimSpace(req.Note),
	}
	if err := h.factorExposureRepo.UpsertLoading(r.Context(), rec); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	// Audit chain so operators can trace "who pushed what loading
	// when" — useful when a quant lab batch and a manual override
	// disagree.
	if h.auditLogger != nil && userID != "" {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "factor_loading.upsert",
			TargetType:  "factor_loading",
			TargetID:    rec.InstrumentKey + ":" + string(rec.Factor) + ":" + rec.AsOf.Format("2006-01-02"),
			After: map[string]any{
				"instrument_key": rec.InstrumentKey,
				"factor":         string(rec.Factor),
				"asof":           rec.AsOf.Format("2006-01-02"),
				"loading":        rec.Loading,
				"source":         string(rec.Source),
				"note":           rec.Note,
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"loading": projectFactorLoading(rec),
	})
}

func (h *adminHandler) handleDeleteFactorLoading(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	q := r.URL.Query()
	instrument := strings.TrimSpace(q.Get("instrument_key"))
	if instrument == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("instrument_key_required", ""))
		return
	}
	factor, ok := factorexposure.ParseFactor(q.Get("factor"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_factor", q.Get("factor")))
		return
	}
	asof, err := time.Parse("2006-01-02", strings.TrimSpace(q.Get("asof")))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_asof", "expected YYYY-MM-DD"))
		return
	}
	if err := h.factorExposureRepo.DeleteLoading(r.Context(), instrument, factor, asof); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if h.auditLogger != nil && userID != "" {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "factor_loading.delete",
			TargetType:  "factor_loading",
			TargetID:    instrument + ":" + string(factor) + ":" + asof.Format("2006-01-02"),
			Before: map[string]any{
				"instrument_key": instrument,
				"factor":         string(factor),
				"asof":           asof.Format("2006-01-02"),
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// jsonNewDecoderRef pins the json import so future edits that
// shrink the file don't break the toolchain on goimports cleanup.
var _ = json.NewDecoder
