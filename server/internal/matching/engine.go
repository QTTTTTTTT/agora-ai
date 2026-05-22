// Package matching provides composable building blocks for simulating order
// matching: an order/fill model, pluggable slippage estimators, and fee
// models. The current trading runtime fills plan actions at the most recent
// quote price with fixed-rate fees. PR-05 introduces these abstractions so
// future PRs (real backtesting / live trading / forward-test) can swap in
// realistic spread- and impact-aware execution without rewriting wiring code.
package matching

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// Side is the direction of an order.
type Side string

const (
	SideBuy  Side = "buy"
	SideSell Side = "sell"
)

// Normalized returns the lower-cased side or empty if it is not a recognised
// value.
func NormalizeSide(s string) Side {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "buy", "long":
		return SideBuy
	case "sell", "short":
		return SideSell
	}
	return ""
}

// Order is a single execution intent. LimitPrice == 0 means market order.
type Order struct {
	InstrumentKey string
	Side          Side
	Quantity      float64
	LimitPrice    float64
	// AssetClass is used by fee and slippage models to switch behaviour
	// (equity / futures / crypto). It is the lower-cased asset class string
	// used elsewhere in the codebase.
	AssetClass string
	// ContractMultiplier is meaningful for futures; equity/crypto orders
	// should leave it 0 or 1.
	ContractMultiplier float64
}

// Quote is the lightweight market snapshot the matching engine needs. Only
// Last is required; Bid/Ask enable spread-aware slippage models.
type Quote struct {
	Last float64
	Bid  float64
	Ask  float64
}

// HasSpread reports whether both bid and ask are populated and form a valid
// spread.
func (q Quote) HasSpread() bool {
	return q.Bid > 0 && q.Ask > 0 && q.Ask >= q.Bid
}

// MidPrice returns the mid of bid/ask if both are present, otherwise Last.
func (q Quote) MidPrice() float64 {
	if q.HasSpread() {
		return (q.Bid + q.Ask) / 2
	}
	return q.Last
}

// Fill is the realised execution of an order.
type Fill struct {
	Quantity    float64
	Price       float64
	Commission  float64
	StampTax    float64
	TransferFee float64
	// Notional is Quantity * Price * ContractMultiplier (for futures the
	// caller may multiply by multiplier; equity callers leave multiplier=1).
	Notional float64
	// SlippageBps records the bps of slippage that was applied to the
	// reference price. Useful for observability/reporting.
	SlippageBps float64
}

// TotalFees returns the sum of all fee components.
func (f Fill) TotalFees() float64 {
	return f.Commission + f.StampTax + f.TransferFee
}

// Engine is the high-level matching entry point. Implementations compose a
// SlippageModel and a FeeModel to turn an Order + Quote into a Fill.
type Engine interface {
	Match(order Order, quote Quote) (Fill, error)
}

// SlippageModel returns the price the order is expected to fill at given the
// current quote and order direction. Implementations should never return a
// negative price; they may fall back to quote.Last when bid/ask are missing.
type SlippageModel interface {
	FillPrice(order Order, quote Quote) float64
}

// FeeModel returns the commission, stamp tax and transfer fee charged for a
// given order and fill price. Different markets (A-share / US-equity /
// futures / crypto) plug in different fee tables here.
type FeeModel interface {
	Fees(order Order, fillPrice float64) (commission, stamp, transfer float64)
}

// ErrInvalidOrder is returned when the order fails basic sanity checks.
var ErrInvalidOrder = errors.New("matching: invalid order")

// ErrNoQuote is returned when neither last nor bid/ask is usable.
var ErrNoQuote = errors.New("matching: quote unavailable")

// ErrLimitNotMarketable is returned when a limit order's limit is not
// crossable against the current quote.
var ErrLimitNotMarketable = errors.New("matching: limit price not marketable")

// MarketableEngine is the default engine. It applies the slippage model first
// (which typically lifts the buy price toward ask and drops the sell price
// toward bid), then the fee model. Limit orders are honoured: if a buy limit
// is below the post-slippage price, the fill is rejected; vice versa for
// sells.
type MarketableEngine struct {
	Slippage SlippageModel
	Fees     FeeModel
}

// NewDefaultEngine returns the engine that exactly reproduces the current
// runtime behaviour (zero slippage + fixed rate equity fees). Callers can
// substitute alternative components for backtests / forward tests.
func NewDefaultEngine() *MarketableEngine {
	return &MarketableEngine{
		Slippage: ZeroSlippage{},
		Fees:     FixedRateEquityFees{},
	}
}

// Match implements Engine.
func (e *MarketableEngine) Match(order Order, quote Quote) (Fill, error) {
	if order.Quantity <= 0 {
		return Fill{}, fmt.Errorf("%w: non-positive quantity", ErrInvalidOrder)
	}
	if order.Side != SideBuy && order.Side != SideSell {
		return Fill{}, fmt.Errorf("%w: side must be buy or sell, got %q", ErrInvalidOrder, order.Side)
	}
	if quote.Last <= 0 && !quote.HasSpread() {
		return Fill{}, ErrNoQuote
	}

	slippage := e.Slippage
	if slippage == nil {
		slippage = ZeroSlippage{}
	}
	fees := e.Fees
	if fees == nil {
		fees = FixedRateEquityFees{}
	}

	reference := quote.Last
	if reference <= 0 {
		reference = quote.MidPrice()
	}
	fillPrice := slippage.FillPrice(order, quote)
	if fillPrice <= 0 {
		return Fill{}, ErrNoQuote
	}

	if order.LimitPrice > 0 {
		switch order.Side {
		case SideBuy:
			if fillPrice > order.LimitPrice {
				return Fill{}, ErrLimitNotMarketable
			}
		case SideSell:
			if fillPrice < order.LimitPrice {
				return Fill{}, ErrLimitNotMarketable
			}
		}
	}

	commission, stamp, transfer := fees.Fees(order, fillPrice)
	multiplier := order.ContractMultiplier
	if multiplier <= 0 {
		multiplier = 1
	}
	notional := order.Quantity * fillPrice * multiplier

	slippageBps := 0.0
	if reference > 0 {
		slippageBps = (fillPrice - reference) / reference * 10000
		if order.Side == SideSell {
			slippageBps = -slippageBps
		}
	}

	return Fill{
		Quantity:    order.Quantity,
		Price:       fillPrice,
		Commission:  commission,
		StampTax:    stamp,
		TransferFee: transfer,
		Notional:    notional,
		SlippageBps: math.Round(slippageBps*100) / 100,
	}, nil
}
