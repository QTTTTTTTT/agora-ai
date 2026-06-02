// wiring_debate.go — S8.2 default DebateProvider.
//
// Each fund gets a Debate orchestrator with one Bull and one Bear
// researcher. Like the S8.1 panel, the LLM client is nil for now
// so both advocates run on their deterministic skeletons; S8.3
// will swap in the real LLM once CompleteWithSchema lands.

package main

import (
	"time"

	"github.com/fundai/server/internal/agent"
)

// newDefaultDebateProvider returns the provider closure installed
// on Services.DebateProvider at startup.
//
// We default to 2 rounds (4 arguments total: r1-bull, r1-bear,
// r2-bull, r2-bear) and a 20s per-argument timeout so a hung LLM
// can't stall the whole debate. These knobs become per-fund
// configurable in S8.4 alongside the reputation surface.
//
// LLM client selection (S8.3):
//   - When svc.LLMRuntime.client is available, both Bull and
//     Bear share a SchemaLLMClient-capable adapter and route
//     through CompleteWithSchema → AdvocateArgumentJSONSchema
//     so the providers (OpenAI / Gemini in particular) produce
//     strict bull-or-bear, support_points + rebuttals JSON.
//   - When no LLM is configured, the adapter is nil and both
//     advocates fall back to their deterministic skeletons.
func newDefaultDebateProvider(svc *Services) DebateProvider {
	return func(fundID string) *agent.Debate {
		llmClient := agentLLMForFund(svc, fundID, "bullbear_debate")
		bull := agent.NewBullResearcher(
			"bull@"+fundID,
			"Bull Researcher",
			fundID, llmClient,
			agent.WithAdvocatePersona(
				"a contrarian-optimist who finds the strongest reason to buy "+
					"even when the panel leans bearish; refuses to settle for neutral."),
		)
		bear := agent.NewBearResearcher(
			"bear@"+fundID,
			"Bear Researcher",
			fundID, llmClient,
			agent.WithAdvocatePersona(
				"a risk-first sceptic who finds the strongest reason to sell "+
					"or avoid even when the panel leans bullish; refuses to settle for neutral."),
		)
		cfg := agent.DebateConfig{
			MaxRounds:          2,
			PerArgumentTimeout: 20 * time.Second,
		}
		return agent.NewDebate(bull, bear, cfg)
	}
}
