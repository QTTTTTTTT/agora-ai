package llm

import (
	"context"
	"time"
)

// ---------------------------------------------------------------------------
// Model Tier – 模型级别
// ---------------------------------------------------------------------------

// ModelTier 模型级别，用于路由到不同质量/成本的模型。
type ModelTier string

const (
	TierCritical ModelTier = "critical" // 关键决策：圆桌总结、PM推理、A/B分析
	TierStandard ModelTier = "standard" // 日常任务：研究摘要、风控检查
	TierSimple   ModelTier = "simple"   // 简单任务：格式化、数据提取
)

// ValidTiers 所有合法的 tier 列表，用于校验。
var ValidTiers = []ModelTier{TierCritical, TierStandard, TierSimple}

// IsValid 检查 tier 值是否合法。
func (t ModelTier) IsValid() bool {
	for _, v := range ValidTiers {
		if t == v {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Provider – 供应商
// ---------------------------------------------------------------------------

// Provider 供应商标识。
type Provider string

const (
	ProviderOpenAI   Provider = "openai"
	ProviderClaude   Provider = "claude"
	ProviderDeepSeek Provider = "deepseek"
	ProviderQwen     Provider = "qwen"
	ProviderGemini   Provider = "gemini"
	ProviderCustom   Provider = "custom" // 用户自定义 OpenAI-Compatible 端点
)

func (p Provider) UsesOpenAIChatCompletions() bool {
	switch p {
	case ProviderOpenAI, ProviderDeepSeek, ProviderQwen, ProviderCustom:
		return true
	default:
		return false
	}
}

func (p Provider) UsesClaudeMessagesAPI() bool {
	return p == ProviderClaude
}

func (p Provider) UsesGeminiGenerateContent() bool {
	return p == ProviderGemini
}

// ---------------------------------------------------------------------------
// ModelConfig – 模型配置
// ---------------------------------------------------------------------------

// ModelConfig 描述一个模型端点及其定价。
type ModelConfig struct {
	Provider      Provider  `json:"provider"`
	ModelName     string    `json:"model_name"`  // e.g. "gpt-4o", "deepseek-chat"
	BaseURL       string    `json:"base_url"`    // API endpoint, e.g. "https://api.openai.com/v1"
	APIKey        string    `json:"api_key"`     // 已加密的 key；运行时解密后使用
	MaxTokens     int       `json:"max_tokens"`  // 生成上限
	Temperature   float64   `json:"temperature"` // 采样温度
	ResolvedTier  ModelTier `json:"-"`
	UsesCustomKey bool      `json:"-"`

	// 定价（每百万 token，单位：分）
	InputPricePer1M  float64 `json:"input_price_per_1m"`  // 平台对外售价
	OutputPricePer1M float64 `json:"output_price_per_1m"` // 平台对外售价
	CostPer1M        float64 `json:"cost_per_1m"`         // 平台实际成本
}

// Clone 返回 ModelConfig 的深拷贝。
func (mc *ModelConfig) Clone() *ModelConfig {
	if mc == nil {
		return nil
	}
	c := *mc
	return &c
}

// ---------------------------------------------------------------------------
// ChatMessage / ChatRequest / ChatResponse
// ---------------------------------------------------------------------------

// ChatMessage 单条聊天消息。
type ChatMessage struct {
	Role    string `json:"role"` // "system" | "user" | "assistant"
	Content string `json:"content"`
}

// ChatRequest 聊天请求。
type ChatRequest struct {
	Messages    []ChatMessage `json:"messages"`
	ModelTier   ModelTier     `json:"model_tier"`  // 如果为空则用具体模型
	Model       string        `json:"model"`       // 具体模型名（覆盖 tier）
	MaxTokens   int           `json:"max_tokens"`  // 0 表示使用 ModelConfig 默认值
	Temperature float64       `json:"temperature"` // <=0 表示使用 ModelConfig 默认值
	UserID      string        `json:"user_id"`     // 用于计费
	AgentID     string        `json:"agent_id"`    // 关联 agent
	FundID      string        `json:"fund_id"`     // 关联基金
	StepName    string        `json:"step_name"`   // 工作流步骤名

	// OwnerID 是为本次调用承担成本/配额的用户。
	// 多数情况下等于 UserID；但在 marketplace 黑盒推理等场景下，
	// 调用者(UserID) 可能租用了其他人发布的策略，
	// 此时 OwnerID 是策略所有者，配额、熔断、自带 Key 都按 OwnerID 隔离，
	// 避免一个所有者把另一个的配额打爆。为空时回退到 UserID。
	OwnerID string `json:"owner_id"`
}

// EffectiveOwner 返回承担成本的用户 ID。
func (r *ChatRequest) EffectiveOwner() string {
	if r == nil {
		return ""
	}
	if r.OwnerID != "" {
		return r.OwnerID
	}
	return r.UserID
}

// ChatResponse 聊天响应。
type ChatResponse struct {
	Content      string `json:"content"`
	Model        string `json:"model"`         // 实际使用的模型
	Provider     string `json:"provider"`      // 实际供应商
	InputTokens  int    `json:"input_tokens"`  // 输入 token 数
	OutputTokens int    `json:"output_tokens"` // 输出 token 数
	TotalTokens  int    `json:"total_tokens"`
	LatencyMs    int64  `json:"latency_ms"` // 端到端延迟（毫秒）

	// 计费 — 单位：分（CNY cents）
	InputCost   float64 `json:"input_cost"` // 成本
	OutputCost  float64 `json:"output_cost"`
	TotalCost   float64 `json:"total_cost"`
	InputPrice  float64 `json:"input_price"` // 售价
	OutputPrice float64 `json:"output_price"`
	TotalPrice  float64 `json:"total_price"`
}

// ---------------------------------------------------------------------------
// UsageRecord – 用量记录
// ---------------------------------------------------------------------------

// UsageRecord 单次 LLM 调用的用量记录，用于持久化和计费。
type UsageRecord struct {
	ID           string    `json:"id,omitempty"`
	UserID       string    `json:"user_id"`
	FundID       string    `json:"fund_id"`
	AgentID      string    `json:"agent_id,omitempty"`
	StepName     string    `json:"step_name"`
	Tier         string    `json:"tier,omitempty"`
	Model        string    `json:"model"`
	Provider     string    `json:"provider"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	Cost         float64   `json:"cost"`  // 成本（分）
	Price        float64   `json:"price"` // 售价（分）
	IsCustomKey  bool      `json:"is_custom_key"`
	CreatedAt    time.Time `json:"created_at"`
}

// ---------------------------------------------------------------------------
// LLMClient – 统一客户端接口
// ---------------------------------------------------------------------------

// LLMClient 所有外部调用方依赖的统一接口。
type LLMObserver interface {
	ObserveLLM(provider, model, step, status string, latency time.Duration)
}

// LLMClient 所有外部调用方依赖的统一接口。
type LLMClient interface {
	// Chat 发送聊天请求并返回响应。
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)

	// ListModels 列出所有可用模型信息（供前端展示）。
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// ---------------------------------------------------------------------------
// ModelInfo – 模型信息（前端展示用）
// ---------------------------------------------------------------------------

// ModelInfo 描述一个对外展示的模型条目。
type ModelInfo struct {
	Provider         string  `json:"provider"`
	ModelName        string  `json:"model_name"`
	DisplayName      string  `json:"display_name"`
	Tier             string  `json:"tier"`               // 推荐级别
	InputPricePer1M  float64 `json:"input_price_per_1m"` // 售价
	OutputPricePer1M float64 `json:"output_price_per_1m"`
	Description      string  `json:"description"`
	IsDefault        bool    `json:"is_default"`
}

// ---------------------------------------------------------------------------
// UsageRecorder – 用量记录接口（由外部实现，如 DB 层）
// ---------------------------------------------------------------------------

// UsageRecorder 用量持久化接口。
type UsageRecorder interface {
	// Record 记录单次调用用量。
	Record(ctx context.Context, record *UsageRecord) error
	// GetDailyUsage 获取某用户某天的用量列表。
	GetDailyUsage(ctx context.Context, userID string, date string) ([]UsageRecord, error)
	// GetMonthlyUsage 获取某用户某月的汇总用量。
	GetMonthlyUsage(ctx context.Context, userID string, yearMonth string) (*UsageSummary, error)
}

// ---------------------------------------------------------------------------
// UsageSummary / 子结构
// ---------------------------------------------------------------------------

// UsageSummary 月度用量汇总。
type UsageSummary struct {
	TotalInputTokens  int64                  `json:"total_input_tokens"`
	TotalOutputTokens int64                  `json:"total_output_tokens"`
	TotalCost         float64                `json:"total_cost"`
	TotalPrice        float64                `json:"total_price"`
	Profit            float64                `json:"profit"`
	ByModel           map[string]*ModelUsage `json:"by_model"`
	ByStep            map[string]*StepUsage  `json:"by_step"`
	DailyTrend        []DailyUsage           `json:"daily_trend"`
}

// ModelUsage 按模型维度的用量。
type ModelUsage struct {
	Model        string  `json:"model"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	Cost         float64 `json:"cost"`
	Price        float64 `json:"price"`
	CallCount    int     `json:"call_count"`
}

// StepUsage 按工作流步骤维度的用量。
type StepUsage struct {
	StepName     string  `json:"step_name"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	Cost         float64 `json:"cost"`
	Price        float64 `json:"price"`
	CallCount    int     `json:"call_count"`
}

// DailyUsage 单日用量趋势条目。
type DailyUsage struct {
	Date         string  `json:"date"` // "2006-01-02"
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	Cost         float64 `json:"cost"`
	Price        float64 `json:"price"`
	CallCount    int     `json:"call_count"`
}
