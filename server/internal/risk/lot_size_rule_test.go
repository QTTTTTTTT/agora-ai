package risk

import (
	"context"
	"testing"
)

func TestLotSizeRule_AShareBoards(t *testing.T) {
	cases := []struct {
		name     string
		trade    ProposedTrade
		position Position
		wantSev  Severity
	}{
		{
			name:    "sh-main-aligned",
			trade:   ProposedTrade{Symbol: "600519", Side: SideBuy, Quantity: 300, Price: 1700},
			wantSev: SeverityInfo,
		},
		{
			name:    "sh-main-misaligned",
			trade:   ProposedTrade{Symbol: "600519", Side: SideBuy, Quantity: 393, Price: 1700},
			wantSev: SeverityFail,
		},
		{
			name:    "sh-main-below-minlot",
			trade:   ProposedTrade{Symbol: "600519", Side: SideBuy, Quantity: 50, Price: 1700},
			wantSev: SeverityFail,
		},
		{
			name:    "chinext-misaligned",
			trade:   ProposedTrade{Symbol: "300750", Side: SideBuy, Quantity: 393, Price: 300},
			wantSev: SeverityFail,
		},
		{
			name:    "chinext-aligned",
			trade:   ProposedTrade{Symbol: "300750", Side: SideBuy, Quantity: 200, Price: 300},
			wantSev: SeverityInfo,
		},
		{
			name:    "star-393-legal",
			trade:   ProposedTrade{Symbol: "688205", Side: SideBuy, Quantity: 393, Price: 50},
			wantSev: SeverityInfo,
		},
		{
			name:    "star-below-minlot",
			trade:   ProposedTrade{Symbol: "688205", Side: SideBuy, Quantity: 150, Price: 50},
			wantSev: SeverityFail,
		},
		{
			name:    "bse-101-legal",
			trade:   ProposedTrade{Symbol: "830799", Side: SideBuy, Quantity: 101, Price: 20},
			wantSev: SeverityInfo,
		},
		{
			name:    "us-equity-skipped",
			trade:   ProposedTrade{Symbol: "AAPL", Side: SideBuy, Quantity: 7, Price: 200},
			wantSev: "", // skipped (no finding)
		},
	}

	rule := LotSizeRule{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc := PlanContext{
				PlanID:      "test",
				TotalAssets: 1_000_000,
				Trades:      []ProposedTrade{tc.trade},
			}
			findings, err := rule.Evaluate(context.Background(), pc)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if tc.wantSev == "" {
				if len(findings) != 0 {
					t.Fatalf("expected no findings, got %+v", findings)
				}
				return
			}
			if len(findings) != 1 {
				t.Fatalf("expected exactly 1 finding, got %d: %+v", len(findings), findings)
			}
			if findings[0].Severity != tc.wantSev {
				t.Errorf("severity = %q, want %q (msg=%s)", findings[0].Severity, tc.wantSev, findings[0].Message)
			}
		})
	}
}

func TestLotSizeRule_SellAllAllowed(t *testing.T) {
	// Selling the full position is always allowed, even when the holding
	// is itself an odd lot (e.g. residual from a corporate action).
	rule := LotSizeRule{}

	pc := PlanContext{
		PlanID: "test",
		Positions: []Position{
			{Symbol: "600519", Quantity: 50, MarketPrice: 1700},
		},
		Trades: []ProposedTrade{
			{Symbol: "600519", Side: SideSell, Quantity: 50, Price: 1700},
		},
		TotalAssets: 1_000_000,
	}
	findings, err := rule.Evaluate(context.Background(), pc)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for _, f := range findings {
		if f.Severity == SeverityFail {
			t.Errorf("sell-all of odd-lot should not fail: %+v", f)
		}
	}
}

func TestLotSizeRule_PartialSellMustAlign(t *testing.T) {
	// Partial sells must still align — selling 393 out of 1000 on SH main
	// is a misaligned partial sell.
	rule := LotSizeRule{}

	pc := PlanContext{
		PlanID: "test",
		Positions: []Position{
			{Symbol: "600519", Quantity: 1000, MarketPrice: 1700},
		},
		Trades: []ProposedTrade{
			{Symbol: "600519", Side: SideReduce, Quantity: 393, Price: 1700},
		},
		TotalAssets: 1_000_000,
	}
	findings, err := rule.Evaluate(context.Background(), pc)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	hasFail := false
	for _, f := range findings {
		if f.Severity == SeverityFail {
			hasFail = true
		}
	}
	if !hasFail {
		t.Errorf("expected fail finding for misaligned partial sell, got %+v", findings)
	}
}

func TestDefaultEquityPolicyIncludesLotSize(t *testing.T) {
	p := DefaultEquityPolicy()
	found := false
	for _, r := range p.Rules {
		if r.Name() == "lot_size" {
			found = true
		}
	}
	if !found {
		t.Error("DefaultEquityPolicy should include lot_size rule")
	}
}
