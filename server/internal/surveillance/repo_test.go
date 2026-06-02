package surveillance

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

func TestRepo_InsertEvent_Inserted(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	t0 := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO surveillance_events")).
		WithArgs("f1", "wash_trade", "warning", "AAPL", nil,
			t0, t0.Add(time.Minute), sqlmock.AnyArg(), "summary", sqlmock.AnyArg(),
			"open", "v1", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ev-1"))

	res, err := repo.InsertEvent(context.Background(), Event{
		FundID:      "f1",
		RuleCode:    RuleWashTrade,
		Severity:    SeverityWarning,
		Symbol:      "AAPL",
		WindowStart: t0,
		WindowEnd:   t0.Add(time.Minute),
		TradeIDs:    []string{"a", "b", "c"},
		Summary:     "summary",
		Status:      StatusOpen,
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !res.Inserted || res.ID != "ev-1" {
		t.Errorf("got %+v", res)
	}
}

func TestRepo_InsertEvent_Conflict_Dedupes(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	t0 := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO surveillance_events")).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id::text FROM surveillance_events WHERE fingerprint")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ev-existing"))

	res, err := repo.InsertEvent(context.Background(), Event{
		FundID:      "f1",
		RuleCode:    RuleWashTrade,
		Severity:    SeverityWarning,
		WindowStart: t0,
		WindowEnd:   t0,
		TradeIDs:    []string{"a"},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Inserted || res.ID != "ev-existing" {
		t.Errorf("got %+v", res)
	}
}

func TestRepo_InsertEvent_RequiresFundID(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	_, err := repo.InsertEvent(context.Background(), Event{
		RuleCode: RuleWashTrade,
	})
	if err == nil {
		t.Error("expected error on missing fund_id")
	}
}

func TestRepo_UpdateStatus_BadStatus(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	if err := repo.UpdateStatus(context.Background(), UpdateStatusParams{
		ID:        "ev-1",
		NewStatus: "wat",
	}); err != ErrInvalidStatus {
		t.Errorf("err = %v, want ErrInvalidStatus", err)
	}
}

func TestRepo_UpdateStatus_NotFound(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE surveillance_events")).
		WithArgs("ev-1", "cleared", "ok", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	err := repo.UpdateStatus(context.Background(), UpdateStatusParams{
		ID:         "ev-1",
		NewStatus:  StatusCleared,
		Note:       "ok",
		ReviewedBy: "user-1",
	})
	if err != ErrEventNotFound {
		t.Errorf("err = %v, want ErrEventNotFound", err)
	}
}

func TestRepo_UpdateStatus_HappyPath(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE surveillance_events")).
		WithArgs("ev-1", "cleared", "looks ok", "user-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := repo.UpdateStatus(context.Background(), UpdateStatusParams{
		ID:         "ev-1",
		NewStatus:  StatusCleared,
		Note:       "looks ok",
		ReviewedBy: "user-1",
	}); err != nil {
		t.Errorf("err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestRepo_CreateRun(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	t0 := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO surveillance_runs")).
		WithArgs("f1", nil, "scheduled", t0, t0.Add(time.Hour),
			3, 1, 1, 0, 0, 42,
			"completed", "", "{}",
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("run-1"))

	run, err := repo.CreateRun(context.Background(), CreateRunParams{
		FundID:        "f1",
		TriggerSource: "scheduled",
		WindowStart:   t0,
		WindowEnd:     t0.Add(time.Hour),
		TradeCount:    3,
		Result: RunResult{
			CountsBySeverity: map[Severity]int{
				SeverityCritical: 1,
				SeverityWarning:  0,
				SeverityInfo:     0,
			},
		},
		DurationMS: 42,
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if run.ID != "run-1" {
		t.Errorf("id = %q", run.ID)
	}
	// Severity counts must reach the run row even when one bucket
	// is zero.
	if run.EventCountCritical != 1 {
		t.Errorf("crit count = %d", run.EventCountCritical)
	}
}
