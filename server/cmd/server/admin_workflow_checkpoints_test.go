package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/repository"
)

type stubCheckpointResumeSink struct {
	calls       int
	lastFundID  string
	lastStep    string
	lastTrading time.Time
	status      *api.WorkflowStatus
	err         error
}

func (s *stubCheckpointResumeSink) ResumeStep(_ context.Context, fundID string, tradingDate time.Time, step string) (*api.WorkflowStatus, error) {
	s.calls++
	s.lastFundID = fundID
	s.lastStep = step
	s.lastTrading = tradingDate
	return s.status, s.err
}

func newAdminWorkflowCheckpointsEnv(t *testing.T) (*adminHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	h := &adminHandler{
		db:                     db,
		metrics:                newServerMetrics(),
		workflowCheckpointRepo: repository.NewWorkflowCheckpointRepo(db),
	}
	return h, mock, func() { _ = db.Close() }
}

func checkpointRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "run_id", "fund_id", "trading_date", "step", "status", "attempts",
		"started_at", "ended_at", "duration_ms", "error_text", "payload",
		"created_at", "updated_at",
	})
}

func TestAdminWorkflowCheckpoints_List_Unauthenticated(t *testing.T) {
	h, _, cleanup := newAdminWorkflowCheckpointsEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/workflow-checkpoints?run_id=r1", nil)
	rr := httptest.NewRecorder()
	h.handleAdminListWorkflowCheckpoints(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminWorkflowCheckpoints_List_NotAdmin(t *testing.T) {
	h, mock, cleanup := newAdminWorkflowCheckpointsEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "user")
	req := authReq(http.MethodGet, "/api/admin/workflow-checkpoints?run_id=r1", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleAdminListWorkflowCheckpoints(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminWorkflowCheckpoints_List_MissingFilter(t *testing.T) {
	h, mock, cleanup := newAdminWorkflowCheckpointsEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodGet, "/api/admin/workflow-checkpoints", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleAdminListWorkflowCheckpoints(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminWorkflowCheckpoints_List_ByRun(t *testing.T) {
	h, mock, cleanup := newAdminWorkflowCheckpointsEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	now := time.Now().UTC()
	mock.ExpectQuery("FROM workflow_checkpoints").
		WithArgs("r1").
		WillReturnRows(checkpointRows().AddRow(
			"cp-1", "r1", "f1", now, "macro_brief", "success", 1,
			now, now, int64(100), nil, json.RawMessage(`{}`),
			now, now,
		))
	req := authReq(http.MethodGet, "/api/admin/workflow-checkpoints?run_id=r1", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleAdminListWorkflowCheckpoints(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Checkpoints []workflowCheckpointWire `json:"checkpoints"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Checkpoints) != 1 || resp.Checkpoints[0].Step != "macro_brief" {
		t.Errorf("unexpected resp: %+v", resp)
	}
}

func TestAdminWorkflowCheckpoints_List_ByFundDate(t *testing.T) {
	h, mock, cleanup := newAdminWorkflowCheckpointsEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	now := time.Now().UTC()
	td, _ := time.Parse("2006-01-02", "2026-06-01")
	mock.ExpectQuery("FROM workflow_checkpoints").
		WithArgs("f1", td).
		WillReturnRows(checkpointRows().AddRow(
			"cp-1", "r1", "f1", td, "pm_plan", "failed", 2,
			now, now, int64(500), "boom", json.RawMessage(`{}`),
			now, now,
		))
	req := authReq(http.MethodGet, "/api/admin/workflow-checkpoints?fund_id=f1&trading_date=2026-06-01", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleAdminListWorkflowCheckpoints(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminWorkflowCheckpoints_Resume_Unauthenticated(t *testing.T) {
	h, _, cleanup := newAdminWorkflowCheckpointsEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/workflow-checkpoints/resume", nil)
	rr := httptest.NewRecorder()
	h.handleAdminResumeWorkflowCheckpoint(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminWorkflowCheckpoints_Resume_NoSink(t *testing.T) {
	h, mock, cleanup := newAdminWorkflowCheckpointsEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodPost, "/api/admin/workflow-checkpoints/resume", `{"run_id":"r1"}`, "u-1")
	rr := httptest.NewRecorder()
	h.handleAdminResumeWorkflowCheckpoint(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminWorkflowCheckpoints_Resume_HappyPath_LatestFailed(t *testing.T) {
	h, mock, cleanup := newAdminWorkflowCheckpointsEnv(t)
	defer cleanup()
	sink := &stubCheckpointResumeSink{
		status: &api.WorkflowStatus{State: "running"},
	}
	h.workflowCheckpointResumeSink = sink
	expectAdminRoleLookup(mock, "u-1", "admin")
	now := time.Now().UTC()
	mock.ExpectQuery("FROM workflow_checkpoints").
		WithArgs("r1").
		WillReturnRows(checkpointRows().AddRow(
			"cp-1", "r1", "f1", now, "pm_plan", "failed", 2,
			now, now, int64(2500), "boom", json.RawMessage(`{}`),
			now, now,
		))
	req := authReq(http.MethodPost, "/api/admin/workflow-checkpoints/resume", `{"run_id":"r1"}`, "u-1")
	rr := httptest.NewRecorder()
	h.handleAdminResumeWorkflowCheckpoint(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if sink.calls != 1 || sink.lastFundID != "f1" || sink.lastStep != "pm_plan" {
		t.Errorf("sink not invoked correctly: %+v", sink)
	}
}

func TestAdminWorkflowCheckpoints_Resume_NoFailed(t *testing.T) {
	h, mock, cleanup := newAdminWorkflowCheckpointsEnv(t)
	defer cleanup()
	h.workflowCheckpointResumeSink = &stubCheckpointResumeSink{}
	expectAdminRoleLookup(mock, "u-1", "admin")
	mock.ExpectQuery("FROM workflow_checkpoints").
		WithArgs("r-clean").
		WillReturnError(sql.ErrNoRows)
	req := authReq(http.MethodPost, "/api/admin/workflow-checkpoints/resume", `{"run_id":"r-clean"}`, "u-1")
	rr := httptest.NewRecorder()
	h.handleAdminResumeWorkflowCheckpoint(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminWorkflowCheckpoints_Resume_ExplicitStep(t *testing.T) {
	h, mock, cleanup := newAdminWorkflowCheckpointsEnv(t)
	defer cleanup()
	sink := &stubCheckpointResumeSink{status: &api.WorkflowStatus{State: "running"}}
	h.workflowCheckpointResumeSink = sink
	expectAdminRoleLookup(mock, "u-1", "admin")
	now := time.Now().UTC()
	mock.ExpectQuery("FROM workflow_checkpoints").
		WithArgs("r1").
		WillReturnRows(checkpointRows().AddRow(
			"cp-1", "r1", "f1", now, "macro_brief", "success", 1,
			now, now, int64(50), nil, json.RawMessage(`{}`),
			now, now,
		).AddRow(
			"cp-2", "r1", "f1", now, "pm_plan", "failed", 2,
			now, now, int64(2500), "boom", json.RawMessage(`{}`),
			now, now,
		))
	req := authReq(http.MethodPost, "/api/admin/workflow-checkpoints/resume", `{"run_id":"r1","step":"macro_brief"}`, "u-1")
	rr := httptest.NewRecorder()
	h.handleAdminResumeWorkflowCheckpoint(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if sink.lastStep != "macro_brief" {
		t.Errorf("expected macro_brief step, got %q", sink.lastStep)
	}
}

func TestAdminWorkflowCheckpoints_Resume_SinkError(t *testing.T) {
	h, mock, cleanup := newAdminWorkflowCheckpointsEnv(t)
	defer cleanup()
	h.workflowCheckpointResumeSink = &stubCheckpointResumeSink{err: errors.New("workflow blew up")}
	expectAdminRoleLookup(mock, "u-1", "admin")
	now := time.Now().UTC()
	mock.ExpectQuery("FROM workflow_checkpoints").
		WithArgs("r1").
		WillReturnRows(checkpointRows().AddRow(
			"cp-1", "r1", "f1", now, "pm_plan", "failed", 1,
			now, now, int64(100), "first failure", json.RawMessage(`{}`),
			now, now,
		))
	req := authReq(http.MethodPost, "/api/admin/workflow-checkpoints/resume", `{"run_id":"r1"}`, "u-1")
	rr := httptest.NewRecorder()
	h.handleAdminResumeWorkflowCheckpoint(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}
