package main

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/fundai/server/internal/broker"
	"github.com/fundai/server/internal/marketdata"
	"github.com/fundai/server/internal/marketimpact"
	"github.com/fundai/server/internal/matching"
	"github.com/fundai/server/internal/stoptrigger"
)

// newBrokerSimulator constructs the in-process Simulator that
// implements broker.Broker. The simulator is the seam through which
// future PRs route order placement (P0-2 expanded order types,
// P0-3 stop-trigger, P0-5 cancel/replace), and where a real
// broker adapter (long-port, IBKR, ...) plugs in by implementing
// the same interface.
//
// This file deliberately does NOT change the runtimeTradingEngine
// execution path: that migration ships in a follow-up so order
// placement / persistence stays atomic with this PR. Today the
// simulator is constructed and parked on Services.BrokerSimulator
// so downstream wiring can reach for it as soon as the order schema
// (P0-2) lands.
//
// QuoteFn adapts marketdata.Service.GetQuote into the
// broker.QuoteFn shape. We pass through every field needed by the
// matching.Engine — price/bid/ask only; the simulator itself does
// not need OHLC, fundamentals, or news.
// newMarketDataQuoteFn adapts marketdata.Service.GetQuote into the
// broker.QuoteFn shape. Exposed (lower-cased package-private) so the
// stop-trigger poller can share the exact same quote pipeline as the
// simulator without re-deriving the conversion.
func newMarketDataQuoteFn(md *marketdata.Service) broker.QuoteFn {
	return func(ctx context.Context, instrumentKey, symbol, market string) (matching.Quote, error) {
		if md == nil {
			return matching.Quote{}, marketdata.ErrQuoteUnavailable
		}
		ref := marketdata.InstrumentRef{
			InstrumentKey: instrumentKey,
			Symbol:        symbol,
			Market:        strings.ToLower(strings.TrimSpace(market)),
		}
		snap, err := md.GetQuote(ctx, ref)
		if err != nil {
			return matching.Quote{}, err
		}
		if snap == nil {
			return matching.Quote{}, marketdata.ErrQuoteUnavailable
		}
		return matching.Quote{
			Last: snap.Price,
			Bid:  snap.Bid,
			Ask:  snap.Ask,
		}, nil
	}
}

func newBrokerSimulator(md *marketdata.Service, opts ...broker.SimulatorOption) *broker.Simulator {
	all := append([]broker.SimulatorOption{}, opts...)
	return broker.NewSimulator(newMarketDataQuoteFn(md), all...)
}

// marketImpactRecorder bridges marketimpact.EstimateRecorder ->
// serverMetrics. Bucketing keeps cardinality bounded:
//
//	bucket_<asset_class>_<bps_bucket>
//
// Bps buckets: 0_5, 5_20, 20_50, 50_100, 100_250, 250_plus.
// Asset class is taken from the probe so we can answer "what
// fraction of equity orders cleared 50 bps" without looking at
// raw fills.
type marketImpactRecorder struct {
	metrics *serverMetrics
}

// RecordEstimate implements marketimpact.EstimateRecorder.
func (r *marketImpactRecorder) RecordEstimate(probe marketimpact.OrderProbe, est marketimpact.Estimate) {
	if r == nil || r.metrics == nil {
		return
	}
	r.metrics.RecordMarketImpactEvent("estimate")
	if est.UsedDefaults {
		r.metrics.RecordMarketImpactEvent("used_defaults")
	}
	if est.UsedADVFallback {
		r.metrics.RecordMarketImpactEvent("used_adv_fallback")
	}
	asset := strings.ToLower(strings.TrimSpace(probe.AssetClass))
	if asset == "" {
		asset = "unknown"
	}
	r.metrics.RecordMarketImpactEvent(fmt.Sprintf("bucket_%s_%s", asset, marketImpactBucket(est.AdverseBps)))
}

// marketImpactBucket maps an adverse-bps value to a coarse
// bucket label for metrics. Buckets are open-ended on the high
// side so a misconfigured 9999 bps still lands in 250_plus.
func marketImpactBucket(bps float64) string {
	if math.IsNaN(bps) || bps < 0 {
		return "invalid"
	}
	switch {
	case bps < 5:
		return "0_5"
	case bps < 20:
		return "5_20"
	case bps < 50:
		return "20_50"
	case bps < 100:
		return "50_100"
	case bps < 250:
		return "100_250"
	default:
		return "250_plus"
	}
}

// newMarketImpactStack builds the Cache + Engine + slippage
// adapter that turns the in-memory calibration store into a
// matching.SlippageModel ready to plug into the broker
// simulator.
//
// The cache is started here (initial Refresh + periodic loop)
// so the simulator never sees an empty cache after boot. The
// returned Cache handle is parked on Services so admin handlers
// can ApplyChange after a write. The returned adapter doubles as
// the matching.SlippageModel and as the EstimateForProbe source
// used by the admin "preview" endpoint.
func newMarketImpactStack(ctx context.Context, repo *marketimpact.Repo, metrics *serverMetrics) (*marketimpact.Cache, *marketimpact.SlippageAdapter) {
	cache := marketimpact.NewCache(repo, marketimpact.CacheConfig{
		OnError: func(err error) {
			if metrics != nil {
				metrics.RecordMarketImpactEvent("cache_refresh_err")
			}
		},
	})
	if err := cache.Start(ctx); err != nil {
		// Start logs via OnError; nothing extra to do here. The
		// adapter still works against an empty cache (engine will
		// fall back to asset-class defaults).
		_ = err
	} else if metrics != nil {
		metrics.RecordMarketImpactEvent("cache_refresh_ok")
	}
	engine := marketimpact.NewEngine()
	adapter := marketimpact.NewSlippageAdapter(
		cache, engine,
		marketimpact.WithRecorder(&marketImpactRecorder{metrics: metrics}),
	)
	return cache, adapter
}

// newMatchingEngineWithImpact returns a matching.Engine that
// uses the size-aware adapter as its slippage model. Falls back
// to matching.NewDefaultEngine() (zero-slip) when the adapter is
// nil.
func newMatchingEngineWithImpact(adapter matching.SlippageModel) matching.Engine {
	if adapter == nil {
		return matching.NewDefaultEngine()
	}
	return &matching.MarketableEngine{
		Slippage: adapter,
		Fees:     matching.FixedRateEquityFees{},
	}
}

// newStopTriggerEngine constructs the venue-side stop-trigger engine
// (P0-3) bound to the supplied simulator. Callers (the marketdata
// quote loop, integration tests, etc.) drive Engine.OnQuote on every
// tick to ratchet trailing stops and fire stops whose trigger has
// been breached.
//
// Returns nil when the simulator is missing — the caller is then
// responsible for skipping any quote-tick fan-out.
func newStopTriggerEngine(sim *broker.Simulator) *stoptrigger.Engine {
	if sim == nil {
		return nil
	}
	return stoptrigger.New(sim)
}
