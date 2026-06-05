package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/repository"
	"github.com/google/uuid"
)

// newFundWorkflowCheckpointsTestHandler builds a minimal handler
// over a sqlmock so we can exercise the auth + query paths
// without standing up real Postgres. The repo wiring is the same
// shape main.go uses — only the audit logger is omitted because
// the read endpoint never writes to it.
func newFundWorkflowCheckpointsTestHandler(t *testing.T) (*fundWorkflowCheckpointsHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	h := &fundWorkflowCheckpointsHandler{
		workflowCheckpointRepo: repository.NewWorkflowCheckpointRepo(db),
		fundRepo:               repository.NewFundRepo(db),
		companyRepo:            repository.NewFundCompanyRepo(db),
	}
	return h, mock, func() { _ = db.Close() }
}

// authFundReq builds an authenticated GET request with the
// /api/funds/{fundId}/workflow-checkpoints PathValue populated,
// since httptest.NewRequest doesn't run the mux pattern matcher
// that would normally fill it.
func authFundReq(method, target, userID, fundID string) *http.Request {
	r := authReq(method, target, "", userID)
	r.SetPathValue("fundId", fundID)
	return r
}

func TestFundWorkflowCheckpoints_List_Unauthenticated(t *testing.T) {
	h, _, cleanup := newFundWorkflowCheckpointsTestHandler(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/funds/f1/workflow-checkpoints?trading_date=2026-06-05", nil)
	req.SetPathValue("fundId", "f1")
	rr := httptest.NewRecorder()
	h.handleList(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestFundWorkflowCheckpoints_List_NotOwner_Forbidden(t *testing.T) {
	h, mock, cleanup := newFundWorkflowCheckpointsTestHandler(t)
	defer cleanup()
	fundID := uuid.New().String()
	companyID := uuid.New().String()
	ownerID := uuid.New().String()
	intruderID := uuid.New().String()

	// Same expectFundAuth helper used by fund_llm_overrides tests:
	// fund row points to companyID, company row says owner_user_id
	// = ownerID. We then issue the request as intruderID, which
	// should hit api.ErrForbidden.
	expectFundAuth(t, mock, fundID, companyID, ownerID)

	req := authFundReq(http.MethodGet, "/api/funds/"+fundID+"/workflow-checkpoints?trading_date=2026-06-05", intruderID, fundID)
	rr := httptest.NewRecorder()
	h.handleList(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestFundWorkflowCheckpoints_List_MissingTradingDate(t *testing.T) {
	h, mock, cleanup := newFundWorkflowCheckpointsTestHandler(t)
	defer cleanup()
	fundID := uuid.New().String()
	companyID := uuid.New().String()
	userID := uuid.New().String()
	expectFundAuth(t, mock, fundID, companyID, userID)

	req := authFundReq(http.MethodGet, "/api/funds/"+fundID+"/workflow-checkpoints", userID, fundID)
	rr := httptest.NewRecorder()
	h.handleList(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestFundWorkflowCheckpoints_List_InvalidTradingDate(t *testing.T) {
	h, mock, cleanup := newFundWorkflowCheckpointsTestHandler(t)
	defer cleanup()
	fundID := uuid.New().String()
	companyID := uuid.New().String()
	userID := uuid.New().String()
	expectFundAuth(t, mock, fundID, companyID, userID)

	req := authFundReq(http.MethodGet, "/api/funds/"+fundID+"/workflow-checkpoints?trading_date=not-a-date", userID, fundID)
	rr := httptest.NewRecorder()
	h.handleList(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestFundWorkflowCheckpoints_List_OK_ReturnsRows(t *testing.T) {
	h, mock, cleanup := newFundWorkflowCheckpointsTestHandler(t)
	defer cleanup()
	fundID := uuid.New().String()
	companyID := uuid.New().String()
	userID := uuid.New().String()
	expectFundAuth(t, mock, fundID, companyID, userID)

	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta("FROM workflow_checkpoints")).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "run_id", "fund_id", "trading_date", "step", "status", "attempts",
			"started_at", "ended_at", "duration_ms", "error_text", "payload",
			"created_at", "updated_at",
		}).AddRow(
			"cp-1", "r-1", fundID, now, "macro_brief", "success", 1,
			now, now, int64(123), nil, json.RawMessage(`{}`),
			now, now,
		))

	req := authFundReq(http.MethodGet, "/api/funds/"+fundID+"/workflow-checkpoints?trading_date=2026-06-05", userID, fundID)
	rr := httptest.NewRecorder()
	h.handleList(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Checkpoints []map[string]any `json:"checkpoints"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Checkpoints) != 1 || resp.Checkpoints[0]["step"] != "macro_brief" {
		t.Fatalf("unexpected body: %s", rr.Body.String())
	}
}
