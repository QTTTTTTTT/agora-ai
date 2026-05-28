package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fundai/server/internal/api"
)

func TestHandleRegisterRequiresAuth(t *testing.T) {
	svc := newDeviceTokensService(nil)
	req := httptest.NewRequest(http.MethodPost, "/api/devices/register", bytes.NewBufferString(`{"token":"t","platform":"android"}`))
	rec := httptest.NewRecorder()
	svc.handleRegister(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestHandleRegisterValidatesBody(t *testing.T) {
	svc := newDeviceTokensService(nil)
	cases := map[string]struct {
		body string
		want int
	}{
		"empty token":     {`{"platform":"android"}`, http.StatusBadRequest},
		"bad platform":    {`{"token":"x","platform":"linux"}`, http.StatusBadRequest},
		"malformed json":  {`{`, http.StatusBadRequest},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/devices/register", bytes.NewBufferString(c.body))
			ctx := api.WithAuthenticatedUserID(req.Context(), "user-1")
			req = req.WithContext(ctx)
			rec := httptest.NewRecorder()
			svc.handleRegister(rec, req)
			if rec.Code != c.want {
				t.Fatalf("expected %d, got %d body=%s", c.want, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandleRegisterUpserts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := newDeviceTokensService(db)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO device_tokens")).
		WithArgs("user-1", "fcm-token-abc", "android", "1.2.3").
		WillReturnResult(sqlmock.NewResult(1, 1))

	body := bytes.NewBufferString(`{"token":"fcm-token-abc","platform":"android","app_version":"1.2.3"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/devices/register", body)
	ctx := api.WithAuthenticatedUserID(req.Context(), "user-1")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	svc.handleRegister(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestHandleRegisterToleratesMissingTable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := newDeviceTokensService(db)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO device_tokens")).
		WithArgs("user-1", "tok", "android", "").
		WillReturnError(errors.New(`pq: relation "device_tokens" does not exist`))

	body := bytes.NewBufferString(`{"token":"tok","platform":"android"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/devices/register", body)
	ctx := api.WithAuthenticatedUserID(req.Context(), "user-1")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	svc.handleRegister(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestHandleUnregisterRevokes(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := newDeviceTokensService(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE device_tokens")).
		WithArgs("user-1", "tok").
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := bytes.NewBufferString(`{"token":"tok"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/devices/unregister", body)
	ctx := api.WithAuthenticatedUserID(req.Context(), "user-1")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	svc.handleUnregister(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestActiveTokensForFundReturnsEmptyOnError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := newDeviceTokensService(db)
	mock.ExpectQuery("FROM device_tokens").WillReturnError(errors.New("boom"))
	tokens := svc.ActiveTokensForFund(context.Background(), "fund-1")
	if tokens != nil {
		t.Fatalf("expected nil tokens on err, got %v", tokens)
	}
}

func TestIsDeviceTokensMissingDetectsPgRelation(t *testing.T) {
	cases := map[string]bool{
		`pq: relation "device_tokens" does not exist`: true,
		`unknown column embedding`:                    true,
		`syntax error near foo`:                       false,
		``:                                            false,
	}
	for msg, want := range cases {
		var err error
		if msg != "" {
			err = errors.New(msg)
		}
		got := isDeviceTokensMissing(err)
		if got != want {
			t.Fatalf("isDeviceTokensMissing(%q): want %v got %v", msg, want, got)
		}
	}
}
