package subscription

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// PlanTier 订阅层级
type PlanTier string

const (
	PlanFree       PlanTier = "free"
	PlanPro        PlanTier = "pro"
	PlanPremium    PlanTier = "premium"
	// PlanTeam 是 seat-based 团队档（min_seats=3，BYOK 抵 LLM 成本，
	// 团队共享 watchlist）。Phase 1 的「Pricing rev」营销卡片里
	// 取代旧 enterprise 档的市场位置；旧 enterprise 仍保留作为
	// "Contact Sales" 兜底，前端不直接走 LS checkout。
	PlanTeam       PlanTier = "team"
	PlanEnterprise PlanTier = "enterprise"
)

// Plan 订阅计划
type Plan struct {
	Tier              PlanTier `json:"tier"`
	Name              string   `json:"name"`
	PriceCentsMonth   int      `json:"price_cents_month"`
	// PriceCentsUSDMonth 是面向海外 SaaS 的美元月费 (USD cents)。
	// PriceCentsMonth 仍然保留作为 CNY 月费（兼容老的 wechat / alipay
	// 充值流程），新接入的 LemonSqueezy hosted checkout 全部按 USD 计费，
	// 前端 /pricing 页固定按这个字段渲染。
	PriceCentsUSDMonth int     `json:"price_cents_usd_month"`
	// PriceCentsUSDYear 是年付价格 (USD cents)；通常 = month * 10
	// （省 2 个月）。0 表示不提供年付（free / enterprise）。
	PriceCentsUSDYear  int     `json:"price_cents_usd_year"`
	// MinSeats 是订阅生效所需的最少席位数。Team 档 = 3，其它都是 1。
	// 前端 /pricing 用它来显示 "min 3 seats"。
	MinSeats           int     `json:"min_seats"`
	// ContactSales=true 表示该档不走 LS hosted checkout，前端
	// CTA 渲染成 "Contact Sales" 按钮跳 mailto / 表单。Enterprise
	// 档专用。
	ContactSales       bool    `json:"contact_sales"`
	MaxFunds          int      `json:"max_funds"`
	MaxCallsPerDay    int      `json:"max_calls_per_day"`
	ModelTiers        []string `json:"model_tiers"`
	Recommended       bool     `json:"recommended"`
	MaxAgentsPerFund  int      `json:"max_agents_per_fund"`
	MaxWorkflowPerDay int      `json:"max_workflow_per_day"`
	AllowCustomKey    bool     `json:"allow_custom_key"`
	AllowABTest       bool     `json:"allow_ab_test"`
	AllowExport       bool     `json:"allow_export"`
	SimulationCapital float64  `json:"simulation_capital"`
	IncludedTokens    int64    `json:"included_tokens"`
	Description       string   `json:"description"`

	// Phase A — advisor 模式按次服务费配额。-1 表示无限。
	// 计费单位为 service unit:
	//   advisor deep consult (10 大师 / 4 战法 panel) = 1 unit
	//   advisor quick consult (单大师 / 单战法)        = 1 quick unit
	AdvisorDeepUnitsPerMonth  int `json:"advisor_deep_units_per_month"`
	AdvisorQuickUnitsPerMonth int `json:"advisor_quick_units_per_month"`

	// Phase B — 是否允许在 /advisor 模式接入 user-BYOK。
	// 与 AllowCustomKey 不同：那是 fund 维度的「用户级 model_config」；
	// 这是 advisor 维度的「用户 LLM key 加密存储 + UserOverrideHook」。
	AllowAdvisorBYOK bool `json:"allow_advisor_byok"`
}

// 预定义计划
//
// 文案合规口径（SEC Marketing Rule § 206(4)-1）：
//   - Name / Description 全部使用英文，避免本地化二义性
//   - 围绕「Master Team」核心卖点（10 大师 + 4 战法的模拟分析），
//     不写 "professional investor" / "investment research" /
//     "fund" 等暗示持牌/咨询/基金管理的词
//   - 强调 simulated / educational / informational 定位，
//     与 ComplianceMode=publishers_exclusion 的口径一致
//
// Pricing rev (2026-06-15)：
//   - free: 10 ask / Top 5 / Disrupt only
//   - pro $14.9 mo / $149 yr (Save $30): 50 ask / Top 20 / 4 strategies
//   - premium $29 mo / $290 yr (Save $58): 200 ask / + backtest + export + alerts
//   - team $49 / seat / mo (min 3 seats): unlimited via BYOK + shared watchlist
//   - enterprise: contact sales (self-host / SLA / compliance audit)
var Plans = map[PlanTier]*Plan{
	PlanFree: {
		Tier: PlanFree, Name: "Free",
		PriceCentsMonth: 0, PriceCentsUSDMonth: 0, PriceCentsUSDYear: 0,
		MinSeats: 1, ContactSales: false,
		MaxFunds: 1, MaxCallsPerDay: 1,
		ModelTiers:        []string{"simple"},
		Recommended:       false,
		MaxAgentsPerFund:  3,
		MaxWorkflowPerDay: 1,
		AllowCustomKey:    false,
		AllowABTest:       false,
		AllowExport:       false,
		SimulationCapital: 100000,
		IncludedTokens:    500000,
		Description:       "Get a taste of the Master Team's daily picks. Perfect for evaluating fit.",

		AdvisorDeepUnitsPerMonth:  10,
		AdvisorQuickUnitsPerMonth: 30,
		AllowAdvisorBYOK:          false,
	},
	PlanPro: {
		Tier: PlanPro, Name: "Pro",
		PriceCentsMonth: 9900, PriceCentsUSDMonth: 1490, PriceCentsUSDYear: 14900,
		MinSeats: 1, ContactSales: false,
		MaxFunds: 3, MaxCallsPerDay: 0,
		ModelTiers:        []string{"simple", "standard", "critical"},
		Recommended:       true,
		MaxAgentsPerFund:  10,
		MaxWorkflowPerDay: 0,
		AllowCustomKey:    true,
		AllowABTest:       true,
		AllowExport:       false,
		SimulationCapital: 10000000,
		IncludedTokens:    0,
		Description:       "Real-time picks across all 4 strategies. The default for active retail use.",

		AdvisorDeepUnitsPerMonth:  50,
		AdvisorQuickUnitsPerMonth: -1,
		AllowAdvisorBYOK:          true,
	},
	PlanPremium: {
		Tier: PlanPremium, Name: "Premium",
		PriceCentsMonth: 24900, PriceCentsUSDMonth: 2900, PriceCentsUSDYear: 29000,
		MinSeats: 1, ContactSales: false,
		MaxFunds: 10, MaxCallsPerDay: 0,
		ModelTiers:        []string{"simple", "standard", "critical"},
		Recommended:       false,
		MaxAgentsPerFund:  0,
		MaxWorkflowPerDay: 0,
		AllowCustomKey:    true,
		AllowABTest:       true,
		AllowExport:       true,
		SimulationCapital: 100000000,
		IncludedTokens:    0,
		Description:       "For power users: historical backtests, custom alerts, and exports on the Master Team strategies.",

		AdvisorDeepUnitsPerMonth:  200,
		AdvisorQuickUnitsPerMonth: -1,
		AllowAdvisorBYOK:          true,
	},
	PlanTeam: {
		Tier: PlanTeam, Name: "Team",
		// $49 / seat / mo, min 3 seats. PriceCentsUSDMonth here is
		// per-seat price; the checkout handler multiplies by seat_count.
		PriceCentsMonth: 0, PriceCentsUSDMonth: 4900, PriceCentsUSDYear: 49000,
		MinSeats: 3, ContactSales: false,
		MaxFunds: 0, MaxCallsPerDay: 0,
		ModelTiers:        []string{"simple", "standard", "critical"},
		Recommended:       false,
		MaxAgentsPerFund:  0,
		MaxWorkflowPerDay: 0,
		AllowCustomKey:    true,
		AllowABTest:       true,
		AllowExport:       true,
		SimulationCapital: 0,
		IncludedTokens:    0,
		Description:       "Shared watchlists and BYOK economics for prop desks and investment clubs.",

		AdvisorDeepUnitsPerMonth:  -1,
		AdvisorQuickUnitsPerMonth: -1,
		AllowAdvisorBYOK:          true,
	},
	PlanEnterprise: {
		Tier: PlanEnterprise, Name: "Enterprise",
		// Contact-sales tier — no LS checkout, no monthly price displayed.
		PriceCentsMonth: 0, PriceCentsUSDMonth: 0, PriceCentsUSDYear: 0,
		MinSeats: 0, ContactSales: true,
		MaxFunds: 0, MaxCallsPerDay: 0,
		ModelTiers:        []string{"simple", "standard", "critical"},
		Recommended:       false,
		MaxAgentsPerFund:  0,
		MaxWorkflowPerDay: 0,
		AllowCustomKey:    true,
		AllowABTest:       true,
		AllowExport:       true,
		SimulationCapital: 0,
		IncludedTokens:    0,
		Description:       "Self-hosted, SLA, compliance audits for funds and institutions.",

		AdvisorDeepUnitsPerMonth:  -1,
		AdvisorQuickUnitsPerMonth: -1,
		AllowAdvisorBYOK:          true,
	},
}

// Subscription 用户订阅
type Subscription struct {
	ID            string    `json:"id"`
	UserID        string    `json:"user_id"`
	PlanTier      PlanTier  `json:"plan_tier"`
	Status        string    `json:"status"`
	StartDate     time.Time `json:"start_date"`
	EndDate       time.Time `json:"end_date"`
	AutoRenew     bool      `json:"auto_renew"`
	PaymentMethod string    `json:"payment_method"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// SubscriptionService 订阅服务
type SubscriptionService struct {
	db *sql.DB
	mu sync.RWMutex
}

type PlatformSettings struct {
	AccessMode                 string
	DefaultTeamIntervalMinutes int
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
}

func NewSubscriptionService(db *sql.DB) *SubscriptionService {
	return &SubscriptionService{db: db}
}

func (s *SubscriptionService) GetPlan(tier string) (*Plan, error) {
	plan, ok := Plans[PlanTier(tier)]
	if !ok {
		return nil, fmt.Errorf("unknown plan tier: %s", tier)
	}
	return plan, nil
}

func (s *SubscriptionService) ListPlans() []*Plan {
	order := []PlanTier{PlanFree, PlanPro, PlanPremium, PlanTeam, PlanEnterprise}
	result := make([]*Plan, 0, len(order))
	for _, tier := range order {
		if p, ok := Plans[tier]; ok {
			result = append(result, p)
		}
	}
	return result
}

func (s *SubscriptionService) GetUserSubscription(ctx context.Context, userID string) (*Subscription, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
		SELECT id, user_id, plan_tier, status, start_date, end_date,
		       auto_renew, payment_method, created_at, updated_at
		FROM subscriptions
		WHERE user_id = $1 AND status = 'active'
		ORDER BY created_at DESC
		LIMIT 1
	`

	sub := &Subscription{}
	err := s.db.QueryRowContext(ctx, query, userID).Scan(
		&sub.ID, &sub.UserID, &sub.PlanTier, &sub.Status,
		&sub.StartDate, &sub.EndDate, &sub.AutoRenew,
		&sub.PaymentMethod, &sub.CreatedAt, &sub.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query subscription: %w", err)
	}
	return sub, nil
}

func (s *SubscriptionService) Subscribe(ctx context.Context, userID, tier, paymentMethod string) (*Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := Plans[PlanTier(tier)]; !ok {
		return nil, fmt.Errorf("unknown plan tier: %s", tier)
	}

	switch paymentMethod {
	case "wechat", "alipay", "manual", "system":
	default:
		return nil, fmt.Errorf("unsupported payment method: %s", paymentMethod)
	}

	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	cancelQuery := `
		UPDATE subscriptions
		SET status = 'cancelled', auto_renew = false, updated_at = $1
		WHERE user_id = $2 AND status = 'active'
	`
	if _, err := tx.ExecContext(ctx, cancelQuery, now, userID); err != nil {
		return nil, fmt.Errorf("cancel existing subscription: %w", err)
	}

	sub := &Subscription{
		ID:            uuid.New().String(),
		UserID:        userID,
		PlanTier:      PlanTier(tier),
		Status:        "active",
		StartDate:     now,
		EndDate:       now.AddDate(0, 1, 0),
		AutoRenew:     true,
		PaymentMethod: paymentMethod,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	insertQuery := `
		INSERT INTO subscriptions (id, user_id, plan_tier, status, start_date, end_date,
		                           auto_renew, payment_method, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`
	_, err = tx.ExecContext(ctx, insertQuery,
		sub.ID, sub.UserID, sub.PlanTier, sub.Status,
		sub.StartDate, sub.EndDate, sub.AutoRenew,
		sub.PaymentMethod, sub.CreatedAt, sub.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert subscription: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return sub, nil
}

func (s *SubscriptionService) Cancel(ctx context.Context, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	query := `
		UPDATE subscriptions
		SET status = 'cancelled', auto_renew = false, updated_at = $1
		WHERE user_id = $2 AND status = 'active'
	`
	result, err := s.db.ExecContext(ctx, query, now, userID)
	if err != nil {
		return fmt.Errorf("cancel subscription: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("no active subscription found for user %s", userID)
	}

	return nil
}

func (s *SubscriptionService) CheckQuota(ctx context.Context, userID, action string, currentCount int) error {
	plan, err := s.GetEffectivePlan(ctx, userID)
	if err != nil {
		return fmt.Errorf("get effective plan: %w", err)
	}

	switch action {
	case "create_fund":
		if plan.MaxFunds > 0 && currentCount >= plan.MaxFunds {
			return fmt.Errorf("plan %s allows maximum %d funds, current: %d", plan.Tier, plan.MaxFunds, currentCount)
		}
	case "create_agent":
		if plan.MaxAgentsPerFund > 0 && currentCount >= plan.MaxAgentsPerFund {
			return fmt.Errorf("plan %s allows maximum %d agents per fund, current: %d", plan.Tier, plan.MaxAgentsPerFund, currentCount)
		}
	case "run_workflow":
		if plan.MaxCallsPerDay > 0 {
			todayCount, err := s.getTodayWorkflowCount(ctx, userID)
			if err != nil {
				return fmt.Errorf("get today workflow count: %w", err)
			}
			if todayCount >= plan.MaxCallsPerDay {
				return fmt.Errorf("plan %s allows maximum %d workflows per day, used: %d", plan.Tier, plan.MaxCallsPerDay, todayCount)
			}
		}
	default:
		return fmt.Errorf("unknown action: %s", action)
	}

	return nil
}

func (s *SubscriptionService) getTodayWorkflowCount(ctx context.Context, userID string) (int, error) {
	today := time.Now().Format("2006-01-02")
	query := `
		SELECT COUNT(*)
		FROM usage_entries
		WHERE user_id = $1 AND DATE(created_at) = $2
	`
	var count int
	err := s.db.QueryRowContext(ctx, query, userID, today).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count today workflows: %w", err)
	}
	return count, nil
}

func (s *SubscriptionService) CheckModelAccess(ctx context.Context, userID, modelTier string) error {
	plan, err := s.GetEffectivePlan(ctx, userID)
	if err != nil {
		return fmt.Errorf("get effective plan: %w", err)
	}

	for _, allowed := range plan.ModelTiers {
		if allowed == modelTier {
			return nil
		}
	}

	return fmt.Errorf("model tier %q not allowed for plan %s (allowed: %v)", modelTier, plan.Tier, plan.ModelTiers)
}

func (s *SubscriptionService) AllowsCustomKey(ctx context.Context, userID string) (bool, error) {
	plan, err := s.GetEffectivePlan(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("get effective plan: %w", err)
	}
	if plan == nil {
		return false, nil
	}
	return plan.AllowCustomKey, nil
}

func normalizePlatformAccessMode(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "free_open":
		return "free_open"
	default:
		return "paid_open"
	}
}

func normalizeTeamIntervalMinutes(value int) int {
	if value < 5 {
		return 15
	}
	if value > 1440 {
		return 1440
	}
	return value
}

func (s *SubscriptionService) GetPlatformSettings(ctx context.Context) (*PlatformSettings, error) {
	settings := &PlatformSettings{}
	err := s.db.QueryRowContext(ctx, `
		SELECT access_mode, default_team_interval_minutes, created_at, updated_at
		FROM platform_settings
		WHERE id = TRUE
	`).Scan(&settings.AccessMode, &settings.DefaultTeamIntervalMinutes, &settings.CreatedAt, &settings.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &PlatformSettings{
				AccessMode:                 "paid_open",
				DefaultTeamIntervalMinutes: 15,
			}, nil
		}
		return nil, fmt.Errorf("get platform settings: %w", err)
	}
	settings.AccessMode = normalizePlatformAccessMode(settings.AccessMode)
	settings.DefaultTeamIntervalMinutes = normalizeTeamIntervalMinutes(settings.DefaultTeamIntervalMinutes)
	return settings, nil
}

func (s *SubscriptionService) UpdatePlatformSettings(ctx context.Context, settings *PlatformSettings) (*PlatformSettings, error) {
	if settings == nil {
		settings = &PlatformSettings{}
	}
	accessMode := normalizePlatformAccessMode(settings.AccessMode)
	intervalMinutes := normalizeTeamIntervalMinutes(settings.DefaultTeamIntervalMinutes)
	updated := &PlatformSettings{}
	if err := s.db.QueryRowContext(ctx, `
		INSERT INTO platform_settings (id, access_mode, default_team_interval_minutes)
		VALUES (TRUE, $1, $2)
		ON CONFLICT (id) DO UPDATE
		SET access_mode = EXCLUDED.access_mode,
		    default_team_interval_minutes = EXCLUDED.default_team_interval_minutes,
		    updated_at = NOW()
		RETURNING access_mode, default_team_interval_minutes, created_at, updated_at
	`, accessMode, intervalMinutes).Scan(&updated.AccessMode, &updated.DefaultTeamIntervalMinutes, &updated.CreatedAt, &updated.UpdatedAt); err != nil {
		return nil, fmt.Errorf("update platform settings: %w", err)
	}
	updated.AccessMode = normalizePlatformAccessMode(updated.AccessMode)
	updated.DefaultTeamIntervalMinutes = normalizeTeamIntervalMinutes(updated.DefaultTeamIntervalMinutes)
	return updated, nil
}

func (s *SubscriptionService) IsExpired(sub *Subscription) bool {
	if sub == nil {
		return true
	}
	if sub.Status != "active" {
		return true
	}
	return time.Now().After(sub.EndDate)
}

func (s *SubscriptionService) GetEffectivePlan(ctx context.Context, userID string) (*Plan, error) {
	settings, err := s.GetPlatformSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("get platform settings: %w", err)
	}
	if settings != nil && settings.AccessMode == "free_open" {
		return Plans[PlanEnterprise], nil
	}

	sub, err := s.GetUserSubscription(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get user subscription: %w", err)
	}

	if sub == nil {
		return Plans[PlanFree], nil
	}

	if s.IsExpired(sub) {
		now := time.Now()
		updateQuery := `
			UPDATE subscriptions
			SET status = 'expired', updated_at = $1
			WHERE id = $2 AND status = 'active'
		`
		_, _ = s.db.ExecContext(ctx, updateQuery, now, sub.ID)
		return Plans[PlanFree], nil
	}

	plan, ok := Plans[sub.PlanTier]
	if !ok {
		return Plans[PlanFree], nil
	}

	return plan, nil
}
