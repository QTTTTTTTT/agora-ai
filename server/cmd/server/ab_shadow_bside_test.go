package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/llm"
	"github.com/fundai/server/internal/repository"
)

// recordedABShadowMetrics is a tiny K-5 stub that lets tests
// assert which outcome label was emitted for each LLM-shadow
// decision. Mirrors the pattern used by `recordedMetrics` in
// corp_action_ingest_loop_test.go so the shape stays familiar.
type recordedABShadowMetrics struct {
	calls []string
}

func (r *recordedABShadowMetrics) RecordABShadowLLMCall(outcome string) {
	r.calls = append(r.calls, outcome)
}

// stubLLMClient implements llm.LLMClient with canned responses.
// Tracks call count + the last prompt the test asked for so
// assertions can pin both behaviours.
type stubLLMClient struct {
	respContent string
	respErr     error
	calls       int
	lastReq     llm.ChatRequest
}

func (s *stubLLMClient) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	s.calls++
	s.lastReq = req
	if s.respErr != nil {
		return nil, s.respErr
	}
	return &llm.ChatResponse{Content: s.respContent}, nil
}

func (s *stubLLMClient) ListModels(_ context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}

// sampleControlTrade is a tiny fixture so tests don't repeat the
// trade-builder boilerplate.
func sampleControlTrade() repository.TradeExecution {
	return repository.TradeExecution{
		ID:        "trade-1",
		Symbol:    "000001.SS",
		Side:      "buy",
		Quantity:  1000,
		Price:     sql.NullFloat64{Float64: 12.34, Valid: true},
		CreatedAt: time.Date(2026, 5, 15, 9, 30, 0, 0, time.UTC),
	}
}

func sampleVariantB() abShadowVariantRuntime {
	return abShadowVariantRuntime{
		ID:   "variant-b",
		Key:  "B",
		Name: "treatment",
		StrategyConfig: map[string]any{
			"pmStyle":           "aggressive",
			"maxSinglePosition": 0.18,
		},
	}
}

// sampleVariantBWithTeam returns the same fixture as
// sampleVariantB but with a 3-member team_snapshot so K-2
// prompt-grounding tests can assert that role/focus/name show
// up in the rendered prompt.
func sampleVariantBWithTeam() abShadowVariantRuntime {
	v := sampleVariantB()
	v.TeamSnapshot = abTeamSnapshot{
		FundID: "fund-b",
		Members: []abTeamSnapshotMember{
			{Role: "pm", Focus: "growth", AgentName: "alpha-pm"},
			{Role: "researcher", Focus: "factor", AgentName: "fox"},
			{Role: "risk", AgentName: "owl"},
		},
	}
	return v
}

// sampleBSideContext returns a small but non-trivial context
// covering 7 trading days. NAV climbs then dips so the
// drawdown statistic is non-zero and we can assert the prompt
// surfaces it.
func sampleBSideContext() abBSideContext {
	day := func(d int) time.Time {
		return time.Date(2026, 5, d, 0, 0, 0, 0, time.UTC)
	}
	navs := []repository.NavSnapshot{
		{TradingDate: day(10), NAV: 1.0000, DailyReturn: 0.000, TotalReturn: 0.000, AvailableCash: 100_000},
		{TradingDate: day(11), NAV: 1.0100, DailyReturn: 0.010, TotalReturn: 0.010, AvailableCash: 95_000},
		{TradingDate: day(12), NAV: 1.0250, DailyReturn: 0.0149, TotalReturn: 0.025, AvailableCash: 90_000},
		{TradingDate: day(13), NAV: 1.0500, DailyReturn: 0.0244, TotalReturn: 0.050, AvailableCash: 80_000},
		{TradingDate: day(14), NAV: 1.0300, DailyReturn: -0.0190, TotalReturn: 0.030, AvailableCash: 82_000},
		{TradingDate: day(15), NAV: 1.0050, DailyReturn: -0.0243, TotalReturn: 0.005, AvailableCash: 84_000},
		{TradingDate: day(16), NAV: 1.0150, DailyReturn: 0.0099, TotalReturn: 0.015, AvailableCash: 84_000},
	}
	trades := []repository.TradeExecution{
		{Symbol: "AAA", Side: "BUY", Quantity: 100, Price: sql.NullFloat64{Float64: 10, Valid: true}, CreatedAt: day(11)},
		{Symbol: "BBB", Side: "BUY", Quantity: 50, Price: sql.NullFloat64{Float64: 20, Valid: true}, CreatedAt: day(12)},
		{Symbol: "AAA", Side: "SELL", Quantity: 60, Price: sql.NullFloat64{Float64: 11, Valid: true}, CreatedAt: day(15)},
	}
	return abBSideContextBuild(navs, trades, day(10), day(16))
}

// ----------------------------------------------------------------------
// Deterministic decider — pins legacy arithmetic
// ----------------------------------------------------------------------

func TestDeterministicBSideDecider_DecideTradeReturnsConstantScale(t *testing.T) {
	d := deterministicBSideDecider{}
	dec1, err := d.DecideTrade(context.Background(), sampleVariantB(), sampleControlTrade(), abBSideContext{})
	if err != nil {
		t.Fatalf("DecideTrade err: %v", err)
	}
	dec2, err := d.DecideTrade(context.Background(), sampleVariantB(), sampleControlTrade(), abBSideContext{})
	if err != nil {
		t.Fatalf("DecideTrade err: %v", err)
	}
	// Same input → same output — that's the deterministic
	// contract.
	if dec1.QuantityScale != dec2.QuantityScale {
		t.Errorf("expected stable qty_scale, got %v vs %v", dec1.QuantityScale, dec2.QuantityScale)
	}
	if dec1.Skip {
		t.Errorf("deterministic path must not skip")
	}
	// Aggressive style with maxSinglePosition=0.18 should give
	// scale > 1 (per abStrategyTradeScale).
	if dec1.QuantityScale <= 1 {
		t.Errorf("expected aggressive variant to scale UP, got %v", dec1.QuantityScale)
	}
	if !strings.Contains(dec1.Reasoning, "[auto-shadow]") {
		t.Errorf("reasoning missing audit tag, got %q", dec1.Reasoning)
	}
}

func TestDeterministicBSideDecider_SummarizeReturnsCannedCopy(t *testing.T) {
	d := deterministicBSideDecider{}
	recap, err := d.SummarizeBLearning(context.Background(), sampleVariantB(), nil, abBSideContext{})
	if err != nil {
		t.Fatalf("SummarizeBLearning err: %v", err)
	}
	if len(recap.Lessons) == 0 || len(recap.Adjustments) == 0 {
		t.Errorf("canned recap must populate lessons + adjustments, got %+v", recap)
	}
	if !strings.Contains(recap.Summary, "[auto-shadow]") {
		t.Errorf("summary missing audit tag, got %q", recap.Summary)
	}
}

// ----------------------------------------------------------------------
// LLM decider — happy path + every fallback class
// ----------------------------------------------------------------------

func TestLLMBSideDecider_HappyPathReturnsParsedDecision(t *testing.T) {
	stub := &stubLLMClient{
		respContent: `{"skip": false, "quantity_scale": 0.5, "side_override": "", "reasoning": "B 偏保守，仓位较高时减半。"}`,
	}
	d, err := newLLMBSideDecider(stub)
	if err != nil {
		t.Fatalf("newLLMBSideDecider err: %v", err)
	}
	dec, err := d.DecideTrade(context.Background(), sampleVariantB(), sampleControlTrade(), abBSideContext{})
	if err != nil {
		t.Fatalf("DecideTrade err: %v", err)
	}
	if dec.Skip {
		t.Errorf("expected skip=false")
	}
	if dec.QuantityScale != 0.5 {
		t.Errorf("expected 0.5, got %v", dec.QuantityScale)
	}
	if !strings.HasPrefix(dec.Reasoning, "[auto-shadow][llm]") {
		t.Errorf("missing llm tag, got %q", dec.Reasoning)
	}
	if stub.calls != 1 {
		t.Errorf("expected 1 LLM call, got %d", stub.calls)
	}
}

// TestLLMBSideDecider_PromptCarriesK2Context is the integration
// glue check: the prompt that the LLM client actually receives
// (via stub.lastReq) must include the K-2 grounding signals.
// Without this, a future refactor could accidentally remove the
// context plumbing while keeping the unit tests on
// buildBSideTradePrompt green.
func TestLLMBSideDecider_PromptCarriesK2Context(t *testing.T) {
	stub := &stubLLMClient{
		respContent: `{"skip": false, "quantity_scale": 1.0, "reasoning": "ok"}`,
	}
	d, _ := newLLMBSideDecider(stub)
	v := sampleVariantBWithTeam()
	bsideCtx := sampleBSideContext()
	trade := repository.TradeExecution{
		Symbol:    "AAA",
		Side:      "BUY",
		Quantity:  100,
		Price:     sql.NullFloat64{Float64: 12.34, Valid: true},
		CreatedAt: time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC),
	}
	if _, err := d.DecideTrade(context.Background(), v, trade, bsideCtx); err != nil {
		t.Fatalf("DecideTrade err: %v", err)
	}
	if len(stub.lastReq.Messages) < 2 {
		t.Fatalf("expected >=2 messages, got %d", len(stub.lastReq.Messages))
	}
	userPrompt := stub.lastReq.Messages[len(stub.lastReq.Messages)-1].Content
	for _, want := range []string{
		"alpha-pm",   // team
		"日期 NAV",     // per-date NAV state
		"近 ",         // trailing window
		"aggressive", // strategy field
	} {
		if !strings.Contains(userPrompt, want) {
			t.Errorf("LLM trade prompt missing %q\nactual:\n%s", want, userPrompt)
		}
	}
}

func TestLLMBSideDecider_SkipDecisionIsHonored(t *testing.T) {
	stub := &stubLLMClient{
		respContent: `{"skip": true, "quantity_scale": 0, "reasoning": "B 在该市场环境下选择 skip。"}`,
	}
	d, _ := newLLMBSideDecider(stub)
	dec, err := d.DecideTrade(context.Background(), sampleVariantB(), sampleControlTrade(), abBSideContext{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !dec.Skip {
		t.Errorf("expected skip=true, got %+v", dec)
	}
}

func TestLLMBSideDecider_FallsBackOnLLMError(t *testing.T) {
	stub := &stubLLMClient{respErr: errors.New("upstream timeout")}
	d, _ := newLLMBSideDecider(stub)
	dec, err := d.DecideTrade(context.Background(), sampleVariantB(), sampleControlTrade(), abBSideContext{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Should match deterministic shape (qty_scale > 0, no skip,
	// canned reasoning prefix).
	if dec.Skip {
		t.Errorf("fallback should not skip")
	}
	if dec.QuantityScale <= 0 {
		t.Errorf("fallback qty_scale should be > 0, got %v", dec.QuantityScale)
	}
	if strings.Contains(dec.Reasoning, "[llm]") {
		t.Errorf("fallback should NOT carry the llm tag, got %q", dec.Reasoning)
	}
}

func TestLLMBSideDecider_FallsBackOnInvalidJSON(t *testing.T) {
	stub := &stubLLMClient{respContent: "this is not json"}
	d, _ := newLLMBSideDecider(stub)
	dec, err := d.DecideTrade(context.Background(), sampleVariantB(), sampleControlTrade(), abBSideContext{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if dec.QuantityScale <= 0 {
		t.Errorf("fallback qty_scale should be > 0, got %v", dec.QuantityScale)
	}
	if strings.Contains(dec.Reasoning, "[llm]") {
		t.Errorf("invalid JSON should fall back, got %q", dec.Reasoning)
	}
}

func TestLLMBSideDecider_TolatesMarkdownFence(t *testing.T) {
	stub := &stubLLMClient{
		respContent: "```json\n{\"skip\": false, \"quantity_scale\": 1.2, \"reasoning\": \"轻度放量\"}\n```",
	}
	d, _ := newLLMBSideDecider(stub)
	dec, err := d.DecideTrade(context.Background(), sampleVariantB(), sampleControlTrade(), abBSideContext{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if dec.QuantityScale != 1.2 {
		t.Errorf("fenced JSON should still parse to 1.2, got %v", dec.QuantityScale)
	}
}

func TestLLMBSideDecider_BudgetCapFallsBack(t *testing.T) {
	stub := &stubLLMClient{
		respContent: `{"skip": false, "quantity_scale": 1.5, "reasoning": "ok"}`,
	}
	d, _ := newLLMBSideDecider(stub)
	d.maxLLMCalls = 1 // tiny budget
	// First call uses the LLM
	dec1, _ := d.DecideTrade(context.Background(), sampleVariantB(), sampleControlTrade(), abBSideContext{})
	if !strings.HasPrefix(dec1.Reasoning, "[auto-shadow][llm]") {
		t.Errorf("first call should hit LLM, got %q", dec1.Reasoning)
	}
	// Second call falls back due to cap
	dec2, _ := d.DecideTrade(context.Background(), sampleVariantB(), sampleControlTrade(), abBSideContext{})
	if !strings.Contains(dec2.Reasoning, "budget reached") {
		t.Errorf("second call should hit budget cap, got %q", dec2.Reasoning)
	}
	if stub.calls != 1 {
		t.Errorf("LLM should have been called exactly once (budget=1), got %d", stub.calls)
	}
}

func TestLLMBSideDecider_RecapHappyPath(t *testing.T) {
	stub := &stubLLMClient{
		respContent: `{"lessons": ["重视风险控制"], "adjustments": ["维持保守仓位"], "summary": "B 在本期表现良好。", "specialization_learning": "", "proposed_evolution_config": {"riskLevel": "low"}}`,
	}
	d, _ := newLLMBSideDecider(stub)
	recap, err := d.SummarizeBLearning(context.Background(), sampleVariantB(), nil, abBSideContext{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(recap.Lessons) != 1 || recap.Lessons[0] != "重视风险控制" {
		t.Errorf("lessons mismatch, got %+v", recap.Lessons)
	}
	if !strings.HasPrefix(recap.Summary, "[auto-shadow][llm]") {
		t.Errorf("missing llm tag in summary, got %q", recap.Summary)
	}
	if recap.ProposedEvolutionConfig["riskLevel"] != "low" {
		t.Errorf("proposed config not propagated, got %+v", recap.ProposedEvolutionConfig)
	}
}

func TestLLMBSideDecider_RecapFallsBackOnInvalidJSON(t *testing.T) {
	stub := &stubLLMClient{respContent: "not json at all"}
	d, _ := newLLMBSideDecider(stub)
	recap, err := d.SummarizeBLearning(context.Background(), sampleVariantB(), nil, abBSideContext{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(recap.Lessons) == 0 {
		t.Errorf("fallback recap must have lessons, got %+v", recap)
	}
	if strings.Contains(recap.Summary, "[llm]") {
		t.Errorf("fallback summary should NOT carry llm tag, got %q", recap.Summary)
	}
}

// TestParseBSideDecision_ClipsAndDefends pins the input
// validator. The system prompt asks the LLM to respect bounds,
// but if it doesn't, we hard-clip and refuse degenerate values.
func TestParseBSideDecision_ClipsAndDefends(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
		check   func(t *testing.T, dec abBSideDecision)
	}{
		{
			name: "qty_scale too high gets clipped to 3",
			raw:  `{"skip": false, "quantity_scale": 99}`,
			check: func(t *testing.T, dec abBSideDecision) {
				if dec.QuantityScale != 3 {
					t.Errorf("expected clip to 3, got %v", dec.QuantityScale)
				}
			},
		},
		{
			name: "qty_scale too low gets clipped to 0.05",
			raw:  `{"skip": false, "quantity_scale": 0.0001}`,
			check: func(t *testing.T, dec abBSideDecision) {
				if dec.QuantityScale != 0.05 {
					t.Errorf("expected clip to 0.05, got %v", dec.QuantityScale)
				}
			},
		},
		{
			name: "negative qty_scale rejected",
			raw:  `{"skip": false, "quantity_scale": -1}`,
			wantErr: true,
		},
		{
			name: "garbage side_override silently dropped",
			raw:  `{"skip": false, "quantity_scale": 1, "side_override": "MAYBE"}`,
			check: func(t *testing.T, dec abBSideDecision) {
				if dec.SideOverride != "" {
					t.Errorf("garbage side should be dropped, got %q", dec.SideOverride)
				}
			},
		},
		{
			name: "valid SELL override accepted",
			raw:  `{"skip": false, "quantity_scale": 1, "side_override": "SELL"}`,
			check: func(t *testing.T, dec abBSideDecision) {
				if dec.SideOverride != "SELL" {
					t.Errorf("expected SELL, got %q", dec.SideOverride)
				}
			},
		},
		{
			name: "missing reasoning gets default",
			raw:  `{"skip": false, "quantity_scale": 1}`,
			check: func(t *testing.T, dec abBSideDecision) {
				if strings.TrimSpace(dec.Reasoning) == "" {
					t.Errorf("expected default reasoning, got empty")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec, err := parseBSideDecision(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil with %+v", dec)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if tc.check != nil {
				tc.check(t, dec)
			}
		})
	}
}

// ----------------------------------------------------------------------
// K-2 — prompt grounding: team config + NAV state + aggregates
// ----------------------------------------------------------------------

// TestBSideContextBuild_AggregatesTrades pins that the per-run
// context counts BUY/SELL/distinct-symbol/notional correctly.
// Without this aggregate the recap prompt would fall back to
// the truncated trade list only and the model wouldn't see
// "this run was 67% buy-heavy" headline stats.
func TestBSideContextBuild_AggregatesTrades(t *testing.T) {
	bsideCtx := sampleBSideContext()
	if bsideCtx.AggTradeCount != 3 {
		t.Errorf("trade count: want 3, got %d", bsideCtx.AggTradeCount)
	}
	if bsideCtx.AggBuyCount != 2 {
		t.Errorf("buy count: want 2, got %d", bsideCtx.AggBuyCount)
	}
	if bsideCtx.AggSellCount != 1 {
		t.Errorf("sell count: want 1, got %d", bsideCtx.AggSellCount)
	}
	if bsideCtx.AggSymbolDistinct != 2 {
		t.Errorf("symbol distinct: want 2 (AAA+BBB), got %d", bsideCtx.AggSymbolDistinct)
	}
	if bsideCtx.AggNotional <= 0 {
		t.Errorf("notional must be > 0, got %v", bsideCtx.AggNotional)
	}
}

// TestBSideContextGetNAVForDate_ExactAndFallback verifies both
// the O(1) exact-match index and the at-or-before linear fallback,
// which is what the trade prompt uses when a trade lands on a
// non-trading day or when there's a gap in the NAV history.
func TestBSideContextGetNAVForDate_ExactAndFallback(t *testing.T) {
	bsideCtx := sampleBSideContext()
	exact := bsideCtx.getNAVForDate(time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC))
	if exact == nil || exact.NAV != 1.0500 {
		t.Errorf("exact match for 2026-05-13 should be 1.05, got %+v", exact)
	}
	// 2026-05-13T20:30:00 — same day, after market close.
	// Should still hit 2026-05-13's bar via the index since
	// the key is normalized to the date.
	sameDay := bsideCtx.getNAVForDate(time.Date(2026, 5, 13, 20, 30, 0, 0, time.UTC))
	if sameDay == nil || sameDay.NAV != 1.0500 {
		t.Errorf("same-day late lookup must hit 1.05, got %+v", sameDay)
	}
	// Trade lands between two bars (e.g., a weekend). Should
	// resolve to the prior bar via the linear fallback.
	gap := bsideCtx.getNAVForDate(time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC).Add(-1 * time.Hour))
	if gap == nil || gap.NAV != 1.0250 {
		t.Errorf("at-or-before lookup must return 2026-05-12 (1.025), got %+v", gap)
	}
	// Before the first bar — must return nil so the prompt
	// degrades to "(暂无 NAV 历史)" rather than fabricating data.
	pre := bsideCtx.getNAVForDate(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	if pre != nil {
		t.Errorf("pre-history lookup must return nil, got %+v", pre)
	}
}

// TestBSideContextTrailingStats_ReportsDrawdown verifies that
// the trailing-window helper computes a non-trivial drawdown
// when the window contains a peak followed by a dip — which is
// exactly the case the prompt is supposed to surface.
func TestBSideContextTrailingStats_ReportsDrawdown(t *testing.T) {
	bsideCtx := sampleBSideContext()
	trail := bsideCtx.trailingNAVStats(time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC), 5)
	if !strings.Contains(trail, "回撤") {
		t.Errorf("trailing summary should mention drawdown, got %q", trail)
	}
	if !strings.Contains(trail, "近 ") {
		t.Errorf("trailing summary should be prefixed with 近 N 日, got %q", trail)
	}
	// Empty context yields empty output.
	empty := abBSideContext{}
	if empty.trailingNAVStats(time.Now(), 5) != "" {
		t.Error("empty context should produce empty trailing summary")
	}
}

// TestBSideContextTeamSummary_TruncatesLongTeams pins that the
// 8-member cap kicks in. A 50-agent team should not be allowed
// to overflow the prompt; when truncated, the summary must end
// with "..." so the model knows it's seeing a sample.
func TestBSideContextTeamSummary_TruncatesLongTeams(t *testing.T) {
	bsideCtx := sampleBSideContext()
	v := abShadowVariantRuntime{}
	for i := 0; i < 12; i++ {
		v.TeamSnapshot.Members = append(v.TeamSnapshot.Members, abTeamSnapshotMember{
			Role:      "researcher",
			Focus:     "factor",
			AgentName: "agent-" + strings.Repeat("x", i+1),
		})
	}
	summary := bsideCtx.teamSummary(v)
	if !strings.HasSuffix(summary, "...") {
		t.Errorf("expected truncation marker, got %q", summary)
	}
	// Hard cap: 8 members + the "..." sentinel = 9 segments.
	if got := strings.Count(summary, ", ") + 1; got != 9 {
		t.Errorf("expected 9 segments after truncation, got %d (summary=%q)", got, summary)
	}
	// Empty team gracefully reports "(空团队)".
	if got := bsideCtx.teamSummary(abShadowVariantRuntime{}); got != "(空团队)" {
		t.Errorf("empty team should produce 空团队, got %q", got)
	}
}

// TestBuildBSideTradePrompt_IncludesGroundingContext is the
// integration check that the full prompt — when assembled —
// carries every K-2 grounding signal: strategy config, team
// roster, NAV state, and trailing stats. Without this test it's
// easy to silently regress the prompt back to the K-1 shape.
func TestBuildBSideTradePrompt_IncludesGroundingContext(t *testing.T) {
	bsideCtx := sampleBSideContext()
	v := sampleVariantBWithTeam()
	trade := repository.TradeExecution{
		Symbol:    "TEST",
		Side:      "BUY",
		Quantity:  100,
		Price:     sql.NullFloat64{Float64: 50, Valid: true},
		CreatedAt: time.Date(2026, 5, 15, 9, 30, 0, 0, time.UTC),
	}
	prompt := buildBSideTradePrompt(v, trade, bsideCtx)
	for _, want := range []string{
		"B 组策略配置",
		"aggressive",       // strategy config field
		"alpha-pm",          // team agent name
		"researcher/factor", // role/focus pair
		"日期 NAV",
		"日收益",
		"近 ",
		"回撤",
		"TEST",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("trade prompt missing %q\n--- prompt ---\n%s", want, prompt)
		}
	}
}

// TestBuildBSideTradePrompt_FallsBackWhenNAVMissing pins that
// when the trade date has no NAV bar at-or-before it (e.g.,
// a brand-new fund), the prompt explicitly degrades rather
// than emitting a misleading 0/0 line. Future maintainers will
// know to keep the explicit "(暂无 NAV 历史)" copy because of
// this test.
func TestBuildBSideTradePrompt_FallsBackWhenNAVMissing(t *testing.T) {
	v := sampleVariantBWithTeam()
	trade := repository.TradeExecution{
		Symbol:    "EARLY",
		Side:      "BUY",
		Quantity:  10,
		Price:     sql.NullFloat64{Float64: 1, Valid: true},
		CreatedAt: time.Date(2026, 5, 1, 9, 30, 0, 0, time.UTC),
	}
	prompt := buildBSideTradePrompt(v, trade, abBSideContext{})
	if !strings.Contains(prompt, "暂无 NAV 历史") {
		t.Errorf("missing graceful NAV fallback, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "无足够样本") {
		t.Errorf("missing trailing-window fallback, got:\n%s", prompt)
	}
}

// TestBuildBSideRecapPrompt_IncludesNAVHeadlineAndAggregates is
// the recap counterpart: the end-of-run prompt must surface
// peak/trough/drawdown + the trade aggregate stats so the LLM
// summary can ground its narrative in real numbers rather than
// hallucinating a generic "B did well" recap.
func TestBuildBSideRecapPrompt_IncludesNAVHeadlineAndAggregates(t *testing.T) {
	bsideCtx := sampleBSideContext()
	v := sampleVariantBWithTeam()
	trades := []repository.TradeExecution{
		{Symbol: "AAA", Side: "BUY", Quantity: 100, Price: sql.NullFloat64{Float64: 10, Valid: true}, CreatedAt: time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)},
	}
	prompt := buildBSideRecapPrompt(v, trades, bsideCtx)
	for _, want := range []string{
		"评估窗口:",
		"NAV: 起",
		"峰",
		"谷",
		"最大回撤",
		"交易聚合: 共 3 笔",
		"BUY 2 / SELL 1",
		"alpha-pm", // team
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("recap prompt missing %q\n--- prompt ---\n%s", want, prompt)
		}
	}
}

// TestRecapNAVHeadline_HandlesEdgeCases pins three risk-of-NaN
// edge cases that shipped silently in K-1: empty NAV slice,
// single-bar history, and a starting NAV of 0 (corrupt). All
// must produce non-NaN, prompt-safe output.
func TestRecapNAVHeadline_HandlesEdgeCases(t *testing.T) {
	if got := recapNAVHeadline(nil); got != "" {
		t.Errorf("empty NAVs must produce empty headline, got %q", got)
	}
	one := []repository.NavSnapshot{{NAV: 1.05, TradingDate: time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)}}
	if got := recapNAVHeadline(one); !strings.Contains(got, "NAV: 起 1.0500") {
		t.Errorf("single-bar history must still render headline, got %q", got)
	}
	zeroStart := []repository.NavSnapshot{
		{NAV: 0, TradingDate: time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)},
		{NAV: 1.10, TradingDate: time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)},
	}
	got := recapNAVHeadline(zeroStart)
	if strings.Contains(got, "NaN") || strings.Contains(got, "+Inf") {
		t.Errorf("zero-start headline must not contain NaN/Inf, got %q", got)
	}
}

// TestPromptStrategyJSON_TruncatesLargeBlobs ensures a runaway
// custom strategy field (e.g., somebody dumped a 100KB CSV into
// strategy_config) cannot blow the prompt budget. The truncation
// marker is required so we can spot truncated runs in logs.
func TestPromptStrategyJSON_TruncatesLargeBlobs(t *testing.T) {
	cfg := map[string]any{"big": strings.Repeat("x", 4096)}
	got := promptStrategyJSON(cfg)
	if len(got) > 1700 {
		t.Errorf("truncated JSON should be ~1.5KB-ish, got %d bytes", len(got))
	}
	if !strings.HasSuffix(got, "...(truncated)") {
		t.Errorf("missing truncation marker, got tail: %q", got[len(got)-20:])
	}
	// Empty config returns canonical "{}".
	if promptStrategyJSON(nil) != "{}" {
		t.Errorf("nil config must serialise to {}")
	}
}

// ----------------------------------------------------------------------
// K-5 — `fundai_ab_shadow_llm_calls_total` outcome accounting
// ----------------------------------------------------------------------

// TestABShadow_MetricsCounter_HappyPathLabelsDecided pins that
// a clean LLM round-trip emits exactly one
// `decided_by_llm` increment. This is what dashboards count
// as "actual LLM spend".
func TestABShadow_MetricsCounter_HappyPathLabelsDecided(t *testing.T) {
	stub := &stubLLMClient{
		respContent: `{"skip": false, "quantity_scale": 1.0, "reasoning": "ok"}`,
	}
	rec := &recordedABShadowMetrics{}
	d, _ := newLLMBSideDecider(stub)
	d = d.WithMetrics(rec)
	if _, err := d.DecideTrade(context.Background(), sampleVariantBWithTeam(), sampleControlTrade(), sampleBSideContext()); err != nil {
		t.Fatalf("DecideTrade err: %v", err)
	}
	if got, want := rec.calls, []string{"decided_by_llm"}; !equalStrings(got, want) {
		t.Errorf("expected exactly [decided_by_llm], got %v", got)
	}
}

// TestABShadow_MetricsCounter_LLMErrorLabelsFallback verifies
// that a network error / timeout / refusal increments only
// `fallback_llm_error`. The fallback decider itself does NOT
// double-count — the counter is for LLM attempts, not for
// "decisions emitted".
func TestABShadow_MetricsCounter_LLMErrorLabelsFallback(t *testing.T) {
	stub := &stubLLMClient{respErr: errors.New("upstream 503")}
	rec := &recordedABShadowMetrics{}
	d, _ := newLLMBSideDecider(stub)
	d = d.WithMetrics(rec)
	if _, err := d.DecideTrade(context.Background(), sampleVariantBWithTeam(), sampleControlTrade(), sampleBSideContext()); err != nil {
		t.Fatalf("DecideTrade err: %v", err)
	}
	if got, want := rec.calls, []string{"fallback_llm_error"}; !equalStrings(got, want) {
		t.Errorf("expected exactly [fallback_llm_error], got %v", got)
	}
}

// TestABShadow_MetricsCounter_ParseErrorLabelsFallback covers
// the second class of LLM failure: "the model spoke but didn't
// give us valid JSON". Ops needs to distinguish this from a
// transport-layer error so the alert message can be different
// (parse errors usually mean the prompt or model-version drifted).
func TestABShadow_MetricsCounter_ParseErrorLabelsFallback(t *testing.T) {
	stub := &stubLLMClient{respContent: "I'm sorry I don't follow JSON instructions"}
	rec := &recordedABShadowMetrics{}
	d, _ := newLLMBSideDecider(stub)
	d = d.WithMetrics(rec)
	if _, err := d.DecideTrade(context.Background(), sampleVariantBWithTeam(), sampleControlTrade(), sampleBSideContext()); err != nil {
		t.Fatalf("DecideTrade err: %v", err)
	}
	if got, want := rec.calls, []string{"fallback_parse_error"}; !equalStrings(got, want) {
		t.Errorf("expected exactly [fallback_parse_error], got %v", got)
	}
}

// TestABShadow_MetricsCounter_BudgetCapLabelsBudget pins the
// most operationally interesting outcome: when the per-run cap
// kicks in, ops sees `fallback_budget_cap` rising. A spike in
// this label means `AB_SHADOW_LLM_MAX_CALLS` is too low for
// today's typical AB trade volume.
func TestABShadow_MetricsCounter_BudgetCapLabelsBudget(t *testing.T) {
	stub := &stubLLMClient{
		respContent: `{"skip": false, "quantity_scale": 1, "reasoning": "ok"}`,
	}
	rec := &recordedABShadowMetrics{}
	d, _ := newLLMBSideDecider(stub)
	d.maxLLMCalls = 1
	d = d.WithMetrics(rec)
	// First call burns the budget.
	if _, err := d.DecideTrade(context.Background(), sampleVariantBWithTeam(), sampleControlTrade(), sampleBSideContext()); err != nil {
		t.Fatalf("DecideTrade err: %v", err)
	}
	// Second call hits the cap.
	if _, err := d.DecideTrade(context.Background(), sampleVariantBWithTeam(), sampleControlTrade(), sampleBSideContext()); err != nil {
		t.Fatalf("DecideTrade err: %v", err)
	}
	if got, want := rec.calls, []string{"decided_by_llm", "fallback_budget_cap"}; !equalStrings(got, want) {
		t.Errorf("expected [decided_by_llm, fallback_budget_cap], got %v", got)
	}
}

// TestABShadow_MetricsCounter_RecapLabels covers the recap
// (end-of-run) path: it has its own outcome family
// (`recap_*`) so dashboards can break down "trade decisions"
// vs "learning recaps" separately, since their cost profiles
// and failure modes differ.
func TestABShadow_MetricsCounter_RecapLabels(t *testing.T) {
	cases := []struct {
		name       string
		stubResp   string
		stubErr    error
		wantCalls  []string
	}{
		{
			name:      "happy path → recap_decided_by_llm",
			stubResp:  `{"lessons":["l1"],"adjustments":["a1"],"summary":"s1"}`,
			wantCalls: []string{"recap_decided_by_llm"},
		},
		{
			name:      "llm error → recap_fallback_llm_error",
			stubErr:   errors.New("timeout"),
			wantCalls: []string{"recap_fallback_llm_error"},
		},
		{
			name:      "parse error → recap_fallback_parse_error",
			stubResp:  "this is not json",
			wantCalls: []string{"recap_fallback_parse_error"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubLLMClient{respContent: tc.stubResp, respErr: tc.stubErr}
			rec := &recordedABShadowMetrics{}
			d, _ := newLLMBSideDecider(stub)
			d = d.WithMetrics(rec)
			if _, err := d.SummarizeBLearning(context.Background(), sampleVariantBWithTeam(), nil, sampleBSideContext()); err != nil {
				t.Fatalf("SummarizeBLearning err: %v", err)
			}
			if !equalStrings(rec.calls, tc.wantCalls) {
				t.Errorf("calls mismatch: want %v, got %v", tc.wantCalls, rec.calls)
			}
		})
	}
}

// TestABShadow_MetricsCounter_NilRecorderIsSafe pins the
// nil-safety contract of the WithMetrics injector: passing nil
// must not panic in DecideTrade. Without this we'd ship a
// surprise NPE the first time someone built the decider through
// `newLLMBSideDecider` directly without calling WithMetrics.
func TestABShadow_MetricsCounter_NilRecorderIsSafe(t *testing.T) {
	stub := &stubLLMClient{
		respContent: `{"skip": false, "quantity_scale": 1, "reasoning": "ok"}`,
	}
	d, _ := newLLMBSideDecider(stub)
	// Default state — no WithMetrics ever called.
	if _, err := d.DecideTrade(context.Background(), sampleVariantBWithTeam(), sampleControlTrade(), sampleBSideContext()); err != nil {
		t.Fatalf("default-metrics DecideTrade err: %v", err)
	}
	// Explicit nil also OK.
	d = d.WithMetrics(nil)
	if _, err := d.DecideTrade(context.Background(), sampleVariantBWithTeam(), sampleControlTrade(), sampleBSideContext()); err != nil {
		t.Fatalf("nil-metrics DecideTrade err: %v", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
