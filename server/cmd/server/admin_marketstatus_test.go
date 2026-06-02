package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newAdminMarketStatusEnv(t *testing.T) (*adminHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	h := &adminHandler{db: db, metrics: newServerMetrics()}
	return h, mock, func() { _ = db.Close() }
}

// Auth gates.
func TestAdminMarketStatus_List_Unauthenticated(t *testing.T) {
	h, _, cleanup := newAdminMarketStatusEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/marketstatus/instruments", nil)
	rr := httptest.NewRecorder()
	h.handleListMarketStatusInstruments(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestAdminMarketStatus_List_Forbidden(t *testing.T) {
	h, mock, cleanup := newAdminMarketStatusEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "user")
	req := authReq(http.MethodGet, "/api/admin/marketstatus/instruments", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListMarketStatusInstruments(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
}

func TestAdminMarketStatus_List_Happy(t *testing.T) {
	h, mock, cleanup := newAdminMarketStatusEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	mock.ExpectQuery("FROM instrument_market_status").
		WillReturnRows(sqlmock.NewRows([]string{
			"instrument_key", "symbol", "market", "status", "halt_reason",
			"halt_started_at", "halt_until", "lower_limit", "upper_limit",
			"last_quote_at", "last_quote_price",
			"asset_class", "staleness_budget_seconds", "note", "updated_at",
		}).AddRow("AAPL.US", "AAPL", "US", "trading", "",
			nil, nil, nil, nil, nil, nil, "equity", nil, "", now))
	req := authReq(http.MethodGet, "/api/admin/marketstatus/instruments", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListMarketStatusInstruments(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Instruments []instrumentStatusWire `json:"instruments"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Instruments) != 1 || body.Instruments[0].Symbol != "AAPL" {
		t.Errorf("got %+v", body.Instruments)
	}
}

// Upsert: bad status.
func TestAdminMarketStatus_Upsert_BadStatus(t *testing.T) {
	h, mock, cleanup := newAdminMarketStatusEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodPut, "/api/admin/marketstatus/instruments/AAPL.US",
		`{"symbol":"AAPL","market":"US","status":"explode"}`, "u-1")
	req.SetPathValue("key", "AAPL.US")
	rr := httptest.NewRecorder()
	h.handleUpsertMarketStatusInstrument(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

// Upsert: lower>upper.
func TestAdminMarketStatus_Upsert_LowerGtUpper(t *testing.T) {
	h, mock, cleanup := newAdminMarketStatusEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodPut, "/api/admin/marketstatus/instruments/AAPL.US",
		`{"symbol":"AAPL","market":"US","status":"trading","lower_limit":300,"upper_limit":100}`, "u-1")
	req.SetPathValue("key", "AAPL.US")
	rr := httptest.NewRecorder()
	h.handleUpsertMarketStatusInstrument(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "lower_limit") {
		t.Errorf("body = %s", rr.Body.String())
	}
}

// Halt: missing key.
func TestAdminMarketStatus_Halt_MissingKey(t *testing.T) {
	h, mock, cleanup := newAdminMarketStatusEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodPost, "/api/admin/marketstatus/instruments//halt",
		`{"reason":"news"}`, "u-1")
	rr := httptest.NewRecorder()
	h.handleHaltMarketStatusInstrument(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// Calendar: missing market.
func TestAdminMarketStatus_Calendar_MissingMarket(t *testing.T) {
	h, mock, cleanup := newAdminMarketStatusEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodGet, "/api/admin/marketstatus/calendar", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListMarketStatusCalendar(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// Calendar upsert: bad date.
func TestAdminMarketStatus_CalendarUpsert_BadDate(t *testing.T) {
	h, mock, cleanup := newAdminMarketStatusEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodPut, "/api/admin/marketstatus/calendar/HK/notadate",
		`{"is_open":true}`, "u-1")
	req.SetPathValue("market", "HK")
	req.SetPathValue("date", "notadate")
	rr := httptest.NewRecorder()
	h.handleUpsertMarketStatusCalendar(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestParseTimestampPtr(t *testing.T) {
	if got, ok := parseTimestampPtr(""); ok || got != nil {
		t.Errorf("empty must yield (nil,false); got=(%v,%v)", got, ok)
	}
	if got, ok := parseTimestampPtr("2026-06-01T14:30:00Z"); !ok || got == nil {
		t.Errorf("rfc3339 must yield ok; got=(%v,%v)", got, ok)
	}
	if _, ok := parseTimestampPtr("nope"); ok {
		t.Error("garbage must yield false")
	}
}

func TestParseDateOnly(t *testing.T) {
	if _, ok := parseDateOnly(""); ok {
		t.Error("empty must yield false")
	}
	if _, ok := parseDateOnly("2026-06-01"); !ok {
		t.Error("happy must parse")
	}
	if _, ok := parseDateOnly("06/01/2026"); ok {
		t.Error("non-iso must fail")
	}
}
