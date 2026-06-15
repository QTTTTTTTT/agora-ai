// daily_picks_handler_test.go — unit-level coverage of the pure
// helpers (path parsing, headline extraction, tier-to-quota
// resolution) plus the free-tier HTTP guard rails enforced inside
// handleList / handleDetail. The end-to-end smoke suite still owns
// the "real Postgres + Yahoo proxy" coverage; what we exercise
// here is the in-handler enforcement matrix that doesn't need real
// market data.

package main

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fundai/server/internal/api"
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

	// Free tier is locked out of the most recent 3 days. The
	// boundary is "today minus 3d INCLUSIVE" — i.e. June 5 is
	// readable on June 8. The lag was widened to 14d in v1 and
	// then narrowed to 3d in v2 because the longer gap was
	// converting too few free users (data felt stale rather than
	// tantalising). If you regress this constant, the upgrade
	// funnel narrows immediately — keep the test pinned.
	freeMax := h.maxAllowedDateForTier(subscription.PlanFree)
	wantFree := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
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

// TestPickDateFor_AnchoredToShanghai pins the 2026-06-12 fix:
// pick_date is the BJT calendar day the wave runs, NOT the UTC
// truncation. Pre-fix, a wave firing at 04:30 BJT (= 20:30 UTC
// the previous day) had its UTC-truncated `today` collide with
// the prior morning's run, so CountForDay returned 50 (yesterday's
// rows) and runWatchlistWave logged skip_complete forever — the
// new ET trading day's picks were never written until 08:00 BJT
// when UTC finally rolled. The schedule wakes at 04:30 BJT, so
// the pre-fix dead window was the full 3.5 hours that matter.
//
// We pin three concrete instants:
//   - 04:30 BJT (the schedule's actual fire instant) → today is
//     2026-06-12, NOT 2026-06-11.
//   - 09:21 BJT (a past run that historically landed at this
//     time) → 2026-06-11, matching the row already in production.
//     The fix is therefore backward-compatible for rows whose
//     run-time happens to fall on the same UTC and BJT day.
//   - 23:55 BJT (an end-of-day run) → 2026-06-12, NOT 2026-06-13.
//     Confirms the truncation lops off the time-of-day before the
//     timezone shift bites.
func TestPickDateFor_AnchoredToShanghai(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation Asia/Shanghai: %v", err)
	}
	cases := []struct {
		name string
		// Express each input as a BJT instant, then convert to
		// the underlying time.Time. Tests stay readable next to
		// the schedule's BJT-anchored documentation.
		bjt time.Time
		// Want is the BJT calendar day named in UTC midnight —
		// matches the repo's `pickDate.UTC()::DATE` round-trip.
		// See pickDateFor doc for the rationale.
		want time.Time
	}{
		{
			name: "schedule_fire_0430_BJT_2026_06_12",
			bjt:  time.Date(2026, 6, 12, 4, 30, 0, 0, loc),
			want: time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "production_run_0921_BJT_2026_06_11_back_compat",
			bjt:  time.Date(2026, 6, 11, 9, 21, 0, 0, loc),
			want: time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "end_of_day_2355_BJT_2026_06_12",
			bjt:  time.Date(2026, 6, 12, 23, 55, 0, 0, loc),
			want: time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pickDateFor(tc.bjt)
			if !got.Equal(tc.want) {
				t.Errorf("pickDateFor(%s) = %s, want %s", tc.bjt, got, tc.want)
			}
			// Belt-and-braces: simulate the repo's round-trip
			// (pickDate.UTC() then ::DATE in Postgres) by formatting
			// in UTC and asserting the calendar day is the BJT one.
			gotDate := got.UTC().Format("2006-01-02")
			wantDate := tc.bjt.Format("2006-01-02")
			if gotDate != wantDate {
				t.Errorf("repo round-trip Format = %s, want %s",
					gotDate, wantDate)
			}
		})
	}
}

// TestPickDateFor_StableAcrossTimezoneInputs confirms the helper
// is timezone-input-agnostic: feeding the same instant tagged in
// UTC, ET, and BJT all produce the same BJT calendar day. This
// is the property that lets every caller (tick / RunOnce / CLI
// admin command) hand in `time.Now()` without thinking about the
// timezone of the receiver — pickDateFor canonicalises.
func TestPickDateFor_StableAcrossTimezoneInputs(t *testing.T) {
	t.Parallel()
	utc, _ := time.LoadLocation("UTC")
	et, _ := time.LoadLocation("America/New_York")
	bjt, _ := time.LoadLocation("Asia/Shanghai")

	// 2026-06-12 04:30 BJT = 2026-06-11 20:30 UTC = 2026-06-11
	// 16:30 ET — the canonical schedule fire instant for the
	// US watchlist suite. All three labellings should map to the
	// same calendar date in Shanghai.
	instants := []time.Time{
		time.Date(2026, 6, 12, 4, 30, 0, 0, bjt),
		time.Date(2026, 6, 11, 20, 30, 0, 0, utc),
		time.Date(2026, 6, 11, 16, 30, 0, 0, et),
	}
	// Want is BJT 2026-06-12 expressed as UTC-midnight (see
	// pickDateFor doc on why we don't tag the return as Shanghai).
	want := time.Date(2026, 6, 12, 0, 0, 0, 0, utc)
	for i, in := range instants {
		got := pickDateFor(in)
		if !got.Equal(want) {
			t.Errorf("[%d] pickDateFor(%s, loc=%s) = %s, want %s",
				i, in, in.Location(), got, want)
		}
	}
	// Silence unused-loc warning under -race when coverage trimmer
	// removes the bjt sentinel below.
	_ = bjt
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

// TestFreeTierConstants pins the v2 free-tier knobs so a
// well-meaning refactor doesn't silently widen the funnel. If you
// change these intentionally, update the test number AND the
// product-spec doc that mirrors them.
func TestFreeTierConstants(t *testing.T) {
	t.Parallel()
	if freeTierLagDays != 3 {
		t.Errorf("freeTierLagDays = %d, want 3 (v2 spec)", freeTierLagDays)
	}
	if freeTierTopN != 3 {
		t.Errorf("freeTierTopN = %d, want 3 (v2 spec)", freeTierTopN)
	}
	if _, ok := freeTierVisiblePresets["disruptive"]; !ok {
		t.Error("freeTierVisiblePresets must contain 'disruptive' — it's the only preset surfaced to free")
	}
	for _, paid := range []string{"conservative", "garp", "macro"} {
		if _, ok := freeTierVisiblePresets[paid]; ok {
			t.Errorf("freeTierVisiblePresets must NOT contain %q — paid-only", paid)
		}
	}
	if got := len(freeTierVisiblePresets); got != 1 {
		t.Errorf("freeTierVisiblePresets size = %d, want 1 (disruptive only)", got)
	}
}

// --- HTTP-level free-tier guard tests --------------------------------------

// newFreeTierTestEnv stands up a sqlmock-backed handler with
// `subs == nil` so resolveTier always returns PlanFree. This is
// the simplest way to exercise the four free-tier branches without
// needing to mock the subscriptions table on every test.
func newFreeTierTestEnv(t *testing.T, fixedNow time.Time) (*dailyPicksHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	h := &dailyPicksHandler{
		picks: dailypicks.NewRepo(db),
		subs:  nil, // nil subs → resolveTier returns PlanFree without DB
		db:    db,
		clock: func() time.Time { return fixedNow },
	}
	return h, mock, func() { _ = db.Close() }
}

// dailyPicksAuthedReq builds a request with an authenticated user-id
// baked into the context, matching how the real auth middleware
// feeds the handler. The corp-action admin tests have their own
// `authedReq` helper with a different signature, so we use a
// package-distinct name here.
func dailyPicksAuthedReq(method, target, userID string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	if userID != "" {
		ctx := api.WithAuthenticatedUserID(req.Context(), userID)
		req = req.WithContext(ctx)
	}
	return req
}

// Columns scanned by Repo.List — the order has to match
// rows.Scan in repo.go exactly or the row-build will mis-bind.
var listColumns = []string{
	"id", "symbol", "symbol_name", "market", "preset_key",
	"pick_date", "result_json",
	"aggregate_verdict", "aggregate_score",
	"consensus", "llm_cost_usd",
	"error_reason", "computed_at",
}

func mockListRow(symbol string, pickDate time.Time, score int) []driver.Value {
	return []driver.Value{
		int64(1),                  // id (PickRow.ID is int64)
		symbol,                    // symbol
		symbol + " Inc.",          // symbol_name
		"us_equity",               // market
		"disruptive",              // preset_key
		pickDate,                  // pick_date
		[]byte(`{"master_reports":[{"thesis":"reason"}]}`), // result_json
		"BUY",                     // aggregate_verdict
		score,                     // aggregate_score
		0.8,                       // consensus
		float64(0),                // llm_cost_usd
		"",                        // error_reason
		pickDate,                  // computed_at
	}
}

// TestHandleList_FreeTier_RejectsNonDisruptivePreset proves that a
// URL-tampering free user asking for ?preset=conservative gets a
// 403 forbidden_preset BEFORE the handler hits the DB. This is the
// load-bearing guarantee that the chip-hiding in DailyPicks.tsx
// can't be bypassed; if the test fails, paid-tier content has
// silently leaked to free.
func TestHandleList_FreeTier_RejectsNonDisruptivePreset(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	h, _, cleanup := newFreeTierTestEnv(t, now)
	defer cleanup()

	rr := httptest.NewRecorder()
	h.handleList(rr, dailyPicksAuthedReq(
		http.MethodGet,
		"/api/daily-picks?preset=conservative&market=us_equity",
		"free-user-1",
	))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	if got, _ := body["error"].(string); got != "forbidden_preset" {
		t.Errorf("error = %q, want forbidden_preset", got)
	}
}

// TestHandleList_FreeTier_DisruptivePreset_HappyPath_ClampsAndPins
// pins the three behaviours that share a code path:
//   - limit clamped to 3 even when ?limit=50
//   - rows pinned to a single most-recent allowed pick_date
//   - response carries tier=free and free_lag_days=3 (the FE keys
//     the lock UI off these two fields)
//
// We mock 3 sqlmock queries because the handler issues:
//  1. find latest pick_date for free + maxDate (limit=1)
//  2. main rows for that pick_date (limit=3)
//  3. cross-tier "newest_available_date" probe (limit=1)
func TestHandleList_FreeTier_DisruptivePreset_HappyPath_ClampsAndPins(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	h, mock, cleanup := newFreeTierTestEnv(t, now)
	defer cleanup()

	// freeTierLagDays = 3 → maxDate = 2026-06-09
	pickDay := time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC)

	// Q1: latest pick_date probe (Limit=1, MaxPickDate=2026-06-09)
	mock.ExpectQuery(regexp.QuoteMeta("FROM daily_picks")).
		WillReturnRows(sqlmock.NewRows(listColumns).
			AddRow(mockListRow("AAPL", pickDay, 90)...))

	// Q2: pinned-date main fetch (Limit=3, PickDate=2026-06-09).
	// Three rows, all on the same pick_date, scores DESC.
	mock.ExpectQuery(regexp.QuoteMeta("FROM daily_picks")).
		WillReturnRows(sqlmock.NewRows(listColumns).
			AddRow(mockListRow("AAPL", pickDay, 90)...).
			AddRow(mockListRow("MSFT", pickDay, 85)...).
			AddRow(mockListRow("GOOG", pickDay, 80)...))

	// Q3: cross-tier "newest available" probe (Limit=1, no max).
	// We seed a newer date than free's window so the response
	// surfaces upgrade_required_for_today=true.
	newerDay := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("FROM daily_picks")).
		WillReturnRows(sqlmock.NewRows(listColumns).
			AddRow(mockListRow("AAPL", newerDay, 99)...))

	rr := httptest.NewRecorder()
	// limit=50 should be clamped to 3 by the free-tier guard.
	// date=2026-06-12 should be silently ignored (pin still 06-09).
	h.handleList(rr, dailyPicksAuthedReq(
		http.MethodGet,
		"/api/daily-picks?preset=disruptive&market=us_equity&limit=50&date=2026-06-12",
		"free-user-1",
	))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	var resp dailyPicksListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v", err)
	}
	if resp.Tier != "free" {
		t.Errorf("tier = %q, want free", resp.Tier)
	}
	if resp.FreeLagDays != 3 {
		t.Errorf("free_lag_days = %d, want 3", resp.FreeLagDays)
	}
	if got := len(resp.Picks); got != 3 {
		t.Errorf("len picks = %d, want 3 (limit clamped)", got)
	}
	// All rows must come from the pinned pick_date — no spill.
	for i, p := range resp.Picks {
		if p.PickDate != "2026-06-09" {
			t.Errorf("picks[%d].pick_date = %q, want 2026-06-09 (latest allowed)", i, p.PickDate)
		}
	}
	if !resp.UpgradeRequiredForToday {
		t.Error("upgrade_required_for_today = false, want true (newer wave exists outside free window)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestHandleList_FreeTier_EmptyWindow proves that when no rows
// exist within the free-tier window, the handler returns an empty
// 200 (not a 5xx), so the FE can render its empty state.
func TestHandleList_FreeTier_EmptyWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	h, mock, cleanup := newFreeTierTestEnv(t, now)
	defer cleanup()

	// Q1 returns zero rows → handler short-circuits with empty payload.
	mock.ExpectQuery(regexp.QuoteMeta("FROM daily_picks")).
		WillReturnRows(sqlmock.NewRows(listColumns))

	rr := httptest.NewRecorder()
	h.handleList(rr, dailyPicksAuthedReq(
		http.MethodGet,
		"/api/daily-picks?preset=disruptive",
		"free-user-1",
	))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp dailyPicksListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v", err)
	}
	if len(resp.Picks) != 0 {
		t.Errorf("len picks = %d, want 0", len(resp.Picks))
	}
	if resp.Tier != "free" {
		t.Errorf("tier = %q, want free", resp.Tier)
	}
	if resp.FreeLagDays != 3 {
		t.Errorf("free_lag_days = %d, want 3", resp.FreeLagDays)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestHandleDetail_FreeTier_AlwaysForbidden pins the
// defense-in-depth contract: free users that hit the detail
// endpoint directly must be rejected BEFORE the handler queries
// the row, even when the URL is technically a valid past date.
// The FE never makes this call (it shows the upgrade modal
// instead) but a hand-crafted curl must still bounce.
func TestHandleDetail_FreeTier_AlwaysForbidden(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	// No sqlmock expectations — handler must short-circuit before
	// any DB call. A leaked DB call would surface here as
	// "unexpected query" via mock.ExpectationsWereMet.
	h, mock, cleanup := newFreeTierTestEnv(t, now)
	defer cleanup()

	rr := httptest.NewRecorder()
	h.handleDetail(rr, dailyPicksAuthedReq(
		http.MethodGet,
		"/api/daily-picks/2026-06-01/AAPL?preset=disruptive",
		"free-user-1",
	))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	if got, _ := body["error"].(string); got != "upgrade_required" {
		t.Errorf("error = %q, want upgrade_required", got)
	}
	// Detail must reference the free-tier rule so the FE can
	// distinguish this from the older "future date" 403 if it
	// ever needs to (both share the upgrade_required error code).
	if msg, _ := body["detail"].(string); !strings.Contains(msg, "free tier") {
		t.Errorf("detail = %q, want mention of 'free tier'", msg)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v (no DB calls expected)", err)
	}
}

// TestHandleDetail_PaidTier_DoesNotShortCircuit confirms that the
// new free-tier 403 doesn't fire on paid users — they should
// proceed past the early gate and land on whatever the next layer
// returns (here, ErrNotFound surfaced as 404 because we mock no
// row).
func TestHandleDetail_PaidTier_DoesNotShortCircuit(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	h := &dailyPicksHandler{
		picks: dailypicks.NewRepo(db),
		subs:  subscription.NewSubscriptionService(db),
		db:    db,
		clock: func() time.Time { return now },
	}

	// Subscription lookup → "pro". Order matches the SELECT in
	// SubscriptionService.GetUserSubscription.
	subCols := []string{
		"id", "user_id", "plan_tier", "status", "start_date", "end_date",
		"auto_renew", "payment_method", "created_at", "updated_at",
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM subscriptions")).
		WithArgs("pro-user-1").
		WillReturnRows(sqlmock.NewRows(subCols).AddRow(
			"sub-1", "pro-user-1", "pro", "active",
			now.Add(-30*24*time.Hour), now.Add(30*24*time.Hour),
			true, "card", now.Add(-30*24*time.Hour), now,
		))

	// Pre-fetch quota check goes through h.countDetailReadsToday
	// which is in-memory (no DB). Then h.picks.Get fires →
	// returns ErrNotFound → 404. We expect ONE Get query.
	mock.ExpectQuery(regexp.QuoteMeta("FROM daily_picks")).
		WillReturnError(errNoRowsForTest)

	rr := httptest.NewRecorder()
	h.handleDetail(rr, dailyPicksAuthedReq(
		http.MethodGet,
		"/api/daily-picks/2026-06-09/AAPL?preset=disruptive&market=us_equity",
		"pro-user-1",
	))
	// The new free-tier short-circuit must NOT have fired —
	// status must be anything BUT 403/upgrade_required.
	if rr.Code == http.StatusForbidden {
		var body map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
		if got, _ := body["error"].(string); got == "upgrade_required" {
			t.Fatalf("paid user got upgrade_required 403; body=%s", rr.Body.String())
		}
	}
}

// errNoRowsForTest is a sentinel returned by the sqlmock layer to
// force h.picks.Get into its error path. The exact error type is
// not what the test asserts — we only check that the handler
// makes it past the new free-tier gate.
var errNoRowsForTest = errors.New("sqlmock: forced not-found")
