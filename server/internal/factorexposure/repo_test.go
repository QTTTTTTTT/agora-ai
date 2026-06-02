package factorexposure

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

func newMockedRepo(t *testing.T) (*Repo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewRepo(db), mock, func() { _ = db.Close() }
}

func loadingRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"instrument_key", "factor", "asof", "loading", "source", "note", "updated_at",
	})
}

func snapshotRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"calculated_at", "factor", "net_exposure", "gross_exposure",
		"capital_pct", "holding_count", "loadings_asof",
	})
}

func TestRepo_LoadingsByInstruments_Happy(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	asof := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta("FROM instrument_factor_loadings")).
		WithArgs(pq.Array([]string{"US:AAPL", "US:MSFT"}), asof).
		WillReturnRows(loadingRows().
			AddRow("US:AAPL", "momentum", asof, 1.2, "manual", "", updated).
			AddRow("US:MSFT", "momentum", asof, 0.5, "msci", "msci_2026Q2", updated),
		)
	got, err := repo.LoadingsByInstruments(context.Background(), []string{"US:AAPL", "US:MSFT"}, asof)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got = %d rows", len(got))
	}
	r := got[LoadingKey{InstrumentKey: "US:AAPL", Factor: FactorMomentum}]
	if r.Loading != 1.2 || r.Source != LoadingSourceManual {
		t.Errorf("aapl row = %+v", r)
	}
	r2 := got[LoadingKey{InstrumentKey: "US:MSFT", Factor: FactorMomentum}]
	if r2.Loading != 0.5 || r2.Source != LoadingSourceMSCI {
		t.Errorf("msft row = %+v", r2)
	}
}

func TestRepo_LoadingsByInstruments_Empty(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	got, err := repo.LoadingsByInstruments(context.Background(), nil, time.Now())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %d entries", len(got))
	}
}

func TestRepo_UpsertLoading_RejectsInvalidFactor(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	if err := repo.UpsertLoading(context.Background(), InstrumentLoading{
		InstrumentKey: "US:AAPL", Factor: "sector", Loading: 1, AsOf: time.Now(), Source: LoadingSourceManual,
	}); err == nil {
		t.Error("expected invalid factor err")
	}
}

func TestRepo_UpsertLoading_RejectsInvalidSource(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	if err := repo.UpsertLoading(context.Background(), InstrumentLoading{
		InstrumentKey: "US:AAPL", Factor: FactorMomentum, Loading: 1, AsOf: time.Now(), Source: "lol",
	}); err == nil {
		t.Error("expected invalid source err")
	}
}

func TestRepo_UpsertLoading_RejectsLoadingOutOfRange(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	if err := repo.UpsertLoading(context.Background(), InstrumentLoading{
		InstrumentKey: "X", Factor: FactorMomentum, AsOf: time.Now(),
		Source: LoadingSourceManual, Loading: 99,
	}); err == nil {
		t.Error("expected out-of-range err")
	}
}

func TestRepo_UpsertLoading_Happy(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO instrument_factor_loadings")).
		WithArgs("US:AAPL", "momentum", sqlmock.AnyArg(), 1.2, "manual", "test").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.UpsertLoading(context.Background(), InstrumentLoading{
		InstrumentKey: "US:AAPL", Factor: FactorMomentum, Loading: 1.2,
		AsOf: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Source: LoadingSourceManual, Note: "test",
	}); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestRepo_DeleteLoading_Happy(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	asof := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM instrument_factor_loadings")).
		WithArgs("US:AAPL", "momentum", asof).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.DeleteLoading(context.Background(), "US:AAPL", FactorMomentum, asof); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestRepo_AppendSnapshot_TransactionalSixRows(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	gen := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	snap := Snapshot{
		FundID:      "f-1",
		GeneratedAt: gen,
		Exposures:   make([]PortfolioExposure, 0, len(AllFactors)),
	}
	for _, f := range AllFactors {
		snap.Exposures = append(snap.Exposures, PortfolioExposure{
			Factor:        f,
			NetExposure:   0.1,
			GrossExposure: 0.1,
			CapitalPct:    1,
			HoldingCount:  2,
			LoadingsAsOf:  gen,
		})
	}
	mock.ExpectBegin()
	for _, f := range AllFactors {
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO portfolio_factor_snapshots")).
			WithArgs("f-1", gen, string(f), 0.1, 0.1, 1.0, 2, gen).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectCommit()
	if err := repo.AppendSnapshot(context.Background(), snap); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestRepo_AppendSnapshot_RejectsEmptyFundID(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	if err := repo.AppendSnapshot(context.Background(), Snapshot{}); err == nil {
		t.Error("expected fund_id required err")
	}
}

func TestRepo_ListSnapshots_Filtered(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	t1 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("FROM portfolio_factor_snapshots")).
		WithArgs("f-1", "momentum").
		WillReturnRows(snapshotRows().AddRow(t1, "momentum", 0.5, 0.6, 0.95, 4, t1))
	got, err := repo.ListSnapshots(context.Background(), "f-1", FactorMomentum, 10)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 1 || got[0].Factor != FactorMomentum || got[0].NetExposure != 0.5 {
		t.Errorf("rows = %+v", got)
	}
}

func TestRepo_ListLoadings_FilterByFactor(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	asof := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("FROM instrument_factor_loadings")).
		WithArgs("momentum").
		WillReturnRows(loadingRows().
			AddRow("US:AAPL", "momentum", asof, 1.2, "manual", "", asof),
		)
	rows, err := repo.ListLoadings(context.Background(), ListLoadingsFilter{Factor: FactorMomentum, Limit: 10})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(rows) != 1 || rows[0].Factor != FactorMomentum || rows[0].Loading != 1.2 {
		t.Errorf("rows = %+v", rows)
	}
}

func TestRepo_ListLoadings_InvalidFactor(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	if _, err := repo.ListLoadings(context.Background(), ListLoadingsFilter{Factor: "sector"}); err == nil {
		t.Error("expected invalid factor err")
	}
}
