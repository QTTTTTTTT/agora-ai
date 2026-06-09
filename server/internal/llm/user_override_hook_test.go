package llm

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestResolveModel_UserOverrideHook_Wins verifies that when a user
// override hook returns a non-nil decision, the returned config is
// the hook's config — outranking fund overrides, agent defaults,
// and platform defaults. UsesCustomKey must be flagged on so the
// downstream cost accounting credits the user, not the platform.
func TestResolveModel_UserOverrideHook_Wins(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{
			ProviderOpenAI: "sys-openai",
			ProviderClaude: "sys-claude",
		},
		DefaultModels, nil, nil,
	)
	router.SetFundOverrideHook(func(_ context.Context, _ *ChatRequest) *FundOverrideDecision {
		return &FundOverrideDecision{
			FundID: "fund-1",
			Config: &ModelConfig{
				Provider: ProviderClaude, ModelName: "claude-3-5-sonnet",
				BaseURL: "https://api.anthropic.com/v1", APIKey: "fund-claude-key",
			},
		}
	})
	router.SetUserOverrideHook(func(_ context.Context, _ *ChatRequest) *UserOverrideDecision {
		return &UserOverrideDecision{
			UserKeyID: "key-1",
			Provider:  ProviderOpenAI,
			UserID:    "user-1",
			Config: &ModelConfig{
				Provider: ProviderOpenAI, ModelName: "gpt-4o",
				BaseURL: "https://api.openai.com/v1", APIKey: "user-byok-sk",
			},
		}
	})

	cfg, err := router.ResolveModel(context.Background(), &ChatRequest{
		UserID:   "user-1",
		StepName: "pm_plan",
	})
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if cfg.APIKey != "user-byok-sk" {
		t.Errorf("expected user BYOK key, got %q", cfg.APIKey)
	}
	if !cfg.UsesCustomKey {
		t.Errorf("UsesCustomKey should be flagged on for BYOK")
	}
}

// TestResolveModel_UserOverrideHook_NilFallsThrough — same
// fall-through contract every other hook honours.
func TestResolveModel_UserOverrideHook_NilFallsThrough(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{ProviderOpenAI: "sys-openai"},
		DefaultModels, nil, nil,
	)
	called := false
	router.SetUserOverrideHook(func(_ context.Context, _ *ChatRequest) *UserOverrideDecision {
		called = true
		return nil
	})

	cfg, err := router.ResolveModel(context.Background(), &ChatRequest{
		UserID: "user-1", StepName: "macro_brief",
	})
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if !called {
		t.Fatalf("hook not invoked")
	}
	if cfg.Provider != ProviderDeepSeek { // default TierStandard
		t.Fatalf("expected fall-through default, got %s", cfg.Provider)
	}
}

// TestResolveModel_ABTrumpsUserOverride documents the priority
// invariant — A/B experiments still beat user BYOK.
func TestResolveModel_ABTrumpsUserOverride(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{ProviderOpenAI: "sys-openai"},
		DefaultModels, nil, nil,
	)
	router.SetModelABHook(func(_ context.Context, _ *ChatRequest) *ModelABDecision {
		return &ModelABDecision{
			ExperimentID: "e1",
			ArmName:      "control",
			Config: &ModelConfig{
				Provider: ProviderOpenAI, ModelName: "gpt-4o-mini",
				BaseURL: "https://api.openai.com/v1",
			},
		}
	})
	userCalls := atomic.Int64{}
	router.SetUserOverrideHook(func(_ context.Context, _ *ChatRequest) *UserOverrideDecision {
		userCalls.Add(1)
		return &UserOverrideDecision{
			Config: &ModelConfig{
				Provider: ProviderClaude, ModelName: "claude-3-haiku",
				APIKey: "user-byok",
			},
		}
	})

	cfg, err := router.ResolveModel(context.Background(), &ChatRequest{
		UserID: "user-1", StepName: "pm_plan",
	})
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if cfg.ModelName != "gpt-4o-mini" {
		t.Fatalf("A/B should win, got %s", cfg.ModelName)
	}
	if userCalls.Load() != 0 {
		t.Errorf("user override MUST NOT run when A/B wins, ran %d", userCalls.Load())
	}
}

// TestResolveModel_UserOverrideBeatsFund — invariant we promised in
// the plan: in /advisor mode (fund-less) the user's BYOK key wins
// against any fund override the platform might surface. The hook
// position above FundOverrideHook in router.ResolveModel is what
// makes that true.
func TestResolveModel_UserOverrideBeatsFund(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{ProviderOpenAI: "sys-openai"},
		DefaultModels, nil, nil,
	)
	router.SetFundOverrideHook(func(_ context.Context, _ *ChatRequest) *FundOverrideDecision {
		return &FundOverrideDecision{
			Config: &ModelConfig{Provider: ProviderClaude, ModelName: "claude-3-5", APIKey: "fund"},
		}
	})
	router.SetUserOverrideHook(func(_ context.Context, _ *ChatRequest) *UserOverrideDecision {
		return &UserOverrideDecision{
			Config: &ModelConfig{Provider: ProviderOpenAI, ModelName: "gpt-4o", APIKey: "user"},
		}
	})

	cfg, err := router.ResolveModel(context.Background(), &ChatRequest{
		UserID: "u1", StepName: "macro_brief",
	})
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if cfg.APIKey != "user" {
		t.Errorf("user BYOK should beat fund override, got %q", cfg.APIKey)
	}
}

// TestUserOverrideHook_NilDisables documents that passing nil
// disables the hook — used at boot before userbyok.Repo is ready.
func TestUserOverrideHook_NilDisables(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{ProviderDeepSeek: "sys-deepseek"},
		DefaultModels, nil, nil,
	)
	calls := atomic.Int64{}
	router.SetUserOverrideHook(func(_ context.Context, _ *ChatRequest) *UserOverrideDecision {
		calls.Add(1)
		return &UserOverrideDecision{Config: &ModelConfig{Provider: ProviderClaude, ModelName: "claude"}}
	})
	router.SetUserOverrideHook(nil)

	_, err := router.ResolveModel(context.Background(), &ChatRequest{
		UserID: "u1", StepName: "macro_brief",
	})
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if calls.Load() != 0 {
		t.Errorf("hook should be disabled, ran %d times", calls.Load())
	}
}

// TestUserOverrideHook_RaceWithResolve — regression guard for the
// hook setter under concurrent resolves.
func TestUserOverrideHook_RaceWithResolve(t *testing.T) {
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
			req := &ChatRequest{UserID: "u1", StepName: "macro_brief"}
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
					router.SetUserOverrideHook(nil)
				} else {
					router.SetUserOverrideHook(func(_ context.Context, _ *ChatRequest) *UserOverrideDecision {
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
