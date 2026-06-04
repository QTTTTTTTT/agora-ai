// fund_llm_catalog_handler_test.go — auth + projection guards for the
// fund-scoped LLM catalog endpoint.

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

func newFundCatalogTestHandler(t *testing.T) (*fundLLMCatalogHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	h := &fundLLMCatalogHandler{
		providerRepo: repository.NewPlatformLLMProviderRepo(db),
		fundRepo:     repository.NewFundRepo(db),
		companyRepo:  repository.NewFundCompanyRepo(db),
	}
	return h, mock, func() { db.Close() }
}

func TestFundLLMCatalog_Unauthenticated(t *testing.T) {
	h, _, done := newFundCatalogTestHandler(t)
	defer done()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet,
		"/api/funds/"+uuid.New().String()+"/llm-catalog", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestFundLLMCatalog_HappyPath_OmitsSecretFields(t *testing.T) {
	h, mock, done := newFundCatalogTestHandler(t)
	defer done()
	fundID := uuid.New().String()
	companyID := uuid.New().String()
	userID := uuid.New().String()
	// Reuse the override handler test's auth fixture: the catalog
	// uses the same authorizeFundAccess chain so the SELECT order
	// is identical.
	expectFundAuth(t, mock, fundID, companyID, userID)

	// Two active rows: one platform default + one alternative.
	// Disabled rows must be filtered server-side via Status='active'
	// — covered implicitly by the WHERE clause in ListAll.
	cols := []string{
		"id", "provider", "label", "model_tier", "model_name",
		"base_url", "api_key_encrypted", "api_key_fingerprint",
		"max_tokens", "temperature",
		"input_price_per_1m", "output_price_per_1m", "cost_per_1m",
		"status", "is_platform_default",
		"last_health_check_at", "last_health_check_result",
		"source", "created_at", "updated_at", "created_by", "updated_by",
	}
	rows := sqlmock.NewRows(cols).
		AddRow(uuid.New().String(), "openai", "openai-prod",
			"critical", "gpt-4o",
			"https://api.openai.com", "ENC", "fp",
			4096, 0.0,
			nil, nil, nil,
			"active", true,
			nil, nil,
			"env", time.Now(), time.Now(), nil, nil).
		AddRow(uuid.New().String(), "claude", "claude-default",
			"critical", "claude-opus-4",
			"https://api.anthropic.com", "ENC", "fp2",
			4096, 0.0,
			nil, nil, nil,
			"active", false,
			nil, nil,
			"env", time.Now(), time.Now(), nil, nil)
	mock.ExpectQuery(regexp.QuoteMeta("FROM platform_llm_providers")).
		WithArgs("active").
		WillReturnRows(rows)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	req := fundOverrideReq(t, http.MethodGet,
		"/api/funds/"+fundID+"/llm-catalog", nil, userID)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(payload.Providers))
	}
	// Pin the platform-default ordering so the UI can pick the
	// first row as a sane suggestion. Two-element list, default
	// row must come first OR be flagged via is_platform_default.
	first := payload.Providers[0]
	if first["provider"] != "openai" {
		t.Fatalf("expected first row openai, got %v", first["provider"])
	}
	if first["is_platform_default"] != true {
		t.Fatalf("expected is_platform_default=true, got %v", first["is_platform_default"])
	}
	// Critical: secret fields MUST NOT be in the JSON. Both raw
	// ciphertext and the fingerprint are sensitive (the latter
	// proves which key is in use across deploys → ops surface).
	for i, row := range payload.Providers {
		for _, secret := range []string{"api_key_encrypted", "api_key_fingerprint", "base_url"} {
			if _, present := row[secret]; present {
				t.Fatalf("row %d leaks secret field %q", i, secret)
			}
		}
	}
}
