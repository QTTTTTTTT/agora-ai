package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sprint 2B — WeChat miniapp single-sign-on.
//
// The miniapp calls `wx.login()` to get a one-time code (≈ 30s TTL),
// then POSTs it here. We exchange the code with WeChat's
// `jscode2session` endpoint, upsert the resulting openid against our
// users table (so the same WeChat user gets a stable internal id),
// and return the same JWT envelope the email/password login uses.
//
// The endpoint deliberately does NOT collect email or username at sign
// up — WeChat doesn't hand those back, and per Sprint 2B's plan we want
// the user inside the app immediately. A follow-up screen prompts them
// to bind an email later (Account / Security flow from Sprint 2A).

const (
	wechatJSCodeSessionURL = "https://api.weixin.qq.com/sns/jscode2session"
	wechatHTTPTimeout      = 8 * time.Second
)

type wechatLoginRequest struct {
	Code string `json:"code"`
}

type wechatLoginConfig struct {
	AppID     string
	AppSecret string
	Endpoint  string // override only in tests
	Timeout   time.Duration
}

// loadWechatLoginConfig pulls the AppID/AppSecret from env. We keep the
// raw config on Config so callers (and tests) can override via fields.
func loadWechatLoginConfig() wechatLoginConfig {
	return wechatLoginConfig{
		AppID:     strings.TrimSpace(firstEnv("WECHAT_MINIAPP_APPID", "")),
		AppSecret: strings.TrimSpace(firstEnv("WECHAT_MINIAPP_SECRET", "")),
		Endpoint:  strings.TrimSpace(firstEnv("WECHAT_JSCODE_SESSION_URL", wechatJSCodeSessionURL)),
		Timeout:   envDuration("WECHAT_LOGIN_TIMEOUT", wechatHTTPTimeout),
	}
}

func (c wechatLoginConfig) Enabled() bool {
	return c.AppID != "" && c.AppSecret != ""
}

// wechatJSCodeSessionResponse mirrors the upstream payload we care
// about. `errcode != 0` signals a failure (expired code, mismatched
// AppID, etc.) and `errmsg` carries a human-readable hint.
type wechatJSCodeSessionResponse struct {
	OpenID     string `json:"openid"`
	SessionKey string `json:"session_key"`
	UnionID    string `json:"unionid"`
	ErrCode    int    `json:"errcode"`
	ErrMsg     string `json:"errmsg"`
}

// wechatSessionExchanger is the seam we override in tests; production
// uses the real HTTP exchange.
type wechatSessionExchanger func(ctx context.Context, cfg wechatLoginConfig, code string) (*wechatJSCodeSessionResponse, error)

// defaultExchanger is reassignable to keep test injection simple.
var defaultExchanger wechatSessionExchanger = exchangeWechatCode

func handleWechatLogin(svc *Services, cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := ensureRequestID(w, r)
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed", "request_id": requestID})
			return
		}
		if svc == nil || svc.DB == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "service unavailable", "request_id": requestID})
			return
		}

		var input wechatLoginRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body", "request_id": requestID})
			return
		}
		code := strings.TrimSpace(input.Code)
		if code == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing code", "detail": "缺少 wx.login() code 参数。", "request_id": requestID})
			return
		}

		wxCfg := loadWechatLoginConfig()
		if !wxCfg.Enabled() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error":      "wechat login not configured",
				"detail":     "服务端缺少 WECHAT_MINIAPP_APPID / WECHAT_MINIAPP_SECRET 配置。",
				"request_id": requestID,
			})
			return
		}

		session, err := defaultExchanger(r.Context(), wxCfg, code)
		if err != nil {
			slog.Warn("wechat exchange failed", "request_id", requestID, "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "wechat exchange failed", "detail": err.Error(), "request_id": requestID})
			return
		}
		if session.ErrCode != 0 || strings.TrimSpace(session.OpenID) == "" {
			slog.Warn("wechat session rejected", "request_id", requestID, "errcode", session.ErrCode, "errmsg", session.ErrMsg)
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error":      "wechat session rejected",
				"detail":     fmt.Sprintf("wx errcode=%d msg=%s", session.ErrCode, session.ErrMsg),
				"request_id": requestID,
			})
			return
		}

		user, err := upsertWechatUser(r.Context(), svc.DB, session.OpenID, session.UnionID)
		if err != nil {
			slog.Error("upsert wechat user", "request_id", requestID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "upsert wechat user", "request_id": requestID})
			return
		}

		// recordSuccessfulLogin keeps the unified login tracking
		// columns (last_login_at, locked_until=0) consistent across
		// email/password and miniapp flows.
		recordSuccessfulLogin(r.Context(), svc.DB, user.ID)
		writeAuthSuccess(w, cfg, requestID, user)
	}
}

// exchangeWechatCode talks to https://api.weixin.qq.com/sns/jscode2session.
// We keep the parameters explicit so tests can substitute the entire
// function for an in-memory implementation.
func exchangeWechatCode(ctx context.Context, cfg wechatLoginConfig, code string) (*wechatJSCodeSessionResponse, error) {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = wechatJSCodeSessionURL
	}
	q := url.Values{}
	q.Set("appid", cfg.AppID)
	q.Set("secret", cfg.AppSecret)
	q.Set("js_code", code)
	q.Set("grant_type", "authorization_code")

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = wechatHTTPTimeout
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("wechat request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if err != nil {
		return nil, fmt.Errorf("wechat read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("wechat status %d body=%s", resp.StatusCode, string(body))
	}
	var parsed wechatJSCodeSessionResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("wechat decode: %w (body=%s)", err, string(body))
	}
	return &parsed, nil
}

// upsertWechatUser atomically finds or creates the user keyed on
// wechat_openid. We give first-time users a placeholder username
// (`wx_<openid_prefix>`) so the existing UNIQUE constraint stays
// satisfied; they can rename / bind an email later through the
// account-security page.
func upsertWechatUser(ctx context.Context, db *sql.DB, openid, unionid string) (*authenticatedUser, error) {
	if db == nil {
		return nil, errors.New("database unavailable")
	}
	openid = strings.TrimSpace(openid)
	if openid == "" {
		return nil, errors.New("empty openid")
	}

	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var user authenticatedUser
	err = tx.QueryRowContext(ctx, `
		SELECT id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status, COALESCE(password_hash, ''), COALESCE(kyc_status, 'unverified'), COALESCE(kyc_level, 'tier1_basic'), COALESCE(preferred_language, 'zh-CN')
		FROM users
		WHERE wechat_openid = $1
		LIMIT 1
	`, openid).Scan(&user.ID, &user.Email, &user.DisplayName, &user.Role, &user.Status, &user.PasswordHash, &user.KYCStatus, &user.KYCLevel, &user.PreferredLanguage)
	if err == nil {
		if !strings.EqualFold(user.Status, userStatusActive) {
			return nil, errUserNotFoundOrInactive
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return &user, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// First-time miniapp login. Inherit super_admin role only if this
	// is the very first account in the system (matches handleRegister
	// behaviour for parity).
	role := userRoleUser
	var superCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = $1`, userRoleSuperAdmin).Scan(&superCount); err != nil {
		return nil, err
	}
	if superCount == 0 {
		role = userRoleSuperAdmin
	}

	prefix := openid
	if len(prefix) > 10 {
		prefix = prefix[:10]
	}
	userID := uuid.NewString()
	username := "wx_" + prefix + "_" + userID[:6]
	displayName := "微信用户"

	if err := tx.QueryRowContext(ctx, `
		INSERT INTO users (id, username, display_name, wechat_openid, status, role)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status, COALESCE(password_hash, ''), COALESCE(kyc_status, 'unverified'), COALESCE(kyc_level, 'tier1_basic'), COALESCE(preferred_language, 'zh-CN')
	`, userID, username, displayName, openid, userStatusActive, role).Scan(
		&user.ID, &user.Email, &user.DisplayName, &user.Role, &user.Status, &user.PasswordHash, &user.KYCStatus, &user.KYCLevel, &user.PreferredLanguage,
	); err != nil {
		if isUniqueViolation(err) {
			// Race: another concurrent request created the row. Re-fetch and return.
			if err := tx.Rollback(); err == nil {
				return reloadWechatUser(ctx, db, openid)
			}
		}
		return nil, err
	}
	_ = unionid // reserved for future cross-app account merging.
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &user, nil
}

func reloadWechatUser(ctx context.Context, db *sql.DB, openid string) (*authenticatedUser, error) {
	var user authenticatedUser
	err := db.QueryRowContext(ctx, `
		SELECT id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status, COALESCE(password_hash, ''), COALESCE(kyc_status, 'unverified'), COALESCE(kyc_level, 'tier1_basic'), COALESCE(preferred_language, 'zh-CN')
		FROM users
		WHERE wechat_openid = $1
		LIMIT 1
	`, openid).Scan(&user.ID, &user.Email, &user.DisplayName, &user.Role, &user.Status, &user.PasswordHash, &user.KYCStatus, &user.KYCLevel, &user.PreferredLanguage)
	if err != nil {
		return nil, err
	}
	return &user, nil
}
