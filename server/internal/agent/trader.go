// Package agent provides autonomous AI agents for the fund simulator.
// This file implements the Trader Agent, which optimises order execution by
// selecting strategies (immediate, TWAP, VWAP, limit) and splitting large
// orders into smaller child orders.
package agent

import (
	"context"
	"fmt"
	"log"
	"math"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Trading domain types
// ---------------------------------------------------------------------------

// TradingEngine is the interface to the simulated (or real) exchange.
type TradingEngine interface {
	GetQuote(ctx context.Context, symbol string) (*MarketQuote, error)
	PlaceOrder(ctx context.Context, order *TradeOrder) (*TradeResult, error)
}

// MarketQuote is a snapshot of a symbol's current market data.
type MarketQuote struct {
	Symbol string
	Price  float64
	Bid    float64
	Ask    float64
	Volume int64
}

// TradeOrder is a single order sent to the TradingEngine.
type TradeOrder struct {
	Symbol    string
	Side      string  // "buy", "sell"
	OrderType string  // "market", "limit"
	Quantity  int
	Price     float64 // relevant for limit orders
}

// TradeResult is the fill report returned by the TradingEngine.
type TradeResult struct {
	OrderID     string
	Symbol      string
	Side        string
	FilledQty   int
	FilledPrice float64
	Fee         float64
	Status      string // "filled", "partial", "rejected"
}

// ExecutionStrategy names a strategy the Trader Agent can use.
type ExecutionStrategy string

const (
	StrategyImmediate ExecutionStrategy = "immediate"
	StrategyTWAP      ExecutionStrategy = "twap"
	StrategyVWAP      ExecutionStrategy = "vwap"
	StrategyLimit     ExecutionStrategy = "limit"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// TraderConfig controls splitting and strategy selection behaviour.
type TraderConfig struct {
	SplitThreshold   int           // orders above this qty get split
	MaxChildQty      int           // max qty per child order
	TWAPSlices       int           // number of time slices for TWAP
	TWAPInterval     time.Duration // simulated delay between slices
	FeeRate          float64       // per-fill fee rate (e.g. 0.001 = 0.1%)
	LimitPriceOffset float64       // offset from mid for limit orders (ratio)
}

// DefaultTraderConfig returns sensible defaults for the simulator.
func DefaultTraderConfig() TraderConfig {
	return TraderConfig{
		SplitThreshold: 1000, MaxChildQty: 500,
		TWAPSlices: 5, TWAPInterval: 200 * time.Millisecond,
		FeeRate: 0.001, LimitPriceOffset: 0.002,
	}
}

// ---------------------------------------------------------------------------
// TraderAgent
// ---------------------------------------------------------------------------

// TraderAgent optimises order execution for approved investment plans.
type TraderAgent struct {
	cfg     TraderConfig
	logger  *log.Logger
	mu      sync.Mutex
	running bool
	stopCh  chan struct{}
}

// NewTraderAgent creates a TraderAgent with the given configuration.
func NewTraderAgent(cfg TraderConfig, logger *log.Logger) *TraderAgent {
	if logger == nil {
		logger = log.Default()
	}
	return &TraderAgent{cfg: cfg, logger: logger}
}

// Start initialises the agent (idempotent).
func (ta *TraderAgent) Start() {
	ta.mu.Lock()
	defer ta.mu.Unlock()
	if ta.running {
		return
	}
	ta.running = true
	ta.stopCh = make(chan struct{})
	ta.logger.Println("[TraderAgent] started")
}

// Stop shuts down the agent gracefully.
func (ta *TraderAgent) Stop() {
	ta.mu.Lock()
	defer ta.mu.Unlock()
	if !ta.running {
		return
	}
	ta.running = false
	close(ta.stopCh)
	ta.logger.Println("[TraderAgent] stopped")
}

// ExecutePlan executes every action in the plan and returns one
// ExecutionResult per action.
func (ta *TraderAgent) ExecutePlan(ctx context.Context, plan *InvestmentPlan, engine TradingEngine) ([]ExecutionResult, error) {
	if plan == nil {
		return nil, fmt.Errorf("trader: plan must not be nil")
	}
	if engine == nil {
		return nil, fmt.Errorf("trader: engine must not be nil")
	}
	results := make([]ExecutionResult, 0, len(plan.Actions))
	for i := range plan.Actions {
		a := &plan.Actions[i]
		strategy := ta.selectStrategy(a)
		ta.logger.Printf("[TraderAgent] %s %s %d @ %.2f strategy=%s", actionSide(a.Action), a.Symbol, a.Quantity, a.Price, strategy)

		var res ExecutionResult
		var err error
		switch strategy {
		case StrategyTWAP:
			res, err = ta.executeTWAP(ctx, a, engine)
		case StrategyVWAP:
			res, err = ta.executeVWAP(ctx, a, engine)
		case StrategyLimit:
			res, err = ta.executeLimit(ctx, a, engine)
		default:
			res, err = ta.executeImmediate(ctx, a, engine)
		}
		if err != nil {
			res.Error = err.Error()
			ta.logger.Printf("[TraderAgent] %s %s failed: %v", actionSide(a.Action), a.Symbol, err)
		}
		res.Action = a.Action
		res.Strategy = strategy
		results = append(results, res)
	}
	ta.logger.Printf("[TraderAgent] plan %s complete — %d actions", plan.ID, len(results))
	return results, nil
}

// ---------------------------------------------------------------------------
// Strategy selection
// ---------------------------------------------------------------------------

func (ta *TraderAgent) selectStrategy(a *PlanAction) ExecutionStrategy {
	if a.Quantity > ta.cfg.SplitThreshold {
		return StrategyTWAP
	}
	if a.Price > 0 {
		return StrategyLimit
	}
	return StrategyImmediate
}

// ---------------------------------------------------------------------------
// Strategy implementations
// ---------------------------------------------------------------------------

func (ta *TraderAgent) executeImmediate(ctx context.Context, a *PlanAction, eng TradingEngine) (ExecutionResult, error) {
	start := time.Now()
	res := ExecutionResult{Symbol: a.Symbol, Action: a.Action,RequestedQty: a.Quantity, StartedAt: start}
	quote, err := eng.GetQuote(ctx, a.Symbol)
	if err != nil {
		res.CompletedAt = time.Now()
		return res, fmt.Errorf("get quote %s: %w", a.Symbol, err)
	}
	fills, err := ta.submitChildOrders(ctx, eng, a.Symbol, actionSide(a.Action), "market", a.Quantity, 0)
	if err != nil {
		res.CompletedAt = time.Now()
		return res, err
	}
	ta.aggregateFills(&res, fills, quote.Price)
	res.CompletedAt = time.Now()
	return res, nil
}

func (ta *TraderAgent) executeTWAP(ctx context.Context, a *PlanAction, eng TradingEngine) (ExecutionResult, error) {
	start := time.Now()
	res := ExecutionResult{Symbol: a.Symbol, Action: a.Action,RequestedQty: a.Quantity, StartedAt: start}
	quote, err := eng.GetQuote(ctx, a.Symbol)
	if err != nil {
		res.CompletedAt = time.Now()
		return res, fmt.Errorf("get quote %s: %w", a.Symbol, err)
	}
	slices := ta.cfg.TWAPSlices
	if slices <= 0 {
		slices = 1
	}
	base, rem := a.Quantity/slices, a.Quantity%slices
	var allFills []TradeResult
	for i := 0; i < slices; i++ {
		qty := base
		if i == slices-1 {
			qty += rem
		}
		if qty <= 0 {
			continue
		}
		if i > 0 {
			select {
			case <-ctx.Done():
				ta.aggregateFills(&res, allFills, quote.Price)
				res.CompletedAt = time.Now()
				return res, ctx.Err()
			case <-time.After(ta.cfg.TWAPInterval):
			}
		}
		fills, err := ta.submitChildOrders(ctx, eng, a.Symbol, actionSide(a.Action), "market", qty, 0)
		if err != nil {
			ta.logger.Printf("[TraderAgent] TWAP slice %d/%d failed: %v", i+1, slices, err)
			continue
		}
		allFills = append(allFills, fills...)
	}
	ta.aggregateFills(&res, allFills, quote.Price)
	res.CompletedAt = time.Now()
	return res, nil
}

func (ta *TraderAgent) executeVWAP(ctx context.Context, a *PlanAction, eng TradingEngine) (ExecutionResult, error) {
	start := time.Now()
	res := ExecutionResult{Symbol: a.Symbol, Action: a.Action,RequestedQty: a.Quantity, StartedAt: start}
	quote, err := eng.GetQuote(ctx, a.Symbol)
	if err != nil {
		res.CompletedAt = time.Now()
		return res, fmt.Errorf("get quote %s: %w", a.Symbol, err)
	}
	// Synthetic U-shaped intraday volume weights.
	weights := []float64{0.25, 0.15, 0.10, 0.15, 0.35}
	totalW := 0.0
	for _, w := range weights {
		totalW += w
	}
	var allFills []TradeResult
	remaining := a.Quantity
	for i, w := range weights {
		qty := int(math.Round(float64(a.Quantity) * w / totalW))
		if i == len(weights)-1 {
			qty = remaining
		}
		if qty <= 0 {
			continue
		}
		if qty > remaining {
			qty = remaining
		}
		remaining -= qty
		if i > 0 {
			select {
			case <-ctx.Done():
				ta.aggregateFills(&res, allFills, quote.Price)
				res.CompletedAt = time.Now()
				return res, ctx.Err()
			case <-time.After(ta.cfg.TWAPInterval):
			}
		}
		fills, err := ta.submitChildOrders(ctx, eng, a.Symbol, actionSide(a.Action), "market", qty, 0)
		if err != nil {
			ta.logger.Printf("[TraderAgent] VWAP bucket %d/%d failed: %v", i+1, len(weights), err)
			continue
		}
		allFills = append(allFills, fills...)
	}
	ta.aggregateFills(&res, allFills, quote.Price)
	res.CompletedAt = time.Now()
	return res, nil
}

func (ta *TraderAgent) executeLimit(ctx context.Context, a *PlanAction, eng TradingEngine) (ExecutionResult, error) {
	start := time.Now()
	res := ExecutionResult{Symbol: a.Symbol, Action: a.Action,RequestedQty: a.Quantity, StartedAt: start}
	quote, err := eng.GetQuote(ctx, a.Symbol)
	if err != nil {
		res.CompletedAt = time.Now()
		return res, fmt.Errorf("get quote %s: %w", a.Symbol, err)
	}
	mid := (quote.Bid + quote.Ask) / 2
	price := a.Price
	if price <= 0 {
		offset := mid * ta.cfg.LimitPriceOffset
		if actionSide(a.Action) == "buy" {
			price = mid - offset
		} else {
			price = mid + offset
		}
	}
	price = math.Round(price*100) / 100
	fills, err := ta.submitChildOrders(ctx, eng, a.Symbol, actionSide(a.Action), "limit", a.Quantity, price)
	if err != nil {
		res.CompletedAt = time.Now()
		return res, err
	}
	ta.aggregateFills(&res, fills, quote.Price)
	res.CompletedAt = time.Now()
	return res, nil
}

// ---------------------------------------------------------------------------
// Order splitting & submission
// ---------------------------------------------------------------------------

// submitChildOrders breaks qty into chunks ≤ MaxChildQty and places each.
func (ta *TraderAgent) submitChildOrders(ctx context.Context, eng TradingEngine, symbol, side, orderType string, qty int, price float64) ([]TradeResult, error) {
	maxChild := ta.cfg.MaxChildQty
	if maxChild <= 0 {
		maxChild = qty
	}
	var fills []TradeResult
	remaining := qty
	for remaining > 0 {
		childQty := remaining
		if childQty > maxChild {
			childQty = maxChild
		}
		order := &TradeOrder{Symbol: symbol, Side: side, OrderType: orderType, Quantity: childQty, Price: price}
		tr, err := eng.PlaceOrder(ctx, order)
		if err != nil {
			return fills, fmt.Errorf("place order %s %s %d: %w", side, symbol, childQty, err)
		}
		fills = append(fills, *tr)
		if tr.Status == "rejected" {
			return fills, fmt.Errorf("order rejected: %s %s %d", side, symbol, childQty)
		}
		remaining -= tr.FilledQty
	}
	return fills, nil
}

// ---------------------------------------------------------------------------
// Aggregation
// ---------------------------------------------------------------------------

func (ta *TraderAgent) aggregateFills(res *ExecutionResult, fills []TradeResult, refPrice float64) {
	res.ChildOrders = fills
	totalQty, totalNotional, totalFees := 0, 0.0, 0.0
	for _, f := range fills {
		totalQty += f.FilledQty
		totalNotional += float64(f.FilledQty) * f.FilledPrice
		totalFees += f.Fee
	}
	res.FilledQty = totalQty
	res.Commission = totalFees
	if totalQty > 0 {
		res.AvgFillPrice = totalNotional / float64(totalQty)
	}
	if totalQty == res.RequestedQty {
		res.Status = "filled"
	} else if totalQty > 0 {
		res.Status = "partial"
	} else {
		res.Status = "rejected"
	}
}

func actionSide(action string) string {
	switch action {
	case "sell", "reduce":
		return "sell"
	default:
		return "buy"
	}
}
