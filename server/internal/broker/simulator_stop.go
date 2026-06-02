// Stop / stop-limit / trailing-stop support for the simulator (P0-3).
//
// Stop-typed orders are NOT routed through the matching engine on
// PlaceOrder. They rest in OrderStatePending until the stop-trigger
// engine — see internal/stoptrigger — observes a quote that breaches
// the trigger and asks the simulator to fire them.
//
// This file holds the simulator-side surface area the trigger engine
// drives:
//
//   - PendingStopsForInstrument: snapshot of stops awaiting trigger
//     on a given instrument.
//   - UpdateTrailingHighWater / UpdateTrailingLowWater: ratchet
//     trailing-stop price as the underlying moves favourably.
//   - FireStopTrigger: convert a pending stop into a working/filled
//     child order via the matching engine.
//
// All three are concurrency-safe; the trigger engine may call them
// from a quote-tick goroutine while user PlaceOrder/CancelOrder is
// happening on another.

package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fundai/server/internal/matching"
)

// ErrStopTriggerNotApplicable is returned when FireStopTrigger or the
// trailing-update helpers are called on an order that does not have
// stop semantics (e.g. a market or limit order).
var ErrStopTriggerNotApplicable = errors.New("broker: order is not a stop-typed order")

// ErrStopAlreadyFired is returned when FireStopTrigger is called on
// an order that is no longer in OrderStatePending — either it has
// already been fired (now Triggered/Filled) or it was cancelled.
// Treating it as an error keeps the trigger-engine idempotency
// surface explicit; callers should ignore this error and move on.
var ErrStopAlreadyFired = errors.New("broker: stop already fired or no longer pending")

// PendingStopsForInstrument returns a snapshot of every stop /
// stop_limit / trailing_stop order currently in OrderStatePending
// for the given (instrumentKey, symbol) pair. At least one of the
// two MUST be non-empty.
//
// The trigger engine calls this on each new quote tick. The result
// is a defensive copy — callers may mutate it freely without
// affecting simulator state.
func (s *Simulator) PendingStopsForInstrument(instrumentKey, symbol string) []Order {
	instrumentKey = strings.TrimSpace(instrumentKey)
	symbol = strings.TrimSpace(symbol)
	if instrumentKey == "" && symbol == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Order
	for _, o := range s.orders {
		if o == nil || o.State != OrderStatePending {
			continue
		}
		if !o.Request.OrderType.IsStopType() {
			continue
		}
		if !instrumentMatches(o.Request, instrumentKey, symbol) {
			continue
		}
		cp := *o
		out = append(out, cp)
	}
	sortOrdersByPlacedAt(out)
	return out
}

// AllPendingStops returns every stop / stop_limit / trailing_stop
// order currently in OrderStatePending across ALL instruments. The
// result is a defensive copy; mutating it does not affect simulator
// state.
//
// The poller (cmd/server/stop_trigger_poller.go) calls this once per
// tick to deduplicate the set of instruments it must fetch quotes
// for. Callers that only care about a single instrument should use
// PendingStopsForInstrument instead — it allocates less and skips the
// no-instrument-filter branch.
func (s *Simulator) AllPendingStops() []Order {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Order
	for _, o := range s.orders {
		if o == nil || o.State != OrderStatePending {
			continue
		}
		if !o.Request.OrderType.IsStopType() {
			continue
		}
		cp := *o
		out = append(out, cp)
	}
	sortOrdersByPlacedAt(out)
	return out
}

// instrumentMatches matches an order against the (instrumentKey,
// symbol) pair the trigger engine queries by. We accept either
// identifier — instrument key is the canonical id, but Asia symbols
// may be addressed by raw symbol when the key is missing.
func instrumentMatches(req PlaceOrderRequest, instrumentKey, symbol string) bool {
	if instrumentKey != "" && req.InstrumentKey != "" {
		return req.InstrumentKey == instrumentKey
	}
	if symbol != "" && req.Symbol != "" {
		return req.Symbol == symbol
	}
	return false
}

// UpdateTrailingHighWater is called by the trigger engine on every
// new quote for a trailing SELL-stop (the common protect-a-long
// case). If last > existing HighWater, the simulator ratchets the
// stored HighWater up and recomputes CurrentStopPrice as
//
//	HighWater - TrailAmount  (or  HighWater * (1 - TrailPercent))
//
// Returns the post-update Order snapshot, or ErrOrderNotFound /
// ErrStopTriggerNotApplicable when the input is wrong.
//
// Idempotent: passing a last that does not improve HighWater is a
// no-op and returns the unchanged order.
func (s *Simulator) UpdateTrailingHighWater(brokerOrderID string, last float64) (*Order, error) {
	if brokerOrderID == "" {
		return nil, fmt.Errorf("%w: missing broker_order_id", ErrInvalidRequest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[brokerOrderID]
	if !ok {
		return nil, ErrOrderNotFound
	}
	if order.Request.OrderType != OrderTypeTrailingStop || order.Request.Side != SideSell {
		return nil, ErrStopTriggerNotApplicable
	}
	if order.State != OrderStatePending {
		return nil, ErrStopAlreadyFired
	}
	if last > order.TrailingHighWater {
		order.TrailingHighWater = last
		order.CurrentStopPrice = computeTrailingStopFromHigh(last, order.Request)
		order.UpdatedAt = s.nowFn()
	}
	cp := *order
	return &cp, nil
}

// UpdateTrailingLowWater is the BUY-side mirror of
// UpdateTrailingHighWater: a trailing buy-stop protects a short
// position, so the stop ratchets DOWN as the market falls. The new
// CurrentStopPrice is
//
//	LowWater + TrailAmount  (or  LowWater * (1 + TrailPercent))
func (s *Simulator) UpdateTrailingLowWater(brokerOrderID string, last float64) (*Order, error) {
	if brokerOrderID == "" {
		return nil, fmt.Errorf("%w: missing broker_order_id", ErrInvalidRequest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[brokerOrderID]
	if !ok {
		return nil, ErrOrderNotFound
	}
	if order.Request.OrderType != OrderTypeTrailingStop || order.Request.Side != SideBuy {
		return nil, ErrStopTriggerNotApplicable
	}
	if order.State != OrderStatePending {
		return nil, ErrStopAlreadyFired
	}
	if order.TrailingLowWater == 0 || last < order.TrailingLowWater {
		order.TrailingLowWater = last
		order.CurrentStopPrice = computeTrailingStopFromLow(last, order.Request)
		order.UpdatedAt = s.nowFn()
	}
	cp := *order
	return &cp, nil
}

// FireStopTrigger transitions a pending stop to OrderStateTriggered
// and places a child order that the matching engine fills as if it
// were a regular market / limit submission. Used by the stop-trigger
// engine when it observes a quote that breaches CurrentStopPrice.
//
// The child order:
//
//   - inherits FundID, Symbol, InstrumentKey, Quantity, Side from
//     the parent;
//   - is OrderTypeMarket for a stop or trailing_stop, OrderTypeLimit
//     for a stop_limit (with the parent's LimitPrice carried over);
//   - is linked to the parent via ParentBrokerOrderID, and the
//     parent records the child's id on TriggeredChildOrderID.
//
// Returns the child Order (already routed to tryFill) or an error
// matching the broker error taxonomy. ErrStopAlreadyFired means
// some other goroutine already fired this stop; the caller should
// treat it as a no-op.
func (s *Simulator) FireStopTrigger(ctx context.Context, brokerOrderID string, quote matching.Quote) (*Order, error) {
	if brokerOrderID == "" {
		return nil, fmt.Errorf("%w: missing broker_order_id", ErrInvalidRequest)
	}
	s.mu.Lock()
	parent, ok := s.orders[brokerOrderID]
	if !ok {
		s.mu.Unlock()
		return nil, ErrOrderNotFound
	}
	if !parent.Request.OrderType.IsStopType() {
		s.mu.Unlock()
		return nil, ErrStopTriggerNotApplicable
	}
	if parent.State != OrderStatePending {
		s.mu.Unlock()
		return nil, ErrStopAlreadyFired
	}

	// Build the child order under the lock so observers never see a
	// "triggered" parent without a matching child id.
	child := buildStopChild(parent, s.idFn(), s.nowFn())
	s.orders[child.BrokerOrderID] = child
	// Note: we deliberately do NOT add the child to clientIndex —
	// the parent owns the user-supplied client_order_id.
	s.markOpenLocked(child.Request.FundID, child.BrokerOrderID)

	parent.State = OrderStateTriggered
	parent.TriggeredChildOrderID = child.BrokerOrderID
	parent.UpdatedAt = s.nowFn()
	s.removeOpenLocked(parent.Request.FundID, parent.BrokerOrderID)
	s.mu.Unlock()

	// Route the child through the existing fill path. tryFill
	// uses the simulator's quote function and matching engine so a
	// market child fills against the live spread; a limit child
	// rests if not marketable.
	if err := s.tryFill(ctx, child); err != nil {
		cp := s.copyOrder(child.BrokerOrderID)
		if cp == nil {
			return nil, err
		}
		return cp, err
	}
	cp := s.copyOrder(child.BrokerOrderID)
	if cp == nil {
		return nil, ErrOrderNotFound
	}
	// Use the supplied quote when tryFill couldn't get one — but
	// in practice tryFill has the canonical quote, so the supplied
	// argument is mostly future-proofing for replay/back-test use.
	_ = quote
	return cp, nil
}

// buildStopChild materialises the child order placed when a stop
// fires. Caller must hold s.mu.
func buildStopChild(parent *Order, childBrokerOrderID string, now time.Time) *Order {
	pr := parent.Request
	childReq := PlaceOrderRequest{
		FundID:             pr.FundID,
		ClientOrderID:      pr.ClientOrderID + ":child",
		Symbol:             pr.Symbol,
		InstrumentKey:      pr.InstrumentKey,
		Market:             pr.Market,
		AssetClass:         pr.AssetClass,
		Side:               pr.Side,
		Quantity:           pr.Quantity,
		ContractMultiplier: pr.ContractMultiplier,
		ReduceOnly:         pr.ReduceOnly,
		PositionSide:       pr.PositionSide,
		Metadata:           pr.Metadata,
	}
	switch pr.OrderType {
	case OrderTypeStop, OrderTypeTrailingStop:
		childReq.OrderType = OrderTypeMarket
		childReq.TimeInForce = TIFIOC
	case OrderTypeStopLimit:
		childReq.OrderType = OrderTypeLimit
		childReq.LimitPrice = pr.LimitPrice
		// Stop-limit children inherit the parent's TIF if any,
		// defaulting to DAY so a non-marketable limit can rest.
		childReq.TimeInForce = pr.TimeInForce
		if childReq.TimeInForce == "" {
			childReq.TimeInForce = TIFDay
		}
	}
	return &Order{
		BrokerOrderID:       childBrokerOrderID,
		ClientOrderID:       childReq.ClientOrderID,
		Request:             childReq,
		State:               OrderStatePending,
		PlacedAt:            now,
		UpdatedAt:           now,
		ParentBrokerOrderID: parent.BrokerOrderID,
	}
}

// seedTrailingFromQuote fetches the current quote and seeds the
// trailing high/low water mark + CurrentStopPrice. Best-effort: if
// the quote feed is down we leave the fields zero and let the trigger
// engine seed on its first OnQuote.
//
// Caller must NOT hold s.mu — we acquire it to mutate the order.
func (s *Simulator) seedTrailingFromQuote(ctx context.Context, order *Order) {
	if order == nil || order.Request.OrderType != OrderTypeTrailingStop {
		return
	}
	q, err := s.quoteFn(ctx, order.Request.InstrumentKey, order.Request.Symbol, order.Request.Market)
	if err != nil || q.Last <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[order.BrokerOrderID]
	if !ok || o.State != OrderStatePending {
		return
	}
	if o.Request.Side == SideSell {
		o.TrailingHighWater = q.Last
		o.CurrentStopPrice = computeTrailingStopFromHigh(q.Last, o.Request)
	} else {
		o.TrailingLowWater = q.Last
		o.CurrentStopPrice = computeTrailingStopFromLow(q.Last, o.Request)
	}
	o.UpdatedAt = s.nowFn()
}

// computeTrailingStopFromHigh returns CurrentStopPrice for a
// trailing SELL-stop given the latest high-water mark. TrailAmount
// is preferred over TrailPercent when both are set, matching IBKR /
// Alpaca conventions.
func computeTrailingStopFromHigh(hw float64, req PlaceOrderRequest) float64 {
	if hw <= 0 {
		return 0
	}
	if req.TrailAmount > 0 {
		return hw - req.TrailAmount
	}
	if req.TrailPercent > 0 {
		return hw * (1 - req.TrailPercent)
	}
	return 0
}

// computeTrailingStopFromLow is the BUY-stop mirror.
func computeTrailingStopFromLow(lw float64, req PlaceOrderRequest) float64 {
	if lw <= 0 {
		return 0
	}
	if req.TrailAmount > 0 {
		return lw + req.TrailAmount
	}
	if req.TrailPercent > 0 {
		return lw * (1 + req.TrailPercent)
	}
	return 0
}

// StopShouldFire reports whether a quote breaches the trigger of a
// pending stop. Buy-stops fire when last >= stop; sell-stops fire
// when last <= stop. Returns false on quote.Last <= 0 to keep
// pre-open / halted instruments quiet.
//
// This is exported (not really — lowercase) so the trigger engine in
// internal/stoptrigger can share the canonical predicate without
// duplicating it.
func StopShouldFire(side Side, currentStopPrice, last float64) bool {
	if currentStopPrice <= 0 || last <= 0 {
		return false
	}
	switch side {
	case SideBuy:
		return last >= currentStopPrice
	case SideSell:
		return last <= currentStopPrice
	}
	return false
}
