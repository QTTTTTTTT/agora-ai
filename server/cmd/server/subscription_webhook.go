// subscription_webhook.go — LemonSqueezy webhook 的 subscription
// 事件分支。
//
// /api/lemonsqueezy/webhook 路由由 advisor_credits_handler 注册，
// 它先识别 order_* 事件给 advisor credit packs 用；其余事件名
// （subscription_created / subscription_updated / subscription_cancelled
// / subscription_expired / subscription_payment_success /
// subscription_payment_failed）会被 fall through 到本文件的
// dispatcher。
//
// 设计要点：
//   - 永远 200-ack（即使业务失败），避免 LS 雪崩重试
//   - lemonsqueezy_webhook_events 做幂等，相同 event_id 第二次进入直接跳过
//   - subscription_created 使用事务：UPDATE checkout_intents +
//     UPSERT subscriptions + outbox publish 三步原子
//   - subscription_expired 自动给用户写一条 free / system 行兜底
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// subscriptionWebhookExt 嵌进 advisorCreditsHandler，由该 handler
// 的 handleWebhook 在它自己 switch 之外的事件名上调用。
type subscriptionWebhookExt struct {
	db *sql.DB
}

func newSubscriptionWebhookExt(svc *Services) *subscriptionWebhookExt {
	if svc == nil || svc.DB == nil {
		return nil
	}
	return &subscriptionWebhookExt{db: svc.DB}
}

// dispatch 接 advisor webhook 在 default 之前调用，返回 (handled, error)。
// handled=false 时 advisor 那边还能继续走它原来的 default 分支（什么都不做）。
// error 永远是 nil 或者业务错——webhook 层面统一 200 ack。
func (s *subscriptionWebhookExt) dispatch(ctx context.Context, eventName, eventID string, body []byte) (handled bool, err error) {
	if s == nil || s.db == nil {
		return false, nil
	}
	if !strings.HasPrefix(eventName, "subscription_") {
		return false, nil
	}
	// 解析 subscription 形状（与 advisor 用的是不同的 schema）
	var p lsSubscriptionWebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		// 签名已 OK 但 body 不是 subscription 形状：还是吃下，避免 LS 重试
		return true, nil
	}

	if eventID == "" {
		eventID = p.Data.ID + ":" + eventName
	}
	// 幂等：插入 webhook events 表（CONFLICT DO NOTHING）；如果未真正插入说明
	// 之前已经处理过同一个 event_id，跳过。
	dup, err := s.markEventSeen(ctx, eventID, eventName, body)
	if err != nil {
		return true, err
	}
	if dup {
		return true, nil
	}

	intentID := strings.TrimSpace(p.Meta.CustomData["intent_id"])
	userID := strings.TrimSpace(p.Meta.CustomData["user_id"])
	tier := strings.TrimSpace(p.Meta.CustomData["plan_tier"])
	period := strings.TrimSpace(p.Meta.CustomData["billing_period"])
	seatCount := 1
	if v := strings.TrimSpace(p.Meta.CustomData["seat_count"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			seatCount = n
		}
	}

	switch eventName {
	case "subscription_created":
		return true, s.activate(ctx, p, intentID, userID, tier, period, seatCount)
	case "subscription_updated":
		return true, s.update(ctx, p)
	case "subscription_cancelled":
		return true, s.cancel(ctx, p)
	case "subscription_expired":
		return true, s.expire(ctx, p)
	case "subscription_payment_success":
		return true, s.renew(ctx, p)
	case "subscription_payment_failed":
		return true, s.markPastDue(ctx, p)
	}
	return true, nil
}

// ---------- payload ---------------------------------------------------------

type lsSubscriptionWebhookPayload struct {
	Meta struct {
		EventName  string            `json:"event_name"`
		CustomData map[string]string `json:"custom_data"`
	} `json:"meta"`
	Data struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Attributes struct {
			StoreID      int    `json:"store_id"`
			CustomerID   int    `json:"customer_id"`
			VariantID    int    `json:"variant_id"`
			ProductID    int    `json:"product_id"`
			UserEmail    string `json:"user_email"`
			Status       string `json:"status"`
			CardBrand    string `json:"card_brand"`
			CardLastFour string `json:"card_last_four"`
			RenewsAt     string `json:"renews_at"`
			EndsAt       string `json:"ends_at"`
			TrialEndsAt  string `json:"trial_ends_at"`
			CreatedAt    string `json:"created_at"`
			UpdatedAt    string `json:"updated_at"`
		} `json:"attributes"`
	} `json:"data"`
}

// ---------- 事件处理 --------------------------------------------------------

func (s *subscriptionWebhookExt) activate(
	ctx context.Context, p lsSubscriptionWebhookPayload,
	intentID, userID, tier, period string, seatCount int,
) error {
	if userID == "" || tier == "" {
		return errors.New("subscription_created: missing user_id/tier in custom_data")
	}
	if period == "" {
		period = "monthly"
	}
	if seatCount <= 0 {
		seatCount = 1
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if intentID != "" {
		_, _ = tx.ExecContext(ctx,
			`UPDATE checkout_intents
			    SET status='completed', completed_at=NOW()
			  WHERE id=$1 AND status='pending'`, intentID)
	}

	renewsAt := parseLSTime(p.Data.Attributes.RenewsAt)
	endsAt := parseLSTime(p.Data.Attributes.EndsAt)
	periodEnd := renewsAt
	if periodEnd.IsZero() {
		periodEnd = endsAt
	}

	var price sql.NullInt64
	_ = tx.QueryRowContext(ctx,
		`SELECT price_cents_usd FROM plan_lemonsqueezy_variants
		  WHERE plan_tier=$1 AND billing_period=$2`, tier, period,
	).Scan(&price)

	// 标记其它 active 订阅为 cancelled（用户从 starter → pro 等情况，
	// LS 那边走的是新建一条新 sub + 原 sub 自动 cancel；保险起见这里
	// 也兜底一次）。
	_, _ = tx.ExecContext(ctx,
		`UPDATE subscriptions
		    SET status='cancelled', cancelled_at=NOW(), updated_at=NOW()
		  WHERE user_id=$1 AND status='active' AND ls_subscription_id <> $2`,
		userID, p.Data.ID)

	customerID := strconv.Itoa(p.Data.Attributes.CustomerID)
	variantID := strconv.Itoa(p.Data.Attributes.VariantID)
	now := time.Now().UTC()

	// 用 ls_subscription_id 做 idempotent UPSERT（迁移里加了 unique
	// partial index）。如果同一个 LS sub 的 created 事件被 LS 重发了
	// 多次，只激活一次。
	_, err = tx.ExecContext(ctx,
		`INSERT INTO subscriptions (
		    id, user_id, plan_tier, status, payment_method,
		    start_date, end_date, auto_renew,
		    billing_period, locked_price_cents, seat_count,
		    ls_subscription_id, ls_customer_id, ls_variant_id,
		    current_period_start, current_period_end, renews_at,
		    created_at, updated_at
		 ) VALUES (
		    gen_random_uuid(), $1, $2, 'active', 'lemonsqueezy',
		    $3, $4, TRUE,
		    $5, $6, $10,
		    $7, $8, $9,
		    $3, $4, $4,
		    $3, $3
		 )
		 ON CONFLICT (ls_subscription_id) WHERE ls_subscription_id IS NOT NULL DO UPDATE SET
		    status='active',
		    seat_count=EXCLUDED.seat_count,
		    end_date=EXCLUDED.end_date,
		    current_period_end=EXCLUDED.current_period_end,
		    renews_at=EXCLUDED.renews_at,
		    updated_at=NOW()`,
		userID, tier,
		now, periodEnd,
		period, price,
		p.Data.ID, customerID, variantID,
		seatCount,
	)
	if err != nil {
		return err
	}

	// outbox 通知（欢迎邮件 + feature flag 缓存刷新）
	_, _ = tx.ExecContext(ctx,
		`INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload)
		 VALUES (gen_random_uuid(), 'subscription.activated', 'subscription', $1, $2)`,
		p.Data.ID,
		mustJSON(map[string]any{
			"user_id":            userID,
			"plan_tier":          tier,
			"billing_period":     period,
			"ls_subscription_id": p.Data.ID,
		}),
	)

	return tx.Commit()
}

func (s *subscriptionWebhookExt) update(ctx context.Context, p lsSubscriptionWebhookPayload) error {
	renewsAt := parseLSTime(p.Data.Attributes.RenewsAt)
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscriptions
		    SET status=$2,
		        renews_at=$3,
		        end_date=COALESCE($3, end_date),
		        current_period_end=COALESCE($3, current_period_end),
		        updated_at=NOW()
		  WHERE ls_subscription_id=$1`,
		p.Data.ID, p.Data.Attributes.Status, nullableTime(renewsAt))
	return err
}

func (s *subscriptionWebhookExt) cancel(ctx context.Context, p lsSubscriptionWebhookPayload) error {
	endsAt := parseLSTime(p.Data.Attributes.EndsAt)
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscriptions
		    SET status='cancelled',
		        cancelled_at=NOW(),
		        ends_at=$2,
		        updated_at=NOW()
		  WHERE ls_subscription_id=$1`,
		p.Data.ID, nullableTime(endsAt))
	return err
}

func (s *subscriptionWebhookExt) expire(ctx context.Context, p lsSubscriptionWebhookPayload) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var userID string
	if err := tx.QueryRowContext(ctx,
		`UPDATE subscriptions
		    SET status='expired', updated_at=NOW()
		  WHERE ls_subscription_id=$1
		  RETURNING user_id`,
		p.Data.ID).Scan(&userID); err != nil {
		return err
	}
	// 自动写一行 free / system 兜底，让 plan-gating 立即生效
	_, err = tx.ExecContext(ctx,
		`INSERT INTO subscriptions
		   (id, user_id, plan_tier, status, payment_method, start_date, end_date, auto_renew, created_at, updated_at)
		 VALUES (gen_random_uuid(), $1, 'free', 'active', 'system', NOW(), NOW() + INTERVAL '100 years', FALSE, NOW(), NOW())`,
		userID)
	if err != nil {
		return err
	}
	_, _ = tx.ExecContext(ctx,
		`INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload)
		 VALUES (gen_random_uuid(), 'subscription.expired', 'subscription', $1, $2)`,
		p.Data.ID, mustJSON(map[string]any{"user_id": userID}))
	return tx.Commit()
}

func (s *subscriptionWebhookExt) renew(ctx context.Context, p lsSubscriptionWebhookPayload) error {
	renewsAt := parseLSTime(p.Data.Attributes.RenewsAt)
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscriptions
		    SET status='active',
		        renews_at=$2,
		        end_date=COALESCE($2, end_date),
		        current_period_end=COALESCE($2, current_period_end),
		        updated_at=NOW()
		  WHERE ls_subscription_id=$1`,
		p.Data.ID, nullableTime(renewsAt))
	return err
}

func (s *subscriptionWebhookExt) markPastDue(ctx context.Context, p lsSubscriptionWebhookPayload) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscriptions
		    SET status='past_due', updated_at=NOW()
		  WHERE ls_subscription_id=$1`, p.Data.ID)
	return err
}

// ---------- helpers ---------------------------------------------------------

func (s *subscriptionWebhookExt) markEventSeen(ctx context.Context, eventID, name string, raw []byte) (dup bool, err error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO lemonsqueezy_webhook_events (event_id, event_name, payload)
		 VALUES ($1,$2,$3) ON CONFLICT (event_id) DO NOTHING`,
		eventID, name, raw)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 0, nil
}

func parseLSTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
