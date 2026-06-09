package backtest

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/fundai/server/internal/decision"
	"github.com/fundai/server/internal/ohlc"
)

// shouldRebalance gate ---------------------------------------------------------

func TestShouldRebalanceDailyAlwaysTrue(t *testing.T) {
	req := Request{RebalanceFrequency: RebalanceDaily}
	day := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	prev := day.AddDate(0, 0, -1)
	if !shouldRebalance(req, day, prev, false) {
		t.Fatalf("daily must always trigger")
	}
}

func TestShouldRebalanceMonthlyFirstDayTriggers(t *testing.T) {
	req := Request{RebalanceFrequency: RebalanceMonthly}
	// 2026-03-31 → 2026-04-01: month changes, must rebalance.
	prev := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
	day := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	if !shouldRebalance(req, day, prev, false) {
		t.Fatalf("monthly must trigger on month rollover")
	}
	// 2026-04-01 → 2026-04-02: same month, must skip.
	prev = day
	day = prev.AddDate(0, 0, 1)
	if shouldRebalance(req, day, prev, false) {
		t.Fatalf("monthly must skip on same-month days")
	}
}

func TestShouldRebalanceWeeklyOnISOWeekRollover(t *testing.T) {
	req := Request{RebalanceFrequency: RebalanceWeekly}
	// ISO week 11 → 12 in 2026 happens on 2026-03-16 (Mon).
	prev := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC) // Sun
	day := time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC)  // Mon
	if !shouldRebalance(req, day, prev, false) {
		t.Fatalf("weekly must trigger on ISO week rollover")
	}
	// 2026-03-16 → 2026-03-17: same ISO week, must skip.
	prev = day
	day = prev.AddDate(0, 0, 1)
	if shouldRebalance(req, day, prev, false) {
		t.Fatalf("weekly must skip mid-week")
	}
}

func TestShouldRebalanceFirstDayAlwaysTrue(t *testing.T) {
	req := Request{RebalanceFrequency: RebalanceMonthly}
	// The runner passes isFirstDay=true on day 0; even monthly
	// must run that once so positions can be sized.
	day := time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC) // mid-month
	if !shouldRebalance(req, day, time.Time{}, true) {
		t.Fatalf("first-day rebalance must always trigger to size into positions")
	}
}

// Runner with monthly rebalance + benchmark -----------------------------------

// stubDecideAlwaysBuy emits one buy each time .Decide is called.
// Used to confirm the rebalance gate actually throttles invocations.
type stubDecideAlwaysBuy struct {
	calls int
}

func (s *stubDecideAlwaysBuy) Decide(_ context.Context, _ decision.DecisionInput) (*decision.DecisionOutput, error) {
	s.calls++
	return &decision.DecisionOutput{
		Actions: []decision.DecisionAction{
			{Symbol: "AAPL", Action: "buy", QtyPct: 0.05, Confidence: 0.5, Reasoning: "stub"},
		},
		Confidence: 0.5,
	}, nil
}

func TestRunnerMonthlyRebalanceLimitsDecisionCalls(t *testing.T) {
	// 60 daily bars ≈ 3 months → monthly cadence should issue
	// 3 decision calls (day 0, day at month rollover #1, day at
	// month rollover #2).
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	closes := make([]float64, 60)
	for i := range closes {
		closes[i] = 100 + float64(i)*0.1 // gentle upward drift
	}
	bars := buildSyntheticBars(start, closes)
	decide := &stubDecideAlwaysBuy{}
	runner := &Runner{
		OHLC:   &stubOHLC{bars: map[string][]ohlc.Bar{"AAPL": bars}},
		Decide: decide,
	}
	res, err := runner.Run(context.Background(), Request{
		FundID:             "f1",
		Symbols:            []string{"AAPL"},
		Start:              start,
		End:                start.AddDate(0, 0, 59),
		InitialCash:        10_000,
		RebalanceFrequency: RebalanceMonthly,
	}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil {
		t.Fatalf("nil result")
	}
	// Synthetic bars are exactly 1 trading day apart so we get
	// 60 trading days spanning Jan 2 → Mar 2 with rollovers on
	// Feb 1 and Mar 1 → 3 decision calls total (day 0 + 2
	// rollovers).
	if decide.calls < 2 || decide.calls > 4 {
		t.Errorf("decision calls = %d, want ~3 (one per month start)", decide.calls)
	}
	// NAV curve must still have one point per trading day even
	// on skipped-decision days — the runner is supposed to
	// mark-to-market every day.
	if len(res.NavCurve) != 60 {
		t.Errorf("NAV curve length = %d, want 60", len(res.NavCurve))
	}
}

// buildBenchmarkCurve ----------------------------------------------------------

func TestBuildBenchmarkCurveAlignsToTradingDays(t *testing.T) {
	days := []time.Time{
		time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 7, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 8, 0, 0, 0, 0, time.UTC),
	}
	bars := []ohlc.Bar{
		{Time: days[0], Close: 100},
		// day[1] missing → carry-forward should keep 100.
		{Time: days[2], Close: 105},
		{Time: days[3], Close: 110},
	}
	curve := buildBenchmarkCurve(bars, days, 10_000)
	if len(curve) != 4 {
		t.Fatalf("curve length = %d, want 4", len(curve))
	}
	if curve[0].Pct != 0 {
		t.Errorf("day-0 pct = %v, want 0", curve[0].Pct)
	}
	if math.Abs(curve[1].Pct-0.0) > 1e-9 {
		t.Errorf("day-1 carry-forward pct = %v, want 0 (carry from day-0)", curve[1].Pct)
	}
	if math.Abs(curve[2].Pct-0.05) > 1e-9 {
		t.Errorf("day-2 pct = %v, want 0.05", curve[2].Pct)
	}
	if math.Abs(curve[3].Pct-0.10) > 1e-9 {
		t.Errorf("day-3 pct = %v, want 0.10", curve[3].Pct)
	}
	// NAV should track openingNav × (close/anchor).
	if math.Abs(curve[3].Nav-11_000) > 1e-6 {
		t.Errorf("day-3 NAV = %v, want 11000", curve[3].Nav)
	}
}

func TestBuildBenchmarkCurveEmptyBarsReturnsNil(t *testing.T) {
	days := []time.Time{time.Now(), time.Now().Add(24 * time.Hour)}
	if curve := buildBenchmarkCurve(nil, days, 10_000); curve != nil {
		t.Errorf("want nil curve for no bars, got %v", curve)
	}
}

// applyBenchmarkMetrics -------------------------------------------------------

func TestApplyBenchmarkMetricsAlphaBetaSane(t *testing.T) {
	// Construct the series from daily returns directly so the
	// OLS regression has a clean, non-degenerate input:
	//   bench_daily[i] = ±0.01 alternating (mean 0, var > 0)
	//   strat_daily[i] = 1.5 × bench_daily[i] + 0.0005 / day
	//                    (so beta should regress out at 1.5
	//                    and annualised alpha ≈ 0.0005 × 252 = 0.126)
	n := 252
	stratNav := 10_000.0
	benchClose := 100.0
	stratCum := 1.0
	benchCum := 1.0
	navCurve := make([]NavPoint, n)
	benchCurve := make([]BenchmarkPoint, n)
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		// Anchor day 0 to a flat NAV — no return for the
		// regression to fit on yet.
		if i == 0 {
			navCurve[i] = NavPoint{Date: day, Nav: stratNav}
			benchCurve[i] = BenchmarkPoint{Date: day, Close: benchClose, Nav: 10_000, Pct: 0}
			continue
		}
		benchDaily := 0.01
		if i%2 == 1 {
			benchDaily = -0.01
		}
		stratDaily := 1.5*benchDaily + 0.0005

		stratCum *= 1 + stratDaily
		benchCum *= 1 + benchDaily
		stratNav = 10_000 * stratCum
		benchClose = 100 * benchCum

		navCurve[i] = NavPoint{
			Date: day.AddDate(0, 0, i),
			Nav:  stratNav,
		}
		benchCurve[i] = BenchmarkPoint{
			Date:  day.AddDate(0, 0, i),
			Close: benchClose,
			Nav:   10_000 * benchCum,
			Pct:   benchCum - 1.0,
		}
	}
	m := computeMetrics(navCurve, nil)
	applyBenchmarkMetrics(&m, navCurve, benchCurve)

	// Beta should regress out to exactly 1.5 (within rounding).
	if math.Abs(m.Beta-1.5) > 0.01 {
		t.Errorf("beta = %v, want ≈ 1.5", m.Beta)
	}
	// Annualised alpha ≈ 0.0005 × 252 = 0.126.
	if math.Abs(m.Alpha-0.126) > 0.01 {
		t.Errorf("alpha = %v, want ≈ 0.126", m.Alpha)
	}
	// Information ratio should be very high (alpha is
	// deterministic per day, so tracking error → 0). We just
	// check it's positive — the exact magnitude depends on
	// floating-point noise in the strat - β·bench subtraction.
	if m.InformationRatio <= 0 {
		t.Errorf("information ratio = %v, want positive", m.InformationRatio)
	}
}

func TestApplyBenchmarkMetricsSkipsOnLengthMismatch(t *testing.T) {
	nav := []NavPoint{{Nav: 1}, {Nav: 1.1}, {Nav: 1.2}}
	bench := []BenchmarkPoint{{Pct: 0}, {Pct: 0.05}}
	m := Metrics{CumulativeReturn: 0.2}
	applyBenchmarkMetrics(&m, nav, bench)
	if m.Alpha != 0 || m.Beta != 0 || m.ExcessReturn != 0 {
		t.Errorf("expected no-op on length mismatch, got %+v", m)
	}
}

func TestRunnerEndToEndBenchmarkPopulated(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	stratBars := buildSyntheticBars(start, []float64{100, 102, 104, 106, 108, 110})
	benchBars := buildSyntheticBars(start, []float64{200, 201, 202, 203, 204, 205})

	runner := &Runner{
		OHLC: &stubOHLC{bars: map[string][]ohlc.Bar{
			"AAPL": stratBars,
			"SPY":  benchBars,
		}},
		Decide: &stubDecideAlwaysBuy{},
	}
	res, err := runner.Run(context.Background(), Request{
		FundID:          "f1",
		Symbols:         []string{"AAPL"},
		Start:           start,
		End:             start.AddDate(0, 0, 5),
		InitialCash:     10_000,
		BenchmarkSymbol: "SPY",
	}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.BenchmarkSymbol != "SPY" {
		t.Errorf("benchmark symbol echo = %q", res.BenchmarkSymbol)
	}
	if len(res.BenchmarkCurve) != len(res.NavCurve) {
		t.Fatalf("benchmark curve length %d != nav %d", len(res.BenchmarkCurve), len(res.NavCurve))
	}
	// SPY +2.5% over the window → benchmark cumret = 0.025.
	if math.Abs(res.Metrics.BenchmarkCumulativeReturn-0.025) > 1e-6 {
		t.Errorf("benchmark cumret = %v, want 0.025", res.Metrics.BenchmarkCumulativeReturn)
	}
}
