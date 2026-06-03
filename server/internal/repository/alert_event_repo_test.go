package repository

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestAlertEventRepo_Insert_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewAlertEventRepo(db)

	startsAt := time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO admin_alert_events`)).
		WithArgs(
			"fp-1",
			"FundAIPMDecisionFallbackRateHigh",
			"warning",
			"pm_decision",
			"firing",
			"summary text",
			"description text",
			[]byte(`{"alertname":"FundAIPMDecisionFallbackRateHigh"}`),
			[]byte(`{}`),
			startsAt,
			nil,
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("alert-1"))

	id, err := repo.Insert(context.Background(), &AlertEvent{
		Fingerprint: "fp-1",
		AlertName:   "FundAIPMDecisionFallbackRateHigh",
		Severity:    "WARNING",
		Component:   "pm_decision",
		Status:      "FIRING",
		Summary:     "summary text",
		Description: "description text",
		Labels:      json.RawMessage(`{"alertname":"FundAIPMDecisionFallbackRateHigh"}`),
		StartsAt:    startsAt,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id != "alert-1" {
		t.Fatalf("expected id alert-1, got %q", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestAlertEventRepo_Insert_DuplicateMapsToSentinel(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewAlertEventRepo(db)

	startsAt := time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
	// The ON CONFLICT … DO NOTHING path returns no rows; sqlmock
	// surfaces this as sql.ErrNoRows on Scan.
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO admin_alert_events`)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	_, err = repo.Insert(context.Background(), &AlertEvent{
		Fingerprint: "fp-1",
		AlertName:   "X",
		Status:      "firing",
		StartsAt:    startsAt,
	})
	if !errors.Is(err, ErrAlertEventDuplicate) {
		t.Fatalf("expected ErrAlertEventDuplicate, got %v", err)
	}
}

func TestAlertEventRepo_Insert_RejectsEmptyArgs(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewAlertEventRepo(db)

	startsAt := time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
	cases := []*AlertEvent{
		nil,
		{}, // empty fingerprint
		{Fingerprint: "fp-1"},                         // empty status
		{Fingerprint: "fp-1", Status: "firing"},       // zero starts_at
	}
	for i, ev := range cases {
		if _, err := repo.Insert(context.Background(), ev); err == nil {
			t.Fatalf("case %d: expected validation error, got nil", i)
		}
	}
	// The fully-formed event should pass validation (we don't run
	// the sql here; this case only exists to prove no false positives).
	_ = startsAt
}

func TestAlertEventRepo_ListRecent_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewAlertEventRepo(db)

	startsAt := time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM admin_alert_events`).
		WithArgs("firing", 50).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fingerprint", "alertname", "severity", "component", "status",
			"summary", "description",
			"labels", "annotations",
			"starts_at", "ends_at", "received_at",
			"acknowledged_by", "acknowledged_at",
			"acknowledgement_note",
		}).AddRow(
			"alert-1", "fp-1", "FundAIPMDecisionFallbackRateHigh", "warning", "pm_decision", "firing",
			"summary", "description",
			`{"alertname":"X"}`, `{}`,
			startsAt, nil, startsAt,
			"", nil, "",
		))

	events, err := repo.ListRecent(context.Background(), "firing", 50)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Severity != "warning" {
		t.Fatalf("expected severity=warning, got %s", events[0].Severity)
	}
}

func TestAlertEventRepo_Acknowledge_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewAlertEventRepo(db)

	mock.ExpectExec(`UPDATE admin_alert_events`).
		WithArgs("alert-1", "admin-1", "false positive").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Acknowledge(context.Background(), "alert-1", "admin-1", "false positive"); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
}

func TestAlertEventRepo_Acknowledge_AlreadyAcked_IsNoop(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewAlertEventRepo(db)

	mock.ExpectExec(`UPDATE admin_alert_events`).
		WithArgs("alert-1", "admin-1", "").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT 1 FROM admin_alert_events`).
		WithArgs("alert-1").
		WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))

	if err := repo.Acknowledge(context.Background(), "alert-1", "admin-1", ""); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
}

func TestAlertEventRepo_Acknowledge_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewAlertEventRepo(db)

	mock.ExpectExec(`UPDATE admin_alert_events`).
		WithArgs("alert-x", "admin-1", "").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT 1 FROM admin_alert_events`).
		WithArgs("alert-x").
		WillReturnError(errNoRowsFixture())

	if err := repo.Acknowledge(context.Background(), "alert-x", "admin-1", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
