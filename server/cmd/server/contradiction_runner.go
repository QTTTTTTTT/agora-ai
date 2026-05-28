package main

import (
	"context"
	"strings"
	"time"

	"github.com/fundai/server/internal/contradiction"
	"github.com/fundai/server/internal/llm"
)

// contradictionRunner adapts contradiction.Checker to the project's
// llmRuntime. Putting it next to the runtime keeps the contradiction
// pkg clean of project-specific routing concerns (agentID / userID
// / tier names) and lets us treat the checker as cheap, optional
// LLM call that gets disabled silently when llm is unwired.
type contradictionRunner struct {
	runtime  *llmRuntime
	disabled bool

	// maxNotes caps how many notes the checker can return.
	// Default 3 — more than that bloats RiskNotes.
	maxNotes int
}

func newContradictionRunner(rt *llmRuntime) *contradictionRunner {
	return &contradictionRunner{
		runtime:  rt,
		maxNotes: 3,
	}
}

// Disabled lets ops kill the checker via env without removing wiring.
func (r *contradictionRunner) Disable() {
	if r == nil {
		return
	}
	r.disabled = true
}

// Check runs the contradiction.Checker. Returns the formatted risk
// notes ready to append to DecisionInput.RiskNotes. Soft-fails on
// every error path so the PM call is never blocked.
func (r *contradictionRunner) Check(ctx context.Context, fundID string, tradingDate time.Time, universe []string, macro, plan string, researchers []contradiction.ResearcherView, userID, agentID string) []string {
	if r == nil || r.disabled || r.runtime == nil {
		return nil
	}
	if len(researchers) < 2 {
		return nil
	}
	adapter := &runtimeChatJSONAdapter{
		runtime: r.runtime,
		userID:  userID,
		agentID: agentID,
	}
	checker := contradiction.New(adapter)
	checker.MaxNotes = r.maxNotes
	checker.ModelTier = "simple"
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	notes, _ := checker.Check(ctx, contradiction.Input{
		FundID:       fundID,
		TradingDate:  tradingDate,
		Universe:     universe,
		Researchers:  researchers,
		MacroSummary: macro,
		PlanSummary:  plan,
	})
	return contradiction.FormatRiskNotes(notes)
}

// runtimeChatJSONAdapter is the bridge between contradiction.LLMClient
// (small interface) and llmRuntime.Chat (full request shape).
type runtimeChatJSONAdapter struct {
	runtime *llmRuntime
	userID  string
	agentID string
}

func (a *runtimeChatJSONAdapter) ChatJSON(ctx context.Context, req contradiction.ChatRequest) (string, error) {
	if a == nil || a.runtime == nil {
		return "", nil
	}
	tier := strings.ToLower(strings.TrimSpace(req.ModelTier))
	var modelTier llm.ModelTier
	switch tier {
	case "critical":
		modelTier = llm.TierCritical
	case "standard":
		modelTier = llm.TierStandard
	default:
		modelTier = llm.TierSimple
	}
	chatReq := llm.ChatRequest{
		Messages: []llm.ChatMessage{
			{Role: "system", Content: req.System},
			{Role: "user", Content: req.User},
		},
		ModelTier: modelTier,
		MaxTokens: req.MaxTokens,
		UserID:    a.userID,
		AgentID:   a.agentID,
		FundID:    req.FundID,
		StepName:  req.StepName,
	}
	resp, err := a.runtime.Chat(ctx, chatReq)
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", nil
	}
	return resp.Content, nil
}
