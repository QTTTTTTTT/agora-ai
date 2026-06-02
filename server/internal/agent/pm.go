package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fundai/server/internal/instrument"
)

// ---------------------------------------------------------------------------
// Investment style constants
// ---------------------------------------------------------------------------

// InvestmentStyle governs how the PM sizes positions, selects instruments,
// and sets risk parameters.
type InvestmentStyle string

const (
	StyleConservative InvestmentStyle = "conservative"
	StyleBalanced     InvestmentStyle = "balanced"
	StyleAggressive   InvestmentStyle = "aggressive"
	StyleValue        InvestmentStyle = "value"
	StyleGrowth       InvestmentStyle = "growth"
)

const (
	maxPromptHoldings       = 25
	maxPromptConsensus      = 30
	maxPromptActions        = 30
	maxPromptReasoningRunes = 800
	maxPromptMemoryRunes    = 4000
	maxReviewReasoningRunes = 3000
	maxReviewItems          = 30
	maxReviewLineRunes      = 500
	maxLessonsReturned      = 5
	maxLessonRunes          = 500
)

// styleParams encapsulates the numeric knobs that vary by style.
type styleParams struct {
	MaxSinglePosition float64 // fraction of total assets
	MaxTotalExposure  float64 // fraction of total assets
	DefaultStopLoss   float64 // percentage below entry
	DefaultTakeProfit float64 // percentage above entry
	MinConfidence     int     // minimum consensus confidence to act on
	PositionScale     float64 // multiplier applied to base position size
}

var styleDefaults = map[InvestmentStyle]styleParams{
	StyleConservative: {
		MaxSinglePosition: 0.15,
		MaxTotalExposure:  0.70,
		DefaultStopLoss:   0.05,
		DefaultTakeProfit: 0.10,
		MinConfidence:     70,
		PositionScale:     0.6,
	},
	StyleBalanced: {
		MaxSinglePosition: 0.20,
		MaxTotalExposure:  0.85,
		DefaultStopLoss:   0.08,
		DefaultTakeProfit: 0.15,
		MinConfidence:     60,
		PositionScale:     0.8,
	},
	StyleAggressive: {
		MaxSinglePosition: 0.30,
		MaxTotalExposure:  0.95,
		DefaultStopLoss:   0.12,
		DefaultTakeProfit: 0.25,
		MinConfidence:     50,
		PositionScale:     1.2,
	},
	StyleValue: {
		MaxSinglePosition: 0.25,
		MaxTotalExposure:  0.80,
		DefaultStopLoss:   0.07,
		DefaultTakeProfit: 0.20,
		MinConfidence:     65,
		PositionScale:     0.9,
	},
	StyleGrowth: {
		MaxSinglePosition: 0.25,
		MaxTotalExposure:  0.90,
		DefaultStopLoss:   0.10,
		DefaultTakeProfit: 0.30,
		MinConfidence:     55,
		PositionScale:     1.1,
	},
}

func paramsForStyle(s InvestmentStyle) styleParams {
	if p, ok := styleDefaults[s]; ok {
		return p
	}
	return styleDefaults[StyleBalanced]
}

// ---------------------------------------------------------------------------
// Domain types (local to this package)
// ---------------------------------------------------------------------------

// ResearchResult is the output from a single researcher agent.
type ResearchResult struct {
	AgentID    string
	AgentName  string
	Focus      string // "stock", "fundamental", "macro"
	Symbol     string
	Direction  string // "bullish", "bearish", "neutral"
	Summary    string
	DataPoints map[string]interface{}
}

// ConsensusItem records the final agreed-upon position for a symbol.
type ConsensusItem struct {
	Symbol     string
	Direction  string // "bullish", "bearish", "neutral"
	Confidence int    // 0-100
	Supporters []string
	Dissenters []string
	Action     string // "buy", "sell", "hold", "watch"
	Reasoning  string
}

// HoldingPosition represents a current portfolio holding.
type HoldingPosition struct {
	Symbol      string
	Quantity    int
	AvgCost     float64
	MarketPrice float64
	MarketValue float64
	Weight      float64 // fraction of total assets
	PnLPct      float64
	Sector      string
	AvgVolume   int64 // 30-day average daily volume
}

// RiskRule is a single risk constraint the PM must honour.
type RiskRule struct {
	ID          string
	Type        string // "max_position", "max_drawdown", "sector_limit", "stop_loss"
	Param       string // rule-specific key (e.g. sector name)
	Limit       float64
	Description string
}

// PlanInput contains every piece of information the PM needs to produce a
// plan for a single trading day.
type PlanInput struct {
	FundID          string
	TradingDate     string
	ResearchResults []ResearchResult
	Consensus       []ConsensusItem
	Holdings        []HoldingPosition
	AvailableCash   float64
	TotalAssets     float64
	RiskRules       []RiskRule
	MemoryContext   string
	Style           InvestmentStyle
}

// InvestmentPlan is the PM's actionable output for the day.
type InvestmentPlan struct {
	ID             string
	FundID         string
	Date           string
	Status         string // "draft", "risk_review", "pending_user", "approved", "rejected", "executing", "completed"
	Actions        []PlanAction
	Reasoning      string
	RiskScore      float64
	ExpectedReturn float64
	CreatedAt      time.Time
}

// PlanAction is one discrete trade or hold instruction.
type PlanAction struct {
	Symbol      string
	Action      string // "buy", "sell", "hold", "reduce", "add"
	Quantity    int
	Price       float64
	Amount      float64
	StopLoss    float64
	TakeProfit  float64
	Reasoning   string
	Confidence  int
	SupportedBy []string
	OpposedBy   []string
}

// ExecutionResult captures what actually happened when a PlanAction was
// executed by the trading engine.
type ExecutionResult struct {
	Symbol       string
	Action       string
	Strategy     ExecutionStrategy
	RequestedQty int
	FilledQty    int
	AvgFillPrice float64
	Commission   float64
	Status       string // "filled", "partial", "rejected"
	ChildOrders  []TradeResult
	StartedAt    time.Time
	CompletedAt  time.Time
	Error        string
}

// DailyReview is the PM's end-of-day self-assessment.
type DailyReview struct {
	FundID        string
	Date          string
	PlanID        string
	Summary       string
	Hits          []string
	Misses        []string
	Lessons       []string
	PnLToday      float64
	PnLCumulative float64
	Adjustments   []string // suggested changes for tomorrow
	CreatedAt     time.Time
}

// ---------------------------------------------------------------------------
// LLM integration interface
// ---------------------------------------------------------------------------

// LLMClient abstracts the underlying large-language-model call. Callers
// provide a system prompt and a user prompt; the implementation returns the
// model's text completion.
type LLMClient interface {
	Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// SchemaLLMClient is the S8.3 extension that asks providers
// for native structured output (Anthropic tool-use, OpenAI
// response_format=json_schema, Gemini responseSchema, …).
//
// schema is a JSON Schema document the model must satisfy.
// Implementations SHOULD send the schema natively when the
// provider supports it; otherwise they should fall back to
// freeform Complete and rely on the tolerant parsers already
// in analyst.go / bullbear.go.
//
// The returned string is always a JSON object that matches
// schema (or as close to it as the provider produced) — callers
// then unmarshal into their concrete shape.
type SchemaLLMClient interface {
	LLMClient
	CompleteWithSchema(ctx context.Context, systemPrompt, userPrompt string, schema []byte) (string, error)
}

// ---------------------------------------------------------------------------
// PMAgent
// ---------------------------------------------------------------------------

// PMAgent is the portfolio manager — the decision-making brain of the fund.
// It consumes research and consensus, produces investment plans, coordinates
// risk review, and performs daily self-reviews.
type PMAgent struct {
	mu     sync.RWMutex
	id     string
	name   string
	fundID string
	style  InvestmentStyle
	llm    LLMClient
	logger *slog.Logger

	running bool
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

// PMOption configures optional PMAgent fields.
type PMOption func(*PMAgent)

// WithPMLogger overrides the default logger.
func WithPMLogger(l *slog.Logger) PMOption {
	return func(p *PMAgent) { p.logger = l }
}

// WithPMStyle overrides the default investment style.
func WithPMStyle(s InvestmentStyle) PMOption {
	return func(p *PMAgent) { p.style = s }
}

// NewPMAgent creates a new portfolio manager agent.
func NewPMAgent(id, name, fundID string, llm LLMClient, opts ...PMOption) *PMAgent {
	pm := &PMAgent{
		id:     id,
		name:   name,
		fundID: fundID,
		style:  StyleBalanced,
		llm:    llm,
		logger: slog.Default(),
		stopCh: make(chan struct{}),
	}
	for _, o := range opts {
		o(pm)
	}
	return pm
}

// AgentID returns the unique identifier.
func (pm *PMAgent) AgentID() string { return pm.id }

// AgentName returns the human-readable name.
func (pm *PMAgent) AgentName() string { return pm.name }

// Style returns the current investment style.
func (pm *PMAgent) Style() InvestmentStyle {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.style
}

// SetStyle atomically changes the investment style.
func (pm *PMAgent) SetStyle(s InvestmentStyle) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.style = s
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// Start initialises background resources. It is idempotent.
func (pm *PMAgent) Start() error {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.running {
		return nil
	}
	pm.running = true
	pm.logger.Info("pm agent started", "id", pm.id, "fund", pm.fundID, "style", string(pm.style))
	return nil
}

// Stop tears down background resources and blocks until cleanup is complete.
func (pm *PMAgent) Stop() error {
	pm.mu.Lock()
	if !pm.running {
		pm.mu.Unlock()
		return nil
	}
	pm.running = false
	close(pm.stopCh)
	pm.mu.Unlock()

	pm.wg.Wait()
	pm.logger.Info("pm agent stopped", "id", pm.id)
	return nil
}

// ---------------------------------------------------------------------------
// GeneratePlan — the core planning logic
// ---------------------------------------------------------------------------

// GeneratePlan produces an InvestmentPlan for the given trading day. It:
//  1. Scores each consensus item according to the PM's style.
//  2. Determines position sizes within risk-rule constraints.
//  3. Uses the LLM for qualitative reasoning when available.
//  4. Returns a fully populated InvestmentPlan in "draft" status.
func (pm *PMAgent) GeneratePlan(ctx context.Context, input PlanInput) (*InvestmentPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("context cancelled before plan generation: %w", err)
	}

	style := input.Style
	if style == "" {
		style = pm.Style()
	}
	params := paramsForStyle(style)

	pm.logger.Info("generating investment plan",
		"fund", input.FundID,
		"date", input.TradingDate,
		"style", string(style),
		"consensus_items", len(input.Consensus),
		"holdings", len(input.Holdings),
		"available_cash", input.AvailableCash,
	)

	// ---- 1. Score and rank consensus items ----
	scored := pm.scoreConsensus(input.Consensus, input.ResearchResults, params)

	// ---- 2. Build candidate actions (sells first, then buys) ----
	holdingsMap := buildHoldingsMap(input.Holdings)
	var actions []PlanAction

	// 2a. Evaluate existing holdings — sell/reduce losers, hold winners.
	sellActions := pm.evaluateHoldings(input.Holdings, scored, params)
	actions = append(actions, sellActions...)

	// Compute cash freed by sells.
	cashAfterSells := input.AvailableCash
	for _, a := range sellActions {
		if a.Action == "sell" || a.Action == "reduce" {
			cashAfterSells += a.Amount
		}
	}

	// 2b. Compute current exposure after sells.
	currentExposure := pm.currentExposure(input.Holdings, sellActions, input.TotalAssets)

	// 2c. Generate buy/add actions for top-ranked consensus items.
	buyActions := pm.generateBuyActions(scored, holdingsMap, params, input.TotalAssets, cashAfterSells, currentExposure, input.RiskRules)
	actions = append(actions, buyActions...)

	// ---- 3. Validate against risk rules ----
	actions = pm.enforceRiskRules(actions, input.Holdings, input.TotalAssets, input.RiskRules, params)

	// ---- 4. LLM-enhanced reasoning ----
	reasoning, riskScore, expectedReturn := pm.llmReasoning(ctx, input, actions, style)

	plan := &InvestmentPlan{
		ID:             fmt.Sprintf("plan_%s_%s_%d", input.FundID, input.TradingDate, time.Now().UnixMilli()),
		FundID:         input.FundID,
		Date:           input.TradingDate,
		Status:         "draft",
		Actions:        actions,
		Reasoning:      reasoning,
		RiskScore:      riskScore,
		ExpectedReturn: expectedReturn,
		CreatedAt:      time.Now(),
	}

	pm.logger.Info("investment plan generated",
		"plan_id", plan.ID,
		"actions", len(plan.Actions),
		"risk_score", plan.RiskScore,
		"expected_return", plan.ExpectedReturn,
	)

	return plan, nil
}

// ---------------------------------------------------------------------------
// Scoring
// ---------------------------------------------------------------------------

// scoredItem pairs a ConsensusItem with a numeric score for ranking.
type scoredItem struct {
	ConsensusItem
	score float64
}

// scoreConsensus ranks consensus items by confidence and alignment with the
// PM's investment style.
func (pm *PMAgent) scoreConsensus(items []ConsensusItem, research []ResearchResult, params styleParams) []scoredItem {
	// Pre-index research by symbol for fast lookup.
	researchBySymbol := make(map[string][]ResearchResult)
	for _, r := range research {
		researchBySymbol[r.Symbol] = append(researchBySymbol[r.Symbol], r)
	}

	scored := make([]scoredItem, 0, len(items))
	for _, ci := range items {
		if ci.Confidence < params.MinConfidence {
			continue
		}

		s := float64(ci.Confidence) / 100.0

		// Boost for unanimity.
		supportRatio := float64(len(ci.Supporters)) / math.Max(1, float64(len(ci.Supporters)+len(ci.Dissenters)))
		s *= (0.7 + 0.3*supportRatio)

		// Style-specific adjustments.
		s = pm.applyStyleBoost(s, ci, researchBySymbol[ci.Symbol], params)

		scored = append(scored, scoredItem{ConsensusItem: ci, score: s})
	}

	sort.Slice(scored, func(i, j int) bool { return scored[i].score > scored[j].score })
	return scored
}

// applyStyleBoost tunes the raw score to reflect the PM's investment
// philosophy. The adjustments are heuristic; in production they would be
// backed by configurable factor models.
func (pm *PMAgent) applyStyleBoost(base float64, ci ConsensusItem, research []ResearchResult, params styleParams) float64 {
	s := base

	// Look for relevant data points in research.
	var hasPEData, hasGrowthData, hasDividendData bool
	for _, r := range research {
		if r.DataPoints == nil {
			continue
		}
		if _, ok := r.DataPoints["pe_ratio"]; ok {
			hasPEData = true
		}
		if _, ok := r.DataPoints["revenue_growth"]; ok {
			hasGrowthData = true
		}
		if _, ok := r.DataPoints["dividend_yield"]; ok {
			hasDividendData = true
		}
	}

	style := params // already resolved
	_ = style

	switch pm.Style() {
	case StyleConservative:
		// Prefer high-confidence, penalise aggressive actions.
		if ci.Confidence >= 80 {
			s *= 1.15
		}
		if ci.Action == "buy" && ci.Confidence < 70 {
			s *= 0.7
		}
		if hasDividendData {
			s *= 1.10
		}

	case StyleValue:
		// Favour low P/E signals.
		if hasPEData {
			s *= 1.20
		}
		if hasDividendData {
			s *= 1.15
		}
		if ci.Direction == "bearish" && ci.Action == "buy" {
			// Contrarian opportunity — slight boost.
			s *= 1.05
		}

	case StyleGrowth:
		if hasGrowthData {
			s *= 1.25
		}
		// Growth PMs accept higher risk for high conviction.
		if ci.Confidence >= 75 {
			s *= 1.10
		}

	case StyleAggressive:
		// Aggressive PMs amplify high-conviction calls and dampen weak ones.
		if ci.Confidence >= 70 {
			s *= 1.20
		} else {
			s *= 0.80
		}

	case StyleBalanced:
		// No special bias — balanced by design.
	}

	return s
}

// ---------------------------------------------------------------------------
// Holdings evaluation (sells / reduces / holds)
// ---------------------------------------------------------------------------

// evaluateHoldings inspects every current position and decides whether to
// sell, reduce, or hold it based on consensus and stop-loss rules.
func (pm *PMAgent) evaluateHoldings(holdings []HoldingPosition, scored []scoredItem, params styleParams) []PlanAction {
	consensusMap := make(map[string]scoredItem)
	for _, s := range scored {
		consensusMap[s.Symbol] = s
	}

	var actions []PlanAction
	for _, h := range holdings {
		ci, hasConsensus := consensusMap[h.Symbol]

		// Mandatory stop-loss check.
		if h.PnLPct <= -params.DefaultStopLoss*100 {
			actions = append(actions, PlanAction{
				Symbol:     h.Symbol,
				Action:     "sell",
				Quantity:   h.Quantity,
				Price:      h.MarketPrice,
				Amount:     float64(h.Quantity) * h.MarketPrice,
				StopLoss:   0,
				TakeProfit: 0,
				Reasoning:  fmt.Sprintf("Stop-loss triggered: position down %.1f%% exceeds %.1f%% limit", h.PnLPct, params.DefaultStopLoss*100),
				Confidence: 95,
			})
			continue
		}

		// Take-profit check.
		if h.PnLPct >= params.DefaultTakeProfit*100 {
			reduceQty := normalizeSellQty(h.Symbol, int(math.Ceil(float64(h.Quantity)*0.5)), h.Quantity)
			if reduceQty <= 0 {
				// Position too small to split per A-share lot rules;
				// fall through to a full sell instead of producing an
				// illegal odd-lot reduce.
				reduceQty = h.Quantity
			}
			actions = append(actions, PlanAction{
				Symbol:     h.Symbol,
				Action:     "reduce",
				Quantity:   reduceQty,
				Price:      h.MarketPrice,
				Amount:     float64(reduceQty) * h.MarketPrice,
				StopLoss:   h.AvgCost, // raise stop to breakeven
				TakeProfit: h.MarketPrice * (1 + params.DefaultTakeProfit),
				Reasoning:  fmt.Sprintf("Take-profit: position up %.1f%%, reducing 50%% and trailing stop to cost", h.PnLPct),
				Confidence: 85,
			})
			continue
		}

		if hasConsensus {
			switch {
			case ci.Direction == "bearish" && ci.Confidence >= params.MinConfidence:
				actions = append(actions, PlanAction{
					Symbol:      h.Symbol,
					Action:      "sell",
					Quantity:    h.Quantity,
					Price:       h.MarketPrice,
					Amount:      float64(h.Quantity) * h.MarketPrice,
					StopLoss:    0,
					TakeProfit:  0,
					Reasoning:   fmt.Sprintf("Consensus bearish (%d%% confidence): %s", ci.Confidence, ci.Reasoning),
					Confidence:  ci.Confidence,
					SupportedBy: ci.Supporters,
					OpposedBy:   ci.Dissenters,
				})
			case ci.Direction == "neutral":
				actions = append(actions, PlanAction{
					Symbol:      h.Symbol,
					Action:      "hold",
					Quantity:    h.Quantity,
					Price:       h.MarketPrice,
					Amount:      0,
					StopLoss:    h.MarketPrice * (1 - params.DefaultStopLoss),
					TakeProfit:  h.MarketPrice * (1 + params.DefaultTakeProfit),
					Reasoning:   fmt.Sprintf("Consensus neutral — maintain position with protective stops"),
					Confidence:  ci.Confidence,
					SupportedBy: ci.Supporters,
					OpposedBy:   ci.Dissenters,
				})
			default:
				// Bullish consensus — hold.
				actions = append(actions, PlanAction{
					Symbol:      h.Symbol,
					Action:      "hold",
					Quantity:    h.Quantity,
					Price:       h.MarketPrice,
					Amount:      0,
					StopLoss:    h.MarketPrice * (1 - params.DefaultStopLoss),
					TakeProfit:  h.MarketPrice * (1 + params.DefaultTakeProfit),
					Reasoning:   fmt.Sprintf("Consensus bullish (%d%% confidence) — hold and update stops", ci.Confidence),
					Confidence:  ci.Confidence,
					SupportedBy: ci.Supporters,
					OpposedBy:   ci.Dissenters,
				})
			}
		} else {
			// No consensus opinion — hold with tighter stops.
			actions = append(actions, PlanAction{
				Symbol:     h.Symbol,
				Action:     "hold",
				Quantity:   h.Quantity,
				Price:      h.MarketPrice,
				Amount:     0,
				StopLoss:   h.MarketPrice * (1 - params.DefaultStopLoss*0.7),
				TakeProfit: h.MarketPrice * (1 + params.DefaultTakeProfit*0.7),
				Reasoning:  "No consensus coverage — hold with tighter protective stops",
				Confidence: 50,
			})
		}
	}
	return actions
}

// ---------------------------------------------------------------------------
// Buy / add generation
// ---------------------------------------------------------------------------

// generateBuyActions creates buy or add actions for the highest-scored
// consensus items that are not already being sold.
func (pm *PMAgent) generateBuyActions(
	scored []scoredItem,
	holdings map[string]HoldingPosition,
	params styleParams,
	totalAssets, availableCash, currentExposure float64,
	rules []RiskRule,
) []PlanAction {
	if totalAssets <= 0 {
		return nil
	}

	var actions []PlanAction
	remainingCash := availableCash
	exposure := currentExposure

	for _, si := range scored {
		if si.Action != "buy" && si.Direction != "bullish" {
			continue
		}
		if remainingCash <= 0 {
			break
		}
		if exposure >= params.MaxTotalExposure {
			break
		}

		// Determine base position size as a fraction of total assets.
		baseFraction := pm.basePositionSize(si, params, totalAssets)

		// Check single-position cap.
		existing, alreadyHeld := holdings[si.Symbol]
		existingWeight := 0.0
		if alreadyHeld {
			existingWeight = existing.Weight
		}
		maxAdditional := params.MaxSinglePosition - existingWeight
		if maxAdditional <= 0 {
			continue // already at max weight
		}
		if baseFraction > maxAdditional {
			baseFraction = maxAdditional
		}

		// Check that we do not exceed total exposure cap.
		if exposure+baseFraction > params.MaxTotalExposure {
			baseFraction = params.MaxTotalExposure - exposure
		}

		amount := baseFraction * totalAssets
		if amount > remainingCash {
			amount = remainingCash
		}
		if amount < 100 { // minimum trade size
			continue
		}

		// Apply per-rule limits.
		amount = pm.applyRuleLimits(si.Symbol, amount, totalAssets, rules, holdings)
		if amount < 100 {
			continue
		}

		// We lack real-time price data here; use a placeholder price of 0
		// to signal that the trading engine should use market price. If the
		// consensus reasoning embeds a price target, the LLM reasoning step
		// can refine it.
		price := 0.0 // market order
		qty := 0
		if price > 0 {
			qty = normalizeBuyQty(si.Symbol, int(math.Floor(amount/price)))
		}

		action := "buy"
		if alreadyHeld {
			action = "add"
		}

		actions = append(actions, PlanAction{
			Symbol:      si.Symbol,
			Action:      action,
			Quantity:    qty,
			Price:       price,
			Amount:      amount,
			StopLoss:    0, // will be set after price is known
			TakeProfit:  0,
			Reasoning:   fmt.Sprintf("Score %.2f | %s consensus (%d%% conf): %s", si.score, si.Direction, si.Confidence, si.Reasoning),
			Confidence:  si.Confidence,
			SupportedBy: si.Supporters,
			OpposedBy:   si.Dissenters,
		})

		remainingCash -= amount
		exposure += amount / totalAssets
	}

	return actions
}

// basePositionSize computes the target allocation fraction for a single
// consensus item based on its score, confidence, and the PM's style.
func (pm *PMAgent) basePositionSize(si scoredItem, params styleParams, totalAssets float64) float64 {
	// Base: 5% of total assets scaled by style and score.
	base := 0.05 * params.PositionScale

	// Scale linearly with confidence (50-100 maps to 0.5-1.0 multiplier).
	confMultiplier := 0.5 + 0.5*(float64(si.Confidence)/100.0)

	// Scale with score (already 0-1 range approximately).
	scoreMultiplier := math.Min(si.score, 1.0)

	size := base * confMultiplier * (0.5 + 0.5*scoreMultiplier)

	// Clamp to single-position limit.
	if size > params.MaxSinglePosition {
		size = params.MaxSinglePosition
	}
	return size
}

// applyRuleLimits adjusts the buy amount downward to satisfy any applicable
// risk rules (e.g. sector limits, max single position as an absolute number).
//
// holdings is used to derive the current sector exposure when a sector_limit
// rule fires - the rule cap is interpreted as a fraction of totalAssets.
func (pm *PMAgent) applyRuleLimits(symbol string, amount, totalAssets float64, rules []RiskRule, holdings map[string]HoldingPosition) float64 {
	for _, r := range rules {
		switch r.Type {
		case "max_position":
			maxAmt := r.Limit * totalAssets
			if amount > maxAmt {
				amount = maxAmt
			}
		case "max_single_trade":
			if amount > r.Limit {
				amount = r.Limit
			}
		case "sector_limit":
			// r.Param holds the sector name this rule applies to. We
			// shrink amount so that the post-trade exposure to that
			// sector stays within r.Limit * totalAssets. If we cannot
			// determine the buy symbol's sector we conservatively skip
			// (matching the previous behaviour) and log for observability.
			sector := holdings[symbol].Sector
			if sector == "" || !strings.EqualFold(sector, r.Param) {
				pm.logger.Debug("sector_limit rule skipped: symbol sector unknown or different", "rule", r.ID, "symbol", symbol, "ruleSector", r.Param)
				continue
			}
			cap := r.Limit * totalAssets
			currentExposure := 0.0
			for _, h := range holdings {
				if strings.EqualFold(h.Sector, sector) {
					currentExposure += h.MarketValue
				}
			}
			headroom := cap - currentExposure
			if headroom <= 0 {
				pm.logger.Debug("sector_limit rule clamped buy to zero", "rule", r.ID, "symbol", symbol, "sector", sector, "exposure", currentExposure, "cap", cap)
				return 0
			}
			if amount > headroom {
				pm.logger.Debug("sector_limit rule clamped buy", "rule", r.ID, "symbol", symbol, "sector", sector, "from", amount, "to", headroom)
				amount = headroom
			}
		}
	}
	return amount
}

// ---------------------------------------------------------------------------
// Exposure helpers
// ---------------------------------------------------------------------------

func buildHoldingsMap(holdings []HoldingPosition) map[string]HoldingPosition {
	m := make(map[string]HoldingPosition, len(holdings))
	for _, h := range holdings {
		m[h.Symbol] = h
	}
	return m
}

// currentExposure returns the fraction of total assets currently invested
// after accounting for sell actions generated in this plan cycle.
func (pm *PMAgent) currentExposure(holdings []HoldingPosition, sellActions []PlanAction, totalAssets float64) float64 {
	if totalAssets <= 0 {
		return 0
	}

	sellSet := make(map[string]float64) // symbol -> amount freed
	for _, a := range sellActions {
		if a.Action == "sell" || a.Action == "reduce" {
			sellSet[a.Symbol] += a.Amount
		}
	}

	invested := 0.0
	for _, h := range holdings {
		val := h.MarketValue
		if freed, ok := sellSet[h.Symbol]; ok {
			val -= freed
		}
		if val > 0 {
			invested += val
		}
	}

	return invested / totalAssets
}

// ---------------------------------------------------------------------------
// Risk-rule enforcement (post-generation pass)
// ---------------------------------------------------------------------------

// enforceRiskRules performs a final compliance pass over all actions. It
// trims or removes actions that would violate hard risk limits.
func (pm *PMAgent) enforceRiskRules(actions []PlanAction, holdings []HoldingPosition, totalAssets float64, rules []RiskRule, params styleParams) []PlanAction {
	if len(rules) == 0 && totalAssets <= 0 {
		return actions
	}

	// Hard caps from PRD: single position ≤30%, total invested ≤95%.
	const hardMaxSingle = 0.30
	const hardMaxTotal = 0.95

	holdingValue := make(map[string]float64)
	for _, h := range holdings {
		holdingValue[h.Symbol] = h.MarketValue
	}

	totalBuyAmount := 0.0
	for i := range actions {
		if actions[i].Action == "buy" || actions[i].Action == "add" {
			totalBuyAmount += actions[i].Amount
		}
	}

	totalInvested := 0.0
	for _, h := range holdings {
		totalInvested += h.MarketValue
	}
	// Add planned buys, subtract planned sells.
	for _, a := range actions {
		switch a.Action {
		case "buy", "add":
			totalInvested += a.Amount
		case "sell":
			totalInvested -= a.Amount
		case "reduce":
			totalInvested -= a.Amount
		}
	}

	// Enforce total exposure cap.
	if totalAssets > 0 && totalInvested/totalAssets > hardMaxTotal {
		excess := totalInvested - hardMaxTotal*totalAssets
		actions = pm.trimBuyActions(actions, excess)
	}

	// Enforce single-position caps.
	for i := range actions {
		a := &actions[i]
		if a.Action != "buy" && a.Action != "add" {
			continue
		}
		existingValue := holdingValue[a.Symbol]
		projectedValue := existingValue + a.Amount
		maxValue := hardMaxSingle * totalAssets
		if totalAssets > 0 && projectedValue > maxValue {
			allowed := maxValue - existingValue
			if allowed < 0 {
				allowed = 0
			}
			a.Amount = allowed
			if a.Price > 0 {
				a.Quantity = normalizeBuyQty(a.Symbol, int(math.Floor(a.Amount/a.Price)))
				a.Amount = float64(a.Quantity) * a.Price
			}
			a.Reasoning += " [trimmed: single-position cap]"
		}
	}

	// Apply custom risk rules.
	for _, r := range rules {
		switch r.Type {
		case "max_drawdown":
			// max_drawdown rules affect stop-loss levels rather than sizes.
			for i := range actions {
				a := &actions[i]
				if a.Price > 0 && a.StopLoss == 0 {
					a.StopLoss = a.Price * (1 - r.Limit)
				}
			}
		case "max_position":
			limit := r.Limit
			if limit > hardMaxSingle {
				limit = hardMaxSingle
			}
			for i := range actions {
				a := &actions[i]
				if a.Action != "buy" && a.Action != "add" {
					continue
				}
				maxValue := limit * totalAssets
				existingValue := holdingValue[a.Symbol]
				if existingValue+a.Amount > maxValue {
					a.Amount = math.Max(0, maxValue-existingValue)
					if a.Price > 0 {
						a.Quantity = normalizeBuyQty(a.Symbol, int(math.Floor(a.Amount/a.Price)))
						a.Amount = float64(a.Quantity) * a.Price
					}
					a.Reasoning += fmt.Sprintf(" [trimmed: risk rule %s]", r.ID)
				}
			}
		}
	}

	// Remove zero-amount buy/add actions.
	filtered := actions[:0]
	for _, a := range actions {
		if (a.Action == "buy" || a.Action == "add") && a.Amount < 1 {
			continue
		}
		filtered = append(filtered, a)
	}

	return filtered
}

// trimBuyActions reduces buy/add amounts in reverse-priority order (lowest
// confidence first) until `excess` dollars have been removed.
func (pm *PMAgent) trimBuyActions(actions []PlanAction, excess float64) []PlanAction {
	if excess <= 0 {
		return actions
	}

	// Build index of buy actions sorted by confidence ascending.
	type indexed struct {
		idx  int
		conf int
	}
	var buys []indexed
	for i, a := range actions {
		if a.Action == "buy" || a.Action == "add" {
			buys = append(buys, indexed{idx: i, conf: a.Confidence})
		}
	}
	sort.Slice(buys, func(i, j int) bool { return buys[i].conf < buys[j].conf })

	remaining := excess
	for _, b := range buys {
		if remaining <= 0 {
			break
		}
		a := &actions[b.idx]
		if a.Amount <= remaining {
			remaining -= a.Amount
			a.Amount = 0
		} else {
			a.Amount -= remaining
			remaining = 0
		}
		if a.Price > 0 {
			a.Quantity = normalizeBuyQty(a.Symbol, int(math.Floor(a.Amount/a.Price)))
			a.Amount = float64(a.Quantity) * a.Price
		}
	}

	return actions
}

// normalizeBuyQty applies A-share lot-size rules to a raw buy quantity.
// It is a thin wrapper around instrument.NormalizeBuyQty that converts the
// int<->float boundary at the PM-agent layer (which still uses int qtys).
// Non-A-share symbols pass through unchanged.
func normalizeBuyQty(symbol string, rawQty int) int {
	if rawQty <= 0 {
		return 0
	}
	return int(instrument.NormalizeBuyQty(symbol, instrument.Hint{}, float64(rawQty)))
}

// normalizeSellQty applies A-share lot-size rules to a raw sell quantity,
// expanding partial sells whose residual would be an odd lot.
func normalizeSellQty(symbol string, rawQty, holdingQty int) int {
	if rawQty <= 0 || holdingQty <= 0 {
		return 0
	}
	return int(instrument.NormalizeSellQty(symbol, instrument.Hint{}, float64(rawQty), float64(holdingQty)))
}

// ---------------------------------------------------------------------------
// LLM reasoning
// ---------------------------------------------------------------------------

// llmReasoning calls the LLM to produce a human-readable rationale for the
// plan. If the LLM is unavailable, it falls back to a deterministic summary.
func (pm *PMAgent) llmReasoning(ctx context.Context, input PlanInput, actions []PlanAction, style InvestmentStyle) (reasoning string, riskScore float64, expectedReturn float64) {
	// Compute deterministic risk score and expected return first.
	riskScore = pm.computeRiskScore(actions, input.TotalAssets)
	expectedReturn = pm.computeExpectedReturn(actions, input.TotalAssets)

	systemPrompt := pm.buildSystemPrompt(style)
	userPrompt := pm.buildPlanPrompt(input, actions, riskScore, expectedReturn)

	llmResponse, err := pm.llm.Complete(ctx, systemPrompt, userPrompt)
	if err != nil {
		pm.logger.Warn("LLM reasoning failed, using deterministic summary", "err", err)
		reasoning = pm.deterministicReasoning(input, actions, style)
		return reasoning, riskScore, expectedReturn
	}

	// Try to parse structured LLM output.
	parsed := pm.parseLLMResponse(llmResponse)
	if parsed.Reasoning != "" {
		reasoning = parsed.Reasoning
	} else {
		reasoning = llmResponse
	}
	if parsed.RiskScore > 0 {
		riskScore = parsed.RiskScore
	}
	if parsed.ExpectedReturn != 0 {
		expectedReturn = parsed.ExpectedReturn
	}

	return reasoning, riskScore, expectedReturn
}

// llmPlanOutput is the expected structured response from the LLM.
type llmPlanOutput struct {
	Reasoning      string  `json:"reasoning"`
	RiskScore      float64 `json:"risk_score"`
	ExpectedReturn float64 `json:"expected_return"`
}

func (pm *PMAgent) parseLLMResponse(raw string) llmPlanOutput {
	// Try to extract JSON from the response.
	raw = strings.TrimSpace(raw)

	// Handle markdown code fences.
	if idx := strings.Index(raw, "```json"); idx >= 0 {
		raw = raw[idx+7:]
		if end := strings.Index(raw, "```"); end >= 0 {
			raw = raw[:end]
		}
	} else if idx := strings.Index(raw, "```"); idx >= 0 {
		raw = raw[idx+3:]
		if end := strings.Index(raw, "```"); end >= 0 {
			raw = raw[:end]
		}
	}

	// Try to find a JSON object.
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start >= 0 && end > start {
		raw = raw[start : end+1]
	}

	var out llmPlanOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return llmPlanOutput{}
	}
	return out
}

func (pm *PMAgent) buildSystemPrompt(style InvestmentStyle) string {
	return fmt.Sprintf(`You are the Portfolio Manager of an AI-driven investment fund.
Your investment style is: %s.

Style guidelines:
- conservative: prefer blue-chip stocks, lower position sizes, strict stop-losses, capital preservation first
- balanced: diversified allocation across sectors, moderate risk tolerance
- aggressive: concentrated positions in high-conviction ideas, wider stop-losses, growth-oriented
- value: focus on undervalued stocks with low P/E ratios, high dividend yields, margin of safety
- growth: target high-growth sectors, accept higher valuations for strong revenue/earnings growth

You must respond with a JSON object containing:
{
  "reasoning": "your detailed investment rationale (2-4 paragraphs)",
  "risk_score": <0.0-1.0 where 1.0 is highest risk>,
  "expected_return": <expected daily return as a decimal, e.g. 0.005 for 0.5%%>
}`, string(style))
}

func (pm *PMAgent) buildPlanPrompt(input PlanInput, actions []PlanAction, riskScore, expectedReturn float64) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "Trading Date: %s\n", input.TradingDate)
	fmt.Fprintf(&sb, "Fund: %s\n", input.FundID)
	fmt.Fprintf(&sb, "Available Cash: $%.2f\n", input.AvailableCash)
	fmt.Fprintf(&sb, "Total Assets: $%.2f\n\n", input.TotalAssets)

	sb.WriteString("## Current Holdings\n")
	for _, h := range firstHoldings(input.Holdings, maxPromptHoldings) {
		fmt.Fprintf(&sb, "- %s: %d shares @ $%.2f (P&L: %.1f%%)\n", h.Symbol, h.Quantity, h.AvgCost, h.PnLPct)
	}
	if omitted := omittedCount(len(input.Holdings), maxPromptHoldings); omitted > 0 {
		fmt.Fprintf(&sb, "- ... %d holdings omitted for prompt budget\n", omitted)
	}

	sb.WriteString("\n## Research Consensus\n")
	for _, c := range firstConsensus(input.Consensus, maxPromptConsensus) {
		fmt.Fprintf(&sb, "- %s: %s (%d%% confidence, action: %s) — %s\n",
			c.Symbol, c.Direction, c.Confidence, c.Action, truncateRunes(c.Reasoning, maxPromptReasoningRunes))
	}
	if omitted := omittedCount(len(input.Consensus), maxPromptConsensus); omitted > 0 {
		fmt.Fprintf(&sb, "- ... %d consensus items omitted for prompt budget\n", omitted)
	}

	sb.WriteString("\n## Proposed Actions\n")
	for _, a := range firstActions(actions, maxPromptActions) {
		fmt.Fprintf(&sb, "- %s %s: qty=%d, amount=$%.2f, reason: %s\n",
			a.Action, a.Symbol, a.Quantity, a.Amount, truncateRunes(a.Reasoning, maxPromptReasoningRunes))
	}
	if omitted := omittedCount(len(actions), maxPromptActions); omitted > 0 {
		fmt.Fprintf(&sb, "- ... %d proposed actions omitted for prompt budget\n", omitted)
	}

	if input.MemoryContext != "" {
		fmt.Fprintf(&sb, "\n## Recent Memory / Lessons\n%s\n", truncateRunes(input.MemoryContext, maxPromptMemoryRunes))
	}

	fmt.Fprintf(&sb, "\n## Preliminary Metrics\nRisk Score: %.2f\nExpected Return: %.4f\n", riskScore, expectedReturn)
	sb.WriteString("\nPlease provide your investment reasoning and final risk assessment.")

	return sb.String()
}

func truncateRunes(s string, max int) string {
	trimmed := strings.TrimSpace(s)
	if max <= 0 || trimmed == "" {
		return trimmed
	}
	runes := []rune(trimmed)
	if len(runes) <= max {
		return trimmed
	}
	return string(runes[:max]) + "\n...[truncated]"
}

func omittedCount(total, max int) int {
	if max <= 0 || total <= max {
		return 0
	}
	return total - max
}

func firstHoldings(items []HoldingPosition, max int) []HoldingPosition {
	if max <= 0 || len(items) <= max {
		return items
	}
	return items[:max]
}

func firstConsensus(items []ConsensusItem, max int) []ConsensusItem {
	if max <= 0 || len(items) <= max {
		return items
	}
	return items[:max]
}

func firstActions(items []PlanAction, max int) []PlanAction {
	if max <= 0 || len(items) <= max {
		return items
	}
	return items[:max]
}

func firstStrings(items []string, max int) []string {
	if max <= 0 || len(items) <= max {
		return items
	}
	return items[:max]
}

func firstExecutionResults(items []ExecutionResult, max int) []ExecutionResult {
	if max <= 0 || len(items) <= max {
		return items
	}
	return items[:max]
}

// deterministicReasoning produces a fallback summary when the LLM is not
// available or fails.
func (pm *PMAgent) deterministicReasoning(input PlanInput, actions []PlanAction, style InvestmentStyle) string {
	buys, sells, holds := 0, 0, 0
	totalBuyAmt, totalSellAmt := 0.0, 0.0
	for _, a := range actions {
		switch a.Action {
		case "buy", "add":
			buys++
			totalBuyAmt += a.Amount
		case "sell", "reduce":
			sells++
			totalSellAmt += a.Amount
		case "hold":
			holds++
		}
	}

	return fmt.Sprintf(
		"[%s style] Plan for %s: %d buy/add ($%.0f), %d sell/reduce ($%.0f), %d hold. "+
			"Based on %d consensus items from roundtable with %d research inputs. "+
			"Available cash: $%.0f of $%.0f total assets (%.1f%% cash ratio).",
		string(style), input.TradingDate,
		buys, totalBuyAmt,
		sells, totalSellAmt,
		holds,
		len(input.Consensus), len(input.ResearchResults),
		input.AvailableCash, input.TotalAssets,
		(input.AvailableCash/math.Max(input.TotalAssets, 1))*100,
	)
}

// ---------------------------------------------------------------------------
// Quantitative helpers
// ---------------------------------------------------------------------------

// computeRiskScore produces a 0-1 risk score based on portfolio
// concentration, exposure level, and action aggressiveness.
func (pm *PMAgent) computeRiskScore(actions []PlanAction, totalAssets float64) float64 {
	if totalAssets <= 0 || len(actions) == 0 {
		return 0
	}

	// Factor 1: concentration — what fraction of assets goes to the largest
	// single action?
	maxAmount := 0.0
	totalAmount := 0.0
	for _, a := range actions {
		if a.Action == "buy" || a.Action == "add" {
			if a.Amount > maxAmount {
				maxAmount = a.Amount
			}
			totalAmount += a.Amount
		}
	}
	concentration := maxAmount / totalAssets

	// Factor 2: exposure — total new capital deployed.
	exposure := totalAmount / totalAssets

	// Factor 3: average confidence (inverse — lower confidence = higher risk).
	totalConf := 0
	n := 0
	for _, a := range actions {
		if a.Action == "buy" || a.Action == "add" {
			totalConf += a.Confidence
			n++
		}
	}
	avgConf := 80.0
	if n > 0 {
		avgConf = float64(totalConf) / float64(n)
	}
	confRisk := 1.0 - avgConf/100.0

	// Weighted combination.
	score := 0.35*concentration + 0.35*exposure + 0.30*confRisk
	return math.Min(math.Max(score, 0), 1)
}

// computeExpectedReturn estimates the portfolio's expected daily return based
// on action confidence and style aggressiveness.
func (pm *PMAgent) computeExpectedReturn(actions []PlanAction, totalAssets float64) float64 {
	if totalAssets <= 0 || len(actions) == 0 {
		return 0
	}

	weightedReturn := 0.0
	totalWeight := 0.0
	for _, a := range actions {
		if a.Action == "buy" || a.Action == "add" {
			weight := a.Amount / totalAssets
			// Heuristic: higher confidence items have higher expected return.
			// Base daily expected return: 0.1%-0.5% scaled by confidence.
			dailyReturn := 0.001 + 0.004*(float64(a.Confidence)/100.0)
			weightedReturn += weight * dailyReturn
			totalWeight += weight
		}
	}

	if totalWeight == 0 {
		return 0
	}
	return weightedReturn
}

// ---------------------------------------------------------------------------
// ReviewExecution — end-of-day self-assessment
// ---------------------------------------------------------------------------

// ReviewExecution produces a DailyReview by comparing the plan's intended
// actions with actual execution results and current P&L. It uses the LLM for
// qualitative lessons when available.
func (pm *PMAgent) ReviewExecution(ctx context.Context, plan *InvestmentPlan, execResults []ExecutionResult) (*DailyReview, error) {
	if plan == nil {
		return nil, fmt.Errorf("cannot review nil plan")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("context cancelled before review: %w", err)
	}

	pm.logger.Info("generating daily review", "plan_id", plan.ID, "date", plan.Date)

	// Compare planned vs actual.
	execMap := make(map[string]ExecutionResult)
	for _, er := range execResults {
		execMap[er.Symbol] = er
	}

	var hits, misses []string
	var totalPnL float64

	for _, a := range plan.Actions {
		er, executed := execMap[a.Symbol]
		if !executed {
			if a.Action == "buy" || a.Action == "add" || a.Action == "sell" || a.Action == "reduce" {
				misses = append(misses, fmt.Sprintf("%s %s: not executed", a.Action, a.Symbol))
			}
			continue
		}

		switch er.Status {
		case "filled":
			if er.FilledQty == er.RequestedQty {
				hits = append(hits, fmt.Sprintf("%s %s: fully filled %d @ $%.2f",
					a.Action, a.Symbol, er.FilledQty, er.AvgFillPrice))
			} else {
				misses = append(misses, fmt.Sprintf("%s %s: partial fill %d/%d @ $%.2f",
					a.Action, a.Symbol, er.FilledQty, er.RequestedQty, er.AvgFillPrice))
			}
		case "partial":
			misses = append(misses, fmt.Sprintf("%s %s: partial fill %d/%d",
				a.Action, a.Symbol, er.FilledQty, er.RequestedQty))
		case "rejected":
			misses = append(misses, fmt.Sprintf("%s %s: rejected — %s",
				a.Action, a.Symbol, er.Error))
		}

		// Estimate P&L for fills.
		if a.Action == "buy" || a.Action == "add" {
			// P&L will be realized later; for now record commission cost.
			totalPnL -= er.Commission
		} else if a.Action == "sell" || a.Action == "reduce" {
			totalPnL += float64(er.FilledQty)*er.AvgFillPrice - er.Commission
		}
	}

	// Use LLM for lessons learned.
	lessons := pm.generateLessons(ctx, plan, execResults, hits, misses)

	review := &DailyReview{
		FundID:    plan.FundID,
		Date:      plan.Date,
		PlanID:    plan.ID,
		Hits:      hits,
		Misses:    misses,
		Lessons:   lessons,
		PnLToday:  totalPnL,
		CreatedAt: time.Now(),
	}

	// Generate summary.
	review.Summary = pm.buildReviewSummary(review)

	pm.logger.Info("daily review complete",
		"plan_id", plan.ID,
		"hits", len(hits),
		"misses", len(misses),
		"lessons", len(lessons),
	)

	return review, nil
}

// generateLessons uses the LLM to extract actionable lessons from the day's
// trading. Falls back to deterministic output on failure.
func (pm *PMAgent) generateLessons(ctx context.Context, plan *InvestmentPlan, results []ExecutionResult, hits, misses []string) []string {
	systemPrompt := `You are a portfolio manager conducting an end-of-day review.
Analyse the plan vs execution results and extract 2-5 concise, actionable lessons.
Respond with a JSON array of strings, e.g. ["lesson 1", "lesson 2"].`

	var sb strings.Builder
	fmt.Fprintf(&sb, "Plan ID: %s, Date: %s\n", plan.ID, plan.Date)
	fmt.Fprintf(&sb, "Plan reasoning: %s\n\n", truncateRunes(plan.Reasoning, maxReviewReasoningRunes))

	sb.WriteString("Hits:\n")
	for _, h := range firstStrings(hits, maxReviewItems) {
		fmt.Fprintf(&sb, "  - %s\n", truncateRunes(h, maxReviewLineRunes))
	}
	if omitted := omittedCount(len(hits), maxReviewItems); omitted > 0 {
		fmt.Fprintf(&sb, "  - ... %d hits omitted for prompt budget\n", omitted)
	}
	sb.WriteString("Misses:\n")
	for _, m := range firstStrings(misses, maxReviewItems) {
		fmt.Fprintf(&sb, "  - %s\n", truncateRunes(m, maxReviewLineRunes))
	}
	if omitted := omittedCount(len(misses), maxReviewItems); omitted > 0 {
		fmt.Fprintf(&sb, "  - ... %d misses omitted for prompt budget\n", omitted)
	}

	sb.WriteString("\nExecution results:\n")
	for _, er := range firstExecutionResults(results, maxReviewItems) {
		fmt.Fprintf(&sb, "  - %s %s: status=%s, filled=%d/%d, price=$%.2f\n",
			er.Action, er.Symbol, er.Status, er.FilledQty, er.RequestedQty, er.AvgFillPrice)
	}
	if omitted := omittedCount(len(results), maxReviewItems); omitted > 0 {
		fmt.Fprintf(&sb, "  - ... %d execution results omitted for prompt budget\n", omitted)
	}

	resp, err := pm.llm.Complete(ctx, systemPrompt, sb.String())
	if err != nil {
		pm.logger.Warn("LLM lesson generation failed, using defaults", "err", err)
		return pm.deterministicLessons(hits, misses)
	}

	var lessons []string
	// Try to parse JSON array from response.
	cleaned := strings.TrimSpace(resp)
	if idx := strings.Index(cleaned, "["); idx >= 0 {
		if end := strings.LastIndex(cleaned, "]"); end > idx {
			cleaned = cleaned[idx : end+1]
		}
	}
	if err := json.Unmarshal([]byte(cleaned), &lessons); err != nil {
		pm.logger.Warn("failed to parse LLM lessons, using raw response", "err", err)
		// Split by newlines as fallback.
		for _, line := range strings.Split(resp, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && len(line) > 10 {
				lessons = append(lessons, line)
			}
		}
	}

	if len(lessons) == 0 {
		return pm.deterministicLessons(hits, misses)
	}
	if len(lessons) > maxLessonsReturned {
		lessons = lessons[:maxLessonsReturned]
	}
	for i := range lessons {
		lessons[i] = truncateRunes(lessons[i], maxLessonRunes)
	}
	return lessons
}

// deterministicLessons produces basic lessons without LLM assistance.
func (pm *PMAgent) deterministicLessons(hits, misses []string) []string {
	var lessons []string
	if len(hits) > len(misses) {
		lessons = append(lessons, "Majority of planned actions executed successfully — plan quality is adequate.")
	}
	if len(misses) > 0 {
		lessons = append(lessons, fmt.Sprintf("%d actions missed or partially filled — review order sizing and timing.", len(misses)))
	}
	if len(hits) == 0 && len(misses) == 0 {
		lessons = append(lessons, "No actions were executed today — consider whether the plan was too conservative.")
	}
	lessons = append(lessons, "Continue monitoring positions with active stop-loss and take-profit levels.")
	return lessons
}

// buildReviewSummary composes a human-readable summary paragraph for the
// daily review.
func (pm *PMAgent) buildReviewSummary(review *DailyReview) string {
	return fmt.Sprintf(
		"Daily review for %s (plan %s): %d successful executions, %d issues. "+
			"Estimated P&L impact: $%.2f. %d lessons captured for memory system.",
		review.Date, review.PlanID,
		len(review.Hits), len(review.Misses),
		review.PnLToday,
		len(review.Lessons),
	)
}
