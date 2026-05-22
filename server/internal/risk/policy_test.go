package risk

import (
	"context"
	"math"
	"testing"
)

func TestSinglePositionLimit_FailsAndPasses(t *testing.T) {
	pc := PlanContext{
		PlanID:      "p1",
		TotalAssets: 1000,
		Positions: []Position{
			{Symbol: "AAA", Sector: "tech", Quantity: 1, MarketPrice: 200},
		},
		Trades: []ProposedTrade{
			{Symbol: "AAA", Side: SideBuy, Quantity: 5, Price: 100, Amount: 500}, // post = 700/1000 = 70%
			{Symbol: "BBB", Side: SideBuy, Quantity: 1, Price: 100, Amount: 100}, // post = 100/1000 = 10%
		},
	}
	rule := SinglePositionLimit{Max: 0.30}
	fs, err := rule.Evaluate(context.Background(), pc)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(fs) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(fs))
	}
	var fail, info int
	for _, f := range fs {
		if f.Symbol == "AAA" && f.Severity != SeverityFail {
			t.Errorf("AAA expected fail, got %s: %s", f.Severity, f.Message)
		}
		if f.Symbol == "BBB" && f.Severity != SeverityInfo {
			t.Errorf("BBB expected info, got %s", f.Severity)
		}
		if f.Severity == SeverityFail {
			fail++
		}
		if f.Severity == SeverityInfo {
			info++
		}
	}
	if fail != 1 || info != 1 {
		t.Fatalf("severity mix wrong fail=%d info=%d", fail, info)
	}
}

func TestTotalExposureLimit(t *testing.T) {
	pc := PlanContext{
		TotalAssets: 1000,
		Positions: []Position{
			{Symbol: "AAA", Quantity: 5, MarketPrice: 100, MarketValue: 500},
		},
		Trades: []ProposedTrade{
			{Symbol: "BBB", Side: SideBuy, Quantity: 5, Price: 100, Amount: 500}, // total = 1000/1000 = 100% > 95%
		},
	}
	rule := TotalExposureLimit{Max: 0.95}
	fs, _ := rule.Evaluate(context.Background(), pc)
	if len(fs) != 1 || fs[0].Severity != SeverityFail {
		t.Fatalf("expected single fail finding, got %#v", fs)
	}
}

func TestSectorExposureLimit_PostTrade(t *testing.T) {
	pc := PlanContext{
		TotalAssets: 1000,
		Positions: []Position{
			{Symbol: "AAA", Sector: "tech", Quantity: 4, MarketPrice: 100, MarketValue: 400},
			{Symbol: "BBB", Sector: "energy", Quantity: 1, MarketPrice: 100, MarketValue: 100},
		},
		Trades: []ProposedTrade{
			{Symbol: "CCC", Side: SideBuy, Sector: "tech", Quantity: 1, Price: 100, Amount: 100}, // tech becomes 500/1000 = 50%
		},
	}
	rule := SectorExposureLimit{Max: 0.40}
	fs, _ := rule.Evaluate(context.Background(), pc)
	var techFinding *Finding
	for i := range fs {
		if !contains(fs[i].Message, "tech") {
			continue
		}
		techFinding = &fs[i]
	}
	if techFinding == nil || techFinding.Severity != SeverityWarn {
		t.Fatalf("expected tech warn finding, got %#v", fs)
	}
}

func TestLiquidityLimit_UnknownIsInfoNotWarn(t *testing.T) {
	pc := PlanContext{
		TotalAssets: 1000,
		Trades: []ProposedTrade{
			{Symbol: "NEW", Side: SideBuy, Quantity: 1000, Price: 1},
		},
		Market: MarketSnapshot{AvgVolume: map[string]float64{}}, // no data
	}
	rule := LiquidityLimit{Max: 0.10}
	fs, _ := rule.Evaluate(context.Background(), pc)
	if len(fs) != 1 || fs[0].Severity != SeverityInfo {
		t.Fatalf("expected info, got %#v", fs)
	}
	if !contains(fs[0].Message, "unknown") {
		t.Fatalf("expected unknown message, got %q", fs[0].Message)
	}
}

func TestLiquidityLimit_LargeOrderWarns(t *testing.T) {
	pc := PlanContext{
		TotalAssets: 1000,
		Trades: []ProposedTrade{
			{Symbol: "AAA", Side: SideBuy, Quantity: 1000, Price: 1},
		},
		Market: MarketSnapshot{AvgVolume: map[string]float64{"AAA": 5000}},
	}
	rule := LiquidityLimit{Max: 0.10}
	fs, _ := rule.Evaluate(context.Background(), pc)
	if len(fs) != 1 || fs[0].Severity != SeverityWarn {
		t.Fatalf("expected warn, got %#v", fs)
	}
}

func TestHistoricalVaR_FailsWhenLossesExceedLimit(t *testing.T) {
	// Construct a simple all-in-AAA scenario where every historical sample
	// is -10%. The 95% VaR equals 10% which exceeds Max=5%.
	const n = 100
	losses := make([]float64, n)
	for i := range losses {
		losses[i] = -0.10
	}
	pc := PlanContext{
		TotalAssets: 1000,
		Positions: []Position{
			{Symbol: "AAA", Quantity: 5, MarketPrice: 100, MarketValue: 500},
		},
		Trades: []ProposedTrade{
			{Symbol: "AAA", Side: SideBuy, Quantity: 5, Price: 100, Amount: 500}, // post = 1000
		},
		Market: MarketSnapshot{HistoricalReturns: map[string][]float64{"AAA": losses}},
	}
	rule := HistoricalVaRLimit{Confidence: 0.95, Max: 0.05, MinSamples: 50}
	fs, err := rule.Evaluate(context.Background(), pc)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(fs) != 1 || fs[0].Severity != SeverityFail {
		t.Fatalf("expected fail finding, got %#v", fs)
	}
	if math.Abs(fs[0].Current-0.10) > 1e-9 {
		t.Fatalf("VaR mismatch: got %v want 0.10", fs[0].Current)
	}
}

func TestHistoricalVaR_InfoWhenSeriesMissing(t *testing.T) {
	pc := PlanContext{
		TotalAssets: 1000,
		Trades: []ProposedTrade{
			{Symbol: "NEW", Side: SideBuy, Quantity: 1, Price: 100, Amount: 100},
		},
		Market: MarketSnapshot{HistoricalReturns: map[string][]float64{}},
	}
	rule := HistoricalVaRLimit{Confidence: 0.95, Max: 0.05, MinSamples: 50}
	fs, _ := rule.Evaluate(context.Background(), pc)
	if len(fs) != 1 || fs[0].Severity != SeverityInfo {
		t.Fatalf("expected info finding, got %#v", fs)
	}
}

func TestCorrelationLimit_FlagsHighlyCorrelatedPair(t *testing.T) {
	const n = 100
	rets := make([]float64, n)
	for i := range rets {
		// alternating +1%/-1% — perfectly correlated when copied
		if i%2 == 0 {
			rets[i] = 0.01
		} else {
			rets[i] = -0.01
		}
	}
	pc := PlanContext{
		TotalAssets: 1000,
		Trades: []ProposedTrade{
			{Symbol: "AAA", Side: SideBuy, Quantity: 1, Price: 200, Amount: 200},
			{Symbol: "BBB", Side: SideBuy, Quantity: 1, Price: 200, Amount: 200},
		},
		Market: MarketSnapshot{
			HistoricalReturns: map[string][]float64{
				"AAA": rets,
				"BBB": rets,
			},
		},
	}
	rule := CorrelationLimit{Max: 0.85, MinWeight: 0.05, MinSamples: 50}
	fs, err := rule.Evaluate(context.Background(), pc)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(fs) != 1 || fs[0].Severity != SeverityWarn {
		t.Fatalf("expected one warn finding, got %#v", fs)
	}
}

func TestStressTestLimit(t *testing.T) {
	pc := PlanContext{
		TotalAssets: 1000,
		Positions: []Position{
			{Symbol: "AAA", Sector: "tech", Quantity: 5, MarketPrice: 100, MarketValue: 500},
		},
		Trades: []ProposedTrade{
			{Symbol: "BBB", Side: SideBuy, Sector: "tech", Quantity: 1, Price: 100, Amount: 100},
		},
		Market: MarketSnapshot{
			StressShocks: map[string]map[string]float64{
				"tech_crash": {"tech": -0.50}, // 600 * -0.5 = -300; loss = 30% of NAV
			},
		},
	}
	rule := StressTestLimit{Max: 0.10, FailAt: 0.25}
	fs, _ := rule.Evaluate(context.Background(), pc)
	if len(fs) != 1 || fs[0].Severity != SeverityFail {
		t.Fatalf("expected fail finding, got %#v", fs)
	}
	if math.Abs(fs[0].Current-0.30) > 1e-9 {
		t.Fatalf("loss pct mismatch: %v", fs[0].Current)
	}
}

func TestEvaluator_DerivesVerdict(t *testing.T) {
	pol := Policy{Name: "t", Rules: []Rule{
		SinglePositionLimit{Max: 0.30},
		LiquidityLimit{Max: 0.10},
	}}
	pc := PlanContext{
		TotalAssets: 1000,
		Trades: []ProposedTrade{
			{Symbol: "AAA", Side: SideBuy, Quantity: 1, Price: 100, Amount: 100}, // 10%, ok
		},
		Market: MarketSnapshot{AvgVolume: map[string]float64{"AAA": 10000}},
	}
	rep := NewEvaluator(pol).Evaluate(context.Background(), pc)
	if rep.Verdict != VerdictApproved {
		t.Fatalf("expected approved, got %s findings=%#v", rep.Verdict, rep.Findings)
	}

	// Now blow through single-position cap.
	pc.Trades[0].Amount = 500
	rep = NewEvaluator(pol).Evaluate(context.Background(), pc)
	if rep.Verdict != VerdictRejected {
		t.Fatalf("expected rejected, got %s", rep.Verdict)
	}
	if !rep.HasFail() {
		t.Fatal("HasFail should be true")
	}
}

func TestDefaultHardRiskPolicyRejectsDailyLossNewBuys(t *testing.T) {
	pc := PlanContext{
		PlanID:      "p-hard-loss",
		TotalAssets: 100000,
		DailyReturn: -0.061,
		Trades: []ProposedTrade{{
			Symbol:   "AAA",
			Side:     SideBuy,
			Quantity: 10,
			Price:    100,
			Amount:   1000,
		}},
	}
	report := NewEvaluator(DefaultHardRiskPolicy()).Evaluate(context.Background(), pc)
	if report.Verdict != VerdictRejected {
		t.Fatalf("expected rejected daily loss gate, got %s findings=%#v", report.Verdict, report.Findings)
	}
	if !hasRule(report.Findings, "hard_daily_loss_limit", SeverityFail) {
		t.Fatalf("expected hard_daily_loss_limit fail, got %#v", report.Findings)
	}
}

func TestDefaultHardRiskPolicyAllowsDailyLossRiskReduction(t *testing.T) {
	pc := PlanContext{
		PlanID:      "p-hard-loss-sell",
		TotalAssets: 100000,
		DailyReturn: -0.061,
		Positions: []Position{{
			Symbol:      "AAA",
			Sector:      "equity",
			Quantity:    100,
			MarketPrice: 100,
			MarketValue: 10000,
		}},
		Trades: []ProposedTrade{{
			Symbol:   "AAA",
			Side:     SideSell,
			Quantity: 10,
			Price:    100,
			Amount:   1000,
		}},
	}
	report := NewEvaluator(DefaultHardRiskPolicy()).Evaluate(context.Background(), pc)
	if hasRule(report.Findings, "hard_daily_loss_limit", SeverityFail) {
		t.Fatalf("sell/reduce should not trip daily loss gate, got %#v", report.Findings)
	}
}

func TestDefaultHardRiskPolicyRejectsMaxOrderNotional(t *testing.T) {
	pc := PlanContext{
		PlanID:      "p-big-order",
		TotalAssets: 100000,
		Trades: []ProposedTrade{{
			Symbol:   "AAA",
			Side:     SideBuy,
			Quantity: 200,
			Price:    100,
			Amount:   20000, // 20% > hard 10%
		}},
	}
	report := NewEvaluator(DefaultHardRiskPolicy()).Evaluate(context.Background(), pc)
	if !hasRule(report.Findings, "hard_max_order_notional", SeverityFail) {
		t.Fatalf("expected max order fail, got %#v", report.Findings)
	}
}

// Real-world bug from prod: OCS fund held 393 shares of 688205 at
// market value ~10% NAV. LLM PM emitted "reduce 393" to clear the
// position, but trader-side quote drift pushed the order notional
// 0.5% above the held market value. MaxOrderNotionalLimit (10% NAV
// cap) then rejected the *clear-out*, leaving the position
// permanently un-trimmable. The fix waives the cap for sell/reduce
// orders whose quantity stays within the held position (real
// de-risking, not new concentration). Short-sell prevention is
// handled by AvailableQty < quantity in the trading engine.
func TestMaxOrderNotionalLimitWaivesSellWithinHolding(t *testing.T) {
	pc := PlanContext{
		PlanID:      "p-clear-out",
		TotalAssets: 100000,
		Positions: []Position{{
			Symbol:      "688205",
			Quantity:    393,
			MarketPrice: 256,
		}},
		Trades: []ProposedTrade{{
			Symbol:   "688205",
			Side:     SideReduce,
			Quantity: 393,
			Price:    256,
			Amount:   100608, // > 10% × 100000 = 10000 cap, but == held position
		}},
	}
	report := NewEvaluator(DefaultHardRiskPolicy()).Evaluate(context.Background(), pc)
	if hasRule(report.Findings, "hard_max_order_notional", SeverityFail) {
		t.Fatalf("position-reducing sell within held quantity must not trip notional cap, got %#v", report.Findings)
	}
}

// Reciprocal guard: if the sell quantity exceeds the held position
// (the LLM tried to short-sell, or hallucinated a larger holding),
// the notional cap must still fail the order. The trading engine
// also catches this via AvailableQty < quantity, but this is the
// earlier and more readable layer.
func TestMaxOrderNotionalLimitStillFailsSellBeyondHolding(t *testing.T) {
	pc := PlanContext{
		PlanID:      "p-short-sell",
		TotalAssets: 100000,
		Positions: []Position{{
			Symbol:      "AAA",
			Quantity:    100,
			MarketPrice: 100,
		}},
		Trades: []ProposedTrade{{
			Symbol:   "AAA",
			Side:     SideSell,
			Quantity: 200, // 2× held — short-sell attempt
			Price:    100,
			Amount:   20000, // 20% > 10% cap
		}},
	}
	report := NewEvaluator(DefaultHardRiskPolicy()).Evaluate(context.Background(), pc)
	if !hasRule(report.Findings, "hard_max_order_notional", SeverityFail) {
		t.Fatalf("sell quantity beyond held position must trip notional cap, got %#v", report.Findings)
	}
}

func TestDefaultHardRiskPolicyRejectsTradeFrequency(t *testing.T) {
	tradesToday := make([]ExecutedTrade, 50)
	for i := range tradesToday {
		tradesToday[i] = ExecutedTrade{Symbol: "AAA", Side: SideBuy, Status: "filled", Amount: 100}
	}
	pc := PlanContext{
		PlanID:      "p-frequency",
		TotalAssets: 100000,
		TradesToday: tradesToday,
		Trades: []ProposedTrade{{
			Symbol:   "BBB",
			Side:     SideBuy,
			Quantity: 1,
			Price:    100,
			Amount:   100,
		}},
	}
	report := NewEvaluator(DefaultHardRiskPolicy()).Evaluate(context.Background(), pc)
	if !hasRule(report.Findings, "hard_trade_frequency", SeverityFail) {
		t.Fatalf("expected trade frequency fail, got %#v", report.Findings)
	}
}

func TestDefaultHardRiskPolicyRejectsHardSectorConcentration(t *testing.T) {
	pc := PlanContext{
		PlanID:      "p-sector",
		TotalAssets: 100000,
		Positions: []Position{{
			Symbol:      "AAA",
			Sector:      "tech",
			Quantity:    300,
			MarketPrice: 100,
			MarketValue: 30000,
		}},
		Trades: []ProposedTrade{{
			Symbol:   "BBB",
			Side:     SideBuy,
			Sector:   "tech",
			Quantity: 150,
			Price:    100,
			Amount:   15000, // tech post = 45% > hard 40%
		}},
	}
	report := NewEvaluator(DefaultHardRiskPolicy()).Evaluate(context.Background(), pc)
	if !hasRule(report.Findings, "sector_concentration", SeverityFail) {
		t.Fatalf("expected hard sector fail, got %#v", report.Findings)
	}
}

// helpers ---------------------------------------------------------------

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func hasRule(findings []Finding, rule string, severity Severity) bool {
	for _, finding := range findings {
		if finding.Rule == rule && finding.Severity == severity {
			return true
		}
	}
	return false
}
