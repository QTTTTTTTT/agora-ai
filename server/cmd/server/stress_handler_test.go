// Per-fund stress-test handler tests (S7 / P3-3).

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
	"github.com/fundai/server/internal/stress"
)

func newStressHandlerEnv(t *testing.T) (*stressHandler, sqlmock.Sqlmock, *sql.DB, func()) {
	t.Helper()
	db, mock := newMockDB(t)
	h := newStressHandler(&Services{DB: db, StressRepo: stress.NewRepo(db)})
	if h == nil {
		t.Fatal("newStressHandler returned nil")
	}
	return h, mock, db, func() { _ = db.Close() }
}

func expectFundOwnershipStress(mock sqlmock.Sqlmock, fundID, companyID, userID string) {
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

func stressPositionRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "fund_id", "instrument_key", "symbol", "name", "market", "exchange",
		"asset_class", "instrument_type", "position_side", "quote_currency",
		"settlement_currency", "margin_mode", "quantity", "available_qty",
		"cost_price", "current_price", "market_value", "weight", "leverage",
		"contract_multiplier", "expiry_date", "unrealized_pnl", "margin_used",
		"updated_at",
	})
}

func TestStress_Run_Unauthenticated(t *testing.T) {
	h, _, _, cleanup := newStressHandlerEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodPost, "/api/funds/f1/risk/stress", nil)
	req.SetPathValue("fundId", "f1")
	rr := httptest.NewRecorder()
	h.handleRun(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestStress_Run_MissingScenarioID(t *testing.T) {
	h, mock, _, cleanup := newStressHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipStress(mock, fundID, companyID, userID)
	req := authReq(http.MethodPost, "/api/funds/"+fundID+"/risk/stress", `{}`, userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleRun(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestStress_Run_ScenarioNotFound(t *testing.T) {
	h, mock, _, cleanup := newStressHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipStress(mock, fundID, companyID, userID)
	mock.ExpectQuery("FROM stress_scenarios").
		WithArgs("missing").
		WillReturnError(sql.ErrNoRows)
	req := authReq(http.MethodPost, "/api/funds/"+fundID+"/risk/stress",
		`{"scenario_id":"missing"}`, userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleRun(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestStress_Run_HappyPath(t *testing.T) {
	h, mock, _, cleanup := newStressHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipStress(mock, fundID, companyID, userID)
	now := time.Now()
	// Scenario lookup
	mock.ExpectQuery("FROM stress_scenarios").
		WithArgs("scen-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "name", "category", "description", "shocks", "created_by", "created_at", "updated_at",
		}).AddRow("scen-1", "Asset-class crash", "historical", "",
			[]byte(`[{"target_type":"asset_class","target_key":"equity","value":-0.20}]`),
			nil, now, now))
	// Positions
	mock.ExpectQuery("FROM holding_positions WHERE fund_id").
		WithArgs(fundID).
		WillReturnRows(stressPositionRows().AddRow(
			"p1", fundID, "US:AAPL", "AAPL", nil, "US", nil,
			"equity", nil, nil, nil,
			nil, nil, float64(100), float64(100),
			float64(150), float64(170), float64(17000), float64(1.0), nil,
			nil, nil, nil, nil,
			now,
		))
	// No factor shock in this scenario → no loadings query.
	req := authReq(http.MethodPost, "/api/funds/"+fundID+"/risk/stress",
		`{"scenario_id":"scen-1"}`, userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleRun(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Result   stressResultWire   `json:"result"`
		Scenario stressScenarioWire `json:"scenario"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Result.NAVBefore != 17000 {
		t.Errorf("nav_before = %f", body.Result.NAVBefore)
	}
	// equity at -20% → -3400 PnL (float epsilon).
	if diff := body.Result.PnLTotal - -3400; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("pnl_total = %f, want -3400", body.Result.PnLTotal)
	}
}

func TestStress_Run_PersistArchives(t *testing.T) {
	h, mock, _, cleanup := newStressHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipStress(mock, fundID, companyID, userID)
	now := time.Now()
	mock.ExpectQuery("FROM stress_scenarios").
		WithArgs("scen-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "name", "category", "description", "shocks", "created_by", "created_at", "updated_at",
		}).AddRow("scen-1", "Wildcard crash", "historical", "",
			[]byte(`[{"target_type":"wildcard","target_key":"*","value":-0.10}]`),
			nil, now, now))
	mock.ExpectQuery("FROM holding_positions WHERE fund_id").
		WithArgs(fundID).
		WillReturnRows(stressPositionRows())
	mock.ExpectExec("INSERT INTO portfolio_stress_results").
		WillReturnResult(sqlmock.NewResult(1, 1))
	req := authReq(http.MethodPost, "/api/funds/"+fundID+"/risk/stress",
		`{"scenario_id":"scen-1","persist":true}`, userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleRun(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestStress_History_HappyPath(t *testing.T) {
	h, mock, _, cleanup := newStressHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipStress(mock, fundID, companyID, userID)
	now := time.Now()
	mock.ExpectQuery("FROM portfolio_stress_results").
		WithArgs(fundID, 90).
		WillReturnRows(sqlmock.NewRows([]string{
			"fund_id", "scenario_id", "calculated_at",
			"nav_before", "nav_after", "pnl_total", "pnl_pct",
			"holding_count", "shocked_count", "impacts",
		}).AddRow(fundID, "scen-1", now,
			100000.0, 80000.0, -20000.0, -0.20,
			3, 3, []byte(`[]`)))
	req := authReq(http.MethodGet, "/api/funds/"+fundID+"/risk/stress/history", "", userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleHistory(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Results []stressResultWire `json:"results"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Results) != 1 || body.Results[0].PnLPct != -0.20 {
		t.Errorf("got %+v", body.Results)
	}
}
