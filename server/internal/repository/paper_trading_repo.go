package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PaperTradingRepo persists the four Stage-4 tables:
//
//   - paper_portfolios            (id + current NAV)
//   - paper_orders                (append-only AI decisions w/ SHA256)
//   - paper_holdings_snapshots    (per-day holding map)
//   - paper_nav_history           (per-day NAV time series)
//
// The repo is deliberately CRUD-only — all business logic
// (canonicalising the payload, computing SHA256, calling out to
// OpenTimestamps, computing daily NAV from holdings × price) lives
// in internal/papertrading. Two reasons:
//
//   1. Tests can use a SQLite-in-memory clone of the schema with no
//      Bitcoin / Twitter side effects.
//   2. Future moves of the hash → OTS pipeline behind a job queue
//      don't touch this file.
type PaperTradingRepo struct {
	db *sql.DB
}

func NewPaperTradingRepo(db *sql.DB) *PaperTradingRepo {
	return &PaperTradingRepo{db: db}
}

func (r *PaperTradingRepo) DB() *sql.DB { return r.db }

// -----------------------------------------------------------------------------
// paper_portfolios
// -----------------------------------------------------------------------------

type PaperPortfolioRow struct {
	ID               string
	Name             string
	Strategy         string
	Market           string
	BenchmarkSymbol  sql.NullString
	InitialCapital   float64
	CurrentNav       float64
	CashBalance      float64
	CreatedAt        time.Time
	LastRebalanceAt  sql.NullTime
}

func (r *PaperTradingRepo) CreatePortfolio(ctx context.Context, p PaperPortfolioRow) (PaperPortfolioRow, error) {
	if p.Name == "" || p.Strategy == "" || p.Market == "" {
		return PaperPortfolioRow{}, errors.New("paper portfolio requires name, strategy, market")
	}
	if p.InitialCapital <= 0 {
		return PaperPortfolioRow{}, errors.New("initial capital must be > 0")
	}
	if p.CurrentNav == 0 {
		p.CurrentNav = p.InitialCapital
	}
	if p.CashBalance == 0 {
		p.CashBalance = p.InitialCapital
	}
	query := `
		INSERT INTO paper_portfolios
		    (name, strategy, market, benchmark_symbol, initial_capital,
		     current_nav, cash_balance)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at
	`
	row := r.db.QueryRowContext(ctx, query,
		p.Name, p.Strategy, p.Market, p.BenchmarkSymbol,
		p.InitialCapital, p.CurrentNav, p.CashBalance,
	)
	if err := row.Scan(&p.ID, &p.CreatedAt); err != nil {
		return PaperPortfolioRow{}, fmt.Errorf("insert paper_portfolios: %w", err)
	}
	return p, nil
}

func (r *PaperTradingRepo) GetPortfolio(ctx context.Context, id string) (*PaperPortfolioRow, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("empty portfolio id")
	}
	const q = `
		SELECT id, name, strategy, market, benchmark_symbol,
		       initial_capital, current_nav, cash_balance,
		       created_at, last_rebalance_at
		FROM paper_portfolios WHERE id = $1
	`
	var p PaperPortfolioRow
	err := r.db.QueryRowContext(ctx, q, id).Scan(
		&p.ID, &p.Name, &p.Strategy, &p.Market, &p.BenchmarkSymbol,
		&p.InitialCapital, &p.CurrentNav, &p.CashBalance,
		&p.CreatedAt, &p.LastRebalanceAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get paper_portfolio %s: %w", id, err)
	}
	return &p, nil
}

func (r *PaperTradingRepo) ListPortfolios(ctx context.Context) ([]PaperPortfolioRow, error) {
	const q = `
		SELECT id, name, strategy, market, benchmark_symbol,
		       initial_capital, current_nav, cash_balance,
		       created_at, last_rebalance_at
		FROM paper_portfolios
		ORDER BY created_at DESC
	`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list paper_portfolios: %w", err)
	}
	defer rows.Close()
	out := make([]PaperPortfolioRow, 0, 32)
	for rows.Next() {
		var p PaperPortfolioRow
		if err := rows.Scan(
			&p.ID, &p.Name, &p.Strategy, &p.Market, &p.BenchmarkSymbol,
			&p.InitialCapital, &p.CurrentNav, &p.CashBalance,
			&p.CreatedAt, &p.LastRebalanceAt,
		); err != nil {
			return nil, fmt.Errorf("scan paper_portfolios: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdatePortfolioNAV refreshes the latest NAV + cash balance and
// stamps last_rebalance_at when nonzero. The caller is expected to
// have computed the new values from the holdings snapshot.
func (r *PaperTradingRepo) UpdatePortfolioNAV(ctx context.Context, id string, currentNav, cashBalance float64, lastRebalance time.Time) error {
	const q = `
		UPDATE paper_portfolios
		SET current_nav = $2,
		    cash_balance = $3,
		    last_rebalance_at = COALESCE($4, last_rebalance_at)
		WHERE id = $1
	`
	var lr sql.NullTime
	if !lastRebalance.IsZero() {
		lr = sql.NullTime{Time: lastRebalance, Valid: true}
	}
	res, err := r.db.ExecContext(ctx, q, id, currentNav, cashBalance, lr)
	if err != nil {
		return fmt.Errorf("update paper_portfolio nav: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("paper_portfolio %s not found", id)
	}
	return nil
}

// -----------------------------------------------------------------------------
// paper_orders (append-only)
// -----------------------------------------------------------------------------

type PaperOrderRow struct {
	ID               string
	PortfolioID      string
	Symbol           string
	Action           string  // BUY / SELL / REBALANCE
	TargetWeight     sql.NullFloat64
	SharesChange     sql.NullFloat64
	DecidedAt        time.Time
	DecidedPrice     sql.NullFloat64
	ExecutedAt       sql.NullTime
	ExecutedPrice    sql.NullFloat64
	AIReasoning      json.RawMessage
	HashSignature    string
	CanonicalPayload string
	PublicProofURL   sql.NullString
	OTSStatus        string // pending / submitted / confirmed / disabled
}

// InsertOrder appends a new order. The caller is expected to have
// already computed HashSignature + CanonicalPayload — keeping the
// hash external to the repo means tests can pin a deterministic
// hash and the repo doesn't pull crypto into its import graph.
func (r *PaperTradingRepo) InsertOrder(ctx context.Context, o PaperOrderRow) (PaperOrderRow, error) {
	if o.PortfolioID == "" || o.Symbol == "" || o.Action == "" {
		return PaperOrderRow{}, errors.New("paper order requires portfolio_id, symbol, action")
	}
	if o.HashSignature == "" || o.CanonicalPayload == "" {
		return PaperOrderRow{}, errors.New("paper order requires hash_signature + canonical_payload")
	}
	if o.OTSStatus == "" {
		o.OTSStatus = "pending"
	}
	const q = `
		INSERT INTO paper_orders
		    (portfolio_id, symbol, action, target_weight, shares_change,
		     decided_price, executed_at, executed_price, ai_reasoning,
		     hash_signature, canonical_payload, public_proof_url, ots_status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id, decided_at
	`
	row := r.db.QueryRowContext(ctx, q,
		o.PortfolioID, o.Symbol, o.Action, o.TargetWeight, o.SharesChange,
		o.DecidedPrice, o.ExecutedAt, o.ExecutedPrice, o.AIReasoning,
		o.HashSignature, o.CanonicalPayload, o.PublicProofURL, o.OTSStatus,
	)
	if err := row.Scan(&o.ID, &o.DecidedAt); err != nil {
		return PaperOrderRow{}, fmt.Errorf("insert paper_orders: %w", err)
	}
	return o, nil
}

// MarkOrderExecuted backfills executed_at + executed_price after the
// next-day open price is known.
func (r *PaperTradingRepo) MarkOrderExecuted(ctx context.Context, orderID string, executedAt time.Time, executedPrice float64) error {
	const q = `
		UPDATE paper_orders
		SET executed_at = $2, executed_price = $3
		WHERE id = $1
	`
	res, err := r.db.ExecContext(ctx, q, orderID, executedAt, executedPrice)
	if err != nil {
		return fmt.Errorf("mark executed: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("paper_order %s not found", orderID)
	}
	return nil
}

// UpdateOrderProof attaches the public proof URL + ots_status after
// the OTS stamp lands. nil proofURL clears it; empty status leaves
// it untouched.
func (r *PaperTradingRepo) UpdateOrderProof(ctx context.Context, orderID, proofURL, otsStatus string) error {
	if orderID == "" {
		return errors.New("empty order id")
	}
	const q = `
		UPDATE paper_orders
		SET public_proof_url = NULLIF($2, ''),
		    ots_status = COALESCE(NULLIF($3, ''), ots_status)
		WHERE id = $1
	`
	res, err := r.db.ExecContext(ctx, q, orderID, proofURL, otsStatus)
	if err != nil {
		return fmt.Errorf("update order proof: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("paper_order %s not found", orderID)
	}
	return nil
}

func (r *PaperTradingRepo) ListOrders(ctx context.Context, portfolioID string, limit int) ([]PaperOrderRow, error) {
	if limit <= 0 {
		limit = 100
	}
	const q = `
		SELECT id, portfolio_id, symbol, action, target_weight, shares_change,
		       decided_at, decided_price, executed_at, executed_price, ai_reasoning,
		       hash_signature, canonical_payload, public_proof_url, ots_status
		FROM paper_orders
		WHERE portfolio_id = $1
		ORDER BY decided_at DESC
		LIMIT $2
	`
	rows, err := r.db.QueryContext(ctx, q, portfolioID, limit)
	if err != nil {
		return nil, fmt.Errorf("list paper_orders: %w", err)
	}
	defer rows.Close()
	out := make([]PaperOrderRow, 0, limit)
	for rows.Next() {
		var o PaperOrderRow
		if err := rows.Scan(
			&o.ID, &o.PortfolioID, &o.Symbol, &o.Action, &o.TargetWeight, &o.SharesChange,
			&o.DecidedAt, &o.DecidedPrice, &o.ExecutedAt, &o.ExecutedPrice, &o.AIReasoning,
			&o.HashSignature, &o.CanonicalPayload, &o.PublicProofURL, &o.OTSStatus,
		); err != nil {
			return nil, fmt.Errorf("scan paper_orders: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// -----------------------------------------------------------------------------
// paper_holdings_snapshots
// -----------------------------------------------------------------------------

type PaperHoldingsSnapshotRow struct {
	PortfolioID  string
	SnapshotDate time.Time
	Holdings     json.RawMessage // {symbol: {shares, market_value, weight}}
	CashBalance  float64
	TotalValue   float64
}

// UpsertHoldings replaces (PortfolioID, SnapshotDate) snapshot. Used
// at end-of-day to record the day's closing holding state for the
// audit trail + verifier.
func (r *PaperTradingRepo) UpsertHoldings(ctx context.Context, s PaperHoldingsSnapshotRow) error {
	if s.PortfolioID == "" || s.SnapshotDate.IsZero() {
		return errors.New("holdings snapshot requires portfolio_id + date")
	}
	const q = `
		INSERT INTO paper_holdings_snapshots
		    (portfolio_id, snapshot_date, holdings, cash_balance, total_value)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (portfolio_id, snapshot_date) DO UPDATE SET
		    holdings = EXCLUDED.holdings,
		    cash_balance = EXCLUDED.cash_balance,
		    total_value = EXCLUDED.total_value
	`
	_, err := r.db.ExecContext(ctx, q,
		s.PortfolioID, s.SnapshotDate, s.Holdings, s.CashBalance, s.TotalValue,
	)
	if err != nil {
		return fmt.Errorf("upsert holdings snapshot: %w", err)
	}
	return nil
}

func (r *PaperTradingRepo) LatestHoldings(ctx context.Context, portfolioID string) (*PaperHoldingsSnapshotRow, error) {
	const q = `
		SELECT portfolio_id, snapshot_date, holdings, cash_balance, total_value
		FROM paper_holdings_snapshots
		WHERE portfolio_id = $1
		ORDER BY snapshot_date DESC
		LIMIT 1
	`
	var s PaperHoldingsSnapshotRow
	err := r.db.QueryRowContext(ctx, q, portfolioID).Scan(
		&s.PortfolioID, &s.SnapshotDate, &s.Holdings, &s.CashBalance, &s.TotalValue,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest holdings: %w", err)
	}
	return &s, nil
}

// -----------------------------------------------------------------------------
// paper_nav_history
// -----------------------------------------------------------------------------

type PaperNavRow struct {
	PortfolioID  string
	SnapshotDate time.Time
	Nav          float64
	DailyReturn  sql.NullFloat64
	BenchmarkNav sql.NullFloat64
}

func (r *PaperTradingRepo) UpsertNav(ctx context.Context, n PaperNavRow) error {
	if n.PortfolioID == "" || n.SnapshotDate.IsZero() {
		return errors.New("nav row requires portfolio_id + date")
	}
	const q = `
		INSERT INTO paper_nav_history
		    (portfolio_id, snapshot_date, nav, daily_return, benchmark_nav)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (portfolio_id, snapshot_date) DO UPDATE SET
		    nav = EXCLUDED.nav,
		    daily_return = EXCLUDED.daily_return,
		    benchmark_nav = EXCLUDED.benchmark_nav
	`
	_, err := r.db.ExecContext(ctx, q,
		n.PortfolioID, n.SnapshotDate, n.Nav, n.DailyReturn, n.BenchmarkNav,
	)
	if err != nil {
		return fmt.Errorf("upsert nav: %w", err)
	}
	return nil
}

func (r *PaperTradingRepo) NavHistory(ctx context.Context, portfolioID string, limit int) ([]PaperNavRow, error) {
	if limit <= 0 {
		limit = 365
	}
	// We want newest-FIRST for paginated UI lists but oldest-FIRST
	// for plotting. Return ascending — caller can reverse cheaply
	// if they need newest-first.
	const q = `
		SELECT portfolio_id, snapshot_date, nav, daily_return, benchmark_nav
		FROM paper_nav_history
		WHERE portfolio_id = $1
		ORDER BY snapshot_date ASC
		LIMIT $2
	`
	rows, err := r.db.QueryContext(ctx, q, portfolioID, limit)
	if err != nil {
		return nil, fmt.Errorf("nav history: %w", err)
	}
	defer rows.Close()
	out := make([]PaperNavRow, 0, limit)
	for rows.Next() {
		var n PaperNavRow
		if err := rows.Scan(&n.PortfolioID, &n.SnapshotDate, &n.Nav, &n.DailyReturn, &n.BenchmarkNav); err != nil {
			return nil, fmt.Errorf("scan nav history: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
