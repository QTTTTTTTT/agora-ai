// admin_brinson.go — admin CRUD for the benchmark side of
// Brinson attribution (S7 / P3-4).
//
// Endpoints
//
//   GET    /api/admin/brinson-compositions[?benchmarkId=X&dimension=Y]
//   POST   /api/admin/brinson-compositions
//   DELETE /api/admin/brinson-compositions/{id}
//
// Writes go through h.requireAdmin and audit-log.

package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/brinson"
)

// brinsonBucketWire mirrors brinson.Bucket on the wire.
type brinsonBucketWire struct {
	Key       string  `json:"key"`
	Weight    float64 `json:"weight"`
	ReturnPct float64 `json:"return_pct"`
}

// brinsonCompositionWire is the projection of a CompositionRow.
type brinsonCompositionWire struct {
	ID          string              `json:"id"`
	BenchmarkID string              `json:"benchmark_id"`
	Dimension   string              `json:"dimension"`
	AsOf        string              `json:"asof"`
	Buckets     []brinsonBucketWire `json:"buckets"`
	Note        string              `json:"note,omitempty"`
	CreatedBy   string              `json:"created_by,omitempty"`
	CreatedAt   string              `json:"created_at"`
	UpdatedAt   string              `json:"updated_at"`
}

func projectBrinsonComposition(row brinson.CompositionRow) brinsonCompositionWire {
	buckets := make([]brinsonBucketWire, 0, len(row.Buckets))
	for _, b := range row.Buckets {
		buckets = append(buckets, brinsonBucketWire{Key: b.Key, Weight: b.Weight, ReturnPct: b.ReturnPct})
	}
	return brinsonCompositionWire{
		ID:          row.ID,
		BenchmarkID: row.BenchmarkID,
		Dimension:   string(row.Dimension),
		AsOf:        row.AsOf.UTC().Format("2006-01-02"),
		Buckets:     buckets,
		Note:        row.Note,
		CreatedBy:   row.CreatedBy,
		CreatedAt:   row.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:   row.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func (h *adminHandler) registerBrinsonAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil || h.brinsonRepo == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/brinson-compositions", h.handleListBrinsonCompositions)
	mux.HandleFunc("POST /api/admin/brinson-compositions", h.handleUpsertBrinsonComposition)
	mux.HandleFunc("DELETE /api/admin/brinson-compositions/{id}", h.handleDeleteBrinsonComposition)
}

func (h *adminHandler) handleListBrinsonCompositions(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	q := r.URL.Query()
	params := brinson.ListCompositionsParams{
		BenchmarkID: strings.TrimSpace(q.Get("benchmarkId")),
	}
	if d := strings.TrimSpace(q.Get("dimension")); d != "" {
		dim, ok := brinson.ParseBucketDimension(d)
		if !ok {
			writeJSON(w, http.StatusBadRequest, errorPayload("invalid_dimension", d))
			return
		}
		params.Dimension = dim
	}
	if limit, err := strconv.Atoi(strings.TrimSpace(q.Get("limit"))); err == nil && limit > 0 {
		params.Limit = limit
	}
	rows, err := h.brinsonRepo.ListCompositions(r.Context(), params)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]brinsonCompositionWire, 0, len(rows))
	for _, row := range rows {
		out = append(out, projectBrinsonComposition(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"compositions": out,
		"dimensions":   brinson.AllDimensions,
	})
}

type brinsonUpsertRequest struct {
	BenchmarkID string              `json:"benchmark_id"`
	Dimension   string              `json:"dimension"`
	AsOf        string              `json:"asof"` // YYYY-MM-DD
	Buckets     []brinsonBucketWire `json:"buckets"`
	Note        string              `json:"note,omitempty"`
}

func (h *adminHandler) handleUpsertBrinsonComposition(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	var req brinsonUpsertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	dim, ok := brinson.ParseBucketDimension(req.Dimension)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_dimension", req.Dimension))
		return
	}
	asof, err := time.Parse("2006-01-02", strings.TrimSpace(req.AsOf))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_asof", "expected YYYY-MM-DD"))
		return
	}
	if strings.TrimSpace(req.BenchmarkID) == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("benchmark_id_required", ""))
		return
	}
	buckets := make([]brinson.Bucket, 0, len(req.Buckets))
	for _, b := range req.Buckets {
		buckets = append(buckets, brinson.Bucket{Key: b.Key, Weight: b.Weight, ReturnPct: b.ReturnPct})
	}
	row := brinson.CompositionRow{
		BenchmarkID: req.BenchmarkID,
		Dimension:   dim,
		AsOf:        asof,
		Buckets:     buckets,
		Note:        req.Note,
	}
	saved, err := h.brinsonRepo.UpsertComposition(r.Context(), row, userID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_composition", err.Error()))
		return
	}
	if h.auditLogger != nil && userID != "" {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "brinson_composition.upsert",
			TargetType:  "brinson_composition",
			TargetID:    saved.ID,
			After: map[string]any{
				"benchmark_id":  saved.BenchmarkID,
				"dimension":     string(saved.Dimension),
				"asof":          saved.AsOf.Format("2006-01-02"),
				"bucket_count":  len(saved.Buckets),
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"composition": projectBrinsonComposition(saved),
	})
}

func (h *adminHandler) handleDeleteBrinsonComposition(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "id required"))
		return
	}
	if err := h.brinsonRepo.DeleteComposition(r.Context(), id); err != nil {
		if err == brinson.ErrNotFound {
			writeJSON(w, http.StatusNotFound, errorPayload("not_found", id))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if h.auditLogger != nil && userID != "" {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "brinson_composition.delete",
			TargetType:  "brinson_composition",
			TargetID:    id,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}
