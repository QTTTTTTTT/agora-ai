package main

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/repository"
	"github.com/google/uuid"
)

func newFundOverrideTestHandler(t *testing.T) (*fundLLMOverridesHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	h := &fundLLMOverridesHandler{
		overrideRepo: repository.NewFundLLMOverrideRepo(db),
		providerRepo: repository.NewPlatformLLMProviderRepo(db),
		fundRepo:     repository.NewFundRepo(db),
		companyRepo:  repository.NewFundCompanyRepo(db),
	}
	return h, mock, func() { db.Close() }
}

func fundOverrideReq(t *testing.T, method, path string, body []byte, userID string) *http.Request {
	t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, bytes.NewReader(body))
	}
	r.Header.Set("Content-Type", "application/json")
	ctx := api.WithAuthenticatedUserID(r.Context(), userID)
	return r.WithContext(ctx)
}

// expectFundAuth primes the SELECTs for authorizeFundAccess: get
// fund, get company, check owner == userID.
func expectFundAuth(t *testing.T, mock sqlmock.Sqlmock, fundID, companyID, userID string) {
	t.Helper()
	mock.ExpectQuery(regexp.QuoteMeta("FROM funds")).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "company_id", "name", "description", "trading_mode",
			"initial_capital", "current_capital", "total_assets",
			"nav", "status", "config", "created_at", "updated_at",
		}).AddRow(fundID, companyID, "Test Fund", "", "live",
			100000.0, 100000.0, 100000.0, 100000.0, "active",
			[]byte("{}"), time.Now(), time.Now()))
	// FundCompanyRepo.GetByID — derive its column shape by querying it.
	mock.ExpectQuery(regexp.QuoteMeta("FROM fund_companies")).
		WithArgs(companyID).
		WillReturnRows(fundCompanyRows().AddRow(fundCompanyValues(companyID, userID)...))
}

// fundCompanyRows / fundCompanyValues mirror the column shape the
// FundCompanyRepo.GetByID scans. Adjust here if the schema evolves.
func fundCompanyRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "owner_user_id", "name", "description",
		"created_at", "updated_at",
	})
}
func fundCompanyValues(companyID, ownerID string) []driver.Value {
	return []driver.Value{
		companyID, ownerID, "Test Co", "",
		time.Now(), time.Now(),
	}
}

func TestFundLLMOverridesHandler_List_Unauthorized(t *testing.T) {
	h, _, done := newFundOverrideTestHandler(t)
	defer done()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	// No auth context.
	req := httptest.NewRequest(http.MethodGet, "/api/funds/"+uuid.New().String()+"/llm-overrides", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestFundLLMOverridesHandler_Upsert_RejectsMissingProvider(t *testing.T) {
	// This test bypasses fundRepo / companyRepo by skipping the
	// auth pre-check (the repo wiring expects real schemas). We
	// directly call the upsert path of the override repo which
	// guards against missing provider — handler returns 422.
	h, mock, done := newFundOverrideTestHandler(t)
	defer done()
	fundID := uuid.New().String()
	companyID := uuid.New().String()
	userID := uuid.New().String()
	expectFundAuth(t, mock, fundID, companyID, userID)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM platform_llm_providers WHERE provider")).
		WithArgs("openai").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	body, _ := json.Marshal(upsertFundLLMOverrideRequest{
		Provider: "openai",
		Enabled:  true,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	req := fundOverrideReq(t, http.MethodPut, "/api/funds/"+fundID+"/llm-overrides", body, userID)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestFundLLMOverridesHandler_Delete_BadID(t *testing.T) {
	h, mock, done := newFundOverrideTestHandler(t)
	defer done()
	fundID := uuid.New().String()
	companyID := uuid.New().String()
	userID := uuid.New().String()
	expectFundAuth(t, mock, fundID, companyID, userID)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	req := fundOverrideReq(t, http.MethodDelete,
		"/api/funds/"+fundID+"/llm-overrides/not-a-uuid", nil, userID)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestFundLLMOverridesHandler_List_HappyPath(t *testing.T) {
	h, mock, done := newFundOverrideTestHandler(t)
	defer done()
	fundID := uuid.New().String()
	companyID := uuid.New().String()
	userID := uuid.New().String()
	rowID := uuid.New()
	now := time.Now()

	expectFundAuth(t, mock, fundID, companyID, userID)
	fundUUID, _ := uuid.Parse(fundID)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE fund_id = $1")).
		WithArgs(fundUUID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "agent_id", "role", "model_tier",
			"provider", "label", "model_name",
			"enabled", "note",
			"created_at", "updated_at", "created_by", "updated_by",
		}).
			AddRow(rowID, fundUUID, nil, nil, nil,
				"openai", sql.NullString{Valid: true, String: "openai-prod"}, nil,
				true, nil, now, now, nil, nil))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE provider=$1 AND label=$2")).
		WithArgs("openai", "openai-prod").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "provider", "label", "model_tier", "model_name", "base_url",
			"api_key_encrypted", "api_key_fingerprint",
			"max_tokens", "temperature",
			"input_price_per_1m", "output_price_per_1m", "cost_per_1m",
			"status", "is_platform_default",
			"last_health_check_at", "last_health_check_result",
			"source", "created_at", "updated_at", "created_by", "updated_by",
		}).
			AddRow(uuid.New(), "openai", "openai-prod", nil, "gpt-4o",
				"https://api.openai.com/v1", "enc", "fp123",
				4096, 0.7, nil, nil, nil, "active", true, nil, nil,
				"manual", now, now, nil, nil))

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	req := fundOverrideReq(t, http.MethodGet,
		"/api/funds/"+fundID+"/llm-overrides", nil, userID)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Overrides []fundLLMOverrideDTO `json:"overrides"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Overrides) != 1 {
		t.Fatalf("expected 1 override, got %d", len(body.Overrides))
	}
	row := body.Overrides[0]
	if row.EffectiveProvider != "openai" || row.EffectiveLabel != "openai-prod" {
		t.Fatalf("effective fields not resolved: %+v", row)
	}
	if row.EffectiveModelName != "gpt-4o" {
		t.Fatalf("effective model name expected gpt-4o, got %q", row.EffectiveModelName)
	}
}

// Smoke check the local sentinel mapping — make sure the writer
// produces the expected status codes.
func TestWriteFundOverrideAuthError_StatusMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"unauthorized", errUnauthorizedFundOverride, http.StatusUnauthorized},
		{"bad fund id", errBadFundIDFundOverride, http.StatusBadRequest},
		{"forbidden", api.ErrForbidden, http.StatusForbidden},
		{"not found", api.ErrNotFound, http.StatusNotFound},
		{"bad input", api.ErrBadInput, http.StatusBadRequest},
		{"opaque", context.Canceled, http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeFundOverrideAuthError(rec, c.err)
			if rec.Code != c.want {
				t.Fatalf("status: got %d want %d", rec.Code, c.want)
			}
		})
	}
}
