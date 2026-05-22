package decision

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/llm"
)

// fakeLLM lets tests record what the engine sent to the LLM and
// inject the canned response. Captures the *last* request so
// assertions can grep the prompt for stable substrings.
type fakeLLM struct {
	lastReq    llm.ChatRequest
	respond    func(req llm.ChatRequest) (*llm.ChatResponse, error)
	listModels func(ctx context.Context) ([]llm.ModelInfo, error)
}

func (f *fakeLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	f.lastReq = req
	if f.respond == nil {
		return &llm.ChatResponse{Content: "{}"}, nil
	}
	return f.respond(req)
}

func (f *fakeLLM) ListModels(ctx context.Context) ([]llm.ModelInfo, error) {
	if f.listModels == nil {
		return nil, nil
	}
	return f.listModels(ctx)
}

// Happy path: LLM returns a well-formed JSON object with one buy and
// one hold; the engine parses both into DecisionAction.
func TestLLMDecisionEngineParsesWellFormedResponse(t *testing.T) {
	fl := &fakeLLM{
		respond: func(_ llm.ChatRequest) (*llm.ChatResponse, error) {
			return &llm.ChatResponse{Content: `{
				"stance": "net long, tactical",
				"confidence": 0.78,
				"actions": [
					{"symbol":"AAPL","action":"buy","qtyPct":0.05,"reasoning":"earnings momentum","confidence":0.82},
					{"symbol":"NVDA","action":"hold","qtyPct":0,"reasoning":"position size already at cap","confidence":0.6}
				]
			}`}, nil
		},
	}
	engine := &LLMDecisionEngine{Client: fl, ModelTier: llm.TierCritical, UserID: "u1", FundID: "f1"}
	out, err := engine.Decide(context.Background(), DecisionInput{
		FundID:      "f1",
		TradingDate: time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC),
		Universe:    []string{"AAPL", "NVDA"},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if out.Confidence < 0.77 || out.Confidence > 0.79 {
		t.Errorf("confidence = %v, want ~0.78", out.Confidence)
	}
	if len(out.Actions) != 2 {
		t.Fatalf("len(actions) = %d, want 2", len(out.Actions))
	}
	if out.Actions[0].Symbol != "AAPL" || out.Actions[0].Action != "buy" {
		t.Errorf("first action mismatch: %+v", out.Actions[0])
	}
}

// Defensive parsing: tolerate markdown fences and surrounding prose
// that some models emit despite the system prompt forbidding it.
func TestLLMDecisionEngineStripsMarkdownFencesAndProse(t *testing.T) {
	fl := &fakeLLM{
		respond: func(_ llm.ChatRequest) (*llm.ChatResponse, error) {
			return &llm.ChatResponse{Content: "Sure, here's the plan:\n\n```json\n{\"stance\":\"flat\",\"confidence\":0.5,\"actions\":[{\"symbol\":\"SPY\",\"action\":\"watch\",\"qtyPct\":0,\"reasoning\":\"unclear\",\"confidence\":0.5}]}\n```\n\nLet me know!"}, nil
		},
	}
	engine := &LLMDecisionEngine{Client: fl}
	out, err := engine.Decide(context.Background(), DecisionInput{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(out.Actions) != 1 || out.Actions[0].Action != "watch" {
		t.Errorf("expected single watch action, got %+v", out.Actions)
	}
}

// Unknown / mistyped actions are dropped silently rather than
// failing the whole plan.
func TestLLMDecisionEngineSkipsUnknownActions(t *testing.T) {
	fl := &fakeLLM{
		respond: func(_ llm.ChatRequest) (*llm.ChatResponse, error) {
			return &llm.ChatResponse{Content: `{"actions":[
				{"symbol":"AAPL","action":"yolo","qtyPct":0.5,"reasoning":"vibes","confidence":0.9},
				{"symbol":"NVDA","action":"buy","qtyPct":0.05,"reasoning":"ok","confidence":0.7}
			]}`}, nil
		},
	}
	engine := &LLMDecisionEngine{Client: fl}
	out, err := engine.Decide(context.Background(), DecisionInput{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(out.Actions) != 1 || out.Actions[0].Symbol != "NVDA" {
		t.Errorf("expected unknown action to be dropped, got %+v", out.Actions)
	}
}

// Network / provider errors propagate so the wiring layer can fall
// back to the deterministic engine.
func TestLLMDecisionEnginePropagatesLLMError(t *testing.T) {
	wanted := errors.New("provider rate-limited")
	fl := &fakeLLM{respond: func(_ llm.ChatRequest) (*llm.ChatResponse, error) { return nil, wanted }}
	engine := &LLMDecisionEngine{Client: fl}
	if _, err := engine.Decide(context.Background(), DecisionInput{}); !errors.Is(err, wanted) {
		t.Errorf("expected wrapped wanted error, got %v", err)
	}
}

// Empty output (no actions, no stance) surfaces as ErrEmptyDecision
// so the caller knows the model "decided nothing" and can substitute
// the fallback engine instead of writing a useless empty plan.
func TestLLMDecisionEngineFlagsEmptyOutput(t *testing.T) {
	fl := &fakeLLM{
		respond: func(_ llm.ChatRequest) (*llm.ChatResponse, error) {
			return &llm.ChatResponse{Content: `{"actions":[],"confidence":0.3,"stance":""}`}, nil
		},
	}
	engine := &LLMDecisionEngine{Client: fl}
	out, err := engine.Decide(context.Background(), DecisionInput{})
	if !errors.Is(err, ErrEmptyDecision) {
		t.Errorf("expected ErrEmptyDecision, got %v (out=%+v)", err, out)
	}
}

// The user prompt must include the inputs verbatim so the LLM has
// the data to reason about. We grep for one Chinese phrase and one
// English numeric so both locales are covered.
func TestLLMDecisionEngineInjectsInputsIntoUserPrompt(t *testing.T) {
	fl := &fakeLLM{
		respond: func(_ llm.ChatRequest) (*llm.ChatResponse, error) {
			return &llm.ChatResponse{Content: `{"actions":[],"stance":"watch","confidence":0.5}`}, nil
		},
	}
	engine := &LLMDecisionEngine{Client: fl}
	_, _ = engine.Decide(context.Background(), DecisionInput{
		FundID:              "fund-xyz",
		TradingDate:         time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC),
		Universe:            []string{"AAPL"},
		RoundtableConsensus: []string{"新能源主线延续"},
		TotalAssets:         1_000_000,
	})
	user := fl.lastReq.Messages[1].Content
	for _, want := range []string{"fund-xyz", "2026-05-20", "AAPL", "新能源主线延续", "1000000"} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing %q\n--- prompt ---\n%s", want, user)
		}
	}
	// system prompt must include the schema directives
	sys := fl.lastReq.Messages[0].Content
	for _, want := range []string{"qtyPct", "watch", "Hard constraints"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}

// Phase 3A-7: when the wiring layer populates SleeveScorecard the
// user prompt MUST surface it under the documented JSON key so the
// LLM can find it, and the system prompt MUST explain how to use it.
func TestLLMDecisionEngineInjectsSleeveScorecard(t *testing.T) {
	fl := &fakeLLM{
		respond: func(_ llm.ChatRequest) (*llm.ChatResponse, error) {
			return &llm.ChatResponse{Content: `{"actions":[],"stance":"watch","confidence":0.5}`}, nil
		},
	}
	engine := &LLMDecisionEngine{Client: fl}
	scorecard := "Strategy scorecard (last 30 days, closed roundtrips only):\n" +
		"Winners ...:\n" +
		"  - sleeve=trend regime=trend_up n=12 win_rate=75% total_pnl=$+1850.20 avg_pnl=+5.20%\n" +
		"Losers ...:\n" +
		"  - sleeve=mean_reversion regime=chop n=7 win_rate=22% total_pnl=$-820.10 avg_pnl=-4.30%"
	_, _ = engine.Decide(context.Background(), DecisionInput{
		FundID:          "fund-xyz",
		TradingDate:     time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC),
		Universe:        []string{"AAPL"},
		SleeveScorecard: scorecard,
	})
	user := fl.lastReq.Messages[1].Content
	if !strings.Contains(user, "sleeveScorecard") {
		t.Errorf("user prompt missing the sleeveScorecard JSON key:\n%s", user)
	}
	if !strings.Contains(user, "trend regime=trend_up") {
		t.Errorf("user prompt missing scorecard contents:\n%s", user)
	}
	// System prompt teaches the LLM how to consume the scorecard.
	sys := fl.lastReq.Messages[0].Content
	for _, want := range []string{"sleeveScorecard", "Winners", "Losers", "soft prior"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}

// When SleeveScorecard is empty the JSON key must be omitted so the
// LLM doesn't see a phantom "no data" section and anchor on it.
func TestLLMDecisionEngineOmitsScorecardWhenEmpty(t *testing.T) {
	fl := &fakeLLM{
		respond: func(_ llm.ChatRequest) (*llm.ChatResponse, error) {
			return &llm.ChatResponse{Content: `{"actions":[],"stance":"watch","confidence":0.5}`}, nil
		},
	}
	engine := &LLMDecisionEngine{Client: fl}
	_, _ = engine.Decide(context.Background(), DecisionInput{
		FundID:          "fund-xyz",
		TradingDate:     time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC),
		Universe:        []string{"AAPL"},
		SleeveScorecard: "",
	})
	user := fl.lastReq.Messages[1].Content
	if strings.Contains(user, "sleeveScorecard") {
		t.Errorf("user prompt must omit sleeveScorecard key when input empty:\n%s", user)
	}
}

// Phase 3A-10: lesson replay round-trip — the user prompt should
// carry the lessonReplay block under its JSON key and the system
// prompt should teach the LLM how to consume it.
func TestLLMDecisionEngineInjectsLessonReplay(t *testing.T) {
	fl := &fakeLLM{
		respond: func(_ llm.ChatRequest) (*llm.ChatResponse, error) {
			return &llm.ChatResponse{Content: `{"actions":[],"stance":"watch","confidence":0.5}`}, nil
		},
	}
	engine := &LLMDecisionEngine{Client: fl}
	replay := "Recent attribution lessons (last 14 days, most-severe first):\n" +
		"  - CRITICAL [mean_reversion × chop] Sleeve mean_reversion is losing money in regime chop\n" +
		"      Across 12 closed lots in regime chop, the mean_reversion sleeve recorded a 22% win rate."
	_, _ = engine.Decide(context.Background(), DecisionInput{
		FundID:       "fund-xyz",
		TradingDate:  time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC),
		Universe:     []string{"AAPL"},
		LessonReplay: replay,
	})
	user := fl.lastReq.Messages[1].Content
	if !strings.Contains(user, "lessonReplay") {
		t.Errorf("user prompt missing lessonReplay JSON key:\n%s", user)
	}
	if !strings.Contains(user, "CRITICAL [mean_reversion") {
		t.Errorf("user prompt missing replay contents:\n%s", user)
	}
	sys := fl.lastReq.Messages[0].Content
	for _, want := range []string{"lessonReplay", "CRITICAL", "lesson replay"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}

// When LessonReplay is empty the JSON key must be omitted.
func TestLLMDecisionEngineOmitsLessonReplayWhenEmpty(t *testing.T) {
	fl := &fakeLLM{
		respond: func(_ llm.ChatRequest) (*llm.ChatResponse, error) {
			return &llm.ChatResponse{Content: `{"actions":[],"stance":"watch","confidence":0.5}`}, nil
		},
	}
	engine := &LLMDecisionEngine{Client: fl}
	_, _ = engine.Decide(context.Background(), DecisionInput{
		FundID:       "fund-xyz",
		TradingDate:  time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC),
		Universe:     []string{"AAPL"},
		LessonReplay: "",
	})
	user := fl.lastReq.Messages[1].Content
	if strings.Contains(user, "lessonReplay") {
		t.Errorf("user prompt must omit lessonReplay key when input empty:\n%s", user)
	}
}

// Fallback engine: with held positions, reduce the first sellable
// one and hold the rest.
func TestFallbackEngineWithPositionsReducesFirstSellable(t *testing.T) {
	out, err := FallbackEngine{}.Decide(context.Background(), DecisionInput{
		Positions: []DecisionPosition{
			{Symbol: "AAPL", Quantity: 100, AvailableQty: 100},
			{Symbol: "NVDA", Quantity: 200, AvailableQty: 200},
		},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(out.Actions) != 2 {
		t.Fatalf("len(actions) = %d, want 2", len(out.Actions))
	}
	if out.Actions[0].Action != "reduce" || out.Actions[0].Symbol != "AAPL" {
		t.Errorf("first action should be reduce AAPL, got %+v", out.Actions[0])
	}
	if out.Actions[1].Action != "hold" {
		t.Errorf("second action should be hold, got %+v", out.Actions[1])
	}
}

// Fallback engine: locked-out first holding falls through to the
// next sellable. Crucial for T+1 days.
func TestFallbackEngineSkipsT1LockedPositions(t *testing.T) {
	out, _ := FallbackEngine{}.Decide(context.Background(), DecisionInput{
		Positions: []DecisionPosition{
			{Symbol: "600519", Quantity: 100, AvailableQty: 0}, // T+1 locked
			{Symbol: "601318", Quantity: 200, AvailableQty: 200},
		},
	})
	if out.Actions[0].Action != "hold" {
		t.Errorf("locked holding should be hold, got %+v", out.Actions[0])
	}
	if out.Actions[1].Action != "reduce" || out.Actions[1].Symbol != "601318" {
		t.Errorf("second (sellable) holding should reduce, got %+v", out.Actions[1])
	}
}

// Fallback engine: no positions and no universe yields a single
// "watch" action so the workflow still completes.
func TestFallbackEngineWithoutUniverseEmitsWatch(t *testing.T) {
	out, _ := FallbackEngine{}.Decide(context.Background(), DecisionInput{})
	if len(out.Actions) != 1 || out.Actions[0].Action != "watch" {
		t.Errorf("expected single watch action, got %+v", out.Actions)
	}
}

// Fallback engine: positions absent + universe present → first
// universe symbol becomes a small buy.
func TestFallbackEngineBuysFirstUniverseSymbol(t *testing.T) {
	out, _ := FallbackEngine{}.Decide(context.Background(), DecisionInput{
		Universe:    []string{"SPY", "QQQ"},
		TotalAssets: 100_000,
		BuyBudget:   5_000,
	})
	if len(out.Actions) != 1 || out.Actions[0].Action != "buy" || out.Actions[0].Symbol != "SPY" {
		t.Errorf("expected buy SPY, got %+v", out.Actions)
	}
	if out.Actions[0].QtyPct != 0.05 {
		t.Errorf("expected 5%% NAV, got %v", out.Actions[0].QtyPct)
	}
}

// Regression guard: reasoning-tier providers (Gemini 3.x Pro Preview,
// OpenAI o-series, Claude extended thinking) burn an internal
// "thoughts" budget *out of* the same MaxOutputTokens cap before
// emitting a single visible token. On a real PM prompt the storage
// fund observed ~600-3000 thoughtTokens; at the old 1500-token cap
// the response was either {} (FinishReason=MAX_TOKENS) or a
// truncated "{stance:..." that failed JSON parse and forced the
// legacy fallback. The Engine.maxTokens default must stay >= 8000 so
// every realistic PM call has room for both the reasoning pass and
// the JSON body. Lowering the default again will silently break the
// LLM decision path; bumping it deliberately is fine.
func TestLLMDecisionEngineMaxTokensDefaultLeavesRoomForThinkingModels(t *testing.T) {
	const minSafe = 8000
	got := (&LLMDecisionEngine{}).maxTokens()
	if got < minSafe {
		t.Fatalf("LLMDecisionEngine.maxTokens() default = %d, want >= %d so reasoning-tier models have budget for thoughts + JSON output (see agent-transcript dce9e865 2026-05-22)", got, minSafe)
	}

	// And an explicit override (>0) must always win — we shouldn't
	// quietly floor every caller to the default just because they
	// asked for a smaller cap (some tests / cheap models legitimately
	// want a small reply).
	custom := (&LLMDecisionEngine{MaxTokens: 200}).maxTokens()
	if custom != 200 {
		t.Errorf("LLMDecisionEngine{MaxTokens:200}.maxTokens() = %d, want 200 — explicit override must win", custom)
	}
}
