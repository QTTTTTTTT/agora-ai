// user_byok_runtime.go — Phase B-2/3 wiring layer that translates
// user_llm_keys rows into a llm.UserOverrideHook on the router.
//
// Mirrors fund_llm_override_runtime.go but for the per-user BYOK
// surface (/advisor consultations):
//
//   * userbyok.Repo (DB-backed encrypted key lookup)
//   * internal/llm.ModelConfig (the router's currency)
//
// Lifecycle:
//   1. llmRuntime construction wires the runtime once via
//      SetUserBYOKRepo (called from main.go).
//   2. The hook closure reads DB live on each call. The hot path
//      is one indexed SELECT on user_llm_keys + one decrypt of
//      ~200 byte ciphertext (negligible vs the 500ms-5s LLM call).
//   3. Best-effort RecordUsage fires in a background goroutine
//      after the model call (the router doesn't tell us whether
//      the call succeeded, so we settle for "we attempted to
//      route through this key" semantics).
//
// Gating:
//   * Skip when req.UserID is empty (system-only calls).
//   * Skip when req.FundID is set (fund-mode calls; fund overrides
//     are the authoritative source there).
//   * Skip when the resolved tier has no provider mapping.
//   * Returns nil when the user has no active key for the resolved
//     provider — the chain falls through to FundOverrideHook /
//     agent default / platform default.

package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/fundai/server/internal/llm"
	"github.com/fundai/server/internal/userbyok"
)

// userBYOKRuntime owns the dependencies the hook closure captures.
// A struct (not free variables) so the hook is unit-testable with
// stubbed dependencies later.
type userBYOKRuntime struct {
	repo   *userbyok.Repo
	logger *slog.Logger
}

// newUserBYOKRuntime returns nil when the repo is missing — the
// router then runs without the hook and BYOK is effectively
// disabled platform-wide.
func newUserBYOKRuntime(repo *userbyok.Repo, logger *slog.Logger) *userBYOKRuntime {
	if repo == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &userBYOKRuntime{repo: repo, logger: logger}
}

// Hook returns the closure to install on the router. The closure
// is safe for concurrent use: the repo is sql.DB-backed and does
// its own locking; the runtime is read-only after construction.
func (r *userBYOKRuntime) Hook() llm.UserOverrideHook {
	if r == nil {
		return nil
	}
	return r.resolve
}

func (r *userBYOKRuntime) resolve(ctx context.Context, req *llm.ChatRequest) *llm.UserOverrideDecision {
	if r == nil || req == nil {
		return nil
	}
	userID := strings.TrimSpace(req.UserID)
	if userID == "" {
		return nil
	}
	// Fund-mode calls go through fund_llm_overrides, not user BYOK.
	// We want a clean separation: a user's personal key is only
	// used when they're the one explicitly driving the call (i.e.
	// /advisor mode where there is no fund).
	if strings.TrimSpace(req.FundID) != "" {
		return nil
	}
	provider := r.providerForRequest(req)
	if provider == "" {
		return nil
	}
	byokProvider := mapLLMProviderToBYOKName(provider)
	if byokProvider == "" {
		return nil
	}

	key, err := r.repo.GetActiveForRouting(ctx, userID, byokProvider)
	if err != nil {
		if !isExpectedBYOKMiss(err) {
			r.logger.Warn("user_byok: lookup failed",
				"err", err.Error(), "user_id", userID, "provider", byokProvider)
		}
		return nil
	}
	cfg := &llm.ModelConfig{
		Provider:      provider,
		ModelName:     firstNonEmptyByokString(key.ModelName, byokDefaultModelForProvider(provider)),
		BaseURL:       firstNonEmptyByokString(key.BaseURL, byokDefaultBaseURLForProvider(provider)),
		APIKey:        key.PlaintextAPIKey,
		MaxTokens:     2048,
		Temperature:   0.4,
		UsesCustomKey: true,
	}

	go func(id string) {
		// Detached context: the parent ctx may be cancelled the
		// instant the model call returns; we still want to stamp
		// last_used_at. Tight timeout so a slow DB doesn't pile
		// up goroutines.
		bg, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := r.repo.RecordUsage(bg, id); err != nil {
			r.logger.Debug("user_byok: record usage failed", "err", err.Error())
		}
	}(key.ID)

	return &llm.UserOverrideDecision{
		Config:    cfg,
		UserKeyID: key.ID,
		Provider:  provider,
		UserID:    userID,
	}
}

// providerForRequest derives the LLM provider the request is
// targeting. Currently we mirror the router's resolveTier logic:
// the tier maps to a platform default which has a provider field.
// We DO NOT call into the router (it would deadlock under the
// read lock) — we reuse llm.DefaultModels + llm.StepTierMapping
// directly.
func (r *userBYOKRuntime) providerForRequest(req *llm.ChatRequest) llm.Provider {
	tier := req.ModelTier
	if !tier.IsValid() {
		if t, ok := llm.StepTierMapping[req.StepName]; ok {
			tier = t
		} else {
			tier = llm.TierStandard
		}
	}
	def := llm.DefaultModels[tier]
	if def == nil {
		return ""
	}
	return def.Provider
}

// mapLLMProviderToBYOKName bridges the LLM-router-side provider
// name (openai/claude/deepseek/qwen/gemini) to the BYOK store's
// provider taxonomy (openai/anthropic/deepseek/kimi/doubao/qwen).
//
// Returns "" when the provider isn't BYOK-supported — that's the
// fallback signal for "let the platform's pool handle this call".
func mapLLMProviderToBYOKName(p llm.Provider) string {
	switch p {
	case llm.ProviderOpenAI:
		return "openai"
	case llm.ProviderClaude:
		return "anthropic"
	case llm.ProviderDeepSeek:
		return "deepseek"
	case llm.ProviderQwen:
		return "qwen"
	default:
		return ""
	}
}

func byokDefaultModelForProvider(p llm.Provider) string {
	if cfg := byokDefaultPlatformConfigFor(p); cfg != nil {
		return cfg.ModelName
	}
	// Fall back to the existing helper that knows env-overrideable
	// defaults.
	return defaultModelForProvider(p)
}

func byokDefaultBaseURLForProvider(p llm.Provider) string {
	if cfg := byokDefaultPlatformConfigFor(p); cfg != nil {
		return cfg.BaseURL
	}
	return ""
}

func byokDefaultPlatformConfigFor(p llm.Provider) *llm.ModelConfig {
	// Pick the highest-tier default for this provider so a
	// "/advisor deep consult" goes through the strongest model
	// the user paid for. Tier ordering: critical > standard > simple.
	for _, tier := range []llm.ModelTier{llm.TierCritical, llm.TierStandard, llm.TierSimple} {
		if cfg, ok := llm.DefaultModels[tier]; ok && cfg != nil && cfg.Provider == p {
			return cfg
		}
	}
	return nil
}

func firstNonEmptyByokString(a, b string) string {
	if a = strings.TrimSpace(a); a != "" {
		return a
	}
	return strings.TrimSpace(b)
}

func isExpectedBYOKMiss(err error) bool {
	if err == nil {
		return true
	}
	return errors.Is(err, userbyok.ErrNotFound) ||
		errors.Is(err, userbyok.ErrEncryptionUnconfigured)
}
