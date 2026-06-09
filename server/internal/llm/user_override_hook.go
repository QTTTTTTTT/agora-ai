// Package llm — Phase B-2 user-supplied LLM key override hook
// for the /advisor surface (BYOK).
//
// UserOverrideHook is the third hook the router consults after
// req.Model and ModelABHook, and ABOVE FundOverrideHook. The
// position is deliberate: advisor consultations are fund-less so
// "user-supplied key for an /advisor MasterAgent call" must win
// over any per-fund routing rule. Fund overrides still beat the
// user override on fund-mode calls because the hook gates itself
// on req.FundID being empty (Phase B-3 sets ChatRequest.UserID
// for advisor flows and leaves FundID empty so the gate is unambiguous).
//
// Priority chain after Phase B-2 (highest first):
//   1. req.Model explicit                    — forensic / smoke
//   2. ModelABHook                           — S10.1 experiments
//   3. UserOverrideHook                      — Phase B-2 (new, /advisor only)
//   4. FundOverrideHook                      — S14.B (fund-mode)
//   5. agent default (userOverrides)         — user's per-agent config
//   6. user tier override
//   7. user custom endpoint
//   8. platform default
//
// The hook contract mirrors FundOverrideHook:
//   * Returning nil = no user-supplied key matches; the router
//     falls through to FundOverrideHook etc.
//   * Returning non-nil = "use this ModelConfig with the user's
//     plaintext API key already filled in".
//   * The hook MUST NOT call back into ModelRouter write methods
//     while ResolveModel is running — it executes under the read
//     lock and would dead-lock.
//
// The hook implementation (in cmd/server, wired against
// userbyok.Repo) is responsible for:
//   * Checking req.UserID is set (skip otherwise).
//   * Checking req.FundID is empty (skip for fund-mode calls —
//     fund operations don't get to silently use a user's personal
//     key; the user has to opt in by adding the key to the fund's
//     own override).
//   * Looking up the active user key for the resolved provider
//     (typically derived from req.ModelTier + platform defaults).
//   * Decrypting the key via userbyok.Repo.GetActiveForRouting.
//   * Returning a UserOverrideDecision with the route's fields
//     fully populated (Provider, ModelName, BaseURL, APIKey).
//
// Best-effort post-call accounting (RecordUsage on the userbyok
// key id) is the hook implementation's responsibility too — the
// router itself doesn't know that the call ultimately succeeded.

package llm

import "context"

// UserOverrideHook is the function signature the router calls.
type UserOverrideHook func(ctx context.Context, req *ChatRequest) *UserOverrideDecision

// UserOverrideDecision is what a UserOverrideHook returns when
// the user has an active BYOK key applicable to the request.
// A nil pointer means "no user override applies"; the router
// continues down its existing priority chain.
type UserOverrideDecision struct {
	// Config is the fully-resolved ModelConfig (provider, model
	// name, base url, API key) the router should use. The hook
	// is responsible for filling APIKey with the user's
	// decrypted plaintext key; we never lazily defer back to
	// the system pool because the whole point of BYOK is that
	// the platform key never touches the wire.
	Config *ModelConfig

	// UserKeyID identifies the user_llm_keys row that won. The
	// hook implementation may use this to call
	// userbyok.Repo.RecordUsage in a deferred goroutine after
	// the model call lands.
	UserKeyID string

	// Provider is echoed for log readability — "why did this
	// call go through Anthropic instead of OpenAI?" → because
	// the user has an active anthropic BYOK key.
	Provider Provider

	// UserID echoes req.UserID so audit logs can join hook
	// output to the user without re-reading the original
	// request.
	UserID string
}

// SetUserOverrideHook attaches a hook. Pass nil to disable user
// BYOK overrides — useful for boot-time before the userbyok repo
// is ready, or for tests that don't want overrides to leak
// between cases.
func (r *ModelRouter) SetUserOverrideHook(hook UserOverrideHook) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.userOverrideHook = hook
}

// userOverrideHookSnapshot returns the current hook value under
// the read lock. Caller invokes it OUTSIDE any subsequent lock,
// mirroring the FundOverrideHook pattern.
func (r *ModelRouter) userOverrideHookSnapshot() UserOverrideHook {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.userOverrideHook
}
