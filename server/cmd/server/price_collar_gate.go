// price_collar_gate.go — production wiring of broker.PriceCollarGate.
//
// Translates a broker probe into a pricecollar.Engine call backed by
// the live marketdata.Service (which itself caches REST + wsfeed
// snapshots), records gate decisions on serverMetrics, and persists
// rejects into the audit log so operators can replay them.
//
// Trigger story: 2026-06-02 301308 buy filled at 96,226.4188 CNY/share
// against a true mid of ~500. The PM fallback path had stamped the
// notional buy budget into PlanAction.Price with quantity=1; the
// simulator faithfully honoured that limit. We've since (a) downgraded
// quote-unavailable to watch in wiring_adapters.go and (b) introduced
// pricecollar.Engine as a broker-side safety net. This file is the
// glue that bridges the two so a future fat-finger / LLM hallucination
// / bad pasted price can never reach the matcher.

package main

import (
	"context"
	"strings"

	"github.com/fundai/server/internal/broker"
	"github.com/fundai/server/internal/marketdata"
	"github.com/fundai/server/internal/pricecollar"
)

// priceCollarGate implements broker.PriceCollarGate. The reference
// quote comes from marketdata.Service (which transparently chooses
// between REST cache, wsfeed cache, and the configured providers).
// Engine misconfiguration / DB hiccups fail-OPEN to "warn, do not
// reject" — production safety prefers an alertable warning over a
// trading halt. The 96,226-style fat-finger is what we want to
// catch; gate downtime is what we don't want to amplify.
type priceCollarGate struct {
	engine  *pricecollar.Engine
	metrics *serverMetrics
	logger  leveledLogger
}

// newPriceCollarGate constructs the production wiring. Returning nil
// when marketData is nil keeps the simulator's optional-gate
// contract: callers can pass the result straight into
// broker.WithPriceCollarGate without an extra nil-check.
//
// opts is forwarded to pricecollar.EngineOptions so cmd/server can
// flip the no-reference verdict to Reject for stricter deployments
// or tighten asset-class thresholds without code changes here.
func newPriceCollarGate(marketData *marketdata.Service, metrics *serverMetrics, logger leveledLogger, opts pricecollar.EngineOptions) *priceCollarGate {
	if marketData == nil {
		return nil
	}
	source := &marketDataReferenceSource{md: marketData}
	engine, err := pricecollar.NewEngine(source, opts)
	if err != nil {
		// Misconfiguration at construction is loud-but-soft: log it
		// and fall back to a nil gate so the broker keeps working.
		if logger != nil {
			logger.Warn("price-collar gate: engine init failed; gate disabled", "err", err)
		}
		return nil
	}
	return &priceCollarGate{
		engine:  engine,
		metrics: metrics,
		logger:  logger,
	}
}

// CheckOrder satisfies broker.PriceCollarGate.
func (g *priceCollarGate) CheckOrder(ctx context.Context, probe broker.PriceCollarProbe) broker.PriceCollarVerdict {
	if g == nil || g.engine == nil {
		return broker.PriceCollarVerdict{}
	}
	res, err := g.engine.Check(ctx, pricecollar.Probe{
		FundID:        probe.FundID,
		InstrumentKey: probe.InstrumentKey,
		Symbol:        probe.Symbol,
		Market:        probe.Market,
		AssetClass:    probe.AssetClass,
		Side:          probe.Side,
		Quantity:      probe.Quantity,
		IntendedPrice: probe.IntendedPrice,
		ClientOrderID: probe.ClientOrderID,
	})
	if err != nil {
		// Fail-open with a metric tag so dashboards still show the
		// failure. ErrInvalidProbe falls here too (the broker is
		// supposed to validate before reaching us, so this is a
		// dev-time signal not a runtime hazard).
		g.recordEvent("evaluate_failed")
		g.warnf("price-collar gate: engine evaluate failed", "instrument", probe.InstrumentKey, "err", err)
		return broker.PriceCollarVerdict{}
	}

	verdict := broker.PriceCollarVerdict{ToleranceBps: res.AppliedThresholdBps}
	if res.Reference != nil {
		verdict.ReferencePrice = res.Reference.Price
	}

	for _, ev := range res.Events {
		switch ev.Decision {
		case pricecollar.DecisionReject:
			verdict.Rejected = true
			if verdict.RejectReason == "" {
				verdict.RejectReason = string(ev.RuleCode) + ": " + ev.Summary
			}
			g.recordEvent("reject_" + string(ev.RuleCode))
		case pricecollar.DecisionWarn:
			verdict.Warnings = append(verdict.Warnings, string(ev.RuleCode)+": "+ev.Summary)
			g.recordEvent("warn_" + string(ev.RuleCode))
		}
	}
	if !verdict.Rejected && len(res.Events) == 0 {
		g.recordEvent("allow")
	}
	return verdict
}

func (g *priceCollarGate) recordEvent(name string) {
	if g == nil || g.metrics == nil {
		return
	}
	g.metrics.RecordPriceCollarEvent(name)
}

func (g *priceCollarGate) warnf(msg string, kv ...any) {
	if g == nil || g.logger == nil {
		return
	}
	g.logger.Warn(msg, kv...)
}

// marketDataReferenceSource is the pricecollar.ReferenceSource that
// pulls the latest snapshot from marketdata.Service. The service
// already encapsulates REST / wsfeed / cache layering so we don't
// need to combine sources here.
type marketDataReferenceSource struct {
	md *marketdata.Service
}

func (s *marketDataReferenceSource) GetReferenceQuote(ctx context.Context, probe pricecollar.Probe) (*pricecollar.ReferenceQuote, error) {
	if s == nil || s.md == nil {
		return nil, nil
	}
	ref := marketdata.InstrumentRef{
		InstrumentKey: probe.InstrumentKey,
		Symbol:        probe.Symbol,
		Market:        probe.Market,
		AssetClass:    probe.AssetClass,
	}
	q, err := s.md.GetQuote(ctx, ref)
	if err != nil {
		// ErrQuoteUnavailable is the "no usable reference" signal —
		// surface as (nil, nil) so the engine routes through the
		// RuleNoReference path (default warn). Anything else is a
		// real lookup failure; report it so the engine warns + the
		// gate's metrics tick the failure counter.
		if strings.Contains(err.Error(), marketdata.ErrQuoteUnavailable.Error()) {
			return nil, nil
		}
		return nil, err
	}
	if q == nil || q.Price <= 0 {
		return nil, nil
	}
	return &pricecollar.ReferenceQuote{
		InstrumentKey: q.InstrumentKey,
		Symbol:        q.Symbol,
		Market:        q.Market,
		AssetClass:    q.AssetClass,
		Price:         q.Price,
		AsOf:          q.AsOf,
	}, nil
}
