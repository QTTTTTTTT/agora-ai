// Package agent — unit tests for TraderAgent.ExecutePlan and the
// supporting strategy / order-splitting helpers. Added during the
// 2026-05-28 audit that found trader.go had zero coverage despite
// being the final execution gate before orders hit the engine.
//
// Test approach uses a fakeTradingEngine that captures every order
// it receives so assertions can verify (a) the strategy selector
// picked the right execution path, (b) submitChildOrders honoured
// MaxChildQty, and (c) the aggregator computed FilledQty / Status
// from the synthesised fills correctly.
package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeTradingEngine records every order it receives and replies with
// a configurable fill. fill defaults to "full fill at quoted price".
type fakeTradingEngine struct {
	mu        sync.Mutex
	quotes    map[string]MarketQuote
	orders    []*TradeOrder
	respond   func(*TradeOrder) *TradeResult // optional override
	failNthGetQuote int                       // 0 = never fail
}

func (f *fakeTradingEngine) GetQuote(_ context.Context, symbol string) (*MarketQuote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNthGetQuote > 0 {
		f.failNthGetQuote--
		if f.failNthGetQuote == 0 {
			return nil, errors.New("quote provider down")
		}
	}
	if q, ok := f.quotes[symbol]; ok {
		return &q, nil
	}
	return &MarketQuote{Symbol: symbol, Price: 100, Bid: 99.5, Ask: 100.5, Volume: 1_000_000}, nil
}

func (f *fakeTradingEngine) PlaceOrder(_ context.Context, order *TradeOrder) (*TradeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	clone := *order
	f.orders = append(f.orders, &clone)
	if f.respond != nil {
		return f.respond(order), nil
	}
	price := order.Price
	if price == 0 {
		// market fill: snap to quote
		if q, ok := f.quotes[order.Symbol]; ok {
			price = q.Price
		} else {
			price = 100
		}
	}
	return &TradeResult{
		OrderID:     "ord-" + order.Symbol,
		Symbol:      order.Symbol,
		Side:        order.Side,
		FilledQty:   order.Quantity,
		FilledPrice: price,
		Fee:         float64(order.Quantity) * price * 0.001,
		Status:      "filled",
	}, nil
}

func newPlan(actions []PlanAction) *InvestmentPlan {
	return &InvestmentPlan{ID: "p", FundID: "f", Date: "2026-05-28", Actions: actions}
}

// Small order, no limit price → executor picks immediate (market).
func TestTraderAgentSelectsImmediateStrategy(t *testing.T) {
	ta := NewTraderAgent(DefaultTraderConfig(), nil)
	plan := newPlan([]PlanAction{{Symbol: "AAPL", Action: "buy", Quantity: 100}})
	eng := &fakeTradingEngine{quotes: map[string]MarketQuote{"AAPL": {Symbol: "AAPL", Price: 180, Bid: 179.5, Ask: 180.5, Volume: 1_000_000}}}
	results, err := ta.ExecutePlan(context.Background(), plan, eng)
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Strategy != StrategyImmediate {
		t.Errorf("strategy = %q, want immediate", results[0].Strategy)
	}
	if results[0].FilledQty != 100 || results[0].Status != "filled" {
		t.Errorf("fill: qty=%d status=%q, want 100/filled", results[0].FilledQty, results[0].Status)
	}
	// One child order (qty ≤ MaxChildQty=500) so one PlaceOrder call.
	if got := len(eng.orders); got != 1 {
		t.Errorf("orders submitted = %d, want 1", got)
	}
	if eng.orders[0].OrderType != "market" {
		t.Errorf("orderType = %q, want market", eng.orders[0].OrderType)
	}
}

// Quantity above SplitThreshold (1000) → TWAP across 5 slices.
func TestTraderAgentSelectsTWAPStrategyAboveSplitThreshold(t *testing.T) {
	cfg := DefaultTraderConfig()
	cfg.TWAPInterval = 1 * time.Millisecond // keep test fast
	ta := NewTraderAgent(cfg, nil)
	plan := newPlan([]PlanAction{{Symbol: "MSFT", Action: "buy", Quantity: 2500}}) // 2500 > 1000 threshold
	eng := &fakeTradingEngine{quotes: map[string]MarketQuote{"MSFT": {Symbol: "MSFT", Price: 420, Bid: 419.5, Ask: 420.5, Volume: 5_000_000}}}
	results, err := ta.ExecutePlan(context.Background(), plan, eng)
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	if results[0].Strategy != StrategyTWAP {
		t.Errorf("strategy = %q, want twap", results[0].Strategy)
	}
	if results[0].FilledQty != 2500 {
		t.Errorf("filledQty = %d, want 2500", results[0].FilledQty)
	}
	if results[0].Status != "filled" {
		t.Errorf("status = %q, want filled", results[0].Status)
	}
	// 2500 qty / 5 slices = 500 per slice; each slice ≤ MaxChildQty=500 so
	// 5 PlaceOrder calls total (one child per slice).
	if got := len(eng.orders); got != 5 {
		t.Errorf("orders submitted = %d, want 5 (one per TWAP slice)", got)
	}
}

// Small order WITH explicit limit price → limit strategy.
func TestTraderAgentSelectsLimitStrategyWhenPriceProvided(t *testing.T) {
	ta := NewTraderAgent(DefaultTraderConfig(), nil)
	plan := newPlan([]PlanAction{{Symbol: "NVDA", Action: "buy", Quantity: 50, Price: 950}})
	eng := &fakeTradingEngine{quotes: map[string]MarketQuote{"NVDA": {Symbol: "NVDA", Price: 960, Bid: 959, Ask: 961}}}
	results, err := ta.ExecutePlan(context.Background(), plan, eng)
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	if results[0].Strategy != StrategyLimit {
		t.Errorf("strategy = %q, want limit", results[0].Strategy)
	}
	if got := len(eng.orders); got != 1 || eng.orders[0].OrderType != "limit" || eng.orders[0].Price != 950 {
		t.Errorf("limit order incorrect: orders=%+v", eng.orders)
	}
}

// When PlanAction.Price is provided the limit executor must use it
// verbatim (rounded to 2 decimals). When Price ≤ 0 the limit fallback
// is unreachable from the selectStrategy path (it routes those orders
// to immediate), so this test only asserts the explicit-price path.
func TestTraderAgentLimitPriceHonouredVerbatim(t *testing.T) {
	ta := NewTraderAgent(DefaultTraderConfig(), nil)
	plan := newPlan([]PlanAction{{Symbol: "TSLA", Action: "buy", Quantity: 10, Price: 195.673}})
	eng := &fakeTradingEngine{quotes: map[string]MarketQuote{"TSLA": {Symbol: "TSLA", Price: 200, Bid: 199, Ask: 201}}}
	results, err := ta.ExecutePlan(context.Background(), plan, eng)
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	if results[0].Strategy != StrategyLimit {
		t.Errorf("strategy = %q, want limit", results[0].Strategy)
	}
	// executeLimit always rounds to 2 decimals (the math.Round line
	// runs after the price branch). 195.673 → 195.67. The rounding is
	// global to all limit orders so e.g. an explicit fractional price
	// from the LLM still survives broker round-lot constraints.
	if eng.orders[0].Price != 195.67 {
		t.Errorf("limit price = %v, want 195.67 (2-decimal rounding)", eng.orders[0].Price)
	}
}

// MaxChildQty enforcement: a 1500-qty order with maxChild=500 must
// fan out to 3 child orders.
func TestTraderAgentSplitsChildOrdersByMaxChildQty(t *testing.T) {
	cfg := DefaultTraderConfig()
	cfg.SplitThreshold = 10_000 // disable TWAP so we exercise child split via immediate
	cfg.MaxChildQty = 500
	ta := NewTraderAgent(cfg, nil)
	plan := newPlan([]PlanAction{{Symbol: "AMZN", Action: "buy", Quantity: 1500}})
	eng := &fakeTradingEngine{quotes: map[string]MarketQuote{"AMZN": {Symbol: "AMZN", Price: 200, Bid: 199.5, Ask: 200.5}}}
	results, err := ta.ExecutePlan(context.Background(), plan, eng)
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	if got := len(eng.orders); got != 3 {
		t.Errorf("child orders = %d, want 3", got)
	}
	want := []int{500, 500, 500}
	for i, o := range eng.orders {
		if o.Quantity != want[i] {
			t.Errorf("child %d qty=%d, want %d", i, o.Quantity, want[i])
		}
	}
	if results[0].FilledQty != 1500 {
		t.Errorf("total filled = %d, want 1500", results[0].FilledQty)
	}
}

// aggregateFills correctness across multiple child orders that all
// fully fill (not partial). Sub-order quantities sum to the requested
// total → status="filled", AvgFillPrice = weighted mean.
func TestTraderAgentAggregatesMultipleChildFills(t *testing.T) {
	cfg := DefaultTraderConfig()
	cfg.MaxChildQty = 50 // 100 qty → 2 children
	cfg.SplitThreshold = 10_000
	ta := NewTraderAgent(cfg, nil)
	plan := newPlan([]PlanAction{{Symbol: "GME", Action: "buy", Quantity: 100}})
	calls := 0
	eng := &fakeTradingEngine{
		quotes: map[string]MarketQuote{"GME": {Symbol: "GME", Price: 25, Bid: 24.9, Ask: 25.1}},
		respond: func(o *TradeOrder) *TradeResult {
			calls++
			// First child fills at 24, second at 26 → weighted avg 25.
			price := 24.0
			if calls == 2 {
				price = 26.0
			}
			return &TradeResult{OrderID: "ord", Symbol: o.Symbol, Side: o.Side, FilledQty: o.Quantity, FilledPrice: price, Fee: 1, Status: "filled"}
		},
	}
	results, err := ta.ExecutePlan(context.Background(), plan, eng)
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	if results[0].FilledQty != 100 || results[0].Status != "filled" {
		t.Errorf("aggregation: filledQty=%d status=%q, want 100/filled", results[0].FilledQty, results[0].Status)
	}
	if results[0].AvgFillPrice < 24.9 || results[0].AvgFillPrice > 25.1 {
		t.Errorf("AvgFillPrice = %v, want ~25 (weighted mean of 24 and 26)", results[0].AvgFillPrice)
	}
	if results[0].Commission != 2 {
		t.Errorf("Commission = %v, want 2 (sum of two fees)", results[0].Commission)
	}
	if len(results[0].ChildOrders) != 2 {
		t.Errorf("ChildOrders = %d, want 2", len(results[0].ChildOrders))
	}
}

// Known semantic: when submitChildOrders hits a "rejected" child it
// returns (fills_so_far, error) AND the immediate executor SHORT-
// CIRCUITS instead of aggregating the partial fills it already
// received. This locks in the current behaviour so a future refactor
// that decides to aggregate-then-error has to consciously update this
// test. See agent/trader.go:executeImmediate — the rejection error
// path returns BEFORE ta.aggregateFills is called.
func TestTraderAgentChildRejectionShortCircuitsAggregation(t *testing.T) {
	ta := NewTraderAgent(DefaultTraderConfig(), nil)
	plan := newPlan([]PlanAction{{Symbol: "GME", Action: "buy", Quantity: 100}})
	calls := 0
	eng := &fakeTradingEngine{
		quotes: map[string]MarketQuote{"GME": {Symbol: "GME", Price: 25, Bid: 24.9, Ask: 25.1}},
		respond: func(o *TradeOrder) *TradeResult {
			calls++
			if calls == 1 {
				return &TradeResult{OrderID: "ord1", Symbol: o.Symbol, Side: o.Side, FilledQty: 80, FilledPrice: 25, Status: "partial"}
			}
			return &TradeResult{OrderID: "ord2", Symbol: o.Symbol, Side: o.Side, FilledQty: 0, Status: "rejected"}
		},
	}
	results, err := ta.ExecutePlan(context.Background(), plan, eng)
	if err != nil {
		t.Fatalf("ExecutePlan should not propagate: %v", err)
	}
	if results[0].Error == "" {
		t.Error("res.Error must capture the rejection")
	}
}

// GetQuote error MUST abort that action with the error captured in
// ExecutionResult.Error, but other actions should still execute.
func TestTraderAgentQuoteErrorIsolatedToFailingAction(t *testing.T) {
	ta := NewTraderAgent(DefaultTraderConfig(), nil)
	plan := newPlan([]PlanAction{
		{Symbol: "BROKEN", Action: "buy", Quantity: 50},
		{Symbol: "OK", Action: "buy", Quantity: 50},
	})
	eng := &fakeTradingEngine{
		quotes: map[string]MarketQuote{"OK": {Symbol: "OK", Price: 100, Bid: 99.5, Ask: 100.5}},
		failNthGetQuote: 1, // first GetQuote fails (BROKEN)
	}
	results, err := ta.ExecutePlan(context.Background(), plan, eng)
	if err != nil {
		t.Fatalf("ExecutePlan should not propagate single-action error, got %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	if results[0].Error == "" {
		t.Errorf("first action should report Error; got %+v", results[0])
	}
	if results[1].Status != "filled" {
		t.Errorf("second action should fill cleanly, got status=%q", results[1].Status)
	}
}

func TestTraderAgentRejectsNilPlan(t *testing.T) {
	ta := NewTraderAgent(DefaultTraderConfig(), nil)
	if _, err := ta.ExecutePlan(context.Background(), nil, &fakeTradingEngine{}); err == nil {
		t.Fatal("nil plan: expected error")
	}
}

func TestTraderAgentRejectsNilEngine(t *testing.T) {
	ta := NewTraderAgent(DefaultTraderConfig(), nil)
	if _, err := ta.ExecutePlan(context.Background(), newPlan(nil), nil); err == nil {
		t.Fatal("nil engine: expected error")
	}
}

// Sell action must route to the "sell" side regardless of being
// labelled "sell" or "reduce".
func TestTraderAgentReduceActionRoutesAsSell(t *testing.T) {
	ta := NewTraderAgent(DefaultTraderConfig(), nil)
	plan := newPlan([]PlanAction{{Symbol: "AAPL", Action: "reduce", Quantity: 50}})
	eng := &fakeTradingEngine{quotes: map[string]MarketQuote{"AAPL": {Symbol: "AAPL", Price: 180, Bid: 179.5, Ask: 180.5}}}
	if _, err := ta.ExecutePlan(context.Background(), plan, eng); err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	if eng.orders[0].Side != "sell" {
		t.Errorf("reduce action routed to side=%q, want sell", eng.orders[0].Side)
	}
}
