package brinson

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newMockRepo(t *testing.T) (*Repo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewRepo(db), mock, func() { _ = db.Close() }
}

func TestRepo_GetLatestComposition_HappyPath(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	now := time.Now()
	asof := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery("FROM brinson_benchmark_compositions").
		WithArgs("spx", "asset_class").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "benchmark_id", "bucket_dimension", "asof", "buckets", "note",
			"created_by", "created_at", "updated_at",
		}).AddRow("c1", "spx", "asset_class", asof,
			[]byte(`[{"key":"equity","weight":1.0,"return_pct":0.10}]`),
			"", nil, now, now))
	row, err := r.GetLatestComposition(context.Background(), "spx", DimAssetClass)
	if err != nil {
		t.Fatal(err)
	}
	if row.ID != "c1" || row.BenchmarkID != "spx" || len(row.Buckets) != 1 {
		t.Errorf("unexpected: %+v", row)
	}
}

func TestRepo_GetLatestComposition_NotFound(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	mock.ExpectQuery("FROM brinson_benchmark_compositions").
		WillReturnError(errors.New("sql: no rows in result set"))
	_, err := r.GetLatestComposition(context.Background(), "spx", DimAssetClass)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRepo_ListCompositions(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	now := time.Now()
	asof := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery("FROM brinson_benchmark_compositions").
		WithArgs("spx", "asset_class", 200).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "benchmark_id", "bucket_dimension", "asof", "buckets", "note",
			"created_by", "created_at", "updated_at",
		}).AddRow("c1", "spx", "asset_class", asof,
			[]byte(`[]`), "", nil, now, now))
	got, err := r.ListCompositions(context.Background(), ListCompositionsParams{
		BenchmarkID: "spx", Dimension: DimAssetClass,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("len = %d", len(got))
	}
}

func TestRepo_UpsertComposition_RejectsBadComposition(t *testing.T) {
	r, _, done := newMockRepo(t)
	defer done()
	row := CompositionRow{
		BenchmarkID: "spx",
		Dimension:   DimAssetClass,
		AsOf:        time.Now(),
		Buckets:     []Bucket{{Key: "", Weight: 1, ReturnPct: 0}}, // empty key
	}
	_, err := r.UpsertComposition(context.Background(), row, "")
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestRepo_UpsertComposition_HappyPath(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	now := time.Now()
	asof := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery("INSERT INTO brinson_benchmark_compositions").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "benchmark_id", "bucket_dimension", "asof", "buckets", "note",
			"created_by", "created_at", "updated_at",
		}).AddRow("c1", "spx", "asset_class", asof,
			[]byte(`[{"key":"equity","weight":1.0,"return_pct":0.10}]`),
			"", nil, now, now))
	out, err := r.UpsertComposition(context.Background(), CompositionRow{
		BenchmarkID: "spx",
		Dimension:   DimAssetClass,
		AsOf:        asof,
		Buckets:     []Bucket{{Key: "equity", Weight: 1.0, ReturnPct: 0.10}},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if out.ID != "c1" {
		t.Errorf("got %+v", out)
	}
}

func TestRepo_DeleteComposition_NotFound(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	mock.ExpectExec("DELETE FROM brinson_benchmark_compositions").
		WillReturnResult(sqlmock.NewResult(0, 0))
	err := r.DeleteComposition(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestRepo_AppendSnapshot_HappyPath(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	mock.ExpectExec("INSERT INTO brinson_attribution_snapshots").
		WillReturnResult(sqlmock.NewResult(1, 1))
	err := r.AppendSnapshot(context.Background(), Result{
		BenchmarkID:      "spx",
		Dimension:        DimAssetClass,
		GeneratedAt:      time.Now(),
		PortfolioReturn:  0.10,
		BenchmarkReturn:  0.08,
		ActiveReturn:     0.02,
		AllocationTotal:  0.005,
		SelectionTotal:   0.008,
		InteractionTotal: 0.007,
		BucketCount:      2,
		Buckets:          []BucketAttribution{},
	}, "f1", "c1")
	if err != nil {
		t.Fatalf("AppendSnapshot: %v", err)
	}
}

func TestRepo_AppendSnapshot_RejectsMissingIds(t *testing.T) {
	r, _, done := newMockRepo(t)
	defer done()
	if err := r.AppendSnapshot(context.Background(), Result{BenchmarkID: "spx"}, "", "c1"); err == nil {
		t.Error("expected error for missing fund_id")
	}
	if err := r.AppendSnapshot(context.Background(), Result{}, "f1", "c1"); err == nil {
		t.Error("expected error for missing benchmark_id")
	}
	if err := r.AppendSnapshot(context.Background(), Result{BenchmarkID: "spx"}, "f1", ""); err == nil {
		t.Error("expected error for missing composition_id")
	}
}

func TestRepo_ListSnapshots_HappyPath(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	now := time.Now()
	mock.ExpectQuery("FROM brinson_attribution_snapshots").
		WithArgs("f1", "spx", 30).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "benchmark_id", "bucket_dimension", "composition_id",
			"calculated_at",
			"allocation_total", "selection_total", "interaction_total",
			"active_return", "portfolio_return", "benchmark_return",
			"bucket_count", "bucket_details",
		}).AddRow(int64(1), "f1", "spx", "asset_class", "c1", now,
			0.005, 0.008, 0.003, 0.016, 0.096, 0.080, 2, []byte(`[]`)))
	got, err := r.ListSnapshots(context.Background(), ListSnapshotsParams{
		FundID: "f1", BenchmarkID: "spx", Limit: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ActiveReturn != 0.016 {
		t.Errorf("got %+v", got)
	}
}
