package lockup

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

func recordRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "fund_id", "instrument_key", "symbol",
		"locked_qty", "locked_until", "lockup_reason",
		"source_lot_id", "note",
		"released_at", "released_reason", "released_by",
		"created_by", "created_at", "updated_at",
	})
}

func TestRepo_ListActiveFor_Happy(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("FROM position_lockups")).
		WithArgs("f1", "AAPL.US", now).
		WillReturnRows(recordRows().AddRow(
			"id1", "f1", "AAPL.US", "AAPL",
			float64(100), until, "ipo",
			nil, "",
			nil, "", nil,
			nil, now, now,
		))
	got, err := repo.ListActiveFor(context.Background(), "f1", "AAPL.US", now)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 1 || got[0].ID != "id1" || got[0].LockedQty != 100 {
		t.Errorf("unexpected rows: %+v", got)
	}
}

func TestRepo_ListActiveFor_RequiresInputs(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	if _, err := repo.ListActiveFor(context.Background(), "", "X", time.Now()); err == nil {
		t.Error("expected fund_id required err")
	}
	if _, err := repo.ListActiveFor(context.Background(), "f", "", time.Now()); err == nil {
		t.Error("expected instrument_key required err")
	}
}

func TestRepo_List_StatusActive(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	// Match "released_at IS NULL AND locked_until > $1" branch.
	mock.ExpectQuery(regexp.QuoteMeta("released_at IS NULL AND locked_until > $1")).
		WithArgs(now, 200, 0).
		WillReturnRows(recordRows().AddRow(
			"id1", "f1", "AAPL.US", "AAPL",
			float64(100), until, "ipo",
			nil, "",
			nil, "", nil,
			nil, now, now,
		))
	got, err := repo.List(context.Background(), ListFilter{Status: "active", AsOf: now})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d rows", len(got))
	}
}

func TestRepo_GetByID_NotFoundReturnsNil(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("FROM position_lockups")).
		WithArgs("missing").
		WillReturnError(sql.ErrNoRows)
	got, err := repo.GetByID(context.Background(), "missing")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestRepo_Create_Happy(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	now := time.Now().UTC()
	until := now.Add(90 * 24 * time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO position_lockups")).
		WithArgs("f1", "AAPL.US", "AAPL", float64(100), sqlmock.AnyArg(), "ipo", nil, "n", nil).
		WillReturnRows(recordRows().AddRow(
			"id1", "f1", "AAPL.US", "AAPL",
			float64(100), until, "ipo",
			nil, "n",
			nil, "", nil,
			nil, now, now,
		))
	got, err := repo.Create(context.Background(), CreateParams{
		FundID:        "f1",
		InstrumentKey: "AAPL.US",
		Symbol:        "AAPL",
		LockedQty:     100,
		LockedUntil:   until,
		Reason:        "ipo",
		Note:          "n",
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got == nil || got.ID != "id1" {
		t.Errorf("unexpected: %+v", got)
	}
}

func TestRepo_Create_Validates(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	now := time.Now().UTC()
	cases := []struct {
		name string
		p    CreateParams
	}{
		{"missing fund", CreateParams{InstrumentKey: "X", Symbol: "X", LockedQty: 1, LockedUntil: now.Add(time.Hour)}},
		{"missing key", CreateParams{FundID: "f", Symbol: "X", LockedQty: 1, LockedUntil: now.Add(time.Hour)}},
		{"missing symbol", CreateParams{FundID: "f", InstrumentKey: "X", LockedQty: 1, LockedUntil: now.Add(time.Hour)}},
		{"qty <= 0", CreateParams{FundID: "f", InstrumentKey: "X", Symbol: "X", LockedQty: 0, LockedUntil: now.Add(time.Hour)}},
		{"missing until", CreateParams{FundID: "f", InstrumentKey: "X", Symbol: "X", LockedQty: 1}},
		{"until in past", CreateParams{FundID: "f", InstrumentKey: "X", Symbol: "X", LockedQty: 1, LockedUntil: now.Add(-24 * time.Hour)}},
		{"bad reason", CreateParams{FundID: "f", InstrumentKey: "X", Symbol: "X", LockedQty: 1, LockedUntil: now.Add(time.Hour), Reason: "wat"}},
	}
	for _, c := range cases {
		if _, err := repo.Create(context.Background(), c.p); err == nil {
			t.Errorf("%s: expected validation err", c.name)
		}
	}
}

func TestRepo_Update_Happy(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	now := time.Now().UTC()
	until := now.Add(180 * 24 * time.Hour)
	newQty := 200.0
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE position_lockups")).
		WithArgs("id1", newQty, nil, nil, nil).
		WillReturnRows(recordRows().AddRow(
			"id1", "f1", "AAPL.US", "AAPL",
			newQty, until, "ipo",
			nil, "",
			nil, "", nil,
			nil, now, now,
		))
	got, err := repo.Update(context.Background(), UpdateParams{
		ID: "id1", LockedQty: &newQty,
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got == nil || got.LockedQty != 200 {
		t.Errorf("unexpected: %+v", got)
	}
}

func TestRepo_Update_RequiresID(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	if _, err := repo.Update(context.Background(), UpdateParams{}); err == nil {
		t.Error("expected id required err")
	}
}

func TestRepo_Update_RejectsBadReason(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	bad := "wat"
	if _, err := repo.Update(context.Background(), UpdateParams{ID: "x", Reason: &bad}); err == nil {
		t.Error("expected bad reason err")
	}
}

func TestRepo_Release_Happy(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	now := time.Now().UTC()
	until := now.Add(90 * 24 * time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE position_lockups")).
		WithArgs("id1", "manual", "u-1").
		WillReturnRows(recordRows().AddRow(
			"id1", "f1", "AAPL.US", "AAPL",
			float64(100), until, "ipo",
			nil, "",
			now, "manual", "u-1",
			nil, now, now,
		))
	got, err := repo.Release(context.Background(), "id1", "manual", "u-1")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got == nil || got.ReleasedAt == nil {
		t.Errorf("expected released, got %+v", got)
	}
}

func TestRepo_Release_RequiresReason(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	if _, err := repo.Release(context.Background(), "id1", "", "u-1"); err == nil {
		t.Error("expected reason required err")
	}
}

func TestRepo_Delete_NotFound(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM position_lockups")).
		WithArgs("missing").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := repo.Delete(context.Background(), "missing"); err == nil {
		t.Error("expected sql.ErrNoRows")
	}
}
