// Package debate implements the multi-agent structured roundtable
// introduced in Phase 2B of the auto-execute + decision overhaul.
//
// The legacy roundtable in runtimeResearcherPool.Roundtable was a
// plain string concatenation of every researcher's report. That
// pipeline could not surface dissent ("bull says buy, bear says
// trim") and gave the PMAgent a single bag of bullet points with no
// way to weigh competing views. Phase 2B replaces it with three LLM
// roles — Bull / Bear / Quant — that produce structured views each
// round, see each other's prior-round views in the next round, and
// converge (or hit a max-rounds cap) before emitting a consolidated
// RoundtableOutput.
//
// Design goals:
//
//   - Keep the package free of repository / market-data imports.
//     The wiring layer (cmd/server) populates the DebateInput from
//     ResearcherPool outputs + fund state and consumes the structured
//     RoundtableOutput; debate stays a pure orchestration kernel.
//   - Tolerate LLM failure on any single role: a missing view is
//     treated as "neutral, 0.5 confidence" so one provider blip can't
//     stop the workflow. The wiring layer can still fall back to the
//     legacy concatenation if Run returns an error.
//   - Be drop-in compatible with workflow.RoundtableResult: the
//     orchestrator output is converted to the existing Consensus
//     bullet form so unchanged downstream code keeps working, with
//     the richer structured fields available to opt-in callers
//     (Phase 2A PMAgent).
package debate

import (
	"context"
	"strings"
	"time"
)

// DebateInput captures everything the bull/bear/quant agents need to
// argue about a fund's plan for the day. The orchestrator handles
// none of the data plumbing — the wiring layer is expected to
// populate the relevant fields from research reports, market
// snapshots, and the existing roundtable input.
type DebateInput struct {
	// FundID and TradingDate are forwarded into prompts and usage
	// records for traceability.
	FundID      string
	TradingDate time.Time

	// Market is the canonical lowercase market tag (a_share /
	// us_equity / crypto / futures / hk_stock). Agents adapt their
	// reasoning style — e.g., A-share-only agents prefer T+1 / lot
	// guidance, crypto agents lean on 24/7 liquidity arguments.
	Market string

	// Universe is the candidate symbol list the operator configured
	// under fund.config.universe.symbols. Agents grade each symbol
	// individually so the PMAgent can pick from the per-symbol
	// verdicts.
	Universe []string

	// MacroBrief is the morning macro paragraph from the existing
	// macro researcher. Forwarded verbatim into bull/bear prompts so
	// they can use it as backdrop.
	MacroBrief string

	// StockReports are per-symbol research blurbs produced by the
	// pre-existing per-focus researchers (focus="stock"). The bull
	// and bear agents lean on these for symbol-specific arguments.
	StockReports []string

	// QuantSignals carries the pre-Phase-2C quant briefings: indicator
	// snapshots or numeric momentum signals. Phase 2C will swap these
	// for real OHLC-driven indicators; the schema here is unchanged.
	QuantSignals []string

	// FundamentalReports are optional fundamental snippets (PE / PB /
	// margin / growth narratives). nil-safe; the package treats empty
	// as "no fundamental coverage".
	FundamentalReports []string

	// MaxRounds caps the number of debate rounds. <= 0 means use the
	// orchestrator's default (2).
	MaxRounds int

	// ConvergenceThreshold is the cosine-similarity floor at which
	// the orchestrator considers two consecutive rounds "converged"
	// for an agent. 0 disables convergence detection (always run all
	// MaxRounds). Default is 0.9 when zero.
	ConvergenceThreshold float64
}

// AgentRole is the persona an LLM researcher takes on. Pinned as a
// string enum so the orchestrator (and observability) can route per
// role without a switch on the agent struct.
type AgentRole string

const (
	RoleBull  AgentRole = "bull"
	RoleBear  AgentRole = "bear"
	RoleQuant AgentRole = "quant"
)

// SymbolVerdict is one role's read on one symbol after a single
// debate round. Direction is one of "bull" / "bear" / "neutral".
// Confidence is in [0,1]. KeyPoints are bulletized rationales the
// orchestrator surfaces to peers in the next round.
type SymbolVerdict struct {
	Symbol     string
	Direction  string
	Confidence float64
	KeyPoints  []string
}

// AgentView is a single role's output for one round. It groups every
// SymbolVerdict the agent emitted, plus a one-liner stance the
// orchestrator merges into the final OverallStance.
type AgentView struct {
	Role       AgentRole
	Round      int
	Stance     string
	Verdicts   []SymbolVerdict
	Confidence float64
}

// RoundtableOutput is the structured result of the entire debate.
// The wiring layer translates it to workflow.RoundtableResult so the
// existing PMAgent / persistence path is unchanged; richer callers
// (Phase 2A LLMDecisionEngine) inspect Symbols directly.
type RoundtableOutput struct {
	Rounds          int
	OverallStance   string
	BullCase        string
	BearCase        string
	QuantCase       string
	Symbols         []SymbolDebate
	Converged       bool
	ConvergedRounds int
}

// SymbolDebate aggregates the three roles' final verdicts on a single
// symbol. Verdict is the orchestrator's majority/quant-tiebreak rule
// applied to the three Direction values. DissentVotes counts the
// agents that voted against Verdict — the auto-execute gate can use
// this as a soft de-rate signal (high dissent → demand higher
// confidence floor before letting it through).
type SymbolDebate struct {
	Symbol       string
	Verdict      string
	BullCase     string
	BearCase     string
	QuantCase    string
	DissentVotes int
}

// Researcher is the interface a single role implements. The
// orchestrator runs all researchers in parallel each round and feeds
// their previous-round views back in as PeerViews. Returning an
// error is acceptable — the orchestrator treats it as "this round
// the agent abstained" and reuses the previous-round view (or a
// neutral default for round 0).
type Researcher interface {
	Role() AgentRole
	Debate(ctx context.Context, input DebateInput, round int, peers []AgentView) (*AgentView, error)
}

// Roundtable is the orchestrator-facing API. Run drives the multi-
// round debate from input until convergence (or MaxRounds) and
// returns the consolidated output.
type Roundtable interface {
	Run(ctx context.Context, input DebateInput) (*RoundtableOutput, error)
}

// joinKeyPoints flattens a verdict's KeyPoints into a single string
// the orchestrator uses for similarity scoring. Exposed as a helper
// so tests can call the same canonical form the orchestrator does.
func joinKeyPoints(v SymbolVerdict) string {
	if len(v.KeyPoints) == 0 {
		return strings.TrimSpace(v.Direction)
	}
	parts := make([]string, 0, len(v.KeyPoints)+1)
	parts = append(parts, strings.TrimSpace(v.Direction))
	for _, kp := range v.KeyPoints {
		if trimmed := strings.TrimSpace(kp); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return strings.Join(parts, " | ")
}
