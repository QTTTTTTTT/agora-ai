// schema_test.go — covers the S8.3 native structured-output
// hooks on each provider's request body. We mock the upstream
// HTTP transport so we can assert exactly what the client
// sends — the JSON body must carry response_format /
// responseSchema / system instruction for the three families
// (OpenAI / Gemini / Claude).

package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// transportFn lets a test capture the outbound request body
// and return any response we like.
type transportFn func(req *http.Request) (*http.Response, error)

func (f transportFn) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// captureBody pulls the body out of an outbound request and
// returns it as a string (assumes UTF-8 JSON).
func captureBody(t *testing.T, req *http.Request) string {
	t.Helper()
	if req.Body == nil {
		return ""
	}
	defer req.Body.Close()
	b, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("captureBody: %v", err)
	}
	return string(b)
}

// stub responses
const stubOpenAIResp = `{"id":"r1","choices":[{"message":{"role":"assistant","content":"{\"ok\":true}"}}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`
const stubGeminiResp = `{"candidates":[{"content":{"parts":[{"text":"{\"ok\":true}"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2,"totalTokenCount":3}}`
const stubClaudeResp = `{"id":"r1","type":"message","role":"assistant","content":[{"type":"text","text":"{\"ok\":true}"}],"usage":{"input_tokens":1,"output_tokens":2}}`

// newCapturingClient builds a MultiProviderClient whose HTTP
// transport just captures requests + returns one canned body.
func newCapturingClient(t *testing.T, capture *string, respBody string) *MultiProviderClient {
	t.Helper()
	httpClient := &http.Client{
		Transport: transportFn(func(req *http.Request) (*http.Response, error) {
			*capture = captureBody(t, req)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(respBody)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Request:    req,
			}, nil
		}),
	}
	return &MultiProviderClient{httpClient: httpClient}
}

func TestCallOpenAI_JSONSchemaSetsResponseFormat(t *testing.T) {
	var body string
	c := newCapturingClient(t, &body, stubOpenAIResp)
	cfg := &ModelConfig{
		Provider: ProviderOpenAI, ModelName: "gpt-4o", BaseURL: "https://api.example/v1",
		APIKey: "k", MaxTokens: 256,
	}
	req := ChatRequest{
		Messages: []ChatMessage{
			{Role: "system", Content: "be helpful"},
			{Role: "user", Content: "go"},
		},
		ResponseFormat: "json_schema",
		ResponseSchema: []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
	}
	if _, err := c.callOpenAI(context.Background(), cfg, req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"response_format"`) {
		t.Errorf("response_format missing from body: %s", body)
	}
	if !strings.Contains(body, `"type":"json_schema"`) {
		t.Errorf("json_schema type missing from body: %s", body)
	}
	if !strings.Contains(body, `"strict":true`) {
		t.Errorf("strict:true missing from body: %s", body)
	}
	// Schema bytes must survive round-trip.
	if !strings.Contains(body, `"required":["ok"]`) {
		t.Errorf("schema content missing from body: %s", body)
	}
}

func TestCallOpenAI_JSONObjectFallsBack(t *testing.T) {
	var body string
	c := newCapturingClient(t, &body, stubOpenAIResp)
	cfg := &ModelConfig{Provider: ProviderOpenAI, ModelName: "gpt-4o", BaseURL: "https://api.example/v1", APIKey: "k"}
	req := ChatRequest{
		Messages:       []ChatMessage{{Role: "user", Content: "go"}},
		ResponseFormat: "json_object",
	}
	if _, err := c.callOpenAI(context.Background(), cfg, req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"type":"json_object"`) {
		t.Errorf("json_object type missing from body: %s", body)
	}
}

func TestCallOpenAI_NoResponseFormat_Default(t *testing.T) {
	var body string
	c := newCapturingClient(t, &body, stubOpenAIResp)
	cfg := &ModelConfig{Provider: ProviderOpenAI, ModelName: "gpt-4o", BaseURL: "https://api.example/v1", APIKey: "k"}
	req := ChatRequest{Messages: []ChatMessage{{Role: "user", Content: "go"}}}
	if _, err := c.callOpenAI(context.Background(), cfg, req); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, `"response_format"`) {
		t.Errorf("response_format should be absent without opt-in: %s", body)
	}
}

func TestCallGemini_JSONSchemaSetsConfig(t *testing.T) {
	var body string
	c := newCapturingClient(t, &body, stubGeminiResp)
	cfg := &ModelConfig{Provider: ProviderGemini, ModelName: "gemini-3.1-flash", BaseURL: "https://gen.example", APIKey: "k", MaxTokens: 256}
	req := ChatRequest{
		Messages: []ChatMessage{
			{Role: "system", Content: "be helpful"},
			{Role: "user", Content: "go"},
		},
		ResponseFormat: "json_schema",
		ResponseSchema: []byte(`{"type":"object"}`),
	}
	if _, err := c.callGemini(context.Background(), cfg, req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"responseMimeType":"application/json"`) {
		t.Errorf("responseMimeType missing from gemini body: %s", body)
	}
	if !strings.Contains(body, `"responseSchema":{"type":"object"}`) {
		t.Errorf("responseSchema missing from gemini body: %s", body)
	}
}

// TestCallGemini_StripsAdditionalProperties is the regression test
// for the upstream 400 our advisor master agents tripped on:
//
//	gemini gemini-3.1-pro-preview failed (invalid_request, status=400):
//	  Invalid JSON payload received. Unknown name "additionalProperties"
//	  at 'generation_config.response_schema': Cannot find field.
//
// Gemini's responseSchema is an OpenAPI 3.0 Schema subset and rejects
// any unknown JSON-Schema keyword. callGemini must strip those before
// the bytes hit the wire so the agent layer can keep one
// OpenAI-compatible schema across all providers.
func TestCallGemini_StripsAdditionalProperties(t *testing.T) {
	var body string
	c := newCapturingClient(t, &body, stubGeminiResp)
	cfg := &ModelConfig{Provider: ProviderGemini, ModelName: "gemini-3.1-pro-preview", BaseURL: "https://gen.example", APIKey: "k", MaxTokens: 256}
	req := ChatRequest{
		Messages:       []ChatMessage{{Role: "user", Content: "go"}},
		ResponseFormat: "json_schema",
		ResponseSchema: []byte(`{
			"type": "object",
			"properties": {
				"verdict": {"type": "string"},
				"nested":  {"type": "object", "properties": {"x": {"type": "integer"}}, "additionalProperties": true}
			},
			"required": ["verdict"],
			"additionalProperties": true,
			"$schema": "http://json-schema.org/draft-07/schema#"
		}`),
	}
	if _, err := c.callGemini(context.Background(), cfg, req); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, `additionalProperties`) {
		t.Errorf("additionalProperties must be stripped, body=%s", body)
	}
	if strings.Contains(body, `$schema`) {
		t.Errorf("$schema must be stripped, body=%s", body)
	}
	if !strings.Contains(body, `"required":["verdict"]`) {
		t.Errorf("required list should survive, body=%s", body)
	}
	if !strings.Contains(body, `"properties"`) || !strings.Contains(body, `"verdict"`) {
		t.Errorf("properties should survive, body=%s", body)
	}
}

// TestSanitizeGeminiSchema_PreservesSupportedKeywords pins the
// allow-list behaviour of the sanitizer: anything not on the deny
// list must round-trip untouched. Without this guard, the next
// drive-by edit could over-strip and silently lose enum / range
// constraints, which Gemini DOES enforce.
func TestSanitizeGeminiSchema_PreservesSupportedKeywords(t *testing.T) {
	in := []byte(`{
		"type": "object",
		"properties": {
			"direction": {"type": "string", "enum": ["bullish","bearish","neutral"]},
			"confidence": {"type": "integer", "minimum": 0, "maximum": 100},
			"tags": {"type": "array", "items": {"type": "string"}, "minItems": 1, "maxItems": 5}
		},
		"required": ["direction","confidence"],
		"additionalProperties": true
	}`)
	out := sanitizeGeminiSchema(in)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("sanitized output not JSON: %v\n%s", err, out)
	}
	if _, ok := got["additionalProperties"]; ok {
		t.Errorf("additionalProperties leaked: %s", out)
	}
	for _, key := range []string{"type", "properties", "required"} {
		if _, ok := got[key]; !ok {
			t.Errorf("supported key %q dropped, got %s", key, out)
		}
	}
	props, _ := got["properties"].(map[string]any)
	direction, _ := props["direction"].(map[string]any)
	if enum, ok := direction["enum"].([]any); !ok || len(enum) != 3 {
		t.Errorf("enum dropped, got %s", out)
	}
	confidence, _ := props["confidence"].(map[string]any)
	if confidence["minimum"] == nil || confidence["maximum"] == nil {
		t.Errorf("minimum/maximum dropped, got %s", out)
	}
	tags, _ := props["tags"].(map[string]any)
	if tags["minItems"] == nil || tags["maxItems"] == nil {
		t.Errorf("minItems/maxItems dropped, got %s", out)
	}
}

// TestSanitizeGeminiSchema_NoOpWhenAlreadyClean keeps the helper
// idempotent so callers can re-run it without ill effect (and so
// schemas that were already Gemini-shaped don't grow an opaque
// JSON re-encoding diff in logs / curl debug output).
func TestSanitizeGeminiSchema_NoOpWhenAlreadyClean(t *testing.T) {
	in := []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	out := sanitizeGeminiSchema(in)
	var a, b any
	if err := json.Unmarshal(in, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("clean schema mutated\n in=%s\nout=%s", in, out)
	}
}

func TestCallGemini_JSONObject_NoSchema(t *testing.T) {
	var body string
	c := newCapturingClient(t, &body, stubGeminiResp)
	cfg := &ModelConfig{Provider: ProviderGemini, ModelName: "gemini-3.1-flash", BaseURL: "https://gen.example", APIKey: "k"}
	req := ChatRequest{
		Messages:       []ChatMessage{{Role: "user", Content: "go"}},
		ResponseFormat: "json_object",
	}
	if _, err := c.callGemini(context.Background(), cfg, req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"responseMimeType":"application/json"`) {
		t.Errorf("responseMimeType missing from gemini body: %s", body)
	}
	if strings.Contains(body, `"responseSchema"`) {
		t.Errorf("responseSchema should be absent for json_object: %s", body)
	}
}

func TestCallClaude_JSONSchemaAppendsInstruction(t *testing.T) {
	var body string
	c := newCapturingClient(t, &body, stubClaudeResp)
	cfg := &ModelConfig{Provider: ProviderClaude, ModelName: "claude-opus-4", BaseURL: "https://claude.example/v1", APIKey: "k", MaxTokens: 256}
	req := ChatRequest{
		Messages: []ChatMessage{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "go"},
		},
		ResponseFormat: "json_schema",
		ResponseSchema: []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
	}
	if _, err := c.callClaude(context.Background(), cfg, req); err != nil {
		t.Fatal(err)
	}
	// System prompt must mention the schema verbatim and the
	// JSON-only instruction.
	var parsed claudeRequest
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal claude body: %v", err)
	}
	if !strings.Contains(parsed.System, "Return ONLY a JSON object") {
		t.Errorf("system prompt missing JSON instruction: %q", parsed.System)
	}
	if !strings.Contains(parsed.System, `"type":"object"`) {
		t.Errorf("system prompt missing schema text: %q", parsed.System)
	}
	if !strings.Contains(parsed.System, "be terse") {
		t.Errorf("system prompt should preserve original: %q", parsed.System)
	}
}

func TestCallClaude_JSONObject_OnlyInstruction(t *testing.T) {
	var body string
	c := newCapturingClient(t, &body, stubClaudeResp)
	cfg := &ModelConfig{Provider: ProviderClaude, ModelName: "claude-opus-4", BaseURL: "https://claude.example/v1", APIKey: "k", MaxTokens: 256}
	req := ChatRequest{
		Messages: []ChatMessage{
			{Role: "user", Content: "go"},
		},
		ResponseFormat: "json_object",
	}
	if _, err := c.callClaude(context.Background(), cfg, req); err != nil {
		t.Fatal(err)
	}
	var parsed claudeRequest
	_ = json.Unmarshal([]byte(body), &parsed)
	if !strings.Contains(parsed.System, "Return ONLY a JSON object") {
		t.Errorf("system prompt missing JSON instruction: %q", parsed.System)
	}
}

func TestCallClaude_NoResponseFormat(t *testing.T) {
	var body string
	c := newCapturingClient(t, &body, stubClaudeResp)
	cfg := &ModelConfig{Provider: ProviderClaude, ModelName: "claude-opus-4", BaseURL: "https://claude.example/v1", APIKey: "k", MaxTokens: 256}
	req := ChatRequest{
		Messages: []ChatMessage{
			{Role: "system", Content: "be helpful"},
			{Role: "user", Content: "go"},
		},
	}
	if _, err := c.callClaude(context.Background(), cfg, req); err != nil {
		t.Fatal(err)
	}
	var parsed claudeRequest
	_ = json.Unmarshal([]byte(body), &parsed)
	if strings.Contains(parsed.System, "Return ONLY a JSON") {
		t.Errorf("legacy claude calls should not get JSON instructions: %q", parsed.System)
	}
}

func TestCallDeepSeek_UsesOpenAIPath_SchemaAsPromptInstruction(t *testing.T) {
	var body string
	c := newCapturingClient(t, &body, stubOpenAIResp)
	cfg := &ModelConfig{Provider: ProviderDeepSeek, ModelName: "deepseek-chat", BaseURL: "https://api.deepseek.com/v1", APIKey: "k"}
	if !cfg.Provider.UsesOpenAIChatCompletions() {
		t.Skip("provider taxonomy changed")
	}
	req := ChatRequest{
		Messages:       []ChatMessage{{Role: "user", Content: "go"}},
		ResponseFormat: "json_schema",
		ResponseSchema: []byte(`{"type":"object"}`),
	}
	if _, err := c.callOpenAI(context.Background(), cfg, req); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, `"response_format"`) {
		t.Errorf("deepseek should not send unsupported response_format field: %s", body)
	}
	if !strings.Contains(body, "Return ONLY one complete JSON object") {
		t.Errorf("deepseek should preserve JSON intent via prompt instruction: %s", body)
	}
}

// TestIsGeminiThinkingModel locks down the family-prefix matcher so
// the 4096-token budget bug we hit on gemini-3.1-pro-preview can't
// silently come back when a new 2.5/3.x point release is wired in.
// Non-thinking families (1.5, 1.0, OpenAI / Claude names) must stay
// false so we don't grow the request payload for models that 400 on
// thinkingConfig.
func TestIsGeminiThinkingModel(t *testing.T) {
	cases := map[string]bool{
		"gemini-2.5-pro":              true,
		"gemini-2.5-flash":            true,
		"gemini-2.5-pro-preview-0325": true,
		"gemini-3.1-pro-preview":      true,
		"gemini-3.1-flash":            true,
		"gemini-3-pro":                true,
		"GEMINI-3.1-PRO-PREVIEW":      true,  // case-insensitive
		"  gemini-3.1-pro-preview  ":  true,  // trim space
		"gemini-1.5-pro":              false, // pre-thinking family
		"gemini-1.5-flash":            false,
		"gemini-pro":                  false, // 1.0 era
		"gpt-4o":                      false,
		"claude-opus-4":               false,
		"":                            false,
	}
	for name, want := range cases {
		if got := isGeminiThinkingModel(name); got != want {
			t.Errorf("isGeminiThinkingModel(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestGeminiThinkingBudget pins the policy: visible answer must
// always have headroom even after the model spends its full thinking
// cap, and callers that already asked for a large envelope (>=
// minVisible + thinkingHeadroom) get to keep that bigger number.
func TestGeminiThinkingBudget(t *testing.T) {
	cases := []struct {
		name              string
		caller            int
		wantMinMaxOutput  int // floor — actual may be >=
		wantThinkingFixed int
	}{
		{"zero_caller_gets_default_envelope", 0, 8192, 4096},
		{"tiny_caller_gets_bumped_to_at_least_visible+headroom", 256, 3072 + 4096, 4096},
		{"4096_old_default_gets_bumped_above_that", 4096, 3072 + 4096, 4096},
		{"large_caller_is_preserved", 16384, 16384, 4096},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, think := geminiThinkingBudget(tc.caller)
			if out < tc.wantMinMaxOutput {
				t.Errorf("maxOutput=%d, want >=%d", out, tc.wantMinMaxOutput)
			}
			if think != tc.wantThinkingFixed {
				t.Errorf("thinkingBudget=%d, want %d", think, tc.wantThinkingFixed)
			}
			// Invariant: visible answer must have >= 3072 tokens
			// even after thinking burns its full cap.
			if visible := out - think; visible < 3072 {
				t.Errorf("visible budget=%d, want >=3072 (out=%d, think=%d)", visible, out, think)
			}
		})
	}
}

// TestCallGemini_ThinkingModelAutoBumpsBudget is the end-to-end
// guard for the regression. The advisor wiring passes MaxTokens=4096
// (a perfectly fine cap for non-thinking models). When the model is
// a thinking-capable family, callGemini must:
//
//  1. raise maxOutputTokens so thinking can't crowd out the answer
//  2. add a thinkingConfig.thinkingBudget cap so a single runaway
//     reasoning chain can't burn the whole envelope either
//
// Both knobs together are what stops the "no json object found"
// downstream parse failures on a 2000-token persona prompt.
func TestCallGemini_ThinkingModelAutoBumpsBudget(t *testing.T) {
	var body string
	c := newCapturingClient(t, &body, stubGeminiResp)
	cfg := &ModelConfig{
		Provider:  ProviderGemini,
		ModelName: "gemini-3.1-pro-preview",
		BaseURL:   "https://gen.example",
		APIKey:    "k",
		MaxTokens: 4096, // the production default that triggered the bug
	}
	req := ChatRequest{
		Messages:       []ChatMessage{{Role: "user", Content: "go"}},
		ResponseFormat: "json_schema",
		ResponseSchema: []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
	}
	if _, err := c.callGemini(context.Background(), cfg, req); err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("unmarshal gemini body: %v\n%s", err, body)
	}
	gen, _ := sent["generationConfig"].(map[string]any)
	if gen == nil {
		t.Fatalf("generationConfig missing from gemini body: %s", body)
	}
	maxOut, _ := gen["maxOutputTokens"].(float64)
	if int(maxOut) <= 4096 {
		t.Errorf("maxOutputTokens=%v not bumped above caller default 4096 for thinking model (body=%s)", maxOut, body)
	}
	tc, _ := gen["thinkingConfig"].(map[string]any)
	if tc == nil {
		t.Fatalf("thinkingConfig missing from gemini body for thinking model: %s", body)
	}
	think, _ := tc["thinkingBudget"].(float64)
	if think <= 0 {
		t.Errorf("thinkingBudget=%v must be a positive cap for thinking model (body=%s)", think, body)
	}
	if visible := int(maxOut) - int(think); visible < 3072 {
		t.Errorf("visible budget=%d, want >=3072 to fit a real answer", visible)
	}
}

// TestCallGemini_NonThinkingModelLeavesBudgetAlone is the negative
// case: for a 1.5 (or any non-thinking) family we must NOT add
// thinkingConfig (the relay would 400 on the unknown field) nor
// alter the caller-supplied maxOutputTokens.
func TestCallGemini_NonThinkingModelLeavesBudgetAlone(t *testing.T) {
	var body string
	c := newCapturingClient(t, &body, stubGeminiResp)
	cfg := &ModelConfig{
		Provider:  ProviderGemini,
		ModelName: "gemini-1.5-pro",
		BaseURL:   "https://gen.example",
		APIKey:    "k",
		MaxTokens: 4096,
	}
	req := ChatRequest{
		Messages: []ChatMessage{{Role: "user", Content: "go"}},
	}
	if _, err := c.callGemini(context.Background(), cfg, req); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, `"thinkingConfig"`) {
		t.Errorf("non-thinking gemini model must not carry thinkingConfig: %s", body)
	}
	if !strings.Contains(body, `"maxOutputTokens":4096`) {
		t.Errorf("non-thinking gemini model must keep caller maxOutputTokens=4096: %s", body)
	}
}

// TestCallGemini_MaxTokensTruncationSurfacesAsDistinctError checks
// that when Gemini truncates the response (finishReason=MAX_TOKENS,
// empty text part), we no longer report a generic "empty_content"
// but a specific "max_tokens_truncated" error so operators can tell
// which knob to tune. This is the symptom that surfaced as the
// silent "暂无足够数据形成强观点" fallback in the advisor.
func TestCallGemini_MaxTokensTruncationSurfacesAsDistinctError(t *testing.T) {
	// finishReason=MAX_TOKENS, empty text (this is exactly what
	// Gemini emits when the thinking pass eats the whole envelope).
	truncatedResp := `{"candidates":[{"content":{"parts":[{"text":""}],"role":"model"},"finishReason":"MAX_TOKENS","index":0}],"usageMetadata":{"promptTokenCount":1416,"candidatesTokenCount":0,"totalTokenCount":5512,"thoughtsTokenCount":4096}}`
	var body string
	c := newCapturingClient(t, &body, truncatedResp)
	cfg := &ModelConfig{
		Provider:  ProviderGemini,
		ModelName: "gemini-3.1-pro-preview",
		BaseURL:   "https://gen.example",
		APIKey:    "k",
		MaxTokens: 4096,
	}
	req := ChatRequest{
		Messages:       []ChatMessage{{Role: "user", Content: "go"}},
		ResponseFormat: "json_schema",
		ResponseSchema: []byte(`{"type":"object"}`),
	}
	_, err := c.callGemini(context.Background(), cfg, req)
	if err == nil {
		t.Fatal("expected error on MAX_TOKENS truncation, got nil")
	}
	lre, ok := err.(*llmRequestError)
	if !ok {
		t.Fatalf("expected *llmRequestError, got %T: %v", err, err)
	}
	if lre.Reason != "max_tokens_truncated" {
		t.Errorf("Reason=%q, want %q", lre.Reason, "max_tokens_truncated")
	}
	if !lre.Retryable {
		t.Error("MAX_TOKENS truncation should be marked Retryable so the orchestrator can re-run with a bigger budget")
	}
	// Diagnostic body should help an operator: cite the token counts.
	for _, want := range []string{"thoughts=4096", "candidates=0", "maxOutputTokens="} {
		if !strings.Contains(lre.Message, want) {
			t.Errorf("error message missing %q, got %q", want, lre.Message)
		}
	}
}
