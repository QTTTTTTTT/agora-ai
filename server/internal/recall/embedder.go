package recall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Embedder 把单条文本编码成 float32 向量。返回向量长度必须与
// Service.EmbeddingDimension 一致；否则后续 Query 会被 dim mismatch
// guard 拦下。
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	// Model 返回 embed model 标识（写到 memories.embedding_model 列），
	// 方便日后切模型时按列灰度。
	Model() string
}

// OpenAIEmbedder 是 OpenAI v1/embeddings endpoint 的最小适配器。
// 默认 text-embedding-3-small（1536 维），可改 text-embedding-3-large
// （3072 维）— 注意 vector 列维度必须同步升级。
type OpenAIEmbedder struct {
	APIKey  string
	BaseURL string
	HTTP    *http.Client
	ModelID string
}

// NewOpenAIEmbedder 用默认配置初始化。BaseURL 缺省 OpenAI 官方，
// 国内部署可换成 mirror。HTTP 超时 30s（不然单个慢调用会把整个
// embed loop 挂住）。
func NewOpenAIEmbedder(apiKey string) *OpenAIEmbedder {
	return &OpenAIEmbedder{
		APIKey:  apiKey,
		BaseURL: "https://api.openai.com/v1",
		ModelID: "text-embedding-3-small",
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (e *OpenAIEmbedder) Model() string {
	if e == nil || strings.TrimSpace(e.ModelID) == "" {
		return "text-embedding-3-small"
	}
	return e.ModelID
}

type openAIEmbedRequest struct {
	Input          string `json:"input"`
	Model          string `json:"model"`
	EncodingFormat string `json:"encoding_format,omitempty"`
}

type openAIEmbedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error,omitempty"`
}

// Embed 调一次 OpenAI v1/embeddings。空字符串 → 直接返回 nil（callsite
// 应该过滤过；如果传到这里就是 bug，但我们 not panic）。
func (e *OpenAIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if e == nil || strings.TrimSpace(e.APIKey) == "" {
		return nil, errors.New("recall: openai embedder unconfigured")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	payload, _ := json.Marshal(openAIEmbedRequest{
		Input: text,
		Model: e.Model(),
	})
	url := strings.TrimSuffix(e.BaseURL, "/") + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.APIKey)
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("recall: openai embed http %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var parsed openAIEmbedResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("recall: parse embed response: %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("recall: openai embed err: %s", parsed.Error.Message)
	}
	if len(parsed.Data) == 0 || len(parsed.Data[0].Embedding) == 0 {
		return nil, errors.New("recall: empty embedding from openai")
	}
	return parsed.Data[0].Embedding, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
