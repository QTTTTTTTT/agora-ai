// platform_llm_provider_loader.go — S13 startup + hot-reload glue.
//
// Bridges the platform_llm_providers DB table (managed by the
// admin UI) and the in-process llm.ModelRouter that drives every
// LLM call site. The flow is:
//
//   1. On first startup the table is empty. loadPlatformProviders
//      writes one row per non-empty env variable (LLM_API_KEY +
//      OPENAI_API_KEY + CLAUDE_API_KEY + ...) with source='env_seed'
//      so the table reflects exactly what env declared.
//
//   2. Every subsequent process start (and every admin mutation)
//      reads the table and builds (systemAPIKeys, tierDefaults)
//      pair that gets pushed into the router via ReplaceSystemConfig.
//
//   3. The admin handler calls llmRuntime.ReloadPlatformProviders
//      to redo step 2 without restarting the process.
//
// Tier semantics:
//   * Rows with model_tier IS NULL feed BOTH the systemAPIKeys map
//     (so router.ResolveModel can find a key when the request
//     specifies provider=X) AND seed the "any tier" catch-all.
//   * Rows with a concrete model_tier override the built-in
//     DefaultModels[tier]: model_name, base_url, api_key, pricing.
//   * The single is_platform_default=true row (if any) is the
//     last-resort default — its (provider, model_tier=NULL)
//     fingerprint replaces the LLM_PROVIDER / LLM_MODEL env
//     semantics for the catch-all standard tier.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/llm"
	"github.com/fundai/server/internal/repository"
	"github.com/google/uuid"
)

// platformProviderSnapshot is the in-memory shape returned to the
// router. systemAPIKeys + tierDefaults mirror the existing pair
// loadSystemLLMKeys + buildPlatformDefaultModels returned from
// env, so the rest of the wiring layer doesn't change.
type platformProviderSnapshot struct {
	SystemAPIKeys map[llm.Provider]string
	TierDefaults  map[llm.ModelTier]*llm.ModelConfig
	// Default holds the row tagged is_platform_default=true, when
	// present. Used by the failover chain to know which provider
	// to append as the last-resort safety net (replaces the old
	// LLM_PROVIDER env read).
	Default *llm.ModelConfig
}

// loadPlatformProviders runs the env→DB seed (if needed) and then
// returns the live snapshot. repo == nil reverts to env-only mode
// (legacy / test wiring that never touched the table).
func loadPlatformProviders(
	ctx context.Context,
	repo *repository.PlatformLLMProviderRepo,
	envDefaults LLMDefaultsConfig,
	auditLogger *audit.DBLogger,
) (*platformProviderSnapshot, error) {
	if repo == nil {
		return &platformProviderSnapshot{
			SystemAPIKeys: loadSystemLLMKeys(envDefaults),
			TierDefaults:  buildPlatformDefaultModels(envDefaults),
		}, nil
	}

	count, err := repo.Count(ctx)
	if err != nil {
		return nil, fmt.Errorf("platform_llm_providers: count: %w", err)
	}
	if count == 0 {
		if err := seedFromEnv(ctx, repo, envDefaults, auditLogger); err != nil {
			// Seeding failures are NOT fatal — we still serve
			// requests by falling back to env. A future admin
			// upsert will populate the table.
			slog.Warn("platform_llm_providers: env seed failed; falling back to env",
				"err", err.Error())
			return &platformProviderSnapshot{
				SystemAPIKeys: loadSystemLLMKeys(envDefaults),
				TierDefaults:  buildPlatformDefaultModels(envDefaults),
			}, nil
		}
	}

	rows, err := repo.ListAll(ctx, repository.ListFilters{Status: "active"})
	if err != nil {
		return nil, fmt.Errorf("platform_llm_providers: list active: %w", err)
	}
	if len(rows) == 0 {
		// Table is non-empty but every row is disabled/draft.
		// Fall back to env to keep the platform alive — the
		// operator can re-enable a row from the admin UI later.
		slog.Warn("platform_llm_providers: no active rows; falling back to env")
		return &platformProviderSnapshot{
			SystemAPIKeys: loadSystemLLMKeys(envDefaults),
			TierDefaults:  buildPlatformDefaultModels(envDefaults),
		}, nil
	}

	snap, err := snapshotFromRows(rows, envDefaults)
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// snapshotFromRows builds the router-shaped snapshot from a list
// of platform_llm_providers rows. Exported for use by the
// hot-reload path on llmRuntime.
func snapshotFromRows(rows []repository.PlatformLLMProviderRow, envDefaults LLMDefaultsConfig) (*platformProviderSnapshot, error) {
	systemKeys := map[llm.Provider]string{}
	tierDefaults := buildPlatformDefaultModels(envDefaults) // start from env-style baseline
	var platformDefault *llm.ModelConfig

	for i := range rows {
		row := &rows[i]
		provider := llm.Provider(strings.ToLower(row.Provider))
		plaintext, err := row.PlainAPIKey()
		if err != nil {
			// One bad row should not blow up the whole load.
			// Log and skip; the admin UI will show this row's
			// last_health_check_result so the operator sees it.
			slog.Warn("platform_llm_providers: decrypt failed",
				"row_id", row.ID.String(),
				"provider", row.Provider,
				"label", row.Label,
				"err", err.Error())
			continue
		}
		systemKeys[provider] = plaintext

		// Build a ModelConfig per row so tier-specific rows can
		// drive tierDefaults.
		cfg := &llm.ModelConfig{
			Provider:         provider,
			ModelName:        row.ModelName,
			BaseURL:          row.BaseURL,
			APIKey:           plaintext,
			MaxTokens:        row.MaxTokens,
			Temperature:      row.Temperature,
			InputPricePer1M:  row.InputPricePer1M.Float64,
			OutputPricePer1M: row.OutputPricePer1M.Float64,
			CostPer1M:        row.CostPer1M.Float64,
		}

		if row.ModelTier.Valid {
			tier := llm.ModelTier(row.ModelTier.String)
			tierDefaults[tier] = cfg
		}
		if row.IsPlatformDefault {
			platformDefault = cfg
		}
	}

	// When a platform default row exists, treat it as the "any
	// tier when no override" config. Mirrors the old
	// LLM_PROVIDER + LLM_MODEL env semantics.
	if platformDefault != nil {
		if _, ok := tierDefaults[llm.TierStandard]; !ok {
			cloned := *platformDefault
			tierDefaults[llm.TierStandard] = &cloned
		}
		if _, ok := tierDefaults[llm.TierSimple]; !ok {
			cloned := *platformDefault
			tierDefaults[llm.TierSimple] = &cloned
		}
		if _, ok := tierDefaults[llm.TierCritical]; !ok {
			cloned := *platformDefault
			tierDefaults[llm.TierCritical] = &cloned
		}
	}

	return &platformProviderSnapshot{
		SystemAPIKeys: systemKeys,
		TierDefaults:  tierDefaults,
		Default:       platformDefault,
	}, nil
}

// seedFromEnv writes one row per non-empty env-derived (provider,
// label) pair the first time the platform boots after the S13
// migration. The default LLM_* tuple becomes the
// is_platform_default row; per-provider <PROVIDER>_API_KEY rows
// become non-default companions. Idempotent at the row level via
// the (provider, label) UNIQUE.
func seedFromEnv(
	ctx context.Context,
	repo *repository.PlatformLLMProviderRepo,
	envDefaults LLMDefaultsConfig,
	auditLogger *audit.DBLogger,
) error {
	secret := strings.TrimSpace(os.Getenv("MODEL_CONFIG_API_KEY_SECRET"))
	if secret == "" {
		secret = strings.TrimSpace(os.Getenv("API_KEY_ENCRYPTION_SECRET"))
	}
	if secret == "" {
		return errors.New("MODEL_CONFIG_API_KEY_SECRET not set; cannot seed")
	}

	seeded := []string{}

	// 1) Per-provider keys (<PROVIDER>_API_KEY env). These become
	//    non-default rows the failover chain and explicit
	//    provider=X requests use.
	type envProvider struct {
		Provider llm.Provider
		KeyEnvs  []string
		Model    string
		BaseURL  string
	}
	envProviders := []envProvider{
		{llm.ProviderOpenAI, []string{"OPENAI_API_KEY"}, strings.TrimSpace(os.Getenv("OPENAI_MODEL")), strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))},
		{llm.ProviderClaude, []string{"CLAUDE_API_KEY", "ANTHROPIC_API_KEY"}, strings.TrimSpace(firstNonEmptyEnv("CLAUDE_MODEL", "ANTHROPIC_MODEL")), strings.TrimSpace(firstNonEmptyEnv("CLAUDE_BASE_URL", "ANTHROPIC_BASE_URL"))},
		{llm.ProviderDeepSeek, []string{"DEEPSEEK_API_KEY"}, strings.TrimSpace(os.Getenv("DEEPSEEK_MODEL")), strings.TrimSpace(os.Getenv("DEEPSEEK_BASE_URL"))},
		{llm.ProviderQwen, []string{"QWEN_API_KEY"}, strings.TrimSpace(os.Getenv("QWEN_MODEL")), strings.TrimSpace(os.Getenv("QWEN_BASE_URL"))},
		{llm.ProviderGemini, []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}, strings.TrimSpace(firstNonEmptyEnv("GEMINI_MODEL", "GOOGLE_MODEL")), strings.TrimSpace(firstNonEmptyEnv("GEMINI_BASE_URL", "GOOGLE_BASE_URL"))},
	}
	for _, ep := range envProviders {
		key := strings.TrimSpace(firstNonEmptyEnv(ep.KeyEnvs...))
		if key == "" {
			continue
		}
		baseURL := ep.BaseURL
		if baseURL == "" {
			baseURL = providerDefaultBaseURL(ep.Provider)
		}
		model := ep.Model
		if model == "" {
			model = defaultModelForProvider(ep.Provider)
		}
		if baseURL == "" || model == "" {
			continue
		}
		label := string(ep.Provider) + "-env"
		if _, err := repo.Upsert(ctx, repository.UpsertInput{
			Provider:        string(ep.Provider),
			Label:           label,
			ModelName:       model,
			BaseURL:         baseURL,
			APIKeyPlaintext: key,
			Status:          "active",
			Source:          "env_seed",
		}); err != nil {
			slog.Warn("platform_llm_providers: env_seed upsert",
				"provider", ep.Provider, "err", err.Error())
			continue
		}
		seeded = append(seeded, string(ep.Provider))
	}

	// 2) The platform default (LLM_PROVIDER / LLM_MODEL / LLM_BASE_URL
	//    / LLM_API_KEY env quartet). This row carries
	//    is_platform_default=true after the SetDefault toggle.
	if defaultKey := strings.TrimSpace(envDefaults.Global.APIKey); defaultKey != "" {
		provider := llmProviderFromString(envDefaults.Global.Provider)
		if provider == "" {
			provider = llm.ProviderOpenAI
		}
		model := strings.TrimSpace(envDefaults.Global.Model)
		if model == "" {
			model = defaultModelForProvider(provider)
		}
		baseURL := strings.TrimSpace(envDefaults.Global.BaseURL)
		if baseURL == "" {
			baseURL = providerDefaultBaseURL(provider)
		}
		if model != "" && baseURL != "" {
			row, err := repo.Upsert(ctx, repository.UpsertInput{
				Provider:        string(provider),
				Label:           "platform-default",
				ModelName:       model,
				BaseURL:         baseURL,
				APIKeyPlaintext: defaultKey,
				Status:          "active",
				Source:          "env_seed",
			})
			if err != nil {
				slog.Warn("platform_llm_providers: env_seed default", "err", err.Error())
			} else if row != nil {
				if err := repo.SetDefault(ctx, row.ID, uuid.NullUUID{}); err != nil {
					slog.Warn("platform_llm_providers: setdefault on seed", "err", err.Error())
				}
				seeded = append(seeded, "platform-default("+string(provider)+")")
			}
		}
	}

	// admin_change_log requires a non-null actor_user_id with an FK
	// to users(id); env-seed happens before any user has signed in
	// so we record the event as a startup slog line instead. The
	// auditLogger parameter is still threaded through in case a
	// future iteration creates a synthetic "system" user row to
	// satisfy the FK without holes.
	if len(seeded) > 0 {
		slog.Info("platform_llm_providers: initial env seed complete",
			"seeded", seeded,
			"sources", "LLM_* + <PROVIDER>_API_KEY env",
			"action", "platform_llm_providers_env_seed")
	}
	_ = auditLogger // suppress unused-import warning when audit gets removed in the future
	return nil
}

// defaultModelForProvider supplies a safe fallback model name when
// the env didn't set <PROVIDER>_MODEL. Mirrors the production
// defaults the failover chain uses.
func defaultModelForProvider(p llm.Provider) string {
	switch p {
	case llm.ProviderOpenAI:
		return "gpt-4o"
	case llm.ProviderClaude:
		return "claude-3-5-sonnet-20241022"
	case llm.ProviderDeepSeek:
		return "deepseek-chat"
	case llm.ProviderQwen:
		return "qwen-max"
	case llm.ProviderGemini:
		return "gemini-1.5-pro"
	default:
		return ""
	}
}
