package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/modelab"
)

func newPromotionTestHandler(t *testing.T) (*adminHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	repo := modelab.NewRepo(db)
	drafts := modelab.NewDraftRepo(db)
	h := &adminHandler{
		db:                        db,
		modelABRepo:               repo,
		modelABPromotionDraftRepo: drafts,
		auditLogger:               audit.NewDBLogger(db),
	}
	return h, mock, func() { db.Close() }
}

func TestAdminPromotionDrafts_RegistrationGuard(t *testing.T) {
	h := &adminHandler{}
	mux := http.NewServeMux()
	h.registerModelABPromotionRoutes(mux)
	// No draft repo wired — routes must not register, so a GET
	// against the list endpoint should 404.
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/admin/model-ab/promotion-drafts")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestAdminPromotionDrafts_List_HappyPath(t *testing.T) {
	h, mock, done := newPromotionTestHandler(t)
	defer done()

	userID := expectAdminGate(t, mock, adminRoleSuperAdmin)
	mock.ExpectQuery(`FROM model_ab_promotion_drafts`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "experiment_id",
			"recommended_arm_index", "recommended_arm_label",
			"primary_arm_index", "primary_arm_label",
			"streak_days", "evaluated_at", "window_from", "window_to",
			"criteria_payload", "report_snapshot",
			"status", "applied_by", "applied_at",
			"rejection_reason", "created_at",
		}).AddRow(
			"draft-1", "exp-1",
			1, "anthropic/claude-opus",
			0, "openai/gpt-4o",
			7, time.Now(), nil, nil,
			"{}", "{}",
			"pending", "", nil,
			"", time.Now(),
		))

	mux := http.NewServeMux()
	h.registerModelABPromotionRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/model-ab/promotion-drafts", nil)
	req = withAdminAuth(req, userID)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []promotionDraftWire `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(body.Items))
	}
	if body.Items[0].RecommendedArmLabel != "anthropic/claude-opus" {
		t.Fatalf("unexpected label: %s", body.Items[0].RecommendedArmLabel)
	}
	// List endpoint must NOT include the report snapshot to keep
	// the payload light.
	if len(body.Items[0].ReportSnapshot) > 0 {
		t.Fatalf("list endpoint must omit report_snapshot, got %s", body.Items[0].ReportSnapshot)
	}
}

func TestAdminPromotionDrafts_Get_IncludesReport(t *testing.T) {
	h, mock, done := newPromotionTestHandler(t)
	defer done()

	userID := expectAdminGate(t, mock, adminRoleSuperAdmin)
	mock.ExpectQuery(`FROM model_ab_promotion_drafts WHERE id`).
		WithArgs("draft-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "experiment_id",
			"recommended_arm_index", "recommended_arm_label",
			"primary_arm_index", "primary_arm_label",
			"streak_days", "evaluated_at", "window_from", "window_to",
			"criteria_payload", "report_snapshot",
			"status", "applied_by", "applied_at",
			"rejection_reason", "created_at",
		}).AddRow(
			"draft-1", "exp-1",
			1, "anthropic/claude-opus",
			0, "openai/gpt-4o",
			7, time.Now(), nil, nil,
			`{"min_streak_days":7}`, `{"foo":"bar"}`,
			"pending", "", nil,
			"", time.Now(),
		))

	mux := http.NewServeMux()
	h.registerModelABPromotionRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/model-ab/promotion-drafts/draft-1", nil)
	req = withAdminAuth(req, userID)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var wire promotionDraftWire
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(wire.ReportSnapshot) == 0 {
		t.Fatalf("detail endpoint must include report_snapshot, got empty")
	}
}

func TestAdminPromotionDrafts_Apply_HappyPath(t *testing.T) {
	h, mock, done := newPromotionTestHandler(t)
	defer done()

	userID := expectAdminGate(t, mock, adminRoleSuperAdmin)

	// 1. Get(): used by handleApply to read the source experiment id.
	mock.ExpectQuery(`FROM model_ab_promotion_drafts WHERE id`).
		WithArgs("draft-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "experiment_id",
			"recommended_arm_index", "recommended_arm_label",
			"primary_arm_index", "primary_arm_label",
			"streak_days", "evaluated_at", "window_from", "window_to",
			"criteria_payload", "report_snapshot",
			"status", "applied_by", "applied_at",
			"rejection_reason", "created_at",
		}).AddRow(
			"draft-1", "11111111-1111-1111-1111-111111111111",
			1, "anthropic/claude-opus",
			0, "openai/gpt-4o",
			7, time.Now(), nil, nil,
			"{}", "{}",
			"pending", "", nil,
			"", time.Now(),
		))
	// 2. Apply() UPDATE on drafts.
	mock.ExpectExec(`UPDATE model_ab_promotion_drafts`).
		WithArgs("draft-1", userID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// 3. modelab.Repo.SetStatus() UPDATE on experiments.
	mock.ExpectExec(`UPDATE model_ab_experiments`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mux := http.NewServeMux()
	h.registerModelABPromotionRoutes(mux)

	req := httptest.NewRequest(http.MethodPatch, "/api/admin/model-ab/promotion-drafts/draft-1/apply", nil)
	req = withAdminAuth(req, userID)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		OK               bool   `json:"ok"`
		ExperimentClosed bool   `json:"experiment_closed"`
		ExperimentID     string `json:"experiment_id"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if !body.OK || !body.ExperimentClosed {
		t.Fatalf("expected ok+experiment_closed, got %+v", body)
	}
}

func TestAdminPromotionDrafts_Reject_HappyPath(t *testing.T) {
	h, mock, done := newPromotionTestHandler(t)
	defer done()

	userID := expectAdminGate(t, mock, adminRoleSuperAdmin)
	mock.ExpectExec(`UPDATE model_ab_promotion_drafts`).
		WithArgs("draft-1", userID, "still trust primary").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mux := http.NewServeMux()
	h.registerModelABPromotionRoutes(mux)

	req := httptest.NewRequest(http.MethodPatch, "/api/admin/model-ab/promotion-drafts/draft-1/reject",
		strings.NewReader(`{"reason":"still trust primary"}`))
	req.Header.Set("Content-Type", "application/json")
	req = withAdminAuth(req, userID)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
}
