package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

func newTestPersona() MasterPersona {
	return MasterPersona{
		Key:        "test_persona",
		NameZh:     "测试",
		NameEn:     "Test",
		Philosophy: "test philosophy",
	}
}

// TestMasterPromptIncludesNameWhenPresent guards the user-facing
// contract that when the loader resolves an issuer's short name
// (e.g. "德科立" for 688205), the master prompt prefixes the symbol
// with a ``name: ...`` line so the LLM reasons about the company
// rather than a bare ticker.
func TestMasterPromptIncludesNameWhenPresent(t *testing.T) {
	a, err := NewMasterAgent(newTestPersona(), nil)
	if err != nil {
		t.Fatalf("NewMasterAgent: %v", err)
	}
	in := MasterInput{
		Symbol: "688205",
		Name:   "德科立",
		Market: "a_share",
		AsOf:   time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC),
	}
	prompt := a.buildUserPrompt(in)
	if !strings.Contains(prompt, "symbol: 688205") {
		t.Errorf("prompt missing symbol line: %q", prompt)
	}
	if !strings.Contains(prompt, "name: 德科立") {
		t.Errorf("prompt missing 'name: 德科立' line:\n%s", prompt)
	}
}

// TestMasterPromptOmitsNameWhenAbsent is the negative case — no
// stray "name:" line for symbols the loader didn't resolve.
func TestMasterPromptOmitsNameWhenAbsent(t *testing.T) {
	a, err := NewMasterAgent(newTestPersona(), nil)
	if err != nil {
		t.Fatalf("NewMasterAgent: %v", err)
	}
	in := MasterInput{Symbol: "AAPL", Market: "us"}
	prompt := a.buildUserPrompt(in)
	if strings.Contains(prompt, "name:") {
		t.Errorf("prompt unexpectedly contains 'name:' line:\n%s", prompt)
	}
}

// TestMasterPromptLabelsFiscalPeriods is the regression guard for
// the 688205 "stale annual verdict" bug. When the loader supplies
// both an annual fiscal period and a fresher interim period, the
// master prompt must label both so the persona reads
// "fund.earnings_growth_yoy=-0.29" as the 2025 annual and
// "fund.earnings_growth_yoy_latest=+0.35" as the Q1 inflection,
// NOT as a single contradictory time series.
func TestMasterPromptLabelsFiscalPeriods(t *testing.T) {
	a, err := NewMasterAgent(newTestPersona(), nil)
	if err != nil {
		t.Fatalf("NewMasterAgent: %v", err)
	}
	in := MasterInput{
		Symbol: "688205",
		Name:   "德科立",
		Fundamentals: &FundamentalsBlock{
			AnnualPeriod: "2025-12-31",
			LatestPeriod: "2026-03-31",
			Metrics: map[string]float64{
				"revenue_growth_yoy":         0.1099,
				"revenue_growth_yoy_latest":  0.2797,
				"earnings_growth_yoy":        -0.2877,
				"earnings_growth_yoy_latest": 0.3508,
			},
		},
	}
	prompt := a.buildUserPrompt(in)
	for _, want := range []string{
		"annual_period: 2025-12-31",
		"latest_period: 2026-03-31",
		"fund.revenue_growth_yoy=0.1099",
		"fund.revenue_growth_yoy_latest=0.2797",
		"fund.earnings_growth_yoy=-0.2877",
		"fund.earnings_growth_yoy_latest=0.3508",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

// TestMasterPromptOmitsPeriodLabelsWhenAbsent — when the upstream
// only carries an annual (no fresher interim), we should NOT emit
// a stray "latest_period:" line.
func TestMasterPromptOmitsPeriodLabelsWhenAbsent(t *testing.T) {
	a, err := NewMasterAgent(newTestPersona(), nil)
	if err != nil {
		t.Fatalf("NewMasterAgent: %v", err)
	}
	in := MasterInput{
		Symbol: "AAPL",
		Fundamentals: &FundamentalsBlock{
			AnnualPeriod: "2025-09-30",
			Metrics:      map[string]float64{"roe": 1.5},
		},
	}
	prompt := a.buildUserPrompt(in)
	if !strings.Contains(prompt, "annual_period: 2025-09-30") {
		t.Errorf("prompt missing annual_period line:\n%s", prompt)
	}
	if strings.Contains(prompt, "latest_period:") {
		t.Errorf("prompt unexpectedly carries latest_period line:\n%s", prompt)
	}
}

// TestMasterAnalyzePopulatesSymbolName confirms the report row
// echoes back the input name so the API layer can surface it
// without diving into the prompt.
func TestMasterAnalyzePopulatesSymbolName(t *testing.T) {
	a, err := NewMasterAgent(newTestPersona(), nil) // nil LLM → returns fallback shell
	if err != nil {
		t.Fatalf("NewMasterAgent: %v", err)
	}
	rep, err := a.Analyze(context.Background(), MasterInput{Symbol: "688205", Name: "德科立"})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.SymbolName != "德科立" {
		t.Errorf("SymbolName got %q want %q", rep.SymbolName, "德科立")
	}
	if rep.Symbol != "688205" {
		t.Errorf("Symbol got %q want %q", rep.Symbol, "688205")
	}
}

// TestMasterPromptIncludesListingTenure is the regression guard for
// rule 7 (listing-tenure framing). When the loader resolves a
// company's listing date, the master prompt must surface both the
// ISO date and a decimal-year tenure so the persona can apply the
// "listing_lt_10y:N年" tag instead of stamping "data_unavailable"
// on every 10-year-horizon must_have_criteria. 德科立 (688205)
// IPO'd 2022-08-09 — the canonical 次新股 case the rule was
// written for.
func TestMasterPromptIncludesListingTenure(t *testing.T) {
	a, err := NewMasterAgent(newTestPersona(), nil)
	if err != nil {
		t.Fatalf("NewMasterAgent: %v", err)
	}
	in := MasterInput{
		Symbol: "688205",
		Name:   "德科立",
		Fundamentals: &FundamentalsBlock{
			AnnualPeriod: "2025-12-31",
			ListingDate:  "2022-08-09",
			ListingYears: 3.83,
			Metrics:      map[string]float64{"roe": 0.0309},
		},
	}
	prompt := a.buildUserPrompt(in)
	for _, want := range []string{
		"listing_date: 2022-08-09",
		"listing_years: 3.83",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

// TestMasterPromptOmitsListingTenureWhenAbsent — the inverse:
// providers that don't resolve the listing date should NOT
// produce a stray "listing_date:" / "listing_years:" line that
// the LLM might mistake for a zero-year-old company.
func TestMasterPromptOmitsListingTenureWhenAbsent(t *testing.T) {
	a, err := NewMasterAgent(newTestPersona(), nil)
	if err != nil {
		t.Fatalf("NewMasterAgent: %v", err)
	}
	in := MasterInput{
		Symbol: "AAPL",
		Fundamentals: &FundamentalsBlock{
			AnnualPeriod: "2025-09-30",
			Metrics:      map[string]float64{"roe": 1.5},
		},
	}
	prompt := a.buildUserPrompt(in)
	if strings.Contains(prompt, "listing_date:") {
		t.Errorf("prompt unexpectedly carries 'listing_date:' line:\n%s", prompt)
	}
	if strings.Contains(prompt, "listing_years:") {
		t.Errorf("prompt unexpectedly carries 'listing_years:' line:\n%s", prompt)
	}
}

// TestMasterSystemPromptCarriesNewRules pins the three new
// behavioural rules onto the system prompt so a drive-by edit
// can't silently drop them. The rules are the user-visible fix
// for three real critique points raised against the 688205
// verdict:
//
//	rule 5 — every cited percentage must carry an explicit
//	         period label (no more "最新季度增速" ambiguity).
//	rule 6 — opposite-sign revenue vs earnings growth with
//	         |gap| >= 20pp must surface in key_risks (增收不
//	         增利 warning was being buried in thesis prose).
//	rule 7 — sub-10-year listings should be tagged
//	         "listing_lt_10y" rather than penalised as
//	         "data_unavailable: history.10yr".
//
// We assert on the *anchor strings* the rules use, not on the
// full sentence, so cosmetic wording tweaks don't break the test
// while structural drift will.
func TestMasterSystemPromptCarriesNewRules(t *testing.T) {
	a, err := NewMasterAgent(newTestPersona(), nil)
	if err != nil {
		t.Fatalf("NewMasterAgent: %v", err)
	}
	sys := a.buildSystemPrompt()
	for ruleName, anchor := range map[string]string{
		"rule5_period_label":  "YYYY年Q[1-4]",
		"rule5_forbid_vague":  "禁止使用 '最新季度'",
		"rule6_quality_gap":   "增收不增利",
		"rule6_pp_threshold":  "20 个百分点",
		"rule7_listing_tag":   "listing_lt_10y",
		"rule7_listing_field": "listing_years",
		// rule 8 must mandate THREE things per *_latest citation
		// (period label, announce date, absolute value) AND name
		// the citation field explicitly so we can grep-audit the
		// generated reports later. The "动能反转" anchor enforces
		// the secondary YoY-vs-QoQ divergence clause.
		"rule8_announce_field": "latest_announce_date",
		"rule8_period_label":   "2026Q1",
		"rule8_announce_date":  "2026-04-28",
		"rule8_absolute_value": "2.54 亿元",
		"rule8_momentum_clause": "动能反转",
	} {
		if !strings.Contains(sys, anchor) {
			t.Errorf("%s missing anchor %q in system prompt:\n--- system prompt ---\n%s\n--- end ---", ruleName, anchor, sys)
		}
	}
}

// TestMasterPromptEmitsCitationMetadata is the regression guard
// for the citation block in the *user* prompt (rule 8's data
// dependency). Without latest_announce_date in the prompt itself
// the LLM has nothing to cite, so even a perfectly-worded rule 8
// produces empty "公告日 ?" placeholders. We pin both the
// metadata lines and that the numeric absolute-value fields land
// in the fund.* dump.
func TestMasterPromptEmitsCitationMetadata(t *testing.T) {
	a, err := NewMasterAgent(newTestPersona(), nil)
	if err != nil {
		t.Fatalf("NewMasterAgent: %v", err)
	}
	in := MasterInput{
		Symbol: "688205",
		Name:   "德科立",
		Fundamentals: &FundamentalsBlock{
			AnnualPeriod:       "2025-12-31",
			LatestPeriod:       "2026-03-31",
			LatestAnnounceDate: "2026-04-28",
			LatestSource:       "eastmoney_yjbb",
			Metrics: map[string]float64{
				"revenue_growth_yoy_latest": 0.2797,
				"latest_revenue":            254444250,
				"latest_net_income":         19639010,
				"latest_revenue_qoq":        -0.0963,
				"latest_net_income_qoq":     -0.3755,
				"gross_margin_latest":       0.2573,
			},
		},
	}
	prompt := a.buildUserPrompt(in)
	for _, want := range []string{
		// Citation metadata lines — these are the anchors the
		// LLM cites verbatim. Absent either one, rule 8's "公告
		// 日" requirement is unfulfillable.
		"latest_announce_date: 2026-04-28",
		"latest_source: eastmoney_yjbb",
		// Absolute-value fund.* entries — without these the LLM
		// can quote a percent but cannot quote the numerator.
		// Go's %g strips trailing zeros, so 254_444_250 ->
		// "2.5444425e+08" (NOT "2.54442425e+08").
		"fund.latest_revenue=2.5444425e+08",
		"fund.latest_net_income=1.963901e+07",
		// QoQ deltas — momentum-reversal evidence.
		"fund.latest_revenue_qoq=-0.0963",
		"fund.latest_net_income_qoq=-0.3755",
		"fund.gross_margin_latest=0.2573",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

// TestMasterPromptOmitsCitationMetadataWhenAbsent is the negative
// case — providers that don't ship the citation block (older
// sidecar, eastmoney_yjbb upstream outage) must NOT produce stray
// "latest_announce_date:" or "latest_source:" lines, which the LLM
// would otherwise quote as "公告日 " (blank) and break rule 8.
func TestMasterPromptOmitsCitationMetadataWhenAbsent(t *testing.T) {
	a, err := NewMasterAgent(newTestPersona(), nil)
	if err != nil {
		t.Fatalf("NewMasterAgent: %v", err)
	}
	in := MasterInput{
		Symbol: "AAPL",
		Fundamentals: &FundamentalsBlock{
			AnnualPeriod: "2025-09-30",
			Metrics:      map[string]float64{"roe": 1.5},
		},
	}
	prompt := a.buildUserPrompt(in)
	for _, dont := range []string{
		"latest_announce_date:",
		"latest_source:",
	} {
		if strings.Contains(prompt, dont) {
			t.Errorf("prompt unexpectedly carries %q:\n%s", dont, prompt)
		}
	}
}
