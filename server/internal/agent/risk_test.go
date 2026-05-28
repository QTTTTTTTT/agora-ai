// Package agent — unit tests for RiskAgent.ReviewPlan and the
// underlying check functions. These tests were added during the
// 2026-05-28 audit that found risk.go had zero coverage despite
// being a hard gate on every plan. Each test isolates one risk rule
// so a future bug stays scoped to the rule that broke.
//
// Test approach:
//   - Construct a single-action InvestmentPlan that targets the rule
//     under test, with everything else "clean" so unrelated checks
//     report pass.
//   - Assert on (a) the per-rule RiskCheckResult status (pass / warn
//     / fail) and (b) the derived overall Verdict.
//   - Use DefaultRiskConfig as the baseline to keep the assertions
//     anchored to production thresholds; tests that need to push the
//     boundary build their own RiskConfig.
package agent

import (
	"context"
	"strings"
	"testing"
)

func basePlan(actions []PlanAction) *InvestmentPlan {
	return &InvestmentPlan{ID: "p-1", FundID: "f-1", Date: "2026-05-28", Status: "draft", Actions: actions}
}

// findCheck returns the first RiskCheckResult whose Rule matches name.
// Tests use it to assert on a specific rule without depending on the
// (unstable) slice index.
func findCheck(t *testing.T, review *RiskReview, rule string) RiskCheckResult {
	t.Helper()
	for _, c := range review.Checks {
		if c.Rule == rule {
			return c
		}
	}
	t.Fatalf("no check with rule=%q; checks=%+v", rule, review.Checks)
	return RiskCheckResult{}
}

func TestRiskAgentApprovesCleanPlan(t *testing.T) {
	ra := NewRiskAgent(DefaultRiskConfig(), nil, nil)
	plan := basePlan([]PlanAction{{Symbol: "AAPL", Action: "buy", Quantity: 50, Price: 180}})
	// Holdings cover most of the NAV so drawdown peak vs current is
	// balanced (computeDrawdown peaks at max(cost, totalAssets); if
	// holdings are tiny vs totalAssets the drawdown denominator becomes
	// totalAssets and current looks like a -99% loss against it).
	// Holdings cover the full NAV so computeDrawdown's peak=max(cost,
	// totalAssets) equals current market value → drawdown ≈ 0. Spread
	// across 4 sectors at 25% each, well below the 40% sector cap.
	// Total exposure 100% looks tight against the 95% cap but the buy
	// is a tiny add and partial cash flow doesn't tip total >95% before
	// the dry-run completes. We pick numbers that stay quietly inside
	// every threshold for the "approved" baseline.
	holdings := []HoldingPosition{
		{Symbol: "AAPL", Quantity: 1000, AvgCost: 230, MarketPrice: 230, MarketValue: 230_000, Sector: "tech", AvgVolume: 5_000_000},
		{Symbol: "JNJ", Quantity: 1000, AvgCost: 230, MarketPrice: 230, MarketValue: 230_000, Sector: "health", AvgVolume: 4_000_000},
		{Symbol: "XOM", Quantity: 1000, AvgCost: 230, MarketPrice: 230, MarketValue: 230_000, Sector: "energy", AvgVolume: 3_000_000},
		{Symbol: "BRK", Quantity: 100, AvgCost: 2200, MarketPrice: 2200, MarketValue: 220_000, Sector: "fin", AvgVolume: 200_000},
	}
	review, err := ra.ReviewPlan(context.Background(), plan, holdings, 1_000_000)
	if err != nil {
		t.Fatalf("ReviewPlan: %v", err)
	}
	if review.Verdict != "approved" {
		t.Errorf("verdict = %q, want approved; rejections=%v warnings=%v", review.Verdict, review.Rejections, review.Warnings)
	}
}

func TestRiskAgentRejectsSinglePositionOverLimit(t *testing.T) {
	ra := NewRiskAgent(DefaultRiskConfig(), nil, nil)
	// 35% projected exposure on AAPL (default limit 30%).
	plan := basePlan([]PlanAction{{Symbol: "AAPL", Action: "buy", Amount: 350_000}})
	holdings := []HoldingPosition{{Symbol: "AAPL", Quantity: 0, MarketPrice: 180, Sector: "tech", AvgVolume: 5_000_000}}
	review, err := ra.ReviewPlan(context.Background(), plan, holdings, 1_000_000)
	if err != nil {
		t.Fatalf("ReviewPlan: %v", err)
	}
	check := findCheck(t, review, "single_position_limit")
	if check.Status != "fail" {
		t.Errorf("single_position status = %q, want fail; check=%+v", check.Status, check)
	}
	if review.Verdict != "rejected" {
		t.Errorf("verdict = %q, want rejected", review.Verdict)
	}
	if len(review.Suggestions) == 0 {
		t.Errorf("expected a suggestion when a position rejected, got none")
	}
}

func TestRiskAgentRejectsTotalPositionOverLimit(t *testing.T) {
	ra := NewRiskAgent(DefaultRiskConfig(), nil, nil)
	plan := basePlan([]PlanAction{{Symbol: "NEW", Action: "buy", Amount: 100_000}})
	// Existing holdings already consume 95% — buying NEW pushes us over.
	holdings := []HoldingPosition{{Symbol: "EXIST", Quantity: 1, MarketPrice: 950_000, MarketValue: 950_000, Sector: "tech", AvgVolume: 5_000_000}}
	review, err := ra.ReviewPlan(context.Background(), plan, holdings, 1_000_000)
	if err != nil {
		t.Fatalf("ReviewPlan: %v", err)
	}
	check := findCheck(t, review, "total_position_limit")
	if check.Status != "fail" {
		t.Errorf("total_position status = %q, want fail", check.Status)
	}
}

func TestRiskAgentWarnsSectorConcentration(t *testing.T) {
	cfg := DefaultRiskConfig()
	ra := NewRiskAgent(cfg, nil, nil)
	plan := basePlan([]PlanAction{{Symbol: "MSFT", Action: "buy", Amount: 100_000}})
	// 350k existing tech + 100k buy → 45% tech (default limit 40%).
	holdings := []HoldingPosition{
		{Symbol: "AAPL", Quantity: 1, MarketPrice: 350_000, MarketValue: 350_000, Sector: "tech", AvgVolume: 5_000_000},
		{Symbol: "MSFT", Quantity: 1, MarketPrice: 100_000, MarketValue: 100_000, Sector: "tech", AvgVolume: 5_000_000},
	}
	review, err := ra.ReviewPlan(context.Background(), plan, holdings, 1_000_000)
	if err != nil {
		t.Fatalf("ReviewPlan: %v", err)
	}
	check := findCheck(t, review, "sector_concentration")
	if check.Status != "warn" {
		t.Errorf("sector_concentration status = %q, want warn; check=%+v", check.Status, check)
	}
	if review.Verdict != "approved_with_warnings" && review.Verdict != "rejected" {
		t.Errorf("verdict = %q, want approved_with_warnings", review.Verdict)
	}
}

func TestRiskAgentWarnsDailyLoss(t *testing.T) {
	ra := NewRiskAgent(DefaultRiskConfig(), nil, nil)
	plan := basePlan([]PlanAction{{Symbol: "SPY", Action: "hold"}})
	// $50K cost, $14K loss = -28% / 1M assets → -1.4% — below the -3% threshold.
	// Push it harder: cost 1M, loss 50K → -5% of NAV.
	holdings := []HoldingPosition{{Symbol: "SPY", Quantity: 1000, AvgCost: 1000, MarketPrice: 950, MarketValue: 950_000, Sector: "etf", AvgVolume: 50_000_000}}
	review, err := ra.ReviewPlan(context.Background(), plan, holdings, 1_000_000)
	if err != nil {
		t.Fatalf("ReviewPlan: %v", err)
	}
	check := findCheck(t, review, "daily_loss_warning")
	if check.Status != "warn" {
		t.Errorf("daily_loss status = %q, want warn; check=%+v", check.Status, check)
	}
}

func TestRiskAgentRejectsCircuitBreaker(t *testing.T) {
	ra := NewRiskAgent(DefaultRiskConfig(), nil, nil)
	plan := basePlan([]PlanAction{{Symbol: "SPY", Action: "hold"}})
	// AvgCost 1000, MarketPrice 750 → -25% on a 1M cost holding; circuit at -20%.
	holdings := []HoldingPosition{{Symbol: "SPY", Quantity: 1000, AvgCost: 1000, MarketPrice: 750, MarketValue: 750_000, Sector: "etf", AvgVolume: 50_000_000}}
	review, err := ra.ReviewPlan(context.Background(), plan, holdings, 1_000_000)
	if err != nil {
		t.Fatalf("ReviewPlan: %v", err)
	}
	check := findCheck(t, review, "circuit_breaker")
	if check.Status != "fail" {
		t.Errorf("circuit_breaker status = %q, want fail; check=%+v", check.Status, check)
	}
	if review.Verdict != "rejected" {
		t.Errorf("verdict = %q, want rejected", review.Verdict)
	}
}

func TestRiskAgentLiquidityWarnsOnLargeOrder(t *testing.T) {
	ra := NewRiskAgent(DefaultRiskConfig(), nil, nil)
	// Order of 200K shares vs 1M avg volume = 20% — above the 10% threshold.
	plan := basePlan([]PlanAction{{Symbol: "ILLIQ", Action: "buy", Quantity: 200_000, Price: 5}})
	holdings := []HoldingPosition{{Symbol: "ILLIQ", Quantity: 0, MarketPrice: 5, AvgVolume: 1_000_000, Sector: "smallcap"}}
	review, err := ra.ReviewPlan(context.Background(), plan, holdings, 10_000_000)
	if err != nil {
		t.Fatalf("ReviewPlan: %v", err)
	}
	check := findCheck(t, review, "liquidity_check")
	if check.Status != "warn" {
		t.Errorf("liquidity status = %q, want warn; check=%+v", check.Status, check)
	}
	suggestionFound := false
	for _, s := range review.Suggestions {
		if strings.Contains(s, "TWAP") {
			suggestionFound = true
			break
		}
	}
	if !suggestionFound {
		t.Errorf("expected TWAP suggestion when liquidity warns, got %v", review.Suggestions)
	}
}

// Regression: prior to the fix in risk.go:276 the AvgVolume=0 path
// silently flagged every symbol-without-metadata as illiquid. The
// check should now pass cleanly and surface a "liquidity unknown"
// message instead.
func TestRiskAgentLiquidityUnknownVolumePasses(t *testing.T) {
	ra := NewRiskAgent(DefaultRiskConfig(), nil, nil)
	plan := basePlan([]PlanAction{{Symbol: "NEWIPO", Action: "buy", Quantity: 100, Price: 50}})
	holdings := []HoldingPosition{{Symbol: "NEWIPO", Quantity: 0, MarketPrice: 50, AvgVolume: 0, Sector: "tech"}}
	review, err := ra.ReviewPlan(context.Background(), plan, holdings, 1_000_000)
	if err != nil {
		t.Fatalf("ReviewPlan: %v", err)
	}
	check := findCheck(t, review, "liquidity_check")
	if check.Status != "pass" {
		t.Errorf("liquidity status with unknown volume = %q, want pass; check=%+v", check.Status, check)
	}
	if !strings.Contains(check.Message, "unknown") {
		t.Errorf("liquidity message should mention unknown volume, got %q", check.Message)
	}
}

func TestRiskAgentVerdictWarningsTrumpedByRejection(t *testing.T) {
	ra := NewRiskAgent(DefaultRiskConfig(), nil, nil)
	// Combo: sector warn + circuit breaker fail. Verdict must collapse to "rejected".
	plan := basePlan([]PlanAction{{Symbol: "AAPL", Action: "hold"}})
	holdings := []HoldingPosition{
		{Symbol: "AAPL", Quantity: 1000, AvgCost: 1000, MarketPrice: 750, MarketValue: 750_000, Sector: "tech", AvgVolume: 5_000_000},
		{Symbol: "MSFT", Quantity: 1, MarketPrice: 200_000, MarketValue: 200_000, Sector: "tech", AvgVolume: 5_000_000},
	}
	review, err := ra.ReviewPlan(context.Background(), plan, holdings, 1_000_000)
	if err != nil {
		t.Fatalf("ReviewPlan: %v", err)
	}
	if review.Verdict != "rejected" {
		t.Errorf("verdict = %q, want rejected (fail beats warn)", review.Verdict)
	}
}

func TestRiskAgentRejectsNilPlan(t *testing.T) {
	ra := NewRiskAgent(DefaultRiskConfig(), nil, nil)
	if _, err := ra.ReviewPlan(context.Background(), nil, nil, 1_000_000); err == nil {
		t.Fatal("nil plan: expected error, got nil")
	}
}

func TestRiskAgentRejectsZeroTotalAssets(t *testing.T) {
	ra := NewRiskAgent(DefaultRiskConfig(), nil, nil)
	plan := basePlan([]PlanAction{{Symbol: "AAPL", Action: "hold"}})
	if _, err := ra.ReviewPlan(context.Background(), plan, nil, 0); err == nil {
		t.Fatal("zero totalAssets: expected error, got nil")
	}
}

// LLM commentary failures must NOT abort the review — they're a
// strictly additive enhancement. Asserts the gate degrades gracefully
// when the commentary client errors.
type erroringRiskLLM struct{}

func (erroringRiskLLM) GenerateRiskCommentary(context.Context, *RiskReview) (string, error) {
	return "", errLLMFailed
}

var errLLMFailed = &llmFailure{}

type llmFailure struct{}

func (*llmFailure) Error() string { return "llm down" }

func TestRiskAgentLLMCommentaryFailureDoesNotAbortReview(t *testing.T) {
	ra := NewRiskAgent(DefaultRiskConfig(), erroringRiskLLM{}, nil)
	plan := basePlan([]PlanAction{{Symbol: "AAPL", Action: "buy", Quantity: 10, Price: 180}})
	holdings := []HoldingPosition{{Symbol: "AAPL", Quantity: 0, MarketPrice: 180, Sector: "tech", AvgVolume: 5_000_000}}
	review, err := ra.ReviewPlan(context.Background(), plan, holdings, 1_000_000)
	if err != nil {
		t.Fatalf("ReviewPlan: %v", err)
	}
	if review.Commentary != "" {
		t.Errorf("commentary should stay empty when LLM errors, got %q", review.Commentary)
	}
	if review.Verdict == "" {
		t.Error("verdict should still be derived even when commentary fails")
	}
}
