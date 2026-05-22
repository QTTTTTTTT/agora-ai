package risk

import (
	"context"
	"math"
	"testing"

	"github.com/fundai/server/internal/instrument"
)

func TestSlippageConfigToleranceFor(t *testing.T) {
	cfg := DefaultSlippageConfig()
	cases := []struct {
		name   string
		symbol string
		market string
		want   float64
	}{
		{"sh-main", "600519", "", 0.008},
		{"sz-main", "000858", "", 0.008},
		{"chinext", "300750", "", 0.012},
		{"star", "688205", "", 0.015},
		{"bse", "830799", "", 0.015},
		{"us-equity", "AAPL", "us_stock", 0.010},
		{"us-equity-equity-alias", "AAPL", "us_equity", 0.010},
		{"hk-equity", "0700", "hk_stock", 0.015},
		{"crypto", "BTCUSDT", "crypto", 0.025},
		{"unknown-falls-back-default", "ZZZZ", "fx", 0.01}, // default 1%
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cfg.ToleranceFor(tc.symbol, tc.market)
			if got != tc.want {
				t.Errorf("ToleranceFor(%q, %q) = %v, want %v", tc.symbol, tc.market, got, tc.want)
			}
		})
	}
}

func TestSlippageConfigBoardOverrideBeatsMarketOverride(t *testing.T) {
	cfg := SlippageConfig{
		DefaultTolerance: 0.05,
		ToleranceByBoard: map[instrument.Board]float64{
			instrument.BoardSTAR: 0.02,
		},
		ToleranceByMarket: map[string]float64{
			"a_share": 0.03,
		},
	}
	// STAR symbol with A-share market hint: board override (0.02) wins.
	got := cfg.ToleranceFor("688205", "a_share")
	if got != 0.02 {
		t.Errorf("expected board override 0.02, got %v", got)
	}
}

func TestSlippageGuardWithinToleranceEmitsInfo(t *testing.T) {
	rule := SlippageGuard{Config: DefaultSlippageConfig()}
	pc := PlanContext{
		Trades: []ProposedTrade{
			// SH main, tolerance 0.8%: 0.5% drift → info.
			{Symbol: "600519", Side: SideBuy, Quantity: 100, Price: 1700, ExecutionPrice: 1708.5},
		},
	}
	findings, err := rule.Evaluate(context.Background(), pc)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != SeverityInfo {
		t.Errorf("severity = %q, want info", findings[0].Severity)
	}
}

func TestSlippageGuardBeyondToleranceEmitsFail(t *testing.T) {
	rule := SlippageGuard{Config: DefaultSlippageConfig()}
	pc := PlanContext{
		Trades: []ProposedTrade{
			// SH main, tolerance 0.8%, drift 1.5% → fail.
			{Symbol: "600519", Side: SideBuy, Quantity: 100, Price: 1700, ExecutionPrice: 1725.5},
		},
	}
	findings, err := rule.Evaluate(context.Background(), pc)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != SeverityFail {
		t.Errorf("severity = %q, want fail (msg=%s)", findings[0].Severity, findings[0].Message)
	}
	if findings[0].Rule != SlippageGuardRuleName {
		t.Errorf("rule name = %q, want %q", findings[0].Rule, SlippageGuardRuleName)
	}
}

func TestSlippageGuardSellsAreExempt(t *testing.T) {
	rule := SlippageGuard{Config: DefaultSlippageConfig()}
	pc := PlanContext{
		Trades: []ProposedTrade{
			// Even with huge drift, sells are not checked.
			{Symbol: "600519", Side: SideSell, Quantity: 100, Price: 1700, ExecutionPrice: 1500},
			{Symbol: "688205", Side: SideReduce, Quantity: 200, Price: 50, ExecutionPrice: 30},
		},
	}
	findings, err := rule.Evaluate(context.Background(), pc)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected no findings for sells, got %d: %+v", len(findings), findings)
	}
}

func TestSlippageGuardSilentWhenExecutionPriceMissing(t *testing.T) {
	rule := SlippageGuard{Config: DefaultSlippageConfig()}
	pc := PlanContext{
		Trades: []ProposedTrade{
			// ExecutionPrice == 0 means "not yet priced live"; no signal.
			{Symbol: "600519", Side: SideBuy, Quantity: 100, Price: 1700, ExecutionPrice: 0},
		},
	}
	findings, err := rule.Evaluate(context.Background(), pc)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected silence when ExecutionPrice is 0, got %+v", findings)
	}
}

func TestSlippageGuardBoardSpecificTolerances(t *testing.T) {
	rule := SlippageGuard{Config: DefaultSlippageConfig()}
	cases := []struct {
		name   string
		symbol string
		plan   float64
		live   float64
		want   Severity
	}{
		// Main board 0.8% tolerance
		{"sh-main-0.5-info", "600519", 1000, 1005, SeverityInfo},
		{"sh-main-1.0-fail", "600519", 1000, 1010, SeverityFail},
		// ChiNext 1.2%
		{"chinext-1.0-info", "300750", 1000, 1010, SeverityInfo},
		{"chinext-1.5-fail", "300750", 1000, 1015, SeverityFail},
		// STAR 1.5%
		{"star-1.2-info", "688205", 1000, 1012, SeverityInfo},
		{"star-2.0-fail", "688205", 1000, 1020, SeverityFail},
		// Negative drift (price dropped)
		{"sh-main-down-0.5-info", "600519", 1000, 995, SeverityInfo},
		{"sh-main-down-1.5-fail", "600519", 1000, 985, SeverityFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc := PlanContext{Trades: []ProposedTrade{{
				Symbol: tc.symbol, Side: SideBuy, Quantity: 100,
				Price: tc.plan, ExecutionPrice: tc.live,
			}}}
			findings, _ := rule.Evaluate(context.Background(), pc)
			if len(findings) != 1 {
				t.Fatalf("expected 1 finding, got %d", len(findings))
			}
			if findings[0].Severity != tc.want {
				drift := (tc.live - tc.plan) / tc.plan * 100
				t.Errorf("drift %.3f%%: severity = %q, want %q (msg=%s)", drift, findings[0].Severity, tc.want, findings[0].Message)
			}
		})
	}
}

func TestSlippageGuardSilentWhenToleranceDisabled(t *testing.T) {
	// Empty config + non-A-share symbol → no fallback tolerance, rule
	// is silent. A-share symbols always have a baked-in fallback via
	// instrument.DefaultSlippageTolerance and are exercised separately.
	cfg := SlippageConfig{}
	rule := SlippageGuard{Config: cfg}
	pc := PlanContext{
		Trades: []ProposedTrade{
			{Symbol: "ZZZZ", Side: SideBuy, Quantity: 100, Price: 1000, ExecutionPrice: 1200},
		},
	}
	findings, err := rule.Evaluate(context.Background(), pc)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected silence with disabled config and non-A-share symbol, got %+v", findings)
	}
}

func TestSlippageGuardAShareFallsBackToInstrumentDefault(t *testing.T) {
	// Even with empty SlippageConfig, A-share boards get the platform-
	// wide default via instrument.DefaultSlippageTolerance. This is a
	// fail-safe so a partially populated fund config can't accidentally
	// disable the guard for A-shares.
	cfg := SlippageConfig{} // empty
	rule := SlippageGuard{Config: cfg}
	pc := PlanContext{
		Trades: []ProposedTrade{
			{Symbol: "600519", Side: SideBuy, Quantity: 100, Price: 1000, ExecutionPrice: 1100},
		},
	}
	findings, _ := rule.Evaluate(context.Background(), pc)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != SeverityFail {
		t.Errorf("expected fail (10%% drift > 0.8%% default), got %q", findings[0].Severity)
	}
}

// driftMatches asserts that the finding records the exact drift value
// the test set up. Floating-point comparison uses a small epsilon.
func driftMatches(got, want float64) bool {
	return math.Abs(got-want) < 1e-9
}

func TestSlippageGuardRecordsExactDrift(t *testing.T) {
	rule := SlippageGuard{Config: DefaultSlippageConfig()}
	pc := PlanContext{Trades: []ProposedTrade{{
		Symbol: "600519", Side: SideBuy, Quantity: 100,
		Price: 1000, ExecutionPrice: 1015,
	}}}
	findings, _ := rule.Evaluate(context.Background(), pc)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding")
	}
	wantDrift := 0.015 // 1.5%
	if !driftMatches(findings[0].Current, wantDrift) {
		t.Errorf("recorded drift = %v, want %v", findings[0].Current, wantDrift)
	}
}
