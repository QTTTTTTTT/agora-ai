package llm

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// 平台默认模型配置
// ---------------------------------------------------------------------------

// DefaultModels 平台为每个 tier 预设的默认模型。
var DefaultModels = map[ModelTier]*ModelConfig{
	TierCritical: {
		Provider:         ProviderOpenAI,
		ModelName:        "gpt-4o",
		BaseURL:          "https://api.openai.com/v1",
		MaxTokens:        4096,
		Temperature:      0.7,
		InputPricePer1M:  3500,  // 售价 ¥3.5/百万 token
		OutputPricePer1M: 14000, // 售价 ¥14/百万 token
		CostPer1M:        2500,  // 成本 ¥2.5/百万
	},
	TierStandard: {
		Provider:         ProviderDeepSeek,
		ModelName:        "deepseek-chat",
		BaseURL:          "https://api.deepseek.com/v1",
		MaxTokens:        4096,
		Temperature:      0.7,
		InputPricePer1M:  150, // 售价 ¥0.15/百万
		OutputPricePer1M: 300,
		CostPer1M:        100,
	},
	TierSimple: {
		Provider:         ProviderOpenAI,
		ModelName:        "gpt-4o-mini",
		BaseURL:          "https://api.openai.com/v1",
		MaxTokens:        2048,
		Temperature:      0.3,
		InputPricePer1M:  25, // 售价 ¥0.025/百万
		OutputPricePer1M: 100,
		CostPer1M:        15,
	},
}

// ---------------------------------------------------------------------------
// 工作流步骤 → 模型级别映射
// ---------------------------------------------------------------------------

// StepTierMapping 将每个工作流步骤映射到推荐的模型级别。
var StepTierMapping = map[string]ModelTier{
	"macro_brief":        TierStandard,
	"research_parallel":  TierStandard,
	"quant_signals":      TierSimple,
	"roundtable_opinion": TierStandard,
	"roundtable_summary": TierCritical, // 圆桌总结用强模型
	"pm_plan":            TierCritical, // PM推理用强模型
	"risk_review":        TierStandard,
	"trade_execution":    TierSimple,
	"settlement":         TierSimple,
	"daily_review":       TierStandard,
	"abtest_analysis":    TierCritical, // A/B分析用强模型
}

// ---------------------------------------------------------------------------
// ModelRouter
// ---------------------------------------------------------------------------

// ModelRouter 根据 tier、步骤名和用户自定义配置选择最终模型。
type SubscriptionGuard interface {
	CheckModelAccess(ctx context.Context, userID, modelTier string) error
	GetEffectivePlan(ctx context.Context, userID string) (EffectivePlan, error)
}

type EffectivePlan interface {
	AllowsCustomKey() bool
}

type ModelRouter struct {
	mu sync.RWMutex

	// 平台系统 API Keys，按 Provider 索引。
	systemAPIKeys map[Provider]string

	// 平台默认模型，按 tier 索引。在 init 时从 DefaultModels 深拷贝。
	defaultModels map[ModelTier]*ModelConfig

	// 用户级别覆盖: key = "userID:tier"
	userOverrides map[string]*ModelConfig

	// 用户自定义端点: key = "userID:provider"
	customEndpoints map[string]*ModelConfig

	// Agent 默认模型配置: key = "userID:agentID"
	agentDefaults map[string]*ModelConfig

	// 用量记录器（可选，为 nil 时跳过记录）。
	usageRecorder UsageRecorder
	guard         SubscriptionGuard

	// Sprint 10.1 — model A/B hook. When non-nil it is invoked
	// after the explicit req.Model check (so manual model pins
	// remain forensic) and before any user / agent override
	// resolution (so operator-initiated experiments override
	// user defaults within their scope). nil = no A/B routing.
	modelABHook ModelABHook

	// S14.B — fund-level provider override hook. Sits between
	// the A/B hook and per-user resolution. See fund_override_hook.go
	// for the priority-chain rationale. nil = no fund overrides.
	fundOverrideHook FundOverrideHook
}

// NewModelRouter 创建 ModelRouter。
//
// systemAPIKeys: 平台为各 Provider 预置的 API Key（未加密），用于填充默认配置。
// defaultModels: 平台默认 tier 模型；为空时回退到 DefaultModels。
// recorder:      用量记录器实现。传 nil 则不记录用量。
func NewModelRouter(systemAPIKeys map[Provider]string, defaultModels map[ModelTier]*ModelConfig, recorder UsageRecorder, guard SubscriptionGuard) *ModelRouter {
	r := &ModelRouter{
		systemAPIKeys:   make(map[Provider]string),
		defaultModels:   make(map[ModelTier]*ModelConfig, len(DefaultModels)),
		userOverrides:   make(map[string]*ModelConfig),
		customEndpoints: make(map[string]*ModelConfig),
		agentDefaults:   make(map[string]*ModelConfig),
		usageRecorder:   recorder,
		guard:           guard,
	}

	for k, v := range systemAPIKeys {
		r.systemAPIKeys[k] = v
	}

	for tier, mc := range DefaultModels {
		cloned := mc.Clone()
		if override, ok := defaultModels[tier]; ok && override != nil {
			cloned = override.Clone()
		}
		if key, ok := r.systemAPIKeys[cloned.Provider]; ok && cloned.APIKey == "" {
			cloned.APIKey = key
		}
		r.defaultModels[tier] = cloned
	}

	return r
}

// ---------------------------------------------------------------------------
// ResolveModel – 核心路由逻辑
// ---------------------------------------------------------------------------

// ResolveModel 根据请求参数解析出最终使用的 ModelConfig。
//
// 优先级（从高到低）：
//  1. req.Model 不为空 → 在 PlatformModels 中查找对应配置
//  2. Agent 默认模型配置（agentDefaults）
//  3. 用户自定义 tier 覆盖（userOverrides）
//  4. 用户自定义端点（customEndpoints）匹配 tier 默认供应商
//  5. StepTierMapping 查找 step → tier
//  6. req.ModelTier 显式指定
//  7. 最终 fallback 到 TierStandard
//
// 配额、自带 Key、tier 准入按 EffectiveOwner（OwnerID 优先，其次 UserID）
// 隔离，这样 marketplace 黑盒推理时调用者用的是策略所有者的配置/额度。
//
// 返回的 *ModelConfig 是一份新拷贝，调用方可安全修改。
func (r *ModelRouter) ResolveModel(ctx context.Context, req *ChatRequest) (*ModelConfig, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	owner := req.EffectiveOwner()
	tier := r.resolveTier(req)
	if err := r.enforceTierAccess(ctx, owner, tier); err != nil {
		return nil, err
	}

	// --- 1. 指定了具体模型名 ---
	if req.Model != "" {
		if cfg := r.findModelByName(req.Model, owner); cfg != nil {
			return r.finalizeConfig(ctx, req, tier, cfg.Clone())
		}
		// 找不到具体模型，记录警告并继续用 tier 路由
		log.Printf("[llm/router] model %q not found, falling back to tier routing", req.Model)
	}

	// --- 1.5. Sprint 10.1 — model A/B routing hook ---
	// The hook is invoked with the router's read lock held. Its
	// implementation (modelab.Resolver) does its own DB work and
	// owns a separate mutex, so there is no cycle. The contract
	// is: a ModelABHook MUST NOT call back into ModelRouter
	// write methods (SetUserOverride, ReplaceAgentConfigs, …)
	// from inside this hook — doing so would dead-lock against
	// the read lock we hold here.
	if hook := r.modelABHook; hook != nil {
		if decision := hook(ctx, req); decision != nil && decision.Config != nil {
			cfg := decision.Config.Clone()
			r.ensureAPIKey(cfg, owner)
			return r.finalizeConfig(ctx, req, tier, cfg)
		}
	}

	// --- 1.6. S14.B — fund-level provider override ---
	// The hook implementation (llmRuntime in cmd/server) reads
	// fund_llm_overrides, ranks by specificity (agent + role + tier
	// + label) and translates the row into a fully-formed ModelConfig
	// including API key + base URL fetched from platform_llm_providers.
	// We DO NOT call ensureAPIKey here because the hook is required
	// to return a complete config — the fund's strategy owner is
	// asserting "use exactly this", not "use the platform key for
	// this provider". If the hook returns nil, the chain continues
	// to per-user resolution below.
	if hook := r.fundOverrideHook; hook != nil {
		if decision := hook(ctx, req); decision != nil && decision.Config != nil {
			cfg := decision.Config.Clone()
			return r.finalizeConfig(ctx, req, tier, cfg)
		}
	}

	// --- 2. Agent 默认模型配置 ---
	if owner != "" && req.AgentID != "" {
		key := agentDefaultKey(owner, req.AgentID)
		if agentCfg, ok := r.agentDefaults[key]; ok {
			cfg := agentCfg.Clone()
			r.ensureAPIKey(cfg, owner)
			return r.finalizeConfig(ctx, req, tier, cfg)
		}
	}

	// --- 3. 用户自定义 tier 覆盖 ---
	if owner != "" {
		key := userTierKey(owner, tier)
		if uo, ok := r.userOverrides[key]; ok {
			cfg := uo.Clone()
			r.ensureAPIKey(cfg, owner)
			return r.finalizeConfig(ctx, req, tier, cfg)
		}
	}

	// --- 4. 用户自定义端点 ---
	if owner != "" {
		defaultCfg := r.defaultModels[tier]
		if defaultCfg != nil {
			epKey := userProviderKey(owner, defaultCfg.Provider)
			if ep, ok := r.customEndpoints[epKey]; ok {
				cfg := defaultCfg.Clone()
				cfg.BaseURL = ep.BaseURL
				cfg.APIKey = ep.APIKey
				cfg.Provider = ep.Provider
				cfg.UsesCustomKey = strings.TrimSpace(ep.APIKey) != ""
				if ep.ModelName != "" {
					cfg.ModelName = ep.ModelName
				}
				return r.finalizeConfig(ctx, req, tier, cfg)
			}
		}
	}

	// --- 5/6/7. 平台默认模型 ---
	cfg := r.defaultModels[tier]
	if cfg == nil {
		log.Printf("[llm/router] no default config for tier %q, falling back to standard", tier)
		cfg = r.defaultModels[TierStandard]
		if cfg == nil {
			cfg = DefaultModels[TierStandard].Clone()
		}
	}

	result := cfg.Clone()
	r.ensureAPIKey(result, owner)
	return r.finalizeConfig(ctx, req, tier, result)
}

// resolveTier 从请求中确定最终 tier。
func (r *ModelRouter) resolveTier(req *ChatRequest) ModelTier {
	if req.StepName != "" {
		if t, ok := StepTierMapping[req.StepName]; ok {
			return t
		}
	}
	if req.ModelTier.IsValid() {
		return req.ModelTier
	}
	return TierStandard
}

func (r *ModelRouter) enforceTierAccess(ctx context.Context, userID string, tier ModelTier) error {
	if strings.TrimSpace(userID) == "" || !tier.IsValid() || r.guard == nil {
		return nil
	}
	if err := r.guard.CheckModelAccess(ctx, userID, string(tier)); err != nil {
		return fmt.Errorf("llm: model access denied for tier %s: %w", tier, err)
	}
	return nil
}

func (r *ModelRouter) finalizeConfig(ctx context.Context, req *ChatRequest, tier ModelTier, cfg *ModelConfig) (*ModelConfig, error) {
	if cfg == nil {
		return nil, fmt.Errorf("llm: empty model config")
	}
	owner := req.EffectiveOwner()
	cfg.ResolvedTier = tier
	cfg.UsesCustomKey = r.isCustomKey(cfg)
	if err := r.enforceCustomKeyAccess(ctx, owner, cfg); err != nil {
		return nil, err
	}
	r.ensureAPIKey(cfg, owner)
	cfg.UsesCustomKey = r.isCustomKey(cfg)
	return r.applyRequestOverrides(cfg, req), nil
}

func (r *ModelRouter) isCustomKey(cfg *ModelConfig) bool {
	if cfg == nil || strings.TrimSpace(cfg.APIKey) == "" {
		return false
	}
	systemKey, ok := r.systemAPIKeys[cfg.Provider]
	if !ok {
		return true
	}
	return strings.TrimSpace(systemKey) != strings.TrimSpace(cfg.APIKey)
}

func (r *ModelRouter) enforceCustomKeyAccess(ctx context.Context, userID string, cfg *ModelConfig) error {
	if strings.TrimSpace(userID) == "" || cfg == nil || !cfg.UsesCustomKey || r.guard == nil {
		return nil
	}
	plan, err := r.guard.GetEffectivePlan(ctx, userID)
	if err != nil {
		return fmt.Errorf("llm: load effective plan for custom key: %w", err)
	}
	if plan == nil || !plan.AllowsCustomKey() {
		return fmt.Errorf("llm: custom model key is not allowed for current subscription")
	}
	return nil
}

// findModelByName 根据模型名查找配置。
// 先从用户自定义中找，再从平台默认中找，最后从 PlatformModels 目录中构建。
func (r *ModelRouter) findModelByName(modelName string, userID string) *ModelConfig {
	// 查用户覆盖中是否有此模型
	if userID != "" {
		for _, tier := range ValidTiers {
			key := userTierKey(userID, tier)
			if uo, ok := r.userOverrides[key]; ok && uo.ModelName == modelName {
				return uo.Clone()
			}
		}
	}

	// 查平台默认
	for _, mc := range r.defaultModels {
		if mc.ModelName == modelName {
			return mc.Clone()
		}
	}

	// 从 PlatformModels 目录构建
	for _, mi := range PlatformModels {
		if mi.ModelName == modelName {
			cfg := &ModelConfig{
				Provider:         Provider(mi.Provider),
				ModelName:        mi.ModelName,
				BaseURL:          providerDefaultBaseURL(Provider(mi.Provider)),
				MaxTokens:        4096,
				Temperature:      0.7,
				InputPricePer1M:  mi.InputPricePer1M,
				OutputPricePer1M: mi.OutputPricePer1M,
				CostPer1M:        mi.InputPricePer1M * 0.7, // 估算成本
			}
			// 注入系统 key
			if key, ok := r.systemAPIKeys[cfg.Provider]; ok {
				cfg.APIKey = key
			}
			return cfg
		}
	}

	return nil
}

// ensureAPIKey 确保 config 有可用的 API Key。
// 如果 config 本身没有 key，尝试从系统 keys 注入。
func (r *ModelRouter) ensureAPIKey(cfg *ModelConfig, _ string) {
	if cfg.APIKey != "" {
		return
	}
	if key, ok := r.systemAPIKeys[cfg.Provider]; ok {
		cfg.APIKey = key
	}
}

// applyRequestOverrides 将请求级参数覆盖到 config 上。
func (r *ModelRouter) applyRequestOverrides(cfg *ModelConfig, req *ChatRequest) *ModelConfig {
	if req.MaxTokens > 0 {
		cfg.MaxTokens = req.MaxTokens
	}
	if req.Temperature > 0 {
		cfg.Temperature = req.Temperature
	}
	return cfg
}

// ---------------------------------------------------------------------------
// 用户配置管理
// ---------------------------------------------------------------------------

// SetUserOverride 设置用户对某个 tier 的模型覆盖。
// 设置后，该用户在此 tier 的所有请求将使用此配置。
func (r *ModelRouter) SetUserOverride(userID string, tier ModelTier, config *ModelConfig) {
	if userID == "" || config == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	key := userTierKey(userID, tier)
	r.userOverrides[key] = config.Clone()
}

// RemoveUserOverride 移除用户对某个 tier 的覆盖，恢复平台默认。
func (r *ModelRouter) RemoveUserOverride(userID string, tier ModelTier) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := userTierKey(userID, tier)
	delete(r.userOverrides, key)
}

// SetCustomEndpoint 设置用户的自定义 API 端点。
// 用户自带 Key 时使用此方法。Provider 字段决定端点归属。
func (r *ModelRouter) SetCustomEndpoint(userID string, config *ModelConfig) {
	if userID == "" || config == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	key := userProviderKey(userID, config.Provider)
	r.customEndpoints[key] = config.Clone()
}

// RemoveCustomEndpoint 移除用户的自定义端点。
func (r *ModelRouter) RemoveCustomEndpoint(userID string, provider Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := userProviderKey(userID, provider)
	delete(r.customEndpoints, key)
}

// GetUserConfig 获取用户当前生效的模型配置（合并默认 + 覆盖）。
// 返回的 map key 为 tier 字符串。
func (r *ModelRouter) GetUserConfig(userID string) map[string]*ModelConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[string]*ModelConfig, len(ValidTiers))
	for _, tier := range ValidTiers {
		// 先检查用户覆盖
		key := userTierKey(userID, tier)
		if uo, ok := r.userOverrides[key]; ok {
			result[string(tier)] = uo.Clone()
			continue
		}
		// 使用平台默认
		if dc, ok := r.defaultModels[tier]; ok {
			result[string(tier)] = dc.Clone()
		}
	}
	return result
}

// GetUsageRecorder 返回关联的 UsageRecorder（可能为 nil）。
func (r *ModelRouter) GetUsageRecorder() UsageRecorder {
	return r.usageRecorder
}

// ReplaceUserConfigs atomically replaces all runtime user-scoped configs for a user.
func (r *ModelRouter) ReplaceUserConfigs(userID string, overrides map[ModelTier]*ModelConfig, endpoints map[Provider]*ModelConfig) {
	if strings.TrimSpace(userID) == "" {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	prefix := userID + ":"
	for key := range r.userOverrides {
		if strings.HasPrefix(key, prefix) {
			delete(r.userOverrides, key)
		}
	}
	for key := range r.customEndpoints {
		if strings.HasPrefix(key, prefix) {
			delete(r.customEndpoints, key)
		}
	}

	for tier, cfg := range overrides {
		if cfg == nil {
			continue
		}
		r.userOverrides[userTierKey(userID, tier)] = cfg.Clone()
	}
	for provider, cfg := range endpoints {
		if cfg == nil {
			continue
		}
		r.customEndpoints[userProviderKey(userID, provider)] = cfg.Clone()
	}
}

// ReplaceAgentConfigs atomically replaces all runtime agent-scoped defaults for a user.
func (r *ModelRouter) ReplaceAgentConfigs(userID string, agentDefaults map[string]*ModelConfig) {
	if strings.TrimSpace(userID) == "" {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	prefix := userID + ":"
	for key := range r.agentDefaults {
		if strings.HasPrefix(key, prefix) {
			delete(r.agentDefaults, key)
		}
	}
	for agentID, cfg := range agentDefaults {
		if cfg == nil || strings.TrimSpace(agentID) == "" {
			continue
		}
		r.agentDefaults[agentDefaultKey(userID, agentID)] = cfg.Clone()
	}
}

// ---------------------------------------------------------------------------
// 辅助函数
// ---------------------------------------------------------------------------

func userTierKey(userID string, tier ModelTier) string {
	return fmt.Sprintf("%s:%s", userID, tier)
}

func userProviderKey(userID string, provider Provider) string {
	return fmt.Sprintf("%s:%s", userID, provider)
}

func agentDefaultKey(userID, agentID string) string {
	return fmt.Sprintf("%s:%s", userID, agentID)
}

// providerDefaultBaseURL 返回各供应商的默认 Base URL。
func providerDefaultBaseURL(p Provider) string {
	switch p {
	case ProviderOpenAI:
		return "https://api.openai.com/v1"
	case ProviderClaude:
		return "https://api.anthropic.com/v1"
	case ProviderDeepSeek:
		return "https://api.deepseek.com/v1"
	case ProviderQwen:
		return "https://dashscope.aliyuncs.com/compatible-mode/v1"
	case ProviderGemini:
		return "https://generativelanguage.googleapis.com/v1beta"
	default:
		return ""
	}
}
