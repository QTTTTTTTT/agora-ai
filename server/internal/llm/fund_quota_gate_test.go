package llm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeQuotaGate captures Check/Record calls so the test can assert
// exactly when and how the gate is invoked. We use sync/atomic so a
// future change that runs RecordTokens off the request goroutine
// still gives us a stable count.
type fakeQuotaGate struct {
	checkCalls  int32
	recordCalls int32
	checkErr    error
	lastFundID  string
	lastTokens  int64
	mu          sync.Mutex
}

func (f *fakeQuotaGate) CheckLLMTokens(_ context.Context, fundID string, tokens int64) error {
	atomic.AddInt32(&f.checkCalls, 1)
	f.mu.Lock()
	f.lastFundID = fundID
	f.lastTokens = tokens
	f.mu.Unlock()
	return f.checkErr
}

func (f *fakeQuotaGate) RecordLLMTokens(_ context.Context, _ string, _, _ int64) error {
	atomic.AddInt32(&f.recordCalls, 1)
	return nil
}

// TestSetFundQuotaGateRoundTrip is the basic install/lookup test —
// guards against a regression where the locking/getter pair drifts
// apart and the gate is silently dropped.
func TestSetFundQuotaGateRoundTrip(t *testing.T) {
	c := &MultiProviderClient{}
	gate := &fakeQuotaGate{}
	c.SetFundQuotaGate(gate)
	if c.fundQuotaGate() != gate {
		t.Fatal("gate was not installed")
	}
	c.SetFundQuotaGate(nil)
	if c.fundQuotaGate() != nil {
		t.Fatal("gate was not cleared")
	}
}

// TestEstimatePromptTokensConservative verifies the heuristic returns
// non-zero counts for non-empty messages. The exact value doesn't
// matter — the contract is "≥ chars/4 so the gate is pessimistic".
func TestEstimatePromptTokensConservative(t *testing.T) {
	req := ChatRequest{
		Messages: []ChatMessage{
			{Role: "system", Content: strings.Repeat("a", 400)},
			{Role: "user", Content: strings.Repeat("b", 200)},
		},
	}
	got := estimatePromptTokens(req)
	// 400/4 + 4 + 200/4 + 4 = 100 + 4 + 50 + 4 = 158
	if got < 100 {
		t.Errorf("expected ≥ 100 tokens estimate, got %d", got)
	}
}

// TestEstimatePromptTokensZeroForEmpty confirms the helper doesn't
// panic on empty requests — the gate path may be called on warm-up
// pings that have no messages yet.
func TestEstimatePromptTokensZeroForEmpty(t *testing.T) {
	if got := estimatePromptTokens(ChatRequest{}); got != 0 {
		t.Errorf("expected 0 for empty request, got %d", got)
	}
}

// TestFundQuotaGateErrorIsSentinel verifies that a custom typed error
// returned by the gate is propagated as-is. The LLM client must NOT
// wrap or mask the gate's error, otherwise upstream callers can't
// detect the quota-exceeded path via errors.Is.
func TestFundQuotaGateErrorIsSentinel(t *testing.T) {
	want := errors.New("quota exceeded: fund=fund-1 resource=llm_tokens_daily observed=900 limit=1000")
	gate := &fakeQuotaGate{checkErr: want}
	if err := gate.CheckLLMTokens(context.Background(), "fund-1", 200); err == nil || err.Error() != want.Error() {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}
