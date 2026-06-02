# S8.3 — Native Structured LLM Output

`llm.ChatRequest` gains two opt-in fields — `ResponseFormat`
and `ResponseSchema` — and each of the five providers
(OpenAI / Claude / Gemini / DeepSeek / Qwen + Custom) now
routes those into its native structured-output path. The
agent layer (S8.1 analysts and S8.2 Bull / Bear advocates)
goes through a new `agent.SchemaLLMClient` interface; an
`agent.LLMAdapter` adapter wraps the production `llm.LLMClient`
and exposes both `Complete` (legacy freeform) and
`CompleteWithSchema` (native JSON).

## Motivation

Before S8.3, the agent layer used a `Complete(sys, user)
(string, error)` interface and parsed freeform LLM output with
tolerant fence-stripping in
`parseLLMJSONReport` / `parseAdvocateJSON`. That worked but
had two problems:

1. The model could (and did) wander outside the expected JSON
   shape under load, especially when context was long enough
   that it ran out of attention budget.
2. There was no per-call audit trail telling us "this call
   came back schema-valid" vs "this call came back as
   freeform text that the parser had to recover".

S8.3 fixes both by pushing the structured-output enforcement
*into the provider*. When the request goes out with
`ResponseFormat: "json_schema"` and a non-empty
`ResponseSchema`, the provider is responsible for producing
JSON that matches the schema or rejecting the request — and
the parser then only has to worry about provider quirks, not
the underlying model's freedom of expression.

## Wire mapping per provider

| Provider | Code path | Native knob |
| --- | --- | --- |
| OpenAI (`gpt-4o`, `gpt-4o-mini`, …) | `callOpenAI` | `response_format: {"type":"json_schema","json_schema":{"name":"out","strict":true,"schema":{…}}}` for schema; `{"type":"json_object"}` for object-only |
| DeepSeek (`deepseek-chat`, …) | shared `callOpenAI` (`UsesOpenAIChatCompletions`) | Same OpenAI body — DeepSeek follows the OpenAI ChatCompletions spec |
| Qwen (`qwen-plus`, …) | shared `callOpenAI` | Same OpenAI body |
| Custom (user-supplied OpenAI-compatible endpoint) | shared `callOpenAI` | Same OpenAI body; endpoint is responsible for honouring or ignoring `response_format` |
| Gemini (`gemini-3.1-flash`, `gemini-3.1-pro`, …) | `callGemini` | `generationConfig.responseMimeType: "application/json"` + `generationConfig.responseSchema: {…}` |
| Claude (`claude-opus-4`, `claude-sonnet-4`, …) | `callClaude` | No `response_format` on the Messages API; we append a strict "Return ONLY a JSON object matching this JSON Schema, no prose, no markdown fences:\n{…}" instruction to the system prompt. The tolerant `parseLLMJSONReport` still strips `\`\`\`json` fences if Claude ignores the instruction. |

The opt-in nature means **none of the legacy callers see a
behaviour change**: any `ChatRequest` that doesn't set
`ResponseFormat` produces the same body as before. The new
fields are pure additions that only matter when set.

## Two new JSON schemas

Two canonical JSON Schema documents live in
`internal/agent/llm_adapter.go`:

- `AnalystReportJSONSchema`: enforces
  `{direction, confidence, thesis, key_findings[], risks[]}`
  with `direction ∈ {bullish, bearish, neutral}` and
  `confidence ∈ [0, 100]`. Used by every analyst's
  `callLLMForReport` path.

- `AdvocateArgumentJSONSchema`: enforces
  `{direction, confidence, thesis, key_findings[], risks[],
  support_points[], rebuttals[]}` with
  `direction ∈ {bullish, bearish}` (no neutral — advocates are
  forbidden from settling) and `confidence ∈ [30, 95]` (matches
  `clampAdvocateConfidence`). Used by `callAdvocateLLM`.

Both schemas set `additionalProperties: true` so a provider
that returns extra keys (Gemini sometimes appends a `summary`
that wasn't asked for) still passes strict mode; the tolerant
parser ignores anything outside the named keys.

## Agent surface change

```go
// pm.go
type LLMClient interface {
    Complete(ctx, sys, user) (string, error)
}

// pm.go (new in S8.3)
type SchemaLLMClient interface {
    LLMClient
    CompleteWithSchema(ctx, sys, user, schema []byte) (string, error)
}
```

`analystBase.callLLMForReport` and `callAdvocateLLM` both now
check `if schemaClient, ok := llm.(SchemaLLMClient); ok` and
prefer the schema path. This is purely additive — every
existing test that wires a `Complete`-only client keeps
working.

## Adapter

`agent.LLMAdapter` wraps an `llm.LLMClient` and implements
both interfaces. The options are:

```go
NewLLMAdapter(client, fundID,
    WithLLMAdapterUser(userID),
    WithLLMAdapterAgent(agentID),
    WithLLMAdapterStep("analyst:fundamentals"),
    WithLLMAdapterTier(llm.TierStandard),
    WithLLMAdapterModel("gpt-4o"),    // optional override
    WithLLMAdapterMaxTokens(2048),
    WithLLMAdapterTemperature(0.2),
)
```

`Complete` sends a 2-message ChatRequest with `ResponseFormat
= ""` (freeform). `CompleteWithSchema` sends the same shape
with `ResponseFormat = "json_schema"` and the schema bytes
attached when non-empty (or `"json_object"` when the schema is
nil — useful for the rare caller who wants any JSON shape).

## Wiring

`cmd/server/wiring_analyst_panel.go` and `wiring_debate.go`
share a new helper:

```go
func agentLLMForFund(svc *Services, fundID, stepName string) agent.LLMClient {
    if svc == nil || svc.LLMRuntime == nil || svc.LLMRuntime.client == nil {
        return nil
    }
    return agent.NewLLMAdapter(svc.LLMRuntime.client, fundID,
        agent.WithLLMAdapterStep(stepName),
        agent.WithLLMAdapterTier(llm.TierStandard),
    )
}
```

The four analysts share one adapter
(`stepName = "analyst_panel"`); Bull and Bear share another
(`stepName = "bullbear_debate"`). When the deployment has no
LLM configured, the helper returns nil and the agents fall
back to their deterministic skeletons as in S8.1 / S8.2.

## Test coverage

`internal/llm/schema_test.go`:
- OpenAI body contains `"response_format":{"type":"json_schema",...,"strict":true}` and the schema bytes round-trip.
- OpenAI `json_object` mode sets the right type.
- OpenAI default request still has no `response_format` (legacy preserved).
- Gemini body sets `responseMimeType` + `responseSchema` for schema mode.
- Gemini `json_object` sets MIME but no schema.
- Claude body system prompt carries the JSON instruction + the schema verbatim (without losing the operator's original system prompt).
- Claude legacy request without `ResponseFormat` doesn't get JSON instructions.
- DeepSeek (shared OpenAI path) carries `json_schema` body.

`internal/agent/llm_adapter_test.go`:
- `Complete` propagates fundID / userID / agentID / stepName / tier / maxTokens / temperature and does NOT set the structured-output knobs.
- `CompleteWithSchema` sets `ResponseFormat = json_schema` and propagates the schema.
- Empty schema falls back to `json_object`.
- Empty user prompt is rejected.
- Nil client errors out cleanly.
- Upstream error is wrapped.
- When the underlying client implements `SchemaLLMClient`, `analystBase.callLLMForReport` routes through `CompleteWithSchema` with `AnalystReportJSONSchema`.
- Same for `callAdvocateLLM` with `AdvocateArgumentJSONSchema`.

## Defaults & follow-ups

- **Schemas are intentionally permissive** (`additionalProperties: true`). Strict-mode failure on a provider would force a fallback retry; we'd rather get a valid superset and ignore extras.
- **Per-fund model overrides are not yet exposed**. S8.4 will surface them via the admin agent-reputation UI so a fund can pin its analysts to a specific model.
- **Claude tool_use is the long-term right answer**. The current system-prompt-injection works but has the same theoretical reliability as the old freeform path. A follow-up ticket can swap Claude over to tool_use once the agent layer can declare the schema as a tool definition.
- **Per-agent persona overrides** are still hard-coded in `wiring_analyst_panel.go` / `wiring_debate.go`. S8.4 will add per-fund persona overrides via admin endpoints.
