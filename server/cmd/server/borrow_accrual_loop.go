// borrow_accrual_loop.go — S6.4 daily borrow-fee accruer.
//
// Once per accrual day (default 23:55 UTC) the loop:
//
//   1. Lists all open short positions across all funds
//      (holding_positions where quantity < 0).
//   2. For each, looks up the borrow rate from the cache.
//   3. Computes the fee via securitiesborrow.AccrualEngine.
//   4. Books a debit to cash_ledger (type='borrow_fee') with
//      an idempotency_key = "borrow:{fund_id}:{instrument_key}:{accrual_date}"
//      so retries don't double-charge.
//   5. UPSERTs the per-day row into short_position_borrow_ledger
//      with the cash_ledger entry id for cross-reference.
//
// The loop is leader-gated and skipping/error-tolerant: a
// single fund's failure does not stop the others. All counts
// are surfaced as Prometheus metrics.

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/securitiesborrow"
)

// borrowAccrualLoop runs the daily accrual.
type borrowAccrualLoop struct {
	db          *sql.DB
	borrowRepo  *securitiesborrow.Repo
	cache       *securitiesborrow.Cache
	engine      *securitiesborrow.AccrualEngine
	cashRepo    *repository.CashLedgerRepo
	metrics     borrowMetricsRecorder
	leader      leaderState
	logger      *slog.Logger

	interval    time.Duration   // how often to scan (default 1h; SOD is gated by HourOfDay)
	hourOfDay   int             // accrue once per day at this UTC hour
	dayCount    int             // 360 or 365 (defaults 365)
	now         func() time.Time

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}

	mu       sync.Mutex
	lastRun  time.Time
}

// borrowAccrualConfig is the constructor input.
type borrowAccrualConfig struct {
	DB         *sql.DB
	BorrowRepo *securitiesborrow.Repo
	Cache      *securitiesborrow.Cache
	CashRepo   *repository.CashLedgerRepo
	Metrics    borrowMetricsRecorder
	Leader     leaderState
	Logger     *slog.Logger
	Interval   time.Duration // default 1h
	HourOfDay  int           // default 23 (UTC)
	DayCount   int           // 360 or 365; default 365
}

// leaderState is the minimal slice of the leader-election
// machinery the loop needs.
type leaderState interface {
	IsLeader() bool
}

// nilLeader is the always-on stub used when no leader gate is
// wired (single-instance dev).
type nilLeader struct{}

func (nilLeader) IsLeader() bool { return true }

// newBorrowAccrualLoop constructs the loop.
func newBorrowAccrualLoop(cfg borrowAccrualConfig) *borrowAccrualLoop {
	l := &borrowAccrualLoop{
		db:         cfg.DB,
		borrowRepo: cfg.BorrowRepo,
		cache:      cfg.Cache,
		engine:     securitiesborrow.NewAccrualEngine(),
		cashRepo:   cfg.CashRepo,
		metrics:    cfg.Metrics,
		leader:     cfg.Leader,
		logger:     cfg.Logger,
		interval:   cfg.Interval,
		hourOfDay:  cfg.HourOfDay,
		dayCount:   cfg.DayCount,
		now:        func() time.Time { return time.Now().UTC() },
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
	if l.interval <= 0 {
		l.interval = time.Hour
	}
	if l.hourOfDay < 0 || l.hourOfDay > 23 {
		l.hourOfDay = 23
	}
	if l.dayCount != 360 && l.dayCount != 365 {
		l.dayCount = 365
	}
	if l.leader == nil {
		l.leader = nilLeader{}
	}
	if l.logger == nil {
		l.logger = slog.Default()
	}
	return l
}

// Start kicks off the loop. Idempotent — repeated calls are
// no-ops.
func (l *borrowAccrualLoop) Start(ctx context.Context) {
	if l == nil || l.db == nil || l.borrowRepo == nil {
		return
	}
	go l.run(ctx)
}

// Stop signals the loop to exit and waits for it.
func (l *borrowAccrualLoop) Stop() {
	if l == nil {
		return
	}
	l.stopOnce.Do(func() {
		close(l.stopCh)
		<-l.doneCh
	})
}

func (l *borrowAccrualLoop) run(ctx context.Context) {
	defer close(l.doneCh)
	t := time.NewTicker(l.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.stopCh:
			return
		case <-t.C:
			l.tickOnce(ctx)
		}
	}
}

// tickOnce wakes up, checks leadership + accrual window, and
// runs the accrual if we haven't already done today's.
func (l *borrowAccrualLoop) tickOnce(ctx context.Context) {
	if !l.leader.IsLeader() {
		return
	}
	now := l.now()
	if now.Hour() < l.hourOfDay {
		return
	}
	l.mu.Lock()
	if !l.lastRun.IsZero() && sameUTCDay(l.lastRun, now) {
		l.mu.Unlock()
		return
	}
	l.lastRun = now
	l.mu.Unlock()
	if err := l.AccrueOnce(ctx, now); err != nil {
		l.logger.Warn("borrow accrual run failed", "err", err)
	}
}

// AccrueOnce processes every fund's short positions for one
// accrual date. Exposed for tests.
func (l *borrowAccrualLoop) AccrueOnce(ctx context.Context, asOf time.Time) error {
	if l == nil || l.db == nil {
		return errors.New("borrow accrual: nil db")
	}
	accrualDate := time.Date(asOf.Year(), asOf.Month(), asOf.Day(), 0, 0, 0, 0, time.UTC)
	rows, err := l.db.QueryContext(ctx, `
		SELECT fund_id, instrument_key, COALESCE(symbol, ''),
		       quantity, COALESCE(current_price, 0)
		  FROM holding_positions
		 WHERE quantity < 0
		 ORDER BY fund_id, instrument_key
	`)
	if err != nil {
		l.recordMetric("scan_failed")
		return fmt.Errorf("borrow accrual: scan shorts: %w", err)
	}
	defer rows.Close()
	processed := 0
	skipped := 0
	for rows.Next() {
		var (
			fundID, instrumentKey, symbol string
			qty, price                    float64
		)
		if err := rows.Scan(&fundID, &instrumentKey, &symbol, &qty, &price); err != nil {
			l.recordMetric("scan_row_failed")
			l.logger.Warn("borrow accrual: row scan", "err", err)
			continue
		}
		shortQty := -qty // qty is negative; we want magnitude
		if shortQty <= 0 {
			continue
		}
		// Resolve the rate.
		rate := l.lookupRate(instrumentKey)
		bps := 0.0
		if rate != nil {
			bps = rate.BorrowRateBpsAnnual
		}
		probe := securitiesborrow.AccrualProbe{
			FundID: fundID, InstrumentKey: instrumentKey, Symbol: symbol,
			AccrualDate:   accrualDate,
			ShortQty:      shortQty,
			MarketPrice:   price,
			RateBpsAnnual: bps,
			DayCountBasis: l.dayCount,
		}
		res := l.engine.Evaluate(probe)
		if res.FeeAmount <= 0 {
			l.recordMetric("accrual_skipped_" + res.Reason)
			skipped++
			continue
		}
		if err := l.bookFee(ctx, fundID, instrumentKey, symbol, accrualDate, probe, res); err != nil {
			l.recordMetric("book_failed")
			l.logger.Warn("borrow accrual: book fee", "err", err, "fund", fundID, "key", instrumentKey)
			continue
		}
		l.recordMetric("accrual_booked")
		processed++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	l.recordMetric("run_completed")
	l.logger.Info("borrow accrual run done",
		"asOf", asOf, "processed", processed, "skipped", skipped)
	return nil
}

// bookFee writes both the cash_ledger debit and the
// short_position_borrow_ledger record. Idempotency comes from
// the cash_ledger idempotency_key + the (fund, instrument, date)
// UNIQUE on the borrow ledger.
func (l *borrowAccrualLoop) bookFee(
	ctx context.Context,
	fundID, instrumentKey, symbol string,
	accrualDate time.Time,
	probe securitiesborrow.AccrualProbe,
	res securitiesborrow.AccrualResult,
) error {
	idemKey := fmt.Sprintf("borrow:%s:%s:%s", fundID, instrumentKey, accrualDate.Format("2006-01-02"))
	cashEntryID := ""
	if l.cashRepo != nil {
		id, err := l.cashRepo.Append(ctx, repository.AppendParams{
			FundID:         fundID,
			PostedAt:       accrualDate,
			TradingDate:    &accrualDate,
			EntryType:      repository.CashEntryBorrowFee,
			Amount:         -res.FeeAmount, // debit
			Currency:       "USD",
			Description:    fmt.Sprintf("Borrow fee %s × %.0f shares @ %.2fbps/yr", symbol, probe.ShortQty, probe.RateBpsAnnual),
			IdempotencyKey: idemKey,
			Metadata: map[string]any{
				"instrument_key":  instrumentKey,
				"short_qty":       probe.ShortQty,
				"market_price":    probe.MarketPrice,
				"rate_bps_annual": probe.RateBpsAnnual,
				"day_count_basis": res.DayCountBasis,
			},
		})
		if err != nil {
			return fmt.Errorf("cash_ledger append: %w", err)
		}
		cashEntryID = id
	}
	if _, err := l.borrowRepo.UpsertLedgerEntry(ctx, securitiesborrow.UpsertLedgerParams{
		FundID:            fundID,
		InstrumentKey:     instrumentKey,
		Symbol:            symbol,
		AccrualDate:       accrualDate,
		ShortQty:          probe.ShortQty,
		MarketPrice:       probe.MarketPrice,
		Notional:          res.Notional,
		RateBpsAnnual:     probe.RateBpsAnnual,
		DayCountBasis:     res.DayCountBasis,
		FeeAmount:         res.FeeAmount,
		CashLedgerEntryID: cashEntryID,
	}); err != nil {
		return fmt.Errorf("borrow ledger upsert: %w", err)
	}
	return nil
}

func (l *borrowAccrualLoop) lookupRate(instrumentKey string) *securitiesborrow.BorrowRate {
	if l == nil || l.cache == nil {
		return nil
	}
	return l.cache.Lookup(instrumentKey)
}

func (l *borrowAccrualLoop) recordMetric(event string) {
	if l == nil || l.metrics == nil {
		return
	}
	// We share the borrow_events counter with the gate; the
	// event labels are distinct ("accrual_*" vs "check_*") so
	// dashboards can separate cleanly.
	l.metrics.RecordBorrowEvent(strings.TrimSpace(event))
}

func sameUTCDay(a, b time.Time) bool {
	ya, ma, da := a.UTC().Date()
	yb, mb, db := b.UTC().Date()
	return ya == yb && ma == mb && da == db
}
