// admin_llm_providers_ping.go — provider-specific 1-token "ping"
// used by the admin Test Connection button (S13).
//
// We deliberately roll our own minimal HTTP client (not the
// production multi-provider client) because the production client
// pulls in usage/billing/budget hooks that depend on a fully wired
// llmRuntime — the test path needs to work even on a brand-new
// row that has not been reloaded into the router. The probe is
// strictly read-only: it does not write usage, does not mutate
// state, and does not retry on failure.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func runProviderPing(ctx context.Context, provider, baseURL, model, apiKey string) testLLMProviderResponse {
	provider = strings.ToLower(strings.TrimSpace(provider))
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	model = strings.TrimSpace(model)
	apiKey = strings.TrimSpace(apiKey)

	url, body, headers, err := buildPingRequest(provider, baseURL, model, apiKey)
	if err != nil {
		return testLLMProviderResponse{OK: false, Message: err.Error()}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return testLLMProviderResponse{OK: false, Message: fmt.Sprintf("new request: %v", err)}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 8 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return testLLMProviderResponse{
			OK:        false,
			LatencyMS: latency,
			Message:   fmt.Sprintf("transport: %v", err),
		}
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return testLLMProviderResponse{
			OK:         false,
			LatencyMS:  latency,
			HTTPStatus: resp.StatusCode,
			Message:    extractErrorMessage(respBytes, resp.StatusCode),
		}
	}

	echoed := extractEchoedModel(respBytes)
	return testLLMProviderResponse{
		OK:          true,
		LatencyMS:   latency,
		HTTPStatus:  resp.StatusCode,
		Message:     "connection successful",
		EchoedModel: echoed,
	}
}

// buildPingRequest produces a provider-appropriate minimal payload.
// We keep it to "Hi" / max_tokens=1 so the dollar cost per ping is
// well under a cent across all providers we support.
func buildPingRequest(provider, baseURL, model, apiKey string) (string, []byte, map[string]string, error) {
	headers := map[string]string{"Content-Type": "application/json"}
	switch provider {
	case "claude":
		body, err := json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": 1,
			"messages": []map[string]string{
				{"role": "user", "content": "Hi"},
			},
		})
		if err != nil {
			return "", nil, nil, fmt.Errorf("marshal claude: %w", err)
		}
		headers["x-api-key"] = apiKey
		headers["anthropic-version"] = "2023-06-01"
		return baseURL + "/messages", body, headers, nil
	case "gemini":
		body, err := json.Marshal(map[string]any{
			"contents": []map[string]any{
				{"role": "user", "parts": []map[string]string{{"text": "Hi"}}},
			},
			"generationConfig": map[string]any{"maxOutputTokens": 1},
		})
		if err != nil {
			return "", nil, nil, fmt.Errorf("marshal gemini: %w", err)
		}
		// Gemini accepts either x-goog-api-key OR ?key=. We use the
		// header form so the key never appears in a URL — same posture
		// as subscription/model_config.go's pre-existing tester.
		headers["x-goog-api-key"] = apiKey
		url := fmt.Sprintf("%s/models/%s:generateContent", baseURL, model)
		return url, body, headers, nil
	default:
		// openai / deepseek / qwen / custom — all OpenAI-compatible.
		body, err := json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": 1,
			"messages": []map[string]string{
				{"role": "user", "content": "Hi"},
			},
			"temperature": 0,
		})
		if err != nil {
			return "", nil, nil, fmt.Errorf("marshal openai-compatible: %w", err)
		}
		headers["Authorization"] = "Bearer " + apiKey
		return baseURL + "/chat/completions", body, headers, nil
	}
}

func extractErrorMessage(body []byte, status int) string {
	// Try a JSON error structure first.
	var generic struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &generic) == nil && generic.Error.Message != "" {
		return fmt.Sprintf("HTTP %d: %s", status, generic.Error.Message)
	}
	// Fallback to a stripped raw payload.
	preview := string(body)
	if len(preview) > 200 {
		preview = preview[:200] + "…"
	}
	preview = strings.ReplaceAll(preview, "\n", " ")
	if preview == "" {
		return fmt.Sprintf("HTTP %d (empty body)", status)
	}
	return fmt.Sprintf("HTTP %d: %s", status, preview)
}

func extractEchoedModel(body []byte) string {
	var resp struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &resp)
	return resp.Model
}
