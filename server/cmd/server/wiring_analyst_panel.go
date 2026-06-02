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
	"github.com/fundai/server/internal/agent"
	"github.com/fundai/server/internal/llm"
)

// agentLLMForFund returns the LLM client the analyst /
// advocate agents should use for a given fund, or nil if no
// LLM is configured at this deployment.
//
// S8.3: when llmRuntime is wired with a real client, we wrap
// it in an agent.LLMAdapter that exposes CompleteWithSchema
// so analysts and Bull/Bear go through provider-native
// structured output. Otherwise we keep the legacy nil-LLM
// fallback path that S8.1 / S8.2 already validate.
func agentLLMForFund(svc *Services, fundID, stepName string) agent.LLMClient {
	if svc == nil || svc.LLMRuntime == nil || svc.LLMRuntime.client == nil {
		return nil
	}
	return agent.NewLLMAdapter(svc.LLMRuntime.client, fundID,
		agent.WithLLMAdapterStep(stepName),
		agent.WithLLMAdapterTier(llm.TierStandard),
	)
}

// newDefaultAnalystPanelProvider builds the provider closure
// installed on Services.AnalystPanelProvider at startup.
//
// The closure returns a fresh AnalystPanel per call so that
// per-fund clocks / loggers / personas can vary without sharing
// state. The 4 analyst instances inside the panel are cheap to
// build (no I/O at construction time).
//
// LLM client selection (S8.3):
//   - When svc.LLMRuntime.client is available, all 4 analysts
//     share a SchemaLLMClient-capable adapter and route through
//     CompleteWithSchema → AnalystReportJSONSchema.
//   - When no LLM is configured, the adapter is nil and the
//     analysts fall back to their deterministic rule paths.
func newDefaultAnalystPanelProvider(svc *Services) AnalystPanelProvider {
	return func(fundID string) *agent.AnalystPanel {
		llmClient := agentLLMForFund(svc, fundID, "analyst_panel")

		analysts := []agent.AnalystAgent{
			agent.NewFundamentalsAnalyst(
				"fundamentals@"+fundID,
				"Fundamentals Analyst",
				fundID, llmClient,
				agent.WithAnalystPersona(
					"a value-investing veteran who anchors on reported financials "+
						"and is sceptical of multiple expansion without earnings growth.")),
			agent.NewSentimentAnalyst(
				"sentiment@"+fundID,
				"Sentiment Analyst",
				fundID, llmClient,
				agent.WithAnalystPersona(
					"a crowd-mood reader who weighs aggregate polarity but discounts "+
						"single-source bias.")),
			agent.NewNewsAnalyst(
				"news@"+fundID,
				"News Analyst",
				fundID, llmClient,
				agent.WithAnalystPersona(
					"a catalyst hunter focused on earnings, M&A, regulator actions, "+
						"and analyst upgrades / downgrades.")),
			agent.NewTechnicalAnalyst(
				"technical@"+fundID,
				"Technical Analyst",
				fundID, llmClient,
				agent.WithAnalystPersona(
					"a regime-aware chartist who reads MA cascade, MACD and "+
						"ATR-budgeted position sizing.")),
		}
		return agent.NewAnalystPanel(fundID, analysts)
	}
}
