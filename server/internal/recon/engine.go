// engine.go — pure diff engine for P1-3 reconciliation.
//
// Why "pure"
//
// The engine takes a Statement + InternalSnapshot and emits
// []Break. No I/O, no DB, no clock. That's what makes it trivially
// testable: golden fixtures + a one-line assertion on the break
// list.
//
// Three diff phases
//
//   1. Positions   — match by (canonicalised) symbol, compare qty
//                    and avg-cost.
//   2. Cash        — match by (canonicalised) currency, compare
//                    balance.
//   3. Trades      — match by broker_trade_id ↔ external_ref. For
//                    each matched pair, compare side / qty / price.
//                    For unmatched, emit either trade_missing_internal
//                    or trade_missing_broker.
//
// What the engine does NOT do
//
//   - Recover from broker pre-rounding. If our internal qty is
//     105.0 and the broker statement is 105 (integer), they're
//     equal under the float compare; if it's 105.0001 we flag
//     it. Tolerances handle this; we don't try to be smarter than
//     the operator who set the tolerance.
//   - Auto-fix anything. The break is the artefact; resolution
//     belongs in the admin UI.

package recon

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Tolerances sets the per-field equality bands used when emitting
// breaks. Default values come from `DefaultTolerances`. Cash uses
// an absolute band because dust under $1 isn't actionable; quantity
// uses a tighter absolute band because a fractional share *can* be
// real (FX-rate-driven NAV ÷ price quotient on a 401k contribution).
type Tolerances struct {
	// QuantityAbs — max absolute quantity drift that's "OK".
	QuantityAbs float64
	// AvgCostAbs — max absolute avg-cost drift that's "OK".
	AvgCostAbs float64
	// AvgCostPct — relative band; the engine uses MAX(abs, pct*broker)
	// so a $200 stock with $0.001 drift doesn't fire a break solely
	// because pct = 0.005% × 200 = $0.01 < $0.001.
	AvgCostPct float64
	// CashAbs — absolute cash drift allowed.
	CashAbs float64
	// CashPct — relative band.
	CashPct float64
	// TradePriceAbs — absolute price drift (per-share) allowed when
	// matching a trade pair.
	TradePriceAbs float64
	// TradePricePct — relative band.
	TradePricePct float64
	// TradeQuantityAbs — absolute quantity drift on a trade pair.
	TradeQuantityAbs float64
}

// DefaultTolerances captures the bands the daily loop runs with.
// Hand-tuned: aimed at catching real settlement drift without
// firing on normal float rounding.
var DefaultTolerances = Tolerances{
	QuantityAbs:      0.000001,
	AvgCostAbs:       0.0001,
	AvgCostPct:       0.00005, // 0.005% of broker avg cost
	CashAbs:          0.01,    // 1¢
	CashPct:          0.00001, // 0.001% of broker balance
	TradePriceAbs:    0.0001,
	TradePricePct:    0.00005,
	TradeQuantityAbs: 0.000001,
}

// Engine is the diff orchestrator. Stateless; safe to share.
type Engine struct {
	tolerances Tolerances
}

// NewEngine builds an engine with the given tolerances. Pass
// `DefaultTolerances` for production; pass a stricter set for
// month-end / audit runs.
func NewEngine(t Tolerances) *Engine {
	if t == (Tolerances{}) {
		t = DefaultTolerances
	}
	return &Engine{tolerances: t}
}

// Result is what the engine returns. The caller persists the run +
// breaks in one tx so the work queue is consistent.
type Result struct {
	Breaks []Break
	// Counts is a convenience map keyed by severity. The Repo derives
	// reconciliation_runs.break_count_* from it.
	Counts map[Severity]int
}

// Diff is the top-level entry. Returns a Result; never an error
// (the caller has already validated the snapshot + statement).
func (e *Engine) Diff(s *Statement, snap *InternalSnapshot) Result {
	if s == nil || snap == nil {
		return Result{Counts: map[Severity]int{}}
	}
	out := Result{Counts: map[Severity]int{}}

	// 1) Positions.
	out.Breaks = append(out.Breaks, e.diffPositions(s, snap)...)

	// 2) Cash.
	out.Breaks = append(out.Breaks, e.diffCash(s, snap)...)

	// 3) Trades.
	out.Breaks = append(out.Breaks, e.diffTrades(s, snap)...)

	// Stable order: severity DESC then break_type then symbol.
	sortBreaks(out.Breaks)

	for _, b := range out.Breaks {
		out.Counts[b.Severity]++
	}
	return out
}

// ----- positions diff -----

func (e *Engine) diffPositions(s *Statement, snap *InternalSnapshot) []Break {
	var breaks []Break
	internalBySymbol := map[string]InternalPosition{}
	for _, p := range snap.Positions {
		internalBySymbol[canonicalSymbol(p.Symbol)] = p
	}
	brokerBySymbol := map[string]StatementPosition{}
	for _, p := range s.Positions {
		brokerBySymbol[canonicalSymbol(p.Symbol)] = p
	}

	// Walk broker side.
	for sym, bp := range brokerBySymbol {
		ip, ok := internalBySymbol[sym]
		if !ok {
			// Broker has it, we don't — could be an unknown
			// fill we missed. CRITICAL, because it usually means
			// internal positions are wrong.
			breaks = append(breaks, Break{
				FundID:        snap.FundID,
				Type:          BreakPositionMissingInternal,
				Severity:      SeverityCritical,
				Symbol:        sym,
				Currency:      canonicalCurrency(bp.Currency),
				BrokerValue:   ptrFloat(bp.Quantity),
				InternalValue: ptrFloat(0),
				DiffValue:     ptrFloat(bp.Quantity),
				Description: fmt.Sprintf(
					"Broker reports %s qty=%v, internal has no position",
					sym, bp.Quantity,
				),
			})
			continue
		}
		// Quantity diff.
		dq := ip.Quantity - bp.Quantity
		if math.Abs(dq) > e.tolerances.QuantityAbs {
			breaks = append(breaks, Break{
				FundID:        snap.FundID,
				Type:          BreakPositionQuantityMismatch,
				Severity:      severityForQty(dq, bp.Quantity),
				Symbol:        sym,
				Currency:      canonicalCurrency(bp.Currency),
				InternalValue: ptrFloat(ip.Quantity),
				BrokerValue:   ptrFloat(bp.Quantity),
				DiffValue:     ptrFloat(dq),
				DiffPercent:   diffPercent(ip.Quantity, bp.Quantity),
				Description: fmt.Sprintf(
					"Quantity drift: internal=%v broker=%v Δ=%v",
					ip.Quantity, bp.Quantity, dq,
				),
			})
		}
		// Avg-cost diff.
		dCost := ip.AvgCost - bp.AvgCost
		costBand := math.Max(e.tolerances.AvgCostAbs, math.Abs(bp.AvgCost)*e.tolerances.AvgCostPct)
		if math.Abs(dCost) > costBand {
			breaks = append(breaks, Break{
				FundID:        snap.FundID,
				Type:          BreakPositionAvgCostMismatch,
				Severity:      SeverityWarning,
				Symbol:        sym,
				Currency:      canonicalCurrency(bp.Currency),
				InternalValue: ptrFloat(ip.AvgCost),
				BrokerValue:   ptrFloat(bp.AvgCost),
				DiffValue:     ptrFloat(dCost),
				DiffPercent:   diffPercent(ip.AvgCost, bp.AvgCost),
				Description: fmt.Sprintf(
					"Avg-cost drift: internal=%v broker=%v",
					ip.AvgCost, bp.AvgCost,
				),
			})
		}
	}

	// Walk internal side for symbols broker didn't report.
	for sym, ip := range internalBySymbol {
		if _, ok := brokerBySymbol[sym]; ok {
			continue
		}
		// Internal has a position, broker doesn't — could be a
		// fully-sold position whose broker statement leg already
		// flushed. CRITICAL only if quantity is non-trivial.
		sev := SeverityCritical
		if math.Abs(ip.Quantity) <= e.tolerances.QuantityAbs {
			// Internal qty rounds to zero; downgrade to info.
			sev = SeverityInfo
		}
		breaks = append(breaks, Break{
			FundID:        snap.FundID,
			Type:          BreakPositionMissingBroker,
			Severity:      sev,
			Symbol:        sym,
			Currency:      canonicalCurrency(ip.Currency),
			InternalValue: ptrFloat(ip.Quantity),
			BrokerValue:   ptrFloat(0),
			DiffValue:     ptrFloat(ip.Quantity),
			Description: fmt.Sprintf(
				"Internal has %s qty=%v, broker statement omits",
				sym, ip.Quantity,
			),
		})
	}
	return breaks
}

// severityForQty returns CRITICAL when the quantity drift is more
// than 1% of the broker's reported size, WARNING otherwise. The
// rationale: a 1-share drift on a 5-share position is suspicious;
// the same drift on a 10,000-share position is settlement noise we
// still want to track but not page on.
func severityForQty(diff, brokerQty float64) Severity {
	if brokerQty == 0 {
		return SeverityCritical
	}
	rel := math.Abs(diff / brokerQty)
	if rel > 0.01 {
		return SeverityCritical
	}
	return SeverityWarning
}

// ----- cash diff -----

func (e *Engine) diffCash(s *Statement, snap *InternalSnapshot) []Break {
	var breaks []Break
	internalByCcy := map[string]InternalCash{}
	for _, c := range snap.Cash {
		internalByCcy[canonicalCurrency(c.Currency)] = c
	}
	brokerByCcy := map[string]StatementCash{}
	for _, c := range s.Cash {
		brokerByCcy[canonicalCurrency(c.Currency)] = c
	}

	for ccy, bc := range brokerByCcy {
		ic, ok := internalByCcy[ccy]
		if !ok {
			breaks = append(breaks, Break{
				FundID:        snap.FundID,
				Type:          BreakCashCurrencyMissingInternal,
				Severity:      SeverityCritical,
				Currency:      ccy,
				InternalValue: ptrFloat(0),
				BrokerValue:   ptrFloat(bc.Balance),
				DiffValue:     ptrFloat(bc.Balance),
				Description: fmt.Sprintf(
					"Broker reports %s cash %v, internal has no entry",
					ccy, bc.Balance,
				),
			})
			continue
		}
		d := ic.Balance - bc.Balance
		band := math.Max(e.tolerances.CashAbs, math.Abs(bc.Balance)*e.tolerances.CashPct)
		if math.Abs(d) > band {
			breaks = append(breaks, Break{
				FundID:        snap.FundID,
				Type:          BreakCashBalanceMismatch,
				Severity:      severityForCash(d, bc.Balance),
				Currency:      ccy,
				InternalValue: ptrFloat(ic.Balance),
				BrokerValue:   ptrFloat(bc.Balance),
				DiffValue:     ptrFloat(d),
				DiffPercent:   diffPercent(ic.Balance, bc.Balance),
				Description: fmt.Sprintf(
					"Cash drift: internal=%v broker=%v Δ=%v",
					ic.Balance, bc.Balance, d,
				),
			})
		}
	}

	for ccy, ic := range internalByCcy {
		if _, ok := brokerByCcy[ccy]; ok {
			continue
		}
		sev := SeverityCritical
		if math.Abs(ic.Balance) <= e.tolerances.CashAbs {
			sev = SeverityInfo
		}
		breaks = append(breaks, Break{
			FundID:        snap.FundID,
			Type:          BreakCashCurrencyMissingBroker,
			Severity:      sev,
			Currency:      ccy,
			InternalValue: ptrFloat(ic.Balance),
			BrokerValue:   ptrFloat(0),
			DiffValue:     ptrFloat(ic.Balance),
			Description: fmt.Sprintf(
				"Internal has %s cash %v, broker statement omits currency",
				ccy, ic.Balance,
			),
		})
	}
	return breaks
}

func severityForCash(diff, brokerBal float64) Severity {
	if brokerBal == 0 {
		return SeverityCritical
	}
	rel := math.Abs(diff / brokerBal)
	if rel > 0.005 {
		return SeverityCritical
	}
	return SeverityWarning
}

// ----- trades diff -----

func (e *Engine) diffTrades(s *Statement, snap *InternalSnapshot) []Break {
	var breaks []Break

	internalByRef := map[string]InternalTrade{}
	for _, t := range snap.Trades {
		key := strings.TrimSpace(t.ExternalRef)
		if key == "" {
			continue
		}
		internalByRef[key] = t
	}
	brokerByID := map[string]StatementTrade{}
	for _, t := range s.Trades {
		brokerByID[strings.TrimSpace(t.BrokerTradeID)] = t
	}

	matched := map[string]struct{}{}

	for id, bt := range brokerByID {
		// Match by broker_trade_id first (the canonical link). If
		// missing, try broker_order_id since some venues only emit
		// the order id on the EOD statement.
		var (
			it      InternalTrade
			matchOK bool
		)
		if cand, ok := internalByRef[id]; ok {
			it, matchOK = cand, true
		} else if oid := strings.TrimSpace(bt.BrokerOrderID); oid != "" {
			if cand, ok := internalByRef[oid]; ok {
				it, matchOK = cand, true
			}
		}
		if !matchOK {
			breaks = append(breaks, Break{
				FundID:      snap.FundID,
				Type:        BreakTradeMissingInternal,
				Severity:    SeverityCritical,
				Symbol:      canonicalSymbol(bt.Symbol),
				Currency:    canonicalCurrency(bt.Currency),
				BrokerValue: ptrFloat(bt.Quantity),
				Description: fmt.Sprintf(
					"Broker trade %s (%s %v @ %v) not found in internal trades",
					id, bt.Side, bt.Quantity, bt.Price,
				),
				Metadata: map[string]any{
					"broker_trade_id": id,
					"broker_order_id": bt.BrokerOrderID,
				},
			})
			continue
		}
		matched[it.ExternalRef] = struct{}{}

		// Side mismatch — CRITICAL: a buy reported as sell is a
		// data corruption bug, not a tolerance issue.
		if !strings.EqualFold(it.Side, bt.Side) {
			breaks = append(breaks, Break{
				FundID:   snap.FundID,
				Type:     BreakTradeSideMismatch,
				Severity: SeverityCritical,
				Symbol:   canonicalSymbol(bt.Symbol),
				Description: fmt.Sprintf(
					"Trade %s side: internal=%s broker=%s",
					id, it.Side, bt.Side,
				),
				Metadata: map[string]any{
					"broker_trade_id": id,
					"internal_side":   it.Side,
					"broker_side":     bt.Side,
				},
			})
		}

		// Quantity diff.
		dq := it.Quantity - bt.Quantity
		if math.Abs(dq) > e.tolerances.TradeQuantityAbs {
			breaks = append(breaks, Break{
				FundID:        snap.FundID,
				Type:          BreakTradeQuantityMismatch,
				Severity:      severityForQty(dq, bt.Quantity),
				Symbol:        canonicalSymbol(bt.Symbol),
				InternalValue: ptrFloat(it.Quantity),
				BrokerValue:   ptrFloat(bt.Quantity),
				DiffValue:     ptrFloat(dq),
				DiffPercent:   diffPercent(it.Quantity, bt.Quantity),
				Description: fmt.Sprintf(
					"Trade %s qty: internal=%v broker=%v",
					id, it.Quantity, bt.Quantity,
				),
				Metadata: map[string]any{"broker_trade_id": id},
			})
		}

		// Price diff.
		dp := it.Price - bt.Price
		band := math.Max(e.tolerances.TradePriceAbs, math.Abs(bt.Price)*e.tolerances.TradePricePct)
		if math.Abs(dp) > band {
			breaks = append(breaks, Break{
				FundID:        snap.FundID,
				Type:          BreakTradePriceMismatch,
				Severity:      SeverityWarning,
				Symbol:        canonicalSymbol(bt.Symbol),
				InternalValue: ptrFloat(it.Price),
				BrokerValue:   ptrFloat(bt.Price),
				DiffValue:     ptrFloat(dp),
				DiffPercent:   diffPercent(it.Price, bt.Price),
				Description: fmt.Sprintf(
					"Trade %s price: internal=%v broker=%v",
					id, it.Price, bt.Price,
				),
				Metadata: map[string]any{"broker_trade_id": id},
			})
		}
	}

	// Internal trades the broker didn't report.
	for ref, it := range internalByRef {
		if _, ok := matched[ref]; ok {
			continue
		}
		breaks = append(breaks, Break{
			FundID:        snap.FundID,
			Type:          BreakTradeMissingBroker,
			Severity:      SeverityCritical,
			Symbol:        canonicalSymbol(it.Symbol),
			Currency:      canonicalCurrency(it.Currency),
			InternalValue: ptrFloat(it.Quantity),
			BrokerValue:   ptrFloat(0),
			Description: fmt.Sprintf(
				"Internal trade %s (%s %v @ %v) not in broker statement",
				ref, it.Side, it.Quantity, it.Price,
			),
			Metadata: map[string]any{
				"internal_external_ref": ref,
			},
		})
	}
	return breaks
}

// ----- helpers -----

func ptrFloat(f float64) *float64 { return &f }

// diffPercent returns ((a - b) / b) * 100 as a *float64; nil when
// the divisor is 0 to avoid Inf serialising into JSON.
func diffPercent(a, b float64) *float64 {
	if b == 0 {
		return nil
	}
	v := (a - b) / b * 100.0
	return &v
}

func severityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 0
	case SeverityWarning:
		return 1
	case SeverityInfo:
		return 2
	}
	return 3
}

func sortBreaks(b []Break) {
	sort.SliceStable(b, func(i, j int) bool {
		if a, c := severityRank(b[i].Severity), severityRank(b[j].Severity); a != c {
			return a < c
		}
		if b[i].Type != b[j].Type {
			return string(b[i].Type) < string(b[j].Type)
		}
		return b[i].Symbol < b[j].Symbol
	})
}
