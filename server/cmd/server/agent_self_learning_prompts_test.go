package main

// Unit tests for the role-specific self-learning prompt builders.
// These pin two contracts:
//
//  1. Each role's prompt body anchors on a DIFFERENT fact subset
//     (PM = allocation, Researcher = focus, Trader = execution
//     micro-structure, Risk = concentration + reject signals).
//     Without this, the daily LLM call would produce four nearly
//     identical lessons across the team, which was the bug that
//     prompted this refactor.
//
//  2. Researcher focus isolation works: when focus contains
//     ticker-shaped tokens (e.g. "688205, 300552"), the prompt
//     shows ONLY those positions / actions — a baijiu position is
//     not surfaced to a semiconductor researcher.
//
// Tests use minimal stub data structures (no DB) and assert on
// string content of the rendered prompt. We deliberately don't
// snapshot the entire prompt — too brittle — and instead check that
// the discriminating substrings are present / absent per role.

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/repository"
)

// buildLearningCtx is the shared test fixture: one fund, a NAV row,
// a plan with reasoning, three holdings (one A-share, one growth
// chinese name, one US ticker), three actions, and three trades with
// mixed status + slippage + cancel reason. Every role-specific test
// runs against the SAME fixture so the only thing varying is the
// role string.
func buildLearningCtx() *learningContext {
	totalAssets := 1_000_000.0
	nav := &repository.NavSnapshot{
		TotalAssets: totalAssets,
		DailyReturn: -0.005,
	}
	plan := &repository.InvestmentPlan{
		Status: "completed",
		Reasoning: sql.NullString{
			Valid:  true,
			String: "今日聚焦半导体板块，688205 因 CPO 出货预期向上调仓，300552 维持观察",
		},
	}
	actions := []repository.PlanAction{
		{
			Symbol:          "688205",
			Action:          "buy",
			Amount:          sql.NullFloat64{Valid: true, Float64: 25_000},
			ExecutionStatus: "filled",
		},
		{
			Symbol:          "300552",
			Action:          "watch",
			Amount:          sql.NullFloat64{Valid: true, Float64: 0},
			ExecutionStatus: "skipped",
		},
		{
			Symbol:          "NVDA",
			Action:          "reduce",
			Amount:          sql.NullFloat64{Valid: true, Float64: 12_000},
			ExecutionStatus: "partial",
		},
	}
	trades := []repository.TradeExecution{
		{
			Symbol:       "688205",
			Side:         "buy",
			Quantity:     100,
			Price:        sql.NullFloat64{Valid: true, Float64: 233.40},
			FilledPrice:  sql.NullFloat64{Valid: true, Float64: 233.65},
			SlippagePct:  sql.NullFloat64{Valid: true, Float64: 0.00107},
			Status:       "filled",
			CancelReason: sql.NullString{},
		},
		{
			Symbol:       "NVDA",
			Side:         "sell",
			Quantity:     50,
			Price:        sql.NullFloat64{Valid: true, Float64: 142.50},
			FilledPrice:  sql.NullFloat64{Valid: true, Float64: 142.55},
			SlippagePct:  sql.NullFloat64{Valid: true, Float64: 0.00035},
			Status:       "partial",
			CancelReason: sql.NullString{Valid: true, String: "ttl"},
		},
		{
			Symbol:       "600519",
			Side:         "buy",
			Quantity:     10,
			Price:        sql.NullFloat64{Valid: true, Float64: 1500},
			Status:       "rejected",
			CancelReason: sql.NullString{Valid: true, String: "lot_size"},
		},
	}
	positions := []repository.HoldingPosition{
		{Symbol: "688205", MarketValue: 64_000},
		{Symbol: "300552", MarketValue: 52_000},
		{Symbol: "600519", MarketValue: 230_000}, // baijiu — should NOT appear in semiconductor researcher's view
		{Symbol: "NVDA", MarketValue: 41_000},
	}
	return &learningContext{
		fund:        &repository.Fund{ID: "fund-1", Name: "OCS-Selection"},
		tradingDate: time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC),
		nav:         nav,
		plan:        plan,
		actions:     actions,
		trades:      trades,
		positions:   positions,
	}
}

func tradeStatsFromCtx(ctx *learningContext) tradeSummary {
	return summarizeTrades(ctx.actions, ctx.trades)
}

// ---------------------------------------------------------------------------
// Per-role discrimination
// ---------------------------------------------------------------------------

// TestRoleSpecificBody_PMShowsAllocationNotExecutionDetail pins that
// the PM prompt is anchored on plan completion + top holdings + plan
// reasoning. It must NOT contain trader-only details (slippage bps,
// per-trade cancel reason histogram) because the PM doesn't reflect
// on those.
func TestRoleSpecificBody_PMShowsAllocationNotExecutionDetail(t *testing.T) {
	ctx := buildLearningCtx()
	body := buildRoleSpecificLearningBody("pm", "", nil, ctx, tradeStatsFromCtx(ctx), ctx.nav.DailyReturn)

	mustContain(t, body, "[组合层视角]")
	mustContain(t, body, "Plan status: completed")
	mustContain(t, body, "Plan completion:")          // PM-specific computed metric
	mustContain(t, body, "688205")                    // top holding makes it in
	mustContain(t, body, "Top holdings (symbol, weight of NAV):")
	mustContain(t, body, "Plan actions:")
	// PM-specific anchor line at the end of the body.
	mustContain(t, body, "组合经理 (PM) 视角")
	// Must NOT contain per-trade execution micro-structure.
	mustNotContain(t, body, "slippage=")
	mustNotContain(t, body, "Reject / cancel reason breakdown:")
}

// TestRoleSpecificBody_ResearcherFocusIsolation is the headline
// behaviour: a researcher who covers "688205, 300552" must NOT see
// the 600519 (baijiu) position or NVDA action in their prompt.
func TestRoleSpecificBody_ResearcherFocusIsolation(t *testing.T) {
	ctx := buildLearningCtx()
	body := buildRoleSpecificLearningBody(
		"researcher",
		"688205, 300552",
		nil,
		ctx,
		tradeStatsFromCtx(ctx),
		ctx.nav.DailyReturn,
	)

	mustContain(t, body, "[研究方向视角]")
	mustContain(t, body, "Focus 解析为 2 个聚焦标的")
	mustContain(t, body, "688205")
	mustContain(t, body, "300552")
	mustContain(t, body, "聚焦标的当前持仓")
	mustContain(t, body, "聚焦标的今日计划动作")
	// The "not in focus" symbols must be absent — researcher
	// isolation is the whole point.
	mustNotContain(t, body, "600519")
	mustNotContain(t, body, "NVDA")
	// Plan reasoning keyword counter wired up.
	mustContain(t, body, "Plan reasoning 中提到 focus 关键词:")
	// Anchor line cites the focus string verbatim.
	mustContain(t, body, "focus=688205, 300552")
}

// TestRoleSpecificBody_ResearcherStructuredCoverageBeatsFocus pins
// migration-087 behaviour: when the fund_team_member_specialization
// row exists, its `instruments[]` overrides whatever the legacy
// focus string says. The OCS-fund kind of case where focus is the
// 3-value category string ("stock" / "fundamental" / "macro") —
// which extractFocusSymbols can never produce ticker tokens from —
// is exactly the situation where the structured path saves us.
//
// Concretely: focus="fundamental" (yields zero focus tokens via
// the legacy heuristic) PLUS coverage=["688205","300552"] must
// still isolate the researcher's view to those two symbols.
func TestRoleSpecificBody_ResearcherStructuredCoverageBeatsFocus(t *testing.T) {
	ctx := buildLearningCtx()
	body := buildRoleSpecificLearningBody(
		"researcher",
		// `focus` deliberately set to a value the legacy
		// extractFocusSymbols cannot extract tickers from —
		// matches the production fund_team_members.focus CHECK
		// constraint ('stock' / 'fundamental' / 'macro').
		"fundamental",
		[]string{"688205", "300552"},
		ctx,
		tradeStatsFromCtx(ctx),
		ctx.nav.DailyReturn,
	)
	mustContain(t, body, "[研究方向视角]")
	// Coverage source label proves the structured branch fired.
	mustContain(t, body, "结构化覆盖 (specialization)")
	mustContain(t, body, "Focus 解析为 2 个聚焦标的")
	mustContain(t, body, "688205")
	mustContain(t, body, "300552")
	// Out-of-coverage symbols must NOT leak in.
	mustNotContain(t, body, "600519")
	mustNotContain(t, body, "NVDA")
}

// TestRoleSpecificBody_ResearcherEmptyCoverageFallsBackToFocus
// confirms the fallback direction: when coverage[] is empty
// (the team-member has no specialization row yet), the builder
// falls through to the legacy focus-string heuristic exactly
// as before. Guards against accidentally silencing the
// fallback path during the 087 rollout.
func TestRoleSpecificBody_ResearcherEmptyCoverageFallsBackToFocus(t *testing.T) {
	ctx := buildLearningCtx()
	body := buildRoleSpecificLearningBody(
		"researcher",
		"688205, 300552",
		[]string{}, // explicit empty = "no specialization row"
		ctx,
		tradeStatsFromCtx(ctx),
		ctx.nav.DailyReturn,
	)
	mustContain(t, body, "[研究方向视角]")
	mustContain(t, body, "legacy focus 字符串")
	mustContain(t, body, "Focus 解析为 2 个聚焦标的")
	mustContain(t, body, "688205")
}

// TestRoleSpecificBody_ResearcherFallbackOnThemeFocus covers the
// theme-shaped focus path (no ticker tokens). The prompt should
// fall back to the team-wide holdings + a note that focus didn't
// resolve to tickers — and explicitly tell the model to reason
// about theme alignment.
func TestRoleSpecificBody_ResearcherFallbackOnThemeFocus(t *testing.T) {
	ctx := buildLearningCtx()
	body := buildRoleSpecificLearningBody(
		"researcher",
		"半导体",
		nil,
		ctx,
		tradeStatsFromCtx(ctx),
		ctx.nav.DailyReturn,
	)
	mustContain(t, body, "[研究方向视角]")
	mustContain(t, body, "Focus 未解析出具体 ticker")
	// Theme-shaped fallback shows team-wide holdings (the
	// previously-isolated 600519 / NVDA come back in).
	mustContain(t, body, "Top holdings (symbol, weight of NAV):")
	mustContain(t, body, "focus=半导体")
}

// TestRoleSpecificBody_TraderShowsSlippageAndRejects pins the
// execution-micro-structure block: per-trade slippage bps, cancel
// reason histogram, no holdings table.
func TestRoleSpecificBody_TraderShowsSlippageAndRejects(t *testing.T) {
	ctx := buildLearningCtx()
	body := buildRoleSpecificLearningBody("trader", "", nil, ctx, tradeStatsFromCtx(ctx), ctx.nav.DailyReturn)

	mustContain(t, body, "[执行层视角]")
	mustContain(t, body, "Per-trade execution detail:")
	// Slippage on the filled and partial trades (both have it set).
	mustContain(t, body, "slippage=10.7bps") // 0.00107 → 10.7 bps
	mustContain(t, body, "slippage=3.5bps")  // 0.00035 → 3.5 bps
	// Reject breakdown with both reasons present.
	mustContain(t, body, "Reject / cancel reason breakdown:")
	mustContain(t, body, "lot_size: 1")
	mustContain(t, body, "ttl: 1")
	// No holdings list — that's PM / Risk territory.
	mustNotContain(t, body, "Top holdings (symbol, weight of NAV):")
	mustContain(t, body, "交易员视角")
}

// TestRoleSpecificBody_RiskShowsConcentrationAndGateSignals — risk
// gets max-single + top-5 weight sum + reject reason histogram, NOT
// the per-trade slippage detail. The drawdown line should fire when
// daily return is negative.
func TestRoleSpecificBody_RiskShowsConcentrationAndGateSignals(t *testing.T) {
	ctx := buildLearningCtx()
	body := buildRoleSpecificLearningBody("risk", "", nil, ctx, tradeStatsFromCtx(ctx), ctx.nav.DailyReturn)

	mustContain(t, body, "[风控视角]")
	mustContain(t, body, "Max single position: 600519") // baijiu is the biggest
	mustContain(t, body, "Top-5 weight sum:")
	mustContain(t, body, "Risk gate signals")
	mustContain(t, body, "lot_size: 1")
	// Drawdown line because daily return is negative in the fixture.
	mustContain(t, body, "Today's drawdown:")
	// No per-trade slippage detail.
	mustNotContain(t, body, "Per-trade execution detail:")
	mustContain(t, body, "风险控制视角")
}

// TestRoleSpecificBody_AllFourRolesDifferAcrossSameDay is the
// belt-and-braces invariant: same learningContext, four roles, four
// distinct prompt bodies. Without this, a future refactor that
// accidentally makes two roles share a code path won't be caught
// by the per-role substring tests above.
func TestRoleSpecificBody_AllFourRolesDifferAcrossSameDay(t *testing.T) {
	ctx := buildLearningCtx()
	stats := tradeStatsFromCtx(ctx)
	dr := ctx.nav.DailyReturn

	bodies := map[string]string{
		"pm":         buildRoleSpecificLearningBody("pm", "", nil, ctx, stats, dr),
		"researcher": buildRoleSpecificLearningBody("researcher", "688205, 300552", nil, ctx, stats, dr),
		"trader":     buildRoleSpecificLearningBody("trader", "", nil, ctx, stats, dr),
		"risk":       buildRoleSpecificLearningBody("risk", "", nil, ctx, stats, dr),
	}
	// All-pairs distinctness check: 6 pairs across 4 roles.
	roles := []string{"pm", "researcher", "trader", "risk"}
	for i := 0; i < len(roles); i++ {
		for j := i + 1; j < len(roles); j++ {
			a := bodies[roles[i]]
			b := bodies[roles[j]]
			if a == b {
				t.Errorf("role bodies must differ; %s == %s", roles[i], roles[j])
			}
		}
	}
}

// ---------------------------------------------------------------------------
// System hint discrimination
// ---------------------------------------------------------------------------

// TestRoleSpecificSystemHint_PerRoleHasDistinctAnchor pins that
// each role's system hint mentions the role's anchor concepts.
// This is the second half of the discrimination contract — the
// user body picks the facts, the system hint picks the lens.
func TestRoleSpecificSystemHint_PerRoleHasDistinctAnchor(t *testing.T) {
	cases := []struct {
		role string
		want []string
	}{
		{"pm", []string{"组合配置", "套件权重", "计划完成度"}},
		{"researcher", []string{"focus 视角"}},
		{"trader", []string{"成交率", "滑点", "拒单"}},
		{"risk", []string{"集中度", "风险预算", "拒单边界"}},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			hint := roleSpecificSystemHint(tc.role)
			for _, w := range tc.want {
				if !strings.Contains(hint, w) {
					t.Errorf("%s hint missing %q; got: %s", tc.role, w, hint)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// extractFocusSymbols
// ---------------------------------------------------------------------------

// TestExtractFocusSymbols pins the focus-token extractor. The
// researcher isolation block depends on this — if it stops
// recognising "688205" as a ticker token, the filter falls through
// and the researcher sees everything.
func TestExtractFocusSymbols(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantAll []string
		wantNot []string
	}{
		{"empty", "", nil, []string{"688205"}},
		{"comma separated A-share", "688205, 300552", []string{"688205", "300552"}, nil},
		{"slash separated US tickers", "NVDA / AAPL / MSFT", []string{"NVDA", "AAPL", "MSFT"}, nil},
		{"theme + ticker mix", "CPO 半导体 688205", []string{"688205"}, []string{"半导体"}},
		{"hk dotted ticker", "0700.HK, 9988.HK", []string{"0700.HK", "9988.HK"}, nil},
		{"crypto colon prefix", "BINANCE:BTCUSDT, ETHUSDT", []string{"BINANCE:BTCUSDT", "ETHUSDT"}, nil},
		{"theme only", "半导体 AI infra", nil, []string{"半导体"}},
		{"deduped", "688205, 688205, 688205", []string{"688205"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractFocusSymbols(tc.input)
			for _, w := range tc.wantAll {
				if !containsString(got, w) {
					t.Errorf("extractFocusSymbols(%q) = %v; want to contain %q", tc.input, got, w)
				}
			}
			for _, n := range tc.wantNot {
				if containsString(got, n) {
					t.Errorf("extractFocusSymbols(%q) = %v; must NOT contain %q", tc.input, got, n)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected body to contain %q; body:\n%s", needle, haystack)
	}
}

func mustNotContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Errorf("expected body to NOT contain %q; body:\n%s", needle, haystack)
	}
}

func containsString(haystack []string, want string) bool {
	for _, s := range haystack {
		if s == want {
			return true
		}
	}
	return false
}
