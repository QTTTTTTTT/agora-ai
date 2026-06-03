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
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/llm"
	"github.com/fundai/server/internal/modelab"
)

func newModelABTestHandler(t *testing.T) (*adminHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	repo := modelab.NewRepo(db)
	reporter := modelab.NewReporter(repo)
	h := &adminHandler{
		db:              db,
		modelABRepo:     repo,
		modelABReporter: reporter,
		auditLogger:     audit.NewDBLogger(db),
	}
	return h, mock, func() { db.Close() }
}

// expectAdminGate primes the sqlmock to satisfy requireAdmin's
// users.role lookup. Returns the user id we'll authenticate as.
func expectAdminGate(t *testing.T, mock sqlmock.Sqlmock, role string) string {
	t.Helper()
	userID := "admin-1"
	mock.ExpectQuery(`SELECT role FROM users WHERE id`).WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow(role))
	return userID
}

func withAdminAuth(req *http.Request, userID string) *http.Request {
	return req.WithContext(api.WithAuthenticatedUserID(req.Context(), userID))
}

func TestAdminModelAB_RegistrationGuard(t *testing.T) {
	h := &adminHandler{}
	mux := http.NewServeMux()
	h.registerModelABAdminRoutes(mux)
	// Without modelABRepo wired the handler must NOT register
	// any routes; the server should respond 404 to model A/B
	// paths.
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/admin/model-ab/experiments")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 when modelABRepo is nil, got %d", resp.StatusCode)
	}
}

func TestAdminModelAB_ListExperiments_HappyPath(t *testing.T) {
	h, mock, done := newModelABTestHandler(t)
	defer done()

	userID := expectAdminGate(t, mock, adminRoleSuperAdmin)

	armsJSON, _ := modelab.MarshalArms([]modelab.ArmConfig{
		{Name: "ctrl", Provider: llm.ProviderOpenAI, ModelName: "gpt-4o", ModelTier: llm.TierCritical},
		{Name: "treat", Provider: llm.ProviderClaude, ModelName: "claude-opus", ModelTier: llm.TierCritical},
	})
	mock.ExpectQuery(`FROM model_ab_experiments`).WillReturnRows(
		sqlmock.NewRows([]string{
			"id", "name", "description", "scope", "scope_target",
			"step_filter", "arms", "traffic_split",
			"status", "start_at", "end_at",
			"max_total_tokens", "tokens_used", "created_by",
			"created_at", "updated_at",
		}).AddRow(
			"00000000-0000-0000-0000-000000000010",
			"claude vs gpt", "",
			string(modelab.ScopeGlobal), "",
			"{}",
			armsJSON,
			"{0.5,0.5}",
			string(modelab.StatusRunning),
			time.Time{}, time.Time{},
			int64(0), int64(0), "",
			time.Now(), time.Now(),
		))

	mux := http.NewServeMux()
	h.registerModelABAdminRoutes(mux)

	req := httptest.NewRequest("GET", "/api/admin/model-ab/experiments", nil)
	req = withAdminAuth(req, userID)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Experiments []modelABExperimentWire `json:"experiments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Experiments) != 1 || body.Experiments[0].Name != "claude vs gpt" {
		t.Fatalf("unexpected payload: %+v", body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
}

func TestAdminModelAB_CreateExperiment_Validates(t *testing.T) {
	h, mock, done := newModelABTestHandler(t)
	defer done()

	userID := expectAdminGate(t, mock, adminRoleSuperAdmin)

	mux := http.NewServeMux()
	h.registerModelABAdminRoutes(mux)

	body := map[string]any{
		"name":  "no arms",
		"scope": "global",
		// Missing arms / traffic_split.
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/admin/model-ab/experiments", bytes.NewReader(raw))
	req = withAdminAuth(req, userID)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (validation failure), got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_experiment") {
		t.Fatalf("expected invalid_experiment error code, body=%s", rec.Body.String())
	}
}

func TestAdminModelAB_SetStatus_AcceptsValidValues(t *testing.T) {
	h, mock, done := newModelABTestHandler(t)
	defer done()

	userID := expectAdminGate(t, mock, adminRoleSuperAdmin)

	mock.ExpectExec(`UPDATE model_ab_experiments\s+SET status`).
		WithArgs("paused", "00000000-0000-0000-0000-000000000099").
		WillReturnResult(sqlmock.NewResult(1, 1))

	armsJSON, _ := modelab.MarshalArms([]modelab.ArmConfig{
		{Name: "ctrl", Provider: llm.ProviderOpenAI, ModelName: "gpt-4o"},
		{Name: "treat", Provider: llm.ProviderClaude, ModelName: "claude-opus"},
	})
	mock.ExpectQuery(`FROM model_ab_experiments\s+WHERE id`).WillReturnRows(
		sqlmock.NewRows([]string{
			"id", "name", "description", "scope", "scope_target",
			"step_filter", "arms", "traffic_split",
			"status", "start_at", "end_at",
			"max_total_tokens", "tokens_used", "created_by",
			"created_at", "updated_at",
		}).AddRow(
			"00000000-0000-0000-0000-000000000099",
			"x", "",
			string(modelab.ScopeGlobal), "",
			"{}", armsJSON, "{0.5,0.5}",
			string(modelab.StatusPaused),
			time.Time{}, time.Time{},
			int64(0), int64(0), "",
			time.Now(), time.Now(),
		))
	mock.ExpectExec(`INSERT INTO data_access_log`).WillReturnResult(sqlmock.NewResult(1, 1))

	mux := http.NewServeMux()
	h.registerModelABAdminRoutes(mux)

	body, _ := json.Marshal(map[string]any{"status": "paused"})
	req := httptest.NewRequest("PATCH",
		"/api/admin/model-ab/experiments/00000000-0000-0000-0000-000000000099/status",
		bytes.NewReader(body))
	req = withAdminAuth(req, userID)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var out modelABExperimentWire
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "paused" {
		t.Fatalf("expected status=paused, got %q", out.Status)
	}
}

func TestAdminModelAB_SetStatus_RejectsInvalidValue(t *testing.T) {
	h, mock, done := newModelABTestHandler(t)
	defer done()
	userID := expectAdminGate(t, mock, adminRoleSuperAdmin)

	mux := http.NewServeMux()
	h.registerModelABAdminRoutes(mux)

	body, _ := json.Marshal(map[string]any{"status": "totally_invalid"})
	req := httptest.NewRequest("PATCH",
		"/api/admin/model-ab/experiments/00000000-0000-0000-0000-000000000099/status",
		bytes.NewReader(body))
	req = withAdminAuth(req, userID)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// ---- S10.4 — clone / edit draft / bulk archive -----------------------------

func TestAdminModelAB_UpdateDraft_HappyPath(t *testing.T) {
	h, mock, done := newModelABTestHandler(t)
	defer done()
	userID := expectAdminGate(t, mock, adminRoleSuperAdmin)

	mock.ExpectExec(`UPDATE model_ab_experiments\s+SET name`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	armsJSON, _ := modelab.MarshalArms([]modelab.ArmConfig{
		{Name: "ctrl", Provider: llm.ProviderOpenAI, ModelName: "gpt-4o"},
		{Name: "treat", Provider: llm.ProviderClaude, ModelName: "claude-opus"},
	})
	mock.ExpectQuery(`FROM model_ab_experiments\s+WHERE id`).WillReturnRows(
		sqlmock.NewRows([]string{
			"id", "name", "description", "scope", "scope_target",
			"step_filter", "arms", "traffic_split",
			"status", "start_at", "end_at",
			"max_total_tokens", "tokens_used", "created_by",
			"created_at", "updated_at",
		}		).AddRow(
			"00000000-0000-0000-0000-000000000300",
			"renamed", "patched",
			string(modelab.ScopeGlobal), "",
			"{}", armsJSON, "{0.5,0.5}",
			string(modelab.StatusDraft),
			time.Time{}, time.Time{}, int64(0), int64(0), "",
			time.Now(), time.Now(),
		))

	mux := http.NewServeMux()
	h.registerModelABAdminRoutes(mux)

	body, _ := json.Marshal(map[string]any{
		"name":        "renamed",
		"description": "patched",
		"scope":       "global",
		"arms": []map[string]any{
			{"name": "ctrl", "provider": "openai", "model_name": "gpt-4o"},
			{"name": "treat", "provider": "claude", "model_name": "claude-opus"},
		},
		"traffic_split": []float64{0.5, 0.5},
	})
	req := httptest.NewRequest("PATCH",
		"/api/admin/model-ab/experiments/00000000-0000-0000-0000-000000000300",
		bytes.NewReader(body))
	req = withAdminAuth(req, userID)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var out modelABExperimentWire
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Name != "renamed" {
		t.Fatalf("expected name=renamed, got %q", out.Name)
	}
}

func TestAdminModelAB_UpdateDraft_RejectsNonDraft(t *testing.T) {
	h, mock, done := newModelABTestHandler(t)
	defer done()
	userID := expectAdminGate(t, mock, adminRoleSuperAdmin)

	// First UPDATE returns 0 affected rows — repo will probe GetExperiment
	mock.ExpectExec(`UPDATE model_ab_experiments\s+SET name`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	armsJSON, _ := modelab.MarshalArms([]modelab.ArmConfig{
		{Name: "ctrl", Provider: llm.ProviderOpenAI, ModelName: "gpt-4o"},
		{Name: "treat", Provider: llm.ProviderClaude, ModelName: "claude-opus"},
	})
	mock.ExpectQuery(`FROM model_ab_experiments\s+WHERE id`).WillReturnRows(
		sqlmock.NewRows([]string{
			"id", "name", "description", "scope", "scope_target",
			"step_filter", "arms", "traffic_split",
			"status", "start_at", "end_at",
			"max_total_tokens", "tokens_used", "created_by",
			"created_at", "updated_at",
		}).AddRow(
			"00000000-0000-0000-0000-000000000301",
			"already running", "",
			string(modelab.ScopeGlobal), "",
			"{}", armsJSON, "{0.5,0.5}",
			string(modelab.StatusRunning),
			time.Time{}, time.Time{}, int64(0), int64(0), "",
			time.Now(), time.Now(),
		))

	mux := http.NewServeMux()
	h.registerModelABAdminRoutes(mux)

	body, _ := json.Marshal(map[string]any{
		"name":  "rename",
		"scope": "global",
		"arms": []map[string]any{
			{"name": "ctrl", "provider": "openai", "model_name": "gpt-4o"},
			{"name": "treat", "provider": "claude", "model_name": "claude-opus"},
		},
		"traffic_split": []float64{0.5, 0.5},
	})
	req := httptest.NewRequest("PATCH",
		"/api/admin/model-ab/experiments/00000000-0000-0000-0000-000000000301",
		bytes.NewReader(body))
	req = withAdminAuth(req, userID)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 (not_editable), got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not_editable") {
		t.Fatalf("expected not_editable error code, body=%s", rec.Body.String())
	}
}

func TestAdminModelAB_Clone_CreatesDraft(t *testing.T) {
	h, mock, done := newModelABTestHandler(t)
	defer done()
	userID := expectAdminGate(t, mock, adminRoleSuperAdmin)

	armsJSON, _ := modelab.MarshalArms([]modelab.ArmConfig{
		{Name: "ctrl", Provider: llm.ProviderOpenAI, ModelName: "gpt-4o"},
		{Name: "treat", Provider: llm.ProviderClaude, ModelName: "claude-opus"},
	})
	// repo.Clone → GetExperiment(source)
	mock.ExpectQuery(`FROM model_ab_experiments\s+WHERE id`).WillReturnRows(
		sqlmock.NewRows([]string{
			"id", "name", "description", "scope", "scope_target",
			"step_filter", "arms", "traffic_split",
			"status", "start_at", "end_at",
			"max_total_tokens", "tokens_used", "created_by",
			"created_at", "updated_at",
		}).AddRow(
			"00000000-0000-0000-0000-000000000400",
			"source exp", "",
			string(modelab.ScopeGlobal), "",
			"{}", armsJSON, "{0.5,0.5}",
			string(modelab.StatusRunning),
			time.Time{}, time.Time{}, int64(0), int64(0), "",
			time.Now(), time.Now(),
		))
	// repo.Clone → CreateExperiment INSERT
	mock.ExpectQuery(`INSERT INTO model_ab_experiments`).WillReturnRows(
		sqlmock.NewRows([]string{"id"}).AddRow("00000000-0000-0000-0000-000000000401"))
	// final GetExperiment of the clone
	mock.ExpectQuery(`FROM model_ab_experiments\s+WHERE id`).WillReturnRows(
		sqlmock.NewRows([]string{
			"id", "name", "description", "scope", "scope_target",
			"step_filter", "arms", "traffic_split",
			"status", "start_at", "end_at",
			"max_total_tokens", "tokens_used", "created_by",
			"created_at", "updated_at",
		}).AddRow(
			"00000000-0000-0000-0000-000000000401",
			"source exp (copy)", "",
			string(modelab.ScopeGlobal), "",
			"{}", armsJSON, "{0.5,0.5}",
			string(modelab.StatusDraft),
			time.Time{}, time.Time{}, int64(0), int64(0), userID,
			time.Now(), time.Now(),
		))

	mux := http.NewServeMux()
	h.registerModelABAdminRoutes(mux)

	req := httptest.NewRequest("POST",
		"/api/admin/model-ab/experiments/00000000-0000-0000-0000-000000000400/clone", nil)
	req = withAdminAuth(req, userID)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	var out modelABExperimentWire
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != string(modelab.StatusDraft) {
		t.Fatalf("expected status=draft, got %q", out.Status)
	}
}

func TestAdminModelAB_BulkSetStatus_Archives(t *testing.T) {
	h, mock, done := newModelABTestHandler(t)
	defer done()
	userID := expectAdminGate(t, mock, adminRoleSuperAdmin)

	mock.ExpectExec(`UPDATE model_ab_experiments\s+SET status`).
		WithArgs("archived", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 2))

	mux := http.NewServeMux()
	h.registerModelABAdminRoutes(mux)

	body, _ := json.Marshal(map[string]any{
		"ids": []string{
			"00000000-0000-0000-0000-000000000500",
			"00000000-0000-0000-0000-000000000501",
		},
		"status": "archived",
	})
	req := httptest.NewRequest("POST",
		"/api/admin/model-ab/experiments/bulk-status",
		bytes.NewReader(body))
	req = withAdminAuth(req, userID)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var out bulkSetModelABStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Updated != 2 {
		t.Fatalf("expected updated=2, got %d", out.Updated)
	}
}

func TestAdminModelAB_BulkSetStatus_RejectsInvalidStatus(t *testing.T) {
	h, mock, done := newModelABTestHandler(t)
	defer done()
	userID := expectAdminGate(t, mock, adminRoleSuperAdmin)

	mux := http.NewServeMux()
	h.registerModelABAdminRoutes(mux)

	body, _ := json.Marshal(map[string]any{
		"ids":    []string{"00000000-0000-0000-0000-000000000600"},
		"status": "totally_invalid",
	})
	req := httptest.NewRequest("POST",
		"/api/admin/model-ab/experiments/bulk-status",
		bytes.NewReader(body))
	req = withAdminAuth(req, userID)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}
