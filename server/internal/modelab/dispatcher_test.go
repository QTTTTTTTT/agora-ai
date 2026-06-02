package modelab

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fundai/server/internal/llm"
)

// stubLLMClient is a deterministic llm.LLMClient that records
// every Chat call. Used to verify the dispatcher correctly
// passes the primary request through and never fans out for
// non-experiment traffic.
type stubLLMClient struct {
	mu     sync.Mutex
	calls  []llm.ChatRequest
	answer *llm.ChatResponse
	err    error
}

func (s *stubLLMClient) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	if s.err != nil {
		return nil, s.err
	}
	resp := s.answer
	if resp == nil {
		resp = &llm.ChatResponse{Content: "ok", Model: "stub", Provider: "stub"}
	}
	return resp, nil
}

func (s *stubLLMClient) ListModels(_ context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}

func (s *stubLLMClient) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func TestShadowDispatcher_NoResolver_Passthrough(t *testing.T) {
	stub := &stubLLMClient{answer: &llm.ChatResponse{Content: "primary"}}
	d := NewShadowDispatcher(stub, nil, nil, HookContext{})
	resp, err := d.Chat(context.Background(), llm.ChatRequest{StepName: "x"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "primary" {
		t.Fatalf("expected primary content, got %q", resp.Content)
	}
	if stub.callCount() != 1 {
		t.Fatalf("expected exactly 1 inner call, got %d", stub.callCount())
	}
}

func TestShadowDispatcher_NoConfigClient_PassthroughEvenWithExperiment(t *testing.T) {
	// stubLLMClient does NOT implement ConfigChatClient, so even
	// when a resolver matches an experiment, fan-out is disabled
	// — every call should just hit the stub once.
	stub := &stubLLMClient{answer: &llm.ChatResponse{Content: "primary"}}
	d := NewShadowDispatcher(stub, &Resolver{}, nil, HookContext{})
	if d.ConfigClient != nil {
		t.Fatalf("stub must not be detected as ConfigChatClient")
	}
	_, err := d.Chat(context.Background(), llm.ChatRequest{StepName: "x"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if stub.callCount() != 1 {
		t.Fatalf("expected exactly 1 inner call, got %d", stub.callCount())
	}
}

func TestShadowDispatcher_InnerError_Propagates(t *testing.T) {
	stub := &stubLLMClient{err: errors.New("boom")}
	d := NewShadowDispatcher(stub, nil, nil, HookContext{})
	_, err := d.Chat(context.Background(), llm.ChatRequest{StepName: "x"})
	if err == nil {
		t.Fatalf("expected error from inner client")
	}
}

func TestShadowDispatcher_NilInner_ReturnsError(t *testing.T) {
	var d *ShadowDispatcher = &ShadowDispatcher{}
	_, err := d.Chat(context.Background(), llm.ChatRequest{})
	if err == nil {
		t.Fatalf("expected error for nil inner client")
	}
}

func TestShadowDispatcher_ListModels_Delegates(t *testing.T) {
	stub := &stubLLMClient{}
	d := NewShadowDispatcher(stub, nil, nil, HookContext{})
	if _, err := d.ListModels(context.Background()); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
}

func TestShadowDispatcher_Semaphore_BoundsConcurrency(t *testing.T) {
	// Verify the semaphore is allocated lazily and respects the
	// configured cap. We don't actually run shadow arms here —
	// they require a *MultiProviderClient — so we just exercise
	// the helpers directly.
	d := &ShadowDispatcher{MaxConcurrentShadowCalls: 2}
	var inFlight int64
	var maxObserved int64
	const N = 20

	wg := sync.WaitGroup{}
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.acquire()
			n := atomic.AddInt64(&inFlight, 1)
			for {
				cur := atomic.LoadInt64(&maxObserved)
				if n <= cur || atomic.CompareAndSwapInt64(&maxObserved, cur, n) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			atomic.AddInt64(&inFlight, -1)
			d.release()
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&maxObserved); got > 2 {
		t.Fatalf("semaphore failed: max observed concurrency %d > 2", got)
	}
}

func TestTryParseJSONOutput(t *testing.T) {
	if _, err := tryParseJSONOutput(""); err == nil {
		t.Fatalf("expected error for empty input")
	}
	if _, err := tryParseJSONOutput("not json"); err == nil {
		t.Fatalf("expected error for non-JSON input")
	}
	if msg, err := tryParseJSONOutput(`{"a":1}`); err != nil {
		t.Fatalf("expected success, got %v", err)
	} else if string(msg) != `{"a":1}` {
		t.Fatalf("expected raw payload back, got %q", string(msg))
	}
}
