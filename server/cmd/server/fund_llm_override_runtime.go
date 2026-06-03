// fund_llm_override_runtime.go — S14.B wiring layer that translates
// fund_llm_overrides rows into a llm.FundOverrideHook on the router.
//
// Lives in cmd/server (not internal/llm) because it needs to bridge:
//   * repository.FundLLMOverrideRepo (DB-backed row lookup)
//   * repository.PlatformLLMProviderRepo (decryption + base URL)
//   * internal/llm.ModelConfig (the router's currency)
//
// Lifecycle:
//   1. newLLMRuntimeWithProviderRepo constructs the hook closure
//      at startup if both repos are non-nil.
//   2. The hook is attached via router.SetFundOverrideHook ONCE
//      at startup (no per-Reload re-attach needed because the
//      closure reads DB live on each call, not from an in-memory
//      snapshot).
//   3. ReloadPlatformProviders is unaffected — the hook does its
//      own (provider, label) lookup against the latest DB state.
//
// Why query DB on every call instead of caching:
//   * The hot path is fund-overrides-resolve: 1 indexed lookup on
//     fund_llm_overrides (small, indexed) + 1 indexed lookup on
//     platform_llm_providers (small, indexed). Total ~1-2 ms vs
//     an LLM call of 500ms-5s. Negligible.
//   * Cache invalidation across multi-replica deployments is a
//     hard problem (S14 is single-replica per design). Live DB
//     query sidesteps it entirely.
//   * S13's existing systemAPIKeys cache stays primary for the
//     non-override path — fund override only fires when a fund
//     row matches, which is the minority of calls.

package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/fundai/server/internal/llm"
	"github.com/fundai/server/internal/repository"
	"github.com/google/uuid"
)

// fundOverrideRuntime owns the dependencies the hook closure
// captures. Exists as a struct (not free variables) so the hook
// can be unit-tested with stubbed dependencies later.
type fundOverrideRuntime struct {
	overrideRepo *repository.FundLLMOverrideRepo
	providerRepo *repository.PlatformLLMProviderRepo
	logger       *slog.Logger
}

// newFundOverrideRuntime returns nil when either dep is missing —
// the router then runs without the hook, falling back to the
// pre-S14 priority chain.
func newFundOverrideRuntime(overrideRepo *repository.FundLLMOverrideRepo, providerRepo *repository.PlatformLLMProviderRepo, logger *slog.Logger) *fundOverrideRuntime {
	if overrideRepo == nil || providerRepo == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &fundOverrideRuntime{
		overrideRepo: overrideRepo,
		providerRepo: providerRepo,
		logger:       logger,
	}
}

// Hook returns the closure to be installed on the router. The
// closure is safe for concurrent use: both repos do their own
// locking (sql.DB pool) and no shared in-process state is mutated.
func (r *fundOverrideRuntime) Hook() llm.FundOverrideHook {
	if r == nil {
		return nil
	}
	return r.resolve
}

func (r *fundOverrideRuntime) resolve(ctx context.Context, req *llm.ChatRequest) *llm.FundOverrideDecision {
	if r == nil || req == nil {
		return nil
	}
	fundUUID, err := uuid.Parse(strings.TrimSpace(req.FundID))
	if err != nil || fundUUID == uuid.Nil {
		return nil
	}
	var agentUUID *uuid.UUID
	if strings.TrimSpace(req.AgentID) != "" {
		if parsed, err := uuid.Parse(req.AgentID); err == nil {
			agentUUID = &parsed
		}
	}
	// Tier resolution: prefer req.ModelTier when explicitly set;
	// otherwise consult StepTierMapping (same logic resolveTier
	// uses internally). We can't just call router.resolveTier from
	// here — it's private — so we duplicate the table lookup.
	// Justified: the table is small, exported, and stable.
	tier := ""
	if req.ModelTier.IsValid() {
		tier = string(req.ModelTier)
	} else if req.StepName != "" {
		if t, ok := llm.StepTierMapping[req.StepName]; ok {
			tier = string(t)
		}
	}

	row, err := r.overrideRepo.ResolveForRequest(ctx, fundUUID, agentUUID, req.AgentRole, tier)
	if err != nil {
		// Log-and-continue: a transient DB error must NOT fail the
		// LLM call. Falling through to the next priority layer is
		// the safe behaviour (user/agent default will still resolve).
		r.logger.Warn("fund_override: resolve failed",
			"fund_id", req.FundID, "err", err)
		return nil
	}
	if row == nil {
		return nil
	}

	cfg, err := r.buildConfigForRow(ctx, row)
	if err != nil {
		r.logger.Warn("fund_override: build config failed",
			"fund_id", req.FundID, "override_id", row.ID.String(), "err", err)
		return nil
	}
	if cfg == nil {
		return nil
	}
	return &llm.FundOverrideDecision{
		Config:      cfg,
		OverrideID:  row.ID.String(),
		Specificity: row.Specificity(),
		FundID:      req.FundID,
	}
}

// buildConfigForRow translates a fund_llm_overrides row into a
// fully-resolved ModelConfig the router can use verbatim. Returns
// (nil, nil) when the referenced provider has no active row — the
// caller treats that as "no override, fall through".
func (r *fundOverrideRuntime) buildConfigForRow(ctx context.Context, row *repository.FundLLMOverrideRow) (*llm.ModelConfig, error) {
	if row == nil {
		return nil, errors.New("fund_override: nil row")
	}
	var providerRow *repository.PlatformLLMProviderRow
	var err error
	if row.Label.Valid && strings.TrimSpace(row.Label.String) != "" {
		providerRow, err = r.providerRepo.GetByProviderLabel(ctx, row.Provider, row.Label.String)
	} else {
		providerRow, err = r.providerRepo.GetActiveDefaultForProvider(ctx, row.Provider)
	}
	if err != nil {
		if errors.Is(err, repository.ErrPlatformLLMProviderNotFound) {
			return nil, nil // fall through
		}
		return nil, err
	}
	if providerRow == nil {
		return nil, nil
	}
	plainKey, err := providerRow.PlainAPIKey()
	if err != nil {
		return nil, err
	}
	modelName := strings.TrimSpace(providerRow.ModelName)
	if row.ModelName.Valid && strings.TrimSpace(row.ModelName.String) != "" {
		modelName = strings.TrimSpace(row.ModelName.String)
	}
	cfg := &llm.ModelConfig{
		Provider:    llm.Provider(strings.ToLower(strings.TrimSpace(providerRow.Provider))),
		ModelName:   modelName,
		BaseURL:     strings.TrimSpace(providerRow.BaseURL),
		APIKey:      plainKey,
		MaxTokens:   providerRow.MaxTokens,
		Temperature: providerRow.Temperature,
	}
	return cfg, nil
}
