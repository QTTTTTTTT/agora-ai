// agent_reputation_handler.go — S8.4 read API for the per-agent
// reputation ledger, scoped to one fund.
//
// Routes
//
//   GET /api/funds/{fundId}/agent-reputation/stats[?kind=analyst|advocate|pm|researcher&limit=N]
//   GET /api/funds/{fundId}/agent-reputation/outcomes[?agent_id=X&symbol=Y&limit=N]
//
// Admin-scoped routes (cross-fund view + rebuild) sit on
// *adminHandler in admin_agent_reputation.go.

package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/agentreputation"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/repository"
)

type agentReputationHandler struct {
	repo        *agentreputation.Repo
	fundRepo    *repository.FundRepo
	companyRepo *repository.FundCompanyRepo
}

// newAgentReputationHandler builds the handler. Returns nil when
// the repo is absent so the wiring layer can no-op without
// crashing on startup.
func newAgentReputationHandler(svc *Services) *agentReputationHandler {
	if svc == nil || svc.DB == nil || svc.AgentReputationRepo == nil {
		return nil
	}
	return &agentReputationHandler{
		repo:        svc.AgentReputationRepo,
		fundRepo:    repository.NewFundRepo(svc.DB),
		companyRepo: repository.NewFundCompanyRepo(svc.DB),
	}
}

func (h *agentReputationHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/funds/{fundId}/agent-reputation/stats",
		h.handleListFundStats)
	mux.HandleFunc("GET /api/funds/{fundId}/agent-reputation/outcomes",
		h.handleListFundOutcomes)
}

// --- Wire shapes (mirrored in shared/api-client) ---------------------------

type agentReputationStatsWire struct {
	FundID         string  `json:"fund_id"`
	AgentID        string  `json:"agent_id"`
	AgentName      string  `json:"agent_name"`
	AgentKind      string  `json:"agent_kind"`
	Category       string  `json:"category"`
	DecisionsCount int64   `json:"decisions_count"`
	HitsCount      int64   `json:"hits_count"`
	MissesCount    int64   `json:"misses_count"`
	HitRate        float64 `json:"hit_rate"`
	AvgAlpha       float64 `json:"avg_alpha"`
	SumAlpha       float64 `json:"sum_alpha"`
	AvgConfidence  float64 `json:"avg_confidence"`
	LastDecisionAt string  `json:"last_decision_at,omitempty"`
	UpdatedAt      string  `json:"updated_at"`
}

type agentReputationOutcomeWire struct {
	ID              string  `json:"id"`
	FundID          string  `json:"fund_id"`
	AgentID         string  `json:"agent_id"`
	AgentName       string  `json:"agent_name"`
	AgentKind       string  `json:"agent_kind"`
	Category        string  `json:"category"`
	Symbol          string  `json:"symbol"`
	AsOf            string  `json:"asof"`
	Direction       string  `json:"direction"`
	Confidence      int     `json:"confidence"`
	RealisedReturn  float64 `json:"realised_return"`
	BenchmarkReturn float64 `json:"benchmark_return"`
	Alpha           float64 `json:"alpha"`
	HorizonDays     int     `json:"horizon_days"`
	SourcePanelID   string  `json:"source_panel_id,omitempty"`
	SourceDebateID  string  `json:"source_debate_id,omitempty"`
	Note            string  `json:"note,omitempty"`
	CreatedAt       string  `json:"created_at"`
}

// --- Handlers --------------------------------------------------------------

func (h *agentReputationHandler) handleListFundStats(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fundId required"))
		return
	}
	if _, err := authorizeFundAccess(r.Context(), h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}
	params := buildStatsParams(r, fundID)
	rows, err := h.repo.ListStats(r.Context(), params)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stats": projectAgentReputationStats(rows)})
}

func (h *agentReputationHandler) handleListFundOutcomes(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fundId required"))
		return
	}
	if _, err := authorizeFundAccess(r.Context(), h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}
	q := r.URL.Query()
	params := agentreputation.ListOutcomesParams{
		FundID:  fundID,
		AgentID: strings.TrimSpace(q.Get("agent_id")),
		Symbol:  strings.TrimSpace(q.Get("symbol")),
	}
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			params.Limit = n
		}
	}
	rows, err := h.repo.ListOutcomes(r.Context(), params)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"outcomes": projectAgentReputationOutcomes(rows)})
}

// --- Shared helpers (used by admin_agent_reputation.go too) ----------------

func buildStatsParams(r *http.Request, fundID string) agentreputation.ListStatsParams {
	q := r.URL.Query()
	p := agentreputation.ListStatsParams{FundID: fundID}
	if v := strings.TrimSpace(q.Get("kind")); v != "" {
		p.AgentKind = agentreputation.AgentKind(v)
	}
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.Limit = n
		}
	}
	return p
}

func projectAgentReputationStats(rows []agentreputation.Stats) []agentReputationStatsWire {
	out := make([]agentReputationStatsWire, 0, len(rows))
	for _, s := range rows {
		w := agentReputationStatsWire{
			FundID:         s.FundID,
			AgentID:        s.AgentID,
			AgentName:      s.AgentName,
			AgentKind:      string(s.AgentKind),
			Category:       s.Category,
			DecisionsCount: s.DecisionsCount,
			HitsCount:      s.HitsCount,
			MissesCount:    s.MissesCount,
			HitRate:        s.HitRate(),
			AvgAlpha:       s.AvgAlpha,
			SumAlpha:       s.SumAlpha,
			AvgConfidence:  s.AvgConfidence,
			UpdatedAt:      s.UpdatedAt.UTC().Format(time.RFC3339Nano),
		}
		if s.LastDecisionAt.Valid {
			w.LastDecisionAt = s.LastDecisionAt.Time.UTC().Format(time.RFC3339Nano)
		}
		out = append(out, w)
	}
	return out
}

func projectAgentReputationOutcomes(rows []agentreputation.Outcome) []agentReputationOutcomeWire {
	out := make([]agentReputationOutcomeWire, 0, len(rows))
	for _, o := range rows {
		w := agentReputationOutcomeWire{
			ID:              o.ID,
			FundID:          o.FundID,
			AgentID:         o.AgentID,
			AgentName:       o.AgentName,
			AgentKind:       string(o.AgentKind),
			Category:        o.Category,
			Symbol:          o.Symbol,
			AsOf:            o.AsOf.UTC().Format(time.RFC3339Nano),
			Direction:       string(o.Direction),
			Confidence:      o.Confidence,
			RealisedReturn:  o.RealisedReturn,
			BenchmarkReturn: o.BenchmarkReturn,
			Alpha:           o.Alpha,
			HorizonDays:     o.HorizonDays,
			Note:            o.Note,
			CreatedAt:       o.CreatedAt.UTC().Format(time.RFC3339Nano),
		}
		if o.SourcePanelID.Valid {
			w.SourcePanelID = o.SourcePanelID.String
		}
		if o.SourceDebateID.Valid {
			w.SourceDebateID = o.SourceDebateID.String
		}
		out = append(out, w)
	}
	return out
}
