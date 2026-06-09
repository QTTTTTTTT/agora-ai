package repository

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newComplianceRepoTest(t *testing.T) (*ComplianceRepo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewComplianceRepo(db), mock, func() { db.Close() }
}

func TestComplianceRepoUpsertAcknowledgment(t *testing.T) {
	repo, mock, cleanup := newComplianceRepoTest(t)
	defer cleanup()

	row := AckRow{
		UserID:           "user-1",
		Surface:          "advisor",
		Mode:             "publisher",
		Locale:           "en",
		AcknowledgedText: "I understand …",
	}
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO compliance_acknowledgments`)).
		WithArgs(row.UserID, "advisor", "publisher", "en",
			row.AcknowledgedText, sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), 1).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ack-1"))

	id, err := repo.UpsertAcknowledgment(context.Background(), row)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if id != "ack-1" {
		t.Errorf("id = %q, want ack-1", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestComplianceRepoUpsertAcknowledgmentValidates(t *testing.T) {
	repo, _, cleanup := newComplianceRepoTest(t)
	defer cleanup()

	_, err := repo.UpsertAcknowledgment(context.Background(), AckRow{Surface: "advisor", Mode: "publisher"})
	if err == nil {
		t.Errorf("missing user_id should error")
	}
}

func TestComplianceRepoHasAcknowledgedFound(t *testing.T) {
	repo, mock, cleanup := newComplianceRepoTest(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta(`FROM compliance_acknowledgments`)).
		WithArgs("user-1", "publisher", 1, "advisor").
		WillReturnRows(sqlmock.NewRows([]string{"?col"}).AddRow(1))

	ok, err := repo.HasAcknowledged(context.Background(), "user-1", "advisor", "publisher", 1)
	if err != nil || !ok {
		t.Errorf("HasAcknowledged = (%v, %v), want (true, nil)", ok, err)
	}
}

func TestComplianceRepoHasAcknowledgedMissing(t *testing.T) {
	repo, mock, cleanup := newComplianceRepoTest(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta(`FROM compliance_acknowledgments`)).
		WithArgs("user-1", "publisher", 1, "advisor").
		WillReturnError(sql.ErrNoRows)

	ok, err := repo.HasAcknowledged(context.Background(), "user-1", "advisor", "publisher", 1)
	if err != nil {
		t.Fatalf("HasAcknowledged: %v", err)
	}
	if ok {
		t.Errorf("expected false for ErrNoRows")
	}
}

func TestComplianceRepoInsertPhraseViolations(t *testing.T) {
	repo, mock, cleanup := newComplianceRepoTest(t)
	defer cleanup()

	uid := sql.NullString{String: "user-1", Valid: true}
	full := sql.NullString{String: "redacted text", Valid: true}
	src := sql.NullString{String: "buffett", Valid: true}
	rows := []PhraseViolationRow{{
		UserID:         uid,
		Surface:        "advisor",
		Rule:           "we_recommend",
		OriginalPhrase: "we recommend",
		Replacement:    "this model identifies",
		FullRedacted:   full,
		SourceEntity:   sql.NullString{String: "advisor_master_report", Valid: true},
		SourceID:       src,
	}}
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO compliance_phrase_violations`)).
		WithArgs(uid, "advisor", "we_recommend", "we recommend",
			"this model identifies", full,
			sql.NullString{String: "advisor_master_report", Valid: true}, src).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.InsertPhraseViolations(context.Background(), rows); err != nil {
		t.Fatalf("insert violations: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestComplianceRepoInsertPhraseViolationsEmpty(t *testing.T) {
	repo, _, cleanup := newComplianceRepoTest(t)
	defer cleanup()
	if err := repo.InsertPhraseViolations(context.Background(), nil); err != nil {
		t.Errorf("empty insert should be no-op; got %v", err)
	}
}

func TestComplianceRepoRecentViolations(t *testing.T) {
	repo, mock, cleanup := newComplianceRepoTest(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta(`FROM compliance_phrase_violations`)).
		WithArgs(50).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "user_id", "surface", "rule", "original_phrase", "replacement",
			"full_redacted", "flagged_at", "source_entity", "source_id",
		}).AddRow("v-1", sql.NullString{String: "user-1", Valid: true}, "advisor",
			"we_recommend", "we recommend", "this model identifies",
			sql.NullString{String: "...", Valid: true},
			time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			sql.NullString{String: "advisor_master_report", Valid: true},
			sql.NullString{String: "buffett", Valid: true}))

	rows, err := repo.RecentViolations(context.Background(), 0)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "v-1" {
		t.Fatalf("unexpected rows %+v", rows)
	}
}

func TestComplianceRepoNilSafe(t *testing.T) {
	var repo *ComplianceRepo
	if _, err := repo.UpsertAcknowledgment(context.Background(), AckRow{UserID: "u", Surface: "advisor", Mode: "publisher"}); err == nil {
		t.Errorf("nil repo should error on upsert")
	} else if !errors.Is(err, err) {
		t.Errorf("error must be non-nil, got %v", err)
	}
	if _, err := repo.HasAcknowledged(context.Background(), "u", "advisor", "publisher", 1); err == nil {
		t.Errorf("nil repo should error on has-ack")
	}
}
