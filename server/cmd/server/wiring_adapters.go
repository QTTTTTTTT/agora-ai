package main

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/fundai/server/internal/agentreputation"
	"github.com/fundai/server/internal/alphalesson"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/attribution"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/broker"
	"github.com/fundai/server/internal/cooldown"
	"github.com/fundai/server/internal/debate"
	"github.com/fundai/server/internal/correlation"
	"github.com/fundai/server/internal/decision"
	"github.com/fundai/server/internal/decision/errorclass"
	"github.com/fundai/server/internal/earnings"
	"github.com/fundai/server/internal/exitmanager"
	"github.com/fundai/server/internal/exposure"
	"github.com/fundai/server/internal/contradiction"
	"github.com/fundai/server/internal/intraday"
	"github.com/fundai/server/internal/recall"
	"github.com/fundai/server/internal/lowbeta"
	"github.com/fundai/server/internal/regime"
	"github.com/fundai/server/internal/strategy"
	"github.com/fundai/server/internal/fundamental"
	"github.com/fundai/server/internal/indicator"
	instrument2 "github.com/fundai/server/internal/instrument"
	"github.com/fundai/server/internal/llm"
	"github.com/fundai/server/internal/lotledger"
	"github.com/fundai/server/internal/marketcalendar"
	"github.com/fundai/server/internal/marketdata"
	"github.com/fundai/server/internal/marketplace"
	"github.com/fundai/server/internal/modelab"
	"github.com/fundai/server/internal/newsrecall"
	"github.com/fundai/server/internal/ohlc"
	"github.com/fundai/server/internal/pairspread"
	"github.com/fundai/server/internal/pead"
	"github.com/fundai/server/internal/quality"
	"github.com/fundai/server/internal/quantsnapshot"
	"github.com/fundai/server/internal/quota"
	"github.com/fundai/server/internal/ranking"
	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/risk"
	"github.com/fundai/server/internal/riskbudget"
	"github.com/fundai/server/internal/sectorflow"
	"github.com/fundai/server/internal/sentiment"
	"github.com/fundai/server/internal/sizing"
	"github.com/fundai/server/internal/social"
	"github.com/fundai/server/internal/subscription"
	"github.com/fundai/server/internal/value"
	"github.com/fundai/server/internal/workflow"
)

type subscriptionServiceAdapter struct {
	service *subscription.SubscriptionService
}

func newSubscriptionServiceAdapter(service *subscription.SubscriptionService) *subscriptionServiceAdapter {
	return &subscriptionServiceAdapter{service: service}
}

func (a *subscriptionServiceAdapter) ListPlans() []*api.SubscriptionPlan {
	plans := a.service.ListPlans()
	result := make([]*api.SubscriptionPlan, 0, len(plans))
	for _, plan := range plans {
		result = append(result, convertSubscriptionPlan(plan))
	}
	return result
}

func (a *subscriptionServiceAdapter) GetPlan(tier string) (*api.SubscriptionPlan, error) {
	plan, err := a.service.GetPlan(tier)
	if err != nil {
		return nil, err
	}
	return convertSubscriptionPlan(plan), nil
}

func (a *subscriptionServiceAdapter) GetUserSubscription(ctx context.Context, userID string) (*api.Subscription, error) {
	sub, err := a.service.GetUserSubscription(ctx, userID)
	if err != nil || sub == nil {
		return nil, err
	}
	return convertSubscription(sub), nil
}

func (a *subscriptionServiceAdapter) Subscribe(ctx context.Context, userID, tier, paymentMethod string) (*api.Subscription, error) {
	sub, err := a.service.Subscribe(ctx, userID, tier, paymentMethod)
	if err != nil {
		return nil, err
	}
	return convertSubscription(sub), nil
}

func (a *subscriptionServiceAdapter) Cancel(ctx context.Context, userID string) error {
	return a.service.Cancel(ctx, userID)
}

func (a *subscriptionServiceAdapter) GetEffectivePlan(ctx context.Context, userID string) (*api.SubscriptionPlan, error) {
	plan, err := a.service.GetEffectivePlan(ctx, userID)
	if err != nil {
		return nil, err
	}
	return convertSubscriptionPlan(plan), nil
}

func (a *subscriptionServiceAdapter) CheckQuota(ctx context.Context, userID, action string, currentCount int) error {
	return a.service.CheckQuota(ctx, userID, action, currentCount)
}

func (a *subscriptionServiceAdapter) CheckModelAccess(ctx context.Context, userID, modelTier string) error {
	return a.service.CheckModelAccess(ctx, userID, modelTier)
}

func (a *subscriptionServiceAdapter) AllowsCustomKey(ctx context.Context, userID string) (bool, error) {
	return a.service.AllowsCustomKey(ctx, userID)
}

var _ llm.SubscriptionGuard = (*llmRuntime)(nil)

type usageTrackerAdapter struct {
	tracker *subscription.UsageTracker
}

func newUsageTrackerAdapter(tracker *subscription.UsageTracker) *usageTrackerAdapter {
	return &usageTrackerAdapter{tracker: tracker}
}

func (a *usageTrackerAdapter) GetDailySummary(ctx context.Context, userID string, date time.Time) (*api.DailySummary, error) {
	summary, err := a.tracker.GetDailySummary(ctx, userID, date)
	if err != nil || summary == nil {
		return nil, err
	}
	return &api.DailySummary{
		UserID:         summary.UserID,
		SummaryDate:    summary.SummaryDate,
		TotalCalls:     summary.TotalCalls,
		InputTokens:    summary.InputTokens,
		OutputTokens:   summary.OutputTokens,
		CostCents:      summary.CostCents,
		PriceCents:     summary.PriceCents,
		CustomKeyCalls: summary.CustomKeyCalls,
		ModelBreakdown: summary.ModelBreakdown,
		StepBreakdown:  summary.StepBreakdown,
	}, nil
}

func (a *usageTrackerAdapter) GetMonthlySummary(ctx context.Context, userID, yearMonth string) (*api.MonthlySummary, error) {
	summary, err := a.tracker.GetMonthlySummary(ctx, userID, yearMonth)
	if err != nil || summary == nil {
		return nil, err
	}
	return &api.MonthlySummary{
		UserID:         summary.UserID,
		YearMonth:      summary.YearMonth,
		TotalCalls:     summary.TotalCalls,
		InputTokens:    summary.InputTokens,
		OutputTokens:   summary.OutputTokens,
		CostCents:      summary.CostCents,
		PriceCents:     summary.PriceCents,
		CustomKeyCalls: summary.CustomKeyCalls,
		ModelBreakdown: summary.ModelBreakdown,
	}, nil
}

func (a *usageTrackerAdapter) GetUsageHistory(ctx context.Context, userID string, offset, limit int) ([]*api.UsageEntry, int, error) {
	entries, total, err := a.tracker.GetUsageHistory(ctx, userID, offset, limit)
	if err != nil {
		return nil, 0, err
	}
	result := make([]*api.UsageEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, &api.UsageEntry{
			ID:            entry.ID,
			FundID:        entry.FundID,
			StepName:      entry.StepName,
			ModelProvider: entry.ModelProvider,
			ModelName:     entry.ModelName,
			InputTokens:   entry.InputTokens,
			OutputTokens:  entry.OutputTokens,
			CostCents:     entry.CostCents,
			PriceCents:    entry.PriceCents,
			IsCustomKey:   entry.IsCustomKey,
			CreatedAt:     entry.CreatedAt,
		})
	}
	return result, total, nil
}

func (a *usageTrackerAdapter) GetBill(ctx context.Context, userID, yearMonth string) (*api.MonthlyBill, error) {
	bill, err := a.tracker.GetBill(ctx, userID, yearMonth)
	if err != nil || bill == nil {
		return nil, err
	}
	return &api.MonthlyBill{
		ID:              bill.ID,
		UserID:          bill.UserID,
		YearMonth:       bill.YearMonth,
		PlanTier:        bill.PlanTier,
		SubscriptionFee: bill.SubscriptionFee,
		ModelUsageFee:   bill.ModelUsageFee,
		CustomKeyCredit: bill.CustomKeyCredit,
		TotalFee:        bill.TotalFee,
		FinalAmount:     bill.FinalAmount,
		Status:          bill.Status,
		DetailsJSON:     bill.DetailsJSON,
	}, nil
}

type llmRuntime struct {
	client              *llm.MultiProviderClient
	router              *llm.ModelRouter
	modelConfigs        *subscription.ModelConfigService
	subscriptionService *subscription.SubscriptionService
	budgetService       *subscription.BudgetService
	metrics             *serverMetrics
	systemAPIKeys       map[llm.Provider]string
	tierDefaults        map[llm.ModelTier]*llm.ModelConfig
	syncedUsers         map[string]struct{}
	// agentRepo is the SECOND source of truth for an agent's model
	// preference. The router previously only honoured user_model_configs
	// rows of type 'agent_default'; the agents table's own
	// model_provider / model_name columns were ignored — so PMs set
	// via the agent editor (which writes the agents row directly with
	// no user_model_configs row) silently routed to the platform
	// default provider. Set via SetAgentRepo before any SyncAll call.
	// nil = legacy behaviour (test wiring).
	agentRepo agentModelLister

	// modelABResolver is the Sprint 10.1 model A/B hook source.
	// AttachModelABResolver installs it on the router; storing
	// the resolver here lets the admin handlers invalidate the
	// cache when an experiment's status flips. nil = no model
	// A/B (legacy / tests without DB).
	modelABResolver *modelab.Resolver

	// modelABRepo is shared between the resolver, the shadow
	// dispatcher (S10.2), and the admin handlers (S10.3/4).
	// Set via SetModelABRepo before AttachModelABResolver so
	// dispatcher persistence is wired immediately.
	modelABRepo *modelab.Repo

	// modelABDispatcher wraps the multi-provider client to fan
	// out shadow arms when an experiment matches. Returned by
	// LLMClient() so all decision/agent paths see it. nil when
	// no resolver / repo / client is wired.
	modelABDispatcher *modelab.ShadowDispatcher

	// S13 — platform LLM provider table. Owned by the admin UI;
	// hot-reload pushes a fresh (systemAPIKeys, tierDefaults)
	// snapshot into the router via ReplaceSystemConfig. nil =
	// pre-S13 / test wiring; ReloadPlatformProviders is then a
	// no-op so legacy tests keep working.
	platformLLMProviderRepo *repository.PlatformLLMProviderRepo
	envDefaults             LLMDefaultsConfig
	auditLogger             *audit.DBLogger

	// S14.B — fund_llm_overrides. The runtime captures the repo so
	// ReloadFundOverrides can be exposed (the admin handler calls
	// it after PUT/DELETE so subsequent LLM calls observe the change
	// even though the hook itself reads DB live; the call is mostly
	// a forensic audit + metrics tick, NOT a cache invalidation).
	// nil = pre-S14.B / test wiring.
	fundLLMOverrideRepo *repository.FundLLMOverrideRepo
}

// ReloadPlatformProviders re-reads the platform_llm_providers
// table, rebuilds the systemAPIKeys + tierDefaults pair, and
// pushes the result into the router with no app restart.
// Implements the providerReloader interface consumed by the
// admin handler.
func (r *llmRuntime) ReloadPlatformProviders(ctx context.Context) error {
	if r == nil || r.platformLLMProviderRepo == nil || r.router == nil {
		return nil
	}
	snap, err := loadPlatformProviders(ctx, r.platformLLMProviderRepo, r.envDefaults, r.auditLogger)
	if err != nil {
		return fmt.Errorf("reload providers: %w", err)
	}
	r.router.ReplaceSystemConfig(snap.SystemAPIKeys, snap.TierDefaults)
	r.systemAPIKeys = snap.SystemAPIKeys
	r.tierDefaults = snap.TierDefaults
	// Re-attach the model-A/B hook with the fresh snapshot so
	// arm resolution uses the new keys / endpoints. Safe no-op
	// when no resolver is wired.
	if r.modelABResolver != nil {
		r.router.SetModelABHook(r.modelABResolver.AsLLMHook(modelab.HookContext{
			SystemAPIKeys: r.systemAPIKeys,
			TierDefaults:  r.tierDefaults,
		}))
	}
	slog.Info("platform_llm_providers: hot reload complete",
		"reload_generation", llm.ReloadGeneration(),
		"active_provider_keys", len(snap.SystemAPIKeys))
	return nil
}

// agentModelLister narrows repository.AgentRepo to the two methods
// llmRuntime needs, so wiring_test can stub it without dragging the
// full sql.DB in.
type agentModelLister interface {
	ListByUser(ctx context.Context, userID string) ([]repository.Agent, error)
	ListDistinctOwners(ctx context.Context) ([]string, error)
}

// SetAgentRepo wires the agents-table fallback source used by SyncUser
// and SyncAll. Safe to call once before SyncAll.
func (r *llmRuntime) SetAgentRepo(repo agentModelLister) {
	if r == nil {
		return
	}
	r.agentRepo = repo
}

// AttachModelABResolver wires the Sprint 10 model A/B engine
// onto the router AND installs the shadow dispatcher so every
// Chat() call passes through arm selection (S10.1) and parallel
// arm execution (S10.2).
//
// Safe to call before or after SyncAll, but should be called
// AFTER the router exists (i.e. after newLLMRuntime returns).
// A nil resolver detaches any previously attached hook so tests
// can swap implementations cleanly.
func (r *llmRuntime) AttachModelABResolver(resolver *modelab.Resolver) {
	if r == nil || r.router == nil {
		return
	}
	r.modelABResolver = resolver
	if resolver == nil {
		r.router.SetModelABHook(nil)
		r.modelABDispatcher = nil
		return
	}
	hc := modelab.HookContext{
		SystemAPIKeys: r.systemAPIKeys,
		TierDefaults:  r.tierDefaults,
	}
	r.router.SetModelABHook(resolver.AsLLMHook(hc))

	// Sprint 10.2 — shadow dispatcher fans out non-primary arms
	// in parallel and persists their responses. The dispatcher
	// wraps the multi-provider client; callers that consume the
	// runtime through LLMClient() get the wrapped version
	// transparently.
	if r.client != nil && r.modelABRepo != nil {
		r.modelABDispatcher = modelab.NewShadowDispatcher(r.client, resolver, r.modelABRepo, hc)
	}
}

// SetModelABRepo wires the modelab repository onto the runtime
// so the shadow dispatcher (S10.2) can persist non-primary arm
// responses AND the admin handlers (S10.3 / S10.4) can read /
// mutate experiments. Safe to call before AttachModelABResolver.
func (r *llmRuntime) SetModelABRepo(repo *modelab.Repo) {
	if r == nil {
		return
	}
	r.modelABRepo = repo
}

// ModelABRepo returns the runtime's modelab repository (nil
// when modelab isn't wired). Used by the admin handlers.
func (r *llmRuntime) ModelABRepo() *modelab.Repo {
	if r == nil {
		return nil
	}
	return r.modelABRepo
}

// ModelABResolver returns the currently attached resolver. Used
// by the admin CRUD handlers to invalidate the cache after a
// status change (start / pause / complete). Returns nil when no
// resolver has been attached.
func (r *llmRuntime) ModelABResolver() *modelab.Resolver {
	if r == nil {
		return nil
	}
	return r.modelABResolver
}

type llmEffectivePlan struct {
	allowCustomKey bool
}

func (p llmEffectivePlan) AllowsCustomKey() bool {
	return p.allowCustomKey
}

type modelConfigServiceAdapter struct {
	service *subscription.ModelConfigService
	runtime *llmRuntime
}

func newModelConfigServiceAdapter(service *subscription.ModelConfigService, runtime *llmRuntime) *modelConfigServiceAdapter {
	return &modelConfigServiceAdapter{service: service, runtime: runtime}
}

func (a *modelConfigServiceAdapter) SaveConfig(ctx context.Context, config *api.UserModelConfig) error {
	if err := a.service.SaveConfig(ctx, &subscription.UserModelConfig{
		ID:              config.ID,
		UserID:          config.UserID,
		AgentID:         config.AgentID,
		ConfigType:      config.ConfigType,
		Tier:            config.Tier,
		Provider:        config.Provider,
		ModelName:       config.ModelName,
		BaseURL:         config.BaseURL,
		APIKeyEncrypted: config.APIKeyEncrypted,
		IsActive:        config.IsActive,
		CreatedAt:       config.CreatedAt,
		UpdatedAt:       config.UpdatedAt,
	}); err != nil {
		return err
	}
	if a.runtime != nil {
		return a.runtime.SyncUser(ctx, config.UserID)
	}
	return nil
}

func (a *modelConfigServiceAdapter) GetUserConfigs(ctx context.Context, userID string) ([]*api.UserModelConfig, error) {
	configs, err := a.service.GetUserConfigs(ctx, userID)
	if err != nil {
		return nil, err
	}
	result := make([]*api.UserModelConfig, 0, len(configs))
	for _, config := range configs {
		result = append(result, &api.UserModelConfig{
			ID:              config.ID,
			UserID:          config.UserID,
			AgentID:         config.AgentID,
			ConfigType:      config.ConfigType,
			Tier:            config.Tier,
			Provider:        config.Provider,
			ModelName:       config.ModelName,
			BaseURL:         config.BaseURL,
			APIKeyEncrypted: config.APIKeyEncrypted,
			IsActive:        config.IsActive,
			CreatedAt:       config.CreatedAt,
			UpdatedAt:       config.UpdatedAt,
		})
	}
	return result, nil
}

func (a *modelConfigServiceAdapter) DeleteConfig(ctx context.Context, userID, configID string) error {
	if err := a.service.DeleteConfig(ctx, userID, configID); err != nil {
		return err
	}
	if a.runtime != nil {
		return a.runtime.SyncUser(ctx, userID)
	}
	return nil
}

func (a *modelConfigServiceAdapter) TestConnection(ctx context.Context, config *api.UserModelConfig) (*api.ConnectionTestResult, error) {
	result, err := a.service.TestConnection(ctx, &subscription.UserModelConfig{
		ID:              config.ID,
		UserID:          config.UserID,
		AgentID:         config.AgentID,
		ConfigType:      config.ConfigType,
		Tier:            config.Tier,
		Provider:        config.Provider,
		ModelName:       config.ModelName,
		BaseURL:         config.BaseURL,
		APIKeyEncrypted: config.APIKeyEncrypted,
		IsActive:        config.IsActive,
		CreatedAt:       config.CreatedAt,
		UpdatedAt:       config.UpdatedAt,
	})
	if err != nil || result == nil {
		return nil, err
	}
	return &api.ConnectionTestResult{
		Success: result.Success,
		Latency: result.Latency,
		Message: result.Message,
		ModelID: result.ModelID,
	}, nil
}

func newLLMRuntime(ctx context.Context, modelConfigs *subscription.ModelConfigService, usageTracker *subscription.UsageTracker, subscriptionService *subscription.SubscriptionService, budgetService *subscription.BudgetService, quotaService *quota.Service, metrics *serverMetrics, defaults LLMDefaultsConfig) (*llmRuntime, error) {
	return newLLMRuntimeWithProviderRepo(ctx, modelConfigs, usageTracker, subscriptionService, budgetService, quotaService, metrics, defaults, nil, nil)
}

// SetFundLLMOverrideRepo wires the S14.B override repo + installs
// the fund-override hook onto the router. Called by main.go right
// after the override repo is constructed. Safe to call once; passing
// nil disables the hook (e.g. tests).
func (r *llmRuntime) SetFundLLMOverrideRepo(repo *repository.FundLLMOverrideRepo) {
	if r == nil || r.router == nil {
		return
	}
	r.fundLLMOverrideRepo = repo
	rt := newFundOverrideRuntime(repo, r.platformLLMProviderRepo, slog.Default())
	if rt == nil {
		r.router.SetFundOverrideHook(nil)
		return
	}
	r.router.SetFundOverrideHook(rt.Hook())
	slog.Info("fund_llm_overrides: hook installed")
}

// newLLMRuntimeWithProviderRepo is the S13 entry point. When repo
// is non-nil it loads the (systemAPIKeys, tierDefaults) pair from
// the platform_llm_providers table (env-seeded on first boot);
// when repo is nil the runtime falls back to pre-S13 env-only
// behaviour. auditLogger is used for the initial_env_seed audit
// trail and may be nil in tests.
func newLLMRuntimeWithProviderRepo(
	ctx context.Context,
	modelConfigs *subscription.ModelConfigService,
	usageTracker *subscription.UsageTracker,
	subscriptionService *subscription.SubscriptionService,
	budgetService *subscription.BudgetService,
	quotaService *quota.Service,
	metrics *serverMetrics,
	defaults LLMDefaultsConfig,
	providerRepo *repository.PlatformLLMProviderRepo,
	auditLogger *audit.DBLogger,
) (*llmRuntime, error) {
	snap, err := loadPlatformProviders(ctx, providerRepo, defaults, auditLogger)
	if err != nil {
		return nil, fmt.Errorf("platform_llm_providers: load: %w", err)
	}
	systemKeys := snap.SystemAPIKeys
	defaultModels := snap.TierDefaults
	runtime := &llmRuntime{
		modelConfigs:            modelConfigs,
		subscriptionService:     subscriptionService,
		metrics:                 metrics,
		systemAPIKeys:           systemKeys,
		tierDefaults:            defaultModels,
		syncedUsers:             make(map[string]struct{}),
		platformLLMProviderRepo: providerRepo,
		envDefaults:             defaults,
		auditLogger:             auditLogger,
	}
	router := llm.NewModelRouter(systemKeys, defaultModels, newUsageRecorderAdapter(usageTracker), runtime)
	client := llm.NewMultiProviderClientWithObserver(router, systemKeys, runtime.metrics)
	// 按 (owner, provider) 维度做限流和熔断，避免一个 fund owner
	// 把另一个 owner 的额度或下游 provider 打爆。
	client.SetOwnerLimiter(llm.NewOwnerLimiter(llm.DefaultLimiterConfig()))
	client.SetChatCache(llm.NewChatCache(llm.ChatCacheConfig{Enabled: true, TTL: 10 * time.Minute, MaxEntries: 1024}))
	client.SetCallBudgetLimiter(llm.NewCallBudgetLimiter(llm.DefaultCallBudgetConfig()))
	// F14: dollar-cap hard gate. Wraps subscription.BudgetService so
	// errors satisfy both subscription.IsLLMBudgetExceeded (admin UI)
	// AND workflow.ErrLLMBudgetExceeded (orchestrator pause logic).
	if budgetService != nil {
		client.SetDollarBudgetGate(newDollarBudgetGateAdapter(budgetService))
	}
	// F28: per-fund token quota gate. Orthogonal to F14: dollar gate
	// caps a user's spend, this gate caps a fund's token throughput.
	// Both can reject a call independently.
	if quotaService != nil {
		client.SetFundQuotaGate(quotaService)
	}
	// F15: provider failover chain. On primary 5xx / circuit-open,
	// retry the same request against the next provider in the chain.
	// Auto-switch-back is automatic — each new Chat() call starts at
	// the head of the chain.
	// The failover chain's primary providers come from the
	// production defaults (openai / deepseek / etc.). On envs
	// where only the platform-default provider (gemini / claude /
	// whatever LLM_PROVIDER points at) has a populated API key,
	// the default chain can exhaust without ever hitting the only
	// reachable provider. WithPlatformDefault appends LLM_PROVIDER
	// to every tier chain as a guaranteed safety net so an agent
	// configured for an unkeyed provider can still get serviced
	// instead of failing with the "every researcher failed"
	// pattern surfaced during Sprint C verification.
	platformDefault := llm.Provider(strings.ToLower(strings.TrimSpace(defaults.Global.Provider)))
	failoverCfg := llm.DefaultFailoverConfig().WithPlatformDefault(platformDefault)
	client.SetFailoverConfig(failoverCfg)
	runtime.client = client
	runtime.router = router
	runtime.budgetService = budgetService
	if err := runtime.SyncAll(ctx); err != nil {
		return nil, err
	}
	return runtime, nil
}

func (r *llmRuntime) ListModels(ctx context.Context) ([]api.ModelInfo, error) {
	models, err := r.client.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]api.ModelInfo, 0, len(models))
	for _, model := range models {
		available := false
		if key := strings.TrimSpace(r.systemAPIKeys[llm.Provider(model.Provider)]); key != "" {
			available = true
		}
		result = append(result, api.ModelInfo{
			Provider:    model.Provider,
			ModelName:   model.ModelName,
			DisplayName: model.DisplayName,
			Tier:        model.Tier,
			InputPrice:  model.InputPricePer1M / 1000,
			OutputPrice: model.OutputPricePer1M / 1000,
			Available:   available,
		})
	}
	return result, nil
}

// LLMClient exposes the LLM client decision/agent paths consume.
// When a Sprint 10.2 ShadowDispatcher is attached we return it
// transparently so every business call goes through the
// experiment fan-out path. Without a dispatcher the runtime
// returns the raw MultiProviderClient — identical to pre-10
// behaviour. Returns nil if the runtime was constructed without
// a client (legacy test paths).
func (r *llmRuntime) LLMClient() llm.LLMClient {
	if r == nil {
		return nil
	}
	if r.modelABDispatcher != nil {
		return r.modelABDispatcher
	}
	return r.client
}

func (r *llmRuntime) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	if r == nil || r.client == nil {
		return nil, fmt.Errorf("llm runtime unavailable")
	}
	resp, err := r.client.Chat(ctx, req)
	provider := "unknown"
	model := strings.TrimSpace(req.Model)
	baseURL := ""
	if config, resolveErr := r.router.ResolveModel(ctx, &req); resolveErr == nil && config != nil {
		provider = string(config.Provider)
		if strings.TrimSpace(config.ModelName) != "" {
			model = config.ModelName
		}
		baseURL = strings.TrimSpace(config.BaseURL)
	}
	status := "success"
	if err != nil {
		status = "error"
	}
	slog.Info("llm runtime chat", "step", req.StepName, "provider", provider, "model", model, "baseUrl", baseURL, "status", status, "userId", strings.TrimSpace(req.UserID), "agentId", strings.TrimSpace(req.AgentID))
	return resp, err
}

func (r *llmRuntime) CheckModelAccess(ctx context.Context, userID, modelTier string) error {
	if r == nil || r.subscriptionService == nil {
		return nil
	}
	return r.subscriptionService.CheckModelAccess(ctx, userID, modelTier)
}

func (r *llmRuntime) GetEffectivePlan(ctx context.Context, userID string) (llm.EffectivePlan, error) {
	if r == nil || r.subscriptionService == nil {
		return nil, nil
	}
	plan, err := r.subscriptionService.GetEffectivePlan(ctx, userID)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, nil
	}
	return llmEffectivePlan{allowCustomKey: plan.AllowCustomKey}, nil
}

func (r *llmRuntime) SyncAll(ctx context.Context) error {
	configs, err := r.modelConfigs.ListRuntimeConfigs(ctx)
	if err != nil {
		return err
	}
	groupedOverrides, groupedEndpoints, groupedAgentDefaults, nextUsers := groupRuntimeConfigs(configs)
	// Augment nextUsers with anyone who configured agent models
	// directly through the agent editor (agents.model_provider/name)
	// without a corresponding user_model_configs row. Otherwise the
	// router never learns about their PM/researcher model preferences
	// at startup, and the first call falls through to the platform
	// default — the P2 symptom for tong on 2026-05-22.
	if r.agentRepo != nil {
		owners, listErr := r.agentRepo.ListDistinctOwners(ctx)
		if listErr != nil {
			slog.Warn("llm runtime: distinct owners list failed", "err", listErr)
		} else {
			for _, owner := range owners {
				owner = strings.TrimSpace(owner)
				if owner == "" {
					continue
				}
				if _, ok := nextUsers[owner]; !ok {
					nextUsers[owner] = struct{}{}
				}
			}
		}
	}
	for userID := range r.syncedUsers {
		if _, ok := nextUsers[userID]; !ok {
			r.router.ReplaceUserConfigs(userID, nil, nil)
			r.router.ReplaceAgentConfigs(userID, nil)
		}
	}
	for userID := range nextUsers {
		r.router.ReplaceUserConfigs(userID, groupedOverrides[userID], groupedEndpoints[userID])
		// Merge per-user agent_default rows with the agents-table
		// fallback for this user. Explicit user_model_configs rows
		// still win — fallback only fills the gaps.
		merged := groupedAgentDefaults[userID]
		if r.agentRepo != nil {
			fallback := r.collectAgentsFallback(ctx, userID)
			if len(fallback) > 0 {
				if merged == nil {
					merged = make(map[string]*llm.ModelConfig, len(fallback))
				}
				for agentID, cfg := range fallback {
					if _, hasExplicit := merged[agentID]; hasExplicit {
						continue
					}
					merged[agentID] = cfg
				}
			}
		}
		r.router.ReplaceAgentConfigs(userID, merged)
	}
	r.syncedUsers = nextUsers
	return nil
}

// collectAgentsFallback returns the agents-table fallback agentDefaults
// for one user. Best-effort: any read error is logged and an empty
// map is returned so SyncAll still wires up user_model_configs.
func (r *llmRuntime) collectAgentsFallback(ctx context.Context, userID string) map[string]*llm.ModelConfig {
	if r == nil || r.agentRepo == nil || strings.TrimSpace(userID) == "" {
		return nil
	}
	agents, err := r.agentRepo.ListByUser(ctx, userID)
	if err != nil {
		slog.Warn("llm runtime: agents fallback per-user list failed", "userId", userID, "err", err)
		return nil
	}
	out := make(map[string]*llm.ModelConfig, len(agents))
	for _, a := range agents {
		id := strings.TrimSpace(a.ID)
		if id == "" {
			continue
		}
		provider := strings.TrimSpace(a.ModelProvider.String)
		modelName := strings.TrimSpace(a.ModelName.String)
		if provider == "" || modelName == "" {
			continue
		}
		if cfg := agentRowToModelConfig(provider, modelName); cfg != nil {
			out[id] = cfg
		}
	}
	return out
}

func (r *llmRuntime) SyncUser(ctx context.Context, userID string) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil
	}
	configs, err := r.modelConfigs.ListRuntimeConfigs(ctx)
	if err != nil {
		return err
	}
	overrides := map[llm.ModelTier]*llm.ModelConfig{}
	endpoints := map[llm.Provider]*llm.ModelConfig{}
	agentDefaults := map[string]*llm.ModelConfig{}
	for _, cfg := range configs {
		if cfg == nil || cfg.UserID != userID || !cfg.IsActive {
			continue
		}
		if cfg.ConfigType == "tier_override" && cfg.Tier != nil {
			overrides[*cfg.Tier] = toLLMModelConfig(cfg)
		}
		if cfg.ConfigType == "custom_endpoint" {
			endpoints[cfg.Provider] = toLLMModelConfig(cfg)
		}
		if cfg.ConfigType == "agent_default" && cfg.AgentID != nil {
			agentDefaults[strings.TrimSpace(*cfg.AgentID)] = toLLMModelConfig(cfg)
		}
	}
	// Agents-table fallback. The agent editor writes model_provider /
	// model_name on the agents row directly, often without creating a
	// corresponding user_model_configs(agent_default) row. Without
	// this fallback the router can't see those preferences and every
	// PM/researcher LLM call routes to the platform default — which
	// was exactly tong's symptom (PM agent set to claude but every
	// request went to gemini, see P2 diagnosis 2026-05-22). Explicit
	// user_model_configs rows still win because we only fill gaps.
	if r.agentRepo != nil {
		agents, listErr := r.agentRepo.ListByUser(ctx, userID)
		if listErr != nil {
			slog.Warn("llm runtime: agents-table fallback list failed", "userId", userID, "err", listErr)
		} else {
			for _, a := range agents {
				id := strings.TrimSpace(a.ID)
				if id == "" {
					continue
				}
				if _, hasExplicit := agentDefaults[id]; hasExplicit {
					continue
				}
				provider := strings.TrimSpace(a.ModelProvider.String)
				modelName := strings.TrimSpace(a.ModelName.String)
				if provider == "" || modelName == "" {
					continue
				}
				cfg := agentRowToModelConfig(provider, modelName)
				if cfg == nil {
					continue
				}
				agentDefaults[id] = cfg
			}
		}
	}
	r.router.ReplaceUserConfigs(userID, overrides, endpoints)
	r.router.ReplaceAgentConfigs(userID, agentDefaults)
	if len(overrides) == 0 && len(endpoints) == 0 && len(agentDefaults) == 0 {
		delete(r.syncedUsers, userID)
		return nil
	}
	r.syncedUsers[userID] = struct{}{}
	return nil
}

// agentRowToModelConfig synthesises a router ModelConfig from the
// provider/model_name stored on the agents row. Tier defaults to
// Critical because PM / debate / sentiment paths all set ModelTier
// explicitly anyway, and the router's finalizeConfig respects that
// override. Provider-specific BaseURL is filled from providerDefaultBaseURL;
// the ensureAPIKey path at request time will inject the system key.
// Returns nil only on unknown providers (defensive — typed string
// in the DB is supposed to be already normalised to llm.Provider).
func agentRowToModelConfig(provider, modelName string) *llm.ModelConfig {
	provider = strings.TrimSpace(strings.ToLower(provider))
	modelName = strings.TrimSpace(modelName)
	if provider == "" || modelName == "" {
		return nil
	}
	cfg := &llm.ModelConfig{
		Provider:    llm.Provider(provider),
		ModelName:   modelName,
		BaseURL:     providerDefaultBaseURL(llm.Provider(provider)),
		MaxTokens:   4096,
		Temperature: 0.7,
	}
	return cfg
}

// dollarBudgetGateAdapter satisfies llm.DollarBudgetGate by delegating
// to subscription.BudgetService. The crucial trick: when Check returns
// subscription.ErrLLMBudgetExceeded, we wrap it so the returned error
// also satisfies errors.Is(workflow.ErrLLMBudgetExceeded). That allows
// the orchestrator's runStep to recognise budget exhaustion and pause
// the run without importing the subscription package.
type dollarBudgetGateAdapter struct {
	svc *subscription.BudgetService
}

func newDollarBudgetGateAdapter(svc *subscription.BudgetService) *dollarBudgetGateAdapter {
	return &dollarBudgetGateAdapter{svc: svc}
}

func (a *dollarBudgetGateAdapter) Check(ctx context.Context, userID, fundID string) error {
	if a == nil || a.svc == nil {
		return nil
	}
	if err := a.svc.Check(ctx, userID, fundID); err != nil {
		if subscription.IsLLMBudgetExceeded(err) {
			// Both sentinels are reachable via errors.Is — the workflow
			// orchestrator matches workflow.ErrLLMBudgetExceeded, while
			// the admin UI can still call subscription.IsLLMBudgetExceeded
			// on the wire-level error string.
			return &budgetExceededError{wrapped: err}
		}
		return err
	}
	return nil
}

// budgetExceededError carries both the subscription-layer descriptor
// AND the workflow sentinel via the Unwrap chain. errors.Is(err,
// workflow.ErrLLMBudgetExceeded) walks Unwrap and matches.
type budgetExceededError struct {
	wrapped error
}

func (e *budgetExceededError) Error() string {
	if e == nil || e.wrapped == nil {
		return workflow.ErrLLMBudgetExceeded.Error()
	}
	return e.wrapped.Error()
}

func (e *budgetExceededError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.wrapped
}

func (e *budgetExceededError) Is(target error) bool {
	// Only claim to match the workflow sentinel when the wrapped error
	// is in fact a budget error. The adapter should never construct a
	// bridge for non-budget errors, but defending here guards against
	// future refactors that might forward unrelated errors through.
	if target == workflow.ErrLLMBudgetExceeded {
		return e != nil && subscription.IsLLMBudgetExceeded(e.wrapped)
	}
	return false
}

type usageRecorderAdapter struct {
	tracker *subscription.UsageTracker
}

func newUsageRecorderAdapter(tracker *subscription.UsageTracker) *usageRecorderAdapter {
	return &usageRecorderAdapter{tracker: tracker}
}

func (a *usageRecorderAdapter) Record(ctx context.Context, record *llm.UsageRecord) error {
	if record == nil {
		return nil
	}
	var fundID *string
	if strings.TrimSpace(record.FundID) != "" {
		value := strings.TrimSpace(record.FundID)
		fundID = &value
	}
	var agentID *string
	if strings.TrimSpace(record.AgentID) != "" {
		value := strings.TrimSpace(record.AgentID)
		agentID = &value
	}
	return a.tracker.Record(ctx, &subscription.UsageEntry{
		UserID:        record.UserID,
		FundID:        fundID,
		AgentID:       agentID,
		StepName:      record.StepName,
		ModelProvider: record.Provider,
		ModelName:     record.Model,
		InputTokens:   record.InputTokens,
		OutputTokens:  record.OutputTokens,
		CostCents:     record.Cost,
		PriceCents:    record.Price,
		IsCustomKey:   record.IsCustomKey,
		CreatedAt:     record.CreatedAt,
	})
}

func (a *usageRecorderAdapter) GetDailyUsage(context.Context, string, string) ([]llm.UsageRecord, error) {
	return nil, nil
}

func (a *usageRecorderAdapter) GetMonthlyUsage(ctx context.Context, userID string, yearMonth string) (*llm.UsageSummary, error) {
	summary, err := a.tracker.GetMonthlySummary(ctx, userID, yearMonth)
	if err != nil || summary == nil {
		return nil, err
	}
	result := &llm.UsageSummary{
		TotalInputTokens:  summary.InputTokens,
		TotalOutputTokens: summary.OutputTokens,
		TotalCost:         summary.CostCents,
		TotalPrice:        summary.PriceCents,
		Profit:            summary.PriceCents - summary.CostCents,
		ByModel:           make(map[string]*llm.ModelUsage),
		ByStep:            make(map[string]*llm.StepUsage),
	}
	return result, nil
}

func groupRuntimeConfigs(configs []*subscription.RuntimeModelConfig) (map[string]map[llm.ModelTier]*llm.ModelConfig, map[string]map[llm.Provider]*llm.ModelConfig, map[string]map[string]*llm.ModelConfig, map[string]struct{}) {
	overrides := make(map[string]map[llm.ModelTier]*llm.ModelConfig)
	endpoints := make(map[string]map[llm.Provider]*llm.ModelConfig)
	agentDefaults := make(map[string]map[string]*llm.ModelConfig)
	users := make(map[string]struct{})
	for _, cfg := range configs {
		if cfg == nil || !cfg.IsActive || strings.TrimSpace(cfg.UserID) == "" {
			continue
		}
		users[cfg.UserID] = struct{}{}
		switch cfg.ConfigType {
		case "tier_override":
			if cfg.Tier == nil {
				continue
			}
			if overrides[cfg.UserID] == nil {
				overrides[cfg.UserID] = make(map[llm.ModelTier]*llm.ModelConfig)
			}
			overrides[cfg.UserID][*cfg.Tier] = toLLMModelConfig(cfg)
		case "custom_endpoint":
			if endpoints[cfg.UserID] == nil {
				endpoints[cfg.UserID] = make(map[llm.Provider]*llm.ModelConfig)
			}
			endpoints[cfg.UserID][cfg.Provider] = toLLMModelConfig(cfg)
		case "agent_default":
			if cfg.AgentID == nil || strings.TrimSpace(*cfg.AgentID) == "" {
				continue
			}
			if agentDefaults[cfg.UserID] == nil {
				agentDefaults[cfg.UserID] = make(map[string]*llm.ModelConfig)
			}
			agentDefaults[cfg.UserID][strings.TrimSpace(*cfg.AgentID)] = toLLMModelConfig(cfg)
		}
	}
	return overrides, endpoints, agentDefaults, users
}

func toLLMModelConfig(cfg *subscription.RuntimeModelConfig) *llm.ModelConfig {
	if cfg == nil {
		return nil
	}
	var modelConfig *llm.ModelConfig
	if cfg.Tier != nil {
		if defaultCfg, ok := llm.DefaultModels[*cfg.Tier]; ok {
			modelConfig = defaultCfg.Clone()
		}
	}
	if modelConfig == nil {
		modelConfig = &llm.ModelConfig{MaxTokens: 4096, Temperature: 0.7}
	}
	modelConfig.Provider = cfg.Provider
	modelConfig.ModelName = cfg.ModelName
	if strings.TrimSpace(cfg.BaseURL) != "" {
		modelConfig.BaseURL = strings.TrimSpace(cfg.BaseURL)
	} else if modelConfig.BaseURL == "" {
		modelConfig.BaseURL = providerDefaultBaseURL(cfg.Provider)
	}
	modelConfig.APIKey = strings.TrimSpace(cfg.APIKey)
	if cfg.Tier != nil {
		if info := llm.FindModelInfoByTier(cfg.ModelName, string(*cfg.Tier)); info != nil {
			modelConfig.InputPricePer1M = info.InputPricePer1M
			modelConfig.OutputPricePer1M = info.OutputPricePer1M
			modelConfig.CostPer1M = info.InputPricePer1M * 0.7
		}
	} else if info := llm.FindModelInfo(cfg.ModelName); info != nil {
		modelConfig.InputPricePer1M = info.InputPricePer1M
		modelConfig.OutputPricePer1M = info.OutputPricePer1M
		modelConfig.CostPer1M = info.InputPricePer1M * 0.7
	}
	return modelConfig
}

func buildPlatformDefaultModels(defaults LLMDefaultsConfig) map[llm.ModelTier]*llm.ModelConfig {
	result := make(map[llm.ModelTier]*llm.ModelConfig, len(llm.DefaultModels))
	resolved := map[llm.ModelTier]LLMEnvModelConfig{
		llm.TierCritical: defaults.Critical,
		llm.TierStandard: defaults.Standard,
		llm.TierSimple:   defaults.Simple,
	}
	for _, tier := range llm.ValidTiers {
		base := llm.DefaultModels[tier]
		if base == nil {
			continue
		}
		result[tier] = applyEnvModelConfig(base.Clone(), resolved[tier], tier)
	}
	return result
}

func applyEnvModelConfig(modelConfig *llm.ModelConfig, cfg LLMEnvModelConfig, tier llm.ModelTier) *llm.ModelConfig {
	if modelConfig == nil {
		modelConfig = &llm.ModelConfig{MaxTokens: 4096, Temperature: 0.7}
	}
	previousProvider := modelConfig.Provider
	if provider := llmProviderFromString(cfg.Provider); provider != "" {
		modelConfig.Provider = provider
	}
	if model := strings.TrimSpace(cfg.Model); model != "" {
		modelConfig.ModelName = model
	} else if modelConfig.ModelName == "" || previousProvider != modelConfig.Provider {
		if fallback := defaultModelNameForProviderTier(modelConfig.Provider, tier); fallback != "" {
			modelConfig.ModelName = fallback
		}
	}
	if baseURL := strings.TrimSpace(cfg.BaseURL); baseURL != "" {
		modelConfig.BaseURL = baseURL
	} else if modelConfig.BaseURL == "" || previousProvider != modelConfig.Provider {
		modelConfig.BaseURL = providerDefaultBaseURL(modelConfig.Provider)
	}
	if apiKey := strings.TrimSpace(cfg.APIKey); apiKey != "" {
		modelConfig.APIKey = apiKey
	}
	if info := llm.FindModelInfoByTier(modelConfig.ModelName, string(tier)); info != nil {
		modelConfig.InputPricePer1M = info.InputPricePer1M
		modelConfig.OutputPricePer1M = info.OutputPricePer1M
		modelConfig.CostPer1M = info.InputPricePer1M * 0.7
	} else if info := llm.FindModelInfo(modelConfig.ModelName); info != nil {
		modelConfig.InputPricePer1M = info.InputPricePer1M
		modelConfig.OutputPricePer1M = info.OutputPricePer1M
		modelConfig.CostPer1M = info.InputPricePer1M * 0.7
	}
	return modelConfig
}

func llmProviderFromString(value string) llm.Provider {
	switch normalizeLLMProvider(value) {
	case "openai":
		return llm.ProviderOpenAI
	case "claude":
		return llm.ProviderClaude
	case "deepseek":
		return llm.ProviderDeepSeek
	case "qwen":
		return llm.ProviderQwen
	case "gemini":
		return llm.ProviderGemini
	case "custom":
		return llm.ProviderCustom
	default:
		return ""
	}
}

func defaultModelNameForProviderTier(provider llm.Provider, tier llm.ModelTier) string {
	for _, model := range llm.PlatformModels {
		if llm.Provider(model.Provider) == provider && llm.ModelTier(model.Tier) == tier && model.IsDefault {
			return model.ModelName
		}
	}
	for _, model := range llm.PlatformModels {
		if llm.Provider(model.Provider) == provider && llm.ModelTier(model.Tier) == tier {
			return model.ModelName
		}
	}
	return ""
}

func providerDefaultBaseURL(provider llm.Provider) string {
	switch provider {
	case llm.ProviderOpenAI:
		if baseURL := strings.TrimSpace(firstNonEmptyEnv("OPENAI_BASE_URL")); baseURL != "" {
			return baseURL
		}
		return "https://api.openai.com/v1"
	case llm.ProviderClaude:
		if baseURL := strings.TrimSpace(firstNonEmptyEnv("CLAUDE_BASE_URL", "ANTHROPIC_BASE_URL")); baseURL != "" {
			return baseURL
		}
		return "https://api.anthropic.com/v1"
	case llm.ProviderDeepSeek:
		if baseURL := strings.TrimSpace(firstNonEmptyEnv("DEEPSEEK_BASE_URL")); baseURL != "" {
			return baseURL
		}
		return "https://api.deepseek.com/v1"
	case llm.ProviderQwen:
		if baseURL := strings.TrimSpace(firstNonEmptyEnv("QWEN_BASE_URL")); baseURL != "" {
			return baseURL
		}
		return "https://dashscope.aliyuncs.com/compatible-mode/v1"
	case llm.ProviderGemini:
		if baseURL := strings.TrimSpace(firstNonEmptyEnv("GEMINI_BASE_URL", "GOOGLE_BASE_URL")); baseURL != "" {
			return baseURL
		}
		return "https://generativelanguage.googleapis.com/v1beta"
	default:
		return ""
	}
}

func loadSystemLLMKeys(defaults LLMDefaultsConfig) map[llm.Provider]string {
	keys := map[llm.Provider]string{}
	for provider, value := range map[llm.Provider]string{
		llm.ProviderOpenAI:   strings.TrimSpace(firstNonEmptyEnv("OPENAI_API_KEY")),
		llm.ProviderClaude:   strings.TrimSpace(firstNonEmptyEnv("CLAUDE_API_KEY", "ANTHROPIC_API_KEY")),
		llm.ProviderDeepSeek: strings.TrimSpace(firstNonEmptyEnv("DEEPSEEK_API_KEY")),
		llm.ProviderQwen:     strings.TrimSpace(firstNonEmptyEnv("QWEN_API_KEY")),
		llm.ProviderGemini:   strings.TrimSpace(firstNonEmptyEnv("GEMINI_API_KEY", "GOOGLE_API_KEY")),
	} {
		if value != "" {
			keys[provider] = value
		}
	}
	for _, cfg := range []LLMEnvModelConfig{defaults.Global, defaults.Critical, defaults.Standard, defaults.Simple} {
		provider := llmProviderFromString(cfg.Provider)
		if provider == "" {
			continue
		}
		if value := strings.TrimSpace(cfg.APIKey); value != "" {
			keys[provider] = value
		}
	}
	return keys
}

func firstNonEmptyEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

type fundServiceAdapter struct {
	db              *sql.DB
	companyRepo     *repository.FundCompanyRepo
	fundRepo        *repository.FundRepo
	workflowService *workflowServiceAdapter
}

type planServiceAdapter struct {
	planRepo        *repository.PlanRepo
	fundRepo        *repository.FundRepo
	companyRepo     *repository.FundCompanyRepo
	workflowService *workflowServiceAdapter
	llmRuntime      *llmRuntime
}

type tradeServiceAdapter struct {
	fundRepo     *repository.FundRepo
	companyRepo  *repository.FundCompanyRepo
	auditLogger  audit.Logger
	tradeRepo    *repository.TradeRepo
	positionRepo *repository.PositionRepo
	navRepo      *repository.NavSnapshotRepo
	// lotRepo is the source of truth for realised P&L. GetPnLAttribution
	// reads closed_lots.realized_pnl (per-roundtrip net P&L computed at
	// close time by the lotledger) instead of summing sell-side trade
	// notionals — the old approach mistook gross sale proceeds for P&L
	// and inflated realised by a factor of "cost basis ÷ profit", which
	// made every sell look like a 100× win. nil-safe: GetPnLAttribution
	// falls back to a (0, 0) realised report when the lot repo is not
	// wired so unit tests with stub adapters keep working.
	lotRepo *repository.LotRepo
	// marketData powers the live-quote overlay on GetPortfolio / portfolio
	// overview. nil-safe: when not wired, the adapter degrades to the
	// legacy DB-cached price (matches pre-PR-2 behaviour). Set via
	// WithMarketData() during server bootstrap.
	marketData *marketdata.Service
}

type workflowServiceAdapter struct {
	db                  *sql.DB
	fundRepo            *repository.FundRepo
	companyRepo         *repository.FundCompanyRepo
	workflowRepo        *repository.WorkflowRunRepo
	planRepo            *repository.PlanRepo
	// activityRepo is the persistence sidecar for ActivityBus. It backs
	// the Team Live Activity timeline so a container restart no longer
	// blanks the panel. Nil when the adapter is constructed without a
	// DB (test paths), which transparently degrades the bus to its
	// original in-memory-only mode.
	activityRepo        *repository.WorkflowActivityRepo
	subscriptionService *subscription.SubscriptionService
	marketData          *marketdata.Service
	calendar            *marketcalendar.Service
	metrics             *serverMetrics
	// quotaService enforces F28 per-fund concurrent workflow caps.
	// Nil-safe: workflows start unconstrained when the service is
	// unwired (tests, single-binary smoke checks).
	quotaService        *quota.Service
	// activityBus fans out workflow.WorkflowEvent to in-process subscribers
	// (REST backfill + SSE stream). Constructed once per process and shared
	// across all per-fund orchestrators so subscribers see events regardless
	// of which orchestrator instance emitted them.
	activityBus        *workflow.ActivityBus
	// runtime is the LLM client used by daily-review-time helpers such as
	// the long-term memory reflector. Optional: when nil the reflector is
	// skipped silently so tests and dev environments without LLM keys do
	// not spam errors.
	runtime            *llmRuntime
	resumePlan         func(fundID string, tradingDate time.Time, planID string) error
	rejectAwaitingPlan func(fundID string, tradingDate time.Time, planID, reason string) error
	// ohlcFetcher is the shared Phase 2C OHLC source (typically a
	// *ohlc.Cache wrapping a registry with Yahoo / Binance /
	// Akshare providers). Nil-safe: the runtime pool treats nil as
	// "no indicator overlay" and silently falls back to legacy
	// qualitative signals.
	ohlcFetcher        ohlc.Fetcher
	// fundamentalFetcher is the shared Phase 2D fundamentals source
	// (a *fundamental.Cache wrapping a registry with Yahoo /
	// Akshare providers). Same nil-safe contract as ohlcFetcher.
	fundamentalFetcher fundamental.Fetcher
	// sectorFlowFetcher is the shared Phase 2D sector-rotation
	// source. nil = no rotation block in the macro brief.
	sectorFlowFetcher  sectorflow.Fetcher
	// sentimentScorer is the shared Phase 2D news sentiment scorer
	// (typically a *sentiment.CompositeScorer chaining LLMScorer →
	// KeywordScorer). nil = the workflow falls back to passing raw
	// headlines through to the LLM agents without scoring.
	sentimentScorer    sentiment.Scorer
	// socialRegistry is the Sprint 9.3 retail-mood feed (Xueqiu /
	// StockTwits / Reddit-WSB). Wired alongside sentimentScorer so
	// the macro / debate sentiment paths see SOCIAL POSTS as
	// additional sentiment.Item rows on top of the news flow. Nil
	// (or empty registry) = feature off and the workflow falls
	// back to news-only sentiment, matching pre-9.3 behaviour.
	socialRegistry     *social.Registry
	// attribution is the Phase 3A-5 closed-lot attribution
	// service. Optional — when nil the daily review hook
	// silently skips the attribution pass. Wired by main.go
	// alongside the api.AttributionService adapter.
	attribution        *attribution.Service
	// recallService + recallEmbedder are the Sprint 3 / L3
	// pgvector-backed semantic memory recall pair. Both nil
	// = feature off (DecisionInput.SemanticRecall stays
	// empty). recallEmbedder may be set without recallService
	// (e.g. backfill only) or vice versa; the runtimePMAgent
	// checks both before issuing a Query.
	recallService      *recall.Service
	recallEmbedder     recall.Embedder
	// agentReputationRepo + alphaLessonRepo back the Sprint 9.1
	// alpha-aware memory context block fed into the PM prompt.
	// Both nil = feature off (DecisionInput.AgentTrackRecord
	// stays empty); the per-fund runtime forwards them straight
	// into the runtimePMAgent so buildAgentTrackRecord can
	// render the markdown block via alphalesson.BuildContext.
	agentReputationRepo *agentreputation.Repo
	alphaLessonRepo     *alphalesson.Repo
	// workflowCheckpointRepo backs the Sprint 9.2 per-step
	// snapshot persistence. The per-fund orchestrator's
	// CheckpointStore is built from this repo via
	// newWorkflowCheckpointSink. nil = checkpoint persistence
	// disabled (orchestrator falls back to in-process state
	// only); the resume / admin-UI endpoints then return empty
	// lists silently.
	workflowCheckpointRepo *repository.WorkflowCheckpointRepo
	// planLifecycleNotifier is the Sprint 4 / android-core push
	// fan-out hook. Optional — nil = no push notifications.
	planLifecycleNotifier workflow.PlanLifecycleNotifier

	// S12-followup (2026-06-04): four of broker.Simulator's
	// five pre-trade gates mirrored onto the PM-direct-fill
	// path. The fifth (LotSizeGate) is intentionally handled
	// by the faster in-memory pmPathLotSizeGuard. The adapter
	// stores the IMPLEMENTATIONS so each per-fund
	// runtimeTradingEngine constructed by newRuntime sees the
	// same singletons the broker.Simulator was wired with —
	// any holiday calendar update, lock-up flip, or borrow
	// inventory change propagates to both paths at once.
	// All four fields are optional (nil = no-op allow) so
	// legacy / smoke wiring still works.
	marketStatusGate broker.MarketStatusGate
	lockupGate       broker.LockupGate
	borrowGate       broker.BorrowGate
	priceCollarGate  broker.PriceCollarGate

	mu                 sync.Mutex
	runtimes           map[string]*workflowRuntime
	scheduler          *fundWorkflowScheduler
}

// WithQuotaService wires the F28 quota gate. Calling with nil is a
// no-op (quota enforcement disabled); calling twice replaces the
// previous service.
func (s *workflowServiceAdapter) WithQuotaService(q *quota.Service) *workflowServiceAdapter {
	if s == nil {
		return s
	}
	s.quotaService = q
	return s
}

// WithLLMRuntime injects the LLM runtime so the per-fund runtime built in
// newRuntime can drive memory.Reflect at the tail of each daily review.
// Idempotent and safe to call before/after StartBackgroundScheduler.
func (s *workflowServiceAdapter) WithLLMRuntime(runtime *llmRuntime) *workflowServiceAdapter {
	if s != nil {
		s.runtime = runtime
	}
	return s
}

// WithPMPathGates injects the same broker-side pre-trade gate
// implementations the broker.Simulator was constructed with, so
// the PM-direct-fill path in runtimeTradingEngine.executePlanAction
// runs the same regulatory checks (market-status, lockup, borrow,
// price-collar) as orders that flow through broker.SubmitOrder.
// Passing nil for any individual gate is fine — the engine treats
// nil as "no-op allow", matching broker.Simulator's contract.
// Idempotent and safe to call before or after the workflow
// scheduler is started; each per-fund runtime is constructed
// lazily by newRuntime and reads the current values.
func (s *workflowServiceAdapter) WithPMPathGates(
	marketStatus broker.MarketStatusGate,
	lockup broker.LockupGate,
	borrowGate broker.BorrowGate,
	priceCollar broker.PriceCollarGate,
) *workflowServiceAdapter {
	if s == nil {
		return s
	}
	s.marketStatusGate = marketStatus
	s.lockupGate = lockup
	s.borrowGate = borrowGate
	s.priceCollarGate = priceCollar
	return s
}

// WithAttributionService wires the Phase 3A-5 attribution
// service used by the daily review hook. Nil disables the
// attribution pass; callers from main.go pass the same service
// instance that backs the HTTP endpoint so both surfaces see
// identical state.
func (s *workflowServiceAdapter) WithAttributionService(svc *attribution.Service) *workflowServiceAdapter {
	if s != nil {
		s.attribution = svc
	}
	return s
}

// WithSemanticRecall wires the Sprint 3 / L3 pgvector-backed semantic
// memory recall stack: a recall.Service (read side) + an Embedder
// (query side, used to encode the current daily context into the
// vector the service searches with). Passing nil to either leaves
// SemanticRecall absent from the prompt — same silent-degrade contract
// as IntradaySnapshots.
func (s *workflowServiceAdapter) WithSemanticRecall(svc *recall.Service, embedder recall.Embedder) *workflowServiceAdapter {
	if s == nil {
		return s
	}
	s.recallService = svc
	s.recallEmbedder = embedder
	return s
}

// WithWorkflowCheckpointRepo wires the Sprint 9.2 per-step
// checkpoint repository. Nil disables checkpoint persistence
// (orchestrator keeps using in-process state only). Idempotent.
func (s *workflowServiceAdapter) WithWorkflowCheckpointRepo(repo *repository.WorkflowCheckpointRepo) *workflowServiceAdapter {
	if s == nil {
		return s
	}
	s.workflowCheckpointRepo = repo
	return s
}

// WithAlphaAwareMemory wires the Sprint 9.1 alpha-aware-memory stack
// that produces the AgentTrackRecord block on the PM prompt:
//   - reputation: the per-fund agent leaderboard (avg α, hit_rate)
//     drawn from agent_reputation_stats.
//   - lessons: the most recent alpha-tagged memory rows the
//     reputation backfill mints when |α| crosses the threshold.
//
// Passing nil to either side leaves the block absent from the
// prompt — same silent-degrade contract as the rest of the
// optional context blocks.
func (s *workflowServiceAdapter) WithAlphaAwareMemory(reputation *agentreputation.Repo, lessons *alphalesson.Repo) *workflowServiceAdapter {
	if s == nil {
		return s
	}
	s.agentReputationRepo = reputation
	s.alphaLessonRepo = lessons
	return s
}

// WithPlanLifecycleNotifier wires the Sprint 4 / android-core push
// fan-out hook. Nil = no push notifications, orchestrator still
// works (workflow.notifyPlan no-ops with a nil notifier).
func (s *workflowServiceAdapter) WithPlanLifecycleNotifier(n workflow.PlanLifecycleNotifier) *workflowServiceAdapter {
	if s == nil {
		return s
	}
	s.planLifecycleNotifier = n
	return s
}

// WithOHLCFetcher wires the Phase 2C OHLC source used by
// runtimeResearcherPool to enrich quant signals and the debate
// Quant role's prompt. Nil disables OHLC overlay entirely. Idempotent.
func (s *workflowServiceAdapter) WithOHLCFetcher(fetcher ohlc.Fetcher) *workflowServiceAdapter {
	if s != nil {
		s.ohlcFetcher = fetcher
	}
	return s
}

// WithFundamentalFetcher wires the Phase 2D fundamentals source.
// Nil disables fundamentals overlay entirely. Idempotent.
func (s *workflowServiceAdapter) WithFundamentalFetcher(fetcher fundamental.Fetcher) *workflowServiceAdapter {
	if s != nil {
		s.fundamentalFetcher = fetcher
	}
	return s
}

// WithSectorFlowFetcher wires the Phase 2D sector-rotation source.
// Nil disables the rotation block in the macro brief. Idempotent.
func (s *workflowServiceAdapter) WithSectorFlowFetcher(fetcher sectorflow.Fetcher) *workflowServiceAdapter {
	if s != nil {
		s.sectorFlowFetcher = fetcher
	}
	return s
}

// WithSentimentScorer wires the Phase 2D news sentiment scorer.
// Nil disables sentiment scoring (workflow passes raw headlines).
// Idempotent.
func (s *workflowServiceAdapter) WithSentimentScorer(scorer sentiment.Scorer) *workflowServiceAdapter {
	if s != nil {
		s.sentimentScorer = scorer
	}
	return s
}

// WithSocialRegistry wires the Sprint 9.3 retail-social provider
// registry. Nil (or empty registry) disables the social ingestion
// path; the workflow then sees news-only sentiment items, which
// matches pre-9.3 behaviour.
//
// Idempotent so main.go can call it unconditionally and still let
// staging override with a different registry. Tests typically pass
// a Registry built from stub Providers via NewRegistry.
func (s *workflowServiceAdapter) WithSocialRegistry(reg *social.Registry) *workflowServiceAdapter {
	if s != nil {
		s.socialRegistry = reg
	}
	return s
}

// resolveSentimentScorer returns the configured scorer, falling
// back to an env-driven default that pairs the shared LLM runtime
// (when wired) with the keyword fallback. Callers receive nil only
// when SENTIMENT_DISABLED=1 — every other state surfaces at least
// the keyword scorer so the workflow always sees a directional
// signal.
func (s *workflowServiceAdapter) resolveSentimentScorer(fundID string) sentiment.Scorer {
	if s == nil {
		return nil
	}
	if s.sentimentScorer != nil {
		return s.sentimentScorer
	}
	// Production path goes through newRuntime → resolves operator
	// once and reuses for both scorer + roundtable; this fast path
	// only fires for the test-only / disabled-feature flows where
	// no fund context is available. Skipping the DB lookup when
	// adapter.db is nil keeps the unit-test build (which spins up
	// workflowServiceAdapter{} stand-alone) panic-free.
	var ownerUserID string
	if s.db != nil {
		ownerUserID, _ = resolveFundOperatorRouting(
			context.Background(),
			fundID,
			repository.NewTeamRepo(s.db),
			repository.NewAgentRepo(s.db),
		)
	}
	return buildSentimentScorerFromRuntime(s.runtime, fundID, ownerUserID)
}

type workflowRuntime struct {
	tradingDate  string
	orchestrator *workflow.DailyOrchestrator
}

type runtimeEventBus struct{}

type runtimeResearcherPool struct {
	fundRepo   *repository.FundRepo
	teamRepo   *repository.TeamRepo
	agentRepo  *repository.AgentRepo
	marketData *marketdata.Service
	memoryRepo *repository.MemoryRepo
	// debateRoundtable, when non-nil and the fund opts in via
	// fund.config.researchTier=="advanced" (or env flag), replaces
	// the legacy Roundtable text-concat implementation with the
	// multi-agent debate orchestrator from internal/debate. Phase 2B.
	debateRoundtable debate.Roundtable
	// debateForceEnabled flips the gate to "always on" regardless of
	// per-fund researchTier. Driven by FUND_DEBATE_ROUNDTABLE=1 so
	// staging/canary deployments can flip the switch fleet-wide.
	debateForceEnabled bool
	// ohlcFetcher is the Phase 2C entry point for historical bars
	// (typically a *ohlc.Cache wrapping a *ohlc.Registry with
	// Yahoo / Binance / Akshare providers). nil = no OHLC, in which
	// case the quant research path silently skips the indicator
	// block and falls back to the legacy qualitative signals.
	ohlcFetcher ohlc.Fetcher
	// fundamentalFetcher (Phase 2D) supplies PE/PB/margins/growth
	// per symbol. Wired into buildQuantResearchContent (per-name
	// fundamentals line) and runDebateRoundtable (DebateInput.
	// FundamentalReports) so the Bull/Bear roles argue over
	// concrete valuation facts instead of generic prose. Nil-safe.
	fundamentalFetcher fundamental.Fetcher
	// sectorFlowFetcher (Phase 2D) supplies sector rotation +
	// money-flow signals. Wired into buildMacroResearchContent
	// (macro brief gets a Top/Bottom sectors block) and the debate
	// MacroBrief input so the Bull/Bear can ground their tape
	// reading in actual rotation evidence. Nil-safe.
	sectorFlowFetcher sectorflow.Fetcher
	// sentimentScorer (Phase 2D) classifies recent news on a -1..+1
	// scale per symbol. Wired into the macro / quant research paths
	// and debate inputs so agents see "AAPL bullish (+0.42)" rather
	// than rummaging through raw headlines. Nil-safe: when unset
	// the pool falls back to passing raw headlines as before.
	sentimentScorer sentiment.Scorer
	// socialRegistry (Sprint 9.3) supplies retail social posts
	// (Xueqiu / StockTwits / Reddit-WSB) per symbol so the
	// sentiment scorer can see RETAIL MOOD on top of wire-news.
	// Nil-safe: when unset (or HasProviders is false) the pool
	// behaves exactly as before, news-only. When set, the macro
	// sentiment block and the per-symbol debate block both call
	// FetchPosts and concatenate the resulting items into the
	// scorer batch.
	socialRegistry *social.Registry
	// llmRuntime is the Sprint 1 / S3 hook for true LLM-driven
	// research summarisation. When non-nil, MacroBrief / RunAll /
	// QuantSignals run the raw provider-text content through a
	// TierStandard LLM call that emits a structured
	// {summary, bullets[], confidence} payload; the formatted
	// result is then prepended to the legacy provider text so
	// downstream consumers (debate, persistence) still see the
	// raw signals AND the LLM synthesis. Nil → legacy behaviour
	// (formatter only, no LLM enrichment); failures inside the
	// LLM path also degrade gracefully to the legacy text.
	llmRuntime *llmRuntime
}

type runtimePMAgent struct {
	// db is the raw DB handle, used by Sprint 3 / M1 lesson hit-rate
	// lookups that sit outside the typed repos. Nil-safe — lookup
	// silently returns 0/0 when missing.
	db *sql.DB

	planRepo     *repository.PlanRepo
	fundRepo     *repository.FundRepo
	positionRepo *repository.PositionRepo
	teamRepo     *repository.TeamRepo
	agentRepo    *repository.AgentRepo
	marketData   *marketdata.Service
	// tradeRepo is consulted by buildPlanActions to determine how many
	// shares of each held position were bought during the current
	// trading session. On T+1 markets those shares are locked from sale
	// today; the PM uses this signal to downgrade a "reduce" proposal
	// to "hold" (or cap its sell qty to what's actually sellable). nil
	// is permitted for tests that don't need the T+1 awareness — the
	// helper falls back to treating the full position as sellable,
	// matching the legacy behaviour the runtime gate will still catch.
	tradeRepo *repository.TradeRepo
	// decisionEngine drives the LLM-based decision pipeline introduced
	// in Phase 2A. When non-nil, buildPlanActions consults it first
	// and only falls back to the deterministic legacy heuristic if the
	// engine returns an error or an empty plan. When nil (legacy
	// deployments + most unit tests), buildPlanActions skips straight
	// to the deterministic path so behaviour is unchanged.
	decisionEngine decision.DecisionEngine
	// lotRepo + exitManager drive the Phase 3A-2 exit manager. When
	// both are non-nil AND the fund's exitPolicy is enabled, the
	// agent runs the deterministic stop-loss / take-profit /
	// trailing / time-stop rules BEFORE the LLM call. Any rule that
	// fires produces a sell PlanAction that overrides the LLM's
	// proposal on that instrument. Nil → no exit manager (legacy
	// behaviour preserved). Same nil-safe pattern as decisionEngine.
	lotRepo     *repository.LotRepo
	exitManager *exitmanager.Service
	// regimeService drives Phase 3A-3 regime tagging. When non-nil,
	// the agent classifies each plan action's instrument as
	// trend_up / trend_down / range / chop and stamps the result
	// onto plan_action.regime_tag. The tag propagates into
	// position_lots.regime_at_entry / closed_lots.regime_at_entry
	// through recordLotFill, giving the attribution agent the data
	// it needs to answer "which sleeve makes money in which
	// regime?". Nil → tag is left NULL (legacy behaviour).
	regimeService *regime.Service
	// ohlcFetcher is the shared bar source. Re-used by both the
	// regime classifier and the Phase 3A-4 strategy sleeves so a
	// single fetch per (symbol, day) supports both sub-systems.
	// Nil → strategy sleeves silently no-op (legacy behaviour).
	ohlcFetcher ohlc.Fetcher
	// memoryRepo gives the PMAgent read access to the
	// attribution lessons the daily-review hook writes to
	// memories (Layer="attribution"). loadMutedSleeveRegimes
	// uses it to fold those lessons into a (sleeve, regime)
	// mute set that strategy.Service consults before evaluating
	// proposals. Nil → no mute pass (legacy behaviour).
	memoryRepo *repository.MemoryRepo
	// attribution is the Phase 3A-7 read-side seam: the PM
	// builds a fresh AttributionReport per decision call and
	// folds the top winners / losers (sleeve × regime) into the
	// LLM's prompt as a "scorecard". The hard mute layer still
	// runs via memoryRepo + strategy.Service; the scorecard is
	// the *soft* feedback channel so the LLM can reason about
	// historical wins/losses on cells the mute didn't silence.
	// Nil → no scorecard injection (legacy behaviour).
	attribution *attribution.Service
	// intradayBuilder is the Sprint 3 / L1 intraday (5/15/60m) snapshot
	// builder. Same OHLC fetcher as quantSnapshot but different cadence
	// (5m default). Nil = feature off (intraday OHLC disabled / not
	// wired); the prompt then omits the block.
	intradayBuilder *intraday.Builder

	// recall + recallEmbedder is the Sprint 3 / L3 semantic memory
	// recall pair. Both nil = feature off (no pgvector / no embed
	// provider) and DecisionInput.SemanticRecall stays empty —
	// silent degrade. The PM uses recall.Service.Query() with an
	// embedding generated on-the-fly from today's MacroBriefing +
	// universe to fetch the top-k similar memories.
	recall         *recall.Service
	recallEmbedder recall.Embedder

	// contradiction is the Sprint 3 / L3 cross-agent contradiction
	// checker. Nil = feature off. When set, the PM runs it right
	// before serialising DecisionInput, appending any warning /
	// block notes into RiskNotes so the LLM PM gets them in-context.
	contradiction *contradictionRunner

	// quantSnapshot is the Sprint A #1 per-symbol regime + ATR
	// + position-size-ceiling builder. The PM calls BuildBatch
	// for the union of universe + held positions on every
	// decision pass and feeds the resulting Snapshots into
	// DecisionInput.QuantSnapshots so the LLM prompt can apply
	// the per-symbol size cap + regime-aware action rules. Nil
	// → no snapshots in the prompt (legacy behaviour); the
	// existing per-symbol RoundtableSymbolVerdict + sleeve
	// scorecard still drive sizing.
	quantSnapshot *quantsnapshot.Builder
	// ranker is the Sprint A #2 cross-sectional ranker. Shares
	// the same OHLC fetcher as quantSnapshot so the cache layer
	// makes the second pass effectively free. BuildRanking
	// returns nil on too-small universes (<3 surviving symbols)
	// — the prompt block is then omitted, preserving the
	// pre-Sprint-A behaviour for funds whose universe + history
	// can't support a meaningful z-score.
	ranker *ranking.Ranker
	// cooldownSvc is the Sprint B #1 event-driven re-entry lock.
	// Reads trade_executions for the fund's own recent fills and
	// surfaces symbols still inside the cooldown window (default
	// 24h after the last fill). nil = no cooldown wiring (legacy
	// behaviour); the prompt block is then omitted.
	cooldownSvc *cooldown.Service
	// riskBudgetSvc is the Sprint B #2 dynamic risk-budget
	// throttle. Reads nav_snapshots for the fund's NAV history
	// and emits a single snapshot describing realised vol vs
	// target, drawdown vs ceiling, and the resulting effective
	// per-trade R%. nil = no risk-budget wiring (legacy
	// behaviour); the prompt omits the block and the PM falls
	// back to its static R prior.
	riskBudgetSvc *riskbudget.Service
	// newsCatalystSvc is the Sprint B #3 per-symbol news
	// catalyst recall. Uses the same marketdata.Service as the
	// rest of the wiring (provider rotation + cache + translation
	// are shared) and parallel-fetches one news call per
	// candidate symbol behind a small worker pool. nil = no
	// catalyst recall (marketdata not enabled); the prompt omits
	// the block and the PM falls back on the existing
	// NewsSentiment text blob.
	newsCatalystSvc *newsrecall.Service
	// earningsSvc is the Sprint E #2 scheduled-earnings catalyst
	// service. Default deployment uses earnings.YahooProvider
	// (zero-config, keyless Yahoo Finance v10 quoteSummary)
	// via buildEarningsFetcherFromEnv(); env knobs
	// EARNINGS_DISABLED=1 / YAHOO_EARNINGS_DISABLED=1 fall back
	// to NoopFetcher so the prompt block is silently absent.
	// Operators can still plug in a hand-curated StaticFetcher
	// (or a future Finnhub / Polygon adapter) by editing the
	// builder. nil = feature off; the prompt omits the block.
	earningsSvc *earnings.Service
	// qualitySvc is the Sprint E #3 cross-sectional quality-factor
	// score builder. Reuses the cached fundamental.Fetcher the
	// wiring layer already shares with the FundamentalSummary
	// renderer so a single per-symbol Fetch covers both. nil =
	// feature off (fundamental data unwired); the prompt omits
	// the block.
	qualitySvc *quality.Service
	// valueSvc is the Sprint F #1 cross-sectional value-factor
	// (Fama-French HML lineage) score builder. Shares the same
	// fundamental.Fetcher cache as qualitySvc so quality + value
	// composites come back from a single per-symbol Fetch.
	// Together the two sleeves implement the AQR QMJ + HML
	// "Quality at a Reasonable Price" double-overlay the system
	// prompt teaches the PM to recognise. nil = feature off
	// (fundamental data unwired); the prompt omits the block.
	valueSvc *value.Service
	// lowBetaSvc is the Sprint F #2 Frazzini-Pedersen
	// Betting-Against-Beta defensive overlay. Shares the same
	// cached ohlc.Fetcher with quantSnapshot / ranker /
	// correlation / pairspread so its per-symbol bar fetches
	// hit the warm cache. nil = feature off (no OHLC fetcher
	// wired) — the prompt omits the block and the PM falls
	// back on quantSnapshots regime + riskBudget alone for
	// any defensive-tilt decision.
	lowBetaSvc *lowbeta.Service
	// peadSvc is the Sprint F #3 Post-Earnings Announcement
	// Drift overlay (Bernard-Thomas 1989). Depends on an
	// earnings.HistoryService + the shared ohlc.Fetcher.
	// Default wiring uses earnings.YahooHistoryProvider so US
	// names get historical earnings out-of-the-box; A-share /
	// HK funds where Yahoo coverage is poor fall back to the
	// NoopHistoryFetcher → snapshot stays nil → prompt block
	// omitted. nil peadSvc = feature off entirely.
	peadSvc *pead.Service
	// correlationSvc is the Sprint C #2 pairwise correlation
	// matrix. Shares the same OHLC fetcher as quantSnapshot and
	// ranker so the cache layer makes the third pass cheap.
	// Compute returns nil when fewer than 2 symbols have usable
	// OHLC data; the prompt omits the block in that case and
	// the PM falls back on per-symbol R sizing alone.
	correlationSvc *correlation.Service
	// pairSpreadSvc is the Sprint E #4 rolling spread monitor.
	// Consumes the HighCorrPairs from the correlation snapshot
	// (so the matrix runs first, the spread monitor second)
	// and computes log(left/right) z-scores over the same
	// lookback window. Reuses the OHLC cache so the second
	// pass is nearly free. nil = feature off (no OHLC fetcher
	// wired or no high-correlation pairs to monitor); the
	// prompt omits the block.
	pairSpreadSvc *pairspread.Service
	// serverMetrics powers Sprint D #1 (Prometheus counters for
	// PM decision-input observability). Optional — tests that
	// build a runtimePMAgent directly leave this nil and the
	// metrics calls become no-ops via the ObserveDecisionInput
	// receiver-nil guard. Wired by newRuntime when the global
	// metrics registry is available.
	serverMetrics *serverMetrics

	// agentReputationRepo + alphaLessonRepo back the Sprint 9.1
	// alpha-aware memory context block. The PM reads the per-fund
	// agent leaderboard (avg α, hit_rate) from the reputation repo
	// and the most recent alpha-tagged lessons from the lesson
	// repo, renders them into a single markdown block via
	// alphalesson.BuildContext, and stuffs it into
	// DecisionInput.AgentTrackRecord. Both are nil-safe — when
	// either is unset, buildAgentTrackRecord returns "" and the
	// prompt simply omits the section. Wired by newRuntime once
	// the agentreputation + alphalesson services are available.
	agentReputationRepo *agentreputation.Repo
	alphaLessonRepo     *alphalesson.Repo
	// lastTraceByFund is the G1 #2 attribution bridge. The
	// buildDecisionInput → GeneratePlan path produces the
	// trace at the START of a plan tick (when we know what
	// signals were available) but persists the plan at the
	// END (when we know the reasoning text). Threading the
	// trace through 3 layers of return values would touch
	// every test that calls buildPlanActions; this small
	// per-fund cache is the surgical alternative.
	//
	// Contract:
	//   - buildDecisionInput STORES the trace keyed by fundID
	//     at the end of the function.
	//   - GeneratePlan LOADS-AND-DELETES the trace right after
	//     CreateWithActions succeeds, then writes the
	//     attribution payload via SetBlockContributions.
	//   - Stale entries never accumulate because each store
	//     overwrites the previous one for the same fundID;
	//     the per-fund PM tick cadence (typically once per
	//     trading day) keeps the map small (≤ N_funds rows).
	//
	// Concurrency: PM ticks per fund are sequential by design
	// (the workflow runner serialises them); sync.Map handles
	// the cross-fund case where two different funds tick
	// simultaneously.
	lastTraceByFund sync.Map

	// decisionSourceObserver (Sprint 11.4) is the optional
	// metrics sink for PM-decision provenance events. nil-safe;
	// production wires *serverMetrics here, tests leave it nil
	// and the recorder is a no-op.
	decisionSourceObserver pmDecisionSourceObserver

	// lastDecisionSourceByFund is the Sprint 11.2 sibling of
	// lastTraceByFund. It carries the decision-provenance tag
	// (llm_pm / llm_three_stage / fallback_after_llm_error / …)
	// plus an optional errorclass.Detail blob from the moment
	// buildPlanActions decides which path it took to the moment
	// GeneratePlan has the plan ID and can persist via
	// PlanRepo.SetDecisionSource. Same load-and-delete contract
	// as lastTraceByFund — see decisionSourceRecord. Keeping
	// this as a separate map (rather than fattening
	// lastTraceByFund) preserves a single-purpose cache per
	// concern and avoids forcing the G1 #2 attribution writer to
	// know about S11 fields.
	lastDecisionSourceByFund sync.Map
}

// decisionSourceRecord is the per-fund payload stashed by
// buildPlanActions for GeneratePlan to consume. Source is one of the
// decision_source enum values; ReasonJSON is the marshalled
// errorclass.Detail JSONB and is nil for successful LLM rows.
type decisionSourceRecord struct {
	Source     string
	ReasonJSON []byte
}

// pmDecisionSourceObserver is the metrics-sink contract for Sprint
// 11.4. *serverMetrics satisfies this interface; tests can stub it
// out cheaply. The signature deliberately mirrors errorclass.Detail
// field semantics — category + provider are empty on the LLM-success
// path, populated on fallback rows.
type pmDecisionSourceObserver interface {
	ObservePMDecisionSource(source, category, provider string)
}

type runtimeRiskAgent struct {
	planRepo     *repository.PlanRepo
	fundRepo     *repository.FundRepo
	positionRepo *repository.PositionRepo
	teamRepo     *repository.TeamRepo
	agentRepo    *repository.AgentRepo
}

type runtimeApprovalGateway struct {
	planRepo  *repository.PlanRepo
	fundRepo  *repository.FundRepo
	tradeRepo *repository.TradeRepo
	isCurrent func() bool
	// now lets tests freeze "today" for the daily-cumulative gate. nil
	// falls back to time.Now (UTC). Always called for the trading-date
	// boundary; we deliberately don't take the market calendar into
	// account here — for guardrails, calendar drift is safer than
	// allowing a second "fresh day" within the same UTC midnight.
	now func() time.Time
}

type runtimeTradingEngine struct {
	planRepo     *repository.PlanRepo
	fundRepo     *repository.FundRepo
	tradeRepo    *repository.TradeRepo
	positionRepo *repository.PositionRepo
	navRepo      *repository.NavSnapshotRepo
	teamRepo     *repository.TeamRepo
	agentRepo    *repository.AgentRepo
	marketData   *marketdata.Service
	metrics      *serverMetrics

	// lotLedger + uow drive the Phase 3A-1 FIFO lot ledger
	// (position_lots / closed_lots). After each successful equity
	// buy/sell fill the engine builds a lotledger.FillEvent and
	// calls RecordWithUoW so the attribution layer has structured
	// roundtrips to query.
	//
	// Both are optional from the struct's perspective: when nil
	// (legacy wiring, unit tests) the engine simply skips the
	// shadow ledger and the trade flow stays identical.
	lotRepo   *repository.LotRepo
	lotLedger *lotledger.Service
	uow       repository.UnitOfWork

	// cashLedger captures every cash movement at fill granularity
	// (P1-1). Optional: when nil the engine writes nothing to the
	// journal, which is the legacy behaviour and is fine for
	// tests that don't care about reconciliation.
	cashLedger *repository.CashLedgerRepo

	// S12-followup (2026-06-04): broker-side pre-trade gates
	// mirrored to the PM-direct-fill path. broker.Simulator
	// already runs these five gates inside SubmitOrder, but
	// runtimeTradingEngine.tradeRepoCreateAndFill bypasses the
	// simulator and writes trade_executions directly. Holding
	// references to the same gate IMPLEMENTATIONS here means
	// PM-path fills go through the SAME regulatory checks as
	// broker-path orders — no behaviour drift between the two
	// code paths.
	//
	// All five fields are optional: a nil gate is treated as
	// "no-op allow" so legacy tests / single-binary smoke builds
	// that don't wire the gates keep working. The production
	// wiring in main.go always sets all five.
	//
	// Lot-size is intentionally NOT covered by broker.LotSizeGate
	// here — pmPathLotSizeGuard runs a faster in-memory check
	// using the positionsByKey snapshot the engine already
	// loaded, avoiding the redundant DB lookup that the broker-
	// side gate performs. The two implementations enforce the
	// exact same A-share board rules; the regression tests in
	// pmpath_lotsize_guard_test.go pin that parity.
	marketStatusGate broker.MarketStatusGate
	lockupGate       broker.LockupGate
	borrowGate       broker.BorrowGate
	priceCollarGate  broker.PriceCollarGate
}

type hardRiskState struct {
	TradingDate time.Time
	TotalAssets float64
	DailyReturn float64
	TradesToday []risk.ExecutedTrade
	Policy      risk.Policy
}

type runtimeMemorySystem struct {
	// db is the raw DB handle — required by Sprint 3 / M1 lesson
	// lineage writes (which sit outside any of the typed repo
	// surfaces below). Optional in tests; nil ⇒ lineage write is
	// skipped silently.
	db *sql.DB

	fundRepo     *repository.FundRepo
	agentRepo    *repository.AgentRepo
	teamRepo     *repository.TeamRepo
	planRepo     *repository.PlanRepo
	tradeRepo    *repository.TradeRepo
	positionRepo *repository.PositionRepo
	navRepo      *repository.NavSnapshotRepo
	workflowRepo *repository.WorkflowRunRepo
	memoryRepo   *repository.MemoryRepo

	// llmRuntime is optional. When set, ConsolidateDaily drives
	// memory.Reflect() on a weekly cadence to graduate daily learnings into
	// long-term reflections (Layer="long_term"). Without it, daily review
	// still runs but no reflections are produced — this keeps unit tests
	// and dev environments cheap.
	llmRuntime *llmRuntime

	// attribution is the Phase 3A-5 cross-tab + lesson generator.
	// Optional: nil means daily review skips the attribution pass
	// silently — the existing reflection / learning paths still
	// run. Wired by main.go from newAttributionServiceAdapter.
	attribution *attribution.Service
}

type learningContext struct {
	fund        *repository.Fund
	tradingDate time.Time
	workflowRun *repository.WorkflowRun
	nav         *repository.NavSnapshot
	plan        *repository.InvestmentPlan
	actions     []repository.PlanAction
	trades      []repository.TradeExecution
	positions   []repository.HoldingPosition

	// fundDayTradeCount is the fund-wide trade count for the trading
	// day, regardless of which plan a trade was attributed to. This
	// matters for the LLM-lesson gate (Step D): on funds with 30-min
	// intraday cadence the LAST plan of the day is often a "watch
	// only" tick that has zero plan-scoped trades, even though earlier
	// plans the same day produced real fills. Without this fallback
	// the gate would mark every day as "no signal" once the last tick
	// rolls in. attribution/templates still consume `trades` (plan
	// scoped) so per-plan attribution is unaffected.
	fundDayTradeCount int
}

type learningResult struct {
	Title          string
	Summary        string
	Hits           []string
	Misses         []string
	Lessons        []string
	Adjustments    []string
	Tags           []string
	Specialization *specializationLearningSummary
}

type specializationLearningSummary struct {
	Markets         map[string]float64 `json:"markets,omitempty"`
	AssetClasses    map[string]float64 `json:"assetClasses,omitempty"`
	Themes          map[string]float64 `json:"themes,omitempty"`
	Instruments     map[string]float64 `json:"instruments,omitempty"`
	StyleHints      map[string]float64 `json:"styleHints,omitempty"`
	RecentLessons   []string           `json:"recentLessons,omitempty"`
	LastAdjustments []string           `json:"lastAdjustments,omitempty"`
}

type agentSpecialization struct {
	Markets      []string `json:"markets,omitempty"`
	AssetClasses []string `json:"assetClasses,omitempty"`
	Themes       []string `json:"themes,omitempty"`
	Instruments  []string `json:"instruments,omitempty"`
	StyleHints   []string `json:"styleHints,omitempty"`
	Patterns     []string `json:"patterns,omitempty"`
}

type skillScenario struct {
	AgentRole    string
	AgentFocus   string
	WorkflowStep string
	FundID       string
	TradingDate  string
	Keywords     []string
}

type resolvedSkill struct {
	Key         string
	Name        string
	Description string
	Content     string
	Priority    int
}

type parsedSkillConfig struct {
	Enabled bool               `json:"enabled"`
	Skills  []parsedSkillEntry `json:"skills"`
}

type parsedSkillEntry struct {
	Key         string           `json:"key"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Content     string           `json:"content"`
	Enabled     *bool            `json:"enabled,omitempty"`
	Priority    int              `json:"priority,omitempty"`
	Match       parsedSkillMatch `json:"match"`

	// F4 fields. Backwards-compatible: missing values keep the legacy
	// behaviour (manually-authored, immediately matchable skill).
	//
	// Status is "" / "approved" / "proposed":
	//   - "" or "approved" → skill is live, subject to Enabled flag.
	//   - "proposed"       → skill was auto-generated from a reflection and
	//     awaits user approval. The resolver treats it as disabled
	//     regardless of the Enabled field, so a buggy ProposedAt write
	//     cannot leak an unapproved lesson into agent prompts.
	//
	// Source records provenance:
	//   - "manual" / "" → user-authored via the admin UI.
	//   - "reflection:<reflection-id>" → produced by the F3 reflection
	//     engine; the suffix lets the UI deep-link back to the originating
	//     long-term reflection row.
	Status     string `json:"status,omitempty"`
	Source     string `json:"source,omitempty"`
	ProposedAt string `json:"proposedAt,omitempty"`
	ApprovedAt string `json:"approvedAt,omitempty"`
}

const (
	skillStatusApproved = "approved"
	skillStatusProposed = "proposed"
)

// skillEntryIsActive reports whether the skill should currently participate
// in prompt matching. Proposed skills are always inactive even if Enabled
// is nil or true — the approval workflow is the single gate.
func skillEntryIsActive(skill parsedSkillEntry) bool {
	if strings.EqualFold(skill.Status, skillStatusProposed) {
		return false
	}
	return skillEntryEnabled(skill)
}

type parsedSkillMatch struct {
	Roles            []string `json:"roles"`
	Focuses          []string `json:"focuses"`
	WorkflowSteps    []string `json:"workflowSteps"`
	ScenarioKeywords []string `json:"scenarioKeywords"`
}

type evolutionLearningConfig struct {
	DailyLearningEnabled       *bool                          `json:"dailyLearningEnabled,omitempty"`
	AutoApplyAdjustments       *bool                          `json:"autoApplyAdjustments,omitempty"`
	LookbackDays               int                            `json:"lookbackDays,omitempty"`
	MaxLessonsPerDay           int                            `json:"maxLessonsPerDay,omitempty"`
	FocusMetrics               []string                       `json:"focusMetrics,omitempty"`
	Guardrails                 []string                       `json:"guardrails,omitempty"`
	RecentLessons              []string                       `json:"recentLessons,omitempty"`
	LastLearningSummary        string                         `json:"lastLearningSummary,omitempty"`
	LastLearningDate           string                         `json:"lastLearningDate,omitempty"`
	LastLearningTags           []string                       `json:"lastLearningTags,omitempty"`
	LastRecommendedAdjustments []string                       `json:"lastRecommendedAdjustments,omitempty"`
	LastDailyReturn            *float64                       `json:"lastDailyReturn,omitempty"`
	LearningUpdatedAt          string                         `json:"learningUpdatedAt,omitempty"`
	SpecializationLearning     *specializationLearningSummary `json:"specializationLearning,omitempty"`
}

type teamServiceAdapter struct {
	db                  *sql.DB
	fundRepo            *repository.FundRepo
	companyRepo         *repository.FundCompanyRepo
	agentRepo           *repository.AgentRepo
	teamRepo            *repository.TeamRepo
	memoryRepo          *repository.MemoryRepo
	lineageRepo         *repository.LineageRepo
	usageTracker        *subscription.UsageTracker
	auditLogger         audit.Logger
	modelConfigs        *subscription.ModelConfigService
	subscriptionService *subscription.SubscriptionService
	llmRuntime          *llmRuntime
	// activityBus is the process-local fan-out for workflow events. Used by
	// ListTeamActivity / SubscribeTeamActivity to power the Team Live Activity
	// timeline. Shared with workflowServiceAdapter so the events emitted by
	// the orchestrator are the same ones the UI observes. May be nil in test
	// fixtures, in which case the methods return empty results / ErrNotFound.
	activityBus *workflow.ActivityBus
}

type memoryServiceAdapter struct {
	fundRepo    *repository.FundRepo
	companyRepo *repository.FundCompanyRepo
	memoryRepo  *repository.MemoryRepo
	auditLogger audit.Logger
}

type decisionTraceServiceAdapter struct {
	fundRepo     *repository.FundRepo
	companyRepo  *repository.FundCompanyRepo
	planRepo     *repository.PlanRepo
	tradeRepo    *repository.TradeRepo
	workflowRepo *repository.WorkflowRunRepo
	memoryRepo   *repository.MemoryRepo
	marketData   *marketdata.Service
	llmRuntime   *llmRuntime
}

type marketServiceAdapter struct {
	fundRepo    *repository.FundRepo
	companyRepo *repository.FundCompanyRepo
	teamRepo    *repository.TeamRepo
	agentRepo   *repository.AgentRepo
	marketData  *marketdata.Service
	llmRuntime  *llmRuntime
}

type walletServiceAdapter struct {
	walletRepo *repository.WalletRepo
}

type marketplaceServiceAdapter struct {
	fundRepo            *repository.FundRepo
	companyRepo         *repository.FundCompanyRepo
	agentRepo           *repository.AgentRepo
	teamRepo            *repository.TeamRepo
	memoryRepo          *repository.MemoryRepo
	walletRepo          *repository.WalletRepo
	marketplaceRepo     *repository.MarketplaceRepo
	lineageRepo         *repository.LineageRepo
	uow                 repository.UnitOfWork
	modelConfigs        *subscription.ModelConfigService
	subscriptionService *subscription.SubscriptionService
	llmRuntime          *llmRuntime
}

func newFundServiceAdapter(db *sql.DB, workflowService *workflowServiceAdapter) *fundServiceAdapter {
	return &fundServiceAdapter{
		db:              db,
		companyRepo:     repository.NewFundCompanyRepo(db),
		fundRepo:        repository.NewFundRepo(db),
		workflowService: workflowService,
	}
}

func newPlanServiceAdapter(db *sql.DB, workflowService *workflowServiceAdapter, runtime *llmRuntime) *planServiceAdapter {
	return &planServiceAdapter{
		planRepo:        repository.NewPlanRepo(db),
		fundRepo:        repository.NewFundRepo(db),
		companyRepo:     repository.NewFundCompanyRepo(db),
		workflowService: workflowService,
		llmRuntime:      runtime,
	}
}

func newTradeServiceAdapter(db *sql.DB) *tradeServiceAdapter {
	return &tradeServiceAdapter{
		fundRepo:     repository.NewFundRepo(db),
		companyRepo:  repository.NewFundCompanyRepo(db),
		auditLogger:  audit.NewDBLogger(db),
		tradeRepo:    repository.NewTradeRepo(db),
		positionRepo: repository.NewPositionRepo(db),
		navRepo:      repository.NewNavSnapshotRepo(db),
		lotRepo:      repository.NewLotRepo(db),
	}
}

// WithMarketData wires the live-quote provider used by the GetPortfolio
// overlay to refresh CurrentPrice / MarketValue / UnrealizedPnL on each
// API hit. nil is treated as "disabled" and the adapter keeps serving the
// DB-cached price (legacy behaviour). Safe to call before or after the
// adapter is registered with the server; the read is unsynchronized
// because in practice WithMarketData is invoked exactly once during
// bootstrap, before any goroutine reads the field.
func (s *tradeServiceAdapter) WithMarketData(md *marketdata.Service) *tradeServiceAdapter {
	if s == nil {
		return nil
	}
	s.marketData = md
	return s
}

func newWorkflowServiceAdapter(db *sql.DB, subscriptionService *subscription.SubscriptionService, metrics *serverMetrics, marketData *marketdata.Service) *workflowServiceAdapter {
	// Activity persister: optional DB-backed sidecar so the Team Live
	// Activity timeline survives container restarts. nil db falls back
	// to the original in-memory-only behaviour (used by tests).
	var activityPersister workflow.ActivityPersister
	var activityRepo *repository.WorkflowActivityRepo
	if db != nil {
		activityRepo = repository.NewWorkflowActivityRepo(db)
		activityPersister = newActivityPersisterAdapter(activityRepo)
	}
	// Default ring buffer = 200 events/fund, subscriber channel = 64 deep.
	// These cover ~one full workflow run (≈40-50 events) plus headroom
	// without holding more than ~1 MB per fund in the worst case. The
	// persister extends durability beyond the ring (1..10 day retention)
	// without slowing down the SSE hot path.
	activityBus := workflow.NewActivityBus(200, 64)
	if activityPersister != nil {
		activityBus = activityBus.WithPersister(activityPersister, 4096)
	}
	ohlcFetcher := buildOHLCFetcherFromEnv()
	adapter := &workflowServiceAdapter{
		db:                  db,
		fundRepo:            repository.NewFundRepo(db),
		companyRepo:         repository.NewFundCompanyRepo(db),
		workflowRepo:        repository.NewWorkflowRunRepo(db),
		planRepo:            repository.NewPlanRepo(db),
		activityRepo:        activityRepo,
		subscriptionService: subscriptionService,
		marketData:          marketData,
		calendar:            marketcalendar.NewService(),
		metrics:             metrics,
		activityBus:         activityBus,
		runtimes:            make(map[string]*workflowRuntime),
		ohlcFetcher:         ohlcFetcher,
		fundamentalFetcher:  buildFundamentalFetcherFromEnv(),
		sectorFlowFetcher:   buildSectorFlowFetcherFromEnv(ohlcFetcher),
		// sentimentScorer is wired separately via WithSentimentScorer
		// because it depends on the LLM runtime (only available after
		// the LLM provider chain is constructed). Kept nil here.
	}
	adapter.scheduler = newFundWorkflowScheduler(adapter)
	return adapter
}

// buildOHLCFetcherFromEnv assembles the default OHLC chain from
// environment variables. Returns nil when nothing is configured —
// the runtime then transparently degrades to legacy qualitative
// quant signals. Env knobs:
//
//	OHLC_DISABLED=1          Force-disable OHLC entirely.
//	YAHOO_OHLC_DISABLED=1    Skip the Yahoo provider (US/HK).
//	BINANCE_OHLC_DISABLED=1  Skip the Binance provider (crypto).
//	BINANCE_OHLC_URL=...     Override Binance API root (private host).
//	AKSHARE_OHLC_URL=...     Akshare-MCP base URL (A-shares / futures).
//	OHLC_CACHE_TTL=...       TTL string (Go duration); default 15m.
//
// The default chain (Yahoo first, Binance for crypto, Akshare when
// URL is set) is intentional: Yahoo is global and key-less, Binance
// is crypto-only, Akshare requires a self-hosted MCP container so
// only kicks in when the operator explicitly wired it.
func buildOHLCFetcherFromEnv() ohlc.Fetcher {
	if envBool("OHLC_DISABLED") {
		return nil
	}
	reg := ohlc.NewRegistry()
	// Registration order matters: Registry.Fetch walks providers
	// in order and only falls through on ErrNoData. Akshare goes
	// FIRST when configured so A-share stock data routes through
	// the dedicated MCP (better A-share stock coverage than
	// Yahoo); EastMoney goes second to handle A-share INDICES
	// reliably (Yahoo only has 1d/5d depth for csi500/chinext/
	// star50 — see internal/ohlc/provider_eastmoney.go for the
	// detailed rationale). Yahoo handles US/HK + the rest of the
	// global coverage as a fallback. Operators can disable the
	// East Money lane via EASTMONEY_OHLC_DISABLED=1 (e.g., when
	// running in environments where the upstream is unreachable).
	if akURL := strings.TrimSpace(os.Getenv("AKSHARE_OHLC_URL")); akURL != "" {
		reg.Register(&ohlc.AkshareProvider{BaseURL: akURL})
	}
	if !envBool("EASTMONEY_OHLC_DISABLED") {
		reg.Register(&ohlc.EastmoneyProvider{BaseURL: strings.TrimSpace(os.Getenv("EASTMONEY_OHLC_URL"))})
	}
	if !envBool("YAHOO_OHLC_DISABLED") {
		reg.Register(&ohlc.YahooProvider{})
	}
	if !envBool("BINANCE_OHLC_DISABLED") {
		reg.Register(&ohlc.BinanceProvider{BaseURL: strings.TrimSpace(os.Getenv("BINANCE_OHLC_URL"))})
	}
	ttl := 15 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("OHLC_CACHE_TTL")); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			ttl = parsed
		}
	}
	return ohlc.NewCache(reg, ttl)
}

// buildFundamentalFetcherFromEnv assembles the Phase 2D fundamentals
// chain. Same nil-degradation contract as buildOHLCFetcherFromEnv.
// Env knobs:
//
//	FUNDAMENTAL_DISABLED=1         Force-disable entirely.
//	YAHOO_FUNDAMENTAL_DISABLED=1   Skip Yahoo (US/HK).
//	AKSHARE_FUNDAMENTAL_URL=...    Akshare-MCP base URL (A-shares).
//	FUNDAMENTAL_CACHE_TTL=...      Go duration; default 24h.
//
// Fundamentals are usually quarterly upstream, so the default TTL
// is much longer than ohlc's (15m). Operators can override if
// they're piping in an intraday source.
func buildFundamentalFetcherFromEnv() fundamental.Fetcher {
	if envBool("FUNDAMENTAL_DISABLED") {
		return nil
	}
	reg := fundamental.NewRegistry()
	if !envBool("YAHOO_FUNDAMENTAL_DISABLED") {
		reg.Register(&fundamental.YahooProvider{})
	}
	if akURL := strings.TrimSpace(os.Getenv("AKSHARE_FUNDAMENTAL_URL")); akURL != "" {
		reg.Register(&fundamental.AkshareProvider{BaseURL: akURL})
	}
	ttl := 24 * time.Hour
	if raw := strings.TrimSpace(os.Getenv("FUNDAMENTAL_CACHE_TTL")); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			ttl = parsed
		}
	}
	return fundamental.NewCache(reg, ttl)
}

// buildEarningsFetcherFromEnv assembles the Sprint E #2 earnings
// catalyst provider. Default = Yahoo Finance's keyless v10
// quoteSummary endpoint (zero-config, US-focused). Operators
// can disable for funds that don't trade catalysts (A-share-only
// portfolios where Yahoo coverage is poor) or swap to a future
// Finnhub / Polygon adapter without touching this signature.
//
// Env knobs:
//
//	EARNINGS_DISABLED=1            Force-disable entirely (NoopFetcher).
//	YAHOO_EARNINGS_DISABLED=1      Skip the Yahoo provider.
//	YAHOO_EARNINGS_BASE_URL=...    Override the Yahoo base URL
//	                               (default https://query2.finance.yahoo.com).
//	                               Used by acceptance tests pointing at
//	                               a stub server.
//	YAHOO_EARNINGS_CONCURRENCY=N   Per-fetch worker count (default 3).
//
// Returns earnings.NoopFetcher when nothing useful is wired so
// the runtime's `earnings.NewService(...)` call stays infallible
// (and the prompt block is silently absent).
//
// G1 #1: the returned provider is ALWAYS wrapped in
// earnings.Cache so the upstream Yahoo call rate is capped at
// roughly N_funds × N_universe per TTL window (default 6h),
// regardless of how many PM ticks fire inside that window. The
// TTL is operator-tuneable via YAHOO_EARNINGS_TTL.
func buildEarningsFetcherFromEnv() earnings.Fetcher {
	if envBool("EARNINGS_DISABLED") {
		return earnings.NoopFetcher{}
	}
	if envBool("YAHOO_EARNINGS_DISABLED") {
		// No other providers wired yet → degrade to noop.
		// When Finnhub / Polygon land, they slot in here.
		return earnings.NoopFetcher{}
	}
	provider := &earnings.YahooProvider{
		BaseURL: strings.TrimSpace(os.Getenv("YAHOO_EARNINGS_BASE_URL")),
	}
	if raw := strings.TrimSpace(os.Getenv("YAHOO_EARNINGS_CONCURRENCY")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 20 {
			provider.Concurrency = n
		}
	}
	return earnings.NewCache(provider, earnings.CacheOptions{
		TTL: parseDurationEnv("YAHOO_EARNINGS_TTL", 0),
	})
}

// buildEarningsHistoryFetcherFromEnv assembles the Sprint F #3
// historical earnings provider used by the PEAD overlay. Default
// = Yahoo Finance's keyless v10 earningsHistory module (zero-
// config, US-focused). Operators can disable per env knob.
//
// Env knobs (mirror buildEarningsFetcherFromEnv):
//
//	EARNINGS_DISABLED=1                 Force-disable entirely (NoopHistoryFetcher).
//	YAHOO_EARNINGS_HISTORY_DISABLED=1   Skip the Yahoo history provider.
//	YAHOO_EARNINGS_BASE_URL=...         Shared base URL with the forward
//	                                    calendar — flipping this for tests
//	                                    moves both providers to the same
//	                                    stub server.
//	YAHOO_EARNINGS_CONCURRENCY=N        Shared concurrency (default 3).
//
// Returns earnings.NoopHistoryFetcher when nothing is wired so
// the earnings.NewHistoryService(...) call stays infallible and
// the PEAD snapshot silently disappears from the prompt.
//
// G1 #1: the returned provider is ALWAYS wrapped in
// earnings.HistoryCache. The default 24h TTL is appropriate
// because epsActual/epsEstimate is fixed once the print lands;
// the only freshness concern is "did a new quarter just print
// inside the last 24h" which the longer TTL accepts (worst
// case: 24h lag on a brand-new print, which is still well
// inside the 60d PEAD window).
func buildEarningsHistoryFetcherFromEnv() earnings.HistoryFetcher {
	if envBool("EARNINGS_DISABLED") {
		return earnings.NoopHistoryFetcher{}
	}
	if envBool("YAHOO_EARNINGS_HISTORY_DISABLED") {
		return earnings.NoopHistoryFetcher{}
	}
	provider := &earnings.YahooHistoryProvider{
		BaseURL: strings.TrimSpace(os.Getenv("YAHOO_EARNINGS_BASE_URL")),
	}
	if raw := strings.TrimSpace(os.Getenv("YAHOO_EARNINGS_CONCURRENCY")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 20 {
			provider.Concurrency = n
		}
	}
	return earnings.NewHistoryCache(provider, earnings.CacheOptions{
		TTL: parseDurationEnv("YAHOO_EARNINGS_HISTORY_TTL", 0),
	})
}

// buildSectorFlowFetcherFromEnv assembles the Phase 2D sector-flow
// chain. Same nil-degradation contract. Env knobs:
//
//	SECTORFLOW_DISABLED=1          Force-disable entirely.
//	YAHOO_SECTORFLOW_DISABLED=1    Skip Yahoo sector ETF derived flow.
//	AKSHARE_SECTORFLOW_URL=...     Akshare-MCP base URL.
//	SECTORFLOW_CACHE_TTL=...       Go duration; default 5m
//	                               (rotation drifts intraday).
//
// The Yahoo provider requires the ohlc fetcher so we accept it as
// a dependency rather than building a duplicate chain.
func buildSectorFlowFetcherFromEnv(ohlcFetcher ohlc.Fetcher) sectorflow.Fetcher {
	if envBool("SECTORFLOW_DISABLED") {
		return nil
	}
	reg := sectorflow.NewRegistry()
	if !envBool("YAHOO_SECTORFLOW_DISABLED") && ohlcFetcher != nil {
		reg.Register(&sectorflow.YahooSectorProvider{OHLC: ohlcFetcher})
	}
	if akURL := strings.TrimSpace(os.Getenv("AKSHARE_SECTORFLOW_URL")); akURL != "" {
		reg.Register(&sectorflow.AkshareSectorProvider{BaseURL: akURL})
	}
	ttl := 5 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("SECTORFLOW_CACHE_TTL")); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			ttl = parsed
		}
	}
	return sectorflow.NewCache(reg, ttl)
}

// buildSentimentScorerFromRuntime wires the Phase 2D sentiment
// scorer. Pass the LLM runtime so the LLMScorer can hit the
// configured router; falls back to a pure KeywordScorer when the
// runtime / LLM client is unwired so deployments without LLM
// access still get a directional signal.
//
// Env knobs:
//
//	SENTIMENT_DISABLED=1     Force-disable entirely.
//	SENTIMENT_LLM_DISABLED=1 Skip LLM, use keyword fallback only.
//
// The keyword scorer is always wired as the safety-net fallback
// behind the LLM scorer, so an LLM outage doesn't blank the
// sentiment signal.
func buildSentimentScorerFromRuntime(runtime *llmRuntime, fundID string, ownerUserID string) sentiment.Scorer {
	if envBool("SENTIMENT_DISABLED") {
		return nil
	}
	keyword := &sentiment.KeywordScorer{}
	if envBool("SENTIMENT_LLM_DISABLED") {
		return keyword
	}
	if runtime == nil || runtime.client == nil {
		return keyword
	}
	// AgentID is a stable sentinel ("sentiment-scorer") that never
	// matches any row in the agents table, so the router skips its
	// agentDefaults bucket and falls through to userDefaults. As long
	// as ownerUserID is populated, sentiment will use the operator's
	// tier-specific preference (user_model_configs.tier="simple") when
	// one is configured, and only fall back to the platform .env
	// default when it isn't — same behaviour the PM gets from its
	// per-agent route, just keyed on (user, simple-tier) instead of
	// (user, specific-agent).
	llmScorer := &sentiment.LLMScorer{
		Client:    runtime.client,
		ModelTier: llm.TierSimple,
		AgentID:   "sentiment-scorer",
		UserID:    strings.TrimSpace(ownerUserID),
		StepName:  "news_sentiment",
		FundID:    fundID,
	}
	return &sentiment.CompositeScorer{
		Primary:  llmScorer,
		Fallback: keyword,
	}
}

func (s *workflowServiceAdapter) tradingProfileForFund(fund *repository.Fund) marketcalendar.Profile {
	if fund == nil {
		return marketcalendar.Profile{}
	}
	profile := decodeFundMarketProfile(fund.Config)
	out := marketcalendar.Profile{
		Market:       profile.Market,
		Exchange:     profile.Exchange,
		AssetClass:   profile.AssetClass,
		CalendarCode: profile.CalendarCode,
		TimeZone:     profile.TimeZone,
	}
	// Per-fund decision interval. When set, NextWorkflowStart will
	// emit one slot every N minutes inside the market's trading
	// windows instead of the legacy single MacroBrief trigger. The
	// override lives inside the AutoExecute config so toggling
	// auto-execute on or off does not erase the cadence preference.
	if profile.AutoExecute != nil && profile.AutoExecute.DecisionIntervalMinutes != nil {
		v := *profile.AutoExecute.DecisionIntervalMinutes
		out.DecisionIntervalMinutes = &v
	}
	return out
}

func (s *workflowServiceAdapter) resolveTradingDateForFund(fund *repository.Fund, now time.Time, mode marketcalendar.ResolutionMode) (time.Time, error) {
	if s.calendar == nil {
		return workflowTradingDate(now), nil
	}
	return s.calendar.ResolveTradingDate(now, s.tradingProfileForFund(fund), mode)
}

func (s *workflowServiceAdapter) resolveStartTradingDateForFund(fund *repository.Fund, now time.Time) (time.Time, error) {
	tradingDate, err := s.resolveTradingDateForFund(fund, now, marketcalendar.ResolutionCurrentTradingDay)
	if err == nil {
		return tradingDate, nil
	}
	return s.resolveTradingDateForFund(fund, now, marketcalendar.ResolutionNextTradingDay)
}

func (s *workflowServiceAdapter) buildWorkflowScheduleForDate(fund *repository.Fund, tradingDate time.Time) workflow.ScheduleConfig {
	return s.buildWorkflowScheduleForDateAt(fund, tradingDate, time.Now())
}

func (s *workflowServiceAdapter) buildWorkflowScheduleForDateAt(fund *repository.Fund, tradingDate time.Time, now time.Time) workflow.ScheduleConfig {
	schedule := workflow.DefaultSchedule(nil)
	intervals := s.getEffectiveTeamIntervals(fund.ID)
	schedule.ResearcherInterval = time.Duration(intervals.Researcher) * time.Minute
	schedule.PMInterval = time.Duration(intervals.PM) * time.Minute
	schedule.RiskInterval = time.Duration(intervals.Risk) * time.Minute
	schedule.TraderInterval = time.Duration(intervals.Trader) * time.Minute
	if s.calendar == nil {
		return schedule
	}
	session, err := s.calendar.SessionForDate(tradingDate, s.tradingProfileForFund(fund))
	if err != nil || session == nil || !session.IsTradingDay || session.Location == nil {
		return schedule
	}
	stepSchedule := s.calendar.BuildStepSchedule(session)
	schedule.Location = session.Location
	schedule.MacroBriefTime = stepSchedule.MacroBrief.In(session.Location).Format("15:04")
	schedule.ResearchParallelTime = stepSchedule.ResearchParallel.In(session.Location).Format("15:04")
	schedule.QuantSignalsTime = stepSchedule.QuantSignals.In(session.Location).Format("15:04")
	schedule.RoundtableTime = stepSchedule.Roundtable.In(session.Location).Format("15:04")
	schedule.PMPlanTime = stepSchedule.PMPlan.In(session.Location).Format("15:04")
	schedule.RiskReviewTime = stepSchedule.RiskReview.In(session.Location).Format("15:04")
	schedule.UserApprovalTime = stepSchedule.UserApproval.In(session.Location).Format("15:04")
	schedule.TradeExecutionTime = stepSchedule.TradeExecution.In(session.Location).Format("15:04")
	schedule.SettlementTime = stepSchedule.Settlement.In(session.Location).Format("15:04")
	schedule.DailyReviewTime = stepSchedule.DailyReview.In(session.Location).Format("15:04")
	schedule.ForceImmediate = s.shouldRunWorkflowImmediately(now, session)
	return schedule
}

func (s *workflowServiceAdapter) shouldRunWorkflowImmediately(now time.Time, session *marketcalendar.TradingSession) bool {
	if s == nil || s.calendar == nil || session == nil || !session.IsTradingDay || session.Location == nil {
		return false
	}
	stepSchedule := s.calendar.BuildStepSchedule(session)
	localNow := now.In(session.Location)
	return !localNow.Before(stepSchedule.MacroBrief) && localNow.Before(stepSchedule.DailyReview.Add(2*time.Hour))
}

func newTeamServiceAdapter(db *sql.DB, usageTracker *subscription.UsageTracker, modelConfigs *subscription.ModelConfigService, subscriptionService *subscription.SubscriptionService, runtime *llmRuntime) *teamServiceAdapter {
	return &teamServiceAdapter{
		db:                  db,
		fundRepo:            repository.NewFundRepo(db),
		companyRepo:         repository.NewFundCompanyRepo(db),
		agentRepo:           repository.NewAgentRepo(db),
		teamRepo:            repository.NewTeamRepo(db),
		memoryRepo:          repository.NewMemoryRepo(db),
		lineageRepo:         repository.NewLineageRepo(db),
		usageTracker:        usageTracker,
		auditLogger:         audit.NewDBLogger(db),
		modelConfigs:        modelConfigs,
		subscriptionService: subscriptionService,
		llmRuntime:          runtime,
	}
}

// WithActivityBus attaches the in-process workflow activity bus so the
// adapter can serve the Team Live Activity REST + SSE endpoints. Returns the
// receiver for fluent chaining at wire time.
func (s *teamServiceAdapter) WithActivityBus(bus *workflow.ActivityBus) *teamServiceAdapter {
	if s != nil {
		s.activityBus = bus
	}
	return s
}

func newMemoryServiceAdapter(db *sql.DB) *memoryServiceAdapter {
	return &memoryServiceAdapter{
		fundRepo:    repository.NewFundRepo(db),
		companyRepo: repository.NewFundCompanyRepo(db),
		memoryRepo:  repository.NewMemoryRepo(db),
		auditLogger: audit.NewDBLogger(db),
	}
}

func newDecisionTraceServiceAdapter(db *sql.DB, marketData *marketdata.Service, runtime *llmRuntime) *decisionTraceServiceAdapter {
	return &decisionTraceServiceAdapter{
		fundRepo:     repository.NewFundRepo(db),
		companyRepo:  repository.NewFundCompanyRepo(db),
		planRepo:     repository.NewPlanRepo(db),
		tradeRepo:    repository.NewTradeRepo(db),
		workflowRepo: repository.NewWorkflowRunRepo(db),
		memoryRepo:   repository.NewMemoryRepo(db),
		marketData:   marketData,
		llmRuntime:   runtime,
	}
}

func newMarketServiceAdapter(db *sql.DB, marketData *marketdata.Service, runtime *llmRuntime) *marketServiceAdapter {
	return &marketServiceAdapter{
		fundRepo:    repository.NewFundRepo(db),
		companyRepo: repository.NewFundCompanyRepo(db),
		teamRepo:    repository.NewTeamRepo(db),
		agentRepo:   repository.NewAgentRepo(db),
		marketData:  marketData,
		llmRuntime:  runtime,
	}
}

func newWalletServiceAdapter(db *sql.DB) *walletServiceAdapter {
	return &walletServiceAdapter{walletRepo: repository.NewWalletRepo(db)}
}

func newMarketplaceServiceAdapter(db *sql.DB, modelConfigs *subscription.ModelConfigService, subscriptionService *subscription.SubscriptionService, runtime *llmRuntime) *marketplaceServiceAdapter {
	return &marketplaceServiceAdapter{
		fundRepo:            repository.NewFundRepo(db),
		companyRepo:         repository.NewFundCompanyRepo(db),
		agentRepo:           repository.NewAgentRepo(db),
		teamRepo:            repository.NewTeamRepo(db),
		memoryRepo:          repository.NewMemoryRepo(db),
		walletRepo:          repository.NewWalletRepo(db),
		marketplaceRepo:     repository.NewMarketplaceRepo(db),
		lineageRepo:         repository.NewLineageRepo(db),
		uow:                 repository.NewUnitOfWork(db),
		modelConfigs:        modelConfigs,
		subscriptionService: subscriptionService,
		llmRuntime:          runtime,
	}
}

func (s *fundServiceAdapter) CreateCompany(input api.CreateCompanyInput) (*api.Company, error) {
	company := &repository.FundCompany{
		OwnerUserID: input.OwnerUserID,
		Name:        input.Name,
		Description: nullString(input.Description),
	}
	id, err := s.companyRepo.Create(context.Background(), company)
	if err != nil {
		return nil, err
	}
	created, err := s.companyRepo.GetByID(context.Background(), id)
	if err != nil {
		return nil, err
	}
	return convertCompany(created), nil
}

func (s *fundServiceAdapter) ListCompanies(ownerUserID string) ([]api.Company, error) {
	companies, err := s.companyRepo.ListByOwner(context.Background(), ownerUserID)
	if err != nil {
		return nil, err
	}
	result := make([]api.Company, 0, len(companies))
	for i := range companies {
		converted := convertCompany(&companies[i])
		if converted != nil {
			result = append(result, *converted)
		}
	}
	return result, nil
}

func (s *fundServiceAdapter) ListCompanyOverviews(ownerUserID string) ([]api.CompanyOverview, error) {
	companies, err := s.companyRepo.ListByOwner(context.Background(), ownerUserID)
	if err != nil {
		return nil, err
	}

	companyIDs := make([]string, 0, len(companies))
	for i := range companies {
		companyIDs = append(companyIDs, companies[i].ID)
	}

	fundsByCompany := make(map[string][]api.Fund, len(companies))
	funds, err := s.fundRepo.ListByCompanyIDs(context.Background(), companyIDs)
	if err != nil {
		return nil, err
	}
	for i := range funds {
		converted := convertFund(&funds[i])
		if converted == nil {
			continue
		}
		fundsByCompany[funds[i].CompanyID] = append(fundsByCompany[funds[i].CompanyID], *converted)
	}

	result := make([]api.CompanyOverview, 0, len(companies))
	for i := range companies {
		company := companies[i]
		funds := fundsByCompany[company.ID]
		if funds == nil {
			funds = []api.Fund{}
		}
		result = append(result, api.CompanyOverview{
			ID:          company.ID,
			OwnerUserID: company.OwnerUserID,
			Name:        company.Name,
			Description: company.Description.String,
			Funds:       funds,
			CreatedAt:   company.CreatedAt,
			UpdatedAt:   company.UpdatedAt,
		})
	}
	return result, nil
}

func (s *fundServiceAdapter) CreateFund(userID string, input api.CreateFundInput) (*api.Fund, error) {
	if _, err := s.getAuthorizedCompany(userID, input.CompanyID); err != nil {
		return nil, err
	}
	fundConfig, err := buildFundConfigJSON(api.FundConfig{
		Market:                optionalString(input.Market),
		Exchange:              optionalString(input.Exchange),
		AssetClass:            optionalString(input.AssetClass),
		BaseCurrency:          optionalString(input.BaseCurrency),
		BenchmarkSymbol:       optionalString(input.BenchmarkSymbol),
		PrimaryDirection:      optionalString(input.PrimaryDirection),
		CalendarCode:          optionalString(input.CalendarCode),
		TimeZone:              optionalString(input.TimeZone),
		Universe:              input.Universe,
		TeamIntervals:         input.TeamIntervals,
		Specialization:        input.Specialization,
		HardRisk:              input.HardRisk,
		ActivityRetentionDays: input.ActivityRetentionDays,
	}, nil)
	if err != nil {
		return nil, err
	}
	fund := &repository.Fund{
		CompanyID:      input.CompanyID,
		Name:           input.Name,
		Description:    nullString(input.Description),
		TradingMode:    input.TradingMode,
		InitialCapital: input.InitialCapital,
		CurrentCapital: input.InitialCapital,
		TotalAssets:    input.InitialCapital,
		NAV:            1,
		Status:         "active",
		Config:         fundConfig,
	}
	id, err := s.fundRepo.Create(context.Background(), fund)
	if err != nil {
		return nil, err
	}
	created, err := s.fundRepo.GetByID(context.Background(), id)
	if err != nil {
		return nil, err
	}
	// F10.1: nudge the workflow scheduler so the new fund is picked up on
	// the next loop iteration (within milliseconds) instead of waiting out
	// the current poll timer.
	if s.workflowService != nil {
		s.workflowService.WakeScheduler()
	}
	return convertFund(created), nil
}

func (s *fundServiceAdapter) ListFunds(userID, companyID string) ([]api.Fund, error) {
	if _, err := s.getAuthorizedCompany(userID, companyID); err != nil {
		return nil, err
	}
	funds, err := s.fundRepo.ListByCompany(context.Background(), companyID)
	if err != nil {
		return nil, err
	}
	result := make([]api.Fund, 0, len(funds))
	for i := range funds {
		converted := convertFund(&funds[i])
		if converted != nil {
			result = append(result, *converted)
		}
	}
	return result, nil
}

func (s *fundServiceAdapter) GetFund(userID, fundID string) (*api.Fund, error) {
	fund, err := s.getAuthorizedFund(userID, fundID)
	if err != nil {
		return nil, err
	}
	return convertFund(fund), nil
}

func (s *fundServiceAdapter) GetForwardGate(userID, fundID string) (*api.ForwardGateStatus, error) {
	fund, err := s.getAuthorizedFund(userID, fundID)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	navRepo := repository.NewNavSnapshotRepo(s.db)
	teamRepo := repository.NewTeamRepo(s.db)
	agentRepo := repository.NewAgentRepo(s.db)
	navs, err := navRepo.ListByFund(ctx, fund.ID, time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), time.Now().UTC().AddDate(1, 0, 0))
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	team, err := teamRepo.ListByFund(ctx, fund.ID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	return buildForwardGateStatus(fund, navs, team, agentRepo, time.Now().UTC()), nil
}

func buildForwardGateStatus(fund *repository.Fund, navs []repository.NavSnapshot, team []repository.TeamMember, agentRepo *repository.AgentRepo, now time.Time) *api.ForwardGateStatus {
	if fund == nil {
		return nil
	}
	policy := marketplace.EligibilityPolicy{}
	liveSince := forwardLiveSince(fund, navs)
	liveDays, eligibilityErr := policy.CheckEligibility(marketplace.EligibilityInputs{
		LiveSince: liveSince,
		Now:       now,
		NAVPoints: len(navs),
	})
	checks := buildForwardGateChecks(fund, policy, liveSince, liveDays, len(navs), eligibilityErr)
	status := "eligible"
	if eligibilityErr != nil {
		status = "pending"
		if ee, ok := eligibilityErr.(*marketplace.EligibilityError); ok && ee.Reason == "not_live" {
			status = "blocked"
		}
	}
	result := &api.ForwardGateStatus{
		FundID:       fund.ID,
		Status:       status,
		Eligible:     eligibilityErr == nil,
		Mode:         strings.TrimSpace(fund.TradingMode),
		Summary:      forwardGateSummary(status, checks),
		RequiredDays: policy.MinForwardTestDays,
		RequiredNAVs: policy.MinDataPoints,
		LiveDays:     liveDays,
		NAVPoints:    len(navs),
		Checks:       checks,
		GeneratedAt:  now,
	}
	if result.RequiredDays <= 0 {
		result.RequiredDays = marketplace.DefaultMinForwardTestDays
	}
	if result.RequiredNAVs <= 0 {
		result.RequiredNAVs = 10
	}
	if !liveSince.IsZero() {
		result.StartDate = liveSince.Format("2006-01-02")
	}
	if len(navs) > 0 {
		result.EndDate = navs[len(navs)-1].TradingDate.Format("2006-01-02")
	}
	if track := buildForwardTrackRecord(navs); track != nil {
		result.TrackRecord = track
	}
	result.Agents = buildForwardAgentGateStatuses(team, agentRepo, result)
	return result
}

func forwardLiveSince(fund *repository.Fund, navs []repository.NavSnapshot) time.Time {
	if fund == nil || strings.ToLower(strings.TrimSpace(fund.TradingMode)) != "live" {
		return time.Time{}
	}
	if len(navs) > 0 {
		return navs[0].TradingDate.UTC()
	}
	return fund.CreatedAt.UTC()
}

func buildForwardGateChecks(fund *repository.Fund, policy marketplace.EligibilityPolicy, liveSince time.Time, liveDays, navPoints int, eligibilityErr error) []api.ForwardGateCheck {
	requiredDays := policy.MinForwardTestDays
	if requiredDays <= 0 {
		requiredDays = marketplace.DefaultMinForwardTestDays
	}
	requiredPoints := policy.MinDataPoints
	if requiredPoints <= 0 {
		requiredPoints = 10
	}
	modeStatus := "pass"
	modeMessage := "Fund is in live forward-test mode."
	if strings.ToLower(strings.TrimSpace(fund.TradingMode)) != "live" || liveSince.IsZero() {
		modeStatus = "block"
		modeMessage = "Fund must be switched to live forward-test mode before strategy or agent admission."
	}
	checks := []api.ForwardGateCheck{
		{Key: "live_mode", Label: "Live forward-test mode", Status: modeStatus, Message: modeMessage},
		{Key: "min_forward_days", Label: "Minimum forward-test days", Status: passWarnStatus(liveDays >= requiredDays), Required: requiredDays, Current: liveDays, Message: fmt.Sprintf("Requires at least %d live forward-test days; currently %d.", requiredDays, liveDays)},
		{Key: "nav_observations", Label: "NAV observations", Status: passWarnStatus(navPoints >= requiredPoints), Required: requiredPoints, Current: navPoints, Message: fmt.Sprintf("Requires at least %d NAV observations; currently %d.", requiredPoints, navPoints)},
	}
	if ee, ok := eligibilityErr.(*marketplace.EligibilityError); ok {
		for i := range checks {
			switch ee.Reason {
			case "not_live":
				if checks[i].Key == "live_mode" {
					checks[i].Status = "block"
				}
			case "insufficient_days":
				if checks[i].Key == "min_forward_days" {
					checks[i].Status = "warn"
				}
			case "insufficient_data":
				if checks[i].Key == "nav_observations" {
					checks[i].Status = "warn"
				}
			}
		}
	}
	return checks
}

func passWarnStatus(ok bool) string {
	if ok {
		return "pass"
	}
	return "warn"
}

func forwardGateSummary(status string, checks []api.ForwardGateCheck) string {
	switch status {
	case "eligible":
		return "Strategy and team agents pass the forward-test gate and can be considered for marketplace/listing flows."
	case "blocked":
		return "Forward-test gate is blocked because the fund is not in live forward-test mode."
	default:
		blockers := make([]string, 0)
		for _, check := range checks {
			if check.Status != "pass" && strings.TrimSpace(check.Message) != "" {
				blockers = append(blockers, check.Message)
			}
		}
		if len(blockers) > 0 {
			return strings.Join(blockers, " ")
		}
		return "Forward-test gate is still collecting enough live evidence."
	}
}

func buildForwardTrackRecord(navs []repository.NavSnapshot) *api.ForwardTrackRecord {
	observations := make([]marketplace.NAVObservation, 0, len(navs))
	for _, nav := range navs {
		observations = append(observations, marketplace.NAVObservation{Date: nav.TradingDate, NAV: nav.NAV})
	}
	record, err := marketplace.ComputeTrackRecord(observations)
	if err != nil {
		return nil
	}
	return &api.ForwardTrackRecord{
		TotalReturn:  record.TotalReturn,
		AnnualReturn: record.AnnualReturn,
		Sharpe:       record.Sharpe,
		MaxDrawdown:  record.MaxDrawdown,
		Volatility:   record.Volatility,
		WinRate:      record.WinRate,
	}
}

func buildForwardAgentGateStatuses(team []repository.TeamMember, agentRepo *repository.AgentRepo, fundGate *api.ForwardGateStatus) []api.ForwardAgentGateStatus {
	if len(team) == 0 || fundGate == nil {
		return nil
	}
	result := make([]api.ForwardAgentGateStatus, 0, len(team))
	for _, member := range team {
		agentName := ""
		role := strings.TrimSpace(member.Role)
		if agentRepo != nil {
			if agent, err := agentRepo.GetByID(context.Background(), member.AgentID); err == nil && agent != nil {
				agentName = agent.Name
				if role == "" {
					role = agent.Role
				}
			}
		}
		checks := append([]api.ForwardGateCheck(nil), fundGate.Checks...)
		memberActive := strings.TrimSpace(member.Status) == "" || strings.EqualFold(member.Status, "active")
		memberCheckStatus := "pass"
		memberMessage := "Agent is active in the fund team."
		if !memberActive {
			memberCheckStatus = "block"
			memberMessage = "Agent is not active in the fund team."
		}
		checks = append(checks, api.ForwardGateCheck{Key: "team_membership", Label: "Active team membership", Status: memberCheckStatus, Message: memberMessage})
		eligible := fundGate.Eligible && memberActive
		blockers, warnings := splitForwardGateIssues(checks)
		status := "eligible"
		if !eligible {
			status = "pending"
			if len(blockers) > 0 {
				status = "blocked"
			}
		}
		result = append(result, api.ForwardAgentGateStatus{
			AgentID:   member.AgentID,
			AgentName: agentName,
			Role:      role,
			Focus:     member.Focus.String,
			Status:    status,
			Eligible:  eligible,
			JoinedAt:  member.JoinedAt,
			Checks:    checks,
			CanList:   eligible,
			Blockers:  blockers,
			Warnings:  warnings,
		})
	}
	return result
}

func splitForwardGateIssues(checks []api.ForwardGateCheck) ([]string, []string) {
	blockers := make([]string, 0)
	warnings := make([]string, 0)
	for _, check := range checks {
		if check.Status == "pass" {
			continue
		}
		message := strings.TrimSpace(check.Message)
		if message == "" {
			message = check.Label
		}
		if check.Status == "block" {
			blockers = append(blockers, message)
		} else {
			warnings = append(warnings, message)
		}
	}
	return uniqueNonEmpty(blockers), uniqueNonEmpty(warnings)
}

func (s *fundServiceAdapter) UpdateFund(userID, fundID string, cfg api.FundConfig) (*api.Fund, error) {
	// Authorisation happens against the un-locked snapshot first:
	// the caller's ability to touch this fund is a property of the
	// (user, company) ownership graph and doesn't depend on the
	// row contents we're about to mutate. Pulling it here avoids
	// holding the row lock across the company lookup.
	if _, err := s.getAuthorizedFund(userID, fundID); err != nil {
		return nil, err
	}

	ctx := context.Background()
	// Wrap the read-modify-write in a transaction with SELECT ...
	// FOR UPDATE so two concurrent PUTs on the same fund are
	// serialised. Without the lock, writer-B can read the fund
	// snapshot before writer-A's UPDATE commits, then merge over
	// the top of writer-A's pending change — see Test 12 in the
	// May-22 P2 sweep, which observed ~26% lost-update rate under
	// 50 iterations of two concurrent writers.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("update fund: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	fund, err := s.fundRepo.GetByIDForUpdateTx(ctx, tx, fundID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}

	oldInitialCapital := fund.InitialCapital
	if cfg.Name != nil {
		fund.Name = *cfg.Name
	}
	if cfg.Description != nil {
		fund.Description = nullString(*cfg.Description)
	}
	if cfg.TradingMode != nil {
		fund.TradingMode = *cfg.TradingMode
	}
	if cfg.Status != nil {
		fund.Status = *cfg.Status
	}
	if cfg.InitialCapital != nil {
		fund.InitialCapital = *cfg.InitialCapital
		if fund.CurrentCapital == oldInitialCapital {
			fund.CurrentCapital = *cfg.InitialCapital
		}
		if fund.TotalAssets == oldInitialCapital {
			fund.TotalAssets = *cfg.InitialCapital
		}
	}
	updatedConfig, err := buildFundConfigJSON(cfg, fund.Config)
	if err != nil {
		return nil, err
	}
	fund.Config = updatedConfig

	if _, err := tx.ExecContext(ctx,
		`UPDATE funds
		 SET name = $1, description = $2, trading_mode = $3, status = $4, config = $5,
		     initial_capital = $6, current_capital = $7, total_assets = $8, nav = $9, updated_at = NOW()
		 WHERE id = $10`,
		fund.Name,
		fund.Description,
		fund.TradingMode,
		fund.Status,
		fund.Config,
		fund.InitialCapital,
		fund.CurrentCapital,
		fund.TotalAssets,
		fund.NAV,
		fund.ID,
	); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("update fund: commit: %w", err)
	}
	committed = true

	updated, err := s.fundRepo.GetByID(ctx, fundID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	// F10.1: changes to status/calendar/intervals can shift the next trigger
	// time. Nudge the scheduler so the new schedule takes effect promptly.
	if s.workflowService != nil {
		s.workflowService.WakeScheduler()
	}
	return convertFund(updated), nil
}

func (s *fundServiceAdapter) DeleteFund(userID, fundID string) error {
	if _, err := s.getAuthorizedFund(userID, fundID); err != nil {
		return err
	}
	if s.workflowService != nil {
		s.workflowService.stopRuntime(fundID)
	}
	if err := s.fundRepo.Delete(context.Background(), fundID); err != nil {
		return mapRepositoryError(err)
	}
	// F10.1: pruning a fund changes the scheduler's set of due funds;
	// wake it so the snapshot reflects reality on the next loop tick.
	if s.workflowService != nil {
		s.workflowService.WakeScheduler()
	}
	return nil
}

// ListPlans powers the decision-center sidebar. The sidebar only
// reads planTitle / status / expectedReturn / riskScore / actionCount
// for each plan — none of which depend on LLM-translated reasoning.
//
// F32: we therefore pass nil runtime to skip the per-plan translation
// round-trips that previously made this endpoint sit on 50 × N LLM
// calls (≈ 90 s wall time with N=2 round-trips per plan against a
// remote Gemini gateway). Reasoning is still returned as source text
// so the FE's pickLocalizedText helper falls back to the original
// language when displaying rejection notes; the detail view fetches
// the fully-translated plan through GET /decision-trace, which is
// served from the locality cache after the first hit.
func (s *planServiceAdapter) ListPlans(userID, fundID string, filter api.PlanListFilter) ([]api.Plan, error) {
	if _, err := authorizeFundAccess(context.Background(), s.fundRepo, s.companyRepo, userID, fundID); err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	plans, err := s.planRepo.ListByFundPageFiltered(context.Background(), fundID, repository.PlanListFilter{
		Limit:  limit,
		Offset: offset,
		Status: strings.TrimSpace(filter.Status),
		From:   filter.From,
		To:     filter.To,
	})
	if err != nil {
		return nil, err
	}

	result := make([]api.Plan, 0, len(plans))
	for i := range plans {
		actions, err := s.planRepo.GetActions(context.Background(), plans[i].ID)
		if err != nil {
			return nil, err
		}
		plan := convertPlanWithLocale(userID, nil, &plans[i], actions)
		if plan != nil {
			result = append(result, *plan)
		}
	}
	return result, nil
}

func (s *planServiceAdapter) GetPlan(userID, planID string) (*api.Plan, error) {
	plan, err := s.planRepo.GetByID(context.Background(), planID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if _, err := authorizeFundAccess(context.Background(), s.fundRepo, s.companyRepo, userID, plan.FundID); err != nil {
		return nil, err
	}
	return s.getPlanWithActions(userID, planID)
}

func (s *planServiceAdapter) ApprovePlan(userID, planID string) (*api.Plan, error) {
	plan, err := s.planRepo.GetByID(context.Background(), planID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if _, err := authorizeFundAccess(context.Background(), s.fundRepo, s.companyRepo, userID, plan.FundID); err != nil {
		return nil, err
	}
	if plan.Status == "approved" || plan.Status == "completed" || plan.Status == "executing" {
		return s.getPlanWithActions(userID, planID)
	}
	if plan.Status != "pending" && plan.Status != "pending_user" {
		return nil, api.ErrConflict
	}
	if s.workflowService != nil {
		if err := s.workflowService.validateApprovePlanResume(plan); err != nil {
			return nil, err
		}
	}
	previousStatus := plan.Status
	if err := s.planRepo.UpdateStatus(context.Background(), planID, "approved"); err != nil {
		return nil, mapRepositoryError(err)
	}
	if s.workflowService != nil {
		if err := s.workflowService.ResumeApprovedPlan(plan.FundID, normalizeTradingDate(plan.TradingDate), plan.ID); err != nil {
			if rollbackErr := s.planRepo.UpdateStatus(context.Background(), planID, previousStatus); rollbackErr != nil {
				return nil, fmt.Errorf("resume approved plan: %w (rollback plan status: %v)", err, rollbackErr)
			}
			return nil, err
		}
	}
	return s.getPlanWithActions(userID, planID)
}

func (s *planServiceAdapter) RejectPlan(userID, planID, reason string) (*api.Plan, error) {
	plan, err := s.planRepo.GetByID(context.Background(), planID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if _, err := authorizeFundAccess(context.Background(), s.fundRepo, s.companyRepo, userID, plan.FundID); err != nil {
		return nil, err
	}
	if plan.Status == "rejected" {
		return s.getPlanWithActions(userID, planID)
	}
	if plan.Status != "pending" && plan.Status != "pending_user" {
		return nil, api.ErrConflict
	}
	if err := s.rejectPlan(context.Background(), planID, reason); err != nil {
		return nil, err
	}
	if s.workflowService != nil {
		if err := s.workflowService.RejectAwaitingPlan(plan.FundID, normalizeTradingDate(plan.TradingDate), plan.ID, reason); err != nil {
			return nil, err
		}
	}
	return s.getPlanWithActions(userID, planID)
}

// RefreshPlanQuote re-prices every still-pending action in the plan
// against the latest market quote and re-applies A-share lot-size rules
// so the user re-approves with a current snapshot. Prior to the
// SlippageGuard rollout this was a remediation-only path that fired
// only for actions whose plan-generation quote was unavailable; the new
// product flow treats refresh as a first-class user action ("I'm about
// to approve, give me current prices"), so we now refresh:
//
//   - buy/add: re-price, then re-normalise quantity to the budget
//     (action.Amount) using the new price and the board's lot rules.
//     Sells of an existing position aren't re-quantised — the user
//     approved selling N shares, the price drift doesn't change that.
//   - sell/reduce: re-price only; quantity stays the same. amount is
//     recomputed so notional displays match the new price.
//   - hold/watch: pure informational refresh (reasoning note); no
//     quantity/amount change because the action wouldn't execute.
//
// quote_refreshed_at is stamped on every successfully refreshed row,
// which the API/UI surface as a "last refreshed N min ago" badge so
// the user can tell whether the prices they see are current.
//
// Returns ErrUpstreamUnavailable only if zero actions could be re-
// priced (e.g. market closed, data provider down). Per-action failures
// are tolerated and visible in the response (those actions just keep
// their previous price/quote_refreshed_at).
func (s *planServiceAdapter) RefreshPlanQuote(ctx context.Context, userID, planID string) (*api.Plan, error) {
	plan, err := s.planRepo.GetByID(ctx, planID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	fund, err := authorizeFundAccess(ctx, s.fundRepo, s.companyRepo, userID, plan.FundID)
	if err != nil {
		return nil, err
	}
	if plan.Status != "pending" && plan.Status != "pending_user" {
		return nil, api.ErrConflict
	}
	if s.workflowService == nil || s.workflowService.marketData == nil || !s.workflowService.marketData.Enabled() {
		return nil, fmt.Errorf("market data service unavailable: %w", api.ErrUpstreamUnavailable)
	}
	actions, err := s.planRepo.GetActions(ctx, planID)
	if err != nil {
		return nil, err
	}
	lang := LanguageFromContext(ctx)
	refreshed := 0
	for _, action := range actions {
		if terminalActionStatus(action.ExecutionStatus) != "" {
			continue
		}
		symbol := strings.TrimSpace(action.Symbol)
		if symbol == "" {
			continue
		}
		instrumentRef := planActionInstrumentRef(
			fund,
			action.Symbol,
			action.InstrumentKey,
			action.Market.String,
			action.Exchange.String,
			action.AssetClass.String,
			action.InstrumentType.String,
			action.QuoteCurrency.String,
			action.SettlementCurrency.String,
			contractMultiplierValue(action.ContractMultiplier),
			formatNullTime(action.ExpiryDate),
		)
		quote, qErr := s.workflowService.marketData.GetQuote(ctx, instrumentRef)
		if qErr != nil || quote == nil || quote.Price <= 0 {
			continue
		}

		newPrice := quote.Price
		newQty, newAmount := refreshActionQuantity(action, fund, newPrice)
		// Hold/watch actions return (0,0) — we still want to stamp
		// quote_refreshed_at and refresh the reasoning note so the UI
		// can show the latest price, but we skip the qty/amount
		// updates by passing the existing values back to the repo.
		if newQty <= 0 {
			newQty = action.Quantity.Float64
			newAmount = action.Amount.Float64
		}
		newReasoning := stripQuoteUnavailableNote(action.Reasoning.String)
		newReasoning = appendQuoteReference(lang, newReasoning, quote)
		if err := s.planRepo.UpdateActionQuote(ctx, action.ID, newPrice, newQty, newAmount, newReasoning); err != nil {
			return nil, mapRepositoryError(err)
		}
		refreshed++
	}
	if refreshed == 0 {
		return nil, fmt.Errorf("no actions could be refreshed: %w", api.ErrUpstreamUnavailable)
	}
	return s.getPlanWithActions(userID, planID)
}

// refreshActionQuantity computes the (quantity, amount) pair to write
// back for an action being re-priced. It is split out from
// RefreshPlanQuote so the per-action policy (which side keeps what) is
// testable in isolation.
//
// Returns (0, 0) for non-tradeable actions (hold/watch). For buy/add,
// the quantity is recomputed from action.Amount (the user-approved
// budget) using the new price, then normalised to A-share lot rules.
// For sell/reduce, quantity is preserved verbatim and only the amount
// is recomputed.
func refreshActionQuantity(action repository.PlanAction, fund *repository.Fund, newPrice float64) (qty, amount float64) {
	if newPrice <= 0 {
		return 0, 0
	}
	side := strings.ToLower(strings.TrimSpace(action.Action))
	switch side {
	case "buy", "add":
		// Use the originally approved notional budget as the anchor;
		// fall back to qty*price if Amount is missing, then to 25% of
		// current capital if the action arrived essentially empty
		// (legacy plans without an explicit budget).
		budget := action.Amount.Float64
		if budget <= 0 {
			budget = action.Price.Float64 * action.Quantity.Float64
		}
		if budget <= 0 && fund != nil && fund.CurrentCapital > 0 {
			budget = roundCurrency(fund.CurrentCapital * 0.25)
		}
		if budget <= 0 {
			return 0, 0
		}
		raw := math.Floor(budget / newPrice)
		// Lot-size normalise so the refreshed quantity remains a legal
		// A-share order. Non-A-share symbols pass through unchanged.
		normalized := instrument2.NormalizeBuyQty(action.Symbol, instrument2.Hint{
			Market:     action.Market.String,
			Exchange:   action.Exchange.String,
			AssetClass: action.AssetClass.String,
		}, raw)
		if normalized <= 0 {
			// Budget too small for one minimum lot on this board.
			// Leave quantity untouched so the UI can still show the
			// refreshed price; the caller is expected to surface a
			// "below minimum lot" warning to the user.
			return 0, 0
		}
		return normalized, roundCurrency(normalized * newPrice)
	case "sell", "reduce":
		qty := action.Quantity.Float64
		if qty <= 0 {
			return 0, 0
		}
		return qty, roundCurrency(qty * newPrice)
	default:
		return 0, 0
	}
}

func stripQuoteUnavailableNote(reasoning string) string {
	if strings.TrimSpace(reasoning) == "" {
		return reasoning
	}
	lines := strings.Split(reasoning, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.Contains(strings.ToLower(line), "quote unavailable") {
			continue
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func (s *planServiceAdapter) getPlanWithActions(userID, planID string) (*api.Plan, error) {
	plan, err := s.planRepo.GetByID(context.Background(), planID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	actions, err := s.planRepo.GetActions(context.Background(), planID)
	if err != nil {
		return nil, err
	}
	converted := convertPlanWithLocale(userID, s.llmRuntime, plan, actions)
	attachDecisionSource(context.Background(), s.planRepo, converted)
	return converted, nil
}

func (s *planServiceAdapter) rejectPlan(ctx context.Context, planID, reason string) error {
	res, err := s.planRepo.DB().ExecContext(ctx,
		`UPDATE investment_plans SET status = $1, reasoning = $2, updated_at = NOW() WHERE id = $3`,
		"rejected", reason, planID,
	)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return api.ErrNotFound
	}
	return nil
}

type decisionTraceSnapshot struct {
	RoundtableID string   `json:"roundtableId,omitempty"`
	Rounds       int      `json:"rounds,omitempty"`
	Consensus    []string `json:"consensus,omitempty"`
	Summary      string   `json:"summary,omitempty"`
	GeneratedAt  string   `json:"generatedAt,omitempty"`
}

type decisionTraceRiskReviewPayload struct {
	Verdict     string                          `json:"verdict,omitempty"`
	Commentary  string                          `json:"commentary,omitempty"`
	OverallNote string                          `json:"overallNote,omitempty"`
	Warnings    []string                        `json:"warnings,omitempty"`
	Rejections  []string                        `json:"rejections,omitempty"`
	Suggestions []string                        `json:"suggestions,omitempty"`
	Checks      []decisionTraceRiskCheckPayload `json:"checks,omitempty"`
}

type decisionTraceRiskCheckPayload struct {
	Rule      string   `json:"rule,omitempty"`
	Name      string   `json:"name,omitempty"`
	Status    string   `json:"status,omitempty"`
	Result    string   `json:"result,omitempty"`
	Current   *float64 `json:"current,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
	Message   string   `json:"message,omitempty"`
	Detail    string   `json:"detail,omitempty"`
}

var decisionTraceStepOrder = []string{
	"macro_brief",
	"research_parallel",
	"quant_signals",
	"roundtable",
	"pm_plan",
	"risk_review",
	"user_approval",
	"trade_execution",
	"settlement",
	"daily_review",
}

type bilingualText struct {
	Zh string `json:"zh"`
	En string `json:"en"`
}

type bilingualDiscussion struct {
	Reasoning   bilingualText `json:"reasoning"`
	Summary     bilingualText `json:"summary"`
	ConsensusZh []string      `json:"consensusZh"`
	ConsensusEn []string      `json:"consensusEn"`
}

// llmEnrichmentTimeout matches the global 5-minute LLM budget
// declared in llm/client.go (llmTotalRequestTimeout). Keeping the
// two in sync means a translation / decision-trace enrichment never
// times out before the underlying llm.Client gives up on its own
// 5-min cap (which would otherwise cause confusing "context deadline"
// errors mid-flight).
const llmEnrichmentTimeout = 5 * time.Minute

// decisionTraceTranslationCache memoises translation results across
// decision-trace requests so opening the same plan twice — or any two
// plans that share action reasoning text — does not re-pay the LLM
// round-trip cost. See decision_trace_locale_cache.go for the design.
var decisionTraceTranslationCache = newTranslationLocaleCache(4096, 24*time.Hour, 30*time.Second)

func llmEnrichmentContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), llmEnrichmentTimeout)
}

// translateBilingualText returns ZH/EN variants of source. The result
// is cached on a content-addressed key (step + tier + targets +
// source) and de-duplicated via single-flight so concurrent decision
// trace requests for the same plan cost one LLM round-trip total.
//
// We intentionally do NOT short-circuit when the source already looks
// monolingual: a Chinese-source plan still needs an English variant
// for en-US users, and vice versa. The FE falls back to source-when-
// missing, which would silently break that cross-language case. The
// cache + single-flight + parallel fan-out below give us most of the
// win without that footgun.
func translateBilingualText(userID string, runtime *llmRuntime, stepName string, source string, tier llm.ModelTier) bilingualText {
	source = strings.TrimSpace(source)
	if runtime == nil || source == "" {
		return bilingualText{}
	}

	key := translationCacheKey(stepName, string(tier), translationTargetsBilingual, []string{"text:" + source})
	zh, en, _ := decisionTraceTranslationCache.GetOrLoad(key, func() (zhOut []string, enOut []string, failed bool) {
		ctx, cancel := llmEnrichmentContext()
		defer cancel()
		resp, err := runtime.Chat(ctx, llm.ChatRequest{
			UserID:    strings.TrimSpace(userID),
			ModelTier: tier,
			StepName:  stepName,
			Messages: []llm.ChatMessage{
				{
					Role:    "system",
					Content: "Return only compact JSON with keys zh and en. Translate the input text into Simplified Chinese and English. Preserve finance-specific symbols, numbers, ticker codes, line breaks, and meaning.",
				},
				{
					Role:    "user",
					Content: source,
				},
			},
		})
		if err != nil || resp == nil {
			return nil, nil, true
		}
		var parsed bilingualText
		if err := json.Unmarshal([]byte(strings.TrimSpace(resp.Content)), &parsed); err != nil {
			return nil, nil, true
		}
		zhValue := strings.TrimSpace(parsed.Zh)
		enValue := strings.TrimSpace(parsed.En)
		return []string{zhValue}, []string{enValue}, zhValue == "" && enValue == ""
	})
	out := bilingualText{}
	if len(zh) > 0 {
		out.Zh = zh[0]
	}
	if len(en) > 0 {
		out.En = en[0]
	}
	return out
}

// containsAnyCJK is a cheap helper used in the short-circuit path of
// translateBilingualText. We don't reuse looksLikeChineseOnly because
// that helper allows mixed Latin which is the common case for fund
// reasoning text.
func containsAnyCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
		if r >= 0x3400 && r <= 0x4DBF {
			return true
		}
		if r >= 0x20000 && r <= 0x2A6DF {
			return true
		}
	}
	return false
}

// translationListChunkSize caps how many items we send to the LLM in one
// translation call. Picked so 8 long-form English news headlines plus
// their Chinese variants comfortably fit inside the 4096-token output
// budget (a single 30-item batch was overflowing → JSON truncation →
// every item silently dropped, which is exactly the "新闻一直英文" bug).
const translationListChunkSize = 8

// translationListMaxParallel bounds how many translation chunks we run
// against the LLM at once for a single high-level call. The wider
// scheduler.LLMRuntime semaphore still applies on top of this.
const translationListMaxParallel = 4

// translateBilingualList batches a list of strings into LLM translation
// calls and returns ZH/EN slices aligned to the input. Results are
// cached on a content-addressed key so opening the same plan twice
// (or two plans sharing action reasoning) is free.
//
// Large inputs are split into fixed-size chunks (translationListChunkSize)
// and fanned out in parallel. Each chunk has its own cache key, so a
// warm 8-item chunk skips the LLM entirely while a cold chunk still
// gets retried on its own. This both keeps each call within the
// 4096-token output budget and lets a single warm chunk benefit a
// previously-failed run.
//
// We intentionally do NOT short-circuit by language detection: an
// all-Chinese list still needs an English variant for en-US users, and
// vice versa. The FE falls back to source-when-missing, so skipping
// translation would silently leave one side of the audience without a
// translated rendering. Caching + parallel fan-out give us most of the
// latency win without that trade-off.
func translateBilingualList(userID string, runtime *llmRuntime, stepName string, values []string, tier llm.ModelTier) ([]string, []string) {
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			normalized = append(normalized, trimmed)
		}
	}
	if runtime == nil || len(normalized) == 0 {
		return nil, nil
	}

	// Small inputs avoid the chunking overhead and keep the legacy cache
	// key shape stable for already-warm plan/action translations.
	if len(normalized) <= translationListChunkSize {
		return translateBilingualChunk(userID, runtime, stepName, normalized, tier)
	}

	chunks := make([][]string, 0, (len(normalized)+translationListChunkSize-1)/translationListChunkSize)
	for i := 0; i < len(normalized); i += translationListChunkSize {
		end := i + translationListChunkSize
		if end > len(normalized) {
			end = len(normalized)
		}
		chunks = append(chunks, normalized[i:end])
	}

	zhParts := make([][]string, len(chunks))
	enParts := make([][]string, len(chunks))
	sem := make(chan struct{}, translationListMaxParallel)
	var wg sync.WaitGroup
	for i, chunk := range chunks {
		wg.Add(1)
		go func(idx int, items []string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			zh, en := translateBilingualChunk(userID, runtime, stepName, items, tier)
			zhParts[idx] = zh
			enParts[idx] = en
		}(i, chunk)
	}
	wg.Wait()

	// Each chunk is independent: if a single chunk truncated/erred, we
	// surface the partial successes and let the caller's missing-fallback
	// logic show the source for the gaps, instead of nuking the whole
	// translation set as the previous single-shot path did.
	flatZh := make([]string, 0, len(normalized))
	flatEn := make([]string, 0, len(normalized))
	for i := range chunks {
		flatZh = appendAligned(flatZh, zhParts[i], len(chunks[i]))
		flatEn = appendAligned(flatEn, enParts[i], len(chunks[i]))
	}
	return flatZh, flatEn
}

// translateBilingualChunk performs the actual LLM round trip for one
// chunk. Behaviour is identical to the legacy translateBilingualList
// single-shot path, so cache keys produced by old (small-list) callers
// keep returning the same value.
func translateBilingualChunk(userID string, runtime *llmRuntime, stepName string, normalized []string, tier llm.ModelTier) ([]string, []string) {
	if runtime == nil || len(normalized) == 0 {
		return nil, nil
	}
	key := translationCacheKey(stepName, string(tier), translationTargetsBilingual, normalized)
	zh, en, _ := decisionTraceTranslationCache.GetOrLoad(key, func() (zhOut []string, enOut []string, failed bool) {
		ctx, cancel := llmEnrichmentContext()
		defer cancel()
		payload, err := json.Marshal(normalized)
		if err != nil {
			return nil, nil, true
		}
		resp, err := runtime.Chat(ctx, llm.ChatRequest{
			UserID:    strings.TrimSpace(userID),
			ModelTier: tier,
			StepName:  stepName,
			Messages: []llm.ChatMessage{
				{
					Role:    "system",
					Content: "Return only compact JSON with keys consensusZh and consensusEn. Translate each list item into Simplified Chinese and English. Preserve order, numbers, finance terms, and ticker codes.",
				},
				{
					Role:    "user",
					Content: string(payload),
				},
			},
		})
		if err != nil || resp == nil {
			return nil, nil, true
		}
		var parsed bilingualDiscussion
		if err := json.Unmarshal([]byte(strings.TrimSpace(resp.Content)), &parsed); err != nil {
			return nil, nil, true
		}
		gotZh := normalizeStringList(parsed.ConsensusZh)
		gotEn := normalizeStringList(parsed.ConsensusEn)
		return gotZh, gotEn, len(gotZh) == 0 && len(gotEn) == 0
	})
	return zh, en
}

// appendAligned pads `chunk` to `want` length with empty strings so the
// concatenated slice keeps the same index alignment as the original
// input, even when a chunk only partially translated.
func appendAligned(out, chunk []string, want int) []string {
	if len(chunk) >= want {
		return append(out, chunk[:want]...)
	}
	out = append(out, chunk...)
	for i := len(chunk); i < want; i++ {
		out = append(out, "")
	}
	return out
}

func normalizeStringList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func (s *decisionTraceServiceAdapter) GetDecisionTrace(userID, fundID, tradingDate, planID string) (*api.DecisionTrace, error) {
	ctx := context.Background()
	fund, err := authorizeFundAccess(ctx, s.fundRepo, s.companyRepo, userID, fundID)
	if err != nil {
		return nil, err
	}

	selectedPlan, targetDate, err := s.resolveDecisionTracePlan(ctx, fundID, tradingDate, planID)
	if err != nil {
		return nil, err
	}

	if selectedPlan == nil && targetDate.IsZero() {
		return nil, api.ErrNotFound
	}
	if selectedPlan != nil && targetDate.IsZero() {
		targetDate = selectedPlan.TradingDate.UTC()
	}

	run, err := s.workflowRepo.GetByFundAndDate(ctx, fundID, targetDate)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return nil, mapRepositoryError(err)
	}
	if errors.Is(err, repository.ErrNotFound) {
		run = nil
	}

	if selectedPlan == nil && run == nil {
		return nil, api.ErrNotFound
	}

	var actions []repository.PlanAction
	if selectedPlan != nil {
		actions, err = s.planRepo.GetActions(ctx, selectedPlan.ID)
		if err != nil {
			return nil, mapRepositoryError(err)
		}
	}

	var trades []repository.TradeExecution
	if selectedPlan != nil {
		trades, err = s.tradeRepo.ListByPlan(ctx, selectedPlan.ID)
		if err != nil {
			return nil, mapRepositoryError(err)
		}
	}

	memories, err := s.memoryRepo.ListByFundAndDate(ctx, fundID, targetDate, 200)
	if err != nil {
		return nil, mapRepositoryError(err)
	}

	// F32: run the two LLM-heavy builders concurrently. Each helper
	// internally fires 1-2 translation round-trips, so taking them
	// off the serial critical path roughly halves the first-paint
	// latency for the Decision Center. Subsequent clicks on the same
	// plan are served from the locality cache and skip LLM entirely.
	var (
		fanOut     sync.WaitGroup
		discussion *api.DecisionTraceDiscussion
		planView   *api.Plan
	)
	fanOut.Add(1)
	go func() {
		defer fanOut.Done()
		discussion = buildDecisionTraceDiscussionWithLocale(userID, s.llmRuntime, selectedPlan)
	}()
	if selectedPlan != nil {
		fanOut.Add(1)
		go func() {
			defer fanOut.Done()
			planView = convertPlanWithLocale(userID, s.llmRuntime, selectedPlan, actions)
			// Sprint 11.3 — the decision-trace card is exactly
			// where users go to understand "where did this plan
			// come from", so the provenance chip belongs here.
			attachDecisionSource(context.Background(), s.planRepo, planView)
		}()
	}
	// Non-LLM builders can run on the calling goroutine while the
	// translations are in flight — they only touch the local DB
	// rows already fetched above.
	execution := buildDecisionTraceExecution(actions, trades)
	review := buildDecisionTraceReview(memories)
	var research []api.MarketResearch
	if selectedPlan != nil {
		research = s.buildDecisionTraceResearch("", fund, actions)
	}
	fanOut.Wait()

	trace := &api.DecisionTrace{
		FundID:      fundID,
		TradingDate: targetDate.Format("2006-01-02"),
		Discussion:  discussion,
		Execution:   execution,
		Review:      review,
	}
	if selectedPlan != nil {
		trace.Plan = planView
		trace.Research = research
		trace.Risk = buildRiskExplanation(trace.Plan.RiskReview)
		trace.Memo = buildCommitteeMemo(trace.Plan, trace.Discussion, trace.Execution, trace.Research)
	}
	if run != nil {
		trace.Run = buildDecisionTraceRun(run)
	}
	return trace, nil
}

func (s *decisionTraceServiceAdapter) resolveDecisionTracePlan(ctx context.Context, fundID, tradingDate, planID string) (*repository.InvestmentPlan, time.Time, error) {
	trimmedPlanID := strings.TrimSpace(planID)
	if trimmedPlanID != "" {
		plan, err := s.planRepo.GetByID(ctx, trimmedPlanID)
		if err != nil {
			return nil, time.Time{}, mapRepositoryError(err)
		}
		if strings.TrimSpace(plan.FundID) != strings.TrimSpace(fundID) {
			return nil, time.Time{}, api.ErrNotFound
		}
		return plan, plan.TradingDate.UTC(), nil
	}

	trimmedDate := strings.TrimSpace(tradingDate)
	if trimmedDate != "" {
		parsed, err := time.Parse("2006-01-02", trimmedDate)
		if err != nil {
			return nil, time.Time{}, api.ErrBadInput
		}
		targetDate := parsed.UTC()
		plan, err := s.planRepo.GetLatestByFundAndDate(ctx, fundID, targetDate)
		if err != nil && !errors.Is(err, repository.ErrNotFound) {
			return nil, time.Time{}, mapRepositoryError(err)
		}
		if errors.Is(err, repository.ErrNotFound) {
			return nil, targetDate, nil
		}
		return plan, targetDate, nil
	}

	plans, err := s.planRepo.ListByFund(ctx, fundID, 1)
	if err != nil {
		return nil, time.Time{}, mapRepositoryError(err)
	}
	if len(plans) > 0 {
		plan := plans[0]
		return &plan, plan.TradingDate.UTC(), nil
	}
	tradingRun, err := s.workflowRepo.GetLatestByFund(ctx, fundID)
	if err != nil {
		return nil, time.Time{}, mapRepositoryError(err)
	}
	return nil, tradingRun.TradingDate.UTC(), nil
}

func buildDecisionTraceRun(run *repository.WorkflowRun) *api.DecisionTraceRun {
	status := convertWorkflowStatus(run)
	if status == nil {
		return nil
	}
	result := &api.DecisionTraceRun{
		State:       status.State,
		Step:        status.Step,
		StartedAt:   status.StartedAt,
		CompletedAt: status.CompletedAt,
		Steps:       buildOrderedDecisionTraceSteps(status.StepResults, status.Step),
		RunID:       run.ID,
	}
	return result
}

func buildOrderedDecisionTraceSteps(stepResults map[string]api.WorkflowStepStatus, currentStep string) []api.DecisionTraceStep {
	result := make([]api.DecisionTraceStep, 0, len(decisionTraceStepOrder))
	for _, step := range decisionTraceStepOrder {
		item, ok := stepResults[step]
		status := item.Status
		if !ok || strings.TrimSpace(status) == "" {
			status = "pending"
		}
		if strings.TrimSpace(currentStep) == step && status == "pending" {
			status = "running"
		}
		result = append(result, api.DecisionTraceStep{
			Step:      step,
			Status:    status,
			StartedAt: item.StartedAt,
			EndedAt:   item.EndedAt,
			UpdatedAt: item.UpdatedAt,
			Error:     item.Error,
		})
	}
	return result
}

func buildDecisionTraceDiscussion(plan *repository.InvestmentPlan) *api.DecisionTraceDiscussion {
	return buildDecisionTraceDiscussionWithLocale("", nil, plan)
}

func buildDecisionTraceDiscussionWithLocale(userID string, runtime *llmRuntime, plan *repository.InvestmentPlan) *api.DecisionTraceDiscussion {
	if plan == nil {
		return nil
	}
	result := &api.DecisionTraceDiscussion{
		Reasoning: strings.TrimSpace(plan.Reasoning.String),
	}
	if hasDecisionTraceSnapshot(plan.DiscussionSnapshot) {
		result.HasSnapshot = true
		result.Snapshot = append(json.RawMessage(nil), plan.DiscussionSnapshot...)
		var snapshot decisionTraceSnapshot
		if err := json.Unmarshal(plan.DiscussionSnapshot, &snapshot); err == nil {
			result.Summary = strings.TrimSpace(snapshot.Summary)
			result.Consensus = normalizeStringList(snapshot.Consensus)
		}
	}
	if result.Summary == "" {
		if len(result.Consensus) > 0 {
			result.Summary = strings.Join(result.Consensus, "\n")
		} else {
			result.Summary = result.Reasoning
		}
	}
	// F32: fan out the two independent LLM batches (reasoning+summary
	// at TierStandard, consensus list at TierSimple). Both are pure
	// translations of distinct payloads so they have no ordering
	// dependency. With the locality cache in place subsequent clicks
	// are served from memory and the goroutines exit immediately.
	fields := []string{result.Reasoning, result.Summary}
	var (
		fanOut       sync.WaitGroup
		summaryZh    []string
		summaryEn    []string
		consensusZh  []string
		consensusEn  []string
	)
	fanOut.Add(1)
	go func() {
		defer fanOut.Done()
		summaryZh, summaryEn = translateBilingualList(userID, runtime, "daily_review", fields, llm.TierStandard)
	}()
	if len(result.Consensus) > 0 {
		fanOut.Add(1)
		go func() {
			defer fanOut.Done()
			consensusZh, consensusEn = translateBilingualList(userID, runtime, "daily_review", result.Consensus, llm.TierSimple)
		}()
	}
	fanOut.Wait()

	result.ReasoningZh = localizedSingleValue(summaryZh, 0)
	result.SummaryZh = localizedSingleValue(summaryZh, 1)
	result.ReasoningEn = localizedSingleValue(summaryEn, 0)
	result.SummaryEn = localizedSingleValue(summaryEn, 1)
	if len(consensusZh) > 0 || len(consensusEn) > 0 {
		result.ConsensusZh = normalizeStringList(consensusZh)
		result.ConsensusEn = normalizeStringList(consensusEn)
	}
	if runtime != nil {
		slog.Info("decision trace discussion localization", "reasoningZhSet", result.ReasoningZh != "", "summaryZhSet", result.SummaryZh != "", "reasoningEnSet", result.ReasoningEn != "", "summaryEnSet", result.SummaryEn != "", "consensusCount", len(result.Consensus), "fundId", strings.TrimSpace(plan.FundID), "planId", strings.TrimSpace(plan.ID))
	}
	return result
}

func buildCommitteeMemo(plan *api.Plan, discussion *api.DecisionTraceDiscussion, execution *api.DecisionTraceExecution, research []api.MarketResearch) *api.CommitteeMemo {
	if plan == nil {
		return nil
	}
	consensus := normalizeStringList(discussionConsensusList(discussion))
	summary := strings.TrimSpace(discussionSummaryText(discussion))
	if summary == "" {
		summary = strings.TrimSpace(plan.Reasoning)
	}
	risk := buildCommitteeRiskOpinion(plan.RiskReview)
	memo := &api.CommitteeMemo{
		Title:             "Investment committee memo",
		Summary:           summary,
		MarketBackground:  buildCommitteeMarketBackground(research),
		Participants:      buildCommitteeParticipants(plan, risk),
		AgentViews:        buildCommitteeAgentViews(plan, consensus, research),
		Consensus:         consensus,
		Contentions:       buildCommitteeContentions(plan),
		FinalDecision:     buildCommitteeFinalDecision(plan),
		RiskOpinion:       risk,
		TraderSuggestions: buildCommitteeTraderSuggestions(plan, execution),
		TraceLinks: []api.CommitteeTraceLink{
			{Label: "Workflow", Target: "workflow"},
			{Label: "Discussion", Target: "discussion"},
			{Label: "Risk", Target: "risk"},
			{Label: "Execution", Target: "execution"},
		},
	}
	if len(memo.AgentViews) == 0 && len(memo.Consensus) == 0 && memo.Summary == "" && memo.FinalDecision == nil && memo.RiskOpinion == nil {
		return nil
	}
	return memo
}

func discussionConsensusList(discussion *api.DecisionTraceDiscussion) []string {
	if discussion == nil {
		return nil
	}
	return discussion.Consensus
}

func discussionSummaryText(discussion *api.DecisionTraceDiscussion) string {
	if discussion == nil {
		return ""
	}
	if summary := strings.TrimSpace(discussion.Summary); summary != "" {
		return summary
	}
	if len(discussion.Consensus) > 0 {
		return strings.Join(discussion.Consensus, "\n")
	}
	return strings.TrimSpace(discussion.Reasoning)
}

func buildCommitteeParticipants(plan *api.Plan, risk *api.CommitteeRiskOpinion) []api.CommitteeParticipant {
	if plan == nil {
		return nil
	}
	participants := make([]api.CommitteeParticipant, 0)
	seen := map[string]struct{}{}
	add := func(agentID, role string) {
		agentID = strings.TrimSpace(agentID)
		role = strings.TrimSpace(role)
		if agentID == "" && role == "" {
			return
		}
		key := strings.ToLower(agentID + "::" + role)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		participants = append(participants, api.CommitteeParticipant{AgentID: agentID, Role: role, Name: agentID})
	}
	add(plan.PMAgentID, "portfolio_manager")
	for _, action := range plan.Actions {
		for _, supporter := range action.SupportedBy {
			add(supporter, "supporter")
		}
		for _, opposer := range action.OpposedBy {
			add(opposer, "opposer")
		}
	}
	if risk != nil && strings.TrimSpace(risk.Verdict) != "" {
		add("risk_agent", "risk")
	}
	if len(plan.Actions) > 0 {
		add("trader_agent", "trader")
	}
	return participants
}

func buildCommitteeAgentViews(plan *api.Plan, consensus []string, research []api.MarketResearch) []api.CommitteeAgentView {
	if plan == nil {
		return nil
	}
	viewsByAgent := map[string]*api.CommitteeAgentView{}
	orderedKeys := make([]string, 0)
	ensure := func(agentID, role, stance string) *api.CommitteeAgentView {
		agentID = strings.TrimSpace(agentID)
		if agentID == "" {
			return nil
		}
		key := strings.ToLower(agentID + "::" + role + "::" + stance)
		if existing, ok := viewsByAgent[key]; ok {
			return existing
		}
		view := &api.CommitteeAgentView{AgentID: agentID, Role: role, Stance: stance}
		viewsByAgent[key] = view
		orderedKeys = append(orderedKeys, key)
		return view
	}
	for _, action := range plan.Actions {
		for _, supporter := range action.SupportedBy {
			if view := ensure(supporter, "supporter", "support"); view != nil {
				view.Symbols = append(view.Symbols, action.Symbol)
				if reasoning := strings.TrimSpace(action.Reasoning); reasoning != "" {
					view.Evidence = append(view.Evidence, reasoning)
				}
			}
		}
		for _, opposer := range action.OpposedBy {
			if view := ensure(opposer, "opposer", "oppose"); view != nil {
				view.Symbols = append(view.Symbols, action.Symbol)
				view.Evidence = append(view.Evidence, fmt.Sprintf("对 %s %s 提出反对", strings.ToUpper(strings.TrimSpace(action.Action)), strings.TrimSpace(action.Symbol)))
			}
		}
	}
	if len(research) > 0 {
		view := ensure("research_agent", "research", "inform")
		if view != nil {
			for _, item := range research {
				view.Symbols = append(view.Symbols, item.Instrument.Symbol)
				if summary := strings.TrimSpace(item.Summary); summary != "" {
					view.Evidence = append(view.Evidence, summary)
				}
				view.Evidence = append(view.Evidence, limitStrings(item.Signals, 2)...)
			}
		}
	}
	result := make([]api.CommitteeAgentView, 0, len(orderedKeys))
	for _, key := range orderedKeys {
		view := viewsByAgent[key]
		view.Symbols = uniqueNonEmpty(view.Symbols)
		view.Evidence = limitStrings(uniqueNonEmpty(view.Evidence), 4)
		if len(view.Evidence) > 0 {
			view.Viewpoint = view.Evidence[0]
		}
		result = append(result, *view)
	}
	if len(result) == 0 && len(consensus) > 0 {
		result = append(result, api.CommitteeAgentView{AgentID: "roundtable", Role: "committee", Stance: "consensus", Viewpoint: consensus[0], Evidence: limitStrings(consensus, 4)})
	}
	return result
}

func buildCommitteeContentions(plan *api.Plan) []string {
	if plan == nil {
		return nil
	}
	contentions := make([]string, 0)
	for _, action := range plan.Actions {
		if len(action.OpposedBy) == 0 {
			continue
		}
		contentions = append(contentions, fmt.Sprintf("%s %s has %d opposing participant(s): %s", strings.ToUpper(strings.TrimSpace(action.Action)), strings.TrimSpace(action.Symbol), len(action.OpposedBy), strings.Join(uniqueNonEmpty(action.OpposedBy), ", ")))
	}
	return contentions
}

func buildCommitteeFinalDecision(plan *api.Plan) *api.CommitteeFinalDecision {
	if plan == nil {
		return nil
	}
	actions := make([]string, 0, len(plan.Actions))
	for _, action := range plan.Actions {
		parts := []string{strings.ToUpper(strings.TrimSpace(action.Action)), strings.TrimSpace(action.Symbol)}
		if action.Quantity != nil {
			parts = append(parts, fmt.Sprintf("qty %.4f", *action.Quantity))
		}
		actions = append(actions, strings.Join(uniqueNonEmpty(parts), " "))
	}
	return &api.CommitteeFinalDecision{
		Status:    strings.TrimSpace(plan.Status),
		PM:        strings.TrimSpace(plan.PMAgentID),
		Reasoning: strings.TrimSpace(plan.Reasoning),
		Actions:   actions,
	}
}

func buildCommitteeRiskOpinion(raw json.RawMessage) *api.CommitteeRiskOpinion {
	if !hasDecisionTraceSnapshot(raw) {
		return nil
	}
	var payload decisionTraceRiskReviewPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	summary := strings.TrimSpace(payload.OverallNote)
	if summary == "" {
		summary = strings.TrimSpace(payload.Commentary)
	}
	for _, check := range payload.Checks {
		status := strings.ToLower(strings.TrimSpace(firstNonEmpty(check.Status, check.Result)))
		if status != "fail" && status != "warn" {
			continue
		}
		message := strings.TrimSpace(firstNonEmpty(check.Message, check.Detail, check.Name, check.Rule))
		if message == "" {
			continue
		}
		if status == "fail" {
			payload.Rejections = append(payload.Rejections, message)
		} else {
			payload.Warnings = append(payload.Warnings, message)
		}
	}
	return &api.CommitteeRiskOpinion{
		Verdict:     strings.TrimSpace(payload.Verdict),
		Summary:     summary,
		Warnings:    uniqueNonEmpty(payload.Warnings),
		Rejections:  uniqueNonEmpty(payload.Rejections),
		Suggestions: uniqueNonEmpty(payload.Suggestions),
	}
}

func buildRiskExplanation(raw json.RawMessage) *api.RiskExplanation {
	if !hasDecisionTraceSnapshot(raw) {
		return nil
	}
	var payload decisionTraceRiskReviewPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	result := &api.RiskExplanation{
		Verdict:     strings.TrimSpace(payload.Verdict),
		Summary:     strings.TrimSpace(firstNonEmpty(payload.OverallNote, payload.Commentary)),
		Warnings:    uniqueNonEmpty(payload.Warnings),
		Suggestions: uniqueNonEmpty(payload.Suggestions),
	}
	result.BlockingReasons = uniqueNonEmpty(payload.Rejections)
	for _, check := range payload.Checks {
		converted := buildRiskCheckExplanation(check)
		if converted.RuleCode == "" && converted.Explanation == "" {
			continue
		}
		result.Checks = append(result.Checks, converted)
		if converted.Status == "fail" && converted.Explanation != "" {
			result.BlockingReasons = append(result.BlockingReasons, converted.Explanation)
		}
		if converted.Status == "warn" && converted.Explanation != "" {
			result.Warnings = append(result.Warnings, converted.Explanation)
		}
		if converted.AdjustmentHint != "" {
			result.AdjustmentAdvice = append(result.AdjustmentAdvice, converted.AdjustmentHint)
		}
	}
	result.BlockingReasons = uniqueNonEmpty(result.BlockingReasons)
	result.Warnings = uniqueNonEmpty(result.Warnings)
	result.Suggestions = uniqueNonEmpty(result.Suggestions)
	result.AdjustmentAdvice = uniqueNonEmpty(append(result.AdjustmentAdvice, result.Suggestions...))
	result.Severity = riskExplanationSeverity(result.Verdict, result.Checks)
	if result.Summary == "" {
		result.Summary = defaultRiskSummary(result)
	}
	if result.Verdict == "" && result.Summary == "" && len(result.Checks) == 0 {
		return nil
	}
	return result
}

func buildRiskCheckExplanation(check decisionTraceRiskCheckPayload) api.RiskCheckExplanation {
	ruleCode := strings.TrimSpace(firstNonEmpty(check.Rule, check.Name))
	status := strings.ToLower(strings.TrimSpace(firstNonEmpty(check.Status, check.Result)))
	if status == "" {
		status = "pass"
	}
	name := riskRuleDisplayName(ruleCode)
	if strings.TrimSpace(check.Name) != "" {
		name = strings.TrimSpace(check.Name)
	}
	explanation := strings.TrimSpace(firstNonEmpty(check.Message, check.Detail))
	if explanation == "" {
		explanation = defaultRiskRuleExplanation(ruleCode, status)
	}
	return api.RiskCheckExplanation{
		RuleCode:       ruleCode,
		RuleName:       name,
		Status:         status,
		Severity:       riskCheckSeverity(status),
		Current:        check.Current,
		Threshold:      check.Threshold,
		Explanation:    explanation,
		UserImpact:     riskRuleUserImpact(ruleCode, status),
		AdjustmentHint: riskRuleAdjustmentHint(ruleCode, status),
	}
}

func riskExplanationSeverity(verdict string, checks []api.RiskCheckExplanation) string {
	normalized := strings.ToLower(strings.TrimSpace(verdict))
	if normalized == "rejected" || normalized == "fail" {
		return "block"
	}
	severity := "pass"
	for _, check := range checks {
		switch check.Severity {
		case "block":
			return "block"
		case "warning":
			severity = "warning"
		}
	}
	if normalized == "approved_with_warnings" || normalized == "warning" || normalized == "warn" {
		return "warning"
	}
	return severity
}

func riskCheckSeverity(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "fail", "failed", "reject", "rejected":
		return "block"
	case "warn", "warning", "approved_with_warnings":
		return "warning"
	default:
		return "pass"
	}
}

func riskRuleDisplayName(ruleCode string) string {
	switch strings.ToLower(strings.TrimSpace(ruleCode)) {
	case "single_position_limit":
		return "Single position limit"
	case "total_position_limit":
		return "Total exposure limit"
	case "daily_loss_warning":
		return "Daily loss warning"
	case "max_drawdown_warning":
		return "Max drawdown warning"
	case "circuit_breaker":
		return "Circuit breaker"
	case "sector_concentration":
		return "Sector concentration"
	case "liquidity_check":
		return "Liquidity check"
	case "hard_daily_loss_limit":
		return "Hard daily loss limit"
	case "hard_single_position_limit":
		return "Hard single position limit"
	case "hard_total_exposure_limit":
		return "Hard total exposure limit"
	case "hard_order_size_limit":
		return "Hard order size limit"
	case "hard_trade_count_limit":
		return "Hard trade count limit"
	default:
		return strings.TrimSpace(toTitleFromSnake(ruleCode))
	}
}

func toTitleFromSnake(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "Risk rule"
	}
	words := strings.Fields(strings.ReplaceAll(strings.ReplaceAll(trimmed, "_", " "), "-", " "))
	for i := range words {
		if words[i] == "" {
			continue
		}
		words[i] = strings.ToUpper(words[i][:1]) + strings.ToLower(words[i][1:])
	}
	return strings.Join(words, " ")
}

func defaultRiskRuleExplanation(ruleCode, status string) string {
	name := riskRuleDisplayName(ruleCode)
	switch riskCheckSeverity(status) {
	case "block":
		return name + " failed and blocks execution until the plan is adjusted."
	case "warning":
		return name + " needs attention before execution."
	default:
		return name + " passed."
	}
}

func riskRuleUserImpact(ruleCode, status string) string {
	severity := riskCheckSeverity(status)
	if severity == "pass" {
		return "No user action required."
	}
	switch strings.ToLower(strings.TrimSpace(ruleCode)) {
	case "single_position_limit", "hard_single_position_limit":
		return "The proposed order may create an oversized single-name exposure."
	case "total_position_limit", "hard_total_exposure_limit":
		return "The plan may push the portfolio beyond the allowed total exposure."
	case "daily_loss_warning", "hard_daily_loss_limit":
		return "Recent loss pressure reduces the room for new risk."
	case "max_drawdown_warning", "circuit_breaker":
		return "Drawdown controls may pause or reduce trading activity."
	case "sector_concentration":
		return "The plan may concentrate too much risk in one sector or theme."
	case "liquidity_check":
		return "Execution may be difficult without moving the market."
	case "hard_order_size_limit":
		return "The single order size exceeds a deterministic hard-control limit."
	case "hard_trade_count_limit":
		return "The plan exceeds the allowed trade frequency."
	default:
		if severity == "block" {
			return "Execution should be blocked until this risk is resolved."
		}
		return "Review this warning before approving execution."
	}
}

func riskRuleAdjustmentHint(ruleCode, status string) string {
	severity := riskCheckSeverity(status)
	if severity == "pass" {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(ruleCode)) {
	case "single_position_limit", "hard_single_position_limit", "hard_order_size_limit":
		return "Reduce order size, split into staged execution, or choose a smaller target weight."
	case "total_position_limit", "hard_total_exposure_limit":
		return "Lower gross exposure, pair the order with risk-reducing sells, or postpone the add-on trade."
	case "daily_loss_warning", "hard_daily_loss_limit", "max_drawdown_warning", "circuit_breaker":
		return "Delay new risk, reduce position size, or switch the plan to watch/hold until drawdown recovers."
	case "sector_concentration":
		return "Diversify across sectors or reduce the largest sector-linked action."
	case "liquidity_check":
		return "Use smaller child orders, wider execution windows, or avoid illiquid symbols."
	case "hard_trade_count_limit":
		return "Consolidate orders or defer lower-priority trades to the next session."
	default:
		if severity == "block" {
			return "Modify the plan and rerun risk review before approving."
		}
		return "Confirm the warning is acceptable or adjust the plan before approving."
	}
}

func defaultRiskSummary(result *api.RiskExplanation) string {
	if result == nil {
		return ""
	}
	switch result.Severity {
	case "block":
		return "Risk review found blocking issues. The plan should not execute until the listed constraints are resolved."
	case "warning":
		return "Risk review passed with warnings. Review the suggested adjustments before approving execution."
	case "pass":
		return "Risk review did not find blocking issues."
	default:
		return "Risk review is available for this plan."
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func buildCommitteeTraderSuggestions(plan *api.Plan, execution *api.DecisionTraceExecution) []api.CommitteeTraderAction {
	if plan == nil || len(plan.Actions) == 0 {
		return nil
	}
	executionStatusByActionID := map[string]string{}
	if execution != nil {
		for _, item := range execution.ActionExecutions {
			if strings.TrimSpace(item.PlanActionID) != "" {
				executionStatusByActionID[item.PlanActionID] = item.ExecutionStatus
			}
		}
	}
	result := make([]api.CommitteeTraderAction, 0, len(plan.Actions))
	for _, action := range plan.Actions {
		instructionParts := []string{strings.ToUpper(strings.TrimSpace(action.Action)), strings.TrimSpace(action.Symbol)}
		if action.Quantity != nil {
			instructionParts = append(instructionParts, fmt.Sprintf("quantity %.4f", *action.Quantity))
		}
		if action.Price != nil {
			instructionParts = append(instructionParts, fmt.Sprintf("limit/reference price %.4f", *action.Price))
		}
		if action.StopLoss != nil {
			instructionParts = append(instructionParts, fmt.Sprintf("stop loss %.4f", *action.StopLoss))
		}
		if action.TakeProfit != nil {
			instructionParts = append(instructionParts, fmt.Sprintf("take profit %.4f", *action.TakeProfit))
		}
		if status := strings.TrimSpace(executionStatusByActionID[action.ID]); status != "" {
			instructionParts = append(instructionParts, "execution "+status)
		}
		result = append(result, api.CommitteeTraderAction{
			PlanActionID: action.ID,
			Symbol:       action.Symbol,
			Action:       action.Action,
			Instruction:  strings.Join(uniqueNonEmpty(instructionParts), " · "),
			SupportedBy:  uniqueNonEmpty(action.SupportedBy),
			OpposedBy:    uniqueNonEmpty(action.OpposedBy),
		})
	}
	return result
}

func buildCommitteeMarketBackground(research []api.MarketResearch) string {
	if len(research) == 0 {
		return ""
	}
	parts := make([]string, 0, len(research))
	for _, item := range research {
		symbol := strings.TrimSpace(item.Instrument.Symbol)
		if symbol == "" {
			symbol = strings.TrimSpace(item.Instrument.InstrumentKey)
		}
		fragments := make([]string, 0, 3)
		if symbol != "" {
			fragments = append(fragments, symbol)
		}
		if item.Quote != nil && item.Quote.Price > 0 {
			fragments = append(fragments, fmt.Sprintf("price %.4f %s", item.Quote.Price, strings.TrimSpace(item.Quote.QuoteCurrency)))
		}
		if summary := strings.TrimSpace(item.Summary); summary != "" {
			fragments = append(fragments, summary)
		} else if len(item.Signals) > 0 {
			fragments = append(fragments, strings.Join(limitStrings(item.Signals, 2), "; "))
		}
		if len(fragments) > 0 {
			parts = append(parts, strings.Join(fragments, ": "))
		}
	}
	return strings.Join(limitStrings(parts, 4), "\n")
}

func (s *decisionTraceServiceAdapter) buildDecisionTraceResearch(_ string, fund *repository.Fund, actions []repository.PlanAction) []api.MarketResearch {
	if s == nil || s.marketData == nil || fund == nil || len(actions) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(actions))
	profile := decodeFundMarketProfile(fund.Config)
	benchmark, benchmarkOK := benchmarkInstrumentRef(profile)
	instruments := make([]marketdata.InstrumentRef, 0, len(actions))
	for _, action := range actions {
		symbol := strings.TrimSpace(action.Symbol)
		if symbol == "" {
			continue
		}
		normalizedSymbol := strings.ToUpper(symbol)
		if _, ok := seen[normalizedSymbol]; ok {
			continue
		}
		seen[normalizedSymbol] = struct{}{}
		instrument := marketQueryInstrument(fund, normalizedSymbol)
		if strings.TrimSpace(instrument.Symbol) == "" {
			continue
		}
		instruments = append(instruments, instrument)
	}
	if len(instruments) == 0 {
		return nil
	}
	results := make([]api.MarketResearch, len(instruments))
	ready := make([]bool, len(instruments))
	var wg sync.WaitGroup
	limiter := make(chan struct{}, 3)
	benchmarkRef := benchmarkPointer(benchmark, benchmarkOK)
	for idx := range instruments {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			limiter <- struct{}{}
			defer func() { <-limiter }()
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			research, err := s.marketData.GetResearchContext(ctx, instruments[i], benchmarkRef, 3)
			if err != nil {
				return
			}
			converted := convertMarketResearch(research)
			if converted == nil {
				return
			}
			results[i] = *converted
			ready[i] = true
		}(idx)
	}
	wg.Wait()
	result := make([]api.MarketResearch, 0, len(instruments))
	for i := range results {
		if ready[i] {
			result = append(result, results[i])
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func buildDecisionTraceExecution(actions []repository.PlanAction, trades []repository.TradeExecution) *api.DecisionTraceExecution {
	result := &api.DecisionTraceExecution{Status: "pending"}
	if len(actions) == 0 && len(trades) == 0 {
		return result
	}
	tradesByAction := make(map[string][]api.Trade)
	if len(trades) > 0 {
		result.Trades = make([]api.Trade, 0, len(trades))
		for i := range trades {
			converted := convertTrade(&trades[i])
			result.Trades = append(result.Trades, converted)
			if trades[i].PlanActionID.Valid && strings.TrimSpace(trades[i].PlanActionID.String) != "" {
				key := strings.TrimSpace(trades[i].PlanActionID.String)
				tradesByAction[key] = append(tradesByAction[key], converted)
			}
		}
		result.Status = "executed"
	}
	if len(actions) > 0 {
		result.ActionExecutions = make([]api.DecisionTraceActionExecution, 0, len(actions))
		for i := range actions {
			action := actions[i]
			status := strings.TrimSpace(action.ExecutionStatus)
			if status == "" {
				status = "pending"
				if len(tradesByAction[action.ID]) > 0 {
					status = tradesByAction[action.ID][0].Status
				}
			}
			result.ActionExecutions = append(result.ActionExecutions, api.DecisionTraceActionExecution{
				PlanActionID:    action.ID,
				Symbol:          action.Symbol,
				Action:          action.Action,
				ExecutionStatus: status,
				Trades:          tradesByAction[action.ID],
			})
		}
	}
	return result
}

func buildDecisionTraceReview(memories []repository.Memory) *api.DecisionTraceReview {
	result := &api.DecisionTraceReview{}
	if len(memories) == 0 {
		return result
	}
	result.Entries = make([]api.MemoryEntry, 0, len(memories))
	for i := range memories {
		result.Entries = append(result.Entries, convertMemoryEntry(&memories[i]))
	}
	return result
}

func hasDecisionTraceSnapshot(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null" && trimmed != "{}"
}

func buildDecisionTraceSnapshot(roundtable *workflow.RoundtableResult) json.RawMessage {
	if roundtable == nil {
		return json.RawMessage(`{}`)
	}
	snapshot := decisionTraceSnapshot{
		RoundtableID: strings.TrimSpace(roundtable.ID),
		Rounds:       roundtable.Rounds,
		Consensus:    normalizedStringSlice(roundtable.Consensus),
		Summary:      strings.TrimSpace(strings.Join(roundtable.Consensus, "\n")),
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return payload
}

func convertTrade(trade *repository.TradeExecution) api.Trade {
	result := api.Trade{}
	if trade == nil {
		return result
	}
	result.ID = trade.ID
	result.FundID = trade.FundID
	result.PlanID = trade.PlanID.String
	result.PlanActionID = trade.PlanActionID.String
	result.InstrumentKey = trade.InstrumentKey
	result.Symbol = trade.Symbol
	result.Market = trade.Market.String
	result.Exchange = trade.Exchange.String
	result.AssetClass = trade.AssetClass.String
	result.InstrumentType = trade.InstrumentType.String
	result.Side = trade.Side
	result.PositionSide = trade.PositionSide.String
	result.OpenClose = trade.OpenClose.String
	result.OrderType = trade.OrderType
	result.Quantity = trade.Quantity
	result.Price = trade.Price.Float64
	result.Amount = trade.Amount.Float64
	result.FilledQty = trade.FilledQty
	result.FilledPrice = trade.FilledPrice.Float64
	result.FeeCommission = trade.FeeCommission
	result.FeeStampTax = trade.FeeStampTax
	result.FeeTransfer = trade.FeeTransfer
	result.TradingMode = trade.TradingMode
	result.BrokerOrderID = trade.BrokerOrderID.String
	result.MCPServerID = trade.MCPServerID.String
	result.Status = trade.Status
	result.QuoteCurrency = trade.QuoteCurrency.String
	result.SettlementCurrency = trade.SettlementCurrency.String
	result.MarginMode = trade.MarginMode.String
	result.CreatedAt = trade.CreatedAt
	if trade.ExecutedAt.Valid {
		result.ExecutedAt = trade.ExecutedAt.Time
	}
	if !trade.Amount.Valid {
		result.Amount = trade.Quantity * trade.Price.Float64
	}
	if !trade.Price.Valid && trade.FilledPrice.Valid {
		result.Price = trade.FilledPrice.Float64
	}
	if trade.Leverage.Valid {
		value := trade.Leverage.Float64
		result.Leverage = &value
	}
	if trade.ContractMultiplier.Valid {
		value := trade.ContractMultiplier.Float64
		result.ContractMultiplier = &value
	}
	if trade.ExpiryDate.Valid {
		result.ExpiryDate = trade.ExpiryDate.Time.Format("2006-01-02")
	}
	if trade.ReduceOnly.Valid {
		value := trade.ReduceOnly.Bool
		result.ReduceOnly = &value
	}
	if trade.SlippagePct.Valid {
		value := trade.SlippagePct.Float64
		result.SlippagePct = &value
	}
	// T1-step2: surface the execution-strategy intent and the
	// child-of-parent link to the UI. Empty for legacy rows
	// (strategy column was added in migration 088); the json
	// `omitempty` tag drops them from the wire payload so the
	// API contract stays additive.
	if trade.Strategy.Valid {
		result.Strategy = trade.Strategy.String
	}
	if trade.StrategyParentTradeID.Valid {
		result.StrategyParentTradeID = trade.StrategyParentTradeID.String
	}
	if !trade.PlanID.Valid {
		result.PlanID = ""
	}
	if !trade.PlanActionID.Valid {
		result.PlanActionID = ""
	}
	return result
}

func (s *tradeServiceAdapter) ListTrades(userID, fundID string, from, to *time.Time, limit, offset int, excludeChildSlices bool) ([]api.Trade, error) {
	if _, err := authorizeFundAccess(context.Background(), s.fundRepo, s.companyRepo, userID, fundID); err != nil {
		return nil, err
	}
	start := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	if from != nil {
		start = from.UTC()
	}
	end := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	if to != nil {
		end = to.UTC()
	}

	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}
	// Forward the UI's hide-children intent to the repo so the
	// underlying SQL applies the strategy_parent_trade_id IS NULL
	// filter. We always route through the *Opts variant; the
	// zero-value opts case is byte-identical to the legacy
	// ListByFundPage path so nothing observable changes for
	// callers that pass false.
	trades, err := s.tradeRepo.ListByFundPageOpts(context.Background(), fundID, start, end, limit, offset,
		repository.TradeListOpts{ExcludeChildSlices: excludeChildSlices})
	if err != nil {
		return nil, mapRepositoryError(err)
	}

	result := make([]api.Trade, 0, len(trades))
	for i := range trades {
		result = append(result, convertTrade(&trades[i]))
	}
	return result, nil
}

// ListTradeChildren returns the per-slice children of a TWAP /
// VWAP / iceberg / POV parent trade. Authz uses the same
// fund-access check as ListTrades. The fund_id on each returned
// row is verified to match `fundID` so a malicious / stale
// `tradeID` from another fund can't be enumerated via this
// endpoint (defense-in-depth — the underlying SQL already
// excludes other funds because children always carry the same
// fund_id as the parent that the splitter inserted, but we
// re-check here to be explicit).
func (s *tradeServiceAdapter) ListTradeChildren(userID, fundID, parentTradeID string) ([]api.Trade, error) {
	if _, err := authorizeFundAccess(context.Background(), s.fundRepo, s.companyRepo, userID, fundID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(parentTradeID) == "" {
		return []api.Trade{}, nil
	}
	rows, err := s.tradeRepo.ListChildrenByStrategyParent(context.Background(), parentTradeID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	result := make([]api.Trade, 0, len(rows))
	for i := range rows {
		// Cross-fund guard: skip any row that belongs to a
		// different fund. In production this never fires (the
		// splitter copies fund_id from the parent) but the
		// check makes the endpoint safe under repo-level
		// corruption / future refactors.
		if rows[i].FundID != fundID {
			continue
		}
		result = append(result, convertTrade(&rows[i]))
	}
	return result, nil
}

func (s *tradeServiceAdapter) GetPortfolio(userID, fundID string) ([]api.Position, error) {
	ctx := context.Background()
	if _, err := authorizeFundAccess(ctx, s.fundRepo, s.companyRepo, userID, fundID); err != nil {
		return nil, err
	}
	positions, err := s.positionRepo.ListByFund(ctx, fundID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}

	// PR-2 overlay: batch-fetch fresh quotes for all positions and merge
	// them into the response so the UI sees real-time CurrentPrice /
	// MarketValue / UnrealizedPnL on every fund-open. Quotes that fail
	// fall back to the persisted DB value with IsStale=true. The marketData
	// pointer is nil-safe — tests / dev builds without it keep the legacy
	// "DB-only" behaviour.
	quotes := s.fetchPortfolioQuotes(ctx, positions)

	result := make([]api.Position, 0, len(positions))
	for i := range positions {
		position := positions[i]
		converted := api.Position{
			InstrumentKey:      position.InstrumentKey,
			Symbol:             position.Symbol,
			Market:             position.Market.String,
			Exchange:           position.Exchange.String,
			AssetClass:         position.AssetClass.String,
			InstrumentType:     position.InstrumentType.String,
			PositionSide:       position.PositionSide.String,
			QuoteCurrency:      position.QuoteCurrency.String,
			SettlementCurrency: position.SettlementCurrency.String,
			MarginMode:         position.MarginMode.String,
			Quantity:           position.Quantity,
			AvailableQty:       position.AvailableQty,
			CostPrice:          position.CostPrice,
			CurrentPrice:       position.CurrentPrice,
			MarketValue:        position.MarketValue,
			Weight:             position.Weight,
		}
		if position.Leverage.Valid {
			value := position.Leverage.Float64
			converted.Leverage = &value
		}
		if position.ContractMultiplier.Valid {
			value := position.ContractMultiplier.Float64
			converted.ContractMultiplier = &value
		}
		if position.ExpiryDate.Valid {
			converted.ExpiryDate = position.ExpiryDate.Time.Format("2006-01-02")
		}
		if position.UnrealizedPnL.Valid {
			value := position.UnrealizedPnL.Float64
			converted.UnrealizedPnL = &value
		}
		if position.MarginUsed.Valid {
			value := position.MarginUsed.Float64
			converted.MarginUsed = &value
		}
		applyPositionLiveOverlay(&converted, &position, quotes)
		result = append(result, converted)
	}
	return result, nil
}

// GetPortfolioQuotes returns the latest live quote snapshot for each
// instrument currently held by the fund. Used by the SSE quote stream
// handler — it intentionally returns a thin []api.PortfolioQuote (not
// the full Position) so the per-frame payload stays compact.
//
// Authorisation matches GetPortfolio. When marketData is unwired the
// method still returns one row per held instrument, but with PriceSource
// empty and IsStale=true so the frontend can render the freshness badge
// instead of silently lying.
func (s *tradeServiceAdapter) GetPortfolioQuotes(userID, fundID string) ([]api.PortfolioQuote, error) {
	ctx := context.Background()
	if _, err := authorizeFundAccess(ctx, s.fundRepo, s.companyRepo, userID, fundID); err != nil {
		return nil, err
	}
	positions, err := s.positionRepo.ListByFund(ctx, fundID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if len(positions) == 0 {
		return []api.PortfolioQuote{}, nil
	}

	quotes := s.fetchPortfolioQuotes(ctx, positions)

	result := make([]api.PortfolioQuote, 0, len(positions))
	for i := range positions {
		position := positions[i]
		ref := positionInstrumentRef(&position)
		row := api.PortfolioQuote{
			InstrumentKey: position.InstrumentKey,
			Symbol:        position.Symbol,
			Market:        position.Market.String,
			AssetClass:    position.AssetClass.String,
			CurrentPrice:  position.CurrentPrice,
			MarketValue:   position.MarketValue,
			IsStale:       true, // default until we prove freshness
		}
		if position.UpdatedAt.IsZero() {
			row.PriceAsOf = ""
		} else {
			row.PriceAsOf = position.UpdatedAt.UTC().Format(time.RFC3339)
		}
		if quote, ok := quotes[ref.CacheKey()]; ok && quote != nil && quote.Price > 0 {
			row.CurrentPrice = quote.Price
			row.MarketValue = quote.Price * position.Quantity
			if !quote.AsOf.IsZero() {
				row.PriceAsOf = quote.AsOf.UTC().Format(time.RFC3339)
			}
			row.PriceSource = quote.Source
			row.IsStale = quote.IsStale
		}
		result = append(result, row)
	}
	return result, nil
}

// fetchPortfolioQuotes batches a GetQuotes call for the unique instrument
// keys in `positions`. Returns a map keyed by InstrumentRef.CacheKey() so
// applyPositionLiveOverlay can pair positions with their live quote in O(1).
// Returns nil when marketData is unwired or there are no positions.
func (s *tradeServiceAdapter) fetchPortfolioQuotes(ctx context.Context, positions []repository.HoldingPosition) map[string]*marketdata.QuoteSnapshot {
	if s == nil || s.marketData == nil || len(positions) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(positions))
	refs := make([]marketdata.InstrumentRef, 0, len(positions))
	for i := range positions {
		ref := positionInstrumentRef(&positions[i])
		key := ref.CacheKey()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		refs = append(refs, ref)
	}
	if len(refs) == 0 {
		return nil
	}
	// Cap the overlay's wall-clock budget so a slow upstream can never
	// make the portfolio request hang for the user. The per-provider
	// rate limiter + singleflight introduced in PR-1 keep this safe even
	// under heavy parallelism.
	overlayCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	bySymbol := s.marketData.GetQuotes(overlayCtx, refs)
	if len(bySymbol) == 0 {
		return nil
	}
	out := make(map[string]*marketdata.QuoteSnapshot, len(bySymbol))
	for _, ref := range refs {
		if q := bySymbol[ref.NormalizedSymbol()]; q != nil {
			out[ref.CacheKey()] = q
		}
	}
	return out
}

// positionInstrumentRef adapts a repository.HoldingPosition into the
// marketdata InstrumentRef shape used by GetQuotes. We deliberately keep
// this in the adapter (instead of repository) so the repository stays
// decoupled from the marketdata package's types.
func positionInstrumentRef(position *repository.HoldingPosition) marketdata.InstrumentRef {
	if position == nil {
		return marketdata.InstrumentRef{}
	}
	contractMultiplier := 0.0
	if position.ContractMultiplier.Valid {
		contractMultiplier = position.ContractMultiplier.Float64
	}
	return marketdata.InstrumentRef{
		InstrumentKey:      position.InstrumentKey,
		Symbol:             position.Symbol,
		Market:             position.Market.String,
		Exchange:           position.Exchange.String,
		AssetClass:         position.AssetClass.String,
		InstrumentType:     position.InstrumentType.String,
		QuoteCurrency:      position.QuoteCurrency.String,
		SettlementCurrency: position.SettlementCurrency.String,
		ContractMultiplier: contractMultiplier,
		ExpiryDate: func() string {
			if position.ExpiryDate.Valid {
				return position.ExpiryDate.Time.Format("2006-01-02")
			}
			return ""
		}(),
	}
}

// applyPositionLiveOverlay merges a fresh quote into the API position. When
// no quote is available it stamps the DB-cached value with IsStale so the
// frontend can render a warning badge, and (when known) also includes the
// position's last refresh time for the "X minutes ago" indicator.
func applyPositionLiveOverlay(out *api.Position, position *repository.HoldingPosition, quotes map[string]*marketdata.QuoteSnapshot) {
	if out == nil || position == nil {
		return
	}
	ref := positionInstrumentRef(position)
	quote := quotes[ref.CacheKey()]
	if quote != nil && quote.Price > 0 {
		out.CurrentPrice = quote.Price
		out.MarketValue = quote.Price * position.Quantity
		// Recompute unrealised P&L on the live price so the dashboard
		// shows a consistent picture (price + P&L derived from the same
		// snapshot). We only override when the cost basis is positive
		// to avoid synthesising bogus numbers for zero-qty rows.
		if position.Quantity != 0 && position.CostPrice > 0 {
			unrealized := (quote.Price - position.CostPrice) * position.Quantity
			out.UnrealizedPnL = &unrealized
		}
		if !quote.AsOf.IsZero() {
			out.PriceAsOf = quote.AsOf.UTC().Format(time.RFC3339)
		}
		out.PriceSource = quote.Source
		out.IsStale = quote.IsStale
		return
	}
	// No live quote: fall back to whatever we last persisted on the
	// position row. Without a fresh sample we can't compute a precise
	// staleness signal (the position table doesn't record per-quote
	// timestamps), so we surface IsStale=true conservatively when the
	// market-data service is wired (operators can verify the upstream
	// state via the provider-health endpoint).
	if position.UpdatedAt.IsZero() {
		out.PriceAsOf = ""
	} else {
		out.PriceAsOf = position.UpdatedAt.UTC().Format(time.RFC3339)
	}
	out.IsStale = true
}

func (s *tradeServiceAdapter) GetNAVHistory(userID, fundID string, from, to *time.Time) ([]api.NAVPoint, error) {
	if _, err := authorizeFundAccess(context.Background(), s.fundRepo, s.companyRepo, userID, fundID); err != nil {
		return nil, err
	}
	start := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	if from != nil {
		start = from.UTC()
	}
	end := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	if to != nil {
		end = to.UTC()
	}

	snapshots, err := s.navRepo.ListByFund(context.Background(), fundID, start, end)
	if err != nil {
		return nil, mapRepositoryError(err)
	}

	result := make([]api.NAVPoint, 0, len(snapshots))
	for i := range snapshots {
		snapshot := snapshots[i]
		result = append(result, api.NAVPoint{
			Date:          snapshot.TradingDate.Format("2006-01-02"),
			NAV:           snapshot.NAV,
			TotalAssets:   snapshot.TotalAssets,
			AvailableCash: snapshot.AvailableCash,
		})
	}
	return result, nil
}

func (s *tradeServiceAdapter) GetPnLAttribution(userID, fundID string, from, to *time.Time) (*api.PnLAttribution, error) {
	ctx := context.Background()
	fund, err := authorizeFundAccess(ctx, s.fundRepo, s.companyRepo, userID, fundID)
	if err != nil {
		return nil, err
	}
	start := time.Now().UTC().AddDate(0, 0, -30)
	if from != nil {
		start = from.UTC()
	}
	end := time.Now().UTC().Add(time.Second)
	if to != nil {
		end = to.UTC()
	}

	navs, err := s.navRepo.ListByFund(ctx, fundID, start, end)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	trades, err := s.tradeRepo.ListByFund(ctx, fundID, start, end, 1000)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	positions, err := s.positionRepo.ListByFund(ctx, fundID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}

	// If the requested range has no NAV snapshots, there is literally
	// no data point in [start, end] from which to compute a PnL
	// number. Returning fund.InitialCapital / fund.TotalAssets here
	// would silently substitute the fund's LIFETIME PnL for the
	// requested window — a confusing footgun that surfaces as the
	// chart showing a non-zero figure for ranges where the fund
	// didn't even exist (P2 sweep Test 8: from=2030-01-01 returned
	// total lifetime PnL instead of zero).
	//
	// Return an empty/zero payload so the UI can plainly render
	// "no data" for the chosen window. The trades and positions
	// arrays remain attached for callers that want to inspect what
	// happened to the (out-of-window) lifetime state, but the
	// aggregate numbers stay zero.
	if len(navs) == 0 {
		return &api.PnLAttribution{
			FundID:          fundID,
			From:            start.Format(time.RFC3339),
			To:              end.Format(time.RFC3339),
			BeginningAssets: 0,
			EndingAssets:    0,
			TotalPnL:        0,
			ReturnPct:       0,
			BySymbol:        []api.PnLAttributionBucket{},
			ByAssetClass:    []api.PnLAttributionBucket{},
			Daily:           []api.PnLAttributionDailyPoint{},
		}, nil
	}

	beginningAssets := navs[0].TotalAssets
	endingAssets := navs[len(navs)-1].TotalAssets
	if beginningAssets <= 0 {
		beginningAssets = fund.InitialCapital
	}
	totalPnL := endingAssets - beginningAssets
	result := &api.PnLAttribution{
		FundID:          fundID,
		From:            start.Format(time.RFC3339),
		To:              end.Format(time.RFC3339),
		BeginningAssets: beginningAssets,
		EndingAssets:    endingAssets,
		TotalPnL:        totalPnL,
		ReturnPct:       safeRatio(totalPnL, beginningAssets) * 100,
		BySymbol:        []api.PnLAttributionBucket{},
		ByAssetClass:    []api.PnLAttributionBucket{},
		Daily:           []api.PnLAttributionDailyPoint{},
	}
	for i, nav := range navs {
		prevAssets := beginningAssets
		if i > 0 {
			prevAssets = navs[i-1].TotalAssets
		}
		result.Daily = append(result.Daily, api.PnLAttributionDailyPoint{
			Date:        nav.TradingDate.Format("2006-01-02"),
			DailyReturn: nav.DailyReturn,
			TotalAssets: nav.TotalAssets,
			DailyPnL:    nav.TotalAssets - prevAssets,
		})
	}

	bySymbol := map[string]*api.PnLAttributionBucket{}
	byAsset := map[string]*api.PnLAttributionBucket{}
	getBucket := func(container map[string]*api.PnLAttributionBucket, key, label string) *api.PnLAttributionBucket {
		key = strings.TrimSpace(key)
		if key == "" {
			key = "unknown"
		}
		bucket := container[key]
		if bucket == nil {
			bucket = &api.PnLAttributionBucket{Key: key, Label: label}
			container[key] = bucket
		}
		return bucket
	}
	// Trade-execution loop covers fee drag and trade counting only.
	// Realised P&L is intentionally NOT summed here: a sell trade's
	// notional is the gross sale proceeds (e.g. 393 × ¥255), NOT the
	// profit (¥491). The earlier version did `if sell: realized +=
	// amount`, which conflated the two and inflated realised by a
	// factor of "cost basis ÷ profit" — that's why a profitable
	// ¥491 close came out as ¥100,482 in the dashboard. The correct
	// realised number comes from closed_lots.realized_pnl (computed
	// at close time by the lotledger as (exit-entry) × qty − fees).
	for _, trade := range trades {
		price := trade.FilledPrice.Float64
		if !trade.FilledPrice.Valid || price <= 0 {
			price = trade.Price.Float64
		}
		amount := trade.Amount.Float64
		if !trade.Amount.Valid || amount == 0 {
			amount = trade.FilledQty * price
			if amount == 0 {
				amount = trade.Quantity * price
			}
		}
		_ = amount // notional is no longer used for realised; kept for future per-trade exposure
		fees := trade.FeeCommission + trade.FeeStampTax + trade.FeeTransfer
		symbolBucket := getBucket(bySymbol, firstNonEmptyText(trade.InstrumentKey, trade.Symbol), trade.Symbol)
		assetBucket := getBucket(byAsset, trade.AssetClass.String, humanizeWorkflowLabel(trade.AssetClass.String))
		for _, bucket := range []*api.PnLAttributionBucket{symbolBucket, assetBucket} {
			bucket.FeeDrag += fees
			bucket.TradeCount++
		}
		result.FeeDrag += fees
	}
	// Realised P&L from closed_lots.realized_pnl. closed_lots.exit_fees
	// is already netted into realized_pnl (lotledger applies it at
	// close time), so we don't double-count fees here. nil lotRepo is
	// fine for test stubs that don't wire it; realised stays 0.
	if s.lotRepo != nil {
		closedLots, err := s.lotRepo.ListClosedBetween(ctx, fundID, start, end)
		if err != nil {
			return nil, mapRepositoryError(err)
		}
		for _, lot := range closedLots {
			if lot == nil {
				continue
			}
			symbolBucket := getBucket(bySymbol, firstNonEmptyText(lot.InstrumentKey, lot.Symbol), lot.Symbol)
			assetBucket := getBucket(byAsset, lot.AssetClass.String, humanizeWorkflowLabel(lot.AssetClass.String))
			for _, bucket := range []*api.PnLAttributionBucket{symbolBucket, assetBucket} {
				bucket.RealizedPnL += lot.RealizedPnL
			}
			result.RealizedPnL += lot.RealizedPnL
		}
	}
	for _, position := range positions {
		unrealized := 0.0
		if position.UnrealizedPnL.Valid {
			unrealized = position.UnrealizedPnL.Float64
		} else {
			multiplier := 1.0
			if position.ContractMultiplier.Valid && position.ContractMultiplier.Float64 > 0 {
				multiplier = position.ContractMultiplier.Float64
			}
			delta := position.CurrentPrice - position.CostPrice
			if strings.EqualFold(position.PositionSide.String, "short") {
				delta = position.CostPrice - position.CurrentPrice
			}
			unrealized = delta * position.Quantity * multiplier
		}
		symbolBucket := getBucket(bySymbol, firstNonEmptyText(position.InstrumentKey, position.Symbol), position.Symbol)
		assetBucket := getBucket(byAsset, position.AssetClass.String, humanizeWorkflowLabel(position.AssetClass.String))
		for _, bucket := range []*api.PnLAttributionBucket{symbolBucket, assetBucket} {
			bucket.UnrealizedPnL += unrealized
			bucket.Exposure += position.MarketValue
		}
		result.UnrealizedPnL += unrealized
	}
	result.BySymbol = flattenAttributionBuckets(bySymbol, endingAssets)
	result.ByAssetClass = flattenAttributionBuckets(byAsset, endingAssets)
	safeAuditLogAccess(ctx, s.auditLogger, userID, "read", "pnl_attribution", fund.ID, map[string]any{
		"fundId":        fund.ID,
		"from":          result.From,
		"to":            result.To,
		"tradeCount":    len(trades),
		"positionCount": len(positions),
		"navPointCount": len(navs),
	})
	return result, nil
}

// GetTodayPnL implements api.TradeService.GetTodayPnL. See the
// TodayPnL DTO and the route comment in fund_handler.go for the
// rationale.
func (s *tradeServiceAdapter) GetTodayPnL(userID, fundID string) (*api.TodayPnL, error) {
	ctx := context.Background()
	fund, err := authorizeFundAccess(ctx, s.fundRepo, s.companyRepo, userID, fundID)
	if err != nil {
		return nil, err
	}
	// Resolve today's local-trading-day start. We don't yet have a
	// market-calendar reference at the trade-service level (calendar
	// lives in workflowServiceAdapter), so we derive "today" from
	// the fund's configured TimeZone. Falls back to UTC when the
	// fund has no TZ set — fine for crypto / global desks.
	profile := decodeFundMarketProfile(fund.Config)
	tz := strings.TrimSpace(profile.TimeZone)
	loc, _ := time.LoadLocation(tz)
	if loc == nil {
		loc = time.UTC
	}
	nowLocal := time.Now().In(loc)
	todayLocalStart := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, loc)
	todayUTC := todayLocalStart.UTC()
	nowUTC := time.Now().UTC()

	// 1. Realised today (already net of entry+exit fees).
	realisedToday := 0.0
	if s.lotRepo != nil {
		lots, err := s.lotRepo.ListClosedBetween(ctx, fundID, todayUTC, nowUTC.Add(time.Second))
		if err != nil {
			return nil, mapRepositoryError(err)
		}
		for _, lot := range lots {
			if lot == nil {
				continue
			}
			realisedToday += lot.RealizedPnL
		}
	}

	// 2. Current unrealised across live position book.
	positions, err := s.positionRepo.ListByFund(ctx, fundID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	currentUnrealised := 0.0
	for _, position := range positions {
		if position.UnrealizedPnL.Valid {
			currentUnrealised += position.UnrealizedPnL.Float64
			continue
		}
		multiplier := 1.0
		if position.ContractMultiplier.Valid && position.ContractMultiplier.Float64 > 0 {
			multiplier = position.ContractMultiplier.Float64
		}
		delta := position.CurrentPrice - position.CostPrice
		if strings.EqualFold(position.PositionSide.String, "short") {
			delta = position.CostPrice - position.CurrentPrice
		}
		currentUnrealised += delta * position.Quantity * multiplier
	}

	// 3. Prior-close unrealised — pull the latest NAV snapshot
	// strictly before todayUTC and reconstruct its position
	// book's unrealised P&L from positions_snapshot JSON. We do
	// this through a single one-row query rather than calling
	// ListByFund with an interval, because we only need the
	// freshest pre-today row.
	priorCloseUnrealised := 0.0
	priorCloseDate := ""
	baselineFresh := false
	priorClose, err := s.priorCloseNavBefore(ctx, fundID, todayUTC)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if priorClose != nil {
		priorCloseDate = priorClose.TradingDate.Format("2006-01-02")
		// positions_snapshot is an array of position-shaped JSON
		// objects with an "unrealizedPnL.Float64" field (see the
		// 2026-05-20 row dumped in PR review). We accumulate it as
		// the baseline; missing field means "treat as zero" for
		// that row.
		var raw []json.RawMessage
		if len(priorClose.PositionsSnapshot) > 0 {
			_ = json.Unmarshal(priorClose.PositionsSnapshot, &raw)
		}
		for _, item := range raw {
			var parsed struct {
				UnrealizedPnL struct {
					Float64 float64 `json:"Float64"`
					Valid   bool    `json:"Valid"`
				} `json:"unrealizedPnL"`
			}
			if err := json.Unmarshal(item, &parsed); err == nil && parsed.UnrealizedPnL.Valid {
				priorCloseUnrealised += parsed.UnrealizedPnL.Float64
			}
		}
		// "Yesterday in local TZ" = the trading day immediately
		// preceding today. We allow either calendar day (handles
		// most cases) or a 1-trading-day gap when settle missed
		// a weekend/holiday — for the UI's purposes anything
		// older than that is "not fresh" and we surface a date
		// label so the user knows they're looking at a multi-day
		// delta.
		expectedYesterday := todayLocalStart.Add(-24 * time.Hour).Format("2006-01-02")
		baselineFresh = priorCloseDate == expectedYesterday
	}

	todayPnL := realisedToday + (currentUnrealised - priorCloseUnrealised)

	safeAuditLogAccess(ctx, s.auditLogger, userID, "read", "today_pnl", fund.ID, map[string]any{
		"fundId":         fund.ID,
		"realised":       realisedToday,
		"priorCloseDate": priorCloseDate,
		"baselineFresh":  baselineFresh,
	})

	return &api.TodayPnL{
		FundID:                  fund.ID,
		RealisedPnL:             realisedToday,
		CurrentUnrealisedPnL:    currentUnrealised,
		PriorCloseUnrealisedPnL: priorCloseUnrealised,
		PriorCloseDate:          priorCloseDate,
		BaselineFresh:           baselineFresh,
		TodayPnL:                todayPnL,
		AsOf:                    nowUTC,
	}, nil
}

// priorCloseNavBefore returns the latest NAV snapshot strictly
// before `before` (typically today_local_start_utc). nil + nil when
// no such snapshot exists; callers should fall back to "no baseline"
// semantics.
func (s *tradeServiceAdapter) priorCloseNavBefore(ctx context.Context, fundID string, before time.Time) (*repository.NavSnapshot, error) {
	if s.navRepo == nil {
		return nil, nil
	}
	// Look back up to one year so weekends, holidays, and short
	// missed-settle gaps don't disqualify the baseline. Anything
	// beyond a year is almost certainly a brand-new fund whose
	// initial_capital is a more sensible baseline anyway, which the
	// caller falls back to when priorClose is nil.
	from := before.AddDate(-1, 0, 0)
	snapshots, err := s.navRepo.ListByFund(ctx, fundID, from, before)
	if err != nil {
		return nil, err
	}
	if len(snapshots) == 0 {
		return nil, nil
	}
	// ListByFund returns oldest → newest; the last entry is the
	// most recent strictly-before-today snapshot.
	out := snapshots[len(snapshots)-1]
	return &out, nil
}

func flattenAttributionBuckets(values map[string]*api.PnLAttributionBucket, endingAssets float64) []api.PnLAttributionBucket {
	result := make([]api.PnLAttributionBucket, 0, len(values))
	for _, bucket := range values {
		// closed_lots.realized_pnl is already net of entry + exit fees
		// (lotledger.buildClosedLot subtracts both attributed fee
		// shares). Subtracting bucket.FeeDrag — which sums all
		// trade_executions fees for the window — would double-count
		// the fees for any sell that has a matching closed lot. We
		// keep FeeDrag in the response as an informational signal
		// ("fees paid this window") but no longer feed it into
		// TotalPnL.
		bucket.TotalPnL = bucket.RealizedPnL + bucket.UnrealizedPnL
		bucket.Weight = safeRatio(bucket.Exposure, endingAssets) * 100
		result = append(result, *bucket)
	}
	sort.Slice(result, func(i, j int) bool {
		return math.Abs(result[i].TotalPnL) > math.Abs(result[j].TotalPnL)
	})
	return result
}

func safeRatio(numerator, denominator float64) float64 {
	if denominator == 0 || math.IsNaN(denominator) || math.IsInf(denominator, 0) {
		return 0
	}
	return numerator / denominator
}

func firstNonEmptyText(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

const (
	manualWorkflowRestartStaleAfter = 20 * time.Minute
	marketNewsDigestMaxAge          = 7 * 24 * time.Hour
)

func (s *workflowServiceAdapter) startWorkflowForFund(fund *repository.Fund, tradingDate time.Time) (*api.WorkflowStatus, error) {
	return s.startWorkflowForFundWithMode(fund, tradingDate, false)
}

// startWorkflowForFundWithSlot is the entry point used by the
// scheduler when intra-day interval mode is active. A non-zero slotTime
// signals "this is a recurring slot trigger" → we allow re-firing on
// top of a completed daily run AND stamp `started_at = slotTime` so
// the next scheduler tick can dedupe slots via the started_at
// comparison. Passing the zero time falls back to the legacy single
// trigger behaviour.
func (s *workflowServiceAdapter) startWorkflowForFundWithSlot(fund *repository.Fund, tradingDate, slotTime time.Time) (*api.WorkflowStatus, error) {
	if fund == nil {
		return nil, api.ErrBadInput
	}
	tradingDate = normalizeTradingDate(tradingDate)
	if slotTime.IsZero() {
		return s.startWorkflowForFundWithMode(fund, tradingDate, false)
	}

	if runtime := s.peekRuntime(fund.ID, tradingDate); runtime != nil && runtime.orchestrator != nil {
		if state := runtime.orchestrator.State(); state != nil && strings.TrimSpace(state.TradingDate) == tradingDate.Format("2006-01-02") {
			if state.Status == workflow.RunStatusRunning || state.Status == workflow.RunStatusPaused {
				status, err := s.persistRuntimeState(fund.ID, state, tradingDate)
				if err != nil {
					return nil, err
				}
				if status != nil && s.metrics != nil {
					s.metrics.ObserveWorkflow(fund.ID, status.State, status.Step)
				}
				return status, nil
			}
		}
	}

	if s.quotaService != nil {
		if quotaErr := s.quotaService.CheckConcurrentWorkflows(context.Background(), fund.ID); quotaErr != nil {
			if s.metrics != nil {
				s.metrics.ObserveWorkflow(fund.ID, "quota_blocked", "")
			}
			return nil, mapRepositoryError(quotaErr)
		}
	}

	// ClaimManualStart allows re-firing on `completed` rows, which is
	// exactly the semantics we need for intra-day slots — the daily
	// run row gets reset to "running" with started_at pinned to this
	// slot's time so dedupe still works on the next tick.
	run, claimed, err := s.workflowRepo.ClaimManualStart(
		context.Background(), fund.ID, tradingDate, slotTime.UTC(), workflow.StepMacroBrief.String(),
	)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if !claimed {
		if s.metrics != nil {
			s.metrics.ObserveWorkflow(fund.ID, run.Status, run.CurrentStep.String)
		}
		return convertWorkflowStatus(run), nil
	}

	s.cancelRuntime(s.takeRuntime(fund.ID, tradingDate))
	runtime := s.getRuntime(fund, tradingDate, time.Now(), true)
	if s.metrics != nil {
		s.metrics.ObserveWorkflow(fund.ID, run.Status, run.CurrentStep.String)
	}
	go s.runFullWorkflow(fund.ID, tradingDate.Format("2006-01-02"), runtime)
	return convertWorkflowStatus(run), nil
}

// --- schedulerService interface implementation (F7) ---
//
// These thin wrappers expose the workflowServiceAdapter's collaborators
// under the narrow schedulerService surface so the fundWorkflowScheduler
// can be unit-tested with stubs. Production behaviour is unchanged.

func (s *workflowServiceAdapter) ListActiveFunds(ctx context.Context) ([]repository.Fund, error) {
	if s == nil || s.fundRepo == nil {
		return nil, nil
	}
	return s.fundRepo.ListActive(ctx)
}

func (s *workflowServiceAdapter) GetWorkflowRun(ctx context.Context, fundID string, tradingDate time.Time) (*repository.WorkflowRun, error) {
	if s == nil || s.workflowRepo == nil {
		return nil, repository.ErrNotFound
	}
	return s.workflowRepo.GetByFundAndDate(ctx, fundID, tradingDate)
}

func (s *workflowServiceAdapter) NextWorkflowStart(now time.Time, profile marketcalendar.Profile) (time.Time, time.Time, error) {
	if s == nil || s.calendar == nil {
		return time.Time{}, time.Time{}, errors.New("calendar unavailable")
	}
	return s.calendar.NextWorkflowStart(now, profile)
}

func (s *workflowServiceAdapter) SessionForDate(date time.Time, profile marketcalendar.Profile) (*marketcalendar.TradingSession, error) {
	if s == nil || s.calendar == nil {
		return nil, errors.New("calendar unavailable")
	}
	return s.calendar.SessionForDate(date, profile)
}

func (s *workflowServiceAdapter) TradingProfileForFund(fund *repository.Fund) marketcalendar.Profile {
	if s == nil {
		return marketcalendar.Profile{}
	}
	return s.tradingProfileForFund(fund)
}

func (s *workflowServiceAdapter) StartWorkflowForFund(fund *repository.Fund, tradingDate, slotTime time.Time) (*api.WorkflowStatus, error) {
	return s.startWorkflowForFundWithSlot(fund, tradingDate, slotTime)
}

// TradingTriggerSlots is the pass-through used by the scheduler's
// catch-up path. See schedulerService.TradingTriggerSlots for the
// rationale.
func (s *workflowServiceAdapter) TradingTriggerSlots(session *marketcalendar.TradingSession, intervalMinutes int) []time.Time {
	if s == nil || s.calendar == nil || session == nil {
		return nil
	}
	return s.calendar.TradingTriggerSlots(session, intervalMinutes)
}

// AdminTriggerFund is the entry point used by the admin REST endpoint
// to force a daily workflow to start right now, regardless of the
// market calendar schedule. It reuses startWorkflowForFundWithMode in
// forceImmediate mode so the same workflow_run row machinery (claim /
// idempotency / restart-on-stale) applies; the admin path never
// bypasses the database guarantees that prevent double-fires.
//
// ctx is accepted for cancellation symmetry with the HTTP handler but
// the actual orchestrator runs in a detached goroutine that owns its
// own 2-hour timeout context (matching the cron's behaviour).
func (s *workflowServiceAdapter) AdminTriggerFund(ctx contextLike, fundID string, tradingDate time.Time) (*adminTriggerResult, error) {
	_ = ctx
	if s == nil || s.fundRepo == nil {
		return nil, errors.New("workflow service not initialised")
	}
	trimmed := strings.TrimSpace(fundID)
	if trimmed == "" {
		return nil, errors.New("fundId is required")
	}
	fund, err := s.fundRepo.GetByID(context.Background(), trimmed)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if fund == nil {
		return nil, api.ErrNotFound
	}
	normalized := normalizeTradingDate(tradingDate)
	status, err := s.startWorkflowForFundWithMode(fund, normalized, true)
	if err != nil {
		return nil, err
	}
	result := &adminTriggerResult{
		FundID:      fund.ID,
		TradingDate: normalized.Format("2006-01-02"),
	}
	if status != nil {
		result.State = status.State
		result.Step = status.Step
	}
	return result, nil
}

func (s *workflowServiceAdapter) startWorkflowForFundWithMode(fund *repository.Fund, tradingDate time.Time, forceImmediate bool) (*api.WorkflowStatus, error) {
	if fund == nil {
		return nil, api.ErrBadInput
	}
	tradingDate = normalizeTradingDate(tradingDate)
	if runtime := s.peekRuntime(fund.ID, tradingDate); runtime != nil && runtime.orchestrator != nil {
		if state := runtime.orchestrator.State(); state != nil && strings.TrimSpace(state.TradingDate) == tradingDate.Format("2006-01-02") {
			if state.Status == workflow.RunStatusRunning || state.Status == workflow.RunStatusPaused {
				status, err := s.persistRuntimeState(fund.ID, state, tradingDate)
				if err != nil {
					return nil, err
				}
				if status != nil && s.metrics != nil {
					s.metrics.ObserveWorkflow(fund.ID, status.State, status.Step)
				}
				return status, nil
			}
		}
	}

	now := time.Now().UTC()
	if forceImmediate {
		if err := s.restartStaleWorkflowRunIfNeeded(fund.ID, tradingDate, now); err != nil {
			return nil, err
		}
	}

	// F28: per-fund concurrent workflow gate. Runs BEFORE claiming the
	// run row so a rejected attempt doesn't leave a half-created
	// workflow_runs entry. Both scheduler ticks and admin manual
	// triggers funnel through here, so this single check covers both
	// surface areas.
	if s.quotaService != nil {
		if quotaErr := s.quotaService.CheckConcurrentWorkflows(context.Background(), fund.ID); quotaErr != nil {
			if s.metrics != nil {
				s.metrics.ObserveWorkflow(fund.ID, "quota_blocked", "")
			}
			return nil, mapRepositoryError(quotaErr)
		}
	}

	claimRun := s.workflowRepo.ClaimStart
	if forceImmediate {
		claimRun = s.workflowRepo.ClaimManualStart
	}
	run, claimed, err := claimRun(context.Background(), fund.ID, tradingDate, now, workflow.StepMacroBrief.String())
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if !claimed {
		if s.metrics != nil {
			s.metrics.ObserveWorkflow(fund.ID, run.Status, run.CurrentStep.String)
		}
		return convertWorkflowStatus(run), nil
	}

	s.cancelRuntime(s.takeRuntime(fund.ID, tradingDate))
	runtime := s.getRuntime(fund, tradingDate, time.Now(), forceImmediate)
	if s.metrics != nil {
		s.metrics.ObserveWorkflow(fund.ID, run.Status, run.CurrentStep.String)
	}
	go s.runFullWorkflow(fund.ID, tradingDate.Format("2006-01-02"), runtime)
	return convertWorkflowStatus(run), nil
}

func (s *workflowServiceAdapter) StartWorkflow(userID, fundID string) (*api.WorkflowStatus, error) {
	fund, err := authorizeFundAccess(context.Background(), s.fundRepo, s.companyRepo, userID, fundID)
	if err != nil {
		return nil, err
	}
	tradingDate, err := s.resolveStartTradingDateForFund(fund, time.Now())
	if err != nil {
		return nil, api.ErrBadInput
	}
	return s.startWorkflowForFundWithMode(fund, tradingDate, true)
}

func (s *workflowServiceAdapter) restartStaleWorkflowRunIfNeeded(fundID string, tradingDate, now time.Time) error {
	run, err := s.workflowRepo.GetByFundAndDate(context.Background(), fundID, tradingDate)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err != nil {
		return mapRepositoryError(err)
	}
	if !shouldRestartWorkflowRunForManualStart(run, now, s.peekRuntime(fundID, tradingDate) != nil) {
		return nil
	}
	updated := cancelledWorkflowRunForRestart(run, now)
	if err := s.workflowRepo.Update(context.Background(), updated); err != nil {
		return mapRepositoryError(err)
	}
	return nil
}

func shouldRestartWorkflowRunForManualStart(run *repository.WorkflowRun, now time.Time, hasRuntime bool) bool {
	if run == nil {
		return false
	}
	status := strings.ToLower(strings.TrimSpace(run.Status))
	if status != "running" && status != "paused" {
		return false
	}
	if workflowRunAwaitingApproval(run) {
		return false
	}
	if !hasRuntime {
		return true
	}
	lastUpdated := workflowRunLastUpdatedAt(run)
	if lastUpdated.IsZero() {
		lastUpdated = nullTimeValue(run.StartedAt)
	}
	if lastUpdated.IsZero() {
		lastUpdated = run.CreatedAt.UTC()
	}
	if lastUpdated.IsZero() {
		return false
	}
	return now.UTC().Sub(lastUpdated) >= manualWorkflowRestartStaleAfter
}

func workflowRunLastUpdatedAt(run *repository.WorkflowRun) time.Time {
	if run == nil {
		return time.Time{}
	}
	latest := time.Time{}
	stepResults := decodeWorkflowStepResults(run.StepResults)
	for _, item := range stepResults {
		for _, candidate := range []string{item.UpdatedAt, item.EndedAt, item.StartedAt} {
			parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(candidate))
			if err != nil {
				continue
			}
			parsed = parsed.UTC()
			if latest.IsZero() || parsed.After(latest) {
				latest = parsed
			}
		}
	}
	return latest
}

func cancelledWorkflowRunForRestart(run *repository.WorkflowRun, now time.Time) *repository.WorkflowRun {
	if run == nil {
		return nil
	}
	updated := *run
	updated.Status = "cancelled"
	updated.CompletedAt = sql.NullTime{Time: now.UTC(), Valid: true}
	stepName := strings.TrimSpace(run.CurrentStep.String)
	if stepName == "" {
		return &updated
	}
	step, err := parseWorkflowStep(stepName)
	if err != nil {
		return &updated
	}
	results := decodeWorkflowStepResultsToRuntime(run.StepResults)
	found := false
	for i := range results {
		if results[i].Step != step {
			continue
		}
		if results[i].StartedAt.IsZero() {
			results[i].StartedAt = nullTimeValue(run.StartedAt)
		}
		results[i].Status = "cancelled"
		results[i].EndedAt = now.UTC()
		results[i].Error = nil
		found = true
		break
	}
	if !found {
		results = append(results, workflow.StepResult{
			Step:      step,
			Status:    "cancelled",
			StartedAt: nullTimeValue(run.StartedAt),
			EndedAt:   now.UTC(),
		})
	}
	syncWorkflowRun(&updated, workflow.WorkflowState{
		FundID:      run.FundID,
		TradingDate: normalizeTradingDate(run.TradingDate).Format("2006-01-02"),
		Status:      workflow.RunStatusCancelled,
		CurrentStep: step,
		StepResults: results,
		StartedAt:   nullTimeValue(run.StartedAt),
		EndedAt:     now.UTC(),
	})
	return &updated
}

func (s *workflowServiceAdapter) TriggerStep(userID, fundID, step string) (*api.WorkflowStatus, error) {
	fund, err := authorizeFundAccess(context.Background(), s.fundRepo, s.companyRepo, userID, fundID)
	if err != nil {
		return nil, err
	}

	workflowStep, err := parseWorkflowStep(step)
	if err != nil {
		return nil, err
	}
	if !workflow.SupportsManualTrigger(workflowStep) {
		return nil, fmt.Errorf("%w: manual trigger unsupported for step %s; supported steps: %s", api.ErrBadInput, workflowStep.String(), strings.Join(workflow.SupportedManualTriggerStepNames(), ", "))
	}

	tradingDate, err := s.resolveTradingDateForFund(fund, time.Now(), marketcalendar.ResolutionCurrentTradingDay)
	if err != nil {
		return nil, api.ErrBadInput
	}
	runtime := s.getRuntime(fund, tradingDate, time.Now(), false)
	run, claimed, err := s.workflowRepo.ClaimManualStep(context.Background(), fund.ID, tradingDate, workflowStep.String())
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if !claimed {
		return nil, api.ErrConflict
	}
	if run != nil {
		s.restoreRuntimeFromRun(runtime, run)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if _, err := runtime.orchestrator.TriggerStep(ctx, workflowStep, tradingDate.Format("2006-01-02")); err != nil {
		status, persistErr := s.persistRuntimeStateIfCurrent(fund.ID, runtime, tradingDate)
		if persistErr != nil {
			return nil, persistErr
		}
		if s.metrics != nil && status != nil {
			s.metrics.ObserveWorkflow(fund.ID, status.State, status.Step)
		}
		return nil, err
	}
	status, err := s.persistRuntimeStateIfCurrent(fund.ID, runtime, tradingDate)
	if err != nil {
		return nil, err
	}
	if s.metrics != nil && status != nil {
		s.metrics.ObserveWorkflow(fund.ID, status.State, status.Step)
	}
	return status, nil
}

// AdminTriggerStep is the Sprint 9.2 admin-side resume entry point.
// Unlike TriggerStep it does NOT call authorizeFundAccess — the
// admin handler already enforced requireAdmin — and it takes the
// trading date the operator (or the resolveResumeTarget helper)
// has already decided on rather than recomputing it from the
// market calendar. The rest of the path mirrors TriggerStep
// exactly so a resumed step goes through the same retry / event /
// checkpoint plumbing as a normally scheduled step.
func (s *workflowServiceAdapter) AdminTriggerStep(ctx context.Context, fundID string, tradingDate time.Time, step string) (*api.WorkflowStatus, error) {
	if s == nil {
		return nil, api.ErrNotImplemented
	}
	if strings.TrimSpace(fundID) == "" || strings.TrimSpace(step) == "" {
		return nil, api.ErrBadInput
	}
	workflowStep, err := parseWorkflowStep(step)
	if err != nil {
		return nil, err
	}
	if !workflow.SupportsManualTrigger(workflowStep) {
		return nil, fmt.Errorf("%w: manual trigger unsupported for step %s; supported steps: %s", api.ErrBadInput, workflowStep.String(), strings.Join(workflow.SupportedManualTriggerStepNames(), ", "))
	}
	fund, err := s.fundRepo.GetByID(ctx, fundID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if fund == nil {
		return nil, api.ErrNotFound
	}
	runtime := s.getRuntime(fund, tradingDate, time.Now(), false)
	run, claimed, err := s.workflowRepo.ClaimManualStep(ctx, fund.ID, tradingDate, workflowStep.String())
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if !claimed {
		return nil, api.ErrConflict
	}
	if run != nil {
		s.restoreRuntimeFromRun(runtime, run)
	}
	triggerCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if _, err := runtime.orchestrator.TriggerStep(triggerCtx, workflowStep, tradingDate.Format("2006-01-02")); err != nil {
		status, persistErr := s.persistRuntimeStateIfCurrent(fund.ID, runtime, tradingDate)
		if persistErr != nil {
			return nil, persistErr
		}
		if s.metrics != nil && status != nil {
			s.metrics.ObserveWorkflow(fund.ID, status.State, status.Step)
		}
		return nil, err
	}
	status, err := s.persistRuntimeStateIfCurrent(fund.ID, runtime, tradingDate)
	if err != nil {
		return nil, err
	}
	if s.metrics != nil && status != nil {
		s.metrics.ObserveWorkflow(fund.ID, status.State, status.Step)
	}
	return status, nil
}

// workflowCheckpointResumeAdapter satisfies the admin handler's
// workflowCheckpointResumeSink contract by forwarding into the
// existing workflowServiceAdapter's AdminTriggerStep path.
type workflowCheckpointResumeAdapter struct {
	svc *workflowServiceAdapter
}

func newWorkflowCheckpointResumeAdapter(svc *workflowServiceAdapter) *workflowCheckpointResumeAdapter {
	if svc == nil {
		return nil
	}
	return &workflowCheckpointResumeAdapter{svc: svc}
}

func (a *workflowCheckpointResumeAdapter) ResumeStep(ctx context.Context, fundID string, tradingDate time.Time, step string) (*api.WorkflowStatus, error) {
	if a == nil || a.svc == nil {
		return nil, api.ErrNotImplemented
	}
	return a.svc.AdminTriggerStep(ctx, fundID, tradingDate, step)
}

func (s *workflowServiceAdapter) GetStatus(userID, fundID string) (*api.WorkflowStatus, error) {
	fund, err := authorizeFundAccess(context.Background(), s.fundRepo, s.companyRepo, userID, fundID)
	if err != nil {
		return nil, err
	}

	tradingDate, err := s.resolveTradingDateForFund(fund, time.Now(), marketcalendar.ResolutionLatestTradingDay)
	if err != nil {
		return nil, api.ErrBadInput
	}
	tradingDate, err = s.preferredWorkflowStatusDate(context.Background(), fund.ID, tradingDate)
	if err != nil {
		return nil, err
	}
	runtime := s.peekRuntime(fund.ID, tradingDate)
	if runtime != nil && runtime.orchestrator != nil {
		if state := runtime.orchestrator.State(); state != nil && strings.TrimSpace(state.TradingDate) == tradingDate.Format("2006-01-02") {
			return convertWorkflowStateToStatus(fund.ID, state.Snapshot()), nil
		}
	}

	run, err := s.workflowRepo.GetByFundAndDate(context.Background(), fund.ID, tradingDate)
	if errors.Is(err, repository.ErrNotFound) {
		return &api.WorkflowStatus{
			FundID:      fund.ID,
			TradingDate: tradingDate.Format("2006-01-02"),
			State:       "idle",
			Step:        "not_started",
		}, nil
	}
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	return convertWorkflowStatus(run), nil
}

// GetNextRun resolves the next workflow trigger for the fund and
// expands it into the per-step wall-clock schedule. The implementation
// is intentionally read-only — it never mutates scheduler state — so
// it's safe to call from a polling banner. When the calendar service
// isn't wired (single-binary smoke runs) it returns ErrServiceUnavailable
// so the frontend renders a "schedule unavailable" placeholder instead
// of a misleading guess.
func (s *workflowServiceAdapter) GetNextRun(userID, fundID string) (*api.NextWorkflowRun, error) {
	fund, err := authorizeFundAccess(context.Background(), s.fundRepo, s.companyRepo, userID, fundID)
	if err != nil {
		return nil, err
	}
	if s.calendar == nil {
		return nil, api.ErrUpstreamUnavailable
	}
	profile := s.tradingProfileForFund(fund)
	now := time.Now()
	triggerAt, tradingDate, err := s.calendar.NextWorkflowStart(now, profile)
	if err != nil {
		return nil, api.ErrUpstreamUnavailable
	}
	out := &api.NextWorkflowRun{
		FundID:        fund.ID,
		TradingDate:   tradingDate.Format("2006-01-02"),
		NextTriggerAt: triggerAt.UTC(),
	}
	// Pull the full step schedule for the same trading date so the
	// banner can show "macro_brief 09:30, daily_review 16:00" without
	// a second round-trip. SessionForDate is the same call
	// buildWorkflowScheduleForDateAt uses internally, so the wall-clock
	// times the user sees match the times the orchestrator will wait for.
	session, err := s.calendar.SessionForDate(tradingDate, profile)
	if err == nil && session != nil && session.IsTradingDay {
		if session.Location != nil {
			out.Timezone = session.Location.String()
			out.CurrentlyInWindow = s.shouldRunWorkflowImmediately(now, session)
		}
		// Interval mode: surface the full slot list for the day so the
		// banner shows "13:00 / 13:30 / 14:00 / ..." instead of a
		// misleading single-shot step schedule. Steps stays nil so the
		// frontend can branch cleanly on Slots != nil.
		if profile.DecisionIntervalMinutes != nil {
			interval := *profile.DecisionIntervalMinutes
			slots := s.calendar.TradingTriggerSlots(session, interval)
			out.Slots = make([]time.Time, 0, len(slots))
			for _, slot := range slots {
				out.Slots = append(out.Slots, slot.UTC())
			}
			out.IntervalMinutes = &interval
		} else {
			step := s.calendar.BuildStepSchedule(session)
			out.Steps = &api.WorkflowStepSchedule{
				MacroBrief:       step.MacroBrief.UTC(),
				ResearchParallel: step.ResearchParallel.UTC(),
				QuantSignals:     step.QuantSignals.UTC(),
				Roundtable:       step.Roundtable.UTC(),
				PMPlan:           step.PMPlan.UTC(),
				RiskReview:       step.RiskReview.UTC(),
				UserApproval:     step.UserApproval.UTC(),
				TradeExecution:   step.TradeExecution.UTC(),
				Settlement:       step.Settlement.UTC(),
				DailyReview:      step.DailyReview.UTC(),
			}
		}
	}
	return out, nil
}

func (s *workflowServiceAdapter) preferredWorkflowStatusDate(ctx context.Context, fundID string, fallback time.Time) (time.Time, error) {
	if s == nil || s.workflowRepo == nil {
		return fallback, nil
	}
	latest, err := s.workflowRepo.GetLatestByFund(ctx, fundID)
	if errors.Is(err, repository.ErrNotFound) {
		return fallback, nil
	}
	if err != nil {
		return time.Time{}, mapRepositoryError(err)
	}
	if latest == nil {
		return fallback, nil
	}
	if latest.TradingDate.After(fallback) && workflowRunStatusIsActive(latest.Status) {
		return latest.TradingDate.UTC(), nil
	}
	return fallback, nil
}

func workflowRunStatusIsActive(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running", "paused":
		return true
	default:
		return false
	}
}

func (s *workflowServiceAdapter) getOrCreateCurrentRun(fundID string, tradingDate time.Time) (*repository.WorkflowRun, error) {
	run, err := s.workflowRepo.GetByFundAndDate(context.Background(), fundID, tradingDate)
	if errors.Is(err, repository.ErrNotFound) {
		return &repository.WorkflowRun{
			FundID:      fundID,
			TradingDate: tradingDate,
			Status:      "pending",
			StepResults: json.RawMessage(`{}`),
		}, nil
	}
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	return run, nil
}

func isDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "duplicate key") || strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}

func (s *workflowServiceAdapter) saveRun(run *repository.WorkflowRun) (*repository.WorkflowRun, error) {
	merged, err := s.workflowRepo.UpsertMerged(context.Background(), run)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	return merged, nil
}

func workflowRuntimeKey(fundID string, tradingDate time.Time) string {
	return strings.TrimSpace(fundID) + ":" + normalizeTradingDate(tradingDate).Format("2006-01-02")
}

func (s *workflowServiceAdapter) restoreRuntimeFromRun(runtime *workflowRuntime, run *repository.WorkflowRun) {
	if runtime == nil || runtime.orchestrator == nil || run == nil {
		return
	}
	runtime.orchestrator.RestoreState(workflow.WorkflowState{
		RunID:       run.ID,
		FundID:      run.FundID,
		TradingDate: normalizeTradingDate(run.TradingDate).Format("2006-01-02"),
		Status:      parseRepositoryWorkflowStatus(run.Status),
		CurrentStep: parseWorkflowStepOrZero(run.CurrentStep.String),
		StepResults: decodeWorkflowStepResultsToRuntime(run.StepResults),
		StartedAt:   nullTimeValue(run.StartedAt),
		EndedAt:     nullTimeValue(run.CompletedAt),
	})
}

func (s *workflowServiceAdapter) getRuntime(fund *repository.Fund, tradingDate time.Time, now time.Time, forceImmediate bool) *workflowRuntime {
	tradingDate = normalizeTradingDate(tradingDate)
	key := workflowRuntimeKey(fund.ID, tradingDate)
	s.mu.Lock()
	defer s.mu.Unlock()
	if runtime, ok := s.runtimes[key]; ok {
		if forceImmediate {
			runtime.orchestrator.SetForceImmediate(true)
		}
		return runtime
	}
	runtime := s.newRuntime(fund, tradingDate, now)
	if forceImmediate {
		runtime.orchestrator.SetForceImmediate(true)
	}
	s.runtimes[key] = runtime
	return runtime
}

func (s *workflowServiceAdapter) peekRuntime(fundID string, tradingDate time.Time) *workflowRuntime {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runtimes[workflowRuntimeKey(fundID, tradingDate)]
}

func (s *workflowServiceAdapter) takeRuntime(fundID string, tradingDate time.Time) *workflowRuntime {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := workflowRuntimeKey(fundID, tradingDate)
	runtime := s.runtimes[key]
	delete(s.runtimes, key)
	return runtime
}

func (s *workflowServiceAdapter) newRuntime(fund *repository.Fund, tradingDate time.Time, now time.Time) *workflowRuntime {
	planRepo := repository.NewPlanRepo(s.db)
	fundRepo := repository.NewFundRepo(s.db)
	agentRepo := repository.NewAgentRepo(s.db)
	teamRepo := repository.NewTeamRepo(s.db)
	tradeRepo := repository.NewTradeRepo(s.db)
	positionRepo := repository.NewPositionRepo(s.db)
	navRepo := repository.NewNavSnapshotRepo(s.db)
	workflowRepo := repository.NewWorkflowRunRepo(s.db)
	memoryRepo := repository.NewMemoryRepo(s.db)
	lotRepo := repository.NewLotRepo(s.db)
	uow := repository.NewUnitOfWork(s.db)
	// lotLedger writes the FIFO lot ledger on every successful
	// equity fill. It runs inside its own UoW (separate from
	// trade_executions) so a lot-ledger error never knocks out a
	// real trade — failures are logged and the trade survives.
	lotLedger := lotledger.NewService(lotRepo, slog.Default())
	schedule := s.buildWorkflowScheduleForDateAt(fund, tradingDate, now)
	runtime := &workflowRuntime{tradingDate: tradingDate.Format("2006-01-02")}
	var delegate workflow.EventBus = runtimeEventBus{}
	if s.activityBus != nil {
		delegate = s.activityBus
	}
	// Wrap with persisting bus so workflow_runs is updated on every step
	// transition (not just when RunFull returns). Critical for paused runs
	// where RunFull blocks indefinitely in WaitForDecision.
	var bus workflow.EventBus = newPersistingEventBus(s, fund.ID, tradingDate, delegate)
	// Resolve the fund operator + primary researcher AgentID exactly
	// once and reuse for both the debate roundtable and the sentiment
	// scorer so the model router can honour per-user / per-agent
	// preferences instead of silently routing every internal step to
	// the platform .env default. P2-T2 follow-up: keeps debate +
	// sentiment in lock-step with the per-agent routing the PM was
	// already getting from runDecisionEngine.
	//
	// Guarded by s.db != nil because a handful of unit tests
	// (TriggerStep + conflict tests) construct a workflowServiceAdapter
	// with only fundRepo + workflowRepo populated and no shared *sql.DB,
	// so creating fresh TeamRepo/AgentRepo here would just wrap nil and
	// panic on the first ListByFund. The test paths never reach the
	// LLM call where routing matters, so leaving hints blank is safe.
	var ownerUserID, researcherAgentID string
	if s.db != nil {
		ownerUserID, researcherAgentID = resolveFundOperatorRouting(context.Background(), fund.ID, teamRepo, agentRepo)
	}
	runtime.orchestrator = workflow.NewDailyOrchestrator(
		fund.ID,
		bus,
		runtimeResearcherPool{
			fundRepo:           fundRepo,
			teamRepo:           teamRepo,
			agentRepo:          agentRepo,
			marketData:         s.marketData,
			memoryRepo:         memoryRepo,
			debateRoundtable:   buildDebateRoundtable(s.runtime, fund.ID, ownerUserID, researcherAgentID),
			debateForceEnabled: debateForceEnabledFromEnv(),
			ohlcFetcher:        s.ohlcFetcher,
			fundamentalFetcher: s.fundamentalFetcher,
			sectorFlowFetcher:  s.sectorFlowFetcher,
			sentimentScorer:    buildSentimentScorerFromRuntime(s.runtime, fund.ID, ownerUserID),
			socialRegistry:     s.socialRegistry,
			llmRuntime:         s.runtime,
		},
		func() *runtimePMAgent {
			// Sprint A #1: regime service + ATR snapshot builder
			// share the same OHLC fetcher so a single fetch per
			// (symbol, day) feeds both the regime classifier AND
			// the volatility math. The builder is intentionally
			// constructed even when s.ohlcFetcher is nil — its
			// BuildBatch then returns nil and the prompt gets no
			// quantSnapshots block, preserving legacy behaviour.
			regimeSvc := regime.NewService(s.ohlcFetcher)
			return &runtimePMAgent{
				db:             s.db,
				planRepo:       planRepo,
				fundRepo:       fundRepo,
				positionRepo:   positionRepo,
				teamRepo:       teamRepo,
				agentRepo:      agentRepo,
				marketData:     s.marketData,
				tradeRepo:      tradeRepo,
				decisionEngine: buildLLMDecisionEngine(s.runtime, fund.ID),
				// Sprint 11.4 — pipe the serverMetrics into the PM
				// agent so recordDecisionSource can publish
				// fundai_pm_decision_total. Nil-safe: smoke /
				// integration builds with no metrics struct fall
				// through to the no-op recorder.
				decisionSourceObserver: pmDecisionSourceObserverFromMetrics(s.metrics),
				lotRepo:        lotRepo,
				exitManager:    exitmanager.NewService(),
				// regimeService is OPTIONAL — when s.ohlcFetcher is
				// nil (single-binary smoke / OHLC disabled builds)
				// the constructor returns a Service that always
				// answers Unknown, which the wiring treats as
				// "skip the tag". No need to fork the construction.
				regimeService: regimeSvc,
				ohlcFetcher:   s.ohlcFetcher,
				// Phase 3A-5: re-using the memory repo for the
				// attribution lesson gate. The daily-review hook
				// already builds one inside runtimeMemorySystem;
				// here we share the repo so the PMAgent can fold
				// active lessons into strategy.Service mutes.
				memoryRepo: memoryRepo,
				// Phase 3A-7: same attribution service the daily
				// review and the HTTP endpoint share. Nil-safe:
				// when s.attribution is nil (legacy / smoke
				// builds) the PMAgent skips the scorecard step.
				attribution: s.attribution,
				// Sprint 3 / L1: intraday snapshot builder
				// shares the same OHLC fetcher. 5m default
				// because A 股 + US 都默认提供 5m bars; funds
				// that want 15m / 60m for less-active universes
				// can swap the interval here. nil ohlcFetcher
				// → Build returns no rows, prompt omits block.
				intradayBuilder: intraday.NewBuilder(s.ohlcFetcher, intraday.Interval5m),

				// Sprint 3 / L3: semantic memory recall (pgvector
				// cosine similarity). Both fields are set together
				// via the buildSemanticRecallStack helper so the
				// nil-checks stay clean — when either piece is
				// missing the whole stack degrades silently.
				recall:         s.recallService,
				recallEmbedder: s.recallEmbedder,

				// Sprint 3 / L3: cross-agent contradiction checker.
				// Same llmRuntime as other LLM steps, simple tier,
				// 20s timeout. Nil runtime → disabled.
				contradiction: newContradictionRunner(s.runtime),

				// Sprint A #1: same regimeSvc + ohlcFetcher
				// pair. Options defaults are deliberate
				// (50bps risk, 2x ATR stop, 0.5%-10% ceiling)
				// — operators tuning these per-fund will go
				// through a config knob in a follow-up PR.
				quantSnapshot: quantsnapshot.NewBuilder(regimeSvc, s.ohlcFetcher, quantsnapshot.Options{}),
				// Sprint B #1: per-symbol re-entry lock
				// driven by trade_executions. NewService
				// degrades gracefully on nil DB (no-op
				// Lookup) so the constructor stays
				// unconditional. We pass s.db rather than
				// a repo so cooldown doesn't pull in the
				// repository package and keep the cycle-free
				// dependency graph intact.
				cooldownSvc: cooldown.NewService(s.db, cooldown.Options{}),
				// Sprint B #2: dynamic risk-budget throttle
				// driven by nav_snapshots. Same nil-DB
				// guard as cooldown; the default Options
				// give us 60d lookback, 15% annualised vol
				// target, 25% drawdown ceiling, and the
				// canonical AHL-style [0.5, 2.0] vol scalar
				// band. Operators can later expose per-fund
				// overrides via fund.config.riskBudget but
				// Sprint B ships with the platform default.
				riskBudgetSvc: riskbudget.NewService(s.db, riskbudget.Options{}),
				// Sprint B #3: per-symbol news catalyst
				// recall. Shares the marketdata.Service
				// instance with the rest of the wiring so
				// cache + provider rotation + translation
				// are reused; default Options give us a 7d
				// MaxAge, top-3 per symbol, 4-way fetch
				// concurrency, 6s per-call timeout. A nil
				// marketdata.Service degrades gracefully —
				// the constructor stays unconditional.
				newsCatalystSvc: newsrecall.NewService(s.marketData, newsrecall.Options{}),
				// Sprint E #2: scheduled-earnings catalyst
				// snapshot. Default = Yahoo Finance v10
				// quoteSummary (zero-auth, US-focused) via
				// buildEarningsFetcherFromEnv; falls back to
				// NoopFetcher when env disables the provider.
				// The fetcher can be swapped to
				// earnings.StaticFetcher (hand-curated YAML)
				// or a future Finnhub / Polygon adapter via
				// the same env-driven builder.
				earningsSvc: earnings.NewService(buildEarningsFetcherFromEnv(), earnings.Options{}),
				// Sprint A #2: cross-sectional ranker shares
				// the same OHLC fetcher so bars asked for
				// by the quant snapshot pass come back from
				// cache. Options defaults are intentionally
				// classic AQR-style (20d momentum / 20d vol
				// / 10d $vol; weights 0.5/-0.3/0.2).
				ranker: ranking.NewRanker(s.ohlcFetcher, ranking.Options{}),
				// Sprint E #3: cross-sectional quality factor
				// (Asness/Frazzini/Pedersen "Quality Minus
				// Junk" decomposition). Shares the cached
				// fundamental.Fetcher with the rest of the
				// wiring so the per-symbol Fetch is reused
				// across QualityScores and the existing
				// FundamentalSummary renderer. nil fetcher
				// degrades gracefully (BuildScores returns
				// nil, the prompt skips the block).
				qualitySvc: quality.NewService(s.fundamentalFetcher, quality.Options{}),
				// Sprint F #1: cross-sectional value factor
				// (B/P + E/P + D/P composite, Fama-French
				// HML lineage). Same cached fundamental
				// fetcher as qualitySvc so quality and value
				// composites come from a single per-symbol
				// Fetch. nil fetcher degrades to no-op.
				valueSvc: value.NewService(s.fundamentalFetcher, value.Options{}),
				// Sprint F #2: Frazzini-Pedersen Betting-
				// Against-Beta defensive overlay. Reuses the
				// shared ohlc.Fetcher cache (warm from
				// quantSnapshot / ranker / correlation /
				// pairspread) plus one extra fetch per
				// market for the benchmark index (SPY / CSI300
				// ETF / Tracker Fund of HK). nil fetcher
				// degrades to no-op.
				lowBetaSvc: lowbeta.NewService(s.ohlcFetcher, lowbeta.Options{}),
				// Sprint F #3: Post-Earnings Announcement
				// Drift overlay. Composes the historical
				// earnings.HistoryService (default = Yahoo
				// v10 earningsHistory module via
				// buildEarningsHistoryFetcherFromEnv) with
				// the shared ohlc.Fetcher. nil components
				// degrade gracefully (block omitted from the
				// prompt).
				peadSvc: pead.NewService(
					earnings.NewHistoryService(
						buildEarningsHistoryFetcherFromEnv(),
						earnings.HistoryOptions{},
					),
					s.ohlcFetcher,
					pead.Options{},
				),
				// Sprint C #2: pairwise correlation matrix
				// over the universe ∪ positions set. Reuses
				// the same OHLC fetcher so the third pass
				// hits the cache populated by quantSnapshot
				// + ranker. The default Options match the
				// classic risk-parity convention (60d daily
				// lookback, 0.7 |rho| threshold, 10 max
				// pairs in the prompt, 4-way fetch
				// concurrency, 6s per-call timeout). A nil
				// OHLC fetcher degrades to no-op Compute so
				// the prompt block is simply omitted.
				correlationSvc: correlation.NewService(s.ohlcFetcher, correlation.Options{}),
				// Sprint E #4: rolling pair-spread monitor.
				// Consumes the HighCorrPairs the
				// correlationSvc emits, then fires its own
				// OHLC fetches behind the shared cache so
				// the second pass is nearly free. Default
				// Options match the correlation lookback
				// (60d) and the classical 2-σ entry
				// threshold; MaxPairs=10 keeps the prompt
				// block bounded. nil ohlcFetcher degrades
				// to no-op Build so the prompt simply
				// omits the block.
				pairSpreadSvc: pairspread.NewService(s.ohlcFetcher, pairspread.Options{}),
				// Sprint D #1 — Prometheus counters for PM
				// decision-input observability. Shares the
				// global metrics registry; nil-safe inside
				// ObserveDecisionInput so tests that don't
				// wire metrics keep working.
				serverMetrics: s.metrics,
				// Sprint 9.1 — alpha-aware memory. The PM
				// reads the per-fund agent leaderboard +
				// recent alpha-tagged lessons from these
				// repos and renders the markdown block via
				// alphalesson.BuildContext. Both are nil-safe:
				// when either is unwired (legacy / smoke
				// builds before the reputation loop is on)
				// buildAgentTrackRecord returns "" and the
				// prompt simply omits the section.
				agentReputationRepo: s.agentReputationRepo,
				alphaLessonRepo:     s.alphaLessonRepo,
			}
		}(),
		&runtimeApprovalGateway{
			planRepo:  planRepo,
			fundRepo:  fundRepo,
			tradeRepo: tradeRepo,
			isCurrent: func() bool { return s.peekRuntime(fund.ID, tradingDate) == runtime },
		},
		&runtimeTradingEngine{
			planRepo:     planRepo,
			fundRepo:     fundRepo,
			tradeRepo:    tradeRepo,
			positionRepo: positionRepo,
			navRepo:      navRepo,
			teamRepo:     teamRepo,
			agentRepo:    agentRepo,
			marketData:   s.marketData,
			metrics:      s.metrics,
			lotRepo:      lotRepo,
			lotLedger:    lotLedger,
			uow:          uow,
			cashLedger:   repository.NewCashLedgerRepo(s.db),
			// S12-followup: share the same gate impls the
			// broker.Simulator was wired with, so PM-direct
			// fills see identical market-status / lockup /
			// borrow / price-collar verdicts. nil-tolerant on
			// each field (engine-side no-op when nil).
			marketStatusGate: s.marketStatusGate,
			lockupGate:       s.lockupGate,
			borrowGate:       s.borrowGate,
			priceCollarGate:  s.priceCollarGate,
		},
		&runtimeMemorySystem{
			db:           s.db,
			fundRepo:     fundRepo,
			agentRepo:    agentRepo,
			teamRepo:     teamRepo,
			planRepo:     planRepo,
			tradeRepo:    tradeRepo,
			positionRepo: positionRepo,
			navRepo:      navRepo,
			workflowRepo: workflowRepo,
			memoryRepo:   memoryRepo,
			llmRuntime:   s.runtime,
			attribution:  s.attribution,
		},
		workflow.WithRiskAgent(&runtimeRiskAgent{
			planRepo:     planRepo,
			fundRepo:     fundRepo,
			positionRepo: positionRepo,
			teamRepo:     teamRepo,
			agentRepo:    agentRepo,
		}),
		workflow.WithSchedule(schedule),
		// Sprint 4 / android-core: terminal plan transitions fan
		// out to FCM via the device-token registry. Notifier is
		// optional — nil-safe in workflow's defensive recover.
		workflow.WithPlanLifecycleNotifier(s.planLifecycleNotifier),
		// Sprint 9.2 — per-step checkpoint persistence. The
		// sink is built once per orchestrator from the shared
		// repo; nil sink (test paths / legacy wiring without
		// a DB) silently disables checkpoint storage.
		workflow.WithCheckpointStore(newWorkflowCheckpointSink(s.workflowCheckpointRepo)),
	)
	return runtime
}

func (s *workflowServiceAdapter) cancelRuntime(runtime *workflowRuntime) {
	if runtime == nil || runtime.orchestrator == nil {
		return
	}
	runtime.orchestrator.Cancel()
}

func (s *workflowServiceAdapter) currentRuntimeOwnsPlan(fundID string, tradingDate time.Time, planID string) bool {
	normalizedTradingDate := normalizeTradingDate(tradingDate)
	runtime := s.peekRuntime(fundID, normalizedTradingDate)
	if runtime == nil || runtime.orchestrator == nil {
		return false
	}
	state := runtime.orchestrator.State()
	if state == nil {
		return false
	}
	if strings.TrimSpace(state.TradingDate) != normalizedTradingDate.Format("2006-01-02") {
		return false
	}
	if strings.TrimSpace(state.PlanID) != strings.TrimSpace(planID) {
		return false
	}
	switch state.Status {
	case workflow.RunStatusPaused:
		return state.CurrentStep == workflow.StepUserApproval
	case workflow.RunStatusRunning, workflow.RunStatusCompleted:
		return true
	default:
		return false
	}
}

type effectiveTeamIntervals struct {
	Researcher int
	PM         int
	Risk       int
	Trader     int
}

func (s *workflowServiceAdapter) getEffectiveTeamIntervals(fundID string) effectiveTeamIntervals {
	defaultMinutes := 15
	if s.subscriptionService != nil {
		settings, err := s.subscriptionService.GetPlatformSettings(context.Background())
		if err == nil && settings != nil {
			defaultMinutes = settings.DefaultTeamIntervalMinutes
		}
	}
	defaultMinutes = normalizeFundTeamIntervalValue(defaultMinutes)
	result := effectiveTeamIntervals{
		Researcher: defaultMinutes,
		PM:         defaultMinutes,
		Risk:       defaultMinutes,
		Trader:     defaultMinutes,
	}
	if s == nil || s.fundRepo == nil {
		return result
	}
	fund, err := s.fundRepo.GetByID(context.Background(), fundID)
	if err != nil || fund == nil {
		return result
	}
	profile := decodeFundMarketProfile(fund.Config)
	if profile.TeamIntervals == nil {
		return result
	}
	if profile.TeamIntervals.Researcher != nil {
		result.Researcher = normalizeFundTeamIntervalValue(*profile.TeamIntervals.Researcher)
	}
	if profile.TeamIntervals.PM != nil {
		result.PM = normalizeFundTeamIntervalValue(*profile.TeamIntervals.PM)
	}
	if profile.TeamIntervals.Risk != nil {
		result.Risk = normalizeFundTeamIntervalValue(*profile.TeamIntervals.Risk)
	}
	if profile.TeamIntervals.Trader != nil {
		result.Trader = normalizeFundTeamIntervalValue(*profile.TeamIntervals.Trader)
	}
	return result
}

func normalizeFundTeamIntervalValue(value int) int {
	if value < 5 {
		return 5
	}
	if value > 1440 {
		return 1440
	}
	return int(math.Round(float64(value)/5.0)) * 5
}

// workflowRunCeiling bounds the wall-clock lifetime of a single
// workflow goroutine. A full daily workflow legitimately needs to span
// the entire trading day when ForceImmediate=false: MacroBrief at 09:00
// ET / 21:00 Beijing → DailyReview at ~16:30 ET / ~04:30 Beijing next
// day. When a user starts (or approves) a workflow late at night /
// before the next session, the goroutine sleeps inside
// waitForScheduledOrInterval until the scheduled wall-clock time. The
// previous 2 h cap killed the context mid-wait, which manifested as a
// silent "trade execution failed" step ~2 hours after approval — no
// trades placed. 24 h covers every single-day schedule (max wait ≈
// MacroBrief→DailyReview ≈ 8 h plus user-trigger lead time) with
// generous headroom. Per-step timeouts inside stepTimeout(...) still
// bound any LLM call individually so a runaway provider cannot tie up
// this slot forever.
const workflowRunCeiling = 24 * time.Hour

func (s *workflowServiceAdapter) runFullWorkflow(fundID, tradingDate string, runtime *workflowRuntime) {
	ctx, cancel := context.WithTimeout(context.Background(), workflowRunCeiling)
	defer cancel()
	_, _ = runtime.orchestrator.RunFull(ctx, tradingDate)
	parsed, err := time.Parse("2006-01-02", strings.TrimSpace(tradingDate))
	if err != nil {
		parsed = workflowTradingDate(time.Now())
	}
	status, _ := s.persistRuntimeStateIfCurrent(fundID, runtime, parsed.UTC())
	if s.metrics != nil && status != nil {
		s.metrics.ObserveWorkflow(fundID, status.State, status.Step)
	}
}

func (s *workflowServiceAdapter) validateApprovePlanResume(plan *repository.InvestmentPlan) error {
	if s == nil {
		return api.ErrNotImplemented
	}
	if s.resumePlan != nil || s.workflowRepo == nil || s.planRepo == nil {
		return nil
	}
	if plan == nil {
		return api.ErrBadInput
	}
	trimmedPlanID := strings.TrimSpace(plan.ID)
	if trimmedPlanID == "" {
		return api.ErrBadInput
	}
	normalizedTradingDate := normalizeTradingDate(plan.TradingDate)
	run, err := s.workflowRepo.GetByFundAndDate(context.Background(), plan.FundID, normalizedTradingDate)
	if errors.Is(err, repository.ErrNotFound) {
		return api.ErrConflict
	}
	if err != nil {
		return mapRepositoryError(err)
	}
	if s.currentRuntimeOwnsPlan(plan.FundID, normalizedTradingDate, trimmedPlanID) {
		return nil
	}
	if !workflowRunAwaitingApproval(run) {
		return api.ErrConflict
	}
	return nil
}

func (s *workflowServiceAdapter) ResumeApprovedPlan(fundID string, tradingDate time.Time, planID string) error {
	if s == nil {
		return api.ErrNotImplemented
	}
	trimmedPlanID := strings.TrimSpace(planID)
	if trimmedPlanID == "" {
		return api.ErrBadInput
	}
	normalizedTradingDate := normalizeTradingDate(tradingDate)
	if s.resumePlan != nil {
		return s.resumePlan(fundID, normalizedTradingDate, trimmedPlanID)
	}
	if s.planRepo == nil {
		return api.ErrNotImplemented
	}
	fund, err := s.fundRepo.GetByID(context.Background(), fundID)
	if err != nil {
		return mapRepositoryError(err)
	}
	plan, err := s.planRepo.GetByID(context.Background(), trimmedPlanID)
	if err != nil {
		return mapRepositoryError(err)
	}
	if plan.FundID != fundID || normalizeTradingDate(plan.TradingDate) != normalizedTradingDate {
		return api.ErrConflict
	}
	planStatus := strings.ToLower(strings.TrimSpace(plan.Status))
	if planStatus != "approved" && planStatus != "executing" && planStatus != "completed" {
		return api.ErrConflict
	}
	run, err := s.workflowRepo.GetByFundAndDate(context.Background(), fundID, normalizedTradingDate)
	if err != nil {
		return mapRepositoryError(err)
	}
	status := strings.ToLower(strings.TrimSpace(run.Status))
	if status == "completed" || status == "rejected" {
		return nil
	}
	if s.currentRuntimeOwnsPlan(fundID, normalizedTradingDate, trimmedPlanID) {
		return nil
	}
	if !workflowRunAwaitingApproval(run) {
		return api.ErrConflict
	}

	s.cancelRuntime(s.takeRuntime(fund.ID, normalizedTradingDate))
	runtime := s.getRuntime(fund, normalizedTradingDate, time.Now(), false)
	if runtime == nil || runtime.orchestrator == nil {
		return api.ErrNotImplemented
	}
	s.restoreRuntimeFromRun(runtime, run)
	runtime.orchestrator.RestoreState(workflow.WorkflowState{
		RunID:       run.ID,
		FundID:      run.FundID,
		TradingDate: normalizedTradingDate.Format("2006-01-02"),
		Status:      workflow.RunStatusPaused,
		CurrentStep: parseWorkflowStepOrZero(run.CurrentStep.String),
		StepResults: decodeWorkflowStepResultsToRuntime(run.StepResults),
		PlanID:      trimmedPlanID,
		StartedAt:   nullTimeValue(run.StartedAt),
		EndedAt:     nullTimeValue(run.CompletedAt),
	})
	go s.resumeApprovedPlan(fund.ID, normalizedTradingDate.Format("2006-01-02"), runtime, trimmedPlanID)
	return nil
}

func (s *workflowServiceAdapter) resumeApprovedPlan(fundID, tradingDate string, runtime *workflowRuntime, planID string) {
	// Use the same ceiling as runFullWorkflow. Approving at e.g. 19:33
	// Beijing time (07:33 ET) for a US fund means waiting ~3.5 h until
	// the 11:00 ET trade-execution window — well beyond the legacy 2 h
	// cap. The new 24 h ceiling also covers the rare case of approving
	// just after a previous-day workflow paused overnight.
	ctx, cancel := context.WithTimeout(context.Background(), workflowRunCeiling)
	defer cancel()
	_, _ = runtime.orchestrator.ResumeApprovedPlan(ctx, tradingDate, planID)
	parsed, err := time.Parse("2006-01-02", strings.TrimSpace(tradingDate))
	if err != nil {
		parsed = workflowTradingDate(time.Now())
	}
	status, _ := s.persistRuntimeStateIfCurrent(fundID, runtime, parsed.UTC())
	if s.metrics != nil && status != nil {
		s.metrics.ObserveWorkflow(fundID, status.State, status.Step)
	}
}

func (s *workflowServiceAdapter) RejectAwaitingPlan(fundID string, tradingDate time.Time, planID, reason string) error {
	if s == nil {
		return api.ErrNotImplemented
	}
	if s.rejectAwaitingPlan != nil {
		return s.rejectAwaitingPlan(fundID, normalizeTradingDate(tradingDate), strings.TrimSpace(planID), reason)
	}
	trimmedPlanID := strings.TrimSpace(planID)
	if trimmedPlanID == "" {
		return api.ErrBadInput
	}
	normalizedTradingDate := normalizeTradingDate(tradingDate)
	if s.planRepo == nil {
		return api.ErrNotImplemented
	}
	plan, err := s.planRepo.GetByID(context.Background(), trimmedPlanID)
	if err != nil {
		return mapRepositoryError(err)
	}
	if plan.FundID != fundID || normalizeTradingDate(plan.TradingDate) != normalizedTradingDate {
		return api.ErrConflict
	}
	run, err := s.workflowRepo.GetByFundAndDate(context.Background(), fundID, normalizedTradingDate)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err != nil {
		return mapRepositoryError(err)
	}
	if !workflowRunAwaitingApproval(run) {
		return nil
	}
	s.cancelRuntime(s.takeRuntime(fundID, normalizedTradingDate))
	stepResults := decodeWorkflowStepResults(run.StepResults)
	approvalStep := stepResults[workflow.StepUserApproval.String()]
	approvalStep.Step = workflow.StepUserApproval.String()
	approvalStep.Status = "rejected"
	approvalStep.Error = strings.TrimSpace(reason)
	approvalStep.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	stepResults[workflow.StepUserApproval.String()] = approvalStep
	encoded, err := json.Marshal(stepResults)
	if err != nil {
		return err
	}
	updated, err := s.saveRun(&repository.WorkflowRun{
		FundID:      fundID,
		TradingDate: normalizedTradingDate,
		Status:      "rejected",
		CurrentStep: sql.NullString{String: workflow.StepUserApproval.String(), Valid: true},
		StepResults: json.RawMessage(encoded),
		StartedAt:   run.StartedAt,
		CompletedAt: sql.NullTime{Time: time.Now().UTC(), Valid: true},
	})
	if err != nil {
		return err
	}
	if s.metrics != nil && updated != nil {
		s.metrics.ObserveWorkflow(fundID, updated.Status, updated.CurrentStep.String)
	}
	return nil
}

func (s *workflowServiceAdapter) stopRuntime(fundID string) {
	s.mu.Lock()
	var runtimes []*workflowRuntime
	prefix := strings.TrimSpace(fundID) + ":"
	for key, runtime := range s.runtimes {
		if strings.HasPrefix(key, prefix) {
			runtimes = append(runtimes, runtime)
			delete(s.runtimes, key)
		}
	}
	s.mu.Unlock()
	for _, runtime := range runtimes {
		s.cancelRuntime(runtime)
	}
}

func (s *workflowServiceAdapter) persistRuntimeStateIfCurrent(fundID string, runtime *workflowRuntime, tradingDate time.Time) (*api.WorkflowStatus, error) {
	if runtime == nil {
		return nil, nil
	}
	current := s.peekRuntime(fundID, tradingDate)
	if current != runtime {
		return nil, nil
	}
	return s.persistRuntimeState(fundID, runtime.orchestrator.State(), tradingDate)
}

func (s *workflowServiceAdapter) persistRuntimeState(fundID string, state *workflow.WorkflowState, tradingDate time.Time) (*api.WorkflowStatus, error) {
	if state == nil {
		return nil, nil
	}
	run := &repository.WorkflowRun{FundID: strings.TrimSpace(fundID), TradingDate: normalizeTradingDate(tradingDate)}
	syncWorkflowRun(run, state.Snapshot())
	merged, err := s.saveRun(run)
	if err != nil {
		return nil, err
	}
	status := convertWorkflowStatus(merged)
	if s.metrics != nil && status != nil {
		s.metrics.ObserveWorkflow(fundID, status.State, status.Step)
	}
	return status, nil
}

func (s *teamServiceAdapter) AddAgent(userID, fundID, role, focus string) (*api.Agent, error) {
	fund, err := s.getAuthorizedFund(userID, fundID)
	if err != nil {
		return nil, err
	}

	normalizedRole, err := normalizeAgentRole(role)
	if err != nil {
		return nil, err
	}
	normalizedFocus, err := normalizeAgentFocus(focus)
	if err != nil {
		return nil, err
	}
	// model_provider / model_name / llm_model are intentionally left
	// NULL on creation: an unset row means "use the .env platform
	// default" (the agents-table SyncAll fallback skips rows with
	// either provider or model_name blank, so router.ResolveModel
	// falls through to defaultModels[tier]). Auto-populating with
	// claude-sonnet-4-6 etc. silently bound new operators to a
	// provider they never picked — and on deployments without an
	// Anthropic key, that was the P2 silent-downgrade we just
	// fixed. The model only gets persisted once the operator picks
	// one through UpdateAgent's ModelConfig path.
	agent := &repository.Agent{
		UserID:          strings.TrimSpace(userID),
		Name:            buildAgentName(normalizedRole, normalizedFocus),
		Role:            normalizedRole,
		Focus:           nullString(normalizedFocus),
		LLMModel:        sql.NullString{},
		ModelProvider:   sql.NullString{},
		ModelName:       sql.NullString{},
		SystemPrompt:    sql.NullString{},
		SkillConfig:     json.RawMessage(`{}`),
		DomainConfig:    json.RawMessage(`{}`),
		EvolutionConfig: json.RawMessage(`{}`),
		Status:          "active",
	}
	agentID, err := s.agentRepo.Create(context.Background(), agent)
	if err != nil {
		return nil, err
	}
	if _, err := s.bindOwnedAgent(context.Background(), userID, fund.ID, agentID); err != nil {
		return nil, err
	}
	return s.getAgent(userID, fund.ID, agentID)
}

func (s *teamServiceAdapter) BindAgent(userID, fundID, agentID string) (*api.Agent, error) {
	fund, err := s.getAuthorizedFund(userID, fundID)
	if err != nil {
		return nil, err
	}
	if _, err := s.bindOwnedAgent(context.Background(), userID, fund.ID, agentID); err != nil {
		return nil, err
	}
	return s.getAgent(userID, fund.ID, agentID)
}

func (s *teamServiceAdapter) ListOwnedAgents(userID, bindStatus string) ([]api.Agent, error) {
	statusFilter := strings.ToLower(strings.TrimSpace(bindStatus))
	if statusFilter == "" {
		statusFilter = "unbound"
	}
	if statusFilter != "all" && statusFilter != "bound" && statusFilter != "unbound" {
		return nil, api.ErrBadInput
	}

	agents, err := s.agentRepo.ListByUser(context.Background(), strings.TrimSpace(userID))
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	activeConfigs, err := s.activeAgentConfigs(userID)
	if err != nil {
		return nil, err
	}

	result := make([]api.Agent, 0, len(agents))
	for i := range agents {
		members, err := s.teamRepo.ListByAgent(context.Background(), agents[i].ID)
		if err != nil {
			return nil, mapRepositoryError(err)
		}
		activeMember := activeMembership(members)
		currentStatus := "unbound"
		if activeMember != nil {
			currentStatus = "bound"
		}
		if statusFilter != "all" && currentStatus != statusFilter {
			continue
		}
		converted := convertOwnedAgent(activeMember, &agents[i])
		applyAgentModelConfig(&converted, activeConfigs[agents[i].ID])
		if activeMember == nil {
			applyPendingMarketplaceSummary(&converted, &agents[i])
		}
		result = append(result, converted)
	}
	return result, nil
}

func (s *teamServiceAdapter) RemoveAgent(userID, fundID, agentID string) error {
	if _, err := s.getAuthorizedFund(userID, fundID); err != nil {
		return err
	}
	if err := s.teamRepo.RemoveMember(context.Background(), fundID, agentID); err != nil {
		return mapRepositoryError(err)
	}
	return nil
}

// GetAgentSpecialization resolves the structured coverage row for
// (fund, agent). Returns (nil, nil) when no row exists — handler
// translates that into an empty-array response so the UI gets a
// consistent shape regardless of whether coverage has been set.
//
// Auth: ownership of the fund (same contract as UpdateAgent).
// We additionally validate the (fund, agent) pair is a real team
// row before touching the specialization table, so an unrelated
// agent_id can't be used to fish coverage rows from other funds.
func (s *teamServiceAdapter) GetAgentSpecialization(userID, fundID, agentID string) (*api.AgentSpecialization, error) {
	if _, err := s.getAuthorizedFund(userID, fundID); err != nil {
		return nil, err
	}
	ctx := context.Background()
	member, err := s.teamRepo.GetMember(ctx, fundID, agentID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	spec, err := s.teamRepo.GetSpecialization(ctx, member.ID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if spec == nil {
		return nil, nil
	}
	return &api.AgentSpecialization{
		FundID:      fundID,
		AgentID:     agentID,
		Instruments: spec.Instruments,
		Themes:      spec.Themes,
		Markets:     spec.Markets,
		UpdatedAt:   spec.UpdatedAt,
	}, nil
}

// UpdateAgentSpecialization upserts the row. Arrays are normalized
// to lower-case here, ONCE — the rest of the pipeline (prompt
// builder, future filters) can rely on case-insensitive comparison
// without rewriting it. UI shows whatever the user typed.
//
// Empty-array writes are a legitimate "clear coverage" signal;
// they intentionally do NOT delete the row, because keeping an
// empty row preserves audit history (updated_at) and lets the
// builder still see "this researcher EXPLICITLY has no
// instruments" — different from "never set up".
func (s *teamServiceAdapter) UpdateAgentSpecialization(userID, fundID, agentID string, body api.AgentSpecialization) (*api.AgentSpecialization, error) {
	if _, err := s.getAuthorizedFund(userID, fundID); err != nil {
		return nil, err
	}
	ctx := context.Background()
	member, err := s.teamRepo.GetMember(ctx, fundID, agentID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	normalize := func(items []string) []string {
		if len(items) == 0 {
			return []string{}
		}
		seen := map[string]struct{}{}
		out := make([]string, 0, len(items))
		for _, raw := range items {
			s := strings.ToLower(strings.TrimSpace(raw))
			if s == "" {
				continue
			}
			if _, ok := seen[s]; ok {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
		return out
	}
	persisted, err := s.teamRepo.UpsertSpecialization(ctx, &repository.TeamMemberSpecialization{
		MemberID:    member.ID,
		Instruments: normalize(body.Instruments),
		Themes:      normalize(body.Themes),
		Markets:     normalize(body.Markets),
	})
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	return &api.AgentSpecialization{
		FundID:      fundID,
		AgentID:     agentID,
		Instruments: persisted.Instruments,
		Themes:      persisted.Themes,
		Markets:     persisted.Markets,
		UpdatedAt:   persisted.UpdatedAt,
	}, nil
}

func (s *teamServiceAdapter) UpdateAgent(userID, fundID, agentID string, cfg api.AgentConfig) (*api.Agent, error) {
	if _, err := s.getAuthorizedFund(userID, fundID); err != nil {
		return nil, err
	}

	member, err := s.teamRepo.GetMember(context.Background(), fundID, agentID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	agent, err := s.agentRepo.GetByID(context.Background(), agentID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	pendingModelConfig, err := s.buildAgentModelConfig(userID, agentID, cfg.ModelConfig)
	if err != nil {
		return nil, err
	}

	if cfg.Role != nil {
		normalizedRole, err := normalizeAgentRole(*cfg.Role)
		if err != nil {
			return nil, err
		}
		member.Role = normalizedRole
		agent.Role = normalizedRole
		// Role changes no longer auto-fill LLMModel. An agent without
		// an explicit model picks up the .env platform default at
		// request time; back-filling here would re-introduce the
		// silent provider lock-in we just removed from AddAgent.
	}
	if cfg.Focus != nil {
		normalizedFocus, err := normalizeAgentFocus(*cfg.Focus)
		if err != nil {
			return nil, err
		}
		member.Focus = nullString(normalizedFocus)
		agent.Focus = nullString(normalizedFocus)
	}
	if agent.Role != "researcher" {
		member.Focus = sql.NullString{}
		agent.Focus = sql.NullString{}
	}
	if cfg.SystemPrompt != nil {
		agent.SystemPrompt = nullString(*cfg.SystemPrompt)
	}
	if cfg.SkillConfig != nil {
		agent.SkillConfig = append(json.RawMessage(nil), (*cfg.SkillConfig)...)
	}
	if cfg.DomainConfig != nil {
		agent.DomainConfig = append(json.RawMessage(nil), (*cfg.DomainConfig)...)
	}
	if cfg.EvolutionConfig != nil {
		agent.EvolutionConfig = append(json.RawMessage(nil), (*cfg.EvolutionConfig)...)
	}
	if pendingModelConfig != nil {
		agent.ModelProvider = nullString(pendingModelConfig.Provider)
		agent.ModelName = nullString(pendingModelConfig.ModelName)
		agent.LLMModel = nullString(pendingModelConfig.ModelName)
	} else {
		provider, modelName := modelDisplayFields(agent.LLMModel)
		if strings.TrimSpace(provider) != "" {
			agent.ModelProvider = nullString(provider)
		}
		if strings.TrimSpace(modelName) != "" {
			agent.ModelName = nullString(modelName)
		}
	}
	agent.Name = buildAgentName(agent.Role, agent.Focus.String)

	if err := s.teamRepo.UpdateMember(context.Background(), member); err != nil {
		return nil, mapRepositoryError(err)
	}
	if err := s.agentRepo.Update(context.Background(), agent); err != nil {
		return nil, mapRepositoryError(err)
	}
	if pendingModelConfig != nil {
		if err := s.modelConfigs.SaveConfig(context.Background(), pendingModelConfig); err != nil {
			return nil, err
		}
		if s.llmRuntime != nil {
			if err := s.llmRuntime.SyncUser(context.Background(), userID); err != nil {
				return nil, err
			}
		}
	}
	return s.getAgent(userID, fundID, agentID)
}

// ListTeamActivity returns the most recent workflow activity events for the
// fund. Enforces fund ownership via the shared getAuthorizedFund helper, so
// non-owners see api.ErrForbidden/api.ErrNotFound instead of a leak of which
// fund IDs exist.
func (s *teamServiceAdapter) ListTeamActivity(userID, fundID string, limit int, sinceSeq uint64) ([]api.TeamActivityItem, error) {
	if _, err := s.getAuthorizedFund(userID, fundID); err != nil {
		return nil, err
	}
	if s.activityBus == nil {
		return []api.TeamActivityItem{}, nil
	}
	events := s.activityBus.Recent(fundID, limit, sinceSeq)
	out := make([]api.TeamActivityItem, len(events))
	for i, e := range events {
		out[i] = activityEventToAPI(e)
	}
	return out, nil
}

// PageTeamActivity returns up to `limit` events strictly older than
// `before`, newest first. Used by the "load earlier" infinite-scroll
// path in the Team Live Activity panel.
//
// When the bus has a persister we go straight to the DB so we can
// always return the requested historical page (the in-memory ring
// only holds the last ~200 events). Without a persister (test paths)
// we fall back to the ring, returning whatever overlaps the cursor.
func (s *teamServiceAdapter) PageTeamActivity(userID, fundID string, before time.Time, limit int) ([]api.TeamActivityItem, error) {
	if _, err := s.getAuthorizedFund(userID, fundID); err != nil {
		return nil, err
	}
	if s.activityBus == nil {
		return []api.TeamActivityItem{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, err := s.activityBus.Page(ctx, fundID, before, limit)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	out := make([]api.TeamActivityItem, len(events))
	for i, e := range events {
		out[i] = activityEventToAPI(e)
	}
	return out, nil
}

// SubscribeTeamActivity opens an SSE-friendly subscription for the fund.
// Caller (handler) is responsible for invoking Stream.Cancel exactly once,
// which is enforced via defer in the SSE handler.
func (s *teamServiceAdapter) SubscribeTeamActivity(userID, fundID string) (*api.TeamActivityStream, error) {
	if _, err := s.getAuthorizedFund(userID, fundID); err != nil {
		return nil, err
	}
	if s.activityBus == nil {
		// Without a bus the SSE endpoint would block forever; return a
		// closed channel so the handler can finalize cleanly with no events
		// (the REST backfill endpoint already returned []).
		ch := make(chan api.TeamActivityItem)
		close(ch)
		return &api.TeamActivityStream{
			Events:       ch,
			Cancel:       func() {},
			DroppedCount: func() uint64 { return 0 },
		}, nil
	}
	sub, err := s.activityBus.Subscribe(fundID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	out := make(chan api.TeamActivityItem, 64)
	stopCh := make(chan struct{})
	go func() {
		defer close(out)
		for {
			select {
			case <-stopCh:
				return
			case evt, alive := <-sub.Events:
				if !alive {
					return
				}
				select {
				case out <- activityEventToAPI(evt):
				case <-stopCh:
					return
				}
			}
		}
	}()
	cancel := func() {
		select {
		case <-stopCh:
			return
		default:
		}
		close(stopCh)
		sub.Cancel()
	}
	return &api.TeamActivityStream{
		Events:       out,
		Cancel:       cancel,
		DroppedCount: sub.DroppedCount,
	}, nil
}

// activityEventToAPI translates the internal workflow.ActivityEvent into the
// JSON-friendly api.TeamActivityItem returned to the UI.
func activityEventToAPI(e workflow.ActivityEvent) api.TeamActivityItem {
	return api.TeamActivityItem{
		Seq:         e.Seq,
		Type:        e.Type,
		Role:        e.Role,
		Step:        e.Step,
		FundID:      e.FundID,
		RunID:       e.RunID,
		TradingDate: e.TradingDate,
		Timestamp:   e.Timestamp,
		Message:     e.Message,
		Error:       e.ErrorMessage,
	}
}

func (s *teamServiceAdapter) ListAgents(userID, fundID string) ([]api.Agent, error) {
	fund, err := s.getAuthorizedFund(userID, fundID)
	if err != nil {
		return nil, err
	}

	members, err := s.teamRepo.ListByFund(context.Background(), fund.ID)
	if err != nil {
		return nil, err
	}
	activeConfigs, err := s.activeAgentConfigs(userID)
	if err != nil {
		return nil, err
	}

	result := make([]api.Agent, 0, len(members))
	for i := range members {
		agent, err := s.agentRepo.GetByID(context.Background(), members[i].AgentID)
		if err != nil {
			return nil, mapRepositoryError(err)
		}
		converted := convertTeamAgent(&members[i], agent)
		applyAgentModelConfig(&converted, activeConfigs[members[i].AgentID])
		if err := s.enrichLatestLearning(context.Background(), fund.ID, &converted); err != nil {
			return nil, err
		}
		result = append(result, converted)
	}
	return result, nil
}

func (s *teamServiceAdapter) GetLLMUsageVisibility(userID, fundID string, from, to time.Time) (*api.LLMUsageVisibility, error) {
	ctx := context.Background()
	fund, err := authorizeFundAccess(ctx, s.fundRepo, s.companyRepo, userID, fundID)
	if err != nil {
		return nil, err
	}
	result := &api.LLMUsageVisibility{
		FundID:      fund.ID,
		From:        from.UTC().Format(time.RFC3339),
		To:          to.UTC().Format(time.RFC3339),
		ByAgent:     []api.LLMUsageBreakdown{},
		ByStep:      []api.LLMUsageBreakdown{},
		ByModel:     []api.LLMUsageBreakdown{},
		RecentCalls: []api.LLMUsageCall{},
	}
	if s.usageTracker == nil {
		return result, nil
	}
	visibility, err := s.usageTracker.GetFundVisibility(ctx, strings.TrimSpace(userID), fund.ID, from.UTC(), to.UTC(), 20)
	if err != nil {
		return nil, err
	}
	if visibility == nil {
		return result, nil
	}
	agentLabels := make(map[string]string)
	resolveAgentName := func(agentID string) string {
		agentID = strings.TrimSpace(agentID)
		if agentID == "" || agentID == "unassigned" {
			return "Unassigned"
		}
		if label, ok := agentLabels[agentID]; ok {
			return label
		}
		agent, err := s.agentRepo.GetByID(ctx, agentID)
		if err != nil || agent == nil {
			agentLabels[agentID] = agentID
			return agentID
		}
		label := strings.TrimSpace(agent.Role)
		if agent.Focus.Valid && strings.TrimSpace(agent.Focus.String) != "" {
			label = strings.TrimSpace(label + " · " + strings.TrimSpace(agent.Focus.String))
		}
		if label == "" {
			label = agent.ID
		}
		agentLabels[agentID] = label
		return label
	}

	result.TotalCalls = visibility.TotalCalls
	result.InputTokens = visibility.InputTokens
	result.OutputTokens = visibility.OutputTokens
	result.TotalTokens = visibility.InputTokens + visibility.OutputTokens
	result.CostCents = visibility.CostCents
	result.PriceCents = visibility.PriceCents
	result.CustomKeyCalls = visibility.CustomKeyCalls
	result.ByAgent = convertUsageBreakdowns(visibility.ByAgent, func(key string) (string, string) {
		if strings.TrimSpace(key) == "unassigned" {
			return "", resolveAgentName(key)
		}
		return key, resolveAgentName(key)
	})
	result.ByStep = convertUsageBreakdowns(visibility.ByStep, func(key string) (string, string) {
		return "", humanizeWorkflowLabel(key)
	})
	result.ByModel = convertUsageBreakdowns(visibility.ByModel, func(key string) (string, string) {
		return "", key
	})
	result.RecentCalls = make([]api.LLMUsageCall, 0, len(visibility.RecentCalls))
	for _, entry := range visibility.RecentCalls {
		if entry == nil {
			continue
		}
		agentID := ""
		agentName := ""
		if entry.AgentID != nil {
			agentID = strings.TrimSpace(*entry.AgentID)
			agentName = resolveAgentName(agentID)
		}
		result.RecentCalls = append(result.RecentCalls, api.LLMUsageCall{
			ID:            entry.ID,
			AgentID:       agentID,
			AgentName:     agentName,
			StepName:      entry.StepName,
			ModelProvider: entry.ModelProvider,
			ModelName:     entry.ModelName,
			InputTokens:   entry.InputTokens,
			OutputTokens:  entry.OutputTokens,
			TotalTokens:   entry.InputTokens + entry.OutputTokens,
			CostCents:     entry.CostCents,
			PriceCents:    entry.PriceCents,
			IsCustomKey:   entry.IsCustomKey,
			CreatedAt:     entry.CreatedAt,
		})
	}
	safeAuditLogAccess(ctx, s.auditLogger, userID, "read", "llm_usage", fund.ID, map[string]any{
		"fundId":      fund.ID,
		"from":        result.From,
		"to":          result.To,
		"totalCalls":  result.TotalCalls,
		"recentCount": len(result.RecentCalls),
	})
	return result, nil
}

func (s *teamServiceAdapter) ListAuditLogs(userID, fundID string, limit int) (*api.AuditLogResponse, error) {
	return s.listAuditLogsWithAction(userID, fundID, limit, "read")
}

func (s *teamServiceAdapter) ExportAuditLogs(userID, fundID string, limit int) (*api.AuditLogResponse, error) {
	return s.listAuditLogsWithAction(userID, fundID, limit, "export")
}

func (s *teamServiceAdapter) listAuditLogsWithAction(userID, fundID string, limit int, auditAction string) (*api.AuditLogResponse, error) {
	ctx := context.Background()
	fund, err := authorizeFundAccess(ctx, s.fundRepo, s.companyRepo, userID, fundID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	result := &api.AuditLogResponse{Entries: []api.AuditLogEntry{}, Limit: limit}
	if s.db == nil {
		return result, nil
	}
	query := `
		SELECT id, COALESCE(actor_user_id::text, ''), action, resource_type, resource_id::text, details, created_at
		FROM data_access_log
		WHERE actor_user_id = $1
		  AND (
		    resource_id::text = $2
		    OR details->>'fundId' = $2
		    OR details->>'fund_id' = $2
		  )
		ORDER BY created_at DESC
		LIMIT $3
	`
	rows, err := s.db.QueryContext(ctx, query, strings.TrimSpace(userID), fund.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("query audit logs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var entry api.AuditLogEntry
		if err := rows.Scan(&entry.ID, &entry.ActorUserID, &entry.Action, &entry.ResourceType, &entry.ResourceID, &entry.Details, &entry.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan audit log: %w", err)
		}
		if len(entry.Details) == 0 || string(entry.Details) == "null" {
			entry.Details = json.RawMessage(`{}`)
		}
		result.Entries = append(result.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audit logs: %w", err)
	}
	safeAuditLogAccess(ctx, s.auditLogger, userID, auditAction, "audit_log", fund.ID, map[string]any{
		"fundId":      fund.ID,
		"limit":       limit,
		"resultCount": len(result.Entries),
	})
	return result, nil
}

func convertUsageBreakdowns(items []subscription.FundUsageBreakdown, labelFor func(string) (string, string)) []api.LLMUsageBreakdown {
	result := make([]api.LLMUsageBreakdown, 0, len(items))
	for _, item := range items {
		agentID, label := labelFor(item.Key)
		result = append(result, api.LLMUsageBreakdown{
			Key:            item.Key,
			Label:          label,
			AgentID:        agentID,
			TotalCalls:     item.TotalCalls,
			InputTokens:    item.InputTokens,
			OutputTokens:   item.OutputTokens,
			TotalTokens:    item.InputTokens + item.OutputTokens,
			CostCents:      item.CostCents,
			PriceCents:     item.PriceCents,
			CustomKeyCalls: item.CustomKeyCalls,
		})
	}
	return result
}

func humanizeWorkflowLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "Unknown"
	}
	value = strings.ReplaceAll(value, "_", " ")
	value = strings.ReplaceAll(value, "-", " ")
	return strings.Title(value)
}

func (s *teamServiceAdapter) GetAgentLearning(userID, agentID string) (*api.AgentLearningStatus, error) {
	ctx := context.Background()
	agent, err := s.agentRepo.GetOwnedByID(ctx, strings.TrimSpace(userID), strings.TrimSpace(agentID))
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	return s.buildAgentLearningStatus(ctx, agent)
}

func (s *teamServiceAdapter) EnableAgentLearning(userID, agentID string, input api.AgentLearningConfigInput) (*api.AgentLearningStatus, error) {
	ctx := context.Background()
	agent, err := s.agentRepo.GetOwnedByID(ctx, strings.TrimSpace(userID), strings.TrimSpace(agentID))
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	config, _, _, _ := parseEvolutionLearningConfig(agent.EvolutionConfig)
	config["dailyLearningEnabled"] = true
	applyLearningConfigInput(config, input)
	updated, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	agent.EvolutionConfig = updated
	if err := s.agentRepo.Update(ctx, agent); err != nil {
		return nil, mapRepositoryError(err)
	}
	return s.buildAgentLearningStatus(ctx, agent)
}

func (s *teamServiceAdapter) DisableAgentLearning(userID, agentID string) (*api.AgentLearningStatus, error) {
	ctx := context.Background()
	agent, err := s.agentRepo.GetOwnedByID(ctx, strings.TrimSpace(userID), strings.TrimSpace(agentID))
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	config, _, _, _ := parseEvolutionLearningConfig(agent.EvolutionConfig)
	config["dailyLearningEnabled"] = false
	config["learningUpdatedAt"] = time.Now().UTC().Format(time.RFC3339)
	updated, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	agent.EvolutionConfig = updated
	if err := s.agentRepo.Update(ctx, agent); err != nil {
		return nil, mapRepositoryError(err)
	}
	return s.buildAgentLearningStatus(ctx, agent)
}

func (s *teamServiceAdapter) UpdateAgentLearningScope(userID, agentID string, scope api.AgentLearningScope) (*api.AgentLearningStatus, error) {
	ctx := context.Background()
	agent, err := s.agentRepo.GetOwnedByID(ctx, strings.TrimSpace(userID), strings.TrimSpace(agentID))
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	config, _, _, _ := parseEvolutionLearningConfig(agent.EvolutionConfig)
	config["learningScope"] = learningScopeToConfig(scope)
	config["learningUpdatedAt"] = time.Now().UTC().Format(time.RFC3339)
	updated, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	agent.EvolutionConfig = updated
	if err := s.agentRepo.Update(ctx, agent); err != nil {
		return nil, mapRepositoryError(err)
	}
	return s.buildAgentLearningStatus(ctx, agent)
}

func (s *teamServiceAdapter) RevokeAgentLearning(userID, agentID string, input api.RevokeAgentLearningInput) (*api.AgentLearningStatus, error) {
	ctx := context.Background()
	agent, err := s.agentRepo.GetOwnedByID(ctx, strings.TrimSpace(userID), strings.TrimSpace(agentID))
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	config, _, _, _ := parseEvolutionLearningConfig(agent.EvolutionConfig)
	now := time.Now().UTC().Format(time.RFC3339)
	for _, key := range []string{"recentLessons", "lastLearningSummary", "lastLearningDate", "lastLearningTags", "lastRecommendedAdjustments", "lastDailyReturn", "specializationLearning"} {
		delete(config, key)
	}
	config["learningRevokedAt"] = now
	if strings.TrimSpace(input.Reason) != "" {
		config["learningRevokedReason"] = strings.TrimSpace(input.Reason)
	} else {
		delete(config, "learningRevokedReason")
	}
	config["learningUpdatedAt"] = now
	updated, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	agent.EvolutionConfig = updated
	if err := s.agentRepo.Update(ctx, agent); err != nil {
		return nil, mapRepositoryError(err)
	}
	return s.buildAgentLearningStatus(ctx, agent)
}

func (s *teamServiceAdapter) GetAgentLineage(userID, agentID string) (*api.AgentLineageTree, error) {
	ctx := context.Background()
	agent, err := s.agentRepo.GetOwnedByID(ctx, strings.TrimSpace(userID), strings.TrimSpace(agentID))
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	root := api.AgentLineageNode{AgentID: agent.ID, AgentName: agent.Name, Role: agent.Role, Focus: agent.Focus.String, OwnerUserID: strings.TrimSpace(userID)}
	if s.lineageRepo != nil {
		visited := map[string]bool{agent.ID: true}
		root.Ancestors = s.buildAgentLineageAncestors(ctx, agent.ID, visited, 1)
	}
	ancestorCount, maxDepth := summarizeAgentLineage(root.Ancestors, 1)
	matryoshkaRisk := lineageTreeHasOwner(root.Ancestors, strings.TrimSpace(userID))
	result := &api.AgentLineageTree{AgentID: agent.ID, Root: root, AncestorCount: ancestorCount, MaxDepth: maxDepth, MatryoshkaRisk: matryoshkaRisk}
	if matryoshkaRisk {
		result.RiskExplanation = "This agent descends from another agent owned by the current user; re-listing it may trigger anti-matryoshka review."
	}
	return result, nil
}

func (s *teamServiceAdapter) buildAgentLineageAncestors(ctx context.Context, agentID string, visited map[string]bool, depth int) []api.AgentLineageNode {
	if s.lineageRepo == nil || depth > 10 {
		return nil
	}
	parents, err := s.lineageRepo.ListParents(ctx, agentID)
	if err != nil {
		return nil
	}
	nodes := make([]api.AgentLineageNode, 0, len(parents))
	for _, parent := range parents {
		if visited[parent.AgentID] {
			continue
		}
		visited[parent.AgentID] = true
		node := api.AgentLineageNode{
			AgentID:         parent.AgentID,
			AgentName:       parent.AgentName,
			Role:            parent.AgentRole,
			Focus:           parent.AgentFocus.String,
			OwnerUserID:     parent.OwnerUserID,
			DerivedVia:      parent.DerivedVia,
			SourceListingID: parent.SourceListingID.String,
			CreatedAt:       parent.CreatedAt,
		}
		node.Ancestors = s.buildAgentLineageAncestors(ctx, parent.AgentID, visited, depth+1)
		nodes = append(nodes, node)
	}
	return nodes
}

func summarizeAgentLineage(nodes []api.AgentLineageNode, depth int) (int, int) {
	count := 0
	maxDepth := 0
	for _, node := range nodes {
		count++
		if depth > maxDepth {
			maxDepth = depth
		}
		childCount, childDepth := summarizeAgentLineage(node.Ancestors, depth+1)
		count += childCount
		if childDepth > maxDepth {
			maxDepth = childDepth
		}
	}
	return count, maxDepth
}

func lineageTreeHasOwner(nodes []api.AgentLineageNode, ownerUserID string) bool {
	if ownerUserID == "" {
		return false
	}
	for _, node := range nodes {
		if strings.TrimSpace(node.OwnerUserID) == ownerUserID {
			return true
		}
		if lineageTreeHasOwner(node.Ancestors, ownerUserID) {
			return true
		}
	}
	return false
}

func (s *teamServiceAdapter) buildAgentLearningStatus(ctx context.Context, agent *repository.Agent) (*api.AgentLearningStatus, error) {
	if agent == nil {
		return nil, api.ErrNotFound
	}
	config, enabled, autoApply, maxLessons := parseEvolutionLearningConfig(agent.EvolutionConfig)
	status := &api.AgentLearningStatus{
		AgentID:              agent.ID,
		AgentName:            agent.Name,
		Role:                 agent.Role,
		Focus:                agent.Focus.String,
		Enabled:              enabled,
		AutoApplyAdjustments: autoApply,
		MaxLessonsPerDay:     maxLessons,
		Scope:                learningScopeFromConfig(config),
		RecentLessons:        stringSliceFromConfig(config, "recentLessons"),
		LastLearningSummary:  stringFromConfig(config, "lastLearningSummary"),
		LastLearningDate:     stringFromConfig(config, "lastLearningDate"),
		LastLearningTags:     stringSliceFromConfig(config, "lastLearningTags"),
		LastAdjustments:      stringSliceFromConfig(config, "lastRecommendedAdjustments"),
		LearningUpdatedAt:    stringFromConfig(config, "learningUpdatedAt"),
		RevokedAt:            stringFromConfig(config, "learningRevokedAt"),
		RevokedReason:        stringFromConfig(config, "learningRevokedReason"),
	}
	if value, ok := floatFromConfig(config, "lastDailyReturn"); ok {
		status.LastDailyReturn = &value
	}
	if s.memoryRepo != nil && s.teamRepo != nil {
		records, err := s.listAgentLearningRecords(ctx, agent.ID, status.RevokedAt, status.RevokedReason)
		if err != nil {
			return nil, err
		}
		status.Records = records
	}
	return status, nil
}

func (s *teamServiceAdapter) listAgentLearningRecords(ctx context.Context, agentID, revokedAt, revokedReason string) ([]api.AgentLearningRecord, error) {
	members, err := s.teamRepo.ListByAgent(ctx, agentID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	seenFunds := map[string]bool{}
	records := []api.AgentLearningRecord{}
	for i := range members {
		fundID := strings.TrimSpace(members[i].FundID)
		if fundID == "" || seenFunds[fundID] {
			continue
		}
		seenFunds[fundID] = true
		memories, err := s.memoryRepo.GetByAgent(ctx, fundID, agentID)
		if err != nil {
			return nil, mapRepositoryError(err)
		}
		for j := range memories {
			if !isLearningMemory(memories[j]) {
				continue
			}
			record := convertAgentLearningRecord(&memories[j])
			record.FundID = fundID
			if revokedAt != "" {
				record.Revoked = true
				record.RevokedAt = revokedAt
				record.RevokedReason = revokedReason
			}
			records = append(records, record)
		}
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].CreatedAt.After(records[j].CreatedAt) })
	if len(records) > 50 {
		records = records[:50]
	}
	return records, nil
}

func (s *teamServiceAdapter) getAgent(userID, fundID, agentID string) (*api.Agent, error) {
	if _, err := s.getAuthorizedFund(userID, fundID); err != nil {
		return nil, err
	}
	member, err := s.teamRepo.GetMember(context.Background(), fundID, agentID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	agent, err := s.agentRepo.GetByID(context.Background(), agentID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	converted := convertTeamAgent(member, agent)
	activeConfigs, err := s.activeAgentConfigs(userID)
	if err != nil {
		return nil, err
	}
	applyAgentModelConfig(&converted, activeConfigs[agentID])
	if err := s.enrichLatestLearning(context.Background(), fundID, &converted); err != nil {
		return nil, err
	}
	return &converted, nil
}

func (s *teamServiceAdapter) bindOwnedAgent(ctx context.Context, userID, fundID, agentID string) (*repository.TeamMember, error) {
	ownedAgent, err := s.agentRepo.GetOwnedByID(ctx, strings.TrimSpace(userID), strings.TrimSpace(agentID))
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	members, err := s.teamRepo.ListByAgent(ctx, ownedAgent.ID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if activeMember := activeMembership(members); activeMember != nil {
		if strings.TrimSpace(activeMember.FundID) == strings.TrimSpace(fundID) {
			return nil, api.ErrConflict
		}
		return nil, api.ErrConflict
	}
	if err := s.checkFundAgentQuota(ctx, userID, fundID); err != nil {
		return nil, err
	}
	member := &repository.TeamMember{
		FundID:  fundID,
		AgentID: ownedAgent.ID,
		Role:    ownedAgent.Role,
		Focus:   ownedAgent.Focus,
		Status:  "active",
	}
	if _, err := s.teamRepo.AddMember(ctx, member); err != nil {
		return nil, mapRepositoryError(err)
	}
	if err := s.importPendingMarketplaceSnapshot(ctx, fundID, ownedAgent); err != nil {
		return nil, err
	}
	return member, nil
}

func (s *teamServiceAdapter) checkFundAgentQuota(ctx context.Context, userID, fundID string) error {
	if s.subscriptionService == nil {
		return nil
	}
	count, err := s.teamRepo.CountByFund(ctx, fundID)
	if err != nil {
		return mapRepositoryError(err)
	}
	if err := s.subscriptionService.CheckQuota(ctx, strings.TrimSpace(userID), "create_agent", count); err != nil {
		return api.ErrConflict
	}
	return nil
}

func (s *teamServiceAdapter) importPendingMarketplaceSnapshot(ctx context.Context, fundID string, agent *repository.Agent) error {
	if agent == nil || len(agent.PendingMarketplaceSnapshot) == 0 || string(agent.PendingMarketplaceSnapshot) == "{}" || agent.MarketplaceSnapshotImportedAt.Valid {
		return nil
	}
	var snapshot marketplaceSnapshot
	if err := json.Unmarshal(agent.PendingMarketplaceSnapshot, &snapshot); err != nil {
		return api.ErrBadInput
	}
	for _, memory := range snapshot.Memories {
		if _, err := s.memoryRepo.Create(ctx, &repository.Memory{
			FundID:      fundID,
			AgentID:     nullString(agent.ID),
			OwnerUserID: nullString(agent.UserID), // The new owner
			Visibility:  "private",                // Imported memories default to private
			Sensitivity: "internal",               // and internal
			OriginKind:  "imported_from_marketplace",
			Layer:       firstNonEmptyValue(memory.Layer, "agent"),
			Title:       nullString(memory.Title),
			Content:     memory.Content,
			TradingDate: parseMarketplaceTradingDate(memory.TradingDate),
			Tags:        append([]string(nil), memory.Tags...),
		}); err != nil {
			return mapRepositoryError(err)
		}
	}
	if err := s.agentRepo.MarkMarketplaceSnapshotImported(ctx, agent.ID); err != nil {
		return mapRepositoryError(err)
	}
	return nil
}

func activeMembership(members []repository.TeamMember) *repository.TeamMember {
	for i := range members {
		if strings.TrimSpace(members[i].Status) == "inactive" {
			continue
		}
		return &members[i]
	}
	return nil
}

func authorizeFundAccess(ctx context.Context, fundRepo *repository.FundRepo, companyRepo *repository.FundCompanyRepo, userID, fundID string) (*repository.Fund, error) {
	fund, err := fundRepo.GetByID(ctx, fundID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if companyRepo == nil {
		return fund, nil
	}
	company, err := companyRepo.GetByID(ctx, fund.CompanyID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if strings.TrimSpace(company.OwnerUserID) != strings.TrimSpace(userID) {
		return nil, api.ErrForbidden
	}
	return fund, nil
}

func authorizeCompanyAccess(ctx context.Context, companyRepo *repository.FundCompanyRepo, userID, companyID string) (*repository.FundCompany, error) {
	company, err := companyRepo.GetByID(ctx, companyID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if strings.TrimSpace(company.OwnerUserID) != strings.TrimSpace(userID) {
		return nil, api.ErrForbidden
	}
	return company, nil
}

func (s *fundServiceAdapter) getAuthorizedCompany(userID, companyID string) (*repository.FundCompany, error) {
	return authorizeCompanyAccess(context.Background(), s.companyRepo, userID, companyID)
}

func (s *fundServiceAdapter) getAuthorizedFund(userID, fundID string) (*repository.Fund, error) {
	return authorizeFundAccess(context.Background(), s.fundRepo, s.companyRepo, userID, fundID)
}

func (s *teamServiceAdapter) getAuthorizedFund(userID, fundID string) (*repository.Fund, error) {
	return authorizeFundAccess(context.Background(), s.fundRepo, s.companyRepo, userID, fundID)
}

func (s *teamServiceAdapter) activeAgentConfigs(userID string) (map[string]*subscription.UserModelConfig, error) {
	result := make(map[string]*subscription.UserModelConfig)
	if s.modelConfigs == nil || strings.TrimSpace(userID) == "" {
		return result, nil
	}
	configs, err := s.modelConfigs.GetUserConfigs(context.Background(), userID)
	if err != nil {
		return nil, err
	}
	for _, cfg := range configs {
		if cfg == nil || !cfg.IsActive || cfg.ConfigType != "agent_default" || cfg.AgentID == nil {
			continue
		}
		agentID := strings.TrimSpace(*cfg.AgentID)
		if agentID == "" {
			continue
		}
		result[agentID] = cfg
	}
	return result, nil
}

func (s *teamServiceAdapter) buildAgentModelConfig(userID, agentID string, cfg *api.AgentModelConfig) (*subscription.UserModelConfig, error) {
	if cfg == nil {
		return nil, nil
	}
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	modelName := strings.TrimSpace(cfg.ModelName)
	if provider == "" || modelName == "" {
		return nil, api.ErrBadInput
	}
	result := &subscription.UserModelConfig{
		UserID:     strings.TrimSpace(userID),
		AgentID:    stringPointer(strings.TrimSpace(agentID)),
		ConfigType: "agent_default",
		Provider:   provider,
		ModelName:  modelName,
		IsActive:   true,
	}
	activeConfigs, err := s.activeAgentConfigs(userID)
	if err != nil {
		return nil, err
	}
	if existing := activeConfigs[strings.TrimSpace(agentID)]; existing != nil {
		result.ID = existing.ID
	}
	if cfg.BaseURL != nil {
		if trimmed := strings.TrimSpace(*cfg.BaseURL); trimmed != "" {
			result.BaseURL = &trimmed
		}
	}
	if cfg.APIKey != nil {
		if trimmed := strings.TrimSpace(*cfg.APIKey); trimmed != "" {
			result.APIKeyEncrypted = &trimmed
		}
	}
	return result, nil
}

func applyAgentModelConfig(agent *api.Agent, cfg *subscription.UserModelConfig) {
	if agent == nil {
		return
	}
	if cfg == nil {
		if strings.TrimSpace(agent.ModelProvider) != "" && strings.TrimSpace(agent.ModelBaseURL) == "" {
			agent.ModelBaseURL = providerDefaultBaseURL(llm.Provider(agent.ModelProvider))
		}
		if strings.TrimSpace(agent.ModelName) != "" {
			agent.LLMModel = agent.ModelName
		}
		return
	}
	agent.HasCustomModelConfig = true
	agent.ModelProvider = cfg.Provider
	agent.ModelName = cfg.ModelName
	agent.LLMModel = cfg.ModelName
	if cfg.BaseURL != nil && strings.TrimSpace(*cfg.BaseURL) != "" {
		agent.ModelBaseURL = strings.TrimSpace(*cfg.BaseURL)
	} else {
		agent.ModelBaseURL = providerDefaultBaseURL(llm.Provider(cfg.Provider))
	}
}

func modelDisplayFields(llmModel sql.NullString) (string, string) {
	modelName := strings.TrimSpace(llmModel.String)
	if !llmModel.Valid || modelName == "" {
		return "", ""
	}
	switch {
	case strings.HasPrefix(modelName, "claude"):
		return string(llm.ProviderClaude), modelName
	case strings.HasPrefix(modelName, "gpt"), strings.HasPrefix(modelName, "o1"), strings.HasPrefix(modelName, "o3"):
		return string(llm.ProviderOpenAI), modelName
	case strings.HasPrefix(modelName, "deepseek"):
		return string(llm.ProviderDeepSeek), modelName
	case strings.HasPrefix(modelName, "qwen"):
		return string(llm.ProviderQwen), modelName
	case strings.HasPrefix(modelName, "gemini"):
		return string(llm.ProviderGemini), modelName
	default:
		return "", modelName
	}
}

func stringPointer(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	trimmed := strings.TrimSpace(value)
	return &trimmed
}

func safeAuditLogAccess(ctx context.Context, logger audit.Logger, actorUserID, action, resourceType, resourceID string, details map[string]any) {
	if logger == nil {
		return
	}
	if err := logger.LogAccess(ctx, actorUserID, action, resourceType, resourceID, details); err != nil {
		slog.Warn("audit log write failed", "action", action, "resource_type", resourceType, "resource_id", resourceID, "error", err)
	}
}

func truncateAuditDetail(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	if maxRunes <= 0 || value == "" {
		return value
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes]) + "…"
}

func (s *memoryServiceAdapter) GetMemory(userID, fundID, layer, agentID string) (*api.MemoryContext, error) {
	fund, err := authorizeFundAccess(context.Background(), s.fundRepo, s.companyRepo, userID, fundID)
	if err != nil {
		return nil, err
	}

	normalizedLayer, err := normalizeMemoryLayer(layer)
	if err != nil {
		return nil, err
	}
	trimmedAgentID := strings.TrimSpace(agentID)

	var memories []repository.Memory
	if trimmedAgentID != "" {
		memories, err = s.memoryRepo.GetByAgent(context.Background(), fund.ID, trimmedAgentID)
		if err != nil {
			return nil, mapRepositoryError(err)
		}
		filtered := memories[:0]
		for _, memory := range memories {
			if memory.Layer == normalizedLayer {
				filtered = append(filtered, memory)
			}
		}
		memories = filtered
	} else {
		memories, err = s.memoryRepo.ListByFund(context.Background(), fund.ID, normalizedLayer, 100)
		if err != nil {
			return nil, mapRepositoryError(err)
		}
	}
	entries := make([]api.MemoryEntry, 0, len(memories))
	for i := range memories {
		entries = append(entries, convertMemoryEntry(&memories[i]))
	}
	safeAuditLogAccess(context.Background(), s.auditLogger, userID, "read", "memory", fund.ID, map[string]any{
		"fundId":      fund.ID,
		"layer":       normalizedLayer,
		"agentId":     trimmedAgentID,
		"resultCount": len(entries),
	})
	return &api.MemoryContext{FundID: fund.ID, AgentID: trimmedAgentID, Layer: normalizedLayer, Entries: entries}, nil
}

func (s *memoryServiceAdapter) SearchMemory(userID, fundID, layer, query string) ([]api.MemoryEntry, error) {
	fund, err := authorizeFundAccess(context.Background(), s.fundRepo, s.companyRepo, userID, fundID)
	if err != nil {
		return nil, err
	}

	normalizedLayer, err := normalizeMemoryLayer(layer)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(query) == "" {
		return nil, api.ErrBadInput
	}

	memories, err := s.memoryRepo.Search(context.Background(), fund.ID, normalizedLayer, strings.TrimSpace(query))
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	entries := make([]api.MemoryEntry, 0, len(memories))
	for i := range memories {
		entries = append(entries, convertMemoryEntry(&memories[i]))
	}
	safeAuditLogAccess(context.Background(), s.auditLogger, userID, "search", "memory", fund.ID, map[string]any{
		"fundId":      fund.ID,
		"layer":       normalizedLayer,
		"query":       truncateAuditDetail(strings.TrimSpace(query), 200),
		"resultCount": len(entries),
	})
	return entries, nil
}

func (s *marketServiceAdapter) GetQuotes(userID, fundID string, symbols []string) (*api.FundMarketQuotes, error) {
	fund, err := authorizeFundAccess(context.Background(), s.fundRepo, s.companyRepo, userID, fundID)
	if err != nil {
		return nil, err
	}
	profile := decodeFundMarketProfile(fund.Config)
	instruments := buildMarketQueryInstruments(fund, profile, symbols)
	quotes := s.marketData.GetQuotes(context.Background(), instruments)
	result := &api.FundMarketQuotes{FundID: fund.ID, Quotes: make([]api.MarketQuote, 0, len(quotes))}
	for _, instrument := range instruments {
		quote := quotes[strings.ToUpper(strings.TrimSpace(instrument.Symbol))]
		if quote == nil {
			continue
		}
		result.Quotes = append(result.Quotes, convertMarketQuote(quote))
	}
	return result, nil
}

func (s *marketServiceAdapter) GetResearch(userID, fundID, symbol string, limit int) (*api.MarketResearch, error) {
	fund, err := authorizeFundAccess(context.Background(), s.fundRepo, s.companyRepo, userID, fundID)
	if err != nil {
		return nil, err
	}
	instrument := marketQueryInstrument(fund, strings.TrimSpace(symbol))
	if strings.TrimSpace(instrument.Symbol) == "" {
		return nil, api.ErrBadInput
	}
	profile := decodeFundMarketProfile(fund.Config)
	benchmark, benchmarkOK := benchmarkInstrumentRef(profile)
	research, err := s.marketData.GetResearchContext(context.Background(), instrument, benchmarkPointer(benchmark, benchmarkOK), limit)
	if err != nil {
		return nil, err
	}
	return convertMarketResearchWithLocale(userID, s.llmRuntime, research), nil
}

func (s *marketServiceAdapter) GetNews(userID, fundID, symbol string, limit int) (*api.FundMarketNews, error) {
	fund, err := authorizeFundAccess(context.Background(), s.fundRepo, s.companyRepo, userID, fundID)
	if err != nil {
		return nil, err
	}
	instrument := marketQueryInstrument(fund, strings.TrimSpace(symbol))
	if strings.TrimSpace(instrument.Symbol) == "" {
		return nil, api.ErrBadInput
	}
	items, err := s.marketData.GetNews(context.Background(), instrument, limit)
	if err != nil {
		return nil, err
	}
	return &api.FundMarketNews{FundID: fund.ID, Symbol: instrument.NormalizedSymbol(), Items: convertMarketNewsItemsWithLocale(userID, s.llmRuntime, items)}, nil
}

func (s *marketServiceAdapter) GetNewsDigest(userID, fundID string, symbols []string, limit int) (*api.MarketNewsDigest, error) {
	fund, err := authorizeFundAccess(context.Background(), s.fundRepo, s.companyRepo, userID, fundID)
	if err != nil {
		return nil, err
	}
	profile := decodeFundMarketProfile(fund.Config)
	teamSpecialization := s.digestTeamSpecialization(context.Background(), fund.ID)
	instruments := buildHybridMarketNewsQueries(fund, profile, symbols, teamSpecialization)
	if len(instruments) == 0 {
		return &api.MarketNewsDigest{FundID: fund.ID, GeneratedAt: time.Now().UTC()}, nil
	}
	perSymbolLimit := limit
	if perSymbolLimit <= 0 {
		perSymbolLimit = 3
	}
	if perSymbolLimit > 5 {
		perSymbolLimit = 5
	}
	resolvedSymbols := digestTickerSymbols(instruments)
	newsBySymbol := make([][]marketdata.NewsItem, len(instruments))
	notesBySymbol := make([][]string, len(instruments))
	failed := make([]bool, len(instruments))
	var wg sync.WaitGroup
	limiter := make(chan struct{}, 3)
	for idx := range instruments {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			limiter <- struct{}{}
			defer func() { <-limiter }()
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			news, notes, err := s.marketData.GetNewsWithNotes(ctx, instruments[i], perSymbolLimit)
			queryLabel := marketNewsQueryLabel(instruments[i])
			if len(notes) > 0 {
				for _, note := range notes {
					notesBySymbol[i] = append(notesBySymbol[i], fmt.Sprintf("%s: %s", queryLabel, note))
				}
			}
			if err != nil {
				failed[i] = true
				notesBySymbol[i] = append(notesBySymbol[i], fmt.Sprintf("%s: %v", queryLabel, err))
				return
			}
			newsBySymbol[i] = tagDigestNewsItems(news, instruments[i])
		}(idx)
	}
	wg.Wait()
	mergedNews := make([]marketdata.NewsItem, 0, len(instruments)*perSymbolLimit)
	providerNotes := make([]string, 0, len(instruments))
	seenTitles := map[string]struct{}{}
	failures := 0
	now := time.Now().UTC()
	for i := range instruments {
		providerNotes = append(providerNotes, notesBySymbol[i]...)
		if failed[i] {
			failures++
		}
		for _, item := range newsBySymbol[i] {
			if isStaleMarketNewsItem(item, now, marketNewsDigestMaxAge) {
				continue
			}
			key := marketNewsDigestItemKey(item)
			if key == "" {
				continue
			}
			if _, ok := seenTitles[key]; ok {
				continue
			}
			seenTitles[key] = struct{}{}
			mergedNews = append(mergedNews, item)
		}
	}
	sort.SliceStable(mergedNews, func(i, j int) bool {
		left := mergedNews[i].PublishedAt
		right := mergedNews[j].PublishedAt
		if left.Equal(right) {
			return mergedNews[i].Title < mergedNews[j].Title
		}
		if left.IsZero() {
			return false
		}
		if right.IsZero() {
			return true
		}
		return left.After(right)
	})
	items := convertMarketNewsItemsWithLocale(userID, s.llmRuntime, mergedNews)
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	if len(items) == 0 && failures == len(instruments) {
		return nil, fmt.Errorf("%w: market news providers unavailable", api.ErrUpstreamUnavailable)
	}
	return &api.MarketNewsDigest{FundID: fund.ID, Symbols: resolvedSymbols, Items: items, ProviderNotes: providerNotes, GeneratedAt: now}, nil
}

type abTestServiceAdapter struct {
	db        *sql.DB
	funds     *repository.FundRepo
	companies *repository.FundCompanyRepo
	tests     *repository.ABTestRepo
	trades    *repository.TradeRepo
	navs      *repository.NavSnapshotRepo

	// Card K-1: pluggable B-side decider for `strategy_compare`
	// AB tests. nil-safe — falls back to deterministic in
	// ensureABShadowExecution when unset (e.g. legacy callers
	// that built the adapter through newABTestServiceAdapter
	// without wiring an LLM client).
	bSideDecider abShadowBSideDecider
}

const (
	abTestVariableModelChange     = "model_change"
	abTestVariableStrategyCompare = "strategy_compare"
	abLearningModeShadowEphemeral = "shadow_ephemeral"
	abPromotionModeMerge          = "merge"
	abPromotionModeOverwrite      = "overwrite"
)

func newABTestServiceAdapter(db *sql.DB) *abTestServiceAdapter {
	return &abTestServiceAdapter{
		db:           db,
		funds:        repository.NewFundRepo(db),
		companies:    repository.NewFundCompanyRepo(db),
		tests:        repository.NewABTestRepo(db),
		trades:       repository.NewTradeRepo(db),
		navs:         repository.NewNavSnapshotRepo(db),
		bSideDecider: deterministicBSideDecider{},
	}
}

// WithLLMShadowDecider opts the AB shadow execution path into the
// real-LLM mode (Card K-1). nil-safe — passing nil leaves the
// existing deterministic decider in place. Operators control the
// switch via env in main.go (AB_SHADOW_LLM_ENABLED=1).
//
// We accept the narrow llm.LLMClient surface instead of the full
// *llmRuntime so this adapter stays unit-testable: a stub
// LLMClient is enough to drive the LLM path in tests.
//
// Card K-5: an optional `metrics` recorder lets the LLM decider
// publish per-outcome counters to Prometheus
// (`fundai_ab_shadow_llm_calls_total`). Passing nil keeps the
// noop recorder so unit tests don't have to wire metrics.
func (s *abTestServiceAdapter) WithLLMShadowDecider(client llm.LLMClient, metrics abShadowMetricsRecorder) *abTestServiceAdapter {
	if s == nil || client == nil {
		return s
	}
	d, err := newLLMBSideDecider(client)
	if err != nil || d == nil {
		// Misconfigured — log? Not worth a hard error: the
		// shadow path simply continues with the deterministic
		// fallback, which is better than refusing to start.
		return s
	}
	s.bSideDecider = d.WithMetrics(metrics)
	return s
}

func (s *abTestServiceAdapter) ListTests(userID, fundID string) ([]api.ABTest, error) {
	if _, err := authorizeFundAccess(context.Background(), s.funds, s.companies, userID, fundID); err != nil {
		return nil, err
	}

	tests, err := s.tests.ListByFund(context.Background(), fundID, 100)
	if err != nil {
		return nil, mapRepositoryError(err)
	}

	result := make([]api.ABTest, 0, len(tests))
	for i := range tests {
		converted := convertABTest(&tests[i])
		if converted != nil {
			result = append(result, *converted)
		}
	}
	return result, nil
}

func (s *abTestServiceAdapter) CreateTest(userID string, input api.CreateABTestInput) (*api.ABTest, error) {
	variableType := strings.TrimSpace(input.VariableType)
	if variableType == "" {
		variableType = abTestVariableStrategyCompare
	}
	if variableType != abTestVariableModelChange && variableType != abTestVariableStrategyCompare {
		return nil, api.ErrNotImplemented
	}
	controlFundID := strings.TrimSpace(input.ControlFundID)
	treatmentFundID := strings.TrimSpace(input.TreatmentFundID)
	if controlFundID == "" {
		return nil, api.ErrBadInput
	}
	if treatmentFundID == "" && variableType == abTestVariableStrategyCompare {
		treatmentFundID = controlFundID
	}
	if treatmentFundID == "" {
		return nil, api.ErrBadInput
	}
	if controlFundID == treatmentFundID && variableType != abTestVariableStrategyCompare {
		return nil, api.ErrBadInput
	}
	if !json.Valid(input.VariableConfig) {
		return nil, api.ErrBadInput
	}
	if strings.TrimSpace(input.Name) == "" {
		return nil, api.ErrBadInput
	}
	if _, err := authorizeFundAccess(context.Background(), s.funds, s.companies, userID, controlFundID); err != nil {
		return nil, err
	}
	if treatmentFundID != controlFundID {
		if _, err := authorizeFundAccess(context.Background(), s.funds, s.companies, userID, treatmentFundID); err != nil {
			return nil, err
		}
	}

	startDate, err := parseABTestDate(input.StartDate)
	if err != nil {
		return nil, api.ErrBadInput
	}
	endDate, err := parseABTestDate(input.EndDate)
	if err != nil {
		return nil, api.ErrBadInput
	}
	if startDate.Valid && endDate.Valid && startDate.Time.After(endDate.Time) {
		return nil, api.ErrBadInput
	}

	test := &repository.ABTest{
		Name:            strings.TrimSpace(input.Name),
		ControlFundID:   controlFundID,
		TreatmentFundID: treatmentFundID,
		VariableType:    variableType,
		VariableConfig:  input.VariableConfig,
		Status:          "draft",
		StartDate:       startDate,
		EndDate:         endDate,
	}
	id, err := s.tests.Create(context.Background(), test)
	if err != nil {
		return nil, err
	}
	if variableType == abTestVariableStrategyCompare {
		if err := s.createABShadowVariants(context.Background(), id, controlFundID, input.VariableConfig); err != nil {
			return nil, err
		}
	}
	return s.GetTest(userID, id)
}

type abStrategyVariantConfig struct {
	Name           string
	StrategyConfig map[string]any
}

type abTeamSnapshot struct {
	FundID            string                 `json:"fundId"`
	LearningIsolation string                 `json:"learningIsolation"`
	PersistLearning   bool                   `json:"persistLearning"`
	ActiveLearning    bool                   `json:"activeLearning"`
	MemoryScope       string                 `json:"memoryScope"`
	Members           []abTeamSnapshotMember `json:"members"`
}

type abTeamSnapshotMember struct {
	MemberID        string          `json:"memberId"`
	AgentID         string          `json:"agentId"`
	Role            string          `json:"role"`
	Focus           string          `json:"focus,omitempty"`
	AgentName       string          `json:"agentName,omitempty"`
	ModelProvider   string          `json:"modelProvider,omitempty"`
	ModelName       string          `json:"modelName,omitempty"`
	SystemPrompt    string          `json:"systemPrompt,omitempty"`
	SkillConfig     json.RawMessage `json:"skillConfig,omitempty"`
	DomainConfig    json.RawMessage `json:"domainConfig,omitempty"`
	EvolutionConfig json.RawMessage `json:"evolutionConfig,omitempty"`
	Status          string          `json:"status"`
	JoinedAt        string          `json:"joinedAt,omitempty"`
	UpdatedAt       string          `json:"updatedAt,omitempty"`
}

func (s *abTestServiceAdapter) createABShadowVariants(ctx context.Context, testID, fundID string, variableConfig json.RawMessage) error {
	teamSnapshot, err := s.buildABTeamSnapshot(ctx, fundID)
	if err != nil {
		return err
	}
	teamSnapshotJSON, err := json.Marshal(teamSnapshot)
	if err != nil {
		return err
	}
	variantA, variantB := extractABStrategyVariantConfig(variableConfig)
	for _, variant := range []struct {
		Key    string
		Config abStrategyVariantConfig
	}{
		{Key: "A", Config: variantA},
		{Key: "B", Config: variantB},
	} {
		strategyConfigJSON, err := json.Marshal(variant.Config.StrategyConfig)
		if err != nil {
			return err
		}
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO ab_test_variants (test_id, variant_key, name, strategy_config, team_snapshot, initial_cash, initial_positions)
			VALUES ($1, $2, $3, $4, $5, 0, '[]')
			ON CONFLICT (test_id, variant_key) DO UPDATE SET
			  name = EXCLUDED.name,
			  strategy_config = EXCLUDED.strategy_config,
			  team_snapshot = EXCLUDED.team_snapshot`, testID, variant.Key, variant.Config.Name, strategyConfigJSON, teamSnapshotJSON)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *abTestServiceAdapter) buildABTeamSnapshot(ctx context.Context, fundID string) (abTeamSnapshot, error) {
	snapshot := abTeamSnapshot{
		FundID:            fundID,
		LearningIsolation: abLearningModeShadowEphemeral,
		PersistLearning:   false,
		ActiveLearning:    true,
		MemoryScope:       "ab_test_variant",
		Members:           []abTeamSnapshotMember{},
	}
	members, err := repository.NewTeamRepo(s.db).ListByFund(ctx, fundID)
	if err != nil {
		return snapshot, err
	}
	agents := repository.NewAgentRepo(s.db)
	for _, member := range members {
		item := abTeamSnapshotMember{
			MemberID:  member.ID,
			AgentID:   member.AgentID,
			Role:      member.Role,
			Focus:     strings.TrimSpace(member.Focus.String),
			Status:    member.Status,
			JoinedAt:  member.JoinedAt.Format(time.RFC3339),
			UpdatedAt: member.UpdatedAt.Format(time.RFC3339),
		}
		agent, err := agents.GetByID(ctx, member.AgentID)
		if err != nil && !errors.Is(err, repository.ErrNotFound) {
			return snapshot, err
		}
		if agent != nil {
			item.AgentName = agent.Name
			item.ModelProvider = strings.TrimSpace(agent.ModelProvider.String)
			item.ModelName = strings.TrimSpace(agent.ModelName.String)
			item.SystemPrompt = strings.TrimSpace(agent.SystemPrompt.String)
			item.SkillConfig = append(json.RawMessage(nil), agent.SkillConfig...)
			item.DomainConfig = append(json.RawMessage(nil), agent.DomainConfig...)
			item.EvolutionConfig = append(json.RawMessage(nil), agent.EvolutionConfig...)
		}
		snapshot.Members = append(snapshot.Members, item)
	}
	return snapshot, nil
}

func extractABStrategyVariantConfig(raw json.RawMessage) (abStrategyVariantConfig, abStrategyVariantConfig) {
	variantA := abStrategyVariantConfig{Name: "当前策略", StrategyConfig: map[string]any{"source": "current_fund"}}
	variantB := abStrategyVariantConfig{Name: "实验策略", StrategyConfig: map[string]any{}}
	var payload map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &payload) != nil {
		return variantA, variantB
	}
	if parsed := parseABVariantConfig(payload["variantA"], variantA); parsed.Name != "" {
		variantA = parsed
	}
	if parsed := parseABVariantConfig(payload["variantB"], variantB); parsed.Name != "" {
		variantB = parsed
	}
	if summary, ok := payload["strategySummary"].(string); ok && strings.TrimSpace(summary) != "" {
		if variantB.StrategyConfig == nil {
			variantB.StrategyConfig = map[string]any{}
		}
		if _, exists := variantB.StrategyConfig["summary"]; !exists {
			variantB.StrategyConfig["summary"] = strings.TrimSpace(summary)
		}
	}
	variantA.StrategyConfig["learningMode"] = abLearningModeShadowEphemeral
	variantB.StrategyConfig["learningMode"] = abLearningModeShadowEphemeral
	variantA.StrategyConfig["persistLearning"] = false
	variantB.StrategyConfig["persistLearning"] = false
	variantA.StrategyConfig["shadowMemoryScope"] = "ab_test_variant"
	variantB.StrategyConfig["shadowMemoryScope"] = "ab_test_variant"
	return variantA, variantB
}

func parseABVariantConfig(raw any, fallback abStrategyVariantConfig) abStrategyVariantConfig {
	result := fallback
	object, ok := raw.(map[string]any)
	if !ok {
		return result
	}
	if name, ok := object["name"].(string); ok && strings.TrimSpace(name) != "" {
		result.Name = strings.TrimSpace(name)
	}
	if strategy, ok := object["strategyConfig"].(map[string]any); ok {
		result.StrategyConfig = copyStringAnyMap(strategy)
	}
	if result.StrategyConfig == nil {
		result.StrategyConfig = map[string]any{}
	}
	return result
}

func copyStringAnyMap(input map[string]any) map[string]any {
	result := make(map[string]any, len(input))
	for key, value := range input {
		trimmed := strings.TrimSpace(key)
		if trimmed != "" {
			result[trimmed] = value
		}
	}
	return result
}

func (s *abTestServiceAdapter) GetTest(userID, testID string) (*api.ABTest, error) {
	test, err := s.tests.GetByID(context.Background(), testID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if _, err := authorizeFundAccess(context.Background(), s.funds, s.companies, userID, test.ControlFundID); err != nil {
		return nil, err
	}
	if _, err := authorizeFundAccess(context.Background(), s.funds, s.companies, userID, test.TreatmentFundID); err != nil {
		return nil, err
	}
	return convertABTest(test), nil
}

func (s *abTestServiceAdapter) StartTest(userID, testID string) (*api.ABTest, error) {
	test, err := s.tests.GetByID(context.Background(), testID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if _, err := authorizeFundAccess(context.Background(), s.funds, s.companies, userID, test.ControlFundID); err != nil {
		return nil, err
	}
	if _, err := authorizeFundAccess(context.Background(), s.funds, s.companies, userID, test.TreatmentFundID); err != nil {
		return nil, err
	}
	if !isSupportedABTestVariable(test.VariableType) {
		return nil, api.ErrNotImplemented
	}
	if test.Status != "draft" {
		return nil, api.ErrConflict
	}
	if err := s.tests.UpdateStatus(context.Background(), testID, "running"); err != nil {
		return nil, mapRepositoryError(err)
	}
	if test.VariableType == abTestVariableStrategyCompare {
		if err := s.ensureABShadowExecution(context.Background(), test); err != nil {
			return nil, err
		}
	}
	return s.GetTest(userID, testID)
}

func (s *abTestServiceAdapter) StopTest(userID, testID string) (*api.ABTest, error) {
	test, err := s.tests.GetByID(context.Background(), testID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if _, err := authorizeFundAccess(context.Background(), s.funds, s.companies, userID, test.ControlFundID); err != nil {
		return nil, err
	}
	if _, err := authorizeFundAccess(context.Background(), s.funds, s.companies, userID, test.TreatmentFundID); err != nil {
		return nil, err
	}
	if !isSupportedABTestVariable(test.VariableType) {
		return nil, api.ErrNotImplemented
	}
	if test.Status != "running" {
		return nil, api.ErrConflict
	}
	if err := s.tests.UpdateStatus(context.Background(), testID, "completed"); err != nil {
		return nil, mapRepositoryError(err)
	}
	return s.GetTest(userID, testID)
}

func (s *abTestServiceAdapter) AnalyzeTest(userID, testID string) (*api.ABTest, error) {
	test, err := s.tests.GetByID(context.Background(), testID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if _, err := authorizeFundAccess(context.Background(), s.funds, s.companies, userID, test.ControlFundID); err != nil {
		return nil, err
	}
	if _, err := authorizeFundAccess(context.Background(), s.funds, s.companies, userID, test.TreatmentFundID); err != nil {
		return nil, err
	}
	if !isSupportedABTestVariable(test.VariableType) {
		return nil, api.ErrNotImplemented
	}
	if test.Status == "draft" || test.Status == "running" {
		return nil, api.ErrConflict
	}
	if test.VariableType == abTestVariableStrategyCompare {
		if err := s.ensureABShadowExecution(context.Background(), test); err != nil {
			return nil, err
		}
	}

	results, err := s.buildABTestResults(test)
	if err != nil {
		return nil, err
	}
	resultsJSON, err := json.Marshal(results)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE ab_tests SET status = $1, results = $2, updated_at = NOW() WHERE id = $3`,
		"analyzed", resultsJSON, testID,
	); err != nil {
		return nil, err
	}
	return s.GetTest(userID, testID)
}

func (s *abTestServiceAdapter) PromoteLearning(userID, testID string, input api.PromoteABTestLearningInput) (*api.ABTestLearningPromotionResult, error) {
	ctx := context.Background()
	test, err := s.tests.GetByID(ctx, testID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if _, err := authorizeFundAccess(ctx, s.funds, s.companies, userID, test.ControlFundID); err != nil {
		return nil, err
	}
	if _, err := authorizeFundAccess(ctx, s.funds, s.companies, userID, test.TreatmentFundID); err != nil {
		return nil, err
	}
	if test.VariableType != abTestVariableStrategyCompare {
		return nil, api.ErrNotImplemented
	}
	requireAnalyzed := true
	if input.RequireAnalyzed != nil {
		requireAnalyzed = *input.RequireAnalyzed
	}
	if requireAnalyzed && test.Status != "analyzed" {
		return nil, api.ErrConflict
	}
	mode := normalizeABPromotionMode(input.Mode)
	if mode == "" {
		return nil, api.ErrBadInput
	}
	variantKey, err := s.resolveABPromotionVariantKey(test, input.VariantKey)
	if err != nil {
		return nil, err
	}
	variantID, teamSnapshot, err := s.loadABVariantSnapshot(ctx, testID, variantKey)
	if err != nil {
		return nil, err
	}
	learningByAgent, err := s.loadABShadowAgentLearning(ctx, testID, variantID)
	if err != nil {
		return nil, err
	}
	selectedAgents := stringSet(input.AgentIDs)
	agents := repository.NewAgentRepo(s.db)
	teamRepo := repository.NewTeamRepo(s.db)
	result := &api.ABTestLearningPromotionResult{
		TestID:        testID,
		VariantKey:    variantKey,
		Mode:          mode,
		DryRun:        input.DryRun,
		UpdatedAgents: []api.ABTestPromotedAgent{},
		SkippedAgents: []api.ABTestPromotionSkip{},
	}
	for _, member := range teamSnapshot.Members {
		agentID := strings.TrimSpace(member.AgentID)
		if agentID == "" {
			continue
		}
		if len(selectedAgents) > 0 {
			if _, ok := selectedAgents[agentID]; !ok {
				continue
			}
		}
		learning, ok := learningByAgent[agentID]
		if !ok || learning.EventCount == 0 {
			result.SkippedAgents = append(result.SkippedAgents, api.ABTestPromotionSkip{AgentID: agentID, Reason: "no_shadow_learning_events"})
			continue
		}
		if _, err := teamRepo.GetMember(ctx, test.ControlFundID, agentID); err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				result.SkippedAgents = append(result.SkippedAgents, api.ABTestPromotionSkip{AgentID: agentID, Reason: "agent_not_in_current_team"})
				continue
			}
			return nil, err
		}
		agent, err := agents.GetByID(ctx, agentID)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				result.SkippedAgents = append(result.SkippedAgents, api.ABTestPromotionSkip{AgentID: agentID, Reason: "agent_not_found"})
				continue
			}
			return nil, err
		}
		previousConfig := append(json.RawMessage(nil), agent.EvolutionConfig...)
		promotedConfig, err := buildPromotedABEvolutionConfig(previousConfig, learning, mode, testID, variantKey)
		if err != nil {
			return nil, err
		}

		// Build the *projected* reflection + skill payloads up-front so a
		// dry-run can report what _would_ be created without touching the
		// DB. The same builders are reused inside the transaction below to
		// guarantee dry-run/real-run parity.
		projectedMemories := buildPromotedReflectionMemories(test.ControlFundID, agentID, learning, testID, variantKey)
		projectedSkills := buildPromotedSkillCandidates(learning, member.Role, testID, variantKey)
		previousSkillConfig := append(json.RawMessage(nil), agent.SkillConfig...)

		var promotedReflectionIDs []string
		var promotedSkillKeys []string
		if !input.DryRun {
			outcome, err := s.applyABLearningPromotion(ctx, abLearningPromotionInput{
				TestID:              testID,
				VariantID:           variantID,
				AgentID:             agentID,
				UserID:              userID,
				Mode:                mode,
				PreviousConfig:      previousConfig,
				PromotedConfig:      promotedConfig,
				PreviousSkillConfig: previousSkillConfig,
				ProjectedMemories:   projectedMemories,
				ProjectedSkills:     projectedSkills,
			})
			if err != nil {
				return nil, err
			}
			promotedReflectionIDs = outcome.PromotedMemoryIDs
			promotedSkillKeys = outcome.PromotedSkillKeys
		} else {
			// Dry-run echo: report the *would-create* keys/titles so the
			// caller can preview the rollout before clicking "promote".
			for _, m := range projectedMemories {
				promotedReflectionIDs = append(promotedReflectionIDs, m.Title.String)
			}
			for _, sk := range projectedSkills {
				promotedSkillKeys = append(promotedSkillKeys, sk.Key)
			}
		}

		result.UpdatedAgents = append(result.UpdatedAgents, api.ABTestPromotedAgent{
			AgentID:               agentID,
			AgentName:             agent.Name,
			Role:                  member.Role,
			AppliedMode:           mode,
			LessonCount:           len(learning.Lessons),
			LearningEventCount:    learning.EventCount,
			LatestTradingDate:     learning.LatestTradingDate,
			Lessons:               limitStrings(learning.Lessons, 5),
			Adjustments:           limitStrings(learning.Adjustments, 5),
			PromotedReflectionIDs: promotedReflectionIDs,
			PromotedSkillKeys:     promotedSkillKeys,
		})
	}
	if len(result.UpdatedAgents) == 0 {
		result.Warnings = append(result.Warnings, "no agent learning was promoted; shadow learning may not have produced events yet")
	}
	return result, nil
}

func (s *abTestServiceAdapter) ListLearningPromotions(userID, testID string) ([]api.ABTestLearningPromotion, error) {
	ctx := context.Background()
	if _, err := s.authorizeABTest(ctx, userID, testID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.test_id, v.variant_key, v.name, p.agent_id, COALESCE(a.name, ''), p.mode,
		       p.previous_config, p.promoted_config, COALESCE(p.promoted_by::text, ''), p.promoted_at
		FROM ab_test_learning_promotions p
		JOIN ab_test_variants v ON v.id = p.variant_id
		LEFT JOIN agents a ON a.id = p.agent_id
		WHERE p.test_id = $1
		ORDER BY p.promoted_at DESC
		LIMIT 100`, testID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	promotions := []api.ABTestLearningPromotion{}
	for rows.Next() {
		var item api.ABTestLearningPromotion
		if err := rows.Scan(&item.ID, &item.TestID, &item.VariantKey, &item.VariantName, &item.AgentID, &item.AgentName, &item.Mode, &item.PreviousConfig, &item.PromotedConfig, &item.PromotedBy, &item.PromotedAt); err != nil {
			return nil, err
		}
		promotions = append(promotions, item)
	}
	return promotions, rows.Err()
}

// RollbackLearningPromotion reverses everything applyABLearningPromotion
// wrote for a single promotions row — evolution_config, skill_config,
// and the cloned long_term memories. All three undo steps run inside one
// transaction so the control fund either fully reverts or stays put;
// there is no partially-rolled-back state for the human operator to
// inspect afterwards.
//
// Idempotency notes:
//   - promoted_memory_ids that were already deleted (e.g. because the
//     fund was re-promoted and the user manually pruned) are skipped
//     silently; we still report the original list so the audit log is
//     accurate.
//   - previous_skill_config replaces skill_config wholesale. Any candidate
//     skills the user added *after* the promotion are NOT preserved — the
//     contract is "rollback restores the snapshot". If a user wants to
//     keep manual additions they should reject the rollback and clean up
//     individual entries instead.
func (s *abTestServiceAdapter) RollbackLearningPromotion(userID, testID, promotionID string) (*api.ABTestLearningRollbackResult, error) {
	ctx := context.Background()
	test, err := s.authorizeABTest(ctx, userID, testID)
	if err != nil {
		return nil, err
	}

	var (
		result              api.ABTestLearningRollbackResult
		previousConfig      json.RawMessage
		previousSkillConfig json.RawMessage
		memoryIDsJSON       json.RawMessage
		skillKeysJSON       json.RawMessage
		agentName           string
	)
	if err := s.db.QueryRowContext(ctx, `
		SELECT p.id, p.test_id, p.agent_id, COALESCE(a.name, ''),
		       p.previous_config, p.previous_skill_config,
		       p.promoted_memory_ids, p.promoted_skill_keys
		FROM ab_test_learning_promotions p
		LEFT JOIN agents a ON a.id = p.agent_id
		WHERE p.test_id = $1 AND p.id = $2`, testID, promotionID).Scan(
		&result.PromotionID, &result.TestID, &result.AgentID, &agentName,
		&previousConfig, &previousSkillConfig,
		&memoryIDsJSON, &skillKeysJSON,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, api.ErrNotFound
		}
		return nil, err
	}
	if _, err := repository.NewTeamRepo(s.db).GetMember(ctx, test.ControlFundID, result.AgentID); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, api.ErrForbidden
		}
		return nil, err
	}

	previousConfig = ensureJSONObject(previousConfig)
	previousSkillConfig = ensureJSONObject(previousSkillConfig)
	promotedMemoryIDs := stringSliceFromJSON(memoryIDsJSON)
	promotedSkillKeys := stringSliceFromJSON(skillKeysJSON)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// 1. Restore evolution_config.
	if _, err := tx.ExecContext(ctx, `UPDATE agents SET evolution_config = $1, updated_at = NOW() WHERE id = $2`, previousConfig, result.AgentID); err != nil {
		return nil, fmt.Errorf("ab_rollback: restore evolution_config: %w", err)
	}

	// 2. Restore skill_config — wholesale snapshot replacement keeps the
	// rollback semantics simple and deterministic. We only do this if a
	// snapshot exists (legacy promotions from migration 022 carry an
	// empty default '{}' which is still valid).
	if _, err := tx.ExecContext(ctx, `UPDATE agents SET skill_config = $1, updated_at = NOW() WHERE id = $2`, previousSkillConfig, result.AgentID); err != nil {
		return nil, fmt.Errorf("ab_rollback: restore skill_config: %w", err)
	}

	// 3. Delete the cloned memories. Treat already-gone rows as a no-op so
	// re-running rollback is safe.
	memoryRepo := repository.NewMemoryRepo(s.db)
	if _, err := memoryRepo.DeleteByIDsWithTx(ctx, tx, promotedMemoryIDs); err != nil {
		return nil, fmt.Errorf("ab_rollback: delete reflections: %w", err)
	}

	// 4. Mark the promotion row as rolled back. We keep the original row
	// (and its snapshots) for audit; later promotions get new rows.
	if _, err := tx.ExecContext(ctx, `
		UPDATE ab_test_learning_promotions
		SET promoted_memory_ids = '[]'::jsonb,
		    promoted_skill_keys = '[]'::jsonb,
		    promoted_at = promoted_at
		WHERE id = $1`, result.PromotionID); err != nil {
		return nil, fmt.Errorf("ab_rollback: mark audit row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	result.AgentName = agentName
	result.RolledBack = true
	result.RolledBackReflectionIDs = promotedMemoryIDs
	result.SkillKeysReverted = promotedSkillKeys
	return &result, nil
}

func (s *abTestServiceAdapter) authorizeABTest(ctx context.Context, userID, testID string) (*repository.ABTest, error) {
	test, err := s.tests.GetByID(ctx, testID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if _, err := authorizeFundAccess(ctx, s.funds, s.companies, userID, test.ControlFundID); err != nil {
		return nil, err
	}
	if _, err := authorizeFundAccess(ctx, s.funds, s.companies, userID, test.TreatmentFundID); err != nil {
		return nil, err
	}
	return test, nil
}

type abShadowAgentLearning struct {
	AgentID                 string
	LatestTradingDate       string
	EventCount              int
	Summaries               []string
	Lessons                 []string
	Adjustments             []string
	SpecializationLearning  []map[string]any
	ProposedEvolutionConfig map[string]any
}

func normalizeABPromotionMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", abPromotionModeMerge:
		return abPromotionModeMerge
	case abPromotionModeOverwrite, "replace", "cover", "覆盖":
		return abPromotionModeOverwrite
	default:
		return ""
	}
}

func (s *abTestServiceAdapter) resolveABPromotionVariantKey(test *repository.ABTest, requested string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(requested)) {
	case "A", "CONTROL":
		return "A", nil
	case "B", "TREATMENT":
		return "B", nil
	case "":
		var results api.ABTestResults
		if len(test.Results) == 0 || string(test.Results) == "null" || json.Unmarshal(test.Results, &results) != nil {
			return "", api.ErrBadInput
		}
		switch results.Winner {
		case "control":
			return "A", nil
		case "treatment":
			return "B", nil
		default:
			return "", api.ErrConflict
		}
	default:
		return "", api.ErrBadInput
	}
}

func (s *abTestServiceAdapter) loadABVariantSnapshot(ctx context.Context, testID, variantKey string) (string, abTeamSnapshot, error) {
	var variantID string
	var snapshotJSON json.RawMessage
	if err := s.db.QueryRowContext(ctx, `
		SELECT id, team_snapshot
		FROM ab_test_variants
		WHERE test_id = $1 AND variant_key = $2`, testID, variantKey).Scan(&variantID, &snapshotJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", abTeamSnapshot{}, api.ErrNotFound
		}
		return "", abTeamSnapshot{}, err
	}
	var snapshot abTeamSnapshot
	if err := json.Unmarshal(snapshotJSON, &snapshot); err != nil {
		return "", abTeamSnapshot{}, err
	}
	return variantID, snapshot, nil
}

func (s *abTestServiceAdapter) loadABShadowAgentLearning(ctx context.Context, testID, variantID string) (map[string]abShadowAgentLearning, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT agent_id::text, trading_date, COALESCE(summary, ''), lessons, adjustments, specialization_learning, proposed_evolution_config
		FROM ab_test_agent_learning_events
		WHERE test_id = $1 AND variant_id = $2
		ORDER BY agent_id, trading_date`, testID, variantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]abShadowAgentLearning{}
	for rows.Next() {
		var agentID string
		var tradingDate time.Time
		var summary string
		var lessonsJSON, adjustmentsJSON, specializationJSON, proposedJSON json.RawMessage
		if err := rows.Scan(&agentID, &tradingDate, &summary, &lessonsJSON, &adjustmentsJSON, &specializationJSON, &proposedJSON); err != nil {
			return nil, err
		}
		learning := result[agentID]
		learning.AgentID = agentID
		learning.EventCount++
		learning.LatestTradingDate = tradingDate.Format("2006-01-02")
		if strings.TrimSpace(summary) != "" {
			learning.Summaries = append(learning.Summaries, strings.TrimSpace(summary))
		}
		learning.Lessons = uniqueNonEmpty(append(learning.Lessons, stringSliceFromJSON(lessonsJSON)...))
		learning.Adjustments = uniqueNonEmpty(append(learning.Adjustments, stringSliceFromJSON(adjustmentsJSON)...))
		if specialization := mapFromJSON(specializationJSON); len(specialization) > 0 {
			learning.SpecializationLearning = append(learning.SpecializationLearning, specialization)
		}
		if proposed := mapFromJSON(proposedJSON); len(proposed) > 0 {
			learning.ProposedEvolutionConfig = proposed
		}
		result[agentID] = learning
	}
	return result, rows.Err()
}

func buildPromotedABEvolutionConfig(previous json.RawMessage, learning abShadowAgentLearning, mode, testID, variantKey string) (json.RawMessage, error) {
	base := mapFromJSON(previous)
	if mode == abPromotionModeOverwrite {
		base = copyStringAnyMap(learning.ProposedEvolutionConfig)
	}
	if base == nil {
		base = map[string]any{}
	}
	base["recentLessons"] = limitStrings(uniqueNonEmpty(append(learning.Lessons, stringSliceFromConfig(base, "recentLessons")...)), 12)
	base["lastRecommendedAdjustments"] = limitStrings(uniqueNonEmpty(learning.Adjustments), 12)
	base["lastLearningSummary"] = strings.Join(limitStrings(learning.Summaries, 3), "；")
	base["lastLearningDate"] = learning.LatestTradingDate
	base["learningUpdatedAt"] = time.Now().UTC().Format(time.RFC3339)
	base["promotedABLearning"] = compactConfigMap(map[string]any{
		"testId":                 testID,
		"variantKey":             variantKey,
		"mode":                   mode,
		"eventCount":             learning.EventCount,
		"latestTradingDate":      learning.LatestTradingDate,
		"summaries":              limitStrings(learning.Summaries, 5),
		"lessons":                limitStrings(learning.Lessons, 12),
		"adjustments":            limitStrings(learning.Adjustments, 12),
		"specializationLearning": learning.SpecializationLearning,
		"promotedAt":             time.Now().UTC().Format(time.RFC3339),
	})
	return json.Marshal(base)
}

// abLearningPromotionInput is the per-agent transactional payload used by
// applyABLearningPromotion. Bundling the inputs into a struct keeps the
// call-site readable as we layer F6's reflection + skill promotion on top
// of the legacy evolution_config flow.
type abLearningPromotionInput struct {
	TestID              string
	VariantID           string
	AgentID             string
	UserID              string
	Mode                string
	PreviousConfig      json.RawMessage
	PromotedConfig      json.RawMessage
	PreviousSkillConfig json.RawMessage
	ProjectedMemories   []repository.Memory
	ProjectedSkills     []parsedSkillEntry
}

// abLearningPromotionOutcome reports what was written inside the
// transaction — the caller surfaces these IDs/keys both for the response
// payload and for follow-up rollback bookkeeping.
type abLearningPromotionOutcome struct {
	PromotionID       string
	PromotedMemoryIDs []string
	PromotedSkillKeys []string
}

// applyABLearningPromotion materialises a winning variant's lessons into
// the control fund. It writes three things atomically:
//
//  1. agents.evolution_config — the legacy "recentLessons" bag.
//  2. memories rows (layer=long_term, fund_id=ControlFundID) — so the
//     control fund's reflection timeline gains the promoted insights and
//     subsequent prompts can recall them.
//  3. agents.skill_config — appends candidate skills derived from the
//     promoted lessons. These are inserted with status=proposed so the
//     control fund's owner still has the F4 approval gate (the A/B run
//     wins the *evidence*; the human wins the final say).
//
// Everything commits together, and the IDs of the new memories + skill
// keys are stored on the promotions row so RollbackLearningPromotion can
// undo them surgically.
func (s *abTestServiceAdapter) applyABLearningPromotion(ctx context.Context, in abLearningPromotionInput) (abLearningPromotionOutcome, error) {
	previousConfig := ensureJSONObject(in.PreviousConfig)
	promotedConfig := ensureJSONObject(in.PromotedConfig)
	previousSkillConfig := ensureJSONObject(in.PreviousSkillConfig)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return abLearningPromotionOutcome{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// 1. evolution_config (legacy + always present).
	if _, err := tx.ExecContext(ctx, `UPDATE agents SET evolution_config = $1, updated_at = NOW() WHERE id = $2`, promotedConfig, in.AgentID); err != nil {
		return abLearningPromotionOutcome{}, fmt.Errorf("ab_promote: update evolution_config: %w", err)
	}

	// 2. memories — clone projected reflections into the control fund. We
	// re-use MemoryRepo.CreateWithTx so the standard memory machinery
	// (visibility/sensitivity defaults, GIN tag index, etc.) applies.
	memoryRepo := repository.NewMemoryRepo(s.db)
	promotedMemoryIDs := make([]string, 0, len(in.ProjectedMemories))
	for i := range in.ProjectedMemories {
		newID, err := memoryRepo.CreateWithTx(ctx, tx, &in.ProjectedMemories[i])
		if err != nil {
			return abLearningPromotionOutcome{}, fmt.Errorf("ab_promote: clone reflection: %w", err)
		}
		promotedMemoryIDs = append(promotedMemoryIDs, newID)
	}

	// 3. skill_config — append candidate skills with status=proposed so a
	// human still has to approve them in the F4 UI. We append onto the
	// snapshot (mode=merge) or overwrite the whole list (mode=overwrite).
	updatedSkillConfig, promotedSkillKeys, err := mergePromotedSkillsIntoConfig(previousSkillConfig, in.ProjectedSkills, in.Mode)
	if err != nil {
		return abLearningPromotionOutcome{}, fmt.Errorf("ab_promote: merge skills: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agents SET skill_config = $1, updated_at = NOW() WHERE id = $2`, updatedSkillConfig, in.AgentID); err != nil {
		return abLearningPromotionOutcome{}, fmt.Errorf("ab_promote: update skill_config: %w", err)
	}

	// 4. promotions audit row, carrying all rollback metadata.
	memoryIDsJSON, err := json.Marshal(promotedMemoryIDs)
	if err != nil {
		return abLearningPromotionOutcome{}, err
	}
	skillKeysJSON, err := json.Marshal(promotedSkillKeys)
	if err != nil {
		return abLearningPromotionOutcome{}, err
	}
	var promotionID string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO ab_test_learning_promotions (
			test_id, variant_id, agent_id, mode,
			previous_config, promoted_config, promoted_by,
			promoted_memory_ids, promoted_skill_keys, previous_skill_config
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id`,
		in.TestID, in.VariantID, in.AgentID, in.Mode,
		previousConfig, promotedConfig, nullUUID(in.UserID),
		memoryIDsJSON, skillKeysJSON, previousSkillConfig,
	).Scan(&promotionID); err != nil {
		return abLearningPromotionOutcome{}, fmt.Errorf("ab_promote: write audit row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return abLearningPromotionOutcome{}, err
	}
	return abLearningPromotionOutcome{
		PromotionID:       promotionID,
		PromotedMemoryIDs: promotedMemoryIDs,
		PromotedSkillKeys: promotedSkillKeys,
	}, nil
}

// buildPromotedReflectionMemories projects an agent's shadow learning
// events into memory rows ready to insert into the control fund. The
// title prefix and tag set are deterministic so a re-promotion of the
// same A/B test is idempotent at the title level (callers can spot
// duplicates), and the rollback path can audit which rows came from
// which test.
//
// Heuristic: each unique non-empty lesson string yields one long_term
// memory. We cap at 12 to mirror the cap that buildPromotedABEvolutionConfig
// applies to recentLessons — anything beyond that is noise.
func buildPromotedReflectionMemories(fundID, agentID string, learning abShadowAgentLearning, testID, variantKey string) []repository.Memory {
	if strings.TrimSpace(fundID) == "" || strings.TrimSpace(agentID) == "" {
		return nil
	}
	lessons := limitStrings(uniqueNonEmpty(learning.Lessons), 12)
	if len(lessons) == 0 {
		return nil
	}
	tradingDate := parsePromotedTradingDate(learning.LatestTradingDate)
	memories := make([]repository.Memory, 0, len(lessons))
	for _, lesson := range lessons {
		title := fmt.Sprintf("A/B promoted reflection · %s · %s", variantKey, firstSentence(lesson, 60))
		mem := repository.Memory{
			FundID:      fundID,
			AgentID:     sql.NullString{String: agentID, Valid: true},
			Visibility:  "private",
			Sensitivity: "normal",
			OriginKind:  "ab_promotion",
			Layer:       reflectionMemoryLayer,
			Title:       sql.NullString{String: title, Valid: true},
			Content:     lesson,
			TradingDate: tradingDate,
			Tags:        []string{"ab_promotion", "ab:" + testID, "variant:" + variantKey, "source:ab_test"},
		}
		memories = append(memories, mem)
	}
	return memories
}

// parsePromotedTradingDate accepts YYYY-MM-DD (the format the learning
// events table uses) and returns a sql.NullTime. Invalid / empty input
// becomes a Null so the memory row falls back to the column default.
func parsePromotedTradingDate(s string) sql.NullTime {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return sql.NullTime{}
	}
	t, err := time.Parse("2006-01-02", trimmed)
	if err != nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}

// buildPromotedSkillCandidates derives parsedSkillEntry values from the
// shadow learning adjustments. Each adjustment that looks instructional
// becomes a candidate skill scoped to the agent's role so it cannot
// accidentally leak into another role's prompts.
//
// We deliberately use the *adjustments* list (not lessons) because
// adjustments are already phrased as "do X" / "avoid Y" — i.e. closer
// to the skill-content the F4 prompts expect.
func buildPromotedSkillCandidates(learning abShadowAgentLearning, role, testID, variantKey string) []parsedSkillEntry {
	candidates := limitStrings(uniqueNonEmpty(learning.Adjustments), 6)
	if len(candidates) == 0 {
		return nil
	}
	disabled := false
	roleTag := strings.ToLower(strings.TrimSpace(role))
	roles := dedupeNonEmptyLower([]string{roleTag})
	entries := make([]parsedSkillEntry, 0, len(candidates))
	for _, content := range candidates {
		sum := sha1.Sum([]byte(testID + "|" + variantKey + "|" + content))
		key := "ab_promotion:" + testID + ":" + variantKey + ":" + hex.EncodeToString(sum[:6])
		entries = append(entries, parsedSkillEntry{
			Key:         key,
			Name:        fmt.Sprintf("A/B 候选技能 · %s", firstSentence(content, 48)),
			Description: firstSentence(content, 160),
			Content:     content,
			Enabled:     &disabled,
			Priority:    0,
			Status:      skillStatusProposed,
			Source:      "ab_promotion:" + testID + ":" + variantKey,
			ProposedAt:  time.Now().UTC().Format(time.RFC3339),
			Match: parsedSkillMatch{
				Roles: roles,
			},
		})
	}
	return entries
}

// mergePromotedSkillsIntoConfig appends the candidate skills onto the
// existing skill_config. For mode=overwrite we discard any pre-existing
// `ab_promotion:*` entries first (they came from an earlier round of the
// same test, so the user explicitly chose to replace them).
//
// Returns the new JSON, the keys that were actually inserted, and any
// parse error. Keys that already exist are reported as "inserted" so the
// rollback path still records them — that keeps idempotent re-runs safe
// (rollback deletes by key, never by row position).
func mergePromotedSkillsIntoConfig(previousRaw json.RawMessage, candidates []parsedSkillEntry, mode string) (json.RawMessage, []string, error) {
	if len(candidates) == 0 {
		return previousRaw, nil, nil
	}
	config := parsedSkillConfig{Enabled: true}
	trimmed := strings.TrimSpace(string(previousRaw))
	if trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal(previousRaw, &config); err != nil {
			return nil, nil, fmt.Errorf("parse skill config: %w", err)
		}
	}
	if strings.EqualFold(mode, abPromotionModeOverwrite) {
		filtered := make([]parsedSkillEntry, 0, len(config.Skills))
		for _, skill := range config.Skills {
			if strings.HasPrefix(skill.Source, "ab_promotion:") {
				continue
			}
			filtered = append(filtered, skill)
		}
		config.Skills = filtered
	}
	existing := make(map[string]struct{}, len(config.Skills))
	for _, skill := range config.Skills {
		existing[skill.Key] = struct{}{}
	}
	insertedKeys := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if _, dup := existing[candidate.Key]; dup {
			insertedKeys = append(insertedKeys, candidate.Key)
			continue
		}
		config.Skills = append(config.Skills, candidate)
		existing[candidate.Key] = struct{}{}
		insertedKeys = append(insertedKeys, candidate.Key)
	}
	out, err := json.Marshal(config)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal skill config: %w", err)
	}
	return out, insertedKeys, nil
}

func stringSet(values []string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			result[trimmed] = struct{}{}
		}
	}
	return result
}

func stringSliceFromJSON(raw json.RawMessage) []string {
	var values []string
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &values) != nil {
		return nil
	}
	return uniqueNonEmpty(values)
}

func mapFromJSON(raw json.RawMessage) map[string]any {
	result := map[string]any{}
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &result) != nil {
		return map[string]any{}
	}
	return compactConfigMap(result)
}

func ensureJSONObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" || !json.Valid(raw) {
		return json.RawMessage(`{}`)
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

type abShadowVariantRuntime struct {
	ID             string
	Key            string
	Name           string
	StrategyConfig map[string]any
	TeamSnapshot   abTeamSnapshot
}

func (s *abTestServiceAdapter) ensureABShadowExecution(ctx context.Context, test *repository.ABTest) error {
	if test == nil || test.VariableType != abTestVariableStrategyCompare {
		return nil
	}
	controlShadow, hasControl, err := s.loadABShadowVariantData(ctx, test.ID, "A")
	if err != nil {
		return err
	}
	treatmentShadow, hasTreatment, err := s.loadABShadowVariantData(ctx, test.ID, "B")
	if err != nil {
		return err
	}
	if hasControl && hasTreatment && (len(controlShadow.NAVs) > 0 || controlShadow.TradeCount > 0) && (len(treatmentShadow.NAVs) > 0 || treatmentShadow.TradeCount > 0) {
		return nil
	}
	variants, err := s.loadABShadowVariants(ctx, test.ID)
	if err != nil {
		return err
	}
	variantA, okA := variants["A"]
	variantB, okB := variants["B"]
	if !okA || !okB {
		if err := s.createABShadowVariants(ctx, test.ID, test.ControlFundID, test.VariableConfig); err != nil {
			return err
		}
		variants, err = s.loadABShadowVariants(ctx, test.ID)
		if err != nil {
			return err
		}
		variantA, okA = variants["A"]
		variantB, okB = variants["B"]
		if !okA || !okB {
			return api.ErrConflict
		}
	}
	start, end := abTestDateRange(test)
	navs, err := s.navs.ListByFund(ctx, test.ControlFundID, start, end)
	if err != nil {
		return err
	}
	if len(navs) == 0 {
		latest, err := s.navs.GetLatest(ctx, test.ControlFundID)
		if err != nil && !errors.Is(err, repository.ErrNotFound) {
			return err
		}
		if latest != nil {
			navs = []repository.NavSnapshot{*latest}
		}
	}
	trades, err := s.trades.ListByFund(ctx, test.ControlFundID, start, end, 1000)
	if err != nil {
		return err
	}
	return s.writeABSyntheticShadowRun(ctx, test, variantA, variantB, navs, trades)
}

func (s *abTestServiceAdapter) loadABShadowVariants(ctx context.Context, testID string) (map[string]abShadowVariantRuntime, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, variant_key, name, strategy_config, team_snapshot
		FROM ab_test_variants
		WHERE test_id = $1`, testID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	variants := map[string]abShadowVariantRuntime{}
	for rows.Next() {
		var variant abShadowVariantRuntime
		var strategyJSON, snapshotJSON json.RawMessage
		if err := rows.Scan(&variant.ID, &variant.Key, &variant.Name, &strategyJSON, &snapshotJSON); err != nil {
			return nil, err
		}
		variant.StrategyConfig = mapFromJSON(strategyJSON)
		if err := json.Unmarshal(snapshotJSON, &variant.TeamSnapshot); err != nil {
			return nil, err
		}
		variants[variant.Key] = variant
	}
	return variants, rows.Err()
}

func (s *abTestServiceAdapter) writeABSyntheticShadowRun(ctx context.Context, test *repository.ABTest, variantA, variantB abShadowVariantRuntime, navs []repository.NavSnapshot, trades []repository.TradeExecution) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM ab_test_variant_trades WHERE test_id = $1 AND reasoning LIKE '[auto-shadow]%'`, test.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ab_test_decision_diffs WHERE test_id = $1 AND explanation LIKE '[auto-shadow]%'`, test.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ab_test_agent_learning_events WHERE test_id = $1 AND summary LIKE '[auto-shadow]%'`, test.ID); err != nil {
		return err
	}
	if err := writeABSyntheticNAVs(ctx, tx, test.ID, variantA, navs, 1); err != nil {
		return err
	}
	decider := s.bSideDecider
	if decider == nil {
		decider = deterministicBSideDecider{}
	}
	// K-2: build the grounding context once per run so per-trade
	// and recap prompts share the same NAV / aggregate stats.
	// Empty when there's no NAV history; the prompt builders
	// degrade gracefully in that case.
	from, to := abTestDateRange(test)
	bsideCtx := abBSideContextBuild(navs, trades, from, to)
	// K-3: B's starting capital anchors to A's NAV[0].TotalAssets
	// so day-1 NAV index lands at the same baseline as A. After
	// that B diverges based on its own lot ledger.
	initialCash := 0.0
	if len(navs) > 0 {
		initialCash = navs[0].TotalAssets
	}
	bLedger, priceTL, err := writeABSyntheticTradesAndDiffs(ctx, tx, test.ID, variantA, variantB, trades, decider, bsideCtx, initialCash)
	if err != nil {
		return err
	}
	// K-3: B's NAV series is now recomputed from the ledger
	// instead of `A.NAV × bias`. The legacy bias scaler is left
	// in place for variants whose strategy_config has no real
	// trade impact yet (synthetic deterministic decider with no
	// SideOverride), but only as a fallback when there are no
	// trades at all to mark on.
	if len(trades) > 0 {
		if err := writeBSideNAVsFromLedger(ctx, tx, test.ID, variantB, navs, bLedger, priceTL); err != nil {
			return err
		}
	} else {
		// No trades → no ledger → fall back to the bias scaler
		// so the AB chart still has *something* to draw. This is
		// the only path where `A.NAV × bias` lives; flag it in
		// the slog so an operator can see when it triggered.
		bias := abStrategyReturnBias(variantB.StrategyConfig)
		if err := writeABSyntheticNAVs(ctx, tx, test.ID, variantB, navs, bias); err != nil {
			return err
		}
		slog.Info("ab shadow B NAV: ledger empty, used bias fallback",
			"testID", test.ID,
			"variantID", variantB.ID,
			"bias", bias,
		)
	}
	latestDate := latestABShadowLearningDate(navs, trades)
	if latestDate.IsZero() {
		latestDate = time.Now().UTC()
	}
	if err := writeABSyntheticLearningEvents(ctx, tx, test.ID, variantA, latestDate, false, decider, trades, bsideCtx); err != nil {
		return err
	}
	if err := writeABSyntheticLearningEvents(ctx, tx, test.ID, variantB, latestDate, true, decider, trades, bsideCtx); err != nil {
		return err
	}
	return tx.Commit()
}

// writeBSideNAVsFromLedger (Card K-3) replaces the legacy
// `A.NAV × bias` shortcut. Walks A's NAV dates and emits one
// `ab_test_variant_nav` row per date for B, sourced from:
//
//   - the ledger's trade history (B's actual decisions, not
//     A's scaled by a constant)
//   - the price timeline built from A's trade stream (so we use
//     real prices A executed at)
//   - A's NAV[0].TotalAssets as the starting capital (B starts
//     identical to A, then diverges)
//
// Idempotent via `ON CONFLICT (variant_id, trading_date)` — same
// shape as the writeABSyntheticNAVs path it replaces.
func writeBSideNAVsFromLedger(ctx context.Context, tx *sql.Tx, testID string, variant abShadowVariantRuntime, aNavs []repository.NavSnapshot, ledger *bSideLotLedger, priceTL *priceTimeline) error {
	if ledger == nil || len(aNavs) == 0 {
		return nil
	}
	rows := computeBSideNAVRows(ledger.History(), aNavs, priceTL, ledger.InitialCash())
	for _, r := range rows {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO ab_test_variant_nav (test_id, variant_id, trading_date, nav, total_assets, cash, daily_return, cumulative_return, drawdown)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (variant_id, trading_date) DO UPDATE SET
			  nav = EXCLUDED.nav,
			  total_assets = EXCLUDED.total_assets,
			  cash = EXCLUDED.cash,
			  daily_return = EXCLUDED.daily_return,
			  cumulative_return = EXCLUDED.cumulative_return,
			  drawdown = EXCLUDED.drawdown`,
			testID, variant.ID, r.TradingDate, r.NAV, r.TotalAssets, r.Cash, r.DailyReturn, r.CumulativeReturn, r.Drawdown,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

func writeABSyntheticNAVs(ctx context.Context, tx *sql.Tx, testID string, variant abShadowVariantRuntime, navs []repository.NavSnapshot, returnBias float64) error {
	if len(navs) == 0 {
		return nil
	}
	baseNAV := navs[0].NAV
	if baseNAV <= 0 {
		baseNAV = 1
	}
	peak := baseNAV
	for i, nav := range navs {
		shadowNAV := nav.NAV
		if i > 0 && returnBias != 1 && navs[0].NAV > 0 {
			shadowNAV = navs[0].NAV * (1 + ((nav.NAV/navs[0].NAV)-1)*returnBias)
		}
		if shadowNAV <= 0 {
			shadowNAV = nav.NAV
		}
		if shadowNAV > peak {
			peak = shadowNAV
		}
		drawdown := 0.0
		if peak > 0 {
			drawdown = shadowNAV/peak - 1
		}
		cumulative := shadowNAV/baseNAV - 1
		dailyReturn := nav.DailyReturn * returnBias
		assets := nav.TotalAssets
		if nav.NAV > 0 {
			assets = nav.TotalAssets * (shadowNAV / nav.NAV)
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO ab_test_variant_nav (test_id, variant_id, trading_date, nav, total_assets, cash, daily_return, cumulative_return, drawdown)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (variant_id, trading_date) DO UPDATE SET
			  nav = EXCLUDED.nav,
			  total_assets = EXCLUDED.total_assets,
			  cash = EXCLUDED.cash,
			  daily_return = EXCLUDED.daily_return,
			  cumulative_return = EXCLUDED.cumulative_return,
			  drawdown = EXCLUDED.drawdown`, testID, variant.ID, nav.TradingDate, shadowNAV, assets, nav.AvailableCash, dailyReturn, cumulative, drawdown)
		if err != nil {
			return err
		}
	}
	return nil
}

// writeABSyntheticTradesAndDiffs writes the per-trade rows for
// both A and B variants AND constructs a B-side lot ledger that
// the caller can use to recompute B's NAV from real positions
// rather than the legacy `A.NAV × bias` shortcut (Card K-3).
//
// Returns (ledger, priceTimeline, error). The ledger is non-nil
// even on early return so the caller can safely chain into the
// NAV writer without nil-checking. The price timeline carries
// every (symbol, date, price) observation from A's trade stream
// so the NAV writer can mark-to-market each B holding using the
// same prices A actually executed at.
func writeABSyntheticTradesAndDiffs(ctx context.Context, tx *sql.Tx, testID string, variantA, variantB abShadowVariantRuntime, trades []repository.TradeExecution, decider abShadowBSideDecider, bsideCtx abBSideContext, initialCash float64) (*bSideLotLedger, *priceTimeline, error) {
	if decider == nil {
		decider = deterministicBSideDecider{}
	}
	bLedger := newBSideLotLedger(initialCash)
	priceTL := newPriceTimeline()
	for _, trade := range trades {
		tradingDate := trade.CreatedAt
		if trade.ExecutedAt.Valid {
			tradingDate = trade.ExecutedAt.Time
		}
		price := abTradePrice(trade)
		notional := abTradeNotional(trade, price)
		realized := abTradeRealizedPnL(trade)
		// Feed every A trade into the price timeline so B's
		// MTM uses the same prices A executed at.
		priceTL.Add(trade.Symbol, tradingDate, price)
		reasonA := "[auto-shadow] A 组沿用当前基金真实决策作为基线影子交易。"
		// A side is always the mirror of the real fund's trade.
		if err := insertABShadowTrade(ctx, tx, testID, variantA.ID, tradingDate, trade.Symbol, trade.Side, trade.Quantity, price, notional, realized, reasonA); err != nil {
			return bLedger, priceTL, err
		}
		// B side goes through the decider. Errors fall back to
		// deterministic inside the decider impl, so we just need
		// to handle a clean error from the call itself.
		decision, err := decider.DecideTrade(ctx, variantB, trade, bsideCtx)
		if err != nil {
			// Defensive fallback in case a custom decider returns
			// an error: synthesize a deterministic decision so
			// AnalyzeTest still completes.
			decision, err = (deterministicBSideDecider{}).DecideTrade(ctx, variantB, trade, bsideCtx)
			if err != nil {
				return bLedger, priceTL, err
			}
		}
		// Skip = B chose not to trade. Record a decision diff
		// (so the dashboard can show "B passed on this signal")
		// but no row in ab_test_variant_trades AND no ledger
		// mutation — B's positions / cash stay where they were.
		if decision.Skip {
			_, derr := tx.ExecContext(ctx, `
				INSERT INTO ab_test_decision_diffs (test_id, trading_date, symbol, variant_a_action, variant_b_action, return_impact, explanation)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`, testID, tradingDate, trade.Symbol, strings.ToUpper(trade.Side), "SKIP", -realized/math.Max(math.Abs(notional), 1)*100, "[auto-shadow] B 组本次未参与该笔交易："+decision.Reasoning)
			if derr != nil {
				return bLedger, priceTL, derr
			}
			continue
		}
		// Transform A trade × decision → B trade. Drop the
		// trade entirely if the transformation came back !ok
		// (e.g., side override produced an unknown side, or
		// scale × qty went to zero).
		bQty, bPrice, bSide, ok := applyBSideDecision(trade.Side, trade.Quantity, price, decision)
		if !ok {
			continue
		}
		// Apply to ledger — this is the source of truth for
		// B's realized PnL. We use the ledger's PnL on the
		// trade row instead of the old `realized * scale` proxy
		// because the ledger reflects FIFO matching against
		// B's actual lots, not just A's outcome scaled by qty.
		applyResult := bLedger.Apply(tradingDate, trade.Symbol, bSide, bQty, bPrice)
		bRealized := applyResult.RealizedPnL
		bNotional := math.Abs(applyResult.Applied * bPrice)
		// If the LLM tried to flip A's side and the override was
		// dropped by applyBSideDecision (already handled above),
		// or applyResult.Applied is zero (ledger no-op'd it,
		// e.g., naked SELL with no inventory), skip the trade
		// row but still record a decision diff so the dashboard
		// can show what B intended.
		if applyResult.Applied <= 0 {
			impactNote := "[auto-shadow] B 组决策被账本拒绝（如无库存可卖）："
			_, derr := tx.ExecContext(ctx, `
				INSERT INTO ab_test_decision_diffs (test_id, trading_date, symbol, variant_a_action, variant_b_action, return_impact, explanation)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`, testID, tradingDate, trade.Symbol, strings.ToUpper(trade.Side), "REJECT", 0.0, impactNote+decision.Reasoning)
			if derr != nil {
				return bLedger, priceTL, derr
			}
			continue
		}
		if err := insertABShadowTrade(ctx, tx, testID, variantB.ID, tradingDate, trade.Symbol, bSide, applyResult.Applied, bPrice, bNotional, bRealized, decision.Reasoning); err != nil {
			return bLedger, priceTL, err
		}
		impact := 0.0
		if math.Abs(notional) > 0 {
			impact = (bRealized - realized) / math.Abs(notional) * 100
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO ab_test_decision_diffs (test_id, trading_date, symbol, variant_a_action, variant_b_action, return_impact, explanation)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, testID, tradingDate, trade.Symbol, strings.ToUpper(trade.Side), fmt.Sprintf("%s x%.2f", bSide, decision.QuantityScale), impact, decision.Reasoning)
		if err != nil {
			return bLedger, priceTL, err
		}
	}
	return bLedger, priceTL, nil
}

func insertABShadowTrade(ctx context.Context, tx *sql.Tx, testID, variantID string, tradingDate time.Time, symbol, side string, qty, price, notional, realized float64, reasoning string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO ab_test_variant_trades (test_id, variant_id, trading_date, symbol, side, quantity, price, notional, realized_pnl, reasoning)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, testID, variantID, tradingDate, symbol, side, qty, price, notional, realized, reasoning)
	return err
}

func writeABSyntheticLearningEvents(ctx context.Context, tx *sql.Tx, testID string, variant abShadowVariantRuntime, tradingDate time.Time, treatment bool, decider abShadowBSideDecider, controlTrades []repository.TradeExecution, bsideCtx abBSideContext) error {
	if len(variant.TeamSnapshot.Members) == 0 {
		return nil
	}
	// Card K-1: when this is the treatment (B) variant and a
	// non-deterministic decider is wired (LLM mode), we ask it
	// to summarize the run end-to-end. The control (A) variant
	// always uses the canned copy because A is a no-op replay
	// of the real fund — there's nothing to "learn" beyond the
	// baseline.
	var lessons []string
	var adjustments []string
	var summaryText string
	specialization := "{}"
	proposedMap := map[string]any{
		"recentLessons":              []string{},
		"lastRecommendedAdjustments": []string{},
		"shadowVariantKey":           variant.Key,
		"shadowLearningMode":         abLearningModeShadowEphemeral,
	}

	if treatment && decider != nil {
		recap, recapErr := decider.SummarizeBLearning(ctx, variant, controlTrades, bsideCtx)
		if recapErr == nil && (len(recap.Lessons) > 0 || len(recap.Adjustments) > 0 || strings.TrimSpace(recap.Summary) != "") {
			lessons = recap.Lessons
			adjustments = recap.Adjustments
			summaryText = recap.Summary
			if strings.TrimSpace(recap.SpecializationLearning) != "" {
				if encoded, jerr := json.Marshal(map[string]string{"summary": recap.SpecializationLearning}); jerr == nil {
					specialization = string(encoded)
				}
			}
			proposedMap["recentLessons"] = recap.Lessons
			proposedMap["lastRecommendedAdjustments"] = recap.Adjustments
			if recap.ProposedEvolutionConfig != nil {
				for k, v := range recap.ProposedEvolutionConfig {
					proposedMap[k] = v
				}
			}
		}
	}
	if len(lessons) == 0 {
		lessons = []string{"复盘影子交易结果，比较收益、回撤与换手差异"}
		if treatment {
			lessons = append(lessons, "实验策略在影子环境中形成独立学习结果，不污染真实 agent")
		}
	}
	if len(adjustments) == 0 {
		adjustments = []string{"继续观察样本充分性后再决定是否提升到真实 agent"}
		if treatment {
			adjustments = append(adjustments, "若置信度充足，可通过 promotion 将学习结果合并或覆盖到真实 agent")
		}
	}
	if strings.TrimSpace(summaryText) == "" {
		summaryText = fmt.Sprintf("[auto-shadow] %s 组影子学习事件：%s", variant.Key, variant.Name)
	}
	// Re-fold the latest lessons/adjustments into the proposed
	// config so a downstream "show me what would change in
	// evolution_config" diff has a stable shape regardless of
	// which decider produced the recap.
	proposedMap["recentLessons"] = lessons
	proposedMap["lastRecommendedAdjustments"] = adjustments
	lessonsJSON, _ := json.Marshal(lessons)
	adjustmentsJSON, _ := json.Marshal(adjustments)
	proposed, _ := json.Marshal(compactConfigMap(proposedMap))

	for _, member := range variant.TeamSnapshot.Members {
		if strings.TrimSpace(member.AgentID) == "" {
			continue
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO ab_test_agent_learning_events (test_id, variant_id, agent_id, trading_date, summary, lessons, adjustments, specialization_learning, proposed_evolution_config)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (variant_id, agent_id, trading_date) DO UPDATE SET
			  summary = EXCLUDED.summary,
			  lessons = EXCLUDED.lessons,
			  adjustments = EXCLUDED.adjustments,
			  specialization_learning = EXCLUDED.specialization_learning,
			  proposed_evolution_config = EXCLUDED.proposed_evolution_config`, testID, variant.ID, member.AgentID, tradingDate, summaryText, lessonsJSON, adjustmentsJSON, specialization, proposed)
		if err != nil {
			return err
		}
	}
	return nil
}

func abStrategyReturnBias(config map[string]any) float64 {
	bias := 1.0
	style := strings.ToLower(strings.TrimSpace(fmt.Sprint(config["pmStyle"])))
	switch style {
	case "aggressive", "growth", "进取", "激进":
		bias += 0.12
	case "conservative", "defensive", "保守":
		bias -= 0.08
	}
	if maxPosition, ok := floatFromAny(config["maxSinglePosition"]); ok {
		if maxPosition >= 0.2 {
			bias += 0.06
		} else if maxPosition > 0 && maxPosition <= 0.08 {
			bias -= 0.04
		}
	}
	return math.Max(0.6, math.Min(1.4, bias))
}

func abStrategyTradeScale(config map[string]any) float64 {
	scale := abStrategyReturnBias(config)
	if maxPosition, ok := floatFromAny(config["maxSinglePosition"]); ok && maxPosition > 0 {
		scale = (scale + math.Min(1.5, math.Max(0.5, maxPosition/0.15))) / 2
	}
	return math.Max(0.5, math.Min(1.5, scale))
}

func floatFromAny(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func abTradePrice(trade repository.TradeExecution) float64 {
	if trade.FilledPrice.Valid && trade.FilledPrice.Float64 > 0 {
		return trade.FilledPrice.Float64
	}
	if trade.Price.Valid && trade.Price.Float64 > 0 {
		return trade.Price.Float64
	}
	return 0
}

func abTradeNotional(trade repository.TradeExecution, price float64) float64 {
	if trade.Amount.Valid {
		return math.Abs(trade.Amount.Float64)
	}
	qty := trade.FilledQty
	if qty <= 0 {
		qty = trade.Quantity
	}
	return math.Abs(qty * price)
}

func abTradeRealizedPnL(trade repository.TradeExecution) float64 {
	return -1 * (trade.FeeCommission + trade.FeeStampTax + trade.FeeTransfer)
}

func latestABShadowLearningDate(navs []repository.NavSnapshot, trades []repository.TradeExecution) time.Time {
	var latest time.Time
	for _, nav := range navs {
		if nav.TradingDate.After(latest) {
			latest = nav.TradingDate
		}
	}
	for _, trade := range trades {
		candidate := trade.CreatedAt
		if trade.ExecutedAt.Valid {
			candidate = trade.ExecutedAt.Time
		}
		if candidate.After(latest) {
			latest = candidate
		}
	}
	return latest
}

func (s *abTestServiceAdapter) buildABTestResults(test *repository.ABTest) (*api.ABTestResults, error) {
	start, end := abTestDateRange(test)
	controlTrades, err := s.trades.ListByFund(context.Background(), test.ControlFundID, start, end, 1000)
	if err != nil {
		return nil, err
	}
	treatmentTrades, err := s.trades.ListByFund(context.Background(), test.TreatmentFundID, start, end, 1000)
	if err != nil {
		return nil, err
	}
	controlNavs, err := s.navs.ListByFund(context.Background(), test.ControlFundID, start, end)
	if err != nil {
		return nil, err
	}
	treatmentNavs, err := s.navs.ListByFund(context.Background(), test.TreatmentFundID, start, end)
	if err != nil {
		return nil, err
	}
	controlShadow, hasControlShadow, err := s.loadABShadowVariantData(context.Background(), test.ID, "A")
	if err != nil {
		return nil, err
	}
	treatmentShadow, hasTreatmentShadow, err := s.loadABShadowVariantData(context.Background(), test.ID, "B")
	if err != nil {
		return nil, err
	}
	useShadow := hasControlShadow && hasTreatmentShadow
	if useShadow {
		controlNavs = controlShadow.NAVs
		treatmentNavs = treatmentShadow.NAVs
		controlTrades = nil
		treatmentTrades = nil
	}

	control := buildABVariantMetrics(controlNavs, controlTrades)
	treatment := buildABVariantMetrics(treatmentNavs, treatmentTrades)
	if useShadow {
		applyABShadowTradeMetrics(control, controlShadow)
		applyABShadowTradeMetrics(treatment, treatmentShadow)
	}
	if len(controlNavs) == 0 {
		if nav, err := s.navs.GetLatest(context.Background(), test.ControlFundID); err == nil {
			control = buildABVariantMetrics([]repository.NavSnapshot{*nav}, controlTrades)
		} else if !errors.Is(err, repository.ErrNotFound) {
			return nil, err
		}
	}
	if len(treatmentNavs) == 0 {
		if nav, err := s.navs.GetLatest(context.Background(), test.TreatmentFundID); err == nil {
			treatment = buildABVariantMetrics([]repository.NavSnapshot{*nav}, treatmentTrades)
		} else if !errors.Is(err, repository.ErrNotFound) {
			return nil, err
		}
	}

	winner := determineABWinner(control, treatment)
	decisionDiffs, err := s.loadABDecisionDiffs(context.Background(), test.ID)
	if err != nil {
		return nil, err
	}

	return &api.ABTestResults{
		VariantA:       control,
		VariantB:       treatment,
		Winner:         winner,
		NavSeries:      buildABNAVSeries(controlNavs, treatmentNavs),
		DecisionDiffs:  decisionDiffs,
		VariantATrades: controlShadow.Trades,
		VariantBTrades: treatmentShadow.Trades,
		Confidence:     buildABConfidenceSummary(control, treatment, len(controlNavs), len(treatmentNavs)),
		Scorecard:      buildABScorecard(control, treatment, len(controlNavs), len(treatmentNavs)),
	}, nil
}

type abShadowVariantData struct {
	NAVs        []repository.NavSnapshot
	TradeCount  int
	Turnover    float64
	RealizedPnL float64
	Trades      []api.ABTestVariantTrade
}

func (s *abTestServiceAdapter) loadABShadowVariantData(ctx context.Context, testID, variantKey string) (abShadowVariantData, bool, error) {
	var data abShadowVariantData
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.trading_date, n.nav, n.total_assets, n.cash, n.daily_return, n.cumulative_return
		FROM ab_test_variants v
		JOIN ab_test_variant_nav n ON n.variant_id = v.id
		WHERE v.test_id = $1 AND v.variant_key = $2
		ORDER BY n.trading_date`, testID, variantKey)
	if err != nil {
		return data, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var nav repository.NavSnapshot
		if err := rows.Scan(&nav.TradingDate, &nav.NAV, &nav.TotalAssets, &nav.AvailableCash, &nav.DailyReturn, &nav.TotalReturn); err != nil {
			return data, false, err
		}
		data.NAVs = append(data.NAVs, nav)
	}
	if err := rows.Err(); err != nil {
		return data, false, err
	}

	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(ABS(t.notional)), 0), COALESCE(SUM(t.realized_pnl), 0)
		FROM ab_test_variants v
		JOIN ab_test_variant_trades t ON t.variant_id = v.id
		WHERE v.test_id = $1 AND v.variant_key = $2`, testID, variantKey).Scan(&data.TradeCount, &data.Turnover, &data.RealizedPnL); err != nil {
		return data, false, err
	}

	tradeRows, err := s.db.QueryContext(ctx, `
		SELECT t.trading_date, v.variant_key, t.symbol, t.side, t.quantity, t.price, t.notional, t.realized_pnl, COALESCE(t.reasoning, '')
		FROM ab_test_variants v
		JOIN ab_test_variant_trades t ON t.variant_id = v.id
		WHERE v.test_id = $1 AND v.variant_key = $2
		ORDER BY t.trading_date DESC, t.created_at DESC
		LIMIT 100`, testID, variantKey)
	if err != nil {
		return data, false, err
	}
	defer tradeRows.Close()
	for tradeRows.Next() {
		var trade api.ABTestVariantTrade
		var tradingDate time.Time
		if err := tradeRows.Scan(&tradingDate, &trade.VariantKey, &trade.Symbol, &trade.Side, &trade.Quantity, &trade.Price, &trade.Notional, &trade.RealizedPnL, &trade.Reasoning); err != nil {
			return data, false, err
		}
		trade.Date = tradingDate.Format("2006-01-02")
		data.Trades = append(data.Trades, trade)
	}
	if err := tradeRows.Err(); err != nil {
		return data, false, err
	}
	return data, len(data.NAVs) > 0 || data.TradeCount > 0, nil
}

func (s *abTestServiceAdapter) loadABDecisionDiffs(ctx context.Context, testID string) ([]api.ABTestDecisionDiff, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT trading_date, symbol, COALESCE(variant_a_action, ''), COALESCE(variant_b_action, ''), return_impact, COALESCE(explanation, '')
		FROM ab_test_decision_diffs
		WHERE test_id = $1
		ORDER BY trading_date DESC, ABS(return_impact) DESC, symbol
		LIMIT 100`, testID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	diffs := []api.ABTestDecisionDiff{}
	for rows.Next() {
		var diff api.ABTestDecisionDiff
		var tradingDate time.Time
		if err := rows.Scan(&tradingDate, &diff.Symbol, &diff.VariantAAction, &diff.VariantBAction, &diff.ReturnImpact, &diff.Explanation); err != nil {
			return nil, err
		}
		diff.Date = tradingDate.Format("2006-01-02")
		diffs = append(diffs, diff)
	}
	return diffs, rows.Err()
}

func applyABShadowTradeMetrics(metrics map[string]float64, data abShadowVariantData) {
	metrics["tradeCount"] = float64(data.TradeCount)
	metrics["totalTurnover"] = data.Turnover
	metrics["realizedPnL"] = data.RealizedPnL
}

func buildABConfidenceSummary(control, treatment map[string]float64, controlNavPoints, treatmentNavPoints int) *api.ABTestConfidenceSummary {
	sampleDays := min(controlNavPoints, treatmentNavPoints)
	tradeCount := int(control["tradeCount"] + treatment["tradeCount"])
	returnGap := math.Abs(treatment["totalReturn"] - control["totalReturn"])
	warnings := []string{}
	score := 0.0
	if sampleDays >= 60 {
		score += 45
	} else if sampleDays >= 20 {
		score += 30
	} else if sampleDays >= 10 {
		score += 18
	} else if sampleDays > 0 {
		score += 8
		warnings = append(warnings, "样本天数偏少，结论可能受短期行情影响")
	} else {
		warnings = append(warnings, "缺少可比较的净值样本")
	}
	if tradeCount >= 40 {
		score += 25
	} else if tradeCount >= 10 {
		score += 15
	} else if tradeCount > 0 {
		score += 7
		warnings = append(warnings, "交易样本偏少，建议继续观察")
	} else {
		warnings = append(warnings, "缺少影子交易样本")
	}
	if returnGap >= 5 {
		score += 20
	} else if returnGap >= 2 {
		score += 12
	} else if returnGap >= 0.5 {
		score += 6
	} else {
		warnings = append(warnings, "A/B 收益差异不明显")
	}
	if math.Abs(treatment["maxDrawdown"]) > math.Abs(control["maxDrawdown"])*1.5 && treatment["totalReturn"] > control["totalReturn"] {
		score -= 10
		warnings = append(warnings, "B 策略收益更高但回撤放大，需要人工复核")
	}
	if treatment["totalTurnover"] > control["totalTurnover"]*2 && treatment["totalTurnover"] > 0 {
		score -= 5
		warnings = append(warnings, "B 策略换手显著更高，需关注交易成本")
	}
	score = math.Max(0, math.Min(100, score))
	level := "low"
	recommendation := "继续观察，暂不建议直接应用胜出策略"
	if score >= 75 {
		level = "high"
		recommendation = "样本较充分，可进入胜出策略应用前复核"
	} else if score >= 45 {
		level = "medium"
		recommendation = "有参考价值，建议结合决策差异与交易明细复核"
	}
	return &api.ABTestConfidenceSummary{
		Level:          level,
		Score:          score,
		SampleDays:     sampleDays,
		TradeCount:     tradeCount,
		Warnings:       warnings,
		Recommendation: recommendation,
	}
}

func buildABScorecard(control, treatment map[string]float64, controlNavPoints, treatmentNavPoints int) *api.ABTestScorecard {
	components := []api.ABTestScoreComponent{
		abScoreHigherBetter("return", "收益贡献", control["totalReturn"], treatment["totalReturn"], 3.0, "累计收益越高越好"),
		abScoreHigherBetter("sharpe", "风险调整收益", control["sharpe"], treatment["sharpe"], 8.0, "夏普比率越高代表单位风险收益越好"),
		abScoreLowerAbsBetter("drawdown", "回撤控制", control["maxDrawdown"], treatment["maxDrawdown"], 2.0, "最大回撤绝对值越低越好"),
		abScoreLowerAbsBetter("volatility", "波动控制", control["volatility"], treatment["volatility"], 0.8, "波动率越低，收益稳定性越好"),
		abScoreLowerBetter("turnover", "换手惩罚", control["totalTurnover"], treatment["totalTurnover"], 0.0002, "换手越高，交易冲击和执行风险越高"),
		abScoreLowerBetter("cost", "成本惩罚", control["totalFees"], treatment["totalFees"], 0.02, "费用越高，净收益侵蚀越明显"),
	}
	sampleDays := min(controlNavPoints, treatmentNavPoints)
	sampleContribution := 0.0
	if sampleDays >= 60 {
		sampleContribution = 6
	} else if sampleDays >= 20 {
		sampleContribution = 3
	} else if sampleDays > 0 {
		sampleContribution = -6
	} else {
		sampleContribution = -12
	}
	components = append(components, api.ABTestScoreComponent{
		Key:          "sample",
		Label:        "样本充分性",
		VariantA:     float64(controlNavPoints),
		VariantB:     float64(treatmentNavPoints),
		Contribution: sampleContribution,
		Direction:    "higher_better",
		Explanation:  "样本天数越多，A/B 结论越稳定；样本不足会降低综合推荐强度",
	})
	variantAScore := 50.0
	variantBScore := 50.0
	for _, component := range components {
		if component.Contribution > 0 {
			variantBScore += component.Contribution
			variantAScore -= component.Contribution / 2
		} else if component.Contribution < 0 {
			variantBScore += component.Contribution
			variantAScore -= component.Contribution / 2
		}
	}
	variantAScore = math.Max(0, math.Min(100, variantAScore))
	variantBScore = math.Max(0, math.Min(100, variantBScore))
	recommended := "tie"
	if variantBScore-variantAScore >= 3 {
		recommended = "treatment"
	} else if variantAScore-variantBScore >= 3 {
		recommended = "control"
	}
	riskNotes := []string{}
	if math.Abs(treatment["maxDrawdown"]) > math.Abs(control["maxDrawdown"])*1.3 && treatment["totalReturn"] > control["totalReturn"] {
		riskNotes = append(riskNotes, "B 策略收益更高但回撤明显放大，建议限制仓位或延长观察期")
	}
	if treatment["volatility"] > control["volatility"]*1.25 && treatment["volatility"] > 0 {
		riskNotes = append(riskNotes, "B 策略波动显著高于 A，可能更依赖行情方向")
	}
	costNotes := []string{}
	if treatment["totalTurnover"] > control["totalTurnover"]*1.5 && treatment["totalTurnover"] > 0 {
		costNotes = append(costNotes, "B 策略换手高于 A，需关注滑点、手续费与执行容量")
	}
	if treatment["totalFees"] > control["totalFees"]*1.5 && treatment["totalFees"] > 0 {
		costNotes = append(costNotes, "B 策略费用显著更高，应使用费后收益复核")
	}
	verdict := "A/B 综合评分接近，建议继续观察。"
	if recommended == "treatment" {
		verdict = "B 策略综合评分更优，可结合置信度与决策差异进入应用前复核。"
	} else if recommended == "control" {
		verdict = "A 策略综合评分更稳健，暂不建议切换到 B。"
	}
	return &api.ABTestScorecard{
		RecommendedVariant: recommended,
		VariantAScore:      variantAScore,
		VariantBScore:      variantBScore,
		ScoreGap:           math.Abs(variantBScore - variantAScore),
		Components:         components,
		RiskNotes:          riskNotes,
		CostNotes:          costNotes,
		Verdict:            verdict,
	}
}

func abScoreHigherBetter(key, label string, control, treatment, weight float64, explanation string) api.ABTestScoreComponent {
	return api.ABTestScoreComponent{Key: key, Label: label, VariantA: control, VariantB: treatment, Contribution: clampABScoreContribution((treatment - control) * weight), Direction: "higher_better", Explanation: explanation}
}

func abScoreLowerBetter(key, label string, control, treatment, weight float64, explanation string) api.ABTestScoreComponent {
	return api.ABTestScoreComponent{Key: key, Label: label, VariantA: control, VariantB: treatment, Contribution: clampABScoreContribution((control - treatment) * weight), Direction: "lower_better", Explanation: explanation}
}

func abScoreLowerAbsBetter(key, label string, control, treatment, weight float64, explanation string) api.ABTestScoreComponent {
	return api.ABTestScoreComponent{Key: key, Label: label, VariantA: control, VariantB: treatment, Contribution: clampABScoreContribution((math.Abs(control) - math.Abs(treatment)) * weight), Direction: "lower_abs_better", Explanation: explanation}
}

func clampABScoreContribution(value float64) float64 {
	return math.Max(-18, math.Min(18, value))
}

func isSupportedABTestVariable(variableType string) bool {
	switch strings.TrimSpace(variableType) {
	case abTestVariableModelChange, abTestVariableStrategyCompare:
		return true
	default:
		return false
	}
}

func buildABVariantMetrics(navs []repository.NavSnapshot, trades []repository.TradeExecution) map[string]float64 {
	metrics := map[string]float64{
		"tradeCount": float64(len(trades)),
	}
	var totalFees float64
	var totalTurnover float64
	for _, trade := range trades {
		totalFees += trade.FeeCommission + trade.FeeStampTax + trade.FeeTransfer
		if trade.Amount.Valid {
			totalTurnover += math.Abs(trade.Amount.Float64)
		} else if trade.FilledPrice.Valid && trade.FilledQty > 0 {
			totalTurnover += math.Abs(trade.FilledPrice.Float64 * trade.FilledQty)
		} else if trade.Price.Valid && trade.Quantity > 0 {
			totalTurnover += math.Abs(trade.Price.Float64 * trade.Quantity)
		}
	}
	metrics["totalFees"] = totalFees
	metrics["totalTurnover"] = totalTurnover

	if len(navs) == 0 {
		return metrics
	}
	start := navs[0]
	end := navs[len(navs)-1]
	metrics["startNav"] = start.NAV
	metrics["endNav"] = end.NAV
	metrics["latestNav"] = end.NAV
	metrics["startAssets"] = start.TotalAssets
	metrics["endAssets"] = end.TotalAssets
	metrics["totalAssets"] = end.TotalAssets
	metrics["navPoints"] = float64(len(navs))
	if start.NAV > 0 {
		metrics["totalReturn"] = (end.NAV/start.NAV - 1) * 100
	}
	if start.TotalAssets > 0 {
		metrics["assetReturn"] = (end.TotalAssets/start.TotalAssets - 1) * 100
	}
	metrics["maxDrawdown"] = calculateABMaxDrawdown(navs)
	metrics["volatility"] = calculateABVolatility(navs)
	metrics["annualizedReturn"] = calculateABAnnualizedReturn(start.TradingDate, end.TradingDate, metrics["totalReturn"])
	if metrics["volatility"] > 0 {
		metrics["sharpe"] = metrics["annualizedReturn"] / metrics["volatility"]
	}
	return metrics
}

func buildABNAVSeries(controlNavs, treatmentNavs []repository.NavSnapshot) []api.ABTestNAVPoint {
	if len(controlNavs) == 0 && len(treatmentNavs) == 0 {
		return nil
	}
	controlByDate := make(map[string]repository.NavSnapshot, len(controlNavs))
	treatmentByDate := make(map[string]repository.NavSnapshot, len(treatmentNavs))
	dates := make(map[string]struct{}, len(controlNavs)+len(treatmentNavs))
	for _, nav := range controlNavs {
		key := nav.TradingDate.Format("2006-01-02")
		controlByDate[key] = nav
		dates[key] = struct{}{}
	}
	for _, nav := range treatmentNavs {
		key := nav.TradingDate.Format("2006-01-02")
		treatmentByDate[key] = nav
		dates[key] = struct{}{}
	}
	ordered := make([]string, 0, len(dates))
	for date := range dates {
		ordered = append(ordered, date)
	}
	sort.Strings(ordered)

	controlStart := firstPositiveNAV(controlNavs)
	treatmentStart := firstPositiveNAV(treatmentNavs)
	series := make([]api.ABTestNAVPoint, 0, len(ordered))
	for _, date := range ordered {
		point := api.ABTestNAVPoint{Date: date}
		if nav, ok := controlByDate[date]; ok {
			value := nav.NAV
			point.VariantA = &value
			if controlStart > 0 {
				ret := (nav.NAV/controlStart - 1) * 100
				point.VariantAReturn = &ret
			}
		}
		if nav, ok := treatmentByDate[date]; ok {
			value := nav.NAV
			point.VariantB = &value
			if treatmentStart > 0 {
				ret := (nav.NAV/treatmentStart - 1) * 100
				point.VariantBReturn = &ret
			}
		}
		if point.VariantAReturn != nil && point.VariantBReturn != nil {
			excess := *point.VariantBReturn - *point.VariantAReturn
			point.ExcessReturn = &excess
		}
		series = append(series, point)
	}
	return series
}

func firstPositiveNAV(navs []repository.NavSnapshot) float64 {
	for _, nav := range navs {
		if nav.NAV > 0 {
			return nav.NAV
		}
	}
	return 0
}

func calculateABMaxDrawdown(navs []repository.NavSnapshot) float64 {
	if len(navs) == 0 {
		return 0
	}
	peak := navs[0].NAV
	maxDrawdown := 0.0
	for _, nav := range navs {
		if nav.NAV > peak {
			peak = nav.NAV
		}
		if peak <= 0 {
			continue
		}
		drawdown := (nav.NAV/peak - 1) * 100
		if drawdown < maxDrawdown {
			maxDrawdown = drawdown
		}
	}
	return maxDrawdown
}

func calculateABVolatility(navs []repository.NavSnapshot) float64 {
	if len(navs) < 2 {
		return 0
	}
	returns := make([]float64, 0, len(navs)-1)
	for i := 1; i < len(navs); i++ {
		prev := navs[i-1].NAV
		if prev <= 0 {
			continue
		}
		returns = append(returns, navs[i].NAV/prev-1)
	}
	if len(returns) < 2 {
		return 0
	}
	var sum float64
	for _, value := range returns {
		sum += value
	}
	mean := sum / float64(len(returns))
	var variance float64
	for _, value := range returns {
		delta := value - mean
		variance += delta * delta
	}
	variance /= float64(len(returns) - 1)
	return math.Sqrt(variance) * math.Sqrt(252) * 100
}

func calculateABAnnualizedReturn(start, end time.Time, totalReturnPercent float64) float64 {
	days := math.Max(end.Sub(start).Hours()/24, 1)
	totalReturn := totalReturnPercent / 100
	if totalReturn <= -1 {
		return -100
	}
	return (math.Pow(1+totalReturn, 365/days) - 1) * 100
}

func determineABWinner(control, treatment map[string]float64) string {
	controlReturn, controlOK := control["totalReturn"]
	treatmentReturn, treatmentOK := treatment["totalReturn"]
	if controlOK && treatmentOK {
		if treatmentReturn > controlReturn {
			return "treatment"
		}
		if treatmentReturn < controlReturn {
			return "control"
		}
	}
	controlSharpe, controlSharpeOK := control["sharpe"]
	treatmentSharpe, treatmentSharpeOK := treatment["sharpe"]
	if controlSharpeOK && treatmentSharpeOK {
		if treatmentSharpe > controlSharpe {
			return "treatment"
		}
		if treatmentSharpe < controlSharpe {
			return "control"
		}
	}
	controlAssets, controlAssetsOK := control["totalAssets"]
	treatmentAssets, treatmentAssetsOK := treatment["totalAssets"]
	if controlAssetsOK && treatmentAssetsOK {
		if treatmentAssets > controlAssets {
			return "treatment"
		}
		if treatmentAssets < controlAssets {
			return "control"
		}
	}
	return "inconclusive"
}

func parseABTestDate(value string) (sql.NullTime, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return sql.NullTime{}, nil
	}
	parsed, err := time.Parse("2006-01-02", trimmed)
	if err != nil {
		return sql.NullTime{}, err
	}
	return sql.NullTime{Time: parsed, Valid: true}, nil
}

func abTestDateRange(test *repository.ABTest) (time.Time, time.Time) {
	start := time.Unix(0, 0).UTC()
	end := time.Now().UTC()
	if test != nil {
		if test.StartDate.Valid {
			start = test.StartDate.Time
		}
		if test.EndDate.Valid {
			end = test.EndDate.Time.Add(24*time.Hour - time.Nanosecond)
		}
	}
	return start, end
}

func convertSubscriptionPlan(plan *subscription.Plan) *api.SubscriptionPlan {
	if plan == nil {
		return nil
	}
	return &api.SubscriptionPlan{
		Tier:              string(plan.Tier),
		Name:              plan.Name,
		PriceCentsMonth:   plan.PriceCentsMonth,
		MaxFunds:          plan.MaxFunds,
		MaxCallsPerDay:    plan.MaxCallsPerDay,
		ModelTiers:        append([]string(nil), plan.ModelTiers...),
		Recommended:       plan.Recommended,
		MaxAgentsPerFund:  plan.MaxAgentsPerFund,
		MaxWorkflowPerDay: plan.MaxWorkflowPerDay,
		AllowCustomKey:    plan.AllowCustomKey,
		AllowABTest:       plan.AllowABTest,
		AllowExport:       plan.AllowExport,
		SimulationCapital: plan.SimulationCapital,
		IncludedTokens:    plan.IncludedTokens,
		Description:       plan.Description,
	}
}

func convertSubscription(sub *subscription.Subscription) *api.Subscription {
	if sub == nil {
		return nil
	}
	return &api.Subscription{
		ID:            sub.ID,
		UserID:        sub.UserID,
		PlanTier:      string(sub.PlanTier),
		Status:        sub.Status,
		StartDate:     sub.StartDate,
		EndDate:       sub.EndDate,
		AutoRenew:     sub.AutoRenew,
		PaymentMethod: sub.PaymentMethod,
	}
}

func (a *walletServiceAdapter) GetWallet(ctx context.Context, userID string) (*api.WalletAccount, error) {
	account, err := a.walletRepo.GetOrCreateByUserID(ctx, userID)
	if err != nil {
		return nil, err
	}
	return convertWalletAccount(account), nil
}

func (a *walletServiceAdapter) ListWalletLedger(ctx context.Context, userID string, offset, limit int) ([]api.WalletLedgerEntry, int, error) {
	entries, total, err := a.walletRepo.ListLedgerByUserID(ctx, userID, offset, limit)
	if err != nil {
		return nil, 0, err
	}
	result := make([]api.WalletLedgerEntry, 0, len(entries))
	for i := range entries {
		result = append(result, convertWalletLedgerEntry(&entries[i]))
	}
	return result, total, nil
}

func convertWalletAccount(account *repository.WalletAccount) *api.WalletAccount {
	if account == nil {
		return nil
	}
	return &api.WalletAccount{
		ID:           account.ID,
		UserID:       account.UserID,
		BaseCurrency: account.BaseCurrency,
		BalanceMinor: account.BalanceMinor,
		CreatedAt:    account.CreatedAt,
		UpdatedAt:    account.UpdatedAt,
	}
}

func convertWalletLedgerEntry(entry *repository.WalletLedgerEntry) api.WalletLedgerEntry {
	if entry == nil {
		return api.WalletLedgerEntry{}
	}
	return api.WalletLedgerEntry{
		ID:                entry.ID,
		AccountID:         entry.AccountID,
		EntryType:         entry.EntryType,
		AmountMinor:       entry.AmountMinor,
		BalanceAfterMinor: entry.BalanceAfterMinor,
		Currency:          entry.Currency,
		ReferenceType:     optionalString(entry.ReferenceType.String),
		ReferenceID:       optionalString(entry.ReferenceID.String),
		CreatedByUserID:   optionalString(entry.CreatedByUserID.String),
		Metadata:          entry.Metadata,
		CreatedAt:         entry.CreatedAt,
	}
}

func convertCompany(company *repository.FundCompany) *api.Company {
	if company == nil {
		return nil
	}
	return &api.Company{
		ID:          company.ID,
		OwnerUserID: company.OwnerUserID,
		Name:        company.Name,
		Description: company.Description.String,
		CreatedAt:   company.CreatedAt,
		UpdatedAt:   company.UpdatedAt,
	}
}

func convertFund(fund *repository.Fund) *api.Fund {
	if fund == nil {
		return nil
	}
	profile := decodeFundMarketProfile(fund.Config)
	return &api.Fund{
		ID:               fund.ID,
		CompanyID:        fund.CompanyID,
		Name:             fund.Name,
		Description:      fund.Description.String,
		TradingMode:      fund.TradingMode,
		InitialCapital:   fund.InitialCapital,
		CurrentCapital:   fund.CurrentCapital,
		TotalAssets:      fund.TotalAssets,
		NAV:              fund.NAV,
		Status:           fund.Status,
		Market:           profile.Market,
		Exchange:         profile.Exchange,
		AssetClass:       profile.AssetClass,
		BaseCurrency:     profile.BaseCurrency,
		BenchmarkSymbol:  profile.BenchmarkSymbol,
		PrimaryDirection: profile.PrimaryDirection,
		CalendarCode:     profile.CalendarCode,
		TimeZone:         profile.TimeZone,
		Universe:              profile.Universe,
		TeamIntervals:         profile.TeamIntervals,
		Specialization:        profile.Specialization,
		HardRisk:              profile.HardRisk,
		AutoExecute:           autoExecuteForAPI(profile.AutoExecute),
		ResearchTier:          normalizeFundResearchTier(profile.ResearchTier),
		ActivityRetentionDays: resolveActivityRetentionDays(profile),
		CreatedAt:             fund.CreatedAt,
		UpdatedAt:        fund.UpdatedAt,
	}
}

// autoExecuteForAPI returns a config the UI can render directly. When
// the persisted profile has no auto-execute block we still return a
// zero-value config (Enabled=false + default thresholds) so the
// settings modal always has something to pre-fill — the user toggling
// On for the first time sees the platform defaults instead of empty
// boxes.
func autoExecuteForAPI(cfg *api.FundAutoExecuteConfig) *api.FundAutoExecuteConfig {
	resolved := resolveAutoExecuteConfig(cfg)
	if cfg != nil {
		resolved.Enabled = cfg.Enabled
	}
	return &resolved
}

func convertWorkflowStatus(run *repository.WorkflowRun) *api.WorkflowStatus {
	if run == nil {
		return nil
	}

	status := run.Status
	if run.Status == "pending" {
		status = "idle"
	}

	result := &api.WorkflowStatus{
		FundID:      run.FundID,
		TradingDate: run.TradingDate.Format("2006-01-02"),
		State:       status,
		Step:        "not_started",
	}
	if run.CurrentStep.Valid && strings.TrimSpace(run.CurrentStep.String) != "" {
		result.Step = run.CurrentStep.String
	}
	if run.StartedAt.Valid {
		result.StartedAt = run.StartedAt.Time.UTC().Format(time.RFC3339)
	}
	if run.CompletedAt.Valid {
		result.CompletedAt = run.CompletedAt.Time.UTC().Format(time.RFC3339)
	}
	result.StepResults = decodeWorkflowStepResults(run.StepResults)
	result.Steps = buildWorkflowTimeline(result.StepResults, result.Step)
	result.TotalSteps = len(result.Steps)
	for _, step := range result.Steps {
		switch strings.ToLower(strings.TrimSpace(step.Status)) {
		case "success", "skipped":
			result.CompletedSteps++
		case "failed", "error":
			result.FailedSteps++
		}
	}
	if result.TotalSteps > 0 {
		result.ProgressPercent = int(math.Round(float64(result.CompletedSteps) / float64(result.TotalSteps) * 100))
	}
	if run.StartedAt.Valid {
		end := time.Now().UTC()
		if run.CompletedAt.Valid {
			end = run.CompletedAt.Time.UTC()
		}
		if end.After(run.StartedAt.Time.UTC()) {
			result.RunningForMs = end.Sub(run.StartedAt.Time.UTC()).Milliseconds()
		}
	}
	return result
}

var workflowTimelineSteps = []string{
	workflow.StepMacroBrief.String(),
	workflow.StepResearchParallel.String(),
	workflow.StepQuantSignals.String(),
	workflow.StepRoundtable.String(),
	workflow.StepPMPlan.String(),
	workflow.StepRiskReview.String(),
	workflow.StepUserApproval.String(),
	workflow.StepTradeExecution.String(),
	workflow.StepSettlement.String(),
	workflow.StepDailyReview.String(),
}

func buildWorkflowTimeline(results map[string]api.WorkflowStepStatus, currentStep string) []api.WorkflowStepStatus {
	steps := make([]api.WorkflowStepStatus, 0, len(workflowTimelineSteps))
	seen := map[string]bool{}
	for index, stepName := range workflowTimelineSteps {
		item := results[stepName]
		item.Step = stepName
		item.Order = index + 1
		item.Label = workflowStepDisplayLabel(stepName)
		if strings.TrimSpace(item.Status) == "" {
			if strings.TrimSpace(currentStep) == stepName {
				item.Status = "running"
			} else {
				item.Status = "pending"
			}
		}
		item.DurationMs = workflowStepDurationMs(item)
		steps = append(steps, item)
		seen[stepName] = true
	}
	for key, item := range results {
		if seen[key] {
			continue
		}
		item.Step = firstNonEmptyValue(item.Step, key)
		item.Order = len(steps) + 1
		item.Label = workflowStepDisplayLabel(item.Step)
		item.DurationMs = workflowStepDurationMs(item)
		steps = append(steps, item)
	}
	return steps
}

func workflowStepDisplayLabel(step string) string {
	switch strings.TrimSpace(step) {
	case workflow.StepMacroBrief.String():
		return "Macro brief"
	case workflow.StepResearchParallel.String():
		return "Research parallel"
	case workflow.StepQuantSignals.String():
		return "Quant signals"
	case workflow.StepRoundtable.String():
		return "Roundtable"
	case workflow.StepPMPlan.String():
		return "PM plan"
	case workflow.StepRiskReview.String():
		return "Risk review"
	case workflow.StepUserApproval.String():
		return "User approval"
	case workflow.StepTradeExecution.String():
		return "Trade execution"
	case workflow.StepSettlement.String():
		return "Settlement"
	case workflow.StepDailyReview.String():
		return "Daily review"
	default:
		return humanizeKey(step)
	}
}

func workflowStepDurationMs(item api.WorkflowStepStatus) int64 {
	if item.DurationMs > 0 {
		return item.DurationMs
	}
	start, startOK := parseRFC3339Time(item.StartedAt)
	end, endOK := parseRFC3339Time(item.EndedAt)
	if !endOK {
		end, endOK = parseRFC3339Time(item.UpdatedAt)
	}
	if !startOK || !endOK || !end.After(start) {
		return 0
	}
	return end.Sub(start).Milliseconds()
}

func parseRFC3339Time(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

func humanizeKey(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	return strings.Title(strings.ReplaceAll(strings.ReplaceAll(trimmed, "_", " "), "-", " "))
}

func convertTeamAgent(member *repository.TeamMember, agent *repository.Agent) api.Agent {
	result := api.Agent{}
	if member != nil {
		result.ID = member.AgentID
		result.AgentID = member.AgentID
		result.Role = member.Role
		result.Focus = member.Focus.String
		result.Status = member.Status
		result.JoinedAt = member.JoinedAt
		if strings.TrimSpace(member.FundID) != "" {
			result.FundID = strings.TrimSpace(member.FundID)
			if strings.TrimSpace(member.Status) != "inactive" {
				result.BindStatus = "bound"
			}
		}
	}
	if agent != nil {
		result.ID = agent.ID
		result.AgentID = agent.ID
		result.Name = agent.Name
		if agent.Role != "" {
			result.Role = agent.Role
		}
		if agent.Focus.Valid {
			result.Focus = agent.Focus.String
		}
		if agent.LLMModel.Valid {
			result.LLMModel = agent.LLMModel.String
		}
		if agent.ModelProvider.Valid {
			result.ModelProvider = agent.ModelProvider.String
		}
		if agent.ModelName.Valid {
			result.ModelName = agent.ModelName.String
		}
		if agent.SystemPrompt.Valid {
			result.SystemPrompt = agent.SystemPrompt.String
		}
		if len(agent.SkillConfig) > 0 && string(agent.SkillConfig) != "null" {
			result.SkillConfig = append(json.RawMessage(nil), agent.SkillConfig...)
		}
		if len(agent.DomainConfig) > 0 && string(agent.DomainConfig) != "null" {
			result.DomainConfig = append(json.RawMessage(nil), agent.DomainConfig...)
		}
		if len(agent.EvolutionConfig) > 0 && string(agent.EvolutionConfig) != "null" {
			result.EvolutionConfig = append(json.RawMessage(nil), agent.EvolutionConfig...)
		}
		if agent.Status != "" {
			result.Status = agent.Status
		}
	}
	return result
}

func convertOwnedAgent(member *repository.TeamMember, agent *repository.Agent) api.Agent {
	result := convertTeamAgent(member, agent)
	result.BindStatus = "unbound"
	if member != nil && strings.TrimSpace(member.FundID) != "" && strings.TrimSpace(member.Status) != "inactive" {
		result.FundID = member.FundID
		result.BindStatus = "bound"
	}
	return result
}

func applyPendingMarketplaceSummary(target *api.Agent, agent *repository.Agent) {
	if target == nil || agent == nil || len(agent.PendingMarketplaceSnapshot) == 0 || string(agent.PendingMarketplaceSnapshot) == "{}" {
		return
	}
	var snapshot marketplaceSnapshot
	if err := json.Unmarshal(agent.PendingMarketplaceSnapshot, &snapshot); err != nil || snapshot.Learning == nil {
		return
	}
	target.LatestLearningSummary = strings.TrimSpace(snapshot.Learning.Summary)
	target.LatestLearningTags = append([]string(nil), snapshot.Learning.Tags...)
	target.LatestLearningAt = snapshot.Learning.CreatedAt
	target.LatestLearningReturn = snapshot.Learning.DailyReturn
}

func convertMemoryEntry(memory *repository.Memory) api.MemoryEntry {
	result := api.MemoryEntry{}
	if memory == nil {
		return result
	}
	result.ID = memory.ID
	result.Title = memory.Title.String
	result.Content = memory.Content
	result.Layer = memory.Layer
	result.Tags = append([]string(nil), memory.Tags...)
	result.CreatedAt = memory.CreatedAt
	result.UpdatedAt = memory.UpdatedAt
	if memory.AgentID.Valid {
		result.AgentID = memory.AgentID.String
	}
	if memory.TradingDate.Valid {
		result.TradingDate = memory.TradingDate.Time.Format("2006-01-02")
	}
	if memory.TemplateKey.Valid && strings.TrimSpace(memory.TemplateKey.String) != "" {
		result.TemplateKey = memory.TemplateKey.String
		// The repo already returns Payload as a json.RawMessage; we
		// pass it through unchanged so the wire format is the
		// caller's bytes verbatim. No copy because RawMessage is
		// just a []byte alias and the response gets Marshalled once.
		if len(memory.Payload) > 0 {
			result.Payload = memory.Payload
		}
	}
	return result
}

func convertAgentLearningRecord(memory *repository.Memory) api.AgentLearningRecord {
	entry := convertMemoryEntry(memory)
	record := api.AgentLearningRecord{
		ID:          entry.ID,
		TradingDate: entry.TradingDate,
		Title:       entry.Title,
		Tags:        entry.Tags,
		CreatedAt:   entry.CreatedAt,
		// Carry the i18n contract through to the agent-learning UI.
		// MemoryEntry already trimmed/validated TemplateKey, so we
		// just propagate the same field set verbatim.
		TemplateKey: entry.TemplateKey,
		Payload:     entry.Payload,
	}
	if memory == nil {
		return record
	}
	if strings.HasPrefix(strings.TrimSpace(memory.Content), "{") {
		var payload struct {
			Summary     string   `json:"summary"`
			Hits        []string `json:"hits"`
			Misses      []string `json:"misses"`
			Lessons     []string `json:"lessons"`
			Adjustments []string `json:"adjustments"`
			DailyReturn *float64 `json:"dailyReturn"`
			Tags        []string `json:"tags"`
		}
		if err := json.Unmarshal([]byte(memory.Content), &payload); err == nil {
			record.Summary = strings.TrimSpace(payload.Summary)
			record.Hits = uniqueNonEmpty(payload.Hits)
			record.Misses = uniqueNonEmpty(payload.Misses)
			record.Lessons = uniqueNonEmpty(payload.Lessons)
			record.Adjustments = uniqueNonEmpty(payload.Adjustments)
			record.DailyReturn = payload.DailyReturn
			if len(payload.Tags) > 0 {
				record.Tags = uniqueNonEmpty(payload.Tags)
			}
			return record
		}
	}
	record.Summary = extractLearningSummary(memory.Content)
	return record
}

func convertMarketInstrument(instrument marketdata.InstrumentRef) api.MarketInstrument {
	return api.MarketInstrument{
		InstrumentKey:      instrument.InstrumentKey,
		Symbol:             instrument.NormalizedSymbol(),
		Market:             instrument.Market,
		Exchange:           instrument.Exchange,
		AssetClass:         instrument.AssetClass,
		InstrumentType:     instrument.InstrumentType,
		QuoteCurrency:      instrument.QuoteCurrency,
		SettlementCurrency: instrument.SettlementCurrency,
		ContractMultiplier: instrument.ContractMultiplier,
		ExpiryDate:         instrument.ExpiryDate,
	}
}

func convertMarketQuote(quote *marketdata.QuoteSnapshot) api.MarketQuote {
	if quote == nil {
		return api.MarketQuote{}
	}
	return api.MarketQuote{
		Symbol:        quote.Symbol,
		InstrumentKey: quote.InstrumentKey,
		Market:        quote.Market,
		Exchange:      quote.Exchange,
		AssetClass:    quote.AssetClass,
		Price:         quote.Price,
		Bid:           quote.Bid,
		Ask:           quote.Ask,
		Volume:        quote.Volume,
		QuoteCurrency: quote.QuoteCurrency,
		AsOf:          quote.AsOf,
		Source:        quote.Source,
		IsStale:       quote.IsStale,
	}
}

func convertMarketNewsItem(item marketdata.NewsItem) api.MarketNewsItem {
	return convertMarketNewsItemWithLocale("", nil, item)
}

func convertMarketNewsItemWithLocale(userID string, runtime *llmRuntime, item marketdata.NewsItem) api.MarketNewsItem {
	items := convertMarketNewsItemsWithLocale(userID, runtime, []marketdata.NewsItem{item})
	if len(items) == 0 {
		return api.MarketNewsItem{}
	}
	return items[0]
}

func convertMarketNewsItems(items []marketdata.NewsItem) []api.MarketNewsItem {
	return convertMarketNewsItemsWithLocale("", nil, items)
}

func convertMarketNewsItemsWithLocale(userID string, runtime *llmRuntime, items []marketdata.NewsItem) []api.MarketNewsItem {
	if len(items) == 0 {
		return nil
	}
	result := make([]api.MarketNewsItem, 0, len(items))
	for _, item := range items {
		result = append(result, api.MarketNewsItem{
			Title:       item.Title,
			TitleZh:     item.TitleZh,
			TitleEn:     item.TitleEn,
			Summary:     item.Summary,
			SummaryZh:   item.SummaryZh,
			SummaryEn:   item.SummaryEn,
			Language:    item.Language,
			URL:         item.URL,
			Source:      item.Source,
			PublishedAt: item.PublishedAt,
			Symbols:     append([]string(nil), item.Symbols...),
		})
	}
	if runtime == nil {
		return result
	}

	// The marketdata layer may already have populated some localized fields
	// (native ZH provider + Phase 2 NewsTranslator). Only fall back to the
	// LLM translator for items where a variant is still missing. This both
	// preserves higher-fidelity native text and avoids spending LLM tokens
	// retranslating the same headlines on every fetch.
	titles := make([]string, 0, len(result))
	titleIndexes := make([]int, 0, len(result))
	summaries := make([]string, 0, len(result))
	summaryIndexes := make([]int, 0, len(result))
	for i := range result {
		title := strings.TrimSpace(result[i].Title)
		if title != "" && (strings.TrimSpace(result[i].TitleZh) == "" || strings.TrimSpace(result[i].TitleEn) == "") {
			titles = append(titles, title)
			titleIndexes = append(titleIndexes, i)
		}
		summary := strings.TrimSpace(result[i].Summary)
		if summary != "" && (strings.TrimSpace(result[i].SummaryZh) == "" || strings.TrimSpace(result[i].SummaryEn) == "") {
			summaries = append(summaries, summary)
			summaryIndexes = append(summaryIndexes, i)
		}
	}

	if len(titleIndexes) > 0 {
		if zh, en := translateBilingualList(userID, runtime, "research_parallel", titles, llm.TierSimple); len(zh) == len(titleIndexes) || len(en) == len(titleIndexes) {
			for idx, itemIndex := range titleIndexes {
				if idx < len(zh) && strings.TrimSpace(result[itemIndex].TitleZh) == "" {
					result[itemIndex].TitleZh = zh[idx]
				}
				if idx < len(en) && strings.TrimSpace(result[itemIndex].TitleEn) == "" {
					result[itemIndex].TitleEn = en[idx]
				}
			}
		}
	}
	if len(summaryIndexes) > 0 {
		if zh, en := translateBilingualList(userID, runtime, "research_parallel", summaries, llm.TierSimple); len(zh) == len(summaryIndexes) || len(en) == len(summaryIndexes) {
			for idx, itemIndex := range summaryIndexes {
				if idx < len(zh) && strings.TrimSpace(result[itemIndex].SummaryZh) == "" {
					result[itemIndex].SummaryZh = zh[idx]
				}
				if idx < len(en) && strings.TrimSpace(result[itemIndex].SummaryEn) == "" {
					result[itemIndex].SummaryEn = en[idx]
				}
			}
		}
	}
	return result
}

func convertMarketResearch(research *marketdata.ResearchContext) *api.MarketResearch {
	return convertMarketResearchWithLocale("", nil, research)
}

func convertMarketResearchWithLocale(userID string, runtime *llmRuntime, research *marketdata.ResearchContext) *api.MarketResearch {
	if research == nil {
		return nil
	}
	result := &api.MarketResearch{
		Instrument:    convertMarketInstrument(research.Instrument),
		News:          convertMarketNewsItemsWithLocale(userID, runtime, research.News),
		Signals:       append([]string(nil), research.Signals...),
		Summary:       research.Summary,
		ProviderNotes: append([]string(nil), research.ProviderNotes...),
		GeneratedAt:   research.GeneratedAt,
	}
	if research.Quote != nil {
		quote := convertMarketQuote(research.Quote)
		result.Quote = &quote
	}
	if research.BenchmarkQuote != nil {
		quote := convertMarketQuote(research.BenchmarkQuote)
		result.BenchmarkQuote = &quote
	}
	return result
}

func normalizeAgentRole(role string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "pm":
		return "pm", nil
	case "researcher":
		return "researcher", nil
	case "trader":
		return "trader", nil
	case "risk":
		return "risk", nil
	default:
		return "", api.ErrBadInput
	}
}

func normalizeAgentFocus(focus string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(focus))
	if trimmed == "" {
		return "", nil
	}
	switch trimmed {
	case "stock", "fundamental", "macro":
		return trimmed, nil
	default:
		return "", api.ErrBadInput
	}
}

func (s *teamServiceAdapter) enrichLatestLearning(ctx context.Context, fundID string, agent *api.Agent) error {
	if s.memoryRepo == nil || agent == nil || strings.TrimSpace(fundID) == "" || strings.TrimSpace(agent.AgentID) == "" {
		return nil
	}
	memories, err := s.memoryRepo.GetByAgent(ctx, fundID, strings.TrimSpace(agent.AgentID))
	if err != nil {
		return nil
	}
	latest := latestLearningMemory(memories)
	if latest == nil {
		return nil
	}
	agent.LatestLearningSummary = extractLearningSummary(latest.Content)
	agent.LatestLearningAt = latest.CreatedAt
	if latest.AgentID.Valid {
		agent.AgentID = latest.AgentID.String
	}
	if dailyReturn, ok := extractLearningDailyReturn(latest.Content); ok {
		agent.LatestLearningReturn = &dailyReturn
	}
	agent.LatestLearningTags = append([]string(nil), latest.Tags...)
	return nil
}

func latestLearningMemory(memories []repository.Memory) *repository.Memory {
	for i := range memories {
		if !isLearningMemory(memories[i]) {
			continue
		}
		memory := memories[i]
		return &memory
	}
	return nil
}

func isLearningMemory(memory repository.Memory) bool {
	for _, tag := range memory.Tags {
		if strings.EqualFold(strings.TrimSpace(tag), "self_learning") {
			return true
		}
	}
	return false
}

func extractLearningSummary(content string) string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "{") {
		var payload struct {
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal([]byte(trimmed), &payload); err == nil {
			return strings.TrimSpace(payload.Summary)
		}
	}
	return trimmed
}

func extractLearningDailyReturn(content string) (float64, bool) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
		return 0, false
	}
	var payload struct {
		DailyReturn *float64 `json:"dailyReturn"`
	}
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil || payload.DailyReturn == nil {
		return 0, false
	}
	return *payload.DailyReturn, true
}

func normalizeMemoryLayer(layer string) (string, error) {
	// Keep this allow-list in sync with the memories_layer_check DB
	// constraint (see migration 039_attribution_memory_layer.sql).
	// 'attribution' is the layer that attribution.Service writes from
	// the lot ledger — operator UI was getting 400 "bad input" trying
	// to read it back even though the rows existed (P3 sweep Test 14).
	switch strings.ToLower(strings.TrimSpace(layer)) {
	case "long_term", "daily", "dreams", "agent", "analysis", "attribution":
		return strings.ToLower(strings.TrimSpace(layer)), nil
	default:
		return "", api.ErrBadInput
	}
}

func buildAgentName(role, focus string) string {
	base := map[string]string{
		"pm":         "Portfolio Manager",
		"researcher": "Research Agent",
		"trader":     "Trader Agent",
		"risk":       "Risk Agent",
	}[role]
	if focus == "" {
		return base
	}
	return base + " · " + strings.ToUpper(focus)
}

// defaultAgentModel was removed in T1 (2026-05-23). It used to seed
// every freshly-created agent with a hard-coded model name (claude-
// sonnet-4-6 for PM/risk, claude-opus-4-7 for researcher, gpt-4o for
// trader). That auto-binding silently locked the agent to whichever
// provider's key happened to be present in .env — and worse, on
// deployments configured for a Gemini relay (no Anthropic key), every
// PM call ended up routed to gemini through the platform default
// while the UI still showed "claude". The current contract is: an
// agent without an explicit model_provider / model_name reads the
// platform default from .env at request time. The operator must
// click into the agent editor and pick a model to opt in to anything
// else.

func mergeWorkflowStepResult(raw json.RawMessage, step, status string, updatedAt time.Time) json.RawMessage {
	payload := map[string]map[string]string{}
	if len(raw) > 0 && string(raw) != "null" && json.Valid(raw) {
		_ = json.Unmarshal(raw, &payload)
	}
	payload[step] = map[string]string{
		"status":    status,
		"updatedAt": updatedAt.UTC().Format(time.RFC3339),
	}
	merged, err := json.Marshal(payload)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return merged
}

func syncWorkflowRun(run *repository.WorkflowRun, state workflow.WorkflowState) {
	run.FundID = state.FundID
	run.TradingDate = parseTradingDateOrNow(state.TradingDate)
	run.Status = repositoryWorkflowStatus(state.Status)
	if stepName := state.CurrentStep.String(); stepName != "" && !strings.HasPrefix(stepName, "unknown_step") {
		run.CurrentStep = sql.NullString{String: stepName, Valid: true}
	}
	if !state.StartedAt.IsZero() {
		run.StartedAt = sql.NullTime{Time: state.StartedAt.UTC(), Valid: true}
	}
	if !state.EndedAt.IsZero() {
		run.CompletedAt = sql.NullTime{Time: state.EndedAt.UTC(), Valid: true}
	} else {
		run.CompletedAt = sql.NullTime{}
	}
	run.StepResults = encodeWorkflowStepResults(state.StepResults)
}

func repositoryWorkflowStatus(status workflow.RunStatus) string {
	switch status {
	case workflow.RunStatusCompleted:
		return "completed"
	case workflow.RunStatusFailed:
		return "failed"
	case workflow.RunStatusCancelled:
		return "cancelled"
	case workflow.RunStatusRejected:
		return "rejected"
	case workflow.RunStatusPaused:
		return "paused"
	case workflow.RunStatusRunning:
		return "running"
	case workflow.RunStatusPending:
		return "pending"
	default:
		return "pending"
	}
}

func parseRepositoryWorkflowStatus(status string) workflow.RunStatus {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed":
		return workflow.RunStatusCompleted
	case "failed":
		return workflow.RunStatusFailed
	case "cancelled":
		return workflow.RunStatusCancelled
	case "rejected":
		return workflow.RunStatusRejected
	case "paused":
		return workflow.RunStatusPaused
	case "running":
		return workflow.RunStatusRunning
	case "pending":
		return workflow.RunStatusPending
	default:
		return workflow.RunStatusPending
	}
}

func convertWorkflowStateToStatus(fundID string, state workflow.WorkflowState) *api.WorkflowStatus {
	status := repositoryWorkflowStatus(state.Status)
	if status == "pending" {
		status = "idle"
	}
	result := &api.WorkflowStatus{
		FundID:      strings.TrimSpace(fundID),
		TradingDate: strings.TrimSpace(state.TradingDate),
		State:       status,
		Step:        "not_started",
		StepResults: decodeWorkflowStepResults(encodeWorkflowStepResults(state.StepResults)),
	}
	if stepName := state.CurrentStep.String(); stepName != "" && !strings.HasPrefix(stepName, "unknown_step") {
		result.Step = stepName
	}
	if !state.StartedAt.IsZero() {
		result.StartedAt = state.StartedAt.UTC().Format(time.RFC3339)
	}
	if !state.EndedAt.IsZero() {
		result.CompletedAt = state.EndedAt.UTC().Format(time.RFC3339)
	}
	return result
}

func encodeWorkflowStepResults(results []workflow.StepResult) json.RawMessage {
	payload := make(map[string]api.WorkflowStepStatus, len(results))
	for _, result := range results {
		stepName := result.Step.String()
		status := api.WorkflowStepStatus{
			Step:      stepName,
			Status:    result.Status,
			UpdatedAt: result.EndedAt.UTC().Format(time.RFC3339),
		}
		if !result.StartedAt.IsZero() {
			status.StartedAt = result.StartedAt.UTC().Format(time.RFC3339)
		}
		if !result.EndedAt.IsZero() {
			status.EndedAt = result.EndedAt.UTC().Format(time.RFC3339)
			status.UpdatedAt = result.EndedAt.UTC().Format(time.RFC3339)
		}
		if result.Error != nil {
			status.Error = result.Error.Error()
		}
		payload[stepName] = status
	}
	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	ordered := make(map[string]api.WorkflowStepStatus, len(keys))
	for _, key := range keys {
		ordered[key] = payload[key]
	}
	encoded, err := json.Marshal(ordered)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return encoded
}

func decodeWorkflowStepResults(raw json.RawMessage) map[string]api.WorkflowStepStatus {
	if len(raw) == 0 || string(raw) == "null" || !json.Valid(raw) {
		return nil
	}
	var payload map[string]api.WorkflowStepStatus
	if err := json.Unmarshal(raw, &payload); err == nil && len(payload) > 0 {
		for key, item := range payload {
			if item.Step == "" {
				item.Step = key
				payload[key] = item
			}
		}
		return payload
	}
	var legacy map[string]map[string]string
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return nil
	}
	payload = make(map[string]api.WorkflowStepStatus, len(legacy))
	for key, item := range legacy {
		payload[key] = api.WorkflowStepStatus{
			Step:      key,
			Status:    item["status"],
			UpdatedAt: item["updatedAt"],
		}
	}
	return payload
}

func parseTradingDateOrNow(value string) time.Time {
	parsed, err := time.Parse("2006-01-02", strings.TrimSpace(value))
	if err != nil {
		return workflowTradingDate(time.Now().UTC())
	}
	return parsed.UTC()
}

func workflowRunAwaitingApproval(run *repository.WorkflowRun) bool {
	if run == nil {
		return false
	}
	status := strings.ToLower(strings.TrimSpace(run.Status))
	if status != "paused" && status != "pending" && status != "failed" {
		return false
	}
	currentStep := strings.ToLower(strings.TrimSpace(run.CurrentStep.String))
	if currentStep != workflow.StepUserApproval.String() {
		return false
	}
	stepResults := decodeWorkflowStepResults(run.StepResults)
	approval := stepResults[workflow.StepUserApproval.String()]
	approvalStatus := strings.ToLower(strings.TrimSpace(approval.Status))
	return approvalStatus == "pending" || approvalStatus == ""
}

func normalizeTradingDate(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

func parseWorkflowStepOrZero(value string) workflow.WorkflowStep {
	step, err := parseWorkflowStep(value)
	if err != nil {
		return workflow.StepMacroBrief
	}
	return step
}

func decodeWorkflowStepResultsToRuntime(raw json.RawMessage) []workflow.StepResult {
	decoded := decodeWorkflowStepResults(raw)
	if len(decoded) == 0 {
		return nil
	}
	results := make([]workflow.StepResult, 0, len(decoded))
	for _, stepName := range decisionTraceStepOrder {
		item, ok := decoded[stepName]
		if !ok {
			continue
		}
		step, err := parseWorkflowStep(stepName)
		if err != nil {
			continue
		}
		result := workflow.StepResult{Step: step, Status: strings.TrimSpace(item.Status)}
		if item.Error != "" {
			result.Error = errors.New(item.Error)
		}
		if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(item.StartedAt)); err == nil {
			result.StartedAt = parsed.UTC()
		}
		if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(item.EndedAt)); err == nil {
			result.EndedAt = parsed.UTC()
		} else if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(item.UpdatedAt)); err == nil {
			result.EndedAt = parsed.UTC()
		}
		results = append(results, result)
	}
	return results
}

func nullTimeValue(value sql.NullTime) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return value.Time.UTC()
}

func parseWorkflowStep(value string) (workflow.WorkflowStep, error) {
	switch strings.TrimSpace(value) {
	case "macro_brief":
		return workflow.StepMacroBrief, nil
	case "research_parallel":
		return workflow.StepResearchParallel, nil
	case "quant_signals":
		return workflow.StepQuantSignals, nil
	case "roundtable":
		return workflow.StepRoundtable, nil
	case "pm_plan":
		return workflow.StepPMPlan, nil
	case "risk_review":
		return workflow.StepRiskReview, nil
	case "user_approval":
		return workflow.StepUserApproval, nil
	case "trade_execution":
		return workflow.StepTradeExecution, nil
	case "settlement":
		return workflow.StepSettlement, nil
	case "daily_review":
		return workflow.StepDailyReview, nil
	default:
		return 0, api.ErrBadInput
	}
}

func parseSkillConfig(raw json.RawMessage) parsedSkillConfig {
	config := parsedSkillConfig{Enabled: true}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return config
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return parsedSkillConfig{}
	}
	return config
}

func resolveSkills(agent *repository.Agent, scenario skillScenario) []resolvedSkill {
	if agent == nil {
		return nil
	}
	config := parseSkillConfig(agent.SkillConfig)
	if !config.Enabled {
		return nil
	}
	resolved := make([]resolvedSkill, 0, len(config.Skills))
	for _, skill := range config.Skills {
		if !skillEntryIsActive(skill) || !skillMatchesScenario(skill, scenario) {
			continue
		}
		content := strings.TrimSpace(skill.Content)
		if content == "" {
			continue
		}
		resolved = append(resolved, resolvedSkill{
			Key:         strings.TrimSpace(skill.Key),
			Name:        strings.TrimSpace(skill.Name),
			Description: strings.TrimSpace(skill.Description),
			Content:     content,
			Priority:    skill.Priority,
		})
	}
	sort.SliceStable(resolved, func(i, j int) bool {
		return resolved[i].Priority > resolved[j].Priority
	})
	return resolved
}

func skillEntryEnabled(skill parsedSkillEntry) bool {
	if skill.Enabled == nil {
		return true
	}
	return *skill.Enabled
}

func skillMatchesScenario(skill parsedSkillEntry, scenario skillScenario) bool {
	if !matchesAnyFold(skill.Match.Roles, scenario.AgentRole) {
		return false
	}
	if !matchesAnyFold(skill.Match.Focuses, scenario.AgentFocus) {
		return false
	}
	if !matchesAnyFold(skill.Match.WorkflowSteps, scenario.WorkflowStep) {
		return false
	}
	if len(skill.Match.ScenarioKeywords) == 0 {
		return true
	}
	for _, keyword := range skill.Match.ScenarioKeywords {
		trimmedKeyword := strings.ToLower(strings.TrimSpace(keyword))
		if trimmedKeyword == "" {
			continue
		}
		for _, candidate := range scenario.Keywords {
			if strings.Contains(strings.ToLower(candidate), trimmedKeyword) {
				return true
			}
		}
	}
	return false
}

func matchesAnyFold(expected []string, actual string) bool {
	if len(expected) == 0 {
		return true
	}
	trimmedActual := strings.TrimSpace(actual)
	if trimmedActual == "" {
		return false
	}
	for _, item := range expected {
		if strings.EqualFold(strings.TrimSpace(item), trimmedActual) {
			return true
		}
	}
	return false
}

func renderSkillContext(lang UserLanguage, skills []resolvedSkill) string {
	if len(skills) == 0 {
		return ""
	}
	var (
		header      = "匹配技能：\n"
		fallbackName = "技能"
	)
	if lang == UserLanguageEN {
		header = "Matched skills:\n"
		fallbackName = "skill"
	}
	var builder strings.Builder
	builder.WriteString(header)
	for _, skill := range skills {
		name := skill.Name
		if name == "" {
			name = skill.Key
		}
		if name == "" {
			name = fallbackName
		}
		builder.WriteString("- ")
		builder.WriteString(name)
		if skill.Description != "" {
			builder.WriteString(": ")
			builder.WriteString(skill.Description)
		}
		builder.WriteString("\n")
		builder.WriteString(strings.TrimSpace(skill.Content))
		builder.WriteString("\n")
	}
	return strings.TrimSpace(builder.String())
}

func buildResearcherSkillContext(lang UserLanguage, agent *repository.Agent, step string, reports []workflow.ResearchReport) string {
	if agent == nil {
		return ""
	}
	keywords := make([]string, 0, len(reports))
	for _, report := range reports {
		keywords = append(keywords, report.Content, report.AgentID, string(report.Focus))
	}
	return renderSkillContext(lang, resolveSkills(agent, skillScenario{
		AgentRole:    agent.Role,
		AgentFocus:   agent.Focus.String,
		WorkflowStep: step,
		Keywords:     keywords,
	}))
}

func buildPMSkillContext(lang UserLanguage, agent *repository.Agent, roundtable *workflow.RoundtableResult) string {
	if agent == nil {
		return ""
	}
	keywords := []string{}
	if roundtable != nil {
		keywords = append(keywords, roundtable.Consensus...)
	}
	return renderSkillContext(lang, resolveSkills(agent, skillScenario{
		AgentRole:    agent.Role,
		AgentFocus:   agent.Focus.String,
		WorkflowStep: workflow.StepPMPlan.String(),
		Keywords:     keywords,
	}))
}

func buildTraderSkillContext(lang UserLanguage, agent *repository.Agent, plan *repository.InvestmentPlan, actions []repository.PlanAction) string {
	if agent == nil {
		return ""
	}
	keywords := collectPlanKeywords(plan, actions, nil)
	return renderSkillContext(lang, resolveSkills(agent, skillScenario{
		AgentRole:    agent.Role,
		AgentFocus:   agent.Focus.String,
		WorkflowStep: workflow.StepTradeExecution.String(),
		Keywords:     keywords,
	}))
}

func buildRiskSkillContext(lang UserLanguage, agent *repository.Agent, plan *repository.InvestmentPlan, actions []repository.PlanAction, positions []repository.HoldingPosition) string {
	if agent == nil {
		return ""
	}
	keywords := collectPlanKeywords(plan, actions, positions)
	return renderSkillContext(lang, resolveSkills(agent, skillScenario{
		AgentRole:    agent.Role,
		AgentFocus:   agent.Focus.String,
		WorkflowStep: workflow.StepRiskReview.String(),
		Keywords:     keywords,
	}))
}

func collectPlanKeywords(plan *repository.InvestmentPlan, actions []repository.PlanAction, positions []repository.HoldingPosition) []string {
	keywords := make([]string, 0, len(actions)*4+len(positions)+2)
	if plan != nil {
		if plan.Reasoning.Valid {
			keywords = append(keywords, plan.Reasoning.String)
		}
		if !plan.TradingDate.IsZero() {
			keywords = append(keywords, plan.TradingDate.Format("2006-01-02"))
		}
	}
	for _, action := range actions {
		keywords = append(keywords, action.Symbol, action.Action)
		if action.Reasoning.Valid {
			keywords = append(keywords, action.Reasoning.String)
		}
	}
	for _, position := range positions {
		keywords = append(keywords, position.Symbol)
		if position.Name.Valid {
			keywords = append(keywords, position.Name.String)
		}
	}
	return keywords
}

func appendSkillContext(base, skillContext string) string {
	trimmedSkillContext := strings.TrimSpace(skillContext)
	if trimmedSkillContext == "" {
		return strings.TrimSpace(base)
	}
	trimmedBase := strings.TrimSpace(base)
	if trimmedBase == "" {
		return trimmedSkillContext
	}
	return trimmedBase + "\n\n" + trimmedSkillContext
}

type fundFocusLabels struct {
	header              string
	market              string
	assetClass          string
	primaryDirection    string
	universeMode        string
	universeSymbols     string
	universeThemes      string
	universeSectors     string
	universeFilters     string
	teamMarkets         string
	teamAssetClasses    string
	teamThemes          string
	teamInstruments     string
	teamStyleHints      string
	memberMarkets       string
	memberAssetClasses  string
	memberThemes        string
	memberInstruments   string
	memberStyleHints    string
	memberPatterns      string
	recentStrengths     string
	recentLessons       string
	recentAdjustments   string
	specHeader          string
	listSep             string
	colon               string
	strengthSep         string
	pipeSep             string
}

func fundFocusLabelSet(lang UserLanguage) fundFocusLabels {
	if lang == UserLanguageEN {
		return fundFocusLabels{
			header:             "Fund focus:",
			market:             "- market: ",
			assetClass:         "- asset class: ",
			primaryDirection:   "- primary direction: ",
			universeMode:       "- universe mode: ",
			universeSymbols:    "- universe symbols: ",
			universeThemes:     "- universe themes: ",
			universeSectors:    "- universe sectors: ",
			universeFilters:    "- universe custom filters: ",
			teamMarkets:        "- team specialization markets: ",
			teamAssetClasses:   "- team specialization asset classes: ",
			teamThemes:         "- team specialization themes: ",
			teamInstruments:    "- team specialization instruments: ",
			teamStyleHints:     "- team specialization style hints: ",
			memberMarkets:      "- member specialization markets: ",
			memberAssetClasses: "- member specialization asset classes: ",
			memberThemes:       "- member specialization themes: ",
			memberInstruments:  "- member specialization instruments: ",
			memberStyleHints:   "- member specialization style hints: ",
			memberPatterns:     "- member specialization patterns: ",
			recentStrengths:    "- recent learned strengths: ",
			recentLessons:      "- recent learned lessons: ",
			recentAdjustments:  "- recent learned adjustments: ",
			specHeader:         "Specialization context:",
			listSep:            ", ",
			colon:              ": ",
			strengthSep:        "; ",
			pipeSep:            " | ",
		}
	}
	return fundFocusLabels{
		header:             "基金研究焦点：",
		market:             "- 市场：",
		assetClass:         "- 资产类别：",
		primaryDirection:   "- 主要方向：",
		universeMode:       "- 标的池模式：",
		universeSymbols:    "- 标的池代码：",
		universeThemes:     "- 标的池主题：",
		universeSectors:    "- 标的池行业：",
		universeFilters:    "- 标的池自定义过滤器：",
		teamMarkets:        "- 团队擅长市场：",
		teamAssetClasses:   "- 团队擅长资产类别：",
		teamThemes:         "- 团队擅长主题：",
		teamInstruments:    "- 团队擅长标的：",
		teamStyleHints:     "- 团队风格提示：",
		memberMarkets:      "- 成员擅长市场：",
		memberAssetClasses: "- 成员擅长资产类别：",
		memberThemes:       "- 成员擅长主题：",
		memberInstruments:  "- 成员擅长标的：",
		memberStyleHints:   "- 成员风格提示：",
		memberPatterns:     "- 成员模式标签：",
		recentStrengths:    "- 近期学习优势：",
		recentLessons:      "- 近期学习经验：",
		recentAdjustments:  "- 近期学习调整：",
		specHeader:         "专长背景：",
		listSep:            "、",
		colon:              "：",
		strengthSep:        "；",
		pipeSep:            " | ",
	}
}

func buildFundFocusContext(lang UserLanguage, fund *repository.Fund) string {
	if fund == nil {
		return ""
	}
	labels := fundFocusLabelSet(lang)
	profile := decodeFundMarketProfile(fund.Config)
	lines := []string{}
	if value := strings.TrimSpace(profile.Market); value != "" {
		lines = append(lines, labels.market+value)
	}
	if value := strings.TrimSpace(profile.AssetClass); value != "" {
		lines = append(lines, labels.assetClass+value)
	}
	if value := strings.TrimSpace(profile.PrimaryDirection); value != "" {
		lines = append(lines, labels.primaryDirection+value)
	}
	if profile.Universe != nil {
		if value := strings.TrimSpace(profile.Universe.Mode); value != "" {
			lines = append(lines, labels.universeMode+value)
		}
		if len(profile.Universe.Symbols) > 0 {
			lines = append(lines, labels.universeSymbols+strings.Join(profile.Universe.Symbols, labels.listSep))
		}
		if len(profile.Universe.Themes) > 0 {
			lines = append(lines, labels.universeThemes+strings.Join(profile.Universe.Themes, labels.listSep))
		}
		if len(profile.Universe.Sectors) > 0 {
			lines = append(lines, labels.universeSectors+strings.Join(profile.Universe.Sectors, labels.listSep))
		}
		if len(profile.Universe.CustomFilters) > 0 {
			lines = append(lines, labels.universeFilters+strings.Join(profile.Universe.CustomFilters, labels.listSep))
		}
	}
	if profile.Specialization != nil && profile.Specialization.Team != nil {
		if len(profile.Specialization.Team.Markets) > 0 {
			lines = append(lines, labels.teamMarkets+strings.Join(profile.Specialization.Team.Markets, labels.listSep))
		}
		if len(profile.Specialization.Team.AssetClasses) > 0 {
			lines = append(lines, labels.teamAssetClasses+strings.Join(profile.Specialization.Team.AssetClasses, labels.listSep))
		}
		if len(profile.Specialization.Team.Themes) > 0 {
			lines = append(lines, labels.teamThemes+strings.Join(profile.Specialization.Team.Themes, labels.listSep))
		}
		if len(profile.Specialization.Team.Instruments) > 0 {
			lines = append(lines, labels.teamInstruments+strings.Join(profile.Specialization.Team.Instruments, labels.listSep))
		}
		if len(profile.Specialization.Team.StyleHints) > 0 {
			lines = append(lines, labels.teamStyleHints+strings.Join(profile.Specialization.Team.StyleHints, labels.listSep))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return labels.header + "\n" + strings.Join(lines, "\n")
}

func buildAgentSpecializationContext(lang UserLanguage, agent *repository.Agent, fund *repository.Fund) string {
	if agent == nil {
		return ""
	}
	labels := fundFocusLabelSet(lang)
	domainSpecialization := extractAgentSpecialization(agent)
	learnedSpecialization := extractEvolutionSpecialization(agent.EvolutionConfig)
	lines := []string{}
	if fund != nil {
		profile := decodeFundMarketProfile(fund.Config)
		if profile.Specialization != nil && profile.Specialization.Team != nil {
			if len(profile.Specialization.Team.Markets) > 0 {
				lines = append(lines, labels.teamMarkets+strings.Join(profile.Specialization.Team.Markets, labels.listSep))
			}
			if len(profile.Specialization.Team.Themes) > 0 {
				lines = append(lines, labels.teamThemes+strings.Join(profile.Specialization.Team.Themes, labels.listSep))
			}
			if len(profile.Specialization.Team.Instruments) > 0 {
				lines = append(lines, labels.teamInstruments+strings.Join(profile.Specialization.Team.Instruments, labels.listSep))
			}
			if len(profile.Specialization.Team.StyleHints) > 0 {
				lines = append(lines, labels.teamStyleHints+strings.Join(profile.Specialization.Team.StyleHints, labels.listSep))
			}
		}
	}
	if domainSpecialization != nil {
		if len(domainSpecialization.Markets) > 0 {
			lines = append(lines, labels.memberMarkets+strings.Join(domainSpecialization.Markets, labels.listSep))
		}
		if len(domainSpecialization.AssetClasses) > 0 {
			lines = append(lines, labels.memberAssetClasses+strings.Join(domainSpecialization.AssetClasses, labels.listSep))
		}
		if len(domainSpecialization.Themes) > 0 {
			lines = append(lines, labels.memberThemes+strings.Join(domainSpecialization.Themes, labels.listSep))
		}
		if len(domainSpecialization.Instruments) > 0 {
			lines = append(lines, labels.memberInstruments+strings.Join(domainSpecialization.Instruments, labels.listSep))
		}
		if len(domainSpecialization.StyleHints) > 0 {
			lines = append(lines, labels.memberStyleHints+strings.Join(domainSpecialization.StyleHints, labels.listSep))
		}
		if len(domainSpecialization.Patterns) > 0 {
			lines = append(lines, labels.memberPatterns+strings.Join(domainSpecialization.Patterns, labels.listSep))
		}
	}
	if strengths := topSpecializationScoreLines(learnedSpecialization); len(strengths) > 0 {
		lines = append(lines, labels.recentStrengths+strings.Join(strengths, labels.strengthSep))
	}
	if len(learnedSpecialization.RecentLessons) > 0 {
		lines = append(lines, labels.recentLessons+strings.Join(limitStrings(learnedSpecialization.RecentLessons, 3), labels.pipeSep))
	}
	if len(learnedSpecialization.LastAdjustments) > 0 {
		lines = append(lines, labels.recentAdjustments+strings.Join(limitStrings(learnedSpecialization.LastAdjustments, 3), labels.pipeSep))
	}
	if len(lines) == 0 {
		return ""
	}
	return labels.specHeader + "\n" + strings.Join(lines, "\n")
}

func buildRuntimeFundContextsByID(ctx context.Context, fundID string, agent *repository.Agent, fundRepo *repository.FundRepo) (string, string) {
	lang := LanguageFromContext(ctx)
	memberContext := buildAgentSpecializationContext(lang, agent, nil)
	if fundRepo == nil || strings.TrimSpace(fundID) == "" {
		return "", memberContext
	}
	fund, err := fundRepo.GetByID(ctx, fundID)
	if err != nil {
		return "", memberContext
	}
	return buildFundFocusContext(lang, fund), buildAgentSpecializationContext(lang, agent, fund)
}

func buildFundFocusContextByID(ctx context.Context, fundID string, fundRepo *repository.FundRepo) string {
	focusContext, _ := buildRuntimeFundContextsByID(ctx, fundID, nil, fundRepo)
	return focusContext
}

func (runtimeEventBus) Publish(context.Context, workflow.WorkflowEvent) error {
	return nil
}

// persistingEventBus wraps an optional delegate (e.g. the activity SSE bus)
// and additionally writes the orchestrator state to the workflow_runs table
// after every meaningful transition. This ensures the admin/scheduler views
// of workflow progress stay fresh even when RunFull is blocked inside
// WaitForDecision (e.g. user approval) for an extended period.
type persistingEventBus struct {
	delegate    workflow.EventBus
	adapter     *workflowServiceAdapter
	fundID      string
	tradingDate time.Time
}

func newPersistingEventBus(adapter *workflowServiceAdapter, fundID string, tradingDate time.Time, delegate workflow.EventBus) *persistingEventBus {
	return &persistingEventBus{
		delegate:    delegate,
		adapter:     adapter,
		fundID:      strings.TrimSpace(fundID),
		tradingDate: normalizeTradingDate(tradingDate),
	}
}

func (b *persistingEventBus) Publish(ctx context.Context, evt workflow.WorkflowEvent) error {
	if b == nil {
		return nil
	}
	if b.delegate != nil {
		_ = b.delegate.Publish(ctx, evt)
	}
	if b.adapter == nil || !workflowEventPersistsState(evt.Type) || evt.Snapshot == nil {
		return nil
	}
	tradingDate := b.tradingDate
	if tradingDate.IsZero() && strings.TrimSpace(evt.Snapshot.TradingDate) != "" {
		if parsed, err := time.Parse("2006-01-02", strings.TrimSpace(evt.Snapshot.TradingDate)); err == nil {
			tradingDate = parsed.UTC()
		}
	}
	if tradingDate.IsZero() {
		return nil
	}
	status, err := b.adapter.persistRuntimeState(b.fundID, evt.Snapshot, tradingDate)
	if err != nil {
		slog.Default().Warn("failed to persist workflow state from event",
			"fund_id", b.fundID,
			"event_type", evt.Type,
			"err", err,
		)
		return nil
	}
	if status != nil && b.adapter.metrics != nil {
		b.adapter.metrics.ObserveWorkflow(b.fundID, status.State, status.Step)
	}
	return nil
}

// workflowEventPersistsState returns true for event types that materially
// change the workflow row (status / current_step / step_results / timestamps).
// Excludes purely informational events to keep DB write rate sane.
func workflowEventPersistsState(eventType string) bool {
	switch eventType {
	case "run_started",
		"run_completed",
		"run_failed",
		"run_rejected",
		"run_resumed",
		"step_started",
		"step_completed",
		"step_failed",
		"step_paused",
		"step_skipped",
		"awaiting_user":
		return true
	default:
		return false
	}
}

// researcherMaxInstrumentsPerStep is the soft safety cap that
// replaced the hardcoded `i >= 3` truncation in
// buildUniverseResearchContent / buildQuantResearchContent (Sprint 1
// / S3). A 12-symbol fund was losing 75% of its coverage to the
// old cap; 16 keeps production funds (≤ 12 symbols typical) at
// 100% coverage while still bounding the prompt for a
// misconfigured 100-symbol universe. Increase via PR if a future
// universe legitimately needs more.
const researcherMaxInstrumentsPerStep = 16

// researchLLMSynthesis is the shape the Sprint 1 / S3 LLM call
// returns. summary is the single-sentence headline; bullets the
// most-actionable 3-5 observations; confidence is the LLM's own
// 0..1 rating of how decisive the underlying signals are.
type researchLLMSynthesis struct {
	Summary    string   `json:"summary"`
	Bullets    []string `json:"bullets"`
	Confidence float64  `json:"confidence"`
}

// summariseResearchWithLLM runs the provider-stitched research text
// through a TierStandard LLM call and returns the legacy text with
// an LLM-generated synthesis prepended. Sprint 1 / S3 wiring.
//
// Failures degrade gracefully: any error path (no runtime, empty
// input, LLM error, JSON parse failure) returns the original
// content unchanged. The synthesis adds 3-8 lines to the top of the
// research text; the downstream debate / persistence layer sees
// both the synthesis AND the original signals so no information is
// lost.
//
// stepKind is a short tag ("macro" / "stock" / "fundamental" /
// "quant") used both for the LLM step_name attribute and the
// human-readable section header.
func (p runtimeResearcherPool) summariseResearchWithLLM(ctx context.Context, agent *repository.Agent, fundID, stepKind, providerText string) string {
	if p.llmRuntime == nil {
		return providerText
	}
	cleaned := strings.TrimSpace(providerText)
	if cleaned == "" {
		return providerText
	}
	// Bail if there's no meaningful content (just a "unavailable"
	// stub) — running an LLM on "macro brief unavailable: market
	// data source disabled" wastes a token budget.
	lower := strings.ToLower(cleaned)
	if strings.Contains(lower, "unavailable") && len(cleaned) < 200 {
		return providerText
	}
	lang := LanguageFromContext(ctx)
	zh := lang != UserLanguageEN

	systemPrompt := "You are a senior buy-side research analyst. The user will give you a researcher's raw notes for one trading day. Distil them into JSON of the shape {\"summary\":string,\"bullets\":[string],\"confidence\":number 0..1}. Rules: summary is one decisive sentence; produce 3-5 bullets, each a short actionable observation citing concrete numbers from the notes; confidence reflects how decisive the underlying signals are (0.4 for thin/conflicting, 0.75 for strong, 0.9 for unambiguous). Reply ONLY with the JSON object — no markdown, no preamble."
	if zh {
		systemPrompt = "你是一名资深买方研究员。用户会给你一份研究员当日的原始笔记，请精炼为 JSON：{\"summary\":字符串,\"bullets\":[字符串数组],\"confidence\":0..1数字}。规则：summary 一句话给出关键判断；bullets 输出 3-5 条，每条含原文里的具体数字或事件；confidence 是你对信号确定性的评分（0.4 信号薄弱/冲突，0.75 较强，0.9 明确）。只输出 JSON，不要 markdown 包裹，不要任何前言。"
	}
	header := fmt.Sprintf("【%s 研究综述】", stepKind)
	if !zh {
		header = fmt.Sprintf("[%s research synthesis]", stepKind)
	}

	userID := ""
	if agent != nil {
		userID = strings.TrimSpace(agent.UserID)
	}
	// Cap the input we send to the LLM: provider text can run
	// several KB on a 12-symbol universe with full fundamentals.
	// 16 KB is comfortably under any TierStandard input budget while
	// preserving the per-symbol blocks intact.
	maxInputBytes := 16 * 1024
	bounded := cleaned
	if len(bounded) > maxInputBytes {
		bounded = bounded[:maxInputBytes] + "\n...(input truncated for prompt budget)"
	}

	stepName := "research_synthesis_" + strings.ToLower(strings.TrimSpace(stepKind))
	llmCtx, cancel := llmEnrichmentContext()
	defer cancel()
	resp, err := p.llmRuntime.Chat(llmCtx, llm.ChatRequest{
		UserID:    userID,
		FundID:    fundID,
		ModelTier: llm.TierStandard,
		StepName:  stepName,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: bounded},
		},
	})
	if err != nil || resp == nil {
		if err != nil {
			slog.Debug("research llm synthesis failed", "step", stepKind, "fund", fundID, "err", err)
		}
		return providerText
	}
	body := strings.TrimSpace(resp.Content)
	if body == "" {
		return providerText
	}
	// Strip optional markdown code fences (`{...}` or ```json {...}```).
	body = stripCodeFences(body)
	var parsed researchLLMSynthesis
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		// One more attempt: extract the first {...} block — the
		// LLM occasionally wraps the JSON with prose despite the
		// instruction.
		if start, end := strings.Index(body, "{"), strings.LastIndex(body, "}"); start >= 0 && end > start {
			if jsonErr := json.Unmarshal([]byte(body[start:end+1]), &parsed); jsonErr != nil {
				slog.Debug("research llm synthesis parse failed", "step", stepKind, "err", err, "snippet", trimForLog(body, 200))
				return providerText
			}
		} else {
			return providerText
		}
	}
	parsed.Summary = strings.TrimSpace(parsed.Summary)
	cleanedBullets := make([]string, 0, len(parsed.Bullets))
	for _, b := range parsed.Bullets {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		cleanedBullets = append(cleanedBullets, "- "+b)
	}
	if parsed.Summary == "" && len(cleanedBullets) == 0 {
		return providerText
	}
	if parsed.Confidence < 0 {
		parsed.Confidence = 0
	}
	if parsed.Confidence > 1 {
		parsed.Confidence = 1
	}
	confLabel := "confidence"
	if zh {
		confLabel = "置信度"
	}
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	if parsed.Summary != "" {
		b.WriteString(parsed.Summary)
		b.WriteString("\n")
	}
	if len(cleanedBullets) > 0 {
		b.WriteString(strings.Join(cleanedBullets, "\n"))
		b.WriteString("\n")
	}
	b.WriteString(fmt.Sprintf("(%s: %.2f)\n\n", confLabel, parsed.Confidence))
	b.WriteString(providerText)
	return b.String()
}

// stripCodeFences removes ``` fences and an optional language tag.
func stripCodeFences(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	t = strings.TrimPrefix(t, "```")
	if idx := strings.Index(t, "\n"); idx >= 0 {
		t = t[idx+1:]
	}
	t = strings.TrimSuffix(t, "```")
	return strings.TrimSpace(t)
}

// trimForLog truncates a string to a max-length safe for slog Debug.
func trimForLog(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func (p runtimeResearcherPool) MacroBrief(ctx context.Context, fundID string, tradingDate string) (workflow.ResearchReport, error) {
	agent := p.findResearcherAgent(ctx, fundID, workflow.FocusMacro)
	fundFocusContext, specializationContext := buildRuntimeFundContextsByID(ctx, fundID, agent, p.fundRepo)
	content := p.buildMacroResearchContent(ctx, fundID, tradingDate)
	// Sprint 1 / S3: run the provider-stitched macro text through a
	// TierStandard LLM to produce a structured synthesis; prepend it
	// to the raw content so downstream consumers (debate, persistence)
	// still see the underlying signals. Failures degrade gracefully.
	content = p.summariseResearchWithLLM(ctx, agent, fundID, "macro", content)
	content = appendSkillContext(content, fundFocusContext)
	content = appendSkillContext(content, specializationContext)
	content = appendSkillContext(content, buildResearcherSkillContext(LanguageFromContext(ctx), agent, workflow.StepMacroBrief.String(), nil))
	return workflow.ResearchReport{AgentID: researcherAgentID(agent, "macro"), Focus: workflow.FocusMacro, Content: content}, nil
}

func (p runtimeResearcherPool) RunAll(ctx context.Context, fundID string, tradingDate string) ([]workflow.ResearchReport, error) {
	stockAgent := p.findResearcherAgent(ctx, fundID, workflow.FocusStock)
	fundamentalAgent := p.findResearcherAgent(ctx, fundID, workflow.FocusFundamental)
	stockFundFocusContext, stockSpecializationContext := buildRuntimeFundContextsByID(ctx, fundID, stockAgent, p.fundRepo)
	fundamentalFundFocusContext, fundamentalSpecializationContext := buildRuntimeFundContextsByID(ctx, fundID, fundamentalAgent, p.fundRepo)
	stockContent := p.buildUniverseResearchContent(ctx, fundID, workflow.FocusStock, tradingDate)
	stockContent = p.summariseResearchWithLLM(ctx, stockAgent, fundID, "stock", stockContent)
	stockContent = appendSkillContext(stockContent, stockFundFocusContext)
	stockContent = appendSkillContext(stockContent, stockSpecializationContext)
	stockContent = appendSkillContext(stockContent, buildResearcherSkillContext(LanguageFromContext(ctx), stockAgent, workflow.StepResearchParallel.String(), nil))
	fundamentalContent := p.buildUniverseResearchContent(ctx, fundID, workflow.FocusFundamental, tradingDate)
	fundamentalContent = p.summariseResearchWithLLM(ctx, fundamentalAgent, fundID, "fundamental", fundamentalContent)
	fundamentalContent = appendSkillContext(fundamentalContent, fundamentalFundFocusContext)
	fundamentalContent = appendSkillContext(fundamentalContent, fundamentalSpecializationContext)
	fundamentalContent = appendSkillContext(fundamentalContent, buildResearcherSkillContext(LanguageFromContext(ctx), fundamentalAgent, workflow.StepResearchParallel.String(), nil))
	return []workflow.ResearchReport{
		{AgentID: researcherAgentID(stockAgent, "stock"), Focus: workflow.FocusStock, Content: stockContent},
		{AgentID: researcherAgentID(fundamentalAgent, "fundamental"), Focus: workflow.FocusFundamental, Content: fundamentalContent},
	}, nil
}

func (p runtimeResearcherPool) QuantSignals(ctx context.Context, fundID string, tradingDate string) ([]workflow.ResearchReport, error) {
	agent := p.findResearcherAgent(ctx, fundID, workflow.FocusStock)
	fundFocusContext, specializationContext := buildRuntimeFundContextsByID(ctx, fundID, agent, p.fundRepo)
	content := p.buildQuantResearchContent(ctx, fundID, tradingDate)
	content = p.summariseResearchWithLLM(ctx, agent, fundID, "quant", content)
	content = appendSkillContext(content, fundFocusContext)
	content = appendSkillContext(content, specializationContext)
	content = appendSkillContext(content, buildResearcherSkillContext(LanguageFromContext(ctx), agent, workflow.StepQuantSignals.String(), nil))
	return []workflow.ResearchReport{{AgentID: researcherAgentID(agent, "quant"), Focus: workflow.FocusStock, Content: content}}, nil
}

func (p runtimeResearcherPool) Roundtable(ctx context.Context, fundID string, reports []workflow.ResearchReport, maxRounds int) (*workflow.RoundtableResult, error) {
	if p.shouldRunDebate(ctx, fundID) {
		if result, err := p.runDebateRoundtable(ctx, fundID, reports, maxRounds); err == nil && result != nil {
			return result, nil
		} else if err != nil {
			// Debate is best-effort: log and fall back to the legacy
			// text-concat consensus so the workflow keeps moving
			// even if the LLM tier is misbehaving.
			slog.Warn("debate roundtable failed; falling back to legacy consensus", "fundId", fundID, "err", err)
		}
	}
	consensus := make([]string, 0, len(reports))
	for _, report := range reports {
		consensus = append(consensus, report.Content)
	}
	return &workflow.RoundtableResult{
		ID:        uuid.NewString(),
		Rounds:    1,
		Consensus: consensus,
	}, nil
}

// shouldRunDebate gates the Phase 2B multi-agent roundtable on three
// signals (any one is sufficient):
//
//  1. debateForceEnabled — operator flipped the env flag fleet-wide.
//  2. fund.config.researchTier == "advanced" — the fund opted in.
//
// The function is conservative: any error reading the fund config
// is treated as "stay on legacy" so a flaky DB cannot accidentally
// enable the more expensive path.
func (p runtimeResearcherPool) shouldRunDebate(ctx context.Context, fundID string) bool {
	if p.debateRoundtable == nil {
		return false
	}
	if p.debateForceEnabled {
		return true
	}
	if p.fundRepo == nil {
		return false
	}
	fund, err := p.fundRepo.GetByID(ctx, fundID)
	if err != nil || fund == nil {
		return false
	}
	profile := decodeFundMarketProfile(fund.Config)
	return strings.EqualFold(strings.TrimSpace(profile.ResearchTier), "advanced")
}

// runDebateRoundtable projects the existing ResearchReport bag into
// a debate.DebateInput, runs the bull/bear/quant orchestrator, and
// converts the structured output back to workflow.RoundtableResult.
// The Consensus slice is preserved for unchanged downstream
// consumers (legacy PMAgent prompt building) while the richer
// fields carry the per-symbol verdicts for Phase 2A's decision
// engine.
func (p runtimeResearcherPool) runDebateRoundtable(ctx context.Context, fundID string, reports []workflow.ResearchReport, maxRounds int) (*workflow.RoundtableResult, error) {
	fund, err := p.fundRepo.GetByID(ctx, fundID)
	if err != nil {
		return nil, err
	}
	profile := decodeFundMarketProfile(fund.Config)

	input := debate.DebateInput{
		FundID:               fundID,
		TradingDate:          time.Now().UTC(),
		Market:               profile.Market,
		Universe:             profileUniverseSymbols(profile),
		MaxRounds:            maxRounds,
		ConvergenceThreshold: debate.DefaultConvergenceThreshold,
	}
	for _, report := range reports {
		switch report.Focus {
		case workflow.FocusMacro:
			if input.MacroBrief == "" {
				input.MacroBrief = report.Content
			} else {
				input.MacroBrief += "\n\n" + report.Content
			}
		case workflow.FocusStock:
			input.StockReports = append(input.StockReports, report.Content)
		case workflow.FocusFundamental:
			input.FundamentalReports = append(input.FundamentalReports, report.Content)
		default:
			// Quant signals come in as FocusStock content when the
			// existing buildQuantResearchContent path produced them.
			// Future PRs (Phase 2C) will add a dedicated focus.
			input.QuantSignals = append(input.QuantSignals, report.Content)
		}
	}
	// Phase 2C: enrich QuantSignals with per-symbol indicator
	// snapshots so the Quant role sees real RSI/MACD/KDJ numbers
	// instead of just the qualitative narratives above. Best-effort:
	// the universe may be large, missing providers cause individual
	// symbols to be skipped, and per-symbol failures never stop the
	// debate. We cap at 20 symbols to bound LLM token usage; the
	// debate prompt will still grade every universe symbol because
	// the LLMResearcher fans out per the input.Universe field.
	if p.ohlcFetcher != nil && len(input.Universe) > 0 {
		input.QuantSignals = append(input.QuantSignals, p.collectIndicatorBlock(ctx, profile, input.Universe)...)
	}
	// Phase 2D: enrich FundamentalReports with per-symbol valuation
	// + growth + margins. Bull / Bear roles use this to argue
	// "stretched valuation" vs "earnings re-acceleration" with
	// concrete numbers. Same best-effort + 20-symbol cap as the
	// indicator block.
	var fundamentalSummary string
	if p.fundamentalFetcher != nil && len(input.Universe) > 0 {
		fundamentalLines := p.collectFundamentalBlock(ctx, profile, input.Universe)
		input.FundamentalReports = append(input.FundamentalReports, fundamentalLines...)
		fundamentalSummary = strings.Join(fundamentalLines, "\n")
	}
	// Phase 2D: append the cross-sector rotation snapshot to the
	// MacroBrief so all three agents start with the same view of
	// which industries the tape is bidding up vs bleeding.
	rotation := p.sectorRotationDebateBlock(ctx, profile)
	if rotation != "" {
		if input.MacroBrief == "" {
			input.MacroBrief = rotation
		} else {
			input.MacroBrief += "\n\n" + rotation
		}
	}
	// Phase 2D: append per-symbol news sentiment to QuantSignals so
	// the Quant role can weigh "noise" vs "real catalyst" and the
	// Bull/Bear see exactly which symbols are getting flow.
	sentBlock := p.collectSentimentDebateBlock(ctx, fund, profile)
	if sentBlock != "" {
		input.QuantSignals = append(input.QuantSignals, sentBlock)
	}

	output, err := p.debateRoundtable.Run(ctx, input)
	if err != nil {
		return nil, err
	}

	consensus := buildConsensusFromDebate(output, reports)
	result := &workflow.RoundtableResult{
		ID:                 uuid.NewString(),
		Rounds:             output.Rounds,
		Consensus:          consensus,
		OverallStance:      output.OverallStance,
		BullCase:           output.BullCase,
		BearCase:           output.BearCase,
		QuantCase:          output.QuantCase,
		FundamentalSummary: fundamentalSummary,
		SectorRotation:     rotation,
		NewsSentiment:      sentBlock,
	}
	if len(output.Symbols) > 0 {
		result.Symbols = make([]workflow.RoundtableSymbolVerdict, 0, len(output.Symbols))
		for _, sd := range output.Symbols {
			result.Symbols = append(result.Symbols, workflow.RoundtableSymbolVerdict{
				Symbol:       sd.Symbol,
				Verdict:      sd.Verdict,
				BullCase:     sd.BullCase,
				BearCase:     sd.BearCase,
				QuantCase:    sd.QuantCase,
				DissentVotes: sd.DissentVotes,
			})
		}
	}
	return result, nil
}

// buildConsensusFromDebate produces a Consensus slice that any
// legacy code path (older PMAgent text-blender) can still consume.
// The debate roundtable's per-role one-liners go first, then the
// existing research reports — this preserves backwards-compatible
// text the LLM decision engine can still summarize when it falls
// back to the deterministic path.
func buildConsensusFromDebate(output *debate.RoundtableOutput, reports []workflow.ResearchReport) []string {
	consensus := make([]string, 0, len(reports)+4)
	if s := strings.TrimSpace(output.BullCase); s != "" {
		consensus = append(consensus, "BULL: "+s)
	}
	if s := strings.TrimSpace(output.BearCase); s != "" {
		consensus = append(consensus, "BEAR: "+s)
	}
	if s := strings.TrimSpace(output.QuantCase); s != "" {
		consensus = append(consensus, "QUANT: "+s)
	}
	for _, sd := range output.Symbols {
		if sd.Verdict == "" {
			continue
		}
		consensus = append(consensus, fmt.Sprintf("%s → %s (dissent=%d)", sd.Symbol, sd.Verdict, sd.DissentVotes))
	}
	for _, report := range reports {
		consensus = append(consensus, report.Content)
	}
	return consensus
}

// profileUniverseSymbols returns the operator-configured candidate
// symbols, trimmed to non-empty entries. Returns nil when no
// universe is configured — the debate orchestrator treats nil
// universe as "agents may name their own symbols".
func profileUniverseSymbols(profile fundMarketProfile) []string {
	if profile.Universe == nil {
		return nil
	}
	out := make([]string, 0, len(profile.Universe.Symbols))
	for _, sym := range profile.Universe.Symbols {
		if trimmed := strings.TrimSpace(sym); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// collectIndicatorBlock fetches OHLC for each universe symbol and
// returns a list of pre-formatted indicator lines suitable for
// dropping into debate.DebateInput.QuantSignals. Symbols missing
// from the provider chain or producing too-short bars are silently
// skipped — the debate already handles the "no quant data" path
// gracefully.
//
// The fan-out is sequential (capped to 20 symbols) for two reasons:
//   1. The Cache layer above the providers already de-dupes
//      repeated fetches for the same symbol within the TTL window,
//      so back-to-back debate rounds cost one upstream call each.
//   2. Parallelizing would race against external provider rate
//      limits (Yahoo in particular). Sequential keeps the worst-
//      case cost predictable; 20 symbols at ~150ms each is ~3s,
//      which fits comfortably within the workflow tick budget.
func (p runtimeResearcherPool) collectIndicatorBlock(ctx context.Context, profile fundMarketProfile, symbols []string) []string {
	if p.ohlcFetcher == nil || len(symbols) == 0 {
		return nil
	}
	const maxSymbols = 20
	limit := len(symbols)
	if limit > maxSymbols {
		limit = maxSymbols
	}
	out := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		sym := strings.TrimSpace(symbols[i])
		if sym == "" {
			continue
		}
		instrument := marketdata.InstrumentRef{
			Symbol:     sym,
			Market:     profile.Market,
			Exchange:   profile.Exchange,
			AssetClass: profile.AssetClass,
		}
		snap, ok := p.indicatorSnapshot(ctx, instrument)
		if !ok {
			continue
		}
		line := snap.FormatForPrompt(sym)
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// collectFundamentalBlock fetches fundamentals for each universe
// symbol and returns pre-formatted "AAPL: PE 28.3..." lines for
// the debate's FundamentalReports input. Mirrors
// collectIndicatorBlock's contract: best-effort, capped at 20
// symbols, sequential to play nice with provider rate limits.
//
// Symbols that the fundamental Registry can't satisfy are
// silently dropped so the debate isn't bloated with empty rows.
// The Cache layer above the registry dedupes across the
// per-call fan-out.
func (p runtimeResearcherPool) collectFundamentalBlock(ctx context.Context, profile fundMarketProfile, symbols []string) []string {
	if p.fundamentalFetcher == nil || len(symbols) == 0 {
		return nil
	}
	const maxSymbols = 20
	limit := len(symbols)
	if limit > maxSymbols {
		limit = maxSymbols
	}
	out := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		sym := strings.TrimSpace(symbols[i])
		if sym == "" {
			continue
		}
		instrument := marketdata.InstrumentRef{
			Symbol:     sym,
			Market:     profile.Market,
			Exchange:   profile.Exchange,
			AssetClass: profile.AssetClass,
		}
		metrics, ok := p.fundamentalMetrics(ctx, instrument)
		if !ok || metrics == nil {
			continue
		}
		line := metrics.FormatForPrompt()
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// sectorRotationDebateBlock formats sector rotation for the debate
// MacroBrief slot. Same provider call as the user-facing macro
// brief, but always uses English headers because the debate runs
// in English (the agent prompts are English-only).
func (p runtimeResearcherPool) sectorRotationDebateBlock(ctx context.Context, profile fundMarketProfile) string {
	if p.sectorFlowFetcher == nil {
		return ""
	}
	market := strings.TrimSpace(strings.ToLower(profile.Market))
	if market == "" {
		return ""
	}
	snap, err := p.sectorFlowFetcher.Fetch(ctx, sectorflow.FetchRequest{Market: market})
	if err != nil {
		if !errors.Is(err, sectorflow.ErrNoData) && !errors.Is(err, sectorflow.ErrNoProvider) {
			slog.Debug("debate sectorflow fetch failed", "market", market, "err", err)
		}
		return ""
	}
	body := snap.FormatForPrompt(3, 3)
	if body == "" {
		return ""
	}
	return "Sector rotation\n" + body
}

// collectSentimentDebateBlock fetches recent themed news, scores
// it, and returns a single concatenated block suitable for the
// debate's QuantSignals slot. Falls back gracefully when the
// scorer is unwired or returns an error.
func (p runtimeResearcherPool) collectSentimentDebateBlock(ctx context.Context, fund *repository.Fund, profile fundMarketProfile) string {
	if p.sentimentScorer == nil || p.marketData == nil || !p.marketData.Enabled() || fund == nil {
		return ""
	}
	queries := buildHybridMarketNewsQueries(fund, profile, nil, nil)
	if len(queries) == 0 {
		return ""
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	seen := make(map[string]struct{}, 16)
	collected := make([]marketdata.NewsItem, 0, 12)
	for _, q := range queries {
		if len(collected) >= 12 {
			break
		}
		items, _, err := p.marketData.GetNewsWithNotes(ctx, q, 6)
		if err != nil {
			continue
		}
		for _, item := range items {
			if item.PublishedAt.IsZero() || item.PublishedAt.Before(cutoff) {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(item.URL))
			if key == "" {
				key = strings.ToLower(strings.TrimSpace(item.Title))
			}
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			collected = append(collected, item)
			if len(collected) >= 12 {
				break
			}
		}
	}
	items := newsItemsToSentiment(collected)
	// Sprint 9.3: enrich with retail social posts (Xueqiu /
	// StockTwits / Reddit-WSB) before scoring. The scorer sees the
	// extra rows transparently and the per-symbol aggregator picks
	// up the broader sample. Capped per symbol to keep the prompt
	// budget bounded.
	items = append(items, p.collectSocialItems(ctx, queries, 5)...)
	if len(items) == 0 {
		return ""
	}
	scores, err := p.sentimentScorer.Score(ctx, items)
	if err != nil {
		slog.Debug("debate sentiment scorer failed", "err", err, "items", len(items))
		return ""
	}
	aggregates := sentiment.AggregateBySymbol(scores, items)
	body := sentiment.FormatForPrompt(aggregates, len(items))
	if body == "" {
		return ""
	}
	return body
}

func (p runtimeResearcherPool) findResearcherAgent(ctx context.Context, fundID string, focus workflow.ResearchFocus) *repository.Agent {
	return findFundAgentByRoleWithFocus(ctx, fundID, string(workflow.RoleResearcher), string(focus), p.teamRepo, p.agentRepo, p.fundRepo)
}

func (p runtimeResearcherPool) buildMacroResearchContent(ctx context.Context, fundID string, tradingDate string) string {
	lang := LanguageFromContext(ctx)
	zh := lang != UserLanguageEN
	if p.marketData == nil || !p.marketData.Enabled() {
		if zh {
			return "宏观简报暂不可用：行情数据源未启用"
		}
		return "Macro brief unavailable: market data source disabled"
	}
	fund, err := p.fundRepo.GetByID(ctx, fundID)
	if err != nil || fund == nil {
		if zh {
			return "宏观简报暂不可用：基金行情画像加载失败"
		}
		return "Macro brief unavailable: failed to load fund market profile"
	}
	profile := decodeFundMarketProfile(fund.Config)
	header := "宏观简报"
	benchmarkLabel := "基准"
	noBenchmark := "宏观简报暂不可用：基准研究无法刷新"
	themedHeader := "主题资讯"
	if !zh {
		header = "Macro brief"
		benchmarkLabel = "Benchmark"
		noBenchmark = "Macro brief unavailable: benchmark research could not refresh"
		themedHeader = "Themed news"
	}
	lines := []string{header}
	if instrument, ok := benchmarkInstrumentRef(profile); ok {
		if research, err := p.marketResearch(ctx, instrument, nil); err == nil {
			lines = append(lines, formatResearchContextBlock(lang, benchmarkLabel, research))
		}
	}
	if themed := p.collectMacroThemedNews(ctx, fund, profile, lang); themed != "" {
		lines = append(lines, themedHeader+"\n"+themed)
	}
	// Phase 2D: append sector rotation when a sector-flow fetcher is
	// configured. Best-effort: the section is silently omitted on
	// provider error or empty market support.
	if rotation := p.sectorRotationBlock(ctx, profile, lang); rotation != "" {
		lines = append(lines, rotation)
	}
	// Phase 2D: append aggregated news sentiment so the macro brief
	// surfaces the directional vibe of the news flow before the
	// debate reads individual headlines.
	if sentBlock := p.macroSentimentBlock(ctx, fund, profile, lang); sentBlock != "" {
		lines = append(lines, sentBlock)
	}
	if len(lines) == 1 {
		lines[0] = noBenchmark
	}
	if note := marketDataAsOfNote(lang, tradingDate); note != "" {
		lines = append(lines, note)
	}
	return strings.Join(lines, "\n\n")
}

// sectorRotationBlock formats the sector-flow snapshot for the
// macro brief. Returns "" when no fetcher is wired, the upstream
// produced no data, or the market doesn't have a provider in the
// chain. Localised heading; the sector names themselves stay in
// the provider's source language because translating them risks
// losing ticker fidelity (e.g., "Consumer Discretionary").
func (p runtimeResearcherPool) sectorRotationBlock(ctx context.Context, profile fundMarketProfile, lang UserLanguage) string {
	if p.sectorFlowFetcher == nil {
		return ""
	}
	market := strings.TrimSpace(strings.ToLower(profile.Market))
	if market == "" {
		return ""
	}
	snap, err := p.sectorFlowFetcher.Fetch(ctx, sectorflow.FetchRequest{Market: market})
	if err != nil {
		if !errors.Is(err, sectorflow.ErrNoData) && !errors.Is(err, sectorflow.ErrNoProvider) {
			slog.Debug("sectorflow fetch failed", "market", market, "err", err)
		}
		return ""
	}
	body := snap.FormatForPrompt(3, 3)
	if body == "" {
		return ""
	}
	header := "板块轮动"
	if lang == UserLanguageEN {
		header = "Sector rotation"
	}
	return header + "\n" + body
}

// macroSentimentBlock collects the themed news already used by the
// macro brief, scores each item, and aggregates into a single
// "market-level mood + per-symbol mood" block. Pure additive on
// top of the existing themed-news bullets; if the sentiment
// scorer fails the macro brief still renders normally.
func (p runtimeResearcherPool) macroSentimentBlock(ctx context.Context, fund *repository.Fund, profile fundMarketProfile, lang UserLanguage) string {
	if p.sentimentScorer == nil || p.marketData == nil || !p.marketData.Enabled() || fund == nil {
		return ""
	}
	queries := buildHybridMarketNewsQueries(fund, profile, nil, nil)
	if len(queries) == 0 {
		return ""
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	seen := make(map[string]struct{}, 16)
	collected := make([]marketdata.NewsItem, 0, 12)
	for _, q := range queries {
		if len(collected) >= 12 {
			break
		}
		items, _, err := p.marketData.GetNewsWithNotes(ctx, q, 6)
		if err != nil {
			continue
		}
		for _, item := range items {
			if item.PublishedAt.IsZero() || item.PublishedAt.Before(cutoff) {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(item.URL))
			if key == "" {
				key = strings.ToLower(strings.TrimSpace(item.Title))
			}
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			collected = append(collected, item)
			if len(collected) >= 12 {
				break
			}
		}
	}
	items := newsItemsToSentiment(collected)
	// Sprint 9.3: enrich the macro brief with retail social posts.
	// Same dispatching pattern as the debate block — best-effort,
	// nil-safe, dedup'd by item.ID. We keep the per-symbol cap a
	// little higher (8 vs 5) because the macro brief is the
	// "wide-angle" read and benefits from broader retail mood
	// coverage; the debate block above prefers tight focus.
	items = append(items, p.collectSocialItems(ctx, queries, 8)...)
	if len(items) == 0 {
		return ""
	}
	scores, err := p.sentimentScorer.Score(ctx, items)
	if err != nil {
		slog.Debug("sentiment scorer failed", "err", err, "items", len(items))
		return ""
	}
	aggregates := sentiment.AggregateBySymbol(scores, items)
	body := sentiment.FormatForPrompt(aggregates, len(items))
	if body == "" {
		return ""
	}
	header := "新闻情绪"
	if lang == UserLanguageEN {
		header = "News sentiment"
	}
	return header + "\n" + body
}

// collectSocialItems pulls retail social posts for the supplied
// instrument refs via the configured social.Registry (Sprint 9.3).
//
// Best-effort: returns an empty slice when the registry isn't
// wired or every per-symbol fetch errors out. The caller appends
// the returned items to its news-derived []sentiment.Item before
// invoking the scorer, so social posts and news are scored and
// aggregated identically downstream — the only externally visible
// difference is the Item.Source value (`xueqiu` / `stocktwits` /
// `reddit_wsb` vs the news outlet name).
//
// We cap perSymbolLimit because the daily macro brief already pays
// for N news items per symbol; doubling that with social would
// blow the LLM scorer's prompt budget and dilute the news signal.
func (p runtimeResearcherPool) collectSocialItems(ctx context.Context, instruments []marketdata.InstrumentRef, perSymbolLimit int) []sentiment.Item {
	if p.socialRegistry == nil || !p.socialRegistry.HasProviders() || len(instruments) == 0 {
		return nil
	}
	if perSymbolLimit <= 0 {
		perSymbolLimit = 10
	}
	seen := make(map[string]struct{}, len(instruments)*perSymbolLimit)
	out := make([]sentiment.Item, 0, len(instruments)*perSymbolLimit)
	for _, ref := range instruments {
		sym := strings.TrimSpace(ref.Symbol)
		if sym == "" || !marketdata.IsTickerLikeSymbol(sym) {
			continue
		}
		posts, err := p.socialRegistry.FetchPosts(ctx, social.Request{
			Symbol: sym,
			Market: ref.Market,
			Limit:  perSymbolLimit,
		})
		if err != nil {
			slog.Debug("social registry fetch failed",
				"symbol", sym, "market", ref.Market, "err", err)
			continue
		}
		for _, item := range posts {
			if item.ID == "" {
				continue
			}
			if _, ok := seen[item.ID]; ok {
				continue
			}
			seen[item.ID] = struct{}{}
			out = append(out, item)
		}
	}
	return out
}

// newsItemsToSentiment is the small adapter between the marketdata
// NewsItem shape and the sentiment.Item shape. We localise the
// fields the scorer actually reads (Title / Summary / Symbols /
// PublishedAt) and synthesize a stable ID from URL → title hash
// so deduplication and ID-keyed matching survive across calls.
func newsItemsToSentiment(items []marketdata.NewsItem) []sentiment.Item {
	out := make([]sentiment.Item, 0, len(items))
	for i, item := range items {
		id := strings.TrimSpace(item.URL)
		if id == "" {
			id = strings.TrimSpace(item.Title)
		}
		if id == "" {
			id = fmt.Sprintf("news-%d", i)
		}
		out = append(out, sentiment.Item{
			ID:          id,
			Title:       firstNonEmpty(item.Title, item.TitleEn, item.TitleZh),
			Summary:     firstNonEmpty(item.Summary, item.SummaryEn, item.SummaryZh),
			Source:      item.Source,
			URL:         item.URL,
			Language:    item.Language,
			PublishedAt: item.PublishedAt,
			Symbols:     item.Symbols,
		})
	}
	return out
}

// collectMacroThemedNews fetches themed/sector news using the fund's hybrid
// query set so the macro brief reflects the fund's actual focus instead of just
// the benchmark ticker. Returns a multi-line string of "- title (source, date)".
func (p runtimeResearcherPool) collectMacroThemedNews(ctx context.Context, fund *repository.Fund, profile fundMarketProfile, lang UserLanguage) string {
	if p.marketData == nil || !p.marketData.Enabled() || fund == nil {
		return ""
	}
	queries := buildHybridMarketNewsQueries(fund, profile, nil, nil)
	if len(queries) == 0 {
		return ""
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	seenURL := make(map[string]struct{}, 8)
	seenTitle := make(map[string]struct{}, 8)
	collected := make([]marketdata.NewsItem, 0, 6)
	for _, q := range queries {
		if len(collected) >= 6 {
			break
		}
		items, _, err := p.marketData.GetNewsWithNotes(ctx, q, 3)
		if err != nil {
			continue
		}
		for _, item := range items {
			if item.PublishedAt.IsZero() || item.PublishedAt.Before(cutoff) {
				continue
			}
			urlKey := strings.ToLower(strings.TrimSpace(item.URL))
			titleKey := strings.ToLower(strings.TrimSpace(item.Title))
			if urlKey != "" {
				if _, ok := seenURL[urlKey]; ok {
					continue
				}
				seenURL[urlKey] = struct{}{}
			}
			if titleKey != "" {
				if _, ok := seenTitle[titleKey]; ok {
					continue
				}
				seenTitle[titleKey] = struct{}{}
			}
			collected = append(collected, item)
			if len(collected) >= 6 {
				break
			}
		}
	}
	if len(collected) == 0 {
		return ""
	}
	sort.SliceStable(collected, func(i, j int) bool {
		return collected[i].PublishedAt.After(collected[j].PublishedAt)
	})
	dateLabel := "日期"
	sourceLabel := "来源"
	if lang == UserLanguageEN {
		dateLabel = "date"
		sourceLabel = "source"
	}
	var b strings.Builder
	for _, item := range collected {
		title := strings.TrimSpace(item.Title)
		if title == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(title)
		meta := make([]string, 0, 2)
		if src := strings.TrimSpace(item.Source); src != "" {
			meta = append(meta, sourceLabel+": "+src)
		}
		if !item.PublishedAt.IsZero() {
			meta = append(meta, dateLabel+": "+item.PublishedAt.UTC().Format("2006-01-02"))
		}
		if len(meta) > 0 {
			b.WriteString(" (")
			b.WriteString(strings.Join(meta, ", "))
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (p runtimeResearcherPool) buildUniverseResearchContent(ctx context.Context, fundID string, focus workflow.ResearchFocus, tradingDate string) string {
	lang := LanguageFromContext(ctx)
	zh := lang != UserLanguageEN
	focusLabel := researchFocusLabel(lang, focus)
	if p.marketData == nil || !p.marketData.Enabled() {
		if zh {
			return fmt.Sprintf("%s暂不可用：行情数据源未启用", focusLabel)
		}
		return fmt.Sprintf("%s unavailable: market data source disabled", focusLabel)
	}
	fund, err := p.fundRepo.GetByID(ctx, fundID)
	if err != nil || fund == nil {
		if zh {
			return fmt.Sprintf("%s暂不可用：基金行情画像加载失败", focusLabel)
		}
		return fmt.Sprintf("%s unavailable: failed to load fund market profile", focusLabel)
	}
	profile := decodeFundMarketProfile(fund.Config)
	benchmark, _ := benchmarkInstrumentRef(profile)
	instruments := profileUniverseInstruments(profile)
	if len(instruments) == 0 {
		instruments = append(instruments, defaultInstrumentRef(fund, focus, inferWorkflowSymbol(fund, nil)))
	}
	sections := []string{focusLabel}
	// Sprint 1 / S3: drop the hardcoded i >= 3 cap. A 12-symbol
	// universe was previously losing 75% of its coverage. Replace
	// with a soft safety bound (16) so a misconfigured universe
	// can't blow the LLM token budget; production funds stay well
	// inside the bound.
	for i, instrument := range instruments {
		if i >= researcherMaxInstrumentsPerStep {
			break
		}
		research, err := p.marketResearch(ctx, instrument, benchmarkPointer(benchmark))
		if err != nil {
			if zh {
				sections = append(sections, fmt.Sprintf("%s：研究数据暂不可用（%v）", instrument.Symbol, err))
			} else {
				sections = append(sections, fmt.Sprintf("%s: research data unavailable (%v)", instrument.Symbol, err))
			}
			continue
		}
		sections = append(sections, formatResearchContextBlock(lang, instrument.NormalizedSymbol(), research))
		p.persistResearchMemory(ctx, fundID, researcherAgentID(p.findResearcherAgent(ctx, fundID, focus), strings.ToLower(string(focus))), tradingDate, research)
	}
	return strings.Join(sections, "\n\n")
}

func (p runtimeResearcherPool) buildQuantResearchContent(ctx context.Context, fundID string, tradingDate string) string {
	lang := LanguageFromContext(ctx)
	zh := lang != UserLanguageEN
	if p.marketData == nil || !p.marketData.Enabled() {
		if zh {
			return "量化信号暂不可用：行情数据源未启用"
		}
		return "Quant signals unavailable: market data source disabled"
	}
	fund, err := p.fundRepo.GetByID(ctx, fundID)
	if err != nil || fund == nil {
		if zh {
			return "量化信号暂不可用：基金行情画像加载失败"
		}
		return "Quant signals unavailable: failed to load fund market profile"
	}
	profile := decodeFundMarketProfile(fund.Config)
	benchmark, _ := benchmarkInstrumentRef(profile)
	instruments := profileUniverseInstruments(profile)
	header := "量化信号"
	noUniverse := "量化信号暂不可用"
	noSignal := "暂无信号"
	colon := "："
	signalSep := "；"
	if !zh {
		header = "Quant signals"
		noUniverse = "Quant signals unavailable"
		noSignal = "no signal"
		colon = ": "
		signalSep = "; "
	}
	if len(instruments) == 0 {
		return noUniverse
	}
	lines := []string{header}
	// Sprint 1 / S3: same cap-removal as buildUniverseResearchContent.
	for i, instrument := range instruments {
		if i >= researcherMaxInstrumentsPerStep {
			break
		}
		research, err := p.marketResearch(ctx, instrument, benchmarkPointer(benchmark))
		if err != nil {
			if zh {
				lines = append(lines, fmt.Sprintf("- %s：暂不可用（%v）", instrument.NormalizedSymbol(), err))
			} else {
				lines = append(lines, fmt.Sprintf("- %s: unavailable (%v)", instrument.NormalizedSymbol(), err))
			}
			continue
		}
		base := fmt.Sprintf("- %s%s", instrument.NormalizedSymbol(), colon)
		if len(research.Signals) == 0 {
			base += noSignal
		} else {
			base += strings.Join(research.Signals, signalSep)
		}
		// Phase 2C: append indicator snapshot when an OHLC fetcher is
		// configured. The snapshot is "best effort" — any error here
		// (no provider, network blip) is silently swallowed so the
		// qualitative signals above still surface. The indicator
		// block uses the canonical FormatForPrompt output so the
		// debate's Quant role and this pool's text consumers see
		// identical numbers.
		if snapshot, ok := p.indicatorSnapshot(ctx, instrument); ok {
			line := snapshot.FormatForPrompt(instrument.NormalizedSymbol())
			if line != "" {
				base += " | " + line
			}
		}
		// Phase 2D: append fundamentals when a fundamental fetcher is
		// configured. Same best-effort contract as indicators.
		if metrics, ok := p.fundamentalMetrics(ctx, instrument); ok {
			line := metrics.FormatForPrompt()
			if line != "" {
				base += " | " + line
			}
		}
		lines = append(lines, base)
	}
	if note := marketDataAsOfNote(lang, tradingDate); note != "" {
		lines = append(lines, note)
	}
	return strings.Join(lines, "\n")
}

// indicatorSnapshot fetches OHLC bars for the given instrument and
// computes an indicator.Snapshot. Returns (snap, true) on success;
// (Snapshot{}, false) when the fetcher is unwired, the provider
// chain returns ErrNoData / ErrNoProvider, or the resulting bars are
// too short to compute a meaningful snapshot. The caller decides
// whether to gracefully skip or surface the missing data — every
// current caller treats false as "no indicator overlay this round".
//
// The fetch is capped to 200 daily bars: enough for SMA200, MACD
// (slow=26, signal=9 → need ~35 bars to be valid), and any 60-bar
// lookback the future Phase 2D fundamentals layer wants to compare
// against. The cache layer above this fetcher dedupes across the
// per-call fan-out so back-to-back debate / quant signal queries on
// the same symbol cost one upstream call.
func (p runtimeResearcherPool) indicatorSnapshot(ctx context.Context, instrument marketdata.InstrumentRef) (indicator.Snapshot, bool) {
	if p.ohlcFetcher == nil {
		return indicator.Snapshot{}, false
	}
	req := ohlc.FetchRequest{
		Symbol:    instrument.NormalizedSymbol(),
		Market:    instrument.Market,
		Interval:  ohlc.IntervalDay,
		LookbackN: 200,
	}
	bars, err := p.ohlcFetcher.Fetch(ctx, req)
	if err != nil {
		if !errors.Is(err, ohlc.ErrNoData) && !errors.Is(err, ohlc.ErrNoProvider) {
			slog.Debug("ohlc fetch failed", "symbol", instrument.NormalizedSymbol(), "market", instrument.Market, "err", err)
		}
		return indicator.Snapshot{}, false
	}
	if len(bars) < 5 {
		return indicator.Snapshot{}, false
	}
	snap := indicator.Compute(bars)
	if snap.LastClose <= 0 || len(snap.Tags) == 0 {
		return indicator.Snapshot{}, false
	}
	return snap, true
}

// fundamentalMetrics fetches a single symbol's PE/PB/margins/growth
// snapshot via the wired fundamental.Fetcher. Best-effort: returns
// (nil, false) when the fetcher is nil, the upstream lacks
// coverage (ErrNoData / ErrNoProvider), or the symbol normalisation
// would produce an empty key.
//
// The returned *Metrics is safe to format via FormatForPrompt
// directly. The Cache layer above the registry dedupes across the
// per-call fan-out so the same symbol costs one upstream call per
// TTL window.
func (p runtimeResearcherPool) fundamentalMetrics(ctx context.Context, instrument marketdata.InstrumentRef) (*fundamental.Metrics, bool) {
	if p.fundamentalFetcher == nil {
		return nil, false
	}
	symbol := strings.TrimSpace(instrument.NormalizedSymbol())
	if symbol == "" {
		return nil, false
	}
	metrics, err := p.fundamentalFetcher.Fetch(ctx, fundamental.FetchRequest{
		Symbol: symbol,
		Market: instrument.Market,
	})
	if err != nil {
		if !errors.Is(err, fundamental.ErrNoData) && !errors.Is(err, fundamental.ErrNoProvider) {
			slog.Debug("fundamental fetch failed", "symbol", symbol, "market", instrument.Market, "err", err)
		}
		return nil, false
	}
	return metrics, metrics != nil
}

func (p runtimeResearcherPool) marketResearch(ctx context.Context, instrument marketdata.InstrumentRef, benchmark *marketdata.InstrumentRef) (*marketdata.ResearchContext, error) {
	if p.marketData == nil || !p.marketData.Enabled() {
		return nil, marketdata.ErrQuoteUnavailable
	}
	return p.marketData.GetResearchContext(ctx, instrument, benchmark, 3)
}

func (p runtimeResearcherPool) persistResearchMemory(ctx context.Context, fundID, agentID, tradingDate string, research *marketdata.ResearchContext) {
	if p.memoryRepo == nil || research == nil {
		return
	}
	symbol := research.Instrument.NormalizedSymbol()
	if symbol == "" {
		return
	}
	lang := LanguageFromContext(ctx)
	content := formatResearchContextBlock(lang, symbol, research)
	titleSuffix := " 市场研究"
	if lang == UserLanguageEN {
		titleSuffix = " market research"
	}
	memory := &repository.Memory{
		FundID:      fundID,
		AgentID:     nullString(agentID),
		Layer:       "analysis",
		Title:       nullString(symbol + titleSuffix),
		Content:     content,
		TradingDate: sql.NullTime{Time: parseTradingDateOrNow(tradingDate), Valid: strings.TrimSpace(tradingDate) != ""},
		Tags:        normalizedStringSlice([]string{"market-research", symbol, research.Instrument.Market, research.Instrument.AssetClass}),
	}
	_, _ = p.memoryRepo.Create(ctx, memory)
}

func researcherAgentID(agent *repository.Agent, fallback string) string {
	if agent != nil && strings.TrimSpace(agent.ID) != "" {
		return strings.TrimSpace(agent.ID)
	}
	return fallback
}

func researchFocusLabel(lang UserLanguage, focus workflow.ResearchFocus) string {
	if lang == UserLanguageEN {
		switch focus {
		case workflow.FocusMacro:
			return "Macro research"
		case workflow.FocusStock:
			return "Stock research"
		case workflow.FocusFundamental:
			return "Fundamental research"
		default:
			raw := strings.TrimSpace(string(focus))
			if raw == "" {
				return "Research"
			}
			return raw + " research"
		}
	}
	switch focus {
	case workflow.FocusMacro:
		return "宏观研究"
	case workflow.FocusStock:
		return "个股研究"
	case workflow.FocusFundamental:
		return "基本面研究"
	default:
		raw := strings.TrimSpace(string(focus))
		if raw == "" {
			return "研究"
		}
		return raw + "研究"
	}
}

func (a *runtimePMAgent) GeneratePlan(ctx context.Context, fundID, tradingDate string, roundtable *workflow.RoundtableResult) (*workflow.InvestmentPlanResult, error) {
	if roundtable == nil {
		return nil, api.ErrBadInput
	}
	skillContext := a.buildSkillContext(ctx, fundID, roundtable)
	parsedTradingDate := parseTradingDateOrNow(tradingDate)
	actions, planConfidence, err := a.buildPlanActions(ctx, fundID, parsedTradingDate, roundtable)
	if err != nil {
		return nil, err
	}
	// Phase 3A-1: stamp the default attribution tag on every
	// action produced by the LLM PM path. When classical strategy
	// sleeves (Phase 3A-4) become co-producers of plan actions
	// they'll set their own sleeve/signal_source before this call,
	// and the helper leaves those values alone.
	stampDefaultAttribution(actions, "llm_pm", "llm_pm")
	// Phase 3A-4: deterministic strategy sleeves (trend +
	// mean_reversion) run AFTER the LLM path. Their proposals
	// take priority on the instruments they cover because
	// indicator signals are deterministic and the LLM PM can be
	// noisy. The merge replaces conflicting LLM actions
	// in-place; sleeve actions on instruments the LLM didn't
	// touch are appended.
	if sleeveActions := a.evaluateStrategySleeves(ctx, fundID, parsedTradingDate); len(sleeveActions) > 0 {
		actions = mergeSleeveActions(sleeveActions, actions)
	}
	// Phase 3A-2: deterministic exit manager runs AFTER both the
	// LLM and the strategy sleeves so the operator can still see
	// what each upstream proposed in the trace, but its sell
	// decisions take priority on any instrument it fires on. The
	// merge replaces conflicting actions and prepends the exit-
	// manager sells so they execute first in the trading engine.
	// Failures are logged but never propagated — exits are a
	// risk hygiene layer, not a hard dependency of plan
	// generation.
	if exitActions := a.evaluateExitActions(ctx, fundID, parsedTradingDate); len(exitActions) > 0 {
		actions = mergeExitActions(exitActions, actions)
	}
	// Phase 3A-3: classify the recent market regime for every
	// (symbol, market) referenced by the plan and stamp the
	// matching tag onto the action. This runs LAST so the regime
	// is recorded for BOTH LLM-proposed actions and exit-manager
	// sells using the same classifier — the closed_lots row that
	// records the eventual exit will inherit the entry regime
	// from position_lots and observe its own exit-time regime.
	a.stampRegimeTags(ctx, actions)
	reasoning := buildReadablePMPlanReasoning(roundtable, skillContext, actions)
	if strings.TrimSpace(reasoning) == "" {
		reasoning = strings.Join(roundtable.Consensus, "\n")
	}
	if skillContext != "" {
		if reasoning != "" && !strings.Contains(reasoning, skillContext) {
			reasoning += "\n\n"
			reasoning += "补充约束:\n" + skillContext
		} else if reasoning == "" {
			reasoning += skillContext
		}
	}
	plan := &repository.InvestmentPlan{
		FundID:             fundID,
		TradingDate:        parsedTradingDate,
		Status:             string(workflow.PlanStatusPendingUser),
		Reasoning:          sql.NullString{String: reasoning, Valid: reasoning != ""},
		RiskScore:          sql.NullFloat64{Float64: 0, Valid: true},
		ExpectedReturn:     sql.NullFloat64{Float64: 0, Valid: true},
		DiscussionSnapshot: buildDecisionTraceSnapshot(roundtable),
		// Plan-level confidence comes from the LLM decision engine
		// (Phase 2A) or, when fallback fires, from the deterministic
		// legacy heuristic (0.55, below the auto-execute floor).
		Confidence: sql.NullFloat64{Float64: planConfidence, Valid: planConfidence > 0},
	}
	id, err := a.planRepo.CreateWithActions(ctx, plan, actions)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	// Sprint 11.2 — persist the decision provenance tag right
	// after CreateWithActions returns. This is intentionally
	// soft-fail: a transient UPDATE failure logs a warning but
	// does not break plan creation. Worst case the row stays at
	// the SQL default ('legacy'), which is strictly better than
	// failing the whole PM tick. consumeDecisionSource also
	// guards against missing records so tests that drive the
	// adapter without going through buildPlanActions keep
	// passing.
	if rec, ok := a.consumeDecisionSource(fundID); ok {
		if err := a.planRepo.SetDecisionSource(ctx, id, rec.Source, rec.ReasonJSON); err != nil {
			slog.Warn("plan_repo SetDecisionSource failed", "fundId", fundID, "planId", id, "err", err)
		}
	}
	// G1 #2: attribution writer needs the LLM's per-action
	// reasoning (where the PM names blocks like "qualityScores",
	// "valueScores", etc.) plus the high-level summary. The
	// summary alone — buildReadablePMPlanReasoning — paraphrases
	// roundtable cases and rarely mentions block vocabulary, so
	// the per-action JSON reasoning is where most citations
	// actually live.
	a.persistBlockContributions(ctx, fundID, id, combineReasoning(reasoning, actions))
	return &workflow.InvestmentPlanResult{
		ID:           id,
		FundID:       fundID,
		Status:       workflow.PlanStatusPendingUser,
		RoundtableID: roundtable.ID,
	}, nil
}

// combineReasoning concatenates the plan's top-level summary
// with every action's per-action reasoning into one big text
// blob, separated by line breaks so the citation regex can scan
// the entire surface. We strip empty entries to keep noise
// down. The output is consumed by BuildContributions; it never
// goes back into the database.
func combineReasoning(summary string, actions []repository.PlanAction) string {
	parts := make([]string, 0, 1+len(actions))
	if s := strings.TrimSpace(summary); s != "" {
		parts = append(parts, s)
	}
	for _, a := range actions {
		if a.Reasoning.Valid {
			if s := strings.TrimSpace(a.Reasoning.String); s != "" {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// persistBlockContributions is the G1 #2 attribution writer. It
// pairs the trace stashed by buildDecisionInput with the final
// reasoning text and writes the JSON payload onto the freshly-
// created plan. Soft-fail throughout — attribution is a
// dashboard signal, not a correctness one; a missing trace
// (legacy path took over OR the fund had no decision input
// built this tick) silently drops the write, and DB errors are
// logged-and-swallowed so the plan-create response is
// unaffected.
func (a *runtimePMAgent) persistBlockContributions(ctx context.Context, fundID, planID, reasoning string) {
	if a == nil || a.planRepo == nil || strings.TrimSpace(planID) == "" {
		return
	}
	raw, ok := a.lastTraceByFund.LoadAndDelete(fundID)
	if !ok {
		// Legacy path / decision engine off / fundID empty —
		// no attribution to record.
		return
	}
	trace, ok := raw.(decision.Trace)
	if !ok {
		return
	}
	contributions := decision.BuildContributions(trace, reasoning)
	payload, err := contributions.EncodeToJSON()
	if err != nil {
		slog.Warn("plan attribution: encode contributions failed",
			"fundId", fundID, "planId", planID, "err", err)
		return
	}
	if err := a.planRepo.SetBlockContributions(ctx, planID, payload); err != nil {
		slog.Warn("plan attribution: persist contributions failed",
			"fundId", fundID, "planId", planID, "err", err)
	}
}

// stampDefaultAttribution fills in any blank sleeve / signal_source
// fields on the actions in-place, and (for sell/reduce actions) the
// default exit_reason. The helper is no-op on fields that already
// carry a value, so classical strategy engines (Phase 3A-4) can
// pre-tag their actions with their own sleeve name and this fallback
// won't clobber them.
//
// This is the *write-side* counterpart to the recordLotFill default
// in tradeRepoCreateAndFill: stamping at action-creation time means
// the plan_actions table carries the tag too, so attribution queries
// that don't touch the lot ledger still get a non-NULL sleeve column.
func stampDefaultAttribution(actions []repository.PlanAction, sleeve, signalSource string) {
	for i := range actions {
		if !actions[i].Sleeve.Valid || strings.TrimSpace(actions[i].Sleeve.String) == "" {
			actions[i].Sleeve = sql.NullString{String: sleeve, Valid: sleeve != ""}
		}
		if !actions[i].SignalSource.Valid || strings.TrimSpace(actions[i].SignalSource.String) == "" {
			actions[i].SignalSource = sql.NullString{String: signalSource, Valid: signalSource != ""}
		}
		// Default exit_reason for sell-like actions when the
		// originating engine didn't set one (e.g. an LLM-proposed
		// reduce). The exit manager (Phase 3A-2) will set its own
		// reason — "stop_loss" / "trailing" / "time_stop" — which
		// this guard preserves.
		switch strings.ToLower(strings.TrimSpace(actions[i].Action)) {
		case "sell", "reduce":
			if !actions[i].ExitReason.Valid || strings.TrimSpace(actions[i].ExitReason.String) == "" {
				actions[i].ExitReason = sql.NullString{String: "llm_decision", Valid: true}
			}
		}
	}
}

// evaluateExitActions runs the Phase 3A-2 deterministic exit
// manager against the fund's open lots and returns one
// PlanAction per (fund, instrument) where a rule fired.
//
// Returns nil — never an error — on every failure path:
//
//   - exit manager / lotRepo not wired (legacy deployments + tests)
//   - fund.config.exitPolicy disabled or empty
//   - fund / position lookup failed (logged at warn level)
//   - no positions held
//
// The "nil on failure" convention matches the helper's role: the
// exit manager is a safety net that augments the LLM plan; if it
// can't run, the LLM plan stands.
func (a *runtimePMAgent) evaluateExitActions(ctx context.Context, fundID string, tradingDate time.Time) []repository.PlanAction {
	if a == nil || a.exitManager == nil || a.lotRepo == nil || a.fundRepo == nil || a.positionRepo == nil {
		return nil
	}
	fund, err := a.fundRepo.GetByID(ctx, fundID)
	if err != nil || fund == nil {
		if err != nil {
			slog.Warn("exit manager: fund lookup failed",
				"fund_id", fundID,
				"error", err,
			)
		}
		return nil
	}
	policy := exitmanager.PolicyFromFundConfig(fund.Config)
	if !policy.HasAnyRule() {
		return nil
	}
	positions, err := a.positionRepo.ListByFund(ctx, fundID)
	if err != nil {
		slog.Warn("exit manager: position lookup failed",
			"fund_id", fundID,
			"error", err,
		)
		return nil
	}
	if len(positions) == 0 {
		return nil
	}
	// Sprint D #3: pre-fetch per-symbol ATR14 once for the whole
	// fund so the ATR stop rule can fire without re-running
	// quantsnapshot per (instrument, lot). Cheap: BuildBatch is
	// already deduped by (symbol, market) inside the package.
	atrBySymbol := a.fetchATRForPositions(ctx, fund, positions)
	views := make([]exitmanager.PositionView, 0, len(positions))
	for i := range positions {
		pos := positions[i]
		// Skip futures shorts — the lot ledger only models long
		// lots in Phase 3A-1, so the exit manager can't reason
		// about short positions yet. A future iteration will
		// invert the threshold checks and lift this guard.
		if strings.EqualFold(strings.TrimSpace(pos.PositionSide.String), "short") {
			continue
		}
		if pos.Quantity <= 0 {
			continue
		}
		// Skip closed-out / nominally-held rows: a zero
		// current_price means the refresher hasn't ticked yet,
		// and we'd rather no-op than fire on a stale zero.
		if pos.CurrentPrice <= 0 {
			continue
		}
		lots, err := a.lotRepo.ListOpenByInstrument(ctx, fundID, pos.InstrumentKey)
		if err != nil {
			slog.Warn("exit manager: lot lookup failed",
				"fund_id", fundID,
				"instrument_key", pos.InstrumentKey,
				"error", err,
			)
			continue
		}
		if len(lots) == 0 {
			// Legacy positions with no lot-ledger backing — the
			// exit manager has nothing to evaluate against
			// (entry price / opened_at / highest_price_seen are
			// all missing). Skip silently; PR-3A-1 backfill
			// covers new fills going forward.
			continue
		}
		// Resolve ATR14 from the prefetch map. Missing key just
		// means "no quantsnapshot for this symbol today" — the
		// ATR stop will no-op for this position, the other
		// rules keep firing as usual.
		atr := 0.0
		if atrBySymbol != nil {
			if v, ok := atrBySymbol[strings.ToUpper(strings.TrimSpace(pos.Symbol))]; ok {
				atr = v
			}
		}
		views = append(views, exitmanager.PositionView{
			InstrumentKey: pos.InstrumentKey,
			Symbol:        pos.Symbol,
			Market:        pos.Market.String,
			AssetClass:    pos.AssetClass.String,
			CurrentPrice:  pos.CurrentPrice,
			QuoteAsOf:     pos.UpdatedAt,
			ATR14:         atr,
			OpenLots:      lots,
		})
	}
	if len(views) == 0 {
		return nil
	}
	decisions := a.exitManager.Evaluate(policy, views)
	if len(decisions) == 0 {
		return nil
	}
	out := make([]repository.PlanAction, 0, len(decisions))
	for _, d := range decisions {
		// Match the position-side / market metadata from the
		// holding row so the trading engine treats the close as
		// a same-currency same-market sell. We re-lookup the
		// position from the slice above; cheap (≤ 30 entries
		// per fund in practice).
		var posMeta *repository.HoldingPosition
		for i := range positions {
			if positions[i].InstrumentKey == d.InstrumentKey {
				posMeta = &positions[i]
				break
			}
		}
		out = append(out, buildExitPlanAction(d, posMeta))
	}
	return out
}

// fetchATRForPositions reuses the existing quantsnapshot builder
// (Sprint A #1) to source per-symbol 14-bar ATR for the exit
// manager's ATR-stop rule. We piggyback on the same indicator
// pipeline rather than recomputing ATR locally so:
//
//   - the rule respects whatever ATRPeriod / OHLC source the
//     quantsnapshot builder is wired with (no drift between the
//     "what the LLM sees" and "what the exit manager fires on"
//     volatility series);
//   - we get free dedup, concurrency, and short-history guards
//     out of the builder;
//   - tests can omit the quantsnapshot builder entirely (returns
//     nil), which makes the ATR-stop rule a no-op — exactly the
//     behaviour the rest of the exit manager already expects when
//     a signal is missing.
//
// Returns nil when the builder isn't wired or the position slice
// has no usable symbol. A nil return is treated by the caller as
// "no ATR available for any symbol" → ATR-stop rule no-ops for
// every position; the other rules continue firing normally.
//
// Market resolution: positions inherit the fund's primary market
// when their own market column is blank (legacy rows pre-dating
// the per-row market backfill). This matches the convention used
// by buildQuantSnapshots so a fund's holdings always resolve
// against the same OHLC adapter.
func (a *runtimePMAgent) fetchATRForPositions(ctx context.Context, fund *repository.Fund, positions []repository.HoldingPosition) map[string]float64 {
	if a == nil || a.quantSnapshot == nil || fund == nil || len(positions) == 0 {
		return nil
	}
	profile := decodeFundMarketProfile(fund.Config)
	fundMarket := strings.ToLower(strings.TrimSpace(profile.Market))
	seen := make(map[string]struct{}, len(positions))
	requests := make([]quantsnapshot.SymbolRequest, 0, len(positions))
	for _, pos := range positions {
		key := strings.ToUpper(strings.TrimSpace(pos.Symbol))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		mk := fundMarket
		if posMarket := strings.ToLower(strings.TrimSpace(pos.Market.String)); posMarket != "" {
			mk = posMarket
		}
		requests = append(requests, quantsnapshot.SymbolRequest{Symbol: key, Market: mk})
	}
	if len(requests) == 0 {
		return nil
	}
	snapshots := a.quantSnapshot.BuildBatch(ctx, requests)
	if len(snapshots) == 0 {
		return nil
	}
	out := make(map[string]float64, len(snapshots))
	for _, s := range snapshots {
		if s.ATR14 <= 0 {
			continue
		}
		out[strings.ToUpper(strings.TrimSpace(s.Symbol))] = s.ATR14
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildExitPlanAction translates an ExitDecision plus the
// matching holding row into a fully-typed PlanAction ready for
// PlanRepo.CreateWithActions. The plan_actions row carries:
//
//   - action       = "sell"
//   - sleeve       = "exit_manager"  (overrides the default "llm_pm")
//   - signal_source= decision.SignalSource (= decision.Reason)
//   - exit_reason  = decision.Reason
//   - confidence   = 1.0 (these are deterministic risk rules)
//   - reasoning    = human-readable rule trace from the rule
//   - price        = current quote at evaluation time
//
// Currency / multiplier / settlement fields are copied straight
// from the holding row so the executor doesn't have to re-fetch
// instrument metadata.
func buildExitPlanAction(d exitmanager.ExitDecision, pos *repository.HoldingPosition) repository.PlanAction {
	action := repository.PlanAction{
		InstrumentKey: d.InstrumentKey,
		Symbol:        d.Symbol,
		Action:        "sell",
		Quantity:      sql.NullFloat64{Float64: d.Quantity, Valid: d.Quantity > 0},
		Price:         sql.NullFloat64{Float64: d.TriggerPrice, Valid: d.TriggerPrice > 0},
		Reasoning:     sql.NullString{String: d.Reasoning, Valid: d.Reasoning != ""},
		Confidence:    sql.NullFloat64{Float64: 1.0, Valid: true},
		Sleeve:        sql.NullString{String: "exit_manager", Valid: true},
		SignalSource:  sql.NullString{String: d.SignalSource, Valid: d.SignalSource != ""},
		ExitReason:    sql.NullString{String: d.Reason, Valid: d.Reason != ""},
	}
	if d.Market != "" {
		action.Market = sql.NullString{String: d.Market, Valid: true}
	}
	if d.AssetClass != "" {
		action.AssetClass = sql.NullString{String: d.AssetClass, Valid: true}
	}
	if pos != nil {
		action.Exchange = pos.Exchange
		action.PositionSide = pos.PositionSide
		// exit_manager always reduces an existing long → open_close
		// = "close". Tagging it explicitly avoids an unnecessary
		// inference pass in the trading engine.
		action.OpenClose = sql.NullString{String: "close", Valid: true}
		action.QuoteCurrency = pos.QuoteCurrency
		action.SettlementCurrency = pos.SettlementCurrency
		action.MarginMode = pos.MarginMode
		action.Leverage = pos.Leverage
		action.ContractMultiplier = pos.ContractMultiplier
		action.ReduceOnly = sql.NullBool{Bool: true, Valid: true}
		if action.AssetClass.String == "" {
			action.AssetClass = pos.AssetClass
		}
		if action.Market.String == "" {
			action.Market = pos.Market
		}
	}
	return action
}

// evaluateStrategySleeves runs the Phase 3A-4 classical strategy
// sleeves (trend + mean_reversion) against the fund's current
// holdings and returns the resulting PlanActions. Behaviour is
// nil-safe: any missing wiring or disabled policy short-circuits
// to nil with zero side effects.
//
// Scope decision: Phase 3A-4 evaluates ONLY held positions. The
// sleeves can therefore propose:
//
//   - "add to a winner" (trend BUY signal on a held name)
//   - "exit on a breakdown" (trend SELL signal)
//   - "trim into strength" (mean_reversion SELL on overbought)
//   - "average down" (mean_reversion BUY on oversold)
//
// New-name discovery (sleeves scanning the universe for fresh
// breakouts) is deferred to a later PR. Doing it now would
// collide with the LLM PM's universe-scanning role and force a
// sizing model the sleeves don't have yet.
//
// Errors are logged and swallowed: a flaky OHLC upstream MUST
// NOT block decision generation — sleeves are a hygiene + data
// layer, not a hard gate.
// loadMutedSleeveRegimes folds the most recent batch of
// attribution lessons (layer="attribution") into a
// []strategy.SleeveRegimeMute. Only lessons tagged "loser" with
// a sleeve + regime pair survive — winner and insufficient_data
// lessons stay in the memory store but never gate the strategy
// service. Nil-safe: missing memoryRepo or query failure both
// produce an empty slice (the loop in evaluateStrategySleeves
// skips the WithMutedSleeveRegimes call in that case).
func (a *runtimePMAgent) loadMutedSleeveRegimes(ctx context.Context, fundID string) []strategy.SleeveRegimeMute {
	if a == nil || a.memoryRepo == nil {
		return nil
	}
	// 100 is plenty: the lesson generator caps itself at 20 per
	// run and we never look back more than ~5 attribution
	// windows in practice.
	rows, err := a.memoryRepo.ListByFund(ctx, fundID, attribution.MemoryLayer, 100)
	if err != nil {
		slog.Warn("strategy: failed to load attribution lessons", "fund_id", fundID, "error", err)
		return nil
	}
	seen := make(map[string]struct{}, len(rows))
	out := make([]strategy.SleeveRegimeMute, 0, len(rows))
	for _, row := range rows {
		if !isLoserLesson(row.Tags) {
			continue
		}
		sleeve, regimeName, ok := extractSleeveRegimeTags(row.Tags)
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(sleeve)) + "|" + strings.ToLower(strings.TrimSpace(regimeName))
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, strategy.SleeveRegimeMute{Sleeve: sleeve, Regime: regimeName})
	}
	return out
}

// isLoserLesson returns true when the tag set contains the
// canonical "loser" marker the attribution lesson generator
// writes. Other lesson kinds (winner, insufficient_data) are
// informational only and must NOT mute sleeves.
func isLoserLesson(tags []string) bool {
	for _, t := range tags {
		if t == "loser" {
			return true
		}
	}
	return false
}

// extractSleeveRegimeTags pulls the "sleeve:X" and "regime:Y"
// pair out of a tag set. Returns ok=false when either prefix
// is missing or empty (the lesson generator always writes both,
// but defensive against future tag-set drift).
func extractSleeveRegimeTags(tags []string) (sleeve, regimeName string, ok bool) {
	for _, t := range tags {
		switch {
		case strings.HasPrefix(t, "sleeve:"):
			v := strings.TrimSpace(strings.TrimPrefix(t, "sleeve:"))
			if v != "" && v != "(unspecified)" {
				sleeve = v
			}
		case strings.HasPrefix(t, "regime:"):
			v := strings.TrimSpace(strings.TrimPrefix(t, "regime:"))
			if v != "" && v != "(unspecified)" {
				regimeName = v
			}
		}
	}
	return sleeve, regimeName, sleeve != "" && regimeName != ""
}

func (a *runtimePMAgent) evaluateStrategySleeves(ctx context.Context, fundID string, tradingDate time.Time) []repository.PlanAction {
	if a == nil || a.ohlcFetcher == nil || a.fundRepo == nil || a.positionRepo == nil {
		return nil
	}
	fund, err := a.fundRepo.GetByID(ctx, fundID)
	if err != nil || fund == nil {
		return nil
	}
	policy := strategy.PolicyFromFundConfig(fund.Config)
	if !policy.HasAnySleeve() {
		return nil
	}
	svc := strategy.NewService(policy)
	if svc == nil {
		return nil
	}
	// Phase 3A-5 self-learning loop: load the attribution
	// lesson gate so strategy.Service silently drops proposals
	// from (sleeve, regime) cells the lesson generator has
	// previously flagged as money-losers. Soft-fail — a memory
	// fetch error must never block the trading workflow.
	if muted := a.loadMutedSleeveRegimes(ctx, fundID); len(muted) > 0 {
		svc = svc.WithMutedSleeveRegimes(muted)
	}
	// Phase 3A-6 ATR sizing: opt-in per fund. When disabled
	// the sizing layer returns Applied=false for every action
	// and the legacy downstream sizer continues to set
	// quantities. We resolve NAV once here so the per-action
	// loop stays allocation-free.
	sizingPolicy := sizing.PolicyFromFundConfig(fund.Config)
	nav := fund.CurrentCapital
	if nav <= 0 {
		nav = fund.InitialCapital
	}
	positions, err := a.positionRepo.ListByFund(ctx, fundID)
	if err != nil {
		// Repository error is genuine — the fund might still
		// have a universe configured we could rank on, but we
		// shouldn't paper over a broken repo. Empty positions
		// (no holdings yet) is a different story: fall through
		// to the universe-only ranking path.
		return nil
	}
	bundles := make([]strategy.Bundle, 0, len(positions)+8)
	posBySymbol := make(map[string]*repository.HoldingPosition, len(positions))
	// barsByKey + lastCloseByKey power the ATR sizer below.
	// We pull them straight from the bundle so we don't pay
	// for a second OHLC fetch per instrument.
	barsByKey := make(map[string][]ohlc.Bar, len(positions)+8)
	lastCloseByKey := make(map[string]float64, len(positions)+8)
	seenInstrumentKey := make(map[string]struct{}, len(positions)+8)
	for i := range positions {
		pos := &positions[i]
		// Skip futures shorts — the lot ledger only models long
		// lots in Phase 3A-1, and the sleeves' SELL = "close
		// long" semantics break on a short position.
		if strings.EqualFold(strings.TrimSpace(pos.PositionSide.String), "short") {
			continue
		}
		if pos.Quantity <= 0 {
			continue
		}
		symbol := strings.TrimSpace(pos.Symbol)
		if symbol == "" {
			continue
		}
		bars, err := a.ohlcFetcher.Fetch(ctx, ohlc.FetchRequest{
			Symbol:    symbol,
			Market:    strings.ToLower(strings.TrimSpace(pos.Market.String)),
			Interval:  ohlc.IntervalDay,
			LookbackN: 250,
			EndTime:   tradingDate,
		})
		if err != nil || len(bars) == 0 {
			// Same soft-fail convention as the regime service:
			// no bars → skip this instrument silently.
			continue
		}
		r := regime.Unknown
		if a.regimeService != nil {
			classified, _ := a.regimeService.Classify(ctx, regime.Instrument{
				Symbol: symbol,
				Market: pos.Market.String,
			})
			r = classified
		}
		bundles = append(bundles, strategy.Bundle{
			InstrumentKey: pos.InstrumentKey,
			Symbol:        symbol,
			Market:        pos.Market.String,
			AssetClass:    pos.AssetClass.String,
			Bars:          bars,
			Regime:        r,
			AsOf:          bars[len(bars)-1].Time,
		})
		posBySymbol[pos.InstrumentKey] = pos
		barsByKey[pos.InstrumentKey] = bars
		lastCloseByKey[pos.InstrumentKey] = bars[len(bars)-1].Close
		seenInstrumentKey[pos.InstrumentKey] = struct{}{}
	}
	// PR-3A8 universe expansion: the cross-sectional momentum
	// sleeve needs a universe of >= MinUniverseSize bundles to
	// rank, and the per-instrument sleeves (trend / dual_ma /
	// mean_reversion) can legitimately want to open new
	// positions on names the fund doesn't currently hold. We
	// pull fund.config.universe.symbols and add any name the
	// position loop didn't already cover. Cap at 20 to stay
	// within the same OHLC budget the debate roundtable uses
	// — see profileUniverseSymbols + collectIndicatorBlock for
	// the matching contract.
	a.appendUniverseBundles(ctx, fund, tradingDate, seenInstrumentKey, &bundles, barsByKey, lastCloseByKey)
	if len(bundles) == 0 {
		return nil
	}
	sleeveActions, err := svc.Evaluate(ctx, bundles)
	if err != nil {
		slog.Warn("strategy: sleeve evaluation failed",
			"fund_id", fundID,
			"error", err,
		)
		return nil
	}
	if len(sleeveActions) == 0 {
		return nil
	}
	out := make([]repository.PlanAction, 0, len(sleeveActions))
	for _, sa := range sleeveActions {
		// Run the ATR sizer for buy actions. Sells already
		// cap quantity to the held lot, so sizing is a no-op
		// for them — we still pass through a disabled Result
		// so buildSleevePlanAction keeps a single signature.
		var sized sizing.Result
		if sa.Proposal.Action == strategy.ActionBuy && sizingPolicy.Enabled {
			lastClose := lastCloseByKey[sa.InstrumentKey]
			sized = sizing.Size(sizingPolicy, sizing.Input{
				NAV:          nav,
				Price:        lastClose,
				Bars:         barsByKey[sa.InstrumentKey],
				ExistingStop: sa.Proposal.StopLoss,
			})
			if !sized.Applied {
				slog.Info("sizing: ATR sizing not applied",
					"fund_id", fundID,
					"symbol", sa.Symbol,
					"sleeve", sa.Sleeve,
					"reason", sized.Reason,
				)
			}
		}
		out = append(out, buildSleevePlanAction(sa, posBySymbol[sa.InstrumentKey], sized, lastCloseByKey[sa.InstrumentKey]))
	}
	return out
}

// appendUniverseBundles adds bundle entries for any
// fund.config.universe.symbols the position loop didn't already
// cover. Pre-3A8 the strategy.Service only saw bundles for
// holdings, which left the cross-sectional momentum sleeve
// permanently below MinUniverseSize and the per-instrument
// sleeves unable to open new positions. The function is a
// best-effort enricher: OHLC failures, unresolved instrument
// keys, and missing market metadata all soft-fail to "skip this
// symbol" rather than blocking the trading workflow.
//
// instrumentKey synthesis: universe symbols come as bare strings;
// we synthesise a "<market>:<symbol>" key the same way other
// wiring paths do (see resolveInstrumentKey in plan_executor.go).
// Keeping the key stable across paths is what lets the merge
// layer dedupe correctly.
func (a *runtimePMAgent) appendUniverseBundles(
	ctx context.Context,
	fund *repository.Fund,
	tradingDate time.Time,
	seen map[string]struct{},
	bundles *[]strategy.Bundle,
	barsByKey map[string][]ohlc.Bar,
	lastCloseByKey map[string]float64,
) {
	if a == nil || a.ohlcFetcher == nil || fund == nil {
		return
	}
	profile := decodeFundMarketProfile(fund.Config)
	symbols := profileUniverseSymbols(profile)
	if len(symbols) == 0 {
		return
	}
	const maxUniverseSymbols = 20
	limit := len(symbols)
	if limit > maxUniverseSymbols {
		limit = maxUniverseSymbols
	}
	market := strings.ToLower(strings.TrimSpace(profile.Market))
	for i := 0; i < limit; i++ {
		symbol := strings.TrimSpace(symbols[i])
		if symbol == "" {
			continue
		}
		instrumentKey := strings.ToUpper(symbol)
		if market != "" {
			instrumentKey = strings.ToUpper(market + ":" + symbol)
		}
		if _, dup := seen[instrumentKey]; dup {
			continue
		}
		bars, err := a.ohlcFetcher.Fetch(ctx, ohlc.FetchRequest{
			Symbol:    symbol,
			Market:    market,
			Interval:  ohlc.IntervalDay,
			LookbackN: 250,
			EndTime:   tradingDate,
		})
		if err != nil || len(bars) == 0 {
			continue
		}
		r := regime.Unknown
		if a.regimeService != nil {
			classified, _ := a.regimeService.Classify(ctx, regime.Instrument{
				Symbol: symbol,
				Market: profile.Market,
			})
			r = classified
		}
		*bundles = append(*bundles, strategy.Bundle{
			InstrumentKey: instrumentKey,
			Symbol:        symbol,
			Market:        profile.Market,
			AssetClass:    profile.AssetClass,
			Bars:          bars,
			Regime:        r,
			AsOf:          bars[len(bars)-1].Time,
		})
		barsByKey[instrumentKey] = bars
		lastCloseByKey[instrumentKey] = bars[len(bars)-1].Close
		seen[instrumentKey] = struct{}{}
	}
}

// buildSleevePlanAction maps a strategy.SleeveAction onto a fully
// typed PlanAction.
//
//	BUY:  When the Phase 3A-6 ATR sizer (Result.Applied) ran,
//	      Quantity / Amount / Price / StopLoss are populated
//	      from the sizer output and the reason string is
//	      concatenated onto Reasoning. When the sizer was
//	      disabled / unable to size, Quantity is left NULL so
//	      the legacy downstream sizer picks a size from
//	      fund.config.autoExecute.maxOrderPctOfAssets — the
//	      pre-3A6 behaviour is preserved.
//	SELL: Always closes the held quantity (sleeves never short).
//
// sized.Applied is FALSE on the sell path because we don't run
// the ATR sizer there; the held lot is the only legal quantity.
//
// lastClose is the most recent close from the sleeve bundle —
// used (a) as the planning Price stamp so refreshActionQuantity
// has something to recompute against and (b) as the Amount
// denominator (qty * close).
func buildSleevePlanAction(sa strategy.SleeveAction, pos *repository.HoldingPosition, sized sizing.Result, lastClose float64) repository.PlanAction {
	action := repository.PlanAction{
		InstrumentKey: sa.InstrumentKey,
		Symbol:        sa.Symbol,
		Action:        string(sa.Proposal.Action),
		Price:         sql.NullFloat64{}, // let the executor refresh quote on dispatch
		Reasoning:     sql.NullString{String: sa.Proposal.Reasoning, Valid: sa.Proposal.Reasoning != ""},
		Confidence:    sql.NullFloat64{Float64: sa.Proposal.Confidence, Valid: sa.Proposal.Confidence > 0},
		Sleeve:        sql.NullString{String: sa.Sleeve, Valid: sa.Sleeve != ""},
		SignalSource:  sql.NullString{String: sa.Proposal.SignalSource, Valid: sa.Proposal.SignalSource != ""},
	}
	if sa.Market != "" {
		action.Market = sql.NullString{String: sa.Market, Valid: true}
	}
	if sa.AssetClass != "" {
		action.AssetClass = sql.NullString{String: sa.AssetClass, Valid: true}
	}
	if sa.Regime.IsKnown() {
		action.RegimeTag = sql.NullString{String: sa.Regime.String(), Valid: true}
	}
	if sa.Proposal.StopLoss > 0 {
		action.StopLoss = sql.NullFloat64{Float64: sa.Proposal.StopLoss, Valid: true}
	}
	if sa.Proposal.TakeProfit > 0 {
		action.TakeProfit = sql.NullFloat64{Float64: sa.Proposal.TakeProfit, Valid: true}
	}
	if pos != nil {
		action.Exchange = pos.Exchange
		action.PositionSide = pos.PositionSide
		action.QuoteCurrency = pos.QuoteCurrency
		action.SettlementCurrency = pos.SettlementCurrency
		action.MarginMode = pos.MarginMode
		action.Leverage = pos.Leverage
		action.ContractMultiplier = pos.ContractMultiplier
		if action.AssetClass.String == "" {
			action.AssetClass = pos.AssetClass
		}
		if action.Market.String == "" {
			action.Market = pos.Market
		}
	}
	switch sa.Proposal.Action {
	case strategy.ActionSell:
		// Cap the sleeve sell at the held quantity. Phase 3A-4
		// sleeves never short — they only close longs.
		if pos != nil && pos.Quantity > 0 {
			action.Quantity = sql.NullFloat64{Float64: pos.Quantity, Valid: true}
		}
		action.OpenClose = sql.NullString{String: "close", Valid: true}
		action.ReduceOnly = sql.NullBool{Bool: true, Valid: true}
		// Sleeve-driven sells get their exit_reason from the
		// signal source — the attribution agent groups by this.
		action.ExitReason = sql.NullString{String: sa.Sleeve, Valid: true}
	case strategy.ActionBuy:
		action.OpenClose = sql.NullString{String: "open", Valid: true}
		if sized.Applied {
			hint := instrument2.Hint{
				Market:     firstNonEmptyValue(sa.Market, action.Market.String),
				Exchange:   action.Exchange.String,
				AssetClass: firstNonEmptyValue(sa.AssetClass, action.AssetClass.String),
			}
			normalisedQty := instrument2.NormalizeBuyQty(sa.Symbol, hint, sized.Quantity)
			if normalisedQty > 0 {
				action.Quantity = sql.NullFloat64{Float64: normalisedQty, Valid: true}
				if lastClose > 0 {
					action.Price = sql.NullFloat64{Float64: lastClose, Valid: true}
					action.Amount = sql.NullFloat64{Float64: roundCurrency(normalisedQty * lastClose), Valid: true}
				}
				if sized.StopPrice > 0 {
					action.StopLoss = sql.NullFloat64{Float64: sized.StopPrice, Valid: true}
				}
				if sized.Reason != "" {
					if existing := strings.TrimSpace(action.Reasoning.String); existing != "" {
						action.Reasoning = sql.NullString{String: existing + " | " + sized.Reason, Valid: true}
					} else {
						action.Reasoning = sql.NullString{String: sized.Reason, Valid: true}
					}
				}
			}
		}
		// If sizing didn't apply, Quantity stays NULL and the
		// legacy downstream sizer (planBuyAmountWithinRiskCap
		// + refreshActionQuantity at dispatch) takes over.
	}
	return action
}

// mergeSleeveActions blends sleeve proposals into the running
// actions slice. Rules:
//
//  1. Sleeve actions are deduped per (instrument_key, action):
//     when two sleeves agree on the same side for the same
//     instrument, we keep the higher-confidence one and prepend
//     "+also <sleeve>" to its reasoning. The attribution agent
//     can still see the duplicate via plan_actions audit (this
//     deduplication is purely to avoid double-dispatching the
//     same order at the trading engine).
//  2. When sleeves disagree on the same instrument (one buy,
//     one sell), the higher-confidence one wins.
//  3. Sleeve actions REPLACE any LLM action on the same
//     instrument (deterministic indicators beat opinions).
//  4. Sleeve actions prepend LLM actions in the output slice
//     so they're dispatched first.
func mergeSleeveActions(sleeve, llm []repository.PlanAction) []repository.PlanAction {
	if len(sleeve) == 0 {
		return llm
	}
	// Step 1+2: de-duplicate sleeve actions per instrument by
	// keeping the strongest opinion.
	byInst := make(map[string]repository.PlanAction, len(sleeve))
	for _, s := range sleeve {
		key := s.InstrumentKey
		if existing, ok := byInst[key]; ok {
			if confidenceOrZero(existing) >= confidenceOrZero(s) {
				// Existing wins; annotate that another sleeve
				// concurred (if they agree) for the audit log.
				if existing.Action == s.Action && s.Sleeve.Valid {
					existing.Reasoning = sql.NullString{
						String: strings.TrimSpace(existing.Reasoning.String) + " | also " + s.Sleeve.String,
						Valid:  true,
					}
					byInst[key] = existing
				}
				continue
			}
			// Replace with the stronger opinion.
		}
		byInst[key] = s
	}
	// Step 3: strip LLM actions on instruments the sleeves cover.
	out := make([]repository.PlanAction, 0, len(byInst)+len(llm))
	for _, s := range byInst {
		out = append(out, s)
	}
	for _, a := range llm {
		if _, clash := byInst[a.InstrumentKey]; clash {
			slog.Info("strategy: dropping LLM action for instrument under sleeve",
				"instrument_key", a.InstrumentKey,
				"llm_action", a.Action,
			)
			continue
		}
		out = append(out, a)
	}
	return out
}

func confidenceOrZero(a repository.PlanAction) float64 {
	if a.Confidence.Valid {
		return a.Confidence.Float64
	}
	return 0
}

// stampRegimeTags batches every (symbol, market) pair referenced
// by the actions through the regime classifier and writes the
// result back onto action.RegimeTag in place. Behaviour:
//
//   - regimeService not wired or nil   → no-op (legacy behaviour)
//   - action already carries a regime  → preserved (a future
//     strategy sleeve may have set a regime-specific tag we don't
//     want to clobber with the generic classifier)
//   - classifier returns Unknown       → tag left NULL (the
//     attribution agent prefers NULL over a "we guessed wrong"
//     placeholder)
//   - classifier returns a known value → action.RegimeTag set
//
// Errors are logged and swallowed: regime tagging is a learning
// signal, not a trading gate. The decision path must survive a
// flaky OHLC upstream.
func (a *runtimePMAgent) stampRegimeTags(ctx context.Context, actions []repository.PlanAction) {
	if a == nil || a.regimeService == nil || len(actions) == 0 {
		return
	}
	// Collect the unique (symbol, market) set so a portfolio of
	// 10 positions with 5 distinct symbols only triggers 5
	// classifier calls (the Service de-dupes internally too, but
	// skipping the duplicate calls avoids the cache lookup cost).
	seen := make(map[string]regime.Instrument)
	for i := range actions {
		symbol := strings.TrimSpace(actions[i].Symbol)
		if symbol == "" {
			continue
		}
		market := strings.TrimSpace(actions[i].Market.String)
		key := strings.ToUpper(symbol) + "|" + strings.ToLower(market)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = regime.Instrument{Symbol: symbol, Market: market}
	}
	if len(seen) == 0 {
		return
	}
	instruments := make([]regime.Instrument, 0, len(seen))
	for _, inst := range seen {
		instruments = append(instruments, inst)
	}
	results, err := a.regimeService.ClassifyBatch(ctx, instruments)
	if err != nil {
		// Soft-fail: log the representative cause but still
		// apply whichever classifications succeeded.
		slog.Warn("regime: batch classify partially failed",
			"error", err,
			"instruments", len(instruments),
		)
	}
	if len(results) == 0 {
		return
	}
	for i := range actions {
		// Preserve any tag the upstream already wrote (e.g. a
		// strategy sleeve in Phase 3A-4 might know its own
		// regime better than the generic classifier).
		if actions[i].RegimeTag.Valid && strings.TrimSpace(actions[i].RegimeTag.String) != "" {
			continue
		}
		symbol := strings.TrimSpace(actions[i].Symbol)
		if symbol == "" {
			continue
		}
		market := strings.TrimSpace(actions[i].Market.String)
		key := strings.ToUpper(symbol) + "|" + strings.ToLower(market)
		r, ok := results[key]
		if !ok || !r.IsKnown() {
			continue
		}
		actions[i].RegimeTag = sql.NullString{String: r.String(), Valid: true}
	}
}

// mergeExitActions blends the deterministic exit-manager sells
// with the LLM-produced actions. Rules:
//
//  1. For every instrument with an exit decision, REMOVE any
//     LLM-produced action on the same instrument. The exit
//     manager wins outright — letting the LLM "vote against"
//     a stop-loss would defeat the purpose.
//  2. Prepend the exit actions so they appear first in the
//     plan_actions table (sort_order matters for the executor's
//     dispatch loop — exits before opens reduces capital tied
//     up in soon-to-close positions).
//  3. Preserve the relative ordering of the LLM actions that
//     survive the dedup, so the operator still sees the
//     decision engine's intended priority for unrelated names.
func mergeExitActions(exits, llm []repository.PlanAction) []repository.PlanAction {
	if len(exits) == 0 {
		return llm
	}
	exitKeys := make(map[string]struct{}, len(exits))
	for _, e := range exits {
		exitKeys[e.InstrumentKey] = struct{}{}
	}
	out := make([]repository.PlanAction, 0, len(exits)+len(llm))
	out = append(out, exits...)
	for _, a := range llm {
		if _, clash := exitKeys[a.InstrumentKey]; clash {
			slog.Info("exit manager: dropping LLM action for instrument under exit rule",
				"instrument_key", a.InstrumentKey,
				"llm_action", a.Action,
			)
			continue
		}
		out = append(out, a)
	}
	return out
}

func buildReadablePMPlanReasoning(roundtable *workflow.RoundtableResult, skillContext string, actions []repository.PlanAction) string {
	if roundtable == nil {
		return ""
	}
	sections := make([]string, 0, 4)
	if actionSummary := summarizePlanActionsForReasoning(actions); actionSummary != "" {
		sections = append(sections, "策略摘要:\n- 当前建议: "+actionSummary)
	} else {
		sections = append(sections, "策略摘要:\n- 当前建议: 暂不执行自动交易，等待更完整的标的、报价或风控输入。")
	}
	if evidence := extractDecisionEvidence(roundtable.Consensus, 4); len(evidence) > 0 {
		sections = append(sections, "主要依据:\n- "+strings.Join(evidence, "\n- "))
	}
	if riskNotes := extractRiskNotesFromActions(actions); len(riskNotes) > 0 {
		sections = append(sections, "执行与风险提示:\n- "+strings.Join(riskNotes, "\n- "))
	}
	if trimmedSkill := strings.TrimSpace(skillContext); trimmedSkill != "" {
		sections = append(sections, "策略纪律与上下文:\n"+trimmedSkill)
	}
	return strings.TrimSpace(strings.Join(sections, "\n\n"))
}

func summarizePlanActionsForReasoning(actions []repository.PlanAction) string {
	items := make([]string, 0, len(actions))
	for _, action := range actions {
		side := strings.TrimSpace(action.Action)
		if side == "" {
			continue
		}
		symbol := firstNonEmptyValue(strings.TrimSpace(action.Symbol), strings.TrimSpace(action.InstrumentKey), "未指定标的")
		parts := []string{humanizeActionForReasoning(side), symbol}
		if action.Amount.Valid && action.Amount.Float64 > 0 {
			parts = append(parts, fmt.Sprintf("约 %.2f 名义金额", action.Amount.Float64))
		}
		if action.Confidence.Valid && action.Confidence.Float64 > 0 {
			parts = append(parts, fmt.Sprintf("置信度 %.0f%%", action.Confidence.Float64*100))
		}
		items = append(items, strings.Join(parts, " · "))
	}
	return strings.Join(limitStrings(items, 5), "；")
}

func humanizeActionForReasoning(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "buy", "add":
		return "买入/增配"
	case "sell", "reduce":
		return "卖出/降仓"
	case "hold":
		return "持有观察"
	case "watch":
		return "仅观察"
	default:
		return strings.TrimSpace(action)
	}
}

func extractDecisionEvidence(consensus []string, limit int) []string {
	seen := make(map[string]struct{})
	items := make([]string, 0, limit)
	for _, block := range consensus {
		for _, line := range strings.Split(block, "\n") {
			line = normalizeReasoningLine(line)
			if line == "" || shouldSkipRawReasoningLine(line) {
				continue
			}
			key := strings.ToLower(line)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			items = append(items, line)
			if len(items) >= limit {
				return items
			}
		}
	}
	return items
}

func normalizeReasoningLine(line string) string {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
	line = strings.TrimSpace(strings.TrimPrefix(line, "•"))
	if strings.HasPrefix(strings.ToLower(line), "news:") {
		line = strings.TrimSpace(line[len("news:"):])
		if line != "" {
			line = "新闻: " + line
		}
	}
	if len([]rune(line)) > 180 {
		runes := []rune(line)
		line = string(runes[:180]) + "…"
	}
	return line
}

func shouldSkipRawReasoningLine(line string) bool {
	normalized := strings.ToLower(strings.TrimSpace(line))
	if normalized == "" {
		return true
	}
	for _, prefix := range []string{
		"macro brief", "stock research", "fundamental research", "quant signals", "fund focus", "specialization context",
		"market data snapshot", "benchmark:", "spy:", "market:", "asset class:", "primary direction:", "universe mode:",
		"universe themes:", "universe sectors:", "team specialization", "member specialization",
	} {
		if strings.HasPrefix(normalized, prefix) {
			return true
		}
	}
	return false
}

func extractRiskNotesFromActions(actions []repository.PlanAction) []string {
	items := make([]string, 0, 3)
	// settlementLockedPositions counts how many action lines have a
	// non-zero T+1 lock; we collapse those into a single market-level
	// reminder rather than repeating "A股 T+1" once per holding,
	// because the rule is a market property, not a per-symbol quirk.
	settlementLockedPositions := 0
	for _, action := range actions {
		reasoning := strings.TrimSpace(action.Reasoning.String)
		if reasoning == "" {
			continue
		}
		lower := strings.ToLower(reasoning)
		if strings.Contains(lower, "quote unavailable") {
			items = append(items, "未能从行情源取到 "+strings.ToUpper(strings.TrimSpace(action.Symbol))+" 的实时报价；可点击 \"刷新报价并重报\" 后再下单。")
		}
		if strings.Contains(lower, "structured ticker configuration is missing") {
			items = append(items, "标的配置仍不完整，建议先补充 universe symbols 或 specialization instruments。")
		}
		// Match the marker emitted by buildPlanActions when a T+1
		// settlement lock trimmed the proposed reduce qty.
		if strings.Contains(reasoning, "A股市场 T+1") {
			settlementLockedPositions++
		}
	}
	if settlementLockedPositions > 0 {
		// Phrased as a market rule so reviewers don't read it as a
		// per-stock judgement call.
		items = append(items, fmt.Sprintf(
			"A 股市场 T+1 结算规则生效：今日新建仓的 %d 个 A 股仓位需待下一交易日方可卖出，已自动从本次减仓提案中剔除。",
			settlementLockedPositions,
		))
	}
	return uniqueStrings(limitStrings(items, 3))
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func (a *runtimePMAgent) SubmitForExecution(ctx context.Context, planID string) error {
	return mapRepositoryError(a.planRepo.UpdateStatus(ctx, planID, "executing"))
}

func (a *runtimePMAgent) buildSkillContext(ctx context.Context, fundID string, roundtable *workflow.RoundtableResult) string {
	agent := findFundAgentByRoleWithFocus(ctx, fundID, string(workflow.RolePM), "", a.teamRepo, a.agentRepo, a.fundRepo)
	context := buildPMSkillContext(LanguageFromContext(ctx), agent, roundtable)
	fundFocusContext, specializationContext := buildRuntimeFundContextsByID(ctx, fundID, agent, a.fundRepo)
	context = appendSkillContext(context, fundFocusContext)
	return appendSkillContext(context, specializationContext)
}

func (a *runtimeRiskAgent) ReviewPlan(ctx context.Context, plan *workflow.InvestmentPlanResult) (bool, string, error) {
	if plan == nil {
		return false, "", api.ErrBadInput
	}
	storedPlan, err := a.planRepo.GetByID(ctx, plan.ID)
	if err != nil {
		return false, "", mapRepositoryError(err)
	}
	actions, err := a.planRepo.GetActions(ctx, plan.ID)
	if err != nil {
		return false, "", mapRepositoryError(err)
	}
	positions, err := a.positionRepo.ListByFund(ctx, plan.FundID)
	if err != nil {
		return false, "", mapRepositoryError(err)
	}
	riskAgent := findFundAgentByRoleWithFocus(ctx, plan.FundID, string(workflow.RoleRisk), "", a.teamRepo, a.agentRepo, a.fundRepo)
	lang := LanguageFromContext(ctx)
	skillContext := buildRiskSkillContext(lang, riskAgent, storedPlan, actions, positions)
	fundFocusContext, specializationContext := buildRuntimeFundContextsByID(ctx, plan.FundID, riskAgent, a.fundRepo)
	skillContext = appendSkillContext(skillContext, fundFocusContext)
	skillContext = appendSkillContext(skillContext, specializationContext)
	reviewCompleted := "风控审查已完成"
	matchedSkillsToken := "匹配技能："
	if lang == UserLanguageEN {
		reviewCompleted = "Risk review completed"
		matchedSkillsToken = "Matched skills:"
	}
	remarks := appendSkillContext(reviewCompleted, skillContext)
	payload, err := json.Marshal(map[string]any{
		"approved":      true,
		"remarks":       remarks,
		"reviewedAt":    time.Now().UTC().Format(time.RFC3339),
		"matchedSkills": strings.Contains(remarks, matchedSkillsToken),
	})
	if err != nil {
		return false, "", err
	}
	if _, err := a.planRepo.DB().ExecContext(ctx, `UPDATE investment_plans SET risk_review = $1, updated_at = NOW() WHERE id = $2`, payload, plan.ID); err != nil {
		return false, "", mapRepositoryError(err)
	}
	return true, remarks, nil
}

// buildPlanActions assembles the structured list of PlanActions for a
// new investment plan. Phase 2A introduces a two-stage flow:
//
//  1. If decisionEngine is wired (LLM-backed in production), call it
//     with a structured DecisionInput. The engine returns a list of
//     intended actions plus a plan-level confidence the auto-execute
//     gate consumes. We translate that output to []PlanAction while
//     still enforcing lot-size, sellable-today and T+1 normalisation.
//  2. If the engine is unset OR fails (network error, JSON parse
//     error, empty output), fall back to the deterministic legacy
//     heuristic ("reduce first position OR buy first universe symbol")
//     that this codebase has shipped since v1. The fallback path
//     emits confidence 0.55 so the auto-execute gate (default floor
//     0.60) never auto-approves a fallback plan.
//
// Return shape: (actions, planConfidence, err). planConfidence is in
// [0,1]; the wiring layer writes it directly to plan.confidence.
func (a *runtimePMAgent) buildPlanActions(ctx context.Context, fundID string, tradingDate time.Time, roundtable *workflow.RoundtableResult) ([]repository.PlanAction, float64, error) {
	fund, err := a.fundRepo.GetByID(ctx, fundID)
	if err != nil {
		return nil, 0, mapRepositoryError(err)
	}
	pmAgent := findFundAgentByRoleWithFocus(ctx, fundID, string(workflow.RolePM), "", a.teamRepo, a.agentRepo, a.fundRepo)
	positions, err := a.positionRepo.ListByFund(ctx, fundID)
	if err != nil {
		return nil, 0, mapRepositoryError(err)
	}
	positions = filterWorkflowPlanPositions(positions)
	// Pull today's filled buys so we can compute SellableQtyToday for
	// each held position. On T+1 markets (A-share) freshly bought
	// shares are locked from sale during the current session — both
	// the LLM path and the legacy fallback need this signal to either
	// demote sell/reduce actions to hold or cap qty to sellableToday.
	// tradeRepo may be nil in tests; degrade to "no intraday buys
	// known" which fails open (runtime SettlementCycleRule still
	// catches violations).
	boughtTodayByKey := make(map[string]float64)
	if a.tradeRepo != nil && len(positions) > 0 {
		if sums, err := a.tradeRepo.SumFilledBuyTodayByInstrument(ctx, fundID, normalizeTradingDate(tradingDate)); err == nil {
			boughtTodayByKey = sums
		}
	}

	if a.decisionEngine != nil {
		actions, confidence, err := a.runDecisionEngine(ctx, fund, pmAgent, positions, boughtTodayByKey, roundtable, tradingDate, fundID)
		if err == nil && len(actions) > 0 {
			a.recordDecisionSource(fundID, llmDecisionSourceFor(a.decisionEngine), errorclass.Detail{})
			return actions, confidence, nil
		}
		// LLM path failed OR returned an empty plan. Both feed the
		// fallback heuristic, but Sprint 11 distinguishes them in
		// the persisted decision_source tag so the admin LLM-health
		// board and the user-facing chip can differentiate "model
		// errored" (fallback_after_llm_error, actionable for the
		// user) from "model returned no actions" (fallback_empty_plan,
		// often signals a degenerate input rather than infra trouble).
		if err != nil {
			slog.Warn("pm decision engine failed, falling back to deterministic heuristic", "fundId", fundID, "err", err)
			detail := errorclass.Classify(err)
			a.recordDecisionSource(fundID, "fallback_after_llm_error", detail)
		} else {
			a.recordDecisionSource(fundID, "fallback_empty_plan", errorclass.Detail{
				Category: errorclass.CategoryEmptyResponse,
				Summary:  "decision engine returned zero actions",
				At:       time.Now().UTC(),
			})
		}
	} else {
		a.recordDecisionSource(fundID, "fallback_no_llm", errorclass.Detail{
			Category: errorclass.CategoryUnknown,
			Summary:  "no decision engine wired (legacy deploy or test stub)",
			At:       time.Now().UTC(),
		})
	}

	actions, err := a.buildPlanActionsLegacy(ctx, fund, pmAgent, positions, boughtTodayByKey, roundtable, tradingDate, fundID)
	if err != nil {
		return nil, 0, err
	}
	return actions, 0.55, nil
}

// llmDecisionSourceFor maps the concrete decision.DecisionEngine
// implementation to its Sprint 11 decision_source tag. Returns
// "fallback_no_llm" for the FallbackEngine — that engine signals
// "no LLM" rather than "LLM succeeded".
func llmDecisionSourceFor(engine decision.DecisionEngine) string {
	switch engine.(type) {
	case *decision.ThreeStageEngine:
		return "llm_three_stage"
	case *decision.LLMDecisionEngine:
		return "llm_pm"
	case decision.FallbackEngine:
		return "fallback_no_llm"
	default:
		return "llm_pm" // unknown wrapper, treat as a normal LLM run
	}
}

// recordDecisionSource stashes the Sprint 11 provenance tuple for the
// current fund's in-flight plan. The companion GeneratePlan path will
// load-and-delete the entry via consumeDecisionSource right after
// PlanRepo.CreateWithActions returns, then call
// PlanRepo.SetDecisionSource. Stale rows can't accumulate because each
// store overwrites the previous one for the same fundID.
//
// The reason argument is a zero-value Detail for the "successful LLM"
// case; we detect that by checking Category == "" and skip storing
// the reason JSON.
func (a *runtimePMAgent) recordDecisionSource(fundID, source string, reason errorclass.Detail) {
	if a == nil || strings.TrimSpace(fundID) == "" || strings.TrimSpace(source) == "" {
		return
	}
	rec := decisionSourceRecord{Source: source}
	if reason.Category != "" {
		if blob, err := json.Marshal(reason); err == nil {
			rec.ReasonJSON = blob
		}
	}
	a.lastDecisionSourceByFund.Store(fundID, rec)
	// Sprint 11.4 — emit the metrics event at the same call
	// site. We tag category/provider only when present so the
	// cardinality stays bounded. The observer is nil-safe.
	if a.decisionSourceObserver != nil {
		a.decisionSourceObserver.ObservePMDecisionSource(source, string(reason.Category), reason.Provider)
	}
}

// AttachDecisionSourceObserver lets the wiring layer plug a metrics
// sink onto an existing runtimePMAgent. Tests can pass nil to confirm
// the recorder degrades to a no-op without panicking. Concurrency:
// called exactly once at startup (single goroutine) before any PM
// tick fires, so no synchronisation is needed.
func (a *runtimePMAgent) AttachDecisionSourceObserver(observer pmDecisionSourceObserver) {
	if a == nil {
		return
	}
	a.decisionSourceObserver = observer
}

// pmDecisionSourceObserverFromMetrics wraps an arbitrary
// *serverMetrics-shaped value into the pmDecisionSourceObserver
// interface. The indirection lets us pass a nil *serverMetrics
// without panicking inside the constructor — the typed nil would
// otherwise survive the interface conversion and crash on the first
// call.
func pmDecisionSourceObserverFromMetrics(m *serverMetrics) pmDecisionSourceObserver {
	if m == nil {
		return nil
	}
	return m
}

// consumeDecisionSource is GeneratePlan's load-and-delete counterpart
// to recordDecisionSource. Returns ok=false when no record was stored
// (e.g. tests that drive buildPlanActions directly or legacy code
// paths that skip the new bookkeeping); GeneratePlan in that case
// falls through without calling SetDecisionSource and the row keeps
// the SQL default 'legacy' tag.
func (a *runtimePMAgent) consumeDecisionSource(fundID string) (decisionSourceRecord, bool) {
	if a == nil || strings.TrimSpace(fundID) == "" {
		return decisionSourceRecord{}, false
	}
	v, ok := a.lastDecisionSourceByFund.LoadAndDelete(fundID)
	if !ok {
		return decisionSourceRecord{}, false
	}
	rec, ok := v.(decisionSourceRecord)
	return rec, ok
}

// buildPlanActionsLegacy is the pre-Phase-2A deterministic plan
// generator extracted into its own function so the LLM-driven path
// can short-circuit it without losing the safety net. With holdings
// it proposes a reduce on the first sellable-today position; without
// holdings it buys the first universe symbol (or watches if
// quote/lot-size constraints fail).
//
// On top of those base actions the function ALSO emits a
// non-executing "watch" action for every other universe / team-
// research symbol the plan didn't otherwise touch. This makes the
// remaining stocks visible in the Decision Center / plan UI even
// when the LLM engine fails — operators were previously seeing
// only the single first-universe-symbol action and assuming the
// system had forgotten about their other researched tickers.
// Watch actions don't execute and don't change the risk surface,
// so this is purely an observability fix.
func (a *runtimePMAgent) buildPlanActionsLegacy(ctx context.Context, fund *repository.Fund, pmAgent *repository.Agent, positions []repository.HoldingPosition, boughtTodayByKey map[string]float64, roundtable *workflow.RoundtableResult, tradingDate time.Time, fundID string) ([]repository.PlanAction, error) {
	// Compute the team-coverage symbol list once so both branches
	// (with/without positions) can call appendUniverseWatchActions
	// without re-querying fund_team_members / agents. Fetching here
	// also keeps the side-effect at a predictable place for tests
	// that mock the underlying queries.
	universeCandidates := a.workflowSymbolCandidates(ctx, fundID, pmAgent)
	if len(positions) > 0 {
		actions := make([]repository.PlanAction, 0, len(positions))
		for i := range positions {
			position := positions[i]
			hint := instrument2.Hint{
				Market:     position.Market.String,
				Exchange:   position.Exchange.String,
				AssetClass: position.AssetClass.String,
			}
			posKey := positionMapKey(position.InstrumentKey, position.Symbol)
			boughtToday := boughtTodayByKey[posKey]
			sellableToday := instrument2.SellableQtyToday(position.Symbol, hint, position.Quantity, boughtToday)
			lockedToday := position.Quantity - sellableToday
			if lockedToday < 0 {
				lockedToday = 0
			}

			// Pick action type. Only the first position is a candidate
			// for "reduce" today (legacy heuristic). If T+1 has locked
			// the entire holding (sellableToday == 0), demote to hold
			// — proposing a reduce with qty=0 would just confuse the
			// user.
			actionType := "hold"
			if i == 0 && sellableToday > 0 {
				actionType = "reduce"
			}
			price := fallbackPositive(position.CurrentPrice, position.CostPrice)
			if quote, err := a.quoteForAction(ctx, planActionInstrumentRef(fund, position.Symbol, position.InstrumentKey, position.Market.String, position.Exchange.String, position.AssetClass.String, position.InstrumentType.String, position.QuoteCurrency.String, position.SettlementCurrency.String, contractMultiplierValue(position.ContractMultiplier), formatNullTime(position.ExpiryDate))); err == nil && quote != nil && quote.Price > 0 {
				price = quote.Price
			}
			instrumentKey := firstNonEmptyValue(position.InstrumentKey, position.Symbol)

			// qty/amount for the action: 0 for hold, sellableToday for
			// reduce. We never propose selling locked lots from this
			// path; the next plan (post-Settle) will be free to do so.
			var qtyVal, amountVal float64
			if actionType == "reduce" {
				qtyVal = sellableToday
				amountVal = roundCurrency(sellableToday * price)
			}

			reasoning := selectConsensus(roundtable, i, "existing holding rebalance")
			if lockedToday > 0 {
				// T+1 is an A-share *market* rule (uniform across
				// SH/SZ main, ChiNext, STAR, BSE), not a per-symbol
				// property. Phrase the note so reviewers don't read
				// it as a stock-specific quirk.
				reasoning = strings.TrimSpace(reasoning) + fmt.Sprintf(
					" | A股市场 T+1 结算规则：今日新买 %.0f 股需待下一交易日方可卖出，本次减仓只涉及已结算的 %.0f 股",
					lockedToday, sellableToday,
				)
			}

			actions = append(actions, repository.PlanAction{
				InstrumentKey:      instrumentKey,
				Symbol:             position.Symbol,
				Market:             position.Market,
				Exchange:           position.Exchange,
				AssetClass:         position.AssetClass,
				InstrumentType:     position.InstrumentType,
				Action:             actionType,
				PositionSide:       position.PositionSide,
				Quantity:           sql.NullFloat64{Float64: qtyVal, Valid: qtyVal > 0},
				Price:              sql.NullFloat64{Float64: price, Valid: price > 0},
				Amount:             sql.NullFloat64{Float64: amountVal, Valid: amountVal > 0},
				Reasoning:          sql.NullString{String: reasoning, Valid: reasoning != ""},
				Confidence:         sql.NullFloat64{Float64: 0.72, Valid: true},
				SupportedBy:        []string{"roundtable"},
				ExecutionStatus:    "pending",
				SortOrder:          i,
				QuoteCurrency:      position.QuoteCurrency,
				SettlementCurrency: position.SettlementCurrency,
				MarginMode:         position.MarginMode,
				Leverage:           position.Leverage,
				ContractMultiplier: position.ContractMultiplier,
				ExpiryDate:         position.ExpiryDate,
			})
		}
		// Make the universe / team-research symbols that weren't
		// touched by the position loop visible as watch actions, so
		// the Decision Center reflects EVERY symbol the team covers
		// rather than only the held subset.
		actions = a.appendUniverseWatchActions(ctx, actions, fund, universeCandidates, roundtable)
		return actions, nil
	}
	buyAmount := planBuyAmountWithinRiskCap(fund)
	symbolCandidates := universeCandidates
	symbol, symbolSource := inferWorkflowBuySymbol(fund, symbolCandidates)
	if symbol == "" {
		reasoning := appendSkillContext(selectConsensus(roundtable, 0, "roundtable consensus"), "structured ticker configuration is missing; add universe symbols or specialization instruments before automatic buy actions")
		return a.appendUniverseWatchActions(ctx, []repository.PlanAction{{
			InstrumentKey:   firstNonEmptyValue(fundID, "workflow-watch"),
			Action:          "watch",
			Reasoning:       sql.NullString{String: reasoning, Valid: true},
			Confidence:      sql.NullFloat64{Float64: 0.55, Valid: true},
			SupportedBy:     []string{"roundtable"},
			ExecutionStatus: "pending",
			SortOrder:       0,
		}}, fund, universeCandidates, roundtable), nil
	}
	slog.Info("pm generate plan", "fundId", strings.TrimSpace(fundID), "roundtableConsensus", roundtable.Consensus, "symbolCandidates", symbolCandidates, "selectedSymbol", symbol, "symbolSource", symbolSource, "buyAmount", buyAmount)
	instrument := defaultInstrumentRef(fund, workflow.FocusStock, symbol)
	quote, err := a.quoteForAction(ctx, instrument)
	if err != nil || quote == nil || quote.Price <= 0 {
		// quote unavailable → DOWNGRADE to watch, never fake a buy.
		// The previous behaviour wrote planBuyAmount into Price with
		// Quantity=1, which the broker simulator happily honoured as a
		// limit order — that's how the 96,226.42 CNY/share 301308 fill
		// on 2026-06-02 happened (budget got stamped as per-share price).
		// A production-grade trading system NEVER converts a missing
		// quote into an executable order: it must defer until a
		// reference price is available. We surface the missing quote
		// reason + the budget that was on the table so the next PM run
		// has full context.
		errSummary := "unknown error"
		if err != nil {
			errSummary = err.Error()
		}
		reasoning := appendSkillContext(
			selectConsensus(roundtable, 0, "roundtable consensus"),
			fmt.Sprintf("quote unavailable for %s (%s); downgraded to watch — budget on the table was %.4f, awaiting reference price before any order", symbol, errSummary, buyAmount),
		)
		return a.appendUniverseWatchActions(ctx, []repository.PlanAction{{
			InstrumentKey:      firstNonEmptyValue(instrument.InstrumentKey, buildInstrumentKey(instrument.Exchange, symbol), symbol),
			Symbol:             symbol,
			Market:             nullString(instrument.Market),
			Exchange:           nullString(instrument.Exchange),
			AssetClass:         nullString(instrument.AssetClass),
			InstrumentType:     nullString(instrument.InstrumentType),
			Action:             "watch",
			Reasoning:          sql.NullString{String: reasoning, Valid: true},
			Confidence:         sql.NullFloat64{Float64: 0.55, Valid: true},
			SupportedBy:        []string{"roundtable"},
			ExecutionStatus:    "pending",
			SortOrder:          0,
			QuoteCurrency:      nullString(instrument.QuoteCurrency),
			SettlementCurrency: nullString(instrument.SettlementCurrency),
			ContractMultiplier: sql.NullFloat64{Float64: instrument.ContractMultiplier, Valid: instrument.ContractMultiplier > 0},
		}}, fund, universeCandidates, roundtable), nil
	}
	rawQuantity := math.Floor(buyAmount / quote.Price)
	hint := instrument2.Hint{
		Market:     firstNonEmptyValue(quote.Market, instrument.Market),
		Exchange:   firstNonEmptyValue(quote.Exchange, instrument.Exchange),
		AssetClass: firstNonEmptyValue(quote.AssetClass, instrument.AssetClass),
	}
	quantity := instrument2.NormalizeBuyQty(symbol, hint, rawQuantity)
	// Non-A-share assets retain the legacy "at least 1 share" behaviour;
	// A-share boards return 0 when buyAmount cannot cover the minimum
	// lot, which the workflow surfaces upstream as a watch action.
	if !instrument2.SpecFor(instrument2.Classify(symbol, hint)).IsAShare() {
		quantity = math.Max(1, rawQuantity)
	}
	if quantity <= 0 {
		spec := instrument2.SpecFor(instrument2.Classify(symbol, hint))
		reasoning := appendSkillContext(
			selectConsensus(roundtable, 0, "roundtable consensus"),
			fmt.Sprintf("buy budget below A-share lot minimum (%d shares) for %s — switched to watch", spec.MinLot, symbol),
		)
		return a.appendUniverseWatchActions(ctx, []repository.PlanAction{{
			InstrumentKey:      firstNonEmptyValue(quote.InstrumentKey, instrument.InstrumentKey, buildInstrumentKey(quote.Exchange, symbol), symbol),
			Symbol:             symbol,
			Market:             nullString(firstNonEmptyValue(quote.Market, instrument.Market)),
			Exchange:           nullString(firstNonEmptyValue(quote.Exchange, instrument.Exchange)),
			AssetClass:         nullString(firstNonEmptyValue(quote.AssetClass, instrument.AssetClass)),
			InstrumentType:     nullString(instrument.InstrumentType),
			Action:             "watch",
			Price:              sql.NullFloat64{Float64: quote.Price, Valid: true},
			Reasoning:          sql.NullString{String: reasoning, Valid: true},
			Confidence:         sql.NullFloat64{Float64: 0.55, Valid: true},
			SupportedBy:        []string{"roundtable", "marketdata"},
			ExecutionStatus:    "pending",
			SortOrder:          0,
			QuoteCurrency:      nullString(firstNonEmptyValue(quote.QuoteCurrency, instrument.QuoteCurrency)),
			SettlementCurrency: nullString(instrument.SettlementCurrency),
			ContractMultiplier: sql.NullFloat64{Float64: instrument.ContractMultiplier, Valid: instrument.ContractMultiplier > 0},
		}}, fund, universeCandidates, roundtable), nil
	}
	return a.appendUniverseWatchActions(ctx, []repository.PlanAction{{
		InstrumentKey:      firstNonEmptyValue(quote.InstrumentKey, instrument.InstrumentKey, buildInstrumentKey(quote.Exchange, symbol), symbol),
		Symbol:             symbol,
		Market:             nullString(firstNonEmptyValue(quote.Market, instrument.Market)),
		Exchange:           nullString(firstNonEmptyValue(quote.Exchange, instrument.Exchange)),
		AssetClass:         nullString(firstNonEmptyValue(quote.AssetClass, instrument.AssetClass)),
		InstrumentType:     nullString(instrument.InstrumentType),
		Action:             "buy",
		Quantity:           sql.NullFloat64{Float64: quantity, Valid: true},
		Price:              sql.NullFloat64{Float64: quote.Price, Valid: true},
		Amount:             sql.NullFloat64{Float64: roundCurrency(quantity * quote.Price), Valid: true},
		Reasoning:          sql.NullString{String: appendQuoteReference(LanguageFromContext(ctx), selectConsensus(roundtable, 0, "roundtable consensus"), quote), Valid: true},
		Confidence:         sql.NullFloat64{Float64: 0.74, Valid: true},
		SupportedBy:        []string{"roundtable", "marketdata"},
		ExecutionStatus:    "pending",
		SortOrder:          0,
		QuoteCurrency:      nullString(firstNonEmptyValue(quote.QuoteCurrency, instrument.QuoteCurrency)),
		SettlementCurrency: nullString(instrument.SettlementCurrency),
		ContractMultiplier: sql.NullFloat64{Float64: instrument.ContractMultiplier, Valid: instrument.ContractMultiplier > 0},
	}}, fund, universeCandidates, roundtable), nil
}

// appendUniverseWatchActions augments a deterministic-fallback plan
// with non-executing "watch" actions for every fund-universe and
// team-research symbol the plan didn't already touch.
//
// Why this exists: the legacy fallback heuristic produces actions
// for at most ONE new symbol (the first universe entry) when there
// are no positions, and only for already-held symbols when there
// are. Operators with multiple universe symbols / multiple
// researchers were therefore seeing only one symbol per fallback
// plan in the Decision Center and reasonably concluded that the
// other tickers had been silently dropped from the workflow.
//
// Watch actions don't execute and don't change the risk surface;
// they exist only as observability so the UI reflects EVERY symbol
// the team covers. Existing actions are returned unchanged when
// the fund has no universe / candidates beyond what's already
// covered.
//
// The cap of 16 extra watch actions is a soft safety to keep plans
// readable even when a fund declares a very large universe; the
// LLM-driven path doesn't go through here so a 200-symbol universe
// won't blow up the plan size in steady state.
func (a *runtimePMAgent) appendUniverseWatchActions(ctx context.Context, actions []repository.PlanAction, fund *repository.Fund, teamCandidates []string, roundtable *workflow.RoundtableResult) []repository.PlanAction {
	if a == nil {
		return actions
	}
	covered := make(map[string]struct{}, len(actions))
	for _, act := range actions {
		key := strings.ToUpper(strings.TrimSpace(act.Symbol))
		if key == "" {
			continue
		}
		covered[key] = struct{}{}
	}

	profile := fundMarketProfile{}
	if fund != nil {
		profile = decodeFundMarketProfile(fund.Config)
	}

	// Build a whitelist from the fund universe — operators put the
	// authoritative ticker list there. normalizedWorkflowSymbol passes
	// any short uppercase string through, so theme strings like
	// "DRAM" / "NAND" / "HBM" that a researcher puts in their
	// specialization.themes also satisfy it; gating on the fund
	// universe filters that noise out without changing data the
	// operator hasn't explicitly configured.
	universeSet := make(map[string]struct{}, 8)
	if profile.Universe != nil {
		for _, sym := range profile.Universe.Symbols {
			if key := normalizedWorkflowSymbol(sym); key != "" {
				universeSet[key] = struct{}{}
			}
		}
	}

	ordered := make([]string, 0, 8)
	seenSource := make(map[string]struct{}, 8)
	addCandidate := func(raw string, requireUniverse bool) {
		trimmed := normalizedWorkflowSymbol(raw)
		if trimmed == "" {
			return
		}
		if requireUniverse {
			if _, ok := universeSet[trimmed]; !ok {
				return
			}
		}
		if _, ok := covered[trimmed]; ok {
			return
		}
		if _, ok := seenSource[trimmed]; ok {
			return
		}
		seenSource[trimmed] = struct{}{}
		ordered = append(ordered, trimmed)
	}
	if profile.Universe != nil {
		for _, sym := range profile.Universe.Symbols {
			addCandidate(sym, false)
		}
	}
	// Team candidates only contribute when they're already in the
	// fund universe; this keeps theme acronyms from being rendered
	// as fake watch tickers, but still lets a researcher whose
	// instrument list IS authoritative (e.g. for funds without an
	// explicit universe) show through — when universeSet is empty
	// we skip the gate so behaviour matches the pre-universe-aware
	// fallback.
	requireGate := len(universeSet) > 0
	for _, sym := range teamCandidates {
		addCandidate(sym, requireGate)
	}
	if len(ordered) == 0 {
		return actions
	}

	const maxWatch = 16
	if len(ordered) > maxWatch {
		ordered = ordered[:maxWatch]
	}

	sortOffset := len(actions)
	zh := LanguageFromContext(ctx) == UserLanguageZH
	reasoning := "team coverage symbol not actioned this slot"
	if zh {
		reasoning = "本次未对该团队覆盖标的下单，仅纳入观察"
	}
	reasoning = appendSkillContext(selectConsensus(roundtable, 0, reasoning), reasoning)

	for i, sym := range ordered {
		instrument := defaultInstrumentRef(fund, workflow.FocusStock, sym)
		instrumentKey := firstNonEmptyValue(instrument.InstrumentKey, buildInstrumentKey(instrument.Exchange, sym), sym)
		actions = append(actions, repository.PlanAction{
			InstrumentKey:      instrumentKey,
			Symbol:             sym,
			Market:             nullString(instrument.Market),
			Exchange:           nullString(instrument.Exchange),
			AssetClass:         nullString(instrument.AssetClass),
			InstrumentType:     nullString(instrument.InstrumentType),
			Action:             "watch",
			Reasoning:          sql.NullString{String: reasoning, Valid: reasoning != ""},
			Confidence:         sql.NullFloat64{Float64: 0.5, Valid: true},
			SupportedBy:        []string{"team_coverage"},
			ExecutionStatus:    "pending",
			SortOrder:          sortOffset + i,
			QuoteCurrency:      nullString(instrument.QuoteCurrency),
			SettlementCurrency: nullString(instrument.SettlementCurrency),
			ContractMultiplier: sql.NullFloat64{Float64: instrument.ContractMultiplier, Valid: instrument.ContractMultiplier > 0},
		})
	}
	return actions
}

// buildLLMDecisionEngine wires a decision.LLMDecisionEngine when the
// llmRuntime is available, or returns nil so the runtimePMAgent
// transparently falls back to its deterministic legacy heuristic.
// Wiring is per-fund so accounting / quota / observability are
// attributed correctly (StepName "pm_decision", FundID stamped on
// every usage record).
func buildLLMDecisionEngine(runtime *llmRuntime, fundID string) decision.DecisionEngine {
	if runtime == nil {
		return nil
	}
	client := runtime.LLMClient()
	if client == nil {
		return nil
	}
	inner := &decision.LLMDecisionEngine{
		Client:    client,
		ModelTier: llm.TierCritical,
		StepName:  "pm_decision",
		FundID:    fundID,
	}
	// Sprint 9.4 — opt-in three-stage pipeline. When the env flag
	// is set, wrap the legacy single-shot engine with
	// Trader.Propose → Risk.Assess → PM.FinalApprove. The wrapper
	// satisfies the same DecisionEngine interface so downstream
	// callers (workflowRuntime, fallback gates) need no changes.
	// The trader + risk stages route to TierStandard by default —
	// the PM final stage stays on TierCritical via the inner
	// engine — to keep cost amortised. Operators can override
	// with PM_THREE_STAGE_PROPOSAL_TIER / _ASSESSMENT_TIER if
	// they want a different cost/quality tradeoff.
	if envFlagEnabled("PM_THREE_STAGE_DECISION") {
		return &decision.ThreeStageEngine{
			Inner:          inner,
			Client:         client,
			ProposalTier:   llmTierFromEnv("PM_THREE_STAGE_PROPOSAL_TIER", llm.TierStandard),
			AssessmentTier: llmTierFromEnv("PM_THREE_STAGE_ASSESSMENT_TIER", llm.TierStandard),
			StepName:       "pm_decision",
			FundID:         fundID,
			StageTimeout:   60 * time.Second,
		}
	}
	return inner
}

// llmTierFromEnv reads an env var and returns the requested LLM
// tier. Unset / unknown values fall back to the provided default
// so a typo doesn't silently route to the wrong cost bucket.
func llmTierFromEnv(name string, fallback llm.ModelTier) llm.ModelTier {
	v := strings.TrimSpace(os.Getenv(name))
	switch strings.ToLower(v) {
	case "":
		return fallback
	case "simple", "fast":
		return llm.TierSimple
	case "standard":
		return llm.TierStandard
	case "critical":
		return llm.TierCritical
	default:
		return fallback
	}
}

// buildDebateRoundtable wires the Phase 2B bull/bear/quant
// roundtable. Returns nil when no LLM client is configured so the
// runtime quietly falls back to the legacy text-concat consensus
// (the gate in shouldRunDebate also short-circuits on nil).
//
// The same llm.LLMClient is shared across all three personas; the
// per-role differentiation is in the system prompt baked into
// debate.LLMResearcher. Each researcher consumes the same fundID
// so usage records and rate limits attribute correctly.
func buildDebateRoundtable(runtime *llmRuntime, fundID string, ownerUserID string, researcherAgentID string) debate.Roundtable {
	if runtime == nil {
		return nil
	}
	client := runtime.LLMClient()
	if client == nil {
		return nil
	}
	// All three personas (bull / bear / quant) share the same
	// AgentID + UserID — they are rhetorical positions inside the
	// debate orchestrator, not separate rows in the agents table.
	// Using the fund's primary researcher AgentID lets the router
	// honour any model the operator picked on that researcher
	// (router.agentDefaults[researcherAgentID]). When no researcher
	// exists or has no explicit model, the router falls through to
	// userDefaults[standard] and finally the .env default — same
	// fallback chain as the PM agent.
	user := strings.TrimSpace(ownerUserID)
	agent := strings.TrimSpace(researcherAgentID)
	return &debate.LLMRoundtable{
		Researchers: []debate.Researcher{
			&debate.LLMResearcher{PersonaRole: debate.RoleBull, Client: client, ModelTier: llm.TierStandard, FundID: fundID, StepName: "debate", UserID: user, AgentID: agent},
			&debate.LLMResearcher{PersonaRole: debate.RoleBear, Client: client, ModelTier: llm.TierStandard, FundID: fundID, StepName: "debate", UserID: user, AgentID: agent},
			&debate.LLMResearcher{PersonaRole: debate.RoleQuant, Client: client, ModelTier: llm.TierStandard, FundID: fundID, StepName: "debate", UserID: user, AgentID: agent},
		},
	}
}

// resolveFundOperatorRouting looks up the fund's operator UserID and
// the AgentID of its primary researcher so per-step LLM calls
// (sentiment / debate) carry routing hints the model router can match
// against agentDefaults + userDefaults. Returns blank strings when
// the team is empty or repo lookups fail — callers fall through to
// the platform .env default, which is the safe behaviour.
//
// Two SQL roundtrips: ListByFund (single member array) plus one
// GetByID per researcher hit until a non-empty UserID is found. The
// runtime is cached per (fundID, tradingDate) so this fires at most
// once per fund per trading day.
func resolveFundOperatorRouting(ctx context.Context, fundID string, teamRepo *repository.TeamRepo, agentRepo *repository.AgentRepo) (ownerUserID string, researcherAgentID string) {
	if teamRepo == nil || agentRepo == nil || strings.TrimSpace(fundID) == "" {
		return "", ""
	}
	members, err := teamRepo.ListByFund(ctx, fundID)
	if err != nil || len(members) == 0 {
		return "", ""
	}
	// Two-pass scan: prefer a researcher (its model preference is
	// what debate + sentiment should track), but on a team without
	// any researcher we still grab the first member's UserID so
	// userDefaults[simple/standard] routing kicks in for sentiment.
	for _, m := range members {
		if strings.ToLower(strings.TrimSpace(m.Role)) != "researcher" {
			continue
		}
		agent, err := agentRepo.GetByID(ctx, m.AgentID)
		if err != nil || agent == nil {
			continue
		}
		if strings.TrimSpace(agent.UserID) == "" {
			continue
		}
		return agent.UserID, agent.ID
	}
	for _, m := range members {
		agent, err := agentRepo.GetByID(ctx, m.AgentID)
		if err != nil || agent == nil {
			continue
		}
		if strings.TrimSpace(agent.UserID) == "" {
			continue
		}
		return agent.UserID, ""
	}
	return "", ""
}

// debateForceEnabledFromEnv reads FUND_DEBATE_ROUNDTABLE to flip the
// debate path on fleet-wide for canary / staging. Anything that
// parses as truthy ("1", "true", "yes", case-insensitive) flips it
// on. Empty / "0" / anything else keeps the per-fund opt-in via
// fund.config.researchTier.
func debateForceEnabledFromEnv() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("FUND_DEBATE_ROUNDTABLE")))
	switch value {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// runDecisionEngine drives the LLM-backed Phase 2A decision pipeline.
// It assembles a decision.DecisionInput from the loaded fund /
// positions / roundtable, invokes the engine, then translates the
// structured output back to []repository.PlanAction while still
// enforcing the same lot-size, sellable-today and T+1 safety
// normalisations the legacy path applies. Caller is expected to fall
// through to buildPlanActionsLegacy on any non-nil error.
func (a *runtimePMAgent) runDecisionEngine(ctx context.Context, fund *repository.Fund, pmAgent *repository.Agent, positions []repository.HoldingPosition, boughtTodayByKey map[string]float64, roundtable *workflow.RoundtableResult, tradingDate time.Time, fundID string) ([]repository.PlanAction, float64, error) {
	if a.decisionEngine == nil {
		return nil, 0, fmt.Errorf("decision engine not configured")
	}
	input := a.buildDecisionInput(ctx, fund, pmAgent, positions, boughtTodayByKey, roundtable, tradingDate, fundID)
	// P2 observability: a single line per PM decision call that lets
	// operators confirm the agent the router resolved against. Kept
	// permanent (not behind a debug flag) because the cost is one log
	// per workflow run and the alternative is reproducing the silent
	// "PM set to claude but ran on gemini" symptom from production.
	if pmAgent != nil {
		slog.Info("pm decision routing",
			"fundId", fundID,
			"userId", input.UserID,
			"pmAgentId", input.PMAgentID,
			"agentProvider", pmAgent.ModelProvider.String,
			"agentModel", pmAgent.ModelName.String,
		)
	}
	output, err := a.decisionEngine.Decide(ctx, input)
	if err != nil {
		return nil, 0, err
	}
	if output == nil || len(output.Actions) == 0 {
		return nil, 0, fmt.Errorf("decision engine returned empty plan")
	}
	actions := a.translateDecisionActions(ctx, fund, positions, boughtTodayByKey, roundtable, output, fundID)
	if len(actions) == 0 {
		return nil, 0, fmt.Errorf("decision engine output produced zero actionable PlanActions")
	}
	return actions, output.Confidence, nil
}

// buildDecisionInput projects the wiring layer's runtime state down to
// the engine-facing DecisionInput shape. The engine sees:
//
//   - Market context (market tag, base currency, primary direction).
//   - The full position list with AvailableQty already accounting for
//     T+1 / intraday-buy locks so the engine doesn't propose
//     unsellable lots.
//   - The configured universe + per-symbol instrument hints so a
//     first-time buy doesn't require re-classifying the symbol.
//   - Roundtable consensus bullets so the engine can weigh them
//     against macro / stock / fundamental reports.
//   - BuyBudget = the per-plan hard cap (NAV * risk-cap) so the engine
//     knows how much notional it's allowed to allocate even before
//     the auto-execute gate checks.
func (a *runtimePMAgent) buildDecisionInput(ctx context.Context, fund *repository.Fund, pmAgent *repository.Agent, positions []repository.HoldingPosition, boughtTodayByKey map[string]float64, roundtable *workflow.RoundtableResult, tradingDate time.Time, fundID string) decision.DecisionInput {
	profile := fundMarketProfile{}
	if fund != nil {
		profile = decodeFundMarketProfile(fund.Config)
	}

	decisionPositions := make([]decision.DecisionPosition, 0, len(positions))
	instrumentHints := make(map[string]decision.InstrumentHint, len(positions))
	for _, p := range positions {
		hint := instrument2.Hint{
			Market:     p.Market.String,
			Exchange:   p.Exchange.String,
			AssetClass: p.AssetClass.String,
		}
		posKey := positionMapKey(p.InstrumentKey, p.Symbol)
		sellableToday := instrument2.SellableQtyToday(p.Symbol, hint, p.Quantity, boughtTodayByKey[posKey])
		decisionPositions = append(decisionPositions, decision.DecisionPosition{
			Symbol:        p.Symbol,
			InstrumentKey: p.InstrumentKey,
			Market:        p.Market.String,
			Exchange:      p.Exchange.String,
			AssetClass:    p.AssetClass.String,
			Quantity:      p.Quantity,
			AvailableQty:  sellableToday,
			CurrentPrice:  p.CurrentPrice,
			CostPrice:     p.CostPrice,
			UnrealizedPnL: p.UnrealizedPnL.Float64,
		})
		if key := strings.ToUpper(strings.TrimSpace(p.Symbol)); key != "" {
			instrumentHints[key] = decision.InstrumentHint{
				Market:         p.Market.String,
				Exchange:       p.Exchange.String,
				AssetClass:     p.AssetClass.String,
				InstrumentType: p.InstrumentType.String,
				QuoteCurrency:  p.QuoteCurrency.String,
			}
		}
	}

	universe := a.workflowSymbolCandidates(ctx, fundID, pmAgent)
	for _, sym := range universe {
		key := strings.ToUpper(strings.TrimSpace(sym))
		if key == "" {
			continue
		}
		if _, ok := instrumentHints[key]; ok {
			continue
		}
		instrumentHints[key] = decision.InstrumentHint{
			Market:        profile.Market,
			Exchange:      profile.Exchange,
			AssetClass:    profile.AssetClass,
			QuoteCurrency: profile.BaseCurrency,
		}
	}

	var consensus []string
	var roundtableStance, bullCase, bearCase, quantCase string
	var symbolVerdicts []decision.RoundtableSymbolVerdict
	var fundamentalSummary, sectorRotation, newsSentiment, macroBriefing string
	if roundtable != nil {
		consensus = append(consensus, roundtable.Consensus...)
		roundtableStance = roundtable.OverallStance
		bullCase = roundtable.BullCase
		bearCase = roundtable.BearCase
		quantCase = roundtable.QuantCase
		fundamentalSummary = roundtable.FundamentalSummary
		sectorRotation = roundtable.SectorRotation
		newsSentiment = roundtable.NewsSentiment
		// Sprint 1 / S2: pick up the macro brief the orchestrator
		// stitched onto the roundtable after StepMacroBrief. The PM
		// prompt has consumed this slot since Phase 1; it was just
		// never being populated.
		macroBriefing = strings.TrimSpace(roundtable.MacroBriefing)
		if len(roundtable.Symbols) > 0 {
			symbolVerdicts = make([]decision.RoundtableSymbolVerdict, 0, len(roundtable.Symbols))
			for _, sd := range roundtable.Symbols {
				symbolVerdicts = append(symbolVerdicts, decision.RoundtableSymbolVerdict{
					Symbol:       sd.Symbol,
					Verdict:      sd.Verdict,
					BullCase:     sd.BullCase,
					BearCase:     sd.BearCase,
					QuantCase:    sd.QuantCase,
					DissentVotes: sd.DissentVotes,
				})
			}
		}
	}

	totalAssets := 0.0
	availableCash := 0.0
	if fund != nil {
		totalAssets = fund.TotalAssets
		availableCash = fund.CurrentCapital
	}

	// LLM routing hints: the PM agent and its owning user are what
	// llm.ModelRouter.ResolveModel needs to fire the per-agent or
	// per-user model override path. Without these, every PM LLM call
	// falls through to the platform default provider regardless of
	// what model the operator picked in the agent editor — that was
	// the P2 symptom: tong's PM agent set to claude but every plan
	// went to gemini. The owner is read from pmAgent.UserID (the
	// agent table column), not the fund company, because models are
	// configured per user, and the PM agent is what carries the
	// routing identity end-to-end.
	ownerUserID := ""
	pmAgentID := ""
	if pmAgent != nil {
		ownerUserID = strings.TrimSpace(pmAgent.UserID)
		pmAgentID = strings.TrimSpace(pmAgent.ID)
	}
	_ = fund // reserved for future fund-level routing overrides

	// Sprint C #2 / E #4: correlation matrix is built first so
	// its HighCorrPairs can feed the Sprint E #4 pair-spread
	// monitor below. The OHLC cache shared between the two
	// services makes the spread monitor's second pass cheap.
	correlations := a.buildCorrelations(ctx, profile.Market, universe, decisionPositions, profile.CorrelationPolicy)
	pairSpreads := a.buildPairSpreads(ctx, profile.Market, correlations)

	// Sprint 1 / S1: populate the three learning-loop blocks that
	// surface the agent learning system's state directly to the PM.
	// Each helper is soft-failing — if the repo lookup errors out or
	// returns nothing, the prompt simply omits the corresponding
	// optional JSON key (per omitempty in prompt.go).
	agentSkills := a.collectAgentSkillContexts(ctx, fundID)
	recentLessons := a.collectRecentLessonContexts(ctx, fundID, tradingDate)
	longTermReflections := a.collectLongTermReflectionContexts(ctx, fundID, tradingDate)

	// Sprint 10.1 — model A/B sticky key. We use (fund × trading
	// date) so a single daily run keeps all its LLM calls on the
	// same arm. If the orchestrator gains an explicit workflow
	// run id we can swap this for that, but until then this
	// gives identical stickiness because PM decisions run at
	// most once per fund per trading day in production.
	runIDForAB := fundID + ":" + tradingDate.Format("2006-01-02")

	input := decision.DecisionInput{
		FundID:              fundID,
		TradingDate:         tradingDate,
		RunID:               runIDForAB,
		Market:              profile.Market,
		BaseCurrency:        profile.BaseCurrency,
		PrimaryDirection:    profile.PrimaryDirection,
		Benchmark:           profile.BenchmarkSymbol,
		TotalAssets:         totalAssets,
		AvailableCash:       availableCash,
		Positions:           decisionPositions,
		Universe:            universe,
		InstrumentHints:     instrumentHints,
		RoundtableConsensus: consensus,
		RoundtableStance:    roundtableStance,
		BullCase:            bullCase,
		BearCase:            bearCase,
		QuantCase:           quantCase,
		SymbolVerdicts:      symbolVerdicts,
		FundamentalSummary:  fundamentalSummary,
		MacroBriefing:       macroBriefing,
		UserID:              ownerUserID,
		PMAgentID:           pmAgentID,
		SectorRotation:      sectorRotation,
		NewsSentiment:       newsSentiment,
		AgentSkills:         agentSkills,
		RecentLessons:       recentLessons,
		LongTermReflections: longTermReflections,
		SleeveScorecard:     a.buildSleeveScorecard(ctx, fundID),
		LessonReplay:        a.buildLessonReplay(ctx, fundID),
		AgentTrackRecord:    a.buildAgentTrackRecord(ctx, fundID),
		QuantSnapshots:      a.buildQuantSnapshots(ctx, profile.Market, universe, decisionPositions),
		IntradaySnapshots:   a.buildIntradaySnapshots(ctx, profile.Market, universe, decisionPositions, tradingDate),
		SemanticRecall:      a.buildSemanticRecall(ctx, fundID, macroBriefing, universe),
		UniverseRanking:     a.buildUniverseRanking(ctx, profile.Market, universe, decisionPositions),
		QualityScores:       a.buildQualityScores(ctx, profile.Market, universe, decisionPositions),
		ValueScores:         a.buildValueScores(ctx, profile.Market, universe, decisionPositions),
		LowBetaScores:       a.buildLowBetaScores(ctx, profile.Market, universe, decisionPositions),
		PEAD:                a.buildPEAD(ctx, profile.Market, universe, decisionPositions),
		Cooldowns:           a.buildCooldowns(ctx, fundID, universe, decisionPositions, tradingDate),
		RiskBudget:          a.buildRiskBudget(ctx, fundID, tradingDate),
		NewsCatalysts:       a.buildNewsCatalysts(ctx, profile.Market, universe, decisionPositions, tradingDate),
		EarningsCalendar:    a.buildEarningsCalendar(ctx, profile.Market, universe, decisionPositions),
		Exposure:            buildExposureSnapshot(totalAssets, availableCash, decisionPositions, instrumentHints, profile.ExposurePolicy),
		Correlations:        correlations,
		PairSpreads:         pairSpreads,
		BuyBudget:           planBuyAmountWithinRiskCap(fund),
		Now:                 tradingDate,
	}

	// Sprint 3 / L3: cross-agent contradiction check. Runs only
	// when bull/bear/quant cases are all populated (a true 3-way
	// roundtable produced them); single-researcher or stub paths
	// yield <2 views and the runner short-circuits to nil. Notes
	// land in RiskNotes so the PM prompt's risk_notes block
	// catches them — no prompt schema change needed.
	if a != nil && a.contradiction != nil {
		researcherViews := buildContradictionViews(bullCase, bearCase, quantCase, roundtableStance)
		if len(researcherViews) >= 2 {
			notes := a.contradiction.Check(ctx, fundID, tradingDate, universe, macroBriefing, "", researcherViews, ownerUserID, pmAgentID)
			if len(notes) > 0 {
				input.RiskNotes = append(input.RiskNotes, notes...)
			}
		}
	}

	// Sprint C #3 — decision-input fingerprint observability.
	// Emit a structured slog line on every PM call so the audit
	// trail can be grepped by signal presence ("which blocks
	// were live on the call that produced this plan?"). The
	// fingerprint is deterministic so the same input always
	// produces the same trace; safe to log unconditionally.
	// We also surface the present-blocks ribbon as a RiskNote so
	// it lands inside Plan.Reasoning for end-to-end audit.
	trace := decision.Fingerprint(input)
	slog.Info("decision_input_fingerprint", trace.SlogAttrs()...)
	if blocks := trace.PresentBlocks(); len(blocks) > 0 {
		input.RiskNotes = append(input.RiskNotes,
			"signal_blocks_present: "+strings.Join(blocks, ", "))
	}
	// G1 #2: stash the trace so the GeneratePlan path can
	// pair it with the final Reasoning text and persist the
	// per-plan attribution payload. The store overwrites any
	// earlier entry for the same fund, so the map stays small
	// (≤ N_funds rows live at once).
	if a != nil && strings.TrimSpace(fundID) != "" {
		a.lastTraceByFund.Store(fundID, trace)
	}

	// Sprint D #1 — Prometheus counters for signal-block presence,
	// exposure breach kinds, high-correlation pair count, cooldown
	// vetos, and dynamic risk-budget throttles. Per-fund cardinality
	// is intentionally absent: the slog line above carries fund_id
	// for drill-down, while these counters stay aggregated so the
	// time series count is bounded and dashboards scale.
	if a.serverMetrics != nil {
		var (
			breachKinds      = input.Exposure.BreachKinds()
			highCorrPairs    int
			cooldownSymbols  []string
			riskBudgetReason string
		)
		if input.Correlations != nil {
			highCorrPairs = len(input.Correlations.HighCorrPairs)
		}
		for _, c := range input.Cooldowns {
			if s := strings.TrimSpace(c.Symbol); s != "" {
				cooldownSymbols = append(cooldownSymbols, strings.ToUpper(s))
			}
		}
		if rb := input.RiskBudget; rb != nil {
			switch {
			case rb.DDScalar > 0 && rb.DDScalar < 1.0:
				riskBudgetReason = "drawdown_throttle"
			case rb.VolScalar > 0 && rb.VolScalar < 1.0:
				riskBudgetReason = "vol_target_throttle"
			case rb.VolScalar > 1.0:
				riskBudgetReason = "vol_target_boost"
			}
		}
		a.serverMetrics.ObserveDecisionInput(
			trace.PresentBlocks(),
			trace.AbsentBlocks(),
			breachKinds,
			highCorrPairs,
			cooldownSymbols,
			riskBudgetReason,
		)
	}

	return input
}

// collectAgentSkillContexts walks every agent on the fund's team and
// returns the approved + enabled skill cards as decision-engine
// AgentSkillContext rows. Sprint 1 / S1 wiring: the LLM PM gets to
// read its own playbook before producing actions, instead of after.
//
// Soft-failing: any repo error along the way returns an empty slice
// (the decision prompt then simply omits the agentSkills block per
// omitempty). We never want a skill lookup glitch to block a tick.
func (a *runtimePMAgent) collectAgentSkillContexts(ctx context.Context, fundID string) []decision.AgentSkillContext {
	if a == nil || strings.TrimSpace(fundID) == "" || a.teamRepo == nil || a.agentRepo == nil {
		return nil
	}
	members, err := a.teamRepo.ListByFund(ctx, fundID)
	if err != nil || len(members) == 0 {
		return nil
	}
	out := make([]decision.AgentSkillContext, 0, 8)
	seen := make(map[string]struct{}) // dedupe across team rows (one agent multiple roles)
	for i := range members {
		member := members[i]
		agent, err := a.agentRepo.GetByID(ctx, member.AgentID)
		if err != nil || agent == nil {
			continue
		}
		cfg := parseSkillConfig(agent.SkillConfig)
		// parsedSkillConfig.Enabled at the top level is the
		// "skill library globally enabled" master switch — when
		// false the agent ignores skills entirely.
		if !cfg.Enabled {
			continue
		}
		for _, skill := range cfg.Skills {
			if !skillEntryIsActive(skill) {
				continue
			}
			name := strings.TrimSpace(skill.Name)
			if name == "" {
				name = strings.TrimSpace(skill.Key)
			}
			if name == "" {
				continue
			}
			role := strings.ToLower(strings.TrimSpace(member.Role))
			dedupKey := role + "::" + strings.ToLower(name)
			if _, dup := seen[dedupKey]; dup {
				continue
			}
			seen[dedupKey] = struct{}{}
			// Prefer Description over Content for the prompt
			// (Content is the full skill body, often a long
			// paragraph; Description is the one-liner). Fall back
			// to Content's first 240 chars if Description is
			// empty.
			desc := strings.TrimSpace(skill.Description)
			if desc == "" {
				desc = strings.TrimSpace(skill.Content)
			}
			out = append(out, decision.AgentSkillContext{
				AgentRole:   role,
				AgentName:   strings.TrimSpace(agent.Name),
				Name:        name,
				Description: desc,
				Source:      strings.ToLower(strings.TrimSpace(skill.Source)),
			})
		}
	}
	return out
}

// collectRecentLessonContexts pulls the most recent agent-level +
// daily-level lesson memories for the fund. Sprint 1 / S1 wiring.
// We read 7 days of layer="agent" personalised lessons and 14 days of
// layer="daily" fund-wide summaries, sort by most-recent, and cap the
// total to 12 entries (the prompt-side cap will further trim if a
// future deployment loosens these numbers).
//
// Soft-failing: a repo error returns an empty slice; the decision
// prompt then omits the block.
func (a *runtimePMAgent) collectRecentLessonContexts(ctx context.Context, fundID string, tradingDate time.Time) []decision.RecentLessonContext {
	if a == nil || strings.TrimSpace(fundID) == "" || a.memoryRepo == nil {
		return nil
	}
	const (
		agentLookbackDays = 7
		dailyLookbackDays = 14
		hardCap           = 12
	)
	if tradingDate.IsZero() {
		tradingDate = time.Now().UTC()
	}
	cutoffAgent := tradingDate.AddDate(0, 0, -agentLookbackDays)
	cutoffDaily := tradingDate.AddDate(0, 0, -dailyLookbackDays)

	out := make([]decision.RecentLessonContext, 0, hardCap)
	agentMems, err := a.memoryRepo.ListByFund(ctx, fundID, "agent", 40)
	if err == nil {
		for _, m := range agentMems {
			when := lessonContextTradingDate(m)
			if when.Before(cutoffAgent) {
				continue
			}
			ctxLesson := decision.RecentLessonContext{
				TradingDate: when.Format("2006-01-02"),
				Layer:       "agent",
				AgentRole:   lessonContextAgentRole(ctx, a.agentRepo, m),
				Title:       strings.TrimSpace(m.Title.String),
				Content:     strings.TrimSpace(m.Content),
				Tags:        m.Tags,
			}
			// Sprint 3 / M1: surface "该 lesson 历史命中率" to the
			// PM prompt so it can deprioritise lessons whose past
			// predictions consistently failed. Soft-failing: a
			// lineage lookup error just leaves the rate at 0 and
			// the prompt renderer omits the marker.
			if a.db != nil {
				if rate, total, lerr := lessonHitRate(ctx, a.db, m.ID); lerr == nil && total > 0 {
					ctxLesson.HitRate = rate
					ctxLesson.SamplesObserved = total
				}
			}
			out = append(out, ctxLesson)
		}
	}
	dailyMems, err := a.memoryRepo.ListByFund(ctx, fundID, "daily", 30)
	if err == nil {
		for _, m := range dailyMems {
			when := lessonContextTradingDate(m)
			if when.Before(cutoffDaily) {
				continue
			}
			out = append(out, decision.RecentLessonContext{
				TradingDate: when.Format("2006-01-02"),
				Layer:       "daily",
				Title:       strings.TrimSpace(m.Title.String),
				Content:     strings.TrimSpace(m.Content),
				Tags:        m.Tags,
			})
		}
	}
	// Stable sort: newest trading_date first, then agent-layer
	// before daily so personalised lessons surface first when ties
	// exist on the same date.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TradingDate != out[j].TradingDate {
			return out[i].TradingDate > out[j].TradingDate
		}
		if out[i].Layer != out[j].Layer {
			return out[i].Layer == "agent"
		}
		return false
	})
	if len(out) > hardCap {
		out = out[:hardCap]
	}
	return out
}

// collectLongTermReflectionContexts surfaces at most 5 of the newest
// long-term reflection memories (layer="long_term") within the last
// 30 days. Sprint 1 / S1 wiring.
func (a *runtimePMAgent) collectLongTermReflectionContexts(ctx context.Context, fundID string, tradingDate time.Time) []decision.LongTermReflectionContext {
	if a == nil || strings.TrimSpace(fundID) == "" || a.memoryRepo == nil {
		return nil
	}
	const (
		lookbackDays = 30
		hardCap      = 5
	)
	if tradingDate.IsZero() {
		tradingDate = time.Now().UTC()
	}
	cutoff := tradingDate.AddDate(0, 0, -lookbackDays)
	mems, err := a.memoryRepo.ListByFund(ctx, fundID, "long_term", 20)
	if err != nil || len(mems) == 0 {
		return nil
	}
	out := make([]decision.LongTermReflectionContext, 0, hardCap)
	for _, m := range mems {
		when := m.CreatedAt
		if m.TradingDate.Valid {
			when = m.TradingDate.Time
		}
		if when.Before(cutoff) {
			continue
		}
		out = append(out, decision.LongTermReflectionContext{
			CreatedAt: when.UTC().Format("2006-01-02"),
			Title:     strings.TrimSpace(m.Title.String),
			Content:   strings.TrimSpace(m.Content),
			Tags:      m.Tags,
		})
		if len(out) >= hardCap {
			break
		}
	}
	return out
}

// lessonContextTradingDate picks the lesson's effective trading-day
// stamp: prefer the explicit memories.trading_date column when set,
// otherwise fall back to created_at. Both come from the same repo
// scan so no extra round-trip is needed.
func lessonContextTradingDate(m repository.Memory) time.Time {
	if m.TradingDate.Valid {
		return m.TradingDate.Time
	}
	return m.CreatedAt
}

// lessonContextAgentRole resolves the agent's role for a per-agent
// lesson memory. Returns "" when the memory lacks an agent_id or the
// lookup fails (memory is then rendered as fund-wide context).
func lessonContextAgentRole(ctx context.Context, agentRepo *repository.AgentRepo, m repository.Memory) string {
	if agentRepo == nil || !m.AgentID.Valid {
		return ""
	}
	agent, err := agentRepo.GetByID(ctx, m.AgentID.String)
	if err != nil || agent == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(agent.Role))
}

// buildCorrelations is the Sprint C #2 entry point. Composes the
// universe + held-positions sets into a deduped correlation
// request and asks the correlation service for the snapshot.
// Returns nil whenever the service / fetcher / sample is too thin
// (the prompt builder omits the block in that case).
//
// policy carries the per-fund overrides parsed from
// fund.config.correlationPolicy. nil = use the service's
// configured defaults (60-day lookback, 0.7 |rho| floor, 10 max
// pairs). ComputeWithOptions clamps any out-of-range override.
func (a *runtimePMAgent) buildCorrelations(ctx context.Context, market string, universe []string, positions []decision.DecisionPosition, policy *FundCorrelationPolicy) *decision.CorrelationSnapshot {
	if a == nil || a.correlationSvc == nil {
		return nil
	}
	requests := make([]correlation.SymbolRequest, 0, len(universe)+len(positions))
	for _, sym := range universe {
		s := strings.TrimSpace(sym)
		if s == "" {
			continue
		}
		requests = append(requests, correlation.SymbolRequest{
			Symbol: s,
			Market: market,
			Held:   false,
		})
	}
	for _, p := range positions {
		s := strings.TrimSpace(p.Symbol)
		if s == "" {
			continue
		}
		requests = append(requests, correlation.SymbolRequest{
			Symbol: s,
			Market: market,
			Held:   true,
		})
	}
	if len(requests) < 2 {
		return nil
	}
	snap := a.correlationSvc.ComputeWithOptions(ctx, requests, resolveCorrelationOptions(policy))
	if snap == nil {
		return nil
	}
	return snap
}

// buildPairSpreads assembles the Sprint E #4 rolling spread
// monitor from the correlation snapshot's HighCorrPairs. We
// only look at pairs the correlation matrix already flagged as
// "tight" (|rho| ≥ threshold) — spread divergence on a pair
// without a stable long-run relationship isn't a tradeable
// signal, just noise.
//
// Returns nil when:
//   - the pair-spread service isn't wired
//   - the correlation snapshot is nil / empty
//   - the snapshot carries no HighCorrPairs to monitor
//   - every spread computation drops out (insufficient OHLC bars)
//
// In any of those cases the prompt simply omits the block, and
// the PM falls back on the per-symbol blocks above.
func (a *runtimePMAgent) buildPairSpreads(ctx context.Context, market string, corr *decision.CorrelationSnapshot) *decision.PairSpreadSnapshot {
	if a == nil || a.pairSpreadSvc == nil {
		return nil
	}
	if corr == nil || len(corr.HighCorrPairs) == 0 {
		return nil
	}
	requests := make([]pairspread.PairRequest, 0, len(corr.HighCorrPairs))
	for _, p := range corr.HighCorrPairs {
		left := strings.TrimSpace(p.Left)
		right := strings.TrimSpace(p.Right)
		if left == "" || right == "" {
			continue
		}
		requests = append(requests, pairspread.PairRequest{
			Left:   left,
			Right:  right,
			Market: market,
			Rho:    p.Rho,
		})
	}
	if len(requests) == 0 {
		return nil
	}
	return a.pairSpreadSvc.Build(ctx, requests)
}

// buildExposureSnapshot reduces the wiring layer's view of the
// fund's NAV / cash / positions into the minimal Position slice
// the exposure package consumes. Sector is taken from the
// InstrumentHint AssetClass when present (cleanest available
// proxy on a multi-asset fund); falls back to the position's own
// Market field, then to "unclassified" when nothing else is
// known.
//
// This is intentionally pure / cheap so we can call it on every
// PM run even when totalAssets is zero — exposure.Compute itself
// degrades gracefully on the zero-NAV path.
//
// policy carries the per-fund overrides parsed from
// fund.config.exposurePolicy. nil = use the AQR / Bridgewater /
// Citadel defaults baked into resolveExposureOptions.
func buildExposureSnapshot(totalAssets, availableCash float64, positions []decision.DecisionPosition, hints map[string]decision.InstrumentHint, policy *FundExposurePolicy) decision.ExposureSnapshot {
	if totalAssets <= 0 {
		return decision.ExposureSnapshot{}
	}
	out := make([]exposure.Position, 0, len(positions))
	for _, p := range positions {
		mv := p.Quantity * p.CurrentPrice
		if mv <= 0 || strings.TrimSpace(p.Symbol) == "" {
			continue
		}
		sector := ""
		if h, ok := hints[strings.ToUpper(strings.TrimSpace(p.Symbol))]; ok {
			sector = strings.TrimSpace(h.AssetClass)
		}
		if sector == "" {
			sector = strings.TrimSpace(p.AssetClass)
		}
		if sector == "" {
			sector = strings.TrimSpace(p.Market)
		}
		out = append(out, exposure.Position{
			Symbol:      p.Symbol,
			Sector:      sector,
			MarketValue: mv,
		})
	}
	return exposure.Compute(resolveExposureOptions(policy), totalAssets, availableCash, out)
}

// resolveExposureOptions overlays the per-fund policy on top of
// the ship defaults. Nil-safe so callers that don't have a
// fund.config.exposurePolicy stanza fall through to the
// production defaults (25/50/60/5). Each pointer field is
// applied only when set; the resulting Options run through
// exposure's own withDefaults clamp so out-of-range values
// snap to safe bounds rather than break the breach math.
func resolveExposureOptions(policy *FundExposurePolicy) exposure.Options {
	// Ship defaults (AQR / Bridgewater / Citadel conventions).
	opts := exposure.Options{
		SingleNameCap: 0.25,
		SectorCap:     0.50,
		Top3Cap:       0.60,
		CashFloorPct:  0.05,
	}
	if policy == nil {
		return opts
	}
	if policy.SingleNameCapPct != nil {
		opts.SingleNameCap = *policy.SingleNameCapPct
	}
	if policy.SectorCapPct != nil {
		opts.SectorCap = *policy.SectorCapPct
	}
	if policy.Top3CapPct != nil {
		opts.Top3Cap = *policy.Top3CapPct
	}
	if policy.CashFloorPct != nil {
		opts.CashFloorPct = *policy.CashFloorPct
	}
	return opts
}

// resolveCorrelationOptions converts the per-fund policy into
// the correlation.Options shape ComputeWithOptions consumes.
// Same nil-safety convention as resolveExposureOptions: nil
// policy = empty Options (the service falls back to its own
// configured defaults inside mergeOptions/withDefaults).
func resolveCorrelationOptions(policy *FundCorrelationPolicy) correlation.Options {
	if policy == nil {
		return correlation.Options{}
	}
	opts := correlation.Options{}
	if policy.LookbackDays != nil {
		opts.LookbackBars = *policy.LookbackDays
	}
	if policy.HighCorrThreshold != nil {
		opts.HighCorrThreshold = *policy.HighCorrThreshold
	}
	if policy.MaxHighCorrPairs != nil {
		opts.MaxPairs = *policy.MaxHighCorrPairs
	}
	return opts
}

// buildNewsCatalysts assembles the Sprint B #3 per-symbol recent
// news catalyst list. Same universe ∪ positions candidate set the
// Sprint A / B #1 helpers use so the PM prompt has a coherent view
// across blocks. Returns nil when the service isn't wired, the
// fund's marketdata is offline, or no symbols carry recent items.
//
// All errors are swallowed inside newsrecall.BuildCatalysts (the
// service downgrades per-symbol failures to "no catalysts" for
// that symbol), so this wrapper only needs the dedupe + symbol-
// to-Request translation.
func (a *runtimePMAgent) buildNewsCatalysts(ctx context.Context, fundMarket string, universe []string, positions []decision.DecisionPosition, now time.Time) []decision.SymbolNewsCatalysts {
	if a == nil || a.newsCatalystSvc == nil {
		return nil
	}
	market := strings.ToLower(strings.TrimSpace(fundMarket))
	seen := make(map[string]struct{}, len(universe)+len(positions))
	requests := make([]newsrecall.Request, 0, len(universe)+len(positions))
	for _, sym := range universe {
		key := strings.ToUpper(strings.TrimSpace(sym))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		requests = append(requests, newsrecall.Request{Symbol: key, Market: market})
	}
	for _, pos := range positions {
		key := strings.ToUpper(strings.TrimSpace(pos.Symbol))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		mk := market
		if posMarket := strings.ToLower(strings.TrimSpace(pos.Market)); posMarket != "" {
			mk = posMarket
		}
		requests = append(requests, newsrecall.Request{
			Symbol:         key,
			Market:         mk,
			Exchange:       strings.TrimSpace(pos.Exchange),
			AssetClass:     strings.TrimSpace(pos.AssetClass),
			InstrumentType: strings.TrimSpace(pos.InstrumentKey),
		})
	}
	if len(requests) == 0 {
		return nil
	}
	return a.newsCatalystSvc.BuildCatalysts(ctx, requests, now)
}

// buildEarningsCalendar assembles the Sprint E #2 per-symbol
// scheduled-earnings snapshot. Dedup-merges universe ∪ positions
// onto upper-cased ticker keys and delegates to the wired
// earnings.Service. Returns nil when the service isn't wired
// (default deployment uses earnings.NoopFetcher → no events)
// so the prompt simply omits the block. The horizonDays cap is
// owned by earnings.Service (default 14d); we don't override here.
//
// Per-fund market is passed through as the disambiguation hint
// for back-ends that might know e.g. BABA both NYSE-listed and
// HK-listed.
func (a *runtimePMAgent) buildEarningsCalendar(ctx context.Context, fundMarket string, universe []string, positions []decision.DecisionPosition) *decision.EarningsCalendarSnapshot {
	if a == nil || a.earningsSvc == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(universe)+len(positions))
	symbols := make([]string, 0, len(universe)+len(positions))
	for _, sym := range universe {
		key := strings.ToUpper(strings.TrimSpace(sym))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		symbols = append(symbols, key)
	}
	for _, pos := range positions {
		key := strings.ToUpper(strings.TrimSpace(pos.Symbol))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		symbols = append(symbols, key)
	}
	if len(symbols) == 0 {
		return nil
	}
	return a.earningsSvc.Build(ctx, symbols, fundMarket)
}

// buildRiskBudget assembles the Sprint B #2 fund-level risk-budget
// snapshot. Returns nil when the service isn't wired or when the
// fund has insufficient NAV history (the snapshot is then omitted
// from the prompt and the PM falls back to its static R prior).
//
// SQL errors are downgraded to a warning log + nil result for the
// same reason cooldown is: risk budget is advisory, and a transient
// DB hiccup should never block a PM run.
func (a *runtimePMAgent) buildRiskBudget(ctx context.Context, fundID string, now time.Time) *decision.RiskBudgetSnapshot {
	if a == nil || a.riskBudgetSvc == nil {
		return nil
	}
	if strings.TrimSpace(fundID) == "" {
		return nil
	}
	snap, err := a.riskBudgetSvc.BuildSnapshot(ctx, fundID, now)
	if err != nil {
		slog.Warn("riskbudget snapshot failed; falling back to static R", "fund_id", fundID, "err", err)
		return nil
	}
	return snap
}

// buildCooldowns assembles the Sprint B #1 re-entry lock list. Same
// universe ∪ positions candidate set as the Sprint A blocks so the
// PM prompt sees a single coherent picture. Returns nil when the
// cooldown service isn't wired, the fund has no fills inside the
// window, or the candidate set is empty.
//
// SQL errors are downgraded to a warning log + nil result rather
// than propagated: cooldown is advisory, and a transient DB hiccup
// should never block a PM run.
func (a *runtimePMAgent) buildCooldowns(ctx context.Context, fundID string, universe []string, positions []decision.DecisionPosition, now time.Time) []decision.SymbolCooldown {
	if a == nil || a.cooldownSvc == nil {
		return nil
	}
	if strings.TrimSpace(fundID) == "" {
		return nil
	}
	seen := make(map[string]struct{}, len(universe)+len(positions))
	symbols := make([]string, 0, len(universe)+len(positions))
	for _, sym := range universe {
		key := strings.ToUpper(strings.TrimSpace(sym))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		symbols = append(symbols, key)
	}
	for _, pos := range positions {
		key := strings.ToUpper(strings.TrimSpace(pos.Symbol))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		symbols = append(symbols, key)
	}
	if len(symbols) == 0 {
		return nil
	}
	locks, err := a.cooldownSvc.Lookup(ctx, fundID, symbols, now)
	if err != nil {
		slog.Warn("cooldown lookup failed; treating as no active locks", "fund_id", fundID, "err", err)
		return nil
	}
	return locks
}

// buildUniverseRanking assembles the Sprint A #2 cross-sectional
// ranking table. Same candidate set as buildQuantSnapshots so the
// OHLC fetch cache satisfies the second pass for free. Returns nil
// when the ranker isn't wired (legacy / OHLC-disabled builds) or
// when the universe is too small for a meaningful z-score (the
// Ranker enforces a MinUniverse=3 floor).
func (a *runtimePMAgent) buildUniverseRanking(ctx context.Context, fundMarket string, universe []string, positions []decision.DecisionPosition) []decision.SymbolRanking {
	if a == nil || a.ranker == nil {
		return nil
	}
	market := strings.ToLower(strings.TrimSpace(fundMarket))
	seen := make(map[string]struct{}, len(universe)+len(positions))
	requests := make([]ranking.SymbolRequest, 0, len(universe)+len(positions))
	for _, sym := range universe {
		key := strings.ToUpper(strings.TrimSpace(sym))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		requests = append(requests, ranking.SymbolRequest{Symbol: key, Market: market})
	}
	for _, pos := range positions {
		key := strings.ToUpper(strings.TrimSpace(pos.Symbol))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		mk := market
		if posMarket := strings.ToLower(strings.TrimSpace(pos.Market)); posMarket != "" {
			mk = posMarket
		}
		requests = append(requests, ranking.SymbolRequest{Symbol: key, Market: mk})
	}
	if len(requests) == 0 {
		return nil
	}
	return a.ranker.BuildRanking(ctx, requests)
}

// buildQualityScores assembles the Sprint E #3 cross-sectional
// quality-factor table for the LLM prompt. Same universe ∪
// positions request set as buildUniverseRanking; the fundamental
// fetcher's cache makes the second pass effectively free.
//
// Returns nil when the quality service isn't wired (legacy /
// fundamental-disabled builds) or when fewer than MinUniverse
// (default 3) symbols carry any usable fundamental data — in
// either case the prompt skips the block and the PM falls back
// on the existing FundamentalSummary text blob.
func (a *runtimePMAgent) buildQualityScores(ctx context.Context, fundMarket string, universe []string, positions []decision.DecisionPosition) []decision.SymbolQualityScore {
	if a == nil || a.qualitySvc == nil {
		return nil
	}
	market := strings.ToLower(strings.TrimSpace(fundMarket))
	seen := make(map[string]struct{}, len(universe)+len(positions))
	requests := make([]quality.SymbolRequest, 0, len(universe)+len(positions))
	for _, sym := range universe {
		key := strings.ToUpper(strings.TrimSpace(sym))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		requests = append(requests, quality.SymbolRequest{Symbol: key, Market: market})
	}
	for _, pos := range positions {
		key := strings.ToUpper(strings.TrimSpace(pos.Symbol))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		mk := market
		if posMarket := strings.ToLower(strings.TrimSpace(pos.Market)); posMarket != "" {
			mk = posMarket
		}
		requests = append(requests, quality.SymbolRequest{Symbol: key, Market: mk})
	}
	if len(requests) == 0 {
		return nil
	}
	return a.qualitySvc.BuildScores(ctx, requests)
}

// buildValueScores assembles the Sprint F #1 cross-sectional
// value-factor table for the LLM prompt. Same universe ∪
// positions request set as buildQualityScores; the fundamental
// fetcher's cache makes the second pass effectively free.
//
// Returns nil when the value service isn't wired (legacy /
// fundamental-disabled deployments) or when nothing in the
// candidate set is non-empty. The prompt builder treats nil
// as "feature off" and silently omits the block. The PM falls
// back on the existing FundamentalSummary text + QualityScores.
func (a *runtimePMAgent) buildValueScores(ctx context.Context, fundMarket string, universe []string, positions []decision.DecisionPosition) []decision.SymbolValueScore {
	if a == nil || a.valueSvc == nil {
		return nil
	}
	market := strings.ToLower(strings.TrimSpace(fundMarket))
	seen := make(map[string]struct{}, len(universe)+len(positions))
	requests := make([]value.SymbolRequest, 0, len(universe)+len(positions))
	for _, sym := range universe {
		key := strings.ToUpper(strings.TrimSpace(sym))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		requests = append(requests, value.SymbolRequest{Symbol: key, Market: market})
	}
	for _, pos := range positions {
		key := strings.ToUpper(strings.TrimSpace(pos.Symbol))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		mk := market
		if posMarket := strings.ToLower(strings.TrimSpace(pos.Market)); posMarket != "" {
			mk = posMarket
		}
		requests = append(requests, value.SymbolRequest{Symbol: key, Market: mk})
	}
	if len(requests) == 0 {
		return nil
	}
	return a.valueSvc.BuildScores(ctx, requests)
}

// buildLowBetaScores assembles the Sprint F #2 Frazzini-Pedersen
// Betting-Against-Beta defensive overlay table for the LLM
// prompt. Same universe ∪ positions request set as
// buildQuantSnapshots; the ohlc fetcher's cache makes the
// per-symbol bar fetches free (they were already requested by
// quantSnapshot / ranker / correlation). The lowbeta service
// also fires one extra fetch per distinct market for the
// market-index bars (SPY / CSI300 ETF / Tracker Fund of HK)
// which is amortised across the whole fund's portfolio.
//
// Returns nil when the lowbeta service isn't wired (legacy /
// OHLC-disabled deployments) or when nothing in the candidate
// set is non-empty. The prompt builder treats nil as
// "feature off" and silently omits the block.
func (a *runtimePMAgent) buildLowBetaScores(ctx context.Context, fundMarket string, universe []string, positions []decision.DecisionPosition) []decision.SymbolLowBetaScore {
	if a == nil || a.lowBetaSvc == nil {
		return nil
	}
	market := strings.ToLower(strings.TrimSpace(fundMarket))
	seen := make(map[string]struct{}, len(universe)+len(positions))
	requests := make([]lowbeta.SymbolRequest, 0, len(universe)+len(positions))
	for _, sym := range universe {
		key := strings.ToUpper(strings.TrimSpace(sym))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		requests = append(requests, lowbeta.SymbolRequest{Symbol: key, Market: market})
	}
	for _, pos := range positions {
		key := strings.ToUpper(strings.TrimSpace(pos.Symbol))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		mk := market
		if posMarket := strings.ToLower(strings.TrimSpace(pos.Market)); posMarket != "" {
			mk = posMarket
		}
		requests = append(requests, lowbeta.SymbolRequest{Symbol: key, Market: mk})
	}
	if len(requests) == 0 {
		return nil
	}
	return a.lowBetaSvc.BuildScores(ctx, requests)
}

// buildPEAD assembles the Sprint F #3 Post-Earnings Announcement
// Drift snapshot. Reuses the universe ∪ positions candidate set
// the rest of the per-symbol blocks already build off. The
// service internally:
//   - calls earnings.HistoryService once to pull the trailing
//     60d earnings prints (Yahoo by default)
//   - calls ohlc.Fetch once per symbol with a recent print to
//     compute (entryClose, currentClose) → drift
//   - classifies each into {continuing, complete, faded, neutral}
//
// Both the earnings history call and the OHLC pulls hit the
// shared caches (history doesn't have a cache layer yet — Sprint
// G candidate — but Yahoo's keyless endpoint is fast enough for
// the symbol counts we run).
//
// Returns nil when the PEAD service isn't wired OR when nothing
// in the candidate set has a recent print → the prompt block is
// silently absent.
func (a *runtimePMAgent) buildPEAD(ctx context.Context, fundMarket string, universe []string, positions []decision.DecisionPosition) *decision.PEADSnapshot {
	if a == nil || a.peadSvc == nil {
		return nil
	}
	market := strings.ToLower(strings.TrimSpace(fundMarket))
	seen := make(map[string]struct{}, len(universe)+len(positions))
	requests := make([]pead.SymbolRequest, 0, len(universe)+len(positions))
	for _, sym := range universe {
		key := strings.ToUpper(strings.TrimSpace(sym))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		requests = append(requests, pead.SymbolRequest{Symbol: key, Market: market})
	}
	for _, pos := range positions {
		key := strings.ToUpper(strings.TrimSpace(pos.Symbol))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		mk := market
		if posMarket := strings.ToLower(strings.TrimSpace(pos.Market)); posMarket != "" {
			mk = posMarket
		}
		requests = append(requests, pead.SymbolRequest{Symbol: key, Market: mk})
	}
	if len(requests) == 0 {
		return nil
	}
	return a.peadSvc.BuildSnapshot(ctx, requests)
}

// buildQuantSnapshots assembles the per-symbol regime + ATR +
// position-size-ceiling block for the LLM prompt. Sprint A #1.
//
// The candidate set is the union of the configured universe and the
// current holdings (so the prompt can size both first-time buys AND
// proposed reduces against the same volatility unit). Snapshots
// without any usable signal — typically newly listed symbols whose
// daily history is shorter than ATRPeriod+1 — are dropped here so
// the prompt only sees rows that actually carry information.
//
// Returns nil when the builder isn't wired (legacy / smoke builds);
// the prompt simply omits the quantSnapshots key in that case.
func (a *runtimePMAgent) buildQuantSnapshots(ctx context.Context, fundMarket string, universe []string, positions []decision.DecisionPosition) []decision.SymbolQuantSnapshot {
	if a == nil || a.quantSnapshot == nil {
		return nil
	}
	market := strings.ToLower(strings.TrimSpace(fundMarket))
	// Dedup keyed on upper-cased symbol; the snapshot builder also
	// dedups internally but that drops the (symbol, market) tuple,
	// while we want a single Snapshot per symbol regardless of
	// market source. Universe symbols inherit the fund market; held
	// positions carry their own per-row market via DecisionPosition.
	seen := make(map[string]struct{}, len(universe)+len(positions))
	requests := make([]quantsnapshot.SymbolRequest, 0, len(universe)+len(positions))
	for _, sym := range universe {
		key := strings.ToUpper(strings.TrimSpace(sym))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		requests = append(requests, quantsnapshot.SymbolRequest{Symbol: key, Market: market})
	}
	for _, pos := range positions {
		key := strings.ToUpper(strings.TrimSpace(pos.Symbol))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		mk := market
		if posMarket := strings.ToLower(strings.TrimSpace(pos.Market)); posMarket != "" {
			mk = posMarket
		}
		requests = append(requests, quantsnapshot.SymbolRequest{Symbol: key, Market: mk})
	}
	if len(requests) == 0 {
		return nil
	}
	snapshots := a.quantSnapshot.BuildBatch(ctx, requests)
	if len(snapshots) == 0 {
		return nil
	}
	// Drop snapshots that carry no signal so the prompt JSON doesn't
	// bloat with one no-op row per universe entry on funds whose
	// OHLC pipeline is half-wired.
	out := make([]decision.SymbolQuantSnapshot, 0, len(snapshots))
	for _, s := range snapshots {
		if !s.HasSignal() {
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildIntradaySnapshots is the Sprint 3 / L1 builder. Mirrors
// buildQuantSnapshots' dedup-then-fetch shape but uses the cheaper
// 5m intraday provider and returns DecisionInput.IntradaySnapshots.
// nil-safe: when the intraday builder is unwired (legacy fund profiles
// without intraday OHLC) it returns nil and the prompt block is
// silently omitted.
func (a *runtimePMAgent) buildIntradaySnapshots(ctx context.Context, fundMarket string, universe []string, positions []decision.DecisionPosition, asOf time.Time) []decision.IntradayContext {
	if a == nil || a.intradayBuilder == nil {
		return nil
	}
	market := strings.ToLower(strings.TrimSpace(fundMarket))
	seen := make(map[string]struct{}, len(universe)+len(positions))
	symbols := make([]string, 0, len(universe)+len(positions))
	addSym := func(raw string) {
		key := strings.ToUpper(strings.TrimSpace(raw))
		if key == "" {
			return
		}
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		symbols = append(symbols, key)
	}
	for _, s := range universe {
		addSym(s)
	}
	for _, p := range positions {
		addSym(p.Symbol)
	}
	if len(symbols) == 0 {
		return nil
	}
	snaps := a.intradayBuilder.Build(ctx, symbols, market, asOf)
	if len(snaps) == 0 {
		return nil
	}
	out := make([]decision.IntradayContext, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, decision.IntradayContext{
			Symbol:         s.Symbol,
			Interval:       s.Interval,
			TrendDirection: s.TrendDirection,
			LastClose:      s.LastClose,
			OpenClose:      s.OpenClose,
			VolZScore:      s.VolZScore,
			VolRatio:       s.VolRatio,
		})
	}
	return out
}

// buildSemanticRecall is the Sprint 3 / L3 hook. It encodes the
// current daily context (macro briefing + universe symbols) into an
// embedding vector and asks recall.Service for the k most-similar
// past memories. Returns nil on every soft-failure path — the prompt
// builder then simply omits the SemanticRecall block.
//
// Soft failure cases (no error propagated):
//   - recall service / embedder unwired (no OPENAI_API_KEY or
//     pgvector extension missing — embed_loop also short-circuits in
//     that case, so the column is unpopulated and Query returns
//     nothing).
//   - embed call fails (network, rate limit).
//   - Query returns zero rows (no embedded memories exist yet for
//     this fund).
func (a *runtimePMAgent) buildSemanticRecall(ctx context.Context, fundID, macroBriefing string, universe []string) []decision.SemanticRecallContext {
	if a == nil || a.recall == nil || a.recallEmbedder == nil {
		return nil
	}
	query := buildSemanticRecallQueryText(macroBriefing, universe)
	if strings.TrimSpace(query) == "" {
		return nil
	}
	embedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	vec, err := a.recallEmbedder.Embed(embedCtx, query)
	if err != nil {
		slog.Debug("semantic recall: embed query failed",
			"fund_id", fundID,
			"err", err,
		)
		return nil
	}
	if len(vec) == 0 {
		return nil
	}
	hits, err := a.recall.Query(ctx, fundID, "", vec, 8)
	if err != nil {
		slog.Debug("semantic recall: query failed",
			"fund_id", fundID,
			"err", err,
		)
		return nil
	}
	if len(hits) == 0 {
		return nil
	}
	out := make([]decision.SemanticRecallContext, 0, len(hits))
	for _, h := range hits {
		out = append(out, decision.SemanticRecallContext{
			CreatedAt:  h.CreatedAt.Format(time.RFC3339),
			Layer:      h.Layer,
			Title:      h.Title,
			Snippet:    h.Snippet,
			Tags:       h.Tags,
			Similarity: h.Similarity,
		})
	}
	return out
}

// buildContradictionViews materialises a []ResearcherView from the
// orchestrator's already-extracted bull/bear/quant cases. We drop
// blank cases so the checker doesn't lose count of "real" voices —
// a debate where only bull spoke would otherwise look like a 3-view
// run with 2 empty bodies and the checker would short-circuit on the
// wrong condition.
func buildContradictionViews(bull, bear, quant, stance string) []contradiction.ResearcherView {
	views := make([]contradiction.ResearcherView, 0, 3)
	addView := func(role, body string) {
		body = strings.TrimSpace(body)
		if body == "" {
			return
		}
		views = append(views, contradiction.ResearcherView{
			Role:   role,
			Stance: strings.TrimSpace(stance),
			Body:   body,
		})
	}
	addView("bull", bull)
	addView("bear", bear)
	addView("quant", quant)
	return views
}

// buildSemanticRecallQueryText composes the embed-input text from the
// (a) current MacroBriefing summary and (b) the trimmed candidate
// universe. Truncated tight so the embed cost stays bounded — for
// recall purposes the macro summary is what should drive the search;
// universe symbols are tail context that may or may not survive
// truncation downstream.
func buildSemanticRecallQueryText(macroBriefing string, universe []string) string {
	var sb strings.Builder
	macro := strings.TrimSpace(macroBriefing)
	if macro != "" {
		if len(macro) > 600 {
			macro = macro[:600]
		}
		sb.WriteString(macro)
	}
	if len(universe) > 0 {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("Universe: ")
		max := 20
		if len(universe) < max {
			max = len(universe)
		}
		sb.WriteString(strings.Join(universe[:max], ", "))
	}
	return sb.String()
}

// buildSleeveScorecard renders the attribution scorecard for the
// LLM PM prompt. Returns "" on every failure path so the prompt
// builder simply omits the section — attribution feedback is a
// soft prior; the LLM must continue to work without it.
//
// Soft failures (any of which produce a blank scorecard):
//   - a.attribution is nil (legacy deployments)
//   - BuildReport errors out (memory store down, weird date)
//   - The fund has no closed lots → report.HasData()==false
//   - No cell meets the MinSampleSize floor
//
// The lookback window mirrors attribution.DefaultLookbackDays
// (30 days) so the prompt and the dashboard see the same data.
func (a *runtimePMAgent) buildSleeveScorecard(ctx context.Context, fundID string) string {
	if a == nil || a.attribution == nil {
		return ""
	}
	report, err := a.attribution.BuildReport(ctx, fundID, attribution.DefaultLookbackDays)
	if err != nil {
		slog.Debug("decision prompt: attribution scorecard unavailable",
			"fund_id", fundID,
			"err", err,
		)
		return ""
	}
	if report == nil {
		return ""
	}
	scorecard := attribution.BuildPromptScorecard(*report, attribution.PromptScorecardOptions{})
	return scorecard.Summary
}

// buildLessonReplay renders the recent-attribution-lessons block
// for the LLM PM prompt (Phase 3A-10). Same defensive contract
// as buildSleeveScorecard: every failure path returns "" so the
// prompt builder simply omits the section.
//
// Where the scorecard is the numeric channel (rows of
// win-rate / total-pnl per sleeve × regime), the replay is the
// textual channel — the lesson generator's own prose, in the
// same words the AgentLearning dashboard surfaces to human
// operators. The wiring layer pulls the most recent N
// attribution memories from the store, hands them to the pure
// builder, and forwards the rendered Markdown block.
//
// Soft failures:
//   - a.memoryRepo nil (very old test wirings)
//   - ListByFund errors (DB blip)
//   - No attribution memories yet (brand-new fund)
//   - Every survivor falls outside the LookbackDays window
//
// Lookback defaults to 14 days inside BuildLessonReplay; we
// fetch up to 50 rows so cluttered funds (multiple sleeves,
// regime variations) still have a chance of populating the
// dedup map before the cap is hit.
func (a *runtimePMAgent) buildLessonReplay(ctx context.Context, fundID string) string {
	if a == nil || a.memoryRepo == nil {
		return ""
	}
	memories, err := a.memoryRepo.ListByFund(ctx, fundID, attribution.MemoryLayer, 50)
	if err != nil {
		slog.Debug("decision prompt: attribution lesson replay unavailable",
			"fund_id", fundID,
			"err", err,
		)
		return ""
	}
	if len(memories) == 0 {
		return ""
	}
	replay := attribution.BuildLessonReplay(memories, time.Now().UTC(), attribution.LessonReplayOptions{})
	return replay.Summary
}

// buildAgentTrackRecord renders the Sprint 9.1 alpha-aware-memory
// block for the LLM PM prompt. Same defensive contract as
// buildSleeveScorecard / buildLessonReplay: every failure path
// returns "" so the prompt builder simply omits the section.
//
// The block synthesises two soft priors:
//
//   - The per-fund agent leaderboard (top + bottom by avg α vs
//     benchmark, hit_rate, decision count) drawn from
//     agent_reputation_stats. This is how the PM learns which
//     analyst / bull / bear voice has actually been right.
//   - The most recent alpha-tagged lessons (the memory rows the
//     alpha-aware reputation backfill mints when |α| crosses
//     the configured threshold). Each carries the agent tag, the
//     realised α, and the lesson body.
//
// alphalesson.BuildContext is the pure renderer; this wiring
// layer only supplies the repos + fundID + locale-free defaults.
// Both repos are nil-safe — the renderer returns "" when neither
// side has data, which matches the legacy behaviour for brand-new
// funds without a reputation history.
func (a *runtimePMAgent) buildAgentTrackRecord(ctx context.Context, fundID string) string {
	if a == nil || a.agentReputationRepo == nil || a.alphaLessonRepo == nil {
		return ""
	}
	block, err := alphalesson.BuildContext(ctx, a.agentReputationRepo, a.alphaLessonRepo, fundID, alphalesson.ContextOptions{})
	if err != nil {
		slog.Debug("decision prompt: agent track record unavailable",
			"fund_id", fundID,
			"err", err,
		)
		return ""
	}
	return block
}

// translateDecisionActions turns a DecisionOutput coming back from the
// LLM into a slice of repository.PlanAction the runtime can persist
// and execute. Each action is normalised in three ways:
//
//  1. Reduce / sell: resolved against the current positions table so
//     QtyPct (the engine's fraction of position) is converted to an
//     absolute share count, then clamped to SellableQtyToday so T+1
//     locked lots never leak through. If sellableToday is 0 the
//     action is demoted to "hold" with a T+1 reasoning note.
//  2. Buy / add: resolved against the fund's market profile to obtain
//     a live quote, then quantity is computed via lot-size aware
//     NormalizeBuyQty. If the resulting quantity is 0 (e.g. A-share
//     budget below 100 shares) the action is demoted to "watch".
//  3. Hold / watch: straight-through with the engine's reasoning.
//
// Unknown action verbs and empty-symbol actions are dropped. The
// caller treats a fully-empty translation result as a fallback signal.
func (a *runtimePMAgent) translateDecisionActions(ctx context.Context, fund *repository.Fund, positions []repository.HoldingPosition, boughtTodayByKey map[string]float64, roundtable *workflow.RoundtableResult, output *decision.DecisionOutput, fundID string) []repository.PlanAction {
	positionBySymbol := make(map[string]*repository.HoldingPosition, len(positions))
	for i := range positions {
		key := strings.ToUpper(strings.TrimSpace(positions[i].Symbol))
		if key != "" {
			positionBySymbol[key] = &positions[i]
		}
	}

	result := make([]repository.PlanAction, 0, len(output.Actions))
	for sortIdx, da := range output.Actions {
		symbolKey := strings.ToUpper(strings.TrimSpace(da.Symbol))
		switch strings.ToLower(da.Action) {
		case "reduce", "sell":
			pos, ok := positionBySymbol[symbolKey]
			if !ok || pos.Quantity <= 0 {
				continue
			}
			result = append(result, a.translateReduceAction(ctx, fund, pos, boughtTodayByKey, roundtable, da, sortIdx))
		case "hold":
			pos, ok := positionBySymbol[symbolKey]
			if !ok {
				continue
			}
			result = append(result, a.translateHoldAction(ctx, fund, pos, roundtable, da, sortIdx))
		case "buy", "add":
			action, emitted := a.translateBuyAction(ctx, fund, roundtable, da, sortIdx)
			if emitted {
				result = append(result, action)
			}
		case "watch":
			action, emitted := a.translateWatchAction(ctx, fund, roundtable, da, sortIdx, fundID)
			if emitted {
				result = append(result, action)
			}
		}
	}
	return result
}

// translateReduceAction turns a LLM "reduce" (or "sell") instruction
// into a PlanAction backed by the matching position. QtyPct is
// interpreted as a fraction of the *currently sellable* qty; for
// "sell" the engine's QtyPct is ignored and we treat it as 1.0 (full
// liquidation of sellable shares). If T+1 has locked the entire
// position, the action is demoted to "hold" with a market-rule note.
func (a *runtimePMAgent) translateReduceAction(ctx context.Context, fund *repository.Fund, pos *repository.HoldingPosition, boughtTodayByKey map[string]float64, roundtable *workflow.RoundtableResult, da decision.DecisionAction, sortIdx int) repository.PlanAction {
	hint := instrument2.Hint{
		Market:     pos.Market.String,
		Exchange:   pos.Exchange.String,
		AssetClass: pos.AssetClass.String,
	}
	posKey := positionMapKey(pos.InstrumentKey, pos.Symbol)
	sellableToday := instrument2.SellableQtyToday(pos.Symbol, hint, pos.Quantity, boughtTodayByKey[posKey])
	lockedToday := pos.Quantity - sellableToday
	if lockedToday < 0 {
		lockedToday = 0
	}

	pct := da.QtyPct
	if strings.EqualFold(da.Action, "sell") {
		pct = 1.0
	}
	if pct <= 0 {
		pct = 1.0
	}
	if pct > 1 {
		pct = 1
	}

	actionType := "reduce"
	qtyVal := math.Floor(sellableToday * pct)
	if sellableToday <= 0 || qtyVal <= 0 {
		actionType = "hold"
		qtyVal = 0
	}

	price := fallbackPositive(pos.CurrentPrice, pos.CostPrice)
	if quote, err := a.quoteForAction(ctx, planActionInstrumentRef(fund, pos.Symbol, pos.InstrumentKey, pos.Market.String, pos.Exchange.String, pos.AssetClass.String, pos.InstrumentType.String, pos.QuoteCurrency.String, pos.SettlementCurrency.String, contractMultiplierValue(pos.ContractMultiplier), formatNullTime(pos.ExpiryDate))); err == nil && quote != nil && quote.Price > 0 {
		price = quote.Price
	}
	amountVal := 0.0
	if actionType == "reduce" {
		amountVal = roundCurrency(qtyVal * price)
	}

	reasoning := strings.TrimSpace(da.Reasoning)
	if reasoning == "" {
		reasoning = selectConsensus(roundtable, sortIdx, "decision engine reduce")
	}
	if lockedToday > 0 {
		reasoning = strings.TrimSpace(reasoning) + fmt.Sprintf(
			" | A股市场 T+1 结算规则：今日新买 %.0f 股需待下一交易日方可卖出，本次减仓只涉及已结算的 %.0f 股",
			lockedToday, sellableToday,
		)
	}

	confidence := da.Confidence
	if confidence <= 0 {
		confidence = 0.7
	}
	return repository.PlanAction{
		InstrumentKey:      firstNonEmptyValue(pos.InstrumentKey, pos.Symbol),
		Symbol:             pos.Symbol,
		Market:             pos.Market,
		Exchange:           pos.Exchange,
		AssetClass:         pos.AssetClass,
		InstrumentType:     pos.InstrumentType,
		Action:             actionType,
		PositionSide:       pos.PositionSide,
		Quantity:           sql.NullFloat64{Float64: qtyVal, Valid: qtyVal > 0},
		Price:              sql.NullFloat64{Float64: price, Valid: price > 0},
		Amount:             sql.NullFloat64{Float64: amountVal, Valid: amountVal > 0},
		Reasoning:          sql.NullString{String: reasoning, Valid: reasoning != ""},
		Confidence:         sql.NullFloat64{Float64: confidence, Valid: true},
		SupportedBy:        []string{"decision_engine", "roundtable"},
		ExecutionStatus:    "pending",
		SortOrder:          sortIdx,
		QuoteCurrency:      pos.QuoteCurrency,
		SettlementCurrency: pos.SettlementCurrency,
		MarginMode:         pos.MarginMode,
		Leverage:           pos.Leverage,
		ContractMultiplier: pos.ContractMultiplier,
		ExpiryDate:         pos.ExpiryDate,
	}
}

// translateHoldAction emits a "hold" PlanAction tied to an existing
// position. No quantity / amount fields are populated — hold is a
// declarative no-op the runtime persists for audit traceability.
func (a *runtimePMAgent) translateHoldAction(ctx context.Context, fund *repository.Fund, pos *repository.HoldingPosition, roundtable *workflow.RoundtableResult, da decision.DecisionAction, sortIdx int) repository.PlanAction {
	price := fallbackPositive(pos.CurrentPrice, pos.CostPrice)
	if quote, err := a.quoteForAction(ctx, planActionInstrumentRef(fund, pos.Symbol, pos.InstrumentKey, pos.Market.String, pos.Exchange.String, pos.AssetClass.String, pos.InstrumentType.String, pos.QuoteCurrency.String, pos.SettlementCurrency.String, contractMultiplierValue(pos.ContractMultiplier), formatNullTime(pos.ExpiryDate))); err == nil && quote != nil && quote.Price > 0 {
		price = quote.Price
	}

	reasoning := strings.TrimSpace(da.Reasoning)
	if reasoning == "" {
		reasoning = selectConsensus(roundtable, sortIdx, "decision engine hold")
	}
	confidence := da.Confidence
	if confidence <= 0 {
		confidence = 0.65
	}
	return repository.PlanAction{
		InstrumentKey:      firstNonEmptyValue(pos.InstrumentKey, pos.Symbol),
		Symbol:             pos.Symbol,
		Market:             pos.Market,
		Exchange:           pos.Exchange,
		AssetClass:         pos.AssetClass,
		InstrumentType:     pos.InstrumentType,
		Action:             "hold",
		PositionSide:       pos.PositionSide,
		Price:              sql.NullFloat64{Float64: price, Valid: price > 0},
		Reasoning:          sql.NullString{String: reasoning, Valid: reasoning != ""},
		Confidence:         sql.NullFloat64{Float64: confidence, Valid: true},
		SupportedBy:        []string{"decision_engine", "roundtable"},
		ExecutionStatus:    "pending",
		SortOrder:          sortIdx,
		QuoteCurrency:      pos.QuoteCurrency,
		SettlementCurrency: pos.SettlementCurrency,
		MarginMode:         pos.MarginMode,
		Leverage:           pos.Leverage,
		ContractMultiplier: pos.ContractMultiplier,
		ExpiryDate:         pos.ExpiryDate,
	}
}

// translateBuyAction resolves a LLM "buy"/"add" recommendation against
// the fund's market profile, fetches a live quote, and computes the
// share quantity via lot-size aware NormalizeBuyQty. The notional is
// QtyPct * TotalAssets, capped by the configured BuyBudget.
//
// Quote-unavailable handling
//
// When the quote service can't price the symbol we DOWNGRADE the
// action to "watch" instead of synthesising an executable buy. The
// previous behaviour stamped the *notional budget* into PlanAction.
// Price with Quantity=1, which the broker simulator faithfully
// honoured as a limit order — on 2026-06-02 this produced a 301308
// fill at 96,226.4188 CNY/share (true mid was ~500). Production
// trading systems never invent a reference price; missing quotes
// must defer until the next plan cycle can re-price.
//
// If quantity rounds to 0 (e.g. A-share board minimum 100 shares but
// budget < 100 * price) we also emit "watch".
func (a *runtimePMAgent) translateBuyAction(ctx context.Context, fund *repository.Fund, roundtable *workflow.RoundtableResult, da decision.DecisionAction, sortIdx int) (repository.PlanAction, bool) {
	symbol := strings.TrimSpace(da.Symbol)
	if symbol == "" {
		return repository.PlanAction{}, false
	}
	pct := da.QtyPct
	if pct <= 0 {
		pct = 0.05
	}
	if pct > 1 {
		pct = 1
	}
	totalAssets := 0.0
	if fund != nil {
		totalAssets = fund.TotalAssets
	}
	notional := pct * totalAssets
	if cap := planBuyAmountWithinRiskCap(fund); cap > 0 && notional > cap {
		notional = cap
	}
	if notional <= 0 {
		notional = planBuyAmountWithinRiskCap(fund)
	}

	instrument := defaultInstrumentRef(fund, workflow.FocusStock, symbol)
	quote, qerr := a.quoteForAction(ctx, instrument)
	reasoning := strings.TrimSpace(da.Reasoning)
	if reasoning == "" {
		reasoning = selectConsensus(roundtable, sortIdx, "decision engine buy")
	}
	confidence := da.Confidence
	if confidence <= 0 {
		confidence = 0.7
	}

	if qerr != nil || quote == nil || quote.Price <= 0 {
		// quote unavailable → DOWNGRADE to watch, never fake a buy.
		// See the symmetric branch in pmGenerateBuyPlan for the full
		// rationale (96,226 CNY/share fill on 2026-06-02). A
		// production-grade trading system must not synthesize a
		// limit price from a notional budget — the next time this
		// symbol gets a real quote the PM will produce a properly
		// priced order.
		errSummary := "unknown error"
		if qerr != nil {
			errSummary = qerr.Error()
		}
		reasoning := appendSkillContext(
			reasoning,
			fmt.Sprintf("quote unavailable for %s (%s); downgraded to watch — notional budget on the table was %.4f, awaiting reference price before any order", symbol, errSummary, notional),
		)
		return repository.PlanAction{
			InstrumentKey:      firstNonEmptyValue(instrument.InstrumentKey, buildInstrumentKey(instrument.Exchange, symbol), symbol),
			Symbol:             symbol,
			Market:             nullString(instrument.Market),
			Exchange:           nullString(instrument.Exchange),
			AssetClass:         nullString(instrument.AssetClass),
			InstrumentType:     nullString(instrument.InstrumentType),
			Action:             "watch",
			Reasoning:          sql.NullString{String: reasoning, Valid: true},
			Confidence:         sql.NullFloat64{Float64: confidence, Valid: true},
			SupportedBy:        []string{"decision_engine", "roundtable"},
			ExecutionStatus:    "pending",
			SortOrder:          sortIdx,
			QuoteCurrency:      nullString(instrument.QuoteCurrency),
			SettlementCurrency: nullString(instrument.SettlementCurrency),
			ContractMultiplier: sql.NullFloat64{Float64: instrument.ContractMultiplier, Valid: instrument.ContractMultiplier > 0},
		}, true
	}

	rawQuantity := math.Floor(notional / quote.Price)
	hint := instrument2.Hint{
		Market:     firstNonEmptyValue(quote.Market, instrument.Market),
		Exchange:   firstNonEmptyValue(quote.Exchange, instrument.Exchange),
		AssetClass: firstNonEmptyValue(quote.AssetClass, instrument.AssetClass),
	}
	quantity := instrument2.NormalizeBuyQty(symbol, hint, rawQuantity)
	if !instrument2.SpecFor(instrument2.Classify(symbol, hint)).IsAShare() {
		quantity = math.Max(1, rawQuantity)
	}
	if quantity <= 0 {
		spec := instrument2.SpecFor(instrument2.Classify(symbol, hint))
		return repository.PlanAction{
			InstrumentKey:      firstNonEmptyValue(quote.InstrumentKey, instrument.InstrumentKey, buildInstrumentKey(quote.Exchange, symbol), symbol),
			Symbol:             symbol,
			Market:             nullString(firstNonEmptyValue(quote.Market, instrument.Market)),
			Exchange:           nullString(firstNonEmptyValue(quote.Exchange, instrument.Exchange)),
			AssetClass:         nullString(firstNonEmptyValue(quote.AssetClass, instrument.AssetClass)),
			InstrumentType:     nullString(instrument.InstrumentType),
			Action:             "watch",
			Price:              sql.NullFloat64{Float64: quote.Price, Valid: true},
			Reasoning:          sql.NullString{String: appendSkillContext(reasoning, fmt.Sprintf("buy budget below A-share lot minimum (%d shares) for %s — switched to watch", spec.MinLot, symbol)), Valid: true},
			Confidence:         sql.NullFloat64{Float64: 0.55, Valid: true},
			SupportedBy:        []string{"decision_engine", "roundtable", "marketdata"},
			ExecutionStatus:    "pending",
			SortOrder:          sortIdx,
			QuoteCurrency:      nullString(firstNonEmptyValue(quote.QuoteCurrency, instrument.QuoteCurrency)),
			SettlementCurrency: nullString(instrument.SettlementCurrency),
			ContractMultiplier: sql.NullFloat64{Float64: instrument.ContractMultiplier, Valid: instrument.ContractMultiplier > 0},
		}, true
	}

	return repository.PlanAction{
		InstrumentKey:      firstNonEmptyValue(quote.InstrumentKey, instrument.InstrumentKey, buildInstrumentKey(quote.Exchange, symbol), symbol),
		Symbol:             symbol,
		Market:             nullString(firstNonEmptyValue(quote.Market, instrument.Market)),
		Exchange:           nullString(firstNonEmptyValue(quote.Exchange, instrument.Exchange)),
		AssetClass:         nullString(firstNonEmptyValue(quote.AssetClass, instrument.AssetClass)),
		InstrumentType:     nullString(instrument.InstrumentType),
		Action:             "buy",
		Quantity:           sql.NullFloat64{Float64: quantity, Valid: true},
		Price:              sql.NullFloat64{Float64: quote.Price, Valid: true},
		Amount:             sql.NullFloat64{Float64: roundCurrency(quantity * quote.Price), Valid: true},
		Reasoning:          sql.NullString{String: appendQuoteReference(LanguageFromContext(ctx), reasoning, quote), Valid: true},
		Confidence:         sql.NullFloat64{Float64: confidence, Valid: true},
		SupportedBy:        []string{"decision_engine", "roundtable", "marketdata"},
		ExecutionStatus:    "pending",
		SortOrder:          sortIdx,
		QuoteCurrency:      nullString(firstNonEmptyValue(quote.QuoteCurrency, instrument.QuoteCurrency)),
		SettlementCurrency: nullString(instrument.SettlementCurrency),
		ContractMultiplier: sql.NullFloat64{Float64: instrument.ContractMultiplier, Valid: instrument.ContractMultiplier > 0},
	}, true
}

// translateWatchAction emits a bare "watch" PlanAction. Watch is the
// engine's "no-trade today, monitor only" verdict; we keep the
// reasoning and confidence as the engine returned them. When the
// engine forgets to supply a symbol we anchor the action to the
// fund itself so the plan still persists cleanly.
func (a *runtimePMAgent) translateWatchAction(ctx context.Context, fund *repository.Fund, roundtable *workflow.RoundtableResult, da decision.DecisionAction, sortIdx int, fundID string) (repository.PlanAction, bool) {
	reasoning := strings.TrimSpace(da.Reasoning)
	if reasoning == "" {
		reasoning = selectConsensus(roundtable, sortIdx, "decision engine watch")
	}
	confidence := da.Confidence
	if confidence <= 0 {
		confidence = 0.55
	}
	symbol := strings.TrimSpace(da.Symbol)
	if symbol == "" {
		return repository.PlanAction{
			InstrumentKey:   firstNonEmptyValue(fundID, "workflow-watch"),
			Action:          "watch",
			Reasoning:       sql.NullString{String: reasoning, Valid: reasoning != ""},
			Confidence:      sql.NullFloat64{Float64: confidence, Valid: true},
			SupportedBy:     []string{"decision_engine", "roundtable"},
			ExecutionStatus: "pending",
			SortOrder:       sortIdx,
		}, true
	}
	instrument := defaultInstrumentRef(fund, workflow.FocusStock, symbol)
	return repository.PlanAction{
		InstrumentKey:      firstNonEmptyValue(instrument.InstrumentKey, buildInstrumentKey(instrument.Exchange, symbol), symbol),
		Symbol:             symbol,
		Market:             nullString(instrument.Market),
		Exchange:           nullString(instrument.Exchange),
		AssetClass:         nullString(instrument.AssetClass),
		InstrumentType:     nullString(instrument.InstrumentType),
		Action:             "watch",
		Reasoning:          sql.NullString{String: reasoning, Valid: reasoning != ""},
		Confidence:         sql.NullFloat64{Float64: confidence, Valid: true},
		SupportedBy:        []string{"decision_engine", "roundtable"},
		ExecutionStatus:    "pending",
		SortOrder:          sortIdx,
		QuoteCurrency:      nullString(instrument.QuoteCurrency),
		SettlementCurrency: nullString(instrument.SettlementCurrency),
	}, true
}

func (a *runtimePMAgent) workflowSymbolCandidates(ctx context.Context, fundID string, pmAgent *repository.Agent) []string {
	agents := make([]*repository.Agent, 0, 4)
	if pmAgent != nil {
		agents = append(agents, pmAgent)
	}
	if a.teamRepo == nil || a.agentRepo == nil {
		return candidateWorkflowSymbolsFromTeamAgents(agents...)
	}
	members, err := a.teamRepo.ListByFund(ctx, fundID)
	if err != nil {
		return candidateWorkflowSymbolsFromTeamAgents(agents...)
	}
	pmID := ""
	if pmAgent != nil {
		pmID = strings.TrimSpace(pmAgent.ID)
	}
	for i := range members {
		agentID := strings.TrimSpace(members[i].AgentID)
		if agentID == "" || agentID == pmID {
			continue
		}
		agent, err := a.agentRepo.GetByID(ctx, agentID)
		if err != nil {
			continue
		}
		agents = append(agents, agent)
	}
	return candidateWorkflowSymbolsFromTeamAgents(agents...)
}

func (a *runtimeApprovalGateway) RequestApproval(ctx context.Context, plan *workflow.InvestmentPlanResult) error {
	if plan == nil {
		return api.ErrBadInput
	}
	// Default path: standard pending-user approval. The auto-execute
	// fast path overrides this below — note that we only flip status
	// to "approved" once *every* guardrail check passes, otherwise we
	// fall through to pending_user and write the reason into the
	// plan's risk_review JSON so the UI can surface it.
	plan.Status = workflow.PlanStatusPendingUser

	autoCfg, fund, dbPlan, actions, decision := a.evaluateAutoExecute(ctx, plan)
	if decision.passed {
		// Auto-execute fast path: stamp the audit column on every
		// action *before* flipping plan status, otherwise a follow-up
		// daily-sum query could miss this plan if the gateway crashed
		// between the two UPDATEs. StampAutoExecuted is idempotent so
		// a duplicate run after a crash-and-resume is harmless.
		now := a.timeNow()
		if err := a.planRepo.StampAutoExecuted(ctx, plan.ID, now); err != nil {
			return mapRepositoryError(err)
		}
		if err := a.persistAutoExecuteAudit(ctx, plan, dbPlan, autoCfg, decision, actions, fund); err != nil {
			// audit-write failure is non-fatal — we'd rather approve
			// the plan than block trading on a JSON-serialise glitch.
			// Still log via the metrics layer once we have one for
			// approval gating; for now silent best-effort matches the
			// "always succeed if status update succeeds" semantics.
			_ = err
		}
		plan.Status = workflow.PlanStatusApproved
		return mapRepositoryError(a.planRepo.UpdateStatus(ctx, plan.ID, "approved"))
	}

	// Auto-execute either disabled or one of the guardrails fired.
	// Persist the audit (reason + thresholds) so the UI's approval
	// modal can explain why this plan didn't auto-approve. Before
	// persisting, append an outcome suffix that matches what we
	// actually do next — the runAutoExecuteGuardrails layer only
	// describes the *cause* (e.g. "confidence_below_floor"); only
	// here do we know whether the consequence is "auto-rejected and
	// move on" (autoExecute enabled) or "wait for a human"
	// (autoExecute disabled). Previously every cause text was
	// hard-coded with "已退回人工审批" which lied to the user when
	// autoExecute was on (plan was actually rejected, not pending).
	if decision.reason != "" {
		if autoCfg.Enabled {
			decision.reason = decision.reason + "，已自动驳回，等待下次决策窗口"
		} else {
			decision.reason = decision.reason + "，已退回人工审批"
		}
		if err := a.persistAutoExecuteAudit(ctx, plan, dbPlan, autoCfg, decision, actions, fund); err != nil {
			_ = err
		}
	}
	// Two routes depending on whether the fund opted into
	// autoExecute:
	//   1. autoExecute.enabled == false → legacy human-in-loop.
	//      Drop to pending_user and wait for a human.
	//   2. autoExecute.enabled == true  → the user asked the system
	//      to run unattended. Blocking on pending_user defeats that
	//      promise and — worse — wedges the workflow_run row in
	//      "paused" state, which causes the next 30-min slot to be
	//      silently skipped (we observed 10:30/14:00 slots dropped
	//      on 2026-05-22 because the 10:00 plan stalled awaiting
	//      human approval for 31 minutes). When autoExecute is on,
	//      a gate refusal is treated as an autonomous rejection:
	//      we mark the plan rejected with the gate's reason, the
	//      orchestrator's WaitForDecision returns (false, nil)
	//      immediately, the workflow rolls past trade_execution
	//      into settle/daily_review, and the scheduler is freed
	//      for the next slot.
	if autoCfg.Enabled {
		plan.Status = workflow.PlanStatusRejected
		return mapRepositoryError(a.planRepo.UpdateStatus(ctx, plan.ID, "rejected"))
	}
	return mapRepositoryError(a.planRepo.UpdateStatus(ctx, plan.ID, "pending_user"))
}

// planHasActionableTrade reports whether a plan has at least one
// action that will move capital when executed. Watch / hold actions
// and zero-amount/zero-quantity rows don't count — they're audit
// records, not trades. The auto-execute gate consults this to skip
// guardrails on watch-only plans (a deliberate PM "no-op today"
// verdict shouldn't be marked rejected just because the LLM's
// confidence happened to land below the floor).
func planHasActionableTrade(actions []repository.PlanAction) bool {
	for _, action := range actions {
		switch strings.ToLower(strings.TrimSpace(action.Action)) {
		case "buy", "sell", "reduce", "add":
			// An action only moves capital if at least one of
			// amount/quantity is non-zero. We check both because
			// reduce/sell paths sometimes carry quantity without
			// amount when the executor is expected to derive
			// notional from live price at fill time.
			if math.Abs(action.Amount.Float64) > 1e-6 || action.Quantity.Float64 > 0 {
				return true
			}
		}
	}
	return false
}

// autoExecuteDecision is the structured outcome of evaluating the
// auto-execute guardrails for a single plan. Exists so we can write
// it into the plan's risk_review JSON whether the gate passed or
// failed, and so the test suite can assert on the specific guardrail
// that fired (instead of grepping a Chinese sentence).
type autoExecuteDecision struct {
	enabled       bool
	passed        bool
	reason        string  // human-readable, surfaced to the UI
	reasonCode    string  // stable enum for tests / metrics
	planNotional  float64 // sum |amount| in this plan
	planPctNAV    float64 // planNotional / totalAssets
	dailyNotional float64 // sum |amount| of already-auto-approved plans today
	dailyPctNAV   float64 // dailyNotional / totalAssets
	confidence    float64 // plan-level confidence from risk_review JSON
}

// timeNow returns the gateway clock. Indirected so tests can freeze
// "today" for the daily-cumulative window assertion.
func (a *runtimeApprovalGateway) timeNow() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now().UTC()
}

// evaluateAutoExecute is the dispatcher. It loads the fund + plan +
// actions, normalizes the per-fund auto-execute config and runs each
// guardrail. Errors during loading downgrade to "skip auto-execute" —
// we never block a plan from going to pending_user just because we
// failed to look up its fund row. Returns (cfg, fund, dbPlan, actions,
// decision) — callers use cfg + dbPlan + actions for the audit write.
func (a *runtimeApprovalGateway) evaluateAutoExecute(
	ctx context.Context,
	plan *workflow.InvestmentPlanResult,
) (api.FundAutoExecuteConfig, *repository.Fund, *repository.InvestmentPlan, []repository.PlanAction, autoExecuteDecision) {
	resolvedDefaults := resolveAutoExecuteConfig(nil)
	if a.fundRepo == nil || a.planRepo == nil {
		return resolvedDefaults, nil, nil, nil, autoExecuteDecision{
			enabled:    false,
			passed:     false,
			reasonCode: "auto_execute_disabled",
		}
	}

	fund, err := a.fundRepo.GetByID(ctx, plan.FundID)
	if err != nil {
		return resolvedDefaults, nil, nil, nil, autoExecuteDecision{
			enabled:    false,
			passed:     false,
			reasonCode: "auto_execute_disabled",
		}
	}
	profile := decodeFundMarketProfile(fund.Config)
	autoCfg := resolveAutoExecuteConfig(profile.AutoExecute)
	if !autoCfg.Enabled {
		return autoCfg, fund, nil, nil, autoExecuteDecision{
			enabled:    false,
			passed:     false,
			reasonCode: "auto_execute_disabled",
		}
	}

	dbPlan, err := a.planRepo.GetByID(ctx, plan.ID)
	if err != nil || dbPlan == nil {
		return autoCfg, fund, dbPlan, nil, autoExecuteDecision{
			enabled:    true,
			passed:     false,
			reasonCode: "plan_load_failed",
			reason:     "无法加载方案数据",
		}
	}
	actions, err := a.planRepo.GetActions(ctx, plan.ID)
	if err != nil {
		return autoCfg, fund, dbPlan, actions, autoExecuteDecision{
			enabled:    true,
			passed:     false,
			reasonCode: "actions_load_failed",
			reason:     "无法加载方案动作",
		}
	}

	decision := a.runAutoExecuteGuardrails(ctx, autoCfg, fund, dbPlan, actions, profile)
	return autoCfg, fund, dbPlan, actions, decision
}

// runAutoExecuteGuardrails is the pure-ish guardrail engine: given the
// resolved config + loaded plan/fund/actions it returns the
// autoExecuteDecision. Kept separate from evaluateAutoExecute so unit
// tests can drive it with hand-built inputs.
func (a *runtimeApprovalGateway) runAutoExecuteGuardrails(
	ctx context.Context,
	cfg api.FundAutoExecuteConfig,
	fund *repository.Fund,
	dbPlan *repository.InvestmentPlan,
	actions []repository.PlanAction,
	profile fundMarketProfile,
) autoExecuteDecision {
	decision := autoExecuteDecision{enabled: true}

	// 0) No-actionable-trade fast path. A plan whose actions are all
	// watch/hold (or carry zero amount/qty) is a deliberate "monitor
	// only today" verdict from the PM — there is literally no trade
	// to gate. Subjecting it to the confidence floor / order caps /
	// daily caps is wrong on two counts:
	//   (a) Semantics: those gates exist to bound *capital movement*;
	//       a zero-notional plan moves nothing.
	//   (b) UX: forcing a watch-only plan through the confidence
	//       gate caused it to be marked status="rejected" whenever
	//       the LLM's plan-level confidence dipped under the floor
	//       (the storage fund's 5 most-recent plans before this fix
	//       all surfaced in the Decision Center as "已驳回" even
	//       though the PM was correctly choosing to wait for a
	//       cleaner setup). "Watch" should never read as "rejected"
	//       to the operator.
	// We early-return passed=true with a stable reasonCode so the
	// audit JSON makes the no-op explicit; downstream
	// trade_execution is a no-op for watch/hold actions anyway.
	if !planHasActionableTrade(actions) {
		decision.passed = true
		decision.reasonCode = "no_actionable_trade"
		decision.reason = "今日方案仅含观察/持有动作，无可执行交易（PM 主动选择观望）"
		return decision
	}

	totalAssets := fund.TotalAssets
	if totalAssets <= 0 {
		// Without a NAV anchor we can't evaluate %-of-assets caps, so
		// we conservatively refuse to bypass approval.
		decision.reason = "基金净值未就绪，无法计算护栏阈值"
		decision.reasonCode = "nav_unavailable"
		return decision
	}

	// 1) Market whitelist: every action's market must be in the
	// configured allowlist (if any). Plans cannot mix markets in
	// today's runtime, but we check per-action to be safe — a
	// stray allocation to an unrelated market should NOT silently
	// auto-execute.
	if len(cfg.AllowedMarkets) > 0 {
		allowed := make(map[string]struct{}, len(cfg.AllowedMarkets))
		for _, m := range cfg.AllowedMarkets {
			allowed[strings.ToLower(strings.TrimSpace(m))] = struct{}{}
		}
		fundMarket := strings.ToLower(strings.TrimSpace(profile.Market))
		if fundMarket != "" {
			if _, ok := allowed[fundMarket]; !ok {
				decision.reasonCode = "market_not_allowed"
				decision.reason = fmt.Sprintf("基金所属市场 %q 不在自动执行白名单内", profile.Market)
				return decision
			}
		}
		for _, action := range actions {
			am := strings.ToLower(strings.TrimSpace(action.Market.String))
			if am == "" {
				continue
			}
			if _, ok := allowed[am]; !ok {
				decision.reasonCode = "market_not_allowed"
				decision.reason = fmt.Sprintf("动作市场 %q 不在自动执行白名单内", action.Market.String)
				return decision
			}
		}
	}

	// 2) Per-order NAV cap: every action's |amount| stays under the
	// per-order ceiling. We check each action individually because the
	// cap is a "no single trade can move more than X%" guarantee, not
	// a sum.
	planNotional := 0.0
	maxOrderPct := DefaultAutoExecuteMaxOrderPctOfAssets
	if cfg.MaxOrderPctOfAssets != nil {
		maxOrderPct = *cfg.MaxOrderPctOfAssets
	}
	maxOrderAbs := maxOrderPct * totalAssets
	for _, action := range actions {
		amt := math.Abs(action.Amount.Float64)
		planNotional += amt
		if amt > maxOrderAbs+1e-6 {
			decision.planNotional = planNotional
			decision.planPctNAV = planNotional / totalAssets
			decision.reasonCode = "order_pct_exceeded"
			decision.reason = fmt.Sprintf("动作 %s 名义金额 %.2f 超过单笔自动执行上限 %.2f（%.1f%% NAV）", action.Symbol, amt, maxOrderAbs, maxOrderPct*100)
			return decision
		}
	}
	decision.planNotional = planNotional
	decision.planPctNAV = planNotional / totalAssets

	// 3) Daily cumulative cap: sum of already-auto-approved notional
	// today + this plan's notional must stay under the daily ceiling.
	// The day boundary is UTC midnight; we explicitly don't use the
	// market calendar because the cap is a "money I'm willing to let
	// you move per calendar day without supervision" promise. Using
	// the market calendar would let a fund auto-execute well into the
	// next session under the prior day's budget.
	now := a.timeNow()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)
	dailyAlready := 0.0
	if a.planRepo != nil {
		if sum, err := a.planRepo.SumAutoExecutedAmountForFundDay(ctx, fund.ID, dayStart, dayEnd); err == nil {
			dailyAlready = sum
		}
	}
	dailyTotal := dailyAlready + planNotional
	maxDailyPct := DefaultAutoExecuteMaxDailyPctOfAssets
	if cfg.MaxDailyPctOfAssets != nil {
		maxDailyPct = *cfg.MaxDailyPctOfAssets
	}
	maxDailyAbs := maxDailyPct * totalAssets
	decision.dailyNotional = dailyTotal
	decision.dailyPctNAV = dailyTotal / totalAssets
	if dailyTotal > maxDailyAbs+1e-6 {
		decision.reasonCode = "daily_pct_exceeded"
		decision.reason = fmt.Sprintf("加上本方案后今日自动执行累计 %.2f（%.1f%% NAV）将超过日累计上限 %.2f（%.1f%% NAV）", dailyTotal, decision.dailyPctNAV*100, maxDailyAbs, maxDailyPct*100)
		return decision
	}

	// 4) Plan-level confidence floor. confidence is written to
	// investment_plans.confidence by the LLM PMAgent (Phase 2A) and
	// also mirrored into risk_review for human-readable audit. On
	// legacy plans both signals are missing -> we treat it as 0 and
	// refuse to bypass approval, which is the desired conservative
	// fallback ("the system has no idea how confident it is, so the
	// human looks").
	planConfidence := extractPlanConfidence(dbPlan, actions)
	decision.confidence = planConfidence
	minConfidence := DefaultAutoExecuteMinConfidence
	if cfg.MinConfidence != nil {
		minConfidence = *cfg.MinConfidence
	}
	if planConfidence < minConfidence-1e-9 {
		decision.reasonCode = "confidence_below_floor"
		decision.reason = fmt.Sprintf("方案置信度 %.2f 低于自动执行下限 %.2f", planConfidence, minConfidence)
		return decision
	}

	decision.passed = true
	return decision
}

// extractPlanConfidence resolves the plan-level confidence that the
// auto-execute gate compares against MinConfidence.
//
// Resolution order:
//
//  1. Server-side enforcement of the system-prompt rule "plan-level
//     confidence is the lower bound across actions you actually want
//     executed; don't inflate it." When at least one executing-verb
//     action (buy/sell/add/reduce) carries a valid action-level
//     confidence, the minimum across those actions becomes
//     authoritative. This deliberately overrides whatever the PM
//     reported as plan-level confidence, because we've observed the
//     PM mis-applying the rule in two directions:
//       - under-pricing: averaging in watch/hold conf and reporting
//         plan_conf below the executing min (real prod incident on
//         OCS fund 2026-05-26T02:10 UTC — buy@0.74 + watch@0.50 →
//         plan_conf 0.55 → auto-rejected by the 0.60 floor; same
//         buy fired again 4h later with plan_conf=0.75 and filled);
//       - inflation: stamping plan_conf above what its weakest
//         executing action supports, slipping a marginal trade past
//         the gate.
//     Watch/hold confidences are intentionally excluded because the
//     prompt says they are not part of the bar.
//
//  2. dbPlan.Confidence (the typed column added in migration 033).
//     Always set when the LLM decision engine ran; used when the
//     plan has zero executing actions (pure watch/hold day) or every
//     executing action has null confidence.
//
//  3. risk_review JSON {"confidence": ...} as a fallback for plans
//     written before migration 033 (or by callers that haven't yet
//     been wired through PlanRepo.UpdateConfidence).
//
//  4. The arithmetic mean of all action-level confidences as a last
//     resort, so a legacy plan still has *some* number.
//
// Returns 0 when no signal is available — the gate then fails the
// confidence guardrail, which is the conservative outcome.
func extractPlanConfidence(plan *repository.InvestmentPlan, actions []repository.PlanAction) float64 {
	if execMin, ok := minExecutingActionConfidence(actions); ok {
		return execMin
	}
	if plan != nil && plan.Confidence.Valid {
		return plan.Confidence.Float64
	}
	if plan != nil && len(plan.RiskReview) > 0 {
		var parsed struct {
			Confidence *float64 `json:"confidence"`
		}
		if err := json.Unmarshal(plan.RiskReview, &parsed); err == nil && parsed.Confidence != nil {
			return *parsed.Confidence
		}
	}
	if len(actions) == 0 {
		return 0
	}
	sum := 0.0
	count := 0
	for _, action := range actions {
		if action.Confidence.Valid {
			sum += action.Confidence.Float64
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

// isExecutingAction reports whether an action verb actually moves
// capital. The PM's universe is {buy, sell, add, reduce, hold,
// watch}; only the first four place orders. Used by the auto-execute
// confidence floor and anywhere else that needs to separate
// "actually trading" from "advisory only".
func isExecutingAction(verb string) bool {
	switch strings.ToLower(strings.TrimSpace(verb)) {
	case "buy", "sell", "add", "reduce":
		return true
	default:
		return false
	}
}

// minExecutingActionConfidence returns the smallest action-level
// confidence across the subset of actions that will actually trade.
// When no such action carries a valid confidence (watch-only plan,
// legacy plan with null action conf, etc.) it reports ok=false so
// the caller can fall back to the PM-reported plan-level number.
func minExecutingActionConfidence(actions []repository.PlanAction) (float64, bool) {
	minConf := math.Inf(1)
	ok := false
	for _, a := range actions {
		if !a.Confidence.Valid {
			continue
		}
		if !isExecutingAction(a.Action) {
			continue
		}
		if a.Confidence.Float64 < minConf {
			minConf = a.Confidence.Float64
		}
		ok = true
	}
	if !ok {
		return 0, false
	}
	return minConf, true
}

// persistAutoExecuteAudit writes the gate decision into the plan's
// risk_review JSON so the web/miniapp approval modal can surface "why
// did/didn't this plan auto-execute". We append onto the existing
// risk_review document (RiskAgent has already populated it) instead
// of replacing it. Failure to write is non-fatal — see RequestApproval
// for the rationale.
func (a *runtimeApprovalGateway) persistAutoExecuteAudit(
	ctx context.Context,
	plan *workflow.InvestmentPlanResult,
	dbPlan *repository.InvestmentPlan,
	cfg api.FundAutoExecuteConfig,
	decision autoExecuteDecision,
	actions []repository.PlanAction,
	fund *repository.Fund,
) error {
	_ = actions
	_ = fund
	_ = ctx
	var existing map[string]any
	if dbPlan != nil && len(dbPlan.RiskReview) > 0 {
		_ = json.Unmarshal(dbPlan.RiskReview, &existing)
	}
	if existing == nil {
		existing = map[string]any{}
	}
	auditAt := a.timeNow().UTC().Format(time.RFC3339)
	existing["autoExecute"] = map[string]any{
		"enabled":       decision.enabled,
		"passed":        decision.passed,
		"reason":        decision.reason,
		"reasonCode":    decision.reasonCode,
		"planPctOfNav":  decision.planPctNAV,
		"dailyPctOfNav": decision.dailyPctNAV,
		"confidence":    decision.confidence,
		"thresholds": map[string]any{
			"maxOrderPctOfAssets":  cfg.MaxOrderPctOfAssets,
			"maxDailyPctOfAssets":  cfg.MaxDailyPctOfAssets,
			"minConfidence":        cfg.MinConfidence,
			"slippageBouncePolicy": cfg.SlippageBouncePolicy,
		},
		"at":     auditAt,
		"planId": plan.ID,
	}
	encoded, err := json.Marshal(existing)
	if err != nil {
		return err
	}
	if a.planRepo == nil {
		return nil
	}
	return a.planRepo.UpdateRiskReview(ctx, plan.ID, encoded)
}

func (a *runtimeApprovalGateway) WaitForDecision(ctx context.Context, planID string) (bool, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if a.isCurrent != nil && !a.isCurrent() {
			return false, context.Canceled
		}
		plan, err := a.planRepo.GetByID(ctx, planID)
		if err != nil {
			return false, mapRepositoryError(err)
		}
		switch plan.Status {
		case "approved", "completed", "executing":
			if a.isCurrent != nil && !a.isCurrent() {
				return false, context.Canceled
			}
			return true, nil
		case "rejected":
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (e *runtimeTradingEngine) Execute(ctx context.Context, planID string) error {
	plan, err := e.planRepo.GetByID(ctx, planID)
	if err != nil {
		return mapRepositoryError(err)
	}
	fund, err := e.fundRepo.GetByID(ctx, plan.FundID)
	if err != nil {
		return mapRepositoryError(err)
	}
	actions, err := e.planRepo.GetActions(ctx, planID)
	if err != nil {
		return mapRepositoryError(err)
	}
	if len(actions) == 0 {
		return api.ErrConflict
	}
	if skillContext := e.buildSkillContext(ctx, plan, actions); skillContext != "" {
		for i := range actions {
			actions[i].Reasoning = sql.NullString{String: appendSkillContext(actions[i].Reasoning.String, skillContext), Valid: true}
		}
		if err := e.syncPlanActionReasoning(ctx, planID, actions); err != nil {
			return err
		}
	}

	positions, err := e.positionRepo.ListByFund(ctx, fund.ID)
	if err != nil {
		return mapRepositoryError(err)
	}
	positionsByKey := make(map[string]repository.HoldingPosition, len(positions))
	for i := range positions {
		positionsByKey[positionMapKey(positions[i].InstrumentKey, positions[i].Symbol)] = positions[i]
	}
	hardRisk, err := e.buildHardRiskState(ctx, fund, plan)
	if err != nil {
		return err
	}

	availableCash := fund.CurrentCapital
	tradeStatusByAction := make(map[string]string, len(actions))
	var bounce *slippageBounceError
	bounceIdx := -1

	for i, action := range actions {
		status, execErr := e.executePlanAction(ctx, fund, plan, action, positionsByKey, &availableCash, hardRisk, executePlanActionOptions{})
		if action.ID != "" && status != "" {
			// Don't overwrite action status with 'pending' when we
			// bounced — leave the existing 'pending' DB row untouched
			// so the next approval re-runs this action.
			if !(execErr != nil && status == "pending") {
				tradeStatusByAction[action.ID] = status
			}
		}
		if execErr != nil {
			var b *slippageBounceError
			if errors.As(execErr, &b) {
				// Persist progress for actions already filled in this
				// pass and short-circuit the loop: any later action's
				// reference price is just as stale as this one's.
				bounce = b
				bounceIdx = i
				break
			}
			if updateErr := e.syncPlanActionStatuses(ctx, planID, tradeStatusByAction); updateErr != nil {
				return updateErr
			}
			// A hard-risk / quote / lot-size rejection (anything that's
			// NOT a slippage bounce) is terminal for the plan: the
			// rejected action will not re-run, so leaving plan.status
			// at "executing" wedges the row forever — that's the bug
			// that produced 4 stale plans on the storage fund. Flip to
			// "rejected" before returning so the workflow run finishes
			// in a clean terminal state and the dashboard stops
			// showing "executing" indefinitely. Best-effort: a write
			// failure here is logged but doesn't mask the original
			// execErr the caller really wants to see.
			if updateErr := e.planRepo.UpdateStatus(ctx, planID, "rejected"); updateErr != nil {
				slog.Error("trading engine: failed to flip plan to rejected after execution error",
					"plan_id", planID,
					"update_err", updateErr,
					"exec_err", execErr,
				)
			}
			return execErr
		}
	}

	if bounce != nil {
		// Slippage bounced one action. The default behaviour (and the
		// only behaviour for human-approved plans) is to send the plan
		// back to pending_user. Auto-executed plans can opt into two
		// other behaviours via fund.config.autoExecute.slippageBouncePolicy:
		//   - "reject":         mark all still-pending actions rejected
		//                       and set plan.status = rejected. Nothing
		//                       fills at a stale-vs-live mismatch.
		//   - "force_execute":  retry the bouncing action (and every
		//                       still-pending one after it) with the
		//                       SlippageGuard rule disabled, filling at
		//                       the live quote regardless of drift.
		// The detection of "this plan was auto-executed" looks at the
		// AutoExecutedAt column on the bouncing action; that column is
		// stamped by the gateway before flipping plan.status =
		// approved.
		policy := slippageBouncePolicyForPlan(fund, actions)
		switch policy {
		case "reject":
			for j := bounceIdx; j < len(actions); j++ {
				if actions[j].ID == "" {
					continue
				}
				if terminalActionStatus(actions[j].ExecutionStatus) != "" {
					continue
				}
				tradeStatusByAction[actions[j].ID] = "rejected"
			}
			if err := e.syncPlanActionStatuses(ctx, planID, tradeStatusByAction); err != nil {
				return err
			}
			if err := e.persistPortfolioState(ctx, fund, positionsByKey, availableCash, plan.TradingDate); err != nil {
				return err
			}
			return mapRepositoryError(e.planRepo.UpdateStatus(ctx, planID, "rejected"))
		case "force_execute":
			for j := bounceIdx; j < len(actions); j++ {
				action := actions[j]
				if terminalActionStatus(action.ExecutionStatus) != "" {
					continue
				}
				status, execErr := e.executePlanAction(ctx, fund, plan, action, positionsByKey, &availableCash, hardRisk, executePlanActionOptions{skipSlippage: true})
				if action.ID != "" && status != "" {
					if !(execErr != nil && status == "pending") {
						tradeStatusByAction[action.ID] = status
					}
				}
				if execErr != nil {
					// A non-slippage hard-risk error during force-execute
					// still rejects (not bounces) — the same gate that
					// rejects in manual mode rejects here. We never
					// loop on slippage again because skipSlippage=true.
					if updateErr := e.syncPlanActionStatuses(ctx, planID, tradeStatusByAction); updateErr != nil {
						return updateErr
					}
					return execErr
				}
			}
			if err := e.syncPlanActionStatuses(ctx, planID, tradeStatusByAction); err != nil {
				return err
			}
			if err := e.persistPortfolioState(ctx, fund, positionsByKey, availableCash, plan.TradingDate); err != nil {
				return err
			}
			return mapRepositoryError(e.planRepo.UpdateStatus(ctx, planID, "completed"))
		default:
			// "bounce_to_user" — preserve historical behavior.
			if err := e.syncPlanActionStatuses(ctx, planID, tradeStatusByAction); err != nil {
				return err
			}
			if err := e.persistPortfolioState(ctx, fund, positionsByKey, availableCash, plan.TradingDate); err != nil {
				return err
			}
			return mapRepositoryError(e.planRepo.UpdateStatus(ctx, planID, "pending_user"))
		}
	}

	if err := e.syncPlanActionStatuses(ctx, planID, tradeStatusByAction); err != nil {
		return err
	}
	if err := e.persistPortfolioState(ctx, fund, positionsByKey, availableCash, plan.TradingDate); err != nil {
		return err
	}
	return mapRepositoryError(e.planRepo.UpdateStatus(ctx, planID, "completed"))
}

// executePlanActionOptions toggles per-call behaviour for
// executePlanAction. Used today only by the slippage-bounce
// "force_execute" policy in Execute(), which re-runs a bouncing action
// without the SlippageGuard tripwire.
type executePlanActionOptions struct {
	// skipSlippage drops the SlippageGuard rule from the hard-risk
	// policy chain for this single action evaluation. It does NOT
	// affect any other rule (T+1, lot size, NAV cap, stale-quote, ...).
	skipSlippage bool
}

// slippageBouncePolicyForPlan returns the policy to apply when a
// slippage bounce fires during Execute. The policy lives on the fund's
// auto-execute config (slippageBouncePolicy field). We only honour
// non-default policies when the plan was actually auto-executed; a
// human-approved plan always bounces to pending_user regardless of the
// fund's auto-execute config — the human already chose to look at it.
func slippageBouncePolicyForPlan(fund *repository.Fund, actions []repository.PlanAction) string {
	if fund == nil {
		return DefaultAutoExecuteSlippageBouncePolicy
	}
	if !planWasAutoExecuted(actions) {
		return DefaultAutoExecuteSlippageBouncePolicy
	}
	profile := decodeFundMarketProfile(fund.Config)
	cfg := resolveAutoExecuteConfig(profile.AutoExecute)
	if !cfg.Enabled {
		return DefaultAutoExecuteSlippageBouncePolicy
	}
	policy := strings.TrimSpace(cfg.SlippageBouncePolicy)
	if _, ok := validAutoExecuteSlippagePolicies[policy]; !ok || policy == "" {
		return DefaultAutoExecuteSlippageBouncePolicy
	}
	return policy
}

func planWasAutoExecuted(actions []repository.PlanAction) bool {
	for _, a := range actions {
		if a.AutoExecutedAt.Valid {
			return true
		}
	}
	return false
}

// terminalActionStatus returns the input status if it represents a
// terminal state (already filled, cancelled, or rejected) and the empty
// string otherwise. Used by executePlanAction to early-return when the
// caller is replaying after a bounce.
func terminalActionStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "filled", "cancelled", "rejected":
		return strings.ToLower(strings.TrimSpace(s))
	default:
		return ""
	}
}

// ReportPartialFill implements workflow.PartialFillReporter (Sprint 3 / L2).
// Returns true when the plan has BOTH successful and failed plan_actions.
// "Successful" = execution_status ∈ {filled, executed, partial}. "Failed"
// = execution_status ∈ {failed, cancelled, rejected}. Pending / blank
// status counts toward neither — partial fail is only stamped when we
// have hard evidence of at least one fill and at least one miss.
func (e *runtimeTradingEngine) ReportPartialFill(ctx context.Context, planID string) (bool, error) {
	if e == nil || e.planRepo == nil {
		return false, nil
	}
	actions, err := e.planRepo.GetActions(ctx, planID)
	if err != nil {
		return false, fmt.Errorf("partial-fill: list actions: %w", err)
	}
	hasSuccess := false
	hasFailure := false
	for _, a := range actions {
		status := strings.ToLower(strings.TrimSpace(a.ExecutionStatus))
		switch status {
		case "filled", "executed", "partial":
			hasSuccess = true
		case "failed", "cancelled", "rejected":
			hasFailure = true
		}
		if hasSuccess && hasFailure {
			return true, nil
		}
	}
	return false, nil
}

func (e *runtimeTradingEngine) Settle(ctx context.Context, fundID string, tradingDate string) error {
	fund, err := e.fundRepo.GetByID(ctx, fundID)
	if err != nil {
		return mapRepositoryError(err)
	}
	positions, err := e.positionRepo.ListByFund(ctx, fundID)
	if err != nil {
		return mapRepositoryError(err)
	}
	positionsByKey := make(map[string]repository.HoldingPosition, len(positions))
	for i := range positions {
		positionsByKey[positionMapKey(positions[i].InstrumentKey, positions[i].Symbol)] = positions[i]
	}
	// Settle is the canonical "next trading day" boundary in the
	// simulator: it's where T+1 markets (A-share) release the lock
	// on shares bought during the prior session. The unlock step
	// runs *before* persistence so the released AvailableQty values
	// are what gets written back to holding_positions. T+0 markets
	// are a no-op here (their AvailableQty already mirrored Quantity).
	releaseLockedShares(positionsByKey)
	latest, err := e.navRepo.GetLatest(ctx, fundID)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return mapRepositoryError(err)
	}
	availableCash := fund.CurrentCapital
	if latest != nil {
		availableCash = latest.AvailableCash
	}
	return e.persistPortfolioState(ctx, fund, positionsByKey, availableCash, parseTradingDateOrNow(tradingDate))
}

func (e *runtimeTradingEngine) executePlanAction(
	ctx context.Context,
	fund *repository.Fund,
	plan *repository.InvestmentPlan,
	action repository.PlanAction,
	positionsByKey map[string]repository.HoldingPosition,
	availableCash *float64,
	hardRisk *hardRiskState,
	opts executePlanActionOptions,
) (string, error) {
	// Idempotent re-entry: if a previous Execute pass already terminated
	// this action (filled / rejected / cancelled) we surface that status
	// without re-pulling a quote or re-debiting cash. This lets the
	// Execute loop safely retry after a slippage bounce sets the plan
	// back to pending_user — only actions still in 'pending' run.
	if terminal := terminalActionStatus(action.ExecutionStatus); terminal != "" {
		return terminal, nil
	}

	side, quantity, planPrice, amount := normalizeExecutionAction(action)
	if side == "" || quantity <= 0 {
		return "cancelled", nil
	}

	// Always pull a fresh quote at execution time. Two reasons:
	//   1. SlippageGuard needs the live price to compare against the
	//      plan's reference price (planPrice) and decide whether to
	//      execute, rewrite, or bounce.
	//   2. StaleQuoteGuard needs the live AsOf/IsStale signal even when
	//      the plan already carries a price.
	// The market-data layer caches each symbol with a short TTL so this
	// extra call is cheap.
	var (
		quoteAsOf      time.Time
		quoteIsStale   bool
		executionPrice float64
	)
	if quote, err := e.quoteForAction(ctx, fund, action); err == nil && quote != nil && quote.Price > 0 {
		executionPrice = quote.Price
		quoteAsOf = quote.AsOf
		quoteIsStale = quote.IsStale
	} else if planPrice <= 0 {
		// No live quote AND no plan-time price: cannot price this trade
		// at all. Treat as an unavailable-quote rejection (existing
		// behavior).
		return "rejected", marketdata.ErrQuoteUnavailable
	}

	// orderPrice is the price we'll actually execute at and record as
	// filled_price. For risk-increasing trades (buy/add) we rewrite it
	// to the live quote so the simulation reflects the actual market
	// state at approval/execution time; SlippageGuard separately gates
	// large drifts. For sells/reduces we keep the plan price to mirror
	// "I'm willing to sell at $X" semantics — slippage on the way down
	// doesn't change the intent to de-risk.
	orderPrice := planPrice
	if planPrice <= 0 {
		// First-time pricing: the plan had no price at all (e.g.
		// quote-unavailable at generation time). The live quote becomes
		// both the reference price and the execution price; slippage
		// is N/A in this case (computeSlippagePct returns NULL when
		// planPrice <= 0).
		orderPrice = executionPrice
		planPrice = executionPrice
	} else if side != "sell" && executionPrice > 0 {
		orderPrice = executionPrice
	}
	amount = roundCurrency(float64(quantity) * orderPrice)
	if isFuturesAction(action) {
		amount = roundCurrency(float64(quantity) * orderPrice * contractMultiplierValue(action.ContractMultiplier))
	}

	status := "filled"
	filledPrice := sql.NullFloat64{Float64: orderPrice, Valid: orderPrice > 0}
	feeCommission := roundCurrency(amount * 0.001)
	feeStampTax := 0.0
	feeTransfer := roundCurrency(float64(quantity) * 0.0002)
	if side == "sell" {
		feeStampTax = roundCurrency(amount * 0.001)
	}

	positionKey := positionMapKey(action.InstrumentKey, action.Symbol)
	position := positionsByKey[positionKey]
	// enforceHardRiskGate receives the plan price as `price` (so notional
	// caps and exposure rules are evaluated against the user-approved
	// reference) and the live price as `executionPrice` (so SlippageGuard
	// can compute drift). Both signals flow into the same ProposedTrade.
	if err := enforceHardRiskGate(fund, plan, action, positionsByKey, hardRisk, side, quantity, planPrice, amount, quoteAsOf, quoteIsStale, executionPrice, opts.skipSlippage); err != nil {
		var bounce *slippageBounceError
		if errors.As(err, &bounce) {
			if e.metrics != nil {
				e.metrics.RecordHardRiskRejection(bounce.Rule, action.Symbol)
			}
			// Bounce surfaces as a recoverable error so the caller can
			// transition the plan back to pending_user. The action
			// status stays 'pending' (this run made no change to it).
			return "pending", err
		}
		var rejection *hardRiskRejectionError
		if errors.As(err, &rejection) && e.metrics != nil {
			e.metrics.RecordHardRiskRejection(rejection.Rule, action.Symbol)
		}
		return "rejected", err
	}

	// S12-followup (2026-06-04): mirror the four broker-side
	// regulatory gates (market-status, lockup, borrow, price-
	// collar) on the PM-direct-fill path BEFORE the equity /
	// futures branch split. Without this, after-hours / halted
	// / fat-finger / borrow-denied orders silently filled
	// because tradeRepoCreateAndFill bypasses broker.Simulator.
	// LotSizeGate (the fifth simulator gate) is kept on the
	// faster in-memory pmPathLotSizeGuard path further down,
	// after the cash / qty availability checks.
	clientOrderID := mintTradeIdempotencyKey(action.ID, side, quantity).String
	if _, gateErr := e.pmPathPreTradeGateChain(ctx, fund, action, side, quantity, orderPrice, clientOrderID); gateErr != nil {
		return "rejected", gateErr
	}

	if isFuturesAction(action) {
		status, err := e.executeFuturesPlanAction(ctx, fund, plan, action, positionKey, position, positionsByKey, availableCash, side, quantity, planPrice, orderPrice, amount, status, filledPrice, feeCommission, feeStampTax, feeTransfer)
		if err == nil {
			recordHardRiskTrade(hardRisk, action, side, quantity, orderPrice, amount, status)
		}
		return status, err
	}
	// Sprint 1 / S6: for TWAP / VWAP requested strategies, slice the
	// order into N child fills with a simulated intraday-VWAP-anchored
	// execution price. The simulator stays single-trade-per-DB-row
	// (slicing across rows would explode the audit trail), but the
	// orderPrice we record is the SLICE-AVERAGED VWAP, which gives the
	// LLM PM honest feedback that "asking for TWAP on a 20% NAV buy
	// would have got me X bps better than immediate."
	if priceAdj, sliced := e.applyStrategyExecutionPrice(action, side, orderPrice, planPrice); sliced {
		orderPrice = priceAdj
		amount = roundCurrency(float64(quantity) * orderPrice)
		feeCommission = roundCurrency(amount * 0.001)
		if side == "sell" {
			feeStampTax = roundCurrency(amount * 0.001)
		}
		filledPrice = sql.NullFloat64{Float64: orderPrice, Valid: orderPrice > 0}
	}

	// Trader-style execution strategy label (B-step1: record only, no
	// child-order splitting yet). The PM-direct-fill engine still
	// emits one trade_execution per action; this just tags the row
	// with the strategy a real TraderAgent would have picked, so
	// analytics + the daily-review LLM can reason about execution
	// intent ("today's trader logged TWAP intent on a 4000-share buy
	// that filled at +3bps"). See pm_path_execution_strategy.go.
	strategy := selectPMPathExecutionStrategy(action, quantity)

	if side == "buy" {
		totalDebit := amount + feeCommission + feeTransfer + feeStampTax
		if totalDebit > *availableCash+0.0001 {
			return "rejected", api.ErrConflict
		}
		if err := e.pmPathLotSizeGuard(side, action, quantity, orderPrice, position); err != nil {
			return "rejected", err
		}
		// Equity buy: no realized PnL on an open.
		rolledStatus, err := e.tradeRepoCreateAndFill(ctx, fund, plan, action, side, quantity, planPrice, amount, status, filledPrice, feeCommission, feeStampTax, feeTransfer, strategy, sql.NullFloat64{})
		if err != nil {
			return "rejected", err
		}
		position = mergeBoughtPosition(position, fund.ID, action, quantity, orderPrice)
		positionsByKey[positionKey] = position
		*availableCash = roundCurrency(*availableCash - totalDebit)
		recordHardRiskTrade(hardRisk, action, side, quantity, orderPrice, amount, status)
		// rolledStatus is non-empty only when the splitter path
		// produced an aggregated label for this plan_action. Surface
		// it so the caller writes the aggregate, not the parent intent.
		if rolledStatus != "" {
			return rolledStatus, nil
		}
		return status, nil
	}

	availableQty := int(position.AvailableQty)
	if availableQty < quantity {
		return "rejected", api.ErrConflict
	}
	if err := e.pmPathLotSizeGuard(side, action, quantity, orderPrice, position); err != nil {
		return "rejected", err
	}
	// Equity sell: T7's realizedPnL leg is futures-only, so equity
	// sells always pass the zero value. (Equity realized PnL is
	// already captured by the trade_sell_notional credit + the
	// FIFO lot ledger; v2 doesn't change that.)
	rolledStatus, err := e.tradeRepoCreateAndFill(ctx, fund, plan, action, side, quantity, planPrice, amount, status, filledPrice, feeCommission, feeStampTax, feeTransfer, strategy, sql.NullFloat64{})
	if err != nil {
		return "rejected", err
	}
	remainingQty := position.Quantity - float64(quantity)
	if remainingQty <= 0.0001 {
		delete(positionsByKey, positionKey)
	} else {
		position.Quantity = remainingQty
		position.AvailableQty = remainingQty
		position.CurrentPrice = orderPrice
		position.MarketValue = roundCurrency(remainingQty * orderPrice)
		positionsByKey[positionKey] = position
	}
	netCredit := amount - feeCommission - feeTransfer - feeStampTax
	*availableCash = roundCurrency(*availableCash + netCredit)
	recordHardRiskTrade(hardRisk, action, side, quantity, orderPrice, amount, status)
	if rolledStatus != "" {
		return rolledStatus, nil
	}
	return status, nil
}

// applyStrategyExecutionPrice is the Sprint 1 / S6 execution-strategy
// dispatch. It returns (adjustedPrice, sliced=true) when the action's
// strategy hint asks for a TWAP/VWAP simulation, and (orderPrice, false)
// otherwise so the caller knows whether to re-derive notional / fees.
//
// We model a TWAP/VWAP fill in the simulator as a single DB row whose
// filled_price equals the average of N slices (default 5) of the day's
// reference price ± a small random slippage per slice. The average is
// what the live broker would have reported back; keeping it to one DB
// row preserves the audit trail and avoids cascading changes through
// position lots / NAV snapshots. The slippage budget is bounded by the
// per-strategy cap so a buggy or hostile plan can't move the simulated
// price arbitrarily.
//
// strategy hint resolution:
//   - "twap" / "vwap"   → slice and average
//   - "limit"           → reserved; falls back to immediate (the limit-
//                          book simulator lands in Sprint 5).
//   - "" / "immediate"  → no change
//   - unknown values    → no change (forward-compatible)
func (e *runtimeTradingEngine) applyStrategyExecutionPrice(action repository.PlanAction, side string, orderPrice, planPrice float64) (float64, bool) {
	if orderPrice <= 0 {
		return orderPrice, false
	}
	strategy := strings.ToLower(strings.TrimSpace(action.Strategy.String))
	if strategy != "twap" && strategy != "vwap" {
		return orderPrice, false
	}
	const (
		slices       = 5
		maxSlipPerSlice = 0.0015 // ±15 bps per slice
	)
	// Anchor the slice-average drift around 1× orderPrice so the
	// simulator reflects "fills came in close to the day's VWAP".
	// Use a deterministic RNG seeded from the action ID + symbol so
	// repeated executes of the same plan return identical fills (the
	// audit replay path depends on this).
	seed := stableSeedFromActionID(action.ID, action.Symbol)
	rng := newDeterministicRNG(seed)
	total := 0.0
	for i := 0; i < slices; i++ {
		// Each slice's price = orderPrice × (1 + bias) where bias is
		// in [-maxSlipPerSlice, +maxSlipPerSlice]. Buys average
		// SLIGHTLY above the day's price (you pay the spread), sells
		// SLIGHTLY below — matching empirical TWAP slippage profiles.
		bias := (rng.NextFloat64()*2 - 1) * maxSlipPerSlice
		if side == "buy" {
			bias += maxSlipPerSlice * 0.20 // small directional bias
		} else if side == "sell" {
			bias -= maxSlipPerSlice * 0.20
		}
		total += orderPrice * (1 + bias)
	}
	avgPrice := total / float64(slices)
	// Round to 4 dp so the persisted price doesn't look fake-precise.
	avgPrice = math.Round(avgPrice*10000) / 10000
	_ = planPrice
	return avgPrice, true
}

// stableSeedFromActionID hashes the action ID + symbol into a
// 64-bit seed so the TWAP/VWAP simulator is deterministic per action.
// A fresh execution call on the same approved action returns the
// same fill price; useful for audit replay and for the "rerun a
// failed step" recovery path.
func stableSeedFromActionID(actionID, symbol string) uint64 {
	var seed uint64 = 14695981039346656037 // FNV-1a 64 offset
	for _, b := range []byte(actionID) {
		seed ^= uint64(b)
		seed *= 1099511628211
	}
	for _, b := range []byte(symbol) {
		seed ^= uint64(b)
		seed *= 1099511628211
	}
	if seed == 0 {
		seed = 1
	}
	return seed
}

// deterministicRNG is a tiny xorshift64* used only by the strategy
// simulator. We avoid the standard math/rand global to keep the
// simulator's results deterministic per action even when many
// goroutines share the engine.
type deterministicRNG struct {
	state uint64
}

func newDeterministicRNG(seed uint64) *deterministicRNG {
	if seed == 0 {
		seed = 1
	}
	return &deterministicRNG{state: seed}
}

func (r *deterministicRNG) next() uint64 {
	r.state ^= r.state >> 12
	r.state ^= r.state << 25
	r.state ^= r.state >> 27
	return r.state * 2685821657736338717
}

func (r *deterministicRNG) NextFloat64() float64 {
	return float64(r.next()>>11) / (1 << 53)
}

func (e *runtimeTradingEngine) buildHardRiskState(ctx context.Context, fund *repository.Fund, plan *repository.InvestmentPlan) (*hardRiskState, error) {
	if fund == nil || plan == nil {
		return &hardRiskState{}, nil
	}
	tradingDate := normalizeTradingDate(plan.TradingDate)
	state := &hardRiskState{TradingDate: tradingDate, TotalAssets: fund.TotalAssets}
	if state.TotalAssets <= 0 {
		state.TotalAssets = fund.CurrentCapital
	}

	if e.navRepo != nil {
		latest, err := e.navRepo.GetLatest(ctx, fund.ID)
		if err != nil && !errors.Is(err, repository.ErrNotFound) {
			return nil, mapRepositoryError(err)
		}
		if latest != nil {
			if latest.TotalAssets > 0 {
				state.TotalAssets = latest.TotalAssets
			}
			if sameTradingDate(latest.TradingDate, tradingDate) {
				state.DailyReturn = latest.DailyReturn
			}
		}
	}
	if state.TotalAssets <= 0 {
		state.TotalAssets = 1
	}
	profile := decodeFundMarketProfile(fund.Config)
	state.Policy = risk.HardRiskPolicyFromConfig(riskHardConfigFromAPI(profile.HardRisk))

	if e.tradeRepo != nil {
		from := tradingDate
		to := tradingDate.Add(24*time.Hour - time.Nanosecond)
		trades, err := e.tradeRepo.ListByFund(ctx, fund.ID, from, to, 10000)
		if err != nil {
			return nil, mapRepositoryError(err)
		}
		state.TradesToday = convertRepositoryTradesToRisk(trades)
	}
	return state, nil
}

func enforceHardRiskGate(
	fund *repository.Fund,
	plan *repository.InvestmentPlan,
	action repository.PlanAction,
	positionsByKey map[string]repository.HoldingPosition,
	state *hardRiskState,
	side string,
	quantity int,
	price float64,
	amount float64,
	quoteAsOf time.Time,
	quoteIsStale bool,
	executionPrice float64,
	skipSlippage bool,
) error {
	if state == nil {
		state = &hardRiskState{}
	}
	totalAssets := state.TotalAssets
	if totalAssets <= 0 && fund != nil {
		totalAssets = fund.TotalAssets
	}
	if totalAssets <= 0 {
		totalAssets = 1
	}
	planID := ""
	if plan != nil {
		planID = plan.ID
	}
	pc := risk.PlanContext{
		PlanID:      planID,
		TotalAssets: totalAssets,
		Positions:   convertPositionsToRisk(positionsByKey),
		Trades: []risk.ProposedTrade{{
			Symbol:         action.Symbol,
			Side:           riskSide(side),
			Quantity:       float64(quantity),
			Price:          price,
			Amount:         amount,
			Sector:         riskSectorForAction(action),
			QuoteAsOf:      quoteAsOf,
			QuoteIsStale:   quoteIsStale,
			ExecutionPrice: executionPrice,
			Market:         action.Market.String,
			Exchange:       action.Exchange.String,
			AssetClass:     action.AssetClass.String,
		}},
		TradesToday: state.TradesToday,
		DailyReturn: state.DailyReturn,
	}
	policy := risk.DefaultHardRiskPolicy()
	if state.Policy.Name != "" || len(state.Policy.Rules) > 0 {
		policy = state.Policy
	}
	// skipSlippage drops the SlippageGuard rule from this single
	// evaluation. Used by the trading engine when re-executing under
	// the auto-execute "force_execute" slippage policy: the user has
	// configured "fill at live regardless of drift", so the gate must
	// not surface the drift as a fail. Every other rule (T+1, lot
	// size, NAV cap, ...) stays on.
	if skipSlippage {
		filtered := make([]risk.Rule, 0, len(policy.Rules))
		for _, r := range policy.Rules {
			if r == nil {
				continue
			}
			if r.Name() == risk.SlippageGuardRuleName {
				continue
			}
			filtered = append(filtered, r)
		}
		policy = risk.Policy{Name: policy.Name, Rules: filtered}
	}
	report := risk.NewEvaluator(policy).Evaluate(context.Background(), pc)
	if !report.HasFail() {
		return nil
	}
	// Slippage failures are signaled with a typed error so the trading
	// engine can bounce the plan back to pending_user (giving the user
	// a chance to refresh and re-approve) instead of permanently
	// rejecting the action like other hard-risk failures.
	if slip := findSlippageFail(report.Findings); slip != nil {
		return &slippageBounceError{
			Rule:      slip.Rule,
			Symbol:    action.Symbol,
			Drift:     slip.Current,
			Tolerance: slip.Threshold,
			PlanPrice: price,
			LivePrice: executionPrice,
			Message:   fmt.Sprintf("plan bounced for %s: %s", action.Symbol, slip.Message),
		}
	}
	rule, summary := hardRiskFailureSummary(report.Findings)
	return &hardRiskRejectionError{Rule: rule, Symbol: action.Symbol, Message: fmt.Sprintf("hard risk gate rejected %s: %s", action.Symbol, summary)}
}

func findSlippageFail(findings []risk.Finding) *risk.Finding {
	for i := range findings {
		f := &findings[i]
		if f.Rule == risk.SlippageGuardRuleName && f.Severity == risk.SeverityFail {
			return f
		}
	}
	return nil
}

type hardRiskRejectionError struct {
	Rule    string
	Symbol  string
	Message string
}

func (e *hardRiskRejectionError) Error() string {
	if e == nil {
		return "hard risk gate rejected"
	}
	return e.Message
}

// slippageBounceError is returned by enforceHardRiskGate when the
// SlippageGuard rule fires with SeverityFail. The trading engine
// treats this as a recoverable "bounce" — it persists any actions
// that already filled, then resets the plan back to pending_user so
// the user can refresh the quote and re-approve.
type slippageBounceError struct {
	Rule      string  // always risk.SlippageGuardRuleName
	Symbol    string  // the symbol whose drift exceeded tolerance
	Drift     float64 // signed fractional drift, e.g. 0.0123 = +1.23%
	Tolerance float64 // configured tolerance, e.g. 0.008 = 0.8%
	PlanPrice float64 // reference price the plan was approved at
	LivePrice float64 // live quote price at execution time
	Message   string
}

func (e *slippageBounceError) Error() string {
	if e == nil {
		return "slippage bounce"
	}
	return e.Message
}

func hardRiskFailureSummary(findings []risk.Finding) (string, string) {
	parts := make([]string, 0)
	firstRule := "unknown"
	for _, finding := range findings {
		if finding.Severity != risk.SeverityFail {
			continue
		}
		if firstRule == "unknown" && strings.TrimSpace(finding.Rule) != "" {
			firstRule = finding.Rule
		}
		if strings.TrimSpace(finding.Message) != "" {
			parts = append(parts, finding.Message)
			continue
		}
		parts = append(parts, finding.Rule)
	}
	if len(parts) == 0 {
		return firstRule, "unknown hard risk failure"
	}
	return firstRule, strings.Join(parts, "; ")
}

func recordHardRiskTrade(state *hardRiskState, action repository.PlanAction, side string, quantity int, price float64, amount float64, status string) {
	if state == nil {
		return
	}
	state.TradesToday = append(state.TradesToday, risk.ExecutedTrade{
		Symbol:     action.Symbol,
		Side:       riskSide(side),
		Quantity:   float64(quantity),
		Price:      price,
		Amount:     amount,
		Status:     status,
		ExecutedAt: time.Now().UTC(),
	})
}

func convertPositionsToRisk(positionsByKey map[string]repository.HoldingPosition) []risk.Position {
	positions := make([]risk.Position, 0, len(positionsByKey))
	for _, position := range positionsByKey {
		positions = append(positions, risk.Position{
			Symbol:       position.Symbol,
			Sector:       riskSectorForPosition(position),
			Quantity:     position.Quantity,
			AvgCost:      position.CostPrice,
			MarketPrice:  position.CurrentPrice,
			MarketValue:  position.MarketValue,
			AvailableQty: position.AvailableQty,
		})
	}
	sort.Slice(positions, func(i, j int) bool { return positions[i].Symbol < positions[j].Symbol })
	return positions
}

func convertRepositoryTradesToRisk(trades []repository.TradeExecution) []risk.ExecutedTrade {
	out := make([]risk.ExecutedTrade, 0, len(trades))
	for _, trade := range trades {
		price := 0.0
		if trade.Price.Valid {
			price = trade.Price.Float64
		}
		amount := 0.0
		if trade.Amount.Valid {
			amount = trade.Amount.Float64
		}
		executedAt := trade.CreatedAt
		if trade.ExecutedAt.Valid {
			executedAt = trade.ExecutedAt.Time
		}
		out = append(out, risk.ExecutedTrade{
			Symbol:     trade.Symbol,
			Side:       riskSide(trade.Side),
			Quantity:   trade.Quantity,
			Price:      price,
			Amount:     amount,
			Status:     trade.Status,
			ExecutedAt: executedAt,
		})
	}
	return out
}

func riskSide(side string) risk.Side {
	switch strings.ToLower(strings.TrimSpace(side)) {
	case "sell", "reduce", "close_short", "close_long":
		return risk.SideSell
	default:
		return risk.SideBuy
	}
}

func riskSectorForAction(action repository.PlanAction) string {
	return firstNonEmptyValue(action.AssetClass.String, action.InstrumentType.String, action.Market.String)
}

func riskSectorForPosition(position repository.HoldingPosition) string {
	return firstNonEmptyValue(position.AssetClass.String, position.InstrumentType.String, position.Market.String)
}

func (e *runtimeTradingEngine) buildSkillContext(ctx context.Context, plan *repository.InvestmentPlan, actions []repository.PlanAction) string {
	if plan == nil {
		return ""
	}
	traderAgent := findFundAgentByRoleWithFocus(ctx, plan.FundID, string(workflow.RoleTrader), "", e.teamRepo, e.agentRepo, e.fundRepo)
	context := buildTraderSkillContext(LanguageFromContext(ctx), traderAgent, plan, actions)
	fundFocusContext, specializationContext := buildRuntimeFundContextsByID(ctx, plan.FundID, traderAgent, e.fundRepo)
	context = appendSkillContext(context, fundFocusContext)
	return appendSkillContext(context, specializationContext)
}

// pmPathLotSizeGuard is the PM-direct-fill counterpart to
// broker.LotSizeGate (S12.1). The broker-side gate only catches
// orders that flow through Simulator.SubmitOrder; the runtime
// trading engine's "direct fill" path (executePlanAction →
// tradeRepoCreateAndFill, written for fast plan-action settlement)
// previously skipped that gate entirely, so an A-share order whose
// quantity violated the board's MinLot/Step rules could land in
// trade_executions unchallenged.
//
// Trigger story: 2026-06-03 the OCS A-share fund (STAR-Market
// instruments 688205 / 688195) accumulated 105 / 283-share
// fractional / odd-lot residuals because the PM sized partial sells
// (62, 85, 104 …) that would leave a residual < 200 (STAR MinLot).
// All eleven trade_executions for that fund had broker_order_id =
// NULL — definitive proof they bypassed the simulator. This guard
// closes the gap so PM-path fills get the same odd-lot residual /
// step-alignment treatment as broker-path orders.
//
// Verdict semantics mirror instrument.IsAligned (buy) and
// instrument.NormalizeSellQty (sell). On reject the function
// returns a wrapped api.ErrConflict so the caller bubbles the
// action to "rejected" without any side effects on holdings,
// cash, or the lot ledger.
//
// Non-A-share symbols (US, HK, crypto, futures) are partially
// covered: A-share board lot rules and US ≥$1 / sub-dollar tick
// rules are deterministic in code, so we enforce them here. HK
// banded ticks and crypto step_size still rely on broker-side
// LotSizeGate (instrument_metadata-backed) when those orders
// flow through broker.Simulator.SubmitOrder. PM-direct fills of
// HK / crypto skip the tick check on this path — production
// wiring should not route those venues through tradeRepoCreateAndFill.
//
// orderPrice is the limit price the trade will be recorded at
// (filled_price for limit orders). Pass 0 for market orders;
// the tick check is then skipped because the broker would fill
// at the venue's own tick-aligned price.
func (e *runtimeTradingEngine) pmPathLotSizeGuard(
	side string,
	action repository.PlanAction,
	quantity int,
	orderPrice float64,
	position repository.HoldingPosition,
) error {
	if quantity <= 0 {
		return nil
	}
	hint := instrument2.Hint{
		Market:     action.Market.String,
		Exchange:   action.Exchange.String,
		AssetClass: action.AssetClass.String,
	}

	// S12-followup tick check: applies to A-share (0.01 CNY
	// across all boards) and US equity (Reg NMS 612 — 0.01 USD
	// at ≥$1, 0.0001 at <$1). Returns 0 / aligned-true for
	// venues we don't deterministically model (HK banded,
	// crypto step) — those are gated by the broker-side
	// LotSizeGate via instrument_metadata, separately.
	if orderPrice > 0 && !instrument2.IsTickAligned(action.Symbol, hint, orderPrice) {
		tick := instrument2.TickSizeFor(action.Symbol, hint, orderPrice)
		suggested := instrument2.FloorToTick(action.Symbol, hint, orderPrice)
		e.recordPMPathLotSizeReject("tick")
		return fmt.Errorf("%w: tick-size: %s price=%g not aligned to %g; suggested floor=%g",
			api.ErrConflict, action.Symbol, orderPrice, tick, suggested)
	}

	spec := instrument2.SpecFor(instrument2.Classify(action.Symbol, hint))
	if !spec.IsAShare() {
		return nil
	}
	qty := float64(quantity)
	normalisedSide := strings.ToLower(strings.TrimSpace(side))
	switch normalisedSide {
	case "buy":
		if !instrument2.IsAligned(action.Symbol, hint, qty) {
			suggested := instrument2.NormalizeBuyQty(action.Symbol, hint, qty)
			e.recordPMPathLotSizeReject("buy")
			return fmt.Errorf("%w: lot-size: %s buy qty=%d violates %s board (min_lot=%d, step=%d); suggested qty=%g",
				api.ErrConflict, action.Symbol, quantity, spec.Board, spec.MinLot, spec.Step, suggested)
		}
	case "sell", "reduce", "close_long", "close_short":
		held := position.AvailableQty
		if held <= 0 {
			held = position.Quantity
		}
		if held <= 0 {
			e.recordPMPathLotSizeReject("sell_no_position")
			return fmt.Errorf("%w: lot-size: %s sell rejected — no recorded position",
				api.ErrConflict, action.Symbol)
		}
		legal := instrument2.NormalizeSellQty(action.Symbol, hint, qty, held)
		// Allow a tiny float fuzz when comparing the legal value
		// against the requested quantity (NormalizeSellQty returns
		// integer share counts as float64).
		if legal-qty > 1e-6 || qty-legal > 1e-6 {
			e.recordPMPathLotSizeReject("sell_residual")
			return fmt.Errorf("%w: lot-size: %s sell qty=%d on holding %g would leave odd-lot residual (< %s board min_lot=%d); must sell %g to liquidate",
				api.ErrConflict, action.Symbol, quantity, held, spec.Board, spec.MinLot, legal)
		}
	}
	return nil
}

// recordPMPathLotSizeReject bumps the lot-size metric with a
// PM-path discriminator so the dashboard can separate broker-path
// rejects (already tracked) from PM-direct-fill rejects (this gate).
func (e *runtimeTradingEngine) recordPMPathLotSizeReject(reason string) {
	if e == nil || e.metrics == nil {
		return
	}
	e.metrics.RecordLotSizeEvent("pmpath_reject_" + reason)
}

// pmPathPreTradeGateChain runs the four broker-side regulatory
// gates (market-status, lockup, borrow, price-collar) against a
// PM-direct-fill request. Mirrors broker.Simulator.SubmitOrder's
// chain so trades that bypass the simulator face exactly the same
// pre-trade checks. The fifth gate (lot-size) lives in
// pmPathLotSizeGuard and runs separately after the cash / qty
// availability checks — same ordering as the broker path.
//
// All four gates are nil-tolerant: a nil field is treated as "no
// gate wired" and skipped, identical to how broker.Simulator
// behaves when its WithXxxGate option isn't applied. This keeps
// the chain a no-op under legacy test wiring and single-binary
// smoke builds where the production main.go isn't running.
//
// On reject the function returns a wrapped api.ErrConflict so the
// caller (executePlanAction) transitions the action to "rejected"
// with no side effects. Gate Warnings flow into the returned
// []string for the trade row to carry forward — matching the
// broker simulator's gateWarnings accumulation.
//
// Verdict ordering = simulator's gate chain:
//   1. market-status (halted symbol, calendar, circuit breaker)
//   2. lockup        (T+1 / post-IPO lock, broker-side reinforcement)
//   3. borrow        (short-sell locate / inventory)
//   4. price-collar  (fat-finger / limit too far from reference)
//
// The same precedence rule applies to the rejection reason: a
// halted-symbol reject wins over a lockup reject, etc., so an
// operator reading the error message sees the "harder" reason
// first (mirrors broker simulator behaviour).
func (e *runtimeTradingEngine) pmPathPreTradeGateChain(
	ctx context.Context,
	fund *repository.Fund,
	action repository.PlanAction,
	side string,
	quantity int,
	orderPrice float64,
	clientOrderID string,
) ([]string, error) {
	if e == nil {
		return nil, nil
	}
	var warnings []string
	qty := float64(quantity)
	sideStr := strings.ToLower(strings.TrimSpace(side))
	instrumentKey := action.InstrumentKey
	if instrumentKey == "" {
		instrumentKey = buildInstrumentKey(action.Exchange.String, action.Symbol)
	}

	if e.marketStatusGate != nil {
		v := e.marketStatusGate.CheckOrder(ctx, broker.MarketStatusProbe{
			FundID:        fund.ID,
			InstrumentKey: instrumentKey,
			Symbol:        action.Symbol,
			Market:        action.Market.String,
			AssetClass:    action.AssetClass.String,
			Side:          sideStr,
			Quantity:      qty,
			IntendedPrice: orderPrice,
			ClientOrderID: clientOrderID,
		})
		if v.Rejected {
			e.recordPMPathGateReject("market_status")
			reason := v.RejectReason
			if reason == "" {
				reason = "rejected by market-status gate"
			}
			return warnings, fmt.Errorf("%w: market-status: %s", api.ErrConflict, reason)
		}
		warnings = append(warnings, v.Warnings...)
	}

	if e.lockupGate != nil {
		v := e.lockupGate.CheckOrder(ctx, broker.LockupProbe{
			FundID:        fund.ID,
			InstrumentKey: instrumentKey,
			Symbol:        action.Symbol,
			AssetClass:    action.AssetClass.String,
			Side:          sideStr,
			Quantity:      qty,
			IntendedPrice: orderPrice,
			ClientOrderID: clientOrderID,
		})
		if v.Rejected {
			e.recordPMPathGateReject("lockup")
			reason := v.RejectReason
			if reason == "" {
				reason = "rejected by lockup gate"
			}
			return warnings, fmt.Errorf("%w: lockup: %s", api.ErrConflict, reason)
		}
		warnings = append(warnings, v.Warnings...)
	}

	if e.borrowGate != nil {
		v := e.borrowGate.CheckOrder(ctx, broker.BorrowProbe{
			FundID:        fund.ID,
			InstrumentKey: instrumentKey,
			Symbol:        action.Symbol,
			AssetClass:    action.AssetClass.String,
			Side:          sideStr,
			Quantity:      qty,
			IntendedPrice: orderPrice,
			ClientOrderID: clientOrderID,
		})
		if v.Rejected {
			e.recordPMPathGateReject("borrow")
			reason := v.RejectReason
			if reason == "" {
				reason = "rejected by borrow gate"
			}
			return warnings, fmt.Errorf("%w: borrow: %s", api.ErrConflict, reason)
		}
		warnings = append(warnings, v.Warnings...)
	}

	if e.priceCollarGate != nil {
		v := e.priceCollarGate.CheckOrder(ctx, broker.PriceCollarProbe{
			FundID:        fund.ID,
			InstrumentKey: instrumentKey,
			Symbol:        action.Symbol,
			Market:        action.Market.String,
			AssetClass:    action.AssetClass.String,
			Side:          sideStr,
			Quantity:      qty,
			IntendedPrice: orderPrice,
			ClientOrderID: clientOrderID,
		})
		if v.Rejected {
			e.recordPMPathGateReject("price_collar")
			reason := v.RejectReason
			if reason == "" {
				reason = "rejected by price-collar gate"
			}
			return warnings, fmt.Errorf("%w: price-collar: %s", api.ErrConflict, reason)
		}
		warnings = append(warnings, v.Warnings...)
	}

	return warnings, nil
}

// recordPMPathGateReject increments the per-gate event counter
// using the same metric series each broker-side gate already
// emits, with a `pmpath_reject` event so the dashboard can
// distinguish PM-direct-fill rejects from broker-path rejects.
func (e *runtimeTradingEngine) recordPMPathGateReject(gate string) {
	if e == nil || e.metrics == nil {
		return
	}
	switch gate {
	case "market_status":
		e.metrics.RecordMarketStatusEvent("pmpath_reject")
	case "lockup":
		e.metrics.RecordLockupEvent("pmpath_reject")
	case "borrow":
		e.metrics.RecordBorrowEvent("pmpath_reject")
	case "price_collar":
		e.metrics.RecordPriceCollarEvent("pmpath_reject")
	}
}

func (e *runtimeTradingEngine) tradeRepoCreateAndFill(
	ctx context.Context,
	fund *repository.Fund,
	plan *repository.InvestmentPlan,
	action repository.PlanAction,
	side string,
	quantity int,
	planPrice float64,
	amount float64,
	status string,
	filledPrice sql.NullFloat64,
	feeCommission float64,
	feeStampTax float64,
	feeTransfer float64,
	// strategy is the agent.TraderAgent-style execution style label
	// ("immediate" / "limit" / "twap" / "vwap"). Caller picks it via
	// selectPMPathExecutionStrategy. Persisted on
	// trade_executions.strategy so downstream analytics + the daily-
	// review LLM can see which execution intent the trade fell into,
	// even before the splitter actually carves a parent into children
	// (B-step2 follow-up).
	strategy string,
	// realizedPnL is signed (positive = profit, negative = loss).
	// Carries a Valid value ONLY on a futures CLOSE; equity paths
	// and futures opens pass the zero-value sql.NullFloat64{}. The
	// cash-ledger dispatcher routes futures fills on opted-in funds
	// (futures_cash_ledger_v2 flag) through the v2 path that writes
	// margin_post / margin_release / realized_pnl instead of the
	// equity-shaped trade_*_notional pair. Callers that aren't
	// closing a futures position can safely pass the zero value;
	// the dispatcher treats Valid=false as "no PnL leg to write".
	realizedPnL sql.NullFloat64,
) (rolledStatus string, err error) {
	// rolledStatus return:
	//
	//   "" — non-split path; caller should fall back to its own
	//        status decision (status arg, broker.Simulator return).
	//   "filled" / "partial:NN" / "pending" / "rejected" — split
	//        path; aggregateChildrenStatus over the children's
	//        actual statuses. Caller writes this into the
	//        plan_actions.execution_status sync map so the per-
	//        plan_action status reflects the aggregate of N
	//        slices (not just the parent's intent).
	//
	// Today every code path that reaches the splitter is the
	// synchronous broker.Simulator, which fills every child at
	// the same status string passed in (`status` arg). So
	// rolledStatus == "filled" 100% of the time on this path,
	// and the wire below is technically a no-op refactor for
	// "filled". When live brokers (Alpaca / IBKR) replace the
	// simulator and start emitting genuinely partial / mixed
	// child statuses, this wire surfaces the right roll-up
	// without further code changes — the splitter and the
	// status helper already agree on the partial:NN format
	// (see pm_path_children_status.go's matrix tests).
	// price (the column) holds the plan reference price the user
	// approved at. filled_price holds the actual execution price.
	// slippage_pct denormalises the drift for analytics; it's only
	// meaningful for risk-increasing trades that were filled at a
	// live price (skipped for sells and for fills where planPrice
	// was zero).
	slippagePct := computeSlippagePct(side, planPrice, filledPrice)
	executedAt := time.Now().UTC()
	// P0-4: mint a deterministic idempotency key from
	// (plan_action_id, side, attempt) so that an HTTP retry, a
	// process restart, or any other duplicate path that calls
	// tradeRepoCreateAndFill for the same plan_action_id collapses
	// to the existing row rather than double-booking. The key MUST
	// include side because reduce/sell against the same action_id
	// is logically a different submission. quantity is appended to
	// disambiguate partial-fill follow-ons (a future PR may slice
	// a single action into multiple smaller submissions). The key
	// is empty when action.ID is empty (synthetic test fixtures);
	// in that case Create falls back to its non-idempotent path.
	normalizedStrategy := normalizePMPathStrategy(strategy)
	// B-step2: if the per-fund flag is on AND the splitter says
	// this (qty, strategy) pair warrants more than one child AND
	// the splitter is wired for this side+position, fan out into
	// a parent + N children. The flag defaults to false so
	// non-opted-in funds keep the legacy single-row path on this
	// same call.
	//
	// Splitter wiring matrix:
	//
	//   side  | position_side | enabled
	//   ------+---------------+--------
	//   buy   | (any non-short) | YES (T1-step2-buy commit)
	//   sell  | long / unset    | YES (this commit)
	//   sell  | short           | NO  (futures short open semantics
	//                                  flip the lot ledger — recorded
	//                                  by recordLotFill as a no-op
	//                                  today. The cash_ledger sell
	//                                  legs would still write, but
	//                                  the lot ledger drift across
	//                                  multiple children needs a
	//                                  parallel short-lot model.)
	//   buy   | short           | NO  (closes a short — symmetric
	//                                  blocker to the above.)
	//
	// See docs/TRADER_AGENT_INTEGRATION.md "step 2 status".
	if splitterEnabledForSideWithConfig(side, action, fund.Config) &&
		pmPathChildSplittingEnabled(fund.Config) &&
		shouldSplitParent(quantity, normalizedStrategy) {
		return e.tradeRepoCreateAndFillSplit(
			ctx, fund, plan, action, side, quantity,
			planPrice, amount, status, filledPrice,
			feeCommission, feeStampTax, feeTransfer,
			normalizedStrategy, slippagePct, executedAt,
			realizedPnL,
		)
	}

	// Non-split path: caller already decided the status string
	// (broker.Simulator return / local var), so we have nothing
	// to roll up. Returning empty rolledStatus tells the caller
	// "use your own status, the engine didn't override it" —
	// a deliberate signal so an empty string never gets mistaken
	// for a valid execution_status value.

	clientIdempotencyKey := mintTradeIdempotencyKey(action.ID, side, quantity)
	trade := &repository.TradeExecution{
		FundID:               fund.ID,
		PlanID:               nullUUID(plan.ID),
		PlanActionID:         nullUUID(action.ID),
		InstrumentKey:        firstNonEmptyValue(action.InstrumentKey, buildInstrumentKey(action.Exchange.String, action.Symbol), action.Symbol),
		Symbol:               action.Symbol,
		Market:               action.Market,
		Exchange:             action.Exchange,
		AssetClass:           action.AssetClass,
		InstrumentType:       action.InstrumentType,
		Side:                 side,
		PositionSide:         action.PositionSide,
		OpenClose:            action.OpenClose,
		OrderType:            executionOrderType(action),
		Quantity:             float64(quantity),
		Price:                nullableFloat(planPrice),
		Amount:               nullableFloat(amount),
		FilledQty:            float64(quantity),
		FilledPrice:          filledPrice,
		FeeCommission:        feeCommission,
		FeeStampTax:          feeStampTax,
		FeeTransfer:          feeTransfer,
		TradingMode:          normalizedTradingMode(fund.TradingMode),
		Status:               status,
		ExecutedAt:           sql.NullTime{Time: executedAt, Valid: true},
		QuoteCurrency:        action.QuoteCurrency,
		SettlementCurrency:   action.SettlementCurrency,
		MarginMode:           action.MarginMode,
		Leverage:             action.Leverage,
		ContractMultiplier:   action.ContractMultiplier,
		ExpiryDate:           action.ExpiryDate,
		ReduceOnly:           action.ReduceOnly,
		SlippagePct:          slippagePct,
		// B-step2: persist the chosen strategy on the single-row
		// path too. Children inherit this value verbatim when the
		// splitter path is taken (see tradeRepoCreateAndFillSplit).
		// Legacy rows (pre-088 migration) keep strategy=NULL.
		Strategy:             sql.NullString{String: normalizedStrategy, Valid: true},
		ClientIdempotencyKey: clientIdempotencyKey,
	}
	slog.Info("pm-path execute trade",
		"fund_id", fund.ID,
		"plan_id", plan.ID,
		"action_id", action.ID,
		"symbol", action.Symbol,
		"side", side,
		"quantity", quantity,
		"strategy", normalizedStrategy,
		"plan_price", planPrice,
		"status", status,
		"path", "single",
	)
	tradeID, createErr := e.tradeRepo.Create(ctx, trade)
	if createErr != nil {
		return "", mapRepositoryError(createErr)
	}
	if statusErr := e.tradeRepo.UpdateStatus(ctx, tradeID, status, float64(quantity), filledPrice, feeCommission, feeStampTax, feeTransfer, slippagePct); statusErr != nil {
		return "", mapRepositoryError(statusErr)
	}
	// Lot ledger is a best-effort shadow update. Failures are
	// logged + counted but never propagated — the trade itself
	// has already filled, and the FIFO ledger can be reconciled
	// later if it drifts.
	e.recordLotFill(ctx, fund, action, tradeID, side, quantity, filledPrice, planPrice, feeCommission+feeStampTax+feeTransfer, executedAt, status)
	// P1-1 — append cash_ledger rows for this fill so the journal
	// stays in sync with funds.current_capital. Best-effort: a
	// failure here doesn't roll the trade back, but the row's
	// idempotency_key lets a future reconciliation job re-attempt
	// the missed entries safely.
	if status == "filled" {
		filledExecutionPrice := planPrice
		if filledPrice.Valid && filledPrice.Float64 > 0 {
			filledExecutionPrice = filledPrice.Float64
		}
		// realizedPnL is non-Valid for equity + futures-open paths
		// and Valid (signed) for a futures CLOSE — see the
		// tradeRepoCreateAndFill docstring for the contract.
		e.recordCashLedgerForFill(ctx, fund, plan, action, tradeID, side, quantity, filledExecutionPrice, amount, feeCommission, feeStampTax, feeTransfer, executedAt, realizedPnL)
	}
	// Single-row path: caller keeps its own status — see the
	// return-value docstring at the function head.
	return "", nil
}

// tradeRepoCreateAndFillSplit is the B-step2 child-order splitting
// path. Reached only when ALL of the following are true (the gate
// is enforced at the call site, not here, so this function can
// always assume splitting is appropriate):
//
//   - side == "buy" (sell + futures-close land in a follow-up
//     commit, see ADR docs/TRADER_AGENT_INTEGRATION.md).
//   - pmPathChildSplittingEnabled(fund.Config) returned true.
//   - shouldSplitParent(quantity, strategy) returned true (i.e.
//     splitParentIntoChildren produces > 1 slice).
//
// Behaviour:
//
//   * INSERT a single PARENT row carrying the aggregated qty +
//     summed fees + the chosen strategy. The parent has
//     strategy_parent_trade_id = NULL and is NEVER written to
//     cash_ledger / position_lots — those follow the children so
//     FIFO cost basis remains accurate per slice. The parent's
//     filled_price is the (shared) execution price for this
//     commit; a future variant of the splitter that returns
//     per-slice prices will populate it as a weighted average.
//
//   * For each child slice, INSERT a child row (qty = slice qty,
//     strategy = inherited, strategy_parent_trade_id = parent.ID,
//     fees = pro-rata share by qty with the LAST child absorbing
//     rounding remainder). Each child writes its own
//     cash_ledger legs + position_lots entry.
//
//   * Slippage on every row equals the parent's slippage (single
//     execution price model in this commit). The aggregation work
//     a multi-price model would need (qty-weighted avg fill price,
//     per-child slippage vs parent reference) is in scope for the
//     next commit; this one preserves the contract that all rows
//     for one action carry consistent slippage so reports don't
//     regress.
//
// All children share the executedAt timestamp because the underlying
// broker.Simulator call returned a single fill (we're not yet
// stretching the slices across the trading day). This is purely a
// step-2 simplification — schema, indices, and downstream readers
// are already ready for distinct per-child executedAt values when
// the venue path produces them.
func (e *runtimeTradingEngine) tradeRepoCreateAndFillSplit(
	ctx context.Context,
	fund *repository.Fund,
	plan *repository.InvestmentPlan,
	action repository.PlanAction,
	side string,
	quantity int,
	planPrice float64,
	amount float64,
	status string,
	filledPrice sql.NullFloat64,
	feeCommission, feeStampTax, feeTransfer float64,
	normalizedStrategy string,
	slippagePct sql.NullFloat64,
	executedAt time.Time,
	// realizedPnL: same contract as tradeRepoCreateAndFill.
	// Splitter today is gated off for futures so this is always
	// the zero value in production; the param exists to keep the
	// signatures aligned and unblock the futures-splitter unlock
	// without another wire change.
	realizedPnL sql.NullFloat64,
) (rolledStatus string, err error) {
	childQtys := splitParentIntoChildren(quantity, normalizedStrategy)
	if len(childQtys) <= 1 {
		// Should never happen — the gate at the call site
		// already verified shouldSplitParent. Belt + suspenders.
		return "", fmt.Errorf("tradeRepoCreateAndFillSplit: splitter returned %d child(ren) for qty=%d strategy=%s",
			len(childQtys), quantity, normalizedStrategy)
	}

	instrumentKey := firstNonEmptyValue(action.InstrumentKey, buildInstrumentKey(action.Exchange.String, action.Symbol), action.Symbol)
	tradingMode := normalizedTradingMode(fund.TradingMode)
	executionPrice := planPrice
	if filledPrice.Valid && filledPrice.Float64 > 0 {
		executionPrice = filledPrice.Float64
	}

	// Parent INSERT — same total qty + summed fees as the legacy
	// single-row path so any reader that only looks at the parent
	// (e.g. a NAV reconciler that hasn't been updated yet) sees
	// the same totals it would have seen pre-088. The
	// strategy_parent_trade_id is NULL: parent rows do not chain.
	parentTrade := &repository.TradeExecution{
		FundID:               fund.ID,
		PlanID:               nullUUID(plan.ID),
		PlanActionID:         nullUUID(action.ID),
		InstrumentKey:        instrumentKey,
		Symbol:               action.Symbol,
		Market:               action.Market,
		Exchange:             action.Exchange,
		AssetClass:           action.AssetClass,
		InstrumentType:       action.InstrumentType,
		Side:                 side,
		PositionSide:         action.PositionSide,
		OpenClose:            action.OpenClose,
		OrderType:            executionOrderType(action),
		Quantity:             float64(quantity),
		Price:                nullableFloat(planPrice),
		Amount:               nullableFloat(amount),
		FilledQty:            float64(quantity),
		FilledPrice:          filledPrice,
		FeeCommission:        feeCommission,
		FeeStampTax:          feeStampTax,
		FeeTransfer:          feeTransfer,
		TradingMode:          tradingMode,
		Status:               status,
		ExecutedAt:           sql.NullTime{Time: executedAt, Valid: true},
		QuoteCurrency:        action.QuoteCurrency,
		SettlementCurrency:   action.SettlementCurrency,
		MarginMode:           action.MarginMode,
		Leverage:             action.Leverage,
		ContractMultiplier:   action.ContractMultiplier,
		ExpiryDate:           action.ExpiryDate,
		ReduceOnly:           action.ReduceOnly,
		SlippagePct:          slippagePct,
		Strategy:             sql.NullString{String: normalizedStrategy, Valid: true},
		// strategy_parent_trade_id = NULL — this IS the parent.
		ClientIdempotencyKey: mintTradeIdempotencyKey(action.ID, side, quantity),
	}
	parentID, createErr := e.tradeRepo.Create(ctx, parentTrade)
	if createErr != nil {
		return "", mapRepositoryError(createErr)
	}
	if statusErr := e.tradeRepo.UpdateStatus(ctx, parentID, status, float64(quantity), filledPrice, feeCommission, feeStampTax, feeTransfer, slippagePct); statusErr != nil {
		return "", mapRepositoryError(statusErr)
	}
	slog.Info("pm-path execute trade",
		"fund_id", fund.ID,
		"plan_id", plan.ID,
		"action_id", action.ID,
		"symbol", action.Symbol,
		"side", side,
		"quantity", quantity,
		"strategy", normalizedStrategy,
		"plan_price", planPrice,
		"status", status,
		"path", "split-parent",
		"parent_trade_id", parentID,
		"child_count", len(childQtys),
	)

	// Pro-rata fee split: first N-1 children get
	// round(fee * childQty/totalQty); last child absorbs the
	// rounding remainder so sum equals the input exactly. We
	// round per leg independently (commission, stamp tax,
	// transfer) so the per-leg invariant holds in isolation.
	commissionByChild := proRataFeeSplit(feeCommission, childQtys)
	stampByChild := proRataFeeSplit(feeStampTax, childQtys)
	transferByChild := proRataFeeSplit(feeTransfer, childQtys)

	// Collected per-child (status, filledQty) so the parent log
	// line can carry an aggregated execution_status that future
	// async / partial-fill paths can drop into plan_actions
	// without touching the splitter. Today's broker.Simulator
	// fills everything synchronously at `status` so the rollup
	// will read "filled"; the helper is here so the contract is
	// pinned before live-broker integrations (Alpaca / IBKR)
	// land and start emitting genuinely partial children.
	childStatuses := make([]ChildStatus, 0, len(childQtys))

	// Notional is the row's signed-price * qty input.
	// Recompute per child rather than pro-rata-dividing the
	// parent's notional so rounding stays consistent with the
	// existing single-row legacy behaviour.
	for childIdx, childQty := range childQtys {
		childNotional := executionPrice * float64(childQty)

		childTrade := &repository.TradeExecution{
			FundID:                fund.ID,
			PlanID:                nullUUID(plan.ID),
			PlanActionID:          nullUUID(action.ID),
			InstrumentKey:         instrumentKey,
			Symbol:                action.Symbol,
			Market:                action.Market,
			Exchange:              action.Exchange,
			AssetClass:            action.AssetClass,
			InstrumentType:        action.InstrumentType,
			Side:                  side,
			PositionSide:          action.PositionSide,
			OpenClose:             action.OpenClose,
			OrderType:             executionOrderType(action),
			Quantity:              float64(childQty),
			Price:                 nullableFloat(planPrice),
			Amount:                nullableFloat(childNotional),
			FilledQty:             float64(childQty),
			FilledPrice:           filledPrice,
			FeeCommission:         commissionByChild[childIdx],
			FeeStampTax:           stampByChild[childIdx],
			FeeTransfer:           transferByChild[childIdx],
			TradingMode:           tradingMode,
			Status:                status,
			ExecutedAt:            sql.NullTime{Time: executedAt, Valid: true},
			QuoteCurrency:         action.QuoteCurrency,
			SettlementCurrency:    action.SettlementCurrency,
			MarginMode:            action.MarginMode,
			Leverage:              action.Leverage,
			ContractMultiplier:    action.ContractMultiplier,
			ExpiryDate:            action.ExpiryDate,
			ReduceOnly:            action.ReduceOnly,
			SlippagePct:           slippagePct,
			Strategy:              sql.NullString{String: normalizedStrategy, Valid: true},
			StrategyParentTradeID: sql.NullString{String: parentID, Valid: true},
			ClientIdempotencyKey: sql.NullString{
				String: fmt.Sprintf("trade:%s:%s:%d:child:%d", action.ID, side, quantity, childIdx),
				Valid:  action.ID != "",
			},
		}
		childID, childCreateErr := e.tradeRepo.Create(ctx, childTrade)
		if childCreateErr != nil {
			return "", mapRepositoryError(childCreateErr)
		}
		if childStatusErr := e.tradeRepo.UpdateStatus(ctx, childID, status, float64(childQty), filledPrice,
			commissionByChild[childIdx], stampByChild[childIdx], transferByChild[childIdx],
			slippagePct); childStatusErr != nil {
			return "", mapRepositoryError(childStatusErr)
		}
		// Per-child lot + cash ledger. Same call signatures as the
		// single-row path; the only difference is they're invoked
		// N times (and never for the parent).
		childTotalFees := commissionByChild[childIdx] + stampByChild[childIdx] + transferByChild[childIdx]
		e.recordLotFill(ctx, fund, action, childID, side, childQty, filledPrice, planPrice, childTotalFees, executedAt, status)
		if status == "filled" {
			// realizedPnL: splitter today only runs for equity
			// (futures are gated off in splitterEnabledForSide)
			// so a per-child PnL split is moot. When futures
			// splitting unlocks we'll pro-rate the parent PnL
			// across children by childQty/totalQty here; for
			// now pass through the parent's value so the wire
			// is plumbed and unit tests can assert it without
			// needing a futures-aware splitter gate change.
			childPnL := sql.NullFloat64{}
			if realizedPnL.Valid {
				childPnL = sql.NullFloat64{
					Float64: realizedPnL.Float64 * float64(childQty) / float64(quantity),
					Valid:   true,
				}
			}
			e.recordCashLedgerForFill(ctx, fund, plan, action, childID, side, childQty, executionPrice, childNotional,
				commissionByChild[childIdx], stampByChild[childIdx], transferByChild[childIdx], executedAt, childPnL)
		}

		// Capture per-child status for the aggregated rollup
		// emitted below. FilledQty == childQty when status =
		// "filled" (the synchronous broker.Simulator path);
		// future live-broker integrations may set partial qty
		// here, at which point aggregateChildrenStatus picks
		// the right "partial:NN" label.
		childFilled := 0.0
		if status == "filled" {
			childFilled = float64(childQty)
		}
		childStatuses = append(childStatuses, ChildStatus{
			Status:    status,
			FilledQty: childFilled,
		})
	}

	// Rollup the per-child statuses into a single parent-level
	// label. Today this is "filled" for every code path because
	// broker.Simulator fills synchronously; tomorrow when a
	// live-broker integration emits genuinely partial / rejected
	// child statuses the helper will return "partial:NN" /
	// "rejected" instead and the caller (executePlanAction)
	// surfaces it directly into plan_actions.execution_status
	// without any further code changes.
	rolledStatus = aggregateChildrenStatus(childStatuses, float64(quantity))
	slog.Info("pm-path execute trade rollup",
		"fund_id", fund.ID,
		"plan_id", plan.ID,
		"action_id", action.ID,
		"parent_trade_id", parentID,
		"child_count", len(childQtys),
		"rolled_status", rolledStatus,
	)
	return rolledStatus, nil
}

// proRataFeeSplit returns the per-child share of `total` allocated
// proportional to qtys. First N-1 children get
// round(total * qty[i]/sumQty, 4); the last child absorbs whatever
// remainder is needed so the sum equals `total` exactly. Rounding
// is to 4 decimal places to match recordCashLedgerForFill's
// roundCurrency convention.
//
// If total is 0 (e.g. zero-fee trade), all children get 0 — same
// invariant the cash_ledger leg loop already enforces (skip zero
// amounts).
func proRataFeeSplit(total float64, qtys []int) []float64 {
	if len(qtys) == 0 {
		return nil
	}
	out := make([]float64, len(qtys))
	if total == 0 {
		return out
	}
	sumQty := 0
	for _, q := range qtys {
		sumQty += q
	}
	if sumQty == 0 {
		return out
	}
	allocated := 0.0
	for i := 0; i < len(qtys)-1; i++ {
		share := roundCurrency(total * float64(qtys[i]) / float64(sumQty))
		out[i] = share
		allocated += share
	}
	// Last child absorbs remainder so the sum is exact. We
	// don't round here — the residual is already at most one
	// rounding-step's worth of float drift.
	out[len(qtys)-1] = total - allocated
	return out
}

// recordCashLedgerForFill writes the per-leg cash_ledger rows
// for a single equity fill (P1-1). Best-effort: on failure we
// log + count but do NOT bubble the error so the trade flow
// stays unchanged. The idempotency_key is deterministic
// ("trade:{tradeID}:{leg}") so a retry path collapses cleanly.
//
// Sign convention reminder: amounts are SIGNED.
//
//	buy
//	  notional      = -quantity * price       (cash out)
//	  commission    = -feeCommission          (cash out)
//	  transfer_fee  = -feeTransfer            (cash out)
//	  stamp_tax     = -feeStampTax            (cash out, usually 0 for buys)
//	sell
//	  notional      = +quantity * price       (cash in)
//	  commission    = -feeCommission          (cash out)
//	  transfer_fee  = -feeTransfer            (cash out)
//	  stamp_tax     = -feeStampTax            (cash out)
//
// We deliberately separate the four legs rather than netting
// them so reports can subtotal commissions cleanly. Net cash
// movement equals SUM over the four entries.
//
// Currency: we record the fund's quote currency (action.QuoteCurrency)
// when present, otherwise USD. P1-4 (FX) is responsible for
// folding multi-currency entries into base-currency NAV.
func (e *runtimeTradingEngine) recordCashLedgerForFill(
	ctx context.Context,
	fund *repository.Fund,
	plan *repository.InvestmentPlan,
	action repository.PlanAction,
	tradeID string,
	side string,
	quantity int,
	executionPrice float64,
	notional float64,
	feeCommission, feeStampTax, feeTransfer float64,
	executedAt time.Time,
	// realizedPnL is signed: positive = profit, negative = loss.
	// Only carries a Valid value on a FUTURES CLOSE; equity paths
	// and futures opens pass the zero value (Valid=false). The
	// dispatcher below routes the call to recordCashLedgerFuturesForFill
	// when the fund has opted into the v2 cash flow model
	// (futures_cash_ledger_v2 flag); legacy funds keep writing
	// trade_buy_notional / trade_sell_notional even on futures.
	realizedPnL sql.NullFloat64,
) {
	if e == nil || e.cashLedger == nil {
		return
	}
	if fund == nil || fund.ID == "" || tradeID == "" {
		return
	}
	// T7 dispatch: futures fills on opted-in funds go through
	// the v2 path so the journal records margin movement +
	// realized PnL instead of the misleading "full notional"
	// cash flow that the equity model assumes.
	if strings.EqualFold(strings.TrimSpace(action.AssetClass.String), "futures") &&
		futuresCashLedgerV2Enabled(fund.Config) {
		e.recordCashLedgerFuturesForFill(ctx, fund, plan, action, tradeID, side, quantity,
			executionPrice, notional, feeCommission, feeStampTax, feeTransfer,
			executedAt, realizedPnL)
		return
	}
	currency := "USD"
	if action.QuoteCurrency.Valid && strings.TrimSpace(action.QuoteCurrency.String) != "" {
		currency = strings.ToUpper(strings.TrimSpace(action.QuoteCurrency.String))
	}
	tradingDate := executedAt
	planID := ""
	if plan != nil {
		planID = plan.ID
		if !plan.TradingDate.IsZero() {
			tradingDate = plan.TradingDate
		}
	}
	desc := fmt.Sprintf("%s %d %s @ %.4f", side, quantity, action.Symbol, executionPrice)
	commonMeta := map[string]any{
		"symbol":     action.Symbol,
		"quantity":   quantity,
		"price":      executionPrice,
		"action_id":  action.ID,
	}

	type leg struct {
		entryType string
		amount    float64
		key       string
	}
	var legs []leg
	if strings.EqualFold(side, "buy") {
		legs = []leg{
			{entryType: repository.CashEntryTradeBuyNotional, amount: -notional, key: "notional"},
			{entryType: repository.CashEntryTradeBuyCommission, amount: -feeCommission, key: "commission"},
			{entryType: repository.CashEntryTradeBuyTransfer, amount: -feeTransfer, key: "transfer"},
			{entryType: repository.CashEntryTradeBuyStampTax, amount: -feeStampTax, key: "stamp_tax"},
		}
	} else {
		legs = []leg{
			{entryType: repository.CashEntryTradeSellNotional, amount: notional, key: "notional"},
			{entryType: repository.CashEntryTradeSellCommission, amount: -feeCommission, key: "commission"},
			{entryType: repository.CashEntryTradeSellTransfer, amount: -feeTransfer, key: "transfer"},
			{entryType: repository.CashEntryTradeSellStampTax, amount: -feeStampTax, key: "stamp_tax"},
		}
	}
	for _, l := range legs {
		// Skip zero-fee legs — the table CHECK rejects amount=0
		// and there's no point recording "no commission paid".
		if l.amount == 0 {
			continue
		}
		params := repository.AppendParams{
			FundID:         fund.ID,
			PostedAt:       executedAt,
			TradingDate:    &tradingDate,
			EntryType:      l.entryType,
			Amount:         roundCurrency(l.amount),
			Currency:       currency,
			TradeID:        tradeID,
			PlanID:         planID,
			PlanActionID:   action.ID,
			Description:    desc,
			Metadata:       commonMeta,
			IdempotencyKey: fmt.Sprintf("trade:%s:%s", tradeID, l.key),
		}
		if _, err := e.cashLedger.Append(ctx, params); err != nil {
			slog.Warn("cash_ledger: append failed",
				"fund_id", fund.ID,
				"trade_id", tradeID,
				"entry_type", l.entryType,
				"err", err.Error())
			if e.metrics != nil {
				e.metrics.RecordCashLedgerWriteFailure(l.entryType)
			}
		}
	}
}

// recordCashLedgerFuturesForFill is the T7 futures-aware writer.
// Called only via recordCashLedgerForFill's dispatch when the fund
// has opted into the v2 model (futures_cash_ledger_v2 flag).
//
// Cash flow model:
//
//   OPEN (open_close == "open" / unset on a buy intent):
//     futures_margin_post     amount = -initialMargin
//     trade_buy_commission    amount = -feeCommission
//     trade_buy_transfer_fee  amount = -feeTransfer
//     (stamp tax not material for futures; included only when
//      caller supplies a non-zero value.)
//
//   CLOSE (open_close == "close" or any close-like flag):
//     futures_margin_release  amount = +initialMargin
//     futures_realized_pnl    amount = realizedPnL (signed)
//     trade_sell_commission   amount = -feeCommission
//     trade_sell_transfer_fee amount = -feeTransfer
//
// Margin is derived from the action's leverage + the trade's
// notional via futuresMarginRequired — same function the cash-
// check gate in executePlanAction uses, so the journal entry
// matches the cash that was reserved at gate time.
//
// realizedPnL is sql.NullFloat64 to distinguish "caller forgot
// to pass it" (Valid=false → we skip the PnL leg and log a
// warning) from "PnL is genuinely zero" (Valid=true, Float64=0).
// On an OPEN the leg is never written regardless; on a CLOSE
// missing PnL is a bug worth surfacing.
//
// Idempotency keys are namespaced under "trade:<id>:futures:<leg>"
// so a replay can't collide with the legacy "trade:<id>:notional"
// key (different vocabulary, different row).
func (e *runtimeTradingEngine) recordCashLedgerFuturesForFill(
	ctx context.Context,
	fund *repository.Fund,
	plan *repository.InvestmentPlan,
	action repository.PlanAction,
	tradeID string,
	side string,
	quantity int,
	executionPrice float64,
	notional float64,
	feeCommission, feeStampTax, feeTransfer float64,
	executedAt time.Time,
	realizedPnL sql.NullFloat64,
) {
	currency := "USD"
	if action.QuoteCurrency.Valid && strings.TrimSpace(action.QuoteCurrency.String) != "" {
		currency = strings.ToUpper(strings.TrimSpace(action.QuoteCurrency.String))
	}
	tradingDate := executedAt
	planID := ""
	if plan != nil {
		planID = plan.ID
		if !plan.TradingDate.IsZero() {
			tradingDate = plan.TradingDate
		}
	}
	desc := fmt.Sprintf("futures %s %d %s @ %.4f", side, quantity, action.Symbol, executionPrice)
	commonMeta := map[string]any{
		"symbol":    action.Symbol,
		"quantity":  quantity,
		"price":     executionPrice,
		"action_id": action.ID,
	}

	initialMargin := futuresMarginRequired(notional, action.Leverage)
	openClose := strings.ToLower(strings.TrimSpace(action.OpenClose.String))
	// Default to "open" when unset to match the runtime engine's
	// own default in the futures branch of executePlanAction.
	if openClose == "" {
		openClose = "open"
	}

	type leg struct {
		entryType string
		amount    float64
		key       string
	}
	var legs []leg
	if openClose == "open" {
		legs = []leg{
			{entryType: repository.CashEntryFuturesMarginPost, amount: -initialMargin, key: "margin_post"},
			{entryType: repository.CashEntryTradeBuyCommission, amount: -feeCommission, key: "commission"},
			{entryType: repository.CashEntryTradeBuyTransfer, amount: -feeTransfer, key: "transfer"},
			{entryType: repository.CashEntryTradeBuyStampTax, amount: -feeStampTax, key: "stamp_tax"},
		}
	} else {
		legs = []leg{
			{entryType: repository.CashEntryFuturesMarginRelease, amount: initialMargin, key: "margin_release"},
			{entryType: repository.CashEntryTradeSellCommission, amount: -feeCommission, key: "commission"},
			{entryType: repository.CashEntryTradeSellTransfer, amount: -feeTransfer, key: "transfer"},
			{entryType: repository.CashEntryTradeSellStampTax, amount: -feeStampTax, key: "stamp_tax"},
		}
		if realizedPnL.Valid {
			legs = append(legs, leg{entryType: repository.CashEntryFuturesRealizedPnL, amount: realizedPnL.Float64, key: "realized_pnl"})
		} else {
			// CLOSE without realizedPnL is almost certainly a
			// caller bug — log loudly but don't fail the trade.
			slog.Warn("cash_ledger: futures CLOSE missing realizedPnL — PnL leg skipped",
				"fund_id", fund.ID,
				"trade_id", tradeID,
				"action_id", action.ID,
			)
		}
	}
	for _, l := range legs {
		if l.amount == 0 {
			continue
		}
		params := repository.AppendParams{
			FundID:         fund.ID,
			PostedAt:       executedAt,
			TradingDate:    &tradingDate,
			EntryType:      l.entryType,
			Amount:         roundCurrency(l.amount),
			Currency:       currency,
			TradeID:        tradeID,
			PlanID:         planID,
			PlanActionID:   action.ID,
			Description:    desc,
			Metadata:       commonMeta,
			IdempotencyKey: fmt.Sprintf("trade:%s:futures:%s", tradeID, l.key),
		}
		if _, err := e.cashLedger.Append(ctx, params); err != nil {
			slog.Warn("cash_ledger: append failed (futures)",
				"fund_id", fund.ID,
				"trade_id", tradeID,
				"entry_type", l.entryType,
				"err", err.Error())
			if e.metrics != nil {
				e.metrics.RecordCashLedgerWriteFailure(l.entryType)
			}
		}
	}
}

// recordLotFill bridges a successful trade-fill into the FIFO
// lot ledger introduced in Phase 3A-1. The bridge is intentionally
// soft: any error from the ledger gets logged but is never
// surfaced upward, because the authoritative position state
// already lives in holding_positions + trade_executions and the
// lot ledger is a derived attribution shadow.
//
// Filtering rules:
//   - status must be "filled" — pending/partial/rejected fills
//     have no closing semantics yet.
//   - filledPrice must be valid; otherwise we don't have a
//     defensible entry/exit price to record.
//   - Futures sides are mapped through lotledger.ClassifyFuturesSide;
//     short-side opens (open_short) currently yield an empty
//     classification and are skipped. Phase 3A onwards models
//     long-only lots; PR-3A-X will add the short side.
func (e *runtimeTradingEngine) recordLotFill(
	ctx context.Context,
	fund *repository.Fund,
	action repository.PlanAction,
	tradeID string,
	side string,
	quantity int,
	filledPrice sql.NullFloat64,
	planPrice float64,
	totalFees float64,
	executedAt time.Time,
	status string,
) {
	if e == nil || e.lotLedger == nil || e.uow == nil {
		return
	}
	if status != "filled" {
		return
	}
	if quantity <= 0 {
		return
	}
	if fund == nil {
		return
	}
	// Phase 3A-1 models long lots only. Short positions
	// (position_side == "short", typically futures) flip the
	// trade-direction semantics — a "sell" opens the position and
	// a "buy" closes it — which the FIFO buy→sell ledger here
	// can't represent. Skip them outright; a later phase will
	// add a parallel short-lot ledger.
	if strings.EqualFold(strings.TrimSpace(action.PositionSide.String), "short") {
		return
	}
	canonicalSide := lotledger.ClassifyFuturesSide(side)
	if canonicalSide == "" {
		return
	}
	price := planPrice
	if filledPrice.Valid && filledPrice.Float64 > 0 {
		price = filledPrice.Float64
	}
	if price <= 0 {
		return
	}
	instrumentKey := firstNonEmptyValue(action.InstrumentKey, buildInstrumentKey(action.Exchange.String, action.Symbol), action.Symbol)

	// Default attribution: if the PlanAction didn't already carry
	// a sleeve label (early Phase 3A builds before the classical
	// strategy engines come online) we stamp "llm_pm" so the
	// scorecard has a non-empty baseline bucket. Same for
	// signal_source. For sells without an explicit exit_reason
	// (e.g. LLM-proposed reductions), we default to "llm_decision"
	// so the attribution column never collapses to NULL.
	sleeve := action.Sleeve
	if !sleeve.Valid || strings.TrimSpace(sleeve.String) == "" {
		sleeve = sql.NullString{String: "llm_pm", Valid: true}
	}
	signalSource := action.SignalSource
	if !signalSource.Valid || strings.TrimSpace(signalSource.String) == "" {
		signalSource = sql.NullString{String: "llm_pm", Valid: true}
	}
	exitReason := action.ExitReason
	if canonicalSide == "sell" && (!exitReason.Valid || strings.TrimSpace(exitReason.String) == "") {
		exitReason = sql.NullString{String: "llm_decision", Valid: true}
	}

	ev := lotledger.FillEvent{
		FundID:            fund.ID,
		PlanActionID:      nullUUID(action.ID),
		TradeExecutionID:  tradeID,
		InstrumentKey:     instrumentKey,
		Symbol:            action.Symbol,
		Market:            action.Market,
		AssetClass:        action.AssetClass,
		Side:              canonicalSide,
		Quantity:          float64(quantity),
		FilledPrice:       price,
		TotalFees:         totalFees,
		ExecutedAt:        executedAt,
		Sleeve:            sleeve,
		RegimeTag:         action.RegimeTag,
		SignalSource:      signalSource,
		ConfidenceAtEntry: action.Confidence,
		ExitReason:        exitReason,
		RegimeAtExit:      action.RegimeTag,
	}
	if _, err := e.lotLedger.RecordWithUoW(ctx, e.uow, ev); err != nil {
		if e.metrics != nil {
			e.metrics.RecordLotLedgerFailure(canonicalSide, action.Symbol)
		}
		slog.WarnContext(ctx, "lotledger: record failed",
			slog.String("fund_id", fund.ID),
			slog.String("symbol", action.Symbol),
			slog.String("side", canonicalSide),
			slog.String("trade_id", tradeID),
			slog.Any("err", err),
		)
	}
}

// computeSlippagePct returns the signed fractional drift between the
// plan reference price and the actual fill price. Returns an invalid
// NullFloat64 (NULL) for sells, missing inputs, or when planPrice is
// zero — these are documented as "non-priced fills" in the column
// comment on trade_executions.slippage_pct.
func computeSlippagePct(side string, planPrice float64, filledPrice sql.NullFloat64) sql.NullFloat64 {
	if strings.EqualFold(strings.TrimSpace(side), "sell") {
		return sql.NullFloat64{}
	}
	if !filledPrice.Valid || filledPrice.Float64 <= 0 || planPrice <= 0 {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{
		Float64: (filledPrice.Float64 - planPrice) / planPrice,
		Valid:   true,
	}
}

func (e *runtimeTradingEngine) syncPlanActionStatuses(ctx context.Context, planID string, statuses map[string]string) error {
	if len(statuses) == 0 {
		return nil
	}
	tx, err := e.planRepo.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for actionID, status := range statuses {
		if strings.TrimSpace(actionID) == "" || strings.TrimSpace(status) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE plan_actions SET execution_status = $1 WHERE id = $2 AND plan_id = $3`,
			status, actionID, planID,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (e *runtimeTradingEngine) syncPlanActionReasoning(ctx context.Context, planID string, actions []repository.PlanAction) error {
	if len(actions) == 0 {
		return nil
	}
	tx, err := e.planRepo.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, action := range actions {
		if strings.TrimSpace(action.ID) == "" || !action.Reasoning.Valid {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE plan_actions SET reasoning = $1 WHERE id = $2 AND plan_id = $3`,
			action.Reasoning, action.ID, planID,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func findFundAgentByRole(ctx context.Context, fundID, role string, teamRepo *repository.TeamRepo, agentRepo *repository.AgentRepo) *repository.Agent {
	return findFundAgentByRoleWithFocus(ctx, fundID, role, "", teamRepo, agentRepo, nil)
}

func findFundAgentByRoleWithFocus(ctx context.Context, fundID, role, focus string, teamRepo *repository.TeamRepo, agentRepo *repository.AgentRepo, fundRepo *repository.FundRepo) *repository.Agent {
	if teamRepo == nil || agentRepo == nil {
		return nil
	}
	members, err := teamRepo.ListByFund(ctx, fundID)
	if err != nil {
		return nil
	}
	var fund *repository.Fund
	if fundRepo != nil {
		fund, _ = fundRepo.GetByID(ctx, fundID)
	}
	selected := selectBestTeamAgent(ctx, members, role, focus, fund, agentRepo)
	if selected == nil {
		return nil
	}
	return selected
}

func selectBestTeamAgent(ctx context.Context, members []repository.TeamMember, role, focus string, fund *repository.Fund, agentRepo *repository.AgentRepo) *repository.Agent {
	type candidate struct {
		agent *repository.Agent
		score int
	}
	var best *candidate
	for i := range members {
		member := members[i]
		if !strings.EqualFold(member.Role, role) {
			continue
		}
		agent, err := agentRepo.GetByID(ctx, member.AgentID)
		if err != nil {
			continue
		}
		score, ok := teamAgentMatchScore(member, agent, focus, fund)
		if !ok {
			continue
		}
		if best == nil || score > best.score {
			best = &candidate{agent: agent, score: score}
		}
	}
	if best == nil {
		return nil
	}
	return best.agent
}

func teamAgentMatchScore(member repository.TeamMember, agent *repository.Agent, focus string, fund *repository.Fund) (int, bool) {
	score := 0
	normalizedFocus := strings.ToLower(strings.TrimSpace(focus))
	legacyFocus := strings.ToLower(strings.TrimSpace(member.Focus.String))
	coverage := extractAgentCoverage(agent)
	if len(coverage.Markets) > 0 || len(coverage.AssetClasses) > 0 || len(coverage.Directions) > 0 {
		matchedCoverage := false
		if coverageMatchesFund(coverage, fund) {
			score += 10
			matchedCoverage = true
		} else if fund != nil {
			return 0, false
		}
		if coverageMatchesFocus(coverage, normalizedFocus) {
			score += 5
			matchedCoverage = true
		}
		if normalizedFocus != "" && !matchedCoverage && legacyFocus != normalizedFocus {
			return 0, false
		}
	}
	if normalizedFocus != "" {
		if legacyFocus == normalizedFocus {
			score += 3
		} else if len(coverage.Markets) == 0 && len(coverage.AssetClasses) == 0 && len(coverage.Directions) == 0 {
			return 0, false
		}
	}
	score += specializationAffinityScore(agent, fund)
	if strings.EqualFold(agent.Status, "active") {
		score += 1
	}
	return score, true
}

func coverageMatchesFund(coverage agentCoverage, fund *repository.Fund) bool {
	if fund == nil {
		return false
	}
	profile := decodeFundMarketProfile(fund.Config)
	matched := false
	if len(coverage.Markets) > 0 {
		matched = stringInSliceFold(profile.Market, coverage.Markets)
		if !matched {
			return false
		}
	}
	if len(coverage.AssetClasses) > 0 {
		matched = true
		if !stringInSliceFold(profile.AssetClass, coverage.AssetClasses) {
			return false
		}
	}
	if len(coverage.Directions) > 0 {
		matched = true
		if !stringInSliceFold(profile.PrimaryDirection, coverage.Directions) {
			return false
		}
	}
	return matched
}

func coverageMatchesFocus(coverage agentCoverage, focus string) bool {
	if focus == "" {
		return false
	}
	for _, direction := range coverage.Directions {
		if strings.EqualFold(direction, focusToDirection(focus)) {
			return true
		}
	}
	for _, assetClass := range coverage.AssetClasses {
		if strings.EqualFold(assetClass, focusToAssetClass(focus)) {
			return true
		}
	}
	for _, market := range coverage.Markets {
		if strings.EqualFold(market, focusToMarket(focus)) {
			return true
		}
	}
	return false
}

type agentCoverage struct {
	Markets      []string `json:"markets"`
	AssetClasses []string `json:"assetClasses"`
	Directions   []string `json:"directions"`
}

type agentDomainConfig struct {
	Coverage       agentCoverage        `json:"coverage"`
	Specialization *agentSpecialization `json:"specialization,omitempty"`
}

func parseAgentDomainConfig(agent *repository.Agent) agentDomainConfig {
	if agent == nil || len(agent.DomainConfig) == 0 || string(agent.DomainConfig) == "null" {
		return agentDomainConfig{}
	}
	var config agentDomainConfig
	if err := json.Unmarshal(agent.DomainConfig, &config); err != nil {
		return agentDomainConfig{}
	}
	return config
}

func extractAgentCoverage(agent *repository.Agent) agentCoverage {
	config := parseAgentDomainConfig(agent)
	return agentCoverage{
		Markets:      normalizedStringSlice(config.Coverage.Markets),
		AssetClasses: normalizedStringSlice(config.Coverage.AssetClasses),
		Directions:   normalizedStringSlice(config.Coverage.Directions),
	}
}

func extractAgentSpecialization(agent *repository.Agent) *agentSpecialization {
	config := parseAgentDomainConfig(agent)
	return normalizeAgentSpecialization(config.Specialization)
}

func normalizeAgentSpecialization(specialization *agentSpecialization) *agentSpecialization {
	if specialization == nil {
		return nil
	}
	normalized := &agentSpecialization{
		Markets:      normalizedStringSlice(specialization.Markets),
		AssetClasses: normalizedStringSlice(specialization.AssetClasses),
		Themes:       normalizedStringSlice(specialization.Themes),
		Instruments:  normalizedStringSlice(specialization.Instruments),
		StyleHints:   normalizedStringSlice(specialization.StyleHints),
		Patterns:     normalizedStringSlice(specialization.Patterns),
	}
	if len(normalized.Markets) == 0 && len(normalized.AssetClasses) == 0 && len(normalized.Themes) == 0 && len(normalized.Instruments) == 0 && len(normalized.StyleHints) == 0 && len(normalized.Patterns) == 0 {
		return nil
	}
	return normalized
}

func specializationAffinityScore(agent *repository.Agent, fund *repository.Fund) int {
	if agent == nil || fund == nil {
		return 0
	}
	profile := decodeFundMarketProfile(fund.Config)
	staticSpecialization := extractAgentSpecialization(agent)
	learnedSpecialization := extractEvolutionSpecialization(agent.EvolutionConfig)
	markets, assetClasses, themes, instruments, styleHints := collectFundSpecializationTargets(profile)
	score := 0
	if staticSpecialization != nil {
		score += overlapScore(markets, staticSpecialization.Markets, 3, 6)
		score += overlapScore(assetClasses, staticSpecialization.AssetClasses, 3, 6)
		score += overlapScore(themes, staticSpecialization.Themes, 1, 3)
		score += overlapScore(instruments, staticSpecialization.Instruments, 1, 2)
		score += overlapScore(styleHints, staticSpecialization.StyleHints, 1, 2)
	}
	score += learnedMapAffinityScore(markets, learnedSpecialization.Markets, 2)
	score += learnedMapAffinityScore(assetClasses, learnedSpecialization.AssetClasses, 2)
	score += learnedMapAffinityScore(themes, learnedSpecialization.Themes, 2)
	score += learnedMapAffinityScore(instruments, learnedSpecialization.Instruments, 2)
	score += learnedMapAffinityScore(styleHints, learnedSpecialization.StyleHints, 2)
	return score
}

func collectFundSpecializationTargets(profile fundMarketProfile) ([]string, []string, []string, []string, []string) {
	markets := []string{}
	assetClasses := []string{}
	themes := []string{}
	instruments := []string{}
	styleHints := []string{}
	if value := strings.TrimSpace(profile.Market); value != "" {
		markets = append(markets, value)
	}
	if value := strings.TrimSpace(profile.AssetClass); value != "" {
		assetClasses = append(assetClasses, value)
	}
	if value := strings.TrimSpace(profile.PrimaryDirection); value != "" {
		styleHints = append(styleHints, value)
	}
	if profile.Universe != nil {
		themes = append(themes, profile.Universe.Themes...)
		instruments = append(instruments, profile.Universe.Symbols...)
	}
	if profile.Specialization != nil && profile.Specialization.Team != nil {
		markets = append(markets, profile.Specialization.Team.Markets...)
		assetClasses = append(assetClasses, profile.Specialization.Team.AssetClasses...)
		themes = append(themes, profile.Specialization.Team.Themes...)
		instruments = append(instruments, profile.Specialization.Team.Instruments...)
		styleHints = append(styleHints, profile.Specialization.Team.StyleHints...)
	}
	return normalizedStringSlice(markets), normalizedStringSlice(assetClasses), normalizedStringSlice(themes), normalizedStringSlice(instruments), normalizedStringSlice(styleHints)
}

func overlapScore(targets, candidates []string, pointsPerMatch, cap int) int {
	if len(targets) == 0 || len(candidates) == 0 || pointsPerMatch <= 0 || cap <= 0 {
		return 0
	}
	score := 0
	for _, target := range targets {
		if stringInSliceFold(target, candidates) {
			score += pointsPerMatch
			if score >= cap {
				return cap
			}
		}
	}
	return score
}

func learnedMapAffinityScore(targets []string, values map[string]float64, cap float64) int {
	if len(targets) == 0 || len(values) == 0 || cap <= 0 {
		return 0
	}
	score := 0.0
	for _, target := range targets {
		for key, value := range values {
			if value > 0 && strings.EqualFold(strings.TrimSpace(key), strings.TrimSpace(target)) {
				score += value
				break
			}
		}
	}
	return int(math.Round(math.Min(score, cap)))
}

func normalizedStringSlice(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		for _, part := range splitNormalizedValues(value) {
			key := strings.ToLower(part)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, part)
		}
	}
	return result
}

func splitNormalizedValues(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		switch r {
		case '\n', '\r', ',', '，', '、', ';', '；':
			return true
		default:
			return false
		}
	})
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func stringInSliceFold(target string, values []string) bool {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return false
	}
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), trimmed) {
			return true
		}
	}
	return false
}

func focusToDirection(focus string) string {
	switch strings.ToLower(strings.TrimSpace(focus)) {
	case "stock", "fundamental":
		return "stocks"
	case "macro":
		return "multi_asset"
	default:
		return ""
	}
}

func focusToAssetClass(focus string) string {
	switch strings.ToLower(strings.TrimSpace(focus)) {
	case "stock", "fundamental":
		return "equity"
	default:
		return ""
	}
}

func focusToMarket(focus string) string {
	switch strings.ToLower(strings.TrimSpace(focus)) {
	case "stock", "fundamental":
		return "us_equity"
	default:
		return ""
	}
}

func (e *runtimeTradingEngine) persistPortfolioState(ctx context.Context, fund *repository.Fund, positions map[string]repository.HoldingPosition, availableCash float64, tradingDate time.Time) error {
	if err := e.positionRepo.DeleteAllByFund(ctx, fund.ID); err != nil {
		return mapRepositoryError(err)
	}
	positionList := make([]repository.HoldingPosition, 0, len(positions))
	for _, position := range positions {
		e.refreshPositionQuote(ctx, fund, &position)
		applyPositionValuation(&position)
		positionList = append(positionList, position)
	}
	sort.Slice(positionList, func(i, j int) bool {
		return positionMapKey(positionList[i].InstrumentKey, positionList[i].Symbol) < positionMapKey(positionList[j].InstrumentKey, positionList[j].Symbol)
	})
	totalMarketValue := 0.0
	for i := range positionList {
		totalMarketValue += positionList[i].MarketValue
	}
	totalAssets := roundCurrency(availableCash + totalMarketValue)
	nav := 0.0
	if fund.InitialCapital > 0 {
		nav = totalAssets / fund.InitialCapital
	}
	for i := range positionList {
		if totalAssets > 0 {
			positionList[i].Weight = positionList[i].MarketValue / totalAssets
		}
		if err := e.positionRepo.Upsert(ctx, &positionList[i]); err != nil {
			return mapRepositoryError(err)
		}
	}
	latest, err := e.navRepo.GetLatest(ctx, fund.ID)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return mapRepositoryError(err)
	}
	dailyReturn := 0.0
	totalReturn := 0.0
	if latest != nil && latest.TotalAssets > 0 {
		dailyReturn = (totalAssets - latest.TotalAssets) / latest.TotalAssets
	}
	if fund.InitialCapital > 0 {
		totalReturn = (totalAssets - fund.InitialCapital) / fund.InitialCapital
	}
	positionsSnapshot, err := json.Marshal(positionList)
	if err != nil {
		return err
	}
	if err := e.upsertNavSnapshot(ctx, &repository.NavSnapshot{
		FundID:            fund.ID,
		TradingDate:       tradingDate,
		NAV:               nav,
		TotalAssets:       totalAssets,
		TotalMarketValue:  roundCurrency(totalMarketValue),
		AvailableCash:     roundCurrency(availableCash),
		DailyReturn:       dailyReturn,
		TotalReturn:       totalReturn,
		PositionsSnapshot: positionsSnapshot,
	}); err != nil {
		return err
	}
	return mapRepositoryError(e.fundRepo.UpdateCapital(ctx, fund.ID, roundCurrency(availableCash), totalAssets, nav))
}

func (e *runtimeTradingEngine) upsertNavSnapshot(ctx context.Context, snapshot *repository.NavSnapshot) error {
	if snapshot == nil {
		return api.ErrBadInput
	}
	if err := e.navRepo.Create(ctx, snapshot); err != nil {
		if !strings.Contains(err.Error(), "duplicate key") {
			return mapRepositoryError(err)
		}
		positionsJSON := []byte(snapshot.PositionsSnapshot)
		if len(positionsJSON) == 0 {
			positionsJSON = []byte(`[]`)
		}
		_, updateErr := e.navRepo.DB().ExecContext(ctx,
			`UPDATE nav_snapshots SET nav = $1, total_assets = $2, total_market_value = $3, available_cash = $4, daily_return = $5, total_return = $6, positions_snapshot = $7, created_at = NOW() WHERE fund_id = $8 AND trading_date = $9`,
			snapshot.NAV, snapshot.TotalAssets, snapshot.TotalMarketValue, snapshot.AvailableCash, snapshot.DailyReturn, snapshot.TotalReturn, positionsJSON, snapshot.FundID, snapshot.TradingDate,
		)
		if updateErr != nil {
			return updateErr
		}
	}
	return nil
}

func normalizeExecutionAction(action repository.PlanAction) (side string, quantity int, price float64, amount float64) {
	switch strings.ToLower(strings.TrimSpace(action.Action)) {
	case "buy", "add":
		side = "buy"
	case "sell", "reduce":
		side = "sell"
	case "hold", "watch":
		return "", 0, 0, 0
	default:
		return "", 0, 0, 0
	}
	if action.Price.Valid && action.Price.Float64 > 0 {
		price = action.Price.Float64
	} else if action.Amount.Valid && action.Quantity.Valid && action.Quantity.Float64 > 0 {
		price = action.Amount.Float64 / action.Quantity.Float64
	}
	quantity = normalizedQuantity(action.Quantity, action.Amount, price)
	if quantity <= 0 {
		return side, 0, price, 0
	}
	if price <= 0 && action.Amount.Valid && action.Amount.Float64 > 0 {
		price = action.Amount.Float64 / float64(quantity)
	}
	if price <= 0 {
		return side, quantity, 0, 0
	}
	amount = roundCurrency(float64(quantity) * price)
	if isFuturesAction(action) {
		multiplier := contractMultiplierValue(action.ContractMultiplier)
		amount = roundCurrency(float64(quantity) * price * multiplier)
	}
	return side, quantity, price, amount
}

func (a *runtimePMAgent) quoteForAction(ctx context.Context, instrument marketdata.InstrumentRef) (*marketdata.QuoteSnapshot, error) {
	if a.marketData == nil || !a.marketData.Enabled() {
		return nil, marketdata.ErrQuoteUnavailable
	}
	return a.marketData.GetQuote(ctx, instrument)
}

// planBuyAmountWithinRiskCap returns the notional budget the PM should
// target for a fresh buy action so the resulting order survives
// risk.MaxOrderNotionalLimit.
//
// Historically the planner hardcoded 25% of CurrentCapital. That number
// happily exceeded the default 10% hard cap (or any tighter operator
// override), which left the daily workflow stuck in a
// "trade execution failed: hard risk gate rejected … exceeds hard cap"
// loop: every plan got generated, every plan got rejected, no orders
// landed. We now consult the fund's own hard-risk config (with the
// same normalization the execution gate uses) and intersect that with
// the legacy 25% heuristic so the planner is *consistent with* the
// gate by construction.
//
// Returns at least a positive amount so downstream `Math.floor` /
// quantity sizing still works for tiny accounts.
func planBuyAmountWithinRiskCap(fund *repository.Fund) float64 {
	if fund == nil {
		return 1000
	}
	nav := fund.CurrentCapital
	if nav <= 0 {
		nav = fund.InitialCapital
	}
	if nav <= 0 {
		return 1000
	}
	// Soft planner budget — defensive default that matches historical
	// behavior for funds without a custom hard-risk policy.
	budget := roundCurrency(nav * 0.25)

	// Honor the operator's hard cap (per-fund or defaults). We intersect
	// MaxOrderPctOfAssets * TotalAssets and the absolute MaxOrderAmount,
	// because that's exactly what risk.MaxOrderNotionalLimit will do at
	// execution time. If either cap is tighter than our soft budget we
	// shrink to the tighter cap so the proposal lands cleanly.
	profile := decodeFundMarketProfile(fund.Config)
	cfg := riskHardConfigFromAPI(profile.HardRisk)
	totalAssets := fund.TotalAssets
	if totalAssets <= 0 {
		totalAssets = nav
	}
	var hardCap float64
	if cfg.MaxOrderPctOfAssets > 0 && totalAssets > 0 {
		hardCap = cfg.MaxOrderPctOfAssets * totalAssets
	}
	if cfg.MaxOrderAmount > 0 && (hardCap <= 0 || cfg.MaxOrderAmount < hardCap) {
		hardCap = cfg.MaxOrderAmount
	}
	if hardCap > 0 {
		// Leave a small buffer between the planner's budget and the
		// execution-time hard cap. The cap is recomputed at dispatch
		// against the *live* TotalAssets — if a held position has
		// dipped a few percent between plan creation and dispatch
		// (very common in equities), an exact-cap plan misses the
		// gate by a hair and rejects, e.g. "10247.86 exceeds 10027.78"
		// after a 2% market-value drift on the rest of the book.
		// PlanBudgetSafetyMargin pulls the planner-side cap below the
		// dispatch-side cap so this race no longer produces a
		// rejection on a plan that was perfectly legal at write time.
		// Set conservatively to absorb up to ~3% TotalAssets drift,
		// which covers most intra-session moves on equity portfolios.
		hardCap = roundCurrency(hardCap * PlanBudgetSafetyMargin)
		if hardCap < budget {
			budget = hardCap
		}
	}
	if budget <= 0 {
		budget = 1000
	}
	return budget
}

// PlanBudgetSafetyMargin is the fraction of the dispatch-time hard
// cap that the planner is willing to *propose*. The 0.97 figure
// gives a ~3% cushion: if TotalAssets drifts down by less than 3%
// between plan write and dispatch, the plan still survives the
// hard-risk gate. Exposed as a package constant so the test suite
// can pin the contract.
const PlanBudgetSafetyMargin = 0.97

func (e *runtimeTradingEngine) quoteForAction(ctx context.Context, fund *repository.Fund, action repository.PlanAction) (*marketdata.QuoteSnapshot, error) {
	if e.marketData == nil || !e.marketData.Enabled() {
		return nil, marketdata.ErrQuoteUnavailable
	}
	instrument := planActionInstrumentRef(
		fund,
		action.Symbol,
		action.InstrumentKey,
		action.Market.String,
		action.Exchange.String,
		action.AssetClass.String,
		action.InstrumentType.String,
		action.QuoteCurrency.String,
		action.SettlementCurrency.String,
		contractMultiplierValue(action.ContractMultiplier),
		formatNullTime(action.ExpiryDate),
	)
	return e.marketData.GetQuote(ctx, instrument)
}

func (e *runtimeTradingEngine) refreshPositionQuote(ctx context.Context, fund *repository.Fund, position *repository.HoldingPosition) {
	if position == nil || strings.TrimSpace(position.Symbol) == "" || e.marketData == nil || !e.marketData.Enabled() {
		return
	}
	quote, err := e.marketData.GetQuote(ctx, planActionInstrumentRef(fund, position.Symbol, position.InstrumentKey, position.Market.String, position.Exchange.String, position.AssetClass.String, position.InstrumentType.String, position.QuoteCurrency.String, position.SettlementCurrency.String, contractMultiplierValue(position.ContractMultiplier), formatNullTime(position.ExpiryDate)))
	if err != nil || quote == nil || quote.Price <= 0 {
		return
	}
	position.CurrentPrice = quote.Price
	if quote.QuoteCurrency != "" {
		position.QuoteCurrency = nullString(quote.QuoteCurrency)
	}
}

func normalizedQuantity(quantity sql.NullFloat64, amount sql.NullFloat64, price float64) int {
	if quantity.Valid && quantity.Float64 > 0 {
		return int(quantity.Float64)
	}
	if amount.Valid && amount.Float64 > 0 && price > 0 {
		return int(amount.Float64 / price)
	}
	return 0
}

func mergeBoughtPosition(position repository.HoldingPosition, fundID string, action repository.PlanAction, quantity int, price float64) repository.HoldingPosition {
	instrumentKey := firstNonEmptyValue(action.InstrumentKey, buildInstrumentKey(action.Exchange.String, action.Symbol))
	if position.FundID == "" {
		position = repository.HoldingPosition{
			FundID:             fundID,
			InstrumentKey:      instrumentKey,
			Symbol:             action.Symbol,
			Name:               nullString(action.Symbol),
			Market:             action.Market,
			Exchange:           action.Exchange,
			AssetClass:         action.AssetClass,
			InstrumentType:     action.InstrumentType,
			PositionSide:       action.PositionSide,
			QuoteCurrency:      action.QuoteCurrency,
			SettlementCurrency: action.SettlementCurrency,
			MarginMode:         action.MarginMode,
			Leverage:           action.Leverage,
			ContractMultiplier: action.ContractMultiplier,
			ExpiryDate:         action.ExpiryDate,
			Quantity:           0,
		}
	}
	totalQuantity := position.Quantity + float64(quantity)
	if totalQuantity <= 0 {
		return position
	}
	costBasis := position.CostPrice*position.Quantity + price*float64(quantity)
	// Capture the prior unlocked balance BEFORE applyActionMetadataToPosition
	// reassigns any fields; we need the original AvailableQty so a T+1
	// buy on top of an existing settled position doesn't accidentally
	// lock the older lots too.
	priorAvailable := position.AvailableQty
	position = applyActionMetadataToPosition(position, fundID, action)
	position.Quantity = totalQuantity
	// Settlement lock is a *market* rule, not a per-symbol one:
	// the A-share market is T+1 (every SH/SZ/ChiNext/STAR/BSE
	// instrument), and freshly bought shares can't be sold until the
	// next trading day. (e *runtimeTradingEngine).Settle is the
	// platform's "next trading day" boundary and releases the lock.
	// On T+0 markets (US/HK/crypto/futures via the non-futures path)
	// AvailableQty mirrors Quantity exactly.
	cycle := instrument2.SettlementCycleFor(action.Symbol, instrument2.Hint{
		Market:     action.Market.String,
		Exchange:   action.Exchange.String,
		AssetClass: action.AssetClass.String,
	})
	if cycle.IsLocked() {
		// New lot is locked; keep the previously settled portion
		// available for sale. Cap at totalQuantity so legacy positions
		// that somehow had AvailableQty > Quantity don't end up with
		// an impossible state after a buy.
		if priorAvailable > totalQuantity {
			priorAvailable = totalQuantity
		}
		position.AvailableQty = priorAvailable
	} else {
		position.AvailableQty = totalQuantity
	}
	position.CostPrice = costBasis / totalQuantity
	position.CurrentPrice = price
	applyPositionValuation(&position)
	return position
}

// releaseLockedShares applies the A-share market's T+1 unlock rule
// during Settle: for every position on a T+1 market it raises
// AvailableQty up to the current Quantity, effectively releasing any
// lots that were locked by an intra-session buy. T+0 markets are
// left untouched (their positions were never locked to begin with).
// The rule is uniform across A-share boards — it's a property of the
// market, not the symbol — so we classify via Hint.Market /
// AssetClass rather than per-instrument logic. Idempotent — running
// it twice on the same map has the same effect as running it once,
// which matters because Settle may be invoked multiple times per day
// for re-pricing.
func releaseLockedShares(positionsByKey map[string]repository.HoldingPosition) {
	for key, p := range positionsByKey {
		cycle := instrument2.SettlementCycleFor(p.Symbol, instrument2.Hint{
			Market:     p.Market.String,
			Exchange:   p.Exchange.String,
			AssetClass: p.AssetClass.String,
		})
		if !cycle.IsLocked() {
			continue
		}
		if p.AvailableQty+0.0001 >= p.Quantity {
			continue
		}
		p.AvailableQty = p.Quantity
		positionsByKey[key] = p
	}
}

func executeDirectionForAction(action repository.PlanAction, fallback string) string {
	openClose := strings.ToLower(strings.TrimSpace(action.OpenClose.String))
	positionSide := strings.ToLower(strings.TrimSpace(action.PositionSide.String))
	side := strings.ToLower(strings.TrimSpace(fallback))
	if openClose == "close" || openClose == "close_today" || openClose == "roll" {
		switch positionSide {
		case "short":
			return "buy"
		case "long":
			return "sell"
		}
	}
	if positionSide == "short" {
		return "sell"
	}
	if positionSide == "long" {
		return "buy"
	}
	return side
}

func isFuturesAction(action repository.PlanAction) bool {
	if strings.EqualFold(strings.TrimSpace(action.AssetClass.String), "futures") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(action.Market.String), "futures") {
		return true
	}
	if strings.TrimSpace(action.OpenClose.String) != "" {
		return true
	}
	if strings.TrimSpace(action.PositionSide.String) != "" {
		return true
	}
	return false
}

func contractMultiplierValue(value sql.NullFloat64) float64 {
	if value.Valid && value.Float64 > 0 {
		return value.Float64
	}
	return 1
}

func leverageValue(value sql.NullFloat64) float64 {
	if value.Valid && value.Float64 > 0 {
		return value.Float64
	}
	return 1
}

func futuresMarginRequired(notional float64, leverage sql.NullFloat64) float64 {
	lev := leverageValue(leverage)
	if lev <= 0 {
		lev = 1
	}
	return roundCurrency(notional / lev)
}

func applyActionMetadataToPosition(position repository.HoldingPosition, fundID string, action repository.PlanAction) repository.HoldingPosition {
	instrumentKey := firstNonEmptyValue(action.InstrumentKey, buildInstrumentKey(action.Exchange.String, action.Symbol), action.Symbol)
	if position.FundID == "" {
		position.FundID = fundID
	}
	position.InstrumentKey = instrumentKey
	position.Symbol = action.Symbol
	position.Name = nullString(action.Symbol)
	position.Market = action.Market
	position.Exchange = action.Exchange
	position.AssetClass = action.AssetClass
	position.InstrumentType = action.InstrumentType
	if action.PositionSide.Valid {
		position.PositionSide = action.PositionSide
	}
	if action.QuoteCurrency.Valid {
		position.QuoteCurrency = action.QuoteCurrency
	}
	if action.SettlementCurrency.Valid {
		position.SettlementCurrency = action.SettlementCurrency
	}
	if action.MarginMode.Valid {
		position.MarginMode = action.MarginMode
	}
	if action.Leverage.Valid {
		position.Leverage = action.Leverage
	}
	if action.ContractMultiplier.Valid {
		position.ContractMultiplier = action.ContractMultiplier
	}
	if action.ExpiryDate.Valid {
		position.ExpiryDate = action.ExpiryDate
	}
	return position
}

func applyPositionValuation(position *repository.HoldingPosition) {
	if position == nil {
		return
	}
	multiplier := contractMultiplierValue(position.ContractMultiplier)
	marketValue := roundCurrency(position.Quantity * position.CurrentPrice * multiplier)
	if isFuturesPosition(*position) {
		position.MarketValue = marketValue
		if position.CostPrice > 0 && position.CurrentPrice > 0 {
			delta := position.CurrentPrice - position.CostPrice
			if strings.EqualFold(strings.TrimSpace(position.PositionSide.String), "short") {
				delta = position.CostPrice - position.CurrentPrice
			}
			pnl := roundCurrency(delta * position.Quantity * multiplier)
			position.UnrealizedPnL = sql.NullFloat64{Float64: pnl, Valid: true}
		}
		marginUsed := futuresMarginRequired(marketValue, position.Leverage)
		position.MarginUsed = sql.NullFloat64{Float64: marginUsed, Valid: marginUsed > 0}
		return
	}
	position.MarketValue = roundCurrency(position.Quantity * position.CurrentPrice)
}

func isFuturesPosition(position repository.HoldingPosition) bool {
	if strings.EqualFold(strings.TrimSpace(position.AssetClass.String), "futures") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(position.Market.String), "futures")
}

func (e *runtimeTradingEngine) executeFuturesPlanAction(
	ctx context.Context,
	fund *repository.Fund,
	plan *repository.InvestmentPlan,
	action repository.PlanAction,
	positionKey string,
	position repository.HoldingPosition,
	positionsByKey map[string]repository.HoldingPosition,
	availableCash *float64,
	side string,
	quantity int,
	planPrice float64,
	orderPrice float64,
	amount float64,
	status string,
	filledPrice sql.NullFloat64,
	feeCommission float64,
	feeStampTax float64,
	feeTransfer float64,
) (string, error) {
	executionSide := executeDirectionForAction(action, side)
	openClose := strings.ToLower(strings.TrimSpace(action.OpenClose.String))
	if openClose == "" {
		openClose = "open"
	}
	marginRequired := futuresMarginRequired(amount, action.Leverage)
	// Strategy label (B-step1) — same selector as equity path so a
	// futures TWAP-eligible open shows up with strategy='twap' even
	// though we still write one trade_execution per action.
	strategy := selectPMPathExecutionStrategy(action, quantity)
	if openClose == "open" {
		totalDebit := marginRequired + feeCommission + feeTransfer + feeStampTax
		if totalDebit > *availableCash+0.0001 {
			return "rejected", api.ErrConflict
		}
		// Futures open: no realized PnL leg — opening a position
		// doesn't realize anything. Pass the zero value so the
		// v2 cash flow records margin_post + fees, with the PnL
		// row deliberately skipped.
		rolledStatus, err := e.tradeRepoCreateAndFill(ctx, fund, plan, action, executionSide, quantity, planPrice, amount, status, filledPrice, feeCommission, feeStampTax, feeTransfer, strategy, sql.NullFloat64{})
		if err != nil {
			return "rejected", err
		}
		position = applyActionMetadataToPosition(position, fund.ID, action)
		totalQuantity := position.Quantity + float64(quantity)
		costBasis := position.CostPrice*position.Quantity + orderPrice*float64(quantity)
		position.Quantity = totalQuantity
		position.AvailableQty = totalQuantity
		position.CostPrice = costBasis / totalQuantity
		position.CurrentPrice = orderPrice
		applyPositionValuation(&position)
		positionsByKey[positionKey] = position
		*availableCash = roundCurrency(*availableCash - totalDebit)
		// splitterEnabledForSide currently excludes futures so
		// rolledStatus is always "" here; keep the guard for
		// when futures splitting lands so a regression doesn't
		// silently drop the rollup.
		if rolledStatus != "" {
			return rolledStatus, nil
		}
		return status, nil
	}
	if position.FundID == "" || position.Quantity < float64(quantity)-0.0001 {
		return "rejected", api.ErrConflict
	}
	// Futures close: compute realized PnL UP FRONT so we can
	// pass it down to the cash-ledger writer. PnL sign convention:
	//   long close:  (close - cost) * qty * multiplier
	//   short close: (cost - close) * qty * multiplier
	// Both are dollars-realized (positive = profit), matching
	// what funds.current_capital expects to net in below.
	releasedMargin := futuresMarginRequired(roundCurrency(position.CostPrice*float64(quantity)*contractMultiplierValue(position.ContractMultiplier)), position.Leverage)
	realizedPnL := roundCurrency((orderPrice - position.CostPrice) * float64(quantity) * contractMultiplierValue(position.ContractMultiplier))
	if strings.EqualFold(strings.TrimSpace(position.PositionSide.String), "short") {
		realizedPnL = roundCurrency((position.CostPrice - orderPrice) * float64(quantity) * contractMultiplierValue(position.ContractMultiplier))
	}
	rolledStatus, err := e.tradeRepoCreateAndFill(ctx, fund, plan, action, executionSide, quantity, planPrice, amount, status, filledPrice, feeCommission, feeStampTax, feeTransfer, strategy, sql.NullFloat64{Float64: realizedPnL, Valid: true})
	if err != nil {
		return "rejected", err
	}
	remainingQty := position.Quantity - float64(quantity)
	if remainingQty <= 0.0001 {
		delete(positionsByKey, positionKey)
	} else {
		position.Quantity = remainingQty
		position.AvailableQty = remainingQty
		position.CurrentPrice = orderPrice
		applyPositionValuation(&position)
		positionsByKey[positionKey] = position
	}
	netCredit := releasedMargin + realizedPnL - feeCommission - feeTransfer - feeStampTax
	*availableCash = roundCurrency(*availableCash + netCredit)
	if rolledStatus != "" {
		return rolledStatus, nil
	}
	return status, nil
}

func executionOrderType(action repository.PlanAction) string {
	if action.Price.Valid && action.Price.Float64 > 0 {
		return "limit"
	}
	return "market"
}

func normalizedTradingMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "live", "paper", "simulation":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "simulation"
	}
}

func nullableFloat(value float64) sql.NullFloat64 {
	if value == 0 {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: value, Valid: true}
}

// mintTradeIdempotencyKey produces a deterministic client-side
// idempotency key for a trade submission. It is the bridge between
// the runtime engine's "I want to fill this plan_action" intent and
// the broker.PlaceOrderRequest.ClientOrderID contract: the value
// returned here flows into trade_executions.client_idempotency_key,
// which has a partial UNIQUE index (migration 027) so duplicate
// submissions for the same (action, side, qty) collapse to the
// existing row instead of double-booking.
//
// Format: "trade:<actionID>:<side>:<qty>". Empty actionID returns an
// invalid sql.NullString so legacy / synthetic call sites that lack a
// plan_action_id keep the legacy non-idempotent insert path.
func mintTradeIdempotencyKey(actionID, side string, quantity int) sql.NullString {
	if strings.TrimSpace(actionID) == "" {
		return sql.NullString{}
	}
	side = strings.ToLower(strings.TrimSpace(side))
	if side == "" {
		side = "buy"
	}
	return sql.NullString{
		String: fmt.Sprintf("trade:%s:%s:%d", actionID, side, quantity),
		Valid:  true,
	}
}

func nullUUID(value string) sql.NullString {
	return nullString(strings.TrimSpace(value))
}

func roundCurrency(value float64) float64 {
	return math.Round(value*10000) / 10000
}

func fallbackPositive(values ...float64) float64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func selectConsensus(roundtable *workflow.RoundtableResult, index int, fallback string) string {
	if roundtable != nil && index >= 0 && index < len(roundtable.Consensus) {
		if value := strings.TrimSpace(roundtable.Consensus[index]); value != "" {
			return value
		}
	}
	return fallback
}

func inferWorkflowSymbol(fund *repository.Fund, roundtable *workflow.RoundtableResult) string {
	_ = roundtable
	symbol, _ := inferWorkflowSymbolWithCandidates(fund, nil)
	return symbol
}

func inferWorkflowSymbolWithSpecialization(fund *repository.Fund, specialization *agentSpecialization) string {
	symbol, _ := inferWorkflowSymbolWithCandidates(fund, candidateWorkflowSymbolsFromSpecialization(specialization))
	return symbol
}

func inferWorkflowBuySymbol(fund *repository.Fund, candidates []string) (string, string) {
	if fund == nil {
		return "", ""
	}
	profile := decodeFundMarketProfile(fund.Config)
	if profile.Universe != nil {
		for _, symbol := range profile.Universe.Symbols {
			if trimmed := normalizedWorkflowSymbol(symbol); trimmed != "" {
				slog.Info("pm symbol inference", "source", "universe", "symbol", trimmed, "fundId", strings.TrimSpace(fund.ID))
				return trimmed, "universe"
			}
		}
	}
	if profile.Specialization != nil && profile.Specialization.Team != nil {
		for _, symbol := range normalizedStringSlice(profile.Specialization.Team.Instruments) {
			if trimmed := resolvedWorkflowSymbolCandidate(symbol); trimmed != "" {
				slog.Info("pm symbol inference", "source", "fund_specialization_instruments", "symbol", trimmed, "fundId", strings.TrimSpace(fund.ID))
				return trimmed, "fund_specialization_instruments"
			}
		}
	}
	for _, symbol := range candidates {
		if trimmed := normalizedWorkflowSymbol(symbol); trimmed != "" {
			slog.Info("pm symbol inference", "source", "team_specialization", "symbol", trimmed, "fundId", strings.TrimSpace(fund.ID))
			return trimmed, "team_specialization"
		}
	}
	return "", ""
}

func inferWorkflowSymbolWithCandidates(fund *repository.Fund, candidates []string) (string, string) {
	if fund != nil {
		if symbol, source := inferWorkflowBuySymbol(fund, candidates); symbol != "" {
			return symbol, source
		}
		profile := decodeFundMarketProfile(fund.Config)
		if profile.Specialization != nil && profile.Specialization.Team != nil {
			for _, symbol := range candidateWorkflowSymbolsFromSpecialization(&agentSpecialization{
				Themes: profile.Specialization.Team.Themes,
			}) {
				if trimmed := normalizedWorkflowSymbol(symbol); trimmed != "" {
					slog.Info("pm symbol inference", "source", "fund_specialization_theme", "symbol", trimmed, "fundId", strings.TrimSpace(fund.ID))
					return trimmed, "fund_specialization_theme"
				}
			}
		}
		if trimmed := strings.ToUpper(strings.TrimSpace(profile.BenchmarkSymbol)); trimmed != "" {
			slog.Info("pm symbol inference", "source", "benchmark", "symbol", trimmed, "fundId", strings.TrimSpace(fund.ID))
			return trimmed, "benchmark"
		}
		// F11.2: prefer a market-native default before falling back to fund
		// name parsing. fund.Name extraction is hand-wavy and frequently
		// produces noise tokens like "SMOKE", "TEST", "ALPHA" which then
		// flow through to market-data quote calls and pollute the cache.
		if marketDefault := defaultBenchmarkForMarketProfile(profile); marketDefault != "" {
			slog.Info("pm symbol inference", "source", "market_default", "symbol", marketDefault, "fundId", strings.TrimSpace(fund.ID))
			return marketDefault, "market_default"
		}
		if fallback := fallbackWorkflowSymbolFromFundName(fund.Name); fallback != "" {
			slog.Info("pm symbol inference", "source", "fund_name", "symbol", fallback, "fundId", strings.TrimSpace(fund.ID))
			return fallback, "fund_name"
		}
		slog.Info("pm symbol inference", "source", "default", "symbol", "SPY", "fundId", strings.TrimSpace(fund.ID))
		return "SPY", "default"
	}
	slog.Info("pm symbol inference", "source", "default", "symbol", "SPY")
	return "SPY", "default"
}

// defaultBenchmarkForMarketProfile returns a market-appropriate default
// benchmark when the fund has no benchmark_symbol set. Empty string means
// no market-specific default — caller should fall through to subsequent
// inference (fund name parsing or "SPY"). Centralised here so adding a new
// market only touches one switch.
func defaultBenchmarkForMarketProfile(profile fundMarketProfile) string {
	switch strings.ToLower(strings.TrimSpace(profile.Market)) {
	case "crypto":
		return "BTC-USD"
	case "a_share":
		return "000300.SS"
	case "us_equity":
		return "SPY"
	case "futures":
		return "ES=F"
	}
	switch strings.ToLower(strings.TrimSpace(profile.AssetClass)) {
	case "crypto":
		return "BTC-USD"
	case "futures":
		return "ES=F"
	}
	return ""
}

func candidateWorkflowSymbolsFromSpecialization(specialization *agentSpecialization) []string {
	if specialization == nil {
		return nil
	}
	candidates := make([]string, 0, len(specialization.Instruments)+len(specialization.Themes))
	seen := make(map[string]struct{}, len(specialization.Instruments)+len(specialization.Themes))
	appendCandidate := func(values []string) {
		for _, value := range values {
			for _, part := range splitNormalizedValues(value) {
				trimmed := resolvedWorkflowSymbolCandidate(part)
				if trimmed == "" {
					continue
				}
				if _, ok := seen[trimmed]; ok {
					continue
				}
				seen[trimmed] = struct{}{}
				candidates = append(candidates, trimmed)
			}
		}
	}
	appendCandidate(specialization.Instruments)
	appendCandidate(specialization.Themes)
	return candidates
}

func candidateWorkflowSymbolsFromTeamAgents(agents ...*repository.Agent) []string {
	candidates := make([]string, 0, len(agents)*2)
	seen := make(map[string]struct{}, len(agents)*2)
	for _, agent := range agents {
		for _, candidate := range candidateWorkflowSymbolsFromSpecialization(extractAgentSpecialization(agent)) {
			if _, ok := seen[candidate]; ok {
				continue
			}
			seen[candidate] = struct{}{}
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

func resolvedWorkflowSymbolCandidate(value string) string {
	if trimmed := normalizedWorkflowSymbol(value); trimmed != "" {
		return trimmed
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "micron", "micron technology", "美光", "美光科技":
		return "MU"
	case "sandisk", "闪迪":
		return "SNDK"
	default:
		return ""
	}
}

func normalizedWorkflowSymbol(value string) string {
	trimmed := strings.ToUpper(strings.TrimSpace(value))
	if trimmed == "" {
		return ""
	}
	for _, r := range trimmed {
		if r > unicode.MaxASCII {
			return ""
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.' && r != '-' {
			return ""
		}
	}
	if len(trimmed) < 1 || len(trimmed) > 16 {
		return ""
	}
	if strings.ContainsFunc(trimmed, func(r rune) bool { return unicode.IsLetter(r) }) {
		return trimmed
	}
	// Allow pure-numeric tickers used by A-share (6-digit, e.g. 688205,
	// 600519, 000858) and HK Stock (4-5 zero-padded digits, e.g. 0700,
	// 00700). The length bound rejects noise such as year/quantity tokens
	// (e.g. "2024", "100") that may slip through from theme metadata
	// when more specific candidates are absent.
	for _, r := range trimmed {
		if !unicode.IsDigit(r) {
			return ""
		}
	}
	if len(trimmed) >= 4 && len(trimmed) <= 6 {
		return trimmed
	}
	return ""
}

func isStaleMarketNewsItem(item marketdata.NewsItem, now time.Time, maxAge time.Duration) bool {
	if maxAge <= 0 {
		return false
	}
	if item.PublishedAt.IsZero() {
		return false
	}
	return now.UTC().Sub(item.PublishedAt.UTC()) > maxAge
}

// fallbackNonTickerWords is the deny-list applied to fund-name tokens before
// they are treated as tickers. These are common adjectives/nouns that
// appear in fund names but are never actual instruments. Without this
// guard, "Smoke Test Crypto Fund" extracts "SMOKE" and the macro brief
// then quotes a non-existent symbol.
var fallbackNonTickerWords = map[string]struct{}{
	"FUND": {}, "TEST": {}, "SMOKE": {}, "DEMO": {}, "PILOT": {}, "TRIAL": {},
	"ALPHA": {}, "BETA": {}, "GAMMA": {}, "DELTA": {}, "OMEGA": {},
	"QUANT": {}, "MACRO": {}, "MICRO": {}, "GLOBAL": {}, "ASIA": {},
	"CRYPTO": {}, "EQUITY": {}, "FUTURES": {}, "BOND": {}, "BONDS": {},
	"GROWTH": {}, "VALUE": {}, "INCOME": {}, "YIELD": {}, "TOTAL": {},
	"FOCUS": {}, "CORE": {}, "EDGE": {}, "PRIME": {}, "LITE": {}, "PRO": {},
	"FUNDAI": {}, "PARTNERS": {}, "CAPITAL": {}, "ASSET": {}, "ASSETS": {},
	"MGMT": {}, "MANAGEMENT": {}, "STRATEGY": {}, "STRATEGIES": {},
	"AND": {}, "THE": {}, "FOR": {}, "WITH": {}, "FROM": {},
	"USD": {}, "USDT": {}, "BUY": {}, "SELL": {}, "LONG": {}, "SHORT": {},
}

func fallbackWorkflowSymbolFromFundName(name string) string {
	trimmed := strings.ToUpper(strings.TrimSpace(name))
	if trimmed == "" {
		return ""
	}
	tokens := strings.FieldsFunc(trimmed, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, token := range tokens {
		if len(token) < 2 || len(token) > 8 {
			continue
		}
		if _, blocked := fallbackNonTickerWords[token]; blocked {
			continue
		}
		hasLetter := false
		asciiOnly := true
		for _, r := range token {
			if r > unicode.MaxASCII {
				asciiOnly = false
				break
			}
			if unicode.IsLetter(r) {
				hasLetter = true
			}
		}
		if asciiOnly && hasLetter {
			return token
		}
	}
	return ""
}

func filterWorkflowPlanPositions(positions []repository.HoldingPosition) []repository.HoldingPosition {
	if len(positions) == 0 {
		return positions
	}
	filtered := make([]repository.HoldingPosition, 0, len(positions))
	for _, position := range positions {
		if isLegacyWorkflowPlaceholderPosition(position) {
			slog.Info("skip legacy workflow placeholder position", "symbol", strings.TrimSpace(position.Symbol), "instrumentKey", strings.TrimSpace(position.InstrumentKey))
			continue
		}
		filtered = append(filtered, position)
	}
	return filtered
}

func isLegacyWorkflowPlaceholderPosition(position repository.HoldingPosition) bool {
	symbol := strings.ToUpper(strings.TrimSpace(position.Symbol))
	instrumentKey := strings.ToUpper(strings.TrimSpace(position.InstrumentKey))
	switch symbol {
	case "MACRO", "BENCHMARK", "NEWS", "NOTES":
		return true
	}
	switch instrumentKey {
	case "MACRO", "BENCHMARK", "NEWS", "NOTES", "NASDAQ:MACRO", "NASDAQ:BENCHMARK", "NASDAQ:NEWS", "NASDAQ:NOTES":
		return true
	}
	return false
}

func benchmarkInstrumentRef(profile fundMarketProfile) (marketdata.InstrumentRef, bool) {
	symbol := strings.TrimSpace(profile.BenchmarkSymbol)
	if symbol == "" {
		return marketdata.InstrumentRef{}, false
	}
	instrument := marketdata.InstrumentRef{
		InstrumentKey: buildInstrumentKey(profile.Exchange, symbol),
		Symbol:        symbol,
		Market:        profile.Market,
		Exchange:      profile.Exchange,
		AssetClass:    profile.AssetClass,
	}
	return instrument, true
}

func benchmarkPointer(instrument marketdata.InstrumentRef, ok ...bool) *marketdata.InstrumentRef {
	if len(ok) > 0 && !ok[0] {
		return nil
	}
	if strings.TrimSpace(instrument.Symbol) == "" {
		return nil
	}
	copy := instrument
	return &copy
}

func profileUniverseInstruments(profile fundMarketProfile) []marketdata.InstrumentRef {
	if profile.Universe == nil || len(profile.Universe.Symbols) == 0 {
		return nil
	}
	instruments := make([]marketdata.InstrumentRef, 0, len(profile.Universe.Symbols))
	for _, symbol := range profile.Universe.Symbols {
		trimmed := strings.TrimSpace(symbol)
		if trimmed == "" {
			continue
		}
		instruments = append(instruments, marketdata.InstrumentRef{
			InstrumentKey: buildInstrumentKey(profile.Exchange, trimmed),
			Symbol:        trimmed,
			Market:        profile.Market,
			Exchange:      profile.Exchange,
			AssetClass:    profile.AssetClass,
		})
	}
	return instruments
}

func defaultInstrumentRef(fund *repository.Fund, focus workflow.ResearchFocus, symbol string) marketdata.InstrumentRef {
	profile := fundMarketProfile{}
	if fund != nil {
		profile = decodeFundMarketProfile(fund.Config)
	}
	market := firstNonEmptyValue(profile.Market, focusToMarket(string(focus)))
	assetClass := firstNonEmptyValue(profile.AssetClass, focusToAssetClass(string(focus)))
	return marketdata.InstrumentRef{
		InstrumentKey: buildInstrumentKey(profile.Exchange, symbol),
		Symbol:        symbol,
		Market:        market,
		Exchange:      profile.Exchange,
		AssetClass:    assetClass,
		QuoteCurrency: profile.BaseCurrency,
	}
}

func planActionInstrumentRef(fund *repository.Fund, symbol, instrumentKey, market, exchange, assetClass, instrumentType, quoteCurrency, settlementCurrency string, contractMultiplier float64, expiryDate string) marketdata.InstrumentRef {
	instrument := defaultInstrumentRef(fund, workflow.FocusStock, symbol)
	instrument.InstrumentKey = firstNonEmptyValue(instrumentKey, instrument.InstrumentKey)
	instrument.Market = firstNonEmptyValue(market, instrument.Market)
	instrument.Exchange = firstNonEmptyValue(exchange, instrument.Exchange)
	instrument.AssetClass = firstNonEmptyValue(assetClass, instrument.AssetClass)
	instrument.InstrumentType = strings.TrimSpace(instrumentType)
	instrument.QuoteCurrency = firstNonEmptyValue(quoteCurrency, instrument.QuoteCurrency)
	instrument.SettlementCurrency = strings.TrimSpace(settlementCurrency)
	instrument.ContractMultiplier = contractMultiplier
	instrument.ExpiryDate = strings.TrimSpace(expiryDate)
	return instrument
}

func formatResearchContextBlock(lang UserLanguage, label string, research *marketdata.ResearchContext) string {
	zh := lang != UserLanguageEN
	if research == nil {
		if zh {
			return strings.TrimSpace(label) + "：研究数据暂不可用"
		}
		return strings.TrimSpace(label) + ": research data unavailable"
	}
	summaryFallback := "市场研究"
	colon := "："
	signalLabel := "技术信号"
	signalSep := "；"
	newsLabel := "新闻"
	quoteFmt := "报价 %.4f %s（来源：%s，时间：%s）"
	if !zh {
		summaryFallback = "Market research"
		colon = ": "
		signalLabel = "Signals"
		signalSep = "; "
		newsLabel = "News"
		quoteFmt = "Quote %.4f %s (source: %s, asOf: %s)"
	}
	lines := []string{strings.TrimSpace(label) + colon + firstNonEmptyValue(research.Summary, summaryFallback)}
	if research.Quote != nil {
		lines = append(lines, fmt.Sprintf(quoteFmt, research.Quote.Price, strings.TrimSpace(research.Quote.QuoteCurrency), research.Quote.Source, research.Quote.AsOf.UTC().Format(time.RFC3339)))
	}
	if len(research.Signals) > 0 {
		lines = append(lines, signalLabel+colon+strings.Join(research.Signals, signalSep))
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	added := 0
	for _, item := range research.News {
		if added >= 3 {
			break
		}
		if item.PublishedAt.IsZero() || item.PublishedAt.Before(cutoff) {
			continue
		}
		lines = append(lines, newsLabel+colon+item.Title)
		added++
	}
	return strings.Join(lines, "\n")
}

func marketDataAsOfNote(lang UserLanguage, tradingDate string) string {
	if strings.TrimSpace(tradingDate) == "" {
		return ""
	}
	formatted := parseTradingDateOrNow(tradingDate).Format("2006-01-02")
	if lang == UserLanguageEN {
		return "Market snapshot trading date: " + formatted
	}
	return "行情快照交易日：" + formatted
}

func appendQuoteReference(lang UserLanguage, reasoning string, quote *marketdata.QuoteSnapshot) string {
	if quote == nil || quote.Price <= 0 {
		return reasoning
	}
	var reference string
	if lang == UserLanguageEN {
		reference = fmt.Sprintf("Reference quote %.4f (source: %s, asOf: %s)", quote.Price, quote.Source, quote.AsOf.UTC().Format(time.RFC3339))
	} else {
		reference = fmt.Sprintf("参考报价 %.4f（来源：%s，时间：%s）", quote.Price, quote.Source, quote.AsOf.UTC().Format(time.RFC3339))
	}
	return appendSkillContext(reasoning, reference)
}

func formatNullTime(value sql.NullTime) string {
	if !value.Valid || value.Time.IsZero() {
		return ""
	}
	return value.Time.UTC().Format("2006-01-02")
}

func (m *runtimeMemorySystem) ConsolidateDaily(ctx context.Context, fundID string, state workflow.WorkflowState) error {
	tradingDate := parseTradingDateOrNow(state.TradingDate)
	learningCtx, err := m.buildLearningContext(ctx, fundID, tradingDate)
	if err != nil {
		return mapRepositoryError(err)
	}

	// Per-trading-day dedupe. The workflow scheduler ticks intraday
	// funds (e.g. OCS-style 30-min cadence) 7-8 times per session;
	// StepDailyReview runs at the END of every tick. Without the
	// guards below we wrote a near-identical "self_learning" row per
	// tick per agent, polluting the agent learning UI and burning
	// LLM tokens on the (Step D) lesson generator every half-hour.
	// We skip the WRITE but still fall through to maybeRunReflection
	// and runDailyAttribution because those are cheap-or-cached and
	// happen to be the steps that *want* to re-run as new ticks
	// arrive (more memories => better long-term reflection input).
	if m.memoryRepo != nil {
		exists, err := m.memoryRepo.ExistsByFundAgentLayerDate(ctx, fundID, sql.NullString{}, "daily", tradingDate)
		if err != nil {
			slog.Warn("daily review: fund-level dedupe check failed; falling through", "fund_id", fundID, "trading_date", tradingDate.Format("2006-01-02"), "err", err)
		} else if !exists {
			fundLearning := m.buildFundLearning(state, learningCtx)
			if writeErr := m.writeLearningMemory(ctx, fundID, sql.NullString{}, "daily", tradingDate, 0, fundLearning); writeErr != nil {
				return mapRepositoryError(writeErr)
			}
		}
	} else {
		fundLearning := m.buildFundLearning(state, learningCtx)
		if err := m.writeLearningMemory(ctx, fundID, sql.NullString{}, "daily", tradingDate, 0, fundLearning); err != nil {
			return mapRepositoryError(err)
		}
	}

	if m.teamRepo == nil || m.agentRepo == nil {
		return nil
	}
	members, err := m.teamRepo.ListByFund(ctx, fundID)
	if err != nil {
		return mapRepositoryError(err)
	}
	for i := range members {
		member := members[i]
		agent, err := m.agentRepo.GetByID(ctx, member.AgentID)
		if err != nil {
			return mapRepositoryError(err)
		}
		config, enabled, autoApply, maxLessons := parseEvolutionLearningConfig(agent.EvolutionConfig)
		if !enabled {
			continue
		}
		if !learningScopeAllowsFund(learningScopeFromConfig(config), fundID) {
			continue
		}
		// Sprint 3 / M7: skip the whole learning stack for an agent
		// whose lastLearningDate already equals today AND zero
		// trades happened today fund-wide. This is a strict no-op:
		// the fact that learning previously ran today means lessons
		// are already on disk, and a zero-trade tick adds no new
		// signal. We check fundDayTradeCount (not the plan-scoped
		// learningCtx.trades) because intraday cadence funds emit a
		// "watch only" plan at end-of-day whose plan-scoped trades
		// list is empty even though earlier plans the same day
		// produced real fills — those still count as new signal.
		// The check sits outside the dedupe path because we want to
		// skip BOTH the LLM call and the DB write — the existence
		// dedupe below would only skip the write.
		if existingLearningCoversToday(config, tradingDate) && learningCtx.fundDayTradeCount == 0 {
			continue
		}
		// Per-agent dedupe — see fund-level rationale above. Doing
		// the existence check BEFORE buildAgentLearning matters
		// because that call now (Step D) invokes the LLM lesson
		// generator, which we want to charge for at most once per
		// agent per trading day. Partial-failure recovery: if the
		// previous tick's loop crashed after fund row but before
		// some agent rows, those un-written agents still flow
		// through this iteration normally.
		if m.memoryRepo != nil {
			exists, existsErr := m.memoryRepo.ExistsByFundAgentLayerDate(ctx, fundID, nullUUID(agent.ID), "agent", tradingDate)
			if existsErr == nil && exists {
				continue
			}
			if existsErr != nil {
				slog.Warn("daily review: per-agent dedupe check failed; proceeding with write", "fund_id", fundID, "agent_id", agent.ID, "trading_date", tradingDate.Format("2006-01-02"), "err", existsErr)
			}
		}
		learning := m.buildAgentLearning(ctx, member, agent, state, learningCtx, maxLessons)
		dailyReturn := 0.0
		if learningCtx.nav != nil {
			dailyReturn = learningCtx.nav.DailyReturn
		}
		if err := m.writeLearningMemory(ctx, fundID, nullUUID(agent.ID), "agent", tradingDate, dailyReturn, learning); err != nil {
			return mapRepositoryError(err)
		}
		if !autoApply {
			continue
		}
		updatedConfig, err := applyLearningToEvolutionConfig(config, learning, tradingDate, dailyReturn)
		if err != nil {
			return err
		}
		agent.EvolutionConfig = updatedConfig
		if err := m.agentRepo.Update(ctx, agent); err != nil {
			return mapRepositoryError(err)
		}
	}

	// After the per-agent learning fan-out we attempt a long-term
	// reflection pass. This is rate-limited to once per cadence window so
	// the LLM bill stays bounded; see maybeRunReflection for the policy.
	// Failures are logged inside the helper and never bubble up — daily
	// review must succeed even if reflection cannot.
	m.maybeRunReflection(ctx, fundID, learningCtx.fund, tradingDate)

	// Phase 3A-5: deterministic strategy attribution pass. Cheap
	// (one COUNT(*) per dimension), idempotent against re-runs,
	// and entirely soft-fail: any failure surfaces as a slog
	// warning and the workflow continues to mark itself complete.
	runDailyAttribution(m.attribution, fundID)

	return nil
}

func (m *runtimeMemorySystem) buildLearningContext(ctx context.Context, fundID string, tradingDate time.Time) (*learningContext, error) {
	result := &learningContext{tradingDate: tradingDate}
	if m.fundRepo != nil {
		fund, err := m.fundRepo.GetByID(ctx, fundID)
		if err != nil && !errors.Is(err, repository.ErrNotFound) {
			return nil, err
		}
		result.fund = fund
	}
	if m.workflowRepo != nil {
		run, err := m.workflowRepo.GetByFundAndDate(ctx, fundID, tradingDate)
		if err != nil && !errors.Is(err, repository.ErrNotFound) {
			return nil, err
		}
		result.workflowRun = run
	}
	if m.navRepo != nil {
		nav, err := m.navRepo.GetByFundAndDate(ctx, fundID, tradingDate)
		if err != nil && !errors.Is(err, repository.ErrNotFound) {
			return nil, err
		}
		result.nav = nav
		result.positions = decodeHoldingPositionsSnapshot(nav)
	}
	if m.planRepo != nil {
		plans, err := m.planRepo.ListByFund(ctx, fundID, 20)
		if err != nil {
			return nil, err
		}
		result.plan = selectLearningPlan(plans, tradingDate)
		if result.plan != nil {
			actions, err := m.planRepo.GetActions(ctx, result.plan.ID)
			if err != nil {
				return nil, err
			}
			result.actions = actions
		}
	}
	if m.tradeRepo != nil {
		var (
			trades []repository.TradeExecution
			err    error
		)
		if result.plan != nil {
			trades, err = m.tradeRepo.ListByPlan(ctx, result.plan.ID)
		} else {
			start := tradingDate
			end := tradingDate.Add(24*time.Hour - time.Nanosecond)
			trades, err = m.tradeRepo.ListByFund(ctx, fundID, start, end, 200)
		}
		if err != nil {
			return nil, err
		}
		result.trades = trades
		// Fund-wide day trade count for the LLM-lesson gate. Skip the
		// second query when the plan-scoped result already covers the
		// whole day (i.e. plan==nil branch above) — those are the
		// same rows.
		if result.plan != nil {
			start := tradingDate
			end := tradingDate.Add(24*time.Hour - time.Nanosecond)
			dayTrades, listErr := m.tradeRepo.ListByFund(ctx, fundID, start, end, 200)
			if listErr != nil {
				slog.Warn("learning context: fund-wide day trade count failed; falling back to plan scope", "fund_id", fundID, "err", listErr)
				result.fundDayTradeCount = len(trades)
			} else {
				result.fundDayTradeCount = len(dayTrades)
			}
		} else {
			result.fundDayTradeCount = len(trades)
		}
	}
	if len(result.positions) == 0 && m.positionRepo != nil {
		positions, err := m.positionRepo.ListByFund(ctx, fundID)
		if err != nil {
			return nil, err
		}
		result.positions = positions
	}
	return result, nil
}

func selectLearningPlan(plans []repository.InvestmentPlan, tradingDate time.Time) *repository.InvestmentPlan {
	for i := range plans {
		if sameTradingDate(plans[i].TradingDate, tradingDate) {
			plan := plans[i]
			return &plan
		}
	}
	return nil
}

func decodeHoldingPositionsSnapshot(nav *repository.NavSnapshot) []repository.HoldingPosition {
	if nav == nil || len(nav.PositionsSnapshot) == 0 {
		return nil
	}
	var positions []repository.HoldingPosition
	if err := json.Unmarshal(nav.PositionsSnapshot, &positions); err != nil {
		return nil
	}
	return positions
}

func sameTradingDate(left, right time.Time) bool {
	return left.UTC().Format("2006-01-02") == right.UTC().Format("2006-01-02")
}

func (m *runtimeMemorySystem) buildFundLearning(state workflow.WorkflowState, learningCtx *learningContext) learningResult {
	dailyReturn := 0.0
	if learningCtx != nil && learningCtx.nav != nil {
		dailyReturn = learningCtx.nav.DailyReturn
	}
	tradeStats := summarizeTrades(nil, learningCtx.trades)
	hits := []string{}
	misses := []string{}
	lessons := []string{}
	adjustments := []string{}
	if learningCtx.plan != nil {
		hits = append(hits, fmt.Sprintf("生成了当日计划，状态为 %s。", learningCtx.plan.Status))
	} else {
		misses = append(misses, "未找到当日投资计划，导致团队缺少统一执行锚点。")
	}
	if learningCtx.nav != nil {
		hits = append(hits, fmt.Sprintf("完成了净值结算，日收益率 %.2f%%。", dailyReturn*100))
	} else {
		misses = append(misses, "缺少当日净值快照，无法沉淀收益驱动的复盘。")
	}
	if tradeStats.total > 0 {
		hits = append(hits, fmt.Sprintf("记录了 %d 笔执行，其中 %d 笔完全成交。", tradeStats.total, tradeStats.filled))
	}
	if tradeStats.partial > 0 || tradeStats.rejected > 0 {
		misses = append(misses, fmt.Sprintf("执行阶段存在 %d 笔部分成交、%d 笔拒单。", tradeStats.partial, tradeStats.rejected))
	}
	if dailyReturn > 0 {
		lessons = append(lessons, "正收益日应保留当日有效决策路径，避免次日完全重置执行偏好。")
		adjustments = append(adjustments, "继续强化当日贡献正收益的研究主题与执行节奏。")
	} else if dailyReturn < 0 {
		lessons = append(lessons, "负收益日需要把损失拆解到计划质量、执行质量与风控质量三个层面。")
		adjustments = append(adjustments, "次日优先降低仓位激进度并收紧执行容忍区间。")
	} else {
		lessons = append(lessons, "零收益或数据缺失时，应优先确认是否存在观测盲区。")
	}
	if len(learningCtx.actions) == 0 {
		adjustments = append(adjustments, "补充更具体的计划动作与支持理由，减少口头共识无法执行的问题。")
	}
	lessons = limitStrings(uniqueNonEmpty(lessons), 4)
	adjustments = limitStrings(uniqueNonEmpty(adjustments), 4)
	specialization := buildSpecializationLearningSummary("team", learningCtx, lessons, adjustments)
	tags := []string{"workflow", string(state.Status), "self_learning", "team"}
	if specialization != nil {
		tags = append(tags, "specialization")
	}
	summary := fmt.Sprintf("%s 团队日复盘已完成：工作流状态 %s，当前步骤 %s，日收益率 %.2f%%。", state.TradingDate, state.Status, state.CurrentStep.String(), dailyReturn*100)
	return learningResult{
		Title:          fmt.Sprintf("团队日复盘 %s", state.TradingDate),
		Summary:        summary,
		Hits:           hits,
		Misses:         misses,
		Lessons:        lessons,
		Adjustments:    adjustments,
		Tags:           tags,
		Specialization: specialization,
	}
}

func (m *runtimeMemorySystem) buildAgentLearning(ctx context.Context, member repository.TeamMember, agent *repository.Agent, state workflow.WorkflowState, learningCtx *learningContext, maxLessons int) learningResult {
	tradeStats := summarizeTrades(learningCtx.actions, learningCtx.trades)
	dailyReturn := 0.0
	if learningCtx != nil && learningCtx.nav != nil {
		dailyReturn = learningCtx.nav.DailyReturn
	}
	maxWeight, maxWeightSymbol := largestPositionWeight(learningCtx.positions)
	hits := []string{}
	misses := []string{}
	lessons := []string{}
	adjustments := []string{}
	role := strings.ToLower(strings.TrimSpace(member.Role))
	focus := strings.TrimSpace(member.Focus.String)
	_ = agent // reserved for future per-agent prompt customisation

	switch role {
	case "pm":
		if learningCtx.plan != nil {
			hits = append(hits, fmt.Sprintf("形成了 %d 条计划动作，计划状态为 %s。", len(learningCtx.actions), learningCtx.plan.Status))
		}
		if dailyReturn > 0 {
			hits = append(hits, fmt.Sprintf("组合当日取得 %.2f%% 正收益。", dailyReturn*100))
		} else if dailyReturn < 0 {
			misses = append(misses, fmt.Sprintf("组合当日收益为 %.2f%%，需要重新评估计划质量。", dailyReturn*100))
		}
		if tradeStats.rejected > 0 {
			misses = append(misses, fmt.Sprintf("有 %d 笔计划执行被拒，说明计划与执行约束未充分对齐。", tradeStats.rejected))
		}
		lessons = append(lessons,
			"组合经理应把收益结果与计划动作逐一映射，保留高命中决策模板。",
			"若执行反馈频繁偏离计划，应收窄次日计划中的仓位与价格假设。",
		)
		adjustments = append(adjustments,
			"优先输出更少但置信度更高的动作清单。",
			"为高不确定性标的增加明确的放弃条件。",
		)
	case "trader":
		if tradeStats.total > 0 {
			hits = append(hits, fmt.Sprintf("共处理 %d 笔执行，完全成交 %d 笔，成交率 %.0f%%。", tradeStats.total, tradeStats.filled, tradeStats.fillRatio*100))
		}
		if tradeStats.partial > 0 {
			misses = append(misses, fmt.Sprintf("存在 %d 笔部分成交，说明挂单时机或单笔数量仍需优化。", tradeStats.partial))
		}
		if tradeStats.rejected > 0 {
			misses = append(misses, fmt.Sprintf("存在 %d 笔拒单，需要复查价格边界与下单参数。", tradeStats.rejected))
		}
		if dailyReturn > 0 && tradeStats.filled > 0 {
			hits = append(hits, "执行结果与收益方向一致，说明下单节奏基本有效。")
		}
		// Splitter-aware: if at least one parent was sliced
		// today, surface the average slice count so the LLM
		// can speak to slice sizing rather than only treating
		// the parent as a black-box "1 trade". The threshold
		// ">= 1 parent" intentionally fires even on a single
		// TWAP because that's the canonical "did the strategy
		// engine actually engage?" signal.
		if tradeStats.twapParentCount > 0 {
			avgSlices := float64(tradeStats.twapSliceCount) / float64(tradeStats.twapParentCount)
			hits = append(hits, fmt.Sprintf("%d 个父订单走了 TWAP/VWAP 等多笔策略，共拆出 %d 个子分笔，平均 %.1f 笔/父单。",
				tradeStats.twapParentCount, tradeStats.twapSliceCount, avgSlices))
			lessons = append(lessons,
				"对走多笔策略的父订单，应回看每个子分笔的成交价散布，发现尾盘集中拖累整体均价的模式。",
			)
		}
		lessons = append(lessons,
			"交易员应优先复用高成交率的下单节奏，并减少对低流动性窗口的暴露。",
			"当部分成交增加时，应缩小单笔规模并提高价格容忍度的一致性。",
		)
		adjustments = append(adjustments,
			"次日优先拆分大单并减少临近收盘的被动追价。",
			"对重复拒单的参数组合设置更严格的自检。",
		)
	case "risk":
		if maxWeight > 0 {
			hits = append(hits, fmt.Sprintf("当前最大仓位为 %s %.2f%%，可用于检查集中度。", maxWeightSymbol, maxWeight*100))
		}
		if dailyReturn < 0 {
			misses = append(misses, fmt.Sprintf("组合当日回撤 %.2f%%，风险约束可能偏松。", -dailyReturn*100))
		}
		if tradeStats.rejected > 0 {
			hits = append(hits, fmt.Sprintf("通过 %d 笔拒单暴露了执行边界问题。", tradeStats.rejected))
		}
		if maxWeight > 0.35 {
			misses = append(misses, fmt.Sprintf("单一持仓 %s 权重达到 %.2f%%，需要进一步压降集中度。", maxWeightSymbol, maxWeight*100))
		}
		lessons = append(lessons,
			"风控应把收益波动与仓位集中度、执行异常一起纳入次日阈值调整。",
			"负收益日优先收紧仓位上限与异常订单的放行条件。",
		)
		adjustments = append(adjustments,
			"下调单标的风险预算并提高异常订单复核频率。",
			"将集中度与执行异常联动为同一组风控触发条件。",
		)
	case "researcher":
		if focus != "" {
			hits = append(hits, fmt.Sprintf("研究方向聚焦在 %s。", focus))
		}
		if learningCtx.plan != nil && learningCtx.plan.Reasoning.Valid {
			hits = append(hits, "研究输出已进入计划推演，可继续追踪其对收益的解释力。")
		} else {
			misses = append(misses, "未观察到研究结论稳定映射到当日计划。")
		}
		if dailyReturn < 0 {
			misses = append(misses, fmt.Sprintf("研究结论支撑下的组合当日收益 %.2f%%，需要复查研究假设。", dailyReturn*100))
		}
		lessons = append(lessons,
			"研究员应持续记录哪些主题真正进入计划与执行，而不是只输出观点。",
			"当研究主题未转化为收益时，需要更明确地标记失效假设。",
		)
		adjustments = append(adjustments,
			"次日减少宽泛结论，优先输出可执行且可验证的研究命题。",
			"为重点结论补充失效条件与时间窗口。",
		)
	default:
		lessons = append(lessons, "团队成员需要把当日收益、执行与协作结果转成可复用规则。")
		adjustments = append(adjustments, "次日优先保留有效做法并剔除重复失误。")
	}

	// Step D: replace the role-templated lessons/adjustments with an
	// LLM-generated pair grounded in the actual day. Templates are kept
	// as the deterministic fallback so model errors never wedge the
	// daily review; the gate function prevents us from paying for an
	// LLM call on truly-quiet days (no fills, no rejects, flat NAV)
	// where the templates and the LLM would say roughly the same
	// thing anyway.
	if m.shouldGenerateLLMLessons(learningCtx, tradeStats, dailyReturn) {
		// Resolve the team member's structured specialization (migration
		// 087). When set, this becomes the canonical "what does this
		// researcher cover?" signal — the legacy focus-string regex is
		// only consulted as a fallback inside the prompt builder.
		// Failure here is non-fatal: a DB hiccup shouldn't suppress the
		// LLM call, we just lose the per-researcher isolation for this
		// run and the builder falls back to focus parsing.
		var coverage []string
		if m.teamRepo != nil {
			if spec, specErr := m.teamRepo.GetSpecialization(ctx, member.ID); specErr == nil && spec != nil {
				coverage = spec.Instruments
			} else if specErr != nil {
				slog.Warn("daily review: specialization lookup failed; falling back to focus string", "member_id", member.ID, "err", specErr)
			}
		}
		if llmLessons, llmAdjustments, err := m.generateAgentLessonsLLM(ctx, role, focus, coverage, learningCtx, tradeStats, dailyReturn, maxLessons); err == nil {
			if len(llmLessons) > 0 {
				lessons = llmLessons
			}
			if len(llmAdjustments) > 0 {
				adjustments = llmAdjustments
			}
		} else {
			fundIDForLog := ""
			if learningCtx != nil && learningCtx.fund != nil {
				fundIDForLog = learningCtx.fund.ID
			}
			slog.Warn("daily review: llm lesson generation failed; keeping role templates", "fund_id", fundIDForLog, "role", role, "err", err)
		}
	}

	if len(learningCtx.actions) == 0 {
		misses = append(misses, "缺少可执行动作，导致学习样本不足。")
	}
	if learningCtx.nav == nil {
		misses = append(misses, "缺少收益快照，学习结论只能基于流程结果而非真实收益。")
	}
	lessons = limitStrings(uniqueNonEmpty(lessons), normalizedMaxLessons(maxLessons))
	adjustments = limitStrings(uniqueNonEmpty(adjustments), normalizedMaxLessons(maxLessons))
	hits = limitStrings(uniqueNonEmpty(hits), 4)
	misses = limitStrings(uniqueNonEmpty(misses), 4)
	specialization := buildSpecializationLearningSummary(role, learningCtx, lessons, adjustments)
	tags := []string{"self_learning", role, string(state.Status)}
	if specialization != nil {
		tags = append(tags, "specialization")
	}
	name := role
	if agent != nil && strings.TrimSpace(agent.Name) != "" {
		name = strings.TrimSpace(agent.Name)
	}
	summary := fmt.Sprintf("%s 在 %s 完成自主学习复盘：日收益率 %.2f%%，记录 %d 条命中、%d 条问题，并沉淀 %d 条学习结论。", name, state.TradingDate, dailyReturn*100, len(hits), len(misses), len(lessons))
	return learningResult{
		Title:          fmt.Sprintf("自主复盘 %s %s", role, state.TradingDate),
		Summary:        summary,
		Hits:           hits,
		Misses:         misses,
		Lessons:        lessons,
		Adjustments:    adjustments,
		Tags:           tags,
		Specialization: specialization,
	}
}

// shouldGenerateLLMLessons gates whether to spend an LLM call on
// realistic lessons/adjustments for this agent today. The deterministic
// templates are good enough when NOTHING happened (no fills, no
// rejects, flat NAV, no actions) — in that case the LLM would
// produce another version of "today was uneventful, keep monitoring"
// and waste tokens.
//
// We say "yes, call the LLM" if any of these are true:
//   - daily return is non-zero (real PnL signal)
//   - any trade was filled, partial, or rejected (execution signal)
//   - the plan emitted at least one buy/sell/reduce/add action that
//     the prompt can dissect (planning signal)
//
// nil runtime always short-circuits to false (covers test fixtures
// that don't wire an LLM).
func (m *runtimeMemorySystem) shouldGenerateLLMLessons(learningCtx *learningContext, tradeStats tradeSummary, dailyReturn float64) bool {
	if m == nil || m.llmRuntime == nil || learningCtx == nil {
		return false
	}
	if math.Abs(dailyReturn) > 1e-9 {
		return true
	}
	if tradeStats.filled > 0 || tradeStats.partial > 0 || tradeStats.rejected > 0 {
		return true
	}
	// Fund-wide day trade count covers the intraday-cadence case
	// where the selected plan is a watch-only late-day tick but
	// earlier ticks of the SAME day produced fills — the day still
	// has something worth reflecting on.
	if learningCtx.fundDayTradeCount > 0 {
		return true
	}
	for _, a := range learningCtx.actions {
		switch strings.ToLower(strings.TrimSpace(a.Action)) {
		case "buy", "sell", "add", "reduce":
			return true
		}
	}
	return false
}

// generateAgentLessonsLLM asks the standard-tier model to write today's
// lessons and tomorrow's adjustments for a specific agent role, in
// Chinese, grounded in concrete numbers from learningCtx. Returns
// (lessons, adjustments, nil) on success.
//
// Cost shape: with the per-trading-day dedupe in ConsolidateDaily this
// runs at most once per (fund, agent, day), so on the OCS Selection
// fund (4 agents, 1 trading day) it's 4 standard-tier requests per
// day — bounded and cheap.
//
// We deliberately request a tight JSON schema rather than free text
// because the caller treats the output as `[]string`; a malformed
// response (or no JSON at all) returns an error and the caller falls
// back to the role templates.
func (m *runtimeMemorySystem) generateAgentLessonsLLM(ctx context.Context, role, focus string, coverage []string, learningCtx *learningContext, tradeStats tradeSummary, dailyReturn float64, maxLessons int) ([]string, []string, error) {
	if m == nil || m.llmRuntime == nil {
		return nil, nil, errors.New("memory: llm runtime not configured")
	}
	if learningCtx == nil || learningCtx.fund == nil {
		return nil, nil, errors.New("memory: learning context missing fund")
	}

	cap := normalizedMaxLessons(maxLessons)
	if cap < 2 {
		cap = 2
	}

	// Role-specific learning body. The four roles in a fund team
	// observe DIFFERENT facts about the same day — a PM thinks about
	// allocation + plan quality, a Researcher about how their thesis
	// translated into actions for their focus area, a Trader about
	// execution micro-structure, a Risk overseer about concentration
	// and reject signals. Before this dispatch the LLM saw the same
	// fund-wide summary block regardless of role, and the only thing
	// distinguishing the outputs was the role label in the system
	// prompt — predictably it produced near-identical lessons across
	// the team. The dispatcher below carves out the fact-subset each
	// role actually needs so the resulting lessons read like a real
	// team's per-role journal entries instead of four paraphrases of
	// the same paragraph.
	//
	// `coverage` is the structured instrument list from migration
	// 087's fund_team_member_specialization. When set, the researcher
	// branch uses it directly; when empty the builder falls back to
	// extractFocusSymbols(focus) for legacy funds that haven't been
	// migrated yet.
	userPromptBody := buildRoleSpecificLearningBody(role, focus, coverage, learningCtx, tradeStats, dailyReturn)

	// Prompt design notes (matters because the first cut of this
	// prompt confused gemini-3.1-pro into echoing the constraint
	// phrases as plain text instead of emitting JSON — see runtime
	// logs around 16:01 SGT). Three rules that made it reliable:
	//   (1) Put the "ONLY JSON" instruction in BOTH system + user
	//       so the model can't claim it didn't see it.
	//   (2) Show an example output so the model copies the shape
	//       instead of paraphrasing the rule set.
	//   (3) Keep the constraint list ≤ 4 lines; longer bullet lists
	//       trigger paraphrasing on smaller models.
	// Sprint 3 / M6: the example uses <SYM_A>/<SYM_B> placeholders
	// (not real tickers) so smaller models don't latch onto a
	// literal symbol that has nothing to do with the actual fund.
	// We explicitly tell the model to substitute the placeholders
	// with real symbols pulled from the fund's holdings / plan
	// data above.
	systemPrompt := "你是 AI 基金团队的复盘教练。基于给定的当日数据为一位 agent 生成今日复盘和明日调整方向。" +
		"严格按 JSON 对象输出（不要 markdown 围栏、不要任何说明文字）。每条字符串：简体中文 1 句，引用数据中的具体数字、占比或股票代码，以 。 结尾。" +
		"不允许的句子：以 \"为了让\"、\"为了实现\"、\"To maximize\"、\"To improve\" 开头的空洞陈述。\n\n" +
		roleSpecificSystemHint(role) + "\n\n" +
		"重要：示例中的 <SYM_A> / <SYM_B> 是占位符，请用上文上下文里出现的真实股票代码替换；不要在最终输出中保留这两个尖括号占位符。\n\n" +
		"输出格式（这是一个示例，必须完全照抄结构）：\n" +
		"{\"lessons\":[\"<SYM_A> 当日成交 49984 元，仓位扩张到 5%，符合风控预期。\",\"组合当日收益持平但 watch 类标的仍占 60%，明显说明执行力度不足。\"]," +
		"\"adjustments\":[\"明日开盘前评估 watch 类标的是否具备转 buy 条件。\",\"对 <SYM_B> 设置明确的放弃条件以减少观望成本。\"]}"

	userPrompt := userPromptBody +
		"\n\n请仅输出 JSON 对象，2-3 条 lessons + 2-3 条 adjustments，每条不超过 60 个中文字符。"

	req := llm.ChatRequest{
		FundID:    learningCtx.fund.ID,
		ModelTier: llm.TierStandard,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		// 1200 covers up to 6 short Chinese sentences + JSON keys
		// with room to spare. The earlier value (400) was guessed
		// from an English context and routinely truncated CJK
		// payloads (≈3 bytes/char) mid-string.
		MaxTokens:   1200,
		Temperature: 0.4,
		StepName:    "agent_self_learning",
	}

	// No local timeout wrapper: the global 5-minute budget enforced
	// inside llm.Client (llmTotalRequestTimeout) already caps this
	// call. A tighter local cap was tried in an earlier iteration and
	// produced false fallbacks against slow reasoning models — leave
	// the parent ctx alone and let the client be the one source of
	// truth for "this took too long".
	resp, err := m.llmRuntime.Chat(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	if resp == nil {
		return nil, nil, errors.New("memory: empty LLM response")
	}

	lessons, adjustments, err := parseAgentLessonsResponse(resp.Content, cap)
	if err != nil {
		return nil, nil, err
	}
	// Sprint 3 / M2 fact-check: drop any string that mentions a
	// symbol the fund doesn't actually trade or a percentage that
	// is wildly inconsistent with today's actual numbers. This
	// neutralises the most common LLM hallucinations (latched on a
	// stale ticker, or invented a "+12%" gain when daily return was
	// flat) without us having to ask the model to verify itself.
	ctxCheck := buildLessonFactCheckContext(learningCtx, dailyReturn)
	lessons = factCheckLessonStrings(lessons, ctxCheck)
	adjustments = factCheckLessonStrings(adjustments, ctxCheck)
	if len(lessons) == 0 || len(adjustments) == 0 {
		return nil, nil, errors.New("memory: LLM returned empty lessons or adjustments")
	}
	return lessons, adjustments, nil
}

// parseAgentLessonsResponse pulls {"lessons":[…], "adjustments":[…]}
// out of an LLM message body. Implementations sometimes wrap the JSON
// in ```json fences or add a preamble sentence; we slice from the
// first '{' to the last '}' to survive that. Returns trimmed,
// deduped, capped lists; a string that fails the "ends with 。"
// shape is dropped (cheap proxy for "got a half-sentence
// fragment").
func parseAgentLessonsResponse(raw string, cap int) ([]string, []string, error) {
	body := strings.TrimSpace(raw)
	start := strings.Index(body, "{")
	end := strings.LastIndex(body, "}")
	if start < 0 || end <= start {
		return nil, nil, fmt.Errorf("memory: LLM response missing JSON object: %q", truncatePreview(body, 120))
	}
	body = body[start : end+1]

	var parsed struct {
		Lessons     []string `json:"lessons"`
		Adjustments []string `json:"adjustments"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return nil, nil, fmt.Errorf("memory: parse LLM lessons JSON: %w", err)
	}
	clean := func(in []string) []string {
		out := make([]string, 0, len(in))
		seen := make(map[string]struct{}, len(in))
		for _, s := range in {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			// Reject fragments that don't look like full sentences.
			// LLMs occasionally produce 一句没说完 endings; we'd rather
			// fall back to templates than serve those.
			lastRune := []rune(s)
			if len(lastRune) == 0 {
				continue
			}
			tail := lastRune[len(lastRune)-1]
			if tail != '。' && tail != '.' && tail != '!' && tail != '?' && tail != '！' && tail != '？' {
				continue
			}
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
		if cap > 0 && len(out) > cap {
			out = out[:cap]
		}
		return out
	}
	return clean(parsed.Lessons), clean(parsed.Adjustments), nil
}

// truncatePreview is a small helper for logging untrusted strings
// (LLM output, raw memory content) without flooding the slog with a
// 4KB blob. Uses runes so multi-byte CJK content gets cleanly
// truncated.
func truncatePreview(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Sprint 3 / M2: lesson fact-check.
//
// Goal: cheaply suppress the most common LLM lesson hallucinations
// before they get persisted to memory. We only check two dimensions
// because those are the two we have ground truth for:
//
//  1. Symbol citations. Any A-share-style 6-digit code or 1-5 letter
//     US-style ticker mentioned in the lesson MUST appear in either
//     the fund's current holdings or today's plan actions / universe
//     references. Otherwise the model invented it.
//
//  2. Percent citations. A percentage in the [-100%, +100%] range
//     MUST be within a tolerance band of either:
//       - today's daily return (±5pp), or
//       - any top-holding weight (±5pp).
//     Numbers outside both bands are almost always confabulated.
//     We intentionally allow positive numbers that look like notional
//     dollar amounts (e.g. "49984 元") through — those are dollar
//     figures, not percents, and aren't checkable from the inputs we
//     have here.
//
// On a check failure we DROP the offending lesson; the caller still
// receives the other surviving lessons, and the role-template
// fallback kicks in when the entire list ends up empty (see callers
// of generateAgentLessonsLLM).

const (
	lessonPercentToleranceBp = 5.0 // 5 percentage points; loose enough for paraphrased estimates
)

var (
	lessonSymbolARE     = regexp.MustCompile(`(?:^|[^0-9])(\d{6})(?:[^0-9]|$)`)         // A-share style 6-digit
	lessonSymbolUSRE    = regexp.MustCompile(`(?:^|[^A-Za-z])([A-Z]{1,5})(?:[^A-Za-z]|$)`) // US-style 1-5 cap letters
	lessonPercentRE     = regexp.MustCompile(`(-?\d+(?:\.\d+)?)\s*%`)
)

type lessonFactCheckContext struct {
	knownSymbols   map[string]struct{}
	dailyReturnPct float64 // already in percent (e.g. 0.5 for +0.5%)
	weightsPct     []float64
}

func buildLessonFactCheckContext(learningCtx *learningContext, dailyReturn float64) lessonFactCheckContext {
	ctx := lessonFactCheckContext{
		knownSymbols:   make(map[string]struct{}),
		dailyReturnPct: dailyReturn * 100,
	}
	if learningCtx == nil {
		return ctx
	}
	totalAssets := 0.0
	if learningCtx.nav != nil {
		totalAssets = learningCtx.nav.TotalAssets
	}
	for _, p := range learningCtx.positions {
		sym := strings.TrimSpace(p.Symbol)
		if sym != "" {
			ctx.knownSymbols[strings.ToUpper(sym)] = struct{}{}
		}
		if totalAssets > 0 {
			ctx.weightsPct = append(ctx.weightsPct, (p.MarketValue/totalAssets)*100)
		}
	}
	for _, a := range learningCtx.actions {
		sym := strings.TrimSpace(a.Symbol)
		if sym != "" {
			ctx.knownSymbols[strings.ToUpper(sym)] = struct{}{}
		}
	}
	return ctx
}

func factCheckLessonStrings(in []string, ctx lessonFactCheckContext) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !lessonSymbolsLookSafe(s, ctx) {
			slog.Debug("lesson fact-check: dropped (symbol)", "lesson", truncatePreview(s, 120))
			continue
		}
		if !lessonPercentsLookSafe(s, ctx) {
			slog.Debug("lesson fact-check: dropped (percent)", "lesson", truncatePreview(s, 120))
			continue
		}
		out = append(out, s)
	}
	return out
}

func lessonSymbolsLookSafe(s string, ctx lessonFactCheckContext) bool {
	if len(ctx.knownSymbols) == 0 {
		// No ground truth available — don't drop on the symbol axis.
		return true
	}
	for _, m := range lessonSymbolARE.FindAllStringSubmatch(s, -1) {
		if len(m) < 2 {
			continue
		}
		if _, ok := ctx.knownSymbols[strings.ToUpper(m[1])]; !ok {
			return false
		}
	}
	for _, m := range lessonSymbolUSRE.FindAllStringSubmatch(s, -1) {
		if len(m) < 2 {
			continue
		}
		sym := strings.ToUpper(m[1])
		if _, denyAllCaps := commonAllCapsWords[sym]; denyAllCaps {
			// Words like "PM", "NAV", "OK", "ETF" are not tickers
			// — let those through without lookup.
			continue
		}
		if _, ok := ctx.knownSymbols[sym]; !ok {
			return false
		}
	}
	return true
}

func lessonPercentsLookSafe(s string, ctx lessonFactCheckContext) bool {
	for _, m := range lessonPercentRE.FindAllStringSubmatch(s, -1) {
		if len(m) < 2 {
			continue
		}
		val, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		// Skip absurdly large numbers (e.g. "300%") — they're either
		// hyperbole or a misformatted ratio; don't reward the
		// model for them but don't drop the whole lesson either.
		if math.Abs(val) > 100 {
			continue
		}
		if math.Abs(val-ctx.dailyReturnPct) <= lessonPercentToleranceBp {
			continue
		}
		allowed := false
		for _, w := range ctx.weightsPct {
			if math.Abs(val-w) <= lessonPercentToleranceBp {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	return true
}

// decayRecentLessons drops legacy lessons whose recorded timestamp is
// older than `cutoffDays` days when projected onto an exponential
// half-life of `cutoffDays` (i.e. weight(age) = 0.5^(age/cutoffDays)).
// Lessons whose weight would fall below `minWeight` are removed.
// Lessons without a timestamp (legacy entries) are kept once but
// stamped with `now` so the next pass can decay them properly.
//
// Returns (survivors, survivorTimestamps) — same length, index-aligned.
func decayRecentLessons(lessons, timestamps []string, now time.Time, cutoffDays int, minWeight float64) ([]string, []string) {
	if cutoffDays <= 0 {
		cutoffDays = 60
	}
	survivors := make([]string, 0, len(lessons))
	stamps := make([]string, 0, len(lessons))
	for i, lesson := range lessons {
		var stamp string
		if i < len(timestamps) {
			stamp = strings.TrimSpace(timestamps[i])
		}
		if stamp == "" {
			survivors = append(survivors, lesson)
			stamps = append(stamps, now.Format(time.RFC3339))
			continue
		}
		t, err := time.Parse(time.RFC3339, stamp)
		if err != nil {
			survivors = append(survivors, lesson)
			stamps = append(stamps, now.Format(time.RFC3339))
			continue
		}
		ageDays := now.Sub(t).Hours() / 24
		if ageDays < 0 {
			ageDays = 0
		}
		halfLife := float64(cutoffDays)
		weight := math.Pow(0.5, ageDays/halfLife)
		if weight < minWeight {
			continue
		}
		survivors = append(survivors, lesson)
		stamps = append(stamps, stamp)
	}
	return survivors, stamps
}

// containsStringFold is the case-insensitive variant of slices.Contains
// for strings. Used to dedupe recentLessons before storage so the merge
// step doesn't end up with "AAPL ..." and "aapl ..." both surviving.
func containsStringFold(haystack []string, needle string) bool {
	needle = strings.TrimSpace(needle)
	for _, s := range haystack {
		if strings.EqualFold(strings.TrimSpace(s), needle) {
			return true
		}
	}
	return false
}

// existingLearningCoversToday reports whether the agent's evolution
// config already carries a lastLearningDate matching the supplied
// trading date. We compare in the agent's reported YYYY-MM-DD format
// (same shape applyLearningToEvolutionConfig writes) so timezone
// drift can't mis-trigger a re-run.
func existingLearningCoversToday(config map[string]any, tradingDate time.Time) bool {
	last := strings.TrimSpace(stringFromConfig(config, "lastLearningDate"))
	if last == "" {
		return false
	}
	return last == tradingDate.Format("2006-01-02")
}

// commonAllCapsWords keeps the US-ticker regex from rejecting common
// English/Chinese-mixed prose that happens to have all-caps tokens
// (the alternative — banning the regex altogether — would let real
// hallucinated tickers slip through).
var commonAllCapsWords = map[string]struct{}{
	"PM": {}, "NAV": {}, "OK": {}, "ETF": {}, "API": {}, "ID": {},
	"AI": {}, "ML": {}, "IPO": {}, "EPS": {}, "ROE": {}, "ROI": {},
	"OCS": {}, "QDII": {}, "RMB": {}, "USD": {}, "EUR": {}, "CN": {},
	"US": {}, "HK": {}, "TWD": {}, "JPY": {}, "CNY": {}, "DCF": {},
	"PE": {}, "PB": {}, "PS": {}, "MA": {}, "RSI": {}, "MACD": {},
	"VWAP": {}, "TWAP": {}, "P": {}, "T": {}, "B": {}, "A": {},
	"OK?": {}, "NB": {},
}

func buildSpecializationLearningSummary(role string, learningCtx *learningContext, lessons, adjustments []string) *specializationLearningSummary {
	if learningCtx == nil || learningCtx.fund == nil {
		return nil
	}
	profile := decodeFundMarketProfile(learningCtx.fund.Config)
	markets, assetClasses, themes, instruments, styleHints := collectFundSpecializationTargets(profile)
	delta := 0.15
	if learningCtx.nav != nil {
		switch {
		case learningCtx.nav.DailyReturn > 0:
			delta = 0.8
		case learningCtx.nav.DailyReturn < 0:
			delta = -0.8
		default:
			delta = 0.2
		}
	}
	summary := &specializationLearningSummary{
		RecentLessons:   limitStrings(uniqueNonEmpty(lessons), 3),
		LastAdjustments: limitStrings(uniqueNonEmpty(adjustments), 3),
	}
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "researcher":
		summary.Markets = specializationScoreMap(markets, delta)
		summary.Themes = specializationScoreMap(themes, delta)
		summary.Instruments = specializationScoreMap(instruments, delta/2)
		summary.StyleHints = specializationScoreMap(styleHints, delta)
	case "pm":
		summary.Markets = specializationScoreMap(markets, delta)
		summary.AssetClasses = specializationScoreMap(assetClasses, delta)
		summary.Themes = specializationScoreMap(themes, delta)
		summary.Instruments = specializationScoreMap(instruments, delta/2)
		summary.StyleHints = specializationScoreMap(styleHints, delta)
	case "risk":
		summary.Markets = specializationScoreMap(markets, delta)
		summary.AssetClasses = specializationScoreMap(assetClasses, delta)
		summary.StyleHints = specializationScoreMap(styleHints, delta)
	case "trader":
		summary.Markets = specializationScoreMap(markets, delta)
		summary.Instruments = specializationScoreMap(instruments, delta)
		summary.StyleHints = specializationScoreMap(styleHints, delta/2)
	default:
		summary.Markets = specializationScoreMap(markets, delta)
		summary.AssetClasses = specializationScoreMap(assetClasses, delta)
		summary.Themes = specializationScoreMap(themes, delta)
		summary.Instruments = specializationScoreMap(instruments, delta)
		summary.StyleHints = specializationScoreMap(styleHints, delta)
	}
	if len(summary.Markets) == 0 && len(summary.AssetClasses) == 0 && len(summary.Themes) == 0 && len(summary.Instruments) == 0 && len(summary.StyleHints) == 0 {
		return nil
	}
	return summary
}

func specializationScoreMap(targets []string, delta float64) map[string]float64 {
	if len(targets) == 0 || delta == 0 {
		return nil
	}
	result := make(map[string]float64, len(targets))
	for _, target := range normalizedStringSlice(targets) {
		result[target] = delta
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func (m *runtimeMemorySystem) writeLearningMemory(ctx context.Context, fundID string, agentID sql.NullString, layer string, tradingDate time.Time, dailyReturn float64, learning learningResult) error {
	payload := map[string]any{
		"summary":     learning.Summary,
		"hits":        learning.Hits,
		"misses":      learning.Misses,
		"lessons":     learning.Lessons,
		"adjustments": learning.Adjustments,
		"dailyReturn": dailyReturn,
		"tags":        learning.Tags,
		"recordedAt":  time.Now().UTC().Format(time.RFC3339),
	}
	if learning.Specialization != nil {
		payload["specializationLearning"] = learning.Specialization
	}
	content, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	memoryID, err := m.memoryRepo.Create(ctx, &repository.Memory{
		FundID:      fundID,
		AgentID:     agentID,
		Layer:       layer,
		Title:       nullString(strings.TrimSpace(learning.Title)),
		Content:     string(content),
		TradingDate: sql.NullTime{Time: tradingDate, Valid: true},
		Tags:        uniqueNonEmpty(learning.Tags),
	})
	if err != nil {
		return err
	}
	// Sprint 3 / M1: 把每条 lesson 解析成 hypothesis 落到 lineage 表，
	// 评分 worker 之后会按窗口到期 score。代理级 lesson (layer=agent)
	// 才落 — fund-level 那条已经聚合过了，重复入会把 hit-rate 算重。
	if m.db != nil && layer == "agent" && len(learning.Lessons) > 0 {
		recordLessonLineage(ctx, m.db, memoryID, fundID, agentID, learning.Lessons, tradingDate)
	}
	return nil
}

func extractEvolutionSpecialization(raw json.RawMessage) specializationLearningSummary {
	config := make(map[string]any)
	if len(raw) > 0 && string(raw) != "null" {
		_ = json.Unmarshal(raw, &config)
	}
	specializationConfig := mapFromConfig(config, "specializationLearning")
	return specializationLearningSummary{
		Markets:         scoreMapFromConfig(specializationConfig, "markets"),
		AssetClasses:    scoreMapFromConfig(specializationConfig, "assetClasses"),
		Themes:          scoreMapFromConfig(specializationConfig, "themes"),
		Instruments:     scoreMapFromConfig(specializationConfig, "instruments"),
		StyleHints:      scoreMapFromConfig(specializationConfig, "styleHints"),
		RecentLessons:   stringSliceFromConfig(specializationConfig, "recentLessons"),
		LastAdjustments: stringSliceFromConfig(specializationConfig, "lastAdjustments"),
	}
}

func topSpecializationScoreLines(learning specializationLearningSummary) []string {
	lines := []string{}
	if line := topSpecializationScoreLine("markets", learning.Markets); line != "" {
		lines = append(lines, line)
	}
	if line := topSpecializationScoreLine("assetClasses", learning.AssetClasses); line != "" {
		lines = append(lines, line)
	}
	if line := topSpecializationScoreLine("themes", learning.Themes); line != "" {
		lines = append(lines, line)
	}
	if line := topSpecializationScoreLine("instruments", learning.Instruments); line != "" {
		lines = append(lines, line)
	}
	if line := topSpecializationScoreLine("styleHints", learning.StyleHints); line != "" {
		lines = append(lines, line)
	}
	return lines
}

func topSpecializationScoreLine(label string, values map[string]float64) string {
	if len(values) == 0 {
		return ""
	}
	type scoreEntry struct {
		key   string
		score float64
	}
	entries := make([]scoreEntry, 0, len(values))
	for key, value := range values {
		if strings.TrimSpace(key) == "" || value <= 0 {
			continue
		}
		entries = append(entries, scoreEntry{key: strings.TrimSpace(key), score: value})
	}
	if len(entries) == 0 {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].score == entries[j].score {
			return entries[i].key < entries[j].key
		}
		return entries[i].score > entries[j].score
	})
	parts := make([]string, 0, 2)
	for _, entry := range entries[:min(2, len(entries))] {
		parts = append(parts, fmt.Sprintf("%s(+%.2f)", entry.key, entry.score))
	}
	return label + "=" + strings.Join(parts, ", ")
}

func parseEvolutionLearningConfig(raw json.RawMessage) (map[string]any, bool, bool, int) {
	config := make(map[string]any)
	if len(raw) > 0 && string(raw) != "null" {
		_ = json.Unmarshal(raw, &config)
	}
	enabled := true
	if value, ok := boolFromConfig(config, "dailyLearningEnabled"); ok {
		enabled = value
	}
	autoApply := true
	if value, ok := boolFromConfig(config, "autoApplyAdjustments"); ok {
		autoApply = value
	}
	maxLessons := 3
	if value, ok := intFromConfig(config, "maxLessonsPerDay"); ok && value > 0 {
		maxLessons = value
	}
	return config, enabled, autoApply, maxLessons
}

func applyLearningConfigInput(config map[string]any, input api.AgentLearningConfigInput) {
	if config == nil {
		return
	}
	if input.AutoApplyAdjustments != nil {
		config["autoApplyAdjustments"] = *input.AutoApplyAdjustments
	}
	if input.MaxLessonsPerDay != nil {
		maxLessons := *input.MaxLessonsPerDay
		if maxLessons < 1 {
			maxLessons = 1
		}
		if maxLessons > 20 {
			maxLessons = 20
		}
		config["maxLessonsPerDay"] = maxLessons
	}
	if input.Scope != nil {
		config["learningScope"] = learningScopeToConfig(*input.Scope)
	}
	config["learningUpdatedAt"] = time.Now().UTC().Format(time.RFC3339)
}

func learningScopeToConfig(scope api.AgentLearningScope) map[string]any {
	return compactConfigMap(map[string]any{
		"fundIds":      uniqueNonEmpty(scope.FundIDs),
		"markets":      uniqueNonEmpty(scope.Markets),
		"assetClasses": uniqueNonEmpty(scope.AssetClasses),
		"themes":       uniqueNonEmpty(scope.Themes),
		"instruments":  uniqueNonEmpty(scope.Instruments),
		"styleHints":   uniqueNonEmpty(scope.StyleHints),
		"memoryScope":  strings.TrimSpace(scope.MemoryScope),
	})
}

func learningScopeFromConfig(config map[string]any) *api.AgentLearningScope {
	scopeConfig := mapFromConfig(config, "learningScope")
	if len(scopeConfig) == 0 {
		return nil
	}
	scope := &api.AgentLearningScope{
		FundIDs:      stringSliceFromConfig(scopeConfig, "fundIds"),
		Markets:      stringSliceFromConfig(scopeConfig, "markets"),
		AssetClasses: stringSliceFromConfig(scopeConfig, "assetClasses"),
		Themes:       stringSliceFromConfig(scopeConfig, "themes"),
		Instruments:  stringSliceFromConfig(scopeConfig, "instruments"),
		StyleHints:   stringSliceFromConfig(scopeConfig, "styleHints"),
		MemoryScope:  stringFromConfig(scopeConfig, "memoryScope"),
	}
	if len(scope.FundIDs) == 0 && len(scope.Markets) == 0 && len(scope.AssetClasses) == 0 && len(scope.Themes) == 0 && len(scope.Instruments) == 0 && len(scope.StyleHints) == 0 && strings.TrimSpace(scope.MemoryScope) == "" {
		return nil
	}
	return scope
}

func learningScopeAllowsFund(scope *api.AgentLearningScope, fundID string) bool {
	if scope == nil || len(scope.FundIDs) == 0 {
		return true
	}
	for _, allowed := range scope.FundIDs {
		if strings.EqualFold(strings.TrimSpace(allowed), strings.TrimSpace(fundID)) {
			return true
		}
	}
	return false
}

func applyLearningToEvolutionConfig(config map[string]any, learning learningResult, tradingDate time.Time, dailyReturn float64) (json.RawMessage, error) {
	if config == nil {
		config = make(map[string]any)
	}
	existingLessons := stringSliceFromConfig(config, "recentLessons")
	// Sprint 3 / M4: 给 recentLessons 加上指数衰减 — 60+ 天的旧 lesson
	// 权重 < 0.1 时直接被淘汰出 recentLessons bag。新 lesson 总是带
	// 最新 lessonsUpdatedAt timestamp，所以下次 decay 才有起算点。
	// 我们也写入 lessonsTimestamps 数组（与 recentLessons 索引对齐）
	// 这样下次再 decay 时知道哪条多老。读取端不读 timestamps 也无碍
	// （legacy config 没这个字段，按 "全部认为今天写入" 处理 — 衰减
	// 会从下一次开始生效）。
	now := tradingDate
	if now.IsZero() {
		now = time.Now().UTC()
	}
	existingTimestamps := stringSliceFromConfig(config, "recentLessonsTimestamps")
	survivors, survivorTimestamps := decayRecentLessons(existingLessons, existingTimestamps, now, 60, 0.1)
	merged := append([]string{}, learning.Lessons...)
	mergedTimestamps := make([]string, 0, len(learning.Lessons))
	stamp := now.Format(time.RFC3339)
	for range learning.Lessons {
		mergedTimestamps = append(mergedTimestamps, stamp)
	}
	for i, s := range survivors {
		if containsStringFold(merged, s) {
			continue
		}
		merged = append(merged, s)
		if i < len(survivorTimestamps) {
			mergedTimestamps = append(mergedTimestamps, survivorTimestamps[i])
		} else {
			mergedTimestamps = append(mergedTimestamps, stamp)
		}
	}
	merged = limitStrings(uniqueNonEmpty(merged), 8)
	if len(mergedTimestamps) > len(merged) {
		mergedTimestamps = mergedTimestamps[:len(merged)]
	}
	config["recentLessons"] = merged
	config["recentLessonsTimestamps"] = mergedTimestamps
	config["lastLearningSummary"] = learning.Summary
	config["lastLearningDate"] = tradingDate.Format("2006-01-02")
	config["lastLearningTags"] = uniqueNonEmpty(learning.Tags)
	config["lastRecommendedAdjustments"] = learning.Adjustments
	config["lastDailyReturn"] = dailyReturn
	config["learningUpdatedAt"] = time.Now().UTC().Format(time.RFC3339)
	if learning.Specialization != nil {
		existing := mapFromConfig(config, "specializationLearning")
		merged := map[string]any{
			"markets":         mergeSpecializationScoreMaps(scoreMapFromConfig(existing, "markets"), learning.Specialization.Markets),
			"assetClasses":    mergeSpecializationScoreMaps(scoreMapFromConfig(existing, "assetClasses"), learning.Specialization.AssetClasses),
			"themes":          mergeSpecializationScoreMaps(scoreMapFromConfig(existing, "themes"), learning.Specialization.Themes),
			"instruments":     mergeSpecializationScoreMaps(scoreMapFromConfig(existing, "instruments"), learning.Specialization.Instruments),
			"styleHints":      mergeSpecializationScoreMaps(scoreMapFromConfig(existing, "styleHints"), learning.Specialization.StyleHints),
			"recentLessons":   limitStrings(uniqueNonEmpty(append(learning.Specialization.RecentLessons, stringSliceFromConfig(existing, "recentLessons")...)), 6),
			"lastAdjustments": limitStrings(uniqueNonEmpty(append(learning.Specialization.LastAdjustments, stringSliceFromConfig(existing, "lastAdjustments")...)), 6),
		}
		config["specializationLearning"] = compactConfigMap(merged)
	}
	updated, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(updated), nil
}

func mergeSpecializationScoreMaps(existing, delta map[string]float64) map[string]float64 {
	if len(existing) == 0 && len(delta) == 0 {
		return nil
	}
	merged := make(map[string]float64, len(existing)+len(delta))
	for key, value := range existing {
		if strings.TrimSpace(key) != "" {
			merged[strings.TrimSpace(key)] = value
		}
	}
	for key, value := range delta {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" || value == 0 {
			continue
		}
		merged[trimmedKey] = math.Max(-3, math.Min(3, merged[trimmedKey]+value))
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func compactConfigMap(config map[string]any) map[string]any {
	if len(config) == 0 {
		return nil
	}
	compact := make(map[string]any, len(config))
	for key, value := range config {
		skip := false
		switch typed := value.(type) {
		case nil:
			skip = true
		case string:
			skip = strings.TrimSpace(typed) == ""
		case []string:
			skip = len(typed) == 0
		case map[string]any:
			skip = len(typed) == 0
		case map[string]float64:
			skip = len(typed) == 0
		}
		if !skip {
			compact[key] = value
		}
	}
	if len(compact) == 0 {
		return nil
	}
	return compact
}

func boolFromConfig(config map[string]any, key string) (bool, bool) {
	value, ok := config[key]
	if !ok {
		return false, false
	}
	parsed, ok := value.(bool)
	return parsed, ok
}

func intFromConfig(config map[string]any, key string) (int, bool) {
	value, ok := config[key]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	default:
		return 0, false
	}
}

func floatFromConfig(config map[string]any, key string) (float64, bool) {
	value, ok := config[key]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func stringFromConfig(config map[string]any, key string) string {
	value, ok := config[key]
	if !ok {
		return ""
	}
	parsed, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(parsed)
}

func stringSliceFromConfig(config map[string]any, key string) []string {
	value, ok := config[key]
	if !ok {
		return nil
	}
	switch items := value.(type) {
	case []any:
		result := make([]string, 0, len(items))
		for _, item := range items {
			text, ok := item.(string)
			if ok && strings.TrimSpace(text) != "" {
				result = append(result, strings.TrimSpace(text))
			}
		}
		return result
	case []string:
		return uniqueNonEmpty(items)
	default:
		return nil
	}
}

func mapFromConfig(config map[string]any, key string) map[string]any {
	value, ok := config[key]
	if !ok {
		return nil
	}
	parsed, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return parsed
}

func scoreMapFromConfig(config map[string]any, key string) map[string]float64 {
	value, ok := config[key]
	if !ok {
		return nil
	}
	result := map[string]float64{}
	switch items := value.(type) {
	case map[string]any:
		for itemKey, itemValue := range items {
			trimmedKey := strings.TrimSpace(itemKey)
			if trimmedKey == "" {
				continue
			}
			switch typed := itemValue.(type) {
			case float64:
				result[trimmedKey] = typed
			case int:
				result[trimmedKey] = float64(typed)
			}
		}
	case map[string]float64:
		for itemKey, itemValue := range items {
			trimmedKey := strings.TrimSpace(itemKey)
			if trimmedKey != "" {
				result[trimmedKey] = itemValue
			}
		}
	default:
		return nil
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

type tradeSummary struct {
	total     int
	filled    int
	partial   int
	rejected  int
	fillRatio float64
	// twapSliceCount is the number of CHILD trade rows seen
	// across all parents — i.e. the count of TWAP/VWAP/iceberg
	// slices the splitter materialised this period. Zero when
	// the splitter wasn't engaged (everything was a single-row
	// trade). Surfaces into the trader-role learning prompt so
	// the LLM can speak to slice-level execution quality.
	twapSliceCount int
	// twapParentCount is the number of distinct parents that
	// had at least one child slice (== number of plan_actions
	// that went through the splitter). Combined with
	// twapSliceCount the LLM can compute average slices/parent.
	twapParentCount int
}

// summarizeTrades aggregates per-action counters across the
// day's trade rows. It is splitter-aware: child rows (rows with
// strategy_parent_trade_id NOT NULL) are NOT counted toward
// "total" / "filled" / "partial" / "rejected" — those slices
// are already represented by their parent row, and counting
// both would double-count the plan_action. Children DO feed
// twapSliceCount / twapParentCount so the LLM can still see
// "this day had 5 TWAP intents that landed in 25 slices".
//
// fillRatio is computed against the requested quantity from
// plan actions and the sum of CHILD fills (children carry the
// actual per-slice filled_qty in the splitter world). When the
// splitter is off, child rows == 0 and the function degrades
// to the parent's filled_qty — identical to the pre-splitter
// behaviour for legacy single-row trades.
func summarizeTrades(actions []repository.PlanAction, trades []repository.TradeExecution) tradeSummary {
	summary := tradeSummary{}
	requested := 0.0
	filled := 0.0
	for _, action := range actions {
		quantity := normalizedQuantity(action.Quantity, action.Amount, fallbackPositive(action.Price.Float64, 1))
		if quantity > 0 {
			requested += float64(quantity)
		}
	}
	// Track which parents we've seen at least one child for so
	// the LLM-side twapParentCount only counts distinct parents.
	parentIDsWithChildren := make(map[string]struct{}, len(trades))
	for _, trade := range trades {
		// Splitter shape: trade rows where
		// strategy_parent_trade_id is set are children of a
		// parent row that's ALSO in this slice. The parent is
		// the canonical plan_action accounting record (its
		// quantity == sum of children); treating the child
		// rows as independent "trades" would double-count
		// everything from total to fillRatio. So we skip them
		// for the per-plan-action counters and only collect
		// slice-level metadata for the trader prompt.
		if trade.StrategyParentTradeID.Valid && strings.TrimSpace(trade.StrategyParentTradeID.String) != "" {
			summary.twapSliceCount++
			parentIDsWithChildren[trade.StrategyParentTradeID.String] = struct{}{}
			// Children carry the per-slice fill quantity; the
			// parent row's filled_qty mirrors the sum so we
			// count children here and skip the parent below.
			filled += trade.FilledQty
			continue
		}
		summary.total++
		switch strings.ToLower(strings.TrimSpace(trade.Status)) {
		case "filled":
			summary.filled++
		case "partial":
			summary.partial++
		case "rejected", "failed", "cancelled":
			summary.rejected++
		}
	}
	// Non-split (standalone) parents: their filled_qty is the
	// actual fill. Split parents: skip — children already
	// contributed above so we'd double-count.
	for _, trade := range trades {
		if trade.StrategyParentTradeID.Valid && strings.TrimSpace(trade.StrategyParentTradeID.String) != "" {
			continue
		}
		if _, hasChildren := parentIDsWithChildren[trade.ID]; hasChildren {
			continue
		}
		filled += trade.FilledQty
	}
	summary.twapParentCount = len(parentIDsWithChildren)
	if requested <= 0 {
		requested = filled
	}
	if requested > 0 {
		summary.fillRatio = math.Min(1, filled/requested)
	}
	return summary
}

func largestPositionWeight(positions []repository.HoldingPosition) (float64, string) {
	maxWeight := 0.0
	symbol := ""
	for _, position := range positions {
		if position.Weight > maxWeight {
			maxWeight = position.Weight
			symbol = position.Symbol
		}
	}
	return maxWeight, symbol
}

func normalizedMaxLessons(value int) int {
	if value <= 0 {
		return 3
	}
	if value > 6 {
		return 6
	}
	return value
}

func limitStrings(values []string, limit int) []string {
	if limit <= 0 || len(values) <= limit {
		return values
	}
	return append([]string(nil), values[:limit]...)
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}

func workflowTradingDate(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}

func convertABTest(test *repository.ABTest) *api.ABTest {
	if test == nil {
		return nil
	}
	result := &api.ABTest{
		ID:              test.ID,
		Name:            test.Name,
		ControlFundID:   test.ControlFundID,
		TreatmentFundID: test.TreatmentFundID,
		VariableType:    test.VariableType,
		VariableConfig:  append(json.RawMessage(nil), test.VariableConfig...),
		Status:          test.Status,
		CreatedAt:       test.CreatedAt,
		UpdatedAt:       test.UpdatedAt,
	}
	if test.StartDate.Valid {
		result.StartDate = test.StartDate.Time.Format("2006-01-02")
	}
	if test.EndDate.Valid {
		result.EndDate = test.EndDate.Time.Format("2006-01-02")
	}
	if len(test.Results) > 0 && string(test.Results) != "null" {
		var parsed api.ABTestResults
		if err := json.Unmarshal(test.Results, &parsed); err == nil {
			result.Results = &parsed
		}
	}
	return result
}

func convertPlan(plan *repository.InvestmentPlan, actions []repository.PlanAction) *api.Plan {
	return convertPlanWithLocale("", nil, plan, actions)
}

// attachDecisionSource is the Sprint 11.3 opt-in augment that pulls the
// provenance tag + redacted fallback reason from PlanRepo and stamps
// them onto the API-facing Plan. Soft-fail by design: a transient
// repo error logs a warning and leaves the chip absent rather than
// breaking the plan render. Endpoints that want the chip call this
// right after convertPlan(); endpoints that don't (e.g. bulk plan
// list views) skip it to avoid the per-row round trip.
//
// The returned Plan is the same pointer that was passed in — we
// modify in place so the convention matches the rest of the
// converter layer.
func attachDecisionSource(ctx context.Context, repo *repository.PlanRepo, plan *api.Plan) {
	if plan == nil || repo == nil || strings.TrimSpace(plan.ID) == "" {
		return
	}
	source, reasonJSON, err := repo.GetDecisionSource(ctx, plan.ID)
	if err != nil {
		slog.Warn("attachDecisionSource: lookup failed",
			"planId", plan.ID, "fundId", plan.FundID, "err", err)
		return
	}
	plan.DecisionSource = source
	if len(reasonJSON) == 0 {
		return
	}
	var detail errorclass.Detail
	if err := json.Unmarshal(reasonJSON, &detail); err != nil {
		slog.Warn("attachDecisionSource: unmarshal reason failed",
			"planId", plan.ID, "err", err)
		return
	}
	// Strip the technical Summary — non-admin callers must not see
	// raw provider error text. The admin LLM-health board (S11.4)
	// reads the raw JSONB column directly and bypasses this
	// converter.
	plan.FallbackReason = &api.PlanFallbackReason{
		Category: string(detail.Category),
		Provider: detail.Provider,
	}
	if !detail.At.IsZero() {
		plan.FallbackReason.At = detail.At.UTC().Format(time.RFC3339)
	}
}

func convertPlanWithLocale(userID string, runtime *llmRuntime, plan *repository.InvestmentPlan, actions []repository.PlanAction) *api.Plan {
	if plan == nil {
		return nil
	}
	result := &api.Plan{
		ID:                 plan.ID,
		FundID:             plan.FundID,
		Status:             plan.Status,
		Reasoning:          strings.TrimSpace(plan.Reasoning.String),
		RiskReview:         plan.RiskReview,
		DiscussionSnapshot: append(json.RawMessage(nil), plan.DiscussionSnapshot...),
		CreatedAt:          plan.CreatedAt,
		UpdatedAt:          plan.UpdatedAt,
	}
	if !plan.TradingDate.IsZero() {
		result.TradingDate = plan.TradingDate.Format(time.RFC3339)
	}
	if plan.RiskScore.Valid {
		value := plan.RiskScore.Float64
		result.RiskScore = &value
	}
	if plan.ExpectedReturn.Valid {
		value := plan.ExpectedReturn.Float64
		result.ExpectedReturn = &value
	}
	if plan.RoundtableID.Valid {
		result.RoundtableID = plan.RoundtableID.String
	}
	if plan.PMAgentID.Valid {
		result.PMAgentID = plan.PMAgentID.String
	}

	// Build action list up front so we can compute its translation
	// payload in parallel with the plan-reasoning translation below.
	var (
		actionReasoning        []string
		actionReasoningIndexes []int
	)
	if len(actions) > 0 {
		result.Actions = make([]api.PlanAction, 0, len(actions))
		actionReasoning = make([]string, 0, len(actions))
		actionReasoningIndexes = make([]int, 0, len(actions))
		for i := range actions {
			converted := convertPlanAction(&actions[i])
			result.Actions = append(result.Actions, converted)
			if reasoning := strings.TrimSpace(converted.Reasoning); reasoning != "" {
				actionReasoning = append(actionReasoning, reasoning)
				actionReasoningIndexes = append(actionReasoningIndexes, len(result.Actions)-1)
			}
		}
	}

	// F32: fan out the two independent LLM translation calls (plan
	// reasoning at TierCritical, action reasoning batch at TierSimple)
	// so they run concurrently instead of serially. Cache hits return
	// immediately and the WaitGroup adds no measurable overhead.
	var (
		fanOut       sync.WaitGroup
		planZh       []string
		planEn       []string
		actionZh     []string
		actionEn     []string
	)
	if strings.TrimSpace(result.Reasoning) != "" {
		fanOut.Add(1)
		go func() {
			defer fanOut.Done()
			planZh, planEn = translateBilingualList(userID, runtime, "pm_plan", []string{result.Reasoning}, llm.TierCritical)
		}()
	}
	if len(actionReasoningIndexes) > 0 {
		fanOut.Add(1)
		go func() {
			defer fanOut.Done()
			actionZh, actionEn = translateBilingualList(userID, runtime, "pm_plan", actionReasoning, llm.TierSimple)
		}()
	}
	fanOut.Wait()

	result.ReasoningZh = localizedSingleValue(planZh, 0)
	result.ReasoningEn = localizedSingleValue(planEn, 0)
	for idx, actionIndex := range actionReasoningIndexes {
		result.Actions[actionIndex].ReasoningZh = localizedSingleValue(actionZh, idx)
		result.Actions[actionIndex].ReasoningEn = localizedSingleValue(actionEn, idx)
	}
	if runtime != nil {
		slog.Info("pm plan localization", "reasoningZhSet", result.ReasoningZh != "", "reasoningEnSet", result.ReasoningEn != "", "actionCount", len(result.Actions), "actionZhCount", len(actionZh), "actionEnCount", len(actionEn), "fundId", strings.TrimSpace(plan.FundID), "planId", strings.TrimSpace(plan.ID))
	}
	return result
}

func localizedListValue(values []string, index int, fallback string) string {
	if index >= 0 && index < len(values) {
		if trimmed := strings.TrimSpace(values[index]); trimmed != "" {
			return trimmed
		}
	}
	return strings.TrimSpace(fallback)
}

// localizedSingleValue returns the trimmed value at index without falling back
// to a different language's text. Used for ReasoningZh / ReasoningEn /
// SummaryZh / SummaryEn so that a failed translation does not pollute the
// other-language field with English (or vice versa). Frontend can then choose
// its own fallback when the language-specific value is empty.
func localizedSingleValue(values []string, index int) string {
	if index >= 0 && index < len(values) {
		return strings.TrimSpace(values[index])
	}
	return ""
}

func localizedListWithFallback(values []string, fallback []string) []string {
	if len(fallback) == 0 {
		return normalizeStringList(values)
	}
	result := make([]string, 0, len(fallback))
	for i := range fallback {
		result = append(result, localizedListValue(values, i, fallback[i]))
	}
	return normalizeStringList(result)
}

func convertPlanAction(action *repository.PlanAction) api.PlanAction {
	result := api.PlanAction{}
	if action == nil {
		return result
	}
	result.ID = action.ID
	result.InstrumentKey = action.InstrumentKey
	result.Action = action.Action
	result.Symbol = action.Symbol
	result.Market = action.Market.String
	result.Exchange = action.Exchange.String
	result.AssetClass = action.AssetClass.String
	result.InstrumentType = action.InstrumentType.String
	result.PositionSide = action.PositionSide.String
	result.OpenClose = action.OpenClose.String
	result.Reasoning = strings.TrimSpace(action.Reasoning.String)
	result.SupportedBy = append([]string(nil), action.SupportedBy...)
	result.OpposedBy = append([]string(nil), action.OpposedBy...)
	result.ExecutionStatus = action.ExecutionStatus
	result.SortOrder = action.SortOrder
	result.QuoteCurrency = action.QuoteCurrency.String
	result.SettlementCurrency = action.SettlementCurrency.String
	result.MarginMode = action.MarginMode.String
	if action.Quantity.Valid {
		value := action.Quantity.Float64
		result.Quantity = &value
	}
	if action.Price.Valid {
		value := action.Price.Float64
		result.Price = &value
	}
	if action.Amount.Valid {
		value := action.Amount.Float64
		result.Amount = &value
	}
	if action.StopLoss.Valid {
		value := action.StopLoss.Float64
		result.StopLoss = &value
	}
	if action.TakeProfit.Valid {
		value := action.TakeProfit.Float64
		result.TakeProfit = &value
	}
	if action.Confidence.Valid {
		value := action.Confidence.Float64
		result.Confidence = &value
	}
	if action.Leverage.Valid {
		value := action.Leverage.Float64
		result.Leverage = &value
	}
	if action.ContractMultiplier.Valid {
		value := action.ContractMultiplier.Float64
		result.ContractMultiplier = &value
	}
	if action.ExpiryDate.Valid {
		result.ExpiryDate = action.ExpiryDate.Time.Format("2006-01-02")
	}
	if action.ReduceOnly.Valid {
		value := action.ReduceOnly.Bool
		result.ReduceOnly = &value
	}
	if action.QuoteRefreshedAt.Valid {
		result.QuoteRefreshedAt = action.QuoteRefreshedAt.Time.UTC().Format(time.RFC3339)
	}
	return result
}

func mapRepositoryError(err error) error {
	if errors.Is(err, repository.ErrNotFound) {
		return api.ErrNotFound
	}
	return err
}

type fundMarketProfile struct {
	Market           string                  `json:"market,omitempty"`
	Exchange         string                  `json:"exchange,omitempty"`
	AssetClass       string                  `json:"assetClass,omitempty"`
	BaseCurrency     string                  `json:"baseCurrency,omitempty"`
	BenchmarkSymbol  string                  `json:"benchmarkSymbol,omitempty"`
	PrimaryDirection string                  `json:"primaryDirection,omitempty"`
	CalendarCode     string                  `json:"calendarCode,omitempty"`
	TimeZone         string                  `json:"timeZone,omitempty"`
	Universe         *api.FundUniverse       `json:"universe,omitempty"`
	TeamIntervals    *api.FundTeamIntervals  `json:"teamIntervals,omitempty"`
	Specialization   *api.FundSpecialization `json:"specialization,omitempty"`
	HardRisk         *api.FundHardRiskConfig `json:"hardRisk,omitempty"`
	// AutoExecute persists the per-fund auto-execute toggle + guardrails
	// in fund.config. nil means "auto-execute disabled" (treated as
	// Enabled=false by callers); a non-nil pointer stores both the
	// toggle and the guardrail thresholds.
	AutoExecute *api.FundAutoExecuteConfig `json:"autoExecute,omitempty"`
	// ResearchTier picks which roundtable implementation runs for the
	// fund. Phase 2B introduces "advanced" → the multi-agent bull/
	// bear/quant debate. Any other value (including the default empty
	// string) keeps the legacy text-concat roundtable so existing
	// funds inherit the cheaper path automatically.
	ResearchTier string `json:"researchTier,omitempty"`
	// ActivityRetentionDays is the per-fund retention horizon for the
	// Team Live Activity panel. nil means "use the default (7)" so
	// existing funds created before this field existed transparently
	// inherit the same value the panel ships with.
	ActivityRetentionDays *int `json:"activityRetentionDays,omitempty"`
	// ExposurePolicy is the per-fund override for the Sprint C #1
	// portfolio-exposure caps. nil = use the AQR / Bridgewater /
	// Citadel defaults baked into buildExposureSnapshot. Each
	// inner field is also nullable so an operator can tighten one
	// dimension (e.g. cap single-name at 15% for a high-conviction
	// concentrated fund) without disturbing the others.
	ExposurePolicy *FundExposurePolicy `json:"exposurePolicy,omitempty"`
	// CorrelationPolicy is the per-fund override for the Sprint C
	// #2 pairwise correlation matrix. nil = use the package
	// defaults (60-day lookback, 0.7 |rho| floor, 10 max pairs).
	// Same nullable-field convention as ExposurePolicy.
	CorrelationPolicy *FundCorrelationPolicy `json:"correlationPolicy,omitempty"`
	// ReflectionCadenceDays overrides the long-term reflection
	// rate-limit window (defaultReflectionCadenceDays = 7). nil
	// keeps the default; setting it to e.g. 3 makes a fast-moving
	// macro fund re-distil twice as often, while a long-horizon
	// value fund can stretch to 14 to save LLM tokens. Clamped to
	// [1, 30] by the consumer.
	ReflectionCadenceDays *int `json:"reflectionCadenceDays,omitempty"`
}

// FundExposurePolicy is the per-fund override surface for the
// Sprint C #1 portfolio guardrails. Each pointer field is a
// percentage in [0, 1]; nil means "fall back to the package
// default". All values run through the same withDefaults clamp
// inside exposure.Options so an out-of-range JSON entry doesn't
// silently break the breach math.
type FundExposurePolicy struct {
	// SingleNameCapPct caps any single position's weight. Default
	// 0.25 (25%) — the classic concentrated-fund threshold.
	SingleNameCapPct *float64 `json:"singleNameCapPct,omitempty"`
	// SectorCapPct caps the aggregate weight of any one sector.
	// Default 0.50 (50%) — Bridgewater All Weather style.
	SectorCapPct *float64 `json:"sectorCapPct,omitempty"`
	// Top3CapPct caps the sum of the top three position weights.
	// Default 0.60 (60%) — a Citadel-style diversification floor.
	Top3CapPct *float64 `json:"top3CapPct,omitempty"`
	// CashFloorPct enforces a minimum cash buffer. Default 0.05
	// (5%) — the standard "no fully-deployed" guardrail.
	CashFloorPct *float64 `json:"cashFloorPct,omitempty"`
}

// FundCorrelationPolicy is the per-fund override surface for the
// Sprint C #2 pairwise correlation matrix. Same nullable-field
// convention as FundExposurePolicy so an operator can tune one
// dimension without disturbing the rest.
type FundCorrelationPolicy struct {
	// LookbackDays is the rolling-window size for the daily
	// returns used by the Pearson math. Default 60 (≈ 3 months);
	// clamped to [20, 252].
	LookbackDays *int `json:"lookbackDays,omitempty"`
	// HighCorrThreshold is the |rho| floor for the HighCorrPairs
	// list surfaced to the PM. Default 0.7 — the conventional
	// "diversifying" cutoff. Clamped to [0.3, 0.99].
	HighCorrThreshold *float64 `json:"highCorrThreshold,omitempty"`
	// MaxHighCorrPairs caps how many pairs the prompt actually
	// receives so a degenerate universe doesn't blow the context.
	// Default 10; clamped to [1, 50].
	MaxHighCorrPairs *int `json:"maxHighCorrPairs,omitempty"`
}

// DefaultActivityRetentionDays is the retention horizon applied to a
// fund whose config doesn't pin a value (existing funds before this
// feature shipped + brand new funds with the field omitted).
const DefaultActivityRetentionDays = 7

// MaxActivityRetentionDays is the operator-facing ceiling. The DB can
// of course hold longer histories, but we deliberately cap the UI to
// keep table sizes predictable across many funds (worst case ~200
// events/fund/day × 10 days × N funds).
const MaxActivityRetentionDays = 10

// normalizeActivityRetentionDays clamps a user-supplied value to the
// supported range [1, MaxActivityRetentionDays]. Returns nil when v
// is nil so callers (PATCH merge) can distinguish "leave unchanged"
// from "explicitly set". Invalid pointers (≤0 or >10) round to the
// nearest valid bound rather than rejecting the whole request — the
// alternative (return 400) is too noisy for a forgiving setting like
// retention.
func normalizeActivityRetentionDays(v *int) *int {
	if v == nil {
		return nil
	}
	days := *v
	if days < 1 {
		days = 1
	}
	if days > MaxActivityRetentionDays {
		days = MaxActivityRetentionDays
	}
	return &days
}

// resolveActivityRetentionDays returns the *effective* retention horizon
// for a fund: the configured value if valid, otherwise the default. The
// retention cron and the Fund→API mapper both call this so they agree
// on what "missing config" means.
func resolveActivityRetentionDays(profile fundMarketProfile) int {
	normalized := normalizeActivityRetentionDays(profile.ActivityRetentionDays)
	if normalized != nil {
		return *normalized
	}
	return DefaultActivityRetentionDays
}

func decodeFundMarketProfile(raw json.RawMessage) fundMarketProfile {
	if len(raw) == 0 || string(raw) == "null" {
		return normalizeFundMarketProfile(fundMarketProfile{})
	}
	var profile fundMarketProfile
	if err := json.Unmarshal(raw, &profile); err != nil {
		return normalizeFundMarketProfile(fundMarketProfile{})
	}
	profile.TeamIntervals = normalizeFundTeamIntervals(profile.TeamIntervals)
	profile.Specialization = normalizeFundSpecialization(profile.Specialization)
	profile.HardRisk = normalizeFundHardRisk(profile.HardRisk)
	profile.AutoExecute = normalizeFundAutoExecute(profile.AutoExecute)
	profile.ResearchTier = normalizeFundResearchTier(profile.ResearchTier)
	return normalizeFundMarketProfile(profile)
}

// decodeFundMarketProfileForBuild is the variant used by buildFundConfigJSON
// when merging caller overrides onto an existing config. Unlike
// decodeFundMarketProfile it skips the initial normalize so the empty-profile
// case does NOT pre-stamp the catch-all `US-XNAS / America/New_York`
// defaults. If we pre-stamped, a subsequent override of `market` (e.g. to
// "a_share") would then be ignored by normalize because calendarCode is
// already non-empty, and every newly-created A-share/crypto/futures fund
// would end up with the US calendar regardless of the requested market.
func decodeFundMarketProfileForBuild(raw json.RawMessage) fundMarketProfile {
	if len(raw) == 0 || string(raw) == "null" {
		return fundMarketProfile{}
	}
	var profile fundMarketProfile
	if err := json.Unmarshal(raw, &profile); err != nil {
		return fundMarketProfile{}
	}
	profile.TeamIntervals = normalizeFundTeamIntervals(profile.TeamIntervals)
	profile.Specialization = normalizeFundSpecialization(profile.Specialization)
	profile.HardRisk = normalizeFundHardRisk(profile.HardRisk)
	profile.AutoExecute = normalizeFundAutoExecute(profile.AutoExecute)
	profile.ResearchTier = normalizeFundResearchTier(profile.ResearchTier)
	return profile
}

// normalizeFundResearchTier coerces stored tier strings to the
// canonical set we route on. Anything other than "advanced" is
// collapsed to "standard" so a typo or stale value never silently
// enables the more expensive debate path.
func normalizeFundResearchTier(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "advanced":
		return "advanced"
	default:
		return "standard"
	}
}

func normalizeFundMarketProfile(profile fundMarketProfile) fundMarketProfile {
	calendarProfile, err := marketcalendar.NewService().NormalizeProfile(marketcalendar.Profile{
		Market:       profile.Market,
		Exchange:     profile.Exchange,
		AssetClass:   profile.AssetClass,
		CalendarCode: profile.CalendarCode,
		TimeZone:     profile.TimeZone,
	})
	if err != nil {
		return profile
	}
	profile.Market = calendarProfile.Market
	profile.Exchange = calendarProfile.Exchange
	profile.AssetClass = calendarProfile.AssetClass
	profile.CalendarCode = calendarProfile.CalendarCode
	profile.TimeZone = calendarProfile.TimeZone
	profile.Universe = sanitizeFundUniverse(profile.Universe)
	return profile
}

// sanitizeFundUniverse strips whitespace-only entries, trims surrounding
// spaces, and case-insensitively dedupes symbols / sectors / themes /
// customFilters. Operators frequently paste lists that contain empty
// rows, mixed-case tickers, or duplicated entries; without sanitisation
// the noise leaks into every downstream consumer — quote fetches loop
// over `""`, the LLM prompt repeats `AAPL` three times, etc.
//
// Symbols are also uppercased to match the convention used everywhere
// else in the codebase (instrument hints, plan_actions.symbol,
// normalizedWorkflowSymbol). Sectors / themes / filters are left in
// their original casing — they're free-text labels and capitalising
// "Tech" would surprise a human reader; case-insensitive dedup is
// enough to remove "Tech" vs "tech" duplicates.
//
// A hard cap of 500 entries per field protects downstream loops from
// pathological universes. Real funds in production sit at <50 symbols.
func sanitizeFundUniverse(u *api.FundUniverse) *api.FundUniverse {
	if u == nil {
		return nil
	}
	out := *u
	out.Mode = strings.TrimSpace(out.Mode)
	out.Symbols = sanitizeUniverseList(out.Symbols, true)
	out.Sectors = sanitizeUniverseList(out.Sectors, false)
	out.Themes = sanitizeUniverseList(out.Themes, false)
	out.CustomFilters = sanitizeUniverseList(out.CustomFilters, false)
	return &out
}

func sanitizeUniverseList(values []string, upper bool) []string {
	if len(values) == 0 {
		return nil
	}
	const maxEntries = 500
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if upper {
			trimmed = strings.ToUpper(trimmed)
		}
		result = append(result, trimmed)
		if len(result) >= maxEntries {
			break
		}
	}
	return result
}

// defaultBenchmarkForProfile returns the canonical broad benchmark
// for the fund's market when the caller didn't provide one. Empty
// string when we can't make a sensible default — preserving the
// legacy "no benchmark" behaviour so PnLAttribution / risk-attribution
// code paths don't get surprised by a value the user never opted into.
//
// Important: this is intentionally invoked ONLY from buildFundConfigJSON
// (the create / update entry point) and NOT from
// normalizeFundMarketProfile (the every-read path). We only want to
// stamp a default into the persisted config when the user actively
// configures the fund — legacy funds that were created before this
// helper existed keep their explicit empty benchmark on every read so
// downstream code (e.g. news digest, attribution) sees the same shape
// it always saw and doesn't silently start probing SPY on legacy rows.
//
// The mapping targets liquid, free-to-quote tickers that every
// market-data provider in the platform supports:
//   - a_share  → 000300.SS (CSI 300, the broad onshore equity index)
//   - us_equity → SPY      (S&P 500 ETF, the de-facto US benchmark)
//   - crypto    → BTC-USD  (single-asset proxy for the crypto basket)
//   - futures   → ES=F     (E-mini S&P 500 continuous front-month)
func defaultBenchmarkForProfile(profile fundMarketProfile) string {
	if existing := strings.TrimSpace(profile.BenchmarkSymbol); existing != "" {
		return existing
	}
	switch strings.ToLower(strings.TrimSpace(profile.Market)) {
	case "a_share":
		return "000300.SS"
	case "us_equity":
		return "SPY"
	case "crypto":
		return "BTC-USD"
	case "futures":
		return "ES=F"
	}
	return ""
}

func cloneStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}

func cloneFundSpecialization(specialization *api.FundSpecialization) *api.FundSpecialization {
	if specialization == nil || specialization.Team == nil {
		return nil
	}
	return &api.FundSpecialization{Team: &api.FundTeamSpecialization{
		Markets:      cloneStringSlice(specialization.Team.Markets),
		AssetClasses: cloneStringSlice(specialization.Team.AssetClasses),
		Themes:       cloneStringSlice(specialization.Team.Themes),
		Instruments:  cloneStringSlice(specialization.Team.Instruments),
		StyleHints:   cloneStringSlice(specialization.Team.StyleHints),
	}}
}

func buildFundConfigJSON(cfg api.FundConfig, existing json.RawMessage) (json.RawMessage, error) {
	profile := decodeFundMarketProfileForBuild(existing)
	if cfg.Market != nil {
		profile.Market = strings.TrimSpace(*cfg.Market)
	}
	if cfg.Exchange != nil {
		profile.Exchange = strings.TrimSpace(*cfg.Exchange)
	}
	if cfg.AssetClass != nil {
		profile.AssetClass = strings.TrimSpace(*cfg.AssetClass)
	}
	if cfg.BaseCurrency != nil {
		profile.BaseCurrency = strings.TrimSpace(*cfg.BaseCurrency)
	}
	if cfg.BenchmarkSymbol != nil {
		profile.BenchmarkSymbol = strings.TrimSpace(*cfg.BenchmarkSymbol)
	}
	if cfg.PrimaryDirection != nil {
		profile.PrimaryDirection = strings.TrimSpace(*cfg.PrimaryDirection)
	}
	if cfg.CalendarCode != nil {
		profile.CalendarCode = strings.TrimSpace(*cfg.CalendarCode)
	}
	if cfg.TimeZone != nil {
		profile.TimeZone = strings.TrimSpace(*cfg.TimeZone)
	}
	if cfg.Universe != nil {
		copied := *cfg.Universe
		copied.Symbols = cloneStringSlice(cfg.Universe.Symbols)
		copied.Sectors = cloneStringSlice(cfg.Universe.Sectors)
		copied.Themes = cloneStringSlice(cfg.Universe.Themes)
		copied.CustomFilters = cloneStringSlice(cfg.Universe.CustomFilters)
		profile.Universe = &copied
	}
	if cfg.TeamIntervals != nil {
		profile.TeamIntervals = mergeFundTeamIntervals(profile.TeamIntervals, cfg.TeamIntervals)
	}
	if cfg.Specialization != nil {
		profile.Specialization = mergeFundSpecialization(profile.Specialization, cfg.Specialization)
	}
	if cfg.HardRisk != nil {
		profile.HardRisk = mergeFundHardRisk(profile.HardRisk, cfg.HardRisk)
	}
	if cfg.AutoExecute != nil {
		profile.AutoExecute = mergeFundAutoExecute(profile.AutoExecute, cfg.AutoExecute)
	}
	if cfg.ResearchTier != nil {
		profile.ResearchTier = normalizeFundResearchTier(*cfg.ResearchTier)
	}
	if cfg.ActivityRetentionDays != nil {
		profile.ActivityRetentionDays = normalizeActivityRetentionDays(cfg.ActivityRetentionDays)
	}
	profile = normalizeFundMarketProfile(profile)
	// Apply benchmark default AFTER calendar normalization so that
	// profile.Market is in its canonical form ("us_equity" not
	// "us_stock"). Only applied at create/update time — see the
	// comment on defaultBenchmarkForProfile for why we don't do
	// this on every read.
	profile.BenchmarkSymbol = defaultBenchmarkForProfile(profile)
	encoded, err := json.Marshal(profile)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func normalizeFundTeamIntervals(intervals *api.FundTeamIntervals) *api.FundTeamIntervals {
	if intervals == nil {
		return nil
	}
	normalized := &api.FundTeamIntervals{
		PM:         normalizeFundTeamInterval(intervals.PM),
		Researcher: normalizeFundTeamInterval(intervals.Researcher),
		Trader:     normalizeFundTeamInterval(intervals.Trader),
		Risk:       normalizeFundTeamInterval(intervals.Risk),
	}
	if normalized.PM == nil && normalized.Researcher == nil && normalized.Trader == nil && normalized.Risk == nil {
		return nil
	}
	return normalized
}

func mergeFundTeamIntervals(existing, patch *api.FundTeamIntervals) *api.FundTeamIntervals {
	merged := normalizeFundTeamIntervals(existing)
	if merged == nil {
		merged = &api.FundTeamIntervals{}
	}
	if patch.PM != nil {
		merged.PM = normalizeFundTeamInterval(patch.PM)
	}
	if patch.Researcher != nil {
		merged.Researcher = normalizeFundTeamInterval(patch.Researcher)
	}
	if patch.Trader != nil {
		merged.Trader = normalizeFundTeamInterval(patch.Trader)
	}
	if patch.Risk != nil {
		merged.Risk = normalizeFundTeamInterval(patch.Risk)
	}
	if merged.PM == nil && merged.Researcher == nil && merged.Trader == nil && merged.Risk == nil {
		return nil
	}
	return merged
}

func normalizeFundSpecialization(specialization *api.FundSpecialization) *api.FundSpecialization {
	if specialization == nil || specialization.Team == nil {
		return nil
	}
	normalizedTeam := &api.FundTeamSpecialization{
		Markets:      normalizedStringSlice(specialization.Team.Markets),
		AssetClasses: normalizedStringSlice(specialization.Team.AssetClasses),
		Themes:       normalizedStringSlice(specialization.Team.Themes),
		Instruments:  normalizedStringSlice(specialization.Team.Instruments),
		StyleHints:   normalizedStringSlice(specialization.Team.StyleHints),
	}
	if len(normalizedTeam.Markets) == 0 && len(normalizedTeam.AssetClasses) == 0 && len(normalizedTeam.Themes) == 0 && len(normalizedTeam.Instruments) == 0 && len(normalizedTeam.StyleHints) == 0 {
		return nil
	}
	return &api.FundSpecialization{Team: normalizedTeam}
}

func mergeFundSpecialization(existing, patch *api.FundSpecialization) *api.FundSpecialization {
	merged := normalizeFundSpecialization(existing)
	if merged == nil {
		merged = &api.FundSpecialization{Team: &api.FundTeamSpecialization{}}
	}
	if patch == nil {
		return normalizeFundSpecialization(merged)
	}
	if patch.Team != nil {
		if patch.Team.Markets != nil {
			merged.Team.Markets = normalizedStringSlice(patch.Team.Markets)
		}
		if patch.Team.AssetClasses != nil {
			merged.Team.AssetClasses = normalizedStringSlice(patch.Team.AssetClasses)
		}
		if patch.Team.Themes != nil {
			merged.Team.Themes = normalizedStringSlice(patch.Team.Themes)
		}
		if patch.Team.Instruments != nil {
			merged.Team.Instruments = normalizedStringSlice(patch.Team.Instruments)
		}
		if patch.Team.StyleHints != nil {
			merged.Team.StyleHints = normalizedStringSlice(patch.Team.StyleHints)
		}
	}
	return normalizeFundSpecialization(merged)
}

func normalizeFundHardRisk(cfg *api.FundHardRiskConfig) *api.FundHardRiskConfig {
	if cfg == nil {
		return nil
	}
	normalized := &api.FundHardRiskConfig{}
	if cfg.DailyLossLimit != nil {
		normalized.DailyLossLimit = normalizedRiskFloatPtr(*cfg.DailyLossLimit, 0, 0.50)
	}
	if cfg.MaxSinglePosition != nil {
		normalized.MaxSinglePosition = normalizedRiskFloatPtr(*cfg.MaxSinglePosition, 0, 1)
	}
	if cfg.MaxSectorExposure != nil {
		normalized.MaxSectorExposure = normalizedRiskFloatPtr(*cfg.MaxSectorExposure, 0, 1)
	}
	if cfg.MaxTotalExposure != nil {
		normalized.MaxTotalExposure = normalizedRiskFloatPtr(*cfg.MaxTotalExposure, 0, 1.5)
	}
	if cfg.MaxOrderPctOfAssets != nil {
		normalized.MaxOrderPctOfAssets = normalizedRiskFloatPtr(*cfg.MaxOrderPctOfAssets, 0, 1)
	}
	if cfg.MaxOrderAmount != nil && *cfg.MaxOrderAmount > 0 {
		value := *cfg.MaxOrderAmount
		normalized.MaxOrderAmount = &value
	}
	if cfg.MaxTradesPerDay != nil && *cfg.MaxTradesPerDay > 0 && *cfg.MaxTradesPerDay <= 10000 {
		value := *cfg.MaxTradesPerDay
		normalized.MaxTradesPerDay = &value
	}
	if cfg.MaxTradesPerSymbolDay != nil && *cfg.MaxTradesPerSymbolDay > 0 && *cfg.MaxTradesPerSymbolDay <= 10000 {
		value := *cfg.MaxTradesPerSymbolDay
		normalized.MaxTradesPerSymbolDay = &value
	}
	if cfg.MaxQuoteAgeSeconds != nil && *cfg.MaxQuoteAgeSeconds > 0 && *cfg.MaxQuoteAgeSeconds <= 86400 {
		value := *cfg.MaxQuoteAgeSeconds
		normalized.MaxQuoteAgeSeconds = &value
	}
	if normalized.DailyLossLimit == nil && normalized.MaxSinglePosition == nil && normalized.MaxSectorExposure == nil && normalized.MaxTotalExposure == nil && normalized.MaxOrderPctOfAssets == nil && normalized.MaxOrderAmount == nil && normalized.MaxTradesPerDay == nil && normalized.MaxTradesPerSymbolDay == nil && normalized.MaxQuoteAgeSeconds == nil {
		return nil
	}
	return normalized
}

func mergeFundHardRisk(existing, patch *api.FundHardRiskConfig) *api.FundHardRiskConfig {
	merged := normalizeFundHardRisk(existing)
	if merged == nil {
		merged = &api.FundHardRiskConfig{}
	}
	if patch == nil {
		return normalizeFundHardRisk(merged)
	}
	// Important: every patch field is range-validated BEFORE overwriting
	// the existing value. Without this gate, an out-of-range PATCH would
	// store a junk pointer that the final normalizeFundHardRisk call
	// silently drops (returns nil), surfacing as "user raised the cap
	// to 0.99 → field disappears → default 0.05 takes over" — a silent
	// guardrail RELAXATION caught by P2 sweep Test 4. Preserving the
	// previously-valid value is safer: the user's most recent valid
	// intent stays in force rather than reverting to a permissive
	// platform default.
	if patch.DailyLossLimit != nil {
		if v := normalizedRiskFloatPtr(*patch.DailyLossLimit, 0, 0.50); v != nil {
			merged.DailyLossLimit = v
		}
	}
	if patch.MaxSinglePosition != nil {
		if v := normalizedRiskFloatPtr(*patch.MaxSinglePosition, 0, 1); v != nil {
			merged.MaxSinglePosition = v
		}
	}
	if patch.MaxSectorExposure != nil {
		if v := normalizedRiskFloatPtr(*patch.MaxSectorExposure, 0, 1); v != nil {
			merged.MaxSectorExposure = v
		}
	}
	if patch.MaxTotalExposure != nil {
		if v := normalizedRiskFloatPtr(*patch.MaxTotalExposure, 0, 1.5); v != nil {
			merged.MaxTotalExposure = v
		}
	}
	if patch.MaxOrderPctOfAssets != nil {
		if v := normalizedRiskFloatPtr(*patch.MaxOrderPctOfAssets, 0, 1); v != nil {
			merged.MaxOrderPctOfAssets = v
		}
	}
	// Int caps use 0 as a "clear this override" sentinel — keep that
	// behaviour for compatibility (see TestMergeFundHardRiskClearsViaZeroSentinel).
	// Out-of-range positives preserve the existing value rather than
	// clobbering it, matching the float pattern above.
	if patch.MaxOrderAmount != nil {
		switch {
		case *patch.MaxOrderAmount == 0:
			merged.MaxOrderAmount = nil
		case *patch.MaxOrderAmount > 0:
			v := *patch.MaxOrderAmount
			merged.MaxOrderAmount = &v
		}
	}
	if patch.MaxTradesPerDay != nil {
		switch {
		case *patch.MaxTradesPerDay == 0:
			merged.MaxTradesPerDay = nil
		case *patch.MaxTradesPerDay > 0 && *patch.MaxTradesPerDay <= 10000:
			v := *patch.MaxTradesPerDay
			merged.MaxTradesPerDay = &v
		}
	}
	if patch.MaxTradesPerSymbolDay != nil {
		switch {
		case *patch.MaxTradesPerSymbolDay == 0:
			merged.MaxTradesPerSymbolDay = nil
		case *patch.MaxTradesPerSymbolDay > 0 && *patch.MaxTradesPerSymbolDay <= 10000:
			v := *patch.MaxTradesPerSymbolDay
			merged.MaxTradesPerSymbolDay = &v
		}
	}
	if patch.MaxQuoteAgeSeconds != nil {
		switch {
		case *patch.MaxQuoteAgeSeconds == 0:
			merged.MaxQuoteAgeSeconds = nil
		case *patch.MaxQuoteAgeSeconds > 0 && *patch.MaxQuoteAgeSeconds <= 86400:
			v := *patch.MaxQuoteAgeSeconds
			merged.MaxQuoteAgeSeconds = &v
		}
	}
	return normalizeFundHardRisk(merged)
}

func normalizedRiskFloatPtr(value, minExclusive, maxInclusive float64) *float64 {
	if value <= minExclusive || value > maxInclusive || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	return &value
}

// Default guardrails applied to a fund that has auto-execute enabled
// but didn't pin a specific threshold (or pinned an invalid one). The
// thresholds match the human-facing copy in the web/miniapp settings
// modal — change one, change the other.
const (
	DefaultAutoExecuteMaxOrderPctOfAssets = 0.05 // 5% NAV per order
	DefaultAutoExecuteMaxDailyPctOfAssets = 0.20 // 20% NAV cumulative per trading day
	DefaultAutoExecuteMinConfidence       = 0.60 // plan-level LLM confidence floor
	DefaultAutoExecuteSlippageBouncePolicy = "bounce_to_user"
)

// validAutoExecuteSlippagePolicies enumerates the policies the gateway
// understands at execution time. Any value outside this set gets
// rewritten to the default during normalization so the gateway never
// has to defend against typos / older client versions.
var validAutoExecuteSlippagePolicies = map[string]struct{}{
	"bounce_to_user":  {},
	"reject":          {},
	"force_execute":   {},
}

// normalizeFundAutoExecute clamps user-supplied guardrails into the
// supported ranges and rewrites bad enum values to the default policy.
// Returns nil only when the caller passes nil — an explicit "disabled"
// config (Enabled=false) is preserved so the web client can distinguish
// "not configured yet" from "user has explicitly turned it off".
func normalizeFundAutoExecute(cfg *api.FundAutoExecuteConfig) *api.FundAutoExecuteConfig {
	if cfg == nil {
		return nil
	}
	normalized := &api.FundAutoExecuteConfig{
		Enabled: cfg.Enabled,
	}
	if cfg.MaxOrderPctOfAssets != nil {
		normalized.MaxOrderPctOfAssets = normalizedRiskFloatPtr(*cfg.MaxOrderPctOfAssets, 0, 1)
	}
	if cfg.MaxDailyPctOfAssets != nil {
		normalized.MaxDailyPctOfAssets = normalizedRiskFloatPtr(*cfg.MaxDailyPctOfAssets, 0, 1)
	}
	if cfg.MinConfidence != nil {
		// confidence is a 0..1 probability; clamp inclusively above 0
		// (a "0 or below" floor is a misconfiguration — fall back to
		// default rather than letting low-quality plans through).
		normalized.MinConfidence = normalizedRiskFloatPtr(*cfg.MinConfidence, 0, 1)
	}
	policy := strings.TrimSpace(cfg.SlippageBouncePolicy)
	if _, ok := validAutoExecuteSlippagePolicies[policy]; ok && policy != "" {
		normalized.SlippageBouncePolicy = policy
	}
	if len(cfg.AllowedMarkets) > 0 {
		seen := map[string]struct{}{}
		filtered := make([]string, 0, len(cfg.AllowedMarkets))
		for _, m := range cfg.AllowedMarkets {
			m = strings.ToLower(strings.TrimSpace(m))
			if m == "" {
				continue
			}
			if _, dup := seen[m]; dup {
				continue
			}
			seen[m] = struct{}{}
			filtered = append(filtered, m)
		}
		if len(filtered) > 0 {
			normalized.AllowedMarkets = filtered
		}
	}
	if cfg.DecisionIntervalMinutes != nil {
		// Non-positive value = "interval mode disabled". Drop the
		// field so the calendar/scheduler treats it as nil and falls
		// back to the legacy one-shot daily trigger.
		if *cfg.DecisionIntervalMinutes > 0 {
			clamped := clampDecisionIntervalMinutes(*cfg.DecisionIntervalMinutes)
			normalized.DecisionIntervalMinutes = &clamped
		}
	}
	return normalized
}

// clampDecisionIntervalMinutes folds the per-fund interval into the
// supported envelope before we hand it to the calendar layer. Pinned
// to marketcalendar.{Min,Max}DecisionIntervalMinutes so the two layers
// agree on what counts as "absurd input".
func clampDecisionIntervalMinutes(v int) int {
	if v < marketcalendar.MinDecisionIntervalMinutes {
		return marketcalendar.MinDecisionIntervalMinutes
	}
	if v > marketcalendar.MaxDecisionIntervalMinutes {
		return marketcalendar.MaxDecisionIntervalMinutes
	}
	return v
}

// mergeFundAutoExecute applies a PATCH-style merge: nil patch fields
// preserve existing values. The Enabled flag is the one exception —
// PATCH always overwrites it, which matches the UX (the Toggle is the
// single source of truth, settings live behind the gear).
func mergeFundAutoExecute(existing, patch *api.FundAutoExecuteConfig) *api.FundAutoExecuteConfig {
	merged := normalizeFundAutoExecute(existing)
	if merged == nil {
		merged = &api.FundAutoExecuteConfig{}
	}
	if patch == nil {
		return normalizeFundAutoExecute(merged)
	}
	merged.Enabled = patch.Enabled
	// Same pre-merge validation as mergeFundHardRisk: out-of-range
	// patches must NOT clobber a previously-valid cap, otherwise we
	// silently relax the auto-execute floor (P2 sweep Test 5: PATCH
	// minConfidence:1.5 → field cleared → default 0.6 applied → more
	// permissive auto-execute than the user originally configured).
	if patch.MaxOrderPctOfAssets != nil {
		if v := normalizedRiskFloatPtr(*patch.MaxOrderPctOfAssets, 0, 1); v != nil {
			merged.MaxOrderPctOfAssets = v
		}
	}
	if patch.MaxDailyPctOfAssets != nil {
		if v := normalizedRiskFloatPtr(*patch.MaxDailyPctOfAssets, 0, 1); v != nil {
			merged.MaxDailyPctOfAssets = v
		}
	}
	if patch.MinConfidence != nil {
		if v := normalizedRiskFloatPtr(*patch.MinConfidence, 0, 1); v != nil {
			merged.MinConfidence = v
		}
	}
	// Reject unknown slippage policies. An unknown policy string would
	// be silently dropped by normalizeFundAutoExecute, leaving merged
	// with the default "bounce_to_user" — but if the user already had
	// a stricter policy set, we'd silently downgrade them.
	if strings.TrimSpace(patch.SlippageBouncePolicy) != "" {
		if _, ok := validAutoExecuteSlippagePolicies[strings.TrimSpace(patch.SlippageBouncePolicy)]; ok {
			merged.SlippageBouncePolicy = strings.TrimSpace(patch.SlippageBouncePolicy)
		}
	}
	if patch.AllowedMarkets != nil {
		// nil means "leave alone"; an empty slice means "reset to no
		// whitelist". Distinguishing the two is important because
		// frontend toggle clears can pass [] explicitly.
		merged.AllowedMarkets = append([]string(nil), patch.AllowedMarkets...)
	}
	if patch.DecisionIntervalMinutes != nil {
		v := *patch.DecisionIntervalMinutes
		// 0 (or any non-positive value) is a sentinel for "revert to
		// one-shot daily mode" — the JSON wire format can't express a
		// distinct "set to null" via a *int with omitempty, so a
		// positive number turns the loop on, zero turns it off.
		if v <= 0 {
			merged.DecisionIntervalMinutes = nil
		} else {
			merged.DecisionIntervalMinutes = &v
		}
	}
	return normalizeFundAutoExecute(merged)
}

func cloneFundAutoExecute(cfg *api.FundAutoExecuteConfig) *api.FundAutoExecuteConfig {
	if cfg == nil {
		return nil
	}
	clone := &api.FundAutoExecuteConfig{
		Enabled:              cfg.Enabled,
		SlippageBouncePolicy: cfg.SlippageBouncePolicy,
	}
	if cfg.MaxOrderPctOfAssets != nil {
		v := *cfg.MaxOrderPctOfAssets
		clone.MaxOrderPctOfAssets = &v
	}
	if cfg.MaxDailyPctOfAssets != nil {
		v := *cfg.MaxDailyPctOfAssets
		clone.MaxDailyPctOfAssets = &v
	}
	if cfg.MinConfidence != nil {
		v := *cfg.MinConfidence
		clone.MinConfidence = &v
	}
	if len(cfg.AllowedMarkets) > 0 {
		clone.AllowedMarkets = append([]string(nil), cfg.AllowedMarkets...)
	}
	if cfg.DecisionIntervalMinutes != nil {
		v := *cfg.DecisionIntervalMinutes
		clone.DecisionIntervalMinutes = &v
	}
	return clone
}

// resolveAutoExecuteConfig returns a *non-nil* config with every field
// resolved (defaults filled in) for runtime use. Callers should still
// check cfg.Enabled before letting anything bypass approval. We never
// return nil here so the gate code can safely deref guardrail fields.
func resolveAutoExecuteConfig(cfg *api.FundAutoExecuteConfig) api.FundAutoExecuteConfig {
	resolved := api.FundAutoExecuteConfig{
		SlippageBouncePolicy: DefaultAutoExecuteSlippageBouncePolicy,
	}
	if cfg != nil {
		resolved = *cloneFundAutoExecute(cfg)
	}
	if resolved.MaxOrderPctOfAssets == nil {
		v := DefaultAutoExecuteMaxOrderPctOfAssets
		resolved.MaxOrderPctOfAssets = &v
	}
	if resolved.MaxDailyPctOfAssets == nil {
		v := DefaultAutoExecuteMaxDailyPctOfAssets
		resolved.MaxDailyPctOfAssets = &v
	}
	if resolved.MinConfidence == nil {
		v := DefaultAutoExecuteMinConfidence
		resolved.MinConfidence = &v
	}
	if strings.TrimSpace(resolved.SlippageBouncePolicy) == "" {
		resolved.SlippageBouncePolicy = DefaultAutoExecuteSlippageBouncePolicy
	}
	return resolved
}

func riskHardConfigFromAPI(cfg *api.FundHardRiskConfig) risk.HardRiskConfig {
	if cfg == nil {
		return risk.DefaultHardRiskConfig()
	}
	normalized := normalizeFundHardRisk(cfg)
	result := risk.DefaultHardRiskConfig()
	if normalized == nil {
		return result
	}
	if normalized.DailyLossLimit != nil {
		result.DailyLossLimit = *normalized.DailyLossLimit
	}
	if normalized.MaxSinglePosition != nil {
		result.MaxSinglePosition = *normalized.MaxSinglePosition
	}
	if normalized.MaxSectorExposure != nil {
		result.MaxSectorExposure = *normalized.MaxSectorExposure
	}
	if normalized.MaxTotalExposure != nil {
		result.MaxTotalExposure = *normalized.MaxTotalExposure
	}
	if normalized.MaxOrderPctOfAssets != nil {
		result.MaxOrderPctOfAssets = *normalized.MaxOrderPctOfAssets
	}
	if normalized.MaxOrderAmount != nil {
		result.MaxOrderAmount = *normalized.MaxOrderAmount
	}
	if normalized.MaxTradesPerDay != nil {
		result.MaxTradesPerDay = *normalized.MaxTradesPerDay
	}
	if normalized.MaxTradesPerSymbolDay != nil {
		result.MaxTradesPerSymbolDay = *normalized.MaxTradesPerSymbolDay
	}
	if normalized.MaxQuoteAgeSeconds != nil {
		result.MaxQuoteAge = time.Duration(*normalized.MaxQuoteAgeSeconds) * time.Second
	}
	return result
}

func normalizeFundTeamInterval(value *int) *int {
	if value == nil {
		return nil
	}
	minutes := *value
	if minutes <= 0 {
		return nil
	}
	normalized := int(math.Round(float64(minutes)/5.0)) * 5
	if normalized < 5 {
		normalized = 5
	}
	if normalized > 1440 {
		normalized = 1440
	}
	return &normalized
}

func optionalString(value string) *string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func firstNonEmptyValue(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func buildInstrumentKey(exchange, symbol string) string {
	trimmedSymbol := strings.ToUpper(strings.TrimSpace(symbol))
	trimmedExchange := strings.ToUpper(strings.TrimSpace(exchange))
	if trimmedExchange == "" {
		return trimmedSymbol
	}
	if trimmedSymbol == "" {
		return trimmedExchange
	}
	return trimmedExchange + ":" + trimmedSymbol
}

func buildMarketQueryInstruments(fund *repository.Fund, profile fundMarketProfile, symbols []string) []marketdata.InstrumentRef {
	if len(symbols) == 0 {
		instruments := profileUniverseInstruments(profile)
		if len(instruments) > 0 {
			return instruments
		}
		if instrument, ok := benchmarkInstrumentRef(profile); ok {
			return []marketdata.InstrumentRef{instrument}
		}
		if fund != nil {
			return []marketdata.InstrumentRef{defaultInstrumentRef(fund, workflow.FocusStock, inferWorkflowSymbol(fund, nil))}
		}
		return nil
	}
	result := make([]marketdata.InstrumentRef, 0, len(symbols))
	for _, symbol := range normalizedStringSlice(symbols) {
		instrument := marketQueryInstrument(fund, symbol)
		if strings.TrimSpace(instrument.Symbol) == "" {
			continue
		}
		result = append(result, instrument)
	}
	return result
}

func (s *marketServiceAdapter) digestTeamSpecialization(ctx context.Context, fundID string) *agentSpecialization {
	if s == nil || s.teamRepo == nil || s.agentRepo == nil {
		return nil
	}
	members, err := s.teamRepo.ListByFund(ctx, fundID)
	if err != nil {
		return nil
	}
	merged := &agentSpecialization{}
	for i := range members {
		member := members[i]
		if strings.TrimSpace(member.Status) != "" && !strings.EqualFold(member.Status, "active") {
			continue
		}
		agent, err := s.agentRepo.GetByID(ctx, member.AgentID)
		if err != nil {
			continue
		}
		specialization := extractAgentSpecialization(agent)
		if specialization == nil {
			continue
		}
		merged.Markets = append(merged.Markets, specialization.Markets...)
		merged.AssetClasses = append(merged.AssetClasses, specialization.AssetClasses...)
		merged.Themes = append(merged.Themes, specialization.Themes...)
		merged.Instruments = append(merged.Instruments, specialization.Instruments...)
		merged.StyleHints = append(merged.StyleHints, specialization.StyleHints...)
		merged.Patterns = append(merged.Patterns, specialization.Patterns...)
	}
	return normalizeAgentSpecialization(merged)
}

func buildHybridMarketNewsQueries(fund *repository.Fund, profile fundMarketProfile, symbols []string, teamSpecialization *agentSpecialization) []marketdata.InstrumentRef {
	const maxTickerQueries = 3
	const maxContextQueries = 3

	tickerQueries := make([]marketdata.InstrumentRef, 0, maxTickerQueries)
	seenTicker := make(map[string]struct{}, maxTickerQueries)
	appendTicker := func(instrument marketdata.InstrumentRef) {
		if strings.TrimSpace(instrument.Symbol) == "" {
			return
		}
		if !marketdata.IsTickerLikeSymbol(instrument.Symbol) {
			return
		}
		key := instrument.CacheKey()
		if _, ok := seenTicker[key]; ok {
			return
		}
		seenTicker[key] = struct{}{}
		tickerQueries = append(tickerQueries, instrument)
	}
	for _, instrument := range buildMarketQueryInstruments(fund, profile, symbols) {
		appendTicker(instrument)
		if len(tickerQueries) >= maxTickerQueries {
			break
		}
	}
	if len(tickerQueries) < maxTickerQueries {
		for _, instrument := range profileUniverseInstruments(profile) {
			appendTicker(instrument)
			if len(tickerQueries) >= maxTickerQueries {
				break
			}
		}
	}
	if len(tickerQueries) < maxTickerQueries {
		if instrument, ok := benchmarkInstrumentRef(profile); ok {
			appendTicker(instrument)
		}
	}
	if len(tickerQueries) < maxTickerQueries && profile.Specialization != nil && profile.Specialization.Team != nil {
		for _, symbol := range normalizedStringSlice(profile.Specialization.Team.Instruments) {
			resolved := resolvedWorkflowSymbolCandidate(symbol)
			if resolved == "" {
				resolved = strings.ToUpper(strings.TrimSpace(symbol))
			}
			appendTicker(marketQueryInstrument(fund, resolved))
			if len(tickerQueries) >= maxTickerQueries {
				break
			}
		}
	}
	if len(tickerQueries) < maxTickerQueries && teamSpecialization != nil {
		for _, symbol := range normalizedStringSlice(teamSpecialization.Instruments) {
			resolved := resolvedWorkflowSymbolCandidate(symbol)
			if resolved == "" {
				resolved = strings.ToUpper(strings.TrimSpace(symbol))
			}
			appendTicker(marketQueryInstrument(fund, resolved))
			if len(tickerQueries) >= maxTickerQueries {
				break
			}
		}
	}

	contextQueries := make([]marketdata.InstrumentRef, 0, maxContextQueries)
	seenContext := make(map[string]struct{}, maxContextQueries)
	appendContext := func(query string) {
		query = strings.TrimSpace(query)
		if query == "" {
			return
		}
		instrument := marketNewsQueryRef(fund, profile, query)
		if strings.TrimSpace(instrument.Symbol) == "" {
			return
		}
		key := strings.ToLower(strings.TrimSpace(instrument.Symbol)) + "|" + strings.ToLower(strings.TrimSpace(instrument.Market)) + "|" + strings.ToLower(strings.TrimSpace(instrument.AssetClass))
		if _, ok := seenContext[key]; ok {
			return
		}
		seenContext[key] = struct{}{}
		contextQueries = append(contextQueries, instrument)
	}
	for _, query := range fundContextNewsQueries(fund, profile, teamSpecialization) {
		appendContext(query)
		if len(contextQueries) >= maxContextQueries {
			break
		}
	}

	result := make([]marketdata.InstrumentRef, 0, len(tickerQueries)+len(contextQueries))
	result = append(result, tickerQueries...)
	result = append(result, contextQueries...)
	return result
}

func fundContextNewsQueries(fund *repository.Fund, profile fundMarketProfile, teamSpecialization *agentSpecialization) []string {
	queries := make([]string, 0, 12)
	marketLabel := fundMarketNewsLabel(profile)
	appendQuery := func(value, suffix string) {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return
		}
		if suffix == "" {
			queries = append(queries, trimmed)
			return
		}
		queries = append(queries, trimmed+" "+suffix)
	}
	if profile.Universe != nil {
		for _, theme := range normalizedStringSlice(profile.Universe.Themes) {
			appendQuery(theme, marketLabel)
		}
		for _, sector := range normalizedStringSlice(profile.Universe.Sectors) {
			appendQuery(sector, marketLabel)
		}
	}
	if profile.Specialization != nil && profile.Specialization.Team != nil {
		for _, theme := range normalizedStringSlice(profile.Specialization.Team.Themes) {
			appendQuery(theme, marketLabel)
		}
		for _, hint := range normalizedStringSlice(profile.Specialization.Team.StyleHints) {
			appendQuery(hint, marketLabel)
		}
		for _, market := range normalizedStringSlice(profile.Specialization.Team.Markets) {
			appendQuery(market, "market news")
		}
		for _, assetClass := range normalizedStringSlice(profile.Specialization.Team.AssetClasses) {
			appendQuery(assetClass, "market news")
		}
	}
	if teamSpecialization != nil {
		for _, theme := range normalizedStringSlice(teamSpecialization.Themes) {
			appendQuery(theme, marketLabel)
		}
		for _, hint := range normalizedStringSlice(teamSpecialization.StyleHints) {
			appendQuery(hint, marketLabel)
		}
		for _, market := range normalizedStringSlice(teamSpecialization.Markets) {
			appendQuery(market, "market news")
		}
		for _, assetClass := range normalizedStringSlice(teamSpecialization.AssetClasses) {
			appendQuery(assetClass, "market news")
		}
	}
	return normalizedStringSlice(queries)
}

func marketNewsQueryRef(fund *repository.Fund, profile fundMarketProfile, query string) marketdata.InstrumentRef {
	if fund != nil {
		instrument := defaultInstrumentRef(fund, workflow.FocusStock, strings.TrimSpace(query))
		instrument.InstrumentKey = ""
		return instrument
	}
	return marketdata.InstrumentRef{
		Symbol:        strings.TrimSpace(query),
		Market:        profile.Market,
		Exchange:      profile.Exchange,
		AssetClass:    profile.AssetClass,
		QuoteCurrency: profile.BaseCurrency,
	}
}

func digestTickerSymbols(instruments []marketdata.InstrumentRef) []string {
	result := make([]string, 0, len(instruments))
	seen := make(map[string]struct{}, len(instruments))
	for _, instrument := range instruments {
		symbol := strings.ToUpper(strings.TrimSpace(instrument.Symbol))
		if symbol == "" || !marketdata.IsTickerLikeSymbol(symbol) {
			continue
		}
		if _, ok := seen[symbol]; ok {
			continue
		}
		seen[symbol] = struct{}{}
		result = append(result, symbol)
	}
	return result
}

func marketNewsQueryLabel(instrument marketdata.InstrumentRef) string {
	symbol := strings.TrimSpace(instrument.Symbol)
	if symbol == "" {
		return "market-news"
	}
	return symbol
}

func marketNewsDigestItemKey(item marketdata.NewsItem) string {
	if url := strings.ToLower(strings.TrimSpace(item.URL)); url != "" {
		return url
	}
	return strings.ToLower(strings.TrimSpace(item.Title))
}

func tagDigestNewsItems(items []marketdata.NewsItem, instrument marketdata.InstrumentRef) []marketdata.NewsItem {
	tag := strings.ToUpper(strings.TrimSpace(instrument.Symbol))
	if tag == "" || !marketdata.IsTickerLikeSymbol(tag) {
		for i := range items {
			items[i].Symbols = nil
		}
		return items
	}
	for i := range items {
		if len(items[i].Symbols) == 0 {
			items[i].Symbols = []string{tag}
			continue
		}
		items[i].Symbols = normalizedStringSlice(items[i].Symbols)
	}
	return items
}

func fundMarketNewsLabel(profile fundMarketProfile) string {
	switch strings.ToLower(strings.TrimSpace(profile.PrimaryDirection)) {
	case "stocks", "equity", "equities":
		return "stock market news"
	case "crypto", "tokens":
		return "crypto market news"
	case "futures":
		return "futures market news"
	}
	switch profile.AssetClass {
	case "equity":
		return "stock market news"
	default:
		return "market news"
	}
}

func marketQueryInstrument(fund *repository.Fund, symbol string) marketdata.InstrumentRef {
	trimmedSymbol := strings.TrimSpace(symbol)
	if trimmedSymbol == "" {
		return marketdata.InstrumentRef{}
	}
	return defaultInstrumentRef(fund, workflow.FocusStock, trimmedSymbol)
}

func positionMapKey(instrumentKey, symbol string) string {
	return firstNonEmptyValue(instrumentKey, symbol)
}

func nullString(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}
