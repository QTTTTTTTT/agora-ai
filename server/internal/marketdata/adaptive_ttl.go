package marketdata

import (
	"strings"
	"time"
)

// adaptiveNewsTTL returns the news cache TTL to apply at `now`, scaled by
// whether any major market the platform tracks is currently in or near a
// trading session. The intent is to refresh aggressively during active
// trading windows (where headlines move price quickly) and conserve upstream
// quota outside them.
//
// Policy:
//
//   - During likely active hours (rough union of Asia + EU + US sessions in
//     UTC): use the configured NewsTTL as-is.
//   - Outside those hours (deep overnight in all major TZs): triple the TTL,
//     capped at 10 minutes so we never let news sit too long.
//
// When AdaptiveTTLEnabled is false the configured NewsTTL is always used.
// The policy intentionally does not consider specific exchange calendars or
// holidays – the goal is a coarse cost optimisation, not exact market hours.
func (s *Service) adaptiveNewsTTL(now time.Time) time.Duration {
	base := s.cfg.NewsTTL
	if base <= 0 {
		base = 2 * time.Minute
	}
	if s == nil || !s.cfg.AdaptiveTTLEnabled {
		return base
	}
	if isMajorMarketActive(now) {
		return base
	}
	expanded := base * 3
	const cap = 10 * time.Minute
	if expanded > cap {
		return cap
	}
	return expanded
}

// IsMajorMarketActive is the exported wrapper around isMajorMarketActive,
// used by other packages (e.g. the position-quote refresher) that need to
// know whether the platform-wide cadence should be in-session or
// off-session without duplicating the heuristic.
func IsMajorMarketActive(now time.Time) bool {
	return isMajorMarketActive(now)
}

// isMajorMarketActive returns true when `now` falls within the rough trading
// window of *any* major market the platform serves (mainland China, HK,
// Europe, US). The check uses UTC ranges and is intentionally generous:
//
//   - Asia (Tokyo/Shanghai/HK): 00:00 - 08:00 UTC
//   - Europe (London/Frankfurt): 07:00 - 17:00 UTC
//   - US (NYSE/Nasdaq): 13:00 - 21:00 UTC
//
// Saturday and Sunday in UTC count as off-hours (modulo the small overlap
// with Asian Sunday-evening US trading, which we treat as inactive). The
// resulting union covers ~00:00 - 21:00 UTC on weekdays, with a quiet
// 21:00 - 24:00 UTC window plus the full weekend.
func isMajorMarketActive(now time.Time) bool {
	weekday := now.UTC().Weekday()
	if weekday == time.Saturday || weekday == time.Sunday {
		return false
	}
	hour := now.UTC().Hour()
	if hour >= 0 && hour < 21 {
		return true
	}
	return false
}

// adaptiveQuoteTTL is the quote-cache counterpart to adaptiveNewsTTL. The
// goal is different but the shape mirrors it:
//
//   - When the instrument's primary session is open, use QuoteTTLInSession
//     (default 5s). Combined with singleflight this gives the SSE pusher
//     a high cache-hit rate while still updating UI within ~5s of an
//     actual print.
//   - When the session is closed, use QuoteTTLOffSession (default 60s).
//     The instrument isn't moving, so caching longer reduces upstream
//     load without the user noticing.
//
// Crypto is always-on; we treat it as "in session" 24/7. When
// AdaptiveQuoteTTLEnabled is false the legacy uniform QuoteTTL is used.
func (s *Service) adaptiveQuoteTTL(instrument InstrumentRef, now time.Time) time.Duration {
	if s == nil {
		return 10 * time.Second
	}
	base := s.cfg.QuoteTTL
	if base <= 0 {
		base = 10 * time.Second
	}
	if !s.cfg.AdaptiveQuoteTTLEnabled {
		return base
	}
	inSession := s.cfg.QuoteTTLInSession
	offSession := s.cfg.QuoteTTLOffSession
	if inSession <= 0 {
		inSession = 5 * time.Second
	}
	if offSession <= 0 {
		offSession = 60 * time.Second
	}
	if instrumentInSession(instrument, now) {
		return inSession
	}
	return offSession
}

// instrumentInSession decides whether the instrument's primary market is
// currently transacting. The instrument's `Market` / `AssetClass` is the
// primary signal; we fall back to isMajorMarketActive() for unknown
// instruments. Crypto and futures (which run nearly 24/7) always return
// true so their quote TTL stays short even when equity markets sleep.
func instrumentInSession(instrument InstrumentRef, now time.Time) bool {
	class := strings.ToLower(strings.TrimSpace(instrument.AssetClass))
	if class == "crypto" || class == "futures" || class == "fx" || class == "forex" {
		return true
	}
	market := strings.ToLower(strings.TrimSpace(instrument.Market))
	switch market {
	case "crypto":
		return true
	case "cnstock", "cn-stock", "china", "cn":
		// A股交易时段: 周一至周五 09:30-15:00 (含午休) CST = UTC+8。
		// We use 01:30-07:00 UTC as a generous window covering both
		// sessions; this is the same coarseness as the news helper.
		return isWeekdayUTC(now) && hourMinuteInRange(now, 1, 30, 7, 0)
	case "hkstock", "hk-stock", "hk":
		return isWeekdayUTC(now) && hourMinuteInRange(now, 1, 30, 8, 0)
	case "usstock", "us-stock", "us":
		// US regular session 09:30-16:00 ET = 13:30-20:00 UTC in
		// summer; we use a wider 13:00-21:00 to keep DST simple.
		return isWeekdayUTC(now) && hourMinuteInRange(now, 13, 0, 21, 0)
	case "eustock", "eu":
		return isWeekdayUTC(now) && hourMinuteInRange(now, 7, 0, 17, 0)
	}
	// Unknown market: fall back to the coarse union used for news so we
	// don't accidentally treat an instrument-less request as off-hours.
	return isMajorMarketActive(now)
}

func isWeekdayUTC(now time.Time) bool {
	d := now.UTC().Weekday()
	return d != time.Saturday && d != time.Sunday
}

func hourMinuteInRange(now time.Time, startHour, startMinute, endHour, endMinute int) bool {
	t := now.UTC()
	minutes := t.Hour()*60 + t.Minute()
	startMinutes := startHour*60 + startMinute
	endMinutes := endHour*60 + endMinute
	return minutes >= startMinutes && minutes < endMinutes
}
