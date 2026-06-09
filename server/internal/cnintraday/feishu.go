package cnintraday

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

// FeishuWebhook is the minimal POST-JSON client for a Feishu
// bot incoming-webhook. Production deployments configure the
// webhook URL via env; tests inject a stub that captures the
// posted payload.
//
// We don't depend on the Feishu official SDK because:
//   - the SDK pulls in too many transitive deps for a 200-byte
//     POST
//   - the incoming-webhook surface is stable and trivial
//   - making it injectable lets tests verify the rendered card
//     without an external service
type FeishuWebhook interface {
	Send(ctx context.Context, payload FeishuMessage) error
}

// FeishuMessage is the wire shape of the Feishu incoming webhook.
// The MVP uses the "rich text" (post) message type because:
//   - it renders well on mobile (where the operator will read it)
//   - it supports clickable links + bold headers
//   - the JSON schema is stable
type FeishuMessage struct {
	MsgType string         `json:"msg_type"`
	Content FeishuContent  `json:"content"`
}

type FeishuContent struct {
	// Post is the rich-text envelope. Required when MsgType="post".
	Post map[string]FeishuLocale `json:"post,omitempty"`
	// Text is the fallback for MsgType="text".
	Text string `json:"text,omitempty"`
}

type FeishuLocale struct {
	Title   string           `json:"title"`
	Content [][]FeishuPostNode `json:"content"`
}

type FeishuPostNode struct {
	Tag  string `json:"tag"`            // "text" / "a"
	Text string `json:"text,omitempty"`
	Href string `json:"href,omitempty"`
}

// HTTPFeishuWebhook is the production implementation. WebhookURL
// comes from the operator's bot configuration.
type HTTPFeishuWebhook struct {
	WebhookURL string
	Client     *http.Client
}

func NewHTTPFeishuWebhook(url string) *HTTPFeishuWebhook {
	return &HTTPFeishuWebhook{
		WebhookURL: url,
		Client: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// Send POSTs the payload to Feishu. Returns nil on 2xx, error
// otherwise. Idempotent — Feishu de-dupes identical bodies
// arriving within a 1-minute window.
func (w *HTTPFeishuWebhook) Send(ctx context.Context, payload FeishuMessage) error {
	if w == nil || strings.TrimSpace(w.WebhookURL) == "" {
		return fmt.Errorf("feishu webhook URL not configured")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal feishu payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build feishu request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := w.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post feishu: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("feishu returned %d: %s", resp.StatusCode, string(respBody))
}

// StubFeishuWebhook is the test/dry-run implementation: it
// captures the latest payload in a slice instead of POSTing.
type StubFeishuWebhook struct {
	Sent []FeishuMessage
}

func (s *StubFeishuWebhook) Send(_ context.Context, payload FeishuMessage) error {
	s.Sent = append(s.Sent, payload)
	return nil
}

// RenderSignal converts a TradeSignal into a Feishu rich-text
// post. The card layout (rendered on mobile):
//
//   [BUY] 海康威视 (002415) ¥35.42
//   信心 0.78 · 建议仓位 10%
//   ─────────────────────────────
//   ↑ 突破前 60min 高点 (z=2.31)
//   ↑ 放量 (5min vol / 20min vol = 1.82x)
//   ↑ 大单净流入 580 万
//   ─────────────────────────────
//   目标 ¥37.19 (+5%)
//   止损 ¥34.36 (-3%)
//
// Keep this rendering distinct from the engine itself so the
// frontend / Feishu / Bark / Discord / etc. can all reuse the
// TradeSignal data and only differ in rendering.
func RenderSignal(sig *TradeSignal) FeishuMessage {
	if sig == nil {
		return FeishuMessage{MsgType: "text", Content: FeishuContent{Text: "(empty signal)"}}
	}
	title := fmt.Sprintf("[%s] %s (%s) ¥%.2f", sig.Type, sig.Name, sig.Symbol, sig.Price)
	header := fmt.Sprintf("信心 %.2f · 建议仓位 %.0f%%", sig.Confidence, sig.SuggestedPosition*100)

	rows := [][]FeishuPostNode{
		{{Tag: "text", Text: header}},
	}
	for _, r := range sig.Reasons {
		rows = append(rows, []FeishuPostNode{{Tag: "text", Text: "• " + r}})
	}
	if sig.TargetPrice > 0 {
		rows = append(rows, []FeishuPostNode{
			{Tag: "text", Text: fmt.Sprintf("目标 ¥%.2f · 止损 ¥%.2f", sig.TargetPrice, sig.StopLoss)},
		})
	}
	for _, w := range sig.RiskWarnings {
		rows = append(rows, []FeishuPostNode{{Tag: "text", Text: "⚠ " + w}})
	}
	return FeishuMessage{
		MsgType: "post",
		Content: FeishuContent{
			Post: map[string]FeishuLocale{
				"zh_cn": {Title: title, Content: rows},
			},
		},
	}
}
