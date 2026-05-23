package riskbudget

// Sprint B #2 contract tests for the riskbudget.Service:
//
//   - Options.withDefaults installs the production tunings (60d
//     lookback, 20-sample floor, 0.5% base R, 15% vol target, 25%
//     DD ceiling) and clamps every field so a misconfigured fund
//     config can never blow the throttle out.
//   - BuildSnapshot is fail-soft: nil service, nil DB, blank fundID,
//     too-few NAV rows all return (nil, nil).
//   - The arithmetic is correct on a known-good NAV series:
//       - VolScalar = clamp(VolTarget / RealisedVol, floor, ceil)
//       - DDScalar  = clamp(1 - DD/Ceiling, floor, 1.0)
//       - EffectiveR = Base * VolScalar * DDScalar
//   - Quiet-period series → vol scalar caps at the ceiling so
//     EffectiveR doesn't run away.
//   - Drawdown series → DD scalar floors at DDScalarFloor so the
//     fund can still trade out of a deep hole.
//   - NaN / Inf inputs (degenerate NAV transitions) are scrubbed,
//     never leak into Snapshot fields.
//   - The SQL query shape is locked so a refactor of the column
//     list surfaces here before reaching the prompt.

import (
	"context"
	"errors"
	"math"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// Production defaults — pinned so any drift in the tunings has to
// be explicit in this test.
func TestOptionsWithDefaultsProductionTunings(t *testing.T) {
	got := Options{}.withDefaults()
	want := Options{
		LookbackDays:        60,
		MinSamples:          20,
		BasePerTradeRiskPct: 0.005,
		VolTargetAnnualized: 0.15,
		VolScalarFloor:      0.5,
		VolScalarCeil:       2.0,
		DDCeilingPct:        0.25,
		DDScalarFloor:       0.4,
	}
	if got != want {
		t.Errorf("Options{}.withDefaults() = %+v, want %+v", got, want)
	}
}

// Each clamp must reject every degenerate corner. We don't unfold
// every combination — just the boundary points that matter so a
// future tweak doesn't accidentally lift a ceiling.
func TestOptionsWithDefaultsClampsBounds(t *testing.T) {
	got := Options{
		LookbackDays:        1000,
		MinSamples:          1,
		BasePerTradeRiskPct: 0.5,
		VolTargetAnnualized: 0.9,
		VolScalarFloor:      -1,
		VolScalarCeil:       -1,
		DDCeilingPct:        2,
		DDScalarFloor:       2,
	}.withDefaults()
	if got.LookbackDays != 252 {
		t.Errorf("LookbackDays clamp: got %d, want 252", got.LookbackDays)
	}
	if got.MinSamples != 5 {
		t.Errorf("MinSamples floor: got %d, want 5", got.MinSamples)
	}
	if got.BasePerTradeRiskPct != 0.05 {
		t.Errorf("BasePerTradeRiskPct ceiling: got %v, want 0.05", got.BasePerTradeRiskPct)
	}
	if got.VolTargetAnnualized != 0.40 {
		t.Errorf("VolTargetAnnualized ceiling: got %v, want 0.40", got.VolTargetAnnualized)
	}
	if got.DDCeilingPct != 0.60 {
		t.Errorf("DDCeilingPct ceiling: got %v, want 0.60", got.DDCeilingPct)
	}
	if got.DDScalarFloor != 1.0 {
		t.Errorf("DDScalarFloor ceiling: got %v, want 1.0", got.DDScalarFloor)
	}
}

// MinSamples cannot exceed LookbackDays — otherwise the
// floor would deterministically suppress the snapshot.
func TestOptionsWithDefaultsMinSamplesLEQLookback(t *testing.T) {
	got := Options{LookbackDays: 30, MinSamples: 100}.withDefaults()
	if got.MinSamples > got.LookbackDays {
		t.Errorf("MinSamples=%d > LookbackDays=%d", got.MinSamples, got.LookbackDays)
	}
}

// Nil-safety. nil service + nil DB never panic and never report
// errors — the wiring layer calls this unconditionally.
func TestBuildSnapshotNilSafe(t *testing.T) {
	var s *Service
	got, err := s.BuildSnapshot(context.Background(), "f1", time.Now())
	if got != nil || err != nil {
		t.Errorf("nil receiver: got %+v err %v, want nil,nil", got, err)
	}
	s2 := NewService(nil, Options{})
	got, err = s2.BuildSnapshot(context.Background(), "f1", time.Now())
	if got != nil || err != nil {
		t.Errorf("nil db: got %+v err %v, want nil,nil", got, err)
	}
}

// Blank fundID short-circuits before any DB call.
func TestBuildSnapshotBlankFundIDShortCircuits(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewService(db, Options{})
	got, err := s.BuildSnapshot(context.Background(), "   ", time.Now())
	if got != nil || err != nil {
		t.Errorf("blank fundID: got %+v err %v, want nil,nil", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB activity on blank fundID: %v", err)
	}
}

// Below-floor NAV row count → no snapshot. We seed 4 rows with a
// MinSamples=5 config.
func TestBuildSnapshotBelowMinSamplesReturnsNil(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{"trading_date", "nav"})
	now := time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		rows.AddRow(now.AddDate(0, 0, -i), 1.0+float64(i)*0.001)
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT trading_date, nav`)).
		WithArgs("f1", sqlmock.AnyArg()).
		WillReturnRows(rows)

	s := NewService(db, Options{LookbackDays: 10, MinSamples: 5})
	got, err := s.BuildSnapshot(context.Background(), "f1", now)
	if got != nil || err != nil {
		t.Errorf("below floor: got %+v err %v, want nil,nil", got, err)
	}
}

// Happy path with a small known-good series. We seed 6 daily NAV
// rows whose returns are constant (0% every day) and assert:
//   - RealisedVol = 0 → VolScalar caps at the ceiling
//   - Drawdown = 0   → DDScalar = 1.0
//   - EffectiveR = Base * Ceiling
func TestBuildSnapshotConstantNAVCapsAtCeiling(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{"trading_date", "nav"})
	now := time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		rows.AddRow(now.AddDate(0, 0, -5+i), 1.0)
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT trading_date, nav`)).
		WithArgs("f1", sqlmock.AnyArg()).
		WillReturnRows(rows)

	s := NewService(db, Options{LookbackDays: 10, MinSamples: 5})
	got, err := s.BuildSnapshot(context.Background(), "f1", now)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil {
		t.Fatal("expected snapshot, got nil")
	}
	if got.RealisedVolAnnualized != 0 {
		t.Errorf("constant NAV → RealisedVol = %v, want 0", got.RealisedVolAnnualized)
	}
	if got.VolScalar != 2.0 {
		t.Errorf("constant NAV → VolScalar = %v, want 2.0 (ceiling)", got.VolScalar)
	}
	if got.DDScalar != 1.0 {
		t.Errorf("constant NAV → DDScalar = %v, want 1.0", got.DDScalar)
	}
	want := 0.005 * 2.0 * 1.0
	if math.Abs(got.EffectivePerTradeRiskPct-want) > 1e-12 {
		t.Errorf("EffectivePerTradeRiskPct = %v, want %v", got.EffectivePerTradeRiskPct, want)
	}
}

// Drawdown path: NAV ramps up to a peak, then loses 20% from peak.
// With DDCeiling = 0.25 the throttle is at 1 - 0.20/0.25 = 0.2,
// clamped to the DDScalarFloor = 0.4.
func TestBuildSnapshotHeavyDrawdownFloorsAtDDFloor(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC)
	navs := []float64{1.0, 1.05, 1.10, 1.20, 1.00, 0.96} // peak=1.20, current=0.96 → DD=20%
	rows := sqlmock.NewRows([]string{"trading_date", "nav"})
	for i, n := range navs {
		rows.AddRow(now.AddDate(0, 0, -len(navs)+1+i), n)
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT trading_date, nav`)).
		WithArgs("f1", sqlmock.AnyArg()).
		WillReturnRows(rows)

	s := NewService(db, Options{LookbackDays: 10, MinSamples: 5})
	got, err := s.BuildSnapshot(context.Background(), "f1", now)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil {
		t.Fatal("expected snapshot, got nil")
	}
	if math.Abs(got.DrawdownPct-0.20) > 1e-9 {
		t.Errorf("DrawdownPct = %v, want 0.20", got.DrawdownPct)
	}
	if got.DDScalar != 0.4 {
		t.Errorf("DDScalar = %v, want 0.4 (floor)", got.DDScalar)
	}
	// PeakNAV / CurrentNAV mirror the input series — quick sanity
	// check so a refactor of the indexing surfaces here.
	if got.PeakNAV != 1.20 {
		t.Errorf("PeakNAV = %v, want 1.20", got.PeakNAV)
	}
	if got.CurrentNAV != 0.96 {
		t.Errorf("CurrentNAV = %v, want 0.96", got.CurrentNAV)
	}
}

// Non-zero realised vol path: tiny daily returns of ~10 bps. With
// VolTarget = 15% annualised the scalar is in the upper range
// — but because the realised vol is non-zero we exercise the
// non-degenerate branch (rather than the floor / ceiling clamp).
func TestBuildSnapshotComputesRealisedVolFromReturns(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC)
	// Alternating up/down 1% returns → realised daily vol ≈ 1%
	// → annualised ≈ 0.01 * sqrt(252) ≈ 0.1587.
	navs := []float64{1.00, 1.01, 1.00, 1.01, 1.00, 1.01, 1.00}
	rows := sqlmock.NewRows([]string{"trading_date", "nav"})
	for i, n := range navs {
		rows.AddRow(now.AddDate(0, 0, -len(navs)+1+i), n)
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT trading_date, nav`)).
		WithArgs("f1", sqlmock.AnyArg()).
		WillReturnRows(rows)

	s := NewService(db, Options{LookbackDays: 10, MinSamples: 5})
	got, err := s.BuildSnapshot(context.Background(), "f1", now)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil {
		t.Fatal("expected snapshot, got nil")
	}
	if got.RealisedVolAnnualized < 0.10 || got.RealisedVolAnnualized > 0.25 {
		t.Errorf("RealisedVolAnnualized = %v, want ~0.16", got.RealisedVolAnnualized)
	}
	// Vol scalar should be in the open band (between floor and
	// ceiling); the exact value depends on the realised vs target.
	if got.VolScalar <= 0.5 || got.VolScalar >= 2.0 {
		t.Errorf("VolScalar = %v should be in (0.5, 2.0)", got.VolScalar)
	}
}

// SQL errors bubble up; the wiring layer logs them but the rest of
// the PM run still proceeds.
func TestBuildSnapshotPropagatesQueryError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT trading_date, nav`)).
		WillReturnError(errors.New("boom"))

	s := NewService(db, Options{})
	got, err := s.BuildSnapshot(context.Background(), "f1", time.Now())
	if err == nil {
		t.Fatal("expected error from failed query, got nil")
	}
	if got != nil {
		t.Errorf("expected nil snapshot on error, got %+v", got)
	}
}

// Non-positive NAV rows (data quality artefact) are skipped so log
// returns stay defined. We seed two non-positive rows alongside 5
// good ones to verify the filter.
func TestBuildSnapshotSkipsNonPositiveNAVRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{"trading_date", "nav"}).
		AddRow(now.AddDate(0, 0, -6), 1.00).
		AddRow(now.AddDate(0, 0, -5), 0.0). // skipped
		AddRow(now.AddDate(0, 0, -4), 1.01).
		AddRow(now.AddDate(0, 0, -3), -1.0). // skipped
		AddRow(now.AddDate(0, 0, -2), 1.02).
		AddRow(now.AddDate(0, 0, -1), 1.03).
		AddRow(now, 1.04)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT trading_date, nav`)).
		WithArgs("f1", sqlmock.AnyArg()).
		WillReturnRows(rows)

	s := NewService(db, Options{LookbackDays: 10, MinSamples: 5})
	got, err := s.BuildSnapshot(context.Background(), "f1", now)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil {
		t.Fatal("expected snapshot from 5 valid rows, got nil")
	}
	if got.SampleSize != 5 {
		t.Errorf("SampleSize = %d, want 5", got.SampleSize)
	}
}

// stdev on small inputs is defensive — empty + single-value both
// return 0 instead of NaN (Sqrt(0/0)).
func TestStdevEdgeCases(t *testing.T) {
	if got := stdev(nil); got != 0 {
		t.Errorf("stdev(nil) = %v, want 0", got)
	}
	if got := stdev([]float64{1.0}); got != 0 {
		t.Errorf("stdev([1.0]) = %v, want 0", got)
	}
	// Known good: stdev of (1,2,3,4,5) with n-1 denominator = ~1.5811.
	got := stdev([]float64{1, 2, 3, 4, 5})
	if math.Abs(got-1.5811388300841898) > 1e-9 {
		t.Errorf("stdev([1..5]) = %v, want ~1.5811", got)
	}
}

// safe scrubs NaN / ±Inf to 0 so the prompt JSON is always valid.
func TestSafeScrubsNonFinite(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want float64
	}{
		{"normal", 0.5, 0.5},
		{"zero", 0, 0},
		{"NaN", math.NaN(), 0},
		{"+Inf", math.Inf(1), 0},
		{"-Inf", math.Inf(-1), 0},
	}
	for _, c := range cases {
		if got := safe(c.in); got != c.want {
			t.Errorf("safe(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

// clamp respects [lo, hi]. lo / hi are validated upstream by
// withDefaults; we only need to lock the corner behaviour here.
func TestClamp(t *testing.T) {
	cases := []struct {
		x, lo, hi, want float64
	}{
		{0.5, 0, 1, 0.5},
		{-1, 0, 1, 0},
		{2, 0, 1, 1},
		{0, 0, 1, 0},
		{1, 0, 1, 1},
	}
	for _, c := range cases {
		if got := clamp(c.x, c.lo, c.hi); got != c.want {
			t.Errorf("clamp(%v, %v, %v) = %v, want %v", c.x, c.lo, c.hi, got, c.want)
		}
	}
}

// Options accessor on nil receiver — same defensive guard as the
// cooldown Service.
func TestOptionsAccessor(t *testing.T) {
	var s *Service
	if got := s.Options(); got.LookbackDays != 60 {
		t.Errorf("nil receiver Options(): LookbackDays = %d, want 60", got.LookbackDays)
	}
	s2 := NewService(nil, Options{LookbackDays: 30})
	if got := s2.Options(); got.LookbackDays != 30 {
		t.Errorf("configured Options(): LookbackDays = %d, want 30", got.LookbackDays)
	}
}
