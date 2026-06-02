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

func newAdminReconEnv(t *testing.T) (*adminHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	h := &adminHandler{db: db, metrics: newServerMetrics()}
	return h, mock, func() { _ = db.Close() }
}

func authReq(method, target, body string, userID string) *http.Request {
	var br *http.Request
	if body == "" {
		br = httptest.NewRequest(method, target, nil)
	} else {
		br = httptest.NewRequest(method, target, bytes.NewBufferString(body))
		br.Header.Set("Content-Type", "application/json")
	}
	ctx := api.WithAuthenticatedUserID(br.Context(), userID)
	return br.WithContext(ctx)
}

// 401 when no auth.
func TestAdminRecon_ListRuns_Unauthenticated(t *testing.T) {
	h, _, cleanup := newAdminReconEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/reconciliation/runs", nil)
	rr := httptest.NewRecorder()
	h.handleListReconRuns(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

// 403 when user is not admin.
func TestAdminRecon_ListRuns_Forbidden(t *testing.T) {
	h, mock, cleanup := newAdminReconEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "user")
	req := authReq(http.MethodGet, "/api/admin/reconciliation/runs", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListReconRuns(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

// Happy path: returns rows.
func TestAdminRecon_ListRuns_HappyPath(t *testing.T) {
	h, mock, cleanup := newAdminReconEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	mock.ExpectQuery("FROM reconciliation_runs").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "statement_id", "run_date", "triggered_by", "trigger_source", "status",
			"break_count_total", "break_count_critical", "break_count_warning", "break_count_info",
			"summary", "started_at", "completed_at", "error_message",
		}).AddRow("run-1", "fund-1", "stmt-1", time.Now().UTC(),
			"", "scheduled", "completed",
			0, 0, 0, 0,
			"{}", time.Now().UTC(), time.Now().UTC(), ""))

	req := authReq(http.MethodGet, "/api/admin/reconciliation/runs", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListReconRuns(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Runs []reconRunWire `json:"runs"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Runs) != 1 || body.Runs[0].ID != "run-1" {
		t.Errorf("runs = %+v", body.Runs)
	}
}

// Resolve break: invalid status rejected.
func TestAdminRecon_ResolveBreak_InvalidStatus(t *testing.T) {
	h, mock, cleanup := newAdminReconEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")

	req := authReq(http.MethodPost, "/api/admin/reconciliation/breaks/b-1/resolve",
		`{"status":"bogus"}`, "u-1")
	req.SetPathValue("id", "b-1")
	rr := httptest.NewRecorder()
	h.handleResolveReconBreak(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// Resolve break: empty path id rejected.
func TestAdminRecon_ResolveBreak_EmptyID(t *testing.T) {
	h, mock, cleanup := newAdminReconEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodPost, "/api/admin/reconciliation/breaks//resolve",
		`{"status":"resolved"}`, "u-1")
	rr := httptest.NewRecorder()
	h.handleResolveReconBreak(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// TriggerRun: requires fund_id.
func TestAdminRecon_TriggerRun_MissingFundID(t *testing.T) {
	h, mock, cleanup := newAdminReconEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodPost, "/api/admin/reconciliation/runs",
		`{"use_mock_provider": true}`, "u-1")
	rr := httptest.NewRecorder()
	h.handleTriggerReconRun(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "fund_id_required") {
		t.Errorf("body = %s", rr.Body.String())
	}
}

// TriggerRun: provider must be mock until real ingest is wired.
func TestAdminRecon_TriggerRun_RequiresMockProvider(t *testing.T) {
	h, mock, cleanup := newAdminReconEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodPost, "/api/admin/reconciliation/runs",
		`{"fund_id":"fund-1","use_mock_provider": false}`, "u-1")
	rr := httptest.NewRecorder()
	h.handleTriggerReconRun(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "provider_required") {
		t.Errorf("body = %s", rr.Body.String())
	}
}

// TriggerRun: invalid as_of_date rejected.
func TestAdminRecon_TriggerRun_InvalidAsOf(t *testing.T) {
	h, mock, cleanup := newAdminReconEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodPost, "/api/admin/reconciliation/runs",
		`{"fund_id":"fund-1","use_mock_provider":true,"as_of_date":"6/1/2026"}`, "u-1")
	rr := httptest.NewRecorder()
	h.handleTriggerReconRun(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "invalid_as_of") {
		t.Errorf("body = %s", rr.Body.String())
	}
}
