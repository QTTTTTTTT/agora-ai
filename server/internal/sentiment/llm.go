package sentiment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fundai/server/internal/llm"
)

// LLMScorer hands a batch of news items to the configured LLM and
// parses a structured JSON response. The prompt is engineered to
// be terse so a fast-tier model (Haiku / Sonnet / DeepSeek-V3) can
// score 20+ headlines in a single call.
//
// The scorer does NOT chain to keyword fallback by itself —
// CompositeScorer is responsible for that policy. The scorer
// reports an error and the caller decides what to do.
type LLMScorer struct {
	Client      llm.LLMClient
	ModelTier   llm.ModelTier
	Temperature float64
	MaxTokens   int

	// Identification for accounting.
	UserID   string
	AgentID  string
	StepName string
	FundID   string

	// MaxItems caps a single LLM call. Beyond this we chunk so we
	// don't blow the context window. Default 30.
	MaxItems int
}

// Score implements Scorer. Returns an error if the LLM produces
// malformed JSON or empty content.
func (s *LLMScorer) Score(ctx context.Context, items []Item) ([]Score, error) {
	if s == nil || s.Client == nil {
		return nil, errors.New("llm sentiment scorer: client not configured")
	}
	if len(items) == 0 {
		return nil, nil
	}
	limit := s.MaxItems
	if limit <= 0 {
		limit = 30
	}
	out := make([]Score, 0, len(items))
	for i := 0; i < len(items); i += limit {
		end := i + limit
		if end > len(items) {
			end = len(items)
		}
		scored, err := s.scoreBatch(ctx, items[i:end])
		if err != nil {
			return nil, err
		}
		out = append(out, scored...)
	}
	return out, nil
}

func (s *LLMScorer) scoreBatch(ctx context.Context, items []Item) ([]Score, error) {
	req := llm.ChatRequest{
		Messages: []llm.ChatMessage{
			{Role: "system", Content: llmSystemPrompt()},
			{Role: "user", Content: llmUserPrompt(items)},
		},
		ModelTier:   s.ModelTier,
		MaxTokens:   s.maxTokens(),
		Temperature: s.temperature(),
		UserID:      s.UserID,
		AgentID:     s.AgentID,
		StepName:    s.StepName,
		FundID:      s.FundID,
	}
	resp, err := s.Client.Chat(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("llm sentiment scorer: chat: %w", err)
	}
	if resp == nil || strings.TrimSpace(resp.Content) == "" {
		return nil, errors.New("llm sentiment scorer: empty response")
	}
	parsed, err := parseLLMScores(resp.Content)
	if err != nil {
		return nil, fmt.Errorf("llm sentiment scorer: parse: %w", err)
	}
	// Build an ID→Score map so we tolerate the LLM reordering rows
	// or omitting some items.
	byID := make(map[string]Score, len(parsed))
	for _, r := range parsed {
		byID[r.ID] = r
	}
	out := make([]Score, 0, len(items))
	for _, item := range items {
		if r, ok := byID[item.ID]; ok {
			r.Score = clampScore(r.Score)
			r.Confidence = clampConfidence(r.Confidence)
			out = append(out, r)
			continue
		}
		// Unscored items default to neutral / low confidence so
		// the aggregator doesn't drop them entirely.
		out = append(out, Score{ID: item.ID, Confidence: 0.1})
	}
	return out, nil
}

func (s *LLMScorer) maxTokens() int {
	if s.MaxTokens > 0 {
		return s.MaxTokens
	}
	// Reasoning-tier models (Gemini 3.x Pro Preview etc.) burn an
	// internal "thoughts" budget out of MaxOutputTokens before
	// emitting a single visible char. At the old 1200 cap a 12-item
	// sentiment batch returned an empty / truncated payload and the
	// caller fell back to the legacy scorer ("primary sentiment
	// scorer failed; trying fallback" / "unexpected end of JSON
	// input" — see agent transcript dce9e865 2026-05-22). 6000
	// leaves ~3-4k for thoughts and still keeps the per-call cost
	// bounded for the largest realistic batch we send.
	return 6000
}

func (s *LLMScorer) temperature() float64 {
	if s.Temperature > 0 {
		return s.Temperature
	}
	return 0.1
}

func llmSystemPrompt() string {
	return strings.Join([]string{
		"You are a financial news sentiment classifier.",
		"For each news item, score how it is likely to move the priced equities involved.",
		"Use a strict JSON schema — no preamble, no markdown, no trailing prose.",
		"Output shape:",
		`{"scores":[{"id":"...","score":-1.0..1.0,"confidence":0.0..1.0,"reason":"≤16 words"}]}`,
		"Conventions:",
		"  score < 0  = bearish for the named symbols",
		"  score > 0  = bullish for the named symbols",
		"  |score| ≥ 0.6 = strong signal (clear catalyst)",
		"  confidence = how sure you are about the direction",
		"Always emit every input id exactly once.",
	}, "\n")
}

func llmUserPrompt(items []Item) string {
	var sb strings.Builder
	sb.WriteString("Score these news items:\n\n")
	for _, item := range items {
		sb.WriteString(`{"id":`)
		writeJSONString(&sb, item.ID)
		sb.WriteString(`,"title":`)
		writeJSONString(&sb, item.Title)
		if item.Summary != "" {
			sb.WriteString(`,"summary":`)
			writeJSONString(&sb, item.Summary)
		}
		if len(item.Symbols) > 0 {
			sb.WriteString(`,"symbols":[`)
			for i, sym := range item.Symbols {
				if i > 0 {
					sb.WriteString(",")
				}
				writeJSONString(&sb, sym)
			}
			sb.WriteString(`]`)
		}
		if !item.PublishedAt.IsZero() {
			sb.WriteString(`,"published":`)
			writeJSONString(&sb, item.PublishedAt.UTC().Format(time.RFC3339))
		}
		sb.WriteString("}\n")
	}
	return sb.String()
}

// writeJSONString serialises a string with proper escaping via the
// standard library so we don't hand-roll quoting bugs.
func writeJSONString(sb *strings.Builder, s string) {
	b, _ := json.Marshal(s)
	sb.Write(b)
}

// parseLLMScores tolerates two response shapes:
//
//   {"scores":[ ... ]}     ← preferred
//   [ ... ]                ← bare array
//
// We also strip any markdown code fences the model occasionally
// emits before/after the JSON despite the system prompt.
func parseLLMScores(content string) ([]Score, error) {
	content = stripMarkdownFences(content)
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, errors.New("empty content")
	}
	if content[0] == '[' {
		var arr []rawScoreRow
		if err := json.Unmarshal([]byte(content), &arr); err != nil {
			return nil, fmt.Errorf("array decode: %w", err)
		}
		return rowsToScores(arr), nil
	}
	var wrapper struct {
		Scores []rawScoreRow `json:"scores"`
	}
	if err := json.Unmarshal([]byte(content), &wrapper); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if len(wrapper.Scores) == 0 {
		// Try the LLM emitting {scores: ...} variants like "results".
		var alt struct {
			Results []rawScoreRow `json:"results"`
			Items   []rawScoreRow `json:"items"`
		}
		if err := json.Unmarshal([]byte(content), &alt); err == nil {
			if len(alt.Results) > 0 {
				return rowsToScores(alt.Results), nil
			}
			if len(alt.Items) > 0 {
				return rowsToScores(alt.Items), nil
			}
		}
		return nil, errors.New("scores array empty")
	}
	return rowsToScores(wrapper.Scores), nil
}

type rawScoreRow struct {
	ID         string  `json:"id"`
	Score      float64 `json:"score"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

func rowsToScores(rows []rawScoreRow) []Score {
	out := make([]Score, 0, len(rows))
	for _, r := range rows {
		out = append(out, Score{
			ID:         r.ID,
			Score:      clampScore(r.Score),
			Confidence: clampConfidence(r.Confidence),
			Reason:     strings.TrimSpace(r.Reason),
		})
	}
	return out
}

func stripMarkdownFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		// Drop up to the first newline (handles ``` and ```json).
		if idx := strings.Index(s, "\n"); idx >= 0 {
			s = s[idx+1:]
		}
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
	}
	return strings.TrimSpace(s)
}
