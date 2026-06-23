// subscription_checkout_handler.go — LemonSqueezy hosted-checkout
// 订阅入口（USD 计费）。
//
// Routes
//
//	POST /api/subscription/checkout         body: {tier, billing_period}
//	GET  /api/subscription/intent/{id}      前端 success 回跳后轮询
//	GET  /api/subscription/portal           跳 LS customer portal（取消/换卡）
//
// 流程：
//
//	/pricing 点 Subscribe → POST checkout → 后端 INSERT checkout_intents
//	pending → 调 LS CreateHostedCheckout（custom_data 带 intent_id /
//	user_id / tier / billing_period）→ 返回 checkout_url 给前端 →
//	前端 window.assign → LS 收款 → success_url 回跳 SPA 携带 intent_id
//	→ SPA 轮询 GET /intent/{id} 等到 status=completed → webhook
//	subscription_created 已经 UPSERT subscriptions + 关 intent。
//
// 与 advisor_credits 共用同一个 /api/lemonsqueezy/webhook —— 该 webhook
// 在 advisor_credits_handler.go 里，本文件只负责创建 checkout 的入口
// 和前端轮询/portal 跳转，**不重复**注册 webhook 路由。
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/lemonsqueezy"
	"github.com/fundai/server/internal/subscription"
)

type subscriptionCheckoutHandler struct {
	db         *sql.DB
	lsClient   *lemonsqueezy.Client
	appBaseURL string
	intentTTL  time.Duration
	now        func() time.Time
}

func newSubscriptionCheckoutHandler(svc *Services) *subscriptionCheckoutHandler {
	if svc == nil || svc.DB == nil {
		return nil
	}
	lsClient, _, err := lemonsqueezy.NewClientFromEnv()
	if err != nil {
		lsClient = nil
	}
	appBaseURL := strings.TrimSpace(os.Getenv("APP_PUBLIC_URL"))
	if appBaseURL == "" {
		appBaseURL = "http://localhost:8080"
	}
	return &subscriptionCheckoutHandler{
		db:         svc.DB,
		lsClient:   lsClient,
		appBaseURL: strings.TrimRight(appBaseURL, "/"),
		intentTTL:  30 * time.Minute,
		now:        time.Now,
	}
}

func (h *subscriptionCheckoutHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("POST /api/subscription/checkout", h.handleCreateCheckout)
	mux.HandleFunc("GET /api/subscription/intent/{id}", h.handleGetIntent)
	mux.HandleFunc("GET /api/subscription/portal", h.handleCustomerPortal)
}

// ----- POST /api/subscription/checkout --------------------------------------

type subCheckoutReq struct {
	Tier          string `json:"tier"`
	BillingPeriod string `json:"billing_period"`
	// SeatCount 是 seat-based 订阅（team 档）必填字段；个人档忽略。
	// 默认 1（个人）；team 档要求 >= 3。
	SeatCount int `json:"seat_count"`
}

type subCheckoutResp struct {
	IntentID    string `json:"intent_id"`
	CheckoutURL string `json:"checkout_url"`
	ExpiresAt   string `json:"expires_at"`
}

func (h *subscriptionCheckoutHandler) handleCreateCheckout(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing bearer token"))
		return
	}
	if h.lsClient == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			errorPayload("checkout_unavailable", "LemonSqueezy not configured on this deployment"))
		return
	}

	var req subCheckoutReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("bad_json", err.Error()))
		return
	}
	tier := strings.ToLower(strings.TrimSpace(req.Tier))
	period := strings.ToLower(strings.TrimSpace(req.BillingPeriod))
	if period == "" {
		period = "monthly"
	}
	if !isValidSubscriptionTier(tier) {
		writeJSON(w, http.StatusBadRequest,
			errorPayload("invalid_tier", "tier must be one of pro|premium|team"))
		return
	}
	if period != "monthly" && period != "yearly" {
		writeJSON(w, http.StatusBadRequest,
			errorPayload("invalid_period", "billing_period must be monthly or yearly"))
		return
	}

	// Enterprise = contact-sales only — refuse to create LS checkout
	// even if a variant happens to be wired.
	if tier == "enterprise" {
		writeJSON(w, http.StatusBadRequest,
			errorPayload("contact_sales", "enterprise is contact-sales only; please email sales@agora-ai.com"))
		return
	}

	// Seat validation. Team 档 min 3 seats; 其它档忽略 SeatCount 强制 1.
	seatCount := 1
	if tier == "team" {
		seatCount = req.SeatCount
		if seatCount < 3 {
			writeJSON(w, http.StatusBadRequest,
				errorPayload("invalid_seat_count", "team plan requires seat_count >= 3"))
			return
		}
		if seatCount > 100 {
			writeJSON(w, http.StatusBadRequest,
				errorPayload("invalid_seat_count", "max 100 seats via self-serve; contact sales for larger orders"))
			return
		}
	}

	ctx := r.Context()

	// 1. 防重订阅 - 已有同 plan active subscription → 409
	if dup, err := h.userHasActivePlan(ctx, userID, tier); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("db_error", err.Error()))
		return
	} else if dup {
		writeJSON(w, http.StatusConflict,
			errorPayload("already_subscribed", "user already on this plan"))
		return
	}

	// 2. 查 LS variant
	variantID, _, err := h.lookupVariant(ctx, tier, period)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusNotFound,
			errorPayload("variant_not_bound",
				fmt.Sprintf("no LS variant bound for %s/%s; admin must seed plan_lemonsqueezy_variants", tier, period)))
		return
	} else if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("db_error", err.Error()))
		return
	}

	// 3. 创建 intent（用 uuid 作为 id，避免引入 ulid 依赖）
	intentID := strings.ReplaceAll(uuid.NewString(), "-", "")
	expiresAt := h.now().Add(h.intentTTL)
	if _, err := h.db.ExecContext(ctx,
		`INSERT INTO checkout_intents
		   (id, user_id, plan_tier, billing_period, ls_variant_id, status, expires_at)
		 VALUES ($1,$2,$3,$4,$5,'pending',$6)`,
		intentID, userID, tier, period, variantID, expiresAt,
	); err != nil {
		writeJSON(w, http.StatusInternalServerError,
			errorPayload("intent_create_failed", err.Error()))
		return
	}

	// 4. 拿 user email/name 给 LS prefill
	email, name := h.lookupUserContact(ctx, userID)

	// 5. 调 LS hosted checkout
	successURL := fmt.Sprintf("%s/subscription?status=processing&intent_id=%s", h.appBaseURL, intentID)
	cancelURL := fmt.Sprintf("%s/pricing?status=cancelled", h.appBaseURL)
	variantQuantity := 0
	if tier == "team" {
		variantQuantity = seatCount
	}
	resp, err := h.lsClient.CreateHostedCheckout(ctx, lemonsqueezy.CheckoutRequest{
		VariantID:       variantID,
		UserEmail:       email,
		UserName:        name,
		VariantQuantity: variantQuantity,
		CustomData: map[string]string{
			"intent_id":      intentID,
			"user_id":        userID,
			"plan_tier":      tier,
			"billing_period": period,
			"seat_count":     strconv.Itoa(seatCount),
			"source":         "subscription_checkout",
		},
		SuccessRedirectURL: successURL,
		RedirectURL:        cancelURL,
	})
	if err != nil {
		// intent 留在 pending，30min 后被 sweeper 标 expired
		writeJSON(w, http.StatusBadGateway,
			errorPayload("ls_checkout_failed", err.Error()))
		return
	}

	// 6. 回填 ls_checkout_id（不阻塞主流程）
	_, _ = h.db.ExecContext(ctx,
		`UPDATE checkout_intents SET ls_checkout_id=$1 WHERE id=$2`,
		resp.ID, intentID)

	writeJSON(w, http.StatusOK, subCheckoutResp{
		IntentID:    intentID,
		CheckoutURL: resp.URL,
		ExpiresAt:   expiresAt.UTC().Format(time.RFC3339),
	})
}

// ----- GET /api/subscription/intent/{id} ------------------------------------

func (h *subscriptionCheckoutHandler) handleGetIntent(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", ""))
		return
	}
	intentID := strings.TrimSpace(r.PathValue("id"))
	if intentID == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("missing_id", "intent id required"))
		return
	}
	var (
		ownerID, status, tier, period string
		completedAt                   sql.NullTime
	)
	err := h.db.QueryRowContext(r.Context(),
		`SELECT user_id, status, plan_tier, billing_period, completed_at
		   FROM checkout_intents WHERE id=$1`, intentID,
	).Scan(&ownerID, &status, &tier, &period, &completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, errorPayload("intent_not_found", ""))
		return
	} else if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("db_error", err.Error()))
		return
	}
	if ownerID != userID {
		writeJSON(w, http.StatusForbidden, errorPayload("forbidden", ""))
		return
	}
	out := map[string]any{
		"intent_id":      intentID,
		"status":         status,
		"plan_tier":      tier,
		"billing_period": period,
	}
	if completedAt.Valid {
		out["completed_at"] = completedAt.Time.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
}

// ----- GET /api/subscription/portal -----------------------------------------

func (h *subscriptionCheckoutHandler) handleCustomerPortal(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", ""))
		return
	}
	if h.lsClient == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			errorPayload("portal_unavailable", "LemonSqueezy not configured"))
		return
	}
	var lsCustomerID sql.NullString
	err := h.db.QueryRowContext(r.Context(),
		`SELECT ls_customer_id FROM subscriptions
		  WHERE user_id=$1 AND status='active' AND ls_customer_id IS NOT NULL
		  ORDER BY created_at DESC LIMIT 1`, userID,
	).Scan(&lsCustomerID)
	if errors.Is(err, sql.ErrNoRows) || !lsCustomerID.Valid {
		writeJSON(w, http.StatusNotFound,
			errorPayload("no_active_subscription", "no LS-backed active subscription on this account"))
		return
	} else if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("db_error", err.Error()))
		return
	}
	portalURL, err := h.lsClient.GetCustomerPortalURL(r.Context(), lsCustomerID.String)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errorPayload("portal_failed", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"portal_url": portalURL})
}

// ----- helpers --------------------------------------------------------------

func isValidSubscriptionTier(t string) bool {
	switch t {
	case "pro", "premium", "team", "enterprise":
		return true
	}
	return false
}

func (h *subscriptionCheckoutHandler) userHasActivePlan(ctx context.Context, userID, tier string) (bool, error) {
	var n int
	err := h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM subscriptions
		  WHERE user_id=$1 AND plan_tier=$2 AND status='active'`,
		userID, tier).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (h *subscriptionCheckoutHandler) lookupVariant(ctx context.Context, tier, period string) (string, int, error) {
	var (
		variantID  string
		priceCents int
	)
	err := h.db.QueryRowContext(ctx,
		`SELECT ls_variant_id, price_cents_usd
		   FROM plan_lemonsqueezy_variants
		  WHERE plan_tier=$1 AND billing_period=$2 AND active=TRUE`,
		tier, period,
	).Scan(&variantID, &priceCents)
	if errors.Is(err, sql.ErrNoRows) {
		if envVariantID := subscriptionVariantIDFromEnv(tier, period); envVariantID != "" {
			return envVariantID, subscriptionPriceCents(tier, period), nil
		}
	}
	return variantID, priceCents, err
}

func subscriptionVariantIDFromEnv(tier, period string) string {
	upperTier := strings.ToUpper(strings.TrimSpace(tier))
	upperPeriod := strings.ToUpper(strings.TrimSpace(period))
	if upperTier == "" || upperPeriod == "" {
		return ""
	}
	for _, key := range []string{
		"LEMONSQUEEZY_VARIANT_" + upperTier + "_" + upperPeriod,
		"LS_VARIANT_" + upperTier + "_" + upperPeriod,
	} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}

func subscriptionPriceCents(tier, period string) int {
	plan, ok := subscription.Plans[subscription.PlanTier(strings.ToLower(strings.TrimSpace(tier)))]
	if !ok || plan == nil {
		return 0
	}
	if strings.EqualFold(period, "yearly") {
		return plan.PriceCentsUSDYear
	}
	return plan.PriceCentsUSDMonth
}

func (h *subscriptionCheckoutHandler) lookupUserContact(ctx context.Context, userID string) (email, name string) {
	_ = h.db.QueryRowContext(ctx,
		`SELECT COALESCE(email, ''), COALESCE(display_name, '')
		   FROM users WHERE id=$1`, userID).Scan(&email, &name)
	return
}
