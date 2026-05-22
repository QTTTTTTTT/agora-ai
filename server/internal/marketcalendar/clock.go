package marketcalendar

import "time"

// Clock abstracts wall-clock access so calendar logic can be unit-tested
// deterministically. Production code uses RealClock; tests use FixedClock.
type Clock interface {
	Now() time.Time
}

// RealClock returns the current system time.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

// FixedClock returns a fixed instant. Useful for tests.
type FixedClock struct {
	Instant time.Time
}

func (c FixedClock) Now() time.Time { return c.Instant }
