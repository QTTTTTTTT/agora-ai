package backtest

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fundai/server/internal/decision"
	"github.com/fundai/server/internal/ohlc"
)

// -------------------- portfolio --------------------

func TestPortfolioBuyDeductsCashAndStoresLot(t *testing.T) {
	p := newPortfolio(10_000, nil)
	notional, err := p.buy("AAPL", 50, 100)
	if err != nil {
		t.Fatalf("buy: %v", err)
	}
	if notional != 5000 {
		t.Errorf("notional = %v, want 5000", notional)
	}
	if p.cash != 5000 {
		t.Errorf("cash = %v, want 5000", p.cash)
	}
	if p.quantityOf("aapl") != 50 {
		t.Errorf("qty case-insensitive lookup failed: %v", p.quantityOf("aapl"))
	}
}

func TestPortfolioBuyRejectsInsufficientCash(t *testing.T) {
	p := newPortfolio(1000, nil)
	if _, err := p.buy("AAPL", 50, 100); !errors.Is(err, errInsufficientCash) {
		t.Errorf("expected errInsufficientCash, got %v", err)
	}
}

func TestPortfolioSellFIFOLotsAndRealizesPnL(t *testing.T) {
	p := newPortfolio(0, []InitialPosition{
		{Symbol: "AAPL", Quantity: 50, CostPrice: 80},
		{Symbol: "AAPL", Quantity: 50, CostPrice: 100}, // second lot, higher cost
	})
	notional, pnl, err := p.sell("AAPL", 60, 120)
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	if notional != 60*120 {
		t.Errorf("notional = %v", notional)
	}
	// First 50 lot @ cost 80 fully consumed → P&L 50*(120-80) = 2000
	// Plus 10 of second lot @ cost 100 → P&L 10*(120-100) = 200
	want := 50*(120.0-80.0) + 10*(120.0-100.0)
	if math.Abs(pnl-want) > 1e-6 {
		t.Errorf("pnl = %v, want %v", pnl, want)
	}
	if p.quantityOf("AAPL") != 40 {
		t.Errorf("residual qty = %v, want 40", p.quantityOf("AAPL"))
	}
}

func TestPortfolioSellMissingPositionErrors(t *testing.T) {
	p := newPortfolio(10_000, nil)
	if _, _, err := p.sell("MSFT", 10, 200); !errors.Is(err, errNoPosition) {
		t.Errorf("expected errNoPosition, got %v", err)
	}
}

func TestPortfolioSnapshotComputesNavAndDrawdown(t *testing.T) {
	p := newPortfolio(10_000, []InitialPosition{{Symbol: "AAPL", Quantity: 100, CostPrice: 100}})
	day1 := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	day2 := day1.AddDate(0, 0, 1)
	day3 := day1.AddDate(0, 0, 2)

	// Peak NAV initialised to opening NAV of 20k (10k cash + 100*100 stock).
	p.peakNav = 20_000
	s1 := p.snapshot(day1, map[string]float64{"AAPL": 100}, nil)
	if s1.Nav != 20_000 || s1.DrawdownPct != 0 {
		t.Errorf("day1 NAV/DD wrong: %+v", s1)
	}
	// Day 2: AAPL ↑ to 110 → NAV 21k (new peak), DD 0.
	s2 := p.snapshot(day2, map[string]float64{"AAPL": 110}, nil)
	if s2.Nav != 21_000 {
		t.Errorf("day2 NAV = %v, want 21000", s2.Nav)
	}
	if s2.DrawdownPct != 0 {
		t.Errorf("day2 should reset DD to 0 on new peak, got %v", s2.DrawdownPct)
	}
	// Day 3: AAPL ↓ to 90 → NAV 19k vs peak 21k → DD ≈ -0.0952.
	s3 := p.snapshot(day3, map[string]float64{"AAPL": 90}, nil)
	if s3.Nav != 19_000 {
		t.Errorf("day3 NAV = %v", s3.Nav)
	}
	expectedDD := 19_000.0/21_000.0 - 1
	if math.Abs(s3.DrawdownPct-expectedDD) > 1e-6 {
		t.Errorf("day3 DD = %v, want %v", s3.DrawdownPct, expectedDD)
	}
}

func TestPortfolioSnapshotFallsBackToCostWhenPriceMissing(t *testing.T) {
	p := newPortfolio(0, []InitialPosition{{Symbol: "AAPL", Quantity: 10, CostPrice: 95}})
	p.peakNav = 950
	s := p.snapshot(time.Now().UTC(), nil, nil)
	if s.Nav != 950 {
		t.Errorf("expected mark-to-cost fallback, got %v", s.Nav)
	}
}

// -------------------- metrics --------------------

func TestComputeMetricsHappyCurve(t *testing.T) {
	day := func(n int) time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n)
	}
	curve := []NavPoint{
		{Date: day(0), Nav: 1000, DrawdownPct: 0},
		{Date: day(1), Nav: 1100, DrawdownPct: 0},
		{Date: day(2), Nav: 1050, DrawdownPct: -0.0454545},
		{Date: day(3), Nav: 1210, DrawdownPct: 0},
	}
	trades := []TradeEvent{
		{Action: "buy", Status: "filled"},
		{Action: "sell", Status: "filled", Confidence: 250},  // winning close
		{Action: "sell", Status: "filled", Confidence: -100}, // losing close
		{Action: "watch", Status: "skipped"},
	}
	m := computeMetrics(curve, trades)
	wantCum := 0.21
	if math.Abs(m.CumulativeReturn-wantCum) > 1e-9 {
		t.Errorf("CumulativeReturn = %v, want %v", m.CumulativeReturn, wantCum)
	}
	if m.MaxDrawdown >= 0 {
		t.Errorf("MaxDrawdown should be negative on the dip day, got %v", m.MaxDrawdown)
	}
	if m.WinRate <= 0 || m.WinRate >= 1 {
		t.Errorf("WinRate should be in (0,1), got %v", m.WinRate)
	}
	if m.TradeCount != 3 {
		t.Errorf("expected 3 filled trades, got %d", m.TradeCount)
	}
	if m.WinningTradeCount != 1 || m.LosingTradeCount != 1 {
		t.Errorf("expected 1 win / 1 loss, got %d / %d", m.WinningTradeCount, m.LosingTradeCount)
	}
	if m.Volatility <= 0 || m.SharpeRatio == 0 {
		t.Errorf("Volatility / Sharpe should be populated: %+v", m)
	}
}

func TestComputeMetricsEmptyCurve(t *testing.T) {
	m := computeMetrics(nil, nil)
	if m.CumulativeReturn != 0 || m.SharpeRatio != 0 {
		t.Errorf("empty curve should yield zero metrics: %+v", m)
	}
}

func TestComputeMetricsZeroOpenNavSafe(t *testing.T) {
	curve := []NavPoint{
		{Nav: 0}, {Nav: 100},
	}
	m := computeMetrics(curve, nil)
	if m.CumulativeReturn != 0 {
		t.Errorf("zero-open should not divide-by-zero, got %v", m.CumulativeReturn)
	}
}

// -------------------- runner integration --------------------

// stubOHLC returns canned bars by symbol; the runner pre-fetches
// once per symbol so a single canned slice is enough.
type stubOHLC struct {
	bars map[string][]ohlc.Bar
}

func (s *stubOHLC) Fetch(_ context.Context, req ohlc.FetchRequest) ([]ohlc.Bar, error) {
	bars, ok := s.bars[req.Symbol]
	if !ok {
		return nil, ohlc.ErrNoData
	}
	out := make([]ohlc.Bar, len(bars))
	copy(out, bars)
	return out, nil
}

// stubDecide hands back a canned series of actions, cycling
// through them as the runner advances.
type stubDecide struct {
	scripts [][]decision.DecisionAction
	calls   atomic.Int32
}

func (s *stubDecide) Decide(_ context.Context, _ decision.DecisionInput) (*decision.DecisionOutput, error) {
	idx := int(s.calls.Add(1) - 1)
	if idx >= len(s.scripts) {
		idx = len(s.scripts) - 1
	}
	if idx < 0 {
		return &decision.DecisionOutput{Stance: "watch"}, nil
	}
	return &decision.DecisionOutput{Actions: s.scripts[idx], Confidence: 0.5}, nil
}

func buildSyntheticBars(start time.Time, closes []float64) []ohlc.Bar {
	bars := make([]ohlc.Bar, len(closes))
	for i, c := range closes {
		bars[i] = ohlc.Bar{
			Time:  start.AddDate(0, 0, i),
			Open:  c, High: c, Low: c, Close: c, Volume: 1000,
		}
	}
	return bars
}

// Happy-path: 5 days, a buy on day-0 followed by a sell on day-3,
// NAV grows because the price rises.
func TestRunnerHappyPathBuyThenSell(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	bars := buildSyntheticBars(start, []float64{100, 105, 110, 115, 120})
	ohlcStub := &stubOHLC{bars: map[string][]ohlc.Bar{"AAPL": bars}}
	scripts := [][]decision.DecisionAction{
		{{Symbol: "AAPL", Action: "buy", QtyPct: 0.05, Confidence: 0.7}}, // day 0
		nil, nil,                                                          // hold days 1,2
		{{Symbol: "AAPL", Action: "sell", QtyPct: 1, Confidence: 0.8}},   // day 3
		nil,                                                                // day 4
	}
	runner := &Runner{OHLC: ohlcStub, Decide: &stubDecide{scripts: scripts}}
	prog := &Progress{}
	req := Request{
		FundID:      "fund-1",
		Market:      "us_equity",
		Symbols:     []string{"AAPL"},
		Start:       start,
		End:         start.AddDate(0, 0, 4),
		InitialCash: 100_000,
	}
	res, err := runner.Run(context.Background(), req, prog)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.NavCurve) != 5 {
		t.Errorf("expected 5 NAV points, got %d", len(res.NavCurve))
	}
	if res.FinalNav <= res.InitialCash {
		t.Errorf("FinalNav %v should exceed InitialCash %v on rising tape", res.FinalNav, res.InitialCash)
	}
	if res.Metrics.TradeCount < 2 {
		t.Errorf("expected at least 2 filled trades, got %d", res.Metrics.TradeCount)
	}
	if prog.Snapshot().Status != "completed" {
		t.Errorf("progress should be completed, got %q", prog.Snapshot().Status)
	}
}

// Empty universe → ErrEmptyUniverse.
func TestRunnerRejectsEmptyUniverse(t *testing.T) {
	runner := &Runner{
		OHLC:   &stubOHLC{bars: map[string][]ohlc.Bar{}},
		Decide: &stubDecide{},
	}
	req := Request{
		FundID:      "fund-1",
		Start:       time.Now().Add(-7 * 24 * time.Hour),
		End:         time.Now(),
		InitialCash: 1000,
	}
	if _, err := runner.Run(context.Background(), req, nil); !errors.Is(err, ErrEmptyUniverse) {
		t.Errorf("expected ErrEmptyUniverse, got %v", err)
	}
}

// Bad time window → ErrInvalidWindow.
func TestRunnerRejectsInvertedWindow(t *testing.T) {
	runner := &Runner{
		OHLC:   &stubOHLC{bars: map[string][]ohlc.Bar{"AAPL": buildSyntheticBars(time.Now(), []float64{1, 2})}},
		Decide: &stubDecide{},
	}
	now := time.Now()
	req := Request{
		FundID:      "fund-1",
		Symbols:     []string{"AAPL"},
		Start:       now,
		End:         now.AddDate(0, 0, -3), // inverted
		InitialCash: 1000,
	}
	if _, err := runner.Run(context.Background(), req, nil); !errors.Is(err, ErrInvalidWindow) {
		t.Errorf("expected ErrInvalidWindow, got %v", err)
	}
}

// Cancellation: context cancelled before run → ErrCancelled.
func TestRunnerHonoursContextCancel(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	bars := buildSyntheticBars(start, make([]float64, 252))
	for i := range bars {
		bars[i].Close = 100
		bars[i].High = 100
		bars[i].Low = 100
		bars[i].Open = 100
	}
	runner := &Runner{
		OHLC:   &stubOHLC{bars: map[string][]ohlc.Bar{"AAPL": bars}},
		Decide: &stubDecide{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	req := Request{
		FundID:      "fund-1",
		Symbols:     []string{"AAPL"},
		Start:       start,
		End:         start.AddDate(0, 0, 250),
		InitialCash: 1000,
	}
	prog := &Progress{}
	if _, err := runner.Run(ctx, req, prog); !errors.Is(err, ErrCancelled) {
		t.Errorf("expected ErrCancelled, got %v", err)
	}
	if prog.Snapshot().Status != "cancelled" {
		t.Errorf("progress status should be cancelled, got %q", prog.Snapshot().Status)
	}
}

// Buy capped at 10% of NAV per the system prompt's hard rule.
func TestRunnerCapsBuyAtTenPercentOfNav(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	bars := buildSyntheticBars(start, []float64{100, 100, 100})
	ohlcStub := &stubOHLC{bars: map[string][]ohlc.Bar{"AAPL": bars}}
	// Engine asks for 50% allocation — runner should clamp to 10%.
	decide := &stubDecide{scripts: [][]decision.DecisionAction{
		{{Symbol: "AAPL", Action: "buy", QtyPct: 0.5, Confidence: 0.5}},
	}}
	runner := &Runner{OHLC: ohlcStub, Decide: decide}
	req := Request{
		FundID:      "fund-1",
		Symbols:     []string{"AAPL"},
		Start:       start,
		End:         start.AddDate(0, 0, 2),
		InitialCash: 100_000,
	}
	res, err := runner.Run(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Cap = 10k notional / ~100 per share with slippage → ≤ 99 shares.
	var bought float64
	for _, tr := range res.Trades {
		if tr.Action == "buy" && tr.Status == "filled" {
			bought = tr.Quantity
		}
	}
	if bought <= 0 || bought > 100 {
		t.Errorf("buy should be capped at ~10%% of NAV, got %v shares", bought)
	}
}

// Engine returns nil → runner records "skipped" without crashing.
func TestRunnerToleratesNilEngineOutput(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	bars := buildSyntheticBars(start, []float64{100, 101, 102})
	runner := &Runner{
		OHLC:   &stubOHLC{bars: map[string][]ohlc.Bar{"AAPL": bars}},
		Decide: errorDecide{err: errors.New("engine on fire")},
	}
	req := Request{
		FundID:      "fund-1",
		Symbols:     []string{"AAPL"},
		Start:       start,
		End:         start.AddDate(0, 0, 2),
		InitialCash: 1000,
	}
	res, err := runner.Run(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.NavCurve) != 3 {
		t.Errorf("NAV curve should still cover all days: %d", len(res.NavCurve))
	}
	// Every day records a skipped event because the engine errors.
	skipped := 0
	for _, tr := range res.Trades {
		if tr.Status == "skipped" {
			skipped++
		}
	}
	if skipped == 0 {
		t.Errorf("expected per-day skipped trades, got %d", skipped)
	}
}

type errorDecide struct {
	err error
}

func (e errorDecide) Decide(_ context.Context, _ decision.DecisionInput) (*decision.DecisionOutput, error) {
	return nil, e.err
}

// Buy then sell uses FIFO + slippage; verify cash + qty residue.
func TestRunnerSlippageAppliesBothDirections(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	bars := buildSyntheticBars(start, []float64{100, 100, 100})
	scripts := [][]decision.DecisionAction{
		{{Symbol: "AAPL", Action: "buy", QtyPct: 0.05}},
		nil,
		{{Symbol: "AAPL", Action: "sell", QtyPct: 1}},
	}
	runner := &Runner{
		OHLC:   &stubOHLC{bars: map[string][]ohlc.Bar{"AAPL": bars}},
		Decide: &stubDecide{scripts: scripts},
	}
	req := Request{
		FundID:        "fund-1",
		Symbols:       []string{"AAPL"},
		Start:         start,
		End:           start.AddDate(0, 0, 2),
		InitialCash:   100_000,
		SlippageBps:   50,
		CommissionBps: 50,
	}
	res, err := runner.Run(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Round-tripping at flat price with 50bps each side ⇒ NAV loss.
	if res.FinalNav >= res.InitialCash {
		t.Errorf("flat tape + slippage should erode NAV; got final=%v initial=%v", res.FinalNav, res.InitialCash)
	}
}

// Max orders per day cap demotes surplus actions to "capped".
func TestRunnerMaxOrdersPerDayCap(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	bars := buildSyntheticBars(start, []float64{100, 100, 100})
	actions := []decision.DecisionAction{
		{Symbol: "AAPL", Action: "buy", QtyPct: 0.01},
		{Symbol: "AAPL", Action: "buy", QtyPct: 0.01},
		{Symbol: "AAPL", Action: "buy", QtyPct: 0.01},
	}
	runner := &Runner{
		OHLC:   &stubOHLC{bars: map[string][]ohlc.Bar{"AAPL": bars}},
		Decide: &stubDecide{scripts: [][]decision.DecisionAction{actions, nil, nil}},
	}
	req := Request{
		FundID:          "fund-1",
		Symbols:         []string{"AAPL"},
		Start:           start,
		End:             start.AddDate(0, 0, 2),
		InitialCash:     100_000,
		MaxOrdersPerDay: 2,
	}
	res, _ := runner.Run(context.Background(), req, nil)
	var filled, capped int
	for _, tr := range res.Trades {
		if tr.Date.Equal(start) {
			if tr.Status == "filled" {
				filled++
			}
			if tr.Status == "capped" {
				capped++
			}
		}
	}
	if filled != 2 || capped != 1 {
		t.Errorf("expected 2 filled + 1 capped on day 0, got filled=%d capped=%d", filled, capped)
	}
}

// -------------------- jobs --------------------

func TestJobStoreSubmitRunsAndCompletes(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	bars := buildSyntheticBars(start, []float64{100, 101, 102})
	engine := &Runner{
		OHLC:   &stubOHLC{bars: map[string][]ohlc.Bar{"AAPL": bars}},
		Decide: &stubDecide{},
	}
	store := NewJobStore(engine)
	req := Request{FundID: "fund-1", Symbols: []string{"AAPL"}, Start: start, End: start.AddDate(0, 0, 2), InitialCash: 1000}
	job, err := store.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if waitForStatus(job.Progress, "completed", 2*time.Second) != "completed" {
		t.Fatalf("job did not complete: status=%s", job.Progress.Snapshot().Status)
	}
	// W14-4 — read Result via Snapshot. Direct field access
	// races with the runner goroutine even after status flips
	// to "completed" (Progress.mu and Job.mu are independent
	// happens-before edges).
	snap := job.Snapshot()
	if snap.Result == nil || len(snap.Result.NavCurve) != 3 {
		t.Errorf("result wrong: %+v", snap.Result)
	}
}

func TestJobStoreCancel(t *testing.T) {
	// 250-day flat tape so the job is alive long enough to cancel.
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	bars := make([]ohlc.Bar, 250)
	for i := range bars {
		bars[i] = ohlc.Bar{Time: start.AddDate(0, 0, i), Close: 100}
	}
	engine := &Runner{
		OHLC: &stubOHLC{bars: map[string][]ohlc.Bar{"AAPL": bars}},
		Decide: blockingDecide{},
	}
	store := NewJobStore(engine)
	req := Request{FundID: "fund-1", Symbols: []string{"AAPL"}, Start: start, End: start.AddDate(0, 0, 249), InitialCash: 1000}
	job, err := store.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// Give the runner a moment to start.
	time.Sleep(20 * time.Millisecond)
	if !store.Cancel(job.ID) {
		t.Fatal("Cancel returned false")
	}
	status := waitForStatusAnyOf(job.Progress, []string{"cancelled", "failed", "completed"}, time.Second)
	if status != "cancelled" {
		t.Errorf("expected status=cancelled, got %q", status)
	}
}

type blockingDecide struct{}

func (b blockingDecide) Decide(ctx context.Context, _ decision.DecisionInput) (*decision.DecisionOutput, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(50 * time.Millisecond):
		return &decision.DecisionOutput{}, nil
	}
}

func TestJobStoreListFiltersAndSortsNewestFirst(t *testing.T) {
	store := NewJobStore(&Runner{
		OHLC:   &stubOHLC{bars: map[string][]ohlc.Bar{"AAPL": buildSyntheticBars(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), []float64{1, 2})}},
		Decide: &stubDecide{},
	})
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	mkReq := func(fund string) Request {
		return Request{FundID: fund, Symbols: []string{"AAPL"}, Start: start, End: start.AddDate(0, 0, 1), InitialCash: 1000}
	}
	a, _ := store.Submit(context.Background(), mkReq("fund-a"))
	time.Sleep(5 * time.Millisecond)
	b, _ := store.Submit(context.Background(), mkReq("fund-b"))
	time.Sleep(5 * time.Millisecond)
	c, _ := store.Submit(context.Background(), mkReq("fund-a"))

	all := store.List("")
	if len(all) != 3 {
		t.Errorf("expected 3 jobs, got %d", len(all))
	}
	if all[0].ID != c.ID {
		t.Errorf("newest job should be first, got %s expected %s", all[0].ID, c.ID)
	}
	filtered := store.List("fund-a")
	if len(filtered) != 2 {
		t.Errorf("expected 2 fund-a jobs, got %d", len(filtered))
	}
	for _, j := range filtered {
		if j.ID == b.ID {
			t.Errorf("fund-a filter leaked fund-b job %s", j.ID)
		}
	}
	_ = a // referenced for completeness
}

// TestJobSnapshotIsRaceFree — W14-4 regression test.
//
// Before the fix, jobToView (and any other cross-goroutine
// reader) accessed job.StartedAt / Result / Err directly while
// the runner goroutine was still writing them. The race
// detector flagged this in cmd/server tests via
// TestAdapterSubmitSweepFansOutCartesian. This test pins the
// fix in the package that owns the mutex: a concurrent reader
// pounding Snapshot() while a Submit-triggered runner is in
// flight must complete cleanly under -race.
//
// We deliberately race the two: kick off Submit, immediately
// spin up a reader goroutine, let both run for a beat, then
// wait for completion and assert Snapshot reflects the final
// state. If anyone removes Job.mu, this test fails with a
// race detector warning rather than a silently-wrong assertion.
func TestJobSnapshotIsRaceFree(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	bars := buildSyntheticBars(start, []float64{100, 101, 102})
	engine := &Runner{
		OHLC:   &stubOHLC{bars: map[string][]ohlc.Bar{"AAPL": bars}},
		Decide: &stubDecide{},
	}
	store := NewJobStore(engine)
	req := Request{FundID: "fund-1", Symbols: []string{"AAPL"}, Start: start, End: start.AddDate(0, 0, 2), InitialCash: 1000}
	job, err := store.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Start a reader goroutine that hammers Snapshot. If we
	// ever stop synchronising the mutable fields, the race
	// detector trips here.
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			snap := job.Snapshot()
			_ = snap.StartedAt
			_ = snap.CompletedAt
			_ = snap.Result
			_ = snap.Err
		}
	}()

	if waitForStatus(job.Progress, "completed", 2*time.Second) != "completed" {
		t.Fatalf("job did not complete")
	}
	<-done

	final := job.Snapshot()
	if final.Result == nil {
		t.Fatalf("expected non-nil Result on completed job, got %+v", final)
	}
	if final.StartedAt.IsZero() {
		t.Errorf("expected StartedAt to be set after completion")
	}
	if final.CompletedAt.IsZero() {
		t.Errorf("expected CompletedAt to be set after completion")
	}
	if final.Err != nil {
		t.Errorf("expected nil Err on success path, got %v", final.Err)
	}
}

// TestJobSnapshotNilSafe pins the defensive nil-receiver guard
// — callers in cmd/server occasionally end up with a nil *Job
// from store.Get on an unknown ID; Snapshot must return a zero
// value rather than panic.
func TestJobSnapshotNilSafe(t *testing.T) {
	var j *Job
	snap := j.Snapshot()
	if snap.ID != "" {
		t.Errorf("expected zero JobSnapshot for nil receiver, got ID=%q", snap.ID)
	}
}

func TestJobStoreEvictionDoesNotDropActive(t *testing.T) {
	store := NewJobStore(&Runner{
		OHLC:   &stubOHLC{bars: map[string][]ohlc.Bar{"AAPL": buildSyntheticBars(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), []float64{1, 2})}},
		Decide: &stubDecide{},
	})
	store.maxJobs = 2
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	req := Request{FundID: "fund-1", Symbols: []string{"AAPL"}, Start: start, End: start.AddDate(0, 0, 1), InitialCash: 1000}
	for i := 0; i < 5; i++ {
		_, _ = store.Submit(context.Background(), req)
		time.Sleep(20 * time.Millisecond)
	}
	if got := len(store.List("")); got > 2 {
		t.Errorf("expected ≤ 2 jobs after eviction, got %d", got)
	}
}

// -------------------- helpers --------------------

func waitForStatus(prog *Progress, want string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		snap := prog.Snapshot()
		if snap.Status == want {
			return snap.Status
		}
		time.Sleep(5 * time.Millisecond)
	}
	return prog.Snapshot().Status
}

func waitForStatusAnyOf(prog *Progress, wants []string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		snap := prog.Snapshot()
		for _, w := range wants {
			if snap.Status == w {
				return snap.Status
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return prog.Snapshot().Status
}
