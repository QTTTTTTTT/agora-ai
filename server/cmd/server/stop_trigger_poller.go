package main

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fundai/server/internal/broker"
	"github.com/fundai/server/internal/matching"
	"github.com/fundai/server/internal/stoptrigger"
)

// stopTriggerPoller drives the stop-trigger engine on a periodic
// tick. Each tick it:
//
//  1. Snapshots all pending stop / stop_limit / trailing_stop
//     orders on the simulator.
//  2. Deduplicates them by (instrument_key, symbol, market) so we
//     fetch each quote at most once per tick.
//  3. Calls the supplied QuoteFn for each unique instrument.
//  4. Forwards the quote into Engine.OnQuote, which ratchets
//     trailing stops and fires triggered stops.
//
// Concurrency: a single poller serialises ticks, so the in-process
// "fired-this-tick" guard inside Engine.OnQuote is sufficient. The
// poller is safe to start/stop via Run + ctx cancellation.
//
// Production wiring runs the poller every QUOTE_TICK_INTERVAL (env)
// or 5s by default — short enough to feel responsive on
// human-timescale stops, long enough to never overrun the upstream
// quote provider's rate limits. WS-driven push tick streams (when
// available) bypass this poller and call Engine.OnQuote directly.
type stopTriggerPoller struct {
	engine   *stoptrigger.Engine
	sim      *broker.Simulator
	quoteFn  broker.QuoteFn
	interval time.Duration
	log      *slog.Logger

	// metrics.
	tickCount    atomic.Uint64
	firedCount   atomic.Uint64
	updatedCount atomic.Uint64
	errorCount   atomic.Uint64

	// stopOnce guards Run from being entered twice on the same
	// instance.
	stopOnce sync.Once

	// lifecycle for Start/Stop.
	cancel context.CancelFunc
	done   chan struct{}
}

// newStopTriggerPoller builds a poller bound to engine + simulator.
// Returns nil if any required dep is missing — caller can short-
// circuit the lifecycle.
func newStopTriggerPoller(engine *stoptrigger.Engine, sim *broker.Simulator, quoteFn broker.QuoteFn, interval time.Duration, log *slog.Logger) *stopTriggerPoller {
	if engine == nil || sim == nil || quoteFn == nil {
		return nil
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &stopTriggerPoller{
		engine:   engine,
		sim:      sim,
		quoteFn:  quoteFn,
		interval: interval,
		log:      log,
	}
}

// Run blocks until ctx is cancelled, calling Tick on every interval.
// Safe to call once per instance; a second concurrent Run returns
// immediately. The first Tick fires after one interval (not
// immediately) so the boot path can complete schema migrations and
// prime caches before tripping any stops.
func (p *stopTriggerPoller) Run(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var ran bool
	p.stopOnce.Do(func() { ran = true })
	if !ran {
		return nil
	}
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			p.Tick(ctx)
		}
	}
}

// Start launches the poller in a background goroutine. Idempotent
// on a nil receiver. Pair with Stop in the shutdown path. Calling
// Start twice on the same instance is a no-op (the second call
// returns immediately).
func (p *stopTriggerPoller) Start() {
	if p == nil {
		return
	}
	if p.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		t := time.NewTicker(p.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.Tick(ctx)
			}
		}
	}()
}

// Stop signals the background goroutine to exit and blocks until it
// has finished its in-flight Tick. Idempotent on a nil receiver or
// a poller that was never started.
func (p *stopTriggerPoller) Stop() {
	if p == nil || p.cancel == nil {
		return
	}
	p.cancel()
	if p.done != nil {
		<-p.done
	}
	p.cancel = nil
	p.done = nil
}

// Tick runs one trigger pass across all pending stops. Exported so
// admin endpoints, tests, and back-tests can drive the poller
// synchronously without spinning up the goroutine.
func (p *stopTriggerPoller) Tick(ctx context.Context) {
	if p == nil || p.sim == nil || p.engine == nil {
		return
	}
	p.tickCount.Add(1)

	pendings := p.sim.AllPendingStops()
	if len(pendings) == 0 {
		return
	}
	// Dedupe by (instrument_key, symbol, market). Map key is the
	// concat with a unit separator so two distinct instruments that
	// happen to share a symbol but differ in market are NOT merged.
	type instKey struct {
		instrumentKey string
		symbol        string
		market        string
	}
	uniq := make(map[instKey]struct{}, len(pendings))
	for _, p := range pendings {
		uniq[instKey{
			instrumentKey: p.Request.InstrumentKey,
			symbol:        p.Request.Symbol,
			market:        p.Request.Market,
		}] = struct{}{}
	}
	for k := range uniq {
		quote, err := p.quoteFn(ctx, k.instrumentKey, k.symbol, k.market)
		if err != nil {
			p.errorCount.Add(1)
			p.log.Debug("stop-trigger poller: quote fetch failed",
				"instrument_key", k.instrumentKey,
				"symbol", k.symbol,
				"market", k.market,
				"err", err.Error())
			continue
		}
		if quote.Last <= 0 && !quote.HasSpread() {
			// Halted / pre-open. Skip silently.
			continue
		}
		p.fanOutTick(ctx, k.instrumentKey, k.symbol, quote)
	}
}

func (p *stopTriggerPoller) fanOutTick(ctx context.Context, instrumentKey, symbol string, q matching.Quote) {
	res, err := p.engine.OnQuote(ctx, stoptrigger.QuoteTick{
		InstrumentKey: instrumentKey,
		Symbol:        symbol,
		Quote:         q,
	})
	if err != nil {
		p.errorCount.Add(1)
		p.log.Warn("stop-trigger poller: OnQuote error",
			"instrument_key", instrumentKey,
			"symbol", symbol,
			"err", err.Error())
		return
	}
	p.firedCount.Add(uint64(res.Fired))
	p.updatedCount.Add(uint64(res.Updated))
	if len(res.Errors) > 0 {
		p.errorCount.Add(uint64(len(res.Errors)))
	}
	if res.Fired > 0 || res.Updated > 0 {
		p.log.Info("stop-trigger poller tick",
			"instrument_key", instrumentKey,
			"symbol", symbol,
			"inspected", res.Inspected,
			"updated", res.Updated,
			"fired", res.Fired,
			"errors", len(res.Errors))
	}
}

// Snapshot returns the current poller counters. Atomic loads so the
// admin /metrics scrape can read concurrently with the goroutine.
func (p *stopTriggerPoller) Snapshot() stopTriggerPollerSnapshot {
	if p == nil {
		return stopTriggerPollerSnapshot{}
	}
	return stopTriggerPollerSnapshot{
		Ticks:   p.tickCount.Load(),
		Fired:   p.firedCount.Load(),
		Updated: p.updatedCount.Load(),
		Errors:  p.errorCount.Load(),
	}
}

type stopTriggerPollerSnapshot struct {
	Ticks   uint64 `json:"ticks"`
	Fired   uint64 `json:"fired"`
	Updated uint64 `json:"updated"`
	Errors  uint64 `json:"errors"`
}
