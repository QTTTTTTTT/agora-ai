package risk

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestStaleQuoteGuardBlocksRiskIncreasingTradeOnStaleFlag(t *testing.T) {
	pc := PlanContext{
		Trades: []ProposedTrade{
			{Symbol: "600519", Side: SideBuy, Quantity: 1, Price: 100, QuoteIsStale: true},
		},
	}
	rule := StaleQuoteGuard{MaxAge: 15 * time.Minute}
	findings, err := rule.Evaluate(context.Background(), pc)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != SeverityFail {
		t.Fatalf("severity = %v, want fail", findings[0].Severity)
	}
	if findings[0].Symbol != "600519" {
		t.Fatalf("symbol = %q, want 600519", findings[0].Symbol)
	}
}

func TestStaleQuoteGuardBlocksRiskIncreasingTradeBeyondMaxAge(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	pc := PlanContext{
		Trades: []ProposedTrade{
			{Symbol: "AAPL", Side: SideBuy, Quantity: 1, Price: 200, QuoteAsOf: now.Add(-2 * time.Hour)},
		},
	}
	rule := StaleQuoteGuard{MaxAge: 30 * time.Minute, nowFn: func() time.Time { return now }}
	findings, err := rule.Evaluate(context.Background(), pc)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if !strings.Contains(findings[0].Message, "outdated") {
		t.Fatalf("message %q should mention 'outdated'", findings[0].Message)
	}
	if findings[0].Severity != SeverityFail {
		t.Fatalf("severity = %v, want fail", findings[0].Severity)
	}
}

func TestStaleQuoteGuardPassesWithFreshQuote(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	pc := PlanContext{
		Trades: []ProposedTrade{
			{Symbol: "AAPL", Side: SideBuy, Quantity: 1, Price: 200, QuoteAsOf: now.Add(-2 * time.Minute)},
		},
	}
	rule := StaleQuoteGuard{MaxAge: 15 * time.Minute, nowFn: func() time.Time { return now }}
	findings, err := rule.Evaluate(context.Background(), pc)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings for fresh quote, got %d (first: %q)", len(findings), findings[0].Message)
	}
}

func TestStaleQuoteGuardAllowsStaleSells(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	pc := PlanContext{
		Trades: []ProposedTrade{
			// Stale sell — we must still be able to liquidate when the
			// tape goes cold, otherwise the system gets stuck long.
			{Symbol: "AAPL", Side: SideSell, Quantity: 5, Price: 200, QuoteIsStale: true, QuoteAsOf: now.Add(-3 * time.Hour)},
		},
	}
	rule := StaleQuoteGuard{MaxAge: 15 * time.Minute, nowFn: func() time.Time { return now }}
	findings, err := rule.Evaluate(context.Background(), pc)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected sells to be exempt, got %d findings", len(findings))
	}
}

func TestStaleQuoteGuardIgnoresZeroAsOfWhenNotFlagged(t *testing.T) {
	// QuoteAsOf zero (no signal) AND IsStale false → policy stays silent.
	// This prevents a misconfigured caller from blocking every trade.
	pc := PlanContext{
		Trades: []ProposedTrade{
			{Symbol: "AAPL", Side: SideBuy, Quantity: 1, Price: 200},
		},
	}
	rule := StaleQuoteGuard{MaxAge: 15 * time.Minute}
	findings, err := rule.Evaluate(context.Background(), pc)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected no signal, got %d findings", len(findings))
	}
}

func TestHardRiskPolicyIncludesStaleQuoteGuard(t *testing.T) {
	policy := DefaultHardRiskPolicy()
	for _, rule := range policy.Rules {
		if _, ok := rule.(StaleQuoteGuard); ok {
			return
		}
	}
	t.Fatalf("DefaultHardRiskPolicy is missing StaleQuoteGuard")
}

func TestHardRiskConfigNormalizesMaxQuoteAge(t *testing.T) {
	cfg := HardRiskConfig{MaxQuoteAge: -1 * time.Hour}
	policy := HardRiskPolicyFromConfig(cfg)
	for _, rule := range policy.Rules {
		if guard, ok := rule.(StaleQuoteGuard); ok {
			if guard.MaxAge != DefaultHardRiskConfig().MaxQuoteAge {
				t.Fatalf("negative MaxQuoteAge should fall back to default; got %v", guard.MaxAge)
			}
			return
		}
	}
	t.Fatalf("guard not present in normalized policy")
}

func TestHardRiskConfigClampsExcessiveMaxQuoteAge(t *testing.T) {
	cfg := HardRiskConfig{MaxQuoteAge: 100 * time.Hour}
	policy := HardRiskPolicyFromConfig(cfg)
	for _, rule := range policy.Rules {
		if guard, ok := rule.(StaleQuoteGuard); ok {
			if guard.MaxAge != 24*time.Hour {
				t.Fatalf("MaxQuoteAge should clamp to 24h, got %v", guard.MaxAge)
			}
			return
		}
	}
	t.Fatalf("guard not present in normalized policy")
}
