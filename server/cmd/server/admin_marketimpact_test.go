package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/marketimpact"
)

func newAdminMarketImpactEnv(t *testing.T) (*adminHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	repo := marketimpact.NewRepo(db)
	cache := marketimpact.NewCache(nil, marketimpact.CacheConfig{})
	adapter := marketimpact.NewSlippageAdapter(cache, marketimpact.NewEngine())
	h := &adminHandler{
		db:                  db,
		metrics:             newServerMetrics(),
		marketImpactRepo:    repo,
		marketImpactCache:   cache,
		marketImpactAdapter: adapter,
	}
	return h, mock, func() { _ = db.Close() }
}

// ----- auth gates -----

func TestAdminMarketImpact_List_Unauthenticated(t *testing.T) {
	h, _, cleanup := newAdminMarketImpactEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/marketimpact/instruments", nil)
	rr := httptest.NewRecorder()
	h.handleListMarketImpactInstruments(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestAdminMarketImpact_List_Forbidden(t *testing.T) {
	h, mock, cleanup := newAdminMarketImpactEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "user")
	req := authReq(http.MethodGet, "/api/admin/marketimpact/instruments", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListMarketImpactInstruments(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
}

// ----- list / get -----

func TestAdminMarketImpact_List_Happy(t *testing.T) {
	h, mock, cleanup := newAdminMarketImpactEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	mock.ExpectQuery("FROM instrument_liquidity").
		WillReturnRows(sqlmock.NewRows([]string{
			"instrument_key", "symbol", "market", "asset_class",
			"adv_shares", "adv_notional", "adv_window_days",
			"daily_volatility", "impact_coefficient", "impact_exponent",
			"min_slippage_bps", "max_slippage_bps",
			"last_calibrated_at", "calibration_source", "note", "updated_at",
		}).AddRow(
			"AAPL.US", "AAPL", "US", "equity",
			float64(50_000_000), nil, 20,
			0.02, 1.0, 0.5,
			1.0, 200.0,
			nil, "manual", "", now,
		))
	req := authReq(http.MethodGet, "/api/admin/marketimpact/instruments", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListMarketImpactInstruments(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Instruments []liquidityWire `json:"instruments"`
		Total       int             `json:"total"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 1 || len(body.Instruments) != 1 || body.Instruments[0].Symbol != "AAPL" {
		t.Errorf("got %+v", body)
	}
}

func TestAdminMarketImpact_Get_NotFound(t *testing.T) {
	h, mock, cleanup := newAdminMarketImpactEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	mock.ExpectQuery("FROM instrument_liquidity").
		WithArgs("MISS").
		WillReturnRows(sqlmock.NewRows([]string{}))
	req := authReq(http.MethodGet, "/api/admin/marketimpact/instruments/MISS", "", "u-1")
	req.SetPathValue("key", "MISS")
	rr := httptest.NewRecorder()
	h.handleGetMarketImpactInstrument(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

// ----- upsert -----

func TestAdminMarketImpact_Upsert_RequiresFields(t *testing.T) {
	h, mock, cleanup := newAdminMarketImpactEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	// Missing market.
	req := authReq(http.MethodPut, "/api/admin/marketimpact/instruments/AAPL.US",
		`{"symbol":"AAPL"}`, "u-1")
	req.SetPathValue("key", "AAPL.US")
	rr := httptest.NewRecorder()
	h.handleUpsertMarketImpactInstrument(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminMarketImpact_Upsert_Happy(t *testing.T) {
	h, mock, cleanup := newAdminMarketImpactEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	mock.ExpectQuery("INSERT INTO instrument_liquidity").
		WillReturnRows(sqlmock.NewRows([]string{
			"instrument_key", "symbol", "market", "asset_class",
			"adv_shares", "adv_notional", "adv_window_days",
			"daily_volatility", "impact_coefficient", "impact_exponent",
			"min_slippage_bps", "max_slippage_bps",
			"last_calibrated_at", "calibration_source", "note", "updated_at",
		}).AddRow(
			"AAPL.US", "AAPL", "US", "equity",
			float64(50_000_000), nil, 20,
			0.02, 1.0, 0.5,
			1.0, 500.0,
			nil, "manual", "", now,
		))
	req := authReq(http.MethodPut, "/api/admin/marketimpact/instruments/AAPL.US",
		`{"symbol":"AAPL","market":"US","asset_class":"equity","adv_shares":50000000,"daily_volatility":0.02}`,
		"u-1")
	req.SetPathValue("key", "AAPL.US")
	rr := httptest.NewRecorder()
	h.handleUpsertMarketImpactInstrument(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	// Cache should now have the row so the engine sees it.
	if h.marketImpactCache.Lookup("AAPL.US") == nil {
		t.Error("expected cache to have row after upsert")
	}
	if h.metrics.marketImpactEvents["admin_upsert"] != 1 {
		t.Errorf("admin_upsert metric = %d", h.metrics.marketImpactEvents["admin_upsert"])
	}
}

// ----- delete -----

func TestAdminMarketImpact_Delete_Happy(t *testing.T) {
	h, mock, cleanup := newAdminMarketImpactEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	mock.ExpectExec("DELETE FROM instrument_liquidity").
		WithArgs("AAPL.US").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Pre-seed the cache so we can verify ApplyChange(nil) works.
	h.marketImpactCache.SetRows([]marketimpact.Liquidity{{InstrumentKey: "AAPL.US"}})
	req := authReq(http.MethodDelete, "/api/admin/marketimpact/instruments/AAPL.US", "", "u-1")
	req.SetPathValue("key", "AAPL.US")
	rr := httptest.NewRecorder()
	h.handleDeleteMarketImpactInstrument(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if h.marketImpactCache.Lookup("AAPL.US") != nil {
		t.Error("expected cache row to be removed")
	}
}

func TestAdminMarketImpact_Delete_NotFound(t *testing.T) {
	h, mock, cleanup := newAdminMarketImpactEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	mock.ExpectExec("DELETE FROM instrument_liquidity").
		WithArgs("X.US").
		WillReturnResult(sqlmock.NewResult(0, 0))
	req := authReq(http.MethodDelete, "/api/admin/marketimpact/instruments/X.US", "", "u-1")
	req.SetPathValue("key", "X.US")
	rr := httptest.NewRecorder()
	h.handleDeleteMarketImpactInstrument(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

// ----- preview -----

func TestAdminMarketImpact_Preview_Happy(t *testing.T) {
	h, mock, cleanup := newAdminMarketImpactEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	// Pre-seed cache so the engine returns a calibrated bps.
	adv := 10_000_000.0
	sigma := 0.02
	h.marketImpactCache.SetRows([]marketimpact.Liquidity{{
		InstrumentKey:     "AAPL.US",
		Symbol:            "AAPL",
		Market:            "US",
		AssetClass:        "equity",
		ADVShares:         &adv,
		DailyVolatility:   &sigma,
		ImpactCoefficient: 1.0,
		ImpactExponent:    0.5,
		MinSlippageBps:    1,
		MaxSlippageBps:    500,
	}})
	body := `{"instrument_key":"AAPL.US","symbol":"AAPL","asset_class":"equity","side":"buy","quantity":100000,"reference_price":200}`
	req := authReq(http.MethodPost, "/api/admin/marketimpact/preview", body, "u-1")
	rr := httptest.NewRecorder()
	h.handleMarketImpactPreview(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Estimate    estimateWire `json:"estimate"`
		ImpliedFill float64      `json:"implied_fill"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 1% of ADV → 20 bps; ImpliedFill = 200*1.002 = 200.4.
	if resp.Estimate.AdverseBps < 19 || resp.Estimate.AdverseBps > 21 {
		t.Errorf("expected ~20 bps, got %v", resp.Estimate.AdverseBps)
	}
	if resp.ImpliedFill < 200.39 || resp.ImpliedFill > 200.41 {
		t.Errorf("expected ~200.40 implied, got %v", resp.ImpliedFill)
	}
}

func TestAdminMarketImpact_Preview_RejectsBadInput(t *testing.T) {
	h, mock, cleanup := newAdminMarketImpactEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	cases := []string{
		`{}`,
		`{"instrument_key":"AAPL.US","side":"buy","quantity":-1,"reference_price":100}`,
		`{"instrument_key":"AAPL.US","side":"x","quantity":1,"reference_price":100}`,
	}
	for i, b := range cases {
		mock.ExpectQuery("SELECT (.+) FROM users").
			WithArgs("u-1").
			WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("admin"))
		req := authReq(http.MethodPost, "/api/admin/marketimpact/preview", b, "u-1")
		rr := httptest.NewRecorder()
		h.handleMarketImpactPreview(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("case %d: status = %d body=%s", i, rr.Code, rr.Body.String())
		}
	}
}

// ----- cache stats / refresh -----

func TestAdminMarketImpact_CacheStats_Happy(t *testing.T) {
	h, mock, cleanup := newAdminMarketImpactEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	h.marketImpactCache.SetRows([]marketimpact.Liquidity{
		{InstrumentKey: "A.US"},
		{InstrumentKey: "B.US"},
	})
	req := authReq(http.MethodGet, "/api/admin/marketimpact/cache", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleMarketImpactCacheStats(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Size        int    `json:"size"`
		LastRefresh string `json:"last_refresh"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Size != 2 {
		t.Errorf("size = %d, want 2", resp.Size)
	}
	if resp.LastRefresh == "" {
		t.Error("last_refresh empty")
	}
}
