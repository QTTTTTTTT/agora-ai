// llm_adapter.go — S8.3 bridge between the production
// llm.LLMClient (Chat-based, multi-provider, with budget /
// quota / failover) and the agent-side LLMClient +
// SchemaLLMClient interfaces (Complete-based, prompt-only).
//
// Why this lives in the agent package: it converts
// llm.LLMClient → agent.LLMClient. Putting it in internal/llm
// would force that package to import internal/agent (cycle).
// Putting it in cmd/server/wiring_*.go is acceptable but
// inconvenient because the panel / debate constructors need a
// concrete type that satisfies the interface — and tests for
// analyst.go + bullbear.go want to mock the same interface
// without depending on the wiring layer.

package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fundai/server/internal/llm"
)

// LLMAdapter wraps an llm.LLMClient and exposes the agent-side
// LLMClient + SchemaLLMClient surface. Each Complete /
// CompleteWithSchema call assembles a one-shot two-message
// ChatRequest (system + user). The adapter is otherwise
// stateless and safe for concurrent use.
//
// The adapter carries identifying fields (UserID / FundID /
// AgentID / StepName) so the underlying client can route the
// call through the right per-fund budget, quota, audit, and
// failover paths.
type LLMAdapter struct {
	client    llm.LLMClient
	userID    string
	fundID    string
	agentID   string
	stepName  string
	modelTier llm.ModelTier
	model     string
	maxTokens int
	temperature float64
}

// LLMAdapterOption configures an LLMAdapter.
type LLMAdapterOption func(*LLMAdapter)

// WithLLMAdapterUser tags the underlying ChatRequest with the
// user paying for it.
func WithLLMAdapterUser(userID string) LLMAdapterOption {
	return func(a *LLMAdapter) { a.userID = userID }
}

// WithLLMAdapterAgent tags the underlying ChatRequest with the
// calling agent so usage shows up in audit logs.
func WithLLMAdapterAgent(agentID string) LLMAdapterOption {
	return func(a *LLMAdapter) { a.agentID = agentID }
}

// WithLLMAdapterStep tags the ChatRequest with the workflow
// step name (e.g. "analyst:fundamentals", "debate:bull").
func WithLLMAdapterStep(name string) LLMAdapterOption {
	return func(a *LLMAdapter) { a.stepName = name }
}

// WithLLMAdapterTier selects a model tier (cheap/standard/expert).
func WithLLMAdapterTier(tier llm.ModelTier) LLMAdapterOption {
	return func(a *LLMAdapter) { a.modelTier = tier }
}

// WithLLMAdapterModel overrides the model with a specific name
// (takes precedence over the tier).
func WithLLMAdapterModel(model string) LLMAdapterOption {
	return func(a *LLMAdapter) { a.model = model }
}

// WithLLMAdapterMaxTokens overrides the per-call output cap.
func WithLLMAdapterMaxTokens(n int) LLMAdapterOption {
	return func(a *LLMAdapter) { a.maxTokens = n }
}

// WithLLMAdapterTemperature overrides sampling temperature.
func WithLLMAdapterTemperature(t float64) LLMAdapterOption {
	return func(a *LLMAdapter) { a.temperature = t }
}

// NewLLMAdapter builds an LLMAdapter. fundID is required so the
// per-fund token budget can be enforced. Pass options to set
// userID, agentID, step name, model tier, etc.
func NewLLMAdapter(client llm.LLMClient, fundID string, opts ...LLMAdapterOption) *LLMAdapter {
	a := &LLMAdapter{
		client:    client,
		fundID:    fundID,
		modelTier: llm.TierStandard,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Complete implements agent.LLMClient. The provider is told to
// return freeform text (no response_format) — callers parse
// whatever shape comes back.
func (a *LLMAdapter) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	if a == nil || a.client == nil {
		return "", fmt.Errorf("agent.LLMAdapter: nil client")
	}
	resp, err := a.chat(ctx, systemPrompt, userPrompt, "", nil)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// CompleteWithSchema implements agent.SchemaLLMClient. When
// schema is non-empty the request goes out with
// ResponseFormat=json_schema; otherwise it's json_object
// (any JSON shape).
func (a *LLMAdapter) CompleteWithSchema(ctx context.Context, systemPrompt, userPrompt string, schema []byte) (string, error) {
	if a == nil || a.client == nil {
		return "", fmt.Errorf("agent.LLMAdapter: nil client")
	}
	format := "json_object"
	if len(schema) > 0 {
		format = "json_schema"
	}
	resp, err := a.chat(ctx, systemPrompt, userPrompt, format, schema)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// chat assembles + dispatches a ChatRequest. Empty system or
// user prompts are tolerated — empty system means the model
// receives only the user prompt; empty user is rejected
// because the providers we wrap all require at least one
// non-system message.
func (a *LLMAdapter) chat(ctx context.Context, systemPrompt, userPrompt, format string, schema []byte) (*llm.ChatResponse, error) {
	if strings.TrimSpace(userPrompt) == "" {
		return nil, fmt.Errorf("agent.LLMAdapter: user prompt required")
	}

	messages := make([]llm.ChatMessage, 0, 2)
	if strings.TrimSpace(systemPrompt) != "" {
		messages = append(messages, llm.ChatMessage{Role: "system", Content: systemPrompt})
	}
	messages = append(messages, llm.ChatMessage{Role: "user", Content: userPrompt})

	// Honour ctx deadline so a downstream caller can bound the
	// LLM step independently of the provider's own timeout.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
	}

	req := llm.ChatRequest{
		Messages:       messages,
		ModelTier:      a.modelTier,
		Model:          a.model,
		MaxTokens:      a.maxTokens,
		Temperature:    a.temperature,
		UserID:         a.userID,
		AgentID:        a.agentID,
		FundID:         a.fundID,
		StepName:       a.stepName,
		ResponseFormat: format,
		ResponseSchema: schema,
	}
	return a.client.Chat(ctx, req)
}

// AnalystReportJSONSchema is the JSON Schema the analyst LLM
// prompt enforces when going through CompleteWithSchema. It
// matches parseLLMJSONReport — direction is bullish/bearish/neutral,
// confidence is 0-100, and key_findings / risks / data_points are
// arrays (key_findings + risks required, data_points optional).
//
// The schema is intentionally permissive on nested arrays so a
// provider that returns extra fields (e.g. "summary") doesn't
// fail strict mode. Strict mode requires *no additional
// properties at the named keys we care about*; extras land in
// the parser as ignored fields.
var AnalystReportJSONSchema = []byte(`{
  "type": "object",
  "properties": {
    "direction":   { "type": "string", "enum": ["bullish","bearish","neutral"] },
    "confidence":  { "type": "integer", "minimum": 0, "maximum": 100 },
    "thesis":      { "type": "string" },
    "key_findings":{ "type": "array", "items": { "type": "string" } },
    "risks":       { "type": "array", "items": { "type": "string" } },
    "data_points": { "type": "array", "items": { "type": "object" } }
  },
  "required": ["direction","confidence","thesis","key_findings","risks"],
  "additionalProperties": true
}`)

// AdvocateArgumentJSONSchema is the JSON Schema the Bull /
// Bear LLM prompt enforces. Mirrors parseAdvocateJSON: direction
// is bullish/bearish only (no neutral), key_findings == support
// points, risks == rebuttals.
var AdvocateArgumentJSONSchema = []byte(`{
  "type": "object",
  "properties": {
    "direction":     { "type": "string", "enum": ["bullish","bearish"] },
    "confidence":    { "type": "integer", "minimum": 30, "maximum": 95 },
    "thesis":        { "type": "string" },
    "key_findings":  { "type": "array", "items": { "type": "string" } },
    "risks":         { "type": "array", "items": { "type": "string" } },
    "support_points":{ "type": "array", "items": { "type": "string" } },
    "rebuttals":     { "type": "array", "items": { "type": "string" } }
  },
  "required": ["direction","confidence","thesis"],
  "additionalProperties": true
}`)
