package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Domain types (subscription / usage / model-config domain)
// ---------------------------------------------------------------------------

// SubscriptionPlan represents a subscription plan (free / pro / premium / enterprise).
// Named differently from the investment "Plan" in fund_handler.go.
type SubscriptionPlan struct {
	Tier              string   `json:"tier"`
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

type PlatformSettings struct {
	AccessMode                 string    `json:"access_mode"`
	DefaultTeamIntervalMinutes int       `json:"default_team_interval_minutes"`
	UpdatedAt                  time.Time `json:"updated_at,omitempty"`
}

type Subscription struct {
	ID            string    `json:"id"`
	UserID        string    `json:"user_id"`
	PlanTier      string    `json:"plan_tier"`
	Status        string    `json:"status"`
	StartDate     time.Time `json:"start_date"`
	EndDate       time.Time `json:"end_date"`
	AutoRenew     bool      `json:"auto_renew"`
	PaymentMethod string    `json:"payment_method"`
}

type DailySummary struct {
	UserID         string          `json:"user_id"`
	SummaryDate    string          `json:"summary_date"`
	TotalCalls     int             `json:"total_calls"`
	InputTokens    int64           `json:"input_tokens"`
	OutputTokens   int64           `json:"output_tokens"`
	CostCents      float64         `json:"cost_cents"`
	PriceCents     float64         `json:"price_cents"`
	CustomKeyCalls int             `json:"custom_key_calls"`
	ModelBreakdown json.RawMessage `json:"model_breakdown"`
	StepBreakdown  json.RawMessage `json:"step_breakdown"`
}

type MonthlySummary struct {
	UserID         string          `json:"user_id"`
	YearMonth      string          `json:"year_month"`
	TotalCalls     int             `json:"total_calls"`
	InputTokens    int64           `json:"input_tokens"`
	OutputTokens   int64           `json:"output_tokens"`
	CostCents      float64         `json:"cost_cents"`
	PriceCents     float64         `json:"price_cents"`
	CustomKeyCalls int             `json:"custom_key_calls"`
	ModelBreakdown json.RawMessage `json:"model_breakdown"`
}

type UsageEntry struct {
	ID            string    `json:"id"`
	FundID        *string   `json:"fund_id,omitempty"`
	StepName      string    `json:"step_name"`
	ModelProvider string    `json:"model_provider"`
	ModelName     string    `json:"model_name"`
	InputTokens   int       `json:"input_tokens"`
	OutputTokens  int       `json:"output_tokens"`
	CostCents     float64   `json:"cost_cents"`
	PriceCents    float64   `json:"price_cents"`
	IsCustomKey   bool      `json:"is_custom_key"`
	CreatedAt     time.Time `json:"created_at"`
}

type MonthlyBill struct {
	ID              string          `json:"id"`
	UserID          string          `json:"user_id"`
	YearMonth       string          `json:"year_month"`
	PlanTier        string          `json:"plan_tier"`
	SubscriptionFee int             `json:"subscription_fee"`
	ModelUsageFee   float64         `json:"model_usage_fee"`
	CustomKeyCredit float64         `json:"custom_key_credit"`
	TotalFee        float64         `json:"total_fee"`
	FinalAmount     float64         `json:"final_amount"`
	Status          string          `json:"status"`
	DetailsJSON     json.RawMessage `json:"details_json,omitempty"`
}

type UserModelConfig struct {
	ID              string    `json:"id"`
	UserID          string    `json:"user_id"`
	AgentID         *string   `json:"agent_id,omitempty"`
	ConfigType      string    `json:"config_type"`
	Tier            *string   `json:"tier,omitempty"`
	Provider        string    `json:"provider"`
	ModelName       string    `json:"model_name"`
	BaseURL         *string   `json:"base_url,omitempty"`
	APIKeyEncrypted *string   `json:"-"`
	IsActive        bool      `json:"is_active"`
	CreatedAt       time.Time `json:"created_at,omitempty"`
	UpdatedAt       time.Time `json:"updated_at,omitempty"`
}

type ConnectionTestResult struct {
	Success bool   `json:"success"`
	Latency int    `json:"latency_ms"`
	Message string `json:"message"`
	ModelID string `json:"model_id,omitempty"`
}

type ModelInfo struct {
	Provider       string  `json:"provider"`
	ModelName      string  `json:"model_name"`
	DisplayName    string  `json:"display_name"`
	Tier           string  `json:"tier"`
	InputPrice     float64 `json:"input_price_per_1k"`
	OutputPrice    float64 `json:"output_price_per_1k"`
	Available      bool    `json:"available"`
	UsesCustomKey  bool    `json:"uses_custom_key,omitempty"`
	CustomKeyReady bool    `json:"custom_key_ready,omitempty"`
}

type WalletAccount struct {
	ID           string    `json:"id"`
	UserID       string    `json:"user_id"`
	BaseCurrency string    `json:"base_currency"`
	BalanceMinor int64     `json:"balance_minor"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type WalletLedgerEntry struct {
	ID                string          `json:"id"`
	AccountID         string          `json:"account_id"`
	EntryType         string          `json:"entry_type"`
	AmountMinor       int64           `json:"amount_minor"`
	BalanceAfterMinor int64           `json:"balance_after_minor"`
	Currency          string          `json:"currency"`
	ReferenceType     *string         `json:"reference_type,omitempty"`
	ReferenceID       *string         `json:"reference_id,omitempty"`
	CreatedByUserID   *string         `json:"created_by_user_id,omitempty"`
	Metadata          json.RawMessage `json:"metadata"`
	CreatedAt         time.Time       `json:"created_at"`
}

// ---------------------------------------------------------------------------
// Service interfaces
// ---------------------------------------------------------------------------

type SubscriptionServiceInterface interface {
	ListPlans() []*SubscriptionPlan
	GetPlan(tier string) (*SubscriptionPlan, error)
	GetUserSubscription(ctx context.Context, userID string) (*Subscription, error)
	Subscribe(ctx context.Context, userID, tier, paymentMethod string) (*Subscription, error)
	Cancel(ctx context.Context, userID string) error
	GetEffectivePlan(ctx context.Context, userID string) (*SubscriptionPlan, error)
	CheckQuota(ctx context.Context, userID, action string, currentCount int) error
	CheckModelAccess(ctx context.Context, userID, modelTier string) error
	AllowsCustomKey(ctx context.Context, userID string) (bool, error)
}

type UsageTrackerInterface interface {
	GetDailySummary(ctx context.Context, userID string, date time.Time) (*DailySummary, error)
	GetMonthlySummary(ctx context.Context, userID, yearMonth string) (*MonthlySummary, error)
	GetUsageHistory(ctx context.Context, userID string, offset, limit int) ([]*UsageEntry, int, error)
	GetBill(ctx context.Context, userID, yearMonth string) (*MonthlyBill, error)
}

type ModelConfigServiceInterface interface {
	SaveConfig(ctx context.Context, config *UserModelConfig) error
	GetUserConfigs(ctx context.Context, userID string) ([]*UserModelConfig, error)
	DeleteConfig(ctx context.Context, userID, configID string) error
	TestConnection(ctx context.Context, config *UserModelConfig) (*ConnectionTestResult, error)
}

type LLMClientInterface interface {
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

type WalletServiceInterface interface {
	GetWallet(ctx context.Context, userID string) (*WalletAccount, error)
	ListWalletLedger(ctx context.Context, userID string, offset, limit int) ([]WalletLedgerEntry, int, error)
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// SubscriptionHandler 订阅和模型配置相关端点
type SubscriptionHandler struct {
	subService   SubscriptionServiceInterface
	usageTracker UsageTrackerInterface
	modelConfig  ModelConfigServiceInterface
	llmClient    LLMClientInterface
	wallets      WalletServiceInterface
}

// NewSubscriptionHandler creates a new SubscriptionHandler.
func NewSubscriptionHandler(
	sub SubscriptionServiceInterface,
	usage UsageTrackerInterface,
	modelCfg ModelConfigServiceInterface,
	llm LLMClientInterface,
	wallets WalletServiceInterface,
) *SubscriptionHandler {
	return &SubscriptionHandler{
		subService:   sub,
		usageTracker: usage,
		modelConfig:  modelCfg,
		llmClient:    llm,
		wallets:      wallets,
	}
}

// RegisterSubscriptionRoutes 注册订阅相关路由
func (h *SubscriptionHandler) RegisterRoutes(mux *http.ServeMux) {
	// 订阅相关
	mux.HandleFunc("GET /api/plans", h.handleListPlans)
	mux.HandleFunc("GET /api/subscription", h.handleGetSubscription)
	mux.HandleFunc("POST /api/subscription", h.handleSubscribe)
	mux.HandleFunc("DELETE /api/subscription", h.handleCancelSubscription)

	// 模型配置
	mux.HandleFunc("GET /api/models", h.handleListModels)
	mux.HandleFunc("GET /api/models/config", h.handleGetModelConfigs)
	mux.HandleFunc("POST /api/models/config", h.handleSaveModelConfig)
	mux.HandleFunc("DELETE /api/models/config/{configId}", h.handleDeleteModelConfig)
	mux.HandleFunc("POST /api/models/test", h.handleTestConnection)

	// 用量和账单
	mux.HandleFunc("GET /api/usage/today", h.handleTodayUsage)
	mux.HandleFunc("GET /api/usage/monthly", h.handleMonthlyUsage)
	mux.HandleFunc("GET /api/usage/history", h.handleUsageHistory)
	mux.HandleFunc("GET /api/usage/bill", h.handleGetBill)
	mux.HandleFunc("GET /api/usage/estimate", h.handleEstimate)

	// 钱包
	mux.HandleFunc("GET /api/wallet", h.handleGetWallet)
	mux.HandleFunc("GET /api/wallet/ledger", h.handleWalletLedger)
}

// ---------------------------------------------------------------------------
// Helpers (subscription-specific; shared helpers live in fund_handler.go)
// ---------------------------------------------------------------------------

func subGetUserID(r *http.Request) (string, error) {
	uid, ok := AuthenticatedUserID(r)
	if !ok {
		return "", http.ErrNoCookie
	}
	return uid, nil
}

func subWriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type subErrorResponse struct {
	Error string `json:"error"`
}

func subWriteError(w http.ResponseWriter, status int, msg string) {
	subWriteJSON(w, status, subErrorResponse{Error: msg})
}

func optionalStringPtr(value string) *string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// ---------------------------------------------------------------------------
// 订阅相关端点
// ---------------------------------------------------------------------------

// handleListPlans 返回所有计划及推荐标记
func (h *SubscriptionHandler) handleListPlans(w http.ResponseWriter, _ *http.Request) {
	plans := h.subService.ListPlans()
	subWriteJSON(w, http.StatusOK, map[string]any{
		"plans": plans,
	})
}

// handleGetSubscription 返回当前订阅 + 生效的 plan
func (h *SubscriptionHandler) handleGetSubscription(w http.ResponseWriter, r *http.Request) {
	userID, err := subGetUserID(r)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			subWriteError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		subWriteError(w, http.StatusUnauthorized, err.Error())
		return
	}

	sub, err := h.subService.GetUserSubscription(r.Context(), userID)
	if err != nil {
		subWriteError(w, http.StatusInternalServerError, "failed to get subscription: "+err.Error())
		return
	}

	plan, err := h.subService.GetEffectivePlan(r.Context(), userID)
	if err != nil {
		subWriteError(w, http.StatusInternalServerError, "failed to get effective plan: "+err.Error())
		return
	}

	subWriteJSON(w, http.StatusOK, map[string]any{
		"subscription": sub,
		"plan":         plan,
		"permissions": map[string]any{
			"allow_custom_key": plan != nil && plan.AllowCustomKey,
			"allow_ab_test":    plan != nil && plan.AllowABTest,
			"allow_export":     plan != nil && plan.AllowExport,
		},
	})
}

// handleSubscribe 订阅计划
func (h *SubscriptionHandler) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	userID, err := subGetUserID(r)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			subWriteError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		subWriteError(w, http.StatusUnauthorized, err.Error())
		return
	}

	var req struct {
		Tier          string `json:"tier"`
		PaymentMethod string `json:"payment_method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		subWriteError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	defer r.Body.Close()

	if req.Tier == "" {
		subWriteError(w, http.StatusBadRequest, "tier is required")
		return
	}

	// Validate tier
	if _, err := h.subService.GetPlan(req.Tier); err != nil {
		subWriteError(w, http.StatusBadRequest, "invalid tier: "+req.Tier)
		return
	}

	if req.PaymentMethod == "" {
		req.PaymentMethod = "manual"
	}

	sub, err := h.subService.Subscribe(r.Context(), userID, req.Tier, req.PaymentMethod)
	if err != nil {
		subWriteError(w, http.StatusInternalServerError, "failed to subscribe: "+err.Error())
		return
	}

	subWriteJSON(w, http.StatusCreated, map[string]any{
		"subscription": sub,
	})
}

// handleCancelSubscription 取消订阅
func (h *SubscriptionHandler) handleCancelSubscription(w http.ResponseWriter, r *http.Request) {
	userID, err := subGetUserID(r)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			subWriteError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		subWriteError(w, http.StatusUnauthorized, err.Error())
		return
	}

	if err := h.subService.Cancel(r.Context(), userID); err != nil {
		subWriteError(w, http.StatusInternalServerError, "failed to cancel subscription: "+err.Error())
		return
	}

	subWriteJSON(w, http.StatusOK, map[string]any{
		"message": "subscription cancelled",
	})
}

// ---------------------------------------------------------------------------
// 模型配置端点
// ---------------------------------------------------------------------------

// handleListModels 返回平台模型列表 + 用户自定义模型
func (h *SubscriptionHandler) handleListModels(w http.ResponseWriter, r *http.Request) {
	userID, err := subGetUserID(r)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			subWriteError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		subWriteError(w, http.StatusUnauthorized, err.Error())
		return
	}

	platformModels, err := h.llmClient.ListModels(r.Context())
	if err != nil {
		subWriteError(w, http.StatusInternalServerError, "failed to list platform models: "+err.Error())
		return
	}

	userConfigs, err := h.modelConfig.GetUserConfigs(r.Context(), userID)
	if err != nil {
		subWriteError(w, http.StatusInternalServerError, "failed to list user model configs: "+err.Error())
		return
	}

	allowedCustomKey, err := h.subService.AllowsCustomKey(r.Context(), userID)
	if err != nil {
		subWriteError(w, http.StatusInternalServerError, "failed to resolve custom key permission: "+err.Error())
		return
	}

	// Build custom model list from user configs with custom_endpoint type
	var customModels []ModelInfo
	for _, cfg := range userConfigs {
		if cfg.ConfigType == "custom_endpoint" && cfg.IsActive {
			customModels = append(customModels, ModelInfo{
				Provider:       cfg.Provider,
				ModelName:      cfg.ModelName,
				DisplayName:    fmt.Sprintf("%s (%s)", cfg.ModelName, cfg.Provider),
				Tier:           "custom",
				Available:      allowedCustomKey,
				UsesCustomKey:  true,
				CustomKeyReady: allowedCustomKey,
			})
		}
	}

	subWriteJSON(w, http.StatusOK, map[string]any{
		"platform_models": platformModels,
		"custom_models":   customModels,
	})
}

// handleGetModelConfigs 返回用户的模型配置列表
func (h *SubscriptionHandler) handleGetModelConfigs(w http.ResponseWriter, r *http.Request) {
	userID, err := subGetUserID(r)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			subWriteError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		subWriteError(w, http.StatusUnauthorized, err.Error())
		return
	}

	configs, err := h.modelConfig.GetUserConfigs(r.Context(), userID)
	if err != nil {
		subWriteError(w, http.StatusInternalServerError, "failed to get model configs: "+err.Error())
		return
	}

	subWriteJSON(w, http.StatusOK, map[string]any{
		"configs": configs,
	})
}

// handleSaveModelConfig 保存模型配置
func (h *SubscriptionHandler) handleSaveModelConfig(w http.ResponseWriter, r *http.Request) {
	userID, err := subGetUserID(r)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			subWriteError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		subWriteError(w, http.StatusUnauthorized, err.Error())
		return
	}

	var req struct {
		ConfigType string  `json:"config_type"`
		Tier       *string `json:"tier,omitempty"`
		Provider   string  `json:"provider"`
		ModelName  string  `json:"model_name"`
		BaseURL    *string `json:"base_url,omitempty"`
		APIKey     *string `json:"api_key,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		subWriteError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	defer r.Body.Close()

	// Validate required fields
	if req.ConfigType == "" {
		subWriteError(w, http.StatusBadRequest, "config_type is required")
		return
	}
	if req.Provider == "" {
		subWriteError(w, http.StatusBadRequest, "provider is required")
		return
	}
	if req.ModelName == "" {
		subWriteError(w, http.StatusBadRequest, "model_name is required")
		return
	}

	// Validate config_type
	if req.ConfigType != "tier_override" && req.ConfigType != "custom_endpoint" {
		subWriteError(w, http.StatusBadRequest, "config_type must be 'tier_override' or 'custom_endpoint'")
		return
	}

	// tier_override requires a tier
	if req.ConfigType == "tier_override" && (req.Tier == nil || *req.Tier == "") {
		subWriteError(w, http.StatusBadRequest, "tier is required for tier_override config")
		return
	}

	// Validate provider
	validProviders := map[string]bool{
		"openai": true, "claude": true, "deepseek": true, "qwen": true, "custom": true,
	}
	if !validProviders[req.Provider] {
		subWriteError(w, http.StatusBadRequest, "invalid provider: "+req.Provider)
		return
	}

	// Validate tier if provided
	if req.Tier != nil && *req.Tier != "" {
		validTiers := map[string]bool{"critical": true, "standard": true, "simple": true}
		if !validTiers[*req.Tier] {
			subWriteError(w, http.StatusBadRequest, "invalid tier: "+*req.Tier)
			return
		}
	}

	// Check model access based on plan
	if req.ConfigType == "tier_override" && req.Tier != nil {
		if err := h.subService.CheckModelAccess(r.Context(), userID, *req.Tier); err != nil {
			subWriteError(w, http.StatusForbidden, "model access denied: "+err.Error())
			return
		}
	}
	if req.ConfigType == "custom_endpoint" {
		allowed, err := h.subService.AllowsCustomKey(r.Context(), userID)
		if err != nil {
			subWriteError(w, http.StatusInternalServerError, "failed to validate custom key permission: "+err.Error())
			return
		}
		if !allowed {
			subWriteError(w, http.StatusForbidden, "custom model key is not allowed for current subscription")
			return
		}
	}

	config := &UserModelConfig{
		UserID:     userID,
		ConfigType: req.ConfigType,
		Tier:       req.Tier,
		Provider:   req.Provider,
		ModelName:  req.ModelName,
		BaseURL:    req.BaseURL,
		IsActive:   true,
	}

	// If api_key is provided, store it (service layer handles encryption)
	if req.APIKey != nil {
		config.APIKeyEncrypted = req.APIKey
	}

	if err := h.modelConfig.SaveConfig(r.Context(), config); err != nil {
		subWriteError(w, http.StatusInternalServerError, "failed to save model config: "+err.Error())
		return
	}

	subWriteJSON(w, http.StatusOK, map[string]any{
		"message": "model config saved",
		"config":  config,
	})
}

// handleDeleteModelConfig 删除配置
func (h *SubscriptionHandler) handleDeleteModelConfig(w http.ResponseWriter, r *http.Request) {
	userID, err := subGetUserID(r)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			subWriteError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		subWriteError(w, http.StatusUnauthorized, err.Error())
		return
	}

	configID := r.PathValue("configId")
	if configID == "" {
		subWriteError(w, http.StatusBadRequest, "configId is required")
		return
	}

	if err := h.modelConfig.DeleteConfig(r.Context(), userID, configID); err != nil {
		subWriteError(w, http.StatusInternalServerError, "failed to delete model config: "+err.Error())
		return
	}

	subWriteJSON(w, http.StatusOK, map[string]any{
		"message": "model config deleted",
	})
}

// handleTestConnection 测试模型连通性
func (h *SubscriptionHandler) handleTestConnection(w http.ResponseWriter, r *http.Request) {
	userID, err := subGetUserID(r)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			subWriteError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		subWriteError(w, http.StatusUnauthorized, err.Error())
		return
	}

	var req struct {
		Provider  string  `json:"provider"`
		ModelName string  `json:"model_name"`
		BaseURL   *string `json:"base_url,omitempty"`
		APIKey    *string `json:"api_key,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		subWriteError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	defer r.Body.Close()

	if req.Provider == "" {
		subWriteError(w, http.StatusBadRequest, "provider is required")
		return
	}
	if req.ModelName == "" {
		subWriteError(w, http.StatusBadRequest, "model_name is required")
		return
	}

	config := &UserModelConfig{
		UserID:    userID,
		Provider:  req.Provider,
		ModelName: req.ModelName,
		BaseURL:   req.BaseURL,
	}
	if req.APIKey != nil {
		config.APIKeyEncrypted = req.APIKey
	}

	result, err := h.modelConfig.TestConnection(r.Context(), config)
	if err != nil {
		subWriteError(w, http.StatusInternalServerError, "connection test failed: "+err.Error())
		return
	}

	subWriteJSON(w, http.StatusOK, result)
}

// ---------------------------------------------------------------------------
// 用量和账单端点
// ---------------------------------------------------------------------------

// handleTodayUsage 返回今日用量汇总
func (h *SubscriptionHandler) handleTodayUsage(w http.ResponseWriter, r *http.Request) {
	userID, err := subGetUserID(r)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			subWriteError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		subWriteError(w, http.StatusUnauthorized, err.Error())
		return
	}

	today := time.Now().Truncate(24 * time.Hour)
	summary, err := h.usageTracker.GetDailySummary(r.Context(), userID, today)
	if err != nil {
		subWriteError(w, http.StatusInternalServerError, "failed to get today's usage: "+err.Error())
		return
	}

	// Also fetch quota info for context
	plan, err := h.subService.GetEffectivePlan(r.Context(), userID)
	if err != nil {
		subWriteError(w, http.StatusInternalServerError, "failed to get plan info: "+err.Error())
		return
	}

	remaining := 0
	dailyLimit := 0
	if plan != nil {
		dailyLimit = plan.MaxCallsPerDay
	}
	if dailyLimit > 0 && summary != nil {
		remaining = dailyLimit - summary.TotalCalls
		if remaining < 0 {
			remaining = 0
		}
	}

	subWriteJSON(w, http.StatusOK, map[string]any{
		"summary":         summary,
		"daily_limit":     dailyLimit,
		"remaining_calls": remaining,
	})
}

// handleMonthlyUsage 返回月度用量汇总，支持 ?month=2026-04
func (h *SubscriptionHandler) handleMonthlyUsage(w http.ResponseWriter, r *http.Request) {
	userID, err := subGetUserID(r)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			subWriteError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		subWriteError(w, http.StatusUnauthorized, err.Error())
		return
	}

	yearMonth := r.URL.Query().Get("month")
	if yearMonth == "" {
		yearMonth = time.Now().Format("2006-01")
	}

	// Validate format YYYY-MM
	if _, err := time.Parse("2006-01", yearMonth); err != nil {
		subWriteError(w, http.StatusBadRequest, "invalid month format, expected YYYY-MM")
		return
	}

	summary, err := h.usageTracker.GetMonthlySummary(r.Context(), userID, yearMonth)
	if err != nil {
		subWriteError(w, http.StatusInternalServerError, "failed to get monthly usage: "+err.Error())
		return
	}

	subWriteJSON(w, http.StatusOK, map[string]any{
		"summary": summary,
	})
}

// handleUsageHistory 分页用量历史，支持 ?offset=0&limit=20
func (h *SubscriptionHandler) handleUsageHistory(w http.ResponseWriter, r *http.Request) {
	userID, err := subGetUserID(r)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			subWriteError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		subWriteError(w, http.StatusUnauthorized, err.Error())
		return
	}

	offset := 0
	limit := 20

	if v := r.URL.Query().Get("offset"); v != "" {
		if parsed, parseErr := strconv.Atoi(v); parseErr == nil && parsed >= 0 {
			offset = parsed
		} else {
			subWriteError(w, http.StatusBadRequest, "invalid offset parameter")
			return
		}
	}

	if v := r.URL.Query().Get("limit"); v != "" {
		if parsed, parseErr := strconv.Atoi(v); parseErr == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		} else {
			subWriteError(w, http.StatusBadRequest, "invalid limit parameter (must be 1-100)")
			return
		}
	}

	entries, total, err := h.usageTracker.GetUsageHistory(r.Context(), userID, offset, limit)
	if err != nil {
		subWriteError(w, http.StatusInternalServerError, "failed to get usage history: "+err.Error())
		return
	}

	subWriteJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"total":   total,
		"offset":  offset,
		"limit":   limit,
	})
}

// handleGetBill 月度账单，支持 ?month=2026-04
func (h *SubscriptionHandler) handleGetBill(w http.ResponseWriter, r *http.Request) {
	userID, err := subGetUserID(r)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			subWriteError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		subWriteError(w, http.StatusUnauthorized, err.Error())
		return
	}

	yearMonth := r.URL.Query().Get("month")
	if yearMonth == "" {
		yearMonth = time.Now().Format("2006-01")
	}

	if _, err := time.Parse("2006-01", yearMonth); err != nil {
		subWriteError(w, http.StatusBadRequest, "invalid month format, expected YYYY-MM")
		return
	}

	bill, err := h.usageTracker.GetBill(r.Context(), userID, yearMonth)
	if err != nil {
		subWriteError(w, http.StatusInternalServerError, "failed to get bill: "+err.Error())
		return
	}

	subWriteJSON(w, http.StatusOK, map[string]any{
		"bill": bill,
	})
}

// handleEstimate 根据当前计划和用量预估月费用
func (h *SubscriptionHandler) handleEstimate(w http.ResponseWriter, r *http.Request) {
	userID, err := subGetUserID(r)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			subWriteError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		subWriteError(w, http.StatusUnauthorized, err.Error())
		return
	}

	// Get effective plan
	plan, err := h.subService.GetEffectivePlan(r.Context(), userID)
	if err != nil {
		subWriteError(w, http.StatusInternalServerError, "failed to get plan: "+err.Error())
		return
	}

	if plan == nil {
		subWriteError(w, http.StatusInternalServerError, "failed to get plan: empty effective plan")
		return
	}

	// Get current month usage
	yearMonth := time.Now().Format("2006-01")
	monthlySummary, err := h.usageTracker.GetMonthlySummary(r.Context(), userID, yearMonth)
	if err != nil {
		subWriteError(w, http.StatusInternalServerError, "failed to get monthly usage: "+err.Error())
		return
	}
	if monthlySummary == nil {
		monthlySummary = &MonthlySummary{YearMonth: yearMonth}
	}

	// Calculate estimate based on days elapsed / total days in month
	now := time.Now()
	daysInMonth := float64(time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, now.Location()).Day())
	daysElapsed := float64(now.Day())

	var estimatedUsageFee float64
	if monthlySummary != nil && daysElapsed > 0 {
		dailyRate := monthlySummary.PriceCents / daysElapsed
		estimatedUsageFee = dailyRate * daysInMonth
	}

	subscriptionFee := float64(plan.PriceCentsMonth)
	estimatedTotal := subscriptionFee + estimatedUsageFee

	subWriteJSON(w, http.StatusOK, map[string]any{
		"plan_tier":           plan.Tier,
		"subscription_fee":    subscriptionFee,
		"current_usage_fee":   monthlySummary.PriceCents,
		"estimated_usage_fee": estimatedUsageFee,
		"estimated_total":     estimatedTotal,
		"days_elapsed":        int(daysElapsed),
		"days_in_month":       int(daysInMonth),
		"year_month":          yearMonth,
	})
}

func (h *SubscriptionHandler) handleGetWallet(w http.ResponseWriter, r *http.Request) {
	userID, err := subGetUserID(r)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			subWriteError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		subWriteError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if h.wallets == nil {
		subWriteError(w, http.StatusServiceUnavailable, "wallet service unavailable")
		return
	}
	wallet, err := h.wallets.GetWallet(r.Context(), userID)
	if err != nil {
		subWriteError(w, http.StatusInternalServerError, "failed to get wallet: "+err.Error())
		return
	}
	if wallet == nil {
		wallet = &WalletAccount{UserID: userID, BaseCurrency: "USD"}
	}
	subWriteJSON(w, http.StatusOK, map[string]any{"wallet": wallet})
}

func (h *SubscriptionHandler) handleWalletLedger(w http.ResponseWriter, r *http.Request) {
	userID, err := subGetUserID(r)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			subWriteError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		subWriteError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if h.wallets == nil {
		subWriteError(w, http.StatusServiceUnavailable, "wallet service unavailable")
		return
	}

	offset := 0
	limit := 20
	if v := r.URL.Query().Get("offset"); v != "" {
		if parsed, parseErr := strconv.Atoi(v); parseErr == nil && parsed >= 0 {
			offset = parsed
		} else {
			subWriteError(w, http.StatusBadRequest, "invalid offset parameter")
			return
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if parsed, parseErr := strconv.Atoi(v); parseErr == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		} else {
			subWriteError(w, http.StatusBadRequest, "invalid limit parameter (must be 1-100)")
			return
		}
	}

	entries, total, err := h.wallets.ListWalletLedger(r.Context(), userID, offset, limit)
	if err != nil {
		subWriteError(w, http.StatusInternalServerError, "failed to get wallet ledger: "+err.Error())
		return
	}
	if entries == nil {
		entries = []WalletLedgerEntry{}
	}
	subWriteJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"total":   total,
		"offset":  offset,
		"limit":   limit,
	})
}
