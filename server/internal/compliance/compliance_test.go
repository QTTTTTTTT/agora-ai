package compliance

import (
	"strings"
	"testing"
)

// ParseMode default-to-Publisher is the safety property we MUST
// preserve — an env typo dropping us into a less-restricted
// mode would be a compliance failure.
func TestParseModeDefaultsToPublisher(t *testing.T) {
	cases := []struct {
		in   string
		want Mode
	}{
		{"", ModePublisher},
		{"  ", ModePublisher},
		{"unknown", ModePublisher},
		{"publisher", ModePublisher},
		{"PUBLISHER", ModePublisher},
		{"a", ModePublisher},
		{"ria", ModeRIARegistered},
		{"RIA_REGISTERED", ModeRIARegistered},
		{"b", ModeRIARegistered},
	}
	for _, c := range cases {
		if got := ParseMode(c.in); got != c.want {
			t.Errorf("ParseMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDisclosureBilingual(t *testing.T) {
	surfaces := []Surface{
		SurfaceAdvisor, SurfacePaperTrading, SurfaceBacktest, SurfaceCNIntraday,
	}
	for _, s := range surfaces {
		zh := Disclosure(ModePublisher, s, "zh-CN")
		en := Disclosure(ModePublisher, s, "en-US")
		if zh == "" || en == "" {
			t.Errorf("surface %q missing localised disclosure", s)
		}
		if zh == en {
			t.Errorf("surface %q zh and en should differ", s)
		}
	}
	// RIA mode disclosure must mention "registered" — that is
	// the whole point of Path B.
	ria := Disclosure(ModeRIARegistered, SurfaceAdvisor, "en")
	if !strings.Contains(strings.ToLower(ria), "registered") {
		t.Errorf("RIA-mode disclosure missing 'registered' keyword: %s", ria)
	}
}

// TestPublisherDisclosureMustWarnNotRecommendation lines up with
// SEC's published guidance that the disclosure must affirmatively
// state "NOT a recommendation" — a softer "consider this" is
// insufficient.
func TestPublisherDisclosureMustWarnNotRecommendation(t *testing.T) {
	en := Disclosure(ModePublisher, SurfaceAdvisor, "en")
	upper := strings.ToUpper(en)
	for _, must := range []string{"NOT", "RECOMMENDATION"} {
		if !strings.Contains(upper, must) {
			t.Errorf("Publisher advisor disclosure missing %q keyword: %s", must, en)
		}
	}
}

func TestHypotheticalPerformanceDisclaimer(t *testing.T) {
	en := HypotheticalPerformanceDisclaimer("en-US")
	for _, want := range []string{"hypothetical", "Past performance", "actual trading"} {
		if !strings.Contains(en, want) {
			t.Errorf("hypothetical disclaimer missing %q: %s", want, en)
		}
	}
}

// TestScanRedactsRecommendVerb is the core property: the EN
// "we recommend" must become a model-state phrase.
func TestScanRedactsRecommendVerb(t *testing.T) {
	in := "We recommend buying NVDA at $850 with a stop loss at $810."
	got := Scan(in)
	if !got.HasViolations() {
		t.Fatalf("expected violations, got none. in=%q", in)
	}
	if strings.Contains(strings.ToLower(got.Redacted), "we recommend") {
		t.Errorf("redacted text still contains 'we recommend': %s", got.Redacted)
	}
	if !strings.Contains(strings.ToLower(got.Redacted), "model") {
		t.Errorf("redacted text should mention 'model': %s", got.Redacted)
	}
	// stop_loss_directive also fires here.
	if !contains(got.Violations, "stop_loss_directive") {
		t.Errorf("stop_loss_directive should fire on 'stop loss at $810': %+v", got.Violations)
	}
}

func TestScanRedactsChineseDirectives(t *testing.T) {
	in := "我推荐建议仓位 15%，立即买入并设置止损线在 $810。"
	got := Scan(in)
	if !got.HasViolations() {
		t.Fatalf("expected violations, got none. in=%q", in)
	}
	// At least the recommend verb and the suggested position
	// must be redacted.
	if strings.Contains(got.Redacted, "我推荐") {
		t.Errorf("redacted should not contain 我推荐: %s", got.Redacted)
	}
	if strings.Contains(got.Redacted, "建议仓位") {
		t.Errorf("redacted should not contain 建议仓位: %s", got.Redacted)
	}
	if strings.Contains(got.Redacted, "立即买入") {
		t.Errorf("redacted should not contain 立即买入: %s", got.Redacted)
	}
}

func TestScanIdempotentOnAlreadyCompliantText(t *testing.T) {
	in := "Under the Buffett framework, NVDA scores 78/100. The model's stop-loss trigger sits at the 50-day moving average."
	got := Scan(in)
	if got.HasViolations() {
		t.Errorf("compliant text should have no violations: %+v", got.Violations)
	}
	if got.Redacted != in {
		t.Errorf("compliant text should be untouched.\n want: %q\n  got: %q", in, got.Redacted)
	}
}

func TestScanLeavesIdentifiersAlone(t *testing.T) {
	// BUYTOK-1234 contains "BUY" as a prefix but should not
	// be touched by the exclamative_action rule.
	in := "Order id BUYTOK-1234 cleared."
	got := Scan(in)
	if got.Redacted != in {
		t.Errorf("identifier prefix should be untouched.\n want: %q\n  got: %q", in, got.Redacted)
	}
}

func TestScanRedactsExclamativeAction(t *testing.T) {
	in := "BUY! NVDA is the play of the year."
	got := Scan(in)
	if !strings.Contains(got.Redacted, "(model action)") {
		t.Errorf("'BUY!' should be tagged as (model action): %s", got.Redacted)
	}
}

func TestMaybeScanRespectsMode(t *testing.T) {
	in := "We recommend buying NVDA."
	ria := MaybeScan(ModeRIARegistered, in)
	if ria.Redacted != in {
		t.Errorf("RIA mode should NOT redact: got %q", ria.Redacted)
	}
	pub := MaybeScan(ModePublisher, in)
	if pub.Redacted == in {
		t.Errorf("Publisher mode should redact: got unchanged %q", pub.Redacted)
	}
}

func TestWrapWithAgentPrefacePublisher(t *testing.T) {
	out := WrapWithAgentPreface(ModePublisher, "Buffett", "en-US", "NVDA looks attractive.")
	if !strings.Contains(out, "impersonal analysis") {
		t.Errorf("preface should declare 'impersonal analysis': %s", out)
	}
	if !strings.Contains(out, "Buffett") {
		t.Errorf("preface should name the framework: %s", out)
	}
	if !strings.Contains(out, "NVDA looks attractive.") {
		t.Errorf("preface should retain the original text: %s", out)
	}
}

func TestWrapWithAgentPrefaceRIASkips(t *testing.T) {
	in := "NVDA looks attractive."
	out := WrapWithAgentPreface(ModeRIARegistered, "Buffett", "en", in)
	if out != in {
		t.Errorf("RIA mode should not prefix; got %q", out)
	}
}

// --- geo tests ---

func TestGeoDecideOFACBlocks(t *testing.T) {
	for _, cc := range []string{"CU", "IR", "KP", "SY", "RU"} {
		d := GeoDecide(cc)
		if d.Action != ActionBlock {
			t.Errorf("country %q should be ActionBlock, got %q", cc, d.Action)
		}
		if d.RuleID != "ofac_comprehensive" {
			t.Errorf("country %q rule id mismatch: %q", cc, d.RuleID)
		}
	}
}

func TestGeoDecideEUWarns(t *testing.T) {
	d := GeoDecide("DE")
	if d.Action != ActionWarn {
		t.Errorf("EU member should warn, got %q", d.Action)
	}
}

func TestGeoDecideUSAllows(t *testing.T) {
	d := GeoDecide("US")
	if d.Action != ActionAllow {
		t.Errorf("US should allow, got %q", d.Action)
	}
}

func TestGeoDecideExUSStateScrutiny(t *testing.T) {
	d := GeoDecideEx("US", "CA")
	if d.Action != ActionRequiresAck {
		t.Errorf("CA should require ack, got %q", d.Action)
	}
	if !strings.Contains(d.RuleID, "us_state_blue_sky_ca") {
		t.Errorf("rule id should mention ca: %q", d.RuleID)
	}
}

func TestGeoDecideExUSStateDefault(t *testing.T) {
	d := GeoDecideEx("US", "WA")
	if d.Action != ActionAllow {
		t.Errorf("WA should allow, got %q", d.Action)
	}
}

func TestGeoDecideEmptyAllows(t *testing.T) {
	d := GeoDecide("")
	if d.Action != ActionAllow {
		t.Errorf("empty should allow (fail-open), got %q", d.Action)
	}
}

// --- technical-analysis red-line tests ---

// TestScanRedactsPriceTargetDirective covers the most common
// sell-side phrase that has to be redacted for Publisher-mode:
// "price target of $850". This is the bright line under SEC's
// Marketing Rule for non-RIA firms — a "specific price forecast"
// is the classic recommendation indicator.
func TestScanRedactsPriceTargetDirective(t *testing.T) {
	in := "Our price target of $850 looks reasonable given the moat."
	got := Scan(in)
	if !contains(got.Violations, "price_target_directive") {
		t.Fatalf("price_target_directive rule should fire, got %+v", got.Violations)
	}
	if strings.Contains(strings.ToLower(got.Redacted), "price target of $850") {
		t.Errorf("redacted should not contain 'price target of $850': %s", got.Redacted)
	}
}

// TestScanRedactsTakeProfitDirective is the upside leg sibling
// of the existing stop_loss_directive test. Without this rule
// the pattern "stop loss at $X / take profit at $Y" leaks the
// take-profit half through.
func TestScanRedactsTakeProfitDirective(t *testing.T) {
	in := "Set stop loss at $100 and take-profit at $150."
	got := Scan(in)
	if !contains(got.Violations, "stop_loss_directive") {
		t.Errorf("stop_loss_directive should fire: %+v", got.Violations)
	}
	if !contains(got.Violations, "take_profit_directive") {
		t.Errorf("take_profit_directive should fire: %+v", got.Violations)
	}
}

// TestScanRedactsEntryExitDirective covers the "entry point at
// $X" / "exit level $Y" phrases that read like trade
// instructions. Replacement keeps the level but reframes it as
// a model reference.
func TestScanRedactsEntryExitDirective(t *testing.T) {
	in := "Recommended entry point near the 50-day MA and exit price above resistance."
	got := Scan(in)
	if !contains(got.Violations, "entry_exit_directive") {
		t.Errorf("entry_exit_directive should fire: %+v", got.Violations)
	}
	// The replacement must NOT contain "entry point" / "exit price".
	lower := strings.ToLower(got.Redacted)
	if strings.Contains(lower, "entry point") || strings.Contains(lower, "exit price") {
		t.Errorf("redacted should not contain trade-action phrases: %s", got.Redacted)
	}
}

// TestScanRedactsGoldenCrossSignal is the precision test: the
// rule should leave a bare "golden cross" observation alone but
// redact "golden cross signal" / "golden cross triggered" /
// "death cross confirmed".
func TestScanRedactsGoldenCrossSignal(t *testing.T) {
	plain := "A golden cross is forming as MA5 approaches MA20."
	if got := Scan(plain); got.HasViolations() {
		t.Errorf("plain 'golden cross' observation should NOT redact: %+v", got.Violations)
	}
	signal := "Golden cross signal triggered yesterday — typically bullish."
	got := Scan(signal)
	if !contains(got.Violations, "golden_cross_signal") {
		t.Errorf("'golden cross signal triggered' should redact: %+v", got.Violations)
	}
	if strings.Contains(strings.ToLower(got.Redacted), "signal triggered") {
		t.Errorf("redacted should drop 'signal triggered': %s", got.Redacted)
	}
}

// TestScanRedactsBreakoutImminent covers forward-looking
// "breakout imminent" / "breakdown expected" predictions.
// Past-tense "broke out above the 20-day high" stays untouched.
func TestScanRedactsBreakoutImminent(t *testing.T) {
	past := "NVDA broke out above the 20-day high on heavy volume."
	if got := Scan(past); got.HasViolations() {
		t.Errorf("past-tense breakout observation should NOT redact: %+v", got.Violations)
	}
	future := "A breakout is imminent based on the bull-flag pattern."
	got := Scan(future)
	if !contains(got.Violations, "breakout_imminent") {
		t.Errorf("'breakout imminent' should redact: %+v", got.Violations)
	}
}

// TestScanRedactsStrongBuyRating covers the sell-side analyst
// rating label that surviving in formal IA research can stay
// but in Publisher mode has to become a model state.
func TestScanRedactsStrongBuyRating(t *testing.T) {
	in := "We are issuing a Strong Buy rating on TSLA."
	got := Scan(in)
	if !contains(got.Violations, "strong_buy_rating") {
		t.Errorf("'Strong Buy rating' should redact: %+v", got.Violations)
	}
	if strings.Contains(got.Redacted, "Strong Buy rating") {
		t.Errorf("redacted should drop 'Strong Buy rating': %s", got.Redacted)
	}
}

// TestScanRedactsGoLong covers the "go long" / "go short" trade
// instruction phrases.
func TestScanRedactsGoLong(t *testing.T) {
	in := "Investors should go long NVDA at current levels."
	got := Scan(in)
	if !contains(got.Violations, "go_long_short") {
		t.Errorf("'go long' should redact: %+v", got.Violations)
	}
}

// TestScanRedactsChineseTechnicalDirectives covers the Chinese
// mirrors of the TA red lines. Without explicit coverage the
// English-only rules silently miss "目标价" / "买点" / "金叉信号" /
// "建议买入".
func TestScanRedactsChineseTechnicalDirectives(t *testing.T) {
	cases := []struct {
		in   string
		rule string
	}{
		{"目标价为$150，可以建仓。", "target_price_cn"},
		{"止损位设在$120以下。", "stop_loss_position_cn"},
		{"买点在 50 日均线附近。", "buy_sell_point_cn"},
		{"建议买入并加仓至 20%。", "suggestion_buysell_cn"},
		{"金叉信号确认，趋势向上。", "golden_cross_signal_cn"},
		{"突破在即，关注成交量。", "breakout_imminent_cn"},
	}
	for _, c := range cases {
		got := Scan(c.in)
		if !contains(got.Violations, c.rule) {
			t.Errorf("rule %s should fire on %q, violations=%+v", c.rule, c.in, got.Violations)
		}
	}
}

// helper

func contains(vs []Violation, rule string) bool {
	for _, v := range vs {
		if v.Rule == rule {
			return true
		}
	}
	return false
}
