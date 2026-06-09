// daily_picks_loop.go — nightly publisher-mode wave that pre-scores
// every (symbol, preset) cell in every active watchlist and writes
// the shared cache row into daily_picks.
//
// Why a separate loop (vs piggy-backing on advisorReputationLoop):
//
//	The reputation loop GRADES historical consultations against
//	realised price moves. This loop GENERATES new consultations,
//	burns LLM dollars, and is the cost-bearing path of the entire
//	publisher product. Keeping them separate means an outage in
//	one doesn't block the other (e.g. if Polygon goes down the
//	reputation loop fails to fetch realised returns, but
//	publisher scoring still runs and the next day's daily picks
//	still ship).
//
// Scheduling model:
//
//	v1 uses a fixed-interval ticker keyed off the named tag in
//	the watchlist row (@daily_after_us_close → US 16:30 ET local
//	wall clock = +20 min after close). On boot the loop sleeps
//	to the next scheduled instant, then triggers daily forever.
//	Named tags are intentional — they make the operator-facing
//	semantics readable from psql ("the us pool runs after US
//	close") without needing a cron parser.
//
//	v1 deliberately does NOT use a literal cron expression even
//	though the column allows it; that path is reserved for ops
//	overrides and is hooked up via the RunOnce admin endpoint
//	rather than the auto-tick.
//
// Concurrency model:
//
//	One goroutine per binary. The existing leader-election gate
//	in main.go prevents two replicas from running waves in
//	parallel. Within a wave, symbols are scored SEQUENTIALLY
//	(no fan-out) because (a) the underlying LLM is the
//	bottleneck and we already saturate it with one in-flight
//	call, and (b) sequential failure isolation makes the
//	error-row UI much cleaner than chasing dropped goroutines.
//
// Cost guard-rails:
//
//	maxPicksPerWave (default 200) caps how many UPSERTs one
//	wave does. The seed watchlist is 50 tickers so we have 4x
//	headroom; if an ops user accidentally adds a 1000-ticker
//	watchlist, the cap protects the LLM bill from a runaway
//	mistake.

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/fundai/server/internal/advisor"
	"github.com/fundai/server/internal/dailypicks"
	"github.com/fundai/server/internal/subscription"
)

// dailyPicksLoopOptions configures the scheduler. Defaults are
// sized for the seed 50-ticker us_largecap_disruptive_v1 watchlist
// running 1×/day.
type dailyPicksLoopOptions struct {
	// CheckInterval is how often the loop wakes to evaluate
	// "is it time to run any watchlist now?". Defaults to 5
	// minutes — fine-grained enough that an operator adding a
	// new watchlist via SQL doesn't have to restart the binary
	// to see it picked up.
	CheckInterval time.Duration
	// JitterPct adds ±N% noise to CheckInterval to spread wake-
	// ups across replicas. Defaults to 5%.
	JitterPct float64
	// WaveTimeout caps a single watchlist's full run. Defaults
	// to 1 hour — at ~12s/ticker for a 50-ticker pool that's
	// 5x headroom for transient LLM slowness without leaving a
	// half-done watchlist mid-wave forever.
	WaveTimeout time.Duration
	// PerSymbolTimeout caps any single PublishConsult call.
	// Defaults to 90s — Gemini-3.1-pro-preview p99 is ~30s but
	// thinking models can spike under load.
	PerSymbolTimeout time.Duration
	// MaxPicksPerWave is the safety cap on UPSERTs per wave.
	// Defaults to 200.
	MaxPicksPerWave int
	// SkipIfAlreadyComplete: NOT settable from this struct any
	// more — see newDailyPicksLoop. Hardcoded to true because
	// the bool zero-value silently disabled it and double-billed
	// LLM on accidental re-runs. Kept as a runtime field on the
	// loop itself for legibility in the runWatchlistWave code.
	SkipIfAlreadyComplete bool
}

func defaultDailyPicksLoopOptions() dailyPicksLoopOptions {
	return dailyPicksLoopOptions{
		CheckInterval:         5 * time.Minute,
		JitterPct:             0.05,
		WaveTimeout:           1 * time.Hour,
		PerSymbolTimeout:      90 * time.Second,
		MaxPicksPerWave:       200,
		SkipIfAlreadyComplete: true,
	}
}

// dailyPicksLoop is the runnable.
type dailyPicksLoop struct {
	advisor *advisor.Service
	picks   *dailypicks.Repo
	// db is the same *sql.DB used by the rest of the app. The
	// loop only uses it for read-only cost summaries (aggregating
	// usage_entries after each wave). nil is permitted — cost
	// telemetry just becomes a no-op without it, so the loop
	// degrades cleanly in a test binary that doesn't wire it.
	db *sql.DB
	// usageTracker is the buffered LLM-cost recorder shared with
	// the rest of the app. The wave-cost summary calls Flush()
	// on it BEFORE querying usage_entries, otherwise our summary
	// runs against a buffer that hasn't hit the DB yet (default
	// flush interval is 10s). nil is permitted.
	usageTracker *subscription.UsageTracker
	clock        func() time.Time
	opts         dailyPicksLoopOptions
	rand         *rand.Rand
	// lastRunByWatchlist tracks the last successful wave start
	// per watchlist id, so a 5-minute check tick doesn't trigger
	// the same watchlist twice in one calendar day. Reset at
	// process restart — the SkipIfAlreadyComplete gate plus the
	// idempotent UPSERT keeps the worst case ("we restarted at
	// 16:35 ET, did the 16:30 ET wave already fire?") cheap.
	lastRunByWatchlist map[string]time.Time
}

func newDailyPicksLoop(
	adv *advisor.Service,
	picks *dailypicks.Repo,
	db *sql.DB,
	usageTracker *subscription.UsageTracker,
	opts dailyPicksLoopOptions,
) *dailyPicksLoop {
	if adv == nil || picks == nil {
		return nil
	}
	d := defaultDailyPicksLoopOptions()
	if opts.CheckInterval > 0 {
		d.CheckInterval = opts.CheckInterval
	}
	if opts.JitterPct > 0 {
		d.JitterPct = opts.JitterPct
	}
	if opts.WaveTimeout > 0 {
		d.WaveTimeout = opts.WaveTimeout
	}
	if opts.PerSymbolTimeout > 0 {
		d.PerSymbolTimeout = opts.PerSymbolTimeout
	}
	if opts.MaxPicksPerWave > 0 {
		d.MaxPicksPerWave = opts.MaxPicksPerWave
	}
	// SkipIfAlreadyComplete: NEVER merge the zero value from a
	// caller-supplied opts struct. Bool has no "not set" sentinel,
	// so `dailyPicksLoopOptions{}` (which main.go passes) would
	// stamp false on top of the desired default of true,
	// silently disabling the skip and double-billing the LLM if
	// someone re-runs the wave. The default is true and that's
	// what stays unless an explicit opts.DisableSkipIfAlreadyComplete
	// flag is added in the future.
	return &dailyPicksLoop{
		advisor:            adv,
		picks:              picks,
		db:                 db,
		usageTracker:       usageTracker,
		clock:              time.Now,
		opts:               d,
		rand:               rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(time.Now().UnixNano()>>32))),
		lastRunByWatchlist: make(map[string]time.Time),
	}
}

// Run is the long-running entry point. Blocks until ctx is
// cancelled. Re-checks the watchlist roster every CheckInterval ±
// jitter so newly-inserted rows get picked up without a restart.
func (l *dailyPicksLoop) Run(ctx context.Context) {
	if l == nil {
		return
	}
	slog.Info("daily_picks_loop.start",
		"check_interval", l.opts.CheckInterval,
		"wave_timeout", l.opts.WaveTimeout,
		"per_symbol_timeout", l.opts.PerSymbolTimeout,
		"max_per_wave", l.opts.MaxPicksPerWave)
	for {
		select {
		case <-ctx.Done():
			slog.Info("daily_picks_loop.stop", "reason", ctx.Err())
			return
		case <-time.After(l.nextCheckWait()):
		}
		l.tick(ctx)
	}
}

// RunOnce executes one full pass over every active watchlist
// synchronously, ignoring the "already ran today" gate. Used by
// (a) the admin "rebuild daily picks" endpoint and (b) the e2e
// smoke test. Returns the total number of UPSERTs performed.
func (l *dailyPicksLoop) RunOnce(ctx context.Context) (int, error) {
	if l == nil {
		return 0, nil
	}
	wls, err := l.picks.ListActiveWatchlists(ctx)
	if err != nil {
		return 0, fmt.Errorf("daily_picks_loop: list watchlists: %w", err)
	}
	// Cost rollup across the whole tick: per-wave summary is
	// emitted inside runWatchlistWave; this second summary adds
	// "what did the entire night cost across ALL presets" so the
	// operator gets a single bottom-line number for the daily
	// $-burn budget check.
	tickStart := l.clock()
	total := 0
	for _, wl := range wls {
		// RunOnce ignores the time-of-day gate but DOES respect
		// SkipIfAlreadyComplete — running RunOnce twice in a row
		// shouldn't double-bill LLM if the first run already
		// finished cleanly.
		n, runErr := l.runWatchlistWave(ctx, wl, true)
		total += n
		if runErr != nil {
			slog.Warn("daily_picks_loop.run_once.watchlist_failed",
				"watchlist", wl.Name, "err", runErr)
			// Don't bail — keep going so a single broken
			// watchlist doesn't poison the rest of the wave.
		}
	}
	if l.db != nil {
		if summary, sErr := l.summarizeWaveCost(ctx, tickStart); sErr == nil {
			usdEst := estimateUSDFromTokens(summary.inputTokens, summary.outputTokens)
			slog.Info("daily_picks_loop.run_once.total_cost",
				"watchlists_run", len(wls),
				"picks_written", total,
				"llm_calls", summary.calls,
				"input_tokens", summary.inputTokens,
				"output_tokens", summary.outputTokens,
				"total_tokens", summary.inputTokens+summary.outputTokens,
				"cost_cny_cents_recorded", summary.costCNYCents,
				"price_cny_cents_recorded", summary.priceCNYCents,
				"usd_est_from_tokens", usdEst,
				"usd_per_pick", divIgnoreZero(usdEst, total),
				"monthly_usd_est_at_this_rate", usdEst*30,
				"elapsed_sec", time.Since(tickStart).Seconds())
		}
	}
	return total, nil
}

// estimateUSDFromTokens converts (input, output) token counts to a
// rough USD estimate using Gemini-3.1-pro-preview list pricing as
// of 2026-06:
//
//	$1.25 / M input tokens
//	$5.00 / M output tokens
//
// This is intentionally a TOKEN-BASED estimate rather than reading
// the recorded cost_cents column, because the pricing config the
// rest of the app uses is independently configured and can drift
// (or be zero for new models). Token counts are sourced direct
// from the provider response and are the most reliable signal.
//
// Update this constant block when we switch tiers or the prices
// move materially. The number is "what should I expect to pay if
// nothing went wrong with billing"; it does NOT replace the
// canonical cost figure for accounting.
func estimateUSDFromTokens(inputTokens, outputTokens int) float64 {
	const (
		usdPerMInput  = 1.25
		usdPerMOutput = 5.00
	)
	return float64(inputTokens)/1_000_000*usdPerMInput +
		float64(outputTokens)/1_000_000*usdPerMOutput
}

// divIgnoreZero returns 0 instead of NaN/Inf when n is 0, so the
// log line is always parseable by JSON tooling.
func divIgnoreZero(x float64, n int) float64 {
	if n <= 0 {
		return 0
	}
	return x / float64(n)
}

// tick is the per-CheckInterval body. Walks every active
// watchlist; for each one, decides whether "now" is past its
// scheduled run instant for today AND we haven't already run it
// today; if both, fires runWatchlistWave.
func (l *dailyPicksLoop) tick(ctx context.Context) {
	wls, err := l.picks.ListActiveWatchlists(ctx)
	if err != nil {
		slog.Warn("daily_picks_loop.tick.list_failed", "err", err)
		return
	}
	now := l.clock()
	for _, wl := range wls {
		scheduledAt, ok := nextScheduledInstant(wl.ScheduleCron, now)
		if !ok {
			// Unknown / unsupported schedule tag — skip
			// silently; an ops user can RunOnce manually.
			continue
		}
		if now.Before(scheduledAt) {
			continue
		}
		// Run-already-today gate: if the last successful run's
		// calendar day (UTC) == today (UTC), skip. Using the
		// in-process map is fine because (a) the per-day cache
		// in daily_picks is idempotent anyway, and (b) a
		// restart simply gets one extra "is the cell complete?"
		// query at boot, which is cheap.
		if last, seen := l.lastRunByWatchlist[wl.ID]; seen {
			if sameUTCDate(last, now) {
				continue
			}
		}
		n, runErr := l.runWatchlistWave(ctx, wl, false)
		if runErr != nil {
			slog.Warn("daily_picks_loop.tick.watchlist_failed",
				"watchlist", wl.Name, "err", runErr)
			continue
		}
		l.lastRunByWatchlist[wl.ID] = now
		slog.Info("daily_picks_loop.tick.watchlist_done",
			"watchlist", wl.Name, "picks_written", n)
	}
}

// runWatchlistWave scores every symbol in a watchlist under its
// configured preset. If forceRun is true the "already ran today"
// in-process gate is bypassed (used by RunOnce); the
// SkipIfAlreadyComplete gate (which consults the DB) still
// applies if enabled, so manual re-runs don't burn money when
// the row set is already there.
func (l *dailyPicksLoop) runWatchlistWave(ctx context.Context, wl dailypicks.Watchlist, forceRun bool) (int, error) {
	if len(wl.Symbols) == 0 {
		return 0, nil
	}
	// Capture wave start for the cost-summary query at the end:
	// every usage_entries row written between waveStart and "now"
	// for the publisher user is attributable to this wave.
	waveStart := l.clock()
	waveCtx, cancel := context.WithTimeout(ctx, l.opts.WaveTimeout)
	defer cancel()

	today := l.clock().UTC().Truncate(24 * time.Hour)

	// Pre-flight cost guard: if SkipIfAlreadyComplete and we
	// already have N rows for (market, preset, today) where N >=
	// len(symbols), the prior wave finished. Bail before we burn
	// LLM dollars re-doing it.
	if l.opts.SkipIfAlreadyComplete {
		existing, cerr := l.picks.CountForDay(waveCtx, wl.Market, wl.PresetKey, today)
		if cerr == nil && existing >= len(wl.Symbols) {
			slog.Info("daily_picks_loop.wave.skip_complete",
				"watchlist", wl.Name, "existing", existing, "want", len(wl.Symbols))
			return 0, nil
		}
	}

	cap := len(wl.Symbols)
	if cap > l.opts.MaxPicksPerWave {
		slog.Warn("daily_picks_loop.wave.capped",
			"watchlist", wl.Name, "symbols", cap, "cap", l.opts.MaxPicksPerWave)
		cap = l.opts.MaxPicksPerWave
	}

	written := 0
	for _, raw := range wl.Symbols[:cap] {
		sym := strings.ToUpper(strings.TrimSpace(raw))
		if sym == "" {
			continue
		}
		select {
		case <-waveCtx.Done():
			slog.Warn("daily_picks_loop.wave.timeout",
				"watchlist", wl.Name, "completed", written, "remaining", cap-written)
			return written, waveCtx.Err()
		default:
		}
		if l.opts.SkipIfAlreadyComplete {
			// Per-symbol short-circuit: if the row already
			// exists for today, don't re-call the LLM. This
			// covers the "wave failed halfway, restarted"
			// case: the second wave only pays for the
			// remaining cells.
			//
			// We deliberately do NOT gate this on !forceRun.
			// forceRun's purpose is to bypass the per-watchlist
			// "already ran today" in-process cache so RunOnce
			// can pick up where a previous wave left off; it
			// is NEVER correct to re-pay the LLM for a symbol
			// that already has a row, because the result is
			// publisher-mode shared content keyed by
			// (symbol, market, preset, date) — re-running just
			// overwrites the existing row with an indistinguishable
			// one. Earlier versions DID gate on !forceRun, which
			// caused a partial wave (e.g. garp 18/50 after
			// timeout) to re-bill all 18 existing rows on the
			// next RunOnce instead of resuming at row #19.
			if _, gerr := l.picks.Get(waveCtx, sym, wl.Market, wl.PresetKey, today); gerr == nil {
				continue
			}
		}
		if err := l.scoreOne(waveCtx, wl, sym); err != nil {
			slog.Warn("daily_picks_loop.wave.symbol_failed",
				"watchlist", wl.Name, "symbol", sym, "err", err)
			// Don't bail — keep going so one bad ticker
			// (delisted, Yahoo 404) doesn't poison the wave.
			continue
		}
		written++
	}

	// Emit per-wave cost summary so the operator can see
	// "conservative preset cost $X for Y stocks tonight"
	// without grepping every per-call usage_entries row. This
	// query is cheap (single aggregate over a short time
	// window, indexed by user_id + created_at) and we run it
	// outside waveCtx so a wave-timeout doesn't suppress the
	// telemetry — we WANT the partial-run cost visible.
	if l.db != nil {
		if summary, sErr := l.summarizeWaveCost(ctx, waveStart); sErr == nil {
			slog.Info("daily_picks_loop.wave.cost",
				"watchlist", wl.Name,
				"preset", wl.PresetKey,
				"symbols_scored", written,
				"llm_calls", summary.calls,
				"input_tokens", summary.inputTokens,
				"output_tokens", summary.outputTokens,
				"total_tokens", summary.inputTokens+summary.outputTokens,
				"cost_cny_cents_recorded", summary.costCNYCents,
				"price_cny_cents_recorded", summary.priceCNYCents,
				"usd_est_from_tokens", estimateUSDFromTokens(summary.inputTokens, summary.outputTokens),
				"elapsed_sec", time.Since(waveStart).Seconds())
		} else {
			slog.Warn("daily_picks_loop.wave.cost_summary_failed",
				"watchlist", wl.Name, "err", sErr)
		}
	}

	return written, nil
}

// waveCostSummary is what summarizeWaveCost returns. Plain struct
// rather than streaming named return values because we want all
// fields aggregated in ONE row of slog output (operators reading
// logs at 2am don't want to correlate split lines).
type waveCostSummary struct {
	calls          int
	inputTokens    int
	outputTokens   int
	costCNYCents   int64 // raw provider cost in CNY cents
	priceCNYCents  int64 // user-facing price in CNY cents (cost × markup)
}

// summarizeWaveCost queries usage_entries for everything the
// publisher user spent between waveStart and now. This is the
// cleanest signal because usage_entries is written by the LLM
// adapter on EVERY call regardless of which Go module triggered
// it — no per-module instrumentation drift.
//
// Returns an empty summary (not an error) if no rows match — the
// wave may have been entirely cache-hits or all-skipped.
//
// The 7.2 CNY/USD conversion in the log line is a rough estimate
// for human-eyeball convenience; canonical accounting stays in
// CNY cents because that's what the pricing table stores.
func (l *dailyPicksLoop) summarizeWaveCost(ctx context.Context, waveStart time.Time) (waveCostSummary, error) {
	// Force any buffered usage records to disk so our aggregate
	// includes the rows the wave JUST wrote. subscription.UsageTracker
	// flushes on its own 10s tick but the wave can complete inside
	// that window; without this Flush the summary would report 0.
	if l.usageTracker != nil {
		if err := l.usageTracker.Flush(ctx); err != nil {
			slog.Warn("daily_picks_loop.cost_summary.flush_failed", "err", err)
			// Continue anyway — partial / stale numbers beat
			// no telemetry, and the next wave's summary will
			// pick up the missed rows.
		}
	}
	var s waveCostSummary
	const q = `
		SELECT COALESCE(count(*), 0),
		       COALESCE(sum(input_tokens), 0),
		       COALESCE(sum(output_tokens), 0),
		       COALESCE(sum(cost_cents), 0)::BIGINT,
		       COALESCE(sum(price_cents), 0)::BIGINT
		  FROM usage_entries
		 WHERE user_id = $1::UUID
		   AND created_at >= $2`
	row := l.db.QueryRowContext(ctx, q,
		advisor.PublisherUserID, waveStart)
	if err := row.Scan(&s.calls, &s.inputTokens, &s.outputTokens, &s.costCNYCents, &s.priceCNYCents); err != nil {
		return waveCostSummary{}, err
	}
	return s, nil
}

// scoreOne runs PublishConsult for a single ticker, honouring the
// per-symbol timeout. Wrapped so the wave loop reads cleanly.
func (l *dailyPicksLoop) scoreOne(parentCtx context.Context, wl dailypicks.Watchlist, symbol string) error {
	ctx, cancel := context.WithTimeout(parentCtx, l.opts.PerSymbolTimeout)
	defer cancel()
	_, err := l.advisor.PublishConsult(ctx, advisor.PublishConsultInput{
		Symbol:    symbol,
		Market:    wl.Market,
		PresetKey: wl.PresetKey,
	})
	return err
}

// --- helpers -----------------------------------------------------------------

// nextCheckWait returns the (CheckInterval ± JitterPct) wait
// before the next tick.
func (l *dailyPicksLoop) nextCheckWait() time.Duration {
	base := l.opts.CheckInterval
	if l.opts.JitterPct <= 0 {
		return base
	}
	delta := float64(base) * l.opts.JitterPct * (2*l.rand.Float64() - 1)
	w := time.Duration(float64(base) + delta)
	if w < time.Second {
		w = time.Second
	}
	return w
}

// nextScheduledInstant maps a named schedule tag onto the
// firing instant the watchlist should next aim for. Returns
// ok=false for unknown tags so the caller can skip-and-warn
// instead of busy-looping.
//
// IMPORTANT SEMANTIC NOTE — return value is the firing instant
// the caller compares against `now`, NOT a strict "future" time:
//
//	caller:  if now.Before(scheduledAt) { continue }
//
// So when the daily window for TODAY has already passed, this
// function MUST return today's instant (now will not be before
// it → caller fires) and NOT tomorrow's (now would be before it
// → caller waits a whole 24h, missing today's run entirely).
//
// The "did we already run today" gate that prevents re-firing
// is the lastRunByWatchlist map in tick() — not this function.
// Earlier versions of this code returned `today + 1 day` once
// `nowLocal >= today`, which silently disabled the daily cron:
// every tick after 16:30 ET would set scheduledAt to tomorrow,
// caller would `continue` for the rest of the day, and only the
// 16:30:00 ET tick (which jitter + 5-min ticker makes very
// unlikely to land exactly there) had any chance to fire.
//
// Time zones: US tags use America/New_York (16:30 ET = post-close
// + 30 min); CN tags use Asia/Shanghai (15:30 CST = post-close +
// 30 min). The 30-min buffer lets after-hours news / Yahoo
// re-indexing settle before we score.
func nextScheduledInstant(tag string, now time.Time) (time.Time, bool) {
	switch strings.TrimSpace(strings.ToLower(tag)) {
	case "@daily_after_us_close":
		loc, err := time.LoadLocation("America/New_York")
		if err != nil {
			// Container without tzdata — fall back to UTC
			// translation of 20:30 (16:30 ET + 4 hours EST,
			// or +5 EDT — this is the corner case that
			// motivates dropping tzdata into the Dockerfile).
			loc = time.UTC
		}
		nowLocal := now.In(loc)
		today := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 16, 30, 0, 0, loc)
		// Day-roll: if "now" is already past today's close, the
		// "next" instant is TOMORROW's close, not today's. Without
		// this guard the function returns a past timestamp and the
		// scheduler relies on lastRunByWatchlist as the only gate
		// against double-firing — a fragile two-level dependency.
		if !today.After(now) {
			today = today.AddDate(0, 0, 1)
		}
		return today.UTC(), true
	case "@daily_after_cn_close":
		loc, err := time.LoadLocation("Asia/Shanghai")
		if err != nil {
			loc = time.UTC
		}
		nowLocal := now.In(loc)
		today := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 15, 30, 0, 0, loc)
		// Same day-roll as @daily_after_us_close above.
		if !today.After(now) {
			today = today.AddDate(0, 0, 1)
		}
		return today.UTC(), true
	default:
		return time.Time{}, false
	}
}

// sameUTCDate reports whether a and b fall on the same UTC calendar
// day. Used by the "already ran today" gate.
func sameUTCDate(a, b time.Time) bool {
	au := a.UTC()
	bu := b.UTC()
	return au.Year() == bu.Year() && au.Month() == bu.Month() && au.Day() == bu.Day()
}

// ensure errors import doesn't go unused if scoreOne ever returns
// a sentinel — keeps refactors honest.
var _ = errors.New
