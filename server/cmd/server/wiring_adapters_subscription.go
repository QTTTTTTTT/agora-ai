// wiring_adapters_subscription.go — adapters that bridge the
// `internal/subscription` package (plan / billing / usage / per-user
// model config) to the `internal/api` DTOs the HTTP handlers consume.
//
// Pulled out of wiring_adapters.go (which had grown to 24,654 lines)
// to give a stable file boundary for "all things billing & per-user
// config" wiring. No new logic — these structs and methods are
// preserved verbatim from the original positions:
//
//	subscriptionServiceAdapter            wiring_adapters.go:73-136
//	usageTrackerAdapter                   wiring_adapters.go:140-227
//	modelConfigServiceAdapter             wiring_adapters.go:408-499
//	convertSubscriptionPlan / convertSubscription
//	                                      wiring_adapters.go:10690-10727
//
// The interface assertion `var _ llm.SubscriptionGuard = (*llmRuntime)(nil)`
// stays in wiring_adapters.go because llmRuntime itself still lives
// there (next extraction candidate, ~900 lines on its own).

package main

import (
	"context"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/subscription"
)

// ---------------------------------------------------------------------------
// subscriptionServiceAdapter — adapts subscription.SubscriptionService
// (the in-package billing core) to the api.SubscriptionService interface
// the HTTP layer talks to.
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// usageTrackerAdapter — adapts subscription.UsageTracker (LLM usage
// + monthly billing) to the api.UsageTracker interface.
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// modelConfigServiceAdapter — adapts subscription.ModelConfigService
// (per-user / per-agent BYO LLM config). Re-syncs the LLM runtime on
// SaveConfig / DeleteConfig so the running provider router sees the
// new keys without a server restart.
//
// Note: this still references *llmRuntime, which lives in
// wiring_adapters.go for now — same package, so the type reference
// works without extra imports.
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Plan / Subscription DTO converters used by both the subscription
// service adapter above and a couple of admin handlers elsewhere.
// ---------------------------------------------------------------------------

func convertSubscriptionPlan(plan *subscription.Plan) *api.SubscriptionPlan {
	if plan == nil {
		return nil
	}
	return &api.SubscriptionPlan{
		Tier:              string(plan.Tier),
		Name:              plan.Name,
		PriceCentsMonth:   plan.PriceCentsMonth,
		PriceCentsUSDMonth: plan.PriceCentsUSDMonth,
		PriceCentsUSDYear:  plan.PriceCentsUSDYear,
		MinSeats:           plan.MinSeats,
		ContactSales:       plan.ContactSales,
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
