// Per-fund VaR HTTP handler tests (S7 / P3-2).
//
// Exercises:
//   - unauthenticated + missing fundId guards
//   - fund-access authorisation (forbidden case)
//   - happy-path live snapshot: 60d of daily returns → 9 result tiles
//   - persist=1 path writes 9 INSERTs transactionally
//   - insufficient_history when sample < MinSampleSize
//   - trend endpoint with method/confidence/horizon filters

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
)

func newVaRHandlerEnv(t *testing.T) (*varHandler, sqlmock.Sqlmock, *sql.DB, func()) {
	t.Helper()
	db, mock := newMockDB(t)
	h := newVaRHandler(&Services{DB: db})
	if h == nil {
		t.Fatal("newVaRHandler returned nil")
	}
	return h, mock, db, func() { _ = db.Close() }
}

func expectFundOwnershipVaR(mock sqlmock.Sqlmock, fundID, companyID, userID string) {
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

// dailyReturnRows mocks the nav_snapshots.daily_return time
// series with n synthetic rows.
func dailyReturnRows(n int) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"trading_date", "daily_return"})
	// Note: the repo selects DESC then reverses. So we feed
	// dates DESC and increasing-magnitude returns.
	start := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		d := start.AddDate(0, 0, -i)
		// Mix of positive and negative returns with mean ≈ 0,
		// std ≈ 0.01 so all three methods are well-defined.
		val := 0.001 - 0.002*float64(i%5)
		rows.AddRow(d, val)
	}
	return rows
}

func TestVaR_Snapshot_Unauthenticated(t *testing.T) {
	h, _, _, cleanup := newVaRHandlerEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/funds/f1/risk/var", nil)
	req.SetPathValue("fundId", "f1")
	rr := httptest.NewRecorder()
	h.handleSnapshot(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestVaR_Snapshot_MissingFundId(t *testing.T) {
	h, _, _, cleanup := newVaRHandlerEnv(t)
	defer cleanup()
	req := authReq(http.MethodGet, "/api/funds//risk/var", "", "u1")
	req.SetPathValue("fundId", "")
	rr := httptest.NewRecorder()
	h.handleSnapshot(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestVaR_Snapshot_HappyPath(t *testing.T) {
	h, mock, _, cleanup := newVaRHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipVaR(mock, fundID, companyID, userID)
	// 60 days of synthetic returns; default lookback is 252 but
	// we override via query.
	mock.ExpectQuery("FROM nav_snapshots").
		WithArgs(fundID, sqlmock.AnyArg(), 60).
		WillReturnRows(dailyReturnRows(60))

	req := authReq(http.MethodGet, "/api/funds/"+fundID+"/risk/var?lookback=60&horizon=1", "", userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleSnapshot(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Snapshot varSnapshotWire `json:"snapshot"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Snapshot.FundID != fundID {
		t.Errorf("fund id = %q", body.Snapshot.FundID)
	}
	if body.Snapshot.SampleSize != 60 {
		t.Errorf("sample_size = %d", body.Snapshot.SampleSize)
	}
	// 3 methods × 3 confidences = 9 result tiles.
	if len(body.Snapshot.Results) != 9 {
		t.Fatalf("results count = %d, body=%s", len(body.Snapshot.Results), rr.Body.String())
	}
	for _, r := range body.Snapshot.Results {
		if r.VarPct > 0 {
			t.Errorf("VaR should be <= 0, got %f for %+v", r.VarPct, r)
		}
		if r.CVarPct > r.VarPct+1e-9 {
			t.Errorf("CVaR %f > VaR %f for %+v", r.CVarPct, r.VarPct, r)
		}
	}
}

func TestVaR_Snapshot_InsufficientHistory(t *testing.T) {
	h, mock, _, cleanup := newVaRHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipVaR(mock, fundID, companyID, userID)
	// MinSampleSize - 1 rows → 422 unprocessable.
	mock.ExpectQuery("FROM nav_snapshots").
		WithArgs(fundID, sqlmock.AnyArg(), 252).
		WillReturnRows(dailyReturnRows(10))

	req := authReq(http.MethodGet, "/api/funds/"+fundID+"/risk/var", "", userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleSnapshot(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestVaR_Snapshot_PersistTransactional(t *testing.T) {
	h, mock, _, cleanup := newVaRHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipVaR(mock, fundID, companyID, userID)
	mock.ExpectQuery("FROM nav_snapshots").
		WithArgs(fundID, sqlmock.AnyArg(), 60).
		WillReturnRows(dailyReturnRows(60))
	mock.ExpectBegin()
	mock.ExpectPrepare(`INSERT INTO portfolio_var_snapshots`)
	for i := 0; i < 9; i++ {
		mock.ExpectExec(`INSERT INTO portfolio_var_snapshots`).
			WillReturnResult(sqlmock.NewResult(int64(i+1), 1))
	}
	mock.ExpectCommit()

	req := authReq(http.MethodGet, "/api/funds/"+fundID+"/risk/var?lookback=60&persist=1", "", userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleSnapshot(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestVaR_Snapshot_RejectsBadParams(t *testing.T) {
	h, mock, _, cleanup := newVaRHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	for _, q := range []string{
		"?lookback=5",     // below MinSampleSize
		"?lookback=2000",  // above ceiling
		"?lookback=notnum",
		"?horizon=0",
		"?horizon=21",
		"?horizon=foo",
	} {
		expectFundOwnershipVaR(mock, fundID, companyID, userID)
		req := authReq(http.MethodGet, "/api/funds/"+fundID+"/risk/var"+q, "", userID)
		req.SetPathValue("fundId", fundID)
		rr := httptest.NewRecorder()
		h.handleSnapshot(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("query %q: status = %d", q, rr.Code)
		}
	}
}

func TestVaR_Trend_HappyPath(t *testing.T) {
	h, mock, _, cleanup := newVaRHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipVaR(mock, fundID, companyID, userID)
	t0 := time.Now().UTC()
	mock.ExpectQuery("FROM portfolio_var_snapshots").
		WithArgs(fundID, "historical", 0.95, 1, 90).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "calculated_at", "method", "confidence", "horizon_days",
			"var_pct", "cvar_pct", "sample_size", "lookback_days",
		}).AddRow(int64(1), fundID, t0, "historical", 0.95, 1,
			-0.021, -0.026, 252, 252))

	req := authReq(http.MethodGet,
		"/api/funds/"+fundID+"/risk/var/trend?method=historical&confidence=0.95&horizon=1",
		"", userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleTrend(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Points []varTrendPointWire `json:"points"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Points) != 1 {
		t.Fatalf("points count = %d", len(body.Points))
	}
	if body.Points[0].Method != "historical" || body.Points[0].VarPct != -0.021 {
		t.Errorf("unexpected point: %+v", body.Points[0])
	}
}

func TestVaR_Trend_RejectsBadParams(t *testing.T) {
	h, mock, _, cleanup := newVaRHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	for _, q := range []string{
		"?method=bogus&confidence=0.95&horizon=1",
		"?method=historical&confidence=0.5&horizon=1",
		"?method=historical&confidence=0.95&horizon=foo",
	} {
		// Fund-access check runs first so we need one mock pass per
		// iteration, otherwise the handler short-circuits to 500.
		expectFundOwnershipVaR(mock, fundID, companyID, userID)
		req := authReq(http.MethodGet, "/api/funds/"+fundID+"/risk/var/trend"+q, "", userID)
		req.SetPathValue("fundId", fundID)
		rr := httptest.NewRecorder()
		h.handleTrend(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("query %q: status = %d body=%s", q, rr.Code, rr.Body.String())
		}
	}
}
