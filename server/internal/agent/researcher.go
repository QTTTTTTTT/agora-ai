// Package agent — researcher.go implements ResearcherAgent, the third
// first-class fund agent (alongside PMAgent and TraderAgent). Before this
// PR, the researcher role only existed as an enum value in the workflow
// package and a free-text "memory" row produced by cmd/server wiring code.
// PMAgent consumed researcher output by string-joining a slice of opaque
// content strings, so there was no structured trail from a research thesis
// to the plan it influenced.
//
// ResearcherAgent fixes that by providing:
//
//   - A structured ResearchBrief output with explicit symbol, direction,
//     confidence, thesis, catalysts, risks, price target, horizon, data
//     points and sources.
//   - A ProduceBrief method that uses an LLMClient (already used by PMAgent)
//     to generate the brief from a MarketContext.
//   - A ToOpinion adapter that converts a brief into the existing
//     workflow.ResearcherOpinion shape so the dormant RoundtableEngine can
//     finally be driven by real, structured research.
//
// The agent's lifecycle (Start/Stop/Logger/Persona) is intentionally
// modelled on PMAgent so that the rest of the system can treat all four
// roles symmetrically.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Public types
// ---------------------------------------------------------------------------

// ResearchFocus describes the analytical lens the researcher applies. Mirrors
// workflow.ResearchFocus to keep the boundary between domain (this package)
// and orchestration (workflow) explicit; callers map between them.
type ResearchFocus string

const (
	FocusStock       ResearchFocus = "stock"
	FocusFundamental ResearchFocus = "fundamental"
	FocusMacro       ResearchFocus = "macro"
	FocusQuant       ResearchFocus = "quant"
)

// ResearchDirection captures the directional view of a brief.
type ResearchDirection string

const (
	DirectionBullish ResearchDirection = "bullish"
	DirectionBearish ResearchDirection = "bearish"
	DirectionNeutral ResearchDirection = "neutral"
)

// MarketContext is the package-internal input handed to ProduceBrief. It is
// deliberately decoupled from marketdata.ResearchContext so this package can
// be unit-tested without dragging the whole market data subsystem in.
type MarketContext struct {
	Symbol       string
	AssetClass   string // "equity", "futures", "crypto", ...
	Market       string // exchange or region code
	AsOf         time.Time
	PriceLast    float64
	PriceChange  float64 // 1d return, fractional
	Volume       float64
	AvgVolume    float64
	Headlines    []string
	Fundamentals map[string]float64 // optional metrics keyed by name
	Signals      map[string]float64 // technical/quant signals keyed by name
	Notes        string             // freeform extra context for the LLM
}

// DataPoint is a single named, typed observation referenced by a brief.
type DataPoint struct {
	Name   string
	Value  string
	Source string // optional; e.g. "wind", "yahoo-finance"
}

// ResearchBrief is the structured output of a ResearcherAgent.
//
// Unlike workflow.ResearchReport (which has a single Content string), every
// field on a brief is queryable: callers can persist it into a research_reports
// table, link plans to it via plan_research_links, and compute
// hit-rate / forward-return aggregates against research_outcomes.
type ResearchBrief struct {
	ID           string
	AgentID      string
	AgentName    string
	FundID       string
	Focus        ResearchFocus
	Symbol       string
	Direction    ResearchDirection
	Confidence   int     // 0-100
	Thesis       string  // one-paragraph investment thesis
	Catalysts    []string
	Risks        []string
	PriceTarget  float64
	HorizonDays  int
	DataPoints   []DataPoint
	Sources      []string
	GeneratedAt  time.Time
}

// Validate returns an error if any required field is missing or out of range.
// Callers should run this before persisting a brief.
func (b ResearchBrief) Validate() error {
	if strings.TrimSpace(b.Symbol) == "" {
		return errors.New("researcher: brief.Symbol is required")
	}
	switch b.Direction {
	case DirectionBullish, DirectionBearish, DirectionNeutral:
	default:
		return fmt.Errorf("researcher: brief.Direction %q invalid", b.Direction)
	}
	if b.Confidence < 0 || b.Confidence > 100 {
		return fmt.Errorf("researcher: brief.Confidence %d out of [0,100]", b.Confidence)
	}
	if b.HorizonDays < 0 {
		return fmt.Errorf("researcher: brief.HorizonDays %d must be ≥0", b.HorizonDays)
	}
	switch b.Focus {
	case FocusStock, FocusFundamental, FocusMacro, FocusQuant, "":
	default:
		return fmt.Errorf("researcher: brief.Focus %q invalid", b.Focus)
	}
	return nil
}

// ToOpinion converts a brief into the loose-typed shape that
// workflow.RoundtableEngine consumes. Callers in the wiring layer can use
// this to drive the dormant engine with structured input.
func (b ResearchBrief) ToOpinion() ResearchResult {
	dp := make(map[string]interface{}, len(b.DataPoints))
	for _, d := range b.DataPoints {
		dp[d.Name] = d.Value
	}
	dp["catalysts"] = strings.Join(b.Catalysts, "; ")
	dp["risks"] = strings.Join(b.Risks, "; ")
	if b.PriceTarget > 0 {
		dp["price_target"] = b.PriceTarget
	}
	if b.HorizonDays > 0 {
		dp["horizon_days"] = b.HorizonDays
	}
	return ResearchResult{
		AgentID:    b.AgentID,
		AgentName:  b.AgentName,
		Focus:      string(b.Focus),
		Symbol:     b.Symbol,
		Direction:  string(b.Direction),
		Summary:    b.Thesis,
		DataPoints: dp,
	}
}

// ---------------------------------------------------------------------------
// ResearcherAgent
// ---------------------------------------------------------------------------

// ResearcherAgent is a first-class fund agent that produces structured
// research briefs from a MarketContext. Safe for concurrent use after Start.
type ResearcherAgent struct {
	mu      sync.RWMutex
	id      string
	name    string
	fundID  string
	focus   ResearchFocus
	persona string
	llm     LLMClient
	logger  *slog.Logger
	now     func() time.Time

	running bool
	stopCh  chan struct{}
}

// ResearcherOption configures optional ResearcherAgent fields.
type ResearcherOption func(*ResearcherAgent)

// WithResearcherLogger overrides the default logger.
func WithResearcherLogger(l *slog.Logger) ResearcherOption {
	return func(r *ResearcherAgent) { r.logger = l }
}

// WithResearcherFocus pins the agent's research focus.
func WithResearcherFocus(f ResearchFocus) ResearcherOption {
	return func(r *ResearcherAgent) { r.focus = f }
}

// WithResearcherPersona sets a persona snippet appended to the system prompt.
// Personas are how operators give a researcher a voice ("a value-investing
// veteran skeptical of mean-reversion") without changing the codebase.
func WithResearcherPersona(persona string) ResearcherOption {
	return func(r *ResearcherAgent) { r.persona = persona }
}

// WithResearcherClock injects a deterministic clock for tests.
func WithResearcherClock(now func() time.Time) ResearcherOption {
	return func(r *ResearcherAgent) {
		if now != nil {
			r.now = now
		}
	}
}

// NewResearcherAgent constructs a ResearcherAgent. llm may be nil — when nil
// the agent falls back to a deterministic, signal-driven brief so callers
// without LLM credit (e.g. unit tests) still get useful output.
func NewResearcherAgent(id, name, fundID string, llm LLMClient, opts ...ResearcherOption) *ResearcherAgent {
	r := &ResearcherAgent{
		id:     id,
		name:   name,
		fundID: fundID,
		focus:  FocusStock,
		llm:    llm,
		logger: slog.Default(),
		now:    time.Now,
		stopCh: make(chan struct{}),
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// ID returns the agent's identifier.
func (r *ResearcherAgent) ID() string { return r.id }

// Name returns the agent's display name.
func (r *ResearcherAgent) Name() string { return r.name }

// Focus returns the configured research focus.
func (r *ResearcherAgent) Focus() ResearchFocus { return r.focus }

// Start initialises the agent (idempotent).
func (r *ResearcherAgent) Start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return
	}
	r.running = true
	r.stopCh = make(chan struct{})
	r.logger.Info("researcher agent started", "id", r.id, "fund", r.fundID, "focus", r.focus)
}

// Stop shuts the agent down.
func (r *ResearcherAgent) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return
	}
	r.running = false
	close(r.stopCh)
	r.logger.Info("researcher agent stopped", "id", r.id)
}

// ProduceBrief generates a single ResearchBrief for ctx's symbol. If an
// LLMClient is configured it is consulted for narrative thesis text;
// directional/confidence fields are still derived deterministically from
// PriceChange and Signals so the output remains testable.
func (r *ResearcherAgent) ProduceBrief(ctx context.Context, mc MarketContext) (ResearchBrief, error) {
	if strings.TrimSpace(mc.Symbol) == "" {
		return ResearchBrief{}, errors.New("researcher: MarketContext.Symbol is required")
	}
	dir, conf := scoreDirection(mc)
	catalysts, risks := summariseSignals(mc)
	now := r.now()

	thesis, err := r.callLLMForThesis(ctx, mc, dir, conf)
	if err != nil {
		// Fail open: the structured brief is still valid even if the
		// narrative thesis couldn't be generated.
		r.logger.Warn("researcher LLM thesis unavailable; using fallback", "err", err, "symbol", mc.Symbol)
		thesis = fallbackThesis(mc, dir, conf)
	}

	brief := ResearchBrief{
		AgentID:     r.id,
		AgentName:   r.name,
		FundID:      r.fundID,
		Focus:       r.focus,
		Symbol:      mc.Symbol,
		Direction:   dir,
		Confidence:  conf,
		Thesis:      thesis,
		Catalysts:   catalysts,
		Risks:       risks,
		HorizonDays: defaultHorizonForFocus(r.focus),
		DataPoints:  buildDataPoints(mc),
		GeneratedAt: now,
	}
	if pt := suggestedPriceTarget(mc, dir, conf); pt > 0 {
		brief.PriceTarget = pt
	}
	if err := brief.Validate(); err != nil {
		return ResearchBrief{}, err
	}
	return brief, nil
}

// ProduceBriefs runs ProduceBrief for each context and collects the
// successful outputs. Errors are logged and skipped so a single bad symbol
// doesn't kill an entire research batch.
func (r *ResearcherAgent) ProduceBriefs(ctx context.Context, mcs []MarketContext) []ResearchBrief {
	out := make([]ResearchBrief, 0, len(mcs))
	for _, mc := range mcs {
		b, err := r.ProduceBrief(ctx, mc)
		if err != nil {
			r.logger.Warn("researcher: ProduceBrief failed", "symbol", mc.Symbol, "err", err)
			continue
		}
		out = append(out, b)
	}
	return out
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// scoreDirection maps PriceChange + Signals into a direction/confidence pair.
// The mapping is intentionally simple: PriceChange weighted at 60%, the
// average of Signals at 40%. Confidence is the absolute value of the blended
// score scaled into [0,100] and clamped.
func scoreDirection(mc MarketContext) (ResearchDirection, int) {
	priceScore := mc.PriceChange
	sigScore := 0.0
	if len(mc.Signals) > 0 {
		var sum float64
		for _, v := range mc.Signals {
			sum += v
		}
		sigScore = sum / float64(len(mc.Signals))
	}
	blend := 0.6*priceScore + 0.4*sigScore
	conf := int(absFloat(blend) * 200)
	if conf > 100 {
		conf = 100
	}
	if conf < 20 {
		conf = 20 // floor: even a flat market means "low conviction", not "zero"
	}
	switch {
	case blend > 0.005:
		return DirectionBullish, conf
	case blend < -0.005:
		return DirectionBearish, conf
	default:
		return DirectionNeutral, conf
	}
}

func summariseSignals(mc MarketContext) (catalysts, risks []string) {
	for name, v := range mc.Signals {
		switch {
		case v >= 0.5:
			catalysts = append(catalysts, fmt.Sprintf("%s strong (%.2f)", name, v))
		case v <= -0.5:
			risks = append(risks, fmt.Sprintf("%s weak (%.2f)", name, v))
		}
	}
	if len(mc.Headlines) > 0 {
		// Each headline is both a potential catalyst and a potential risk;
		// the fallback heuristic surfaces them as catalysts so they're not
		// silently dropped.
		for _, h := range mc.Headlines {
			catalysts = append(catalysts, h)
		}
	}
	if mc.AvgVolume > 0 && mc.Volume > 3*mc.AvgVolume {
		catalysts = append(catalysts, fmt.Sprintf("volume spike: %.1fx avg", mc.Volume/mc.AvgVolume))
	}
	if mc.AvgVolume > 0 && mc.Volume < 0.3*mc.AvgVolume {
		risks = append(risks, fmt.Sprintf("liquidity thin: %.1fx avg", mc.Volume/mc.AvgVolume))
	}
	return catalysts, risks
}

func buildDataPoints(mc MarketContext) []DataPoint {
	var dp []DataPoint
	if mc.PriceLast != 0 {
		dp = append(dp, DataPoint{Name: "price_last", Value: fmt.Sprintf("%.4f", mc.PriceLast)})
	}
	if mc.PriceChange != 0 {
		dp = append(dp, DataPoint{Name: "price_change_1d", Value: fmt.Sprintf("%.4f", mc.PriceChange)})
	}
	for k, v := range mc.Fundamentals {
		dp = append(dp, DataPoint{Name: "fund." + k, Value: fmt.Sprintf("%.4f", v)})
	}
	for k, v := range mc.Signals {
		dp = append(dp, DataPoint{Name: "sig." + k, Value: fmt.Sprintf("%.4f", v)})
	}
	return dp
}

func defaultHorizonForFocus(f ResearchFocus) int {
	switch f {
	case FocusMacro:
		return 90
	case FocusFundamental:
		return 60
	case FocusQuant:
		return 5
	default:
		return 20
	}
}

func suggestedPriceTarget(mc MarketContext, dir ResearchDirection, conf int) float64 {
	if mc.PriceLast <= 0 {
		return 0
	}
	// Confidence-scaled +/- 20% target.
	move := 0.20 * float64(conf) / 100.0
	switch dir {
	case DirectionBullish:
		return mc.PriceLast * (1 + move)
	case DirectionBearish:
		return mc.PriceLast * (1 - move)
	default:
		return 0
	}
}

func fallbackThesis(mc MarketContext, dir ResearchDirection, conf int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s view (confidence %d%%).", mc.Symbol, dir, conf)
	if mc.PriceChange != 0 {
		fmt.Fprintf(&b, " 1d change %.2f%%.", mc.PriceChange*100)
	}
	if len(mc.Headlines) > 0 {
		fmt.Fprintf(&b, " Key headline: %s", mc.Headlines[0])
	}
	return b.String()
}

func (r *ResearcherAgent) callLLMForThesis(ctx context.Context, mc MarketContext, dir ResearchDirection, conf int) (string, error) {
	if r.llm == nil {
		return "", errors.New("researcher: no LLM configured")
	}
	sys := r.buildSystemPrompt()
	user := r.buildUserPrompt(mc, dir, conf)
	return r.llm.Complete(ctx, sys, user)
}

func (r *ResearcherAgent) buildSystemPrompt() string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a research analyst with focus=%s for fund %s. ", r.name, r.focus, r.fundID)
	b.WriteString("Produce a one-paragraph investment thesis: lead with the directional call, ")
	b.WriteString("cite the strongest evidence, name the most material risk, and avoid hedge phrases.")
	if strings.TrimSpace(r.persona) != "" {
		b.WriteString(" Persona: ")
		b.WriteString(r.persona)
	}
	return b.String()
}

func (r *ResearcherAgent) buildUserPrompt(mc MarketContext, dir ResearchDirection, conf int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Symbol: %s (%s/%s)\n", mc.Symbol, mc.Market, mc.AssetClass)
	fmt.Fprintf(&b, "Direction (preliminary): %s, confidence %d%%\n", dir, conf)
	if mc.PriceLast != 0 {
		fmt.Fprintf(&b, "Last price: %.4f, 1d change: %.2f%%\n", mc.PriceLast, mc.PriceChange*100)
	}
	if mc.AvgVolume > 0 {
		fmt.Fprintf(&b, "Volume: %.0f (avg %.0f)\n", mc.Volume, mc.AvgVolume)
	}
	if len(mc.Headlines) > 0 {
		b.WriteString("Headlines:\n")
		for _, h := range mc.Headlines {
			fmt.Fprintf(&b, "  - %s\n", h)
		}
	}
	if len(mc.Signals) > 0 {
		b.WriteString("Signals:\n")
		for k, v := range mc.Signals {
			fmt.Fprintf(&b, "  - %s = %.4f\n", k, v)
		}
	}
	if strings.TrimSpace(mc.Notes) != "" {
		fmt.Fprintf(&b, "Notes: %s\n", mc.Notes)
	}
	b.WriteString("\nReturn ONLY the thesis paragraph; no markdown, no headers.")
	return b.String()
}

func absFloat(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
