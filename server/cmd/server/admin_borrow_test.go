package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/securitiesborrow"
)

func newAdminBorrowEnv(t *testing.T) (*adminHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	repo := securitiesborrow.NewRepo(db)
	cache := securitiesborrow.NewCache(securitiesborrow.CacheConfig{})
	h := &adminHandler{db: db, metrics: newServerMetrics(), borrowRepo: repo, borrowCache: cache}
	return h, mock, func() { _ = db.Close() }
}

func borrowRateMockRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "instrument_key", "symbol", "market", "asset_class",
		"borrow_rate_bps_annual", "locate_fee_bps",
		"availability", "available_shares", "min_locate_qty", "max_locate_qty",
		"source", "last_calibrated_at", "note", "updated_by",
		"created_at", "updated_at",
	})
}

func TestAdminBorrow_List_Unauthenticated(t *testing.T) {
	h, _, cleanup := newAdminBorrowEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/borrow/rates", nil)
	rr := httptest.NewRecorder()
	h.handleListBorrowRates(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminBorrow_List_Forbidden(t *testing.T) {
	h, mock, cleanup := newAdminBorrowEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "user")
	req := authReq(http.MethodGet, "/api/admin/borrow/rates", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListBorrowRates(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminBorrow_List_Happy(t *testing.T) {
	h, mock, cleanup := newAdminBorrowEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	now := time.Now().UTC()
	mock.ExpectQuery("FROM security_borrow_rates").
		WillReturnRows(borrowRateMockRows().AddRow(
			"id1", "TSLA.US", "TSLA", "US", "equity",
			float64(3000), float64(25),
			"hard", int64(50000), nil, nil,
			"manual", now, "", nil,
			now, now,
		))
	req := authReq(http.MethodGet, "/api/admin/borrow/rates", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListBorrowRates(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Rates []borrowRateWire `json:"rates"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Rates) != 1 || body.Rates[0].Availability != "hard" {
		t.Errorf("got %+v", body)
	}
}

func TestAdminBorrow_Upsert_Happy(t *testing.T) {
	h, mock, cleanup := newAdminBorrowEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO security_borrow_rates")).
		WillReturnRows(borrowRateMockRows().AddRow(
			"id1", "TSLA.US", "TSLA", "US", "equity",
			float64(3000), float64(25),
			"hard", int64(50000), nil, nil,
			"manual", now, "", nil,
			now, now,
		))
	body := `{"instrument_key":"TSLA.US","symbol":"TSLA","borrow_rate_bps_annual":3000,"locate_fee_bps":25,"availability":"hard","available_shares":50000}`
	req := authReq(http.MethodPost, "/api/admin/borrow/rates", body, "u-1")
	rr := httptest.NewRecorder()
	h.handleUpsertBorrowRate(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if h.metrics.borrowEvents["admin_upsert_rate"] != 1 {
		t.Errorf("admin_upsert_rate metric missing")
	}
	// Cache should reflect the upsert immediately.
	if got := h.borrowCache.Lookup("TSLA.US"); got == nil {
		t.Errorf("expected cache hit after upsert")
	}
}

func TestAdminBorrow_Delete_Happy(t *testing.T) {
	h, mock, cleanup := newAdminBorrowEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM security_borrow_rates")).
		WithArgs("TSLA.US").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Pre-populate cache so we can verify the ApplyChange delete.
	h.borrowCache.SetRows([]securitiesborrow.BorrowRate{
		{InstrumentKey: "TSLA.US", Availability: securitiesborrow.AvailabilityHard},
	})
	req := authReq(http.MethodDelete, "/api/admin/borrow/rates/TSLA.US", "", "u-1")
	req.SetPathValue("key", "TSLA.US")
	rr := httptest.NewRecorder()
	h.handleDeleteBorrowRate(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if h.borrowCache.Lookup("TSLA.US") != nil {
		t.Errorf("expected cache eviction after delete")
	}
}

func TestAdminBorrow_LocatePreview_Happy(t *testing.T) {
	h, mock, cleanup := newAdminBorrowEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	h.borrowCache.SetRows([]securitiesborrow.BorrowRate{
		{InstrumentKey: "TSLA.US", Symbol: "TSLA", Availability: securitiesborrow.AvailabilityHard, BorrowRateBpsAnnual: 3000, LocateFeeBps: 25},
	})
	body := `{"fund_id":"f1","instrument_key":"TSLA.US","requested_qty":1000,"intended_price":200}`
	req := authReq(http.MethodPost, "/api/admin/borrow/locate/preview", body, "u-1")
	rr := httptest.NewRecorder()
	h.handleBorrowLocatePreview(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body2 struct {
		Decision         string  `json:"decision"`
		Allowed          bool    `json:"allowed"`
		LocateFeeAmount  float64 `json:"locate_fee_amount"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&body2)
	if body2.Decision != "allow" || !body2.Allowed {
		t.Errorf("expected allow, got %+v", body2)
	}
	if body2.LocateFeeAmount <= 0 {
		t.Errorf("expected positive fee, got %v", body2.LocateFeeAmount)
	}
}

func TestAdminBorrow_CacheStats(t *testing.T) {
	h, mock, cleanup := newAdminBorrowEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	h.borrowCache.SetRows([]securitiesborrow.BorrowRate{
		{InstrumentKey: "TSLA.US"},
		{InstrumentKey: "AMZN.US"},
	})
	req := authReq(http.MethodGet, "/api/admin/borrow/cache", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleBorrowCacheStats(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Size int `json:"size"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body.Size != 2 {
		t.Errorf("size = %d", body.Size)
	}
}

func TestAdminBorrow_ListLocateEvents(t *testing.T) {
	h, mock, cleanup := newAdminBorrowEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	now := time.Now().UTC()
	mock.ExpectQuery("FROM security_locate_events").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "instrument_key", "symbol",
			"requested_qty", "decision",
			"rate_bps_annual", "locate_fee_bps", "locate_fee_amount",
			"intended_price", "notional",
			"reason", "client_order_id", "created_at",
		}).AddRow(
			"evt-1", "f1", "TSLA.US", "TSLA",
			float64(1000), "allow",
			float64(3000), float64(25), float64(250),
			float64(200), float64(200000),
			"ok", "co-1", now,
		))
	req := authReq(http.MethodGet, "/api/admin/borrow/locate/events?fund_id=f1", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListLocateEvents(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Events []locateEventWire `json:"events"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if len(body.Events) != 1 || body.Events[0].Decision != "allow" {
		t.Errorf("got %+v", body)
	}
}
