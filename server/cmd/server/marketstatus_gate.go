// marketstatus_gate.go — production wiring of the broker's
// MarketStatusGate hook. Translates a probe into a marketstatus
// engine call backed by the live DB tables, and persists every
// reject/warn into marketstatus_events for the audit trail.
//
// Why this lives in cmd/server, not internal/broker
//
//   - The broker package defines the interface but stays out of
//     the marketstatus dependency graph (so tests don't pull in
//     a DB).
//   - The marketstatus package owns the rules but knows nothing
//     about broker types.
//   - This file is the cmd/server-level glue that lets them meet
//     without either side reaching across.

package main

import (
	"context"
	"strings"
	"time"

	"github.com/fundai/server/internal/broker"
	"github.com/fundai/server/internal/marketstatus"
)

// marketStatusGate implements broker.MarketStatusGate. It loads
// the live status row + the calendar day, evaluates the engine,
// persists any non-allow events, and returns a verdict the
// simulator can act on.
type marketStatusGate struct {
	repo    *marketstatus.Repo
	engine  *marketstatus.Engine
	metrics *serverMetrics
	logger  leveledLogger
}

// newMarketStatusGate constructs the gate. nil repo → returns
// nil so callers can simply pass-through nil into
// broker.WithMarketStatusGate (no-op gate).
func newMarketStatusGate(repo *marketstatus.Repo, metrics *serverMetrics, logger leveledLogger) *marketStatusGate {
	if repo == nil {
		return nil
	}
	return &marketStatusGate{
		repo:    repo,
		engine:  marketstatus.NewEngine(),
		metrics: metrics,
		logger:  logger,
	}
}

// CheckOrder satisfies broker.MarketStatusGate. The body never
// hard-fails the order on an internal error (e.g. DB hiccup) —
// production safety prefers "allow with operator alert" over
// "reject and stop trading"; misconfiguration of the gate must
// never be a denial-of-service for the rest of the platform.
func (g *marketStatusGate) CheckOrder(ctx context.Context, probe broker.MarketStatusProbe) broker.MarketStatusVerdict {
	if g == nil || g.repo == nil {
		return broker.MarketStatusVerdict{}
	}
	// Load the per-instrument status row. Missing → engine treats
	// nil as "not configured", which yields no halt/limit/stale
	// rules. Calendar same idea: missing → "open by default".
	status, err := g.repo.GetByKey(ctx, probe.InstrumentKey)
	if err != nil {
		g.recordEvent("lookup_failed")
		g.warnf("marketstatus gate: status lookup failed", "instrument", probe.InstrumentKey, "err", err)
		return broker.MarketStatusVerdict{}
	}
	market := strings.TrimSpace(probe.Market)
	if market == "" && status != nil {
		market = status.Market
	}
	var day *marketstatus.CalendarDay
	if market != "" {
		today := time.Now().UTC()
		day, err = g.repo.GetCalendarDay(ctx, market, today)
		if err != nil {
			g.recordEvent("calendar_lookup_failed")
			g.warnf("marketstatus gate: calendar lookup failed", "market", market, "err", err)
		}
	}
	res, err := g.engine.Check(marketstatus.OrderProbe{
		FundID:        probe.FundID,
		InstrumentKey: probe.InstrumentKey,
		Symbol:        probe.Symbol,
		Market:        market,
		AssetClass:    probe.AssetClass,
		Side:          probe.Side,
		Quantity:      probe.Quantity,
		IntendedPrice: probe.IntendedPrice,
		ClientOrderID: probe.ClientOrderID,
	}, status, day)
	if err != nil {
		g.recordEvent("evaluate_failed")
		g.warnf("marketstatus gate: engine evaluate failed", "instrument", probe.InstrumentKey, "err", err)
		return broker.MarketStatusVerdict{}
	}

	verdict := broker.MarketStatusVerdict{}
	if res.Reject() {
		verdict.Rejected = true
	}
	for _, ev := range res.Events {
		// Persist every non-allow event for the audit trail.
		// Best-effort: a DB hiccup here doesn't change the
		// verdict.
		if _, perr := g.repo.InsertEvent(ctx, probe.FundID, probe.InstrumentKey, probe.Symbol, probe.ClientOrderID, ev); perr != nil {
			g.recordEvent("persist_failed")
			g.warnf("marketstatus gate: persist event failed", "rule", string(ev.RuleCode), "err", perr)
		}
		if ev.Decision == marketstatus.DecisionReject {
			if verdict.RejectReason == "" {
				verdict.RejectReason = string(ev.RuleCode) + ": " + ev.Summary
			}
			g.recordEvent("reject_" + string(ev.RuleCode))
		} else if ev.Decision == marketstatus.DecisionWarn {
			verdict.Warnings = append(verdict.Warnings, string(ev.RuleCode)+": "+ev.Summary)
			g.recordEvent("warn_" + string(ev.RuleCode))
		}
	}
	if !res.Reject() && len(res.Events) == 0 {
		g.recordEvent("allow")
	}
	return verdict
}

func (g *marketStatusGate) recordEvent(name string) {
	if g == nil || g.metrics == nil {
		return
	}
	g.metrics.RecordMarketStatusEvent(name)
}

func (g *marketStatusGate) warnf(msg string, kv ...any) {
	if g == nil || g.logger == nil {
		return
	}
	g.logger.Warn(msg, kv...)
}
