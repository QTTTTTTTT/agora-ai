// Unit tests for the per-trading-day self-learning helpers added in
// Steps A/D. None of these touch the real LLM — they exercise the
// pure helpers `shouldGenerateLLMLessons` (the gate that decides
// whether to spend an LLM call) and `parseAgentLessonsResponse` (the
// JSON sanitiser that consumes the model's reply).
package main

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/fundai/server/internal/repository"
)

// dummyLLMRuntime is a non-nil llmRuntime placeholder for the gate
// test. The gate only checks `m.llmRuntime != nil`; it never calls
// Chat. We use a sentinel value because creating a real llmRuntime
// requires a DB, config, and registered providers — overkill for a
// pure-predicate test.
var dummyLLMRuntime = &llmRuntime{}

// Gate must return false when the runtime isn't wired (nil) so unit
// tests and dev environments don't accidentally try to call the
// model.
func TestShouldGenerateLLMLessonsRequiresRuntime(t *testing.T) {
	m := &runtimeMemorySystem{llmRuntime: nil}
	ctx := &learningContext{}
	if m.shouldGenerateLLMLessons(ctx, tradeSummary{filled: 5}, 0.05) {
		t.Fatal("nil runtime should always gate out")
	}
}

// Gate must return false when no signal at all: no PnL, no trades,
// no executing actions. This is the templates-are-equivalent case
// and we don't want to burn a token to re-produce the same generic
// daily-review boilerplate.
func TestShouldGenerateLLMLessonsSkipsQuietDay(t *testing.T) {
	m := &runtimeMemorySystem{llmRuntime: dummyLLMRuntime}
	ctx := &learningContext{
		actions: []repository.PlanAction{
			{Action: "watch"},
			{Action: "hold"},
		},
	}
	if m.shouldGenerateLLMLessons(ctx, tradeSummary{}, 0) {
		t.Fatal("quiet day with only watch/hold actions should gate out")
	}
}

// The gate fires on any of: PnL ≠ 0, executed trades, rejected
// trades, or an executing action verb in the plan. We test each
// trigger independently so a regression in one doesn't get masked
// by the others.
func TestShouldGenerateLLMLessonsFiresOnEachSignal(t *testing.T) {
	m := &runtimeMemorySystem{llmRuntime: dummyLLMRuntime}

	cases := []struct {
		name        string
		ctx         *learningContext
		stats       tradeSummary
		dailyReturn float64
	}{
		{"dailyReturn positive", &learningContext{}, tradeSummary{}, 0.012},
		{"dailyReturn negative", &learningContext{}, tradeSummary{}, -0.008},
		{"filled trade", &learningContext{}, tradeSummary{filled: 1}, 0},
		{"partial trade", &learningContext{}, tradeSummary{partial: 1}, 0},
		{"rejected trade", &learningContext{}, tradeSummary{rejected: 1}, 0},
		{"buy action only", &learningContext{actions: []repository.PlanAction{{Action: "BUY"}}}, tradeSummary{}, 0},
		{"reduce action only", &learningContext{actions: []repository.PlanAction{{Action: "reduce"}}}, tradeSummary{}, 0},
		{
			// Intraday cadence regression: selected plan is a
			// late-day watch-only tick (no executing actions, no
			// plan-scoped trades) but earlier ticks on the SAME
			// trading day produced real fills. fundDayTradeCount
			// must be enough to fire the gate.
			"fund-wide day trades only",
			&learningContext{
				fundDayTradeCount: 3,
				actions:           []repository.PlanAction{{Action: "watch"}},
			},
			tradeSummary{},
			0,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if !m.shouldGenerateLLMLessons(tc.ctx, tc.stats, tc.dailyReturn) {
				t.Errorf("expected gate to fire for %s", tc.name)
			}
		})
	}
}

// Happy path: the model returns clean JSON with the expected shape.
// Output must be trimmed, capped, and survive the sentence-tail
// check.
func TestParseAgentLessonsResponseHappyPath(t *testing.T) {
	raw := `{
		"lessons": [
			"组合经理今日仓位维持稳定，未追加 688205 的高位买入是正确选择。",
			"688205 当日收益 -0.12%，需要结合资金流走向校验持仓节奏。"
		],
		"adjustments": [
			"次日开盘前确认 688205 融资客买入是否持续。",
			"对计划中的 watch 类标的设定明确的转 buy 触发价格。"
		]
	}`
	lessons, adjustments, err := parseAgentLessonsResponse(raw, 3)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(lessons) != 2 {
		t.Fatalf("lessons = %d, want 2", len(lessons))
	}
	if len(adjustments) != 2 {
		t.Fatalf("adjustments = %d, want 2", len(adjustments))
	}
	if !strings.HasSuffix(lessons[0], "。") {
		t.Errorf("first lesson should end with 。, got %q", lessons[0])
	}
}

// Some models wrap output in ```json fences or include a preamble
// sentence — we slice between first '{' and last '}' so the parse
// still succeeds.
func TestParseAgentLessonsResponseHandlesFencesAndPreamble(t *testing.T) {
	raw := "Here is the JSON you asked for:\n```json\n" +
		`{"lessons":["688205 今日成交一笔 49984 元，仓位扩张到约 5%。"],` +
		`"adjustments":["明日复核 688205 的 ATR 是否突破止损边界。"]}` +
		"\n```"
	lessons, adjustments, err := parseAgentLessonsResponse(raw, 3)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(lessons) != 1 || len(adjustments) != 1 {
		t.Fatalf("expected 1+1 strings, got %d+%d", len(lessons), len(adjustments))
	}
}

// Fragments that don't end with a sentence-final mark must be dropped
// — they're usually a sign the model ran out of token budget and
// truncated mid-thought. Better to fall back to templates than to
// show "688205 当日的执行节奏" as a "lesson".
func TestParseAgentLessonsResponseDropsTruncatedFragments(t *testing.T) {
	raw := `{
		"lessons": [
			"688205 当日的执行节奏",
			"688205 当日仓位维持 5%，回撤 0.12%，符合风控预期。"
		],
		"adjustments": [
			"明日"
		]
	}`
	lessons, _, err := parseAgentLessonsResponse(raw, 3)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(lessons) != 1 {
		t.Fatalf("expected exactly 1 lesson (fragment dropped), got %d", len(lessons))
	}
	if !strings.Contains(lessons[0], "5%") {
		t.Errorf("kept lesson should be the substantive one, got %q", lessons[0])
	}
}

// Malformed responses (no JSON object at all) must surface as an
// error so the caller falls back to the role templates instead of
// persisting garbage.
func TestParseAgentLessonsResponseRejectsNonJSON(t *testing.T) {
	if _, _, err := parseAgentLessonsResponse("I cannot help with that.", 3); err == nil {
		t.Fatal("expected error on non-JSON response")
	}
}

// ExistsByFundAgentLayerDate dedupe contract — we don't need a real
// DB here, but we DO want the helper's NullString handling to be
// covered. Use a hand-rolled smoke via the sql package to ensure
// we don't accidentally accept `agentID.Valid=true` with an empty
// string and silently match the fund-level (IS NULL) branch.
func TestNullUUIDStripsEmptyString(t *testing.T) {
	got := nullUUID("   ")
	if got.Valid {
		t.Errorf("blank string should produce NullString{Valid:false}, got %+v", got)
	}
	got = nullUUID(" abc ")
	if !got.Valid || got.String != "abc" {
		t.Errorf("nullUUID should trim and keep value; got %+v", got)
	}
	_ = sql.NullString{} // touch sql package
}
