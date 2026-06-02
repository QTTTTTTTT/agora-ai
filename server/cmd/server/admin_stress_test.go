// Admin stress-scenarios handler tests (S7 / P3-3).

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/stress"
)

func newAdminStressEnv(t *testing.T) (*adminHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	h := &adminHandler{
		db:         db,
		metrics:    newServerMetrics(),
		stressRepo: stress.NewRepo(db),
	}
	return h, mock, func() { _ = db.Close() }
}

func stressScenarioRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "name", "category", "description", "shocks", "created_by", "created_at", "updated_at",
	})
}

func TestAdminStress_List_Unauthenticated(t *testing.T) {
	h, _, cleanup := newAdminStressEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/stress-scenarios", nil)
	rr := httptest.NewRecorder()
	h.handleListStressScenarios(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminStress_List_Forbidden_NonAdmin(t *testing.T) {
	h, mock, cleanup := newAdminStressEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "user")
	req := authReq(http.MethodGet, "/api/admin/stress-scenarios", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListStressScenarios(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminStress_List_Happy(t *testing.T) {
	h, mock, cleanup := newAdminStressEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	now := time.Now()
	mock.ExpectQuery("FROM stress_scenarios").
		WillReturnRows(stressScenarioRows().
			AddRow("s1", "Lehman", "historical", "desc",
				[]byte(`[{"target_type":"wildcard","target_key":"*","value":-0.4}]`),
				nil, now, now))
	req := authReq(http.MethodGet, "/api/admin/stress-scenarios", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListStressScenarios(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Scenarios []stressScenarioWire `json:"scenarios"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Scenarios) != 1 || body.Scenarios[0].Name != "Lehman" {
		t.Errorf("got %+v", body.Scenarios)
	}
}

func TestAdminStress_List_RejectsBadCategory(t *testing.T) {
	h, mock, cleanup := newAdminStressEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodGet, "/api/admin/stress-scenarios?category=bogus", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListStressScenarios(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminStress_Upsert_Happy(t *testing.T) {
	h, mock, cleanup := newAdminStressEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	now := time.Now()
	mock.ExpectQuery("INSERT INTO stress_scenarios").
		WillReturnRows(stressScenarioRows().
			AddRow("s1", "Lehman", "historical", "",
				[]byte(`[{"target_type":"wildcard","target_key":"*","value":-0.4}]`),
				nil, now, now))
	body, _ := json.Marshal(map[string]any{
		"name":     "Lehman",
		"category": "historical",
		"shocks": []map[string]any{
			{"target_type": "wildcard", "target_key": "*", "value": -0.4},
		},
	})
	req := authReq(http.MethodPost, "/api/admin/stress-scenarios", string(body), "u-1")
	rr := httptest.NewRecorder()
	h.handleUpsertStressScenario(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminStress_Upsert_RejectsInvalidShock(t *testing.T) {
	h, mock, cleanup := newAdminStressEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	body, _ := json.Marshal(map[string]any{
		"name":     "Bad",
		"category": "historical",
		"shocks": []map[string]any{
			{"target_type": "bogus", "target_key": "x", "value": -0.4},
		},
	})
	req := authReq(http.MethodPost, "/api/admin/stress-scenarios", string(body), "u-1")
	rr := httptest.NewRecorder()
	h.handleUpsertStressScenario(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminStress_Delete_Happy(t *testing.T) {
	h, mock, cleanup := newAdminStressEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	mock.ExpectExec("DELETE FROM stress_scenarios").
		WillReturnResult(sqlmock.NewResult(0, 1))
	req := authReq(http.MethodDelete, "/api/admin/stress-scenarios/s1", "", "u-1")
	req.SetPathValue("id", "s1")
	rr := httptest.NewRecorder()
	h.handleDeleteStressScenario(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminStress_Delete_NotFound(t *testing.T) {
	h, mock, cleanup := newAdminStressEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	mock.ExpectExec("DELETE FROM stress_scenarios").
		WillReturnResult(sqlmock.NewResult(0, 0))
	req := authReq(http.MethodDelete, "/api/admin/stress-scenarios/missing", "", "u-1")
	req.SetPathValue("id", "missing")
	rr := httptest.NewRecorder()
	h.handleDeleteStressScenario(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d", rr.Code)
	}
}
