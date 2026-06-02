package main

import (
	"testing"

	"github.com/fundai/server/internal/decision"
	"github.com/fundai/server/internal/llm"
)

// makeStubRuntime constructs a minimal llmRuntime whose Chat path
// is never exercised by these tests — we only assert which engine
// type buildLLMDecisionEngine returns under different env flags.
// A zero-value MultiProviderClient satisfies the runtime's
// LLMClient() non-nil check without needing real provider keys.
func makeStubRuntime(t *testing.T) *llmRuntime {
	t.Helper()
	return &llmRuntime{client: &llm.MultiProviderClient{}}
}

func TestBuildLLMDecisionEngine_FlagOff_ReturnsSingleStage(t *testing.T) {
	t.Setenv("PM_THREE_STAGE_DECISION", "")
	engine := buildLLMDecisionEngine(makeStubRuntime(t), "fund1")
	if _, ok := engine.(*decision.LLMDecisionEngine); !ok {
		t.Fatalf("expected *LLMDecisionEngine, got %T", engine)
	}
}

func TestBuildLLMDecisionEngine_FlagOn_ReturnsThreeStage(t *testing.T) {
	t.Setenv("PM_THREE_STAGE_DECISION", "1")
	engine := buildLLMDecisionEngine(makeStubRuntime(t), "fund1")
	wrapper, ok := engine.(*decision.ThreeStageEngine)
	if !ok {
		t.Fatalf("expected *ThreeStageEngine, got %T", engine)
	}
	if wrapper.Inner == nil {
		t.Fatalf("inner engine not wired")
	}
	if wrapper.Client == nil {
		t.Fatalf("client not wired")
	}
	if wrapper.FundID != "fund1" {
		t.Fatalf("fund id not propagated, got %q", wrapper.FundID)
	}
	if wrapper.StageTimeout == 0 {
		t.Fatalf("stage timeout should be set")
	}
}

func TestBuildLLMDecisionEngine_NilRuntime_ReturnsNil(t *testing.T) {
	if got := buildLLMDecisionEngine(nil, "fund1"); got != nil {
		t.Fatalf("expected nil with nil runtime, got %v", got)
	}
}

func TestLlmTierFromEnv_DefaultsAndKnownTiers(t *testing.T) {
	cases := []struct {
		v        string
		fallback llm.ModelTier
		want     llm.ModelTier
	}{
		{"", llm.TierStandard, llm.TierStandard},
		{"  ", llm.TierStandard, llm.TierStandard},
		{"standard", llm.TierCritical, llm.TierStandard},
		{"STANDARD", llm.TierCritical, llm.TierStandard},
		{"critical", llm.TierStandard, llm.TierCritical},
		{"simple", llm.TierCritical, llm.TierSimple},
		{"fast", llm.TierCritical, llm.TierSimple},
		{"unknown", llm.TierStandard, llm.TierStandard},
	}
	for _, c := range cases {
		t.Setenv("__FUNDAI_TEST_TIER__", c.v)
		got := llmTierFromEnv("__FUNDAI_TEST_TIER__", c.fallback)
		if got != c.want {
			t.Errorf("llmTierFromEnv(%q, %v) = %v want %v", c.v, c.fallback, got, c.want)
		}
	}
}
