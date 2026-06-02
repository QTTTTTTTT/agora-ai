package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fundai/server/internal/api"
)

func newAdminFXEnv(t *testing.T) (*adminHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	h := &adminHandler{db: db}
	return h, mock, func() { _ = db.Close() }
}

func expectAdminRoleLookup(mock sqlmock.Sqlmock, userID, role string) {
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT role FROM users WHERE id = $1`)).
		WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow(role))
}

func TestAdminFX_List_Unauthenticated(t *testing.T) {
	h, _, cleanup := newAdminFXEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/fx-rates", nil)
	rr := httptest.NewRecorder()
	h.handleFXRates(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestAdminFX_List_Forbidden(t *testing.T) {
	h, mock, cleanup := newAdminFXEnv(t)
	defer cleanup()
	const userID = "u-1"
	expectAdminRoleLookup(mock, userID, "user")
	req := httptest.NewRequest(http.MethodGet, "/api/admin/fx-rates", nil).
		WithContext(api.WithAuthenticatedUserID(httptest.NewRequest(http.MethodGet, "/", nil).Context(), userID))
	rr := httptest.NewRecorder()
	h.handleFXRates(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminFX_List_HappyPath(t *testing.T) {
	h, mock, cleanup := newAdminFXEnv(t)
	defer cleanup()
	const userID = "u-1"
	expectAdminRoleLookup(mock, userID, "admin")
	now := time.Now().UTC()
	mock.ExpectQuery("FROM fx_rates").
		WillReturnRows(sqlmock.NewRows([]string{
			"base_currency", "quote_currency", "rate", "rate_at", "source",
		}).AddRow("USD", "CNY", 7.18, now, "yahoo"))

	req := httptest.NewRequest(http.MethodGet, "/api/admin/fx-rates", nil).
		WithContext(api.WithAuthenticatedUserID(httptest.NewRequest(http.MethodGet, "/", nil).Context(), userID))
	rr := httptest.NewRecorder()
	h.handleFXRates(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Rates      []fxAdminWire `json:"rates"`
		Currencies []string      `json:"currencies"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Rates) != 1 || body.Rates[0].Rate != 7.18 {
		t.Errorf("rates = %+v", body.Rates)
	}
	if len(body.Currencies) == 0 {
		t.Error("expected currencies populated")
	}
}

func TestAdminFX_Upsert_HappyPath(t *testing.T) {
	h, mock, cleanup := newAdminFXEnv(t)
	defer cleanup()
	const userID = "u-1"
	expectAdminRoleLookup(mock, userID, "admin")
	mock.ExpectQuery("INSERT INTO fx_rates").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("rate-1"))

	body, _ := json.Marshal(map[string]any{
		"base":   "USD",
		"quote":  "CNY",
		"rate":   7.20,
		"source": "manual",
		"note":   "operator override",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/fx-rates", bytes.NewReader(body)).
		WithContext(api.WithAuthenticatedUserID(httptest.NewRequest(http.MethodGet, "/", nil).Context(), userID))
	rr := httptest.NewRecorder()
	h.handleFXRates(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "rate-1") {
		t.Errorf("body = %s", rr.Body.String())
	}
}

func TestAdminFX_Upsert_RejectsBadCurrency(t *testing.T) {
	h, mock, cleanup := newAdminFXEnv(t)
	defer cleanup()
	const userID = "u-1"
	expectAdminRoleLookup(mock, userID, "admin")
	body, _ := json.Marshal(map[string]any{
		"base":  "USD",
		"quote": "BTC",
		"rate":  1.0,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/fx-rates", bytes.NewReader(body)).
		WithContext(api.WithAuthenticatedUserID(httptest.NewRequest(http.MethodGet, "/", nil).Context(), userID))
	rr := httptest.NewRecorder()
	h.handleFXRates(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminFX_Upsert_RejectsZeroRate(t *testing.T) {
	h, mock, cleanup := newAdminFXEnv(t)
	defer cleanup()
	const userID = "u-1"
	expectAdminRoleLookup(mock, userID, "admin")
	body, _ := json.Marshal(map[string]any{
		"base":  "USD",
		"quote": "CNY",
		"rate":  0,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/fx-rates", bytes.NewReader(body)).
		WithContext(api.WithAuthenticatedUserID(httptest.NewRequest(http.MethodGet, "/", nil).Context(), userID))
	rr := httptest.NewRecorder()
	h.handleFXRates(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminFX_Upsert_RejectsBadSource(t *testing.T) {
	h, mock, cleanup := newAdminFXEnv(t)
	defer cleanup()
	const userID = "u-1"
	expectAdminRoleLookup(mock, userID, "admin")
	body, _ := json.Marshal(map[string]any{
		"base":   "USD",
		"quote":  "CNY",
		"rate":   7.0,
		"source": "yahoo",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/fx-rates", bytes.NewReader(body)).
		WithContext(api.WithAuthenticatedUserID(httptest.NewRequest(http.MethodGet, "/", nil).Context(), userID))
	rr := httptest.NewRecorder()
	h.handleFXRates(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "invalid_source") {
		t.Errorf("body = %s", rr.Body.String())
	}
}

func TestAdminFX_MethodNotAllowed(t *testing.T) {
	h, _, cleanup := newAdminFXEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodDelete, "/api/admin/fx-rates", nil)
	rr := httptest.NewRecorder()
	h.handleFXRates(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d", rr.Code)
	}
}
