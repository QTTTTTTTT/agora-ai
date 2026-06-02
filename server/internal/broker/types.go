// Package broker abstracts the execution venue (in-process simulator,
// paper-trading broker, live broker REST/FIX adapter) behind a single
// stable interface. Every order routing path in the platform should
// flow through this interface so swapping a simulator for a live
// broker is a one-file change.
//
// This file defines the data model. The interface itself lives in
// broker.go; the in-process Simulator implementation lives in
// simulator.go.
//
// Design notes:
//
//   - PlaceOrderRequest.ClientOrderID is REQUIRED. The caller mints
//     a stable id (typically derived from plan_action_id + retry
//     counter) and the broker guarantees idempotent semantics for
//     duplicate calls. This is the foundation for P0-4 (idempotency
//     middleware) — adding the contract here means the Simulator
//     never books the same order twice even before the higher layer
//     enforces the constraint.
//
//   - OrderType currently only honours market / limit in the
//     Simulator. The remaining values (stop / stop_limit /
//     trailing_stop / mo[c|o] / iceberg) are part of the contract so
//     future PRs (P0-2 schema, P0-3 stop-trigger engine) can extend
//     without a breaking interface change.
//
//   - Fees are returned as a structured Fees value so the
//     reconciliation layer (P1-3) can attribute commission / stamp /
//     transfer separately when matching against broker statements.
package broker

import (
	"time"
)

// Side enumerates the order direction. Strings are deliberately
// lower-case to match every other side serialisation in the
// codebase (matching.Side, repository.TradeExecution.Side).
type Side string

const (
	SideBuy  Side = "buy"
	SideSell Side = "sell"
)

// IsValid reports whether s is a recognised side.
func (s Side) IsValid() bool {
	return s == SideBuy || s == SideSell
}

// OrderType enumerates the order types the platform models. Not
// every venue supports every type — callers should consult
// Broker.Capabilities() before submitting.
type OrderType string

const (
	OrderTypeMarket       OrderType = "market"
	OrderTypeLimit        OrderType = "limit"
	OrderTypeStop         OrderType = "stop"
	OrderTypeStopLimit    OrderType = "stop_limit"
	OrderTypeTrailingStop OrderType = "trailing_stop"
	// OrderTypeMOC is "market on close" — submitted intra-day, fills
	// at the closing auction.
	OrderTypeMOC OrderType = "moc"
	// OrderTypeMOO is "market on open" — submitted before the
	// session, fills at the opening auction.
	OrderTypeMOO OrderType = "moo"
	// OrderTypeIceberg is a limit order with hidden size. DisplayQty
	// controls the visible portion.
	OrderTypeIceberg OrderType = "iceberg"
)

// IsValid reports whether t is a known order type.
func (t OrderType) IsValid() bool {
	switch t {
	case OrderTypeMarket, OrderTypeLimit, OrderTypeStop, OrderTypeStopLimit,
		OrderTypeTrailingStop, OrderTypeMOC, OrderTypeMOO, OrderTypeIceberg:
		return true
	}
	return false
}

// RequiresLimitPrice reports whether the order type needs a non-zero
// LimitPrice on the request. Stop-limit needs both LimitPrice and
// StopPrice.
func (t OrderType) RequiresLimitPrice() bool {
	return t == OrderTypeLimit || t == OrderTypeStopLimit || t == OrderTypeIceberg
}

// RequiresStopPrice reports whether the order type needs a non-zero
// StopPrice on the request.
func (t OrderType) RequiresStopPrice() bool {
	return t == OrderTypeStop || t == OrderTypeStopLimit
}

// RequiresTrailParams reports whether the order type needs a
// trailing-stop trigger expression (TrailAmount or TrailPercent must
// be > 0).
func (t OrderType) RequiresTrailParams() bool {
	return t == OrderTypeTrailingStop
}

// IsStopType reports whether the order type carries trigger
// semantics — i.e. the order rests in OrderStatePending until a stop
// price is breached and is then fired by the stop-trigger engine.
// Stop / stop-limit / trailing-stop all return true.
func (t OrderType) IsStopType() bool {
	return t == OrderTypeStop || t == OrderTypeStopLimit || t == OrderTypeTrailingStop
}

// TimeInForce controls when the order is eligible to fill and when
// it expires.
type TimeInForce string

const (
	// TIFDay expires at the end of the current trading session.
	TIFDay TimeInForce = "day"
	// TIFGTC stays working until cancelled.
	TIFGTC TimeInForce = "gtc"
	// TIFIOC fills immediately whatever it can; cancels the rest.
	TIFIOC TimeInForce = "ioc"
	// TIFFOK fills the entire quantity immediately or cancels.
	TIFFOK TimeInForce = "fok"
	// TIFGTD stays working until the GoodTillDate timestamp.
	TIFGTD TimeInForce = "gtd"
	// TIFOPG participates in the opening auction only.
	TIFOPG TimeInForce = "opg"
)

// IsValid reports whether t is a known time-in-force.
func (t TimeInForce) IsValid() bool {
	switch t {
	case TIFDay, TIFGTC, TIFIOC, TIFFOK, TIFGTD, TIFOPG:
		return true
	}
	return false
}

// PositionSide is meaningful for futures and short-equity. Spot
// equity orders should leave it empty (or "long").
type PositionSide string

const (
	PositionSideLong  PositionSide = "long"
	PositionSideShort PositionSide = "short"
)

// PlaceOrderRequest is the payload accepted by Broker.PlaceOrder.
type PlaceOrderRequest struct {
	// ClientOrderID is a caller-minted unique id for this order
	// intent. The broker uses it as an idempotency key: a duplicate
	// PlaceOrder with the same (FundID, ClientOrderID) pair returns
	// the existing order without booking a new one.
	ClientOrderID string

	// FundID identifies which fund's cash / position pool the order
	// trades against. Required.
	FundID string

	// InstrumentKey is the platform-wide canonical key
	// (e.g. "us-equity:AAPL"). Required for routing fees, slippage
	// and quotes; the simulator passes it straight through to the
	// matching engine.
	InstrumentKey string

	// Symbol is the venue-native ticker (e.g. "AAPL", "0700.HK",
	// "600519.SS").
	Symbol string

	// Market identifies the venue ("a-share", "hk", "us",
	// "futures-cn", "crypto"). Used by capability checks and by the
	// fee model when routing.
	Market string

	// AssetClass is "equity" / "futures" / "crypto" — used by the
	// fee/slippage models inside the simulator.
	AssetClass string

	// ContractMultiplier is meaningful for futures. Equity / crypto
	// callers leave it 0 or 1.
	ContractMultiplier float64

	Side      Side
	OrderType OrderType

	// Quantity is the requested order size in contracts (futures)
	// or shares/units (equity/crypto). Must be > 0.
	Quantity float64

	// LimitPrice is required for limit / stop-limit / iceberg.
	LimitPrice float64

	// StopPrice is required for stop / stop-limit. The Simulator
	// rejects stop-typed orders directly; the stop-trigger engine
	// (P0-3) is the component that monitors quotes and re-submits
	// triggered orders as market/limit.
	StopPrice float64

	// TrailAmount is the absolute trailing distance (price units)
	// for trailing-stop orders. Mutually exclusive with TrailPercent.
	TrailAmount float64

	// TrailPercent is the trailing distance expressed as a fraction
	// (0.05 = 5%). Mutually exclusive with TrailAmount.
	TrailPercent float64

	// DisplayQty is the visible portion of an iceberg order. Must
	// be > 0 and ≤ Quantity when OrderType is iceberg.
	DisplayQty float64

	TimeInForce  TimeInForce
	GoodTillDate time.Time

	// ReduceOnly limits the order to closing or reducing the
	// existing position. Honoured by futures venues; the Simulator
	// records it but does not enforce it (positions live in the
	// repository layer, not in the broker).
	ReduceOnly bool

	// PositionSide is "long" / "short" for futures or short-equity
	// orders. Empty for spot long-only.
	PositionSide PositionSide

	// Tag is a free-form caller-supplied label (typically
	// plan_action_id) returned unchanged on Order.Request so callers
	// can correlate fills back to their own domain rows.
	Tag string

	// Metadata is opaque caller-supplied k/v that round-trips with
	// the order. Useful for live brokers that accept tag fields on
	// the wire; the Simulator stores it for reconciliation but does
	// not act on it.
	Metadata map[string]string
}

// CancelOrderRequest cancels a working order.
type CancelOrderRequest struct {
	FundID        string
	BrokerOrderID string
	// ClientOrderID is accepted as a fallback identifier when the
	// caller didn't persist the BrokerOrderID. At least one of
	// (BrokerOrderID, ClientOrderID) must be set.
	ClientOrderID string
}

// ReplaceOrderRequest modifies a working order's quantity or price
// fields atomically (no cancel-and-replace race window). Pointer
// fields are nil-as-no-change.
type ReplaceOrderRequest struct {
	FundID          string
	BrokerOrderID   string
	ClientOrderID   string
	NewQuantity     *float64
	NewLimitPrice   *float64
	NewStopPrice    *float64
	NewTrailAmount  *float64
	NewTrailPercent *float64
	NewDisplayQty   *float64
}

// OrderState is the current lifecycle state of an order.
type OrderState string

const (
	// OrderStatePending: accepted by the broker, not yet active in
	// the order book (e.g. stop awaiting trigger, MOC awaiting
	// close).
	OrderStatePending OrderState = "pending"
	// OrderStateWorking: resting in the order book.
	OrderStateWorking OrderState = "working"
	// OrderStateTriggered: a stop-typed order whose trigger fired;
	// it has been converted to a market/limit child and is now
	// working/filled.
	OrderStateTriggered OrderState = "triggered"
	// OrderStatePartial: some quantity filled, remainder is working.
	OrderStatePartial OrderState = "partial"
	// OrderStateFilled: terminal — fully filled.
	OrderStateFilled OrderState = "filled"
	// OrderStateCancelled: terminal — cancelled by user or by IOC/FOK
	// non-marketable rule.
	OrderStateCancelled OrderState = "cancelled"
	// OrderStateRejected: terminal — rejected by validation, risk,
	// or broker (e.g. limit not marketable + IOC, insufficient
	// cash).
	OrderStateRejected OrderState = "rejected"
	// OrderStateExpired: terminal — TIF passed without a fill.
	OrderStateExpired OrderState = "expired"
)

// IsTerminal reports whether the order has reached an end state and
// can no longer transition.
func (s OrderState) IsTerminal() bool {
	switch s {
	case OrderStateFilled, OrderStateCancelled, OrderStateRejected, OrderStateExpired:
		return true
	}
	return false
}

// Fees is the broker-reported execution cost.
type Fees struct {
	Commission  float64
	StampTax    float64
	TransferFee float64
	// Currency is the ISO code in which the fees are denominated
	// ("USD", "HKD", "CNY"). Empty means "same as instrument quote
	// currency".
	Currency string
}

// Total returns the sum of all fee components.
func (f Fees) Total() float64 {
	return f.Commission + f.StampTax + f.TransferFee
}

// Order is the broker-side authoritative state of an order.
type Order struct {
	BrokerOrderID  string
	ClientOrderID  string
	Request        PlaceOrderRequest
	State          OrderState
	FilledQuantity float64
	AvgFillPrice   float64
	LastFillAt     time.Time
	PlacedAt       time.Time
	UpdatedAt      time.Time
	Fees           Fees
	// SlippageBps records the bps drift between the reference price
	// (typically last/mid) and the actual fill price. Useful for
	// observability; matches matching.Fill.SlippageBps semantics.
	SlippageBps float64
	// RejectReason is human-readable; populated only when State is
	// OrderStateRejected or OrderStateCancelled (with a cancel
	// reason).
	RejectReason string

	// CurrentStopPrice is the live trigger price for stop /
	// stop_limit / trailing_stop orders. For non-trailing stops it
	// equals Request.StopPrice and never changes. For trailing
	// stops it is ratcheted by the stop-trigger engine as the
	// market moves favourably; observers should ALWAYS read this
	// field rather than Request.StopPrice when checking the live
	// trigger level.
	CurrentStopPrice float64

	// TrailingHighWater is the highest Last seen on the instrument
	// since this order was placed. Used by trailing sell-stops to
	// ratchet CurrentStopPrice upwards.
	TrailingHighWater float64

	// TrailingLowWater is the lowest Last seen on the instrument
	// since this order was placed. Used by trailing buy-stops to
	// ratchet CurrentStopPrice downwards.
	TrailingLowWater float64

	// TriggeredChildOrderID is the BrokerOrderID of the child
	// order placed when this stop order's trigger fired. Empty on
	// non-stop orders or stops that have not yet fired. Populated
	// before the parent transitions to OrderStateTriggered so the
	// link is observable as soon as the state change is visible.
	TriggeredChildOrderID string

	// ParentBrokerOrderID is set on a child order created by the
	// stop-trigger engine when a stop fires. It links back to the
	// parent stop so the lifecycle pair is reconstructable from
	// either side.
	ParentBrokerOrderID string

	// Warnings carries advisory messages from the pre-trade
	// market-status gate (e.g. "stale quote", "half-day session
	// in effect"). Empty on a normal allow. The fill itself
	// proceeds; warnings are informational only and ride on the
	// Order so the caller / UI can render them next to the row.
	Warnings []string
}

// IsOpen reports whether the order can still receive fills /
// cancels.
func (o *Order) IsOpen() bool {
	return o != nil && !o.State.IsTerminal()
}

// Fill is a single execution event. Multiple fills may roll up to
// one Order; a market order in the Simulator emits exactly one.
type Fill struct {
	BrokerOrderID string
	ClientOrderID string
	FundID        string
	Quantity      float64
	Price         float64
	OccurredAt    time.Time
	Fees          Fees
	// Liquidity is "maker" / "taker" / "" if unknown. Simulator
	// market fills are always taker; resting limit fills (when
	// implemented) are maker.
	Liquidity string
	// SlippageBps mirrors Order.SlippageBps for this individual
	// fill.
	SlippageBps float64
}

// CashBalance is one row of an account snapshot.
type CashBalance struct {
	Currency string
	Amount   float64
}

// PositionRow is one row of an account snapshot.
type PositionRow struct {
	InstrumentKey string
	Symbol        string
	Market        string
	Quantity      float64
	AvgCost       float64
	Currency      string
	PositionSide  PositionSide
}

// AccountSnapshot is the broker-reported account state used by the
// daily reconciliation loop (P1-3). The Simulator does not own
// position state — it returns ErrUnsupported. Live brokers populate
// this from the broker statement endpoint.
type AccountSnapshot struct {
	FundID    string
	BrokerID  string
	AsOf      time.Time
	Cash      []CashBalance
	Positions []PositionRow
}
