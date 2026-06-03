// router_reload.go — hot-reload entry point used by the platform
// LLM provider admin path (S13).
//
// ReplaceSystemConfig is the ONLY external mutation that may
// rebuild the systemAPIKeys map + tier-default chain after the
// router has been constructed. The admin handler calls this each
// time a row in platform_llm_providers is upserted / deleted /
// promoted to default, so a config change goes live in-process
// without an app restart.
//
// Concurrency model:
//   * NewModelRouter never publishes the freshly-built router
//     until the caller passes it to consumers, so initial population
//     does not need the write lock.
//   * Every later mutation goes through the existing RWMutex r.mu.
//     ResolveModel takes the read lock for the entire resolution
//     (see ResolveModel in router.go) so swapping the two maps
//     under the write lock is safe — pending readers see the old
//     state, readers arriving after the swap see the new state,
//     and the swap itself is a couple of pointer assignments.

package llm

import (
	"sync/atomic"
)

// reloadGen counts how many times ReplaceSystemConfig has run
// successfully. Exposed via ReloadGeneration() for diagnostics
// (the admin handler logs it; tests assert it advances).
var reloadGen atomic.Uint64

// ReloadGeneration returns the number of completed reload swaps
// since process start. Strictly monotonic; useful as a health
// check ("did my upsert actually move the router?").
func ReloadGeneration() uint64 {
	return reloadGen.Load()
}

// ReplaceSystemConfig swaps in a new (systemAPIKeys, tierDefaults)
// pair. The tierDefaults map is overlaid on the built-in
// DefaultModels (same merge logic NewModelRouter uses on construction)
// so callers can pass a partial map without erasing tiers they did
// not configure.
//
// Existing per-user / per-agent overrides and the modelABHook are
// preserved across the reload — the swap only touches platform-level
// state, never user-level.
//
// Pass an empty systemAPIKeys map to clear all platform keys; pass
// an empty tierDefaults to fall back to DefaultModels for every
// tier. Passing nil for either is treated the same as empty.
func (r *ModelRouter) ReplaceSystemConfig(systemAPIKeys map[Provider]string, tierDefaults map[ModelTier]*ModelConfig) {
	if r == nil {
		return
	}

	// Build the new maps OUTSIDE the lock so the critical section
	// is just two pointer swaps. AES decrypts happen in the caller
	// (wiring layer), so this path is pure in-memory work.
	newKeys := make(map[Provider]string, len(systemAPIKeys))
	for k, v := range systemAPIKeys {
		newKeys[k] = v
	}

	newDefaults := make(map[ModelTier]*ModelConfig, len(DefaultModels))
	for tier, mc := range DefaultModels {
		cloned := mc.Clone()
		if override, ok := tierDefaults[tier]; ok && override != nil {
			cloned = override.Clone()
		}
		if key, ok := newKeys[cloned.Provider]; ok && cloned.APIKey == "" {
			cloned.APIKey = key
		}
		newDefaults[tier] = cloned
	}

	r.mu.Lock()
	r.systemAPIKeys = newKeys
	r.defaultModels = newDefaults
	r.mu.Unlock()

	reloadGen.Add(1)
}

// SystemAPIKeySnapshot returns a defensive copy of the current
// systemAPIKeys map. Used by the admin handler for the dry-run
// "what would change?" view, and by the test-connection probe to
// build a one-off llm.Client without writing to the router. NEVER
// returns the live internal map.
func (r *ModelRouter) SystemAPIKeySnapshot() map[Provider]string {
	if r == nil {
		return map[Provider]string{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[Provider]string, len(r.systemAPIKeys))
	for k, v := range r.systemAPIKeys {
		out[k] = v
	}
	return out
}

// HasProviderKey reports whether the router currently has a
// non-empty platform key for the given provider. Used by the A/B
// admin endpoint to reject `arm.provider = "claude"` configurations
// when no claude key is configured — preventing the silent
// "gemini vs gemini" fake-comparison failure mode.
func (r *ModelRouter) HasProviderKey(p Provider) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.systemAPIKeys[p]
	return ok && v != ""
}
