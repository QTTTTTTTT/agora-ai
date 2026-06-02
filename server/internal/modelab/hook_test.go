package modelab

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/llm"
)

func TestBuildLLMConfig_FillsDefaultsFromTier(t *testing.T) {
	hc := HookContext{
		TierDefaults: map[llm.ModelTier]*llm.ModelConfig{
			llm.TierCritical: {
				Provider:    llm.ProviderOpenAI,
				ModelName:   "gpt-4o",
				BaseURL:     "https://api.openai.com/v1",
				MaxTokens:   4096,
				Temperature: 0.7,
			},
		},
		SystemAPIKeys: map[llm.Provider]string{
			llm.ProviderClaude: "sk-claude-system",
		},
	}
	arm := ArmConfig{
		Provider:  llm.ProviderClaude,
		ModelName: "claude-opus",
		ModelTier: llm.TierCritical,
	}
	cfg := BuildLLMConfig(arm, hc)
	if cfg.Provider != llm.ProviderClaude {
		t.Fatalf("provider not honoured: %v", cfg.Provider)
	}
	if cfg.ModelName != "claude-opus" {
		t.Fatalf("model not honoured: %v", cfg.ModelName)
	}
	if cfg.BaseURL != "https://api.anthropic.com/v1" {
		t.Fatalf("BaseURL should resolve to claude default, got %q", cfg.BaseURL)
	}
	if cfg.MaxTokens != 4096 {
		t.Fatalf("MaxTokens should inherit from tier default, got %d", cfg.MaxTokens)
	}
	if cfg.APIKey != "sk-claude-system" {
		t.Fatalf("system key not injected, got %q", cfg.APIKey)
	}
	if cfg.ResolvedTier != llm.TierCritical {
		t.Fatalf("tier not set, got %v", cfg.ResolvedTier)
	}
	if cfg.UsesCustomKey {
		t.Fatalf("UsesCustomKey must be false for system-key experiments")
	}
}

func TestBuildLLMConfig_RespectsArmOverrides(t *testing.T) {
	arm := ArmConfig{
		Provider:    llm.ProviderClaude,
		ModelName:   "claude-opus",
		BaseURL:     "https://internal.proxy.example.com",
		ModelTier:   llm.TierCritical,
		MaxTokens:   8192,
		Temperature: 0.2,
	}
	cfg := BuildLLMConfig(arm, HookContext{})
	if cfg.BaseURL != "https://internal.proxy.example.com" {
		t.Fatalf("BaseURL override lost: %q", cfg.BaseURL)
	}
	if cfg.MaxTokens != 8192 {
		t.Fatalf("MaxTokens override lost: %d", cfg.MaxTokens)
	}
	if cfg.Temperature != 0.2 {
		t.Fatalf("Temperature override lost: %v", cfg.Temperature)
	}
}

func TestBuildLLMConfig_InvalidTierFallsBackToStandard(t *testing.T) {
	arm := ArmConfig{Provider: llm.ProviderClaude, ModelName: "claude-opus"}
	cfg := BuildLLMConfig(arm, HookContext{})
	if cfg.ResolvedTier != llm.TierStandard {
		t.Fatalf("expected fallback to TierStandard, got %v", cfg.ResolvedTier)
	}
}

func TestResolverAsLLMHook_NilResolverReturnsNilHook(t *testing.T) {
	var r *Resolver
	hook := r.AsLLMHook(HookContext{})
	if hook != nil {
		t.Fatalf("nil resolver should produce nil hook")
	}
}

func TestResolverAsLLMHook_NoMatchReturnsNilDecision(t *testing.T) {
	db, mock := openMock(t)
	defer db.Close()
	mock.ExpectQuery(`FROM model_ab_experiments\s+WHERE status = ANY`).
		WillReturnRows(experimentRows())

	r := NewResolver(NewRepo(db))
	r.Logger = discardLogger()
	hook := r.AsLLMHook(HookContext{})
	d := hook(context.Background(), &llm.ChatRequest{FundID: "f1", AgentID: "a1", AgentRole: "pm", StepName: "pm_decision", RunID: "r1"})
	if d != nil {
		t.Fatalf("expected nil decision for no-match, got %+v", d)
	}
}

func TestResolverAsLLMHook_MatchReturnsDecision(t *testing.T) {
	db, mock := openMock(t)
	defer db.Close()
	armsJSON, _ := MarshalArms(stubArms())

	rows := experimentRows().AddRow(
		"00000000-0000-0000-0000-000000000001",
		"hookable", "",
		string(ScopeAgentRole), "pm",
		stringArray(),
		armsJSON,
		float64Array(0.5, 0.5),
		string(StatusRunning),
		time.Time{}, time.Time{},
		int64(0), int64(0), "",
		time.Now(), time.Now(),
	)
	mock.ExpectQuery(`FROM model_ab_experiments\s+WHERE status = ANY`).WillReturnRows(rows)
	mock.ExpectQuery(`INSERT INTO model_ab_assignments`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "arm_index", "arm_name", "assigned_at"}).
			AddRow("00000000-0000-0000-0000-000000000077", 0, "control", time.Now()))

	r := NewResolver(NewRepo(db))
	r.Logger = discardLogger()
	hook := r.AsLLMHook(HookContext{})
	d := hook(context.Background(), &llm.ChatRequest{
		FundID: "fund-X", AgentID: "ag-7", AgentRole: "PM",
		StepName: "pm_decision", RunID: "run-1",
	})
	if d == nil {
		t.Fatalf("expected decision, got nil")
	}
	if d.ExperimentID == "" || d.AssignmentID == "" {
		t.Fatalf("decision missing IDs: %+v", d)
	}
	if d.Config == nil || d.Config.ModelName == "" {
		t.Fatalf("decision missing config: %+v", d)
	}
}

func TestResolverAsLLMHook_NilRequest(t *testing.T) {
	db, _ := openMock(t)
	defer db.Close()
	r := NewResolver(NewRepo(db))
	hook := r.AsLLMHook(HookContext{})
	if got := hook(context.Background(), nil); got != nil {
		t.Fatalf("nil request must produce nil decision, got %+v", got)
	}
}
