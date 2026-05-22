package risk

import (
	"context"
	"strings"
	"testing"
)

func TestSettlementCycleRule_T1SellOfLockedShares(t *testing.T) {
	// 1000 shares of 600519 held but only 600 are unlocked
	// (400 bought today are still settling). Trying to sell 800
	// should fail with a clear T+1 message.
	rule := SettlementCycleRule{}
	pc := PlanContext{
		Positions: []Position{{
			Symbol: "600519", Quantity: 1000, AvailableQty: 600, MarketPrice: 1700,
		}},
		Trades: []ProposedTrade{{
			Symbol: "600519", Side: SideSell, Quantity: 800,
			Price: 1700, Market: "a_share",
		}},
	}
	findings, err := rule.Evaluate(context.Background(), pc)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.Rule != SettlementCycleRuleName {
		t.Errorf("rule name = %q, want %q", f.Rule, SettlementCycleRuleName)
	}
	if f.Severity != SeverityFail {
		t.Errorf("severity = %q, want fail", f.Severity)
	}
	if f.Symbol != "600519" {
		t.Errorf("symbol = %q", f.Symbol)
	}
	if f.Current != 600 || f.Threshold != 800 {
		t.Errorf("current=%v threshold=%v", f.Current, f.Threshold)
	}
	if !strings.Contains(f.Message, "T+1") {
		t.Errorf("message should mention T+1: %q", f.Message)
	}
	if !strings.Contains(f.Message, "A-share market") {
		t.Errorf("message should be framed as a market rule, got %q", f.Message)
	}
	if !strings.Contains(f.Message, "next trading day") {
		t.Errorf("message should mention next trading day, got %q", f.Message)
	}
}

func TestSettlementCycleRule_T1SellWithinAvailableIsSilent(t *testing.T) {
	// Selling 500 of 1000 (600 available) is fine — no finding.
	rule := SettlementCycleRule{}
	pc := PlanContext{
		Positions: []Position{{
			Symbol: "600519", Quantity: 1000, AvailableQty: 600,
		}},
		Trades: []ProposedTrade{{
			Symbol: "600519", Side: SideSell, Quantity: 500,
			Market: "a_share",
		}},
	}
	findings, _ := rule.Evaluate(context.Background(), pc)
	if len(findings) != 0 {
		t.Errorf("expected no findings, got %+v", findings)
	}
}

func TestSettlementCycleRule_T0MarketIsSkipped(t *testing.T) {
	// Selling on T+0 markets (US/HK/crypto) never trips this rule,
	// regardless of AvailableQty.
	rule := SettlementCycleRule{}
	cases := []struct {
		name   string
		symbol string
		market string
	}{
		{"us-equity", "AAPL", "us_stock"},
		{"hk-equity", "0700", "hk_stock"},
		{"crypto", "BTCUSDT", "crypto"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc := PlanContext{
				Positions: []Position{{
					Symbol: tc.symbol, Quantity: 100, AvailableQty: 30,
				}},
				Trades: []ProposedTrade{{
					Symbol: tc.symbol, Side: SideSell, Quantity: 100,
					Market: tc.market,
				}},
			}
			findings, _ := rule.Evaluate(context.Background(), pc)
			if len(findings) != 0 {
				t.Errorf("T+0 market should not trip rule, got %+v", findings)
			}
		})
	}
}

func TestSettlementCycleRule_BuysAreExempt(t *testing.T) {
	rule := SettlementCycleRule{}
	pc := PlanContext{
		Positions: []Position{{Symbol: "600519", Quantity: 100, AvailableQty: 100}},
		Trades: []ProposedTrade{
			{Symbol: "600519", Side: SideBuy, Quantity: 200, Market: "a_share"},
			{Symbol: "600519", Side: SideAdd, Quantity: 200, Market: "a_share"},
		},
	}
	findings, _ := rule.Evaluate(context.Background(), pc)
	if len(findings) != 0 {
		t.Errorf("buys must never trip settlement rule, got %+v", findings)
	}
}

func TestSettlementCycleRule_LegacyZeroAvailableFailsOpen(t *testing.T) {
	// Positions persisted before AvailableQty existed will have
	// AvailableQty == 0. The rule must not block sells in that
	// case — the engine's downstream availableQty check is the
	// authoritative gate and will run after migrations populate the
	// column.
	rule := SettlementCycleRule{}
	pc := PlanContext{
		Positions: []Position{{
			Symbol: "600519", Quantity: 1000, AvailableQty: 0,
		}},
		Trades: []ProposedTrade{{
			Symbol: "600519", Side: SideSell, Quantity: 500, Market: "a_share",
		}},
	}
	findings, _ := rule.Evaluate(context.Background(), pc)
	if len(findings) != 0 {
		t.Errorf("legacy zero-AvailableQty must fail open, got %+v", findings)
	}
}

func TestSettlementCycleRule_ReduceCountsAsSell(t *testing.T) {
	rule := SettlementCycleRule{}
	pc := PlanContext{
		Positions: []Position{{
			Symbol: "688205", Quantity: 1000, AvailableQty: 200,
		}},
		Trades: []ProposedTrade{{
			Symbol: "688205", Side: SideReduce, Quantity: 600, Market: "a_share",
		}},
	}
	findings, _ := rule.Evaluate(context.Background(), pc)
	if len(findings) != 1 {
		t.Fatalf("reduce should be checked like sell, got %d", len(findings))
	}
	if findings[0].Severity != SeverityFail {
		t.Errorf("severity = %q", findings[0].Severity)
	}
}

func TestSettlementCycleRule_AllowsExactlyAvailable(t *testing.T) {
	rule := SettlementCycleRule{}
	pc := PlanContext{
		Positions: []Position{{
			Symbol: "600519", Quantity: 1000, AvailableQty: 600,
		}},
		Trades: []ProposedTrade{{
			Symbol: "600519", Side: SideSell, Quantity: 600, Market: "a_share",
		}},
	}
	findings, _ := rule.Evaluate(context.Background(), pc)
	if len(findings) != 0 {
		t.Errorf("selling exactly AvailableQty should not fail: %+v", findings)
	}
}

func TestSettlementCycleRule_SuggestionMentionsBothMitigations(t *testing.T) {
	rule := SettlementCycleRule{}
	pc := PlanContext{
		Positions: []Position{{
			Symbol: "300750", Quantity: 1000, AvailableQty: 400,
		}},
		Trades: []ProposedTrade{{
			Symbol: "300750", Side: SideSell, Quantity: 900, Market: "a_share",
		}},
	}
	findings, _ := rule.Evaluate(context.Background(), pc)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding")
	}
	sug := findings[0].Suggestion
	if !strings.Contains(sug, "Reduce") || !strings.Contains(sug, "next trading day") {
		t.Errorf("suggestion should mention both mitigations: %q", sug)
	}
}
