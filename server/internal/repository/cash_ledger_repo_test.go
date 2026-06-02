package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// cashLedgerColumns mirrors cashLedgerSelectColumns so the mock
// rowsets line up with scanCashLedgerEntry.
var cashLedgerColumns = []string{
	"id", "fund_id", "posted_at", "trading_date", "entry_type",
	"amount", "currency", "trade_id", "plan_id", "plan_action_id",
	"corp_action_id", "broker_link_id", "description", "metadata",
	"created_by", "idempotency_key", "created_at", "updated_at",
}

func newMockedCashLedgerRepo(t *testing.T) (*CashLedgerRepo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewCashLedgerRepo(db), mock, func() { _ = db.Close() }
}

func TestCashLedger_Append_HappyPath(t *testing.T) {
	repo, mock, cleanup := newMockedCashLedgerRepo(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO cash_ledger`)).
		WithArgs(
			"fund-1",                              // fund_id
			sqlmock.AnyArg(),                      // posted_at (zero → NULL → coalesce now)
			sqlmock.AnyArg(),                      // trading_date
			"trade_buy_notional",                  // entry_type
			-1000.50,                              // amount
			"USD",                                 // currency
			"trade-1",                             // trade_id
			"",                                    // plan_id
			"",                                    // plan_action_id
			"",                                    // corp_action_id
			"",                                    // broker_link_id
			"buy AAPL @150",                       // description
			sqlmock.AnyArg(),                      // metadata
			"",                                    // created_by
			"trade:trade-1:notional",              // idempotency_key
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ledger-1"))

	id, err := repo.Append(context.Background(), AppendParams{
		FundID:         "fund-1",
		EntryType:      CashEntryTradeBuyNotional,
		Amount:         -1000.50,
		Currency:       "usd", // lowercase → uppercased
		TradeID:        "trade-1",
		Description:    "buy AAPL @150",
		IdempotencyKey: "trade:trade-1:notional",
		Metadata:       map[string]any{"symbol": "AAPL"},
	})
	if err != nil {
		t.Fatalf("Append err = %v", err)
	}
	if id != "ledger-1" {
		t.Errorf("id = %q, want ledger-1", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestCashLedger_Append_RejectsZeroAmount(t *testing.T) {
	repo, _, cleanup := newMockedCashLedgerRepo(t)
	defer cleanup()
	_, err := repo.Append(context.Background(), AppendParams{
		FundID:    "fund-1",
		EntryType: CashEntryTradeBuyNotional,
		Amount:    0,
	})
	if err == nil {
		t.Fatal("expected zero-amount error, got nil")
	}
}

func TestCashLedger_Append_RejectsUnknownType(t *testing.T) {
	repo, _, cleanup := newMockedCashLedgerRepo(t)
	defer cleanup()
	_, err := repo.Append(context.Background(), AppendParams{
		FundID:    "fund-1",
		EntryType: "fake_type",
		Amount:    -1,
	})
	if err == nil {
		t.Fatal("expected unknown-type error, got nil")
	}
}

func TestCashLedger_Append_IdempotentConflictReturnsExisting(t *testing.T) {
	repo, mock, cleanup := newMockedCashLedgerRepo(t)
	defer cleanup()

	// First INSERT: ON CONFLICT DO NOTHING → no rows returned →
	// driver returns sql.ErrNoRows on Scan. We then SELECT to
	// recover the existing id.
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO cash_ledger`)).
		WillReturnRows(sqlmock.NewRows([]string{"id"})) // empty
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM cash_ledger`)).
		WithArgs("fund-1", "key-x").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ledger-existing"))

	id, err := repo.Append(context.Background(), AppendParams{
		FundID:         "fund-1",
		EntryType:      CashEntryDividendCash,
		Amount:         42,
		IdempotencyKey: "key-x",
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if id != "ledger-existing" {
		t.Errorf("id = %q, want ledger-existing", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestCashLedger_BalanceByFund(t *testing.T) {
	repo, mock, cleanup := newMockedCashLedgerRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(SUM(amount), 0) FROM cash_ledger WHERE fund_id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(123456.78))
	bal, err := repo.BalanceByFund(context.Background(), "fund-1", BalanceByFundParams{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if bal != 123456.78 {
		t.Errorf("balance = %v, want 123456.78", bal)
	}
}

func TestCashLedger_BalanceByFund_RangeBound(t *testing.T) {
	repo, mock, cleanup := newMockedCashLedgerRepo(t)
	defer cleanup()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(SUM(amount), 0) FROM cash_ledger WHERE fund_id = $1 AND posted_at >= $2 AND posted_at < $3`)).
		WithArgs("fund-1", from, to).
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(50.0))
	bal, err := repo.BalanceByFund(context.Background(), "fund-1", BalanceByFundParams{From: from, To: to})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if bal != 50.0 {
		t.Errorf("balance = %v, want 50.0", bal)
	}
}

func TestCashLedger_ListByFund(t *testing.T) {
	repo, mock, cleanup := newMockedCashLedgerRepo(t)
	defer cleanup()
	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta(`FROM cash_ledger WHERE fund_id = $1`)).
		WithArgs("fund-1", 100).
		WillReturnRows(sqlmock.NewRows(cashLedgerColumns).
			AddRow(
				"l-1", "fund-1", now, nil, "trade_buy_notional",
				-100.0, "USD", "t-1", nil, nil,
				nil, nil, "buy", []byte(`{}`),
				nil, "trade:t-1:notional", now, now,
			).
			AddRow(
				"l-2", "fund-1", now, nil, "dividend_cash",
				5.0, "USD", nil, nil, nil,
				"corp-1", nil, "div", []byte(`{}`),
				nil, nil, now, now,
			),
		)

	got, err := repo.ListByFund(context.Background(), "fund-1", ListByFundParams{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].EntryType != "trade_buy_notional" {
		t.Errorf("first entry type = %q", got[0].EntryType)
	}
	if got[1].Amount != 5.0 {
		t.Errorf("second amount = %v, want 5.0", got[1].Amount)
	}
}

func TestCashLedger_SubtotalByEntryType(t *testing.T) {
	repo, mock, cleanup := newMockedCashLedgerRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT entry_type, SUM(amount) FROM cash_ledger WHERE fund_id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"entry_type", "sum"}).
			AddRow("trade_buy_notional", -100000.0).
			AddRow("trade_buy_commission", -120.5).
			AddRow("dividend_cash", 50.0),
		)
	got, err := repo.SubtotalByEntryType(context.Background(), "fund-1", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got["trade_buy_notional"] != -100000.0 {
		t.Errorf("notional = %v", got["trade_buy_notional"])
	}
	if got["dividend_cash"] != 50.0 {
		t.Errorf("dividend = %v", got["dividend_cash"])
	}
}

func TestCashLedger_SubtotalByCurrency(t *testing.T) {
	repo, mock, cleanup := newMockedCashLedgerRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT currency, SUM(amount) FROM cash_ledger WHERE fund_id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"currency", "sum"}).
			AddRow("USD", 1000.0).
			AddRow("CNY", -500.0),
		)
	got, err := repo.SubtotalByCurrency(context.Background(), "fund-1", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got["USD"] != 1000.0 || got["CNY"] != -500.0 {
		t.Errorf("got = %+v", got)
	}
}

func TestCashLedger_SubtotalByCurrency_RangeBound(t *testing.T) {
	repo, mock, cleanup := newMockedCashLedgerRepo(t)
	defer cleanup()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT currency, SUM(amount) FROM cash_ledger WHERE fund_id = $1 AND posted_at >= $2 AND posted_at < $3 GROUP BY currency`)).
		WithArgs("fund-1", from, to).
		WillReturnRows(sqlmock.NewRows([]string{"currency", "sum"}).AddRow("HKD", 200.0))
	got, err := repo.SubtotalByCurrency(context.Background(), "fund-1", from, to)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got["HKD"] != 200.0 {
		t.Errorf("got = %+v", got)
	}
}

func TestCashLedger_AppendTx_NilTx(t *testing.T) {
	repo, _, cleanup := newMockedCashLedgerRepo(t)
	defer cleanup()
	_, err := repo.AppendTx(context.Background(), nil, AppendParams{
		FundID:    "fund-1",
		EntryType: CashEntryDividendCash,
		Amount:    10,
	})
	if err == nil {
		t.Fatal("expected nil-tx error")
	}
}
