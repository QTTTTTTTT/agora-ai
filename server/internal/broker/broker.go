// Package broker — interface contract and shared errors.
//
// See types.go for the data model.
package broker

import (
	"context"
	"errors"
)

// Capabilities advertises which features a Broker implementation
// supports. Routers (and tests) should consult Capabilities() before
// submitting an order so that an unsupported request fails fast with
// a clear error rather than a generic ErrUnsupportedOrderType deep
// inside the venue.
type Capabilities struct {
	// Name is the broker identifier ("simulator", "longport",
	// "ibkr", ...). Returned for logging and dashboards; matches
	// Broker.Name().
	Name string

	// OrderTypes lists the OrderType values the broker accepts.
	OrderTypes []OrderType

	// TimeInForces lists the TimeInForce values the broker honours.
	TimeInForces []TimeInForce

	// AssetClasses lists the asset classes the broker can route
	// (e.g. {"equity", "futures"}).
	AssetClasses []string

	// Markets lists the venues / markets the broker can route to
	// ("us", "hk", "a-share", "futures-cn", "crypto"). Empty means
	// "ask the broker per order".
	Markets []string

	// SupportsCancel/SupportsReplace report whether the broker can
	// cancel or atomically replace working orders. The Simulator
	// supports cancel today; replace lands with P0-5.
	SupportsCancel  bool
	SupportsReplace bool

	// SupportsStream reports whether StreamFills emits events
	// (true) or always returns ErrUnsupported (false).
	SupportsStream bool

	// SupportsShort reports whether short selling is permitted on
	// equity. Live brokers gate this on borrow availability — the
	// platform's own pre-trade risk layer also checks. The
	// Simulator returns false today; short-sell with borrow
	// modelling lands in S6 (P2-5).
	SupportsShort bool

	// SupportsMargin reports whether the broker accounts margin /
	// leverage on equity. Futures already imply leverage; this flag
	// is about equity buying power.
	SupportsMargin bool

	// SupportsAccountSnapshot reports whether GetAccountSnapshot
	// returns the broker-reported state (live broker) or always
	// errors (simulator: the platform itself is the source of
	// truth, so reconciliation against the simulator is a no-op).
	SupportsAccountSnapshot bool
}

// HasOrderType reports whether t is in c.OrderTypes.
func (c Capabilities) HasOrderType(t OrderType) bool {
	for _, ot := range c.OrderTypes {
		if ot == t {
			return true
		}
	}
	return false
}

// HasTimeInForce reports whether tif is in c.TimeInForces.
func (c Capabilities) HasTimeInForce(tif TimeInForce) bool {
	for _, t := range c.TimeInForces {
		if t == tif {
			return true
		}
	}
	return false
}

// Broker is the unified interface every execution venue
// (Simulator, paper-trading broker, live broker REST/FIX adapter)
// implements. All order routing in the platform should go through
// this interface so that swapping a venue is a one-file change.
//
// Implementations MUST be safe for concurrent use from multiple
// goroutines.
type Broker interface {
	// Name returns the broker identifier for logs/metrics.
	Name() string

	// Capabilities reports the broker's supported feature set.
	Capabilities() Capabilities

	// PlaceOrder submits an order. The returned Order reflects the
	// broker's authoritative state at the moment the call returns
	// — this may already be terminal (filled / rejected) for
	// market-style orders, or non-terminal (pending / working) for
	// stop-typed or non-marketable resting orders.
	//
	// PlaceOrder is idempotent on (Request.FundID,
	// Request.ClientOrderID): a duplicate call with the same key
	// returns the existing Order without booking a second one.
	// Implementations MUST honour this.
	PlaceOrder(ctx context.Context, req PlaceOrderRequest) (*Order, error)

	// CancelOrder cancels a working order. Returns nil if the
	// cancel was accepted (the order will transition to
	// OrderStateCancelled), ErrOrderNotFound if no matching order
	// exists, or ErrOrderTerminal if the order has already reached
	// a terminal state.
	CancelOrder(ctx context.Context, req CancelOrderRequest) error

	// ReplaceOrder atomically modifies a working order. Pointer
	// fields are nil-as-no-change. Returns the updated Order.
	// Implementations that don't support replace return
	// ErrUnsupported.
	ReplaceOrder(ctx context.Context, req ReplaceOrderRequest) (*Order, error)

	// GetOrder returns the current state of an order.
	GetOrder(ctx context.Context, fundID, brokerOrderID string) (*Order, error)

	// GetOrderByClientID returns the current state of an order
	// looked up by its caller-minted client_order_id. Useful for
	// the order-replay loop on restart (P1-5) where we may have
	// the client id but not the broker id.
	GetOrderByClientID(ctx context.Context, fundID, clientOrderID string) (*Order, error)

	// ListOpenOrders returns all non-terminal orders for the fund.
	ListOpenOrders(ctx context.Context, fundID string) ([]Order, error)

	// StreamFills returns a channel that emits each new fill as it
	// occurs. The channel is closed when ctx is cancelled.
	// Implementations that don't support streaming return
	// ErrUnsupported. Synchronous-fill simulators MAY emit fills
	// to the channel from inside PlaceOrder, but a subscriber that
	// only attaches AFTER PlaceOrder will not see them — for the
	// simulator the canonical record is the Order returned by
	// PlaceOrder, and StreamFills is best-effort observability.
	StreamFills(ctx context.Context, fundID string) (<-chan Fill, error)

	// GetAccountSnapshot returns the broker's authoritative view
	// of the fund's positions and cash, used by the daily
	// reconciliation loop. Implementations that aren't the source
	// of truth (the Simulator: the platform is) return
	// ErrUnsupported.
	GetAccountSnapshot(ctx context.Context, fundID string) (*AccountSnapshot, error)
}

// Errors returned by Broker implementations. Callers should compare
// with errors.Is rather than string-match.
var (
	// ErrInvalidRequest is returned when the request fails basic
	// validation (missing fund id, missing client order id, zero
	// quantity, ...). The error message includes the specific
	// field; callers wrap with their own context.
	ErrInvalidRequest = errors.New("broker: invalid request")

	// ErrUnsupportedOrderType is returned when the request asks
	// for an OrderType that this broker does not advertise via
	// Capabilities().
	ErrUnsupportedOrderType = errors.New("broker: unsupported order type")

	// ErrUnsupportedTimeInForce is returned when the request asks
	// for a TimeInForce that this broker does not advertise.
	ErrUnsupportedTimeInForce = errors.New("broker: unsupported time-in-force")

	// ErrUnsupportedAssetClass is returned when the broker can't
	// route the asset class.
	ErrUnsupportedAssetClass = errors.New("broker: unsupported asset class")

	// ErrUnsupported is returned by methods that the implementation
	// does not provide (e.g. Simulator.GetAccountSnapshot).
	ErrUnsupported = errors.New("broker: operation unsupported")

	// ErrOrderNotFound means the (fund_id, broker_order_id) or
	// (fund_id, client_order_id) lookup did not match.
	ErrOrderNotFound = errors.New("broker: order not found")

	// ErrOrderTerminal means an attempt to cancel/replace an order
	// that is already in a terminal state.
	ErrOrderTerminal = errors.New("broker: order is in a terminal state")

	// ErrLimitNotMarketable is returned for IOC/FOK limit orders
	// whose limit is on the wrong side of the current quote (the
	// broker has nothing to fill against immediately).
	ErrLimitNotMarketable = errors.New("broker: limit price not marketable")

	// ErrNoQuote means the broker could not source a quote for the
	// instrument and therefore cannot price the order.
	ErrNoQuote = errors.New("broker: quote unavailable")

	// ErrMarketStatusRejected means the pre-trade market-status
	// gate rejected the order (halted symbol, suspended,
	// price-limit breach, market closed, half-day after early
	// close, etc). The wrapping reason ("halted: news pending",
	// "price 1700 below lower limit 1800") rides on the error
	// string so the caller can show the operator a useful message.
	ErrMarketStatusRejected = errors.New("broker: rejected by market-status gate")

	// ErrLockupRejected means the pre-trade lock-up gate
	// rejected a sell because the requested quantity exceeds
	// the unlocked qty (= position - sum(active locked qty)).
	// The wrapping reason includes the next-unlock timestamp
	// when known, e.g. "locked: order requires 100 but only 40
	// unlocked, next unlock at 2026-12-01T00:00:00Z".
	ErrLockupRejected = errors.New("broker: rejected by lock-up gate")

	// ErrBorrowRejected means the pre-trade borrow / locate
	// gate rejected a short order because the security is
	// unborrowable or the requested qty exceeds the available
	// borrowable supply. The wrapping reason carries the
	// specifics ("only 50000 shares available, requested 100000").
	ErrBorrowRejected = errors.New("broker: rejected by borrow gate")

	// ErrInsufficientCash and ErrInsufficientPosition are reserved
	// for live brokers; the Simulator does not enforce balance
	// checks (the platform's risk layer does that upstream). They
	// are part of the contract here so that live-broker adapters
	// can return them and shared error-handling code can route on
	// them.
	ErrInsufficientCash     = errors.New("broker: insufficient cash")
	ErrInsufficientPosition = errors.New("broker: insufficient position")
)
