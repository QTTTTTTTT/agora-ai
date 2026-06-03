package main

// translate_buy_quote_unavailable_test.go locks in the
// production-grade behaviour of the LLM-path PM fallback when the
// quote service can't price an instrument.
//
// Until 2026-06-03 translateBuyAction synthesised an executable buy
// by stamping the *notional budget* into PlanAction.Price with
// Quantity=1. The broker simulator faithfully honoured that as a
// limit order — on 2026-06-02 the bug produced a 301308 fill at
// 96,226.4188 CNY/share (true mid was ~500). Production trading
// systems must NEVER invent a reference price from a budget; when
// the quote is missing, the only correct verdict is "defer"
// (watch). This test pins that invariant.

import (
	"context"
	"strings"
	"testing"

	"github.com/fundai/server/internal/decision"
	"github.com/fundai/server/internal/repository"
)

func TestTranslateBuyAction_QuoteUnavailable_DowngradesToWatch(t *testing.T) {
	// marketData intentionally nil → quoteForAction returns
	// marketdata.ErrQuoteUnavailable on the first call. That's
	// the precondition we want to exercise.
	agent := &runtimePMAgent{}

	fund := &repository.Fund{
		ID:             "fund-1",
		CurrentCapital: 1_000_000, // big enough that planBuyAmount > 0
		TotalAssets:    1_000_000,
		Config:         []byte(`{"market":"a_share","assetClass":"equity","exchange":"SZSE"}`),
	}
	da := decision.DecisionAction{
		Symbol:     "301308",
		Action:     "buy",
		QtyPct:     0.05,
		Reasoning:  "LLM thinks storage cycle bottoming",
		Confidence: 0.7,
	}

	got, ok := agent.translateBuyAction(context.Background(), fund, nil, da, 0)
	if !ok {
		t.Fatalf("translateBuyAction returned ok=false; want a watch fallback action")
	}

	if got.Action != "watch" {
		t.Fatalf("Action = %q, want %q (quote unavailable must downgrade to watch — see 96,226 CNY/share fill on 2026-06-02 for the regression we're guarding against)", got.Action, "watch")
	}
	if got.Price.Valid {
		t.Fatalf("Price.Valid = true (value=%.4f); want unset — the regression stamped notional budget into Price and the simulator filled at that price", got.Price.Float64)
	}
	if got.Amount.Valid {
		t.Fatalf("Amount.Valid = true (value=%.4f); want unset for a watch action", got.Amount.Float64)
	}
	if got.Quantity.Valid {
		t.Fatalf("Quantity.Valid = true (value=%.4f); want unset for a watch action", got.Quantity.Float64)
	}
	if !got.Reasoning.Valid || got.Reasoning.String == "" {
		t.Fatalf("Reasoning empty; want a human-readable explanation referencing the missing quote so operators can diagnose")
	}
	// The reasoning text must at minimum mention "quote unavailable"
	// and the symbol so an operator scanning Decision Center can
	// understand why this symbol skipped trading.
	for _, needle := range []string{"quote unavailable", "301308"} {
		if !strings.Contains(got.Reasoning.String, needle) {
			t.Fatalf("Reasoning = %q; want it to contain %q so the downgrade is auditable", got.Reasoning.String, needle)
		}
	}
	// The reasoning must NOT contain a synthesised price — the whole
	// point of the fix is that we don't pretend to know the price.
	// We do log the *budget* (notional / buyAmount) so the next plan
	// has context, but it must be labelled as such.
	if strings.Contains(got.Reasoning.String, "/share") || strings.Contains(got.Reasoning.String, "limit price") {
		t.Fatalf("Reasoning = %q; must not advertise a synthesised price (limit/share). Use 'budget on the table' or similar so operators don't mistake the number for a quote.", got.Reasoning.String)
	}
}
