// brinson_handler_test.go — per-fund Brinson runner tests (S7 / P3-4).

package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/brinson"
)

func newBrinsonHandlerEnv(t *testing.T) (*brinsonHandler, sqlmock.Sqlmock, *sql.DB, func()) {
	t.Helper()
	db, mock := newMockDB(t)
	h := newBrinsonHandler(&Services{DB: db, BrinsonRepo: brinson.NewRepo(db)})
	if h == nil {
		t.Fatal("newBrinsonHandler returned nil")
	}
	return h, mock, db, func() { _ = db.Close() }
}

func expectFundOwnershipBrinson(mock sqlmock.Sqlmock, fundID, companyID, userID string) {
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "company_id", "name", "description", "trading_mode",
			"initial_capital", "current_capital", "total_assets", "nav", "status",
			"config", "created_at", "updated_at",
		}).AddRow(fundID, companyID, "Fund", "", "simulation",
			100000.0, 100000.0, 100000.0, 1.0, "active",
			[]byte("{}"), now, now,
		))
	mock.ExpectQuery("FROM fund_companies").
		WithArgs(companyID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "owner_user_id", "name", "description", "created_at", "updated_at",
		}).AddRow(companyID, userID, "Co", "", now, now))
}

func brinsonPositionRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "fund_id", "instrument_key", "symbol", "name", "market", "exchange",
		"asset_class", "instrument_type", "position_side", "quote_currency",
		"settlement_currency", "margin_mode", "quantity", "available_qty",
		"cost_price", "current_price", "market_value", "weight", "leverage",
		"contract_multiplier", "expiry_date", "unrealized_pnl", "margin_used",
		"updated_at",
	})
}

func TestBrinson_Run_Unauthenticated(t *testing.T) {
	h, _, _, cleanup := newBrinsonHandlerEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodPost, "/api/funds/f1/brinson/run", nil)
	req.SetPathValue("fundId", "f1")
	rr := httptest.NewRecorder()
	h.handleRun(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestBrinson_Run_MissingBenchmarkID(t *testing.T) {
	h, mock, _, cleanup := newBrinsonHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipBrinson(mock, fundID, companyID, userID)
	req := authReq(http.MethodPost, "/api/funds/"+fundID+"/brinson/run",
		`{"dimension":"asset_class"}`, userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleRun(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBrinson_Run_InvalidDimension(t *testing.T) {
	h, mock, _, cleanup := newBrinsonHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipBrinson(mock, fundID, companyID, userID)
	req := authReq(http.MethodPost, "/api/funds/"+fundID+"/brinson/run",
		`{"benchmark_id":"spx","dimension":"bogus"}`, userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleRun(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBrinson_Run_CompositionNotFound(t *testing.T) {
	h, mock, _, cleanup := newBrinsonHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipBrinson(mock, fundID, companyID, userID)
	mock.ExpectQuery("FROM brinson_benchmark_compositions").
		WillReturnError(sql.ErrNoRows)
	req := authReq(http.MethodPost, "/api/funds/"+fundID+"/brinson/run",
		`{"benchmark_id":"unknown","dimension":"asset_class"}`, userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleRun(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBrinson_Run_HappyPath(t *testing.T) {
	h, mock, _, cleanup := newBrinsonHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipBrinson(mock, fundID, companyID, userID)
	now := time.Now()
	asof := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery("FROM brinson_benchmark_compositions").
		WithArgs("spx", "asset_class").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "benchmark_id", "bucket_dimension", "asof", "buckets", "note",
			"created_by", "created_at", "updated_at",
		}).AddRow("c1", "spx", "asset_class", asof,
			[]byte(`[{"key":"equity","weight":0.6,"return_pct":0.10},
			          {"key":"bond","weight":0.4,"return_pct":0.05}]`),
			"", nil, now, now))
	// Two positions, both equity, with a tasty +20% gain so the
	// equity bucket clearly outperforms the benchmark.
	mock.ExpectQuery("FROM holding_positions WHERE fund_id").
		WithArgs(fundID).
		WillReturnRows(brinsonPositionRows().
			AddRow("p1", fundID, "US:AAPL", "AAPL", nil, "US", nil,
				"equity", nil, nil, nil,
				nil, nil, float64(100), float64(100),
				float64(150), float64(180), float64(18000), float64(0.6), nil,
				nil, nil, nil, nil,
				now,
			).
			AddRow("p2", fundID, "US:MSFT", "MSFT", nil, "US", nil,
				"equity", nil, nil, nil,
				nil, nil, float64(50), float64(50),
				float64(200), float64(240), float64(12000), float64(0.4), nil,
				nil, nil, nil, nil,
				now,
			))
	req := authReq(http.MethodPost, "/api/funds/"+fundID+"/brinson/run",
		`{"benchmark_id":"spx","dimension":"asset_class"}`, userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleRun(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Result        brinsonResultWire `json:"result"`
		CompositionID string            `json:"composition_id"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.CompositionID != "c1" {
		t.Errorf("composition_id = %q", body.CompositionID)
	}
	if body.Result.BenchmarkID != "spx" {
		t.Errorf("benchmark_id = %q", body.Result.BenchmarkID)
	}
	if body.Result.BucketCount != 2 {
		t.Errorf("bucket_count = %d (expected 2; equity + benchmark-only bond)", body.Result.BucketCount)
	}
	// portfolio_return = 0.20 (100% equity at 20%);
	// benchmark_return = 0.6*0.10 + 0.4*0.05 = 0.08
	if body.Result.PortfolioReturn-0.20 > 1e-9 || body.Result.PortfolioReturn-0.20 < -1e-9 {
		t.Errorf("portfolio_return = %f, want 0.20", body.Result.PortfolioReturn)
	}
	if body.Result.BenchmarkReturn-0.08 > 1e-9 || body.Result.BenchmarkReturn-0.08 < -1e-9 {
		t.Errorf("benchmark_return = %f, want 0.08", body.Result.BenchmarkReturn)
	}
	if body.Result.ActiveReturn-0.12 > 1e-9 || body.Result.ActiveReturn-0.12 < -1e-9 {
		t.Errorf("active_return = %f, want 0.12", body.Result.ActiveReturn)
	}
	// Identity: alloc + sel + inter ≈ active_return
	sum := body.Result.AllocationTotal + body.Result.SelectionTotal + body.Result.InteractionTotal
	diff := sum - body.Result.ActiveReturn
	if diff > 1e-9 || diff < -1e-9 {
		t.Errorf("decomposition broken: sum=%f active=%f", sum, body.Result.ActiveReturn)
	}
}

func TestBrinson_Run_Persist(t *testing.T) {
	h, mock, _, cleanup := newBrinsonHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipBrinson(mock, fundID, companyID, userID)
	now := time.Now()
	asof := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery("FROM brinson_benchmark_compositions").
		WithArgs("spx", "asset_class").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "benchmark_id", "bucket_dimension", "asof", "buckets", "note",
			"created_by", "created_at", "updated_at",
		}).AddRow("c1", "spx", "asset_class", asof,
			[]byte(`[{"key":"equity","weight":1.0,"return_pct":0.05}]`),
			"", nil, now, now))
	mock.ExpectQuery("FROM holding_positions WHERE fund_id").
		WithArgs(fundID).
		WillReturnRows(brinsonPositionRows().AddRow(
			"p1", fundID, "US:AAPL", "AAPL", nil, "US", nil,
			"equity", nil, nil, nil,
			nil, nil, float64(100), float64(100),
			float64(150), float64(180), float64(18000), float64(1.0), nil,
			nil, nil, nil, nil,
			now,
		))
	mock.ExpectExec("INSERT INTO brinson_attribution_snapshots").
		WillReturnResult(sqlmock.NewResult(1, 1))
	req := authReq(http.MethodPost, "/api/funds/"+fundID+"/brinson/run",
		`{"benchmark_id":"spx","dimension":"asset_class","persist":true}`, userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleRun(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBrinson_Run_NoHoldings(t *testing.T) {
	h, mock, _, cleanup := newBrinsonHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipBrinson(mock, fundID, companyID, userID)
	now := time.Now()
	asof := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery("FROM brinson_benchmark_compositions").
		WithArgs("spx", "asset_class").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "benchmark_id", "bucket_dimension", "asof", "buckets", "note",
			"created_by", "created_at", "updated_at",
		}).AddRow("c1", "spx", "asset_class", asof,
			[]byte(`[{"key":"equity","weight":1.0,"return_pct":0.10}]`),
			"", nil, now, now))
	mock.ExpectQuery("FROM holding_positions WHERE fund_id").
		WithArgs(fundID).
		WillReturnRows(brinsonPositionRows())
	req := authReq(http.MethodPost, "/api/funds/"+fundID+"/brinson/run",
		`{"benchmark_id":"spx","dimension":"asset_class"}`, userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleRun(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBrinson_History_HappyPath(t *testing.T) {
	h, mock, _, cleanup := newBrinsonHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipBrinson(mock, fundID, companyID, userID)
	now := time.Now()
	mock.ExpectQuery("FROM brinson_attribution_snapshots").
		WithArgs(fundID, 90).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "benchmark_id", "bucket_dimension", "composition_id",
			"calculated_at",
			"allocation_total", "selection_total", "interaction_total",
			"active_return", "portfolio_return", "benchmark_return",
			"bucket_count", "bucket_details",
		}).AddRow(int64(1), fundID, "spx", "asset_class", "c1", now,
			0.005, 0.008, 0.003, 0.016, 0.096, 0.080, 2, []byte(`[]`)))
	req := authReq(http.MethodGet, "/api/funds/"+fundID+"/brinson/history", "", userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleHistory(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Results) != 1 {
		t.Errorf("got %d results", len(body.Results))
	}
}

func TestBrinson_ListBenchmarks_Unauthenticated(t *testing.T) {
	h, _, _, cleanup := newBrinsonHandlerEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/brinson/benchmarks", nil)
	rr := httptest.NewRecorder()
	h.handleListBenchmarks(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestBrinson_ListBenchmarks_Dedupes(t *testing.T) {
	h, mock, _, cleanup := newBrinsonHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	now := time.Now()
	asof1 := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	asof2 := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	// Two rows of the same (spx, asset_class) — only one wire item.
	mock.ExpectQuery("FROM brinson_benchmark_compositions").
		WithArgs(500).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "benchmark_id", "bucket_dimension", "asof", "buckets", "note",
			"created_by", "created_at", "updated_at",
		}).
			AddRow("c1", "spx", "asset_class", asof1, []byte(`[]`), "", nil, now, now).
			AddRow("c2", "spx", "asset_class", asof2, []byte(`[]`), "", nil, now, now).
			AddRow("c3", "spx", "market", asof1, []byte(`[]`), "", nil, now, now))
	req := authReq(http.MethodGet, "/api/brinson/benchmarks", "", userID)
	rr := httptest.NewRecorder()
	h.handleListBenchmarks(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Benchmarks []map[string]any `json:"benchmarks"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if len(body.Benchmarks) != 2 {
		t.Errorf("expected 2 deduped entries, got %d: %+v", len(body.Benchmarks), body.Benchmarks)
	}
}
