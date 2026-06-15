// ab_shadow_bside.go is the Card-K extension point for the AB
// "shadow B-variant" execution path.
//
// Background
// ----------
//
// When AnalyzeTest fires for a `strategy_compare` AB test, the
// existing synthetic path mirrors the control fund's trades into
// the B variant scaled by an arithmetic `tradeScale` derived from
// the variant's strategy_config (see abStrategyTradeScale). This
// is deterministic and free, but the resulting decisions are NOT
// what an aggressive / conservative agent would actually have
// done — they are just multiplied versions of A's trades.
//
// Card K introduces a real LLM-driven path where, for each control
// trade, an LLM consultation answers "given B's strategy_config,
// what would B have done with this trade?". The LLM has the
// authority to:
//
//   - keep the trade as-is (qty_scale = 1)
//   - resize the trade (qty_scale != 1)
//   - skip the trade entirely (skip = true)
//   - in the future: emit a brand-new trade B would have made
//     instead (intentionally out of scope for K-1; the surface
//     stays "react to A's trade").
//
// Architecture
// ------------
//
// `abShadowBSideDecider` is the extension point. K-1 ships two
// impls:
//
//   - deterministicBSideDecider — wraps the legacy
//     abStrategyTradeScale / abStrategyReturnBias math.
//     This is the default and what existing tests pin.
//
//   - llmBSideDecider — calls llm.LLMClient with a compact prompt
//     per trade, parses a small JSON contract, and falls back to
//     deterministic on any error (parse, timeout, budget cap).
//     Gated behind AB_SHADOW_LLM_ENABLED=1 so deployments without
//     a configured LLM route degrade cleanly.
//
// Cost & latency notes
// --------------------
//
// AnalyzeTest can fan out hundreds of trades when the test window
// is long. Even at ~1k input tokens / 100 output tokens per trade,
// that's a few cents per analysis — small but not free. The LLM
// decider therefore:
//
//   - caps total LLM calls per Analyze run (maxLLMCalls field).
//   - falls back to deterministic for any trade beyond the cap.
//   - uses ModelTierLite by default; operators can set
//     AB_SHADOW_LLM_MODEL to override.
//   - prompts in Chinese, matching the existing `[auto-shadow]`
//     reasoning copy so the audit trail reads consistently.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/i18nmsg"
	"github.com/fundai/server/internal/llm"
	"github.com/fundai/server/internal/repository"
)

// abBSideDecision is the per-trade verdict returned by an
// abShadowBSideDecider. The fields are normalized (clipped to
// safe ranges, no NaNs) before being handed to the trade-writer.
type abBSideDecision struct {
	// Skip reports that B would not have made this trade. When
	// true the rest of the fields are ignored and no row goes
	// into ab_test_variant_trades for this iteration.
	Skip bool

	// QuantityScale is 1.0 when B mirrors A, < 1 when B sizes
	// down, > 1 when B sizes up. Always > 0 (we don't allow a
	// flipped sign — that would be a side change, which we don't
	// support yet at this surface).
	QuantityScale float64

	// SideOverride lets B flip BUY ↔ SELL. Empty string = mirror
	// A's side. Mostly used by the LLM path; the deterministic
	// path always returns "" here.
	SideOverride string

	// Reasoning is the human-readable note recorded into
	// ab_test_variant_trades.reasoning. Should explain why B
	// adjusted (or didn't) so the dashboard's "decision diffs"
	// timeline reads sensibly.
	Reasoning string
}

// abBSideContext carries the per-run grounding context that
// gives the LLM decider enough situational awareness to make
// non-trivial decisions. K-1 shipped without this — the LLM
// only saw "A made trade X, B has config Y", which is too thin
// to ground a real different decision. K-2 plumbs the run's
// NAV trajectory + team metadata + per-day NAV index through.
//
// All fields are read-only and pre-built by ensureABShadowExecution
// once per Analyze invocation.
type abBSideContext struct {
	// NAVs is the control fund's NAV trajectory over the test
	// window, oldest first. Used for both per-trade context
	// (the bar at the trade's date + a small trailing window)
	// and the end-of-run recap (start/end/peak/trough stats).
	// Empty when the fund has no NAV history yet.
	NAVs []repository.NavSnapshot

	// navByDate is a lazy index built on first use so
	// per-trade lookups are O(1). Populated by
	// abBSideContextBuild; nil-safe via getNAVForDate.
	navByDate map[string]*repository.NavSnapshot

	// AggTrade* fields summarise the control trade list for the
	// recap prompt. Cheaper than asking the LLM to sum N trades
	// itself. The summary is light enough to live on the
	// per-run struct without separate caching.
	AggTradeCount     int
	AggBuyCount       int
	AggSellCount      int
	AggSymbolDistinct int
	AggNotional       float64

	// TestWindowFrom / TestWindowTo are the analyze window's
	// bounds (UTC). Used in the recap prompt header so the
	// model anchors its narrative on the right horizon.
	TestWindowFrom time.Time
	TestWindowTo   time.Time
}

// abBSideContextBuild constructs the context once per analyze
// run. Pulled out so the unit tests can exercise the prompt
// builders directly.
func abBSideContextBuild(navs []repository.NavSnapshot, trades []repository.TradeExecution, from, to time.Time) abBSideContext {
	ctx := abBSideContext{
		NAVs:           navs,
		TestWindowFrom: from,
		TestWindowTo:   to,
	}
	if len(navs) > 0 {
		ctx.navByDate = make(map[string]*repository.NavSnapshot, len(navs))
		for i := range navs {
			key := navs[i].TradingDate.UTC().Format("2006-01-02")
			ctx.navByDate[key] = &navs[i]
		}
	}
	symbols := make(map[string]struct{}, len(trades))
	for _, t := range trades {
		ctx.AggTradeCount++
		side := strings.ToUpper(strings.TrimSpace(t.Side))
		if side == "BUY" {
			ctx.AggBuyCount++
		} else if side == "SELL" {
			ctx.AggSellCount++
		}
		if t.Symbol != "" {
			symbols[t.Symbol] = struct{}{}
		}
		ctx.AggNotional += abTradeNotional(t, abTradePrice(t))
	}
	ctx.AggSymbolDistinct = len(symbols)
	return ctx
}

// getNAVForDate returns the NAV bar for the given trading date,
// preferring an exact match and falling back to the most recent
// bar at-or-before the date. nil when the context has no NAV
// history at all (fund just launched).
func (c *abBSideContext) getNAVForDate(date time.Time) *repository.NavSnapshot {
	if c == nil || len(c.NAVs) == 0 {
		return nil
	}
	target := date.UTC().Format("2006-01-02")
	if c.navByDate != nil {
		if hit, ok := c.navByDate[target]; ok {
			return hit
		}
	}
	// Linear walk back. NAVs is small (test windows <= a few
	// hundred bars), so the cost is negligible.
	var best *repository.NavSnapshot
	for i := range c.NAVs {
		if c.NAVs[i].TradingDate.After(date) {
			break
		}
		best = &c.NAVs[i]
	}
	return best
}

// trailingNAVStats returns a compact textual hint about the
// trailing N bars before (and including) the given date. We
// summarise as "近 N 日: ret=±x%/d, drawdown=-y%" so the LLM
// can ground its decision in regime context.
//
// Returns "" when there's no usable history.
func (c *abBSideContext) trailingNAVStats(date time.Time, lookback int) string {
	if c == nil || len(c.NAVs) == 0 || lookback <= 0 {
		return ""
	}
	end := -1
	for i := range c.NAVs {
		if c.NAVs[i].TradingDate.After(date) {
			break
		}
		end = i
	}
	if end < 0 {
		return ""
	}
	start := end - lookback + 1
	if start < 0 {
		start = 0
	}
	bars := c.NAVs[start : end+1]
	if len(bars) == 0 {
		return ""
	}
	startNAV := bars[0].NAV
	endNAV := bars[len(bars)-1].NAV
	if startNAV <= 0 {
		startNAV = 1
	}
	cumRet := (endNAV/startNAV - 1) * 100
	peak := bars[0].NAV
	maxDD := 0.0
	for _, b := range bars {
		if b.NAV > peak {
			peak = b.NAV
		}
		if peak > 0 {
			dd := (b.NAV/peak - 1) * 100
			if dd < maxDD {
				maxDD = dd
			}
		}
	}
	return fmt.Sprintf("近 %d 日: 累计收益 %.2f%%, 最大回撤 %.2f%%", len(bars), cumRet, maxDD)
}

// teamSummary returns a compact one-line description of the
// agents on the variant's team. Only role + focus + name —
// system prompts are too long and would blow the token budget.
func (c *abBSideContext) teamSummary(variant abShadowVariantRuntime) string {
	if len(variant.TeamSnapshot.Members) == 0 {
		return "(空团队)"
	}
	parts := make([]string, 0, len(variant.TeamSnapshot.Members))
	for _, m := range variant.TeamSnapshot.Members {
		role := strings.TrimSpace(m.Role)
		if role == "" {
			role = "agent"
		}
		entry := role
		if m.Focus != "" {
			entry += "/" + m.Focus
		}
		if m.AgentName != "" {
			entry += "(" + m.AgentName + ")"
		}
		parts = append(parts, entry)
	}
	// Hard-cap so a 50-agent team can't blow the prompt budget.
	if len(parts) > 8 {
		parts = parts[:8]
		parts = append(parts, "...")
	}
	return strings.Join(parts, ", ")
}

// abShadowBSideDecider is the extension point. Implementations
// must be safe to call concurrently and must never panic on
// malformed input — the synthetic fallback path inside
// llmBSideDecider relies on this contract.
type abShadowBSideDecider interface {
	// DecideTrade returns B's response to a single control trade.
	// The variant argument carries B's strategy_config and team
	// snapshot so impls can read knobs like pmStyle /
	// maxSinglePosition / risk_appetite. The bsideCtx argument
	// (K-2) carries grounding context — NAV history, trailing
	// regime stats — so the LLM can make decisions that respect
	// the actual market state rather than just the strategy
	// labels.
	DecideTrade(ctx context.Context, variant abShadowVariantRuntime, controlTrade repository.TradeExecution, bsideCtx abBSideContext) (abBSideDecision, error)

	// SummarizeBLearning returns the lessons / adjustments that
	// the B variant's agents "learned" from this analyze run.
	// The synthetic impl returns canned copy; the LLM impl asks
	// the model to summarize once at the end of the run (cheaper
	// than per-trade and gives the model the full context).
	SummarizeBLearning(ctx context.Context, variant abShadowVariantRuntime, controlTrades []repository.TradeExecution, bsideCtx abBSideContext) (abBSideLearning, error)
}

// abBSideLearning is the per-run learning event for B. It
// translates 1:1 to the JSON columns in
// ab_test_agent_learning_events; the writer in
// writeABSyntheticLearningEvents marshals this into the
// adjustments/lessons/proposed_evolution_config columns.
type abBSideLearning struct {
	Lessons                 []string
	Adjustments             []string
	SpecializationLearning  string
	ProposedEvolutionConfig map[string]any
	Summary                 string
}

// ----------------------------------------------------------------------
// deterministicBSideDecider — the default, no-LLM path.
//
// Wraps the legacy abStrategyReturnBias / abStrategyTradeScale
// arithmetic so existing tests stay green. The DecideTrade impl
// returns a constant qty_scale per analyze run (computed from
// variant.StrategyConfig); SummarizeBLearning returns the same
// canned lessons the old writeABSyntheticLearningEvents did.
// ----------------------------------------------------------------------

type deterministicBSideDecider struct{}

func (deterministicBSideDecider) DecideTrade(_ context.Context, variant abShadowVariantRuntime, controlTrade repository.TradeExecution, _ abBSideContext) (abBSideDecision, error) {
	scale := abStrategyTradeScale(variant.StrategyConfig)
	if scale <= 0 {
		scale = 1
	}
	return abBSideDecision{
		Skip:          false,
		QuantityScale: scale,
		SideOverride:  "",
		Reasoning: fmt.Sprintf(
			"[auto-shadow] B 组根据策略参数进行影子执行，交易规模系数 %.2f。",
			scale,
		),
	}, nil
}

func (deterministicBSideDecider) SummarizeBLearning(_ context.Context, _ abShadowVariantRuntime, _ []repository.TradeExecution, _ abBSideContext) (abBSideLearning, error) {
	return abBSideLearning{
		Lessons: []string{
			"复盘影子交易结果，比较收益、回撤与换手差异",
			"实验策略在影子环境中形成独立学习结果，不污染真实 agent",
		},
		Adjustments: []string{
			"继续观察样本充分性后再决定是否提升到真实 agent",
			"若置信度充足，可通过 promotion 将学习结果合并或覆盖到真实 agent",
		},
		SpecializationLearning: "[auto-shadow] B variant deterministic path",
		Summary:                "[auto-shadow] 算术影子运行：根据 strategy_config 缩放 A 组交易得到 B 组的对照轨迹。",
	}, nil
}

// ----------------------------------------------------------------------
// llmBSideDecider — Card K's real LLM-driven path.
//
// For each control trade the decider builds a tiny system / user
// prompt pair, asks the LLM for a JSON verdict, and parses it.
// Errors fall back to the deterministic path so a transient LLM
// outage doesn't fail the whole AnalyzeTest.
// ----------------------------------------------------------------------

// abShadowMetricsRecorder is the narrow surface that the LLM
// decider needs from `serverMetrics`. Defining it as an
// interface lets the unit tests inject a recording stub without
// importing the heavyweight serverMetrics struct, and lets
// production wire the real `*serverMetrics` value.
//
// The single method matches `serverMetrics.RecordABShadowLLMCall`
// exactly; the noop implementation below is what gets used when
// the decider is constructed without a recorder (e.g. legacy
// callers that build through `newLLMBSideDecider` directly).
type abShadowMetricsRecorder interface {
	RecordABShadowLLMCall(outcome string)
}

type noopABShadowMetrics struct{}

func (noopABShadowMetrics) RecordABShadowLLMCall(string) {}

type llmBSideDecider struct {
	client       llm.LLMClient
	model        string         // optional explicit model override
	tier         llm.ModelTier  // model tier when model is empty
	maxLLMCalls  int            // hard cap; trades beyond this fall to deterministic
	timeout      time.Duration  // per-call deadline
	fallback     deterministicBSideDecider
	callsThisRun int // updated by DecideTrade; reset by SummarizeBLearning at run start
	metrics      abShadowMetricsRecorder // K-5 — never nil; defaults to noop
}

// newLLMBSideDecider builds a decider from a configured LLM
// client. Returns nil + error when the client is missing — the
// caller should drop back to deterministic in that case.
func newLLMBSideDecider(client llm.LLMClient) (*llmBSideDecider, error) {
	if client == nil {
		return nil, errors.New("ab shadow llm decider: nil llm client")
	}
	maxCalls := 50
	if v := strings.TrimSpace(os.Getenv("AB_SHADOW_LLM_MAX_CALLS")); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 && parsed <= 1000 {
			maxCalls = parsed
		}
	}
	tier := llm.TierStandard
	model := strings.TrimSpace(os.Getenv("AB_SHADOW_LLM_MODEL"))
	timeout := 12 * time.Second
	if v := strings.TrimSpace(os.Getenv("AB_SHADOW_LLM_TIMEOUT_SEC")); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed >= 1 && parsed <= 60 {
			timeout = time.Duration(parsed) * time.Second
		}
	}
	return &llmBSideDecider{
		client:      client,
		model:       model,
		tier:        tier,
		maxLLMCalls: maxCalls,
		timeout:     timeout,
		fallback:    deterministicBSideDecider{},
		metrics:     noopABShadowMetrics{},
	}, nil
}

// WithMetrics returns the same decider but with the supplied
// recorder injected. nil is treated as "use noop" so callers
// don't have to nil-check at the wiring layer. Method-on-pointer
// so the wired-once main.go can do
// `dec = dec.WithMetrics(serverMetrics)` without having to
// re-build the rest of the config.
func (d *llmBSideDecider) WithMetrics(recorder abShadowMetricsRecorder) *llmBSideDecider {
	if d == nil {
		return nil
	}
	if recorder == nil {
		d.metrics = noopABShadowMetrics{}
	} else {
		d.metrics = recorder
	}
	return d
}

// resetRun marks the start of a new AnalyzeTest run. Internal
// helper used by tests; the production path implicitly resets via
// SummarizeBLearning being called at the END of the run (the
// writer's contract).
func (d *llmBSideDecider) resetRun() {
	d.callsThisRun = 0
}

// DecideTrade — see abShadowBSideDecider. Falls back to
// deterministic on:
//
//   - LLM call cap reached for this run
//   - LLM call returns error (timeout, provider failover failed)
//   - JSON parse fails
//   - the parsed decision is degenerate (NaN qty_scale, etc.)
//
// We deliberately don't surface the LLM error up the stack — a
// strict failure mode would block AnalyzeTest entirely whenever
// the LLM has a hiccup, which is a much worse UX than "B looks
// like deterministic for a few minutes".
func (d *llmBSideDecider) DecideTrade(ctx context.Context, variant abShadowVariantRuntime, controlTrade repository.TradeExecution, bsideCtx abBSideContext) (abBSideDecision, error) {
	if d == nil || d.client == nil {
		return d.fallback.DecideTrade(ctx, variant, controlTrade, bsideCtx)
	}
	if d.callsThisRun >= d.maxLLMCalls {
		// Budget exhausted. Use deterministic, but tag the
		// reasoning so dashboards can see we hit the cap.
		d.metrics.RecordABShadowLLMCall("fallback_budget_cap")
		base, err := d.fallback.DecideTrade(ctx, variant, controlTrade, bsideCtx)
		if err != nil {
			return base, err
		}
		base.Reasoning = "[auto-shadow] LLM call budget reached, fell back to deterministic. " + base.Reasoning
		return base, nil
	}
	d.callsThisRun++

	prompt := buildBSideTradePrompt(variant, controlTrade, bsideCtx)
	callCtx := ctx
	if d.timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, d.timeout)
		defer cancel()
	}
	req := llm.ChatRequest{
		Messages: []llm.ChatMessage{
			{Role: "system", Content: bSideSystemPromptFor(i18nmsg.FromCtx(ctx))},
			{Role: "user", Content: prompt},
		},
	}
	if d.model != "" {
		req.Model = d.model
	} else {
		req.ModelTier = d.tier
	}

	resp, err := d.client.Chat(callCtx, req)
	if err != nil || resp == nil || strings.TrimSpace(resp.Content) == "" {
		d.metrics.RecordABShadowLLMCall("fallback_llm_error")
		return d.fallback.DecideTrade(ctx, variant, controlTrade, bsideCtx)
	}
	parsed, parseErr := parseBSideDecision(resp.Content)
	if parseErr != nil {
		d.metrics.RecordABShadowLLMCall("fallback_parse_error")
		return d.fallback.DecideTrade(ctx, variant, controlTrade, bsideCtx)
	}
	d.metrics.RecordABShadowLLMCall("decided_by_llm")
	parsed.Reasoning = "[auto-shadow][llm] " + strings.TrimSpace(parsed.Reasoning)
	return parsed, nil
}

func (d *llmBSideDecider) SummarizeBLearning(ctx context.Context, variant abShadowVariantRuntime, controlTrades []repository.TradeExecution, bsideCtx abBSideContext) (abBSideLearning, error) {
	d.callsThisRun = 0 // run boundary
	if d == nil || d.client == nil {
		return d.fallback.SummarizeBLearning(ctx, variant, controlTrades, bsideCtx)
	}

	prompt := buildBSideRecapPrompt(variant, controlTrades, bsideCtx)
	callCtx := ctx
	if d.timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, d.timeout)
		defer cancel()
	}
	req := llm.ChatRequest{
		Messages: []llm.ChatMessage{
			{Role: "system", Content: bSideRecapSystemPromptFor(i18nmsg.FromCtx(ctx))},
			{Role: "user", Content: prompt},
		},
	}
	if d.model != "" {
		req.Model = d.model
	} else {
		req.ModelTier = d.tier
	}
	resp, err := d.client.Chat(callCtx, req)
	if err != nil || resp == nil || strings.TrimSpace(resp.Content) == "" {
		d.metrics.RecordABShadowLLMCall("recap_fallback_llm_error")
		return d.fallback.SummarizeBLearning(ctx, variant, controlTrades, bsideCtx)
	}
	parsed, parseErr := parseBSideRecap(resp.Content)
	if parseErr != nil {
		d.metrics.RecordABShadowLLMCall("recap_fallback_parse_error")
		return d.fallback.SummarizeBLearning(ctx, variant, controlTrades, bsideCtx)
	}
	d.metrics.RecordABShadowLLMCall("recap_decided_by_llm")
	parsed.Summary = "[auto-shadow][llm] " + strings.TrimSpace(parsed.Summary)
	return parsed, nil
}

// ----------------------------------------------------------------------
// Prompt construction — kept here so the prompt copy is co-
// located with the decider that uses it.
// ----------------------------------------------------------------------

// bSideSystemPromptFor returns the locale-appropriate system prompt
// for the per-trade B-side decider. The two language variants are
// hand-authored (rather than runtime-translated) because the JSON
// schema lines and the field constraints are load-bearing — the
// LLM downstream parser pins them.
func bSideSystemPromptFor(loc i18nmsg.Locale) string {
	if loc == i18nmsg.LocaleEN {
		return `You are a rigorous shadow agent for a quant A/B experiment.
Task: read a real fund (variant A) trade, combine variant B's strategy config + team roles + the live market / NAV state, and infer how variant B would have decided this trade.
Return STRICT JSON only (no comments, no prefix/suffix):
{
  "skip": true|false,
  "quantity_scale": 0.0~3.0,
  "side_override": ""|"BUY"|"SELL",
  "reasoning": "<=80 words English explaining why variant B would decide this way"
}
Constraints:
- When variant B is conservative and drawdown is already deep, lower quantity_scale or skip.
- When variant B is aggressive and the trend is favourable, increase quantity_scale.
- Do NOT flip the side without explicit evidence; default side_override = "".
- reasoning must reference at least one piece of context (strategy / team / NAV / drawdown).
- Any out-of-range or missing field is treated as an error; ensure the JSON is valid.`
	}
	return `你是一名严谨的量化对照实验影子 agent。
任务：阅读真实基金（A 组）的一笔交易，结合 B 组的策略配置 + 团队角色 + 当下市场/NAV 状态，判断 B 组本次会如何决策。
只允许返回**严格的 JSON**（无注释、无前后缀）：
{
  "skip": true|false,
  "quantity_scale": 0.0~3.0,
  "side_override": ""|"BUY"|"SELL",
  "reasoning": "≤80字中文，解释 B 组为何如此决策"
}
约束：
- 当 B 策略偏保守且回撤已较深时，可降低 quantity_scale 或 skip。
- 当 B 策略激进且趋势顺时，可放大 quantity_scale。
- 不允许翻转方向除非明确证据；side_override 默认 ""。
- reasoning 必须引用至少一项上下文（策略 / 团队 / NAV / 回撤）。
- 任何字段越界或缺失会被视为错误，请确保 JSON 合法。`
}

// promptStrategyJSON marshals the strategy config and clips the
// JSON to a hard ceiling so a runaway custom field can't blow
// the prompt budget. We pick 1.5KB which is enough for any
// realistic config but small enough to leave room for the rest
// of the prompt within a 4-8K token window.
func promptStrategyJSON(cfg map[string]any) string {
	if len(cfg) == 0 {
		return "{}"
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "{}"
	}
	const maxBytes = 1536
	if len(raw) <= maxBytes {
		return string(raw)
	}
	return string(raw[:maxBytes]) + "...(truncated)"
}

func buildBSideTradePrompt(variant abShadowVariantRuntime, t repository.TradeExecution, bsideCtx abBSideContext) string {
	side := strings.ToUpper(strings.TrimSpace(t.Side))
	executed := t.CreatedAt
	if t.ExecutedAt.Valid {
		executed = t.ExecutedAt.Time
	}
	cfgJSON := promptStrategyJSON(variant.StrategyConfig)
	team := bsideCtx.teamSummary(variant)

	// NAV state at trade date — gives the LLM a sense of "we
	// were already down 8% this month, do we really want to
	// double down?". Falls back to "(暂无 NAV 历史)" when the
	// fund has no NAV bars before this trade.
	var navLine string
	if nav := bsideCtx.getNAVForDate(executed); nav != nil {
		navLine = fmt.Sprintf(
			"  日期 NAV: %.4f  日收益: %+.2f%%  累计收益: %+.2f%%  可用现金: %.0f",
			nav.NAV,
			nav.DailyReturn*100,
			nav.TotalReturn*100,
			nav.AvailableCash,
		)
	} else {
		navLine = "  日期 NAV: (暂无 NAV 历史)"
	}
	trail := bsideCtx.trailingNAVStats(executed, 5)
	if trail == "" {
		trail = "近 5 日: (无足够样本)"
	}

	return fmt.Sprintf(
		"B 组策略配置:\n%s\nB 组团队成员: %s\n\n实际交易（A 组）:\n  日期: %s\n  标的: %s\n  方向: %s\n  数量: %.2f\n  成交价: %.4f\n%s\n  %s\n\n请基于以上上下文返回 B 组的 JSON 决策。",
		cfgJSON,
		team,
		executed.Format("2006-01-02"),
		t.Symbol,
		side,
		t.Quantity,
		abTradePrice(t),
		navLine,
		trail,
	)
}

// bSideRecapSystemPromptFor returns the locale-appropriate recap system
// prompt. Same hand-authored / pin-with-tests reasoning as
// bSideSystemPromptFor.
func bSideRecapSystemPromptFor(loc i18nmsg.Locale) string {
	if loc == i18nmsg.LocaleEN {
		return `You are the shadow-learning recap engine of the AI fund platform.
Task: based on variant B's strategy config + team roles + variant A's real trade sequence + the full NAV trajectory, produce variant B's lessons for this shadow run.
Return STRICT JSON only:
{
  "lessons": ["<=20 words", "<=20 words"],
  "adjustments": ["<=25 words", "<=25 words"],
  "summary": "<=80 words English summary",
  "specialization_learning": "<=50 words",
  "proposed_evolution_config": { ...nested object, may be empty }
}
- lessons / adjustments each have 1-3 entries.
- summary MUST reference the NAV path (peak / trough / max drawdown) AND the trade aggregate stats.
- proposed_evolution_config is the diff proposal against variant B's config; emit {} when no change is warranted.`
	}
	return `你是 AI 基金平台的影子学习总结器。
任务：基于 B 组策略配置 + 团队角色 + A 组真实交易序列 + 全程 NAV 走势，给出 B 组本轮影子运行的学习要点。
只允许返回严格 JSON：
{
  "lessons": ["≤30字", "≤30字"],
  "adjustments": ["≤40字", "≤40字"],
  "summary": "≤120字 中文 总结",
  "specialization_learning": "≤80字",
  "proposed_evolution_config": { ...嵌套对象，可空 }
}
- lessons / adjustments 各 1~3 条
- summary 必须引用 NAV 走势（峰/谷/最大回撤之一）以及交易聚合统计。
- proposed_evolution_config 是 B 策略配置的差异提议；不必要时给 {}`
}

// recapNAVHeadline summarises the test window's NAV trajectory in
// one prompt-ready line: start/end NAV, peak, trough, total
// return, and max drawdown. Returns "" when no NAVs exist.
func recapNAVHeadline(navs []repository.NavSnapshot) string {
	if len(navs) == 0 {
		return ""
	}
	start := navs[0].NAV
	end := navs[len(navs)-1].NAV
	peak, peakIdx := start, 0
	trough, troughIdx := start, 0
	maxDD := 0.0
	for i, n := range navs {
		if n.NAV > peak {
			peak = n.NAV
			peakIdx = i
		}
		if n.NAV < trough {
			trough = n.NAV
			troughIdx = i
		}
		// Drawdown from running peak.
		runPeak := navs[0].NAV
		for j := 0; j <= i; j++ {
			if navs[j].NAV > runPeak {
				runPeak = navs[j].NAV
			}
		}
		if runPeak > 0 {
			dd := (n.NAV/runPeak - 1) * 100
			if dd < maxDD {
				maxDD = dd
			}
		}
	}
	if start <= 0 {
		start = 1
	}
	totalRet := (end/start - 1) * 100
	return fmt.Sprintf(
		"NAV: 起 %.4f → 终 %.4f (累计 %+.2f%%); 峰 %.4f@%s; 谷 %.4f@%s; 最大回撤 %.2f%%",
		navs[0].NAV, end, totalRet,
		peak, navs[peakIdx].TradingDate.Format("2006-01-02"),
		trough, navs[troughIdx].TradingDate.Format("2006-01-02"),
		maxDD,
	)
}

func buildBSideRecapPrompt(variant abShadowVariantRuntime, controlTrades []repository.TradeExecution, bsideCtx abBSideContext) string {
	cfgJSON := promptStrategyJSON(variant.StrategyConfig)
	// Truncate the trade list to keep the prompt cheap. The model
	// only needs a representative slice to summarize; we send up
	// to 30 trades head + tail.
	const maxRecap = 30
	visible := controlTrades
	if len(visible) > maxRecap {
		head := visible[:maxRecap/2]
		tail := visible[len(visible)-maxRecap/2:]
		merged := make([]repository.TradeExecution, 0, len(head)+len(tail))
		merged = append(merged, head...)
		merged = append(merged, tail...)
		visible = merged
	}
	rows := make([]string, 0, len(visible))
	for _, t := range visible {
		executed := t.CreatedAt
		if t.ExecutedAt.Valid {
			executed = t.ExecutedAt.Time
		}
		rows = append(rows, fmt.Sprintf("- %s %s %s 数量=%.2f 价=%.4f",
			executed.Format("2006-01-02"),
			strings.ToUpper(strings.TrimSpace(t.Side)),
			t.Symbol,
			t.Quantity,
			abTradePrice(t),
		))
	}

	team := bsideCtx.teamSummary(variant)
	navLine := recapNAVHeadline(bsideCtx.NAVs)
	if navLine == "" {
		navLine = "NAV: (本期无 NAV 历史)"
	}
	windowLine := "评估窗口: (未指定)"
	if !bsideCtx.TestWindowFrom.IsZero() && !bsideCtx.TestWindowTo.IsZero() {
		windowLine = fmt.Sprintf(
			"评估窗口: %s ~ %s (共 %d NAV 日)",
			bsideCtx.TestWindowFrom.Format("2006-01-02"),
			bsideCtx.TestWindowTo.Format("2006-01-02"),
			len(bsideCtx.NAVs),
		)
	}
	tradeAgg := fmt.Sprintf(
		"交易聚合: 共 %d 笔 (BUY %d / SELL %d), 涉及 %d 个标的, 名义额合计 %.2f",
		bsideCtx.AggTradeCount,
		bsideCtx.AggBuyCount,
		bsideCtx.AggSellCount,
		bsideCtx.AggSymbolDistinct,
		bsideCtx.AggNotional,
	)

	return fmt.Sprintf(
		"B 组策略配置:\n%s\nB 组团队成员: %s\n\n%s\n%s\n%s\n\n本期 A 组交易序列（最多展示 %d 条）:\n%s\n请输出 JSON 总结。",
		cfgJSON,
		team,
		windowLine,
		navLine,
		tradeAgg,
		maxRecap,
		strings.Join(rows, "\n"),
	)
}

// ----------------------------------------------------------------------
// JSON parsers — strict on shape, tolerant on trailing whitespace
// or markdown fencing (some providers wrap the response in
// ```json ... ``` even when told not to).
// ----------------------------------------------------------------------

// stripJSONFence removes a leading/trailing ```json``` fence the
// LLM might add despite system instructions. Idempotent on
// already-clean strings.
func stripJSONFence(s string) string {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "```") {
		// Drop the first line up to the first newline.
		nl := strings.Index(t, "\n")
		if nl > 0 {
			t = t[nl+1:]
		}
	}
	if strings.HasSuffix(t, "```") {
		t = strings.TrimSuffix(t, "```")
	}
	return strings.TrimSpace(t)
}

func parseBSideDecision(raw string) (abBSideDecision, error) {
	var payload struct {
		Skip          bool    `json:"skip"`
		QuantityScale float64 `json:"quantity_scale"`
		SideOverride  string  `json:"side_override"`
		Reasoning     string  `json:"reasoning"`
	}
	if err := json.Unmarshal([]byte(stripJSONFence(raw)), &payload); err != nil {
		return abBSideDecision{}, err
	}
	if payload.Skip {
		return abBSideDecision{Skip: true, Reasoning: strings.TrimSpace(payload.Reasoning)}, nil
	}
	scale := payload.QuantityScale
	if scale <= 0 || math.IsNaN(scale) || math.IsInf(scale, 0) {
		return abBSideDecision{}, errors.New("ab bside: invalid quantity_scale")
	}
	// Clip to sane bounds. The system prompt asks for 0..3 but we
	// hard-clip in case the LLM ignores us.
	scale = math.Min(3, math.Max(0.05, scale))
	side := strings.ToUpper(strings.TrimSpace(payload.SideOverride))
	if side != "" && side != "BUY" && side != "SELL" {
		side = ""
	}
	reasoning := strings.TrimSpace(payload.Reasoning)
	if reasoning == "" {
		reasoning = "B 组决策（LLM）"
	}
	return abBSideDecision{
		Skip:          false,
		QuantityScale: scale,
		SideOverride:  side,
		Reasoning:     reasoning,
	}, nil
}

func parseBSideRecap(raw string) (abBSideLearning, error) {
	var payload struct {
		Lessons                 []string       `json:"lessons"`
		Adjustments             []string       `json:"adjustments"`
		Summary                 string         `json:"summary"`
		SpecializationLearning  string         `json:"specialization_learning"`
		ProposedEvolutionConfig map[string]any `json:"proposed_evolution_config"`
	}
	if err := json.Unmarshal([]byte(stripJSONFence(raw)), &payload); err != nil {
		return abBSideLearning{}, err
	}
	if len(payload.Lessons) == 0 && len(payload.Adjustments) == 0 && strings.TrimSpace(payload.Summary) == "" {
		return abBSideLearning{}, errors.New("ab bside recap: empty payload")
	}
	if payload.ProposedEvolutionConfig == nil {
		payload.ProposedEvolutionConfig = map[string]any{}
	}
	return abBSideLearning{
		Lessons:                 payload.Lessons,
		Adjustments:             payload.Adjustments,
		Summary:                 strings.TrimSpace(payload.Summary),
		SpecializationLearning:  strings.TrimSpace(payload.SpecializationLearning),
		ProposedEvolutionConfig: payload.ProposedEvolutionConfig,
	}, nil
}
