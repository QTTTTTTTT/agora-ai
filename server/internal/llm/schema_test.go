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

func TestCallDeepSeek_UsesOpenAIPath_SchemaSurvives(t *testing.T) {
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
	if !strings.Contains(body, `"type":"json_schema"`) {
		t.Errorf("deepseek must also produce json_schema body: %s", body)
	}
}
