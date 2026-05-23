package llm

import (
	"errors"
	"strings"
	"sync"
)

// FailoverConfig configures the F15 provider failover chain. When the
// primary provider for a tier returns a failover-eligible error
// (circuit-open, 5xx, network failure), MultiProviderClient retries the
// same request against the next provider in TierChains[tier].
//
// Auto-switch-back is automatic: every fresh Chat() call starts from
// the head of the chain. The OwnerLimiter circuit-breaker keeps the
// primary cooled-off briefly when it's down; once the breaker closes,
// the next call resumes routing through the primary.
//
// MaxAttempts caps the total tries across the chain (primary + fallbacks)
// so a fully-broken vendor cluster can't make every workflow step burn
// N provider calls before failing. Defaults to 3.
type FailoverConfig struct {
	// TierChains maps a tier to its preferred provider order. The first
	// entry is the primary; the rest are fallbacks tried in order.
	//
	// Resolution: when failing over TO a provider, we find that
	// provider's representative model from PlatformModels (matching
	// both tier and provider). If no such model exists, the fallback
	// step is skipped and we move to the next provider in the chain.
	TierChains map[ModelTier][]Provider

	// MaxAttempts caps total provider attempts including the primary.
	// Must be >= 1; 0 falls back to 3.
	MaxAttempts int
}

// DefaultFailoverConfig returns a sensible production default chain.
// Adjust by env or admin API if vendor preferences change.
//
//	Critical : openai → claude → gemini      (most-expensive primary)
//	Standard : deepseek → openai → qwen      (cheapest primary, diverse fallbacks)
//	Simple   : openai → deepseek             (cheap → cheaper backup)
//
// Rationale: the primary for each tier is the provider chosen by the
// existing platform default; fallbacks add vendor diversity so a
// vendor-wide outage doesn't take down the whole workflow.
//
// MaxAttempts is sized at 4 so a chain of three preferred providers
// still has one slot left for the platform-default safety net
// appended by WithPlatformDefault below.
func DefaultFailoverConfig() FailoverConfig {
	return FailoverConfig{
		MaxAttempts: 4,
		TierChains: map[ModelTier][]Provider{
			TierCritical: {ProviderOpenAI, ProviderClaude, ProviderGemini},
			TierStandard: {ProviderDeepSeek, ProviderOpenAI, ProviderQwen},
			TierSimple:   {ProviderOpenAI, ProviderDeepSeek},
		},
	}
}

// WithPlatformDefault returns a copy of cfg with the supplied
// provider appended to every tier chain that doesn't already list
// it. The MaxAttempts cap is bumped to len(longest_chain) so the
// extra fallback has a slot in every tier.
//
// Sprint C verification rationale: the local / staging env
// typically configures LLM_API_KEY + LLM_PROVIDER (e.g. gemini)
// without populating provider-specific keys (OPENAI_API_KEY,
// CLAUDE_API_KEY, …). The default chain might not include the
// platform-default provider for every tier, which means an
// agent configured for one of the unkeyed providers exhausts the
// chain before reaching the only-keyed provider. Appending the
// platform default to every chain prevents that whole class of
// "every researcher failed" symptoms.
//
// A zero / empty provider is a no-op so callers can pass an
// unconfigured ChatRequest.Owner-less router safely.
func (c FailoverConfig) WithPlatformDefault(provider Provider) FailoverConfig {
	trimmed := Provider(strings.ToLower(strings.TrimSpace(string(provider))))
	if trimmed == "" {
		return c
	}
	out := FailoverConfig{
		MaxAttempts: c.MaxAttempts,
		TierChains:  make(map[ModelTier][]Provider, len(c.TierChains)),
	}
	maxChain := 0
	for tier, chain := range c.TierChains {
		copied := make([]Provider, len(chain))
		copy(copied, chain)
		alreadyPresent := false
		for _, p := range copied {
			if Provider(strings.ToLower(strings.TrimSpace(string(p)))) == trimmed {
				alreadyPresent = true
				break
			}
		}
		if !alreadyPresent {
			copied = append(copied, trimmed)
		}
		if len(copied) > maxChain {
			maxChain = len(copied)
		}
		out.TierChains[tier] = copied
	}
	// maxChain + 1 covers the "primary provider isn't in the
	// chain at all" case (an agent configured for an
	// off-chain provider like claude when the standard chain
	// is [deepseek, openai, qwen, gemini]). In that case
	// nextProvider returns chain[0] as the first fallback, so
	// the loop still needs len(chain) additional attempts
	// after the primary to walk the full chain. Without this,
	// the very last entry (typically the platform default we
	// just appended) never gets tried.
	if out.MaxAttempts < maxChain+1 {
		out.MaxAttempts = maxChain + 1
	}
	return out
}

// ShouldFailover decides whether a given error should trigger the next
// fallback in the chain. Returns true for:
//   - circuit breaker open (primary cooled off by the rate limiter)
//   - 5xx / 429 / timeout / network failures from the actual call
//   - missing-credentials for the requested provider (operator has an
//     agent pointed at a provider whose API key was never configured —
//     deterministic, won't fix on retry of the same provider; the next
//     provider in the chain is the only chance of success)
//
// Returns false for:
//   - 4xx validation errors (won't succeed on a different provider)
//   - context cancellation (caller is done; pointless to keep trying)
//   - budget exceeded (no provider will help; user must bump cap)
//   - nil (caller should not invoke this for success cases)
func ShouldFailover(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrCallBudgetExceeded) {
		return false
	}
	if errors.Is(err, ErrMissingCredentials) {
		return true
	}
	if IsCircuitOpen(err) {
		return true
	}
	return shouldTripBreaker(err)
}

// fallbackModelFor returns the platform-default model NAME for a (tier,
// provider) pair, looking it up from PlatformModels. Empty string if no
// model exists for that combo; the caller should skip to the next
// provider in that case.
//
// Preference order within PlatformModels:
//  1. an entry where IsDefault=true AND tier+provider match
//  2. the first entry that matches tier+provider
//
// We never invent a model name — only known PlatformModels are used so
// the router downstream can resolve costs / endpoints correctly.
func fallbackModelFor(tier ModelTier, provider Provider) string {
	tierStr := string(tier)
	providerStr := strings.ToLower(strings.TrimSpace(string(provider)))
	if tierStr == "" || providerStr == "" {
		return ""
	}
	var defaultMatch string
	for i := range PlatformModels {
		m := PlatformModels[i]
		if m.Tier != tierStr || strings.ToLower(strings.TrimSpace(m.Provider)) != providerStr {
			continue
		}
		if m.IsDefault {
			return m.ModelName
		}
		if defaultMatch == "" {
			defaultMatch = m.ModelName
		}
	}
	return defaultMatch
}

// nextProvider returns the next provider in the chain after current. If
// current isn't in the chain (unusual — user-customised primary), the
// first non-current entry is returned. Returns ("", false) when the
// chain is exhausted (current is in the chain but no entry follows it).
func (c FailoverConfig) nextProvider(tier ModelTier, current Provider) (Provider, bool) {
	chain, ok := c.TierChains[tier]
	if !ok || len(chain) == 0 {
		return "", false
	}
	// First pass: position-based lookup. If current is in the chain,
	// return the next entry — or ("", false) if it's the last.
	for i, p := range chain {
		if p == current {
			if i+1 < len(chain) {
				return chain[i+1], true
			}
			return "", false
		}
	}
	// current isn't in the chain at all (user-customised primary).
	// Surface the first chain entry as the best-effort fallback.
	return chain[0], true
}

// maxAttempts returns the effective cap (defaulting to 3 when unset).
func (c FailoverConfig) maxAttempts() int {
	if c.MaxAttempts <= 0 {
		return 3
	}
	return c.MaxAttempts
}

// failoverState is the per-client mutable view of the failover config.
// Wrapped in a struct so SetFailoverConfig can swap the whole config
// atomically without racing readers.
type failoverState struct {
	mu  sync.RWMutex
	cfg FailoverConfig
}

func newFailoverState(cfg FailoverConfig) *failoverState {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	return &failoverState{cfg: cfg}
}

func (s *failoverState) snapshot() FailoverConfig {
	if s == nil {
		return FailoverConfig{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}
