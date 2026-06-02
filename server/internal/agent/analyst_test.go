// analyst_test.go — covers the four S8.1 analysts and the
// shared helpers in analyst.go.

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// --- Shared helpers --------------------------------------------------------

// fakeLLM is the test double for the package's LLMClient interface.
// reply / err are returned verbatim from Complete; calls records
// each invocation so tests can assert on the prompt content.
type fakeLLM struct {
	reply string
	err   error
	calls []fakeLLMCall
}

type fakeLLMCall struct {
	system string
	user   string
}

func (f *fakeLLM) Complete(_ context.Context, sys, user string) (string, error) {
	f.calls = append(f.calls, fakeLLMCall{system: sys, user: user})
	if f.err != nil {
		return "", f.err
	}
	return f.reply, nil
}

// fixedClock returns the same Time on every call. Lets us assert
// on AnalystReport.GeneratedAt without flakiness.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

var testClock = time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)

func newClockOption() AnalystOption { return WithAnalystClock(fixedClock(testClock)) }

// --- AnalystCategory + ParseAnalystCategory --------------------------------

func TestParseAnalystCategory(t *testing.T) {
	cases := map[string]struct {
		want   AnalystCategory
		wantOk bool
	}{
		"fundamentals": {CategoryFundamentals, true},
		"sentiment":    {CategorySentiment, true},
		"NEWS":         {CategoryNews, true},
		" technical ":  {CategoryTechnical, true},
		"bogus":        {"", false},
		"":             {"", false},
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			got, ok := ParseAnalystCategory(in)
			if ok != want.wantOk || got != want.want {
				t.Errorf("ParseAnalystCategory(%q) = (%q, %v), want (%q, %v)",
					in, got, ok, want.want, want.wantOk)
			}
		})
	}
}

// --- AnalystReport.Validate ------------------------------------------------

func TestAnalystReport_Validate_Rejects(t *testing.T) {
	base := AnalystReport{
		Symbol:      "AAPL",
		Category:    CategoryFundamentals,
		Direction:   DirectionBullish,
		Confidence:  60,
		Thesis:      "ok",
		KeyFindings: []string{"a"},
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("base valid: %v", err)
	}
	bad := []AnalystReport{
		{Category: CategoryFundamentals, Direction: DirectionBullish, Thesis: "ok", KeyFindings: []string{"a"}},
		{Symbol: "AAPL", Category: "bogus", Direction: DirectionBullish, Thesis: "ok", KeyFindings: []string{"a"}, Confidence: 60},
		{Symbol: "AAPL", Category: CategoryFundamentals, Direction: "bogus", Thesis: "ok", KeyFindings: []string{"a"}, Confidence: 60},
		{Symbol: "AAPL", Category: CategoryFundamentals, Direction: DirectionBullish, Thesis: "ok", KeyFindings: []string{"a"}, Confidence: 999},
		{Symbol: "AAPL", Category: CategoryFundamentals, Direction: DirectionBullish, KeyFindings: []string{"a"}, Confidence: 60},
		{Symbol: "AAPL", Category: CategoryFundamentals, Direction: DirectionBullish, Thesis: "ok", Confidence: 60},
	}
	for i, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("case %d: expected validation error, got nil for %+v", i, r)
		}
	}
}

// --- parseLLMJSONReport ----------------------------------------------------

func TestParseLLMJSONReport(t *testing.T) {
	cases := map[string]struct {
		in        string
		wantDir   string
		wantConf  int
		wantErr   bool
		wantThesis string
	}{
		"bare json": {
			in: `{"direction":"bullish","confidence":80,"thesis":"good","key_findings":["a"],"risks":[]}`,
			wantDir: "bullish", wantConf: 80, wantThesis: "good",
		},
		"with fences": {
			in: "```json\n{\"direction\":\"bearish\",\"confidence\":40,\"thesis\":\"meh\",\"key_findings\":[\"a\"],\"risks\":[]}\n```",
			wantDir: "bearish", wantConf: 40, wantThesis: "meh",
		},
		"with preamble + fences without lang tag": {
			in:      "Sure, here you go:\n```\n{\"direction\":\"neutral\",\"confidence\":30,\"thesis\":\"flat\",\"key_findings\":[\"a\"],\"risks\":[]}\n```\nDone.",
			wantDir: "neutral", wantConf: 30, wantThesis: "flat",
		},
		"empty reply": {
			in: "", wantErr: true,
		},
		"no braces": {
			in: "nothing structured here", wantErr: true,
		},
		"malformed json": {
			in: `{"direction": "bullish", "confidence": "not-a-number"}`, wantErr: true,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := parseLLMJSONReport(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if got.Direction != c.wantDir {
				t.Errorf("Direction = %q, want %q", got.Direction, c.wantDir)
			}
			if got.Confidence != c.wantConf {
				t.Errorf("Confidence = %d, want %d", got.Confidence, c.wantConf)
			}
			if got.Thesis != c.wantThesis {
				t.Errorf("Thesis = %q, want %q", got.Thesis, c.wantThesis)
			}
		})
	}
}

// --- normaliseDirection ----------------------------------------------------

func TestNormaliseDirection(t *testing.T) {
	cases := map[string]Direction{
		"bullish":  DirectionBullish,
		"Bull":     DirectionBullish,
		"buy":      DirectionBullish,
		"LONG":     DirectionBullish,
		"bear":     DirectionBearish,
		"sell":     DirectionBearish,
		"short":    DirectionBearish,
		"neutral":  DirectionNeutral,
		"":         DirectionNeutral,
		"whatever": DirectionNeutral,
	}
	for in, want := range cases {
		if got := normaliseDirection(in); got != want {
			t.Errorf("normaliseDirection(%q) = %v, want %v", in, got, want)
		}
	}
}

// --- mergeDirectionWithRule ------------------------------------------------

func TestMergeDirectionWithRule(t *testing.T) {
	cases := []struct {
		rule, llm, want Direction
	}{
		{DirectionBullish, DirectionBullish, DirectionBullish},
		{DirectionBullish, DirectionNeutral, DirectionBullish},
		{DirectionNeutral, DirectionBullish, DirectionNeutral},
		// Conflict: rule wins.
		{DirectionBullish, DirectionBearish, DirectionBullish},
		{DirectionBearish, DirectionBullish, DirectionBearish},
	}
	for _, c := range cases {
		if got := mergeDirectionWithRule(c.rule, c.llm); got != c.want {
			t.Errorf("merge(rule=%v, llm=%v) = %v, want %v", c.rule, c.llm, got, c.want)
		}
	}
}

// --- FundamentalsAnalyst ---------------------------------------------------

func TestFundamentalsAnalyst_NoLLM_Fallback(t *testing.T) {
	a := NewFundamentalsAnalyst("fa-1", "Fundamentals Bot", "f-1", nil, newClockOption())
	input := AnalystInput{
		Symbol: "AAPL",
		AsOf:   testClock,
		Fundamentals: &FundamentalsBlock{
			QualityScore: &QualityScoreLite{CompositeZ: 1.2, ProfitabilityZ: 0.8, GrowthZ: 0.6, SafetyZ: 0.9, Quartile: 1},
			Metrics:      map[string]float64{"pe": 18, "roe": 0.25},
		},
	}
	rep, err := a.Analyze(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Direction != DirectionBullish {
		t.Errorf("direction = %v, want bullish (CompositeZ = 1.2)", rep.Direction)
	}
	if rep.Confidence < 50 {
		t.Errorf("confidence = %d, want >= 50 for |z|=1.2", rep.Confidence)
	}
	if rep.LLMModel != "fallback" {
		t.Errorf("LLMModel = %q, want fallback", rep.LLMModel)
	}
	if !strings.Contains(rep.Thesis, "AAPL") {
		t.Errorf("thesis does not mention symbol: %q", rep.Thesis)
	}
}

func TestFundamentalsAnalyst_LLMReply_OverridesThesis(t *testing.T) {
	llm := &fakeLLM{reply: `{"direction":"bullish","confidence":90,"thesis":"LLM thesis here","key_findings":["LLM finding"],"risks":["LLM risk"]}`}
	a := NewFundamentalsAnalyst("fa-1", "Bot", "f-1", llm, newClockOption())
	input := AnalystInput{
		Symbol: "AAPL", AsOf: testClock,
		Fundamentals: &FundamentalsBlock{QualityScore: &QualityScoreLite{CompositeZ: 1.0}},
	}
	rep, err := a.Analyze(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if rep.LLMModel != "llm" {
		t.Errorf("LLMModel = %q, want llm", rep.LLMModel)
	}
	if rep.Thesis != "LLM thesis here" {
		t.Errorf("thesis = %q", rep.Thesis)
	}
	if rep.Confidence != 90 {
		t.Errorf("confidence = %d, want 90", rep.Confidence)
	}
	if len(rep.KeyFindings) != 1 || rep.KeyFindings[0] != "LLM finding" {
		t.Errorf("KeyFindings = %v", rep.KeyFindings)
	}
	if len(llm.calls) != 1 {
		t.Fatalf("expected 1 LLM call, got %d", len(llm.calls))
	}
	if !strings.Contains(llm.calls[0].system, "fundamentals-focused") {
		t.Errorf("system prompt mismatch")
	}
}

func TestFundamentalsAnalyst_LLMConflict_RuleWins(t *testing.T) {
	// Quality says bearish (CompositeZ = -1.0) but LLM says bullish.
	// Rule must win.
	llm := &fakeLLM{reply: `{"direction":"bullish","confidence":80,"thesis":"narrative wins","key_findings":["a"],"risks":[]}`}
	a := NewFundamentalsAnalyst("fa-1", "Bot", "f-1", llm, newClockOption())
	input := AnalystInput{
		Symbol: "AAPL", AsOf: testClock,
		Fundamentals: &FundamentalsBlock{QualityScore: &QualityScoreLite{CompositeZ: -1.0}},
	}
	rep, err := a.Analyze(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Direction != DirectionBearish {
		t.Errorf("direction = %v, want bearish (rule wins)", rep.Direction)
	}
}

func TestFundamentalsAnalyst_LLMError_FallsBack(t *testing.T) {
	llm := &fakeLLM{err: errors.New("rate limited")}
	a := NewFundamentalsAnalyst("fa-1", "Bot", "f-1", llm, newClockOption())
	input := AnalystInput{
		Symbol: "AAPL", AsOf: testClock,
		Fundamentals: &FundamentalsBlock{QualityScore: &QualityScoreLite{CompositeZ: 0.8}},
	}
	rep, err := a.Analyze(context.Background(), input)
	if err != nil {
		t.Fatalf("expected fallback success, got err=%v", err)
	}
	if rep.LLMModel != "fallback" {
		t.Errorf("LLMModel = %q, want fallback", rep.LLMModel)
	}
	if rep.Direction != DirectionBullish {
		t.Errorf("direction = %v, want bullish", rep.Direction)
	}
}

func TestFundamentalsAnalyst_NoData_NeutralFloor(t *testing.T) {
	a := NewFundamentalsAnalyst("fa-1", "Bot", "f-1", nil, newClockOption())
	rep, err := a.Analyze(context.Background(), AnalystInput{Symbol: "ZZZ", AsOf: testClock})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Direction != DirectionNeutral {
		t.Errorf("direction = %v, want neutral", rep.Direction)
	}
	if rep.Confidence != 20 {
		t.Errorf("confidence = %d, want 20 (floor)", rep.Confidence)
	}
}

// --- SentimentAnalyst ------------------------------------------------------

func TestSentimentAnalyst_NoLLM_Fallback(t *testing.T) {
	a := NewSentimentAnalyst("sa-1", "Sentiment Bot", "f-1", nil, newClockOption())
	input := AnalystInput{
		Symbol: "TSLA", AsOf: testClock,
		Sentiment: &SentimentBlock{
			Aggregate: SentimentAggregateLite{Average: 0.55, Count: 18, Polarity: "bullish"},
			RecentItems: []SentimentItemLite{
				{Title: "WSB pumping", Source: "reddit", Score: 0.7},
				{Title: "Reuters downgrade", Source: "reuters", Score: -0.5},
			},
			SourceBreakdown: map[string]int{"reddit": 12, "reuters": 3, "xueqiu": 3},
		},
	}
	rep, err := a.Analyze(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Direction != DirectionBullish {
		t.Errorf("direction = %v, want bullish (avg=0.55)", rep.Direction)
	}
	if rep.Confidence < 50 {
		t.Errorf("confidence = %d, want >= 50", rep.Confidence)
	}
}

func TestSentimentAnalyst_SourceBias_AddsRisk(t *testing.T) {
	a := NewSentimentAnalyst("sa-1", "Bot", "f-1", nil, newClockOption())
	input := AnalystInput{
		Symbol: "GME", AsOf: testClock,
		Sentiment: &SentimentBlock{
			Aggregate:       SentimentAggregateLite{Average: 0.6, Count: 20, Polarity: "bullish"},
			SourceBreakdown: map[string]int{"reddit": 16, "reuters": 2, "ft": 2}, // reddit > 70%
		},
	}
	rep, _ := a.Analyze(context.Background(), input)
	hasBias := false
	for _, r := range rep.Risks {
		if strings.Contains(r, "source bias") {
			hasBias = true
		}
	}
	if !hasBias {
		t.Errorf("expected source-bias risk, got risks=%v", rep.Risks)
	}
}

func TestSentimentAnalyst_FewItems_DampensConfidence(t *testing.T) {
	a := NewSentimentAnalyst("sa-1", "Bot", "f-1", nil, newClockOption())
	input := AnalystInput{
		Symbol: "ZZZ", AsOf: testClock,
		Sentiment: &SentimentBlock{
			Aggregate: SentimentAggregateLite{Average: 0.8, Count: 2, Polarity: "strongly bullish"},
		},
	}
	rep, _ := a.Analyze(context.Background(), input)
	if rep.Confidence > 50 {
		t.Errorf("confidence = %d, want dampened (<=50) for count=2", rep.Confidence)
	}
}

// --- NewsAnalyst -----------------------------------------------------------

func TestNewsAnalyst_EarningsBeat_Bullish(t *testing.T) {
	a := NewNewsAnalyst("na-1", "News Bot", "f-1", nil, newClockOption())
	input := AnalystInput{
		Symbol: "NVDA", AsOf: testClock,
		News: &NewsBlock{
			Headlines: []NewsHeadline{
				{Title: "NVDA beats Q1 revenue", Source: "reuters", PublishedAt: testClock},
			},
			MaterialEventTags: []string{"earnings_beat", "guidance_raise"},
		},
	}
	rep, err := a.Analyze(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Direction != DirectionBullish {
		t.Errorf("direction = %v, want bullish", rep.Direction)
	}
	if rep.Confidence < 50 {
		t.Errorf("confidence = %d, want >= 50", rep.Confidence)
	}
}

func TestNewsAnalyst_NegativeRegulator_Bearish(t *testing.T) {
	a := NewNewsAnalyst("na-1", "Bot", "f-1", nil, newClockOption())
	input := AnalystInput{
		Symbol: "BABA", AsOf: testClock,
		News: &NewsBlock{
			Headlines: []NewsHeadline{
				{Title: "Regulator fines BABA", Source: "scmp", PublishedAt: testClock},
			},
			MaterialEventTags: []string{"regulator_action_neg", "downgrade"},
		},
	}
	rep, _ := a.Analyze(context.Background(), input)
	if rep.Direction != DirectionBearish {
		t.Errorf("direction = %v, want bearish", rep.Direction)
	}
	hasNeg := false
	for _, r := range rep.Risks {
		if strings.Contains(r, "regulator_action_neg") {
			hasNeg = true
		}
	}
	if !hasNeg {
		t.Errorf("expected negative-event risk, got risks=%v", rep.Risks)
	}
}

func TestNewsAnalyst_HeadlinesNoTags_LowConvictionNeutral(t *testing.T) {
	a := NewNewsAnalyst("na-1", "Bot", "f-1", nil, newClockOption())
	input := AnalystInput{
		Symbol: "AAPL", AsOf: testClock,
		News: &NewsBlock{
			Headlines: []NewsHeadline{
				{Title: "AAPL hosts WWDC", Source: "bbg", PublishedAt: testClock},
			},
		},
	}
	rep, _ := a.Analyze(context.Background(), input)
	if rep.Direction != DirectionNeutral {
		t.Errorf("direction = %v, want neutral (no material tags)", rep.Direction)
	}
}

// --- TechnicalAnalyst ------------------------------------------------------

func TestTechnicalAnalyst_TrendUp_GoldenCross_Bullish(t *testing.T) {
	a := NewTechnicalAnalyst("ta-1", "Tech Bot", "f-1", nil, newClockOption())
	input := AnalystInput{
		Symbol: "SPY", AsOf: testClock,
		Technical: &TechnicalBlock{
			Snapshot: QuantSnapshotLite{Regime: "TrendUp", Close: 500, ATR14: 5, ATRPct: 1.0, PositionSizeCeilingPct: 5},
			Signals:  map[string]float64{"ma50_over_ma200": 1, "macd_hist": 0.3, "rsi14": 55},
		},
	}
	rep, err := a.Analyze(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Direction != DirectionBullish {
		t.Errorf("direction = %v, want bullish", rep.Direction)
	}
}

func TestTechnicalAnalyst_TrendDown_Bearish(t *testing.T) {
	a := NewTechnicalAnalyst("ta-1", "Bot", "f-1", nil, newClockOption())
	input := AnalystInput{
		Symbol: "ZZZ", AsOf: testClock,
		Technical: &TechnicalBlock{
			Snapshot: QuantSnapshotLite{Regime: "TrendDown", ATRPct: 6.0},
			Signals:  map[string]float64{"ma50_over_ma200": -1, "macd_hist": -0.4, "rsi14": 30},
		},
	}
	rep, _ := a.Analyze(context.Background(), input)
	if rep.Direction != DirectionBearish {
		t.Errorf("direction = %v, want bearish", rep.Direction)
	}
	hasVolRisk := false
	for _, r := range rep.Risks {
		if strings.Contains(r, "elevated vol") {
			hasVolRisk = true
		}
	}
	if !hasVolRisk {
		t.Errorf("expected elevated-vol risk for ATR%% = 6, got risks=%v", rep.Risks)
	}
}

func TestTechnicalAnalyst_Overbought_FlagsRSI(t *testing.T) {
	a := NewTechnicalAnalyst("ta-1", "Bot", "f-1", nil, newClockOption())
	input := AnalystInput{
		Symbol: "QQQ", AsOf: testClock,
		Technical: &TechnicalBlock{
			Snapshot: QuantSnapshotLite{Regime: "TrendUp"},
			Signals:  map[string]float64{"rsi14": 78, "macd_hist": 0.1, "ma50_over_ma200": 1},
		},
	}
	rep, _ := a.Analyze(context.Background(), input)
	hasOverbought := false
	for _, r := range rep.Risks {
		if strings.Contains(r, "overbought") {
			hasOverbought = true
		}
	}
	if !hasOverbought {
		t.Errorf("expected overbought risk, got risks=%v", rep.Risks)
	}
}

func TestTechnicalAnalyst_NoData_NeutralFloor(t *testing.T) {
	a := NewTechnicalAnalyst("ta-1", "Bot", "f-1", nil, newClockOption())
	rep, _ := a.Analyze(context.Background(), AnalystInput{Symbol: "ZZZ", AsOf: testClock})
	if rep.Direction != DirectionNeutral || rep.Confidence != 20 {
		t.Errorf("got (%v, %d), want (neutral, 20)", rep.Direction, rep.Confidence)
	}
}

// --- AnalystReport.ToBrief -------------------------------------------------

func TestAnalystReport_ToBrief_FocusMapping(t *testing.T) {
	cases := map[AnalystCategory]ResearchFocus{
		CategoryFundamentals: FocusFundamental,
		CategoryTechnical:    FocusQuant,
		CategoryNews:         FocusStock,
		CategorySentiment:    FocusStock,
	}
	for cat, wantFocus := range cases {
		r := AnalystReport{
			Symbol: "X", Category: cat, Direction: DirectionBullish,
			Confidence: 60, Thesis: "ok", KeyFindings: []string{"a"},
		}
		brief := r.ToBrief()
		if brief.Focus != wantFocus {
			t.Errorf("category %s → focus %s, want %s", cat, brief.Focus, wantFocus)
		}
	}
}
