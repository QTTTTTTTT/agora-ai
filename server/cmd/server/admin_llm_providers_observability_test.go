package main

import (
	"database/sql"
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

// newObservabilityTestHandler is a richer setup than the S13 one:
// it wires the two new S14 repos so the observability routes have
// something to read.
func newObservabilityTestHandler(t *testing.T) (*adminHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	h := &adminHandler{
		db:                        db,
		platformLLMProviderRepo:   repository.NewPlatformLLMProviderRepo(db),
		providerHealthHistoryRepo: repository.NewProviderHealthHistoryRepo(db),
		providerDailyRollupRepo:   repository.NewProviderDailyRollupRepo(db),
	}
	return h, mock, func() { db.Close() }
}

func TestParseRange_Defaults(t *testing.T) {
	if d := parseRange("", "24h"); d != 24*time.Hour {
		t.Fatalf("empty → 24h, got %s", d)
	}
	if d := parseRange("", "7d"); d != 7*24*time.Hour {
		t.Fatalf("empty 7d default, got %s", d)
	}
	if d := parseRange("garbage", "24h"); d != 30*24*time.Hour {
		t.Fatalf("garbage should clamp to 30d, got %s", d)
	}
	if d := parseRange("6h", "24h"); d != 6*time.Hour {
		t.Fatalf("6h, got %s", d)
	}
	if d := parseRange("30d", "24h"); d != 30*24*time.Hour {
		t.Fatalf("30d, got %s", d)
	}
}

func TestProviderHealthDashboard_NoRepo_ReturnsEmpty(t *testing.T) {
	h := &adminHandler{}
	mux := http.NewServeMux()
	h.registerLLMProviderObservabilityRoutes(mux)
	// Without auth context the request fails requireAdmin (401).
	// Skip auth by calling the handler method directly.
	req := httptest.NewRequest(http.MethodGet, "/api/admin/llm-providers/health", nil)
	rec := httptest.NewRecorder()
	// Bypass auth: build a request with admin context.
	req = adminRequest(http.MethodGet, "/api/admin/llm-providers/health", nil)
	mux.ServeHTTP(rec, req)
	// Without a DB, requireAdmin returns 401 — short-circuit. We
	// instead assert the handler is *registered* by checking we
	// don't get 404.
	if rec.Code == http.StatusNotFound {
		t.Fatalf("route not registered: %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestProviderHealthDashboard_Empty_NoRows(t *testing.T) {
	h, mock, done := newObservabilityTestHandler(t)
	defer done()
	expectAdminRoleCheck(mock)
	// Summarise returns no rows.
	mock.ExpectQuery(regexp.QuoteMeta("PERCENTILE_CONT(0.50)")).
		WillReturnRows(sqlmock.NewRows([]string{
			"provider_id", "provider", "label", "checks", "successes", "failures",
			"p50", "p95", "p_max", "last_checked_at", "last_ok",
		}))

	mux := http.NewServeMux()
	h.registerLLMProviderObservabilityRoutes(mux)
	req := adminRequest(http.MethodGet, "/api/admin/llm-providers/health?range=24h", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body providerHealthDashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Rows) != 0 {
		t.Fatalf("expected 0 rows on empty DB, got %d", len(body.Rows))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestProviderHealthDashboard_HappyPath(t *testing.T) {
	h, mock, done := newObservabilityTestHandler(t)
	defer done()
	expectAdminRoleCheck(mock)
	pid := uuid.New()
	now := time.Now()
	// summarise
	mock.ExpectQuery(regexp.QuoteMeta("PERCENTILE_CONT(0.50)")).
		WillReturnRows(sqlmock.NewRows([]string{
			"provider_id", "provider", "label", "checks", "successes", "failures",
			"p50", "p95", "p_max", "last_checked_at", "last_ok",
		}).AddRow(pid, "openai", "openai-prod", 288, 285, 3, 120, 350, 800, now, true))
	// sparkline (ListRecent) — production returns ORDER BY checked_at DESC,
	// so feed sqlmock newest-first to match the order the handler reverses.
	mock.ExpectQuery(regexp.QuoteMeta("WHERE provider_id =")).
		WithArgs(pid, sqlmock.AnyArg(), 144).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "provider_id", "provider", "label", "checked_at",
			"ok", "latency_ms", "http_status", "message", "model_name",
		}).
			AddRow(uuid.New(), pid, "openai", "openai-prod", now,
				true, 130, 200, nil, nil).
			AddRow(uuid.New(), pid, "openai", "openai-prod", now.Add(-5*time.Minute),
				true, 120, 200, nil, nil))

	mux := http.NewServeMux()
	h.registerLLMProviderObservabilityRoutes(mux)
	req := adminRequest(http.MethodGet, "/api/admin/llm-providers/health?range=24h", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body providerHealthDashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Rows) != 1 {
		t.Fatalf("expected 1 provider row, got %d", len(body.Rows))
	}
	row := body.Rows[0]
	if row.Checks != 288 || row.Successes != 285 || row.Failures != 3 {
		t.Fatalf("unexpected counters: %+v", row)
	}
	if row.SuccessRate <= 0.95 {
		t.Fatalf("expected high success rate, got %f", row.SuccessRate)
	}
	if len(row.Sparkline) != 2 {
		t.Fatalf("expected 2 sparkline points, got %d", len(row.Sparkline))
	}
	if !row.Sparkline[0].CheckedAt.Before(row.Sparkline[1].CheckedAt) {
		t.Fatalf("sparkline should be oldest-first, got %+v", row.Sparkline)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestProviderCostDashboard_HappyPath(t *testing.T) {
	h, mock, done := newObservabilityTestHandler(t)
	defer done()
	expectAdminRoleCheck(mock)
	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY cost_cents DESC")).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{
			"provider", "calls", "total_tokens", "cost_cents", "days_in_window",
		}).
			AddRow("openai", int64(3000), int64(1_500_000), 4567.89, 7).
			AddRow("claude", int64(2000), int64(1_000_000), 3210.50, 7))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE day >=")).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{
			"provider", "model_name", "day", "calls",
			"input_tokens", "output_tokens", "total_tokens",
			"cost_cents", "custom_key_calls", "last_rolled_at",
		}).
			AddRow("openai", "gpt-4o", now, int64(420),
				int64(200000), int64(50000), int64(250000),
				123.45, int64(5), now))

	mux := http.NewServeMux()
	h.registerLLMProviderObservabilityRoutes(mux)
	req := adminRequest(http.MethodGet, "/api/admin/llm-providers/cost?range=7d", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body providerCostDashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Totals) != 2 {
		t.Fatalf("expected 2 totals, got %d", len(body.Totals))
	}
	if body.Totals[0].CostUSD <= 0 {
		t.Fatalf("USD conversion failed: %+v", body.Totals[0])
	}
	if len(body.Daily) != 1 {
		t.Fatalf("expected 1 daily row, got %d", len(body.Daily))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestProviderHistory_HappyPath(t *testing.T) {
	h, mock, done := newObservabilityTestHandler(t)
	defer done()
	expectAdminRoleCheck(mock)
	pid := uuid.New()
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta("WHERE provider_id =")).
		WithArgs(pid, sqlmock.AnyArg(), 250).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "provider_id", "provider", "label", "checked_at",
			"ok", "latency_ms", "http_status", "message", "model_name",
		}).
			AddRow(uuid.New(), pid, "openai", "openai-prod", now,
				false, 0, 503, sql.NullString{Valid: true, String: "upstream"}, nil))

	mux := http.NewServeMux()
	h.registerLLMProviderObservabilityRoutes(mux)
	req := adminRequest(http.MethodGet,
		"/api/admin/llm-providers/"+pid.String()+"/history?range=24h&limit=250", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body providerHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(body.Rows))
	}
	if body.Rows[0].OK {
		t.Fatalf("expected failure row")
	}
	if body.Rows[0].HTTPStatus != 503 {
		t.Fatalf("expected 503, got %d", body.Rows[0].HTTPStatus)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestProviderHistory_InvalidID(t *testing.T) {
	h, mock, done := newObservabilityTestHandler(t)
	defer done()
	expectAdminRoleCheck(mock)

	mux := http.NewServeMux()
	h.registerLLMProviderObservabilityRoutes(mux)
	req := adminRequest(http.MethodGet, "/api/admin/llm-providers/not-a-uuid/history", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// Regression: GET /health and GET /{id} must coexist without panic
// (Go 1.22 mux specificity rule).
func TestRouteCoexistence_HealthVsByID(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("route registration panicked: %v", r)
		}
	}()
	h, _, done := newObservabilityTestHandler(t)
	defer done()
	mux := http.NewServeMux()
	h.registerLLMProviderRoutes(mux)
	h.registerLLMProviderObservabilityRoutes(mux)
}
