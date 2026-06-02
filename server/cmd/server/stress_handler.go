// stress_handler.go — per-fund stress-test runner (S7 / P3-3).
//
// Routes
//
//   POST /api/funds/{fundId}/risk/stress
//        Run one scenario against the fund's current holdings.
//        Body: {"scenario_id": "<uuid>", "persist": false}
//        Response: the full Result with per-holding impacts.
//
//   GET  /api/funds/{fundId}/risk/stress/history[?scenarioId=X&limit=N]
//        Returns the last N stress runs (or only those of one
//        scenario) for the dashboard timeline.
//
// Authorisation reuses the shared authorizeFundAccess gate; the
// engine + repo are nil-safe so the handler short-circuits to
// 503 when StressRepo isn't wired (test environments).
//
// Why POST for the run path — every run touches an external
// scenario id, optionally writes an archive row, and we want it
// to be non-idempotent (clicking "run again" produces a fresh
// snapshot). GETs would also be confusable with HTTP cache
// proxies and we don't want a cached stale stress result.

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/factorexposure"
	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/stress"
)

type stressHandler struct {
	scenarios       *stress.Repo
	loadingsRepo    *factorexposure.Repo
	positions       *repository.PositionRepo
	fundRepo        *repository.FundRepo
	companyRepo     *repository.FundCompanyRepo
	engine          *stress.Engine
	metrics         *serverMetrics
}

func newStressHandler(svc *Services) *stressHandler {
	if svc == nil || svc.DB == nil || svc.StressRepo == nil {
		return nil
	}
	return &stressHandler{
		scenarios:    svc.StressRepo,
		loadingsRepo: factorexposure.NewRepo(svc.DB),
		positions:    repository.NewPositionRepo(svc.DB),
		fundRepo:     repository.NewFundRepo(svc.DB),
		companyRepo:  repository.NewFundCompanyRepo(svc.DB),
		engine:       stress.NewEngine(),
		metrics:      svc.Metrics,
	}
}

func (h *stressHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("POST /api/funds/{fundId}/risk/stress", h.handleRun)
	mux.HandleFunc("GET /api/funds/{fundId}/risk/stress/history", h.handleHistory)
	// Public-but-authed scenario library. Any authenticated user
	// can browse the catalog; mutations live behind the admin
	// gate in admin_stress.go.
	mux.HandleFunc("GET /api/risk/stress-scenarios", h.handleListScenarios)
}

func (h *stressHandler) handleListScenarios(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
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
	rows, err := h.scenarios.ListScenarios(r.Context(), category)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
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

type stressRunRequest struct {
	ScenarioID string `json:"scenario_id"`
	Persist    bool   `json:"persist"`
}

type stressImpactWire struct {
	InstrumentKey      string  `json:"instrument_key"`
	Symbol             string  `json:"symbol"`
	AssetClass         string  `json:"asset_class,omitempty"`
	MarketValueBefore  float64 `json:"market_value_before"`
	MarketValueAfter   float64 `json:"market_value_after"`
	PnL                float64 `json:"pnl"`
	AppliedReturn      float64 `json:"applied_return"`
	AppliedShockType   string  `json:"applied_shock_type,omitempty"`
	AppliedShockKey    string  `json:"applied_shock_key,omitempty"`
}

type stressResultWire struct {
	FundID        string             `json:"fund_id"`
	ScenarioID    string             `json:"scenario_id"`
	CalculatedAt  string             `json:"calculated_at"`
	NAVBefore     float64            `json:"nav_before"`
	NAVAfter      float64            `json:"nav_after"`
	PnLTotal      float64            `json:"pnl_total"`
	PnLPct        float64            `json:"pnl_pct"`
	HoldingCount  int                `json:"holding_count"`
	ShockedCount  int                `json:"shocked_count"`
	Impacts       []stressImpactWire `json:"impacts"`
}

func projectStressResult(res stress.Result) stressResultWire {
	impacts := make([]stressImpactWire, 0, len(res.Impacts))
	for _, im := range res.Impacts {
		impacts = append(impacts, stressImpactWire{
			InstrumentKey:     im.InstrumentKey,
			Symbol:            im.Symbol,
			AssetClass:        im.AssetClass,
			MarketValueBefore: im.MarketValueBefore,
			MarketValueAfter:  im.MarketValueAfter,
			PnL:               im.PnL,
			AppliedReturn:     im.AppliedReturn,
			AppliedShockType:  im.AppliedShockType,
			AppliedShockKey:   im.AppliedShockKey,
		})
	}
	return stressResultWire{
		FundID:       res.FundID,
		ScenarioID:   res.ScenarioID,
		CalculatedAt: res.GeneratedAt.UTC().Format(time.RFC3339Nano),
		NAVBefore:    res.NAVBefore,
		NAVAfter:     res.NAVAfter,
		PnLTotal:     res.PnLTotal,
		PnLPct:       res.PnLPct,
		HoldingCount: res.HoldingCount,
		ShockedCount: res.ShockedCount,
		Impacts:      impacts,
	}
}

// holdingsToStress converts repo positions to engine inputs.
// Shorts are signed negative MV.
func holdingsToStress(rows []repository.HoldingPosition) []stress.Holding {
	out := make([]stress.Holding, 0, len(rows))
	for _, r := range rows {
		if r.InstrumentKey == "" {
			continue
		}
		mv := r.MarketValue
		side := strings.ToLower(strings.TrimSpace(r.PositionSide.String))
		if side == "short" || r.Quantity < 0 {
			if mv > 0 {
				mv = -mv
			}
		}
		out = append(out, stress.Holding{
			InstrumentKey: r.InstrumentKey,
			Symbol:        r.Symbol,
			AssetClass:    r.AssetClass.String,
			Market:        r.Market.String,
			MarketValue:   mv,
		})
	}
	return out
}

// scenarioNeedsFactorLoadings returns true when at least one
// shock has TargetType=factor; only then do we incur the
// loadings query.
func scenarioNeedsFactorLoadings(scen stress.Scenario) bool {
	for _, sh := range scen.Shocks {
		if sh.TargetType == stress.TargetFactor {
			return true
		}
	}
	return false
}

func (h *stressHandler) handleRun(w http.ResponseWriter, r *http.Request) {
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
	ctx := r.Context()
	if _, err := authorizeFundAccess(ctx, h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}
	var req stressRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	if strings.TrimSpace(req.ScenarioID) == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("scenario_id_required", ""))
		return
	}
	scen, err := h.scenarios.GetScenario(ctx, req.ScenarioID)
	if err != nil {
		if errors.Is(err, stress.ErrNotFound) {
			writeOrderActionJSON(w, http.StatusNotFound, errorPayload("scenario_not_found", req.ScenarioID))
			return
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	positions, err := h.positions.ListByFund(ctx, fundID)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	holdings := holdingsToStress(positions)

	var loadings stress.FactorLoadings
	if scenarioNeedsFactorLoadings(scen) && h.loadingsRepo != nil && len(holdings) > 0 {
		keys := make([]string, 0, len(holdings))
		for _, hd := range holdings {
			keys = append(keys, hd.InstrumentKey)
		}
		raw, err := h.loadingsRepo.LoadingsByInstruments(ctx, keys, time.Now().UTC())
		if err != nil {
			writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
			return
		}
		loadings = make(stress.FactorLoadings, len(holdings))
		for key, ld := range raw {
			if loadings[key.InstrumentKey] == nil {
				loadings[key.InstrumentKey] = map[string]float64{}
			}
			loadings[key.InstrumentKey][string(ld.Factor)] = ld.Loading
		}
	}

	res := h.engine.Compute(fundID, scen, holdings, loadings)
	if req.Persist {
		if err := h.scenarios.AppendResult(ctx, res); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"result":        projectStressResult(res),
				"persist_error": err.Error(),
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"result":   projectStressResult(res),
		"scenario": projectStressScenario(scen),
	})
}

func (h *stressHandler) handleHistory(w http.ResponseWriter, r *http.Request) {
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
	ctx := r.Context()
	if _, err := authorizeFundAccess(ctx, h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}
	q := r.URL.Query()
	limit := 90
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 && v <= 365 {
			limit = v
		}
	}
	rows, err := h.scenarios.ListResults(ctx, stress.ListResultsParams{
		FundID:     fundID,
		ScenarioID: strings.TrimSpace(q.Get("scenarioId")),
		Limit:      limit,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]stressResultWire, 0, len(rows))
	for _, row := range rows {
		out = append(out, projectStressResult(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": out})
}
