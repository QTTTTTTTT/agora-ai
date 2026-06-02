// admin_stress.go — admin REST surface for S7 / P3-3 stress
// scenarios.
//
// Endpoints
//
//   GET    /api/admin/stress-scenarios          list (?category)
//   POST   /api/admin/stress-scenarios          upsert a scenario
//   DELETE /api/admin/stress-scenarios/{id}     remove a scenario
//
// Writes go through h.requireAdmin and audit-log the mutation.
// Stress scenarios drive an admin-curated library — there are
// dozens of named scenarios across history / hypothetical /
// regulatory categories and they get reused by every fund.

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/stress"
)

// scenarioWire is the on-wire projection of a Scenario.
type stressScenarioWire struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Category    string             `json:"category"`
	Description string             `json:"description"`
	Shocks      []stressShockWire  `json:"shocks"`
	CreatedBy   string             `json:"created_by,omitempty"`
	CreatedAt   string             `json:"created_at"`
	UpdatedAt   string             `json:"updated_at"`
}

type stressShockWire struct {
	TargetType string  `json:"target_type"`
	TargetKey  string  `json:"target_key"`
	Value      float64 `json:"value"`
}

func projectStressScenario(s stress.Scenario) stressScenarioWire {
	out := stressScenarioWire{
		ID:          s.ID,
		Name:        s.Name,
		Category:    string(s.Category),
		Description: s.Description,
		CreatedBy:   s.CreatedBy,
		CreatedAt:   s.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:   s.UpdatedAt.UTC().Format(time.RFC3339Nano),
		Shocks:      make([]stressShockWire, 0, len(s.Shocks)),
	}
	for _, sh := range s.Shocks {
		out.Shocks = append(out.Shocks, stressShockWire{
			TargetType: string(sh.TargetType),
			TargetKey:  sh.TargetKey,
			Value:      sh.Value,
		})
	}
	return out
}

func (h *adminHandler) registerStressAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil || h.stressRepo == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/stress-scenarios", h.handleListStressScenarios)
	mux.HandleFunc("POST /api/admin/stress-scenarios", h.handleUpsertStressScenario)
	mux.HandleFunc("DELETE /api/admin/stress-scenarios/{id}", h.handleDeleteStressScenario)
}

func (h *adminHandler) handleListStressScenarios(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var category stress.Category
	if c := strings.TrimSpace(r.URL.Query().Get("category")); c != "" {
		cat := stress.Category(c)
		if !cat.IsValid() {
			writeJSON(w, http.StatusBadRequest, errorPayload("invalid_category", c))
			return
		}
		category = cat
	}
	rows, err := h.stressRepo.ListScenarios(r.Context(), category)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]stressScenarioWire, 0, len(rows))
	for _, row := range rows {
		out = append(out, projectStressScenario(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scenarios":  out,
		"categories": stress.AllCategories,
	})
}

type stressUpsertRequest struct {
	Name        string            `json:"name"`
	Category    string            `json:"category"`
	Description string            `json:"description"`
	Shocks      []stressShockWire `json:"shocks"`
}

func (h *adminHandler) handleUpsertStressScenario(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	var req stressUpsertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	scen := stress.Scenario{
		Name:        strings.TrimSpace(req.Name),
		Category:    stress.Category(strings.TrimSpace(req.Category)),
		Description: req.Description,
		Shocks:      make([]stress.Shock, 0, len(req.Shocks)),
	}
	for _, sh := range req.Shocks {
		scen.Shocks = append(scen.Shocks, stress.Shock{
			TargetType: stress.TargetType(sh.TargetType),
			TargetKey:  sh.TargetKey,
			Value:      sh.Value,
		})
	}
	if err := scen.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_scenario", err.Error()))
		return
	}
	saved, err := h.stressRepo.UpsertScenario(r.Context(), scen, userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if h.auditLogger != nil && userID != "" {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "stress_scenario.upsert",
			TargetType:  "stress_scenario",
			TargetID:    saved.ID,
			After: map[string]any{
				"name":        saved.Name,
				"category":    string(saved.Category),
				"shock_count": len(saved.Shocks),
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"scenario": projectStressScenario(saved)})
}

func (h *adminHandler) handleDeleteStressScenario(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "id required"))
		return
	}
	if err := h.stressRepo.DeleteScenario(r.Context(), id); err != nil {
		if err == stress.ErrNotFound {
			writeJSON(w, http.StatusNotFound, errorPayload("not_found", id))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if h.auditLogger != nil && userID != "" {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "stress_scenario.delete",
			TargetType:  "stress_scenario",
			TargetID:    id,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}
