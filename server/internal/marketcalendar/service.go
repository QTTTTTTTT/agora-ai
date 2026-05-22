package marketcalendar

import (
	"fmt"
	"strings"
	"time"
)

type ResolutionMode string

const (
	ResolutionCurrentTradingDay ResolutionMode = "current"
	ResolutionLatestTradingDay  ResolutionMode = "latest"
	ResolutionNextTradingDay    ResolutionMode = "next"
)

type Profile struct {
	Market       string
	Exchange     string
	AssetClass   string
	CalendarCode string
	TimeZone     string
	// DecisionIntervalMinutes turns the one-shot daily workflow into a
	// recurring intra-day loop anchored on the market's trading
	// windows. When non-nil, NextWorkflowStart returns the next slot
	// (PreOpen + k·interval ∈ active windows). When nil, the legacy
	// MacroBrief = PreOpen − 30 min anchor is used so funds that have
	// never opted in keep the same behaviour they had before.
	// Forwarded from FundAutoExecuteConfig and preserved across
	// NormalizeProfile so callers stamp it on once and forget about
	// it.
	DecisionIntervalMinutes *int
}

// Interval bounds. Below 5 min is hard to honour given the scheduler's
// poll cadence and would also pummel LLM providers; above 12h leaves at
// most one trigger per session for most markets, at which point you may
// as well use the legacy daily mode. Clamping (rather than rejecting)
// makes a fat-finger fall back to a usable cadence instead of silently
// reverting to the legacy default.
const (
	MinDecisionIntervalMinutes = 5
	MaxDecisionIntervalMinutes = 12 * 60
)

// clampDecisionIntervalMinutes folds the override into the supported
// envelope. Pure function so the calendar / scheduler tests can verify
// both extremes without touching real time.
func clampDecisionIntervalMinutes(v int) int {
	if v < MinDecisionIntervalMinutes {
		return MinDecisionIntervalMinutes
	}
	if v > MaxDecisionIntervalMinutes {
		return MaxDecisionIntervalMinutes
	}
	return v
}

type SessionSegment struct {
	Label string
	Open  time.Time
	Close time.Time
}

type TradingSession struct {
	TradingDate  string
	Location     *time.Location
	PreOpenTime  time.Time
	OpenTime     time.Time
	CloseTime    time.Time
	IsTradingDay bool
	IsHalfDay    bool
	Segments     []SessionSegment
}

type StepSchedule struct {
	MacroBrief       time.Time
	ResearchParallel time.Time
	QuantSignals     time.Time
	Roundtable       time.Time
	PMPlan           time.Time
	RiskReview       time.Time
	UserApproval     time.Time
	TradeExecution   time.Time
	Settlement       time.Time
	DailyReview      time.Time
}

type Service struct {
	clock    Clock
	holidays HolidayCalendar
}

// NewService constructs a Service with the production clock and the default
// (hardcoded) holiday calendar. Callers wanting deterministic time or a
// custom holiday source should use NewServiceWith.
func NewService() *Service {
	return NewServiceWith(RealClock{}, DefaultHolidayCalendar())
}

// NewServiceWith constructs a Service with explicit dependencies. A nil clock
// or holidays argument falls back to the production defaults so call sites
// only need to override what they care about.
func NewServiceWith(clock Clock, holidays HolidayCalendar) *Service {
	if clock == nil {
		clock = RealClock{}
	}
	if holidays == nil {
		holidays = DefaultHolidayCalendar()
	}
	return &Service{clock: clock, holidays: holidays}
}

// Now returns the current instant according to the service's Clock. Callers
// that want to respect the configured clock (instead of calling time.Now()
// directly) should use this helper.
func (s *Service) Now() time.Time {
	if s == nil || s.clock == nil {
		return time.Now()
	}
	return s.clock.Now()
}

func (s *Service) NormalizeProfile(profile Profile) (Profile, error) {
	market := strings.ToLower(strings.TrimSpace(profile.Market))
	exchange := strings.ToUpper(strings.TrimSpace(profile.Exchange))
	assetClass := strings.ToLower(strings.TrimSpace(profile.AssetClass))
	calendarCode := strings.ToUpper(strings.TrimSpace(profile.CalendarCode))
	timeZone := strings.TrimSpace(profile.TimeZone)

	// Normalise alternate market spellings to the canonical token so the
	// switch below (and downstream callers that pattern-match on
	// profile.Market) work whether the caller used the legacy short name
	// or the longer "cn_a_share" / "us_stock" forms the create-fund API
	// accepts. Without this, a fund created via the API with
	// market="cn_a_share" would fall off every case in this function and
	// end up with the catch-all US-XNAS calendar.
	switch market {
	case "cn_a_share", "a_shares", "cn_equity", "china_a_share":
		market = "a_share"
	case "us_stock", "us_stocks", "us_equities", "usequity":
		market = "us_equity"
	case "crypto_currency", "cryptocurrency", "digital_asset":
		market = "crypto"
	case "future", "futures_contract", "commodity_futures":
		market = "futures"
	}

	if market == "" {
		switch exchange {
		case "SSE", "SZSE":
			market = "a_share"
		case "NASDAQ", "XNYS", "NYSE", "XNAS":
			market = "us_equity"
		case "BINANCE", "OKX", "BYBIT", "COINBASE":
			market = "crypto"
		case "CME", "CBOT", "COMEX", "NYMEX":
			market = "futures"
		case "CFFEX", "SHFE", "DCE", "CZCE", "INE":
			market = "futures"
		}
	}

	if calendarCode == "" {
		switch {
		case exchange == "SSE":
			calendarCode = "CN-SSE"
		case exchange == "SZSE":
			calendarCode = "CN-SZSE"
		case exchange == "NASDAQ" || exchange == "XNAS":
			calendarCode = "US-XNAS"
		case exchange == "NYSE" || exchange == "XNYS":
			calendarCode = "US-XNYS"
		case exchange == "CME" || exchange == "CBOT" || exchange == "COMEX" || exchange == "NYMEX":
			calendarCode = "CME-INDEX"
		case exchange == "CFFEX":
			calendarCode = "CFFEX"
		case exchange == "SHFE" || exchange == "DCE" || exchange == "CZCE" || exchange == "INE":
			calendarCode = exchange
		case market == "a_share":
			calendarCode = "CN-SSE"
		case market == "us_equity":
			calendarCode = "US-XNAS"
		case market == "crypto" || assetClass == "crypto":
			calendarCode = "CRYPTO-24X7"
		case market == "futures" || assetClass == "futures":
			calendarCode = "CME-INDEX"
		}
	}

	if calendarCode == "" {
		calendarCode = "US-XNAS"
	}
	// Infer timezone AFTER calendarCode defaulting so the catch-all US-XNAS
	// case still gets America/New_York rather than UTC. The "UTC -> inferred"
	// path covers callers that pass an explicit UTC but really wanted the
	// calendar-native zone (common mistake from clients that copy time.Now().UTC()).
	if inferred := defaultTimeZoneForCalendar(calendarCode); timeZone == "" || (strings.EqualFold(timeZone, "UTC") && inferred != "" && inferred != "UTC") {
		timeZone = inferred
	}
	if timeZone == "" {
		timeZone = "UTC"
	}
	if _, err := time.LoadLocation(timeZone); err != nil {
		return Profile{}, fmt.Errorf("marketcalendar: invalid timezone %q: %w", timeZone, err)
	}

	out := Profile{
		Market:       market,
		Exchange:     exchange,
		AssetClass:   assetClass,
		CalendarCode: calendarCode,
		TimeZone:     timeZone,
	}
	// Preserve the per-fund interval override across normalization so
	// callers that stamp it on once (e.g. tradingProfileForFund) do
	// not have to re-stamp after every NormalizeProfile round-trip.
	if profile.DecisionIntervalMinutes != nil {
		clamped := clampDecisionIntervalMinutes(*profile.DecisionIntervalMinutes)
		out.DecisionIntervalMinutes = &clamped
	}
	return out, nil
}

func defaultTimeZoneForCalendar(calendarCode string) string {
	switch calendarCode {
	case "CN-SSE", "CN-SZSE", "CFFEX", "SHFE", "DCE", "CZCE", "INE":
		return "Asia/Shanghai"
	case "US-XNAS", "US-XNYS":
		return "America/New_York"
	case "CME-INDEX":
		return "America/Chicago"
	case "CRYPTO-24X7":
		return "UTC"
	default:
		return ""
	}
}

func (s *Service) ResolveTradingDate(now time.Time, profile Profile, mode ResolutionMode) (time.Time, error) {
	normalized, err := s.NormalizeProfile(profile)
	if err != nil {
		return time.Time{}, err
	}
	loc, _ := time.LoadLocation(normalized.TimeZone)
	localNow := now.In(loc)
	current := dateOnly(localNow)

	switch mode {
	case ResolutionCurrentTradingDay:
		if err := s.validateCalendarSupport(current, normalized); err != nil {
			return time.Time{}, err
		}
		if !s.isTradingDay(current, normalized) {
			return time.Time{}, fmt.Errorf("marketcalendar: %s is not a trading day for %s", current.Format("2006-01-02"), normalized.CalendarCode)
		}
		return storageDate(current), nil
	case ResolutionLatestTradingDay:
		for i := 0; i < 370; i++ {
			candidate := current.AddDate(0, 0, -i)
			if err := s.validateCalendarSupport(candidate, normalized); err != nil {
				return time.Time{}, err
			}
			if s.isTradingDay(candidate, normalized) {
				return storageDate(candidate), nil
			}
		}
	case ResolutionNextTradingDay:
		for i := 0; i < 370; i++ {
			candidate := current.AddDate(0, 0, i)
			if err := s.validateCalendarSupport(candidate, normalized); err != nil {
				return time.Time{}, err
			}
			if s.isTradingDay(candidate, normalized) {
				return storageDate(candidate), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("marketcalendar: unable to resolve trading date for %s", normalized.CalendarCode)
}

func (s *Service) SessionForDate(tradingDate time.Time, profile Profile) (*TradingSession, error) {
	normalized, err := s.NormalizeProfile(profile)
	if err != nil {
		return nil, err
	}
	loc, _ := time.LoadLocation(normalized.TimeZone)
	base := dateFromStorage(tradingDate, loc)
	if err := s.validateCalendarSupport(base, normalized); err != nil {
		return nil, err
	}
	if !s.isTradingDay(base, normalized) {
		return &TradingSession{
			TradingDate:  base.Format("2006-01-02"),
			Location:     loc,
			IsTradingDay: false,
		}, nil
	}

	session := &TradingSession{
		TradingDate:  base.Format("2006-01-02"),
		Location:     loc,
		IsTradingDay: true,
	}

	switch normalized.CalendarCode {
	case "CN-SSE", "CN-SZSE":
		session.PreOpenTime = combineClock(base, loc, 9, 0)
		session.OpenTime = combineClock(base, loc, 9, 30)
		session.CloseTime = combineClock(base, loc, 15, 0)
		session.Segments = []SessionSegment{
			{Label: "morning", Open: combineClock(base, loc, 9, 30), Close: combineClock(base, loc, 11, 30)},
			{Label: "afternoon", Open: combineClock(base, loc, 13, 0), Close: combineClock(base, loc, 15, 0)},
		}
	case "US-XNAS", "US-XNYS":
		session.PreOpenTime = combineClock(base, loc, 9, 0)
		session.OpenTime = combineClock(base, loc, 9, 30)
		closeHour, closeMinute := 16, 0
		if s.isUSHalfDay(base) {
			session.IsHalfDay = true
			closeHour, closeMinute = 13, 0
		}
		session.CloseTime = combineClock(base, loc, closeHour, closeMinute)
		session.Segments = []SessionSegment{{Label: "regular", Open: session.OpenTime, Close: session.CloseTime}}
	case "CRYPTO-24X7":
		session.PreOpenTime = combineClock(base, loc, 8, 30)
		session.OpenTime = combineClock(base, loc, 9, 0)
		session.CloseTime = combineClock(base, loc, 21, 0)
		session.Segments = []SessionSegment{{Label: "continuous", Open: session.OpenTime, Close: session.CloseTime}}
	case "CME-INDEX":
		session.PreOpenTime = combineClock(base, loc, 8, 0)
		session.OpenTime = combineClock(base, loc, 8, 30)
		session.CloseTime = combineClock(base, loc, 15, 0)
		session.Segments = []SessionSegment{{Label: "regular", Open: session.OpenTime, Close: session.CloseTime}}
	default:
		session.PreOpenTime = combineClock(base, loc, 8, 45)
		session.OpenTime = combineClock(base, loc, 9, 0)
		session.CloseTime = combineClock(base, loc, 15, 0)
		session.Segments = []SessionSegment{{Label: "day", Open: session.OpenTime, Close: session.CloseTime}}
	}

	return session, nil
}

func (s *Service) BuildStepSchedule(session *TradingSession) StepSchedule {
	if session == nil {
		return StepSchedule{}
	}
	return StepSchedule{
		MacroBrief:       session.PreOpenTime.Add(-30 * time.Minute),
		ResearchParallel: session.PreOpenTime.Add(-10 * time.Minute),
		QuantSignals:     session.OpenTime,
		Roundtable:       session.OpenTime.Add(20 * time.Minute),
		PMPlan:           session.OpenTime.Add(45 * time.Minute),
		RiskReview:       session.OpenTime.Add(60 * time.Minute),
		UserApproval:     session.OpenTime.Add(75 * time.Minute),
		TradeExecution:   session.OpenTime.Add(90 * time.Minute),
		Settlement:       session.CloseTime.Add(5 * time.Minute),
		DailyReview:      session.CloseTime.Add(30 * time.Minute),
	}
}

// activeTradingWindows returns the contiguous wall-clock windows during
// which an interval-mode workflow should fire, ordered chronologically.
// The pre-open auction (PreOpenTime → OpenTime) is folded into the
// first window when distinct from regular trading so the user-facing
// "9:00 auction" is still reachable. Adjacent segments are merged so a
// market with no midday break produces one big window, while CN-style
// 11:30–13:00 lunches naturally split the day into two.
func activeTradingWindows(session *TradingSession) []SessionSegment {
	if session == nil || !session.IsTradingDay {
		return nil
	}
	segments := []SessionSegment{}
	if !session.PreOpenTime.IsZero() && !session.OpenTime.IsZero() && session.PreOpenTime.Before(session.OpenTime) {
		segments = append(segments, SessionSegment{
			Label: "pre_open",
			Open:  session.PreOpenTime,
			Close: session.OpenTime,
		})
	}
	if len(session.Segments) > 0 {
		segments = append(segments, session.Segments...)
	} else if !session.OpenTime.IsZero() && !session.CloseTime.IsZero() && session.OpenTime.Before(session.CloseTime) {
		segments = append(segments, SessionSegment{Label: "regular", Open: session.OpenTime, Close: session.CloseTime})
	}
	if len(segments) == 0 {
		return nil
	}
	merged := []SessionSegment{segments[0]}
	for _, seg := range segments[1:] {
		tail := &merged[len(merged)-1]
		if !seg.Open.After(tail.Close) {
			if seg.Close.After(tail.Close) {
				tail.Close = seg.Close
			}
			continue
		}
		merged = append(merged, seg)
	}
	return merged
}

// TradingTriggerSlots returns every trigger time produced by anchoring
// `intervalMinutes` to the start of each active trading window. Slots
// are strictly inside the window — the window-close instant is excluded
// so a 30-minute cadence on CN's morning segment fires at 9:30, 10:00,
// 10:30, 11:00 (not 11:30, which is the close). Returns nil when the
// interval is unset or the day is not a trading day. Pure function;
// safe to call from unit tests.
func (s *Service) TradingTriggerSlots(session *TradingSession, intervalMinutes int) []time.Time {
	if session == nil || !session.IsTradingDay {
		return nil
	}
	interval := time.Duration(clampDecisionIntervalMinutes(intervalMinutes)) * time.Minute
	if interval <= 0 {
		return nil
	}
	windows := activeTradingWindows(session)
	if len(windows) == 0 {
		return nil
	}
	slots := []time.Time{}
	for _, w := range windows {
		if !w.Open.Before(w.Close) {
			continue
		}
		for t := w.Open; t.Before(w.Close); t = t.Add(interval) {
			slots = append(slots, t)
		}
	}
	return slots
}

// TriggerSlotGrace absorbs the millisecond / sub-second drift
// between when the scheduler timer fires and the wall-clock slot
// boundary. Without it, a scheduler woken at 14:30:00.018 looking
// for the 14:30 slot sees `14:30.Before(14:30:00.018) == true`
// and rolls the trigger forward to the next trading day's first
// slot — the bug that left `tong/ocs` idle after the 14:00
// → 14:30 → 15:00 sequence on 2026-05-21.
//
// 30s is well below the 5-minute floor on DecisionIntervalMinutes
// (clampDecisionIntervalMinutes) so the grace can never let two
// adjacent slots collide; and any slot that has already fired is
// caught by workflowRunStartedAtOrAfter dedup one layer up.
const TriggerSlotGrace = 30 * time.Second

// NextTriggerSlot returns the earliest slot that is not before `now`
// (with TriggerSlotGrace of leniency for timer drift) plus an ok
// flag. When no slot remains in the day ok is false and callers
// should advance to the next trading day. Encapsulating the
// "find next" logic here keeps the scheduler loop a one-liner
// per fund.
func (s *Service) NextTriggerSlot(session *TradingSession, intervalMinutes int, now time.Time) (time.Time, bool) {
	cutoff := now.Add(-TriggerSlotGrace)
	for _, slot := range s.TradingTriggerSlots(session, intervalMinutes) {
		if !slot.Before(cutoff) {
			return slot, true
		}
	}
	return time.Time{}, false
}

func (s *Service) NextWorkflowStart(now time.Time, profile Profile) (time.Time, time.Time, error) {
	normalized, err := s.NormalizeProfile(profile)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	loc, _ := time.LoadLocation(normalized.TimeZone)
	localNow := now.In(loc)
	current := dateOnly(localNow)

	for i := 0; i < 370; i++ {
		candidate := current.AddDate(0, 0, i)
		if err := s.validateCalendarSupport(candidate, normalized); err != nil {
			return time.Time{}, time.Time{}, err
		}
		if !s.isTradingDay(candidate, normalized) {
			continue
		}
		session, err := s.SessionForDate(storageDate(candidate), normalized)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}

		// Interval mode: walk this day's slot list and return the
		// first slot at or after `now`. When none remain, fall
		// through to the next trading day (loop continues). The legacy
		// MacroBrief path is bypassed entirely so the scheduler sees a
		// strictly slot-aligned trigger time.
		if normalized.DecisionIntervalMinutes != nil {
			if slot, ok := s.NextTriggerSlot(session, *normalized.DecisionIntervalMinutes, localNow); ok {
				return slot, storageDate(candidate), nil
			}
			continue
		}

		stepSchedule := s.BuildStepSchedule(session)
		if i == 0 {
			if localNow.Before(stepSchedule.MacroBrief) {
				return stepSchedule.MacroBrief, storageDate(candidate), nil
			}
			if localNow.Before(stepSchedule.DailyReview.Add(2 * time.Hour)) {
				return localNow, storageDate(candidate), nil
			}
			continue
		}
		return stepSchedule.MacroBrief, storageDate(candidate), nil
	}
	return time.Time{}, time.Time{}, fmt.Errorf("marketcalendar: unable to find next workflow start for %s", normalized.CalendarCode)
}

func (s *Service) isTradingDay(localDate time.Time, profile Profile) bool {
	if strings.EqualFold(profile.CalendarCode, "CRYPTO-24X7") {
		return true
	}
	if isWeekend(localDate) {
		return false
	}
	if s.holidayCalendar().IsHoliday(profile.CalendarCode, localDate) {
		return false
	}
	return true
}

func (s *Service) validateCalendarSupport(localDate time.Time, profile Profile) error {
	switch profile.CalendarCode {
	case "CN-SSE", "CN-SZSE", "CFFEX", "SHFE", "DCE", "CZCE", "INE":
		if !s.holidayCalendar().CoverageYear(profile.CalendarCode, localDate.Year()) {
			return fmt.Errorf("marketcalendar: china holiday calendar for %d is unavailable", localDate.Year())
		}
	}
	return nil
}

func (s *Service) holidayCalendar() HolidayCalendar {
	if s == nil || s.holidays == nil {
		return DefaultHolidayCalendar()
	}
	return s.holidays
}

func isUSHolidayStatic(localDate time.Time) bool {
	year := localDate.Year()
	formatted := localDate.Format("2006-01-02")
	holidays := map[string]struct{}{
		observedHoliday(time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC)).Format("2006-01-02"):   {},
		thirdWeekday(year, time.January, time.Monday).Format("2006-01-02"):                             {},
		thirdWeekday(year, time.February, time.Monday).Format("2006-01-02"):                            {},
		goodFriday(year).Format("2006-01-02"):                                                          {},
		lastWeekday(year, time.May, time.Monday).Format("2006-01-02"):                                  {},
		observedHoliday(time.Date(year, time.June, 19, 0, 0, 0, 0, time.UTC)).Format("2006-01-02"):     {},
		observedHoliday(time.Date(year, time.July, 4, 0, 0, 0, 0, time.UTC)).Format("2006-01-02"):      {},
		firstWeekday(year, time.September, time.Monday).Format("2006-01-02"):                           {},
		fourthThursday(year, time.November).Format("2006-01-02"):                                       {},
		observedHoliday(time.Date(year, time.December, 25, 0, 0, 0, 0, time.UTC)).Format("2006-01-02"): {},
	}
	newYearNext := observedHoliday(time.Date(year+1, time.January, 1, 0, 0, 0, 0, time.UTC)).Format("2006-01-02")
	if strings.HasPrefix(newYearNext, fmt.Sprintf("%04d-", year)) {
		holidays[newYearNext] = struct{}{}
	}
	_, found := holidays[formatted]
	return found
}

func (s *Service) isUSHalfDay(localDate time.Time) bool {
	if isWeekend(localDate) || s.holidayCalendar().IsHoliday("US-XNAS", localDate) {
		return false
	}
	year := localDate.Year()
	thanksgiving := fourthThursday(year, time.November)
	if sameDay(localDate, thanksgiving.AddDate(0, 0, 1)) {
		return true
	}
	if localDate.Month() == time.December && localDate.Day() == 24 {
		return true
	}
	if localDate.Month() == time.July && localDate.Day() == 3 && localDate.Weekday() >= time.Monday && localDate.Weekday() <= time.Thursday {
		return true
	}
	if localDate.Month() == time.July && localDate.Day() == 2 {
		july4Weekday := time.Date(year, time.July, 4, 0, 0, 0, 0, localDate.Location()).Weekday()
		if july4Weekday == time.Sunday {
			return true
		}
	}
	return false
}

func storageDate(localDate time.Time) time.Time {
	return time.Date(localDate.Year(), localDate.Month(), localDate.Day(), 0, 0, 0, 0, time.UTC)
}

func dateFromStorage(storage time.Time, loc *time.Location) time.Time {
	utc := storage.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, loc)
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func combineClock(base time.Time, loc *time.Location, hour, minute int) time.Time {
	return time.Date(base.Year(), base.Month(), base.Day(), hour, minute, 0, 0, loc)
}

func isWeekend(t time.Time) bool {
	return t.Weekday() == time.Saturday || t.Weekday() == time.Sunday
}

func observedHoliday(date time.Time) time.Time {
	switch date.Weekday() {
	case time.Saturday:
		return date.AddDate(0, 0, -1)
	case time.Sunday:
		return date.AddDate(0, 0, 1)
	default:
		return date
	}
}

func firstWeekday(year int, month time.Month, weekday time.Weekday) time.Time {
	date := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	for date.Weekday() != weekday {
		date = date.AddDate(0, 0, 1)
	}
	return date
}

func thirdWeekday(year int, month time.Month, weekday time.Weekday) time.Time {
	date := firstWeekday(year, month, weekday)
	return date.AddDate(0, 0, 14)
}

func lastWeekday(year int, month time.Month, weekday time.Weekday) time.Time {
	date := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC)
	for date.Weekday() != weekday {
		date = date.AddDate(0, 0, -1)
	}
	return date
}

func fourthThursday(year int, month time.Month) time.Time {
	date := firstWeekday(year, month, time.Thursday)
	return date.AddDate(0, 0, 21)
}

func goodFriday(year int) time.Time {
	return easterSunday(year).AddDate(0, 0, -2)
}

func easterSunday(year int) time.Time {
	a := year % 19
	b := year / 100
	c := year % 100
	d := b / 4
	e := b % 4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i := c / 4
	k := c % 4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := ((h + l - 7*m + 114) % 31) + 1
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
}

func sameDay(a, b time.Time) bool {
	return a.Year() == b.Year() && a.Month() == b.Month() && a.Day() == b.Day()
}

var chinaMarketClosures = map[int]map[string]struct{}{
	2025: {
		"2025-01-01": {},
		"2025-01-28": {},
		"2025-01-29": {},
		"2025-01-30": {},
		"2025-01-31": {},
		"2025-02-03": {},
		"2025-02-04": {},
		"2025-04-04": {},
		"2025-05-01": {},
		"2025-05-02": {},
		"2025-05-05": {},
		"2025-06-02": {},
		"2025-10-01": {},
		"2025-10-02": {},
		"2025-10-03": {},
		"2025-10-06": {},
		"2025-10-07": {},
		"2025-10-08": {},
	},
	2026: {
		"2026-01-01": {},
		"2026-01-02": {},
		"2026-02-16": {},
		"2026-02-17": {},
		"2026-02-18": {},
		"2026-02-19": {},
		"2026-02-20": {},
		"2026-02-23": {},
		"2026-04-06": {},
		"2026-05-01": {},
		"2026-05-04": {},
		"2026-05-05": {},
		"2026-06-19": {},
		"2026-09-25": {},
		"2026-10-01": {},
		"2026-10-02": {},
		"2026-10-05": {},
		"2026-10-06": {},
		"2026-10-07": {},
	},
	2027: {
		"2027-01-01": {},
		"2027-02-08": {},
		"2027-02-09": {},
		"2027-02-10": {},
		"2027-02-11": {},
		"2027-02-12": {},
		"2027-04-05": {},
		"2027-05-03": {},
		"2027-05-04": {},
		"2027-05-05": {},
		"2027-06-14": {},
		"2027-09-24": {},
		"2027-10-01": {},
		"2027-10-04": {},
		"2027-10-05": {},
		"2027-10-06": {},
		"2027-10-07": {},
	},
}
