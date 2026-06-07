package decision

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildContributionsCapturesPresentAndCited(t *testing.T) {
	tr := Fingerprint(DecisionInput{
		Universe:        []string{"A"},
		QuantSnapshots:  []SymbolQuantSnapshot{{}},
		UniverseRanking: []SymbolRanking{{}},
		QualityScores:   []SymbolQualityScore{{Symbol: "A"}},
		ValueScores:     []SymbolValueScore{{Symbol: "A"}},
	})
	reasoning := `
		Sizing AAPL at qtyPct=0.05 because quantSnapshots regime=trend_up
		(ATR=2.1) and universeRanking puts it in Q1. The qualityScores
		composite is +1.2 driven by profitabilityZ, while the valueScores
		(HML) overlay shows the name is mid-quartile — still Quality at a
		Reasonable Price.
	`
	got := BuildContributions(tr, reasoning)
	// Both present blocks AND cited blocks should overlap.
	mustContain(t, got.Present, "quantSnapshots", "universeRanking", "qualityScores", "valueScores")
	mustContain(t, got.Cited, "quantSnapshots", "universeRanking", "qualityScores", "valueScores")
	// Counts should reflect the trace.
	if got.Counts["qualityScores"] != 1 {
		t.Errorf("expected qualityScores count=1, got %d", got.Counts["qualityScores"])
	}
	if got.Counts["valueScores"] != 1 {
		t.Errorf("expected valueScores count=1, got %d", got.Counts["valueScores"])
	}
	if !strings.HasPrefix(got.Signature, "pres=") {
		t.Errorf("signature missing pres= prefix: %q", got.Signature)
	}
	if !strings.Contains(got.Signature, "cited=") {
		t.Errorf("signature missing cited= section: %q", got.Signature)
	}
}

func TestBuildContributionsRecognisesAcademicShorthand(t *testing.T) {
	// PM is allowed to write "QMJ", "HML", "BAB", "PEAD" etc.
	// instead of the verbose block name. Each must map back to
	// the canonical block.
	tr := Fingerprint(DecisionInput{
		QualityScores: []SymbolQualityScore{{Symbol: "A"}},
		ValueScores:   []SymbolValueScore{{Symbol: "A"}},
		LowBetaScores: []SymbolLowBetaScore{{Symbol: "A"}},
		PEAD:          &PEADSnapshot{Signals: []PEADSignal{{Symbol: "A", SurprisePercent: 0.05, State: "continuing"}}},
	})
	reasoning := `Tilting toward the QMJ + HML overlap, with a BAB defensive
		layer because riskBudget shows drawdown_throttle. PEAD on AAPL is
		still continuing on the May beat.`
	got := BuildContributions(tr, reasoning)
	mustContain(t, got.Cited, "qualityScores", "valueScores", "lowBetaScores", "pead", "riskBudget")
}

// TestBuildContributionsRecognisesChineseAliases captures the live
// regression we hit on 2026-05-24: the PM wrote per-action
// reasoning entirely in Chinese (动量排名 Q1, 低Beta得分, 量化快照,
// 新闻情绪, 辩论结论) and the English-only vocabulary scored
// n_cited=0 across the board. Each Chinese alias added to
// citationVocabulary needs a covering row here so future
// regex tightening doesn't silently break the bilingual mode.
func TestBuildContributionsRecognisesChineseAliases(t *testing.T) {
	tr := Fingerprint(DecisionInput{
		QuantSnapshots:  []SymbolQuantSnapshot{{}},
		UniverseRanking: []SymbolRanking{{}},
		LowBetaScores:   []SymbolLowBetaScore{{Symbol: "A"}},
		NewsCatalysts:   []SymbolNewsCatalysts{{}},
	})
	reasoning := `
		SNDK在动量排名中位列Q1，辩论结论看涨且新闻情绪偏向乐观。
		多级别上升趋势支持其Q4低Beta得分的进攻性敞口，按量化快照
		上限0.035买入。回撤throttle未启动，敞口允许。
	`
	got := BuildContributions(tr, reasoning)
	// "辩论结论" maps to roundtableStance (the overall debate
	// verdict); per-symbol verdicts would use "个股 verdict".
	mustContain(t, got.Cited,
		"universeRanking", "roundtableStance", "newsSentiment",
		"lowBetaScores", "quantSnapshots", "riskBudget", "exposure",
	)
}

// TestBuildContributionsRecognisesAdditionalChinesePhrases captures
// the second-pass regression — the PM also writes "宇宙排名 Q1",
// "排名 Q2", "分歧票数=2", and bare technical-indicator
// vocabulary (MACD / KDJ / RSI) that originates from the
// quantSnapshots block. Each alias added in the second pass
// needs a covering test row.
func TestBuildContributionsRecognisesAdditionalChinesePhrases(t *testing.T) {
	tr := Fingerprint(DecisionInput{
		QuantSnapshots:  []SymbolQuantSnapshot{{}},
		UniverseRanking: []SymbolRanking{{}},
		LowBetaScores:   []SymbolLowBetaScore{{Symbol: "A"}},
	})
	reasoning := `
		MU: 辩论结论中立，分歧票数达2票；MACD为负且KDJ偏弱。
		宇宙排名Q1，低Beta得分Q1，但动能衰退迹象明显，按规则降级为观望。
	`
	got := BuildContributions(tr, reasoning)
	mustContain(t, got.Cited,
		"roundtableStance",   // 辩论结论
		"symbolVerdicts",     // 分歧票数
		"quantSnapshots",     // MACD / KDJ
		"universeRanking",    // 宇宙排名 / 排名Q1
		"lowBetaScores",      // 低Beta得分
	)
}

// PM prose mentioning volume / support / resistance vocabulary
// must attribute back to the quantSnapshots block — these are
// downstream of the technical analyst's hard-vote signals
// (`relative_volume`, `breakout`, `support_distance_pct`,
// `resistance_distance_pct`) added in W15.
func TestBuildContributionsRecognisesVolumeAndSupportVocabulary(t *testing.T) {
	cases := []struct {
		name      string
		reasoning string
	}{
		{"english_relative_volume", "AAPL relative volume 2.1x; respect the volume surge."},
		{"english_breakout", "MU just printed a breakout above the 20-day high."},
		{"english_support", "Holding above key support level keeps the trend alive."},
		{"english_resistance", "Watch the resistance line at 198 — rejection is plausible."},
		{"chinese_volume", "AAPL 放量上行，成交量是平均的两倍，量能配合突破。"},
		{"chinese_breakdown", "MU 跌破了前期低点，止损要紧。"},
		{"chinese_support", "支撑位在 95 一线，破位再考虑减仓。"},
		{"chinese_resistance", "阻力位 200 附近压力较大，先观望。"},
	}
	tr := Fingerprint(DecisionInput{
		QuantSnapshots: []SymbolQuantSnapshot{{}},
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildContributions(tr, tc.reasoning)
			mustContain(t, got.Cited, "quantSnapshots")
		})
	}
}

// W16-3 — macroBriefing citation vocabulary. Pre-fix the
// macroBriefing block had a presence flag in the trace but no
// regex in citationVocabulary, so PM prose like "rate-cut
// tailwind" could never be attributed back. These cases exercise
// the new English + Chinese phrases.
func TestBuildContributionsRecognisesMacroBriefingVocabulary(t *testing.T) {
	cases := []struct {
		name      string
		reasoning string
	}{
		{"english_macro_brief", "Macro brief is risk-off into Fed week — defer the AAPL add."},
		{"english_rate_cuts", "Rate cuts in the macro briefing reinforce the long-duration tilt."},
		{"english_dovish", "Dovish macro view supports a tactical add on QQQ."},
		{"english_risk_on", "Risk-on macro tilt; size the buy at the per-symbol ceiling."},
		{"english_macro_headwinds", "Macro headwinds (rate hikes) cap the growth-stock conviction."},
		{"chinese_macro", "宏观简报偏避险，先观察。"},
		{"chinese_rate_cut", "降息周期下加仓优质成长。"},
		{"chinese_central_bank", "美联储维持利率，宏观环境偏中性。"},
		{"chinese_risk_aversion", "避险情绪上升，降低高Beta敞口。"},
	}
	tr := Fingerprint(DecisionInput{
		MacroBriefing: "Fed weights rate cuts; growth basket bid.",
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildContributions(tr, tc.reasoning)
			mustContain(t, got.Cited, "macroBriefing")
		})
	}
}

func TestBuildContributionsEmptyReasoningHasNoCited(t *testing.T) {
	tr := Fingerprint(DecisionInput{
		QualityScores: []SymbolQualityScore{{Symbol: "A"}},
	})
	got := BuildContributions(tr, "")
	if len(got.Cited) != 0 {
		t.Errorf("expected empty cited slice, got %v", got.Cited)
	}
	if len(got.Present) == 0 {
		t.Errorf("present should still flow through")
	}
}

func TestBuildContributionsCaseInsensitive(t *testing.T) {
	tr := Fingerprint(DecisionInput{
		QualityScores: []SymbolQualityScore{{Symbol: "A"}},
	})
	got := BuildContributions(tr, "the QUALITY SCORES on AAPL look strong")
	mustContain(t, got.Cited, "qualityScores")
}

func TestBuildContributionsFlagsCitedButAbsent(t *testing.T) {
	// PM cited "qualityScores" but the trace shows it was absent
	// from the input. The Cited slice still includes the block —
	// surfacing it on the dashboard so the operator can see the
	// drift (prompt referenced a block the wiring layer didn't
	// build).
	tr := Fingerprint(DecisionInput{
		QuantSnapshots: []SymbolQuantSnapshot{{}},
	})
	got := BuildContributions(tr, "qualityScores indicates Q1 — sizing at ceiling")
	mustContain(t, got.Cited, "qualityScores")
	mustContain(t, got.Absent, "qualityScores")
	mustNotContain(t, got.Present, "qualityScores")
}

func TestBuildContributionsCountsStripZeros(t *testing.T) {
	tr := Fingerprint(DecisionInput{
		Universe: []string{"A", "B"},
	})
	got := BuildContributions(tr, "")
	if _, has := got.Counts["qualityScores"]; has {
		t.Errorf("zero-count qualityScores must not appear: %v", got.Counts)
	}
	if got.Counts["universe"] != 2 {
		t.Errorf("non-zero universe count must appear: %v", got.Counts)
	}
}

func TestBuildContributionsCitedOrderingPresentFirstThenAlphabetical(t *testing.T) {
	tr := Fingerprint(DecisionInput{
		QuantSnapshots: []SymbolQuantSnapshot{{}},
		QualityScores:  []SymbolQualityScore{{Symbol: "A"}},
	})
	// All blocks in vocabulary cited; expected order = the
	// present-blocks listing first, then alphabetical residue.
	reasoning := `qualityScores + valueScores + lowBetaScores + PEAD +
		quantSnapshots + universeRanking + roundtable stance + bull case`
	got := BuildContributions(tr, reasoning)
	// quantSnapshots + qualityScores are PRESENT; they MUST come
	// first in the cited slice.
	if got.Cited[0] != "quantSnapshots" && got.Cited[1] != "quantSnapshots" {
		t.Errorf("present-and-cited blocks must lead Cited, got %v", got.Cited)
	}
}

func TestEncodeToJSONProducesExpectedKeys(t *testing.T) {
	tr := Fingerprint(DecisionInput{
		QualityScores: []SymbolQualityScore{{Symbol: "A"}},
	})
	c := BuildContributions(tr, "qualityScores looks Q1")
	raw, err := c.EncodeToJSON()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	for _, key := range []string{"present", "absent", "cited", "counts", "signature"} {
		if _, has := out[key]; !has {
			t.Errorf("missing key %q in payload: %s", key, raw)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustContain(t *testing.T, slice []string, items ...string) {
	t.Helper()
	for _, item := range items {
		found := false
		for _, s := range slice {
			if s == item {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in %v", item, slice)
		}
	}
}

func mustNotContain(t *testing.T, slice []string, items ...string) {
	t.Helper()
	for _, item := range items {
		for _, s := range slice {
			if s == item {
				t.Errorf("unexpected %q in %v", item, slice)
			}
		}
	}
}
