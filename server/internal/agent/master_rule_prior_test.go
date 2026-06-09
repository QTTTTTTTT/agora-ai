package agent

import (
	"strings"
	"testing"
)

func TestBuildMasterRulePriorBuffettStyle(t *testing.T) {
	persona := MasterPersona{
		Key: "buffett",
		Raw: map[string]any{
			"must_have_criteria": map[string]any{
				"ROE_10yr_avg":     ">= 15%",
				"debt_to_equity":   "<= 0.5",
				"free_cash_flow":   "连续10年为正",
				"gross_margin":     "10年稳定，波动<5pp",
				"earnings_predictability": "高",
			},
		},
	}

	years := []YearlyMetricsLite{
		{Year: 2024, ReturnOnEquity: 0.20, FreeCashFlow: 2_000_000_000, GrossMargin: 0.42, EPS: 6.0},
		{Year: 2023, ReturnOnEquity: 0.18, FreeCashFlow: 1_800_000_000, GrossMargin: 0.41, EPS: 5.3},
		{Year: 2022, ReturnOnEquity: 0.17, FreeCashFlow: 1_500_000_000, GrossMargin: 0.40, EPS: 4.8},
		{Year: 2021, ReturnOnEquity: 0.16, FreeCashFlow: 1_200_000_000, GrossMargin: 0.39, EPS: 4.2},
	}
	block := BuildMasterRulePrior(persona, &FundamentalsBlock{
		History: years,
		Metrics: map[string]float64{"debt_to_equity": 0.3},
	})
	if block == nil {
		t.Fatalf("expected non-nil prior block")
	}
	statuses := map[string]string{}
	for _, it := range block.Items {
		statuses[it.Key] = it.Status
	}
	if statuses["ROE_10yr_avg"] != "PASS" {
		t.Fatalf("expected ROE PASS, got %q (items=%+v)", statuses["ROE_10yr_avg"], block.Items)
	}
	if statuses["debt_to_equity"] != "PASS" {
		t.Fatalf("expected debt_to_equity PASS, got %q", statuses["debt_to_equity"])
	}
	if statuses["free_cash_flow"] != "PASS" {
		t.Fatalf("expected free_cash_flow PASS, got %q", statuses["free_cash_flow"])
	}
	if statuses["earnings_predictability"] != "PASS" {
		t.Fatalf("expected earnings_predictability PASS, got %q", statuses["earnings_predictability"])
	}
}

func TestBuildMasterRulePriorFailsOnLowROE(t *testing.T) {
	persona := MasterPersona{
		Key: "buffett",
		Raw: map[string]any{
			"must_have_criteria": map[string]any{
				"ROE_10yr_avg":   ">= 15%",
				"debt_to_equity": "<= 0.5",
			},
		},
	}
	years := []YearlyMetricsLite{
		{Year: 2024, ReturnOnEquity: 0.08},
		{Year: 2023, ReturnOnEquity: 0.05},
		{Year: 2022, ReturnOnEquity: 0.07},
	}
	block := BuildMasterRulePrior(persona, &FundamentalsBlock{
		History: years,
		Metrics: map[string]float64{"debt_to_equity": 1.2},
	})
	if block == nil {
		t.Fatalf("expected non-nil prior block")
	}
	statuses := map[string]string{}
	for _, it := range block.Items {
		statuses[it.Key] = it.Status
	}
	if statuses["ROE_10yr_avg"] != "FAIL" {
		t.Fatalf("expected ROE FAIL, got %q", statuses["ROE_10yr_avg"])
	}
	if statuses["debt_to_equity"] != "FAIL" {
		t.Fatalf("expected debt FAIL, got %q", statuses["debt_to_equity"])
	}
}

func TestBuildMasterRulePriorUnknownWithoutHistory(t *testing.T) {
	persona := MasterPersona{
		Key: "buffett",
		Raw: map[string]any{
			"must_have_criteria": map[string]any{
				"ROE_10yr_avg": ">= 15%",
			},
		},
	}
	block := BuildMasterRulePrior(persona, &FundamentalsBlock{
		Metrics: map[string]float64{"pe": 18},
	})
	if block == nil {
		t.Fatalf("expected non-nil prior block")
	}
	if got := block.Items[0].Status; got != "UNKNOWN" {
		t.Fatalf("expected UNKNOWN without history, got %q", got)
	}
	joined := strings.Join(block.Notes, ";")
	if !strings.Contains(joined, "history.unavailable") {
		t.Fatalf("expected unavailable note, got %q", joined)
	}
}

func TestBuildMasterRulePriorMapThreshold(t *testing.T) {
	persona := MasterPersona{
		Key: "tail_sniper",
		Raw: map[string]any{
			"must_have_criteria": map[string]any{
				"pe": map[string]any{"min": 10.0, "max": 25.0},
			},
		},
	}
	block := BuildMasterRulePrior(persona, &FundamentalsBlock{
		Metrics: map[string]float64{"pe": 18},
	})
	if block == nil {
		t.Fatalf("nil block")
	}
	if got := block.Items[0].Status; got != "PASS" {
		t.Fatalf("expected PASS for pe inside range, got %q", got)
	}
}

func TestCompareStringThreshold(t *testing.T) {
	cases := []struct {
		spec    string
		val     float64
		want    string
	}{
		{">= 15%", 0.20, "PASS"},
		{">= 15%", 0.10, "FAIL"},
		{"<= 0.5", 0.3, "PASS"},
		{"<= 0.5", 0.9, "FAIL"},
		{">10", 11, "PASS"},
		{">10", 9, "FAIL"},
		{"=5", 5, "PASS"},
		{"abc", 5, "UNKNOWN"},
	}
	for _, c := range cases {
		got := compareStringThreshold(c.spec, c.val)
		if got != c.want {
			t.Fatalf("compareStringThreshold(%q,%v)=%q want %q", c.spec, c.val, got, c.want)
		}
	}
}
