package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestExchangeWechatCodeParsesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("appid") != "test-appid" || query.Get("secret") != "test-secret" || query.Get("js_code") != "code-123" {
			t.Errorf("unexpected upstream query=%s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"openid":"open-xyz","session_key":"sk","unionid":"u1"}`))
	}))
	defer server.Close()

	cfg := wechatLoginConfig{AppID: "test-appid", AppSecret: "test-secret", Endpoint: server.URL, Timeout: 2 * time.Second}
	got, err := exchangeWechatCode(context.Background(), cfg, "code-123")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if got.OpenID != "open-xyz" {
		t.Fatalf("expected openid open-xyz, got %s", got.OpenID)
	}
}

func TestExchangeWechatCodeSurfacesErrCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"errcode":40029,"errmsg":"invalid code"}`))
	}))
	defer server.Close()

	cfg := wechatLoginConfig{AppID: "a", AppSecret: "s", Endpoint: server.URL, Timeout: time.Second}
	got, err := exchangeWechatCode(context.Background(), cfg, "code")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if got.ErrCode != 40029 || got.ErrMsg != "invalid code" {
		t.Fatalf("unexpected payload %+v", got)
	}
}

func TestHandleWechatLoginCreatesNewUserOnFirstLogin(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	t.Setenv("WECHAT_MINIAPP_APPID", "test-appid")
	t.Setenv("WECHAT_MINIAPP_SECRET", "test-secret")
	t.Setenv("WECHAT_JSCODE_SESSION_URL", "http://invalid.local")

	// Override exchanger to skip the network round-trip.
	original := defaultExchanger
	defaultExchanger = func(_ context.Context, _ wechatLoginConfig, code string) (*wechatJSCodeSessionResponse, error) {
		if code != "code-1" {
			t.Errorf("unexpected code %s", code)
		}
		return &wechatJSCodeSessionResponse{OpenID: "open-new"}, nil
	}
	t.Cleanup(func() { defaultExchanger = original })

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status, COALESCE(password_hash, ''), COALESCE(kyc_status, 'unverified'), COALESCE(kyc_level, 'tier1_basic')
		FROM users
		WHERE wechat_openid = $1
		LIMIT 1
	`)).WithArgs("open-new").
		WillReturnRows(sqlmock.NewRows([]string{}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM users WHERE role = $1`)).
		WithArgs(userRoleSuperAdmin).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO users`)).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "微信用户", "open-new", userStatusActive, userRoleUser).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "display_name", "role", "status", "password_hash", "kyc_status", "kyc_level"}).
			AddRow("user-1", "", "微信用户", userRoleUser, userStatusActive, "", "unverified", "tier1_basic"))
	mock.ExpectCommit()

	svc := &Services{DB: db}
	cfg := &Config{JWTSecret: "test-secret-32-chars-minimum-for-jwt-signer", SessionTTL: time.Hour}

	body, _ := json.Marshal(wechatLoginRequest{Code: "code-1"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/wechat-login", bytes.NewBuffer(body))
	rr := httptest.NewRecorder()
	handleWechatLogin(svc, cfg).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["token"] == nil || payload["token"] == "" {
		t.Fatalf("expected token in response, got %v", payload)
	}
	if payload["user_id"] != "user-1" {
		t.Fatalf("expected user_id user-1, got %v", payload["user_id"])
	}
}

func TestHandleWechatLoginReturnsExistingUser(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	t.Setenv("WECHAT_MINIAPP_APPID", "appid")
	t.Setenv("WECHAT_MINIAPP_SECRET", "secret")

	original := defaultExchanger
	defaultExchanger = func(_ context.Context, _ wechatLoginConfig, _ string) (*wechatJSCodeSessionResponse, error) {
		return &wechatJSCodeSessionResponse{OpenID: "open-existing"}, nil
	}
	t.Cleanup(func() { defaultExchanger = original })

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status, COALESCE(password_hash, ''), COALESCE(kyc_status, 'unverified'), COALESCE(kyc_level, 'tier1_basic')
		FROM users
		WHERE wechat_openid = $1
		LIMIT 1
	`)).WithArgs("open-existing").
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "display_name", "role", "status", "password_hash", "kyc_status", "kyc_level"}).
			AddRow("user-2", "", "微信用户", userRoleUser, userStatusActive, "", "unverified", "tier1_basic"))
	mock.ExpectCommit()

	svc := &Services{DB: db}
	cfg := &Config{JWTSecret: "test-secret-32-chars-minimum-for-jwt-signer", SessionTTL: time.Hour}

	body, _ := json.Marshal(wechatLoginRequest{Code: "code-2"})
	rr := httptest.NewRecorder()
	handleWechatLogin(svc, cfg).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/auth/wechat-login", bytes.NewBuffer(body)))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var payload map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &payload)
	if payload["user_id"] != "user-2" {
		t.Fatalf("expected user_id user-2, got %v", payload["user_id"])
	}
}

func TestHandleWechatLoginRejectsMissingConfig(t *testing.T) {
	db, _ := newMockDB(t)
	defer db.Close()
	// Ensure env vars are cleared for this test.
	os.Unsetenv("WECHAT_MINIAPP_APPID")
	os.Unsetenv("WECHAT_MINIAPP_SECRET")

	cfg := &Config{JWTSecret: "test-secret-32-chars-minimum-for-jwt-signer", SessionTTL: time.Hour}
	body, _ := json.Marshal(wechatLoginRequest{Code: "code-x"})
	rr := httptest.NewRecorder()
	handleWechatLogin(&Services{DB: db}, cfg).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/auth/wechat-login", bytes.NewBuffer(body)))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when wechat config missing, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleWechatLoginSurfacesUpstreamRejection(t *testing.T) {
	db, _ := newMockDB(t)
	defer db.Close()
	t.Setenv("WECHAT_MINIAPP_APPID", "appid")
	t.Setenv("WECHAT_MINIAPP_SECRET", "secret")

	original := defaultExchanger
	defaultExchanger = func(_ context.Context, _ wechatLoginConfig, _ string) (*wechatJSCodeSessionResponse, error) {
		return &wechatJSCodeSessionResponse{ErrCode: 40029, ErrMsg: "invalid code"}, nil
	}
	t.Cleanup(func() { defaultExchanger = original })

	cfg := &Config{JWTSecret: "test-secret-32-chars-minimum-for-jwt-signer", SessionTTL: time.Hour}
	body, _ := json.Marshal(wechatLoginRequest{Code: "bad"})
	rr := httptest.NewRecorder()
	handleWechatLogin(&Services{DB: db}, cfg).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/auth/wechat-login", bytes.NewBuffer(body)))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestExchangeWechatCodeBuildsQueryEscaping(t *testing.T) {
	// Catch-all that just echoes back the parsed query.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"openid":"o"}`))
		// Sanity check: query must round-trip via url.ParseQuery.
		if _, err := url.ParseQuery(r.URL.RawQuery); err != nil {
			t.Errorf("bad query escape: %v", err)
		}
	}))
	defer server.Close()

	cfg := wechatLoginConfig{AppID: "id&", AppSecret: "se cret", Endpoint: server.URL, Timeout: time.Second}
	if _, err := exchangeWechatCode(context.Background(), cfg, "co=de"); err != nil {
		t.Fatalf("exchange: %v", err)
	}
}
