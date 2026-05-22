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
	PlanEnterprise PlanTier = "enterprise"
)

// Plan 订阅计划
type Plan struct {
	Tier              PlanTier `json:"tier"`
	Name              string   `json:"name"`
	PriceCentsMonth   int      `json:"price_cents_month"`
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
}

// 预定义计划
var Plans = map[PlanTier]*Plan{
	PlanFree: {
		Tier: PlanFree, Name: "免费版", PriceCentsMonth: 0,
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
		Description:       "体验AI基金模拟的基础版本",
	},
	PlanPro: {
		Tier: PlanPro, Name: "专业版", PriceCentsMonth: 9900,
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
		Description:       "专业投资者的AI投研团队",
	},
	PlanPremium: {
		Tier: PlanPremium, Name: "旗舰版", PriceCentsMonth: 24900,
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
		Description:       "全功能AI基金公司模拟",
	},
	PlanEnterprise: {
		Tier: PlanEnterprise, Name: "企业版", PriceCentsMonth: 99900,
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
		Description:       "企业级私有化部署",
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
	order := []PlanTier{PlanFree, PlanPro, PlanPremium, PlanEnterprise}
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
