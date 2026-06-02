// wiring_analyst_panel.go — S8.1 default AnalystPanelProvider.
//
// Each fund gets a panel with the standard four analysts:
// fundamentals / sentiment / news / technical. The LLM client
// is intentionally nil for S8.1 — every analyst falls back to
// its deterministic rule path. S8.3 will introduce
// CompleteWithSchema on internal/llm and swap the nil LLM here
// for the real adapter, at which point analysts start producing
// LLM-authored thesis text on top of the same rule scaffolding.
//
// Why a provider closure instead of a singleton panel: per-fund
// LLM credentials, per-fund persona overrides, and per-fund
// model selection all live downstream of fund_id. The closure
// shape keeps the seam open for those (currently absent) knobs
// without changing the handler API once they land.

package main

import (
	"context"

	"github.com/fundai/server/internal/agent"
)

// nilLLMClient is the no-op LLM used by the S8.1 default panel.
// agent.LLMClient is the freeform-Complete interface; returning
// a synthetic error here keeps the analysts on their
// deterministic fallback paths until S8.3 wires a real client.
type nilLLMClient struct{}

func (nilLLMClient) Complete(_ context.Context, _ string, _ string) (string, error) {
	return "", nil
}

// newDefaultAnalystPanelProvider builds the provider closure
// installed on Services.AnalystPanelProvider at startup. It is
// safe to call with any *Services value (including one missing
// AnalystReportRepo) since the closure does not deref svc.
//
// The closure returns a fresh AnalystPanel per call so that
// per-fund clocks / loggers / personas can vary without sharing
// state. The 4 analyst instances inside the panel are cheap to
// build (no I/O at construction time).
func newDefaultAnalystPanelProvider(svc *Services) AnalystPanelProvider {
	_ = svc
	return func(fundID string) *agent.AnalystPanel {
		var llm agent.LLMClient // nil → fallback path. S8.3 swaps this.

		analysts := []agent.AnalystAgent{
			agent.NewFundamentalsAnalyst(
				"fundamentals@"+fundID,
				"Fundamentals Analyst",
				fundID, llm,
				agent.WithAnalystPersona(
					"a value-investing veteran who anchors on reported financials "+
						"and is sceptical of multiple expansion without earnings growth.")),
			agent.NewSentimentAnalyst(
				"sentiment@"+fundID,
				"Sentiment Analyst",
				fundID, llm,
				agent.WithAnalystPersona(
					"a crowd-mood reader who weighs aggregate polarity but discounts "+
						"single-source bias.")),
			agent.NewNewsAnalyst(
				"news@"+fundID,
				"News Analyst",
				fundID, llm,
				agent.WithAnalystPersona(
					"a catalyst hunter focused on earnings, M&A, regulator actions, "+
						"and analyst upgrades / downgrades.")),
			agent.NewTechnicalAnalyst(
				"technical@"+fundID,
				"Technical Analyst",
				fundID, llm,
				agent.WithAnalystPersona(
					"a regime-aware chartist who reads MA cascade, MACD and "+
						"ATR-budgeted position sizing.")),
		}
		return agent.NewAnalystPanel(fundID, analysts)
	}
}
