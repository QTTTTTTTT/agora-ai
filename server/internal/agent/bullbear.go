// bullbear.go — S8.2 Bull / Bear adversarial researchers.
//
// Where S8.1 produced four independent analyst reports, S8.2
// introduces two adversarial roles that stress-test the panel
// before it reaches the PM:
//
//   - BullResearcher: forced to argue the bullish thesis.
//     Reads the panel + supporting analyst reports and produces
//     the strongest bullish opinion, citing only the supportive
//     findings and discounting the bearish risks.
//
//   - BearResearcher: forced to argue the bearish thesis.
//     Same input, opposite charter — surfaces the risk register,
//     dismisses the bullish narrative when the numbers don't
//     back it, and refuses to "settle" on neutral.
//
// Together they fuel the existing workflow.DebateGraph as
// supports / rebuts edges across rounds. The PM (S9.4) then
// reads the debate verdict + the Bull/Bear transcript, never
// the raw analyst reports — separation of concerns.
//
// Why forced personas rather than another vote: the four S8.1
// analysts have an explicit "sit out → neutral" failure mode
// when their input block is missing. That's correct for
// information gathering. For the debate it's wrong: the PM
// learns much more from "what's the strongest case for buying
// despite weak data?" than from "we have no opinion".
// Bull/Bear are obligated to take a side every round.

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Stance taxonomy
// ---------------------------------------------------------------------------

// AdvocateStance is the forced posture of an adversarial
// researcher. Mirrors TradingAgents' Bull/Bear convention.
type AdvocateStance string

const (
	StanceBull AdvocateStance = "bull"
	StanceBear AdvocateStance = "bear"
)

// IsValid reports whether s is bull or bear.
func (s AdvocateStance) IsValid() bool { return s == StanceBull || s == StanceBear }

// Opposite returns the contrary stance.
func (s AdvocateStance) Opposite() AdvocateStance {
	if s == StanceBull {
		return StanceBear
	}
	return StanceBull
}

// directionFor maps a forced stance to the direction the
// resulting AnalystReport must carry.
func (s AdvocateStance) directionFor() Direction {
	if s == StanceBear {
		return DirectionBearish
	}
	return DirectionBullish
}

// ---------------------------------------------------------------------------
// AdvocateAgent interface + base
// ---------------------------------------------------------------------------

// AdvocateAgent is the S8.2 first-class interface for the two
// adversarial researchers. Both Bull and Bear implement Argue
// for one symbol given the four analyst reports + (optional)
// the opponent's most recent argument from the previous round.
type AdvocateAgent interface {
	ID() string
	Name() string
	Stance() AdvocateStance
	Persona() string
	Argue(ctx context.Context, input AdvocateInput) (AdvocateArgument, error)
}

// AdvocateInput is the per-round input. The wiring layer fills
// it once per (symbol, round) and hands it to both researchers
// in parallel.
type AdvocateInput struct {
	Symbol     string
	AssetClass string
	Market     string
	AsOf       time.Time
	Round      int

	// PanelReport carries the four analyst reports. Both Bull
	// and Bear see the same panel — the prompt is what forces
	// them to weigh it differently.
	Panel PanelReport

	// Opponent is the contrary researcher's most-recent
	// argument, when available (round >= 2). Empty in round 1.
	Opponent AdvocateArgument

	// Notes lets the operator inject extra context per debate
	// (e.g. "FOMC tomorrow") without changing the schema.
	Notes string
}

// AdvocateArgument is one round of one researcher's case. It
// converts cleanly to a workflow.ResearcherOpinion via
// ToResearcherOpinion so the existing DebateGraph pipeline
// consumes it without changes.
type AdvocateArgument struct {
	AgentID      string
	AgentName    string
	Stance       AdvocateStance
	Symbol       string
	Round        int
	AsOf         time.Time
	GeneratedAt  time.Time

	// Direction is always derived from Stance — bull → bullish,
	// bear → bearish. Stored here so downstream consumers
	// (DebateGraph in particular) don't need to special-case
	// the stance enum.
	Direction Direction

	// Confidence is the strength the advocate is willing to
	// stake on this round (0..100). Forced to be ≥ 30 — a true
	// debate can't have a participant who says "weak claim".
	Confidence int

	// Thesis is the one-paragraph argument for this round.
	Thesis string

	// SupportPoints are the bullets the advocate uses to
	// support its case. Drawn from the panel's findings
	// (bullish stance) or risks (bearish stance).
	SupportPoints []string

	// Rebuttals are direct counters to the opponent's
	// SupportPoints. Empty in round 1.
	Rebuttals []string

	// CitedReports lists the per-category reports the advocate
	// quoted (by category). Lets the UI highlight which
	// analyst's report fuelled each side.
	CitedReports []AnalystCategory

	// LLMModel is "llm" when the LLM filled in the thesis,
	// "fallback" otherwise.
	LLMModel string
}

// Validate enforces the must-have fields.
func (a AdvocateArgument) Validate() error {
	if strings.TrimSpace(a.Symbol) == "" {
		return errors.New("advocate: argument.Symbol required")
	}
	if !a.Stance.IsValid() {
		return fmt.Errorf("advocate: argument.Stance %q invalid", a.Stance)
	}
	switch a.Direction {
	case DirectionBullish, DirectionBearish:
	default:
		return fmt.Errorf("advocate: argument.Direction %q must be bullish or bearish", a.Direction)
	}
	if a.Confidence < 0 || a.Confidence > 100 {
		return fmt.Errorf("advocate: argument.Confidence %d out of [0,100]", a.Confidence)
	}
	if strings.TrimSpace(a.Thesis) == "" {
		return errors.New("advocate: argument.Thesis required")
	}
	if len(a.SupportPoints) == 0 {
		return errors.New("advocate: argument.SupportPoints must have at least one entry")
	}
	if a.Round < 1 {
		return fmt.Errorf("advocate: argument.Round %d must be >= 1", a.Round)
	}
	return nil
}

// advocateBase holds the shared fields both Bull and Bear use.
type advocateBase struct {
	mu      sync.RWMutex
	id      string
	name    string
	stance  AdvocateStance
	persona string
	fundID  string
	llm     LLMClient
	logger  *slog.Logger
	now     func() time.Time
}

// ID returns the advocate's identifier.
func (a *advocateBase) ID() string { return a.id }

// Name returns the advocate's display name.
func (a *advocateBase) Name() string { return a.name }

// Stance returns bull / bear.
func (a *advocateBase) Stance() AdvocateStance { return a.stance }

// Persona returns the optional persona snippet.
func (a *advocateBase) Persona() string { return a.persona }

// AdvocateOption configures an AdvocateAgent.
type AdvocateOption func(*advocateBase)

// WithAdvocateLogger overrides the default logger.
func WithAdvocateLogger(l *slog.Logger) AdvocateOption {
	return func(b *advocateBase) {
		if l != nil {
			b.logger = l
		}
	}
}

// WithAdvocatePersona sets a persona snippet appended to the
// system prompt.
func WithAdvocatePersona(p string) AdvocateOption {
	return func(b *advocateBase) {
		b.persona = strings.TrimSpace(p)
	}
}

// WithAdvocateClock injects a deterministic clock for tests.
func WithAdvocateClock(now func() time.Time) AdvocateOption {
	return func(b *advocateBase) {
		if now != nil {
			b.now = now
		}
	}
}

func newAdvocateBase(id, name string, stance AdvocateStance, fundID string, llm LLMClient, opts ...AdvocateOption) *advocateBase {
	b := &advocateBase{
		id:     id,
		name:   name,
		stance: stance,
		fundID: fundID,
		llm:    llm,
		logger: slog.Default(),
		now:    time.Now,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// ---------------------------------------------------------------------------
// Concrete advocates
// ---------------------------------------------------------------------------

// BullResearcher is the forced-bullish advocate.
type BullResearcher struct {
	*advocateBase
}

// NewBullResearcher constructs the bullish advocate.
func NewBullResearcher(id, name, fundID string, llm LLMClient, opts ...AdvocateOption) *BullResearcher {
	return &BullResearcher{advocateBase: newAdvocateBase(id, name, StanceBull, fundID, llm, opts...)}
}

// Argue runs the bullish case on the panel + previous opponent.
func (a *BullResearcher) Argue(ctx context.Context, input AdvocateInput) (AdvocateArgument, error) {
	return runAdvocate(ctx, a.advocateBase, input)
}

// BearResearcher is the forced-bearish advocate.
type BearResearcher struct {
	*advocateBase
}

// NewBearResearcher constructs the bearish advocate.
func NewBearResearcher(id, name, fundID string, llm LLMClient, opts ...AdvocateOption) *BearResearcher {
	return &BearResearcher{advocateBase: newAdvocateBase(id, name, StanceBear, fundID, llm, opts...)}
}

// Argue runs the bearish case on the panel + previous opponent.
func (a *BearResearcher) Argue(ctx context.Context, input AdvocateInput) (AdvocateArgument, error) {
	return runAdvocate(ctx, a.advocateBase, input)
}

// runAdvocate is shared by both stances since the prompt
// machinery is symmetric — only the system prompt text and the
// support / rebuttal extraction differ on stance.
func runAdvocate(ctx context.Context, b *advocateBase, input AdvocateInput) (AdvocateArgument, error) {
	if strings.TrimSpace(input.Symbol) == "" {
		return AdvocateArgument{}, errors.New("advocate: input.Symbol required")
	}
	if input.Round < 1 {
		input.Round = 1
	}

	// 1) Deterministic skeleton from the panel.
	supports, rebuttals := extractAdvocatePoints(b.stance, input)
	cited := citedCategories(b.stance, input.Panel)

	arg := AdvocateArgument{
		AgentID:       b.id,
		AgentName:     b.name,
		Stance:        b.stance,
		Symbol:        input.Symbol,
		Round:         input.Round,
		AsOf:          input.AsOf,
		GeneratedAt:   b.now(),
		Direction:     b.stance.directionFor(),
		Confidence:    advocateBaseConfidence(b.stance, input.Panel),
		Thesis:        fallbackAdvocateThesis(b.stance, input, supports),
		SupportPoints: supports,
		Rebuttals:     rebuttals,
		CitedReports:  cited,
		LLMModel:      "fallback",
	}

	// 2) LLM enrichment (optional).
	if b.llm != nil {
		sys := buildAdvocateSystemPrompt(b)
		user := buildAdvocateUserPrompt(b.stance, input, supports, rebuttals)
		if parsed, err := callAdvocateLLM(ctx, b.llm, sys, user); err == nil {
			if t := strings.TrimSpace(parsed.Thesis); t != "" {
				arg.Thesis = t
			}
			if parsed.Confidence > 0 {
				arg.Confidence = clampAdvocateConfidence(parsed.Confidence)
			}
			if len(parsed.SupportPoints) > 0 {
				arg.SupportPoints = parsed.SupportPoints
			}
			if len(parsed.Rebuttals) > 0 {
				arg.Rebuttals = parsed.Rebuttals
			}
			arg.LLMModel = "llm"
		} else {
			b.logger.Warn("advocate: LLM failed, using fallback",
				"stance", b.stance, "symbol", input.Symbol, "err", err)
		}
	}

	if err := arg.Validate(); err != nil {
		return AdvocateArgument{}, err
	}
	return arg, nil
}

// ToResearcherOpinion projects the AdvocateArgument into the
// loose-typed shape workflow.RoundtableEngine consumes so the
// existing DebateGraph pipeline + ConsensusItem code keeps
// working unchanged.
//
// We return a value of the right shape but don't depend on the
// workflow package here (would create an import cycle). The
// adapter at the wiring layer turns this into a
// workflow.ResearcherOpinion.
type advocateOpinionWire struct {
	AgentID    string
	AgentName  string
	Focus      string
	Symbol     string
	Direction  string
	Confidence int
	Reasoning  string
	DataPoints []string
}

// ToOpinion returns the loose-typed projection.
func (a AdvocateArgument) ToOpinion() advocateOpinionWire {
	focus := "stock"
	if a.Stance == StanceBear {
		focus = "stock"
	}
	dp := make([]string, 0, len(a.SupportPoints)+len(a.Rebuttals))
	dp = append(dp, a.SupportPoints...)
	for _, r := range a.Rebuttals {
		dp = append(dp, "rebut: "+r)
	}
	return advocateOpinionWire{
		AgentID:    a.AgentID,
		AgentName:  a.AgentName,
		Focus:      focus,
		Symbol:     a.Symbol,
		Direction:  string(a.Direction),
		Confidence: a.Confidence,
		Reasoning:  a.Thesis,
		DataPoints: dp,
	}
}

// ---------------------------------------------------------------------------
// Deterministic skeleton helpers
// ---------------------------------------------------------------------------

// extractAdvocatePoints pulls the support + rebuttal bullets
// the advocate would lean on from a deterministic read of the
// panel. Bull side gathers each analyst's KeyFindings; Bear
// gathers each analyst's Risks. Rebuttals are filled when an
// opponent argument is present.
func extractAdvocatePoints(stance AdvocateStance, input AdvocateInput) (supports, rebuttals []string) {
	cats := make([]AnalystCategory, 0, len(input.Panel.Reports))
	for c := range input.Panel.Reports {
		cats = append(cats, c)
	}
	sort.Slice(cats, func(i, j int) bool { return cats[i] < cats[j] })
	for _, c := range cats {
		r := input.Panel.Reports[c]
		if stance == StanceBull {
			for _, f := range r.KeyFindings {
				supports = append(supports, fmt.Sprintf("%s: %s", c, f))
			}
		} else {
			for _, f := range r.Risks {
				supports = append(supports, fmt.Sprintf("%s: %s", c, f))
			}
		}
	}
	if len(supports) == 0 {
		// Forced to argue → fall back to a high-level claim
		// even when nothing concrete supports the stance.
		if stance == StanceBull {
			supports = append(supports,
				"panel aggregate direction "+string(input.Panel.Aggregate.Direction)+
					" leaves room for further upside")
		} else {
			supports = append(supports,
				"panel aggregate confidence "+
					fmtInt(input.Panel.Aggregate.Confidence)+
					"% is not high enough to justify holding the position")
		}
	}
	// Rebuttals only apply when the opponent already spoke.
	if input.Opponent.Stance.IsValid() && len(input.Opponent.SupportPoints) > 0 {
		for _, p := range input.Opponent.SupportPoints {
			rebuttals = append(rebuttals, "counter: "+p)
			if len(rebuttals) >= 3 {
				break
			}
		}
	}
	return supports, rebuttals
}

// citedCategories lists which analyst categories actually
// contributed to the deterministic support set.
func citedCategories(stance AdvocateStance, panel PanelReport) []AnalystCategory {
	cited := map[AnalystCategory]bool{}
	for c, r := range panel.Reports {
		if stance == StanceBull && len(r.KeyFindings) > 0 {
			cited[c] = true
		}
		if stance == StanceBear && len(r.Risks) > 0 {
			cited[c] = true
		}
	}
	out := make([]AnalystCategory, 0, len(cited))
	for c := range cited {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// advocateBaseConfidence picks the initial conviction the
// advocate brings into the round before any LLM commentary:
//
//   - Bull: average confidence of the bullish analysts plus a
//     +10 bonus for each analyst that agrees with the stance;
//     floor at 30 so a forced advocate is never weak.
//   - Bear: mirror image, using bearish analysts and risk count.
//
// Result is clamped to [30, 95] — an advocate at 100% is rarely
// honest and we want to leave room for the verdict resolver to
// register a real disagreement.
func advocateBaseConfidence(stance AdvocateStance, panel PanelReport) int {
	matched := 0
	confSum := 0
	for _, r := range panel.Reports {
		if stance == StanceBull && r.Direction == DirectionBullish {
			matched++
			confSum += r.Confidence
		}
		if stance == StanceBear && r.Direction == DirectionBearish {
			matched++
			confSum += r.Confidence
		}
	}
	base := 50
	if matched > 0 {
		base = confSum/matched + matched*5
	}
	return clampAdvocateConfidence(base)
}

func clampAdvocateConfidence(c int) int {
	if c < 30 {
		return 30
	}
	if c > 95 {
		return 95
	}
	return c
}

func fallbackAdvocateThesis(stance AdvocateStance, input AdvocateInput, supports []string) string {
	var b strings.Builder
	if stance == StanceBull {
		fmt.Fprintf(&b, "Bull case on %s, round %d. ", input.Symbol, input.Round)
	} else {
		fmt.Fprintf(&b, "Bear case on %s, round %d. ", input.Symbol, input.Round)
	}
	if len(supports) > 0 {
		fmt.Fprintf(&b, "Lead point: %s.", truncateRunes(supports[0], 140))
	}
	if input.Round > 1 && len(input.Opponent.SupportPoints) > 0 {
		fmt.Fprintf(&b, " Counter to opponent: %s.", truncateRunes(input.Opponent.SupportPoints[0], 100))
	}
	return b.String()
}

func fmtInt(x int) string { return fmt.Sprintf("%d", x) }

// ---------------------------------------------------------------------------
// LLM prompt + parsing
// ---------------------------------------------------------------------------

type advocateLLMReply struct {
	Direction     string   `json:"direction"`
	Confidence    int      `json:"confidence"`
	Thesis        string   `json:"thesis"`
	SupportPoints []string `json:"support_points"`
	Rebuttals     []string `json:"rebuttals"`
}

func callAdvocateLLM(ctx context.Context, llm LLMClient, sys, user string) (advocateLLMReply, error) {
	var (
		raw string
		err error
	)
	// S8.3: prefer native structured output when the underlying
	// client supports it. The schema enforces stance =
	// bullish/bearish, conf 30-95, and the support_points /
	// rebuttals arrays.
	if schemaClient, ok := llm.(SchemaLLMClient); ok {
		raw, err = schemaClient.CompleteWithSchema(ctx, sys, user, AdvocateArgumentJSONSchema)
	} else {
		raw, err = llm.Complete(ctx, sys, user)
	}
	if err != nil {
		return advocateLLMReply{}, fmt.Errorf("llm: %w", err)
	}
	// Reuse the parseLLMJSONReport tolerant parser since the
	// envelope shape overlaps with analyst.go. We unmarshal
	// twice: once into the shared shape, then once into the
	// advocate-specific shape so support_points / rebuttals
	// surface correctly even when the LLM follows the analyst
	// schema by accident.
	gen, gerr := parseLLMJSONReport(raw)
	if gerr == nil && (len(gen.KeyFindings) > 0 || len(gen.Risks) > 0) {
		return advocateLLMReply{
			Direction:     gen.Direction,
			Confidence:    gen.Confidence,
			Thesis:        gen.Thesis,
			SupportPoints: gen.KeyFindings,
			Rebuttals:     gen.Risks,
		}, nil
	}
	body := strings.TrimSpace(raw)
	if body == "" {
		return advocateLLMReply{}, errors.New("empty llm reply")
	}
	out, perr := parseAdvocateJSON(body)
	if perr != nil {
		return advocateLLMReply{}, perr
	}
	return out, nil
}

func parseAdvocateJSON(body string) (advocateLLMReply, error) {
	gen, err := parseLLMJSONReport(body)
	if err != nil {
		return advocateLLMReply{}, err
	}
	out := advocateLLMReply{
		Direction:     gen.Direction,
		Confidence:    gen.Confidence,
		Thesis:        gen.Thesis,
		SupportPoints: gen.KeyFindings,
		Rebuttals:     gen.Risks,
	}
	return out, nil
}

func buildAdvocateSystemPrompt(b *advocateBase) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "You are %s, a %s researcher on fund %s. ", b.name, b.stance, b.fundID)
	if b.stance == StanceBull {
		sb.WriteString("You are FORCED to argue the bullish case for the named symbol. ")
		sb.WriteString("Find the strongest reasons to BUY, lean on the supporting analyst findings, ")
		sb.WriteString("and treat the bearish risks as objections you must rebut. You are not allowed ")
		sb.WriteString("to settle on neutral — pick the most defensible bullish thesis even when the ")
		sb.WriteString("panel is mixed.")
	} else {
		sb.WriteString("You are FORCED to argue the bearish case for the named symbol. ")
		sb.WriteString("Find the strongest reasons to SELL or AVOID, lean on the risk findings, and ")
		sb.WriteString("treat the bullish narrative as objections you must rebut. You are not allowed ")
		sb.WriteString("to settle on neutral — pick the most defensible bearish thesis even when the ")
		sb.WriteString("panel is mixed.")
	}
	if b.persona != "" {
		fmt.Fprintf(&sb, " Persona: %s.", b.persona)
	}
	sb.WriteString("\n\nReturn ONLY a JSON object with this exact shape, no markdown:")
	sb.WriteString(`
{
  "direction": "bullish" | "bearish",
  "confidence": <int 30-95>,
  "thesis": "<one-paragraph>",
  "key_findings": ["<bullet>", ...],
  "risks": ["<rebuttal of opponent>", ...]
}`)
	sb.WriteString("\n\nIn this debate, \"key_findings\" are your support points and \"risks\" are your ")
	sb.WriteString("direct rebuttals to the opponent (empty on round 1).")
	return sb.String()
}

func buildAdvocateUserPrompt(stance AdvocateStance, input AdvocateInput, supports, rebuttals []string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Symbol: %s (%s / %s)\n", input.Symbol, input.Market, input.AssetClass)
	fmt.Fprintf(&sb, "Round: %d\n", input.Round)
	fmt.Fprintf(&sb, "Panel aggregate: %s (confidence %d%%, %d categories voted)\n\n",
		input.Panel.Aggregate.Direction, input.Panel.Aggregate.Confidence, input.Panel.Aggregate.CategoriesVoted)

	sb.WriteString("Per-analyst reports:\n")
	cats := make([]AnalystCategory, 0, len(input.Panel.Reports))
	for c := range input.Panel.Reports {
		cats = append(cats, c)
	}
	sort.Slice(cats, func(i, j int) bool { return cats[i] < cats[j] })
	for _, c := range cats {
		r := input.Panel.Reports[c]
		fmt.Fprintf(&sb, "  - %s (%s, conf %d%%): %s\n",
			c, r.Direction, r.Confidence, truncateRunes(r.Thesis, 200))
		if stance == StanceBull && len(r.KeyFindings) > 0 {
			for _, f := range r.KeyFindings {
				fmt.Fprintf(&sb, "      + %s\n", truncateRunes(f, 140))
			}
		}
		if stance == StanceBear && len(r.Risks) > 0 {
			for _, f := range r.Risks {
				fmt.Fprintf(&sb, "      ! %s\n", truncateRunes(f, 140))
			}
		}
	}
	if input.Opponent.Stance.IsValid() && len(input.Opponent.SupportPoints) > 0 {
		fmt.Fprintf(&sb, "\nOpponent (%s) said in round %d: %q\n",
			input.Opponent.Stance, input.Opponent.Round,
			truncateRunes(input.Opponent.Thesis, 300))
		sb.WriteString("Opponent support points:\n")
		for _, p := range input.Opponent.SupportPoints {
			fmt.Fprintf(&sb, "  - %s\n", truncateRunes(p, 200))
		}
	}
	if len(supports) > 0 {
		sb.WriteString("\nDeterministic skeleton you may lean on:\n")
		for _, s := range supports {
			fmt.Fprintf(&sb, "  - %s\n", truncateRunes(s, 200))
		}
	}
	if len(rebuttals) > 0 {
		sb.WriteString("Skeleton rebuttals to consider:\n")
		for _, r := range rebuttals {
			fmt.Fprintf(&sb, "  - %s\n", truncateRunes(r, 200))
		}
	}
	if strings.TrimSpace(input.Notes) != "" {
		fmt.Fprintf(&sb, "\nOperator notes: %s\n", input.Notes)
	}
	return sb.String()
}
