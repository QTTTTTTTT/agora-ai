package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fundai/server/internal/api"
)

func newAdminSurveillanceEnv(t *testing.T) (*adminHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	h := &adminHandler{db: db, metrics: newServerMetrics()}
	return h, mock, func() { _ = db.Close() }
}

// 401 path.
func TestAdminSurveillance_ListEvents_Unauthenticated(t *testing.T) {
	h, _, cleanup := newAdminSurveillanceEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/surveillance/events", nil)
	rr := httptest.NewRecorder()
	h.handleListSurveillanceEvents(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

// 403 path.
func TestAdminSurveillance_ListEvents_Forbidden(t *testing.T) {
	h, mock, cleanup := newAdminSurveillanceEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "user")
	req := authReq(http.MethodGet, "/api/admin/surveillance/events", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListSurveillanceEvents(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
}

// Happy path — returns rows.
func TestAdminSurveillance_ListEvents_HappyPath(t *testing.T) {
	h, mock, cleanup := newAdminSurveillanceEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	mock.ExpectQuery("FROM surveillance_events").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "rule_code", "severity",
			"symbol", "instrument_key", "window_start", "window_end",
			"trade_ids", "summary", "metadata",
			"status", "review_note", "reviewed_by", "reviewed_at",
			"detected_at", "detector_version", "fingerprint",
		}).AddRow("ev-1", "fund-1", "wash_trade", "warning",
			"AAPL", "", time.Now().UTC(), time.Now().UTC(),
			`["a","b","c"]`, "summary", "{}",
			"open", "", "", nil,
			time.Now().UTC(), "v1", "fp-1"))

	req := authReq(http.MethodGet, "/api/admin/surveillance/events", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListSurveillanceEvents(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Events []surveillanceEventWire `json:"events"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Events) != 1 || body.Events[0].ID != "ev-1" || body.Events[0].RuleCode != "wash_trade" {
		t.Errorf("events = %+v", body.Events)
	}
}

// Review event — invalid status rejected.
func TestAdminSurveillance_ReviewEvent_InvalidStatus(t *testing.T) {
	h, mock, cleanup := newAdminSurveillanceEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodPost, "/api/admin/surveillance/events/ev-1/review",
		`{"status":"bogus"}`, "u-1")
	req.SetPathValue("id", "ev-1")
	rr := httptest.NewRecorder()
	h.handleReviewSurveillanceEvent(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 body=%s", rr.Code, rr.Body.String())
	}
}

// Review event — missing id.
func TestAdminSurveillance_ReviewEvent_EmptyID(t *testing.T) {
	h, mock, cleanup := newAdminSurveillanceEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	body := bytes.NewBufferString(`{"status":"cleared"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/surveillance/events//review", body)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), "u-1"))
	rr := httptest.NewRecorder()
	h.handleReviewSurveillanceEvent(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 body=%s", rr.Code, rr.Body.String())
	}
}

// Trigger scan — fund_id required.
func TestAdminSurveillance_TriggerScan_MissingFundID(t *testing.T) {
	h, mock, cleanup := newAdminSurveillanceEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodPost, "/api/admin/surveillance/scan", `{}`, "u-1")
	rr := httptest.NewRecorder()
	h.handleTriggerSurveillanceScan(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "fund_id_required") {
		t.Errorf("body = %s", rr.Body.String())
	}
}

// Trigger scan — invalid as_of_date.
func TestAdminSurveillance_TriggerScan_InvalidAsOf(t *testing.T) {
	h, mock, cleanup := newAdminSurveillanceEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodPost, "/api/admin/surveillance/scan",
		`{"fund_id":"fund-1","as_of_date":"6/1/2026"}`, "u-1")
	rr := httptest.NewRecorder()
	h.handleTriggerSurveillanceScan(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "invalid_as_of") {
		t.Errorf("body = %s", rr.Body.String())
	}
}

// Trigger scan — invalid session close.
func TestAdminSurveillance_TriggerScan_InvalidSessionClose(t *testing.T) {
	h, mock, cleanup := newAdminSurveillanceEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodPost, "/api/admin/surveillance/scan",
		`{"fund_id":"fund-1","session_close_utc":"24:99"}`, "u-1")
	rr := httptest.NewRecorder()
	h.handleTriggerSurveillanceScan(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "invalid_session_close") {
		t.Errorf("body = %s", rr.Body.String())
	}
}
