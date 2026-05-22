// Package risk provides a pluggable risk policy DSL for evaluating an
// investment plan against a set of named, configurable risk rules.
//
// The package is deliberately decoupled from the rest of the platform: it
// works on small value structs (Position, ProposedTrade, MarketSnapshot) that
// any caller can populate from their own domain model. The intent is for
// agent.RiskAgent (and later, fund-level/firm-level policy stacks) to delegate
// individual rule logic here so that risk policies can be configured per fund
// rather than hard-coded.
//
// A Policy is an ordered list of Rules. Each Rule implements Evaluate, which
// is given the full PlanContext and returns zero or more Findings. Findings
// carry a Severity (info/warn/fail) plus a human-readable message and an
// optional Suggestion. The Evaluator aggregates Findings and derives a final
// Verdict.
package risk

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

// Side is the direction of a proposed trade.
type Side string

const (
	SideBuy    Side = "buy"
	SideSell   Side = "sell"
	SideAdd    Side = "add"
	SideReduce Side = "reduce"
)

// IsSell reports whether s reduces an existing long position.
func (s Side) IsSell() bool { return s == SideSell || s == SideReduce }

// Position represents a current holding in a portfolio.
type Position struct {
	Symbol      string
	Sector      string
	Quantity    float64
	AvgCost     float64
	MarketPrice float64
	MarketValue float64 // optional; computed if zero
	// AvailableQty is the unlocked portion of Quantity — shares that
	// have completed their settlement cycle and can be sold today. For
	// T+0 markets this equals Quantity; for T+1 markets (A-share)
	// shares bought during the current trading day are excluded until
	// the daily Settle step releases them. Zero is treated as "fully
	// available" so legacy positions persisted without the column keep
	// working (the SettlementCycleRule fails open in that case).
	AvailableQty float64
}

// Value returns the current market value of the position.
func (p Position) Value() float64 {
	if p.MarketValue > 0 {
		return p.MarketValue
	}
	return p.Quantity * p.MarketPrice
}

// ProposedTrade is a single buy/sell action under evaluation.
type ProposedTrade struct {
	Symbol   string
	Side     Side
	Quantity float64
	Price    float64
	Amount   float64 // notional; computed from qty*price if zero
	Sector   string  // optional; falls back to portfolio if blank
	// QuoteAsOf is the upstream timestamp of the price used to price this
	// trade. Zero value means "unknown" (rule treats as no signal).
	QuoteAsOf time.Time
	// QuoteIsStale is the freshness flag computed by the market-data layer
	// (see marketdata.isQuoteStale). Carried into the risk gate so we can
	// hard-reject buys on stale data even if QuoteAsOf alone wouldn't have
	// breached this rule's local threshold (the market-data layer's
	// threshold can be tighter than the risk gate's).
	QuoteIsStale bool
	// ExecutionPrice is the live quote price observed at execution time,
	// used by SlippageGuard to compute drift against the plan reference
	// price (Price). Zero means slippage check is not applicable
	// (evaluation phase before any live quote was pulled, or sell-side
	// trades that are exempt by design).
	ExecutionPrice float64
	// Market / Exchange / AssetClass carry instrument metadata so rules
	// that need to disambiguate by venue (e.g. SlippageGuard board
	// tolerances) can do so without a separate lookup. All optional.
	Market     string
	Exchange   string
	AssetClass string
}

// Notional returns the notional value of the trade.
func (t ProposedTrade) Notional() float64 {
	if t.Amount > 0 {
		return t.Amount
	}
	return t.Quantity * t.Price
}

// ExecutedTrade is a compact representation of already-created trades used by
// hard controls such as daily frequency limits.
type ExecutedTrade struct {
	Symbol     string
	Side       Side
	Quantity   float64
	Price      float64
	Amount     float64
	Status     string
	ExecutedAt time.Time
}

// MarketSnapshot exposes auxiliary market data the policy may need (returns,
// volumes, correlations). All maps are keyed by symbol.
type MarketSnapshot struct {
	// HistoricalReturns is the time-series of daily returns per symbol used
	// for VaR estimation. The newest sample is last.
	HistoricalReturns map[string][]float64
	// AvgVolume is the trailing-N-day average daily volume per symbol.
	AvgVolume map[string]float64
	// Correlations is an optional precomputed pairwise correlation matrix.
	// If absent, the evaluator computes it on the fly from HistoricalReturns.
	Correlations map[string]map[string]float64
	// StressShocks describes percent shocks for named scenarios. The keys of
	// the inner map are sectors (or "*" for portfolio-wide).
	StressShocks map[string]map[string]float64
	// AsOf is the snapshot timestamp (informational only).
	AsOf time.Time
}

// PlanContext is the full input to the policy evaluator.
type PlanContext struct {
	PlanID      string
	TotalAssets float64
	Cash        float64
	Positions   []Position
	Trades      []ProposedTrade
	TradesToday []ExecutedTrade
	DailyReturn float64
	Market      MarketSnapshot
	// SectorOverrides allows callers to inject sector metadata for symbols
	// that don't have a sector on the Position (e.g. brand new buys). Keys
	// are symbols.
	SectorOverrides map[string]string
}

// SectorOf returns the resolved sector for a symbol, preferring an explicit
// override, then existing positions, then trade.Sector.
func (c PlanContext) SectorOf(symbol string) string {
	if s, ok := c.SectorOverrides[symbol]; ok && s != "" {
		return s
	}
	for _, p := range c.Positions {
		if p.Symbol == symbol && p.Sector != "" {
			return p.Sector
		}
	}
	for _, t := range c.Trades {
		if t.Symbol == symbol && t.Sector != "" {
			return t.Sector
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Findings & verdict
// ---------------------------------------------------------------------------

// Severity captures how serious a finding is. Severities are ordered
// info < warn < fail.
type Severity string

const (
	SeverityInfo Severity = "info"
	SeverityWarn Severity = "warn"
	SeverityFail Severity = "fail"
)

// Finding is the result of a single rule evaluation.
type Finding struct {
	Rule       string
	Severity   Severity
	Symbol     string  // optional
	Current    float64 // current measured value (e.g. ratio)
	Threshold  float64 // configured limit
	Message    string
	Suggestion string
}

// Verdict is the aggregated outcome of a policy evaluation.
type Verdict string

const (
	VerdictApproved          Verdict = "approved"
	VerdictApprovedWithWarns Verdict = "approved_with_warnings"
	VerdictRejected          Verdict = "rejected"
)

// Report is the full evaluator output.
type Report struct {
	PlanID      string
	Verdict     Verdict
	Findings    []Finding
	EvaluatedAt time.Time
}

// HasFail reports whether any finding was a hard failure.
func (r Report) HasFail() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityFail {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Rule interface
// ---------------------------------------------------------------------------

// Rule is a single risk check.
type Rule interface {
	// Name is a stable identifier used in Findings.
	Name() string
	// Evaluate runs the rule against ctx and returns zero or more findings.
	Evaluate(ctx context.Context, pc PlanContext) ([]Finding, error)
}

// Policy is an ordered list of rules.
type Policy struct {
	Name  string
	Rules []Rule
}

// ---------------------------------------------------------------------------
// Evaluator
// ---------------------------------------------------------------------------

// Evaluator runs a Policy against a PlanContext.
type Evaluator struct {
	Policy Policy
}

// NewEvaluator constructs an Evaluator.
func NewEvaluator(p Policy) *Evaluator { return &Evaluator{Policy: p} }

// Evaluate runs every rule and aggregates findings into a Report. Rule errors
// are surfaced as fail-severity findings rather than aborting the run, so the
// caller always gets a complete picture.
func (e *Evaluator) Evaluate(ctx context.Context, pc PlanContext) Report {
	report := Report{PlanID: pc.PlanID, EvaluatedAt: time.Now()}
	for _, r := range e.Policy.Rules {
		fs, err := r.Evaluate(ctx, pc)
		if err != nil {
			report.Findings = append(report.Findings, Finding{
				Rule:     r.Name(),
				Severity: SeverityFail,
				Message:  fmt.Sprintf("%s evaluation failed: %v", r.Name(), err),
			})
			continue
		}
		report.Findings = append(report.Findings, fs...)
	}
	report.Verdict = deriveVerdict(report.Findings)
	return report
}

func deriveVerdict(findings []Finding) Verdict {
	hasFail, hasWarn := false, false
	for _, f := range findings {
		switch f.Severity {
		case SeverityFail:
			hasFail = true
		case SeverityWarn:
			hasWarn = true
		}
	}
	switch {
	case hasFail:
		return VerdictRejected
	case hasWarn:
		return VerdictApprovedWithWarns
	default:
		return VerdictApproved
	}
}

// ---------------------------------------------------------------------------
// Helpers shared across rules
// ---------------------------------------------------------------------------

// portfolioValue is the post-trade aggregate market value.
func portfolioValue(pc PlanContext) float64 {
	v := 0.0
	for _, p := range pc.Positions {
		v += p.Value()
	}
	return v
}

// sectorExposurePostTrade returns sector -> notional after applying trades.
func sectorExposurePostTrade(pc PlanContext) map[string]float64 {
	exp := map[string]float64{}
	for _, p := range pc.Positions {
		if p.Sector == "" {
			continue
		}
		exp[p.Sector] += p.Value()
	}
	for _, t := range pc.Trades {
		sector := t.Sector
		if sector == "" {
			sector = pc.SectorOf(t.Symbol)
		}
		if sector == "" {
			continue
		}
		delta := t.Notional()
		if t.Side.IsSell() {
			delta = -delta
		}
		exp[sector] += delta
	}
	return exp
}

// projectedExposurePostTrade returns symbol -> projected market value.
func projectedExposurePostTrade(pc PlanContext) map[string]float64 {
	exp := map[string]float64{}
	for _, p := range pc.Positions {
		exp[p.Symbol] += p.Value()
	}
	for _, t := range pc.Trades {
		delta := t.Notional()
		if t.Side.IsSell() {
			delta = -delta
		}
		exp[t.Symbol] += delta
	}
	return exp
}

// sortedKeys returns the keys of m in deterministic order. Useful for stable
// finding ordering inside rules.
func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// fmtPct formats a ratio as a percentage string with two decimals.
func fmtPct(ratio float64) string { return fmt.Sprintf("%.2f%%", ratio*100) }
