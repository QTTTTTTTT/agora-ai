package marketcalendar

import (
	"strings"
	"time"
)

// HolidayCalendar reports closures for a given calendar code/date. Implementations
// may be backed by hardcoded data, a database table, or a remote service.
//
// IsHoliday returns true when the given local date is a market closure for the
// supplied calendar code. CoverageYear reports whether the calendar has data
// for the year - callers use this to surface "calendar not available" errors
// early instead of silently treating an unknown year as fully open.
type HolidayCalendar interface {
	IsHoliday(calendarCode string, localDate time.Time) bool
	CoverageYear(calendarCode string, year int) bool
}

// staticHolidayCalendar is the default implementation backed by the hardcoded
// chinaMarketClosures map plus the algorithmic US holiday detection. Other
// markets (crypto/futures) intentionally have no holidays here and rely on
// weekend rules in Service.isTradingDay.
type staticHolidayCalendar struct{}

// DefaultHolidayCalendar returns the embedded calendar used when callers do
// not supply their own. The data covers China (CN-SSE/CN-SZSE/CFFEX/SHFE/DCE/
// CZCE/INE) and US (US-XNAS/US-XNYS) markets.
func DefaultHolidayCalendar() HolidayCalendar {
	return staticHolidayCalendar{}
}

func (staticHolidayCalendar) IsHoliday(calendarCode string, localDate time.Time) bool {
	switch strings.ToUpper(strings.TrimSpace(calendarCode)) {
	case "CN-SSE", "CN-SZSE", "CFFEX", "SHFE", "DCE", "CZCE", "INE":
		if dates, ok := chinaMarketClosures[localDate.Year()]; ok {
			_, found := dates[localDate.Format("2006-01-02")]
			return found
		}
		return false
	case "US-XNAS", "US-XNYS":
		return isUSHolidayStatic(localDate)
	}
	return false
}

func (staticHolidayCalendar) CoverageYear(calendarCode string, year int) bool {
	switch strings.ToUpper(strings.TrimSpace(calendarCode)) {
	case "CN-SSE", "CN-SZSE", "CFFEX", "SHFE", "DCE", "CZCE", "INE":
		_, ok := chinaMarketClosures[year]
		return ok
	}
	// US holidays are computed algorithmically, so any year is covered.
	// Other calendars (crypto/futures index) have no closure data and are
	// effectively always covered.
	return true
}
