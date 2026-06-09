package backtest

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/fundai/server/internal/decision"
	"github.com/fundai/server/internal/ohlc"
)

// Runner is the default Engine implementation. It owns the
// per-day replay loop and orchestrates OHLC + DecisionEngine +
// portfolio updates.
//
// Concurrency model: a Runner instance is safe to reuse across
// goroutines because every Run() call constructs its own
// portfolio + progress. The shared dependencies (ohlcFetcher,
// decisionEngine) MUST themselves be concurrency-safe — which
// they are by design (ohlc.Cache uses a RWMutex, the
// decision.LLMDecisionEngine is read-mostly).
type Runner struct {
	// OHLC is the historical bar source. Required.
	OHLC ohlc.Fetcher
	// Decide is the strategy under test. Required.
	Decide decision.DecisionEngine
}

// Run implements Engine. ctx cancellation is checked at the top of
// every day; the runner cooperates by returning ErrCancelled when
// triggered.
func (r *Runner) Run(ctx context.Context, req Request, progress *Progress) (*Result, error) {
	if r == nil || r.OHLC == nil || r.Decide == nil {
		return nil, fmt.Errorf("backtest: runner missing dependencies")
	}
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	req = applyRequestDefaults(req)
	symbols := canonicalSymbols(req.Symbols, req.InitialPositions)
	if len(symbols) == 0 {
		return nil, ErrEmptyUniverse
	}

	// Pre-fetch the full history per symbol in one go. This is
	// the big perf win versus per-day fetches — even though every
	// day's prompt only looks at the slice up to that date, we
	// only call the upstream once per symbol. For Yahoo / Akshare
	// that's a 90% latency reduction.
	history, err := r.fetchHistory(ctx, req, symbols)
	if err != nil {
		return nil, err
	}

	tradingDays := uniqueTradingDays(history, req.Start, req.End)
	if len(tradingDays) < 2 {
		return nil, ErrInvalidWindow
	}
	if progress != nil {
		progress.markTotal(len(tradingDays))
		progress.markStatus("running", nil)
	}

	port := newPortfolio(req.InitialCash, req.InitialPositions)
	openingNav := port.totalNav(historyCloseAt(history, tradingDays[0]), nil)
	port.peakNav = openingNav

	curve := make([]NavPoint, 0, len(tradingDays))
	var trades []TradeEvent
	lastPrices := map[string]float64{}

	// Pre-fetch the benchmark in one shot (same as universe
	// symbols). Failures degrade silently — the result just
	// won't carry the BenchmarkCurve and the UI hides the
	// "超额收益" line.
	benchmarkBars := r.fetchBenchmark(ctx, req)

	for i, day := range tradingDays {
		if err := ctx.Err(); err != nil {
			if progress != nil {
				progress.markStatus("cancelled", ErrCancelled)
			}
			return nil, ErrCancelled
		}
		closes := historyCloseAt(history, day)
		// Update lastPrices BEFORE the decision call so the
		// engine sees the most recent quote — the decision
		// engine uses today's close for sizing.
		for sym, px := range closes {
			if px > 0 {
				lastPrices[sym] = px
			}
		}
		// Stage 1: gate the decision step by RebalanceFrequency.
		// On non-rebalance days we still mark-to-market the
		// portfolio (snapshot below) but skip the engine call —
		// the NAV moves with the held positions' close prices,
		// the strategy "holds".
		var dayTrades []TradeEvent
		var prevDay time.Time
		if i > 0 {
			prevDay = tradingDays[i-1]
		}
		if shouldRebalance(req, day, prevDay, i == 0) {
			dayTrades = r.runDecisionStep(ctx, req, port, history, closes, lastPrices, day)
			trades = append(trades, dayTrades...)
		}
		snap := port.snapshot(day, closes, lastPrices)
		curve = append(curve, snap)
		if progress != nil {
			progress.markDayDone(day)
		}
		// Cooperative yield once per ~50 days so a stuck context
		// can preempt the loop (helps tests that cancel mid-run).
		if i%50 == 0 {
			runtimeYield(ctx)
		}
	}

	benchmarkCurve := buildBenchmarkCurve(benchmarkBars, tradingDays, openingNav)
	metrics := computeMetrics(curve, trades)
	if len(benchmarkCurve) == len(curve) && len(benchmarkCurve) >= 2 {
		applyBenchmarkMetrics(&metrics, curve, benchmarkCurve)
	}

	result := &Result{
		FundID:          req.FundID,
		Name:            req.Name,
		EngineKind:      req.EngineKind,
		Start:           tradingDays[0],
		End:             tradingDays[len(tradingDays)-1],
		InitialCash:     openingNav,
		FinalNav:        curve[len(curve)-1].Nav,
		NavCurve:        curve,
		Trades:          trades,
		Metrics:         metrics,
		CompletedAt:     time.Now().UTC(),
		BenchmarkSymbol: req.BenchmarkSymbol,
		BenchmarkCurve:  benchmarkCurve,
	}
	if progress != nil {
		progress.markStatus("completed", nil)
	}
	return result, nil
}

// fetchBenchmark pulls the benchmark symbol from the same
// ohlc.Fetcher as the strategy universe. Returns nil (not an
// error) when no benchmark was requested or the upstream couldn't
// satisfy the request — benchmark is decorative, never blocking.
func (r *Runner) fetchBenchmark(ctx context.Context, req Request) []ohlc.Bar {
	if r == nil || r.OHLC == nil {
		return nil
	}
	sym := strings.ToUpper(strings.TrimSpace(req.BenchmarkSymbol))
	if sym == "" {
		return nil
	}
	windowDays := int(req.End.Sub(req.Start).Hours()/24.0) + 1
	lookback := int(float64(windowDays)*1.1) + 20
	if lookback > 2000 {
		lookback = 2000
	}
	endPad := req.End.Add(24 * time.Hour)
	bars, err := r.OHLC.Fetch(ctx, ohlc.FetchRequest{
		Symbol:    sym,
		Market:    req.Market,
		Interval:  ohlc.IntervalDay,
		LookbackN: lookback,
		EndTime:   endPad,
	})
	if err != nil || len(bars) == 0 {
		return nil
	}
	return bars
}

// buildBenchmarkCurve aligns benchmark bars to the strategy's
// trading-day calendar so the SPA can plot them on the same X
// axis without alignment math. Missing bars carry-forward the
// previous close (last-observation-carried-forward) so the
// excess-return arithmetic doesn't divide by zero. Returns nil
// when no benchmark data was available.
func buildBenchmarkCurve(bars []ohlc.Bar, days []time.Time, openingNav float64) []BenchmarkPoint {
	if len(bars) == 0 || len(days) == 0 || openingNav <= 0 {
		return nil
	}
	dayKey := func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
	byDay := make(map[time.Time]float64, len(bars))
	for _, b := range bars {
		if b.Close > 0 {
			byDay[dayKey(b.Time)] = b.Close
		}
	}
	// Find day-0 close: walk forward from days[0] until we hit
	// a bar. If the benchmark only starts mid-window (e.g. you
	// asked for a 5y window but the index ETF is newer), use
	// the first available close as the anchor.
	var anchor float64
	for _, d := range days {
		if c, ok := byDay[d]; ok {
			anchor = c
			break
		}
	}
	if anchor <= 0 {
		return nil
	}
	out := make([]BenchmarkPoint, 0, len(days))
	lastClose := anchor
	for _, d := range days {
		if c, ok := byDay[d]; ok && c > 0 {
			lastClose = c
		}
		pct := (lastClose / anchor) - 1.0
		out = append(out, BenchmarkPoint{
			Date:  d,
			Close: lastClose,
			Nav:   openingNav * (lastClose / anchor),
			Pct:   pct,
		})
	}
	return out
}

// runDecisionStep runs the engine for one trading day and applies
// the decisions to the portfolio. Returns the per-day trade events
// (filled + skipped + demoted).
func (r *Runner) runDecisionStep(ctx context.Context, req Request, port *portfolio, history map[string][]ohlc.Bar, closes, lastPrices map[string]float64, day time.Time) []TradeEvent {
	totalAssets := port.totalNav(closes, lastPrices)
	if totalAssets <= 0 {
		return []TradeEvent{{Date: day, Status: "skipped", Reason: "non-positive NAV"}}
	}
	input := buildDecisionInput(req, port, totalAssets, day, history)
	output, err := r.Decide.Decide(ctx, input)
	if err != nil || output == nil {
		return []TradeEvent{{Date: day, Status: "skipped", Reason: errOr(err, "engine returned nil")}}
	}

	maxOrders := req.MaxOrdersPerDay
	if maxOrders <= 0 {
		maxOrders = 5
	}

	events := make([]TradeEvent, 0, len(output.Actions))
	for i, action := range output.Actions {
		if i >= maxOrders {
			events = append(events, TradeEvent{
				Date:       day,
				Symbol:     action.Symbol,
				Action:     strings.ToLower(action.Action),
				Status:     "capped",
				Reason:     "max-orders-per-day reached",
				Confidence: action.Confidence,
			})
			continue
		}
		events = append(events, r.executeAction(req, port, action, closes, day))
	}
	return events
}

// executeAction translates a single DecisionAction into one
// TradeEvent applied to the portfolio. Hold/watch never touch the
// portfolio; buy/add/reduce/sell go through portfolio.buy/sell.
func (r *Runner) executeAction(req Request, port *portfolio, action decision.DecisionAction, closes map[string]float64, day time.Time) TradeEvent {
	symbol := strings.ToUpper(strings.TrimSpace(action.Symbol))
	verb := strings.ToLower(strings.TrimSpace(action.Action))
	event := TradeEvent{
		Date:       day,
		Symbol:     symbol,
		Action:     verb,
		Reason:     action.Reasoning,
		Confidence: action.Confidence,
	}
	if symbol == "" {
		event.Status = "skipped"
		event.Reason = "empty symbol"
		return event
	}
	switch verb {
	case "hold", "watch", "":
		event.Status = "skipped"
		event.Reason = strings.TrimSpace(action.Reasoning + " (no-op)")
		return event
	case "buy", "add":
		return r.executeBuy(req, port, action, closes, event)
	case "sell", "reduce":
		return r.executeSell(req, port, action, closes, event)
	default:
		event.Status = "skipped"
		event.Reason = "unknown action: " + verb
		return event
	}
}

func (r *Runner) executeBuy(req Request, port *portfolio, action decision.DecisionAction, closes map[string]float64, event TradeEvent) TradeEvent {
	close := closes[event.Symbol]
	if close <= 0 {
		event.Status = "no_quote"
		event.Reason = "no close price"
		return event
	}
	// qtyPct is a fraction of TotalAssets to allocate to the buy.
	// Cap at 10% per the system prompt's hard constraint so a
	// rogue engine output can't blow the budget.
	pct := action.QtyPct
	if pct <= 0 {
		event.Status = "skipped"
		event.Reason = "buy with non-positive qtyPct"
		return event
	}
	if pct > 0.10 {
		pct = 0.10
	}
	totalAssets := port.totalNav(closes, nil)
	notional := totalAssets * pct
	if notional <= 0 {
		event.Status = "skipped"
		event.Reason = "buy notional non-positive"
		return event
	}
	fillPrice := close * (1 + req.SlippageBps/10000.0) * (1 + req.CommissionBps/10000.0)
	if fillPrice <= 0 {
		event.Status = "no_quote"
		return event
	}
	qty := math.Floor(notional / fillPrice)
	if qty <= 0 {
		event.Status = "skipped"
		event.Reason = "buy qty rounds to zero"
		return event
	}
	notional, err := port.buy(event.Symbol, qty, fillPrice)
	if err != nil {
		event.Status = mapPortfolioError(err)
		event.Reason = err.Error()
		return event
	}
	event.Status = "filled"
	event.Quantity = qty
	event.FillPrice = fillPrice
	event.Notional = notional
	return event
}

func (r *Runner) executeSell(req Request, port *portfolio, action decision.DecisionAction, closes map[string]float64, event TradeEvent) TradeEvent {
	close := closes[event.Symbol]
	if close <= 0 {
		event.Status = "no_quote"
		event.Reason = "no close price"
		return event
	}
	held := port.quantityOf(event.Symbol)
	if held <= 0 {
		event.Status = "no_qty"
		event.Reason = "no position to sell"
		return event
	}
	verb := strings.ToLower(strings.TrimSpace(action.Action))
	qty := held
	if verb == "reduce" {
		pct := action.QtyPct
		if pct <= 0 {
			event.Status = "skipped"
			event.Reason = "reduce with non-positive qtyPct"
			return event
		}
		if pct > 1 {
			pct = 1
		}
		qty = math.Floor(held * pct)
		if qty <= 0 {
			event.Status = "skipped"
			event.Reason = "reduce qty rounds to zero"
			return event
		}
	}
	fillPrice := close * (1 - req.SlippageBps/10000.0) * (1 - req.CommissionBps/10000.0)
	if fillPrice <= 0 {
		event.Status = "no_quote"
		return event
	}
	notional, pnl, err := port.sell(event.Symbol, qty, fillPrice)
	if err != nil {
		event.Status = mapPortfolioError(err)
		event.Reason = err.Error()
		return event
	}
	event.Status = "filled"
	event.Quantity = qty
	event.FillPrice = fillPrice
	event.Notional = notional
	// Stash realized P&L in Confidence so metrics.countTradeWins
	// can detect winning vs losing closes without re-walking
	// lots. Positive = profitable close, negative = loss.
	event.Confidence = pnl
	return event
}

// validateRequest enforces the documented invariants up front so
// the per-day loop doesn't need to defend against degenerate
// shapes.
func validateRequest(req Request) error {
	if strings.TrimSpace(req.FundID) == "" {
		return fmt.Errorf("backtest: FundID required")
	}
	if req.Start.IsZero() || req.End.IsZero() || !req.End.After(req.Start) {
		return ErrInvalidWindow
	}
	if req.InitialCash <= 0 && len(req.InitialPositions) == 0 {
		return fmt.Errorf("backtest: need InitialCash > 0 or non-empty InitialPositions")
	}
	return nil
}

// applyRequestDefaults fills omitted Request fields with sensible
// defaults so the runner never has to defend against ambiguous
// inputs downstream.
func applyRequestDefaults(req Request) Request {
	out := req
	if strings.TrimSpace(out.Name) == "" {
		out.Name = "backtest-" + req.Start.UTC().Format("2006-01-02") + "_" + req.End.UTC().Format("2006-01-02")
	}
	out.Market = strings.ToLower(strings.TrimSpace(out.Market))
	if out.Market == "" {
		out.Market = "us_equity"
	}
	if out.SlippageBps <= 0 {
		out.SlippageBps = 5
	}
	if out.CommissionBps <= 0 {
		out.CommissionBps = 5
	}
	if out.MaxOrdersPerDay <= 0 {
		out.MaxOrdersPerDay = 5
	}
	if strings.TrimSpace(out.EngineKind) == "" {
		out.EngineKind = "fallback"
	}
	if strings.TrimSpace(out.BaseCurrency) == "" {
		out.BaseCurrency = "USD"
	}
	// Rebalance default depends on market because the two
	// product lines have different cadences: US monthly (Stage 1
	// SaaS), A-share daily (existing intraday research). Caller
	// can always override.
	if strings.TrimSpace(out.RebalanceFrequency) == "" {
		out.RebalanceFrequency = RebalanceDaily
	} else {
		out.RebalanceFrequency = strings.ToLower(strings.TrimSpace(out.RebalanceFrequency))
	}
	// Stage 1 benchmark defaulting: pick a sensible index per
	// market when the operator forgets to set one. They can
	// still pass "" to opt out — empty string here means the
	// caller already chose to skip the benchmark leg.
	out.BenchmarkSymbol = strings.ToUpper(strings.TrimSpace(out.BenchmarkSymbol))
	return out
}

// shouldRebalance returns true when the runner should invoke the
// decision engine on the given day per Request.RebalanceFrequency.
// Always true on day 0 so the runner gets one chance to size into
// positions; otherwise we'd be flat for the whole first month.
func shouldRebalance(req Request, day, prevDay time.Time, isFirstDay bool) bool {
	if isFirstDay {
		return true
	}
	switch req.RebalanceFrequency {
	case RebalanceWeekly:
		_, prevWeek := prevDay.ISOWeek()
		_, curWeek := day.ISOWeek()
		return curWeek != prevWeek || prevDay.Year() != day.Year()
	case RebalanceMonthly:
		return prevDay.Month() != day.Month() || prevDay.Year() != day.Year()
	default:
		return true
	}
}

// canonicalSymbols merges Request.Symbols + InitialPositions into a
// deduped uppercase symbol list. Preserves the request order so
// the engine sees a deterministic universe.
func canonicalSymbols(symbols []string, initial []InitialPosition) []string {
	seen := make(map[string]struct{}, len(symbols)+len(initial))
	out := make([]string, 0, len(symbols)+len(initial))
	add := func(raw string) {
		s := strings.ToUpper(strings.TrimSpace(raw))
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, s := range symbols {
		add(s)
	}
	for _, p := range initial {
		add(p.Symbol)
	}
	return out
}

// fetchHistory pulls one block of bars per symbol covering the
// requested window. We pad EndTime + 1 day to make sure the last
// trading day's bar is included even when the upstream filters
// strictly < EndTime.
func (r *Runner) fetchHistory(ctx context.Context, req Request, symbols []string) (map[string][]ohlc.Bar, error) {
	history := make(map[string][]ohlc.Bar, len(symbols))
	// LookbackN heuristic: window in days × 1.1 + 20 for buffer
	// so the runner still sees indicators on day 1. Cap at 2000
	// to keep individual responses cheap.
	windowDays := int(req.End.Sub(req.Start).Hours()/24.0) + 1
	lookback := int(float64(windowDays)*1.1) + 20
	if lookback > 2000 {
		lookback = 2000
	}
	endPad := req.End.Add(24 * time.Hour)
	for _, sym := range symbols {
		bars, err := r.OHLC.Fetch(ctx, ohlc.FetchRequest{
			Symbol:    sym,
			Market:    req.Market,
			Interval:  ohlc.IntervalDay,
			LookbackN: lookback,
			EndTime:   endPad,
		})
		if err != nil {
			// Soft fail — one missing symbol shouldn't sink the
			// whole run.
			continue
		}
		history[sym] = bars
	}
	if len(history) == 0 {
		return nil, ErrEmptyUniverse
	}
	return history, nil
}

// uniqueTradingDays returns the sorted distinct dates (truncated
// to YYYY-MM-DD UTC) where AT LEAST ONE symbol has a bar within
// [start, end]. We dedupe across symbols because A-share and
// US-equity holidays sometimes differ within a multi-market
// universe.
func uniqueTradingDays(history map[string][]ohlc.Bar, start, end time.Time) []time.Time {
	dayKey := func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
	startKey := dayKey(start)
	endKey := dayKey(end)
	dedupe := make(map[time.Time]struct{}, 64)
	for _, bars := range history {
		for _, b := range bars {
			d := dayKey(b.Time)
			if d.Before(startKey) || d.After(endKey) {
				continue
			}
			dedupe[d] = struct{}{}
		}
	}
	days := make([]time.Time, 0, len(dedupe))
	for d := range dedupe {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	return days
}

// historyCloseAt returns symbol → close for the given day. Missing
// symbols are omitted; the caller falls back to lastPrices.
func historyCloseAt(history map[string][]ohlc.Bar, day time.Time) map[string]float64 {
	out := make(map[string]float64, len(history))
	target := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	for sym, bars := range history {
		for _, b := range bars {
			d := time.Date(b.Time.Year(), b.Time.Month(), b.Time.Day(), 0, 0, 0, 0, time.UTC)
			if d.Equal(target) {
				out[sym] = b.Close
				break
			}
		}
	}
	return out
}

// buildDecisionInput projects portfolio + universe into the shape
// the decision engine expects. We don't populate RoundtableConsensus
// / BullCase / BearCase here because the backtest engine intentionally
// doesn't run the debate (cost). LLM-driven backtest variants can
// be added later by composing a different DecisionEngine that does
// run the debate per day.
func buildDecisionInput(req Request, port *portfolio, totalAssets float64, day time.Time, history map[string][]ohlc.Bar) decision.DecisionInput {
	positions := make([]decision.DecisionPosition, 0, len(port.positions))
	for _, sym := range port.sortedSymbols() {
		st := port.positions[sym]
		closes := closeUpTo(history[sym], day)
		var lastClose float64
		if len(closes) > 0 {
			lastClose = closes[len(closes)-1]
		}
		positions = append(positions, decision.DecisionPosition{
			Symbol:       sym,
			Market:       req.Market,
			Quantity:     st.quantity,
			AvailableQty: st.quantity, // backtest assumes T+0; T+1 modelling is a future PR
			CurrentPrice: lastClose,
			CostPrice:    weightedAvgCost(st),
		})
	}
	universe := canonicalSymbols(req.Symbols, req.InitialPositions)
	return decision.DecisionInput{
		FundID:        req.FundID,
		TradingDate:   day,
		Market:        req.Market,
		BaseCurrency:  req.BaseCurrency,
		TotalAssets:   totalAssets,
		AvailableCash: port.cash,
		Positions:     positions,
		Universe:      universe,
		Now:           day,
		BuyBudget:     port.cash, // simplest budget = current cash
		RiskNotes:     []string{"backtest mode: T+0 assumed; debate + sentiment disabled"},
	}
}

// closeUpTo returns the close prices for bars on or before `day`,
// in chronological order. Useful for indicator computation inside
// the decision engine (we don't currently compute indicators in
// the backtest path but exposing the slice keeps the option open).
func closeUpTo(bars []ohlc.Bar, day time.Time) []float64 {
	if len(bars) == 0 {
		return nil
	}
	target := time.Date(day.Year(), day.Month(), day.Day(), 23, 59, 59, 0, time.UTC)
	out := make([]float64, 0, len(bars))
	for _, b := range bars {
		if b.Time.After(target) {
			break
		}
		out = append(out, b.Close)
	}
	return out
}

func mapPortfolioError(err error) string {
	switch err {
	case errInsufficientCash:
		return "no_cash"
	case errNoPosition:
		return "no_qty"
	case errBadOrder:
		return "skipped"
	}
	return "skipped"
}

func errOr(err error, fallback string) string {
	if err == nil {
		return fallback
	}
	return err.Error()
}

// runtimeYield is a cheap cooperative cancellation point. We don't
// want to import runtime.Gosched everywhere — a select with the
// context's Done channel is the idiomatic Go way to yield.
func runtimeYield(ctx context.Context) {
	select {
	case <-ctx.Done():
	default:
	}
}
