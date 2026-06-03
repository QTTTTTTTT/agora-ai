package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/llm"
	"github.com/fundai/server/internal/modelab"
	"github.com/fundai/server/internal/repository"
	"github.com/google/uuid"
)

// fakeReloader records reload calls so tests can assert the
// admin handler always pushes a fresh snapshot after a mutation.
type fakeReloader struct{ Calls int }

func (f *fakeReloader) ReloadPlatformProviders(ctx context.Context) error {
	f.Calls++
	return nil
}

func newLLMProviderTestHandler(t *testing.T) (*adminHandler, sqlmock.Sqlmock, *fakeReloader, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	router := llm.NewModelRouter(
		map[llm.Provider]string{llm.ProviderOpenAI: "k"},
		nil, nil, nil,
	)
	reloader := &fakeReloader{}
	h := &adminHandler{
		db:                      db,
		auditLogger:             audit.NewDBLogger(db),
		platformLLMProviderRepo: repository.NewPlatformLLMProviderRepo(db),
		modelRouter:             router,
		providerReloader:        reloader,
	}
	return h, mock, reloader, func() { db.Close() }
}

// adminRequest builds an *http.Request whose context carries an
// admin user. Tests that hit requireAdmin must mock the SELECT role
// query downstream because requireAdmin reads users.role per call.
func adminRequest(method, path string, body []byte) *http.Request {
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, bytes.NewReader(body))
	}
	r.Header.Set("Content-Type", "application/json")
	ctx := api.WithAuthenticatedUserID(r.Context(), "11111111-1111-1111-1111-111111111111")
	ctx = api.WithAuthenticatedUserRole(ctx, "admin")
	return r.WithContext(ctx)
}

// expectAdminRoleCheck primes the userIsAdmin SELECT so requireAdmin returns true.
func expectAdminRoleCheck(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT role FROM users WHERE id = $1`)).
		WithArgs("11111111-1111-1111-1111-111111111111").
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("admin"))
}

func TestAdminLLMProviders_List_Empty(t *testing.T) {
	h, mock, _, done := newLLMProviderTestHandler(t)
	defer done()
	expectAdminRoleCheck(mock)
	mock.ExpectQuery(`FROM platform_llm_providers`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "provider", "label", "model_tier", "model_name", "base_url",
			"api_key_encrypted", "api_key_fingerprint",
			"max_tokens", "temperature",
			"input_price_per_1m", "output_price_per_1m", "cost_per_1m",
			"status", "is_platform_default",
			"last_health_check_at", "last_health_check_result",
			"source", "created_at", "updated_at", "created_by", "updated_by",
		}))

	mux := http.NewServeMux()
	h.registerLLMProviderRoutes(mux)
	req := adminRequest(http.MethodGet, "/api/admin/llm-providers", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Providers         []llmProviderDTO `json:"providers"`
		RouterActiveKeys  map[string]bool  `json:"router_active_keys"`
		ReloadGeneration  uint64           `json:"reload_generation"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if len(body.Providers) != 0 {
		t.Fatalf("expected empty providers, got %d", len(body.Providers))
	}
	if body.RouterActiveKeys == nil {
		t.Fatalf("expected router_active_keys present")
	}
	if !body.RouterActiveKeys["openai"] {
		t.Fatalf("expected router_active_keys[openai] true, got %v", body.RouterActiveKeys)
	}
}

func TestAdminLLMProviders_Upsert_Create_HappyPath(t *testing.T) {
	t.Setenv("MODEL_CONFIG_API_KEY_SECRET", "test-secret-32-chars-long-enough-for-aes")
	h, mock, reloader, done := newLLMProviderTestHandler(t)
	defer done()
	expectAdminRoleCheck(mock)

	rowID := uuid.New()
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO platform_llm_providers`)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "provider", "label", "model_tier", "model_name", "base_url",
			"api_key_encrypted", "api_key_fingerprint",
			"max_tokens", "temperature",
			"input_price_per_1m", "output_price_per_1m", "cost_per_1m",
			"status", "is_platform_default",
			"last_health_check_at", "last_health_check_result",
			"source", "created_at", "updated_at", "created_by", "updated_by",
		}).AddRow(
			rowID, "openai", "openai-prod", nil, "gpt-4o", "https://api.openai.com/v1",
			"ENCRYPTED", "abc12345",
			4096, 0.7,
			nil, nil, nil,
			"active", false,
			nil, nil,
			"admin", time.Now(), time.Now(), nil, nil,
		))
	// auditLogger.LogMutation inserts a row.
	mock.ExpectExec(`INSERT INTO admin_change_log`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	mux := http.NewServeMux()
	h.registerLLMProviderRoutes(mux)
	payload, _ := json.Marshal(upsertLLMProviderRequest{
		Provider:  "openai",
		Label:     "openai-prod",
		ModelName: "gpt-4o",
		BaseURL:   "https://api.openai.com/v1",
		APIKey:    "sk-test-xyz",
	})
	req := adminRequest(http.MethodPut, "/api/admin/llm-providers", payload)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if reloader.Calls != 1 {
		t.Fatalf("expected reloader called once, got %d", reloader.Calls)
	}
	var dto llmProviderDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.APIKeyMaskedPreview != "sk-…abc12345" {
		t.Fatalf("masked preview wrong: %q", dto.APIKeyMaskedPreview)
	}
	if !dto.APIKeyConfigured {
		t.Fatalf("api_key_configured should be true")
	}
}

func TestAdminLLMProviders_Upsert_ValidationReturns422(t *testing.T) {
	t.Setenv("MODEL_CONFIG_API_KEY_SECRET", "test-secret-32-chars-long-enough-for-aes")
	h, mock, reloader, done := newLLMProviderTestHandler(t)
	defer done()
	expectAdminRoleCheck(mock)

	mux := http.NewServeMux()
	h.registerLLMProviderRoutes(mux)
	payload, _ := json.Marshal(upsertLLMProviderRequest{
		Provider:  "nope-not-a-provider",
		Label:     "x",
		ModelName: "y",
		BaseURL:   "z",
		APIKey:    "k",
	})
	req := adminRequest(http.MethodPut, "/api/admin/llm-providers", payload)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d body=%s", rec.Code, rec.Body.String())
	}
	if reloader.Calls != 0 {
		t.Fatalf("expected no reload on validation reject, got %d", reloader.Calls)
	}
}

func TestAdminLLMProviders_Delete_NotFound(t *testing.T) {
	h, mock, reloader, done := newLLMProviderTestHandler(t)
	defer done()
	expectAdminRoleCheck(mock)
	id := uuid.New()
	// Pre-delete read for audit; returns no rows.
	mock.ExpectQuery(`FROM platform_llm_providers`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{}))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM platform_llm_providers WHERE id = $1`)).
		WithArgs(id).
		WillReturnResult(sqlmock.NewResult(0, 0))

	mux := http.NewServeMux()
	h.registerLLMProviderRoutes(mux)
	req := adminRequest(http.MethodDelete, "/api/admin/llm-providers/"+id.String(), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rec.Code, rec.Body.String())
	}
	if reloader.Calls != 0 {
		t.Fatalf("expected no reload on 404, got %d", reloader.Calls)
	}
}

func TestAdminLLMProviders_Test_MissingFields(t *testing.T) {
	h, mock, _, done := newLLMProviderTestHandler(t)
	defer done()
	expectAdminRoleCheck(mock)
	mux := http.NewServeMux()
	h.registerLLMProviderRoutes(mux)
	payload, _ := json.Marshal(testLLMProviderRequest{Provider: "openai"})
	req := adminRequest(http.MethodPost, "/api/admin/llm-providers/test", payload)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on missing fields, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMissingProviderKeys_AcceptsRouterKey(t *testing.T) {
	router := llm.NewModelRouter(map[llm.Provider]string{
		llm.ProviderOpenAI: "k-openai",
	}, nil, nil, nil)
	h := &adminHandler{modelRouter: router}
	missing := h.missingProviderKeys(context.Background(), nil)
	if len(missing) != 0 {
		t.Fatalf("empty arms: got %v", missing)
	}
}

func TestMissingProviderKeys_AcceptsActiveDBRow(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	router := llm.NewModelRouter(map[llm.Provider]string{}, nil, nil, nil)
	h := &adminHandler{
		modelRouter:             router,
		platformLLMProviderRepo: repository.NewPlatformLLMProviderRepo(db),
	}

	mock.ExpectQuery(`FROM platform_llm_providers`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "provider", "label", "model_tier", "model_name", "base_url",
			"api_key_encrypted", "api_key_fingerprint", "max_tokens", "temperature",
			"input_price_per_1m", "output_price_per_1m", "cost_per_1m",
			"status", "is_platform_default", "last_health_check_at",
			"last_health_check_result", "source", "created_at", "updated_at",
			"created_by", "updated_by",
		}).AddRow(
			uuid.New(), "claude", "claude-prod", nil, "claude-3-5-sonnet", "https://api.anthropic.com",
			"ENC", "abc12345", 4096, 0.7, nil, nil, nil,
			"active", false, nil, nil,
			"admin", time.Now(), time.Now(), nil, nil,
		))

	missing := h.missingProviderKeys(context.Background(), armsFromProviders("claude"))
	if len(missing) != 0 {
		t.Fatalf("expected accept (active DB row), got %v", missing)
	}
}

func TestMissingProviderKeys_RejectsUnknown(t *testing.T) {
	router := llm.NewModelRouter(map[llm.Provider]string{
		llm.ProviderOpenAI: "k",
	}, nil, nil, nil)
	h := &adminHandler{modelRouter: router}
	missing := h.missingProviderKeys(context.Background(), armsFromProviders("claude", "deepseek", "openai"))
	if len(missing) != 2 {
		t.Fatalf("expected 2 missing, got %v", missing)
	}
}

func armsFromProviders(providers ...string) []modelab.ArmConfig {
	out := make([]modelab.ArmConfig, len(providers))
	for i, p := range providers {
		out[i] = modelab.ArmConfig{
			Name:      "arm-" + p,
			Provider:  llm.Provider(p),
			ModelName: "test-model",
		}
	}
	return out
}

func TestMaskKeyPreview_NeverRevealsPlaintext(t *testing.T) {
	if maskKeyPreview("abcdef01") != "sk-…abcdef01" {
		t.Fatalf("got %q", maskKeyPreview("abcdef01"))
	}
	if maskKeyPreview("") != "" {
		t.Fatalf("empty fingerprint should produce empty preview")
	}
}
