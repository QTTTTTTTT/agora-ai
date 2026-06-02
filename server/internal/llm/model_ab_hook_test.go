package llm

import (
	"context"
	"testing"
)

// TestResolveModel_ModelABHook_Wins exercises the Sprint 10.1
// router hook: when the hook returns a non-nil decision the
// returned model config must match the arm, NOT the platform
// default for the request's tier.
func TestResolveModel_ModelABHook_Wins(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{
			ProviderOpenAI: "sys-openai",
			ProviderClaude: "sys-claude",
		},
		DefaultModels,
		nil,
		nil,
	)
	router.SetModelABHook(func(_ context.Context, _ *ChatRequest) *ModelABDecision {
		return &ModelABDecision{
			ExperimentID:   "exp-1",
			ExperimentName: "claude vs gpt",
			ArmIndex:       1,
			ArmName:        "treat",
			ArmLabel:       "claude/claude-opus",
			Config: &ModelConfig{
				Provider:  ProviderClaude,
				ModelName: "claude-opus",
				BaseURL:   "https://api.anthropic.com/v1",
				MaxTokens: 4096,
			},
		}
	})

	cfg, err := router.ResolveModel(context.Background(), &ChatRequest{
		StepName: "pm_decision",
		FundID:   "fund-1",
		AgentID:  "ag-1",
		RunID:    "run-1",
	})
	if err != nil {
		t.Fatalf("ResolveModel error: %v", err)
	}
	if cfg.Provider != ProviderClaude || cfg.ModelName != "claude-opus" {
		t.Fatalf("expected arm model, got %s/%s", cfg.Provider, cfg.ModelName)
	}
	if cfg.APIKey != "sys-claude" {
		t.Fatalf("expected system API key to be filled, got %q", cfg.APIKey)
	}
}

// TestResolveModel_ModelABHook_NilFallsThrough verifies that
// when the hook returns nil ("no experiment matches"), the
// router resumes its normal priority chain.
func TestResolveModel_ModelABHook_NilFallsThrough(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{ProviderDeepSeek: "sys-deepseek"},
		DefaultModels,
		nil,
		nil,
	)
	called := false
	router.SetModelABHook(func(_ context.Context, _ *ChatRequest) *ModelABDecision {
		called = true
		return nil
	})

	cfg, err := router.ResolveModel(context.Background(), &ChatRequest{
		StepName: "macro_brief", // → tier standard → deepseek
	})
	if err != nil {
		t.Fatalf("ResolveModel error: %v", err)
	}
	if !called {
		t.Fatalf("hook was not invoked")
	}
	if cfg.Provider != ProviderDeepSeek {
		t.Fatalf("expected DeepSeek default, got %s", cfg.Provider)
	}
}

// TestResolveModel_ExplicitModel_BypassesHook documents the
// forensic-pin contract: when req.Model is set, the experiment
// hook must NOT override it.
func TestResolveModel_ExplicitModel_BypassesHook(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{ProviderOpenAI: "sys-openai"},
		DefaultModels,
		nil,
		nil,
	)
	hookCalls := 0
	router.SetModelABHook(func(_ context.Context, _ *ChatRequest) *ModelABDecision {
		hookCalls++
		return &ModelABDecision{
			Config: &ModelConfig{Provider: ProviderClaude, ModelName: "claude-opus"},
		}
	})

	cfg, err := router.ResolveModel(context.Background(), &ChatRequest{
		Model: "gpt-4o", // explicit pin
	})
	if err != nil {
		t.Fatalf("ResolveModel error: %v", err)
	}
	if cfg.ModelName != "gpt-4o" {
		t.Fatalf("explicit model was overridden, got %q", cfg.ModelName)
	}
	if hookCalls != 0 {
		t.Fatalf("hook was invoked even though req.Model was pinned (count=%d)", hookCalls)
	}
}

// TestSetModelABHook_NilDisables documents that passing nil to
// the setter resets the hook back to "no experiments".
func TestSetModelABHook_NilDisables(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{ProviderDeepSeek: "k"},
		DefaultModels,
		nil,
		nil,
	)
	router.SetModelABHook(func(_ context.Context, _ *ChatRequest) *ModelABDecision {
		return &ModelABDecision{Config: &ModelConfig{Provider: ProviderClaude, ModelName: "x"}}
	})
	router.SetModelABHook(nil)
	cfg, err := router.ResolveModel(context.Background(), &ChatRequest{StepName: "macro_brief"})
	if err != nil {
		t.Fatalf("ResolveModel error: %v", err)
	}
	if cfg.Provider != ProviderDeepSeek {
		t.Fatalf("hook was not disabled, got %s", cfg.Provider)
	}
}
