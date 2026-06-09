// scanner.go — forbidden-phrase scanner for any text we route
// out to a Publisher-mode user.
//
// Why a centralised scanner instead of fixing each prompt:
//
//   - The 10 master persona JSONs already define verdict_enum =
//     ["STRONG_BUY", "BUY", "HOLD", "AVOID", ...]. Rewriting all
//     of those + the LLM responses would require coordinated
//     prompt + parser + UI changes and would lose the ability to
//     A/B test back to the raw verdict form for the RIA-mode path.
//   - LLMs slip the forbidden verbs in even when explicitly told
//     not to ("you should buy at $X"). A defence-in-depth post-
//     processor lets us catch those in test.
//   - The scanner is pure-function and trivially unit-testable,
//     so we can lock down the redacted output as part of the test
//     suite even before any LLM is wired.
//
// What it does NOT do:
//
//   - It does not strip out the verbs from internal types like
//     MasterReport.Verdict — that remains "BUY" for analytics /
//     persistence / RIA-mode rendering. The scanner runs at the
//     *render* boundary (handler → response body), and the API
//     view layer is what calls it.
//
//   - It does not attempt full semantic NLP. A determined LLM
//     output ("nineteen times your money awaits you in Tesla")
//     would pass. That's an acceptable false-negative for the
//     phrase scanner; the disclosures + the model-action labeling
//     + the geo-block are the layered defences.
//
// The redaction strategy is to replace each forbidden phrase
// with its compliant equivalent and record the replacement in a
// Violation slice so the caller can decide to log / flag / fail.

package compliance

import (
	"regexp"
	"strings"
	"sync"
)

// Violation is one (phrase, replacement, position) tuple
// produced by the scanner. The caller can use these to surface
// a "rewritten for compliance" badge in the UI or to fail loudly
// in tests.
type Violation struct {
	// Phrase is the original forbidden phrase as matched in the
	// input text. May be a regex match; the actual text is what
	// the user wrote, not the regex pattern.
	Phrase string `json:"phrase"`
	// Replacement is what we substituted in.
	Replacement string `json:"replacement"`
	// Index is the byte offset in the ORIGINAL input.
	Index int `json:"index"`
	// Rule is a short identifier ("recommend_verb",
	// "buy_now_call_to_action", ...) so we can tag analytics
	// without leaking the actual user-facing string.
	Rule string `json:"rule"`
}

// ScanResult bundles the redacted text and the violation list.
// An empty Violations slice means the text was already compliant
// and the Redacted field is byte-identical to the input.
type ScanResult struct {
	Redacted   string      `json:"redacted"`
	Violations []Violation `json:"violations,omitempty"`
}

// HasViolations is a convenience so callers don't repeat
// len(Violations) > 0.
func (r ScanResult) HasViolations() bool { return len(r.Violations) > 0 }

// phraseRule is one entry in the forbidden-phrase table. We
// keep them grouped so it's easy to disable a whole rule class
// (e.g. drop "buy_now_call_to_action" once we're RIA-registered).
type phraseRule struct {
	rule        string
	pattern     *regexp.Regexp
	replacement string
}

// loadRules returns the rule table. Cached so the regex compile
// happens once. Rule design:
//
//   - "we recommend" → "this model identifies": the strongest
//     red-line phrase; the regex is greedy enough to catch
//     "we strongly recommend" / "I recommend" / "我推荐" too.
//   - "suggested position" → "model allocation": clears the
//     "individualized recommendation" framing.
//   - "stop loss at $X" → "model's stop-loss trigger: $X":
//     keeps the number, replaces the verbal action.
//   - "buy now" / "立即买入" → "current model status: signal
//     active": calls-to-action are the bright line under
//     Marketing Rule 206(4)-1(a)(1).
//   - bare "BUY!" / "SELL!" exclamatives → bracketed "(model action)".
//     Whole-word match so "BUY" inside a longer alphanumeric
//     token (like an order ID "BUYTOK-1234") is not touched.
var (
	rulesOnce sync.Once
	rules     []phraseRule
)

func loadRules() []phraseRule {
	rulesOnce.Do(func() {
		rules = []phraseRule{
			{
				rule:        "we_recommend",
				pattern:     regexp.MustCompile(`(?i)\b(?:we|i|the team)\s+(?:strongly\s+)?recommend(?:s|ed|ing)?\b`),
				replacement: "this model identifies",
			},
			{
				rule:        "we_recommend_cn",
				pattern:     regexp.MustCompile(`我(?:强烈)?(?:推荐|建议)`),
				replacement: "本模型识别到",
			},
			{
				rule:        "suggested_position",
				pattern:     regexp.MustCompile(`(?i)\bsuggested\s+(?:position|allocation|weight)\b`),
				replacement: "model allocation",
			},
			{
				rule:        "suggested_position_cn",
				pattern:     regexp.MustCompile(`建议仓位`),
				replacement: "模型仓位",
			},
			{
				rule:        "stop_loss_directive",
				pattern:     regexp.MustCompile(`(?i)\b(?:set\s+a\s+)?stop[\s-]?loss\s+at\b`),
				replacement: "model stop-loss trigger at",
			},
			{
				rule:        "stop_loss_directive_cn",
				pattern:     regexp.MustCompile(`止损(?:线)?(?:设(?:置|在|为))`),
				replacement: "模型减仓触发线",
			},
			{
				rule:        "buy_now_cta",
				pattern:     regexp.MustCompile(`(?i)\b(?:buy|sell)\s+now\b`),
				replacement: "model signal active",
			},
			{
				rule:        "buy_now_cta_cn",
				pattern:     regexp.MustCompile(`立即(?:买入|卖出|清仓)`),
				replacement: "市场状态切换",
			},
			{
				rule:        "exclamative_action",
				pattern:     regexp.MustCompile(`\b(BUY|SELL)\s*!`),
				replacement: "$1 (model action)",
			},
			{
				rule:        "stock_picker_marketing",
				pattern:     regexp.MustCompile(`(?i)\bAI\s+(?:stock\s+)?pick(?:er|s)?\b`),
				replacement: "AI-powered stock analysis",
			},
			{
				rule:        "ai_will_pick",
				pattern:     regexp.MustCompile(`(?i)\bAI\s+will\s+pick\s+stocks\b`),
				replacement: "AI-powered stock analysis is provided",
			},
			{
				rule:        "guaranteed_returns",
				pattern:     regexp.MustCompile(`(?i)\bguaranteed?\s+(?:return|profit|gain)s?\b`),
				replacement: "hypothetical historical return",
			},
			// -----------------------------------------------------------------
			// Technical-analysis red lines (added with the daily-picks
			// technical-snapshot integration). Rationale per rule:
			//
			//   * price_target_directive: "price target of $850" / "target
			//     price at $850" is the canonical sell-side recommendation
			//     phrase. SEC Marketing Rule treats it as a "specific price
			//     forecast" for IA-regulated firms. The Publisher product
			//     wants the model to QUOTE levels (resistance, prior high)
			//     but never to PROJECT one.
			//   * take_profit_directive: same shape as stop_loss_directive
			//     for the upside leg. We already redact "stop loss at $X";
			//     "take profit at $X" was an untreated mirror.
			//   * entry_exit_directive: "entry point at $X" / "exit price"
			//     are the trade-instruction phrases that make a piece of
			//     content read like a trade signal. The replacement keeps
			//     the level but reframes it as a model reference.
			//   * golden_cross_signal / 金叉_signal_cn: short-term moving
			//     average crossing above a long-term one is FACT (the
			//     bare phrase "golden cross" / "金叉" is fine). Pairing it
			//     with "signal" / "triggered" / "confirmed" turns the
			//     observation into a recommendation. We redact only the
			//     directive pairing.
			//   * breakout_imminent: predicting a breakout is the
			//     forward-looking forecast the SEC explicitly polices.
			//     Past-tense "broke out above the 20-day high" is fine
			//     (and not matched by this rule).
			//   * strong_buy_rating: "Strong Buy" / "Strong Sell" /
			//     "Hold rating" are the sell-side analyst rating labels.
			//     They survive in formal research notes because firms
			//     are RIA-registered; the Publisher product can't use
			//     them. The redactor swaps to "model verdict: STRONG_BUY"
			//     which preserves the enum semantics without claiming a
			//     personalised rating.
			//   * go_long_short: "go long NVDA" / "go short TSLA" are
			//     direct trade instructions. Replaced with the neutral
			//     model-exposure phrasing.
			//   * target_price_cn / 止盈_cn / buy_sell_point_cn /
			//     suggestion_buysell_cn: Chinese mirrors of the above.
			//     We deliberately allow "止损" alone (it's a generic
			//     concept) but redact "止损位" / "止损价" (specific
			//     directive shapes).
			{
				rule:        "price_target_directive",
				pattern:     regexp.MustCompile(`(?i)\b(?:price\s+target|target\s+price)\s+(?:of|at|near|around)\s+\$?\d`),
				replacement: "model fair-value band near $",
			},
			{
				rule:        "take_profit_directive",
				pattern:     regexp.MustCompile(`(?i)\btake[\s-]?profit\s+(?:at|of|near)\b`),
				replacement: "model price-band ceiling at",
			},
			{
				rule:        "entry_exit_directive",
				pattern:     regexp.MustCompile(`(?i)\b(?:entry|exit)\s+(?:point|level|price)\b`),
				replacement: "model price-band reference",
			},
			{
				rule:        "go_long_short",
				pattern:     regexp.MustCompile(`(?i)\bgo\s+(long|short)\b`),
				replacement: "model exposure stance: $1",
			},
			{
				rule:        "golden_cross_signal",
				pattern:     regexp.MustCompile(`(?i)\b(golden|death)\s+cross\s+(signal|triggered|confirmed|imminent)\b`),
				replacement: "$1-cross moving-average crossover observed",
			},
			{
				rule:        "breakout_imminent",
				pattern:     regexp.MustCompile(`(?i)\b(breakout|breakdown)\s+(?:is\s+|appears\s+|seems\s+|looks\s+)?(imminent|expected|likely|coming|due|near|forming)\b`),
				replacement: "$1 pattern observed",
			},
			{
				rule:        "strong_buy_rating",
				pattern:     regexp.MustCompile(`(?i)\b(strong\s+buy|strong\s+sell|hold)\s+rating\b`),
				replacement: "model verdict: $1",
			},
			{
				rule:        "target_price_cn",
				pattern:     regexp.MustCompile(`(目标价|目标位)(?:为|在|约)?`),
				replacement: "模型参考价位",
			},
			{
				rule:        "stop_loss_position_cn",
				pattern:     regexp.MustCompile(`(止损位|止损价|止盈位|止盈价)`),
				replacement: "模型价格参考区间",
			},
			{
				rule:        "buy_sell_point_cn",
				pattern:     regexp.MustCompile(`(买点|卖点|进场点|出场点)`),
				replacement: "模型价格参考点位",
			},
			{
				rule:        "suggestion_buysell_cn",
				pattern:     regexp.MustCompile(`(?:建议|应当|可以)(买入|卖出|加仓|减仓|清仓|抄底|逃顶)`),
				replacement: "本模型显示$1信号",
			},
			{
				rule:        "golden_cross_signal_cn",
				pattern:     regexp.MustCompile(`(金叉|死叉)\s*(信号|形成|出现|预示|确认)`),
				replacement: "$1（均线穿越事件）",
			},
			{
				rule:        "breakout_imminent_cn",
				pattern:     regexp.MustCompile(`(突破|跌破)\s*(在即|临近|预期|即将)`),
				replacement: "$1形态正在形成",
			},
		}
	})
	return rules
}

// Scan runs the forbidden-phrase table over text and returns a
// redacted copy plus the list of violations. Always safe to call;
// if text is empty the result is empty too.
//
// IMPORTANT: Scan is intended for the Publisher-mode render
// boundary. In RIA-registered mode the caller is free to skip
// it (use MaybeScan instead). Calling Scan in RIA mode is not
// wrong per se, just unnecessarily aggressive.
func Scan(text string) ScanResult {
	if text == "" {
		return ScanResult{Redacted: ""}
	}
	out := text
	var vio []Violation
	for _, rule := range loadRules() {
		matches := rule.pattern.FindAllStringIndex(out, -1)
		if len(matches) == 0 {
			continue
		}
		// Build replacements right-to-left so earlier indices
		// stay valid.
		var b strings.Builder
		b.Grow(len(out))
		prev := 0
		for _, m := range matches {
			start, end := m[0], m[1]
			b.WriteString(out[prev:start])
			original := out[start:end]
			replaced := rule.pattern.ReplaceAllString(original, rule.replacement)
			b.WriteString(replaced)
			vio = append(vio, Violation{
				Phrase:      original,
				Replacement: replaced,
				Index:       start,
				Rule:        rule.rule,
			})
			prev = end
		}
		b.WriteString(out[prev:])
		out = b.String()
	}
	return ScanResult{Redacted: out, Violations: vio}
}

// MaybeScan is Scan gated on the compliance mode. RIA-registered
// deployments skip the scan (the rewrites are inappropriate
// when you can legally say "we recommend X").
func MaybeScan(mode Mode, text string) ScanResult {
	if mode.IsRIARegistered() {
		return ScanResult{Redacted: text}
	}
	return Scan(text)
}

// WrapWithAgentPreface returns the text prefixed with the
// mandatory "the following is impersonal analysis under the
// X framework" preface required by the Publisher mode for any
// AI-agent output. In RIA mode the preface is omitted (the
// firm-level Form ADV / Form CRS disclosure replaces it).
//
// The frameworkName is the per-agent label ("Buffett",
// "Lynch", ...) so the preface stays consistent with the
// persona that produced the output.
func WrapWithAgentPreface(mode Mode, frameworkName, locale, text string) string {
	if mode.IsRIARegistered() {
		return text
	}
	loc := normalizeLocale(locale)
	if loc == "zh" {
		return "下文为基于 " + frameworkName +
			" 投资框架的非个性化分析，仅供研究与教育用途，不构成投资建议。请在做出任何投资决策前咨询持牌投资顾问。\n\n" +
			text
	}
	return "The following is impersonal analysis under the " + frameworkName +
		" investment framework, provided for research and education only. It is NOT a recommendation. Consult a licensed financial adviser before making any investment decision.\n\n" +
		text
}
