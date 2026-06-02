// Admin Brinson composition handler tests (S7 / P3-4).

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/brinson"
)

func newAdminBrinsonEnv(t *testing.T) (*adminHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	h := &adminHandler{
		db:          db,
		metrics:     newServerMetrics(),
		brinsonRepo: brinson.NewRepo(db),
	}
	return h, mock, func() { _ = db.Close() }
}

func brinsonCompositionRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "benchmark_id", "bucket_dimension", "asof", "buckets", "note",
		"created_by", "created_at", "updated_at",
	})
}

func TestAdminBrinson_List_Unauthenticated(t *testing.T) {
	h, _, cleanup := newAdminBrinsonEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/brinson-compositions", nil)
	rr := httptest.NewRecorder()
	h.handleListBrinsonCompositions(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminBrinson_List_Forbidden_NonAdmin(t *testing.T) {
	h, mock, cleanup := newAdminBrinsonEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "user")
	req := authReq(http.MethodGet, "/api/admin/brinson-compositions", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListBrinsonCompositions(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminBrinson_List_Happy(t *testing.T) {
	h, mock, cleanup := newAdminBrinsonEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	now := time.Now()
	asof := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery("FROM brinson_benchmark_compositions").
		WillReturnRows(brinsonCompositionRows().
			AddRow("c1", "spx", "asset_class", asof,
				[]byte(`[{"key":"equity","weight":1.0,"return_pct":0.10}]`),
				"", nil, now, now))
	req := authReq(http.MethodGet, "/api/admin/brinson-compositions", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListBrinsonCompositions(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Compositions []brinsonCompositionWire `json:"compositions"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Compositions) != 1 || body.Compositions[0].BenchmarkID != "spx" {
		t.Errorf("got %+v", body.Compositions)
	}
}

func TestAdminBrinson_List_RejectsBadDimension(t *testing.T) {
	h, mock, cleanup := newAdminBrinsonEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodGet, "/api/admin/brinson-compositions?dimension=bogus", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListBrinsonCompositions(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminBrinson_Upsert_Happy(t *testing.T) {
	h, mock, cleanup := newAdminBrinsonEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	now := time.Now()
	asof := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery("INSERT INTO brinson_benchmark_compositions").
		WillReturnRows(brinsonCompositionRows().
			AddRow("c1", "spx", "asset_class", asof,
				[]byte(`[{"key":"equity","weight":1.0,"return_pct":0.10}]`),
				"", nil, now, now))
	body, _ := json.Marshal(map[string]any{
		"benchmark_id": "spx",
		"dimension":    "asset_class",
		"asof":         "2026-05-31",
		"buckets": []map[string]any{
			{"key": "equity", "weight": 1.0, "return_pct": 0.10},
		},
	})
	req := authReq(http.MethodPost, "/api/admin/brinson-compositions", string(body), "u-1")
	rr := httptest.NewRecorder()
	h.handleUpsertBrinsonComposition(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminBrinson_Upsert_RejectsBadAsof(t *testing.T) {
	h, mock, cleanup := newAdminBrinsonEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	body, _ := json.Marshal(map[string]any{
		"benchmark_id": "spx",
		"dimension":    "asset_class",
		"asof":         "not-a-date",
		"buckets": []map[string]any{
			{"key": "equity", "weight": 1.0, "return_pct": 0.10},
		},
	})
	req := authReq(http.MethodPost, "/api/admin/brinson-compositions", string(body), "u-1")
	rr := httptest.NewRecorder()
	h.handleUpsertBrinsonComposition(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminBrinson_Upsert_RejectsBadBuckets(t *testing.T) {
	h, mock, cleanup := newAdminBrinsonEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	body, _ := json.Marshal(map[string]any{
		"benchmark_id": "spx",
		"dimension":    "asset_class",
		"asof":         "2026-05-31",
		"buckets": []map[string]any{
			{"key": "equity", "weight": 0.5, "return_pct": 0.10},
		},
	})
	req := authReq(http.MethodPost, "/api/admin/brinson-compositions", string(body), "u-1")
	rr := httptest.NewRecorder()
	h.handleUpsertBrinsonComposition(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminBrinson_Delete_Happy(t *testing.T) {
	h, mock, cleanup := newAdminBrinsonEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	mock.ExpectExec("DELETE FROM brinson_benchmark_compositions").
		WillReturnResult(sqlmock.NewResult(0, 1))
	req := authReq(http.MethodDelete, "/api/admin/brinson-compositions/c1", "", "u-1")
	req.SetPathValue("id", "c1")
	rr := httptest.NewRecorder()
	h.handleDeleteBrinsonComposition(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminBrinson_Delete_NotFound(t *testing.T) {
	h, mock, cleanup := newAdminBrinsonEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	mock.ExpectExec("DELETE FROM brinson_benchmark_compositions").
		WillReturnResult(sqlmock.NewResult(0, 0))
	req := authReq(http.MethodDelete, "/api/admin/brinson-compositions/missing", "", "u-1")
	req.SetPathValue("id", "missing")
	rr := httptest.NewRecorder()
	h.handleDeleteBrinsonComposition(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d", rr.Code)
	}
}
