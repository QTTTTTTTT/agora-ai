package decision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fundai/server/internal/llm"
)

// ThreeStageEngine implements the Sprint 9.4 split of the PM
// decision into a trader-proposal → risk-assessment → PM-final
// pipeline, modelled on how a real institutional desk takes a
// trade from idea to authorisation.
//
// Why three stages instead of one prompt:
//
//   - Trader proposals frame the candidate actions in execution
//     language (size, urgency, side). Forcing the LLM to emit
//     this AS A SEPARATE STAGE biases the model toward concrete
//     trades instead of equivocating prose.
//   - Risk assessment runs against the proposal, so concerns are
//     attached to specific actions ("this 8% AAPL add exceeds
//     single-name cap given the existing NVDA hedge"). That's
//     impossible to produce in a single combined prompt because
//     the LLM has to invent the proposal before it can critique it.
//   - The PM final call reads BOTH the proposal AND the risk
//     concerns and emits the final action list. This mirrors the
//     "trader pitches, risk pushes back, PM decides" pattern
//     real funds use to keep accountability separated.
//
// Engineering posture. Each stage is one LLM call. The chain runs
// sequentially because the PM cannot decide without the risk
// assessment, and the risk assessment cannot run without the
// proposal. Total latency is ~3x a single Decide() call; if the
// chain exceeds StageTimeout the partial result so far is
// surfaced via ErrStageTimedOut and the caller can fall back to
// the legacy single-shot engine.
//
// Inner engine. The PM-final stage delegates to an INNER
// DecisionEngine (typically the existing LLMDecisionEngine) so
// the final-stage prompt, parsing contract, and JSON schema are
// unchanged from the legacy path — only the input enrichment is
// new. The enrichment hangs off DecisionInput.TraderProposal and
// DecisionInput.RiskAssessment, which the existing prompt
// renders behind feature flags.
//
// Failure handling. Errors on the trader or risk stages are
// returned WITHOUT calling the PM stage, on the assumption that
// the wiring layer's fallback to the legacy engine is the
// better behaviour than a half-blind PM decision. Errors on the
// PM stage propagate through; the caller deals with them as it
// would for the legacy engine.
type ThreeStageEngine struct {
	// Inner is the engine the third stage delegates to. Usually
	// this is *LLMDecisionEngine but tests pass an in-memory stub.
	// Must not be nil.
	Inner DecisionEngine

	// Client is the LLM client used for stage 1 (Trader.Propose)
	// and stage 2 (Risk.Assess). Typically the SAME client the
	// inner engine uses; we don't share state with the inner
	// engine because callers may want stages 1/2 routed to a
	// cheaper tier than stage 3 (the most consequential call).
	Client llm.LLMClient

	// ProposalTier / AssessmentTier let callers route the cheaper
	// upstream stages to a faster (cheaper) model while keeping
	// the inner PM stage on smart-tier. Zero value falls back to
	// e.Client's defaults.
	ProposalTier   llm.ModelTier
	AssessmentTier llm.ModelTier

	// MaxTokens for stages 1/2. The outputs are short JSON
	// objects so 2000 is generous; 0 = 2000 default.
	MaxTokens int

	// Temperature for stages 1/2. 0 = 0.1 default — the
	// structured outputs benefit from low non-determinism.
	Temperature float64

	// StageTimeout caps each individual stage. Total worst-case
	// latency is roughly 3 * StageTimeout. Zero = 60s.
	StageTimeout time.Duration

	// Identification for accounting + telemetry — same fields as
	// LLMDecisionEngine.
	UserID, AgentID, StepName, FundID string
}

// TraderProposal is the structured output of stage 1.
// Reasoning is the trader's narrative for the candidate plan —
// passed through to the PM stage as additional context.
type TraderProposal struct {
	Stance     string                  `json:"stance"`
	Confidence float64                 `json:"confidence"`
	Actions    []TraderProposedAction  `json:"actions"`
	Reasoning  string                  `json:"reasoning"`
}

// TraderProposedAction is one row of the trader's draft plan.
type TraderProposedAction struct {
	Symbol     string  `json:"symbol"`
	Side       string  `json:"side"` // "buy" | "sell" | "hold" | "reduce" | "add"
	QtyPct     float64 `json:"qtyPct"`
	Urgency    string  `json:"urgency"` // "now" | "today" | "this_week"
	Confidence float64 `json:"confidence"`
}

// RiskAssessment is the structured output of stage 2.
type RiskAssessment struct {
	// Verdict is "approve" | "approve_with_mitigations" | "veto".
	// The PM stage is not bound by it (PM has the final say) but
	// the prompt encourages the PM to honour vetoes unless it
	// can articulate why the risk officer is wrong.
	Verdict   string             `json:"verdict"`
	Concerns  []RiskConcern      `json:"concerns"`
	Mitigations []string         `json:"mitigations"`
	Commentary string            `json:"commentary"`
}

// RiskConcern is one risk officer flag attached to a proposed
// action (or to the plan as a whole when Symbol is empty).
type RiskConcern struct {
	Symbol   string `json:"symbol"`
	Severity string `json:"severity"` // "info" | "warn" | "block"
	Reason   string `json:"reason"`
}

// ErrStageTimedOut is the sentinel returned when one of the
// three stages exceeds StageTimeout. Callers can errors.Is
// against this to know "fall back to legacy single-shot engine"
// is the right move.
var ErrStageTimedOut = errors.New("decision three-stage: stage timed out")

// Decide runs the three-stage pipeline and returns the PM's
// final DecisionOutput. Satisfies the DecisionEngine interface.
//
// Pre-conditions:
//   - e.Inner is non-nil (panics on nil; this is a wiring bug,
//     not a runtime condition).
//   - e.Client is non-nil for stages 1/2; nil falls back to
//     calling e.Inner.Decide directly so the engine degrades
//     to single-stage behaviour rather than erroring out.
func (e *ThreeStageEngine) Decide(ctx context.Context, input DecisionInput) (*DecisionOutput, error) {
	if e == nil || e.Inner == nil {
		return nil, errors.New("three-stage engine: inner engine not configured")
	}
	if e.Client == nil {
		// Degrade to single-stage. Logging this is the wiring
		// layer's job — the engine just refuses to invent calls
		// it doesn't have a client for.
		return e.Inner.Decide(ctx, input)
	}

	proposal, err := e.runProposalStage(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("three-stage: trader proposal: %w", err)
	}

	assessment, err := e.runAssessmentStage(ctx, input, proposal)
	if err != nil {
		return nil, fmt.Errorf("three-stage: risk assessment: %w", err)
	}

	enriched := input
	enriched.TraderProposal = renderTraderProposalForPM(proposal)
	enriched.RiskAssessment = renderRiskAssessmentForPM(assessment)

	out, err := e.Inner.Decide(ctx, enriched)
	if err != nil {
		return nil, fmt.Errorf("three-stage: pm final: %w", err)
	}
	return out, nil
}

func (e *ThreeStageEngine) runProposalStage(ctx context.Context, input DecisionInput) (*TraderProposal, error) {
	stageCtx, cancel := e.stageContext(ctx)
	defer cancel()
	sys := traderProposalSystemPrompt()
	user := userPrompt(input)
	req := llm.ChatRequest{
		Messages: []llm.ChatMessage{
			{Role: "system", Content: sys},
			{Role: "user", Content: user},
		},
		ModelTier:   e.proposalTier(),
		MaxTokens:   e.maxTokens(),
		Temperature: e.temperature(),
		UserID:      e.userID(input),
		AgentID:     e.agentID(input),
		StepName:    e.StepName + ".trader_propose",
		FundID:      e.FundID,
		AgentRole:   "trader",
		RunID:       input.RunID,
	}
	resp, err := e.Client.Chat(stageCtx, req)
	if err != nil {
		if errors.Is(stageCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w (trader): %v", ErrStageTimedOut, err)
		}
		return nil, err
	}
	if resp == nil || strings.TrimSpace(resp.Content) == "" {
		return nil, errors.New("three-stage trader: empty response")
	}
	return parseTraderProposal(resp.Content)
}

func (e *ThreeStageEngine) runAssessmentStage(ctx context.Context, input DecisionInput, proposal *TraderProposal) (*RiskAssessment, error) {
	stageCtx, cancel := e.stageContext(ctx)
	defer cancel()
	sys := riskAssessmentSystemPrompt()
	user := riskAssessmentUserPrompt(input, proposal)
	req := llm.ChatRequest{
		Messages: []llm.ChatMessage{
			{Role: "system", Content: sys},
			{Role: "user", Content: user},
		},
		ModelTier:   e.assessmentTier(),
		MaxTokens:   e.maxTokens(),
		Temperature: e.temperature(),
		UserID:      e.userID(input),
		AgentID:     e.agentID(input),
		StepName:    e.StepName + ".risk_assess",
		FundID:      e.FundID,
		AgentRole:   "risk",
		RunID:       input.RunID,
	}
	resp, err := e.Client.Chat(stageCtx, req)
	if err != nil {
		if errors.Is(stageCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w (risk): %v", ErrStageTimedOut, err)
		}
		return nil, err
	}
	if resp == nil || strings.TrimSpace(resp.Content) == "" {
		return nil, errors.New("three-stage risk: empty response")
	}
	return parseRiskAssessment(resp.Content)
}

func (e *ThreeStageEngine) stageContext(parent context.Context) (context.Context, context.CancelFunc) {
	t := e.StageTimeout
	if t <= 0 {
		t = 60 * time.Second
	}
	return context.WithTimeout(parent, t)
}

func (e *ThreeStageEngine) maxTokens() int {
	if e.MaxTokens > 0 {
		return e.MaxTokens
	}
	return 2000
}

func (e *ThreeStageEngine) temperature() float64 {
	if e.Temperature > 0 {
		return e.Temperature
	}
	return 0.1
}

func (e *ThreeStageEngine) proposalTier() llm.ModelTier {
	if e.ProposalTier != "" {
		return e.ProposalTier
	}
	return llm.TierStandard
}

func (e *ThreeStageEngine) assessmentTier() llm.ModelTier {
	if e.AssessmentTier != "" {
		return e.AssessmentTier
	}
	return llm.TierStandard
}

func (e *ThreeStageEngine) userID(input DecisionInput) string {
	if input.UserID != "" {
		return input.UserID
	}
	return e.UserID
}

func (e *ThreeStageEngine) agentID(input DecisionInput) string {
	if input.PMAgentID != "" {
		return input.PMAgentID
	}
	return e.AgentID
}

func traderProposalSystemPrompt() string {
	return `You are the TRADER on a quantitative fund desk. Read the
market state, analyst panel, and signals below and produce a
PROPOSED action plan — what would you put on the tape if you
had to commit right now? Be concrete: pick names, sides,
sizes (as a fraction of NAV), and an urgency tag.

You are NOT the final decision maker. The portfolio manager
will read your proposal alongside a risk-officer assessment
and may modify or veto any item. Your job is to PROPOSE, not
to second-guess yourself.

Return ONLY a JSON object with this exact shape (no markdown,
no commentary outside the JSON):

{
  "stance": "<one-line: net long, defensive, raising cash, etc.>",
  "confidence": <0..1 plan-level conviction>,
  "actions": [
    {
      "symbol": "<ticker>",
      "side": "buy"|"sell"|"hold"|"reduce"|"add",
      "qtyPct": <0..1, fraction of NAV>,
      "urgency": "now"|"today"|"this_week",
      "confidence": <0..1>
    }
  ],
  "reasoning": "<one-paragraph rationale for the whole plan>"
}

If you'd stand pat, return an empty actions array and explain
why in reasoning. Do NOT fabricate symbols not in the input.`
}

func riskAssessmentSystemPrompt() string {
	return `You are the RISK OFFICER on the fund. The trader has just
proposed the action plan below. Read it against the portfolio
state, exposure / concentration / drawdown signals, and the
fund's stated risk budget. Flag every concern — be specific
about which proposed action triggers each concern.

Severity scale:
- "info"  : worth noting but not blocking.
- "warn"  : the PM should size down or add a hedge.
- "block" : the PM should NOT take this action as proposed
            without a structural change.

If the proposal is fine, return verdict="approve" with an
empty concerns array. If everything blocks, return
verdict="veto". Otherwise verdict="approve_with_mitigations".

Return ONLY a JSON object (no markdown, no prose outside JSON):

{
  "verdict": "approve"|"approve_with_mitigations"|"veto",
  "concerns": [
    {
      "symbol": "<ticker or empty for plan-level>",
      "severity": "info"|"warn"|"block",
      "reason": "<one sentence>"
    }
  ],
  "mitigations": ["<bullet>", ...],
  "commentary": "<one-paragraph summary>"
}`
}

func riskAssessmentUserPrompt(input DecisionInput, proposal *TraderProposal) string {
	var b strings.Builder
	b.WriteString("=== TRADER PROPOSAL ===\n")
	b.WriteString(renderTraderProposalForPM(proposal))
	b.WriteString("\n\n=== PORTFOLIO / SIGNAL CONTEXT ===\n")
	b.WriteString(userPrompt(input))
	return b.String()
}

func renderTraderProposalForPM(p *TraderProposal) string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	if s := strings.TrimSpace(p.Stance); s != "" {
		fmt.Fprintf(&b, "Stance: %s (confidence %.2f)\n", s, p.Confidence)
	} else {
		fmt.Fprintf(&b, "Stance: (unspecified) (confidence %.2f)\n", p.Confidence)
	}
	if len(p.Actions) == 0 {
		b.WriteString("Actions: (stand pat — no candidate trades)\n")
	} else {
		b.WriteString("Actions:\n")
		for _, a := range p.Actions {
			fmt.Fprintf(&b, "  - %s %s qty=%.2f%% urgency=%s conf=%.2f\n",
				strings.ToUpper(strings.TrimSpace(a.Side)),
				strings.ToUpper(strings.TrimSpace(a.Symbol)),
				a.QtyPct*100, a.Urgency, a.Confidence)
		}
	}
	if r := strings.TrimSpace(p.Reasoning); r != "" {
		fmt.Fprintf(&b, "Reasoning: %s\n", r)
	}
	return b.String()
}

func renderRiskAssessmentForPM(a *RiskAssessment) string {
	if a == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Verdict: %s\n", strings.TrimSpace(a.Verdict))
	if len(a.Concerns) > 0 {
		b.WriteString("Concerns:\n")
		for _, c := range a.Concerns {
			sym := strings.TrimSpace(c.Symbol)
			if sym == "" {
				sym = "PLAN"
			}
			fmt.Fprintf(&b, "  - [%s] %s: %s\n",
				strings.ToUpper(strings.TrimSpace(c.Severity)),
				strings.ToUpper(sym),
				strings.TrimSpace(c.Reason))
		}
	}
	if len(a.Mitigations) > 0 {
		b.WriteString("Mitigations:\n")
		for _, m := range a.Mitigations {
			fmt.Fprintf(&b, "  - %s\n", strings.TrimSpace(m))
		}
	}
	if c := strings.TrimSpace(a.Commentary); c != "" {
		fmt.Fprintf(&b, "Commentary: %s\n", c)
	}
	return b.String()
}

func parseTraderProposal(raw string) (*TraderProposal, error) {
	cleaned := stripJSONNoise(raw)
	var dto TraderProposal
	if err := json.Unmarshal([]byte(cleaned), &dto); err != nil {
		return nil, fmt.Errorf("trader proposal unmarshal: %w", err)
	}
	dto.Confidence = clampUnit(dto.Confidence)
	cleanActions := make([]TraderProposedAction, 0, len(dto.Actions))
	for _, a := range dto.Actions {
		side := strings.ToLower(strings.TrimSpace(a.Side))
		switch side {
		case "buy", "sell", "hold", "reduce", "add":
			a.Side = side
		default:
			continue
		}
		a.Symbol = strings.TrimSpace(a.Symbol)
		a.QtyPct = clampUnit(a.QtyPct)
		a.Confidence = clampUnit(a.Confidence)
		urg := strings.ToLower(strings.TrimSpace(a.Urgency))
		switch urg {
		case "now", "today", "this_week":
			a.Urgency = urg
		default:
			a.Urgency = "today"
		}
		cleanActions = append(cleanActions, a)
	}
	dto.Actions = cleanActions
	dto.Stance = strings.TrimSpace(dto.Stance)
	dto.Reasoning = strings.TrimSpace(dto.Reasoning)
	return &dto, nil
}

func parseRiskAssessment(raw string) (*RiskAssessment, error) {
	cleaned := stripJSONNoise(raw)
	var dto RiskAssessment
	if err := json.Unmarshal([]byte(cleaned), &dto); err != nil {
		return nil, fmt.Errorf("risk assessment unmarshal: %w", err)
	}
	dto.Verdict = strings.ToLower(strings.TrimSpace(dto.Verdict))
	switch dto.Verdict {
	case "approve", "approve_with_mitigations", "veto":
		// ok
	default:
		dto.Verdict = "approve_with_mitigations"
	}
	cleanConcerns := make([]RiskConcern, 0, len(dto.Concerns))
	for _, c := range dto.Concerns {
		sev := strings.ToLower(strings.TrimSpace(c.Severity))
		switch sev {
		case "info", "warn", "block":
			c.Severity = sev
		default:
			c.Severity = "info"
		}
		c.Symbol = strings.TrimSpace(c.Symbol)
		c.Reason = strings.TrimSpace(c.Reason)
		if c.Reason == "" {
			continue
		}
		cleanConcerns = append(cleanConcerns, c)
	}
	dto.Concerns = cleanConcerns
	cleanMit := make([]string, 0, len(dto.Mitigations))
	for _, m := range dto.Mitigations {
		m = strings.TrimSpace(m)
		if m != "" {
			cleanMit = append(cleanMit, m)
		}
	}
	dto.Mitigations = cleanMit
	dto.Commentary = strings.TrimSpace(dto.Commentary)
	return &dto, nil
}
