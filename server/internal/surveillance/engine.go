// engine.go — orchestrates configured rules over a TradeSnapshot
// stream, dedupes by fingerprint, and produces a RunResult.
//
// Why a dedicated Engine
//
// Rules can flag the same trade twice (a self-trade pair is
// trivially a wash in some shapes); the engine deduplicates by
// fingerprint so a single trade pattern produces ONE Event row,
// not N copies.
//
// The Engine is stateless aside from its rule list; the Repo
// adds idempotency on the persist side via the unique index.

package surveillance

import "time"

// Engine runs a configured set of rules over input.
type Engine struct {
	rules []Rule
}

// NewEngine builds an engine with the given rules. Pass
// DefaultRules() for the production set.
func NewEngine(rules ...Rule) *Engine {
	return &Engine{rules: rules}
}

// DefaultRules returns the production rule set. Order matters
// only insofar as the dedup keeps the FIRST rule that fires for
// a given fingerprint — we put the most specific rule
// (self_trade_pair) first so it wins over wash_trade for the
// same trade pair when both would fire.
func DefaultRules() []Rule {
	return []Rule{
		NewSelfTradePairRule(DefaultSelfTradePairOptions),
		NewWashTradeRule(DefaultWashTradeOptions),
		NewMarkingCloseRule(DefaultMarkingCloseOptions),
	}
}

// Run executes every rule, dedupes, and returns a RunResult.
func (e *Engine) Run(snap []TradeSnapshot, ctx *MarketContext) RunResult {
	out := RunResult{
		CountsBySeverity: map[Severity]int{},
		CountsByRule:     map[RuleCode]int{},
	}
	if e == nil || len(snap) == 0 {
		return out
	}
	seen := map[string]struct{}{}
	now := time.Now().UTC()
	for _, rule := range e.rules {
		evts := rule.Detect(snap, ctx)
		for _, ev := range evts {
			if ev.Fingerprint == "" {
				ev.Fingerprint = fingerprintFor(ev.FundID, ev.RuleCode, ev.TradeIDs)
			}
			if _, ok := seen[ev.Fingerprint]; ok {
				continue
			}
			seen[ev.Fingerprint] = struct{}{}
			if ev.Status == "" {
				ev.Status = StatusOpen
			}
			if ev.DetectorVersion == "" {
				ev.DetectorVersion = detectorVersion
			}
			if ev.DetectedAt.IsZero() {
				ev.DetectedAt = now
			}
			out.Events = append(out.Events, ev)
			out.CountsBySeverity[ev.Severity]++
			out.CountsByRule[ev.RuleCode]++
		}
	}
	return out
}
