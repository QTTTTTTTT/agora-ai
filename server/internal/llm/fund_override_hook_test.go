package llm

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestResolveModel_FundOverrideHook_Wins verifies that when a fund
// override hook returns a non-nil decision, the returned config is
// that decision's config — even if the user has an agent default
// or a tier override set.
func TestResolveModel_FundOverrideHook_Wins(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{
			ProviderOpenAI: "sys-openai",
			ProviderClaude: "sys-claude",
		},
		DefaultModels, nil, nil,
	)
	// Pre-populate a user agent default (should be overridden by fund hook).
	router.ReplaceAgentConfigs("user-1", map[string]*ModelConfig{
		"agent-1": {
			Provider:  ProviderOpenAI,
			ModelName: "gpt-4o",
			BaseURL:   "https://api.openai.com/v1",
			APIKey:    "user-key",
		},
	})

	router.SetFundOverrideHook(func(_ context.Context, _ *ChatRequest) *FundOverrideDecision {
		return &FundOverrideDecision{
			FundID:      "fund-1",
			OverrideID:  "ov-1",
			Specificity: 8,
			Config: &ModelConfig{
				Provider:  ProviderClaude,
				ModelName: "claude-3-5-sonnet",
				BaseURL:   "https://api.anthropic.com/v1",
				APIKey:    "fund-claude-key",
				MaxTokens: 4096,
			},
		}
	})

	cfg, err := router.ResolveModel(context.Background(), &ChatRequest{
		UserID:   "user-1",
		AgentID:  "agent-1",
		FundID:   "fund-1",
		StepName: "pm_decision",
	})
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if cfg.Provider != ProviderClaude {
		t.Fatalf("expected fund-override provider=claude, got %s", cfg.Provider)
	}
	if cfg.ModelName != "claude-3-5-sonnet" {
		t.Fatalf("expected claude model, got %s", cfg.ModelName)
	}
	if cfg.APIKey != "fund-claude-key" {
		t.Fatalf("expected hook's API key intact, got %q", cfg.APIKey)
	}
}

// TestResolveModel_FundOverrideHook_NilFallsThrough verifies the
// fall-through contract: nil decision → router continues to agent
// default / user override / platform default.
func TestResolveModel_FundOverrideHook_NilFallsThrough(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{
			ProviderOpenAI: "sys-openai",
		},
		DefaultModels, nil, nil,
	)
	router.ReplaceAgentConfigs("user-1", map[string]*ModelConfig{
		"agent-1": {
			Provider:  ProviderOpenAI,
			ModelName: "gpt-4o",
			BaseURL:   "https://api.openai.com/v1",
		},
	})

	called := false
	router.SetFundOverrideHook(func(_ context.Context, _ *ChatRequest) *FundOverrideDecision {
		called = true
		return nil
	})

	cfg, err := router.ResolveModel(context.Background(), &ChatRequest{
		UserID:   "user-1",
		AgentID:  "agent-1",
		FundID:   "fund-1",
		StepName: "pm_decision",
	})
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if !called {
		t.Fatalf("hook was not invoked")
	}
	// Falls through to agent default.
	if cfg.Provider != ProviderOpenAI || cfg.ModelName != "gpt-4o" {
		t.Fatalf("expected fall-through to agent default, got %s/%s", cfg.Provider, cfg.ModelName)
	}
}

// TestResolveModel_ModelABHook_TrumpsFundOverride documents the
// invariant from S14.B6 (plan): A/B experiments outrank fund
// overrides. If both hooks would match, A/B wins.
func TestResolveModel_ModelABHook_TrumpsFundOverride(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{
			ProviderOpenAI: "sys-openai",
			ProviderClaude: "sys-claude",
		},
		DefaultModels, nil, nil,
	)

	abCalls, fundCalls := 0, 0
	router.SetModelABHook(func(_ context.Context, _ *ChatRequest) *ModelABDecision {
		abCalls++
		return &ModelABDecision{
			ExperimentID: "exp-1",
			ArmIndex:     0,
			ArmName:      "control",
			Config: &ModelConfig{
				Provider:  ProviderOpenAI,
				ModelName: "gpt-4o",
				BaseURL:   "https://api.openai.com/v1",
			},
		}
	})
	router.SetFundOverrideHook(func(_ context.Context, _ *ChatRequest) *FundOverrideDecision {
		fundCalls++
		return &FundOverrideDecision{
			Config: &ModelConfig{
				Provider:  ProviderClaude,
				ModelName: "claude-3-5-sonnet",
				BaseURL:   "https://api.anthropic.com/v1",
				APIKey:    "fund-claude-key",
			},
		}
	})

	cfg, err := router.ResolveModel(context.Background(), &ChatRequest{
		FundID:   "fund-1",
		StepName: "pm_decision",
	})
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if cfg.Provider != ProviderOpenAI {
		t.Fatalf("expected A/B winner (openai), got %s", cfg.Provider)
	}
	if abCalls != 1 {
		t.Fatalf("expected A/B hook to be called once, got %d", abCalls)
	}
	if fundCalls != 0 {
		t.Fatalf("fund override hook MUST NOT run when A/B wins, called %d times", fundCalls)
	}
}

// TestResolveModel_ExplicitModel_BypassesFundOverride documents
// the same forensic-pin contract for the fund layer.
func TestResolveModel_ExplicitModel_BypassesFundOverride(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{ProviderOpenAI: "sys-openai"},
		DefaultModels, nil, nil,
	)
	fundCalls := 0
	router.SetFundOverrideHook(func(_ context.Context, _ *ChatRequest) *FundOverrideDecision {
		fundCalls++
		return &FundOverrideDecision{
			Config: &ModelConfig{Provider: ProviderClaude, ModelName: "claude-opus"},
		}
	})

	cfg, err := router.ResolveModel(context.Background(), &ChatRequest{
		Model: "gpt-4o", // explicit pin — must be a model PlatformModels knows about
	})
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if cfg.ModelName != "gpt-4o" {
		t.Fatalf("explicit pin was overridden, got %q", cfg.ModelName)
	}
	if fundCalls != 0 {
		t.Fatalf("fund hook MUST NOT run when explicit pin succeeds, called %d", fundCalls)
	}
}

// TestSetFundOverrideHook_NilDisables documents that passing nil
// disables the hook — used at boot before the repo is ready.
func TestSetFundOverrideHook_NilDisables(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{ProviderDeepSeek: "sys-deepseek"},
		DefaultModels, nil, nil,
	)
	calls := atomic.Int64{}
	router.SetFundOverrideHook(func(_ context.Context, _ *ChatRequest) *FundOverrideDecision {
		calls.Add(1)
		return &FundOverrideDecision{
			Config: &ModelConfig{Provider: ProviderClaude, ModelName: "claude-opus"},
		}
	})
	router.SetFundOverrideHook(nil) // disable

	cfg, err := router.ResolveModel(context.Background(), &ChatRequest{
		StepName: "macro_brief",
	})
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("hook should not run after being set to nil, ran %d times", calls.Load())
	}
	if cfg.Provider == ProviderClaude {
		t.Fatalf("expected fall-through default, got claude")
	}
}

// TestResolveModel_ABNil_FundOverrideStillRuns documents the seam
// between hooks: when the A/B hook is wired but returns nil, the
// router MUST still invoke the fund override hook. Without this
// invariant, attaching an A/B hook would silently disable fund
// overrides for every fund not currently in an experiment — a
// classic correctness foot-gun.
func TestResolveModel_ABNil_FundOverrideStillRuns(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{
			ProviderOpenAI: "sys-openai",
			ProviderClaude: "sys-claude",
		},
		DefaultModels, nil, nil,
	)

	abCalls := atomic.Int64{}
	fundCalls := atomic.Int64{}

	router.SetModelABHook(func(_ context.Context, _ *ChatRequest) *ModelABDecision {
		abCalls.Add(1)
		return nil // no experiment matched
	})
	router.SetFundOverrideHook(func(_ context.Context, _ *ChatRequest) *FundOverrideDecision {
		fundCalls.Add(1)
		return &FundOverrideDecision{
			FundID:     "fund-1",
			OverrideID: "ov-after-ab-nil",
			Config: &ModelConfig{
				Provider:  ProviderClaude,
				ModelName: "claude-3-5-sonnet",
				BaseURL:   "https://api.anthropic.com/v1",
				APIKey:    "fund-claude-key",
			},
		}
	})

	cfg, err := router.ResolveModel(context.Background(), &ChatRequest{
		FundID:   "fund-1",
		StepName: "pm_decision",
	})
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if abCalls.Load() != 1 {
		t.Fatalf("A/B hook should run exactly once, ran %d", abCalls.Load())
	}
	if fundCalls.Load() != 1 {
		t.Fatalf("fund hook MUST still run when A/B returns nil, ran %d", fundCalls.Load())
	}
	if cfg.Provider != ProviderClaude || cfg.ModelName != "claude-3-5-sonnet" {
		t.Fatalf("expected fund override winner, got %s/%s", cfg.Provider, cfg.ModelName)
	}
}

// TestResolveModel_FundOverrideBeatsUserTierOverride verifies that
// a fund-level override outranks BOTH the agent default and the
// user's per-tier override. The marketplace policy is: the strategy
// owner's fund-level intent wins over the subscriber's personal
// preference. (The companion test above only covers agent default;
// this one closes the tier-override gap.)
func TestResolveModel_FundOverrideBeatsUserTierOverride(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{
			ProviderOpenAI: "sys-openai",
			ProviderClaude: "sys-claude",
		},
		DefaultModels, nil, nil,
	)
	router.SetUserOverride("user-1", TierCritical, &ModelConfig{
		Provider:  ProviderOpenAI,
		ModelName: "gpt-4o",
		BaseURL:   "https://api.openai.com/v1",
		APIKey:    "user-tier-key",
	})

	router.SetFundOverrideHook(func(_ context.Context, _ *ChatRequest) *FundOverrideDecision {
		return &FundOverrideDecision{
			FundID: "fund-1",
			Config: &ModelConfig{
				Provider:  ProviderClaude,
				ModelName: "claude-3-5-sonnet",
				BaseURL:   "https://api.anthropic.com/v1",
				APIKey:    "fund-claude-key",
			},
		}
	})

	cfg, err := router.ResolveModel(context.Background(), &ChatRequest{
		UserID:   "user-1",
		FundID:   "fund-1",
		StepName: "pm_plan", // → TierCritical, hits the user override
	})
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if cfg.Provider != ProviderClaude {
		t.Fatalf("fund override should outrank user tier override, got %s", cfg.Provider)
	}
	if cfg.APIKey != "fund-claude-key" {
		t.Fatalf("fund hook's API key must survive finalize, got %q", cfg.APIKey)
	}
}

// TestResolveModel_PriorityChain_E2E exercises the full chain by
// progressively disabling each layer and verifying the next-most-
// specific source wins. The intent is a single regression guard
// that catches any future reordering bug in router.ResolveModel.
//
// Layers checked (highest first):
//   - explicit req.Model
//   - A/B hook
//   - fund override hook
//   - agent default
//   - user tier override
//   - platform default
func TestResolveModel_PriorityChain_E2E(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{
			ProviderOpenAI:   "sys-openai",
			ProviderClaude:   "sys-claude",
			ProviderDeepSeek: "sys-deepseek",
		},
		DefaultModels, nil, nil,
	)

	router.ReplaceAgentConfigs("user-1", map[string]*ModelConfig{
		"agent-1": {
			Provider:  ProviderDeepSeek,
			ModelName: "deepseek-chat",
			BaseURL:   "https://api.deepseek.com/v1",
		},
	})
	router.SetUserOverride("user-1", TierCritical, &ModelConfig{
		Provider:  ProviderOpenAI,
		ModelName: "gpt-4o-mini",
		BaseURL:   "https://api.openai.com/v1",
	})

	ab := func(_ context.Context, _ *ChatRequest) *ModelABDecision {
		return &ModelABDecision{
			ExperimentID: "exp",
			ArmName:      "control",
			Config: &ModelConfig{
				Provider:  ProviderClaude,
				ModelName: "claude-3-haiku",
				BaseURL:   "https://api.anthropic.com/v1",
			},
		}
	}
	fund := func(_ context.Context, _ *ChatRequest) *FundOverrideDecision {
		return &FundOverrideDecision{
			FundID: "fund-1",
			Config: &ModelConfig{
				Provider:  ProviderClaude,
				ModelName: "claude-3-5-sonnet",
				BaseURL:   "https://api.anthropic.com/v1",
				APIKey:    "fund-key",
			},
		}
	}
	baseReq := func() *ChatRequest {
		return &ChatRequest{
			UserID:   "user-1",
			AgentID:  "agent-1",
			FundID:   "fund-1",
			StepName: "pm_plan", // TierCritical
		}
	}

	// 1. Explicit pin trumps everything.
	router.SetModelABHook(ab)
	router.SetFundOverrideHook(fund)
	req1 := baseReq()
	req1.Model = "gpt-4o"
	cfg, err := router.ResolveModel(context.Background(), req1)
	if err != nil {
		t.Fatalf("step 1: %v", err)
	}
	if cfg.ModelName != "gpt-4o" {
		t.Fatalf("step 1 (explicit): want gpt-4o, got %s", cfg.ModelName)
	}

	// 2. A/B beats fund.
	cfg, err = router.ResolveModel(context.Background(), baseReq())
	if err != nil {
		t.Fatalf("step 2: %v", err)
	}
	if cfg.ModelName != "claude-3-haiku" {
		t.Fatalf("step 2 (A/B): want claude-3-haiku, got %s", cfg.ModelName)
	}

	// 3. Drop A/B → fund wins.
	router.SetModelABHook(nil)
	cfg, err = router.ResolveModel(context.Background(), baseReq())
	if err != nil {
		t.Fatalf("step 3: %v", err)
	}
	if cfg.ModelName != "claude-3-5-sonnet" {
		t.Fatalf("step 3 (fund): want claude-3-5-sonnet, got %s", cfg.ModelName)
	}

	// 4. Drop fund → agent default wins (deepseek-chat).
	router.SetFundOverrideHook(nil)
	cfg, err = router.ResolveModel(context.Background(), baseReq())
	if err != nil {
		t.Fatalf("step 4: %v", err)
	}
	if cfg.ModelName != "deepseek-chat" {
		t.Fatalf("step 4 (agent default): want deepseek-chat, got %s", cfg.ModelName)
	}

	// 5. Drop agent → user tier override wins (gpt-4o-mini).
	router.ReplaceAgentConfigs("user-1", nil)
	cfg, err = router.ResolveModel(context.Background(), baseReq())
	if err != nil {
		t.Fatalf("step 5: %v", err)
	}
	if cfg.ModelName != "gpt-4o-mini" {
		t.Fatalf("step 5 (user tier): want gpt-4o-mini, got %s", cfg.ModelName)
	}

	// 6. Drop user override → platform default (TierCritical = gpt-4o).
	router.RemoveUserOverride("user-1", TierCritical)
	cfg, err = router.ResolveModel(context.Background(), baseReq())
	if err != nil {
		t.Fatalf("step 6: %v", err)
	}
	if cfg.ModelName != "gpt-4o" {
		t.Fatalf("step 6 (platform default): want gpt-4o, got %s", cfg.ModelName)
	}
}

// TestFundOverrideHook_RaceWithResolve exercises concurrent
// SetFundOverrideHook + ResolveModel under the race detector. The
// sync model is identical to the A/B hook so this is a regression
// guard, not a behaviour check.
func TestFundOverrideHook_RaceWithResolve(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{ProviderOpenAI: "sys-openai"},
		DefaultModels, nil, nil,
	)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			req := &ChatRequest{StepName: "macro_brief"}
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = router.ResolveModel(ctx, req)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		flip := false
		for {
			select {
			case <-stop:
				return
			default:
				if flip {
					router.SetFundOverrideHook(nil)
				} else {
					router.SetFundOverrideHook(func(_ context.Context, _ *ChatRequest) *FundOverrideDecision {
						return nil
					})
				}
				flip = !flip
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}
