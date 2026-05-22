package backtest

import (
	"math"
	"sort"
	"strings"
	"time"
)

// portfolio is the runner's mutable per-symbol holdings + cash
// state. It tracks per-symbol cost basis using FIFO lots so the
// round-trip P&L attribution in metrics.go is deterministic.
//
// All quantity / cash arithmetic uses float64. We accept the
// floating-point error because backtest results are heuristic by
// definition (slippage is bps-rounded, prices are bar closes, etc.)
// — adding decimal/rational arithmetic doesn't buy real precision.
type portfolio struct {
	cash float64
	// positions maps Symbol → state (qty + lots). Symbol is the
	// canonical uppercase normalised key the runner uses
	// everywhere.
	positions map[string]*positionState
	// realizedPnL tracks lifetime realized P&L (cash basis). The
	// metrics helper uses this together with the per-trade marks
	// to count winning trades.
	realizedPnL float64
	// peakNav tracks the running NAV peak for drawdown
	// computation. Initialised to the opening NAV.
	peakNav float64
}

// positionState carries the current quantity and a FIFO queue of
// lots (cost basis tracking). Each lot is (qty, cost-per-share).
type positionState struct {
	quantity float64
	lots     []lot
}

type lot struct {
	qty       float64
	costPrice float64
}

// newPortfolio constructs a portfolio with the given cash and
// pre-loaded positions. Each InitialPosition becomes a single lot
// at its CostPrice.
func newPortfolio(cash float64, initial []InitialPosition) *portfolio {
	p := &portfolio{
		cash:      cash,
		positions: make(map[string]*positionState, len(initial)),
	}
	for _, ip := range initial {
		sym := strings.ToUpper(strings.TrimSpace(ip.Symbol))
		if sym == "" || ip.Quantity <= 0 {
			continue
		}
		st := p.positions[sym]
		if st == nil {
			st = &positionState{}
			p.positions[sym] = st
		}
		st.quantity += ip.Quantity
		st.lots = append(st.lots, lot{qty: ip.Quantity, costPrice: ip.CostPrice})
	}
	return p
}

// buy applies a buy order. quantity must be > 0; price is the
// post-slippage / post-commission fill price. Returns the order
// notional (cash deducted, positive number) and any error.
func (p *portfolio) buy(symbol string, quantity, price float64) (float64, error) {
	if quantity <= 0 || price <= 0 {
		return 0, errBadOrder
	}
	notional := quantity * price
	if notional > p.cash+1e-6 {
		return 0, errInsufficientCash
	}
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	st := p.positions[sym]
	if st == nil {
		st = &positionState{}
		p.positions[sym] = st
	}
	st.quantity += quantity
	st.lots = append(st.lots, lot{qty: quantity, costPrice: price})
	p.cash -= notional
	return notional, nil
}

// sell applies a sell order, consuming lots FIFO. Returns the cash
// proceeds (positive), the realized P&L delta, and any error. If
// quantity exceeds the held position we cap it at the holding;
// the runner is responsible for validating the trade before
// calling so this is a safety net.
func (p *portfolio) sell(symbol string, quantity, price float64) (float64, float64, error) {
	if quantity <= 0 || price <= 0 {
		return 0, 0, errBadOrder
	}
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	st := p.positions[sym]
	if st == nil || st.quantity <= 0 {
		return 0, 0, errNoPosition
	}
	if quantity > st.quantity {
		quantity = st.quantity
	}
	notional := quantity * price
	remaining := quantity
	pnl := 0.0
	// Pop FIFO lots until we've covered the sell quantity. We
	// allow partial lot consumption (qty < lot.qty) so the
	// residue stays in the queue at its original cost basis.
	for remaining > 1e-9 && len(st.lots) > 0 {
		head := st.lots[0]
		take := math.Min(head.qty, remaining)
		pnl += take * (price - head.costPrice)
		remaining -= take
		head.qty -= take
		if head.qty <= 1e-9 {
			st.lots = st.lots[1:]
		} else {
			st.lots[0] = head
		}
	}
	st.quantity -= quantity
	if st.quantity <= 1e-9 {
		st.quantity = 0
		st.lots = nil
		delete(p.positions, sym)
	}
	p.cash += notional
	p.realizedPnL += pnl
	return notional, pnl, nil
}

// quantityOf returns the held qty for a symbol; 0 when unheld.
func (p *portfolio) quantityOf(symbol string) float64 {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if st := p.positions[sym]; st != nil {
		return st.quantity
	}
	return 0
}

// snapshot produces a NavPoint for the given date using the
// supplied prices map (symbol → close). Symbols missing from
// prices contribute their last-known mark via lastPrices; if even
// that's missing, we mark-to-cost (sum of lot costPrice × qty) so
// the NAV doesn't collapse to zero on a stale row.
func (p *portfolio) snapshot(date time.Time, prices map[string]float64, lastPrices map[string]float64) NavPoint {
	positions := make(map[string]float64, len(p.positions))
	positionValue := 0.0
	for sym, st := range p.positions {
		positions[sym] = st.quantity
		px := lookupPrice(sym, prices, lastPrices)
		if px <= 0 {
			// fall back to weighted-average cost so NAV doesn't
			// collapse — better to mark-to-cost than mark-to-zero.
			px = weightedAvgCost(st)
		}
		positionValue += st.quantity * px
	}
	nav := p.cash + positionValue
	if nav > p.peakNav {
		p.peakNav = nav
	}
	drawdown := 0.0
	if p.peakNav > 0 {
		drawdown = (nav - p.peakNav) / p.peakNav
	}
	return NavPoint{
		Date:          date,
		Nav:           nav,
		Cash:          p.cash,
		Positions:     positions,
		PositionValue: positionValue,
		DrawdownPct:   drawdown,
	}
}

// totalNav is a tiny helper for the open-of-day NAV used by the
// engine to compute "X% of total assets" buy budgets. lastPrices
// is mandatory here because the engine wants a single number.
func (p *portfolio) totalNav(prices, lastPrices map[string]float64) float64 {
	value := p.cash
	for sym, st := range p.positions {
		px := lookupPrice(sym, prices, lastPrices)
		if px <= 0 {
			px = weightedAvgCost(st)
		}
		value += st.quantity * px
	}
	return value
}

// sortedSymbols returns held symbols in deterministic order. The
// runner needs determinism for the JSON output, otherwise two
// equivalent backtests render slightly different position maps.
func (p *portfolio) sortedSymbols() []string {
	out := make([]string, 0, len(p.positions))
	for sym := range p.positions {
		out = append(out, sym)
	}
	sort.Strings(out)
	return out
}

func lookupPrice(sym string, primary, fallback map[string]float64) float64 {
	if v, ok := primary[sym]; ok && v > 0 {
		return v
	}
	if v, ok := fallback[sym]; ok && v > 0 {
		return v
	}
	return 0
}

func weightedAvgCost(st *positionState) float64 {
	if st == nil || st.quantity <= 0 {
		return 0
	}
	var notional float64
	for _, l := range st.lots {
		notional += l.qty * l.costPrice
	}
	return notional / st.quantity
}

// errBadOrder / errInsufficientCash / errNoPosition are internal
// sentinel errors; the runner translates them into TradeEvent
// Status strings so the user-facing trade log reads cleanly.
var (
	errBadOrder         = errPortfolio("bad_order")
	errInsufficientCash = errPortfolio("no_cash")
	errNoPosition       = errPortfolio("no_qty")
)

type errPortfolio string

func (e errPortfolio) Error() string { return "portfolio: " + string(e) }
