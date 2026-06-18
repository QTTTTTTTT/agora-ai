package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// MultiProviderClient
// ---------------------------------------------------------------------------

const (
	// All LLM-side timeouts are unified to 5 minutes. The previous
	// 240/250s split was an artefact of accommodating a single
	// slow provider; reasoning models (gemini-3.1-pro-preview,
	// o1-style chains) regularly need 2-3 minutes for a single
	// complex response, and a tight cap surfaced as false
	// "context deadline exceeded" errors in the daily review
	// (agent_self_learning), reflection, and roundtable steps.
	// 5 min is wide enough to absorb genuinely slow long-form
	// completions while still bounding the per-call cost.
	llmTotalRequestTimeout = 5 * time.Minute
	llmHTTPClientTimeout   = 5 * time.Minute
	llmAttemptTimeout      = 5 * time.Minute
)

// DollarBudgetGate is the F14 pre-call check that enforces a configurable
// daily/monthly cents cap per (user, fund). Implemented by
// subscription.BudgetService in production; mocked in tests.
//
// Implementations MUST return an error that wraps a sentinel the workflow
// layer recognises (subscription.ErrLLMBudgetExceeded today) so the
// orchestrator can pause the run with an explicit "out of budget" reason.
// Returning a non-nil error blocks the LLM call.
type DollarBudgetGate interface {
	Check(ctx context.Context, userID, fundID string) error
}

// FundQuotaGate is the F28 per-fund token-budget check. The dollar gate
// (DollarBudgetGate) is per-user, this is per-fund — both are
// orthogonal and both can reject a call. RecordTokens is called after
// a successful provider response so cumulative usage stays accurate.
//
// Implemented by quota.Service in production. Nil-safe: when not
// installed the gate is skipped entirely (preserves legacy behaviour).
type FundQuotaGate interface {
	CheckLLMTokens(ctx context.Context, fundID string, requestedTokens int64) error
	RecordLLMTokens(ctx context.Context, fundID string, promptTokens, completionTokens int64) error
}

// MultiProviderClient 是统一的多供应商 LLM 客户端，实现 LLMClient 接口。
type MultiProviderClient struct {
	router       *ModelRouter
	httpClient   *http.Client
	systemKeys   map[Provider]string // 平台系统 API Keys
	observer     LLMObserver
	limiter      *OwnerLimiter
	cache        *ChatCache
	budget       *CallBudgetLimiter
	dollarBudget DollarBudgetGate
	fundQuota    FundQuotaGate  // F28: per-fund token gate (orthogonal to dollarBudget)
	failover     *failoverState // F15: provider failover chain
	mu           sync.RWMutex
}

// 编译期断言：MultiProviderClient 实现了 LLMClient 接口。
var _ LLMClient = (*MultiProviderClient)(nil)

// NewMultiProviderClient 创建 MultiProviderClient。
func NewMultiProviderClient(router *ModelRouter, systemKeys map[Provider]string) *MultiProviderClient {
	return NewMultiProviderClientWithObserver(router, systemKeys, nil)
}

func NewMultiProviderClientWithObserver(router *ModelRouter, systemKeys map[Provider]string, observer LLMObserver) *MultiProviderClient {
	keys := make(map[Provider]string, len(systemKeys))
	for k, v := range systemKeys {
		keys[k] = v
	}

	return &MultiProviderClient{
		router: router,
		httpClient: &http.Client{
			Timeout: llmHTTPClientTimeout,
		},
		systemKeys: keys,
		observer:   observer,
	}
}

// SetOwnerLimiter 安装按所有者隔离的配额/熔断器。nil 表示禁用。
func (c *MultiProviderClient) SetOwnerLimiter(limiter *OwnerLimiter) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.limiter = limiter
}

// SetChatCache installs an in-process exact-match chat cache. nil disables caching.
func (c *MultiProviderClient) SetChatCache(cache *ChatCache) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache = cache
}

// SetCallBudgetLimiter installs a short-window owner+step call budget limiter.
// nil disables call budgeting.
func (c *MultiProviderClient) SetCallBudgetLimiter(budget *CallBudgetLimiter) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.budget = budget
}

// SetDollarBudgetGate installs the F14 cents-denominated daily/monthly
// hard cap. nil disables it (the call-count limiter still runs).
func (c *MultiProviderClient) SetDollarBudgetGate(gate DollarBudgetGate) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dollarBudget = gate
}

// SetFailoverConfig installs the F15 provider failover chain. Pass an
// empty config to disable failover (single-attempt behaviour).
func (c *MultiProviderClient) SetFailoverConfig(cfg FailoverConfig) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failover = newFailoverState(cfg)
}

func (c *MultiProviderClient) failoverConfig() FailoverConfig {
	if c == nil {
		return FailoverConfig{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.failover == nil {
		return FailoverConfig{}
	}
	return c.failover.snapshot()
}

// SetFundQuotaGate installs the F28 per-fund token quota gate. nil
// disables it (other gates still run).
func (c *MultiProviderClient) SetFundQuotaGate(gate FundQuotaGate) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fundQuota = gate
}

func (c *MultiProviderClient) fundQuotaGate() FundQuotaGate {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.fundQuota
}

func (c *MultiProviderClient) dollarBudgetGate() DollarBudgetGate {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dollarBudget
}

func (c *MultiProviderClient) ownerLimiter() *OwnerLimiter {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.limiter
}

func (c *MultiProviderClient) chatCache() *ChatCache {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cache
}

func (c *MultiProviderClient) callBudget() *CallBudgetLimiter {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.budget
}

// ---------------------------------------------------------------------------
// Chat – 统一入口
// ---------------------------------------------------------------------------

// Chat 发送聊天请求。
//
// Flow:
//  1. First attempt: route via primary provider for the resolved tier.
//  2. On failover-eligible error (circuit-open, 5xx, network), F15
//     iterates the configured TierChains, substituting req.Model with
//     the platform-default model for each fallback provider. Each
//     attempt is independent (own cache check, breaker check, billing).
//  3. Returns the first success, or the last error after all attempts.
//
// Failover is enabled by SetFailoverConfig. When no failover config is
// installed (or the chain is empty), Chat behaves exactly like a single
// call to chatOnce — identical to pre-F15 behaviour.
func (c *MultiProviderClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	resp, err := c.chatOnce(ctx, req)
	if err == nil || !ShouldFailover(err) {
		return resp, err
	}

	failover := c.failoverConfig()
	if failover.maxAttempts() <= 1 || len(failover.TierChains) == 0 {
		return resp, err
	}

	// Determine which (tier, provider) just failed. ResolveModel is
	// pure for the purposes of this call (no I/O beyond a DB-free in-
	// memory lookup), so re-running is cheap.
	origConfig, resolveErr := c.router.ResolveModel(ctx, &req)
	if resolveErr != nil || origConfig == nil {
		return resp, err
	}
	tier := origConfig.ResolvedTier
	currentProvider := origConfig.Provider

	owner := req.EffectiveOwner()
	for attempts := 1; attempts < failover.maxAttempts(); attempts++ {
		nextProvider, ok := failover.nextProvider(tier, currentProvider)
		if !ok {
			break
		}
		fallbackModel := fallbackModelFor(tier, nextProvider)
		currentProvider = nextProvider
		if fallbackModel == "" {
			// No registered model for this (tier, provider). Move on
			// to the next entry in the chain instead of erroring out.
			continue
		}

		if c.observer != nil {
			c.observer.ObserveLLM(string(nextProvider), fallbackModel, req.StepName, "failover_attempt", 0)
		}
		slog.Warn("llm chat failing over",
			"step", req.StepName,
			"owner", owner,
			"from_provider", origConfig.Provider,
			"to_provider", nextProvider,
			"to_model", fallbackModel,
			"attempt", attempts+1,
			"prev_error", err,
		)

		forkReq := req
		forkReq.Model = fallbackModel

		resp2, err2 := c.chatOnce(ctx, forkReq)
		if err2 == nil {
			if c.observer != nil {
				c.observer.ObserveLLM(string(nextProvider), fallbackModel, req.StepName, "failover_success", 0)
			}
			return resp2, nil
		}
		if !ShouldFailover(err2) {
			// Permanent error from the fallback — surface it directly
			// rather than continuing to burn the chain.
			return nil, err2
		}
		err = err2
	}
	return resp, err
}

// chatOnce executes one provider attempt. Extracted from Chat so the F15
// failover wrapper can call it repeatedly with different fallback model
// names. No retries, no chain semantics — exactly one provider call.
func (c *MultiProviderClient) chatOnce(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	// 1. 路由
	config, err := c.router.ResolveModel(ctx, &req)
	if err != nil {
		return nil, err
	}
	if config == nil {
		return nil, fmt.Errorf("llm: failed to resolve model for request (tier=%s, step=%s)", req.ModelTier, req.StepName)
	}
	return c.chatWithResolvedConfig(ctx, req, config)
}

// ChatWithConfig executes a chat call against a pre-built ModelConfig,
// bypassing the router. The full cache / rate-limit / budget / quota /
// observability / usage-record pipeline is applied normally.
//
// Used by the Sprint 10.2 ShadowDispatcher to fire arm-specific calls
// against fully-formed configs (arm.BaseURL, arm.Temperature, …) that
// the router's findModelByName path couldn't reach with a model-name
// lookup alone. Failover is NOT applied for shadow calls — a single
// attempt is enough for comparison and a multi-provider chain would
// muddy the per-arm attribution.
func (c *MultiProviderClient) ChatWithConfig(ctx context.Context, req ChatRequest, config *ModelConfig) (*ChatResponse, error) {
	if c == nil {
		return nil, fmt.Errorf("llm: nil MultiProviderClient")
	}
	if config == nil {
		return nil, fmt.Errorf("llm: ChatWithConfig requires a non-nil ModelConfig")
	}
	return c.chatWithResolvedConfig(ctx, req, config)
}

// chatWithResolvedConfig is the post-resolve body of chatOnce, factored
// out so both the router-driven path and ChatWithConfig can share it
// without duplicating cache / quota / budget / billing / observability
// code. Called with config already validated for non-nil.
func (c *MultiProviderClient) chatWithResolvedConfig(ctx context.Context, req ChatRequest, config *ModelConfig) (*ChatResponse, error) {
	var err error
	if config.APIKey == "" {
		// Wrap the missing-key error in the sentinel so the
		// failover layer can route around an agent whose
		// provider key was never configured. This is the most
		// common cause of "every researcher failed" in local /
		// staging envs where a single API key (LLM_API_KEY) is
		// configured but per-agent overrides point at provider-
		// specific keys (CLAUDE_API_KEY, OPENAI_API_KEY, ...)
		// that the operator hasn't set.
		return nil, fmt.Errorf("llm: no API key available for provider %s: %w", config.Provider, ErrMissingCredentials)
	}

	cache := c.chatCache()
	cacheKey := ""
	var resp *ChatResponse
	var lease *ChatCacheLease
	if cache != nil {
		cacheKey = buildChatCacheKey(req, config)
		if cached, ok := cache.Get(cacheKey); ok {
			cached.Model = config.ModelName
			cached.Provider = string(config.Provider)
			if c.observer != nil {
				c.observer.ObserveLLM(string(config.Provider), config.ModelName, req.StepName, "cache_hit", 0)
			}
			return cached, nil
		}
		lease = cache.Acquire(cacheKey)
		if lease != nil && !lease.IsLeader() {
			cached, waitErr := lease.Wait()
			if waitErr != nil {
				if c.observer != nil {
					c.observer.ObserveLLM(string(config.Provider), config.ModelName, req.StepName, "coalesced_error", 0)
				}
				return nil, waitErr
			}
			if cached != nil {
				cached.Model = config.ModelName
				cached.Provider = string(config.Provider)
			}
			if c.observer != nil {
				c.observer.ObserveLLM(string(config.Provider), config.ModelName, req.StepName, "coalesced_hit", 0)
			}
			return cached, nil
		}
	}
	if lease != nil && lease.IsLeader() {
		defer func() { lease.Finish(resp, err) }()
	}

	// 2. 配额 / 熔断（按 owner+provider 隔离）。limiter 为 nil 时跳过。
	// Trusted scheduler entry points (see WithLimiterBypass) opt out of
	// the per-owner rate limiter and per-step budget — those caps are
	// sized for interactive HTTP traffic and would starve a batch run
	// like daily_picks (4 watchlists × 50 symbols × 9 masters).
	// Cost gates (dollarBudgetGate / fundQuotaGate) below are NOT
	// bypassed; cost protection always applies.
	owner := req.EffectiveOwner()
	bypassLimiter := limiterBypassed(ctx)
	limiter := c.ownerLimiter()
	if limiter != nil && !bypassLimiter {
		if allowErr := limiter.Allow(owner, string(config.Provider)); allowErr != nil {
			err = allowErr
			if c.observer != nil {
				status := "rate_limited"
				if IsCircuitOpen(err) {
					status = "circuit_open"
				}
				c.observer.ObserveLLM(string(config.Provider), config.ModelName, req.StepName, status, 0)
			}
			return nil, err
		}
	}
	budget := c.callBudget()
	if budget != nil && !bypassLimiter {
		if budgetErr := budget.Allow(owner, req.StepName); budgetErr != nil {
			err = budgetErr
			if c.observer != nil {
				c.observer.ObserveLLM(string(config.Provider), config.ModelName, req.StepName, "budget_exceeded", 0)
			}
			return nil, err
		}
	}
	// F14: dollar-cap hard gate. Fires when cumulative price_cents for
	// the (user, fund) window meets the configured limit. Returning
	// here BEFORE the provider call is the only safe place — recording
	// usage after a successful call can't prevent the call itself.
	if dollarGate := c.dollarBudgetGate(); dollarGate != nil {
		if gateErr := dollarGate.Check(ctx, owner, req.FundID); gateErr != nil {
			err = gateErr
			if c.observer != nil {
				c.observer.ObserveLLM(string(config.Provider), config.ModelName, req.StepName, "dollar_budget_exceeded", 0)
			}
			slog.Warn("llm dollar budget exceeded",
				"owner", owner,
				"fund_id", req.FundID,
				"step", req.StepName,
				"err", err,
			)
			return nil, err
		}
	}
	// F28: per-fund token quota. Same return-before-call rationale as
	// the dollar gate. requestedTokens is a pessimistic upper bound:
	// max_tokens (output) + an estimate of the prompt size. When
	// MaxTokens is 0 we still gate on a conservative 4096 to keep
	// runaway prompts in check.
	if quotaGate := c.fundQuotaGate(); quotaGate != nil && req.FundID != "" {
		estimated := int64(req.MaxTokens)
		if estimated <= 0 {
			estimated = 4096
		}
		estimated += int64(estimatePromptTokens(req))
		if gateErr := quotaGate.CheckLLMTokens(ctx, req.FundID, estimated); gateErr != nil {
			err = gateErr
			if c.observer != nil {
				c.observer.ObserveLLM(string(config.Provider), config.ModelName, req.StepName, "fund_token_quota_exceeded", 0)
			}
			slog.Warn("llm fund token quota exceeded",
				"fund_id", req.FundID,
				"step", req.StepName,
				"estimated_tokens", estimated,
				"err", err,
			)
			return nil, err
		}
	}

	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > llmTotalRequestTimeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, llmTotalRequestTimeout)
		defer cancel()
	}

	start := time.Now()

	// 3. 调用
	switch {
	case config.Provider.UsesOpenAIChatCompletions():
		resp, err = c.callOpenAI(ctx, config, req)
	case config.Provider.UsesClaudeMessagesAPI():
		resp, err = c.callClaude(ctx, config, req)
	case config.Provider.UsesGeminiGenerateContent():
		resp, err = c.callGemini(ctx, config, req)
	default:
		err = fmt.Errorf("llm: unsupported provider %s", config.Provider)
	}

	latency := time.Since(start)
	if err != nil {
		// 仅"传输/服务端"类错误计入熔断器（429/5xx/超时），避免请求格式错把熔断打爆。
		if limiter != nil && shouldTripBreaker(err) {
			limiter.RecordFailure(owner, string(config.Provider))
		}
		if c.observer != nil {
			c.observer.ObserveLLM(string(config.Provider), config.ModelName, req.StepName, "error", latency)
		}
		slog.Warn("llm chat failed",
			"provider", config.Provider,
			"model", config.ModelName,
			"step", req.StepName,
			"owner", owner,
			"latency_ms", latency.Milliseconds(),
			"error", err,
		)
		return nil, err
	}
	if limiter != nil {
		limiter.RecordSuccess(owner, string(config.Provider))
	}
	if c.observer != nil {
		c.observer.ObserveLLM(string(config.Provider), config.ModelName, req.StepName, "success", latency)
	}

	// 4. 补充元信息
	resp.LatencyMs = latency.Milliseconds()
	resp.Model = config.ModelName
	resp.Provider = string(config.Provider)
	resp.TotalTokens = resp.InputTokens + resp.OutputTokens

	// 5. 计费
	resp.InputCost, resp.OutputCost, resp.TotalCost = CalcCost(config, resp.InputTokens, resp.OutputTokens)
	resp.InputPrice, resp.OutputPrice, resp.TotalPrice = CalcPrice(config, resp.InputTokens, resp.OutputTokens)

	// F28: per-fund token usage roll-up. Runs synchronously (small,
	// indexed UPSERT) so the next Check sees correct totals. Failures
	// are logged but don't surface to the caller — the call already
	// succeeded and we'd rather under-account than fail user-facing
	// requests on an audit-table write.
	if quotaGate := c.fundQuotaGate(); quotaGate != nil && req.FundID != "" {
		if recErr := quotaGate.RecordLLMTokens(ctx, req.FundID, int64(resp.InputTokens), int64(resp.OutputTokens)); recErr != nil {
			slog.Warn("llm fund token usage record failed", "fund_id", req.FundID, "error", recErr)
		}
	}

	// 6. 异步记录用量
	if recorder := c.router.GetUsageRecorder(); recorder != nil {
		record := &UsageRecord{
			UserID:       owner,
			FundID:       req.FundID,
			AgentID:      req.AgentID,
			StepName:     req.StepName,
			Tier:         string(config.ResolvedTier),
			Model:        config.ModelName,
			Provider:     string(config.Provider),
			InputTokens:  resp.InputTokens,
			OutputTokens: resp.OutputTokens,
			Cost:         resp.TotalCost,
			Price:        resp.TotalPrice,
			IsCustomKey:  config.UsesCustomKey,
			CreatedAt:    time.Now(),
		}
		go func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if recErr := recorder.Record(bgCtx, record); recErr != nil {
				slog.Warn("llm usage record failed", "provider", config.Provider, "model", config.ModelName, "error", recErr)
			}
		}()
	}

	if cache != nil {
		cache.Set(cacheKey, req.StepName, resp)
	}

	return resp, nil
}

// shouldTripBreaker 判断该错误是否应当计入熔断器失败计数。
// 我们只对"提供方真的不行了"的错误打分：5xx、429、超时、传输错误；
// 4xx 客户端错误（如 invalid_request）不应触发熔断。
func shouldTripBreaker(err error) bool {
	var reqErr *llmRequestError
	if errors.As(err, &reqErr) {
		switch reqErr.Reason {
		case "rate_limited", "server_error", "timeout", "transport_error":
			return true
		}
		return false
	}
	return false
}

// ---------------------------------------------------------------------------
// OpenAI-Compatible 调用（OpenAI / DeepSeek / Qwen / Custom）
// ---------------------------------------------------------------------------

// openAIRequest OpenAI chat/completions 请求体。
type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature float64         `json:"temperature,omitempty"`
	Stream      bool            `json:"stream"`

	// ResponseFormat is OpenAI's native structured-output knob.
	// Two shapes are accepted:
	//   {"type":"json_object"}
	//   {"type":"json_schema","json_schema":{"name":"out","strict":true,"schema":{...}}}
	// Wired by callOpenAI() from ChatRequest.ResponseFormat /
	// ChatRequest.ResponseSchema. DeepSeek / Qwen / Custom share
	// this path (UsesOpenAIChatCompletions). Providers that don't
	// support the field must be handled before marshaling this body.
	ResponseFormat *openAIResponseFormat `json:"response_format,omitempty"`
}

type openAIResponseFormat struct {
	Type       string                  `json:"type"`
	JSONSchema *openAIResponseFormatJS `json:"json_schema,omitempty"`
}

type openAIResponseFormatJS struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openAIResponse OpenAI chat/completions 响应体。
type openAIResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *openAIError `json:"error,omitempty"`
}

type openAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

type retryPolicy struct {
	maxAttempts    int
	attemptTimeout time.Duration
	baseBackoff    time.Duration
	maxBackoff     time.Duration
}

type llmRequestError struct {
	Provider   Provider
	Model      string
	StatusCode int
	Retryable  bool
	Reason     string
	Message    string
}

// RequestError is the exported type alias for llmRequestError so other
// packages (decision/errorclass, in particular) can errors.As() into a
// concrete shape with Provider / Model / Reason / StatusCode fields
// without paying the API-stability cost of moving the unexported
// struct definition. The classifier reads Reason + StatusCode to map
// raw provider failures into the bounded user-facing category set.
type RequestError = llmRequestError

func (e *llmRequestError) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("llm: %s %s failed (%s, status=%d): %s", e.Provider, e.Model, e.Reason, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("llm: %s %s failed (%s): %s", e.Provider, e.Model, e.Reason, e.Message)
}

func defaultRetryPolicy() retryPolicy {
	return retryPolicy{
		maxAttempts:    3,
		attemptTimeout: llmAttemptTimeout,
		baseBackoff:    500 * time.Millisecond,
		maxBackoff:     3 * time.Second,
	}
}

func appendOpenAIJSONInstruction(messages []openAIMessage, format string, schema json.RawMessage) []openAIMessage {
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" {
		return messages
	}

	instruction := "Return ONLY a JSON object, no prose, no markdown fences."
	if format == "json_schema" && len(schema) > 0 {
		// The caller's system prompt already contains the human-readable
		// output contract. Do NOT paste the full JSON Schema here: for
		// providers without native response_format support (notably
		// DeepSeek), duplicating the schema inflates the prompt and makes
		// truncated half-JSON replies much more likely. A concise JSON-only
		// reminder gives the tolerant downstream parser the best chance.
		instruction = "Return ONLY one complete JSON object matching the requested schema, no prose, no markdown fences. Finish the JSON object completely."
	}

	for i := range messages {
		if strings.EqualFold(strings.TrimSpace(messages[i].Role), "system") {
			if strings.TrimSpace(messages[i].Content) == "" {
				messages[i].Content = instruction
			} else {
				messages[i].Content += "\n\n" + instruction
			}
			return messages
		}
	}

	return append([]openAIMessage{{Role: "system", Content: instruction}}, messages...)
}

func (c *MultiProviderClient) callOpenAI(ctx context.Context, config *ModelConfig, req ChatRequest) (*ChatResponse, error) {
	messages := make([]openAIMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		messages = append(messages, openAIMessage{Role: m.Role, Content: m.Content})
	}

	// DeepSeek's OpenAI-compatible endpoint currently rejects the
	// response_format field for some models (for example
	// deepseek-v4-pro returns "This response_format type is unavailable
	// now"). Keep structured-output intent by moving the JSON-only
	// requirement into the prompt, like the Claude fallback path does,
	// instead of sending an unsupported request field.
	nativeResponseFormat := config.Provider != ProviderDeepSeek
	if !nativeResponseFormat {
		messages = appendOpenAIJSONInstruction(messages, req.ResponseFormat, req.ResponseSchema)
	}

	body := openAIRequest{
		Model:       config.ModelName,
		Messages:    messages,
		MaxTokens:   config.MaxTokens,
		Temperature: config.Temperature,
		Stream:      false,
	}

	// S8.3 — wire structured output. We treat "json_schema" with
	// a non-empty schema as the strict variant; everything else
	// falls back to the looser "json_object" mode (any JSON shape).
	if nativeResponseFormat {
		switch strings.ToLower(strings.TrimSpace(req.ResponseFormat)) {
		case "json_schema":
			if len(req.ResponseSchema) > 0 {
				body.ResponseFormat = &openAIResponseFormat{
					Type: "json_schema",
					JSONSchema: &openAIResponseFormatJS{
						Name:   "out",
						Strict: true,
						Schema: append(json.RawMessage(nil), req.ResponseSchema...),
					},
				}
			} else {
				body.ResponseFormat = &openAIResponseFormat{Type: "json_object"}
			}
		case "json_object":
			body.ResponseFormat = &openAIResponseFormat{Type: "json_object"}
		}
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal openai request: %w", err)
	}

	endpoint := strings.TrimRight(config.BaseURL, "/") + "/chat/completions"
	headers := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + config.APIKey,
	}
	logDebugCurl(config.Provider, config.ModelName, endpoint, headers, bodyBytes)
	policy := defaultRetryPolicy()

	respBody, err := c.doWithRetry(ctx, policy, config, func(attemptCtx context.Context) ([]byte, int, error) {
		httpReq, reqErr := http.NewRequestWithContext(attemptCtx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
		if reqErr != nil {
			return nil, 0, fmt.Errorf("llm: create openai request: %w", reqErr)
		}
		for key, value := range headers {
			httpReq.Header.Set(key, value)
		}
		return c.executeRequest(attemptCtx, httpReq, config)
	})
	if err != nil {
		return nil, err
	}

	var oaiResp openAIResponse
	if err = json.Unmarshal(respBody, &oaiResp); err != nil {
		return nil, fmt.Errorf("llm: unmarshal openai response: %w", err)
	}
	if oaiResp.Error != nil && oaiResp.Error.Message != "" {
		return nil, &llmRequestError{
			Provider:  config.Provider,
			Model:     config.ModelName,
			Reason:    "provider_error",
			Message:   oaiResp.Error.Message,
			Retryable: false,
		}
	}
	if len(oaiResp.Choices) == 0 {
		return nil, &llmRequestError{Provider: config.Provider, Model: config.ModelName, Reason: "empty_choices", Message: "provider returned empty choices", Retryable: false}
	}

	return &ChatResponse{
		Content:      oaiResp.Choices[0].Message.Content,
		InputTokens:  oaiResp.Usage.PromptTokens,
		OutputTokens: oaiResp.Usage.CompletionTokens,
	}, nil
}

// ---------------------------------------------------------------------------
// Claude Messages API 调用
// ---------------------------------------------------------------------------

// claudeRequest Claude Messages API 请求体。
type claudeRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    string          `json:"system,omitempty"`
	Messages  []claudeMessage `json:"messages"`
}

type claudeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// claudeResponse Claude Messages API 响应体。
type claudeResponse struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *claudeError `json:"error,omitempty"`
}

type claudeError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (c *MultiProviderClient) callClaude(ctx context.Context, config *ModelConfig, req ChatRequest) (*ChatResponse, error) {
	var systemPrompt string
	messages := make([]claudeMessage, 0, len(req.Messages))

	for _, m := range req.Messages {
		if m.Role == "system" {
			if systemPrompt != "" {
				systemPrompt += "\n\n"
			}
			systemPrompt += m.Content
			continue
		}
		messages = append(messages, claudeMessage{Role: m.Role, Content: m.Content})
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("llm: claude requires at least one user message")
	}

	// S8.3 — Claude Messages API has no response_format field; the
	// idiomatic structured-output path is tool_use, which is a much
	// larger refactor (multiple response shapes). For the
	// ResponseFormat hook we instead pre-pend a strict instruction
	// to the system prompt — this is the same trick Anthropic
	// recommends in the prompt engineering guide when tool_use is
	// not desired. The downstream tolerant parser in
	// agent/analyst.go already strips ```json fences.
	switch strings.ToLower(strings.TrimSpace(req.ResponseFormat)) {
	case "json_schema":
		if len(req.ResponseSchema) > 0 {
			schemaText := strings.TrimSpace(string(req.ResponseSchema))
			schemaLine := "Return ONLY a JSON object matching this JSON Schema, no prose, no markdown fences:\n" + schemaText
			if systemPrompt == "" {
				systemPrompt = schemaLine
			} else {
				systemPrompt = systemPrompt + "\n\n" + schemaLine
			}
		} else if systemPrompt == "" {
			systemPrompt = "Return ONLY a JSON object, no prose, no markdown fences."
		} else {
			systemPrompt += "\n\nReturn ONLY a JSON object, no prose, no markdown fences."
		}
	case "json_object":
		if systemPrompt == "" {
			systemPrompt = "Return ONLY a JSON object, no prose, no markdown fences."
		} else {
			systemPrompt += "\n\nReturn ONLY a JSON object, no prose, no markdown fences."
		}
	}

	body := claudeRequest{Model: config.ModelName, MaxTokens: config.MaxTokens, System: systemPrompt, Messages: messages}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal claude request: %w", err)
	}

	endpoint := strings.TrimRight(config.BaseURL, "/") + "/messages"
	headers := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         config.APIKey,
		"anthropic-version": "2023-06-01",
	}
	logDebugCurl(config.Provider, config.ModelName, endpoint, headers, bodyBytes)
	policy := defaultRetryPolicy()

	respBody, err := c.doWithRetry(ctx, policy, config, func(attemptCtx context.Context) ([]byte, int, error) {
		httpReq, reqErr := http.NewRequestWithContext(attemptCtx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
		if reqErr != nil {
			return nil, 0, fmt.Errorf("llm: create claude request: %w", reqErr)
		}
		for key, value := range headers {
			httpReq.Header.Set(key, value)
		}
		return c.executeRequest(attemptCtx, httpReq, config)
	})
	if err != nil {
		return nil, err
	}

	var cResp claudeResponse
	if err = json.Unmarshal(respBody, &cResp); err != nil {
		return nil, fmt.Errorf("llm: unmarshal claude response: %w", err)
	}
	if cResp.Error != nil && cResp.Error.Message != "" {
		return nil, &llmRequestError{Provider: config.Provider, Model: config.ModelName, Reason: "provider_error", Message: cResp.Error.Message, Retryable: false}
	}

	var contentParts []string
	for _, block := range cResp.Content {
		if block.Type == "text" {
			contentParts = append(contentParts, block.Text)
		}
	}
	if len(contentParts) == 0 {
		return nil, &llmRequestError{Provider: config.Provider, Model: config.ModelName, Reason: "empty_content", Message: "provider returned empty content", Retryable: false}
	}

	return &ChatResponse{
		Content:      strings.Join(contentParts, ""),
		InputTokens:  cResp.Usage.InputTokens,
		OutputTokens: cResp.Usage.OutputTokens,
	}, nil
}

type geminiRequest struct {
	SystemInstruction *geminiInstruction      `json:"system_instruction,omitempty"`
	Contents          []geminiContent         `json:"contents"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig,omitempty"`
}

type geminiInstruction struct {
	Parts []geminiPart `json:"parts"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text,omitempty"`
}

type geminiGenerationConfig struct {
	Temperature     float64 `json:"temperature,omitempty"`
	MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`

	// S8.3 — native structured output. responseMimeType must be
	// "application/json" for json_object / json_schema modes;
	// responseSchema, when set, makes Gemini enforce the schema.
	ResponseMimeType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   json.RawMessage `json:"responseSchema,omitempty"`

	// ThinkingConfig is opt-in for Gemini 2.5+/3.x preview models
	// that emit internal reasoning tokens. Setting ThinkingBudget
	// caps how many tokens the model is allowed to spend thinking
	// before it has to emit the visible answer. Without a cap the
	// model can blow through MaxOutputTokens entirely on hidden
	// thoughts and return an empty `text` field (finishReason =
	// MAX_TOKENS), which manifests downstream as "no json object
	// found" parse failures. Pointer so the field is omitted on
	// non-thinking models that would 400 on the unknown key.
	ThinkingConfig *geminiThinkingConfig `json:"thinkingConfig,omitempty"`
}

type geminiThinkingConfig struct {
	// ThinkingBudget is the max tokens the model may spend thinking
	// before it must emit the final answer. Use -1 for "dynamic"
	// (model chooses), 0 to disable thinking entirely, or a positive
	// integer to cap. We default to a positive cap so a single
	// runaway thought chain can't truncate the visible response.
	ThinkingBudget int `json:"thinkingBudget"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
		ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
	} `json:"usageMetadata"`
	Error *struct {
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error,omitempty"`
}

// geminiUnsupportedSchemaKeys are JSON-Schema keywords that Gemini's
// responseSchema (an OpenAPI 3.0 Schema subset) does not recognize.
// Sending any of these makes Gemini reject the whole request with
// "Unknown name <key> at 'generation_config.response_schema'" (400).
//
// The list is conservative: we drop only keys that are confirmed
// unsupported, leaving OpenAPI-flavoured keys (type, properties,
// required, items, enum, format, minimum, maximum, minItems, maxItems,
// description, nullable, anyOf, etc.) untouched.
var geminiUnsupportedSchemaKeys = map[string]struct{}{
	"additionalProperties":   {},
	"$schema":                {},
	"$id":                    {},
	"$ref":                   {},
	"$defs":                  {},
	"$comment":               {},
	"definitions":            {},
	"patternProperties":      {},
	"unevaluatedProperties":  {},
	"unevaluatedItems":       {},
	"dependentRequired":      {},
	"dependentSchemas":       {},
	"if":                     {},
	"then":                   {},
	"else":                   {},
	"contains":               {},
	"minContains":            {},
	"maxContains":            {},
	"prefixItems":            {},
	"examples":               {},
}

// sanitizeGeminiSchema rewrites a JSON-Schema document into a form
// Gemini's responseSchema accepts. It drops the keys listed in
// geminiUnsupportedSchemaKeys at every nesting level and leaves the
// rest of the structure intact. On any parse error the original bytes
// are returned as-is — the upstream 400 is more diagnostic than a
// silent re-shape.
func sanitizeGeminiSchema(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return append(json.RawMessage(nil), raw...)
	}
	cleaned := stripUnsupportedKeys(node, geminiUnsupportedSchemaKeys)
	out, err := json.Marshal(cleaned)
	if err != nil {
		return append(json.RawMessage(nil), raw...)
	}
	return out
}

// stripUnsupportedKeys walks the decoded JSON tree depth-first and
// removes every map entry whose key is in deny. It returns a new tree
// rather than mutating in place so callers can keep the original.
func stripUnsupportedKeys(node any, deny map[string]struct{}) any {
	switch v := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, child := range v {
			if _, drop := deny[k]; drop {
				continue
			}
			out[k] = stripUnsupportedKeys(child, deny)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = stripUnsupportedKeys(child, deny)
		}
		return out
	default:
		return v
	}
}

// isGeminiThinkingModel reports whether a Gemini model name belongs
// to a family that emits internal reasoning tokens
// (`usageMetadata.thoughtsTokenCount > 0`). For these models the
// `maxOutputTokens` budget covers thinking + visible answer
// combined, so a fixed 4096 cap on a chatty 2000-token system
// prompt can leave zero tokens for the JSON reply and silently
// truncate the response to empty.
//
// Covers the families currently in production at Google + relays:
//   - gemini-2.5-* (pro/flash thinking)
//   - gemini-3.x-* (pro/flash preview, thinking-by-default)
//
// We deliberately match on family prefixes rather than the full
// model name so newer point releases pick up the bigger envelope
// automatically.
func isGeminiThinkingModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return false
	}
	thinkingPrefixes := []string{
		"gemini-2.5",
		"gemini-3.",
		"gemini-3-",
	}
	for _, p := range thinkingPrefixes {
		if strings.HasPrefix(m, p) {
			return true
		}
	}
	return false
}

// geminiThinkingBudget returns the (maxOutputTokens, thinkingBudget)
// pair to send to Gemini for a thinking-capable model given the
// caller's requested cap. The contract is:
//
//   - the *visible* answer must always have at least
//     geminiMinVisibleBudgetTokens to spend, even if thinking
//     consumes its full budget
//   - thinking should be capped at geminiMaxThinkingBudgetTokens so
//     a runaway chain-of-thought can't burn the entire envelope
//   - we never *shrink* what the caller asked for: if the caller
//     asked for 16384 tokens we keep 16384
//
// callerMax == 0 means "let the provider default win"; we still
// return a sane floor in that case so the visible answer is
// protected.
func geminiThinkingBudget(callerMax int) (maxOutput, thinkingCap int) {
	const (
		minVisible        = 3072
		defaultThinkCap   = 4096
		thinkingHeadroom  = 4096
		defaultOutputBase = 8192
	)
	thinkingCap = defaultThinkCap
	switch {
	case callerMax <= 0:
		maxOutput = defaultOutputBase
	case callerMax < minVisible+thinkingHeadroom:
		// Caller asked for too small an envelope to fit both a
		// useful thinking pass and a useful answer. Bump it.
		maxOutput = minVisible + thinkingHeadroom
	default:
		maxOutput = callerMax
	}
	return maxOutput, thinkingCap
}

func (c *MultiProviderClient) callGemini(ctx context.Context, config *ModelConfig, req ChatRequest) (*ChatResponse, error) {
	var systemPrompt string
	contents := make([]geminiContent, 0, len(req.Messages))
	for _, message := range req.Messages {
		trimmed := strings.TrimSpace(message.Content)
		if trimmed == "" {
			continue
		}
		if message.Role == "system" {
			if systemPrompt != "" {
				systemPrompt += "\n\n"
			}
			systemPrompt += trimmed
			continue
		}
		role := "user"
		if message.Role == "assistant" {
			role = "model"
		}
		contents = append(contents, geminiContent{Role: role, Parts: []geminiPart{{Text: trimmed}}})
	}
	if len(contents) == 0 {
		return nil, fmt.Errorf("llm: gemini requires at least one non-system message")
	}

	body := geminiRequest{Contents: contents}
	if strings.TrimSpace(systemPrompt) != "" {
		body.SystemInstruction = &geminiInstruction{Parts: []geminiPart{{Text: systemPrompt}}}
	}
	// S8.3 — Gemini structured output.
	wantsJSONObject := strings.EqualFold(strings.TrimSpace(req.ResponseFormat), "json_object")
	wantsJSONSchema := strings.EqualFold(strings.TrimSpace(req.ResponseFormat), "json_schema")
	isThinking := isGeminiThinkingModel(config.ModelName)
	needGenCfg := config.MaxTokens > 0 || config.Temperature > 0 || wantsJSONObject || wantsJSONSchema || isThinking
	if needGenCfg {
		body.GenerationConfig = &geminiGenerationConfig{}
		if isThinking {
			// Thinking-capable Gemini models (2.5+/3.x) count their
			// internal reasoning tokens against maxOutputTokens. A
			// chatty 2000-token persona prompt easily provokes a
			// 3000+ token thought chain, which on a 4096 cap
			// truncates the visible JSON answer to empty and surfaces
			// downstream as "no json object found" parse failures.
			// Bump the envelope and cap thinking explicitly so the
			// visible response always has headroom.
			maxOut, thinkCap := geminiThinkingBudget(config.MaxTokens)
			body.GenerationConfig.MaxOutputTokens = maxOut
			body.GenerationConfig.ThinkingConfig = &geminiThinkingConfig{
				ThinkingBudget: thinkCap,
			}
		} else if config.MaxTokens > 0 {
			body.GenerationConfig.MaxOutputTokens = config.MaxTokens
		}
		if config.Temperature > 0 {
			body.GenerationConfig.Temperature = config.Temperature
		}
		if wantsJSONObject || wantsJSONSchema {
			body.GenerationConfig.ResponseMimeType = "application/json"
		}
		if wantsJSONSchema && len(req.ResponseSchema) > 0 {
			// Gemini's responseSchema is a strict subset of OpenAPI 3.0
			// Schema and rejects vanilla JSON-Schema keywords with
			// "Unknown name ... at 'generation_config.response_schema'"
			// (400 invalid_request). Strip the unsupported keywords so
			// the agent layer can keep one OpenAI-compatible schema and
			// not have to fork per provider.
			body.GenerationConfig.ResponseSchema = sanitizeGeminiSchema(req.ResponseSchema)
		}
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal gemini request: %w", err)
	}

	endpoint := buildGeminiGenerateContentURL(config.BaseURL, config.ModelName)
	headers := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + config.APIKey,
	}
	logDebugCurl(config.Provider, config.ModelName, endpoint, headers, bodyBytes)
	policy := defaultRetryPolicy()
	respBody, err := c.doWithRetry(ctx, policy, config, func(attemptCtx context.Context) ([]byte, int, error) {
		httpReq, reqErr := http.NewRequestWithContext(attemptCtx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
		if reqErr != nil {
			return nil, 0, fmt.Errorf("llm: create gemini request: %w", reqErr)
		}
		for key, value := range headers {
			httpReq.Header.Set(key, value)
		}
		return c.executeRequest(attemptCtx, httpReq, config)
	})
	if err != nil {
		return nil, err
	}

	var gResp geminiResponse
	if err = json.Unmarshal(respBody, &gResp); err != nil {
		return nil, fmt.Errorf("llm: unmarshal gemini response: %w", err)
	}
	if gResp.Error != nil && gResp.Error.Message != "" {
		return nil, &llmRequestError{Provider: config.Provider, Model: config.ModelName, Reason: "provider_error", Message: gResp.Error.Message, Retryable: false}
	}
	if len(gResp.Candidates) == 0 {
		return nil, &llmRequestError{Provider: config.Provider, Model: config.ModelName, Reason: "empty_candidates", Message: "provider returned empty candidates", Retryable: false}
	}

	cand := gResp.Candidates[0]
	var contentParts []string
	for _, part := range cand.Content.Parts {
		if text := strings.TrimSpace(part.Text); text != "" {
			contentParts = append(contentParts, text)
		}
	}
	if len(contentParts) == 0 {
		// Empty visible content on a thinking model almost always means
		// the model spent the whole maxOutputTokens budget on internal
		// thinking and had nothing left to emit. Surface that as a
		// distinct retryable error so the operator can tell it apart
		// from a model that genuinely refused / safety-blocked, and so
		// we can retry with a bigger budget next time.
		if strings.EqualFold(cand.FinishReason, "MAX_TOKENS") {
			return nil, &llmRequestError{
				Provider: config.Provider,
				Model:    config.ModelName,
				Reason:   "max_tokens_truncated",
				Message: fmt.Sprintf(
					"provider truncated response: thoughts=%d, candidates=%d, total=%d, maxOutputTokens=%d (visible text was empty)",
					gResp.UsageMetadata.ThoughtsTokenCount,
					gResp.UsageMetadata.CandidatesTokenCount,
					gResp.UsageMetadata.TotalTokenCount,
					body.GenerationConfig.MaxOutputTokens,
				),
				Retryable: true,
			}
		}
		return nil, &llmRequestError{
			Provider: config.Provider,
			Model:    config.ModelName,
			Reason:   "empty_content",
			Message: fmt.Sprintf("provider returned empty content (finishReason=%q, thoughts=%d, candidates=%d)",
				cand.FinishReason,
				gResp.UsageMetadata.ThoughtsTokenCount,
				gResp.UsageMetadata.CandidatesTokenCount,
			),
			Retryable: false,
		}
	}

	inputTokens := gResp.UsageMetadata.PromptTokenCount
	outputTokens := gResp.UsageMetadata.CandidatesTokenCount
	if outputTokens == 0 && gResp.UsageMetadata.TotalTokenCount > inputTokens {
		outputTokens = gResp.UsageMetadata.TotalTokenCount - inputTokens
	}

	return &ChatResponse{
		Content:      strings.Join(contentParts, "\n"),
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
	}, nil
}

func (c *MultiProviderClient) doWithRetry(
	ctx context.Context,
	policy retryPolicy,
	config *ModelConfig,
	call func(context.Context) ([]byte, int, error),
) ([]byte, error) {
	if policy.maxAttempts <= 0 {
		policy.maxAttempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= policy.maxAttempts; attempt++ {
		attemptCtx := ctx
		cancel := func() {}
		if policy.attemptTimeout > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, policy.attemptTimeout)
		}
		startedAt := time.Now()
		respBody, statusCode, err := call(attemptCtx)
		cancel()
		if err == nil {
			return respBody, nil
		}

		lastErr = err
		retryable := isRetryableLLMError(err)
		slog.Warn("llm attempt failed",
			"provider", config.Provider,
			"model", config.ModelName,
			"attempt", attempt,
			"max_attempts", policy.maxAttempts,
			"status", statusCode,
			"retryable", retryable,
			"latency_ms", time.Since(startedAt).Milliseconds(),
			"error", err,
		)
		if !retryable || attempt == policy.maxAttempts {
			break
		}
		backoff := nextBackoff(policy, attempt)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
	return nil, lastErr
}

func (c *MultiProviderClient) executeRequest(ctx context.Context, req *http.Request, config *ModelConfig) ([]byte, int, error) {
	httpResp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, classifyTransportError(config, err)
	}
	defer httpResp.Body.Close()

	respBody, readErr := io.ReadAll(httpResp.Body)
	if readErr != nil {
		return nil, httpResp.StatusCode, fmt.Errorf("llm: read %s response: %w", config.Provider, readErr)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, httpResp.StatusCode, classifyStatusError(config, httpResp.StatusCode, respBody)
	}
	return respBody, httpResp.StatusCode, nil
}

func classifyTransportError(config *ModelConfig, err error) error {
	reason := "transport_error"
	retryable := true
	message := err.Error()
	if errors.Is(err, context.DeadlineExceeded) {
		reason = "timeout"
		message = "request timed out"
	}
	if errors.Is(err, context.Canceled) {
		reason = "cancelled"
		retryable = false
		message = "request cancelled"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		reason = "timeout"
		message = "request timed out"
	}
	return &llmRequestError{Provider: config.Provider, Model: config.ModelName, Reason: reason, Message: message, Retryable: retryable}
}

func classifyStatusError(config *ModelConfig, statusCode int, body []byte) error {
	retryable := statusCode == http.StatusTooManyRequests || statusCode >= 500
	reason := "provider_error"
	switch {
	case statusCode == http.StatusTooManyRequests:
		reason = "rate_limited"
	case statusCode >= 500:
		reason = "server_error"
	case statusCode >= 400:
		reason = "invalid_request"
	}
	return &llmRequestError{
		Provider:   config.Provider,
		Model:      config.ModelName,
		StatusCode: statusCode,
		Retryable:  retryable,
		Reason:     reason,
		Message:    truncateBody(body, 500),
	}
}

func isRetryableLLMError(err error) bool {
	var reqErr *llmRequestError
	if errors.As(err, &reqErr) {
		return reqErr.Retryable
	}
	return false
}

func nextBackoff(policy retryPolicy, attempt int) time.Duration {
	backoff := time.Duration(attempt) * policy.baseBackoff
	if backoff <= 0 {
		backoff = 500 * time.Millisecond
	}
	if policy.maxBackoff > 0 && backoff > policy.maxBackoff {
		return policy.maxBackoff
	}
	return backoff
}

func logDebugCurl(provider Provider, modelName, endpoint string, headers map[string]string, body []byte) {
	redactedHeaders := make([]string, 0, len(headers))
	for _, key := range []string{"Authorization", "Content-Type", "anthropic-version", "x-api-key", "x-goog-api-key"} {
		value, ok := headers[key]
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		redactedHeaders = append(redactedHeaders, fmt.Sprintf("-H %q", key+": "+redactHeaderValue(key, value)))
	}

	bodyLiteral := shellSingleQuote(string(body))
	parts := []string{"curl -sS", "-X POST", fmt.Sprintf("%q", endpoint)}
	parts = append(parts, redactedHeaders...)
	parts = append(parts, "--data-raw", bodyLiteral)

	slog.Info("llm relay curl",
		"provider", provider,
		"model", modelName,
		"curl", strings.Join(parts, " "),
	)
}

func redactHeaderValue(key, value string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "authorization":
		if strings.HasPrefix(value, "Bearer ") {
			return "Bearer ***REDACTED***"
		}
		return "***REDACTED***"
	case "x-api-key", "x-goog-api-key":
		return "***REDACTED***"
	default:
		return value
	}
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func buildGeminiGenerateContentURL(baseURL, modelName string) string {
	trimmedBaseURL := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	lowerBaseURL := strings.ToLower(trimmedBaseURL)
	if strings.HasSuffix(lowerBaseURL, "/v1beta") {
		return trimmedBaseURL + "/models/" + url.PathEscape(modelName) + ":generateContent"
	}
	return trimmedBaseURL + "/v1beta/models/" + url.PathEscape(modelName) + ":generateContent"
}

// ---------------------------------------------------------------------------
// ListModels
// ---------------------------------------------------------------------------

// ListModels 列出平台所有可用模型信息。
func (c *MultiProviderClient) ListModels(_ context.Context) ([]ModelInfo, error) {
	result := make([]ModelInfo, len(PlatformModels))
	copy(result, PlatformModels)
	return result, nil
}

// ---------------------------------------------------------------------------
// 工具函数
// ---------------------------------------------------------------------------

// truncateBody 截断响应体用于错误消息展示。
func truncateBody(body []byte, maxLen int) string {
	s := string(body)
	if len(s) > maxLen {
		return s[:maxLen] + "...(truncated)"
	}
	return s
}
