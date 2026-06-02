// Package stoptrigger implements the venue-side stop-order trigger
// engine (P0-3). It watches quote ticks and fires stop / stop_limit /
// trailing_stop orders that are resting on the broker.Simulator.
//
// Why a separate package
//
// The matching engine (internal/matching) is intentionally
// stateless: it prices a single Order against a Quote and either
// fills or rejects. Stop semantics are inherently stateful — they
// need a high-water mark for trailing, and they need a tick stream
// to know when "last" crosses the trigger. Putting that logic into
// matching would force every venue (broker.Simulator and future
// live broker adapters) to re-implement it. By owning trigger logic
// in a discrete engine that talks only to broker.Simulator's
// PendingStops* / FireStopTrigger / UpdateTrailing* surface, we
// keep the venue interface narrow and reuse the same trigger
// behaviour across replays, paper, and back-tests.
//
// Live brokers do their own stop-trigger venue-side and so do NOT
// need this engine — they receive a Fill straight from the wire.
package stoptrigger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/fundai/server/internal/broker"
	"github.com/fundai/server/internal/matching"
)

// Venue is the narrow surface of broker.Simulator the engine drives.
// Defining it here (not in package broker) keeps stoptrigger
// importable without forcing a cyclic dep, and lets tests mock the
// venue without touching the simulator.
type Venue interface {
	// PendingStopsForInstrument returns active stop / stop_limit /
	// trailing_stop orders awaiting a trigger on the given
	// instrument. The engine treats the returned slice as a
	// snapshot — concurrent placements/cancels appear on the next
	// tick.
	PendingStopsForInstrument(instrumentKey, symbol string) []broker.Order

	// UpdateTrailingHighWater ratchets the trailing-sell-stop
	// price upwards if last exceeds the existing high-water mark.
	// Idempotent.
	UpdateTrailingHighWater(brokerOrderID string, last float64) (*broker.Order, error)

	// UpdateTrailingLowWater is the buy-side mirror.
	UpdateTrailingLowWater(brokerOrderID string, last float64) (*broker.Order, error)

	// FireStopTrigger transitions a pending stop to triggered and
	// places its child order. Returns the child Order (already
	// routed to the matching engine) or ErrStopAlreadyFired when
	// some other tick beat the caller to it.
	FireStopTrigger(ctx context.Context, brokerOrderID string, quote matching.Quote) (*broker.Order, error)
}

// Engine is the stop-trigger orchestrator. Construct via New, drive
// it via OnQuote.
//
// Concurrency
//
// OnQuote is safe for concurrent callers; the engine serialises
// access to its in-process bookkeeping (most importantly, the
// "fired-this-tick" guard that prevents a single quote from firing
// the same stop twice). The Venue is responsible for its own
// concurrency.
type Engine struct {
	venue Venue
	now   func() time.Time
	log   *slog.Logger

	// firedMu guards firing; we serialise per-instrument trigger
	// passes so two simultaneous OnQuote calls for the same symbol
	// can't both fire the same stop.
	firedMu sync.Mutex
}

// Option configures the Engine at construction time.
type Option func(*Engine)

// WithClock injects a deterministic clock for tests.
func WithClock(now func() time.Time) Option {
	return func(e *Engine) {
		if now != nil {
			e.now = now
		}
	}
}

// WithLogger injects a structured logger. Defaults to slog.Default.
func WithLogger(l *slog.Logger) Option {
	return func(e *Engine) {
		if l != nil {
			e.log = l
		}
	}
}

// New constructs a stop-trigger Engine bound to venue.
func New(venue Venue, opts ...Option) *Engine {
	if venue == nil {
		panic("stoptrigger: Venue is required")
	}
	e := &Engine{
		venue: venue,
		now:   time.Now,
		log:   slog.Default(),
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// QuoteTick is the input to the trigger engine — a single market
// quote on a specific instrument. Identifying the instrument by
// either InstrumentKey OR Symbol matches how broker.Simulator stores
// orders; the engine forwards both to the venue lookup.
type QuoteTick struct {
	InstrumentKey string
	Symbol        string
	Quote         matching.Quote
}

// FireResult summarises one trigger pass. Returned for callers that
// want to surface metrics; ignored on most code paths.
type FireResult struct {
	// Inspected is the count of pending stops examined this tick.
	Inspected int
	// Updated is the count of trailing stops whose high/low water
	// mark moved this tick (regardless of whether they then
	// fired).
	Updated int
	// Fired is the count of stops whose trigger condition was met
	// AND whose child order was successfully placed.
	Fired int
	// Errors aggregates non-fatal venue errors encountered while
	// firing (e.g. ErrStopAlreadyFired when two ticks raced). Each
	// entry has the parent broker_order_id so operators can
	// correlate.
	Errors []FireError
}

// FireError is a per-order failure during a trigger pass.
type FireError struct {
	BrokerOrderID string
	Err           error
}

// Error reports the wrapped venue error.
func (e FireError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("stoptrigger: %s", e.BrokerOrderID)
	}
	return fmt.Sprintf("stoptrigger: order %s: %v", e.BrokerOrderID, e.Err)
}

// Unwrap exposes the wrapped error for errors.Is/As.
func (e FireError) Unwrap() error { return e.Err }

// OnQuote runs one trigger pass for tick. Behaviour:
//
//  1. Snapshot pending stops on the instrument via the venue.
//  2. For trailing stops, ratchet the high/low water mark using
//     tick.Quote.Last.
//  3. For every stop (trailing or static), evaluate the trigger
//     predicate (broker.StopShouldFire) against the post-update
//     CurrentStopPrice and the tick's last.
//  4. Fire matching stops sequentially via the venue. Errors are
//     collected — they do NOT stop the pass — so a single
//     pathological order can't mask others.
//
// Returns a FireResult with per-tick counters and a slice of
// non-fatal errors. The returned error is non-nil ONLY when the
// inputs are invalid (e.g. nil context) — venue errors live on
// FireResult.Errors.
func (e *Engine) OnQuote(ctx context.Context, tick QuoteTick) (FireResult, error) {
	if ctx == nil {
		return FireResult{}, errors.New("stoptrigger: nil context")
	}
	if tick.InstrumentKey == "" && tick.Symbol == "" {
		return FireResult{}, errors.New("stoptrigger: tick must specify instrument_key or symbol")
	}
	if tick.Quote.Last <= 0 && !tick.Quote.HasSpread() {
		// No usable price — the instrument is halted or pre-open.
		// Skip silently; metric callers can rely on Inspected==0
		// to detect this.
		return FireResult{}, nil
	}

	e.firedMu.Lock()
	defer e.firedMu.Unlock()

	pendings := e.venue.PendingStopsForInstrument(tick.InstrumentKey, tick.Symbol)
	res := FireResult{Inspected: len(pendings)}
	if len(pendings) == 0 {
		return res, nil
	}

	last := tick.Quote.Last
	if last <= 0 {
		// Use mid as fallback when the venue reports only a
		// spread (e.g. pre-trade quote without last).
		last = tick.Quote.MidPrice()
	}

	for _, p := range pendings {
		stop := p.CurrentStopPrice

		// Ratchet trailing stops first; they may move the
		// trigger AWAY from last and prevent a fire that would
		// otherwise have happened on a stale CurrentStopPrice.
		if p.Request.OrderType == broker.OrderTypeTrailingStop {
			if updated, ok := e.ratchetTrailing(p, last); ok {
				stop = updated.CurrentStopPrice
				res.Updated++
			}
		}

		if !broker.StopShouldFire(p.Request.Side, stop, last) {
			continue
		}

		fired, err := e.venue.FireStopTrigger(ctx, p.BrokerOrderID, tick.Quote)
		if err != nil {
			res.Errors = append(res.Errors, FireError{BrokerOrderID: p.BrokerOrderID, Err: err})
			e.log.Warn("stop-trigger fire failed",
				"broker_order_id", p.BrokerOrderID,
				"client_order_id", p.ClientOrderID,
				"order_type", string(p.Request.OrderType),
				"err", err.Error())
			continue
		}
		res.Fired++
		e.log.Info("stop fired",
			"parent_broker_order_id", p.BrokerOrderID,
			"child_broker_order_id", fired.BrokerOrderID,
			"order_type", string(p.Request.OrderType),
			"side", string(p.Request.Side),
			"stop_price", stop,
			"last", last,
			"fund_id", p.Request.FundID)
	}
	return res, nil
}

// ratchetTrailing dispatches to the side-appropriate water-mark
// update on the venue. Returns the post-update order and a bool
// reporting whether the water mark actually moved (so callers can
// increment the FireResult.Updated counter only on real moves).
func (e *Engine) ratchetTrailing(p broker.Order, last float64) (*broker.Order, bool) {
	var (
		updated *broker.Order
		err     error
	)
	switch p.Request.Side {
	case broker.SideSell:
		updated, err = e.venue.UpdateTrailingHighWater(p.BrokerOrderID, last)
	case broker.SideBuy:
		updated, err = e.venue.UpdateTrailingLowWater(p.BrokerOrderID, last)
	default:
		return nil, false
	}
	if err != nil {
		// The order may have been cancelled or already fired
		// between PendingStopsForInstrument and now. Treat all
		// errors as "don't ratchet, but still evaluate the stop
		// at its previous CurrentStopPrice".
		e.log.Debug("trailing ratchet skipped",
			"broker_order_id", p.BrokerOrderID,
			"err", err.Error())
		return nil, false
	}
	if updated == nil {
		return nil, false
	}
	moved := updated.CurrentStopPrice != p.CurrentStopPrice ||
		updated.TrailingHighWater != p.TrailingHighWater ||
		updated.TrailingLowWater != p.TrailingLowWater
	return updated, moved
}
