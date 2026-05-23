package llm

import (
	"errors"
	"fmt"
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

// ErrMissingCredentials must trigger failover. This is the live-
// debugging scenario from Sprint C verification: an agent
// configured for provider=claude but the env only carries
// LLM_API_KEY for the platform default (gemini). Without this
// case, the debate roundtable fails every round and the PM falls
// back to the legacy text-concat consensus, silently dropping
// the structured RoundtableStance / BullCase / BearCase blocks
// from the prompt.
func TestShouldFailoverMissingCredentialsIsTrue(t *testing.T) {
	if !ShouldFailover(ErrMissingCredentials) {
		t.Fatal("plain ErrMissingCredentials must trigger failover")
	}
	// errors.Is must match through wrapping (the chatOnce wrap is
	// `fmt.Errorf("...: %w", ErrMissingCredentials)`).
	wrapped := fmt.Errorf("llm: no API key available for provider claude: %w", ErrMissingCredentials)
	if !ShouldFailover(wrapped) {
		t.Fatal("wrapped ErrMissingCredentials must trigger failover (errors.Is regression)")
	}
}

// WithPlatformDefault appends the supplied provider to every tier
// chain that doesn't already list it and bumps MaxAttempts past
// the longest chain length so a primary provider that isn't
// itself in the chain can still walk every entry. Locked here
// because the Sprint C live-verification depended on this exact
// behaviour: an environment whose only configured key was
// LLM_API_KEY (provider=gemini) couldn't service tier=standard
// until gemini was guaranteed both a chain slot AND enough
// attempts to be reached from an off-chain primary like claude.
func TestWithPlatformDefaultAppendsAndBumpsAttempts(t *testing.T) {
	cfg := DefaultFailoverConfig().WithPlatformDefault(ProviderGemini)
	for tier, chain := range cfg.TierChains {
		if len(chain) == 0 {
			t.Fatalf("tier %s chain empty after WithPlatformDefault", tier)
		}
		seen := false
		for _, p := range chain {
			if p == ProviderGemini {
				seen = true
			}
		}
		if !seen {
			t.Errorf("tier %s missing ProviderGemini after WithPlatformDefault: %v", tier, chain)
		}
	}
	maxChain := 0
	for _, chain := range cfg.TierChains {
		if len(chain) > maxChain {
			maxChain = len(chain)
		}
	}
	// MUST be strictly > longest chain so an off-chain primary
	// (e.g. claude when standard chain is [deepseek, openai,
	// qwen, gemini]) gets one attempt for itself + one per
	// chain entry.
	if cfg.MaxAttempts < maxChain+1 {
		t.Errorf("MaxAttempts=%d must be >= longest chain + 1 (%d)", cfg.MaxAttempts, maxChain+1)
	}
}

// WithPlatformDefault is idempotent: applying the same default
// twice doesn't duplicate the entry.
func TestWithPlatformDefaultIdempotent(t *testing.T) {
	first := DefaultFailoverConfig().WithPlatformDefault(ProviderGemini)
	second := first.WithPlatformDefault(ProviderGemini)
	for tier, chain := range second.TierChains {
		count := 0
		for _, p := range chain {
			if p == ProviderGemini {
				count++
			}
		}
		if count != 1 {
			t.Errorf("tier %s has ProviderGemini %d times after idempotent re-apply: %v", tier, count, chain)
		}
	}
}

// Empty / whitespace provider is a no-op (Sprint C wiring passes
// strings.TrimSpace(LLM_PROVIDER); an unset env should not corrupt
// the chain).
func TestWithPlatformDefaultEmptyIsNoOp(t *testing.T) {
	base := DefaultFailoverConfig()
	got := base.WithPlatformDefault("")
	for tier, baseChain := range base.TierChains {
		gotChain := got.TierChains[tier]
		if len(gotChain) != len(baseChain) {
			t.Errorf("tier %s chain altered by empty default: got %v, want %v", tier, gotChain, baseChain)
		}
		for i := range baseChain {
			if baseChain[i] != gotChain[i] {
				t.Errorf("tier %s chain altered by empty default: got %v, want %v", tier, gotChain, baseChain)
			}
		}
	}
	got2 := base.WithPlatformDefault("   ")
	if got2.MaxAttempts != base.MaxAttempts {
		t.Errorf("whitespace default must not change MaxAttempts (got %d, want %d)", got2.MaxAttempts, base.MaxAttempts)
	}
}

// WithPlatformDefault must not mutate the receiver — needed so the
// runtime wiring can re-derive failover configs across reloads.
func TestWithPlatformDefaultDoesNotMutateReceiver(t *testing.T) {
	original := DefaultFailoverConfig()
	originalCritical := append([]Provider{}, original.TierChains[TierCritical]...)
	_ = original.WithPlatformDefault(ProviderGemini)
	if len(original.TierChains[TierCritical]) != len(originalCritical) {
		t.Errorf("WithPlatformDefault mutated receiver chain: %v", original.TierChains[TierCritical])
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
