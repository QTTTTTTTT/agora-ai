package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/repository"
)

func newLLMHealthTestHandler(t *testing.T) (*adminHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	h := &adminHandler{
		db:            db,
		llmHealthRepo: repository.NewLLMHealthRepo(db),
		auditLogger:   audit.NewDBLogger(db),
	}
	return h, mock, func() { db.Close() }
}

func TestAdminLLMHealth_RegistrationGuard(t *testing.T) {
	// When the repo is nil the routes must not be registered. This
	// pins the nil-safe contract — the prod build that doesn't run
	// migration 077 still boots.
	h := &adminHandler{}
	mux := http.NewServeMux()
	h.registerLLMHealthAdminRoutes(mux)
	req, _ := http.NewRequest(http.MethodGet, "/api/admin/llm-health/summary", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when repo is nil, got %d", rec.Code)
	}
}

func TestAdminLLMHealth_Summary_HappyPath(t *testing.T) {
	h, mock, done := newLLMHealthTestHandler(t)
	defer done()

	userID := expectAdminGate(t, mock, adminRoleSuperAdmin)

	mock.ExpectQuery(`FROM investment_plans[\s\S]*GROUP BY 1`).
		WillReturnRows(sqlmock.NewRows([]string{"src", "count"}).
			AddRow("llm_pm", 412).
			AddRow("llm_three_stage", 88).
			AddRow("fallback_after_llm_error", 11).
			AddRow("legacy", 3))
	mock.ExpectQuery(`fallback_reason->>'category'`).
		WillReturnRows(sqlmock.NewRows([]string{"category", "provider", "count"}).
			AddRow("rate_limited", "openai", 7).
			AddRow("service_unavailable", "claude", 3).
			AddRow("unknown", "", 1))

	mux := http.NewServeMux()
	h.registerLLMHealthAdminRoutes(mux)

	req, _ := http.NewRequest(http.MethodGet, "/api/admin/llm-health/summary?window_hours=24", nil)
	req = withAdminAuth(req, userID)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body llmHealthSummaryWire
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.WindowHours != 24 {
		t.Fatalf("expected window=24, got %d", body.WindowHours)
	}
	if len(body.Sources) != 4 {
		t.Fatalf("expected 4 source rows, got %d", len(body.Sources))
	}
	if body.Sources[0].Source != "llm_pm" || body.Sources[0].Count != 412 {
		t.Fatalf("expected first row llm_pm=412, got %+v", body.Sources[0])
	}
	if len(body.Categories) != 3 {
		t.Fatalf("expected 3 category rows, got %d", len(body.Categories))
	}
	if body.Categories[0].Category != "rate_limited" {
		t.Fatalf("expected first category=rate_limited, got %s", body.Categories[0].Category)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestAdminLLMHealth_Summary_DefaultsTo24Hours(t *testing.T) {
	h, mock, done := newLLMHealthTestHandler(t)
	defer done()

	userID := expectAdminGate(t, mock, adminRoleSuperAdmin)

	mock.ExpectQuery(`FROM investment_plans`).
		WillReturnRows(sqlmock.NewRows([]string{"src", "count"}).AddRow("llm_pm", 1))
	mock.ExpectQuery(`fallback_reason`).
		WillReturnRows(sqlmock.NewRows([]string{"category", "provider", "count"}))

	mux := http.NewServeMux()
	h.registerLLMHealthAdminRoutes(mux)

	req, _ := http.NewRequest(http.MethodGet, "/api/admin/llm-health/summary", nil)
	req = withAdminAuth(req, userID)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var body llmHealthSummaryWire
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body.WindowHours != 24 {
		t.Fatalf("expected default window=24, got %d", body.WindowHours)
	}
}

func TestAdminLLMHealth_RecentFallbacks_HappyPath(t *testing.T) {
	h, mock, done := newLLMHealthTestHandler(t)
	defer done()

	userID := expectAdminGate(t, mock, adminRoleSuperAdmin)

	createdAt := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`LIKE 'fallback_%'`).
		WithArgs("24 hours", 50).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "src", "category", "provider", "model", "summary", "created_at",
		}).AddRow(
			"plan-1", "fund-1", "fallback_after_llm_error",
			"rate_limited", "openai", "gpt-4o", "rate limit details here", createdAt,
		).AddRow(
			"plan-2", "fund-2", "fallback_empty_plan",
			"empty_response", "", "", "", createdAt.Add(-time.Hour),
		))

	mux := http.NewServeMux()
	h.registerLLMHealthAdminRoutes(mux)

	req, _ := http.NewRequest(http.MethodGet, "/api/admin/llm-health/recent-fallbacks", nil)
	req = withAdminAuth(req, userID)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		WindowHours int                   `json:"window_hours"`
		Items       []llmHealthRecentWire `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(body.Items))
	}
	if body.Items[0].Summary != "rate limit details here" {
		t.Fatalf("admin endpoint MUST surface raw summary; got %q", body.Items[0].Summary)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestAdminLLMHealth_RequiresAdmin(t *testing.T) {
	h, _, done := newLLMHealthTestHandler(t)
	defer done()

	mux := http.NewServeMux()
	h.registerLLMHealthAdminRoutes(mux)

	// No auth context → requireAdmin must reject with 401.
	req, _ := http.NewRequest(http.MethodGet, "/api/admin/llm-health/summary", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d", rec.Code)
	}
}

func TestParseHealthWindow(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", 24 * time.Hour},
		{"  ", 24 * time.Hour},
		{"abc", 24 * time.Hour},
		{"0", 24 * time.Hour},
		{"-5", 24 * time.Hour},
		{"1", time.Hour},
		{"72", 72 * time.Hour},
	}
	for _, c := range cases {
		if got := parseHealthWindow(c.raw); got != c.want {
			t.Fatalf("parseHealthWindow(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}
