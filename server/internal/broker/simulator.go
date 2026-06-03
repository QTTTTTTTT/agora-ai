// Package broker — in-process Simulator implementation.
//
// The Simulator is the default Broker for the platform. It executes
// every order against an in-process matching.Engine using cached
// market quotes. Orders, fills and idempotency state live in
// memory; persistence is the responsibility of the caller (today
// the runtimeTradingEngine writes trades to the trade repo right
// after PlaceOrder returns).
//
// Concurrency
//
// All exported methods are safe for concurrent use. Internal state
// (orders map, idempotency map, fill subscriber list) is protected
// by a single sync.Mutex. The lock is not held across user
// callbacks (QuoteFn, fill subscriber sends) so a slow subscriber
// cannot block PlaceOrder.
//
// Idempotency
//
// (FundID, ClientOrderID) is the idempotency key. A second
// PlaceOrder with the same pair returns the previously-booked Order
// without invoking the matching engine and without emitting a
// duplicate Fill. An empty ClientOrderID is rejected.
package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/fundai/server/internal/matching"
)

// QuoteFn returns the current quote for an instrument. The
// Simulator calls it every PlaceOrder so the matching engine has a
// fresh price; live brokers do their own quote sourcing and don't
// need this hook.
type QuoteFn func(ctx context.Context, instrumentKey, symbol, market string) (matching.Quote, error)

// SimulatorOption configures a Simulator at construction time.
type SimulatorOption func(*Simulator)

// WithIDGenerator overrides the broker_order_id generator. Default
// is uuid.NewString. Useful for deterministic tests.
func WithIDGenerator(fn func() string) SimulatorOption {
	return func(s *Simulator) {
		if fn != nil {
			s.idFn = fn
		}
	}
}

// WithClock overrides the wall-clock used for PlacedAt /
// UpdatedAt / OccurredAt. Default is time.Now.
func WithClock(fn func() time.Time) SimulatorOption {
	return func(s *Simulator) {
		if fn != nil {
			s.nowFn = fn
		}
	}
}

// WithMatchEngine overrides the matching.Engine used to price
// orders. Default is matching.NewDefaultEngine() (zero-slippage,
// fixed-rate equity fees).
func WithMatchEngine(eng matching.Engine) SimulatorOption {
	return func(s *Simulator) {
		if eng != nil {
			s.matchEngine = eng
		}
	}
}

// WithStreamBufferSize sets the per-subscriber channel buffer. A
// slow subscriber whose buffer fills will start dropping fills
// silently — the canonical record is always Order returned by
// PlaceOrder.
func WithStreamBufferSize(n int) SimulatorOption {
	return func(s *Simulator) {
		if n > 0 {
			s.streamBuf = n
		}
	}
}

// MarketStatusGate is the optional pre-match check the simulator
// runs against an order. Implementations consult the market-status
// table (halt / suspended / price-limit / stale-quote / calendar)
// and return:
//
//   - rejected=true and an error → the order is rejected with the
//     given reason; PlaceOrder returns the error;
//   - rejected=false and a non-empty warnings slice → the order
//     proceeds but the warnings ride on the Order so the caller
//     can surface them;
//   - rejected=false and empty warnings → silent allow.
//
// Decoupling via an interface keeps the broker package out of
// the marketstatus dependency graph; production wiring lives in
// cmd/server. nil gate (default) → behavior identical to today's
// simulator.
type MarketStatusGate interface {
	// CheckOrder runs the pre-trade check. probe carries the
	// order details (instrument, side, intended price). Returns
	// an error → reject; warnings are advisory.
	CheckOrder(ctx context.Context, probe MarketStatusProbe) MarketStatusVerdict
}

// MarketStatusProbe is the lightweight payload the gate sees.
// We deliberately do NOT pass the full PlaceOrderRequest so the
// gate signature stays stable across order-type evolution.
type MarketStatusProbe struct {
	FundID         string
	InstrumentKey  string
	Symbol         string
	Market         string
	AssetClass     string
	Side           string
	Quantity       float64
	IntendedPrice  float64
	ClientOrderID  string
}

// MarketStatusVerdict is the gate's reply.
type MarketStatusVerdict struct {
	Rejected     bool
	RejectReason string
	// Warnings ride on the placed order so the UI / risk layer
	// can render "executed despite stale quote" badges.
	Warnings     []string
}

// WithMarketStatusGate wires a pre-trade gate into the simulator.
// Default = nil (no gate), preserving the old behaviour.
func WithMarketStatusGate(gate MarketStatusGate) SimulatorOption {
	return func(s *Simulator) {
		s.gate = gate
	}
}

// LockupGate is the second pre-trade gate (S6.3). It runs
// after MarketStatusGate so the more dramatic halts / price
// limits take precedence in the reject reason. Lock-ups never
// block buys (the constraint is on disposing existing shares).
//
// Same Rejected / Warnings shape as MarketStatusVerdict so the
// PlaceOrder code path can fold both gates uniformly.
type LockupGate interface {
	CheckOrder(ctx context.Context, probe LockupProbe) LockupVerdict
}

// LockupProbe is the lock-up gate input.
type LockupProbe struct {
	FundID        string
	InstrumentKey string
	Symbol        string
	AssetClass    string
	Side          string
	Quantity      float64
	IntendedPrice float64
	ClientOrderID string
}

// LockupVerdict is the gate's reply. Mirrors
// MarketStatusVerdict so the simulator can compose the two
// pipelines without importing the lockup domain.
type LockupVerdict struct {
	Rejected     bool
	RejectReason string
	Warnings     []string
}

// WithLockupGate wires the lock-up gate into the simulator.
// Default = nil (no gate), preserving the old behaviour.
func WithLockupGate(gate LockupGate) SimulatorOption {
	return func(s *Simulator) {
		s.lockupGate = gate
	}
}

// BorrowGate is the third pre-trade gate (S6.4). It runs after
// the lock-up gate and only applies to orders that would
// **open or grow** a short position. The gate's job is to:
//
//   - reject when the security is unavailable / insufficient
//     borrowable supply
//   - charge the optional one-time locate fee on accepted
//     opens (the adapter is responsible for booking the fee
//     to the cash_ledger; the gate only signals intent)
//   - attach a warning when borrow is HARD or RESTRICTED so
//     the operator sees the financing premium
//
// The gate sees the broker's own `Side` + a hint about how
// many shares are about to be borrowed (`ShortQty`); the
// adapter computes ShortQty from the existing position
// (`max(0, order_qty - max(0, position_qty))`) so the
// non-short portion of a partially-shorting sell still goes
// through the lock-up rules naturally.
//
// Same Rejected / Warnings shape as the other two gates.
type BorrowGate interface {
	CheckOrder(ctx context.Context, probe BorrowProbe) BorrowVerdict
}

// BorrowProbe is the borrow gate input.
type BorrowProbe struct {
	FundID        string
	InstrumentKey string
	Symbol        string
	AssetClass    string
	Side          string
	Quantity      float64  // total order qty
	IntendedPrice float64
	ClientOrderID string
}

// BorrowVerdict mirrors the other gates' shape. LocateFee is
// passed back when allow path attaches a positive locate fee
// so future code can route the fee booking; the simulator
// today just forwards the warnings (booking is wired later in
// the cash_ledger integration).
type BorrowVerdict struct {
	Rejected     bool
	RejectReason string
	Warnings     []string
	LocateFee    float64
}

// WithBorrowGate wires the borrow gate into the simulator.
// Default = nil (no gate), preserving the old behaviour.
func WithBorrowGate(gate BorrowGate) SimulatorOption {
	return func(s *Simulator) {
		s.borrowGate = gate
	}
}

// PriceCollarGate is the fourth pre-trade gate. It runs last among
// the four (marketstatus → lockup → borrow → price-collar) so:
//
//   - hard-reject reasons (halted, calendar, lockup, borrow) keep
//     precedence in the surfaced RejectReason;
//   - the collar's reject metadata (intended vs reference price)
//     is only computed when the order would otherwise have made
//     it to the matcher.
//
// The collar's job is the broker-side fat-finger / bad-quote safety
// net: when the LIMIT price deviates from a recent reference quote
// by more than the configured tolerance (default 11% / 21% for
// A-share boards, 15% for US equity, 30% for crypto), the order
// gets rejected before the matcher books it. Trigger story for
// the gate's introduction was the 2026-06-02 301308 fill at
// 96,226.4188 CNY/share when the true mid was ~500 CNY — a PM
// fallback path had stamped the budget into the limit price.
type PriceCollarGate interface {
	CheckOrder(ctx context.Context, probe PriceCollarProbe) PriceCollarVerdict
}

// PriceCollarProbe is the gate input. We pass IntendedPrice=0 for
// market orders so the engine can short-circuit and allow.
type PriceCollarProbe struct {
	FundID        string
	InstrumentKey string
	Symbol        string
	Market        string
	AssetClass    string
	Side          string
	Quantity      float64
	IntendedPrice float64
	ClientOrderID string
}

// PriceCollarVerdict mirrors the other gates' shape. ReferencePrice
// and ToleranceBps are populated on reject for audit.
type PriceCollarVerdict struct {
	Rejected       bool
	RejectReason   string
	Warnings       []string
	ReferencePrice float64
	ToleranceBps   int
}

// WithPriceCollarGate wires the collar gate into the simulator.
// Default = nil (no gate). Production wiring lives in cmd/server.
func WithPriceCollarGate(gate PriceCollarGate) SimulatorOption {
	return func(s *Simulator) {
		s.priceCollarGate = gate
	}
}

// LotSizeGate is the fifth pre-trade gate. It runs LAST among the
// five gates (marketstatus → lockup → borrow → price-collar →
// lot-size) so hard-reject reasons keep precedence in the
// surfaced reason, and the lot-size verdict only needs to be
// computed for orders that would otherwise have reached the
// matcher.
//
// The gate's job is the broker-side market-microstructure safety
// net. Every venue has rules about the smallest legal trade unit
// and the increment above it:
//
//   - A-share SH/SZ main + ChiNext (300/301): MinLot 100, Step 100.
//   - A-share STAR (688/689): MinLot 200, Step 1.
//   - A-share BSE (43/83/87/88/92): MinLot 100, Step 1.
//   - Hong Kong equities: per-symbol lot table (00700=100,
//     00939=1000, 09988=100, …); odd-lot board handles residuals
//     but a regular order MUST be a lot multiple.
//   - US equities: integer shares by default; fractional shares
//     only when the venue's Capabilities advertise it.
//   - Futures (CN): integer "hands" of contract_multiplier.
//   - Crypto: per-pair step_size + min_notional.
//
// Sell-side semantics: an "odd-lot residual" sell that liquidates
// the remaining position is always legal (handled by the engine,
// not by short-circuiting the gate). A partial sell that would
// leave an odd-lot residual must be expanded to liquidate the
// whole position — the engine returns Rejected with a Suggestion
// quantity so the wiring layer can re-submit.
//
// Trigger story for the gate's introduction was the 2026-06-02/03
// bad fills: 301308 buy 1 share (ChiNext minimum 100) and
// 688195 sell 85 / 688205 sell 62 (STAR Market step 1 but the
// odd-lot residual rule was bypassed because no broker-side gate
// existed).
type LotSizeGate interface {
	CheckOrder(ctx context.Context, probe LotSizeProbe) LotSizeVerdict
}

// LotSizeProbe is the gate input. The probe deliberately does NOT
// carry the position quantity — the production gate implementation
// looks it up via its own positions repository so the broker
// package stays free of the positions domain. Tests can pass
// fakes that ignore the lookup.
//
// S12.5 — LimitPrice is included so the gate can also enforce the
// per-venue tick size on limit orders (A-share 0.01 CNY, US ≥$1
// 0.01 USD, HK banded by price tier, crypto per pair). 0 means
// "market order" and the tick check is skipped.
type LotSizeProbe struct {
	FundID         string
	InstrumentKey  string
	Symbol         string
	Market         string
	Exchange       string
	AssetClass     string
	InstrumentType string
	Side           string
	Quantity       float64
	LimitPrice     float64
	ClientOrderID  string
}

// LotSizeVerdict mirrors the other gates' shape. SuggestedQty is
// populated on reject when the engine can propose a legal
// quantity (e.g. floor to the step, or expand to liquidate the
// residual); callers can use it to re-submit programmatically
// without an extra round trip.
type LotSizeVerdict struct {
	Rejected     bool
	RejectReason string
	Warnings     []string
	SuggestedQty float64
}

// WithLotSizeGate wires the lot-size gate into the simulator.
// Default = nil (no gate), preserving the old behaviour. Production
// wiring lives in cmd/server.
func WithLotSizeGate(gate LotSizeGate) SimulatorOption {
	return func(s *Simulator) {
		s.lotSizeGate = gate
	}
}

// Simulator is an in-process Broker that fills orders against
// matching.Engine.
type Simulator struct {
	mu           sync.Mutex
	quoteFn      QuoteFn
	matchEngine  matching.Engine
	idFn         func() string
	nowFn        func() time.Time
	streamBuf    int
	gate            MarketStatusGate
	lockupGate      LockupGate
	borrowGate      BorrowGate
	priceCollarGate PriceCollarGate
	lotSizeGate     LotSizeGate
	orders       map[string]*Order        // brokerOrderID -> order
	clientIndex  map[string]string        // (fundID|clientOrderID) -> brokerOrderID
	subscribers  map[string][]chan Fill   // fundID -> subscribers
	openByFund   map[string]map[string]struct{} // fundID -> set of open brokerOrderID
}

// NewSimulator constructs a Simulator. quoteFn is required. opts
// may override the matching engine, clock, id generator and stream
// buffer size.
func NewSimulator(quoteFn QuoteFn, opts ...SimulatorOption) *Simulator {
	s := &Simulator{
		quoteFn:     quoteFn,
		matchEngine: matching.NewDefaultEngine(),
		idFn:        uuid.NewString,
		nowFn:       time.Now,
		streamBuf:   16,
		orders:      make(map[string]*Order),
		clientIndex: make(map[string]string),
		subscribers: make(map[string][]chan Fill),
		openByFund:  make(map[string]map[string]struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Name implements Broker.
func (s *Simulator) Name() string { return "simulator" }

// Capabilities implements Broker. The Simulator honours market,
// limit, stop, stop-limit, and trailing-stop orders. Stop-typed
// orders are accepted on PlaceOrder but rest in OrderStatePending
// until the internal/stoptrigger engine fires them — see
// simulator_stop.go for the venue-side trigger surface.
func (s *Simulator) Capabilities() Capabilities {
	return Capabilities{
		Name: "simulator",
		OrderTypes: []OrderType{
			OrderTypeMarket,
			OrderTypeLimit,
			OrderTypeStop,
			OrderTypeStopLimit,
			OrderTypeTrailingStop,
		},
		TimeInForces: []TimeInForce{TIFDay, TIFIOC, TIFFOK, TIFGTC},
		AssetClasses: []string{"equity", "futures", "crypto"},
		Markets:      []string{"a-share", "hk", "us", "futures-cn", "crypto"},
		// Cancel is supported (working limits sit in memory).
		SupportsCancel: true,
		// Replace shipped in P0-5.
		SupportsReplace: true,
		SupportsStream:  true,
		// Short / margin / account-snapshot belong to live brokers.
		SupportsShort:           false,
		SupportsMargin:          false,
		SupportsAccountSnapshot: false,
	}
}

// PlaceOrder implements Broker.
func (s *Simulator) PlaceOrder(ctx context.Context, req PlaceOrderRequest) (*Order, error) {
	if err := validatePlaceOrder(req); err != nil {
		return nil, err
	}
	caps := s.Capabilities()
	if !caps.HasOrderType(req.OrderType) {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedOrderType, req.OrderType)
	}
	if req.TimeInForce == "" {
		req.TimeInForce = TIFDay
	}
	if !caps.HasTimeInForce(req.TimeInForce) {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedTimeInForce, req.TimeInForce)
	}

	// Idempotent re-entry: a duplicate (FundID, ClientOrderID) returns
	// the previously-booked order. We hold the lock for the full
	// PlaceOrder call so a concurrent duplicate sees a fully-booked
	// order rather than a half-initialised one.
	idemKey := idempotencyKey(req.FundID, req.ClientOrderID)
	s.mu.Lock()
	if existingID, ok := s.clientIndex[idemKey]; ok {
		existing := s.orders[existingID]
		s.mu.Unlock()
		if existing == nil {
			return nil, ErrOrderNotFound
		}
		// Return a defensive copy so callers can't mutate our state.
		cp := *existing
		return &cp, nil
	}

	// Pre-trade market-status gate: halt / suspended / price-limit /
	// stale-quote / calendar. We run BEFORE booking the order so a
	// reject doesn't pollute the orders map / client index. The
	// gate is OPTIONAL — when nil the simulator behaves exactly
	// as before. Warnings (advisory) are attached to the order
	// once it's booked.
	var gateWarnings []string
	if s.gate != nil {
		intended := req.LimitPrice
		if intended <= 0 {
			intended = req.StopPrice
		}
		verdict := s.gate.CheckOrder(ctx, MarketStatusProbe{
			FundID:        req.FundID,
			InstrumentKey: req.InstrumentKey,
			Symbol:        req.Symbol,
			Market:        req.Market,
			AssetClass:    req.AssetClass,
			Side:          string(req.Side),
			Quantity:      req.Quantity,
			IntendedPrice: intended,
			ClientOrderID: req.ClientOrderID,
		})
		if verdict.Rejected {
			s.mu.Unlock()
			reason := verdict.RejectReason
			if reason == "" {
				reason = "rejected by market-status gate"
			}
			return nil, fmt.Errorf("%w: %s", ErrMarketStatusRejected, reason)
		}
		gateWarnings = append(gateWarnings, verdict.Warnings...)
	}

	// Pre-trade lock-up gate: blocks sells of locked shares
	// (S6.3). Runs after MarketStatusGate so a halted symbol
	// reports its halt reason rather than a lock-up reason. Lock-
	// ups are silently bypassed on buys; the gate returns
	// Rejected=false unconditionally for non-sell sides.
	if s.lockupGate != nil {
		intended := req.LimitPrice
		if intended <= 0 {
			intended = req.StopPrice
		}
		verdict := s.lockupGate.CheckOrder(ctx, LockupProbe{
			FundID:        req.FundID,
			InstrumentKey: req.InstrumentKey,
			Symbol:        req.Symbol,
			AssetClass:    req.AssetClass,
			Side:          string(req.Side),
			Quantity:      req.Quantity,
			IntendedPrice: intended,
			ClientOrderID: req.ClientOrderID,
		})
		if verdict.Rejected {
			s.mu.Unlock()
			reason := verdict.RejectReason
			if reason == "" {
				reason = "rejected by lock-up gate"
			}
			return nil, fmt.Errorf("%w: %s", ErrLockupRejected, reason)
		}
		gateWarnings = append(gateWarnings, verdict.Warnings...)
	}

	// Pre-trade borrow / locate gate (S6.4): rejects opens of
	// short positions when the security is unborrowable or the
	// borrowable supply is insufficient. Runs after market-status
	// and lock-up so the cleaner halt / lock-up reasons take
	// precedence in operator messaging. The gate is invoked for
	// every order — internally it short-circuits when the order
	// is purely closing an existing long (no borrow needed).
	if s.borrowGate != nil {
		intended := req.LimitPrice
		if intended <= 0 {
			intended = req.StopPrice
		}
		verdict := s.borrowGate.CheckOrder(ctx, BorrowProbe{
			FundID:        req.FundID,
			InstrumentKey: req.InstrumentKey,
			Symbol:        req.Symbol,
			AssetClass:    req.AssetClass,
			Side:          string(req.Side),
			Quantity:      req.Quantity,
			IntendedPrice: intended,
			ClientOrderID: req.ClientOrderID,
		})
		if verdict.Rejected {
			s.mu.Unlock()
			reason := verdict.RejectReason
			if reason == "" {
				reason = "rejected by borrow gate"
			}
			return nil, fmt.Errorf("%w: %s", ErrBorrowRejected, reason)
		}
		gateWarnings = append(gateWarnings, verdict.Warnings...)
	}

	// Pre-trade price-collar gate: fat-finger / bad-quote safety
	// net (limit price too far from a recent reference quote).
	// Runs after the regulatory gates so a more dramatic reject
	// (halted, calendar, lockup, borrow) takes precedence in the
	// surfaced reason. Market orders are short-circuited inside
	// the gate's engine — collar checks only apply to limits.
	if s.priceCollarGate != nil {
		intended := req.LimitPrice
		if intended <= 0 {
			intended = req.StopPrice
		}
		verdict := s.priceCollarGate.CheckOrder(ctx, PriceCollarProbe{
			FundID:        req.FundID,
			InstrumentKey: req.InstrumentKey,
			Symbol:        req.Symbol,
			Market:        req.Market,
			AssetClass:    req.AssetClass,
			Side:          string(req.Side),
			Quantity:      req.Quantity,
			IntendedPrice: intended,
			ClientOrderID: req.ClientOrderID,
		})
		if verdict.Rejected {
			s.mu.Unlock()
			reason := verdict.RejectReason
			if reason == "" {
				reason = "rejected by price-collar gate"
			}
			return nil, fmt.Errorf("%w: %s", ErrPriceCollarRejected, reason)
		}
		gateWarnings = append(gateWarnings, verdict.Warnings...)
	}

	// Pre-trade lot-size gate: market-microstructure safety net.
	// Catches A-share board minimums (100/200) and step (100/1),
	// HK custom per-symbol lot, futures integer hands, crypto
	// step_size, US fractional-share capability. Runs LAST in the
	// gate chain so the regulatory rejects (status / lockup /
	// borrow) and the price-collar fat-finger reject keep
	// precedence in the surfaced reason. The gate sees the fund's
	// current position quantity so it can apply the A-share
	// odd-lot residual rule on partial sells.
	if s.lotSizeGate != nil {
		intended := req.LimitPrice
		if intended <= 0 {
			intended = req.StopPrice
		}
		verdict := s.lotSizeGate.CheckOrder(ctx, LotSizeProbe{
			FundID:        req.FundID,
			InstrumentKey: req.InstrumentKey,
			Symbol:        req.Symbol,
			Market:        req.Market,
			AssetClass:    req.AssetClass,
			Side:          string(req.Side),
			Quantity:      req.Quantity,
			LimitPrice:    intended,
			ClientOrderID: req.ClientOrderID,
		})
		if verdict.Rejected {
			s.mu.Unlock()
			reason := verdict.RejectReason
			if reason == "" {
				reason = "rejected by lot-size gate"
			}
			if verdict.SuggestedQty > 0 {
				reason = fmt.Sprintf("%s (suggested qty=%g)", reason, verdict.SuggestedQty)
			}
			return nil, fmt.Errorf("%w: %s", ErrLotSizeRejected, reason)
		}
		gateWarnings = append(gateWarnings, verdict.Warnings...)
	}

	order := &Order{
		BrokerOrderID: s.idFn(),
		ClientOrderID: req.ClientOrderID,
		Request:       req,
		State:         OrderStatePending,
		PlacedAt:      s.nowFn(),
		UpdatedAt:     s.nowFn(),
		Warnings:      gateWarnings,
	}
	// Stop / stop-limit / trailing-stop orders carry trigger
	// semantics: they MUST rest in OrderStatePending until the
	// stop-trigger engine observes a quote that breaches the
	// trigger. Initialise the dynamic stop tracking fields here
	// from the request; the trigger engine ratchets them on tick.
	if req.OrderType.IsStopType() {
		order.CurrentStopPrice = req.StopPrice
	}
	s.orders[order.BrokerOrderID] = order
	s.clientIndex[idemKey] = order.BrokerOrderID
	s.markOpenLocked(req.FundID, order.BrokerOrderID)
	s.mu.Unlock()

	// Stop-typed orders must NOT enter the matcher synchronously.
	// They wait for the trigger engine to fire them. Returning
	// here leaves them in OrderStatePending with CurrentStopPrice
	// populated; ListPendingStops surfaces them to the engine.
	if req.OrderType.IsStopType() {
		// Trailing stops need a seed quote so the high/low water
		// mark is anchored to "now" rather than to the first
		// tick the engine happens to see. Best-effort: if the
		// quote feed is unavailable we still accept the order;
		// the engine will seed on its first OnQuote.
		if req.OrderType == OrderTypeTrailingStop {
			s.seedTrailingFromQuote(ctx, order)
		}
		cp := s.copyOrder(order.BrokerOrderID)
		if cp == nil {
			return nil, ErrOrderNotFound
		}
		return cp, nil
	}

	// We've booked the order in the pending state. Now try to fill.
	// We release and re-acquire the lock around the quote call so a
	// slow upstream provider doesn't stall every other order.
	if err := s.tryFill(ctx, order); err != nil {
		// tryFill has already mutated order.State (rejected /
		// cancelled / working) under the lock. Return the error so
		// callers see WHY, but the Order is also fully observable.
		cp := s.copyOrder(order.BrokerOrderID)
		if cp == nil {
			return nil, err
		}
		return cp, err
	}
	cp := s.copyOrder(order.BrokerOrderID)
	if cp == nil {
		return nil, ErrOrderNotFound
	}
	return cp, nil
}

// tryFill prices the order via the quote provider and matching
// engine, then transitions the order to its post-match state. It
// emits a Fill on success and updates fees/slippage on the Order.
func (s *Simulator) tryFill(ctx context.Context, order *Order) error {
	req := order.Request

	quote, err := s.quoteFn(ctx, req.InstrumentKey, req.Symbol, req.Market)
	if err != nil {
		s.markRejected(order.BrokerOrderID, "quote unavailable: "+err.Error())
		return fmt.Errorf("%w: %v", ErrNoQuote, err)
	}
	if quote.Last <= 0 && !quote.HasSpread() {
		s.markRejected(order.BrokerOrderID, "quote unavailable: empty quote")
		return ErrNoQuote
	}

	matchSide := matching.NormalizeSide(string(req.Side))
	matchOrder := matching.Order{
		InstrumentKey:      req.InstrumentKey,
		Side:               matchSide,
		Quantity:           req.Quantity,
		LimitPrice:         req.LimitPrice,
		AssetClass:         strings.ToLower(req.AssetClass),
		ContractMultiplier: req.ContractMultiplier,
	}

	fill, matchErr := s.matchEngine.Match(matchOrder, quote)
	if matchErr != nil {
		// Limit-not-marketable: IOC/FOK cancel; DAY/GTC rest in
		// the working state. The Simulator's "book" is just the
		// order map — there's no resting matcher yet, so DAY/GTC
		// limits stay in OrderStateWorking until cancelled.
		if errors.Is(matchErr, matching.ErrLimitNotMarketable) {
			if req.TimeInForce == TIFIOC || req.TimeInForce == TIFFOK {
				s.markCancelled(order.BrokerOrderID, "ioc/fok not marketable")
				return fmt.Errorf("%w: %s", ErrLimitNotMarketable, req.TimeInForce)
			}
			s.markWorking(order.BrokerOrderID)
			return nil
		}
		s.markRejected(order.BrokerOrderID, matchErr.Error())
		return fmt.Errorf("%w: %v", ErrInvalidRequest, matchErr)
	}

	// Successful fill — transition to filled and emit the Fill.
	emitFill := s.markFilled(order.BrokerOrderID, fill, s.nowFn())
	if emitFill != nil {
		s.emit(req.FundID, *emitFill)
	}
	return nil
}

// CancelOrder implements Broker.
func (s *Simulator) CancelOrder(ctx context.Context, req CancelOrderRequest) error {
	if req.FundID == "" {
		return fmt.Errorf("%w: missing fund id", ErrInvalidRequest)
	}
	if req.BrokerOrderID == "" && req.ClientOrderID == "" {
		return fmt.Errorf("%w: must provide broker_order_id or client_order_id", ErrInvalidRequest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	brokerID := req.BrokerOrderID
	if brokerID == "" {
		var ok bool
		brokerID, ok = s.clientIndex[idempotencyKey(req.FundID, req.ClientOrderID)]
		if !ok {
			return ErrOrderNotFound
		}
	}
	order, ok := s.orders[brokerID]
	if !ok || order.Request.FundID != req.FundID {
		return ErrOrderNotFound
	}
	if order.State.IsTerminal() {
		return ErrOrderTerminal
	}
	order.State = OrderStateCancelled
	order.RejectReason = "cancelled by user"
	order.UpdatedAt = s.nowFn()
	s.removeOpenLocked(order.Request.FundID, brokerID)
	return nil
}

// ReplaceOrder implements Broker. P0-5 — applies pointer-as-no-change
// modifications to an open order's quantity, limit, stop, trailing,
// or display fields. Atomic from the caller's perspective: either
// the new params are committed AND any re-fill attempt has run, or
// the order is unchanged and an error is returned.
//
// Behaviour:
//
//   - Order must exist and belong to the request's fund; otherwise
//     ErrOrderNotFound.
//   - Order must be open (pending / working / partial). Terminal
//     orders return ErrOrderTerminal — replace-after-fill is not a
//     thing.
//   - At least one New* pointer must be non-nil; otherwise
//     ErrInvalidRequest. We treat a no-op replace as user error so
//     the audit log doesn't fill with empty change rows.
//   - Stop-typed orders (stop / stop_limit / trailing_stop) staying
//     in pending stay pending — the trigger engine picks up the new
//     stop on its next tick.
//   - Working LIMIT / ICEBERG orders re-route through tryFill: a
//     new limit price may now be marketable and should fill
//     immediately.
//
// Concurrency: the replace + re-fill pair runs under s.mu. Other
// PlaceOrder / Cancel calls block briefly, matching the venue
// semantics expected by the runtime trading engine.
func (s *Simulator) ReplaceOrder(ctx context.Context, req ReplaceOrderRequest) (*Order, error) {
	if req.FundID == "" {
		return nil, fmt.Errorf("%w: missing fund id", ErrInvalidRequest)
	}
	if req.BrokerOrderID == "" && req.ClientOrderID == "" {
		return nil, fmt.Errorf("%w: must provide broker_order_id or client_order_id", ErrInvalidRequest)
	}
	if !replaceRequestHasChanges(req) {
		return nil, fmt.Errorf("%w: replace must change at least one field", ErrInvalidRequest)
	}

	s.mu.Lock()
	brokerID := req.BrokerOrderID
	if brokerID == "" {
		var ok bool
		brokerID, ok = s.clientIndex[idempotencyKey(req.FundID, req.ClientOrderID)]
		if !ok {
			s.mu.Unlock()
			return nil, ErrOrderNotFound
		}
	}
	order, ok := s.orders[brokerID]
	if !ok || order.Request.FundID != req.FundID {
		s.mu.Unlock()
		return nil, ErrOrderNotFound
	}
	if order.State.IsTerminal() {
		s.mu.Unlock()
		return nil, ErrOrderTerminal
	}

	// Snapshot pre-mutation state for partial-fill safety: a
	// new quantity must not drop below the already-filled qty.
	if req.NewQuantity != nil {
		if *req.NewQuantity <= 0 {
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: new_quantity must be > 0", ErrInvalidRequest)
		}
		if *req.NewQuantity < order.FilledQuantity {
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: new_quantity (%v) below already-filled qty (%v)", ErrInvalidRequest, *req.NewQuantity, order.FilledQuantity)
		}
		order.Request.Quantity = *req.NewQuantity
	}
	if req.NewLimitPrice != nil {
		if *req.NewLimitPrice <= 0 {
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: new_limit_price must be > 0", ErrInvalidRequest)
		}
		order.Request.LimitPrice = *req.NewLimitPrice
	}
	if req.NewStopPrice != nil {
		if *req.NewStopPrice <= 0 {
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: new_stop_price must be > 0", ErrInvalidRequest)
		}
		order.Request.StopPrice = *req.NewStopPrice
		// Re-anchor CurrentStopPrice unless this is a trailing
		// stop — the trigger engine owns CurrentStopPrice for
		// trailing types.
		if order.Request.OrderType != OrderTypeTrailingStop {
			order.CurrentStopPrice = *req.NewStopPrice
		}
	}
	if req.NewTrailAmount != nil {
		if *req.NewTrailAmount <= 0 {
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: new_trail_amount must be > 0", ErrInvalidRequest)
		}
		order.Request.TrailAmount = *req.NewTrailAmount
		// For trailing stops, recompute CurrentStopPrice from
		// the in-flight HWM/LWM so the new trail takes effect
		// immediately.
		if order.Request.OrderType == OrderTypeTrailingStop {
			recomputeTrailingStopLocked(order)
		}
	}
	if req.NewTrailPercent != nil {
		if *req.NewTrailPercent <= 0 {
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: new_trail_percent must be > 0", ErrInvalidRequest)
		}
		order.Request.TrailPercent = *req.NewTrailPercent
		if order.Request.OrderType == OrderTypeTrailingStop {
			recomputeTrailingStopLocked(order)
		}
	}
	if req.NewDisplayQty != nil {
		if order.Request.OrderType != OrderTypeIceberg {
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: new_display_qty only valid on iceberg orders", ErrInvalidRequest)
		}
		if *req.NewDisplayQty <= 0 || *req.NewDisplayQty > order.Request.Quantity {
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: new_display_qty out of range", ErrInvalidRequest)
		}
		order.Request.DisplayQty = *req.NewDisplayQty
	}
	order.UpdatedAt = s.nowFn()

	// For working LIMIT / ICEBERG orders, the replaced limit may
	// now be marketable. Re-route through tryFill. We release the
	// lock around the quote call so a slow upstream provider
	// doesn't block other PlaceOrder callers — the same pattern
	// the original PlaceOrder uses.
	shouldRetryFill := order.State == OrderStateWorking &&
		(order.Request.OrderType == OrderTypeLimit || order.Request.OrderType == OrderTypeIceberg)
	cpForReturn := *order
	s.mu.Unlock()

	if shouldRetryFill {
		// tryFill takes the lock again internally and may
		// transition the order to filled / partial / cancelled
		// depending on the new limit + tif combo.
		if err := s.tryFill(ctx, order); err != nil {
			cp := s.copyOrder(order.BrokerOrderID)
			if cp == nil {
				return nil, err
			}
			return cp, err
		}
		cp := s.copyOrder(order.BrokerOrderID)
		if cp == nil {
			return nil, ErrOrderNotFound
		}
		return cp, nil
	}
	return &cpForReturn, nil
}

// replaceRequestHasChanges reports whether req carries at least one
// non-nil mutator pointer. Used as a fast-fail guard so a no-op
// replace doesn't silently bump UpdatedAt and audit-log noise.
func replaceRequestHasChanges(req ReplaceOrderRequest) bool {
	return req.NewQuantity != nil ||
		req.NewLimitPrice != nil ||
		req.NewStopPrice != nil ||
		req.NewTrailAmount != nil ||
		req.NewTrailPercent != nil ||
		req.NewDisplayQty != nil
}

// recomputeTrailingStopLocked updates CurrentStopPrice on a trailing
// stop using the current HWM/LWM and freshly-replaced trail params.
// Caller must hold s.mu.
func recomputeTrailingStopLocked(o *Order) {
	if o == nil || o.Request.OrderType != OrderTypeTrailingStop {
		return
	}
	if o.Request.Side == SideSell && o.TrailingHighWater > 0 {
		o.CurrentStopPrice = computeTrailingStopFromHigh(o.TrailingHighWater, o.Request)
	}
	if o.Request.Side == SideBuy && o.TrailingLowWater > 0 {
		o.CurrentStopPrice = computeTrailingStopFromLow(o.TrailingLowWater, o.Request)
	}
}

// GetOrder implements Broker.
func (s *Simulator) GetOrder(ctx context.Context, fundID, brokerOrderID string) (*Order, error) {
	if fundID == "" || brokerOrderID == "" {
		return nil, fmt.Errorf("%w: fund id and broker order id required", ErrInvalidRequest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[brokerOrderID]
	if !ok || order.Request.FundID != fundID {
		return nil, ErrOrderNotFound
	}
	cp := *order
	return &cp, nil
}

// GetOrderByClientID implements Broker.
func (s *Simulator) GetOrderByClientID(ctx context.Context, fundID, clientOrderID string) (*Order, error) {
	if fundID == "" || clientOrderID == "" {
		return nil, fmt.Errorf("%w: fund id and client order id required", ErrInvalidRequest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	brokerID, ok := s.clientIndex[idempotencyKey(fundID, clientOrderID)]
	if !ok {
		return nil, ErrOrderNotFound
	}
	order, ok := s.orders[brokerID]
	if !ok {
		return nil, ErrOrderNotFound
	}
	cp := *order
	return &cp, nil
}

// ListOpenOrders implements Broker. Returns orders in deterministic
// insertion order (sorted by PlacedAt ascending) so callers /
// snapshot tests can rely on the result.
func (s *Simulator) ListOpenOrders(ctx context.Context, fundID string) ([]Order, error) {
	if fundID == "" {
		return nil, fmt.Errorf("%w: missing fund id", ErrInvalidRequest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	openSet, ok := s.openByFund[fundID]
	if !ok || len(openSet) == 0 {
		return nil, nil
	}
	out := make([]Order, 0, len(openSet))
	for id := range openSet {
		order, ok := s.orders[id]
		if !ok {
			continue
		}
		cp := *order
		out = append(out, cp)
	}
	// Stable order — sort by PlacedAt then BrokerOrderID.
	sortOrdersByPlacedAt(out)
	return out, nil
}

// StreamFills implements Broker. The returned channel is closed
// when ctx is cancelled. Buffer size is configurable via
// WithStreamBufferSize; a full buffer drops new fills (the Order
// returned by PlaceOrder remains the canonical record).
func (s *Simulator) StreamFills(ctx context.Context, fundID string) (<-chan Fill, error) {
	if fundID == "" {
		return nil, fmt.Errorf("%w: missing fund id", ErrInvalidRequest)
	}
	ch := make(chan Fill, s.streamBuf)
	s.mu.Lock()
	s.subscribers[fundID] = append(s.subscribers[fundID], ch)
	s.mu.Unlock()

	go func() {
		<-ctx.Done()
		s.removeSubscriber(fundID, ch)
		close(ch)
	}()
	return ch, nil
}

// GetAccountSnapshot implements Broker. The Simulator is not the
// source of truth for positions — the platform's repos are. Live
// brokers populate this from their statement endpoint and the
// reconciliation loop diffs against the repos.
func (s *Simulator) GetAccountSnapshot(ctx context.Context, fundID string) (*AccountSnapshot, error) {
	return nil, ErrUnsupported
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func (s *Simulator) markRejected(brokerID, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[brokerID]
	if !ok {
		return
	}
	order.State = OrderStateRejected
	order.RejectReason = reason
	order.UpdatedAt = s.nowFn()
	s.removeOpenLocked(order.Request.FundID, brokerID)
}

func (s *Simulator) markCancelled(brokerID, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[brokerID]
	if !ok {
		return
	}
	order.State = OrderStateCancelled
	order.RejectReason = reason
	order.UpdatedAt = s.nowFn()
	s.removeOpenLocked(order.Request.FundID, brokerID)
}

func (s *Simulator) markWorking(brokerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[brokerID]
	if !ok {
		return
	}
	order.State = OrderStateWorking
	order.UpdatedAt = s.nowFn()
}

// markFilled transitions to filled and returns the Fill that should
// be emitted to subscribers (or nil if the order was already
// terminal — defensive against races). Caller emits OUTSIDE the
// lock to avoid blocking other PlaceOrder callers on a slow
// subscriber.
func (s *Simulator) markFilled(brokerID string, fill matching.Fill, at time.Time) *Fill {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[brokerID]
	if !ok || order.State.IsTerminal() {
		return nil
	}
	order.State = OrderStateFilled
	order.FilledQuantity = fill.Quantity
	order.AvgFillPrice = fill.Price
	order.LastFillAt = at
	order.UpdatedAt = at
	order.Fees = Fees{
		Commission:  fill.Commission,
		StampTax:    fill.StampTax,
		TransferFee: fill.TransferFee,
		Currency:    "", // caller fills from instrument metadata
	}
	order.SlippageBps = fill.SlippageBps
	s.removeOpenLocked(order.Request.FundID, brokerID)
	return &Fill{
		BrokerOrderID: order.BrokerOrderID,
		ClientOrderID: order.ClientOrderID,
		FundID:        order.Request.FundID,
		Quantity:      fill.Quantity,
		Price:         fill.Price,
		OccurredAt:    at,
		Fees:          order.Fees,
		Liquidity:     "taker",
		SlippageBps:   fill.SlippageBps,
	}
}

func (s *Simulator) markOpenLocked(fundID, brokerID string) {
	if s.openByFund[fundID] == nil {
		s.openByFund[fundID] = make(map[string]struct{})
	}
	s.openByFund[fundID][brokerID] = struct{}{}
}

func (s *Simulator) removeOpenLocked(fundID, brokerID string) {
	if s.openByFund[fundID] == nil {
		return
	}
	delete(s.openByFund[fundID], brokerID)
	if len(s.openByFund[fundID]) == 0 {
		delete(s.openByFund, fundID)
	}
}

func (s *Simulator) copyOrder(brokerID string) *Order {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[brokerID]
	if !ok {
		return nil
	}
	cp := *order
	return &cp
}

// emit broadcasts a fill to every subscriber for the fund. We snapshot
// the subscriber list under the lock, then send OUTSIDE the lock so a
// slow / blocked subscriber can't deadlock other broker calls. Sends
// are non-blocking — a full buffer drops the fill (PlaceOrder's return
// value is canonical anyway).
func (s *Simulator) emit(fundID string, fill Fill) {
	s.mu.Lock()
	subs := append([]chan Fill(nil), s.subscribers[fundID]...)
	s.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- fill:
		default:
		}
	}
}

func (s *Simulator) removeSubscriber(fundID string, target chan Fill) {
	s.mu.Lock()
	defer s.mu.Unlock()
	subs := s.subscribers[fundID]
	for i, ch := range subs {
		if ch == target {
			s.subscribers[fundID] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	if len(s.subscribers[fundID]) == 0 {
		delete(s.subscribers, fundID)
	}
}

// idempotencyKey returns the (fundID, clientOrderID) compound key
// used to gate duplicate PlaceOrder calls. Keep it tab-separated so
// neither field can collide with the other regardless of content.
func idempotencyKey(fundID, clientOrderID string) string {
	return fundID + "\t" + clientOrderID
}

// validatePlaceOrder enforces broker-level invariants. The
// platform's risk layer runs upstream and is far stricter; the
// checks here are the minimum a venue MUST do so the matching
// engine never sees garbage.
func validatePlaceOrder(req PlaceOrderRequest) error {
	if req.FundID == "" {
		return fmt.Errorf("%w: missing fund id", ErrInvalidRequest)
	}
	if strings.TrimSpace(req.ClientOrderID) == "" {
		return fmt.Errorf("%w: missing client_order_id (idempotency key required)", ErrInvalidRequest)
	}
	if req.Symbol == "" && req.InstrumentKey == "" {
		return fmt.Errorf("%w: missing symbol / instrument_key", ErrInvalidRequest)
	}
	if !req.Side.IsValid() {
		return fmt.Errorf("%w: side must be buy or sell, got %q", ErrInvalidRequest, req.Side)
	}
	if !req.OrderType.IsValid() {
		return fmt.Errorf("%w: unknown order type %q", ErrInvalidRequest, req.OrderType)
	}
	if req.Quantity <= 0 {
		return fmt.Errorf("%w: quantity must be > 0", ErrInvalidRequest)
	}
	if req.OrderType.RequiresLimitPrice() && req.LimitPrice <= 0 {
		return fmt.Errorf("%w: %s requires limit_price > 0", ErrInvalidRequest, req.OrderType)
	}
	if req.OrderType.RequiresStopPrice() && req.StopPrice <= 0 {
		return fmt.Errorf("%w: %s requires stop_price > 0", ErrInvalidRequest, req.OrderType)
	}
	if req.OrderType.RequiresTrailParams() && req.TrailAmount <= 0 && req.TrailPercent <= 0 {
		return fmt.Errorf("%w: trailing_stop requires trail_amount or trail_percent > 0", ErrInvalidRequest)
	}
	if req.OrderType == OrderTypeIceberg && (req.DisplayQty <= 0 || req.DisplayQty > req.Quantity) {
		return fmt.Errorf("%w: iceberg requires 0 < display_qty <= quantity", ErrInvalidRequest)
	}
	if req.TimeInForce != "" && !req.TimeInForce.IsValid() {
		return fmt.Errorf("%w: unknown time_in_force %q", ErrInvalidRequest, req.TimeInForce)
	}
	if req.TimeInForce == TIFGTD && req.GoodTillDate.IsZero() {
		return fmt.Errorf("%w: GTD requires good_till_date", ErrInvalidRequest)
	}
	return nil
}

// sortOrdersByPlacedAt sorts in place by PlacedAt then BrokerOrderID
// so test snapshots are stable.
func sortOrdersByPlacedAt(orders []Order) {
	// Insertion sort — fund-level open-order lists are tiny.
	for i := 1; i < len(orders); i++ {
		j := i
		for j > 0 {
			a, b := orders[j-1], orders[j]
			if a.PlacedAt.After(b.PlacedAt) ||
				(a.PlacedAt.Equal(b.PlacedAt) && a.BrokerOrderID > b.BrokerOrderID) {
				orders[j-1], orders[j] = b, a
				j--
				continue
			}
			break
		}
	}
}
