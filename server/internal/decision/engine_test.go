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
// TestLLMDecisionEnginePrefersInputRoutingHintsOverEngineDefaults pins
// down the P2 fix: the wiring layer builds one LLMDecisionEngine per
// fund at workflow-construction time and at that point the PM agent
// for the trading day hasn't been resolved yet. Threading UserID and
// PMAgentID through DecisionInput is what lets ModelRouter's
// agentDefaults lookup fire — otherwise every PM call routes to the
// platform default, which was tong's symptom (claude configured,
// gemini used). If the input fields are empty the engine's static
// values are still used (legacy callers / tests).
func TestLLMDecisionEnginePrefersInputRoutingHintsOverEngineDefaults(t *testing.T) {
	fl := &fakeLLM{respond: func(_ llm.ChatRequest) (*llm.ChatResponse, error) {
		return &llm.ChatResponse{Content: `{"stance":"net long","confidence":0.7,"actions":[]}`}, nil
	}}
	engine := &LLMDecisionEngine{
		Client:    fl,
		ModelTier: llm.TierCritical,
		UserID:    "engine-default-user",
		AgentID:   "engine-default-agent",
	}

	if _, err := engine.Decide(context.Background(), DecisionInput{
		FundID:      "f1",
		TradingDate: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
		UserID:      "input-user",
		PMAgentID:   "input-pm-agent",
	}); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if fl.lastReq.UserID != "input-user" {
		t.Errorf("UserID = %q, want input-user", fl.lastReq.UserID)
	}
	if fl.lastReq.AgentID != "input-pm-agent" {
		t.Errorf("AgentID = %q, want input-pm-agent", fl.lastReq.AgentID)
	}

	// Now verify the legacy fallback: when input hints are empty the
	// engine's static fields still flow through.
	if _, err := engine.Decide(context.Background(), DecisionInput{
		FundID:      "f1",
		TradingDate: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Decide (legacy): %v", err)
	}
	if fl.lastReq.UserID != "engine-default-user" {
		t.Errorf("legacy UserID = %q, want engine-default-user", fl.lastReq.UserID)
	}
	if fl.lastReq.AgentID != "engine-default-agent" {
		t.Errorf("legacy AgentID = %q, want engine-default-agent", fl.lastReq.AgentID)
	}
}

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

// Sprint A #1: the system prompt must instruct the PM how to read
// the quantSnapshots block. Pin the exact section markers so a
// future prompt refactor that drops the regime/ATR rules fails this
// test loudly rather than silently shipping a model that ignores
// the new position-size ceiling. The markers are quoted from the
// prompt itself.
func TestSystemPromptDocumentsQuantSnapshotRules(t *testing.T) {
	prompt := systemPrompt()
	required := []string{
		"input.quantSnapshots",
		"positionSizeCeilingPct",
		"regime is \"chop\"",
		"regime is \"trend_down\"",
		"regime is \"trend_up\"",
		"regime is \"range\"",
	}
	for _, frag := range required {
		if !strings.Contains(prompt, frag) {
			t.Errorf("systemPrompt() missing %q — the regime + ATR rule block has regressed", frag)
		}
	}
}

// User prompt must serialise QuantSnapshots under the documented
// JSON key with the same field shapes the system prompt references.
// Skipped Snapshots (HasSignal == false) are NOT included; this is
// what keeps the prompt clean on funds whose OHLC pipeline is half-
// wired.
func TestUserPromptIncludesQuantSnapshotsBlock(t *testing.T) {
	prompt := userPrompt(DecisionInput{
		FundID:      "f1",
		TradingDate: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
		Universe:    []string{"AAPL", "TSLA", "NEW"},
		QuantSnapshots: []SymbolQuantSnapshot{
			{Symbol: "AAPL", Regime: "trend_up", Close: 187.5, ATR14: 2.6, ATRPct: 1.3866, PositionSizeCeilingPct: 0.08},
			{Symbol: "TSLA", Regime: "chop", Close: 240.2, ATR14: 11.5, ATRPct: 4.7877, PositionSizeCeilingPct: 0.0261},
			{Symbol: "NEW"}, // no signal — must be dropped
		},
	})
	if !strings.Contains(prompt, `"quantSnapshots"`) {
		t.Errorf("user prompt missing quantSnapshots key:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"regime": "trend_up"`) {
		t.Errorf("AAPL trend_up regime not in prompt")
	}
	if !strings.Contains(prompt, `"regime": "chop"`) {
		t.Errorf("TSLA chop regime not in prompt")
	}
	if !strings.Contains(prompt, `"positionSizeCeilingPct": 0.08`) {
		t.Errorf("AAPL ceiling not surfaced verbatim:\n%s", prompt)
	}
	// The bare-Symbol NEW row must be filtered out — that's the
	// no-signal contract that keeps the prompt from bloating.
	if strings.Contains(prompt, `"symbol": "NEW"`) {
		t.Errorf("bare-Symbol NEW row leaked into prompt:\n%s", prompt)
	}
}

// When QuantSnapshots is empty / nil the prompt must omit the key
// entirely so legacy deployments (OHLC fetcher unwired) don't see a
// dangling empty array the LLM has to special-case.
func TestUserPromptOmitsQuantSnapshotsWhenEmpty(t *testing.T) {
	prompt := userPrompt(DecisionInput{
		FundID:      "f1",
		TradingDate: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
		Universe:    []string{"AAPL"},
	})
	if strings.Contains(prompt, `"quantSnapshots"`) {
		t.Errorf("empty QuantSnapshots should not appear in prompt:\n%s", prompt)
	}
}

// buildQuantSnapshotPromptItems is the function the prompt uses to
// drop no-signal rows. Locking its contract here makes the rounding
// + drop behaviour debuggable without rendering the whole prompt.
func TestBuildQuantSnapshotPromptItemsDropsNoSignalAndRounds(t *testing.T) {
	got := buildQuantSnapshotPromptItems([]SymbolQuantSnapshot{
		{Symbol: "A", Regime: "trend_up", Close: 100.0000004, ATR14: 1.500000003, ATRPct: 1.50000045, PositionSizeCeilingPct: 0.0833333333},
		{Symbol: "B"}, // dropped
	})
	if len(got) != 1 {
		t.Fatalf("expected 1 surviving row, got %d (%+v)", len(got), got)
	}
	if got[0].Symbol != "A" {
		t.Errorf("expected A, got %q", got[0].Symbol)
	}
	// All four numeric fields rounded to 6dp (0.0833333333 → 0.083333).
	if got[0].PositionSizeCeilingPct != 0.083333 {
		t.Errorf("ceiling not rounded to 6dp: got %v", got[0].PositionSizeCeilingPct)
	}
}

// Sprint A #2: the system prompt must teach the PM how to read the
// universeRanking block — same loud-fail-on-regression pattern as
// the quantSnapshot rule test above.
func TestSystemPromptDocumentsUniverseRankingRules(t *testing.T) {
	prompt := systemPrompt()
	required := []string{
		"input.universeRanking",
		"compositeZ",
		"Q1",
		"Q4",
		"liquidityZ",
	}
	for _, frag := range required {
		if !strings.Contains(prompt, frag) {
			t.Errorf("systemPrompt() missing %q — the universeRanking rule has regressed", frag)
		}
	}
}

// The user prompt must serialise UniverseRanking under the documented
// JSON key with the fields the system prompt references.
func TestUserPromptIncludesUniverseRankingBlock(t *testing.T) {
	prompt := userPrompt(DecisionInput{
		FundID:      "f1",
		TradingDate: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
		Universe:    []string{"STRONG", "MID", "WEAK"},
		UniverseRanking: []SymbolRanking{
			{Symbol: "STRONG", MomentumZ: 1.23, VolatilityZ: -0.4, LiquidityZ: 0.8, CompositeZ: 0.91, Quartile: 1},
			{Symbol: "MID", MomentumZ: 0, VolatilityZ: 0, LiquidityZ: 0, CompositeZ: 0, Quartile: 2},
			{Symbol: "WEAK", MomentumZ: -1.23, VolatilityZ: 0.4, LiquidityZ: -0.8, CompositeZ: -0.91, Quartile: 4},
		},
	})
	if !strings.Contains(prompt, `"universeRanking"`) {
		t.Errorf("user prompt missing universeRanking key:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"compositeZ": 0.91`) {
		t.Errorf("STRONG compositeZ not surfaced:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"quartile": 1`) || !strings.Contains(prompt, `"quartile": 4`) {
		t.Errorf("quartile labels missing:\n%s", prompt)
	}
	// Z-scores get round-to-4dp; check the negative case for the
	// signed-rounder path.
	if !strings.Contains(prompt, `"momentumZ": -1.23`) {
		t.Errorf("negative MomentumZ not preserved with sign:\n%s", prompt)
	}
}

// Empty / nil UniverseRanking → no key in prompt.
func TestUserPromptOmitsUniverseRankingWhenEmpty(t *testing.T) {
	prompt := userPrompt(DecisionInput{
		FundID:      "f1",
		TradingDate: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
		Universe:    []string{"AAPL"},
	})
	if strings.Contains(prompt, `"universeRanking"`) {
		t.Errorf("empty UniverseRanking should not appear in prompt:\n%s", prompt)
	}
}

// round4Signed handles the negative-number rounding path the
// universeRanking serialiser relies on. Locking it here so the
// signed truncation never regresses to a positive-only rounder
// that would push -1.234 to -1.2339 etc.
func TestRound4SignedHandlesBothSigns(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{0.123456, 0.1235},
		{-0.123456, -0.1235},
		{0, 0},
		{1.99999, 2.0},
		{-1.99999, -2.0},
	}
	for _, c := range cases {
		got := round4Signed(c.in)
		if got != c.want {
			t.Errorf("round4Signed(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// Sprint B #1: the system prompt must teach the PM the cooldown
// veto rules. We assert on multiple stable substrings so a future
// edit that loses one of the rules surfaces as a regression
// rather than silently shipping a weaker prompt.
func TestSystemPromptDocumentsCooldownRules(t *testing.T) {
	prompt := systemPrompt()
	required := []string{
		"input.cooldowns",
		"hoursRemaining",
		"lastFillSide",
		"watch",
		"extreme catalyst",
	}
	for _, frag := range required {
		if !strings.Contains(prompt, frag) {
			t.Errorf("systemPrompt() missing %q — the cooldown rule has regressed", frag)
		}
	}
}

// The user prompt must serialise Cooldowns under the documented
// JSON key with the fields the system prompt references.
func TestUserPromptIncludesCooldownsBlock(t *testing.T) {
	fillAt := time.Date(2026, 5, 24, 4, 0, 0, 0, time.UTC)
	blockedUntil := fillAt.Add(24 * time.Hour)
	prompt := userPrompt(DecisionInput{
		FundID:      "f1",
		TradingDate: time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
		Universe:    []string{"AAPL"},
		Cooldowns: []SymbolCooldown{
			{
				Symbol:         "AAPL",
				LastFillSide:   "buy",
				LastFillAt:     fillAt,
				BlockedUntil:   blockedUntil,
				HoursSinceFill: 8.0,
				HoursRemaining: 16.0,
			},
		},
	})
	if !strings.Contains(prompt, `"cooldowns"`) {
		t.Errorf("user prompt missing cooldowns key:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"symbol": "AAPL"`) {
		t.Errorf("AAPL symbol not surfaced under cooldowns:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"lastFillSide": "buy"`) {
		t.Errorf("lastFillSide missing under cooldowns:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"hoursRemaining": 16`) {
		t.Errorf("hoursRemaining missing under cooldowns:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"lastFillAt": "2026-05-24T04:00:00Z"`) {
		t.Errorf("RFC-3339 lastFillAt missing under cooldowns:\n%s", prompt)
	}
}

// Empty / nil Cooldowns → no key in prompt.
func TestUserPromptOmitsCooldownsWhenEmpty(t *testing.T) {
	prompt := userPrompt(DecisionInput{
		FundID:      "f1",
		TradingDate: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
		Universe:    []string{"AAPL"},
	})
	if strings.Contains(prompt, `"cooldowns"`) {
		t.Errorf("empty Cooldowns should not appear in prompt:\n%s", prompt)
	}
}

// buildCooldownPromptItems drops blank-symbol Locks and rounds the
// hour counts to one decimal. Locking this contract pins both the
// safety filter and the prompt-size discipline.
func TestBuildCooldownPromptItemsDropsBlankAndRounds(t *testing.T) {
	got := buildCooldownPromptItems([]SymbolCooldown{
		{Symbol: " ", HoursSinceFill: 1, HoursRemaining: 1},                                                                      // dropped
		{Symbol: "AAPL", LastFillSide: "buy", HoursSinceFill: 8.273, HoursRemaining: 15.726, LastFillAt: time.Time{}, BlockedUntil: time.Time{}}, // ZERO times → no string
	})
	if len(got) != 1 {
		t.Fatalf("expected 1 surviving row, got %d (%+v)", len(got), got)
	}
	if got[0].Symbol != "AAPL" {
		t.Errorf("expected AAPL, got %q", got[0].Symbol)
	}
	if got[0].HoursSinceFill != 8.3 {
		t.Errorf("HoursSinceFill not rounded to 1dp: got %v", got[0].HoursSinceFill)
	}
	if got[0].HoursRemaining != 15.7 {
		t.Errorf("HoursRemaining not rounded to 1dp: got %v", got[0].HoursRemaining)
	}
	if got[0].LastFillAt != "" {
		t.Errorf("zero LastFillAt should produce empty string, got %q", got[0].LastFillAt)
	}
	if got[0].BlockedUntil != "" {
		t.Errorf("zero BlockedUntil should produce empty string, got %q", got[0].BlockedUntil)
	}
}

// roundTenth never produces negative numbers — the cooldown service
// can only emit non-negative durations, and we clamp here as defence
// in depth so a defective Lock can't poison the prompt.
func TestRoundTenthClampsAndRounds(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{0, 0},
		{1.0, 1.0},
		{1.249, 1.2},
		{1.25, 1.3}, // banker would round to 1.2; we use half-up
		{-3.5, 0},
	}
	for _, c := range cases {
		got := roundTenth(c.in)
		if got != c.want {
			t.Errorf("roundTenth(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// Sprint B #2: the system prompt must teach the PM how to read the
// riskBudget block — same loud-fail pattern as cooldowns above.
func TestSystemPromptDocumentsRiskBudgetRules(t *testing.T) {
	prompt := systemPrompt()
	required := []string{
		"input.riskBudget",
		"effectivePerTradeRiskPct",
		"volScalar",
		"ddScalar",
		"drawdown",
	}
	for _, frag := range required {
		if !strings.Contains(prompt, frag) {
			t.Errorf("systemPrompt() missing %q — the riskBudget rule has regressed", frag)
		}
	}
}

// The user prompt must serialise RiskBudget under the documented
// JSON key with the fields the system prompt references.
func TestUserPromptIncludesRiskBudgetBlock(t *testing.T) {
	prompt := userPrompt(DecisionInput{
		FundID:      "f1",
		TradingDate: time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC),
		Universe:    []string{"AAPL"},
		RiskBudget: &RiskBudgetSnapshot{
			Window:                   "60 trading days",
			SampleSize:               60,
			BasePerTradeRiskPct:      0.005,
			RealisedVolAnnualized:    0.18,
			VolTargetAnnualized:      0.15,
			VolScalar:                0.83,
			PeakNAV:                  1.20,
			CurrentNAV:               1.05,
			DrawdownPct:              0.125,
			DDCeilingPct:             0.25,
			DDScalar:                 0.5,
			EffectivePerTradeRiskPct: 0.00208,
		},
	})
	if !strings.Contains(prompt, `"riskBudget"`) {
		t.Errorf("user prompt missing riskBudget key:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"window": "60 trading days"`) {
		t.Errorf("window string missing under riskBudget:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"volScalar": 0.83`) {
		t.Errorf("volScalar missing/wrong under riskBudget:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"ddScalar": 0.5`) {
		t.Errorf("ddScalar missing/wrong under riskBudget:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"effectivePerTradeRiskPct": 0.0021`) {
		t.Errorf("effectivePerTradeRiskPct missing/wrong rounding under riskBudget:\n%s", prompt)
	}
	// NAV fields use 2dp rounder; expected outputs.
	if !strings.Contains(prompt, `"peakNav": 1.2`) {
		t.Errorf("peakNav missing/wrong under riskBudget:\n%s", prompt)
	}
}

// Nil RiskBudget → no key in prompt.
func TestUserPromptOmitsRiskBudgetWhenNil(t *testing.T) {
	prompt := userPrompt(DecisionInput{
		FundID:      "f1",
		TradingDate: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
		Universe:    []string{"AAPL"},
	})
	if strings.Contains(prompt, `"riskBudget"`) {
		t.Errorf("nil RiskBudget should not appear in prompt:\n%s", prompt)
	}
}

// buildRiskBudgetPromptItem on nil input returns nil.
func TestBuildRiskBudgetPromptItemNilSafe(t *testing.T) {
	if got := buildRiskBudgetPromptItem(nil); got != nil {
		t.Errorf("buildRiskBudgetPromptItem(nil) = %+v, want nil", got)
	}
}

// round2 clamps negatives and rounds half-up to 2dp. Pin the
// corner behaviour — NAV figures lean on this for the prompt.
//
// We deliberately avoid the 1.005 case here: 1.005 has no exact
// float64 representation (it's stored as ~1.00499999...) so the
// half-up rounder produces 1.00 — that's a property of IEEE-754,
// not of round2. The NAV figures we feed in have far more
// decimals from real NAV calculations so this edge case never
// surfaces in production.
func TestRound2(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{0, 0},
		{1.234, 1.23},
		{1.235, 1.24},
		{1.215, 1.22}, // half-up on a representable boundary
		{-3.5, 0},     // clamp negatives
	}
	for _, c := range cases {
		got := round2(c.in)
		if got != c.want {
			t.Errorf("round2(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// Sprint B #3: the system prompt must teach the PM how to read the
// newsCatalysts block.
func TestSystemPromptDocumentsNewsCatalystsRules(t *testing.T) {
	prompt := systemPrompt()
	required := []string{
		"input.newsCatalysts",
		"hoursOld",
		"publishedAt",
		"contradicts",
		"REINFORCES",
	}
	for _, frag := range required {
		if !strings.Contains(prompt, frag) {
			t.Errorf("systemPrompt() missing %q — the newsCatalysts rule has regressed", frag)
		}
	}
}

// The user prompt must serialise NewsCatalysts under the documented
// JSON key with all the fields the system prompt references.
func TestUserPromptIncludesNewsCatalystsBlock(t *testing.T) {
	published := time.Date(2026, 5, 24, 8, 0, 0, 0, time.UTC)
	prompt := userPrompt(DecisionInput{
		FundID:      "f1",
		TradingDate: time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
		Universe:    []string{"AAPL"},
		NewsCatalysts: []SymbolNewsCatalysts{
			{
				Symbol: "AAPL",
				Hits: []NewsHit{
					{
						Title:       "AAPL Q4 guidance cut",
						Summary:     "Apple lowered its Q4 services guidance citing weaker China demand and headwinds.",
						Source:      "reuters",
						Language:    "en",
						PublishedAt: published,
						HoursOld:    4.0,
					},
				},
			},
		},
	})
	if !strings.Contains(prompt, `"newsCatalysts"`) {
		t.Errorf("user prompt missing newsCatalysts key:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"symbol": "AAPL"`) {
		t.Errorf("AAPL symbol not surfaced under newsCatalysts:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"title": "AAPL Q4 guidance cut"`) {
		t.Errorf("title not surfaced under newsCatalysts:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"source": "reuters"`) {
		t.Errorf("source not surfaced under newsCatalysts:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"publishedAt": "2026-05-24T08:00:00Z"`) {
		t.Errorf("RFC-3339 publishedAt not surfaced under newsCatalysts:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"hoursOld": 4`) {
		t.Errorf("hoursOld not surfaced under newsCatalysts:\n%s", prompt)
	}
}

// Empty / nil NewsCatalysts → no key in prompt.
func TestUserPromptOmitsNewsCatalystsWhenEmpty(t *testing.T) {
	prompt := userPrompt(DecisionInput{
		FundID:      "f1",
		TradingDate: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
		Universe:    []string{"AAPL"},
	})
	if strings.Contains(prompt, `"newsCatalysts"`) {
		t.Errorf("empty NewsCatalysts should not appear in prompt:\n%s", prompt)
	}
}

// Sprint C #1: the system prompt must teach the PM the exposure
// guardrails.
func TestSystemPromptDocumentsExposureRules(t *testing.T) {
	prompt := systemPrompt()
	required := []string{
		"input.exposure",
		"breaches",
		"sectorCap",
		"top3",
		"cashFloorPct",
	}
	for _, frag := range required {
		if !strings.Contains(prompt, frag) {
			t.Errorf("systemPrompt() missing %q — the exposure rule has regressed", frag)
		}
	}
}

// The user prompt must serialise the Exposure snapshot under the
// documented JSON key with the fields the system prompt
// references.
func TestUserPromptIncludesExposureBlock(t *testing.T) {
	prompt := userPrompt(DecisionInput{
		FundID:      "f1",
		TradingDate: time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC),
		Universe:    []string{"AAPL"},
		Exposure: ExposureSnapshot{
			TotalAssets:   1000,
			AvailableCash: 50,
			CashPct:       0.05,
			CashFloorPct:  0.05,
			PositionCount: 3,
			SingleName: []SymbolWeight{
				{Symbol: "AAPL", Weight: 0.40, Cap: 0.25, Breach: true},
				{Symbol: "MSFT", Weight: 0.30, Cap: 0.25, Breach: true},
				{Symbol: "NVDA", Weight: 0.25, Cap: 0.25, Breach: false},
			},
			SingleNameCap: 0.25,
			SectorWeights: []SectorWeight{
				{Sector: "tech", Weight: 0.95, Cap: 0.50, Breach: true},
			},
			SectorCap:  0.50,
			Top3Weight: 0.95,
			Top3Cap:    0.60,
			Breaches: []string{
				"BREACH: sector=tech weight=95.0% > cap=50.0%",
				"BREACH: top-3=cluster weight=95.0% > cap=60.0%",
			},
		},
	})
	if !strings.Contains(prompt, `"exposure"`) {
		t.Errorf("user prompt missing exposure key:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"symbol": "AAPL"`) || !strings.Contains(prompt, `"breach": true`) {
		t.Errorf("AAPL breach row missing under exposure:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"sectorCap": 0.5`) {
		t.Errorf("sectorCap missing under exposure:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"top3Weight": 0.95`) {
		t.Errorf("top3Weight missing under exposure:\n%s", prompt)
	}
	if !strings.Contains(prompt, "BREACH: sector=tech") {
		t.Errorf("sector breach line missing under exposure.breaches:\n%s", prompt)
	}
}

// Empty (zero-NAV) Exposure → no key in prompt.
func TestUserPromptOmitsExposureWhenEmpty(t *testing.T) {
	prompt := userPrompt(DecisionInput{
		FundID:      "f1",
		TradingDate: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
		Universe:    []string{"AAPL"},
	})
	if strings.Contains(prompt, `"exposure"`) {
		t.Errorf("empty Exposure should not appear in prompt:\n%s", prompt)
	}
}

// buildExposurePromptItem returns nil on no-signal input — no
// matter how the inner SingleName slice is populated, an
// Exposure{} with zero TotalAssets must produce nil.
func TestBuildExposurePromptItemNoSignalReturnsNil(t *testing.T) {
	got := buildExposurePromptItem(ExposureSnapshot{})
	if got != nil {
		t.Errorf("expected nil on no-signal exposure, got %+v", got)
	}
}

// SymbolWeight + SectorWeight type aliases are exported so the
// wiring layer + tests can construct them without importing
// exposure directly.
func TestExposureAliasesAreUsable(t *testing.T) {
	var sn SymbolWeight = SymbolWeight{Symbol: "AAPL", Weight: 0.1}
	var sw SectorWeight = SectorWeight{Sector: "tech", Weight: 0.2}
	if sn.Symbol != "AAPL" || sw.Sector != "tech" {
		t.Errorf("alias round-trip failed: %+v %+v", sn, sw)
	}
}

// Sprint C #2: the system prompt must teach the PM the
// correlation-aware sizing rules.
func TestSystemPromptDocumentsCorrelationRules(t *testing.T) {
	prompt := systemPrompt()
	required := []string{
		"input.correlations",
		"highCorrPairs",
		"candidateSummaries",
		"heldCluster",
		"maxRho",
	}
	for _, frag := range required {
		if !strings.Contains(prompt, frag) {
			t.Errorf("systemPrompt() missing %q — the correlation rule has regressed", frag)
		}
	}
}

// The user prompt must serialise a CorrelationSnapshot under the
// "correlations" key with all three sub-blocks.
func TestUserPromptIncludesCorrelationsBlock(t *testing.T) {
	snap := CorrelationSnapshot{
		Window:            "60 daily bars",
		SampleSize:        4,
		HighCorrThreshold: 0.7,
		HighCorrPairs: []HighCorrPair{
			{Left: "AMD", Right: "NVDA", Rho: 0.85},
		},
		CandidateSummaries: []CorrCandidateSummary{
			{Symbol: "AMD", MaxRho: 0.85, MaxAbsRho: 0.85, MaxAbsTarget: "NVDA"},
		},
		HeldCluster: &HeldClusterStats{
			HeldCount:   3,
			AvgPairwise: 0.62,
			MaxPairwise: 0.85,
			MaxLeft:     "MSFT",
			MaxRight:    "NVDA",
		},
	}
	prompt := userPrompt(DecisionInput{
		FundID:       "f1",
		TradingDate:  time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC),
		Universe:     []string{"AMD"},
		Correlations: &snap,
	})
	if !strings.Contains(prompt, `"correlations"`) {
		t.Errorf("user prompt missing correlations key:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"left": "AMD"`) || !strings.Contains(prompt, `"right": "NVDA"`) {
		t.Errorf("high-corr pair missing from prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"maxAbsTarget": "NVDA"`) {
		t.Errorf("candidate summary target missing:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"heldCount": 3`) {
		t.Errorf("held cluster missing from prompt:\n%s", prompt)
	}
}

// Nil Correlations → no key in the prompt.
func TestUserPromptOmitsCorrelationsWhenNil(t *testing.T) {
	prompt := userPrompt(DecisionInput{
		FundID:      "f1",
		TradingDate: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
		Universe:    []string{"AAPL"},
	})
	if strings.Contains(prompt, `"correlations"`) {
		t.Errorf("nil Correlations should not appear in prompt:\n%s", prompt)
	}
}

// buildCorrelationsPromptItem must be nil-safe and reject
// snapshots with no signal even when fields are partially set.
func TestBuildCorrelationsPromptItemNoSignalReturnsNil(t *testing.T) {
	if got := buildCorrelationsPromptItem(nil); got != nil {
		t.Errorf("nil snapshot: got %+v, want nil", got)
	}
	if got := buildCorrelationsPromptItem(&CorrelationSnapshot{SampleSize: 1}); got != nil {
		t.Errorf("sample size 1: got %+v, want nil", got)
	}
}

// truncateSummary respects the rune boundary (avoids the classic
// "slice in the middle of a 3-byte UTF-8 character" trap) and
// returns the original when within the cap.
func TestTruncateSummaryUTF8Safe(t *testing.T) {
	if got := truncateSummary("", 10); got != "" {
		t.Errorf("empty in → got %q, want empty", got)
	}
	if got := truncateSummary("short", 10); got != "short" {
		t.Errorf("within cap: got %q, want short", got)
	}
	// 4 Chinese characters, each 3 bytes (12 bytes total) — cap 2
	// runes means we keep 2 chars + "…" suffix.
	got := truncateSummary("一二三四五六", 2)
	want := "一二…"
	if got != want {
		t.Errorf("truncate 6 chars to 2: got %q, want %q", got, want)
	}
	// Negative / zero cap → empty.
	if got := truncateSummary("anything", 0); got != "" {
		t.Errorf("zero cap should produce empty, got %q", got)
	}
}

// =============================================================================
// Sprint 1 / Tier-S learning-loop closure: prompt-side coverage for
// AgentSkills / RecentLessons / LongTermReflections.
//
// These three blocks are the ground-truth contract between the wiring
// layer (which loads them) and the LLM (which acts on them). When the
// audit found they were wired but had ZERO prompt-side assertions we
// added these tests to nail down (a) the system prompt teaches the
// LLM how to consume each block, (b) the user prompt actually surfaces
// the data under the documented JSON key, and (c) the cap helpers
// enforce the documented limits so a runaway skill set or memory
// burst can't blow the prompt budget.
// =============================================================================

// SystemPrompt documents the agentSkills block — the LLM needs to
// know how to weigh approved skills (e.g. "first-class behavioural
// constraint"). Asserts on stable substrings from prompt.go.
func TestSystemPromptDocumentsAgentSkillsRules(t *testing.T) {
	fl := &fakeLLM{
		respond: func(_ llm.ChatRequest) (*llm.ChatResponse, error) {
			return &llm.ChatResponse{Content: `{"actions":[],"stance":"watch","confidence":0.5}`}, nil
		},
	}
	engine := &LLMDecisionEngine{Client: fl}
	_, _ = engine.Decide(context.Background(), DecisionInput{FundID: "f", Universe: []string{"AAPL"}})
	sys := fl.lastReq.Messages[0].Content
	for _, want := range []string{"input.agentSkills", "APPROVED + ENABLED", "first-class behavioural constraint"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q\n--- prompt ---\n%s", want, sys)
		}
	}
}

// User prompt surfaces agentSkills under the documented JSON key with
// the human-readable fields (name + description + source) intact.
func TestLLMDecisionEngineInjectsAgentSkills(t *testing.T) {
	fl := &fakeLLM{
		respond: func(_ llm.ChatRequest) (*llm.ChatResponse, error) {
			return &llm.ChatResponse{Content: `{"actions":[],"stance":"watch","confidence":0.5}`}, nil
		},
	}
	engine := &LLMDecisionEngine{Client: fl}
	_, _ = engine.Decide(context.Background(), DecisionInput{
		FundID:      "fund-skills",
		TradingDate: time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC),
		Universe:    []string{"AAPL"},
		AgentSkills: []AgentSkillContext{
			{
				AgentRole:   "pm",
				AgentName:   "PM-Alpha",
				Name:        "earnings_window_caution",
				Description: "降低 5 个交易日内有财报标的的新建仓比例",
				Source:      "reflection",
			},
		},
	})
	user := fl.lastReq.Messages[1].Content
	for _, want := range []string{"agentSkills", "earnings_window_caution", "PM-Alpha", "reflection", "降低"} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing %q\n--- prompt ---\n%s", want, user)
		}
	}
}

// When the wiring layer passes no skills the JSON key must be omitted
// (omitempty) so the LLM doesn't hallucinate a "no skills configured"
// signal where none exists.
func TestLLMDecisionEngineOmitsAgentSkillsWhenEmpty(t *testing.T) {
	fl := &fakeLLM{
		respond: func(_ llm.ChatRequest) (*llm.ChatResponse, error) {
			return &llm.ChatResponse{Content: `{"actions":[],"stance":"watch","confidence":0.5}`}, nil
		},
	}
	engine := &LLMDecisionEngine{Client: fl}
	_, _ = engine.Decide(context.Background(), DecisionInput{
		FundID:   "fund-bare",
		Universe: []string{"AAPL"},
	})
	user := fl.lastReq.Messages[1].Content
	if strings.Contains(user, "agentSkills") {
		t.Errorf("user prompt contains agentSkills key despite empty input:\n%s", user)
	}
}

// SystemPrompt documents recentLessons — the textual memory the PM
// should pattern-match against today's candidates.
func TestSystemPromptDocumentsRecentLessonsRules(t *testing.T) {
	fl := &fakeLLM{
		respond: func(_ llm.ChatRequest) (*llm.ChatResponse, error) {
			return &llm.ChatResponse{Content: `{"actions":[],"stance":"watch","confidence":0.5}`}, nil
		},
	}
	engine := &LLMDecisionEngine{Client: fl}
	_, _ = engine.Decide(context.Background(), DecisionInput{FundID: "f", Universe: []string{"AAPL"}})
	sys := fl.lastReq.Messages[0].Content
	for _, want := range []string{"input.recentLessons", "distilled lesson", "Cite the lesson date"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q\n--- prompt ---\n%s", want, sys)
		}
	}
}

// User prompt surfaces recentLessons with date / layer / role / body.
func TestLLMDecisionEngineInjectsRecentLessons(t *testing.T) {
	fl := &fakeLLM{
		respond: func(_ llm.ChatRequest) (*llm.ChatResponse, error) {
			return &llm.ChatResponse{Content: `{"actions":[],"stance":"watch","confidence":0.5}`}, nil
		},
	}
	engine := &LLMDecisionEngine{Client: fl}
	_, _ = engine.Decide(context.Background(), DecisionInput{
		FundID:      "fund-lessons",
		TradingDate: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
		Universe:    []string{"TSLA"},
		RecentLessons: []RecentLessonContext{
			{
				TradingDate: "2026-05-21",
				Layer:       "agent",
				AgentRole:   "pm",
				Title:       "TSLA add in trend_down",
				Content:     "added TSLA into a confirmed trend_down regime, lost 1.8% same session",
				Tags:        []string{"trend_down", "TSLA"},
			},
		},
	})
	user := fl.lastReq.Messages[1].Content
	for _, want := range []string{"recentLessons", "2026-05-21", "pm", "TSLA add in trend_down", "lost 1.8%"} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing %q\n--- prompt ---\n%s", want, user)
		}
	}
}

// SystemPrompt documents longTermReflections — the slow-moving
// steering field on top of recentLessons.
func TestSystemPromptDocumentsLongTermReflectionsRules(t *testing.T) {
	fl := &fakeLLM{
		respond: func(_ llm.ChatRequest) (*llm.ChatResponse, error) {
			return &llm.ChatResponse{Content: `{"actions":[],"stance":"watch","confidence":0.5}`}, nil
		},
	}
	engine := &LLMDecisionEngine{Client: fl}
	_, _ = engine.Decide(context.Background(), DecisionInput{FundID: "f", Universe: []string{"AAPL"}})
	sys := fl.lastReq.Messages[0].Content
	for _, want := range []string{"input.longTermReflections", "at most 5", "slow-moving steering field"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q\n--- prompt ---\n%s", want, sys)
		}
	}
}

// User prompt surfaces longTermReflections with createdAt + body.
func TestLLMDecisionEngineInjectsLongTermReflections(t *testing.T) {
	fl := &fakeLLM{
		respond: func(_ llm.ChatRequest) (*llm.ChatResponse, error) {
			return &llm.ChatResponse{Content: `{"actions":[],"stance":"watch","confidence":0.5}`}, nil
		},
	}
	engine := &LLMDecisionEngine{Client: fl}
	_, _ = engine.Decide(context.Background(), DecisionInput{
		FundID:      "fund-reflections",
		TradingDate: time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC),
		Universe:    []string{"NVDA"},
		LongTermReflections: []LongTermReflectionContext{
			{
				CreatedAt: "2026-05-10",
				Title:     "AI infra leadership thesis",
				Content:   "NVDA + AMD + AVGO持续受益于推理需求，回调即买点直到 Q4 财报季",
				Tags:      []string{"ai", "semis"},
			},
		},
	})
	user := fl.lastReq.Messages[1].Content
	for _, want := range []string{"longTermReflections", "2026-05-10", "AI infra leadership thesis", "持续受益"} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing %q\n--- prompt ---\n%s", want, user)
		}
	}
}

// Cap fence: capAgentSkillContexts enforces both the slice-length cap
// (so a fund with 200 skills can't blow the prompt budget) and the
// per-row description cap (so a single 5000-char skill doesn't slip
// through). limit=0 / nil input return nil rather than empty slice so
// json.MarshalIndent emits omitempty.
func TestCapAgentSkillContextsEnforcesBudgets(t *testing.T) {
	if got := capAgentSkillContexts(nil, 10); got != nil {
		t.Errorf("nil input should return nil, got %v", got)
	}
	if got := capAgentSkillContexts([]AgentSkillContext{{Name: "x"}}, 0); got != nil {
		t.Errorf("zero limit should return nil, got %v", got)
	}
	// Length cap at 3
	items := []AgentSkillContext{{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}}
	if got := capAgentSkillContexts(items, 3); len(got) != 3 {
		t.Errorf("length cap: got len=%d, want 3", len(got))
	}
	// Description rune cap at 240
	long := strings.Repeat("一", 300)
	out := capAgentSkillContexts([]AgentSkillContext{{Name: "n", Description: long}}, 5)
	if len(out) != 1 {
		t.Fatalf("got %d items, want 1", len(out))
	}
	if runes := []rune(out[0].Description); len(runes) > 241 { // 240 + ellipsis
		t.Errorf("description not truncated: %d runes (want ≤241)", len(runes))
	}
	if !strings.HasSuffix(out[0].Description, "…") {
		t.Errorf("description missing ellipsis suffix: %q", out[0].Description)
	}
}

// Cap fence: capRecentLessonContexts limit + body 280 + title 80.
func TestCapRecentLessonContextsEnforcesBudgets(t *testing.T) {
	if got := capRecentLessonContexts(nil, 10); got != nil {
		t.Errorf("nil input should return nil")
	}
	items := []RecentLessonContext{{Content: "a"}, {Content: "b"}, {Content: "c"}}
	if got := capRecentLessonContexts(items, 2); len(got) != 2 {
		t.Errorf("length cap: got %d, want 2", len(got))
	}
	bigContent := strings.Repeat("内", 400)
	bigTitle := strings.Repeat("题", 100)
	out := capRecentLessonContexts([]RecentLessonContext{{Title: bigTitle, Content: bigContent}}, 5)
	if r := []rune(out[0].Content); len(r) > 281 {
		t.Errorf("content not truncated: %d runes", len(r))
	}
	if r := []rune(out[0].Title); len(r) > 81 {
		t.Errorf("title not truncated: %d runes", len(r))
	}
}

// Cap fence: capLongTermReflectionContexts limit + body 360 + title 80.
func TestCapLongTermReflectionContextsEnforcesBudgets(t *testing.T) {
	if got := capLongTermReflectionContexts(nil, 5); got != nil {
		t.Errorf("nil input should return nil")
	}
	items := []LongTermReflectionContext{{Content: "a"}, {Content: "b"}, {Content: "c"}, {Content: "d"}, {Content: "e"}, {Content: "f"}}
	if got := capLongTermReflectionContexts(items, 5); len(got) != 5 {
		t.Errorf("length cap: got %d, want 5", len(got))
	}
	bigContent := strings.Repeat("思", 500)
	out := capLongTermReflectionContexts([]LongTermReflectionContext{{Content: bigContent}}, 5)
	if r := []rune(out[0].Content); len(r) > 361 {
		t.Errorf("content not truncated: %d runes", len(r))
	}
}
