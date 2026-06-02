package corpaction

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestApplyEvent_HappyPath_TenSongSi is the regression test pinned to
// the production incident on 2026-05-29: the OCS Selection fund held
// 289 shares of 688195 (腾景科技) at cost_price=335.20, the company
// emitted a 10送4 + 派 0.164 元/股 event, and the position quote
// refresher mark-to-market'd current_price down to 238.74 — leaving
// holding_positions.unrealized_pnl reading -27,876.94, a phantom 41%
// loss with no trading involvement. The applier must:
//
//   - turn 289 shares into 404.6 (× 1.4)
//   - turn cost_price 335.20 into 239.4286 (÷ 1.4)
//   - recompute market_value = 404.6 × 238.74 = 96,594.204
//   - recompute unrealized_pnl ≈ -282 (real residual intraday move)
//   - record cash_credit = 289 × 0.164 = 47.396 for audit
//   - persist a corp_action_applications PK row
func TestApplyEvent_HappyPath_TenSongSi(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	evt := Event{
		ID:            "evt-1",
		InstrumentKey: "SSE:688195",
		ExDate:        time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC),
		ActionType:    "combined",
		SplitRatio:    1.4,
		CashDividend:  0.164,
		Source:        "manual",
	}
	fundID := "fund-ocs"

	mock.ExpectBegin()
	// Idempotency probe — no prior application yet.
	mock.ExpectQuery(regexp.QuoteMeta("FROM corp_action_applications")).
		WithArgs(evt.ID, fundID).
		WillReturnError(errNoRowsForTest)
	// Lock pre-state.
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WithArgs(fundID, evt.InstrumentKey).
		WillReturnRows(sqlmock.NewRows([]string{"quantity", "cost_price", "current_price", "market_value", "unrealized_pnl"}).
			AddRow(289.0, 335.20, 238.74, 68996.86, -27876.94))
	// 289 * 1.4 = 404.6   shares
	// 335.20 / 1.4 = 239.42857142857... → round8 = 239.42857143
	// 404.6 * 238.74 = 96594.204 (round4)
	// 404.6 * (238.74 - 239.42857143) = -278.596 (round4)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE holding_positions")).
		WithArgs(fundID, evt.InstrumentKey, 404.6, 1.4, 1.0, 239.42857143, 96594.204, -278.596).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE position_lots")).
		WithArgs(fundID, evt.InstrumentKey, 1.4).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Cash credit must post to funds.current_capital BEFORE the
	// application audit row lands. cash_credit = 289 × 0.164 = 47.396.
	mock.ExpectExec(regexp.QuoteMeta("UPDATE funds")).
		WithArgs(fundID, 47.396).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// P1-1 — cash_ledger row co-commits inside the same tx.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO cash_ledger")).
		WithArgs(fundID, 47.396, evt.ID, sqlmock.AnyArg(), sqlmock.AnyArg(), "corp:"+evt.ID+":"+fundID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// cash_credit = 289 × 0.164 = 47.396
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO corp_action_applications")).
		WithArgs(evt.ID, fundID, 289.0, 404.6, 335.20, 239.42857143, 47.396).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	got, err := ApplyEvent(context.Background(), db, evt, fundID)
	if err != nil {
		t.Fatalf("ApplyEvent: %v", err)
	}

	wantCash := round4(289.0 * 0.164)
	if got.AlreadyApplied {
		t.Fatalf("AlreadyApplied = true on first call")
	}
	if got.PostQuantity != 404.6 {
		t.Errorf("PostQuantity = %v, want 404.6", got.PostQuantity)
	}
	if got.PostCostPrice != 239.42857143 {
		t.Errorf("PostCostPrice = %v, want 239.42857143", got.PostCostPrice)
	}
	if got.CashCredit != wantCash {
		t.Errorf("CashCredit = %v, want %v", got.CashCredit, wantCash)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestApplyEvent_Idempotent guarantees the (event_id, fund_id) PK
// short-circuit: a second call with the same args must return the
// prior receipt verbatim and must not issue any UPDATE. Critical
// because the daily corp-action sweep can re-run after a crash and
// double-applying would invent shares out of thin air.
func TestApplyEvent_Idempotent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	evt := Event{
		ID:            "evt-2",
		InstrumentKey: "NASDAQ:NVDA",
		SplitRatio:    10.0,
		CashDividend:  0,
	}
	fundID := "fund-x"

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM corp_action_applications")).
		WithArgs(evt.ID, fundID).
		WillReturnRows(sqlmock.NewRows([]string{"pre_quantity", "post_quantity", "pre_cost_price", "post_cost_price", "cash_credit"}).
			AddRow(10.0, 100.0, 1200.0, 120.0, 0.0))
	mock.ExpectCommit()

	got, err := ApplyEvent(context.Background(), db, evt, fundID)
	if err != nil {
		t.Fatalf("ApplyEvent: %v", err)
	}
	if !got.AlreadyApplied {
		t.Errorf("AlreadyApplied = false, want true")
	}
	if got.PostQuantity != 100.0 || got.PostCostPrice != 120.0 {
		t.Errorf("idempotent receipt mismatch: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestApplyEvent_PositionMissing covers the "fund doesn't hold the
// instrument by ex-date" case. The daily sweep iterates every fund
// in the system; the vast majority won't hold any given instrument.
// We must return ErrPositionMissing without burning a tx commit.
func TestApplyEvent_PositionMissing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	evt := Event{ID: "e", InstrumentKey: "SSE:600519", SplitRatio: 1.5}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM corp_action_applications")).
		WillReturnError(errNoRowsForTest)
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WillReturnError(errNoRowsForTest)
	mock.ExpectRollback()

	_, err = ApplyEvent(context.Background(), db, evt, "fund-no-holding")
	if !errors.Is(err, ErrPositionMissing) {
		t.Fatalf("err = %v, want ErrPositionMissing", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestApplyEvent_ReverseSplit pins the math when ratio < 1 (a 10:1
// reverse split is split_ratio=0.1). Shares should shrink, cost
// should multiply. Production has not seen one yet but US small caps
// reverse-split routinely and the sign convention has tripped audit
// reviewers in past projects.
func TestApplyEvent_ReverseSplit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	evt := Event{
		ID:            "evt-rev",
		InstrumentKey: "NASDAQ:XYZ",
		SplitRatio:    0.1, // 10:1 reverse
		CashDividend:  0,
	}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM corp_action_applications")).
		WillReturnError(errNoRowsForTest)
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WillReturnRows(sqlmock.NewRows([]string{"quantity", "cost_price", "current_price", "market_value", "unrealized_pnl"}).
			AddRow(1000.0, 1.5, 14.0, 14000.0, 12500.0))
	// 1000 * 0.1 = 100 shares, 1.5 / 0.1 = 15.0 cost.
	mock.ExpectExec(regexp.QuoteMeta("UPDATE holding_positions")).
		WithArgs("fund-r", evt.InstrumentKey, 100.0, 0.1, 1.0, 15.0, 1400.0, -100.0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE position_lots")).
		WithArgs("fund-r", evt.InstrumentKey, 0.1).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO corp_action_applications")).
		WithArgs(evt.ID, "fund-r", 1000.0, 100.0, 1.5, 15.0, 0.0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	got, err := ApplyEvent(context.Background(), db, evt, "fund-r")
	if err != nil {
		t.Fatalf("ApplyEvent: %v", err)
	}
	if got.PostQuantity != 100.0 {
		t.Errorf("PostQuantity = %v, want 100.0 (reverse split)", got.PostQuantity)
	}
	if got.PostCostPrice != 15.0 {
		t.Errorf("PostCostPrice = %v, want 15.0", got.PostCostPrice)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestApplyEvent_RejectsBadInput exercises validateEvent. These
// errors must fail BEFORE any tx opens — the admin handler returns
// 400 on ErrEventInvalid and we don't want the operator to see a
// half-rolled-back state.
func TestApplyEvent_RejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		evt  Event
		fund string
	}{
		{"empty event id", Event{InstrumentKey: "x", SplitRatio: 1}, "fund-a"},
		{"empty instrument", Event{ID: "e", SplitRatio: 1}, "fund-a"},
		{"zero ratio", Event{ID: "e", InstrumentKey: "x", SplitRatio: 0}, "fund-a"},
		{"negative ratio", Event{ID: "e", InstrumentKey: "x", SplitRatio: -1}, "fund-a"},
		{"negative dividend", Event{ID: "e", InstrumentKey: "x", SplitRatio: 1, CashDividend: -0.5}, "fund-a"},
		{"empty fund", Event{ID: "e", InstrumentKey: "x", SplitRatio: 1}, ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			db, _, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()
			_, err = ApplyEvent(context.Background(), db, tc.evt, tc.fund)
			if !errors.Is(err, ErrEventInvalid) {
				t.Fatalf("err = %v, want ErrEventInvalid", err)
			}
		})
	}
}

// TestRoundingHelpers locks the round8/round4 contracts that the
// ledger relies on. Drift here would be the first sign that
// re-running the applier with the same inputs starts producing
// off-by-1-LSB results, breaking idempotency receipts.
func TestRoundingHelpers(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		fn   func(float64) float64
		want float64
	}{
		{"round8 down", 1.123456784, round8, 1.12345678},
		{"round8 up", 1.123456786, round8, 1.12345679},
		{"round8 negative", -1.123456786, round8, -1.12345679},
		{"round4 down", 1.23454, round4, 1.2345},
		{"round4 up", 1.23456, round4, 1.2346},
		{"round4 negative", -1.23456, round4, -1.2346},
		{"round8 of 1.4 ratio reciprocal", 335.20 / 1.4, round8, 239.42857143},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := tc.fn(tc.in)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// errNoRowsForTest is sql.ErrNoRows. Splitting it out as a named
// sentinel keeps the test bodies short and signals intent to readers.
var errNoRowsForTest = sql.ErrNoRows
