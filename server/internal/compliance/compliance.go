// Package compliance is the SEC / Marketing-Rule / Publishers'
// Exclusion shield that wraps every public-facing surface that
// could be construed as "individualized investment advice".
//
// Background (US regulatory landscape, 2025-2026):
//
//   - Investment Advisers Act of 1940 Section 202(a)(11)(D) carves
//     out a "publishers' exclusion" for bona-fide publications.
//     The Seeking Alpha v. SEC 2024 decision confirmed the
//     exclusion but established 4 strict conditions:
//       (1) general & regular circulation
//       (2) impersonal & not tailored to subscribers
//       (3) bona-fide publication (not a touting vehicle)
//       (4) no scienter / fraud
//   - Marketing Rule 206(4)-1 (2021, effective 2022) governs
//     hypothetical performance, past specific recommendations,
//     and AI claims. 2024 saw the first "AI Washing" actions
//     (Delphia, Global Predictions).
//   - SEC 2024 Internet Adviser Exemption amendment requires
//     SEC registration (even for state-eligible RIAs) when
//     providing advice through an interactive website. Effective
//     2025-03-31.
//
// The package provides:
//
//   - Mode: ModePublisher (path A — no RIA registration, requires
//     impersonal-only outputs + heavy disclaimers) vs
//     ModeRIARegistered (path B — full personalisation OK once
//     Form ADV is on file).
//   - Phrase scanner: catches forbidden phrases like "buy now" /
//     "we recommend" / "suggested position" in any LLM output BEFORE
//     it reaches the user. Returns a redacted variant + a violation
//     report so the UI can surface a "this was rewritten for
//     compliance" notice.
//   - Disclosure text bank: bilingual (zh / en) standard disclosure
//     blocks for the four core surfaces (advisor, paper trading,
//     backtest, intraday). All bundled here so legal review touches
//     one file.
//
// Nothing in this package depends on the LLM client, the DB, or
// the HTTP layer — it's a pure-function utility consumed by the
// adapters that DO depend on those. That keeps the violation
// scanner unit-testable without spinning up the whole binary.
package compliance

import (
	"strings"
	"sync"
)

// Mode enumerates the compliance posture the deployment is
// operating under. Changes mid-process are disallowed —
// downgrading from RIA to Publisher would create a window of
// previously-public personalised content that can't be retracted.
type Mode string

const (
	// ModePublisher = path A. We rely on Publishers' Exclusion.
	// Strict requirements:
	//   * NO individualized advice (no per-user "buy X at Y%")
	//   * NO "buy / sell / recommend" verbs in outputs
	//   * Heavy disclaimers prefixed on every advisory surface
	//   * Geo-block OFAC-sanctioned countries
	//   * Paper Trading is the SAME content for all subscribers
	ModePublisher Mode = "publisher"

	// ModeRIARegistered = path B. Form ADV on file (SEC or state),
	// CCO designated, Form CRS delivered. Most restrictions
	// relax but Marketing Rule still binds (hypothetical
	// performance disclosure, no cherry-picked past
	// recommendations, AI claims must be substantiated).
	ModeRIARegistered Mode = "ria_registered"
)

// DefaultMode is the safe default for new deployments. Override
// via env COMPLIANCE_MODE in cmd/server/main.go once the legal
// review for the alternate path is signed.
const DefaultMode = ModePublisher

// IsPublisher / IsRIARegistered are tiny helpers so callers
// don't repeat the string compares.
func (m Mode) IsPublisher() bool     { return m == ModePublisher }
func (m Mode) IsRIARegistered() bool { return m == ModeRIARegistered }

// ParseMode normalises a raw env value to a Mode. Unknown
// values fall back to DefaultMode so a typo'd env doesn't
// accidentally drop us out of "publisher" guardrails.
func ParseMode(s string) Mode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case string(ModePublisher), "a", "publisher_mode":
		return ModePublisher
	case string(ModeRIARegistered), "ria", "b":
		return ModeRIARegistered
	default:
		return DefaultMode
	}
}

// Surface enumerates the four user-facing surfaces that need
// distinct disclosure text. We keep them enumerated (rather than
// free-form strings) so the disclosure bank can't be silently
// referenced via a typo.
type Surface string

const (
	SurfaceAdvisor      Surface = "advisor"        // /advisor consultation
	SurfacePaperTrading Surface = "paper_trading"  // /papertrading admin page
	SurfaceBacktest     Surface = "backtest"       // /funds/.../backtests
	SurfaceCNIntraday   Surface = "cn_intraday"    // /cnintraday dry-run page
	SurfaceDailyPicks   Surface = "daily_picks"    // /daily-picks publisher newsletter
)

// Disclosure returns the standard disclosure block for the
// given surface, in the requested locale ("zh" / "en"). The
// returned text is suitable for use as a pre-content banner
// (Marketing Rule requires disclosure to precede or be
// contemporaneous with the marketed content — NOT in a footer).
//
// The text intentionally varies by Mode:
//
//   - Publisher mode emphasizes "NOT registered as an investment
//     adviser" + "impersonal, available to all subscribers".
//   - RIA mode emphasizes "registered as an investment adviser"
//     + Form ADV link + Form CRS.
func Disclosure(mode Mode, surface Surface, locale string) string {
	loc := normalizeLocale(locale)
	bank := disclosureBank()
	if loc == "zh" {
		if text, ok := bank[mode][surface]["zh"]; ok {
			return text
		}
	}
	if text, ok := bank[mode][surface]["en"]; ok {
		return text
	}
	// Last-resort fallback so a typo'd surface never produces
	// content WITHOUT a disclosure.
	return bank[ModePublisher][SurfaceAdvisor]["en"]
}

// AcknowledgmentText returns the short paragraph the user must
// affirmatively click "I understand" on before first using any
// surface in ModePublisher. We keep this distinct from the
// per-surface Disclosure() so the modal stays focused.
func AcknowledgmentText(mode Mode, locale string) string {
	loc := normalizeLocale(locale)
	if mode == ModeRIARegistered {
		if loc == "zh" {
			return "我已阅读 Form ADV Part 2A 及 Form CRS，理解本服务为注册投资顾问提供的非个性化研究与教育内容，本人为最终投资决策的唯一负责人。"
		}
		return "I have read Form ADV Part 2A and Form CRS, understand that this service offers impersonal research and education from a registered investment adviser, and acknowledge that I am solely responsible for my investment decisions."
	}
	if loc == "zh" {
		return "我理解本平台并非注册投资顾问，所有内容为通用市场分析与教育，不构成针对个人的投资建议；所有订阅用户看到的是相同的非个性化内容，本人为投资决策的唯一负责人。"
	}
	return "I understand that this platform is NOT a registered investment adviser, that all content is general market analysis and education made available to every subscriber equally, that it is NOT tailored to my individual circumstances, and that I am solely responsible for my investment decisions."
}

// HypotheticalPerformanceDisclaimer is the standard Marketing
// Rule 206(4)-1 boilerplate that MUST accompany any displayed
// backtest / hypothetical performance number. Includes the four
// required elements: assumptions, limitations, fees, and the
// general "not actual trading" warning.
func HypotheticalPerformanceDisclaimer(locale string) string {
	if normalizeLocale(locale) == "zh" {
		return "回测业绩为基于历史数据的假设性结果，未实际执行交易。结果未考虑实时市场冲击、流动性约束、订单延迟与税费的全部影响；过往业绩不代表未来收益。已扣除模拟交易成本：佣金 2.5bp、印花税 10bp (卖出)、滑点 5bp。最差年份回撤、与基准对比、净费用收益已在 KPI 中列出。"
	}
	return "Backtest results are hypothetical and do NOT represent actual trading. They reflect deductions for simulated commissions (2.5bp), stamp tax (10bp on sells), and slippage (5bp) but do not capture all real-world frictions including market impact, partial fills, taxes, or borrowing costs. Past performance does not guarantee future results. Worst-year drawdown, benchmark comparison, and net-of-fees return are reported in the headline KPIs."
}

// disclosureBank holds the per-mode × per-surface × per-locale
// text. Kept as a function (not a top-level var) so the strings
// are only allocated when the first request lands.
var (
	bankOnce sync.Once
	bank     map[Mode]map[Surface]map[string]string
)

func disclosureBank() map[Mode]map[Surface]map[string]string {
	bankOnce.Do(func() {
		bank = map[Mode]map[Surface]map[string]string{
			ModePublisher: {
				SurfaceAdvisor: {
					"zh": "⚠ 本服务非注册投资顾问。以下为基于公开数据的非个性化框架分析，供教育与一般市场研究之用，不构成对您个人情况的投资建议。任何「评分」「模型动作」「目标价格」均为大师投资框架下的假设性分析输出，并非买入或卖出建议。所有订阅用户看到的是相同内容。投资决策由您本人独立做出，本服务不承担任何盈亏责任。",
					"en": "⚠ This service is NOT a registered investment adviser. The following content is impersonal, general analysis under named investment frameworks (Buffett / Lynch / etc.), provided for education and market research only. It is NOT a recommendation to buy or sell any security and is NOT tailored to your individual circumstances. All subscribers see the same content. You are solely responsible for your investment decisions and any resulting gains or losses.",
				},
				SurfacePaperTrading: {
					"zh": "⚠ 本页面为公开发行的纸面回测组合（Paper Trading），所有订单为发行方公开发布的非个性化研究内容，使用 SHA-256 哈希加 OpenTimestamps 链上存证以证明发布时间。订单的「动作（Action）」为本组合策略模型的状态标识，非买卖建议；任何「目标价」或「止损」均为本模型在该价位触发再平衡规则的标识。本服务不构成投资建议，亦非投资顾问关系。",
					"en": "⚠ This page displays a publicly-published paper-traded portfolio. All orders are impersonal research content released to every subscriber equally, with SHA-256 + OpenTimestamps chain-of-custody proving publication time. Order Action labels (BUY / SELL / REBALANCE) are model-state markers — they are NOT recommendations. Any displayed target price or stop-loss indicates the price at which the model would trigger a rebalance rule. Nothing here constitutes investment advice and no adviser-client relationship is established.",
				},
				SurfaceBacktest: {
					"zh": "回测业绩为基于历史数据的假设性结果，未实际执行交易。过往业绩不代表未来收益。本工具供策略研究与教育用途，不构成投资建议。所有回测费用与成本假设已在结果中标注。",
					"en": "Backtest results are hypothetical, do not represent actual trading, and are presented for strategy research and educational purposes only. Past performance does not guarantee future results. All assumed costs and fees are disclosed alongside the results. Nothing here is investment advice.",
				},
				SurfaceCNIntraday: {
					"zh": "本页面为 A 股日内信号 Dry-Run 工具，仅供策略研究与因子可视化用途。本服务不构成投资建议、不接入实盘券商、不为美国境内用户提供。",
					"en": "This page is a CN A-share intraday signal dry-run tool intended for strategy research and factor visualization. It is NOT investment advice, NOT connected to any broker, and NOT available to US residents.",
				},
				SurfaceDailyPicks: {
					// Publisher-mode daily picks is the strongest
					// form of the "general circulation, non-personalised"
					// argument (Lowe v. SEC). We say so explicitly so the
					// disclosure block doubles as evidence in any
					// hypothetical regulatory inquiry: every reader
					// sees the same row for the same (symbol, date),
					// the content is computed by a fixed cron not in
					// response to any user query, and the cadence is
					// regular (daily after US market close).
					"zh": "⚠ 本页面为公开发布的每日股票观察榜（Financial Newsletter），由本平台 AI 大师团队每日美东收盘后自动生成，发布给所有订阅者的内容完全一致，不针对任何用户个人情况做调整。所有「评分」「模型评级」均为大师投资框架下的假设性输出，并非买入或卖出建议。本服务非注册投资顾问，仅供研究与教育用途，投资决策由您本人独立做出。",
					"en": "⚠ This page is a publicly-distributed daily stock newsletter generated automatically by our AI master-persona panel after each US market close. Every subscriber sees identical content for the same (stock, date); the analysis is NOT tailored to any individual's circumstances. All scores and model ratings are hypothetical outputs under named investment frameworks — they are NOT recommendations to buy or sell. This service is NOT a registered investment adviser. You are solely responsible for your investment decisions.",
				},
			},
			ModeRIARegistered: {
				SurfaceAdvisor: {
					"zh": "本服务由 [实体] 提供，已注册为投资顾问（CRD #XXXXXXX）。请参阅 Form ADV Part 2A 与 Form CRS 了解服务详情、费用、利益冲突与历史业绩。投资有风险，过往业绩不代表未来收益。",
					"en": "This service is offered by [Entity Name], a registered investment adviser (CRD #XXXXXXX). Please review Form ADV Part 2A and Form CRS for service details, fees, conflicts of interest, and prior performance. All investments involve risk; past performance does not guarantee future results.",
				},
				SurfacePaperTrading: {
					"zh": "本页面为本顾问公司公开发行的纸面组合记录。已注册投资顾问下的所有公开材料须符合 SEC Marketing Rule 206(4)-1，相关披露详见 Form ADV。",
					"en": "This page displays a paper portfolio published by the registered adviser. All public marketing material complies with SEC Marketing Rule 206(4)-1. See Form ADV for related disclosures.",
				},
				SurfaceBacktest: {
					"zh": "回测业绩为假设性结果，受 SEC Marketing Rule 206(4)-1 关于假设业绩展示的约束（费用扣除、净收益、最差年份、与基准对比已列出）。过往业绩不代表未来收益。",
					"en": "Backtest results are hypothetical performance subject to SEC Marketing Rule 206(4)-1 (fees deducted, net-of-fees return, worst-year drawdown, and benchmark comparison are disclosed). Past performance does not guarantee future results.",
				},
				SurfaceCNIntraday: {
					"zh": "A 股日内信号 Dry-Run 工具，仅供策略研究。本服务不在注册投资顾问的咨询范围内，亦不对美国境内用户开放。",
					"en": "CN A-share intraday signal dry-run tool, for strategy research only. Outside the scope of the adviser's registered services and not available to US residents.",
				},
				SurfaceDailyPicks: {
					"zh": "本每日股票观察榜由本顾问公司公开发行，所有订阅用户看到同一份内容。所有相关披露（Form ADV / Form CRS / 利益冲突）请参阅顾问公司公开文件。投资有风险，过往业绩不代表未来收益。",
					"en": "This daily stock newsletter is published by the registered adviser for general distribution; every subscriber sees identical content. See Form ADV Part 2A and Form CRS for related disclosures, fees, and conflicts of interest. All investments involve risk; past performance does not guarantee future results.",
				},
			},
		}
	})
	return bank
}

func normalizeLocale(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case strings.HasPrefix(s, "zh"):
		return "zh"
	case strings.HasPrefix(s, "en"):
		return "en"
	}
	return "en"
}
