package sentiment

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/llm"
)

// KeywordScorer flags positive headlines as positive and negative
// headlines as negative.
func TestKeywordScorerCatchesBullsAndBears(t *testing.T) {
	scorer := &KeywordScorer{}
	items := []Item{
		{ID: "1", Title: "Apple beats Q2 earnings, raised forecast", Symbols: []string{"AAPL"}},
		{ID: "2", Title: "Tesla recall halts production, lawsuit filed", Symbols: []string{"TSLA"}},
		{ID: "3", Title: "茅台股价突破历史新高，回购计划公布", Symbols: []string{"600519"}},
		{ID: "4", Title: "比亚迪因质量问题被调查，股价暴跌", Symbols: []string{"002594"}},
		{ID: "5", Title: "Apple announces new feature", Symbols: []string{"AAPL"}},
	}
	scores, err := scorer.Score(context.Background(), items)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if len(scores) != 5 {
		t.Fatalf("expected 5 scores, got %d", len(scores))
	}
	if scores[0].Score <= 0 {
		t.Errorf("AAPL beat should be positive: %+v", scores[0])
	}
	if scores[1].Score >= 0 {
		t.Errorf("TSLA recall should be negative: %+v", scores[1])
	}
	if scores[2].Score <= 0 {
		t.Errorf("Maotai 新高 should be positive: %+v", scores[2])
	}
	if scores[3].Score >= 0 {
		t.Errorf("BYD investigation should be negative: %+v", scores[3])
	}
	// Neutral item should have low confidence.
	if scores[4].Confidence > 0.2 {
		t.Errorf("neutral item confidence too high: %+v", scores[4])
	}
}

// KeywordScorer tanh-normalises so multi-hit headlines saturate
// rather than blow past 1.0.
func TestKeywordScorerSaturates(t *testing.T) {
	scorer := &KeywordScorer{}
	items := []Item{
		{ID: "loud", Title: "beat exceeded outperform surge soar rally upgrade record high buyback"},
	}
	scores, _ := scorer.Score(context.Background(), items)
	if scores[0].Score < 0.95 || scores[0].Score > 1.01 {
		t.Errorf("score should saturate near 1.0, got %v", scores[0].Score)
	}
}

// KeywordScorer custom lexicons override the defaults.
func TestKeywordScorerCustomLexicon(t *testing.T) {
	scorer := &KeywordScorer{
		PositiveTerms: []string{"moonshot", "to-da-moon"},
		NegativeTerms: []string{"rugpull"},
	}
	items := []Item{
		{ID: "1", Title: "Project announces moonshot acquisition"},
		{ID: "2", Title: "Token rugpull rocks DeFi community"},
	}
	scores, _ := scorer.Score(context.Background(), items)
	if scores[0].Score <= 0 {
		t.Errorf("custom positive failed: %+v", scores[0])
	}
	if scores[1].Score >= 0 {
		t.Errorf("custom negative failed: %+v", scores[1])
	}
}

// fakeLLM lets us drive the LLMScorer through canned JSON.
type fakeLLM struct {
	resp string
	err  error
	req  llm.ChatRequest
}

func (f *fakeLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	f.req = req
	if f.err != nil {
		return nil, f.err
	}
	return &llm.ChatResponse{Content: f.resp}, nil
}
func (f *fakeLLM) ListModels(_ context.Context) ([]llm.ModelInfo, error) { return nil, nil }

// LLMScorer parses the {"scores":[ ... ]} response shape.
func TestLLMScorerParsesScoresShape(t *testing.T) {
	llmFake := &fakeLLM{resp: `{"scores":[
		{"id":"1","score":0.65,"confidence":0.8,"reason":"earnings beat"},
		{"id":"2","score":-0.55,"confidence":0.7,"reason":"recall pressure"}
	]}`}
	scorer := &LLMScorer{Client: llmFake}
	items := []Item{
		{ID: "1", Title: "Apple beats Q2", Symbols: []string{"AAPL"}},
		{ID: "2", Title: "Tesla recall", Symbols: []string{"TSLA"}},
	}
	got, err := scorer.Score(context.Background(), items)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 scores, got %d", len(got))
	}
	if got[0].Score != 0.65 || got[0].Reason != "earnings beat" {
		t.Errorf("AAPL score wrong: %+v", got[0])
	}
	if got[1].Score != -0.55 {
		t.Errorf("TSLA score wrong: %+v", got[1])
	}
}

// LLMScorer parses bare-array response (some models drop the
// wrapping object despite the system prompt).
func TestLLMScorerParsesBareArrayShape(t *testing.T) {
	llmFake := &fakeLLM{resp: `[
		{"id":"x","score":0.4,"confidence":0.6}
	]`}
	scorer := &LLMScorer{Client: llmFake}
	items := []Item{{ID: "x", Title: "Something"}}
	got, err := scorer.Score(context.Background(), items)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if got[0].Score != 0.4 {
		t.Errorf("expected 0.4, got %+v", got[0])
	}
}

// LLMScorer strips markdown fences the model occasionally emits.
func TestLLMScorerStripsMarkdownFences(t *testing.T) {
	llmFake := &fakeLLM{resp: "```json\n{\"scores\":[{\"id\":\"a\",\"score\":0.2,\"confidence\":0.5}]}\n```"}
	scorer := &LLMScorer{Client: llmFake}
	items := []Item{{ID: "a", Title: "n"}}
	got, err := scorer.Score(context.Background(), items)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if got[0].Score != 0.2 {
		t.Errorf("expected 0.2 after stripping fences, got %+v", got[0])
	}
}

// LLMScorer clamps wild output to -1..+1.
func TestLLMScorerClampsScores(t *testing.T) {
	llmFake := &fakeLLM{resp: `{"scores":[{"id":"a","score":3.5,"confidence":2.0}]}`}
	scorer := &LLMScorer{Client: llmFake}
	items := []Item{{ID: "a", Title: "n"}}
	got, err := scorer.Score(context.Background(), items)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if got[0].Score != 1.0 || got[0].Confidence != 1.0 {
		t.Errorf("expected clamping to 1.0, got %+v", got[0])
	}
}

// LLMScorer with no client returns an error (the composite scorer
// is responsible for fallback policy).
func TestLLMScorerWithoutClient(t *testing.T) {
	scorer := &LLMScorer{}
	_, err := scorer.Score(context.Background(), []Item{{ID: "a"}})
	if err == nil {
		t.Errorf("expected error when client unset")
	}
}

// LLMScorer with chat error propagates the error.
func TestLLMScorerChatError(t *testing.T) {
	scorer := &LLMScorer{Client: &fakeLLM{err: errors.New("upstream 500")}}
	_, err := scorer.Score(context.Background(), []Item{{ID: "a", Title: "n"}})
	if err == nil {
		t.Errorf("expected error")
	}
}

// CompositeScorer falls back to keyword on LLM error.
func TestCompositeScorerFallsBackOnLLMError(t *testing.T) {
	primary := &LLMScorer{Client: &fakeLLM{err: errors.New("rate limit")}}
	composite := &CompositeScorer{Primary: primary, Fallback: &KeywordScorer{}}
	items := []Item{{ID: "1", Title: "Apple beats Q2 earnings"}}
	got, err := composite.Score(context.Background(), items)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if got[0].Score <= 0 {
		t.Errorf("fallback should still find positive sentiment: %+v", got[0])
	}
}

// CompositeScorer uses primary when it succeeds.
func TestCompositeScorerPrefersPrimary(t *testing.T) {
	primary := &LLMScorer{Client: &fakeLLM{resp: `{"scores":[{"id":"1","score":0.7,"confidence":0.9,"reason":"LLM"}]}`}}
	composite := &CompositeScorer{Primary: primary, Fallback: &KeywordScorer{}}
	got, err := composite.Score(context.Background(), []Item{{ID: "1", Title: "test"}})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if got[0].Reason != "LLM" {
		t.Errorf("expected primary result, got %+v", got[0])
	}
}

// AggregateBySymbol weights by confidence and produces a MARKET
// roll-up plus per-symbol buckets.
func TestAggregateBySymbolProducesMarketAndSymbolBuckets(t *testing.T) {
	items := []Item{
		{ID: "1", Title: "Apple beats earnings", Symbols: []string{"AAPL"}},
		{ID: "2", Title: "Tesla recall pressure", Symbols: []string{"TSLA"}},
		{ID: "3", Title: "Apple guidance raised", Symbols: []string{"AAPL"}},
	}
	scores := []Score{
		{ID: "1", Score: 0.6, Confidence: 0.8, Reason: "beat"},
		{ID: "2", Score: -0.5, Confidence: 0.7, Reason: "recall"},
		{ID: "3", Score: 0.5, Confidence: 0.8, Reason: "guidance"},
	}
	aggs := AggregateBySymbol(scores, items)
	// Find MARKET / AAPL / TSLA buckets.
	byScope := make(map[string]AggregateScore, len(aggs))
	for _, a := range aggs {
		byScope[a.Scope] = a
	}
	market, ok := byScope["MARKET"]
	if !ok {
		t.Fatalf("MARKET bucket missing: %+v", aggs)
	}
	if market.Count != 3 {
		t.Errorf("MARKET should cover all 3 items, got %d", market.Count)
	}
	aapl, ok := byScope["AAPL"]
	if !ok {
		t.Fatalf("AAPL bucket missing")
	}
	if aapl.Average <= 0 {
		t.Errorf("AAPL avg should be bullish: %+v", aapl)
	}
	if aapl.Polarity != "bullish" && aapl.Polarity != "strongly bullish" {
		t.Errorf("AAPL polarity should be bullish-ish, got %q", aapl.Polarity)
	}
	tsla, ok := byScope["TSLA"]
	if !ok {
		t.Fatalf("TSLA bucket missing")
	}
	if tsla.Average >= 0 {
		t.Errorf("TSLA avg should be bearish: %+v", tsla)
	}
	if len(aapl.Reasons) == 0 {
		t.Errorf("AAPL should have reason strings: %+v", aapl)
	}
}

// AggregateBySymbol orders the MARKET bucket first.
func TestAggregateBySymbolMarketFirst(t *testing.T) {
	items := []Item{{ID: "1", Title: "x", Symbols: []string{"X"}}}
	scores := []Score{{ID: "1", Score: 0.5, Confidence: 0.5}}
	aggs := AggregateBySymbol(scores, items)
	if len(aggs) < 1 || aggs[0].Scope != "MARKET" {
		t.Errorf("first scope should be MARKET, got %+v", aggs)
	}
}

// FormatForPrompt produces a readable block.
func TestFormatForPromptHasMarketLineAndSymbolDetail(t *testing.T) {
	items := []Item{
		{ID: "1", Title: "Apple beats earnings", Symbols: []string{"AAPL"}, PublishedAt: time.Now()},
	}
	scores := []Score{{ID: "1", Score: 0.6, Confidence: 0.8, Reason: "beat"}}
	aggs := AggregateBySymbol(scores, items)
	out := FormatForPrompt(aggs, len(items))
	if !strings.Contains(out, "News sentiment (1 items)") {
		t.Errorf("missing market line: %s", out)
	}
	if !strings.Contains(out, "AAPL") || !strings.Contains(out, "bullish") {
		t.Errorf("missing AAPL bullish detail: %s", out)
	}
}

// FormatForPrompt returns empty for nil / zero aggregates.
func TestFormatForPromptEmpty(t *testing.T) {
	if FormatForPrompt(nil, 0) != "" {
		t.Errorf("expected empty for zero aggregates")
	}
	if FormatForPrompt([]AggregateScore{{Scope: "MARKET"}}, 0) != "" {
		t.Errorf("expected empty when totalItems is 0")
	}
}

// polarityLabel sanity check across the score range.
func TestPolarityLabelBoundaries(t *testing.T) {
	cases := map[float64]string{
		-0.8: "strongly bearish",
		-0.4: "bearish",
		0:    "neutral",
		0.4:  "bullish",
		0.8:  "strongly bullish",
	}
	for score, want := range cases {
		if got := polarityLabel(score); got != want {
			t.Errorf("polarityLabel(%v) = %q, want %q", score, got, want)
		}
	}
}
