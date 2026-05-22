package agent

import (
	"fmt"
	"strings"
	"testing"
)

func TestPMAgentBuildPlanPromptAppliesPromptBudget(t *testing.T) {
	pm := NewPMAgent("pm-1", "PM", "fund-1", nil)
	input := PlanInput{
		FundID:        "fund-1",
		TradingDate:   "2026-05-17",
		AvailableCash: 100_000,
		TotalAssets:   1_000_000,
		MemoryContext: strings.Repeat("memory context ", 600),
	}
	for i := 0; i < maxPromptHoldings+3; i++ {
		input.Holdings = append(input.Holdings, HoldingPosition{Symbol: fmt.Sprintf("H%02d", i), Quantity: 100 + i, AvgCost: 10, PnLPct: 1.5})
	}
	longReasoning := strings.Repeat("reasoning ", maxPromptReasoningRunes/10+100)
	for i := 0; i < maxPromptConsensus+2; i++ {
		input.Consensus = append(input.Consensus, ConsensusItem{Symbol: fmt.Sprintf("C%02d", i), Direction: "bullish", Confidence: 80, Action: "buy", Reasoning: longReasoning})
	}
	actions := make([]PlanAction, 0, maxPromptActions+2)
	for i := 0; i < maxPromptActions+2; i++ {
		actions = append(actions, PlanAction{Symbol: fmt.Sprintf("A%02d", i), Action: "buy", Quantity: 10, Amount: 1000, Reasoning: longReasoning})
	}

	prompt := pm.buildPlanPrompt(input, actions, 0.3, 0.01)

	if strings.Contains(prompt, "H25") || strings.Contains(prompt, "C30") || strings.Contains(prompt, "A30") {
		t.Fatalf("prompt included items beyond configured budget:\n%s", prompt)
	}
	for _, want := range []string{
		"3 holdings omitted for prompt budget",
		"2 consensus items omitted for prompt budget",
		"2 proposed actions omitted for prompt budget",
		"...[truncated]",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("expected prompt to contain %q", want)
		}
	}
	if got := strings.Count(prompt, "...[truncated]"); got < 3 {
		t.Fatalf("expected reasoning and memory truncation markers, got %d", got)
	}
}

func TestTruncateRunesHandlesUnicode(t *testing.T) {
	got := truncateRunes("你好世界ABCDE", 4)
	if got != "你好世界\n...[truncated]" {
		t.Fatalf("unexpected unicode truncation: %q", got)
	}
	if notTruncated := truncateRunes("  short  ", 10); notTruncated != "short" {
		t.Fatalf("expected trimmed short text, got %q", notTruncated)
	}
}

func TestNormalizeBuyQtyEnforcesAShareLotRules(t *testing.T) {
	cases := []struct {
		name   string
		symbol string
		raw    int
		want   int
	}{
		{"sh-main-floor-100", "600519", 393, 300},
		{"chinext-floor-100", "300750", 393, 300},
		{"star-keep-1-share", "688205", 393, 393},
		{"star-below-minlot-drops", "688205", 150, 0},
		{"bse-keep-1-share", "830799", 393, 393},
		{"us-equity-passthrough", "AAPL", 7, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeBuyQty(tc.symbol, tc.raw); got != tc.want {
				t.Errorf("normalizeBuyQty(%q, %d) = %d, want %d", tc.symbol, tc.raw, got, tc.want)
			}
		})
	}
}

func TestNormalizeSellQtyHandlesOddLotResidual(t *testing.T) {
	cases := []struct {
		name    string
		symbol  string
		raw     int
		holding int
		want    int
	}{
		// SH main: 50 residual under 100 lot forces full sell.
		{"sh-residual-odd-forces-sellall", "600519", 100, 150, 150},
		// STAR: 100 residual under 200 lot forces full sell.
		{"star-residual-odd-forces-sellall", "688205", 600, 700, 700},
		// Clean splits stay partial.
		{"sh-clean-split", "600519", 300, 1000, 300},
		{"star-clean-split", "688205", 500, 1000, 500},
		// Non-A-share passes through.
		{"us-equity", "AAPL", 3, 10, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeSellQty(tc.symbol, tc.raw, tc.holding); got != tc.want {
				t.Errorf("normalizeSellQty(%q, raw=%d, hold=%d) = %d, want %d",
					tc.symbol, tc.raw, tc.holding, got, tc.want)
			}
		})
	}
}

func TestPMEvaluateHoldingsAppliesLotSizeOnReduce(t *testing.T) {
	pm := NewPMAgent("pm-1", "PM", "fund-1", nil)
	params := paramsForStyle(StyleBalanced)

	// Take-profit reduce on SH main: 1000 shares * 50% = 500, then floor
	// to 500 (already aligned). Residual 500 is also aligned → keep 500.
	holdings := []HoldingPosition{
		{Symbol: "600519", Quantity: 1000, AvgCost: 100, MarketPrice: 130, PnLPct: 30, Weight: 0.2},
	}
	actions := pm.evaluateHoldings(holdings, nil, params)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Action != "reduce" {
		t.Fatalf("expected reduce, got %s", actions[0].Action)
	}
	if actions[0].Quantity != 500 {
		t.Errorf("expected reduce qty 500, got %d", actions[0].Quantity)
	}

	// Now a 150-share holding: 50% → 75, must floor to 0 (below lot),
	// then the helper falls through to full sell of 150 to avoid
	// producing an illegal sub-lot reduce.
	holdings = []HoldingPosition{
		{Symbol: "600519", Quantity: 150, AvgCost: 100, MarketPrice: 130, PnLPct: 30, Weight: 0.05},
	}
	actions = pm.evaluateHoldings(holdings, nil, params)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Quantity != 150 {
		t.Errorf("expected full-sell 150 when reduce would create odd lot, got %d", actions[0].Quantity)
	}

	// STAR: 1000 shares, 50% = 500, but residual 500 ≥ MinLot 200 so
	// reduce stays at 500.
	holdings = []HoldingPosition{
		{Symbol: "688205", Quantity: 1000, AvgCost: 50, MarketPrice: 65, PnLPct: 30, Weight: 0.2},
	}
	actions = pm.evaluateHoldings(holdings, nil, params)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Quantity != 500 {
		t.Errorf("expected STAR reduce qty 500, got %d", actions[0].Quantity)
	}
}
