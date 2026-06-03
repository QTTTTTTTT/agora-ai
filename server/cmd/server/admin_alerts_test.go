package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/repository"
)

func newAlertTestHandler(t *testing.T) (*adminHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	h := &adminHandler{
		db:             db,
		alertEventRepo: repository.NewAlertEventRepo(db),
		auditLogger:    audit.NewDBLogger(db),
	}
	return h, mock, func() { db.Close() }
}

const testWebhookSecret = "supersecret"

func setWebhookSecret(t *testing.T) {
	t.Helper()
	old := os.Getenv("FUNDAI_ALERT_WEBHOOK_SECRET")
	os.Setenv("FUNDAI_ALERT_WEBHOOK_SECRET", testWebhookSecret)
	t.Cleanup(func() { os.Setenv("FUNDAI_ALERT_WEBHOOK_SECRET", old) })
}

func TestAdminAlerts_Webhook_RejectsMissingSecret(t *testing.T) {
	h, _, done := newAlertTestHandler(t)
	defer done()
	os.Unsetenv("FUNDAI_ALERT_WEBHOOK_SECRET")
	mux := http.NewServeMux()
	h.registerAlertAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/alerts/webhook", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when secret missing, got %d", rec.Code)
	}
}

func TestAdminAlerts_Webhook_RejectsWrongBearer(t *testing.T) {
	h, _, done := newAlertTestHandler(t)
	defer done()
	setWebhookSecret(t)
	mux := http.NewServeMux()
	h.registerAlertAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/alerts/webhook", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer wrong-secret")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAdminAlerts_Webhook_HappyPath(t *testing.T) {
	h, mock, done := newAlertTestHandler(t)
	defer done()
	setWebhookSecret(t)

	mock.ExpectQuery(`INSERT INTO admin_alert_events`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("alert-1"))

	mux := http.NewServeMux()
	h.registerAlertAdminRoutes(mux)

	payload := `{
		"version":"4","status":"firing",
		"alerts":[{
			"fingerprint":"fp-1","status":"firing",
			"labels":{"alertname":"FundAIPMDecisionFallbackRateHigh","severity":"warning","component":"pm_decision"},
			"annotations":{"summary":"PM fallback rate high","description":"detail"},
			"startsAt":"2026-06-03T00:00:00Z","endsAt":"0001-01-01T00:00:00Z"
		}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/alerts/webhook", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+testWebhookSecret)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Ingested int `json:"ingested"`
		Deduped  int `json:"deduped"`
		Failed   int `json:"failed"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body.Ingested != 1 {
		t.Fatalf("expected ingested=1, got %+v", body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestAdminAlerts_Webhook_DedupCountsAsSuccess(t *testing.T) {
	h, mock, done := newAlertTestHandler(t)
	defer done()
	setWebhookSecret(t)

	// First call returns the new row id, second is the conflict path
	// (zero rows returned by RETURNING).
	mock.ExpectQuery(`INSERT INTO admin_alert_events`).
		WillReturnRows(sqlmock.NewRows([]string{"id"})) // dedup hit

	mux := http.NewServeMux()
	h.registerAlertAdminRoutes(mux)

	payload := `{"version":"4","status":"firing","alerts":[{
		"fingerprint":"fp-1","status":"firing",
		"labels":{"alertname":"X","severity":"warning"},
		"annotations":{},
		"startsAt":"2026-06-03T00:00:00Z","endsAt":"0001-01-01T00:00:00Z"
	}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/alerts/webhook", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+testWebhookSecret)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Ingested int `json:"ingested"`
		Deduped  int `json:"deduped"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body.Deduped != 1 {
		t.Fatalf("expected dedup count, got %+v", body)
	}
}

func TestAdminAlerts_List_RequiresAdmin(t *testing.T) {
	h, _, done := newAlertTestHandler(t)
	defer done()
	mux := http.NewServeMux()
	h.registerAlertAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/alerts", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAdminAlerts_Acknowledge_HappyPath(t *testing.T) {
	h, mock, done := newAlertTestHandler(t)
	defer done()
	userID := expectAdminGate(t, mock, adminRoleSuperAdmin)

	mock.ExpectExec(`UPDATE admin_alert_events`).
		WithArgs("alert-1", userID, "false positive").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mux := http.NewServeMux()
	h.registerAlertAdminRoutes(mux)

	req := httptest.NewRequest(http.MethodPatch, "/api/admin/alerts/alert-1/ack",
		strings.NewReader(`{"note":"false positive"}`))
	req.Header.Set("Content-Type", "application/json")
	req = withAdminAuth(req, userID)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestStrconvAtoiBounded(t *testing.T) {
	cases := []struct {
		raw     string
		lo, hi  int
		want    int
		wantErr bool
	}{
		{"50", 1, 500, 50, false},
		{"   ", 1, 500, 0, true},
		{"abc", 1, 500, 0, true},
		{"0", 1, 500, 1, false},
		{"99999", 1, 500, 500, false},
		{"-5", 1, 500, 1, false},
	}
	for _, c := range cases {
		got, err := strconvAtoiBounded(c.raw, c.lo, c.hi)
		if c.wantErr && err == nil {
			t.Fatalf("raw=%q: want err, got %d", c.raw, got)
		}
		if !c.wantErr && err != nil {
			t.Fatalf("raw=%q: unexpected err %v", c.raw, err)
		}
		if !c.wantErr && got != c.want {
			t.Fatalf("raw=%q: got %d want %d", c.raw, got, c.want)
		}
	}
}
