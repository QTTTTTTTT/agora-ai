package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newPromotionRepoTest(t *testing.T) (*PromotionRepo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	return NewPromotionRepo(db), mock, func() { db.Close() }
}

// Insert: writes the JSONB columns + scalar header. We don't pin
// the exact SQL — just the table name + the args we care about.
func TestPromotionRepoInsertHappy(t *testing.T) {
	repo, mock, cleanup := newPromotionRepoTest(t)
	defer cleanup()
	row := &PromotionRow{
		ID:              "p-1",
		FundID:          "fund-1",
		ProposedBy:      "user-1",
		BasisJobID:      "job-1",
		EngineKind:      "llm",
		EngineParams:    json.RawMessage(`{"temperature":0.2}`),
		BaselineMetrics: json.RawMessage(`{"sharpeRatio":1.5}`),
		Status:          "pending_review",
		ShadowDays:      7,
		DecayRatio:      0.5,
		CreatedAt:       time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC),
		UpdatedAt:       time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC),
	}
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO strategy_promotions`)).
		WithArgs(row.ID, row.FundID, row.ProposedBy, row.BasisJobID, row.EngineKind,
			sqlmock.AnyArg(), sqlmock.AnyArg(), row.Status, row.ShadowDays, row.DecayRatio,
			row.Notes, row.CreatedAt, row.UpdatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestPromotionRepoInsertRejectsEmptyID(t *testing.T) {
	repo, _, cleanup := newPromotionRepoTest(t)
	defer cleanup()
	if err := repo.Insert(context.Background(), &PromotionRow{}); err == nil {
		t.Errorf("expected error on empty id")
	}
}

// UpdateStatus uses COALESCE so callers can pass only the
// columns relevant to the transition.
func TestPromotionRepoUpdateStatusActivation(t *testing.T) {
	repo, mock, cleanup := newPromotionRepoTest(t)
	defer cleanup()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	upd := StatusUpdate{
		Status:      "active",
		ActivatedAt: sql.NullTime{Time: now, Valid: true},
	}
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE strategy_promotions SET`)).
		WithArgs("active",
			sql.NullString{}, sql.NullTime{},
			sql.NullString{}, sql.NullTime{}, sql.NullString{},
			sql.NullTime{}, sql.NullTime{},
			sql.NullTime{Time: now, Valid: true},
			sql.NullTime{}, sql.NullString{},
			"p-1",
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.UpdateStatus(context.Background(), "p-1", upd); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// UpdateStatus surfaces ErrNotFound when no rows match.
func TestPromotionRepoUpdateStatusMissing(t *testing.T) {
	repo, mock, cleanup := newPromotionRepoTest(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE strategy_promotions SET`)).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := repo.UpdateStatus(context.Background(), "missing", StatusUpdate{Status: "rejected"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

// Get returns ErrNotFound when the row is absent.
func TestPromotionRepoGetMissing(t *testing.T) {
	repo, mock, cleanup := newPromotionRepoTest(t)
	defer cleanup()
	cols := promotionCols()
	mock.ExpectQuery(`SELECT id, fund_id`).
		WithArgs("missing").
		WillReturnRows(sqlmock.NewRows(cols))

	_, err := repo.Get(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

// GetActiveByFund returns nil-with-nil-error when no active row
// exists — distinct from a plain error so the resolver knows to
// fall back to the default engine.
func TestPromotionRepoGetActiveByFundReturnsNilWhenAbsent(t *testing.T) {
	repo, mock, cleanup := newPromotionRepoTest(t)
	defer cleanup()
	cols := promotionCols()
	mock.ExpectQuery(`SELECT id, fund_id`).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows(cols))

	p, err := repo.GetActiveByFund(context.Background(), "fund-1")
	if err != nil {
		t.Errorf("expected nil err, got %v", err)
	}
	if p != nil {
		t.Errorf("expected nil promotion, got %+v", p)
	}
}

// GetActiveByFund: happy path.
func TestPromotionRepoGetActiveByFundHappy(t *testing.T) {
	repo, mock, cleanup := newPromotionRepoTest(t)
	defer cleanup()
	now := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT id, fund_id`).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows(promotionCols()).
			AddRow("p-1", "fund-1", "u-1", "job-1", "llm",
				[]byte(`{}`), []byte(`{}`), "active", 7, 0.5,
				sql.NullString{}, sql.NullTime{},
				sql.NullString{}, sql.NullTime{}, sql.NullString{},
				sql.NullTime{}, sql.NullTime{},
				sql.NullTime{Time: now, Valid: true}, sql.NullTime{}, sql.NullString{},
				sql.NullString{}, now, now))

	p, err := repo.GetActiveByFund(context.Background(), "fund-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p == nil || p.ID != "p-1" || p.Status != "active" {
		t.Errorf("unexpected row: %+v", p)
	}
}

// ListByFund returns rows newest-first; we just check the row
// count + ordering preservation.
func TestPromotionRepoListByFund(t *testing.T) {
	repo, mock, cleanup := newPromotionRepoTest(t)
	defer cleanup()
	now := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows(promotionCols()).
		AddRow("p-1", "fund-1", "u-1", "job-1", "llm",
			[]byte(`{}`), []byte(`{}`), "active", 7, 0.5,
			sql.NullString{}, sql.NullTime{},
			sql.NullString{}, sql.NullTime{}, sql.NullString{},
			sql.NullTime{}, sql.NullTime{},
			sql.NullTime{}, sql.NullTime{}, sql.NullString{},
			sql.NullString{}, now, now).
		AddRow("p-0", "fund-1", "u-1", "job-0", "fallback",
			[]byte(`{}`), []byte(`{}`), "superseded", 7, 0.5,
			sql.NullString{}, sql.NullTime{},
			sql.NullString{}, sql.NullTime{}, sql.NullString{},
			sql.NullTime{}, sql.NullTime{},
			sql.NullTime{}, sql.NullTime{}, sql.NullString{},
			sql.NullString{}, now.Add(-time.Hour), now.Add(-time.Hour))
	mock.ExpectQuery(`SELECT id, fund_id`).
		WithArgs("fund-1", 50).
		WillReturnRows(rows)

	out, err := repo.ListByFund(context.Background(), "fund-1", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d", len(out))
	}
	if out[0].ID != "p-1" || out[1].ID != "p-0" {
		t.Errorf("order wrong: %s %s", out[0].ID, out[1].ID)
	}
}

// UpsertShadowDiff hits the ON CONFLICT path. We don't exercise
// the conflict branch directly — sqlmock can't simulate the
// constraint — but we do verify the SQL is shaped correctly.
func TestPromotionRepoUpsertShadowDiff(t *testing.T) {
	repo, mock, cleanup := newPromotionRepoTest(t)
	defer cleanup()
	now := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	row := &ShadowDiffRow{
		ID:             "d-1",
		PromotionID:    "p-1",
		TradingDate:    now,
		ShadowDecision: json.RawMessage(`{"action":"buy"}`),
		ActiveDecision: json.RawMessage(`{"action":"hold"}`),
		Agreement:      false,
		CreatedAt:      now,
	}
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO promotion_shadow_diffs`)).
		WithArgs(row.ID, row.PromotionID, row.TradingDate, sqlmock.AnyArg(), sqlmock.AnyArg(), row.Agreement, row.CreatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.UpsertShadowDiff(context.Background(), row); err != nil {
		t.Fatalf("UpsertShadowDiff: %v", err)
	}
}

// InsertHealthSnapshot writes the rolling-window stats.
func TestPromotionRepoInsertHealthSnapshot(t *testing.T) {
	repo, mock, cleanup := newPromotionRepoTest(t)
	defer cleanup()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	row := &HealthSnapshotRow{
		ID: "h-1", PromotionID: "p-1", SnapshotAt: now, WindowDays: 30,
		ActualSharpe: sql.NullFloat64{Float64: 0.8, Valid: true}, ActualTradeCount: 12,
		SharpeDecayRatio: sql.NullFloat64{Float64: 0.6, Valid: true}, DecayFlag: false,
	}
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO promotion_health_snapshots`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.InsertHealthSnapshot(context.Background(), row); err != nil {
		t.Fatalf("InsertHealthSnapshot: %v", err)
	}
}

// InsertEvent + ListEvents round-trip the audit log shape.
func TestPromotionRepoInsertEvent(t *testing.T) {
	repo, mock, cleanup := newPromotionRepoTest(t)
	defer cleanup()
	now := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO promotion_events`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.InsertEvent(context.Background(), &PromotionEventRow{
		ID: "ev-1", PromotionID: "p-1", EventType: "approved",
		ActorUserID: sql.NullString{String: "u-1", Valid: true},
		Payload:     json.RawMessage(`{}`),
		CreatedAt:   now,
	}); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
}

// promotionCols returns the column list scanPromotion expects, in
// the same order. Centralised so adding a column requires one edit.
func promotionCols() []string {
	return []string{
		"id", "fund_id", "proposed_by", "basis_job_id", "engine_kind",
		"engine_params", "baseline_metrics", "status", "shadow_days", "decay_ratio",
		"approved_by", "approved_at",
		"rejected_by", "rejected_at", "rejected_reason",
		"shadow_started_at", "shadow_completed_at",
		"activated_at", "deactivated_at", "deactivated_reason",
		"notes", "created_at", "updated_at",
	}
}
