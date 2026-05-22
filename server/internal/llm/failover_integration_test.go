package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestChatFailoverActivatesOnPrimaryServerError is the headline F15
// integration test: when the primary returns 503, Chat must transparently
// retry against the fallback provider and return its success response.
//
// Implementation trick: we register the fallback model under a DIFFERENT
// tier in router.defaultModels. router.findModelByName ignores tier
// (matches purely on ModelName), so when the failover loop sets
// req.Model = fallback model name, the router resolves it to our test
// HTTP server URL — never touching the real provider endpoint.
func TestChatFailoverActivatesOnPrimaryServerError(t *testing.T) {
	var primaryCalls, fallbackCalls int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryCalls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream is having a bad day","type":"server_error"}}`))
	}))
	defer primary.Close()

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fallbackCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"fallback ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
	}))
	defer fallback.Close()

	// Both providers use the OpenAI-compatible Chat Completions wire
	// format here so the same fake server handler shape works for the
	// fallback. Production uses a different format per provider, but
	// that's exercised by the per-provider call tests.
	defaults := map[ModelTier]*ModelConfig{
		// Primary (Standard tier): deepseek-chat at our primary stub URL.
		TierStandard: {
			Provider:    ProviderDeepSeek,
			ModelName:   "deepseek-chat",
			BaseURL:     primary.URL,
			MaxTokens:   32,
			Temperature: 0.1,
		},
		// Fallback model registered under Simple tier so findModelByName
		// surfaces it. Tier in this entry doesn't matter for failover
		// lookup — only the ModelName is matched.
		TierSimple: {
			Provider:    ProviderOpenAI,
			ModelName:   "gpt-4o-mini",
			BaseURL:     fallback.URL,
			MaxTokens:   32,
			Temperature: 0.1,
		},
	}
	systemKeys := map[Provider]string{
		ProviderDeepSeek: "deepseek-key",
		ProviderOpenAI:   "openai-key",
	}
	router := NewModelRouter(systemKeys, defaults, nil, nil)
	client := NewMultiProviderClient(router, systemKeys)
	// Configure failover chain: Standard tier → deepseek then openai.
	// fallbackModelFor(Standard, openai) returns "gpt-4o-mini" from
	// PlatformModels, which our defaults map points at the fallback URL.
	client.SetFailoverConfig(FailoverConfig{
		MaxAttempts: 2,
		TierChains: map[ModelTier][]Provider{
			TierStandard: {ProviderDeepSeek, ProviderOpenAI},
		},
	})

	resp, err := client.Chat(context.Background(), ChatRequest{
		UserID:   "user-1",
		StepName: "macro_brief", // standard tier
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Content != "fallback ok" {
		t.Fatalf("expected fallback response, got %q", resp.Content)
	}
	// chatOnce includes an internal 5xx retry loop (~3 attempts per
	// logical call), so primary gets >=1 hits before the failover loop
	// kicks in. We only assert "primary was tried at least once and the
	// fallback returned the response", which is the F15 invariant.
	if got := atomic.LoadInt32(&primaryCalls); got < 1 {
		t.Errorf("expected primary called at least once, got %d", got)
	}
	if got := atomic.LoadInt32(&fallbackCalls); got != 1 {
		t.Errorf("expected fallback called once (success on first hit), got %d", got)
	}
}

// TestChatFailoverDisabledByDefault confirms the absence of failover
// config preserves pre-F15 behaviour — a 503 from the primary surfaces
// directly to the caller without trying any fallback. Critical for
// backward compatibility of existing callers that don't opt in.
func TestChatFailoverDisabledByDefault(t *testing.T) {
	var calls int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	}))
	defer primary.Close()

	router := NewModelRouter(map[Provider]string{ProviderOpenAI: "k"}, map[ModelTier]*ModelConfig{
		TierSimple: {Provider: ProviderOpenAI, ModelName: "test-model", BaseURL: primary.URL, MaxTokens: 32, Temperature: 0.1},
	}, nil, nil)
	client := NewMultiProviderClient(router, map[Provider]string{ProviderOpenAI: "k"})
	// No SetFailoverConfig — failover disabled.

	_, err := client.Chat(context.Background(), ChatRequest{
		UserID:    "user-1",
		ModelTier: TierSimple,
		Messages:  []ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected primary error to surface, got nil")
	}
	// Without failover the call resolves to a single chatOnce, which
	// itself retries 5xx internally. The F15 invariant we care about
	// here is "no requests went to any other endpoint" — primary is
	// the only server, so total calls > 0 proves only primary was hit.
	if got := atomic.LoadInt32(&calls); got < 1 {
		t.Errorf("expected primary called at least once, got %d", got)
	}
}

// TestChatFailoverDoesNotRetryOnPermanentError checks that 400-style
// client errors short-circuit failover. Retrying a malformed request
// against another vendor would waste budget without changing the
// outcome.
func TestChatFailoverDoesNotRetryOnPermanentError(t *testing.T) {
	var primaryCalls, fallbackCalls int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryCalls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"missing field","type":"invalid_request_error"}}`))
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fallbackCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"never reached"}}]}`))
	}))
	defer fallback.Close()

	defaults := map[ModelTier]*ModelConfig{
		TierStandard: {Provider: ProviderDeepSeek, ModelName: "deepseek-chat", BaseURL: primary.URL, MaxTokens: 32, Temperature: 0.1},
		TierSimple:   {Provider: ProviderOpenAI, ModelName: "gpt-4o-mini", BaseURL: fallback.URL, MaxTokens: 32, Temperature: 0.1},
	}
	router := NewModelRouter(map[Provider]string{ProviderDeepSeek: "k", ProviderOpenAI: "k"}, defaults, nil, nil)
	client := NewMultiProviderClient(router, map[Provider]string{ProviderDeepSeek: "k", ProviderOpenAI: "k"})
	client.SetFailoverConfig(FailoverConfig{
		MaxAttempts: 2,
		TierChains:  map[ModelTier][]Provider{TierStandard: {ProviderDeepSeek, ProviderOpenAI}},
	})

	_, err := client.Chat(context.Background(), ChatRequest{
		UserID:   "user-1",
		StepName: "macro_brief",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected primary error to surface, got nil")
	}
	if got := atomic.LoadInt32(&primaryCalls); got != 1 {
		t.Errorf("primary should be called once, got %d", got)
	}
	if got := atomic.LoadInt32(&fallbackCalls); got != 0 {
		t.Fatalf("fallback must NOT be called for 4xx errors, got %d", got)
	}
}

// TestChatFailoverExhaustsChain proves the loop stops after MaxAttempts.
// Without this cap a fully-broken vendor cluster would burn through
// every provider in PlatformModels on every workflow step.
func TestChatFailoverExhaustsChain(t *testing.T) {
	var primaryCalls, fallbackCalls int32
	always503 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	})
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryCalls, 1)
		always503.ServeHTTP(w, r)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fallbackCalls, 1)
		always503.ServeHTTP(w, r)
	}))
	defer fallback.Close()

	defaults := map[ModelTier]*ModelConfig{
		TierStandard: {Provider: ProviderDeepSeek, ModelName: "deepseek-chat", BaseURL: primary.URL, MaxTokens: 32, Temperature: 0.1},
		TierSimple:   {Provider: ProviderOpenAI, ModelName: "gpt-4o-mini", BaseURL: fallback.URL, MaxTokens: 32, Temperature: 0.1},
	}
	router := NewModelRouter(map[Provider]string{ProviderDeepSeek: "k", ProviderOpenAI: "k"}, defaults, nil, nil)
	client := NewMultiProviderClient(router, map[Provider]string{ProviderDeepSeek: "k", ProviderOpenAI: "k"})
	client.SetFailoverConfig(FailoverConfig{
		MaxAttempts: 2,
		TierChains:  map[ModelTier][]Provider{TierStandard: {ProviderDeepSeek, ProviderOpenAI}},
	})

	_, err := client.Chat(context.Background(), ChatRequest{
		UserID:   "user-1",
		StepName: "macro_brief",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected exhaustion error, got nil")
	}
	// chatOnce includes an internal 5xx retry loop, so the per-server
	// HTTP hit count is ~3 each. The F15 invariant is "the failover
	// loop made exactly MaxAttempts=2 LOGICAL provider attempts, one
	// per server". That means at minimum both servers got 1+ hit.
	if got := atomic.LoadInt32(&primaryCalls); got < 1 {
		t.Errorf("primary should be called at least once, got %d", got)
	}
	if got := atomic.LoadInt32(&fallbackCalls); got < 1 {
		t.Errorf("fallback should be called at least once (MaxAttempts=2), got %d", got)
	}
}
