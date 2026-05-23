package earnings

import (
	"context"
	"errors"
	"testing"
	"time"
)

// staticClock returns a deterministic time function pinned to t.
func staticClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// ---------------------------------------------------------------------------
// NoopFetcher / NewService defaults
// ---------------------------------------------------------------------------

func TestNewServiceTreatsNilFetcherAsNoop(t *testing.T) {
	svc := NewService(nil, Options{})
	snap := svc.Build(context.Background(), []string{"AAPL"}, "us_equity")
	if snap != nil {
		t.Fatalf("expected nil snapshot with nil fetcher, got %+v", snap)
	}
}

func TestNewServiceAppliesDefaults(t *testing.T) {
	svc := NewService(NoopFetcher{}, Options{})
	if svc.horizonDays != 14 {
		t.Errorf("horizonDays default = %d, want 14", svc.horizonDays)
	}
	if svc.now == nil {
		t.Error("now function default missing")
	}
}

// ---------------------------------------------------------------------------
// StaticFetcher happy path
// ---------------------------------------------------------------------------

func TestServiceBuildKeepsFutureInsideHorizon(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	fetcher := StaticFetcher{Events: []Event{
		{Symbol: "AAPL", EventDate: now.AddDate(0, 0, 3), TimeOfDay: TimeAMC, Source: "test"},
		{Symbol: "MSFT", EventDate: now.AddDate(0, 0, 10), TimeOfDay: TimeBMO, Source: "test"},
		{Symbol: "GOOG", EventDate: now.AddDate(0, 0, 60), TimeOfDay: TimeAMC, Source: "test"}, // beyond horizon
		{Symbol: "NVDA", EventDate: now.AddDate(0, 0, -1), TimeOfDay: TimeBMO, Source: "test"}, // past
	}}
	svc := NewService(fetcher, Options{Now: staticClock(now), HorizonDays: 14})
	snap := svc.Build(context.Background(), []string{"AAPL", "MSFT", "GOOG", "NVDA"}, "us_equity")
	if !snap.HasSignal() {
		t.Fatalf("expected signal, got %+v", snap)
	}
	if _, ok := snap.PerSymbol["AAPL"]; !ok {
		t.Error("expected AAPL inside horizon")
	}
	if _, ok := snap.PerSymbol["MSFT"]; !ok {
		t.Error("expected MSFT inside horizon")
	}
	if _, ok := snap.PerSymbol["GOOG"]; ok {
		t.Error("GOOG (60d out) should be filtered by horizon=14")
	}
	if _, ok := snap.PerSymbol["NVDA"]; ok {
		t.Error("NVDA (past event) should be filtered")
	}
}

func TestServiceBuildKeepsEarliestEventPerSymbol(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	fetcher := StaticFetcher{Events: []Event{
		{Symbol: "AAPL", EventDate: now.AddDate(0, 0, 10), TimeOfDay: TimeAMC, Source: "later"},
		{Symbol: "AAPL", EventDate: now.AddDate(0, 0, 2), TimeOfDay: TimeBMO, Source: "earlier"}, // wins
		{Symbol: "AAPL", EventDate: now.AddDate(0, 0, 7), TimeOfDay: TimeAMC, Source: "middle"},
	}}
	svc := NewService(fetcher, Options{Now: staticClock(now), HorizonDays: 30})
	snap := svc.Build(context.Background(), []string{"AAPL"}, "us_equity")
	if snap == nil {
		t.Fatal("expected snapshot")
	}
	got := snap.PerSymbol["AAPL"]
	if got.Source != "earlier" {
		t.Errorf("expected earliest event, got source=%q date=%v", got.Source, got.EventDate)
	}
	if got.TimeOfDay != TimeBMO {
		t.Errorf("expected TimeBMO from earliest event, got %v", got.TimeOfDay)
	}
}

func TestServiceBuildNormalisesSymbolCasingAndTimeOfDay(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	fetcher := StaticFetcher{Events: []Event{
		{Symbol: "  aapl  ", EventDate: now.AddDate(0, 0, 5), TimeOfDay: TimeOfDay("BMO"), Source: "vendor"},
		{Symbol: "msft", EventDate: now.AddDate(0, 0, 5), TimeOfDay: "GARBAGE", Source: "vendor"},
	}}
	svc := NewService(fetcher, Options{Now: staticClock(now)})
	snap := svc.Build(context.Background(), []string{"aapl", "MSFT"}, "us_equity")
	if snap == nil {
		t.Fatal("expected snapshot")
	}
	if _, ok := snap.PerSymbol["AAPL"]; !ok {
		t.Error("expected upper-cased AAPL key")
	}
	if snap.PerSymbol["AAPL"].TimeOfDay != TimeBMO {
		t.Errorf("expected canonical 'bmo', got %q", snap.PerSymbol["AAPL"].TimeOfDay)
	}
	if snap.PerSymbol["MSFT"].TimeOfDay != TimeUnknown {
		t.Errorf("expected unknown for garbage time, got %q", snap.PerSymbol["MSFT"].TimeOfDay)
	}
}

func TestServiceBuildReturnsNilOnEmptySymbols(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	svc := NewService(StaticFetcher{Events: []Event{
		{Symbol: "AAPL", EventDate: now.AddDate(0, 0, 5), TimeOfDay: TimeAMC},
	}}, Options{Now: staticClock(now)})
	if got := svc.Build(context.Background(), nil, "us_equity"); got != nil {
		t.Errorf("expected nil on empty symbols, got %+v", got)
	}
	if got := svc.Build(context.Background(), []string{"", "   "}, "us_equity"); got != nil {
		t.Errorf("expected nil on whitespace-only symbols, got %+v", got)
	}
}

func TestServiceBuildReturnsNilOnFetcherError(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	svc := NewService(errFetcher{}, Options{Now: staticClock(now)})
	if got := svc.Build(context.Background(), []string{"AAPL"}, "us_equity"); got != nil {
		t.Errorf("expected nil on fetcher error, got %+v", got)
	}
}

func TestSnapshotSortedEventsByDateThenSymbol(t *testing.T) {
	now := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	snap := &Snapshot{
		PerSymbol: map[string]Event{
			"ZZZ":  {Symbol: "ZZZ", EventDate: now.AddDate(0, 0, 3)},
			"AAA":  {Symbol: "AAA", EventDate: now.AddDate(0, 0, 5)},
			"AAA2": {Symbol: "AAA2", EventDate: now.AddDate(0, 0, 3)},
		},
	}
	got := snap.SortedEvents()
	if len(got) != 3 {
		t.Fatalf("expected 3 events, got %d", len(got))
	}
	// d=3, d=3 (AAA2 < ZZZ), d=5
	if got[0].Symbol != "AAA2" || got[1].Symbol != "ZZZ" || got[2].Symbol != "AAA" {
		t.Errorf("sort order: %+v", got)
	}
}

func TestSnapshotHasSignalRejectsNilAndEmpty(t *testing.T) {
	var nilSnap *Snapshot
	if nilSnap.HasSignal() {
		t.Error("nil snapshot must not signal")
	}
	if (&Snapshot{}).HasSignal() {
		t.Error("empty snapshot must not signal")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func TestNormaliseSymbolsTrimsUppersAndDeduplicates(t *testing.T) {
	got := normaliseSymbols([]string{"  aapl ", "AAPL", "msft", "", "MSFT"})
	want := []string{"AAPL", "MSFT"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

type errFetcher struct{}

func (errFetcher) Fetch(_ context.Context, _ FetchRequest) ([]Event, error) {
	return nil, errors.New("boom")
}
