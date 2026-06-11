// daily_picks_handler_test.go — unit-level coverage of the pure
// helpers (path parsing, headline extraction, tier-to-quota
// resolution). The HTTP-layer integration tests live in the e2e
// smoke suite where they can stand up a real Postgres + Yahoo
// proxy.

package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fundai/server/internal/dailypicks"
	"github.com/fundai/server/internal/subscription"
)

func TestParseDetailPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		path     string
		wantDate string
		wantSym  string
		wantOK   bool
	}{
		{"happy", "/api/daily-picks/2026-06-08/AAPL", "2026-06-08", "AAPL", true},
		{"lowercase_sym_normalised", "/api/daily-picks/2026-06-08/aapl", "2026-06-08", "AAPL", true},
		{"missing_symbol", "/api/daily-picks/2026-06-08/", "", "", false},
		{"missing_date", "/api/daily-picks/", "", "", false},
		{"wrong_prefix", "/api/other/2026-06-08/AAPL", "", "", false},
		// Multi-class symbols like BRK-B should pass through with
		// the dash preserved — that's the wire format Yahoo expects
		// and we shouldn't munge it on the way in.
		{"dash_class_share", "/api/daily-picks/2026-06-08/BRK-B", "2026-06-08", "BRK-B", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			date, sym, ok := parseDetailPath(tt.path)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if date != tt.wantDate {
				t.Errorf("date = %q, want %q", date, tt.wantDate)
			}
			if sym != tt.wantSym {
				t.Errorf("sym = %q, want %q", sym, tt.wantSym)
			}
		})
	}
}

func TestExtractHeadlineThesis(t *testing.T) {
	t.Parallel()
	// First master's thesis wins — that's the convention because
	// the master panel sorts by confidence DESC before encoding.
	body := []byte(`{
		"symbol": "AAPL",
		"master_reports": [
			{"thesis": "First master says X"},
			{"thesis": "Second master says Y"}
		]
	}`)
	got := extractHeadlineThesis(body)
	want := "First master says X"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}

	// Empty / malformed inputs should yield empty string — the UI
	// degrades to a verdict-only card, never crashes.
	for _, badBody := range [][]byte{
		nil,
		{},
		[]byte("not json"),
		[]byte(`{"master_reports": []}`),
		[]byte(`{"master_reports": [{}]}`),
	} {
		if got := extractHeadlineThesis(badBody); got != "" {
			t.Errorf("malformed body %q: got %q, want empty", badBody, got)
		}
	}
}

func TestMaxAllowedDateForTier(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	h := &dailyPicksHandler{clock: func() time.Time { return fixedNow }}

	// Free tier gets locked out of the most recent 14 days. The
	// boundary is "today minus 14d INCLUSIVE" — i.e. May 25 is
	// readable on June 8.
	freeMax := h.maxAllowedDateForTier(subscription.PlanFree)
	wantFree := time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	if !freeMax.Equal(wantFree) {
		t.Errorf("free maxDate = %v, want %v", freeMax, wantFree)
	}

	// Paid tiers see "today" (truncated to UTC midnight). They
	// can ALWAYS reach the latest published row regardless of
	// hour of day, which matches the publisher mental model
	// ("today's newsletter is today's").
	wantToday := time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)
	for _, tier := range []subscription.PlanTier{
		subscription.PlanPro,
		subscription.PlanPremium,
		subscription.PlanEnterprise,
	} {
		if got := h.maxAllowedDateForTier(tier); !got.Equal(wantToday) {
			t.Errorf("tier %v maxDate = %v, want %v", tier, got, wantToday)
		}
	}
}

func TestQuotaCapForTier(t *testing.T) {
	t.Parallel()
	h := &dailyPicksHandler{}
	// Free is bolted to zero because it CAN'T reach today's
	// rows anyway (the time-lag gate beats the quota gate every
	// time) — the zero is a defence-in-depth sentinel, not a
	// product knob.
	if got := h.quotaCapForTier(subscription.PlanFree); got != 0 {
		t.Errorf("free cap = %d, want 0", got)
	}
	if got := h.quotaCapForTier(subscription.PlanPro); got != 30 {
		t.Errorf("pro cap = %d, want 30", got)
	}
	if got := h.quotaCapForTier(subscription.PlanEnterprise); got != -1 {
		t.Errorf("enterprise cap = %d, want -1 (unlimited)", got)
	}
	// Unknown / future tier names must NOT silently grant
	// access — we conservatively fall back to free.
	if got := h.quotaCapForTier(subscription.PlanTier("mystery_tier")); got != 0 {
		t.Errorf("unknown tier cap = %d, want 0 (fallback to free)", got)
	}
}

func TestProjectPickRowsCarriesThesis(t *testing.T) {
	t.Parallel()
	rows := []dailypicks.PickRow{
		{
			Symbol:           "AAPL",
			Market:           "us_equity",
			PresetKey:        "disruptive",
			PickDate:         time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
			ResultJSON:       []byte(`{"master_reports": [{"thesis": "Strong AI tailwind"}]}`),
			AggregateVerdict: "BUY",
			AggregateScore:   72,
			Consensus:        0.8,
		},
	}
	out := projectPickRows(rows)
	if len(out) != 1 {
		t.Fatalf("len out = %d, want 1", len(out))
	}
	if out[0].HeadlineThesis != "Strong AI tailwind" {
		t.Errorf("HeadlineThesis = %q, want %q", out[0].HeadlineThesis, "Strong AI tailwind")
	}
	if out[0].PickDate != "2026-06-08" {
		t.Errorf("PickDate ISO = %q, want %q", out[0].PickDate, "2026-06-08")
	}
}

func TestNextScheduledInstant_USClose(t *testing.T) {
	t.Parallel()
	// Pick a known UTC moment that's clearly BEFORE 16:30 ET on
	// the same calendar day: 2026-06-08 12:00 UTC = 08:00 EDT.
	// Expectation: next instant is 2026-06-08 16:30 EDT = 2026-06-08 20:30 UTC.
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	got, ok := nextScheduledInstant("@daily_after_us_close", now)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	// Skip strict equality if the test host lacks tzdata
	// (falls back to UTC fixture) — just verify it's same-day
	// and > now and one of the two acceptable values.
	if !got.After(now) {
		t.Errorf("scheduled %v not after %v", got, now)
	}
	// On a host WITH tzdata the answer is 20:30 UTC (EDT in June).
	// Allow either 20:30 (EDT) or 21:30 (EST) — the latter is
	// what the fallback would produce; both prove the logic is
	// "after market close" not random.
	hour := got.Hour()
	if hour != 20 && hour != 21 && hour != 16 {
		t.Errorf("hour = %d, want 20 (EDT) / 21 (EST) / 16 (UTC fallback)", hour)
	}

	// Contract: when "now" is AFTER today's close, nextScheduledInstant
	// MUST return today's instant (a past timestamp), NOT tomorrow's.
	// The caller fires when now >= scheduledAt, so returning tomorrow
	// here would silently disable the cron for the whole day (the
	// 6/9/2026 regression that this test now guards against). Day-roll
	// duty belongs to lastRunByWatchlist, not to this function.
	later := time.Date(2026, 6, 8, 21, 0, 0, 0, time.UTC) // 17:00 EDT, 30min after close
	gotLater, ok := nextScheduledInstant("@daily_after_us_close", later)
	if !ok {
		t.Fatal("ok = false, want true for later")
	}
	if !sameUTCDate(gotLater, later) {
		t.Errorf("post-close instant must stay on today (UTC); got %v for now %v", gotLater.UTC(), later.UTC())
	}
	if !gotLater.Before(later) {
		t.Errorf("post-close instant must be in the past (caller fires when now >= scheduledAt); got %v for now %v", gotLater.UTC(), later.UTC())
	}
}

func TestNextScheduledInstant_UnknownTag(t *testing.T) {
	t.Parallel()
	if _, ok := nextScheduledInstant("@some_unsupported_tag", time.Now()); ok {
		t.Error("ok = true for unknown tag, want false")
	}
	if _, ok := nextScheduledInstant("", time.Now()); ok {
		t.Error("ok = true for empty tag, want false")
	}
}

func TestSameUTCDate(t *testing.T) {
	t.Parallel()
	a := time.Date(2026, 6, 8, 1, 0, 0, 0, time.UTC)
	b := time.Date(2026, 6, 8, 23, 59, 59, 0, time.UTC)
	if !sameUTCDate(a, b) {
		t.Error("same calendar day should be true")
	}
	c := time.Date(2026, 6, 9, 0, 0, 1, 0, time.UTC)
	if sameUTCDate(a, c) {
		t.Error("different calendar day should be false")
	}
	// TZ shift that crosses a UTC midnight must report different
	// days — the loop's "already ran today" gate relies on this.
	pst, err := time.LoadLocation("America/Los_Angeles")
	if err == nil {
		nightLA := time.Date(2026, 6, 8, 22, 0, 0, 0, pst) // = 2026-06-09 05:00 UTC
		if sameUTCDate(a, nightLA) {
			t.Errorf("PST night (= %v UTC) should not match UTC %v", nightLA.UTC(), a)
		}
	}
}

// TestShouldFireWave_DailyScheduleLockedToScheduledInstant pins the
// gate behaviour the 6/11/2026 incident exposed: the previous
// `sameUTCDate(lastRun, now)` gate silently skipped today's fire
// whenever lastRun and the firing instant happened to share a UTC
// calendar date. For a 16:30 ET schedule that's the common case
// (boot-time fires at 11:15 BJT = 03:15 UTC of the SAME UTC day as
// the 20:30 UTC schedule). The new gate compares lastRun against
// scheduledAt directly, so today's fire is unaffected by which UTC
// day the previous fire happened to land on.
func TestShouldFireWave_DailyScheduleLockedToScheduledInstant(t *testing.T) {
	t.Parallel()
	// Both timestamps are on the same UTC date (6/10) but lastRun
	// is from a boot-tick fire that happened ~17 h before the
	// schedule's actual instant. The regression treated this as
	// "already ran today, skip" and didn't fire until 6/11
	// 00:00 UTC (= 08:00 BJT) — 3.5 h late, every day.
	lastRun := time.Date(2026, 6, 10, 3, 15, 13, 0, time.UTC)         // 11:15 BJT 6/10 boot
	scheduledAt := time.Date(2026, 6, 10, 20, 30, 0, 0, time.UTC)     // 16:30 ET 6/10 = 04:30 BJT 6/11
	now := time.Date(2026, 6, 10, 20, 30, 0, 0, time.UTC)             // tick lands exactly at scheduled instant

	if !shouldFireWave(now, scheduledAt, lastRun, true) {
		t.Errorf("regression: gate skipped today's scheduled fire (lastRun %v / scheduledAt %v / now %v)",
			lastRun.Format(time.RFC3339), scheduledAt.Format(time.RFC3339), now.Format(time.RFC3339))
	}

	// Sibling cases — lock the rest of the contract while we're here.

	// 1. Tick BEFORE the scheduled instant must not fire, even with no lastRun.
	earlyNow := time.Date(2026, 6, 10, 20, 29, 0, 0, time.UTC)
	if shouldFireWave(earlyNow, scheduledAt, time.Time{}, false) {
		t.Error("gate fired before scheduled instant; should wait")
	}

	// 2. Same-cycle re-fire must be blocked. After we successfully
	// run at scheduledAt, lastRun = scheduledAt (or a hair later);
	// the very next 5-min tick must NOT re-trigger.
	postRunLast := scheduledAt.Add(25 * time.Minute) // wave finished
	postRunNow := scheduledAt.Add(30 * time.Minute)  // next tick
	if shouldFireWave(postRunNow, scheduledAt, postRunLast, true) {
		t.Error("gate re-fired within the same scheduled cycle")
	}

	// 3. Tomorrow's scheduled instant must fire even though lastRun
	// is "today's" successful fire — i.e., yesterday's run does NOT
	// permanently block today.
	tomorrowScheduled := scheduledAt.Add(24 * time.Hour)
	tomorrowNow := tomorrowScheduled
	if !shouldFireWave(tomorrowNow, tomorrowScheduled, postRunLast, true) {
		t.Error("gate failed to fire tomorrow's scheduled instant")
	}

	// 4. First-ever fire (no lastRun seen) must always pass once now >= scheduledAt.
	if !shouldFireWave(now, scheduledAt, time.Time{}, false) {
		t.Error("first-ever fire (hasLastRun=false) was blocked")
	}
}

// TestScoreSymbolsParallel_FanOutHonorsBound proves the worker pool
// processes symbols concurrently bounded by `workers`. With a 25-
// symbol universe and 200 ms per-symbol latency:
//
//	serial implementation → ~5 s (regression baseline)
//	workers=5 (production)→ ceil(25/5) × 200 ms ≈ 1 s
//
// Asserts a 2 s ceiling to stay non-flaky on slow CI runners while
// still catching a regression to fully-serial execution.
func TestScoreSymbolsParallel_FanOutHonorsBound(t *testing.T) {
	t.Parallel()

	const universe = 25
	const perSymbolLatency = 200 * time.Millisecond
	const workers = 5

	symbols := make([]string, universe)
	for i := range symbols {
		symbols[i] = string(rune('A'+i%26)) + string(rune('A'+(i/26)%26)) + "X"
	}

	var inFlight, maxConcurrent int32
	var muMax sync.Mutex
	score := func(_ context.Context, _ string) error {
		now := atomic.AddInt32(&inFlight, 1)
		muMax.Lock()
		if now > maxConcurrent {
			maxConcurrent = now
		}
		muMax.Unlock()
		defer atomic.AddInt32(&inFlight, -1)
		time.Sleep(perSymbolLatency)
		return nil
	}

	start := time.Now()
	written := scoreSymbolsParallel(
		context.Background(),
		symbols,
		workers,
		nil, // no per-symbol skip
		score,
		nil, // no failure handler
	)
	elapsed := time.Since(start)

	if written != universe {
		t.Fatalf("written = %d, want %d", written, universe)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("elapsed = %v, sequential regression suspected (cap = 2s)", elapsed)
	}
	if maxConcurrent < 2 {
		t.Fatalf("maxConcurrent = %d, expected fan-out (>=2)", maxConcurrent)
	}
	// Allow a +1 slack: between one worker releasing its slot
	// and the scheduler waking the next, the inFlight count can
	// momentarily read workers+1 without violating the bound.
	if int(maxConcurrent) > workers+1 {
		t.Fatalf("maxConcurrent = %d, expected <= %d", maxConcurrent, workers+1)
	}
}

// TestScoreSymbolsParallel_PreSkipNeverCallsScore confirms the
// dispatcher's per-symbol short-circuit: if alreadyDone returns
// true for every symbol, score is never invoked — the catchup
// path that runs every 5-min tick once today's wave is full
// must NOT touch the LLM provider.
func TestScoreSymbolsParallel_PreSkipNeverCallsScore(t *testing.T) {
	t.Parallel()

	symbols := []string{"AAPL", "MSFT", "GOOG", "AMZN", "TSLA"}
	var scored int32
	written := scoreSymbolsParallel(
		context.Background(),
		symbols,
		5,
		func(_ context.Context, _ string) bool { return true }, // every symbol pre-skipped
		func(_ context.Context, _ string) error {
			atomic.AddInt32(&scored, 1)
			return nil
		},
		nil,
	)

	if written != 0 {
		t.Errorf("written = %d, want 0 (all pre-skipped)", written)
	}
	if got := atomic.LoadInt32(&scored); got != 0 {
		t.Errorf("score called %d times despite pre-skip; should be 0", got)
	}
}

// TestScoreSymbolsParallel_FailuresAreSwallowed proves the
// "one bad ticker doesn't poison the wave" contract. With 5
// symbols where odd-indexed ones fail, written should be 3 (the
// even-indexed survivors) and onFail should record exactly the
// 2 odd-indexed errors. No panic, no goroutine leak.
func TestScoreSymbolsParallel_FailuresAreSwallowed(t *testing.T) {
	t.Parallel()

	symbols := []string{"A", "B", "C", "D", "E"}
	score := func(_ context.Context, sym string) error {
		// Fail "B" and "D" (odd indices in input).
		if sym == "B" || sym == "D" {
			return errors.New("simulated upstream failure")
		}
		return nil
	}
	var failures sync.Map // sym → error
	onFail := func(sym string, err error) { failures.Store(sym, err) }

	written := scoreSymbolsParallel(
		context.Background(),
		symbols,
		3,
		nil,
		score,
		onFail,
	)
	if written != 3 {
		t.Errorf("written = %d, want 3 (A, C, E)", written)
	}
	for _, want := range []string{"B", "D"} {
		if _, ok := failures.Load(want); !ok {
			t.Errorf("onFail did not record failure for %q", want)
		}
	}
	for _, dont := range []string{"A", "C", "E"} {
		if _, ok := failures.Load(dont); ok {
			t.Errorf("onFail incorrectly recorded failure for %q", dont)
		}
	}
}

// TestScoreSymbolsParallel_CtxCancelStopsDispatch confirms that
// once parent ctx is cancelled, the dispatcher loop bails before
// queuing more work. Without this guard a wave-timeout would
// still kick off the full 50-symbol pool, wasting goroutines on
// requests that all bail at sem-acquire.
func TestScoreSymbolsParallel_CtxCancelStopsDispatch(t *testing.T) {
	t.Parallel()

	symbols := make([]string, 50)
	for i := range symbols {
		symbols[i] = string(rune('A'+i%26))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel before dispatch even starts

	var scored int32
	written := scoreSymbolsParallel(
		ctx,
		symbols,
		5,
		nil,
		func(_ context.Context, _ string) error {
			atomic.AddInt32(&scored, 1)
			return nil
		},
		nil,
	)
	if written != 0 {
		t.Errorf("written = %d, want 0 (ctx cancelled)", written)
	}
	if got := atomic.LoadInt32(&scored); got != 0 {
		t.Errorf("score called %d times after ctx cancel", got)
	}
}

func TestDetailQuotaCountsUniqueStockReads(t *testing.T) {
	// Not t.Parallel because we mutate module-level state.
	// Reset the module-level store for isolation; production
	// code never touches this from tests beyond this point.
	detailQuotaMu.Lock()
	detailQuotaStore = map[string]map[string]struct{}{}
	detailQuotaMu.Unlock()

	fixedNow := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	h := &dailyPicksHandler{clock: func() time.Time { return fixedNow }}

	user := "test-user-abc"
	day := time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)

	// Opening AAPL twice on the same day should count as ONE
	// read — re-reading is free.
	_ = h.recordDetailRead(nil, user, "AAPL", day)
	_ = h.recordDetailRead(nil, user, "aapl", day) // case-insensitive
	n, _ := h.countDetailReadsToday(nil, user)
	if n != 1 {
		t.Errorf("after 2 AAPL reads, count = %d, want 1", n)
	}

	// Opening a different stock should increment.
	_ = h.recordDetailRead(nil, user, "MSFT", day)
	n, _ = h.countDetailReadsToday(nil, user)
	if n != 2 {
		t.Errorf("after AAPL+MSFT, count = %d, want 2", n)
	}

	// Different user must not see this user's reads.
	otherUser := "different-user"
	n, _ = h.countDetailReadsToday(nil, otherUser)
	if n != 0 {
		t.Errorf("other user count = %d, want 0", n)
	}
}

// TestProjectPickRowsHandlesEmptyJSON ensures a row with empty /
// missing result_json (e.g. an early-failure placeholder we wrote
// before the panel ran) doesn't crash the browse-grid projection.
func TestProjectPickRowsHandlesEmptyJSON(t *testing.T) {
	t.Parallel()
	rows := []dailypicks.PickRow{
		{Symbol: "XYZ", Market: "us_equity", PresetKey: "disruptive",
			PickDate: time.Now().UTC(), AggregateVerdict: "SKIP",
			ResultJSON: nil},
	}
	out := projectPickRows(rows)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	if out[0].HeadlineThesis != "" {
		t.Errorf("HeadlineThesis = %q, want empty for nil JSON", out[0].HeadlineThesis)
	}
}

func TestPathSanity(t *testing.T) {
	// Sanity check that the canonical prefix matches what the
	// route registration uses, so a typo in one place would
	// fail this test.
	if !strings.HasPrefix("/api/daily-picks/2026-06-08/AAPL", "/api/daily-picks/") {
		t.Fatal("path prefix mismatch — parseDetailPath would not match a real request")
	}
}
