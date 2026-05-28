package pead

import (
	"context"
	"testing"
	"time"

	"github.com/fundai/server/internal/earnings"
	"github.com/fundai/server/internal/ohlc"
)

// staticOHLC returns pre-seeded bars keyed by upper-cased symbol.
type staticOHLC struct {
	byKey map[string][]ohlc.Bar
}

func (s staticOHLC) Fetch(_ context.Context, req ohlc.FetchRequest) ([]ohlc.Bar, error) {
	bars, ok := s.byKey[req.Symbol]
	if !ok {
		return nil, nil
	}
	return bars, nil
}

// ---------------------------------------------------------------------------
// Classifier
// ---------------------------------------------------------------------------

func TestClassifyDrift_Continuing(t *testing.T) {
	// +5% surprise, +3% drift (same sign, drift < 1.5×surprise)
	got := classifyDrift(0.05, 0.03, 0.03, 1.5)
	if got != DriftStateContinuing {
		t.Errorf("expected continuing, got %q", got)
	}
}

func TestClassifyDrift_Complete(t *testing.T) {
	// +5% surprise, +10% drift (same sign, drift > 1.5×surprise)
	got := classifyDrift(0.05, 0.10, 0.03, 1.5)
	if got != DriftStateComplete {
		t.Errorf("expected complete, got %q", got)
	}
}

func TestClassifyDrift_Faded(t *testing.T) {
	// +5% surprise, -2% drift (opposite signs)
	got := classifyDrift(0.05, -0.02, 0.03, 1.5)
	if got != DriftStateFaded {
		t.Errorf("expected faded, got %q", got)
	}
}

func TestClassifyDrift_NeutralBelowSurpriseFloor(t *testing.T) {
	// +1% surprise → below 3% floor → neutral
	got := classifyDrift(0.01, 0.05, 0.03, 1.5)
	if got != DriftStateNeutral {
		t.Errorf("expected neutral, got %q", got)
	}
}

func TestClassifyDrift_NegativeContinuing(t *testing.T) {
	// -8% surprise, -3% drift (same sign, drift < 1.5×|surprise|)
	got := classifyDrift(-0.08, -0.03, 0.03, 1.5)
	if got != DriftStateContinuing {
		t.Errorf("expected continuing (negative side), got %q", got)
	}
}

// ---------------------------------------------------------------------------
// alignBars
// ---------------------------------------------------------------------------

func TestAlignBars_FindsExactEventDay(t *testing.T) {
	day := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	closes := []closeBar{
		{Time: day.AddDate(0, 0, -2), Close: 100},
		{Time: day.AddDate(0, 0, -1), Close: 101},
		{Time: day, Close: 110},
		{Time: day.AddDate(0, 0, 5), Close: 115},
	}
	entry, current := alignBars(closes, day)
	if entry != 110 {
		t.Errorf("entry close wrong: %v", entry)
	}
	if current != 115 {
		t.Errorf("current close wrong: %v", current)
	}
}

func TestAlignBars_FallsToNextTradingDay(t *testing.T) {
	// Event on Saturday, next trading day is Monday.
	day := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC) // Saturday
	closes := []closeBar{
		{Time: day.AddDate(0, 0, -1), Close: 100}, // Friday
		{Time: day.AddDate(0, 0, 2), Close: 110},  // Monday
		{Time: day.AddDate(0, 0, 5), Close: 115},
	}
	entry, current := alignBars(closes, day)
	if entry != 110 {
		t.Errorf("expected next-trading-day close 110, got %v", entry)
	}
	if current != 115 {
		t.Errorf("current close wrong: %v", current)
	}
}

func TestAlignBars_EventBeforeAnyBarReturnsFirstBar(t *testing.T) {
	day := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	closes := []closeBar{
		{Time: day.AddDate(0, 0, 5), Close: 110},
		{Time: day.AddDate(0, 0, 10), Close: 115},
	}
	entry, current := alignBars(closes, day)
	if entry != 110 {
		t.Errorf("expected first-bar close 110, got %v", entry)
	}
	if current != 115 {
		t.Errorf("current close wrong: %v", current)
	}
}

func TestAlignBars_EventAfterAllBarsReturnsZero(t *testing.T) {
	day := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	closes := []closeBar{
		{Time: day.AddDate(0, 0, -10), Close: 100},
		{Time: day.AddDate(0, 0, -5), Close: 105},
	}
	entry, current := alignBars(closes, day)
	if entry != 0 || current != 0 {
		t.Errorf("expected (0,0), got (%v,%v)", entry, current)
	}
}

// ---------------------------------------------------------------------------
// HistorySnapshot helpers (in earnings package, sanity-checked here)
// ---------------------------------------------------------------------------

func TestHistoryServiceFiltersOutOfWindowEvents(t *testing.T) {
	now := time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC)
	hs := earnings.NewHistoryService(
		earnings.StaticHistoryFetcher{
			Events: []earnings.HistoricalEvent{
				{Symbol: "RECENT", EventDate: now.AddDate(0, 0, -10), EpsActual: 1.5, EpsEstimate: 1.4, SurprisePercent: 0.071},
				{Symbol: "OLD", EventDate: now.AddDate(0, 0, -120), EpsActual: 1.5, EpsEstimate: 1.4, SurprisePercent: 0.071},
				{Symbol: "FUTURE", EventDate: now.AddDate(0, 0, 5), EpsActual: 1.5, EpsEstimate: 1.4, SurprisePercent: 0.071},
			},
		},
		earnings.HistoryOptions{
			Now:          func() time.Time { return now },
			LookbackDays: 60,
		},
	)
	snap := hs.Build(context.Background(), []string{"RECENT", "OLD", "FUTURE"}, "us_equity")
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if _, ok := snap.PerSymbol["RECENT"]; !ok {
		t.Errorf("RECENT should be in snapshot, got %+v", snap.PerSymbol)
	}
	if _, ok := snap.PerSymbol["OLD"]; ok {
		t.Errorf("OLD must be filtered out (> lookback)")
	}
	if _, ok := snap.PerSymbol["FUTURE"]; ok {
		t.Errorf("FUTURE must be filtered out (after now)")
	}
}

func TestHistoryServiceKeepsMostRecentEventPerSymbol(t *testing.T) {
	now := time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC)
	hs := earnings.NewHistoryService(
		earnings.StaticHistoryFetcher{
			Events: []earnings.HistoricalEvent{
				{Symbol: "AAPL", EventDate: now.AddDate(0, 0, -50), EpsActual: 1.0, EpsEstimate: 1.0},
				{Symbol: "AAPL", EventDate: now.AddDate(0, 0, -10), EpsActual: 1.5, EpsEstimate: 1.4, SurprisePercent: 0.071},
				{Symbol: "AAPL", EventDate: now.AddDate(0, 0, -30), EpsActual: 1.2, EpsEstimate: 1.1},
			},
		},
		earnings.HistoryOptions{
			Now:          func() time.Time { return now },
			LookbackDays: 60,
		},
	)
	snap := hs.Build(context.Background(), []string{"AAPL"}, "us_equity")
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}
	ev := snap.PerSymbol["AAPL"]
	if !ev.EventDate.Equal(now.AddDate(0, 0, -10)) {
		t.Errorf("expected most recent (10 days ago), got %v", ev.EventDate)
	}
	if ev.SurprisePercent != 0.071 {
		t.Errorf("expected surprise from most recent print, got %v", ev.SurprisePercent)
	}
}

// ---------------------------------------------------------------------------
// BuildSnapshot end-to-end
// ---------------------------------------------------------------------------

func TestNewServiceNilDependenciesReturnsNil(t *testing.T) {
	svc := NewService(nil, nil, Options{})
	if got := svc.BuildSnapshot(context.Background(), []SymbolRequest{{Symbol: "A"}}); got != nil {
		t.Errorf("expected nil snapshot, got %+v", got)
	}
	svc2 := NewService(earnings.NewHistoryService(earnings.NoopHistoryFetcher{}, earnings.HistoryOptions{}), nil, Options{})
	if got := svc2.BuildSnapshot(context.Background(), []SymbolRequest{{Symbol: "A"}}); got != nil {
		t.Errorf("nil ohlc must yield nil")
	}
}

func TestBuildSnapshotProducesContinuingState(t *testing.T) {
	now := time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC)
	eventDay := now.AddDate(0, 0, -10)
	bars := []ohlc.Bar{
		{Time: now.AddDate(0, 0, -20), Close: 100},
		{Time: now.AddDate(0, 0, -15), Close: 102},
		{Time: eventDay, Close: 105}, // entry
		{Time: now.AddDate(0, 0, -5), Close: 107},
		{Time: now, Close: 108}, // drift = (108-105)/105 ≈ 2.86%
	}
	historySvc := earnings.NewHistoryService(
		earnings.StaticHistoryFetcher{
			Events: []earnings.HistoricalEvent{
				{Symbol: "AAPL", Market: "us_equity", EventDate: eventDay, EpsActual: 1.5, EpsEstimate: 1.4, SurprisePercent: 0.071},
			},
		},
		earnings.HistoryOptions{Now: func() time.Time { return now }},
	)
	ohlcFetcher := staticOHLC{byKey: map[string][]ohlc.Bar{"AAPL": bars}}
	svc := NewService(historySvc, ohlcFetcher, Options{Now: func() time.Time { return now }})
	snap := svc.BuildSnapshot(context.Background(), []SymbolRequest{{Symbol: "AAPL", Market: "us_equity"}})
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if len(snap.Signals) != 1 {
		t.Fatalf("expected 1 signal, got %d", len(snap.Signals))
	}
	sig := snap.Signals[0]
	if sig.Symbol != "AAPL" {
		t.Errorf("symbol = %q", sig.Symbol)
	}
	if sig.DaysSinceEvent != 10 {
		t.Errorf("days = %d, want 10", sig.DaysSinceEvent)
	}
	if sig.State != DriftStateContinuing {
		t.Errorf("state = %q, want continuing", sig.State)
	}
	if sig.EntryClose != 105 || sig.CurrentClose != 108 {
		t.Errorf("entry/current = %v/%v, want 105/108", sig.EntryClose, sig.CurrentClose)
	}
}

func TestBuildSnapshotProducesFadedState(t *testing.T) {
	now := time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC)
	eventDay := now.AddDate(0, 0, -15)
	// Positive surprise but stock dropped (gap fade / deep-value setup).
	bars := []ohlc.Bar{
		{Time: now.AddDate(0, 0, -30), Close: 100},
		{Time: eventDay, Close: 110}, // entry — popped on surprise
		{Time: now.AddDate(0, 0, -5), Close: 102},
		{Time: now, Close: 100}, // drift = (100-110)/110 ≈ -9%
	}
	historySvc := earnings.NewHistoryService(
		earnings.StaticHistoryFetcher{
			Events: []earnings.HistoricalEvent{
				{Symbol: "BEAT", Market: "us_equity", EventDate: eventDay, SurprisePercent: 0.10},
			},
		},
		earnings.HistoryOptions{Now: func() time.Time { return now }},
	)
	ohlcFetcher := staticOHLC{byKey: map[string][]ohlc.Bar{"BEAT": bars}}
	svc := NewService(historySvc, ohlcFetcher, Options{Now: func() time.Time { return now }})
	snap := svc.BuildSnapshot(context.Background(), []SymbolRequest{{Symbol: "BEAT", Market: "us_equity"}})
	if snap == nil || len(snap.Signals) != 1 {
		t.Fatalf("expected 1 signal, got %+v", snap)
	}
	if snap.Signals[0].State != DriftStateFaded {
		t.Errorf("expected faded, got %q", snap.Signals[0].State)
	}
}

func TestBuildSnapshotSortsByAbsSurpriseDescending(t *testing.T) {
	now := time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC)
	eventDay := now.AddDate(0, 0, -10)
	bars := []ohlc.Bar{
		{Time: now.AddDate(0, 0, -20), Close: 100},
		{Time: eventDay, Close: 100},
		{Time: now, Close: 105},
	}
	historySvc := earnings.NewHistoryService(
		earnings.StaticHistoryFetcher{
			Events: []earnings.HistoricalEvent{
				{Symbol: "A", Market: "us_equity", EventDate: eventDay, SurprisePercent: 0.05},
				{Symbol: "B", Market: "us_equity", EventDate: eventDay, SurprisePercent: -0.12},
				{Symbol: "C", Market: "us_equity", EventDate: eventDay, SurprisePercent: 0.08},
			},
		},
		earnings.HistoryOptions{Now: func() time.Time { return now }},
	)
	ohlcFetcher := staticOHLC{byKey: map[string][]ohlc.Bar{
		"A": bars, "B": bars, "C": bars,
	}}
	svc := NewService(historySvc, ohlcFetcher, Options{Now: func() time.Time { return now }})
	snap := svc.BuildSnapshot(context.Background(), []SymbolRequest{
		{Symbol: "A", Market: "us_equity"},
		{Symbol: "B", Market: "us_equity"},
		{Symbol: "C", Market: "us_equity"},
	})
	if snap == nil {
		t.Fatal("expected snapshot")
	}
	if len(snap.Signals) != 3 {
		t.Fatalf("expected 3 signals, got %d", len(snap.Signals))
	}
	// Order should be B (|surprise|=0.12), C (0.08), A (0.05).
	want := []string{"B", "C", "A"}
	for i, w := range want {
		if snap.Signals[i].Symbol != w {
			t.Errorf("idx %d: got %q, want %q", i, snap.Signals[i].Symbol, w)
		}
	}
}

func TestBuildSnapshotHasSignalRequiresNonNeutralRow(t *testing.T) {
	now := time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC)
	eventDay := now.AddDate(0, 0, -10)
	bars := []ohlc.Bar{
		{Time: now.AddDate(0, 0, -20), Close: 100},
		{Time: eventDay, Close: 100},
		{Time: now, Close: 105},
	}
	// Tiny surprise (1%) → neutral after classifier.
	historySvc := earnings.NewHistoryService(
		earnings.StaticHistoryFetcher{
			Events: []earnings.HistoricalEvent{
				{Symbol: "TINY", Market: "us_equity", EventDate: eventDay, SurprisePercent: 0.01},
			},
		},
		earnings.HistoryOptions{Now: func() time.Time { return now }},
	)
	ohlcFetcher := staticOHLC{byKey: map[string][]ohlc.Bar{"TINY": bars}}
	svc := NewService(historySvc, ohlcFetcher, Options{Now: func() time.Time { return now }})
	snap := svc.BuildSnapshot(context.Background(), []SymbolRequest{{Symbol: "TINY", Market: "us_equity"}})
	if snap == nil {
		t.Fatal("snapshot should still build the neutral row")
	}
	if snap.HasSignal() {
		t.Errorf("HasSignal must be false on all-neutral snapshot")
	}
}

func TestBuildSnapshotEmptyRequestsReturnsNil(t *testing.T) {
	historySvc := earnings.NewHistoryService(earnings.NoopHistoryFetcher{}, earnings.HistoryOptions{})
	svc := NewService(historySvc, staticOHLC{}, Options{})
	if got := svc.BuildSnapshot(context.Background(), nil); got != nil {
		t.Errorf("empty requests must yield nil, got %+v", got)
	}
}

func TestBuildSnapshotDedupsRequests(t *testing.T) {
	now := time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC)
	eventDay := now.AddDate(0, 0, -10)
	bars := []ohlc.Bar{
		{Time: now.AddDate(0, 0, -20), Close: 100},
		{Time: eventDay, Close: 100},
		{Time: now, Close: 110},
	}
	historySvc := earnings.NewHistoryService(
		earnings.StaticHistoryFetcher{
			Events: []earnings.HistoricalEvent{
				{Symbol: "AAPL", Market: "us_equity", EventDate: eventDay, SurprisePercent: 0.05},
			},
		},
		earnings.HistoryOptions{Now: func() time.Time { return now }},
	)
	ohlcFetcher := staticOHLC{byKey: map[string][]ohlc.Bar{"AAPL": bars}}
	svc := NewService(historySvc, ohlcFetcher, Options{Now: func() time.Time { return now }})
	snap := svc.BuildSnapshot(context.Background(), []SymbolRequest{
		{Symbol: "AAPL", Market: "us_equity"},
		{Symbol: "aapl", Market: "us_equity"},
		{Symbol: "AAPL", Market: "us_equity"},
	})
	if snap == nil || len(snap.Signals) != 1 {
		t.Fatalf("expected 1 deduped signal, got %+v", snap)
	}
}
