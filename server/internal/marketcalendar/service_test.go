package marketcalendar

import (
	"testing"
	"time"
)

func TestNormalizeProfileInfersCalendarAndTimezone(t *testing.T) {
	svc := NewService()
	profile, err := svc.NormalizeProfile(Profile{Market: "us_equity", Exchange: "NASDAQ", AssetClass: "equity"})
	if err != nil {
		t.Fatalf("normalize profile: %v", err)
	}
	if profile.CalendarCode != "US-XNAS" {
		t.Fatalf("expected calendar code US-XNAS, got %q", profile.CalendarCode)
	}
	if profile.TimeZone != "America/New_York" {
		t.Fatalf("expected timezone America/New_York, got %q", profile.TimeZone)
	}
}

// TestNormalizeProfileEmptyInputDefaultsToUSCalendarWithNYTimezone locks in
// F11.1's secondary fix: when the caller provides no signals at all, the
// catch-all default is US-XNAS — but the resulting timezone must be
// America/New_York (the calendar's native zone) rather than UTC. Without
// this, scheduler logs were misleading and trading windows were computed
// against the wrong wall-clock.
func TestNormalizeProfileEmptyInputDefaultsToUSCalendarWithNYTimezone(t *testing.T) {
	svc := NewService()
	profile, err := svc.NormalizeProfile(Profile{})
	if err != nil {
		t.Fatalf("normalize empty profile: %v", err)
	}
	if profile.CalendarCode != "US-XNAS" {
		t.Fatalf("expected calendar code US-XNAS, got %q", profile.CalendarCode)
	}
	if profile.TimeZone != "America/New_York" {
		t.Fatalf("expected timezone America/New_York, got %q (must not stay UTC)", profile.TimeZone)
	}
}

// TestNormalizeProfileInfersCryptoFromMarket locks in F11.1's primary
// contract: a fund tagged with market=crypto gets 24x7 calendar + UTC.
func TestNormalizeProfileInfersCryptoFromMarket(t *testing.T) {
	svc := NewService()
	profile, err := svc.NormalizeProfile(Profile{Market: "crypto"})
	if err != nil {
		t.Fatalf("normalize crypto profile: %v", err)
	}
	if profile.CalendarCode != "CRYPTO-24X7" {
		t.Fatalf("expected CRYPTO-24X7, got %q", profile.CalendarCode)
	}
	if profile.TimeZone != "UTC" {
		t.Fatalf("expected UTC, got %q", profile.TimeZone)
	}
}

// TestNormalizeProfileInfersCryptoFromAssetClass covers the case where
// the API caller only sets assetClass (not market) — common when wiring
// the UI's asset-class dropdown without a separate market field.
func TestNormalizeProfileInfersCryptoFromAssetClass(t *testing.T) {
	svc := NewService()
	profile, err := svc.NormalizeProfile(Profile{AssetClass: "crypto"})
	if err != nil {
		t.Fatalf("normalize crypto profile: %v", err)
	}
	if profile.CalendarCode != "CRYPTO-24X7" {
		t.Fatalf("expected CRYPTO-24X7, got %q", profile.CalendarCode)
	}
	if profile.TimeZone != "UTC" {
		t.Fatalf("expected UTC, got %q", profile.TimeZone)
	}
}

// TestNormalizeProfileMarketAliases pins the alias-to-canonical mapping for
// the market field. The /api/companies/{id}/funds endpoint accepts these
// alternate spellings ("cn_a_share", "us_stock", "cryptocurrency", ...)
// from frontends; without normalising them here NormalizeProfile would skip
// every market-based case and fall through to the catch-all US-XNAS, so
// every newly-created A-share/crypto/futures fund would silently get the
// wrong calendar + timezone.
func TestNormalizeProfileMarketAliases(t *testing.T) {
	svc := NewService()
	cases := []struct {
		input         string
		wantMarket    string
		wantCalendar  string
		wantTimeZone  string
	}{
		{"cn_a_share", "a_share", "CN-SSE", "Asia/Shanghai"},
		{"china_a_share", "a_share", "CN-SSE", "Asia/Shanghai"},
		{"us_stock", "us_equity", "US-XNAS", "America/New_York"},
		{"cryptocurrency", "crypto", "CRYPTO-24X7", "UTC"},
		{"future", "futures", "CME-INDEX", "America/Chicago"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			profile, err := svc.NormalizeProfile(Profile{Market: tc.input})
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if profile.Market != tc.wantMarket {
				t.Errorf("market: want %q got %q", tc.wantMarket, profile.Market)
			}
			if profile.CalendarCode != tc.wantCalendar {
				t.Errorf("calendar: want %q got %q", tc.wantCalendar, profile.CalendarCode)
			}
			if profile.TimeZone != tc.wantTimeZone {
				t.Errorf("tz: want %q got %q", tc.wantTimeZone, profile.TimeZone)
			}
		})
	}
}

// TestNormalizeProfileInfersCryptoFromExchange covers exchange-only input
// — the common case for retail UIs that ask "which exchange?" first.
func TestNormalizeProfileInfersCryptoFromExchange(t *testing.T) {
	svc := NewService()
	for _, exchange := range []string{"BINANCE", "COINBASE", "OKX", "BYBIT"} {
		t.Run(exchange, func(t *testing.T) {
			profile, err := svc.NormalizeProfile(Profile{Exchange: exchange})
			if err != nil {
				t.Fatalf("normalize crypto profile: %v", err)
			}
			if profile.Market != "crypto" {
				t.Fatalf("expected market crypto, got %q", profile.Market)
			}
			if profile.CalendarCode != "CRYPTO-24X7" {
				t.Fatalf("expected CRYPTO-24X7, got %q", profile.CalendarCode)
			}
			if profile.TimeZone != "UTC" {
				t.Fatalf("expected UTC, got %q", profile.TimeZone)
			}
		})
	}
}

func TestResolveTradingDateRejectsNonTradingDayForAShares(t *testing.T) {
	svc := NewService()
	now := time.Date(2026, time.May, 16, 10, 0, 0, 0, time.UTC)
	_, err := svc.ResolveTradingDate(now, Profile{CalendarCode: "CN-SSE", TimeZone: "Asia/Shanghai"}, ResolutionCurrentTradingDay)
	if err == nil {
		t.Fatal("expected non-trading day error")
	}
}

func TestResolveTradingDateLatestSkipsChinaHolidayWindow(t *testing.T) {
	svc := NewService()
	now := time.Date(2026, time.May, 4, 2, 0, 0, 0, time.UTC)
	tradingDate, err := svc.ResolveTradingDate(now, Profile{CalendarCode: "CN-SSE", TimeZone: "Asia/Shanghai"}, ResolutionLatestTradingDay)
	if err != nil {
		t.Fatalf("resolve latest trading date: %v", err)
	}
	if got := tradingDate.Format("2006-01-02"); got != "2026-04-30" {
		t.Fatalf("expected latest trading date 2026-04-30, got %s", got)
	}
}

func TestResolveTradingDateUsesCryptoTimezoneRollover(t *testing.T) {
	svc := NewService()
	now := time.Date(2026, time.May, 13, 16, 30, 0, 0, time.UTC)
	tradingDate, err := svc.ResolveTradingDate(now, Profile{CalendarCode: "CRYPTO-24X7", TimeZone: "Asia/Shanghai"}, ResolutionCurrentTradingDay)
	if err != nil {
		t.Fatalf("resolve crypto trading date: %v", err)
	}
	if got := tradingDate.Format("2006-01-02"); got != "2026-05-14" {
		t.Fatalf("expected crypto trading date 2026-05-14, got %s", got)
	}
}

func TestSessionForDateMarksUSHalfDay(t *testing.T) {
	svc := NewService()
	tradingDate := time.Date(2026, time.November, 27, 0, 0, 0, 0, time.UTC)
	session, err := svc.SessionForDate(tradingDate, Profile{CalendarCode: "US-XNAS", TimeZone: "America/New_York"})
	if err != nil {
		t.Fatalf("session for date: %v", err)
	}
	if !session.IsHalfDay {
		t.Fatal("expected half day session")
	}
	if got := session.CloseTime.Format("15:04"); got != "13:00" {
		t.Fatalf("expected close time 13:00, got %s", got)
	}
}

func TestSessionForDateMarksUSHalfDayWhenJulyFourthObservedOnMonday(t *testing.T) {
	svc := NewService()
	tradingDate := time.Date(2027, time.July, 2, 0, 0, 0, 0, time.UTC)
	session, err := svc.SessionForDate(tradingDate, Profile{CalendarCode: "US-XNAS", TimeZone: "America/New_York"})
	if err != nil {
		t.Fatalf("session for date: %v", err)
	}
	if !session.IsHalfDay {
		t.Fatal("expected half day session")
	}
	if got := session.CloseTime.Format("15:04"); got != "13:00" {
		t.Fatalf("expected close time 13:00, got %s", got)
	}
}

func TestSessionForDateDoesNotMarkUSHalfDayWhenJulyFourthObservedOnFriday(t *testing.T) {
	svc := NewService()
	tradingDate := time.Date(2026, time.July, 2, 0, 0, 0, 0, time.UTC)
	session, err := svc.SessionForDate(tradingDate, Profile{CalendarCode: "US-XNAS", TimeZone: "America/New_York"})
	if err != nil {
		t.Fatalf("session for date: %v", err)
	}
	if session.IsHalfDay {
		t.Fatal("expected regular session")
	}
	if got := session.CloseTime.Format("15:04"); got != "16:00" {
		t.Fatalf("expected close time 16:00, got %s", got)
	}
}

func TestSessionForDateMarksUSHalfDayOnChristmasEveWhenMarketIsOpen(t *testing.T) {
	svc := NewService()
	tradingDate := time.Date(2026, time.December, 24, 0, 0, 0, 0, time.UTC)
	session, err := svc.SessionForDate(tradingDate, Profile{CalendarCode: "US-XNAS", TimeZone: "America/New_York"})
	if err != nil {
		t.Fatalf("session for date: %v", err)
	}
	if !session.IsHalfDay {
		t.Fatal("expected half day session")
	}
	if got := session.CloseTime.Format("15:04"); got != "13:00" {
		t.Fatalf("expected close time 13:00, got %s", got)
	}
}

func TestBuildStepScheduleUsesTradeExecutionAnchor(t *testing.T) {
	svc := NewService()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	session := &TradingSession{
		TradingDate:  "2026-05-14",
		Location:     loc,
		IsTradingDay: true,
		PreOpenTime:  time.Date(2026, time.May, 14, 9, 0, 0, 0, loc),
		OpenTime:     time.Date(2026, time.May, 14, 9, 30, 0, 0, loc),
		CloseTime:    time.Date(2026, time.May, 14, 16, 0, 0, 0, loc),
	}
	schedule := svc.BuildStepSchedule(session)
	if got := schedule.TradeExecution.Format("15:04"); got != "11:00" {
		t.Fatalf("expected trade execution at 11:00, got %s", got)
	}
	if got := schedule.Settlement.Format("15:04"); got != "16:05" {
		t.Fatalf("expected settlement at 16:05, got %s", got)
	}
}

// TestTradingTriggerSlotsCNSplitsAroundMiddayBreak documents the
// canonical user example: A-share + 30-minute interval should produce
// 5 morning + 4 afternoon slots. The morning window MERGES pre-open
// (9:00) with the regular morning session (9:30 → 11:30) so the
// auction is reachable; the afternoon window is its own block (no
// trigger at 11:30 = morning close and no trigger at 15:00 = day
// close — both ends are exclusive).
func TestTradingTriggerSlotsCNSplitsAroundMiddayBreak(t *testing.T) {
	svc := NewService()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	session, err := svc.SessionForDate(time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC), Profile{CalendarCode: "CN-SSE", TimeZone: "Asia/Shanghai"})
	if err != nil {
		t.Fatalf("session for date: %v", err)
	}
	if loc != session.Location {
		// sanity check
		t.Logf("note: session loc=%s requested=%s", session.Location, loc)
	}
	slots := svc.TradingTriggerSlots(session, 30)
	want := []string{
		"09:00", "09:30", "10:00", "10:30", "11:00", // morning (incl. pre-open auction window)
		"13:00", "13:30", "14:00", "14:30",          // afternoon
	}
	if len(slots) != len(want) {
		t.Fatalf("slot count = %d, want %d (slots=%v)", len(slots), len(want), formatSlots(slots))
	}
	for i, s := range slots {
		if got := s.Format("15:04"); got != want[i] {
			t.Fatalf("slot[%d] = %s, want %s", i, got, want[i])
		}
	}
}

func formatSlots(slots []time.Time) []string {
	out := make([]string, len(slots))
	for i, s := range slots {
		out[i] = s.Format("15:04")
	}
	return out
}

// TestTradingTriggerSlotsUSContiguousDay covers the single-segment case
// (US equities): no midday break, so pre-open + regular session merge
// into one big window and we expect a slot every interval from 9:00
// until just before 16:00.
func TestTradingTriggerSlotsUSContiguousDay(t *testing.T) {
	svc := NewService()
	session, err := svc.SessionForDate(time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC), Profile{CalendarCode: "US-XNAS", TimeZone: "America/New_York"})
	if err != nil {
		t.Fatalf("session for date: %v", err)
	}
	slots := svc.TradingTriggerSlots(session, 60)
	want := []string{"09:00", "10:00", "11:00", "12:00", "13:00", "14:00", "15:00"}
	if len(slots) != len(want) {
		t.Fatalf("slot count = %d, want %d (slots=%v)", len(slots), len(want), formatSlots(slots))
	}
	for i, s := range slots {
		if got := s.Format("15:04"); got != want[i] {
			t.Fatalf("slot[%d] = %s, want %s", i, got, want[i])
		}
	}
}

// TestTradingTriggerSlotsClampedInterval verifies the safety net for
// fat-finger inputs — values below 5 min and above 12h get folded into
// the supported range rather than silently producing a hot loop or a
// single trigger per day.
func TestTradingTriggerSlotsClampedInterval(t *testing.T) {
	svc := NewService()
	session, err := svc.SessionForDate(time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC), Profile{CalendarCode: "US-XNAS", TimeZone: "America/New_York"})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	// Asking for 1 min should be clamped UP to the minimum (5 min).
	if got := len(svc.TradingTriggerSlots(session, 1)); got != len(svc.TradingTriggerSlots(session, MinDecisionIntervalMinutes)) {
		t.Fatalf("interval=1 should clamp to %d, got %d slots", MinDecisionIntervalMinutes, got)
	}
	// Asking for a year (525600 min) should be clamped DOWN to 12h —
	// the window is 7h so we'd see exactly one slot (the window open).
	if got := len(svc.TradingTriggerSlots(session, 525600)); got != 1 {
		t.Fatalf("interval=525600 should clamp to 12h, expected 1 slot in the day, got %d", got)
	}
}

// TestNextTriggerSlotToleratesSubSecondTimerDrift is the regression
// test for the production bug surfaced on 2026-05-21: tong's OCS
// fund had a 30-min interval and a 14:30 slot. The scheduler timer
// woke at 14:30:00.018 — eighteen milliseconds late — and the old
// `!slot.Before(now)` check rejected the 14:30 slot for being
// "in the past". With no later slots in the afternoon window
// (15:00 isn't in the half-open [13:00, 15:00) enumeration),
// NextWorkflowStart rolled the trigger over to the next trading
// day, and from then on the fund's nextTriggerAt was dominated by
// other funds' calendars. The 30s grace window absorbs that drift.
func TestNextTriggerSlotToleratesSubSecondTimerDrift(t *testing.T) {
	svc := NewService()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}
	session, err := svc.SessionForDate(time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC), Profile{CalendarCode: "CN-SSE", TimeZone: "Asia/Shanghai"})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	slot1430 := time.Date(2026, 5, 21, 14, 30, 0, 0, loc)
	// 18ms drift — the exact gap observed in production.
	drifted := slot1430.Add(18 * time.Millisecond)
	got, ok := svc.NextTriggerSlot(session, 30, drifted)
	if !ok {
		t.Fatal("expected to find 14:30 slot despite 18ms drift, got !ok")
	}
	if !got.Equal(slot1430) {
		t.Fatalf("expected 14:30 slot, got %s", got.Format(time.RFC3339Nano))
	}
}

// TestNextTriggerSlotRejectsSlotsBeyondGrace ensures the lenient
// behaviour does NOT let a slot fire much later than its window:
// a 90s drift past the slot is too far and the next slot wins.
func TestNextTriggerSlotRejectsSlotsBeyondGrace(t *testing.T) {
	svc := NewService()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}
	session, err := svc.SessionForDate(time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC), Profile{CalendarCode: "CN-SSE", TimeZone: "Asia/Shanghai"})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	slot1400 := time.Date(2026, 5, 21, 14, 0, 0, 0, loc)
	slot1430 := time.Date(2026, 5, 21, 14, 30, 0, 0, loc)
	got, ok := svc.NextTriggerSlot(session, 30, slot1400.Add(90*time.Second))
	if !ok {
		t.Fatal("expected next slot, got !ok")
	}
	if !got.Equal(slot1430) {
		t.Fatalf("90s past 14:00 should advance to 14:30, got %s", got.Format(time.RFC3339))
	}
}

// TestNextTriggerSlotNoMoreSlotsToday confirms a `now` past the
// last slot of the day STILL returns ok=false — the day-rollover
// behaviour the scheduler loop relies on must survive the grace.
func TestNextTriggerSlotNoMoreSlotsToday(t *testing.T) {
	svc := NewService()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}
	session, err := svc.SessionForDate(time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC), Profile{CalendarCode: "CN-SSE", TimeZone: "Asia/Shanghai"})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	pastClose := time.Date(2026, 5, 21, 15, 30, 0, 0, loc) // half hour past 15:00 close
	if _, ok := svc.NextTriggerSlot(session, 30, pastClose); ok {
		t.Fatal("past last slot should not be ok=true")
	}
}

// TestNextWorkflowStartHonoursIntervalCN walks the scheduler's view
// of a fund with a 30-minute interval on the SSE calendar: at any
// instant during the trading day, the next workflow trigger should
// be the next slot >= now.
func TestNextWorkflowStartHonoursIntervalCN(t *testing.T) {
	svc := NewService()
	interval := 30
	profile := Profile{
		CalendarCode:            "CN-SSE",
		TimeZone:                "Asia/Shanghai",
		DecisionIntervalMinutes: &interval,
	}
	cases := []struct {
		name     string
		nowLocal time.Time
		wantSlot string
	}{
		// Before the day starts → first slot of the day.
		{name: "before-open", nowLocal: time.Date(2026, time.May, 14, 7, 0, 0, 0, mustLoc(t)), wantSlot: "09:00"},
		// Between 9:00 and 9:30 → next slot is 9:30.
		{name: "during-auction", nowLocal: time.Date(2026, time.May, 14, 9, 15, 0, 0, mustLoc(t)), wantSlot: "09:30"},
		// Right on a slot → that slot.
		{name: "on-slot", nowLocal: time.Date(2026, time.May, 14, 10, 30, 0, 0, mustLoc(t)), wantSlot: "10:30"},
		// During midday break → next slot is 13:00 (afternoon open).
		{name: "midday-break", nowLocal: time.Date(2026, time.May, 14, 12, 0, 0, 0, mustLoc(t)), wantSlot: "13:00"},
		// After last afternoon slot but before close → roll to next day.
		{name: "after-last-slot", nowLocal: time.Date(2026, time.May, 14, 14, 45, 0, 0, mustLoc(t)), wantSlot: "09:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			triggerAt, _, err := svc.NextWorkflowStart(tc.nowLocal.UTC(), profile)
			if err != nil {
				t.Fatalf("next workflow start: %v", err)
			}
			if got := triggerAt.In(mustLoc(t)).Format("15:04"); got != tc.wantSlot {
				t.Fatalf("trigger HH:MM = %s, want %s (full=%s)", got, tc.wantSlot, triggerAt.Format(time.RFC3339))
			}
		})
	}
}

func mustLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	return loc
}

func TestNextWorkflowStartUsesSameDayFutureMacroBrief(t *testing.T) {
	svc := NewService()
	now := time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)
	triggerAt, tradingDate, err := svc.NextWorkflowStart(now, Profile{CalendarCode: "CN-SSE", TimeZone: "Asia/Shanghai"})
	if err != nil {
		t.Fatalf("next workflow start: %v", err)
	}
	if got := tradingDate.Format("2006-01-02"); got != "2026-05-14" {
		t.Fatalf("expected trading date 2026-05-14, got %s", got)
	}
	if got := triggerAt.Format("2006-01-02 15:04"); got != "2026-05-14 08:30" {
		t.Fatalf("expected trigger 2026-05-14 08:30, got %s", got)
	}
}

func TestNextWorkflowStartReturnsImmediateCatchUpDuringActiveTradingDay(t *testing.T) {
	svc := NewService()
	now := time.Date(2026, time.May, 14, 2, 0, 0, 0, time.UTC)
	triggerAt, tradingDate, err := svc.NextWorkflowStart(now, Profile{CalendarCode: "CN-SSE", TimeZone: "Asia/Shanghai"})
	if err != nil {
		t.Fatalf("next workflow start: %v", err)
	}
	if got := tradingDate.Format("2006-01-02"); got != "2026-05-14" {
		t.Fatalf("expected trading date 2026-05-14, got %s", got)
	}
	if !triggerAt.Equal(now) {
		t.Fatalf("expected immediate catch-up trigger at %s, got %s", now.Format(time.RFC3339), triggerAt.Format(time.RFC3339))
	}
}

func TestNextWorkflowStartRollsToNextTradingDayAfterWindow(t *testing.T) {
	svc := NewService()
	now := time.Date(2026, time.May, 14, 10, 0, 0, 0, time.UTC)
	triggerAt, tradingDate, err := svc.NextWorkflowStart(now, Profile{CalendarCode: "CN-SSE", TimeZone: "Asia/Shanghai"})
	if err != nil {
		t.Fatalf("next workflow start: %v", err)
	}
	if got := tradingDate.Format("2006-01-02"); got != "2026-05-15" {
		t.Fatalf("expected next trading date 2026-05-15, got %s", got)
	}
	if got := triggerAt.Format("2006-01-02 15:04"); got != "2026-05-15 08:30" {
		t.Fatalf("expected trigger 2026-05-15 08:30, got %s", got)
	}
}

func TestNextWorkflowStartSkipsWeekend(t *testing.T) {
	svc := NewService()
	now := time.Date(2026, time.May, 16, 10, 0, 0, 0, time.UTC)
	triggerAt, tradingDate, err := svc.NextWorkflowStart(now, Profile{CalendarCode: "CN-SSE", TimeZone: "Asia/Shanghai"})
	if err != nil {
		t.Fatalf("next workflow start: %v", err)
	}
	if got := tradingDate.Format("2006-01-02"); got != "2026-05-18" {
		t.Fatalf("expected next trading date 2026-05-18, got %s", got)
	}
	if got := triggerAt.Format("2006-01-02 15:04"); got != "2026-05-18 08:30" {
		t.Fatalf("expected trigger 2026-05-18 08:30, got %s", got)
	}
}

func TestNormalizeProfileRewritesExchangeCalendarUTC(t *testing.T) {
	svc := NewService()
	profile, err := svc.NormalizeProfile(Profile{CalendarCode: "US-XNAS", TimeZone: "UTC"})
	if err != nil {
		t.Fatalf("normalize profile: %v", err)
	}
	if profile.TimeZone != "America/New_York" {
		t.Fatalf("expected timezone America/New_York, got %q", profile.TimeZone)
	}
}

func TestResolveTradingDateLatestSkipsChinaHolidayWindowIn2027(t *testing.T) {
	svc := NewService()
	now := time.Date(2027, time.October, 5, 2, 0, 0, 0, time.UTC)
	tradingDate, err := svc.ResolveTradingDate(now, Profile{CalendarCode: "CN-SSE", TimeZone: "Asia/Shanghai"}, ResolutionLatestTradingDay)
	if err != nil {
		t.Fatalf("resolve latest trading date: %v", err)
	}
	if got := tradingDate.Format("2006-01-02"); got != "2027-09-30" {
		t.Fatalf("expected latest trading date 2027-09-30, got %s", got)
	}
}

func TestResolveTradingDateErrorsWhenChinaHolidayCalendarYearUnavailable(t *testing.T) {
	svc := NewService()
	now := time.Date(2028, time.January, 3, 2, 0, 0, 0, time.UTC)
	_, err := svc.ResolveTradingDate(now, Profile{CalendarCode: "CN-SSE", TimeZone: "Asia/Shanghai"}, ResolutionCurrentTradingDay)
	if err == nil {
		t.Fatal("expected unsupported china calendar year error")
	}
	if got := err.Error(); got != "marketcalendar: china holiday calendar for 2028 is unavailable" {
		t.Fatalf("unexpected error: %s", got)
	}
}

func TestNextWorkflowStartErrorsWhenChinaHolidayCalendarYearUnavailable(t *testing.T) {
	svc := NewService()
	now := time.Date(2028, time.January, 3, 0, 0, 0, 0, time.UTC)
	_, _, err := svc.NextWorkflowStart(now, Profile{CalendarCode: "CN-SSE", TimeZone: "Asia/Shanghai"})
	if err == nil {
		t.Fatal("expected unsupported china calendar year error")
	}
	if got := err.Error(); got != "marketcalendar: china holiday calendar for 2028 is unavailable" {
		t.Fatalf("unexpected error: %s", got)
	}
}

// stubHolidayCalendar lets tests override holiday detection without touching
// the embedded hardcoded data.
type stubHolidayCalendar struct {
	holidays map[string]struct{}
	coverage map[int]bool
}

func (s stubHolidayCalendar) IsHoliday(_ string, localDate time.Time) bool {
	_, ok := s.holidays[localDate.Format("2006-01-02")]
	return ok
}

func (s stubHolidayCalendar) CoverageYear(_ string, year int) bool {
	covered, ok := s.coverage[year]
	if !ok {
		return true
	}
	return covered
}

func TestServiceUsesInjectedClockForNow(t *testing.T) {
	fixed := time.Date(2026, time.May, 14, 12, 34, 56, 0, time.UTC)
	svc := NewServiceWith(FixedClock{Instant: fixed}, nil)
	if got := svc.Now(); !got.Equal(fixed) {
		t.Fatalf("expected clock to return %s, got %s", fixed, got)
	}
}

func TestServiceUsesInjectedHolidayCalendar(t *testing.T) {
	stub := stubHolidayCalendar{
		holidays: map[string]struct{}{"2026-05-14": {}},
		coverage: map[int]bool{2026: true},
	}
	svc := NewServiceWith(nil, stub)
	now := time.Date(2026, time.May, 14, 2, 0, 0, 0, time.UTC)
	_, err := svc.ResolveTradingDate(now, Profile{CalendarCode: "CN-SSE", TimeZone: "Asia/Shanghai"}, ResolutionCurrentTradingDay)
	if err == nil {
		t.Fatal("expected stubbed holiday to mark 2026-05-14 non-trading")
	}
}

func TestServiceUsesInjectedHolidayCalendarCoverage(t *testing.T) {
	stub := stubHolidayCalendar{
		holidays: map[string]struct{}{},
		coverage: map[int]bool{2030: false},
	}
	svc := NewServiceWith(nil, stub)
	now := time.Date(2030, time.January, 6, 2, 0, 0, 0, time.UTC)
	_, err := svc.ResolveTradingDate(now, Profile{CalendarCode: "CN-SSE", TimeZone: "Asia/Shanghai"}, ResolutionCurrentTradingDay)
	if err == nil {
		t.Fatal("expected coverage-missing error from stub calendar")
	}
	if got := err.Error(); got != "marketcalendar: china holiday calendar for 2030 is unavailable" {
		t.Fatalf("unexpected error: %s", got)
	}
}
