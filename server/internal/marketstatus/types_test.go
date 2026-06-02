package marketstatus

import (
	"testing"
	"time"
)

func dur(s time.Duration) *time.Duration { return &s }
func tptr(t time.Time) *time.Time         { return &t }
func fptr(f float64) *float64             { return &f }

func newProbe(price float64) OrderProbe {
	return OrderProbe{
		FundID:        "f1",
		InstrumentKey: "AAPL.US",
		Symbol:        "AAPL",
		Market:        "US",
		AssetClass:    "equity",
		Side:          "buy",
		Quantity:      100,
		IntendedPrice: price,
	}
}

func newOpenCalendar(d time.Time) *CalendarDay {
	return &CalendarDay{
		Market:      "US",
		TradingDate: d,
		IsOpen:      true,
		OpenLocal:   "09:30",
		CloseLocal:  "16:00",
		MarketTZ:    "America/New_York",
	}
}

func TestEngine_AllowsCleanInstrument(t *testing.T) {
	e := NewEngine().withClock(func() time.Time {
		return time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC) // 10:30 ET
	})
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	status := &InstrumentStatus{
		InstrumentKey: "AAPL.US", Symbol: "AAPL", Market: "US", AssetClass: "equity",
		Status:      "trading",
		LowerLimit:  fptr(150), UpperLimit: fptr(250),
		LastQuoteAt:    tptr(now.Add(-10 * time.Second)),
		LastQuotePrice: fptr(200),
	}
	day := newOpenCalendar(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	res, err := e.Check(newProbe(200), status, day)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Decision != DecisionAllow {
		t.Errorf("decision = %s, want allow; events=%+v", res.Decision, res.Events)
	}
}

func TestEngine_RejectsHalted(t *testing.T) {
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	e := NewEngine().withClock(func() time.Time { return now })
	status := &InstrumentStatus{
		InstrumentKey: "AAPL.US", Symbol: "AAPL", Market: "US", AssetClass: "equity",
		Status: "halted", HaltReason: "news pending",
		HaltStartedAt: tptr(now.Add(-1 * time.Hour)),
		HaltUntil:     tptr(now.Add(15 * time.Minute)),
		LastQuoteAt:   tptr(now.Add(-10 * time.Second)),
	}
	res, _ := e.Check(newProbe(200), status, newOpenCalendar(now))
	if res.Decision != DecisionReject {
		t.Fatalf("decision = %s; events=%+v", res.Decision, res.Events)
	}
	if res.Events[0].RuleCode != RuleHalted {
		t.Errorf("rule = %s, want halted", res.Events[0].RuleCode)
	}
}

func TestEngine_HaltUntilExpired_DoesNotReject(t *testing.T) {
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	e := NewEngine().withClock(func() time.Time { return now })
	status := &InstrumentStatus{
		InstrumentKey: "AAPL.US", Symbol: "AAPL", AssetClass: "equity",
		Status: "halted", HaltUntil: tptr(now.Add(-5 * time.Minute)),
		LastQuoteAt: tptr(now.Add(-10 * time.Second)),
	}
	res, _ := e.Check(newProbe(200), status, newOpenCalendar(now))
	if res.Decision == DecisionReject {
		t.Errorf("expired halt should not reject; events=%+v", res.Events)
	}
}

func TestEngine_RejectsSuspended(t *testing.T) {
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	e := NewEngine().withClock(func() time.Time { return now })
	status := &InstrumentStatus{
		InstrumentKey: "ZZZ.US", Symbol: "ZZZ", Market: "US", Status: "suspended",
	}
	res, _ := e.Check(newProbe(10), status, nil)
	if res.Decision != DecisionReject || res.Events[0].RuleCode != RuleSuspended {
		t.Errorf("expected suspended reject: %+v", res)
	}
}

func TestEngine_RejectsBelowLowerLimit(t *testing.T) {
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	e := NewEngine().withClock(func() time.Time { return now })
	status := &InstrumentStatus{
		InstrumentKey: "600519.SH", Symbol: "600519", AssetClass: "equity",
		Status: "trading",
		LowerLimit: fptr(1800), UpperLimit: fptr(2200),
		LastQuoteAt: tptr(now.Add(-2 * time.Second)),
	}
	res, _ := e.Check(newProbe(1700), status, nil)
	if res.Decision != DecisionReject || res.Events[0].RuleCode != RulePriceLimit {
		t.Errorf("expected price-limit reject: %+v", res)
	}
}

func TestEngine_RejectsAboveUpperLimit(t *testing.T) {
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	e := NewEngine().withClock(func() time.Time { return now })
	status := &InstrumentStatus{
		InstrumentKey: "600519.SH", Symbol: "600519", AssetClass: "equity",
		Status:      "trading",
		LowerLimit:  fptr(1800), UpperLimit: fptr(2200),
		LastQuoteAt: tptr(now.Add(-2 * time.Second)),
	}
	res, _ := e.Check(newProbe(2300), status, nil)
	if res.Decision != DecisionReject || res.Events[0].RuleCode != RulePriceLimit {
		t.Errorf("expected price-limit reject: %+v", res)
	}
}

func TestEngine_PriceLimitSkippedForMarketOrder(t *testing.T) {
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	e := NewEngine().withClock(func() time.Time { return now })
	status := &InstrumentStatus{
		InstrumentKey: "AAPL.US", Symbol: "AAPL", AssetClass: "equity",
		Status: "trading",
		LowerLimit: fptr(150), UpperLimit: fptr(160),
		LastQuoteAt: tptr(now.Add(-2 * time.Second)),
	}
	probe := newProbe(0)
	res, _ := e.Check(probe, status, nil)
	if res.Decision != DecisionAllow {
		t.Errorf("market order with no IntendedPrice should pass: %+v", res)
	}
}

func TestEngine_StaleQuoteWarns_Equity(t *testing.T) {
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	e := NewEngine().withClock(func() time.Time { return now })
	status := &InstrumentStatus{
		InstrumentKey: "AAPL.US", Symbol: "AAPL", AssetClass: "equity",
		Status: "trading",
		LastQuoteAt: tptr(now.Add(-90 * time.Second)),
	}
	res, _ := e.Check(newProbe(200), status, nil)
	if res.Decision != DecisionWarn {
		t.Errorf("decision = %s, want warn", res.Decision)
	}
	if len(res.Events) != 1 || res.Events[0].RuleCode != RuleStaleQuote {
		t.Errorf("events = %+v", res.Events)
	}
}

func TestEngine_StaleQuoteWarns_FuturesTighter(t *testing.T) {
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	e := NewEngine().withClock(func() time.Time { return now })
	status := &InstrumentStatus{
		InstrumentKey: "ESM6.CME", Symbol: "ESM6", AssetClass: "futures",
		Status: "trading",
		LastQuoteAt: tptr(now.Add(-10 * time.Second)),
	}
	res, _ := e.Check(newProbe(5300), status, nil)
	if res.Decision != DecisionWarn {
		t.Errorf("decision = %s, want warn (futures budget=5s)", res.Decision)
	}
}

func TestEngine_StaleQuoteOverrideHonoured(t *testing.T) {
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	e := NewEngine().withClock(func() time.Time { return now })
	override := 600 * time.Second
	status := &InstrumentStatus{
		InstrumentKey: "BOND.OTC", Symbol: "BOND", AssetClass: "bond",
		Status: "trading",
		StalenessBudget: &override,
		LastQuoteAt:     tptr(now.Add(-300 * time.Second)),
	}
	res, _ := e.Check(newProbe(99), status, nil)
	if res.Decision != DecisionAllow {
		t.Errorf("override should allow: %+v", res)
	}
}

func TestEngine_NoQuoteTimestamp_Warns(t *testing.T) {
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	e := NewEngine().withClock(func() time.Time { return now })
	status := &InstrumentStatus{
		InstrumentKey: "BOND.OTC", Symbol: "BOND", AssetClass: "bond",
		Status: "trading",
	}
	res, _ := e.Check(newProbe(99), status, nil)
	if res.Decision != DecisionWarn {
		t.Errorf("missing quote timestamp must warn: %+v", res)
	}
}

func TestEngine_RejectsClosedDay(t *testing.T) {
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	e := NewEngine().withClock(func() time.Time { return now })
	day := &CalendarDay{
		Market: "CN", TradingDate: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		IsOpen: false, MarketTZ: "Asia/Shanghai",
	}
	res, _ := e.Check(newProbe(200), nil, day)
	if res.Decision != DecisionReject || res.Events[0].RuleCode != RuleMarketClosed {
		t.Errorf("expected market_closed reject: %+v", res)
	}
}

func TestEngine_RejectsBeforeOpenLocal(t *testing.T) {
	// 06:00 UTC = 14:00 Shanghai but day Open is 09:30 local =
	// 01:30 UTC; so 06:00 UTC is AFTER open. Pick a time before
	// open instead: 00:00 UTC = 08:00 SH which is before open
	// at 09:30 SH.
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	e := NewEngine().withClock(func() time.Time { return now })
	day := &CalendarDay{
		Market: "CN", TradingDate: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		IsOpen: true, OpenLocal: "09:30", CloseLocal: "15:00",
		MarketTZ: "Asia/Shanghai",
	}
	res, _ := e.Check(newProbe(200), nil, day)
	if res.Decision != DecisionReject {
		t.Errorf("decision = %s, want reject (before open)", res.Decision)
	}
}

func TestEngine_HalfDayWarns_DuringSession(t *testing.T) {
	// Half-day session 09:30..12:00 HKT. now = 11:30 HKT = 03:30 UTC.
	now := time.Date(2026, 12, 24, 3, 30, 0, 0, time.UTC)
	e := NewEngine().withClock(func() time.Time { return now })
	day := &CalendarDay{
		Market: "HK", TradingDate: time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC),
		IsOpen: true, OpenLocal: "09:30", CloseLocal: "12:00",
		MarketTZ: "Asia/Hong_Kong", HalfDay: true,
	}
	res, _ := e.Check(newProbe(50), nil, day)
	if res.Decision != DecisionWarn {
		t.Errorf("decision = %s, want warn (half-day in-session)", res.Decision)
	}
	if res.Events[0].RuleCode != RuleHalfDayClosed {
		t.Errorf("rule = %s, want half_day_closed", res.Events[0].RuleCode)
	}
}

func TestEngine_HalfDayRejects_AfterEarlyClose(t *testing.T) {
	// 13:00 HKT = 05:00 UTC, after the 12:00 HKT half-day close.
	now := time.Date(2026, 12, 24, 5, 0, 0, 0, time.UTC)
	e := NewEngine().withClock(func() time.Time { return now })
	day := &CalendarDay{
		Market: "HK", TradingDate: time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC),
		IsOpen: true, OpenLocal: "09:30", CloseLocal: "12:00",
		MarketTZ: "Asia/Hong_Kong", HalfDay: true,
	}
	res, _ := e.Check(newProbe(50), nil, day)
	if res.Decision != DecisionReject || res.Events[0].RuleCode != RuleHalfDayClosed {
		t.Errorf("expected half-day reject: %+v", res)
	}
}

func TestEngine_BadTZ_Warns(t *testing.T) {
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	e := NewEngine().withClock(func() time.Time { return now })
	day := &CalendarDay{
		Market: "??", TradingDate: now, IsOpen: true,
		OpenLocal: "09:30", CloseLocal: "15:00",
		MarketTZ: "NotARealTZ/Foo",
	}
	res, _ := e.Check(newProbe(200), nil, day)
	if res.Decision != DecisionWarn {
		t.Errorf("bad tz should warn, got %s", res.Decision)
	}
}

func TestEngine_HardestRejectShortCircuits(t *testing.T) {
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	e := NewEngine().withClock(func() time.Time { return now })
	// Halted AND price-limit breach AND stale: only halted event
	// should be in the result (suspended runs first but is not
	// set; halted dominates).
	status := &InstrumentStatus{
		InstrumentKey: "AAPL.US", Symbol: "AAPL", AssetClass: "equity",
		Status: "halted", HaltReason: "circuit breaker",
		LowerLimit: fptr(190), UpperLimit: fptr(210),
		LastQuoteAt: tptr(now.Add(-10 * time.Minute)),
	}
	res, _ := e.Check(newProbe(500), status, newOpenCalendar(now))
	if res.Decision != DecisionReject {
		t.Fatalf("decision = %s", res.Decision)
	}
	if len(res.Events) != 1 || res.Events[0].RuleCode != RuleHalted {
		t.Errorf("expected only halted event: %+v", res.Events)
	}
}

func TestEngine_NilEngine_Errors(t *testing.T) {
	var e *Engine
	if _, err := e.Check(newProbe(1), nil, nil); err == nil {
		t.Error("expected nil-engine error")
	}
}

func TestEngine_EmptyInstrumentKey_Errors(t *testing.T) {
	e := NewEngine()
	probe := newProbe(1)
	probe.InstrumentKey = ""
	if _, err := e.Check(probe, nil, nil); err == nil {
		t.Error("expected invalid-probe error")
	}
}

func TestEffectiveStalenessBudget_Lookup(t *testing.T) {
	if EffectiveStalenessBudget(nil) != fallbackStalenessBudget {
		t.Errorf("nil status → fallback")
	}
	s := &InstrumentStatus{AssetClass: "futures"}
	if EffectiveStalenessBudget(s) != 5*time.Second {
		t.Errorf("futures default = %v", EffectiveStalenessBudget(s))
	}
	s2 := &InstrumentStatus{AssetClass: "equity", StalenessBudget: dur(120 * time.Second)}
	if EffectiveStalenessBudget(s2) != 120*time.Second {
		t.Errorf("override should win = %v", EffectiveStalenessBudget(s2))
	}
}
