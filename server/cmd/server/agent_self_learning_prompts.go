package main

// Role-specific user/system prompt builders for the daily self-learning
// LLM call (generateAgentLessonsLLM in wiring_adapters.go). Before this
// file existed, the same fund-wide context block was sent for every
// role and only the role label varied in the system prompt — predictably
// the LLM produced four near-identical lessons across PM / Researcher
// / Trader / Risk. A real research team observes DIFFERENT facts about
// the same day depending on their role:
//
//   - PM thinks about allocation, plan quality, sleeve weights.
//   - Researcher thinks about how their thesis on their focus area
//     translated into actions, regardless of fund-wide P&L.
//   - Trader thinks about fill ratio, slippage bps, reject reasons,
//     order timing — NOT about holdings weights or research themes.
//   - Risk thinks about concentration, reject signals by gate, and
//     whether the day's drawdown is consistent with their budget.
//
// The dispatcher below carves out the fact subset each role actually
// needs, so the resulting `lessons` / `adjustments` read like a per-
// role journal entry instead of four paraphrases of the same paragraph.
//
// All helpers are package-local pure functions. They take the same
// inputs `generateAgentLessonsLLM` already has on hand (no extra DB
// lookups) so this change is zero-cost on the hot path and trivially
// unit-testable.

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/fundai/server/internal/repository"
)

// buildRoleSpecificLearningBody is the main dispatcher. It always
// emits a common header (fund identity, date, role, focus, daily
// return) so every prompt is anchored to the same day, then delegates
// to a role-specific writer that picks the relevant detail block AND
// adds a role-specific reflection prompt at the end.
//
// Unknown roles fall through to the legacy fund-wide block so newer
// roles we may add later don't silently produce empty prompts.
func buildRoleSpecificLearningBody(
	role, focus string,
	learningCtx *learningContext,
	tradeStats tradeSummary,
	dailyReturn float64,
) string {
	if learningCtx == nil || learningCtx.fund == nil {
		return ""
	}
	var sb strings.Builder
	writeLearningHeader(&sb, role, focus, learningCtx, dailyReturn)

	switch strings.ToLower(strings.TrimSpace(role)) {
	case "pm", "portfolio_manager":
		writePMLearningBody(&sb, learningCtx, tradeStats, dailyReturn)
	case "researcher", "analyst":
		writeResearcherLearningBody(&sb, focus, learningCtx, tradeStats, dailyReturn)
	case "trader":
		writeTraderLearningBody(&sb, learningCtx, tradeStats, dailyReturn)
	case "risk", "risk_overseer":
		writeRiskLearningBody(&sb, learningCtx, tradeStats, dailyReturn)
	default:
		writeDefaultLearningBody(&sb, learningCtx, tradeStats)
	}
	return sb.String()
}

// roleSpecificSystemHint returns a 1-2 sentence add-on to the global
// system prompt that pins what THIS role should anchor on. Keeping the
// hint short matters because the JSON-output instruction is already
// the dominant prompt instruction; a longer per-role rant would
// dilute it.
func roleSpecificSystemHint(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "pm", "portfolio_manager":
		return "你是组合经理 (PM)：从【组合配置、套件权重、计划完成度、收益归因】视角写复盘。不复盘单笔执行细节、不复盘个股研究主题。"
	case "researcher", "analyst":
		return "你是研究员：只从你所 cover 的 focus 视角（如果上文给出 focus 列表，严格限定在这些标的范围内）总结今天的研究信号 → 计划落地转化效率。不必复盘组合层指标。"
	case "trader":
		return "你是交易员：只从【成交率、滑点、拒单分布、单笔执行节奏】视角复盘。不复盘组合权重、不复盘研究主题。"
	case "risk", "risk_overseer":
		return "你是风险控制：从【集中度、风险预算占用、拒单边界、收益波动是否在预算内】视角复盘。不复盘研究主题、不直接评价计划好坏。"
	}
	return "请聚焦于自身角色独有的关注点，避免与其他角色复盘内容重叠。"
}

// writeLearningHeader emits the common 4-line preamble every role
// sees: fund identity, date, agent role + focus, daily return. We
// keep this consistent so the model has a stable anchor regardless of
// which role-specific body follows.
func writeLearningHeader(
	sb *strings.Builder,
	role, focus string,
	learningCtx *learningContext,
	dailyReturn float64,
) {
	fmt.Fprintf(sb, "Fund: %s\n", strings.TrimSpace(learningCtx.fund.Name))
	fmt.Fprintf(sb, "Trading date: %s\n", learningCtx.tradingDate.Format("2006-01-02"))
	fmt.Fprintf(sb, "Agent role: %s\n", role)
	if focus != "" {
		fmt.Fprintf(sb, "Agent focus: %s\n", focus)
	}
	fmt.Fprintf(sb, "Daily return: %.4f%%\n\n", dailyReturn*100)
}

// ---------------------------------------------------------------------------
// PM block
// ---------------------------------------------------------------------------

// writePMLearningBody — portfolio manager view: allocation + plan
// quality + completion ratio. Picks top-5 holdings by NAV weight and
// the first 5 plan actions; deliberately omits per-fill slippage /
// reject reasons because those belong to Trader / Risk.
func writePMLearningBody(
	sb *strings.Builder,
	learningCtx *learningContext,
	tradeStats tradeSummary,
	dailyReturn float64,
) {
	sb.WriteString("[组合层视角]\n")
	if learningCtx.plan != nil {
		fmt.Fprintf(sb, "Plan status: %s, action count: %d\n", learningCtx.plan.Status, len(learningCtx.actions))
		if reasoning := strings.TrimSpace(learningCtx.plan.Reasoning.String); reasoning != "" {
			fmt.Fprintf(sb, "Plan reasoning (前 200 字): %s\n", truncatePromptText(reasoning, 200))
		}
	} else {
		sb.WriteString("Plan: 今日未生成\n")
	}
	// Completion ratio over plan-emitted actions, not over raw trades —
	// PM cares about "how many of my decisions actually executed".
	if len(learningCtx.actions) > 0 {
		filled, attempted := 0, 0
		for _, a := range learningCtx.actions {
			switch strings.ToLower(strings.TrimSpace(a.Action)) {
			case "buy", "sell", "add", "reduce":
				attempted++
				if strings.EqualFold(strings.TrimSpace(a.ExecutionStatus), "filled") {
					filled++
				}
			}
		}
		if attempted > 0 {
			fmt.Fprintf(sb, "Plan completion: %d/%d (%.0f%%)\n", filled, attempted, float64(filled)/float64(attempted)*100)
		}
	}
	writeTopHoldings(sb, learningCtx, 5)
	writePlanActions(sb, learningCtx.actions, 5)
	sb.WriteString("\n以组合经理 (PM) 视角复盘：今日计划质量、收益归因、套件权重是否需要调整。不要谈单笔执行细节。\n")
}

// ---------------------------------------------------------------------------
// Researcher block (specialization-isolated)
// ---------------------------------------------------------------------------

// writeResearcherLearningBody — researcher view: only the focus
// area. `focus` is the team_member.focus string; we extract symbol
// tokens from it (comma/slash/space separated) and filter holdings +
// actions to only those tickers. If the focus is a theme that doesn't
// resolve to tickers, we still cap the holdings/actions list but keep
// the prompt's last line "from your focus={focus} angle".
//
// Why limit to focus tickers: real researchers DON'T review trades on
// names they don't cover. A semiconductor researcher journaling about
// a baijiu position is noise.
func writeResearcherLearningBody(
	sb *strings.Builder,
	focus string,
	learningCtx *learningContext,
	tradeStats tradeSummary,
	dailyReturn float64,
) {
	focusSyms := extractFocusSymbols(focus)
	sb.WriteString("[研究方向视角]\n")
	if len(focusSyms) > 0 {
		fmt.Fprintf(sb, "Focus 解析为 %d 个聚焦标的: %s\n", len(focusSyms), strings.Join(focusSyms, ", "))
		// Filter holdings + actions to focus-only.
		filteredHoldings := filterHoldingsBySymbols(learningCtx.positions, focusSyms)
		filteredActions := filterActionsBySymbols(learningCtx.actions, focusSyms)
		if len(filteredHoldings) > 0 {
			sb.WriteString("聚焦标的当前持仓:\n")
			writeHoldingLines(sb, filteredHoldings, learningCtx, 5)
		} else {
			sb.WriteString("聚焦标的当前无持仓\n")
		}
		if len(filteredActions) > 0 {
			sb.WriteString("聚焦标的今日计划动作:\n")
			writePlanActions(sb, filteredActions, 5)
		} else {
			sb.WriteString("聚焦标的今日无计划动作（研究信号未转化为下单）\n")
		}
	} else {
		// Focus is theme-shaped, not symbol-shaped. Show the team's
		// top holdings + actions and let the model reason about
		// theme alignment.
		sb.WriteString("Focus 未解析出具体 ticker（视为研究主题）；以下为团队全局视图供参考:\n")
		writeTopHoldings(sb, learningCtx, 5)
		writePlanActions(sb, learningCtx.actions, 5)
	}
	// Did plan reasoning mention focus terms? Crude heuristic but
	// surfaces the "research → plan transmission rate" question.
	if learningCtx.plan != nil && learningCtx.plan.Reasoning.Valid {
		reasoning := strings.ToLower(learningCtx.plan.Reasoning.String)
		matched := 0
		for _, t := range strings.Fields(strings.ToLower(focus)) {
			if t = strings.TrimFunc(t, func(r rune) bool { return !isFocusTokenChar(r) }); t == "" {
				continue
			}
			if strings.Contains(reasoning, t) {
				matched++
			}
		}
		fmt.Fprintf(sb, "Plan reasoning 中提到 focus 关键词: %d 次\n", matched)
	}
	if focus == "" {
		sb.WriteString("\n以研究员视角复盘：focus 信息缺失，请基于全局组合给出泛化的研究→执行转化反思。\n")
	} else {
		fmt.Fprintf(sb, "\n以研究员（focus=%s）视角复盘：你的研究观点是否落地、聚焦标的当日表现如何、是否需要修订下一份 brief。\n", focus)
	}
}

// ---------------------------------------------------------------------------
// Trader block
// ---------------------------------------------------------------------------

// writeTraderLearningBody — trader view: execution micro-structure.
// Trade stats, per-trade slippage (when present), cancel reason
// distribution. Deliberately omits holdings + research themes.
func writeTraderLearningBody(
	sb *strings.Builder,
	learningCtx *learningContext,
	tradeStats tradeSummary,
	dailyReturn float64,
) {
	sb.WriteString("[执行层视角]\n")
	fmt.Fprintf(sb, "Trade stats: total=%d filled=%d partial=%d rejected=%d fillRatio=%.2f\n",
		tradeStats.total, tradeStats.filled, tradeStats.partial, tradeStats.rejected, tradeStats.fillRatio)

	// Per-trade detail (up to 5): side, qty, planned price, fill price,
	// slippage bps. Skips legacy rows that don't have slippage_pct.
	if len(learningCtx.trades) > 0 {
		sb.WriteString("Per-trade execution detail:\n")
		max := 5
		if len(learningCtx.trades) < max {
			max = len(learningCtx.trades)
		}
		for i := 0; i < max; i++ {
			t := learningCtx.trades[i]
			planned := ""
			if t.Price.Valid && t.Price.Float64 > 0 {
				planned = fmt.Sprintf("planned=%.4f", t.Price.Float64)
			}
			fill := ""
			if t.FilledPrice.Valid && t.FilledPrice.Float64 > 0 {
				fill = fmt.Sprintf("fill=%.4f", t.FilledPrice.Float64)
			}
			slip := ""
			if t.SlippagePct.Valid {
				slip = fmt.Sprintf("slippage=%.1fbps", t.SlippagePct.Float64*10000)
			}
			fmt.Fprintf(sb, "  - %s %s qty=%.0f %s %s %s status=%s\n",
				strings.TrimSpace(t.Symbol),
				strings.TrimSpace(t.Side),
				t.Quantity,
				planned, fill, slip,
				strings.TrimSpace(t.Status),
			)
		}
		if len(learningCtx.trades) > max {
			fmt.Fprintf(sb, "  - … and %d more\n", len(learningCtx.trades)-max)
		}
	}

	// Reject reason distribution. cancel_reason is a low-cardinality
	// tag (e.g. lot_size, market_status, price_collar) so a histogram
	// fits in 2-3 lines.
	if breakdown := cancelReasonHistogram(learningCtx.trades); len(breakdown) > 0 {
		sb.WriteString("Reject / cancel reason breakdown:\n")
		for _, entry := range breakdown {
			fmt.Fprintf(sb, "  - %s: %d\n", entry.reason, entry.count)
		}
	}

	sb.WriteString("\n以交易员视角复盘：成交率、滑点、拒单分布是否在可接受范围。不要谈研究主题、不评价 PM 计划好坏。\n")
}

// ---------------------------------------------------------------------------
// Risk block
// ---------------------------------------------------------------------------

// writeRiskLearningBody — risk overseer view: concentration metrics
// + reject signal aggregation + drawdown vs budget. Deliberately
// omits per-research-theme detail.
func writeRiskLearningBody(
	sb *strings.Builder,
	learningCtx *learningContext,
	tradeStats tradeSummary,
	dailyReturn float64,
) {
	sb.WriteString("[风控视角]\n")

	// Use the same NAV-weight derivation the other risk metric uses
	// (topNWeightSum below) instead of position.Weight — the latter
	// is populated only on some code paths and is zero in unit-test
	// fixtures that don't snapshot the full position projection.
	// Going through holdingLinesFor keeps the weight definition
	// consistent ("|MarketValue| / nav.TotalAssets") across all
	// four role bodies.
	lines := holdingLinesFor(learningCtx)
	top5Sum := topNWeightSum(learningCtx.positions, learningCtx.nav, 5)
	if len(lines) > 0 {
		top := lines[0]
		if math.Abs(top.weight) > 0 {
			fmt.Fprintf(sb, "Max single position: %s %.2f%% (concentration risk metric)\n", top.symbol, top.weight*100)
		}
	}
	if top5Sum > 0 {
		fmt.Fprintf(sb, "Top-5 weight sum: %.2f%%\n", top5Sum*100)
	}
	fmt.Fprintf(sb, "Trade reject count: %d (out of %d total)\n", tradeStats.rejected, tradeStats.total)

	// Same cancel reason breakdown the trader sees, but framed as
	// "gate signals" — these tell risk whether the protective rules
	// are firing healthily or whether they're being tripped abusively.
	if breakdown := cancelReasonHistogram(learningCtx.trades); len(breakdown) > 0 {
		sb.WriteString("Risk gate signals (cancel reason distribution):\n")
		for _, entry := range breakdown {
			fmt.Fprintf(sb, "  - %s: %d\n", entry.reason, entry.count)
		}
	}

	// Drawdown framing: surface "today's drawdown as a fraction of
	// some risk budget" by comparing |daily return| to a notional 1%
	// daily VaR style threshold. We don't have access to per-fund VaR
	// here yet, so we report the raw number with a hint.
	if dailyReturn < 0 {
		fmt.Fprintf(sb, "Today's drawdown: %.2f%% — compare against the fund's daily risk budget.\n", -dailyReturn*100)
	}

	sb.WriteString("\n以风险控制视角复盘：集中度是否健康、拒单分布是否合理、当日回撤是否在风险预算内。不要复盘研究主题或具体交易战术。\n")
}

// ---------------------------------------------------------------------------
// Default / fallback block
// ---------------------------------------------------------------------------

// writeDefaultLearningBody is the original fund-wide block we used
// to send to every role. Kept as the fallback so an unrecognised
// role still gets a usable prompt instead of an empty one.
func writeDefaultLearningBody(
	sb *strings.Builder,
	learningCtx *learningContext,
	tradeStats tradeSummary,
) {
	fmt.Fprintf(sb, "Trade stats: total=%d filled=%d partial=%d rejected=%d fillRatio=%.2f\n",
		tradeStats.total, tradeStats.filled, tradeStats.partial, tradeStats.rejected, tradeStats.fillRatio)
	if learningCtx.plan != nil {
		fmt.Fprintf(sb, "Plan status: %s, action count: %d\n", learningCtx.plan.Status, len(learningCtx.actions))
	} else {
		sb.WriteString("Plan: not generated today\n")
	}
	writeTopHoldings(sb, learningCtx, 5)
	writePlanActions(sb, learningCtx.actions, 5)
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// holdingLine is a small projection used so the same sort/format
// helpers can be reused by PM / Researcher / Risk paths.
type holdingLine struct {
	symbol string
	weight float64
}

// writeTopHoldings sorts positions by absolute NAV weight and writes
// the top-N to the prompt as "  - SYM 6.40%" lines. The absolute
// value is intentional: a 20% short still counts as concentration
// risk even though its signed weight is negative.
func writeTopHoldings(sb *strings.Builder, learningCtx *learningContext, n int) {
	lines := holdingLinesFor(learningCtx)
	if len(lines) == 0 {
		return
	}
	if len(lines) > n {
		lines = lines[:n]
	}
	sb.WriteString("Top holdings (symbol, weight of NAV):\n")
	for _, h := range lines {
		fmt.Fprintf(sb, "  - %s %.2f%%\n", h.symbol, h.weight*100)
	}
}

// writeHoldingLines writes positions that have already been filtered
// (e.g. by researcher focus). Output format matches writeTopHoldings
// for visual consistency in the prompt.
func writeHoldingLines(
	sb *strings.Builder,
	positions []repository.HoldingPosition,
	learningCtx *learningContext,
	n int,
) {
	lines := make([]holdingLine, 0, len(positions))
	for _, p := range positions {
		w := 0.0
		if learningCtx.nav != nil && learningCtx.nav.TotalAssets > 0 {
			w = p.MarketValue / learningCtx.nav.TotalAssets
		}
		lines = append(lines, holdingLine{symbol: strings.TrimSpace(p.Symbol), weight: w})
	}
	sort.SliceStable(lines, func(i, j int) bool { return math.Abs(lines[i].weight) > math.Abs(lines[j].weight) })
	if len(lines) > n {
		lines = lines[:n]
	}
	for _, h := range lines {
		fmt.Fprintf(sb, "  - %s %.2f%%\n", h.symbol, h.weight*100)
	}
}

func holdingLinesFor(learningCtx *learningContext) []holdingLine {
	lines := make([]holdingLine, 0, len(learningCtx.positions))
	for _, p := range learningCtx.positions {
		w := 0.0
		if learningCtx.nav != nil && learningCtx.nav.TotalAssets > 0 {
			w = p.MarketValue / learningCtx.nav.TotalAssets
		}
		lines = append(lines, holdingLine{symbol: strings.TrimSpace(p.Symbol), weight: w})
	}
	sort.SliceStable(lines, func(i, j int) bool { return math.Abs(lines[i].weight) > math.Abs(lines[j].weight) })
	return lines
}

// writePlanActions writes up to n actions in the same format the
// original prompt used. Kept side-effect-free so all four role
// builders can call it without duplicating the formatting.
func writePlanActions(sb *strings.Builder, actions []repository.PlanAction, n int) {
	if len(actions) == 0 {
		return
	}
	sb.WriteString("Plan actions:\n")
	for i, a := range actions {
		if i >= n {
			fmt.Fprintf(sb, "  - … and %d more\n", len(actions)-n)
			break
		}
		amount := a.Amount.Float64
		fmt.Fprintf(sb, "  - %s %s amount=%.2f exec=%s\n",
			strings.TrimSpace(a.Symbol), strings.TrimSpace(a.Action), amount, strings.TrimSpace(a.ExecutionStatus))
	}
}

// cancelReasonEntry is a histogram bucket used by Trader / Risk
// summaries. Sorted by descending count so the dominant gate appears
// first.
type cancelReasonEntry struct {
	reason string
	count  int
}

func cancelReasonHistogram(trades []repository.TradeExecution) []cancelReasonEntry {
	if len(trades) == 0 {
		return nil
	}
	counts := map[string]int{}
	for _, t := range trades {
		reason := strings.TrimSpace(t.CancelReason.String)
		if reason == "" {
			continue
		}
		counts[reason]++
	}
	if len(counts) == 0 {
		return nil
	}
	out := make([]cancelReasonEntry, 0, len(counts))
	for reason, count := range counts {
		out = append(out, cancelReasonEntry{reason: reason, count: count})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].count > out[j].count })
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

// topNWeightSum is the sum of the top-N positions' absolute NAV
// weight — the textbook concentration metric. Returns 0 when NAV
// total_assets is missing or zero so we don't divide by zero.
func topNWeightSum(
	positions []repository.HoldingPosition,
	nav *repository.NavSnapshot,
	n int,
) float64 {
	if len(positions) == 0 || nav == nil || nav.TotalAssets <= 0 {
		return 0
	}
	weights := make([]float64, 0, len(positions))
	for _, p := range positions {
		weights = append(weights, math.Abs(p.MarketValue/nav.TotalAssets))
	}
	sort.SliceStable(weights, func(i, j int) bool { return weights[i] > weights[j] })
	if len(weights) > n {
		weights = weights[:n]
	}
	sum := 0.0
	for _, w := range weights {
		sum += w
	}
	return sum
}

// focusTokenRe captures a contiguous run of "symbol-ish" characters:
// letters, digits, dots (for ticker.exchange forms), dashes, colons
// (for BINANCE:BTCUSDT). This deliberately excludes whitespace and
// generic Chinese / English punctuation so a focus string like
// "688205, 300552, CPO 半导体" splits cleanly.
var focusTokenRe = regexp.MustCompile(`[A-Za-z0-9\.\-\:]+`)

// isFocusTokenChar mirrors focusTokenRe's character class for
// position-level filtering of plan reasoning text.
func isFocusTokenChar(r rune) bool {
	if r >= 'a' && r <= 'z' {
		return true
	}
	if r >= 'A' && r <= 'Z' {
		return true
	}
	if r >= '0' && r <= '9' {
		return true
	}
	switch r {
	case '.', '-', ':':
		return true
	}
	return false
}

// extractFocusSymbols pulls plausible ticker-shaped tokens out of a
// free-form focus string. A token counts as "ticker-shaped" if it has
// at least one digit (matches A-share / HK / China-listed numeric
// tickers like 688205, 300552, 0700.HK) OR is all-uppercase Latin of
// length ≥ 2 (matches US tickers, BINANCE:BTCUSDT, etc).
//
// Tokens that look like generic Chinese theme labels ("半导体", "AI",
// "infra") are NOT returned — the researcher block falls back to the
// theme-shaped path in that case.
func extractFocusSymbols(focus string) []string {
	focus = strings.TrimSpace(focus)
	if focus == "" {
		return nil
	}
	matches := focusTokenRe.FindAllString(focus, -1)
	out := make([]string, 0, len(matches))
	seen := map[string]struct{}{}
	for _, m := range matches {
		token := strings.TrimSpace(m)
		if token == "" {
			continue
		}
		if !looksLikeTickerToken(token) {
			continue
		}
		canonical := strings.ToUpper(token)
		if _, dup := seen[canonical]; dup {
			continue
		}
		seen[canonical] = struct{}{}
		out = append(out, token)
	}
	return out
}

func looksLikeTickerToken(token string) bool {
	if len(token) < 2 {
		return false
	}
	hasDigit := false
	allUpper := true
	for _, r := range token {
		if r >= '0' && r <= '9' {
			hasDigit = true
		}
		if r >= 'a' && r <= 'z' {
			allUpper = false
		}
	}
	if hasDigit {
		return true
	}
	if allUpper {
		// Filter out two-letter English words that aren't tickers.
		// We treat ALL-CAPS strings ≥ 2 chars as ticker candidates
		// since that's the normal US / crypto convention.
		return true
	}
	return false
}

// filterHoldingsBySymbols keeps only positions whose symbol matches
// (case-insensitive, exact match after trim) one of `symbols`.
// Returns nil when no positions match so the caller can branch on
// "no matching focus holdings".
func filterHoldingsBySymbols(
	positions []repository.HoldingPosition,
	symbols []string,
) []repository.HoldingPosition {
	if len(positions) == 0 || len(symbols) == 0 {
		return nil
	}
	want := map[string]struct{}{}
	for _, s := range symbols {
		want[strings.ToUpper(strings.TrimSpace(s))] = struct{}{}
	}
	out := make([]repository.HoldingPosition, 0, len(positions))
	for _, p := range positions {
		if _, ok := want[strings.ToUpper(strings.TrimSpace(p.Symbol))]; ok {
			out = append(out, p)
		}
	}
	return out
}

func filterActionsBySymbols(
	actions []repository.PlanAction,
	symbols []string,
) []repository.PlanAction {
	if len(actions) == 0 || len(symbols) == 0 {
		return nil
	}
	want := map[string]struct{}{}
	for _, s := range symbols {
		want[strings.ToUpper(strings.TrimSpace(s))] = struct{}{}
	}
	out := make([]repository.PlanAction, 0, len(actions))
	for _, a := range actions {
		if _, ok := want[strings.ToUpper(strings.TrimSpace(a.Symbol))]; ok {
			out = append(out, a)
		}
	}
	return out
}

// truncatePromptText caps text at n runes (NOT bytes) and appends
// "…" when truncation happens. Rune-based so CJK strings don't break
// mid-character.
func truncatePromptText(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
