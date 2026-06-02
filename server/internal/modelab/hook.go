package modelab

import (
	"context"
	"strings"

	"github.com/fundai/server/internal/llm"
)

// HookContext bundles the bits of router state the BuildLLMConfig
// helper needs to materialise an ArmConfig into a full
// llm.ModelConfig. We pass them explicitly (rather than holding a
// pointer to the router) so the modelab package stays independent
// of any concrete ModelRouter implementation — easier to test and
// easier to swap in future.
type HookContext struct {
	// SystemAPIKeys: provider → key. Defaults to the platform
	// system keys. The hook only reads, never mutates.
	SystemAPIKeys map[llm.Provider]string

	// TierDefaults: tier → fallback ModelConfig. Used to fill in
	// BaseURL / MaxTokens / Pricing fields the experiment arm
	// didn't specify. Defaults to llm.DefaultModels.
	TierDefaults map[llm.ModelTier]*llm.ModelConfig
}

// BuildLLMConfig turns an ArmConfig + HookContext into a fully
// populated llm.ModelConfig ready for the router to use. Empty
// arm fields fall back to either the tier default (BaseURL,
// MaxTokens, Temperature, pricing) or the system key store.
//
// The returned config is a fresh allocation — callers can mutate
// it without affecting the tier defaults map.
func BuildLLMConfig(arm ArmConfig, hc HookContext) *llm.ModelConfig {
	tier := arm.ModelTier
	if !tier.IsValid() {
		tier = llm.TierStandard
	}
	var base *llm.ModelConfig
	if hc.TierDefaults != nil {
		base = hc.TierDefaults[tier]
	}
	if base == nil {
		// Fall back to the package-level defaults if the hook
		// context didn't supply tier defaults. Belt-and-braces:
		// the wiring code is supposed to pass these.
		if def, ok := llm.DefaultModels[tier]; ok && def != nil {
			base = def
		} else {
			base = llm.DefaultModels[llm.TierStandard]
		}
	}
	cfg := base.Clone()

	cfg.Provider = arm.Provider
	cfg.ModelName = arm.ModelName
	if strings.TrimSpace(arm.BaseURL) != "" {
		cfg.BaseURL = arm.BaseURL
	} else {
		// If the arm didn't pin a URL and the tier default's URL
		// doesn't match the arm's provider, derive one from the
		// provider's canonical endpoint. Otherwise the user would
		// end up calling Claude through OpenAI's URL.
		cfg.BaseURL = providerDefaultBaseURLFor(arm.Provider, cfg.BaseURL, base)
	}
	if arm.MaxTokens > 0 {
		cfg.MaxTokens = arm.MaxTokens
	}
	if arm.Temperature > 0 {
		cfg.Temperature = arm.Temperature
	}
	cfg.ResolvedTier = tier

	// Wire system API key if any. Custom user keys are NOT
	// applied here — experiments deliberately run on the
	// platform's system key store so cost shows up in the
	// platform's budget, not on the end-user's bill. (Operators
	// don't want to surprise a user with charges for a test
	// they didn't opt into.)
	if key, ok := hc.SystemAPIKeys[arm.Provider]; ok && cfg.APIKey == "" {
		cfg.APIKey = key
	}
	cfg.UsesCustomKey = false

	return cfg
}

// providerDefaultBaseURLFor returns the canonical BaseURL for a
// provider. If the existing fallback URL already matches the
// provider, keep it (so an operator who overrode the platform
// defaults with a different endpoint stays in control). Else
// derive from a hard-coded list mirroring router.go.
func providerDefaultBaseURLFor(p llm.Provider, existing string, fallback *llm.ModelConfig) string {
	if fallback != nil && fallback.Provider == p && strings.TrimSpace(existing) != "" {
		return existing
	}
	switch p {
	case llm.ProviderOpenAI:
		return "https://api.openai.com/v1"
	case llm.ProviderClaude:
		return "https://api.anthropic.com/v1"
	case llm.ProviderDeepSeek:
		return "https://api.deepseek.com/v1"
	case llm.ProviderQwen:
		return "https://dashscope.aliyuncs.com/compatible-mode/v1"
	case llm.ProviderGemini:
		return "https://generativelanguage.googleapis.com/v1beta"
	default:
		return existing
	}
}

// AsLLMHook adapts a *Resolver into a llm.ModelABHook closure
// the router can install via SetModelABHook. The hook context
// is captured by reference — wiring code passes the live
// systemAPIKeys / tierDefaults maps the router itself uses so
// changes propagate without re-installing the hook.
func (r *Resolver) AsLLMHook(hc HookContext) llm.ModelABHook {
	if r == nil {
		return nil
	}
	return func(ctx context.Context, req *llm.ChatRequest) *llm.ModelABDecision {
		if req == nil {
			return nil
		}
		role := SanitizeAgentRole(req.AgentRole)
		d := r.Resolve(ctx, req.FundID, req.AgentID, role, req.StepName, req.RunID)
		if !d.InExperiment || d.Experiment == nil {
			return nil
		}
		cfg := BuildLLMConfig(d.Arm, hc)
		assignmentID := ""
		if d.Assignment != nil {
			assignmentID = d.Assignment.ID
		}
		return &llm.ModelABDecision{
			Config:         cfg,
			ExperimentID:   d.Experiment.ID,
			ExperimentName: d.Experiment.Name,
			ArmIndex:       d.ArmIndex,
			ArmName:        d.Arm.Name,
			ArmLabel:       d.Arm.Label(),
			AssignmentID:   assignmentID,
		}
	}
}
