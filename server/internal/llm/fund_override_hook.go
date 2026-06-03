// Package llm — S14.B fund-level provider override hook.
//
// FundOverrideHook is the second hook the router consults (after
// the model A/B hook). It exists for the marketplace economics
// reason described in migration 084: the strategy owner needs final
// say over which provider powers which agent inside their fund, NOT
// the subscriber who is currently driving the calls. Without a
// fund-level layer the user-level override (subscription/model_config)
// wins, which means a subscriber's personal preference can override
// the strategy owner's intent.
//
// Priority chain after S14.B (highest first):
//   1. req.Model explicit                    — forensic / smoke
//   2. ModelABHook                           — S10.1 experiments
//   3. FundOverrideHook                      — S14.B (new)
//   4. agent default (userOverrides)         — user's per-agent config
//   5. user tier override                    — user's per-tier config
//   6. user custom endpoint
//   7. platform default
//
// Why between A/B and agent default:
//   * A/B experiments stay authoritative — they're operator-driven
//     and need reproducibility (B6 in the S14 plan).
//   * Fund overrides supersede the user's personal config because
//     the fund owner is the one paying the credits.
//   * Within the fund layer we resolve the MOST specific match via
//     the SQL ORDER BY in repository.FundLLMOverrideRepo.ResolveForRequest.
//
// The hook contract mirrors ModelABHook:
//   * Returning nil = no override matches; the router falls through.
//   * Returning non-nil = "use this ModelConfig", router treats it
//     as authoritative (API key, BaseURL, provider, etc. must all
//     be filled in by the hook implementation).
//   * The hook MUST NOT call back into ModelRouter write methods
//     while ResolveModel is running — it executes under the read
//     lock and would dead-lock.

package llm

import "context"

// FundOverrideHook is the function signature the router calls.
type FundOverrideHook func(ctx context.Context, req *ChatRequest) *FundOverrideDecision

// FundOverrideDecision is what a FundOverrideHook returns when an
// override matches. A nil pointer means "no fund override applies"
// and the router continues down its existing priority chain.
type FundOverrideDecision struct {
	// Config is the fully-resolved ModelConfig (with API key,
	// BaseURL, provider, model name etc.) the router should use.
	Config *ModelConfig
	// OverrideID identifies which fund_llm_overrides row matched.
	// Surfaced into logs so an operator can answer "why did this
	// call go to claude?" by looking up the row.
	OverrideID string
	// Specificity (0..15) — the rank the resolver assigned to this
	// row. Higher = more specific match. Logged for audit.
	Specificity int
	// FundID echoed back so the dispatcher can attribute usage to
	// the right ledger (matches req.FundID but explicit here keeps
	// the hook self-describing).
	FundID string
}

// SetFundOverrideHook attaches a hook. Pass nil to disable fund
// overrides — useful for boot-time before the repo is ready, or
// for tests that don't want overrides to leak between cases.
func (r *ModelRouter) SetFundOverrideHook(hook FundOverrideHook) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fundOverrideHook = hook
}

// fundOverrideHookSnapshot returns the current hook value under
// the read lock. Caller invokes it OUTSIDE any subsequent lock,
// mirroring the ModelABHook pattern.
func (r *ModelRouter) fundOverrideHookSnapshot() FundOverrideHook {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.fundOverrideHook
}
