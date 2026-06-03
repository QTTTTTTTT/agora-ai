package repository

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func newHealthMockRepo(t *testing.T) (*ProviderHealthHistoryRepo, sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	return NewProviderHealthHistoryRepo(db), mock, db
}

func TestProviderHealthHistoryRepo_Insert_HappyPath(t *testing.T) {
	repo, mock, db := newHealthMockRepo(t)
	defer db.Close()
	pid := uuid.New()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO platform_llm_provider_health_history")).
		WithArgs(pid, "openai", "openai-prod",
			sqlmock.AnyArg(), true, 123, 200, sql.NullString{}, sql.NullString{Valid: true, String: "gpt-4o"}).
		WillReturnResult(sqlmock.NewResult(1, 1))
	err := repo.Insert(context.Background(), ProviderHealthRow{
		ProviderID: pid,
		Provider:   "OPENAI", // verifying lowercase normalisation
		Label:      " openai-prod ",
		OK:         true,
		LatencyMS:  123,
		HTTPStatus: 200,
		ModelName:  sql.NullString{Valid: true, String: "gpt-4o"},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestProviderHealthHistoryRepo_Insert_RequiresProviderID(t *testing.T) {
	repo, _, db := newHealthMockRepo(t)
	defer db.Close()
	err := repo.Insert(context.Background(), ProviderHealthRow{
		ProviderID: uuid.Nil,
		OK:         true,
	})
	if err == nil {
		t.Fatalf("expected error on nil provider id")
	}
}

func TestProviderHealthHistoryRepo_Insert_ClampsNegativeLatency(t *testing.T) {
	repo, mock, db := newHealthMockRepo(t)
	defer db.Close()
	pid := uuid.New()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO platform_llm_provider_health_history")).
		WithArgs(pid, "claude", "claude-prod",
			sqlmock.AnyArg(), false, 0, 500, sql.NullString{Valid: true, String: "boom"}, sql.NullString{}).
		WillReturnResult(sqlmock.NewResult(1, 1))
	err := repo.Insert(context.Background(), ProviderHealthRow{
		ProviderID: pid,
		Provider:   "claude",
		Label:      "claude-prod",
		OK:         false,
		LatencyMS:  -10, // clamped to 0
		HTTPStatus: 500,
		Message:    sql.NullString{Valid: true, String: "boom"},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestProviderHealthHistoryRepo_ListRecent_ByProvider(t *testing.T) {
	repo, mock, db := newHealthMockRepo(t)
	defer db.Close()
	pid := uuid.New()
	since := time.Now().Add(-2 * time.Hour)
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta("WHERE provider_id =")).
		WithArgs(pid, since, 50).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "provider_id", "provider", "label", "checked_at",
			"ok", "latency_ms", "http_status", "message", "model_name",
		}).
			AddRow(uuid.New(), pid, "openai", "openai-prod", now,
				true, 120, 200, nil, "gpt-4o").
			AddRow(uuid.New(), pid, "openai", "openai-prod", now.Add(-5*time.Minute),
				false, 0, 502, "upstream", nil))
	rows, err := repo.ListRecent(context.Background(), pid, since, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].OK != true || rows[1].OK != false {
		t.Fatalf("unexpected ok values: %+v", rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestProviderHealthHistoryRepo_ListRecent_AllProviders(t *testing.T) {
	repo, mock, db := newHealthMockRepo(t)
	defer db.Close()
	since := time.Now().Add(-1 * time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE checked_at >=")).
		WithArgs(since, 1000).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "provider_id", "provider", "label", "checked_at",
			"ok", "latency_ms", "http_status", "message", "model_name",
		}))
	rows, err := repo.ListRecent(context.Background(), uuid.Nil, since, 0) // 0 → limit 1000
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected empty result, got %d", len(rows))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestProviderHealthHistoryRepo_SummariseByProvider(t *testing.T) {
	repo, mock, db := newHealthMockRepo(t)
	defer db.Close()
	since := time.Now().Add(-24 * time.Hour)
	pid := uuid.New()
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta("PERCENTILE_CONT(0.50)")).
		WithArgs(since).
		WillReturnRows(sqlmock.NewRows([]string{
			"provider_id", "provider", "label", "checks", "successes", "failures",
			"p50", "p95", "p_max", "last_checked_at", "last_ok",
		}).
			AddRow(pid, "openai", "openai-prod", 288, 280, 8, 120, 350, 1200, now, true))
	out, err := repo.SummariseByProvider(context.Background(), since)
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 summary row, got %d", len(out))
	}
	if out[0].Checks != 288 || out[0].Successes != 280 || out[0].Failures != 8 {
		t.Fatalf("unexpected counters: %+v", out[0])
	}
	if out[0].LatencyP50 != 120 || out[0].LatencyP95 != 350 || out[0].LatencyMax != 1200 {
		t.Fatalf("unexpected latency percentiles: %+v", out[0])
	}
	if out[0].SuccessRate() <= 0.95 || out[0].SuccessRate() > 1 {
		t.Fatalf("success rate looks wrong: %f", out[0].SuccessRate())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestProviderHealthHistoryRepo_DeleteOlderThan(t *testing.T) {
	repo, mock, db := newHealthMockRepo(t)
	defer db.Close()
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM platform_llm_provider_health_history WHERE checked_at <")).
		WithArgs(cutoff).
		WillReturnResult(sqlmock.NewResult(0, 42))
	n, err := repo.DeleteOlderThan(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if n != 42 {
		t.Fatalf("expected 42 rows deleted, got %d", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestNewProviderHealthHistoryRepo_NilDB(t *testing.T) {
	repo := NewProviderHealthHistoryRepo(nil)
	if repo != nil {
		t.Fatalf("expected nil repo on nil db")
	}
}

func TestProviderHealthSummary_SuccessRate_NoChecks(t *testing.T) {
	s := ProviderHealthSummary{Checks: 0}
	if s.SuccessRate() != 0 {
		t.Fatalf("empty window should report 0, got %f", s.SuccessRate())
	}
}
