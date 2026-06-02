package marketimpact

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newMockedRepo(t *testing.T) (*Repo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewRepo(db), mock, func() { _ = db.Close() }
}

func TestRepo_GetByKey_NotFoundReturnsNil(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("FROM instrument_liquidity")).
		WithArgs("AAPL.US").
		WillReturnError(sql.ErrNoRows)
	got, err := repo.GetByKey(context.Background(), "AAPL.US")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestRepo_GetByKey_Happy(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	cal := now.Add(-24 * time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("FROM instrument_liquidity")).
		WithArgs("AAPL.US").
		WillReturnRows(sqlmock.NewRows([]string{
			"instrument_key", "symbol", "market", "asset_class",
			"adv_shares", "adv_notional", "adv_window_days",
			"daily_volatility", "impact_coefficient", "impact_exponent",
			"min_slippage_bps", "max_slippage_bps",
			"last_calibrated_at", "calibration_source", "note", "updated_at",
		}).AddRow(
			"AAPL.US", "AAPL", "US", "equity",
			float64(50_000_000), float64(10_000_000_000), 20,
			0.02, 1.0, 0.5,
			1.0, 200.0,
			cal, "manual", "", now,
		))
	got, err := repo.GetByKey(context.Background(), "AAPL.US")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got == nil {
		t.Fatal("got nil")
	}
	if got.Symbol != "AAPL" || got.ADVShares == nil || *got.ADVShares != 50_000_000 {
		t.Errorf("unexpected row: %+v", got)
	}
	if got.LastCalibratedAt == nil || !got.LastCalibratedAt.Equal(cal) {
		t.Errorf("LastCalibratedAt = %v", got.LastCalibratedAt)
	}
}

func TestRepo_GetByKey_RejectsBlankKey(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	if _, err := repo.GetByKey(context.Background(), "  "); err == nil {
		t.Error("expected validation error")
	}
}

func TestRepo_List_Filtered(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta("FROM instrument_liquidity WHERE market = $1 AND asset_class = $2")).
		WithArgs("US", "equity", 200, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"instrument_key", "symbol", "market", "asset_class",
			"adv_shares", "adv_notional", "adv_window_days",
			"daily_volatility", "impact_coefficient", "impact_exponent",
			"min_slippage_bps", "max_slippage_bps",
			"last_calibrated_at", "calibration_source", "note", "updated_at",
		}).AddRow(
			"AAPL.US", "AAPL", "US", "equity",
			float64(50_000_000), nil, 20,
			0.02, 1.0, 0.5,
			1.0, 200.0,
			nil, "manual", "", now,
		))
	got, err := repo.List(context.Background(), ListFilter{Market: "US", AssetClass: "equity"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 1 || got[0].Symbol != "AAPL" {
		t.Errorf("unexpected list: %+v", got)
	}
}

func TestRepo_Upsert_RequiresFields(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	if _, err := repo.Upsert(context.Background(), UpsertParams{Symbol: "X", Market: "US"}); err == nil {
		t.Error("expected key validation error")
	}
	if _, err := repo.Upsert(context.Background(), UpsertParams{InstrumentKey: "X", Market: "US"}); err == nil {
		t.Error("expected symbol validation error")
	}
	if _, err := repo.Upsert(context.Background(), UpsertParams{InstrumentKey: "X", Symbol: "X"}); err == nil {
		t.Error("expected market validation error")
	}
}

func TestRepo_Upsert_RejectsBadSource(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	_, err := repo.Upsert(context.Background(), UpsertParams{
		InstrumentKey:     "AAPL.US",
		Symbol:            "AAPL",
		Market:            "US",
		CalibrationSource: "guess",
	})
	if err == nil {
		t.Error("expected validation error")
	}
}

func TestRepo_Upsert_Happy(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	adv := 50_000_000.0
	sigma := 0.02
	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO instrument_liquidity")).
		WithArgs(
			"AAPL.US", "AAPL", "US", "equity",
			adv, nil, nil,
			sigma, nil, nil,
			nil, nil,
			nil, "manual", "test",
			sqlmock.AnyArg(), nil,
		).
		WillReturnRows(sqlmock.NewRows([]string{
			"instrument_key", "symbol", "market", "asset_class",
			"adv_shares", "adv_notional", "adv_window_days",
			"daily_volatility", "impact_coefficient", "impact_exponent",
			"min_slippage_bps", "max_slippage_bps",
			"last_calibrated_at", "calibration_source", "note", "updated_at",
		}).AddRow(
			"AAPL.US", "AAPL", "US", "equity",
			adv, nil, 20,
			sigma, 1.0, 0.5,
			1.0, 500.0,
			nil, "manual", "test", now,
		))
	got, err := repo.Upsert(context.Background(), UpsertParams{
		InstrumentKey:   "AAPL.US",
		Symbol:          "AAPL",
		Market:          "US",
		ADVShares:       &adv,
		DailyVolatility: &sigma,
		Note:            "test",
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got == nil || got.ImpactExponent != 0.5 {
		t.Errorf("unexpected row: %+v", got)
	}
}

func TestRepo_Delete_Happy(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM instrument_liquidity")).
		WithArgs("X.US").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Delete(context.Background(), "X.US"); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestRepo_Delete_NotFound(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM instrument_liquidity")).
		WithArgs("X.US").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := repo.Delete(context.Background(), "X.US"); err == nil {
		t.Error("expected ErrNoRows")
	}
}
