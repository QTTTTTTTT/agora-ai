package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/api"
)

// stubSchedulerInspector implements schedulerInspector for handler
// tests. We don't need leader-checker semantics here — only that
// Snapshot returns whatever the fixture configures.
type stubSchedulerInspector struct {
	snap FundSchedulerSnapshot
}

func (s *stubSchedulerInspector) Snapshot() FundSchedulerSnapshot {
	return s.snap
}

// stubWorkflowAdminTrigger implements workflowAdminTrigger so the
// trigger handler can be exercised without spinning up a real
// workflowServiceAdapter.
type stubWorkflowAdminTrigger struct {
	calls  []stubTriggerCall
	result *adminTriggerResult
	err    error
}

type stubTriggerCall struct {
	FundID      string
	TradingDate time.Time
}

func (s *stubWorkflowAdminTrigger) AdminTriggerFund(_ contextLike, fundID string, tradingDate time.Time) (*adminTriggerResult, error) {
	s.calls = append(s.calls, stubTriggerCall{FundID: fundID, TradingDate: tradingDate})
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

// adminSuperRequest builds a request authenticated as a super admin —
// the gate the scheduler endpoints share with the rest of the admin
// surface.
func adminSuperRequest(method, target, body string) *http.Request {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	ctx := api.WithAuthenticatedUserID(req.Context(), "admin-1")
	ctx = api.WithAuthenticatedUserRole(ctx, adminRoleSuperAdmin)
	return req.WithContext(ctx)
}

// adminUserRequest builds a request authenticated as a regular user —
// the negative case for the super-admin gate.
func adminUserRequest(method, target, body string) *http.Request {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	ctx := api.WithAuthenticatedUserID(req.Context(), "user-1")
	ctx = api.WithAuthenticatedUserRole(ctx, userRoleUser)
	return req.WithContext(ctx)
}

// TestSchedulerSnapshotRequiresSuperAdmin guards the auth gate — the
// snapshot exposes fund IDs and configured trigger times, so a plain
// authenticated user must not be able to read it.
func TestSchedulerSnapshotRequiresSuperAdmin(t *testing.T) {
	h := &adminHandler{scheduler: &stubSchedulerInspector{}}
	rr := httptest.NewRecorder()
	h.handleSchedulerSnapshot(rr, adminUserRequest(http.MethodGet, "/api/admin/workflow/scheduler", ""))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-admin, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestSchedulerSnapshot503WhenUnwired ensures the handler degrades to
// a 503 when the scheduler isn't installed (tests, single-binary CI),
// instead of nil-panicking. Important for the admin UI to show a
// useful empty state.
func TestSchedulerSnapshot503WhenUnwired(t *testing.T) {
	h := &adminHandler{} // no scheduler
	rr := httptest.NewRecorder()
	h.handleSchedulerSnapshot(rr, adminSuperRequest(http.MethodGet, "/api/admin/workflow/scheduler", ""))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestSchedulerSnapshotReturnsPayload verifies the happy path — the
// admin can read back the latest tick, including per-fund decisions.
func TestSchedulerSnapshotReturnsPayload(t *testing.T) {
	now := time.Date(2026, 5, 19, 9, 30, 0, 0, time.UTC)
	inspector := &stubSchedulerInspector{snap: FundSchedulerSnapshot{
		LastPollAt:     now,
		IsLeader:       true,
		TotalActive:    2,
		TriggeredCount: 1,
		Funds: []FundSchedulerStatus{
			{FundID: "fund-A", FundName: "Alpha", Started: true, LastStatus: "running"},
			{FundID: "fund-B", FundName: "Beta", Due: false, SkipReason: "not-yet-due"},
		},
	}}
	h := &adminHandler{scheduler: inspector}

	rr := httptest.NewRecorder()
	h.handleSchedulerSnapshot(rr, adminSuperRequest(http.MethodGet, "/api/admin/workflow/scheduler", ""))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var out FundSchedulerSnapshot
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.TotalActive != 2 || out.TriggeredCount != 1 {
		t.Fatalf("unexpected counts: total=%d triggered=%d", out.TotalActive, out.TriggeredCount)
	}
	if len(out.Funds) != 2 {
		t.Fatalf("expected 2 fund rows, got %d", len(out.Funds))
	}
	if out.Funds[0].FundID != "fund-A" || !out.Funds[0].Started {
		t.Fatalf("expected fund-A to be Started=true, got %+v", out.Funds[0])
	}
	if out.Funds[1].SkipReason != "not-yet-due" {
		t.Fatalf("expected fund-B skip-reason=not-yet-due, got %q", out.Funds[1].SkipReason)
	}
}

// TestSchedulerTriggerRequiresSuperAdmin is the parallel guard for
// the manual-trigger endpoint — even more important because it's a
// write operation that bypasses the calendar schedule.
func TestSchedulerTriggerRequiresSuperAdmin(t *testing.T) {
	stub := &stubWorkflowAdminTrigger{}
	h := &adminHandler{workflowService: stub}
	rr := httptest.NewRecorder()
	req := adminUserRequest(http.MethodPost, "/api/admin/workflow/scheduler/trigger/fund-A", "")
	req.SetPathValue("fundId", "fund-A")
	h.handleSchedulerTrigger(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(stub.calls) != 0 {
		t.Fatalf("expected zero trigger calls, got %d", len(stub.calls))
	}
}

// TestSchedulerTrigger503WhenUnwired — same degradation contract as
// the snapshot endpoint.
func TestSchedulerTrigger503WhenUnwired(t *testing.T) {
	h := &adminHandler{} // no workflow service
	rr := httptest.NewRecorder()
	req := adminSuperRequest(http.MethodPost, "/api/admin/workflow/scheduler/trigger/fund-A", "")
	req.SetPathValue("fundId", "fund-A")
	h.handleSchedulerTrigger(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestSchedulerTriggerBadFundID rejects empty fund IDs early with a
// 400 — protects the downstream service from a meaningless call.
func TestSchedulerTriggerBadFundID(t *testing.T) {
	stub := &stubWorkflowAdminTrigger{}
	h := &adminHandler{workflowService: stub}
	rr := httptest.NewRecorder()
	req := adminSuperRequest(http.MethodPost, "/api/admin/workflow/scheduler/trigger/", "")
	// Don't SetPathValue("fundId") to simulate an empty match.
	h.handleSchedulerTrigger(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestSchedulerTriggerInvalidTradingDate rejects malformed dates
// before reaching the workflow service. The parsing contract is
// "YYYY-MM-DD only"; anything else returns 400.
func TestSchedulerTriggerInvalidTradingDate(t *testing.T) {
	stub := &stubWorkflowAdminTrigger{}
	h := &adminHandler{workflowService: stub}
	rr := httptest.NewRecorder()
	req := adminSuperRequest(http.MethodPost, "/api/admin/workflow/scheduler/trigger/fund-A", `{"tradingDate":"not-a-date"}`)
	req.SetPathValue("fundId", "fund-A")
	h.handleSchedulerTrigger(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(stub.calls) != 0 {
		t.Fatalf("expected zero trigger calls, got %d", len(stub.calls))
	}
}

// TestSchedulerTriggerSuccess verifies the happy path — fundId from
// the URL + tradingDate from the body are forwarded to the workflow
// service, the result is JSON-encoded back to the caller.
func TestSchedulerTriggerSuccess(t *testing.T) {
	stub := &stubWorkflowAdminTrigger{
		result: &adminTriggerResult{
			FundID:      "fund-A",
			TradingDate: "2026-05-19",
			State:       "running",
			Step:        "macro_brief",
		},
	}
	h := &adminHandler{workflowService: stub}
	rr := httptest.NewRecorder()
	req := adminSuperRequest(http.MethodPost, "/api/admin/workflow/scheduler/trigger/fund-A", `{"tradingDate":"2026-05-19"}`)
	req.SetPathValue("fundId", "fund-A")
	h.handleSchedulerTrigger(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 trigger call, got %d", len(stub.calls))
	}
	got := stub.calls[0]
	if got.FundID != "fund-A" {
		t.Fatalf("expected fundId=fund-A, got %q", got.FundID)
	}
	if got.TradingDate.Format("2006-01-02") != "2026-05-19" {
		t.Fatalf("expected tradingDate=2026-05-19, got %s", got.TradingDate)
	}
	var resp adminTriggerResult
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.State != "running" || resp.Step != "macro_brief" {
		t.Fatalf("unexpected response payload: %+v", resp)
	}
}

// TestSchedulerTriggerDefaultsToTodayWhenBodyEmpty exercises the
// "no body" path — operators that hit the endpoint with curl and no
// payload should still get a well-formed trigger for today (UTC).
func TestSchedulerTriggerDefaultsToTodayWhenBodyEmpty(t *testing.T) {
	stub := &stubWorkflowAdminTrigger{
		result: &adminTriggerResult{FundID: "fund-A", TradingDate: time.Now().UTC().Format("2006-01-02"), State: "running"},
	}
	h := &adminHandler{workflowService: stub}
	rr := httptest.NewRecorder()
	req := adminSuperRequest(http.MethodPost, "/api/admin/workflow/scheduler/trigger/fund-A", "")
	req.SetPathValue("fundId", "fund-A")
	h.handleSchedulerTrigger(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 trigger call, got %d", len(stub.calls))
	}
	// Date should be within a 5-min window of "now UTC".
	delta := time.Since(stub.calls[0].TradingDate)
	if delta < 0 {
		delta = -delta
	}
	if delta > 5*time.Minute {
		t.Fatalf("expected default trigger time to be ~now, got delta=%s", delta)
	}
}

// TestSchedulerTriggerPropagatesServiceError surfaces backend errors
// as 500 so operators see _something_ went wrong rather than getting
// a misleading 200.
func TestSchedulerTriggerPropagatesServiceError(t *testing.T) {
	stub := &stubWorkflowAdminTrigger{err: errors.New("db conn refused")}
	h := &adminHandler{workflowService: stub}
	rr := httptest.NewRecorder()
	req := adminSuperRequest(http.MethodPost, "/api/admin/workflow/scheduler/trigger/fund-A", "")
	req.SetPathValue("fundId", "fund-A")
	h.handleSchedulerTrigger(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "db conn refused") {
		t.Fatalf("expected service error in body, got %s", rr.Body.String())
	}
}

// _ keeps context import alive even if a future refactor drops the
// direct usage — the contextLike alias is intentionally an empty
// interface{} and Go's type-checker would otherwise warn here.
var _ = context.Background
