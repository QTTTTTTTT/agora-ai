// brinson_handler.go — per-fund Brinson attribution runner (S7 / P3-4).
//
// Routes
//
//   POST /api/funds/{fundId}/brinson/run
//        Body: {
//           "benchmark_id":  "spx",
//           "dimension":     "asset_class",
//           "composition_id": "optional override",
//           "asof":          "YYYY-MM-DD",
//           "persist":        false
//        }
//        Pulls the portfolio composition from current holdings,
//        joins it with the chosen benchmark composition, runs the
//        Brinson three-effect engine and (optionally) persists
//        the snapshot.
//
//   GET  /api/funds/{fundId}/brinson/history
//             [?benchmarkId=X&dimension=Y&limit=N]
//        Append-only archive used by the trend chart.
//
//   GET  /api/brinson/benchmarks
//        Authenticated catalog of saved benchmark compositions
//        so fund operators can pick one without admin rights.
//
// Why POST for the run path: identical to stress_handler — each
// run touches an external benchmark id and (optionally) writes a
// new archive row, so it can't be a cacheable GET.

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/brinson"
	"github.com/fundai/server/internal/repository"
)

type brinsonHandler struct {
	repo        *brinson.Repo
	positions   *repository.PositionRepo
	fundRepo    *repository.FundRepo
	companyRepo *repository.FundCompanyRepo
	engine      *brinson.Engine
	now         func() time.Time
}

func newBrinsonHandler(svc *Services) *brinsonHandler {
	if svc == nil || svc.DB == nil || svc.BrinsonRepo == nil {
		return nil
	}
	return &brinsonHandler{
		repo:        svc.BrinsonRepo,
		positions:   repository.NewPositionRepo(svc.DB),
		fundRepo:    repository.NewFundRepo(svc.DB),
		companyRepo: repository.NewFundCompanyRepo(svc.DB),
		engine:      brinson.NewEngine(),
		now:         time.Now,
	}
}

func (h *brinsonHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("POST /api/funds/{fundId}/brinson/run", h.handleRun)
	mux.HandleFunc("GET /api/funds/{fundId}/brinson/history", h.handleHistory)
	mux.HandleFunc("GET /api/brinson/benchmarks", h.handleListBenchmarks)
}

// handleListBenchmarks: any authenticated user can browse the
// catalog. The admin endpoints in admin_brinson.go gate write
// access. We deduplicate by (benchmark_id, dimension) so the
// fund operator sees one row per benchmark slice.
func (h *brinsonHandler) handleListBenchmarks(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	rows, err := h.repo.ListCompositions(r.Context(), brinson.ListCompositionsParams{Limit: 500})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	type wire struct {
		BenchmarkID string `json:"benchmark_id"`
		Dimension   string `json:"dimension"`
		LatestAsOf  string `json:"latest_asof"`
	}
	seen := map[string]bool{}
	out := make([]wire, 0)
	for _, row := range rows {
		k := row.BenchmarkID + "|" + string(row.Dimension)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, wire{
			BenchmarkID: row.BenchmarkID,
			Dimension:   string(row.Dimension),
			LatestAsOf:  row.AsOf.UTC().Format("2006-01-02"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"benchmarks": out,
		"dimensions": brinson.AllDimensions,
	})
}

type brinsonRunRequest struct {
	BenchmarkID   string `json:"benchmark_id"`
	Dimension     string `json:"dimension"`
	CompositionID string `json:"composition_id,omitempty"`
	AsOf          string `json:"asof,omitempty"`
	Persist       bool   `json:"persist"`
}

type brinsonBucketAttributionWire struct {
	Key             string  `json:"key"`
	PortfolioWeight float64 `json:"portfolio_weight"`
	BenchmarkWeight float64 `json:"benchmark_weight"`
	PortfolioReturn float64 `json:"portfolio_return"`
	BenchmarkReturn float64 `json:"benchmark_return"`
	Allocation      float64 `json:"allocation"`
	Selection       float64 `json:"selection"`
	Interaction     float64 `json:"interaction"`
	TotalEffect     float64 `json:"total_effect"`
}

func projectBrinsonBucket(b brinson.BucketAttribution) brinsonBucketAttributionWire {
	return brinsonBucketAttributionWire{
		Key:             b.Key,
		PortfolioWeight: b.PortfolioWeight,
		BenchmarkWeight: b.BenchmarkWeight,
		PortfolioReturn: b.PortfolioReturn,
		BenchmarkReturn: b.BenchmarkReturn,
		Allocation:      b.AllocationEffect,
		Selection:       b.SelectionEffect,
		Interaction:     b.InteractionEffect,
		TotalEffect:     b.AllocationEffect + b.SelectionEffect + b.InteractionEffect,
	}
}

type brinsonResultWire struct {
	FundID           string                         `json:"fund_id"`
	BenchmarkID      string                         `json:"benchmark_id"`
	Dimension        string                         `json:"dimension"`
	CompositionID    string                         `json:"composition_id,omitempty"`
	CalculatedAt     string                         `json:"calculated_at"`
	PortfolioReturn  float64                        `json:"portfolio_return"`
	BenchmarkReturn  float64                        `json:"benchmark_return"`
	ActiveReturn     float64                        `json:"active_return"`
	AllocationTotal  float64                        `json:"allocation_total"`
	SelectionTotal   float64                        `json:"selection_total"`
	InteractionTotal float64                        `json:"interaction_total"`
	BucketCount      int                            `json:"bucket_count"`
	Buckets          []brinsonBucketAttributionWire `json:"buckets"`
}

func projectBrinsonResult(fundID, compositionID string, res brinson.Result) brinsonResultWire {
	buckets := make([]brinsonBucketAttributionWire, 0, len(res.Buckets))
	for _, b := range res.Buckets {
		buckets = append(buckets, projectBrinsonBucket(b))
	}
	return brinsonResultWire{
		FundID:           fundID,
		BenchmarkID:      res.BenchmarkID,
		Dimension:        string(res.Dimension),
		CompositionID:    compositionID,
		CalculatedAt:     res.GeneratedAt.UTC().Format(time.RFC3339Nano),
		PortfolioReturn:  res.PortfolioReturn,
		BenchmarkReturn:  res.BenchmarkReturn,
		ActiveReturn:     res.ActiveReturn,
		AllocationTotal:  res.AllocationTotal,
		SelectionTotal:   res.SelectionTotal,
		InteractionTotal: res.InteractionTotal,
		BucketCount:      res.BucketCount,
		Buckets:          buckets,
	}
}

// holdingsToBrinsonInputs converts repo positions to bucket-keyed
// rows for a chosen dimension. Buckets w/o data are skipped.
//
// Return is computed as (current_price - cost_price) / cost_price
// — i.e. since-inception unrealized P&L percentage. Sector data
// isn't stored on HoldingPosition yet, so the "sector" dimension
// will yield no portfolio buckets until that field is added.
func holdingsToBrinsonInputs(rows []repository.HoldingPosition, dim brinson.BucketDimension) []brinson.HoldingInput {
	out := make([]brinson.HoldingInput, 0, len(rows))
	for _, r := range rows {
		var bucket string
		switch dim {
		case brinson.DimAssetClass:
			bucket = r.AssetClass.String
		case brinson.DimMarket:
			bucket = r.Market.String
		case brinson.DimSector:
			// Sector classification not yet on HoldingPosition;
			// caller will get an empty portfolio composition and
			// receive a 400.
			continue
		}
		if strings.TrimSpace(bucket) == "" {
			continue
		}
		var ret float64
		if r.CostPrice > 0 {
			ret = (r.CurrentPrice - r.CostPrice) / r.CostPrice
		}
		out = append(out, brinson.HoldingInput{
			Bucket:      bucket,
			MarketValue: r.MarketValue,
			ReturnPct:   ret,
		})
	}
	return out
}

func (h *brinsonHandler) handleRun(w http.ResponseWriter, r *http.Request) {
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
	var req brinsonRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	dim, ok := brinson.ParseBucketDimension(req.Dimension)
	if !ok {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_dimension", req.Dimension))
		return
	}
	if strings.TrimSpace(req.BenchmarkID) == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("benchmark_id_required", ""))
		return
	}
	// Fetch benchmark composition. composition_id wins over
	// latest-by-asof so admins can pin a specific snapshot.
	var (
		compRow brinson.CompositionRow
		err     error
	)
	if strings.TrimSpace(req.CompositionID) != "" {
		rows, lerr := h.repo.ListCompositions(ctx, brinson.ListCompositionsParams{
			BenchmarkID: req.BenchmarkID,
			Dimension:   dim,
			Limit:       1000,
		})
		if lerr != nil {
			writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", lerr.Error()))
			return
		}
		for _, row := range rows {
			if row.ID == req.CompositionID {
				compRow = row
				break
			}
		}
		if compRow.ID == "" {
			writeOrderActionJSON(w, http.StatusNotFound, errorPayload("composition_not_found", req.CompositionID))
			return
		}
	} else {
		compRow, err = h.repo.GetLatestComposition(ctx, req.BenchmarkID, dim)
		if err != nil {
			if errors.Is(err, brinson.ErrNotFound) {
				writeOrderActionJSON(w, http.StatusNotFound, errorPayload("composition_not_found", req.BenchmarkID))
				return
			}
			writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
			return
		}
	}
	// Build the portfolio composition from current holdings.
	positions, err := h.positions.ListByFund(ctx, fundID)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	asof := h.now().UTC()
	if v := strings.TrimSpace(req.AsOf); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			asof = t
		}
	}
	port := brinson.PortfolioFromHoldings(dim, holdingsToBrinsonInputs(positions, dim), asof)
	if len(port.Buckets) == 0 {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("no_portfolio_holdings", "no holdings carry the requested dimension"))
		return
	}
	res := h.engine.Compute(port, compRow.AsComposition())
	if req.Persist {
		if perr := h.repo.AppendSnapshot(ctx, res, fundID, compRow.ID); perr != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"result":         projectBrinsonResult(fundID, compRow.ID, res),
				"composition_id": compRow.ID,
				"persist_error":  perr.Error(),
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"result":         projectBrinsonResult(fundID, compRow.ID, res),
		"composition_id": compRow.ID,
	})
}

func (h *brinsonHandler) handleHistory(w http.ResponseWriter, r *http.Request) {
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
	params := brinson.ListSnapshotsParams{
		FundID:      fundID,
		BenchmarkID: strings.TrimSpace(q.Get("benchmarkId")),
	}
	if d := strings.TrimSpace(q.Get("dimension")); d != "" {
		dim, ok := brinson.ParseBucketDimension(d)
		if !ok {
			writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_dimension", d))
			return
		}
		params.Dimension = dim
	}
	if v, err := strconv.Atoi(strings.TrimSpace(q.Get("limit"))); err == nil && v > 0 && v <= 365 {
		params.Limit = v
	}
	rows, err := h.repo.ListSnapshots(ctx, params)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	type historyWire struct {
		ID               int64                          `json:"id"`
		FundID           string                         `json:"fund_id"`
		BenchmarkID      string                         `json:"benchmark_id"`
		Dimension        string                         `json:"dimension"`
		CompositionID    string                         `json:"composition_id"`
		CalculatedAt     string                         `json:"calculated_at"`
		ActiveReturn     float64                        `json:"active_return"`
		PortfolioReturn  float64                        `json:"portfolio_return"`
		BenchmarkReturn  float64                        `json:"benchmark_return"`
		AllocationTotal  float64                        `json:"allocation_total"`
		SelectionTotal   float64                        `json:"selection_total"`
		InteractionTotal float64                        `json:"interaction_total"`
		BucketCount      int                            `json:"bucket_count"`
		Buckets          []brinsonBucketAttributionWire `json:"buckets,omitempty"`
	}
	out := make([]historyWire, 0, len(rows))
	for _, row := range rows {
		buckets := make([]brinsonBucketAttributionWire, 0, len(row.Buckets))
		for _, b := range row.Buckets {
			buckets = append(buckets, projectBrinsonBucket(b))
		}
		out = append(out, historyWire{
			ID:               row.ID,
			FundID:           row.FundID,
			BenchmarkID:      row.BenchmarkID,
			Dimension:        string(row.Dimension),
			CompositionID:    row.CompositionID,
			CalculatedAt:     row.CalculatedAt.UTC().Format(time.RFC3339Nano),
			ActiveReturn:     row.ActiveReturn,
			PortfolioReturn:  row.PortfolioReturn,
			BenchmarkReturn:  row.BenchmarkReturn,
			AllocationTotal:  row.AllocationTotal,
			SelectionTotal:   row.SelectionTotal,
			InteractionTotal: row.InteractionTotal,
			BucketCount:      row.BucketCount,
			Buckets:          buckets,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": out})
}
