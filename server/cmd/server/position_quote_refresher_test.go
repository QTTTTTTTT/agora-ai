package main

import (
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/marketdata"
	"github.com/fundai/server/internal/repository"
)

// TestComputeRefreshedValuationLongCash exercises the happy path for an
// equity long position: market value = qty * price; PnL = (price - cost) * qty.
func TestComputeRefreshedValuationLongCash(t *testing.T) {
	position := repository.HoldingPosition{
		Quantity:     100,
		CostPrice:    20.0,
		PositionSide: sql.NullString{String: "long", Valid: true},
		AssetClass:   sql.NullString{String: "equity", Valid: true},
	}
	marketValue, unrealized := computeRefreshedValuation(&position, 25.0)
	if marketValue != 2500 {
		t.Fatalf("marketValue = %v, want 2500", marketValue)
	}
	if !unrealized.Valid {
		t.Fatalf("unrealized should be set when cost basis > 0")
	}
	if unrealized.Float64 != 500 {
		t.Fatalf("unrealized = %v, want 500", unrealized.Float64)
	}
}

// TestComputeRefreshedValuationShort inverts the delta sign for short
// positions so a price drop registers as a gain.
func TestComputeRefreshedValuationShort(t *testing.T) {
	position := repository.HoldingPosition{
		Quantity:     50,
		CostPrice:    100.0,
		PositionSide: sql.NullString{String: "short", Valid: true},
		AssetClass:   sql.NullString{String: "equity", Valid: true},
	}
	marketValue, unrealized := computeRefreshedValuation(&position, 90.0)
	if marketValue != 4500 {
		t.Fatalf("marketValue = %v, want 4500", marketValue)
	}
	if !unrealized.Valid || unrealized.Float64 != 500 {
		t.Fatalf("short unrealized = %+v, want 500", unrealized)
	}
}

// TestComputeRefreshedValuationFuturesUsesMultiplier verifies futures
// positions still pick up the contract multiplier in both market value
// and unrealized P&L.
func TestComputeRefreshedValuationFuturesUsesMultiplier(t *testing.T) {
	position := repository.HoldingPosition{
		Quantity:           5,
		CostPrice:          200.0,
		PositionSide:       sql.NullString{String: "long", Valid: true},
		AssetClass:         sql.NullString{String: "futures", Valid: true},
		ContractMultiplier: sql.NullFloat64{Float64: 50, Valid: true},
	}
	marketValue, unrealized := computeRefreshedValuation(&position, 210.0)
	if marketValue != 5*210*50 {
		t.Fatalf("marketValue = %v, want %v", marketValue, 5*210*50)
	}
	if !unrealized.Valid {
		t.Fatalf("expected unrealized PnL for futures position")
	}
	if unrealized.Float64 != (210-200)*5*50 {
		t.Fatalf("unrealized = %v, want %v", unrealized.Float64, (210-200)*5*50)
	}
}

// stubRefresherMetrics records each RecordRefreshPass call so we can
// assert the refresher reports the right counters per pass.
type stubRefresherMetrics struct {
	passes int
	rows   int
	failed int
}

func (s *stubRefresherMetrics) RecordRefreshPass(rows int, _ time.Duration, failed bool) {
	s.passes++
	s.rows += rows
	if failed {
		s.failed++
	}
}

// stubMarketData reuses a test-only Service whose only purpose is to
// satisfy the refresher's dependency. We populate the quote cache by
// hand so GetQuotes returns deterministic values without going through
// any provider chain.
func stubMarketDataWithQuotes(symbols map[string]float64) *marketdata.Service {
	svc := marketdata.NewService(marketdata.Config{
		QuoteProviders: []string{"stub"},
		QuoteTTL:       10 * time.Second,
	})
	svc.SeedQuotesForTesting(symbols)
	return svc
}

// TestPositionQuoteRefresherRunOnceWritesBack drives the full pass: 1
// fund × 2 positions, GetQuotes returns deterministic prices, and we
// verify the UPDATE statements hit the DB with the right values.
func TestPositionQuoteRefresherRunOnceWritesBack(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	mock.MatchExpectationsInOrder(false)
	// ListActive
	mock.ExpectQuery(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE status = 'active'
		 ORDER BY created_at DESC`).WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
		AddRow("fund-1", "co-1", "Test Fund", "", "paper", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{}`), time.Now(), time.Now()))

	// ListByFund for fund-1 (note column order: current_price after cost_price, before market_value).
	mock.ExpectQuery(`SELECT id, fund_id, instrument_key, symbol, name, market, exchange, asset_class, instrument_type, position_side, quote_currency, settlement_currency, margin_mode, quantity, available_qty, cost_price, current_price, market_value, weight, leverage, contract_multiplier, expiry_date, unrealized_pnl, margin_used, updated_at
		 FROM holding_positions WHERE fund_id = $1 ORDER BY instrument_key`).WithArgs("fund-1").WillReturnRows(positionListRows().
		AddRow("pos-1", "fund-1", "us|nasdaq|equity|MU", "MU", nil, "usstock", "NASDAQ", "equity", "stock", "long", "USD", "USD", nil, 100.0, 100.0, 80.0, 90.0, 9000.0, 0.0, nil, nil, nil, nil, nil, time.Now()).
		AddRow("pos-2", "fund-1", "us|nasdaq|equity|TSLA", "TSLA", nil, "usstock", "NASDAQ", "equity", "stock", "long", "USD", "USD", nil, 10.0, 10.0, 200.0, 210.0, 2100.0, 0.0, nil, nil, nil, nil, nil, time.Now()))

	// Two UPDATEs, one per position. Both must show the fresh price + recomputed market_value.
	mock.ExpectExec(`UPDATE holding_positions
		   SET current_price = $3,
		       market_value = $4,
		       unrealized_pnl = $5,
		       updated_at = NOW()
		 WHERE fund_id = $1 AND instrument_key = $2`).
		WithArgs("fund-1", "us|nasdaq|equity|MU", 110.0, 11000.0, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE holding_positions
		   SET current_price = $3,
		       market_value = $4,
		       unrealized_pnl = $5,
		       updated_at = NOW()
		 WHERE fund_id = $1 AND instrument_key = $2`).
		WithArgs("fund-1", "us|nasdaq|equity|TSLA", 250.0, 2500.0, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	metrics := &stubRefresherMetrics{}
	refresher := newPositionQuoteRefresher(
		repository.NewFundRepo(db),
		repository.NewPositionRepo(db),
		stubMarketDataWithQuotes(map[string]float64{"MU": 110.0, "TSLA": 250.0}),
	)
	refresher.SetMetrics(metrics)

	refresher.runOnce()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
	if metrics.passes != 1 {
		t.Fatalf("metrics passes = %d, want 1", metrics.passes)
	}
	if metrics.rows != 2 {
		t.Fatalf("metrics rows = %d, want 2", metrics.rows)
	}
	if metrics.failed != 0 {
		t.Fatalf("metrics failed = %d, want 0", metrics.failed)
	}
}

// TestPositionQuoteRefresherSkipsWhenNotLeader confirms followers never
// touch the database or upstream. Important: leader-election was already
// proven by the activity-retention loop, but we want a regression test
// dedicated to this loop so the gating can't silently drift.
func TestPositionQuoteRefresherSkipsWhenNotLeader(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	// No SQL expectations: the loop must short-circuit before the
	// first query when isLeader() returns false.

	metrics := &stubRefresherMetrics{}
	refresher := newPositionQuoteRefresher(
		repository.NewFundRepo(db),
		repository.NewPositionRepo(db),
		stubMarketDataWithQuotes(nil),
	)
	refresher.SetMetrics(metrics)
	refresher.SetLeaderChecker(stubLeaderChecker{leader: false})

	refresher.runOnce()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected SQL on follower: %v", err)
	}
	if metrics.passes != 0 {
		t.Fatalf("metrics passes = %d, want 0 on follower", metrics.passes)
	}
}

// TestPositionQuoteRefresherSwitchesIntervals exercises the in-/off-session
// cadence picker so the production schedule actually flips when a market
// is closed.
func TestPositionQuoteRefresherSwitchesIntervals(t *testing.T) {
	refresher := newPositionQuoteRefresher(nil, nil, nil)
	refresher.SetIntervals(20*time.Millisecond, 10*time.Second)
	refresher.inSessionFn = func(time.Time) bool { return true }
	if got := refresher.nextInterval(); got != 20*time.Millisecond {
		t.Fatalf("in-session next = %v, want 20ms", got)
	}
	refresher.inSessionFn = func(time.Time) bool { return false }
	if got := refresher.nextInterval(); got != 10*time.Second {
		t.Fatalf("off-session next = %v, want 10s", got)
	}
}

// stubLeaderChecker is the minimal leaderChecker used to drive the loop
// in tests without bringing up a real scheduler.LeaseManager.
type stubLeaderChecker struct {
	leader bool
}

func (s stubLeaderChecker) IsLeader(_ string) bool { return s.leader }

// positionListRows returns a sqlmock.Rows pre-populated with the column
// schema produced by PositionRepo.ListByFund so individual tests can
// AddRow with the right ordinals. The ordering matches the SELECT in
// fund_repo.go (cost_price, current_price, market_value, ...).
func positionListRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "fund_id", "instrument_key", "symbol", "name", "market", "exchange", "asset_class",
		"instrument_type", "position_side", "quote_currency", "settlement_currency", "margin_mode",
		"quantity", "available_qty", "cost_price", "current_price", "market_value", "weight",
		"leverage", "contract_multiplier", "expiry_date", "unrealized_pnl", "margin_used",
		"updated_at",
	})
}

