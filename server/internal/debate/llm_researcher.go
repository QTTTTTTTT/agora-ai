package debate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fundai/server/internal/llm"
)

// LLMResearcher is the production implementation of Researcher used
// for all three personas (bull / bear / quant). The role determines
// the system prompt and the persona-specific framing instructions —
// everything else is shared, including parsing, retry-on-noise, and
// the convergence-friendly output schema.
//
// A single struct with a Role field beats three copy-pasted types:
// the only thing that varies between bull and bear is the system
// prompt nudge ("look for upside catalysts" vs "look for risks").
// Tests can swap LLMResearcher.Client for a fake without touching
// any persona-specific code.
type LLMResearcher struct {
	// PersonaRole pins this researcher's bias for the orchestrator.
	PersonaRole AgentRole
	// Client is the underlying LLM client. Nil produces a graceful
	// "neutral abstain" view so an unconfigured deployment still runs
	// the debate (the orchestrator will fall back to a neutral
	// stance instead of crashing).
	Client llm.LLMClient
	// ModelTier picks the LLM router bucket. Defaults to "standard"
	// when zero — debate consumes a lot of tokens, smart-tier rate
	// limits would be expensive to fan-out across three agents per
	// round.
	ModelTier llm.ModelTier
	// AgentID / UserID flow into the LLM router's usage records so
	// debate calls are billed and audited per fund.
	AgentID  string
	UserID   string
	FundID   string
	StepName string

	// Temperature lets each role be slightly different — bull and
	// bear get 0.3 (creative arguing), quant gets 0.1 (more
	// deterministic technical reads). Wired automatically below;
	// callers normally leave it zero.
	Temperature float64
	// MaxTokens caps the reply per round. Each round emits a small
	// JSON object; 1200 tokens is plenty.
	MaxTokens int
}

func (r *LLMResearcher) Role() AgentRole {
	if r == nil {
		return RoleQuant
	}
	return r.PersonaRole
}

// Debate runs one round for the configured role. PeerViews are the
// other agents' previous-round outputs so the LLM can write a
// rebuttal-aware view. Returning a neutral AgentView (rather than
// an error) when the client is unwired keeps the orchestrator on a
// happy path during local development / tests without an LLM key.
func (r *LLMResearcher) Debate(ctx context.Context, input DebateInput, round int, peers []AgentView) (*AgentView, error) {
	if r == nil {
		return nil, errors.New("debate: nil researcher")
	}
	if r.Client == nil {
		return r.neutralView(round, input.Universe, "llm client not configured"), nil
	}

	req := llm.ChatRequest{
		Messages: []llm.ChatMessage{
			{Role: "system", Content: r.systemPrompt()},
			{Role: "user", Content: r.userPrompt(input, round, peers)},
		},
		ModelTier:   r.modelTier(),
		MaxTokens:   r.maxTokens(),
		Temperature: r.temperature(),
		UserID:      r.UserID,
		AgentID:     r.AgentID,
		StepName:    r.stepName(round),
		FundID:      r.FundID,
	}
	resp, err := r.Client.Chat(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("debate llm %s round %d: %w", r.PersonaRole, round, err)
	}
	if resp == nil || strings.TrimSpace(resp.Content) == "" {
		return nil, fmt.Errorf("debate llm %s round %d: empty response", r.PersonaRole, round)
	}

	view, err := r.parse(resp.Content, round)
	if err != nil {
		return nil, fmt.Errorf("debate llm %s round %d: parse: %w", r.PersonaRole, round, err)
	}
	return view, nil
}

func (r *LLMResearcher) modelTier() llm.ModelTier {
	if r.ModelTier != "" {
		return r.ModelTier
	}
	return llm.TierStandard
}

func (r *LLMResearcher) maxTokens() int {
	if r.MaxTokens > 0 {
		return r.MaxTokens
	}
	// Reasoning-tier models (Gemini 3.x Pro Preview, Claude
	// extended-thinking) consume a hidden "thoughts" budget from
	// MaxOutputTokens *before* writing the bull/bear/quant case JSON.
	// Observed thoughtsTokenCount on production debate rounds:
	// 800–2500 with the full prompt (universe + macro + prior
	// roundtable). At 1200 every round in the 2026-05-22 storage-
	// fund run returned "unexpected end of JSON input" because the
	// thoughts pass alone exhausted the budget. 6000 leaves a
	// comfortable cushion for both reasoning *and* the multi-line
	// JSON the parser expects.
	return 6000
}

func (r *LLMResearcher) temperature() float64 {
	if r.Temperature > 0 {
		return r.Temperature
	}
	switch r.PersonaRole {
	case RoleBull, RoleBear:
		return 0.3
	default:
		return 0.1
	}
}

func (r *LLMResearcher) stepName(round int) string {
	base := r.StepName
	if strings.TrimSpace(base) == "" {
		base = "debate"
	}
	return fmt.Sprintf("%s.%s.r%d", base, r.PersonaRole, round)
}

// neutralView is the well-defined "I have nothing to add" view the
// orchestrator emits when the LLM client is missing or fails. Keeps
// the debate moving along on a degraded path instead of producing a
// half-built RoundtableOutput.
func (r *LLMResearcher) neutralView(round int, universe []string, reason string) *AgentView {
	verdicts := make([]SymbolVerdict, 0, len(universe))
	for _, sym := range universe {
		verdicts = append(verdicts, SymbolVerdict{
			Symbol:     sym,
			Direction:  "neutral",
			Confidence: 0.5,
			KeyPoints:  []string{"abstain: " + reason},
		})
	}
	return &AgentView{
		Role:       r.PersonaRole,
		Round:      round,
		Stance:     "abstain: " + reason,
		Verdicts:   verdicts,
		Confidence: 0.5,
	}
}

// parse handles the model's JSON reply. It is forgiving in the same
// two well-defined ways as decision.parseDecisionOutput:
//
//  1. Strips markdown fences ```json ... ``` the model sometimes
//     emits despite the system prompt.
//  2. Extracts the first {...} block to tolerate leading prose.
//
// Unknown directions are coerced to "neutral" rather than failing
// the whole round — one weird symbol shouldn't poison the debate.
func (r *LLMResearcher) parse(raw string, round int) (*AgentView, error) {
	cleaned := stripJSONNoise(raw)
	var dto struct {
		Stance     string  `json:"stance"`
		Confidence float64 `json:"confidence"`
		Verdicts   []struct {
			Symbol     string   `json:"symbol"`
			Direction  string   `json:"direction"`
			Confidence float64  `json:"confidence"`
			KeyPoints  []string `json:"keyPoints"`
		} `json:"verdicts"`
	}
	if err := json.Unmarshal([]byte(cleaned), &dto); err != nil {
		return nil, err
	}
	view := &AgentView{
		Role:       r.PersonaRole,
		Round:      round,
		Stance:     strings.TrimSpace(dto.Stance),
		Confidence: clampUnit(dto.Confidence),
	}
	for _, v := range dto.Verdicts {
		direction := strings.ToLower(strings.TrimSpace(v.Direction))
		switch direction {
		case "bull", "bear", "neutral":
			// ok
		default:
			direction = "neutral"
		}
		view.Verdicts = append(view.Verdicts, SymbolVerdict{
			Symbol:     strings.TrimSpace(v.Symbol),
			Direction:  direction,
			Confidence: clampUnit(v.Confidence),
			KeyPoints:  v.KeyPoints,
		})
	}
	return view, nil
}

func stripJSONNoise(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}

func clampUnit(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
