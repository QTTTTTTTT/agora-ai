package attribution

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/fundai/server/internal/repository"
)

// ---------------------------------------------------------------------------
// Stubs
// ---------------------------------------------------------------------------

type stubLots struct {
	sleeve         []repository.SleeveStat
	regime         []repository.RegimeStat
	sleeveRegime   []repository.SleeveRegimeStat
	openCount      int
	openEarliest   sql.NullTime
	openInvErr     error
	bySleeveErr    error
	byRegimeErr    error
	byCrossErr     error
	bySleeveCalled bool
	byRegimeCalled bool
	byCrossCalled  bool
	openInvCalled  bool
}

func (s *stubLots) StatsBySleeve(_ context.Context, fundID string, since time.Time) ([]repository.SleeveStat, error) {
	s.bySleeveCalled = true
	if s.bySleeveErr != nil {
		return nil, s.bySleeveErr
	}
	return s.sleeve, nil
}

func (s *stubLots) StatsByRegime(_ context.Context, fundID string, since time.Time) ([]repository.RegimeStat, error) {
	s.byRegimeCalled = true
	if s.byRegimeErr != nil {
		return nil, s.byRegimeErr
	}
	return s.regime, nil
}

func (s *stubLots) StatsBySleeveRegime(_ context.Context, fundID string, since time.Time) ([]repository.SleeveRegimeStat, error) {
	s.byCrossCalled = true
	if s.byCrossErr != nil {
		return nil, s.byCrossErr
	}
	return s.sleeveRegime, nil
}

func (s *stubLots) OpenLotInventory(_ context.Context, fundID string) (int, sql.NullTime, error) {
	s.openInvCalled = true
	if s.openInvErr != nil {
		return 0, sql.NullTime{}, s.openInvErr
	}
	return s.openCount, s.openEarliest, nil
}

type stubMemory struct {
	created []*repository.Memory
	listed  []repository.Memory
	listErr error
	createErr error
}

func (m *stubMemory) Create(_ context.Context, mem *repository.Memory) (string, error) {
	if m.createErr != nil {
		return "", m.createErr
	}
	m.created = append(m.created, mem)
	return "mem-" + mem.Title.String, nil
}

func (m *stubMemory) ListByFund(_ context.Context, fundID, layer string, limit int) ([]repository.Memory, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.listed, nil
}

// ---------------------------------------------------------------------------
// NewService
// ---------------------------------------------------------------------------

func TestNewServiceNilWhenDepsMissing(t *testing.T) {
	if NewService(nil, &stubMemory{}) != nil {
		t.Fatal("expected nil with nil lots dep")
	}
	if NewService(&stubLots{}, nil) != nil {
		t.Fatal("expected nil with nil memory dep")
	}
}

func TestNewServiceUsesInjectedClock(t *testing.T) {
	want := time.Date(2026, 5, 21, 8, 0, 0, 0, time.UTC)
	svc := NewService(&stubLots{}, &stubMemory{}, WithClock(func() time.Time { return want }))
	if got := svc.clock().UTC(); !got.Equal(want) {
		t.Fatalf("clock: got %s, want %s", got, want)
	}
}

// ---------------------------------------------------------------------------
// BuildReport
// ---------------------------------------------------------------------------

func TestBuildReportPullsAllThreeStatSlices(t *testing.T) {
	lots := &stubLots{
		sleeve:       []repository.SleeveStat{{Sleeve: "trend", TradeCount: 10}},
		regime:       []repository.RegimeStat{{Regime: "trend_up", TradeCount: 10}},
		sleeveRegime: []repository.SleeveRegimeStat{{Sleeve: "trend", Regime: "trend_up", TradeCount: 10}},
	}
	now := time.Date(2026, 5, 21, 9, 0, 0, 0, time.UTC)
	svc := NewService(lots, &stubMemory{}, WithClock(func() time.Time { return now }))

	report, err := svc.BuildReport(context.Background(), "fund-1", 30)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !lots.bySleeveCalled || !lots.byRegimeCalled || !lots.byCrossCalled {
		t.Fatalf("expected all three queries called: %+v", lots)
	}
	if report.Window.Days != 30 {
		t.Fatalf("window days: got %d", report.Window.Days)
	}
	if !report.Window.Since.Equal(now.Add(-30 * 24 * time.Hour)) {
		t.Fatalf("window since: got %s", report.Window.Since)
	}
	if len(report.BySleeve) != 1 || len(report.BySleeveRegime) != 1 {
		t.Fatalf("missing rows: %+v", report)
	}
}

func TestBuildReportFallsBackToDefaultLookback(t *testing.T) {
	lots := &stubLots{}
	now := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	svc := NewService(lots, &stubMemory{}, WithClock(func() time.Time { return now }))
	report, err := svc.BuildReport(context.Background(), "fund-1", 0)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if report.Window.Days != DefaultLookbackDays {
		t.Fatalf("expected default lookback %d, got %d", DefaultLookbackDays, report.Window.Days)
	}
}

func TestBuildReportPropagatesQueryErrors(t *testing.T) {
	lots := &stubLots{bySleeveErr: errors.New("boom")}
	svc := NewService(lots, &stubMemory{})
	if _, err := svc.BuildReport(context.Background(), "fund-1", 30); err == nil {
		t.Fatal("expected sleeve error to surface, got nil")
	}
}

func TestBuildReportRequiresFundID(t *testing.T) {
	svc := NewService(&stubLots{}, &stubMemory{})
	if _, err := svc.BuildReport(context.Background(), "  ", 30); err == nil {
		t.Fatal("expected error for empty fund_id, got nil")
	}
}

// ---------------------------------------------------------------------------
// RunAndPersist
// ---------------------------------------------------------------------------

func TestRunAndPersistWritesLessonAndReturnsIt(t *testing.T) {
	lots := &stubLots{
		sleeveRegime: []repository.SleeveRegimeStat{
			{Sleeve: "trend", Regime: "chop", TradeCount: 10, WinCount: 2, LossCount: 8, TotalPnL: -200, WinRate: 0.2, AvgPnLPct: -0.03},
		},
	}
	mem := &stubMemory{}
	now := time.Date(2026, 5, 21, 9, 0, 0, 0, time.UTC)
	svc := NewService(lots, mem, WithClock(func() time.Time { return now }))

	_, persisted, err := svc.RunAndPersist(context.Background(), "fund-1", "", 30)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(persisted) != 1 {
		t.Fatalf("expected 1 lesson persisted, got %d", len(persisted))
	}
	if len(mem.created) != 1 {
		t.Fatalf("expected 1 memory row created, got %d", len(mem.created))
	}
	row := mem.created[0]
	if row.Layer != MemoryLayer {
		t.Fatalf("layer: got %q, want %q", row.Layer, MemoryLayer)
	}
	if row.FundID != "fund-1" {
		t.Fatalf("fund_id: got %q", row.FundID)
	}
	if !row.TradingDate.Valid || row.TradingDate.Time.Day() != 21 {
		t.Fatalf("trading_date: got %+v", row.TradingDate)
	}
	if len(row.Tags) == 0 {
		t.Fatal("tags should be populated")
	}
}

func TestRunAndPersistIsIdempotent(t *testing.T) {
	lots := &stubLots{
		sleeveRegime: []repository.SleeveRegimeStat{
			{Sleeve: "trend", Regime: "chop", TradeCount: 10, WinCount: 2, LossCount: 8, TotalPnL: -200, WinRate: 0.2},
		},
	}
	now := time.Date(2026, 5, 21, 9, 0, 0, 0, time.UTC)
	tradingDate := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	// Simulate an existing memory row with the same tag set for today.
	// Tags must match what GenerateLessons → buildThrottleLesson would
	// emit for this row (10 trades, win 0.2, PnL -200): the THROTTLE
	// tier with "throttle" replacing the legacy "loser" tag. tagKey()
	// in service.go sorts alphabetically before joining, so order in
	// this fixture doesn't matter.
	mem := &stubMemory{
		listed: []repository.Memory{
			{
				FundID:      "fund-1",
				Layer:       MemoryLayer,
				Tags:        []string{"throttle", "sleeve:trend", "regime:chop"},
				TradingDate: sql.NullTime{Time: tradingDate, Valid: true},
			},
		},
	}
	svc := NewService(lots, mem, WithClock(func() time.Time { return now }))
	_, persisted, err := svc.RunAndPersist(context.Background(), "fund-1", "", 30)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(persisted) != 0 {
		t.Fatalf("expected 0 persisted (dedupe), got %d", len(persisted))
	}
	if len(mem.created) != 0 {
		t.Fatalf("expected 0 created (dedupe), got %d", len(mem.created))
	}
}

func TestRunAndPersistInsufficientDataStillWritesOneLesson(t *testing.T) {
	lots := &stubLots{} // no closed lots
	mem := &stubMemory{}
	now := time.Date(2026, 5, 21, 9, 0, 0, 0, time.UTC)
	svc := NewService(lots, mem, WithClock(func() time.Time { return now }))

	_, persisted, err := svc.RunAndPersist(context.Background(), "fund-1", "", 30)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(persisted) != 1 || persisted[0].Kind != LessonInsufficientData {
		t.Fatalf("expected one insufficient_data lesson, got %+v", persisted)
	}
	if len(mem.created) != 1 {
		t.Fatalf("expected 1 memory row, got %d", len(mem.created))
	}
}

func TestRunAndPersistMemoryWriteFailureDoesNotAbortRemaining(t *testing.T) {
	lots := &stubLots{
		sleeveRegime: []repository.SleeveRegimeStat{
			{Sleeve: "trend", Regime: "chop", TradeCount: 10, WinCount: 2, LossCount: 8, TotalPnL: -200, WinRate: 0.2},
			{Sleeve: "mean_reversion", Regime: "trend_down", TradeCount: 10, WinCount: 1, LossCount: 9, TotalPnL: -300, WinRate: 0.1},
		},
	}
	mem := &stubMemory{createErr: errors.New("db down")}
	now := time.Date(2026, 5, 21, 9, 0, 0, 0, time.UTC)
	svc := NewService(lots, mem, WithClock(func() time.Time { return now }))
	_, persisted, err := svc.RunAndPersist(context.Background(), "fund-1", "", 30)
	if err != nil {
		t.Fatalf("run: returned err %v, expected nil (soft fail)", err)
	}
	if len(persisted) != 0 {
		t.Fatalf("expected 0 persisted (writes failed), got %d", len(persisted))
	}
}

// TestRunAndPersistDedupeIgnoresOlderTradingDate confirms a
// lesson with the same tags but a different trading_date does NOT
// suppress today's lesson.
func TestRunAndPersistDedupeIgnoresOlderTradingDate(t *testing.T) {
	lots := &stubLots{
		sleeveRegime: []repository.SleeveRegimeStat{
			{Sleeve: "trend", Regime: "chop", TradeCount: 10, WinCount: 2, LossCount: 8, TotalPnL: -200, WinRate: 0.2},
		},
	}
	mem := &stubMemory{
		listed: []repository.Memory{
			{
				FundID: "fund-1", Layer: MemoryLayer,
				Tags:        []string{"loser", "sleeve:trend", "regime:chop"},
				TradingDate: sql.NullTime{Time: time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC), Valid: true},
			},
		},
	}
	now := time.Date(2026, 5, 21, 9, 0, 0, 0, time.UTC)
	svc := NewService(lots, mem, WithClock(func() time.Time { return now }))
	_, persisted, err := svc.RunAndPersist(context.Background(), "fund-1", "", 30)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(persisted) != 1 {
		t.Fatalf("yesterday's same-tag memory should not dedupe today's, got %d", len(persisted))
	}
}
