// advisor_reputation_loop.go — Phase 5 backfill driver for the
// /advisor surface.
//
// Mirrors agentReputationLoop but iterates advisor_consultations
// instead of analyst panels / debate transcripts. Each
// MasterReport becomes one agent_reputation_outcomes row keyed by
// agent_id = "master:<key>"; each TacticReport becomes one row
// keyed by agent_id = "tactic:<key>". Rows carry fund_id IS NULL
// (migration 099) so they share zero scope with the fund-side
// agents.
//
// Concurrency model matches the fund loop: a single goroutine per
// binary, paced by Interval ± jitter, and the existing leader gate
// in main.go gates wave start. UPSERT semantics in the repo make
// re-runs idempotent.
//
// Output mapping
//
//   Master verdict        Direction (ledger)   Hit rule
//   ------------------    ------------------   --------------------
//   STRONG_BUY / BUY      buy                  realised_return > 0
//   AVOID / SHORT         avoid                realised_return < 0
//   HOLD / PASS / SKIP    skip                 never a hit / miss
//
//   Tactic verdict
//   ------------------
//   BUY_TAIL / BUY_DIP / CHASE_LIMIT_UP / BUY_PULLBACK   -> buy
//   WAIT_FOR_CONFIRMATION / WAIT_FOR_WINDOW              -> wait
//   SKIP                                                  -> skip

package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/fundai/server/internal/advisor"
	"github.com/fundai/server/internal/agentreputation"
	"github.com/fundai/server/internal/ohlc"
)

// AdvisorRealisedReturnFn is the price lookup used by the advisor
// loop. Same shape as agentreputation.RealisedReturnFn but the
// fund_id is dropped (advisor mode is fund-less) and the market /
// benchmark are resolved by the closure itself based on the
// consultation row's market column (A-share vs US).
type AdvisorRealisedReturnFn func(ctx context.Context, market, symbol string, asof time.Time, horizonDays int) (realised, benchmark float64, ok bool, err error)

// advisorReputationLoopOptions configures the scheduler.
type advisorReputationLoopOptions struct {
	// Interval between backfill waves. Defaults to 24h.
	Interval time.Duration
	// JitterPct adds up to ±N% noise to the interval. Defaults
	// to 5% — same as the fund loop so a multi-replica wave is
	// spread.
	JitterPct float64
	// WaveTimeout caps one whole wave (all consultations within
	// the lookback window). Defaults to 5m.
	WaveTimeout time.Duration
	// LookbackDays sets the trailing window of consultations
	// re-graded each wave. Default 60d — long enough that a
	// 21d horizon row written ~21 days after the consultation
	// is still re-evaluated for the longer tail of late-arriving
	// price data.
	LookbackDays int
	// MinAgeDays is the floor — consultations younger than this
	// are skipped because no horizon has closed yet. Default 1d
	// (so the 1d-horizon row can be written the next morning).
	MinAgeDays int
	// Horizons is the list of forward windows (in days) the
	// backfill produces outcomes for. Defaults to {1, 5, 21} —
	// matches the fund loop so the same UI panels can display
	// both leaderboards.
	Horizons []int
	// BatchLimit caps how many consultations are pulled per
	// wave. Defaults to 200.
	BatchLimit int
}

// advisorReputationLoop is the runnable produced by newAdvisorReputationLoop.
type advisorReputationLoop struct {
	rep     *agentreputation.Repo
	advisor *advisor.Repo
	returns AdvisorRealisedReturnFn
	opts    advisorReputationLoopOptions
	rand    *rand.Rand
}

// newAdvisorReputationLoop wires the loop. Returns nil when either
// of the two repos is absent so the wiring layer can no-op.
func newAdvisorReputationLoop(
	rep *agentreputation.Repo,
	adv *advisor.Repo,
	returns AdvisorRealisedReturnFn,
	opts advisorReputationLoopOptions,
) *advisorReputationLoop {
	if rep == nil || adv == nil {
		return nil
	}
	if returns == nil {
		returns = nullAdvisorRealisedReturn
	}
	if opts.Interval <= 0 {
		opts.Interval = 24 * time.Hour
	}
	if opts.JitterPct <= 0 {
		opts.JitterPct = 0.05
	}
	if opts.WaveTimeout <= 0 {
		opts.WaveTimeout = 5 * time.Minute
	}
	if opts.LookbackDays <= 0 {
		opts.LookbackDays = 60
	}
	if opts.MinAgeDays < 0 {
		opts.MinAgeDays = 0
	}
	if len(opts.Horizons) == 0 {
		opts.Horizons = []int{1, 5, 21}
	}
	if opts.BatchLimit <= 0 || opts.BatchLimit > 500 {
		opts.BatchLimit = 200
	}
	return &advisorReputationLoop{
		rep:     rep,
		advisor: adv,
		returns: returns,
		opts:    opts,
		rand:    rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(time.Now().UnixNano()>>32))),
	}
}

// Run blocks until ctx is cancelled, scheduling a backfill wave
// every Interval ± jitter.
func (l *advisorReputationLoop) Run(ctx context.Context) {
	if l == nil {
		return
	}
	slog.Info("advisor_reputation_loop.start",
		"interval", l.opts.Interval,
		"lookback_days", l.opts.LookbackDays,
		"horizons", formatHorizons(l.opts.Horizons))
	for {
		select {
		case <-ctx.Done():
			slog.Info("advisor_reputation_loop.stop", "reason", ctx.Err())
			return
		case <-time.After(l.nextWaitWithJitter()):
		}
		l.runWave(ctx)
	}
}

// RunOnce executes one wave synchronously. Used by tests and the
// admin "rebuild advisor reputation" button.
func (l *advisorReputationLoop) RunOnce(ctx context.Context) (int, error) {
	if l == nil {
		return 0, nil
	}
	return l.runWave(ctx), nil
}

func (l *advisorReputationLoop) runWave(ctx context.Context) int {
	waveCtx, cancel := context.WithTimeout(ctx, l.opts.WaveTimeout)
	defer cancel()

	// olderThan is the cutoff: anything created after this is
	// too young (no horizon closed). Use the smallest horizon as
	// the floor so the 1d row is written ~1 day later.
	olderThan := time.Now().AddDate(0, 0, -l.opts.MinAgeDays)
	if l.opts.MinAgeDays == 0 && len(l.opts.Horizons) > 0 {
		olderThan = time.Now().AddDate(0, 0, -minHorizon(l.opts.Horizons))
	}

	rows, err := l.advisor.ListConsultationsForReputation(
		waveCtx,
		olderThan,
		l.opts.LookbackDays,
		l.opts.BatchLimit,
	)
	if err != nil {
		slog.Warn("advisor_reputation_loop.list_failed", "err", err.Error())
		return 0
	}
	if len(rows) == 0 {
		slog.Debug("advisor_reputation_loop.empty_wave",
			"older_than", olderThan.Format(time.RFC3339))
		return 0
	}

	var outcomes []agentreputation.Outcome
	for _, c := range rows {
		for _, h := range l.opts.Horizons {
			// Skip when the horizon isn't fully elapsed yet.
			if c.CreatedAt.AddDate(0, 0, h).After(time.Now()) {
				continue
			}
			realised, bench, ok, rErr := l.returns(waveCtx, c.Market, c.Symbol, c.CreatedAt, h)
			if rErr != nil {
				slog.Debug("advisor_reputation_loop.returns_err",
					"consultation_id", c.ID, "symbol", c.Symbol, "h", h, "err", rErr.Error())
				continue
			}
			if !ok {
				continue
			}
			for _, m := range c.MasterReports {
				o := buildMasterOutcome(c, m, h, realised, bench)
				if o == nil {
					continue
				}
				outcomes = append(outcomes, *o)
			}
			for _, t := range c.TacticReports {
				o := buildTacticOutcome(c, t, h, realised, bench)
				if o == nil {
					continue
				}
				outcomes = append(outcomes, *o)
			}
		}
	}

	if len(outcomes) == 0 {
		slog.Info("advisor_reputation_loop.wave_done",
			"consultations", len(rows), "outcomes", 0)
		return 0
	}
	if err := l.rep.UpsertOutcomes(waveCtx, outcomes); err != nil {
		slog.Warn("advisor_reputation_loop.upsert_failed", "err", err.Error())
		return 0
	}
	if err := l.rep.RecomputeStats(waveCtx, ""); err != nil {
		// stats recompute failure is degraded — outcomes are
		// already written, the next wave will retry the rollup.
		slog.Warn("advisor_reputation_loop.recompute_failed", "err", err.Error())
	}
	slog.Info("advisor_reputation_loop.wave_done",
		"consultations", len(rows), "outcomes", len(outcomes))
	return len(outcomes)
}

func (l *advisorReputationLoop) nextWaitWithJitter() time.Duration {
	base := l.opts.Interval
	if base <= 0 || l.opts.JitterPct <= 0 {
		return base
	}
	delta := float64(base) * l.opts.JitterPct
	noise := (l.rand.Float64()*2 - 1) * delta
	d := time.Duration(float64(base) + noise)
	if d <= 0 {
		d = base
	}
	return d
}

// --- mapping helpers --------------------------------------------------------

// buildMasterOutcome turns one MasterReport + realised return pair
// into a ledger Outcome row, or nil when the verdict is too vague
// to attribute alpha (e.g. PASS / HOLD with no direction).
func buildMasterOutcome(
	c advisor.ReputationConsultation,
	m advisor.MasterReportRow,
	horizon int,
	realised, bench float64,
) *agentreputation.Outcome {
	dir := masterVerdictToDirection(m.Verdict)
	if dir == "" {
		return nil
	}
	alpha := agentreputation.AlphaForDirection(dir, realised, bench)
	return &agentreputation.Outcome{
		// fund_id intentionally left blank — advisor rows live with
		// fund_id IS NULL per migration 099.
		AgentID:         "master:" + strings.ToLower(strings.TrimSpace(m.MasterKey)),
		AgentName:       advisorFirstNonEmpty(m.MasterNameZh, m.MasterNameEn, m.MasterKey),
		AgentKind:       agentreputation.KindMaster,
		Category:        "master",
		Symbol:          c.Symbol,
		AsOf:            c.CreatedAt,
		Direction:       dir,
		Confidence:      m.Confidence,
		RealisedReturn:  realised,
		BenchmarkReturn: bench,
		Alpha:           alpha,
		HorizonDays:     horizon,
		Note:            fmt.Sprintf("advisor:%s preset_key=%s", c.ID, c.AssetClass),
	}
}

// buildTacticOutcome mirrors buildMasterOutcome but for tactics.
// Tactic verdicts are richer (BUY_TAIL / BUY_DIP / ...); all
// "buy at *" verdicts collapse to DirBuy. WAIT / SKIP rows are
// still written so the leaderboard can show "this tactic correctly
// refused to enter on N days" — they just contribute to
// decisions_count but never to hits/misses (see the SQL CASE in
// RecomputeStats).
func buildTacticOutcome(
	c advisor.ReputationConsultation,
	t advisor.TacticReportRow,
	horizon int,
	realised, bench float64,
) *agentreputation.Outcome {
	dir := tacticVerdictToDirection(t.Verdict)
	if dir == "" {
		return nil
	}
	alpha := agentreputation.AlphaForDirection(dir, realised, bench)
	return &agentreputation.Outcome{
		AgentID:         "tactic:" + strings.ToLower(strings.TrimSpace(t.TacticKey)),
		AgentName:       advisorFirstNonEmpty(t.TacticNameZh, t.TacticNameEn, t.TacticKey),
		AgentKind:       agentreputation.KindTactic,
		Category:        "tactic",
		Symbol:          c.Symbol,
		AsOf:            c.CreatedAt,
		Direction:       dir,
		Confidence:      t.Confidence,
		RealisedReturn:  realised,
		BenchmarkReturn: bench,
		Alpha:           alpha,
		HorizonDays:     horizon,
		Note:            fmt.Sprintf("advisor:%s tactic=%s", c.ID, t.TacticKey),
	}
}

func masterVerdictToDirection(v string) agentreputation.Direction {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "STRONG_BUY", "BUY":
		return agentreputation.DirBuy
	case "AVOID", "SHORT":
		return agentreputation.DirAvoid
	case "HOLD", "PASS", "SKIP":
		return agentreputation.DirSkip
	}
	return ""
}

func tacticVerdictToDirection(v string) agentreputation.Direction {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "BUY_TAIL", "BUY_DIP", "CHASE_LIMIT_UP", "BUY_PULLBACK":
		return agentreputation.DirBuy
	case "WAIT_FOR_CONFIRMATION", "WAIT_FOR_WINDOW":
		return agentreputation.DirWait
	case "SKIP":
		return agentreputation.DirSkip
	}
	return ""
}

func advisorFirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func minHorizon(hs []int) int {
	m := hs[0]
	for _, h := range hs {
		if h < m {
			m = h
		}
	}
	return m
}

// --- default returns lookup -------------------------------------------------

// nullAdvisorRealisedReturn is the safe default — returns ok=false
// so the loop produces zero outcomes. The wiring layer swaps in
// advisorRealisedReturnFromOHLC when the shared OHLC fetcher is
// available.
func nullAdvisorRealisedReturn(_ context.Context, _, _ string, _ time.Time, _ int) (float64, float64, bool, error) {
	return 0, 0, false, nil
}

// advisorRealisedReturnFromOHLC builds an AdvisorRealisedReturnFn
// closing over the shared OHLC fetcher. Since advisor mode is
// fund-less, the benchmark is picked from the consultation's
// market column rather than from a fund profile.
//
// Default benchmarks:
//
//   * us / nasdaq / nyse  -> SPY   (S&P 500 proxy)
//   * cn / cn_a / a_share -> 000300 (CSI 300)
//   * hk                  -> 2800  (Tracker Fund of HK)
//
// Unknown markets fall back to symbol-vs-symbol (benchmark = symbol)
// which collapses alpha to zero but still records the realised
// return — better than silently dropping the row.
func advisorRealisedReturnFromOHLC(fetcher ohlc.Fetcher) AdvisorRealisedReturnFn {
	return func(ctx context.Context, market, symbol string, asof time.Time, horizonDays int) (float64, float64, bool, error) {
		if fetcher == nil {
			return 0, 0, false, nil
		}
		symbol = strings.TrimSpace(symbol)
		if symbol == "" || horizonDays <= 0 {
			return 0, 0, false, nil
		}
		market = strings.ToLower(strings.TrimSpace(market))
		if market == "" {
			market = guessMarketFromSymbol(symbol)
		}
		benchmark := advisorBenchmarkForMarket(market)
		lookback := horizonDays + 30
		endTime := asof.AddDate(0, 0, horizonDays+5)
		symBars, err := fetcher.Fetch(ctx, ohlc.FetchRequest{
			Symbol:    symbol,
			Market:    market,
			Interval:  ohlc.IntervalDay,
			LookbackN: lookback,
			EndTime:   endTime,
		})
		if err != nil || len(symBars) == 0 {
			return 0, 0, false, nil
		}
		var benchBars []ohlc.Bar
		if benchmark != "" && benchmark != symbol {
			benchBars, _ = fetcher.Fetch(ctx, ohlc.FetchRequest{
				Symbol:    benchmark,
				Market:    market,
				Interval:  ohlc.IntervalDay,
				LookbackN: lookback,
				EndTime:   endTime,
			})
		}
		exitTarget := asof.AddDate(0, 0, horizonDays)
		symEntry, ok1 := closeAtOrBefore(symBars, asof)
		symExit, ok2 := closeAtOrBefore(symBars, exitTarget)
		if !ok1 || !ok2 || symEntry <= 0 {
			return 0, 0, false, nil
		}
		if symExit == symEntry {
			return 0, 0, false, nil
		}
		realised := (symExit - symEntry) / symEntry
		var bench float64
		if len(benchBars) > 0 {
			be, okE := closeAtOrBefore(benchBars, asof)
			bx, okX := closeAtOrBefore(benchBars, exitTarget)
			if okE && okX && be > 0 {
				bench = (bx - be) / be
			}
		}
		return realised, bench, true, nil
	}
}

func advisorBenchmarkForMarket(market string) string {
	switch market {
	case "us", "usa", "nyse", "nasdaq":
		return "SPY"
	case "cn", "cn_a", "a_share", "ashare", "sh", "sz":
		return "000300"
	case "hk", "hkex":
		return "2800"
	}
	return ""
}

// guessMarketFromSymbol is a coarse fallback when the consultation
// row's market column was left blank. Six-digit numeric -> A-share,
// otherwise US.
func guessMarketFromSymbol(symbol string) string {
	if len(symbol) == 6 {
		allDigit := true
		for _, r := range symbol {
			if r < '0' || r > '9' {
				allDigit = false
				break
			}
		}
		if allDigit {
			return "cn"
		}
	}
	return "us"
}
