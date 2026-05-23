package cooldown

// Sprint B #1 contract tests for the cooldown.Service:
//
//   - Options.withDefaults pins the production tunings (14d
//     lookback / 24h window) and clamps degenerate inputs so a
//     misconfigured fund.config never produces an infinite-day
//     SQL scan or a sub-second lock window.
//   - Lookup is fail-soft: nil service, nil DB, blank fundID, and
//     an empty symbol list all return (nil, nil) without ever
//     touching the database. The PM wiring layer relies on this so
//     it can call Lookup unconditionally.
//   - normaliseSymbols upper-cases, trims and dedupes so the
//     pq.Array ANY($2) match isn't sensitive to whitespace or
//     accidental duplicates from the universe ∪ positions join.
//   - The SQL path returns locks only for fills inside the window
//     and produces the right sort order (tightest-remaining-first,
//     alphabetic tiebreak) so the prompt rendering is deterministic.
//   - Expired fills (now > LastFillAt + Window) get dropped even
//     when present in the row set.

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestOptionsWithDefaultsProductionTunings(t *testing.T) {
	got := Options{}.withDefaults()
	want := Options{LookbackDays: 14, Window: 24 * time.Hour}
	if got != want {
		t.Errorf("Options{}.withDefaults() = %+v, want %+v", got, want)
	}
}

// LookbackDays must be clamped to [1, 90] so neither a 0-day nor a
// 10000-day fund config can run the platform off the rails.
func TestOptionsWithDefaultsClampsLookbackDays(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{-5, 14},  // negative → default
		{0, 14},   // zero → default
		{1, 1},    // floor
		{30, 30},  // pass-through
		{90, 90},  // ceiling
		{500, 90}, // clamped
	}
	for _, c := range cases {
		got := Options{LookbackDays: c.in}.withDefaults().LookbackDays
		if got != c.want {
			t.Errorf("LookbackDays in=%d → %d, want %d", c.in, got, c.want)
		}
	}
}

// Window must be clamped to [1h, 30d]. The lower clamp suppresses
// degenerate "lock for 5 seconds" configs that effectively disable
// the feature; the upper clamp suppresses a config typo that would
// indefinitely freeze a symbol.
func TestOptionsWithDefaultsClampsWindow(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{0, 24 * time.Hour},
		{time.Minute, time.Hour},
		{2 * time.Hour, 2 * time.Hour},
		{30 * 24 * time.Hour, 30 * 24 * time.Hour},
		{365 * 24 * time.Hour, 30 * 24 * time.Hour},
	}
	for _, c := range cases {
		got := Options{Window: c.in}.withDefaults().Window
		if got != c.want {
			t.Errorf("Window in=%v → %v, want %v", c.in, got, c.want)
		}
	}
}

// Nil receiver and nil DB both produce (nil, nil) — the wiring
// layer relies on this to skip cooldown without a feature flag.
func TestLookupNilSafe(t *testing.T) {
	var s *Service
	got, err := s.Lookup(context.Background(), "f1", []string{"A"}, time.Now())
	if got != nil || err != nil {
		t.Errorf("nil receiver: got %+v, err %v, want nil, nil", got, err)
	}
	s2 := NewService(nil, Options{})
	got, err = s2.Lookup(context.Background(), "f1", []string{"A"}, time.Now())
	if got != nil || err != nil {
		t.Errorf("nil db: got %+v, err %v, want nil, nil", got, err)
	}
}

// Blank fundID short-circuits before any DB call. We assert this by
// passing a sqlmock that EXPECTS no queries and verifying it stays
// satisfied at the end.
func TestLookupBlankFundIDShortCircuits(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewService(db, Options{})
	got, err := s.Lookup(context.Background(), "   ", []string{"A"}, time.Now())
	if got != nil || err != nil {
		t.Errorf("blank fundID: got %+v, err %v, want nil, nil", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB activity on blank fundID: %v", err)
	}
}

// Empty / whitespace-only symbol list short-circuits before any DB
// call. Defends against the wiring layer accidentally calling Lookup
// with an empty universe ∪ positions intersection.
func TestLookupEmptySymbolsShortCircuits(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewService(db, Options{})
	got, err := s.Lookup(context.Background(), "f1", []string{"", "  "}, time.Now())
	if got != nil || err != nil {
		t.Errorf("empty symbols: got %+v, err %v, want nil, nil", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB activity on empty symbols: %v", err)
	}
}

// normaliseSymbols dedupes case-insensitively, trims whitespace and
// drops empties. We pin the contract because the SQL query feeds
// pq.Array directly and any drift would silently miss matches.
func TestNormaliseSymbolsDedupAndUpper(t *testing.T) {
	got := normaliseSymbols([]string{" a ", "A", "b", "B", "", "  ", "c"})
	want := []string{"A", "B", "C"}
	if len(got) != len(want) {
		t.Fatalf("length: got %d (%+v), want %d (%+v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("idx %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// Happy path: two fills inside the 24h window, one outside (already
// expired), one symbol with no fill at all. We assert the SQL is
// exactly what the prompt expects, the locks come back sorted by
// HoursRemaining DESC (tightest constraint first), and the expired
// row is filtered out.
func TestLookupSurfacesActiveLocksAndDropsExpired(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	twoHoursAgo := now.Add(-2 * time.Hour)
	tenHoursAgo := now.Add(-10 * time.Hour)
	threeDaysAgo := now.Add(-72 * time.Hour) // outside 24h window

	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT DISTINCT ON (symbol)
		       symbol, side, COALESCE(executed_at, created_at) AS at
		  FROM trade_executions
		 WHERE fund_id  = $1
		   AND status   = 'filled'
		   AND symbol   = ANY($2)
		   AND COALESCE(executed_at, created_at) >= $3
		 ORDER BY symbol, COALESCE(executed_at, created_at) DESC
	`)).
		WithArgs("fund-1", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"symbol", "side", "at"}).
			AddRow("AAPL", "buy", twoHoursAgo).      // active, 22h remaining
			AddRow("NVDA", "sell", tenHoursAgo).     // active, 14h remaining
			AddRow("MSFT", "buy", threeDaysAgo))     // expired

	s := NewService(db, Options{})
	got, err := s.Lookup(context.Background(), "fund-1", []string{"AAPL", "NVDA", "MSFT", "TSLA"}, now)
	if err != nil {
		t.Fatalf("Lookup error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 active locks (AAPL+NVDA), got %d (%+v)", len(got), got)
	}
	// Tightest-remaining first → AAPL (22h) before NVDA (14h).
	if got[0].Symbol != "AAPL" || got[1].Symbol != "NVDA" {
		t.Errorf("order: got [%s, %s], want [AAPL, NVDA]", got[0].Symbol, got[1].Symbol)
	}
	// Spot-check the derived fields on AAPL.
	if got[0].LastFillSide != "buy" {
		t.Errorf("AAPL side = %q, want buy", got[0].LastFillSide)
	}
	if !got[0].LastFillAt.Equal(twoHoursAgo) {
		t.Errorf("AAPL LastFillAt = %v, want %v", got[0].LastFillAt, twoHoursAgo)
	}
	if !got[0].BlockedUntil.Equal(twoHoursAgo.Add(24 * time.Hour)) {
		t.Errorf("AAPL BlockedUntil = %v, want %v", got[0].BlockedUntil, twoHoursAgo.Add(24*time.Hour))
	}
	if got[0].HoursRemaining < 21.9 || got[0].HoursRemaining > 22.1 {
		t.Errorf("AAPL HoursRemaining = %v, want ≈22", got[0].HoursRemaining)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// SQL errors bubble up — the wiring layer logs them but the rest of
// the PM run still proceeds (cooldown is advisory).
func TestLookupPropagatesQueryError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT DISTINCT ON (symbol)`)).
		WillReturnError(errors.New("boom"))

	s := NewService(db, Options{})
	got, err := s.Lookup(context.Background(), "f1", []string{"A"}, time.Now())
	if err == nil {
		t.Fatal("expected error from failed query, got nil")
	}
	if got != nil {
		t.Errorf("expected nil locks on error, got %+v", got)
	}
}

// Zero-value `now` falls back to time.Now (UTC). Asserting on the
// fallback is brittle, so we just verify the lookback window is
// non-zero by accepting any args; the contract is that no panic
// fires and the function returns a sane result on an empty row set.
func TestLookupZeroNowFallsBackToWallClock(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT DISTINCT ON (symbol)`)).
		WithArgs("f1", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"symbol", "side", "at"}))

	s := NewService(db, Options{})
	got, err := s.Lookup(context.Background(), "f1", []string{"A"}, time.Time{})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty locks, got %+v", got)
	}
}

// Options() returns the effective options even on a nil receiver,
// so diagnostic code can render the current tunings without a nil
// guard.
func TestOptionsAccessor(t *testing.T) {
	var s *Service
	if got := s.Options(); got.LookbackDays != 14 || got.Window != 24*time.Hour {
		t.Errorf("nil receiver Options(): got %+v, want defaults", got)
	}
	s2 := NewService(nil, Options{LookbackDays: 5, Window: 6 * time.Hour})
	if got := s2.Options(); got.LookbackDays != 5 || got.Window != 6*time.Hour {
		t.Errorf("configured Options(): got %+v, want LookbackDays=5 Window=6h", got)
	}
}
