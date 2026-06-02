package recon

import (
	"context"
	"database/sql"
	"errors"
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

// IngestStatement happy path: pre-check (no row) + INSERT broker_statements
// + child inserts + COMMIT.
func TestRepo_IngestStatement_HappyPath(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()

	date := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// Pre-check returns no row.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id::text, fund_id::text")).
		WithArgs("fund-1", date, "mock", sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)

	mock.ExpectBegin()

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO broker_statements")).
		WithArgs("fund-1", nil, date, "mock", sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), nil).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("stmt-1"))

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO broker_statement_positions")).
		WithArgs("stmt-1", "AAPL", 100.0, 175.5, 0.0, "USD", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO broker_statement_cash")).
		WithArgs("stmt-1", "USD", 5000.0, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	mock.ExpectCommit()

	got, err := repo.IngestStatement(context.Background(), IngestParams{
		FundID:        "fund-1",
		StatementDate: date,
		Source:        SourceMock,
		Positions:     []StatementPosition{{Symbol: "aapl", Quantity: 100, AvgCost: 175.5, Currency: "usd"}},
		Cash:          []StatementCash{{Currency: "usd", Balance: 5000}},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.ID != "stmt-1" {
		t.Errorf("id = %q", got.ID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// IngestStatement returns ErrAlreadyIngested when the pre-check
// finds a row with the same hash.
func TestRepo_IngestStatement_DuplicateHash(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	date := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	rows := sqlmock.NewRows([]string{
		"id", "fund_id", "broker_link_id", "statement_date", "source",
		"payload_hash", "ingested_at", "ingested_by", "status",
	}).AddRow("stmt-existing", "fund-1", "", date, "mock",
		"any-hash", time.Now().UTC(), "", "pending")

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id::text, fund_id::text")).
		WithArgs("fund-1", date, "mock", sqlmock.AnyArg()).
		WillReturnRows(rows)

	got, err := repo.IngestStatement(context.Background(), IngestParams{
		FundID:        "fund-1",
		StatementDate: date,
		Source:        SourceMock,
	})
	if !errors.Is(err, ErrAlreadyIngested) {
		t.Errorf("err = %v, want ErrAlreadyIngested", err)
	}
	if got == nil || got.ID != "stmt-existing" {
		t.Errorf("got = %+v", got)
	}
}

// CreateRun happy path with no breaks.
func TestRepo_CreateRun_NoBreaks(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO reconciliation_runs")).
		WithArgs("fund-1", "stmt-1", sqlmock.AnyArg(), nil, "manual", "completed",
			0, 0, 0, 0, "{}",
			sqlmock.AnyArg(), sqlmock.AnyArg(), "").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("run-1"))
	mock.ExpectCommit()

	run, err := repo.CreateRun(context.Background(), CreateRunParams{
		FundID:      "fund-1",
		StatementID: "stmt-1",
		RunDate:     time.Now().UTC(),
		Result:      Result{Counts: map[Severity]int{}},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if run.ID != "run-1" || run.BreakCountTotal != 0 {
		t.Errorf("got %+v", run)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// CreateRun with breaks: one INSERT per break + counts derived from
// Result.Counts.
func TestRepo_CreateRun_WithBreaks(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO reconciliation_runs")).
		WithArgs("fund-1", "stmt-1", sqlmock.AnyArg(), nil, "scheduled", "completed",
			2, 1, 1, 0, "{}",
			sqlmock.AnyArg(), sqlmock.AnyArg(), "").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("run-1"))

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO reconciliation_breaks")).
		WithArgs("run-1", "fund-1", "position_quantity_mismatch", "critical",
			"AAPL", "USD",
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO reconciliation_breaks")).
		WithArgs("run-1", "fund-1", "cash_balance_mismatch", "warning",
			nil, "USD",
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	mock.ExpectCommit()

	res := Result{
		Breaks: []Break{
			{Type: BreakPositionQuantityMismatch, Severity: SeverityCritical, Symbol: "AAPL", Currency: "USD"},
			{Type: BreakCashBalanceMismatch, Severity: SeverityWarning, Currency: "USD"},
		},
		Counts: map[Severity]int{SeverityCritical: 1, SeverityWarning: 1},
	}
	run, err := repo.CreateRun(context.Background(), CreateRunParams{
		FundID:        "fund-1",
		StatementID:   "stmt-1",
		RunDate:       time.Now().UTC(),
		TriggerSource: "scheduled",
		Result:        res,
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if run.BreakCountCritical != 1 || run.BreakCountWarning != 1 {
		t.Errorf("counts wrong: %+v", run)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// ResolveBreak rejects an unknown status.
func TestRepo_ResolveBreak_BadStatus(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	if _, err := repo.ResolveBreak(context.Background(), ResolveBreakParams{
		ID: "x", NewStatus: "nope",
	}); err == nil || !contains(err.Error(), "invalid status") {
		t.Errorf("err = %v", err)
	}
}

// computePayloadHash is deterministic regardless of input order.
func TestComputePayloadHash_OrderIndependent(t *testing.T) {
	date := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	a := IngestParams{
		FundID: "fund-1", StatementDate: date,
		Positions: []StatementPosition{
			{Symbol: "AAPL", Quantity: 100, AvgCost: 175},
			{Symbol: "TSLA", Quantity: 10, AvgCost: 250},
		},
	}
	b := IngestParams{
		FundID: "fund-1", StatementDate: date,
		Positions: []StatementPosition{
			{Symbol: "tsla", Quantity: 10, AvgCost: 250},
			{Symbol: " AAPL ", Quantity: 100, AvgCost: 175},
		},
	}
	if computePayloadHash(a) != computePayloadHash(b) {
		t.Error("hash should be order- and case-independent")
	}
}

// computePayloadHash differs when content differs.
func TestComputePayloadHash_ContentSensitive(t *testing.T) {
	date := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	a := IngestParams{FundID: "fund-1", StatementDate: date,
		Positions: []StatementPosition{{Symbol: "AAPL", Quantity: 100}}}
	b := IngestParams{FundID: "fund-1", StatementDate: date,
		Positions: []StatementPosition{{Symbol: "AAPL", Quantity: 101}}}
	if computePayloadHash(a) == computePayloadHash(b) {
		t.Error("hash should differ on content change")
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
