package modelab

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// errNoRowsFixture is a tiny helper for sqlmock tests that need to
// simulate a "row not found" path on a follow-up SELECT.
func errNoRowsFixture() error { return sql.ErrNoRows }

func TestDraftRepo_UpsertPending_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewDraftRepo(db)

	rec := &Recommendation{
		ExperimentID:        "11111111-1111-1111-1111-111111111111",
		RecommendedArmIndex: 1,
		RecommendedArmLabel: "anthropic/claude-opus",
		PrimaryArmIndex:     0,
		PrimaryArmLabel:     "openai/gpt-4o",
		StreakDays:          7,
		WindowFrom:          time.Now().UTC().Add(-7 * 24 * time.Hour),
		WindowTo:            time.Now().UTC(),
		Criteria:            Criteria{}.FilledDefaults(),
		ReportSnapshot:      json.RawMessage(`{"foo":"bar"}`),
	}

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE model_ab_promotion_drafts`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`INSERT INTO model_ab_promotion_drafts`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("draft-1"))
	mock.ExpectCommit()

	id, fresh, err := repo.UpsertPending(context.Background(), rec)
	if err != nil {
		t.Fatalf("UpsertPending: %v", err)
	}
	if id != "draft-1" {
		t.Fatalf("expected draft-1, got %q", id)
	}
	if !fresh {
		t.Fatalf("expected fresh=true (no previous pending), got false")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestDraftRepo_UpsertPending_SupersedesPrevious(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewDraftRepo(db)

	rec := &Recommendation{
		ExperimentID:        "11111111-1111-1111-1111-111111111111",
		RecommendedArmIndex: 1,
		RecommendedArmLabel: "anthropic/claude-opus",
		PrimaryArmIndex:     0,
		PrimaryArmLabel:     "openai/gpt-4o",
		StreakDays:          7,
	}

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE model_ab_promotion_drafts`).
		WillReturnResult(sqlmock.NewResult(0, 1)) // one row was pending
	mock.ExpectQuery(`INSERT INTO model_ab_promotion_drafts`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("draft-2"))
	mock.ExpectCommit()

	_, fresh, err := repo.UpsertPending(context.Background(), rec)
	if err != nil {
		t.Fatalf("UpsertPending: %v", err)
	}
	if fresh {
		t.Fatalf("expected fresh=false when superseding, got true")
	}
}

func TestDraftRepo_Apply_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewDraftRepo(db)

	mock.ExpectExec(`UPDATE model_ab_promotion_drafts`).
		WithArgs("draft-1", "admin-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Apply(context.Background(), "draft-1", "admin-1"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func TestDraftRepo_Apply_RejectsNonPending(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewDraftRepo(db)

	mock.ExpectExec(`UPDATE model_ab_promotion_drafts`).
		WithArgs("draft-1", "admin-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT id::text, experiment_id::text`).
		WithArgs("draft-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "experiment_id",
			"recommended_arm_index", "recommended_arm_label",
			"primary_arm_index", "primary_arm_label",
			"streak_days", "evaluated_at", "window_from", "window_to",
			"criteria_payload", "report_snapshot",
			"status", "applied_by", "applied_at",
			"rejection_reason", "created_at",
		}).AddRow(
			"draft-1", "exp-1",
			1, "anthropic/claude-opus",
			0, "openai/gpt-4o",
			7, time.Now(), nil, nil,
			"{}", "{}",
			"applied", "admin-other", time.Now(),
			"", time.Now(),
		))

	err = repo.Apply(context.Background(), "draft-1", "admin-1")
	if err == nil {
		t.Fatalf("expected error for non-pending draft, got nil")
	}
}

func TestDraftRepo_Apply_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewDraftRepo(db)

	mock.ExpectExec(`UPDATE model_ab_promotion_drafts`).
		WithArgs("draft-x", "admin-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT id::text, experiment_id::text`).
		WithArgs("draft-x").
		WillReturnError(errNoRowsFixture())

	if err := repo.Apply(context.Background(), "draft-x", "admin-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDraftRepo_Reject_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewDraftRepo(db)

	mock.ExpectExec(`UPDATE model_ab_promotion_drafts`).
		WithArgs("draft-1", "admin-1", "we still trust primary").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Reject(context.Background(), "draft-1", "admin-1", "we still trust primary"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
}
