// var_handler.go — per-fund VaR / CVaR read API (S7 / P3-2).
//
// Routes
//
//	GET /api/funds/{fundId}/risk/var
//	    Live read: pulls the last N daily returns from
//	    nav_snapshots.daily_return, runs every (method ×
//	    confidence) combination, optionally persists a
//	    snapshot (?persist=1), and returns everything.
//
//	GET /api/funds/{fundId}/risk/var/trend
//	    Returns historical snapshots for one (method,
//	    confidence, horizon) combo. Powers the dashboard
//	    sparkline.
//
// Authorization mirrors the cash-ledger handler: shared
// authorizeFundAccess gate so a user can only read VaR for a
// fund they have an active role on.
//
// Why we don't expose Monte Carlo as the default — the three
// methods agree on calm markets but the spread between them is
// itself a fat-tail diagnostic. The UI shows all nine tiles
// (3 methods × 3 confidences) at the 1-day horizon by default
// and the PM clicks for 5d / 10d if needed.

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/varisk"
)

// Defaults. Tuned to give a stable 1y window which is what most
// regulators / LPs want to see; UI can override via query.
const (
	defaultVaRLookbackDays = 252
	defaultVaRHorizonDays  = 1
)

type varHandler struct {
	repo        *varisk.Repo
	fundRepo    *repository.FundRepo
	companyRepo *repository.FundCompanyRepo
	engine      *varisk.Engine
	metrics     *serverMetrics
}

func newVaRHandler(svc *Services) *varHandler {
	if svc == nil || svc.DB == nil {
		return nil
	}
	return &varHandler{
		repo:        varisk.NewRepo(svc.DB),
		fundRepo:    repository.NewFundRepo(svc.DB),
		companyRepo: repository.NewFundCompanyRepo(svc.DB),
		engine:      varisk.NewEngine(),
		metrics:     svc.Metrics,
	}
}

func (h *varHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/funds/{fundId}/risk/var", h.handleSnapshot)
	mux.HandleFunc("GET /api/funds/{fundId}/risk/var/trend", h.handleTrend)
}

// varResultWire is one (method, confidence) row.
type varResultWire struct {
	Method            string   `json:"method"`
	Confidence        float64  `json:"confidence"`
	Horizon           int      `json:"horizon"`
	VarPct            float64  `json:"var_pct"`
	CVarPct           float64  `json:"cvar_pct"`
	MonteCarloSeed    *int64   `json:"monte_carlo_seed,omitempty"`
	MonteCarloPaths   *int     `json:"monte_carlo_paths,omitempty"`
}

type varSnapshotWire struct {
	FundID           string          `json:"fund_id"`
	GeneratedAt      string          `json:"generated_at"`
	Horizon          int             `json:"horizon"`
	LookbackDays     int             `json:"lookback_days"`
	SampleSize       int             `json:"sample_size"`
	MeanDailyReturn  float64         `json:"mean_daily_return"`
	StdevDailyReturn float64         `json:"stdev_daily_return"`
	SampleWindowStart string         `json:"sample_window_start,omitempty"`
	SampleWindowEnd   string         `json:"sample_window_end,omitempty"`
	Results          []varResultWire `json:"results"`
}

func projectVaRSnapshot(snap varisk.Snapshot) varSnapshotWire {
	out := varSnapshotWire{
		FundID:           snap.FundID,
		GeneratedAt:      snap.GeneratedAt.UTC().Format(time.RFC3339Nano),
		Horizon:          snap.Horizon,
		LookbackDays:     snap.LookbackDays,
		SampleSize:       snap.SampleSize,
		MeanDailyReturn:  snap.Mean,
		StdevDailyReturn: snap.Std,
		Results:          make([]varResultWire, 0, len(snap.Results)),
	}
	if len(snap.Results) > 0 {
		first := snap.Results[0]
		if !first.SampleWindowStart.IsZero() {
			out.SampleWindowStart = first.SampleWindowStart.UTC().Format("2006-01-02")
		}
		if !first.SampleWindowEnd.IsZero() {
			out.SampleWindowEnd = first.SampleWindowEnd.UTC().Format("2006-01-02")
		}
	}
	for _, r := range snap.Results {
		out.Results = append(out.Results, varResultWire{
			Method:          string(r.Method),
			Confidence:      float64(r.Confidence),
			Horizon:         r.Horizon,
			VarPct:          r.Var,
			CVarPct:         r.CVar,
			MonteCarloSeed:  r.MonteCarloSeed,
			MonteCarloPaths: r.MonteCarloPaths,
		})
	}
	return out
}

func (h *varHandler) handleSnapshot(w http.ResponseWriter, r *http.Request) {
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
	lookback := defaultVaRLookbackDays
	if raw := strings.TrimSpace(q.Get("lookback")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v >= varisk.MinSampleSize && v <= 1500 {
			lookback = v
		} else {
			writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_lookback",
				"lookback must be an integer in [20, 1500]"))
			return
		}
	}
	horizon := defaultVaRHorizonDays
	if raw := strings.TrimSpace(q.Get("horizon")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v >= 1 && v <= 20 {
			horizon = v
		} else {
			writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_horizon",
				"horizon must be an integer in [1, 20]"))
			return
		}
	}

	returns, err := h.repo.DailyReturns(ctx, varisk.DailyReturnsParams{
		FundID:       fundID,
		LookbackDays: lookback,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if len(returns) < varisk.MinSampleSize {
		writeOrderActionJSON(w, http.StatusUnprocessableEntity, errorPayload("insufficient_history",
			"fund has fewer than "+strconv.Itoa(varisk.MinSampleSize)+
				" days of daily returns; populate nav_snapshots first"))
		return
	}

	opts := varisk.ComputeOptions{
		FundID:       fundID,
		Returns:      returns,
		LookbackDays: lookback,
		Horizon:      horizon,
	}
	snap, err := h.engine.ComputeAll(opts)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("compute_failed", err.Error()))
		return
	}

	if strings.TrimSpace(q.Get("persist")) == "1" {
		if err := h.repo.AppendSnapshot(ctx, snap); err != nil {
			// Surface the write failure so the caller can retry,
			// but still return the live snapshot — the PM cares
			// about the number more than the archive row.
			writeJSON(w, http.StatusOK, map[string]any{
				"snapshot":      projectVaRSnapshot(snap),
				"persist_error": err.Error(),
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshot":    projectVaRSnapshot(snap),
		"methods":     varisk.AllMethods,
		"confidences": varisk.AllConfidences,
	})
}

// trendPointWire mirrors varisk.TrendRow.
type varTrendPointWire struct {
	ID            int64   `json:"id"`
	CalculatedAt  string  `json:"calculated_at"`
	Method        string  `json:"method"`
	Confidence    float64 `json:"confidence"`
	HorizonDays   int     `json:"horizon_days"`
	VarPct        float64 `json:"var_pct"`
	CVarPct       float64 `json:"cvar_pct"`
	SampleSize    int     `json:"sample_size"`
	LookbackDays  int     `json:"lookback_days"`
}

func (h *varHandler) handleTrend(w http.ResponseWriter, r *http.Request) {
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
	method, ok := varisk.ParseMethod(q.Get("method"))
	if !ok {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_method",
			"method must be one of historical / parametric / monte_carlo"))
		return
	}
	confRaw := strings.TrimSpace(q.Get("confidence"))
	confVal := 0.95
	if confRaw != "" {
		v, err := strconv.ParseFloat(confRaw, 64)
		if err != nil {
			writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_confidence",
				"confidence must be one of 0.90 / 0.95 / 0.99"))
			return
		}
		confVal = v
	}
	conf := varisk.Confidence(confVal)
	if !conf.IsValid() {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_confidence",
			"confidence must be one of 0.90 / 0.95 / 0.99"))
		return
	}
	horizon := defaultVaRHorizonDays
	if raw := strings.TrimSpace(q.Get("horizon")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > 20 {
			writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_horizon",
				"horizon must be an integer in [1, 20]"))
			return
		}
		horizon = v
	}
	limit := 90
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 && v <= 365 {
			limit = v
		}
	}

	rows, err := h.repo.ListSnapshots(ctx, varisk.ListSnapshotsParams{
		FundID:      fundID,
		Method:      method,
		Confidence:  conf,
		HorizonDays: horizon,
		Limit:       limit,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]varTrendPointWire, 0, len(rows))
	for _, row := range rows {
		out = append(out, varTrendPointWire{
			ID:           row.ID,
			CalculatedAt: row.CalculatedAt.UTC().Format(time.RFC3339Nano),
			Method:       string(row.Method),
			Confidence:   float64(row.Confidence),
			HorizonDays:  row.HorizonDays,
			VarPct:       row.Var,
			CVarPct:      row.CVar,
			SampleSize:   row.SampleSize,
			LookbackDays: row.LookbackDays,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"points":      out,
		"methods":     varisk.AllMethods,
		"confidences": varisk.AllConfidences,
	})
}

// Silence unused-import warnings.
var (
	_ = json.NewDecoder
	_ = errors.New
)
