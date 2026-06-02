package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func newAdminDrawdownEnv(t *testing.T) (*adminHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	h := &adminHandler{db: db, metrics: newServerMetrics()}
	return h, mock, func() { _ = db.Close() }
}

// 401 path.
func TestAdminDrawdown_GetPolicy_Unauthenticated(t *testing.T) {
	h, _, cleanup := newAdminDrawdownEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/drawdown/funds/f1/policy", nil)
	req.SetPathValue("fundId", "f1")
	rr := httptest.NewRecorder()
	h.handleGetDrawdownPolicy(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

// 403 path.
func TestAdminDrawdown_GetPolicy_Forbidden(t *testing.T) {
	h, mock, cleanup := newAdminDrawdownEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "user")
	req := authReq(http.MethodGet, "/api/admin/drawdown/funds/f1/policy", "", "u-1")
	req.SetPathValue("fundId", "f1")
	rr := httptest.NewRecorder()
	h.handleGetDrawdownPolicy(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
}

// Happy path.
func TestAdminDrawdown_GetPolicy_HappyPath(t *testing.T) {
	h, mock, cleanup := newAdminDrawdownEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	mock.ExpectQuery("FROM drawdown_policies").
		WithArgs("f1").
		WillReturnRows(sqlmock.NewRows([]string{"tier", "dd_pct", "action", "trim_ratio", "cooldown_hours", "auto_execute", "note"}).
			AddRow(1, -0.05, "trim_proportional", 0.25, 24, false, ""))
	req := authReq(http.MethodGet, "/api/admin/drawdown/funds/f1/policy", "", "u-1")
	req.SetPathValue("fundId", "f1")
	rr := httptest.NewRecorder()
	h.handleGetDrawdownPolicy(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Policy drawdownPolicyWire `json:"policy"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Policy.FundID != "f1" || len(body.Policy.Tiers) != 1 {
		t.Errorf("got %+v", body.Policy)
	}
}

// Tier upsert: bad JSON.
func TestAdminDrawdown_UpsertTier_InvalidBody(t *testing.T) {
	h, mock, cleanup := newAdminDrawdownEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodPut, "/api/admin/drawdown/funds/f1/policy/tiers/1", "{garbage", "u-1")
	req.SetPathValue("fundId", "f1")
	req.SetPathValue("tier", "1")
	rr := httptest.NewRecorder()
	h.handleUpsertDrawdownTier(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

// Tier upsert: out-of-range tier.
func TestAdminDrawdown_UpsertTier_OutOfRange(t *testing.T) {
	h, mock, cleanup := newAdminDrawdownEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodPut, "/api/admin/drawdown/funds/f1/policy/tiers/9",
		`{"dd_pct":-0.05,"action":"trim_proportional"}`, "u-1")
	req.SetPathValue("fundId", "f1")
	req.SetPathValue("tier", "9")
	rr := httptest.NewRecorder()
	h.handleUpsertDrawdownTier(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "invalid_tier") {
		t.Errorf("body = %s", rr.Body.String())
	}
}

// Tier upsert: bad action.
func TestAdminDrawdown_UpsertTier_BadAction(t *testing.T) {
	h, mock, cleanup := newAdminDrawdownEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodPut, "/api/admin/drawdown/funds/f1/policy/tiers/1",
		`{"dd_pct":-0.05,"action":"explode"}`, "u-1")
	req.SetPathValue("fundId", "f1")
	req.SetPathValue("tier", "1")
	rr := httptest.NewRecorder()
	h.handleUpsertDrawdownTier(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

// Review event: invalid status.
func TestAdminDrawdown_ReviewEvent_InvalidStatus(t *testing.T) {
	h, mock, cleanup := newAdminDrawdownEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodPost, "/api/admin/drawdown/events/ev-1/review",
		`{"status":"bogus"}`, "u-1")
	req.SetPathValue("id", "ev-1")
	rr := httptest.NewRecorder()
	h.handleReviewDrawdownEvent(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 body=%s", rr.Code, rr.Body.String())
	}
}

// Review event: refuses manual 'executed'.
func TestAdminDrawdown_ReviewEvent_RefusesManualExecuted(t *testing.T) {
	h, mock, cleanup := newAdminDrawdownEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodPost, "/api/admin/drawdown/events/ev-1/review",
		`{"status":"executed"}`, "u-1")
	req.SetPathValue("id", "ev-1")
	rr := httptest.NewRecorder()
	h.handleReviewDrawdownEvent(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "executed") {
		t.Errorf("body = %s", rr.Body.String())
	}
}

// Review event: missing id.
func TestAdminDrawdown_ReviewEvent_MissingID(t *testing.T) {
	h, mock, cleanup := newAdminDrawdownEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodPost, "/api/admin/drawdown/events//review",
		`{"status":"approved"}`, "u-1")
	rr := httptest.NewRecorder()
	h.handleReviewDrawdownEvent(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// Trigger check: missing fund_id.
func TestAdminDrawdown_TriggerCheck_MissingFundID(t *testing.T) {
	h, mock, cleanup := newAdminDrawdownEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodPost, "/api/admin/drawdown/funds//check", `{}`, "u-1")
	rr := httptest.NewRecorder()
	h.handleTriggerDrawdownCheck(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// Trigger check: no policy => 400 not_found-ish.
func TestAdminDrawdown_TriggerCheck_NoPolicy(t *testing.T) {
	h, mock, cleanup := newAdminDrawdownEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	mock.ExpectQuery("FROM drawdown_policies").
		WithArgs("f1").
		WillReturnRows(sqlmock.NewRows([]string{"tier", "dd_pct", "action", "trim_ratio", "cooldown_hours", "auto_execute", "note"}))
	req := authReq(http.MethodPost, "/api/admin/drawdown/funds/f1/check", `{}`, "u-1")
	req.SetPathValue("fundId", "f1")
	rr := httptest.NewRecorder()
	h.handleTriggerDrawdownCheck(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "no_policy") {
		t.Errorf("body = %s", rr.Body.String())
	}
}
