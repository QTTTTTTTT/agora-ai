package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/lockup"
)

func newAdminLockupEnv(t *testing.T) (*adminHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	repo := lockup.NewRepo(db)
	h := &adminHandler{db: db, metrics: newServerMetrics(), lockupRepo: repo}
	return h, mock, func() { _ = db.Close() }
}

func adminLockupRecordRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "fund_id", "instrument_key", "symbol",
		"locked_qty", "locked_until", "lockup_reason",
		"source_lot_id", "note",
		"released_at", "released_reason", "released_by",
		"created_by", "created_at", "updated_at",
	})
}

// ----- auth -----

func TestAdminLockup_List_Unauthenticated(t *testing.T) {
	h, _, cleanup := newAdminLockupEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/lockups", nil)
	rr := httptest.NewRecorder()
	h.handleListLockups(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminLockup_List_Forbidden(t *testing.T) {
	h, mock, cleanup := newAdminLockupEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "user")
	req := authReq(http.MethodGet, "/api/admin/lockups", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListLockups(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d", rr.Code)
	}
}

// ----- list -----

func TestAdminLockup_List_Happy(t *testing.T) {
	h, mock, cleanup := newAdminLockupEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	now := time.Now().UTC()
	until := now.Add(90 * 24 * time.Hour)
	mock.ExpectQuery("FROM position_lockups").
		WillReturnRows(adminLockupRecordRows().AddRow(
			"id1", "f1", "AAPL.US", "AAPL",
			float64(100), until, "ipo",
			nil, "",
			nil, "", nil,
			nil, now, now,
		))
	req := authReq(http.MethodGet, "/api/admin/lockups", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListLockups(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Lockups []lockupWire `json:"lockups"`
		Total   int          `json:"total"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 1 || body.Lockups[0].Symbol != "AAPL" {
		t.Errorf("got %+v", body)
	}
	if body.Lockups[0].Status != "active" {
		t.Errorf("expected status=active, got %s", body.Lockups[0].Status)
	}
}

func TestAdminLockup_List_StatusReleased(t *testing.T) {
	h, mock, cleanup := newAdminLockupEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	now := time.Now().UTC()
	until := now.Add(90 * 24 * time.Hour)
	mock.ExpectQuery("FROM position_lockups").
		WillReturnRows(adminLockupRecordRows().AddRow(
			"id1", "f1", "AAPL.US", "AAPL",
			float64(100), until, "ipo",
			nil, "",
			now, "manual", "u-2",
			nil, now, now,
		))
	req := authReq(http.MethodGet, "/api/admin/lockups?status=released", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListLockups(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Lockups []lockupWire `json:"lockups"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body.Lockups[0].Status != "released" {
		t.Errorf("expected status=released, got %s", body.Lockups[0].Status)
	}
	if body.Lockups[0].ReleasedReason != "manual" {
		t.Errorf("released_reason = %q", body.Lockups[0].ReleasedReason)
	}
}

// ----- get -----

func TestAdminLockup_Get_NotFound(t *testing.T) {
	h, mock, cleanup := newAdminLockupEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	mock.ExpectQuery("FROM position_lockups").
		WithArgs("missing").
		WillReturnRows(adminLockupRecordRows())
	req := authReq(http.MethodGet, "/api/admin/lockups/missing", "", "u-1")
	req.SetPathValue("id", "missing")
	rr := httptest.NewRecorder()
	h.handleGetLockup(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d", rr.Code)
	}
}

// ----- create -----

func TestAdminLockup_Create_Happy(t *testing.T) {
	h, mock, cleanup := newAdminLockupEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	now := time.Now().UTC()
	until := now.Add(90 * 24 * time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO position_lockups")).
		WillReturnRows(adminLockupRecordRows().AddRow(
			"id1", "f1", "AAPL.US", "AAPL",
			float64(100), until, "ipo",
			nil, "",
			nil, "", nil,
			"u-1", now, now,
		))
	body := `{"fund_id":"f1","instrument_key":"AAPL.US","symbol":"AAPL","locked_qty":100,"locked_until":"` +
		until.Format(time.RFC3339) + `","reason":"ipo"}`
	req := authReq(http.MethodPost, "/api/admin/lockups", body, "u-1")
	rr := httptest.NewRecorder()
	h.handleCreateLockup(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if h.metrics.lockupEvents["admin_create"] != 1 {
		t.Errorf("expected admin_create metric")
	}
}

func TestAdminLockup_Create_RejectsBadDate(t *testing.T) {
	h, mock, cleanup := newAdminLockupEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	body := `{"fund_id":"f1","instrument_key":"AAPL.US","symbol":"AAPL","locked_qty":100,"locked_until":"not-rfc3339"}`
	req := authReq(http.MethodPost, "/api/admin/lockups", body, "u-1")
	rr := httptest.NewRecorder()
	h.handleCreateLockup(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

// ----- update -----

func TestAdminLockup_Update_Happy(t *testing.T) {
	h, mock, cleanup := newAdminLockupEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	now := time.Now().UTC()
	until := now.Add(180 * 24 * time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE position_lockups")).
		WillReturnRows(adminLockupRecordRows().AddRow(
			"id1", "f1", "AAPL.US", "AAPL",
			float64(200), until, "ipo",
			nil, "extended",
			nil, "", nil,
			"u-1", now, now,
		))
	body := `{"locked_qty":200}`
	req := authReq(http.MethodPatch, "/api/admin/lockups/id1", body, "u-1")
	req.SetPathValue("id", "id1")
	rr := httptest.NewRecorder()
	h.handleUpdateLockup(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if h.metrics.lockupEvents["admin_update"] != 1 {
		t.Errorf("admin_update metric missing")
	}
}

// ----- release -----

func TestAdminLockup_Release_Happy(t *testing.T) {
	h, mock, cleanup := newAdminLockupEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	now := time.Now().UTC()
	until := now.Add(90 * 24 * time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE position_lockups")).
		WithArgs("id1", "regulatory clearance", "u-1").
		WillReturnRows(adminLockupRecordRows().AddRow(
			"id1", "f1", "AAPL.US", "AAPL",
			float64(100), until, "ipo",
			nil, "",
			now, "regulatory clearance", "u-1",
			nil, now, now,
		))
	body := `{"reason":"regulatory clearance"}`
	req := authReq(http.MethodPost, "/api/admin/lockups/id1/release", body, "u-1")
	req.SetPathValue("id", "id1")
	rr := httptest.NewRecorder()
	h.handleReleaseLockup(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body2 struct {
		Lockup lockupWire `json:"lockup"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&body2)
	if body2.Lockup.Status != "released" {
		t.Errorf("expected status=released, got %s", body2.Lockup.Status)
	}
	if h.metrics.lockupEvents["admin_release"] != 1 {
		t.Errorf("admin_release metric missing")
	}
}

func TestAdminLockup_Release_RequiresReason(t *testing.T) {
	h, mock, cleanup := newAdminLockupEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	body := `{}`
	req := authReq(http.MethodPost, "/api/admin/lockups/id1/release", body, "u-1")
	req.SetPathValue("id", "id1")
	rr := httptest.NewRecorder()
	h.handleReleaseLockup(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rr.Code)
	}
}

// ----- delete -----

func TestAdminLockup_Delete_NotFound(t *testing.T) {
	h, mock, cleanup := newAdminLockupEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM position_lockups")).
		WithArgs("missing").
		WillReturnResult(sqlmock.NewResult(0, 0))
	req := authReq(http.MethodDelete, "/api/admin/lockups/missing", "", "u-1")
	req.SetPathValue("id", "missing")
	rr := httptest.NewRecorder()
	h.handleDeleteLockup(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminLockup_Delete_Happy(t *testing.T) {
	h, mock, cleanup := newAdminLockupEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM position_lockups")).
		WithArgs("id1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	req := authReq(http.MethodDelete, "/api/admin/lockups/id1", "", "u-1")
	req.SetPathValue("id", "id1")
	rr := httptest.NewRecorder()
	h.handleDeleteLockup(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if h.metrics.lockupEvents["admin_delete"] != 1 {
		t.Errorf("admin_delete metric missing")
	}
}
