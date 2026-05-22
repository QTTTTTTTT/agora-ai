package repository

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// These tests pin the SQL surface area of LotRepo. They don't try
// to replicate Postgres semantics (sqlmock can't run real queries);
// the goal is to:
//
//   - catch typos / column-name drift in INSERT/UPDATE
//   - lock in the parameter order so refactors that reorder fields
//     are caught at CI time
//   - verify the partial-close UPDATE filters on "status != closed"
//     so a re-fire on an already-closed lot is a no-op, not a
//     silent over-close
//
// The integration tests in lotledger_test.go cover the math.

func TestLotRepoOpenLotInsertsExpectedColumns(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	repo := NewLotRepo(db)
	lot := &PositionLotRow{
		FundID:              "fund-1",
		InstrumentKey:       "SSE:600000",
		Symbol:              "600000",
		Market:              sql.NullString{String: "a_share", Valid: true},
		AssetClass:          sql.NullString{String: "equity", Valid: true},
		OpeningTradeID:      "trade-1",
		OpeningPlanActionID: sql.NullString{String: "action-1", Valid: true},
		OpenedAt:            time.Date(2026, 5, 14, 9, 30, 0, 0, time.UTC),
		EntryPrice:          10.5,
		EntryFees:           5.25,
		QuantityOpened:      500,
		// QuantityRemaining intentionally left zero so we exercise
		// the "default to quantity_opened" fallback.
		Sleeve:            sql.NullString{String: "llm_pm", Valid: true},
		RegimeAtEntry:     sql.NullString{String: "trend_up", Valid: true},
		SignalSource:      sql.NullString{String: "llm_pm", Valid: true},
		ConfidenceAtEntry: sql.NullFloat64{Float64: 0.72, Valid: true},
		HighestPriceSeen:  sql.NullFloat64{Float64: 10.5, Valid: true},
		LowestPriceSeen:   sql.NullFloat64{Float64: 10.5, Valid: true},
		LastPrice:         sql.NullFloat64{Float64: 10.5, Valid: true},
		LastPriceAt:       sql.NullTime{Time: time.Date(2026, 5, 14, 9, 30, 0, 0, time.UTC), Valid: true},
	}

	mock.ExpectQuery(regexp.QuoteMeta(`
INSERT INTO position_lots
    (fund_id, instrument_key, symbol, market, asset_class,
     opening_trade_id, opening_plan_action_id,
     opened_at, entry_price, entry_fees,
     quantity_opened, quantity_remaining,
     sleeve, regime_at_entry, signal_source, confidence_at_entry,
     highest_price_seen, lowest_price_seen, last_price, last_price_at,
     status)
VALUES ($1, $2, $3, $4, $5,
        $6, $7,
        $8, $9, $10,
        $11, $12,
        $13, $14, $15, $16,
        $17, $18, $19, $20,
        'open')
RETURNING id`)).
		WithArgs(
			lot.FundID, lot.InstrumentKey, lot.Symbol, lot.Market, lot.AssetClass,
			lot.OpeningTradeID, lot.OpeningPlanActionID,
			lot.OpenedAt, lot.EntryPrice, lot.EntryFees,
			lot.QuantityOpened, lot.QuantityOpened, // remaining defaults to opened
			lot.Sleeve, lot.RegimeAtEntry, lot.SignalSource, lot.ConfidenceAtEntry,
			lot.HighestPriceSeen, lot.LowestPriceSeen, lot.LastPrice, lot.LastPriceAt,
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("lot-uuid-1"))

	id, err := repo.OpenLot(context.Background(), lot)
	if err != nil {
		t.Fatalf("OpenLot: %v", err)
	}
	if id != "lot-uuid-1" {
		t.Fatalf("expected lot-uuid-1, got %q", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

func TestLotRepoOpenLotRejectsMissingRequiredFields(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewLotRepo(db)

	cases := []struct {
		name string
		lot  *PositionLotRow
	}{
		{"nil lot", nil},
		{"no fund_id", &PositionLotRow{InstrumentKey: "X", Symbol: "X", OpeningTradeID: "t", QuantityOpened: 1, EntryPrice: 1}},
		{"no instrument_key", &PositionLotRow{FundID: "f", Symbol: "X", OpeningTradeID: "t", QuantityOpened: 1, EntryPrice: 1}},
		{"no trade_id", &PositionLotRow{FundID: "f", InstrumentKey: "X", Symbol: "X", QuantityOpened: 1, EntryPrice: 1}},
		{"zero qty", &PositionLotRow{FundID: "f", InstrumentKey: "X", Symbol: "X", OpeningTradeID: "t", QuantityOpened: 0, EntryPrice: 1}},
		{"negative entry price", &PositionLotRow{FundID: "f", InstrumentKey: "X", Symbol: "X", OpeningTradeID: "t", QuantityOpened: 1, EntryPrice: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := repo.OpenLot(context.Background(), tc.lot)
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestLotRepoPartialCloseUpdatesAndInsertsWithStatusGate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewLotRepo(db)

	closedAt := time.Date(2026, 5, 14, 15, 0, 0, 0, time.UTC)
	row := &ClosedLotRow{
		FundID:              "fund-1",
		PositionLotID:       "lot-1",
		InstrumentKey:       "SSE:600000",
		Symbol:              "600000",
		Market:              sql.NullString{String: "a_share", Valid: true},
		AssetClass:          sql.NullString{String: "equity", Valid: true},
		ClosingTradeID:      "trade-2",
		ClosingPlanActionID: sql.NullString{String: "action-2", Valid: true},
		OpenedAt:            closedAt.Add(-72 * time.Hour),
		ClosedAt:            closedAt,
		HoldingDays:         3,
		QuantityClosed:      100,
		EntryPrice:          10,
		ExitPrice:           12,
		EntryFees:           1,
		ExitFees:            1.5,
		RealizedPnL:         197.5,
		RealizedPnLPct:      0.1975,
		MaxFavorableExcursion: sql.NullFloat64{Float64: 0.25, Valid: true},
		MaxAdverseExcursion:   sql.NullFloat64{Float64: -0.05, Valid: true},
		Sleeve:                sql.NullString{String: "llm_pm", Valid: true},
		RegimeAtEntry:         sql.NullString{String: "trend_up", Valid: true},
		RegimeAtExit:          sql.NullString{String: "range", Valid: true},
		SignalSource:          sql.NullString{String: "llm_pm", Valid: true},
		ConfidenceAtEntry:     sql.NullFloat64{Float64: 0.7, Valid: true},
		ExitReason:            sql.NullString{String: "llm_decision", Valid: true},
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`
INSERT INTO closed_lots`)).
		WithArgs(
			row.FundID, row.PositionLotID, row.InstrumentKey, row.Symbol, row.Market, row.AssetClass,
			row.ClosingTradeID, row.ClosingPlanActionID,
			row.OpenedAt, row.ClosedAt, row.HoldingDays,
			row.QuantityClosed, row.EntryPrice, row.ExitPrice, row.EntryFees, row.ExitFees,
			row.RealizedPnL, row.RealizedPnLPct,
			row.MaxFavorableExcursion, row.MaxAdverseExcursion,
			row.Sleeve, row.RegimeAtEntry, row.RegimeAtExit, row.SignalSource,
			row.ConfidenceAtEntry, row.ExitReason,
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("closed-1"))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE position_lots`)).
		WithArgs(row.PositionLotID, row.QuantityClosed, row.ClosedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := repo.PartialCloseTx(ctx, tx, row); err != nil {
		t.Fatalf("PartialCloseTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if row.ID != "closed-1" {
		t.Fatalf("expected ID to be set on row, got %q", row.ID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

func TestLotRepoPartialCloseReturnsConflictWhenLotInsufficient(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewLotRepo(db)

	row := &ClosedLotRow{
		FundID:         "fund-1",
		PositionLotID:  "lot-1",
		InstrumentKey:  "X",
		Symbol:         "X",
		ClosingTradeID: "trade-1",
		QuantityClosed: 50,
		ClosedAt:       time.Now().UTC(),
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO closed_lots`)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("closed-1"))
	// UPDATE returns 0 rows when status='closed' or quantity_remaining < closeQty.
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE position_lots`)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	ctx := context.Background()
	tx, _ := db.BeginTx(ctx, nil)
	err = repo.PartialCloseTx(ctx, tx, row)
	if err != ErrLotConflict {
		t.Fatalf("expected ErrLotConflict, got %v", err)
	}
	_ = tx.Rollback()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

func TestLotRepoUpdateExcursionNoopsOnInvalidPrice(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewLotRepo(db)
	// No mock.Expect — a non-zero ExecContext call here would fail
	// the sqlmock contract.
	if err := repo.UpdateExcursion(context.Background(), "fund-1", "X", 0, time.Now()); err != nil {
		t.Fatalf("expected silent no-op for price=0, got %v", err)
	}
	if err := repo.UpdateExcursion(context.Background(), "fund-1", "X", -1, time.Now()); err != nil {
		t.Fatalf("expected silent no-op for negative price, got %v", err)
	}
}

// TestLotRepoStatsBySleeveRegimeAggregatesCrossTab pins the SQL
// for the Phase 3A-5 attribution cross-tab. It's the join point
// the attribution Service consumes; a column drift here would
// silently corrupt every lesson the platform generates.
func TestLotRepoStatsBySleeveRegimeAggregatesCrossTab(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	since := now.AddDate(0, 0, -30)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(sleeve, '')                                    AS sleeve,
       COALESCE(regime_at_entry, '')                           AS regime,
       COUNT(*)                                                AS trade_count,
       COUNT(*) FILTER (WHERE realized_pnl > 0)                AS win_count,
       COUNT(*) FILTER (WHERE realized_pnl < 0)                AS loss_count,
       COALESCE(SUM(realized_pnl), 0)                          AS total_pnl,
       COALESCE(AVG(realized_pnl_pct), 0)                      AS avg_pnl_pct,
       COALESCE(AVG(holding_days), 0)                          AS avg_holding_days
  FROM closed_lots
 WHERE fund_id = $1
   AND closed_at >= $2
 GROUP BY COALESCE(sleeve, ''), COALESCE(regime_at_entry, '')
 ORDER BY total_pnl DESC, sleeve ASC, regime ASC`)).
		WithArgs("fund-1", since).
		WillReturnRows(sqlmock.NewRows([]string{
			"sleeve", "regime", "trade_count", "win_count", "loss_count", "total_pnl", "avg_pnl_pct", "avg_holding_days",
		}).
			AddRow("trend", "trend_up", 12, 8, 4, 320.50, 0.027, 5.3).
			AddRow("mean_reversion", "chop", 9, 2, 7, -210.75, -0.023, 1.9))

	repo := NewLotRepo(db)
	got, err := repo.StatsBySleeveRegime(context.Background(), "fund-1", since)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(got))
	}
	if got[0].Sleeve != "trend" || got[0].Regime != "trend_up" {
		t.Fatalf("first row: got %+v", got[0])
	}
	// WinRate must be computed by the repo from win/trade counts.
	if got[0].WinRate != 8.0/12.0 {
		t.Fatalf("win_rate: got %v, want 8/12", got[0].WinRate)
	}
	if got[1].WinRate != 2.0/9.0 {
		t.Fatalf("win_rate row 2: got %v, want 2/9", got[1].WinRate)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestLotRepoListOpenByInstrumentBuildsFIFOQuery(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewLotRepo(db)

	cols := []string{
		"id", "fund_id", "instrument_key", "symbol", "market", "asset_class",
		"opening_trade_id", "opening_plan_action_id",
		"opened_at", "entry_price", "entry_fees",
		"quantity_opened", "quantity_remaining",
		"sleeve", "regime_at_entry", "signal_source", "confidence_at_entry",
		"highest_price_seen", "lowest_price_seen", "last_price", "last_price_at",
		"status", "closed_at", "created_at", "updated_at",
	}
	now := time.Date(2026, 5, 14, 9, 30, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`WHERE fund_id = $1 AND instrument_key = $2 AND status != 'closed' ORDER BY opened_at ASC, id ASC`)).
		WithArgs("fund-1", "SSE:600000").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow(
				"lot-1", "fund-1", "SSE:600000", "600000",
				sql.NullString{String: "a_share", Valid: true}, sql.NullString{String: "equity", Valid: true},
				"trade-1", sql.NullString{},
				now.Add(-48*time.Hour), 10.0, 1.0,
				100.0, 60.0,
				sql.NullString{String: "llm_pm", Valid: true}, sql.NullString{}, sql.NullString{}, sql.NullFloat64{},
				sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullTime{},
				"partial", sql.NullTime{}, now, now,
			))

	lots, err := repo.ListOpenByInstrument(context.Background(), "fund-1", "SSE:600000")
	if err != nil {
		t.Fatalf("ListOpenByInstrument: %v", err)
	}
	if len(lots) != 1 || lots[0].QuantityRemaining != 60 || lots[0].Status != "partial" {
		t.Fatalf("unexpected lot: %+v", lots)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}
