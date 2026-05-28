package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/workflow"
)

// Sprint 4 / android-core: device tokens registry + fan-out helper.
//
// 简单 HTTP wiring：
//   POST /api/devices/register   { token, platform, app_version }
//   POST /api/devices/unregister { token }
//
// 两个 endpoint 都需要登录态 — 我们用既有的 AuthenticatedUserID
// middleware（reading authenticatedUserIDKey from the request ctx）。
// 注册行为是 upsert：同一 (user, token) 撞库则 bump last_seen_at。
//
// 推送 fan-out 由 push provider（FCM）做；本文件只负责把 device
// tokens 喂给 caller。真正的"发送 push"实现可以是 stub（默认）— 把
// recipients 列出来打 log，留 hook 给后续 GCM/APNs adapter。

type deviceTokensService struct {
	db *sql.DB
}

func newDeviceTokensService(db *sql.DB) *deviceTokensService {
	return &deviceTokensService{db: db}
}

type registerDeviceRequest struct {
	Token      string `json:"token"`
	Platform   string `json:"platform"`
	AppVersion string `json:"app_version,omitempty"`
}

type unregisterDeviceRequest struct {
	Token string `json:"token"`
}

func (s *deviceTokensService) handleRegister(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	var req registerDeviceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body")
		return
	}
	req.Token = strings.TrimSpace(req.Token)
	req.Platform = strings.ToLower(strings.TrimSpace(req.Platform))
	if req.Token == "" {
		writeJSONError(w, http.StatusBadRequest, "token required")
		return
	}
	switch req.Platform {
	case "android", "ios", "web":
	default:
		writeJSONError(w, http.StatusBadRequest, "platform must be android/ios/web")
		return
	}
	if err := s.upsert(r.Context(), userID, req); err != nil {
		if isDeviceTokensMissing(err) {
			// 迁移 048 没跑 — 接受请求但什么都不做，回 200
			// 让客户端 happy。日志 debug 一次便于排障。
			slog.Debug("device_tokens table missing; registration skipped", "user_id", userID)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stored": false})
			return
		}
		slog.Warn("device_tokens upsert failed", "user_id", userID, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "register failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stored": true})
}

func (s *deviceTokensService) handleUnregister(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	var req unregisterDeviceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body")
		return
	}
	req.Token = strings.TrimSpace(req.Token)
	if req.Token == "" {
		writeJSONError(w, http.StatusBadRequest, "token required")
		return
	}
	if err := s.revoke(r.Context(), userID, req.Token); err != nil {
		if isDeviceTokensMissing(err) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		slog.Warn("device_tokens revoke failed", "user_id", userID, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "unregister failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *deviceTokensService) upsert(ctx context.Context, userID string, req registerDeviceRequest) error {
	if s == nil || s.db == nil {
		return errors.New("db unavailable")
	}
	const q = `
INSERT INTO device_tokens (user_id, token, platform, app_version, created_at, last_seen_at)
VALUES ($1, $2, $3, NULLIF($4, ''), NOW(), NOW())
ON CONFLICT (user_id, token) DO UPDATE
SET platform     = EXCLUDED.platform,
    app_version  = EXCLUDED.app_version,
    last_seen_at = NOW(),
    revoked_at   = NULL`
	_, err := s.db.ExecContext(ctx, q, userID, req.Token, req.Platform, req.AppVersion)
	return err
}

func (s *deviceTokensService) revoke(ctx context.Context, userID, token string) error {
	if s == nil || s.db == nil {
		return errors.New("db unavailable")
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE device_tokens
   SET revoked_at = NOW()
 WHERE user_id = $1 AND token = $2 AND revoked_at IS NULL`,
		userID, token)
	return err
}

// ActiveTokensForFund 找到一个基金对应的所有 active device tokens
// (fund -> users via fund_company_members)。该 helper 给推送 fan-out
// service 使用，与 HTTP 层无关。
//
// 不返回错误时长 — 上层 push fan-out 是 best-effort，迁移没跑也只
// 安静吞。
func (s *deviceTokensService) ActiveTokensForFund(ctx context.Context, fundID string) []string {
	if s == nil || s.db == nil || strings.TrimSpace(fundID) == "" {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT dt.token
  FROM device_tokens dt
  JOIN fund_company_members fcm ON fcm.user_id = dt.user_id
 WHERE fcm.fund_id = $1
   AND dt.revoked_at IS NULL`,
		fundID)
	if err != nil {
		if !isDeviceTokensMissing(err) {
			slog.Debug("device_tokens fan-out query failed", "fund_id", fundID, "err", err)
		}
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err == nil && strings.TrimSpace(t) != "" {
			out = append(out, t)
		}
	}
	return out
}

func isDeviceTokensMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, `relation "device_tokens"`) ||
		strings.Contains(msg, `"device_tokens" does not exist`) ||
		strings.Contains(msg, `unknown column`) ||
		strings.Contains(msg, `column "revoked_at"`)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"code":    status,
		"message": message,
	})
}

// pushTrigger 是一个简单的"该给谁推什么"的 fan-out shim。真正的
// 调用 FCM 由 provider 实现（生产可注入 fcmClient adapter）。当前
// 默认实现仅记录日志 + metrics — 让"推送已触发"在审计层可观察。
type pushTrigger string

const (
	triggerPlanReady       pushTrigger = "plan_ready"
	triggerPlanFailed      pushTrigger = "plan_failed"
	triggerPlanMixed       pushTrigger = "plan_mixed"
	triggerReflectionReady pushTrigger = "reflection_ready"
)

// notifyPlanLifecycleByString is the underlying impl. Status is a
// raw string so callers can come from package workflow (typed alias)
// or any test harness without an import.
func (s *deviceTokensService) notifyPlanLifecycleByString(ctx context.Context, fundID, planID, status string) {
	if s == nil || strings.TrimSpace(fundID) == "" || strings.TrimSpace(planID) == "" {
		return
	}
	var trigger pushTrigger
	switch strings.ToLower(status) {
	case "completed", "approved":
		trigger = triggerPlanReady
	case "mixed":
		trigger = triggerPlanMixed
	case "failed", "rejected":
		trigger = triggerPlanFailed
	default:
		return
	}
	s.emit(ctx, fundID, trigger, map[string]string{
		"plan_id": planID,
		"status":  status,
	})
}

func (s *deviceTokensService) emit(ctx context.Context, fundID string, trigger pushTrigger, payload map[string]string) {
	tokens := s.ActiveTokensForFund(ctx, fundID)
	if len(tokens) == 0 {
		return
	}
	// 真实 push 接入留 hook：当 OPS 配 FCM_SERVER_KEY 后，本函数应该
	// 调用 firebaseClient.Send(tokens, payload)。现在仅记录用于
	// 在 staging / dev 验证 fan-out 链路。
	slog.Info("push fan-out",
		"fund_id", fundID,
		"trigger", trigger,
		"recipients", len(tokens),
		"payload_keys", payloadKeys(payload),
		"sent_at", time.Now().UTC().Format(time.RFC3339),
	)
}

func payloadKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// fundIDFromPlan 一个独立辅助：plan_id → fund_id。当 emit 的调用者
// 不直接知道 fund_id 时可走它。Soft-fail 返回 "".
func fundIDFromPlan(ctx context.Context, db *sql.DB, planID string) string {
	if db == nil || strings.TrimSpace(planID) == "" {
		return ""
	}
	var fid string
	row := db.QueryRowContext(ctx, `SELECT fund_id FROM investment_plans WHERE id = $1`, planID)
	if err := row.Scan(&fid); err != nil {
		return ""
	}
	return fid
}

// planLifecycleNotifierAdapter wires the workflow.PlanLifecycleNotifier
// interface (which uses workflow.PlanWorkflowStatus) to deviceTokensService
// without forcing the latter to import the workflow package.
type planLifecycleNotifierAdapter struct {
	svc *deviceTokensService
}

func newPlanLifecycleNotifierAdapter(svc *deviceTokensService) *planLifecycleNotifierAdapter {
	return &planLifecycleNotifierAdapter{svc: svc}
}

func (a *planLifecycleNotifierAdapter) NotifyPlanLifecycle(ctx context.Context, fundID, planID string, status workflow.PlanWorkflowStatus) {
	if a == nil || a.svc == nil {
		return
	}
	a.svc.notifyPlanLifecycleByString(ctx, fundID, planID, string(status))
}

// unused — keep import lint clean
var _ = fmt.Errorf
