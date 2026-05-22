package llm

import (
	"errors"
	"testing"
)

func TestShouldFailoverNilIsFalse(t *testing.T) {
	if ShouldFailover(nil) {
		t.Fatal("nil error must not trigger failover")
	}
}

func TestShouldFailoverBudgetExceededDoesNotFailover(t *testing.T) {
	if ShouldFailover(ErrCallBudgetExceeded) {
		t.Fatal("budget exhaustion must NOT trigger failover (would burn next provider's budget too)")
	}
}

func TestShouldFailoverCircuitOpenIsTrue(t *testing.T) {
	// ErrCircuitOpen is the canonical sentinel raised by the rate
	// limiter. Wrapping must still match via errors.Is.
	if !ShouldFailover(ErrCircuitOpen) {
		t.Fatal("ErrCircuitOpen MUST trigger failover")
	}
}

func TestShouldFailoverPermanentClientErrorIsFalse(t *testing.T) {
	// 400-style validation errors are llmRequestError with reason=
	// "bad_request" / "auth_failed" etc. — shouldTripBreaker returns
	// false for those.
	clientErr := &llmRequestError{Reason: "bad_request", StatusCode: 400}
	if ShouldFailover(clientErr) {
		t.Fatal("4xx client errors must NOT trigger failover (won't succeed elsewhere)")
	}
}

func TestShouldFailoverServerErrorIsTrue(t *testing.T) {
	for _, reason := range []string{"rate_limited", "server_error", "timeout", "transport_error"} {
		err := &llmRequestError{Reason: reason, StatusCode: 503}
		if !ShouldFailover(err) {
			t.Errorf("reason=%q must trigger failover", reason)
		}
	}
}

func TestDefaultFailoverConfigHasAllTiers(t *testing.T) {
	cfg := DefaultFailoverConfig()
	for _, tier := range ValidTiers {
		chain, ok := cfg.TierChains[tier]
		if !ok || len(chain) == 0 {
			t.Errorf("tier %v missing from default failover chain", tier)
		}
	}
	if cfg.MaxAttempts < 2 {
		t.Errorf("default MaxAttempts should be >= 2, got %d", cfg.MaxAttempts)
	}
}

func TestDefaultFailoverConfigChainsAreDiverse(t *testing.T) {
	// A chain of [openai, openai] would defeat the purpose: when openai
	// goes down, the fallback is also openai.
	cfg := DefaultFailoverConfig()
	for tier, chain := range cfg.TierChains {
		seen := map[Provider]bool{}
		for _, p := range chain {
			if seen[p] {
				t.Errorf("tier %v has duplicate provider %v in chain", tier, p)
			}
			seen[p] = true
		}
	}
}

func TestNextProviderAdvancesPastCurrent(t *testing.T) {
	cfg := FailoverConfig{
		TierChains: map[ModelTier][]Provider{
			TierStandard: {ProviderDeepSeek, ProviderOpenAI, ProviderQwen},
		},
	}
	next, ok := cfg.nextProvider(TierStandard, ProviderDeepSeek)
	if !ok || next != ProviderOpenAI {
		t.Errorf("expected next=openai after deepseek, got %v ok=%v", next, ok)
	}
	next, ok = cfg.nextProvider(TierStandard, ProviderOpenAI)
	if !ok || next != ProviderQwen {
		t.Errorf("expected next=qwen after openai, got %v ok=%v", next, ok)
	}
	_, ok = cfg.nextProvider(TierStandard, ProviderQwen)
	if ok {
		t.Errorf("expected no next after last provider, got ok=true")
	}
}

func TestNextProviderUnknownCurrentFallsBackToFirstNonCurrent(t *testing.T) {
	cfg := FailoverConfig{
		TierChains: map[ModelTier][]Provider{
			TierStandard: {ProviderDeepSeek, ProviderOpenAI},
		},
	}
	// Current is not in the chain (user-customised primary). We should
	// pick the first chain entry that isn't current.
	next, ok := cfg.nextProvider(TierStandard, ProviderClaude)
	if !ok || next != ProviderDeepSeek {
		t.Errorf("expected fallback to first non-current, got %v ok=%v", next, ok)
	}
}

func TestFallbackModelForFindsKnownPlatformModel(t *testing.T) {
	// PlatformModels includes openai/gpt-4o-mini at Standard tier. The
	// failover lookup must surface it.
	got := fallbackModelFor(TierStandard, ProviderOpenAI)
	if got == "" {
		t.Fatal("expected a model name for (standard, openai)")
	}
	// Default OpenAI standard model is gpt-4o-mini per PlatformModels.
	if got != "gpt-4o-mini" {
		t.Errorf("expected gpt-4o-mini for (standard, openai), got %q", got)
	}
}

func TestFallbackModelForUnknownComboReturnsEmpty(t *testing.T) {
	// Custom provider isn't in PlatformModels.
	got := fallbackModelFor(TierStandard, ProviderCustom)
	if got != "" {
		t.Errorf("expected empty for unknown combo, got %q", got)
	}
}

func TestFallbackModelForPrefersIsDefault(t *testing.T) {
	// Critical+openai has both gpt-4o (IsDefault=true) and o4-mini.
	// We must pick the default to mirror non-failover routing.
	got := fallbackModelFor(TierCritical, ProviderOpenAI)
	if got != "gpt-4o" {
		t.Errorf("expected gpt-4o (IsDefault) for (critical, openai), got %q", got)
	}
}

func TestFailoverConfigMaxAttemptsDefault(t *testing.T) {
	cfg := FailoverConfig{} // MaxAttempts=0
	if cfg.maxAttempts() != 3 {
		t.Errorf("expected default MaxAttempts=3, got %d", cfg.maxAttempts())
	}
	cfg.MaxAttempts = 5
	if cfg.maxAttempts() != 5 {
		t.Errorf("explicit MaxAttempts not honoured, got %d", cfg.maxAttempts())
	}
}

// TestNewFailoverStateClonesAndNormalises ensures the state wrapper
// applies the default maxAttempts at construction time.
func TestNewFailoverStateClonesAndNormalises(t *testing.T) {
	state := newFailoverState(FailoverConfig{TierChains: map[ModelTier][]Provider{TierSimple: {ProviderOpenAI}}})
	snap := state.snapshot()
	if snap.MaxAttempts != 3 {
		t.Errorf("expected normalised MaxAttempts=3, got %d", snap.MaxAttempts)
	}
}

// TestShouldFailoverWrappedError protects against future error wrapping
// in callers — a wrapped circuit-open error must still match.
func TestShouldFailoverWrappedError(t *testing.T) {
	wrapped := errors.Join(errors.New("upstream context"), ErrCircuitOpen)
	if !ShouldFailover(wrapped) {
		t.Fatal("wrapped circuit-open must still trigger failover")
	}
}
