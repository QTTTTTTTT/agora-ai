// Card K-4 — B-side independent lot ledger for AB shadow runs.
//
// Why a separate ledger (instead of reusing fundlots.LotLedger):
//
//   - The production lot ledger persists to `position_lots` and
//     drives real PnL/tax accounting. Mixing synthetic AB shadow
//     lots with real lots would corrupt ANY downstream consumer
//     (attribution, fund summary, Compose Daily Reports, etc.).
//   - The shadow run is short-lived: it lives for the duration
//     of one `AnalyzeTest` call. Persisting it would also mean
//     migrations, RLS, replication, all for data that's
//     thrown away after the analyze finishes.
//   - We need a much narrower API: just "apply a B trade",
//     "snapshot positions + cash", "report realized PnL". We
//     don't need cost-basis tax lots, FIFO/LIFO/HIFO toggles,
//     wash-sale detection, etc.
//
// Scope of v1 (this file):
//
//   - Long-only. Shorts are out — the AB shadow path doesn't
//     short today, and supporting it would double the test
//     surface for a feature nobody is using.
//   - FIFO matching. The most defensible default; matches the
//     production ledger's behaviour so dashboards stay consistent.
//   - Defensive clamp on over-sell. The LLM might (rarely) try
//     to sell more than B holds — we clamp to held quantity
//     instead of going short. This is a safety net, not a feature.
//   - Cash can go negative. The B side starts with the same
//     initial capital as A (control fund's current_capital at
//     test creation) — if the LLM scales up trades aggressively
//     we let it overdraw; the resulting NAV will simply look bad,
//     which is the correct economic signal. Margin modeling is
//     out of scope.
//   - No fees / slippage. K-3 will plug in a fee model if needed.
//
// The K-3 NAV recompute consumer pattern:
//
//   ledger := newBSideLotLedger(initialCash)
//   for _, navDate := range aNavDates {
//     for len(queue) > 0 && !queue[0].Date.After(navDate) {
//       ledger.Apply(queue[0])
//       queue = queue[1:]
//     }
//     positions, cash := ledger.PositionsAndCash()
//     marketValue := mtm(positions, navDate)
//     bNAV := (cash + marketValue) / initialCash * baseline
//   }
//
// All methods are nil-safe and side-effect-free except Apply.
// No mutex — the ledger is single-threaded inside one analyze
// run. If we ever fan out the analyze loop, we'd add one here.
package main

import (
	"sort"
	"strings"
	"time"

	"github.com/fundai/server/internal/repository"
)

// bSideLot is one open position layer. We use a flat slice
// (FIFO queue) per symbol rather than a balanced tree because
// the typical AB shadow run touches < 100 lots — linear walk is
// faster than tree maintenance overhead.
type bSideLot struct {
	OpenedAt  time.Time
	Symbol    string
	Quantity  float64 // remaining open quantity; mutates as SELLs match
	CostBasis float64 // per-share cost when this lot was opened
}

// bSideLotApplyResult is the side-channel result of Apply for
// callers that want to record per-trade economics (e.g., write
// `realized_pnl` into ab_test_variant_trades). All numeric
// fields are zero on a no-op.
type bSideLotApplyResult struct {
	// Applied is the actual quantity that landed in the ledger.
	// For a BUY this is the input qty. For a SELL this is the
	// FIFO-clamped fill (≤ held quantity). 0 means the call
	// was a no-op (over-sell with no inventory, qty <= 0, etc.)
	Applied float64

	// CashDelta is the signed cash change. BUY → negative,
	// SELL → positive. Doesn't include fees (none in v1).
	CashDelta float64

	// RealizedPnL is non-zero only for SELL fills; computed as
	// sum(fillQty * (sellPrice - lotCostBasis)) across the lots
	// that were closed.
	RealizedPnL float64

	// Clamped is true when a SELL was clamped to held quantity
	// (i.e., the LLM tried to over-sell). Surface this in metrics
	// or logs so operators can see when the synthetic path is
	// drifting from realistic behaviour.
	Clamped bool
}

// bSideLotLedger is the per-analyze-run in-memory ledger.
// Construct via newBSideLotLedger; do NOT zero-init by hand —
// the lot map needs to be allocated.
type bSideLotLedger struct {
	cash        float64
	initial     float64
	realizedPnL float64

	// lots[symbol] is a FIFO queue of open lots, oldest first.
	// We keep zero-quantity lots out by trimming after each fill.
	lots map[string][]bSideLot

	// trades is an append-only history of every Apply call that
	// changed state. Useful for debugging diff between A and B
	// trade sequences when AB compare looks suspicious.
	trades []bSideAppliedTrade
}

// bSideAppliedTrade mirrors the inputs to Apply plus the result.
// Stored on the ledger so K-3 can pluck it back out and write
// `ab_test_variant_trades` rows with realized_pnl populated.
type bSideAppliedTrade struct {
	Date     time.Time
	Symbol   string
	Side     string // "BUY" or "SELL", upper-cased
	Quantity float64
	Price    float64
	Result   bSideLotApplyResult
}

// newBSideLotLedger builds an empty ledger seeded with the given
// initial cash. A zero or negative initial is allowed (it just
// means "B starts with no cash and any BUY drives cash negative")
// — useful for unit tests but should never happen in production
// because the wiring path passes `funds.current_capital`.
func newBSideLotLedger(initialCash float64) *bSideLotLedger {
	return &bSideLotLedger{
		cash:    initialCash,
		initial: initialCash,
		lots:    map[string][]bSideLot{},
		trades:  []bSideAppliedTrade{},
	}
}

// Apply ingests one B trade. The contract:
//
//   - Caller MUST pass trades in non-decreasing date order. We
//     don't sort internally because the analyze loop already has
//     a sorted A trade list and B trades follow the same order.
//   - side is normalized to upper-case. Anything other than BUY
//     or SELL is treated as a no-op (and recorded as such in the
//     trade history for visibility).
//   - qty <= 0 or price < 0 → no-op. Defensive against the LLM
//     occasionally returning degenerate decisions.
//
// Returns the apply result so the caller can record realized
// PnL on the per-trade row.
func (l *bSideLotLedger) Apply(date time.Time, symbol, side string, qty, price float64) bSideLotApplyResult {
	if l == nil {
		return bSideLotApplyResult{}
	}
	side = strings.ToUpper(strings.TrimSpace(side))
	symbol = strings.TrimSpace(symbol)
	res := bSideLotApplyResult{}
	if symbol == "" || qty <= 0 || price < 0 {
		l.trades = append(l.trades, bSideAppliedTrade{
			Date: date, Symbol: symbol, Side: side, Quantity: qty, Price: price,
			Result: res,
		})
		return res
	}
	switch side {
	case "BUY":
		l.lots[symbol] = append(l.lots[symbol], bSideLot{
			OpenedAt:  date,
			Symbol:    symbol,
			Quantity:  qty,
			CostBasis: price,
		})
		res.Applied = qty
		res.CashDelta = -qty * price
		l.cash += res.CashDelta
	case "SELL":
		queue := l.lots[symbol]
		held := 0.0
		for _, lot := range queue {
			held += lot.Quantity
		}
		if held <= 0 {
			// Cannot sell what we don't hold. No-op rather than
			// going short.
			res.Clamped = true
			break
		}
		fillQty := qty
		if fillQty > held {
			res.Clamped = true
			fillQty = held
		}
		remaining := fillQty
		pnl := 0.0
		// FIFO walk. We mutate a copy of the slice header but
		// modify lot quantities in place via index access.
		for remaining > 0 && len(queue) > 0 {
			take := remaining
			if take > queue[0].Quantity {
				take = queue[0].Quantity
			}
			pnl += take * (price - queue[0].CostBasis)
			queue[0].Quantity -= take
			remaining -= take
			if queue[0].Quantity <= 1e-9 {
				// Drop empty front lot. Use slice trick rather
				// than append-copy to keep per-trade allocation
				// at zero.
				queue = queue[1:]
			}
		}
		l.lots[symbol] = queue
		res.Applied = fillQty
		res.CashDelta = fillQty * price
		res.RealizedPnL = pnl
		l.cash += res.CashDelta
		l.realizedPnL += pnl
	default:
		// Unknown side — record but don't touch state.
	}
	l.trades = append(l.trades, bSideAppliedTrade{
		Date: date, Symbol: symbol, Side: side, Quantity: qty, Price: price,
		Result: res,
	})
	return res
}

// PositionsAndCash returns the current snapshot. The positions
// map is owned by the ledger; callers must NOT mutate it. (We
// don't return a copy because K-3 only reads, and zero-allocation
// per NAV date is worth the discipline.)
//
// Symbols with zero open quantity are not present in the result.
func (l *bSideLotLedger) PositionsAndCash() (positions map[string]float64, cash float64) {
	if l == nil {
		return map[string]float64{}, 0
	}
	out := make(map[string]float64, len(l.lots))
	for sym, queue := range l.lots {
		total := 0.0
		for _, lot := range queue {
			total += lot.Quantity
		}
		if total > 1e-9 {
			out[sym] = total
		}
	}
	return out, l.cash
}

// Cash returns the bare cash balance. Convenience wrapper around
// PositionsAndCash for callers that don't need the position map.
func (l *bSideLotLedger) Cash() float64 {
	if l == nil {
		return 0
	}
	return l.cash
}

// InitialCash returns the seed value the ledger was built with.
// K-3 needs this to compute B's NAV index (NAV = total_assets /
// initialCash, anchored to the same baseline as A's NAV[0]).
func (l *bSideLotLedger) InitialCash() float64 {
	if l == nil {
		return 0
	}
	return l.initial
}

// RealizedPnL is the running total across all SELL fills. K-3
// can report this on the run-end summary, and the AB compare
// UI can show it as "B realized PnL: $X" alongside A's.
func (l *bSideLotLedger) RealizedPnL() float64 {
	if l == nil {
		return 0
	}
	return l.realizedPnL
}

// History returns the appended trade log (in apply order). Used
// by K-3 to write `ab_test_variant_trades` rows with the right
// realized_pnl on each one. Read-only — caller must not mutate.
func (l *bSideLotLedger) History() []bSideAppliedTrade {
	if l == nil {
		return nil
	}
	return l.trades
}

// HeldSymbols returns the symbols currently in the portfolio.
// Convenience for tests that want a deterministic order.
func (l *bSideLotLedger) HeldSymbols() []string {
	pos, _ := l.PositionsAndCash()
	syms := make([]string, 0, len(pos))
	for s := range pos {
		syms = append(syms, s)
	}
	sort.Strings(syms)
	return syms
}

// AvgCostBasis returns the volume-weighted cost basis of all
// open lots for the given symbol. Used as the mark-to-market
// fallback price when the priceTimeline has no observation for
// the symbol on the requested date — better than 0 (which would
// instantly zero out NAV) and better than the most-recent lot's
// price alone (which ignores earlier larger lots).
//
// Returns 0 when the symbol has no open lots.
func (l *bSideLotLedger) AvgCostBasis(symbol string) float64 {
	if l == nil {
		return 0
	}
	queue := l.lots[symbol]
	totalQty, totalCost := 0.0, 0.0
	for _, lot := range queue {
		totalQty += lot.Quantity
		totalCost += lot.Quantity * lot.CostBasis
	}
	if totalQty <= 1e-9 {
		return 0
	}
	return totalCost / totalQty
}

// applyBSideDecision converts an A trade + decider response into
// a B trade. Pure transformation; the only state it touches is
// the LLM decision JSON (already validated by parseBSideDecision).
//
// Returned (qty, price, side, ok) — ok=false means the trade is
// a no-op for B (skip, or zero-qty after scaling). Callers should
// neither write a trade row nor invoke ledger.Apply on a no-op.
func applyBSideDecision(side string, qty, price float64, dec abBSideDecision) (outQty float64, outPrice float64, outSide string, ok bool) {
	if dec.Skip {
		return 0, 0, "", false
	}
	scale := dec.QuantityScale
	if scale <= 0 {
		// Defensive — parser should have clipped to >= 0.05 but
		// we re-check so a malformed in-memory decision (e.g.,
		// constructed by a future caller without parseBSideDecision)
		// can't slip a 0 through.
		scale = 1
	}
	outQty = qty * scale
	if outQty <= 0 {
		return 0, 0, "", false
	}
	outPrice = price
	outSide = strings.ToUpper(strings.TrimSpace(side))
	if dec.SideOverride != "" {
		outSide = strings.ToUpper(strings.TrimSpace(dec.SideOverride))
	}
	if outSide != "BUY" && outSide != "SELL" {
		// Unknown side after override — drop the trade rather
		// than guess. The caller should log this if it ever
		// happens, but we don't want to crash the analyze run.
		return 0, 0, "", false
	}
	return outQty, outPrice, outSide, true
}

// ----------------------------------------------------------------------
// Card K-3 — price timeline + B NAV recomputation
// ----------------------------------------------------------------------

// priceTimeline records the most-recent observed price for each
// symbol up to a given date. Built from A's full trade list so
// that B's mark-to-market on each NAV date uses the same
// per-symbol price A used (same execution-quality assumption).
//
// Symbols that B holds but never appear in A's trade stream fall
// back to the lot's average cost basis at MTM time.
type priceTimeline struct {
	bySymbol map[string][]priceTimelinePoint
}

type priceTimelinePoint struct {
	Date  time.Time
	Price float64
}

func newPriceTimeline() *priceTimeline {
	return &priceTimeline{bySymbol: map[string][]priceTimelinePoint{}}
}

// Add records (symbol, date, price). Caller MUST add in
// non-decreasing date order per symbol — that's the natural
// order trades come in. We don't sort to avoid an N log N pass
// on an already-sorted input.
func (pt *priceTimeline) Add(symbol string, date time.Time, price float64) {
	if pt == nil || symbol == "" || price <= 0 {
		return
	}
	pt.bySymbol[symbol] = append(pt.bySymbol[symbol], priceTimelinePoint{Date: date, Price: price})
}

// PriceAt returns the most-recent price observation at-or-before
// the given date, plus a boolean indicating whether any
// observation exists. Binary search keeps NAV recompute O(log n)
// per symbol per date, which matters when an analyze window
// touches hundreds of symbols across 250 trading days.
//
// Comparison is done at day-granularity to match NAV semantics:
// a trade on 2026-05-29 09:30 UTC is considered observable for
// MTM on the 2026-05-29 00:00 UTC NAV bar (NAV is end-of-day).
// Without this, end-of-window same-day trades would silently
// fall through and B's MTM would use a stale price.
func (pt *priceTimeline) PriceAt(symbol string, date time.Time) (float64, bool) {
	if pt == nil {
		return 0, false
	}
	series := pt.bySymbol[symbol]
	if len(series) == 0 {
		return 0, false
	}
	dayEnd := time.Date(date.Year(), date.Month(), date.Day(), 23, 59, 59, int(time.Second-time.Nanosecond), time.UTC)
	// Find first index whose date is strictly after the end of
	// the target day. `idx-1` is then the last entry on-or-before
	// the day.
	idx := sort.Search(len(series), func(i int) bool {
		return series[i].Date.After(dayEnd)
	})
	if idx == 0 {
		return 0, false
	}
	return series[idx-1].Price, true
}

// bSideNAVRow is one row of the recomputed B NAV series. Mirrors
// the relevant columns of ab_test_variant_nav so the writer can
// straight-up `INSERT ... VALUES`.
type bSideNAVRow struct {
	TradingDate      time.Time
	NAV              float64 // index, anchored to baseline NAV at t0
	TotalAssets      float64
	Cash             float64
	DailyReturn      float64
	CumulativeReturn float64
	Drawdown         float64
}

// computeBSideNAVRows replays the ledger's trade history against
// each A NAV date and produces B's NAV series anchored to the
// same baseline NAV unit (so B[0] == A[0]) but diverging from
// there based on B's actual trades and lot ledger.
//
// Pure function — no I/O, no DB access. Designed to be unit
// testable independently of the wiring.
//
// Inputs:
//   - history     — ledger trade log, in apply order
//   - aNavs       — A's NAV series; we use TradingDate to anchor
//                   each B row, NAV[0] as the baseline unit, and
//                   TotalAssets[0] as B's starting capital
//   - priceTL     — price observations from A's trade stream
//   - initialCash — what the ledger was seeded with; should
//                   equal aNavs[0].TotalAssets so B starts on
//                   the same dollar baseline as A
//
// Returns nil when there's no NAV history (consistent with the
// existing writeABSyntheticNAVs behaviour: nothing to write).
func computeBSideNAVRows(
	history []bSideAppliedTrade,
	aNavs []repository.NavSnapshot,
	priceTL *priceTimeline,
	initialCash float64,
) []bSideNAVRow {
	if len(aNavs) == 0 {
		return nil
	}
	baseline := aNavs[0].NAV
	if baseline <= 0 {
		baseline = 1
	}
	if initialCash <= 0 {
		// Defensive — the only way this happens is when
		// navs[0].TotalAssets was zero (corrupt fund). Fall
		// back to baseline so cumulative_return == 0 on day 1
		// instead of NaN. Better than crashing the analyze run.
		initialCash = baseline
	}
	replay := newBSideLotLedger(initialCash)
	rows := make([]bSideNAVRow, 0, len(aNavs))
	peak := baseline
	prevNAV := baseline
	historyIdx := 0
	// truncToDay collapses a timestamp to the start of its UTC
	// day. Why: NAV bars are stored at trading_date 00:00 UTC,
	// but trade timestamps are during market hours
	// (e.g. 09:30 UTC). A naive `trade.Date.After(nav.Date)`
	// would say "this BUY happened AFTER today's NAV bar" and
	// defer the trade to tomorrow — but tomorrow's NAV bar may
	// not exist (e.g. the test window only spans 2 days), so
	// the trade silently disappears from the ledger replay.
	// Comparing at day granularity matches the economic intent:
	// NAV is end-of-day, so any trade with the same date should
	// already be reflected in that day's NAV.
	truncToDay := func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
	for i, aNav := range aNavs {
		navDay := truncToDay(aNav.TradingDate)
		// Advance replay ledger by every trade whose trade-day
		// is on or before this NAV day.
		for historyIdx < len(history) && !truncToDay(history[historyIdx].Date).After(navDay) {
			h := history[historyIdx]
			replay.Apply(h.Date, h.Symbol, h.Side, h.Quantity, h.Price)
			historyIdx++
		}
		positions, cash := replay.PositionsAndCash()
		marketValue := 0.0
		for sym, qty := range positions {
			price, ok := priceTL.PriceAt(sym, aNav.TradingDate)
			if !ok {
				price = replay.AvgCostBasis(sym)
			}
			marketValue += qty * price
		}
		totalAssets := cash + marketValue
		// NAV index anchored to baseline so day 1 equals A's
		// NAV[0]. After that B diverges based on its own trades.
		nav := baseline * (totalAssets / initialCash)
		if nav <= 0 {
			// Defensive: a fully blown-up B (cash deeply
			// negative + zero positions) would render as 0 or
			// negative NAV, which the UI can't draw. Floor to
			// baseline/100 so the chart at least stays on
			// screen and the "drawdown" tells the story.
			nav = baseline / 100
		}
		if nav > peak {
			peak = nav
		}
		drawdown := 0.0
		if peak > 0 {
			drawdown = nav/peak - 1
		}
		cumulative := nav/baseline - 1
		dailyReturn := 0.0
		if i > 0 && prevNAV > 0 {
			dailyReturn = nav/prevNAV - 1
		}
		rows = append(rows, bSideNAVRow{
			TradingDate:      aNav.TradingDate,
			NAV:              nav,
			TotalAssets:      totalAssets,
			Cash:             cash,
			DailyReturn:      dailyReturn,
			CumulativeReturn: cumulative,
			Drawdown:         drawdown,
		})
		prevNAV = nav
	}
	return rows
}
