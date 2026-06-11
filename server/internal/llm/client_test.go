package llm

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recordingLLMObserver struct {
	mu       sync.Mutex
	statuses []string
}

func (o *recordingLLMObserver) ObserveLLM(_, _, _, status string, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.statuses = append(o.statuses, status)
}

func (o *recordingLLMObserver) Count(status string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	count := 0
	for _, value := range o.statuses {
		if value == status {
			count++
		}
	}
	return count
}

func TestClassifyStatusErrorRetryability(t *testing.T) {
	config := &ModelConfig{Provider: ProviderOpenAI, ModelName: "test-model"}

	rateLimited := classifyStatusError(config, http.StatusTooManyRequests, []byte("slow down"))
	if !isRetryableLLMError(rateLimited) {
		t.Fatal("expected 429 error to be retryable")
	}

	badRequest := classifyStatusError(config, http.StatusBadRequest, []byte("bad input"))
	if isRetryableLLMError(badRequest) {
		t.Fatal("expected 400 error to be non-retryable")
	}
}

func TestClassifyStatusErrorTruncatesBody(t *testing.T) {
	config := &ModelConfig{Provider: ProviderOpenAI, ModelName: "test-model"}
	body := []byte(strings.Repeat("x", 600))

	err := classifyStatusError(config, http.StatusInternalServerError, body)
	var reqErr *llmRequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("expected llmRequestError, got %v", err)
	}
	if !reqErr.Retryable {
		t.Fatal("expected 500 error to be retryable")
	}
	if !strings.HasSuffix(reqErr.Message, "...(truncated)") {
		t.Fatalf("expected truncated suffix, got %q", reqErr.Message)
	}
	if len(reqErr.Message) != 514 {
		t.Fatalf("expected truncated message length 514, got %d", len(reqErr.Message))
	}
}

func TestClassifyTransportErrorRetryability(t *testing.T) {
	config := &ModelConfig{Provider: ProviderClaude, ModelName: "claude-test"}

	cancelled := classifyTransportError(config, context.Canceled)
	if isRetryableLLMError(cancelled) {
		t.Fatal("expected cancelled error to be non-retryable")
	}

	timedOut := classifyTransportError(config, context.DeadlineExceeded)
	if !isRetryableLLMError(timedOut) {
		t.Fatal("expected timeout error to be retryable")
	}
}

func TestClassifyTransportErrorNetTimeoutIsRetryable(t *testing.T) {
	config := &ModelConfig{Provider: ProviderClaude, ModelName: "claude-test"}

	err := classifyTransportError(config, timeoutNetError{msg: "i/o timeout"})
	var reqErr *llmRequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("expected llmRequestError, got %v", err)
	}
	if !reqErr.Retryable {
		t.Fatal("expected timeout net error to be retryable")
	}
	if reqErr.Reason != "timeout" {
		t.Fatalf("expected timeout reason, got %q", reqErr.Reason)
	}
}

func TestProviderDispatchClassifiers(t *testing.T) {
	if !ProviderOpenAI.UsesOpenAIChatCompletions() {
		t.Fatal("expected openai provider to use chat completions")
	}
	if !ProviderGemini.UsesGeminiGenerateContent() {
		t.Fatal("expected gemini provider to use generateContent")
	}
	if !ProviderClaude.UsesClaudeMessagesAPI() {
		t.Fatal("expected claude provider to use messages API")
	}
	if ProviderGemini.UsesOpenAIChatCompletions() {
		t.Fatal("expected gemini not to use openai protocol")
	}
}

func TestLLMTimeoutConstants(t *testing.T) {
	// All LLM-side timeouts are unified to 5 minutes — see
	// llm/client.go for the rationale. The test pins the value
	// so a future "just bump it 10%" change doesn't silently drift
	// the budget that downstream callers (daily review, reflection,
	// roundtable) plan their wall-clock against.
	want := 5 * time.Minute
	if llmAttemptTimeout != want {
		t.Fatalf("expected attempt timeout %s, got %s", want, llmAttemptTimeout)
	}
	if llmHTTPClientTimeout != want {
		t.Fatalf("expected http client timeout %s, got %s", want, llmHTTPClientTimeout)
	}
	if llmTotalRequestTimeout != want {
		t.Fatalf("expected total request timeout %s, got %s", want, llmTotalRequestTimeout)
	}
}

func TestChatCacheExactMatchAndExpiry(t *testing.T) {
	cache := NewChatCache(ChatCacheConfig{Enabled: true, TTL: 20 * time.Millisecond, MaxEntries: 10})
	key := "same-prompt"
	cache.Set(key, "", &ChatResponse{Content: "cached", InputTokens: 10, OutputTokens: 5, TotalCost: 1, TotalPrice: 2, LatencyMs: 123})

	got, ok := cache.Get(key)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got.Content != "cached" || got.InputTokens != 10 || got.TotalCost != 0 || got.TotalPrice != 0 || got.LatencyMs != 0 {
		t.Fatalf("unexpected cached response: %#v", got)
	}

	time.Sleep(30 * time.Millisecond)
	if _, ok := cache.Get(key); ok {
		t.Fatal("expected cache entry to expire")
	}
}

// TestChatCachePerStepTTL verifies B1: the per-StepName TTL map
// overrides the default TTL only for matching steps, and entries
// inserted for different steps age out independently.
func TestChatCachePerStepTTL(t *testing.T) {
	cache := NewChatCache(ChatCacheConfig{
		Enabled:    true,
		TTL:        15 * time.Millisecond,
		MaxEntries: 10,
		TTLByStep: map[string]time.Duration{
			"long_lived": 200 * time.Millisecond,
		},
	})

	// Sanity-check the public resolver.
	if got, want := cache.TTLForStep("long_lived"), 200*time.Millisecond; got != want {
		t.Fatalf("TTLForStep(long_lived) = %s, want %s", got, want)
	}
	if got, want := cache.TTLForStep("default_step"), 15*time.Millisecond; got != want {
		t.Fatalf("TTLForStep(default_step) = %s, want %s", got, want)
	}
	if got, want := cache.TTLForStep(""), 15*time.Millisecond; got != want {
		t.Fatalf("TTLForStep(empty) = %s, want %s", got, want)
	}

	cache.Set("long-k", "long_lived", &ChatResponse{Content: "long"})
	cache.Set("short-k", "default_step", &ChatResponse{Content: "short"})

	// After the short TTL but before the long TTL: short evicted,
	// long still present.
	time.Sleep(30 * time.Millisecond)
	if _, ok := cache.Get("short-k"); ok {
		t.Fatalf("default-TTL entry should have expired after 30ms")
	}
	if _, ok := cache.Get("long-k"); !ok {
		t.Fatalf("per-step TTL entry was wrongly evicted before its 200ms TTL")
	}
}

// TestChatCacheMaxEntriesDefault locks in the post-B1 capacity
// bump (1024 -> 4096). A regression here is a cost regression —
// daily-picks reruns blow the cache and pay double for the same
// prompt — so it's worth a dedicated assertion.
func TestChatCacheMaxEntriesDefault(t *testing.T) {
	cache := NewChatCache(ChatCacheConfig{Enabled: true})
	if cache.maxEntries != 4096 {
		t.Fatalf("expected default MaxEntries to be 4096, got %d", cache.maxEntries)
	}
}

func TestChatCacheKeyIncludesOwnerAndParams(t *testing.T) {
	cfg := &ModelConfig{Provider: ProviderOpenAI, ModelName: "gpt-test", BaseURL: "https://example.com", MaxTokens: 100, Temperature: 0.1}
	base := ChatRequest{OwnerID: "owner-1", StepName: "plan", Messages: []ChatMessage{{Role: "user", Content: "hello"}}}

	key1 := buildChatCacheKey(base, cfg)
	key2 := buildChatCacheKey(ChatRequest{OwnerID: "owner-2", StepName: "plan", Messages: base.Messages}, cfg)
	if key1 == key2 {
		t.Fatal("expected owner to be part of cache key")
	}

	cfg2 := *cfg
	cfg2.Temperature = 0.2
	key3 := buildChatCacheKey(base, &cfg2)
	if key1 == key3 {
		t.Fatal("expected temperature to be part of cache key")
	}
}

func TestChatWithoutOwnerLimiterDoesNotPanic(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer server.Close()

	router := NewModelRouter(map[Provider]string{ProviderOpenAI: "system-key"}, map[ModelTier]*ModelConfig{
		TierSimple: {
			Provider:    ProviderOpenAI,
			ModelName:   "test-model",
			BaseURL:     server.URL,
			MaxTokens:   32,
			Temperature: 0.1,
		},
	}, nil, nil)
	client := NewMultiProviderClient(router, map[Provider]string{ProviderOpenAI: "system-key"})

	resp, err := client.Chat(context.Background(), ChatRequest{
		UserID:    "user-1",
		ModelTier: TierSimple,
		Messages:  []ChatMessage{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Content != "ok" || calls != 1 {
		t.Fatalf("unexpected response/calls: resp=%#v calls=%d", resp, calls)
	}
}

func TestChatCacheCoalescesConcurrentIdenticalRequests(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"coalesced"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer server.Close()

	observer := &recordingLLMObserver{}
	router := NewModelRouter(map[Provider]string{ProviderOpenAI: "system-key"}, map[ModelTier]*ModelConfig{
		TierSimple: {
			Provider:    ProviderOpenAI,
			ModelName:   "test-model",
			BaseURL:     server.URL,
			MaxTokens:   32,
			Temperature: 0.1,
		},
	}, nil, nil)
	client := NewMultiProviderClientWithObserver(router, map[Provider]string{ProviderOpenAI: "system-key"}, observer)
	client.SetChatCache(NewChatCache(ChatCacheConfig{Enabled: true, TTL: time.Minute, MaxEntries: 10}))

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Chat(context.Background(), ChatRequest{
				UserID:    "user-1",
				ModelTier: TierSimple,
				StepName:  "same-step",
				Messages:  []ChatMessage{{Role: "user", Content: "same prompt"}},
			})
			if err != nil {
				errs <- err
				return
			}
			if resp == nil || resp.Content != "coalesced" {
				errs <- errors.New("unexpected response")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("chat: %v", err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected one provider call, got %d", got)
	}
	if observer.Count("success") != 1 || observer.Count("coalesced_hit") == 0 {
		t.Fatalf("expected one provider success and coalesced hits, got statuses=%#v", observer.statuses)
	}
}

func TestChatCacheHitReportsZeroCostStatus(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"cached-answer"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer server.Close()

	observer := &recordingLLMObserver{}
	router := NewModelRouter(map[Provider]string{ProviderOpenAI: "system-key"}, map[ModelTier]*ModelConfig{
		TierSimple: {
			Provider:    ProviderOpenAI,
			ModelName:   "test-model",
			BaseURL:     server.URL,
			MaxTokens:   32,
			Temperature: 0.1,
		},
	}, nil, nil)
	client := NewMultiProviderClientWithObserver(router, map[Provider]string{ProviderOpenAI: "system-key"}, observer)
	client.SetChatCache(NewChatCache(ChatCacheConfig{Enabled: true, TTL: time.Minute, MaxEntries: 10}))

	req := ChatRequest{UserID: "user-1", ModelTier: TierSimple, StepName: "cache_test", Messages: []ChatMessage{{Role: "user", Content: "same prompt"}}}
	first, err := client.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("first chat: %v", err)
	}
	second, err := client.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("second chat: %v", err)
	}
	if first.InputTokens == 0 || second.TotalPrice != 0 || second.TotalCost != 0 || second.LatencyMs != 0 {
		t.Fatalf("expected cached response to zero cost/latency fields, first=%#v second=%#v", first, second)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected one provider call, got %d", got)
	}
	if observer.Count("success") != 1 || observer.Count("cache_hit") != 1 {
		t.Fatalf("expected success and cache_hit statuses, got %#v", observer.statuses)
	}
}

func TestCallBudgetLimiterLimitsOwnerStepWindow(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	budget := NewCallBudgetLimiter(CallBudgetConfig{
		Window:       time.Minute,
		DefaultLimit: 2,
	})
	budget.now = func() time.Time { return now }

	if err := budget.Allow("owner-1", "pm_plan"); err != nil {
		t.Fatalf("first allow: %v", err)
	}
	if err := budget.Allow("owner-1", "pm_plan"); err != nil {
		t.Fatalf("second allow: %v", err)
	}
	if err := budget.Allow("owner-1", "pm_plan"); !IsCallBudgetExceeded(err) {
		t.Fatalf("expected budget exceeded, got %v", err)
	}

	now = now.Add(time.Minute)
	if err := budget.Allow("owner-1", "pm_plan"); err != nil {
		t.Fatalf("expected new window to allow, got %v", err)
	}
}

func TestChatCallBudgetStopsProviderCall(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer server.Close()

	observer := &recordingLLMObserver{}
	router := NewModelRouter(map[Provider]string{ProviderOpenAI: "system-key"}, map[ModelTier]*ModelConfig{
		TierSimple: {
			Provider:    ProviderOpenAI,
			ModelName:   "test-model",
			BaseURL:     server.URL,
			MaxTokens:   32,
			Temperature: 0.1,
		},
	}, nil, nil)
	client := NewMultiProviderClientWithObserver(router, map[Provider]string{ProviderOpenAI: "system-key"}, observer)
	client.SetCallBudgetLimiter(NewCallBudgetLimiter(CallBudgetConfig{Window: time.Minute, DefaultLimit: 1}))

	req := ChatRequest{UserID: "user-1", ModelTier: TierSimple, StepName: "budget_test", Messages: []ChatMessage{{Role: "user", Content: "hello"}}}
	if _, err := client.Chat(context.Background(), req); err != nil {
		t.Fatalf("first chat: %v", err)
	}
	if _, err := client.Chat(context.Background(), req); !IsCallBudgetExceeded(err) {
		t.Fatalf("expected budget error, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected one provider call, got %d", got)
	}
	if observer.Count("budget_exceeded") != 1 {
		t.Fatalf("expected budget_exceeded observer status, got %#v", observer.statuses)
	}
}

func TestDoWithRetryRetriesRetryableErrors(t *testing.T) {
	client := &MultiProviderClient{}
	config := &ModelConfig{Provider: ProviderOpenAI, ModelName: "retry-model"}
	attempts := 0

	body, err := client.doWithRetry(context.Background(), retryPolicy{
		maxAttempts:    3,
		attemptTimeout: time.Second,
		baseBackoff:    time.Millisecond,
		maxBackoff:     5 * time.Millisecond,
	}, config, func(context.Context) ([]byte, int, error) {
		attempts++
		if attempts == 1 {
			return nil, http.StatusTooManyRequests, &llmRequestError{Provider: config.Provider, Model: config.ModelName, StatusCode: http.StatusTooManyRequests, Retryable: true, Reason: "rate_limited", Message: "retry me"}
		}
		return []byte(`{"ok":true}`), http.StatusOK, nil
	})
	if err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("unexpected body: %s", string(body))
	}
}

func TestDoWithRetryExhaustsRetryableErrors(t *testing.T) {
	client := &MultiProviderClient{}
	config := &ModelConfig{Provider: ProviderOpenAI, ModelName: "retry-model"}
	attempts := 0

	_, err := client.doWithRetry(context.Background(), retryPolicy{
		maxAttempts:    3,
		attemptTimeout: time.Second,
		baseBackoff:    time.Millisecond,
		maxBackoff:     5 * time.Millisecond,
	}, config, func(context.Context) ([]byte, int, error) {
		attempts++
		return nil, http.StatusServiceUnavailable, &llmRequestError{Provider: config.Provider, Model: config.ModelName, StatusCode: http.StatusServiceUnavailable, Retryable: true, Reason: "server_error", Message: "try again"}
	})

	var reqErr *llmRequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("expected llmRequestError, got %v", err)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
	if reqErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503, got %d", reqErr.StatusCode)
	}
}

func TestDoWithRetryReturnsContextCancellationDuringBackoff(t *testing.T) {
	client := &MultiProviderClient{}
	config := &ModelConfig{Provider: ProviderOpenAI, ModelName: "retry-model"}
	attempts := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := client.doWithRetry(ctx, retryPolicy{
		maxAttempts:    3,
		attemptTimeout: time.Second,
		baseBackoff:    50 * time.Millisecond,
		maxBackoff:     50 * time.Millisecond,
	}, config, func(context.Context) ([]byte, int, error) {
		attempts++
		cancel()
		return nil, http.StatusTooManyRequests, &llmRequestError{Provider: config.Provider, Model: config.ModelName, StatusCode: http.StatusTooManyRequests, Retryable: true, Reason: "rate_limited", Message: "retry me"}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("expected 1 attempt before cancellation, got %d", attempts)
	}
}

func TestDoWithRetryStopsOnNonRetryableError(t *testing.T) {
	client := &MultiProviderClient{}
	config := &ModelConfig{Provider: ProviderOpenAI, ModelName: "no-retry-model"}
	attempts := 0
	expected := &llmRequestError{Provider: config.Provider, Model: config.ModelName, StatusCode: http.StatusBadRequest, Retryable: false, Reason: "invalid_request", Message: "bad request"}

	_, err := client.doWithRetry(context.Background(), retryPolicy{
		maxAttempts:    3,
		attemptTimeout: time.Second,
		baseBackoff:    time.Millisecond,
		maxBackoff:     5 * time.Millisecond,
	}, config, func(context.Context) ([]byte, int, error) {
		attempts++
		return nil, http.StatusBadRequest, expected
	})
	if !errors.As(err, &expected) {
		t.Fatalf("expected llmRequestError, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("expected 1 attempt, got %d", attempts)
	}
}

type stubPlan struct {
	allowCustomKey bool
}

func (p stubPlan) AllowsCustomKey() bool {
	return p.allowCustomKey
}

type stubGuard struct {
	checkModelAccess func(context.Context, string, string) error
	getEffectivePlan func(context.Context, string) (EffectivePlan, error)
}

func (g stubGuard) CheckModelAccess(ctx context.Context, userID, modelTier string) error {
	if g.checkModelAccess == nil {
		return nil
	}
	return g.checkModelAccess(ctx, userID, modelTier)
}

func (g stubGuard) GetEffectivePlan(ctx context.Context, userID string) (EffectivePlan, error) {
	if g.getEffectivePlan == nil {
		return nil, nil
	}
	return g.getEffectivePlan(ctx, userID)
}

func TestResolveModelRejectsTierWithoutEntitlement(t *testing.T) {
	router := NewModelRouter(map[Provider]string{ProviderOpenAI: "system-key"}, nil, nil, stubGuard{
		checkModelAccess: func(context.Context, string, string) error {
			return errors.New("forbidden")
		},
	})

	_, err := router.ResolveModel(context.Background(), &ChatRequest{UserID: "u1", ModelTier: TierCritical})
	if err == nil || !strings.Contains(err.Error(), "model access denied") {
		t.Fatalf("expected model access denied error, got %v", err)
	}
}

func TestResolveModelRejectsCustomKeyWhenPlanDisallowsIt(t *testing.T) {
	router := NewModelRouter(map[Provider]string{ProviderOpenAI: "system-key"}, nil, nil, stubGuard{
		getEffectivePlan: func(context.Context, string) (EffectivePlan, error) {
			return stubPlan{allowCustomKey: false}, nil
		},
	})
	router.ReplaceUserConfigs("u1", nil, map[Provider]*ModelConfig{
		ProviderOpenAI: {
			Provider:  ProviderOpenAI,
			ModelName: "gpt-4o",
			BaseURL:   "https://api.openai.com/v1",
			APIKey:    "user-key",
		},
	})

	_, err := router.ResolveModel(context.Background(), &ChatRequest{UserID: "u1", ModelTier: TierCritical})
	if err == nil || !strings.Contains(err.Error(), "custom model key is not allowed") {
		t.Fatalf("expected custom key denial, got %v", err)
	}
}

func TestResolveModelMarksCustomKeyUsage(t *testing.T) {
	router := NewModelRouter(map[Provider]string{ProviderOpenAI: "system-key"}, nil, nil, stubGuard{
		getEffectivePlan: func(context.Context, string) (EffectivePlan, error) {
			return stubPlan{allowCustomKey: true}, nil
		},
	})
	router.ReplaceUserConfigs("u1", nil, map[Provider]*ModelConfig{
		ProviderOpenAI: {
			Provider:  ProviderOpenAI,
			ModelName: "gpt-4o",
			BaseURL:   "https://api.openai.com/v1",
			APIKey:    "user-key",
		},
	})

	cfg, err := router.ResolveModel(context.Background(), &ChatRequest{UserID: "u1", ModelTier: TierCritical})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if cfg == nil || !cfg.UsesCustomKey {
		t.Fatalf("expected resolved config to mark custom key usage")
	}
	if cfg.ResolvedTier != TierCritical {
		t.Fatalf("expected resolved tier %s, got %s", TierCritical, cfg.ResolvedTier)
	}
}

func BenchmarkDoWithRetrySuccessFirstAttempt(b *testing.B) {
	client := &MultiProviderClient{}
	config := &ModelConfig{Provider: ProviderOpenAI, ModelName: "bench-model"}
	policy := retryPolicy{
		maxAttempts:    3,
		attemptTimeout: time.Second,
		baseBackoff:    time.Millisecond,
		maxBackoff:     5 * time.Millisecond,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		body, err := client.doWithRetry(context.Background(), policy, config, func(context.Context) ([]byte, int, error) {
			return []byte(`{"ok":true}`), http.StatusOK, nil
		})
		if err != nil {
			b.Fatalf("doWithRetry failed: %v", err)
		}
		if len(body) == 0 {
			b.Fatal("expected response body")
		}
	}
}

type timeoutNetError struct{ msg string }

func (e timeoutNetError) Error() string { return e.msg }
func (timeoutNetError) Timeout() bool   { return true }
func (timeoutNetError) Temporary() bool { return true }

var _ net.Error = timeoutNetError{}
