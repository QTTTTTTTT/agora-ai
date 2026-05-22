package decision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fundai/server/internal/llm"
)

// LLMDecisionEngine is the production DecisionEngine. It marshals the
// DecisionInput into a JSON payload, sends it to the configured LLM
// with a strict system prompt, and parses the model's JSON response.
//
// The engine is deliberately small: it owns the prompt + parsing
// contract and nothing else. Anything that requires DB / repository /
// market-data access happens in the wiring layer before/after the
// call.
type LLMDecisionEngine struct {
	// Client is the abstraction over the LLM router. Tests inject a
	// fake LLMClient that records the request and returns a canned
	// JSON response.
	Client llm.LLMClient

	// ModelTier picks the bucket the LLM router uses; the
	// MultiProviderClient already knows how to map "smart" → claude-
	// opus, "fast" → claude-haiku, etc. Decisions of this size
	// benefit from a smart-tier model.
	ModelTier llm.ModelTier

	// Identification for accounting + telemetry. These flow through
	// the LLM router and end up on usage records.
	UserID   string
	AgentID  string
	StepName string
	FundID   string

	// Temperature stays low: this is a structured task and
	// non-determinism just creates parser failures. The default 0.1
	// works well across Anthropic / OpenAI / DeepSeek.
	Temperature float64
	// MaxTokens caps the reply size. Plans rarely exceed 5 actions →
	// ~1500 tokens of JSON is plenty.
	MaxTokens int
}

// ErrEmptyDecision is returned when the model produced a structurally
// valid response but with zero actions AND zero stance — the caller
// (PMAgent) should fall back to the deterministic engine in that case.
var ErrEmptyDecision = errors.New("decision engine returned empty output")

// Decide implements DecisionEngine. Errors are propagated unchanged
// so the wiring layer can decide whether to fall back. The engine
// never panics on malformed JSON — instead it returns a wrapped
// error and lets the caller substitute the deterministic engine.
func (e *LLMDecisionEngine) Decide(ctx context.Context, input DecisionInput) (*DecisionOutput, error) {
	if e == nil || e.Client == nil {
		return nil, errors.New("llm decision engine: client not configured")
	}

	req := llm.ChatRequest{
		Messages: []llm.ChatMessage{
			{Role: "system", Content: systemPrompt()},
			{Role: "user", Content: userPrompt(input)},
		},
		ModelTier:   e.ModelTier,
		MaxTokens:   e.maxTokens(),
		Temperature: e.temperature(),
		UserID:      e.UserID,
		AgentID:     e.AgentID,
		StepName:    e.StepName,
		FundID:      e.FundID,
	}

	resp, err := e.Client.Chat(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("llm decision engine: chat: %w", err)
	}
	if resp == nil || strings.TrimSpace(resp.Content) == "" {
		return nil, errors.New("llm decision engine: empty response")
	}

	parsed, err := parseDecisionOutput(resp.Content)
	if err != nil {
		return nil, fmt.Errorf("llm decision engine: parse: %w", err)
	}
	if len(parsed.Actions) == 0 && strings.TrimSpace(parsed.Stance) == "" {
		return parsed, ErrEmptyDecision
	}
	return parsed, nil
}

func (e *LLMDecisionEngine) maxTokens() int {
	if e.MaxTokens > 0 {
		return e.MaxTokens
	}
	return 1500
}

func (e *LLMDecisionEngine) temperature() float64 {
	if e.Temperature > 0 {
		return e.Temperature
	}
	return 0.1
}

// parseDecisionOutput unmarshals the model's response. It is forgiving
// in two well-defined ways:
//
//  1. Strips markdown code fences ```json ... ``` if the model emits
//     them anyway despite the system prompt saying not to. Some open-
//     weight models add fences habitually.
//  2. If the response contains text before/after a JSON object, it
//     extracts the first {...} block. This handles models that prefix
//     with a sentence like "Here is the plan: { ... }".
//
// Anything beyond those two recoveries (truncated JSON, wrong
// schema) returns an error and the caller falls back.
func parseDecisionOutput(raw string) (*DecisionOutput, error) {
	cleaned := stripJSONNoise(raw)

	var dto struct {
		Stance     string `json:"stance"`
		Confidence *float64 `json:"confidence"`
		Actions    []struct {
			Symbol     string  `json:"symbol"`
			Action     string  `json:"action"`
			QtyPct     float64 `json:"qtyPct"`
			Reasoning  string  `json:"reasoning"`
			Confidence float64 `json:"confidence"`
		} `json:"actions"`
	}
	if err := json.Unmarshal([]byte(cleaned), &dto); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}

	output := &DecisionOutput{
		Stance: strings.TrimSpace(dto.Stance),
	}
	if dto.Confidence != nil {
		output.Confidence = clampUnit(*dto.Confidence)
	}
	for _, a := range dto.Actions {
		action := strings.ToLower(strings.TrimSpace(a.Action))
		switch action {
		case "buy", "sell", "hold", "reduce", "add", "watch":
			// ok
		default:
			// unknown action — skip rather than fail the whole plan.
			continue
		}
		output.Actions = append(output.Actions, DecisionAction{
			Symbol:     strings.TrimSpace(a.Symbol),
			Action:     action,
			QtyPct:     clampUnit(a.QtyPct),
			Reasoning:  strings.TrimSpace(a.Reasoning),
			Confidence: clampUnit(a.Confidence),
		})
	}
	return output, nil
}

func stripJSONNoise(s string) string {
	s = strings.TrimSpace(s)
	// strip ```json ... ``` or ``` ... ``` fences
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	// extract the first {...} block to tolerate leading/trailing prose
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
