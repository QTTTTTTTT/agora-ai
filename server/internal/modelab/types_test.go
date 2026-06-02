package modelab

import (
	"strings"
	"testing"

	"github.com/fundai/server/internal/llm"
)

func TestExperimentValidate_HappyPath(t *testing.T) {
	e := &Experiment{
		Name:  "claude vs gpt on PM",
		Scope: ScopeAgentRole,
		ScopeTarget: "pm",
		Arms: []ArmConfig{
			{Name: "ctrl", Provider: llm.ProviderOpenAI, ModelName: "gpt-4o", ModelTier: llm.TierCritical},
			{Name: "treat", Provider: llm.ProviderClaude, ModelName: "claude-opus", ModelTier: llm.TierCritical},
		},
		TrafficSplit: []float64{0.5, 0.5},
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

func TestExperimentValidate_RejectsBadSum(t *testing.T) {
	e := &Experiment{
		Name:  "bad sum",
		Scope: ScopeGlobal,
		Arms: []ArmConfig{
			{Provider: llm.ProviderOpenAI, ModelName: "x"},
			{Provider: llm.ProviderClaude, ModelName: "y"},
		},
		TrafficSplit: []float64{0.3, 0.4},
	}
	err := e.Validate()
	if err == nil || !strings.Contains(err.Error(), "sum to ~1.0") {
		t.Fatalf("expected sum error, got %v", err)
	}
}

func TestExperimentValidate_RejectsSingleArm(t *testing.T) {
	e := &Experiment{
		Name:  "lonely",
		Scope: ScopeGlobal,
		Arms: []ArmConfig{
			{Provider: llm.ProviderOpenAI, ModelName: "x"},
		},
		TrafficSplit: []float64{1.0},
	}
	if err := e.Validate(); err == nil {
		t.Fatalf("expected at-least-2-arms error")
	}
}

func TestExperimentValidate_RequiresScopeTarget(t *testing.T) {
	e := &Experiment{
		Name:  "missing tgt",
		Scope: ScopeFund,
		Arms: []ArmConfig{
			{Provider: llm.ProviderOpenAI, ModelName: "x"},
			{Provider: llm.ProviderClaude, ModelName: "y"},
		},
		TrafficSplit: []float64{0.5, 0.5},
	}
	err := e.Validate()
	if err == nil || !strings.Contains(err.Error(), "scope_target") {
		t.Fatalf("expected scope_target error, got %v", err)
	}
}

func TestExperimentValidate_RejectsBadArm(t *testing.T) {
	e := &Experiment{
		Name:  "bad arm",
		Scope: ScopeGlobal,
		Arms: []ArmConfig{
			{Provider: "", ModelName: "x"},
			{Provider: llm.ProviderClaude, ModelName: "y"},
		},
		TrafficSplit: []float64{0.5, 0.5},
	}
	err := e.Validate()
	if err == nil || !strings.Contains(err.Error(), "Provider") {
		t.Fatalf("expected provider error, got %v", err)
	}
}

func TestExperimentMatch(t *testing.T) {
	e := &Experiment{Status: StatusRunning, Scope: ScopeFund, ScopeTarget: "fund-1"}
	if !e.Match("fund-1", "ag", "pm", "pm_decision") {
		t.Fatalf("expected fund match")
	}
	if e.Match("fund-2", "ag", "pm", "pm_decision") {
		t.Fatalf("should not match different fund")
	}

	roleExp := &Experiment{Status: StatusRunning, Scope: ScopeAgentRole, ScopeTarget: "pm"}
	if !roleExp.Match("fund-1", "ag-x", "PM", "pm_decision") {
		t.Fatalf("role match should be case-insensitive")
	}

	stepExp := &Experiment{Status: StatusRunning, Scope: ScopeGlobal, StepFilter: []string{"pm_decision"}}
	if !stepExp.Match("fund-1", "ag", "pm", "pm_decision") {
		t.Fatalf("step filter hit expected")
	}
	if stepExp.Match("fund-1", "ag", "pm", "debate") {
		t.Fatalf("step filter should drop unrelated steps")
	}

	if (*Experiment)(nil).Match("fund-1", "ag", "pm", "pm_decision") {
		t.Fatalf("nil receiver should not match")
	}

	draft := &Experiment{Status: StatusDraft, Scope: ScopeGlobal}
	if draft.Match("fund-1", "ag", "pm", "pm_decision") {
		t.Fatalf("draft should not match")
	}
}

func TestArmConfigLabel(t *testing.T) {
	a := ArmConfig{Provider: llm.ProviderOpenAI, ModelName: "gpt-4o"}
	if got := a.Label(); got != "openai/gpt-4o" {
		t.Fatalf("label = %q", got)
	}
}

func TestBudgetExhausted(t *testing.T) {
	e := &Experiment{MaxTotalTokens: 100, TokensUsed: 50}
	if e.BudgetExhausted() {
		t.Fatalf("not exhausted yet")
	}
	e.TokensUsed = 100
	if !e.BudgetExhausted() {
		t.Fatalf("should be exhausted at cap")
	}
	uncapped := &Experiment{MaxTotalTokens: 0, TokensUsed: 1e9}
	if uncapped.BudgetExhausted() {
		t.Fatalf("zero cap means no cap")
	}
}

func TestSanitizeAgentRole(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"PM", "pm"},
		{"  Pm  ", "pm"},
		{"portfolio_manager", "pm"},
		{"PortfolioManager", "pm"},
		{"risk_officer", "risk"},
		{"unknown", "unknown"},
		{"", ""},
	}
	for _, c := range cases {
		if got := SanitizeAgentRole(c.in); got != c.want {
			t.Errorf("SanitizeAgentRole(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
