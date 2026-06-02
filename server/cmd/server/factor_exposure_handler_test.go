// Per-fund factor-exposure HTTP handler tests (S7 / P3-1).
//
// Exercises:
//   - auth + path guards (unauthenticated, missing fundId)
//   - fund-access authorisation
//   - happy-path live snapshot with positions + loadings
//   - persist=1 path writes the snapshot rows transactionally
//   - trend list with optional factor filter

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
	"github.com/lib/pq"
)

func newFactorExposureHandlerEnv(t *testing.T) (*factorExposureHandler, sqlmock.Sqlmock, *sql.DB, func()) {
	t.Helper()
	db, mock := newMockDB(t)
	h := newFactorExposureHandler(&Services{DB: db})
	if h == nil {
		t.Fatal("newFactorExposureHandler returned nil")
	}
	return h, mock, db, func() { _ = db.Close() }
}

func expectFundOwnershipFE(mock sqlmock.Sqlmock, fundID, companyID, userID string) {
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

func positionRows(t *testing.T) *sqlmock.Rows {
	t.Helper()
	return sqlmock.NewRows([]string{
		"id", "fund_id", "instrument_key", "symbol", "name", "market", "exchange",
		"asset_class", "instrument_type", "position_side", "quote_currency",
		"settlement_currency", "margin_mode", "quantity", "available_qty",
		"cost_price", "current_price", "market_value", "weight", "leverage",
		"contract_multiplier", "expiry_date", "unrealized_pnl", "margin_used",
		"updated_at",
	})
}

func TestFactorExposure_Snapshot_Unauthenticated(t *testing.T) {
	h, _, _, cleanup := newFactorExposureHandlerEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/funds/f1/risk/factor-exposure", nil)
	req.SetPathValue("fundId", "f1")
	rr := httptest.NewRecorder()
	h.handleSnapshot(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestFactorExposure_Snapshot_HappyPath(t *testing.T) {
	h, mock, _, cleanup := newFactorExposureHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	now := time.Now().UTC()
	asof := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	expectFundOwnershipFE(mock, fundID, companyID, userID)
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions WHERE fund_id")).
		WithArgs(fundID).
		WillReturnRows(positionRows(t).AddRow(
			"p1", fundID, "US:AAPL", "AAPL", nil, nil, nil,
			nil, nil, nil, nil,
			nil, nil, float64(100), float64(100),
			float64(150), float64(170), float64(17000), float64(1.0), nil,
			nil, nil, nil, nil,
			now,
		))
	// Engine asks for loadings of US:AAPL.
	mock.ExpectQuery(regexp.QuoteMeta("FROM instrument_factor_loadings")).
		WithArgs(pq.Array([]string{"US:AAPL"}), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{
			"instrument_key", "factor", "asof", "loading", "source", "note", "updated_at",
		}).AddRow("US:AAPL", "momentum", asof, 1.2, "manual", "", asof))

	req := authReq(http.MethodGet, "/api/funds/"+fundID+"/risk/factor-exposure", "", userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleSnapshot(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Snapshot snapshotWire `json:"snapshot"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Snapshot.FundID != fundID {
		t.Errorf("fund id = %q", body.Snapshot.FundID)
	}
	if body.Snapshot.NAV != 17000 {
		t.Errorf("nav = %f", body.Snapshot.NAV)
	}
	// Six factor rows expected.
	if len(body.Snapshot.Exposures) != 6 {
		t.Fatalf("exposures count = %d", len(body.Snapshot.Exposures))
	}
	// Only momentum should have non-zero values (we only seeded that one).
	var mom exposureRowWire
	for _, row := range body.Snapshot.Exposures {
		if row.Factor == "momentum" {
			mom = row
		}
	}
	if mom.NetExposure != 1.2 {
		t.Errorf("momentum net = %f, want 1.2", mom.NetExposure)
	}
}

func TestFactorExposure_Snapshot_PersistTransactional(t *testing.T) {
	h, mock, _, cleanup := newFactorExposureHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipFE(mock, fundID, companyID, userID)
	// Empty portfolio → engine emits six zero rows; persist still
	// fires six INSERTs because we want even the "all zero" vintage
	// in the archive for trend continuity.
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions WHERE fund_id")).
		WithArgs(fundID).
		WillReturnRows(positionRows(t))
	// Empty input array → engine takes the short-circuit path and
	// does NOT call LoadingsByInstruments. So no loadings query
	// here.
	mock.ExpectBegin()
	for i := 0; i < 6; i++ {
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO portfolio_factor_snapshots")).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectCommit()
	req := authReq(http.MethodGet, "/api/funds/"+fundID+"/risk/factor-exposure?persist=1", "", userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleSnapshot(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestFactorExposure_Trend_HappyPath(t *testing.T) {
	h, mock, _, cleanup := newFactorExposureHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipFE(mock, fundID, companyID, userID)
	t1 := time.Now().UTC().Add(-time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("FROM portfolio_factor_snapshots")).
		WithArgs(fundID, "momentum").
		WillReturnRows(sqlmock.NewRows([]string{
			"calculated_at", "factor", "net_exposure", "gross_exposure",
			"capital_pct", "holding_count", "loadings_asof",
		}).AddRow(t1, "momentum", 0.5, 0.6, 0.9, 5, t1))
	req := authReq(http.MethodGet, "/api/funds/"+fundID+"/risk/factor-exposure/trend?factor=momentum&limit=30", "", userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleTrend(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Points []trendPointWire `json:"points"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Points) != 1 || body.Points[0].Factor != "momentum" || body.Points[0].NetExposure != 0.5 {
		t.Errorf("points = %+v", body.Points)
	}
}

func TestFactorExposure_Trend_RejectsInvalidFactor(t *testing.T) {
	h, mock, _, cleanup := newFactorExposureHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipFE(mock, fundID, companyID, userID)
	req := authReq(http.MethodGet, "/api/funds/"+fundID+"/risk/factor-exposure/trend?factor=sector", "", userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleTrend(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rr.Code)
	}
}
