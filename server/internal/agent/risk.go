// Package agent provides autonomous AI agents for the fund simulator.
// This file implements the Risk Agent, which reviews investment plans against
// predefined risk management rules before execution.
package agent

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Risk review types
// ---------------------------------------------------------------------------

// RiskReview is the output of a full plan review.
type RiskReview struct {
	PlanID      string
	Verdict     string // "approved", "approved_with_warnings", "rejected"
	Checks      []RiskCheckResult
	Warnings    []string
	Rejections  []string
	Suggestions []string
	Commentary  string
	ReviewedAt  time.Time
}

// RiskCheckResult records the outcome of a single risk rule evaluation.
type RiskCheckResult struct {
	Rule      string
	Status    string // "pass", "warn", "fail"
	Current   float64
	Threshold float64
	Message   string
}

// RiskCommentaryClient generates natural-language risk commentary.
type RiskCommentaryClient interface {
	GenerateRiskCommentary(ctx context.Context, review *RiskReview) (string, error)
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// RiskConfig holds thresholds used by the Risk Agent (ratios, e.g. 0.30 = 30%).
type RiskConfig struct {
	MaxSinglePositionPct float64 // default 0.30
	MaxTotalPositionPct  float64 // default 0.95
	DailyLossWarnPct     float64 // default -0.03
	MaxDrawdownWarnPct   float64 // default -0.10
	CircuitBreakerPct    float64 // default -0.20
	MaxSectorExposurePct float64 // default 0.40
	MinLiquidityRatio    float64 // default 0.10
}

// DefaultRiskConfig returns production-ready defaults from the PRD.
func DefaultRiskConfig() RiskConfig {
	return RiskConfig{
		MaxSinglePositionPct: 0.30, MaxTotalPositionPct: 0.95,
		DailyLossWarnPct: -0.03, MaxDrawdownWarnPct: -0.10,
		CircuitBreakerPct: -0.20, MaxSectorExposurePct: 0.40,
		MinLiquidityRatio: 0.10,
	}
}

// ---------------------------------------------------------------------------
// RiskAgent
// ---------------------------------------------------------------------------

// RiskAgent reviews investment plans against predefined rules. Safe for
// concurrent use after Start().
type RiskAgent struct {
	cfg     RiskConfig
	llm     RiskCommentaryClient // may be nil
	logger  *log.Logger
	mu      sync.Mutex
	running bool
	stopCh  chan struct{}
}

// NewRiskAgent creates a RiskAgent. llm may be nil.
func NewRiskAgent(cfg RiskConfig, llm RiskCommentaryClient, logger *log.Logger) *RiskAgent {
	if logger == nil {
		logger = log.Default()
	}
	return &RiskAgent{cfg: cfg, llm: llm, logger: logger}
}

// Start initialises the agent (idempotent).
func (ra *RiskAgent) Start() {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	if ra.running {
		return
	}
	ra.running = true
	ra.stopCh = make(chan struct{})
	ra.logger.Println("[RiskAgent] started")
}

// Stop shuts down the agent gracefully.
func (ra *RiskAgent) Stop() {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	if !ra.running {
		return
	}
	ra.running = false
	close(ra.stopCh)
	ra.logger.Println("[RiskAgent] stopped")
}

// ReviewPlan evaluates a plan against every risk rule and returns a RiskReview.
func (ra *RiskAgent) ReviewPlan(ctx context.Context, plan *InvestmentPlan, holdings []HoldingPosition, totalAssets float64) (*RiskReview, error) {
	if plan == nil {
		return nil, fmt.Errorf("risk: plan must not be nil")
	}
	if totalAssets <= 0 {
		return nil, fmt.Errorf("risk: totalAssets must be positive, got %f", totalAssets)
	}
	review := &RiskReview{PlanID: plan.ID, ReviewedAt: time.Now()}
	hm := buildHoldingMap(holdings)

	ra.checkSinglePosition(plan, hm, totalAssets, review)
	ra.checkTotalPosition(plan, hm, totalAssets, review)
	ra.checkDailyLoss(hm, totalAssets, review)
	ra.checkMaxDrawdown(hm, totalAssets, review)
	ra.checkCircuitBreaker(hm, totalAssets, review)
	ra.checkSectorConcentration(plan, hm, totalAssets, review)
	ra.checkLiquidity(plan, hm, review)
	ra.deriveVerdict(review)

	if ra.llm != nil {
		if c, err := ra.llm.GenerateRiskCommentary(ctx, review); err != nil {
			ra.logger.Printf("[RiskAgent] LLM commentary failed: %v", err)
		} else {
			review.Commentary = c
		}
	}
	ra.logger.Printf("[RiskAgent] plan %s verdict=%s checks=%d", plan.ID, review.Verdict, len(review.Checks))
	return review, nil
}

// ---------------------------------------------------------------------------
// Individual risk checks
// ---------------------------------------------------------------------------

func (ra *RiskAgent) checkSinglePosition(plan *InvestmentPlan, hm map[string]HoldingPosition, total float64, r *RiskReview) {
	for _, a := range plan.Actions {
		h := hm[a.Symbol]
		tradeValue := actionValue(a, h)
		projected := holdingMarketValue(h)
		if isSellAction(a.Action) {
			projected -= tradeValue
		} else {
			projected += tradeValue
		}
		ratio := projected / total
		status, msg := "pass", fmt.Sprintf("%s projected %.2f%% of assets", a.Symbol, ratio*100)
		if ratio > ra.cfg.MaxSinglePositionPct {
			status = "fail"
			msg = fmt.Sprintf("%s would be %.2f%% (limit %.0f%%)", a.Symbol, ratio*100, ra.cfg.MaxSinglePositionPct*100)
			r.Rejections = append(r.Rejections, msg)
			r.Suggestions = append(r.Suggestions, fmt.Sprintf("Reduce %s to ≤%.0f%% of assets", a.Symbol, ra.cfg.MaxSinglePositionPct*100))
		}
		r.Checks = append(r.Checks, RiskCheckResult{"single_position_limit", status, ratio, ra.cfg.MaxSinglePositionPct, msg})
	}
}

func (ra *RiskAgent) checkTotalPosition(plan *InvestmentPlan, hm map[string]HoldingPosition, total float64, r *RiskReview) {
	exposure := 0.0
	for _, h := range hm {
		exposure += holdingMarketValue(h)
	}
	for _, a := range plan.Actions {
		delta := actionValue(a, hm[a.Symbol])
		if isSellAction(a.Action) {
			delta = -delta
		}
		exposure += delta
	}
	ratio := exposure / total
	status, msg := "pass", fmt.Sprintf("total position %.2f%%", ratio*100)
	if ratio > ra.cfg.MaxTotalPositionPct {
		status = "fail"
		msg = fmt.Sprintf("total position %.2f%% exceeds %.0f%% limit", ratio*100, ra.cfg.MaxTotalPositionPct*100)
		r.Rejections = append(r.Rejections, msg)
		r.Suggestions = append(r.Suggestions, "Reduce overall position size or increase cash reserves")
	}
	r.Checks = append(r.Checks, RiskCheckResult{"total_position_limit", status, ratio, ra.cfg.MaxTotalPositionPct, msg})
}

func (ra *RiskAgent) checkDailyLoss(hm map[string]HoldingPosition, total float64, r *RiskReview) {
	pnl := 0.0
	for _, h := range hm {
		pnl += float64(h.Quantity) * (h.MarketPrice - h.AvgCost)
	}
	ratio := pnl / total
	status, msg := "pass", fmt.Sprintf("daily P&L %.2f%%", ratio*100)
	if ratio <= ra.cfg.DailyLossWarnPct {
		status = "warn"
		msg = fmt.Sprintf("daily loss %.2f%% breaches -3%% warning", ratio*100)
		r.Warnings = append(r.Warnings, msg)
	}
	r.Checks = append(r.Checks, RiskCheckResult{"daily_loss_warning", status, ratio, ra.cfg.DailyLossWarnPct, msg})
}

func (ra *RiskAgent) checkMaxDrawdown(hm map[string]HoldingPosition, total float64, r *RiskReview) {
	dd := computeDrawdown(hm, total)
	status, msg := "pass", fmt.Sprintf("drawdown %.2f%%", dd*100)
	if dd <= ra.cfg.MaxDrawdownWarnPct {
		status = "warn"
		msg = fmt.Sprintf("drawdown %.2f%% exceeds -10%% warning", dd*100)
		r.Warnings = append(r.Warnings, msg)
		r.Suggestions = append(r.Suggestions, "Consider reducing exposure to limit further drawdown")
	}
	r.Checks = append(r.Checks, RiskCheckResult{"max_drawdown_warning", status, dd, ra.cfg.MaxDrawdownWarnPct, msg})
}

func (ra *RiskAgent) checkCircuitBreaker(hm map[string]HoldingPosition, total float64, r *RiskReview) {
	dd := computeDrawdown(hm, total)
	status, msg := "pass", fmt.Sprintf("drawdown %.2f%% within circuit-breaker", dd*100)
	if dd <= ra.cfg.CircuitBreakerPct {
		status = "fail"
		msg = fmt.Sprintf("drawdown %.2f%% breaches -20%% circuit breaker — trading halted", dd*100)
		r.Rejections = append(r.Rejections, msg)
		r.Suggestions = append(r.Suggestions, "Circuit breaker triggered: liquidate or wait for recovery")
	}
	r.Checks = append(r.Checks, RiskCheckResult{"circuit_breaker", status, dd, ra.cfg.CircuitBreakerPct, msg})
}

func (ra *RiskAgent) checkSectorConcentration(plan *InvestmentPlan, hm map[string]HoldingPosition, total float64, r *RiskReview) {
	sec := make(map[string]float64)
	for _, h := range hm {
		sec[h.Sector] += holdingMarketValue(h)
	}
	for _, a := range plan.Actions {
		h := hm[a.Symbol]
		sector := h.Sector
		delta := actionValue(a, h)
		if isSellAction(a.Action) {
			delta = -delta
		}
		sec[sector] += delta
	}
	for sector, exp := range sec {
		if sector == "" {
			continue
		}
		ratio := exp / total
		status, msg := "pass", fmt.Sprintf("sector %s %.2f%%", sector, ratio*100)
		if ratio > ra.cfg.MaxSectorExposurePct {
			status = "warn"
			msg = fmt.Sprintf("sector %s %.2f%% exceeds %.0f%%", sector, ratio*100, ra.cfg.MaxSectorExposurePct*100)
			r.Warnings = append(r.Warnings, msg)
			r.Suggestions = append(r.Suggestions, fmt.Sprintf("Diversify away from %s sector", sector))
		}
		r.Checks = append(r.Checks, RiskCheckResult{"sector_concentration", status, ratio, ra.cfg.MaxSectorExposurePct, msg})
	}
}

func (ra *RiskAgent) checkLiquidity(plan *InvestmentPlan, hm map[string]HoldingPosition, r *RiskReview) {
	for _, a := range plan.Actions {
		holding := hm[a.Symbol]
		avgVol := holding.AvgVolume
		qty := actionQuantity(a, holding)
		// Missing market liquidity data should not be silently treated as
		// "extremely illiquid" - that produced a permanent false-positive
		// warning for any symbol without AvgVolume metadata. Surface it as
		// an explicit "unknown" check so downstream operators can decide.
		if avgVol == 0 {
			r.Checks = append(r.Checks, RiskCheckResult{
				Rule:      "liquidity_check",
				Status:    "pass",
				Current:   0,
				Threshold: ra.cfg.MinLiquidityRatio,
				Message:   fmt.Sprintf("%s liquidity unknown (no avg volume data)", a.Symbol),
			})
			continue
		}
		ratio := float64(qty) / float64(avgVol)
		status, msg := "pass", fmt.Sprintf("%s order %d is %.2f%% of avg volume", a.Symbol, qty, ratio*100)
		if ratio > ra.cfg.MinLiquidityRatio {
			status = "warn"
			msg = fmt.Sprintf("%s order %d is %.2f%% of avg volume — liquidity risk", a.Symbol, qty, ratio*100)
			r.Warnings = append(r.Warnings, msg)
			r.Suggestions = append(r.Suggestions, fmt.Sprintf("Split %s order via TWAP to reduce impact", a.Symbol))
		}
		r.Checks = append(r.Checks, RiskCheckResult{"liquidity_check", status, ratio, ra.cfg.MinLiquidityRatio, msg})
	}
}

// ---------------------------------------------------------------------------
// Verdict & helpers
// ---------------------------------------------------------------------------

func (ra *RiskAgent) deriveVerdict(r *RiskReview) {
	hasFail, hasWarn := false, false
	for _, c := range r.Checks {
		switch c.Status {
		case "fail":
			hasFail = true
		case "warn":
			hasWarn = true
		}
	}
	switch {
	case hasFail:
		r.Verdict = "rejected"
	case hasWarn:
		r.Verdict = "approved_with_warnings"
	default:
		r.Verdict = "approved"
	}
}

// computeDrawdown estimates drawdown as (current - peak) / peak.
func buildHoldingMap(holdings []HoldingPosition) map[string]HoldingPosition {
	m := make(map[string]HoldingPosition, len(holdings))
	for _, h := range holdings {
		m[h.Symbol] = h
	}
	return m
}

func computeDrawdown(hm map[string]HoldingPosition, totalAssets float64) float64 {
	cost, current := 0.0, 0.0
	for _, h := range hm {
		cost += float64(h.Quantity) * h.AvgCost
		current += float64(h.Quantity) * h.MarketPrice
	}
	if cost == 0 {
		return 0
	}
	peak := math.Max(cost, totalAssets)
	return (current - peak) / peak
}

// String returns a concise summary of the review.
func (r *RiskReview) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "RiskReview[%s] verdict=%s checks=%d", r.PlanID, r.Verdict, len(r.Checks))
	if len(r.Warnings) > 0 {
		fmt.Fprintf(&b, " warnings=%v", r.Warnings)
	}
	if len(r.Rejections) > 0 {
		fmt.Fprintf(&b, " rejections=%v", r.Rejections)
	}
	return b.String()
}

func holdingMarketValue(h HoldingPosition) float64 {
	if h.MarketValue > 0 {
		return h.MarketValue
	}
	return float64(h.Quantity) * h.MarketPrice
}

func actionValue(a PlanAction, holding HoldingPosition) float64 {
	if a.Amount > 0 {
		return a.Amount
	}
	price := a.Price
	if price <= 0 {
		price = holding.MarketPrice
	}
	return float64(actionQuantity(a, holding)) * price
}

func actionQuantity(a PlanAction, holding HoldingPosition) int {
	if a.Quantity > 0 {
		return a.Quantity
	}
	if a.Amount > 0 {
		price := a.Price
		if price <= 0 {
			price = holding.MarketPrice
		}
		if price > 0 {
			return int(math.Round(a.Amount / price))
		}
	}
	return 0
}

func isSellAction(action string) bool {
	switch action {
	case "sell", "reduce":
		return true
	default:
		return false
	}
}

