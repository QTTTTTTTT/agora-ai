package drawdown

import (
	"context"
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

func TestRepo_UpsertTier_HappyPath(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO drawdown_policies")).
		WithArgs("f1", 1, -0.05, "trim_proportional", 0.25, 24, false, nil).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := repo.UpsertTier(context.Background(), "f1", Tier{
		Tier:          1,
		DDPct:         -0.05,
		Action:        ActionTrimProportional,
		TrimRatio:     0.25,
		CooldownHours: 24,
	}); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestRepo_UpsertTier_RejectsBadDD(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	if err := repo.UpsertTier(context.Background(), "f1", Tier{
		Tier:   1,
		DDPct:  0.05, // positive — must be negative
		Action: ActionTrimProportional,
	}); err == nil {
		t.Error("expected dd_pct validation error")
	}
}

func TestRepo_UpsertTier_RejectsBadTier(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	if err := repo.UpsertTier(context.Background(), "f1", Tier{
		Tier: 6,
		DDPct: -0.05, Action: ActionTrimProportional,
	}); err != ErrInvalidTier {
		t.Errorf("err = %v, want ErrInvalidTier", err)
	}
}

func TestRepo_UpsertTier_RejectsUnknownAction(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	if err := repo.UpsertTier(context.Background(), "f1", Tier{
		Tier: 1, DDPct: -0.05, Action: "wat",
	}); err == nil {
		t.Error("expected action validation error")
	}
}

func TestRepo_UpsertTier_ClampsTrimRatio(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	// Pass trim_ratio=2; expect clamp to 1.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO drawdown_policies")).
		WithArgs("f1", 1, -0.05, "trim_proportional", float64(1), 24, false, nil).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := repo.UpsertTier(context.Background(), "f1", Tier{
		Tier: 1, DDPct: -0.05, Action: ActionTrimProportional,
		TrimRatio: 2,
		CooldownHours: 24,
	}); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestRepo_GetPolicy_Empty(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("FROM drawdown_policies")).
		WithArgs("f1").
		WillReturnRows(sqlmock.NewRows([]string{"tier", "dd_pct", "action", "trim_ratio", "cooldown_hours", "auto_execute", "note"}))
	got, err := repo.GetPolicy(context.Background(), "f1")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.FundID != "f1" || len(got.Tiers) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestRepo_GetPolicy_HappyPath(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("FROM drawdown_policies")).
		WithArgs("f1").
		WillReturnRows(sqlmock.NewRows([]string{"tier", "dd_pct", "action", "trim_ratio", "cooldown_hours", "auto_execute", "note"}).
			AddRow(1, -0.05, "trim_proportional", 0.25, 24, false, "").
			AddRow(2, -0.10, "flatten", 0, 24, true, "panic mode"))
	got, err := repo.GetPolicy(context.Background(), "f1")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got.Tiers) != 2 {
		t.Fatalf("tiers = %+v", got.Tiers)
	}
	if got.Tiers[1].Action != ActionFlatten || !got.Tiers[1].AutoExecute {
		t.Errorf("tier 2 = %+v", got.Tiers[1])
	}
}

func TestRepo_InsertEvent_HappyPath(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	now := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO drawdown_events")).
		WithArgs("f1", 2, -0.10, float64(100), float64(90),
			"trim_proportional", sqlmock.AnyArg(), "proposed",
			sqlmock.AnyArg(),
			now, "v1", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ev-1"))

	id, err := repo.InsertEvent(context.Background(), BreachEvent{
		FundID:       "f1",
		Tier:         2,
		CurrentDDPct: -0.10,
		PeakNAV:      100,
		CurrentNAV:   90,
		Action:       ActionTrimProportional,
		TrimPlan: []TrimPlanItem{
			{Symbol: "AAPL", Side: "sell", Quantity: 25, Reason: "trim_proportional"},
		},
		DetectedAt:      now,
		DetectorVersion: "v1",
	}, "")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if id != "ev-1" {
		t.Errorf("id = %q", id)
	}
}

func TestRepo_UpdateStatus_NotFound(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE drawdown_events")).
		WithArgs("ev-1", "dismissed", "fp", "u-1", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := repo.UpdateStatus(context.Background(), UpdateStatusParams{
		ID: "ev-1", NewStatus: StatusDismissed, Note: "fp", ReviewedBy: "u-1",
	}); err != ErrEventNotFound {
		t.Errorf("err = %v, want ErrEventNotFound", err)
	}
}

func TestRepo_UpdateStatus_BadStatus(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	if err := repo.UpdateStatus(context.Background(), UpdateStatusParams{
		ID: "ev-1", NewStatus: "wat",
	}); err != ErrInvalidStatus {
		t.Errorf("err = %v", err)
	}
}

func TestRepo_LastFiredAtForFund(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta("FROM drawdown_events")).
		WithArgs("f1", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"tier", "max"}).
			AddRow(1, now.Add(-2*time.Hour)).
			AddRow(2, now.Add(-30*time.Minute)))
	got, err := repo.LastFiredAtForFund(context.Background(), "f1", 24*time.Hour)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 2 || got[2].IsZero() {
		t.Errorf("got %+v", got)
	}
}

func TestRepo_DeleteTier(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM drawdown_policies")).
		WithArgs("f1", 2).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.DeleteTier(context.Background(), "f1", 2); err != nil {
		t.Errorf("err = %v", err)
	}
}
